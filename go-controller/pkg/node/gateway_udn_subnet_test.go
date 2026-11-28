// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"fmt"
	"testing"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestUDNManagementPortSubnetChange(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("Test requires root privileges")
	}

	for _, tc := range []struct {
		name          string
		before        []string
		noPrefixRoute bool
	}{
		{name: "replaces obsolete subnets", before: []string{"172.18.1.2/24", "fd00:1::2/64"}},
		{name: "removes obsolete addresses alongside current ones", before: []string{"172.18.1.2/24", "172.18.2.2/24", "fd00:1::2/64", "fd00:2::2/64"}},
		{name: "removes stale IPv4 secondaries along with primary", before: []string{"172.18.1.2/24", "172.18.1.3/24", "172.18.1.4/24"}},
		{name: "repairs changed masks", before: []string{"172.18.2.2/25", "fd00:2::2/80"}},
		{name: "restores desired IPv4 secondary after removing primary", before: []string{"172.18.2.3/24", "172.18.2.2/24"}},
		{name: "preserves no-prefix-route on replacement", before: []string{"172.18.1.2/24", "fd00:1::2/64"}, noPrefixRoute: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "initialize test configuration")
			t.Cleanup(func() { g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "restore test configuration") })
			config.IPv4Mode, config.IPv6Mode = true, true
			nad := ovntest.GenerateNAD("blue", "blue", "ns1", types.Layer3Topology, "172.18.0.0/16/24,fd00::/16/64", types.NetworkRolePrimary)
			if tc.noPrefixRoute {
				config.Gateway.Mode = config.GatewayModeLocal
				// ParseNADInfo gets transport from the NAD configuration.
				nad.Spec.Config = fmt.Sprintf(`{"cniVersion":"1.0.0","name":"blue","type":"ovn-k8s-cni-overlay","topology":"layer3","role":"primary","subnets":"172.18.0.0/16/24,fd00::/16/64","netAttachDefName":"ns1/blue","transport":"%s"}`, types.NetworkTransportNoOverlay)
			}
			netInfo, err := util.ParseNADInfo(nad)
			g.Expect(err).NotTo(gomega.HaveOccurred(), "parse the blue network configuration")
			gateway := &UserDefinedNetworkGateway{NetInfo: netInfo, node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": `{"blue":["172.18.2.0/24","fd00:2::/64"]}`,
				}},
			}}
			testNS, err := testutils.NewNS()
			g.Expect(err).NotTo(gomega.HaveOccurred(), "create the test network namespace")
			t.Cleanup(func() {
				g.Expect(testNS.Close()).To(gomega.Succeed(), "close the test network namespace")
				g.Expect(testutils.UnmountNS(testNS)).To(gomega.Succeed(), "unmount the test network namespace")
			})
			g.Expect(testNS.Do(func(ns.NetNS) error {
				link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "mp-test"}}
				g.Expect(netlink.LinkAdd(link)).To(gomega.Succeed(), "create the management interface")
				for _, cidr := range append(tc.before, "fe80::1234/64") {
					addr, err := netlink.ParseAddr(cidr)
					g.Expect(err).NotTo(gomega.HaveOccurred(), "parse initial address %s", cidr)
					addr.Flags = unix.IFA_F_NODAD
					g.Expect(netlink.AddrAdd(link, addr)).To(gomega.Succeed(), "add initial address %s", cidr)
				}
				for range 2 {
					g.Expect(gateway.addUDNManagementPortIPs(link)).To(gomega.Succeed(), "reconcile management addresses")
					addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
					g.Expect(err).NotTo(gomega.HaveOccurred(), "list reconciled management addresses")
					var got []string
					for _, addr := range addrs {
						got = append(got, addr.IPNet.String())
						if tc.noPrefixRoute && !addr.IP.IsLinkLocalUnicast() {
							g.Expect(addr.Flags&unix.IFA_F_NOPREFIXROUTE).NotTo(gomega.BeZero(), "address %s must not create a connected route", addr.IPNet)
						}
					}
					g.Expect(got).To(gomega.ConsistOf("172.18.2.2/24", "fd00:2::2/64", "fe80::1234/64"), "only current management addresses and IPv6 link-local should remain")
				}
				return nil
			})).To(gomega.Succeed(), "run management-address reconciliation in the test network namespace")
		})
	}
}
