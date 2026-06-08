// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"context"
	"fmt"
	"net"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	ovnconfig "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
)

var (
	nftRuleEgressIPSNATChain = `
      add table inet ovn-kubernetes
      add chain inet ovn-kubernetes egress-ip-snat { type nat hook postrouting priority 100 ; }
	`
	nftRuleEgressIPSNATMapV4 = `
      add map inet ovn-kubernetes egress-ip-snat-v4 { type ipv4_addr . ifname : ipv4_addr ; }
	`
	nftRuleEgressIPSNATRuleV4 = `
      add rule inet ovn-kubernetes egress-ip-snat snat to ip saddr . meta oifname map @egress-ip-snat-v4
	`
	nftRuleEgressIPSNATMapV6 = `
      add map inet ovn-kubernetes egress-ip-snat-v6 { type ipv6_addr . ifname : ipv6_addr ; }
	`
	nftRuleEgressIPSNATRuleV6 = `
      add rule inet ovn-kubernetes egress-ip-snat snat to ip6 saddr . meta oifname map @egress-ip-snat-v6
	`
	nftRuleEgressIPConnmarkChain = `
      add chain inet ovn-kubernetes egress-ip-connmark { type filter hook prerouting priority -150 ; }
      add rule inet ovn-kubernetes egress-ip-connmark meta mark 0 meta mark set ct mark
      add rule inet ovn-kubernetes egress-ip-connmark meta mark 1008 ct mark set meta mark
	`
	nftRuleEgressIPSecondaryInterfaceAnnotation = `
      add chain inet ovn-kubernetes ovn-kube-egress-ip { type filter hook postrouting priority srcnat + 1 ; comment "OVN egress IP traffic handling" ; }
	`
	nftRuleEgressIPSecondaryInterfaceMarkRuleIPv4 = `
      add rule inet ovn-kubernetes ovn-kube-egress-ip ip saddr %s meta mark 1009  drop comment "Drop egress IP pod traffic from %s"
	`
	nftRuleEgressIPSecondaryInterfaceMarkRuleIPv6 = `
      add rule inet ovn-kubernetes ovn-kube-egress-ip ip6 saddr %s meta mark 1009  drop comment "Drop egress IP pod traffic from %s"
	`
	nftRuleEgressIPSecondaryInterfaceMarkRuleIPv4Counter = `
      add rule inet ovn-kubernetes ovn-kube-egress-ip ip saddr %s meta mark 1009 counter drop comment "Drop egress IP pod traffic from %s"
	`
	nftRuleEgressIPSecondaryInterfaceMarkRuleIPv6Counter = `
      add rule inet ovn-kubernetes ovn-kube-egress-ip ip6 saddr %s meta mark 1009 counter drop comment "Drop egress IP pod traffic from %s"
	`
)

type initNFTablesTestCase struct {
	v4               bool
	v6               bool
	v4CIDRs          []string
	v6CIDRs          []string
	logLevel         int
	gatewayModeLocal bool
	expectInitErr    string
}

var _ = ginkgo.DescribeTable("initNFTables", func(tc initNFTablesTestCase) {
	fake := nodenft.SetFakeNFTablesHelper()

	ovnconfig.Default.ClusterSubnets = nil
	for _, cidrStr := range tc.v4CIDRs {
		_, cidr, _ := net.ParseCIDR(cidrStr)
		ovnconfig.Default.ClusterSubnets = append(ovnconfig.Default.ClusterSubnets,
			ovnconfig.CIDRNetworkEntry{CIDR: cidr})
	}
	for _, cidrStr := range tc.v6CIDRs {
		_, cidr, _ := net.ParseCIDR(cidrStr)
		ovnconfig.Default.ClusterSubnets = append(ovnconfig.Default.ClusterSubnets,
			ovnconfig.CIDRNetworkEntry{CIDR: cidr})
	}

	savedLogLevel := ovnconfig.Logging.Level
	if tc.logLevel > 0 {
		ovnconfig.Logging.Level = tc.logLevel
	} else {
		ovnconfig.Logging.Level = 0
	}
	defer func() { ovnconfig.Logging.Level = savedLogLevel }()

	savedGatewayMode := ovnconfig.Gateway.Mode
	if tc.gatewayModeLocal {
		ovnconfig.Gateway.Mode = ovnconfig.GatewayModeLocal
	} else {
		ovnconfig.Gateway.Mode = ovnconfig.GatewayModeShared
	}
	defer func() { ovnconfig.Gateway.Mode = savedGatewayMode }()

	c := &Controller{v4: tc.v4, v6: tc.v6}

	nft, err := nodenft.GetNFTablesHelper()
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	tx := nft.NewTransaction()
	err = c.initNFTables(tx)
	if tc.expectInitErr != "" {
		gomega.Expect(err).Should(gomega.MatchError(tc.expectInitErr))
		return
	}
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	gomega.Expect(nft.Run(context.Background(), tx)).Should(gomega.Succeed())

	expectedChain := nftRuleEgressIPSNATChain
	if tc.v4 {
		expectedChain += nftRuleEgressIPSNATMapV4 + nftRuleEgressIPSNATRuleV4
	}
	if tc.v6 {
		expectedChain += nftRuleEgressIPSNATMapV6 + nftRuleEgressIPSNATRuleV6
	}
	if tc.gatewayModeLocal {
		expectedChain += nftRuleEgressIPConnmarkChain
	}
	expectedChain += nftRuleEgressIPSecondaryInterfaceAnnotation
	useCounter := tc.logLevel > 4
	if tc.v4 {
		tmpl := nftRuleEgressIPSecondaryInterfaceMarkRuleIPv4
		if useCounter {
			tmpl = nftRuleEgressIPSecondaryInterfaceMarkRuleIPv4Counter
		}
		for _, cidr := range tc.v4CIDRs {
			expectedChain += "\n" + fmt.Sprintf(tmpl, cidr, cidr)
		}
	}
	if tc.v6 {
		tmpl := nftRuleEgressIPSecondaryInterfaceMarkRuleIPv6
		if useCounter {
			tmpl = nftRuleEgressIPSecondaryInterfaceMarkRuleIPv6Counter
		}
		for _, cidr := range tc.v6CIDRs {
			expectedChain += "\n" + fmt.Sprintf(tmpl, cidr, cidr)
		}
	}

	gomega.Expect(nodenft.MatchNFTRules(expectedChain, fake.Dump())).To(gomega.Succeed())
},
	ginkgo.Entry("dual-stack single CIDRs", initNFTablesTestCase{
		v4: true, v6: true,
		v4CIDRs: []string{"5.5.5.0/24"},
		v6CIDRs: []string{"2001:0:0:1::/64"},
	}),
	ginkgo.Entry("IPv4 only", initNFTablesTestCase{
		v4: true, v6: false,
		v4CIDRs: []string{"10.128.0.0/14"},
	}),
	ginkgo.Entry("IPv6 only", initNFTablesTestCase{
		v4: false, v6: true,
		v6CIDRs: []string{"fd00:10:244::/48"},
	}),
	ginkgo.Entry("IPv4 enabled but no CIDRs configured", initNFTablesTestCase{
		v4: true, v6: false,
		v4CIDRs:       nil,
		expectInitErr: "IPv4 enabled but no cluster pod CIDRs configured",
	}),
	ginkgo.Entry("IPv6 enabled but no CIDRs configured", initNFTablesTestCase{
		v4: false, v6: true,
		v6CIDRs:       nil,
		expectInitErr: "IPv6 enabled but no cluster pod CIDRs configured",
	}),
	ginkgo.Entry("multiple IPv4 CIDRs", initNFTablesTestCase{
		v4: true, v6: false,
		v4CIDRs: []string{"10.128.0.0/14", "10.132.0.0/14"},
	}),
	ginkgo.Entry("multiple IPv6 CIDRs", initNFTablesTestCase{
		v4: false, v6: true,
		v6CIDRs: []string{"fd00:10:244::/48", "fd00:10:245::/48"},
	}),
	ginkgo.Entry("dual-stack multiple CIDRs", initNFTablesTestCase{
		v4: true, v6: true,
		v4CIDRs: []string{"10.128.0.0/14", "10.132.0.0/14"},
		v6CIDRs: []string{"fd00:10:244::/48", "fd00:10:245::/48"},
	}),
	ginkgo.Entry("both protocols disabled creates chain only", initNFTablesTestCase{
		v4: false, v6: false,
	}),
	ginkgo.Entry("debug logging enables counter in rules", initNFTablesTestCase{
		v4: true, v6: true,
		v4CIDRs:  []string{"10.0.0.0/16"},
		v6CIDRs:  []string{"fd00::/112"},
		logLevel: 5,
	}),
	ginkgo.Entry("LGW mode adds connmark chain", initNFTablesTestCase{
		v4: true, v6: true,
		v4CIDRs:          []string{"10.128.0.0/14"},
		v6CIDRs:          []string{"fd00:10:244::/48"},
		gatewayModeLocal: true,
	}),
)
