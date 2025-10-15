package networkconnect

import (
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/node"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	utilnet "k8s.io/utils/net"
)

var (
	p2pIPV4SubnetMask = 31
	p2pIPV6SubnetMask = 127
)

// HybridConnectSubnetAllocator provides hybrid allocation for network connect subnets:
//   - Layer3 networks: Each gets a full layer3NetworkPrefix block (e.g., /24)
//   - Layer2 networks: Pooled allocation - multiple Layer2 networks share layer3NetworkPrefix blocks,
//     with each Layer2 network getting a /31 (IPv4) or /127 (IPv6) from the shared pool
//
// This allocator uses the node.SubnetAllocator to allocate subnets underneath using the layer3NetworkPrefix.
//
// We run one instance of this allocator per CNC.
type HybridConnectSubnetAllocator interface {
	// AddNetworkRange initializes the allocator with the overall CIDR range and network prefix
	AddNetworkRange(network *net.IPNet, networkPrefix int) error

	// AllocateLayer3Subnet allocates network subnets for the layer3 network owner (could be both IPv4 and IPv6 in dual-stack)
	AllocateLayer3Subnet(owner string) ([]*net.IPNet, error)

	// AllocateLayer2Subnet allocates /31 (IPv4) and/or /127 (IPv6) for Layer2 networks from shared pools
	// Returns subnets for all available address families in the cluster
	AllocateLayer2Subnet(owner string) ([]*net.IPNet, error)

	// MarkAllocatedSubnets marks multiple subnets as already allocated (for existing allocations)
	MarkAllocatedSubnets(owner string, subnets ...*net.IPNet) error
}

// layer2SubnetPool manages dual-stack networkPrefix blocks subdivided into Layer2 subnets
// Each pool can contain both IPv4 and IPv6 allocations
type layer2SubnetPool struct {
	v4PoolCIDR *net.IPNet              // IPv4 networkPrefix block (nil if not available)
	v6PoolCIDR *net.IPNet              // IPv6 networkPrefix block (nil if not available)
	allocator  node.SubnetAllocator    // Dual-stack allocator: subdivides v4 pool into /31s and v6 pool into /127s
	allocated  map[string][]*net.IPNet // Track what's allocated from this pool (can be v4, v6, or both)
}

// hybridConnectSubnetAllocator implements HybridConnectSubnetAllocator
type hybridConnectSubnetAllocator struct {
	sync.RWMutex

	// Layer3: Standard subnet allocator (each network gets full networkPrefix block)
	layer3Allocator node.SubnetAllocator

	// Layer2: Pools of networkPrefix blocks, each subdivided into /31s or /127s
	layer2Pools []*layer2SubnetPool
	// Current pool for both families (for efficiency, avoid scanning all pools)
	currentPool *layer2SubnetPool

	// Track all allocations for management - can store multiple networks per owner
	allAllocations map[string][]*net.IPNet
}

// NewHybridConnectSubnetAllocator creates a new hybrid connect subnet allocator
func NewHybridConnectSubnetAllocator() HybridConnectSubnetAllocator {
	return &hybridConnectSubnetAllocator{
		layer2Pools:     make([]*layer2SubnetPool, 0),
		allAllocations:  make(map[string][]*net.IPNet),
		layer3Allocator: node.NewSubnetAllocator(),
	}
}

// AddNetworkRange initializes the allocator with the base CIDR range and network prefix
func (hca *hybridConnectSubnetAllocator) AddNetworkRange(network *net.IPNet, networkPrefix int) error {
	hca.Lock()
	defer hca.Unlock()

	// Validate network prefix
	ones, bits := network.Mask.Size()
	if networkPrefix <= ones {
		return fmt.Errorf("networkPrefix %d must be larger than base CIDR prefix %d", networkPrefix, ones)
	}
	if networkPrefix >= bits {
		return fmt.Errorf("networkPrefix %d must be smaller than address length %d", networkPrefix, bits)
	}

	// Initialize Layer3 allocator if not already done
	if hca.layer3Allocator == nil {
		hca.layer3Allocator = node.NewSubnetAllocator()
	}

	// Add this network range to the Layer3 allocator - each allocation gets a networkPrefix block
	if err := hca.layer3Allocator.AddNetworkRange(network, networkPrefix); err != nil {
		return fmt.Errorf("failed to add network range to Layer3 allocator: %v", err)
	}

	return nil
}

// AllocateLayer3Subnet allocates a full networkPrefix block for Layer3 networks
// This will try to allocate from available ranges (both IPv4 and IPv6)
func (hca *hybridConnectSubnetAllocator) AllocateLayer3Subnet(owner string) ([]*net.IPNet, error) {
	hca.Lock()
	defer hca.Unlock()

	if hca.layer3Allocator == nil {
		return nil, fmt.Errorf("allocator not initialized")
	}

	// Try to allocate networks like the regular SubnetAllocator does
	subnets, err := hca.layer3Allocator.AllocateNetworks(owner)
	if err != nil {
		return nil, fmt.Errorf("Layer3 allocation failed for %s: %v", owner, err)
	}

	if len(subnets) == 0 {
		return nil, fmt.Errorf("no networks allocated for %s", owner)
	}

	// Store all allocated networks
	hca.allAllocations[owner] = subnets

	// Return all allocated networks (could be both IPv4 and IPv6)
	return subnets, nil
}

// AllocateLayer2Subnet allocates /31 (IPv4) and/or /127 (IPv6) from shared Layer2 pools
// This will allocate from all available address families (both IPv4 and IPv6 if dual-stack)
func (hca *hybridConnectSubnetAllocator) AllocateLayer2Subnet(owner string) ([]*net.IPNet, error) {
	hca.Lock()
	defer hca.Unlock()

	if hca.layer3Allocator == nil {
		return nil, fmt.Errorf("allocator not initialized")
	}

	// Try to allocate from current Layer2 pool
	var err error
	var subnets []*net.IPNet
	if hca.currentPool != nil {
		subnets, err = hca.currentPool.allocator.AllocateNetworks(owner)
		if err == nil {
			// Track allocations
			hca.currentPool.allocated[owner] = subnets
			hca.allAllocations[owner] = subnets
			return subnets, nil
		}
		if err != nil && !errors.Is(err, node.ErrSubnetAllocatorFull) {
			return nil, fmt.Errorf("failed to allocate from Layer2 pool: %v", err)
		}
	}

	// Current pool doesn't exist or is full - create new pool
	newPool, err := hca.createNewLayer2Pool(owner)
	if err != nil {
		return nil, fmt.Errorf("failed to create new Layer2 pool: %v", err)
	}

	hca.layer2Pools = append(hca.layer2Pools, newPool)
	hca.currentPool = newPool

	// Allocate first subnets from new pool using dual-stack aware allocation
	subnets, err = newPool.allocator.AllocateNetworks(owner)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate from new Layer2 pool: %v", err)
	}

	// Track allocations
	newPool.allocated[owner] = subnets
	hca.allAllocations[owner] = subnets

	return subnets, nil
}

// createNewLayer2Pool creates a new dual-stack Layer2 pool
// It tries to allocate both IPv4 and IPv6 blocks from the Layer3 allocator
// If only one family is available, the pool will be single-stack
func (hca *hybridConnectSubnetAllocator) createNewLayer2Pool(firstNetworkName string) (*layer2SubnetPool, error) {
	poolOwner := fmt.Sprintf("layer2-pool-%s", firstNetworkName)

	pool := &layer2SubnetPool{
		allocated: make(map[string][]*net.IPNet),
		allocator: node.NewSubnetAllocator(),
	}

	// Allocate pool blocks using dual-stack aware allocation
	poolBlocks, err := hca.layer3Allocator.AllocateNetworks(poolOwner)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate pool blocks: %v", err)
	}

	if len(poolBlocks) == 0 {
		return nil, fmt.Errorf("no pool blocks allocated for %s", poolOwner)
	}

	// Add each allocated block to the pool allocator
	for _, block := range poolBlocks {
		if utilnet.IsIPv6CIDR(block) && config.IPv6Mode {
			// IPv6 block
			if err := pool.allocator.AddNetworkRange(block, p2pIPV6SubnetMask); err != nil {
				return nil, fmt.Errorf("failed to add IPv6 range to pool allocator: %v", err)
			}
			pool.v6PoolCIDR = block
		}
		if utilnet.IsIPv4CIDR(block) && config.IPv4Mode {
			// IPv4 block
			if err := pool.allocator.AddNetworkRange(block, p2pIPV4SubnetMask); err != nil {
				return nil, fmt.Errorf("failed to add IPv4 range to pool allocator: %v", err)
			}
			pool.v4PoolCIDR = block
		}
	}

	return pool, nil
}

// MarkAllocatedSubnets marks multiple subnets as already allocated (for existing allocations)
func (hca *hybridConnectSubnetAllocator) MarkAllocatedSubnets(owner string, subnets ...*net.IPNet) error {
	hca.Lock()
	defer hca.Unlock()

	if hca.layer3Allocator == nil {
		return fmt.Errorf("allocator not initialized")
	}

	// Try efficient batch marking in Layer3 allocator first
	if err := hca.layer3Allocator.MarkAllocatedNetworks(owner, subnets...); err == nil {
		// All subnets were successfully marked in Layer3 - store all subnets
		if len(subnets) > 0 {
			hca.allAllocations[owner] = append(hca.allAllocations[owner], subnets...)
		}
		return nil
	}

	// If batch marking failed, try marking each subnet individually
	// This handles cases where some subnets are Layer3 and others are Layer2
	for _, subnet := range subnets {
		if err := hca.markSingleAllocatedSubnet(owner, subnet); err != nil {
			return fmt.Errorf("failed to mark subnet %s: %v", subnet.String(), err)
		}
	}

	return nil
}

// addAllocation adds a subnet to an owner's allocation list
func (hca *hybridConnectSubnetAllocator) addAllocation(owner string, subnet *net.IPNet) {
	hca.allAllocations[owner] = append(hca.allAllocations[owner], subnet)
}

// markSingleAllocatedSubnet is the implementation for marking a single subnet
// This is used by both MarkAllocatedSubnet and MarkAllocatedSubnets
func (hca *hybridConnectSubnetAllocator) markSingleAllocatedSubnet(owner string, subnet *net.IPNet) error {
	// Try to mark in Layer3 allocator first
	if err := hca.layer3Allocator.MarkAllocatedNetworks(owner, subnet); err == nil {
		hca.addAllocation(owner, subnet)
		return nil
	}

	// If that fails, it might be a Layer2 subnet in one of our pools
	// Determine if this is IPv4 or IPv6
	isIPv6 := subnet.IP.To4() == nil

	// Check all pools for this subnet
	for _, pool := range hca.layer2Pools {
		var poolCIDR *net.IPNet

		// Select the appropriate pool CIDR based on address family
		if isIPv6 {
			poolCIDR = pool.v6PoolCIDR
		} else {
			poolCIDR = pool.v4PoolCIDR
		}

		// Check if this pool contains this subnet
		if poolCIDR != nil && poolCIDR.Contains(subnet.IP) {
			// This pool should contain this subnet - use the dual-stack allocator
			if err := pool.allocator.MarkAllocatedNetworks(owner, subnet); err == nil {
				pool.allocated[owner] = append(pool.allocated[owner], subnet)
				hca.addAllocation(owner, subnet)
				return nil
			}
		}
	}

	return fmt.Errorf("subnet %s does not belong to any known range", subnet.String())
}
