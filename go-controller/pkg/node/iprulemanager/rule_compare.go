package iprulemanager

import (
	"net"

	"github.com/vishvananda/netlink"
)

func areNetlinkRulesEqual(r1, r2 *netlink.Rule) bool {
	// Basic fields
	if r1.Family != r2.Family ||
		r1.Priority != r2.Priority ||
		r1.Table != r2.Table ||
		r1.Type != r2.Type ||
		r1.Protocol != r2.Protocol {
		return false
	}

	// Mark only: skip Mask comparison.
	// Mask is sometimes populated differently by kernel/iproute2 vs netlink.RuleList,
	// which can cause false mismatches for identical fwmark rules.
	if r1.Mark != r2.Mark {
		return false
	}

	// Other integer fields
	if r1.Tos != r2.Tos ||
		r1.TunID != r2.TunID ||
		r1.Goto != r2.Goto ||
		r1.Flow != r2.Flow ||
		r1.SuppressIfgroup != r2.SuppressIfgroup ||
		r1.SuppressPrefixlen != r2.SuppressPrefixlen ||
		r1.IPProto != r2.IPProto {
		return false
	}

	// String fields
	if r1.IifName != r2.IifName || r1.OifName != r2.OifName {
		return false
	}

	// Bool field
	if r1.Invert != r2.Invert {
		return false
	}

	// Pointer fields
	if !areIPNetsEqual(r1.Src, r2.Src) || !areIPNetsEqual(r1.Dst, r2.Dst) {
		return false
	}

	if !areRulePortRangesEqual(r1.Sport, r2.Sport) || !areRulePortRangesEqual(r1.Dport, r2.Dport) {
		return false
	}

	if !areRuleUIDRangesEqual(r1.UIDRange, r2.UIDRange) {
		return false
	}

	return true
}

func areRulePortRangesEqual(r1, r2 *netlink.RulePortRange) bool {
	if r1 == nil && r2 == nil {
		return true
	}
	if r1 == nil || r2 == nil {
		return false
	}
	return r1.Start == r2.Start && r1.End == r2.End
}

func areRuleUIDRangesEqual(r1, r2 *netlink.RuleUIDRange) bool {
	if r1 == nil && r2 == nil {
		return true
	}
	if r1 == nil || r2 == nil {
		return false
	}
	return r1.Start == r2.Start && r1.End == r2.End
}

func areIPNetsEqual(n1, n2 *net.IPNet) bool {
	if n1 == nil && n2 == nil {
		return true
	}
	if n1 == nil || n2 == nil {
		return false
	}

	if !n1.IP.Equal(n2.IP) {
		return false
	}

	n1ones, n1bits := n1.Mask.Size()
	n2ones, n2bits := n2.Mask.Size()
	return n1ones == n2ones && n1bits == n2bits
}

func isNetlinkRuleInSlice(rules []netlink.Rule, candidate *netlink.Rule) (bool, *netlink.Rule) {
	for _, r := range rules {
		r := r
		if areNetlinkRulesEqual(&r, candidate) {
			return true, &r
		}
	}
	return false, netlink.NewRule()
}
