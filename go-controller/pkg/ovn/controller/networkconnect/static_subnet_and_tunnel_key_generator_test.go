package networkconnect

import (
	"math/big"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func TestIPToBigInt(t *testing.T) {
	tests := []struct {
		name     string
		ip       net.IP
		expected *big.Int
	}{
		{
			name:     "192.168.0.1",
			ip:       net.ParseIP("192.168.0.1"),
			expected: big.NewInt(0xC0A80001), // 192*2^24 + 168*2^16 + 0*2^8 + 1
		},
		{
			name:     "10.0.0.0",
			ip:       net.ParseIP("10.0.0.0"),
			expected: big.NewInt(0x0A000000), // 10*2^24
		},
		{
			name:     "255.255.255.255",
			ip:       net.ParseIP("255.255.255.255"),
			expected: big.NewInt(0xFFFFFFFF),
		},
		{
			name:     "0.0.0.0",
			ip:       net.ParseIP("0.0.0.0"),
			expected: big.NewInt(0),
		},
		{
			name:     "192.168.5.0",
			ip:       net.ParseIP("192.168.5.0"),
			expected: big.NewInt(0xC0A80500), // 192*2^24 + 168*2^16 + 5*2^8
		},
		{
			name:     "IPv6 address fd00::1",
			ip:       net.ParseIP("fd00::1"),
			expected: new(big.Int).SetBytes(net.ParseIP("fd00::1").To16()),
		},
		{
			name:     "IPv6 address 2001:db8::1",
			ip:       net.ParseIP("2001:db8::1"),
			expected: new(big.Int).SetBytes(net.ParseIP("2001:db8::1").To16()),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ipToBigInt(tt.ip)
			assert.Equal(t, 0, tt.expected.Cmp(result), "expected %s, got %s", tt.expected.String(), result.String())
		})
	}
}

func TestGetLayer2SubIndex(t *testing.T) {
	tests := []struct {
		name     string
		subnets  []*net.IPNet
		expected int
	}{
		{
			name:     "empty subnets",
			subnets:  []*net.IPNet{},
			expected: 0,
		},
		{
			name: "IPv4 /31 at start",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/31"),
			},
			expected: 0,
		},
		{
			name: "IPv4 /31 at offset 2",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.2/31"),
			},
			expected: 1,
		},
		{
			name: "IPv4 /31 at offset 10",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.10/31"),
			},
			expected: 5,
		},
		{
			name: "IPv6 /127 at start",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("fd00::0/127"),
			},
			expected: 0,
		},
		{
			name: "IPv6 /127 at offset 4",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("fd00::4/127"),
			},
			expected: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getLayer2SubIndex(tt.subnets)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestCalculateP2PSubnets(t *testing.T) {
	tests := []struct {
		name           string
		subnets        []*net.IPNet
		nodeID         int
		expectedSubnet string
		expectError    bool
	}{
		{
			name: "IPv4 node ID 0",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/24"),
			},
			nodeID:         0,
			expectedSubnet: "192.168.0.0/31",
			expectError:    false,
		},
		{
			name: "IPv4 node ID 1",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/24"),
			},
			nodeID:         1,
			expectedSubnet: "192.168.0.2/31",
			expectError:    false,
		},
		{
			name: "IPv4 node ID 5",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/24"),
			},
			nodeID:         5,
			expectedSubnet: "192.168.0.10/31",
			expectError:    false,
		},
		{
			name: "IPv6 node ID 0",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("fd00::/64"),
			},
			nodeID:         0,
			expectedSubnet: "fd00::/127",
			expectError:    false,
		},
		{
			name: "IPv6 node ID 1",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("fd00::/64"),
			},
			nodeID:         1,
			expectedSubnet: "fd00::2/127",
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := calculateP2PSubnets(tt.subnets, tt.nodeID)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Len(t, result, 1)
				assert.Equal(t, tt.expectedSubnet, result[0].String())
			}
		})
	}
}

func TestGetP2PIPs(t *testing.T) {
	tests := []struct {
		name           string
		subnets        []*net.IPNet
		expectedFirst  []string
		expectedSecond []string
	}{
		{
			name: "single IPv4 /31 subnet",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/31"),
			},
			expectedFirst:  []string{"192.168.0.0/31"},
			expectedSecond: []string{"192.168.0.1/31"},
		},
		{
			name: "single IPv4 /31 subnet at offset",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.10/31"),
			},
			expectedFirst:  []string{"192.168.0.10/31"},
			expectedSecond: []string{"192.168.0.11/31"},
		},
		{
			name: "single IPv6 /127 subnet",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("fd00::0/127"),
			},
			expectedFirst:  []string{"fd00::/127"},
			expectedSecond: []string{"fd00::1/127"},
		},
		{
			name: "dual stack subnets",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/31"),
				ovntest.MustParseIPNet("fd00::0/127"),
			},
			expectedFirst:  []string{"192.168.0.0/31", "fd00::/127"},
			expectedSecond: []string{"192.168.0.1/31", "fd00::1/127"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first, second := getP2PIPs(tt.subnets)
			require.Len(t, first, len(tt.expectedFirst))
			require.Len(t, second, len(tt.expectedSecond))

			for i, expected := range tt.expectedFirst {
				assert.Equal(t, expected, first[i].String())
			}
			for i, expected := range tt.expectedSecond {
				assert.Equal(t, expected, second[i].String())
			}
		})
	}
}

func TestGetNetworkIndexAndMaxNodes(t *testing.T) {
	tests := []struct {
		name                 string
		connectSubnets       []networkconnectv1.ConnectSubnet
		subnets              []*net.IPNet
		expectedNetworkIndex int
		expectedMaxNodes     int
	}{
		{
			name: "IPv4 /16 CIDR with /24 prefix, first network",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{
					CIDR:          "192.168.0.0/16",
					NetworkPrefix: 24,
				},
			},
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/24"),
			},
			expectedNetworkIndex: 0,
			expectedMaxNodes:     256, // 2^(32-24) = 256
		},
		{
			name: "IPv4 /16 CIDR with /24 prefix, fifth network",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{
					CIDR:          "192.168.0.0/16",
					NetworkPrefix: 24,
				},
			},
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.5.0/24"),
			},
			expectedNetworkIndex: 5,
			expectedMaxNodes:     256,
		},
		{
			name: "IPv4 /16 CIDR with /20 prefix",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{
					CIDR:          "10.0.0.0/16",
					NetworkPrefix: 20,
				},
			},
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("10.0.0.0/20"),
			},
			expectedNetworkIndex: 0,
			expectedMaxNodes:     4096, // 2^(32-20) = 4096
		},
		{
			name: "IPv4 with large maxNodes capped at 5000",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{
					CIDR:          "10.0.0.0/8",
					NetworkPrefix: 16,
				},
			},
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("10.0.0.0/16"),
			},
			expectedNetworkIndex: 0,
			expectedMaxNodes:     5000, // 2^(32-16) = 65536, but capped at 5000
		},
		{
			name: "IPv6 /112 CIDR with /120 prefix, first network",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{
					CIDR:          "fd00::/112",
					NetworkPrefix: 120,
				},
			},
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("fd00::/120"),
			},
			expectedNetworkIndex: 0,
			expectedMaxNodes:     256, // 2^(128-120) = 256
		},
		{
			name: "IPv6 /112 CIDR with /120 prefix, fifth network",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{
					CIDR:          "fd00::/112",
					NetworkPrefix: 120,
				},
			},
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("fd00::500/120"),
			},
			expectedNetworkIndex: 5,
			expectedMaxNodes:     256,
		},
		{
			name: "IPv6 /64 CIDR with /72 prefix",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{
					CIDR:          "2001:db8::/64",
					NetworkPrefix: 72,
				},
			},
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("2001:db8::/72"),
			},
			expectedNetworkIndex: 0,
			expectedMaxNodes:     5000, // 2^(128-72) would be huge, capped at 5000
		},
		{
			name: "IPv6 /64 CIDR with /72 prefix, third network",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{
					CIDR:          "2001:db8::/64",
					NetworkPrefix: 72,
				},
			},
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("2001:db8::300:0:0:0/72"),
			},
			expectedNetworkIndex: 3,
			expectedMaxNodes:     5000,
		},
		{
			name: "DualStack with consistent prefix (32-24 == 128-120)",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{
					CIDR:          "192.168.0.0/16",
					NetworkPrefix: 24,
				},
				{
					CIDR:          "fd00:192:168::/112",
					NetworkPrefix: 120,
				},
			},
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.2.0/24"),
				ovntest.MustParseIPNet("fd00:192:168::200/120"),
			},
			// networkIndex is calculated from first matching subnet (IPv4: 192.168.2.0 is index 2)
			// maxNodes = 2^(32-24) = 256 (same as 2^(128-120) due to CEL validation)
			expectedNetworkIndex: 2,
			expectedMaxNodes:     256,
		},
		{
			name: "DualStack with larger prefix, fifth network",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{
					CIDR:          "10.0.0.0/8",
					NetworkPrefix: 16,
				},
				{
					CIDR:          "fd00:10::/48",
					NetworkPrefix: 56,
				},
			},
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("10.5.0.0/16"),
				ovntest.MustParseIPNet("fd00:10:500::/56"),
			},
			// networkIndex from IPv4: 10.5.0.0 is index 5 within 10.0.0.0/8
			// maxNodes = 2^(32-16) = 65536 capped at 5000
			expectedNetworkIndex: 5,
			expectedMaxNodes:     5000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			networkIndex, maxNodes := getNetworkIndexAndMaxNodes(tt.connectSubnets, tt.subnets)
			assert.Equal(t, tt.expectedNetworkIndex, networkIndex, "networkIndex mismatch")
			assert.Equal(t, tt.expectedMaxNodes, maxNodes, "maxNodes mismatch")
		})
	}
}

func TestGetTunnelKey(t *testing.T) {
	tests := []struct {
		name              string
		connectSubnets    []networkconnectv1.ConnectSubnet
		allocatedSubnets  []*net.IPNet
		topologyType      string
		nodeID            int
		expectedTunnelKey int
	}{
		// Layer3 tests
		{
			name: "Layer3 IPv4, first network, node 1",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("192.168.0.0/24")},
			topologyType:      ovntypes.Layer3Topology,
			nodeID:            1,
			expectedTunnelKey: 0*256 + 1 + 1, // networkIndex=0, maxNodes=256, nodeID=1 -> 2
		},
		{
			name: "Layer3 IPv4, first network, node 5",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("192.168.0.0/24")},
			topologyType:      ovntypes.Layer3Topology,
			nodeID:            5,
			expectedTunnelKey: 0*256 + 5 + 1, // networkIndex=0, maxNodes=256, nodeID=5 -> 6
		},
		{
			name: "Layer3 IPv4, third network, node 1",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("192.168.3.0/24")},
			topologyType:      ovntypes.Layer3Topology,
			nodeID:            1,
			expectedTunnelKey: 3*256 + 1 + 1, // networkIndex=3, maxNodes=256, nodeID=1 -> 770
		},
		{
			name: "Layer3 IPv6, second network, node 2",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "fd00::/112", NetworkPrefix: 120},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("fd00::200/120")},
			topologyType:      ovntypes.Layer3Topology,
			nodeID:            2,
			expectedTunnelKey: 2*256 + 2 + 1, // networkIndex=2, maxNodes=256, nodeID=2 -> 515
		},
		{
			name: "Layer3 DualStack, fifth network, node 3",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
				{CIDR: "fd00:192:168::/112", NetworkPrefix: 120},
			},
			allocatedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.5.0/24"),
				ovntest.MustParseIPNet("fd00:192:168::500/120"),
			},
			topologyType:      ovntypes.Layer3Topology,
			nodeID:            3,
			expectedTunnelKey: 5*256 + 3 + 1, // networkIndex=5, maxNodes=256, nodeID=3 -> 1284
		},
		// Layer2 tests
		{
			name: "Layer2 IPv4, first network",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("192.168.0.0/31")},
			topologyType:      ovntypes.Layer2Topology,
			nodeID:            0,             // ignored for Layer2
			expectedTunnelKey: 0*256 + 0 + 1, // networkIndex=0, maxNodes=256, subIndex=0 -> 1
		},
		{
			name: "Layer2 IPv4, second network with subIndex",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("192.168.1.4/31")},
			topologyType:      ovntypes.Layer2Topology,
			nodeID:            0,
			expectedTunnelKey: 1*256 + 2 + 1, // networkIndex=1, maxNodes=256, subIndex=4/2=2 -> 259
		},
		{
			name: "Layer2 IPv6, third network",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "fd00::/112", NetworkPrefix: 120},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("fd00::300/127")},
			topologyType:      ovntypes.Layer2Topology,
			nodeID:            0,
			expectedTunnelKey: 3*256 + 0 + 1, // networkIndex=3, maxNodes=256, subIndex=0 -> 769
		},
		{
			name: "Layer2 DualStack, fourth network",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
				{CIDR: "fd00:192:168::/112", NetworkPrefix: 120},
			},
			allocatedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.4.2/31"),
				ovntest.MustParseIPNet("fd00:192:168::402/127"),
			},
			topologyType:      ovntypes.Layer2Topology,
			nodeID:            0,
			expectedTunnelKey: 4*256 + 1 + 1, // networkIndex=4, maxNodes=256, subIndex=2/2=1 -> 1026
		},
		// Large maxNodes (capped at 5000)
		{
			name: "Layer3 with large maxNodes capped",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "10.0.0.0/8", NetworkPrefix: 16},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("10.2.0.0/16")},
			topologyType:      ovntypes.Layer3Topology,
			nodeID:            10,
			expectedTunnelKey: 2*5000 + 10 + 1, // networkIndex=2, maxNodes=5000 (capped), nodeID=10 -> 10011
		},
		{
			name: "Layer3 DualStack with large maxNodes capped",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "10.0.0.0/8", NetworkPrefix: 16},
				{CIDR: "fd00:10::/48", NetworkPrefix: 56},
			},
			allocatedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("10.3.0.0/16"),
				ovntest.MustParseIPNet("fd00:10:300::/56"),
			},
			topologyType:      ovntypes.Layer3Topology,
			nodeID:            7,
			expectedTunnelKey: 3*5000 + 7 + 1, // networkIndex=3, maxNodes=5000 (capped), nodeID=7 -> 15008
		},
		{
			name: "Layer2 DualStack with large maxNodes capped",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "10.0.0.0/8", NetworkPrefix: 16},
				{CIDR: "fd00:10::/48", NetworkPrefix: 56},
			},
			allocatedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("10.4.0.6/31"),
				ovntest.MustParseIPNet("fd00:10:400::6/127"),
			},
			topologyType:      ovntypes.Layer2Topology,
			nodeID:            0,
			expectedTunnelKey: 4*5000 + 3 + 1, // networkIndex=4, maxNodes=5000 (capped), subIndex=6/2=3 -> 20004
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tunnelKey := GetTunnelKey(tt.connectSubnets, tt.allocatedSubnets, tt.topologyType, tt.nodeID)
			assert.Equal(t, tt.expectedTunnelKey, tunnelKey, "tunnelKey mismatch")
		})
	}
}
