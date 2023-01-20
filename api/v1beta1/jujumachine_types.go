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
	JujuMachineFinalizer = "juju.machine.x-k8s.io"
)

// JujuMachineSpec defines the desired state of JujuMachine
type JujuMachineSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Machine holds a pointer the ID of the machine that is returned when a machine gets created by the Juju API
	// This is generally a number like 0, 1, 2 etc
	// This is expected to eventually be set by the machine controller
	MachineID *string `json:"machineID,omitempty"`

	// Required fields for infra providers
	// +optional
	// This is expected to eventually be set by the machine controller
	ProviderID *string `json:"providerID,omitempty"`
}

// JujuMachineStatus defines the observed state of JujuMachine
type JujuMachineStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Required fields for infra providers
	//+kubebuilder:default=false
	Ready bool `json:"ready"`

	// Optional fields for infra providers
	FailureReason  string `json:"failureReason,omitempty"`  // error string for programs
	FailureMessage string `json:"failureMessage,omitempty"` // error string for humans
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// JujuMachine is the Schema for the jujumachines API
type JujuMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   JujuMachineSpec   `json:"spec,omitempty"`
	Status JujuMachineStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// JujuMachineList contains a list of JujuMachine
type JujuMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []JujuMachine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&JujuMachine{}, &JujuMachineList{})
}
