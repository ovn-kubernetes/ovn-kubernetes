package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ClusterUserDefinedNetwork describe network request for a shared network across namespaces.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=clusteruserdefinednetworks,scope=Cluster
// +kubebuilder:singular=clusteruserdefinednetwork
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type ClusterUserDefinedNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +kubebuilder:validation:Required
	// +required
	Spec ClusterUserDefinedNetworkSpec `json:"spec"`
	// +optional
	Status ClusterUserDefinedNetworkStatus `json:"status,omitempty"`
}

// ClusterUserDefinedNetworkSpec defines the desired state of ClusterUserDefinedNetwork.
type ClusterUserDefinedNetworkSpec struct {
	// NamespaceSelector Label selector for which namespace network should be available for.
	// +kubebuilder:validation:Required
	// +required
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`

	// Network is the user-defined-network spec
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="has(self.topology) && self.topology == 'Layer3' ? has(self.layer3): !has(self.layer3)", message="spec.layer3 is required when topology is Layer3 and forbidden otherwise"
	// +kubebuilder:validation:XValidation:rule="has(self.topology) && self.topology == 'Layer2' ? has(self.layer2): !has(self.layer2)", message="spec.layer2 is required when topology is Layer2 and forbidden otherwise"
	// +kubebuilder:validation:XValidation:rule="has(self.topology) && self.topology == 'Localnet' ? has(self.localnet): !has(self.localnet)", message="spec.localnet is required when topology is Localnet and forbidden otherwise"
	// +kubebuilder:validation:XValidation:rule="!has(self.transport) || self.transport != 'NoOverlay' || (self.topology == 'Layer3' && has(self.layer3) && self.layer3.role == 'Primary')", message="transport 'NoOverlay' is only supported for Layer3 primary networks"
	// +kubebuilder:validation:XValidation:rule="!has(self.transport) || self.transport != 'NoOverlay' || has(self.noOverlay)", message="spec.noOverlay is required when type transport is 'NoOverlay'"
	// +kubebuilder:validation:XValidation:rule="self.transport == 'NoOverlay' || !has(self.noOverlay)", message="spec.noOverlay is forbidden when transport type is not 'NoOverlay'"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf", message="Network spec is immutable"
	// +required
	Network NetworkSpec `json:"network"`
}

// NetworkSpec defines the desired state of UserDefinedNetworkSpec.
// +union
type NetworkSpec struct {
	// Topology describes network configuration.
	//
	// Allowed values are "Layer3", "Layer2" and "Localnet".
	// Layer3 topology creates a layer 2 segment per node, each with a different subnet. Layer 3 routing is used to interconnect node subnets.
	// Layer2 topology creates one logical switch shared by all nodes.
	// Localnet topology is based on layer 2 topology, but also allows connecting to an existent (configured) physical network to provide north-south traffic to the workloads.
	//
	// +kubebuilder:validation:Enum=Layer2;Layer3;Localnet
	// +kubebuilder:validation:Required
	// +required
	// +unionDiscriminator
	Topology NetworkTopology `json:"topology"`

	// Layer3 is the Layer3 topology configuration.
	// +optional
	Layer3 *Layer3Config `json:"layer3,omitempty"`

	// Layer2 is the Layer2 topology configuration.
	// +optional
	Layer2 *Layer2Config `json:"layer2,omitempty"`

	// Localnet is the Localnet topology configuration.
	// +optional
	Localnet *LocalnetConfig `json:"localnet,omitempty"`

	// Transport describes the transport technology for pod-to-pod traffic.
	// Allowed values are "NoOverlay" and "Geneve".
	// - "NoOverlay": The network operates in no-overlay mode.
	// - "Geneve": The network uses Geneve overlay.
	// When omitted, the default behaviour is Geneve.
	// +kubebuilder:validation:Enum=NoOverlay;Geneve
	// +optional
	Transport TransportOption `json:"transport,omitempty"`
	// NoOverlay contains configuration for no-overlay mode.
	// This is only allowed when Transport is "NoOverlay".
	// +optional
	NoOverlay *NoOverlayConfig `json:"noOverlay,omitempty"`
}

// ClusterUserDefinedNetworkStatus contains the observed status of the ClusterUserDefinedNetwork.
type ClusterUserDefinedNetworkStatus struct {
	// Conditions slice of condition objects indicating details about ClusterUserDefineNetwork status.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ClusterUserDefinedNetworkList contains a list of ClusterUserDefinedNetwork.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ClusterUserDefinedNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterUserDefinedNetwork `json:"items"`
}

const NetworkTopologyLocalnet NetworkTopology = "Localnet"

// +kubebuilder:validation:XValidation:rule="!has(self.ipam) || !has(self.ipam.mode) || self.ipam.mode == 'Enabled' ? has(self.subnets) : !has(self.subnets)", message="Subnets is required with ipam.mode is Enabled or unset, and forbidden otherwise"
// +kubebuilder:validation:XValidation:rule="!has(self.excludeSubnets) || has(self.subnets)", message="excludeSubnets must be unset when subnets is unset"
// +kubebuilder:validation:XValidation:rule="!has(self.subnets) || !has(self.mtu) || !self.subnets.exists_one(i, isCIDR(i) && cidr(i).ip().family() == 6) || self.mtu >= 1280", message="MTU should be greater than or equal to 1280 when an IPv6 subnet is used"
// + ---
// + TODO: enable the below validation once the following issue is resolved https://github.com/kubernetes/kubernetes/issues/130441
// + kubebuilder:validation:XValidation:rule="!has(self.excludeSubnets) || self.excludeSubnets.all(e, self.subnets.exists(s, cidr(s).containsCIDR(cidr(e))))",message="excludeSubnets must be subnetworks of the networks specified in the subnets field",fieldPath=".excludeSubnets"
type LocalnetConfig struct {
	// role describes the network role in the pod, required.
	// Controls whether the pod interface will act as primary or secondary.
	// Localnet topology supports `Secondary` only.
	// The network will be assigned to pods that have the `k8s.v1.cni.cncf.io/networks` annotation in place pointing
	// to subject.
	//
	// +kubebuilder:validation:Enum=Secondary
	// +required
	Role NetworkRole `json:"role"`

	// physicalNetworkName points to the OVS bridge-mapping's network-name configured in the nodes, required.
	// Min length is 1, max length is 253, cannot contain `,` or `:` characters.
	// In case OVS bridge-mapping is defined by Kubernetes-nmstate with `NodeNetworkConfigurationPolicy` (NNCP),
	// this field should point to the NNCP `spec.desiredState.ovn.bridge-mappings` item's `localnet` value.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self.matches('^[^,:]+$')", message="physicalNetworkName cannot contain `,` or `:` characters"
	// +required
	PhysicalNetworkName string `json:"physicalNetworkName"`

	// subnets is a list of subnets used for pods in this localnet network across the cluster.
	// The list may be either 1 IPv4 subnet, 1 IPv6 subnet, or 1 of each IP family.
	// When set, OVN-Kubernetes assigns an IP address from the specified CIDRs to the connected pod,
	// eliminating the need for manual IP assignment or reliance on an external IPAM service (e.g., a DHCP server).
	// subnets is optional. When omitted OVN-Kubernetes won't assign IP address automatically.
	// Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.
	// The format should match standard CIDR notation (for example, "10.128.0.0/16").
	// This field must be omitted if `ipam.mode` is `Disabled`.
	// When physicalNetworkName points to the OVS bridge mapping of a network that provides IPAM services
	// (e.g., a DHCP server), ipam.mode should be set to Disabled. This turns off OVN-Kubernetes IPAM and avoids
	// conflicts with the existing IPAM services on this localnet network.
	//
	// +optional
	Subnets DualStackCIDRs `json:"subnets,omitempty"`

	// excludeSubnets is a list of CIDRs to be removed from the specified CIDRs in `subnets`.
	// The CIDRs in this list must be in range of at least one subnet specified in `subnets`.
	// excludeSubnets is optional. When omitted no IP address is excluded and all IP addresses specified in `subnets`
	// are subject to assignment.
	// The format should match standard CIDR notation (for example, "10.128.0.0/16").
	// This field must be omitted if `subnets` is unset or `ipam.mode` is `Disabled`.
	// When `physicalNetworkName` points to OVS bridge mapping of a network with reserved IP addresses
	// (which shouldn't be assigned by OVN-Kubernetes), the specified CIDRs will not be assigned. For example:
	// Given: `subnets: "10.0.0.0/24"`, `excludeSubnets: "10.0.0.200/30", the following addresses will not be assigned
	// to pods: `10.0.0.201`, `10.0.0.202`.
	//
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=25
	ExcludeSubnets []CIDR `json:"excludeSubnets,omitempty"`

	// ipam configurations for the network.
	// ipam is optional. When omitted, `subnets` must be specified.
	// When `ipam.mode` is `Disabled`, `subnets` must be omitted.
	// `ipam.mode` controls how much of the IP configuration will be managed by OVN.
	//    When `Enabled`, OVN-Kubernetes will apply IP configuration to the SDN infra and assign IPs from the selected
	//    subnet to the pods.
	//    When `Disabled`, OVN-Kubernetes only assigns MAC addresses, and provides layer2 communication, and enables users
	//    to configure IP addresses on the pods.
	// `ipam.lifecycle` controls IP addresses management lifecycle.
	//    When set to 'Persistent', the assigned IP addresses will be persisted in `ipamclaims.k8s.cni.cncf.io` object.
	// 	  Useful for VMs, IP address will be persistent after restarts and migrations. Supported when `ipam.mode` is `Enabled`.
	//
	// +optional
	IPAM *IPAMConfig `json:"ipam,omitempty"`

	// mtu is the maximum transmission unit for a network.
	// mtu is optional. When omitted, the configured value in OVN-Kubernetes (defaults to 1500 for localnet topology)
	// is used for the network.
	// Minimum value for IPv4 subnet is 576, and for IPv6 subnet is 1280.
	// Maximum value is 65536.
	// In a scenario `physicalNetworkName` points to OVS bridge mapping of a network configured with certain MTU settings,
	// this field enables configuring the same MTU on pod interface, having the pod MTU aligned with the network MTU.
	// Misaligned MTU across the stack (e.g.: pod has MTU X, node NIC has MTU Y), could result in network disruptions
	// and bad performance.
	//
	// +kubebuilder:validation:Minimum=576
	// +kubebuilder:validation:Maximum=65536
	// +optional
	MTU int32 `json:"mtu,omitempty"`

	// vlan configuration for the network.
	// vlan.mode is the VLAN mode.
	//   When "Access" is set, OVN-Kubernetes configures the network logical switch port in access mode.
	// vlan.access is the access VLAN configuration.
	// vlan.access.id is the VLAN ID (VID) to be set on the network logical switch port.
	// vlan is optional, when omitted the underlying network default VLAN will be used (usually `1`).
	// When set, OVN-Kubernetes will apply VLAN configuration to the SDN infra and to the connected pods.
	//
	// +optional
	VLAN *VLANConfig `json:"vlan,omitempty"`
}

// AccessVLANConfig describes an access VLAN configuration.
type AccessVLANConfig struct {
	// id is the VLAN ID (VID) to be set for the network.
	// id should be higher than 0 and lower than 4095.
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4094
	ID int32 `json:"id"`
}

// +kubebuilder:validation:Enum=Access
type VLANMode string

const VLANModeAccess VLANMode = "Access"

// VLANConfig describes the network VLAN configuration.
// +union
// +kubebuilder:validation:XValidation:rule="has(self.mode) && self.mode == 'Access' ? has(self.access): !has(self.access)", message="vlan access config is required when vlan mode is 'Access', and forbidden otherwise"
type VLANConfig struct {
	// mode describe the network VLAN mode.
	// Allowed value is "Access".
	// Access sets the network logical switch port in access mode, according to the config.
	// +required
	// +unionDiscriminator
	Mode VLANMode `json:"mode"`

	// Access is the access VLAN configuration
	// +optional
	Access *AccessVLANConfig `json:"access"`
}

type TransportOption string
type SNATOption string
type RoutingOption string

const (
	TransportOptionNoOverlay TransportOption = "NoOverlay"
	TransportOptionGeneve    TransportOption = "Geneve"

	SNATEnabled  SNATOption = "Enabled"
	SNATDisabled SNATOption = "Disabled"

	RoutingManaged   RoutingOption = "Managed"
	RoutingUnmanaged RoutingOption = "Unmanaged"
)

// NoOverlayConfig contains configuration options for networks operating in no-overlay mode.
type NoOverlayConfig struct {
	// OutboundSNAT defines the SNAT behavior for outbound traffic from pods.
	// +kubebuilder:validation:Enum=Enabled;Disabled
	// +required
	OutboundSNAT SNATOption `json:"outboundSNAT"`
	// Routing specifies whether the pod network routing is managed by OVN-Kubernetes or users.
	// +kubebuilder:validation:Enum=Managed;Unmanaged
	// +required
	Routing RoutingOption `json:"routing"`
}
