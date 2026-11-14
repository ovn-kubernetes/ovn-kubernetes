// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"fmt"
	"slices"

	"github.com/vishvananda/netlink"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

// Publish applied SNAT state even if pruning startup claims fails.
func (c *Controller) syncOwnedResources(owner string, applied *state) error {
	pruneErr := c.pruneOwnedResources(owner, applied)
	snatErr := c.syncSelectiveSNAT(owner, applied)
	return utilerrors.Join(pruneErr, snatErr)
}

// Claim changes and manager operations share a lock so a last-owner deletion
// cannot race with another owner acquiring the same resource.
func (c *Controller) addOwnedIPRule(owner string, rule iprulemanager.IPRule) error {
	c.ruleOwnersMutex.Lock()
	defer c.ruleOwnersMutex.Unlock()
	if err := c.ruleManager.Add(rule); err != nil {
		return fmt.Errorf("add IP rule for EgressIP %s: %w", owner, err)
	}
	if c.ruleOwners[owner] == nil {
		c.ruleOwners[owner] = sets.New[iprulemanager.IPRule]()
	}
	c.ruleOwners[owner].Insert(rule)
	return nil
}

func (c *Controller) deleteOwnedIPRule(owner string, rule iprulemanager.IPRule) error {
	c.ruleOwnersMutex.Lock()
	defer c.ruleOwnersMutex.Unlock()
	shared := false
	for peer, rules := range c.ruleOwners {
		if peer != owner && rules.Has(rule) {
			shared = true
			break
		}
	}
	if !shared {
		if err := c.ruleManager.Delete(rule); err != nil {
			return err
		}
	}
	c.ruleOwners[owner].Delete(rule)
	if len(c.ruleOwners[owner]) == 0 {
		delete(c.ruleOwners, owner)
	}
	return nil
}

func (c *Controller) addOwnedRoute(owner string, route netlink.Route) error {
	c.routeOwnersMutex.Lock()
	defer c.routeOwnersMutex.Unlock()
	if err := c.routeManager.Add(route); err != nil {
		return fmt.Errorf("add route for EgressIP %s: %w", owner, err)
	}
	c.recordOwnedRoute(owner, route)
	return nil
}

// Caller holds routeOwnersMutex, or is repairing the node before workers start.
func (c *Controller) recordOwnedRoute(owner string, route netlink.Route) {
	for i, existing := range c.routeOwners[owner] {
		if routemanager.SameKey(existing, route) {
			c.routeOwners[owner][i] = route
			return
		}
	}
	c.routeOwners[owner] = append(c.routeOwners[owner], route)
}

func (c *Controller) deleteOwnedRoute(owner string, route netlink.Route) error {
	c.routeOwnersMutex.Lock()
	defer c.routeOwnersMutex.Unlock()
	shared := false
	for peer, routes := range c.routeOwners {
		if peer != owner && slices.ContainsFunc(routes, func(other netlink.Route) bool {
			return routemanager.SameKey(other, route)
		}) {
			shared = true
			break
		}
	}
	if !shared {
		if err := c.routeManager.Del(route); err != nil {
			return err
		}
	}
	c.routeOwners[owner] = slices.DeleteFunc(c.routeOwners[owner], func(other netlink.Route) bool {
		return routemanager.SameKey(other, route)
	})
	if len(c.routeOwners[owner]) == 0 {
		delete(c.routeOwners, owner)
	}
	return nil
}

// A first reconciliation may replace claims seeded by startup repair without
// having an applied cache entry. Keep only claims represented by its new state.
// Caller holds the owner's cache lock.
func (c *Controller) pruneOwnedResources(owner string, applied *state) error {
	wantedRules := sets.New[iprulemanager.IPRule]()
	var wantedRoutes []netlink.Route
	if applied != nil {
		for _, pods := range applied.namespacesWithPodIPConfigs {
			for _, configs := range pods {
				for _, config := range configs.elems {
					wantedRules.Insert(config.ipRule)
				}
			}
		}
		wantedRoutes = applied.eIPConfig.routes
	}
	c.ruleOwnersMutex.Lock()
	obsoleteRules := c.ruleOwners[owner].Difference(wantedRules)
	c.ruleOwnersMutex.Unlock()
	var errs []error
	for rule := range obsoleteRules {
		if err := c.deleteOwnedIPRule(owner, rule); err != nil {
			errs = append(errs, err)
		}
	}
	c.routeOwnersMutex.Lock()
	obsoleteRoutes := slices.Clone(c.routeOwners[owner])
	c.routeOwnersMutex.Unlock()
	for _, route := range obsoleteRoutes {
		if slices.ContainsFunc(wantedRoutes, func(wanted netlink.Route) bool {
			return routemanager.SameKey(wanted, route)
		}) {
			continue
		}
		if err := c.deleteOwnedRoute(owner, route); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.Join(errs...)
}
