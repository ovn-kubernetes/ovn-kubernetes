/*
Copyright 2024.

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

type NetworkTopology string

const (
	NetworkTopologyLayer2 NetworkTopology = "Layer2"
	NetworkTopologyLayer3 NetworkTopology = "Layer3"
)

// +kubebuilder:validation:Enum=Geneve;None
type TransportEncapsulation string

const (
	TransportEncapsulationGeneve TransportEncapsulation = "Geneve"
	TransportEncapsulationNone   TransportEncapsulation = "None"
)

// +kubebuilder:validation:XValidation:rule="!has(self.joinSubnets) || has(self.role) && self.role == 'Primary'", message="JoinSubnets is only supported for Primary network"
// +kubebuilder:validation:XValidation:rule="!has(self.subnets) || !has(self.mtu) || !self.subnets.exists_one(i, isCIDR(i.cidr) && cidr(i.cidr).ip().family() == 6) || self.mtu >= 1280", message="MTU should be greater than or equal to 1280 when IPv6 subnet is used"
type Layer3Config struct {
	// Role describes the network role in the pod.
	//
	// Allowed values are "Primary" and "Secondary".
	// Primary network is automatically assigned to every pod created in the same namespace.
	// Secondary network is only assigned to pods that use `k8s.v1.cni.cncf.io/networks` annotation to select given network.
	//
	// +kubebuilder:validation:Enum=Primary;Secondary
	// +kubebuilder:validation:Required
	// +required
	Role NetworkRole `json:"role"`

	// MTU is the maximum transmission unit for a network.
	//
	// MTU is optional, if not provided, the globally configured value in OVN-Kubernetes (defaults to 1400) is used for the network.
	//
	// +kubebuilder:validation:Minimum=576
	// +kubebuilder:validation:Maximum=65536
	// +optional
	MTU int32 `json:"mtu,omitempty"`

	// Subnets are used for the pod network across the cluster.
	//
	// Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.
	// Given subnet is split into smaller subnets for every node.
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +required
	// +kubebuilder:validation:XValidation:rule="size(self) != 2 || !isCIDR(self[0].cidr) || !isCIDR(self[1].cidr) || cidr(self[0].cidr).ip().family() != cidr(self[1].cidr).ip().family()", message="When 2 CIDRs are set, they must be from different IP families"
	Subnets []Layer3Subnet `json:"subnets,omitempty"`

	// JoinSubnets are used inside the OVN network topology.
	//
	// Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.
	// This field is only allowed for "Primary" network.
	// It is not recommended to set this field without explicit need and understanding of the OVN network topology.
	// When omitted, the platform will choose a reasonable default which is subject to change over time.
	//
	// +optional
	JoinSubnets DualStackCIDRs `json:"joinSubnets,omitempty"`

	// Encapsulation specifies the encapsulation type used for the network.
	// Allowed values are "Geneve", "None".
	// When set to "None", the east-west traffic will not use encapsulation. When unset, the default value is "Geneve".
	//
	// +kubebuilder:validation:Enum=Geneve;None
	// +optional
	Encapsulation TransportEncapsulation `json:"encapsulation,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.hostSubnet) || !isCIDR(self.cidr) || self.hostSubnet > cidr(self.cidr).prefixLength()", message="HostSubnet must be smaller than CIDR subnet"
// +kubebuilder:validation:XValidation:rule="!has(self.hostSubnet) || !isCIDR(self.cidr) || (cidr(self.cidr).ip().family() != 4 || self.hostSubnet < 32)", message="HostSubnet must < 32 for ipv4 CIDR"
type Layer3Subnet struct {
	// CIDR specifies L3Subnet, which is split into smaller subnets for every node.
	//
	// +required
	CIDR CIDR `json:"cidr,omitempty"`

	// HostSubnet specifies the subnet size for every node.
	//
	// When not set, it will be assigned automatically.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=127
	// +optional
	HostSubnet int32 `json:"hostSubnet,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.ipam) && has(self.ipam.mode) && self.ipam.mode != 'Enabled' || has(self.subnets)", message="Subnets is required with ipam.mode is Enabled or unset"
// +kubebuilder:validation:XValidation:rule="!has(self.ipam) || !has(self.ipam.mode) || self.ipam.mode != 'Disabled' || !has(self.subnets)", message="Subnets must be unset when ipam.mode is Disabled"
// +kubebuilder:validation:XValidation:rule="!has(self.ipam) || !has(self.ipam.mode) || self.ipam.mode != 'Disabled' || self.role == 'Secondary'", message="Disabled ipam.mode is only supported for Secondary network"
// +kubebuilder:validation:XValidation:rule="!has(self.joinSubnets) || has(self.role) && self.role == 'Primary'", message="JoinSubnets is only supported for Primary network"
// +kubebuilder:validation:XValidation:rule="!has(self.subnets) || !has(self.mtu) || !self.subnets.exists_one(i, isCIDR(i) && cidr(i).ip().family() == 6) || self.mtu >= 1280", message="MTU should be greater than or equal to 1280 when IPv6 subnet is used"
// +kubebuilder:validation:XValidation:rule="!has(self.defaultGatewayIPs) || has(self.role) && self.role == 'Primary'", message="defaultGatewayIPs is only supported for Primary network"
// +kubebuilder:validation:XValidation:rule="!has(self.defaultGatewayIPs) || self.defaultGatewayIPs.all(ip, self.subnets.exists(subnet, cidr(subnet).containsIP(ip)))", message="defaultGatewayIPs must belong to one of the subnets specified in the subnets field"
// +kubebuilder:validation:XValidation:rule="!has(self.defaultGatewayIPs) || size(self.defaultGatewayIPs) == size(self.subnets)", message="defaultGatewayIPs must be specified for all IP families"
// +kubebuilder:validation:XValidation:rule="!has(self.reservedSubnets) || has(self.subnets)", message="reservedSubnets must be unset when subnets is unset"
// +kubebuilder:validation:XValidation:rule="!has(self.reservedSubnets) || has(self.role) && self.role == 'Primary'", message="reservedSubnets is only supported for Primary network"
// +kubebuilder:validation:XValidation:rule="!has(self.infrastructureSubnets) || has(self.subnets)", message="infrastructureSubnets must be unset when subnets is unset"
// +kubebuilder:validation:XValidation:rule="!has(self.infrastructureSubnets) || has(self.role) && self.role == 'Primary'", message="infrastructureSubnets is only supported for Primary network"
// +kubebuilder:validation:XValidation:rule="!has(self.infrastructureSubnets) || !has(self.defaultGatewayIPs) || self.defaultGatewayIPs.all(ip, self.infrastructureSubnets.exists(subnet, cidr(subnet).containsIP(ip)))", message="defaultGatewayIPs have to belong to infrastructureSubnets"
// +kubebuilder:validation:XValidation:rule="!has(self.reservedSubnets) || self.reservedSubnets.all(e, self.subnets.exists(s, cidr(s).containsCIDR(cidr(e))))",message="reservedSubnets must be subnetworks of the networks specified in the subnets field",fieldPath=".reservedSubnets"
// +kubebuilder:validation:XValidation:rule="!has(self.infrastructureSubnets) || self.infrastructureSubnets.all(e, self.subnets.exists(s, cidr(s).containsCIDR(cidr(e))))",message="infrastructureSubnets must be subnetworks of the networks specified in the subnets field",fieldPath=".infrastructureSubnets"
// +kubebuilder:validation:XValidation:rule="!has(self.infrastructureSubnets) || !has(self.reservedSubnets) || self.infrastructureSubnets.all(infra, !self.reservedSubnets.exists(reserved, cidr(infra).containsCIDR(reserved) || cidr(reserved).containsCIDR(infra)))", message="infrastructureSubnets and reservedSubnets must not overlap"
type Layer2Config struct {
	// Role describes the network role in the pod.
	//
	// Allowed value is "Secondary".
	// Secondary network is only assigned to pods that use `k8s.v1.cni.cncf.io/networks` annotation to select given network.
	//
	// +kubebuilder:validation:Enum=Primary;Secondary
	// +kubebuilder:validation:Required
	// +required
	Role NetworkRole `json:"role"`

	// MTU is the maximum transmission unit for a network.
	// MTU is optional, if not provided, the globally configured value in OVN-Kubernetes (defaults to 1400) is used for the network.
	//
	// +kubebuilder:validation:Minimum=576
	// +kubebuilder:validation:Maximum=65536
	// +optional
	MTU int32 `json:"mtu,omitempty"`

	// Subnets are used for the pod network across the cluster.
	// Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.
	//
	// The format should match standard CIDR notation (for example, "10.128.0.0/16").
	// This field must be omitted if `ipam.mode` is `Disabled`.
	//
	// +optional
	Subnets DualStackCIDRs `json:"subnets,omitempty"`

	// reservedSubnets specifies a list of CIDRs reserved for static IP assignment, excluded from automatic allocation.
	// reservedSubnets is optional. When omitted, all IP addresses in `subnets` are available for automatic assignment.
	// IPs from these ranges can still be requested through static IP assignment.
	// Each item should be in range of the specified CIDR(s) in `subnets`.
	// The maximum number of entries allowed is 25.
	// The format should match standard CIDR notation (for example, "10.128.0.0/16").
	// This field must be omitted if `subnets` is unset or `ipam.mode` is `Disabled`.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=25
	ReservedSubnets []CIDR `json:"reservedSubnets,omitempty"`

	// infrastructureSubnets specifies a list of internal CIDR ranges that OVN-Kubernetes will reserve for internal network infrastructure.
	// Any IP addresses within these ranges cannot be assigned to workloads.
	// When omitted, OVN-Kubernetes will automatically allocate IP addresses from `subnets` for its infrastructure needs.
	// When there are not enough available IPs in the provided infrastructureSubnets, OVN-Kubernetes will automatically allocate IP addresses from subnets for its infrastructure needs.
	// When `reservedSubnets` is also specified the CIDRs cannot overlap.
	// When `defaultGatewayIPs` is also specified, the default gateway IPs must belong to one of the infrastructure subnet CIDRs.
	// Each item should be in range of the specified CIDR(s) in `subnets`.
	// The maximum number of entries allowed is 4.
	// The format should match standard CIDR notation (for example, "10.128.0.0/16").
	// This field must be omitted if `subnets` is unset or `ipam.mode` is `Disabled`.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=4
	InfrastructureSubnets []CIDR `json:"infrastructureSubnets,omitempty"`

	// defaultGatewayIPs specifies the default gateway IP used in the internal OVN topology.
	//
	// Dual-stack clusters may set 2 IPs (one for each IP family), otherwise only 1 IP is allowed.
	// This field is only allowed for "Primary" network.
	// It is not recommended to set this field without explicit need and understanding of the OVN network topology.
	// When omitted, an IP from the subnets field is used.
	//
	// +optional
	DefaultGatewayIPs DualStackIPs `json:"defaultGatewayIPs,omitempty"`

	// JoinSubnets are used inside the OVN network topology.
	//
	// Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.
	// This field is only allowed for "Primary" network.
	// It is not recommended to set this field without explicit need and understanding of the OVN network topology.
	// When omitted, the platform will choose a reasonable default which is subject to change over time.
	//
	// +optional
	JoinSubnets DualStackCIDRs `json:"joinSubnets,omitempty"`

	// IPAM section contains IPAM-related configuration for the network.
	// +optional
	IPAM *IPAMConfig `json:"ipam,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.lifecycle) || self.lifecycle != 'Persistent' || !has(self.mode) || self.mode == 'Enabled'", message="lifecycle Persistent is only supported when ipam.mode is Enabled"
// +kubebuilder:validation:MinProperties=1
type IPAMConfig struct {
	// Mode controls how much of the IP configuration will be managed by OVN.
	// `Enabled` means OVN-Kubernetes will apply IP configuration to the SDN infrastructure and it will also assign IPs
	// from the selected subnet to the individual pods.
	// `Disabled` means OVN-Kubernetes will only assign MAC addresses and provide layer 2 communication, letting users
	// configure IP addresses for the pods.
	// `Disabled` is only available for Secondary networks.
	// By disabling IPAM, any Kubernetes features that rely on selecting pods by IP will no longer function
	// (such as network policy, services, etc). Additionally, IP port security will also be disabled for interfaces attached to this network.
	// Defaults to `Enabled`.
	// +optional
	Mode IPAMMode `json:"mode,omitempty"`

	// Lifecycle controls IP addresses management lifecycle.
	//
	// The only allowed value is Persistent. When set, the IP addresses assigned by OVN Kubernetes will be persisted in an
	// `ipamclaims.k8s.cni.cncf.io` object. These IP addresses will be reused by other pods if requested.
	// Only supported when mode is `Enabled`.
	//
	// +optional
	Lifecycle NetworkIPAMLifecycle `json:"lifecycle,omitempty"`
}

// +kubebuilder:validation:Enum=Enabled;Disabled
type IPAMMode string

const (
	IPAMEnabled  IPAMMode = "Enabled"
	IPAMDisabled IPAMMode = "Disabled"
)

type NetworkRole string

const (
	NetworkRolePrimary   NetworkRole = "Primary"
	NetworkRoleSecondary NetworkRole = "Secondary"
)

// +kubebuilder:validation:Enum=Persistent
type NetworkIPAMLifecycle string

const IPAMLifecyclePersistent NetworkIPAMLifecycle = "Persistent"

// +kubebuilder:validation:XValidation:rule="isCIDR(self)", message="CIDR is invalid"
// +kubebuilder:validation:MaxLength=43
type CIDR string

// +kubebuilder:validation:MinItems=1
// +kubebuilder:validation:MaxItems=2
// +kubebuilder:validation:XValidation:rule="size(self) != 2 || !isCIDR(self[0]) || !isCIDR(self[1]) || cidr(self[0]).ip().family() != cidr(self[1]).ip().family()", message="When 2 CIDRs are set, they must be from different IP families"
type DualStackCIDRs []CIDR

// +kubebuilder:validation:XValidation:rule="isIP(self)", message="IP is invalid"
type IP string

// +kubebuilder:validation:MinItems=1
// +kubebuilder:validation:MaxItems=2
// +kubebuilder:validation:XValidation:rule="size(self) != 2 || !isIP(self[0]) || !isIP(self[1]) || ip(self[0]).family() != ip(self[1]).family()", message="When 2 IPs are set, they must be from different IP families"
type DualStackIPs []IP
