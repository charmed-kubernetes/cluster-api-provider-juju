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

// APIEndpoint represents a reachable Kubernetes API endpoint.
type APIEndpoint struct {
	// Host is the hostname on which the API server is serving.
	Host string `json:"host"`

	// Port is the port on which the API server is serving.
	Port int `json:"port"`
}

type Attrs map[string]string

type RegionConfig map[string]Attrs

type Region struct {
	// Name is the name of the region.
	Name string `json:"name"`

	// Endpoint is the region's primary endpoint URL.
	Endpoint string `json:"endpoint"`

	// IdentityEndpoint is the region's identity endpoint URL.
	// If the cloud/region does not have an identity-specific
	// endpoint URL, this will be empty.
	IdentityEndpoint string `json:"identityEndpoint,omitempty"`

	// StorageEndpoint is the region's storage endpoint URL.
	// If the cloud/region does not have a storage-specific
	// endpoint URL, this will be empty.
	StorageEndpoint string `json:"storageEndpoint,omitempty"`
}

type AuthType string

type AuthTypes []AuthType

type Cloud struct {
	// Name of the cloud.
	Name string `json:"name"`

	// Type is the type of cloud, eg ec2, openstack etc.
	// This is one of the provider names registered with
	// environs.RegisterProvider.
	Type string `json:"type"`

	// HostCloudRegion represents the k8s host cloud. The format is <cloudType>/<region>.
	HostCloudRegion string `json:"hostCloudRegion,omitempty"`

	// Description describes the type of cloud.
	Description string `json:"description,omitempty"`

	// AuthTypes are the authentication modes supported by the cloud.
	AuthTypes AuthTypes `json:"authTypes"`

	// Endpoint is the default endpoint for the cloud regions, may be
	// overridden by a region.
	Endpoint string `json:"endpoint,omitempty"`

	// IdentityEndpoint is the default identity endpoint for the cloud
	// regions, may be overridden by a region.
	IdentityEndpoint string `json:"identityEndpoint,omitempty"`

	// StorageEndpoint is the default storage endpoint for the cloud
	// regions, may be overridden by a region.
	StorageEndpoint string `json:"storageEndpoint,omitempty"`

	// Regions are the regions available in the cloud.
	//
	// Regions is a slice, and not a map, because order is important.
	// The first region in the slice is the default region for the
	// cloud.
	Regions []Region `json:"regions,omitempty"`

	// Config contains optional cloud-specific configuration to use
	// when bootstrapping Juju in this cloud. The cloud configuration
	// will be combined with Juju-generated, and user-supplied values;
	// user-supplied values taking precedence.
	Config map[string]string `json:"config,omitempty"`

	// RegionConfig contains optional region specific configuration.
	// Like Config above, this will be combined with Juju-generated and user
	// supplied values; with user supplied values taking precedence.
	RegionConfig RegionConfig `json:"regionConfig,omitempty"`

	// CACertificates contains an optional list of Certificate
	// Authority certificates to be used to validate certificates
	// of cloud infrastructure components
	// The contents are Base64 encoded x.509 certs.
	CACertificates []string `json:"CACertificates,omitempty"`

	// SkipTLSVerify is true if the client should be asked not to
	// validate certificates. It is not recommended for production clouds.
	// It is secure (false) by default.
	SkipTLSVerify bool `json:"skipTLSVerify,omitempty"`

	// IsControllerCloud is true when this is the cloud used by the controller.
	IsControllerCloud bool `json:"isControllerCloud,omitempty"`
}

// JujuClusterSpec defines the desired state of JujuCluster
// +kubebuilder:object:generate=true
type JujuClusterSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Controller service type defines what type of service will be created for the
	// juju controller. Should be cluster, loadbalancer, or external
	//+kubebuilder:default="cluster"
	ControllerServiceType string `json:"controllerServiceType"`

	// CloudName is used to specify a predefined cloud such as aws or azure that Juju works with out of the box
	// If unspecified, a Cloud must be provided
	// +optional
	CloudName string `json:"cloudName,omitempty"`

	// Cloud is used to define a cloud for clouds that Juju does not work with out of the box
	// such as VSphere.
	// If unspecified, a CloudName must be provided
	// +optional
	Cloud *Cloud `json:"cloud,omitempty"`

	// Required fields for infra providers
	// ControlPlaneEndpoint represents the endpoint used to communicate with the control plane.
	// Expected to eventually be set by the user/controller
	// +optional
	ControlPlaneEndpoint APIEndpoint `json:"controlPlaneEndpoint"`

	// Optional fields for infra providers
	FailureReason  string `json:"failureReason,omitempty"`  // error string for programs
	FailureMessage string `json:"failureMessage,omitempty"` // error string for humans
}

// JujuClusterStatus defines the observed state of JujuCluster
type JujuClusterStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Required fields for infra providers
	// Ready denotes that the cluster (infrastructure) is ready.
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
