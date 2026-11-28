// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package node

import (
	"net"

	"sigs.k8s.io/knftables"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Gateway NFTables", func() {
	var originalTransport string
	var originalOutboundSNAT string
	var originalClusterSubnets []config.CIDRNetworkEntry
	var isNetworkAdvertised bool

	BeforeEach(func() {
		// Save original config
		originalTransport = config.Default.Transport
		originalOutboundSNAT = config.NoOverlay.OutboundSNAT
		originalClusterSubnets = config.Default.ClusterSubnets
	})

	AfterEach(func() {
		// Restore original config
		config.Default.Transport = originalTransport
		config.NoOverlay.OutboundSNAT = originalOutboundSNAT
		config.Default.ClusterSubnets = originalClusterSubnets
	})

	Describe("getLocalGatewayPodSubnetMasqueradeNFTRule", func() {
		Context("with outboundSNAT enabled in no-overlay mode", func() {
			BeforeEach(func() {
				config.Default.Transport = types.NetworkTransportNoOverlay
				config.NoOverlay.OutboundSNAT = types.NoOverlaySNATEnabled
				isNetworkAdvertised = true // It's always true for no-overlay mode
			})

			It("should create two rules for IPv4: return rule using nftables set and unconditional masquerade", func() {
				podSubnet := ovntest.MustParseIPNet("10.244.0.0/24")
				clusterCIDR := ovntest.MustParseIPNet("10.244.0.0/16")
				config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
					{CIDR: clusterCIDR},
				}

				rules, err := getLocalGatewayPodSubnetMasqueradeNFTRule(podSubnet, isNetworkAdvertised)
				Expect(err).NotTo(HaveOccurred())
				Expect(rules).To(HaveLen(2))

				// First rule should be return rule using the nftables set
				Expect(*rules[0]).To(Equal(knftables.Rule{
					Rule: knftables.Concat(
						"ip", "saddr", podSubnet,
						"ip", "daddr", "@", types.NFTNoOverlaySNATExemptV4,
						"return",
					),
					Chain: nftablesPodSubnetMasqChain,
				}))

				// Second rule should be unconditional masquerade
				Expect(*rules[1]).To(Equal(knftables.Rule{
					Rule: knftables.Concat(
						"ip", "saddr", podSubnet,
						"masquerade",
					),
					Chain: nftablesPodSubnetMasqChain,
				}))
			})

			It("should create two rules for IPv6: return rule using nftables set and unconditional masquerade", func() {
				podSubnet := ovntest.MustParseIPNet("fd00:10:244:1::/64")
				clusterCIDR := ovntest.MustParseIPNet("fd00:10:244::/48")
				config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
					{CIDR: clusterCIDR},
				}

				rules, err := getLocalGatewayPodSubnetMasqueradeNFTRule(podSubnet, isNetworkAdvertised)
				Expect(err).NotTo(HaveOccurred())
				Expect(rules).To(HaveLen(2))

				// First rule should be return rule using the nftables set
				Expect(*rules[0]).To(Equal(knftables.Rule{
					Rule: knftables.Concat(
						"ip6", "saddr", podSubnet,
						"ip6", "daddr", "@", types.NFTNoOverlaySNATExemptV6,
						"return",
					),
					Chain: nftablesPodSubnetMasqChain,
				}))

				// Second rule should be unconditional masquerade
				Expect(*rules[1]).To(Equal(knftables.Rule{
					Rule: knftables.Concat(
						"ip6", "saddr", podSubnet,
						"masquerade",
					),
					Chain: nftablesPodSubnetMasqChain,
				}))
			})

			It("should handle dual-stack configuration correctly", func() {
				podSubnetV4 := ovntest.MustParseIPNet("10.244.0.0/24")
				podSubnetV6 := ovntest.MustParseIPNet("fd00:10:244:1::/64")
				clusterCIDRV4 := ovntest.MustParseIPNet("10.244.0.0/16")
				clusterCIDRV6 := ovntest.MustParseIPNet("fd00:10:244::/48")
				config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
					{CIDR: clusterCIDRV4},
					{CIDR: clusterCIDRV6},
				}

				// Test IPv4 subnet - should use IPv4 nftables set
				rulesV4, err := getLocalGatewayPodSubnetMasqueradeNFTRule(podSubnetV4, isNetworkAdvertised)
				Expect(err).NotTo(HaveOccurred())
				Expect(rulesV4).To(HaveLen(2))
				Expect(*rulesV4[0]).To(Equal(knftables.Rule{
					Rule: knftables.Concat(
						"ip", "saddr", podSubnetV4,
						"ip", "daddr", "@", types.NFTNoOverlaySNATExemptV4,
						"return",
					),
					Chain: nftablesPodSubnetMasqChain,
				}))

				// Test IPv6 subnet - should use IPv6 nftables set
				rulesV6, err := getLocalGatewayPodSubnetMasqueradeNFTRule(podSubnetV6, isNetworkAdvertised)
				Expect(err).NotTo(HaveOccurred())
				Expect(rulesV6).To(HaveLen(2))
				Expect(*rulesV6[0]).To(Equal(knftables.Rule{
					Rule: knftables.Concat(
						"ip6", "saddr", podSubnetV6,
						"ip6", "daddr", "@", types.NFTNoOverlaySNATExemptV6,
						"return",
					),
					Chain: nftablesPodSubnetMasqChain,
				}))
			})

			It("should create single return rule using nftables set for multiple cluster CIDRs", func() {
				podSubnet := ovntest.MustParseIPNet("10.244.0.0/24")
				clusterCIDR1 := ovntest.MustParseIPNet("10.244.0.0/16")
				clusterCIDR2 := ovntest.MustParseIPNet("10.128.0.0/16")
				config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
					{CIDR: clusterCIDR1},
					{CIDR: clusterCIDR2},
				}

				rules, err := getLocalGatewayPodSubnetMasqueradeNFTRule(podSubnet, true)
				Expect(err).NotTo(HaveOccurred())
				// Should have 2 rules: 1 return rule using nftables set + 1 unconditional masquerade
				// The nftables set will contain both cluster CIDRs
				Expect(rules).To(HaveLen(2))

				// First rule should be return rule using the nftables set (which contains both CIDRs)
				Expect(*rules[0]).To(Equal(knftables.Rule{
					Rule: knftables.Concat(
						"ip", "saddr", podSubnet,
						"ip", "daddr", "@", types.NFTNoOverlaySNATExemptV4,
						"return",
					),
					Chain: nftablesPodSubnetMasqChain,
				}))

				// Second rule should be unconditional masquerade
				Expect(*rules[1]).To(Equal(knftables.Rule{
					Rule: knftables.Concat(
						"ip", "saddr", podSubnet,
						"masquerade",
					),
					Chain: nftablesPodSubnetMasqChain,
				}))
			})
		})

		Context("with advertised network", func() {
			BeforeEach(func() {
				isNetworkAdvertised = true
			})

			It("should create single masquerade rule with remote-node-ips destination for IPv4", func() {
				podSubnet := ovntest.MustParseIPNet("10.244.0.0/24")
				clusterCIDR := ovntest.MustParseIPNet("10.244.0.0/16")
				config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
					{CIDR: clusterCIDR},
				}

				rules, err := getLocalGatewayPodSubnetMasqueradeNFTRule(podSubnet, isNetworkAdvertised)
				Expect(err).NotTo(HaveOccurred())
				Expect(rules).To(HaveLen(1))

				// Should only masquerade traffic to remote node IPs
				Expect(*rules[0]).To(Equal(knftables.Rule{
					Rule: knftables.Concat(
						"ip", "saddr", podSubnet,
						"ip", "daddr", "@", types.NFTRemoteNodeIPsv4,
						"masquerade",
					),
					Chain: nftablesPodSubnetMasqChain,
				}))
			})

			It("should create single masquerade rule with remote-node-ips destination for IPv6", func() {
				podSubnet := ovntest.MustParseIPNet("fd00:10:244:1::/64")
				clusterCIDR := ovntest.MustParseIPNet("fd00:10:244::/48")
				config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
					{CIDR: clusterCIDR},
				}

				rules, err := getLocalGatewayPodSubnetMasqueradeNFTRule(podSubnet, isNetworkAdvertised)
				Expect(err).NotTo(HaveOccurred())
				Expect(rules).To(HaveLen(1))

				// Should only masquerade traffic to remote node IPs
				Expect(*rules[0]).To(Equal(knftables.Rule{
					Rule: knftables.Concat(
						"ip6", "saddr", podSubnet,
						"ip6", "daddr", "@", types.NFTRemoteNodeIPsv6,
						"masquerade",
					),
					Chain: nftablesPodSubnetMasqChain,
				}))
			})
		})

		Context("with default overlay mode and network not advertised", func() {
			BeforeEach(func() {
				isNetworkAdvertised = false
			})

			It("should create unconditional masquerade rule for IPv4", func() {
				podSubnet := ovntest.MustParseIPNet("10.244.0.0/24")

				rules, err := getLocalGatewayPodSubnetMasqueradeNFTRule(podSubnet, isNetworkAdvertised)
				Expect(err).NotTo(HaveOccurred())
				Expect(rules).To(HaveLen(1))

				Expect(*rules[0]).To(Equal(knftables.Rule{
					Rule: knftables.Concat(
						"ip", "saddr", podSubnet,
						"masquerade",
					),
					Chain: nftablesPodSubnetMasqChain,
				}))
			})

			It("should create unconditional masquerade rule for IPv6", func() {
				podSubnet := ovntest.MustParseIPNet("fd00:10:244:1::/64")

				rules, err := getLocalGatewayPodSubnetMasqueradeNFTRule(podSubnet, isNetworkAdvertised)
				Expect(err).NotTo(HaveOccurred())
				Expect(rules).To(HaveLen(1))

				Expect(*rules[0]).To(Equal(knftables.Rule{
					Rule: knftables.Concat(
						"ip6", "saddr", podSubnet,
						"masquerade",
					),
					Chain: nftablesPodSubnetMasqChain,
				}))
			})
		})
	})
})

var _ = Describe("getServiceMSSClampNFTRules", func() {
	var originalServiceCIDRs []*net.IPNet
	var originalMTU, originalRoutableMTU int

	BeforeEach(func() {
		originalServiceCIDRs = config.Kubernetes.ServiceCIDRs
		originalMTU = config.Default.MTU
		originalRoutableMTU = config.Default.RoutableMTU
	})

	AfterEach(func() {
		config.Kubernetes.ServiceCIDRs = originalServiceCIDRs
		config.Default.MTU = originalMTU
		config.Default.RoutableMTU = originalRoutableMTU
	})

	clampRule := func(rule string) *knftables.Rule {
		return &knftables.Rule{Chain: nftablesServiceMSSClampChain, Rule: rule}
	}

	It("clamps IPv4 SYN-ACKs to the MTU minus 40", func() {
		config.Kubernetes.ServiceCIDRs = ovntest.MustParseIPNets("10.96.0.0/16")
		config.Default.MTU = 1500
		config.Default.RoutableMTU = 0

		Expect(getServiceMSSClampNFTRules()).To(Equal([]*knftables.Rule{
			clampRule("ct direction reply ct status dnat ct reply ip saddr { 10.96.0.0/16 } " +
				"tcp flags & (syn | rst) == syn tcp option maxseg size > 1460 tcp option maxseg size set 1460"),
		}), "the IPv4 rule should clamp to 1460 for a 1500-byte MTU")
	})

	It("clamps IPv6 SYN-ACKs to the MTU minus 60", func() {
		config.Kubernetes.ServiceCIDRs = ovntest.MustParseIPNets("fd00:10:96::/112")
		config.Default.MTU = 1500
		config.Default.RoutableMTU = 0

		Expect(getServiceMSSClampNFTRules()).To(Equal([]*knftables.Rule{
			clampRule("ct direction reply ct status dnat ct reply ip6 saddr { fd00:10:96::/112 } " +
				"tcp flags & (syn | rst) == syn tcp option maxseg size > 1440 tcp option maxseg size set 1440"),
		}), "the IPv6 rule should clamp to 1440 for a 1500-byte MTU")
	})

	It("uses the routable MTU and every Service CIDR of each family", func() {
		config.Kubernetes.ServiceCIDRs = ovntest.MustParseIPNets("10.96.0.0/16", "fd00:10:96::/112", "10.97.0.0/16")
		config.Default.MTU = 9000
		config.Default.RoutableMTU = 1400

		Expect(getServiceMSSClampNFTRules()).To(Equal([]*knftables.Rule{
			clampRule("ct direction reply ct status dnat ct reply ip saddr { 10.96.0.0/16, 10.97.0.0/16 } " +
				"tcp flags & (syn | rst) == syn tcp option maxseg size > 1360 tcp option maxseg size set 1360"),
			clampRule("ct direction reply ct status dnat ct reply ip6 saddr { fd00:10:96::/112 } " +
				"tcp flags & (syn | rst) == syn tcp option maxseg size > 1340 tcp option maxseg size set 1340"),
		}), "the rules should clamp to the routable MTU and list every Service CIDR of each family")
	})
})
