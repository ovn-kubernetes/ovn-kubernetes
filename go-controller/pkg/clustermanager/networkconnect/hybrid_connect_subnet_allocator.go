package networkconnect

import (
	"errors"
	"fmt"
	"net"
	"sync"

	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/node"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
)

var (
	p2pIPV4SubnetMask = 31
	p2pIPV6SubnetMask = 127
)

// poolBlockDetails represents the details of the layer3 block allocated for the layer2 pool
// this ends up representing a single contiguous range within the layer2 allocator
type poolBlockDetails struct {
	ownerName string // owner name used in layer3 allocator (for release)
	refCount  int    // number of layer2 owners using this block
}

// HybridConnectSubnetAllocator provides hybrid allocation for network connect subnets:
//   - Layer3 networks: Each gets a full layer3NetworkPrefix block (e.g., /24)
//   - Layer2 networks: Pooled allocation - multiple Layer2 networks share layer3Network‍Prefix blocks,
//     with each Layer2 network getting a /31 (IPv4) or /127 (IPv6) from the shared pool
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

	// used to protect the allocator pool block caches that are used from AllocateLayer2Subnet and ReleaseLayer2Subnet
	mu sync.RWMutex

	// Pool block tracking for proper release
	// When a layer2 network is released, we need to also check if the
	// subsequent layer3 pool should be released if that's the last network
	// that was holding that block.
	// Key is the CIDR string of the pool block (e.g., "192.168.0.0/28" or "fd00::/124")
	poolBlocks map[string]*poolBlockDetails // maps pool block CIDR to poolBlockDetails
	// Key is the layer2 network owner name, value is the pool block details that the layer2 network is using.
	layer2OwnerToPoolBlockDetails map[string]*poolBlockDetails // maps layer2 owner to their pool block details
}

// NewHybridConnectSubnetAllocator creates a new hybrid connect subnet allocator
func NewHybridConnectSubnetAllocator() HybridConnectSubnetAllocator {
	return &hybridConnectSubnetAllocator{
		layer3Allocator:               node.NewSubnetAllocator(),
		layer2Allocator:               node.NewSubnetAllocator(),
		poolBlocks:                    make(map[string]*poolBlockDetails),
		layer2OwnerToPoolBlockDetails: make(map[string]*poolBlockDetails),
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

// AllocateLayer2Subnet allocates /31 (IPv4) and/or /127 (IPv6) from shared Layer2 pools.
// This will allocate from all available address families (both IPv4 and IPv6 if dual-stack).
// Caller must call AddNetworkRange before calling this function.
func (hca *hybridConnectSubnetAllocator) AllocateLayer2Subnet(owner string) ([]*net.IPNet, error) {
	hca.mu.Lock()
	defer hca.mu.Unlock()

	var err error
	var subnets []*net.IPNet

	// Try to allocate from current Layer2 pool
	subnets, err = hca.layer2Allocator.AllocateNetworks(owner)
	// Only return if we got subnets - empty slice means no ranges configured yet
	// but before that, let's track the pool block details for the layer2 network
	// so that we can use it during the release.
	if err == nil && len(subnets) > 0 {
		// Allocation succeeded from existing pool - find which block it came from
		pbDetails := hca.findPoolBlockForSubnets(subnets)
		if pbDetails != nil {
			pbDetails.refCount++
			hca.layer2OwnerToPoolBlockDetails[owner] = pbDetails
		}
		return subnets, nil
	}
	if err != nil && !errors.Is(err, node.ErrSubnetAllocatorFull) {
		return nil, fmt.Errorf("Layer2 allocation failed for %s: %v", owner, err)
	}

	var expandedPBDetails *poolBlockDetails
	// Current layer2 allocator is empty (no ranges added yet - lazy initialization) or
	// full (ErrSubnetAllocatorFull) - expand it with new blocks and then allocate
	expandedPBDetails, err = hca.expandLayer2Allocator(owner)
	if err != nil {
		return nil, fmt.Errorf("failed to expand Layer2 allocator: %v", err)
	}

	// Retry allocation after expanding - this will come from the new block
	subnets, err = hca.layer2Allocator.AllocateNetworks(owner)
	if err != nil {
		return nil, fmt.Errorf("Layer2 allocation failed after expansion for %s: %v", owner, err)
	}

	// Track this allocation in the expanded block
	if expandedPBDetails != nil {
		expandedPBDetails.refCount++
		hca.layer2OwnerToPoolBlockDetails[owner] = expandedPBDetails
	}

	return subnets, nil
}

// findPoolBlockForSubnets finds which pool block contains the given subnets
// Uses mathematical derivation based on networkPrefix to compute parent block CIDR
func (hca *hybridConnectSubnetAllocator) findPoolBlockForSubnets(subnets []*net.IPNet) *poolBlockDetails {
	for _, subnet := range subnets {
		parentCIDR := hca.getParentBlockCIDR(subnet)
		if pb, exists := hca.poolBlocks[parentCIDR]; exists {
			return pb
		}
	}
	return nil
}

// getParentBlockCIDR computes the parent pool block CIDR for a given subnet
// by masking the subnet IP to the networkPrefix boundary
func (hca *hybridConnectSubnetAllocator) getParentBlockCIDR(subnet *net.IPNet) string {
	var networkPrefix int
	var bits int

	if utilnet.IsIPv6CIDR(subnet) {
		networkPrefix = hca.v6NetworkPrefix
		bits = 128
	} else {
		networkPrefix = hca.v4NetworkPrefix
		bits = 32
	}

	mask := net.CIDRMask(networkPrefix, bits)
	parentIP := subnet.IP.Mask(mask)
	parentNet := &net.IPNet{IP: parentIP, Mask: mask}
	return parentNet.String()
}

// expandLayer2Allocator expands the existing layer2 allocator by allocating new blocks from layer3 allocator
// It tries to allocate both IPv4 and IPv6 blocks from the Layer3 allocator
// If only one family is available, the pool will be single-stack
// Returns the new pool block for tracking purposes
func (hca *hybridConnectSubnetAllocator) expandLayer2Allocator(firstNetworkName string) (*poolBlockDetails, error) {
	poolOwner := fmt.Sprintf("layer2-pool-%s", firstNetworkName)

	allocatedBlocks, err := hca.layer3Allocator.AllocateNetworks(poolOwner)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate pool blocks: %v", err)
	}

	// Create new pool block to track this allocation
	newPoolBlock := &poolBlockDetails{
		ownerName: poolOwner,
		refCount:  0,
	}

	// Add each allocated block to the existing layer2 allocator (expanding it)
	// and register in poolBlockDetailsByCIDR map for O(1) lookup
	for _, block := range allocatedBlocks {
		if utilnet.IsIPv6CIDR(block) && config.IPv6Mode {
			if err := hca.layer2Allocator.AddNetworkRange(block, p2pIPV6SubnetMask); err != nil {
				return nil, fmt.Errorf("failed to add IPv6 range to layer2 allocator: %v", err)
			}
			hca.poolBlocks[block.String()] = newPoolBlock
		}
		if utilnet.IsIPv4CIDR(block) && config.IPv4Mode {
			// IPv4 block
			if err := hca.layer2Allocator.AddNetworkRange(block, p2pIPV4SubnetMask); err != nil {
				return nil, fmt.Errorf("failed to add IPv4 range to layer2 allocator: %v", err)
			}
			hca.poolBlocks[block.String()] = newPoolBlock
		}
	}

	return newPoolBlock, nil
}

func (hca *hybridConnectSubnetAllocator) ReleaseLayer3Subnet(owner string) {
	hca.layer3Allocator.ReleaseAllNetworks(owner)
}

func (hca *hybridConnectSubnetAllocator) ReleaseLayer2Subnet(owner string) {
	hca.mu.Lock()
	defer hca.mu.Unlock()

	hca.layer2Allocator.ReleaseAllNetworks(owner)

	// Find and update the pool block this owner was using
	pbDetails, exists := hca.layer2OwnerToPoolBlockDetails[owner]
	if !exists {
		// best effort to release layer3 block, if its not in cache we can't do much.
		return
	}
	delete(hca.layer2OwnerToPoolBlockDetails, owner)

	pbDetails.refCount--
	if pbDetails.refCount <= 0 { // should be rare operation where all layer2 networks are gone
		// Pool block is empty - release it back to layer3
		hca.layer3Allocator.ReleaseAllNetworks(pbDetails.ownerName)
		// Remove from poolBlockDetailsByCIDR map
		hca.removePoolBlock(pbDetails)
	}
}

// removePoolBlock removes a pool block from the poolBlocks map.
// In dual-stack, both the IPv4 and IPv6 CIDRs point to the same poolBlockDetails,
// so we need to remove all CIDR keys that reference this pool block.
func (hca *hybridConnectSubnetAllocator) removePoolBlock(pb *poolBlockDetails) {
	for cidr, block := range hca.poolBlocks {
		if block == pb {
			delete(hca.poolBlocks, cidr)
		}
	}
}
