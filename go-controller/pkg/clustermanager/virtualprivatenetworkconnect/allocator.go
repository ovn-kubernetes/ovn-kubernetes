package virtualprivatenetworkconnect

import (
	"fmt"
	"net"
	"sync"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

// VPNCSubnetAllocator manages /31 point-to-point subnet allocation for VirtualPrivateNetworkConnect
type VPNCSubnetAllocator interface {
	// AddConnectSubnet adds a connect subnet range for /31 allocation
	AddConnectSubnet(subnet *net.IPNet) error
	// AllocateSubnet allocates a /31 subnet for a node-network pair
	AllocateSubnet(owner string) (*net.IPNet, error)
	// ReleaseSubnet releases a /31 subnet for a node-network pair
	ReleaseSubnet(owner string, subnet *net.IPNet) error
	// ReleaseAllSubnetsForOwner releases all subnets for a given owner
	ReleaseAllSubnetsForOwner(owner string)
}

// VPNCPointToPointAllocator implements VPNCSubnetAllocator for /31 point-to-point allocations
type VPNCPointToPointAllocator struct {
	sync.Mutex
	// subnets is the list of available connect subnets from the VPNC spec
	subnets []*net.IPNet
	// allocations tracks owner -> allocated subnet mappings
	allocations map[string]*net.IPNet
	// nextOffset tracks the next available /31 offset within the subnets
	nextOffset uint32
}

// NewVPNCPointToPointAllocator creates a new /31 subnet allocator
func NewVPNCPointToPointAllocator() VPNCSubnetAllocator {
	return &VPNCPointToPointAllocator{
		allocations: make(map[string]*net.IPNet),
		nextOffset:  0,
	}
}

// AddConnectSubnet adds a connect subnet range for /31 allocation
func (vpnca *VPNCPointToPointAllocator) AddConnectSubnet(subnet *net.IPNet) error {
	vpnca.Lock()
	defer vpnca.Unlock()

	vpnca.subnets = append(vpnca.subnets, subnet)
	klog.V(5).Infof("Added connect subnet %s to VPNC allocator", subnet)
	return nil
}

// AllocateSubnet allocates a /31 subnet for a node-network pair
func (vpnca *VPNCPointToPointAllocator) AllocateSubnet(owner string) (*net.IPNet, error) {
	vpnca.Lock()
	defer vpnca.Unlock()

	// Check if already allocated
	if existing, ok := vpnca.allocations[owner]; ok {
		return existing, nil
	}

	// Find next available /31 subnet
	for _, subnet := range vpnca.subnets {
		allocated, err := vpnca.allocateFromSubnet(owner, subnet)
		if err == nil {
			vpnca.allocations[owner] = allocated
			klog.V(4).Infof("Allocated /31 subnet %s for %s", allocated, owner)
			return allocated, nil
		}
	}

	return nil, fmt.Errorf("no available /31 subnets for owner %s", owner)
}

// allocateFromSubnet allocates the next available /31 from the given subnet
func (vpnca *VPNCPointToPointAllocator) allocateFromSubnet(owner string, subnet *net.IPNet) (*net.IPNet, error) {
	ones, bits := subnet.Mask.Size()
	if ones >= bits-1 {
		return nil, fmt.Errorf("subnet %s is too small for /31 allocation", subnet)
	}

	// Calculate how many /31s can fit in this subnet
	subnetSize := uint32(1) << (bits - ones) // Total IPs in the subnet
	maxPoint2Point := subnetSize / 2         // Each /31 uses 2 IPs

	// Try to find an unused /31 within this subnet
	subnetIP := subnet.IP.Mask(subnet.Mask)
	for i := uint32(0); i < maxPoint2Point; i++ {
		// Calculate the /31 network address
		offset := i * 2
		p2pIP := make(net.IP, len(subnetIP))
		copy(p2pIP, subnetIP)

		// Add the offset to the IP
		if utilnet.IsIPv4(subnetIP) {
			ip4 := p2pIP.To4()
			val := uint32(ip4[0])<<24 + uint32(ip4[1])<<16 + uint32(ip4[2])<<8 + uint32(ip4[3])
			val += offset
			ip4[0] = byte((val >> 24) & 0xFF)
			ip4[1] = byte((val >> 16) & 0xFF)
			ip4[2] = byte((val >> 8) & 0xFF)
			ip4[3] = byte(val & 0xFF)
		} else {
			// For IPv6, add the offset to the last 4 bytes
			ip6 := p2pIP.To16()
			val := uint32(ip6[12])<<24 + uint32(ip6[13])<<16 + uint32(ip6[14])<<8 + uint32(ip6[15])
			val += offset
			ip6[12] = byte((val >> 24) & 0xFF)
			ip6[13] = byte((val >> 16) & 0xFF)
			ip6[14] = byte((val >> 8) & 0xFF)
			ip6[15] = byte(val & 0xFF)
		}

		// Create the /31 subnet
		var mask net.IPMask
		if utilnet.IsIPv4(subnetIP) {
			mask = net.CIDRMask(31, 32)
		} else {
			mask = net.CIDRMask(127, 128) // IPv6 equivalent of /31
		}

		p2pSubnet := &net.IPNet{IP: p2pIP, Mask: mask}

		// Check if this /31 is already allocated
		isAllocated := false
		for _, allocated := range vpnca.allocations {
			if allocated.String() == p2pSubnet.String() {
				isAllocated = true
				break
			}
		}

		if !isAllocated {
			return p2pSubnet, nil
		}
	}

	return nil, fmt.Errorf("no available /31 subnets in range %s", subnet)
}

// ReleaseSubnet releases a /31 subnet for a node-network pair
func (vpnca *VPNCPointToPointAllocator) ReleaseSubnet(owner string, subnet *net.IPNet) error {
	vpnca.Lock()
	defer vpnca.Unlock()

	if allocated, ok := vpnca.allocations[owner]; ok {
		if allocated.String() == subnet.String() {
			delete(vpnca.allocations, owner)
			klog.V(4).Infof("Released /31 subnet %s for %s", subnet, owner)
			return nil
		}
		return fmt.Errorf("owner %s has subnet %s, not %s", owner, allocated, subnet)
	}

	return fmt.Errorf("owner %s has no allocated subnet", owner)
}

// ReleaseAllSubnetsForOwner releases all subnets for a given owner
func (vpnca *VPNCPointToPointAllocator) ReleaseAllSubnetsForOwner(owner string) {
	vpnca.Lock()
	defer vpnca.Unlock()

	if allocated, ok := vpnca.allocations[owner]; ok {
		delete(vpnca.allocations, owner)
		klog.V(4).Infof("Released all subnets for %s (was %s)", owner, allocated)
	}
}
