package networkconnect

import (
	"fmt"
	"net"
	"testing"

	"github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
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

			allocator := NewHybridConnectSubnetAllocator()
			network := mustParseCIDR(tt.network)
			err := allocator.AddNetworkRange(network, tt.networkPrefix)

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
				// IPv6 allocator skips subnet 0 when subnetBits >= 16 to avoid address compression issues
				"layer3_1": {"fd00:0:0:1::/64"},
			},
		},
		{
			name:     "multiple IPv6 allocations",
			ipv4Mode: false,
			ipv6Mode: true,
			owners:   []string{"layer3_1", "layer3_2", "layer3_3"},
			expectSubnets: map[string][]string{
				// IPv6 allocator skips subnet 0 when subnetBits >= 16 to avoid address compression issues
				"layer3_1": {"fd00:0:0:1::/64"},
				"layer3_2": {"fd00:0:0:2::/64"},
				"layer3_3": {"fd00:0:0:3::/64"},
			},
		},
		{
			name:     "dual-stack allocation",
			ipv4Mode: true,
			ipv6Mode: true,
			owners:   []string{"layer3_1"},
			expectSubnets: map[string][]string{
				// IPv6 allocator skips subnet 0 when subnetBits >= 16
				"layer3_1": {"192.168.0.0/24", "fd00:0:0:1::/64"},
			},
		},
		{
			name:     "multiple dual-stack allocations",
			ipv4Mode: true,
			ipv6Mode: true,
			owners:   []string{"layer3_1", "layer3_2", "layer3_3"},
			expectSubnets: map[string][]string{
				"layer3_1": {"192.168.0.0/24", "fd00:0:0:1::/64"},
				"layer3_2": {"192.168.1.0/24", "fd00:0:0:2::/64"},
				"layer3_3": {"192.168.2.0/24", "fd00:0:0:3::/64"},
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

			allocator := NewHybridConnectSubnetAllocator()

			// Add network ranges based on IP mode
			if tt.ipv4Mode {
				err := allocator.AddNetworkRange(mustParseCIDR("192.168.0.0/16"), 24)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}
			if tt.ipv6Mode {
				err := allocator.AddNetworkRange(mustParseCIDR("fd00::/48"), 64)
				g.Expect(err).ToNot(gomega.HaveOccurred())
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
				// Layer2 block gets /64 block from layer3 (fd00:0:0:1::/64), then allocates /127 from it
				// /127 has subnetBits = 127 - 64 = 63, which is >= 16, so it skips subnets with 0s in low word
				// First non-zero low word is ::2 (since ::1 has low word = 1 which is non-zero but ::0 is skipped)
				"layer2_1": {"fd00:0:0:1::2/127"},
			},
		},
		{
			name:     "multiple IPv6 layer2 allocations get /127 each",
			ipv4Mode: false,
			ipv6Mode: true,
			owners:   []string{"layer2_1", "layer2_2", "layer2_3"},
			expectSubnets: map[string][]string{
				"layer2_1": {"fd00:0:0:1::2/127"},
				"layer2_2": {"fd00:0:0:1::4/127"},
				"layer2_3": {"fd00:0:0:1::6/127"},
			},
		},
		{
			name:     "dual-stack layer2 allocation",
			ipv4Mode: true,
			ipv6Mode: true,
			owners:   []string{"layer2_1"},
			expectSubnets: map[string][]string{
				// Layer2 block gets /24 block from layer3 (192.168.0.0/24), then allocates /31 from it
				// IPv6 block gets /64 block from layer3 (fd00:0:0:1::/64), then allocates /127 from it
				"layer2_1": {"192.168.0.0/31", "fd00:0:0:1::2/127"},
			},
		},
		{
			name:     "multiple dual-stack layer2 allocations",
			ipv4Mode: true,
			ipv6Mode: true,
			owners:   []string{"layer2_1", "layer2_2", "layer2_3"},
			expectSubnets: map[string][]string{
				"layer2_1": {"192.168.0.0/31", "fd00:0:0:1::2/127"},
				"layer2_2": {"192.168.0.2/31", "fd00:0:0:1::4/127"},
				"layer2_3": {"192.168.0.4/31", "fd00:0:0:1::6/127"},
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

			allocator := NewHybridConnectSubnetAllocator()

			// Add network ranges based on IP mode
			if tt.ipv4Mode {
				err := allocator.AddNetworkRange(mustParseCIDR("192.168.0.0/16"), 24)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}
			if tt.ipv6Mode {
				err := allocator.AddNetworkRange(mustParseCIDR("fd00::/48"), 64)
				g.Expect(err).ToNot(gomega.HaveOccurred())
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

	allocator := NewHybridConnectSubnetAllocator()
	err := allocator.AddNetworkRange(mustParseCIDR("192.168.0.0/16"), 24)
	g.Expect(err).ToNot(gomega.HaveOccurred())

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
			ipv6Network:       "fd00::/32",
			ipv6NetworkPrefix: 64,
			allocateFirst:     []string{"layer3_1", "layer3_2"},
			release:           []string{"layer3_1"},
			allocateAgain:     []string{"layer3_3"},
			expectSubnets: map[string][]string{
				// Allocator continues from next position (IPv6 skips subnet 0)
				"layer3_3": {"fd00:0:0:3::/64"},
			},
		},
		{
			name:              "IPv6 released subnet is reused when allocator wraps around",
			ipv4Mode:          false,
			ipv6Mode:          true,
			ipv6Network:       "fd00::/60", // Small range: only 4 /62 subnets
			ipv6NetworkPrefix: 62,
			allocateFirst:     []string{"layer3_1", "layer3_2", "layer3_3", "layer3_4"}, // Allocate all 4
			release:           []string{"layer3_2"},                                     // Release the second one
			allocateAgain:     []string{"layer3_5"},
			expectSubnets: map[string][]string{
				// Allocator wraps around and reuses the released subnet
				"layer3_5": {"fd00:0:0:4::/62"},
			},
		},
		{
			name:              "dual-stack allocation continues from where it left off after release",
			ipv4Mode:          true,
			ipv6Mode:          true,
			ipv4Network:       "192.168.0.0/16",
			ipv4NetworkPrefix: 24,
			ipv6Network:       "fd00::/32",
			ipv6NetworkPrefix: 64,
			allocateFirst:     []string{"layer3_1", "layer3_2"},
			release:           []string{"layer3_1"},
			allocateAgain:     []string{"layer3_3"},
			expectSubnets: map[string][]string{
				// Both IPv4 and IPv6 continue from next position
				"layer3_3": {"192.168.2.0/24", "fd00:0:0:3::/64"},
			},
		},
		{
			name:              "dual-stack released subnet is reused when allocator wraps around",
			ipv4Mode:          true,
			ipv6Mode:          true,
			ipv4Network:       "192.168.0.0/24", // Small range: only 4 /26 subnets
			ipv4NetworkPrefix: 26,
			ipv6Network:       "fd00::/60", // Small range: only 4 /62 subnets
			ipv6NetworkPrefix: 62,
			allocateFirst:     []string{"layer3_1", "layer3_2", "layer3_3", "layer3_4"}, // Allocate all 4
			release:           []string{"layer3_2"},                                     // Release the second one
			allocateAgain:     []string{"layer3_5"},
			expectSubnets: map[string][]string{
				// Both IPv4 and IPv6 wrap around and reuse the released subnet
				"layer3_5": {"192.168.0.64/26", "fd00:0:0:4::/62"},
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

			allocator := NewHybridConnectSubnetAllocator()
			if tt.ipv4Mode && tt.ipv4Network != "" {
				err := allocator.AddNetworkRange(mustParseCIDR(tt.ipv4Network), tt.ipv4NetworkPrefix)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}
			if tt.ipv6Mode && tt.ipv6Network != "" {
				err := allocator.AddNetworkRange(mustParseCIDR(tt.ipv6Network), tt.ipv6NetworkPrefix)
				g.Expect(err).ToNot(gomega.HaveOccurred())
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
			ipv6Network:       "fd00::/48",
			ipv6NetworkPrefix: 64,
			allocateFirst:     []string{"layer2_1", "layer2_2"},
			release:           []string{"layer2_1"},
			allocateAgain:     []string{"layer2_3"},
			expectSubnets: map[string][]string{
				// Both IPv4 and IPv6 continue from next position
				"layer2_3": {"192.168.0.4/31", "fd00:0:0:1::6/127"},
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

			allocator := NewHybridConnectSubnetAllocator()
			if tt.ipv4Mode && tt.ipv4Network != "" {
				err := allocator.AddNetworkRange(mustParseCIDR(tt.ipv4Network), tt.ipv4NetworkPrefix)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}
			if tt.ipv6Mode && tt.ipv6Network != "" {
				err := allocator.AddNetworkRange(mustParseCIDR(tt.ipv6Network), tt.ipv6NetworkPrefix)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

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

	allocator := NewHybridConnectSubnetAllocator()
	err := allocator.AddNetworkRange(mustParseCIDR("192.168.0.0/16"), 24)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	err = allocator.AddNetworkRange(mustParseCIDR("fd00::/32"), 64)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Allocate layer3 subnets (dual-stack: both IPv4 and IPv6)
	l3Sub1, err := allocator.AllocateLayer3Subnet("layer3_1")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l3Sub1).To(gomega.HaveLen(2))
	g.Expect(l3Sub1[0].String()).To(gomega.Equal("192.168.0.0/24"))
	g.Expect(l3Sub1[1].String()).To(gomega.Equal("fd00:0:0:1::/64"))

	l3Sub2, err := allocator.AllocateLayer3Subnet("layer3_2")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l3Sub2).To(gomega.HaveLen(2))
	g.Expect(l3Sub2[0].String()).To(gomega.Equal("192.168.1.0/24"))
	g.Expect(l3Sub2[1].String()).To(gomega.Equal("fd00:0:0:2::/64"))

	// Allocate layer2 subnets (will get new blocks from layer3: 192.168.2.0/24 and fd00:0:0:3::/64)
	l2Sub1, err := allocator.AllocateLayer2Subnet("layer2_1")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l2Sub1).To(gomega.HaveLen(2))
	g.Expect(l2Sub1[0].String()).To(gomega.Equal("192.168.2.0/31"))
	g.Expect(l2Sub1[1].String()).To(gomega.Equal("fd00:0:0:3::2/127"))

	l2Sub2, err := allocator.AllocateLayer2Subnet("layer2_2")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l2Sub2).To(gomega.HaveLen(2))
	g.Expect(l2Sub2[0].String()).To(gomega.Equal("192.168.2.2/31"))
	g.Expect(l2Sub2[1].String()).To(gomega.Equal("fd00:0:0:3::4/127"))

	// Release one layer3 and one layer2
	allocator.ReleaseLayer3Subnet("layer3_1")
	allocator.ReleaseLayer2Subnet("layer2_1")

	// Allocate new layer3 - allocator continues from where it left off
	// IPv4: 192.168.2.0/24 is taken by layer2-block, so next available is 192.168.3.0/24
	// IPv6: fd00:0:0:3::/64 is taken by layer2-block, so next available is fd00:0:0:4::/64
	l3Sub3, err := allocator.AllocateLayer3Subnet("layer3_3")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l3Sub3).To(gomega.HaveLen(2))
	g.Expect(l3Sub3[0].String()).To(gomega.Equal("192.168.3.0/24"))
	g.Expect(l3Sub3[1].String()).To(gomega.Equal("fd00:0:0:4::/64"))

	// Allocate new layer2 - allocator continues from where it left off
	l2Sub3, err := allocator.AllocateLayer2Subnet("layer2_3")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l2Sub3).To(gomega.HaveLen(2))
	g.Expect(l2Sub3[0].String()).To(gomega.Equal("192.168.2.4/31"))
	g.Expect(l2Sub3[1].String()).To(gomega.Equal("fd00:0:0:3::6/127"))

	// Verify layer3 and layer2 allocators are independent
	// Allocate more layer2 - should continue in layer2 block
	l2Sub4, err := allocator.AllocateLayer2Subnet("layer2_4")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l2Sub4).To(gomega.HaveLen(2))
	g.Expect(l2Sub4[0].String()).To(gomega.Equal("192.168.2.6/31"))
	g.Expect(l2Sub4[1].String()).To(gomega.Equal("fd00:0:0:3::8/127"))

	// Allocate layer3 - should continue in layer3 space
	l3Sub4, err := allocator.AllocateLayer3Subnet("layer3_4")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l3Sub4).To(gomega.HaveLen(2))
	g.Expect(l3Sub4[0].String()).To(gomega.Equal("192.168.4.0/24"))
	g.Expect(l3Sub4[1].String()).To(gomega.Equal("fd00:0:0:5::/64"))

	// Verify released owner gets new subnet (not the old one)
	l3Sub1Again, err := allocator.AllocateLayer3Subnet("layer3_1")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l3Sub1Again).To(gomega.HaveLen(2))
	// layer3_1 was released, so new allocation continues from next position
	g.Expect(l3Sub1Again[0].String()).To(gomega.Equal("192.168.5.0/24"))
	g.Expect(l3Sub1Again[1].String()).To(gomega.Equal("fd00:0:0:6::/64"))
}

func TestHybridConnectSubnetAllocator_Layer2BlockExpansionFromLayer3(t *testing.T) {
	g := gomega.NewWithT(t)

	config.IPv4Mode = true
	config.IPv6Mode = true

	allocator := NewHybridConnectSubnetAllocator()
	// Use small CIDRs to test block exhaustion and expansion
	// IPv4: 192.168.0.0/24 with /28 prefix gives us 16 /28 blocks, each holding 8 /31 subnets
	// IPv6: fd00::/120 with /124 prefix gives us 16 /124 blocks, each holding 8 /127 subnets
	err := allocator.AddNetworkRange(mustParseCIDR("192.168.0.0/24"), 28)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	err = allocator.AddNetworkRange(mustParseCIDR("fd00::/120"), 124)
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

	allocator := NewHybridConnectSubnetAllocator()
	// Use a very small CIDR that can only hold 4 /26 subnets
	// 192.168.0.0/24 with /26 prefix = 4 subnets (256 IPs / 64 IPs per /26 = 4)
	err := allocator.AddNetworkRange(mustParseCIDR("192.168.0.0/24"), 26)
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

	allocator := NewHybridConnectSubnetAllocator()
	// Use very small CIDRs that can only hold 2 subnets each
	// IPv4: 192.168.0.0/24 with /25 prefix = 2 subnets
	// IPv6: fd00::/121 with /122 prefix = 2 subnets
	err := allocator.AddNetworkRange(mustParseCIDR("192.168.0.0/24"), 25)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	err = allocator.AddNetworkRange(mustParseCIDR("fd00::/121"), 122)
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

	allocator := NewHybridConnectSubnetAllocator()
	// Small range: 192.168.0.0/26 with /28 prefix = 4 /28 blocks
	// Each /28 block has 8 /31 slots
	err := allocator.AddNetworkRange(mustParseCIDR("192.168.0.0/26"), 28)
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

	allocator := NewHybridConnectSubnetAllocator()
	// Small ranges: 4 blocks each
	// IPv4: 10.100.0.0/26 with /28 prefix = 4 /28 blocks (each has 8 /31 slots)
	// IPv6: fd00::/121 with /123 prefix = 4 /123 blocks (each has 4 /127 slots)
	err := allocator.AddNetworkRange(mustParseCIDR("10.100.0.0/26"), 28)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	err = allocator.AddNetworkRange(mustParseCIDR("fd00::/121"), 123)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Allocate 2 layer2 networks - both in the same block
	l2Sub1, err := allocator.AllocateLayer2Subnet("layer2_1")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(l2Sub1).To(gomega.HaveLen(2)) // dual-stack
	g.Expect(l2Sub1[0].String()).To(gomega.Equal("10.100.0.0/31"))
	// /123 to /127: subnetBits = 4, which is < 16, so it doesn't skip subnet 0
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
	g.Expect(l3Sub4[1].String()).To(gomega.Equal("fd00::/123"))
}
