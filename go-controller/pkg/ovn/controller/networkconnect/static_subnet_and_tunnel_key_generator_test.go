package networkconnect

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func TestGetLayer2SubIndex(t *testing.T) {
	tests := []struct {
		name          string
		subnet        *net.IPNet
		networkPrefix int
		expected      int
	}{
		{
			name:          "IPv4 /31 at start of /24 block",
			subnet:        ovntest.MustParseIPNet("192.168.0.0/31"),
			networkPrefix: 24,
			expected:      0,
		},
		{
			name:          "IPv4 /31 at offset 2 in /24 block",
			subnet:        ovntest.MustParseIPNet("192.168.0.2/31"),
			networkPrefix: 24,
			expected:      1,
		},
		{
			name:          "IPv4 /31 at offset 10 in /24 block",
			subnet:        ovntest.MustParseIPNet("192.168.0.10/31"),
			networkPrefix: 24,
			expected:      5,
		},
		{
			name:          "IPv6 /127 at start of /120 block",
			subnet:        ovntest.MustParseIPNet("fd00::0/127"),
			networkPrefix: 120,
			expected:      0,
		},
		{
			name:          "IPv6 /127 at offset 4 in /120 block",
			subnet:        ovntest.MustParseIPNet("fd00::4/127"),
			networkPrefix: 120,
			expected:      2,
		},
		// Test cases for networkPrefix spanning multiple octets
		{
			name:          "IPv4 /31 in second octet of /20 block",
			subnet:        ovntest.MustParseIPNet("10.0.17.0/31"), // 10.0.16.0/20 block, offset 256
			networkPrefix: 20,
			expected:      128, // (256 IPs in 10.0.16.x) / 2
		},
		{
			name:          "IPv4 /31 at end of /20 block",
			subnet:        ovntest.MustParseIPNet("10.0.31.254/31"), // 10.0.16.0/20 block, last /31
			networkPrefix: 20,
			expected:      2047, // (4096 IPs - 2) / 2
		},
		{
			name:          "IPv6 /127 within /112 block",
			subnet:        ovntest.MustParseIPNet("fd00::1:2/127"), // within fd00::1:0/112 block, offset 2
			networkPrefix: 112,
			expected:      1, // 2 / 2 = 1
		},
		{
			name:          "IPv6 /127 spanning octets - large offset",
			subnet:        ovntest.MustParseIPNet("fd00::100:0/127"), // within fd00::/72 block, offset 0x01000000
			networkPrefix: 72,
			expected:      8388608, // 16777216 / 2
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getLayer2SubIndex(tt.subnet, tt.networkPrefix)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetP2PAddresses(t *testing.T) {
	tests := []struct {
		name           string
		subnets        []*net.IPNet
		nodeID         int
		expectedFirst  []string
		expectedSecond []string
	}{
		{
			name: "IPv4 node ID 0",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/24"),
			},
			nodeID:         0,
			expectedFirst:  []string{"192.168.0.0/31"},
			expectedSecond: []string{"192.168.0.1/31"},
		},
		{
			name: "IPv4 node ID 1",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/24"),
			},
			nodeID:         1,
			expectedFirst:  []string{"192.168.0.2/31"},
			expectedSecond: []string{"192.168.0.3/31"},
		},
		{
			name: "IPv4 node ID 5",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/24"),
			},
			nodeID:         5,
			expectedFirst:  []string{"192.168.0.10/31"},
			expectedSecond: []string{"192.168.0.11/31"},
		},
		{
			name: "IPv6 node ID 0",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("fd00::/64"),
			},
			nodeID:         0,
			expectedFirst:  []string{"fd00::/127"},
			expectedSecond: []string{"fd00::1/127"},
		},
		{
			name: "IPv6 node ID 1",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("fd00::/64"),
			},
			nodeID:         1,
			expectedFirst:  []string{"fd00::2/127"},
			expectedSecond: []string{"fd00::3/127"},
		},
		{
			name: "dual stack node ID 0",
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/24"),
				ovntest.MustParseIPNet("fd00::/64"),
			},
			nodeID:         0,
			expectedFirst:  []string{"192.168.0.0/31", "fd00::/127"},
			expectedSecond: []string{"192.168.0.1/31", "fd00::1/127"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			portPairInfo, err := GetP2PAddresses(tt.subnets, tt.nodeID)
			require.NoError(t, err)
			require.Len(t, portPairInfo.connectPortIPs, len(tt.expectedFirst))
			require.Len(t, portPairInfo.networkPortIPs, len(tt.expectedSecond))

			for i, expected := range tt.expectedFirst {
				assert.Equal(t, expected, portPairInfo.connectPortIPs[i].String())
			}
			for i, expected := range tt.expectedSecond {
				assert.Equal(t, expected, portPairInfo.networkPortIPs[i].String())
			}
		})
	}
}

func TestGetNetworkIndexAndMaxNodes(t *testing.T) {
	tests := []struct {
		name                 string
		connectSubnets       []networkconnectv1.ConnectSubnet
		subnet               *net.IPNet
		expectedNetworkIndex int
		expectedMaxNodes     int
		expectedErr          string
	}{
		// IPv4 with /20 networkPrefix (4096 IPs per network)
		{
			name: "IPv4 /12 CIDR with /20 prefix, first network",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "172.16.0.0/12", NetworkPrefix: 20},
			},
			subnet:               ovntest.MustParseIPNet("172.16.0.0/20"),
			expectedNetworkIndex: 0,
			expectedMaxNodes:     4096, // 2^(32-20) = 4096
		},
		{
			name: "IPv4 /12 CIDR with /20 prefix, network at 172.20.0.0",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "172.16.0.0/12", NetworkPrefix: 20},
			},
			subnet:               ovntest.MustParseIPNet("172.20.0.0/20"),
			expectedNetworkIndex: 64, // (172.20.0.0 - 172.16.0.0) / 4096 = 262144 / 4096 = 64
			expectedMaxNodes:     4096,
		},
		// IPv4 with /22 networkPrefix (1024 IPs per network)
		{
			name: "IPv4 /16 CIDR with /22 prefix, network index 7",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "10.100.0.0/16", NetworkPrefix: 22},
			},
			subnet:               ovntest.MustParseIPNet("10.100.28.0/22"),
			expectedNetworkIndex: 7, // 28 / 4 = 7 (each /22 spans 4 in third octet)
			expectedMaxNodes:     1024,
		},
		// IPv4 with /26 networkPrefix (64 IPs per network)
		{
			name: "IPv4 /20 CIDR with /26 prefix, network spanning octets",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "10.0.16.0/20", NetworkPrefix: 26},
			},
			subnet:               ovntest.MustParseIPNet("10.0.17.128/26"),
			expectedNetworkIndex: 6, // offset 384 / 64 = 6
			expectedMaxNodes:     64,
		},
		// IPv4 with /28 networkPrefix (16 IPs per network)
		{
			name: "IPv4 /24 CIDR with /28 prefix, last network",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.100.0/24", NetworkPrefix: 28},
			},
			subnet:               ovntest.MustParseIPNet("192.168.100.240/28"),
			expectedNetworkIndex: 15, // 240 / 16 = 15
			expectedMaxNodes:     16,
		},
		// IPv4 with /18 networkPrefix (16384 IPs, capped at 5000)
		{
			name: "IPv4 /8 CIDR with /18 prefix, large network",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "10.0.0.0/8", NetworkPrefix: 18},
			},
			subnet:               ovntest.MustParseIPNet("10.4.64.0/18"),
			expectedNetworkIndex: 17, // (10.4.64.0 - 10.0.0.0) / 16384 = 279552 / 16384 = 17
			expectedMaxNodes:     5000,
		},
		// IPv6 with /116 networkPrefix (4096 IPs per network)
		{
			name: "IPv6 /108 CIDR with /116 prefix, network index 3",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "fd00:abcd::/108", NetworkPrefix: 116},
			},
			subnet:               ovntest.MustParseIPNet("fd00:abcd::3000/116"),
			expectedNetworkIndex: 3, // 0x3000 / 0x1000 = 3
			expectedMaxNodes:     4096,
		},
		// IPv6 with /124 networkPrefix (16 IPs per network)
		{
			name: "IPv6 /120 CIDR with /124 prefix, network index 10",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "2001:db8:cafe::/120", NetworkPrefix: 124},
			},
			subnet:               ovntest.MustParseIPNet("2001:db8:cafe::a0/124"),
			expectedNetworkIndex: 10, // 0xa0 / 0x10 = 10
			expectedMaxNodes:     16,
		},
		// IPv6 with /104 networkPrefix (16M IPs, capped at 5000)
		{
			name: "IPv6 /96 CIDR with /104 prefix, capped maxNodes",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "fd12:3456::/96", NetworkPrefix: 104},
			},
			subnet:               ovntest.MustParseIPNet("fd12:3456::500:0/104"),
			expectedNetworkIndex: 5, // 0x05000000 >> 24 = 5
			expectedMaxNodes:     5000,
		},
		// DualStack with /20 and /116 (matching host bits: 32-20 = 128-116 = 12)
		// Note: function only uses the first (IPv4) subnet for calculation
		{
			name: "DualStack /16+/112 with /20+/116 prefix",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "10.50.0.0/16", NetworkPrefix: 20},
				{CIDR: "fd00:50::/112", NetworkPrefix: 116},
			},
			subnet:               ovntest.MustParseIPNet("10.50.48.0/20"),
			expectedNetworkIndex: 3, // IPv4: 48 / 16 = 3
			expectedMaxNodes:     4096,
		},
		// DualStack with /22 and /118 (matching host bits: 32-22 = 128-118 = 10)
		{
			name: "DualStack /14+/110 with /22+/118 prefix",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "172.16.0.0/14", NetworkPrefix: 22},
				{CIDR: "fd00:172::/110", NetworkPrefix: 118},
			},
			subnet:               ovntest.MustParseIPNet("172.18.8.0/22"),
			expectedNetworkIndex: 130, // IPv4: (172.18.8.0 - 172.16.0.0) / 1024 = 133120 / 1024 = 130
			expectedMaxNodes:     1024,
		},
		// Error cases (validation errors - getNetworkPrefixAndConnectCIDR errors are tested via TestGetTunnelKey)
		{
			name: "error: networkPrefix equal to connect CIDR prefix",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.0.0/24", NetworkPrefix: 24}, // same as CIDR prefix - invalid
			},
			subnet:      ovntest.MustParseIPNet("192.168.0.0/24"),
			expectedErr: "invalid configuration: networkPrefix (24) must be greater than connect CIDR prefix (24)",
		},
		{
			name: "error: networkPrefix smaller than connect CIDR prefix",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.0.0/24", NetworkPrefix: 16}, // smaller than CIDR prefix - invalid
			},
			subnet:      ovntest.MustParseIPNet("192.168.0.0/16"),
			expectedErr: "invalid configuration: networkPrefix (16) must be greater than connect CIDR prefix (24)",
		},
		// Note: In practice, this case is prevented by CRD CEL validation which enforces networkPrefix < 32 for IPv4.
		// This test is for defense-in-depth validation in the code.
		{
			name: "error: shift is zero (IPv4 networkPrefix equals totalBits)",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.0.0/16", NetworkPrefix: 32}, // shift = 32 - 32 = 0
			},
			subnet:      ovntest.MustParseIPNet("192.168.0.1/32"),
			expectedErr: "invalid configuration: networkPrefix (32) must be greater than connect CIDR prefix (16) and less than 32",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// First get networkPrefix and connectCIDR from connectSubnets
			networkPrefix, connectCIDR, err := getNetworkPrefixAndConnectCIDR(tt.connectSubnets, tt.subnet)
			if err != nil {
				// This shouldn't happen for valid test cases; errors from getNetworkPrefixAndConnectCIDR
				// are tested via TestGetTunnelKey
				t.Fatalf("unexpected error from getNetworkPrefixAndConnectCIDR: %v", err)
			}

			networkIndex, maxNodes, err := getNetworkIndexAndMaxNodes(tt.subnet, networkPrefix, connectCIDR)
			if tt.expectedErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectedErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectedNetworkIndex, networkIndex, "networkIndex mismatch")
				assert.Equal(t, tt.expectedMaxNodes, maxNodes, "maxNodes mismatch")
			}
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
		expectedErr       string
	}{
		// Layer3 with /20 networkPrefix (4096 maxNodes)
		{
			name: "Layer3 IPv4 /12 CIDR with /20 prefix, network 5, node 100",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "172.16.0.0/12", NetworkPrefix: 20},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("172.16.80.0/20")},
			topologyType:      ovntypes.Layer3Topology,
			nodeID:            100,
			expectedTunnelKey: 5*4096 + 100 + 1, // networkIndex=5 (80/16=5), maxNodes=4096, nodeID=100 -> 20581
		},
		// Layer3 with /22 networkPrefix (1024 maxNodes)
		{
			name: "Layer3 IPv4 /16 CIDR with /22 prefix, network 12, node 500",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "10.100.0.0/16", NetworkPrefix: 22},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("10.100.48.0/22")},
			topologyType:      ovntypes.Layer3Topology,
			nodeID:            500,
			expectedTunnelKey: 12*1024 + 500 + 1, // networkIndex=12 (48/4=12), maxNodes=1024 -> 12789
		},
		// Layer3 with /26 networkPrefix (64 maxNodes)
		{
			name: "Layer3 IPv4 /20 CIDR with /26 prefix, network spanning octets",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "10.0.16.0/20", NetworkPrefix: 26},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("10.0.19.64/26")},
			topologyType:      ovntypes.Layer3Topology,
			nodeID:            30,
			expectedTunnelKey: 13*64 + 30 + 1, // networkIndex=13 (offset 832/64=13), maxNodes=64 -> 863
		},
		// Layer3 with /28 networkPrefix (16 maxNodes)
		{
			name: "Layer3 IPv4 /24 CIDR with /28 prefix, small network",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.100.0/24", NetworkPrefix: 28},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("192.168.100.176/28")},
			topologyType:      ovntypes.Layer3Topology,
			nodeID:            7,
			expectedTunnelKey: 11*16 + 7 + 1, // networkIndex=11 (176/16=11), maxNodes=16 -> 184
		},
		// Layer3 IPv6 with /116 networkPrefix (4096 maxNodes)
		{
			name: "Layer3 IPv6 /108 CIDR with /116 prefix",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "fd00:abcd::/108", NetworkPrefix: 116},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("fd00:abcd::7000/116")},
			topologyType:      ovntypes.Layer3Topology,
			nodeID:            200,
			expectedTunnelKey: 7*4096 + 200 + 1, // networkIndex=7, maxNodes=4096 -> 28873
		},
		// Layer3 IPv6 with /124 networkPrefix (16 maxNodes)
		{
			name: "Layer3 IPv6 /120 CIDR with /124 prefix",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "2001:db8:cafe::/120", NetworkPrefix: 124},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("2001:db8:cafe::b0/124")},
			topologyType:      ovntypes.Layer3Topology,
			nodeID:            5,
			expectedTunnelKey: 11*16 + 5 + 1, // networkIndex=11 (0xb0/0x10=11), maxNodes=16 -> 182
		},
		// Layer3 DualStack with /20+/116 (matching host bits)
		{
			name: "Layer3 DualStack /16+/112 with /20+/116 prefix",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "10.50.0.0/16", NetworkPrefix: 20},
				{CIDR: "fd00:50::/112", NetworkPrefix: 116},
			},
			allocatedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("10.50.64.0/20"),
				ovntest.MustParseIPNet("fd00:50::4000/116"),
			},
			topologyType:      ovntypes.Layer3Topology,
			nodeID:            1000,
			expectedTunnelKey: 4*4096 + 1000 + 1, // networkIndex=4, maxNodes=4096 -> 17385
		},
		// Layer2 with /20 networkPrefix (4096 maxNodes), /31 spanning octets
		{
			name: "Layer2 IPv4 /12 CIDR with /20 prefix, /31 in second octet",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "172.16.0.0/12", NetworkPrefix: 20},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("172.16.17.128/31")},
			topologyType:      ovntypes.Layer2Topology,
			nodeID:            0,
			expectedTunnelKey: 1*4096 + 192 + 1, // networkIndex=1 (172.16.16.0/20 block), subIndex=384/2=192 -> 4289
		},
		// Layer2 with /22 networkPrefix (1024 maxNodes)
		{
			name: "Layer2 IPv4 /16 CIDR with /22 prefix, /31 at high offset",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "10.100.0.0/16", NetworkPrefix: 22},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("10.100.51.254/31")},
			topologyType:      ovntypes.Layer2Topology,
			nodeID:            0,
			expectedTunnelKey: 12*1024 + 511 + 1, // networkIndex=12 (48-51 = block 12), subIndex=(3*256+254)/2=511 -> 12800
		},
		// Layer2 with /26 networkPrefix (64 maxNodes)
		{
			name: "Layer2 IPv4 /20 CIDR with /26 prefix",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "10.0.16.0/20", NetworkPrefix: 26},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("10.0.20.62/31")},
			topologyType:      ovntypes.Layer2Topology,
			nodeID:            0,
			expectedTunnelKey: 16*64 + 31 + 1, // networkIndex=16 (offset 1024/64), subIndex=62/2=31 -> 1056
		},
		// Layer2 IPv6 with /116 networkPrefix (4096 maxNodes)
		{
			name: "Layer2 IPv6 /108 CIDR with /116 prefix",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "fd00:abcd::/108", NetworkPrefix: 116},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("fd00:abcd::5800/127")},
			topologyType:      ovntypes.Layer2Topology,
			nodeID:            0,
			expectedTunnelKey: 5*4096 + 1024 + 1, // networkIndex=5, subIndex=0x800/2=1024 -> 21505
		},
		// Layer2 DualStack with /20+/116
		{
			name: "Layer2 DualStack /16+/112 with /20+/116 prefix",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "10.50.0.0/16", NetworkPrefix: 20},
				{CIDR: "fd00:50::/112", NetworkPrefix: 116},
			},
			allocatedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("10.50.96.100/31"),
				ovntest.MustParseIPNet("fd00:50::6064/127"),
			},
			topologyType:      ovntypes.Layer2Topology,
			nodeID:            0,
			expectedTunnelKey: 6*4096 + 50 + 1, // networkIndex=6, subIndex=100/2=50 -> 24627
		},
		// Large maxNodes (capped at 5000)
		{
			name: "Layer3 with /18 prefix, maxNodes capped",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "10.0.0.0/8", NetworkPrefix: 18},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("10.4.64.0/18")},
			topologyType:      ovntypes.Layer3Topology,
			nodeID:            2500,
			expectedTunnelKey: 17*5000 + 2500 + 1, // networkIndex=17, maxNodes=5000 (capped) -> 87501
		},
		{
			name: "Layer2 with /18 prefix, maxNodes capped, high subIndex",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "10.0.0.0/8", NetworkPrefix: 18},
			},
			allocatedSubnets:  []*net.IPNet{ovntest.MustParseIPNet("10.8.127.254/31")},
			topologyType:      ovntypes.Layer2Topology,
			nodeID:            0,
			expectedTunnelKey: 33*5000 + 8191 + 1, // networkIndex=33 (10.8.64.0/18 block), subIndex=16382/2=8191 -> 173192
		},
		// Error cases
		{
			name: "error: invalid CIDR",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "invalid", NetworkPrefix: 24},
			},
			allocatedSubnets: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.0/24")},
			topologyType:     ovntypes.Layer3Topology,
			nodeID:           1,
			expectedErr:      "failed to parse connect subnet",
		},
		{
			name: "error: no matching connect subnet for Layer3 IPv4",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "fd00::/112", NetworkPrefix: 120},
			},
			allocatedSubnets: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.0/24")},
			topologyType:     ovntypes.Layer3Topology,
			nodeID:           1,
			expectedErr:      "no connect subnet found for IP family",
		},
		{
			name: "error: no matching connect subnet for Layer2 IPv6",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
			},
			allocatedSubnets: []*net.IPNet{ovntest.MustParseIPNet("fd00::0/127")},
			topologyType:     ovntypes.Layer2Topology,
			nodeID:           0,
			expectedErr:      "no connect subnet found for IP family",
		},
		{
			name: "error: networkPrefix equal to connect CIDR prefix",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.0.0/24", NetworkPrefix: 24},
			},
			allocatedSubnets: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.0/24")},
			topologyType:     ovntypes.Layer3Topology,
			nodeID:           1,
			expectedErr:      "invalid configuration: networkPrefix",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tunnelKey, err := GetTunnelKey(tt.connectSubnets, tt.allocatedSubnets, tt.topologyType, tt.nodeID)
			if tt.expectedErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectedErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectedTunnelKey, tunnelKey, "tunnelKey mismatch")
			}
		})
	}
}
