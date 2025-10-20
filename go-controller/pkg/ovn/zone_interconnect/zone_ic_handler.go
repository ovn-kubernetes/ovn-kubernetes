package zoneinterconnect

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	ipgenerator "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/ip"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/udn"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	lportTypeRouter     = "router"
	lportTypeRouterAddr = "router"
	lportTypeRemote     = "remote"

	BaseTransitSwitchTunnelKey = 16711683
)

/*
 * ZoneInterconnectHandler manages OVN resources required for interconnecting
 * multiple zones. This handler exposes functions which a network controller
 * (default and UDN) is expected to call on different events.

 * For routed topologies:
 *
 * AddLocalZoneNode(node) should be called if the node 'node' is a local zone node.
 * AddRemoteZoneNode(node) should be called if the node 'node' is a remote zone node.
 * Zone Interconnect Handler first creates a transit switch with the name - <network_name>+ "_" + types.TransitSwitch
 * if it is still not present.
 *
 * Local zone node handling
 * ------------------------
 * When network controller calls AddLocalZoneNode(ovn-worker)
 *    -  A logical switch port - router port pair is created connecting the ovn_cluster_router
 *       to the transit switch.
 *    -  Node annotation - k8s.ovn.org/ovn-node-transit-switch-port-ifaddr value is used
 *       as the logical router port address
 *
 * When network controller calls AddRemoteZoneNode(ovn-worker3)
 *    - A logical switch port of type "remote" is created in OVN Northbound transit_switch
 *      for the node ovn-worker3
 *    - A static route {IPPrefix: "ovn-worker3_subnet", Nexthop: "ovn-worker3_transit_port_ip"} is
 *      added in the ovn_cluster_router.
 *    - For the default network, additional static route
 *      {IPPrefix: "ovn-worker3_gw_router_port_host_ip", Nexthop: "ovn-worker3_transit_port_ip"} is
 *      added in the ovn_cluster_router
 *    - The corresponding port binding row in OVN Southbound DB for this logical port
 *      is manually bound to the remote OVN Southbound DB Chassis "ovn-worker3"
 *
 * -----------------------------------------------------------------------------------------------------
 * $ ovn-nbctl show ovn_cluster_router (on ovn-worker zone DB)
 *   router ovn_cluster_router
 *   ...
 *   port rtots-ovn-worker
 *      mac: "0a:58:a8:fe:00:08"
 *      networks: ["100.88.0.8/16", "fd97::8/64"]
 *
 * $ ovn-nbctl show transit_switch
 *     port tstor-ovn-worker
 *        type: router
 *        router-port: rtots-ovn-worker
 *     port tstor-ovn-worker3
 *        type: remote
 *        addresses: ["0a:58:a8:fe:00:02 100.88.0.2/16 fd97::2/64"]
 *
 * $ ovn-nbctl lr-route-list ovn_cluster_router
 *    IPv4 Routes
 *    Route Table <main>:
 *    ...
 *    ...
 *    10.244.0.0/24 (ovn-worker3 subnet)            100.88.0.2 (ovn-worker3 transit switch port ip) dst-ip
 *    100.64.0.2/32 (ovn-worker3 gw router port ip) 100.88.0.2 dst-ip
 *    ...
 *    IPv6 Routes
 *    Route Table <main>:
 *    ...
 *    ...
 *    fd00:10:244:1::/64 (ovn-worker3 subnet)       fd97::2 (ovn-worker3 transit switch port ip) dst-ip
 *    fd98::2 (ovn-worker3 gw router port ip)       fd97::2 dst-ip
 *    ...
 *
 * $ ovn-sbctl show
 *     ...
 *     Chassis "c391c626-e1f0-4b1e-af0b-66f0807f9495"
 *     hostname: ovn-worker3 (Its a remote chassis entry on which tstor-ovn-worker3 is bound)
 *     Encap geneve
 *         ip: "10.89.0.26"
 *         options: {csum="true"}
 *     Port_Binding tstor-ovn-worker3
 *
 * -----------------------------------------------------------------------------------------------------
 *
 *
 * For single switch flat topologies that require transit accross nodes:
 *
 * AddTransitSwitchConfig will add to the switch the specific transit config
 * AddTransitPortConfig will add to the local or remote port the specific transit config
 * BindTransitRemotePort will bind the remote port to the remote chassis
 *
 *
 * Note that the Chassis entry for each remote zone node is created by ZoneChassisHandler
 *
 */

// ZoneInterconnectHandler creates the OVN resources required for interconnecting
// multiple zones for a network (default or layer 3) UDN
type ZoneInterconnectHandler struct {
	watchFactory *factory.WatchFactory
	// network which is inter-connected
	util.NetInfo
	nbClient libovsdbclient.Client
	sbClient libovsdbclient.Client
	// ovn_cluster_router name for the network
	networkClusterRouterName string
	// transit switch name for the network
	networkTransitSwitchName string
	// Transit switch IP generators for deriving transit switch port IPs from tunnel IDs
	transitSwitchIPv4Generator *ipgenerator.IPGenerator
	transitSwitchIPv6Generator *ipgenerator.IPGenerator
}

// NewZoneInterconnectHandler returns a new ZoneInterconnectHandler object
func NewZoneInterconnectHandler(nInfo util.NetInfo, nbClient, sbClient libovsdbclient.Client, watchFactory *factory.WatchFactory) (*ZoneInterconnectHandler, error) {
	zic := &ZoneInterconnectHandler{
		NetInfo:      nInfo,
		nbClient:     nbClient,
		sbClient:     sbClient,
		watchFactory: watchFactory,
	}

	zic.networkClusterRouterName = zic.GetNetworkScopedName(types.OVNClusterRouter)
	zic.networkTransitSwitchName = getTransitSwitchName(nInfo)

	// Initialize IP generators for deriving transit switch port IPs from tunnel IDs
	var err error
	if config.IPv4Mode {
		zic.transitSwitchIPv4Generator, err = ipgenerator.NewIPGenerator(config.ClusterManager.V4TransitSubnet)
		if err != nil {
			return nil, fmt.Errorf("error creating IP Generator for v4 transit subnet %s: %w", config.ClusterManager.V4TransitSubnet, err)
		}
	}
	if config.IPv6Mode {
		zic.transitSwitchIPv6Generator, err = ipgenerator.NewIPGenerator(config.ClusterManager.V6TransitSubnet)
		if err != nil {
			return nil, fmt.Errorf("error creating IP Generator for v6 transit subnet %s: %w", config.ClusterManager.V6TransitSubnet, err)
		}
	}

	return zic, nil
}

func getTransitSwitchName(nInfo util.NetInfo) string {
	switch nInfo.TopologyType() {
	case types.Layer2Topology:
		return nInfo.GetNetworkScopedName(types.OVNLayer2Switch)
	default:
		return nInfo.GetNetworkScopedName(types.TransitSwitch)
	}
}

func (zic *ZoneInterconnectHandler) getTransitSwitchToRouterPortName(nodeName string, index int) string {
	baseName := zic.GetNetworkScopedName(types.TransitSwitchToRouterPrefix + nodeName)
	if index == 0 {
		return baseName
	}

	return fmt.Sprintf("%s_encap%d", baseName, index)
}

func (zic *ZoneInterconnectHandler) getRouterToTransitSwitchPortName(nodeName string, index int) string {
	baseName := zic.GetNetworkScopedName(types.RouterToTransitSwitchPrefix + nodeName)
	if index == 0 {
		return baseName
	}

	return fmt.Sprintf("%s_encap%d", baseName, index)
}

// deriveTransitSwitchPortIPs derives the transit switch port IP addresses from a tunnel ID
// using the configured transit switch subnet. For example, with subnet 100.88.0.0/16 and
// tunnel ID 7, this returns 100.88.0.7/16.
func (zic *ZoneInterconnectHandler) deriveTransitSwitchPortIPs(tunnelID int) ([]*net.IPNet, error) {
	var ips []*net.IPNet
	var err error

	if config.IPv4Mode && zic.transitSwitchIPv4Generator != nil {
		var ip *net.IPNet
		ip, err = zic.transitSwitchIPv4Generator.GenerateIP(tunnelID)
		if err != nil {
			return nil, fmt.Errorf("failed to generate transit switch port IPv4 address for tunnel ID %d: %w", tunnelID, err)
		}
		ips = append(ips, ip)
	}

	if config.IPv6Mode && zic.transitSwitchIPv6Generator != nil {
		var ip *net.IPNet
		ip, err = zic.transitSwitchIPv6Generator.GenerateIP(tunnelID)
		if err != nil {
			return nil, fmt.Errorf("failed to generate transit switch port IPv6 address for tunnel ID %d: %w", tunnelID, err)
		}
		ips = append(ips, ip)
	}

	return ips, nil
}

func (zic *ZoneInterconnectHandler) createOrUpdateTransitSwitch(networkID int) error {
	externalIDs := make(map[string]string)
	if zic.IsUserDefinedNetwork() {
		externalIDs = getUserDefinedNetTransitSwitchExtIDs(zic.GetNetworkName(), zic.TopologyType(), zic.IsPrimaryNetwork())
	}
	ts := &nbdb.LogicalSwitch{
		Name:        zic.networkTransitSwitchName,
		ExternalIDs: externalIDs,
	}
	zic.addTransitSwitchConfig(ts, BaseTransitSwitchTunnelKey+networkID)
	// Create transit switch if it doesn't exist
	if err := libovsdbops.CreateOrUpdateLogicalSwitch(zic.nbClient, ts); err != nil {
		return fmt.Errorf("failed to create/update transit switch %s: %w", zic.networkTransitSwitchName, err)
	}
	return nil
}

// ensureTransitSwitch sets up the global transit switch required for interoperability with other zones
// Must wait for network id to be annotated to any node by cluster manager
func (zic *ZoneInterconnectHandler) ensureTransitSwitch(nodes []*corev1.Node) error {
	if len(nodes) == 0 { // nothing to do
		return nil
	}
	start := time.Now()

	if err := zic.createOrUpdateTransitSwitch(zic.GetNetworkID()); err != nil {
		return err
	}

	klog.Infof("Time taken to create transit switch: %s", time.Since(start))

	return nil
}

// AddLocalZoneNode creates the interconnect resources in OVN NB DB for the local zone node.
// See createLocalZoneNodeResources() below for more details.
func (zic *ZoneInterconnectHandler) AddLocalZoneNode(node *corev1.Node) error {
	klog.Infof("Creating interconnect resources for local zone node %s for the network %s", node.Name, zic.GetNetworkName())
	nodeID, _ := util.GetNodeID(node)
	if nodeID == -1 {
		// Don't consider this node as cluster-manager has not allocated node id yet.
		return fmt.Errorf("failed to get node id for node - %s", node.Name)
	}

	if err := zic.createLocalZoneNodeResources(node, nodeID); err != nil {
		return fmt.Errorf("creating interconnect resources for local zone node %s for the network %s failed : err - %w", node.Name, zic.GetNetworkName(), err)
	}

	return nil
}

// AddRemoteZoneNode creates the interconnect resources in OVN NBDB and SBDB for the remote zone node.
// // See createRemoteZoneNodeResources() below for more details.
func (zic *ZoneInterconnectHandler) AddRemoteZoneNode(node *corev1.Node) error {
	start := time.Now()

	nodeID, _ := util.GetNodeID(node)
	if nodeID == -1 {
		// Don't consider this node as cluster-manager has not allocated node id yet.
		return fmt.Errorf("failed to get node id for node - %s", node.Name)
	}

	nodeSubnets, err := util.ParseNodeHostSubnetAnnotation(node, zic.GetNetworkName())
	if err != nil {
		err = fmt.Errorf("failed to parse node %s subnets annotation %w", node.Name, err)
		if util.IsAnnotationNotSetError(err) {
			// remote node may not have the annotation yet, suppress it
			return types.NewSuppressedError(err)
		}
		return err
	}

	nodeTransitSwitchPortIPs, err := util.ParseNodeTransitSwitchPortAddrs(node)
	if err != nil || len(nodeTransitSwitchPortIPs) == 0 {
		err = fmt.Errorf("failed to get the node transit switch port IP addresses : %w", err)
		if util.IsAnnotationNotSetError(err) {
			return types.NewSuppressedError(err)
		}
		return err
	}

	var nodeGRPIPs []*net.IPNet
	// only primary networks have cluster router connected to join switch+GR
	// used for adding routes to GR
	if !zic.IsUserDefinedNetwork() || (util.IsNetworkSegmentationSupportEnabled() && zic.IsPrimaryNetwork()) {
		nodeGRPIPs, err = udn.GetGWRouterIPs(node, zic.GetNetInfo())
		if err != nil {
			if util.IsAnnotationNotSetError(err) {
				// FIXME(tssurya): This is present for backwards compatibility
				// Remove me a few months from now
				var err1 error
				nodeGRPIPs, err1 = util.ParseNodeGatewayRouterLRPAddrs(node)
				if err1 != nil {
					err1 = fmt.Errorf("failed to parse node %s Gateway router LRP Addrs annotation %w", node.Name, err1)
					if util.IsAnnotationNotSetError(err1) {
						return types.NewSuppressedError(err1)
					}
					return err1
				}
			}
		}
	}

	klog.Infof("Creating interconnect resources for remote zone node %s for the network %s", node.Name, zic.GetNetworkName())

	if err := zic.createRemoteZoneNodeResources(node, nodeID, nodeTransitSwitchPortIPs, nodeSubnets, nodeGRPIPs); err != nil {
		return fmt.Errorf("creating interconnect resources for remote zone node %s for the network %s failed : err - %w", node.Name, zic.GetNetworkName(), err)
	}
	klog.Infof("Creating Interconnect resources for node %q on network %q took: %s", node.Name, zic.GetNetworkName(), time.Since(start))
	return nil
}

// DeleteNode deletes the local zone node or remote zone node resources
func (zic *ZoneInterconnectHandler) DeleteNode(node *corev1.Node) error {
	klog.Infof("Deleting interconnect resources for the node %s for the network %s", node.Name, zic.GetNetworkName())

	return zic.cleanupNode(node.Name)
}

// SyncNodes ensures a transit switch exists and cleans up the interconnect
// resources present in the OVN Northbound db for the stale nodes
func (zic *ZoneInterconnectHandler) SyncNodes(objs []interface{}) error {
	foundNodeNames := sets.New[string]()
	foundNodes := make([]*corev1.Node, len(objs))
	for i, obj := range objs {
		node, ok := obj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("spurious object in syncNodes: %v", obj)
		}
		foundNodeNames.Insert(node.Name)
		foundNodes[i] = node
	}

	// Get the transit switch. If its not present no cleanup to do
	ts := &nbdb.LogicalSwitch{
		Name: zic.networkTransitSwitchName,
	}

	ts, err := libovsdbops.GetLogicalSwitch(zic.nbClient, ts)
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			// This can happen for the first time when interconnect is enabled.
			// Let's ensure the transit switch exists
			return zic.ensureTransitSwitch(foundNodes)
		}

		return err
	}

	staleNodeNames := []string{}
	for _, p := range ts.Ports {
		lp := &nbdb.LogicalSwitchPort{
			UUID: p,
		}

		lp, err = libovsdbops.GetLogicalSwitchPort(zic.nbClient, lp)
		if err != nil {
			continue
		}

		if lp.ExternalIDs == nil {
			continue
		}

		lportNode := lp.ExternalIDs["node"]
		if !foundNodeNames.Has(lportNode) {
			staleNodeNames = append(staleNodeNames, lportNode)
		}
	}

	for _, staleNodeName := range staleNodeNames {
		if err := zic.cleanupNode(staleNodeName); err != nil {
			klog.Errorf("Failed to cleanup the interconnect resources from OVN Northbound db for the stale node %s: %v", staleNodeName, err)
		}
	}

	return nil
}

// Cleanup deletes the transit switch for the network
func (zic *ZoneInterconnectHandler) Cleanup() error {
	klog.Infof("Deleting the transit switch %s for the network %s", zic.networkTransitSwitchName, zic.GetNetworkName())
	return libovsdbops.DeleteLogicalSwitch(zic.nbClient, zic.networkTransitSwitchName)
}

// AddTransitSwitchConfig is only used by the layer2 network controller
func (zic *ZoneInterconnectHandler) AddTransitSwitchConfig(sw *nbdb.LogicalSwitch, tunnelKey int) error {
	if zic.TopologyType() != types.Layer2Topology {
		return nil
	}

	zic.addTransitSwitchConfig(sw, tunnelKey)
	return nil
}

func (zic *ZoneInterconnectHandler) AddTransitPortConfig(remote bool, podAnnotation *util.PodAnnotation, port *nbdb.LogicalSwitchPort) error {
	if zic.TopologyType() != types.Layer2Topology {
		return nil
	}

	// make sure we have a good ID
	if podAnnotation.TunnelID == 0 {
		return fmt.Errorf("invalid id %d for port %s", podAnnotation.TunnelID, port.Name)
	}

	if port.Options == nil {
		port.Options = map[string]string{}
	}
	port.Options[libovsdbops.RequestedTnlKey] = strconv.Itoa(podAnnotation.TunnelID)

	if remote {
		port.Type = lportTypeRemote
	}

	return nil
}

func (zic *ZoneInterconnectHandler) addTransitSwitchConfig(sw *nbdb.LogicalSwitch, tunnelKey int) {
	if sw.OtherConfig == nil {
		sw.OtherConfig = map[string]string{}
	}

	sw.OtherConfig["interconn-ts"] = sw.Name
	sw.OtherConfig[libovsdbops.RequestedTnlKey] = strconv.Itoa(tunnelKey)
	sw.OtherConfig["mcast_snoop"] = "true"
	sw.OtherConfig["mcast_querier"] = "false"
	sw.OtherConfig["mcast_flood_unregistered"] = "true"
}

// createLocalNodeTransitSwitchPort creates a transit switch port for a specific tunnel ID
// and connects it to the cluster router. This is called:
// - For single-VTEP nodes: once during node initialization
// - For multi-VTEP nodes: on-demand when the first pod using a specific encap IP is scheduled
func (zic *ZoneInterconnectHandler) createLocalNodeTransitSwitchPort(node *corev1.Node, tunnelID int, encapIp string, encapIndex int) error {
	// Derive transit switch port IPs from tunnel ID
	nodeTransitSwitchPortIPs, err := zic.deriveTransitSwitchPortIPs(tunnelID)
	if err != nil {
		return fmt.Errorf("failed to derive transit switch port IPs for node %s tunnel ID %d: %w", node.Name, tunnelID, err)
	}
	if len(nodeTransitSwitchPortIPs) == 0 {
		return fmt.Errorf("no transit switch port IPs derived for node %s tunnel ID %d", node.Name, tunnelID)
	}

	// Generate MAC address from first IP
	transitRouterPortMac := util.IPAddrToHWAddr(nodeTransitSwitchPortIPs[0].IP)
	var transitRouterPortNetworks []string
	for _, ip := range nodeTransitSwitchPortIPs {
		transitRouterPortNetworks = append(transitRouterPortNetworks, ip.String())
	}

	// Create logical router port name with appropriate suffix for multi-VTEP
	logicalRouterPortName := zic.getRouterToTransitSwitchPortName(node.Name, encapIndex)
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name:     logicalRouterPortName,
		MAC:      transitRouterPortMac.String(),
		Networks: transitRouterPortNetworks,
		Options: map[string]string{
			"mcast_flood": "true",
		},
		ExternalIDs: map[string]string{
			types.NetworkExternalID: zic.GetNetworkName(),
			types.NodeExternalID:    node.Name,
		},
	}
	logicalRouter := nbdb.LogicalRouter{
		Name: zic.networkClusterRouterName,
	}

	if err := libovsdbops.CreateOrUpdateLogicalRouterPort(zic.nbClient, &logicalRouter, &logicalRouterPort, nil); err != nil {
		return fmt.Errorf("failed to create/update cluster router %s to add transit switch port %s for the node %s: %w", zic.networkClusterRouterName, logicalRouterPortName, node.Name, err)
	}

	lspOptions := map[string]string{
		libovsdbops.RouterPort:      logicalRouterPortName,
		libovsdbops.RequestedTnlKey: strconv.Itoa(tunnelID),
	}

	// Store the node name and network name in the external_ids column for book keeping
	externalIDs := map[string]string{
		types.NetworkExternalID: zic.GetNetworkName(),
		"node":                  node.Name,
	}

	// Create transit switch port name with appropriate suffix
	transitSwitchPortName := zic.getTransitSwitchToRouterPortName(node.Name, encapIndex)
	err = zic.addNodeLogicalSwitchPort(zic.networkTransitSwitchName, transitSwitchPortName,
		lportTypeRouter, []string{lportTypeRouterAddr}, lspOptions, externalIDs)
	if err != nil {
		return err
	}

	if encapIp != "" {
		klog.V(5).Infof("Updating port binding for local LSP %s with encap ip %s", transitSwitchPortName, encapIp)
		chassisID, err := util.ParseNodeChassisIDAnnotation(node)
		if err != nil || chassisID == "" {
			return fmt.Errorf("failed to parse chassis ID for node %s: %w", node.Name, err)
		}
		if err = libovsdbops.UpdatePortBindingSetEncap(zic.sbClient, transitSwitchPortName, chassisID, encapIp); err != nil {
			return fmt.Errorf("failed to update port binding for %s: %w", transitSwitchPortName, err)
		}
	}

	// Its possible that node is moved from a remote zone to the local zone. Check and delete the remote zone routes
	// for this node as it's no longer needed.
	return zic.deleteLocalNodeStaticRoutes(node, nodeTransitSwitchPortIPs)
}

// EnsureLocalNodeTransitSwitchPortForPod ensures that a transit switch port exists for the
// pod's encap IP on a multi-VTEP local node. This is called when adding a pod on a multi-VTEP node
// to create the transit switch port on-demand.
//
// For single-VTEP nodes, this function does nothing as the port is already created during node initialization.
func (zic *ZoneInterconnectHandler) EnsureLocalNodeTransitSwitchPortForPod(pod *corev1.Pod, podAnnotation *util.PodAnnotation) error {

	if !util.IsMultiVTEPEnabled() {
		return nil
	}

	node, err := zic.watchFactory.GetNode(pod.Spec.NodeName)
	if err != nil {
		return fmt.Errorf("failed to get node %s for pod %s/%s: %w", pod.Spec.NodeName, pod.Namespace, pod.Name, err)
	}

	// If annotation doesn't exist, treat as single-VTEP node
	encapIPs, err := util.ParseNodeEncapIPsAnnotation(node)
	if err != nil && !util.IsAnnotationNotSetError(err) {
		return err
	}

	if len(encapIPs) <= 1 {
		// Single-VTEP node or multi-VTEP not enabled, port already created during node initialization
		return nil
	}

	nodeID, err := util.GetNodeID(node)
	if err != nil || nodeID == util.InvalidNodeID {
		return fmt.Errorf("failed to get node ID for node %s: %w", node.Name, err)
	}

	// Get tunnel IDs and encap IPs for the node
	tunnelIds, err := util.GetNodeTransitSwitchPortTunnelIDs(node)
	if err != nil {
		return fmt.Errorf("failed to get tunnel IDs for node %s: %w", node.Name, err)
	}

	if len(tunnelIds) != len(encapIPs) {
		return fmt.Errorf("number of encap IPs (%d) and transit switch port tunnel IDs (%d) for node %s do not match",
			len(encapIPs), len(tunnelIds), node.Name)
	}

	// Multi-VTEP node: find which tunnel ID corresponds to the pod's encap IP
	encapIndex := -1
	tunnelID := -1
	encapIP := podAnnotation.EncapIP
	if encapIP == "" {
		// If a network is configured to use SR-IOV VF interfaces, all pods on the network will have `encap_ip` set.
		// Otherwise, the Pod will use veth interface and don't have `encap_ip` specified in `k8s.ovn.org/pod-networks`.
		// In this case, the first VTEP on local node will be used to create the local transit switch port.
		encapIP = encapIPs[0]
		encapIndex = 0
		tunnelID = tunnelIds[0]
	} else {
		for i, ip := range encapIPs {
			if ip == encapIP {
				encapIndex = i
				tunnelID = tunnelIds[i]
				break
			}
		}
	}

	if encapIndex == -1 {
		return fmt.Errorf("pod %s/%s encap IP %s not found in node %s encap IPs %v",
			pod.Namespace, pod.Name, encapIP, node.Name, encapIPs)
	}

	klog.V(5).Infof("Local pod %s/%s on node %s using encap IP %v, tunnel ID %d, index %d",
		pod.Namespace, pod.Name, node.Name, encapIP, tunnelID, encapIndex)

	// Check if transit switch port already exists for this tunnel ID
	transitSwitchPortName := zic.getTransitSwitchToRouterPortName(node.Name, encapIndex)
	lsp := &nbdb.LogicalSwitchPort{Name: transitSwitchPortName}
	lsp, err = libovsdbops.GetLogicalSwitchPort(zic.nbClient, lsp)
	if err == nil && lsp != nil {
		// Port already exists
		return nil
	}

	// Port doesn't exist, create it
	klog.Infof("Creating local transit switch port for node %s encap IP %s (tunnel ID %d, index %d)",
		node.Name, encapIP, tunnelID, encapIndex)

	return zic.createLocalNodeTransitSwitchPort(node, tunnelID, encapIP, encapIndex)
}

// createLocalZoneNodeResources creates the local zone node resources for interconnect
//
//   - creates Transit switch if it doesn't yet exit
//
//   - if node has only one encap IP, the below steps are performed:
//
//     creates a logical switch port of type "router" in the transit switch with the name as - <network_name>.tstor-<node_name>
//     Eg. if the node name is ovn-worker and the network is default, the name would be - tstor-ovn-worker
//     if the node name is ovn-worker and the network name is blue, the logical port name would be - blue.tstor-ovn-worker
//
//     creates a logical router port in the ovn_cluster_router with the name - <network_name>.rtots-<node_name> and connects
//     to the node logical switch port in the transit switch
//
//   - if node has multiple encap IPs, no logical switch port and logical router port are created here, they will be when a Pod
//     is scheduled on the node.
//
//   - remove any stale static routes in the ovn_cluster_router for the node
func (zic *ZoneInterconnectHandler) createLocalZoneNodeResources(node *corev1.Node, nodeID int) error {
	nodeTransitSwitchPortIPs, err := util.ParseNodeTransitSwitchPortAddrs(node)
	if err != nil || len(nodeTransitSwitchPortIPs) == 0 {
		return fmt.Errorf("failed to get the node transit switch port ips for node %s: %w", node.Name, err)
	}

	tunnelIds, _ := util.GetNodeTransitSwitchPortTunnelIDs(node)
	if len(tunnelIds) == 0 {
		// use the node ID as the first transit switch port tunnel id, which is always allocated.
		tunnelIds = append(tunnelIds, nodeID)
	}

	encapIPs, _ := util.ParseNodeEncapIPsAnnotation(node)
	if len(encapIPs) > 0 && len(tunnelIds) != len(encapIPs) {
		return fmt.Errorf("number of encap IPs (%d) and tunnel IDs (%d) for node %s do not match",
			len(encapIPs), len(tunnelIds), node.Name)
	}

	if util.IsMultiVTEPEnabled() && len(encapIPs) > 1 {
		// If node has multiple encap IPs, the logical router port will be created when a pod is scheduled on the node.
		return zic.deleteLocalNodeStaticRoutes(node, nodeTransitSwitchPortIPs)
	}
	// node may don't have `k8s.ovn.org/node-encap-ips` annotation, set encapIP to empty string in this case.
	encapIP := ""
	if len(encapIPs) > 0 {
		encapIP = encapIPs[0]
	}

	// for single-VTEP node, create transit switch port for the single encap IP
	if err := zic.createLocalNodeTransitSwitchPort(node, nodeID, encapIP, 0); err != nil {
		return err
	}

	// Its possible that node is moved from a remote zone to the local zone. Check and delete the remote zone routes
	// for this node as it's no longer needed.
	return zic.deleteLocalNodeStaticRoutes(node, nodeTransitSwitchPortIPs)
}

// createRemoteNodeTransitSwitchPort creates a remote transit switch port for a specific tunnel ID.
// This is called:
// - For single-VTEP nodes: once during node initialization
// - For multi-VTEP nodes: on-demand when the first pod using a specific encap IP is scheduled on the remote node
func (zic *ZoneInterconnectHandler) createRemoteNodeTransitSwitchPort(node *corev1.Node, tunnelID int, encapIp string, encapIndex int) error {
	// Derive transit switch port IPs from tunnel ID
	nodeTransitSwitchPortIPs, err := zic.deriveTransitSwitchPortIPs(tunnelID)
	if err != nil {
		return fmt.Errorf("failed to derive transit switch port IPs for remote node %s tunnel ID %d: %w", node.Name, tunnelID, err)
	}
	if len(nodeTransitSwitchPortIPs) == 0 {
		return fmt.Errorf("no transit switch port IPs derived for remote node %s tunnel ID %d", node.Name, tunnelID)
	}

	// Generate MAC address from first IP
	transitRouterPortMac := util.IPAddrToHWAddr(nodeTransitSwitchPortIPs[0].IP)
	var transitRouterPortNetworks []string
	for _, ip := range nodeTransitSwitchPortIPs {
		transitRouterPortNetworks = append(transitRouterPortNetworks, ip.String())
	}

	// Build remote port addresses
	remotePortAddr := transitRouterPortMac.String()
	for _, tsNetwork := range transitRouterPortNetworks {
		remotePortAddr = remotePortAddr + " " + tsNetwork
	}

	lspOptions := map[string]string{
		libovsdbops.RequestedTnlKey:  strconv.Itoa(tunnelID),
		libovsdbops.RequestedChassis: node.Name,
	}

	// Store the node name and network name in the external_ids column for book keeping
	externalIDs := map[string]string{
		types.NetworkExternalID: zic.GetNetworkName(),
		"node":                  node.Name,
	}

	// Create remote port name with appropriate suffix
	remotePortName := GetNodeTransitSwitchPortName(zic.GetNetworkScopedName(types.TransitSwitchToRouterPrefix+node.Name), encapIndex)
	if err := zic.addNodeLogicalSwitchPort(zic.networkTransitSwitchName, remotePortName, lportTypeRemote, []string{remotePortAddr}, lspOptions, externalIDs); err != nil {
		return err
	}

	if encapIp != "" {
		klog.V(5).Infof("Updating port binding for remote TSP %s with encap ip %s", remotePortName, encapIp)
		chassisID, err := util.ParseNodeChassisIDAnnotation(node)
		if err != nil || chassisID == "" {
			return fmt.Errorf("failed to parse chassis ID for node %s: %w", node.Name, err)
		}
		if err = libovsdbops.UpdatePortBindingSetEncap(zic.sbClient, remotePortName, chassisID, encapIp); err != nil {
			return fmt.Errorf("failed to update port binding for %s: %w", remotePortName, err)
		}
	}

	return nil
}

// EnsureRemoteNodeTransitSwitchPortForPod ensures that a remote transit switch port exists for the
// pod's encap IP on a multi-VTEP remote node. This is called when adding a pod on a multi-VTEP remote node
// to create the remote transit switch port on-demand.
//
// For single-VTEP nodes, this function does nothing as the port is already created during node initialization.
func (zic *ZoneInterconnectHandler) EnsureRemoteNodeTransitSwitchPortForPod(pod *corev1.Pod, podAnnotation *util.PodAnnotation) error {

	if !util.IsMultiVTEPEnabled() {
		return nil
	}

	node, err := zic.watchFactory.GetNode(pod.Spec.NodeName)
	if err != nil {
		return fmt.Errorf("failed to get node %s for pod %s/%s: %w", pod.Spec.NodeName, pod.Namespace, pod.Name, err)
	}

	// If annotation doesn't exist, treat as single-VTEP node
	encapIPs, err := util.ParseNodeEncapIPsAnnotation(node)
	if err != nil && !util.IsAnnotationNotSetError(err) {
		return err
	}

	if len(encapIPs) <= 1 {
		// Single-VTEP node, port already created during node initialization
		return nil
	}

	nodeID, err := util.GetNodeID(node)
	if err != nil || nodeID == util.InvalidNodeID {
		return fmt.Errorf("failed to get node ID for node %s: %w", node.Name, err)
	}

	// Get tunnel IDs for the node
	tunnelIds, err := util.GetNodeTransitSwitchPortTunnelIDs(node)
	if err != nil {
		return fmt.Errorf("failed to get tunnel IDs for node %s: %w", node.Name, err)
	}
	if len(tunnelIds) == 0 {
		tunnelIds = append(tunnelIds, nodeID)
	}

	if len(tunnelIds) != len(encapIPs) {
		return fmt.Errorf("number of encap IPs (%d) and tunnel IDs (%d) for node %s do not match",
			len(encapIPs), len(tunnelIds), node.Name)
	}

	// Multi-VTEP node: find which tunnel ID corresponds to the pod's encap IP
	encapIndex := -1
	tunnelID := -1
	encapIP := podAnnotation.EncapIP
	if encapIP == "" {
		// If a network is configured to use SR-IOV VF interfaces, all pods on the network will have `encap_ip` set.
		// Otherwise, the Pod will use veth interface and don't have `encap_ip` specified in `k8s.ovn.org/pod-networks`.
		// In this case, the first VTEP on remote node will be used to create the remote transit switch port.
		encapIP = encapIPs[0]
		encapIndex = 0
		tunnelID = tunnelIds[0]
	} else {
		for i, ip := range encapIPs {
			if ip == encapIP {
				encapIndex = i
				tunnelID = tunnelIds[i]
				break
			}
		}
	}

	if encapIndex == -1 {
		return fmt.Errorf("pod %s/%s encap IP %s not found in node %s encap IPs %v",
			pod.Namespace, pod.Name, encapIP, node.Name, encapIPs)
	}

	klog.V(5).Infof("Remote pod %s/%s on node %s using encap IP %v, tunnel ID %d, encap IP index %d",
		pod.Namespace, pod.Name, node.Name, encapIP, tunnelID, encapIndex)

	// Check if remote transit switch port already exists for this pod's encap IP
	tspNamePrefix := zic.GetNetworkScopedName(types.TransitSwitchToRouterPrefix + node.Name)
	remotePortName := GetNodeTransitSwitchPortName(tspNamePrefix, encapIndex)

	lsp := &nbdb.LogicalSwitchPort{Name: remotePortName}
	lsp, err = libovsdbops.GetLogicalSwitchPort(zic.nbClient, lsp)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to check if transit switch port %s exists: %w", remotePortName, err)
	}

	// Create TSP if it doesn't exist
	if lsp == nil {
		klog.Infof("Creating remote transit switch port %s for node %s, tunnel ID %d, encap IP %s, encap IP index %d",
			remotePortName, node.Name, tunnelID, encapIP, encapIndex)
		if err := zic.createRemoteNodeTransitSwitchPort(node, tunnelID, encapIP, encapIndex); err != nil {
			return err
		}
	} else {
		klog.V(5).Infof("Transit switch port %s already exists for node %s encap index %d",
			remotePortName, node.Name, encapIndex)
		// Ensure port binding is updated with encap even if port already exists
		if encapIP != "" {
			klog.V(5).Infof("Updating port binding for existing remote TSP %s with encap ip %s", remotePortName, encapIP)
			chassisID, err := util.ParseNodeChassisIDAnnotation(node)
			if err != nil || chassisID == "" {
				return fmt.Errorf("failed to parse chassis ID for node %s: %w", node.Name, err)
			}
			if err = libovsdbops.UpdatePortBindingSetEncap(zic.sbClient, remotePortName, chassisID, encapIP); err != nil {
				return fmt.Errorf("failed to update port binding for %s: %w", remotePortName, err)
			}
		}
	}

	// Derive transit switch port IPs for the nexthop
	nodeTransitSwitchPortIPs, err := zic.deriveTransitSwitchPortIPs(tunnelID)
	if err != nil {
		return fmt.Errorf("failed to derive transit switch port IPs for tunnel ID %d: %w", tunnelID, err)
	}

	// Add static routes based on whether this is the first pod on node
	p := func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
		return lrsr.ExternalIDs["ic-node"] == node.Name &&
			lrsr.ExternalIDs[types.NetworkExternalID] == zic.GetNetworkName()
	}
	lr := &nbdb.LogicalRouter{Name: zic.networkClusterRouterName}
	logicalRouterStaticRoutes, err := libovsdbops.GetRouterLogicalRouterStaticRoutesWithPredicate(zic.nbClient, lr, p)
	if err != nil {
		return fmt.Errorf("unable to get logical router static routes with predicate on router %s: %w", zic.networkClusterRouterName, err)
	}

	nodeSubnets, err := util.ParseNodeHostSubnetAnnotation(node, zic.GetNetworkName())
	if err != nil {
		return fmt.Errorf("failed to parse node %s subnets annotation: %w", node.Name, err)
	}
	nodeSubnetStaticRoutes := []*nbdb.LogicalRouterStaticRoute{}
	for _, r := range logicalRouterStaticRoutes {
		for _, ns := range nodeSubnets {
			if r.IPPrefix == ns.String() {
				nodeSubnetStaticRoutes = append(nodeSubnetStaticRoutes, r)
			}
		}
	}

	if len(nodeSubnetStaticRoutes) == 0 {
		// First pod on this remote node: add /24 node subnet route
		klog.Infof("Adding node subnet route for first pod on remote node %s", node.Name)

		var nodeGRPIPs []*net.IPNet
		// only primary networks have cluster router connected to join switch+GR
		// used for adding routes to GR
		if !zic.IsUserDefinedNetwork() || (util.IsNetworkSegmentationSupportEnabled() && zic.IsPrimaryNetwork()) {
			nodeGRPIPs, err = udn.GetGWRouterIPs(node, zic.GetNetInfo())
			if err != nil {
				return err
			}
		}

		if err := zic.addRemoteNodeStaticRoutes(node, nodeTransitSwitchPortIPs, nodeSubnets, nodeGRPIPs); err != nil {
			return err
		}

		return nil
	}

	canUseNodeSubnetRoute := false
	for _, r := range nodeSubnetStaticRoutes {
		for _, nexthop := range nodeTransitSwitchPortIPs {
			if r.Nexthop == nexthop.IP.String() {
				canUseNodeSubnetRoute = true
				break
			}
		}
	}

	if !canUseNodeSubnetRoute {
		// a pod uses a differnt VTEP on remote node: add /32 host route for this pod
		klog.Infof("Adding /32 host route for pod %s/%s on remote node %s", pod.Namespace, pod.Name, node.Name)

		// Convert pod IPs to /32 (or /128 for IPv6) host routes
		podIps := make([]*net.IPNet, 0, len(podAnnotation.IPs))
		for _, podIP := range podAnnotation.IPs {
			podIps = append(podIps, &net.IPNet{
				IP:   podIP.IP,
				Mask: util.GetIPFullMask(podIP.IP),
			})
		}

		if err := zic.addRemoteNodeStaticRoutes(node, nodeTransitSwitchPortIPs, podIps, nil); err != nil {
			return err
		}
	}
	return nil
}

// createRemoteZoneNodeResources creates the remote zone node resources
//   - creates Transit switch if it doesn't yet exit
//   - creates a logical port of type "remote" in the transit switch with the name as - <network_name>_tstor-<node_name>
//     Eg. if the node name is ovn-worker and the network is default, the name would be - tstor-ovn-worker
//     if the node name is ovn-worker and the network name is blue, the logical port name would be - blue_tstor-ovn-worker
//   - binds the remote port to the node remote chassis in SBDB
//   - adds static routes for the remote node via the remote port ip in the ovn_cluster_router
func (zic *ZoneInterconnectHandler) createRemoteZoneNodeResources(node *corev1.Node, nodeID int, nodeTransitSwitchPortIPs, nodeSubnets, nodeGRPIPs []*net.IPNet) error {
	tunnelIds, _ := util.GetNodeTransitSwitchPortTunnelIDs(node)
	if len(tunnelIds) == 0 {
		// use the node ID as the first transit switch port tunnel id, which is always allocated.
		tunnelIds = append(tunnelIds, nodeID)
	}

	encapIPs, _ := util.ParseNodeEncapIPsAnnotation(node)
	if len(encapIPs) > 0 && len(tunnelIds) != len(encapIPs) {
		return fmt.Errorf("number of encap IPs (%d) and tunnel IDs (%d) for node %s do not match",
			len(encapIPs), len(tunnelIds), node.Name)
	}

	// if remote node has multiple encap IPs, no remote logical switch port needs to be created, the remote logical switch port
	// and static route will be created on-demand.
	if util.IsMultiVTEPEnabled() && len(encapIPs) > 1 {
		klog.Infof("Postpone remote multi-VTEP node %s resources creation", node.Name)
		// for Multi-VTEP node, the remote transit switch port and static route for node subnet
		// will be created when a pod is scheduled on the node.
		// if err := zic.addRemoteNodeStaticRoutes(node, nodeTransitSwitchPortIPs, nil, nodeGRPIPs); err != nil {
		// 	return err
		// }
		return zic.cleanupNodeClusterRouterPort(node.Name)
	}
	encapIP := ""
	if len(encapIPs) > 0 {
		encapIP = encapIPs[0]
	}

	// Single-VTEP node: create remote transit switch port
	if err := zic.createRemoteNodeTransitSwitchPort(node, nodeID, encapIP, 0); err != nil {
		return err
	}

	if err := zic.addRemoteNodeStaticRoutes(node, nodeTransitSwitchPortIPs, nodeSubnets, nodeGRPIPs); err != nil {
		return err
	}

	// Cleanup the logical router port connecting to the transit switch for the remote node (if present)
	// Cleanup would be required when a local zone node moves to a remote zone.
	return zic.cleanupNodeClusterRouterPort(node.Name)
}

func (zic *ZoneInterconnectHandler) addNodeLogicalSwitchPort(logicalSwitchName, portName, portType string, addresses []string, options, externalIDs map[string]string) error {
	logicalSwitch := nbdb.LogicalSwitch{
		Name: logicalSwitchName,
	}

	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:        portName,
		Type:        portType,
		Options:     options,
		Addresses:   addresses,
		ExternalIDs: externalIDs,
	}
	if err := libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(zic.nbClient, &logicalSwitch, &logicalSwitchPort); err != nil {
		return fmt.Errorf("failed to add logical port %s to switch %s, error: %w", portName, logicalSwitch.Name, err)
	}
	return nil
}

// cleanupNode cleansup the local zone node or remote zone node resources
func (zic *ZoneInterconnectHandler) cleanupNode(nodeName string) error {
	klog.Infof("Cleaning up interconnect resources for the node %s for the network %s", nodeName, zic.GetNetworkName())

	// Cleanup the logical router port in the cluster router for the node
	// if it exists.
	if err := zic.cleanupNodeClusterRouterPort(nodeName); err != nil {
		return err
	}

	// Cleanup the logical switch port in the transit switch for the node
	// if it exists.
	if err := zic.cleanupNodeTransitSwitchPort(nodeName); err != nil {
		return err
	}

	// Delete any static routes in the cluster router for this node.
	// skip types.NetworkExternalID check in the predicate function as this static route may be deleted
	// before types.NetworkExternalID external-ids is set correctly during upgrade.
	p := func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
		return lrsr.ExternalIDs["ic-node"] == nodeName
	}
	if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(zic.nbClient, zic.networkClusterRouterName, p); err != nil {
		return fmt.Errorf("failed to cleanup static routes for the node %s: %w", nodeName, err)
	}

	return nil
}

func (zic *ZoneInterconnectHandler) cleanupNodeClusterRouterPort(nodeName string) error {

	p := func(lrp *nbdb.LogicalRouterPort) bool {
		// In multi-VTEP case, a local node may have multiple logical router ports.
		// Match by network and node external_ids to ensure all corresponding ports are cleaned up.
		if lrp.ExternalIDs[types.NetworkExternalID] == zic.GetNetworkName() &&
			lrp.ExternalIDs[types.NodeExternalID] == nodeName {
			return true
		}
		// Backward compatibility: legacy ports without external_ids
		// use exact name matching to avoid collision (ovn-worker vs ovn-worker2)
		lrpName := zic.GetNetworkScopedName(types.RouterToTransitSwitchPrefix + nodeName)
		return lrp.Name == lrpName
	}
	lrps, err := libovsdbops.FindLogicalRouterPortWithPredicate(zic.nbClient, p)
	if err != nil {
		return fmt.Errorf("failed to find logical router port for the node %s: %w", nodeName, err)
	}

	logicalRouter := nbdb.LogicalRouter{
		Name: zic.networkClusterRouterName,
	}

	if err := libovsdbops.DeleteLogicalRouterPorts(zic.nbClient, &logicalRouter, lrps...); err != nil {
		return fmt.Errorf("failed to delete logical router port %v from router %s for the node %s, error: %w", lrps, zic.networkClusterRouterName, nodeName, err)
	}

	return nil
}

func (zic *ZoneInterconnectHandler) cleanupNodeTransitSwitchPort(nodeName string) error {

	p := func(lsp *nbdb.LogicalSwitchPort) bool {
		// In multi-VTEP case, a remote node may have multiple remote transit switch ports.
		// Match by network and node external_ids to ensure all corresponding ports are cleaned up.
		if lsp.ExternalIDs[types.NetworkExternalID] == zic.GetNetworkName() &&
			lsp.ExternalIDs["node"] == nodeName {
			return true
		}
		// Backward compatibility: legacy ports without external_ids
		// use exact name matching to avoid collision (ovn-worker vs ovn-worker2)
		lspName := zic.GetNetworkScopedName(types.TransitSwitchToRouterPrefix + nodeName)
		return lsp.Name == lspName
	}
	lsps, err := libovsdbops.FindLogicalSwitchPortWithPredicate(zic.nbClient, p)
	if err != nil {
		return fmt.Errorf("failed to find logical router port for the node %s: %w", nodeName, err)
	}

	logicalSwitch := &nbdb.LogicalSwitch{
		Name: zic.networkTransitSwitchName,
	}

	if err := libovsdbops.DeleteLogicalSwitchPorts(zic.nbClient, logicalSwitch, lsps...); err != nil {
		return fmt.Errorf("failed to delete logical switch port %v from transit switch %s for the node %s, error: %w", lsps, zic.networkTransitSwitchName, nodeName, err)
	}
	return nil
}

// addRemoteNodeStaticRoutes adds static routes in ovn_cluster_router to reach the remote node via the
// remote node transit switch port.
// Eg. if node ovn-worker2 is a remote node
// ovn-worker2 - { node_subnet = 10.244.0.0/24,  node id = 2,  transit switch port ip = 100.88.0.2/16,  join ip connecting to GR_ovn-worker = 100.64.0.2/16}
// Then the below static routes are added
// ip4.dst == 10.244.0.0/24 , nexthop = 100.88.0.2
// ip4.dst == 100.64.0.2/16 , nexthop = 100.88.0.2  (only for default primary network)
func (zic *ZoneInterconnectHandler) addRemoteNodeStaticRoutes(node *corev1.Node, nodeTransitSwitchPortIPs, nodeSubnets, nodeGRPIPs []*net.IPNet) error {
	ops := make([]ovsdb.Operation, 0, 2)
	addRoute := func(prefix, nexthop string) error {
		logicalRouterStaticRoute := nbdb.LogicalRouterStaticRoute{
			ExternalIDs: map[string]string{
				"ic-node":               node.Name,
				types.NetworkExternalID: zic.GetNetworkName(),
			},
			Nexthop:  nexthop,
			IPPrefix: prefix,
		}
		// Note that because logical router static routes were originally created without types.NetworkExternalID
		// external-ids, skip types.NetworkExternalID check in the predicate function to replace existing static route
		// with correct external-ids on an upgrade scenario.
		p := func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
			return lrsr.IPPrefix == prefix &&
				lrsr.ExternalIDs["ic-node"] == node.Name
		}
		var err error
		ops, err = libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicateOps(zic.nbClient, ops, zic.networkClusterRouterName, &logicalRouterStaticRoute, p)
		if err != nil {
			return fmt.Errorf("failed to create static route ops: %w", err)
		}
		return nil
	}

	nodeSubnetStaticRoutes := zic.getStaticRoutes(nodeSubnets, nodeTransitSwitchPortIPs, false)
	for _, staticRoute := range nodeSubnetStaticRoutes {
		if err := addRoute(staticRoute.prefix, staticRoute.nexthop); err != nil {
			return fmt.Errorf("error adding static route %s - %s to the router %s : %w", staticRoute.prefix, staticRoute.nexthop, zic.networkClusterRouterName, err)
		}
	}

	if len(nodeGRPIPs) > 0 {
		nodeGRPIPStaticRoutes := zic.getStaticRoutes(nodeGRPIPs, nodeTransitSwitchPortIPs, true)
		for _, staticRoute := range nodeGRPIPStaticRoutes {
			if err := addRoute(staticRoute.prefix, staticRoute.nexthop); err != nil {
				return fmt.Errorf("error adding static route %s - %s to the router %s : %w", staticRoute.prefix, staticRoute.nexthop, zic.networkClusterRouterName, err)
			}
		}
	}

	_, err := libovsdbops.TransactAndCheck(zic.nbClient, ops)
	return err
}

// deleteLocalNodeStaticRoutes deletes the static routes added by the function addRemoteNodeStaticRoutes
func (zic *ZoneInterconnectHandler) deleteLocalNodeStaticRoutes(node *corev1.Node, nodeTransitSwitchPortIPs []*net.IPNet) error {
	// skip types.NetworkExternalID check in the predicate function as this static route may be deleted
	// before types.NetworkExternalID external-ids is set correctly during upgrade.
	deleteRoute := func(prefix, nexthop string) error {
		p := func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
			return lrsr.IPPrefix == prefix &&
				lrsr.Nexthop == nexthop &&
				lrsr.ExternalIDs["ic-node"] == node.Name
		}
		if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(zic.nbClient, zic.networkClusterRouterName, p); err != nil {
			return fmt.Errorf("failed to delete static route: %w", err)
		}
		return nil
	}

	nodeSubnets, err := util.ParseNodeHostSubnetAnnotation(node, zic.GetNetworkName())
	if err != nil {
		return fmt.Errorf("failed to parse node %s subnets annotation %w", node.Name, err)
	}

	nodeSubnetStaticRoutes := zic.getStaticRoutes(nodeSubnets, nodeTransitSwitchPortIPs, false)
	for _, staticRoute := range nodeSubnetStaticRoutes {
		// Possible optimization: Add all the routes in one transaction
		if err := deleteRoute(staticRoute.prefix, staticRoute.nexthop); err != nil {
			return fmt.Errorf("error deleting static route %s - %s from the router %s : %w", staticRoute.prefix, staticRoute.nexthop, zic.networkClusterRouterName, err)
		}
	}

	if zic.IsUserDefinedNetwork() {
		// UDN cluster router doesn't connect to a join switch
		// or to a Gateway router.
		return nil
	}

	// Clear the routes connecting to the GW Router for the default network
	nodeGRPIPs, err := udn.GetGWRouterIPs(node, zic.GetNetInfo())
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			// FIXME(tssurya): This is present for backwards compatibility
			// Remove me a few months from now
			var err1 error
			nodeGRPIPs, err1 = util.ParseNodeGatewayRouterLRPAddrs(node)
			if err1 != nil {
				return fmt.Errorf("failed to parse node %s Gateway router LRP Addrs annotation %w", node.Name, err1)
			}
		}
	}

	nodenodeGRPIPStaticRoutes := zic.getStaticRoutes(nodeGRPIPs, nodeTransitSwitchPortIPs, true)
	for _, staticRoute := range nodenodeGRPIPStaticRoutes {
		// Possible optimization: Add all the routes in one transaction
		if err := deleteRoute(staticRoute.prefix, staticRoute.nexthop); err != nil {
			return fmt.Errorf("error deleting static route %s - %s from the router %s : %w", staticRoute.prefix, staticRoute.nexthop, zic.networkClusterRouterName, err)
		}
	}

	return nil
}

func (zic *ZoneInterconnectHandler) DeleteRemotePod(pod *corev1.Pod, podAnnotation *util.PodAnnotation) error {
	node, err := zic.watchFactory.GetNode(pod.Spec.NodeName)
	if err != nil {
		return fmt.Errorf("failed to find node %s: %w", pod.Spec.NodeName, err)
	}
	encapIPs, _ := util.ParseNodeEncapIPsAnnotation(node)
	if !util.IsMultiVTEPEnabled() || len(encapIPs) <= 1 {
		// For remote node with single VTEP, no pod IP /32 static routes are added, so nothing to delete
		return nil
	}

	// delete the /32 static routes for the pod's IP addresses if they exist
	for _, podIP := range podAnnotation.IPs {
		ipNet := util.GetIPNetFullMaskFromIP(podIP.IP)
		p := func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
			return lrsr.IPPrefix == ipNet.String() &&
				lrsr.ExternalIDs["ic-node"] == pod.Spec.NodeName &&
				lrsr.ExternalIDs[types.NetworkExternalID] == zic.GetNetworkName()
		}

		if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(zic.nbClient, zic.networkClusterRouterName, p); err != nil {
			return fmt.Errorf("failed to delete static route for pod IP %s: %w", podIP.String(), err)
		}
	}

	return nil
}

// interconnectStaticRoute represents a static route
type interconnectStaticRoute struct {
	prefix  string
	nexthop string
}

// getStaticRoutes returns a list of static routes from the provided ipPrefix'es and nexthops
// Eg. If ipPrefixes - [10.0.0.4/24, aef0::4/64] and nexthops - [100.88.0.4/16, bef0::4/64] and fullMask is true
//
// It will return [interconnectStaticRoute { prefix : 10.0.0.4/32, nexthop : 100.88.0.4},
// -               interconnectStaticRoute { prefix : aef0::4/128, nexthop : bef0::4}}
//
// If fullMask is false, it will return
// [interconnectStaticRoute { prefix : 10.0.0.4/24, nexthop : 100.88.0.4},
// -               interconnectStaticRoute { prefix : aef0::4/64, nexthop : bef0::4}}
func (zic *ZoneInterconnectHandler) getStaticRoutes(ipPrefixes []*net.IPNet, nexthops []*net.IPNet, fullMask bool) []*interconnectStaticRoute {
	var staticRoutes []*interconnectStaticRoute

	for _, prefix := range ipPrefixes {
		for _, nexthop := range nexthops {
			if utilnet.IPFamilyOfCIDR(prefix) != utilnet.IPFamilyOfCIDR(nexthop) {
				continue
			}
			p := ""
			if fullMask {
				p = prefix.IP.String() + util.GetIPFullMaskString(prefix.IP.String())
			} else {
				p = prefix.String()
			}

			staticRoute := &interconnectStaticRoute{
				prefix:  p,
				nexthop: nexthop.IP.String(),
			}
			staticRoutes = append(staticRoutes, staticRoute)
		}
	}

	return staticRoutes
}

func getUserDefinedNetTransitSwitchExtIDs(networkName, topology string, isPrimaryUDN bool) map[string]string {
	return map[string]string{
		types.NetworkExternalID:     networkName,
		types.NetworkRoleExternalID: util.GetUserDefinedNetworkRole(isPrimaryUDN),
		types.TopologyExternalID:    topology,
	}
}

// GetNodeTransitSwitchPortName returns the transit switch port name corresponding to
// the encap IP at the given `index` in the k8s.ovn.org/node-encap-ips annotation.
func GetNodeTransitSwitchPortName(baseName string, index int) string {
	if index == 0 {
		return baseName
	}
	return fmt.Sprintf("%s_encap%d", baseName, index)
}
