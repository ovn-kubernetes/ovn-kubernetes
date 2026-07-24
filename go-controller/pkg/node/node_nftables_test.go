// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package node

import (
	"context"
	"testing"

	"sigs.k8s.io/knftables"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"

	. "github.com/onsi/gomega"
)

func TestSetupEgressIPARPBlockNFTables(t *testing.T) {
	const testUplinkName = "eth0"

	tests := []struct {
		name               string
		egressIPs          []string
		uplinkName         string
		ipv4Mode           bool
		ipv6Mode           bool
		expectedV4Elements []string
		expectedV6Elements []string
		expectedRules      []*knftables.Rule
		expectError        string
	}{
		{
			name:               "single IPv4 egress IP",
			egressIPs:          []string{"192.168.1.10"},
			uplinkName:         testUplinkName,
			ipv4Mode:           true,
			ipv6Mode:           false,
			expectedV4Elements: []string{"192.168.1.10"},
			expectedV6Elements: nil,
			expectedRules: []*knftables.Rule{{
				Chain: nftEgressIPDropChain,
				Rule: knftables.Concat(
					"arp daddr ip @"+types.NFTEgressIPARPBlockV4,
					"drop"),
			}},
			expectError: "",
		},
		{
			name:               "multiple IPv4 egress IPs",
			egressIPs:          []string{"192.168.1.10", "192.168.1.20", "10.0.0.5"},
			uplinkName:         testUplinkName,
			ipv4Mode:           true,
			ipv6Mode:           false,
			expectedV4Elements: []string{"192.168.1.10", "192.168.1.20", "10.0.0.5"},
			expectedV6Elements: nil,
			expectedRules: []*knftables.Rule{{
				Chain: nftEgressIPDropChain,
				Rule: knftables.Concat(
					"arp daddr ip @"+types.NFTEgressIPARPBlockV4,
					"drop"),
			}},
			expectError: "",
		},
		{
			name:               "single IPv6 egress IP",
			egressIPs:          []string{"2001:db8::1"},
			uplinkName:         testUplinkName,
			ipv4Mode:           false,
			ipv6Mode:           true,
			expectedV4Elements: nil,
			expectedV6Elements: []string{"2001:db8::1"},
			expectedRules: []*knftables.Rule{{
				Chain: nftEgressIPDropChain,
				Rule: knftables.Concat(
					"icmpv6 type nd-neighbor-solicit",
					"icmpv6 taddr @"+types.NFTEgressIPNDPBlockV6,
					"drop"),
			}},
			expectError: "",
		},
		{
			name:               "multiple IPv6 egress IPs",
			egressIPs:          []string{"2001:db8::1", "2001:db8::2", "fd00::100"},
			uplinkName:         testUplinkName,
			ipv4Mode:           false,
			ipv6Mode:           true,
			expectedV4Elements: nil,
			expectedV6Elements: []string{"2001:db8::1", "2001:db8::2", "fd00::100"},
			expectedRules: []*knftables.Rule{{
				Chain: nftEgressIPDropChain,
				Rule: knftables.Concat(
					"icmpv6 type nd-neighbor-solicit",
					"icmpv6 taddr @"+types.NFTEgressIPNDPBlockV6,
					"drop"),
			}},
			expectError: "",
		},
		{
			name:               "dual-stack egress IPs",
			egressIPs:          []string{"192.168.1.10", "2001:db8::1", "10.0.0.5", "fd00::200"},
			uplinkName:         testUplinkName,
			ipv4Mode:           true,
			ipv6Mode:           true,
			expectedV4Elements: []string{"192.168.1.10", "10.0.0.5"},
			expectedV6Elements: []string{"2001:db8::1", "fd00::200"},
			expectedRules: []*knftables.Rule{
				{
					Chain: nftEgressIPDropChain,
					Rule: knftables.Concat(
						"arp daddr ip @"+types.NFTEgressIPARPBlockV4,
						"drop"),
				},
				{
					Chain: nftEgressIPDropChain,
					Rule: knftables.Concat(
						"icmpv6 type nd-neighbor-solicit",
						"icmpv6 taddr @"+types.NFTEgressIPNDPBlockV6,
						"drop"),
				},
			},
			expectError: "",
		},
		{
			name:        "missing uplink name",
			egressIPs:   []string{"192.168.1.10"},
			uplinkName:  "",
			ipv4Mode:    true,
			ipv6Mode:    false,
			expectError: "uplink interface name is required for netdev nftables rules",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			config.IPv4Mode = tt.ipv4Mode
			config.IPv6Mode = tt.ipv6Mode

			nft := nodenft.SetFakeEgressIPNFTablesHelper()

			err := SetupEgressIPARPBlockNFTables(tt.egressIPs, tt.uplinkName)

			if tt.expectError != "" {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring(tt.expectError))
				return
			}

			g.Expect(err).NotTo(HaveOccurred())

			// Verify IPv4 elements
			if tt.expectedV4Elements != nil {
				v4Elements, err := nft.ListElements(context.TODO(), "set", types.NFTEgressIPARPBlockV4)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(v4Elements).To(HaveLen(len(tt.expectedV4Elements)))
				for i, expectedIP := range tt.expectedV4Elements {
					g.Expect(v4Elements[i].Key).To(Equal([]string{expectedIP}))
				}
			}

			// Verify IPv6 elements
			if tt.expectedV6Elements != nil {
				v6Elements, err := nft.ListElements(context.TODO(), "set", types.NFTEgressIPNDPBlockV6)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(v6Elements).To(HaveLen(len(tt.expectedV6Elements)))
				for i, expectedIP := range tt.expectedV6Elements {
					g.Expect(v6Elements[i].Key).To(Equal([]string{expectedIP}))
				}
			}

			// Verify rules
			if tt.expectedRules != nil {
				rules, err := nft.ListRules(context.TODO(), nftEgressIPDropChain)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(rules).To(HaveLen(len(tt.expectedRules)))
				for i, expectedRule := range tt.expectedRules {
					g.Expect(rules[i].Rule).To(Equal(expectedRule.Rule))
				}
			}
		})
	}
}

func TestCleanupEgressIPARPBlockNFTTable(t *testing.T) {
	const testUplinkName = "eth0"

	tests := []struct {
		name        string
		setupTable  bool
		egressIPs   []string
		uplinkName  string
		ipv4Mode    bool
		ipv6Mode    bool
		expectError string
	}{
		{
			name:        "delete existing table with IPv4 rules",
			setupTable:  true,
			egressIPs:   []string{"192.168.1.10"},
			uplinkName:  testUplinkName,
			ipv4Mode:    true,
			ipv6Mode:    false,
			expectError: "",
		},
		{
			name:        "delete existing table with IPv6 rules",
			setupTable:  true,
			egressIPs:   []string{"2001:db8::1"},
			uplinkName:  testUplinkName,
			ipv4Mode:    false,
			ipv6Mode:    true,
			expectError: "",
		},
		{
			name:        "delete existing table with dual-stack rules",
			setupTable:  true,
			egressIPs:   []string{"192.168.1.10", "2001:db8::1"},
			uplinkName:  testUplinkName,
			ipv4Mode:    true,
			ipv6Mode:    true,
			expectError: "",
		},
		{
			name:        "delete non-existent table",
			setupTable:  false,
			ipv4Mode:    false,
			ipv6Mode:    false,
			expectError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			config.IPv4Mode = tt.ipv4Mode
			config.IPv6Mode = tt.ipv6Mode

			nft := nodenft.SetFakeEgressIPNFTablesHelper()

			if tt.setupTable {
				// Create the table first by calling SetupEgressIPARPBlockNFTables
				err := SetupEgressIPARPBlockNFTables(tt.egressIPs, tt.uplinkName)
				g.Expect(err).NotTo(HaveOccurred())
				rules, err := nft.ListRules(context.TODO(), nftEgressIPDropChain)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(rules).NotTo(BeEmpty(), "Rules should exist before cleanup")
			}

			// Call the cleanup function
			err := CleanupEgressIPARPBlockNFTTable(context.TODO())

			if tt.expectError != "" {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring(tt.expectError))
				return
			}
			g.Expect(err).NotTo(HaveOccurred())

			_, err = nft.ListRules(context.TODO(), nftEgressIPDropChain)
			g.Expect(knftables.IsNotFound(err)).To(BeTrue(), "Chain should not exist after cleanup")
		})
	}
}
