//go:build linux
// +build linux

package node

import (
	"testing"

	"sigs.k8s.io/knftables"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

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
				config.Default.Transport = config.TransportNoOverlay
				config.NoOverlay.OutboundSNAT = config.NoOverlaySNATEnabled
				isNetworkAdvertised = true // It's always true for no-overlay mode
			})

			It("should create two rules for IPv4: return rule for cluster CIDR and unconditional masquerade", func() {
				podSubnet := ovntest.MustParseIPNet("10.244.0.0/24")
				clusterCIDR := ovntest.MustParseIPNet("10.244.0.0/16")
				config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
					{CIDR: clusterCIDR},
				}

				rules, err := getLocalGatewayPodSubnetMasqueradeNFTRule(podSubnet, isNetworkAdvertised)
				Expect(err).NotTo(HaveOccurred())
				Expect(rules).To(HaveLen(2))

				// First rule should be return rule for cluster CIDR
				Expect(*rules[0]).To(Equal(knftables.Rule{
					Rule: knftables.Concat(
						"ip", "saddr", podSubnet,
						"ip", "daddr", clusterCIDR,
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

			It("should create two rules for IPv6: return rule for cluster CIDR and unconditional masquerade", func() {
				podSubnet := ovntest.MustParseIPNet("fd00:10:244:1::/64")
				clusterCIDR := ovntest.MustParseIPNet("fd00:10:244::/48")
				config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
					{CIDR: clusterCIDR},
				}

				rules, err := getLocalGatewayPodSubnetMasqueradeNFTRule(podSubnet, isNetworkAdvertised)
				Expect(err).NotTo(HaveOccurred())
				Expect(rules).To(HaveLen(2))

				// First rule should be return rule for cluster CIDR
				Expect(*rules[0]).To(Equal(knftables.Rule{
					Rule: knftables.Concat(
						"ip6", "saddr", podSubnet,
						"ip6", "daddr", clusterCIDR,
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

				// Test IPv4 subnet - should match IPv4 cluster CIDR
				rulesV4, err := getLocalGatewayPodSubnetMasqueradeNFTRule(podSubnetV4, isNetworkAdvertised)
				Expect(err).NotTo(HaveOccurred())
				Expect(rulesV4).To(HaveLen(2))
				Expect(*rulesV4[0]).To(Equal(knftables.Rule{
					Rule: knftables.Concat(
						"ip", "saddr", podSubnetV4,
						"ip", "daddr", clusterCIDRV4,
						"return",
					),
					Chain: nftablesPodSubnetMasqChain,
				}))

				// Test IPv6 subnet - should match IPv6 cluster CIDR
				rulesV6, err := getLocalGatewayPodSubnetMasqueradeNFTRule(podSubnetV6, isNetworkAdvertised)
				Expect(err).NotTo(HaveOccurred())
				Expect(rulesV6).To(HaveLen(2))
				Expect(*rulesV6[0]).To(Equal(knftables.Rule{
					Rule: knftables.Concat(
						"ip6", "saddr", podSubnetV6,
						"ip6", "daddr", clusterCIDRV6,
						"return",
					),
					Chain: nftablesPodSubnetMasqChain,
				}))
			})

			It("should create return rules for multiple cluster CIDRs of the same IP family", func() {
				podSubnet := ovntest.MustParseIPNet("10.244.0.0/24")
				clusterCIDR1 := ovntest.MustParseIPNet("10.244.0.0/16")
				clusterCIDR2 := ovntest.MustParseIPNet("10.128.0.0/16")
				config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
					{CIDR: clusterCIDR1},
					{CIDR: clusterCIDR2},
				}

				rules, err := getLocalGatewayPodSubnetMasqueradeNFTRule(podSubnet, true)
				Expect(err).NotTo(HaveOccurred())
				// Should have 3 rules: 2 return rules (one per cluster CIDR) + 1 unconditional masquerade
				Expect(rules).To(HaveLen(3))

				// First rule should be return rule for first cluster CIDR
				Expect(*rules[0]).To(Equal(knftables.Rule{
					Rule: knftables.Concat(
						"ip", "saddr", podSubnet,
						"ip", "daddr", clusterCIDR1,
						"return",
					),
					Chain: nftablesPodSubnetMasqChain,
				}))

				// Second rule should be return rule for second cluster CIDR
				Expect(*rules[1]).To(Equal(knftables.Rule{
					Rule: knftables.Concat(
						"ip", "saddr", podSubnet,
						"ip", "daddr", clusterCIDR2,
						"return",
					),
					Chain: nftablesPodSubnetMasqChain,
				}))

				// Third rule should be unconditional masquerade
				Expect(*rules[2]).To(Equal(knftables.Rule{
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

func TestGatewayNFTables(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gateway NFTables Suite")
}
