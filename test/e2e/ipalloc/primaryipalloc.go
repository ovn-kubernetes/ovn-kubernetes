// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ipalloc

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// Reserved range for E2E test IPs to avoid conflicts with node IPs
const (
	testIPv4Start = 200 // Start from .200 to avoid typical node IPs (.1-.199)
	testIPv4End   = 254 // Last usable before .255 broadcast
	testIPv6Start = 200 // Same for IPv6 (0xC8)
	testIPv6End   = 255 // Last byte of reserved range (0xFF)
)

// allocState holds per-subnet allocator state
type allocState struct {
	alloc *ipAllocator
	maxIP net.IP // upper bound of reserved range (e.g. .254)
}

// primaryIPAllocator attempts to allocate an IP in the same subnet as a node's primary network.
// It maintains per-subnet allocators to support clusters where nodes span multiple subnets.
type primaryIPAllocator struct {
	mu         *sync.Mutex
	v4Allocs   map[string]*allocState // keyed by subnet prefix (last byte zeroed)
	v6Allocs   map[string]*allocState
	nodeClient v1.NodeInterface
}

var pia *primaryIPAllocator

// InitPrimaryIPAllocator must be called to init IP allocator(s). Callers must be synchronise.
func InitPrimaryIPAllocator(nodeClient v1.NodeInterface) error {
	var err error
	pia, err = newPrimaryIPAllocator(nodeClient)
	return err
}

func NewPrimaryIPv4(subnets ...string) (net.IP, error) {
	return pia.AllocateNextV4(subnets...)
}

func NewPrimaryIPv6(subnets ...string) (net.IP, error) {
	return pia.AllocateNextV6(subnets...)
}

// newPrimaryIPAllocator initializes a primaryIPAllocator with empty per-subnet maps.
// Subnet allocators are created lazily when AllocateNextV4/V6 is called with specific subnet CIDRs.
func newPrimaryIPAllocator(nodeClient v1.NodeInterface) (*primaryIPAllocator, error) {
	ipa := &primaryIPAllocator{
		mu:         &sync.Mutex{},
		nodeClient: nodeClient,
		v4Allocs:   make(map[string]*allocState),
		v6Allocs:   make(map[string]*allocState),
	}
	nodes, err := nodeClient.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return ipa, fmt.Errorf("failed to get a list of node(s): %v", err)
	}
	if len(nodes.Items) == 0 {
		return ipa, fmt.Errorf("expected at least one node but found zero")
	}
	return ipa, nil
}

// subnetKey returns a map key that groups IPs sharing the same first N-1 bytes.
// The last byte is zeroed so that e.g. 10.0.0.4 and 10.0.0.5 produce the same key.
func subnetKey(ip net.IP) string {
	ip16 := make(net.IP, 16)
	copy(ip16, ip.To16())
	ip16[15] = 0
	return string(ip16)
}

// getOrCreateV4 returns or lazily creates a per-subnet IPv4 allocator.
// Uses a /24 mask internally so the allocator produces IPs in the .200-.254 range
// regardless of the annotation's actual mask (which may be /17, /32, etc.).
func (p *primaryIPAllocator) getOrCreateV4(ip net.IP) *allocState {
	key := subnetKey(ip)
	if state, ok := p.v4Allocs[key]; ok {
		return state
	}
	ip4 := ip.To4()
	startIP := make(net.IP, 4)
	copy(startIP, ip4)
	startIP[3] = testIPv4Start - 1 // AllocateNextIP increments before returning
	maxIP := make(net.IP, 4)
	copy(maxIP, ip4)
	maxIP[3] = testIPv4End
	state := &allocState{
		alloc: newIPAllocator(&net.IPNet{IP: startIP, Mask: net.CIDRMask(24, 32)}),
		maxIP: maxIP.To16(),
	}
	p.v4Allocs[key] = state
	return state
}

// getOrCreateV6 returns or lazily creates a per-subnet IPv6 allocator.
// Uses a /120 mask internally so the allocator produces IPs in the ::c8-::ff range.
func (p *primaryIPAllocator) getOrCreateV6(ip net.IP) *allocState {
	key := subnetKey(ip)
	if state, ok := p.v6Allocs[key]; ok {
		return state
	}
	ip16 := ip.To16()
	startIP := make(net.IP, 16)
	copy(startIP, ip16)
	startIP[15] = testIPv6Start - 1
	maxIP := make(net.IP, 16)
	copy(maxIP, ip16)
	maxIP[15] = testIPv6End
	state := &allocState{
		alloc: newIPAllocator(&net.IPNet{IP: startIP, Mask: net.CIDRMask(120, 128)}),
		maxIP: maxIP,
	}
	p.v6Allocs[key] = state
	return state
}

// AllocateNextV4 allocates the next available IPv4 from the reserved range (.200-.254)
// within one of the provided subnet CIDRs. Each subnet string should be a CIDR like "10.0.0.4/17"
// derived from the node's k8s.ovn.org/node-primary-ifaddr annotation.
func (p *primaryIPAllocator) AllocateNextV4(subnets ...string) (net.IP, error) {
	if len(subnets) == 0 {
		return nil, fmt.Errorf("at least one subnet must be provided")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var lastErr error
	for _, subnet := range subnets {
		ip, _, err := net.ParseCIDR(subnet)
		if err != nil {
			lastErr = err
			continue
		}
		ip4 := ip.To4()
		if ip4 == nil {
			continue
		}
		state := p.getOrCreateV4(ip4)
		allocated, err := allocateIP(p.nodeClient, state.alloc.AllocateNextIP, state.maxIP)
		if err != nil {
			lastErr = err
			continue // try next subnet
		}
		return allocated, nil
	}
	return nil, fmt.Errorf("failed to allocate IPv4 from any of the provided subnets: %v", lastErr)
}

// AllocateNextV6 allocates the next available IPv6 from the reserved range (::c8-::ff)
// within one of the provided subnet CIDRs.
func (p *primaryIPAllocator) AllocateNextV6(subnets ...string) (net.IP, error) {
	if len(subnets) == 0 {
		return nil, fmt.Errorf("at least one subnet must be provided")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var lastErr error
	for _, subnet := range subnets {
		ip, _, err := net.ParseCIDR(subnet)
		if err != nil {
			lastErr = err
			continue
		}
		if ip.To4() != nil {
			continue // skip IPv4
		}
		state := p.getOrCreateV6(ip.To16())
		allocated, err := allocateIP(p.nodeClient, state.alloc.AllocateNextIP, state.maxIP)
		if err != nil {
			lastErr = err
			continue // try next subnet
		}
		return allocated, nil
	}
	return nil, fmt.Errorf("failed to allocate IPv6 from any of the provided subnets: %v", lastErr)
}

func (pia *primaryIPAllocator) IncrementAndGetNextV4(times int, subnets ...string) (net.IP, error) {
	var err error
	for i := 0; i < times; i++ {
		if _, err = pia.AllocateNextV4(subnets...); err != nil {
			return nil, err
		}
	}
	return pia.AllocateNextV4(subnets...)
}

func (pia *primaryIPAllocator) IncrementAndGetNextV6(times int, subnets ...string) (net.IP, error) {
	var err error
	for i := 0; i < times; i++ {
		if _, err = pia.AllocateNextV6(subnets...); err != nil {
			return nil, err
		}
	}
	return pia.AllocateNextV6(subnets...)
}

type allocNextFn func() (net.IP, error)

// allocateIP allocates the next available IP from the reserved range (.200-.254 for IPv4,
// ::c8-::ff for IPv6) that doesn't conflict with existing node IPs. maxIP is the upper
// bound (inclusive) of the reserved range; any candidate beyond it triggers an exhaustion error.
func allocateIP(nodeClient v1.NodeInterface, allocateFn allocNextFn, maxIP net.IP) (net.IP, error) {
	nodeList, err := nodeClient.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %v", err)
	}
	for {
		nextIP, err := allocateFn()
		if err != nil {
			return nil, fmt.Errorf("failed to allocate next IP address (reserved test range may be exhausted): %v", err)
		}
		if bytes.Compare(nextIP, maxIP) > 0 {
			return nil, fmt.Errorf("reserved IP range exhausted: next candidate %s exceeds maximum %s", net.IP(nextIP), net.IP(maxIP))
		}
		lastOctet := nextIP[len(nextIP)-1]
		// Skip reserved addresses (.0 is network, .1 typically gateway)
		// This shouldn't happen since we start from .200, but check anyway
		if lastOctet == 0 || lastOctet == 1 {
			continue
		}
		isConflict, err := isConflictWithExistingHostIPs(nodeList.Items, nextIP)
		if err != nil {
			return nil, fmt.Errorf("failed to determine if IP conflicts with existing IPs: %v", err)
		}
		if !isConflict {
			return nextIP, nil
		}
		// IP conflicts with a node, try next one
	}
}

func isConflictWithExistingHostIPs(nodes []corev1.Node, ip net.IP) (bool, error) {
	ipStr := ip.String()
	for _, node := range nodes {
		nodeIPsSet, err := util.ParseNodeHostCIDRsDropNetMask(&node)
		if err != nil {
			return false, fmt.Errorf("failed to parse node %s primary annotation info: %v", node.Name, err)
		}
		if nodeIPsSet.Has(ipStr) {
			return true, nil
		}
	}
	return false, nil
}
