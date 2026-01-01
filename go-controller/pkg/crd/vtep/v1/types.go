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
	// CIDRs is the list of IP ranges from which VTEP IPs are allocated.
	// Dual-stack clusters may set 2 CIDRs (one for each IP family), otherwise only 1 CIDR is allowed.
	// The format should match standard CIDR notation (for example, "100.64.0.0/24" or "fd00::/64").
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
// +kubebuilder:validation:MaxItems=2
// +kubebuilder:validation:XValidation:rule="size(self) != 2 || !isCIDR(self[0]) || !isCIDR(self[1]) || cidr(self[0]).ip().family() != cidr(self[1]).ip().family()", message="When 2 CIDRs are set, they must be from different IP families"
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

// VTEPStatus contains the observed state of the VTEP.
type VTEPStatus struct {
	// Conditions slice of condition objects indicating details about VTEP status.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VTEPList contains a list of VTEP.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type VTEPList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VTEP `json:"items"`
}
