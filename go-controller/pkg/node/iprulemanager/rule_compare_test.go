package iprulemanager

import (
	"net"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"

	"k8s.io/utils/ptr"
)

var _ = ginkgo.Describe("Rule Compare", func() {
	var _, testIPNet1, _ = net.ParseCIDR("192.168.1.0/24")
	var _, testIPNet2, _ = net.ParseCIDR("10.0.0.0/8")

	ginkgo.Describe("areNetlinkRulesEqual", func() {
		ginkgo.It("returns true for identical rules", func() {
			r1 := netlink.NewRule()
			r1.Family = netlink.FAMILY_V4
			r1.Priority = 2000
			r1.Table = 1009
			r1.Mark = 0x1001

			r2 := netlink.NewRule()
			r2.Family = netlink.FAMILY_V4
			r2.Priority = 2000
			r2.Table = 1009
			r2.Mark = 0x1001

			gomega.Expect(areNetlinkRulesEqual(r1, r2)).To(gomega.BeTrue())
		})

		ginkgo.It("returns false when Family differs (IPv4 vs IPv6)", func() {
			r1 := netlink.NewRule()
			r1.Family = netlink.FAMILY_V4
			r1.Priority = 2000
			r1.Table = 1009
			r1.Mark = 0x1001

			r2 := netlink.NewRule()
			r2.Family = netlink.FAMILY_V6
			r2.Priority = 2000
			r2.Table = 1009
			r2.Mark = 0x1001

			gomega.Expect(areNetlinkRulesEqual(r1, r2)).To(gomega.BeFalse())
		})

		ginkgo.It("returns false when Priority differs", func() {
			r1 := netlink.NewRule()
			r1.Priority = 2000

			r2 := netlink.NewRule()
			r2.Priority = 3000

			gomega.Expect(areNetlinkRulesEqual(r1, r2)).To(gomega.BeFalse())
		})

		ginkgo.It("returns false when Table differs", func() {
			r1 := netlink.NewRule()
			r1.Table = 1009

			r2 := netlink.NewRule()
			r2.Table = 1011

			gomega.Expect(areNetlinkRulesEqual(r1, r2)).To(gomega.BeFalse())
		})

		ginkgo.It("returns false when Mark differs", func() {
			r1 := netlink.NewRule()
			r1.Mark = 0x1001

			r2 := netlink.NewRule()
			r2.Mark = 0x1002

			gomega.Expect(areNetlinkRulesEqual(r1, r2)).To(gomega.BeFalse())
		})

		ginkgo.It("returns false when Mask differs", func() {
			r1 := netlink.NewRule()
			r1.Mask = ptr.To(uint32(0xFFFF))

			r2 := netlink.NewRule()
			r2.Mask = ptr.To(uint32(0xFF00))

			gomega.Expect(areNetlinkRulesEqual(r1, r2)).To(gomega.BeFalse())
		})

		ginkgo.It("returns true when one Mask is nil", func() {
			r1 := netlink.NewRule()
			r1.Mask = ptr.To(uint32(0xFFFF))

			r2 := netlink.NewRule()
			r2.Mask = nil

			gomega.Expect(areNetlinkRulesEqual(r1, r2)).To(gomega.BeTrue())
		})

		ginkgo.It("returns true when both Masks are nil", func() {
			r1 := netlink.NewRule()
			r1.Mask = nil

			r2 := netlink.NewRule()
			r2.Mask = nil

			gomega.Expect(areNetlinkRulesEqual(r1, r2)).To(gomega.BeTrue())
		})

		ginkgo.It("returns false when Src differs", func() {
			r1 := netlink.NewRule()
			r1.Src = testIPNet1

			r2 := netlink.NewRule()
			r2.Src = testIPNet2

			gomega.Expect(areNetlinkRulesEqual(r1, r2)).To(gomega.BeFalse())
		})

		ginkgo.It("returns false when Dst differs", func() {
			r1 := netlink.NewRule()
			r1.Dst = testIPNet1

			r2 := netlink.NewRule()
			r2.Dst = testIPNet2

			gomega.Expect(areNetlinkRulesEqual(r1, r2)).To(gomega.BeFalse())
		})

		ginkgo.It("returns false when IifName differs", func() {
			r1 := netlink.NewRule()
			r1.IifName = "eth0"

			r2 := netlink.NewRule()
			r2.IifName = "eth1"

			gomega.Expect(areNetlinkRulesEqual(r1, r2)).To(gomega.BeFalse())
		})

		ginkgo.It("returns false when OifName differs", func() {
			r1 := netlink.NewRule()
			r1.OifName = "br-int"

			r2 := netlink.NewRule()
			r2.OifName = "br-ex"

			gomega.Expect(areNetlinkRulesEqual(r1, r2)).To(gomega.BeFalse())
		})

		ginkgo.It("returns false when Invert differs", func() {
			r1 := netlink.NewRule()
			r1.Invert = true

			r2 := netlink.NewRule()
			r2.Invert = false

			gomega.Expect(areNetlinkRulesEqual(r1, r2)).To(gomega.BeFalse())
		})
	})

	ginkgo.Describe("areIPNetsEqual", func() {
		ginkgo.It("returns true for identical IPNets", func() {
			gomega.Expect(areIPNetsEqual(testIPNet1, testIPNet1)).To(gomega.BeTrue())
		})

		ginkgo.It("returns true when both are nil", func() {
			gomega.Expect(areIPNetsEqual(nil, nil)).To(gomega.BeTrue())
		})

		ginkgo.It("returns false when one is nil", func() {
			gomega.Expect(areIPNetsEqual(testIPNet1, nil)).To(gomega.BeFalse())
			gomega.Expect(areIPNetsEqual(nil, testIPNet1)).To(gomega.BeFalse())
		})

		ginkgo.It("returns false for different IPNets", func() {
			gomega.Expect(areIPNetsEqual(testIPNet1, testIPNet2)).To(gomega.BeFalse())
		})

		ginkgo.It("returns false when mask differs", func() {
			_, net1, _ := net.ParseCIDR("192.168.1.0/24")
			_, net2, _ := net.ParseCIDR("192.168.1.0/16")
			gomega.Expect(areIPNetsEqual(net1, net2)).To(gomega.BeFalse())
		})
	})

	ginkgo.Describe("areRulePortRangesEqual", func() {
		ginkgo.It("returns true when both are nil", func() {
			gomega.Expect(areRulePortRangesEqual(nil, nil)).To(gomega.BeTrue())
		})

		ginkgo.It("returns false when one is nil", func() {
			r := &netlink.RulePortRange{Start: 80, End: 443}
			gomega.Expect(areRulePortRangesEqual(r, nil)).To(gomega.BeFalse())
			gomega.Expect(areRulePortRangesEqual(nil, r)).To(gomega.BeFalse())
		})

		ginkgo.It("returns true for identical ranges", func() {
			r1 := &netlink.RulePortRange{Start: 80, End: 443}
			r2 := &netlink.RulePortRange{Start: 80, End: 443}
			gomega.Expect(areRulePortRangesEqual(r1, r2)).To(gomega.BeTrue())
		})

		ginkgo.It("returns false for different ranges", func() {
			r1 := &netlink.RulePortRange{Start: 80, End: 443}
			r2 := &netlink.RulePortRange{Start: 8080, End: 8443}
			gomega.Expect(areRulePortRangesEqual(r1, r2)).To(gomega.BeFalse())
		})
	})

	ginkgo.Describe("areRuleUIDRangesEqual", func() {
		ginkgo.It("returns true when both are nil", func() {
			gomega.Expect(areRuleUIDRangesEqual(nil, nil)).To(gomega.BeTrue())
		})

		ginkgo.It("returns false when one is nil", func() {
			r := &netlink.RuleUIDRange{Start: 1000, End: 2000}
			gomega.Expect(areRuleUIDRangesEqual(r, nil)).To(gomega.BeFalse())
			gomega.Expect(areRuleUIDRangesEqual(nil, r)).To(gomega.BeFalse())
		})

		ginkgo.It("returns true for identical ranges", func() {
			r1 := &netlink.RuleUIDRange{Start: 1000, End: 2000}
			r2 := &netlink.RuleUIDRange{Start: 1000, End: 2000}
			gomega.Expect(areRuleUIDRangesEqual(r1, r2)).To(gomega.BeTrue())
		})

		ginkgo.It("returns false for different ranges", func() {
			r1 := &netlink.RuleUIDRange{Start: 1000, End: 2000}
			r2 := &netlink.RuleUIDRange{Start: 3000, End: 4000}
			gomega.Expect(areRuleUIDRangesEqual(r1, r2)).To(gomega.BeFalse())
		})
	})

	ginkgo.Describe("isNetlinkRuleInSlice", func() {
		ginkgo.It("returns true when rule is in slice", func() {
			r1 := netlink.NewRule()
			r1.Family = netlink.FAMILY_V4
			r1.Priority = 2000
			r1.Table = 1009

			r2 := netlink.NewRule()
			r2.Family = netlink.FAMILY_V6
			r2.Priority = 2000
			r2.Table = 1009

			rules := []netlink.Rule{*r1, *r2}

			found, foundRule := isNetlinkRuleInSlice(rules, r1)
			gomega.Expect(found).To(gomega.BeTrue())
			gomega.Expect(foundRule.Family).To(gomega.Equal(netlink.FAMILY_V4))
		})

		ginkgo.It("returns false when rule is not in slice", func() {
			r1 := netlink.NewRule()
			r1.Family = netlink.FAMILY_V4
			r1.Priority = 2000
			r1.Table = 1009

			r2 := netlink.NewRule()
			r2.Family = netlink.FAMILY_V6
			r2.Priority = 3000
			r2.Table = 1011

			rules := []netlink.Rule{*r2}

			found, _ := isNetlinkRuleInSlice(rules, r1)
			gomega.Expect(found).To(gomega.BeFalse())
		})

		ginkgo.It("distinguishes IPv4 and IPv6 rules with same mark/table/priority", func() {
			// This is the exact bug we fixed - IPv4 and IPv6 fwmark rules
			// should NOT be considered equal
			ruleV4 := netlink.NewRule()
			ruleV4.Family = netlink.FAMILY_V4
			ruleV4.Priority = 2000
			ruleV4.Table = 1009
			ruleV4.Mark = 0x1001

			ruleV6 := netlink.NewRule()
			ruleV6.Family = netlink.FAMILY_V6
			ruleV6.Priority = 2000
			ruleV6.Table = 1009
			ruleV6.Mark = 0x1001

			// Slice contains only IPv4 rule
			rules := []netlink.Rule{*ruleV4}

			// IPv4 should be found
			found, _ := isNetlinkRuleInSlice(rules, ruleV4)
			gomega.Expect(found).To(gomega.BeTrue())

			// IPv6 should NOT be found (this was the bug!)
			found, _ = isNetlinkRuleInSlice(rules, ruleV6)
			gomega.Expect(found).To(gomega.BeFalse())
		})
	})
})
