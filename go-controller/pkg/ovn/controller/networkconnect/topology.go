package networkconnect

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
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

// getCNCServiceLBGroupName returns the LoadBalancerGroup name for a CNC's cross-network service LBs.
// Each CNC gets its own LBG so that overlapping CNCs don't interfere with each other's cleanup.
func getCNCServiceLBGroupName(cncName string) string {
	return ovntypes.NetworkConnectServiceLBGroupPrefix + cncName
}

// findCNCServiceLBGroup looks up the CNC's service LoadBalancerGroup by name and returns it
// with its UUID populated. Returns nil, nil if the LBG doesn't exist.
// caller checks for nil value and handles it appropriately based on if its create or delete case
func (c *Controller) findCNCServiceLBGroup(cncName string) (*nbdb.LoadBalancerGroup, error) {
	lbgName := getCNCServiceLBGroupName(cncName)
	lbg, err := libovsdbops.GetLoadBalancerGroup(c.nbClient, &nbdb.LoadBalancerGroup{Name: lbgName})
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to find LBG %s: %w", lbgName, err)
	}
	return lbg, nil
}

// buildACLDBIDs builds DbObjectIDs for a CNC-owned ACL.
func buildACLDBIDs(cncName, aclType string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLClusterNetworkConnect, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: cncName,
			libovsdbops.TypeKey:       aclType,
		})
}

// buildLRPortDBIDs builds DbObjectIDs for a CNC-owned Logical Router Port.
func buildLRPortDBIDs(cncName, networkID, nodeID, routerName string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPortClusterNetworkConnect, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: cncName,
			libovsdbops.NetworkIDKey:  networkID,
			libovsdbops.NodeIDKey:     nodeID,
			libovsdbops.RouterNameKey: routerName,
		})
}

// buildLRPolicyDBIDs builds DbObjectIDs for a CNC-owned Logical Router Policy.
func buildLRPolicyDBIDs(cncName, srcNetID, dstNetID, ipFamily, routerName string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyClusterNetworkConnect, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey:           cncName,
			libovsdbops.SourceNetworkIDKey:      srcNetID,
			libovsdbops.DestinationNetworkIDKey: dstNetID,
			libovsdbops.IPFamilyKey:             ipFamily,
			libovsdbops.RouterNameKey:           routerName,
		})
}

// buildLRExternalIDs builds ExternalIDs for a CNC logical router.
func buildLRExternalIDs(cncName string) map[string]string {
	return map[string]string{
		libovsdbops.ObjectNameKey.String():      cncName,
		libovsdbops.OwnerControllerKey.String(): controllerName,
		libovsdbops.OwnerTypeKey.String():       libovsdbops.ClusterNetworkConnectOwnerType,
	}
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
		Name:        routerName,
		ExternalIDs: buildLRExternalIDs(cnc.Name),
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
// STEP2: Handle partial connectivity ACLs BEFORE creating network connections
// This ensures drop rules are in place before connectivity is established (security)
// STEP3: Create the patch ports connecting network router's to the connect router
// using IPs from the network subnet CNC annotation.
// STEP4: If PodNetworkConnect is enabled, create the logical router policies on network router's
// to steer traffic to the connect router for other connected networks.
// STEP5: If PodNetworkConnect is enabled, add static routes to connect router towards
// each of the connected networks.
// STEP6: If ClusterIPServiceNetwork connectivity if enabled, add load balancers of connected networks
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
	podConnectivityDesired := podConnectivityEnabled(cnc)
	partialConnectivityWasEnabled := cncState.clusterIPServiceNetworkEnabled && !cncState.podNetworkConnectEnabled
	partialConnectivityDesired := serviceConnectivityDesired && !podConnectivityDesired
	var errs []error

	// Prepare partial connectivity ACLs if needed (service connectivity without pod connectivity)
	// If preparation fails, return early since per-network ACL ops require a valid state.
	// It's a security risk if the ACLs are not prepared correctly.
	var partialConnState *partialConnectivityState
	if partialConnectivityDesired {
		partialConnState, err = c.preparePartialConnectivityACLs(cncName, allocatedSubnets)
		if err != nil {
			return fmt.Errorf("CNC %s: failed to prepare partial connectivity ACLs: %w", cncName, err)
		}
	}

	// Create/update the CNC's service LBG before the per-network loop (like partial connectivity ACLs).
	// The LBG is created once (with UUID populated via lookup) and reused inside the loop for each network.
	// If the LBG cannot be created, return early since per-network LBG ops require a valid LBG.
	var serviceLBG *nbdb.LoadBalancerGroup
	if serviceConnectivityDesired {
		lbgName := getCNCServiceLBGroupName(cncName)
		if err := libovsdbops.CreateOrUpdateLoadBalancerGroup(c.nbClient, &nbdb.LoadBalancerGroup{Name: lbgName}); err != nil {
			return fmt.Errorf("CNC %s: failed to create/update LBG %s: %w", cncName, lbgName, err)
		}
		serviceLBG, err = c.findCNCServiceLBGroup(cncName)
		if err != nil || serviceLBG == nil {
			return fmt.Errorf("CNC %s: failed to find LBG %s after creation: %v", cncName, lbgName, err)
		}
	}

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

		// Add partial connectivity ACLs to this network's switch
		// This creates the ACLs (idempotent) and adds them to the switch
		if partialConnectivityDesired {
			createOps, err = c.ensurePartialConnectivityACLsOps(createOps, partialConnState, networkID)
			if err != nil {
				errs = append(errs, fmt.Errorf("CNC %s: failed to ensure partial connectivity ACLs for network %s: %w", cncName, netInfo.GetNetworkName(), err))
				continue
			}
		}

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

		// If ClusterIPServiceNetwork is enabled, add this network's LBs to the CNC's LBG
		// and attach the LBG to this network's switch.
		if serviceConnectivityDesired {
			createOps, err = c.ensureLoadBalancerGroupOps(createOps, serviceLBG, netInfo)
			if err != nil {
				errs = append(errs, fmt.Errorf("CNC %s: failed to ensure LBG for network %s: %w", cncName, netInfo.GetNetworkName(), err))
				continue
			}
		}

		// Transact all ops (ports, policies, routes, ACLs, LBG) in a single transaction per network
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
		// With LBG: remove the disconnected network's LBs from the CNC's LBG,
		// and remove the LBG from the disconnected network's switch.
		if serviceConnectivityDesired {
			deleteOps, err = c.cleanupLoadBalancerGroupOps(deleteOps, cncName, networkID)
			if err != nil {
				errs = append(errs, fmt.Errorf("CNC %s: failed to cleanup service connectivity for network %s: %w", cncName, owner, err))
			}
		}

		// Cleanup partial connectivity ACLs from this network's switch
		if partialConnectivityWasEnabled {
			deleteOps, err = c.cleanupPartialConnectivityACLsOps(deleteOps, cncName, networkID)
			if err != nil {
				errs = append(errs, fmt.Errorf("CNC %s: failed to cleanup partial connectivity ACLs for network %s: %w", cncName, owner, err))
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

	// Cleanup partial connectivity ACLs if transitioning away from partial connectivity mode.
	// Partial = service enabled && pod disabled. Cleanup needed when:
	// - Service was enabled && pod was disabled (partial was active)
	// - AND now either service is disabled OR pod is enabled
	if partialConnectivityWasEnabled && !partialConnectivityDesired {
		klog.V(4).Infof("CNC %s: partial connectivity disabled, cleaning up ACLs", cncName)
		if err := c.cleanupPartialConnectivity(cncName); err != nil {
			errs = append(errs, fmt.Errorf("CNC %s: failed to cleanup partial connectivity: %w", cncName, err))
		}
	}

	// Only update state flags if no errors occurred, so that on the next reconcile
	// the controller correctly detects transitions (e.g., partial connectivity was
	// enabled but setup failed → retry setup instead of skipping to cleanup).
	// NOTE: Since ops are idempotent, its OK even if the errors were partial
	// say affects only service connectivity but not partial connectivity. It's not
	// worth the overhead to track failures separately.
	if len(errs) == 0 {
		cncState.clusterIPServiceNetworkEnabled = serviceConnectivityDesired
		cncState.podNetworkConnectEnabled = podConnectivityDesired
	}

	return utilerrors.Join(errs...)
}

// cleanupNetworkConnections removes all network connections for a CNC.
// This is called when a CNC is being deleted.
// 1. If ClusterIPServiceNetwork was enabled, cleanup cross-network LB attachments for this CNC
// 2. If partial connectivity was enabled (service && !pod), cleanup ACLs and address sets
// 3. Then delete network router ports from the network routers for this CNC
// 4. Then delete routing policies on the network routers for this CNC
func (c *Controller) cleanupNetworkConnections(cncName string, serviceConnectivityWasEnabled, podConnectivityWasEnabled bool) error {
	// Cleanup cross-network LB attachments if ClusterIPServiceNetwork was enabled
	if serviceConnectivityWasEnabled {
		if err := c.cleanupServiceConnectivity(cncName); err != nil {
			return fmt.Errorf("failed to cleanup service connectivity for CNC %s: %v", cncName, err)
		}
	}

	// Cleanup partial connectivity ACLs if partial was enabled (service enabled && pod disabled)
	partialConnectivityWasEnabled := serviceConnectivityWasEnabled && !podConnectivityWasEnabled
	if partialConnectivityWasEnabled {
		if err := c.cleanupPartialConnectivity(cncName); err != nil {
			return fmt.Errorf("failed to cleanup partial connectivity for CNC %s: %v", cncName, err)
		}
	}

	return c.cleanupNetworkConnectivity(cncName)
}

// cleanupNetworkConnectivity removes all router ports and routing policies owned by a CNC
// from the network routers. Ports on the connect router are skipped (the connect router
// is deleted separately). Discovery is done from OVN directly, so this is safe to call
// even during startup repair when the cncCache may not be populated.
func (c *Controller) cleanupNetworkConnectivity(cncName string) error {
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
		klog.Infof("CNC %s: cleaned up network connectivity (%d ops)", cncName, len(ops))
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

	dbIndexes := buildLRPortDBIDs(cncName, strconv.Itoa(networkID), strconv.Itoa(nodeID), routerName)

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

		dbIndexes := buildLRPolicyDBIDs(cncName, strconv.Itoa(srcNetworkID), strconv.Itoa(dstNetworkID), ipFamily, routerName)
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

// countExpectedClusterIPLBs returns the number of ClusterIP LBs expected for a network.
// Each ClusterIP service produces one LB per unique protocol (TCP/UDP/SCTP).
func (c *Controller) countExpectedClusterIPLBs(netInfo util.NetInfo) (int, error) {
	namespaces, err := c.networkManager.GetActiveNetworkNamespaces(netInfo.GetNetworkName())
	if err != nil {
		return 0, fmt.Errorf("failed to get namespaces for network %s: %w", netInfo.GetNetworkName(), err)
	}
	expectedCount := 0
	for _, ns := range namespaces {
		services, err := c.serviceLister.Services(ns).List(labels.Everything())
		if err != nil {
			return 0, fmt.Errorf("failed to list services in namespace %s: %w", ns, err)
		}
		for _, svc := range services {
			if !util.IsClusterIPSet(svc) {
				continue // skip headless services
			}
			// Each service gets 1 LB per unique protocol
			protocols := sets.New[corev1.Protocol]()
			for _, port := range svc.Spec.Ports {
				protocols.Insert(port.Protocol)
			}
			expectedCount += protocols.Len()
		}
	}
	return expectedCount, nil
}

// findLoadBalancers finds and validates ClusterIP LBs for a single network.
// Returns the found LBs and an error if the found count doesn't match the expected count.
// This is a pure find+validate function -- it does NOT build any switch or LBG mutation ops.
// The caller (ensureLoadBalancerGroupOps) handles attaching the LBs to the CNC's LBG.
// The returned LBs are valid even when an error is returned (partial results).
func (c *Controller) findLoadBalancers(netInfo util.NetInfo) ([]*nbdb.LoadBalancer, error) {
	// Get this network's LBs
	thisnetworkLBs, err := c.findClusterIPLoadBalancers(netInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to find ClusterIP LBs for network %s: %w", netInfo.GetNetworkName(), err)
	}

	// Count expected LBs from ClusterIP services in this network's namespaces so we can detect
	// when the services controller hasn't finished creating LBs yet. If found != expected we set
	// needsRetry: we still transact the ops we have (partial connectivity is better than none),
	// but the caller will requeue the CNC so we run again and attach any LBs that appear later.
	expectedLBCount, err := c.countExpectedClusterIPLBs(netInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to count expected LBs for network %s: %w", netInfo.GetNetworkName(), err)
	}

	foundLBCount := len(thisnetworkLBs)

	klog.V(5).Infof("Network %s (ID=%d): found %d ClusterIP LBs (expected %d)",
		netInfo.GetNetworkName(), netInfo.GetNetworkID(), foundLBCount, expectedLBCount)

	if foundLBCount != expectedLBCount {
		return thisnetworkLBs, fmt.Errorf("network %s: found %d ClusterIP LBs but expected %d",
			netInfo.GetNetworkName(), foundLBCount, expectedLBCount)
	}

	return thisnetworkLBs, nil
}

// ensureLoadBalancerGroupOps adds the given network's ClusterIP LBs to the CNC's
// LoadBalancerGroup and attaches the LBG to the network's switch.
// The LBG must already exist (created before the per-network loop).
// Errors are collected but don't prevent building ops for what succeeds.
func (c *Controller) ensureLoadBalancerGroupOps(ops []ovsdb.Operation,
	lbg *nbdb.LoadBalancerGroup, netInfo util.NetInfo) ([]ovsdb.Operation, error) {

	var errs []error

	// Find and validate LBs for this network
	lbs, err := c.findLoadBalancers(netInfo)
	if err != nil {
		errs = append(errs, err)
	}

	// Add this network's LBs to the CNC's LBG
	if len(lbs) > 0 {
		ops, err = libovsdbops.AddLoadBalancersToGroupOps(c.nbClient, ops, lbg, lbs...)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to add LBs to LBG %s for network %s: %w",
				lbg.Name, netInfo.GetNetworkName(), err))
		} else {
			klog.V(5).Infof("Adding %d LBs from network %s to LBG %s",
				len(lbs), netInfo.GetNetworkName(), lbg.Name)
		}
	}

	// Attach the LBG to this network's switch
	switchName, err := c.getNetworkSwitchName(netInfo)
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to get switch name for network %s: %w", netInfo.GetNetworkName(), err))
	} else {
		sw := &nbdb.LogicalSwitch{Name: switchName}
		ops, err = libovsdbops.AddLoadBalancerGroupsToLogicalSwitchOps(c.nbClient, ops, sw, lbg)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to attach LBG %s to switch %s: %w", lbg.Name, switchName, err))
		} else {
			klog.V(5).Infof("Attaching LBG %s to switch %s (network %s)",
				lbg.Name, switchName, netInfo.GetNetworkName())
		}
	}

	return ops, utilerrors.Join(errs...)
}

// cleanupServiceConnectivity removes all cross-network LB attachments for a CNC.
// Called when ClusterIPServiceNetwork is disabled on the CNC or when CNC is deleted.
// With the LBG approach, this removes the CNC's LBG from all switches that reference it
// (LogicalSwitch.load_balancer_group is a strong reference, so we must remove
// all references before deleting the LBG), then deletes the LBG itself.
// Switches are discovered from OVN (not from cncCache) so this is safe to call
// even when the cache is out of sync or during startup repair.
func (c *Controller) cleanupServiceConnectivity(cncName string) error {
	lbgName := getCNCServiceLBGroupName(cncName)

	// Look up the LBG to get its UUID (needed for switch mutation ops)
	lbg, err := c.findCNCServiceLBGroup(cncName)
	if err != nil {
		return fmt.Errorf("failed to find LBG %s: %w", lbgName, err)
	}
	if lbg == nil {
		klog.V(4).Infof("CNC %s: LBG %s not found, nothing to clean up", cncName, lbgName)
		return nil
	}

	// Find all switches that reference this LBG.
	// LogicalSwitch.load_balancer_group is a strong reference, so we cannot
	// delete the LBG while any switch still references it.
	switches, err := libovsdbops.FindLogicalSwitchesWithPredicate(c.nbClient, func(sw *nbdb.LogicalSwitch) bool {
		for _, uuid := range sw.LoadBalancerGroup {
			if uuid == lbg.UUID {
				return true
			}
		}
		return false
	})
	if err != nil {
		return fmt.Errorf("failed to find switches referencing LBG %s: %w", lbgName, err)
	}

	var ops []ovsdb.Operation
	for _, sw := range switches {
		removeOps, err := libovsdbops.RemoveLoadBalancerGroupsFromLogicalSwitchOps(c.nbClient, nil, sw, lbg)
		if err != nil {
			klog.Warningf("CNC %s: failed to remove LBG %s from switch %s: %v", cncName, lbgName, sw.Name, err)
			continue
		}
		ops = append(ops, removeOps...)
	}

	// Then delete the LBG itself
	deleteOps, err := libovsdbops.DeleteLoadBalancerGroupsOps(c.nbClient, nil, lbg)
	if err != nil {
		return fmt.Errorf("failed to create delete ops for LBG %s: %w", lbgName, err)
	}
	ops = append(ops, deleteOps...)

	if len(ops) > 0 {
		if _, err := libovsdbops.TransactAndCheck(c.nbClient, ops); err != nil {
			return fmt.Errorf("failed to cleanup service LBG %s for CNC %s: %w", lbgName, cncName, err)
		}
		klog.Infof("CNC %s: cleaned up service LBG %s (removed from %d switch(es))", cncName, lbgName, len(switches))
	}

	return nil
}

// cleanupLoadBalancerGroupOps creates ops to cleanup cross-network LB attachments
// when a network is disconnected from a CNC with ClusterIPServiceNetwork enabled.
// With the LBG approach, this:
// 1. Removes the disconnected network's LBs from the CNC's LoadBalancerGroup
// 2. Removes the CNC's LBG from the disconnected network's switch
func (c *Controller) cleanupLoadBalancerGroupOps(ops []ovsdb.Operation,
	cncName string, disconnectedNetworkID int) ([]ovsdb.Operation, error) {

	lbgName := getCNCServiceLBGroupName(cncName)

	// Look up the LBG to get its UUID (needed for switch mutation ops)
	lbg, err := c.findCNCServiceLBGroup(cncName)
	if err != nil {
		return ops, fmt.Errorf("failed to find LBG %s: %w", lbgName, err)
	}
	if lbg == nil {
		klog.V(4).Infof("CNC %s: LBG %s not found, nothing to clean up", cncName, lbgName)
		return ops, nil
	}

	// Try to get the disconnected network's info - it might still exist in network manager
	disconnectedNetInfo := c.networkManager.GetNetworkByID(disconnectedNetworkID)
	if disconnectedNetInfo == nil {
		// Network no longer exists in network manager - we can't find its LBs or switches.
		// The LBs belonging to this network will be deleted by services controller when
		// the network is fully removed, and weak refs will auto-remove them from the LBG.
		klog.V(4).Infof("Disconnected network ID %d not found in network manager, skipping LBG cleanup", disconnectedNetworkID)
		return ops, nil
	}

	// Remove the disconnected network's LBs from the CNC's LBG
	disconnectedLBs, err := c.findClusterIPLoadBalancers(disconnectedNetInfo)
	if err != nil {
		return ops, fmt.Errorf("failed to find LBs for disconnected network %s: %w", disconnectedNetInfo.GetNetworkName(), err)
	}
	if len(disconnectedLBs) > 0 {
		removeOps, err := libovsdbops.RemoveLoadBalancersFromGroupOps(c.nbClient, nil, lbg, disconnectedLBs...)
		if err != nil {
			return ops, fmt.Errorf("failed to remove LBs from LBG %s: %w", lbgName, err)
		}
		ops = append(ops, removeOps...)
		klog.V(5).Infof("CNC %s: removing %d LBs of network %s from LBG %s",
			cncName, len(disconnectedLBs), disconnectedNetInfo.GetNetworkName(), lbgName)
	}

	// Remove the CNC's LBG from the disconnected network's switch
	disconnectedSwitchName, err := c.getNetworkSwitchName(disconnectedNetInfo)
	if err != nil {
		klog.Warningf("CNC %s: failed to get switch name for disconnected network %s: %v",
			cncName, disconnectedNetInfo.GetNetworkName(), err)
		return ops, nil
	}

	sw := &nbdb.LogicalSwitch{Name: disconnectedSwitchName}
	removeOps, err := libovsdbops.RemoveLoadBalancerGroupsFromLogicalSwitchOps(c.nbClient, nil, sw, lbg)
	if err != nil {
		klog.Warningf("CNC %s: failed to remove LBG %s from switch %s: %v",
			cncName, lbgName, disconnectedSwitchName, err)
		return ops, nil
	}
	ops = append(ops, removeOps...)
	klog.V(5).Infof("CNC %s: removing LBG %s from disconnected switch %s",
		cncName, lbgName, disconnectedSwitchName)

	return ops, nil
}

// getConnectedUDNSubnetsAddressSetDbIDs returns DbObjectIDs for a partial connectivity address set.
func getConnectedUDNSubnetsAddressSetDbIDs(cncName string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetClusterNetworkConnect, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: cncName,
		})
}

// partialConnectivityState holds pre-computed ACLs for partial connectivity.
// This is built once at the start of syncNetworkConnections and reused per-network.
type partialConnectivityState struct {
	sharedACLs      []*nbdb.ACL       // allow-service + drop-pod (same for all switches)
	perNetworkACLs  map[int]*nbdb.ACL // networkID -> allow-same-network ACL
	networkSwitches map[int]string    // networkID -> switch name
}

// preparePartialConnectivityACLs creates the address set and builds the shared ACLs, and per-network ACLs.
// This is called once before the network creation loop.
// Returns nil if there are fewer than 2 networks (no partial connectivity needed).
func (c *Controller) preparePartialConnectivityACLs(cncName string, allocatedSubnets map[string][]*net.IPNet) (*partialConnectivityState, error) {

	state := &partialConnectivityState{
		perNetworkACLs:  make(map[int]*nbdb.ACL),
		networkSwitches: make(map[int]string),
	}

	// Collect all subnets for the address set, and build per-network ACLs
	var allSubnets []string
	for owner := range allocatedSubnets {
		_, networkID, err := util.ParseNetworkOwner(owner)
		if err != nil {
			klog.Warningf("Failed to parse owner key %s: %v", owner, err)
			continue
		}

		netInfo := c.networkManager.GetNetworkByID(networkID)
		if netInfo == nil {
			klog.Warningf("Network with ID %d not found", networkID)
			continue
		}

		// Get switch name for this network
		switchName, err := c.getNetworkSwitchName(netInfo)
		if err != nil {
			klog.Warningf("Failed to get switch name for network %s: %v", netInfo.GetNetworkName(), err)
			continue
		}
		state.networkSwitches[networkID] = switchName

		// Get the actual network subnets (pod subnets) for the address set
		var networkSubnets []string
		for _, subnet := range netInfo.Subnets() {
			if subnet.CIDR != nil {
				subnetStr := subnet.CIDR.String()
				allSubnets = append(allSubnets, subnetStr)
				networkSubnets = append(networkSubnets, subnetStr)
			}
		}

		// Build allow-same-network ACL for this network (priority 475)
		if len(networkSubnets) > 0 {
			acl := c.buildAllowSameNetworkACL(cncName, networkID, networkSubnets)
			if acl != nil {
				state.perNetworkACLs[networkID] = acl
			}
		}
	}

	// Create address set directly (not as ops) - this is simpler than passing ops around
	dbIDs := getConnectedUDNSubnetsAddressSetDbIDs(cncName)
	as, err := c.addressSetFactory.NewAddressSet(dbIDs, allSubnets)
	if err != nil {
		return nil, fmt.Errorf("failed to create address set: %w", err)
	}

	// Get the hashed address set names for ACL matches
	hashNameV4, hashNameV6 := as.GetASHashNames()

	// Build shared ACLs (allow-service at 500, drop at 450)
	state.sharedACLs = c.buildSharedPartialConnectivityACLs(cncName, hashNameV4, hashNameV6)

	return state, nil
}

// ensurePartialConnectivityACLsOps builds ops to add partial connectivity ACLs to a network's switch.
// The ACLs are created/updated first, then added to the switch.
// Note: Address set is already created in preparePartialConnectivityACLs.
func (c *Controller) ensurePartialConnectivityACLsOps(ops []ovsdb.Operation, state *partialConnectivityState,
	networkID int) ([]ovsdb.Operation, error) {

	if state == nil {
		return ops, fmt.Errorf("partial connectivity state is nil for network ID %d", networkID)
	}

	switchName, ok := state.networkSwitches[networkID]
	if !ok {
		return ops, fmt.Errorf("switch name not found for network ID %d in partial connectivity state", networkID)
	}

	// Build the ACL list for this switch: shared ACLs + this network's allow-same-network ACL
	switchACLs := append([]*nbdb.ACL{}, state.sharedACLs...)
	if perNetACL, exists := state.perNetworkACLs[networkID]; exists {
		switchACLs = append(switchACLs, perNetACL)
	}

	// Create/update ACLs (idempotent - first network creates them, others are no-op)
	var err error
	ops, err = libovsdbops.CreateOrUpdateACLsOps(c.nbClient, ops, nil, switchACLs...)
	if err != nil {
		return ops, fmt.Errorf("failed to create ACL ops: %w", err)
	}

	// Add ACLs to this switch
	ops, err = libovsdbops.AddACLsToLogicalSwitchOps(c.nbClient, ops, switchName, switchACLs...)
	if err != nil {
		return ops, fmt.Errorf("failed to add ACLs to switch %s: %w", switchName, err)
	}

	return ops, nil
}

// cleanupPartialConnectivityACLsOps builds ops to remove partial connectivity ACLs from a network's switch.
// Called when a network is disconnected.
func (c *Controller) cleanupPartialConnectivityACLsOps(ops []ovsdb.Operation, cncName string,
	networkID int) ([]ovsdb.Operation, error) {

	// Try to get the network's switch name - network might still exist in network manager
	netInfo := c.networkManager.GetNetworkByID(networkID)
	if netInfo == nil {
		klog.V(4).Infof("Network ID %d not found in network manager, skipping ACL cleanup", networkID)
		return ops, nil
	}

	switchName, err := c.getNetworkSwitchName(netInfo)
	if err != nil {
		klog.V(4).Infof("Cannot determine switch name for network %s (ID %d), skipping ACL cleanup: %v",
			netInfo.GetNetworkName(), networkID, err)
		return ops, nil
	}

	// Find all ACLs owned by this CNC
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLClusterNetworkConnect, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: cncName,
		})
	aclPredicate := libovsdbops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
	acls, err := libovsdbops.FindACLsWithPredicate(c.nbClient, aclPredicate)
	if err != nil {
		return ops, fmt.Errorf("failed to find ACLs for CNC %s: %w", cncName, err)
	}

	if len(acls) == 0 {
		return ops, nil
	}

	// Remove all CNC ACLs from this switch
	// ACLs are owned by switches, so removing from switches will garbage-collect the ACL rows
	ops, err = libovsdbops.RemoveACLsFromLogicalSwitchesWithPredicateOps(c.nbClient, ops,
		func(sw *nbdb.LogicalSwitch) bool {
			return sw.Name == switchName
		}, acls...)
	if err != nil {
		return ops, fmt.Errorf("failed to remove ACLs from switch %s: %w", switchName, err)
	}

	return ops, nil
}

// buildSharedPartialConnectivityACLs builds the shared ACLs for partial connectivity.
// These ACLs are identical across all switches: allow-service (500) and drop-pod (450).
// The allow-same-network ACLs (475) are built separately per-network.
func (c *Controller) buildSharedPartialConnectivityACLs(cncName, addressSetNameV4, addressSetNameV6 string) []*nbdb.ACL {
	var acls []*nbdb.ACL

	// Build allow-service ACL match combining all service CIDRs (single ACL)
	var serviceMatches []string
	for _, serviceCIDR := range config.Kubernetes.ServiceCIDRs {
		ipPrefix := "ip4"
		if utilnet.IsIPv6CIDR(serviceCIDR) {
			ipPrefix = "ip6"
		}
		serviceMatches = append(serviceMatches, fmt.Sprintf("%s.dst == %s", ipPrefix, serviceCIDR.String()))
	}

	if len(serviceMatches) > 0 {
		allowMatch := fmt.Sprintf("(%s)", strings.Join(serviceMatches, " || "))
		dbIDs := buildACLDBIDs(cncName, "allow-service")
		allowServiceACL := libovsdbutil.BuildACL(dbIDs, ovntypes.NetworkConnectAllowServiceTrafficPriority,
			allowMatch, nbdb.ACLActionPass, nil, libovsdbutil.LportEgress, 0)
		acls = append(acls, allowServiceACL)
	}

	// Build drop-pod ACL match based on IP mode (single ACL)
	// This drops any new connections to connected networks' pod subnets.
	// Same-network traffic is allowed by the per-network allow-same-network ACLs
	// at higher priority (475). Service traffic is allowed by allow-service ACL (500).
	// We only match on dst (not src) because:
	// 1. This ACL is on egress (from-lport), so it only evaluates outbound pod traffic
	// 2. Matching only dst is more restrictive - blocks traffic regardless of source
	// ct.new is required because the allow-service ACL uses "pass" action which sends
	// traffic to the LB pipeline. After DNAT, the packet's dst becomes the backend pod IP
	// (which is in the connected subnets). Without ct.new, the drop ACL would match the
	// DNAT'd packet and drop it. With ct.new, only new connections are dropped; DNAT'd
	// packets from service traffic are part of an established connection and are not matched.
	// See https://issues.redhat.com/browse/FDP-3124 and once that is solved, we can remove ct.new.
	var dropMatch string
	switch {
	case config.IPv4Mode && config.IPv6Mode:
		dropMatch = fmt.Sprintf("(ip4.dst == $%s || ip6.dst == $%s) && ct.new",
			addressSetNameV4, addressSetNameV6)
	case config.IPv4Mode:
		dropMatch = fmt.Sprintf("ip4.dst == $%s && ct.new", addressSetNameV4)
	case config.IPv6Mode:
		dropMatch = fmt.Sprintf("ip6.dst == $%s && ct.new", addressSetNameV6)
	}

	if dropMatch != "" {
		dbIDs := buildACLDBIDs(cncName, "drop-pod")
		dropPodACL := libovsdbutil.BuildACL(dbIDs, ovntypes.NetworkConnectDropPodTrafficPriority,
			dropMatch, nbdb.ACLActionDrop, nil, libovsdbutil.LportEgress, 0)
		acls = append(acls, dropPodACL)
	}

	return acls
}

// buildAllowSameNetworkACL builds an ACL that allows traffic within the same network.
// This ACL has priority 475, sitting between allow-service (500) and drop-pod (450).
// It prevents the drop ACL from blocking intra-network communication when only
// ClusterIPServiceNetwork connectivity is requested (without PodNetwork).
func (c *Controller) buildAllowSameNetworkACL(cncName string, networkID int, subnets []string) *nbdb.ACL {
	// Separate subnets by IP family
	var v4Subnets, v6Subnets []string
	for _, subnet := range subnets {
		_, ipNet, err := net.ParseCIDR(subnet)
		if err != nil {
			klog.Warningf("Failed to parse subnet %s: %v", subnet, err)
			continue
		}
		if ipNet.IP.To4() != nil {
			v4Subnets = append(v4Subnets, subnet)
		} else {
			v6Subnets = append(v6Subnets, subnet)
		}
	}

	// Build match for same-network traffic (dst in this network's subnets).
	// We only match on dst (not src) because this ACL is on egress (from-lport),
	// so it only evaluates outbound pod traffic. Pods on this switch always have
	// src in this network's subnet, so checking dst is sufficient to identify
	// intra-network traffic.
	var matches []string
	for _, v4Subnet := range v4Subnets {
		matches = append(matches, fmt.Sprintf("ip4.dst == %s", v4Subnet))
	}
	for _, v6Subnet := range v6Subnets {
		matches = append(matches, fmt.Sprintf("ip6.dst == %s", v6Subnet))
	}

	if len(matches) == 0 {
		return nil
	}

	allowMatch := strings.Join(matches, " || ")

	dbIDs := buildACLDBIDs(cncName, fmt.Sprintf("allow-same-network-%d", networkID))

	return libovsdbutil.BuildACL(dbIDs, ovntypes.NetworkConnectAllowSameNetworkPriority,
		allowMatch, nbdb.ACLActionPass, nil, libovsdbutil.LportEgress, 0)
}

// cleanupPartialConnectivity removes partial connectivity ACLs and address sets for a CNC.
func (c *Controller) cleanupPartialConnectivity(cncName string) error {
	// Find all ACLs owned by this CNC using proper DbObjectIDs predicate
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLClusterNetworkConnect, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: cncName,
		})
	aclPredicate := libovsdbops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
	acls, err := libovsdbops.FindACLsWithPredicate(c.nbClient, aclPredicate)
	if err != nil {
		return fmt.Errorf("failed to find ACLs for CNC %s: %w", cncName, err)
	}

	if len(acls) == 0 {
		// No ACLs to clean up, but still try to clean address sets
		goto cleanupAddressSets
	}

	// Remove ACLs from all switches that have them
	// ACLs are owned by switches, so removing from switches will garbage-collect the ACL rows
	err = libovsdbops.RemoveACLsFromLogicalSwitchesWithPredicate(c.nbClient,
		func(sw *nbdb.LogicalSwitch) bool {
			// Check if any of the ACLs are on this switch
			for _, aclUUID := range sw.ACLs {
				for _, acl := range acls {
					if aclUUID == acl.UUID {
						return true
					}
				}
			}
			return false
		}, acls...)
	if err != nil {
		return fmt.Errorf("failed to remove ACLs from switches: %w", err)
	}

cleanupAddressSets:
	// Delete address sets using DestroyAddressSet which handles lookup+delete internally
	err = c.addressSetFactory.DestroyAddressSet(getConnectedUDNSubnetsAddressSetDbIDs(cncName))
	if err != nil {
		return fmt.Errorf("failed to delete address sets for CNC %s: %w", cncName, err)
	}

	klog.V(4).Infof("CNC %s: cleaned up partial connectivity ACLs", cncName)
	return nil
}
