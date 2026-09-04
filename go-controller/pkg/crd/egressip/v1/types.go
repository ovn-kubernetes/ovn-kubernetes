// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// When we bump to Kubernetes 1.19 we should get this fix: https://github.com/kubernetes/kubernetes/pull/89660
// Until then Assigned Nodes/EgressIPs can only print the first item in the status.

// +genclient
// +genclient:nonNamespaced
// +genclient:noStatus
// +resource:path=egressip
// +kubebuilder:resource:shortName=eip,scope=Cluster
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:printcolumn:name="EgressIPs",type=string,JSONPath=".spec.egressIPs[*]"
// +kubebuilder:printcolumn:name="Assigned Node",type=string,JSONPath=".status.items[*].node"
// +kubebuilder:printcolumn:name="Assigned EgressIPs",type=string,JSONPath=".status.items[*].egressIP"
// EgressIP is a CRD allowing the user to define a fixed
// source IP for all egress traffic originating from any pods which
// match the EgressIP resource according to its spec definition.
type EgressIP struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Specification of the desired behavior of EgressIP.
	Spec EgressIPSpec `json:"spec"`
	// Observed status of EgressIP. Read-only.
	// +optional
	Status EgressIPStatus `json:"status,omitempty"`
}

type EgressIPStatus struct {
	// The list of assigned egress IPs and their corresponding node assignment.
	// +listType=atomic
	Items []EgressIPStatusItem `json:"items"`
}

// The per node status, for those egress IPs who have been assigned.
type EgressIPStatusItem struct {
	// Assigned node name
	Node string `json:"node"`
	// Assigned egress IP
	EgressIP string `json:"egressIP"`
}

// EgressIPSpec is a desired state description of EgressIP.
type EgressIPSpec struct {
	// EgressIPs is the list of egress IP addresses requested. Can be IPv4 and/or IPv6.
	// This field is mandatory.
	// +listType=atomic
	EgressIPs []string `json:"egressIPs"`
	// NamespaceSelector applies the egress IP only to the namespace(s) whose label
	// matches this definition. This field is mandatory.
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`
	// PodSelector applies the egress IP only to the pods whose label
	// matches this definition. This field is optional, and in case it is not set:
	// results in the egress IP being applied to all pods in the namespace(s)
	// matched by the NamespaceSelector. In case it is set: is intersected with
	// the NamespaceSelector, thus applying the egress IP to the pods
	// (in the namespace(s) already matched by the NamespaceSelector) which
	// match this pod selector.
	// +optional
	PodSelector metav1.LabelSelector `json:"podSelector,omitempty"`

	// EgressNodeSelector limits the pool of nodes that can host this
	// EgressIP. Only nodes whose labels match this selector are eligible
	// for assignment.
	// This field is optional. When not specified by the user, the CRD
	// default is applied: {matchExpressions: [{key:
	// k8s.ovn.org/egress-assignable, operator: Exists}]}.
	// An empty selector ({}) matches all nodes — use with caution as it
	// expands the eligible pool and health-check set to the entire cluster.
	// This field restricts only where the IPs in this EgressIP object are
	// placed; it does not reserve the matched nodes for exclusive use.
	// Other EgressIP objects can still consume capacity on the same nodes.
	// +kubebuilder:default={matchExpressions: {{key: "k8s.ovn.org/egress-assignable", operator: "Exists"}}}
	// +optional
	EgressNodeSelector metav1.LabelSelector `json:"egressNodeSelector,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +resource:path=egressip
// EgressIPList is the list of EgressIPList.
type EgressIPList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	// List of EgressIP.
	Items []EgressIP `json:"items"`
}
