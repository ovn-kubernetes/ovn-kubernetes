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
type VTEPSpec struct {
	// CIDRs is the list of IP ranges from which VTEP IPs are discovered or allocated.
	// Multiple CIDRs may be specified to expand capacity. Only IPv4 CIDRs are currently supported.
	// IPv6 VTEP endpoints are not yet supported by FRR for EVPN transport.
	// The format should match standard CIDR notation (for example, "100.64.0.0/24").
	// +kubebuilder:validation:Required
	// +required
	CIDRs DualStackCIDRs `json:"cidrs"`

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

// DualStackCIDRs is a list of CIDRs that supports dual-stack (IPv4 and IPv6).
// +kubebuilder:validation:MinItems=1
// +kubebuilder:validation:MaxItems=10
// +kubebuilder:validation:XValidation:rule="self.all(c, isCIDR(c) && cidr(c).ip().family() == 4)", message="Only IPv4 CIDRs are currently supported; IPv6 VTEP endpoints are not yet supported by FRR for EVPN transport"
// +kubebuilder:validation:XValidation:rule="size(self) >= size(oldSelf)", message="CIDRs cannot be removed; only appending new CIDRs is allowed"
// +kubebuilder:validation:XValidation:rule="oldSelf.all(i, v, cidr(self[i]).containsIP(cidr(v).ip()) && cidr(self[i]).prefixLength() <= cidr(v).prefixLength())", message="Existing CIDRs must remain at the same position and can only be expanded to a wider mask; shrinking or reordering is not allowed"
// +kubebuilder:validation:XValidation:rule="self.all(i, x, self.all(j, y, j <= i || !(cidr(x).containsIP(cidr(y).ip()) || cidr(y).containsIP(cidr(x).ip()))))", message="CIDRs must not overlap with each other"
type DualStackCIDRs []CIDR

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

// IP represents an IP address.
// +kubebuilder:validation:MaxLength=45
// +kubebuilder:validation:XValidation:rule="isIP(self)", message="IP is invalid"
type IP string

// DualStackIPs is a list of IP addresses supporting dual-stack (at most one IPv4 and one IPv6).
// +kubebuilder:validation:MinItems=1
// +kubebuilder:validation:MaxItems=2
// +kubebuilder:validation:XValidation:rule="size(self) != 2 || ip(self[0]).family() != ip(self[1]).family()", message="When 2 IPs are set, they must be from different IP families"
type DualStackIPs []IP

// VTEPStatus contains the observed state of the VTEP.
type VTEPStatus struct {
	// Conditions slice of condition objects indicating details about VTEP status.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// NodeAllocations is the list of per-node VTEP IP allocations.
	// Each entry maps a node to its discovered or allocated VTEP IP(s).
	// +kubebuilder:validation:MaxItems=5000
	// +listType=map
	// +listMapKey=nodeName
	// +optional
	NodeAllocations []NodeVTEPAllocation `json:"nodeAllocations,omitempty"`
}

// NodeVTEPAllocation represents the VTEP IP allocation for a specific node.
type NodeVTEPAllocation struct {
	// NodeName is the name of the node.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Required
	// +required
	NodeName string `json:"nodeName"`

	// VTEPIPs are the VTEP IP addresses assigned to or discovered on this node.
	// For dual-stack configurations, at most one IPv4 and one IPv6 address may be present.
	// +kubebuilder:validation:Required
	// +required
	VTEPIPs DualStackIPs `json:"vtepIPs"`
}

// VTEPList contains a list of VTEP.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type VTEPList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VTEP `json:"items"`
}
