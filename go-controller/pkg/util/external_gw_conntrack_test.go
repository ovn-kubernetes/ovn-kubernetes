// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package util

import (
	"fmt"
	"net"
	"runtime"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
)

var _ = ginkgo.Describe("findInterfaceForDstIP", func() {
	const linkName = "egct-test0"

	ginkgo.BeforeEach(func() {
		if ovntest.NoRoot() {
			ginkgo.Skip("Test requires root privileges")
		}
	})

	ginkgo.DescribeTable("selects the egress interface from the kernel routing table",
		func(hostCIDR, dstIP string, addOnLinkGatewayRoute bool) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			testNS, err := testutils.NewNS()
			if err != nil {
				ginkgo.Skip(fmt.Sprintf("netns unavailable: %v", err))
			}
			defer func() {
				gomega.Expect(testNS.Close()).To(gomega.Succeed())
				gomega.Expect(testutils.UnmountNS(testNS)).To(gomega.Succeed())
			}()

			err = testNS.Do(func(ns.NetNS) error {
				link := ovntest.AddLink(linkName)
				defer ovntest.DelLink(linkName)

				parsedDst := net.ParseIP(dstIP)
				gomega.Expect(parsedDst).NotTo(gomega.BeNil())

				if addOnLinkGatewayRoute {
					// Peer is reached only via an explicit on-link host route
					// (/32 or /128). Prefix Contains() matching fails here; RouteGet must win.
					bits := 32
					if parsedDst.To4() == nil {
						bits = 128
					}
					gwRoute := &netlink.Route{
						LinkIndex: link.Attrs().Index,
						Dst: &net.IPNet{
							IP:   parsedDst,
							Mask: net.CIDRMask(bits, bits),
						},
						Scope: netlink.SCOPE_LINK,
						Type:  unix.RTN_UNICAST,
					}
					if err := netlink.RouteAdd(gwRoute); err != nil {
						return fmt.Errorf("failed to add on-link gateway route for %s: %w", dstIP, err)
					}
				} else {
					// Kernel installs a connected route from the address prefix
					// (e.g. /24 or /64) covering dstIP.
					hostIPNet := ovntest.MustParseIPNet(hostCIDR)
					if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: hostIPNet}); err != nil {
						return fmt.Errorf("failed to add address %s: %w", hostCIDR, err)
					}
				}

				iface, err := findInterfaceForDstIP(parsedDst)
				if err != nil {
					return err
				}
				gomega.Expect(iface.Name).To(gomega.Equal(linkName))
				gomega.Expect(iface.Index).To(gomega.Equal(link.Attrs().Index))
				return nil
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("IPv6 /64 connected route (control)", "2001:db8:64::1/64", "2001:db8:64::99", false),
		ginkgo.Entry("IPv6 /128 host address with on-link gateway route", "2001:db8:128::1/128", "2001:db8:128::2", true),
		ginkgo.Entry("IPv4 /24 connected route (control)", "198.51.100.1/24", "198.51.100.99", false),
		ginkgo.Entry("IPv4 /32 host address with on-link gateway route", "198.51.100.1/32", "198.51.100.2", true),
	)
})
