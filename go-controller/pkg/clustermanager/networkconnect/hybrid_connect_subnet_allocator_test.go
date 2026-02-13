package networkconnect

import (
	"fmt"
	"net"
	"testing"

	"github.com/onsi/gomega"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	networkconnectv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func mustParseCIDR(cidr string) *net.IPNet {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	return network
}

func TestHybridConnectSubnetAllocator_AddNetworkRange(t *testing.T) {
	tests := []struct {
		name          string
		network       string
		networkPrefix int
		expectErr     string
	}{
		{
			name:          "valid IPv4 range",
			network:       "192.168.0.0/16",
			networkPrefix: 24,
			expectErr:     "",
		},
		{
			name:          "valid IPv6 range",
			network:       "fd00::/48",
			networkPrefix: 64,
			expectErr:     "",
		},
		{
			name:          "networkPrefix smaller than base CIDR prefix",
			network:       "192.168.0.0/24",
			networkPrefix: 16,
			expectErr:     "networkPrefix 16 must be larger than base CIDR prefix 24",
		},
		{
			name:          "networkPrefix equal to base CIDR prefix",
			network:       "192.168.0.0/24",
			networkPrefix: 24,
			expectErr:     "networkPrefix 24 must be larger than base CIDR prefix 24",
		},
		{
			name:          "networkPrefix equal to address length",
			network:       "192.168.0.0/16",
			networkPrefix: 32,
			expectErr:     "networkPrefix 32 must be smaller than address length 32",
		},
		{
			name:          "networkPrefix larger than address length",
			network:       "192.168.0.0/16",
			networkPrefix: 33,
			expectErr:     "networkPrefix 33 must be smaller than address length 32",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			config.IPv4Mode = true
			config.IPv6Mode = true

			allocator, err := NewHybridConnectSubnetAllocator(nil, "test-cnc")
			if err != nil {
				t.Fatalf("failed to create subnet allocator: %v", err)
			}
			network := mustParseCIDR(tt.network)
			err = allocator.AddNetworkRange(network, tt.networkPrefix)

			if tt.expectErr != "" {
				g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(tt.expectErr)))
			} else {
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}
		})
	}
}

func TestHybridConnectSubnetAllocator_AllocateLayer3Subnet(t *testing.T) {
	tests := []struct {
		name          string
		ipv4Mode      bool
		ipv6Mode      bool
		owners        []string
		expectSubnets map[string][]string
	}{
		{
			name:     "single IPv4 allocation",
			ipv4Mode: true,
			ipv6Mode: false,
			owners:   []string{"layer3_1"},
			expectSubnets: map[string][]string{
				"layer3_1": {"192.168.0.0/24"},
			},
		},
		{
			name:     "multiple IPv4 allocations",
			ipv4Mode: true,
			ipv6Mode: false,
			owners:   []string{"layer3_1", "layer3_2", "layer3_3"},
			expectSubnets: map[string][]string{
				"layer3_1": {"192.168.0.0/24"},
				"layer3_2": {"192.168.1.0/24"},
				"layer3_3": {"192.168.2.0/24"},
			},
		},
		{
			name:     "single IPv6 allocation",
			ipv4Mode: false,
			ipv6Mode: true,
			owners:   []string{"layer3_1"},
			expectSubnets: map[string][]string{
				// With /112 CIDR and /120 prefix, blocks are /120 (256 addresses each)
				"layer3_1": {"fd00::/120"},
			},
		},
		{
			name:     "multiple IPv6 allocations",
			ipv4Mode: false,
			ipv6Mode: true,
			owners:   []string{"layer3_1", "layer3_2", "layer3_3"},
			expectSubnets: map[string][]string{
				// /120 blocks: fd00::/120, fd00::100/120, fd00::200/120, etc.
				"layer3_1": {"fd00::/120"},
				"layer3_2": {"fd00::100/120"},
				"layer3_3": {"fd00::200/120"},
			},
		},
		{
			name:     "dual-stack allocation",
			ipv4Mode: true,
			ipv6Mode: true,
			owners:   []string{"layer3_1"},
			expectSubnets: map[string][]string{
				// IPv4 /24, IPv6 /120 (both have 8 host bits)
				"layer3_1": {"192.168.0.0/24", "fd00::/120"},
			},
		},
		{
			name:     "multiple dual-stack allocations",
			ipv4Mode: true,
			ipv6Mode: true,
			owners:   []string{"layer3_1", "layer3_2", "layer3_3"},
			expectSubnets: map[string][]string{
				"layer3_1": {"192.168.0.0/24", "fd00::/120"},
				"layer3_2": {"192.168.1.0/24", "fd00::100/120"},
				"layer3_3": {"192.168.2.0/24", "fd00::200/120"},
			},
		},
		{
			name:     "same owner gets same subnet on repeated allocation",
			ipv4Mode: true,
			ipv6Mode: false,
			owners:   []string{"layer3_1", "layer3_1"}, // same owner twice
			expectSubnets: map[string][]string{
				"layer3_1": {"192.168.0.0/24"},
			},
		},
		{
			name:     "different owners get different subnets",
			ipv4Mode: true,
			ipv6Mode: false,
			owners:   []string{"layer3_1", "layer3_2"},
			expectSubnets: map[string][]string{
				"layer3_1": {"192.168.0.0/24"},
				"layer3_2": {"192.168.1.0/24"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			config.IPv4Mode = tt.ipv4Mode
			config.IPv6Mode = tt.ipv6Mode

			connectSubnets := []networkconnectv1.ConnectSubnet{
				{
					CIDR:          "192.168.0.0/16",
					NetworkPrefix: 24,
				},
				{
					CIDR:          "fd00::/112",
					NetworkPrefix: 120, // 32-24=8, so 128-8=120
				},
			}
			allocator, err := NewHybridConnectSubnetAllocator(connectSubnets, "test-cnc")
			if err != nil {
				t.Fatalf("failed to create subnet allocator: %v", err)
			}

			// Allocate subnets
			for _, owner := range tt.owners {
				subnets, err := allocator.AllocateLayer3Subnet(owner)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				if expected, ok := tt.expectSubnets[owner]; ok {
					g.Expect(subnets).To(gomega.HaveLen(len(expected)))
					for i, subnet := range subnets {
						g.Expect(subnet.String()).To(gomega.Equal(expected[i]))
					}
				}
			}
		})
	}
}

func TestHybridConnectSubnetAllocator_AllocateLayer2Subnet(t *testing.T) {
	tests := []struct {
		name          string
		ipv4Mode      bool
		ipv6Mode      bool
		owners        []string
		expectSubnets map[string][]string
	}{
		{
			name:     "single IPv4 layer2 allocation gets /31",
			ipv4Mode: true,
			ipv6Mode: false,
			owners:   []string{"layer2_1"},
			expectSubnets: map[string][]string{
				"layer2_1": {"192.168.0.0/31"},
			},
		},
		{
			name:     "multiple IPv4 layer2 allocations get /31 each",
			ipv4Mode: true,
			ipv6Mode: false,
			owners:   []string{"layer2_1", "layer2_2", "layer2_3"},
			expectSubnets: map[string][]string{
				"layer2_1": {"192.168.0.0/31"},
				"layer2_2": {"192.168.0.2/31"},
				"layer2_3": {"192.168.0.4/31"},
			},
		},
		{
			name:     "single IPv6 layer2 allocation gets /127",
			ipv4Mode: false,
			ipv6Mode: true,
			owners:   []string{"layer2_1"},
			expectSubnets: map[string][]string{
				// Layer2 block gets /120 block from layer3 (fd00::/120), then allocates /127 from it
				// /127 has subnetBits = 127 - 120 = 7, which is < 16, so no address skipping
				"layer2_1": {"fd00::/127"},
			},
		},
		{
			name:     "multiple IPv6 layer2 allocations get /127 each",
			ipv4Mode: false,
			ipv6Mode: true,
			owners:   []string{"layer2_1", "layer2_2", "layer2_3"},
			expectSubnets: map[string][]string{
				"layer2_1": {"fd00::/127"},
				"layer2_2": {"fd00::2/127"},
				"layer2_3": {"fd00::4/127"},
			},
		},
		{
			name:     "dual-stack layer2 allocation",
			ipv4Mode: true,
			ipv6Mode: true,
			owners:   []string{"layer2_1"},
			expectSubnets: map[string][]string{
				// Layer2 block gets /24 block from layer3 (192.168.0.0/24), then allocates /31 from it
				// IPv6 block gets /120 block from layer3 (fd00::/120), then allocates /127 from it
				"layer2_1": {"192.168.0.0/31", "fd00::/127"},
			},
		},
		{
			name:     "multiple dual-stack layer2 allocations",
			ipv4Mode: true,
			ipv6Mode: true,
			owners:   []string{"layer2_1", "layer2_2", "layer2_3"},
			expectSubnets: map[string][]string{
				"layer2_1": {"192.168.0.0/31", "fd00::/127"},
				"layer2_2": {"192.168.0.2/31", "fd00::2/127"},
				"layer2_3": {"192.168.0.4/31", "fd00::4/127"},
			},
		},
		{
			name:     "same owner gets same subnet on repeated allocation",
			ipv4Mode: true,
			ipv6Mode: false,
			owners:   []string{"layer2_1", "layer2_1"}, // same owner twice
			expectSubnets: map[string][]string{
				"layer2_1": {"192.168.0.0/31"},
			},
		},
		{
			name:     "different owners get different subnets",
			ipv4Mode: true,
			ipv6Mode: false,
			owners:   []string{"layer2_1", "layer2_2"},
			expectSubnets: map[string][]string{
				"layer2_1": {"192.168.0.0/31"},
				"layer2_2": {"192.168.0.2/31"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			config.IPv4Mode = tt.ipv4Mode
			config.IPv6Mode = tt.ipv6Mode

			connectSubnets := []networkconnectv1.ConnectSubnet{
				{
					CIDR:          "192.168.0.0/16",
					NetworkPrefix: 24,
				},
				{
					CIDR:          "fd00::/112",
					NetworkPrefix: 120, // 32-24=8, so 128-8=120
				},
			}
			allocator, err := NewHybridConnectSubnetAllocator(connectSubnets, "test-cnc")
			if err != nil {
				t.Fatalf("failed to create subnet allocator: %v", err)
			}

			// Allocate layer2 subnets
			for _, owner := range tt.owners {
				subnets, err := allocator.AllocateLayer2Subnet(owner)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				if expected, ok := tt.expectSubnets[owner]; ok {
					g.Expect(subnets).To(gomega.HaveLen(len(expected)))
					for i, subnet := range subnets {
						g.Expect(subnet.String()).To(gomega.Equal(expected[i]))
					}
				}
			}
		})
	}
}

func TestHybridConnectSubnetAllocator_AllocateMixedLayer3AndLayer2Subnets(t *testing.T) {
	g := gomega.NewWithT(t)

	config.IPv4Mode = true
	config.IPv6Mode = false

	connectSubnets := []networkconnectv1.ConnectSubnet{
		{
			CIDR:          "192.168.0.0/16",
			NetworkPrefix: 24,
		},
	}
	allocator, err := NewHybridConnectSubnetAllocator(connectSubnets, "test-cnc")
	if err != nil {
		t.Fatalf("failed to create subnet allocator: %v", err)
	}

	// Allocate some Layer3 subnets first
	layer3Subnets := make(map[string][]*net.IPNet)
	for i := 1; i <= 3; i++ {
		owner := "layer3_" + string(rune('0'+i))
		subnets, err := allocator.AllocateLayer3Subnet(owner)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		layer3Subnets[owner] = subnets
	}

	// Now allocate Layer2 subnets - they should come from a different /24 block
	layer2Subnets := make(map[string][]*net.IPNet)
	for i := 1; i <= 5; i++ {
		owner := "layer2_" + string(rune('0'+i))
		subnets, err := allocator.AllocateLayer2Subnet(owner)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		layer2Subnets[owner] = subnets

		// Verify Layer2 subnets are /31
		ones, _ := subnets[0].Mask.Size()
		g.Expect(ones).To(gomega.Equal(31))
	}

	// Verify no overlap between Layer3 and Layer2 subnets.
	// Layer2 subnets are /31s within a /24 block allocated from layer3 for the layer2-block.
	// So we verify they're in different /24 blocks than the Layer3 subnets.
	for _, l3Subnets := range layer3Subnets {
		for _, l3Subnet := range l3Subnets {
			for _, l2Subnets := range layer2Subnets {
				for _, l2Subnet := range l2Subnets {
					l3Network := l3Subnet.IP.Mask(net.CIDRMask(24, 32))
					l2Network := l2Subnet.IP.Mask(net.CIDRMask(24, 32))
					g.Expect(l3Network.String()).ToNot(gomega.Equal(l2Network.String()),
						"Layer3 subnet %s and Layer2 subnet %s should not be in the same /24 block",
						l3Subnet.String(), l2Subnet.String())
				}
			}
		}
	}
}

func TestHybridConnectSubnetAllocator_ReleaseLayer3Subnets(t *testing.T) {
	tests := []struct {
		name              string
		ipv4Mode          bool
		ipv6Mode          bool
		ipv4Network       string
		ipv4NetworkPrefix int
		ipv6Network       string
		ipv6NetworkPrefix int
		allocateFirst     []string
		release           []string
		allocateAgain     []string
		expectSubnets     map[string][]string // owner -> expected subnets after re-allocation of released subnets
	}{
		{
			name:              "IPv4 allocation continues from where it left off after release",
			ipv4Mode:          true,
			ipv6Mode:          false,
			ipv4Network:       "192.168.0.0/16",
			ipv4NetworkPrefix: 24,
			allocateFirst:     []string{"layer3_1", "layer3_2"},
			release:           []string{"layer3_1"},
			allocateAgain:     []string{"layer3_3"},
			expectSubnets: map[string][]string{
				// Allocator continues from next position, doesn't immediately reuse released subnet
				"layer3_3": {"192.168.2.0/24"},
			},
		},
		{
			name:              "IPv4 released subnet is reused when allocator wraps around",
			ipv4Mode:          true,
			ipv6Mode:          false,
			ipv4Network:       "192.168.0.0/24", // Small range: only 4 /26 subnets
			ipv4NetworkPrefix: 26,
			allocateFirst:     []string{"layer3_1", "layer3_2", "layer3_3", "layer3_4"},
			release:           []string{"layer3_2"}, // Release the second one
			allocateAgain:     []string{"layer3_5"},
			expectSubnets: map[string][]string{
				// Allocator wraps around and reuses the released subnet
				"layer3_5": {"192.168.0.64/26"},
			},
		},
		{
			name:              "IPv6 allocation continues from where it left off after release",
			ipv4Mode:          false,
			ipv6Mode:          true,
			ipv6Network:       "fd00::/112",
			ipv6NetworkPrefix: 120, // matches ipv4 /24: 32-24=8, 128-8=120
			allocateFirst:     []string{"layer3_1", "layer3_2"},
			release:           []string{"layer3_1"},
			allocateAgain:     []string{"layer3_3"},
			expectSubnets: map[string][]string{
				// Allocator continues from next position: layer3_1=fd00::/120, layer3_2=fd00::100/120
				// layer3_3 gets fd00::200/120
				"layer3_3": {"fd00::200/120"},
			},
		},
		{
			name:              "IPv6 released subnet is reused when allocator wraps around",
			ipv4Mode:          false,
			ipv6Mode:          true,
			ipv6Network:       "fd00::/120",                                             // Small range: only 4 /122 subnets
			ipv6NetworkPrefix: 122,                                                      // matches ipv4 /26: 32-26=6, 128-6=122
			allocateFirst:     []string{"layer3_1", "layer3_2", "layer3_3", "layer3_4"}, // Allocate all 4
			release:           []string{"layer3_2"},                                     // Release the second one
			allocateAgain:     []string{"layer3_5"},
			expectSubnets: map[string][]string{
				// Allocator wraps around and reuses the released subnet (fd00::40/122)
				"layer3_5": {"fd00::40/122"},
			},
		},
		{
			name:              "dual-stack allocation continues from where it left off after release",
			ipv4Mode:          true,
			ipv6Mode:          true,
			ipv4Network:       "192.168.0.0/16",
			ipv4NetworkPrefix: 24,
			ipv6Network:       "fd00::/112",
			ipv6NetworkPrefix: 120, // matches ipv4 /24: 32-24=8, 128-8=120
			allocateFirst:     []string{"layer3_1", "layer3_2"},
			release:           []string{"layer3_1"},
			allocateAgain:     []string{"layer3_3"},
			expectSubnets: map[string][]string{
				// Both IPv4 and IPv6 continue from next position
				"layer3_3": {"192.168.2.0/24", "fd00::200/120"},
			},
		},
		{
			name:              "dual-stack released subnet is reused when allocator wraps around",
			ipv4Mode:          true,
			ipv6Mode:          true,
			ipv4Network:       "192.168.0.0/24", // Small range: only 4 /26 subnets
			ipv4NetworkPrefix: 26,
			ipv6Network:       "fd00::/120",                                             // Small range: only 4 /122 subnets
			ipv6NetworkPrefix: 122,                                                      // matches ipv4 /26: 32-26=6, 128-6=122
			allocateFirst:     []string{"layer3_1", "layer3_2", "layer3_3", "layer3_4"}, // Allocate all 4
			release:           []string{"layer3_2"},                                     // Release the second one
			allocateAgain:     []string{"layer3_5"},
			expectSubnets: map[string][]string{
				// Both IPv4 and IPv6 wrap around and reuse the released subnet
				// IPv4: 192.168.0.64/26 was layer3_2; IPv6: fd00::40/122 was layer3_2
				"layer3_5": {"192.168.0.64/26", "fd00::40/122"},
			},
		},
		{
			name:              "releasing non-existent owner is safe",
			ipv4Mode:          true,
			ipv6Mode:          false,
			ipv4Network:       "192.168.0.0/16",
			ipv4NetworkPrefix: 24,
			allocateFirst:     []string{"layer3_1"},
			release:           []string{"layer3_nonexistent"},
			allocateAgain:     []string{"layer3_2"},
			expectSubnets: map[string][]string{
				"layer3_2": {"192.168.1.0/24"}, // gets next available (nothing was actually released)
			},
		},
		{
			name:              "same owner gets same subnet without re-allocating",
			ipv4Mode:          true,
			ipv6Mode:          false,
			ipv4Network:       "192.168.0.0/16",
			ipv4NetworkPrefix: 24,
			allocateFirst:     []string{"layer3_1", "layer3_2"},
			release:           []string{}, // no release
			allocateAgain:     []string{"layer3_1"},
			expectSubnets: map[string][]string{
				// Same owner gets same subnet back
				"layer3_1": {"192.168.0.0/24"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			config.IPv4Mode = tt.ipv4Mode
			config.IPv6Mode = tt.ipv6Mode

			connectSubnets := []networkconnectv1.ConnectSubnet{}
			if tt.ipv4Mode && tt.ipv4Network != "" {
				connectSubnets = append(connectSubnets, networkconnectv1.ConnectSubnet{
					CIDR:          networkconnectv1.CIDR(tt.ipv4Network),
					NetworkPrefix: int32(tt.ipv4NetworkPrefix),
				})
			}
			if tt.ipv6Mode && tt.ipv6Network != "" {
				connectSubnets = append(connectSubnets, networkconnectv1.ConnectSubnet{
					CIDR:          networkconnectv1.CIDR(tt.ipv6Network),
					NetworkPrefix: int32(tt.ipv6NetworkPrefix),
				})
			}
			allocator, err := NewHybridConnectSubnetAllocator(connectSubnets, "test-cnc")
			if err != nil {
				t.Fatalf("failed to create subnet allocator: %v", err)
			}

			// First allocation
			for _, owner := range tt.allocateFirst {
				_, err := allocator.AllocateLayer3Subnet(owner)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			// Release
			for _, owner := range tt.release {
				allocator.ReleaseLayer3Subnet(owner)
			}

			// Allocate again and verify
			for _, owner := range tt.allocateAgain {
				subnets, err := allocator.AllocateLayer3Subnet(owner)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				if expected, ok := tt.expectSubnets[owner]; ok {
					g.Expect(subnets).To(gomega.HaveLen(len(expected)))
					for i, subnet := range subnets {
						g.Expect(subnet.String()).To(gomega.Equal(expected[i]))
					}
				}
			}
		})
	}
}

func TestHybridConnectSubnetAllocator_ReleaseLayer2Subnets(t *testing.T) {
	tests := []struct {
		name              string
		ipv4Mode          bool
		ipv6Mode          bool
		ipv4Network       string
		ipv4NetworkPrefix int
		ipv6Network       string
		ipv6NetworkPrefix int
		allocateFirst     []string
		release           []string
		allocateAgain     []string
		expectSubnets     map[string][]string // owner -> expected subnets after re-allocation
	}{
		{
			name:              "IPv4 allocation continues from where it left off after release",
			ipv4Mode:          true,
			ipv6Mode:          false,
			ipv4Network:       "192.168.0.0/16",
			ipv4NetworkPrefix: 24,
			allocateFirst:     []string{"layer2_1", "layer2_2"},
			release:           []string{"layer2_1"},
			allocateAgain:     []string{"layer2_3"},
			expectSubnets: map[string][]string{
				// Allocator continues from next position, doesn't immediately reuse released subnet
				"layer2_3": {"192.168.0.4/31"},
			},
		},
		{
			name:              "IPv4 released subnet is reused before expanding to new layer3 block",
			ipv4Mode:          true,
			ipv6Mode:          false,
			ipv4Network:       "192.168.0.0/27", // Two /28 blocks available at layer3 level
			ipv4NetworkPrefix: 28,
			// Fill the first /28 block (8 /31s), then release one
			allocateFirst: []string{"layer2_1", "layer2_2", "layer2_3", "layer2_4", "layer2_5", "layer2_6", "layer2_7", "layer2_8"},
			release:       []string{"layer2_5"}, // Release 192.168.0.8/31
			allocateAgain: []string{"layer2_9"},
			expectSubnets: map[string][]string{
				// Should reuse the released /31 from first block, not expand to second /28 block
				"layer2_9": {"192.168.0.8/31"},
			},
		},
		{
			name:              "IPv6 allocation continues from where it left off after release",
			ipv4Mode:          false,
			ipv6Mode:          true,
			ipv6Network:       "fd00::/48",
			ipv6NetworkPrefix: 64,
			allocateFirst:     []string{"layer2_1", "layer2_2"},
			release:           []string{"layer2_1"},
			allocateAgain:     []string{"layer2_3"},
			expectSubnets: map[string][]string{
				// Allocator continues from next position (layer2 block gets /64, then allocates /127s)
				"layer2_3": {"fd00:0:0:1::6/127"},
			},
		},
		{
			name:              "IPv6 released subnet is reused before expanding to new layer3 block",
			ipv4Mode:          false,
			ipv6Mode:          true,
			ipv6Network:       "fd00::/123", // Two /124 blocks available at layer3 level
			ipv6NetworkPrefix: 124,          // Each /124 = 16 IPs = 8 /127s
			// Fill the first /124 block (8 /127s), then release one
			allocateFirst: []string{"layer2_1", "layer2_2", "layer2_3", "layer2_4", "layer2_5", "layer2_6", "layer2_7", "layer2_8"},
			release:       []string{"layer2_5"}, // Release the fifth one (fd00::8/127)
			allocateAgain: []string{"layer2_9"},
			expectSubnets: map[string][]string{
				// Should reuse the released /127 from first block, not expand to second /124 block
				"layer2_9": {"fd00::8/127"},
			},
		},
		{
			name:              "dual-stack allocation continues from where it left off after release",
			ipv4Mode:          true,
			ipv6Mode:          true,
			ipv4Network:       "192.168.0.0/16",
			ipv4NetworkPrefix: 24,
			ipv6Network:       "fd00::/112",
			ipv6NetworkPrefix: 120, // matches ipv4 /24: 32-24=8, 128-8=120
			allocateFirst:     []string{"layer2_1", "layer2_2"},
			release:           []string{"layer2_1"},
			allocateAgain:     []string{"layer2_3"},
			expectSubnets: map[string][]string{
				// Both IPv4 and IPv6 continue from next position (index 2)
				"layer2_3": {"192.168.0.4/31", "fd00::4/127"},
			},
		},
		{
			name:              "dual-stack released subnet is reused before expanding to new layer3 block",
			ipv4Mode:          true,
			ipv6Mode:          true,
			ipv4Network:       "192.168.0.0/27", // Two /28 blocks at layer3, each /28 = 8 /31s for layer2
			ipv4NetworkPrefix: 28,
			ipv6Network:       "fd00::/123", // Two /124 blocks at layer3, each /124 = 8 /127s for layer2
			ipv6NetworkPrefix: 124,
			// Fill the first blocks (8 /31s for IPv4, 8 /127s for IPv6), then release one
			allocateFirst: []string{"layer2_1", "layer2_2", "layer2_3", "layer2_4", "layer2_5", "layer2_6", "layer2_7", "layer2_8"},
			release:       []string{"layer2_5"}, // Release the fifth one
			allocateAgain: []string{"layer2_9"},
			expectSubnets: map[string][]string{
				// Should reuse the released subnets from first blocks, not expand to second blocks
				"layer2_9": {"192.168.0.8/31", "fd00::8/127"},
			},
		},
		{
			name:              "releasing non-existent owner is safe",
			ipv4Mode:          true,
			ipv6Mode:          false,
			ipv4Network:       "192.168.0.0/16",
			ipv4NetworkPrefix: 24,
			allocateFirst:     []string{"layer2_1"},
			release:           []string{"layer2_nonexistent"},
			allocateAgain:     []string{"layer2_2"},
			expectSubnets: map[string][]string{
				"layer2_2": {"192.168.0.2/31"}, // gets next available (nothing was actually released)
			},
		},
		{
			name:              "same owner gets same subnet without re-allocating",
			ipv4Mode:          true,
			ipv6Mode:          false,
			ipv4Network:       "192.168.0.0/16",
			ipv4NetworkPrefix: 24,
			allocateFirst:     []string{"layer2_1", "layer2_2"},
			release:           []string{}, // no release
			allocateAgain:     []string{"layer2_1"},
			expectSubnets: map[string][]string{
				// Same owner gets same subnet back
				"layer2_1": {"192.168.0.0/31"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			config.IPv4Mode = tt.ipv4Mode
			config.IPv6Mode = tt.ipv6Mode

			connectSubnets := []networkconnectv1.ConnectSubnet{}
			if tt.ipv4Mode && tt.ipv4Network != "" {
				connectSubnets = append(connectSubnets, networkconnectv1.ConnectSubnet{
					CIDR:          networkconnectv1.CIDR(tt.ipv4Network),
					NetworkPrefix: int32(tt.ipv4NetworkPrefix),
				})
			}
			if tt.ipv6Mode && tt.ipv6Network != "" {
				connectSubnets = append(connectSubnets, networkconnectv1.ConnectSubnet{
					CIDR:          networkconnectv1.CIDR(tt.ipv6Network),
					NetworkPrefix: int32(tt.ipv6NetworkPrefix),
				})
			}
			allocator, err := NewHybridConnectSubnetAllocator(connectSubnets, "test-cnc")
			g.Expect(err).ToNot(gomega.HaveOccurred())

			// First allocation
			for _, owner := range tt.allocateFirst {
				_, err := allocator.AllocateLayer2Subnet(owner)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			// Release
			for _, owner := range tt.release {
				allocator.ReleaseLayer2Subnet(owner)
			}

			// Allocate again and verify
			for _, owner := range tt.allocateAgain {
				subnets, err := allocator.AllocateLayer2Subnet(owner)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				if expected, ok := tt.expectSubnets[owner]; ok {
					g.Expect(subnets).To(gomega.HaveLen(len(expected)))
					for i, subnet := range subnets {
						g.Expect(subnet.String()).To(gomega.Equal(expected[i]))
					}
				}
			}
		})
	}
}

func TestHybridConnectSubnetAllocator_ReleaseMixedLayer3AndLayer2Subnets(t *testing.T) {
	g := gomega.NewWithT(t)

	config.IPv4Mode = true
	config.IPv6Mode = true

	connectSubnets := []networkconnectv1.ConnectSubnet{
		{
			CIDR:          networkconnectv1.CIDR("192.168.0.0/16"),
			NetworkPrefix: 24,
		},
		{
			CIDR:          networkconnectv1.CIDR("fd00::/112"),
			NetworkPrefix: 120, // matches ipv4 /24: 32-24=8, 128-8=120
		},
	}
	allocator, err := NewHybridConnectSubnetAllocator(connectSubnets, "test-cnc")
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Allocate layer3 subnets (dual-stack: both IPv4 and IPv6)
	// With /120 prefix, IPv6 blocks are fd00::/120, fd00::100/120, etc.
	l3Sub1, err := allocator.AllocateLayer3Subnet("layer3_1")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l3Sub1).To(gomega.HaveLen(2))
	g.Expect(l3Sub1[0].String()).To(gomega.Equal("192.168.0.0/24"))
	g.Expect(l3Sub1[1].String()).To(gomega.Equal("fd00::/120"))

	l3Sub2, err := allocator.AllocateLayer3Subnet("layer3_2")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l3Sub2).To(gomega.HaveLen(2))
	g.Expect(l3Sub2[0].String()).To(gomega.Equal("192.168.1.0/24"))
	g.Expect(l3Sub2[1].String()).To(gomega.Equal("fd00::100/120"))

	// Allocate layer2 subnets (will get new blocks from layer3: 192.168.2.0/24 and fd00::200/120)
	l2Sub1, err := allocator.AllocateLayer2Subnet("layer2_1")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l2Sub1).To(gomega.HaveLen(2))
	g.Expect(l2Sub1[0].String()).To(gomega.Equal("192.168.2.0/31"))
	g.Expect(l2Sub1[1].String()).To(gomega.Equal("fd00::200/127"))

	l2Sub2, err := allocator.AllocateLayer2Subnet("layer2_2")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l2Sub2).To(gomega.HaveLen(2))
	g.Expect(l2Sub2[0].String()).To(gomega.Equal("192.168.2.2/31"))
	g.Expect(l2Sub2[1].String()).To(gomega.Equal("fd00::202/127"))

	// Release one layer3 and one layer2
	allocator.ReleaseLayer3Subnet("layer3_1")
	allocator.ReleaseLayer2Subnet("layer2_1")

	// Allocate new layer3 - allocator continues from where it left off
	// IPv4: 192.168.2.0/24 is taken by layer2-block, so next available is 192.168.3.0/24
	// IPv6: fd00::200/120 is taken by layer2-block, so next available is fd00::300/120
	l3Sub3, err := allocator.AllocateLayer3Subnet("layer3_3")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l3Sub3).To(gomega.HaveLen(2))
	g.Expect(l3Sub3[0].String()).To(gomega.Equal("192.168.3.0/24"))
	g.Expect(l3Sub3[1].String()).To(gomega.Equal("fd00::300/120"))

	// Allocate new layer2 - allocator continues from where it left off
	l2Sub3, err := allocator.AllocateLayer2Subnet("layer2_3")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l2Sub3).To(gomega.HaveLen(2))
	g.Expect(l2Sub3[0].String()).To(gomega.Equal("192.168.2.4/31"))
	g.Expect(l2Sub3[1].String()).To(gomega.Equal("fd00::204/127"))

	// Verify layer3 and layer2 allocators are independent
	// Allocate more layer2 - should continue in layer2 block
	l2Sub4, err := allocator.AllocateLayer2Subnet("layer2_4")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l2Sub4).To(gomega.HaveLen(2))
	g.Expect(l2Sub4[0].String()).To(gomega.Equal("192.168.2.6/31"))
	g.Expect(l2Sub4[1].String()).To(gomega.Equal("fd00::206/127"))

	// Allocate layer3 - should continue in layer3 space
	l3Sub4, err := allocator.AllocateLayer3Subnet("layer3_4")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l3Sub4).To(gomega.HaveLen(2))
	g.Expect(l3Sub4[0].String()).To(gomega.Equal("192.168.4.0/24"))
	g.Expect(l3Sub4[1].String()).To(gomega.Equal("fd00::400/120"))

	// Verify released owner gets new subnet (not the old one)
	l3Sub1Again, err := allocator.AllocateLayer3Subnet("layer3_1")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l3Sub1Again).To(gomega.HaveLen(2))
	// layer3_1 was released, so new allocation continues from next position
	g.Expect(l3Sub1Again[0].String()).To(gomega.Equal("192.168.5.0/24"))
	g.Expect(l3Sub1Again[1].String()).To(gomega.Equal("fd00::500/120"))
}

func TestHybridConnectSubnetAllocator_Layer2BlockExpansionFromLayer3(t *testing.T) {
	g := gomega.NewWithT(t)

	config.IPv4Mode = true
	config.IPv6Mode = true

	// Use small CIDRs to test block exhaustion and expansion
	// IPv4: 192.168.0.0/24 with /28 prefix gives us 16 /28 blocks, each holding 8 /31 subnets
	// IPv6: fd00::/120 with /124 prefix gives us 16 /124 blocks, each holding 8 /127 subnets
	connectSubnets := []networkconnectv1.ConnectSubnet{
		{
			CIDR:          networkconnectv1.CIDR("192.168.0.0/24"),
			NetworkPrefix: 28,
		},
		{
			CIDR:          networkconnectv1.CIDR("fd00::/120"),
			NetworkPrefix: 124,
		},
	}
	allocator, err := NewHybridConnectSubnetAllocator(connectSubnets, "test-cnc")
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Allocate more than 8 subnets to trigger expansion of new blocks from layer3 to layer2
	// First 8 allocations will use the first /28 (IPv4) and /124 (IPv6) blocks
	// 9th allocation should trigger expansion - allocating new blocks from layer3
	layer2Subnets := make(map[string][]*net.IPNet)
	for i := 1; i <= 20; i++ { // Allocate 20 to span multiple blocks
		owner := fmt.Sprintf("layer2_%d", i)
		subnets, err := allocator.AllocateLayer2Subnet(owner)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(subnets).To(gomega.HaveLen(2)) // dual-stack: IPv4 + IPv6
		layer2Subnets[owner] = subnets

		// Verify IPv4 is /31
		ipv4Ones, _ := subnets[0].Mask.Size()
		g.Expect(ipv4Ones).To(gomega.Equal(31))

		// Verify IPv6 is /127
		ipv6Ones, _ := subnets[1].Mask.Size()
		g.Expect(ipv6Ones).To(gomega.Equal(127))
	}

	// All 20 allocations should have succeeded
	g.Expect(layer2Subnets).To(gomega.HaveLen(20))

	// Verify IPv4 subnets span multiple /28 blocks (i.e., expansion happened)
	ipv4BlocksSeen := make(map[string]bool)
	for _, subnets := range layer2Subnets {
		ip := subnets[0].IP.To4()
		blockStart := ip[3] & 0xF0 // mask to /28 boundary
		blockKey := fmt.Sprintf("192.168.0.%d/28", blockStart)
		ipv4BlocksSeen[blockKey] = true
	}
	// With 20 /31 allocations, we need at least 3 /28 blocks (8 + 8 + 4)
	g.Expect(len(ipv4BlocksSeen)).To(gomega.BeNumerically(">=", 3),
		"Expected at least 3 IPv4 /28 blocks to be used, got %d: %v", len(ipv4BlocksSeen), ipv4BlocksSeen)

	// Verify IPv6 subnets span multiple /124 blocks (i.e., expansion happened)
	ipv6BlocksSeen := make(map[string]bool)
	for _, subnets := range layer2Subnets {
		ip := subnets[1].IP.To16()
		// For /124, the block boundary is at the last nibble (4 bits)
		blockStart := ip[15] & 0xF0 // mask to /124 boundary
		blockKey := fmt.Sprintf("fd00::%x/124", blockStart)
		ipv6BlocksSeen[blockKey] = true
	}
	// With 20 /127 allocations, we need at least 3 /124 blocks (8 + 8 + 4)
	g.Expect(len(ipv6BlocksSeen)).To(gomega.BeNumerically(">=", 3),
		"Expected at least 3 IPv6 /124 blocks to be used, got %d: %v", len(ipv6BlocksSeen), ipv6BlocksSeen)
}

func TestHybridConnectSubnetAllocator_Layer3RangeFull(t *testing.T) {
	g := gomega.NewWithT(t)

	config.IPv4Mode = true
	config.IPv6Mode = false

	// Use a very small CIDR that can only hold 4 /26 subnets
	// 192.168.0.0/24 with /26 prefix = 4 subnets (256 IPs / 64 IPs per /26 = 4)
	connectSubnets := []networkconnectv1.ConnectSubnet{
		{
			CIDR:          networkconnectv1.CIDR("192.168.0.0/24"),
			NetworkPrefix: 26,
		},
	}
	allocator, err := NewHybridConnectSubnetAllocator(connectSubnets, "test-cnc")
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Allocate all 4 available /26 subnets
	for i := 1; i <= 4; i++ {
		owner := fmt.Sprintf("layer3_%d", i)
		subnets, err := allocator.AllocateLayer3Subnet(owner)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(subnets).To(gomega.HaveLen(1))

		// Verify it's a /26
		ones, _ := subnets[0].Mask.Size()
		g.Expect(ones).To(gomega.Equal(26))
	}

	// 5th allocation should fail - range is exhausted
	_, err = allocator.AllocateLayer3Subnet("layer3_5")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("Layer3 allocation failed"))
}

func TestHybridConnectSubnetAllocator_Layer2RangeFullAfterLayer3Exhausted(t *testing.T) {
	g := gomega.NewWithT(t)

	config.IPv4Mode = true
	config.IPv6Mode = true

	// Use very small CIDRs that can only hold 2 subnets each
	// IPv4: 192.168.0.0/24 with /25 prefix = 2 subnets
	// IPv6: fd00::/121 with /122 prefix = 2 subnets
	connectSubnets := []networkconnectv1.ConnectSubnet{
		{
			CIDR:          networkconnectv1.CIDR("192.168.0.0/24"),
			NetworkPrefix: 25,
		},
		{
			CIDR:          networkconnectv1.CIDR("fd00::/114"),
			NetworkPrefix: 121, // matches ipv4 /25: 32-25=7, 128-7=121
		},
	}
	allocator, err := NewHybridConnectSubnetAllocator(connectSubnets, "test-cnc")
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Allocate both subnets as layer3 (exhausts both IPv4 and IPv6 ranges)
	for i := 1; i <= 2; i++ {
		owner := fmt.Sprintf("layer3_%d", i)
		subnets, err := allocator.AllocateLayer3Subnet(owner)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(subnets).To(gomega.HaveLen(2)) // dual-stack: one IPv4 + one IPv6
	}

	// Now try to allocate layer2 - this should fail because
	// layer2 needs to expand by getting blocks from layer3, but layer3 is exhausted
	_, err = allocator.AllocateLayer2Subnet("layer2_1")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("failed to expand Layer2 allocator"))
}

func TestHybridConnectSubnetAllocator_Layer2CanReuseFromEarlierRange(t *testing.T) {
	// This test confirms that when the layer2 allocator has multiple ranges (blocks),
	// it can reuse a released slot from an EARLIER range, not just the most recent one.
	// This is important for understanding block release behavior.
	g := gomega.NewWithT(t)

	config.IPv4Mode = true
	config.IPv6Mode = false

	// Small range: 192.168.0.0/26 with /28 prefix = 4 /28 blocks
	// Each /28 block has 8 /31 slots
	connectSubnets := []networkconnectv1.ConnectSubnet{
		{
			CIDR:          networkconnectv1.CIDR("192.168.0.0/26"),
			NetworkPrefix: 28,
		},
	}
	allocator, err := NewHybridConnectSubnetAllocator(connectSubnets, "test-cnc")
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Allocate 8 layer2 networks - this fills the first /28 block
	// First allocation triggers expansion, gets 192.168.0.0/28
	firstBlockOwners := make([]string, 8)
	for i := 0; i < 8; i++ {
		owner := fmt.Sprintf("layer2_block1_%d", i)
		firstBlockOwners[i] = owner
		subnets, err := allocator.AllocateLayer2Subnet(owner)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(subnets).To(gomega.HaveLen(1))
		// All should be in 192.168.0.0/28 range (192.168.0.0 - 192.168.0.15)
		g.Expect(subnets[0].IP[3]).To(gomega.BeNumerically("<", 16))
	}

	// Allocate one more - this triggers expansion to second /28 block (192.168.0.16/28)
	secondBlockOwner := "layer2_block2_0"
	subnets, err := allocator.AllocateLayer2Subnet(secondBlockOwner)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(subnets).To(gomega.HaveLen(1))
	// Should be in second block (192.168.0.16/28 range)
	g.Expect(subnets[0].String()).To(gomega.Equal("192.168.0.16/31"))

	// Now release one from the FIRST block
	allocator.ReleaseLayer2Subnet(firstBlockOwners[0]) // releases 192.168.0.0/31

	// Allocate again - should reuse the released slot from the FIRST block
	// (not allocate from the second block which also has free slots)
	newOwner := "layer2_new"
	subnets, err = allocator.AllocateLayer2Subnet(newOwner)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(subnets).To(gomega.HaveLen(1))

	// The key assertion: the allocation should reuse 192.168.0.0/31 from first block
	// This confirms the allocator can pick from earlier ranges, not just the latest
	g.Expect(subnets[0].String()).To(gomega.Equal("192.168.0.0/31"))
}

func TestHybridConnectSubnetAllocator_Layer2ReleaseReleasesBlockToLayer3(t *testing.T) {
	// Test that blocks are released back to layer3 only when ALL layer2 owners
	// in that block are released. Also verifies the range is removed from layer2Allocator.
	// This test covers:
	// 1. Partial release (block NOT released back to layer3)
	// 2. Full release (block released back to layer3 AND removed from layer2Allocator)
	g := gomega.NewWithT(t)

	config.IPv4Mode = true
	config.IPv6Mode = true

	connectSubnets := []networkconnectv1.ConnectSubnet{
		{
			CIDR:          networkconnectv1.CIDR("10.100.0.0/26"),
			NetworkPrefix: 28, // 32-28=4 host bits
		},
		{
			CIDR:          networkconnectv1.CIDR("fd00::/122"),
			NetworkPrefix: 124, // 128-124=4 host bits (matches IPv4)
		},
	}
	allocator, err := NewHybridConnectSubnetAllocator(connectSubnets, "test-cnc")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	// Small ranges: 4 blocks each
	// IPv4: 10.100.0.0/26 with /28 prefix = 4 /28 blocks (each has 8 /31 slots)
	// IPv6: fd00::/122 with /124 prefix = 4 /124 blocks (each has 8 /127 slots)

	// Allocate 2 layer2 networks - both in the same block
	l2Sub1, err := allocator.AllocateLayer2Subnet("layer2_1")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l2Sub1).To(gomega.HaveLen(2)) // dual-stack
	g.Expect(l2Sub1[0].String()).To(gomega.Equal("10.100.0.0/31"))
	// /124 to /127: subnetBits = 3, which is < 16, so it doesn't skip subnet 0
	g.Expect(l2Sub1[1].String()).To(gomega.Equal("fd00::/127"))

	l2Sub2, err := allocator.AllocateLayer2Subnet("layer2_2")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l2Sub2).To(gomega.HaveLen(2)) // dual-stack

	// Verify layer2 allocator has 1 range each (the block)
	v4RangeCount, v6RangeCount := allocator.Layer2RangeCount()
	g.Expect(v4RangeCount).To(gomega.Equal(uint64(1)))
	g.Expect(v6RangeCount).To(gomega.Equal(uint64(1)))

	// Use up the remaining layer3 blocks
	_, err = allocator.AllocateLayer3Subnet("layer3_1")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	_, err = allocator.AllocateLayer3Subnet("layer3_2")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	_, err = allocator.AllocateLayer3Subnet("layer3_3")
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Layer3 should be full now (1 block used by layer2 networks + 3 blocks used by layer3)
	_, err = allocator.AllocateLayer3Subnet("layer3_should_fail")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("Layer3 allocation failed"))

	// PARTIAL RELEASE: Release only ONE layer2 network - block should NOT be released
	allocator.ReleaseLayer2Subnet("layer2_1")

	// Layer2 allocator should still have 1 range each (block not released yet)
	v4RangeCount, v6RangeCount = allocator.Layer2RangeCount()
	g.Expect(v4RangeCount).To(gomega.Equal(uint64(1)))
	g.Expect(v6RangeCount).To(gomega.Equal(uint64(1)))

	// Layer3 should still be full (block not released because layer2_2 still using it)
	_, err = allocator.AllocateLayer3Subnet("layer3_still_full")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("Layer3 allocation failed"))

	// FULL RELEASE: Release the other layer2 network - block should now be released
	allocator.ReleaseLayer2Subnet("layer2_2")

	// Layer2 allocator should now have 0 ranges (block removed via FreeUnusedRanges)
	v4RangeCount, v6RangeCount = allocator.Layer2RangeCount()
	g.Expect(v4RangeCount).To(gomega.Equal(uint64(0)))
	g.Expect(v6RangeCount).To(gomega.Equal(uint64(0)))

	// Now layer3 should have a free block (the block was released back)
	l3Sub4, err := allocator.AllocateLayer3Subnet("layer3_4")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l3Sub4).To(gomega.HaveLen(2)) // dual-stack
	// Should get the block that was released from layer2 networks
	g.Expect(l3Sub4[0].String()).To(gomega.Equal("10.100.0.0/28"))
	g.Expect(l3Sub4[1].String()).To(gomega.Equal("fd00::/124"))
}

func TestHybridConnectSubnetAllocator_Layer2DesyncBugWithMismatchedNetworkPrefix(t *testing.T) {
	// BUG DEMONSTRATION: When IPv4 and IPv6 have different networkPrefix "host bits",
	// they have different capacities per block. When one fills up before the other,
	// the allocator expands but the next allocation gets IPv4 from new block and
	// IPv6 from old block (still has room). This causes desync and breaks block release.
	//
	// Formula for matching: 32 - v4NetworkPrefix == 128 - v6NetworkPrefix
	// This test intentionally uses MISMATCHED prefixes to demonstrate the bug.
	// We have added CEL validation for this on the API so that its not possible, but
	// this test is left here for reference and to ensure we don't use the allocator
	// in this fashion.
	t.Skip("This test demonstrates the desync bug that CEL validation prevents - skipped in CI but kept for documentation")
	g := gomega.NewWithT(t)

	config.IPv4Mode = true
	config.IPv6Mode = true

	// MISMATCHED networkPrefix values:
	// IPv4 /28: host bits = 32 - 28 = 4 → 2^4 / 2 = 8 /31 slots per block
	// IPv6 /123: host bits = 128 - 123 = 5 → 2^5 / 2 = 16 /127 slots per block
	// IPv4 will fill up FIRST!
	//
	// Use SMALL CIDRs so we can detect leaks:
	// IPv4: /26 with /28 = 4 blocks only
	// IPv6: /121 with /123 = 4 blocks only
	connectSubnets := []networkconnectv1.ConnectSubnet{
		{
			CIDR:          networkconnectv1.CIDR("10.100.0.0/26"), // Only 4 /28 blocks
			NetworkPrefix: 28,                                     // 8 /31 slots per block
		},
		{
			CIDR:          networkconnectv1.CIDR("fd00::/121"), // Only 4 /123 blocks
			NetworkPrefix: 123,                                 // 16 /127 slots per block
		},
	}
	allocator, err := NewHybridConnectSubnetAllocator(connectSubnets, "test-cnc")
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Allocate 9 L2 networks - this will exceed IPv4's 8 slots per block
	// Networks 1-8: both from block1
	// Network 9: IPv4 from block2, IPv6 from block1 (DESYNC!)
	l2Owners := make([]string, 9)
	for i := 0; i < 9; i++ {
		owner := fmt.Sprintf("layer2_%d", i)
		l2Owners[i] = owner
		subnets, err := allocator.AllocateLayer2Subnet(owner)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(subnets).To(gomega.HaveLen(2)) // dual-stack

		if i == 8 {
			// Network 9 (index 8) should show the desync:
			// IPv4: 10.100.0.16/31 (first slot in block2 = 10.100.0.16/28)
			// IPv6: fd00::10/127 (slot 9 in block1 = fd00::/123)
			t.Logf("Network 9 (desynced): IPv4=%s, IPv6=%s", subnets[0].String(), subnets[1].String())

			// IPv4 should be from block2 (10.100.0.16/28)
			g.Expect(subnets[0].String()).To(gomega.Equal("10.100.0.16/31"))
			// IPv6 should still be from block1 (fd00::/123) - index 8
			g.Expect(subnets[1].String()).To(gomega.Equal("fd00::10/127"))
		}
	}

	// Both families have 2 blocks because expandLayer2Allocator() adds blocks for both
	// But the ALLOCATION is desynced: IPv4 from block2, IPv6 from block1
	v4RangeCount, v6RangeCount := allocator.Layer2RangeCount()
	g.Expect(v4RangeCount).To(gomega.Equal(uint64(2)), "IPv4 should have 2 blocks")
	g.Expect(v6RangeCount).To(gomega.Equal(uint64(2)), "IPv6 should have 2 blocks (both added, but only block1 used)")

	// Now release all L2 networks and watch the bug manifest
	// When we release networks 1-8, block1 should be freed for both families
	// But network 9's IPv4 is in block2 alone - the release will fail to find the owner!
	for i := 0; i < 9; i++ {
		allocator.ReleaseLayer2Subnet(l2Owners[i])
	}

	// Layer2 allocator removes ranges via FreeUnusedRanges() regardless of layer3 release success
	v4RangeCount, v6RangeCount = allocator.Layer2RangeCount()
	t.Logf("After release - layer2 IPv4 ranges: %d, IPv6 ranges: %d", v4RangeCount, v6RangeCount)
	g.Expect(v4RangeCount).To(gomega.Equal(uint64(0)))
	g.Expect(v6RangeCount).To(gomega.Equal(uint64(0)))

	// THE REAL BUG: Check layer3 allocator for leaked blocks via Usage()
	// L2 used 2 blocks during allocation (block1 for networks 0-7, block2 for network 8's IPv4)
	// If properly released, layer3 should have 0 allocated blocks
	// If leaked, some blocks are still marked as allocated
	l3v4Usage, l3v6Usage := allocator.Layer3Usage()
	t.Logf("Layer3 usage - IPv4: %d, IPv6: %d (expected 0 each if no leak)", l3v4Usage, l3v6Usage)

	// BUG: Usage should be 0 after releasing all L2 networks
	// If blocks leaked (weren't released back to layer3), usage will be > 0
	g.Expect(l3v4Usage).To(gomega.Equal(uint64(0)), "BUG: IPv4 blocks leaked in layer3 - usage should be 0 but got %d", l3v4Usage)
	g.Expect(l3v6Usage).To(gomega.Equal(uint64(0)), "BUG: IPv6 blocks leaked in layer3 - usage should be 0 but got %d", l3v6Usage)
}

func TestHybridConnectSubnetAllocator_getParentBlockCIDR(t *testing.T) {
	// Test the mathematical derivation of parent block CIDR from a subnet
	// The function masks the subnet IP to the networkPrefix boundary
	tests := []struct {
		name            string
		v4NetworkPrefix int
		v6NetworkPrefix int
		subnet          string
		expectedParent  string
	}{
		// IPv4 tests with /28 networkPrefix (using 10.0.0.0/8 range)
		{
			name:            "IPv4 first address in block",
			v4NetworkPrefix: 28,
			subnet:          "10.20.30.0/31",
			expectedParent:  "10.20.30.0/28",
		},
		{
			name:            "IPv4 middle address in block",
			v4NetworkPrefix: 28,
			subnet:          "10.20.30.6/31",
			expectedParent:  "10.20.30.0/28",
		},
		{
			name:            "IPv4 last address in block",
			v4NetworkPrefix: 28,
			subnet:          "10.20.30.14/31",
			expectedParent:  "10.20.30.0/28",
		},
		{
			name:            "IPv4 second block first address",
			v4NetworkPrefix: 28,
			subnet:          "10.20.30.16/31",
			expectedParent:  "10.20.30.16/28",
		},
		{
			name:            "IPv4 second block middle address",
			v4NetworkPrefix: 28,
			subnet:          "10.20.30.22/31",
			expectedParent:  "10.20.30.16/28",
		},
		// IPv4 with /24 networkPrefix (using 172.16.0.0/12 range)
		{
			name:            "IPv4 /24 prefix first block",
			v4NetworkPrefix: 24,
			subnet:          "172.16.5.100/31",
			expectedParent:  "172.16.5.0/24",
		},
		{
			name:            "IPv4 /24 prefix second block",
			v4NetworkPrefix: 24,
			subnet:          "172.16.6.50/31",
			expectedParent:  "172.16.6.0/24",
		},
		// IPv6 tests with /124 networkPrefix
		{
			name:            "IPv6 first address in block",
			v6NetworkPrefix: 124,
			subnet:          "2001:db8::0/127",
			expectedParent:  "2001:db8::/124",
		},
		{
			name:            "IPv6 middle address in block",
			v6NetworkPrefix: 124,
			subnet:          "2001:db8::6/127",
			expectedParent:  "2001:db8::/124",
		},
		{
			name:            "IPv6 last address in block",
			v6NetworkPrefix: 124,
			subnet:          "2001:db8::e/127",
			expectedParent:  "2001:db8::/124",
		},
		{
			name:            "IPv6 second block",
			v6NetworkPrefix: 124,
			subnet:          "2001:db8::10/127",
			expectedParent:  "2001:db8::10/124",
		},
		{
			name:            "IPv6 second block middle",
			v6NetworkPrefix: 124,
			subnet:          "2001:db8::1a/127",
			expectedParent:  "2001:db8::10/124",
		},
		// IPv6 with /64 networkPrefix
		{
			name:            "IPv6 /64 prefix",
			v6NetworkPrefix: 64,
			subnet:          "2001:db8:cafe:1::abcd/127",
			expectedParent:  "2001:db8:cafe:1::/64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			// Create allocator with the specified network prefixes
			allocator := &hybridConnectSubnetAllocator{
				v4NetworkPrefix: tt.v4NetworkPrefix,
				v6NetworkPrefix: tt.v6NetworkPrefix,
			}

			subnet := mustParseCIDR(tt.subnet)
			parentCIDR := allocator.getParentBlockCIDR(subnet)

			g.Expect(parentCIDR.String()).To(gomega.Equal(tt.expectedParent))
		})
	}
}

func TestHybridConnectSubnetAllocator_getParentBlockCIDR_AllAddressesInBlockMapToSameParent(t *testing.T) {
	// Verify that ALL addresses within a block map to the same parent
	g := gomega.NewWithT(t)

	allocator := &hybridConnectSubnetAllocator{
		v4NetworkPrefix: 28, // /28 = 16 addresses (0-15)
	}

	// All /31 subnets in 10.50.100.0/28 should map to the same parent
	// Block has addresses .0 to .15, so /31 subnets are .0, .2, .4, .6, .8, .10, .12, .14
	expectedParent := "10.50.100.0/28"
	for i := 0; i < 16; i += 2 {
		subnet := mustParseCIDR(fmt.Sprintf("10.50.100.%d/31", i))
		parentCIDR := allocator.getParentBlockCIDR(subnet)
		g.Expect(parentCIDR.String()).To(gomega.Equal(expectedParent), "Address 10.50.100.%d/31 should map to %s", i, expectedParent)
	}

	// All /31 subnets in 10.50.100.16/28 should map to a different parent
	expectedParent2 := "10.50.100.16/28"
	for i := 16; i < 32; i += 2 {
		subnet := mustParseCIDR(fmt.Sprintf("10.50.100.%d/31", i))
		parentCIDR := allocator.getParentBlockCIDR(subnet)
		g.Expect(parentCIDR.String()).To(gomega.Equal(expectedParent2), "Address 10.50.100.%d/31 should map to %s", i, expectedParent2)
	}
}

// newAllocationCheck verifies new allocations don't conflict with marked subnets
type newAllocationCheck struct {
	owner    string
	topology string // types.Layer3Topology or types.Layer2Topology
	notIPv4  string // expected to NOT be this IPv4 CIDR
	notIPv6  string // expected to NOT be this IPv6 CIDR
}

func TestHybridConnectSubnetAllocator_MarkAllocatedSubnets(t *testing.T) {
	tests := []struct {
		name string
		// ipv4Mode and ipv6Mode configure the IP mode
		ipv4Mode bool
		ipv6Mode bool
		// allocatedSubnets is the map of owner -> subnets to mark as allocated
		allocatedSubnets map[string][]*net.IPNet
		// verifyAllocations checks that re-allocating returns exact same subnets
		verifyAllocations []expectedSubnetAllocation
		// verifyBlocks checks layer2 block state (expected block CIDRs in layer2BlockOwners)
		verifyBlocks []string
		// newAllocation verifies new allocations don't conflict
		newAllocation *newAllocationCheck
	}{
		{
			name:     "marks layer3 subnets - re-allocation returns same subnets",
			ipv4Mode: true,
			ipv6Mode: true,
			allocatedSubnets: map[string][]*net.IPNet{
				// IPv6 /120 blocks within the fd00:10:244::/112 range
				"layer3_1": {mustParseCIDR("192.168.0.0/24"), mustParseCIDR("fd00:10:244::/120")},
				"layer3_2": {mustParseCIDR("192.168.1.0/24"), mustParseCIDR("fd00:10:244::100/120")},
			},
			verifyAllocations: []expectedSubnetAllocation{
				{owner: "layer3_1", topology: types.Layer3Topology, ipv4: "192.168.0.0/24", ipv6: "fd00:10:244::/120"},
				{owner: "layer3_2", topology: types.Layer3Topology, ipv4: "192.168.1.0/24", ipv6: "fd00:10:244::100/120"},
			},
			newAllocation: &newAllocationCheck{
				owner:    "layer3_3",
				topology: types.Layer3Topology,
				notIPv4:  "192.168.0.0/24",
				notIPv6:  "fd00:10:244::/120",
			},
		},
		{
			name:     "marks layer2 subnets - re-allocation returns same subnets and blocks are tracked",
			ipv4Mode: true,
			ipv6Mode: true,
			allocatedSubnets: map[string][]*net.IPNet{
				// /127 subnets within the fd00:10:244::/120 block
				"layer2_100": {mustParseCIDR("192.168.0.0/31"), mustParseCIDR("fd00:10:244::/127")},
				"layer2_101": {mustParseCIDR("192.168.0.2/31"), mustParseCIDR("fd00:10:244::2/127")},
			},
			verifyAllocations: []expectedSubnetAllocation{
				{owner: "layer2_100", topology: types.Layer2Topology, ipv4: "192.168.0.0/31", ipv6: "fd00:10:244::/127"},
				{owner: "layer2_101", topology: types.Layer2Topology, ipv4: "192.168.0.2/31", ipv6: "fd00:10:244::2/127"},
			},
			// In dual-stack, getL2BlocksKey creates combined key "v4,v6" with parent blocks
			verifyBlocks: []string{"192.168.0.0/24,fd00:10:244::/120"},
			newAllocation: &newAllocationCheck{
				owner:    "layer2_102",
				topology: types.Layer2Topology,
				notIPv4:  "192.168.0.0/31",
				notIPv6:  "fd00:10:244::/127",
			},
		},
		{
			name:     "marks mixed layer3 and layer2 subnets",
			ipv4Mode: true,
			ipv6Mode: true,
			allocatedSubnets: map[string][]*net.IPNet{
				// IPv6 /120 blocks within the fd00:10:244::/112 range
				"layer3_5": {mustParseCIDR("192.168.0.0/24"), mustParseCIDR("fd00:10:244::/120")},
				"layer2_6": {mustParseCIDR("192.168.1.0/31"), mustParseCIDR("fd00:10:244::100/127")},
			},
			verifyAllocations: []expectedSubnetAllocation{
				{owner: "layer3_5", topology: types.Layer3Topology, ipv4: "192.168.0.0/24", ipv6: "fd00:10:244::/120"},
				{owner: "layer2_6", topology: types.Layer2Topology, ipv4: "192.168.1.0/31", ipv6: "fd00:10:244::100/127"},
			},
			// In dual-stack, getL2BlocksKey creates combined key "v4,v6" with parent blocks
			verifyBlocks: []string{"192.168.1.0/24,fd00:10:244::100/120"},
		},
		{
			name:     "marks IPv4-only layer3 subnets",
			ipv4Mode: true,
			ipv6Mode: false,
			allocatedSubnets: map[string][]*net.IPNet{
				// Use subnets from 192.168.0.0/16 range which is the first range added
				"layer3_10": {mustParseCIDR("192.168.0.0/24")},
				"layer3_11": {mustParseCIDR("192.168.1.0/24")},
			},
			verifyAllocations: []expectedSubnetAllocation{
				{owner: "layer3_10", topology: types.Layer3Topology, ipv4: "192.168.0.0/24"},
				{owner: "layer3_11", topology: types.Layer3Topology, ipv4: "192.168.1.0/24"},
			},
		},
		{
			name:     "marks layer2 subnets from multiple blocks with ref count tracking",
			ipv4Mode: true,
			ipv6Mode: false,
			allocatedSubnets: map[string][]*net.IPNet{
				// All from same block: 192.168.0.0/24
				// This tests that multiple owners share the same block
				"layer2_1": {mustParseCIDR("192.168.0.0/31")},
				"layer2_2": {mustParseCIDR("192.168.0.2/31")},
				"layer2_3": {mustParseCIDR("192.168.0.4/31")},
			},
			verifyAllocations: []expectedSubnetAllocation{
				{owner: "layer2_1", topology: types.Layer2Topology, ipv4: "192.168.0.0/31"},
				{owner: "layer2_2", topology: types.Layer2Topology, ipv4: "192.168.0.2/31"},
				{owner: "layer2_3", topology: types.Layer2Topology, ipv4: "192.168.0.4/31"},
			},
			verifyBlocks: []string{"192.168.0.0/24"},
		},
		{
			name:             "handles empty allocatedSubnets",
			ipv4Mode:         true,
			ipv6Mode:         true,
			allocatedSubnets: map[string][]*net.IPNet{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			config.IPv4Mode = tt.ipv4Mode
			config.IPv6Mode = tt.ipv6Mode

			connectSubnets := []networkconnectv1.ConnectSubnet{}
			if tt.ipv4Mode {
				connectSubnets = append(connectSubnets, networkconnectv1.ConnectSubnet{
					CIDR:          networkconnectv1.CIDR("192.168.0.0/16"),
					NetworkPrefix: 24,
				})
			}
			if tt.ipv6Mode {
				connectSubnets = append(connectSubnets, networkconnectv1.ConnectSubnet{
					CIDR:          networkconnectv1.CIDR("fd00:10:244::/112"),
					NetworkPrefix: 120, // matches ipv4 /24: 32-24=8, 128-8=120
				})
			}
			allocator, err := NewHybridConnectSubnetAllocator(connectSubnets, "test-cnc")
			g.Expect(err).ToNot(gomega.HaveOccurred())

			// Mark allocated subnets
			err = allocator.MarkAllocatedSubnets(tt.allocatedSubnets)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			// Verify re-allocations return exact same subnets
			for _, verify := range tt.verifyAllocations {
				var subnets []*net.IPNet
				var err error
				if verify.topology == types.Layer2Topology {
					subnets, err = allocator.AllocateLayer2Subnet(verify.owner)
				} else {
					subnets, err = allocator.AllocateLayer3Subnet(verify.owner)
				}
				g.Expect(err).ToNot(gomega.HaveOccurred(),
					"re-allocation for %s should succeed", verify.owner)

				// Build map of allocated by type
				allocatedByType := make(map[string]string)
				for _, subnet := range subnets {
					if subnet.IP.To4() != nil {
						allocatedByType["ipv4"] = subnet.String()
					} else {
						allocatedByType["ipv6"] = subnet.String()
					}
				}

				if verify.ipv4 != "" {
					g.Expect(allocatedByType["ipv4"]).To(gomega.Equal(verify.ipv4),
						"owner %s: IPv4 should match exactly", verify.owner)
				}
				if verify.ipv6 != "" {
					g.Expect(allocatedByType["ipv6"]).To(gomega.Equal(verify.ipv6),
						"owner %s: IPv6 should match exactly", verify.owner)
				}
			}

			// Verify block tracking for layer2
			if len(tt.verifyBlocks) > 0 {
				hca := allocator.(*hybridConnectSubnetAllocator)

				// Verify all expected blocks exist with proper owner names
				for _, expectedKey := range tt.verifyBlocks {
					blockOwner, exists := hca.layer2BlockOwners[expectedKey]
					g.Expect(exists).To(gomega.BeTrue(),
						"block %s should be tracked", expectedKey)
					g.Expect(blockOwner).To(gomega.HavePrefix("l2-block-"),
						"block %s should have a l2-block- prefix owner", expectedKey)
				}
			}

			// Verify new allocation doesn't conflict
			if tt.newAllocation != nil {
				var subnets []*net.IPNet
				var err error
				if tt.newAllocation.topology == types.Layer2Topology {
					subnets, err = allocator.AllocateLayer2Subnet(tt.newAllocation.owner)
				} else {
					subnets, err = allocator.AllocateLayer3Subnet(tt.newAllocation.owner)
				}
				g.Expect(err).ToNot(gomega.HaveOccurred())
				g.Expect(subnets).ToNot(gomega.BeEmpty())

				for _, subnet := range subnets {
					if subnet.IP.To4() != nil && tt.newAllocation.notIPv4 != "" {
						g.Expect(subnet.String()).ToNot(gomega.Equal(tt.newAllocation.notIPv4),
							"new allocation should not get %s", tt.newAllocation.notIPv4)
					}
					if subnet.IP.To4() == nil && tt.newAllocation.notIPv6 != "" {
						g.Expect(subnet.String()).ToNot(gomega.Equal(tt.newAllocation.notIPv6),
							"new allocation should not get %s", tt.newAllocation.notIPv6)
					}
				}
			}
		})
	}
}

// TestHybridConnectSubnetAllocator_AfterMarkAllocatedSubnets_ReleaseWorks tests that after
// marking layer2 subnets, the release path works correctly and releases blocks
// back to layer3 when all owners are released.
func TestHybridConnectSubnetAllocator_AfterMarkAllocatedSubnets_ReleaseWorks(t *testing.T) {
	g := gomega.NewWithT(t)

	config.IPv4Mode = true
	config.IPv6Mode = false

	connectSubnets := []networkconnectv1.ConnectSubnet{
		{
			CIDR:          networkconnectv1.CIDR("192.168.0.0/16"),
			NetworkPrefix: 24,
		},
	}
	// Initialize with range
	allocator, err := NewHybridConnectSubnetAllocator(connectSubnets, "test-cnc")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	hca := allocator.(*hybridConnectSubnetAllocator)

	// Verify layer3 allocator starts with 0 usage
	v4used, _ := hca.layer3Allocator.Usage()
	g.Expect(v4used).To(gomega.Equal(uint64(0)), "layer3 allocator should start with 0 usage")

	// Mark two layer2 subnets from the same block
	allocatedSubnets := map[string][]*net.IPNet{
		"layer2_1": {mustParseCIDR("192.168.0.0/31")},
		"layer2_2": {mustParseCIDR("192.168.0.2/31")},
	}
	err = allocator.MarkAllocatedSubnets(allocatedSubnets)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Verify layer3 allocator now has 1 block used (the block for layer2)
	v4used, _ = hca.layer3Allocator.Usage()
	g.Expect(v4used).To(gomega.Equal(uint64(1)), "layer3 allocator should have 1 block used for layer2 block")

	// Verify initial state
	blockCIDR := "192.168.0.0/24"
	pbOwner := hca.layer2BlockOwners[blockCIDR]
	g.Expect(pbOwner).ToNot(gomega.BeEmpty(), "block owner should be set")
	g.Expect(pbOwner).To(gomega.HavePrefix("l2-block-"), "block owner should have l2-block- prefix")
	// Store the initial block owner (could be layer2_1 or layer2_2 depending on map iteration order)
	initialblockOwner := pbOwner

	// Release first owner - block should still exist, layer3 usage unchanged
	allocator.ReleaseLayer2Subnet("layer2_1")
	g.Expect(hca.layer2BlockOwners[blockCIDR]).ToNot(gomega.BeEmpty(), "block should still exist after first release")
	g.Expect(hca.layer2BlockOwners[blockCIDR]).To(gomega.Equal(initialblockOwner), "block owner should remain the same after first release")

	// Verify layer3 allocator still has 1 block used (block not released yet)
	v4used, _ = hca.layer3Allocator.Usage()
	g.Expect(v4used).To(gomega.Equal(uint64(1)), "layer3 allocator should still have 1 block used after first release")

	// Release second owner - block should be released back to layer3
	allocator.ReleaseLayer2Subnet("layer2_2")
	_, exists := hca.layer2BlockOwners[blockCIDR]
	g.Expect(exists).To(gomega.BeFalse(), "block should be removed after all owners released")

	// Verify layer3 allocator now has 0 blocks used (block released)
	v4used, _ = hca.layer3Allocator.Usage()
	g.Expect(v4used).To(gomega.Equal(uint64(0)), "layer3 allocator should have 0 blocks after all layer2 owners released")

	// Now the block should be available for new layer3 allocation
	// A new layer3 allocation should get 192.168.0.0/24 (the released block)
	subnets, err := allocator.AllocateLayer3Subnet("layer3_new")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(subnets).To(gomega.HaveLen(1))
	g.Expect(subnets[0].String()).To(gomega.Equal("192.168.0.0/24"),
		"released block should be available for layer3 allocation")

	// Verify layer3 allocator now has 1 block used again
	v4used, _ = hca.layer3Allocator.Usage()
	g.Expect(v4used).To(gomega.Equal(uint64(1)), "layer3 allocator should have 1 block after new layer3 allocation")
}
