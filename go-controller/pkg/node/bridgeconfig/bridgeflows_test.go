// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package bridgeconfig

import (
	"fmt"
	"net"
	"testing"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	udngenerator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	nodetypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func TestSharedNoOverlayNodeIPFlowUsesNATInDefaultConntrackZone(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.IPv4Mode = true
	config.IPv6Mode = true
	config.Gateway.Mode = config.GatewayModeShared

	bridgeMAC := mustParseMAC(t, "62:41:d0:54:3d:64")
	v4NodeIP := mustParseIPNet(t, "172.18.0.3/24")
	v6NodeIP := mustParseIPNet(t, "fd00::3/64")
	v4GatewayMasqIP := mustParseIPNet(t, "169.254.0.11/32")
	v4ManagementMasqIP := mustParseIPNet(t, "169.254.0.12/32")
	v6GatewayMasqIP := mustParseIPNet(t, "fd69::11/128")
	v6ManagementMasqIP := mustParseIPNet(t, "fd69::12/128")

	bridge := &BridgeConfiguration{
		ofPortPhys: "eth0",
		ofPortHost: nodetypes.OvsLocalPort,
		ips:        []*net.IPNet{v4NodeIP, v6NodeIP},
		macAddress: bridgeMAC,
		netConfig: map[string]*BridgeUDNConfiguration{
			types.DefaultNetworkName: {
				OfPortPatch: "patch-breth0_ov",
				MasqCTMark:  nodetypes.CtMarkOVN,
			},
			"bluenet": {
				OfPortPatch: "patch-breth0_bluenet",
				MasqCTMark:  "0x4",
				PktMark:     "0x3",
				Transport:   types.NetworkTransportNoOverlay,
				V4MasqIPs: &udngenerator.MasqueradeIPs{
					GatewayRouter:  v4GatewayMasqIP,
					ManagementPort: v4ManagementMasqIP,
				},
				V6MasqIPs: &udngenerator.MasqueradeIPs{
					GatewayRouter:  v6GatewayMasqIP,
					ManagementPort: v6ManagementMasqIP,
				},
			},
		},
	}

	flows, err := bridge.commonFlows(nil)
	if err != nil {
		t.Fatalf("failed to render bridge flows: %v", err)
	}

	expectedIPv4 := fmt.Sprintf("cookie=%s, priority=99, in_port=patch-breth0_bluenet, dl_src=%s, ip, nw_src=172.18.0.3, "+
		"actions=ct(commit, zone=%d, nat(src=172.18.0.3), exec(set_field:0x4->ct_mark)), output:eth0",
		nodetypes.DefaultOpenFlowCookie, bridgeMAC, config.Default.ConntrackZone)
	expectedIPv6 := fmt.Sprintf("cookie=%s, priority=99, in_port=patch-breth0_bluenet, dl_src=%s, ipv6, ipv6_src=fd00::3, "+
		"actions=ct(commit, zone=%d, nat(src=fd00::3), exec(set_field:0x4->ct_mark)), output:eth0",
		nodetypes.DefaultOpenFlowCookie, bridgeMAC, config.Default.ConntrackZone)

	expectFlow(t, flows, expectedIPv4)
	expectFlow(t, flows, expectedIPv6)
}

func TestLocalNoOverlayServiceHairpinUsesUDNGatewayMasqueradeIP(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.IPv4Mode = true
	config.IPv6Mode = true
	config.Gateway.Mode = config.GatewayModeLocal

	bridgeMAC := mustParseMAC(t, "62:41:d0:54:3d:64")
	v4NodeIP := mustParseIPNet(t, "172.18.0.3/24")
	v6NodeIP := mustParseIPNet(t, "fd00::3/64")
	v4GatewayMasqIP := mustParseIPNet(t, "169.254.0.11/32")
	v4ManagementMasqIP := mustParseIPNet(t, "169.254.0.12/32")
	v6GatewayMasqIP := mustParseIPNet(t, "fd69::11/128")
	v6ManagementMasqIP := mustParseIPNet(t, "fd69::12/128")

	bridge := &BridgeConfiguration{
		ofPortHost: nodetypes.OvsLocalPort,
		ips:        []*net.IPNet{v4NodeIP, v6NodeIP},
		macAddress: bridgeMAC,
		netConfig: map[string]*BridgeUDNConfiguration{
			types.DefaultNetworkName: {
				OfPortPatch: "patch-breth0_ov",
				MasqCTMark:  nodetypes.CtMarkOVN,
			},
			"bluenet": {
				OfPortPatch: "patch-breth0_bluenet",
				MasqCTMark:  "0x4",
				PktMark:     "0x3",
				Transport:   types.NetworkTransportNoOverlay,
				Subnets: []config.CIDRNetworkEntry{
					{CIDR: mustParseIPNet(t, "10.128.0.0/16")},
					{CIDR: mustParseIPNet(t, "fd10:128::/64")},
				},
				V4MasqIPs: &udngenerator.MasqueradeIPs{
					GatewayRouter:  v4GatewayMasqIP,
					ManagementPort: v4ManagementMasqIP,
				},
				V6MasqIPs: &udngenerator.MasqueradeIPs{
					GatewayRouter:  v6GatewayMasqIP,
					ManagementPort: v6ManagementMasqIP,
				},
			},
		},
	}

	flows, err := bridge.flowsForDefaultBridge(nil)
	if err != nil {
		t.Fatalf("failed to render bridge flows: %v", err)
	}

	expectedIPv4Hairpin := fmt.Sprintf("cookie=%s, priority=100, table=4, in_port=patch-breth0_bluenet, ip, ip_dst=10.128.0.0/16, "+
		"actions=ct(commit,zone=%d,nat(src=169.254.0.11),exec(set_field:0x4->ct_mark),table=3)",
		nodetypes.DefaultOpenFlowCookie, config.Default.OVNMasqConntrackZone)
	expectedIPv4Reply := fmt.Sprintf("cookie=%s, priority=500, in_port=LOCAL, ip, ip_dst=169.254.0.11,"+
		"actions=ct(zone=%d,nat,table=5)",
		nodetypes.DefaultOpenFlowCookie, config.Default.OVNMasqConntrackZone)
	expectedIPv4Table5 := fmt.Sprintf("cookie=%s, priority=100, table=5, ip, ct_mark=0x4, "+
		"actions=ct(commit,zone=%d,nat,exec(set_field:0x4->ct_mark)),set_field:%s->eth_dst,output:patch-breth0_bluenet",
		nodetypes.DefaultOpenFlowCookie, config.Default.HostMasqConntrackZone, bridgeMAC)

	expectedIPv6Hairpin := fmt.Sprintf("cookie=%s, priority=100, table=4, in_port=patch-breth0_bluenet, ipv6, ipv6_dst=fd10:128::/64, "+
		"actions=ct(commit,zone=%d,nat(src=fd69::11),exec(set_field:0x4->ct_mark),table=3)",
		nodetypes.DefaultOpenFlowCookie, config.Default.OVNMasqConntrackZone)
	expectedIPv6Reply := fmt.Sprintf("cookie=%s, priority=500, in_port=LOCAL, ipv6, ipv6_dst=fd69::11,"+
		"actions=ct(zone=%d,nat,table=5)",
		nodetypes.DefaultOpenFlowCookie, config.Default.OVNMasqConntrackZone)
	expectedIPv6Table5 := fmt.Sprintf("cookie=%s, priority=100, table=5, ipv6, ct_mark=0x4, "+
		"actions=ct(commit,zone=%d,nat,exec(set_field:0x4->ct_mark)),set_field:%s->eth_dst,output:patch-breth0_bluenet",
		nodetypes.DefaultOpenFlowCookie, config.Default.HostMasqConntrackZone, bridgeMAC)

	expectFlow(t, flows, expectedIPv4Hairpin)
	expectFlow(t, flows, expectedIPv4Reply)
	expectFlow(t, flows, expectedIPv4Table5)
	expectFlow(t, flows, expectedIPv6Hairpin)
	expectFlow(t, flows, expectedIPv6Reply)
	expectFlow(t, flows, expectedIPv6Table5)
}

func mustParseMAC(t *testing.T, value string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(value)
	if err != nil {
		t.Fatalf("failed to parse MAC %q: %v", value, err)
	}
	return mac
}

func mustParseIPNet(t *testing.T, value string) *net.IPNet {
	t.Helper()
	ip, ipNet, err := net.ParseCIDR(value)
	if err != nil {
		t.Fatalf("failed to parse CIDR %q: %v", value, err)
	}
	ipNet.IP = ip
	return ipNet
}

func expectFlow(t *testing.T, flows []string, expected string) {
	t.Helper()
	for _, flow := range flows {
		if flow == expected {
			return
		}
	}
	t.Fatalf("expected flow not found:\n%s\n\nall flows:\n%v", expected, flows)
}
