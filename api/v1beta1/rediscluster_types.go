/*
Copyright 2024 jaehanbyun.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1beta1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ExporterSpec defines the configuration for Redis metrics exporter
type ExporterSpec struct {
	Enabled   bool                         `json:"enabled"`
	Image     string                       `json:"image,omitempty"`
	Tag       string                       `json:"tag,omitempty"`
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// PersistenceSpec defines the persistence configuration
type PersistenceSpec struct {
	Enabled      bool   `json:"enabled"`
	StorageClass string `json:"storageClass,omitempty"`
	Size         string `json:"size,omitempty"`
}

// RedisClusterSpec defines the desired state of RedisCluster
type RedisClusterSpec struct {
	Image            string                        `json:"image"`
	Tag              string                        `json:"tag,omitempty"`
	ImagePullPolicy  corev1.PullPolicy             `json:"imagePullPolicy,omitempty"`
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	Masters          int32                         `json:"masters"`
	Replicas         int32                         `json:"replicas"`
	BasePort         int32                         `json:"basePort"`
	Maxmemory        string                        `json:"maxMemory"`
	Resources        *corev1.ResourceRequirements  `json:"resources,omitempty"`
	SecurityContext  *corev1.PodSecurityContext    `json:"securityContext,omitempty"`
	NodeSelector     map[string]string             `json:"nodeSelector,omitempty"`
	Tolerations      []corev1.Toleration           `json:"tolerations,omitempty"`
	Affinity         *corev1.Affinity              `json:"affinity,omitempty"`
	Persistence      *PersistenceSpec              `json:"persistence,omitempty"`
	Exporter         *ExporterSpec                 `json:"exporter,omitempty"`
}

// RedisClusterStatus defines the observed state of RedisCluster
type RedisClusterStatus struct {
	MasterMap         map[string]RedisNodeStatus `json:"masterMap,omitempty"`        // Key: NodeID
	ReplicaMap        map[string]RedisNodeStatus `json:"replicaMap,omitempty"`       // Key: NodeID
	FailedMasterMap   map[string]RedisNodeStatus `json:"failedMasterMap,omitempty"`  // Key: NodeID
	FailedReplicaMap  map[string]RedisNodeStatus `json:"failedReplicaMap,omitempty"` // Key: NodeID
	NextAvailablePort int32                      `json:"nextAvailablePort,omitempty"`
}

type RedisNodeStatus struct {
	PodName      string `json:"podName"`
	NodeID       string `json:"nodeID"`
	MasterNodeID string `json:"masterNodeID,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="MASTERS",type=integer,JSONPath=`.spec.masters`,description="Number of master nodes"
// +kubebuilder:printcolumn:name="REPLICAS",type=integer,JSONPath=`.spec.replicas`,description="Number of replica nodes"
// +kubebuilder:printcolumn:name="MAXMEMORY",type=string,JSONPath=`.spec.maxMemory`,description="Maximum memory for Redis nodes"
// +kubebuilder:printcolumn:name="BASEPORT",type=integer,JSONPath=`.spec.basePort`,description="Base port for Redis nodes"
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`,description="Age of the Redis cluster"
// RedisCluster is the Schema for the redisclusters API
type RedisCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RedisClusterSpec   `json:"spec,omitempty"`
	Status RedisClusterStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// RedisClusterList contains a list of RedisCluster
type RedisClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RedisCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RedisCluster{}, &RedisClusterList{})
}
