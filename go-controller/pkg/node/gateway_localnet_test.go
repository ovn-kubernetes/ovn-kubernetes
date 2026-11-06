// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package node

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/knftables"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type localGatewayTestManagementPort struct{ addresses []*net.IPNet }

func (p localGatewayTestManagementPort) GetAddresses() []*net.IPNet { return p.addresses }
func (p localGatewayTestManagementPort) GetInterfaceName() string   { return types.K8sMgmtIntfName }

func TestLocalGatewayMigratedVMSNAT(t *testing.T) {
	require.NoError(t, config.PrepareTestConfig())
	t.Cleanup(func() { require.NoError(t, config.PrepareTestConfig()) })
	config.Gateway.Mode = config.GatewayModeLocal
	config.IPv4Mode, config.IPv6Mode = true, true
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
		{CIDR: ovntest.MustParseIPNet("10.128.0.0/16"), HostSubnetLength: 24},
		{CIDR: ovntest.MustParseIPNet("10.129.0.0/16"), HostSubnetLength: 24},
		{CIDR: ovntest.MustParseIPNet("fd00::/48"), HostSubnetLength: 64},
	}
	nodeSubnets := ovntest.MustParseIPNets("10.128.1.0/24", "fd00:0:0:1::/64")
	subnets := getLocalGatewaySNATSubnets()
	require.Len(t, subnets, 3)
	// Retained addresses are outside this node's subnets but must be masqueraded.
	for _, address := range []string{"10.128.2.9", "10.129.3.9", "fd00:0:0:2::9"} {
		covered := false
		for _, subnet := range subnets {
			covered = covered || subnet.Contains(net.ParseIP(address))
		}
		require.True(t, covered, "retained VM address %s lacks SNAT", address)
	}
	nft := nodenft.SetFakeNFTablesHelper()
	bridge := bridgeconfig.TestDefaultBridgeConfig()
	gw := &gateway{
		nodeIPManager: &addressManager{mgmtPort: localGatewayTestManagementPort{
			addresses: ovntest.MustParseIPNets("10.128.1.2/24", "fd00:0:0:1::2/64"),
		}},
		openflowManager: &openflowManager{defaultBridge: newOpenflowBridge(bridge)},
	}
	tx := nft.NewTransaction()
	tx.Add(&knftables.Set{Name: types.NFTRemoteNodeIPsv4, Type: "ipv4_addr"})
	tx.Add(&knftables.Set{Name: types.NFTRemoteNodeIPsv6, Type: "ipv6_addr"})
	require.NoError(t, nft.Run(context.Background(), tx))
	require.NoError(t, initLocalGatewayNFTNATRules(false, nodeSubnets...))
	util.SetFakeIPTablesHelpers()
	require.NoError(t, initLocalGateway(gw.nodeIPManager.mgmtPort, false))
	require.Len(t, nft.Table.Chains[nftablesPodSubnetMasqChain].Rules, 3)
	require.Equal(t, "ip saddr 10.128.0.0/16 masquerade", nft.Table.Chains[nftablesPodSubnetMasqChain].Rules[0].Rule)
	require.Len(t, nft.Table.Chains[nftablesLocalGatewayMasqChain].Rules, 3) // one host rule per family and the pod-chain jump
	// Reconciliation replaces old host-subnet rules rather than accumulating them.
	require.NoError(t, addOrUpdateLocalGatewayPodSubnetNFTRules(false, nodeSubnets...))
	require.NoError(t, gw.updateSNATRules())
	for range 2 {
		require.NoError(t, gw.updateSNATRules())
		rules := nft.Table.Chains[nftablesPodSubnetMasqChain].Rules
		require.Len(t, rules, 3)
		require.Equal(t, "ip saddr 10.128.0.0/16 masquerade", rules[0].Rule)
		require.Equal(t, "ip saddr 10.129.0.0/16 masquerade", rules[1].Rule)
		require.Equal(t, "ip6 saddr fd00::/48 masquerade", rules[2].Rule)
	}
	// EgressIP/EgressService source NAT at priority 100 must still run first.
	require.Equal(t, "101", string(*nft.Table.Chains[nftablesLocalGatewayMasqChain].Priority))
	bridge.GetNetworkConfig(types.DefaultNetworkName).Advertised.Store(true)
	require.NoError(t, gw.updateSNATRules())
	rules := nft.Table.Chains[nftablesPodSubnetMasqChain].Rules
	require.Len(t, rules, 3)
	require.Equal(t, "ip saddr 10.128.0.0/16 ip daddr @"+types.NFTRemoteNodeIPsv4+" masquerade", rules[0].Rule)
	require.Equal(t, "ip6 saddr fd00::/48 ip6 daddr @"+types.NFTRemoteNodeIPsv6+" masquerade", rules[2].Rule)
	for _, transport := range []string{"", types.NetworkTransportNoOverlay} {
		config.Default.Transport = transport
		require.Equal(t, subnets, getLocalGatewaySNATSubnets(), "transport %s must cover retained VM addresses", transport)
		require.NoError(t, gw.updateSNATRules())
		require.Equal(t, "ip saddr 10.128.0.0/16 ip daddr @"+types.NFTRemoteNodeIPsv4+" masquerade", nft.Table.Chains[nftablesPodSubnetMasqChain].Rules[0].Rule)
	}
	config.Gateway.Mode = config.GatewayModeShared
	require.NoError(t, gw.updateSNATRules())
	require.Empty(t, nft.Table.Chains[nftablesPodSubnetMasqChain].Rules)
}

func TestGatewayInitSNATModeTransitions(t *testing.T) {
	require.NoError(t, config.PrepareTestConfig())
	t.Cleanup(func() { require.NoError(t, config.PrepareTestConfig()) })
	config.IPv4Mode, config.IPv6Mode = true, true
	config.Gateway.NodeportEnable = false
	// Startup must reconcile SNAT without an EgressIP sync or UDN event.
	config.OVNKubernetesFeature.EnableEgressIP = false
	config.OVNKubernetesFeature.EnableMultiNetwork = false
	config.OVNKubernetesFeature.EnableNetworkSegmentation = false
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
		{CIDR: ovntest.MustParseIPNet("10.128.0.0/16"), HostSubnetLength: 24},
		{CIDR: ovntest.MustParseIPNet("fd00::/48"), HostSubnetLength: 64},
	}
	nodeSubnets := ovntest.MustParseIPNets("10.128.1.0/24", "fd00:0:0:1::/64")
	nft := nodenft.SetFakeNFTablesHelper()
	util.SetFakeIPTablesHelpers()
	// Preserve host state across restarts, starting with pre-upgrade SNAT rules.
	require.NoError(t, initLocalGatewayNFTNATRules(false, nodeSubnets...))
	for _, mode := range []config.GatewayMode{
		config.GatewayModeShared, config.GatewayModeLocal, config.GatewayModeLocal,
		config.GatewayModeShared, config.GatewayModeLocal,
	} {
		t.Run(string(mode), func(t *testing.T) {
			config.Gateway.Mode = mode
			wf, err := factory.NewNodeWatchFactory(&util.OVNNodeClientset{KubeClient: fake.NewSimpleClientset()}, "node")
			require.NoError(t, err)
			stop := make(chan struct{})
			wg := &sync.WaitGroup{}
			t.Cleanup(func() {
				close(stop)
				wg.Wait()
				wf.Shutdown()
			})
			require.NoError(t, wf.Start())
			gw := &gateway{
				watchFactory: wf,
				nodeIPManager: &addressManager{mgmtPort: localGatewayTestManagementPort{
					addresses: ovntest.MustParseIPNets("10.128.1.2/24", "fd00:0:0:1::2/64"),
				}},
				openflowManager: &openflowManager{defaultBridge: newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig())},
			}
			if mode == config.GatewayModeLocal {
				// The local-mode pre-start path creates the base chains before Init.
				require.NoError(t, initLocalGateway(gw.nodeIPManager.mgmtPort, false))
			}
			require.NoError(t, gw.Init(stop, wg))
			rules := nft.Table.Chains[nftablesPodSubnetMasqChain].Rules
			if mode == config.GatewayModeShared {
				require.Empty(t, rules, "shared startup must remove local pod SNAT")
			} else {
				require.Len(t, rules, 2)
				require.Equal(t, "ip saddr 10.128.0.0/16 masquerade", rules[0].Rule)
				require.Equal(t, "ip6 saddr fd00::/48 masquerade", rules[1].Rule)
				require.Equal(t, "101", string(*nft.Table.Chains[nftablesLocalGatewayMasqChain].Priority))
			}
		})
	}
}
