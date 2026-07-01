// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package bridgeconfig

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops/ovs"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/egressip"
	nodetypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/types"
	nodeutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// BridgeUDNConfiguration holds the patchport and ctMark
// information for a given network
type BridgeUDNConfiguration struct {
	PatchPort     string
	OfPortPatch   string
	MasqCTMark    string
	PktMark       string
	V4MasqIPs     *udn.MasqueradeIPs
	V6MasqIPs     *udn.MasqueradeIPs
	Subnets       []config.CIDRNetworkEntry
	NodeSubnets   []*net.IPNet
	Advertised    atomic.Bool
	ManagementIPs []*net.IPNet
	// Transport mode for this network ("no-overlay", "evpn", or "" for default network)
	Transport string
	// OutboundSNAT setting for this network ("enabled", "disabled", or "")
	OutboundSNAT string
}

func (netConfig *BridgeUDNConfiguration) ShallowCopy() *BridgeUDNConfiguration {
	copy := &BridgeUDNConfiguration{
		PatchPort:    netConfig.PatchPort,
		OfPortPatch:  netConfig.OfPortPatch,
		MasqCTMark:   netConfig.MasqCTMark,
		PktMark:      netConfig.PktMark,
		V4MasqIPs:    netConfig.V4MasqIPs,
		V6MasqIPs:    netConfig.V6MasqIPs,
		Subnets:      netConfig.Subnets,
		NodeSubnets:  netConfig.NodeSubnets,
		Transport:    netConfig.Transport,
		OutboundSNAT: netConfig.OutboundSNAT,
	}
	copy.Advertised.Store(netConfig.Advertised.Load())
	return copy
}

func (netConfig *BridgeUDNConfiguration) IsDefaultNetwork() bool {
	return netConfig.MasqCTMark == nodetypes.CtMarkOVN
}

func (netConfig *BridgeUDNConfiguration) setOfPatchPort() error {
	ofportPatch, stderr, err := util.GetOVSOfPort("get", "Interface", netConfig.PatchPort, "ofport")
	if err != nil {
		return fmt.Errorf("failed while waiting on patch port %q to be created by ovn-controller and "+
			"while getting ofport. stderr: %v, error: %v", netConfig.PatchPort, stderr, err)
	}
	netConfig.OfPortPatch = ofportPatch
	return nil
}

type BridgeConfiguration struct {
	mutex sync.Mutex

	// variables that are only set on creation and never changed
	// don't require mutex lock to read
	nodeName   string
	bridgeName string
	uplinkName string
	gwIface    string
	// gwIfaceRep is the OVS representor port for the gateway MAC when one exists.
	// In DPU mode this is the host representor. In accelerated full mode this is
	// the representor of the configured gateway VF/SF.
	gwIfaceRep  string
	interfaceID string

	// variables that can be updated (read/write access should be done with mutex held)
	ofPortHost string
	ips        []*net.IPNet
	macAddress net.HardwareAddr
	ofPortPhys string
	netConfig  map[string]*BridgeUDNConfiguration
	eipMarkIPs *egressip.MarkIPsCache
	dropGARP   bool
}

func NewBridgeConfiguration(ovsClient libovsdbclient.Client, intfName, nodeName,
	physicalNetworkName string,
	nodeSubnets, gwIPs []*net.IPNet,
	advertised bool) (*BridgeConfiguration, error) {
	var intfRep string
	var err error
	isGWAcclInterface := false
	gwIntf := intfName

	defaultNetConfig := &BridgeUDNConfiguration{
		MasqCTMark:  nodetypes.CtMarkOVN,
		Subnets:     config.Default.ClusterSubnets,
		NodeSubnets: nodeSubnets,
	}
	for _, subnet := range nodeSubnets {
		defaultNetConfig.ManagementIPs = append(defaultNetConfig.ManagementIPs, util.GetNodeManagementIfAddr(subnet))
	}
	res := BridgeConfiguration{
		nodeName: nodeName,
		netConfig: map[string]*BridgeUDNConfiguration{
			types.DefaultNetworkName: defaultNetConfig,
		},
		eipMarkIPs: egressip.NewMarkIPsCache(),
	}
	res.netConfig[types.DefaultNetworkName].Advertised.Store(advertised)

	// temp workaround for https://issues.redhat.com/browse/FDP-1537
	// we need to ensure we continue dropping GARPs for any new bridge config if the run mode is ovnkube controller + ovnkube node + single zone node
	// FIXME: only add if run mode is ovnkube controller + node in single process
	if config.OVNKubernetesFeature.EnableEgressIP && config.IsModeFull() {
		// drop by default - set to false later when ovnkube controller has sync'd and changes propagated to OVN southbound database
		// we should also match on run mode here to ensure ovnkube controller + ovnkube node are running in the same process
		res.dropGARP = true
	}
	// end temp work around

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
		res.bridgeName = bridgeName
		res.uplinkName = uplinkName
		res.gwIfaceRep = intfRep
		res.gwIface = gwIntf
		res.macAddress = link.Attrs().HardwareAddr
	} else if bridgeName, _, err := util.RunOVSVsctl("port-to-br", intfName); err == nil {
		// This is an OVS bridge's internal port
		uplinkName, err := util.GetNicName(bridgeName)
		if err != nil {
			return nil, fmt.Errorf("failed to find nic name for bridge %s: %w", bridgeName, err)
		}
		res.bridgeName = bridgeName
		res.gwIface = bridgeName
		res.uplinkName = uplinkName
		gwIntf = bridgeName
	} else if _, _, err := util.RunOVSVsctl("br-exists", intfName); err != nil {
		// This is not a OVS bridge. We need to create a OVS bridge
		// and add cluster.GatewayIntf as a port of that bridge.
		bridgeName, err := util.NicToBridge(intfName)
		if err != nil {
			return nil, fmt.Errorf("nicToBridge failed for %s: %w", intfName, err)
		}
		if config.Gateway.DPUHostGatewayRepresentorInterface != "" {
			_, stderr, repErr := util.RunOVSVsctl(
				"--", "--may-exist", "add-port", bridgeName, config.Gateway.DPUHostGatewayRepresentorInterface,
				"--", "set", "port", config.Gateway.DPUHostGatewayRepresentorInterface, "other-config:transient=true",
			)
			if repErr != nil {
				return nil, fmt.Errorf("failed to add DPU host gateway representor %s to bridge %s: %w, stderr: %s",
					config.Gateway.DPUHostGatewayRepresentorInterface, bridgeName, repErr, stderr)
			}
			klog.Infof("Adding host representor interface %s to bridge %s", config.Gateway.DPUHostGatewayRepresentorInterface, bridgeName)
			res.gwIfaceRep = config.Gateway.DPUHostGatewayRepresentorInterface
		}
		res.bridgeName = bridgeName
		res.gwIface = bridgeName
		res.uplinkName = intfName
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
			res.uplinkName = uplinkName
		}
		res.bridgeName = intfName
		res.gwIface = intfName
	}
	// Now, we get IP addresses for the bridge
	if len(gwIPs) > 0 {
		// use gwIPs if provided
		res.ips = gwIPs
	} else {
		// get IP addresses from OVS bridge. If IP does not exist,
		// error out.
		res.ips, err = nodeutil.GetNetworkInterfaceIPAddresses(gwIntf)
		if err != nil {
			return nil, fmt.Errorf("failed to get interface details for %s: %w", gwIntf, err)
		}
	}

	if !isGWAcclInterface { // We do not have an accelerated device for Gateway interface
		res.macAddress, err = util.GetOVSPortMACAddress(gwIntf)
		if err != nil {
			return nil, fmt.Errorf("failed to get MAC address for ovs port %s: %w", gwIntf, err)
		}
	}

	res.interfaceID, err = bridgedGatewayNodeSetup(ovsClient, nodeName, res.bridgeName, physicalNetworkName)
	if err != nil {
		return nil, fmt.Errorf("failed to set up shared interface gateway: %v", err)
	}

	// the name of the patch port created by ovn-controller is of the form
	// patch-<logical_port_name_of_localnet_port>-to-br-int
	defaultNetConfig.PatchPort = (&util.DefaultNetInfo{}).GetNetworkScopedPatchPortName(res.bridgeName, nodeName)

	// for DPU we use the host MAC address for the Gateway configuration
	if config.IsModeDPU() {
		if err := res.setDPUHostGatewayConfiguration(nodeName); err != nil {
			return nil, err
		}
	}

	// If gwIface is set, then accelerated GW interface is present and we use it. Else use external bridge instead.
	if res.gwIface == "" {
		res.gwIface = res.bridgeName
	}

	return &res, nil
}

func (b *BridgeConfiguration) setDPUHostGatewayConfiguration(nodeName string) error {
	if b.gwIfaceRep == "" {
		b.gwIfaceRep = config.Gateway.DPUHostGatewayRepresentorInterface
	}
	if b.gwIfaceRep == "" {
		// When the DPU host representor was not provided explicitly, discover
		// it by inspecting the ports attached to the gateway bridge.
		klog.V(5).Infof("No DPU host gateway representor configured, discovering host representor from bridge %s", b.bridgeName)
		hostRep, err := util.GetDPUOps().GetDPUHostRepInterface(b.bridgeName)
		if err != nil {
			return err
		}
		b.gwIfaceRep = hostRep
	}
	macAddress, err := util.GetDPUOps().GetHostGatewayMACAddress(b.bridgeName, nodeName)
	if err != nil {
		return err
	}
	b.macAddress = macAddress
	return nil
}

// NewUnmanagedBridgeConfiguration creates a bridge configuration for an
// existing OVS bridge. The caller is responsible for provisioning the bridge
// and moving gateway IPs/MACs onto the selected host interface.
func NewUnmanagedBridgeConfiguration(ovsClient libovsdbclient.Client, bridgeName, hostInterfaceName, nodeName,
	physicalNetworkName string, gwIPs []*net.IPNet, macAddress net.HardwareAddr) (*BridgeConfiguration, error) {
	if _, err := ovsops.GetBridge(ovsClient, bridgeName); err != nil {
		return nil, fmt.Errorf("failed to find OVS bridge %s: %w", bridgeName, err)
	}
	if len(gwIPs) == 0 {
		return nil, fmt.Errorf("gateway IP addresses are required for OVS bridge %s", bridgeName)
	}
	if macAddress == nil {
		return nil, fmt.Errorf("gateway MAC address is required for OVS bridge %s", bridgeName)
	}
	uplinkName, err := getIntfName(bridgeName)
	if err != nil {
		return nil, fmt.Errorf("failed to find uplink interface for OVS bridge %s: %w", bridgeName, err)
	}
	interfaceID, err := bridgedGatewayNodeSetup(ovsClient, nodeName, bridgeName, physicalNetworkName)
	if err != nil {
		return nil, fmt.Errorf("failed to set up shared interface gateway: %v", err)
	}

	gwIface := hostInterfaceName
	var gwIfaceRep string
	if gwIface == "" || config.IsModeDPU() {
		gwIface = bridgeName
		if config.IsModeDPU() {
			gwIfaceRep, err = util.GetDPUOps().GetDPUHostRepInterface(bridgeName)
			if err != nil {
				return nil, err
			}
		}
	} else {
		gwIfaceRep, err = gatewayHostOVSInterface(bridgeName, gwIface)
		if err != nil {
			return nil, err
		}
	}

	return &BridgeConfiguration{
		nodeName:    nodeName,
		bridgeName:  bridgeName,
		uplinkName:  uplinkName,
		gwIface:     gwIface,
		gwIfaceRep:  gwIfaceRep,
		interfaceID: interfaceID,
		ips:         gwIPs,
		macAddress:  macAddress,
		netConfig:   map[string]*BridgeUDNConfiguration{},
		eipMarkIPs:  egressip.NewMarkIPsCache(),
	}, nil
}

func (b *BridgeConfiguration) GetGatewayIface() string {
	return b.gwIface
}

func (b *BridgeConfiguration) GetGatewayIfaceRep() string {
	return b.gwIfaceRep
}

// GetStaticFDBPort returns the OVS bridge port that owns the gateway MAC.
func (b *BridgeConfiguration) GetStaticFDBPort() string {
	if b.gwIfaceRep != "" {
		return b.gwIfaceRep
	}
	return b.bridgeName
}

// UpdateInterfaceIPAddresses sets and returns the bridge's current ips
func (b *BridgeConfiguration) UpdateInterfaceIPAddresses(node *corev1.Node) ([]*net.IPNet, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	ifAddrs, err := nodeutil.GetNetworkInterfaceIPAddresses(b.GetGatewayIface())
	if err != nil {
		return nil, err
	}

	// For DPU, here we need to use the DPU host's IP address which is the tenant cluster's
	// host internal IP address instead of the DPU's external bridge IP address.
	if config.IsModeDPU() {
		nodeIfAddr, err := util.GetNodePrimaryDPUHostAddrAnnotation(node)
		if err != nil {
			return nil, err
		}
		// For DPU mode, we only support IPv4 for now.
		nodeAddrStr := nodeIfAddr.IPv4

		nodeAddr, _, err := net.ParseCIDR(nodeAddrStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse node IP address. %v", nodeAddrStr)
		}
		ifAddrs, err = nodeutil.GetDPUHostPrimaryIPAddresses(nodeAddr, ifAddrs)
		if err != nil {
			return nil, err
		}
	}

	b.ips = ifAddrs
	return ifAddrs, nil
}

// GetPortConfigurations returns a slice of Network port configurations along with the
// uplinkName and physical port's ofport value
func (b *BridgeConfiguration) GetPortConfigurations() ([]*BridgeUDNConfiguration, string, string) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	var netConfigs []*BridgeUDNConfiguration
	for _, netConfig := range b.netConfig {
		netConfigs = append(netConfigs, netConfig.ShallowCopy())
	}
	return netConfigs, b.uplinkName, b.ofPortPhys
}

// AddNetworkConfig adds the patchport and ctMark value for the provided netInfo into the bridge configuration cache
func (b *BridgeConfiguration) AddNetworkConfig(nInfo util.NetInfo, nodeSubnets, mgmtIPs []*net.IPNet, masqCTMark, pktMark uint, v6MasqIPs, v4MasqIPs *udn.MasqueradeIPs) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	netName := nInfo.GetNetworkName()
	patchPort := nInfo.GetNetworkScopedPatchPortName(b.bridgeName, b.nodeName)

	_, found := b.netConfig[netName]
	if !found {
		netConfig := &BridgeUDNConfiguration{
			PatchPort:     patchPort,
			MasqCTMark:    fmt.Sprintf("0x%x", masqCTMark),
			PktMark:       fmt.Sprintf("0x%x", pktMark),
			V4MasqIPs:     v4MasqIPs,
			V6MasqIPs:     v6MasqIPs,
			ManagementIPs: mgmtIPs,
			Subnets:       nInfo.Subnets(),
			NodeSubnets:   nodeSubnets,
			Transport:     nInfo.Transport(),
			OutboundSNAT:  nInfo.OutboundSNAT(),
		}
		netConfig.Advertised.Store(util.IsPodNetworkAdvertisedAtNode(nInfo, b.nodeName))

		b.netConfig[netName] = netConfig
	} else {
		klog.Warningf("Trying to update bridge config for network %s which already"+
			"exists in cache...networks are not mutable...ignoring update", nInfo.GetNetworkName())
	}
	return nil
}

// DelNetworkConfig deletes the provided netInfo from the bridge configuration cache
func (b *BridgeConfiguration) DelNetworkConfig(nInfo util.NetInfo) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	delete(b.netConfig, nInfo.GetNetworkName())
}

func (b *BridgeConfiguration) GetNetworkConfig(networkName string) *BridgeUDNConfiguration {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.netConfig[networkName]
}

// GetActiveNetworkBridgeConfigCopy returns a shallow copy of the network configuration corresponding to the
// provided netInfo.
//
// NOTE: if the network configuration can't be found or if the network is not patched by OVN
// yet this returns nil.
func (b *BridgeConfiguration) GetActiveNetworkBridgeConfigCopy(networkName string) *BridgeUDNConfiguration {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	if netConfig, found := b.netConfig[networkName]; found && netConfig.OfPortPatch != "" {
		return netConfig.ShallowCopy()
	}
	return nil
}

func (b *BridgeConfiguration) HasNetworkConfigs() bool {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return len(b.netConfig) > 0
}

// must be called with mutex held
func (b *BridgeConfiguration) patchedNetConfigs() []*BridgeUDNConfiguration {
	result := make([]*BridgeUDNConfiguration, 0, len(b.netConfig))
	for _, netConfig := range b.netConfig {
		if netConfig.OfPortPatch == "" {
			continue
		}
		result = append(result, netConfig)
	}
	return result
}

// IsGatewayReady checks if patch ports of every netConfig are present.
// used by gateway on newGateway readyFunc
func (b *BridgeConfiguration) IsGatewayReady() bool {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	for _, netConfig := range b.netConfig {
		ready := gatewayReady(netConfig.PatchPort)
		if !ready {
			return false
		}
	}
	return true
}

func (b *BridgeConfiguration) ConfigureBridgePorts() error {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	// Get ofport of patchPort
	for _, netConfig := range b.netConfig {
		if err := netConfig.setOfPatchPort(); err != nil {
			return fmt.Errorf("error setting bridge openflow ports for network with patchport %v: err: %v", netConfig.PatchPort, err)
		}
	}

	if b.uplinkName != "" {
		// Get ofport of physical interface
		ofportPhys, stderr, err := util.GetOVSOfPort("get", "interface", b.uplinkName, "ofport")
		if err != nil {
			return fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
				b.uplinkName, stderr, err)
		}
		b.ofPortPhys = ofportPhys
	}

	// Get ofport representing the host. That is, the representor when present, ovsLocalPort otherwise.
	b.ofPortHost = nodetypes.OvsLocalPort
	hostOVSInterfaceName := b.bridgeName
	if b.gwIfaceRep != "" {
		ofPortHost, stderr, err := util.RunOVSVsctl("get", "interface", b.gwIfaceRep, "ofport")
		if err != nil {
			return fmt.Errorf("failed to get ofport of gateway representor %s, stderr: %q, error: %v",
				b.gwIfaceRep, stderr, err)
		}
		b.ofPortHost = ofPortHost
		hostOVSInterfaceName = b.gwIfaceRep
	}

	// Ensure the host port on the bridge carries the configured VLAN tag when requested.
	if hostOVSInterfaceName != "" && config.Gateway.VLANID != 0 {
		ifaceUUID, stderr, err := util.RunOVSVsctl("--data=bare", "--no-heading", "--columns=_uuid",
			"find", "Interface", fmt.Sprintf("name=%s", hostOVSInterfaceName))
		if err != nil {
			return fmt.Errorf("failed to find interface %s on bridge %s, stderr: %q, error: %v",
				hostOVSInterfaceName, b.bridgeName, stderr, err)
		}
		ifaceUUID = strings.TrimSpace(ifaceUUID)
		if ifaceUUID == "" {
			return fmt.Errorf("failed to determine interface UUID for %s on bridge %s", hostOVSInterfaceName, b.bridgeName)
		}

		portName, stderr, err := util.RunOVSVsctl("--data=bare", "--no-heading", "--columns=name",
			"find", "Port", fmt.Sprintf("interface=%s", ifaceUUID))
		if err != nil {
			return fmt.Errorf("failed to find port for interface %s on bridge %s, stderr: %q, error: %v",
				hostOVSInterfaceName, b.bridgeName, stderr, err)
		}
		portName = strings.TrimSpace(portName)
		if portName == "" {
			return fmt.Errorf("failed to determine port for host interface %s on bridge %s", hostOVSInterfaceName, b.bridgeName)
		}
		if _, stderr, err = util.RunOVSVsctl("set", "Port", portName,
			fmt.Sprintf("tag=%d", config.Gateway.VLANID)); err != nil {
			return fmt.Errorf("failed to set VLAN tag on port %s for bridge %s, stderr: %q, error: %v",
				portName, b.bridgeName, stderr, err)
		}
	}

	return nil
}

func (b *BridgeConfiguration) GetIPs() []*net.IPNet {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.ips
}

func (b *BridgeConfiguration) GetBridgeName() string {
	return b.bridgeName
}

func (b *BridgeConfiguration) GetUplinkName() string {
	return b.uplinkName
}

func (b *BridgeConfiguration) GetMAC() net.HardwareAddr {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.macAddress
}

func (b *BridgeConfiguration) SetMAC(macAddr net.HardwareAddr) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.macAddress = macAddr
}

func (b *BridgeConfiguration) SetNetworkOfPatchPort(netName string) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	netConfig, found := b.netConfig[netName]
	if !found {
		return fmt.Errorf("failed to find network %s configuration on bridge %s", netName, b.bridgeName)
	}
	return netConfig.setOfPatchPort()
}

func (b *BridgeConfiguration) GetInterfaceID() string {
	return b.interfaceID
}

func (b *BridgeConfiguration) GetOfPortHost() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.ofPortHost
}

func (b *BridgeConfiguration) GetEIPMarkIPs() *egressip.MarkIPsCache {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.eipMarkIPs
}

func (b *BridgeConfiguration) SetEIPMarkIPs(eipMarkIPs *egressip.MarkIPsCache) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.eipMarkIPs = eipMarkIPs
}

func (b *BridgeConfiguration) SetDropGARP(drop bool) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.dropGARP = drop
}

func gatewayReady(patchPort string) bool {
	// Get ofport of patchPort
	ofport, _, err := util.GetOVSOfPort("--if-exists", "get", "interface", patchPort, "ofport")
	if err != nil || len(ofport) == 0 {
		return false
	}
	klog.Info("Gateway is ready")
	return true
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
func bridgedGatewayNodeSetup(ovsClient libovsdbclient.Client, nodeName, bridgeName, physicalNetworkName string) (string, error) {
	// IPv6 forwarding is enabled globally
	if config.IPv4Mode {
		err := util.SetForwardingModeForInterface(bridgeName)
		if err != nil {
			return "", err
		}
	}

	// ovn-bridge-mappings maps a physical network name to a local ovs bridge
	// that provides connectivity to that network. It is in the form of physnet1:br1,physnet2:br2.
	// Note that there may be multiple ovs bridge mappings, be sure not to override
	// the mappings for the other physical network
	existing := ""
	if ovs, err := ovsops.GetOpenvSwitch(ovsClient); err != nil {
		if !errors.Is(err, libovsdbclient.ErrNotFound) {
			return "", fmt.Errorf("failed to get Open_vSwitch row: %w", err)
		}
		// no row yet => no existing mappings
	} else {
		existing = ovs.ExternalIDs["ovn-bridge-mappings"]
	}

	// skip the existing mapping setting for the specified physicalNetworkName
	mapString := ""
	if existing != "" {
		for _, bridgeMapping := range strings.Split(existing, ",") {
			m := strings.Split(bridgeMapping, ":")
			if network := m[0]; network != physicalNetworkName {
				if len(mapString) != 0 {
					mapString += ","
				}
				mapString += bridgeMapping
			}
		}
	}
	if len(mapString) != 0 {
		mapString += ","
	}
	mapString += physicalNetworkName + ":" + bridgeName

	if err := ovsops.UpdateOpenvSwitchExternalIDs(ovsClient, map[string]string{
		"ovn-bridge-mappings": mapString,
	}); err != nil {
		return "", fmt.Errorf("failed to set ovn-bridge-mappings for ovs bridge %s: %w", bridgeName, err)
	}

	ifaceID := bridgeName + "_" + nodeName
	return ifaceID, nil
}

func getRepresentor(intfName string) (string, error) {
	return util.GetNetdeviceRepresentorName(intfName)
}

func gatewayHostOVSInterface(bridgeName, gwIface string) (string, error) {
	if gwIface == "" || gwIface == bridgeName {
		return "", nil
	}

	if bridgeForInterface, _, err := util.RunOVSVsctl("port-to-br", gwIface); err == nil {
		bridgeForInterface = strings.TrimSpace(bridgeForInterface)
		if bridgeForInterface == bridgeName {
			return gwIface, nil
		}
		if bridgeForInterface != "" {
			return "", fmt.Errorf("gateway interface %s belongs to OVS bridge %s, expected %s",
				gwIface, bridgeForInterface, bridgeName)
		}
	}

	gwIfaceRep, err := getRepresentor(gwIface)
	if err != nil {
		return "", fmt.Errorf("gateway interface %s is not the OVS bridge interface, an OVS port on bridge %s, or an accelerated VF/SF netdevice: %w",
			gwIface, bridgeName, err)
	}
	bridgeForRep, stderr, err := util.RunOVSVsctl("port-to-br", gwIfaceRep)
	if err != nil {
		return "", fmt.Errorf("failed to resolve OVS bridge for representor %s of gateway interface %s, stderr: %q, error: %w",
			gwIfaceRep, gwIface, stderr, err)
	}
	bridgeForRep = strings.TrimSpace(bridgeForRep)
	if bridgeForRep != bridgeName {
		return "", fmt.Errorf("representor %s of gateway interface %s belongs to OVS bridge %s, expected %s",
			gwIfaceRep, gwIface, bridgeForRep, bridgeName)
	}
	return gwIfaceRep, nil
}
