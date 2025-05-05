package k8sutils

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	redisv1beta1 "github.com/jaehanbyun/redis-operator/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// GenerateRedisProbe returns Liveness and Readiness probes
func GenerateRedisProbe(port int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{
					"redis-cli",
					"-p", fmt.Sprintf("%d", port),
					"ping",
				},
			},
		},
		InitialDelaySeconds: 15,
		PeriodSeconds:       10,
		FailureThreshold:    3,
	}
}

// GenerateAnnotations returns a list of annotations for prometheus metrics
func GenerateAnnotations(port int32) map[string]string {
	return map[string]string{
		"prometheus.io/port":   fmt.Sprintf("%d", port+5000),
		"prometheus.io/scrape": "true",
	}
}

// GenerateLabels returns a set of labels for the given info
func GenerateLabels(clusterName string, port int32) map[string]string {
	return map[string]string{
		"clusterName": clusterName,
		"port":        fmt.Sprintf("%d", port),
	}
}

// GenerateRedisClusterAsOwner returns a list of OwnerReferences for the given RedisCluster resource
func GenerateRedisClusterAsOwner(redisCluster *redisv1beta1.RedisCluster, crName string) []metav1.OwnerReference {
	return []metav1.OwnerReference{
		*metav1.NewControllerRef(redisCluster, redisv1beta1.GroupVersion.WithKind("RedisCluster")),
	}
}

// ExtractPortFromPodName extracts the port number from a Pod's name
func ExtractPortFromPodName(podName string) int32 {
	parts := strings.Split(podName, "-")
	portStr := parts[len(parts)-1]
	port, _ := strconv.Atoi(portStr)

	return int32(port)
}

// CreateMasterPod creates a Redis master Pod with the given port
func CreateMasterPod(ctx context.Context, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, port int32) error {
	podName := fmt.Sprintf("rediscluster-%s-%d", redisCluster.Name, port)
	masterPod := GenerateRedisPodDef(redisCluster, port, "")
	_, err := k8scl.CoreV1().Pods(redisCluster.Namespace).Create(ctx, masterPod, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return err
	}

	if err := WaitForPodReady(ctx, k8scl, redisCluster, logger, podName); err != nil {
		logger.Error(err, "Error waiting for master Pod to be ready", "Pod", podName)
		return err
	}

	redisNodeID, err := GetRedisNodeID(ctx, k8scl, logger, redisCluster.Namespace, podName)
	if err != nil {
		logger.Error(err, "Failed to extract Redis node ID", "Pod", podName)
		return err
	}

	if err := UpdatePodLabelWithRedisID(ctx, k8scl, redisCluster, logger, podName, redisNodeID); err != nil {
		logger.Error(err, "Failed to update Pod label", "Pod", podName)
		return err
	}

	redisCluster.Status.MasterMap[redisNodeID] = redisv1beta1.RedisNodeStatus{
		PodName: podName,
		NodeID:  redisNodeID,
	}

	return nil
}

// CreateReplicaPod creates a Redis replica Pod attached to the specified master node
func CreateReplicaPod(ctx context.Context, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, port int32, masterNodeID string) error {
	podName := fmt.Sprintf("rediscluster-%s-%d", redisCluster.Name, port)
	replicaPod := GenerateRedisPodDef(redisCluster, port, masterNodeID)
	_, err := k8scl.CoreV1().Pods(redisCluster.Namespace).Create(ctx, replicaPod, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return err
	}

	if err := WaitForPodReady(ctx, k8scl, redisCluster, logger, podName); err != nil {
		logger.Error(err, "Error waiting for replica Pod to be ready", "Pod", podName)
		return err
	}

	redisNodeID, err := GetRedisNodeID(ctx, k8scl, logger, redisCluster.Namespace, podName)
	if err != nil {
		logger.Error(err, "Failed to extract Redis node ID", "Pod", podName)
		return err
	}

	if err := UpdatePodLabelWithRedisID(ctx, k8scl, redisCluster, logger, podName, redisNodeID); err != nil {
		logger.Error(err, "Failed to update Pod label", "Pod", podName)
		return err
	}

	return nil
}

// WaitForPodReady waits until the specified Pod is in the Running state and its containers are ready
func WaitForPodReady(ctx context.Context, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, podName string) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("timeout waiting for Pod %s to be ready", podName)
		default:
			pod, err := k8scl.CoreV1().Pods(redisCluster.Namespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				if errors.IsNotFound(err) {
					logger.Info("Pod not yet created, retrying...", "Pod", podName)
					time.Sleep(2 * time.Second)
					continue
				}
				logger.Error(err, "Failed to get Pod", "Pod", podName)
				return err
			}

			if pod.Status.Phase == corev1.PodRunning && isContainerReady(pod, "redis") {
				logger.Info("Pod is ready", "Pod", podName)
				return nil
			}

			logger.Info("Pod not ready yet, retrying...", "Pod", podName)
			time.Sleep(5 * time.Second)
		}
	}
}

// GetNextAvailablePort returns the next available port
func GetNextAvailablePort(redisCluster *redisv1beta1.RedisCluster) int32 {
	port := redisCluster.Status.NextAvailablePort
	redisCluster.Status.NextAvailablePort++
	return port
}

// UpdatePodLabelWithRedisID updates the Pod's labels to include the Redis node ID
func UpdatePodLabelWithRedisID(ctx context.Context, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, podName string, redisNodeID string) error {
	pod, err := k8scl.CoreV1().Pods(redisCluster.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		logger.Error(err, "Failed to get Pod", "Pod", podName)
		return err
	}

	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels["redisNodeID"] = redisNodeID

	_, err = k8scl.CoreV1().Pods(redisCluster.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
	if err != nil {
		logger.Error(err, "Error updating Pod label", "Pod", podName)
		return err
	}

	logger.Info("Pod label updated successfully", "Pod", podName, "redisNodeID", redisNodeID)
	return nil
}

// GenerateRedisPodDef generates a Pod definition for a Redis instance
func GenerateRedisPodDef(redisCluster *redisv1beta1.RedisCluster, port int32, matchMasterNodeID string) *corev1.Pod {
	podName := fmt.Sprintf("rediscluster-%s-%d", redisCluster.Name, port)

	image := redisCluster.Spec.Image
	if redisCluster.Spec.Tag != "" {
		image = fmt.Sprintf("%s:%s", redisCluster.Spec.Image, redisCluster.Spec.Tag)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName,
			Namespace:   redisCluster.Namespace,
			Labels:      GenerateLabels(redisCluster.Name, port),
			Annotations: GenerateAnnotations(port),
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(redisCluster, redisv1beta1.GroupVersion.WithKind("RedisCluster")),
			},
		},
		Spec: corev1.PodSpec{
			HostNetwork: true, // Enable HostNetwork
			Containers: []corev1.Container{
				{
					Name:            "redis",
					Image:           image,
					ImagePullPolicy: redisCluster.Spec.ImagePullPolicy,
					Ports: []corev1.ContainerPort{
						{
							ContainerPort: port,
							Name:          "redis",
						},
						{
							ContainerPort: port + 10000,
							Name:          "redis-bus",
						},
					},
					Command: []string{
						"redis-server",
						"--port", fmt.Sprintf("%d", port),
						"--cluster-enabled", "yes",
						"--cluster-port", fmt.Sprintf("%d", port+10000),
						"--cluster-node-timeout", "5000",
						"--maxmemory", redisCluster.Spec.Maxmemory,
						"--protected-mode", "no",
					},
					ReadinessProbe: GenerateRedisProbe(port),
					LivenessProbe:  GenerateRedisProbe(port),
				},
			},
		},
	}

	// Add exporter container if enabled
	if redisCluster.Spec.Exporter != nil && redisCluster.Spec.Exporter.Enabled {
		exporterImage := "oliver006/redis_exporter:latest"
		if redisCluster.Spec.Exporter.Image != "" {
			exporterImage = redisCluster.Spec.Exporter.Image
			if redisCluster.Spec.Exporter.Tag != "" {
				exporterImage = fmt.Sprintf("%s:%s", redisCluster.Spec.Exporter.Image, redisCluster.Spec.Exporter.Tag)
			}
		}

		exporterContainer := corev1.Container{
			Name:  "redis-exporter",
			Image: exporterImage,
			Ports: []corev1.ContainerPort{
				{
					ContainerPort: port + 5000,
					Name:          "redis-exporter",
				},
			},
			Args: []string{
				"--web.listen-address", fmt.Sprintf(":%d", port+5000),
				"--redis.addr", fmt.Sprintf("localhost:%d", port),
			},
		}

		// Apply RedisExporter resource settings if provided
		if redisCluster.Spec.Exporter.Resources != nil {
			exporterContainer.Resources = *redisCluster.Spec.Exporter.Resources
		}

		pod.Spec.Containers = append(pod.Spec.Containers, exporterContainer)
	}

	// Apply Redis resource settings if provided
	if redisCluster.Spec.Resources != nil {
		pod.Spec.Containers[0].Resources = *redisCluster.Spec.Resources
	}

	// Add imagePullSecrets if specified
	if len(redisCluster.Spec.ImagePullSecrets) > 0 {
		pod.Spec.ImagePullSecrets = redisCluster.Spec.ImagePullSecrets
	}

	// Add securityContext if specified
	if redisCluster.Spec.SecurityContext != nil {
		pod.Spec.SecurityContext = redisCluster.Spec.SecurityContext
	}

	// Add nodeSelector if specified
	if redisCluster.Spec.NodeSelector != nil {
		pod.Spec.NodeSelector = redisCluster.Spec.NodeSelector
	}

	// Add tolerations if specified
	if len(redisCluster.Spec.Tolerations) > 0 {
		pod.Spec.Tolerations = redisCluster.Spec.Tolerations
	}

	// TODO: Implement volume mounts for persistence
	// Add persistence if enabled
	// if redisCluster.Spec.Persistence != nil && redisCluster.Spec.Persistence.Enabled {
	// 	fmt.Println("Persistence is enabled but not yet implemented")
	// }

	// If matchMasterNodeID is provided, set PodAntiAffinity
	if matchMasterNodeID != "" {
		if redisCluster.Spec.Affinity != nil {
			pod.Spec.Affinity = redisCluster.Spec.Affinity
		} else {
			pod.Spec.Affinity = &corev1.Affinity{
				PodAntiAffinity: &corev1.PodAntiAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "redisNodeID",
										Operator: metav1.LabelSelectorOpIn,
										Values:   []string{matchMasterNodeID},
									},
								},
							},
							TopologyKey: "kubernetes.io/hostname",
						},
					},
				},
			}
		}
	} else if redisCluster.Spec.Affinity != nil {
		pod.Spec.Affinity = redisCluster.Spec.Affinity
	}

	return pod
}

// GetPodNameByNodeID searches the Pod name associated with a given Redis node ID
func GetPodNameByNodeID(k8scl kubernetes.Interface, namespace string, nodeID string, logger logr.Logger) (string, error) {
	podList, err := k8scl.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("redisNodeID=%s", nodeID),
	})
	if err != nil {
		logger.Error(err, "Failed to search Pod using", "NodeID", nodeID)
		return "", err
	}
	if len(podList.Items) == 0 {
		return "", fmt.Errorf("cannot find Pod with NodeID %s in %s namespace", nodeID, namespace)
	}
	return podList.Items[0].Name, nil
}

// DeleteRedisPod deletes the specified Redis Pod from the cluster
func DeleteRedisPod(ctx context.Context, k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, podName string) error {
	err := k8scl.CoreV1().Pods(redisCluster.Namespace).Delete(ctx, podName, metav1.DeleteOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Pod to delete does not exist", "Pod", podName)
			return nil
		}
		logger.Error(err, "Failed to delete Pod", "Pod", podName)
		return err
	}

	logger.Info("Pod deletion requested", "Pod", podName)
	return nil
}

// GetMastersToRemove selects a list of master node IDs to be removed from the cluster
func GetMastersToRemove(redisCluster *redisv1beta1.RedisCluster, mastersToRemove int32, logger logr.Logger) []string {
	mastersToRemoveList := make([]string, 0, mastersToRemove)
	count := int32(0)

	for nodeID := range redisCluster.Status.MasterMap {
		mastersToRemoveList = append(mastersToRemoveList, nodeID)
		count++
		if count >= mastersToRemove {
			break
		}
	}

	logger.Info("Masters selected for removal", "Masters", mastersToRemoveList)
	return mastersToRemoveList
}

// GetReplicasToRemoveFromMaster selects replica node IDs attached to a specific master node for removal from the cluster
func GetReplicasToRemoveFromMaster(redisCluster *redisv1beta1.RedisCluster, masterNodeID string, replicasToRemove int32, logger logr.Logger) []string {
	var replicasToRemoveList []string
	count := int32(0)

	for nodeID, replica := range redisCluster.Status.ReplicaMap {
		if replica.MasterNodeID == masterNodeID {
			replicasToRemoveList = append(replicasToRemoveList, nodeID)
			count++
			if count >= replicasToRemove {
				break
			}
		}
	}

	logger.Info("Replicas selected for removal from master", "MasterNodeID", masterNodeID, "Replicas", replicasToRemoveList)
	return replicasToRemoveList
}

// WaitForNodeRole waits until the specified node transitions to the expected role (e.g., "slave" or "master")
func WaitForNodeRole(k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, nodeID string, expectedRole string) error {
	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("node %s did not transition to role %s", nodeID, expectedRole)
		case <-ticker.C:
			nodesInfo, err := GetClusterNodesInfo(context.TODO(), k8scl, redisCluster, logger)
			if err != nil {
				logger.Error(err, "Failed to get cluster node information")
				continue
			}

			for _, node := range nodesInfo {
				if node.NodeID == nodeID {
					if node.Role == expectedRole {
						return nil
					}
					break
				}
			}
		}
	}
}

// isContainerReady checks if the container is ready
func isContainerReady(pod *corev1.Pod, containerName string) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == containerName && cs.Ready {
			return true
		}
	}
	return false
}

// IsPodRunning checks if the specified Pod is running and the container is ready
func IsPodRunning(ctx context.Context, k8scl kubernetes.Interface, namespace, podName, containerName string, logger logr.Logger) (bool, error) {
	pod, err := k8scl.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Pod not found", "PodName", podName)
			return false, nil
		}
		logger.Error(err, "Failed to get Pod", "PodName", podName)
		return false, err
	}

	if pod.Status.Phase != corev1.PodRunning {
		logger.Info("Pod is not running", "PodName", podName, "Phase", pod.Status.Phase)
		return false, nil
	}

	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == containerName && cs.Ready {
			return true, nil
		}
	}

	logger.Info("Container is not ready", "PodName", podName, "ContainerName", containerName)
	return false, nil
}
