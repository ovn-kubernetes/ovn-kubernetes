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

	cases := []struct {
		name, hostCIDR, dstIP string
		onLink                bool
	}{
		{"IPv6 /64 connected route (control)", "2001:db8:64::1/64", "2001:db8:64::99", false},
		{"IPv6 /128 host address with on-link gateway route", "2001:db8:128::1/128", "2001:db8:128::2", true},
		{"IPv4 /24 connected route (control)", "198.51.100.1/24", "198.51.100.99", false},
		{"IPv4 /32 host address with on-link gateway route", "198.51.100.1/32", "198.51.100.2", true},
	}
	for _, tc := range cases {
		ovntest.OnSupportedPlatformsIt(tc.name, func() {
			hostCIDR, dstIP, addOnLinkGatewayRoute := tc.hostCIDR, tc.dstIP, tc.onLink
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			testNS, err := testutils.NewNS()
			if err != nil {
				ginkgo.Skip(fmt.Sprintf("netns unavailable: %v", err))
			}
			defer func() {
				gomega.Expect(testNS.Close()).To(gomega.Succeed(), "failed to close namespace for destination %s", dstIP)
				gomega.Expect(testutils.UnmountNS(testNS)).To(gomega.Succeed(), "failed to unmount namespace for destination %s", dstIP)
			}()

			err = testNS.Do(func(ns.NetNS) error {
				defer ginkgo.GinkgoRecover()
				link := ovntest.AddLink(linkName)
				defer ovntest.DelLink(linkName)

				parsedDst := net.ParseIP(dstIP)
				gomega.Expect(parsedDst).NotTo(gomega.BeNil(), "invalid destination IP %q", dstIP)

				hostIPNet := ovntest.MustParseIPNet(hostCIDR)
				if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: hostIPNet}); err != nil {
					return fmt.Errorf("failed to add address %s: %w", hostCIDR, err)
				}
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
				}

				iface, err := findInterfaceForDstIP(parsedDst)
				if err != nil {
					return err
				}
				gomega.Expect(iface.Name).To(gomega.Equal(linkName), "wrong interface name for destination %s", dstIP)
				gomega.Expect(iface.Index).To(gomega.Equal(link.Attrs().Index), "wrong interface index for destination %s", dstIP)
				return nil
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "interface lookup failed for destination %s with host address %s", dstIP, hostCIDR)
		})
	}
})
