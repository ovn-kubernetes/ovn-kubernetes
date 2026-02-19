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
// and/or Cluster User Defined Networks together.
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
	//  any of the node subnets chosen by the platform.
	//  any of other connect subnets for other ClusterNetworkConnects that might be selecting same networks.
	//
	// Does not have a default value for the above reason so
	// that user takes care in setting non-overlapping subnets.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf", message="connectSubnets is immutable"
	// +kubebuilder:validation:XValidation:rule="size(self) != 2 || !isCIDR(self[0].cidr) || !isCIDR(self[1].cidr) || cidr(self[0].cidr).ip().family() != cidr(self[1].cidr).ip().family()", message="When 2 CIDRs are set, they must be from different IP families"
	// +kubebuilder:validation:XValidation:rule="size(self) != 2 || !isCIDR(self[0].cidr) || !isCIDR(self[1].cidr) || cidr(self[0].cidr).ip().family() == cidr(self[1].cidr).ip().family() || (cidr(self[0].cidr).ip().family() == 4 ? (32 - self[0].networkPrefix) == (128 - self[1].networkPrefix) : (128 - self[0].networkPrefix) == (32 - self[1].networkPrefix))", message="For dual-stack, networkPrefix must have matching host bits: (32 - ipv4NetworkPrefix) must equal (128 - ipv6NetworkPrefix)"
	ConnectSubnets []ConnectSubnet `json:"connectSubnets"`

	// connectivity specifies which connectivity types should be enabled for the connected networks.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:XValidation:rule="self.all(x, self.exists_one(y, x == y))",message="connectivity cannot contain duplicate values"
	Connectivity []ConnectivityType `json:"connectivity"`
}

// +kubebuilder:validation:XValidation:rule="isCIDR(self) && cidr(self) == cidr(self).masked()", message="CIDR must be a valid network address"
// +kubebuilder:validation:MaxLength=43
type CIDR string

// +kubebuilder:validation:XValidation:rule="!has(self.networkPrefix) || !isCIDR(self.cidr) || self.networkPrefix > cidr(self.cidr).prefixLength()", message="NetworkPrefix must be smaller than CIDR subnet"
// +kubebuilder:validation:XValidation:rule="!has(self.networkPrefix) || !isCIDR(self.cidr) || (cidr(self.cidr).ip().family() != 4 || self.networkPrefix < 32)", message="NetworkPrefix must < 32 for ipv4 CIDR"
type ConnectSubnet struct {
	// CIDR specifies ConnectSubnet, which is split into smaller subnets for every connected network.
	// This CIDR should be containing 2*((Number of L3 networks*Max Number of Nodes)+Number of L2 networks) IPs.
	// Example: cidr= "192.168.0.0/16", networkPrefix=24 and if the cluster has 128 nodes that means that you can
	// connect 256 layer3 networks and 0 layer2 networks OR 255 layer3 networks and 128 layer2 networks.
	//
	// CIDR also restricts the maximum number of networks that can be connected together
	// based on what CIDR range is picked. So choosing a large enough CIDR for future use cases
	// is important.
	//
	// The largest CIDR that can be used for this field is /16 (65536 IPs) because OVN
	// has a limit of 32K(2^15) tunnel keys per router. So we will only ever have 32K /31 or /127 slices
	// which is 2^16 IPs.
	// Having a CIDR greater than /16 will not be utilized fully for the same reason.
	// +required
	CIDR CIDR `json:"cidr"`

	// NetworkPrefix specifies the prefix length for every connected network.
	// This prefix length should be equal to or longer than the length of the CIDR prefix.
	//
	// For example, if the CIDR is 10.0.0.0/16 and the networkPrefix is 24,
	// then the connect subnet for each connected layer3 network will be 10.0.0.0/24, 10.0.1.0/24, 10.0.2.0/24 etc.
	//
	// For layer2 networks we will allocate the next available /networkPrefix range
	// that is then split into /31 or /127 slices for each layer2 network
	// A good practice is to set this to a value that ensures it contains more
	// than twice the number of maximum nodes planned to be deployed in the cluster.
	// Each node gets a /31 subnet for the layer3 networks, hence networkPrefix should
	// contain enough IPs for 4 times the maximum nodes planned
	// Example - recommended values:
	// if you plan to deploy 10 nodes, set the networkPrefix to /26 (40+ IPs)
	// if you plan to deploy 100 nodes, set the networkPrefix to /23 (400+ IPs)
	// if you plan to deploy 1000 nodes, set the networkPrefix to /20 (4000+ IPs)
	// if you plan to deploy 5000 nodes, set the networkPrefix to /17 (20000+ IPs)
	// This field restricts the maximum number of nodes that can be deployed in the cluster
	// and hence its good to plan this value carefully along with the CIDR.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=127
	// +required
	NetworkPrefix int32 `json:"networkPrefix"`
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
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// ClusterNetworkConnectList contains a list of ClusterNetworkConnect.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ClusterNetworkConnectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterNetworkConnect `json:"items"`
}
