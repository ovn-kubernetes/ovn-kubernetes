package bridgeconfig

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/udn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/egressipgw"
	nodeutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// CtMarkOVN is the conntrack mark value for OVN traffic
	CtMarkOVN = "0x1"
)

// BridgeUDNConfiguration holds the patchport and ctMark
// information for a given network
type BridgeUDNConfiguration struct {
	PatchPort   string
	OfPortPatch string
	MasqCTMark  string
	PktMark     string
	V4MasqIPs   *udn.MasqueradeIPs
	V6MasqIPs   *udn.MasqueradeIPs
	Subnets     []config.CIDRNetworkEntry
	NodeSubnets []*net.IPNet
	Advertised  atomic.Bool
}

func (netConfig *BridgeUDNConfiguration) ShallowCopy() *BridgeUDNConfiguration {
	copy := &BridgeUDNConfiguration{
		PatchPort:   netConfig.PatchPort,
		OfPortPatch: netConfig.OfPortPatch,
		MasqCTMark:  netConfig.MasqCTMark,
		PktMark:     netConfig.PktMark,
		V4MasqIPs:   netConfig.V4MasqIPs,
		V6MasqIPs:   netConfig.V6MasqIPs,
		Subnets:     netConfig.Subnets,
		NodeSubnets: netConfig.NodeSubnets,
	}
	netConfig.Advertised.Store(netConfig.Advertised.Load())
	return copy
}

func (netConfig *BridgeUDNConfiguration) IsDefaultNetwork() bool {
	return netConfig.MasqCTMark == CtMarkOVN
}

func (netConfig *BridgeUDNConfiguration) SetBridgeNetworkOfPortsInternal() error {
	ofportPatch, stderr, err := util.GetOVSOfPort("get", "Interface", netConfig.PatchPort, "ofport")
	if err != nil {
		return fmt.Errorf("failed while waiting on patch port %q to be created by ovn-controller and "+
			"while getting ofport. stderr: %v, error: %v", netConfig.PatchPort, stderr, err)
	}
	netConfig.OfPortPatch = ofportPatch
	return nil
}

type BridgeConfiguration struct {
	sync.Mutex
	NodeName    string
	BridgeName  string
	UplinkName  string
	GwIface     string
	GwIfaceRep  string
	Ips         []*net.IPNet
	InterfaceID string
	MacAddress  net.HardwareAddr
	OfPortPhys  string
	OfPortHost  string
	NetConfig   map[string]*BridgeUDNConfiguration
	EipMarkIPs  *egressipgw.MarkIPsCache
	NextHops    []net.IP
}

func (b *BridgeConfiguration) GetGatewayIface() string {
	// If GwIface is set, then accelerated GW interface is present and we use it. If else use external bridge instead.
	if b.GwIface != "" {
		return b.GwIface
	}
	return b.BridgeName
}

// UpdateInterfaceIPAddresses sets and returns the bridge's current ips
func (b *BridgeConfiguration) UpdateInterfaceIPAddresses(node *corev1.Node) ([]*net.IPNet, error) {
	b.Lock()
	defer b.Unlock()
	ifAddrs, err := nodeutil.GetNetworkInterfaceIPAddresses(b.GetGatewayIface())
	if err != nil {
		return nil, err
	}

	// For DPU, here we need to use the DPU host's IP address which is the tenant cluster's
	// host internal IP address instead of the DPU's external bridge IP address.
	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		nodeAddrStr, err := util.GetNodePrimaryIP(node)
		if err != nil {
			return nil, err
		}
		nodeAddr := net.ParseIP(nodeAddrStr)
		if nodeAddr == nil {
			return nil, fmt.Errorf("failed to parse node IP address. %v", nodeAddrStr)
		}
		ifAddrs, err = nodeutil.GetDPUHostPrimaryIPAddresses(nodeAddr, ifAddrs)
		if err != nil {
			return nil, err
		}
	}

	b.Ips = ifAddrs
	return ifAddrs, nil
}

func BridgeForInterface(intfName, nodeName,
	physicalNetworkName string,
	nodeSubnets, gwIPs []*net.IPNet,
	gwNextHops []net.IP,
	advertised bool) (*BridgeConfiguration, error) {
	var intfRep string
	var err error
	isGWAcclInterface := false
	gwIntf := intfName

	defaultNetConfig := &BridgeUDNConfiguration{
		MasqCTMark:  CtMarkOVN,
		Subnets:     config.Default.ClusterSubnets,
		NodeSubnets: nodeSubnets,
	}
	res := BridgeConfiguration{
		NodeName: nodeName,
		NetConfig: map[string]*BridgeUDNConfiguration{
			types.DefaultNetworkName: defaultNetConfig,
		},
		EipMarkIPs: egressipgw.NewMarkIPsCache(),
	}
	if len(gwNextHops) > 0 {
		res.NextHops = gwNextHops
	}
	res.NetConfig[types.DefaultNetworkName].Advertised.Store(advertised)

	if config.Gateway.GatewayAcceleratedInterface != "" {
		// Try to get representor for the specified gateway device.
		// If function succeeds, then it is either a valid switchdev VF or SF, and we can use this accelerated device
		// for node IP, Host Ofport for Openflow etc.
		// If failed - error for improper configuration option
		intfRep, err = getRepresentor(config.Gateway.GatewayAcceleratedInterface)
		if err != nil {
			return nil, fmt.Errorf("gateway accelerated interface %s is not valid: %w", config.Gateway.GatewayAcceleratedInterface, err)
		}
		gwIntf = config.Gateway.GatewayAcceleratedInterface
		isGWAcclInterface = true
		klog.Infof("For gateway accelerated interface %s representor: %s", config.Gateway.GatewayAcceleratedInterface, intfRep)
	} else {
		intfRep, err = getRepresentor(gwIntf)
		if err == nil {
			isGWAcclInterface = true
		}
	}

	if isGWAcclInterface {
		bridgeName, _, err := util.RunOVSVsctl("port-to-br", intfRep)
		if err != nil {
			return nil, fmt.Errorf("failed to find bridge that has port %s: %w", intfRep, err)
		}
		link, err := util.GetNetLinkOps().LinkByName(gwIntf)
		if err != nil {
			return nil, fmt.Errorf("failed to get netdevice link for %s: %w", gwIntf, err)
		}
		uplinkName, err := util.GetNicName(bridgeName)
		if err != nil {
			return nil, fmt.Errorf("failed to find nic name for bridge %s: %w", bridgeName, err)
		}
		res.BridgeName = bridgeName
		res.UplinkName = uplinkName
		res.GwIfaceRep = intfRep
		res.GwIface = gwIntf
		res.MacAddress = link.Attrs().HardwareAddr
	} else if bridgeName, _, err := util.RunOVSVsctl("port-to-br", intfName); err == nil {
		// This is an OVS bridge's internal port
		uplinkName, err := util.GetNicName(bridgeName)
		if err != nil {
			return nil, fmt.Errorf("failed to find nic name for bridge %s: %w", bridgeName, err)
		}
		res.BridgeName = bridgeName
		res.GwIface = bridgeName
		res.UplinkName = uplinkName
		gwIntf = bridgeName
	} else if _, _, err := util.RunOVSVsctl("br-exists", intfName); err != nil {
		// This is not a OVS bridge. We need to create a OVS bridge
		// and add cluster.GatewayIntf as a port of that bridge.
		bridgeName, err := util.NicToBridge(intfName)
		if err != nil {
			return nil, fmt.Errorf("nicToBridge failed for %s: %w", intfName, err)
		}
		res.BridgeName = bridgeName
		res.GwIface = bridgeName
		res.UplinkName = intfName
		gwIntf = bridgeName
	} else {
		// gateway interface is an OVS bridge
		uplinkName, err := getIntfName(intfName)
		if err != nil {
			if config.Gateway.Mode == config.GatewayModeLocal && config.Gateway.AllowNoUplink {
				klog.Infof("Could not find uplink for %s, setup gateway bridge with no uplink port, egress IP and egress GW will not work", intfName)
			} else {
				return nil, fmt.Errorf("failed to find intfName for %s: %w", intfName, err)
			}
		} else {
			res.UplinkName = uplinkName
		}
		res.BridgeName = intfName
		res.GwIface = intfName
	}
	// Now, we get IP addresses for the bridge
	if len(gwIPs) > 0 {
		// use gwIPs if provided
		res.Ips = gwIPs
	} else {
		// get IP addresses from OVS bridge. If IP does not exist,
		// error out.
		res.Ips, err = nodeutil.GetNetworkInterfaceIPAddresses(gwIntf)
		if err != nil {
			return nil, fmt.Errorf("failed to get interface details for %s: %w", gwIntf, err)
		}
	}

	if !isGWAcclInterface { // We do not have an accelerated device for Gateway interface
		res.MacAddress, err = util.GetOVSPortMACAddress(gwIntf)
		if err != nil {
			return nil, fmt.Errorf("failed to get MAC address for ovs port %s: %w", gwIntf, err)
		}
	}

	res.InterfaceID, err = bridgedGatewayNodeSetup(nodeName, res.BridgeName, physicalNetworkName)
	if err != nil {
		return nil, fmt.Errorf("failed to set up shared interface gateway: %v", err)
	}

	// the name of the patch port created by ovn-controller is of the form
	// patch-<logical_port_name_of_localnet_port>-to-br-int
	defaultNetConfig.PatchPort = (&util.DefaultNetInfo{}).GetNetworkScopedPatchPortName(res.BridgeName, nodeName)

	// for DPU we use the host MAC address for the Gateway configuration
	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		hostRep, err := util.GetDPUHostInterface(res.BridgeName)
		if err != nil {
			return nil, err
		}
		res.MacAddress, err = util.GetSriovnetOps().GetRepresentorPeerMacAddress(hostRep)
		if err != nil {
			return nil, err
		}
	}
	return &res, nil
}

func getRepresentor(intfName string) (string, error) {
	deviceID, err := util.GetDeviceIDFromNetdevice(intfName)
	if err != nil {
		return "", err
	}

	return util.GetFunctionRepresentorName(deviceID)
}

// GetBridgePortConfigurations returns a slice of Network port configurations along with the
// uplinkName and physical port's ofport value
func (b *BridgeConfiguration) GetBridgePortConfigurations() ([]*BridgeUDNConfiguration, string, string) {
	b.Lock()
	defer b.Unlock()
	var netConfigs []*BridgeUDNConfiguration
	for _, netConfig := range b.NetConfig {
		netConfigs = append(netConfigs, netConfig.ShallowCopy())
	}
	return netConfigs, b.UplinkName, b.OfPortPhys
}

// AddNetworkBridgeConfig adds the patchport and ctMark value for the provided netInfo into the bridge configuration cache
func (b *BridgeConfiguration) AddNetworkBridgeConfig(
	nInfo util.NetInfo,
	nodeSubnets []*net.IPNet,
	masqCTMark, pktMark uint,
	v6MasqIPs, v4MasqIPs *udn.MasqueradeIPs) error {
	b.Lock()
	defer b.Unlock()

	netName := nInfo.GetNetworkName()
	patchPort := nInfo.GetNetworkScopedPatchPortName(b.BridgeName, b.NodeName)

	_, found := b.NetConfig[netName]
	if !found {
		netConfig := &BridgeUDNConfiguration{
			PatchPort:   patchPort,
			MasqCTMark:  fmt.Sprintf("0x%x", masqCTMark),
			PktMark:     fmt.Sprintf("0x%x", pktMark),
			V4MasqIPs:   v4MasqIPs,
			V6MasqIPs:   v6MasqIPs,
			Subnets:     nInfo.Subnets(),
			NodeSubnets: nodeSubnets,
		}
		netConfig.Advertised.Store(util.IsPodNetworkAdvertisedAtNode(nInfo, b.NodeName))

		b.NetConfig[netName] = netConfig
	} else {
		klog.Warningf("Trying to update bridge config for network %s which already"+
			"exists in cache...networks are not mutable...ignoring update", nInfo.GetNetworkName())
	}
	return nil
}

// DelNetworkBridgeConfig deletes the provided netInfo from the bridge configuration cache
func (b *BridgeConfiguration) DelNetworkBridgeConfig(nInfo util.NetInfo) {
	b.Lock()
	defer b.Unlock()

	delete(b.NetConfig, nInfo.GetNetworkName())
}

func (b *BridgeConfiguration) GetNetworkBridgeConfig(networkName string) *BridgeUDNConfiguration {
	b.Lock()
	defer b.Unlock()
	return b.NetConfig[networkName]
}

// GetActiveNetworkBridgeConfigCopy returns a shallow copy of the network configuration corresponding to the
// provided netInfo.
//
// NOTE: if the network configuration can't be found or if the network is not patched by OVN
// yet this returns nil.
func (b *BridgeConfiguration) GetActiveNetworkBridgeConfigCopy(networkName string) *BridgeUDNConfiguration {
	b.Lock()
	defer b.Unlock()

	if netConfig, found := b.NetConfig[networkName]; found && netConfig.OfPortPatch != "" {
		return netConfig.ShallowCopy()
	}
	return nil
}

func (b *BridgeConfiguration) PatchedNetConfigs() []*BridgeUDNConfiguration {
	result := make([]*BridgeUDNConfiguration, 0, len(b.NetConfig))
	for _, netConfig := range b.NetConfig {
		if netConfig.OfPortPatch == "" {
			continue
		}
		result = append(result, netConfig)
	}
	return result
}

func getIntfName(gatewayIntf string) (string, error) {
	// The given (or autodetected) interface is an OVS bridge and this could be
	// created by us using util.NicToBridge() or it was pre-created by the user.

	// Is intfName a port of gatewayIntf?
	intfName, err := util.GetNicName(gatewayIntf)
	if err != nil {
		return "", err
	}
	_, stderr, err := util.RunOVSVsctl("get", "interface", intfName, "ofport")
	if err != nil {
		return "", fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
			intfName, stderr, err)
	}
	return intfName, nil
}

// bridgedGatewayNodeSetup enables forwarding on bridge interface, sets up the physical network name mappings for the bridge,
// and returns an ifaceID created from the bridge name and the node name
func bridgedGatewayNodeSetup(nodeName, bridgeName, physicalNetworkName string) (string, error) {
	// IPv6 forwarding is enabled globally
	if config.IPv4Mode {
		// we use forward slash as path separator to allow dotted bridgeName e.g. foo.200
		stdout, stderr, err := util.RunSysctl("-w", fmt.Sprintf("net/ipv4/conf/%s/forwarding=1", bridgeName))
		// systctl output enforces dot as path separator
		if err != nil || stdout != fmt.Sprintf("net.ipv4.conf.%s.forwarding = 1", strings.ReplaceAll(bridgeName, ".", "/")) {
			return "", fmt.Errorf("could not set the correct forwarding value for interface %s: stdout: %v, stderr: %v, err: %v",
				bridgeName, stdout, stderr, err)
		}
	}

	// ovn-bridge-mappings maps a physical network name to a local ovs bridge
	// that provides connectivity to that network. It is in the form of physnet1:br1,physnet2:br2.
	// Note that there may be multiple ovs bridge mappings, be sure not to override
	// the mappings for the other physical network
	stdout, stderr, err := util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".",
		"external_ids:ovn-bridge-mappings")
	if err != nil {
		return "", fmt.Errorf("failed to get ovn-bridge-mappings stderr:%s (%v)", stderr, err)
	}
	// skip the existing mapping setting for the specified physicalNetworkName
	mapString := ""
	bridgeMappings := strings.Split(stdout, ",")
	for _, bridgeMapping := range bridgeMappings {
		m := strings.Split(bridgeMapping, ":")
		if network := m[0]; network != physicalNetworkName {
			if len(mapString) != 0 {
				mapString += ","
			}
			mapString += bridgeMapping
		}
	}
	if len(mapString) != 0 {
		mapString += ","
	}
	mapString += physicalNetworkName + ":" + bridgeName

	_, stderr, err = util.RunOVSVsctl("set", "Open_vSwitch", ".",
		fmt.Sprintf("external_ids:ovn-bridge-mappings=%s", mapString))
	if err != nil {
		return "", fmt.Errorf("failed to set ovn-bridge-mappings for ovs bridge %s"+
			", stderr:%s (%v)", bridgeName, stderr, err)
	}

	ifaceID := bridgeName + "_" + nodeName
	return ifaceID, nil
}
