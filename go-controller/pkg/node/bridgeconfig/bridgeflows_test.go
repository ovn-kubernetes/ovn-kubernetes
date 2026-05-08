// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package bridgeconfig

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	nodetypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func TestAdvertisedNoOverlayUDNServiceReplyFlowsRestorePacketMark(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}

	config.IPv4Mode = true
	config.IPv6Mode = true
	config.Gateway.Mode = config.GatewayModeLocal
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = true

	bridgeMAC, err := net.ParseMAC("5e:21:2c:5b:b9:d0")
	if err != nil {
		t.Fatalf("failed to parse bridge mac: %v", err)
	}

	bridge := &BridgeConfiguration{
		ofPortPhys: "1",
		ofPortHost: nodetypes.OvsLocalPort,
		ips: []*net.IPNet{
			mustParseCIDR(t, "172.18.0.2/16"),
			mustParseCIDR(t, "fc00:f853:ccd:e793::2/64"),
		},
		macAddress: bridgeMAC,
		netConfig: map[string]*BridgeUDNConfiguration{
			types.DefaultNetworkName: {
				OfPortPatch: "2",
				MasqCTMark:  nodetypes.CtMarkOVN,
				Subnets: []config.CIDRNetworkEntry{
					{CIDR: mustParseCIDR(t, "10.244.0.0/16")},
					{CIDR: mustParseCIDR(t, "fd00:10:244::/64")},
				},
			},
			"blue": {
				OfPortPatch: "11",
				PktMark:     "0x1009",
				V4MasqIPs: &udn.MasqueradeIPs{
					GatewayRouter:  mustParseCIDR(t, "169.254.0.13/17"),
					ManagementPort: mustParseCIDR(t, "169.254.0.14/17"),
				},
				V6MasqIPs: &udn.MasqueradeIPs{
					GatewayRouter:  mustParseCIDR(t, "fd69::d/112"),
					ManagementPort: mustParseCIDR(t, "fd69::e/112"),
				},
				Subnets: []config.CIDRNetworkEntry{
					{CIDR: mustParseCIDR(t, "102.102.0.0/16")},
					{CIDR: mustParseCIDR(t, "2013:100:200::/60")},
				},
				Transport: types.NetworkTransportNoOverlay,
			},
			"red": {
				OfPortPatch: "12",
				PktMark:     "0x100a",
				V4MasqIPs: &udn.MasqueradeIPs{
					GatewayRouter:  mustParseCIDR(t, "169.254.0.15/17"),
					ManagementPort: mustParseCIDR(t, "169.254.0.16/17"),
				},
				V6MasqIPs: &udn.MasqueradeIPs{
					GatewayRouter:  mustParseCIDR(t, "fd69::f/112"),
					ManagementPort: mustParseCIDR(t, "fd69::10/112"),
				},
				Subnets: []config.CIDRNetworkEntry{
					{CIDR: mustParseCIDR(t, "103.103.0.0/16")},
					{CIDR: mustParseCIDR(t, "2014:100:200::/60")},
				},
				Transport: types.NetworkTransportNoOverlay,
			},
		},
	}
	bridge.netConfig["blue"].Advertised.Store(true)

	flows, err := bridge.flowsForDefaultBridge(nil)
	if err != nil {
		t.Fatalf("failed to generate default bridge flows: %v", err)
	}

	assertContainsFlow(t, flows, fmt.Sprintf("priority=550, in_port=LOCAL, ip, ip_src=102.102.0.0/16, ip_dst=%s,actions=set_field:0x1009->pkt_mark,ct(zone=%d,nat,table=5)",
		config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP.String(), config.Default.OVNMasqConntrackZone))
	assertContainsFlow(t, flows, fmt.Sprintf("priority=550, in_port=LOCAL, ipv6, ipv6_src=2013:100:200::/60, ipv6_dst=%s,actions=set_field:0x1009->pkt_mark,ct(zone=%d,nat,table=5)",
		config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP.String(), config.Default.OVNMasqConntrackZone))
	assertNotContainsFlow(t, flows, "priority=550, in_port=LOCAL, ip, ip_src=103.103.0.0/16")
	assertNotContainsFlow(t, flows, "priority=550, in_port=LOCAL, ipv6, ipv6_src=2014:100:200::/60")
}

func assertContainsFlow(t *testing.T, flows []string, expected string) {
	t.Helper()
	for _, flow := range flows {
		if strings.Contains(flow, expected) {
			return
		}
	}
	t.Fatalf("expected flow containing %q, got:\n%s", expected, strings.Join(flows, "\n"))
}

func assertNotContainsFlow(t *testing.T, flows []string, unexpected string) {
	t.Helper()
	for _, flow := range flows {
		if strings.Contains(flow, unexpected) {
			t.Fatalf("unexpected flow containing %q: %s", unexpected, flow)
		}
	}
}

func mustParseCIDR(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("failed to parse CIDR %q: %v", cidr, err)
	}
	ipNet.IP = ip
	return ipNet
}
