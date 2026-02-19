package node

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/udn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	nodetypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type openflowManager struct {
	defaultBridge         *bridgeconfig.BridgeConfiguration
	externalGatewayBridge *bridgeconfig.BridgeConfiguration
	// flow cache, use map instead of array for readability when debugging
	flowCache     map[string][]string
	flowMutex     sync.Mutex
	exGWFlowCache map[string][]string
	exGWFlowMutex sync.Mutex
	// channel to indicate we need to update flows immediately
	flowChan chan struct{}
}

// UTILs Needed for UDN (also leveraged for default netInfo) in openflowmanager

func (c *openflowManager) getDefaultBridgePortConfigurations() ([]*bridgeconfig.BridgeUDNConfiguration, string, string) {
	return c.defaultBridge.GetPortConfigurations()
}

func (c *openflowManager) getExGwBridgePortConfigurations() ([]*bridgeconfig.BridgeUDNConfiguration, string, string) {
	return c.externalGatewayBridge.GetPortConfigurations()
}

func (c *openflowManager) addNetwork(nInfo util.NetInfo, nodeSubnets, mgmtIPs []*net.IPNet, masqCTMark, pktMark uint, v6MasqIPs, v4MasqIPs *udn.MasqueradeIPs) error {
	if err := c.defaultBridge.AddNetworkConfig(nInfo, nodeSubnets, mgmtIPs, masqCTMark, pktMark, v6MasqIPs, v4MasqIPs); err != nil {
		return err
	}
	if c.externalGatewayBridge != nil {
		if err := c.externalGatewayBridge.AddNetworkConfig(nInfo, nodeSubnets, mgmtIPs, masqCTMark, pktMark, v6MasqIPs, v4MasqIPs); err != nil {
			return err
		}
	}
	return nil
}

func (c *openflowManager) delNetwork(nInfo util.NetInfo) {
	c.defaultBridge.DelNetworkConfig(nInfo)
	if c.externalGatewayBridge != nil {
		c.externalGatewayBridge.DelNetworkConfig(nInfo)
	}
}

func (c *openflowManager) getActiveNetwork(nInfo util.NetInfo) *bridgeconfig.BridgeUDNConfiguration {
	return c.defaultBridge.GetActiveNetworkBridgeConfigCopy(nInfo.GetNetworkName())
}

// END UDN UTILs

func (c *openflowManager) getDefaultBridgeName() string {
	return c.defaultBridge.GetBridgeName()
}

func (c *openflowManager) getDefaultBridgeMAC() net.HardwareAddr {
	return c.defaultBridge.GetMAC()
}

func (c *openflowManager) setDefaultBridgeMAC(macAddr net.HardwareAddr) {
	c.defaultBridge.SetMAC(macAddr)
}

// setDefaultBridgeGARPDrop is used to enable or disable whether openflow manager generates ovs flows and adds them to
// the default ext bridge to drop GARP
func (c *openflowManager) setDefaultBridgeGARPDrop(isDropped bool) {
	c.defaultBridge.SetDropGARP(isDropped)
}

func (c *openflowManager) updateFlowCacheEntry(key string, flows []string) {
	c.flowMutex.Lock()
	defer c.flowMutex.Unlock()
	c.flowCache[key] = flows
}

func (c *openflowManager) deleteFlowsByKey(key string) {
	c.flowMutex.Lock()
	defer c.flowMutex.Unlock()
	delete(c.flowCache, key)
}

func (c *openflowManager) getFlowsByKey(key string) []string {
	c.flowMutex.Lock()
	defer c.flowMutex.Unlock()
	return c.flowCache[key]
}

func (c *openflowManager) updateExBridgeFlowCacheEntry(key string, flows []string) {
	c.exGWFlowMutex.Lock()
	defer c.exGWFlowMutex.Unlock()
	c.exGWFlowCache[key] = flows
}

func (c *openflowManager) requestFlowSync() {
	select {
	case c.flowChan <- struct{}{}:
		klog.V(5).Infof("Gateway OpenFlow sync requested")
	default:
		klog.V(5).Infof("Gateway OpenFlow sync already requested")
	}
}

func (c *openflowManager) syncFlows() {
	c.flowMutex.Lock()
	flows := flattenFlowCacheEntries(c.flowCache)
	c.flowMutex.Unlock()

	_, stderr, err := util.ReplaceOFFlows(c.defaultBridge.GetBridgeName(), flows)
	if err != nil {
		klog.Errorf("Failed to add flows for bridge %s, error: %v, stderr, %s, flow count: %d",
			c.defaultBridge.GetBridgeName(), err, stderr, len(flows))
	}

	if c.externalGatewayBridge != nil {
		c.exGWFlowMutex.Lock()
		exGWFlows := flattenFlowCacheEntries(c.exGWFlowCache)
		c.exGWFlowMutex.Unlock()

		_, stderr, err := util.ReplaceOFFlows(c.externalGatewayBridge.GetBridgeName(), exGWFlows)
		if err != nil {
			klog.Errorf("Failed to add flows for bridge %s, error: %v, stderr, %s, flow count: %d",
				c.externalGatewayBridge.GetBridgeName(), err, stderr, len(exGWFlows))
		}
	}
}

func flattenFlowCacheEntries(flowCache map[string][]string) []string {
	flowCount := 0
	for _, entry := range flowCache {
		flowCount += len(entry)
	}
	flows := make([]string, 0, flowCount)
	for _, entry := range flowCache {
		flows = append(flows, entry...)
	}
	return flows
}

// since we share the host's k8s node IP, add OpenFlow flows
// -- to steer the NodePort traffic arriving on the host to the OVN logical topology and
// -- to also connection track the outbound north-south traffic through l3 gateway so that
//
//	the return traffic can be steered back to OVN logical topology
//
// -- to handle host -> service access, via masquerading from the host to OVN GR
// -- to handle external -> service(ExternalTrafficPolicy: Local) -> host access without SNAT
func newGatewayOpenFlowManager(gwBridge, exGWBridge *bridgeconfig.BridgeConfiguration) (*openflowManager, error) {
	// add health check function to check default OpenFlow flows are on the shared gateway bridge
	ofm := &openflowManager{
		defaultBridge:         gwBridge,
		externalGatewayBridge: exGWBridge,
		flowCache:             make(map[string][]string),
		flowMutex:             sync.Mutex{},
		exGWFlowCache:         make(map[string][]string),
		exGWFlowMutex:         sync.Mutex{},
		flowChan:              make(chan struct{}, 1),
	}

	// defer flowSync until syncService() to prevent the existing service OpenFlows being deleted
	return ofm, nil
}

// Run starts OpenFlow Manager which will constantly sync flows for managed OVS bridges
func (c *openflowManager) Run(stopChan <-chan struct{}, doneWg *sync.WaitGroup) {
	doneWg.Add(1)
	go func() {
		defer doneWg.Done()
		syncPeriod := 15 * time.Second
		timer := time.NewTicker(syncPeriod)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:

				if err := checkPorts(c.getDefaultBridgePortConfigurations()); err != nil {
					klog.Errorf("Checkports failed %v", err)
					continue
				}

				if c.externalGatewayBridge != nil {
					if err := checkPorts(c.getExGwBridgePortConfigurations()); err != nil {
						klog.Errorf("Checkports failed %v", err)
						continue
					}
				}
				c.syncFlows()
			case <-c.flowChan:
				c.syncFlows()
				timer.Reset(syncPeriod)
			case <-stopChan:
				// sync before shutting down because flows maybe added, and theres a race between flow channel (req sync)
				// and stop chan on shutdown. ensure flows are sync before shut down
				c.syncFlows()
				return
			}
		}
	}()
}

func (c *openflowManager) updateBridgePMTUDFlowCache(key string, ipAddrs []string) {
	dftFlows := c.defaultBridge.PMTUDDropFlows(ipAddrs)
	c.updateFlowCacheEntry(key, dftFlows)
	if c.externalGatewayBridge != nil {
		exGWBridgeDftFlows := c.externalGatewayBridge.PMTUDDropFlows(ipAddrs)
		c.updateExBridgeFlowCacheEntry(key, exGWBridgeDftFlows)
	}
}

// updateBridgeFlowCache generates the "static" per-bridge flows
// note: this is shared between shared and local gateway modes
func (c *openflowManager) updateBridgeFlowCache(hostIPs []net.IP, hostSubnets []*net.IPNet) error {
	// CAUTION: when adding new flows where the in_port is ofPortPatch and the out_port is ofPortPhys, ensure
	// that dl_src is included in match criteria!

	dftFlows, err := c.defaultBridge.DefaultBridgeFlows(hostSubnets, hostIPs)
	if err != nil {
		return err
	}

	c.updateFlowCacheEntry("NORMAL", []string{fmt.Sprintf("table=0,priority=0,actions=%s\n", util.NormalAction)})
	c.updateFlowCacheEntry("DEFAULT", dftFlows)

	// we consume ex gw bridge flows only if that is enabled
	if c.externalGatewayBridge != nil {
		exGWBridgeDftFlows, err := c.externalGatewayBridge.ExternalBridgeFlows(hostSubnets)
		if err != nil {
			return err
		}

		c.updateExBridgeFlowCacheEntry("NORMAL", []string{fmt.Sprintf("table=0,priority=0,actions=%s\n", util.NormalAction)})
		c.updateExBridgeFlowCacheEntry("DEFAULT", exGWBridgeDftFlows)
	}
	return nil
}

func checkPorts(netConfigs []*bridgeconfig.BridgeUDNConfiguration, physIntf, ofPortPhys string) error {
	// it could be that the ovn-controller recreated the patch between the host OVS bridge and
	// the integration bridge, as a result the ofport number changed for that patch interface
	for _, netConfig := range netConfigs {
		if netConfig.OfPortPatch == "" {
			continue
		}
		curOfportPatch, stderr, err := util.GetOVSOfPort("--if-exists", "get", "Interface", netConfig.PatchPort, "ofport")
		if err != nil {
			return fmt.Errorf("failed to get ofport of %s, stderr: %q: %w", netConfig.PatchPort, stderr, err)

		}
		if netConfig.OfPortPatch != curOfportPatch {
			if netConfig.IsDefaultNetwork() {
				klog.Errorf("Fatal error: patch port %s ofport changed from %s to %s",
					netConfig.PatchPort, netConfig.OfPortPatch, curOfportPatch)
				os.Exit(1)
			} else {
				klog.Warningf("UDN patch port %s changed for existing network from %v to %v. Expecting bridge config update.", netConfig.PatchPort, netConfig.OfPortPatch, curOfportPatch)
			}
		}
	}

	// it could be that someone removed the physical interface and added it back on the OVS host
	// bridge, as a result the ofport number changed for that physical interface
	curOfportPhys, stderr, err := util.GetOVSOfPort("--if-exists", "get", "interface", physIntf, "ofport")
	if err != nil {
		return fmt.Errorf("failed to get ofport of %s, stderr: %q: %w", physIntf, stderr, err)
	}
	if ofPortPhys != curOfportPhys {
		klog.Errorf("Fatal error: phys port %s ofport changed from %s to %s",
			physIntf, ofPortPhys, curOfportPhys)
		os.Exit(1)
	}
	return nil
}

// bootstrapOVSFlows handles ensuring basic, required flows are in place. This is done before OpenFlow manager has
// been created/started, and only done when there is just a NORMAL flow programmed and OVN/OVS is already setup
func bootstrapOVSFlows(nodeName string) error {
	// see if patch port exists already
	var portsOutput string
	var stderr string
	var err error
	if portsOutput, stderr, err = util.RunOVSVsctl("--no-heading", "--data=bare", "--format=csv", "--columns",
		"name", "list", "interface"); err != nil {
		// bridge exists, but could not list ports
		return fmt.Errorf("failed to list ports on existing bridge br-int: %s, %w", stderr, err)
	}

	bridge, patchPort := localnetPortInfo(nodeName, portsOutput)

	if len(bridge) == 0 {
		// bridge exists but no patch port was found
		return nil
	}

	// get the current flows and if there is more than just default flow, we dont need to bootstrap as we already
	// have flows
	flows, err := util.GetOFFlows(bridge)
	if err != nil {
		return err
	}
	if len(flows) > 1 {
		// more than 1 flow, assume the OVS has retained previous flows from previous running OVNK instance
		return nil
	}

	// only have 1 flow, need to install required flows
	klog.Infof("Default NORMAL flow installed on OVS bridge: %s, will bootstrap with required port security flows", bridge)

	// Get ofport of patchPort
	ofportPatch, stderr, err := util.GetOVSOfPort("get", "Interface", patchPort, "ofport")
	if err != nil {
		return fmt.Errorf("failed while waiting on patch port %q to be created by ovn-controller and "+
			"while getting ofport. stderr: %q, error: %v", patchPort, stderr, err)
	}

	var bridgeMACAddress net.HardwareAddr
	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		hostRep, err := util.GetDPUHostInterface(bridge)
		if err != nil {
			return err
		}
		bridgeMACAddress, err = util.GetSriovnetOps().GetRepresentorPeerMacAddress(hostRep)
		if err != nil {
			return err
		}
	} else {
		bridgeMACAddress, err = util.GetOVSPortMACAddress(bridge)
		if err != nil {
			return fmt.Errorf("failed to get MAC address for ovs port %s: %w", bridge, err)
		}
	}

	var dftFlows []string
	// table 0, check packets coming from OVN have the correct mac address. Low priority flows that are a catch all
	// for non-IP packets that would normally be forwarded with NORMAL action (table 0, priority 0 flow).
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, priority=10, table=0, in_port=%s, dl_src=%s, actions=output:NORMAL",
			nodetypes.DefaultOpenFlowCookie, ofportPatch, bridgeMACAddress))
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, priority=9, table=0, in_port=%s, actions=drop",
			nodetypes.DefaultOpenFlowCookie, ofportPatch))
	dftFlows = append(dftFlows, "priority=0, table=0, actions=output:NORMAL")

	_, stderr, err = util.ReplaceOFFlows(bridge, dftFlows)
	if err != nil {
		return fmt.Errorf("failed to add flows, error: %v, stderr, %s, flows: %s", err, stderr, dftFlows)
	}

	return nil
}

// localnetPortInfo returns the name of the bridge and the patch port name for the default cluster network
func localnetPortInfo(nodeName string, portsOutput string) (string, string) {
	// This needs to work with:
	// - default network: patch-<bridge name>_<node>-to-br-int
	// but not with:
	// - user defined primary network: patch-<bridge name>_<network-name>_<node>-to-br-int
	// - user defined secondary localnet network: patch-<bridge name>_<network-name>_ovn_localnet_port-to-br-int
	// TODO: going forward, maybe it would preferable to just read the bridge name from the config.
	r := regexp.MustCompile(fmt.Sprintf("^patch-([^_]*)_%s-to-br-int$", nodeName))
	for _, line := range strings.Split(portsOutput, "\n") {
		matches := r.FindStringSubmatch(line)
		if len(matches) == 2 {
			return matches[1], matches[0]
		}
	}
	return "", ""
}
