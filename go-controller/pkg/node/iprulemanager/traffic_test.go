// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package iprulemanager

import (
	"net/netip"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestTrafficRuleRoundTripAndIdentity(t *testing.T) {
	for _, protocol := range []int{unix.IPPROTO_TCP, unix.IPPROTO_UDP, unix.IPPROTO_SCTP} {
		for _, family := range []struct {
			src, dst string
			family   int
		}{
			{"10.128.0.2/32", "192.0.2.0/24", netlink.FAMILY_V4},
			{"fd00::2/128", "2001:db8::/32", netlink.FAMILY_V6},
		} {
			rule := IPRule{Priority: 6000, Table: 1007, Family: family.family, Src: netip.MustParsePrefix(family.src), Dst: netip.MustParsePrefix(family.dst), IPProto: protocol,
				Dport: PortRange{RulePortRange: netlink.RulePortRange{Start: 5060, End: 5062}, Set: true},
				Sport: PortRange{RulePortRange: netlink.RulePortRange{Start: 16384, End: 32767}, Set: true}}
			if got := IPRuleFromNetlinkRule(rule.toNetlinkRule()); got != rule {
				t.Fatalf("roundtrip = %v, want %v", got, rule)
			}
			rules := map[IPRule]bool{rule: true}
			peer := rule
			peer.Dport.Start++
			rules[peer] = true
			peer = rule
			peer.Sport.End--
			rules[peer] = true
			peer = rule
			peer.IPProto = unix.IPPROTO_TCP
			if protocol == unix.IPPROTO_TCP {
				peer.IPProto = unix.IPPROTO_UDP
			}
			rules[peer] = true
			if len(rules) != 4 {
				t.Fatal("independent L4 rules share an identity")
			}
		}
	}
	if rule := (IPRule{}).toNetlinkRule(); rule.Dport != nil || rule.Sport != nil {
		t.Fatal("wildcard ports became explicit port zero")
	}
}

func TestExplicitZeroPortPresence(t *testing.T) {
	for _, tc := range []struct {
		name                string
		source, destination bool
	}{
		{name: "source", source: true},
		{name: "destination", destination: true},
		{name: "both", source: true, destination: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := &netlink.Rule{}
			if tc.source {
				input.Sport = netlink.NewRulePortRange(0, 0)
			}
			if tc.destination {
				input.Dport = netlink.NewRulePortRange(0, 0)
			}
			absent := IPRuleFromNetlinkRule(&netlink.Rule{})
			present := IPRuleFromNetlinkRule(input)
			if absent == present || present.Dport.Set != tc.destination || present.Sport.Set != tc.source {
				t.Fatal("port presence lost in identity")
			}
			got := present.toNetlinkRule()
			if (got.Dport != nil) != tc.destination || (got.Sport != nil) != tc.source {
				t.Fatal("explicit port filters lost")
			}
		})
	}
}

func TestPortRangeValueIdentity(t *testing.T) {
	first := &netlink.Rule{IPProto: unix.IPPROTO_UDP,
		Dport: netlink.NewRulePortRange(80, 80), Sport: netlink.NewRulePortRange(1024, 65535)}
	second := &netlink.Rule{IPProto: unix.IPPROTO_UDP,
		Dport: netlink.NewRulePortRange(80, 80), Sport: netlink.NewRulePortRange(1024, 65535)}
	a, b := IPRuleFromNetlinkRule(first), IPRuleFromNetlinkRule(second)
	if a != b || len(map[IPRule]bool{a: true, b: true}) != 1 {
		t.Fatal("equal port values have different rule identities")
	}
	first.Dport.Start++
	if a != b {
		t.Fatal("rule shares port storage with its netlink input")
	}
	output := a.toNetlinkRule()
	if output.Sport.Start != 1024 || output.Sport.End != 65535 {
		t.Fatal("port range boundaries lost")
	}
	output.Sport.End--
	if a != b {
		t.Fatal("rule shares port storage with its netlink output")
	}
}
