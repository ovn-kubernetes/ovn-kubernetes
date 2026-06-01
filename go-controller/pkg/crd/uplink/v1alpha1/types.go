// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type UplinkType string

const (
	UplinkTypeOVSBridge UplinkType = "OVSBridge"
)

const (
	UplinkConditionReady = "Ready"
)

const (
	UplinkStateConditionReady = "Ready"
)

const (
	UplinkStateReasonReady                  = "Ready"
	UplinkStateReasonHostInterfaceNotFound  = "HostInterfaceNotFound"
	UplinkStateReasonBridgeNotFound         = "BridgeNotFound"
	UplinkStateReasonBridgeUplinkNotFound   = "BridgeUplinkNotFound"
	UplinkStateReasonBridgeInvalid          = "BridgeInvalid"
	UplinkStateReasonInvalidHostInterface   = "InvalidHostInterface"
	UplinkStateReasonGatewayInfoUnavailable = "GatewayInfoUnavailable"
	UplinkStateReasonWaitingForDPU          = "WaitingForDPU"
	UplinkStateReasonWaitingForDPUHost      = "WaitingForDPUHost"
	UplinkStateReasonNodeNotSelected        = "NodeNotSelected"
	UplinkStateReasonNodeSelectorOverlap    = "NodeSelectorOverlap"
	UplinkStateReasonVRFAttachmentFailed    = "UplinkVRFAttachmentFailed"
	UplinkStateReasonBridgeMappingFailed    = "UplinkBridgeMappingFailed"
)

// InterfaceName is a Linux interface name.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=15
// +kubebuilder:validation:Pattern=`^[^/\s]+$`
type InterfaceName string

// IPAddress is an IP address.
// +kubebuilder:validation:MaxLength=64
// +kubebuilder:validation:XValidation:rule="isIP(self)", message="must be a valid IP address"
type IPAddress string

// IPAddressCIDR is an IP address with a network prefix.
// +kubebuilder:validation:MaxLength=64
// +kubebuilder:validation:XValidation:rule="isCIDR(self)", message="must be a valid CIDR"
type IPAddressCIDR string

// MACAddress is an IEEE 802 MAC address.
// +kubebuilder:validation:Pattern=`^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$`
type MACAddress string

// Uplink represents a physical network path out of a node that OVN-Kubernetes
// can connect its logical topology to. Supported node config type: OVSBridge.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=uplinks,scope=Cluster,shortName=uplink
// +kubebuilder:singular=uplink
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type Uplink struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	// +required
	Spec UplinkSpec `json:"spec"`
	// +optional
	Status UplinkStatus `json:"status,omitempty"`
}

type UplinkSpec struct {
	// NodeConfigs contains mappings of nodes to uplink configuration. A node may
	// be selected by at most one nodeConfig entry. Multiple nodeConfigs that
	// select the same node are treated as a configuration error.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +required
	NodeConfigs []UplinkNodeConfig `json:"nodeConfigs"`
}

type UplinkNodeConfig struct {
	// Type is the uplink node config type. Supported value: OVSBridge.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=OVSBridge
	// +required
	Type UplinkType `json:"type"`

	// NodeSelector selects nodes where this uplink config applies. An empty
	// selector matches all nodes.
	// +kubebuilder:validation:Required
	// +required
	NodeSelector metav1.LabelSelector `json:"nodeSelector"`

	// HostInterfaceName is the host-visible Linux interface that carries the
	// gateway L3 identity for this uplink. In non-accelerated deployments,
	// this is typically the OVS bridge Linux interface, whose name usually
	// matches the OVS bridge name. In SmartNIC accelerated deployments, this is
	// the VF/SF netdevice and OVN-Kubernetes resolves the OVS bridge through
	// its representor. In DPU deployments, this is the DPU-host PF interface
	// whose DPU-side PF representor is attached to the OVS bridge.
	// +kubebuilder:validation:Required
	// +required
	HostInterfaceName InterfaceName `json:"hostInterfaceName"`
}

type UplinkStatus struct {
	// Conditions reports aggregate Uplink state.
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchMergeKey:"type" patchStrategy:"merge"`
}

// UplinkList contains a list of Uplink resources.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type UplinkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Uplink `json:"items"`
}

// UplinkState contains node-local discovery and gateway state for an Uplink.
//
// +genclient
// +genclient:nonNamespaced
// +genclient:noStatus
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=uplinkstates,scope=Cluster
// +kubebuilder:singular=uplinkstate
// +kubebuilder:object:root=true
// +kubebuilder:selectablefield:JSONPath=".status.uplinkName"
// +kubebuilder:selectablefield:JSONPath=".status.nodeName"
// +kubebuilder:printcolumn:name="Uplink",type=string,JSONPath=".status.uplinkName"
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".status.nodeName"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type UplinkState struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Status UplinkStateStatus `json:"status,omitempty"`
}

type UplinkStateStatus struct {
	// UplinkName is the Uplink this state belongs to.
	// +optional
	UplinkName string `json:"uplinkName,omitempty"`

	// NodeName is the node this state belongs to.
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// Type is defined by the matched nodeConfig of the Uplink.
	// +kubebuilder:validation:Enum=OVSBridge
	// +optional
	Type UplinkType `json:"type,omitempty"`

	// HostInterfaceName is the host-visible Linux interface selected by
	// Uplink spec. It carries the host-side gateway L3 identity in all modes.
	// +optional
	HostInterfaceName InterfaceName `json:"hostInterfaceName,omitempty"`

	// OVSBridge contains resolved OVS bridge data.
	// +optional
	OVSBridge *OVSBridgeStatus `json:"ovsBridge,omitempty"`

	// MACAddress is the MAC address used for the OVN gateway interface.
	// +optional
	MACAddress MACAddress `json:"macAddress,omitempty"`

	// IPAddresses are host-side shared gateway IP addresses.
	// +kubebuilder:validation:MaxItems=2
	// +optional
	IPAddresses []IPAddressCIDR `json:"ipAddresses,omitempty"`

	// DefaultGateways are default route next-hop IPs discovered for the
	// selected host interface.
	// +kubebuilder:validation:MaxItems=2
	// +optional
	DefaultGateways []IPAddress `json:"defaultGateways,omitempty"`

	// Conditions reports node-local discovery state for this resolved uplink.
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchMergeKey:"type" patchStrategy:"merge"`
}

type OVSBridgeStatus struct {
	// Name is the resolved OVS bridge used for OVN bridge mappings and flows.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:Pattern=`^[^/\s]+$`
	// +optional
	Name string `json:"name,omitempty"`
}

// UplinkStateList contains a list of UplinkState resources.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type UplinkStateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UplinkState `json:"items"`
}
