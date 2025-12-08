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

const (
	// connectRouterPrefix is the prefix for connect router names
	connectRouterPrefix = "connect-router-"

	// policyPriority is the priority for routing policies on network routers
	// This should be higher than normal routing but lower than critical policies
	policyPriority = 9001
)

// getConnectRouterName returns the connect router name for a CNC.
func getConnectRouterName(cncName string) string {
	return connectRouterPrefix + cncName
}

// getConnectRouterToNetworkRouterPortName returns the name of the port on the connect router
// that connects to the network router. For Layer3, includes the node name.
func getConnectRouterToNetworkRouterPortName(cncName, networkName, nodeName string) string {
	if nodeName == "" {
		// Layer2: no per-node ports
		return ovntypes.ConnectRouterToRouterPrefix + cncName + "-" + networkName
	}
	// Layer3: per-node ports
	return ovntypes.ConnectRouterToRouterPrefix + cncName + "-" + networkName + "-" + nodeName
}

// getNetworkRouterToConnectRouterPortName returns the name of the port on the network router
// that connects to the connect router. For Layer3, includes the node name.
func getNetworkRouterToConnectRouterPortName(networkName, nodeName, cncName string) string {
	if nodeName == "" {
		// Layer2: no per-node ports
		return ovntypes.RouterToConnectRouterPrefix + networkName + "-" + cncName
	}
	// Layer3: per-node ports
	return ovntypes.RouterToConnectRouterPrefix + networkName + "-" + nodeName + "-" + cncName
}

// ensureConnectRouter creates or updates the connect router for a CNC.
func (c *Controller) ensureConnectRouter(cnc *networkconnectv1.ClusterNetworkConnect, tunnelID int) error {
	routerName := getConnectRouterName(cnc.Name)
	dbIDs := libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterClusterNetworkConnect, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: cnc.Name,
		})
	router := &nbdb.LogicalRouter{
		Name:        routerName,
		ExternalIDs: dbIDs.GetExternalIDs(),
		Options: map[string]string{
			// Set the tunnel key for the connect router
			"requested-tnl-key": strconv.Itoa(tunnelID),
		},
	}

	// Create or update the router
	err := libovsdbops.CreateOrUpdateLogicalRouter(c.nbClient, router, &router.ExternalIDs, &router.Options)
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

// syncNetworkConnections syncs all network connections for a CNC.
func (c *Controller) syncNetworkConnections(cnc *networkconnectv1.ClusterNetworkConnect, allocatedSubnets map[string][]*net.IPNet) error {
	cncName := cnc.Name
	cncState := c.cncCache[cncName]

	// Get all nodes - the connect-router needs static routes to ALL node subnets
	allNodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list nodes: %v", err)
	}

	desiredNetworks := sets.New[string]()
	for owner := range allocatedSubnets {
		desiredNetworks.Insert(owner)
	}
	networksToDelete := cncState.connectedNetworks.Difference(desiredNetworks)
	networksToCreate := desiredNetworks.Difference(cncState.connectedNetworks)

	klog.V(5).Infof("CNC %s: desiredNetworks=%v, connectedNetworks=%v, networksToCreate=%v, networksToDelete=%v",
		cncName, desiredNetworks.UnsortedList(), cncState.connectedNetworks.UnsortedList(),
		networksToCreate.UnsortedList(), networksToDelete.UnsortedList())

	var ops []ovsdb.Operation
	var errs []error

	// Track networks that had ops built successfully - cache will be updated only after transact succeeds
	// This ensures cache stays consistent with OVN NB even if transact fails
	type networkToAdd struct {
		owner             string
		networkRouterName string
	}
	var successfullyAdded []networkToAdd
	var successfullyDeleted []string

	// Create ports for NEW networks only (ports don't normally change after creation - unless the
	// network is deleted and re-created OR the CNC annotation subnet allocation for this network changed which
	// is an unlikely scenario).
	for owner := range networksToCreate {
		klog.V(5).Infof("CNC %s: creating ports for new network owner=%s", cncName, owner)
		subnets, ok := allocatedSubnets[owner]
		if !ok {
			klog.Warningf("Network %s not found in allocated subnets", owner)
			continue
		}
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

		// Create ports connecting the connect router and network router
		// Local node: full port pair with peer; Remote nodes: connect-router port only
		ops, err = c.ensureConnectPortsOps(ops, cnc, netInfo, subnets, allNodes)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to ensure connect ports for network %s: %w", netInfo.GetNetworkName(), err))
			continue
		}

		// Track this network for cache update after successful transact
		successfullyAdded = append(successfullyAdded, networkToAdd{
			owner:             owner,
			networkRouterName: networkRouterName,
		})
	}

	// Ensure routing policies and static routes for ALL desired networks.
	// These operations are idempotent (CreateOrUpdate), so we reconcile them on every sync.
	// It's tricky to use per network per node caches to be able to react on both
	// source network, destination network, and node events for the policies and static routes.
	// This handles:
	// - New networks needing policies/routes
	// - Existing networks needing policies to newly added networks
	// - Node annotation changes (new subnets becoming available)
	// - New nodes being added to the cluster
	for owner, subnets := range allocatedSubnets {
		_, networkID, err := parseOwnerKey(owner)
		if err != nil {
			klog.Warningf("Failed to parse owner key %s: %v", owner, err)
			continue
		}

		netInfo, err := c.findNetworkByID(networkID)
		if err != nil {
			klog.V(4).Infof("Network with ID %d not found, skipping policies/routes: %v", networkID, err)
			continue
		}

		// Skip if this network's router doesn't exist yet (new network not yet in cache)
		if networksToCreate.Has(owner) {
			networkRouterName := netInfo.GetNetworkScopedClusterRouterName()
			_, err = libovsdbops.GetLogicalRouter(c.nbClient, &nbdb.LogicalRouter{Name: networkRouterName})
			if err != nil {
				// Already logged and error tracked in port creation phase
				continue
			}
		}

		klog.V(5).Infof("CNC %s: ensuring routing policies and static routes for network %s", cncName, netInfo.GetNetworkName())

		// Ensure routing policies on the network router
		ops, err = c.ensureRoutingPoliciesOps(ops, cnc, netInfo, allocatedSubnets)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to ensure routing policies for network %s: %w", netInfo.GetNetworkName(), err))
			continue
		}

		// Ensure static routes on the connect router
		ops, err = c.ensureStaticRoutesOps(ops, cnc, netInfo, subnets, allNodes)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to ensure static routes for network %s: %w", netInfo.GetNetworkName(), err))
			continue
		}
	}

	connectRouterName := getConnectRouterName(cncName)

	// Cleanup ports and routes for nodes that no longer exist.
	// Build set of current node IDs for comparison.
	currentNodeIDs := sets.New[string]()
	for _, node := range allNodes {
		nodeID, err := util.GetNodeID(node)
		if err != nil {
			continue
		}
		currentNodeIDs.Insert(strconv.Itoa(nodeID))
	}

	// Delete connect router ports for nodes that no longer exist
	ops, err = libovsdbops.DeleteLogicalRouterPortWithPredicateOps(c.nbClient, ops, connectRouterName, func(item *nbdb.LogicalRouterPort) bool {
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
	ops, err = libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicateOps(c.nbClient, ops, connectRouterName, func(item *nbdb.LogicalRouterStaticRoute) bool {
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

		// Get the network router name from cache (stored when the network was connected)
		// This allows cleanup even if the network has been deleted from the network manager
		networkRouterName, found := cncState.connectedNetworksRouterNames[owner]
		if !found {
			klog.Warningf("Network router name not found in cache for owner %s, cleaning up cache entry", owner)
			// Still track for deletion since we need to clean cache even if router name unknown
			successfullyDeleted = append(successfullyDeleted, owner)
			continue
		}

		// Delete connect router ports for this network
		ops, err = libovsdbops.DeleteLogicalRouterPortWithPredicateOps(c.nbClient, ops, connectRouterName, func(item *nbdb.LogicalRouterPort) bool {
			return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName &&
				item.ExternalIDs[libovsdbops.NetworkIDKey.String()] == strconv.Itoa(networkID)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to delete connect ports for network %s: %w", owner, err))
			continue
		}

		// Delete network router ports for this network
		ops, err = libovsdbops.DeleteLogicalRouterPortWithPredicateOps(c.nbClient, ops, networkRouterName, func(item *nbdb.LogicalRouterPort) bool {
			return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName &&
				item.ExternalIDs[libovsdbops.NetworkIDKey.String()] == strconv.Itoa(networkID)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to delete network router ports for network %s: %w", owner, err))
			continue
		}

		// Delete routing policies from the disconnected network's router
		// These are policies that route traffic FROM this network TO other connected networks
		ops, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(c.nbClient, ops, networkRouterName, func(item *nbdb.LogicalRouterPolicy) bool {
			return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to delete routing policies from network router %s: %w", networkRouterName, err))
			continue
		}

		// Delete routing policies from other connected networks' routers that reference this deleted network
		for otherOwner, otherNetworkRouterName := range cncState.connectedNetworksRouterNames {
			// Skip the network being deleted (we already cleaned its router above)
			if otherOwner == owner {
				continue
			}

			// Delete policies on other network's router that reference the deleted network
			ops, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(c.nbClient, ops, otherNetworkRouterName, func(item *nbdb.LogicalRouterPolicy) bool {
				return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName &&
					item.ExternalIDs[libovsdbops.NetworkIDKey.String()] == strconv.Itoa(networkID)
			})
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to delete routing policies on %s for network %s: %w", otherNetworkRouterName, owner, err))
				// Don't continue here - we still want to try other cleanups
			}
		}

		// Delete static routes from the connect router for this network
		ops, err = libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicateOps(c.nbClient, ops, connectRouterName, func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName &&
				item.ExternalIDs[libovsdbops.NetworkIDKey.String()] == strconv.Itoa(networkID)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to delete static routes for network %s: %w", owner, err))
			continue
		}

		// Track this network for cache cleanup after successful transact
		successfullyDeleted = append(successfullyDeleted, owner)
	}

	// Execute all operations (create and delete) in a single transaction
	if len(ops) > 0 {
		if _, err := libovsdbops.TransactAndCheck(c.nbClient, ops); err != nil {
			errs = append(errs, fmt.Errorf("failed to execute network connection operations for CNC %s: %w", cncName, err))
			return utilerrors.Join(errs...)
		}
		klog.Infof("CNC %s: executed %d sync network connections operations", cncName, len(ops))
	}

	// Update cache only after successful transact - this ensures cache stays consistent with OVN NB
	for _, added := range successfullyAdded {
		cncState.connectedNetworks.Insert(added.owner)
		cncState.connectedNetworksRouterNames[added.owner] = added.networkRouterName
	}
	for _, deleted := range successfullyDeleted {
		cncState.connectedNetworks.Delete(deleted)
		delete(cncState.connectedNetworksRouterNames, deleted)
	}

	return utilerrors.Join(errs...)
}

// cleanupNetworkConnections removes all network connections for a CNC.
// This is called when a CNC is being deleted. Since the connect router will be deleted
// along with the CNC, we only need to clean up resources on the network routers.
func (c *Controller) cleanupNetworkConnections(cncName string, cncState *networkConnectState) error {
	var ops []ovsdb.Operation

	// For each connected network, clean up ports and policies on the network router
	for owner, networkRouterName := range cncState.connectedNetworksRouterNames {
		_, networkID, parseErr := parseOwnerKey(owner)
		if parseErr != nil {
			klog.Warningf("Failed to parse owner key %s during CNC cleanup: %v", owner, parseErr)
			continue
		}

		// Delete network router ports created by this CNC
		var err error
		ops, err = libovsdbops.DeleteLogicalRouterPortWithPredicateOps(c.nbClient, ops, networkRouterName, func(item *nbdb.LogicalRouterPort) bool {
			return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName &&
				item.ExternalIDs[libovsdbops.NetworkIDKey.String()] == strconv.Itoa(networkID)
		})
		if err != nil {
			return fmt.Errorf("failed to delete network router ports for network %s: %w", owner, err)
		}

		// Delete routing policies created by this CNC on the network router
		ops, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(c.nbClient, ops, networkRouterName, func(item *nbdb.LogicalRouterPolicy) bool {
			return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName
		})
		if err != nil {
			return fmt.Errorf("failed to delete routing policies from network router %s: %w", networkRouterName, err)
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

	// Calculate maxNodes from the CNC's connectSubnets networkPrefix for tunnel key allocation
	// tunnelKey = networkIndex * maxNodes + nodeID + 1 (for Layer3)
	// tunnelKey = networkIndex * maxNodes + subIndex + 1 (for Layer2)
	if len(subnets) == 0 {
		return nil, fmt.Errorf("no subnets allocated for network %s", networkName)
	}
	networkIndex, maxNodes := getNetworkIndexAndMaxNodes(cnc.Spec.ConnectSubnets, subnets)

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

			// Calculate tunnel key: networkIndex * maxNodes + nodeID + 1
			tunnelKey := networkIndex*maxNodes + nodeID + 1

			connectPortName := getConnectRouterToNetworkRouterPortName(cncName, networkName, node.Name)
			networkPortName := getNetworkRouterToConnectRouterPortName(networkName, node.Name, cncName)

			isLocalNode := node.Name == c.zone

			if isLocalNode {
				// Local node: create both ports with peer relationship
				ops, err = c.createRouterPortOps(ops, connectRouterName, connectPortName, connectPortIPs, networkPortName, cncName,
					networkID, nodeID, tunnelKey)
				if err != nil {
					return nil, fmt.Errorf("failed to create connect router port ops %s: %v", connectPortName, err)
				}
				ops, err = c.createRouterPortOps(ops, networkRouterName, networkPortName, networkPortIPs, connectPortName, cncName,
					networkID, nodeID, 0)
				if err != nil {
					return nil, fmt.Errorf("failed to create network router port ops %s: %v", networkPortName, err)
				}
			} else {
				// Remote node: create only the connect-router side port (no peer)
				ops, err = c.createRouterPortOps(ops, connectRouterName, connectPortName, connectPortIPs, "", cncName, networkID, nodeID, tunnelKey)
				if err != nil {
					return nil, fmt.Errorf("failed to create remote connect router port ops %s: %v", connectPortName, err)
				}
			}
		}
	}
	if netInfo.TopologyType() == ovntypes.Layer2Topology {
		// For Layer2 networks, create a single port pair to the transit router
		connectPortIPs, networkPortIPs := getP2PIPs(subnets)

		// For Layer2, calculate tunnel key based on the subnet index within the /networkPrefix block
		subIndex := getLayer2SubIndex(subnets)
		tunnelKey := networkIndex*maxNodes + subIndex + 1

		connectPortName := getConnectRouterToNetworkRouterPortName(cncName, networkName, "")
		networkPortName := getNetworkRouterToConnectRouterPortName(networkName, "", cncName)

		// Create the port on the connect router (with peer set)
		ops, err = c.createRouterPortOps(ops, connectRouterName, connectPortName, connectPortIPs, networkPortName, cncName,
			networkID, 0, tunnelKey)
		if err != nil {
			return nil, fmt.Errorf("failed to create connect router port ops %s: %v", connectPortName, err)
		}

		// Create the peer port on the transit router (with peer set)
		ops, err = c.createRouterPortOps(ops, networkRouterName, networkPortName, networkPortIPs, connectPortName, cncName,
			networkID, 0, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to create network router port ops %s: %v", networkPortName, err)
		}
	}

	return ops, nil
}

// createRouterPortOps returns ops to create a logical router port with peer and tunnel key set.
func (c *Controller) createRouterPortOps(ops []ovsdb.Operation, routerName, portName string, ipNets []*net.IPNet, peerPortName string,
	cncName string, networkID, nodeID, tunnelKey int) ([]ovsdb.Operation, error) {
	if len(ipNets) == 0 {
		return nil, fmt.Errorf("no IPNets provided for router port %s", portName)
	}

	dbIndexes := libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPortClusterNetworkConnect, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.NodeIDKey:     strconv.Itoa(nodeID),
			libovsdbops.NetworkIDKey:  strconv.Itoa(networkID),
			libovsdbops.ObjectNameKey: cncName,
		})

	port := &nbdb.LogicalRouterPort{
		Name:        portName,
		MAC:         util.IPAddrToHWAddr(ipNets[0].IP).String(),
		Networks:    util.IPNetsToStringSlice(ipNets),
		Peer:        &peerPortName,
		ExternalIDs: dbIndexes.GetExternalIDs(),
	}
	if peerPortName != "" {
		port.Peer = &peerPortName
	}
	if tunnelKey != 0 {
		port.Options = map[string]string{
			"requested-tnl-key": strconv.Itoa(tunnelKey),
		}
	}

	router := &nbdb.LogicalRouter{Name: routerName}
	var err error
	ops, err = libovsdbops.CreateOrUpdateLogicalRouterPortOps(c.nbClient, ops, router, port, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create port ops %s on router %s: %v", portName, routerName, err)
	}

	klog.V(5).Infof("Created router port ops %s on %s with peer %s and tunnel key %d", portName, routerName, peerPortName, tunnelKey)
	return ops, nil
}

// ensureRoutingPoliciesOps returns ops to create routing policies on the network router to steer traffic to connected networks.
// For Layer3: creates policy for the local node only (each zone handles its own node)
// For Layer2: creates a single policy (transit router is distributed)
func (c *Controller) ensureRoutingPoliciesOps(ops []ovsdb.Operation, cnc *networkconnectv1.ClusterNetworkConnect, srcNetwork util.NetInfo,
	allocatedSubnets map[string][]*net.IPNet) ([]ovsdb.Operation, error) {
	cncName := cnc.Name
	networkRouterName := srcNetwork.GetNetworkScopedClusterRouterName()
	zoneName := c.zone // zone name is the node name

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
		localNode, err := c.nodeLister.Get(zoneName)
		if err != nil {
			return nil, fmt.Errorf("failed to get local node %s: %v", zoneName, err)
		}
		nodeID, err := util.GetNodeID(localNode)
		if err != nil {
			return nil, fmt.Errorf("local node %s does not have node ID: %v", zoneName, err)
		}

		inportName = srcNetwork.GetNetworkScopedSwitchToRouterPortName(zoneName)

		p2pSubnets, err := calculateP2PSubnets(srcConnectSubnets, nodeID)
		if err != nil {
			return nil, fmt.Errorf("failed to calculate P2P subnets for node %s: %v", zoneName, err)
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
		ops, err = c.createRoutingPoliciesOps(ops, dstNetworkID, networkRouterName, inportName, dstPodSubnets, srcNetwork.GetNetworkName(), nexthops, cncName)
		if err != nil {
			return nil, err
		}
	}
	klog.V(5).Infof("Created/updated routing policies ops on %s: %s -> %s", networkRouterName, inportName, nexthops)

	return ops, nil
}

// createRoutingPoliciesOps returns ops to create logical router policies.
func (c *Controller) createRoutingPoliciesOps(ops []ovsdb.Operation, dstNetworkID int, routerName, inportName string, dstSubnets []config.CIDRNetworkEntry,
	srcNetworkName string, nexthops []net.IP, cncName string) ([]ovsdb.Operation, error) {
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
		ipVersion := "ip4"
		if utilnet.IsIPv6(dstSubnet.CIDR.IP) {
			ipVersion = "ip6"
		}
		match := fmt.Sprintf(`inport == "%s" && %s.dst == %s`, inportName, ipVersion, dstSubnet.CIDR.String())

		dbIndexes := libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyClusterNetworkConnect, controllerName,
			map[libovsdbops.ExternalIDKey]string{
				libovsdbops.NetworkIDKey:  strconv.Itoa(dstNetworkID),
				libovsdbops.NetworkKey:    srcNetworkName,
				libovsdbops.ObjectNameKey: cncName,
			})
		policy := &nbdb.LogicalRouterPolicy{
			Priority:    policyPriority,
			Match:       match,
			Action:      nbdb.LogicalRouterPolicyActionReroute,
			Nexthops:    []string{nexthop},
			ExternalIDs: dbIndexes.GetExternalIDs(),
		}

		var err error
		ops, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(c.nbClient, ops, routerName, policy,
			func(item *nbdb.LogicalRouterPolicy) bool {
				return item.Priority == policy.Priority && item.Match == policy.Match
			})
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
			ops, err = c.createStaticRoutesOps(ops, networkName, connectRouterName, nodeSubnets, nexthops, cncName, nodeID)
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

		ops, err = c.createStaticRoutesOps(ops, networkName, connectRouterName, podSubnetIPNets, nexthops, cncName, 0)
		if err != nil {
			return nil, err
		}
	}

	return ops, nil
}

// createStaticRoutesOps returns ops to create logical router static routes.
func (c *Controller) createStaticRoutesOps(ops []ovsdb.Operation, networkName, routerName string, dstSubnets []*net.IPNet, nexthops []net.IP, cncName string, nodeID int) ([]ovsdb.Operation, error) {
	for _, dstSubnet := range dstSubnets {
		// Find matching nexthop (same IP family)
		var nexthop string
		for _, nh := range nexthops {
			isIPv4Subnet := utilnet.IsIPv4(dstSubnet.IP)
			isIPv4Nexthop := utilnet.IsIPv4(nh)
			if isIPv4Subnet == isIPv4Nexthop {
				nexthop = nh.String()
				break
			}
		}
		if nexthop == "" {
			continue
		}

		dbIndexes := libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterStaticRouteClusterNetworkConnect, controllerName,
			map[libovsdbops.ExternalIDKey]string{
				libovsdbops.NetworkKey:    networkName,
				libovsdbops.NodeIDKey:     strconv.Itoa(nodeID),
				libovsdbops.ObjectNameKey: cncName, // CNC name
			})
		route := &nbdb.LogicalRouterStaticRoute{
			IPPrefix:    dstSubnet.String(),
			Nexthop:     nexthop,
			ExternalIDs: dbIndexes.GetExternalIDs(),
		}

		var err error
		ops, err = libovsdbops.CreateOrUpdateLogicalRouterStaticRoutesWithPredicateOps(c.nbClient, ops, routerName, route,
			func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.IPPrefix == route.IPPrefix && item.Nexthop == route.Nexthop
			},
			&route.ExternalIDs)
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
