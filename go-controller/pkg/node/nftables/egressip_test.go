// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package nftables

import (
	"fmt"
	"net"
	"testing"

	ovnconfig "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
)

var (
	nftRuleEgressIPSecondaryInterfaceAnnotation = `
      add table inet ovn-kubernetes
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

func TestInitEgressIPNFTChain(t *testing.T) {
	for _, tc := range []struct {
		name          string
		v4            bool
		v6            bool
		v4CIDRs       []string
		v6CIDRs       []string
		logLevel      int
		expectInitErr string
	}{
		{
			name:    "Dual-stack single CIDRs",
			v4:      true,
			v6:      true,
			v4CIDRs: []string{"5.5.5.0/24"},
			v6CIDRs: []string{"2001:0:0:1::/64"},
		},
		{
			name:    "IPv4 only",
			v4:      true,
			v6:      false,
			v4CIDRs: []string{"10.128.0.0/14"},
		},
		{
			name:    "IPv6 only",
			v4:      false,
			v6:      true,
			v6CIDRs: []string{"fd00:10:244::/48"},
		},
		{
			name:          "IPv4 enabled but no CIDRs configured",
			v4:            true,
			v6:            false,
			v4CIDRs:       nil,
			expectInitErr: "IPv4 enabled but no cluster pod CIDRs configured",
		},
		{
			name:          "IPv6 enabled but no CIDRs configured",
			v4:            false,
			v6:            true,
			v6CIDRs:       nil,
			expectInitErr: "IPv6 enabled but no cluster pod CIDRs configured",
		},
		{
			name:    "Multiple IPv4 CIDRs",
			v4:      true,
			v6:      false,
			v4CIDRs: []string{"10.128.0.0/14", "10.132.0.0/14"},
		},
		{
			name:    "Multiple IPv6 CIDRs",
			v4:      false,
			v6:      true,
			v6CIDRs: []string{"fd00:10:244::/48", "fd00:10:245::/48"},
		},
		{
			name:    "Dual-stack multiple CIDRs",
			v4:      true,
			v6:      true,
			v4CIDRs: []string{"10.128.0.0/14", "10.132.0.0/14"},
			v6CIDRs: []string{"fd00:10:244::/48", "fd00:10:245::/48"},
		},
		{
			name: "Both protocols disabled creates chain only",
			v4:   false,
			v6:   false,
		},
		{
			name:     "Debug logging enables counter in rules",
			v4:       true,
			v6:       true,
			v4CIDRs:  []string{"10.0.0.0/16"},
			v6CIDRs:  []string{"fd00::/112"},
			logLevel: 5,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := SetFakeNFTablesHelper()

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

			err := InitEgressIPNFTChain(tc.v4, tc.v6)
			if tc.expectInitErr != "" {
				if err == nil {
					t.Fatalf("expected error %q but got nil", tc.expectInitErr)
				}
				if err.Error() != tc.expectInitErr {
					t.Fatalf("expected error %q but got %q", tc.expectInitErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			expectedChain := nftRuleEgressIPSecondaryInterfaceAnnotation
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

			if err = MatchNFTRules(expectedChain, fake.Dump()); err != nil {
				t.Fatalf("expected egressIP NFT rules to match: %v", err)
			}
		})
	}
}
