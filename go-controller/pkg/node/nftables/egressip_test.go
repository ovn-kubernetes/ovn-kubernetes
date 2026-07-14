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
      add rule inet ovn-kubernetes ovn-kube-egress-ip oifname != ovn-k8s-mp0 return
	`
	nftRuleEgressIPSecondaryInterfaceAcceptRuleIPv4 = `
      add rule inet ovn-kubernetes ovn-kube-egress-ip ip saddr %s  accept comment "Accept egress IP pod traffic from %s"
	`
	nftRuleEgressIPSecondaryInterfaceAcceptRuleIPv6 = `
      add rule inet ovn-kubernetes ovn-kube-egress-ip ip6 saddr %s  accept comment "Accept egress IP pod traffic from %s"
	`
	nftRuleEgressIPSecondaryInterfaceAcceptRuleIPv4Counter = `
      add rule inet ovn-kubernetes ovn-kube-egress-ip ip saddr %s counter accept comment "Accept egress IP pod traffic from %s"
	`
	nftRuleEgressIPSecondaryInterfaceAcceptRuleIPv6Counter = `
      add rule inet ovn-kubernetes ovn-kube-egress-ip ip6 saddr %s counter accept comment "Accept egress IP pod traffic from %s"
	`
	nftRuleEgressIPSecondaryInterfaceMarkRuleIPv4 = `
      add rule inet ovn-kubernetes ovn-kube-egress-ip ip saddr %s  drop comment "Drop egress IP pod traffic from %s"
	`
	nftRuleEgressIPSecondaryInterfaceMarkRuleIPv6 = `
      add rule inet ovn-kubernetes ovn-kube-egress-ip ip6 saddr %s  drop comment "Drop egress IP pod traffic from %s"
	`
	nftRuleEgressIPSecondaryInterfaceMarkRuleIPv4Counter = `
      add rule inet ovn-kubernetes ovn-kube-egress-ip ip saddr %s counter drop comment "Drop egress IP pod traffic from %s"
	`
	nftRuleEgressIPSecondaryInterfaceMarkRuleIPv6Counter = `
      add rule inet ovn-kubernetes ovn-kube-egress-ip ip6 saddr %s counter drop comment "Drop egress IP pod traffic from %s"
	`
)

func TestInitEgressIPNFTChain(t *testing.T) {
	for _, tc := range []struct {
		name          string
		v4            bool
		v6            bool
		v4CIDRs       []string
		v6CIDRs       []string
		nodeV4Subnets []string
		nodeV6Subnets []string
		gatewayMode   ovnconfig.GatewayMode
		logLevel      int
		expectInitErr string
	}{
		{
			name:          "LGW dual-stack single CIDRs",
			v4:            true,
			v6:            true,
			v4CIDRs:       []string{"5.5.5.0/24"},
			v6CIDRs:       []string{"2001:0:0:1::/64"},
			nodeV4Subnets: []string{"5.5.5.0/26"},
			nodeV6Subnets: []string{"2001:0:0:1::/80"},
			gatewayMode:   ovnconfig.GatewayModeLocal,
		},
		{
			name:          "LGW IPv4 only",
			v4:            true,
			v6:            false,
			v4CIDRs:       []string{"10.128.0.0/14"},
			nodeV4Subnets: []string{"10.128.0.0/24"},
			gatewayMode:   ovnconfig.GatewayModeLocal,
		},
		{
			name:          "LGW IPv6 only",
			v4:            false,
			v6:            true,
			v6CIDRs:       []string{"fd00:10:244::/48"},
			nodeV6Subnets: []string{"fd00:10:244::/64"},
			gatewayMode:   ovnconfig.GatewayModeLocal,
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
			name:          "LGW multiple IPv4 CIDRs",
			v4:            true,
			v6:            false,
			v4CIDRs:       []string{"10.128.0.0/14", "10.132.0.0/14"},
			nodeV4Subnets: []string{"10.128.0.0/24"},
			gatewayMode:   ovnconfig.GatewayModeLocal,
		},
		{
			name:          "LGW multiple IPv6 CIDRs",
			v4:            false,
			v6:            true,
			v6CIDRs:       []string{"fd00:10:244::/48", "fd00:10:245::/48"},
			nodeV6Subnets: []string{"fd00:10:244::/64"},
			gatewayMode:   ovnconfig.GatewayModeLocal,
		},
		{
			name:          "LGW dual-stack multiple CIDRs",
			v4:            true,
			v6:            true,
			v4CIDRs:       []string{"10.128.0.0/14", "10.132.0.0/14"},
			v6CIDRs:       []string{"fd00:10:244::/48", "fd00:10:245::/48"},
			nodeV4Subnets: []string{"10.128.0.0/24"},
			nodeV6Subnets: []string{"fd00:10:244::/64"},
			gatewayMode:   ovnconfig.GatewayModeLocal,
		},
		{
			name: "Both protocols disabled creates chain only",
			v4:   false,
			v6:   false,
		},
		{
			name:          "LGW debug logging enables counter in rules",
			v4:            true,
			v6:            true,
			v4CIDRs:       []string{"10.0.0.0/16"},
			v6CIDRs:       []string{"fd00::/112"},
			nodeV4Subnets: []string{"10.0.0.0/24"},
			nodeV6Subnets: []string{"fd00::/120"},
			gatewayMode:   ovnconfig.GatewayModeLocal,
			logLevel:      5,
		},
		{
			name:          "SGW dual-stack skips accept rules",
			v4:            true,
			v6:            true,
			v4CIDRs:       []string{"10.128.0.0/14"},
			v6CIDRs:       []string{"fd00:10:244::/48"},
			nodeV4Subnets: []string{"10.128.0.0/24"},
			nodeV6Subnets: []string{"fd00:10:244::/64"},
			gatewayMode:   ovnconfig.GatewayModeShared,
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

			savedGatewayMode := ovnconfig.Gateway.Mode
			ovnconfig.Gateway.Mode = tc.gatewayMode
			defer func() { ovnconfig.Gateway.Mode = savedGatewayMode }()

			var nodeSubnets []*net.IPNet
			for _, cidrStr := range tc.nodeV4Subnets {
				_, cidr, _ := net.ParseCIDR(cidrStr)
				nodeSubnets = append(nodeSubnets, cidr)
			}
			for _, cidrStr := range tc.nodeV6Subnets {
				_, cidr, _ := net.ParseCIDR(cidrStr)
				nodeSubnets = append(nodeSubnets, cidr)
			}

			err := InitEgressIPNFTChain(tc.v4, tc.v6, nodeSubnets)
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
			useCounter := tc.logLevel > 1
			// Rule 0: accept node pod CIDRs (only in local gateway mode)
			if tc.gatewayMode == ovnconfig.GatewayModeLocal {
				if tc.v4 {
					tmpl := nftRuleEgressIPSecondaryInterfaceAcceptRuleIPv4
					if useCounter {
						tmpl = nftRuleEgressIPSecondaryInterfaceAcceptRuleIPv4Counter
					}
					for _, cidr := range tc.nodeV4Subnets {
						expectedChain += "\n" + fmt.Sprintf(tmpl, cidr, cidr)
					}
				}
				if tc.v6 {
					tmpl := nftRuleEgressIPSecondaryInterfaceAcceptRuleIPv6
					if useCounter {
						tmpl = nftRuleEgressIPSecondaryInterfaceAcceptRuleIPv6Counter
					}
					for _, cidr := range tc.nodeV6Subnets {
						expectedChain += "\n" + fmt.Sprintf(tmpl, cidr, cidr)
					}
				}
			}
			// Rule 1: drop cluster pod CIDRs
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
