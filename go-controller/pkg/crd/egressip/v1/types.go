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
	EgressIPs []string `json:"egressIPs"`
	// NamespaceSelector applies the egress IP only to the namespace(s) whose label
	// matches this definition. This field is mandatory.
	// +kubebuilder:validation:Required
	// -------------------------------
	// Label Key Validations (matchLabels)
	// -------------------------------
	// Regex explanation:
	// '^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]/)?([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$'
	//
	// This allows:
	// - An optional DNS prefix ending with '/'
	// - A label name:
	//   • Starts and ends with alphanumeric
	//   • Contains '-', '_', '.' in between
	// +kubebuilder:validation:XValidation:rule="self.matchLabels.all(key, key.matches(r'^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]/)?([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$'))",message="label keys must be valid qualified names (DNS subdomain + '/' + name)"
	// Total key length must be ≤ 253 characters
	// +kubebuilder:validation:XValidation:rule="self.matchLabels.all(key, key.size() <= 253)",message="label keys must be no more than 253 characters"
	// If prefix exists (qualified name), ensure prefix is ≤ 253
	// +kubebuilder:validation:XValidation:rule="self.matchLabels.all(key, !key.contains('/') || key.split('/')[0].size() <= 253)",message="label key prefix must be ≤ 253 characters"
	// If prefix exists, ensure name part is ≤ 63
	// +kubebuilder:validation:XValidation:rule="self.matchLabels.all(key, !key.contains('/') || key.split('/')[1].size() <= 63)",message="label key name must be ≤ 63 characters"
	// If no prefix, the full key (name-only) must be ≤ 63
	// +kubebuilder:validation:XValidation:rule="self.matchLabels.all(key, key.contains('/') || key.size() <= 63)",message="label key name-only must be ≤ 63 characters"
	// -------------------------------
	// Label Value Validations (matchLabels values)
	// -------------------------------
	// Regex explanation:
	// '^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$'
	//
	// This allows:
	// - Empty string
	// - Or a string:
	//   • Starts and ends with alphanumeric
	//   • Contains '-', '_', '.' in between
	// +kubebuilder:validation:XValidation:rule="self.matchLabels.all(key, self.matchLabels[key].matches(r'^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$'))",message="label values must be empty or valid strings that start and end with alphanumeric characters"
	// Label values must be ≤ 63 characters
	// +kubebuilder:validation:XValidation:rule="self.matchLabels.all(key, self.matchLabels[key].size() <= 63)",message="label values must be no more than 63 characters"
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
