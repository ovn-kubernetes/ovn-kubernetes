// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +genclient
// +genclient:nonNamespaced
// +genclient:noStatus
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=tc,scope=Cluster
// +kubebuilder:printcolumn:name="Matches",type=string,JSONPath=".spec.matches[*].destinationCIDR"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// TrafficClass defines matches selectable by EgressIP resources.
type TrafficClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TrafficClassSpec `json:"spec"`
}

// TrafficClassSpec defines destination matches.
type TrafficClassSpec struct {
	// Matches are ORed together.
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=25
	Matches []TrafficMatch `json:"matches"`
}

// TrafficMatch matches a network, optionally restricted to L4 protocols and ports.
type TrafficMatch struct {
	// DestinationCIDR must use the canonical network address.
	// +kubebuilder:validation:MaxLength=43
	// +kubebuilder:validation:XValidation:rule="isCIDR(self) && cidr(self) == cidr(self).masked()",message="CIDR must be a valid network address"
	DestinationCIDR string `json:"destinationCIDR"`
	// Protocols are ORed together; omission matches all protocols and ports.
	// +optional
	// +listType=atomic
	Protocols []ProtocolMatch `json:"protocols,omitempty"`
}

// ProtocolMatch selects exactly one transport protocol.
// +kubebuilder:validation:XValidation:rule="has(self.tcp) ? !has(self.udp) && !has(self.sctp) : has(self.udp) != has(self.sctp)",message="exactly one of tcp, udp, or sctp must be set"
type ProtocolMatch struct {
	// TCP selects TCP traffic.
	// +optional
	TCP *PortProtocol `json:"tcp,omitempty"`
	// UDP selects UDP traffic.
	// +optional
	UDP *PortProtocol `json:"udp,omitempty"`
	// SCTP selects SCTP traffic.
	// +optional
	SCTP *PortProtocol `json:"sctp,omitempty"`
}

// PortProtocol optionally restricts both destination and source ports (AND semantics).
type PortProtocol struct {
	// DestinationPort matches the packet destination port.
	// +optional
	DestinationPort *Port `json:"destinationPort,omitempty"`
	// SourcePort matches the packet source port.
	// +optional
	SourcePort *Port `json:"sourcePort,omitempty"`
}

// Port selects exactly one port number or inclusive range.
// +kubebuilder:validation:XValidation:rule="has(self.number) != has(self.range)",message="exactly one of number or range must be set"
type Port struct {
	// Number selects one port.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Number *int32 `json:"number,omitempty"`
	// Range selects an inclusive range of ports.
	// +optional
	Range *PortRange `json:"range,omitempty"`
}

// PortRange is an inclusive range of transport ports.
// +kubebuilder:validation:XValidation:rule="self.end >= self.start",message="end must be greater than or equal to start"
type PortRange struct {
	// Start is the first port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Start int32 `json:"start"`
	// End is the last port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	End int32 `json:"end"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// TrafficClassList is a list of TrafficClasses.
type TrafficClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TrafficClass `json:"items"`
}
