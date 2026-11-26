// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"fmt"
	"net/netip"
	"slices"

	"github.com/vishvananda/netlink"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/egressip"
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
	// A peer's old rule can still steer traffic after a selector change.
	// Hold the claim lock across the check and installation.
	if rule.Priority == ruleTrafficPriority {
		destination := ruleDestination(rule)
		for applied, peer := range c.selectiveRules[sourceOfRule(rule)] {
			if peer == owner || !destination.Overlaps(ruleDestination(applied)) {
				continue
			}
			if c.ruleWaiters[applied] == nil {
				c.ruleWaiters[applied] = sets.New[string]()
			}
			c.ruleWaiters[applied].Insert(owner)
			return fmt.Errorf("EgressIP %s is waiting for %s to withdraw overlapping IP rule %s", owner, peer, applied.String())
		}
	}
	if err := c.ruleManager.Add(rule); err != nil {
		return fmt.Errorf("add IP rule for EgressIP %s: %w", owner, err)
	}
	if c.ruleOwners[owner] == nil {
		c.ruleOwners[owner] = sets.New[iprulemanager.IPRule]()
	}
	c.ruleOwners[owner].Insert(rule)
	c.indexSelectiveRule(owner, rule)
	return nil
}

type ruleSource struct {
	family int
	prefix netip.Prefix
}

func sourceOfRule(rule iprulemanager.IPRule) ruleSource {
	return ruleSource{family: rule.Family, prefix: rule.Src}
}

// Caller holds ruleOwnersMutex or is restoring claims before workers start.
func (c *Controller) indexSelectiveRule(owner string, rule iprulemanager.IPRule) {
	if rule.Priority != ruleTrafficPriority {
		return
	}
	source := sourceOfRule(rule)
	if c.selectiveRules[source] == nil {
		c.selectiveRules[source] = map[iprulemanager.IPRule]string{}
	}
	c.selectiveRules[source][rule] = owner
}

func ruleDestination(rule iprulemanager.IPRule) egressip.Destination {
	dst := rule.Dst
	if !dst.IsValid() {
		// Linux represents a wildcard destination as an unset prefix.
		dst = netip.PrefixFrom(rule.Src.Addr(), 0).Masked()
	}
	return egressip.Destination{
		CIDR: dst, Protocol: egressip.Protocol(rule.IPProto),
		SourcePort:      egressip.PortRange{Start: rule.Sport.Start, End: rule.Sport.End},
		DestinationPort: egressip.PortRange{Start: rule.Dport.Start, End: rule.Dport.End},
	}
}

func (c *Controller) removeRuleWaiter(owner string) {
	c.ruleOwnersMutex.Lock()
	defer c.ruleOwnersMutex.Unlock()
	for rule, waiters := range c.ruleWaiters {
		waiters.Delete(owner)
		if len(waiters) == 0 {
			delete(c.ruleWaiters, rule)
		}
	}
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
		remove := c.ruleManager.Delete
		if rule.Priority == ruleTrafficPriority {
			// The owner coordinates selective steering with its SNAT state.
			remove = c.ruleManager.DeleteWithoutRetry
		}
		if err := remove(rule); err != nil {
			return fmt.Errorf("delete IP rule for EgressIP %s: %w", owner, err)
		}
		for waiter := range c.ruleWaiters[rule] {
			c.eIPQueue.Add(waiter)
		}
		delete(c.ruleWaiters, rule)
		if rule.Priority == ruleTrafficPriority {
			source := sourceOfRule(rule)
			delete(c.selectiveRules[source], rule)
			if len(c.selectiveRules[source]) == 0 {
				delete(c.selectiveRules, source)
			}
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
	var current []netlink.Route
	if c.hasDifferentRouteClaim(route) {
		var err error
		current, err = c.currentUplinkRoutes(route)
		if err != nil {
			return err
		}
		if !containsRoute(current, route) {
			return fmt.Errorf("uplink route changed before applying EgressIP route %s", route.String())
		}
	}
	if err := c.routeManager.Add(route); err != nil {
		return fmt.Errorf("add route for EgressIP %s: %w", owner, err)
	}
	c.recordOwnedRoute(owner, route)
	if current == nil {
		return nil
	}

	// Owners of this destination share new routes, including alternate metrics.
	// A peer may not reconcile before the owner that discovered a route retires.
	var obsolete []netlink.Route
	for peer, routes := range c.routeOwners {
		if slices.ContainsFunc(routes, func(claimed netlink.Route) bool { return sameRouteDestination(claimed, route) }) {
			c.recordOwnedRoute(peer, route)
		}
		for _, claimed := range routes {
			if sameRouteDestination(claimed, route) &&
				!slices.ContainsFunc(current, func(candidate netlink.Route) bool { return routemanager.SameKey(candidate, claimed) }) &&
				!slices.ContainsFunc(obsolete, func(candidate netlink.Route) bool { return routemanager.SameKey(candidate, claimed) }) {
				obsolete = append(obsolete, claimed)
			}
		}
	}
	for _, stale := range obsolete {
		if err := c.deleteSharedRouteLocked(stale); err != nil {
			return err
		}
	}
	return nil
}

func sameRouteDestination(a, b netlink.Route) bool {
	a.Priority = b.Priority
	return routemanager.SameKey(a, b)
}

func (c *Controller) hasDifferentRouteClaim(route netlink.Route) bool {
	for _, routes := range c.routeOwners {
		for _, claimed := range routes {
			if sameRouteDestination(claimed, route) && !util.RouteEqual(&claimed, &route) {
				return true
			}
		}
	}
	return false
}

// Read under the claim lock so a stale worker cannot overwrite a peer's newer route.
func (c *Controller) currentUplinkRoutes(route netlink.Route) ([]netlink.Route, error) {
	link, err := util.GetNetLinkOps().LinkByIndex(route.LinkIndex)
	if err != nil {
		return nil, fmt.Errorf("get uplink for EgressIP route %s: %w", route.String(), err)
	}
	isV6 := route.Family == netlink.FAMILY_V6 || route.Dst != nil && route.Dst.IP.To4() == nil
	var managedAddresses sets.Set[string]
	if isV6 {
		managedAddresses, err = c.getAnnotation()
		if err != nil {
			return nil, err
		}
	}
	return generateRoutesForLink(link, isV6, managedAddresses)
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

// A live owner still on this uplink can observe a withdrawal before its peers.
func (c *Controller) deleteWithdrawnRoute(owner string, route netlink.Route) error {
	c.routeOwnersMutex.Lock()
	defer c.routeOwnersMutex.Unlock()
	current, err := c.currentUplinkRoutes(route)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(current, func(candidate netlink.Route) bool { return sameRouteDestination(candidate, route) }) {
		return fmt.Errorf("uplink route changed before removing EgressIP route %s", route.String())
	}
	withdrawn := []netlink.Route{route}
	for _, routes := range c.routeOwners {
		for _, claimed := range routes {
			if sameRouteDestination(claimed, route) &&
				!slices.ContainsFunc(withdrawn, func(other netlink.Route) bool { return routemanager.SameKey(other, claimed) }) {
				withdrawn = append(withdrawn, claimed)
			}
		}
	}
	for _, withdrawnRoute := range withdrawn {
		if err := c.deleteSharedRouteLocked(withdrawnRoute); err != nil {
			return fmt.Errorf("delete withdrawn uplink route for EgressIP %s: %w", owner, err)
		}
	}
	return nil
}

// Caller holds routeOwnersMutex. Drop every claim only after removing the route.
func (c *Controller) deleteSharedRouteLocked(route netlink.Route) error {
	if err := c.routeManager.Del(route); err != nil {
		return fmt.Errorf("delete obsolete uplink route %s: %w", route.String(), err)
	}
	for peer := range c.routeOwners {
		c.forgetRouteClaim(peer, route)
	}
	return nil
}

func (c *Controller) deleteOwnedRoute(owner string, route netlink.Route) error {
	c.routeOwnersMutex.Lock()
	defer c.routeOwnersMutex.Unlock()
	return c.deleteOwnedRouteLocked(owner, route)
}

// Caller holds routeOwnersMutex so metric replacement cannot move the claim mid-delete.
func (c *Controller) deleteOwnedRouteLocked(owner string, route netlink.Route) error {
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
			return fmt.Errorf("delete route for EgressIP %s: %w", owner, err)
		}
	}
	c.forgetRouteClaim(owner, route)
	return nil
}

// Caller holds routeOwnersMutex.
func (c *Controller) forgetRouteClaim(owner string, route netlink.Route) {
	c.routeOwners[owner] = slices.DeleteFunc(c.routeOwners[owner], func(other netlink.Route) bool {
		return routemanager.SameKey(other, route)
	})
	if len(c.routeOwners[owner]) == 0 {
		delete(c.routeOwners, owner)
	}
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
	defer c.routeOwnersMutex.Unlock()
	for _, route := range slices.Clone(c.routeOwners[owner]) {
		// The applied snapshot describes the destinations this owner uses.
		// Its route metrics can be stale if a peer refreshed the shared table.
		if slices.ContainsFunc(wantedRoutes, func(wanted netlink.Route) bool {
			return sameRouteDestination(wanted, route)
		}) {
			continue
		}
		if err := c.deleteOwnedRouteLocked(owner, route); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.Join(errs...)
}
