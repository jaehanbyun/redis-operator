package k8sutils

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	redisv1beta1 "github.com/jaehanbyun/redis-operator/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ClusterNodeInfo struct {
	NodeID       string
	PodName      string
	Flags        string
	MasterNodeID string
}

// GetClusterNodesInfo returns information about all nodes in a Cluster by executing "cluster nodes" command via redis-cli
func GetClusterNodesInfo(k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger) ([]ClusterNodeInfo, error) {
	var firstMasterPodName string
	for _, master := range redisCluster.Status.MasterMap {
		firstMasterPodName = master.PodName
		break
	}

	if firstMasterPodName == "" {
		logger.Info("No master nodes in the cluster")
		return []ClusterNodeInfo{}, nil
	}

	port := ExtractPortFromPodName(firstMasterPodName)
	cmd := []string{"redis-cli", "-p", fmt.Sprintf("%d", port), "cluster", "nodes"}
	output, err := RunRedisCLI(k8scl, redisCluster.Namespace, firstMasterPodName, cmd)
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

	for i := int32(0); i < desiredMastersCount-currentMasterCount; i++ {
		port := redisCluster.Spec.BasePort + currentMasterCount + i
		err := CreateMasterPod(ctx, cl, k8scl, redisCluster, logger, port)
		if err != nil {
			return err
		}

		podName := fmt.Sprintf("rediscluster-%s-%d", redisCluster.Name, port)
		if err := WaitForPodReady(ctx, cl, redisCluster, logger, podName); err != nil {
			logger.Error(err, "Error waiting for master Pod to be ready", "Pod", podName)
			return err
		}

		redisNodeID, err := GetRedisNodeID(ctx, k8scl, logger, redisCluster.Namespace, podName)
		if err != nil {
			logger.Error(err, "Failed to extract Redis node ID", "Pod", podName)
			return err
		}

		if err := UpdatePodLabelWithRedisID(ctx, cl, redisCluster, logger, podName, redisNodeID); err != nil {
			logger.Error(err, "Failed to update Pod label", "Pod", podName)
			return err
		}

		redisCluster.Status.MasterMap[redisNodeID] = redisv1beta1.RedisNodeStatus{
			PodName: podName,
			NodeID:  redisNodeID,
		}

		if err := cl.Status().Update(ctx, redisCluster); err != nil {
			logger.Error(err, "Error updating RedisCluster status")
			return err
		}
	}

	return nil
}

// UpdateClusterStatus updates the RedisCluster's MasterMap and ReplicaMap by querying the current Redis cluster nodes
func UpdateClusterStatus(ctx context.Context, cl client.Client, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger) error {
	nodesInfo, err := GetClusterNodesInfo(k8scl, redisCluster, logger)
	if err != nil {
		logger.Error(err, "Failed to get cluster node information")
		return err
	}

	redisCluster.Status.MasterMap = make(map[string]redisv1beta1.RedisNodeStatus)
	redisCluster.Status.ReplicaMap = make(map[string]redisv1beta1.RedisNodeStatus)

	podList, err := k8scl.CoreV1().Pods(redisCluster.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("clusterName=%s", redisCluster.Name),
	})
	if err != nil {
		logger.Error(err, "Failed to get cluster pod list")
		return err
	}

	existingPods := make(map[string]bool)
	for _, pod := range podList.Items {
		existingPods[pod.Name] = true
	}

	if len(nodesInfo) == 0 {
		logger.Info("No cluster node information found. Assuming initial state")
	} else {
		for _, node := range nodesInfo {
			flagsList := strings.Split(node.Flags, ",")

			if !existingPods[node.PodName] {
				continue
			}

			if containsFlag(flagsList, "master") {
				redisCluster.Status.MasterMap[node.NodeID] = redisv1beta1.RedisNodeStatus{
					PodName: node.PodName,
					NodeID:  node.NodeID,
				}
			} else if containsFlag(flagsList, "slave") {
				redisCluster.Status.ReplicaMap[node.NodeID] = redisv1beta1.RedisNodeStatus{
					PodName:      node.PodName,
					NodeID:       node.NodeID,
					MasterNodeID: node.MasterNodeID,
				}
			}
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

		err = DeleteRedisPod(ctx, cl, k8scl, redisCluster, logger, replica.PodName)
		if err != nil {
			logger.Error(err, "Error deleting replica Pod", "PodName", replica.PodName)
			return err
		}
	}

	return nil
}

// AddReplicaToMaster adds a replica node to the specified master node in the cluster
func AddReplicaToMaster(k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, replica redisv1beta1.RedisNodeStatus) error {
	master, exists := redisCluster.Status.MasterMap[replica.MasterNodeID]
	if !exists {
		errMsg := "Master node not found"
		err := fmt.Errorf("%s: MasterNodeID=%s", errMsg, replica.MasterNodeID)
		logger.Error(err, errMsg, "MasterNodeID", replica.MasterNodeID)
		return err
	}

	masterAddress := GetRedisServerAddress(k8scl, logger, redisCluster.Namespace, master.PodName)
	replicaAddress := GetRedisServerAddress(k8scl, logger, redisCluster.Namespace, replica.PodName)

	addNodeCmd := []string{"redis-cli", "--cluster", "add-node", replicaAddress, masterAddress, "--cluster-slave"}
	output, err := RunRedisCLI(k8scl, redisCluster.Namespace, master.PodName, addNodeCmd)
	if err != nil {
		logger.Error(err, "Error adding replica to master", "Output", output)
		return err
	}
	logger.Info("Successfully added replica to master", "Replica", replica.PodName, "Master", master.PodName)

	err = WaitForNodeRole(k8scl, redisCluster, logger, replica.NodeID, "slave", 30*time.Second)
	if err != nil {
		logger.Error(err, "Node did not transition to replica role", "NodeID", replica.NodeID)
		return err
	}
	logger.Info("Node transitioned to replica role", "NodeID", replica.NodeID)

	return nil
}
