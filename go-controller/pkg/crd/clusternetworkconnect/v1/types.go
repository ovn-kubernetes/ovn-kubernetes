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

// ClusterNetworkConnect enables connecting multiple User Defined Networks
// or Cluster User Defined Networks together.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:openapi-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=clusternetworkconnects,scope=Cluster,shortName=cnc,singular=clusternetworkconnect
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.status"
type ClusterNetworkConnect struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	// +required
	Spec ClusterNetworkConnectSpec `json:"spec"`

	// +optional
	Status ClusterNetworkConnectStatus `json:"status,omitempty"`
}

// ClusterNetworkConnectSpec defines the desired state of ClusterNetworkConnect.
// +kubebuilder:validation:XValidation:rule="!self.networkSelectors.exists(i, i.networkSelectionType != 'ClusterUserDefinedNetworks' && i.networkSelectionType != 'PrimaryUserDefinedNetworks')",message="Only ClusterUserDefinedNetworks or PrimaryUserDefinedNetworks can be selected"
type ClusterNetworkConnectSpec struct {
	// networkSelectors selects the networks to be connected together.
	// This can match User Defined Networks (UDNs) and/or Cluster User Defined Networks (CUDNs).
	// Only ClusterUserDefinedNetworkSelector and PrimaryUserDefinedNetworkSelector can be selected.
	//
	// +kubebuilder:validation:Required
	// +required
	NetworkSelectors types.NetworkSelectors `json:"networkSelectors"`

	// connectSubnets specifies the subnets used for interconnecting the selected networks.
	// This creates a shared subnet space that connected networks can use to communicate.
	// Can have at most 1 CIDR for each IP family (IPv4 and IPv6).
	// Must not overlap with:
	//  any of the pod subnets used by the selected networks.
	//  any of the transit subnets used by the selected networks.
	//  any of the service CIDR range used in the cluster.
	//  any of the join subnet of the selected networks to be connected.
	//  any of the masquerade subnet range used in the cluster.
	//  any of the node subnets for choosen by the platform.
	//  any of other connect subnets for other ClusterNetworkConnects that might be selecting same networks.
	//
	// Does not have a default value for the above reason so
	// that user takes care in setting non-overlapping subnets.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +required
	// +kubebuilder:validation:XValidation:rule="size(self) != 2 || !isCIDR(self[0].cidr) || !isCIDR(self[1].cidr) || cidr(self[0].cidr).ip().family() != cidr(self[1].cidr).ip().family()", message="When 2 CIDRs are set, they must be from different IP families"
	ConnectSubnets []ConnectSubnet `json:"connectSubnets"`

	// connectivityEnabled specifies which connectivity types should be enabled for the connected networks.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:XValidation:rule="self.all(x, self.exists_one(y, x == y))",message="connectivityEnabled cannot contain duplicate values"
	ConnectivityEnabled []ConnectivityType `json:"connectivityEnabled"`
}

// +kubebuilder:validation:XValidation:rule="isCIDR(self) && cidr(self) == cidr(self).masked()", message="CIDR must be a valid network address"
// +kubebuilder:validation:MaxLength=43
type CIDR string

// +kubebuilder:validation:XValidation:rule="!has(self.networkPrefix) || !isCIDR(self.cidr) || self.networkPrefix > cidr(self.cidr).prefixLength()", message="NetworkPrefix must be smaller than CIDR subnet"
// +kubebuilder:validation:XValidation:rule="!has(self.networkPrefix) || !isCIDR(self.cidr) || (cidr(self.cidr).ip().family() != 4 || self.networkPrefix < 32)", message="NetworkPrefix must < 32 for ipv4 CIDR"
type ConnectSubnet struct {
	// CIDR specifies ConnectSubnet, which is split into smaller subnets for every connected network.
	// This CIDR should be
	//
	// +required
	CIDR CIDR `json:"cidr,omitempty"`

	// NetworkPrefix specifies the prefix length for every connected network.
	// This prefix length should be smaller than the CIDR prefix length.
	//
	// For example, if the CIDR is 10.0.0.0/8 and the NetworkPrefix is 24,
	// then the connect subnet for each connected network will be 10.0.0.0/24.
	// A good practice is to set this to a value that ensures it contains more
	// than twice the number of maximum nodes planned to be deployed in the cluster.
	// Example; if you plan to deploy 10 nodes, set the NetworkPrefix to /27 (20+ IPs)
	// if you plan to deploy 100 nodes, set the NetworkPrefix to /24 (200+ IPs)
	// if you plan to deploy 1000 nodes, set the NetworkPrefix to /21 (2000+ IPs)
	// if you plan to deploy 5000 nodes, set the NetworkPrefix to /18 (10000+ IPs)
	// This field restricts the maximum number of nodes that can be deployed in the cluster
	// and hence its good to plan this value carefully along with the CIDR.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=127
	// +required
	NetworkPrefix int32 `json:"networkPrefix,omitempty"`
}

// ConnectivityType represents the different connectivity types that can be enabled for connected networks.
// +kubebuilder:validation:Enum=PodNetwork;ClusterIPServiceNetwork
type ConnectivityType string

const (
	// PodNetwork enables direct pod-to-pod communication across connected networks.
	PodNetwork ConnectivityType = "PodNetwork"

	// ClusterIPServiceNetwork enables ClusterIP service access across connected networks.
	ClusterIPServiceNetwork ConnectivityType = "ClusterIPServiceNetwork"
)

// StatusType represents the status of a ClusterNetworkConnect.
// +kubebuilder:validation:Enum=Success;Failure
type StatusType string

const (
	// Success indicates that the ClusterNetworkConnect has been successfully applied.
	Success StatusType = "Success"

	// Failure indicates that the ClusterNetworkConnect has failed to be applied.
	Failure StatusType = "Failure"
)

// ClusterNetworkConnectStatus defines the observed state of ClusterNetworkConnect.
type ClusterNetworkConnectStatus struct {
	// status is a concise indication of whether the ClusterNetworkConnect
	// resource is applied with success.
	// +kubebuilder:validation:Optional
	Status StatusType `json:"status,omitempty"`

	// conditions is an array of condition objects indicating details about
	// status of ClusterNetworkConnect object.
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ClusterNetworkConnectList contains a list of ClusterNetworkConnect.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ClusterNetworkConnectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterNetworkConnect `json:"items"`
}
