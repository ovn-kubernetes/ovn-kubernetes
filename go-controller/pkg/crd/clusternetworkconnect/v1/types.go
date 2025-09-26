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
	//
	// +kubebuilder:validation:Required
	// +required
	ConnectSubnets DualStackCIDRs `json:"connectSubnets"`

	// featuresEnabled specifies which features should be enabled for the connected networks.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:XValidation:rule="self.all(x, self.exists_one(y, x == y))"
	FeaturesEnabled []FeatureType `json:"featuresEnabled"`
}

// +kubebuilder:validation:XValidation:rule="isCIDR(self)", message="CIDR is invalid"
// +kubebuilder:validation:MaxLength=43
type CIDR string

// +kubebuilder:validation:MinItems=1
// +kubebuilder:validation:MaxItems=2
// +kubebuilder:validation:XValidation:rule="size(self) != 2 || !isCIDR(self[0]) || !isCIDR(self[1]) || cidr(self[0]).ip().family() != cidr(self[1]).ip().family()", message="When 2 CIDRs are set, they must be from different IP families"
type DualStackCIDRs []CIDR

// FeatureType represents the different features that can be enabled for connected networks.
// +kubebuilder:validation:Enum=PodNetwork;ClusterIPServiceNetwork
type FeatureType string

const (
	// PodNetwork enables direct pod-to-pod communication across connected networks.
	PodNetwork FeatureType = "PodNetwork"

	// ClusterIPServiceNetwork enables ClusterIP service access across connected networks.
	ClusterIPServiceNetwork FeatureType = "ClusterIPServiceNetwork"
)

// ClusterNetworkConnectStatus defines the observed state of ClusterNetworkConnect.
type ClusterNetworkConnectStatus struct {
	// status is a concise indication of whether the ClusterNetworkConnect
	// resource is applied with success.
	// +kubebuilder:validation:Optional
	Status string `json:"status,omitempty"`

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
