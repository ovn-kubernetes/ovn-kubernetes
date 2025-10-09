/*
Copyright 2025.

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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
)

// VirtualPrivateNetworkConnect enables connecting multiple User Defined Networks together
// to form a virtual private network across namespaces.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=virtualprivatenetworkconnects,scope=Cluster
// +kubebuilder:singular=virtualprivatenetworkconnect
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
type VirtualPrivateNetworkConnect struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	// +required
	Spec VirtualPrivateNetworkConnectSpec `json:"spec"`

	// +optional
	Status VirtualPrivateNetworkConnectStatus `json:"status,omitempty"`
}

// VirtualPrivateNetworkConnectSpec defines the desired state of VirtualPrivateNetworkConnect.
type VirtualPrivateNetworkConnectSpec struct {
	// networkSelectors selects the networks to be connected together.
	// This can match User Defined Networks (UDNs) and/or Cluster User Defined Networks (CUDNs).
	//
	// +kubebuilder:validation:Required
	// +required
	NetworkSelectors types.NetworkSelectors `json:"networkSelectors"`

	// connectSubnets specifies the subnets used for interconnecting the selected networks.
	// This creates a shared subnet space that connected networks can use to communicate.
	// Can have at most 1 CIDR for each IP family (IPv4 and IPv6).
	//
	// +kubebuilder:validation:Required
	// +required
	ConnectSubnets DualStackCIDRs `json:"connectSubnets"`

	// featuresEnabled specifies which features should be enabled for the connected networks.
	// Different features control different aspects of inter-network connectivity and policies.
	//
	// +optional
	FeaturesEnabled []FeatureType `json:"featuresEnabled,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="isCIDR(self)", message="CIDR is invalid"
// +kubebuilder:validation:MaxLength=43
type CIDR string

// +kubebuilder:validation:MinItems=1
// +kubebuilder:validation:MaxItems=2
// +kubebuilder:validation:XValidation:rule="size(self) != 2 || !isCIDR(self[0]) || !isCIDR(self[1]) || cidr(self[0]).ip().family() != cidr(self[1]).ip().family()", message="When 2 CIDRs are set, they must be from different IP families"
type DualStackCIDRs []CIDR

// FeatureType represents the different features that can be enabled for connected networks.
// +kubebuilder:validation:Enum=PodNetworkConnectivity;ClusterIPServicesConnectivity;NetworkPolicyEnforcement
type FeatureType string

const (
	// PodNetworkConnectivity enables direct pod-to-pod communication across connected networks.
	PodNetworkConnectivity FeatureType = "PodNetworkConnectivity"

	// ClusterIPServicesConnectivity enables ClusterIP service access across connected networks.
	// Services from any connected network can be accessed from pods in other connected networks.
	ClusterIPServicesConnectivity FeatureType = "ClusterIPServicesConnectivity"

	// NetworkPolicyEnforcement enables network policy enforcement across connected networks.
	// Network policies can reference pods and services from other connected networks.
	NetworkPolicyEnforcement FeatureType = "NetworkPolicyEnforcement"
)

// VirtualPrivateNetworkConnectStatus defines the observed state of VirtualPrivateNetworkConnect.
type VirtualPrivateNetworkConnectStatus struct {
	// conditions represents the latest available observations of the VirtualPrivateNetworkConnect's current state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// connectedNetworks lists the networks that are currently connected by this VirtualPrivateNetworkConnect.
	// This provides visibility into which networks are actually connected.
	// +optional
	ConnectedNetworks []ConnectedNetworkStatus `json:"connectedNetworks,omitempty"`
}

// ConnectedNetworkStatus describes the status of a connected network.
type ConnectedNetworkStatus struct {
	// networkSelectionType indicates the type of network that is connected.
	// +required
	NetworkSelectionType types.NetworkSelectionType `json:"networkSelectionType"`

	// networkName is the name of the connected network.
	// +required
	NetworkName string `json:"networkName"`

	// namespace is the namespace of the connected network (only applicable for namespaced networks).
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// ready indicates whether this network is successfully connected and ready for inter-network communication.
	// +required
	Ready bool `json:"ready"`

	// message provides additional details about the connection status.
	// +optional
	Message string `json:"message,omitempty"`
}

// VirtualPrivateNetworkConnectList contains a list of VirtualPrivateNetworkConnect.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type VirtualPrivateNetworkConnectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualPrivateNetworkConnect `json:"items"`
}
