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

	"github.com/ovn-kubernetes/libovsdb/ovsdb"

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

// syncNetworkConnections syncs all network connections for a CNC.
// STEP2: Create the patch ports connecting network router's to the connect router
// using IPs from the network subnet CNC annotation.
func (c *Controller) syncNetworkConnections(cnc *networkconnectv1.ClusterNetworkConnect, allocatedSubnets map[string][]*net.IPNet) error {
	cncName := cnc.Name
	cncState, exists := c.cncCache[cncName]
	if !exists || cncState == nil {
		return fmt.Errorf("CNC %s not found in cache", cncName)
	}

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

	var createOps, deleteOps []ovsdb.Operation
	var errs []error

	// Track networks that had ops built successfully - cache will be updated only after transact succeeds
	// This ensures cache stays consistent with OVN NB even if transact fails
	var successfullyAdded []string
	var successfullyDeleted []string

	// Ensure ports for ALL desired networks.
	// All operations are idempotent (CreateOrUpdate), so we reconcile them on every sync.
	// This handles:
	// - New networks: creates ports
	// - New nodes added to existing networks: creates ports
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
		netInfo := c.networkManager.GetNetworkByID(networkID)
		if netInfo == nil {
			klog.V(4).Infof("Network with ID %d not found, skipping", networkID)
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

		klog.V(5).Infof("CNC %s: ensuring ports for network %s (new=%v)", cncName, netInfo.GetNetworkName(), isNewNetwork)

		// Create/update ports connecting the connect router and network router
		// Local node: full port pair with peer; Remote nodes: connect-router port only
		// This is idempotent - existing ports are unchanged, new node ports are created
		createOps, err = c.ensureConnectPortsOps(createOps, cnc, netInfo, subnets, allNodes)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to ensure connect ports for network %s: %w", netInfo.GetNetworkName(), err))
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

	// Cleanup ports for nodes that no longer exist.
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
			portPairInfo, err := GetP2PAddresses(subnets, nodeID)
			if err != nil {
				return nil, fmt.Errorf("failed to calculate P2P IP addresses for node %s: %v", node.Name, err)
			}

			// Calculate tunnel key using the unified function
			tunnelKey, err := GetTunnelKey(cnc.Spec.ConnectSubnets, subnets, ovntypes.Layer3Topology, nodeID)
			if err != nil {
				return nil, fmt.Errorf("failed to calculate tunnel key for node %s: %v", node.Name, err)
			}

			connectPortName := getConnectRouterToNetworkRouterPortName(cncName, networkName, node.Name)
			networkPortName := getNetworkRouterToConnectRouterPortName(networkName, node.Name, cncName)

			isLocalNode := util.GetNodeZone(node) == c.zone

			if isLocalNode {
				// Local node: create both ports with peer relationship
				ops, err = c.createRouterPortOps(ops, connectRouterName, connectPortName, portPairInfo.connectPortIPs,
					networkPortName, cncName, networkID, nodeID, tunnelKey, "")
				if err != nil {
					return nil, fmt.Errorf("failed to create connect router port ops %s: %v", connectPortName, err)
				}
				ops, err = c.createRouterPortOps(ops, networkRouterName, networkPortName, portPairInfo.networkPortIPs,
					connectPortName, cncName, networkID, nodeID, 0, "")
				if err != nil {
					return nil, fmt.Errorf("failed to create network router port ops %s: %v", networkPortName, err)
				}
			} else {
				// Remote node: create only the connect-router side port with requested-chassis set
				// This makes the port type: remote in SB, enabling cross-zone tunneling
				ops, err = c.createRouterPortOps(ops, connectRouterName, connectPortName, portPairInfo.connectPortIPs,
					"", cncName, networkID, nodeID, tunnelKey, node.Name)
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
		portPairInfo, err := GetP2PAddresses(subnets, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to calculate P2P IP addresses for Layer2 network %s: %v", networkName, err)
		}

		// Calculate tunnel key using the unified function (nodeID=0 for Layer2)
		tunnelKey, err := GetTunnelKey(cnc.Spec.ConnectSubnets, subnets, ovntypes.Layer2Topology, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to calculate tunnel key for Layer2 network %s: %v", networkName, err)
		}

		connectPortName := getConnectRouterToNetworkRouterPortName(cncName, networkName, "")
		networkPortName := getNetworkRouterToConnectRouterPortName(networkName, "", cncName)

		// Create the port on the connect router (with peer set)
		ops, err = c.createRouterPortOps(ops, connectRouterName, connectPortName, portPairInfo.connectPortIPs,
			networkPortName, cncName, networkID, 0, tunnelKey, "")
		if err != nil {
			return nil, fmt.Errorf("failed to create connect router port ops %s: %v", connectPortName, err)
		}

		// Create the peer port on the transit router (with peer set)
		ops, err = c.createRouterPortOps(ops, networkRouterName, networkPortName, portPairInfo.networkPortIPs,
			connectPortName, cncName, networkID, 0, 0, "")
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
