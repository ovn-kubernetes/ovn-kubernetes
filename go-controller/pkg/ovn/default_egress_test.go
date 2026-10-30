// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func defaultEgressPolicyExternalIDs(router, chain, match string, priority int) map[string]string {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyDefaultEgress, types.DefaultEgressControllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.RouterNameKey: router,
			libovsdbops.ChainKey:      chain,
			libovsdbops.PriorityKey:   fmt.Sprint(priority),
			libovsdbops.MatchKey:      match,
		}).GetExternalIDs()
}

func defaultEgressRouteExternalIDs(router, prefix string) map[string]string {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterStaticRouteDefaultEgress, types.DefaultEgressControllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.RouterNameKey: router,
			libovsdbops.CIDRKey:       libovsdbops.BuildCIDRKey(prefix),
		}).GetExternalIDs()
}

// appendDefaultEgressExpectedPolicies describes the common gateway policy fixture.
// The explicit priorities and actions also document the intended precedence.
func appendDefaultEgressExpectedPolicies(data []libovsdbtest.TestData, router *nbdb.LogicalRouter, nodeName string, ni util.NetInfo, subnets, joinIPs []*net.IPNet) []libovsdbtest.TestData {
	add := func(chain string, priority int, match, action string, nextHops []string) {
		uuid := fmt.Sprintf("%s-%s-%d-%s", router.Name, chain, priority, match)
		p := &nbdb.LogicalRouterPolicy{UUID: uuid, Priority: priority, Match: match, Action: action, Nexthops: nextHops, ExternalIDs: defaultEgressPolicyExternalIDs(router.Name, chain, match, priority)}
		if chain != "" {
			p.Chain = ptr.To(chain)
		} else {
			p.JumpChain = ptr.To("default-egress")
		}
		if chain == "default-egress-node" {
			p.Options = map[string]string{"pkt_mark": "1008"}
		}
		if !slices.Contains(router.Policies, uuid) {
			router.Policies = append(router.Policies, uuid)
		}
		data = append(data, p)
	}
	add("", 90, "ip", "jump", nil)
	for _, chain := range []string{"default-egress", "default-egress-node"} {
		add(chain, 110, "1", "allow", nil)
		for _, subnet := range subnets {
			ipv6 := utilnet.IsIPv6CIDR(subnet)
			family, transit := "ip4", config.ClusterManager.V4TransitSubnet
			if ipv6 {
				family, transit = "ip6", config.ClusterManager.V6TransitSubnet
			}
			var clusters, destinations []string
			for _, s := range ni.Subnets() {
				if utilnet.IsIPv6CIDR(s.CIDR) == ipv6 {
					clusters = append(clusters, s.CIDR.String())
				}
			}
			destinations = slices.Clone(clusters)
			for _, s := range ni.JoinSubnets() {
				if utilnet.IsIPv6CIDR(s) == ipv6 {
					destinations = append(destinations, s.String())
				}
			}
			destinations = append(destinations, transit)
			add(chain, 140, fmt.Sprintf("%s.mcast || %s.dst == {%s}", family, family, strings.Join(destinations, ", ")), "allow", nil)
			add(chain, 130, fmt.Sprintf("%s && inport == %q", family, ni.GetNetworkScopedRouterToSwitchPortName(nodeName)), "allow", nil)
			joinIP, _ := util.MatchFirstIPNetFamily(ipv6, joinIPs)
			add(chain, 120, fmt.Sprintf("%s.src == {%s}", family, strings.Join(clusters, ", ")), "reroute", []string{joinIP.IP.String()})
		}
	}
	return data
}

// withDefaultEgressExemptions adds the chain terminals that the egress controllers
// initialize, independently of gateway reconciliation. Existing fixtures describe
// the exemptions; this helper supplies the common jump and marking expectations.
// Layer 2 routers retain allow actions and do not use these chains.
func withDefaultEgressExemptions(data []libovsdbtest.TestData) []libovsdbtest.TestData {
	result := make([]libovsdbtest.TestData, 0, len(data))
	policies := map[string]*nbdb.LogicalRouterPolicy{}
	for _, row := range data {
		if p, ok := row.(*nbdb.LogicalRouterPolicy); ok {
			copy := *p
			policies[p.UUID] = &copy
			result = append(result, &copy)
		} else if r, ok := row.(*nbdb.LogicalRouter); ok {
			copy := *r
			copy.Policies = slices.Clone(r.Policies)
			result = append(result, &copy)
		} else {
			result = append(result, row)
		}
	}
	for _, row := range result {
		r, ok := row.(*nbdb.LogicalRouter)
		if !ok || !strings.HasSuffix(r.Name, types.OVNClusterRouter) {
			continue
		}
		chains := map[string]bool{}
		for _, id := range r.Policies {
			p := policies[id]
			if p == nil {
				continue
			}
			if p.Chain != nil {
				chains[*p.Chain] = true
			}
		}
		for _, id := range slices.Clone(r.Policies) {
			p := policies[id]
			if p == nil || p.Priority != 102 {
				continue
			}
			chain := ""
			if p.Match == "pkt.mark == 42" {
				chain = "default-egress"
			}
			if p.Options["pkt_mark"] == "1008" {
				chain = "default-egress-node"
			}
			if chain == "" {
				continue
			}
			p.Action, p.JumpChain, p.Options = "jump", ptr.To(chain), nil
			if chains[chain] {
				continue
			}
			terminal := &nbdb.LogicalRouterPolicy{UUID: r.Name + "-" + chain + "-terminal", Chain: ptr.To(chain), Priority: 110, Match: "1", Action: "allow", ExternalIDs: defaultEgressPolicyExternalIDs(r.Name, chain, "1", 110)}
			if chain == "default-egress-node" {
				terminal.Options = map[string]string{"pkt_mark": "1008"}
			}
			result = append(result, terminal)
			r.Policies = append(r.Policies, terminal.UUID)
			chains[chain] = true
		}
	}
	return result
}

func expectedDefaultEgressTerminals(router string) []libovsdbtest.TestData {
	return []libovsdbtest.TestData{
		&nbdb.LogicalRouterPolicy{Chain: ptr.To("default-egress"), Priority: 110, Match: "1", Action: "allow",
			ExternalIDs: defaultEgressPolicyExternalIDs(router, "default-egress", "1", 110)},
		&nbdb.LogicalRouterPolicy{Chain: ptr.To("default-egress-node"), Priority: 110, Match: "1", Action: "allow",
			Options: map[string]string{"pkt_mark": "1008"}, ExternalIDs: defaultEgressPolicyExternalIDs(router, "default-egress-node", "1", 110)},
	}
}

func TestDefaultEgressGatewayCleanup(t *testing.T) {
	require.NoError(t, config.PrepareTestConfig())
	router := &nbdb.LogicalRouter{UUID: "cr", Name: types.OVNClusterRouter, Policies: []string{"fallback", "feature"}, StaticRoutes: []string{"default"}}
	feature := &nbdb.LogicalRouterPolicy{UUID: "feature", Priority: 100, Match: "ip4.src == 10.128.1.3", Action: "reroute", Nexthops: []string{"100.64.0.9"}}
	data := []libovsdbtest.TestData{router, feature,
		&nbdb.LogicalRouterPolicy{UUID: "fallback", Priority: 90, Match: "ip", Action: "jump", JumpChain: ptr.To("default-egress"), ExternalIDs: defaultEgressPolicyExternalIDs(router.Name, "", "ip", 90)},
		&nbdb.LogicalRouterStaticRoute{UUID: "default", IPPrefix: "0.0.0.0/0", Nexthop: "10.128.1.2", ExternalIDs: defaultEgressRouteExternalIDs(router.Name, "0.0.0.0/0")},
	}
	client, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: data}, nil)
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)
	ni := &util.DefaultNetInfo{}
	for _, node := range []string{"remote", "local"} {
		manager := NewGatewayManagerForCleanup(client, ni, router.Name, types.OVNJoinSwitch, ni.GetNetworkScopedGWRouterName(node), node, false)
		require.NoError(t, manager.Cleanup("local"))
		if node == "local" {
			router.Policies = []string{"feature"}
			router.StaticRoutes = nil
			data = []libovsdbtest.TestData{router, feature}
		}
		gomega.NewWithT(t).Expect(client).To(libovsdbtest.HaveData(data))
	}
}

// A controller restart can observe the previous version's gateway annotation
// while the node manager is still preparing host rules. No NB cutover is safe
// until both families (and every configured CIDR) are ready.
func TestDefaultEgressWaitsForHostSNAT(t *testing.T) {
	require.NoError(t, config.PrepareTestConfig())
	t.Cleanup(func() { require.NoError(t, config.PrepareTestConfig()) })
	config.Gateway.Mode = config.GatewayModeLocal
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
		{CIDR: ovntest.MustParseIPNet("10.128.0.0/16"), HostSubnetLength: 24},
		{CIDR: ovntest.MustParseIPNet("10.129.0.0/16"), HostSubnetLength: 24},
		{CIDR: ovntest.MustParseIPNet("fd00::/48"), HostSubnetLength: 64},
	}
	router := &nbdb.LogicalRouter{UUID: "cr", Name: types.OVNClusterRouter, StaticRoutes: []string{"old"}}
	data := []libovsdbtest.TestData{router, &nbdb.LogicalRouterStaticRoute{
		UUID: "old", IPPrefix: "10.128.0.0/16", Policy: ptr.To("src-ip"), Nexthop: "100.64.0.2",
	}}
	client, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: data}, nil)
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)
	ni := &util.DefaultNetInfo{}
	manager := NewGatewayManagerForCleanup(client, ni, router.Name, types.OVNJoinSwitch, "GR_node", "node", false)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}}
	for _, ready := range [][]string{nil, {"10.128.0.0/16"}, {"10.128.0.0/16", "10.129.0.0/16"}, {"10.128.0.0/16", "fd00::/48"}} {
		cfg := &GatewayConfig{
			annoConfig:  &util.L3GatewayConfig{Mode: config.GatewayModeLocal, LocalGatewaySNATSubnets: ready},
			hostSubnets: ovntest.MustParseIPNets("10.128.1.0/24", "fd00:0:0:1::/64"),
		}
		require.ErrorContains(t, manager.SyncGateway(node, cfg, "node"), "waiting for local gateway host SNAT")
		gomega.NewWithT(t).Expect(client).To(libovsdbtest.HaveData(data))
	}
}

func TestDefaultEgressDisabledGatewayCleanup(t *testing.T) {
	for _, mode := range []config.GatewayMode{config.GatewayModeShared, config.GatewayModeLocal} {
		for _, bootstrapOnly := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/bootstrapOnly=%t", mode, bootstrapOnly), func(t *testing.T) {
				require.NoError(t, config.PrepareTestConfig())
				t.Cleanup(func() { require.NoError(t, config.PrepareTestConfig()) })
				config.Gateway.Mode = mode
				config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
					{CIDR: ovntest.MustParseIPNet("10.128.0.0/16"), HostSubnetLength: 24},
					{CIDR: ovntest.MustParseIPNet("fd00::/48"), HostSubnetLength: 64},
				}
				ni := &util.DefaultNetInfo{}
				router := &nbdb.LogicalRouter{UUID: "cr", Name: types.OVNClusterRouter}
				feature := &nbdb.LogicalRouterPolicy{UUID: "feature", Priority: 100,
					Match: "ip4.src == 10.128.1.3", Action: "reroute", Nexthops: []string{"100.64.0.9"}}
				reply := &nbdb.LogicalRouterPolicy{UUID: "reply", Priority: 102,
					Match: "pkt.mark == 42", Action: "jump", JumpChain: ptr.To("default-egress")}
				node := &nbdb.LogicalRouterPolicy{UUID: "node", Priority: 102,
					Match: "ip4.dst == 192.0.2.10", Action: "jump", JumpChain: ptr.To("default-egress-node")}
				router.Policies = []string{feature.UUID, reply.UUID, node.UUID}
				data := []libovsdbtest.TestData{router, feature, reply, node}
				data = appendDefaultEgressExpectedPolicies(data, router, "local", ni,
					ovntest.MustParseIPNets("10.128.1.0/24", "fd00:0:0:1::/64"),
					ovntest.MustParseIPNets("100.64.0.2/16", "fd98::2/64"))
				// Preserve the exact terminal rows and feature policies, including
				// UUIDs and packet marking, across gateway disablement.
				expectedRouter := *router
				expectedRouter.Policies = []string{feature.UUID, reply.UUID, node.UUID}
				expected := []libovsdbtest.TestData{&expectedRouter, feature, reply, node}
				for _, row := range data {
					if policy, ok := row.(*nbdb.LogicalRouterPolicy); ok && policy.Priority == 110 {
						expected = append(expected, policy)
						expectedRouter.Policies = append(expectedRouter.Policies, policy.UUID)
					}
				}
				if bootstrapOnly {
					data = expected
				} else {
					for _, prefix := range []string{"0.0.0.0/0", "::/0"} {
						nextHop := "100.64.0.2"
						if prefix == "::/0" {
							nextHop = "fd98::2"
						}
						if mode == config.GatewayModeLocal {
							nextHop = "10.128.1.2"
							if prefix == "::/0" {
								nextHop = "fd00:0:0:1::2"
							}
						}
						route := &nbdb.LogicalRouterStaticRoute{UUID: prefix, IPPrefix: prefix, Nexthop: nextHop,
							ExternalIDs: defaultEgressRouteExternalIDs(router.Name, prefix)}
						router.StaticRoutes = append(router.StaticRoutes, route.UUID)
						data = append(data, route)
					}
				}
				client, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: data}, nil)
				require.NoError(t, err)
				t.Cleanup(cleanup.Cleanup)
				g := gomega.NewWithT(t)
				remote := NewGatewayManagerForCleanup(client, ni, router.Name, types.OVNJoinSwitch, "GR_remote", "remote", false)
				require.NoError(t, remote.Cleanup("local"))
				g.Expect(client).To(libovsdbtest.HaveData(data))
				require.NoError(t, remote.SyncGateway(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "remote"}},
					&GatewayConfig{annoConfig: &util.L3GatewayConfig{Mode: config.GatewayModeDisabled}}, "local"))
				g.Expect(client).To(libovsdbtest.HaveData(data))
				manager := NewGatewayManagerForCleanup(client, ni, router.Name, types.OVNJoinSwitch, "GR_local", "local", false)
				for range 2 {
					require.NoError(t, manager.SyncGateway(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "local"}},
						&GatewayConfig{annoConfig: &util.L3GatewayConfig{Mode: config.GatewayModeDisabled}}, "local"))
					g.Expect(client).To(libovsdbtest.HaveData(expected))
				}
				// The terminals need no separate cleanup when their router goes away.
				require.NoError(t, libovsdbops.DeleteLogicalRouter(client, &nbdb.LogicalRouter{Name: router.Name}))
				g.Expect(client).To(libovsdbtest.HaveData([]libovsdbtest.TestData{}))
			})
		}
	}
}
