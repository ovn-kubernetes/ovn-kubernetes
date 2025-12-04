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

// EVPNConfig contains configuration options for networks operating in EVPN mode.
// +kubebuilder:validation:XValidation:rule="has(self.macVRF) || has(self.ipVRF)", message="at least one of macVRF or ipVRF must be specified"
type EVPNConfig struct {
	// VTEP is the name of the VTEP CR that defines VTEP IPs for EVPN.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +required
	VTEP string `json:"vtep"`

	// MACVRF contains the MAC-VRF configuration for Layer 2 EVPN.
	// This field is required for Layer2 topology and forbidden for Layer3 topology.
	// +optional
	MACVRF *VRFConfig `json:"macVRF,omitempty"`

	// IPVRF contains the IP-VRF configuration for Layer 3 EVPN.
	// This field is required for Layer3 topology and optional for Layer2 topology.
	// +optional
	IPVRF *VRFConfig `json:"ipVRF,omitempty"`
}

// RouteTargetString represents a BGP route target in the format "<AS>:<VNI>".
// +kubebuilder:validation:XValidation:rule="self.matches('^[0-9]+:[0-9]+$')", message="routeTarget must be in the format '<AS>:<VNI>'"
// +kubebuilder:validation:XValidation:rule="int(self.split(':')[0]) >= 1 && int(self.split(':')[0]) <= 4294967295", message="AS number in routeTarget must be between 1 and 4294967295"
// +kubebuilder:validation:XValidation:rule="int(self.split(':')[1]) >= 1 && int(self.split(':')[1]) <= 16777215", message="VNI in routeTarget must be between 1 and 16777215"
type RouteTargetString string

// VRFConfig contains configuration for a VRF in EVPN.
type VRFConfig struct {
	// VNI is the Virtual Network Identifier for this VRF.
	// Must be unique across all EVPN configurations in the cluster.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16777215
	// +required
	VNI int32 `json:"vni"`

	// RouteTarget is the import/export route target for this VRF.
	// If not specified, it will be auto-generated as "<AS Number>:<VNI>".
	// Format should be "<AS>:<VNI>" where AS is the autonomous system number and VNI is the Virtual Network Identifier number.
	// AS must be between 1 and 4294967295 (4-byte ASN). VNI must be between 1 and 16777215 (24-bit).
	// +optional
	RouteTarget RouteTargetString `json:"routeTarget,omitempty"`
}
