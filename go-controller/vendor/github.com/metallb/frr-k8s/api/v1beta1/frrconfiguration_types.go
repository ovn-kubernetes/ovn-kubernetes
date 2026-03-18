/*
Copyright 2023.

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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FRRConfigurationSpec defines the desired state of FRRConfiguration.
type FRRConfigurationSpec struct {
	// BGP is the configuration related to the BGP protocol.
	// +optional
	BGP BGPConfig `json:"bgp,omitempty"`

	// Raw is a snippet of raw frr configuration that gets appended to the
	// one rendered translating the type safe API.
	// +optional
	Raw RawConfig `json:"raw,omitempty"`
	// NodeSelector limits the nodes that will attempt to apply this config.
	// When specified, the configuration will be considered only on nodes
	// whose labels match the specified selectors.
	// When it is not specified all nodes will attempt to apply this config.
	// +optional
	NodeSelector metav1.LabelSelector `json:"nodeSelector,omitempty"`
}

// RawConfig is a snippet of raw frr configuration that gets appended to the
// rendered configuration.
//
// WARNING: The RawConfig feature is UNSUPPORTED and intended ONLY FOR EXPERIMENTATION.
// It should not be used in production environments. This feature is provided as-is without any
// guarantees of stability, compatibility, or support. Use at your own risk.
type RawConfig struct {
	// Priority is the order with this configuration is appended to the
	// bottom of the rendered configuration. A higher value means the
	// raw config is appended later in the configuration file.
	Priority int `json:"priority,omitempty"`

	// Config is a raw FRR configuration to be appended to the configuration
	// rendered via the k8s api.
	Config string `json:"rawConfig,omitempty"`
}

// BGPConfig is the configuration related to the BGP protocol.
type BGPConfig struct {
	// Routers is the list of routers we want FRR to configure (one per VRF).
	// +optional
	// +kubebuilder:validation:MaxItems=50
	Routers []Router `json:"routers,omitempty"`
	// BFDProfiles is the list of bfd profiles to be used when configuring the neighbors.
	// +optional
	BFDProfiles []BFDProfile `json:"bfdProfiles,omitempty"`
}

// Router represent a neighbor router we want FRR to connect to.
type Router struct {
	// ASN is the AS number to use for the local end of the session.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	ASN uint32 `json:"asn"`
	// ID is the BGP router ID
	// +optional
	ID string `json:"id,omitempty"`
	// VRF is the host vrf used to establish sessions from this router.
	// +optional
	VRF string `json:"vrf,omitempty"`
	// Neighbors is the list of neighbors we want to establish BGP sessions with.
	// +optional
	Neighbors []Neighbor `json:"neighbors,omitempty"`
	// Prefixes is the list of prefixes we want to advertise from this router instance.
	// +optional
	Prefixes []string `json:"prefixes,omitempty"`

	// Imports is the list of imported VRFs we want for this router / vrf.
	// +optional
	Imports []Import `json:"imports,omitempty"`

	// EVPN specific configuration for the router.
	// +optional
	EVPN *EVPNConfig `json:"evpn,omitempty"`
}

// Import represents the possible imported VRFs to a given router.
type Import struct {
	// Vrf is the vrf we want to import from
	// +optional
	VRF string `json:"vrf,omitempty"`
}

// Neighbor represents a BGP Neighbor we want FRR to connect to.
type Neighbor struct {
	// ASN is the AS number to use for the local end of the session.
	// ASN and DynamicASN are mutually exclusive and one of them must be specified.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	// +optional
	ASN uint32 `json:"asn,omitempty"`

	// DynamicASN detects the AS number to use for the local end of the session
	// without explicitly setting it via the ASN field. Limited to:
	// internal - if the neighbor's ASN is different than the router's the connection is denied.
	// external - if the neighbor's ASN is the same as the router's the connection is denied.
	// ASN and DynamicASN are mutually exclusive and one of them must be specified.
	// +kubebuilder:validation:Enum=internal;external
	// +optional
	DynamicASN DynamicASNMode `json:"dynamicASN,omitempty"`

	// SourceAddress is the IPv4 or IPv6 source address to use for the BGP
	// session to this neighbour, may be specified as either an IP address
	// directly or as an interface name
	// +optional
	SourceAddress string `json:"sourceaddress,omitempty"`

	// Address is the IP address to establish the session with.
	// +optional
	Address string `json:"address,omitempty"`

	// Interface is the node interface over which the unnumbered BGP peering will
	// be established. No API validation takes place as that string value
	// represents an interface name on the host and if user provides an invalid
	// value, only the actual BGP session will not be established.
	// Address and Interface are mutually exclusive and one of them must be specified.
	// Note: when enabling unnumbered, the neighbor will be enabled for both
	// IPv4 and IPv6 address families.
	// +optional
	Interface string `json:"interface,omitempty"`

	// Port is the port to dial when establishing the session.
	// Defaults to 179.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=16384
	Port *uint16 `json:"port,omitempty"`

	// Password to be used for establishing the BGP session.
	// Password and PasswordSecret are mutually exclusive.
	// +optional
	Password string `json:"password,omitempty"`

	// PasswordSecret is name of the authentication secret for the neighbor.
	// the secret must be of type "kubernetes.io/basic-auth", and created in the
	// same namespace as the frr-k8s daemon. The password is stored in the
	// secret as the key "password".
	// Password and PasswordSecret are mutually exclusive.
	// +optional
	PasswordSecret SecretReference `json:"passwordSecret,omitempty"`

	// HoldTime is the requested BGP hold time, per RFC4271.
	// Defaults to 180s.
	// +optional
	HoldTime *metav1.Duration `json:"holdTime,omitempty"`

	// KeepaliveTime is the requested BGP keepalive time, per RFC4271.
	// Defaults to 60s.
	// +optional
	KeepaliveTime *metav1.Duration `json:"keepaliveTime,omitempty"`

	// Requested BGP connect time, controls how long BGP waits between connection attempts to a neighbor.
	// +kubebuilder:validation:XValidation:message="connect time should be between 1 seconds to 65535",rule="duration(self).getSeconds() >= 1 && duration(self).getSeconds() <= 65535"
	// +kubebuilder:validation:XValidation:message="connect time should contain a whole number of seconds",rule="duration(self).getMilliseconds() % 1000 == 0"
	// +optional
	ConnectTime *metav1.Duration `json:"connectTime,omitempty"`

	// EBGPMultiHop indicates if the BGPPeer is multi-hops away.
	// +optional
	EBGPMultiHop bool `json:"ebgpMultiHop,omitempty"`

	// BFDProfile is the name of the BFD Profile to be used for the BFD session associated
	// to the BGP session. If not set, the BFD session won't be set up.
	// +optional
	BFDProfile string `json:"bfdProfile,omitempty"`

	// EnableGracefulRestart allows BGP peer to continue to forward data packets along
	// known routes while the routing protocol information is being restored. If
	// the session is already established, the configuration will have effect
	// after reconnecting to the peer
	// +optional
	EnableGracefulRestart bool `json:"enableGracefulRestart,omitempty"`

	// ToAdvertise represents the list of prefixes to advertise to the given neighbor
	// and the associated properties. Only applies to IPv4 and IPv6 unicast address families.
	// +optional
	ToAdvertise Advertise `json:"toAdvertise,omitempty"`

	// ToReceive represents the list of prefixes to receive from the given neighbor.
	// Only applies to IPv4 and IPv6 unicast address families.
	// +optional
	ToReceive Receive `json:"toReceive,omitempty"`

	// DisableMP is no longer used and has no effect.
	// Use DualStackAddressFamily instead to enable the neighbor for both IPv4 and IPv6 address families.
	//
	// Deprecated: DisableMP is deprecated in favor of DualStackAddressFamily.
	// +optional
	// +kubebuilder:default:=false
	DisableMP bool `json:"disableMP,omitempty"`

	// To set if we want to enable the neighbor not only for the ipfamily related to its session,
	// but also the other one. This allows to advertise/receive IPv4 prefixes over IPv6 sessions and vice versa.
	// +optional
	// +kubebuilder:default:=false
	DualStackAddressFamily bool `json:"dualStackAddressFamily,omitempty"`

	// AddressFamilies specifies which address families to activate this neighbor for.
	// Supported values: "unicast" (IPv4/IPv6 unicast based on neighbor IP), "evpn" (L2VPN EVPN).
	// +optional
	// +kubebuilder:default:={"unicast"}
	// +kubebuilder:validation:MaxItems=2
	AddressFamilies []AddressFamily `json:"addressFamilies,omitempty"`
}

// Advertise represents a list of prefixes to advertise to the given neighbor.

type Advertise struct {

	// Allowed is is the list of prefixes allowed to be propagated to
	// this neighbor. They must match the prefixes defined in the router.
	Allowed AllowedOutPrefixes `json:"allowed,omitempty"`

	// PrefixesWithLocalPref is a list of prefixes that are associated to a local
	// preference when being advertised. The prefixes associated to a given local pref
	// must be in the prefixes allowed to be advertised.
	// +optional
	PrefixesWithLocalPref []LocalPrefPrefixes `json:"withLocalPref,omitempty"`

	// PrefixesWithCommunity is a list of prefixes that are associated to a
	// bgp community when being advertised. The prefixes associated to a given local pref
	// must be in the prefixes allowed to be advertised.
	// +optional
	PrefixesWithCommunity []CommunityPrefixes `json:"withCommunity,omitempty"`
}

// Receive represents a list of prefixes to receive from the given neighbor.
type Receive struct {
	// Allowed is the list of prefixes allowed to be received from
	// this neighbor.
	// +optional
	Allowed AllowedInPrefixes `json:"allowed,omitempty"`
}

// PrefixSelector is a filter of prefixes to receive.
type PrefixSelector struct {
	// +kubebuilder:validation:Format="cidr"
	Prefix string `json:"prefix,omitempty"`
	// The prefix length modifier. This selector accepts any matching prefix with length
	// less or equal the given value.
	// +kubebuilder:validation:Maximum:=128
	// +kubebuilder:validation:Minimum:=1
	LE uint32 `json:"le,omitempty"`
	// The prefix length modifier. This selector accepts any matching prefix with length
	// greater or equal the given value.
	// +kubebuilder:validation:Maximum:=128
	// +kubebuilder:validation:Minimum:=1
	GE uint32 `json:"ge,omitempty"`
}

type AllowedInPrefixes struct {
	Prefixes []PrefixSelector `json:"prefixes,omitempty"`
	// Mode is the mode to use when handling the prefixes.
	// When set to "filtered", only the prefixes in the given list will be allowed.
	// When set to "all", all the prefixes configured on the router will be allowed.
	// +kubebuilder:default:=filtered
	Mode AllowMode `json:"mode,omitempty"`
}

type AllowedOutPrefixes struct {
	Prefixes []string `json:"prefixes,omitempty"`
	// Mode is the mode to use when handling the prefixes.
	// When set to "filtered", only the prefixes in the given list will be allowed.
	// When set to "all", all the prefixes configured on the router will be allowed.
	// +kubebuilder:default:=filtered
	Mode AllowMode `json:"mode,omitempty"`
}

// LocalPrefPrefixes is a list of prefixes associated to a local preference.
type LocalPrefPrefixes struct {
	// Prefixes is the list of prefixes associated to the local preference.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:Format="cidr"
	Prefixes []string `json:"prefixes,omitempty"`
	// LocalPref is the local preference associated to the prefixes.
	LocalPref uint32 `json:"localPref,omitempty"`
}

// CommunityPrefixes is a list of prefixes associated to a community.
type CommunityPrefixes struct {
	// Prefixes is the list of prefixes associated to the community.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:Format="cidr"
	Prefixes []string `json:"prefixes,omitempty"`
	// Community is the community associated to the prefixes.
	Community string `json:"community,omitempty"`
}

// BFDProfile is the configuration related to the BFD protocol associated
// to a BGP session.
type BFDProfile struct {
	// The name of the BFD Profile to be referenced in other parts
	// of the configuration.
	Name string `json:"name"`

	// The minimum interval that this system is capable of
	// receiving control packets in milliseconds.
	// Defaults to 300ms.
	// +kubebuilder:validation:Maximum:=60000
	// +kubebuilder:validation:Minimum:=10
	// +optional
	ReceiveInterval *uint32 `json:"receiveInterval,omitempty"`

	// The minimum transmission interval (less jitter)
	// that this system wants to use to send BFD control packets in
	// milliseconds. Defaults to 300ms
	// +kubebuilder:validation:Maximum:=60000
	// +kubebuilder:validation:Minimum:=10
	// +optional
	TransmitInterval *uint32 `json:"transmitInterval,omitempty"`

	// Configures the detection multiplier to determine
	// packet loss. The remote transmission interval will be multiplied
	// by this value to determine the connection loss detection timer.
	// +kubebuilder:validation:Maximum:=255
	// +kubebuilder:validation:Minimum:=2
	// +optional
	DetectMultiplier *uint32 `json:"detectMultiplier,omitempty"`

	// Configures the minimal echo receive transmission
	// interval that this system is capable of handling in milliseconds.
	// Defaults to 50ms
	// +kubebuilder:validation:Maximum:=60000
	// +kubebuilder:validation:Minimum:=10
	// +optional
	EchoInterval *uint32 `json:"echoInterval,omitempty"`

	// Enables or disables the echo transmission mode.
	// This mode is disabled by default, and not supported on multi
	// hops setups.
	// +optional
	EchoMode *bool `json:"echoMode,omitempty"`

	// Mark session as passive: a passive session will not
	// attempt to start the connection and will wait for control packets
	// from peer before it begins replying.
	// +optional
	PassiveMode *bool `json:"passiveMode,omitempty"`

	// For multi hop sessions only: configure the minimum
	// expected TTL for an incoming BFD control packet.
	// +kubebuilder:validation:Maximum:=254
	// +kubebuilder:validation:Minimum:=1
	// +optional
	MinimumTTL *uint32 `json:"minimumTtl,omitempty"`
}

// FRRConfigurationStatus defines the observed state of FRRConfiguration.
type FRRConfigurationStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//nolint
//+genclient

// FRRConfiguration is a piece of FRR configuration.
type FRRConfiguration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FRRConfigurationSpec   `json:"spec,omitempty"`
	Status FRRConfigurationStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// FRRConfigurationList contains a list of FRRConfiguration.
type FRRConfigurationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FRRConfiguration `json:"items"`
}

//nolint
//+structType=atomic

// SecretReference represents a Secret Reference. It has enough information to retrieve secret
// in any namespace.
type SecretReference struct {
	// name is unique within a namespace to reference a secret resource.
	// +optional
	Name string `json:"name,omitempty" protobuf:"bytes,1,opt,name=name"`
	// namespace defines the space within which the secret name must be unique.
	// +optional
	Namespace string `json:"namespace,omitempty" protobuf:"bytes,2,opt,name=namespace"`
}

func init() {
	SchemeBuilder.Register(&FRRConfiguration{}, &FRRConfigurationList{})
}

// +kubebuilder:validation:Enum=all;filtered
type AllowMode string

const (
	AllowAll        AllowMode = "all"
	AllowRestricted AllowMode = "filtered"
)

type DynamicASNMode string

const (
	InternalASNMode DynamicASNMode = "internal"
	ExternalASNMode DynamicASNMode = "external"
)

// AddressFamily specifies an address family for BGP neighbor activation.
// +kubebuilder:validation:Enum=unicast;evpn
type AddressFamily string

const (
	AddressFamilyUnicast AddressFamily = "unicast"
	AddressFamilyEVPN    AddressFamily = "evpn"
)

// AdvertisePrefixType specifies a prefix type to advertise as EVPN type-5 routes.
// +kubebuilder:validation:Enum=unicast
type AdvertisePrefixType string

const (
	AdvertisePrefixUnicast AdvertisePrefixType = "unicast"
)

// EVPNConfig contains configuration related to EVPN.
type EVPNConfig struct {
	// AdvertiseVNIs controls how VNIs are advertised to EVPN neighbors.
	// - "Disabled": No VNI advertisements
	// - "All": Avertise all VNIs
	// Note: Can only be provided for router instances with EVPN neighbors.
	// +optional
	// +kubebuilder:validation:Enum=Disabled;All
	AdvertiseVNIs *VNIAdvertisement `json:"advertiseVNIs,omitempty"`

	// AdvertiseSVI enables advertising the SVI IP/MAC as a type-2 route.
	// +optional
	AdvertiseSVI bool `json:"advertiseSVI,omitempty"`

	// L2VNIs contains configuration for Layer 2 VNIs.
	// Note: Can only be provided for router instances with EVPN neighbors.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	L2VNIs []L2VNI `json:"l2vnis,omitempty"`

	// L3VNI contains configuration for the Layer 3 VNI.
	// Note: Can only be provided for router instances with no neighbors.
	// This is a temporary limitation until proper EVPN prefix filtering is implemented.
	// +optional
	L3VNI *L3VNI `json:"l3vni,omitempty"`
}

// VNIAdvertisement defines how VNIs are advertised in EVPN.
// +kubebuilder:validation:Enum=Disabled;All
type VNIAdvertisement string

const (
	// VNIAdvertisementDisabled disables VNI advertisement.
	VNIAdvertisementDisabled VNIAdvertisement = "Disabled"

	// VNIAdvertisementAll enables advertisement of all VNIs.
	VNIAdvertisementAll VNIAdvertisement = "All"
)

// VNI contains common fields for all VNI types.
type VNI struct {
	// VNI is the VXLAN Network Identifier (1-16777215).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16777215
	VNI uint32 `json:"vni"`

	// RD is the route distinguisher for this VNI.
	// Format: A.B.C.D:MN|EF:OPQR|GHJK:MN (e.g., "65000:100" or "192.0.2.1:100")
	// +optional
	RD RouteDistinguisher `json:"rd,omitempty"`

	// ImportRTs is the list of route targets to import.
	// Format: A.B.C.D:MN|EF:OPQR|GHJK:MN|*:MN|*:OPQR (e.g., "65000:100", "192.0.2.1:100", "*:100")
	// +optional
	// +kubebuilder:validation:MaxItems=100
	ImportRTs []ImportRouteTarget `json:"importRTs,omitempty"`

	// ExportRTs is the list of route targets to export.
	// Format: A.B.C.D:MN|EF:OPQR|GHJK:MN (e.g., "65000:100", "192.0.2.1:100")
	// +optional
	// +kubebuilder:validation:MaxItems=100
	ExportRTs []ExportRouteTarget `json:"exportRTs,omitempty"`
}

// L2VNI represents a Layer 2 VNI configuration.
type L2VNI struct {
	VNI `json:",inline"`
}

// L3VNI represents a Layer 3 VNI configuration.
type L3VNI struct {
	VNI `json:",inline"`

	// AdvertisePrefixes controls which prefixes to advertise as EVPN type-5 routes.
	// - "unicast": advertise the unicast prefixes of the router.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1
	AdvertisePrefixes []AdvertisePrefixType `json:"advertisePrefixes"`
}

// RouteDistinguisher defines an 8-byte BGP identifier.
// +kubebuilder:validation:MaxLength=21
// +kubebuilder:validation:XValidation:rule="self.split(':').size() == 2",message="RD must contain exactly one colon"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || (isIP(self.split(':')[0]) || self.split(':')[0].matches('[0-9]+'))",message="RD global administrator must be either an IPv4 address or a number"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || self.split(':')[1].matches('[0-9]+')",message="RD local administrator must be a number"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || !self.split(':')[0].contains('.') || (self.split(':')[1].matches('[0-9]+') && uint(self.split(':')[1]) <= 65535u)",message="RD with IPv4 global administrator must have format A.B.C.D:MN where MN <= 65535"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || self.split(':')[0].contains('.') || !self.split(':')[0].matches('[0-9]+') || !self.split(':')[1].matches('[0-9]+') || uint(self.split(':')[0]) <= 65535u || uint(self.split(':')[1]) <= 65535u",message="RD with 4-byte ASN global administrator must have format GHJK:MN where GHJK <= 4294967295 and MN <= 65535"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || self.split(':')[0].contains('.') || !self.split(':')[0].matches('[0-9]+') || !self.split(':')[1].matches('[0-9]+') || uint(self.split(':')[0]) > 65535u || uint(self.split(':')[1]) <= 4294967295u",message="RD with 2-byte ASN global administrator must have format EF:OPQR where EF <= 65535 and OPQR <= 4294967295"
type RouteDistinguisher string

// ImportRouteTarget defines a BGP Extended Community for route filtering on import.
// Supports wildcard matching with "*" as the global administrator (e.g., "*:100").
// +kubebuilder:validation:MaxLength=21
// +kubebuilder:validation:XValidation:rule="self.split(':').size() == 2",message="RT must contain exactly one colon"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || (self.startsWith('*:') || isIP(self.split(':')[0]) || self.split(':')[0].matches('[0-9]+'))",message="RT global administrator must be either '*', an IPv4 address, or a number"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || self.split(':')[1].matches('[0-9]+')",message="RT local administrator must be a number"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || !self.startsWith('*:') || (self.split(':')[1].matches('[0-9]+') && uint(self.split(':')[1]) <= 4294967295u)",message="RT with wildcard global administrator must have format *:OPQR where OPQR <= 4294967295"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || !self.split(':')[0].contains('.') || (self.split(':')[1].matches('[0-9]+') && uint(self.split(':')[1]) <= 65535u)",message="RT with IPv4 global administrator must have format A.B.C.D:MN where MN <= 65535"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || self.startsWith('*:') || self.split(':')[0].contains('.') || !self.split(':')[0].matches('[0-9]+') || !self.split(':')[1].matches('[0-9]+') || uint(self.split(':')[0]) <= 65535u || uint(self.split(':')[1]) <= 65535u",message="RT with 4-byte ASN global administrator must have format GHJK:MN where GHJK <= 4294967295 and MN <= 65535"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || self.startsWith('*:') || self.split(':')[0].contains('.') || !self.split(':')[0].matches('[0-9]+') || !self.split(':')[1].matches('[0-9]+') || uint(self.split(':')[0]) > 65535u || uint(self.split(':')[1]) <= 4294967295u",message="RT with 2-byte ASN global administrator must have format EF:OPQR where EF <= 65535 and OPQR <= 4294967295"
type ImportRouteTarget string

// ExportRouteTarget defines a BGP Extended Community for route filtering on export.
// Does NOT support wildcard matching (wildcards are only valid for import).
// +kubebuilder:validation:MaxLength=21
// +kubebuilder:validation:XValidation:rule="self.split(':').size() == 2",message="RT must contain exactly one colon"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || (isIP(self.split(':')[0]) || self.split(':')[0].matches('[0-9]+'))",message="RT global administrator must be either an IPv4 address or a number"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || self.split(':')[1].matches('[0-9]+')",message="RT local administrator must be a number"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || !self.split(':')[0].contains('.') || (self.split(':')[1].matches('[0-9]+') && uint(self.split(':')[1]) <= 65535u)",message="RT with IPv4 global administrator must have format A.B.C.D:MN where MN <= 65535"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || self.split(':')[0].contains('.') || !self.split(':')[0].matches('[0-9]+') || !self.split(':')[1].matches('[0-9]+') || uint(self.split(':')[0]) <= 65535u || uint(self.split(':')[1]) <= 65535u",message="RT with 4-byte ASN global administrator must have format GHJK:MN where GHJK <= 4294967295 and MN <= 65535"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || self.split(':')[0].contains('.') || !self.split(':')[0].matches('[0-9]+') || !self.split(':')[1].matches('[0-9]+') || uint(self.split(':')[0]) > 65535u || uint(self.split(':')[1]) <= 4294967295u",message="RT with 2-byte ASN global administrator must have format EF:OPQR where EF <= 65535 and OPQR <= 4294967295"
type ExportRouteTarget string
