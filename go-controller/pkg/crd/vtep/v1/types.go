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
)

// VTEP defines VTEP (VXLAN Tunnel Endpoint) IP configuration for EVPN.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=vteps,scope=Cluster
// +kubebuilder:singular=vtep
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=="Accepted")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Accepted")].reason`
type VTEP struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the desired VTEP configuration.
	// +kubebuilder:validation:Required
	// +required
	Spec VTEPSpec `json:"spec"`

	// Status contains the observed state of the VTEP.
	// +optional
	Status VTEPStatus `json:"status,omitempty"`
}

// VTEPSpec defines the desired state of VTEP.
// +kubebuilder:validation:XValidation:rule="self.mode != 'Managed' || size(self.cidrs) >= size(oldSelf.cidrs)", message="CIDRs cannot be removed in managed mode; only appending new CIDRs or expanding the existing CIDRs is allowed"
// +kubebuilder:validation:XValidation:rule="self.mode != 'Managed' || self.cidrs.all(i, v, i >= size(oldSelf.cidrs) || (cidr(v).containsIP(cidr(oldSelf.cidrs[i]).ip()) && cidr(v).prefixLength() <= cidr(oldSelf.cidrs[i]).prefixLength()))", message="In managed mode, existing CIDRs must remain at the same position and can only be expanded to a wider mask; shrinking the mask or reordering is not allowed"
type VTEPSpec struct {
	// CIDRs is the list of IP ranges from which VTEP IPs are discovered (unmanaged mode) or allocated (managed mode).
	// Multiple CIDRs may be specified to expand capacity over time without recreating the VTEP.
	// Each entry must be a valid network address in CIDR notation (for example, "100.64.0.0/24" or "fd00:100::/64").
	// Each node receives at most one IP per address family from the CIDRs listed here.
	// In managed mode, CIDRs are consumed sequentially: IPs are allocated from the first CIDR until it is
	// exhausted, then from the next, and so on.
	// In managed mode, CIDRs are append-only: existing entries cannot be removed, reordered, or shrunk to a
	// smaller mask; they can only be expanded to a wider mask, and new entries may be appended.
	// In unmanaged mode, if multiple IPs on a node match the configured CIDRs, or if the match is otherwise
	// ambiguous, the VTEP will be placed into a failed status.
	// In unmanaged mode, CIDRs may be freely added, removed, reordered, or resized.
	// Caution: removing or modifying CIDRs in unmanaged mode that are actively in use may cause traffic disruption;
	// no downtime guarantees are provided for such operations.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:XValidation:rule="self.all(i, a, self.all(j, b, i == j || !(cidr(a).containsIP(cidr(b).ip()) || cidr(b).containsIP(cidr(a).ip()))))", message="CIDRs must not overlap with each other"
	// +required
	CIDRs []CIDR `json:"cidrs"`

	// Mode specifies how VTEP IPs are managed.
	// "Managed" means OVN-Kubernetes allocates and assigns VTEP IPs per node automatically.
	// "Unmanaged" means an external provider handles IP assignment; OVN-Kubernetes discovers existing IPs on nodes.
	// Defaults to "Managed".
	// +kubebuilder:validation:Enum=Managed;Unmanaged
	// +kubebuilder:default=Managed
	// +optional
	Mode VTEPMode `json:"mode,omitempty"`
}

// CIDR represents a CIDR notation IP range.
// +kubebuilder:validation:XValidation:rule="isCIDR(self) && cidr(self) == cidr(self).masked()", message="CIDR must be a valid network address"
// +kubebuilder:validation:MaxLength=43
type CIDR string

// VTEPMode defines the mode of VTEP IP allocation.
// +kubebuilder:validation:Enum=Managed;Unmanaged
type VTEPMode string

const (
	// VTEPModeManaged means OVN-Kubernetes allocates and assigns VTEP IPs per node automatically.
	VTEPModeManaged VTEPMode = "Managed"
	// VTEPModeUnmanaged means an external provider handles IP assignment;
	// OVN-Kubernetes discovers existing IPs on nodes.
	VTEPModeUnmanaged VTEPMode = "Unmanaged"
)

// VTEPStatus contains the observed state of the VTEP.
type VTEPStatus struct {
	// Conditions slice of condition objects indicating details about VTEP status.
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchMergeKey:"type" patchStrategy:"merge"`
}

// VTEPList contains a list of VTEP.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type VTEPList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VTEP `json:"items"`
}
