// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"maps"
	"net"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/egressip"
)

func (e *EgressIPController) migrateEgressIPReroutePriorities(include egressIPNetworkFilter) error {
	routers, err := libovsdbops.FindLogicalRoutersWithPredicate(e.nbClient, func(*nbdb.LogicalRouter) bool {
		return true
	})
	if err != nil {
		return fmt.Errorf("list routers for EgressIP priority migration: %w", err)
	}
	for _, router := range routers {
		if err := e.migrateRouterEgressIPPolicies(router.Name, include); err != nil {
			return fmt.Errorf("migrate EgressIP priorities on router %s: %w", router.Name, err)
		}
	}
	if !include(types.DefaultNetworkName) {
		return nil
	}
	return e.migrateEgressIPNATMatches()
}

// Migrate each router atomically, merging duplicate old/new-priority policies
// before updating their identity so existing nexthops are preserved.
func (e *EgressIPController) migrateRouterEgressIPPolicies(routerName string, include egressIPNetworkFilter) error {
	policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(e.nbClient, routerName, func(policy *nbdb.LogicalRouterPolicy) bool {
		return ((isEgressIPReroutePriority(policy.Priority) && policy.Action == nbdb.LogicalRouterPolicyActionReroute) ||
			(isEgressIPMarkPriority(policy.Priority) && policy.Action == nbdb.LogicalRouterPolicyActionAllow)) &&
			policy.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == e.controllerName &&
			policy.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.EgressIPOwnerType) &&
			include(policy.ExternalIDs[libovsdbops.NetworkKey.String()])
	})
	if err != nil {
		return err
	}
	groups := map[string]*nbdb.LogicalRouterPolicy{}
	originals := make(map[string]*nbdb.LogicalRouterPolicy, len(policies))
	var ops []ovsdb.Operation
	for _, policy := range policies {
		name, _ := getEIPLRPObjK8MetaData(policy.ExternalIDs)
		owner, lookupErr := e.watchFactory.GetEgressIP(name)
		if lookupErr != nil && !apierrors.IsNotFound(lookupErr) {
			return lookupErr
		}
		ip := egressPodIPFromMatch(policy.Match)
		if apierrors.IsNotFound(lookupErr) || ip == nil || (egressip.HasTrafficSelector(owner.Spec) && !config.OVNKubernetesFeature.EnableTrafficClass) {
			ops, err = libovsdbops.DeleteLogicalRouterPoliciesOps(e.nbClient, ops, routerName, policy)
			if err != nil {
				return err
			}
			continue
		}
		traffic, matchErr := e.egressTrafficMatchesFor(name)
		if matchErr != nil {
			return matchErr
		}
		trafficMatch := traffic.match(ip.To4() == nil)
		markPolicy := isEgressIPMarkPriority(policy.Priority)
		priority := egressIPReroutePriority(trafficMatch)
		if markPolicy {
			priority = egressIPMarkPriority(trafficMatch)
		}
		ids, idErr := libovsdbops.NewDbObjectIDsFromExternalIDs(libovsdbops.LogicalRouterPolicyEgressIP, policy.ExternalIDs)
		if idErr != nil {
			return idErr
		}
		ids = ids.AddIDs(map[libovsdbops.ExternalIDKey]string{libovsdbops.PriorityKey: fmt.Sprint(priority)})
		key := ids.String() + "/" + ip.String()
		if kept := groups[key]; kept != nil {
			hops := sets.New[string](kept.Nexthops...)
			hops.Insert(policy.Nexthops...)
			kept.Nexthops = sets.List(hops)
			ops, err = libovsdbops.DeleteLogicalRouterPoliciesOps(e.nbClient, ops, routerName, policy)
			if err != nil {
				return err
			}
			continue
		}
		updated := *policy
		updated.Priority = priority
		updated.ExternalIDs = ids.GetExternalIDs()
		updated.Match = egressIPPolicyMatch(ip, trafficMatch, markPolicy)
		groups[key] = &updated
		originals[updated.UUID] = policy
	}
	for _, policy := range groups {
		original := originals[policy.UUID]
		if policy.Priority == original.Priority && policy.Match == original.Match &&
			maps.Equal(policy.ExternalIDs, original.ExternalIDs) && slices.Equal(policy.Nexthops, original.Nexthops) {
			continue
		}
		ops, err = libovsdbops.UpdateLogicalRouterPoliciesOps(e.nbClient, ops, policy)
		if err != nil {
			return err
		}
	}
	if _, err = libovsdbops.TransactAndCheck(e.nbClient, ops); err != nil {
		return err
	}
	return nil
}

func (e *EgressIPController) migrateEgressIPNATMatches() error {
	nats, err := libovsdbops.FindNATsWithPredicate(e.nbClient, func(nat *nbdb.NAT) bool {
		return nat.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == e.controllerName &&
			nat.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.EgressIPOwnerType)
	})
	if err != nil {
		return err
	}
	var ops []ovsdb.Operation
	for _, nat := range nats {
		name, _ := getEIPLRPObjK8MetaData(nat.ExternalIDs)
		owner, lookupErr := e.watchFactory.GetEgressIP(name)
		if apierrors.IsNotFound(lookupErr) {
			continue
		}
		if lookupErr != nil {
			return lookupErr
		}
		if egressip.HasTrafficSelector(owner.Spec) && !config.OVNKubernetesFeature.EnableTrafficClass {
			continue
		}
		traffic, matchErr := e.egressTrafficMatchesFor(name)
		if matchErr != nil {
			return matchErr
		}
		match := traffic.match(net.ParseIP(nat.LogicalIP).To4() == nil)
		updated := *nat
		updated.Match = match
		updated.Priority = 0
		if match != "" {
			updated.Priority = types.EgressIPSelectiveNATPriority
		}
		if updated.Match == nat.Match && updated.Priority == nat.Priority {
			continue
		}
		ops, err = libovsdbops.UpdateNATOps(e.nbClient, ops, &updated)
		if err != nil {
			return err
		}
	}
	_, err = libovsdbops.TransactAndCheck(e.nbClient, ops)
	return err
}
