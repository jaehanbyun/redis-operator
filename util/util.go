package util

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-logr/logr"
	"github.com/gorilla/mux"
	redisv1beta1 "github.com/jaehanbyun/redis-operator/api/v1beta1"
	"github.com/jaehanbyun/redis-operator/k8sutils"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func StartHTTPServer(ctx context.Context, cl client.Client, logger logr.Logger) error {
	r := mux.NewRouter()
	r.HandleFunc("/cluster/nodes", func(w http.ResponseWriter, r *http.Request) {
		handleClusterNodesRequest(w, r, cl)
	}).Methods("GET")

	server := &http.Server{
		Addr:    ":9090",
		Handler: r,
	}

	go func() {
		logger.Info("Starting HTTP server on :9090")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error(err, "HTTP server failed")
		}
	}()

	<-ctx.Done()
	if err := server.Shutdown(context.Background()); err != nil {
		logger.Error(err, "HTTP server shutdown failed")
		return err
	}

	return nil
}

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

	var nodes []struct {
		PodName string `json:"podName"`
		IP      string `json:"ip"`
		Port    int32  `json:"port"`
	}

	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodRunning {
			port := k8sutils.ExtractPortFromPodName(pod.Name)
			nodes = append(nodes, struct {
				PodName string `json:"podName"`
				IP      string `json:"ip"`
				Port    int32  `json:"port"`
			}{
				PodName: pod.Name,
				IP:      pod.Status.PodIP,
				Port:    port,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(nodes); err != nil {
		http.Error(w, fmt.Sprintf("Failed to encode nodes to JSON: %v", err), http.StatusInternalServerError)
		return
	}
}
