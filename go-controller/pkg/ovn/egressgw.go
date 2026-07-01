// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"errors"
	"fmt"
	"net"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	apbroutecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func (oc *DefaultNetworkController) syncConntrackForExternalGateways(namespace string, gwIPsToKeep sets.Set[string]) error {
	return util.SyncConntrackForExternalGateways(gwIPsToKeep, oc.isPodInLocalZone, func() ([]*corev1.Pod, error) {
		return oc.watchFactory.GetPods(namespace)
	})
}

func (oc *DefaultNetworkController) checkAndDeleteStaleConntrackEntries() {
	namespaces, err := oc.watchFactory.GetNamespaces()
	if err != nil {
		klog.Errorf("Unable to get pods from informer: %v", err)
		return
	}
	for _, namespace := range namespaces {
		// flush here since we know we have added an egressgw pod and we also know the full list of existing gatewayIPs
		existingGWs, err := oc.apbExternalRouteController.GetAdminPolicyBasedExternalRouteIPsForTargetNamespace(namespace.Name)
		if err != nil {
			klog.Errorf("Unable to retrieve gateway IPs for Admin Policy Based External Route objects for ns %s: %v", namespace.Name, err)
			return
		}
		if len(existingGWs) > 0 {
			pods, err := oc.watchFactory.GetPods(namespace.Name)
			if err != nil {
				klog.Warningf("Unable to get pods from informer for namespace %s: %v", namespace.Name, err)
			}
			if len(pods) > 0 || err != nil {
				// we only need to proceed if there is at least one pod in this namespace on this node
				// OR if we couldn't fetch the pods for some reason at this juncture
				err = oc.syncConntrackForExternalGateways(namespace.Name, existingGWs)
				if err != nil {
					klog.Errorf("Syncing conntrack entries for egressGWs %+v serving the namespace %s failed: %v",
						existingGWs, namespace.Name, err)
				}
			}
		}
	}
}

func (oc *DefaultNetworkController) isPodInLocalZone(pod *corev1.Pod) (bool, error) {
	node, err := oc.watchFactory.GetNode(pod.Spec.NodeName)
	if err != nil {
		return false, err
	}
	return oc.isLocalNode(node), nil
}

func (oc *DefaultNetworkController) deleteLogicalRouterStaticRoute(podIP, mask, gw, gr string) error {
	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.Policy != nil &&
			*item.Policy == nbdb.LogicalRouterStaticRoutePolicySrcIP &&
			item.IPPrefix == podIP+mask &&
			item.Nexthop == gw
	}
	err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(oc.nbClient, gr, p)
	if err != nil {
		return fmt.Errorf("error deleting static route from router %s: %v", gr, err)
	}

	return nil
}

// deletePodGWRoute deletes all associated gateway routing resources for one
// pod gateway route
// this MUST be called with a lock on routeInfo
func (oc *DefaultNetworkController) deletePodGWRoute(routeInfo *apbroutecontroller.RouteInfo, podIP, gw, gr string) error {
	if utilnet.IsIPv6String(gw) != utilnet.IsIPv6String(podIP) {
		return nil
	}
	pod, err := oc.watchFactory.PodCoreInformer().Lister().Pods(routeInfo.PodName.Namespace).Get(routeInfo.PodName.Name)
	if err == nil {
		local, err := oc.isPodInLocalZone(pod)
		if err != nil {
			return err
		}
		if !local {
			klog.V(4).Infof("Not deleting exgw routes for pod %s not in the local zone %s", routeInfo.PodName, oc.zone)
			return nil
		}
	}
	mask := util.GetIPFullMaskString(podIP)
	if err := oc.deleteLogicalRouterStaticRoute(podIP, mask, gw, gr); err != nil {
		return fmt.Errorf("unable to delete pod %s ECMP route to GR %s, GW: %s: %w",
			routeInfo.PodName, gr, gw, err)
	}

	klog.V(5).Infof("ECMP route deleted for pod: %s, on gr: %s, to gw: %s",
		routeInfo.PodName, gr, gw)

	node := util.GetWorkerFromGatewayRouter(gr)

	// The gw is deleted from the routes cache after this func is called, length 1
	// means it is the last gw for the pod and the hybrid route policy should be deleted.
	if entry := routeInfo.PodExternalRoutes[podIP]; len(entry) <= 1 {
		if err := oc.delHybridRoutePolicyForPod(net.ParseIP(podIP), node); err != nil {
			return fmt.Errorf("unable to delete hybrid route policy for pod %s: err: %v", routeInfo.PodName, err)
		}
	}

	portPrefix, err := oc.extSwitchPrefix(node)
	if err != nil {
		return err
	}
	return oc.cleanUpBFDEntry(gw, gr, portPrefix)
}

// deleteGwRoutesForNamespace handles deleting routes to gateways for a pod on a specific GR.
// If a set of gateways is given, only routes for that gateway are deleted. If no gateways
// are given, all routes for the namespace are deleted.
func (oc *DefaultNetworkController) deleteGWRoutesForNamespace(namespace string, matchGWs sets.Set[string]) error {
	deleteAll := (matchGWs == nil || matchGWs.Len() == 0)

	policyGWIPs, err := oc.apbExternalRouteController.GetDynamicGatewayIPsForTargetNamespace(namespace)
	if err != nil {
		return err
	}
	policyStaticGWIPs, err := oc.apbExternalRouteController.GetStaticGatewayIPsForTargetNamespace(namespace)
	if err != nil {
		return err
	}
	policyGWIPs = policyGWIPs.Union(policyStaticGWIPs)
	return oc.externalGatewayRouteInfo.CleanupNamespace(namespace, func(routeInfo *apbroutecontroller.RouteInfo) error {
		for podIP, routes := range routeInfo.PodExternalRoutes {
			for gw, gr := range routes {
				if (deleteAll || matchGWs.Has(gw)) && !policyGWIPs.Has(gw) {
					if err := oc.deletePodGWRoute(routeInfo, podIP, gw, gr); err != nil {
						// if we encounter error while deleting routes for one pod; we return and don't try subsequent pods
						return fmt.Errorf("delete pod GW route failed: %w", err)
					}
					delete(routes, gw)
				}
			}
		}
		return nil
	})
}

// deleteGwRoutesForPod handles deleting all routes to gateways for a pod IP on a specific GR
func (oc *DefaultNetworkController) deleteGWRoutesForPod(name ktypes.NamespacedName, podIPNets []*net.IPNet) (err error) {
	return oc.externalGatewayRouteInfo.Cleanup(name, func(routeInfo *apbroutecontroller.RouteInfo) error {
		policyGWIPs, err := oc.apbExternalRouteController.GetDynamicGatewayIPsForTargetNamespace(name.Namespace)
		if err != nil {
			return err
		}
		policyStaticGWIPs, err := oc.apbExternalRouteController.GetStaticGatewayIPsForTargetNamespace(name.Namespace)
		if err != nil {
			return err
		}
		policyGWIPs = policyGWIPs.Union(policyStaticGWIPs)

		for _, podIPNet := range podIPNets {
			podIP := podIPNet.IP.String()
			routes, ok := routeInfo.PodExternalRoutes[podIP]
			if !ok {
				continue
			}
			if len(routes) == 0 {
				delete(routeInfo.PodExternalRoutes, podIP)
				continue
			}
			for gw, gr := range routes {
				if !policyGWIPs.Has(gw) {
					if err := oc.deletePodGWRoute(routeInfo, podIP, gw, gr); err != nil {
						// if we encounter error while deleting routes for one pod; we return and don't try subsequent pods
						return fmt.Errorf("delete pod GW route failed: %w", err)
					}
					delete(routes, gw)
				}
			}
		}
		return nil
	})
}

// deletePodSNAT removes per pod SNAT rules towards the nodeIP that are applied to the GR where the pod resides
// used when disableSNATMultipleGWs=true
func (oc *DefaultNetworkController) deletePodSNAT(nodeName string, extIPs, podIPNets []*net.IPNet) error {

	node, err := oc.watchFactory.NodeCoreInformer().Lister().Get(nodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If node does not exist, there is nothing to delete
			return nil
		}
		return err
	}
	if !oc.isLocalNode(node) {
		klog.V(4).Infof("Node %s is not in the local zone %s", nodeName, oc.zone)
		return nil
	}
	// Default network does not set any matches in Pod SNAT
	ops, err := deletePodSNATOps(oc.nbClient, nil, oc.GetNetworkScopedGWRouterName(nodeName), extIPs, podIPNets)
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to delete SNAT rule for pod on gateway router %s: %w", oc.GetNetworkScopedGWRouterName(nodeName), err)
	}
	return nil
}

// buildPodSNAT builds per pod SNAT rules towards the nodeIP that are applied to the GR where the pod resides.
// exemptedExtIPs should be an AddressSet UUID.
// When specified, traffic to IPs in that AddressSet will not be SNATed.
func buildPodSNAT(extIPs, podIPNets []*net.IPNet, match string, exemptedExtIPs string) ([]*nbdb.NAT, error) {
	nats := make([]*nbdb.NAT, 0, len(extIPs)*len(podIPNets))
	for _, podIPNet := range podIPNets {
		fullMaskPodNet := &net.IPNet{
			IP:   podIPNet.IP,
			Mask: util.GetIPFullMask(podIPNet.IP),
		}
		if len(extIPs) == 0 {
			nats = append(nats, libovsdbops.BuildSNATWithExemptedExtIPs(nil, fullMaskPodNet, "", nil, match, exemptedExtIPs))
		} else {
			for _, gwIPNet := range extIPs {
				if utilnet.IsIPv6CIDR(gwIPNet) != utilnet.IsIPv6CIDR(podIPNet) {
					continue
				}
				nats = append(nats, libovsdbops.BuildSNATWithExemptedExtIPs(&gwIPNet.IP, fullMaskPodNet, "", nil, match, exemptedExtIPs))
			}
		}
	}
	return nats, nil
}

// getExternalIPsGR returns all the externalIPs for a node(GR) from its l3 gateway annotation
func getExternalIPsGR(watchFactory *factory.WatchFactory, nodeName string) ([]*net.IPNet, error) {
	var err error
	node, err := watchFactory.GetNode(nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %v", nodeName, err)
	}
	l3GWConfig, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		return nil, fmt.Errorf("unable to parse node L3 gw annotation: %v", err)
	}
	return l3GWConfig.IPAddresses, nil
}

// deletePodSNATOps creates ovsdb operation that removes per pod SNAT rules towards the nodeIP that are applied to the GR where the pod resides
// used when disableSNATMultipleGWs=true
func deletePodSNATOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, gwRouterName string, extIPs, podIPNets []*net.IPNet) ([]ovsdb.Operation, error) {
	nats, err := buildPodSNAT(extIPs, podIPNets, "", "") // for delete, match and exemptedExtIPs are not needed - we try to cleanup all the SNATs that match the isEquivalentNAT predicate
	if err != nil {
		return nil, err
	}
	logicalRouter := nbdb.LogicalRouter{
		Name: gwRouterName,
	}
	ops, err = libovsdbops.DeleteNATsOps(nbClient, ops, &logicalRouter, nats...)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return nil, fmt.Errorf("failed create operation for deleting SNAT rule for pod on gateway router %s: %v", logicalRouter.Name, err)
	}
	return ops, nil
}

// addOrUpdatePodSNAT adds or updates per pod SNAT rules towards the nodeIP that are applied to the GR where the pod resides
// used when disableSNATMultipleGWs=true
func addOrUpdatePodSNAT(nbClient libovsdbclient.Client, gwRouterName string, extIPs, podIfAddrs []*net.IPNet) error {
	ops, err := addOrUpdatePodSNATOps(nbClient, gwRouterName, extIPs, podIfAddrs, "", "", nil)
	if err != nil {
		return err
	}
	if _, err = libovsdbops.TransactAndCheck(nbClient, ops); err != nil {
		return fmt.Errorf("failed to update SNAT for pods of router %s: %v", gwRouterName, err)
	}
	return nil
}

// addOrUpdatePodSNATOps returns the operation that adds or updates per pod SNAT rules towards the nodeIP that are
// applied to the GR where the pod resides.
// exemptedExtIPs should be an AddressSet UUID.
// When specified, traffic to IPs in that AddressSet will not be SNATed.
// used when disableSNATMultipleGWs=true
func addOrUpdatePodSNATOps(nbClient libovsdbclient.Client, gwRouterName string, extIPs, podIfAddrs []*net.IPNet, snatMatch string, exemptedExtIPs string, ops []ovsdb.Operation) ([]ovsdb.Operation, error) {
	gwRouter := &nbdb.LogicalRouter{Name: gwRouterName}
	nats, err := buildPodSNAT(extIPs, podIfAddrs, snatMatch, exemptedExtIPs)
	if err != nil {
		return nil, err
	}
	if ops, err = libovsdbops.CreateOrUpdateNATsOps(nbClient, ops, gwRouter, nats...); err != nil {
		return nil, fmt.Errorf("failed to create ops to update SNAT for pods of router: %s, error: %v", gwRouterName, err)
	}
	return ops, nil
}

// delHybridRoutePolicyForPod handles deleting a logical route policy that
// forces pod egress traffic to be rerouted to a gateway router for local gateway mode.
// WARNING: updates same db entries as apbroutecontroller. Make sure to call only when route is not managed by
// apbroute controller.
func (oc *DefaultNetworkController) delHybridRoutePolicyForPod(podIP net.IP, node string) error {
	if config.Gateway.Mode == config.GatewayModeLocal {
		// Delete podIP from the node's address_set.
		asIndex := apbroutecontroller.GetHybridRouteAddrSetDbIDs(node, oc.controllerName)
		as, err := oc.addressSetFactory.EnsureAddressSet(asIndex)
		if err != nil {
			return fmt.Errorf("cannot Ensure that addressSet for node %s exists %v", node, err)
		}
		err = as.DeleteAddresses([]string{podIP.String()})
		if err != nil {
			return fmt.Errorf("unable to remove PodIP %s: to the address set %s, err: %v", podIP, node, err)
		}

		// delete hybrid policy to bypass lr-policy in GR, only if there are zero pods on this node.
		ipv4HashedAS, ipv6HashedAS := as.GetASHashNames()
		ipv4PodIPs, ipv6PodIPs := as.GetAddresses()
		deletePolicy := false
		var l3Prefix string
		var matchSrcAS string
		if utilnet.IsIPv6(podIP) {
			l3Prefix = "ip6"
			if len(ipv6PodIPs) == 0 {
				deletePolicy = true
			}
			matchSrcAS = ipv6HashedAS
		} else {
			l3Prefix = "ip4"
			if len(ipv4PodIPs) == 0 {
				deletePolicy = true
			}
			matchSrcAS = ipv4HashedAS
		}
		if deletePolicy {
			var matchDst string
			var clusterL3Prefix string
			for _, clusterSubnet := range config.Default.ClusterSubnets {
				if utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
					clusterL3Prefix = "ip6"
				} else {
					clusterL3Prefix = "ip4"
				}
				if l3Prefix != clusterL3Prefix {
					continue
				}
				matchDst += fmt.Sprintf(" && %s.dst != %s", l3Prefix, clusterSubnet.CIDR)
			}
			matchStr := fmt.Sprintf(`inport == "%s%s" && %s.src == $%s`, types.RouterToSwitchPrefix, node, l3Prefix, matchSrcAS)
			matchStr += matchDst

			p := func(item *nbdb.LogicalRouterPolicy) bool {
				return item.Priority == types.HybridOverlayReroutePriority && item.Match == matchStr
			}
			err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, oc.GetNetworkScopedClusterRouterName(), p)
			if err != nil {
				return fmt.Errorf("error deleting policy %s on router %s: %v", matchStr, oc.GetNetworkScopedClusterRouterName(), err)
			}
		}
		if len(ipv4PodIPs) == 0 && len(ipv6PodIPs) == 0 {
			// delete address set.
			err := as.Destroy()
			if err != nil {
				return fmt.Errorf("failed to remove address set: %s, on: %s, err: %v",
					as.GetName(), node, err)
			}
		}
	}
	return nil
}

// delAllHybridRoutePolicies deletes all the 501 hybrid-route-policies that
// force pod egress traffic to be rerouted to a gateway router for local gateway mode.
// Called when migrating to SGW from LGW.
func (oc *DefaultNetworkController) delAllHybridRoutePolicies() error {
	// nuke all the policies
	policyPred := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority == types.HybridOverlayReroutePriority
	}
	err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, oc.GetNetworkScopedClusterRouterName(), policyPred)
	if err != nil {
		return fmt.Errorf("error deleting hybrid route policies on %s: %v", oc.GetNetworkScopedClusterRouterName(), err)
	}

	// nuke all the address-sets.
	// if we fail to remove LRP's above, we don't attempt to remove ASes due to dependency constraints.
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetHybridNodeRoute, oc.controllerName, nil)
	asPred := libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, nil)
	err = libovsdbops.DeleteAddressSetsWithPredicate(oc.nbClient, asPred)
	if err != nil {
		return fmt.Errorf("failed to remove hybrid route address sets: %v", err)
	}

	return nil
}

// delAllLegacyHybridRoutePolicies deletes all the 501 hybrid-route-policies that
// force pod egress traffic to be rerouted to a gateway router for local gateway mode.
// New hybrid route matches on address set, while legacy matches just on pod IP
func (oc *DefaultNetworkController) delAllLegacyHybridRoutePolicies() error {
	// nuke all the policies
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		if item.Priority != types.HybridOverlayReroutePriority {
			return false
		}
		if isNewVer, err := regexp.MatchString(`src\s*==\s*\$`, item.Match); err == nil && isNewVer {
			return false
		}
		return true
	}
	err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, oc.GetNetworkScopedClusterRouterName(), p)
	if err != nil {
		return fmt.Errorf("error deleting legacy hybrid route policies on %s: %v", oc.GetNetworkScopedClusterRouterName(), err)
	}

	return nil
}

// cleanUpBFDEntry checks if the BFD table entry related to the associated
// gw router / port / gateway ip is referenced by other routing rules, and if
// not removes the entry to avoid having dangling BFD entries.
func (oc *DefaultNetworkController) cleanUpBFDEntry(gatewayIP, gatewayRouter, prefix string) error {
	portName := prefix + types.GWRouterToExtSwitchPrefix + gatewayRouter
	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.OutputPort != nil && *item.OutputPort == portName && item.Nexthop == gatewayIP && item.BFD != nil && *item.BFD != ""
	}
	logicalRouterStaticRoutes, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(oc.nbClient, p)
	if err != nil {
		return fmt.Errorf("cleanUpBFDEntry failed to list routes for %s: %w", portName, err)
	}

	if len(logicalRouterStaticRoutes) > 0 {
		return nil
	}

	bfd := nbdb.BFD{
		LogicalPort: portName,
		DstIP:       gatewayIP,
	}

	err = libovsdbops.DeleteBFDs(oc.nbClient, &bfd)
	if err != nil {
		return fmt.Errorf("error deleting BFD %+v: %v", bfd, err)
	}

	return nil
}

// extSwitchPrefix returns the prefix of the external switch to use for
// external gateway routes. In case no second bridge is configured, we
// use the default one and the prefix is empty.
func (oc *DefaultNetworkController) extSwitchPrefix(nodeName string) (string, error) {
	node, err := oc.watchFactory.GetNode(nodeName)
	if err != nil {
		return "", fmt.Errorf("extSwitchPrefix failed to find node %s: %w", nodeName, err)
	}
	l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		return "", fmt.Errorf("extSwitchPrefix failed to parse l3 gateway annotation for node %s: %w", nodeName, err)
	}

	if l3GatewayConfig.EgressGWInterfaceID != "" {
		return types.EgressGWSwitchPrefix, nil
	}
	return "", nil
}
