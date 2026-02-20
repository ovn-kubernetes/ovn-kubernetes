package ovn

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"golang.org/x/exp/maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/gateway"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/gatewayrouter"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type GatewayManager struct {
	nodeName                string
	clusterRouterName       string
	gwRouterName            string
	extSwitchName           string
	joinSwitchName          string
	coppUUID                string
	kube                    kube.InterfaceOVN
	nbClient                libovsdbclient.Client
	netInfo                 util.NetInfo
	watchFactory            *factory.WatchFactory
	getNetworkNameForNADKey func(nadKey string) string
	// Cluster wide Load_Balancer_Group UUID.
	// Includes all node switches and node gateway routers.
	clusterLoadBalancerGroupUUID string

	// Cluster wide switch Load_Balancer_Group UUID.
	// Includes all node switches.
	switchLoadBalancerGroupUUID string

	// Cluster wide router Load_Balancer_Group UUID.
	// Includes all node gateway routers.
	routerLoadBalancerGroupUUID string

	transitRouterInfo *transitRouterInfo
}

type GatewayOption func(*GatewayManager)

func NewGatewayManagerForLayer2Topology(
	nodeName string,
	coopUUID string,
	kube kube.InterfaceOVN,
	nbClient libovsdbclient.Client,
	netInfo util.NetInfo,
	watchFactory *factory.WatchFactory,
	useTransitRouter bool,
	opts ...GatewayOption,
) *GatewayManager {
	routerName := ""
	if useTransitRouter {
		routerName = netInfo.GetNetworkScopedClusterRouterName()
	}
	return newGWManager(
		nodeName,
		routerName,
		netInfo.GetNetworkScopedExtSwitchName(nodeName),
		netInfo.GetNetworkScopedSwitchName(""),
		coopUUID,
		kube,
		nbClient,
		netInfo,
		watchFactory,
		opts...,
	)
}

func NewGatewayManager(
	nodeName string,
	coopUUID string,
	kube kube.InterfaceOVN,
	nbClient libovsdbclient.Client,
	netInfo util.NetInfo,
	watchFactory *factory.WatchFactory,
	opts ...GatewayOption,
) *GatewayManager {
	return newGWManager(
		nodeName,
		netInfo.GetNetworkScopedClusterRouterName(),
		netInfo.GetNetworkScopedExtSwitchName(nodeName),
		netInfo.GetNetworkScopedJoinSwitchName(),
		coopUUID,
		kube,
		nbClient,
		netInfo,
		watchFactory,
		opts...,
	)
}

func newGWManager(
	nodeName, clusterRouterName, extSwitchName, joinSwitchName string,
	coopUUID string,
	kube kube.InterfaceOVN,
	nbClient libovsdbclient.Client,
	netInfo util.NetInfo,
	watchFactory *factory.WatchFactory,
	opts ...GatewayOption) *GatewayManager {
	gwManager := &GatewayManager{
		nodeName:          nodeName,
		clusterRouterName: clusterRouterName,
		gwRouterName:      netInfo.GetNetworkScopedGWRouterName(nodeName),
		extSwitchName:     extSwitchName,
		joinSwitchName:    joinSwitchName,
		coppUUID:          coopUUID,
		kube:              kube,
		nbClient:          nbClient,
		netInfo:           netInfo,
		watchFactory:      watchFactory,
	}

	for _, opt := range opts {
		opt(gwManager)
	}

	return gwManager
}

func WithLoadBalancerGroups(routerLBGroup, clusterLBGroup, switchLBGroup string) GatewayOption {
	return func(manager *GatewayManager) {
		manager.routerLoadBalancerGroupUUID = routerLBGroup
		manager.clusterLoadBalancerGroupUUID = clusterLBGroup
		manager.switchLoadBalancerGroupUUID = switchLBGroup
	}
}

func WithNetworkNameForNADKeyResolver(getNetworkNameForNADKey func(nadKey string) string) GatewayOption {
	return func(manager *GatewayManager) {
		manager.getNetworkNameForNADKey = getNetworkNameForNADKey
	}
}

// cleanupStalePodSNATs removes pod SNATs against nodeIP for the given node if
// the SNAT.logicalIP isn't an active podIP, or disableSNATMultipleGWs=false.
// We don't have to worry about
// missing SNATs that should be added because addLogicalPort takes care of this
// for all pods when RequestRetryObjs is called for each node add.
// Other non-pod SNATs like join subnet SNATs are ignored.
// NOTE: On startup libovsdb adds back all the pods and this should normally
// update all existing SNATs accordingly. Due to a stale egressIP cache bug
// https://issues.redhat.com/browse/OCPBUGS-1520 we ended up adding wrong
// pod->nodeSNATs which won't get cleared up unless explicitly deleted.
// NOTE2: egressIP SNATs are synced in EIP controller.
func (gw *GatewayManager) cleanupStalePodSNATs(nodeName string, nodeIPs []*net.IPNet, gwLRPIPs []net.IP) error {
	if gw.netInfo.IsUserDefinedNetwork() && gw.getNetworkNameForNADKey == nil {
		return fmt.Errorf("missing NAD resolver for network %q", gw.netInfo.GetNetworkName())
	}
	// collect all the pod IPs for which we should be doing the SNAT;
	// if DisableSNATMultipleGWs==false we consider all
	// the SNATs stale
	podIPsWithSNAT := sets.New[string]()
	if config.Gateway.DisableSNATMultipleGWs {
		pods, err := gw.watchFactory.GetAllPods()
		if err != nil {
			return fmt.Errorf("unable to list existing pods on node: %s, %w",
				nodeName, err)
		}
		for _, pod := range pods {
			pod := *pod
			if !util.PodScheduled(&pod) { //if the pod is not scheduled we should not remove the nat
				continue
			}
			if pod.Spec.NodeName != nodeName {
				continue
			}
			if util.PodCompleted(&pod) {
				collidingPod, err := findPodWithIPAddresses(gw.watchFactory, gw.netInfo, []net.IP{utilnet.ParseIPSloppy(pod.Status.PodIP)}, "", gw.getNetworkNameForNADKey) //even if a pod is completed we should still delete the nat if the ip is not in use anymore
				if err != nil {
					return fmt.Errorf("lookup for pods with same ip as %s %s failed: %w", pod.Namespace, pod.Name, err)
				}
				if collidingPod != nil { //if the ip is in use we should not remove the nat
					continue
				}
			}
			podIPs, err := util.GetPodIPsOfNetwork(&pod, gw.netInfo, gw.getNetworkNameForNADKey)
			if err != nil && errors.Is(err, util.ErrNoPodIPFound) {
				// It is possible that the pod is scheduled during this time, but the LSP add or
				// IP Allocation has not happened and it is waiting for the WatchPods to start
				// after WatchNodes completes (This function is called during syncNodes). So since
				// the pod doesn't have any IPs, there is no SNAT here to keep for this pod so we skip
				// this pod from processing and move onto the next one.
				klog.Warningf("Unable to fetch podIPs for pod %s/%s: %v", pod.Namespace, pod.Name, err)
				continue // no-op
			} else if err != nil {
				return fmt.Errorf("unable to fetch podIPs for pod %s/%s: %w", pod.Namespace, pod.Name, err)
			}
			for _, podIP := range podIPs {
				podIPsWithSNAT.Insert(podIP.String())
			}
		}
	}

	gatewayRouter := &nbdb.LogicalRouter{
		Name: gw.gwRouterName,
	}
	routerNATs, err := libovsdbops.GetRouterNATs(gw.nbClient, gatewayRouter)
	if err != nil && errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("unable to get NAT entries for router %s on node %s: %w", gatewayRouter.Name, nodeName, err)
	}

	nodeIPset := sets.New(util.IPNetsIPToStringSlice(nodeIPs)...)
	gwLRPIPset := sets.New(util.StringSlice(gwLRPIPs)...)
	natsToDelete := []*nbdb.NAT{}
	for _, routerNat := range routerNATs {
		routerNat := routerNat
		if routerNat.Type != nbdb.NATTypeSNAT {
			continue
		}
		if !nodeIPset.Has(routerNat.ExternalIP) {
			continue
		}
		if podIPsWithSNAT.Has(routerNat.LogicalIP) {
			continue
		}
		if gwLRPIPset.Has(routerNat.LogicalIP) {
			continue
		}
		logicalIP := net.ParseIP(routerNat.LogicalIP)
		if logicalIP == nil {
			// this is probably a CIDR so not a pod IP
			continue
		}
		natsToDelete = append(natsToDelete, routerNat)
	}
	if len(natsToDelete) > 0 {
		err := libovsdbops.DeleteNATs(gw.nbClient, gatewayRouter, natsToDelete...)
		if err != nil {
			return fmt.Errorf("unable to delete NATs %+v from node %s: %w", natsToDelete, nodeName, err)
		}
	}

	return nil
}

func (gw *GatewayManager) createGWRouter(gwConfig *GatewayConfig) (*nbdb.LogicalRouter, error) {
	// Create a gateway router.
	dynamicNeighRouters := "true"
	if config.OVNKubernetesFeature.EnableInterconnect {
		dynamicNeighRouters = "false"
	}

	logicalRouterOptions := map[string]string{
		"always_learn_from_arp_request": "false",
		"dynamic_neigh_routers":         dynamicNeighRouters,
		"chassis":                       gwConfig.annoConfig.ChassisID,
		"lb_force_snat_ip":              "router_ip",
		"mac_binding_age_threshold":     types.GRMACBindingAgeThreshold,
	}
	// set the snat-ct-zone only for the default network
	// for UDN's OVN will pick a random one
	if gw.netInfo.GetNetworkName() == types.DefaultNetworkName {
		logicalRouterOptions["snat-ct-zone"] = "0"
	}
	if gw.netInfo.TopologyType() == types.Layer2Topology {
		// When multiple networks are set of the same logical-router-port
		// the networks get lexicographically sorted; thus there is no
		// ordering or telling on which IP will be chosen as the router-ip
		// when it comes to SNATing traffic after load balancing.
		// Hence for Layer2 UDPNs let's set the snat-ip explicitly to the
		// joinsubnetIP
		logicalRouterOptions["lb_force_snat_ip"] = strings.Join(util.IPNetsIPToStringSlice(gwConfig.gwRouterJoinCIDRs), " ")
	}
	physicalIPs := make([]string, len(gwConfig.annoConfig.IPAddresses))
	for i, ip := range gwConfig.annoConfig.IPAddresses {
		physicalIPs[i] = ip.IP.String()
	}
	logicalRouterExternalIDs := map[string]string{
		"physical_ip":  physicalIPs[0],
		"physical_ips": strings.Join(physicalIPs, ","),
	}

	if gw.netInfo.IsUserDefinedNetwork() {
		maps.Copy(logicalRouterExternalIDs, util.GenerateExternalIDsForSwitchOrRouter(gw.netInfo))
	}

	gwRouter := nbdb.LogicalRouter{
		Name:        gw.gwRouterName,
		Options:     logicalRouterOptions,
		ExternalIDs: logicalRouterExternalIDs,
		Copp:        &gw.coppUUID,
	}

	if gw.clusterLoadBalancerGroupUUID != "" {
		gwRouter.LoadBalancerGroup = []string{gw.clusterLoadBalancerGroupUUID}
		if gwConfig.annoConfig.NodePortEnable && gw.routerLoadBalancerGroupUUID != "" {
			// add routerLoadBalancerGroupUUID to the gateway router only if nodePort is enabled
			gwRouter.LoadBalancerGroup = append(gwRouter.LoadBalancerGroup, gw.routerLoadBalancerGroupUUID)
		}
	}

	err := libovsdbops.CreateOrUpdateLogicalRouter(gw.nbClient, &gwRouter, &gwRouter.Options,
		&gwRouter.ExternalIDs, &gwRouter.LoadBalancerGroup, &gwRouter.Copp)
	if err != nil {
		return nil, fmt.Errorf("failed to create logical router %+v: %v", gwRouter, err)
	}
	return &gwRouter, nil
}

func (gw *GatewayManager) getGWRouterPeerRouterPortName() string {
	return types.TransitRouterToRouterPrefix + gw.gwRouterName
}

func (gw *GatewayManager) getGWRouterPeerSwitchPortName() string {
	if gw.netInfo.TopologyType() == types.Layer2Topology {
		return types.SwitchToRouterPrefix + gw.joinSwitchName
	}
	return types.JoinSwitchToGWRouterPrefix + gw.gwRouterName
}

func (gw *GatewayManager) getGWRouterPortName() string {
	if gw.netInfo.TopologyType() == types.Layer2Topology {
		if gw.transitRouterInfo != nil {
			return types.RouterToTransitRouterPrefix + gw.gwRouterName
		}
		return types.RouterToSwitchPrefix + gw.joinSwitchName
	}
	return types.GWRouterToJoinSwitchPrefix + gw.gwRouterName
}

func (gw *GatewayManager) createGWRouterPeerSwitchPort(nodeName string) error {
	gwSwitchPort := gw.getGWRouterPeerSwitchPortName()
	gwRouterPortName := gw.getGWRouterPortName()

	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      gwSwitchPort,
		Type:      "router",
		Addresses: []string{"router"},
		Options: map[string]string{
			libovsdbops.RouterPort: gwRouterPortName,
		},
	}
	if gw.netInfo.IsUserDefinedNetwork() {
		logicalSwitchPort.ExternalIDs = map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		}
	}
	if gw.netInfo.TopologyType() == types.Layer2Topology {
		node, err := gw.watchFactory.GetNode(nodeName)
		if err != nil {
			return fmt.Errorf("failed to fetch node %s from watch factory %w", node.Name, err)
		}
		tunnelID, err := util.ParseUDNLayer2NodeGRLRPTunnelIDs(node, gw.netInfo.GetNetworkName())
		if err != nil {
			if util.IsAnnotationNotSetError(err) {
				// remote node may not have the annotation yet, suppress it
				return types.NewSuppressedError(err)
			}
			// Don't consider this node as cluster-manager has not allocated node id yet.
			return fmt.Errorf("failed to fetch tunnelID annotation from the node %s for network %s, err: %w",
				nodeName, gw.netInfo.GetNetworkName(), err)
		}
		logicalSwitchPort.Options[libovsdbops.RequestedTnlKey] = strconv.Itoa(tunnelID)
	}
	sw := nbdb.LogicalSwitch{Name: gw.joinSwitchName}
	err := libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(gw.nbClient, &sw, &logicalSwitchPort)
	if err != nil {
		return fmt.Errorf("failed to create port %v on logical switch %q: %v", gwSwitchPort, sw.Name, err)
	}
	return err
}

func (gw *GatewayManager) deleteGWRouterPeerSwitchPort() error {
	// Remove the patch port that connects join switch to gateway router
	lsp := nbdb.LogicalSwitchPort{Name: gw.getGWRouterPeerSwitchPortName()}
	sw := nbdb.LogicalSwitch{Name: gw.joinSwitchName}
	err := libovsdbops.DeleteLogicalSwitchPorts(gw.nbClient, &sw, &lsp)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete logical switch port %s from switch %s: %w", lsp.Name, sw.Name, err)
	}
	return nil
}

func (gw *GatewayManager) createGWRouterPeerRouterPort() error {
	gwPeerPortName := gw.getGWRouterPeerRouterPortName()
	gwRouterPortName := gw.getGWRouterPortName()

	ovnClusterRouterToGWRouterPort := nbdb.LogicalRouterPort{
		Name:     gwPeerPortName,
		MAC:      util.IPAddrToHWAddr(gw.transitRouterInfo.transitRouterNets[0].IP).String(),
		Networks: util.IPNetsToStringSlice(gw.transitRouterInfo.transitRouterNets),
		Options: map[string]string{
			libovsdbops.RequestedTnlKey: getTransitRouterPortTunnelKey(gw.transitRouterInfo.nodeID),
		},
		Peer: ptr.To(gwRouterPortName),
		ExternalIDs: map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		},
	}

	ovnClusterRouter := nbdb.LogicalRouter{Name: gw.clusterRouterName}
	err := libovsdbops.CreateOrUpdateLogicalRouterPort(gw.nbClient, &ovnClusterRouter,
		&ovnClusterRouterToGWRouterPort, nil, &ovnClusterRouterToGWRouterPort.MAC, &ovnClusterRouterToGWRouterPort.Networks,
		&ovnClusterRouterToGWRouterPort.Options, &ovnClusterRouterToGWRouterPort.Peer, &ovnClusterRouterToGWRouterPort.ExternalIDs)
	if err != nil {
		return fmt.Errorf("failed to create port %+v on router %+v: %v", ovnClusterRouterToGWRouterPort, ovnClusterRouter, err)
	}
	return nil
}

func (gw *GatewayManager) deleteGWRouterPeerRouterPort() error {
	ovnClusterRouterToGWRouterPort := nbdb.LogicalRouterPort{Name: gw.getGWRouterPeerRouterPortName()}
	ovnClusterRouter := nbdb.LogicalRouter{Name: gw.clusterRouterName}
	err := libovsdbops.DeleteLogicalRouterPorts(gw.nbClient, &ovnClusterRouter, &ovnClusterRouterToGWRouterPort)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete router port %s from router %s: %w", ovnClusterRouterToGWRouterPort.Name, ovnClusterRouter.Name, err)
	}
	return nil
}

func (gw *GatewayManager) createGWRouterPort(gwConfig *GatewayConfig,
	enableGatewayMTU bool, gwRouter *nbdb.LogicalRouter) error {
	gwLRPNetworks := []string{}
	for _, gwRouterJoinNet := range gwConfig.gwRouterJoinCIDRs {
		gwLRPNetworks = append(gwLRPNetworks, gwRouterJoinNet.String())
	}
	if gw.netInfo.TopologyType() == types.Layer2Topology && gw.transitRouterInfo == nil {
		// At layer2 GR LRP acts as the layer3 ovn_cluster_router so we need
		// to configure here the .1 address, this will work only for IC with
		// one node per zone, since ARPs for .1 will not go beyond local switch.
		// This is being done to add the ICMP SNATs for .1 podSubnet that OVN GR generates
		for _, subnet := range gwConfig.hostSubnets {
			gwLRPNetworks = append(gwLRPNetworks, gw.netInfo.GetNodeGatewayIP(subnet).String())
		}
	}
	if gw.netInfo.TopologyType() == types.Layer2Topology && gw.transitRouterInfo != nil {
		for _, gatewayRouterTransitNetwork := range gw.transitRouterInfo.gatewayRouterNets {
			gwLRPNetworks = append(gwLRPNetworks, gatewayRouterTransitNetwork.String())
		}
	}
	gwLRPMAC := util.IPAddrToHWAddr(gwConfig.gwRouterJoinCIDRs[0].IP)

	var options map[string]string
	if enableGatewayMTU {
		options = map[string]string{
			libovsdbops.GatewayMTU: strconv.Itoa(config.Default.MTU),
		}
	}

	gwRouterPort := nbdb.LogicalRouterPort{
		Name:     gw.getGWRouterPortName(),
		MAC:      gwLRPMAC.String(),
		Networks: gwLRPNetworks,
		Options:  options,
	}
	if gw.netInfo.IsUserDefinedNetwork() {
		gwRouterPort.ExternalIDs = map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		}
		if gw.netInfo.TopologyType() == types.Layer2Topology && gw.transitRouterInfo != nil {
			gwRouterPort.Peer = ptr.To(gw.getGWRouterPeerRouterPortName())
		}

		_, isNetIPv6 := gw.netInfo.IPMode()
		if gw.netInfo.TopologyType() == types.Layer2Topology && isNetIPv6 && config.IPv6Mode && gw.transitRouterInfo == nil {
			gwRouterPort.Ipv6RaConfigs = map[string]string{
				"address_mode":      "dhcpv6_stateful",
				"send_periodic":     "true",
				"max_interval":      "900", // 15 minutes
				"min_interval":      "300", // 5 minutes
				"router_preference": "LOW", // The static gateway configured by CNI is MEDIUM, so make this SLOW so it has less effect for pods
			}
			if gw.netInfo.MTU() > 0 {
				gwRouterPort.Ipv6RaConfigs["mtu"] = fmt.Sprintf("%d", gw.netInfo.MTU())
			}
		}
	}

	err := libovsdbops.CreateOrUpdateLogicalRouterPort(gw.nbClient, gwRouter,
		&gwRouterPort, nil, &gwRouterPort.MAC, &gwRouterPort.Networks,
		&gwRouterPort.Options)
	if err != nil {
		return fmt.Errorf("failed to create port %+v on router %+v: %v", gwRouterPort, gwRouter, err)
	}
	return nil
}

func (gw *GatewayManager) updateGWRouterStaticRoutes(gwConfig *GatewayConfig, externalRouterPort string,
	gwRouter *nbdb.LogicalRouter) error {
	if len(gwConfig.ovnClusterLRPToJoinIfAddrs) > 0 {
		// this is only the case for layer3 topology
		for _, entry := range gwConfig.clusterSubnets {
			drLRPIfAddr, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6CIDR(entry), gwConfig.ovnClusterLRPToJoinIfAddrs)
			if err != nil {
				return fmt.Errorf("failed to add a static route in GR %s with distributed "+
					"router as the nexthop: %v",
					gw.gwRouterName, err)
			}

			// TODO There has to be a better way to do this. It seems like the
			// whole purpose is to update the appropriate route in case it already
			// exists *only* in the context of this router. But then it does not
			// make sense to refresh it on every loop, unless it is also way to
			// check for duplicate cluster IP subnets for which there would also be
			// a better way to do it. Adding support for indirection in ModelClients
			// opModel (being able to operate on thins pointed to from another model)
			// would be a great way to simplify this.
			updatedGWRouter, err := libovsdbops.GetLogicalRouter(gw.nbClient, gwRouter)
			if err != nil {
				return fmt.Errorf("unable to retrieve logical router %+v: %v", gwRouter, err)
			}

			lrsr := nbdb.LogicalRouterStaticRoute{
				IPPrefix: entry.String(),
				Nexthop:  drLRPIfAddr.IP.String(),
			}
			if gw.netInfo.IsUserDefinedNetwork() {
				lrsr.ExternalIDs = map[string]string{
					types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
					types.TopologyExternalID: gw.netInfo.TopologyType(),
				}
			}
			p := func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.IPPrefix == lrsr.IPPrefix && libovsdbops.PolicyEqualPredicate(item.Policy, lrsr.Policy) &&
					util.SliceHasStringItem(updatedGWRouter.StaticRoutes, item.UUID)
			}
			err = libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(gw.nbClient, gw.gwRouterName, &lrsr, p,
				&lrsr.Nexthop)
			if err != nil {
				return fmt.Errorf("failed to add a static route %+v in GR %s with distributed router as the nexthop, err: %v", lrsr, gw.gwRouterName, err)
			}
		}
	}
	// for layer2 topology with transit router, add pod subnet routes via transit router, like so:
	// 10.10.0.0/24                100.88.0.8 dst-ip rtotr-GR_<network_name>
	if gw.netInfo.TopologyType() == types.Layer2Topology && gw.transitRouterInfo != nil {
		for _, subnet := range gwConfig.hostSubnets {
			nexthop, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6(subnet.IP), gw.transitRouterInfo.transitRouterNets)
			if err != nil {
				return err
			}
			subnetRoute := nbdb.LogicalRouterStaticRoute{
				IPPrefix:   subnet.String(),
				Nexthop:    nexthop.IP.String(),
				OutputPort: ptr.To(gw.getGWRouterPortName()),
			}
			subnetRoute.ExternalIDs = map[string]string{
				types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
				types.TopologyExternalID: gw.netInfo.TopologyType(),
			}
			p := func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.OutputPort != nil && *item.OutputPort == *subnetRoute.OutputPort && item.IPPrefix == subnetRoute.IPPrefix &&
					libovsdbops.PolicyEqualPredicate(subnetRoute.Policy, item.Policy)
			}
			if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(gw.nbClient, gw.gwRouterName, &subnetRoute,
				p, &subnetRoute.Nexthop); err != nil {
				return fmt.Errorf("error creating static route %+v in GW router %s: %v", subnetRoute, gw.gwRouterName, err)
			}
		}
	}

	for _, nextHop := range node.DummyNextHopIPs() {
		// Add return service route for OVN back to host
		prefix := config.Gateway.V4MasqueradeSubnet
		if utilnet.IsIPv6(nextHop) {
			prefix = config.Gateway.V6MasqueradeSubnet
		}
		lrsr := nbdb.LogicalRouterStaticRoute{
			IPPrefix:    prefix,
			Nexthop:     nextHop.String(),
			OutputPort:  &externalRouterPort,
			ExternalIDs: map[string]string{util.OvnNodeMasqCIDR: ""},
		}
		if gw.netInfo.IsUserDefinedNetwork() {
			lrsr.ExternalIDs = map[string]string{
				types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
				types.TopologyExternalID: gw.netInfo.TopologyType(),
			}
		}
		p := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.OutputPort != nil && *item.OutputPort == *lrsr.OutputPort && item.IPPrefix == lrsr.IPPrefix &&
				libovsdbops.PolicyEqualPredicate(item.Policy, lrsr.Policy)
		}
		err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(gw.nbClient, gw.gwRouterName, &lrsr, p,
			&lrsr.Nexthop)
		if err != nil {
			return fmt.Errorf("error creating service static route %+v in GR %s: %v", lrsr, gw.gwRouterName, err)
		}
	}

	nextHops := gwConfig.annoConfig.NextHops
	// Add default gateway routes in GR
	for _, nextHop := range nextHops {
		var allIPs string
		if utilnet.IsIPv6(nextHop) {
			allIPs = "::/0"
		} else {
			allIPs = "0.0.0.0/0"
		}

		lrsr := nbdb.LogicalRouterStaticRoute{
			IPPrefix:   allIPs,
			Nexthop:    nextHop.String(),
			OutputPort: &externalRouterPort,
		}
		if gw.netInfo.IsUserDefinedNetwork() {
			lrsr.ExternalIDs = map[string]string{
				types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
				types.TopologyExternalID: gw.netInfo.TopologyType(),
			}
		}
		p := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.OutputPort != nil && *item.OutputPort == *lrsr.OutputPort && item.IPPrefix == lrsr.IPPrefix &&
				libovsdbops.PolicyEqualPredicate(lrsr.Policy, item.Policy)
		}
		err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(gw.nbClient, gw.gwRouterName, &lrsr,
			p, &lrsr.Nexthop)
		if err != nil {
			return fmt.Errorf("error creating static route %+v in GR %s: %v", lrsr, gw.gwRouterName, err)
		}
	}
	return nil
}

func (gw *GatewayManager) updateClusterRouterStaticRoutes(gwConfig *GatewayConfig, gwRouterIPs []net.IP) error {
	// We need to add a route to the Gateway router's IP, on the
	// cluster router, to ensure that the return traffic goes back
	// to the same gateway router
	//
	// This can be removed once https://bugzilla.redhat.com/show_bug.cgi?id=1891516 is fixed.
	// FIXME(trozet): if LRP IP is changed, we do not remove stale instances of these routes
	nextHops := gwRouterIPs
	if gw.netInfo.TopologyType() == types.Layer2Topology && gw.transitRouterInfo != nil {
		nextHops = util.IPNetsToIPs(gw.transitRouterInfo.gatewayRouterNets)
	}

	for _, gwRouterIP := range gwRouterIPs {
		nextHop, err := util.MatchIPFamily(utilnet.IsIPv6(gwRouterIP), nextHops)
		if err != nil {
			if gw.transitRouterInfo != nil {
				// for layer2 networks with transit router it is not an error.
				// JoinIPs are allocated for both IP families always, but transit router IPs and routes
				// are only created for the actual IP families of the network
				continue
			}
			return fmt.Errorf("failed to add source IP address based "+
				"routes in distributed router %s: %v",
				gw.clusterRouterName, err)
		}

		lrsr := nbdb.LogicalRouterStaticRoute{
			IPPrefix: gwRouterIP.String(),
			Nexthop:  nextHop[0].String(),
		}
		if gw.netInfo.IsUserDefinedNetwork() {
			lrsr.ExternalIDs = map[string]string{
				types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
				types.TopologyExternalID: gw.netInfo.TopologyType(),
			}
		}
		p := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.IPPrefix == lrsr.IPPrefix &&
				libovsdbops.PolicyEqualPredicate(lrsr.Policy, item.Policy)
		}

		if gw.clusterRouterName != "" {
			err = libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(gw.nbClient,
				gw.clusterRouterName, &lrsr, p, &lrsr.Nexthop)
			if err != nil {
				return fmt.Errorf("error creating static route %+v in %s: %v", lrsr, gw.clusterRouterName, err)
			}
		}
	}
	if gw.clusterRouterName == "" {
		return nil
	}

	// Add source IP address based routes in distributed router
	// for this gateway router.
	for _, hostSubnet := range gwConfig.hostSubnets {
		nextHop, err := util.MatchIPFamily(utilnet.IsIPv6CIDR(hostSubnet), nextHops)
		if err != nil {
			return fmt.Errorf("failed to add source IP address based "+
				"routes in distributed router %s: %v",
				gw.clusterRouterName, err)
		}

		lrsr := nbdb.LogicalRouterStaticRoute{
			Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
			IPPrefix: hostSubnet.String(),
			Nexthop:  nextHop[0].String(),
		}

		if config.Gateway.Mode != config.GatewayModeLocal {
			if gw.netInfo.IsUserDefinedNetwork() {
				lrsr.ExternalIDs = map[string]string{
					types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
					types.TopologyExternalID: gw.netInfo.TopologyType(),
				}
			}
			p := func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.IPPrefix == lrsr.IPPrefix && libovsdbops.PolicyEqualPredicate(lrsr.Policy, item.Policy)
			}
			// If migrating from local to shared gateway, let's remove the static routes towards
			// management port interface for the hostSubnet prefix before adding the routes
			// towards join switch.
			mgmtIfAddr := gw.netInfo.GetNodeManagementIP(hostSubnet)
			gw.staticRouteCleanup([]net.IP{mgmtIfAddr.IP}, hostSubnet)

			if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(
				gw.nbClient,
				gw.clusterRouterName,
				&lrsr,
				p,
				&lrsr.Nexthop,
			); err != nil {
				return fmt.Errorf("error creating static route %+v in GR %s: %v", lrsr, gw.clusterRouterName, err)
			}
		} else if config.Gateway.Mode == config.GatewayModeLocal {
			// If migrating from shared to local gateway, let's remove the static routes towards
			// join switch for the hostSubnet prefix and any potential routes for UDN enabled services.
			// Note syncManagementPort happens before gateway sync so only remove things pointing to join subnet
			p := func(item *nbdb.LogicalRouterStaticRoute) bool {
				if _, ok := item.ExternalIDs[types.UDNEnabledServiceExternalID]; ok {
					return true
				}
				return item.IPPrefix == lrsr.IPPrefix && item.Policy != nil && *item.Policy == *lrsr.Policy &&
					gw.containsJoinIP(net.ParseIP(item.Nexthop))
			}
			err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(gw.nbClient, gw.clusterRouterName, p)
			if err != nil {
				return fmt.Errorf("error deleting static route %+v in GR %s: %v", lrsr, gw.clusterRouterName, err)
			}
		}
	}
	return nil
}

// syncNATsForGRIPChange updates the SNAT rules on the gateway router that are created outside the GatewayManager.
// Multiple handlers, like
// - DefaultNetworkController.addLogicalPort
// - DefaultNetworkController.updateNamespace
// - EgressIPController.addExternalGWPodSNATOps
// - EgressIPController.addPodEgressIPAssignment
// - Layer2UserDefinedNetworkController.buildUDNEgressSNAT
// - Layer3UserDefinedNetworkController.addUDNNodeSubnetEgressSNAT
// use gateway config parameters to create SNAT rules on the gateway router, but some of them (not all) don't watch
// gateway config changes and rely on the GatewayManager to update their SNAT rules.
// Is it racy? Yes!
// This function also updates SNAT created by `updateGWRouterNAT`, because NATs don't use ExternalIDs,
// and their fields are used to find equivalent NATs. That means on gateway IPs change, instead of updating
// the old NAT, we would create a new one. FIXME: add externalIDs to NATs
func (gw *GatewayManager) syncNATsForGRIPChange(gwConfig *GatewayConfig, oldExtIPs, gwRouterIPs []net.IP,
	gwRouter, oldGWRouter *nbdb.LogicalRouter) error {
	// if config.Gateway.DisabledSNATMultipleGWs is not set (by default it is not),
	// the NAT rules for pods not having annotations to route through either external
	// gws or pod CNFs will be added within pods.go addLogicalPort
	var natsToUpdate []*nbdb.NAT
	// If l3gatewayAnnotation.IPAddresses changed, we need to update the SNATs on the GR
	oldNATs := []*nbdb.NAT{}
	var err error
	if oldGWRouter != nil {
		oldNATs, err = libovsdbops.GetRouterNATs(gw.nbClient, oldGWRouter)
		if err != nil && errors.Is(err, libovsdbclient.ErrNotFound) {
			return fmt.Errorf("unable to get NAT entries for router %s: %w", oldGWRouter.Name, err)
		}
	}

	for _, nat := range oldNATs {
		nat := nat
		natModified := false

		// if not type snat, we don't need to update as we only configure snat types
		if nat.Type != nbdb.NATTypeSNAT {
			continue
		}

		// check external ip changed
		for _, externalIP := range gwConfig.externalIPs {
			oldExternalIP, err := util.MatchFirstIPFamily(utilnet.IsIPv6(externalIP), oldExtIPs)
			if err != nil {
				return fmt.Errorf("failed to update GW SNAT rule for pods on router %s error: %v", gw.gwRouterName, err)
			}
			if externalIP.String() == oldExternalIP.String() {
				// no external ip change, skip
				continue
			}
			if nat.ExternalIP == oldExternalIP.String() {
				// needs to be updated
				natModified = true
				nat.ExternalIP = externalIP.String()
			}

		}

		// note, nat.LogicalIP may be a CIDR or IP, we don't care unless it's an IP
		parsedLogicalIP := net.ParseIP(nat.LogicalIP)
		// check if join ip changed
		if gw.containsJoinIP(parsedLogicalIP) {
			// is a join SNAT, check if IP needs updating
			joinIP, err := util.MatchFirstIPFamily(utilnet.IsIPv6(parsedLogicalIP), gwRouterIPs)
			if err != nil {
				return fmt.Errorf("failed to find valid IP family match for join subnet IP: %s on "+
					"gateway router: %s, provided IPs: %#v", parsedLogicalIP, gw.gwRouterName, gwRouterIPs)
			}
			if nat.LogicalIP != joinIP.String() {
				// needs to be updated
				natModified = true
				nat.LogicalIP = joinIP.String()
			}
		}
		if natModified {
			natsToUpdate = append(natsToUpdate, nat)
		}
	}

	if len(natsToUpdate) > 0 {
		err = libovsdbops.CreateOrUpdateNATs(gw.nbClient, gwRouter, natsToUpdate...)
		if err != nil {
			return fmt.Errorf("failed to update GW SNAT rule for pod on router %s error: %v", gw.gwRouterName, err)
		}
	}
	return nil
}

func (gw *GatewayManager) updateGWRouterNAT(nodeName string, gwConfig *GatewayConfig, gwLRPIPs []net.IP, gwRouter *nbdb.LogicalRouter) error {
	// REMOVEME(trozet) workaround - create join subnet SNAT to handle ICMP needs frag return
	var extIDs map[string]string
	if gw.netInfo.IsUserDefinedNetwork() {
		extIDs = map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		}
	}
	joinNATs := make([]*nbdb.NAT, 0, len(gwLRPIPs))
	for _, gwLRPIP := range gwLRPIPs {
		externalIP, err := util.MatchIPFamily(utilnet.IsIPv6(gwLRPIP), gwConfig.externalIPs)
		if err != nil {
			return fmt.Errorf("failed to find valid external IP family match for join subnet IP: %s on "+
				"gateway router: %s", gwLRPIP, gw.gwRouterName)
		}
		joinIPNet, err := util.GetIPNetFullMask(gwLRPIP.String())
		if err != nil {
			return fmt.Errorf("failed to parse full CIDR mask for join subnet IP: %s", gwLRPIP)
		}
		nat := libovsdbops.BuildSNAT(&externalIP[0], joinIPNet, "", extIDs)
		joinNATs = append(joinNATs, nat)
	}
	err := libovsdbops.CreateOrUpdateNATs(gw.nbClient, gwRouter, joinNATs...)
	if err != nil {
		return fmt.Errorf("failed to create SNAT rule for join subnet on router %s error: %v", gw.gwRouterName, err)
	}

	nats := make([]*nbdb.NAT, 0, len(gwConfig.clusterSubnets))
	var nat *nbdb.NAT
	// DisableSNATMultipleGWs is only applicable to cluster default network and not to user defined networks.
	// For user defined networks, we always add SNAT rules regardless of whether the network is advertised or not.
	if !config.Gateway.DisableSNATMultipleGWs || gw.netInfo.IsPrimaryNetwork() {
		// Default SNAT rules. DisableSNATMultipleGWs=false in LGW (traffic egresses via mp0) always.
		// We are not checking for gateway mode to be shared explicitly to reduce topology differences.
		for _, entry := range gwConfig.clusterSubnets {
			externalIP, err := util.MatchIPFamily(utilnet.IsIPv6CIDR(entry), gwConfig.externalIPs)
			if err != nil {
				return fmt.Errorf("failed to create default SNAT rules for gateway router %s: %v",
					gw.gwRouterName, err)
			}

			// Get the match for this specific subnet's IP family
			ipFamily := utilnet.IPv4
			if utilnet.IsIPv6CIDR(entry) {
				ipFamily = utilnet.IPv6
			}
			snatMatch, err := GetNetworkScopedClusterSubnetSNATMatch(gw.nbClient, gw.netInfo, nodeName,
				gw.isRoutingAdvertised(nodeName), ipFamily)
			if err != nil {
				return fmt.Errorf("failed to get SNAT match for node %s for network %s: %w", nodeName, gw.netInfo.GetNetworkName(), err)
			}

			nat = libovsdbops.BuildSNATWithMatch(&externalIP[0], entry, "", extIDs, snatMatch)
			nats = append(nats, nat)
		}
		err = libovsdbops.CreateOrUpdateNATs(gw.nbClient, gwRouter, nats...)
		if err != nil {
			return fmt.Errorf("failed to update SNAT rule for pod on router %s error: %v", gw.gwRouterName, err)
		}
	} else {
		// ensure we do not have any leftover SNAT entries after an upgrade
		for _, logicalSubnet := range gwConfig.clusterSubnets {
			nat = libovsdbops.BuildSNAT(nil, logicalSubnet, "", extIDs)
			nats = append(nats, nat)
		}
		err = libovsdbops.DeleteNATs(gw.nbClient, gwRouter, nats...)
		if err != nil {
			return fmt.Errorf("failed to delete GW SNAT rule for pod on router %s error: %v", gw.gwRouterName, err)
		}
	}

	if err = gw.cleanupStalePodSNATs(nodeName, gwConfig.annoConfig.IPAddresses, gwLRPIPs); err != nil {
		return fmt.Errorf("failed to sync stale SNATs on node %s: %v", nodeName, err)
	}
	return nil
}

// gatewayInit creates a gateway router for the local chassis.
// enableGatewayMTU enables options:gateway_mtu for gateway routers.
func (gw *GatewayManager) gatewayInit(
	nodeName string,
	gwConfig *GatewayConfig,
	enableGatewayMTU bool,
) error {

	if gw.netInfo.TopologyType() == types.Layer2Topology && gw.clusterRouterName != "" {
		// layer2 network uses transit router, so we need to set the transit router info
		// in all the other operations we can use both `gw.clusterRouterName == ""` and `gw.transitRouterInfo == nil`
		// as an indicator of the old topology.
		err := gw.setTransitRouterInfo(nodeName)
		if err != nil {
			return fmt.Errorf("failed to initialize layer2 info for gateway on node %s: %v", nodeName, err)
		}
		if err = gw.oldLayer2TopoCleanup(); err != nil {
			return fmt.Errorf("failed to cleanup old layer2 topology for gateway on node %s: %v", nodeName, err)
		}
	}
	// If l3gatewayAnnotation.IPAddresses changed, we need to update the perPodSNATs,
	// so let's save the old value before we update the router for later use
	var oldExtIPs []net.IP
	oldLogicalRouter, err := libovsdbops.GetLogicalRouter(gw.nbClient,
		&nbdb.LogicalRouter{
			Name: gw.gwRouterName,
		})
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed in retrieving %s, error: %v", gw.gwRouterName, err)
	}

	if oldLogicalRouter != nil && oldLogicalRouter.ExternalIDs != nil {
		if physicalIPs, ok := oldLogicalRouter.ExternalIDs["physical_ips"]; ok {
			oldExternalIPs := strings.Split(physicalIPs, ",")
			oldExtIPs = make([]net.IP, len(oldExternalIPs))
			for i, oldExternalIP := range oldExternalIPs {
				cidr := oldExternalIP + util.GetIPFullMaskString(oldExternalIP)
				ip, _, err := net.ParseCIDR(cidr)
				if err != nil {
					return fmt.Errorf("invalid cidr:%s error: %v", cidr, err)
				}
				oldExtIPs[i] = ip
			}
		}
	}

	gwRouter, err := gw.createGWRouter(gwConfig)
	if err != nil {
		return err
	}

	if gw.netInfo.TopologyType() == types.Layer2Topology && gw.transitRouterInfo != nil {
		err = gw.createGWRouterPeerRouterPort()
	} else {
		err = gw.createGWRouterPeerSwitchPort(nodeName)
	}
	if err != nil {
		return err
	}

	err = gw.createGWRouterPort(gwConfig, enableGatewayMTU, gwRouter)
	if err != nil {
		return err
	}

	if err := gw.addExternalSwitch("",
		gwConfig.annoConfig.InterfaceID,
		gw.gwRouterName,
		gwConfig.annoConfig.MACAddress.String(),
		physNetName(gw.netInfo),
		gwConfig.annoConfig.IPAddresses,
		gwConfig.annoConfig.VLANID); err != nil {
		return err
	}

	if gwConfig.annoConfig.EgressGWInterfaceID != "" {
		if err := gw.addExternalSwitch(types.EgressGWSwitchPrefix,
			gwConfig.annoConfig.EgressGWInterfaceID,
			gw.gwRouterName,
			gwConfig.annoConfig.EgressGWMACAddress.String(),
			types.PhysicalNetworkExGwName,
			gwConfig.annoConfig.EgressGWIPAddresses,
			nil); err != nil {
			return err
		}
	}

	// Remove stale OVN resources with any old masquerade IP
	if err := deleteStaleMasqueradeResources(gw.nbClient, gw.gwRouterName, nodeName, gw.watchFactory); err != nil {
		return fmt.Errorf("failed to remove stale masquerade resources from northbound database: %w", err)
	}

	if err := gateway.CreateDummyGWMacBindings(gw.nbClient, gw.gwRouterName, gw.netInfo); err != nil {
		return err
	}

	externalRouterPort := types.GWRouterToExtSwitchPrefix + gw.gwRouterName
	if err = gw.updateGWRouterStaticRoutes(gwConfig, externalRouterPort, gwRouter); err != nil {
		return err
	}

	gwRouterIPs := util.IPNetsToIPs(gwConfig.gwRouterJoinCIDRs)
	if err = gw.updateClusterRouterStaticRoutes(gwConfig, gwRouterIPs); err != nil {
		return err
	}

	if err = gw.syncNATsForGRIPChange(gwConfig, oldExtIPs, gwRouterIPs, gwRouter, oldLogicalRouter); err != nil {
		return err
	}

	if err = gw.updateGWRouterNAT(nodeName, gwConfig, gwRouterIPs, gwRouter); err != nil {
		return err
	}

	// recording gateway mode metrics here after gateway setup is done
	metrics.RecordEgressRoutingViaHost()

	return nil
}

// GetNetworkScopedClusterSubnetSNATMatch returns the match for the SNAT rule for the cluster default network
// and the match for the SNAT rule for the L3/L2 user defined network.
// If the network is not advertised:
// - For Layer2 topology, the match is the output port of the GR to the join switch since in L2 there is only 1 router but two cSNATs.
// - For Layer3 topology, the match is empty.
// If the network is advertised:
// - For Layer2 topology, the match is the output port of the GR to the join switch and the destination must be a nodeIP in the cluster.
// - For Layer3 topology, the match is the destination must be a nodeIP in the cluster.
func GetNetworkScopedClusterSubnetSNATMatch(nbClient libovsdbclient.Client, netInfo util.NetInfo, nodeName string,
	isNetworkAdvertised bool, ipFamily utilnet.IPFamily) (string, error) {
	layer2OldTopo := netInfo.TopologyType() == types.Layer2Topology && !config.Layer2UsesTransitRouter
	if !isNetworkAdvertised {
		if !layer2OldTopo {
			return "", nil
		}
		return fmt.Sprintf("outport == %q", types.GWRouterToExtSwitchPrefix+netInfo.GetNetworkScopedGWRouterName(nodeName)), nil
	}

	// if the network is advertised, we need to ensure that the SNAT exists with the correct conditional destination match
	dbIDs := getEgressIPAddrSetDbIDs(NodeIPAddrSetName, types.DefaultNetworkName, DefaultNetworkControllerName)
	addressSetFactory := addressset.NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)
	addrSet, err := addressSetFactory.GetAddressSet(dbIDs)
	if err != nil {
		return "", fmt.Errorf("cannot ensure that addressSet %v exists: %w", dbIDs, err)
	}
	destinationMatch := getClusterNodesDestinationBasedSNATMatch(ipFamily, addrSet)
	if destinationMatch == "" {
		return "", fmt.Errorf("could not build a destination based SNAT match because no addressSet %v exists for IP family %v", dbIDs, ipFamily)
	}
	if !layer2OldTopo {
		return destinationMatch, nil
	}
	return fmt.Sprintf("outport == %q && %s", types.GWRouterToExtSwitchPrefix+netInfo.GetNetworkScopedGWRouterName(nodeName), destinationMatch), nil
}

// addExternalSwitch creates a switch connected to the external bridge and connects it to
// the gateway router
func (gw *GatewayManager) addExternalSwitch(prefix, interfaceID, gatewayRouter, macAddress, physNetworkName string, ipAddresses []*net.IPNet, vlanID *uint) error {
	// Create the GR port that connects to external_switch with mac address of
	// external interface and that IP address. In the case of `local` gateway
	// mode, whenever ovnkube-node container restarts a new br-local bridge will
	// be created with a new `nicMacAddress`.
	externalRouterPort := prefix + types.GWRouterToExtSwitchPrefix + gatewayRouter

	externalRouterPortNetworks := []string{}
	for _, ip := range ipAddresses {
		externalRouterPortNetworks = append(externalRouterPortNetworks, ip.String())
	}
	externalLogicalRouterPort := nbdb.LogicalRouterPort{
		MAC: macAddress,
		ExternalIDs: map[string]string{
			"gateway-physical-ip": "yes",
		},
		Networks: externalRouterPortNetworks,
		Name:     externalRouterPort,
	}
	if gw.netInfo.IsUserDefinedNetwork() {
		externalLogicalRouterPort.ExternalIDs = map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		}
	}
	logicalRouter := nbdb.LogicalRouter{Name: gatewayRouter}

	err := libovsdbops.CreateOrUpdateLogicalRouterPort(gw.nbClient, &logicalRouter,
		&externalLogicalRouterPort, nil, &externalLogicalRouterPort.MAC,
		&externalLogicalRouterPort.Networks, &externalLogicalRouterPort.ExternalIDs,
		&externalLogicalRouterPort.Options)
	if err != nil {
		return fmt.Errorf("failed to add logical router port %+v to router %s: %v", externalLogicalRouterPort, gatewayRouter, err)
	}

	// Create the external switch for the physical interface to connect to
	// and add external interface as a logical port to external_switch.
	// This is a learning switch port with "unknown" address. The external
	// world is accessed via this port.
	externalSwitch := prefix + gw.extSwitchName
	externalLogicalSwitchPort := nbdb.LogicalSwitchPort{
		Addresses: []string{"unknown"},
		Type:      "localnet",
		Options: map[string]string{
			"network_name": physNetworkName,
		},
		Name: interfaceID,
	}
	if gw.netInfo.IsUserDefinedNetwork() {
		externalLogicalSwitchPort.ExternalIDs = map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		}
	}

	if vlanID != nil && int(*vlanID) != 0 {
		intVlanID := int(*vlanID)
		externalLogicalSwitchPort.TagRequest = &intVlanID
	}

	// Also add the port to connect the external_switch to the router.
	externalSwitchPortToRouter := prefix + types.EXTSwitchToGWRouterPrefix + gatewayRouter
	externalLogicalSwitchPortToRouter := nbdb.LogicalSwitchPort{
		Name: externalSwitchPortToRouter,
		Type: "router",
		Options: map[string]string{
			libovsdbops.RouterPort: externalRouterPort,

			// This option will program OVN to start sending GARPs for all external IPS
			// that the logical switch port has been configured to use. This is
			// necessary for egress IP because if an egress IP is moved between two
			// nodes, the nodes need to actively update the ARP cache of all neighbors
			// as to notify them the change. If this is not the case: packets will
			// continue to be routed to the old node which hosted the egress IP before
			// it was moved, and the connections will fail.
			"nat-addresses": "router",

			// Setting nat-addresses to router will send out GARPs for all externalIPs and LB VIPs
			// hosted on the GR. Setting exclude-lb-vips-from-garp to true will make sure GARPs for
			// LB VIPs are not sent, thereby preventing GARP overload.
			"exclude-lb-vips-from-garp": "true",
		},
		Addresses: []string{macAddress},
	}

	if gw.netInfo.IsUserDefinedNetwork() {
		externalLogicalSwitchPortToRouter.ExternalIDs = map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		}
	}
	sw := nbdb.LogicalSwitch{Name: externalSwitch}
	if gw.netInfo.IsUserDefinedNetwork() {
		sw.ExternalIDs = util.GenerateExternalIDsForSwitchOrRouter(gw.netInfo)
	}

	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsAndSwitch(gw.nbClient, &sw, &externalLogicalSwitchPort, &externalLogicalSwitchPortToRouter)
	if err != nil {
		return fmt.Errorf("failed to create logical switch ports %+v, %+v, and switch %s: %v",
			externalLogicalSwitchPort, externalLogicalSwitchPortToRouter, externalSwitch, err)
	}

	return nil
}

// cleanupStaleMasqueradeData removes following from northbound database
//   - LogicalRouterStaticRoute for rtoe-<GW_router> OutputPort anf IPPrefix is same as v4 or v6
//     StaleMasqueradeSubnet
//   - StaticMACBinding for rtoe-<GW_router> LogicalPort and referencing old DummyNextHopMasqueradeIP
func deleteStaleMasqueradeResources(nbClient libovsdbclient.Client, routerName, nodeName string, wf *factory.WatchFactory) error {
	var staleMasqueradeIPs config.MasqueradeIPsConfig
	var nextHops []net.IP
	logicalport := types.GWRouterToExtSwitchPrefix + routerName

	// we first examine the kapi node to see if we can determine if there is a stale masquerade subnet
	// if the masquerade subnet on the kapi node matches whats configured for this controller, it
	// doesn't necessarily mean there is not still a stale masquerade config in nbdb because
	// node could have already updated before this process runs.
	// As a backup, if we don't find the positive match in kapi, we execute a negative lookup in NBDB

	node, err := wf.GetNode(nodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// node doesn't exist for some reason, assume we should still try to clean up with auto-detection
			if err := deleteStaleMasqueradeRouteAndMACBinding(nbClient, routerName, nextHops); err != nil {
				return fmt.Errorf("failed to remove stale MAC binding and static route for logical port %s: %w", logicalport, err)
			}
			return nil
		}
		return err
	}

	subnets, err := util.ParseNodeMasqueradeSubnet(node)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			// no annotation set, must be initial bring up, nothing to clean
			return nil
		}
		return err
	}

	var v4ConfiguredMasqueradeNet, v6ConfiguredMasqueradeNet *net.IPNet

	for _, subnet := range subnets {
		if utilnet.IsIPv6CIDR(subnet) {
			v6ConfiguredMasqueradeNet = subnet
		} else if utilnet.IsIPv4CIDR(subnet) {
			v4ConfiguredMasqueradeNet = subnet
		} else {
			return fmt.Errorf("invalid subnet for masquerade annotation: %s", subnet)
		}
	}

	// Check for KAPI telling us there is a stale masquerade
	if v4ConfiguredMasqueradeNet != nil && config.Gateway.V4MasqueradeSubnet != v4ConfiguredMasqueradeNet.String() {
		if err := config.AllocateV4MasqueradeIPs(v4ConfiguredMasqueradeNet.IP, &staleMasqueradeIPs); err != nil {
			return fmt.Errorf("unable to determine stale V4MasqueradeIPs: %s", err)
		}
		nextHops = append(nextHops, staleMasqueradeIPs.V4DummyNextHopMasqueradeIP)
	}

	if v6ConfiguredMasqueradeNet != nil && config.Gateway.V6MasqueradeSubnet != v6ConfiguredMasqueradeNet.String() {
		if err := config.AllocateV6MasqueradeIPs(v6ConfiguredMasqueradeNet.IP, &staleMasqueradeIPs); err != nil {
			return fmt.Errorf("unable to determine stale V6MasqueradeIPs: %s", err)
		}
		nextHops = append(nextHops, staleMasqueradeIPs.V6DummyNextHopMasqueradeIP)
	}

	if err := deleteStaleMasqueradeRouteAndMACBinding(nbClient, routerName, nextHops); err != nil {
		return fmt.Errorf("failed to remove stale MAC binding and static route for logical port %s: %w", logicalport, err)
	}

	return nil
}

// deleteStaleMasqueradeRouteAndMACBinding will attempt to remove the corresponding routes and MAC bindings given the
// list of nextHopIPs. If nextHopIPs is empty, then an attempt will be made to detect the stale route and MAC bindings
func deleteStaleMasqueradeRouteAndMACBinding(nbClient libovsdbclient.Client, routerName string, nextHopIPs []net.IP) error {
	logicalport := types.GWRouterToExtSwitchPrefix + routerName
	if len(nextHopIPs) == 0 {
		// build valid values
		validNextHops := []net.IP{config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP, config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP}
		// lookup routes for external id that dont match currently configured masquerade subnets
		for _, validNextHop := range validNextHops {
			staticRoutePredicate := func(item *nbdb.LogicalRouterStaticRoute) bool {
				if item.OutputPort != nil && *item.OutputPort == logicalport &&
					item.Nexthop != validNextHop.String() && utilnet.IPFamilyOfString(item.Nexthop) == utilnet.IPFamilyOf(validNextHop) {
					if _, ok := item.ExternalIDs[util.OvnNodeMasqCIDR]; ok {
						return true
					}
				}
				return false
			}

			staleRoutes, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(nbClient, staticRoutePredicate)
			if err != nil {
				return fmt.Errorf("failed to search for stale masquerade routes: %w", err)
			}

			for _, staleRoute := range staleRoutes {
				klog.Infof("Stale masquerade route found: %#v", *staleRoute)
				// found stale routes, derive nexthop and flush the route and mac binding if it exists
				staleNextHop := staleRoute.Nexthop

				macBindingPredicate := func(item *nbdb.StaticMACBinding) bool {
					return item.LogicalPort == logicalport && item.IP == staleNextHop &&
						utilnet.IPFamilyOfString(item.IP) == utilnet.IPFamilyOfString(staleNextHop)
				}
				if err := libovsdbops.DeleteStaticMACBindingWithPredicate(nbClient, macBindingPredicate); err != nil {
					return fmt.Errorf("failed to delete static MAC binding for logical port %s: %v", logicalport, err)
				}
			}
			if err := libovsdbops.DeleteLogicalRouterStaticRoutes(nbClient, routerName, staleRoutes...); err != nil {
				return err
			}
		}
		return nil
	}

	for _, nextHop := range nextHopIPs {
		staticRoutePredicate := func(item *nbdb.LogicalRouterStaticRoute) bool {
			if item.OutputPort != nil && *item.OutputPort == logicalport &&
				item.Nexthop == nextHop.String() && utilnet.IPFamilyOfString(item.Nexthop) == utilnet.IPFamilyOf(nextHop) {
				if _, ok := item.ExternalIDs[util.OvnNodeMasqCIDR]; ok {
					return true
				}
			}
			return false
		}
		if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(nbClient, routerName, staticRoutePredicate); err != nil {
			return fmt.Errorf("failed to delete static route from gateway router %s: %v", routerName, err)
		}

		macBindingPredicate := func(item *nbdb.StaticMACBinding) bool {
			return item.LogicalPort == logicalport && item.IP == nextHop.String() &&
				utilnet.IPFamilyOfString(item.IP) == utilnet.IPFamilyOf(nextHop)
		}
		if err := libovsdbops.DeleteStaticMACBindingWithPredicate(nbClient, macBindingPredicate); err != nil {
			return fmt.Errorf("failed to delete static MAC binding for logical port %s: %v", logicalport, err)
		}
	}
	return nil
}

// Cleanup removes all the NB DB objects created for a node's gateway
func (gw *GatewayManager) Cleanup() error {
	// Get the gateway router port's IP address (connected to join switch)
	var nextHops []net.IP

	gwRouterPortName := gw.getGWRouterPortName()

	gwIPAddrs, err := libovsdbutil.GetLRPAddrs(gw.nbClient, gwRouterPortName)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf(
			"failed to get gateway IPs for network %q from LRP %s: %v",
			gw.netInfo.GetNetworkName(),
			gwRouterPortName,
			err,
		)
	}

	for _, gwIPAddr := range gwIPAddrs {
		nextHops = append(nextHops, gwIPAddr.IP)
	}
	gw.staticRouteCleanup(nextHops, nil)
	gw.policyRouteCleanup(nextHops)

	if gw.netInfo.TopologyType() == types.Layer2Topology && gw.transitRouterInfo != nil {
		err = gw.deleteGWRouterPeerRouterPort()
	} else {
		err = gw.deleteGWRouterPeerSwitchPort()
	}
	if err != nil {
		return err
	}

	// Remove the static mac bindings of the gateway router
	err = gateway.DeleteDummyGWMacBindings(gw.nbClient, gw.gwRouterName, gw.netInfo)
	if err != nil {
		return fmt.Errorf("failed to delete GR dummy mac bindings for node %s: %w", gw.nodeName, err)
	}

	// Remove the gateway router associated with nodeName
	logicalRouter := nbdb.LogicalRouter{Name: gw.gwRouterName}
	err = libovsdbops.DeleteLogicalRouter(gw.nbClient, &logicalRouter)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete gateway router %s: %w", gw.gwRouterName, err)
	}

	// Remove external switch
	err = libovsdbops.DeleteLogicalSwitch(gw.nbClient, gw.extSwitchName)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete external switch %s: %w", gw.extSwitchName, err)
	}

	exGWexternalSwitch := types.EgressGWSwitchPrefix + gw.extSwitchName
	err = libovsdbops.DeleteLogicalSwitch(gw.nbClient, exGWexternalSwitch)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete external switch %s: %w", exGWexternalSwitch, err)
	}

	// This will cleanup the NodeSubnetPolicy in local and shared gateway modes. It will be a no-op for any other mode.
	gw.delPbrAndNatRules(gw.nodeName)
	return nil
}

// NewGatewayManagerForCleanup returns a minimal GatewayManager used only for Cleanup(). Used when
// discovering gateway routers from the DB (e.g. stale cleanup when nodes are gone). layer2UseTransitRouter
// selects the peer port cleanup path (transit router LRP vs join switch LSP).
//
// NOTE: transitRouterInfo is set to an empty struct (not nil) when layer2UseTransitRouter is true.
// This is safe because Cleanup() only checks (transitRouterInfo != nil) to choose between
// deleteGWRouterPeerRouterPort and deleteGWRouterPeerSwitchPort — neither of which accesses
// transitRouterInfo fields. If Cleanup() is ever changed to dereference transitRouterInfo fields,
// this constructor must be updated accordingly.
func NewGatewayManagerForCleanup(
	nbClient libovsdbclient.Client,
	netInfo util.NetInfo,
	clusterRouterName, joinSwitchName, gwRouterName, nodeName string,
	layer2UseTransitRouter bool,
) *GatewayManager {
	var tri *transitRouterInfo
	if layer2UseTransitRouter {
		tri = &transitRouterInfo{}
	}
	return &GatewayManager{
		nodeName:          nodeName,
		clusterRouterName: clusterRouterName,
		gwRouterName:      gwRouterName,
		extSwitchName:     netInfo.GetNetworkScopedExtSwitchName(nodeName),
		joinSwitchName:    joinSwitchName,
		nbClient:          nbClient,
		netInfo:           netInfo,
		transitRouterInfo: tri,
	}
}

func (gw *GatewayManager) delPbrAndNatRules(nodeName string) {
	// delete the dnat_and_snat entry that we added for the management port IP
	// Note: we don't need to delete any MAC bindings that are dynamically learned from OVN SB DB
	// because there will be none since this NAT is only for outbound traffic and not for inbound
	mgmtPortName := util.GetK8sMgmtIntfName(gw.netInfo.GetNetworkScopedName(nodeName))
	nat := libovsdbops.BuildDNATAndSNAT(nil, nil, mgmtPortName, "", nil)
	logicalRouter := nbdb.LogicalRouter{
		Name: gw.clusterRouterName,
	}
	err := libovsdbops.DeleteNATs(gw.nbClient, &logicalRouter, nat)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		klog.Errorf("Failed to delete the dnat_and_snat associated with the management port %s: %v", mgmtPortName, err)
	}

	// delete all logical router policies on ovn_cluster_router
	gw.removeLRPolicies(nodeName)
}

func (gw *GatewayManager) staticRouteCleanup(nextHops []net.IP, ipPrefix *net.IPNet) {
	if len(nextHops) == 0 {
		return // if we do not have next hops, we do not have any routes to cleanup
	}
	ips := sets.Set[string]{}
	for _, nextHop := range nextHops {
		ips.Insert(nextHop.String())
	}
	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		networkName, isUserDefinedNetwork := item.ExternalIDs[types.NetworkExternalID]
		if !isUserDefinedNetwork {
			networkName = types.DefaultNetworkName
		}
		if networkName != gw.netInfo.GetNetworkName() {
			return false
		}
		if ipPrefix != nil && item.IPPrefix != ipPrefix.String() {
			return false
		}
		return ips.Has(item.Nexthop)
	}
	err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(gw.nbClient, gw.clusterRouterName, p)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		klog.Errorf("Failed to delete static route for nexthops %+v: %v", ips.UnsortedList(), err)
	}
}

// policyRouteCleanup cleans up all policies on cluster router that have a nextHop
// in the provided list.
// - if the LRP exists and has the len(nexthops) > 1: it removes
// the specified gatewayRouterIP from nexthops
// - if the LRP exists and has the len(nexthops) == 1: it removes
// the LRP completely
func (gw *GatewayManager) policyRouteCleanup(nextHops []net.IP) {
	for _, nextHop := range nextHops {
		gwIP := nextHop.String()
		policyPred := func(item *nbdb.LogicalRouterPolicy) bool {
			networkName, isUserDefinedNetwork := item.ExternalIDs[types.NetworkExternalID]
			if !isUserDefinedNetwork {
				networkName = types.DefaultNetworkName
			}
			if networkName != gw.netInfo.GetNetworkName() {
				return false
			}
			for _, nexthop := range item.Nexthops {
				if nexthop == gwIP {
					return true
				}
			}
			return false
		}
		err := libovsdbops.DeleteNextHopFromLogicalRouterPoliciesWithPredicate(gw.nbClient, gw.clusterRouterName, policyPred, gwIP)
		if err != nil && err != libovsdbclient.ErrNotFound {
			klog.Errorf("Failed to delete policy route from router %q for nexthop %+v: %v", gw.clusterRouterName, nextHop, err)
		}
	}
}

// remove Logical Router Policy on ovn_cluster_router for a specific node.
// Specify priorities to only delete specific types
func (gw *GatewayManager) removeLRPolicies(nodeName string) {
	priorities := []string{types.NodeSubnetPolicyPriority}

	intPriorities := sets.Set[int]{}
	for _, priority := range priorities {
		intPriority, _ := strconv.Atoi(priority)
		intPriorities.Insert(intPriority)
	}

	managedNetworkName := gw.netInfo.GetNetworkName()
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		networkName, isUserDefinedNetwork := item.ExternalIDs[types.NetworkExternalID]
		if !isUserDefinedNetwork {
			networkName = types.DefaultNetworkName
		}
		if networkName != managedNetworkName {
			return false
		}
		return strings.Contains(item.Match, fmt.Sprintf("%s ", nodeName)) && intPriorities.Has(item.Priority)
	}
	err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(gw.nbClient, gw.clusterRouterName, p)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		klog.Errorf("Error deleting policies for network %q with priorities %v associated with the node %s: %v", gw.netInfo.GetNetworkName(), priorities, nodeName, err)
	}
}

func (gw *GatewayManager) containsJoinIP(ip net.IP) bool {
	ipNet := &net.IPNet{
		IP:   ip,
		Mask: util.GetIPFullMask(ip),
	}
	return util.IsContainedInAnyCIDR(ipNet, gw.netInfo.JoinSubnets()...)
}

func (gw *GatewayManager) isRoutingAdvertised(node string) bool {
	return util.IsPodNetworkAdvertisedAtNode(gw.netInfo, node)
}

// SyncGateway ensures a node's gateway router is configured according to the L3 config and host subnets
func (gw *GatewayManager) SyncGateway(
	node *corev1.Node,
	gwConfig *GatewayConfig,
) error {
	if gwConfig.annoConfig.Mode == config.GatewayModeDisabled {
		if err := gw.Cleanup(); err != nil {
			return fmt.Errorf("error cleaning up gateway for node %s: %v", node.Name, err)
		}
		return nil
	}
	if gwConfig.hostSubnets == nil {
		return nil
	}

	enableGatewayMTU := util.ParseNodeGatewayMTUSupport(node)

	err := gw.gatewayInit(
		node.Name,
		gwConfig,
		enableGatewayMTU,
	)
	if err != nil {
		return fmt.Errorf("failed to init gateway for network %q: %v", gw.netInfo.GetNetworkName(), err)
	}

	routerName := gw.clusterRouterName
	if gw.clusterRouterName == "" {
		routerName = gw.gwRouterName
	}
	for _, subnet := range gwConfig.hostSubnets {
		mgmtIfAddr := gw.netInfo.GetNodeManagementIP(subnet)
		if mgmtIfAddr == nil {
			return fmt.Errorf("management interface address not found for subnet %q on network %q", subnet, gw.netInfo.GetNetworkName())
		}
		l3GatewayConfigIP, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6(mgmtIfAddr.IP), gwConfig.annoConfig.IPAddresses)
		if err != nil {
			return fmt.Errorf("failed to extract the gateway IP addr for network %q: %v", gw.netInfo.GetNetworkName(), err)
		}
		relevantHostIPs, err := util.MatchAllIPStringFamily(utilnet.IsIPv6(mgmtIfAddr.IP), gwConfig.hostAddrs)
		if err != nil && err != util.ErrorNoIP {
			return fmt.Errorf("failed to extract the host IP addrs for network %q: %v", gw.netInfo.GetNetworkName(), err)
		}
		pbrMngr := gatewayrouter.NewPolicyBasedRoutesManager(gw.nbClient, routerName, gw.netInfo)
		if err := pbrMngr.AddSameNodeIPPolicy(node.Name, mgmtIfAddr.IP.String(), l3GatewayConfigIP, relevantHostIPs); err != nil {
			return fmt.Errorf("failed to configure the policy based routes for network %q: %v", gw.netInfo.GetNetworkName(), err)
		}
		if gw.netInfo.TopologyType() == types.Layer2Topology && gw.transitRouterInfo == nil && config.Gateway.Mode == config.GatewayModeLocal {
			if err := pbrMngr.AddHostCIDRPolicy(node, mgmtIfAddr.IP.String(), subnet.String()); err != nil {
				return fmt.Errorf("failed to configure the hostCIDR policy for L2 network %q on local gateway: %v",
					gw.netInfo.GetNetworkName(), err)
			}
		}
	}

	return nil
}

func physNetName(netInfo util.NetInfo) string {
	if netInfo.IsDefault() || netInfo.IsPrimaryNetwork() {
		return types.PhysicalNetworkName
	}
	return netInfo.GetNetworkName()
}

func (gw *GatewayManager) setTransitRouterInfo(nodeName string) error {
	node, err := gw.watchFactory.GetNode(nodeName)
	if err != nil {
		return err
	}
	gw.transitRouterInfo, err = getTransitRouterInfo(gw.netInfo, node)
	if err != nil {
		return err
	}
	return nil
}

// oldLayer2TopoCleanup cleans up the old layer2 topology for the gateway on the node.
// Idempotent, will check if nbdb needs cleanup.
func (gw *GatewayManager) oldLayer2TopoCleanup() error {
	// Check if the stale gateway router port exists.
	// We delete GR a last operation in this cleanup, hence if it doesn't exist, we can skip the cleanup.
	gwRouterPort := &nbdb.LogicalRouterPort{
		Name: types.RouterToSwitchPrefix + gw.joinSwitchName,
	}
	var err error
	gwRouterPort, err = libovsdbops.GetLogicalRouterPort(gw.nbClient, gwRouterPort)
	if err != nil && errors.Is(err, libovsdbclient.ErrNotFound) {
		// cleanup not needed, old port does not exist
		return nil
	}

	// 1. Delete old port from the switch
	if err := gw.deleteGWRouterPeerSwitchPort(); err != nil {
		return fmt.Errorf("failed to delete peer switch port %s: %v", gw.getGWRouterPeerSwitchPortName(), err)
	}
	// 2. Remove the static mac bindings of the gateway router (otherwise you can't delete the router)
	err = gateway.DeleteDummyGWMacBindings(gw.nbClient, gw.gwRouterName, gw.netInfo)
	if err != nil {
		return fmt.Errorf("failed to delete GR dummy mac bindings for node %s: %w", gw.nodeName, err)
	}

	// 3. Delete stale GR, this will remove stale ports, NATs, routes and routing policies
	if err := libovsdbops.DeleteLogicalRouter(gw.nbClient, &nbdb.LogicalRouter{Name: gw.gwRouterName}); err != nil {
		return fmt.Errorf("failed to delete GR port %s: %v", gwRouterPort.Name, err)
	}
	return nil
}
