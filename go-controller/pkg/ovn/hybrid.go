// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	hotypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	hybridCidrClusterKey = "cluster"
)

// handleHybridOverlayPort reconciles the node's overlay port with OVN.
// It needs to handle the following cases:
//   - no subnet allocated: unset MAC annotation
//   - no MAC annotation, no lsp: configure lsp, set annotation
//   - annotation, no lsp: configure lsp
//   - annotation, lsp: ensure lsp matches annotation
//   - no annotation, lsp: set annotation from lsp
func (oc *DefaultNetworkController) handleHybridOverlayPort(node *corev1.Node, annotator kube.Annotator) error {
	var err error
	var annotationMAC, portMAC net.HardwareAddr
	var drIP net.IP
	portName := util.GetHybridOverlayPortName(node.Name)

	// retrieve mac annotation
	am, annotationOK := node.Annotations[hotypes.HybridOverlayDRMAC]
	if annotationOK {
		annotationMAC, err = net.ParseMAC(am)
		if err != nil {
			klog.Errorf("MAC annotation %s on node %s is invalid, ignoring.", annotationMAC, node.Name)
			annotationOK = false
		}
	}

	// no subnet allocated? unset mac annotation, be done.
	subnets, err := util.ParseNodeHostSubnetAnnotation(node, ovntypes.DefaultNetworkName)
	if subnets == nil || err != nil {
		// No subnet allocated yet; clean up
		klog.V(5).Infof("No subnet allocation yet for %s", node.Name)
		if annotationOK {
			if err := oc.deleteHybridOverlayPort(node); err != nil {
				return err
			}
			annotator.Delete(hotypes.HybridOverlayDRMAC)
		}
		return nil
	}

	// Retrieve port MAC address; if the port isn't set up, portMAC will be nil
	lsp := &nbdb.LogicalSwitchPort{Name: portName}
	lsp, err = libovsdbops.GetLogicalSwitchPort(oc.nbClient, lsp)
	if err == nil {
		portMAC, _, _ = libovsdbutil.ExtractPortAddresses(lsp)
	}

	// compare port configuration to annotation MAC, reconcile as needed
	lspOK := false

	if drIP = net.ParseIP(node.Annotations[hotypes.HybridOverlayDRIP]); drIP == nil {
		return fmt.Errorf("cannot set up hybrid overlay ports, distributed router ip is nil")
	}
	// nothing allocated, allocate mac from HybridOverlayDRIP
	if portMAC == nil && annotationMAC == nil {
		portMAC = util.IPAddrToHWAddr(drIP)
		annotationMAC = portMAC
		klog.V(5).Infof("Allocating MAC %s to node %s", portMAC.String(), node.Name)
	} else if portMAC == nil && annotationMAC != nil { // annotation, no port
		portMAC = annotationMAC
	} else if portMAC != nil && annotationMAC == nil { // port, no annotation
		lspOK = true
		annotationMAC = portMAC
	} else if portMAC != nil && annotationMAC != nil { // port & annotation: anno wins
		if portMAC.String() != annotationMAC.String() {
			klog.V(2).Infof("Warning: node %s lsp %s has mismatching hybrid port mac, correcting", node.Name, portName)
			portMAC = annotationMAC
		} else {
			lspOK = true
		}
	}

	// we need to setup a reroute policy for hybrid overlay subnet
	// this is so hybrid pod -> service -> hybrid endpoint will reroute to the DR IP
	if err := oc.setupHybridLRPolicySharedGw(subnets, node.Name, portMAC, drIP); err != nil {
		return fmt.Errorf("unable to setup Hybrid Subnet Logical Route Policy for node: %s, error: %w",
			node.Name, err)
	}

	if !lspOK {
		klog.Infof("Creating / updating node %s hybrid overlay port with mac %s", node.Name, portMAC.String())

		// create / update lsps
		lsp := nbdb.LogicalSwitchPort{
			Name:      portName,
			Addresses: []string{portMAC.String()},
		}
		sw := nbdb.LogicalSwitch{Name: oc.GetNetworkScopedSwitchName(node.Name)}

		err := libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(oc.nbClient, &sw, &lsp)
		if err != nil {
			return fmt.Errorf("failed to add hybrid overlay port %+v for node %s: %w", lsp, node.Name, err)
		}
		for _, subnet := range subnets {
			if err := libovsdbutil.UpdateNodeSwitchExcludeIPs(oc.nbClient, oc.GetNetworkScopedK8sMgmtIntfName(node.Name), oc.GetNetworkScopedSwitchName(node.Name), node.Name, subnet, oc.GetNodeManagementIP(subnet)); err != nil {
				return err
			}
		}
	}

	if !annotationOK {
		klog.Infof("Setting node %s hybrid overlay mac annotation to %s", node.Name, annotationMAC.String())
		if err := annotator.Set(hotypes.HybridOverlayDRMAC, portMAC.String()); err != nil {
			return fmt.Errorf("failed to set node %s hybrid overlay DRMAC annotation: %v", node.Name, err)
		}
	}

	return nil
}

func (oc *DefaultNetworkController) deleteHybridOverlayPort(node *corev1.Node) error {
	portName := util.GetHybridOverlayPortName(node.Name)
	lsp := nbdb.LogicalSwitchPort{Name: portName}
	if _, err := libovsdbops.GetLogicalSwitchPort(oc.nbClient, &lsp); err != nil {
		if !errors.Is(err, libovsdbclient.ErrNotFound) {
			return fmt.Errorf("failed to get logical switch port for hybrid overlay port %s, err: %v", portName, err)
		}
	} else {
		sw := nbdb.LogicalSwitch{Name: oc.GetNetworkScopedSwitchName(node.Name)}
		klog.Infof("Removing node %s hybrid overlay port", node.Name)
		if err := libovsdbops.DeleteLogicalSwitchPorts(oc.nbClient, &sw, &lsp); err != nil {
			return err
		}
	}
	if err := oc.removeHybridLRPolicySharedGW(node); err != nil {
		return err
	}
	return nil
}

func (oc *DefaultNetworkController) setupHybridLRPolicySharedGw(nodeSubnets []*net.IPNet, nodeName string, portMac net.HardwareAddr, drIPs net.IP) error {
	klog.Infof("Setting up logical route policy for hybrid subnet on node: %s", nodeName)
	var L3Prefix string
	for _, nodeSubnet := range nodeSubnets {
		if utilnet.IsIPv6CIDR(nodeSubnet) {
			L3Prefix = "ip6"
		} else {
			L3Prefix = "ip4"
		}
		hybridCIDRs := map[string]*net.IPNet{}
		if len(config.HybridOverlay.ClusterSubnets) > 0 {
			for _, hybridSubnet := range config.HybridOverlay.ClusterSubnets {
				if utilnet.IsIPv6CIDR(hybridSubnet.CIDR) == utilnet.IsIPv6CIDR(nodeSubnet) {
					hybridCIDRs[hybridCidrClusterKey] = hybridSubnet.CIDR
					break
				}
			}
		} else {
			// In cases of OpenShift SDN live migration, where config.HybridOverlay.ClusterSubnets is not provided, we
			// use the host subnets allocated by OpenShiftSDN as the hybrid-overlay-node-subnet and set up hybrid
			// overlay routes/policies to these subnets.
			nodes, err := oc.watchFactory.GetNodes()
			if err != nil {
				return err
			}
			for _, node := range nodes {
				node := *node
				if util.NoHostSubnet(&node) {
					if subnet, _ := houtil.ParseHybridOverlayHostSubnet(&node); subnet != nil {
						hybridCIDRs[node.Name] = subnet
					}
				}
			}
		}

		for hoNodeName, hybridCIDR := range hybridCIDRs {
			if utilnet.IsIPv6CIDR(hybridCIDR) != utilnet.IsIPv6CIDR(nodeSubnet) {
				// skip if the IP family is not match
				continue
			}
			drIP := drIPs
			matchStr := fmt.Sprintf(`inport == "%s%s" && %s.dst == %s`,
				ovntypes.RouterToSwitchPrefix, nodeName, L3Prefix, hybridCIDR)

			name := ovntypes.HybridSubnetPrefix + nodeName + ":" + hoNodeName
			if hoNodeName == hybridCidrClusterKey {
				name = ovntypes.HybridSubnetPrefix + nodeName
			}
			// Logic route policy to steer packet from pod to hybrid overlay nodes
			logicalRouterPolicy := nbdb.LogicalRouterPolicy{
				Priority: ovntypes.HybridOverlaySubnetPriority,
				ExternalIDs: map[string]string{
					"name": name,
				},
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{drIP.String()},
				Match:    matchStr,
			}

			if err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(oc.nbClient, ovntypes.OVNClusterRouter, &logicalRouterPolicy, func(item *nbdb.LogicalRouterPolicy) bool {
				return item.Priority == logicalRouterPolicy.Priority &&
					item.ExternalIDs["name"] == logicalRouterPolicy.ExternalIDs["name"]
			}, &logicalRouterPolicy.Nexthops, &logicalRouterPolicy.Match, &logicalRouterPolicy.Action); err != nil {
				return fmt.Errorf("failed to add policy route '%s' for host %q on %s , error: %w", matchStr, nodeName, ovntypes.OVNClusterRouter, err)
			}

			smb := &nbdb.StaticMACBinding{
				LogicalPort:        oc.GetNetworkScopedRouterToSwitchPortName(nodeName),
				MAC:                portMac.String(),
				IP:                 drIP.String(),
				OverrideDynamicMAC: true,
			}
			if err := libovsdbops.CreateOrUpdateStaticMacBinding(oc.nbClient, smb); err != nil {
				return fmt.Errorf("failed to create MAC Binding for hybrid overlay: %v", err)
			}

			// Logic route policy to steer packet from external to nodePort service backed by non-ovnkube pods to hybrid overlay nodes
			gwLRPIfAddrs, err := libovsdbutil.GetLRPAddrs(oc.nbClient, ovntypes.GWRouterToJoinSwitchPrefix+oc.GetNetworkScopedGWRouterName(nodeName))
			if err != nil {
				return err
			}
			gwLRPIfAddr, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6CIDR(hybridCIDR), gwLRPIfAddrs)
			if err != nil {
				return err
			}
			grMatchStr := fmt.Sprintf(`%s.src == %s && %s.dst == %s`,
				L3Prefix, gwLRPIfAddr.IP.String(), L3Prefix, hybridCIDR)
			grLogicalRouterPolicy := nbdb.LogicalRouterPolicy{
				Priority: ovntypes.HybridOverlaySubnetPriority,
				ExternalIDs: map[string]string{
					"name": ovntypes.HybridSubnetPrefix + nodeName + ovntypes.HybridOverlayGRSubfix,
				},
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{drIP.String()},
				Match:    grMatchStr,
			}

			if err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(oc.nbClient, ovntypes.OVNClusterRouter, &grLogicalRouterPolicy, func(item *nbdb.LogicalRouterPolicy) bool {
				return item.Priority == grLogicalRouterPolicy.Priority &&
					item.ExternalIDs["name"] == grLogicalRouterPolicy.ExternalIDs["name"]
			}, &grLogicalRouterPolicy.Nexthops, &grLogicalRouterPolicy.Match, &grLogicalRouterPolicy.Action); err != nil {
				return fmt.Errorf("failed to add policy route '%s' for host %q on %s , error: %w", matchStr, nodeName, ovntypes.OVNClusterRouter, err)
			}
			klog.Infof("Created hybrid overlay logical route policies for node %s", nodeName)

			// Static route to steer packets from external to nodePort service backed by pods on hybrid overlay node.
			// This route is to used for triggering above route policy
			clusterRouterStaticRoutes := nbdb.LogicalRouterStaticRoute{
				IPPrefix: hybridCIDR.String(),
				Nexthop:  drIP.String(),
				ExternalIDs: map[string]string{
					"name": name,
				},
			}
			if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(oc.nbClient,
				ovntypes.OVNClusterRouter, &clusterRouterStaticRoutes,
				func(item *nbdb.LogicalRouterStaticRoute) bool {
					return item.IPPrefix == clusterRouterStaticRoutes.IPPrefix &&
						item.ExternalIDs["name"] == clusterRouterStaticRoutes.ExternalIDs["name"] &&
						libovsdbops.PolicyEqualPredicate(clusterRouterStaticRoutes.Policy, item.Policy)
				}, &clusterRouterStaticRoutes.Nexthop); err != nil {
				return fmt.Errorf("failed to add policy route static '%s %s' for on %s , error: %w",
					clusterRouterStaticRoutes.IPPrefix, clusterRouterStaticRoutes.Nexthop,
					ovntypes.OVNClusterRouter, err)
			}
			klog.Infof("Created hybrid overlay logical route static route at cluster router for node %s", nodeName)

			// Static route to steer packets from external to nodePort service backed by pods on hybrid overlay node to cluster router.
			drLRPIfAddrs, err := libovsdbutil.GetLRPAddrs(oc.nbClient, ovntypes.GWRouterToJoinSwitchPrefix+ovntypes.OVNClusterRouter)
			if err != nil {
				return err
			}
			drLRPIfAddr, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6CIDR(hybridCIDR), drLRPIfAddrs)
			if err != nil {
				return fmt.Errorf("failed to match cluster router join interface IPs: %v, err: %v", drLRPIfAddr, err)
			}
			nodeGWRouterStaticRoutes := nbdb.LogicalRouterStaticRoute{
				IPPrefix: hybridCIDR.String(),
				Nexthop:  drLRPIfAddr.IP.String(),
				ExternalIDs: map[string]string{
					"name": ovntypes.HybridSubnetPrefix + nodeName + ovntypes.HybridOverlayGRSubfix,
				},
			}
			if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(oc.nbClient,
				oc.GetNetworkScopedGWRouterName(nodeName), &nodeGWRouterStaticRoutes,
				func(item *nbdb.LogicalRouterStaticRoute) bool {
					return item.IPPrefix == nodeGWRouterStaticRoutes.IPPrefix &&
						item.ExternalIDs["name"] == nodeGWRouterStaticRoutes.ExternalIDs["name"] &&
						libovsdbops.PolicyEqualPredicate(nodeGWRouterStaticRoutes.Policy, item.Policy)
				}, &nodeGWRouterStaticRoutes.Nexthop); err != nil {
				return fmt.Errorf("failed to add policy route static '%s %s' for on %s , error: %w",
					nodeGWRouterStaticRoutes.IPPrefix, nodeGWRouterStaticRoutes.Nexthop,
					oc.GetNetworkScopedGWRouterName(nodeName), err)
			}
			klog.Infof("Created hybrid overlay logical route static route at gateway router for node %s", nodeName)
		}
	}
	return nil
}

func (oc *DefaultNetworkController) removeHybridLRPolicySharedGW(node *corev1.Node) error {
	nodeName := node.Name
	name := ovntypes.HybridSubnetPrefix + nodeName

	if err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, ovntypes.OVNClusterRouter, func(item *nbdb.LogicalRouterPolicy) bool {
		if item.Priority != ovntypes.HybridOverlaySubnetPriority {
			return false
		}
		// A node ending in -gr shares its base name with another node's gateway
		// policy. Both live on the cluster router, so distinguish their matches.
		if item.ExternalIDs["name"] == name || strings.HasPrefix(item.ExternalIDs["name"], name+":") {
			return strings.HasPrefix(item.Match, fmt.Sprintf(`inport == "%s%s" && `, ovntypes.RouterToSwitchPrefix, nodeName))
		}
		return item.ExternalIDs["name"] == name+ovntypes.HybridOverlayGRSubfix &&
			(strings.HasPrefix(item.Match, "ip4.src == ") || strings.HasPrefix(item.Match, "ip6.src == "))
	}); err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete policy %s from %s, error: %w", name, ovntypes.OVNClusterRouter, err)
	}

	// Gateway and cluster routes can have colliding names for different nodes.
	// Match each name only on the router where that route is created.
	if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(oc.nbClient, oc.GetNetworkScopedGWRouterName(nodeName), func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.ExternalIDs["name"] == name+ovntypes.HybridOverlayGRSubfix
	}); err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete static route %s from %s, error: %w", name+"gr", oc.GetNetworkScopedGWRouterName(nodeName), err)
	}
	if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(oc.nbClient, ovntypes.OVNClusterRouter, func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.ExternalIDs["name"] == name ||
			strings.HasPrefix(item.ExternalIDs["name"], name+":")
	}); err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete static route %s from %s, error: %w", name, ovntypes.OVNClusterRouter, err)
	}

	// Hybrid-overlay bindings belong to this node's cluster-router port. Match
	// the port so cleanup also removes bindings whose IP annotations were lost.
	portName := oc.GetNetworkScopedRouterToSwitchPortName(nodeName)
	if err := libovsdbops.DeleteStaticMACBindingWithPredicate(oc.nbClient, func(item *nbdb.StaticMACBinding) bool {
		return item.LogicalPort == portName
	}); err != nil {
		return fmt.Errorf("failed to delete hybrid overlay static MAC bindings for node %s: %w", nodeName, err)
	}

	return nil
}

// takes the node name and allocates the hybrid overlay distributed router ip address
func (oc *DefaultNetworkController) allocateHybridOverlayDRIP(node *corev1.Node) error {
	if atomic.LoadUint32(&oc.allInitialPodsProcessed) == 0 {
		return fmt.Errorf("cannot allocate hybrid overlay distributed router ip for nodes until all initial pods are processed")
	}

	var ips []string
	// check if there is a hybrid distributed router IP set as an annotation
	nodeHybridOverlayDRIP := node.Annotations[hotypes.HybridOverlayDRIP]
	if len(nodeHybridOverlayDRIP) > 0 {
		ips = strings.Split(nodeHybridOverlayDRIP, ",")
	}

	allocatedHybridOverlayDRIPs, err := oc.lsManager.AllocateHybridOverlay(node.Name, ips)
	if err != nil {
		return fmt.Errorf("cannot allocate hybrid overlay interface addresses on node %s: %v", node.Name, err)
	}

	var sliceHybridOverlayDRIP []string
	for _, ip := range allocatedHybridOverlayDRIPs {
		sliceHybridOverlayDRIP = append(sliceHybridOverlayDRIP, ip.IP.String())
	}

	err = oc.kube.SetAnnotationsOnNode(context.TODO(), node.Name, map[string]interface{}{hotypes.HybridOverlayDRIP: strings.Join(sliceHybridOverlayDRIP, ",")})
	if err != nil {
		return fmt.Errorf("cannot set hybrid annotation on node %s: %v", node.Name, err)
	}

	return nil
}

// removeRoutesToHONode discovers the destination subnets from OVN, since the
// node's labels and annotations may already be gone when cleanup is retried.
func (oc *DefaultNetworkController) removeRoutesToHONode(nodeName string) error {
	clusterRouter, err := libovsdbops.GetLogicalRouter(oc.nbClient, &nbdb.LogicalRouter{Name: ovntypes.OVNClusterRouter})
	if errors.Is(err, libovsdbclient.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get cluster router for hybrid overlay cleanup of node %s: %w", nodeName, err)
	}
	// Destination-specific records are attached to the cluster router. Use its
	// references to avoid scanning routes and policies belonging to other routers.
	ownsDestination := func(name string) bool {
		return strings.HasPrefix(name, ovntypes.HybridSubnetPrefix) && strings.HasSuffix(name, ":"+nodeName)
	}
	routes, err := libovsdbops.GetRouterLogicalRouterStaticRoutesWithPredicate(oc.nbClient, clusterRouter, func(item *nbdb.LogicalRouterStaticRoute) bool {
		return ownsDestination(item.ExternalIDs["name"]) && item.Policy == nil
	})
	if err != nil {
		return fmt.Errorf("failed to find hybrid overlay routes to node %s: %w", nodeName, err)
	}
	policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(oc.nbClient, clusterRouter.Name, func(item *nbdb.LogicalRouterPolicy) bool {
		return ownsDestination(item.ExternalIDs["name"]) && item.Priority == ovntypes.HybridOverlaySubnetPriority
	})
	if err != nil {
		return fmt.Errorf("failed to find hybrid overlay policies to node %s: %w", nodeName, err)
	}

	// Gateway records use hybrid-subnet-<source>-gr, without the destination
	// node name. Associate them using the destination-specific source records.
	gatewaySubnets := map[string]sets.Set[string]{}
	addSubnet := func(name, subnet string) {
		gatewayName := strings.TrimSuffix(name, ":"+nodeName) + ovntypes.HybridOverlayGRSubfix
		if gatewaySubnets[gatewayName] == nil {
			gatewaySubnets[gatewayName] = sets.New[string]()
		}
		gatewaySubnets[gatewayName].Insert(subnet)
	}
	for _, route := range routes {
		addSubnet(route.ExternalIDs["name"], route.IPPrefix)
	}
	for _, policy := range policies {
		// Policy setup precedes route setup, so a partially configured node may
		// have only the policy's destination available for gateway cleanup.
		fields := strings.Fields(policy.Match)
		if len(fields) != 7 || (fields[4] != "ip4.dst" && fields[4] != "ip6.dst") || fields[5] != "==" {
			return fmt.Errorf("unexpected hybrid overlay policy match for node %s: %q", nodeName, policy.Match)
		}
		addSubnet(policy.ExternalIDs["name"], fields[6])
	}
	if len(gatewaySubnets) == 0 {
		return nil
	}

	ops, err := libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(oc.nbClient, nil, ovntypes.OVNClusterRouter, func(item *nbdb.LogicalRouterPolicy) bool {
		if item.Priority != ovntypes.HybridOverlaySubnetPriority {
			return false
		}
		if ownsDestination(item.ExternalIDs["name"]) {
			return true
		}
		for subnet := range gatewaySubnets[item.ExternalIDs["name"]] {
			if strings.HasSuffix(item.Match, " && ip4.dst == "+subnet) || strings.HasSuffix(item.Match, " && ip6.dst == "+subnet) {
				return true
			}
		}
		return false
	})
	if err != nil {
		return fmt.Errorf("failed to build hybrid overlay policy cleanup for node %s: %w", nodeName, err)
	}
	// Discover routers from OVN rather than the informer: a source node may
	// also have been deleted, leaving its gateway resources to clean up.
	routers, err := libovsdbops.FindLogicalRoutersWithPredicate(oc.nbClient, func(item *nbdb.LogicalRouter) bool {
		return item.Name == ovntypes.OVNClusterRouter || strings.HasPrefix(item.Name, ovntypes.GWRouterPrefix)
	})
	if err != nil {
		return fmt.Errorf("failed to find routers for hybrid overlay cleanup of node %s: %w", nodeName, err)
	}
	for _, router := range routers {
		ops, err = libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicateOps(oc.nbClient, ops, router.Name, func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.Policy == nil && (ownsDestination(item.ExternalIDs["name"]) || gatewaySubnets[item.ExternalIDs["name"]].Has(item.IPPrefix))
		})
		if err != nil {
			return fmt.Errorf("failed to build hybrid overlay route cleanup on router %s for node %s: %w", router.Name, nodeName, err)
		}
	}
	// Delete the discovery records and gateway records atomically. A failed
	// transaction must leave enough information to discover the same work again.
	if _, err := libovsdbops.TransactAndCheck(oc.nbClient, ops); err != nil {
		return fmt.Errorf("failed to remove hybrid overlay routes and policies for node %s: %w", nodeName, err)
	}
	return nil
}
