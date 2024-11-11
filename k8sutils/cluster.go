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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	FailureThreshold = 5
)

type ClusterNodeInfo struct {
	NodeID       string
	PodName      string
	Flags        string
	MasterNodeID string
}

// GetClusterNodesInfo returns information about all nodes in a Cluster by executing "cluster nodes" command via redis-cli
func GetClusterNodesInfo(ctx context.Context, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger) ([]ClusterNodeInfo, error) {
	var masterPodName string
	for _, master := range redisCluster.Status.MasterMap {
		isRunning, err := IsPodRunning(ctx, k8scl, redisCluster.Namespace, master.PodName, "redis", logger)
		if err != nil {
			logger.Error(err, "Error checking if Pod is running", "PodName", master.PodName)
			continue
		}
		if isRunning {
			masterPodName = master.PodName
			break
		}
	}
	if masterPodName == "" {
		logger.Info("No master nodes in the cluster")
		return []ClusterNodeInfo{}, nil
	}

	port := ExtractPortFromPodName(masterPodName)
	cmd := []string{"redis-cli", "-p", fmt.Sprintf("%d", port), "cluster", "nodes"}
	output, err := RunRedisCLI(k8scl, redisCluster.Namespace, masterPodName, cmd)
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
		flags := parts[2]
		masterNodeID := ""
		if len(parts) > 3 && parts[3] != "-" {
			masterNodeID = parts[3]
		}

		podName, err := GetPodNameByNodeID(k8scl, redisCluster.Namespace, nodeID, logger)
		if err != nil {
			logger.Error(err, "Failed to find Pod by NodeID", "NodeID", nodeID)
		}

		nodesInfo = append(nodesInfo, ClusterNodeInfo{
			NodeID:       nodeID,
			PodName:      podName,
			Flags:        flags,
			MasterNodeID: masterNodeID,
		})
	}

	return nodesInfo, nil
}

// SetupRedisCluster sets up the initial cluster with master pods
func SetupRedisCluster(ctx context.Context, cl client.Client, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger) error {
	desiredMastersCount := redisCluster.Spec.Masters
	currentMasterCount := int32(len(redisCluster.Status.MasterMap))

	if redisCluster.Status.MasterMap == nil {
		redisCluster.Status.MasterMap = make(map[string]redisv1beta1.RedisNodeStatus)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	errorCh := make(chan error, desiredMastersCount-currentMasterCount)

	for i := int32(0); i < desiredMastersCount-currentMasterCount; i++ {
		wg.Add(1)
		go func(offset int32) {
			defer wg.Done()
			port := redisCluster.Spec.BasePort + currentMasterCount + offset
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

			redisNodeID, err := GetRedisNodeID(ctx, k8scl, logger, redisCluster.Namespace, podName)
			if err != nil {
				logger.Error(err, "Failed to extract Redis node ID", "Pod", podName)
				errorCh <- err
				return
			}

			mu.Lock()
			redisCluster.Status.MasterMap[redisNodeID] = redisv1beta1.RedisNodeStatus{
				PodName: podName,
				NodeID:  redisNodeID,
			}
			mu.Unlock()
		}(i)
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
	if err := cl.Get(ctx, types.NamespacedName{Name: redisCluster.Name, Namespace: redisCluster.Namespace}, redisCluster); err != nil {
		logger.Error(err, "Failed to get latest RedisCluster state")
		return err
	}

	nodesInfo, err := GetClusterNodesInfo(ctx, k8scl, redisCluster, logger)
	if err != nil {
		logger.Error(err, "Failed to get cluster node information")
		return err
	}

	if redisCluster.Status.MasterMap == nil {
		redisCluster.Status.MasterMap = make(map[string]redisv1beta1.RedisNodeStatus)
	}
	if redisCluster.Status.ReplicaMap == nil {
		redisCluster.Status.ReplicaMap = make(map[string]redisv1beta1.RedisNodeStatus)
	}
	if redisCluster.Status.FailedNodes == nil {
		redisCluster.Status.FailedNodes = make(map[string]redisv1beta1.RedisFailedNodeStatus)
	}

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

	if len(nodesInfo) == 0 {
		logger.Info("No cluster node information found. Assuming initial state")
	} else {
		for _, node := range nodesInfo {
			flagsList := strings.Split(node.Flags, ",")

			pod := existingPods[node.PodName]

			if pod.Status.Phase != corev1.PodRunning || !isContainerReady(pod, "redis") {
				incrementFailureCount(redisCluster, node.NodeID, node.PodName, 2)
				continue
			}

			if containsFlag(flagsList, "fail") || containsFlag(flagsList, "disconnected") {
				incrementFailureCount(redisCluster, node.NodeID, node.PodName, 1)
			} else {
				resetFailureCount(redisCluster, node.NodeID)
			}

			failureCount := redisCluster.Status.FailedNodes[node.NodeID].FailureCount
			if failureCount < 5 {
				if containsFlag(flagsList, "master") {
					currentMasters[node.NodeID] = redisv1beta1.RedisNodeStatus{
						PodName: node.PodName,
						NodeID:  node.NodeID,
					}
				} else if containsFlag(flagsList, "slave") {
					currentReplicas[node.NodeID] = redisv1beta1.RedisNodeStatus{
						PodName:      node.PodName,
						NodeID:       node.NodeID,
						MasterNodeID: node.MasterNodeID,
					}
				}
			}
		}
	}

	redisCluster.Status.MasterMap = currentMasters
	redisCluster.Status.ReplicaMap = currentReplicas

	logger.Info("Current MasterMap", "MasterMap", redisCluster.Status.MasterMap)
	logger.Info("Current ReplicaMap", "ReplicaMap", redisCluster.Status.ReplicaMap)
	logger.Info("Current FailedNodes", "FailedNodes", redisCluster.Status.FailedNodes)

	failedNodes := make(map[string]redisv1beta1.RedisFailedNodeStatus)
	for _, node := range redisCluster.Status.FailedNodes {
		if node.FailureCount >= FailureThreshold {
			logger.Info("Handling permanently failed node", "NodeID", node.NodeID)
			failedNodes[node.NodeID] = node
		}
	}

	if len(failedNodes) > FailureThreshold {
		err = handleFailedNode(ctx, cl, k8scl, redisCluster, logger, failedNodes)
		if err != nil {
			logger.Error(err, "Failed to handle failed node")
			return err
		}
	}

	if err := cl.Status().Update(ctx, redisCluster); err != nil {
		logger.Error(err, "Error updating RedisCluster status")
		return err
	}

	return nil
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
