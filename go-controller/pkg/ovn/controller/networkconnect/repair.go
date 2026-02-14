package networkconnect

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// repairStaleCNCs removes OVN objects (ACLs, LBGs, ports, policies, connect routers)
// that belong to CNCs no longer in the API. Called once at startup before workers run.
// Cleanup is done per object type: for each type we find objects that don't belong to validCNCs and delete them.
func (c *Controller) repairStaleCNCs() error {
	start := time.Now()
	defer func() {
		klog.Infof("Repairing stale ClusterNetworkConnect OVN state took %v", time.Since(start))
	}()

	cncs, err := c.cncLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list CNCs for repair: %w", err)
	}
	validCNCs := sets.New[string]()
	for _, cnc := range cncs {
		validCNCs.Insert(cnc.Name)
	}

	// Clean up each object type: find our objects whose CNC name is not in validCNCs and delete them.
	// Order matters: ACLs and LBGs (remove from switches first), then network connectivity (ports + policies), then routers.
	if err := c.cleanupStaleACLs(validCNCs); err != nil {
		return fmt.Errorf("failed to cleanup stale ACLs: %w", err)
	}
	if err := c.cleanupStaleLBGs(validCNCs); err != nil {
		return fmt.Errorf("failed to cleanup stale LBGs: %w", err)
	}
	if err := c.cleanupStalePortsPolicies(validCNCs); err != nil {
		return fmt.Errorf("failed to cleanup stale ports, routes, policies, routers: %w", err)
	}
	if err := c.cleanupStaleRouters(validCNCs); err != nil {
		return fmt.Errorf("failed to cleanup stale connect routers: %w", err)
	}

	return nil
}

// cleanupStaleACLs finds CNC names that have partial connectivity ACLs but are not in validCNCs,
// and calls cleanupPartialConnectivity for each (removes ACLs from switches and destroys address sets).
func (c *Controller) cleanupStaleACLs(validCNCs sets.Set[string]) error {
	aclPredicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLClusterNetworkConnect, controllerName, nil)
	aclPredicate := libovsdbops.GetPredicate[*nbdb.ACL](aclPredicateIDs, nil)
	acls, err := libovsdbops.FindACLsWithPredicate(c.nbClient, aclPredicate)
	if err != nil {
		return err
	}
	staleCNCNames := sets.New[string]()
	for _, acl := range acls {
		cncName := acl.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		if cncName != "" && !validCNCs.Has(cncName) {
			staleCNCNames.Insert(cncName)
		}
	}
	for _, cncName := range staleCNCNames.UnsortedList() {
		klog.V(4).Infof("Cleaning up partial connectivity for stale CNC %s", cncName)
		if err := c.cleanupPartialConnectivity(cncName); err != nil {
			klog.Warningf("Failed to cleanup partial connectivity for stale CNC %s: %v", cncName, err)
		}
	}
	return nil
}

// cleanupStaleLBGs finds CNC LoadBalancerGroups (name prefix cnc_svc_) whose cncName is not in validCNCs
// and calls cleanupServiceConnectivity to remove them from all referencing switches and delete them.
func (c *Controller) cleanupStaleLBGs(validCNCs sets.Set[string]) error {
	lbgPrefix := ovntypes.NetworkConnectServiceLBGroupPrefix
	lbgs, err := libovsdbops.FindLoadBalancerGroupsWithPredicate(c.nbClient, func(lbg *nbdb.LoadBalancerGroup) bool {
		return strings.HasPrefix(lbg.Name, lbgPrefix)
	})
	if err != nil {
		return err
	}
	staleCNCNames := sets.New[string]()
	for _, lbg := range lbgs {
		if len(lbg.Name) > len(lbgPrefix) {
			cncName := lbg.Name[len(lbgPrefix):]
			if !validCNCs.Has(cncName) {
				staleCNCNames.Insert(cncName)
			}
		}
	}
	for _, cncName := range staleCNCNames.UnsortedList() {
		klog.V(4).Infof("Cleaning up service connections for stale CNC %s", cncName)
		if err := c.cleanupServiceConnectivity(cncName); err != nil {
			klog.Warningf("Failed to cleanup service connections for stale CNC %s: %v", cncName, err)
		}
	}
	return nil
}

// cleanupStalePortsPolicies finds CNC names that own router ports or routing policies
// but are not in validCNCs, and calls cleanupNetworkConnectivity for each (removes ports
// and policies from network routers). Both ports and policies are scanned for discovery
// to handle the case where one type was already cleaned up in a prior partial run.
func (c *Controller) cleanupStalePortsPolicies(validCNCs sets.Set[string]) error {
	staleCNCNames := sets.New[string]()

	// Discover stale CNC names from router ports
	portPredicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPortClusterNetworkConnect, controllerName, nil)
	portPredicate := libovsdbops.GetPredicate[*nbdb.LogicalRouterPort](portPredicateIDs, nil)
	ports, err := libovsdbops.FindLogicalRouterPortWithPredicate(c.nbClient, portPredicate)
	if err != nil {
		return err
	}
	for _, port := range ports {
		cncName := port.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		if cncName != "" && !validCNCs.Has(cncName) {
			staleCNCNames.Insert(cncName)
		}
	}

	// Also discover stale CNC names from routing policies (in case ports were already cleaned up)
	policyPredicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyClusterNetworkConnect, controllerName, nil)
	policyPredicate := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](policyPredicateIDs, nil)
	policies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(c.nbClient, policyPredicate)
	if err != nil {
		return err
	}
	for _, policy := range policies {
		cncName := policy.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		if cncName != "" && !validCNCs.Has(cncName) {
			staleCNCNames.Insert(cncName)
		}
	}

	for _, cncName := range staleCNCNames.UnsortedList() {
		klog.V(4).Infof("Cleaning up network connectivity for stale CNC %s", cncName)
		if err := c.cleanupNetworkConnectivity(cncName); err != nil {
			klog.Warningf("Failed to cleanup network connectivity for stale CNC %s: %v", cncName, err)
		}
	}
	return nil
}

// cleanupStaleRouters finds connect routers whose ObjectNameKey is not in validCNCs
// and calls deleteConnectRouter to delete them.
func (c *Controller) cleanupStaleRouters(validCNCs sets.Set[string]) error {
	routers, err := libovsdbops.FindLogicalRoutersWithPredicate(c.nbClient, func(lr *nbdb.LogicalRouter) bool {
		return lr.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.ClusterNetworkConnectOwnerType)
	})
	if err != nil {
		return err
	}
	for _, lr := range routers {
		cncName := lr.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		if cncName == "" || validCNCs.Has(cncName) {
			continue
		}
		klog.Infof("Removing stale connect router %s (CNC %s)", lr.Name, cncName)
		if err := c.deleteConnectRouter(cncName); err != nil {
			return fmt.Errorf("failed to delete stale connect router for CNC %s: %w", cncName, err)
		}
	}
	return nil
}
