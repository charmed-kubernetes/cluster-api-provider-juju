/*
Copyright 2022.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

const (
	JujuClusterFinalizer = "juju.cluster.x-k8s.io"
)

// JujuClusterSpec defines the desired state of JujuCluster
type JujuClusterSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	//+kubebuilder:default="cluster"
	ControllerServiceType string `json:"controllerServiceType"`
	Endpoint              string `json:"endpoint"`
}

// JujuClusterStatus defines the observed state of JujuCluster
type JujuClusterStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// Ready denotes that the maas cluster (infrastructure) is ready.
	//+kubebuilder:default=false
	Ready bool `json:"ready"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// JujuCluster is the Schema for the jujuclusters API
type JujuCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   JujuClusterSpec   `json:"spec,omitempty"`
	Status JujuClusterStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// JujuClusterList contains a list of JujuCluster
type JujuClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []JujuCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&JujuCluster{}, &JujuClusterList{})
}
