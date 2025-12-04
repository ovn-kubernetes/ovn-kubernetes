package types

import (
	"net"

	"github.com/containernetworking/cni/pkg/types"
)

// NetConf is CNI NetConf with DeviceID
type NetConf struct {
	types.NetConf
	// Role is valid only on L3 / L2 topologies. Not on localnet.
	// It allows for using this network to be either secondary or
	// primary user defined network for the pod.
	// primary user defined networks are used in order to achieve
	// native network isolation.
	// In order to ensure backwards compatibility, if empty the
	// network is considered secondary
	Role string `json:"role,omitempty"`
	// specifies the OVN topology for this network configuration
	// when not specified, by default it is Layer3AttachDefTopoType
	Topology string `json:"topology,omitempty"`
	// captures net-attach-def name in the form of namespace/name
	NADName string `json:"netAttachDefName,omitempty"`
	// Network MTU
	MTU int `json:"mtu,omitempty"`
	// comma-separated subnet cidr
	// for secondary layer3 network, eg. 10.128.0.0/14/23
	// for layer2 and localnet network, eg. 10.1.130.0/24
	Subnets string `json:"subnets,omitempty"`
	// comma-separated list of subnets, to be excluded from being allocated for Pod
	// valid for layer2 and localnet network topology
	// eg. "10.1.130.0/27, 10.1.130.122/32"
	ExcludeSubnets string `json:"excludeSubnets,omitempty"`
	// comma-separated list of subnets, to be reserved for static IP assignment
	// valid for layer2 topology only
	// eg. "10.1.130.0/27, 10.1.130.122/32"
	ReservedSubnets string `json:"reservedSubnets,omitempty"`
	// comma-separated list of subnets, to be reserved for OVN-Kubernetes internal network infrastructure
	// valid for layer2 network topology with primary role only
	// eg. "10.1.130.0/30, 10.1.130.64/30"
	InfrastructureSubnets string `json:"infrastructureSubnets,omitempty"`
	// join subnet cidr is required for supporting
	// services and ingress for user defined networks
	// in case of dualstack cluster, please do a comma-separated list
	// expected format:
	// 1) V4 single stack: "v4CIDR" (eg: "100.65.0.0/16")
	// 2) V6 single stack: "v6CIDR" (eg: "fd99::/64")
	// 3) dualstack: "v4CIDR,v6CIDR" (eg: "100.65.0.0/16,fd99::/64")
	// valid for UDN layer3/layer2 network topology
	// default value: 100.65.0.0/16,fd99::/64 if not provided
	JoinSubnet string `json:"joinSubnet,omitempty"`
	// transit subnet cidr was previously internally set to the default value,
	// but with the recent layer2 topology changes it may overlap with the network Subnet.
	// To avoid that, transit subnet is now configurable. Only used by Primary Layer2 networks.
	// in case of dualstack cluster, please do a comma-separated list
	TransitSubnet string `json:"transitSubnet,omitempty"`
	// comma-separated list of default gateway IPs for layer2 primary networks
	// in case of dualstack cluster, please do a comma-separated list
	// expected format:
	// 1) V4 single stack: "10.128.0.1"
	// 2) V6 single stack: "2001:db8::1"
	// 3) dualstack: "10.128.0.1,2001:db8::1"
	// valid for layer2 primary network topology only
	// when omitted, the .1 address from the subnet is used
	DefaultGatewayIPs string `json:"defaultGatewayIPs,omitempty"`
	// VLANID, valid in localnet topology network only
	VLANID int `json:"vlanID,omitempty"`
	// AllowPersistentIPs is valid on both localnet / layer topologies.
	// It allows for having IP allocations that outlive the pod for which
	// they are originally created - e.g. a KubeVirt VM's migration, or
	// restart.
	AllowPersistentIPs bool `json:"allowPersistentIPs,omitempty"`

	// PhysicalNetworkName indicates the name of the physical network to which
	// the OVN overlay will connect. Only applies to `localnet` topologies.
	// When omitted, the physical network name of the network will be the value
	// of the `name` attribute.
	// This attribute allows multiple overlays to share the same physical
	// network mapping in the hosts.
	PhysicalNetworkName string `json:"physicalNetworkName,omitempty"`

	// Transport describes the transport protocol for east-west traffic.
	// Valid values are "nooverlay", "geneve", and "evpn".
	// Defaults to "geneve".
	Transport string `json:"transport,omitempty"`

	// EVPNConfig contains configuration for EVPN mode.
	// Only valid when Transport is "evpn".
	EVPN *EVPNConfig `json:"evpn,omitempty"`

	// PciAddrs in case of using sriov or Auxiliry device name in case of SF
	DeviceID string `json:"deviceID,omitempty"`
	// LogFile to log all the messages from cni shim binary to
	LogFile string `json:"logFile,omitempty"`
	// Level is the logging verbosity level
	LogLevel string `json:"logLevel,omitempty"`
	// LogFileMaxSize is the maximum size in bytes of the logfile
	// before it gets rolled.
	LogFileMaxSize int `json:"logfile-maxsize"`
	// LogFileMaxBackups represents the maximum number of
	// old log files to retain
	LogFileMaxBackups int `json:"logfile-maxbackups"`
	// LogFileMaxAge represents the maximum number
	// of days to retain old log files
	LogFileMaxAge int `json:"logfile-maxage"`
	// Runtime arguments passed by the NPWG implementation (e.g. multus)
	RuntimeConfig struct {
		// see https://github.com/k8snetworkplumbingwg/device-info-spec
		CNIDeviceInfoFile string `json:"CNIDeviceInfoFile,omitempty"`
	} `json:"runtimeConfig,omitempty"`
}

// EVPNConfig contains EVPN-specific configuration for the network.
type EVPNConfig struct {
	// VTEP is the name of the VTEP CR that defines VTEP IPs for EVPN.
	VTEP string `json:"vtep"`
	// MACVRF contains the MAC-VRF configuration for Layer 2 EVPN.
	MACVRF *VRFConfig `json:"macVRF,omitempty"`
	// IPVRF contains the IP-VRF configuration for Layer 3 EVPN.
	IPVRF *VRFConfig `json:"ipVRF,omitempty"`
}

// VRFConfig contains configuration for a VRF in EVPN.
type VRFConfig struct {
	// VNI is the VXLAN Network Identifier for this VRF.
	VNI int32 `json:"vni"`
	// RouteTarget is the BGP route target for this VRF.
	RouteTarget string `json:"routeTarget,omitempty"`
}

// NetworkSelectionElement represents one element of the JSON format
// Network Attachment Selection Annotation as described in section 4.1.2
// of the CRD specification.
type NetworkSelectionElement struct {
	// Name contains the name of the Network object this element selects
	Name string `json:"name"`
	// Namespace contains the optional namespace that the network referenced
	// by Name exists in
	Namespace string `json:"namespace,omitempty"`
	// MacRequest contains an optional requested MAC address for this
	// network attachment
	MacRequest string `json:"mac,omitempty"`
	// GatewayRequest contains default route IP address for the pod
	GatewayRequest []net.IP `json:"default-route,omitempty"`
}
