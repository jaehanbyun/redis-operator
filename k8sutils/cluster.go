package k8sutils

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	redisv1beta1 "github.com/jaehanbyun/redis-operator/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ClusterNodeInfo struct {
	NodeID       string
	PodName      string
	Role         string
	Up           bool
	MasterNodeID string
}

// GetClusterNodesInfo returns information about all nodes in a Cluster by executing "cluster nodes" command via redis-cli
func GetClusterNodesInfo(ctx context.Context, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger) ([]ClusterNodeInfo, error) {
	podList, err := k8scl.CoreV1().Pods(redisCluster.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("clusterName=%s", redisCluster.Name),
	})
	if err != nil {
		logger.Error(err, "Failed to list Pods")
		return nil, err
	}

	var redisPodName string
	for _, pod := range podList.Items {
		isRunning, err := IsPodRunning(ctx, k8scl, redisCluster.Namespace, pod.Name, "redis", logger)
		if err != nil {
			logger.Error(err, "Error checking if Pod is running", "PodName", pod.Name)
			continue
		}
		if isRunning {
			redisPodName = pod.Name
			break
		}
	}
	if redisPodName == "" {
		logger.Info("No running Pods found in the cluster")
		return []ClusterNodeInfo{}, nil
	}

	port := ExtractPortFromPodName(redisPodName)
	cmd := []string{"redis-cli", "-p", fmt.Sprintf("%d", port), "cluster", "nodes"}
	output, err := RunRedisCLI(k8scl, redisCluster.Namespace, redisPodName, cmd)
	if err != nil {
		logger.Error(err, "Error executing Redis CLI command", "Command", strings.Join(cmd, " "))
		return nil, err
	}

	logger.Info("Output of redis-cli cluster nodes command", "Output", output)

	nodesInfo := make([]ClusterNodeInfo, 0)
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.Split(line, " ")
		nodeID := parts[0]
		masterNodeID := ""
		role := ""
		up := true
		if len(parts) > 3 && parts[3] != "-" {
			masterNodeID = parts[3]
		}
		if strings.Contains(line, "master") {
			role = "master"
		} else {
			role = "slave"
		}
		if strings.Contains(line, "fail") || strings.Contains(line, "disconnected") {
			up = false
		}
		podName, _ := GetPodNameByNodeID(k8scl, redisCluster.Namespace, nodeID, logger)
		nodesInfo = append(nodesInfo, ClusterNodeInfo{
			NodeID:       nodeID,
			PodName:      podName,
			Role:         role,
			MasterNodeID: masterNodeID,
			Up:           up,
		})
	}

	return nodesInfo, nil
}

// SetupRedisCluster sets up the initial cluster with master pods
func SetupRedisCluster(ctx context.Context, cl client.Client, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, newMasterCount int32) error {
	if redisCluster.Status.MasterMap == nil {
		redisCluster.Status.MasterMap = make(map[string]redisv1beta1.RedisNodeStatus)
	}

	var wg sync.WaitGroup
	errorCh := make(chan error, newMasterCount)

	ports := make([]int32, newMasterCount)
	for i := int32(0); i < newMasterCount; i++ {
		ports[i] = GetNextAvailablePort(redisCluster)
	}

	for i := int32(0); i < newMasterCount; i++ {
		wg.Add(1)
		go func(port int32) {
			defer wg.Done()

			err := CreateMasterPod(ctx, k8scl, redisCluster, logger, port)
			if err != nil {
				errorCh <- err
				return
			}

			podName := fmt.Sprintf("rediscluster-%s-%d", redisCluster.Name, port)
			if err := WaitForPodReady(ctx, k8scl, redisCluster, logger, podName); err != nil {
				logger.Error(err, "Error waiting for master Pod to be ready", "Pod", podName)
				errorCh <- err
				return
			}
		}(ports[i])
	}

	wg.Wait()
	close(errorCh)

	if len(errorCh) > 0 {
		return <-errorCh
	}

	return nil
}

// UpdateClusterStatus updates the RedisCluster's MasterMap and ReplicaMap by querying the current Redis cluster nodes
func UpdateClusterStatus(ctx context.Context, cl client.Client, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger) error {
	nodesInfo, err := GetClusterNodesInfo(ctx, k8scl, redisCluster, logger)
	if err != nil {
		logger.Error(err, "Failed to get cluster node information")
		return err
	}

	initializeStatusMaps(redisCluster)

	podList, err := k8scl.CoreV1().Pods(redisCluster.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("clusterName=%s", redisCluster.Name)})
	if err != nil {
		logger.Error(err, "Failed to get cluster pod list")
		return err
	}

	existingPods := make(map[string]*corev1.Pod)
	for i := range podList.Items {
		pod := &podList.Items[i]
		existingPods[pod.Name] = pod
	}

	currentMasters := make(map[string]redisv1beta1.RedisNodeStatus)
	currentReplicas := make(map[string]redisv1beta1.RedisNodeStatus)
	currentMasterFailedMap := make(map[string]redisv1beta1.RedisNodeStatus)
	currentReplicaFailedMap := make(map[string]redisv1beta1.RedisNodeStatus)

	if len(nodesInfo) == 0 {
		logger.Info("No cluster node information found. Assuming initial state")
	} else {
		for _, node := range nodesInfo {
			if !node.Up {
				if node.Role == "master" {
					currentMasterFailedMap[node.NodeID] = redisv1beta1.RedisNodeStatus{
						NodeID: node.NodeID,
					}
				} else {
					currentReplicaFailedMap[node.NodeID] = redisv1beta1.RedisNodeStatus{
						NodeID:       node.NodeID,
						MasterNodeID: node.MasterNodeID,
					}
				}
			} else {
				updateNodeStatus(currentMasters, currentReplicas, node)
			}
		}
	}

	redisCluster.Status.MasterMap = currentMasters
	redisCluster.Status.ReplicaMap = currentReplicas
	redisCluster.Status.FailedMasterMap = currentMasterFailedMap
	redisCluster.Status.FailedReplicaMap = currentReplicaFailedMap

	logClusterStatus(logger, redisCluster)

	return updateClusterStatusWithRetry(ctx, cl, redisCluster)
}

// WaitForClusterStabilization checks if all Redis cluster nodes agree on the configuration
func WaitForClusterStabilization(k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger) error {
	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("cluster stabilization timed out")
		case <-ticker.C:
			nodesAgree, err := CheckClusterConfigurationAgreement(k8scl, redisCluster, logger)
			if err != nil {
				logger.Error(err, "Error checking cluster configuration")
				continue
			}
			if nodesAgree {
				logger.Info("Cluster nodes have agreed on the configuration")
				return nil
			} else {
				logger.Info("Cluster nodes have not yet agreed on the configuration. Waiting...")
			}
		}
	}
}

// CheckClusterConfigurationAgreement verifies if all Redis cluster nodes share the same configuration epoch
func CheckClusterConfigurationAgreement(k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger) (bool, error) {
	var configEpoch string

	for _, master := range redisCluster.Status.MasterMap {
		podName := master.PodName
		namespace := redisCluster.Namespace

		port := ExtractPortFromPodName(podName)

		cmd := []string{"redis-cli", "-p", fmt.Sprintf("%d", port), "cluster", "info"}
		logger.Info("Executing command", "Pod", podName, "Command", strings.Join(cmd, " "))

		output, err := RunRedisCLI(k8scl, namespace, podName, cmd)
		if err != nil {
			return false, err
		}

		lines := strings.Split(output, "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "config-epoch:") {
				epoch := strings.TrimSpace(strings.Split(line, ":")[1])
				if configEpoch == "" {
					configEpoch = epoch
				} else if configEpoch != epoch {
					return false, nil
				}
			}
		}
	}
	return true, nil
}

// RemoveReplicasOfMaster removes all replicas associated with the specified master from the Redis cluster
func RemoveReplicasOfMaster(ctx context.Context, cl client.Client, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, masterNodeID string) error {
	var replicasToRemove []redisv1beta1.RedisNodeStatus

	for nodeID, replica := range redisCluster.Status.ReplicaMap {
		if replica.MasterNodeID == masterNodeID {
			replicasToRemove = append(replicasToRemove, replica)
			delete(redisCluster.Status.ReplicaMap, nodeID)
		}
	}

	for _, replica := range replicasToRemove {
		err := RemoveNodeFromCluster(k8scl, redisCluster, logger, replica)
		if err != nil {
			logger.Error(err, "Error removing replica from cluster", "ReplicaNodeID", replica.NodeID)
			return err
		}

		err = DeleteRedisPod(ctx, k8scl, redisCluster, logger, replica.PodName)
		if err != nil {
			logger.Error(err, "Error deleting replica Pod", "PodName", replica.PodName)
			return err
		}
	}

	return nil
}

func initializeStatusMaps(redisCluster *redisv1beta1.RedisCluster) {
	if redisCluster.Status.NextAvailablePort == 0 {
		redisCluster.Status.NextAvailablePort = redisCluster.Spec.BasePort
	}
	if redisCluster.Status.MasterMap == nil {
		redisCluster.Status.MasterMap = make(map[string]redisv1beta1.RedisNodeStatus)
	}
	if redisCluster.Status.ReplicaMap == nil {
		redisCluster.Status.ReplicaMap = make(map[string]redisv1beta1.RedisNodeStatus)
	}
	if redisCluster.Status.FailedMasterMap == nil {
		redisCluster.Status.FailedMasterMap = make(map[string]redisv1beta1.RedisNodeStatus)
	}
	if redisCluster.Status.FailedMasterMap == nil {
		redisCluster.Status.FailedMasterMap = make(map[string]redisv1beta1.RedisNodeStatus)
	}
}

func updateNodeStatus(currentMasters, currentReplicas map[string]redisv1beta1.RedisNodeStatus, node ClusterNodeInfo) {
	if node.Role == "master" {
		currentMasters[node.NodeID] = redisv1beta1.RedisNodeStatus{
			PodName: node.PodName,
			NodeID:  node.NodeID,
		}
	} else if node.Role == "slave" {
		currentReplicas[node.NodeID] = redisv1beta1.RedisNodeStatus{
			PodName:      node.PodName,
			NodeID:       node.NodeID,
			MasterNodeID: node.MasterNodeID,
		}
	}
}

func GetReplicasOfMaster(redisCluster *redisv1beta1.RedisCluster, masterNodeID string) []redisv1beta1.RedisNodeStatus {
	replicas := []redisv1beta1.RedisNodeStatus{}
	for _, replica := range redisCluster.Status.ReplicaMap {
		if replica.MasterNodeID == masterNodeID {
			replicas = append(replicas, replica)
		}
	}
	return replicas
}

func updateClusterStatusWithRetry(ctx context.Context, cl client.Client, redisCluster *redisv1beta1.RedisCluster) error {
	currentVersion := redisCluster.ResourceVersion

	if err := cl.Status().Update(ctx, redisCluster); err != nil {
		if errors.IsConflict(err) {
			updatedRedisCluster := &redisv1beta1.RedisCluster{}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(redisCluster), updatedRedisCluster); err != nil {
				return err
			}
			if updatedRedisCluster.ResourceVersion != currentVersion {
				updatedRedisCluster.Status = redisCluster.Status
				return cl.Status().Update(ctx, updatedRedisCluster)
			}
		}
		return err
	}

	return nil
}

func logClusterStatus(logger logr.Logger, redisCluster *redisv1beta1.RedisCluster) {
	logger.Info("Current MasterMap", "MasterMap", redisCluster.Status.MasterMap)
	logger.Info("Current ReplicaMap", "ReplicaMap", redisCluster.Status.ReplicaMap)
	logger.Info("Current FailedNodes", "FailedMasterNodes", redisCluster.Status.FailedMasterMap)
	logger.Info("Current FailedNodes", "FailedReplicaNodes", redisCluster.Status.FailedReplicaMap)
}
