package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:subresource:nostatus
// +kubebuilder:resource:shortName=eipt,scope=Cluster
// +kubebuilder:printcolumn:name="DestinationNetworks",type=string,JSONPath=".spec.destinationNetworks"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// EgressIPTraffic defines a set of networks outside the cluster network.
// These networks define the destination traffic that an EgressIP should be applied to
// when selected by an EgressIP's TrafficSelector.
type EgressIPTraffic struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`

	Spec EgressIPTrafficSpec `json:"spec"`
}

// EgressIPTrafficSpec defines the desired state description of EgressIPTraffic.
type EgressIPTrafficSpec struct {
	// DestinationNetworks is the list of Network CIDRs that defines external destinations
	// for IPv4 and/or IPv6.
	// +optional
	// +kubebuilder:validation:MaxItems=25
	DestinationNetworks []CIDR `json:"destinationNetworks,omitempty"`
}

// CIDR is a network CIDR. IPv4 or IPv6.
// +kubebuilder:validation:MaxLength=43
// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="CIDR must be in valid IPV4 or IPV6 CIDR format"
type CIDR string

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// EgressIPTrafficList contains a list of EgressIPTraffic
type EgressIPTrafficList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []EgressIPTraffic `json:"items"`
}
