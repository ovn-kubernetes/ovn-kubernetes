// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package iprulemanager

import (
	"errors"
	"fmt"
	"net/netip"
	"runtime"
	"testing"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
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

// Kernel readback and notifications catch rule churn that a Go-struct round trip cannot.
func TestWildcardRuleKernelReconciliation(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	testNS, err := testutils.NewNS()
	if err != nil {
		t.Fatalf("create rule-test network namespace: %v", err)
	}
	t.Cleanup(func() { _ = testNS.Close(); _ = testutils.UnmountNS(testNS) })
	for _, tc := range []struct {
		name, src, dst string
		family         int
	}{
		{"IPv4", "10.128.0.2/32", "0.0.0.0/0", netlink.FAMILY_V4},
		{"IPv6", "fd00::2/128", "::/0", netlink.FAMILY_V6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rule := IPRule{Priority: 6000, Table: 1007, Family: tc.family, Src: netip.MustParsePrefix(tc.src), Dst: netip.MustParsePrefix(tc.dst), IPProto: unix.IPPROTO_TCP,
				Dport: PortRange{RulePortRange: netlink.RulePortRange{Start: 443, End: 443}, Set: true}}
			if err := testNS.Do(func(ns.NetNS) error { return checkWildcardKernelRule(rule) }); err != nil {
				t.Fatalf("reconcile %s wildcard rule: %v", tc.name, err)
			}
		})
	}
}

// Runs inside the isolated namespace; errors are reported by the test goroutine.
func checkWildcardKernelRule(rule IPRule) error {
	c := NewController(rule.Family == netlink.FAMILY_V4, rule.Family == netlink.FAMILY_V6)
	if err := c.Add(rule); err != nil {
		return fmt.Errorf("install wildcard rule: %w", err)
	}
	installed, err := netlink.RuleListFiltered(rule.Family, &netlink.Rule{Priority: rule.Priority}, netlink.RT_FILTER_PRIORITY)
	if err != nil {
		return fmt.Errorf("read kernel rule: %w", err)
	}
	if len(installed) != 1 {
		return fmt.Errorf("kernel retained %d rules, want one", len(installed))
	}
	if _, managed := c.rules[IPRuleFromNetlinkRule(&installed[0])]; !managed {
		return fmt.Errorf("kernel readback has an unmanaged identity: %v", installed[0])
	}
	if err := withoutRuleChanges(func() error {
		adopter := NewController(rule.Family == netlink.FAMILY_V4, rule.Family == netlink.FAMILY_V6)
		if err := adopter.OwnPriority(rule.Priority, rule); err != nil {
			return err
		}
		if err := c.OwnPriority(rule.Priority); err != nil {
			return err
		}
		return c.reconcile()
	}); err != nil {
		return fmt.Errorf("reconcile installed rule: %w", err)
	}
	// Explicit /0 and kernel readback must address the same rule, including deletion.
	if err := c.Add(IPRuleFromNetlinkRule(&installed[0])); err != nil {
		return fmt.Errorf("add equivalent kernel identity: %w", err)
	}
	if len(c.rules) != 1 {
		return fmt.Errorf("equivalent wildcard forms created %d managed rules", len(c.rules))
	}
	if err := c.Delete(rule); err != nil {
		return fmt.Errorf("delete wildcard rule: %w", err)
	}
	remaining, err := netlink.RuleListFiltered(rule.Family, &netlink.Rule{Priority: rule.Priority}, netlink.RT_FILTER_PRIORITY)
	if err != nil {
		return fmt.Errorf("read rules after deletion: %w", err)
	}
	if len(remaining) != 0 {
		return fmt.Errorf("wildcard deletion left %d installed rules", len(remaining))
	}
	return nil
}

// The final rule list alone cannot distinguish retention from a delete/re-add cycle.
func withoutRuleChanges(action func() error) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_NONBLOCK, unix.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("open rule notification socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	groups := uint32(1<<(unix.RTNLGRP_IPV4_RULE-1) | 1<<(unix.RTNLGRP_IPV6_RULE-1))
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: groups}); err != nil {
		return fmt.Errorf("subscribe to rule changes: %w", err)
	}
	if err := action(); err != nil {
		return err
	}
	n, _, err := unix.Recvfrom(fd, make([]byte, 8192), unix.MSG_DONTWAIT)
	if !errors.Is(err, unix.EAGAIN) {
		return fmt.Errorf("unchanged rule generated kernel notifications: %d bytes, %v", n, err)
	}
	return nil
}
