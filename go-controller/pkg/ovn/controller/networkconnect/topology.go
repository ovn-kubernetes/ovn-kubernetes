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

// computeNodeInfo lists all nodes and returns them along with a set of current node IDs.
// It also updates c.localZoneNode for use by other functions.
func (c *Controller) computeNodeInfo() ([]*corev1.Node, sets.Set[string], error) {
	allNodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list nodes: %v", err)
	}

	currentNodeIDs := sets.New[string]()
	var localNode *corev1.Node
	for _, node := range allNodes {
		if util.GetNodeZone(node) == c.zone {
			// we don't support multiple local nodes per zone for this feature
			localNode = node
		}
		nodeID, err := util.GetNodeID(node)
		if err != nil {
			klog.Warningf("Failed to get node ID for node %s, skipping: %v", node.Name, err)
			continue
		}
		currentNodeIDs.Insert(strconv.Itoa(nodeID))
	}

	c.localZoneNode = localNode
	return allNodes, currentNodeIDs, nil
}

// syncNetworkConnections syncs all network connections for a CNC.
// STEP2: Create the patch ports connecting network router's to the connect router
// using IPs from the network subnet CNC annotation.
// STEP3: If PodNetworkConnect is enabled, create the logical router policies on network router's
// to steer traffic to the connect router for other connected networks.
// STEP4: If PodNetworkConnect is enabled, add static routes to connect router towards
// each of the connected networks.
// STEP5: If ClusterIPServiceNetwork connectivity if enabled, add load balancers of connected networks
// to all other connected networks' switches.
func (c *Controller) syncNetworkConnections(cnc *networkconnectv1.ClusterNetworkConnect, allocatedSubnets map[string][]*net.IPNet) error {
	cncName := cnc.Name
	cncState, exists := c.cncCache[cncName]
	if !exists || cncState == nil {
		return fmt.Errorf("CNC %s not found in cache", cncName)
	}

	// Get all nodes - the connect-router needs static routes to ALL node subnets
	allNodes, currentNodeIDs, err := c.computeNodeInfo()
	if err != nil {
		return fmt.Errorf("failed to compute node info for CNC %s: %w", cncName, err)
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

	serviceConnectivityDesired := serviceConnectivityEnabled(cnc)

	var errs []error

	// Ensure ports, routing policies and static routes for ALL desired networks.
	// All operations are idempotent (CreateOrUpdate), so we reconcile them on every sync.
	// This handles:
	// - New networks: creates ports, policies, and static routes
	// - New nodes added to existing networks: creates ports and static routes
	// - Existing networks needing policies to newly added networks
	// - Node annotation changes (new subnets becoming available)
	// Each network is transacted separately to keep transaction sizes bounded.
	for owner, subnets := range allocatedSubnets {
		isNewNetwork := networksToCreate.Has(owner)
		_, networkID, err := util.ParseNetworkOwner(owner)
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
		localActive := c.localZoneNode != nil && c.networkManager.NodeHasNetwork(c.localZoneNode.Name, netInfo.GetNetworkName())

		// Check if the network router exists before trying to create ports on it.
		// The network might be registered in the network manager but not yet created in OVN NB.
		// If the router doesn't exist, skip this network and retry later.
		if localActive {
			networkRouterName := netInfo.GetNetworkScopedClusterRouterName()
			_, err = libovsdbops.GetLogicalRouter(c.nbClient, &nbdb.LogicalRouter{Name: networkRouterName})
			if err != nil {
				klog.V(4).Infof("Network router %s for network %s does not exist yet, will retry: %v", networkRouterName, netInfo.GetNetworkName(), err)
				errs = append(errs, fmt.Errorf("network router %s for network %s does not exist yet: %w", networkRouterName, netInfo.GetNetworkName(), err))
				continue
			}
		}

		klog.V(5).Infof("CNC %s: ensuring ports, policies and routes for network %s (new=%v)", cncName, netInfo.GetNetworkName(), isNewNetwork)

		// Build ops per network to keep transaction sizes bounded
		var createOps []ovsdb.Operation

		// Create/update ports connecting the connect router and network router
		// Local node: full port pair with peer; Remote nodes: connect-router port only
		// This is idempotent - existing ports are unchanged, new node ports are created
		createOps, err = c.ensureConnectPortsOps(createOps, cnc, netInfo, subnets, allNodes, localActive)
		if err != nil {
			errs = append(errs, fmt.Errorf("CNC %s: failed to ensure connect ports for network %s: %w", cncName, netInfo.GetNetworkName(), err))
			continue
		}

		// Ensure routing policies on the network router (local-active networks only)
		if localActive {
			createOps, err = c.ensureRoutingPoliciesOps(createOps, cncName, netInfo, allocatedSubnets)
			if err != nil {
				errs = append(errs, fmt.Errorf("CNC %s: failed to ensure routing policies for network %s: %w", cncName, netInfo.GetNetworkName(), err))
				continue
			}
		}

		// Ensure static routes on the connect router. For Layer2, skip when the local network is inactive.
		if netInfo.TopologyType() != ovntypes.Layer2Topology || localActive {
			createOps, err = c.ensureStaticRoutesOps(createOps, cnc, netInfo, subnets, allNodes)
			if err != nil {
				errs = append(errs, fmt.Errorf("CNC %s: failed to ensure static routes for network %s: %w", cncName, netInfo.GetNetworkName(), err))
				continue
			}
		}

		// If ClusterIPServiceNetwork is enabled, add LB attachment ops to the same transaction
		// This attaches this network's LBs to all other networks' switches in allocatedSubnets.
		// Over the full loop, all pairs get covered (like routes and policies).
		if serviceConnectivityDesired {
			createOps, err = c.ensureLoadBalancersOps(createOps, netInfo, allocatedSubnets)
			if err != nil {
				errs = append(errs, fmt.Errorf("CNC %s: failed to ensure load balancers for network %s: %w", cncName, netInfo.GetNetworkName(), err))
				continue
			}
		}

		// Transact all ops (ports, policies, routes, LBs) in a single transaction per network
		if len(createOps) > 0 {
			if _, err := libovsdbops.TransactAndCheck(c.nbClient, createOps); err != nil {
				errs = append(errs, fmt.Errorf("CNC %s: failed to execute create operations for network %s: %w", cncName, netInfo.GetNetworkName(), err))
				continue
			}
			klog.Infof("CNC %s: executed %d create operations for network %s", cncName, len(createOps), netInfo.GetNetworkName())
		}

		// Update cache only after successful transaction (both network and service connectivity)
		if isNewNetwork {
			cncState.connectedNetworks.Insert(owner)
		}
	}

	connectRouterName := getConnectRouterName(cncName)

	// Cleanup ports and routes for nodes that no longer exist. - transact separately
	var nodeDeleteOps []ovsdb.Operation
	nodeDeleteOps, err = libovsdbops.DeleteLogicalRouterPortWithPredicateOps(c.nbClient, nodeDeleteOps, connectRouterName,
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
		errs = append(errs, fmt.Errorf("CNC %s: failed to cleanup ports for deleted nodes: %w", cncName, err))
	}

	// Delete static routes for nodes that no longer exist
	nodeDeleteOps, err = libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicateOps(c.nbClient, nodeDeleteOps, connectRouterName,
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
		errs = append(errs, fmt.Errorf("CNC %s: failed to cleanup routes for deleted nodes: %w", cncName, err))
	}

	if len(nodeDeleteOps) > 0 {
		if _, err := libovsdbops.TransactAndCheck(c.nbClient, nodeDeleteOps); err != nil {
			errs = append(errs, fmt.Errorf("CNC %s: failed to execute node cleanup operations: %w", cncName, err))
		} else {
			klog.Infof("CNC %s: executed %d node cleanup operations", cncName, len(nodeDeleteOps))
		}
	}

	// Cleanup networks that are no longer connected - transact per network
	for owner := range networksToDelete {
		klog.V(5).Infof("CNC %s: cleaning up network owner=%s", cncName, owner)
		_, networkID, err := util.ParseNetworkOwner(owner)
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
			errs = append(errs, fmt.Errorf("CNC %s: failed to find ports for network %s: %w", cncName, owner, err))
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

		// Build delete ops for this network
		var deleteOps []ovsdb.Operation
		for routerName := range routerNames {
			deleteOps, err = libovsdbops.DeleteLogicalRouterPortWithPredicateOps(c.nbClient, deleteOps, routerName,
				func(item *nbdb.LogicalRouterPort) bool {
					return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName &&
						item.ExternalIDs[libovsdbops.NetworkIDKey.String()] == strconv.Itoa(networkID) &&
						item.ExternalIDs[libovsdbops.RouterNameKey.String()] == routerName
				})
			if err != nil {
				errs = append(errs, fmt.Errorf("CNC %s: failed to delete network router ports for network %s: %w", cncName, owner, err))
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
			// - Are FROM this network (SourceNetworkIDKey == networkID), OR
			// - Reference this network as destination (DestinationNetworkIDKey == networkID)
			policySourceNetworkID := item.ExternalIDs[libovsdbops.SourceNetworkIDKey.String()]
			policyDestinationNetworkID := item.ExternalIDs[libovsdbops.DestinationNetworkIDKey.String()]
			return policySourceNetworkID == strconv.Itoa(networkID) || policyDestinationNetworkID == strconv.Itoa(networkID)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("CNC %s: failed to find routing policies for network %s: %w", cncName, owner, err))
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
		for routerName, routerPolicies := range policiesByRouter {
			deleteOps, err = libovsdbops.DeleteLogicalRouterPoliciesOps(c.nbClient, deleteOps, routerName, routerPolicies...)
			if err != nil {
				errs = append(errs, fmt.Errorf("CNC %s: failed to delete routing policies from router %s for network %s: %w", cncName, routerName, owner, err))
				// Don't continue here - we still want to try other cleanups
			}
		}

		// Delete static routes from the connect router for this network
		// Note: Static routes don't have RouterNameKey in ExternalIDs, but we're deleting from connectRouterName
		// so matching by ObjectNameKey and NetworkIDKey is sufficient
		deleteOps, err = libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicateOps(c.nbClient, deleteOps, connectRouterName,
			func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName &&
					item.ExternalIDs[libovsdbops.NetworkIDKey.String()] == strconv.Itoa(networkID)
			})
		if err != nil {
			errs = append(errs, fmt.Errorf("CNC %s: failed to delete static routes for network %s: %w", cncName, owner, err))
			continue
		}

		// If ClusterIPServiceNetwork is enabled, cleanup LB attachments for this network.
		// Unlike the add case where bidirectionality happens naturally via the outer loop
		// (each network attaches its LBs to others, covering all pairs), in delete the
		// deleted network won't be iterated again. So we must explicitly cleanup both
		// directions: remove this network's LBs from remaining switches AND remove
		// remaining networks' LBs from this network's switch.
		if serviceConnectivityDesired {
			remainingNetworks := cncState.connectedNetworks.Clone()
			remainingNetworks.Delete(owner)
			deleteOps, err = c.cleanupLoadBalancersOps(deleteOps, networkID, remainingNetworks)
			if err != nil {
				errs = append(errs, fmt.Errorf("CNC %s: failed to cleanup service connectivity for network %s: %w", cncName, owner, err))
			}
		}

		// Transact per network to keep transaction sizes bounded
		if len(deleteOps) > 0 {
			if _, err := libovsdbops.TransactAndCheck(c.nbClient, deleteOps); err != nil {
				errs = append(errs, fmt.Errorf("CNC %s: failed to execute delete operations for network %s: %w", cncName, owner, err))
				continue
			}
			klog.Infof("CNC %s: executed %d delete operations for network %s", cncName, len(deleteOps), owner)
		}

		// Update cache after successful transact for this network
		cncState.connectedNetworks.Delete(owner)
	}

	// If ClusterIPServiceNetwork was enabled but now disabled, cleanup all cross-network LB attachments
	if !serviceConnectivityDesired && cncState.clusterIPServiceNetworkEnabled {
		klog.V(4).Infof("CNC %s: ClusterIPServiceNetwork disabled, cleaning up cross-network LB attachments", cncName)
		if err := c.cleanupServiceConnectivity(cncName); err != nil {
			errs = append(errs, fmt.Errorf("CNC %s: failed to cleanup service connectivity: %w", cncName, err))
		}
	}

	cncState.clusterIPServiceNetworkEnabled = serviceConnectivityDesired

	return utilerrors.Join(errs...)
}

// cleanupNetworkConnections removes all network connections for a CNC.
// This is called when a CNC is being deleted.
// 1. If ClusterIPServiceNetwork was enabled, cleanup cross-network LB attachments for this CNC
// 2. Then delete network router ports from the network routers for this CNC
// 3. Then delete routing policies on the network routers for this CNC
func (c *Controller) cleanupNetworkConnections(cncName string, serviceConnectivityWasEnabled bool) error {
	// Cleanup cross-network LB attachments if ClusterIPServiceNetwork was enabled
	if serviceConnectivityWasEnabled {
		if err := c.cleanupServiceConnectivity(cncName); err != nil {
			return fmt.Errorf("failed to cleanup service connectivity for CNC %s: %v", cncName, err)
		}
	}

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
	for routerName, routerPolicies := range policiesByRouter {
		ops, err = libovsdbops.DeleteLogicalRouterPoliciesOps(c.nbClient, ops, routerName, routerPolicies...)
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
	subnets []*net.IPNet, nodes []*corev1.Node, localActive bool) ([]ovsdb.Operation, error) {
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
				if !localActive {
					ops, err = libovsdbops.DeleteLogicalRouterPortWithPredicateOps(c.nbClient, ops, connectRouterName,
						func(item *nbdb.LogicalRouterPort) bool {
							return item.Name == connectPortName
						})
					if err != nil {
						return nil, fmt.Errorf("failed to delete connect router port ops %s: %v", connectPortName, err)
					}
					ops, err = libovsdbops.DeleteLogicalRouterPortWithPredicateOps(c.nbClient, ops, networkRouterName,
						func(item *nbdb.LogicalRouterPort) bool {
							return item.Name == networkPortName
						})
					if err != nil {
						return nil, fmt.Errorf("failed to delete network router port ops %s: %v", networkPortName, err)
					}
					continue
				}
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
				chassisID, err := util.ParseNodeChassisIDAnnotation(node)
				if err != nil {
					if util.IsAnnotationNotSetError(err) {
						return nil, ovntypes.NewSuppressedError(err)
					}
					return nil, fmt.Errorf("failed to parse node chassis-id for node %s: %w", node.Name, err)
				}
				ops, err = c.createRouterPortOps(ops, connectRouterName, connectPortName, portPairInfo.connectPortIPs,
					"", cncName, networkID, nodeID, tunnelKey, chassisID)
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

		if !localActive {
			ops, err = libovsdbops.DeleteLogicalRouterPortWithPredicateOps(c.nbClient, ops, connectRouterName,
				func(item *nbdb.LogicalRouterPort) bool {
					return item.Name == connectPortName
				})
			if err != nil {
				return nil, fmt.Errorf("failed to delete connect router port ops %s: %v", connectPortName, err)
			}
			ops, err = libovsdbops.DeleteLogicalRouterPortWithPredicateOps(c.nbClient, ops, networkRouterName,
				func(item *nbdb.LogicalRouterPort) bool {
					return item.Name == networkPortName
				})
			if err != nil {
				return nil, fmt.Errorf("failed to delete network router port ops %s: %v", networkPortName, err)
			}
			return ops, nil
		}

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
	srcOwnerKey := util.ComputeNetworkOwner(srcNetwork.TopologyType(), srcNetwork.GetNetworkID())
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

		inportName = srcNetwork.GetNetworkScopedRouterToSwitchPortName(c.localZoneNode.Name)

		portPairInfo, err := GetP2PAddresses(srcConnectSubnets, nodeID)
		if err != nil {
			return nil, fmt.Errorf("failed to calculate P2P IP addresses for node %s: %v", c.localZoneNode.Name, err)
		}
		nexthops = util.IPNetsToIPs(portPairInfo.connectPortIPs)
	} else if srcNetwork.TopologyType() == ovntypes.Layer2Topology {
		// For Layer2, create a single policy (nodeName ignored for Layer2 switch)
		inportName = srcNetwork.GetNetworkScopedRouterToSwitchPortName("")
		// For Layer2, srcConnectSubnets is already a /31 (IPv4) or /127 (IPv6) subnet.
		// Using nodeID=0 extracts the first and second IPs from this existing P2P subnet.
		// The first IP is the connect router port IP.
		portPairInfo, err := GetP2PAddresses(srcConnectSubnets, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to calculate P2P IP addresses for Layer2 network %s: %v", srcNetwork.GetNetworkName(), err)
		}
		nexthops = util.IPNetsToIPs(portPairInfo.connectPortIPs)
	}

	// For each other connected network, add a routing policy.
	// Note: We iterate allocatedSubnets again here (it's also iterated by the caller) because
	// this creates the full mesh of policies. The outer loop in syncNetworkConnections selects
	// the SOURCE network (where policies are created), while this inner loop finds all
	// DESTINATION networks (what the policies route to). This is O(N²) which is intentional
	// for a full mesh connectivity between N networks.
	// This is typically fine since number of networks that are expected to be connected by a CNC is small, eg. 10.
	for owner := range allocatedSubnets {
		_, dstNetworkID, err := util.ParseNetworkOwner(owner)
		if err != nil {
			continue
		}

		// Skip if this is the same network
		if dstNetworkID == srcNetwork.GetNetworkID() {
			continue
		}

		// Find destination network info
		dstNetwork := c.networkManager.GetNetworkByID(dstNetworkID)
		if dstNetwork == nil {
			klog.V(4).Infof("Destination network %d not found, skipping policy", dstNetworkID)
			continue
		}

		// Get destination network's pod subnets
		dstPodSubnets := dstNetwork.Subnets()

		// Create policies for each destination subnet
		ops, err = c.createRoutingPoliciesOps(ops, dstNetworkID, networkRouterName, inportName, dstPodSubnets,
			srcNetwork.GetNetworkID(), nexthops, cncName)
		if err != nil {
			return nil, err
		}
	}
	klog.V(5).Infof("Created/updated routing policies ops on %s: %s -> %s", networkRouterName, inportName, nexthops)

	return ops, nil
}

// createRoutingPoliciesOps returns ops to create logical router policies.
func (c *Controller) createRoutingPoliciesOps(ops []ovsdb.Operation, dstNetworkID int, routerName, inportName string,
	dstSubnets []config.CIDRNetworkEntry, srcNetworkID int, nexthops []net.IP, cncName string) ([]ovsdb.Operation, error) {
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
		ipFamily := "v4"
		if utilnet.IsIPv6(dstSubnet.CIDR.IP) {
			ipVersion = "ip6"
			ipFamily = "v6"
		}
		match := fmt.Sprintf(`inport == "%s" && %s.dst == %s`, inportName, ipVersion, dstSubnet.CIDR.String())

		dbIndexes := libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyClusterNetworkConnect, controllerName,
			map[libovsdbops.ExternalIDKey]string{
				libovsdbops.DestinationNetworkIDKey: strconv.Itoa(dstNetworkID),
				libovsdbops.SourceNetworkIDKey:      strconv.Itoa(srcNetworkID),
				libovsdbops.IPFamilyKey:             ipFamily,
				libovsdbops.ObjectNameKey:           cncName,
				libovsdbops.RouterNameKey:           routerName,
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

	networkID := netInfo.GetNetworkID()

	// Get the network's pod subnets
	podSubnets := netInfo.Subnets()

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
			portPairInfo, err := GetP2PAddresses(subnets, nodeID)
			if err != nil {
				return nil, fmt.Errorf("failed to calculate P2P IP addresses for node %s and Layer3 network %s: %v", node.Name, networkName, err)
			}
			nexthops := util.IPNetsToIPs(portPairInfo.networkPortIPs)

			// Create route for this node's subnets
			ops, err = c.createStaticRoutesOps(ops, networkID, connectRouterName, nodeSubnets, nexthops, cncName, nodeID)
			if err != nil {
				return nil, fmt.Errorf("failed to create static route ops for node %s: %v", node.Name, err)
			}
		}
	}
	if netInfo.TopologyType() == ovntypes.Layer2Topology {
		// For Layer2, create a single route to the network's subnets
		portPairInfo, err := GetP2PAddresses(subnets, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to calculate P2P IP addresses for Layer2 network %s: %v", networkName, err)
		}
		nexthops := util.IPNetsToIPs(portPairInfo.networkPortIPs)

		var podSubnetIPNets []*net.IPNet
		for _, entry := range podSubnets {
			podSubnetIPNets = append(podSubnetIPNets, entry.CIDR)
		}

		ops, err = c.createStaticRoutesOps(ops, networkID, connectRouterName, podSubnetIPNets, nexthops, cncName, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to create static route ops for Layer2 network %s: %v", networkName, err)
		}
	}

	return ops, nil
}

// createStaticRoutesOps returns ops to create logical router static routes.
func (c *Controller) createStaticRoutesOps(ops []ovsdb.Operation, networkID int, routerName string, dstSubnets []*net.IPNet,
	nexthops []net.IP, cncName string, nodeID int) ([]ovsdb.Operation, error) {
	for _, dstSubnet := range dstSubnets {
		// Find matching nexthop (same IP family)
		var nexthop string
		isIPv4Subnet := utilnet.IsIPv4(dstSubnet.IP)
		for _, nh := range nexthops {
			isIPv4Nexthop := utilnet.IsIPv4(nh)
			if isIPv4Subnet == isIPv4Nexthop {
				nexthop = nh.String()
				break
			}
		}
		if nexthop == "" {
			continue
		}

		ipFamily := "v4"
		if !isIPv4Subnet {
			ipFamily = "v6"
		}

		dbIndexes := libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterStaticRouteClusterNetworkConnect, controllerName,
			map[libovsdbops.ExternalIDKey]string{
				libovsdbops.NetworkIDKey:  strconv.Itoa(networkID),
				libovsdbops.NodeIDKey:     strconv.Itoa(nodeID),
				libovsdbops.IPFamilyKey:   ipFamily,
				libovsdbops.ObjectNameKey: cncName, // CNC name
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

// findClusterIPLoadBalancers finds all ClusterIP service load balancers for a network.
// ClusterIP LBs have names ending with "_cluster" and are tagged with the network name.
func (c *Controller) findClusterIPLoadBalancers(netInfo util.NetInfo) ([]*nbdb.LoadBalancer, error) {
	networkName := netInfo.GetNetworkName()
	result, err := c.findClusterIPLoadBalancersForNetworks(sets.New(networkName))
	if err != nil {
		return nil, err
	}
	return result[networkName], nil
}

// findClusterIPLoadBalancersForNetworks finds all ClusterIP service load balancers for multiple networks
// in a single DB scan. Returns a map of networkName -> []*nbdb.LoadBalancer.
// Each service maps to one load balancer per protocol (TCP/UDP/SCTP) for clusterIPs, externalIPs and loadbalancerVIPs.
// So that's maximum 3 clusterIPload balancers per service. See services/loadbalancer.go for more details
// (https://github.com/ovn-kubernetes/ovn-kubernetes/blob/9156a8259dc7c0a9f2150113f313219c643fa866/go-controller/pkg/ovn/controller/services/lb_config.go#L121).
func (c *Controller) findClusterIPLoadBalancersForNetworks(networkNames sets.Set[string]) (map[string][]*nbdb.LoadBalancer, error) {
	predicate := func(lb *nbdb.LoadBalancer) bool {
		// Must be a Service LB
		if lb.ExternalIDs[ovntypes.LoadBalancerKindExternalID] != "Service" {
			return false
		}
		// Must belong to one of the networks
		if !networkNames.Has(lb.ExternalIDs[ovntypes.NetworkExternalID]) {
			return false
		}
		// Must be a ClusterIP LB (name contains the suffix "_cluster")
		return strings.HasSuffix(lb.Name, "_cluster")
	}

	lbs, err := libovsdbops.FindLoadBalancersWithPredicate(c.nbClient, predicate)
	if err != nil {
		return nil, err
	}

	// Group LBs by network name
	result := make(map[string][]*nbdb.LoadBalancer)
	for _, lb := range lbs {
		networkName := lb.ExternalIDs[ovntypes.NetworkExternalID]
		result[networkName] = append(result[networkName], lb)
	}

	return result, nil
}

// getNetworkSwitchName returns the logical switch name for a network for the local zone node.
// For Layer2, there's a single distributed switch (localZoneNode is ignored).
// For Layer3, there's a switch per node, and we use direct lookup by constructing
// the switch name from the node name (avoiding predicate scan over all switches).
// We only support 1 node per zone for this feature.
func (c *Controller) getNetworkSwitchName(netInfo util.NetInfo) (string, error) {
	switch netInfo.TopologyType() {
	case ovntypes.Layer2Topology:
		return netInfo.GetNetworkScopedSwitchName(""), nil
	case ovntypes.Layer3Topology:
		if c.localZoneNode == nil {
			return "", fmt.Errorf("no local zone node found for layer3 network %s", netInfo.GetNetworkName())
		}
		return netInfo.GetNetworkScopedSwitchName(c.localZoneNode.Name), nil
	default:
		return "", fmt.Errorf("unsupported topology type: %s", netInfo.TopologyType())
	}
}

// ensureLoadBalancersOps builds ops to attach this network's LBs to all other connectednetworks' switches.
// This is called for each network in allocatedSubnets, so over the full reconciliation loop,
// all pairs of networks get LB attachments in both directions (like routes and policies).
// It finds ClusterIP LBs from each connected network and attaches them to the
// switches of all OTHER connected networks.
// NOTE: Service deletions are handled automatically by OVN weak references.
// When an LB is deleted by the services controller, OVSDB automatically removes
// the LB UUID from all LogicalSwitch.load_balancer columns (weak ref with min=0).
func (c *Controller) ensureLoadBalancersOps(ops []ovsdb.Operation, netInfo util.NetInfo,
	allocatedSubnets map[string][]*net.IPNet) ([]ovsdb.Operation, error) {

	// Get this network's LBs
	thisnetworkLBs, err := c.findClusterIPLoadBalancers(netInfo)
	if err != nil {
		return ops, fmt.Errorf("failed to find ClusterIP LBs for network %s: %w", netInfo.GetNetworkName(), err)
	}

	if len(thisnetworkLBs) == 0 {
		klog.V(5).Infof("No ClusterIP LBs found for network %s, skipping service connectivity", netInfo.GetNetworkName())
		return ops, nil
	}

	thisNetworkID := netInfo.GetNetworkID()
	klog.V(5).Infof("Ensuring service connectivity for network %s (ID=%d): %d LBs",
		netInfo.GetNetworkName(), thisNetworkID, len(thisnetworkLBs))

	// Attach this network's LBs to all OTHER networks' switches.
	// Note: We iterate allocatedSubnets again here (it's also iterated by the caller) because
	// this creates the full mesh of LB attachments. The outer loop in syncNetworkConnections selects
	// the SOURCE network (whose LBs are attached), while this inner loop finds all
	// DESTINATION networks (whose switches receive the LBs). This is O(N²) which is intentional
	// for a full mesh connectivity between N networks.
	// This is typically fine since number of networks that are expected to be connected by a CNC is small, eg. 10.
	for otherOwner := range allocatedSubnets {
		_, otherNetworkID, err := util.ParseNetworkOwner(otherOwner)
		if err != nil {
			klog.Warningf("Failed to parse network owner key %s: %v", otherOwner, err)
			continue
		}

		// Skip same network - LBs are already attached by services controller
		if otherNetworkID == thisNetworkID {
			continue
		}

		otherNetInfo := c.networkManager.GetNetworkByID(otherNetworkID)
		if otherNetInfo == nil {
			klog.Warningf("Network with ID %d not found", otherNetworkID)
			continue
		}

		otherSwitchName, err := c.getNetworkSwitchName(otherNetInfo)
		if err != nil {
			klog.Warningf("Failed to get switch name for network %s: %v", otherNetInfo.GetNetworkName(), err)
			continue
		}

		// Attach this network's LBs to other network's switch
		otherSwitch := &nbdb.LogicalSwitch{Name: otherSwitchName}
		addOps, err := libovsdbops.AddLoadBalancersToLogicalSwitchOps(c.nbClient, nil, otherSwitch, thisnetworkLBs...)
		if err != nil {
			return ops, fmt.Errorf("failed to create ops to attach LBs to switch %s: %w", otherSwitchName, err)
		}
		ops = append(ops, addOps...)
		klog.V(5).Infof("Adding %d LBs from network %d to switch %s (network %d)",
			len(thisnetworkLBs), thisNetworkID, otherSwitchName, otherNetworkID)
	}

	return ops, nil
}

// cleanupServiceConnectivity removes all cross-network LB attachments for a CNC.
// Called when ClusterIPServiceNetwork is disabled on the CNC or when CNC is deleted.
// Uses the cache to determine which networks had LB attachments (stale cache is correct
// here since we want to clean up what was previously connected).
// Since cleanupLoadBalancersOps is bidirectional (removes A's LBs from B AND B's LBs from A),
// we only need to iterate each network once with "remaining" being networks after it in the list.
// This covers all pairs exactly once: A↔B, A↔C, B↔C for networks [A, B, C].
func (c *Controller) cleanupServiceConnectivity(cncName string) error {
	cncState, exists := c.cncCache[cncName]
	if !exists || cncState == nil {
		return fmt.Errorf("CNC %s state not found in cache", cncName)
	}

	networkList := cncState.connectedNetworks.UnsortedList()
	var ops []ovsdb.Operation

	for i, owner := range networkList {
		_, networkID, err := util.ParseNetworkOwner(owner)
		if err != nil {
			klog.Warningf("Failed to parse network owner key %s: %v", owner, err)
			continue
		}

		// Only pass networks AFTER this one as "remaining"
		// Since cleanupLoadBalancersOps is bidirectional, this covers all pairs without redundancy
		remaining := sets.New[string]()
		for j := i + 1; j < len(networkList); j++ {
			remaining.Insert(networkList[j])
		}

		if remaining.Len() == 0 {
			continue // Last network, all pairs already handled
		}

		ops, err = c.cleanupLoadBalancersOps(ops, networkID, remaining)
		if err != nil {
			klog.Warningf("Failed to cleanup LBs for network %d: %v", networkID, err)
		}
	}

	if len(ops) > 0 {
		if _, err := libovsdbops.TransactAndCheck(c.nbClient, ops); err != nil {
			return fmt.Errorf("failed to cleanup service connectivity: %w", err)
		}
		klog.Infof("CNC %s: cleaned up %d cross-network LB attachment operations", cncName, len(ops))
	}

	return nil
}

// cleanupLoadBalancersOps creates ops to cleanup cross-network LB attachments
// when a network is disconnected from a CNC with ClusterIPServiceNetwork enabled.
// This removes:
// 1. Disconnected network's LBs from remaining connected networks' switches
// 2. Remaining connected networks' LBs from disconnected network's switches
func (c *Controller) cleanupLoadBalancersOps(ops []ovsdb.Operation,
	disconnectedNetworkID int, remainingNetworks sets.Set[string]) ([]ovsdb.Operation, error) {

	// Try to get the disconnected network's info - it might still exist in network manager
	disconnectedNetInfo := c.networkManager.GetNetworkByID(disconnectedNetworkID)
	if disconnectedNetInfo == nil {
		// Network no longer exists in network manager - we can't find its LBs or switches
		// But the LBs belonging to this network will be deleted by services controller when
		// the network is fully removed, and weak refs will auto-cleanup
		klog.V(4).Infof("Disconnected network ID %d not found in network manager, skipping LB cleanup", disconnectedNetworkID)
		return ops, nil
	}

	// Find LBs for the disconnected network
	disconnectedLBs, err := c.findClusterIPLoadBalancers(disconnectedNetInfo)
	if err != nil {
		return ops, fmt.Errorf("failed to find LBs for disconnected network: %w", err)
	}

	// Find switch for the disconnected network
	disconnectedSwitchName, err := c.getNetworkSwitchName(disconnectedNetInfo)
	if err != nil {
		return ops, fmt.Errorf("failed to get switch name for disconnected network: %w", err)
	}

	// First pass: collect remaining network info (names and switch names)
	remainingNetworkNames := sets.New[string]()
	remainingSwitchNames := make(map[string]string) // networkName -> switchName
	for remainingOwner := range remainingNetworks {
		_, networkID, err := util.ParseNetworkOwner(remainingOwner)
		if err != nil {
			klog.Warningf("Failed to parse remaining network owner key %s: %v", remainingOwner, err)
			continue
		}
		remainingNetInfo := c.networkManager.GetNetworkByID(networkID)
		if remainingNetInfo == nil {
			klog.Warningf("Network with ID %d not found", networkID)
			continue
		}

		switchName, err := c.getNetworkSwitchName(remainingNetInfo)
		if err != nil {
			klog.Warningf("Failed to get switch name for remaining network %s: %v", remainingNetInfo.GetNetworkName(), err)
			continue
		}

		networkName := remainingNetInfo.GetNetworkName()
		remainingNetworkNames.Insert(networkName)
		remainingSwitchNames[networkName] = switchName
	}

	// Find all LBs for remaining networks in a single DB scan
	remainingLBsByNetwork, err := c.findClusterIPLoadBalancersForNetworks(remainingNetworkNames)
	if err != nil {
		return ops, fmt.Errorf("failed to find LBs for remaining networks: %w", err)
	}

	// Build ops for each remaining network
	for networkName, switchName := range remainingSwitchNames {
		// Remove disconnected network's LBs from remaining network's switch
		if len(disconnectedLBs) > 0 {
			sw := &nbdb.LogicalSwitch{Name: switchName}
			removeOps, err := libovsdbops.RemoveLoadBalancersFromLogicalSwitchOps(c.nbClient, nil, sw, disconnectedLBs...)
			if err != nil {
				klog.Warningf("Failed to create ops to remove LBs from switch %s: %v", switchName, err)
				continue
			}
			ops = append(ops, removeOps...)
			klog.V(5).Infof("Removing %d LBs from disconnected network %s from switch %s",
				len(disconnectedLBs), disconnectedNetInfo.GetNetworkName(), switchName)
		}

		// Remove remaining network's LBs from disconnected network's switch
		remainingLBs := remainingLBsByNetwork[networkName]
		if len(remainingLBs) > 0 {
			sw := &nbdb.LogicalSwitch{Name: disconnectedSwitchName}
			removeOps, err := libovsdbops.RemoveLoadBalancersFromLogicalSwitchOps(c.nbClient, nil, sw, remainingLBs...)
			if err != nil {
				klog.Warningf("Failed to create ops to remove LBs from switch %s: %v", disconnectedSwitchName, err)
				continue
			}
			ops = append(ops, removeOps...)
			klog.V(5).Infof("Removing %d LBs of remaining network %s from disconnected switch %s",
				len(remainingLBs), networkName, disconnectedSwitchName)
		}
	}

	return ops, nil
}
