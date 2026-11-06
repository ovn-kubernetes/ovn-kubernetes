// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeimport

import (
	"errors"
	"net"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/go-logr/logr"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"k8s.io/apimachinery/pkg/util/sets"

	ovntesting "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	uplinkIPv4CIDR       = "198.51.100.10/24"
	otherLinkIPv4CIDR    = "198.51.100.11/24"
	ipv4Gateway          = "198.51.100.1"
	uplinkIPv6Gateway    = "fe80::1"
	otherLinkIPv6Gateway = "fe80::2"
	externalIPv4CIDR     = "203.0.113.0/24"
	multipathIPv6CIDR    = "2001:db8::/64"
	podRouteCIDR         = "10.128.1.0/24"
	podSubnetCIDR        = "10.128.0.0/16"
	staticRouteCIDR      = "192.0.2.0/24"
	mainTableRouteCIDR   = "198.18.0.0/15"
)

// Exercise route selection and dump fallbacks against real IPv4 and IPv6 routes
// in an isolated network namespace, including multipath and subnet exclusions.
var _ = ginkgo.Describe("BGP route import", func() {
	ovntesting.OnSupportedPlatformsIt("imports matching routes from a network namespace", func() {
		const table = 1000
		testNS, err := testutils.NewNS()
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "create network namespace for BGP route import")
		ginkgo.DeferCleanup(testutils.UnmountNS, testNS)
		ginkgo.DeferCleanup(testNS.Close)

		err = testNS.Do(func(ns.NetNS) error {
			defer ginkgo.GinkgoRecover()
			uplink := setupBGPRouteTest(table)
			c := &controller{log: logr.Discard(), netlink: util.GetNetLinkOps()}

			ginkgo.By("selecting BGP routes from the requested table, including both IP families")
			expectBGPRoutes(c, table, nil, 0,
				route{dst: externalIPv4CIDR, gw: ipv4Gateway},
				route{dst: multipathIPv6CIDR, gw: uplinkIPv6Gateway},
				route{dst: multipathIPv6CIDR, gw: otherLinkIPv6Gateway},
				route{dst: podRouteCIDR, gw: ipv4Gateway},
			)

			ginkgo.By("reading the main routing table")
			expectBGPRoutes(c, unix.RT_TABLE_MAIN, nil, 0,
				route{dst: mainTableRouteCIDR, gw: ipv4Gateway})

			ginkgo.By("excluding pod subnets and selecting the uplink for single and multipath routes")
			expectBGPRoutes(c, table, ovntesting.MustParseIPNets(podSubnetCIDR), uplink.Index,
				route{dst: multipathIPv6CIDR, gw: uplinkIPv6Gateway})

			ginkgo.By("returning no routes when the uplink is unresolved or the table does not exist")
			expectBGPRoutes(c, table, nil, noRouteLinkIndex)
			expectBGPRoutes(c, table+1, nil, 0)

			ginkgo.By("falling back to client side filtering when the kernel cannot filter the dump")
			var matching []netlink.Route
			dumpErr := util.GetNetLinkOps().RouteListFilteredIter(netlink.FAMILY_ALL,
				&netlink.Route{Dst: ovntesting.MustParseIPNet(mainTableRouteCIDR)},
				netlink.RT_FILTER_DST,
				func(nlroute netlink.Route) bool {
					matching = append(matching, nlroute)
					return true
				})
			gomega.Expect(dumpErr).NotTo(gomega.HaveOccurred(), "list routes by destination")
			gomega.Expect(matching).To(gomega.HaveLen(1), "expected the route matching destination %s", mainTableRouteCIDR)
			gomega.Expect(matching[0].Table).To(gomega.Equal(unix.RT_TABLE_MAIN))

			ginkgo.By("falling back for nonzero IPv4 scope regardless of the client filter mask")
			for _, mask := range []uint64{netlink.RT_FILTER_SCOPE, 0} {
				filter := &netlink.Route{Scope: netlink.SCOPE_LINK}
				expected, listErr := netlink.RouteListFiltered(netlink.FAMILY_V4, filter, mask)
				gomega.Expect(listErr).NotTo(gomega.HaveOccurred())
				gomega.Expect(expected).NotTo(gomega.BeEmpty())
				var got []netlink.Route
				dumpErr = util.GetNetLinkOps().RouteListFilteredIter(netlink.FAMILY_V4, filter, mask,
					func(nlroute netlink.Route) bool {
						got = append(got, nlroute)
						return true
					})
				gomega.Expect(dumpErr).NotTo(gomega.HaveOccurred(), "list routes with link scope and mask %d", mask)
				gomega.Expect(got).To(gomega.ConsistOf(expected), "preserve client filtering with mask %d", mask)
			}

			ginkgo.By("adding dump error context while preserving the kernel error")
			const missingLinkIndex = 1000000
			dumpErr = util.GetNetLinkOps().RouteListFilteredIter(netlink.FAMILY_V4,
				&netlink.Route{LinkIndex: missingLinkIndex}, netlink.RT_FILTER_OIF,
				func(netlink.Route) bool { return true })
			gomega.Expect(dumpErr).To(gomega.MatchError(gomega.ContainSubstring("failed to list filtered routes")))
			gomega.Expect(errors.Is(dumpErr, unix.ENODEV)).To(gomega.BeTrue(), "preserve ENODEV for a missing interface")
			return nil
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "run BGP route import checks in namespace %s", testNS.Path())
	})
})

// setupBGPRouteTest installs dual-stack BGP routes and unrelated routes to check
// table, protocol, subnet and uplink filtering, and returns the selected uplink.
func setupBGPRouteTest(table int) *netlink.Dummy {
	ginkgo.GinkgoHelper()
	uplink := addTestRouteLink("uplink", uplinkIPv4CIDR)
	other := addTestRouteLink("other", otherLinkIPv4CIDR)
	gateway := net.ParseIP(ipv4Gateway)
	for _, r := range []netlink.Route{
		{Table: table, Protocol: unix.RTPROT_BGP, LinkIndex: other.Index,
			Dst: ovntesting.MustParseIPNet(externalIPv4CIDR), Gw: gateway},
		{Table: table, Protocol: unix.RTPROT_BGP, Dst: ovntesting.MustParseIPNet(multipathIPv6CIDR),
			MultiPath: []*netlink.NexthopInfo{
				{LinkIndex: uplink.Index, Gw: net.ParseIP(uplinkIPv6Gateway), Flags: unix.RTNH_F_ONLINK},
				{LinkIndex: other.Index, Gw: net.ParseIP(otherLinkIPv6Gateway), Flags: unix.RTNH_F_ONLINK},
			}},
		{Table: table, Protocol: unix.RTPROT_BGP, LinkIndex: uplink.Index,
			Dst: ovntesting.MustParseIPNet(podRouteCIDR), Gw: gateway},
		{Table: table, Protocol: unix.RTPROT_STATIC, LinkIndex: uplink.Index,
			Dst: ovntesting.MustParseIPNet(staticRouteCIDR), Gw: gateway},
		{Table: unix.RT_TABLE_MAIN, Protocol: unix.RTPROT_BGP, LinkIndex: uplink.Index,
			Dst: ovntesting.MustParseIPNet(mainTableRouteCIDR), Gw: gateway},
	} {
		gomega.Expect(netlink.RouteAdd(&r)).To(gomega.Succeed(),
			"add route %s to table %d with protocol %d", r.Dst, r.Table, r.Protocol)
	}
	return uplink
}

// addTestRouteLink creates an addressed dummy interface for reachable test gateways.
func addTestRouteLink(name, address string) *netlink.Dummy {
	ginkgo.GinkgoHelper()
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}
	gomega.Expect(netlink.LinkAdd(link)).To(gomega.Succeed(), "create dummy interface %q", name)
	gomega.Expect(netlink.LinkSetUp(link)).To(gomega.Succeed(), "bring interface %q up", name)
	addr, err := netlink.ParseAddr(address)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "parse address %q for interface %q", address, name)
	gomega.Expect(netlink.AddrAdd(link, addr)).To(gomega.Succeed(), "assign address %q to interface %q", address, name)
	return link
}

// expectBGPRoutes checks the complete imported route set after subnet and uplink filtering.
func expectBGPRoutes(c *controller, table int, ignored []*net.IPNet, linkIndex int, expected ...route) {
	ginkgo.GinkgoHelper()
	got, err := c.getBGPRoutes(table, ignored, linkIndex)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(),
		"list BGP routes from table %d with ignored subnets %v and link index %d", table, ignored, linkIndex)
	gomega.Expect(got).To(gomega.Equal(sets.New(expected...)),
		"unexpected BGP routes from table %d with ignored subnets %v and link index %d", table, ignored, linkIndex)
}
