package k8sutils

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	redisv1beta1 "github.com/jaehanbyun/redis-operator/api/v1beta1"
	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// HandleClusterInitialization handles the initialization of the Redis cluster
func HandleClusterInitialization(ctx context.Context, cl client.Client, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger) error {
	desiredMasterCount := redisCluster.Spec.Masters
	currentMasterCount := int32(len(redisCluster.Status.MasterMap))

	if currentMasterCount == 0 && desiredMasterCount > 0 {
		logger.Info("Initializing cluster")
		newMastersCount := desiredMasterCount - currentMasterCount
		if err := SetupRedisCluster(ctx, cl, k8scl, redisCluster, logger, newMastersCount); err != nil {
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
		if err := ScaleUpMasters(ctx, cl, k8scl, redisCluster, logger, desiredMasterCount-currentMasterCount); err != nil {
			return err
		}
		if err := UpdateClusterStatus(ctx, cl, k8scl, redisCluster, logger); err != nil {
			logger.Error(err, "Error updating cluster status")
			return err
		}
	} else if currentMasterCount > desiredMasterCount {
		if err := ScaleDownMasters(ctx, cl, k8scl, redisCluster, logger, currentMasterCount-desiredMasterCount); err != nil {
			return err
		}
		if err := UpdateClusterStatus(ctx, cl, k8scl, redisCluster, logger); err != nil {
			logger.Error(err, "Error updating cluster status")
			return err
		}
	}

	return nil
}

// ScaleUpMasters scales up the master nodes in the Redis cluster
func ScaleUpMasters(ctx context.Context, cl client.Client, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, mastersToAdd int32) error {
	logger.Info("Scaling up masters", "Masters to add", mastersToAdd)

	var wg sync.WaitGroup
	var mu sync.Mutex
	errorCh := make(chan error, mastersToAdd)
	newMasters := make([]redisv1beta1.RedisNodeStatus, 0, mastersToAdd)
	masterCh := make(chan redisv1beta1.RedisNodeStatus, mastersToAdd)

	for i := int32(0); i < mastersToAdd; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			mu.Lock()
			port := GetNextAvailablePort(redisCluster)
			mu.Unlock()
			if err := CreateMasterPod(ctx, k8scl, redisCluster, logger, port); err != nil {
				logger.Error(err, "Error creating master Pod")
				errorCh <- err
				return
			}

			newMasterPodName := fmt.Sprintf("rediscluster-%s-%d", redisCluster.Name, port)
			if err := WaitForPodReady(ctx, k8scl, redisCluster, logger, newMasterPodName); err != nil {
				logger.Error(err, "Error waiting for new master Pod to be ready", "Pod", newMasterPodName)
				errorCh <- err
				return
			}

			newMasterNodeID, err := GetRedisNodeID(ctx, k8scl, logger, redisCluster.Namespace, newMasterPodName)
			if err != nil {
				logger.Error(err, "Error getting NodeID of new master", "Pod", newMasterPodName)
				errorCh <- err
				return
			}

			newMaster := redisv1beta1.RedisNodeStatus{
				PodName: newMasterPodName,
				NodeID:  newMasterNodeID,
			}
			masterCh <- newMaster
		}()

	}
	wg.Wait()
	close(errorCh)
	close(masterCh)

	if len(errorCh) > 0 {
		return <-errorCh
	}

	for master := range masterCh {
		newMasters = append(newMasters, master)
	}

	if err := AddMasterToCluster(k8scl, redisCluster, logger, newMasters); err != nil {
		logger.Error(err, "Error adding new master to cluster")
		return err
	}

	if err := RebalanceCluster(k8scl, redisCluster, logger, true); err != nil {
		logger.Error(err, "Error during cluster rebalancing")
		return err
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

		if err := DeleteRedisPod(ctx, k8scl, redisCluster, logger, master.PodName); err != nil {
			logger.Error(err, "Error deleting master Pod", "PodName", master.PodName)
			return err
		}

		delete(redisCluster.Status.MasterMap, masterNodeID)
	}

	if err := RebalanceCluster(k8scl, redisCluster, logger, false); err != nil {
		logger.Error(err, "Error during cluster rebalancing")
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
		if err := ScaleUpReplicas(ctx, cl, k8scl, redisCluster, logger, replicasToAdd, masterToReplicas); err != nil {
			return err
		}
		if err := UpdateClusterStatus(ctx, cl, k8scl, redisCluster, logger); err != nil {
			logger.Error(err, "Error updating cluster status")
			return err
		}
	} else if desiredTotalReplicas < currentReplicaCount {
		replicasToRemove := currentReplicaCount - desiredTotalReplicas
		if err := ScaleDownReplicas(ctx, cl, k8scl, redisCluster, logger, replicasToRemove, masterToReplicas); err != nil {
			return err
		}
		if err := UpdateClusterStatus(ctx, cl, k8scl, redisCluster, logger); err != nil {
			logger.Error(err, "Error updating cluster status")
			return err
		}
	}

	return nil
}

// ScaleUpReplicas scales up the replica nodes in the Redis cluster
func ScaleUpReplicas(ctx context.Context, cl client.Client, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, replicasToAdd int32, masterToReplicas map[string]int32) error {
	logger.Info("Scaling up replicas", "Replicas to add", replicasToAdd)
	desiredReplicasPerMaster := redisCluster.Spec.Replicas

	var wg sync.WaitGroup
	errorCh := make(chan error, replicasToAdd)
	newReplicas := make([]redisv1beta1.RedisNodeStatus, 0, replicasToAdd)
	replicaCh := make(chan redisv1beta1.RedisNodeStatus, replicasToAdd)

	ports := make([]int32, replicasToAdd)
	for i := int32(0); i < replicasToAdd; i++ {
		ports[i] = GetNextAvailablePort(redisCluster)
	}

	assignments := make([]string, replicasToAdd)
	for i := range assignments {
		for masterID, count := range masterToReplicas {
			if count < desiredReplicasPerMaster {
				assignments[i] = masterID
				masterToReplicas[masterID]++
				break
			}
		}
	}

	for i := int32(0); i < replicasToAdd; i++ {
		wg.Add(1)
		go func(assignedMasterID string, port int32) {
			defer wg.Done()
			if err := CreateReplicaPod(ctx, k8scl, redisCluster, logger, port, assignedMasterID); err != nil {
				logger.Error(err, "Error creating replica Pod")
				errorCh <- err
				return
			}

			newReplicaPodName := fmt.Sprintf("rediscluster-%s-%d", redisCluster.Name, port)
			if err := WaitForPodReady(ctx, k8scl, redisCluster, logger, newReplicaPodName); err != nil {
				logger.Error(err, "Error waiting for new replica Pod to be ready", "Pod", newReplicaPodName)
				errorCh <- err
				return
			}

			newReplicaNodeID, err := GetRedisNodeID(ctx, k8scl, logger, redisCluster.Namespace, newReplicaPodName)
			if err != nil {
				logger.Error(err, "Error getting NodeID of replica", "Pod", newReplicaPodName)
				errorCh <- err
				return
			}

			replicaCh <- redisv1beta1.RedisNodeStatus{
				PodName:      newReplicaPodName,
				NodeID:       newReplicaNodeID,
				MasterNodeID: assignedMasterID,
			}
		}(assignments[i], ports[i])
	}

	wg.Wait()
	close(errorCh)

	if len(errorCh) > 0 {
		return <-errorCh
	}

	for replica := range replicaCh {
		newReplicas = append(newReplicas, replica)
	}

	if err := AddReplicaToMaster(k8scl, redisCluster, logger, newReplicas); err != nil {
		logger.Error(err, "Error adding new replicas to masters")
		return err
	}

	return nil
}

// ScaleDownReplicas scales down the replica nodes in the Redis cluster
func ScaleDownReplicas(ctx context.Context, cl client.Client, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, replicasToRemove int32, masterToReplicas map[string]int32) error {
	logger.Info("Scaling down replicas", "Replicas to remove", replicasToRemove)
	desiredReplicasPerMaster := redisCluster.Spec.Replicas

	var wg sync.WaitGroup
	errorCh := make(chan error, replicasToRemove)
	removedReplicas := int32(0)

	for masterID, replicas := range masterToReplicas {
		excessReplicas := replicas - desiredReplicasPerMaster
		if excessReplicas <= 0 {
			continue
		}

		replicasList := GetReplicasToRemoveFromMaster(redisCluster, masterID, excessReplicas, logger)

		for _, replicaNodeID := range replicasList {
			if removedReplicas >= replicasToRemove {
				break
			}

			wg.Add(1)
			go func(replicaNodeID string) {
				defer wg.Done()

				replica := redisCluster.Status.ReplicaMap[replicaNodeID]

				if err := RemoveNodeFromCluster(k8scl, redisCluster, logger, replica); err != nil {
					logger.Error(err, "Error removing replica from cluster", "ReplicaNodeID", replicaNodeID)
					errorCh <- err
					return
				}

				if err := DeleteRedisPod(ctx, k8scl, redisCluster, logger, replica.PodName); err != nil {
					logger.Error(err, "Error deleting replica Pod", "PodName", replica.PodName)
					errorCh <- err
					return
				}

				delete(redisCluster.Status.ReplicaMap, replicaNodeID)
				masterToReplicas[masterID]--
			}(replicaNodeID)

			removedReplicas++
		}

		if removedReplicas >= replicasToRemove {
			break
		}
	}

	wg.Wait()
	close(errorCh)

	if len(errorCh) > 0 {
		return <-errorCh
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
func AddMasterToCluster(k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, newMasters []redisv1beta1.RedisNodeStatus) error {
	var existingMaster redisv1beta1.RedisNodeStatus
	for _, master := range redisCluster.Status.MasterMap {
		existingMaster = master
		break
	}

	existingMasterAddress := GetRedisServerAddress(k8scl, logger, redisCluster.Namespace, existingMaster.PodName)
	for _, newMaster := range newMasters {
		newMasterAddress := GetRedisServerAddress(k8scl, logger, redisCluster.Namespace, newMaster.PodName)

		addNodeCmd := []string{"redis-cli", "--cluster", "add-node", newMasterAddress, existingMasterAddress}
		output, err := RunRedisCLI(k8scl, redisCluster.Namespace, existingMaster.PodName, addNodeCmd)
		if err != nil {
			logger.Error(err, "Error adding new master node to cluster", "Output", output)
			return err
		}

		if err := WaitForClusterStabilization(k8scl, redisCluster, logger); err != nil {
			logger.Error(err, "Error during cluster stabilization")
			return err
		}

		logger.Info("Successfully added new master node to cluster", "Node", newMaster.PodName)
	}

	return nil
}

// AddReplicaToMaster adds a replica node to the specified master node in the cluster
func AddReplicaToMaster(k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, newReplicas []redisv1beta1.RedisNodeStatus) error {
	for _, replica := range newReplicas {
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
func RebalanceCluster(k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, emptyMasterOpt bool) error {
	var existingMaster redisv1beta1.RedisNodeStatus
	for _, m := range redisCluster.Status.MasterMap {
		existingMaster = m
		break
	}

	existingMasterAddress := GetRedisServerAddress(k8scl, logger, redisCluster.Namespace, existingMaster.PodName)
	rebalanceCmd := []string{"redis-cli", "--cluster", "rebalance", existingMasterAddress, "--cluster-yes"}
	if emptyMasterOpt {
		rebalanceCmd = append(rebalanceCmd, "--cluster-use-empty-masters")
	}
	logger.Info("Rebalance command", "Command", rebalanceCmd)

	output, err := RunRedisCLI(k8scl, redisCluster.Namespace, existingMaster.PodName, rebalanceCmd)
	if err != nil {
		logger.Error(err, "Error during cluster rebalancing", "Output", output)
		return err
	}
	logger.Info("Cluster rebalancing completed successfully")
	return nil
}

// FixCluster fixes the cluster
func FixCluster(k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger) error {
	var existingMaster redisv1beta1.RedisNodeStatus
	for _, master := range redisCluster.Status.MasterMap {
		existingMaster = master
		break
	}

	existingMasterAddress := GetRedisServerAddress(k8scl, logger, redisCluster.Namespace, existingMaster.PodName)

	cmd := []string{
		"sh", "-c",
		fmt.Sprintf("echo yes | redis-cli --cluster fix %s", existingMasterAddress),
	}
	output, err := RunRedisCLI(k8scl, redisCluster.Namespace, existingMaster.PodName, cmd)
	if err != nil {
		logger.Error(err, "Failed to execute fix cmd", "Output", output)
		return fmt.Errorf("failed to excute fix cmd %s", output)
	}

	return nil
}

// handleFailedNode handels the failed node
func handleFailedNode(ctx context.Context, cl client.Client, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, failedNodes map[string]redisv1beta1.RedisFailedNodeStatus) error {
	var existingMaster redisv1beta1.RedisNodeStatus
	for _, master := range redisCluster.Status.MasterMap {
		existingMaster = master
		break
	}

	existingMasterPort := ExtractPortFromPodName(existingMaster.PodName)

	for _, node := range failedNodes {
		cmd := []string{"redis-cli", "-p", fmt.Sprintf("%d", existingMasterPort), "cluster", "forget", node.NodeID}
		output, err := RunRedisCLI(k8scl, redisCluster.Namespace, existingMaster.PodName, cmd)
		logger.Info("Output of redis-cli cluster forget command", "Output", output)
		if err != nil {
			logger.Error(err, "Error forgetting failed node", "Output", output)
			return err
		}

		err = cl.Get(ctx, client.ObjectKey{Namespace: redisCluster.Namespace, Name: node.PodName}, &corev1.Pod{})
		if err != nil {
			if errors.IsNotFound(err) {
				podName, err := GetPodNameByNodeID(k8scl, redisCluster.Namespace, node.NodeID, logger)
				if err != nil {
					logger.Error(err, "Failed to get Pod name by NodeID", "NodeID", node.NodeID)
					continue
				}
				err = DeleteRedisPod(ctx, k8scl, redisCluster, logger, podName)
				if err != nil {
					logger.Error(err, "Error deleting failed node Pod", "PodName", podName)
					return fmt.Errorf("failed to delete pod %s: %v", podName, err)
				}
				logger.Info("Successfully deleted failed node Pod", "PodName", podName)
			}
		}
		resetFailureCount(redisCluster, node.NodeID)
	}

	if err := FixCluster(k8scl, redisCluster, logger); err != nil {
		logger.Error(err, "Error during cluster fixing", "output", err)
		return err
	}

	return nil
}
