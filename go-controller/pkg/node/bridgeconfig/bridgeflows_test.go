// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package bridgeconfig

import (
	"fmt"
	"net"
	"strings"
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

func TestUplinkBridgeServiceFlowsUseUDNMark(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.IPv4Mode = true
	config.IPv6Mode = true
	config.Gateway.Mode = config.GatewayModeShared
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.Kubernetes.ServiceCIDRs = []*net.IPNet{
		mustParseIPNet(t, "10.96.0.0/16"),
		mustParseIPNet(t, "fd10:96::/112"),
	}

	bridgeMAC := mustParseMAC(t, "62:41:d0:54:3d:64")
	v4NodeIP := mustParseIPNet(t, "172.28.0.3/24")
	v6NodeIP := mustParseIPNet(t, "fd28::3/64")
	v4GatewayMasqIP := mustParseIPNet(t, "169.254.0.11/32")
	v4ManagementMasqIP := mustParseIPNet(t, "169.254.0.12/32")
	v6GatewayMasqIP := mustParseIPNet(t, "fd69::11/128")
	v6ManagementMasqIP := mustParseIPNet(t, "fd69::12/128")

	bridge := &BridgeConfiguration{
		ofPortPhys: "eth1",
		ofPortHost: nodetypes.OvsLocalPort,
		ips:        []*net.IPNet{v4NodeIP, v6NodeIP},
		macAddress: bridgeMAC,
		netConfig: map[string]*BridgeUDNConfiguration{
			"bluenet": {
				OfPortPatch: "patch-ovsbr1_bluenet",
				MasqCTMark:  "0x4",
				PktMark:     "0x1001",
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

	flows, err := bridge.UplinkBridgeFlows(nil)
	if err != nil {
		t.Fatalf("failed to render bridge flows: %v", err)
	}

	expectedIPv4HostToService := fmt.Sprintf("cookie=%s, priority=500, in_port=LOCAL, ip, "+
		"pkt_mark=0x1001, ip_dst=10.96.0.0/16, "+
		"actions=ct(commit,zone=%d,nat(src=%s),table=2)",
		nodetypes.DefaultOpenFlowCookie, config.Default.HostMasqConntrackZone,
		config.Gateway.MasqueradeIPs.V4HostMasqueradeIP)
	expectedIPv4Dispatch := fmt.Sprintf("cookie=%s, priority=250, table=2, ip, pkt_mark=0x1001, "+
		"actions=set_field:%s->eth_dst,output:patch-ovsbr1_bluenet",
		nodetypes.DefaultOpenFlowCookie, bridgeMAC)
	expectedIPv4ReplyToHost := fmt.Sprintf("cookie=%s, priority=500, in_port=patch-ovsbr1_bluenet, "+
		"ip, ip_src=10.96.0.0/16, ip_dst=%s,actions=ct(zone=%d,nat,table=3)",
		nodetypes.DefaultOpenFlowCookie, config.Gateway.V4MasqueradeSubnet,
		config.Default.HostMasqConntrackZone)
	expectedIPv4ReplyToOVN := fmt.Sprintf("cookie=%s, priority=100, table=1, ip, ct_state=+trk+est, "+
		"ct_mark=0x4, actions=output:patch-ovsbr1_bluenet",
		nodetypes.DefaultOpenFlowCookie)
	expectedIPv6HostToService := fmt.Sprintf("cookie=%s, priority=500, in_port=LOCAL, ipv6, "+
		"pkt_mark=0x1001, ipv6_dst=fd10:96::/112, "+
		"actions=ct(commit,zone=%d,nat(src=%s),table=2)",
		nodetypes.DefaultOpenFlowCookie, config.Default.HostMasqConntrackZone,
		config.Gateway.MasqueradeIPs.V6HostMasqueradeIP)
	expectedIPv6Dispatch := fmt.Sprintf("cookie=%s, priority=250, table=2, ipv6, pkt_mark=0x1001, "+
		"actions=set_field:%s->eth_dst,output:patch-ovsbr1_bluenet",
		nodetypes.DefaultOpenFlowCookie, bridgeMAC)
	expectedIPv6ReplyToHost := fmt.Sprintf("cookie=%s, priority=500, in_port=patch-ovsbr1_bluenet, "+
		"ipv6, ipv6_src=fd10:96::/112, ipv6_dst=%s,actions=ct(zone=%d,nat,table=3)",
		nodetypes.DefaultOpenFlowCookie, config.Gateway.V6MasqueradeSubnet,
		config.Default.HostMasqConntrackZone)
	expectedIPv6ReplyToOVN := fmt.Sprintf("cookie=%s, priority=100, table=1, ipv6, ct_state=+trk+est, "+
		"ct_mark=0x4, actions=output:patch-ovsbr1_bluenet",
		nodetypes.DefaultOpenFlowCookie)
	expectedTable3 := fmt.Sprintf("cookie=%s, table=3,  "+
		"actions=move:NXM_OF_ETH_DST[]->NXM_OF_ETH_SRC[],set_field:%s->eth_dst,output:LOCAL",
		nodetypes.DefaultOpenFlowCookie, bridgeMAC)

	expectFlow(t, flows, expectedIPv4HostToService)
	expectFlow(t, flows, expectedIPv4Dispatch)
	expectFlow(t, flows, expectedIPv4ReplyToHost)
	expectFlow(t, flows, expectedIPv4ReplyToOVN)
	expectFlow(t, flows, expectedIPv6HostToService)
	expectFlow(t, flows, expectedIPv6Dispatch)
	expectFlow(t, flows, expectedIPv6ReplyToHost)
	expectFlow(t, flows, expectedIPv6ReplyToOVN)
	expectFlow(t, flows, expectedTable3)
	expectNoFlow(t, flows, fmt.Sprintf("priority=500, in_port=LOCAL, ip, ip_dst=10.96.0.0/16, "+
		"actions=ct(commit,zone=%d,nat(src=%s),table=2)",
		config.Default.HostMasqConntrackZone, config.Gateway.MasqueradeIPs.V4HostMasqueradeIP))
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
		"actions=ct(commit,zone=%d,nat),set_field:%s->eth_dst,output:patch-breth0_bluenet",
		nodetypes.DefaultOpenFlowCookie, config.Default.HostMasqConntrackZone, bridgeMAC)

	expectedIPv6Hairpin := fmt.Sprintf("cookie=%s, priority=100, table=4, in_port=patch-breth0_bluenet, ipv6, ipv6_dst=fd10:128::/64, "+
		"actions=ct(commit,zone=%d,nat(src=fd69::11),exec(set_field:0x4->ct_mark),table=3)",
		nodetypes.DefaultOpenFlowCookie, config.Default.OVNMasqConntrackZone)
	expectedIPv6Reply := fmt.Sprintf("cookie=%s, priority=500, in_port=LOCAL, ipv6, ipv6_dst=fd69::11,"+
		"actions=ct(zone=%d,nat,table=5)",
		nodetypes.DefaultOpenFlowCookie, config.Default.OVNMasqConntrackZone)
	expectedIPv6Table5 := fmt.Sprintf("cookie=%s, priority=100, table=5, ipv6, ct_mark=0x4, "+
		"actions=ct(commit,zone=%d,nat),set_field:%s->eth_dst,output:patch-breth0_bluenet",
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

func expectNoFlow(t *testing.T, flows []string, unexpectedSubstring string) {
	t.Helper()
	for _, flow := range flows {
		if strings.Contains(flow, unexpectedSubstring) {
			t.Fatalf("unexpected flow found:\n%s\n\nall flows:\n%v", flow, flows)
		}
	}
}
