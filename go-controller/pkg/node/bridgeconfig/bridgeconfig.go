package bridgeconfig

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/udn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/egressipgw"
	nodeutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// DefaultOpenFlowCookie identifies default open flow rules added to the host OVS bridge.
	// The hex number 0xdeff105, aka defflos, is meant to sound like default flows.
	DefaultOpenFlowCookie = "0xdeff105"
	// ctMarkHost is the conntrack mark value for host traffic
	ctMarkHost = "0x2"
	// OvnKubeNodeSNATMark is used to mark packets that need to be SNAT-ed to nodeIP for
	// traffic originating from egressIP and egressService controlled pods towards other nodes in the cluster.
	OvnKubeNodeSNATMark = "0x3f0"

	// CtMarkOVN is the conntrack mark value for OVN traffic
	ctMarkOVN = "0x1"
	// ovsLocalPort is the name of the OVS bridge local port
	ovsLocalPort = "LOCAL"
)

type BridgeConfiguration struct {
	mutex sync.Mutex

	// variables that are only set on init and then read
	nodeName    string
	BridgeName  string
	UplinkName  string
	gwIface     string
	gwIfaceRep  string
	InterfaceID string
	OfPortHost  string

	// variables that can be updated (read/write access should be done with mutex held)
	ips        []*net.IPNet                       // updated by address manager
	macAddress net.HardwareAddr                   // updated by openflow manager
	ofPortPhys string                             // set on gateway init
	eipMarkIPs *egressipgw.MarkIPsCache           // set on gateway init
	netConfig  map[string]*BridgeUDNConfiguration // used by everyone, updated by gateway, openflow_manager
}

func BridgeForInterface(intfName, nodeName,
	physicalNetworkName string,
	nodeSubnets, gwIPs []*net.IPNet,
	advertised bool) (*BridgeConfiguration, error) {
	var intfRep string
	var err error
	isGWAcclInterface := false
	gwIntf := intfName

	defaultNetConfig := &BridgeUDNConfiguration{
		masqCTMark:  ctMarkOVN,
		Subnets:     config.Default.ClusterSubnets,
		NodeSubnets: nodeSubnets,
	}
	res := BridgeConfiguration{
		nodeName: nodeName,
		netConfig: map[string]*BridgeUDNConfiguration{
			types.DefaultNetworkName: defaultNetConfig,
		},
		eipMarkIPs: egressipgw.NewMarkIPsCache(),
	}

	res.netConfig[types.DefaultNetworkName].Advertised.Store(advertised)

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
		res.gwIfaceRep = intfRep
		res.gwIface = gwIntf
		res.macAddress = link.Attrs().HardwareAddr
	} else if bridgeName, _, err := util.RunOVSVsctl("port-to-br", intfName); err == nil {
		// This is an OVS bridge's internal port
		uplinkName, err := util.GetNicName(bridgeName)
		if err != nil {
			return nil, fmt.Errorf("failed to find nic name for bridge %s: %w", bridgeName, err)
		}
		res.BridgeName = bridgeName
		res.gwIface = bridgeName
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
		res.gwIface = bridgeName
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
		res.macAddress, err = util.GetSriovnetOps().GetRepresentorPeerMacAddress(hostRep)
		if err != nil {
			return nil, err
		}
	}
	return &res, nil
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

func getRepresentor(intfName string) (string, error) {
	deviceID, err := util.GetDeviceIDFromNetdevice(intfName)
	if err != nil {
		return "", err
	}

	return util.GetFunctionRepresentorName(deviceID)
}

func (bridge *BridgeConfiguration) GetGatewayIface() string {
	// If gwIface is set, then accelerated GW interface is present and we use it. If else use external bridge instead.
	if bridge.gwIface != "" {
		return bridge.gwIface
	}
	return bridge.BridgeName
}

func getIPv(ipnet *net.IPNet) string {
	prefix := "ip"
	if utilnet.IsIPv6CIDR(ipnet) {
		prefix = "ipv6"
	}
	return prefix
}

func (bridge *BridgeConfiguration) DefaultBridgeFlows(hostSubnets []*net.IPNet, extraIPs []net.IP) ([]string, error) {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	dftFlows, err := bridge.flowsForDefaultNetwork(extraIPs)
	if err != nil {
		return nil, err
	}
	dftCommonFlows, err := bridge.commonFlows(hostSubnets)
	if err != nil {
		return nil, err
	}
	return append(dftFlows, dftCommonFlows...), nil
}

func (bridge *BridgeConfiguration) ExternalBridgeFlows(hostSubnets []*net.IPNet) ([]string, error) {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	return bridge.commonFlows(hostSubnets)
}

// must be called with bridge.mutex held
func (bridge *BridgeConfiguration) commonFlows(hostSubnets []*net.IPNet) ([]string, error) {
	// CAUTION: when adding new flows where the in_port is ofPortPatch and the out_port is ofPortPhys, ensure
	// that dl_src is included in match criteria!
	ofPortPhys := bridge.ofPortPhys
	bridgeMacAddress := bridge.macAddress.String()
	ofPortHost := bridge.OfPortHost
	bridgeIPs := bridge.ips

	var dftFlows []string

	strip_vlan := ""
	match_vlan := ""
	mod_vlan_id := ""
	if config.Gateway.VLANID != 0 {
		strip_vlan = "strip_vlan,"
		match_vlan = fmt.Sprintf("dl_vlan=%d,", config.Gateway.VLANID)
		mod_vlan_id = fmt.Sprintf("mod_vlan_vid:%d,", config.Gateway.VLANID)
	}

	if ofPortPhys != "" {
		// table 0, we check to see if this dest mac is the shared mac, if so flood to all ports
		actions := ""
		for _, netConfig := range bridge.patchedNetConfigs() {
			actions += "output:" + netConfig.OfPortPatch + ","
		}
		actions += strip_vlan + "output:" + ofPortHost
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=10, table=0, in_port=%s, %s dl_dst=%s, actions=%s",
				DefaultOpenFlowCookie, ofPortPhys, match_vlan, bridgeMacAddress, actions))
	}

	// table 0, check packets coming from OVN have the correct mac address. Low priority flows that are a catch all
	// for non-IP packets that would normally be forwarded with NORMAL action (table 0, priority 0 flow).
	for _, netConfig := range bridge.patchedNetConfigs() {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=10, table=0, in_port=%s, dl_src=%s, actions=output:NORMAL",
				DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress))
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=9, table=0, in_port=%s, actions=drop",
				DefaultOpenFlowCookie, netConfig.OfPortPatch))
	}

	if config.IPv4Mode {
		physicalIP, err := util.MatchFirstIPNetFamily(false, bridgeIPs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv4 physical IP of host: %v", err)
		}
		if ofPortPhys != "" {
			for _, netConfig := range bridge.patchedNetConfigs() {
				// table0, packets coming from egressIP pods that have mark 1008 on them
				// will be SNAT-ed a final time into nodeIP to maintain consistency in traffic even if the GR
				// SNATs these into egressIP prior to reaching external bridge.
				// egressService pods will also undergo this SNAT to nodeIP since these features are tied
				// together at the OVN policy level on the distributed router.
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=105, in_port=%s, dl_src=%s, ip, pkt_mark=%s "+
						"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)),output:%s",
						DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, OvnKubeNodeSNATMark,
						config.Default.ConntrackZone, physicalIP.IP, netConfig.masqCTMark, ofPortPhys))

				// table 0, packets coming from egressIP pods only from user defined networks. If an egressIP is assigned to
				// this node, then all networks get a flow even if no pods on that network were selected for by this egressIP.
				if util.IsNetworkSegmentationSupportEnabled() && config.OVNKubernetesFeature.EnableInterconnect &&
					config.Gateway.Mode != config.GatewayModeDisabled && bridge.eipMarkIPs != nil {
					if netConfig.masqCTMark != ctMarkOVN {
						for mark, eip := range bridge.eipMarkIPs.GetIPv4() {
							dftFlows = append(dftFlows,
								fmt.Sprintf("cookie=%s, priority=105, in_port=%s, dl_src=%s, ip, pkt_mark=%d, "+
									"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)), output:%s",
									DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, mark,
									config.Default.ConntrackZone, eip, netConfig.masqCTMark, ofPortPhys))
						}
					}
				}

				// table 0, packets coming from pods headed externally. Commit connections with ct_mark ctMarkOVN
				// so that reverse direction goes back to the pods.
				if netConfig.IsDefaultNetwork() {
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=100, in_port=%s, dl_src=%s, ip, "+
							"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
							DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, config.Default.ConntrackZone,
							netConfig.masqCTMark, ofPortPhys))

					// Allow OVN->Host traffic on the same node
					if config.Gateway.Mode == config.GatewayModeShared || config.Gateway.Mode == config.GatewayModeLocal {
						dftFlows = append(dftFlows, ovnToHostNetworkNormalActionFlows(netConfig, bridgeMacAddress, hostSubnets, false)...)
					}
				} else {
					//  for UDN we additionally SNAT the packet from masquerade IP -> node IP
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=100, in_port=%s, dl_src=%s, ip, ip_src=%s, "+
							"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)), output:%s",
							DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, netConfig.v4MasqIPs.GatewayRouter.IP, config.Default.ConntrackZone,
							physicalIP.IP, netConfig.masqCTMark, ofPortPhys))
				}
			}

			// table 0, packets coming from host Commit connections with ct_mark ctMarkHost
			// so that reverse direction goes back to the host.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ip, "+
					"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), %soutput:%s",
					DefaultOpenFlowCookie, ofPortHost, config.Default.ConntrackZone, ctMarkHost, mod_vlan_id, ofPortPhys))
		}
		if config.Gateway.Mode == config.GatewayModeLocal {
			for _, netConfig := range bridge.patchedNetConfigs() {
				// table 0, any packet coming from OVN send to host in LGW mode, host will take care of sending it outside if needed.
				// exceptions are traffic for egressIP and egressGW features and ICMP related traffic which will hit the priority 100 flow instead of this.
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=175, in_port=%s, tcp, nw_src=%s, "+
						"actions=ct(table=4,zone=%d)",
						DefaultOpenFlowCookie, netConfig.OfPortPatch, physicalIP.IP, config.Default.HostMasqConntrackZone))
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=175, in_port=%s, udp, nw_src=%s, "+
						"actions=ct(table=4,zone=%d)",
						DefaultOpenFlowCookie, netConfig.OfPortPatch, physicalIP.IP, config.Default.HostMasqConntrackZone))
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=175, in_port=%s, sctp, nw_src=%s, "+
						"actions=ct(table=4,zone=%d)",
						DefaultOpenFlowCookie, netConfig.OfPortPatch, physicalIP.IP, config.Default.HostMasqConntrackZone))
				// We send BFD traffic coming from OVN to outside directly using a higher priority flow
				if ofPortPhys != "" {
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=650, table=0, in_port=%s, dl_src=%s, udp, tp_dst=3784, actions=output:%s",
							DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, ofPortPhys))
				}
			}
		}

		if ofPortPhys != "" {
			// table 0, packets coming from external. Send it through conntrack and
			// resubmit to table 1 to know the state and mark of the connection.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=50, in_port=%s, ip, "+
					"actions=ct(zone=%d, nat, table=1)", DefaultOpenFlowCookie, ofPortPhys, config.Default.ConntrackZone))
		}
	}

	if config.IPv6Mode {
		physicalIP, err := util.MatchFirstIPNetFamily(true, bridgeIPs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv6 physical IP of host: %v", err)
		}
		if ofPortPhys != "" {
			for _, netConfig := range bridge.patchedNetConfigs() {
				// table0, packets coming from egressIP pods that have mark 1008 on them
				// will be DNAT-ed a final time into nodeIP to maintain consistency in traffic even if the GR
				// DNATs these into egressIP prior to reaching external bridge.
				// egressService pods will also undergo this SNAT to nodeIP since these features are tied
				// together at the OVN policy level on the distributed router.
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=105, in_port=%s, dl_src=%s, ipv6, pkt_mark=%s "+
						"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)),output:%s",
						DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, OvnKubeNodeSNATMark,
						config.Default.ConntrackZone, physicalIP.IP, netConfig.masqCTMark, ofPortPhys))

				// table 0, packets coming from egressIP pods only from user defined networks. If an egressIP is assigned to
				// this node, then all networks get a flow even if no pods on that network were selected for by this egressIP.
				if util.IsNetworkSegmentationSupportEnabled() && config.OVNKubernetesFeature.EnableInterconnect &&
					config.Gateway.Mode != config.GatewayModeDisabled && bridge.eipMarkIPs != nil {
					if netConfig.masqCTMark != ctMarkOVN {
						for mark, eip := range bridge.eipMarkIPs.GetIPv6() {
							dftFlows = append(dftFlows,
								fmt.Sprintf("cookie=%s, priority=105, in_port=%s, dl_src=%s, ipv6, pkt_mark=%d, "+
									"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)), output:%s",
									DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, mark,
									config.Default.ConntrackZone, eip, netConfig.masqCTMark, ofPortPhys))
						}
					}
				}

				// table 0, packets coming from pods headed externally. Commit connections with ct_mark ctMarkOVN
				// so that reverse direction goes back to the pods.
				if netConfig.IsDefaultNetwork() {
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=100, in_port=%s, dl_src=%s, ipv6, "+
							"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
							DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, config.Default.ConntrackZone, netConfig.masqCTMark, ofPortPhys))

					// Allow OVN->Host traffic on the same node
					if config.Gateway.Mode == config.GatewayModeShared || config.Gateway.Mode == config.GatewayModeLocal {
						dftFlows = append(dftFlows, ovnToHostNetworkNormalActionFlows(netConfig, bridgeMacAddress, hostSubnets, true)...)
					}
				} else {
					//  for UDN we additionally SNAT the packet from masquerade IP -> node IP
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=100, in_port=%s, dl_src=%s, ipv6, ipv6_src=%s, "+
							"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)), output:%s",
							DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, netConfig.v6MasqIPs.GatewayRouter.IP, config.Default.ConntrackZone,
							physicalIP.IP, netConfig.masqCTMark, ofPortPhys))
				}
			}

			// table 0, packets coming from host. Commit connections with ct_mark ctMarkHost
			// so that reverse direction goes back to the host.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ipv6, "+
					"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), %soutput:%s",
					DefaultOpenFlowCookie, ofPortHost, config.Default.ConntrackZone, ctMarkHost, mod_vlan_id, ofPortPhys))

		}
		if config.Gateway.Mode == config.GatewayModeLocal {
			for _, netConfig := range bridge.patchedNetConfigs() {
				// table 0, any packet coming from OVN send to host in LGW mode, host will take care of sending it outside if needed.
				// exceptions are traffic for egressIP and egressGW features and ICMP related traffic which will hit the priority 100 flow instead of this.
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=175, in_port=%s, tcp6, ipv6_src=%s, "+
						"actions=ct(table=4,zone=%d)",
						DefaultOpenFlowCookie, netConfig.OfPortPatch, physicalIP.IP, config.Default.HostMasqConntrackZone))
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=175, in_port=%s, udp6, ipv6_src=%s, "+
						"actions=ct(table=4,zone=%d)",
						DefaultOpenFlowCookie, netConfig.OfPortPatch, physicalIP.IP, config.Default.HostMasqConntrackZone))
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=175, in_port=%s, sctp6, ipv6_src=%s, "+
						"actions=ct(table=4,zone=%d)",
						DefaultOpenFlowCookie, netConfig.OfPortPatch, physicalIP.IP, config.Default.HostMasqConntrackZone))
				if ofPortPhys != "" {
					// We send BFD traffic coming from OVN to outside directly using a higher priority flow
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=650, table=0, in_port=%s, dl_src=%s, udp6, tp_dst=3784, actions=output:%s",
							DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, ofPortPhys))
				}
			}
		}
		if ofPortPhys != "" {
			// table 0, packets coming from external. Send it through conntrack and
			// resubmit to table 1 to know the state and mark of the connection.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=50, in_port=%s, ipv6, "+
					"actions=ct(zone=%d, nat, table=1)", DefaultOpenFlowCookie, ofPortPhys, config.Default.ConntrackZone))
		}
	}
	// Egress IP is often configured on a node different from the one hosting the affected pod.
	// Due to the fact that ovn-controllers on different nodes apply the changes independently,
	// there is a chance that the pod traffic will reach the egress node before it configures the SNAT flows.
	// Drop pod traffic that is not SNATed, excluding local pods(required for ICNIv2)
	defaultNetConfig := bridge.netConfig[types.DefaultNetworkName]
	if config.OVNKubernetesFeature.EnableEgressIP {
		for _, clusterEntry := range config.Default.ClusterSubnets {
			cidr := clusterEntry.CIDR
			ipv := getIPv(cidr)
			// table 0, drop packets coming from pods headed externally that were not SNATed.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=104, in_port=%s, %s, %s_src=%s, actions=drop",
					DefaultOpenFlowCookie, defaultNetConfig.OfPortPatch, ipv, ipv, cidr))
		}
		for _, subnet := range defaultNetConfig.NodeSubnets {
			ipv := getIPv(subnet)
			if ofPortPhys != "" {
				// table 0, commit connections from local pods.
				// ICNIv2 requires that local pod traffic can leave the node without SNAT.
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=109, in_port=%s, dl_src=%s, %s, %s_src=%s"+
						"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
						DefaultOpenFlowCookie, defaultNetConfig.OfPortPatch, bridgeMacAddress, ipv, ipv, subnet,
						config.Default.ConntrackZone, ctMarkOVN, ofPortPhys))
			}
		}
	}

	if ofPortPhys != "" {
		for _, netConfig := range bridge.patchedNetConfigs() {
			isNetworkAdvertised := netConfig.Advertised.Load()
			// disableSNATMultipleGWs only applies to default network
			disableSNATMultipleGWs := netConfig.IsDefaultNetwork() && config.Gateway.DisableSNATMultipleGWs
			if !disableSNATMultipleGWs && !isNetworkAdvertised {
				continue
			}
			output := netConfig.OfPortPatch
			if isNetworkAdvertised && config.Gateway.Mode == config.GatewayModeLocal {
				// except if advertised through BGP, go to kernel
				// TODO: MEG enabled pods should still go through the patch port
				// but holding this until
				// https://issues.redhat.com/browse/FDP-646 is fixed, for now we
				// are assuming MEG & BGP are not used together
				output = ovsLocalPort
			}
			for _, clusterEntry := range netConfig.Subnets {
				cidr := clusterEntry.CIDR
				ipv := getIPv(cidr)
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=15, table=1, %s, %s_dst=%s, "+
						"actions=output:%s",
						DefaultOpenFlowCookie, ipv, ipv, cidr, output))
			}
			if output == netConfig.OfPortPatch {
				// except node management traffic
				for _, subnet := range netConfig.NodeSubnets {
					mgmtIP := util.GetNodeManagementIfAddr(subnet)
					ipv := getIPv(mgmtIP)
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=16, table=1, %s, %s_dst=%s, "+
							"actions=output:%s",
							DefaultOpenFlowCookie, ipv, ipv, mgmtIP.IP, ovsLocalPort),
					)
				}
			}
		}

		// table 1, we check to see if this dest mac is the shared mac, if so send to host
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=10, table=1, %s dl_dst=%s, actions=%soutput:%s",
				DefaultOpenFlowCookie, match_vlan, bridgeMacAddress, strip_vlan, ofPortHost))

		if config.IPv6Mode {
			// REMOVEME(trozet) when https://bugzilla.kernel.org/show_bug.cgi?id=11797 is resolved
			// must flood icmpv6 Route Advertisement and Neighbor Advertisement traffic as it fails to create a CT entry
			for _, icmpType := range []int{types.RouteAdvertisementICMPType, types.NeighborAdvertisementICMPType} {
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=14, table=1,icmp6,icmpv6_type=%d actions=FLOOD",
						DefaultOpenFlowCookie, icmpType))
			}
			if ofPortPhys != "" {
				// We send BFD traffic both on the host and in ovn
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=13, table=1, in_port=%s, udp6, tp_dst=3784, actions=output:%s,output:%s",
						DefaultOpenFlowCookie, ofPortPhys, defaultNetConfig.OfPortPatch, ofPortHost))
			}
		}

		if config.IPv4Mode {
			if ofPortPhys != "" {
				// We send BFD traffic both on the host and in ovn
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=13, table=1, in_port=%s, udp, tp_dst=3784, actions=output:%s,output:%s",
						DefaultOpenFlowCookie, ofPortPhys, defaultNetConfig.OfPortPatch, ofPortHost))
			}
		}

		// packets larger than known acceptable MTU need to go to kernel for
		// potential fragmentation
		// introduced specifically for replies to egress traffic not routed
		// through the host
		if config.Gateway.Mode == config.GatewayModeLocal && !config.Gateway.DisablePacketMTUCheck {
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=10, table=11, reg0=0x1, "+
					"actions=output:%s", DefaultOpenFlowCookie, ofPortHost))

			// Send UDN destined traffic to right patch port
			for _, netConfig := range bridge.patchedNetConfigs() {
				if netConfig.masqCTMark != ctMarkOVN {
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=5, table=11, ct_mark=%s, "+
							"actions=output:%s", DefaultOpenFlowCookie, netConfig.masqCTMark, netConfig.OfPortPatch))
				}
			}

			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=1, table=11, "+
					"actions=output:%s", DefaultOpenFlowCookie, defaultNetConfig.OfPortPatch))
		}

		// table 1, all other connections do normal processing
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=0, table=1, actions=output:NORMAL", DefaultOpenFlowCookie))
	}

	return dftFlows, nil
}

// must be called with bridge.mutex held
func (bridge *BridgeConfiguration) flowsForDefaultNetwork(extraIPs []net.IP) ([]string, error) {
	// CAUTION: when adding new flows where the in_port is OfPortPatch and the out_port is ofPortPhys, ensure
	// that dl_src is included in match criteria!

	ofPortPhys := bridge.ofPortPhys
	bridgeMacAddress := bridge.macAddress.String()
	ofPortHost := bridge.OfPortHost
	bridgeIPs := bridge.ips

	var dftFlows []string
	// 14 bytes of overhead for ethernet header (does not include VLAN)
	maxPktLength := getMaxFrameLength()

	strip_vlan := ""
	mod_vlan_id := ""
	match_vlan := ""
	if config.Gateway.VLANID != 0 {
		strip_vlan = "strip_vlan,"
		match_vlan = fmt.Sprintf("dl_vlan=%d,", config.Gateway.VLANID)
		mod_vlan_id = fmt.Sprintf("mod_vlan_vid:%d,", config.Gateway.VLANID)
	}

	if config.IPv4Mode {
		// table0, Geneve packets coming from external. Skip conntrack and go directly to host
		// if dest mac is the shared mac send directly to host.
		if ofPortPhys != "" {
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=205, in_port=%s, dl_dst=%s, udp, udp_dst=%d, "+
					"actions=output:%s", DefaultOpenFlowCookie, ofPortPhys, bridgeMacAddress, config.Default.EncapPort,
					ofPortHost))
			// perform NORMAL action otherwise.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, in_port=%s, udp, udp_dst=%d, "+
					"actions=NORMAL", DefaultOpenFlowCookie, ofPortPhys, config.Default.EncapPort))

			// table0, Geneve packets coming from LOCAL/Host OFPort. Skip conntrack and go directly to external
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, in_port=%s, udp, udp_dst=%d, "+
					"actions=output:%s", DefaultOpenFlowCookie, ofPortHost, config.Default.EncapPort, ofPortPhys))
		}
		physicalIP, err := util.MatchFirstIPNetFamily(false, bridgeIPs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv4 physical IP of host: %v", err)
		}
		for _, netConfig := range bridge.patchedNetConfigs() {
			// table 0, SVC Hairpin from OVN destined to local host, DNAT and go to table 4
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ip, ip_dst=%s, ip_src=%s,"+
					"actions=ct(commit,zone=%d,nat(dst=%s),table=4)",
					DefaultOpenFlowCookie, netConfig.OfPortPatch, config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String(), physicalIP.IP,
					config.Default.HostMasqConntrackZone, physicalIP.IP))
		}

		// table 0, hairpin from OVN destined to local host (but an additional node IP), send to table 4
		for _, ip := range extraIPs {
			if ip.To4() == nil {
				continue
			}
			// not needed for the physical IP
			if ip.Equal(physicalIP.IP) {
				continue
			}

			// not needed for special masquerade IP
			if ip.Equal(config.Gateway.MasqueradeIPs.V4HostMasqueradeIP) {
				continue
			}

			for _, netConfig := range bridge.patchedNetConfigs() {
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ip, ip_dst=%s, ip_src=%s,"+
						"actions=ct(commit,zone=%d,table=4)",
						DefaultOpenFlowCookie, netConfig.OfPortPatch, ip.String(), physicalIP.IP,
						config.Default.HostMasqConntrackZone))
			}
		}

		// table 0, Reply SVC traffic from Host -> OVN, unSNAT and goto table 5
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ip, ip_dst=%s,"+
				"actions=ct(zone=%d,nat,table=5)",
				DefaultOpenFlowCookie, ofPortHost, config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP.String(), config.Default.OVNMasqConntrackZone))
	}
	if config.IPv6Mode {
		if ofPortPhys != "" {
			// table0, Geneve packets coming from external. Skip conntrack and go directly to host
			// if dest mac is the shared mac send directly to host.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=205, in_port=%s, dl_dst=%s, udp6, udp_dst=%d, "+
					"actions=output:%s", DefaultOpenFlowCookie, ofPortPhys, bridgeMacAddress, config.Default.EncapPort,
					ofPortHost))
			// perform NORMAL action otherwise.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, in_port=%s, udp6, udp_dst=%d, "+
					"actions=NORMAL", DefaultOpenFlowCookie, ofPortPhys, config.Default.EncapPort))

			// table0, Geneve packets coming from LOCAL. Skip conntrack and send to external
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, in_port=%s, udp6, udp_dst=%d, "+
					"actions=output:%s", DefaultOpenFlowCookie, ovsLocalPort, config.Default.EncapPort, ofPortPhys))
		}

		physicalIP, err := util.MatchFirstIPNetFamily(true, bridgeIPs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv6 physical IP of host: %v", err)
		}
		// table 0, SVC Hairpin from OVN destined to local host, DNAT to host, send to table 4
		for _, netConfig := range bridge.patchedNetConfigs() {
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ipv6, ipv6_dst=%s, ipv6_src=%s,"+
					"actions=ct(commit,zone=%d,nat(dst=%s),table=4)",
					DefaultOpenFlowCookie, netConfig.OfPortPatch, config.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String(), physicalIP.IP,
					config.Default.HostMasqConntrackZone, physicalIP.IP))
		}

		// table 0, hairpin from OVN destined to local host (but an additional node IP), send to table 4
		for _, ip := range extraIPs {
			if ip.To4() != nil {
				continue
			}
			// not needed for the physical IP
			if ip.Equal(physicalIP.IP) {
				continue
			}

			// not needed for special masquerade IP
			if ip.Equal(config.Gateway.MasqueradeIPs.V6HostMasqueradeIP) {
				continue
			}

			for _, netConfig := range bridge.patchedNetConfigs() {
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ipv6, ipv6_dst=%s, ipv6_src=%s,"+
						"actions=ct(commit,zone=%d,table=4)",
						DefaultOpenFlowCookie, netConfig.OfPortPatch, ip.String(), physicalIP.IP,
						config.Default.HostMasqConntrackZone))
			}
		}

		// table 0, Reply SVC traffic from Host -> OVN, unSNAT and goto table 5
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ipv6, ipv6_dst=%s,"+
				"actions=ct(zone=%d,nat,table=5)",
				DefaultOpenFlowCookie, ofPortHost, config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP.String(), config.Default.OVNMasqConntrackZone))
	}

	var protoPrefix, masqIP, masqSubnet string

	// table 0, packets coming from Host -> Service
	for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
		if utilnet.IsIPv4CIDR(svcCIDR) {
			protoPrefix = "ip"
			masqIP = config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String()
			masqSubnet = config.Gateway.V4MasqueradeSubnet
		} else {
			protoPrefix = "ipv6"
			masqIP = config.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String()
			masqSubnet = config.Gateway.V6MasqueradeSubnet
		}

		// table 0, Host (default network) -> OVN towards SVC, SNAT to special IP.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_dst=%s, "+
				"actions=ct(commit,zone=%d,nat(src=%s),table=2)",
				DefaultOpenFlowCookie, ofPortHost, protoPrefix, protoPrefix,
				svcCIDR, config.Default.HostMasqConntrackZone, masqIP))

		if util.IsNetworkSegmentationSupportEnabled() {
			// table 0, Host (UDNs) -> OVN towards SVC, SNAT to special IP.
			// For packets originating from UDN, commit without NATing, those
			// have already been SNATed to the masq IP of the UDN.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=550, in_port=%s, %s, %s_src=%s, %s_dst=%s, "+
					"actions=ct(commit,zone=%d,table=2)",
					DefaultOpenFlowCookie, ofPortHost, protoPrefix, protoPrefix,
					masqSubnet, protoPrefix, svcCIDR, config.Default.HostMasqConntrackZone))
			if util.IsRouteAdvertisementsEnabled() {
				// If the UDN is advertised then instead of matching on the masqSubnet
				// we match on the UDNPodSubnet itself and we also don't SNAT to 169.254.0.2
				// sample flow: cookie=0xdeff105, duration=1472.742s, table=0, n_packets=9, n_bytes=666, priority=550
				//              ip,in_port=LOCAL,nw_src=103.103.0.0/16,nw_dst=10.96.0.0/16 actions=ct(commit,table=2,zone=64001)
				for _, netConfig := range bridge.patchedNetConfigs() {
					if netConfig.IsDefaultNetwork() {
						continue
					}
					if netConfig.Advertised.Load() {
						var udnAdvertisedSubnets []*net.IPNet
						for _, clusterEntry := range netConfig.Subnets {
							udnAdvertisedSubnets = append(udnAdvertisedSubnets, clusterEntry.CIDR)
						}
						// Filter subnets based on the clusterIP service family
						// NOTE: We don't support more than 1 subnet CIDR of same family type; we only pick the first one
						matchingIPFamilySubnet, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6CIDR(svcCIDR), udnAdvertisedSubnets)
						if err != nil {
							klog.Infof("Unable to determine UDN subnet for the provided family isIPV6: %t, %v", utilnet.IsIPv6CIDR(svcCIDR), err)
							continue
						}

						// Use the filtered subnet for the flow compute instead of the masqueradeIP
						dftFlows = append(dftFlows,
							fmt.Sprintf("cookie=%s, priority=550, in_port=%s, %s, %s_src=%s, %s_dst=%s, "+
								"actions=ct(commit,zone=%d,table=2)",
								DefaultOpenFlowCookie, ofPortHost, protoPrefix, protoPrefix,
								matchingIPFamilySubnet.String(), protoPrefix, svcCIDR, config.Default.HostMasqConntrackZone))
					}
				}
			}
		}

		masqDst := masqIP
		if util.IsNetworkSegmentationSupportEnabled() {
			// In UDN match on the whole masquerade subnet to handle replies from UDN enabled services
			masqDst = masqSubnet
		}
		for _, netConfig := range bridge.patchedNetConfigs() {
			// table 0, Reply hairpin traffic to host, coming from OVN, unSNAT
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_src=%s, %s_dst=%s,"+
					"actions=ct(zone=%d,nat,table=3)",
					DefaultOpenFlowCookie, netConfig.OfPortPatch, protoPrefix, protoPrefix, svcCIDR,
					protoPrefix, masqDst, config.Default.HostMasqConntrackZone))
			// table 0, Reply traffic coming from OVN to outside, drop it if the DNAT wasn't done either
			// at the GR load balancer or switch load balancer. It means the correct port wasn't provided.
			// nodeCIDR->serviceCIDR traffic flow is internal and it shouldn't be carried to outside the cluster
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=115, in_port=%s, %s, %s_dst=%s,"+
					"actions=drop", DefaultOpenFlowCookie, netConfig.OfPortPatch, protoPrefix, protoPrefix, svcCIDR))
		}
	}

	// table 0, add IP fragment reassembly flows, only needed in SGW mode with
	// physical interface attached to bridge
	if config.Gateway.Mode == config.GatewayModeShared && ofPortPhys != "" {
		reassemblyFlows := generateIPFragmentReassemblyFlow(ofPortPhys)
		dftFlows = append(dftFlows, reassemblyFlows...)
	}
	if ofPortPhys != "" {
		for _, netConfig := range bridge.patchedNetConfigs() {
			var actions string
			if config.Gateway.Mode != config.GatewayModeLocal || config.Gateway.DisablePacketMTUCheck {
				actions = fmt.Sprintf("output:%s", netConfig.OfPortPatch)
			} else {
				// packets larger than known acceptable MTU need to go to kernel for
				// potential fragmentation
				// introduced specifically for replies to egress traffic not routed
				// through the host
				actions = fmt.Sprintf("check_pkt_larger(%d)->reg0[0],resubmit(,11)", maxPktLength)
			}

			if config.IPv4Mode {
				// table 1, established and related connections in zone 64000 with ct_mark CtMarkOVN go to OVN
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=100, table=1, ip, ct_state=+trk+est, ct_mark=%s, "+
						"actions=%s", DefaultOpenFlowCookie, netConfig.masqCTMark, actions))

				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=100, table=1, ip, ct_state=+trk+rel, ct_mark=%s, "+
						"actions=%s", DefaultOpenFlowCookie, netConfig.masqCTMark, actions))

			}

			if config.IPv6Mode {
				// table 1, established and related connections in zone 64000 with ct_mark CtMarkOVN go to OVN
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=100, table=1, ipv6, ct_state=+trk+est, ct_mark=%s, "+
						"actions=%s", DefaultOpenFlowCookie, netConfig.masqCTMark, actions))

				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=100, table=1, ipv6, ct_state=+trk+rel, ct_mark=%s, "+
						"actions=%s", DefaultOpenFlowCookie, netConfig.masqCTMark, actions))
			}
		}
		if config.IPv4Mode {
			// table 1, established and related connections in zone 64000 with ct_mark ctMarkHost go to host
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, table=1, %s ip, ct_state=+trk+est, ct_mark=%s, "+
					"actions=%soutput:%s",
					DefaultOpenFlowCookie, match_vlan, ctMarkHost, strip_vlan, ofPortHost))

			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, table=1, %s ip, ct_state=+trk+rel, ct_mark=%s, "+
					"actions=%soutput:%s",
					DefaultOpenFlowCookie, match_vlan, ctMarkHost, strip_vlan, ofPortHost))

		}
		if config.IPv6Mode {
			// table 1, established and related connections in zone 64000 with ct_mark ctMarkHost go to host
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, table=1, %s ip6, ct_state=+trk+est, ct_mark=%s, "+
					"actions=%soutput:%s",
					DefaultOpenFlowCookie, match_vlan, ctMarkHost, strip_vlan, ofPortHost))

			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, table=1, %s ip6, ct_state=+trk+rel, ct_mark=%s, "+
					"actions=%soutput:%s",
					DefaultOpenFlowCookie, match_vlan, ctMarkHost, strip_vlan, ofPortHost))

		}

		// table 1, we check to see if this dest mac is the shared mac, if so send to host
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=10, table=1, %s dl_dst=%s, actions=%soutput:%s",
				DefaultOpenFlowCookie, match_vlan, bridgeMacAddress, strip_vlan, ofPortHost))
	}

	defaultNetConfig := bridge.netConfig[types.DefaultNetworkName]

	// table 2, dispatch from Host -> OVN
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, priority=100, table=2, "+
			"actions=set_field:%s->eth_dst,%soutput:%s", DefaultOpenFlowCookie,
			bridgeMacAddress, mod_vlan_id, defaultNetConfig.OfPortPatch))

	// table 2, priority 200, dispatch from UDN -> Host -> OVN. These packets have
	// already been SNATed to the UDN's masq IP or have been marked with the UDN's packet mark.
	if config.IPv4Mode {
		for _, netConfig := range bridge.patchedNetConfigs() {
			if netConfig.IsDefaultNetwork() {
				continue
			}
			srcIPOrSubnet := netConfig.v4MasqIPs.ManagementPort.IP.String()
			if util.IsRouteAdvertisementsEnabled() && netConfig.Advertised.Load() {
				var udnAdvertisedSubnets []*net.IPNet
				for _, clusterEntry := range netConfig.Subnets {
					udnAdvertisedSubnets = append(udnAdvertisedSubnets, clusterEntry.CIDR)
				}
				// Filter subnets based on the clusterIP service family
				// NOTE: We don't support more than 1 subnet CIDR of same family type; we only pick the first one
				matchingIPFamilySubnet, err := util.MatchFirstIPNetFamily(false, udnAdvertisedSubnets)
				if err != nil {
					klog.Infof("Unable to determine IPV4 UDN subnet for the provided family isIPV6: %v", err)
					continue
				}

				// Use the filtered subnets for the flow compute instead of the masqueradeIP
				srcIPOrSubnet = matchingIPFamilySubnet.String()
			}
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, table=2, ip, ip_src=%s, "+
					"actions=set_field:%s->eth_dst,output:%s",
					DefaultOpenFlowCookie, srcIPOrSubnet,
					bridgeMacAddress, netConfig.OfPortPatch))
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, table=2, ip, pkt_mark=%s, "+
					"actions=set_field:%s->eth_dst,output:%s",
					DefaultOpenFlowCookie, netConfig.PktMark,
					bridgeMacAddress, netConfig.OfPortPatch))
		}
	}

	if config.IPv6Mode {
		for _, netConfig := range bridge.patchedNetConfigs() {
			if netConfig.IsDefaultNetwork() {
				continue
			}
			srcIPOrSubnet := netConfig.v6MasqIPs.ManagementPort.IP.String()
			if util.IsRouteAdvertisementsEnabled() && netConfig.Advertised.Load() {
				var udnAdvertisedSubnets []*net.IPNet
				for _, clusterEntry := range netConfig.Subnets {
					udnAdvertisedSubnets = append(udnAdvertisedSubnets, clusterEntry.CIDR)
				}
				// Filter subnets based on the clusterIP service family
				// NOTE: We don't support more than 1 subnet CIDR of same family type; we only pick the first one
				matchingIPFamilySubnet, err := util.MatchFirstIPNetFamily(true, udnAdvertisedSubnets)
				if err != nil {
					klog.Infof("Unable to determine IPV6 UDN subnet for the provided family isIPV6: %v", err)
					continue
				}

				// Use the filtered subnets for the flow compute instead of the masqueradeIP
				srcIPOrSubnet = matchingIPFamilySubnet.String()
			}
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, table=2, ip6, ipv6_src=%s, "+
					"actions=set_field:%s->eth_dst,output:%s",
					DefaultOpenFlowCookie, srcIPOrSubnet,
					bridgeMacAddress, netConfig.OfPortPatch))
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, table=2, ip6, pkt_mark=%s, "+
					"actions=set_field:%s->eth_dst,output:%s",
					DefaultOpenFlowCookie, netConfig.PktMark,
					bridgeMacAddress, netConfig.OfPortPatch))
		}
	}

	// table 3, dispatch from OVN -> Host
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, table=3, %s "+
			"actions=move:NXM_OF_ETH_DST[]->NXM_OF_ETH_SRC[],set_field:%s->eth_dst,%soutput:%s",
			DefaultOpenFlowCookie, match_vlan, bridgeMacAddress, strip_vlan, ofPortHost))

	// table 4, hairpinned pkts that need to go from OVN -> Host
	// We need to SNAT and masquerade OVN GR IP, send to table 3 for dispatch to Host
	if config.IPv4Mode {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, table=4,ip,"+
				"actions=ct(commit,zone=%d,nat(src=%s),table=3)",
				DefaultOpenFlowCookie, config.Default.OVNMasqConntrackZone, config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP.String()))
	}
	if config.IPv6Mode {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, table=4,ipv6, "+
				"actions=ct(commit,zone=%d,nat(src=%s),table=3)",
				DefaultOpenFlowCookie, config.Default.OVNMasqConntrackZone, config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP.String()))
	}
	// table 5, Host Reply traffic to hairpinned svc, need to unDNAT, send to table 2
	if config.IPv4Mode {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, table=5, ip, "+
				"actions=ct(commit,zone=%d,nat,table=2)",
				DefaultOpenFlowCookie, config.Default.HostMasqConntrackZone))
	}
	if config.IPv6Mode {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, table=5, ipv6, "+
				"actions=ct(commit,zone=%d,nat,table=2)",
				DefaultOpenFlowCookie, config.Default.HostMasqConntrackZone))
	}
	return dftFlows, nil
}

// ovnToHostNetworkNormalActionFlows returns the flows that allow IP{v4,v6} traffic from the OVN network to the host network
// when the destination is on the same node as the sender. This is necessary for pods in the default network to reach
// localnet pods on the same node, when the localnet is mapped to breth0. The expected srcMAC is the MAC address of breth0
// and the expected hostSubnets is the host subnets found on the node primary interface.
func ovnToHostNetworkNormalActionFlows(netConfig *BridgeUDNConfiguration, srcMAC string, hostSubnets []*net.IPNet, isV6 bool) []string {
	var inPort, ctMark, ipFamily, ipFamilyDest string
	var flows []string

	if config.Gateway.Mode == config.GatewayModeShared {
		inPort = netConfig.OfPortPatch
		ctMark = netConfig.masqCTMark
	} else if config.Gateway.Mode == config.GatewayModeLocal {
		inPort = "LOCAL"
		ctMark = ctMarkHost
	} else {
		return nil
	}

	if isV6 {
		ipFamily = "ipv6"
		ipFamilyDest = "ipv6_dst"
	} else {
		ipFamily = "ip"
		ipFamilyDest = "nw_dst"
	}

	for _, hostSubnet := range hostSubnets {
		if (hostSubnet.IP.To4() == nil) != isV6 {
			continue
		}
		// IP traffic from the OVN network to the host network should be handled normally by the bridge instead of
		// being output directly to the NIC by the existing flow at prio=100.
		flows = append(flows,
			fmt.Sprintf("cookie=%s, priority=102, in_port=%s, dl_src=%s, %s, %s=%s, "+
				"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:NORMAL",
				DefaultOpenFlowCookie,
				inPort,
				srcMAC,
				ipFamily,
				ipFamilyDest,
				hostSubnet.String(),
				config.Default.ConntrackZone,
				ctMark))
	}

	if isV6 {
		// Neighbor discovery in IPv6 happens through ICMPv6 messages to a special destination (ff02::1:ff00:0/104),
		// which has nothing to do with the host subnets we're matching against in the flow above at prio=102.
		// Let's allow neighbor discovery by matching against icmp type and in_port.
		for _, icmpType := range []int{types.NeighborSolicitationICMPType, types.NeighborAdvertisementICMPType} {
			flows = append(flows,
				fmt.Sprintf("cookie=%s, priority=102, in_port=%s, dl_src=%s, icmp6, icmpv6_type=%d, "+
					"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:NORMAL",
					DefaultOpenFlowCookie, inPort, srcMAC, icmpType,
					config.Default.ConntrackZone, ctMark))
		}
	}

	return flows
}

// used by gateway on newGateway initFunc
func (bridge *BridgeConfiguration) SetBridgeOfPorts() error {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	// Get ofport of patchPort
	for _, netConfig := range bridge.netConfig {
		if err := netConfig.setBridgeNetworkOfPortsInternal(); err != nil {
			return fmt.Errorf("error setting bridge openflow ports for network with patchport %v: err: %v", netConfig.PatchPort, err)
		}
	}

	if bridge.UplinkName != "" {
		// Get ofport of physical interface
		ofportPhys, stderr, err := util.GetOVSOfPort("get", "interface", bridge.UplinkName, "ofport")
		if err != nil {
			return fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
				bridge.UplinkName, stderr, err)
		}
		bridge.ofPortPhys = ofportPhys
	}

	// Get ofport representing the host. That is, host representor port in case of DPUs, ovsLocalPort otherwise.
	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		var stderr string
		hostRep, err := util.GetDPUHostInterface(bridge.BridgeName)
		if err != nil {
			return err
		}

		bridge.OfPortHost, stderr, err = util.RunOVSVsctl("get", "interface", hostRep, "ofport")
		if err != nil {
			return fmt.Errorf("failed to get ofport of host interface %s, stderr: %q, error: %v",
				hostRep, stderr, err)
		}
	} else {
		var err error
		if bridge.gwIfaceRep != "" {
			bridge.OfPortHost, _, err = util.RunOVSVsctl("get", "interface", bridge.gwIfaceRep, "ofport")
			if err != nil {
				return fmt.Errorf("failed to get ofport of bypass rep %s, error: %v", bridge.gwIfaceRep, err)
			}
		} else {
			bridge.OfPortHost = ovsLocalPort
		}
	}

	return nil
}

// UTILS Needed for UDN (also leveraged for default netInfo) in BridgeConfiguration

// GetBridgePortConfigurations returns a slice of Network port configurations along with the
// UplinkName and physical port's ofport value
func (bridge *BridgeConfiguration) GetBridgePortConfigurations() ([]*BridgeUDNConfiguration, string, string) {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	var netConfigs []*BridgeUDNConfiguration
	for _, netConfig := range bridge.netConfig {
		netConfigs = append(netConfigs, netConfig.shallowCopy())
	}
	return netConfigs, bridge.UplinkName, bridge.ofPortPhys
}

// AddNetworkBridgeConfig adds the patchport and ctMark value for the provided netInfo into the bridge configuration cache
// used by openflow_manager on addNetwork
func (bridge *BridgeConfiguration) AddNetworkBridgeConfig(
	nInfo util.NetInfo,
	nodeSubnets []*net.IPNet,
	masqCTMark, pktMark uint,
	v6MasqIPs, v4MasqIPs *udn.MasqueradeIPs) error {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()

	netName := nInfo.GetNetworkName()
	patchPort := nInfo.GetNetworkScopedPatchPortName(bridge.BridgeName, bridge.nodeName)

	_, found := bridge.netConfig[netName]
	if !found {
		netConfig := &BridgeUDNConfiguration{
			PatchPort:   patchPort,
			masqCTMark:  fmt.Sprintf("0x%x", masqCTMark),
			PktMark:     fmt.Sprintf("0x%x", pktMark),
			v4MasqIPs:   v4MasqIPs,
			v6MasqIPs:   v6MasqIPs,
			Subnets:     nInfo.Subnets(),
			NodeSubnets: nodeSubnets,
		}
		netConfig.Advertised.Store(util.IsPodNetworkAdvertisedAtNode(nInfo, bridge.nodeName))

		bridge.netConfig[netName] = netConfig
	} else {
		klog.Warningf("Trying to update bridge config for network %s which already"+
			"exists in cache...networks are not mutable...ignoring update", nInfo.GetNetworkName())
	}
	return nil
}

// DelNetworkBridgeConfig deletes the provided netInfo from the bridge configuration cache
// used by openflow_manager on delNetwork
func (bridge *BridgeConfiguration) DelNetworkBridgeConfig(nInfo util.NetInfo) {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()

	delete(bridge.netConfig, nInfo.GetNetworkName())
}

func (bridge *BridgeConfiguration) GetNetworkBridgeConfig(networkName string) *BridgeUDNConfiguration {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	return bridge.netConfig[networkName]
}

// GetActiveNetworkBridgeConfigCopy returns a shallow copy of the network configuration corresponding to the
// provided netInfo.
//
// NOTE: if the network configuration can't be found or if the network is not patched by OVN
// yet this returns nil.
func (bridge *BridgeConfiguration) GetActiveNetworkBridgeConfigCopy(networkName string) *BridgeUDNConfiguration {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()

	if netConfig, found := bridge.netConfig[networkName]; found && netConfig.OfPortPatch != "" {
		return netConfig.shallowCopy()
	}
	return nil
}

func (bridge *BridgeConfiguration) GetBridgeName() string {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	return bridge.BridgeName
}

func (bridge *BridgeConfiguration) GetBridgeMAC() net.HardwareAddr {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	return bridge.macAddress
}

func (bridge *BridgeConfiguration) SetBridgeMAC(macAddr net.HardwareAddr) {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	bridge.macAddress = macAddr
}

func (bridge *BridgeConfiguration) GetBridgeIPs() []*net.IPNet {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	return bridge.ips
}

func (bridge *BridgeConfiguration) PatchedNetConfigs() []*BridgeUDNConfiguration {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	return bridge.patchedNetConfigs()
}

func (bridge *BridgeConfiguration) patchedNetConfigs() []*BridgeUDNConfiguration {
	result := make([]*BridgeUDNConfiguration, 0, len(bridge.netConfig))
	for _, netConfig := range bridge.netConfig {
		if netConfig.OfPortPatch == "" {
			continue
		}
		result = append(result, netConfig)
	}
	return result
}

func (bridge *BridgeConfiguration) GetEIPMarkIPs() *egressipgw.MarkIPsCache {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	return bridge.eipMarkIPs
}

func (bridge *BridgeConfiguration) SetEIPMarkIPs(cache *egressipgw.MarkIPsCache) {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	bridge.eipMarkIPs = cache
}

// END UDN UTILs for BridgeConfiguration

// used by gateway_udn on AddNetwork
func (bridge *BridgeConfiguration) SetBridgeNetworkOfPorts(netName string) error {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()

	netConfig, found := bridge.netConfig[netName]
	if !found {
		return fmt.Errorf("failed to find network %s configuration on bridge %s", netName, bridge.BridgeName)
	}
	return netConfig.setBridgeNetworkOfPortsInternal()
}

// getMaxFrameLength returns the maximum frame size (ignoring VLAN header) that a gateway can handle
func getMaxFrameLength() int {
	return config.Default.MTU + 14
}

// generateIPFragmentReassemblyFlow adds flows in table 0 that send packets to a
// specific conntrack zone for reassembly with the same priority as node port
// flows that match on L4 fields. After reassembly packets are reinjected to
// table 0 again. This requires a conntrack immplementation that reassembles
// fragments. This reqreuiment is met for the kernel datapath with the netfilter
// module loaded. This reqreuiment is not met for the userspace datapath.
func generateIPFragmentReassemblyFlow(ofPortPhys string) []string {
	flows := make([]string, 0, 2)
	if config.IPv4Mode {
		flows = append(flows,
			fmt.Sprintf("cookie=%s, priority=110, table=0, in_port=%s, ip, nw_frag=yes, actions=ct(table=0,zone=%d)",
				DefaultOpenFlowCookie,
				ofPortPhys,
				config.Default.ReassemblyConntrackZone,
			),
		)
	}
	if config.IPv6Mode {
		flows = append(flows,
			fmt.Sprintf("cookie=%s, priority=110, table=0, in_port=%s, ipv6, nw_frag=yes, actions=ct(table=0,zone=%d)",
				DefaultOpenFlowCookie,
				ofPortPhys,
				config.Default.ReassemblyConntrackZone,
			),
		)
	}

	return flows
}

// UpdateInterfaceIPAddresses sets and returns the bridge's current ips
func (bridge *BridgeConfiguration) UpdateInterfaceIPAddresses(node *corev1.Node) ([]*net.IPNet, error) {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	ifAddrs, err := nodeutil.GetNetworkInterfaceIPAddresses(bridge.GetGatewayIface())
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

	bridge.ips = ifAddrs
	return ifAddrs, nil
}

// used by gateway on newGateway readyFunc
func (bridge *BridgeConfiguration) WaitGatewayReady() error {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	for _, netConfig := range bridge.netConfig {
		ready := gatewayReady(netConfig.PatchPort)
		if !ready {
			return fmt.Errorf("gateway patch port %s is not ready yet", netConfig.PatchPort)
		}
	}
	return nil
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

// BridgeUDNConfiguration holds the patchport and ctMark
// information for a given network
type BridgeUDNConfiguration struct {
	PatchPort   string
	OfPortPatch string
	masqCTMark  string
	PktMark     string
	v4MasqIPs   *udn.MasqueradeIPs
	v6MasqIPs   *udn.MasqueradeIPs
	Subnets     []config.CIDRNetworkEntry
	NodeSubnets []*net.IPNet
	Advertised  atomic.Bool
}

func (netConfig *BridgeUDNConfiguration) shallowCopy() *BridgeUDNConfiguration {
	copy := &BridgeUDNConfiguration{
		PatchPort:   netConfig.PatchPort,
		OfPortPatch: netConfig.OfPortPatch,
		masqCTMark:  netConfig.masqCTMark,
		PktMark:     netConfig.PktMark,
		v4MasqIPs:   netConfig.v4MasqIPs,
		v6MasqIPs:   netConfig.v6MasqIPs,
		Subnets:     netConfig.Subnets,
		NodeSubnets: netConfig.NodeSubnets,
	}
	netConfig.Advertised.Store(netConfig.Advertised.Load())
	return copy
}

func (netConfig *BridgeUDNConfiguration) IsDefaultNetwork() bool {
	return netConfig.masqCTMark == ctMarkOVN
}

func (netConfig *BridgeUDNConfiguration) setBridgeNetworkOfPortsInternal() error {
	ofportPatch, stderr, err := util.GetOVSOfPort("get", "Interface", netConfig.PatchPort, "ofport")
	if err != nil {
		return fmt.Errorf("failed while waiting on patch port %q to be created by ovn-controller and "+
			"while getting ofport. stderr: %v, error: %v", netConfig.PatchPort, stderr, err)
	}
	netConfig.OfPortPatch = ofportPatch
	return nil
}
