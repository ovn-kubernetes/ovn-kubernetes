package networkconnect

import (
	"errors"
	"fmt"
	"net"
	"sync"

	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/node"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
)

var (
	p2pIPV4SubnetMask = 31
	p2pIPV6SubnetMask = 127
)

// HybridConnectSubnetAllocator provides hybrid allocation for network connect subnets:
//   - Layer3 networks: Each gets a full layer3NetworkPrefix block (e.g., /24)
//   - Layer2 networks: Block allocation - multiple Layer2 networks share layer3Network‍Prefix blocks,
//     with each Layer2 network getting a /31 (IPv4) or /127 (IPv6) from the shared block
//
// This allocator uses the node.SubnetAllocator to allocate subnets underneath using the layer3NetworkPrefix.
//
// We run one instance of this allocator per CNC.
type HybridConnectSubnetAllocator interface {
	// AddNetworkRange initializes the allocator with the overall CIDR range and network prefix.
	// Must be called during initialization before any concurrent Allocate/Release calls.
	AddNetworkRange(network *net.IPNet, networkPrefix int) error

	// AllocateLayer3Subnet allocates network subnets for the layer3 network owner (could be both IPv4 and IPv6 in dual-stack)
	AllocateLayer3Subnet(owner string) ([]*net.IPNet, error)

	// AllocateLayer2Subnet allocates /31 (IPv4) and/or /127 (IPv6) for Layer2 networks from shared layer3 networkPrefix blocks
	AllocateLayer2Subnet(owner string) ([]*net.IPNet, error)

	// ReleaseLayer3Subnet releases all subnets for the layer3 network owner
	ReleaseLayer3Subnet(owner string)

	// ReleaseLayer2Subnet releases all subnets for the layer2 network owner
	ReleaseLayer2Subnet(owner string)

	// Layer2RangeCount returns the number of v4 and v6 ranges in the layer2 allocator (for testing)
	Layer2RangeCount() (uint64, uint64)
}

// hybridConnectSubnetAllocator implements HybridConnectSubnetAllocator
type hybridConnectSubnetAllocator struct {
	// Layer3: Standard subnet allocator (each network gets full networkPrefix block)
	layer3Allocator node.SubnetAllocator

	// Layer2: A chunk allocated from layer3Allocator (one or more networkPrefix blocks are assigned to this allocator)
	// It subdivides the chunk into /31s or /127s for each Layer2 network
	layer2Allocator node.SubnetAllocator

	// networkPrefix per address family - used to mathematically derive parent block from allocated subnets
	// NOTE: This logic assumes we only support atmost 2 CIDR ranges for this allocator - one for IPv4 and one for IPv6 in CNC.
	v4NetworkPrefix int
	v6NetworkPrefix int

	// used to protect layer2BlockOwners cache that is used from AllocateLayer2Subnet and ReleaseLayer2Subnet
	mu sync.RWMutex

	// Layer2 block tracking for proper release
	// When a layer2 network is released, we need to also check if the
	// subsequent layer3 block should be released if that's the last network
	// that was holding that block.
	// Key is the layer2 block subnet CIDR i.e the string form of v4,v6 CIDR block
	// Value is the layer2 block owner name that the layer2 block is using. This is
	// a random string of the pattern "l2-block-<random-string>" to avoid conflicts.
	// maps layer2 block subnet CIDR to the layer2 block owner name
	// Example:
	// "10.100.0.0/28" -> "l2-block-<random-string>" for single-stack IPv4
	// "fd00::/123" -> "l2-block-<random-string>" for single-stack IPv6
	// "10.100.0.0/28,fd00::/123" -> "l2-block-<random-string>" for dual-stack
	layer2BlockOwners map[string]string
}

// NewHybridConnectSubnetAllocator creates a new hybrid connect subnet allocator
func NewHybridConnectSubnetAllocator() HybridConnectSubnetAllocator {
	return &hybridConnectSubnetAllocator{
		layer3Allocator:   node.NewSubnetAllocator(),
		layer2Allocator:   node.NewSubnetAllocator(),
		layer2BlockOwners: make(map[string]string),
	}
}

// AddNetworkRange initializes the allocator with the base CIDR range and network prefix.
// This must be called during initialization before any concurrent Allocate/Release calls.
func (hca *hybridConnectSubnetAllocator) AddNetworkRange(network *net.IPNet, networkPrefix int) error {
	// Validate network prefix
	ones, bits := network.Mask.Size()
	if networkPrefix <= ones {
		return fmt.Errorf("networkPrefix %d must be larger than base CIDR prefix %d", networkPrefix, ones)
	}
	if networkPrefix >= bits {
		return fmt.Errorf("networkPrefix %d must be smaller than address length %d", networkPrefix, bits)
	}

	// Store the networkPrefix per address family
	// It is not thread-safe and should only be called from a single goroutine during setup.
	if utilnet.IsIPv6CIDR(network) {
		hca.v6NetworkPrefix = networkPrefix
	} else {
		hca.v4NetworkPrefix = networkPrefix
	}

	// Add this network range to the Layer3 allocator - each allocation gets a networkPrefix block
	if err := hca.layer3Allocator.AddNetworkRange(network, networkPrefix); err != nil {
		return fmt.Errorf("failed to add network range to Layer3 allocator: %v", err)
	}

	return nil
}

// AllocateLayer3Subnet allocates a full networkPrefix block for Layer3 networks.
// This will try to allocate from available ranges (both IPv4 and IPv6).
// Caller must call AddNetworkRange before calling this function.
func (hca *hybridConnectSubnetAllocator) AllocateLayer3Subnet(owner string) ([]*net.IPNet, error) {
	subnets, err := hca.layer3Allocator.AllocateNetworks(owner)
	if err != nil {
		return nil, fmt.Errorf("Layer3 allocation failed for %s: %v", owner, err)
	}
	return subnets, nil
}

// AllocateLayer2Subnet allocates /31 (IPv4) and/or /127 (IPv6) from shared Layer2 blocks.
// This will allocate from all available address families (both IPv4 and IPv6 if dual-stack).
// Caller must call AddNetworkRange before calling this function.
func (hca *hybridConnectSubnetAllocator) AllocateLayer2Subnet(owner string) ([]*net.IPNet, error) {
	hca.mu.Lock()
	defer hca.mu.Unlock()

	var err error
	var subnets []*net.IPNet

	// Try to allocate from current Layer2 block
	subnets, err = hca.layer2Allocator.AllocateNetworks(owner)
	// Only return if we got subnets - empty slice means no ranges configured yet
	if err == nil && len(subnets) > 0 {
		return subnets, nil
	}
	if err != nil && !errors.Is(err, node.ErrSubnetAllocatorFull) {
		return nil, fmt.Errorf("Layer2 allocation failed for %s: %v", owner, err)
	}

	// Current layer2 allocator is empty (no ranges added yet - lazy initialization) or
	// full (ErrSubnetAllocatorFull) - expand it with new blocks and then allocate
	if err := hca.expandLayer2Allocator(); err != nil {
		return nil, fmt.Errorf("failed to expand Layer2 allocator: %v", err)
	}

	// Retry allocation after expanding - this will come from the new block
	subnets, err = hca.layer2Allocator.AllocateNetworks(owner)
	if err != nil {
		return nil, fmt.Errorf("Layer2 allocation failed after expansion for %s: %v", owner, err)
	}

	return subnets, nil
}

// getL2BlocksKey generates a consistent map key from layer2 block subnets.
// For single-stack: returns the CIDR string (e.g., "192.168.0.0/24")
// For dual-stack: returns "v4,v6" format (e.g., "192.168.0.0/24,fd00::/64")
// The key is used to track layer2 blocks in layer2BlockOwners.
func getL2BlocksKey(subnets []*net.IPNet) string {
	// sort subnets to be v4, v6 to ensure consistent key
	switch len(subnets) {
	case 1:
		return subnets[0].String()
	case 2:
		if subnets[0].IP.To4() != nil {
			return subnets[0].String() + "," + subnets[1].String()
		} else {
			return subnets[1].String() + "," + subnets[0].String()
		}
	default:
		return ""
	}
}

// expandLayer2Allocator expands the existing layer2 allocator by allocating new blocks from layer3 allocator
// It tries to allocate both IPv4 and IPv6 blocks from the Layer3 allocator
// If only one family is available, the block will be single-stack
func (hca *hybridConnectSubnetAllocator) expandLayer2Allocator() error {
	blockOwnerName := fmt.Sprintf("l2-block-%s", rand.String(15)) // Random string to avoid conflicts

	allocatedBlocks, err := hca.layer3Allocator.AllocateNetworks(blockOwnerName)
	if err != nil {
		return fmt.Errorf("failed to allocate layer2 blocks: %v", err)
	}

	// Track the layer2 block owner name
	hca.layer2BlockOwners[getL2BlocksKey(allocatedBlocks)] = blockOwnerName

	// Add each allocated block to the existing layer2 allocator (expanding it)
	for _, block := range allocatedBlocks {
		if utilnet.IsIPv6CIDR(block) && config.IPv6Mode {
			if err := hca.layer2Allocator.AddNetworkRange(block, p2pIPV6SubnetMask); err != nil {
				return fmt.Errorf("failed to add IPv6 range to layer2 allocator: %v", err)
			}
		}
		if utilnet.IsIPv4CIDR(block) && config.IPv4Mode {
			// IPv4 block
			if err := hca.layer2Allocator.AddNetworkRange(block, p2pIPV4SubnetMask); err != nil {
				return fmt.Errorf("failed to add IPv4 range to layer2 allocator: %v", err)
			}
		}
	}

	return nil
}

func (hca *hybridConnectSubnetAllocator) ReleaseLayer3Subnet(owner string) {
	hca.layer3Allocator.ReleaseAllNetworks(owner)
}

func (hca *hybridConnectSubnetAllocator) Layer2RangeCount() (uint64, uint64) {
	return hca.layer2Allocator.RangeCount()
}

func (hca *hybridConnectSubnetAllocator) ReleaseLayer2Subnet(owner string) {
	hca.mu.Lock()
	defer hca.mu.Unlock()

	hca.layer2Allocator.ReleaseAllNetworks(owner)

	// now check if any of the layer2 ranges are free now
	freedRanges := hca.layer2Allocator.FreeUnusedRanges()
	if len(freedRanges) > 0 {
		if len(freedRanges) > 2 {
			// Should never happen, since single owner never spans more than 2 blocks (v4 and v6)
			klog.Errorf("Unexpectedly freed more than 2 ranges (%d) when releasing layer2 subnet for %s", len(freedRanges), owner)
			return
		}
		// Remove free ranges from layer3 allocator
		// find which parent blocks they came from
		l2BlockKey := getL2BlocksKey(freedRanges)
		if blockOwner := hca.layer2BlockOwners[l2BlockKey]; blockOwner != "" {
			hca.layer3Allocator.ReleaseAllNetworks(blockOwner)
			delete(hca.layer2BlockOwners, l2BlockKey)
		}
	}
}
