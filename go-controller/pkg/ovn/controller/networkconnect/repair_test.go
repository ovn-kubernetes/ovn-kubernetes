package networkconnect

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	networkconnectlisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/listers/clusternetworkconnect/v1"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// newFakeCNCLister creates a CNC lister backed by an in-memory indexer containing the given CNCs.
func newFakeCNCLister(cncNames ...string) networkconnectlisters.ClusterNetworkConnectLister {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, name := range cncNames {
		_ = indexer.Add(&networkconnectv1.ClusterNetworkConnect{
			ObjectMeta: metav1.ObjectMeta{Name: name},
		})
	}
	return networkconnectlisters.NewClusterNetworkConnectLister(indexer)
}

// setupTestController creates a Controller with a fake NB client and CNC lister for repair tests.
func setupTestController(t *testing.T, initialDB []libovsdbtest.TestData, cncNames ...string) (*Controller, func()) {
	t.Helper()

	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
		NBData: initialDB,
	}, nil)
	require.NoError(t, err)

	c := &Controller{
		nbClient:          nbClient,
		cncLister:         newFakeCNCLister(cncNames...),
		addressSetFactory: addressset.NewOvnAddressSetFactory(nbClient, true, true),
	}
	return c, cleanup.Cleanup
}

func TestRepairStaleCNCs_NoStaleObjects(t *testing.T) {
	// Valid CNC "cnc-valid" exists in both API and OVN DB — nothing should be cleaned up.
	validCNCName := "cnc-valid"

	initialDB := []libovsdbtest.TestData{
		&nbdb.LogicalRouter{
			UUID:        "valid-router-uuid",
			Name:        getConnectRouterName(validCNCName),
			ExternalIDs: buildLRExternalIDs(validCNCName),
		},
	}

	c, cleanup := setupTestController(t, initialDB, validCNCName)
	defer cleanup()

	err := c.repairStaleCNCs()
	require.NoError(t, err)

	_, err = libovsdbops.GetLogicalRouter(c.nbClient, &nbdb.LogicalRouter{Name: getConnectRouterName(validCNCName)})
	require.NoError(t, err, "valid CNC's connect router should not be deleted")
}

func TestRepairStaleCNCs_StaleConnectRouter(t *testing.T) {
	// "stale-cnc" has a connect router in OVN but no corresponding CNC in the API.
	staleCNCName := "stale-cnc"
	validCNCName := "valid-cnc"

	initialDB := []libovsdbtest.TestData{
		&nbdb.LogicalRouter{
			UUID:        "stale-router-uuid",
			Name:        getConnectRouterName(staleCNCName),
			ExternalIDs: buildLRExternalIDs(staleCNCName),
		},
		&nbdb.LogicalRouter{
			UUID:        "valid-router-uuid",
			Name:        getConnectRouterName(validCNCName),
			ExternalIDs: buildLRExternalIDs(validCNCName),
		},
	}

	c, cleanup := setupTestController(t, initialDB, validCNCName)
	defer cleanup()

	err := c.repairStaleCNCs()
	require.NoError(t, err)

	routers, err := libovsdbops.FindLogicalRoutersWithPredicate(c.nbClient, func(lr *nbdb.LogicalRouter) bool {
		return lr.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.ClusterNetworkConnectOwnerType)
	})
	require.NoError(t, err)
	assert.Len(t, routers, 1, "only valid CNC router should remain")
	assert.Equal(t, getConnectRouterName(validCNCName), routers[0].Name)
}

func TestRepairStaleCNCs_StaleLBG(t *testing.T) {
	// "stale-cnc" has a LoadBalancerGroup and a switch referencing it, but no CNC in API.
	staleCNCName := "stale-cnc"
	staleLBGName := ovntypes.NetworkConnectServiceLBGroupPrefix + staleCNCName
	staleLBGUUID := "stale-lbg-uuid"

	initialDB := []libovsdbtest.TestData{
		&nbdb.LoadBalancerGroup{
			UUID: staleLBGUUID,
			Name: staleLBGName,
		},
		&nbdb.LogicalSwitch{
			UUID:              "switch-uuid",
			Name:              "test-switch",
			LoadBalancerGroup: []string{staleLBGUUID},
		},
	}

	c, cleanup := setupTestController(t, initialDB)
	defer cleanup()

	err := c.repairStaleCNCs()
	require.NoError(t, err)

	lbgs, err := libovsdbops.FindLoadBalancerGroupsWithPredicate(c.nbClient, func(lbg *nbdb.LoadBalancerGroup) bool {
		return strings.HasPrefix(lbg.Name, ovntypes.NetworkConnectServiceLBGroupPrefix)
	})
	require.NoError(t, err)
	assert.Empty(t, lbgs, "stale LBG should be deleted")

	sw, err := libovsdbops.FindLogicalSwitchesWithPredicate(c.nbClient, func(sw *nbdb.LogicalSwitch) bool {
		return sw.Name == "test-switch"
	})
	require.NoError(t, err)
	require.Len(t, sw, 1)
	assert.Empty(t, sw[0].LoadBalancerGroup, "switch should no longer reference stale LBG")
}

func TestRepairStaleCNCs_StaleACLs(t *testing.T) {
	// "stale-cnc" has ACLs on a switch but no CNC in API. ACLs should be cleaned up.
	staleCNCName := "stale-cnc"
	aclUUID := "stale-acl-uuid"

	initialDB := []libovsdbtest.TestData{
		&nbdb.ACL{
			UUID:        aclUUID,
			Action:      nbdb.ACLActionAllowRelated,
			Direction:   nbdb.ACLDirectionFromLport,
			Match:       "ip4",
			Priority:    1000,
			ExternalIDs: buildACLDBIDs(staleCNCName, "allow-service").GetExternalIDs(),
		},
		&nbdb.LogicalSwitch{
			UUID: "switch-uuid",
			Name: "test-switch",
			ACLs: []string{aclUUID},
		},
	}

	c, cleanup := setupTestController(t, initialDB)
	defer cleanup()

	err := c.repairStaleCNCs()
	require.NoError(t, err)

	aclPredicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLClusterNetworkConnect, controllerName, nil)
	acls, err := libovsdbops.FindACLsWithPredicate(c.nbClient, libovsdbops.GetPredicate[*nbdb.ACL](aclPredicateIDs, nil))
	require.NoError(t, err)
	assert.Empty(t, acls, "stale ACLs should be deleted")

	sw, err := libovsdbops.FindLogicalSwitchesWithPredicate(c.nbClient, func(sw *nbdb.LogicalSwitch) bool {
		return sw.Name == "test-switch"
	})
	require.NoError(t, err)
	require.Len(t, sw, 1)
	assert.Empty(t, sw[0].ACLs, "switch should no longer reference stale ACL")
}

func TestRepairStaleCNCs_StalePortsAndPolicies(t *testing.T) {
	// "stale-cnc" has router ports and policies on a network router, but no CNC in API.
	// Ports on the connect router should be skipped (deleted with the router itself).
	staleCNCName := "stale-cnc"
	networkRouterName := "udn_network_router"
	connectRouterName := getConnectRouterName(staleCNCName)

	stalePortUUID := "stale-port-uuid"
	staleConnectPortUUID := "stale-connect-port-uuid"
	stalePolicyUUID := "stale-policy-uuid"

	initialDB := []libovsdbtest.TestData{
		&nbdb.LogicalRouter{
			UUID:     "network-router-uuid",
			Name:     networkRouterName,
			Ports:    []string{stalePortUUID},
			Policies: []string{stalePolicyUUID},
		},
		&nbdb.LogicalRouter{
			UUID:        "connect-router-uuid",
			Name:        connectRouterName,
			ExternalIDs: buildLRExternalIDs(staleCNCName),
			Ports:       []string{staleConnectPortUUID},
		},
		&nbdb.LogicalRouterPort{
			UUID:        stalePortUUID,
			Name:        "lrp-cnc-stale-net1",
			ExternalIDs: buildLRPortDBIDs(staleCNCName, "1", "0", networkRouterName).GetExternalIDs(),
		},
		&nbdb.LogicalRouterPort{
			UUID:        staleConnectPortUUID,
			Name:        "lrp-cnc-stale-connect",
			ExternalIDs: buildLRPortDBIDs(staleCNCName, "1", "0", connectRouterName).GetExternalIDs(),
		},
		&nbdb.LogicalRouterPolicy{
			UUID:        stalePolicyUUID,
			Priority:    100,
			Match:       "ip4.src == 10.0.0.0/16",
			Action:      nbdb.LogicalRouterPolicyActionReroute,
			ExternalIDs: buildLRPolicyDBIDs(staleCNCName, "1", "2", "ip4", networkRouterName).GetExternalIDs(),
		},
	}

	c, cleanup := setupTestController(t, initialDB)
	defer cleanup()

	err := c.repairStaleCNCs()
	require.NoError(t, err)

	netRouter, err := libovsdbops.GetLogicalRouter(c.nbClient, &nbdb.LogicalRouter{Name: networkRouterName})
	require.NoError(t, err)
	assert.Empty(t, netRouter.Ports, "stale ports should be removed from network router")
	assert.Empty(t, netRouter.Policies, "stale policies should be removed from network router")

	_, err = libovsdbops.GetLogicalRouter(c.nbClient, &nbdb.LogicalRouter{Name: connectRouterName})
	require.Error(t, err, "stale connect router should be deleted")
}

func TestRepairStaleCNCs_StalePoliciesOnly(t *testing.T) {
	// Edge case: "stale-cnc" has only policies left (ports already cleaned up in a prior run).
	// Discovery should find the stale CNC from policies and clean them up.
	staleCNCName := "stale-cnc"
	networkRouterName := "udn_network_router"
	stalePolicyUUID := "stale-policy-uuid"

	initialDB := []libovsdbtest.TestData{
		&nbdb.LogicalRouter{
			UUID:     "network-router-uuid",
			Name:     networkRouterName,
			Policies: []string{stalePolicyUUID},
		},
		&nbdb.LogicalRouterPolicy{
			UUID:        stalePolicyUUID,
			Priority:    100,
			Match:       "ip4.src == 10.0.0.0/16",
			Action:      nbdb.LogicalRouterPolicyActionReroute,
			ExternalIDs: buildLRPolicyDBIDs(staleCNCName, "1", "2", "ip4", networkRouterName).GetExternalIDs(),
		},
	}

	c, cleanup := setupTestController(t, initialDB)
	defer cleanup()

	err := c.repairStaleCNCs()
	require.NoError(t, err)

	netRouter, err := libovsdbops.GetLogicalRouter(c.nbClient, &nbdb.LogicalRouter{Name: networkRouterName})
	require.NoError(t, err)
	assert.Empty(t, netRouter.Policies, "stale policies should be removed even when no stale ports exist")
}

func TestRepairStaleCNCs_MixedStaleAndValid(t *testing.T) {
	// Mixed scenario: "valid-cnc" exists in API, "stale-cnc" does not.
	// Both have objects in OVN. Only stale objects should be cleaned.
	validCNCName := "valid-cnc"
	staleCNCName := "stale-cnc"
	networkRouterName := "udn_network_router"

	validPortUUID := "valid-port-uuid"
	stalePortUUID := "stale-port-uuid"
	validPolicyUUID := "valid-policy-uuid"
	stalePolicyUUID := "stale-policy-uuid"
	validLBGUUID := "valid-lbg-uuid"
	staleLBGUUID := "stale-lbg-uuid"

	validLBGName := ovntypes.NetworkConnectServiceLBGroupPrefix + validCNCName
	staleLBGName := ovntypes.NetworkConnectServiceLBGroupPrefix + staleCNCName

	initialDB := []libovsdbtest.TestData{
		&nbdb.LogicalRouter{
			UUID:     "network-router-uuid",
			Name:     networkRouterName,
			Ports:    []string{validPortUUID, stalePortUUID},
			Policies: []string{validPolicyUUID, stalePolicyUUID},
		},
		&nbdb.LogicalRouter{
			UUID:        "valid-connect-router-uuid",
			Name:        getConnectRouterName(validCNCName),
			ExternalIDs: buildLRExternalIDs(validCNCName),
		},
		&nbdb.LogicalRouter{
			UUID:        "stale-connect-router-uuid",
			Name:        getConnectRouterName(staleCNCName),
			ExternalIDs: buildLRExternalIDs(staleCNCName),
		},
		&nbdb.LogicalRouterPort{
			UUID:        validPortUUID,
			Name:        "lrp-valid-net1",
			ExternalIDs: buildLRPortDBIDs(validCNCName, "1", "0", networkRouterName).GetExternalIDs(),
		},
		&nbdb.LogicalRouterPort{
			UUID:        stalePortUUID,
			Name:        "lrp-stale-net1",
			ExternalIDs: buildLRPortDBIDs(staleCNCName, "1", "0", networkRouterName).GetExternalIDs(),
		},
		&nbdb.LogicalRouterPolicy{
			UUID:        validPolicyUUID,
			Priority:    100,
			Match:       "ip4.src == 10.1.0.0/16",
			Action:      nbdb.LogicalRouterPolicyActionReroute,
			ExternalIDs: buildLRPolicyDBIDs(validCNCName, "1", "2", "ip4", networkRouterName).GetExternalIDs(),
		},
		&nbdb.LogicalRouterPolicy{
			UUID:        stalePolicyUUID,
			Priority:    100,
			Match:       "ip4.src == 10.2.0.0/16",
			Action:      nbdb.LogicalRouterPolicyActionReroute,
			ExternalIDs: buildLRPolicyDBIDs(staleCNCName, "1", "2", "ip4", networkRouterName).GetExternalIDs(),
		},
		&nbdb.LoadBalancerGroup{
			UUID: validLBGUUID,
			Name: validLBGName,
		},
		&nbdb.LoadBalancerGroup{
			UUID: staleLBGUUID,
			Name: staleLBGName,
		},
		&nbdb.LogicalSwitch{
			UUID:              "switch-uuid",
			Name:              "test-switch",
			LoadBalancerGroup: []string{validLBGUUID, staleLBGUUID},
		},
	}

	c, cleanup := setupTestController(t, initialDB, validCNCName)
	defer cleanup()

	err := c.repairStaleCNCs()
	require.NoError(t, err)

	netRouter, err := libovsdbops.GetLogicalRouter(c.nbClient, &nbdb.LogicalRouter{Name: networkRouterName})
	require.NoError(t, err)
	assert.Len(t, netRouter.Ports, 1, "only valid port should remain")
	assert.Len(t, netRouter.Policies, 1, "only valid policy should remain")

	_, err = libovsdbops.GetLogicalRouter(c.nbClient, &nbdb.LogicalRouter{Name: getConnectRouterName(validCNCName)})
	require.NoError(t, err, "valid CNC connect router should survive")

	_, err = libovsdbops.GetLogicalRouter(c.nbClient, &nbdb.LogicalRouter{Name: getConnectRouterName(staleCNCName)})
	require.Error(t, err, "stale CNC connect router should be deleted")

	lbgs, err := libovsdbops.FindLoadBalancerGroupsWithPredicate(c.nbClient, func(lbg *nbdb.LoadBalancerGroup) bool {
		return strings.HasPrefix(lbg.Name, ovntypes.NetworkConnectServiceLBGroupPrefix)
	})
	require.NoError(t, err)
	assert.Len(t, lbgs, 1, "only valid LBG should remain")
	assert.Equal(t, validLBGName, lbgs[0].Name)

	sw, err := libovsdbops.FindLogicalSwitchesWithPredicate(c.nbClient, func(sw *nbdb.LogicalSwitch) bool {
		return sw.Name == "test-switch"
	})
	require.NoError(t, err)
	require.Len(t, sw, 1)
	assert.Len(t, sw[0].LoadBalancerGroup, 1, "switch should only reference valid LBG")
}

func TestRepairStaleCNCs_NoCNCs(t *testing.T) {
	// No CNCs in API at all — all CNC-owned objects should be cleaned up.
	staleCNCName := "stale-cnc"

	initialDB := []libovsdbtest.TestData{
		&nbdb.LogicalRouter{
			UUID:        "stale-router-uuid",
			Name:        getConnectRouterName(staleCNCName),
			ExternalIDs: buildLRExternalIDs(staleCNCName),
		},
	}

	c, cleanup := setupTestController(t, initialDB)
	defer cleanup()

	err := c.repairStaleCNCs()
	require.NoError(t, err)

	routers, err := libovsdbops.FindLogicalRoutersWithPredicate(c.nbClient, func(lr *nbdb.LogicalRouter) bool {
		return lr.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.ClusterNetworkConnectOwnerType)
	})
	require.NoError(t, err)
	assert.Empty(t, routers, "all stale routers should be deleted when no CNCs exist")
}

func TestRepairStaleCNCs_EmptyDB(t *testing.T) {
	// Empty OVN DB — nothing to clean, should succeed quickly.
	c, cleanup := setupTestController(t, nil, "cnc1")
	defer cleanup()

	err := c.repairStaleCNCs()
	require.NoError(t, err, "repair should succeed with empty DB")
}
