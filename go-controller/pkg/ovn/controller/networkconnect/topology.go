package networkconnect

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
)

// getConnectRouterName returns the connect router name for a CNC.
func getConnectRouterName(cncName string) string {
	return ovntypes.ConnectRouterPrefix + cncName
}

// getConnectRouterToNetworkRouterPortName returns the name of the port on the connect router
// that connects to the network router. For Layer3, includes the node name.
func getConnectRouterToNetworkRouterPortName(cncName, networkName, nodeName string) string {
	if nodeName == "" {
		// Layer2: no per-node ports
		return ovntypes.ConnectRouterToRouterPrefix + cncName + "_" + networkName
	}
	// Layer3: per-node ports
	return ovntypes.ConnectRouterToRouterPrefix + cncName + "_" + networkName + "_" + nodeName
}

// getNetworkRouterToConnectRouterPortName returns the name of the port on the network router
// that connects to the connect router. For Layer3, includes the node name.
func getNetworkRouterToConnectRouterPortName(networkName, nodeName, cncName string) string {
	if nodeName == "" {
		// Layer2: no per-node ports
		return ovntypes.RouterToConnectRouterPrefix + networkName + "_" + cncName
	}
	// Layer3: per-node ports
	return ovntypes.RouterToConnectRouterPrefix + networkName + "_" + nodeName + "_" + cncName
}

// ensureConnectRouter creates or updates the connect router for a CNC.
func (c *Controller) ensureConnectRouter(cnc *networkconnectv1.ClusterNetworkConnect, tunnelID int) error {
	routerName := getConnectRouterName(cnc.Name)
	// The default COPP is used for all routers in all networks.
	// Since the default COPP is created in SetupMaster() which is
	// called before the network connect controller is initialized (run() method),
	// we can safely fetch and use the default COPP here.
	copp, err := libovsdbops.GetCOPP(c.nbClient, &nbdb.Copp{Name: ovntypes.DefaultCOPPName})
	if err != nil {
		return fmt.Errorf("unable to create router control plane protection: %w", err)
	}
	router := &nbdb.LogicalRouter{
		Name: routerName,
		ExternalIDs: map[string]string{
			libovsdbops.ObjectNameKey.String():      cnc.Name,
			libovsdbops.OwnerControllerKey.String(): controllerName,
			libovsdbops.OwnerTypeKey.String():       libovsdbops.ClusterNetworkConnectOwnerType,
		},
		Options: map[string]string{
			// Set the tunnel key for the connect router
			"requested-tnl-key": strconv.Itoa(tunnelID),
		},
		Copp: &copp.UUID,
	}

	// Create or update the router
	err = libovsdbops.CreateOrUpdateLogicalRouter(c.nbClient, router, &router.ExternalIDs, &router.Options, &router.Copp)
	if err != nil {
		return fmt.Errorf("failed to create/update connect router %s for CNC %s: %v", routerName, cnc.Name, err)
	}

	klog.V(4).Infof("Ensured connect router %s with tunnel ID %d", routerName, tunnelID)
	return nil
}

// deleteConnectRouter deletes the connect router for a CNC.
func (c *Controller) deleteConnectRouter(cncName string) error {
	routerName := getConnectRouterName(cncName)

	router := &nbdb.LogicalRouter{Name: routerName}
	err := libovsdbops.DeleteLogicalRouter(c.nbClient, router)
	if err != nil {
		return fmt.Errorf("failed to delete connect router %s: %v", routerName, err)
	}

	klog.V(4).Infof("Deleted connect router %s", routerName)
	return nil
}

// parseOwnerKey parses an owner key like "layer3_1" into topology type and network ID.
func parseOwnerKey(owner string) (topologyType string, networkID int, err error) {
	if strings.HasPrefix(owner, ovntypes.Layer3Topology+"_") {
		topologyType = ovntypes.Layer3Topology
		_, err = fmt.Sscanf(owner, ovntypes.Layer3Topology+"_%d", &networkID)
	} else if strings.HasPrefix(owner, ovntypes.Layer2Topology+"_") {
		topologyType = ovntypes.Layer2Topology
		_, err = fmt.Sscanf(owner, ovntypes.Layer2Topology+"_%d", &networkID)
	} else {
		err = fmt.Errorf("unknown owner format: %s", owner)
	}
	return
}

// computeNodeInfo computes node information used by sync functions.
// It updates c.localZoneNode and returns:
//   - allNodes: all nodes in the cluster
//   - currentNodeIDs: set of all node IDs (as strings) for comparison
func (c *Controller) computeNodeInfo() ([]*corev1.Node, sets.Set[string], error) {
	allNodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list nodes: %v", err)
	}

	currentNodeIDs := sets.New[string]()

	for _, node := range allNodes {
		if util.GetNodeZone(node) == c.zone {
			// we only support 1 node per zone for this feature
			c.localZoneNode = node
		}
		nodeID, err := util.GetNodeID(node)
		if err != nil {
			continue
		}
		currentNodeIDs.Insert(strconv.Itoa(nodeID))
	}

	return allNodes, currentNodeIDs, nil
}

// syncNetworkConnections syncs all network connections for a CNC.
// STEP2: Create the patch ports connecting network router's to the connect router
// using IPs from the network subnet CNC annotation.
// STEP3: If PodNetworkConnect is enabled, create the logical router policies on network router's
// to steer traffic to the connect router for other connected networks.
// STEP4: If PodNetworkConnect is enabled, add static routes to connect router towards
// each of the connected networks.
func (c *Controller) syncNetworkConnections(cnc *networkconnectv1.ClusterNetworkConnect, allocatedSubnets map[string][]*net.IPNet,
	allNodes []*corev1.Node, currentNodeIDs sets.Set[string]) error {
	cncName := cnc.Name
	cncState := c.cncCache[cncName]

	desiredNetworks := sets.New[string]()
	for owner := range allocatedSubnets {
		desiredNetworks.Insert(owner)
	}
	networksToDelete := cncState.connectedNetworks.Difference(desiredNetworks)
	networksToCreate := desiredNetworks.Difference(cncState.connectedNetworks)

	klog.V(5).Infof("CNC %s: desiredNetworks=%v, connectedNetworks=%v, networksToCreate=%v, networksToDelete=%v",
		cncName, desiredNetworks.UnsortedList(), cncState.connectedNetworks.UnsortedList(),
		networksToCreate.UnsortedList(), networksToDelete.UnsortedList())

	var createOps, deleteOps []ovsdb.Operation
	var errs []error
	var err error

	// Track networks that had ops built successfully - cache will be updated only after transact succeeds
	// This ensures cache stays consistent with OVN NB even if transact fails
	var successfullyAdded []string
	var successfullyDeleted []string

	// Ensure ports, routing policies, and static routes for ALL desired networks.
	// All operations are idempotent (CreateOrUpdate), so we reconcile them on every sync.
	// This handles:
	// - New networks: creates ports, policies, and routes
	// - New nodes added to existing networks: creates ports and routes for new nodes
	// - Existing networks needing policies to newly added networks
	// - Node annotation changes (new subnets becoming available)
	for owner, subnets := range allocatedSubnets {
		isNewNetwork := networksToCreate.Has(owner)
		_, networkID, err := parseOwnerKey(owner)
		if err != nil {
			klog.Warningf("Failed to parse owner key %s: %v", owner, err)
			continue
		}

		// Find the network info for this owner
		netInfo, err := c.findNetworkByID(networkID)
		if err != nil {
			klog.V(4).Infof("Network with ID %d not found, skipping: %v", networkID, err)
			continue
		}

		// Check if the network router exists before trying to create ports on it.
		// The network might be registered in the network manager but not yet created in OVN NB.
		// If the router doesn't exist, skip this network and retry later.
		networkRouterName := netInfo.GetNetworkScopedClusterRouterName()
		_, err = libovsdbops.GetLogicalRouter(c.nbClient, &nbdb.LogicalRouter{Name: networkRouterName})
		if err != nil {
			klog.V(4).Infof("Network router %s for network %s does not exist yet, will retry: %v", networkRouterName, netInfo.GetNetworkName(), err)
			errs = append(errs, fmt.Errorf("network router %s for network %s does not exist yet: %w", networkRouterName, netInfo.GetNetworkName(), err))
			continue
		}

		klog.V(5).Infof("CNC %s: ensuring ports, policies and routes for network %s (new=%v)", cncName, netInfo.GetNetworkName(), isNewNetwork)

		// Create/update ports connecting the connect router and network router
		// Local node: full port pair with peer; Remote nodes: connect-router port only
		// This is idempotent - existing ports are unchanged, new node ports are created
		createOps, err = c.ensureConnectPortsOps(createOps, cnc, netInfo, subnets, allNodes)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to ensure connect ports for network %s: %w", netInfo.GetNetworkName(), err))
			continue
		}

		// Skip if this network's router doesn't exist yet (new network not yet in cache)
		if isNewNetwork {
			networkRouterName := netInfo.GetNetworkScopedClusterRouterName()
			_, err = libovsdbops.GetLogicalRouter(c.nbClient, &nbdb.LogicalRouter{Name: networkRouterName})
			if err != nil {
				// Already logged and error tracked in port creation phase
				continue
			}
		}

		klog.V(5).Infof("CNC %s: ensuring routing policies and static routes for network %s", cncName, netInfo.GetNetworkName())

		// Ensure routing policies on the network router
		createOps, err = c.ensureRoutingPoliciesOps(createOps, cncName, netInfo, allocatedSubnets)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to ensure routing policies for network %s: %w", netInfo.GetNetworkName(), err))
			continue
		}

		// Ensure static routes on the connect router
		createOps, err = c.ensureStaticRoutesOps(createOps, cnc, netInfo, subnets, allNodes)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to ensure static routes for network %s: %w", netInfo.GetNetworkName(), err))
			continue
		}

		// Track new networks for cache update after successful transact
		if isNewNetwork {
			successfullyAdded = append(successfullyAdded, owner)
		}
	}

	// transact the create operations
	if len(createOps) > 0 {
		if _, err := libovsdbops.TransactAndCheck(c.nbClient, createOps); err != nil {
			errs = append(errs, fmt.Errorf("failed to execute create operations for CNC %s: %w", cncName, err))
			return utilerrors.Join(errs...)
		}
		klog.Infof("CNC %s: executed %d sync network connections create operations", cncName, len(createOps))
	}

	connectRouterName := getConnectRouterName(cncName)

	// Cleanup ports and routes for nodes that no longer exist.
	// Delete connect router ports for nodes that no longer exist
	deleteOps, err = libovsdbops.DeleteLogicalRouterPortWithPredicateOps(c.nbClient, deleteOps, connectRouterName,
		func(item *nbdb.LogicalRouterPort) bool {
			// Only delete ports owned by this CNC
			if item.ExternalIDs[libovsdbops.ObjectNameKey.String()] != cncName {
				return false
			}
			nodeIDStr := item.ExternalIDs[libovsdbops.NodeIDKey.String()]
			// nodeID 0 is used for Layer2 networks which don't have per-node ports
			if nodeIDStr == "" || nodeIDStr == "0" {
				return false
			}
			// Delete if nodeID is not in current nodes
			return !currentNodeIDs.Has(nodeIDStr)
		})
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to cleanup ports for deleted nodes: %w", err))
	}

	// Delete static routes for nodes that no longer exist
	deleteOps, err = libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicateOps(c.nbClient, deleteOps, connectRouterName,
		func(item *nbdb.LogicalRouterStaticRoute) bool {
			// Only delete routes owned by this CNC
			if item.ExternalIDs[libovsdbops.ObjectNameKey.String()] != cncName {
				return false
			}
			nodeIDStr := item.ExternalIDs[libovsdbops.NodeIDKey.String()]
			// nodeID 0 is used for Layer2 networks which don't have per-node routes
			if nodeIDStr == "" || nodeIDStr == "0" {
				return false
			}
			// Delete if nodeID is not in current nodes
			return !currentNodeIDs.Has(nodeIDStr)
		})
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to cleanup routes for deleted nodes: %w", err))
	}

	// Cleanup networks that are no longer connected
	for owner := range networksToDelete {
		klog.V(5).Infof("CNC %s: cleaning up network owner=%s", cncName, owner)
		_, networkID, err := parseOwnerKey(owner)
		if err != nil {
			klog.Warningf("Failed to parse owner key %s: %v", owner, err)
			continue
		}

		// Find all ports matching this CNC and network ID (across all routers)
		// This allows cleanup even if the network has been deleted from the network manager
		ports, err := libovsdbops.FindLogicalRouterPortWithPredicate(c.nbClient, func(item *nbdb.LogicalRouterPort) bool {
			return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName &&
				item.ExternalIDs[libovsdbops.NetworkIDKey.String()] == strconv.Itoa(networkID)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to find ports for network %s: %w", owner, err))
			continue
		}

		// Collect unique router names from ports
		routerNames := sets.New[string]()
		for _, port := range ports {
			routerName := port.ExternalIDs[libovsdbops.RouterNameKey.String()]
			if routerName == "" {
				klog.Warningf("Port %s missing router name in ExternalIDs, skipping", port.Name)
				continue
			}
			routerNames.Insert(routerName)
		}

		// Delete network router ports
		for routerName := range routerNames {
			deleteOps, err = libovsdbops.DeleteLogicalRouterPortWithPredicateOps(c.nbClient, deleteOps, routerName,
				func(item *nbdb.LogicalRouterPort) bool {
					return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName &&
						item.ExternalIDs[libovsdbops.NetworkIDKey.String()] == strconv.Itoa(networkID) &&
						item.ExternalIDs[libovsdbops.RouterNameKey.String()] == routerName
				})
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to delete network router ports for network %s: %w", owner, err))
				continue
			}
		}

		// Find all routing policies owned by this CNC that need to be deleted
		// This includes:
		// 1. Policies on the disconnected network's router (routing FROM this network TO others)
		// 2. Policies on other networks' routers that reference this deleted network (routing TO this network)
		allPolicies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(c.nbClient, func(item *nbdb.LogicalRouterPolicy) bool {
			// Find all policies owned by this CNC
			if item.ExternalIDs[libovsdbops.ObjectNameKey.String()] != cncName {
				return false
			}
			// Match policies that either:
			// - Are FROM this network (NetworkKey == owner), OR
			// - Reference this network as destination (NetworkIDKey == networkID)
			policyNetworkKey := item.ExternalIDs[libovsdbops.NetworkKey.String()]
			policyNetworkID := item.ExternalIDs[libovsdbops.NetworkIDKey.String()]
			return policyNetworkKey == owner || policyNetworkID == strconv.Itoa(networkID)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to find routing policies for network %s: %w", owner, err))
			continue
		}

		// Group policies by router name and delete them
		policiesByRouter := make(map[string][]*nbdb.LogicalRouterPolicy)
		for _, policy := range allPolicies {
			routerName := policy.ExternalIDs[libovsdbops.RouterNameKey.String()]
			if routerName == "" {
				klog.Warningf("Policy %s missing router name in ExternalIDs, skipping", policy.UUID)
				continue
			}
			policiesByRouter[routerName] = append(policiesByRouter[routerName], policy)
		}

		// Delete policies from each router
		for routerName := range policiesByRouter {
			deleteOps, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(c.nbClient, deleteOps, routerName,
				func(item *nbdb.LogicalRouterPolicy) bool {
					// Match policies that either:
					// - Are FROM this network (NetworkKey == owner), OR
					// - Reference this network as destination (NetworkIDKey == networkID)
					return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName &&
						(item.ExternalIDs[libovsdbops.NetworkKey.String()] == owner ||
							item.ExternalIDs[libovsdbops.NetworkIDKey.String()] == strconv.Itoa(networkID)) &&
						item.ExternalIDs[libovsdbops.RouterNameKey.String()] == routerName
				})
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to delete routing policies from router %s for network %s: %w", routerName, owner, err))
				// Don't continue here - we still want to try other cleanups
			}
		}

		// Delete static routes from the connect router for this network
		// Note: Static routes don't have RouterNameKey in ExternalIDs, but we're deleting from connectRouterName
		// so matching by ObjectNameKey and NetworkKey is sufficient
		deleteOps, err = libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicateOps(c.nbClient, deleteOps, connectRouterName,
			func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName &&
					item.ExternalIDs[libovsdbops.NetworkKey.String()] == owner
			})
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to delete static routes for network %s: %w", owner, err))
			continue
		}

		// Track this network for cache cleanup after successful transact
		successfullyDeleted = append(successfullyDeleted, owner)
	}

	// Execute all operations (create and delete) in a single transaction
	if len(deleteOps) > 0 {
		if _, err := libovsdbops.TransactAndCheck(c.nbClient, deleteOps); err != nil {
			errs = append(errs, fmt.Errorf("failed to execute network connection operations for CNC %s: %w", cncName, err))
			return utilerrors.Join(errs...)
		}
		klog.Infof("CNC %s: executed %d sync network connections delete operations", cncName, len(deleteOps))
	}

	// Update cache only after successful transact - this ensures cache stays consistent with OVN NB
	for _, added := range successfullyAdded {
		cncState.connectedNetworks.Insert(added)
	}
	for _, deleted := range successfullyDeleted {
		cncState.connectedNetworks.Delete(deleted)
	}

	return utilerrors.Join(errs...)
}

// cleanupNetworkConnections removes all network connections for a CNC.
// This is called when a CNC is being deleted.
// 1. First delete network router ports from the network routers for this CNC
// 2. Then delete routing policies on the network routers for this CNC
func (c *Controller) cleanupNetworkConnections(cncName string) error {
	var ops []ovsdb.Operation

	// Find all ports owned by this CNC (across all routers and networks)
	// This allows cleanup even if networks have been deleted from the network manager
	allPorts, err := libovsdbops.FindLogicalRouterPortWithPredicate(c.nbClient, func(item *nbdb.LogicalRouterPort) bool {
		return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName
	})
	if err != nil {
		return fmt.Errorf("failed to find ports for CNC %s: %w", cncName, err)
	}

	// Collect unique router names from ports
	routerNames := sets.New[string]()

	for _, port := range allPorts {
		routerName := port.ExternalIDs[libovsdbops.RouterNameKey.String()]
		if routerName == "" {
			klog.Warningf("Port %s missing router name in ExternalIDs, skipping", port.Name)
			continue
		}
		routerNames.Insert(routerName)
	}

	// Delete all ports for this CNC
	// All deletions happen in a single transaction, so order doesn't matter
	// OVN handles peer references gracefully when both ports are deleted atomically
	for routerName := range routerNames {
		if routerName == getConnectRouterName(cncName) {
			// the whole connect router will be deleted when the CNC is deleted
			// so no need to delete the ports on the connect router
			continue
		}
		ops, err = libovsdbops.DeleteLogicalRouterPortWithPredicateOps(c.nbClient, ops, routerName,
			func(item *nbdb.LogicalRouterPort) bool {
				return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName &&
					item.ExternalIDs[libovsdbops.RouterNameKey.String()] == routerName
			})
		if err != nil {
			return fmt.Errorf("failed to delete router ports for router %s: %w", routerName, err)
		}
	}

	// Find all routing policies owned by this CNC
	allPolicies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(c.nbClient, func(item *nbdb.LogicalRouterPolicy) bool {
		return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName
	})
	if err != nil {
		return fmt.Errorf("failed to find routing policies for CNC %s: %w", cncName, err)
	}

	// Group policies by router name and delete them
	policiesByRouter := make(map[string][]*nbdb.LogicalRouterPolicy)
	for _, policy := range allPolicies {
		routerName := policy.ExternalIDs[libovsdbops.RouterNameKey.String()]
		if routerName == "" {
			klog.Warningf("Policy %s missing router name in ExternalIDs, skipping", policy.UUID)
			continue
		}
		policiesByRouter[routerName] = append(policiesByRouter[routerName], policy)
	}

	// Delete policies from each router
	for routerName := range policiesByRouter {
		ops, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(c.nbClient, ops, routerName,
			func(item *nbdb.LogicalRouterPolicy) bool {
				return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName &&
					item.ExternalIDs[libovsdbops.RouterNameKey.String()] == routerName
			})
		if err != nil {
			return fmt.Errorf("failed to delete routing policies from router %s: %w", routerName, err)
		}
	}

	// Execute all delete operations
	if len(ops) > 0 {
		if _, err := libovsdbops.TransactAndCheck(c.nbClient, ops); err != nil {
			return fmt.Errorf("failed to execute cleanup operations for CNC %s: %w", cncName, err)
		}
		klog.Infof("CNC %s: executed %d cleanup operations", cncName, len(ops))
	}

	return nil
}

// ensureConnectPortsOps returns ops to create the ports connecting the connect router and network router.
// For Layer3:
//   - Local node: creates full port pair (connect-router ↔ network-router) with peer relationship
//   - Remote nodes: creates only the connect-router side port (with tunnel key, no peer)
//
// For Layer2: creates a single port pair (transit router is distributed)
func (c *Controller) ensureConnectPortsOps(ops []ovsdb.Operation, cnc *networkconnectv1.ClusterNetworkConnect, netInfo util.NetInfo,
	subnets []*net.IPNet, nodes []*corev1.Node) ([]ovsdb.Operation, error) {
	cncName := cnc.Name
	networkName := netInfo.GetNetworkName()
	connectRouterName := getConnectRouterName(cncName)
	networkRouterName := netInfo.GetNetworkScopedClusterRouterName()
	networkID := netInfo.GetNetworkID()

	// Validate subnets are allocated for tunnel key calculation
	if len(subnets) == 0 {
		return nil, fmt.Errorf("no subnets allocated for network %s", networkName)
	}

	var err error
	if netInfo.TopologyType() == ovntypes.Layer3Topology {
		// For Layer3 networks, create ports for all nodes
		for _, node := range nodes {
			nodeID, err := util.GetNodeID(node)
			if err != nil {
				// node update event will trigger the reconciliation again.
				klog.V(4).Infof("Node %s does not have node ID, skipping: %v", node.Name, err)
				continue
			}

			// Calculate the /31 subnet for this node from the allocated subnet
			p2pSubnets, err := calculateP2PSubnets(subnets, nodeID)
			if err != nil {
				return nil, fmt.Errorf("failed to calculate P2P subnets for node %s: %v", node.Name, err)
			}
			connectPortIPs, networkPortIPs := getP2PIPs(p2pSubnets)

			// Calculate tunnel key using the unified function
			tunnelKey := GetTunnelKey(cnc.Spec.ConnectSubnets, subnets, ovntypes.Layer3Topology, nodeID)

			connectPortName := getConnectRouterToNetworkRouterPortName(cncName, networkName, node.Name)
			networkPortName := getNetworkRouterToConnectRouterPortName(networkName, node.Name, cncName)

			isLocalNode := util.GetNodeZone(node) == c.zone

			if isLocalNode {
				// Local node: create both ports with peer relationship
				ops, err = c.createRouterPortOps(ops, connectRouterName, connectPortName, connectPortIPs, networkPortName, cncName,
					networkID, nodeID, tunnelKey, "")
				if err != nil {
					return nil, fmt.Errorf("failed to create connect router port ops %s: %v", connectPortName, err)
				}
				ops, err = c.createRouterPortOps(ops, networkRouterName, networkPortName, networkPortIPs, connectPortName, cncName,
					networkID, nodeID, 0, "")
				if err != nil {
					return nil, fmt.Errorf("failed to create network router port ops %s: %v", networkPortName, err)
				}
			} else {
				// Remote node: create only the connect-router side port with requested-chassis set
				// This makes the port type: remote in SB, enabling cross-zone tunneling
				ops, err = c.createRouterPortOps(ops, connectRouterName, connectPortName, connectPortIPs, "", cncName,
					networkID, nodeID, tunnelKey, node.Name)
				if err != nil {
					return nil, fmt.Errorf("failed to create remote connect router port ops %s: %v", connectPortName, err)
				}
				// Delete the network router port if it exists (cleanup from when node was local)
				ops, err = libovsdbops.DeleteLogicalRouterPortWithPredicateOps(c.nbClient, ops, networkRouterName,
					func(item *nbdb.LogicalRouterPort) bool {
						return item.Name == networkPortName
					})
				if err != nil {
					return nil, fmt.Errorf("failed to delete network router port ops %s: %v", networkPortName, err)
				}
			}
		}
	}
	if netInfo.TopologyType() == ovntypes.Layer2Topology {
		// For Layer2 networks, create a single port pair to the transit router
		connectPortIPs, networkPortIPs := getP2PIPs(subnets)

		// Calculate tunnel key using the unified function (nodeID=0 for Layer2)
		tunnelKey := GetTunnelKey(cnc.Spec.ConnectSubnets, subnets, ovntypes.Layer2Topology, 0)

		connectPortName := getConnectRouterToNetworkRouterPortName(cncName, networkName, "")
		networkPortName := getNetworkRouterToConnectRouterPortName(networkName, "", cncName)

		// Create the port on the connect router (with peer set)
		ops, err = c.createRouterPortOps(ops, connectRouterName, connectPortName, connectPortIPs, networkPortName, cncName,
			networkID, 0, tunnelKey, "")
		if err != nil {
			return nil, fmt.Errorf("failed to create connect router port ops %s: %v", connectPortName, err)
		}

		// Create the peer port on the transit router (with peer set)
		ops, err = c.createRouterPortOps(ops, networkRouterName, networkPortName, networkPortIPs, connectPortName, cncName,
			networkID, 0, 0, "")
		if err != nil {
			return nil, fmt.Errorf("failed to create network router port ops %s: %v", networkPortName, err)
		}
	}

	return ops, nil
}

// createRouterPortOps returns ops to create a logical router port with peer and tunnel key set.
// If remoteChassisName is provided, the port is configured as a remote port (type: remote in SB).
func (c *Controller) createRouterPortOps(ops []ovsdb.Operation, routerName, portName string, ipNets []*net.IPNet, peerPortName string,
	cncName string, networkID, nodeID, tunnelKey int, remoteChassisName string) ([]ovsdb.Operation, error) {
	if len(ipNets) == 0 {
		return nil, fmt.Errorf("no IPNets provided for router port %s", portName)
	}

	dbIndexes := libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPortClusterNetworkConnect, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.NodeIDKey:     strconv.Itoa(nodeID),
			libovsdbops.NetworkIDKey:  strconv.Itoa(networkID),
			libovsdbops.ObjectNameKey: cncName,
			libovsdbops.RouterNameKey: routerName,
		})

	port := &nbdb.LogicalRouterPort{
		Name:        portName,
		MAC:         util.IPAddrToHWAddr(ipNets[0].IP).String(),
		Networks:    util.IPNetsToStringSlice(ipNets),
		ExternalIDs: dbIndexes.GetExternalIDs(),
	}
	if peerPortName != "" {
		port.Peer = &peerPortName
	}

	options := map[string]string{}
	if tunnelKey != 0 {
		options[libovsdbops.RequestedTnlKey] = strconv.Itoa(tunnelKey)
	}
	if remoteChassisName != "" {
		options[libovsdbops.RequestedChassis] = remoteChassisName
	}
	if len(options) > 0 {
		port.Options = options
	}

	router := &nbdb.LogicalRouter{Name: routerName}
	var err error
	ops, err = libovsdbops.CreateOrUpdateLogicalRouterPortOps(c.nbClient, ops, router, port, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create port ops %s on router %s: %v", portName, routerName, err)
	}

	klog.V(5).Infof("Created/updated router port ops %s on %s with peer %s and tunnel key %d, options %v", portName, routerName, peerPortName, tunnelKey, options)
	return ops, nil
}

// ensureRoutingPoliciesOps returns ops to create routing policies on the network router to steer traffic to connected networks.
// For Layer3: creates policy for the local node only (each zone handles its own node)
// For Layer2: creates a single policy (transit router is distributed)
func (c *Controller) ensureRoutingPoliciesOps(ops []ovsdb.Operation, cncName string, srcNetwork util.NetInfo,
	allocatedSubnets map[string][]*net.IPNet) ([]ovsdb.Operation, error) {
	networkRouterName := srcNetwork.GetNetworkScopedClusterRouterName()

	// Get the source network's subnets to build the inport match
	srcSubnets := srcNetwork.Subnets()
	if len(srcSubnets) == 0 {
		return nil, fmt.Errorf("source network %s has no subnets", srcNetwork.GetNetworkName())
	}

	// Get the source network's connect subnets - these determine the nexthop for routing policies
	// The nexthop is the connect-router's port IP that connects to the source network
	srcOwnerKey := fmt.Sprintf("%s_%d", srcNetwork.TopologyType(), srcNetwork.GetNetworkID())
	srcConnectSubnets, found := allocatedSubnets[srcOwnerKey]
	if !found || len(srcConnectSubnets) == 0 {
		return nil, fmt.Errorf("source network %s connect subnets not found in allocated subnets", srcNetwork.GetNetworkName())
	}

	// Calculate inport and nexthop once - these are constant for the source network
	// The nexthop is the connect-router's port that connects to the source network.
	// Traffic flow: srcNetwork router -> connect-router (via srcConnectSubnets) -> dstNetwork
	var inportName string
	var nexthops []net.IP

	if srcNetwork.TopologyType() == ovntypes.Layer3Topology {
		// For Layer3, create policy for the local node
		// If there's no local node (node moved to different zone), skip policy creation.
		// The controller in the node's zone will handle its policies.
		if c.localZoneNode == nil {
			klog.Infof("No local node found for zone %s, skipping routing policy "+
				"creation for Layer3 network %s (node moved to different zone)", c.zone, srcNetwork.GetNetworkName())
			return ops, nil
		}
		nodeID, err := util.GetNodeID(c.localZoneNode)
		if err != nil {
			return nil, fmt.Errorf("local node %s does not have node ID: %v", c.localZoneNode.Name, err)
		}

		inportName = srcNetwork.GetNetworkScopedSwitchToRouterPortName(c.localZoneNode.Name)

		p2pSubnets, err := calculateP2PSubnets(srcConnectSubnets, nodeID)
		if err != nil {
			return nil, fmt.Errorf("failed to calculate P2P subnets for node %s: %v", c.localZoneNode.Name, err)
		}
		connectPortIPs, _ := getP2PIPs(p2pSubnets)
		nexthops = util.IPNetsToIPs(connectPortIPs)
	} else if srcNetwork.TopologyType() == ovntypes.Layer2Topology {
		// For Layer2, create a single policy (nodeName ignored for Layer2 switch)
		inportName = srcNetwork.GetNetworkScopedSwitchToRouterPortName("")
		connectPortIPs, _ := getP2PIPs(srcConnectSubnets)
		nexthops = util.IPNetsToIPs(connectPortIPs)
	}

	// For each other connected network, add a routing policy.
	// Note: We iterate allocatedSubnets again here (it's also iterated by the caller) because
	// this creates the full mesh of policies. The outer loop in syncNetworkConnections selects
	// the SOURCE network (where policies are created), while this inner loop finds all
	// DESTINATION networks (what the policies route to). This is O(N²) which is intentional
	// for a full mesh connectivity between N networks.
	// This is typically fine since number of networks that are expected to be connected by a CNC is small, eg. 10.
	for owner := range allocatedSubnets {
		_, dstNetworkID, err := parseOwnerKey(owner)
		if err != nil {
			continue
		}

		// Skip if this is the same network
		if dstNetworkID == srcNetwork.GetNetworkID() {
			continue
		}

		// Find destination network info
		dstNetwork, err := c.findNetworkByID(dstNetworkID)
		if err != nil {
			klog.V(4).Infof("Destination network %d not found, skipping policy: %v", dstNetworkID, err)
			continue
		}

		// Get destination network's pod subnets
		dstPodSubnets := dstNetwork.Subnets()

		// Create policies for each destination subnet
		ops, err = c.createRoutingPoliciesOps(ops, dstNetworkID, networkRouterName, inportName, dstPodSubnets, srcOwnerKey, nexthops, cncName)
		if err != nil {
			return nil, err
		}
	}
	klog.V(5).Infof("Created/updated routing policies ops on %s: %s -> %s", networkRouterName, inportName, nexthops)

	return ops, nil
}

// createRoutingPoliciesOps returns ops to create logical router policies.
func (c *Controller) createRoutingPoliciesOps(ops []ovsdb.Operation, dstNetworkID int, routerName, inportName string,
	dstSubnets []config.CIDRNetworkEntry, srcOwner string, nexthops []net.IP, cncName string) ([]ovsdb.Operation, error) {
	for _, dstSubnet := range dstSubnets {
		// Determine IP version and get appropriate nexthop
		var nexthop string
		for _, nh := range nexthops {
			isIPv4Subnet := utilnet.IsIPv4(dstSubnet.CIDR.IP)
			isIPv4Nexthop := utilnet.IsIPv4(nh)
			if isIPv4Subnet == isIPv4Nexthop {
				nexthop = nh.String()
				break
			}
		}
		if nexthop == "" {
			continue
		}

		// Build the match string
		ipFamily := "v4"
		ipVersion := "ip4"
		if utilnet.IsIPv6(dstSubnet.CIDR.IP) {
			ipFamily = "v6"
			ipVersion = "ip6"
		}
		match := fmt.Sprintf(`inport == "%s" && %s.dst == %s`, inportName, ipVersion, dstSubnet.CIDR.String())

		dbIndexes := libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyClusterNetworkConnect, controllerName,
			map[libovsdbops.ExternalIDKey]string{
				libovsdbops.NetworkIDKey:  strconv.Itoa(dstNetworkID),
				libovsdbops.NetworkKey:    srcOwner,
				libovsdbops.ObjectNameKey: cncName,
				libovsdbops.IPFamilyKey:   ipFamily,
				libovsdbops.RouterNameKey: routerName,
			})
		policy := &nbdb.LogicalRouterPolicy{
			Priority:    ovntypes.NetworkConnectPolicyPriority,
			Match:       match,
			Action:      nbdb.LogicalRouterPolicyActionReroute,
			Nexthops:    []string{nexthop},
			ExternalIDs: dbIndexes.GetExternalIDs(),
		}

		var err error
		ops, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(c.nbClient, ops, routerName, policy,
			libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIndexes, nil))
		if err != nil {
			return nil, fmt.Errorf("failed to create routing policy ops on %s: %v", routerName, err)
		}

		klog.V(5).Infof("Created/updated routing policy ops on %s: %s -> %s", routerName, match, nexthop)
	}

	return ops, nil
}

// ensureStaticRoutesOps returns ops to create static routes on the connect router for reaching network subnets.
func (c *Controller) ensureStaticRoutesOps(ops []ovsdb.Operation, cnc *networkconnectv1.ClusterNetworkConnect,
	netInfo util.NetInfo, subnets []*net.IPNet, nodes []*corev1.Node) ([]ovsdb.Operation, error) {
	cncName := cnc.Name
	networkName := netInfo.GetNetworkName()
	connectRouterName := getConnectRouterName(cncName)

	// Build owner key for consistent external ID tracking (same format as policies)
	networkOwner := fmt.Sprintf("%s_%d", netInfo.TopologyType(), netInfo.GetNetworkID())

	// Get the network's pod subnets
	podSubnets := netInfo.Subnets()

	var err error
	if netInfo.TopologyType() == ovntypes.Layer3Topology {
		// For Layer3, create routes to each node's subnet slice
		for _, node := range nodes {
			nodeID, err := util.GetNodeID(node)
			if err != nil {
				continue
			}

			// Get the node's subnet from the network
			nodeSubnets, err := c.getNodeSubnet(netInfo, node.Name)
			if err != nil {
				klog.V(4).Infof("Could not get node subnet for %s on network %s: %v", node.Name, networkName, err)
				continue
			}

			// Calculate nexthop (second IP of the P2P subnet on network router side)
			p2pSubnets, err := calculateP2PSubnets(subnets, nodeID)
			if err != nil {
				continue
			}
			_, networkPortIPs := getP2PIPs(p2pSubnets)
			nexthops := util.IPNetsToIPs(networkPortIPs)

			// Create route for this node's subnets
			ops, err = c.createStaticRoutesOps(ops, networkOwner, connectRouterName, nodeSubnets, nexthops, cncName, nodeID)
			if err != nil {
				return nil, err
			}
		}
	}
	if netInfo.TopologyType() == ovntypes.Layer2Topology {
		// For Layer2, create a single route to the network's subnets
		_, networkPortIPs := getP2PIPs(subnets)
		nexthops := util.IPNetsToIPs(networkPortIPs)

		var podSubnetIPNets []*net.IPNet
		for _, entry := range podSubnets {
			podSubnetIPNets = append(podSubnetIPNets, entry.CIDR)
		}

		ops, err = c.createStaticRoutesOps(ops, networkOwner, connectRouterName, podSubnetIPNets, nexthops, cncName, 0)
		if err != nil {
			return nil, err
		}
	}

	return ops, nil
}

// createStaticRoutesOps returns ops to create logical router static routes.
func (c *Controller) createStaticRoutesOps(ops []ovsdb.Operation, networkOwner, routerName string, dstSubnets []*net.IPNet, nexthops []net.IP, cncName string, nodeID int) ([]ovsdb.Operation, error) {
	for _, dstSubnet := range dstSubnets {
		// Find matching nexthop (same IP family)
		ipFamily := "v4"
		var nexthop string
		for _, nh := range nexthops {
			isIPv4Subnet := utilnet.IsIPv4(dstSubnet.IP)
			isIPv4Nexthop := utilnet.IsIPv4(nh)
			if isIPv4Subnet == isIPv4Nexthop {
				nexthop = nh.String()
				if !isIPv4Subnet {
					ipFamily = "v6"
				}
				break
			}
		}
		if nexthop == "" {
			continue
		}

		dbIndexes := libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterStaticRouteClusterNetworkConnect, controllerName,
			map[libovsdbops.ExternalIDKey]string{
				libovsdbops.NetworkKey:    networkOwner,
				libovsdbops.NodeIDKey:     strconv.Itoa(nodeID),
				libovsdbops.ObjectNameKey: cncName, // CNC name
				libovsdbops.IPFamilyKey:   ipFamily,
			})
		route := &nbdb.LogicalRouterStaticRoute{
			IPPrefix:    dstSubnet.String(),
			Nexthop:     nexthop,
			ExternalIDs: dbIndexes.GetExternalIDs(),
		}

		var err error
		// Don't limit fields to update - when node subnets change, IPPrefix and Nexthop need to be updated too
		ops, err = libovsdbops.CreateOrUpdateLogicalRouterStaticRoutesWithPredicateOps(c.nbClient, ops, routerName, route,
			libovsdbops.GetPredicate[*nbdb.LogicalRouterStaticRoute](dbIndexes, nil))
		if err != nil {
			return nil, fmt.Errorf("failed to create static route ops on %s: %v", routerName, err)
		}

		klog.V(5).Infof("Created/updated static route ops on %s: %s via %s", routerName, dstSubnet.String(), nexthop)
	}

	return ops, nil
}

// findNetworkByID finds a network by its ID.
func (c *Controller) findNetworkByID(networkID int) (util.NetInfo, error) {
	var foundNetwork util.NetInfo
	err := c.networkManager.DoWithLock(func(network util.NetInfo) error {
		if network.GetNetworkID() == networkID {
			foundNetwork = network
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to iterate networks: %v", err)
	}
	if foundNetwork == nil {
		return nil, fmt.Errorf("network with ID %d not found", networkID)
	}
	return foundNetwork, nil
}

// getNodeSubnet gets the subnet allocated to a specific node for a network.
func (c *Controller) getNodeSubnet(netInfo util.NetInfo, nodeName string) ([]*net.IPNet, error) {
	// For Layer3 networks, each node gets a subnet slice
	// Get node info to find its allocated subnet
	node, err := c.nodeLister.Get(nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %v", nodeName, err)
	}

	// Parse the subnet for this network
	nodeSubnets, err := util.ParseNodeHostSubnetAnnotation(node, netInfo.GetNetworkName())
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			// we must continue setting up the next network, the node update event will trigger the reconciliation again.
			return nil, nil
		}
		return nil, fmt.Errorf("failed to parse node subnet for network %s: %v", netInfo.GetNetworkName(), err)
	}
	return nodeSubnets, nil
}

// syncServiceConnectivity handles ClusterIPServiceNetwork connectivity.
// It finds ClusterIP LBs from each connected network and attaches them to the
// switches of all OTHER connected networks.
// NOTE: Service deletions are handled automatically by OVN weak references.
// When an LB is deleted by the services controller, OVSDB automatically removes
// the LB UUID from all LogicalSwitch.load_balancer columns (weak ref with min=0).
// NOTE2: Network connections and disconnections OR CNC connectivity field updates
// will trigger the reconciliation again and the service connectivity sync will be called through
// those approriate reconciliation functions.
func (c *Controller) syncServiceConnectivity(cnc *networkconnectv1.ClusterNetworkConnect) error {
	cncState, exists := c.cncCache[cnc.Name]
	if !exists || cncState == nil {
		return fmt.Errorf("CNC %s state not found in cache or is nil", cnc.Name)
	}

	// Get list of connected networks from cache
	connectedNetworks := cncState.connectedNetworks.UnsortedList()
	if len(connectedNetworks) < 2 {
		klog.V(5).Infof("CNC %s has less than 2 connected networks, skipping service connectivity", cnc.Name)
		return nil
	}

	// Collect all LBs per network and all switches per connectednetwork
	networkLBs := make(map[string][]*nbdb.LoadBalancer)       // networkOwnerKey -> ClusterIP LBs
	networkSwitches := make(map[string][]*nbdb.LogicalSwitch) // networkOwnerKey -> switches

	for _, networkOwnerKey := range connectedNetworks {
		// Parse network owner key: "topology_networkID" (e.g., "layer3_5")
		netInfo, err := c.parseNetworkOwnerKey(networkOwnerKey)
		if err != nil {
			klog.Warningf("Failed to parse network owner key %s: %v", networkOwnerKey, err)
			continue
		}

		// Find ClusterIP LBs for this network
		lbs, err := c.findClusterIPLoadBalancers(netInfo)
		if err != nil {
			return fmt.Errorf("failed to find ClusterIP LBs for network %s: %w", netInfo.GetNetworkName(), err)
		}
		networkLBs[networkOwnerKey] = lbs
		klog.V(5).Infof("Found %d ClusterIP LBs for network %s", len(lbs), netInfo.GetNetworkName())

		// Find switches for this network (only for local zone node to avoid predicate scan)
		switches, err := c.findNetworkSwitches(netInfo)
		if err != nil {
			return fmt.Errorf("failed to find switches for network %s: %w", netInfo.GetNetworkName(), err)
		}
		networkSwitches[networkOwnerKey] = switches
		klog.V(5).Infof("Found %d switches for network %s", len(switches), netInfo.GetNetworkName())
	}

	// Build ops to attach each network's LBs to all OTHER networks' switches
	var ops []ovsdb.Operation
	for srcNetworkKey, lbs := range networkLBs {
		if len(lbs) == 0 {
			continue
		}

		for dstNetworkKey, switches := range networkSwitches {
			// Skip same network - LBs are already attached by services controller
			if srcNetworkKey == dstNetworkKey {
				continue
			}

			// Attach srcNetwork's LBs to dstNetwork's switches
			for _, sw := range switches {
				addOps, err := libovsdbops.AddLoadBalancersToLogicalSwitchOps(c.nbClient, nil, sw, lbs...)
				if err != nil {
					return fmt.Errorf("failed to create ops to attach LBs to switch %s: %w", sw.Name, err)
				}
				ops = append(ops, addOps...)
				klog.V(5).Infof("Adding %d LBs from network %s to switch %s (network %s)",
					len(lbs), srcNetworkKey, sw.Name, dstNetworkKey)
			}
		}
	}

	if len(ops) == 0 {
		klog.V(5).Infof("No LB attachment operations needed for CNC %s", cnc.Name)
		return nil
	}

	// Execute all ops in a single transaction
	_, err := libovsdbops.TransactAndCheck(c.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to attach LBs for CNC %s: %w", cnc.Name, err)
	}

	klog.V(4).Infof("Successfully synced service connectivity for CNC %s (%d operations)", cnc.Name, len(ops))
	return nil
}

// parseNetworkOwnerKey parses a network owner key (e.g., "layer3_5") and returns the NetInfo.
// TODO(tssurya): Make this a common utility function and reuse the one in clustermanager/networkconnect/controller.go
func (c *Controller) parseNetworkOwnerKey(ownerKey string) (util.NetInfo, error) {
	// Format: "topology_networkID"
	parts := strings.Split(ownerKey, "_")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid network owner key format: %s", ownerKey)
	}

	networkID, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid network ID in owner key %s: %v", ownerKey, err)
	}

	return c.findNetworkByID(networkID)
}

// findClusterIPLoadBalancers finds all ClusterIP service load balancers for a network.
// ClusterIP LBs have names ending with "_cluster" and are tagged with the network name.
func (c *Controller) findClusterIPLoadBalancers(netInfo util.NetInfo) ([]*nbdb.LoadBalancer, error) {
	networkName := netInfo.GetNetworkName()

	predicate := func(lb *nbdb.LoadBalancer) bool {
		// Must be a Service LB
		if lb.ExternalIDs[ovntypes.LoadBalancerKindExternalID] != "Service" {
			return false
		}
		// Must belong to this network
		if lb.ExternalIDs[ovntypes.NetworkExternalID] != networkName {
			return false
		}
		// Must be a ClusterIP LB (name contains "_cluster")
		return strings.Contains(lb.Name, "_cluster")
	}

	return libovsdbops.FindLoadBalancersWithPredicate(c.nbClient, predicate)
}

// findNetworkSwitches finds logical switches for a network for the local zone node.
// For Layer2, there's a single distributed switch (localZoneNode is ignored).
// For Layer3, there's a switch per node, and we use direct lookup by constructing
// the switch name from the node name (avoiding predicate scan over all switches).
// We only support 1 node per zone for this feature.
func (c *Controller) findNetworkSwitches(netInfo util.NetInfo) ([]*nbdb.LogicalSwitch, error) {
	// For Layer3, there's a switch per node - use direct lookup by name
	if netInfo.TopologyType() == ovntypes.Layer3Topology && c.localZoneNode == nil {
		klog.V(5).Infof("No local zone node found, returning empty switch list for network %s", netInfo.GetNetworkName())
		return nil, nil
	}
	var switchName string
	if netInfo.TopologyType() == ovntypes.Layer2Topology {
		switchName = netInfo.GetNetworkScopedSwitchName("")
	} else if netInfo.TopologyType() == ovntypes.Layer3Topology {
		switchName = netInfo.GetNetworkScopedSwitchName(c.localZoneNode.Name)
	} else {
		return nil, fmt.Errorf("unsupported topology type: %s", netInfo.TopologyType())
	}
	sw := &nbdb.LogicalSwitch{Name: switchName}
	sw, err := libovsdbops.GetLogicalSwitch(c.nbClient, sw)
	if err != nil {
		return nil, fmt.Errorf("failed to get switch %s: %w", switchName, err)
	}
	return []*nbdb.LogicalSwitch{sw}, nil
}
