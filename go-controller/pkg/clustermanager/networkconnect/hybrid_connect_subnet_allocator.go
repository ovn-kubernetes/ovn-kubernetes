package networkconnect

import (
	"errors"
	"fmt"
	"net"

	"k8s.io/apimachinery/pkg/util/rand"
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
	// AddNetworkRange initializes the allocator with the overall CIDR range and network prefix
	AddNetworkRange(network *net.IPNet, networkPrefix int) error

	// AllocateLayer3Subnet allocates network subnets for the layer3 network owner (could be both IPv4 and IPv6 in dual-stack)
	AllocateLayer3Subnet(owner string) ([]*net.IPNet, error)

	// AllocateLayer2Subnet allocates /31 (IPv4) and/or /127 (IPv6) for Layer2 networks from shared layer3 networkPrefix blocks
	AllocateLayer2Subnet(owner string) ([]*net.IPNet, error)

	// ReleaseLayer3Subnet releases all subnets for the layer3 network owner
	ReleaseLayer3Subnet(owner string)

	// ReleaseLayer2Subnet releases all subnets for the layer2 network owner
	ReleaseLayer2Subnet(owner string)
}

// hybridConnectSubnetAllocator implements HybridConnectSubnetAllocator
type hybridConnectSubnetAllocator struct {
	// Layer3: Standard subnet allocator (each network gets full networkPrefix block)
	layer3Allocator node.SubnetAllocator

	// Layer2: A chunk allocated from layer3Allocator (one or more networkPrefix blocks are assigned to this allocator)
	// It subdivides the chunk into /31s or /127s for each Layer2 network
	layer2Allocator node.SubnetAllocator
}

// NewHybridConnectSubnetAllocator creates a new hybrid connect subnet allocator
func NewHybridConnectSubnetAllocator() HybridConnectSubnetAllocator {
	return &hybridConnectSubnetAllocator{
		layer3Allocator: node.NewSubnetAllocator(),
		layer2Allocator: node.NewSubnetAllocator(),
	}
}

// AddNetworkRange initializes the allocator with the base CIDR range and network prefix
func (hca *hybridConnectSubnetAllocator) AddNetworkRange(network *net.IPNet, networkPrefix int) error {
	// Validate network prefix
	ones, bits := network.Mask.Size()
	if networkPrefix <= ones {
		return fmt.Errorf("networkPrefix %d must be larger than base CIDR prefix %d", networkPrefix, ones)
	}
	if networkPrefix >= bits {
		return fmt.Errorf("networkPrefix %d must be smaller than address length %d", networkPrefix, bits)
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
	// Try to allocate from current Layer2 block
	var err error
	var subnets []*net.IPNet

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

	// Retry allocation after expanding
	subnets, err = hca.layer2Allocator.AllocateNetworks(owner)
	if err != nil {
		return nil, fmt.Errorf("Layer2 allocation failed after expansion for %s: %v", owner, err)
	}

	return subnets, nil
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

	// Add each allocated block to the existing layer2 allocator (expanding it)
	for _, block := range allocatedBlocks {
		if utilnet.IsIPv6CIDR(block) && config.IPv6Mode {
			// IPv6 block
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

func (hca *hybridConnectSubnetAllocator) ReleaseLayer2Subnet(owner string) {
	hca.layer2Allocator.ReleaseAllNetworks(owner)

	// TODO(tssurya): We need to also release the blocks from the layer3 allocator if that block is now empty? or do we just keep it for the next layer2 network?
}
