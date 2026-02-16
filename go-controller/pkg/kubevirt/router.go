package kubevirt

import (
	"fmt"
	"net"
	"os"

	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/udn"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	logicalswitchmanager "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func DeleteRoutingForMigratedPodWithZone(nbClient libovsdbclient.Client, pod *corev1.Pod, zone string) error {
	vm := ExtractVMNameFromPod(pod)
	predicate := func(itemExternalIDs map[string]string) bool {
		containsZone := true
		if zone != "" {
			containsZone = itemExternalIDs[OvnZoneExternalIDKey] == zone
		}
		return containsZone && externalIDsContainsVM(itemExternalIDs, vm)
	}
	routePredicate := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return predicate(item.ExternalIDs)
	}
	if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(nbClient, types.OVNClusterRouter, routePredicate); err != nil {
		return fmt.Errorf("failed deleting pod routing when deleting the LR static routes: %v", err)
	}
	policyPredicate := func(item *nbdb.LogicalRouterPolicy) bool {
		return predicate(item.ExternalIDs)
	}
	if err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(nbClient, types.OVNClusterRouter, policyPredicate); err != nil {
		return fmt.Errorf("failed deleting pod routing when deleting the LR policies: %v", err)
	}
	return nil
}

func DeleteRoutingForMigratedPod(nbClient libovsdbclient.Client, pod *corev1.Pod) error {
	return DeleteRoutingForMigratedPodWithZone(nbClient, pod, "")
}

func ReconcileClusterDefaultNetworkLocalZone(watchFactory *factory.WatchFactory, nbClient libovsdbclient.Client,
	lsManager *logicalswitchmanager.LogicalSwitchManager, pod *corev1.Pod, clusterSubnets []config.CIDRNetworkEntry,
) error {
	vmReady, err := virtualMachineReady(watchFactory, pod)
	if err != nil {
		return err
	}
	if !vmReady {
		return nil
	}

	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, types.DefaultNetworkName)
	if err != nil {
		return fmt.Errorf("failed reading local pod annotation: %v", err)
	}
	if err := reconcileClusterDefaultNetworkLocalZonePodAddressesToNodeRoute(watchFactory, nbClient, lsManager, pod, podAnnotation, clusterSubnets); err != nil {
		return fmt.Errorf("failed to reconcile pod address to node route at kubevirt cluster default network local zone: %w", err)
	}

	if err := reconcileClusterDefaultNetworkLocalZoneNeighbours(watchFactory, pod, podAnnotation, types.DefaultNetworkName); err != nil {
		return fmt.Errorf("failed to reconcile neighbours at kubevirt cluster default network local zone: %w", err)
	}
	return nil
}

// reconcileLocalZonePodAddressesToNodeRoute will add static routes and policies to ovn_cluster_route logical router
// to ensure VM traffic work as expected after live migration if the pod is running at the local/global zone.
//
// NOTE: IC with multiple nodes per zone is not supported
//
// Following is the list of NB logical resources created depending if it's interconnected or not:
//
// IC (on node per zone):
//   - static route with cluster wide CIDR as src-ip prefix and nexthop GR, it has less
//     priority than route to use overlay in case of pod to pod communication
//
// NO IC:
//   - low priority policy with src VM ip and reroute GR, since it has low priority
//     it will not override the policy to enroute pod to pod traffic using overlay
//
// Both:
//   - static route with VM ip as dst-ip prefix and output port the LRP pointing to the VM's node switch
func reconcileClusterDefaultNetworkLocalZonePodAddressesToNodeRoute(watchFactory *factory.WatchFactory, nbClient libovsdbclient.Client,
	lsManager *logicalswitchmanager.LogicalSwitchManager, pod *corev1.Pod, podAnnotation *util.PodAnnotation, clusterSubnets []config.CIDRNetworkEntry,
) error {
	nodeOwningSubnet, _ := ZoneContainsPodSubnet(lsManager, podAnnotation.IPs)
	vmRunningAtNodeOwningSubnet := nodeOwningSubnet == pod.Spec.NodeName
	if vmRunningAtNodeOwningSubnet {
		// Point to point routing is no longer needed if vm
		// is running at the node that owns the subnet
		if err := DeleteRoutingForMigratedPod(nbClient, pod); err != nil {
			return fmt.Errorf("failed configuring pod routing when deleting stale static routes or policies for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		return nil
	}

	// For interconnect at static route with a cluster-wide src-ip address is
	// needed to route egress n/s traffic
	if config.OVNKubernetesFeature.EnableInterconnect {
		// NOTE: EIP & ESVC use same route and if this is already present thanks to those features,
		// this will be a no-op
		node, err := watchFactory.GetNode(pod.Spec.NodeName)
		if err != nil {
			return fmt.Errorf("failed getting to list node %q for pod %s/%s: %w", pod.Spec.NodeName, pod.Namespace, pod.Name, err)
		}
		gatewayIPs, err := udn.GetGWRouterIPs(node, &util.DefaultNetInfo{})
		if err != nil {
			return fmt.Errorf("failed to get default network gateway router join IPs for node %q: %w", node.Name, err)
		}
		if err := libovsdbutil.CreateDefaultRouteToExternal(nbClient, types.OVNClusterRouter,
			types.GWRouterPrefix+pod.Spec.NodeName, clusterSubnets, gatewayIPs); err != nil {
			return err
		}
	}

	lrpName := types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + pod.Spec.NodeName
	lrpAddresses, err := libovsdbutil.GetLRPAddrs(nbClient, lrpName)
	if err != nil {
		return fmt.Errorf("failed configuring pod routing when reading LRP %s addresses: %v", lrpName, err)
	}
	for _, podIP := range podAnnotation.IPs {
		podAddress := podIP.IP.String()

		if !config.OVNKubernetesFeature.EnableInterconnect {
			// Policy to with low priority to route traffic to the gateway
			ipFamily := utilnet.IPFamilyOfCIDR(podIP)
			nodeGRAddress, err := util.MatchFirstIPNetFamily(ipFamily == utilnet.IPv6, lrpAddresses)
			if err != nil {
				return err
			}

			// adds a policy so that a migrated pods egress traffic
			// will be routed to the local GR where it now resides
			match := fmt.Sprintf("ip%s.src == %s", ipFamily, podAddress)
			egressPolicy := nbdb.LogicalRouterPolicy{
				Match:    match,
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{nodeGRAddress.IP.String()},
				Priority: types.EgressLiveMigrationReroutePriority,
				ExternalIDs: map[string]string{
					OvnZoneExternalIDKey:         OvnLocalZone,
					VirtualMachineExternalIDsKey: pod.Labels[kubevirtv1.VirtualMachineNameLabel],
					NamespaceExternalIDsKey:      pod.Namespace,
				},
			}
			if err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(nbClient, types.OVNClusterRouter, &egressPolicy, func(item *nbdb.LogicalRouterPolicy) bool {
				return item.Priority == egressPolicy.Priority && item.Match == egressPolicy.Match && item.Action == egressPolicy.Action
			}); err != nil {
				return fmt.Errorf("failed adding point to point policy for pod %s/%s : %v", pod.Namespace, pod.Name, err)
			}
		}
		// Add a route for reroute ingress traffic to the VM port since
		// the subnet is alien to ovn_cluster_router
		outputPort := types.RouterToSwitchPrefix + pod.Spec.NodeName
		ingressRoute := nbdb.LogicalRouterStaticRoute{
			IPPrefix:   podAddress,
			Nexthop:    podAddress,
			Policy:     &nbdb.LogicalRouterStaticRoutePolicyDstIP,
			OutputPort: &outputPort,
			ExternalIDs: map[string]string{
				OvnZoneExternalIDKey:         OvnLocalZone,
				VirtualMachineExternalIDsKey: pod.Labels[kubevirtv1.VirtualMachineNameLabel],
				NamespaceExternalIDsKey:      pod.Namespace,
			},
		}
		if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(nbClient, types.OVNClusterRouter, &ingressRoute, func(item *nbdb.LogicalRouterStaticRoute) bool {
			matches := item.IPPrefix == ingressRoute.IPPrefix && item.Policy != nil && *item.Policy == *ingressRoute.Policy
			return matches
		}); err != nil {
			return fmt.Errorf("failed adding static route: %v", err)
		}

	}
	return nil
}

// reconcileClusterDefaultNetworkLocalZoneNeighbours updates neighbor entries for pods in the
// same subnet as a live migrated VM when the VM is running at a node that doesn't own its subnet.
// In this case, neighbor entries for other pods in the subnet need to be updated to use the gateway
// proxy MAC instead of the VM pod's MAC, since the VM is now on a different switch.
//
// NOTE: Only applies for IC
func reconcileClusterDefaultNetworkLocalZoneNeighbours(watchFactory *factory.WatchFactory, pod *corev1.Pod, podAnnotation *util.PodAnnotation, nadName string) error {
	// TODO: Implement neighbours update for non IC
	if !config.OVNKubernetesFeature.EnableInterconnect {
		return nil
	}

	currentNode, err := getCurrentNode()
	if err != nil {
		return err
	}
	currentNodeOwnsSubnet, err := nodeContainsPodSubnet(watchFactory, currentNode, podAnnotation, nadName)
	if err != nil {
		return err
	}

	vmRunningAtCurrentNode := pod.Spec.NodeName == currentNode

	// Now that the VM is running at a node not owning his subnet, using pod's mac
	// will not work since it's a different switch so overwrite those neighbors with
	// proxy mac from gatway
	if !currentNodeOwnsSubnet && vmRunningAtCurrentNode {
		ipsToNotify, err := findRunningPodsIPsFromPodSubnet(watchFactory, pod, podAnnotation, nadName)
		if err != nil {
			return fmt.Errorf("failed discovering pod IPs within VM's subnet to update neighbors VM: %w", err)
		}
		// Include the gateway IPs in the notification list. After PR#5773 [1]
		// DHCP advertises the actual subnet gateway IP instead of the link-local
		// 169.254.1.1 address. Before migration, the VM resolves its gateway
		// (e.g. 10.244.2.1) to the node's real router port (LRP) MAC because
		// the LRP's ARP responder has higher priority than arp_proxy on the
		// subnet-owning switch. After migration, the VM is on a different switch
		// where that LRP MAC doesn't exist, so we must send a GARP to update
		// the VM's gateway neighbor entry to the arp_proxy MAC. This bug was
		// masked in e2e tests by PR#4755 [2] which changed connectivity test
		// checks.
		// [1] https://github.com/ovn-kubernetes/ovn-kubernetes/pull/5773
		// [2] https://github.com/ovn-kubernetes/ovn-kubernetes/pull/4755
		for _, gw := range podAnnotation.Gateways {
			mask := net.CIDRMask(32, 32)
			if gw.To4() == nil {
				mask = net.CIDRMask(128, 128)
			}
			ipsToNotify = append(ipsToNotify, &net.IPNet{IP: gw, Mask: mask})
		}
		if err := notifyProxy(ipsToNotify); err != nil {
			return err
		}
	}
	return nil
}

func ReconcileClusterDefaultNetworkRemoteZone(watchFactory *factory.WatchFactory, nbClient libovsdbclient.Client, pod *corev1.Pod) error {
	vmReady, err := virtualMachineReady(watchFactory, pod)
	if err != nil {
		return err
	}
	if !vmReady {
		return nil
	}

	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, types.DefaultNetworkName)
	if err != nil {
		return fmt.Errorf("failed reading remote pod annotation: %v", err)
	}
	if err := ensureRemoteZonePodAddressesToNodeRoute(watchFactory, nbClient, pod, podAnnotation, types.DefaultNetworkName); err != nil {
		return fmt.Errorf("failed to reconcile pod address to node route at kubevirt cluster default network remote zone: %w", err)
	}
	if err := reconcileClusterDefaultNetworkRemoteZoneNeighbours(watchFactory, nbClient, pod, podAnnotation, types.DefaultNetworkName); err != nil {
		return fmt.Errorf("failed to reconcile neighbours at kubevirt cluster default network remote zone: %w", err)
	}
	return nil
}

// ensureRemoteZonePodAddressesToNodeRoute will add static routes when live
// migrated pod belongs to remote zone to send traffic over transwitch switch
// port of the node where the pod is running:
//   - A dst-ip with live migrated pod ip as prefix and nexthop the pod's
//     current node transit switch port.
func ensureRemoteZonePodAddressesToNodeRoute(watchFactory *factory.WatchFactory, nbClient libovsdbclient.Client, pod *corev1.Pod, podAnnotation *util.PodAnnotation, nadName string) error {
	// DHCPOptions are only needed at the node is running the VM
	// at that's the local zone node not the remote zone
	if err := DeleteDHCPOptions(nbClient, pod); err != nil {
		return err
	}

	vmRunningAtNodeOwningSubnet, err := nodeContainsPodSubnet(watchFactory, pod.Spec.NodeName, podAnnotation, nadName)
	if err != nil {
		return err
	}
	if vmRunningAtNodeOwningSubnet {
		// Point to point routing is no longer needed if vm
		// is running at the node with VM's subnet
		if err := DeleteRoutingForMigratedPod(nbClient, pod); err != nil {
			return err
		}
		return nil
	} else {
		// Since we are at remote zone we should not have local zone point to
		// to point routing
		if err := DeleteRoutingForMigratedPodWithZone(nbClient, pod, OvnLocalZone); err != nil {
			return err
		}
	}

	node, err := watchFactory.GetNode(pod.Spec.NodeName)
	if err != nil {
		return err
	}
	transitSwitchPortAddrs, err := util.ParseNodeTransitSwitchPortAddrs(node)
	if err != nil {
		return err
	}
	for _, podIP := range podAnnotation.IPs {
		ipFamily := utilnet.IPFamilyOfCIDR(podIP)
		transitSwitchPortAddr, err := util.MatchFirstIPNetFamily(ipFamily == utilnet.IPv6, transitSwitchPortAddrs)
		if err != nil {
			return err
		}
		route := nbdb.LogicalRouterStaticRoute{
			IPPrefix: podIP.IP.String(),
			Nexthop:  transitSwitchPortAddr.IP.String(),
			Policy:   &nbdb.LogicalRouterStaticRoutePolicyDstIP,
			ExternalIDs: map[string]string{
				OvnZoneExternalIDKey:         OvnRemoteZone,
				VirtualMachineExternalIDsKey: pod.Labels[kubevirtv1.VirtualMachineNameLabel],
				NamespaceExternalIDsKey:      pod.Namespace,
			},
		}
		if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(nbClient, types.OVNClusterRouter, &route, func(item *nbdb.LogicalRouterStaticRoute) bool {
			matches := item.IPPrefix == route.IPPrefix && item.Policy != nil && *item.Policy == *route.Policy
			return matches
		}); err != nil {
			return fmt.Errorf("failed adding static route to remote pod: %v", err)
		}

	}
	return nil
}

// reconcileClusterDefaultNetworkRemoteZoneNeighbours handles neighbor reconciliation at the remote zone
// when a VM has live migrated. When the current node owns the VM's subnet but the VM is running at a
// different node (not owning the subnet), this function:
//   - Deletes stale logical switch ports to prevent ARPs from being answered with the wrong MAC
//   - Invalidates neighbor entries for pods in the same subnet so they use the gateway proxy MAC
//     to access the migrated VM
//
// NOTE: Only applies for IC.
func reconcileClusterDefaultNetworkRemoteZoneNeighbours(watchFactory *factory.WatchFactory, nbClient libovsdbclient.Client, pod *corev1.Pod, podAnnotation *util.PodAnnotation, nadName string) error {
	// TODO: Implement neighbour update for non IC
	if !config.OVNKubernetesFeature.EnableInterconnect {
		return nil
	}
	vmRunningAtNodeOwningSubnet, err := nodeContainsPodSubnet(watchFactory, pod.Spec.NodeName, podAnnotation, nadName)
	if err != nil {
		return err
	}

	currentNode, err := getCurrentNode()
	if err != nil {
		return err
	}
	currentNodeOwnsSubnet, err := nodeContainsPodSubnet(watchFactory, currentNode, podAnnotation, nadName)
	if err != nil {
		return err
	}
	if !vmRunningAtNodeOwningSubnet && currentNodeOwnsSubnet {
		// If we don't do this ARPs will be answer again with wrong mac
		if err := deleteStaleLogicalSwitchPorts(watchFactory, nbClient, pod); err != nil {
			return err
		}

		// Now that the VM is running at a node not owning its subnet the pods at the same subnet should use
		// the gateway to access it so we need to invalidate their  neighbor entries to point now to the
		// gateway proxy mac.
		if err := notifyProxy(podAnnotation.IPs); err != nil {
			return err
		}
	}
	return nil
}

func getCurrentNode() (string, error) {
	currentNode := os.Getenv("K8S_NODE")
	if currentNode == "" {
		return "", fmt.Errorf("missing environment variable K8S_NODE")
	}
	return currentNode, nil
}
