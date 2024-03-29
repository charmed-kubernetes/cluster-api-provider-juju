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
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

type JujuMachineTemplateResource struct {
	// Standard object's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	ObjectMeta clusterv1.ObjectMeta `json:"metadata,omitempty"`
	// Spec is the specification of the desired behavior of the machine.
	Spec JujuMachineSpec `json:"spec"`
}

// JujuMachineTemplateSpec defines the desired state of JujuMachineTemplate
type JujuMachineTemplateSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	Template JujuMachineTemplateResource `json:"template"`
}

//+kubebuilder:object:root=true
//+kubebuilder:resource:path=jujumachinetemplates,scope=Namespaced,categories=cluster-api,shortName=jmt
//+kubebuilder:storageversion

// JujuMachineTemplate is the Schema for the jujumachinetemplates API
type JujuMachineTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec JujuMachineTemplateSpec `json:"spec,omitempty"`
}

//+kubebuilder:object:root=true

// JujuMachineTemplateList contains a list of JujuMachineTemplate
type JujuMachineTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []JujuMachineTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&JujuMachineTemplate{}, &JujuMachineTemplateList{})
}
