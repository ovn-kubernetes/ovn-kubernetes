// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"

	"k8s.io/apimachinery/pkg/util/sets"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func (e *EgressIPController) migrateEgressIPReroutePriorities(include egressIPNetworkFilter, matches map[string]egressTrafficMatches) error {
	routers, err := libovsdbops.FindLogicalRoutersWithPredicate(e.nbClient, func(*nbdb.LogicalRouter) bool {
		return true
	})
	if err != nil {
		return fmt.Errorf("list routers for EgressIP priority migration: %w", err)
	}
	var ops []ovsdb.Operation
	for _, router := range routers {
		ops, err = e.migrateRouterEgressIPPoliciesOps(ops, router.Name, include, matches)
		if err != nil {
			return fmt.Errorf("migrate EgressIP priorities on router %s: %w", router.Name, err)
		}
	}
	if include(types.DefaultNetworkName) {
		ops, err = e.migrateEgressIPNATMatchesOps(ops, matches)
		if err != nil {
			return err
		}
	}
	// Commit reroute and NAT matches together so a widened reroute always has matching SNAT.
	if _, err := libovsdbops.TransactAndCheck(e.nbClient, ops); err != nil {
		return fmt.Errorf("transact EgressIP priority and match migration: %w", err)
	}
	return nil
}

// Merge duplicate old/new-priority policies before updating the identity so
// existing nexthops are preserved.
func (e *EgressIPController) migrateRouterEgressIPPoliciesOps(ops []ovsdb.Operation, routerName string, include egressIPNetworkFilter, matches map[string]egressTrafficMatches) ([]ovsdb.Operation, error) {
	policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(e.nbClient, routerName, func(policy *nbdb.LogicalRouterPolicy) bool {
		// Gateway marks are rebuilt after stale owners are removed by per-pod eligibility.
		return isEgressIPReroutePriority(policy.Priority) && policy.Action == nbdb.LogicalRouterPolicyActionReroute &&
			policy.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == e.controllerName &&
			policy.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.EgressIPOwnerType) &&
			include(policy.ExternalIDs[libovsdbops.NetworkKey.String()])
	})
	if errors.Is(err, libovsdbclient.ErrNotFound) {
		return ops, nil // The router can disappear after the initial list.
	}
	if err != nil {
		return nil, fmt.Errorf("list EgressIP policies: %w", err)
	}
	groups := map[string]*nbdb.LogicalRouterPolicy{}
	originals := make(map[string]*nbdb.LogicalRouterPolicy, len(policies))
	for _, policy := range policies {
		name, _ := getEIPLRPObjK8MetaData(policy.ExternalIDs)
		traffic, exists := matches[name]
		ip := egressPodIPFromMatch(policy.Match)
		if !exists || ip == nil {
			ops, err = libovsdbops.DeleteLogicalRouterPoliciesOps(e.nbClient, ops, routerName, policy)
			if err != nil {
				return nil, fmt.Errorf("delete obsolete policy %s: %w", policy.UUID, err)
			}
			continue
		}
		trafficMatch := traffic.match(ip.To4() == nil)
		priority := egressIPReroutePriority(trafficMatch)
		ids, idErr := libovsdbops.NewDbObjectIDsFromExternalIDs(libovsdbops.LogicalRouterPolicyEgressIP, policy.ExternalIDs)
		if idErr != nil {
			return nil, fmt.Errorf("decode ownership of policy %s: %w", policy.UUID, idErr)
		}
		ids = ids.AddIDs(map[libovsdbops.ExternalIDKey]string{libovsdbops.PriorityKey: fmt.Sprint(priority)})
		key := ids.String() + "/" + ip.String()
		if kept := groups[key]; kept != nil {
			hops := sets.New[string](kept.Nexthops...)
			hops.Insert(policy.Nexthops...)
			kept.Nexthops = sets.List(hops)
			ops, err = libovsdbops.DeleteLogicalRouterPoliciesOps(e.nbClient, ops, routerName, policy)
			if err != nil {
				return nil, fmt.Errorf("delete duplicate policy %s: %w", policy.UUID, err)
			}
			continue
		}
		updated := *policy
		updated.Priority = priority
		updated.ExternalIDs = ids.GetExternalIDs()
		updated.Match = egressIPPolicyMatch(ip, trafficMatch, false)
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
			return nil, fmt.Errorf("update migrated policy %s: %w", policy.UUID, err)
		}
	}
	return ops, nil
}

func (e *EgressIPController) migrateEgressIPNATMatchesOps(ops []ovsdb.Operation, matches map[string]egressTrafficMatches) ([]ovsdb.Operation, error) {
	nats, err := libovsdbops.FindNATsWithPredicate(e.nbClient, func(nat *nbdb.NAT) bool {
		return nat.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == e.controllerName &&
			nat.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.EgressIPOwnerType)
	})
	if err != nil {
		return nil, fmt.Errorf("list EgressIP NATs for migration: %w", err)
	}
	for _, nat := range nats {
		name, _ := getEIPLRPObjK8MetaData(nat.ExternalIDs)
		traffic, exists := matches[name]
		if !exists {
			continue
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
			return nil, fmt.Errorf("update migrated NAT %s: %w", nat.UUID, err)
		}
	}
	return ops, nil
}
