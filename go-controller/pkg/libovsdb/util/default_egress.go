// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"fmt"
	"net"
	"strconv"

	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func defaultEgressPolicyIDs(router, chain, match string, priority int) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyDefaultEgress, types.DefaultEgressControllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.RouterNameKey: router,
			libovsdbops.ChainKey:      chain,
			libovsdbops.PriorityKey:   strconv.Itoa(priority),
			libovsdbops.MatchKey:      match,
		})
}

func defaultEgressRouteIDs(router, prefix string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterStaticRouteDefaultEgress, types.DefaultEgressControllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.RouterNameKey: router,
			libovsdbops.CIDRKey:       libovsdbops.BuildCIDRKey(prefix),
		})
}

func defaultEgressOwnerIDs(objType *libovsdbops.ObjectIDsType, router string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(objType, types.DefaultEgressControllerName,
		map[libovsdbops.ExternalIDKey]string{libovsdbops.RouterNameKey: router})
}

func defaultEgressPolicy(router, chain, match string, priority int, action string) *nbdb.LogicalRouterPolicy {
	p := &nbdb.LogicalRouterPolicy{
		Match: match, Priority: priority, Action: action,
		ExternalIDs: defaultEgressPolicyIDs(router, chain, match, priority).GetExternalIDs(),
	}
	if chain != "" {
		p.Chain = ptr.To(chain)
	}
	// OVN does not apply pkt_mark on jump actions. Mark the terminal decision.
	if chain == types.DefaultEgressNodeChain {
		p.Options = map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark}
	}
	return p
}

func upsertDefaultEgressPolicyOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, router string, policy *nbdb.LogicalRouterPolicy) ([]ovsdb.Operation, error) {
	// Build this policy independently so a preceding insert's guard cannot
	// suppress its own duplicate check. Gateway reconciliation and feature
	// bootstrap can both create the same terminal before either cache sees it.
	policyOps, err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(nbClient, nil, router, policy, nil,
		&policy.Action, &policy.Nexthops, &policy.JumpChain, &policy.Options, &policy.ExternalIDs)
	if err != nil {
		return nil, fmt.Errorf("upserting default egress policy on router %q (chain %q, priority %d, match %q): %w",
			router, ptr.Deref(policy.Chain, ""), policy.Priority, policy.Match, err)
	}
	return append(ops, policyOps...), nil
}

// SetDefaultEgressNoReroutePolicy redirects an exemption to ordinary routing,
// bypassing EgressIP and EgressService while preserving the mode's default path.
func SetDefaultEgressNoReroutePolicy(policy *nbdb.LogicalRouterPolicy, netInfo util.NetInfo, chain string) {
	if util.UsesOverlayEgressPolicyChains(netInfo) {
		policy.Action = nbdb.LogicalRouterPolicyActionJump
		policy.JumpChain = ptr.To(chain)
		policy.Options = nil
	} else if policy.Action == nbdb.LogicalRouterPolicyActionJump &&
		(ptr.Deref(policy.JumpChain, "") == types.DefaultEgressChain ||
			ptr.Deref(policy.JumpChain, "") == types.DefaultEgressNodeChain) {
		policy.Action = nbdb.LogicalRouterPolicyActionAllow
		policy.JumpChain = nil
		if chain == types.DefaultEgressNodeChain {
			policy.Options = map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark}
		}
	}
}

// EnsureDefaultEgressChainOps initializes a chain before an exemption jumps to it.
// Until gateway reconciliation populates the chain, the terminal allow preserves
// the old static routes. Call once per chain in a transaction, including dual stack.
func EnsureDefaultEgressChainOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, router string,
	netInfo util.NetInfo, chain string) ([]ovsdb.Operation, error) {
	if !util.UsesOverlayEgressPolicyChains(netInfo) {
		return ops, nil
	}
	terminal := defaultEgressPolicy(router, chain, "1", types.DefaultEgressTerminalPriority, nbdb.LogicalRouterPolicyActionAllow)
	return upsertDefaultEgressPolicyOps(nbClient, ops, router, terminal)
}

// ReconcileDefaultEgress installs destination defaults for all Layer 3 transports
// and source-aware policy chains only for the interconnect overlay, removing
// superseded routes in the same transaction. Only the gateway owns this
// fallback; feature controllers own their higher-priority reroute policies.
func ReconcileDefaultEgress(nbClient libovsdbclient.Client, router, nodeName string, netInfo util.NetInfo,
	localSubnets []*net.IPNet, gatewayIPs []net.IP) error {
	var ops []ovsdb.Operation
	var err error
	policies := []*nbdb.LogicalRouterPolicy{}
	wantedDefaults := map[string]bool{}
	for _, localSubnet := range localSubnets {
		ipv6 := utilnet.IsIPv6CIDR(localSubnet)
		defaultPrefix := "0.0.0.0/0"
		if ipv6 {
			defaultPrefix = "::/0"
		}
		wantedDefaults[defaultPrefix] = true
		gatewayIP, matchErr := util.MatchIPFamily(ipv6, gatewayIPs)
		if matchErr != nil {
			return fmt.Errorf("finding default egress gateway for %s: %w", localSubnet, matchErr)
		}
		nextHop := gatewayIP[0]
		if config.Gateway.Mode == config.GatewayModeLocal {
			nextHop = netInfo.GetNodeManagementIP(localSubnet).IP
		}
		route := &nbdb.LogicalRouterStaticRoute{IPPrefix: defaultPrefix, Nexthop: nextHop.String(),
			ExternalIDs: defaultEgressRouteIDs(router, defaultPrefix).GetExternalIDs()}
		if netInfo.IsUserDefinedNetwork() {
			route.ExternalIDs[types.NetworkExternalID] = netInfo.GetNetworkName()
			route.ExternalIDs[types.TopologyExternalID] = netInfo.TopologyType()
		}
		ops, err = libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicateOps(nbClient, ops, router, route,
			func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.IPPrefix == defaultPrefix && libovsdbops.PolicyEqualPredicate(nil, item.Policy)
			}, &route.Nexthop, &route.ExternalIDs)
		if err != nil {
			return err
		}
	}
	if util.UsesOverlayEgressPolicyChains(netInfo) {
		jump := defaultEgressPolicy(router, "", "ip", types.DefaultEgressJumpPriority, nbdb.LogicalRouterPolicyActionJump)
		jump.JumpChain = ptr.To(types.DefaultEgressChain)
		policies = append(policies, jump)

		chains := []string{types.DefaultEgressChain, types.DefaultEgressNodeChain}
		for _, chain := range chains {
			policies = append(policies, defaultEgressPolicy(router, chain, "1", types.DefaultEgressTerminalPriority, nbdb.LogicalRouterPolicyActionAllow))
		}
		for _, ipv6 := range []bool{false, true} {
			if len(util.MatchAllIPNetFamily(ipv6, localSubnets)) == 0 {
				continue
			}
			gatewayIP, matchErr := util.MatchIPFamily(ipv6, gatewayIPs)
			if matchErr != nil {
				return fmt.Errorf("finding %s default egress gateway: %w", util.IPFamilyName(ipv6), matchErr)
			}
			matches := defaultEgressMatches(netInfo, nodeName, ipv6)
			for _, chain := range chains {
				policies = append(policies,
					defaultEgressPolicy(router, chain, matches.internalDestination,
						types.DefaultEgressDestinationPriority, nbdb.LogicalRouterPolicyActionAllow),
					defaultEgressPolicy(router, chain, matches.localIngress,
						types.DefaultEgressLocalPriority, nbdb.LogicalRouterPolicyActionAllow))
				fallback := defaultEgressPolicy(router, chain, matches.clusterSource,
					types.DefaultEgressClusterPriority, nbdb.LogicalRouterPolicyActionReroute)
				fallback.Nexthops = []string{gatewayIP[0].String()}
				policies = append(policies, fallback)
			}
		}
	}
	wanted := map[string]bool{}
	for _, policy := range policies {
		wanted[policy.ExternalIDs[types.PrimaryIDKey]] = true
		ops, err = upsertDefaultEgressPolicyOps(nbClient, ops, router, policy)
		if err != nil {
			return err
		}
	}
	ops, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(nbClient, ops, router, libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](
		defaultEgressOwnerIDs(libovsdbops.LogicalRouterPolicyDefaultEgress, router), func(item *nbdb.LogicalRouterPolicy) bool {
			return !wanted[item.ExternalIDs[types.PrimaryIDKey]]
		}))
	if err != nil {
		return err
	}
	// Convert existing exemptions in the same transaction as the routes. During
	// upgrade the feature controllers may not have reconciled these policies yet.
	exemptions, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(nbClient, router, func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority == types.DefaultNoRereoutePriority &&
			((item.Action == nbdb.LogicalRouterPolicyActionAllow &&
				(item.Match == fmt.Sprintf("pkt.mark == %d", types.EgressIPReplyTrafficConnectionMark) ||
					item.Options["pkt_mark"] == types.EgressIPNodeConnectionMark)) ||
				(item.Action == nbdb.LogicalRouterPolicyActionJump &&
					(ptr.Deref(item.JumpChain, "") == types.DefaultEgressChain ||
						ptr.Deref(item.JumpChain, "") == types.DefaultEgressNodeChain)))
	})
	if err != nil {
		return err
	}
	for _, exemption := range exemptions {
		chain := types.DefaultEgressChain
		if exemption.Options["pkt_mark"] == types.EgressIPNodeConnectionMark ||
			ptr.Deref(exemption.JumpChain, "") == types.DefaultEgressNodeChain {
			chain = types.DefaultEgressNodeChain
		}
		SetDefaultEgressNoReroutePolicy(exemption, netInfo, chain)
		ops, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(nbClient, ops, router, exemption,
			func(item *nbdb.LogicalRouterPolicy) bool { return item.UUID == exemption.UUID },
			&exemption.Action, &exemption.JumpChain, &exemption.Options)
		if err != nil {
			return err
		}
	}
	ownedRoute := libovsdbops.GetPredicate[*nbdb.LogicalRouterStaticRoute](
		defaultEgressOwnerIDs(libovsdbops.LogicalRouterStaticRouteDefaultEgress, router), nil)
	ops, err = libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicateOps(nbClient, ops, router, func(item *nbdb.LogicalRouterStaticRoute) bool {
		// Reverting to single stack must also remove the disabled family's
		// destination default, without touching routes owned by other features.
		if ownedRoute(item) &&
			(item.IPPrefix == "0.0.0.0/0" || item.IPPrefix == "::/0") &&
			libovsdbops.PolicyEqualPredicate(nil, item.Policy) && !wantedDefaults[item.IPPrefix] {
			return true
		}
		if config.Gateway.Mode == config.GatewayModeLocal {
			if _, ok := item.ExternalIDs[types.UDNEnabledServiceExternalID]; ok {
				return true
			}
		}
		if config.Gateway.Mode == config.GatewayModeShared {
			for _, subnet := range localSubnets {
				if item.IPPrefix == subnet.String() && item.Nexthop == netInfo.GetNodeManagementIP(subnet).IP.String() {
					return true
				}
			}
		}
		if item.Policy == nil || *item.Policy != nbdb.LogicalRouterStaticRoutePolicySrcIP {
			return false
		}
		for _, subnet := range localSubnets {
			if item.IPPrefix == subnet.String() {
				return true
			}
		}
		_, prefix, parseErr := net.ParseCIDR(item.IPPrefix)
		if parseErr != nil {
			return false
		}
		length, _ := prefix.Mask.Size()
		for _, subnet := range netInfo.Subnets() {
			clusterLength, _ := subnet.CIDR.Mask.Size()
			if prefix.IP.Equal(subnet.CIDR.IP) && length >= clusterLength && length < subnet.HostSubnetLength {
				return true
			}
		}
		return false
	})
	if err != nil {
		return err
	}
	_, err = libovsdbops.TransactAndCheck(nbClient, ops)
	return err
}

// CleanupLocalDefaultEgress removes gateway-dependent default egress state while
// retaining chain terminals for feature-owned jumps on the surviving router.
// The caller must establish that this is the local node's gateway.
func CleanupLocalDefaultEgress(nbClient libovsdbclient.Client, router string) error {
	if router != "" {
		ops, err := libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(nbClient, nil, router, libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](
			defaultEgressOwnerIDs(libovsdbops.LogicalRouterPolicyDefaultEgress, router), func(p *nbdb.LogicalRouterPolicy) bool {
				// Feature exemptions can outlive the gateway. Keep their terminal
				// allow actions, including the node chain's packet mark, until the
				// router itself is deleted and OVSDB garbage-collects the policies.
				chain := ptr.Deref(p.Chain, "")
				terminal := (chain == types.DefaultEgressChain || chain == types.DefaultEgressNodeChain) &&
					p.Priority == types.DefaultEgressTerminalPriority && p.Match == "1" &&
					p.Action == nbdb.LogicalRouterPolicyActionAllow
				return !terminal
			}))
		if err != nil {
			return err
		}
		ops, err = libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicateOps(nbClient, ops, router, libovsdbops.GetPredicate[*nbdb.LogicalRouterStaticRoute](
			defaultEgressOwnerIDs(libovsdbops.LogicalRouterStaticRouteDefaultEgress, router), nil))
		if err != nil {
			return err
		}
		if _, err = libovsdbops.TransactAndCheck(nbClient, ops); err != nil {
			return err
		}
	}

	return nil
}

type defaultEgressMatchExpressions struct {
	internalDestination, localIngress, clusterSource string
}

// defaultEgressMatches builds the conditions shared by the two fallback chains.
func defaultEgressMatches(netInfo util.NetInfo, nodeName string, ipv6 bool) defaultEgressMatchExpressions {
	family, transit := "ip4", config.ClusterManager.V4TransitSubnet
	if ipv6 {
		family, transit = "ip6", config.ClusterManager.V6TransitSubnet
	}
	cluster := util.MatchAllIPNetFamily(ipv6, util.GetAllClusterSubnetsFromEntries(netInfo.Subnets()))
	join := util.MatchAllIPNetFamily(ipv6, netInfo.JoinSubnets())
	destinations := append(cluster, join...)
	// Preserve multicast's selected output group. Use the incoming port to
	// recognize local workloads, including migrated VMs with retained IPs.
	return defaultEgressMatchExpressions{
		internalDestination: fmt.Sprintf("%s.mcast || %s.dst == {%s, %s}", family, family, util.JoinIPNets(destinations, ", "), transit),
		localIngress:        fmt.Sprintf("%s && inport == %q", family, netInfo.GetNetworkScopedRouterToSwitchPortName(nodeName)),
		clusterSource:       fmt.Sprintf("%s.src == {%s}", family, util.JoinIPNets(cluster, ", ")),
	}
}
