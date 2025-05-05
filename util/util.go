package util

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
	redisv1beta1 "github.com/jaehanbyun/redis-operator/api/v1beta1"
	"github.com/jaehanbyun/redis-operator/k8sutils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type node struct {
	IP   string `json:"ip"`
	Port int32  `json:"port"`
}

type AlertManagerPayload struct {
	Receiver          string            `json:"receiver"`
	Status            string            `json:"status"`
	Alerts            []Alert           `json:"alerts"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
}

type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	EndsAt       string            `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

type AutoScalingAction struct {
	Type          string `json:"type"` // "master" or "replica"
	ClusterName   string `json:"clusterName"`
	Namespace     string `json:"namespace"`
	Count         int32  `json:"count"`
	ScalingAction string `json:"scalingAction"` // "up" or "down"
}

func StartHTTPServer(cl client.Client) error {
	r := mux.NewRouter()
	r.HandleFunc("/cluster/nodes", func(w http.ResponseWriter, r *http.Request) {
		handleClusterNodesRequest(w, r, cl)
	}).Methods("GET")

	r.HandleFunc("/webhooks/alertmanager", func(w http.ResponseWriter, r *http.Request) {
		handleAlertManagerWebhook(w, r, cl)
	}).Methods("POST")

	return http.ListenAndServe(":9090", r)
}

// handleClusterNodesRequest is a function that handles the cluster nodes request.
func handleClusterNodesRequest(w http.ResponseWriter, r *http.Request, cl client.Client) {
	clusterName := r.URL.Query().Get("clusterName")
	if clusterName == "" {
		http.Error(w, "clusterName is required", http.StatusBadRequest)
		return
	}

	redisClusterList := &redisv1beta1.RedisClusterList{}
	err := cl.List(context.TODO(), redisClusterList, &client.ListOptions{})
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to list RedisClusters: %v", err), http.StatusInternalServerError)
		return
	}

	var redisCluster *redisv1beta1.RedisCluster
	for _, rc := range redisClusterList.Items {
		if rc.Name == clusterName {
			redisCluster = &rc
			break
		}
	}

	if redisCluster == nil {
		http.Error(w, "RedisCluster not found", http.StatusNotFound)
		return
	}

	podList := &corev1.PodList{}
	err = cl.List(context.TODO(), podList, client.InNamespace(redisCluster.Namespace), client.MatchingLabels{"clusterName": clusterName})
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to list pods: %v", err), http.StatusInternalServerError)
		return
	}

	var nodes []node
	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodRunning {
			port := k8sutils.ExtractPortFromPodName(pod.Name)
			node := node{
				IP:   pod.Status.PodIP,
				Port: port,
			}
			nodes = append(nodes, node)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(nodes); err != nil {
		http.Error(w, fmt.Sprintf("Failed to encode nodes to JSON: %v", err), http.StatusInternalServerError)
		return
	}
}

// handleAlertManagerWebhook is a function that handles Alertmanager webhook.
func handleAlertManagerWebhook(w http.ResponseWriter, r *http.Request, cl client.Client) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var payload AlertManagerPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "Invalid JSON format", http.StatusBadRequest)
		return
	}

	for _, alert := range payload.Alerts {
		if alert.Status == "firing" {
			action, err := parseAlertAction(alert)
			if err != nil {
				log.Printf("Failed to parse alert action: %v", err)
				continue
			}

			if err := applyAutoScaling(cl, action); err != nil {
				log.Printf("Failed to apply auto-scaling: %v", err)
				continue
			}

			log.Printf("Successfully applied auto-scaling: %+v", action)
		}
	}

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("Alerts processed")); err != nil {
		log.Printf("Error writing response: %v", err)
	}
}

func parseAlertAction(alert Alert) (*AutoScalingAction, error) {
	clusterName, ok := alert.Labels["redis_cluster"]
	if !ok {
		return nil, fmt.Errorf("missing required label: redis_cluster")
	}

	namespace, ok := alert.Labels["namespace"]
	if !ok {
		namespace = "default"
	}

	var action AutoScalingAction
	action.ClusterName = clusterName
	action.Namespace = namespace

	alertName, ok := alert.Labels["alertname"]
	if !ok {
		return nil, fmt.Errorf("missing required label: alertname")
	}

	switch {
	case strings.Contains(alertName, "HighMemoryUsage"):
		action.Type = "master"
		action.ScalingAction = "up"
		action.Count = 1
	case strings.Contains(alertName, "HighThroughput"):
		action.Type = "replica"
		action.ScalingAction = "up"
		action.Count = 1
	case strings.Contains(alertName, "LowMemoryUsage"):
		action.Type = "master"
		action.ScalingAction = "down"
		action.Count = 1
	case strings.Contains(alertName, "LowThroughput"):
		action.Type = "replica"
		action.ScalingAction = "down"
		action.Count = 1
	default:
		return nil, fmt.Errorf("unknown alert name: %s", alertName)
	}

	if countStr, ok := alert.Annotations["scale_count"]; ok {
		count, err := strconv.ParseInt(countStr, 10, 32)
		if err == nil && count > 0 {
			action.Count = int32(count)
		}
	}

	return &action, nil
}

// applyAutoScaling is a function that applies auto-scaling to a Redis cluster.
func applyAutoScaling(cl client.Client, action *AutoScalingAction) error {
	var redisCluster redisv1beta1.RedisCluster
	if err := cl.Get(context.Background(), types.NamespacedName{
		Name:      action.ClusterName,
		Namespace: action.Namespace,
	}, &redisCluster); err != nil {
		return fmt.Errorf("failed to get RedisCluster: %v", err)
	}

	// 타입에 따라 마스터 또는 레플리카 수 조정
	switch action.Type {
	case "master":
		if action.ScalingAction == "up" {
			redisCluster.Spec.Masters += action.Count
		} else if action.ScalingAction == "down" && redisCluster.Spec.Masters > action.Count {
			redisCluster.Spec.Masters -= action.Count
		} else {
			return fmt.Errorf("invalid scaling down action: current masters %d, scaling down by %d",
				redisCluster.Spec.Masters, action.Count)
		}
	case "replica":
		if action.ScalingAction == "up" {
			redisCluster.Spec.Replicas += action.Count
		} else if action.ScalingAction == "down" && redisCluster.Spec.Replicas > action.Count {
			redisCluster.Spec.Replicas -= action.Count
		} else {
			return fmt.Errorf("invalid scaling down action: current replicas %d, scaling down by %d",
				redisCluster.Spec.Replicas, action.Count)
		}
	default:
		return fmt.Errorf("unknown scaling type: %s", action.Type)
	}

	if err := cl.Update(context.Background(), &redisCluster); err != nil {
		return fmt.Errorf("failed to update RedisCluster: %v", err)
	}

	return nil
}
