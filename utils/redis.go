package utils

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	redisv1beta1 "github.com/jaehanbyun/redis-operator/api/v1beta1"
	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"
)

// GetRedisServerAddress returns the full address (IP and port) of the Redis server
func GetRedisServerAddress(k8scl kubernetes.Interface, logger logr.Logger, namespace, podName string) string {
	ip := GetRedisServerIP(k8scl, logger, namespace, podName)
	port := ExtractPortFromPodName(podName)
	logger.Info(fmt.Sprintf("RedisServerAddress of %s: %s", podName, fmt.Sprintf("%s:%d", ip, port)))
	return fmt.Sprintf("%s:%d", ip, port)
}

// GetRedisServerIP returns the IP address of the Redis server
func GetRedisServerIP(k8scl kubernetes.Interface, logger logr.Logger, namespace string, podName string) string {
	pod, err := k8scl.CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		logger.Error(err, "Failed to get Redis Server IP", "Pod", podName)
		return ""
	}
	return pod.Status.PodIP
}

// ConfigureRedisClient sets up and returns a Redis client connected to the specified Redis server
func ConfigureRedisClient(k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger, podName string) *redis.Client {
	addr := GetRedisServerAddress(k8scl, logger, redisCluster.Namespace, podName)
	opts := &redis.Options{
		Addr: addr,
		DB:   0,
	}
	return redis.NewClient(opts)
}

// CreateClusterCommand generates a Redis cluster creation command with the addresses of given master nodes
func CreateClusterCommand(k8scl kubernetes.Interface, redisCluster *redisv1beta1.RedisCluster, logger logr.Logger) []string {
	cmd := []string{"redis-cli", "--cluster", "create"}

	for _, master := range redisCluster.Status.MasterMap {
		addr := GetRedisServerAddress(k8scl, logger, redisCluster.Namespace, master.PodName)
		cmd = append(cmd, addr)
	}

	cmd = append(cmd, "--cluster-replicas", "0", "--cluster-yes")

	logger.V(1).Info("Redis cluster creation command generated", "Command", cmd)
	return cmd
}

// GetRedisNodeID returns the node ID of a Redis server
func GetRedisNodeID(ctx context.Context, k8scl kubernetes.Interface, logger logr.Logger, namespace string, podName string) (string, error) {
	port := ExtractPortFromPodName(podName)

	cmd := []string{"redis-cli", "-h", "localhost", "-p", fmt.Sprintf("%d", port), "cluster", "myid"}

	output, err := RunRedisCLI(k8scl, namespace, podName, cmd)
	if err != nil {
		logger.Error(err, "Error getting Redis node ID", "Command", strings.Join(cmd, " "))
		return "", fmt.Errorf("failed to get Redis node ID: %w", err)
	}

	nodeID := strings.TrimSpace(output)
	if nodeID == "" {
		return "", fmt.Errorf("redis node ID not found")
	}

	return nodeID, nil
}

// RunRedisCLI executes a Redis CLI command on the Redis pod and returns the command output
func RunRedisCLI(k8scl kubernetes.Interface, namespace string, podName string, cmd []string) (string, error) {
	config, err := ctrl.GetConfig()
	if err != nil {
		return "", fmt.Errorf("failed to get Kubernetes client config: %w", err)
	}

	req := k8scl.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command:   cmd,
			Container: "redis",
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("failed to create SPDY executor: %w", err)
	}

	var exeOut, exeErr bytes.Buffer
	err = exec.StreamWithContext(context.Background(), remotecommand.StreamOptions{
		Stdout: &exeOut,
		Stderr: &exeErr,
		Tty:    false,
	})
	if err != nil {
		return "", fmt.Errorf("failed to execute command: %w, stdout: %s, stderr: %s", err, exeOut.String(), exeErr.String())
	}

	return exeOut.String(), nil
}
