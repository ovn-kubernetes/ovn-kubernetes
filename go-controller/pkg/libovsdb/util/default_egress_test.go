// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"context"
	"net"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"

	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestReconcileDefaultEgress(t *testing.T) {
	for _, network := range []struct {
		name, transport string
		udn             bool
	}{
		{"cdn-overlay", "", false},
		{"cdn-no-overlay", types.NetworkTransportNoOverlay, false},
		{"udn-overlay", "", true},
		{"udn-no-overlay", types.NetworkTransportNoOverlay, true},
		{"udn-evpn", types.NetworkTransportEVPN, true},
	} {
		for _, mode := range []config.GatewayMode{config.GatewayModeLocal, config.GatewayModeShared} {
			if network.transport == types.NetworkTransportEVPN && mode == config.GatewayModeShared {
				continue // EVPN supports local gateway mode only.
			}
			t.Run(network.name+"/"+string(mode), func(t *testing.T) {
				require.NoError(t, config.PrepareTestConfig())
				t.Cleanup(func() { require.NoError(t, config.PrepareTestConfig()) })
				config.Gateway.Mode = mode
				config.IPv4Mode, config.IPv6Mode = true, true
				config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
					{CIDR: ovntest.MustParseIPNet("10.128.0.0/14"), HostSubnetLength: 24},
					{CIDR: ovntest.MustParseIPNet("fd00::/48"), HostSubnetLength: 64},
				}
				config.Default.Transport = network.transport
				var netInfo util.NetInfo = &util.DefaultNetInfo{}
				if network.udn {
					var err error
					netInfo, err = util.NewNetInfo(&ovncnitypes.NetConf{
						NetConf:  cnitypes.NetConf{Name: "test-udn"},
						Topology: types.Layer3Topology, Role: types.NetworkRolePrimary,
						Transport:  network.transport,
						Subnets:    "10.128.0.0/14/24,fd00::/48/64",
						JoinSubnet: "100.64.0.0/16,fd98::/64",
					})
					require.NoError(t, err)
				}
				router := &nbdb.LogicalRouter{UUID: "cr", Name: netInfo.GetNetworkScopedClusterRouterName()}
				otherRouter := &nbdb.LogicalRouter{UUID: "gr", Name: "GR_node"}
				data := []libovsdbtest.TestData{router, otherRouter}
				// Old node routes may point at either management port or an obsolete GR IP.
				for i, prefix := range []string{"10.128.1.0/24", "fd00:0:0:1::/64", "10.128.0.0/14", "10.128.0.0/16", "fd00::/48"} {
					id := "old-" + string(rune('a'+i))
					router.StaticRoutes = append(router.StaticRoutes, id)
					data = append(data, &nbdb.LogicalRouterStaticRoute{UUID: id, IPPrefix: prefix, Nexthop: "100.64.0.99", Policy: ptr.To("src-ip")})
				}
				// VM ingress routes, host-specific feature routes and GR routes must survive.
				preserved := []*nbdb.LogicalRouterStaticRoute{
					{UUID: "vm", IPPrefix: "10.128.2.3/32", Nexthop: "10.128.2.3", OutputPort: ptr.To("rtos-node")},
					{UUID: "feature", IPPrefix: "10.128.1.8/32", Nexthop: "100.64.0.9", Policy: ptr.To("src-ip")},
					{UUID: "meg", IPPrefix: "10.128.1.0/24", Nexthop: "192.0.2.1", Policy: ptr.To("src-ip")},
				}
				router.StaticRoutes = append(router.StaticRoutes, "vm", "feature")
				otherRouter.StaticRoutes = []string{"meg"}
				for _, route := range preserved {
					data = append(data, route)
				}
				featurePolicy := &nbdb.LogicalRouterPolicy{UUID: "eip", Priority: 100, Match: "ip4.src == 10.128.1.8", Action: "reroute", Nexthops: []string{"100.64.0.9", "100.64.0.10"}}
				router.Policies = []string{"eip", "reply", "node"}
				data = append(data, featurePolicy,
					&nbdb.LogicalRouterPolicy{UUID: "reply", Priority: 102, Match: "pkt.mark == 42", Action: "allow"},
					&nbdb.LogicalRouterPolicy{UUID: "node", Priority: 102, Match: "ip4.src == 10.128.1.8 && ip4.dst == 192.0.2.1", Action: "allow", Options: map[string]string{"pkt_mark": "1008"}},
				)
				// Replace policies from the earlier fallback layout atomically.
				for _, chain := range []string{"default-egress", "default-egress-node"} {
					for _, stale := range []*nbdb.LogicalRouterPolicy{
						{UUID: chain + "-tagged-terminal", Priority: 110, Match: "1", Action: "allow"},
						{UUID: chain + "-old-terminal", Priority: 0, Match: "1", Action: "allow"},
						{UUID: chain + "-old-destination", Priority: 30, Match: "ip4.dst == {10.128.0.0/14, 100.64.0.0/16, 100.88.0.0/16}", Action: "allow"},
					} {
						stale.Chain = ptr.To(chain)
						stale.ExternalIDs = defaultEgressPolicyIDs(router.Name, chain, stale.Match, stale.Priority).GetExternalIDs()
						stale.ExternalIDs[types.NodeExternalID] = "node"
						router.Policies = append(router.Policies, stale.UUID)
						data = append(data, stale)
					}
				}
				client, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: data}, nil)
				require.NoError(t, err)
				t.Cleanup(cleanup.Cleanup)
				local := ovntest.MustParseIPNets("10.128.1.0/24", "fd00:0:0:1::/64")
				grIPs := []net.IP{net.ParseIP("100.64.0.2"), net.ParseIP("fd98::2")}
				oppositeMode := config.GatewayModeLocal
				if mode == config.GatewayModeLocal {
					oppositeMode = config.GatewayModeShared
				}
				// Upgrade, repeat reconciliation, switch modes, and switch back.
				// Check both families and preserved feature state after every step.
				modes := []config.GatewayMode{mode, mode, oppositeMode, oppositeMode, mode}
				if network.transport == types.NetworkTransportEVPN {
					modes = []config.GatewayMode{mode, mode}
				}
				for _, currentMode := range modes {
					config.Gateway.Mode = currentMode
					require.NoError(t, ReconcileDefaultEgress(client, router.Name, "node", netInfo, local, grIPs))
					var routes []nbdb.LogicalRouterStaticRoute
					require.NoError(t, client.List(context.Background(), &routes))
					require.Len(t, routes, 5)
					for i := range routes {
						routes[i].UUID = ""
					}
					for _, want := range preserved {
						copy := *want
						copy.UUID = ""
						require.Contains(t, routes, copy)
					}
					v4Hop, v6Hop := "100.64.0.2", "fd98::2"
					if currentMode == config.GatewayModeLocal {
						v4Hop, v6Hop = "10.128.1.2", "fd00:0:0:1::2"
					}
					defaults := map[string]string{}
					for _, r := range routes {
						switch r.IPPrefix {
						case "0.0.0.0/0", "::/0":
							require.NotContains(t, defaults, r.IPPrefix, "duplicate default route")
							defaults[r.IPPrefix] = r.Nexthop
							require.Nil(t, r.Policy)
						}
					}
					require.Equal(t, map[string]string{"0.0.0.0/0": v4Hop, "::/0": v6Hop}, defaults)
					var policies []nbdb.LogicalRouterPolicy
					require.NoError(t, client.List(context.Background(), &policies))
					if !util.UsesOverlayEgressPolicyChains(netInfo) {
						require.Len(t, policies, 3)
						for _, policy := range policies {
							require.Nil(t, policy.Chain)
							require.Nil(t, policy.JumpChain)
							if policy.Priority == 102 {
								require.Equal(t, "allow", policy.Action)
								if policy.Match != "pkt.mark == 42" {
									require.Equal(t, "1008", policy.Options["pkt_mark"])
								}
							} else {
								expected := *featurePolicy
								expected.UUID, policy.UUID = "", ""
								require.Equal(t, expected, policy)
							}
						}
						continue
					}
					require.Len(t, policies, 18) // fallback, EIP and two converted exemptions
					for i := range policies {
						policies[i].UUID = ""
					}
					featureCopy := *featurePolicy
					featureCopy.UUID = ""
					require.Contains(t, policies, featureCopy)
					for _, p := range policies {
						if p.Priority == 100 {
							continue
						}
						require.NotContains(t, p.Match, "!=")
						if p.Priority != 102 {
							require.Equal(t, defaultEgressPolicyIDs(router.Name, ptr.Deref(p.Chain, ""), p.Match, p.Priority).GetExternalIDs(), p.ExternalIDs)
						}
						if p.Chain != nil {
							// OVN's ECMP policy flows may omit the chain match. Even
							// the terminal allow must win after a no-reroute jump.
							require.Greater(t, p.Priority, featurePolicy.Priority)
						}
						if ptr.Deref(p.Chain, "") == "default-egress-node" {
							require.Equal(t, "1008", p.Options["pkt_mark"])
						}
						switch p.Priority {
						case 102:
							require.Equal(t, "jump", p.Action)
							chain := "default-egress-node"
							if p.Match == "pkt.mark == 42" {
								chain = "default-egress"
							}
							require.Equal(t, chain, *p.JumpChain)
							require.Nil(t, p.Options)
						case 90:
							require.Equal(t, "jump", p.Action)
							require.Equal(t, "default-egress", *p.JumpChain)
						case 140:
							require.Equal(t, "allow", p.Action)
							require.Contains(t, []string{
								"ip4.mcast || ip4.dst == {10.128.0.0/14, 100.64.0.0/16, 100.88.0.0/16}",
								"ip6.mcast || ip6.dst == {fd00::/48, fd98::/64, fd97::/64}",
							}, p.Match)
						case 130:
							require.Equal(t, "allow", p.Action)
							require.Contains(t, []string{`ip4 && inport == "` + netInfo.GetNetworkScopedRouterToSwitchPortName("node") + `"`, `ip6 && inport == "` + netInfo.GetNetworkScopedRouterToSwitchPortName("node") + `"`}, p.Match)
						case 120:
							require.Equal(t, "reroute", p.Action)
							require.Contains(t, []string{"ip4.src == {10.128.0.0/14}", "ip6.src == {fd00::/48}"}, p.Match)
						case 110:
							require.Equal(t, "allow", p.Action)
							require.Equal(t, "1", p.Match)
						default:
							t.Fatalf("unexpected fallback priority %d", p.Priority)
						}
					}
				}
			})
		}
	}
}

func TestDefaultEgressIPFamilyRemoval(t *testing.T) {
	for _, mode := range []config.GatewayMode{config.GatewayModeShared, config.GatewayModeLocal} {
		for _, keepIPv6 := range []bool{false, true} {
			t.Run(string(mode)+"/"+util.IPFamilyName(keepIPv6), func(t *testing.T) {
				require.NoError(t, config.PrepareTestConfig())
				t.Cleanup(func() { require.NoError(t, config.PrepareTestConfig()) })
				config.Gateway.Mode = mode
				config.IPv4Mode, config.IPv6Mode = true, true
				config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
					{CIDR: ovntest.MustParseIPNet("10.128.0.0/16"), HostSubnetLength: 24},
					{CIDR: ovntest.MustParseIPNet("fd00::/48"), HostSubnetLength: 64},
				}
				local := ovntest.MustParseIPNets("10.128.1.0/24", "fd00:0:0:1::/64")
				gatewayIPs := ovntest.MustParseIPs("100.64.0.2", "fd98::2")
				keep := 0
				if keepIPv6 {
					keep = 1
				}
				prefixes := []string{"0.0.0.0/0", "::/0"}
				removedPrefix := prefixes[1-keep]
				router := &nbdb.LogicalRouter{UUID: "cr", Name: types.OVNClusterRouter}
				otherRouter := &nbdb.LogicalRouter{UUID: "other", Name: "other-router", StaticRoutes: []string{"other-default"}}
				otherDefault := &nbdb.LogicalRouterStaticRoute{UUID: "other-default", IPPrefix: removedPrefix,
					Nexthop: gatewayIPs[1-keep].String(), ExternalIDs: defaultEgressRouteIDs(otherRouter.Name, removedPrefix).GetExternalIDs()}
				client, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{router, otherRouter, otherDefault},
				}, nil)
				require.NoError(t, err)
				t.Cleanup(cleanup.Cleanup)
				ni := &util.DefaultNetInfo{}
				require.NoError(t, ReconcileDefaultEgress(client, router.Name, "node", ni, local, gatewayIPs))

				owned := defaultEgressRouteIDs(router.Name, removedPrefix).GetExternalIDs()
				preserved := []*nbdb.LogicalRouterStaticRoute{
					{IPPrefix: removedPrefix, Nexthop: gatewayIPs[1-keep].String()},
					{IPPrefix: removedPrefix, Nexthop: gatewayIPs[1-keep].String(), Policy: ptr.To("src-ip")},
					{IPPrefix: "10.128.2.3/32", Nexthop: "10.128.2.3", ExternalIDs: defaultEgressRouteIDs(router.Name, "10.128.2.3/32").GetExternalIDs()},
				}
				// Test explicit dst-ip as well as the nil policy installed above.
				stale := &nbdb.LogicalRouterStaticRoute{IPPrefix: removedPrefix, Nexthop: gatewayIPs[1-keep].String(),
					Policy: ptr.To("dst-ip"), ExternalIDs: owned}
				ops, err := libovsdbops.CreateLogicalRouterStaticRoutesOps(client, nil, router.Name, append(preserved, stale)...)
				require.NoError(t, err)
				_, err = libovsdbops.TransactAndCheck(client, ops)
				require.NoError(t, err)

				config.IPv4Mode, config.IPv6Mode = !keepIPv6, keepIPv6
				config.Default.ClusterSubnets = config.Default.ClusterSubnets[keep : keep+1]
				nextHop := gatewayIPs[keep].String()
				if mode == config.GatewayModeLocal {
					nextHop = ni.GetNodeManagementIP(local[keep]).IP.String()
				}
				for range 2 {
					require.NoError(t, ReconcileDefaultEgress(client, router.Name, "node", ni,
						local[keep:keep+1], gatewayIPs[keep:keep+1]))
					var routes []nbdb.LogicalRouterStaticRoute
					require.NoError(t, client.List(context.Background(), &routes))
					for i := range routes {
						routes[i].UUID = ""
					}
					expected := []nbdb.LogicalRouterStaticRoute{{IPPrefix: prefixes[keep], Nexthop: nextHop, ExternalIDs: defaultEgressRouteIDs(router.Name, prefixes[keep]).GetExternalIDs()}}
					for _, route := range append(preserved, otherDefault) {
						copy := *route
						copy.UUID = ""
						expected = append(expected, copy)
					}
					require.ElementsMatch(t, expected, routes)
				}
			})
		}
	}
}

func TestDefaultEgressPolicyIdentity(t *testing.T) {
	require.NoError(t, config.PrepareTestConfig())
	t.Cleanup(func() { require.NoError(t, config.PrepareTestConfig()) })
	routers := []*nbdb.LogicalRouter{
		{UUID: "default", Name: types.OVNClusterRouter},
		{UUID: "udn", Name: "blue_ovn_cluster_router"},
	}
	client, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{routers[0], routers[1]},
	}, nil)
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)
	identities := map[string]bool{}
	for _, router := range routers {
		for _, chain := range []string{"", "default-egress", "default-egress-node"} {
			for _, match := range []string{"1", "ip6.src == fd00::/48"} {
				for _, priority := range []int{110, 130} {
					policy := defaultEgressPolicy(router.Name, chain, match, priority, "allow")
					id := policy.ExternalIDs[types.PrimaryIDKey]
					require.NotContains(t, identities, id)
					identities[id] = true
					for range 2 {
						ops, err := upsertDefaultEgressPolicyOps(client, nil, router.Name, policy)
						require.NoError(t, err)
						_, err = libovsdbops.TransactAndCheck(client, ops)
						require.NoError(t, err)
						// Use a fresh model with only its ID: no UUID or predicate.
						found, err := libovsdbops.GetLogicalRouterPolicy(client, &nbdb.LogicalRouterPolicy{ExternalIDs: policy.ExternalIDs})
						require.NoError(t, err)
						require.Equal(t, match, found.Match)
						require.Equal(t, priority, found.Priority)
						policy = defaultEgressPolicy(router.Name, chain, match, priority, "allow")
					}
				}
			}
		}
	}
	var policies []nbdb.LogicalRouterPolicy
	require.NoError(t, client.List(context.Background(), &policies))
	require.Len(t, policies, len(identities))
}

func TestDefaultEgressExemptionChain(t *testing.T) {
	require.NoError(t, config.PrepareTestConfig())
	t.Cleanup(func() { require.NoError(t, config.PrepareTestConfig()) })
	router := &nbdb.LogicalRouter{UUID: "cr", Name: types.OVNClusterRouter}
	client, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{router}}, nil)
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)
	ops, err := EnsureDefaultEgressChainOps(client, nil, router.Name, &util.DefaultNetInfo{}, "default-egress")
	require.NoError(t, err)
	_, err = libovsdbops.TransactAndCheck(client, ops)
	require.NoError(t, err)
	for range 2 {
		ops, err := EnsureDefaultEgressChainOps(client, nil, router.Name, &util.DefaultNetInfo{}, "default-egress-node")
		require.NoError(t, err)
		_, err = libovsdbops.TransactAndCheck(client, ops)
		require.NoError(t, err)
	}
	router.Policies = []string{"terminal", "normal-terminal"}
	expected := []libovsdbtest.TestData{router, &nbdb.LogicalRouterPolicy{UUID: "terminal", Chain: ptr.To("default-egress-node"), Priority: 110, Match: "1", Action: "allow", Options: map[string]string{"pkt_mark": "1008"}, ExternalIDs: defaultEgressPolicyIDs(router.Name, "default-egress-node", "1", 110).GetExternalIDs()}}
	expected = append(expected, &nbdb.LogicalRouterPolicy{UUID: "normal-terminal", Chain: ptr.To("default-egress"), Priority: 110, Match: "1", Action: "allow", ExternalIDs: defaultEgressPolicyIDs(router.Name, "default-egress", "1", 110).GetExternalIDs()})
	gomega.NewWithT(t).Expect(client).To(libovsdbtest.HaveData(expected))
	policy := &nbdb.LogicalRouterPolicy{Priority: 102, Match: "pkt.mark == 42", Action: "allow", Options: map[string]string{"pkt_mark": "1008"}}
	SetDefaultEgressNoReroutePolicy(policy, &util.DefaultNetInfo{}, "default-egress-node")
	require.Equal(t, "jump", policy.Action)
	require.Equal(t, "default-egress-node", *policy.JumpChain)
	require.Nil(t, policy.Options)

	config.Default.Transport = types.NetworkTransportNoOverlay
	ops, err = EnsureDefaultEgressChainOps(client, nil, router.Name, &util.DefaultNetInfo{}, "default-egress")
	require.NoError(t, err)
	require.Empty(t, ops)
	SetDefaultEgressNoReroutePolicy(policy, &util.DefaultNetInfo{}, "default-egress-node")
	require.Equal(t, "allow", policy.Action)
	require.Nil(t, policy.JumpChain)
	require.Equal(t, "1008", policy.Options["pkt_mark"])
}

func TestDefaultEgressTransportTransition(t *testing.T) {
	require.NoError(t, config.PrepareTestConfig())
	t.Cleanup(func() { require.NoError(t, config.PrepareTestConfig()) })
	config.Gateway.Mode = config.GatewayModeLocal
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: ovntest.MustParseIPNet("10.128.0.0/16"), HostSubnetLength: 24}}
	router := &nbdb.LogicalRouter{UUID: "cr", Name: types.OVNClusterRouter, Policies: []string{"node", "reply"}}
	data := []libovsdbtest.TestData{router,
		&nbdb.LogicalRouterPolicy{UUID: "node", Priority: 102, Match: "ip4.dst == 192.0.2.1", Action: "allow", Options: map[string]string{"pkt_mark": "1008"}},
		&nbdb.LogicalRouterPolicy{UUID: "reply", Priority: 102, Match: "pkt.mark == 42", Action: "allow"},
	}
	client, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: data}, nil)
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)
	for _, transport := range []string{"", types.NetworkTransportNoOverlay, "", types.NetworkTransportNoOverlay} {
		config.Default.Transport = transport
		require.NoError(t, ReconcileDefaultEgress(client, router.Name, "node", &util.DefaultNetInfo{},
			ovntest.MustParseIPNets("10.128.1.0/24"), ovntest.MustParseIPs("100.64.0.2")))
		var policies []nbdb.LogicalRouterPolicy
		require.NoError(t, client.List(context.Background(), &policies))
		if transport == types.NetworkTransportNoOverlay {
			require.Len(t, policies, 2)
		}
		for _, policy := range policies {
			if policy.Priority != 102 {
				continue
			}
			if transport == "" {
				require.Equal(t, "jump", policy.Action)
				require.NotNil(t, policy.JumpChain)
			} else {
				require.Equal(t, "allow", policy.Action)
				require.Nil(t, policy.JumpChain)
				if policy.Match == "ip4.dst == 192.0.2.1" {
					require.Equal(t, "1008", policy.Options["pkt_mark"])
				}
			}
		}
		var routes []nbdb.LogicalRouterStaticRoute
		require.NoError(t, client.List(context.Background(), &routes))
		require.Len(t, routes, 1)
		require.Equal(t, "0.0.0.0/0", routes[0].IPPrefix)
		require.Equal(t, "10.128.1.2", routes[0].Nexthop)
	}
}

func TestDefaultEgressDuplicatePolicyError(t *testing.T) {
	require.NoError(t, config.PrepareTestConfig())
	router := &nbdb.LogicalRouter{UUID: "cr", Name: types.OVNClusterRouter, Policies: []string{"one", "two"}}
	data := []libovsdbtest.TestData{router}
	for _, id := range router.Policies {
		data = append(data, &nbdb.LogicalRouterPolicy{UUID: id, Chain: ptr.To("default-egress"), Priority: 110, Match: "1", Action: "allow", ExternalIDs: defaultEgressPolicyIDs(router.Name, "default-egress", "1", 110).GetExternalIDs()})
	}
	client, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: data}, nil)
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)
	_, err = EnsureDefaultEgressChainOps(client, nil, router.Name, &util.DefaultNetInfo{}, "default-egress")
	require.ErrorContains(t, err, `router "ovn_cluster_router" (chain "default-egress", priority 110, match "1")`)
	require.ErrorContains(t, err, "multiple results")
}

func TestDefaultEgressBootstrapRace(t *testing.T) {
	require.NoError(t, config.PrepareTestConfig())
	t.Cleanup(func() { require.NoError(t, config.PrepareTestConfig()) })
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: ovntest.MustParseIPNet("10.128.0.0/16"), HostSubnetLength: 24}}
	config.Gateway.Mode = config.GatewayModeLocal
	router := &nbdb.LogicalRouter{UUID: "cr", Name: types.OVNClusterRouter}
	client, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{router}}, nil)
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)
	ni := &util.DefaultNetInfo{}
	// Feature initialization has an earlier policy insert in its transaction.
	// Its guard must not replace the terminal's own uniqueness check.
	buildBootstrap := func() []ovsdb.Operation {
		feature := &nbdb.LogicalRouterPolicy{Priority: 102, Match: "ip4.src == 10.128.0.0/16 && ip4.dst == 10.128.0.0/16", Action: "allow"}
		ops, err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(client, nil, router.Name, feature,
			func(p *nbdb.LogicalRouterPolicy) bool {
				return p.Priority == feature.Priority && p.Match == feature.Match
			})
		require.NoError(t, err)
		ops, err = EnsureDefaultEgressChainOps(client, ops, router.Name, ni, "default-egress")
		require.NoError(t, err)
		return ops
	}
	pending := buildBootstrap()
	require.NoError(t, ReconcileDefaultEgress(client, router.Name, "node", ni,
		ovntest.MustParseIPNets("10.128.1.0/24"), []net.IP{net.ParseIP("100.64.0.2")}))
	// The stale transaction must retry instead of inserting a second terminal.
	_, err = libovsdbops.TransactAndCheck(client, pending)
	require.Error(t, err)
	_, err = libovsdbops.TransactAndCheck(client, buildBootstrap())
	require.NoError(t, err)
	policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(client, router.Name,
		func(p *nbdb.LogicalRouterPolicy) bool {
			return ptr.Deref(p.Chain, "") == "default-egress" && p.Match == "1"
		})
	require.NoError(t, err)
	require.Len(t, policies, 1)
}
