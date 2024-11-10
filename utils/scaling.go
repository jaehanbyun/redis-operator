package utils

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	redisv1beta1 "github.com/jaehanbyun/redis-operator/api/v1beta1"
	"github.com/redis/go-redis/v9"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// HandleClusterInitialization handles the initialization of the Redis cluster
func HandleClusterInitialization(ctx context.Context, cl client.Client, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger) error {
	desiredMasterCount := redisCluster.Spec.Masters
	currentMasterCount := int32(len(redisCluster.Status.MasterMap))

	if currentMasterCount == 0 && desiredMasterCount > 0 {
		logger.Info("Initializing cluster")
		if err := SetupRedisCluster(ctx, cl, k8scl, redisCluster, logger); err != nil {
			logger.Error(err, "Error setting up Redis cluster")
			return err
		}

		cmd := CreateClusterCommand(k8scl, redisCluster, logger)
		var firstMasterPodName string
		for _, master := range redisCluster.Status.MasterMap {
			firstMasterPodName = master.PodName
			break
		}

		_, err := RunRedisCLI(k8scl, redisCluster.Namespace, firstMasterPodName, cmd)
		if err != nil {
			logger.Error(err, "Error running cluster creation command")
			return err
		}
		logger.Info("Redis cluster created successfully")

		if err := UpdateClusterStatus(ctx, cl, k8scl, redisCluster, logger); err != nil {
			logger.Error(err, "Error updating cluster status")
			return err
		}
	}
	return nil
}

// HandleMasterScaling handles scaling up or down the master nodes in the Redis cluster
func HandleMasterScaling(ctx context.Context, cl client.Client, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger) error {
	desiredMasterCount := redisCluster.Spec.Masters
	currentMasterCount := int32(len(redisCluster.Status.MasterMap))

	if currentMasterCount < desiredMasterCount {
		return ScaleUpMasters(ctx, cl, k8scl, redisCluster, logger, desiredMasterCount-currentMasterCount)
	} else if currentMasterCount > desiredMasterCount {
		return ScaleDownMasters(ctx, cl, k8scl, redisCluster, logger, currentMasterCount-desiredMasterCount)
	}
	return nil
}

// ScaleUpMasters scales up the master nodes in the Redis cluster
func ScaleUpMasters(ctx context.Context, cl client.Client, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, mastersToAdd int32) error {
	logger.Info("Scaling up masters", "Masters to add", mastersToAdd)
	for i := int32(0); i < mastersToAdd; i++ {
		port, err := FindAvailablePort(cl, redisCluster, logger)
		if err != nil {
			logger.Error(err, "Failed to find available port")
			return err
		}

		if err := CreateMasterPod(ctx, cl, k8scl, redisCluster, logger, port); err != nil {
			logger.Error(err, "Error creating master Pod")
			return err
		}

		newMasterPodName := fmt.Sprintf("rediscluster-%s-%d", redisCluster.Name, port)
		if err := WaitForPodReady(ctx, cl, redisCluster, logger, newMasterPodName); err != nil {
			logger.Error(err, "Error waiting for new master Pod to be ready", "Pod", newMasterPodName)
			return err
		}

		newMasterNodeID, err := GetRedisNodeID(ctx, k8scl, logger, redisCluster.Namespace, newMasterPodName)
		if err != nil {
			logger.Error(err, "Error getting NodeID of new master", "Pod", newMasterPodName)
			return err
		}

		newMaster := redisv1beta1.RedisNodeStatus{
			PodName: newMasterPodName,
			NodeID:  newMasterNodeID,
		}

		if err := AddMasterToCluster(k8scl, redisCluster, logger, newMaster); err != nil {
			logger.Error(err, "Error adding new master to cluster", "NodeID", newMasterNodeID)
			return err
		}

		if err := UpdateClusterStatus(ctx, cl, k8scl, redisCluster, logger); err != nil {
			logger.Error(err, "Error updating cluster status")
			return err
		}
	}
	return nil
}

// ScaleDownMasters scales down the master nodes in the Redis cluster
func ScaleDownMasters(ctx context.Context, cl client.Client, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, mastersToRemove int32) error {
	logger.Info("Scaling down masters", "Masters to remove", mastersToRemove)
	mastersToRemoveList := GetMastersToRemove(redisCluster, mastersToRemove, logger)

	for _, masterNodeID := range mastersToRemoveList {
		master := redisCluster.Status.MasterMap[masterNodeID]

		if err := ReShardRedisCluster(ctx, k8scl, redisCluster, logger, master); err != nil {
			logger.Error(err, "Error migrating slots from master", "MasterNodeID", masterNodeID)
			return err
		}

		if err := RemoveReplicasOfMaster(ctx, cl, k8scl, redisCluster, logger, masterNodeID); err != nil {
			logger.Error(err, "Error removing replicas of master", "MasterNodeID", masterNodeID)
			return err
		}

		if err := RemoveNodeFromCluster(k8scl, redisCluster, logger, master); err != nil {
			logger.Error(err, "Error removing master from cluster", "MasterNodeID", masterNodeID)
			return err
		}

		if err := DeleteRedisPod(ctx, cl, k8scl, redisCluster, logger, master.PodName); err != nil {
			logger.Error(err, "Error deleting master Pod", "PodName", master.PodName)
			return err
		}

		delete(redisCluster.Status.MasterMap, masterNodeID)
	}

	if err := UpdateClusterStatus(ctx, cl, k8scl, redisCluster, logger); err != nil {
		logger.Error(err, "Error updating cluster status")
		return err
	}
	return nil
}

// HandleReplicaScaling handles scaling up or down the replica nodes in the Redis cluster
func HandleReplicaScaling(ctx context.Context, cl client.Client, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger) error {
	desiredReplicasPerMaster := redisCluster.Spec.Replicas
	currentMasterCount := int32(len(redisCluster.Status.MasterMap))
	currentReplicaCount := int32(len(redisCluster.Status.ReplicaMap))
	desiredTotalReplicas := desiredReplicasPerMaster * currentMasterCount

	masterToReplicas := make(map[string]int32)
	for masterID := range redisCluster.Status.MasterMap {
		masterToReplicas[masterID] = 0
	}
	for _, replica := range redisCluster.Status.ReplicaMap {
		masterToReplicas[replica.MasterNodeID]++
	}

	if desiredTotalReplicas > currentReplicaCount {
		replicasToAdd := desiredTotalReplicas - currentReplicaCount
		return ScaleUpReplicas(ctx, cl, k8scl, redisCluster, logger, replicasToAdd, masterToReplicas)
	} else if desiredTotalReplicas < currentReplicaCount {
		replicasToRemove := currentReplicaCount - desiredTotalReplicas
		return ScaleDownReplicas(ctx, cl, k8scl, redisCluster, logger, replicasToRemove, masterToReplicas)
	}
	return nil
}

// ScaleUpReplicas scales up the replica nodes in the Redis cluster
func ScaleUpReplicas(ctx context.Context, cl client.Client, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, replicasToAdd int32, masterToReplicas map[string]int32) error {
	logger.Info("Scaling up replicas", "Replicas to add", replicasToAdd)
	desiredReplicasPerMaster := redisCluster.Spec.Replicas

	for i := int32(0); i < replicasToAdd; i++ {
		var assignedMasterID string
		for masterID, count := range masterToReplicas {
			if count < desiredReplicasPerMaster {
				assignedMasterID = masterID
				break
			}
		}

		port, err := FindAvailablePort(cl, redisCluster, logger)
		if err != nil {
			logger.Error(err, "Failed to find replica port")
			return err
		}

		if err := CreateReplicaPod(ctx, cl, k8scl, redisCluster, logger, port, assignedMasterID); err != nil {
			logger.Error(err, "Error creating replica Pod")
			return err
		}

		newReplicaPodName := fmt.Sprintf("rediscluster-%s-%d", redisCluster.Name, port)
		if err := WaitForPodReady(ctx, cl, redisCluster, logger, newReplicaPodName); err != nil {
			logger.Error(err, "Error waiting for new replica Pod to be ready", "Pod", newReplicaPodName)
			return err
		}

		newReplicaNodeID, err := GetRedisNodeID(ctx, k8scl, logger, redisCluster.Namespace, newReplicaPodName)
		if err != nil {
			logger.Error(err, "Error getting NodeID of replica", "Pod", newReplicaPodName)
			return err
		}

		if redisCluster.Status.ReplicaMap == nil {
			redisCluster.Status.ReplicaMap = make(map[string]redisv1beta1.RedisNodeStatus)
		}
		redisCluster.Status.ReplicaMap[newReplicaNodeID] = redisv1beta1.RedisNodeStatus{
			PodName:      newReplicaPodName,
			NodeID:       newReplicaNodeID,
			MasterNodeID: assignedMasterID,
		}

		newReplica := redisCluster.Status.ReplicaMap[newReplicaNodeID]

		if err := AddReplicaToMaster(k8scl, redisCluster, logger, newReplica); err != nil {
			logger.Error(err, "Error adding replica to master", "ReplicaNodeID", newReplicaNodeID)
			return err
		}

		if err := UpdateClusterStatus(ctx, cl, k8scl, redisCluster, logger); err != nil {
			logger.Error(err, "Error updating cluster status")
			return err
		}

		masterToReplicas[assignedMasterID]++
	}
	return nil
}

// ScaleDownReplicas scales down the replica nodes in the Redis cluster
func ScaleDownReplicas(ctx context.Context, cl client.Client, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, replicasToRemove int32, masterToReplicas map[string]int32) error {
	logger.Info("Scaling down replicas", "Replicas to remove", replicasToRemove)
	desiredReplicasPerMaster := redisCluster.Spec.Replicas

	for masterID, replicas := range masterToReplicas {
		excessReplicas := replicas - desiredReplicasPerMaster
		if excessReplicas > 0 {
			replicasList := GetReplicasToRemoveFromMaster(redisCluster, masterID, excessReplicas, logger)

			for _, replicaNodeID := range replicasList {
				replica := redisCluster.Status.ReplicaMap[replicaNodeID]

				if err := RemoveNodeFromCluster(k8scl, redisCluster, logger, replica); err != nil {
					logger.Error(err, "Error removing replica from cluster", "ReplicaNodeID", replicaNodeID)
					return err
				}

				if err := DeleteRedisPod(ctx, cl, k8scl, redisCluster, logger, replica.PodName); err != nil {
					logger.Error(err, "Error deleting replica Pod", "PodName", replica.PodName)
					return err
				}

				delete(redisCluster.Status.ReplicaMap, replicaNodeID)

				masterToReplicas[masterID]--
				replicasToRemove--
				if replicasToRemove == 0 {
					break
				}
			}
		}

		if replicasToRemove == 0 {
			break
		}
	}

	if err := UpdateClusterStatus(ctx, cl, k8scl, redisCluster, logger); err != nil {
		logger.Error(err, "Error updating cluster status")
		return err
	}
	return nil
}

// getRedisClusterSlots returns the total number of slots assigned to a specific Redis node
func getRedisClusterSlots(ctx context.Context, redisClient *redis.Client, logger logr.Logger, nodeID string) (int64, error) {
	var totalSlots int64

	redisSlots, err := redisClient.ClusterSlots(ctx).Result()
	if err != nil {
		logger.Error(err, "Failed to get cluster slots")
		return 0, err
	}

	for _, slot := range redisSlots {
		for _, node := range slot.Nodes {
			if node.ID == nodeID {
				totalSlots += int64(slot.End - slot.Start + 1)
				break
			}
		}
	}

	logger.V(1).Info("Total cluster slots to be transferred from node", "NodeID", nodeID, "TotalSlots", totalSlots)
	return totalSlots, nil
}

// ReShardRedisCluster reshards slots from one master to another in the Redis cluster
func ReShardRedisCluster(ctx context.Context, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, master redisv1beta1.RedisNodeStatus) error {
	masterPodName := master.PodName
	redisClient := ConfigureRedisClient(k8scl, redisCluster, logger, masterPodName)
	defer func() {
		if err := redisClient.Close(); err != nil {
			logger.Error(err, "Error closing Redis client")
		}
	}()

	totalSlots, err := getRedisClusterSlots(ctx, redisClient, logger, master.NodeID)
	if err != nil {
		logger.Error(err, "Failed to get slots of master node", "MasterNodeID", master.NodeID)
		return err
	}

	if totalSlots == 0 {
		logger.Info("No slots to migrate from master", "MasterNodeID", master.NodeID)
		return nil
	}

	var targetMaster redisv1beta1.RedisNodeStatus
	for _, m := range redisCluster.Status.MasterMap {
		if m.NodeID != master.NodeID {
			targetMaster = m
			break
		}
	}

	if targetMaster.NodeID == "" {
		return fmt.Errorf("no target master available for slot migration")
	}

	masterAddress := GetRedisServerAddress(k8scl, logger, redisCluster.Namespace, master.PodName)
	reshardCmd := []string{
		"redis-cli", "--cluster", "reshard", masterAddress,
		"--cluster-from", master.NodeID,
		"--cluster-to", targetMaster.NodeID,
		"--cluster-slots", strconv.FormatInt(totalSlots, 10),
		"--cluster-yes",
	}

	logger.Info("Resharding slots from master", "Command", reshardCmd)
	output, err := RunRedisCLI(k8scl, redisCluster.Namespace, master.PodName, reshardCmd)
	if err != nil {
		logger.Error(err, "Error resharding slots", "Output", output)
		return err
	}

	logger.Info("Slot migration completed", "MasterNodeID", master.NodeID)
	return nil
}

// WaitForSlotMigration waits for slot migration to complete
func WaitForSlotMigration(ctx context.Context, redisClient *redis.Client, logger logr.Logger, nodeID string, timeout time.Duration) error {
	startTime := time.Now()
	for {
		elapsed := time.Since(startTime)
		if elapsed > timeout {
			return fmt.Errorf("slot migration did not complete within the timeout period")
		}

		totalSlots, err := getRedisClusterSlots(ctx, redisClient, logger, nodeID)
		if err != nil {
			logger.Error(err, "Failed to get slots of node", "NodeID", nodeID)
			return err
		}

		if totalSlots == 0 {
			logger.Info("Slot migration completed", "NodeID", nodeID)
			return nil
		}

		logger.Info("Waiting for slot migration to complete...", "NodeID", nodeID, "RemainingSlots", totalSlots)
		time.Sleep(2 * time.Second)
	}
}

// AddMasterToCluster adds a new master node to the Redis cluster and rebalances the slots
func AddMasterToCluster(k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, newMaster redisv1beta1.RedisNodeStatus) error {
	var existingMaster redisv1beta1.RedisNodeStatus
	for _, master := range redisCluster.Status.MasterMap {
		existingMaster = master
		break
	}

	existingMasterAddress := GetRedisServerAddress(k8scl, logger, redisCluster.Namespace, existingMaster.PodName)
	newMasterAddress := GetRedisServerAddress(k8scl, logger, redisCluster.Namespace, newMaster.PodName)

	addNodeCmd := []string{"redis-cli", "--cluster", "add-node", newMasterAddress, existingMasterAddress}
	output, err := RunRedisCLI(k8scl, redisCluster.Namespace, existingMaster.PodName, addNodeCmd)
	if err != nil {
		logger.Error(err, "Error adding new master node to cluster", "Output", output)
		return err
	}
	logger.Info("Successfully added new master node to cluster", "Node", newMaster.PodName)

	if err := WaitForClusterStabilization(k8scl, redisCluster, logger); err != nil {
		logger.Error(err, "Error during cluster stabilization")
		return err
	}

	masterAddress := GetRedisServerAddress(k8scl, logger, redisCluster.Namespace, newMaster.PodName)
	if err := RebalanceCluster(k8scl, redisCluster, logger, masterAddress); err != nil {
		logger.Error(err, "Error during cluster rebalancing")
		return err
	}

	return nil
}

// RemoveNodeFromCluster removes a node from the Redis cluster using its NodeID
func RemoveNodeFromCluster(k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, node redisv1beta1.RedisNodeStatus) error {
	var existingMaster redisv1beta1.RedisNodeStatus
	for _, m := range redisCluster.Status.MasterMap {
		if m.NodeID != node.NodeID {
			existingMaster = m
			break
		}
	}

	if existingMaster.NodeID == "" {
		return fmt.Errorf("no existing master available to remove node")
	}

	existingMasterAddress := GetRedisServerAddress(k8scl, logger, redisCluster.Namespace, existingMaster.PodName)

	delNodeCmd := []string{"redis-cli", "--cluster", "del-node", existingMasterAddress, node.NodeID}

	logger.Info("Removing node from cluster", "Command", delNodeCmd)
	output, err := RunRedisCLI(k8scl, redisCluster.Namespace, existingMaster.PodName, delNodeCmd)
	if err != nil {
		logger.Error(err, "Error removing node from cluster", "Output", output)
		return err
	}

	logger.Info("Node removed from cluster", "NodePod", node.PodName)
	return nil
}

// RebalanceCluster rebalances the cluster
func RebalanceCluster(k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, masterAddress string) error {
	var existingMaster redisv1beta1.RedisNodeStatus
	for _, m := range redisCluster.Status.MasterMap {
		existingMaster = m
		break
	}

	rebalanceCmd := []string{"redis-cli", "--cluster", "rebalance", masterAddress, "--cluster-use-empty-masters", "--cluster-yes"}
	logger.Info("Rebalance command", "Command", rebalanceCmd)

	output, err := RunRedisCLI(k8scl, redisCluster.Namespace, existingMaster.PodName, rebalanceCmd)
	if err != nil {
		logger.Error(err, "Error during cluster rebalancing", "Output", output)
		return err
	}
	logger.Info("Cluster rebalancing completed successfully")
	return nil
}
