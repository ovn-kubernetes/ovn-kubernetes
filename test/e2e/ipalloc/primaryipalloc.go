// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ipalloc

import (
	"context"
	"fmt"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"net"
	"sync"
)

// primaryIPAllocator attempts to allocate an IP in the same subnet as a nodes primary network
type primaryIPAllocator struct {
	mu         *sync.Mutex
	v4         *ipAllocator
	v6         *ipAllocator
	nodeClient v1.NodeInterface
}

var pia *primaryIPAllocator

// InitPrimaryIPAllocator must be called to init IP allocator(s). Callers must be synchronise.
func InitPrimaryIPAllocator(nodeClient v1.NodeInterface) error {
	var err error
	pia, err = newPrimaryIPAllocator(nodeClient)
	return err
}

func NewPrimaryIPv4() (net.IP, error) {
	return pia.AllocateNextV4()
}

func NewPrimaryIPv6() (net.IP, error) {
	return pia.AllocateNextV6()
}

// newPrimaryIPAllocator gets a Nodes primary interfaces network info and allocates IPs from a reserved range
// within the same subnet. For serial test execution, IPs are allocated from .200-.254 (IPv4) or the equivalent
// range for IPv6 to avoid conflicts with node IPs which typically use lower addresses.
func newPrimaryIPAllocator(nodeClient v1.NodeInterface) (*primaryIPAllocator, error) {
	ipa := &primaryIPAllocator{mu: &sync.Mutex{}, nodeClient: nodeClient}
	nodes, err := nodeClient.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return ipa, fmt.Errorf("failed to get a list of node(s): %v", err)
	}
	if len(nodes.Items) == 0 {
		return ipa, fmt.Errorf("expected at least one node but found zero")
	}

	// Reserved range for E2E test IPs to avoid conflicts with node IPs
	const (
		testIPv4Start = 200 // Start from .200 to avoid typical node IPs (.1-.199)
		testIPv4End   = 254 // Last usable before .255 broadcast
		testIPv6Start = 200 // Same for IPv6 (0xC8)
		testIPv6End   = 255 // Last byte of reserved range (0xFF)
	)

	nodePrimaryIPs, err := util.ParseNodePrimaryIfAddr(&nodes.Items[0])
	if err != nil {
		return ipa, fmt.Errorf("failed to parse node primary interface address from Node object: %v", err)
	}
	if nodePrimaryIPs.V4.IP != nil {
		// Start from .199 so first allocation returns .200 (AllocateNextIP increments before returning)
		startIP := make(net.IP, len(nodePrimaryIPs.V4.IP))
		copy(startIP, nodePrimaryIPs.V4.IP)
		startIP[len(startIP)-1] = testIPv4Start - 1
		ipa.v4 = newIPAllocator(&net.IPNet{IP: startIP, Mask: nodePrimaryIPs.V4.Net.Mask})
	}
	if nodePrimaryIPs.V6.IP != nil {
		// Start from one less so first allocation returns the desired start IP
		startIP := make(net.IP, len(nodePrimaryIPs.V6.IP))
		copy(startIP, nodePrimaryIPs.V6.IP)
		startIP[len(startIP)-1] = testIPv6Start - 1
		ipa.v6 = newIPAllocator(&net.IPNet{IP: startIP, Mask: nodePrimaryIPs.V6.Net.Mask})
	}
	// Verify the starting IP from the reserved range is within all nodes' subnets
	if nodePrimaryIPs.V4.IP != nil {
		ipNets, err := getNodePrimaryProviderIPs(nodes.Items, false)
		if err != nil {
			return ipa, err
		}
		// Validate both the first (.200) and last (.254) IPs of the reserved range are
		// within all nodes' subnets so the allocator never produces out-of-subnet addresses.
		firstIP := make(net.IP, len(nodePrimaryIPs.V4.IP))
		copy(firstIP, nodePrimaryIPs.V4.IP)
		firstIP[len(firstIP)-1] = testIPv4Start
		if !isIPWithinAllSubnets(ipNets, firstIP) {
			return ipa, fmt.Errorf("IPv4 %s from reserved test range is not within all node subnets - this may indicate /32 node subnets or incompatible network configuration", firstIP)
		}
		lastIP := make(net.IP, len(nodePrimaryIPs.V4.IP))
		copy(lastIP, nodePrimaryIPs.V4.IP)
		lastIP[len(lastIP)-1] = testIPv4End
		if !isIPWithinAllSubnets(ipNets, lastIP) {
			return ipa, fmt.Errorf("IPv4 %s (end of reserved test range) is not within all node subnets - subnet may be too narrow to hold the full .%d-.%d range", lastIP, testIPv4Start, testIPv4End)
		}
	}
	if nodePrimaryIPs.V6.IP != nil {
		ipNets, err := getNodePrimaryProviderIPs(nodes.Items, true)
		if err != nil {
			return ipa, err
		}
		// Validate both the first (::c8) and last (::ff) IPs of the reserved range.
		firstIP := make(net.IP, len(nodePrimaryIPs.V6.IP))
		copy(firstIP, nodePrimaryIPs.V6.IP)
		firstIP[len(firstIP)-1] = testIPv6Start
		if !isIPWithinAllSubnets(ipNets, firstIP) {
			return ipa, fmt.Errorf("IPv6 %s from reserved test range is not within all node subnets - this may indicate /128 node subnets or incompatible network configuration", firstIP)
		}
		lastIP := make(net.IP, len(nodePrimaryIPs.V6.IP))
		copy(lastIP, nodePrimaryIPs.V6.IP)
		lastIP[len(lastIP)-1] = testIPv6End
		if !isIPWithinAllSubnets(ipNets, lastIP) {
			return ipa, fmt.Errorf("IPv6 %s (end of reserved test range) is not within all node subnets - subnet may be too narrow to hold the full ::%.2x-::%.2x range", lastIP, testIPv6Start, testIPv6End)
		}
	}

	return ipa, nil
}

func getNodePrimaryProviderIPs(nodes []corev1.Node, isIPv6 bool) ([]*net.IPNet, error) {
	ipNets := make([]*net.IPNet, 0, len(nodes))
	for _, node := range nodes {
		nodePrimaryIPs, err := util.ParseNodePrimaryIfAddr(&node)
		if err != nil {
			return nil, fmt.Errorf("failed to parse node primary interface address from Node %s object: %v", node.Name, err)
		}
		var mask net.IPMask
		var ip net.IP

		if isIPv6 {
			ip = nodePrimaryIPs.V6.IP
			mask = nodePrimaryIPs.V6.Net.Mask
		} else {
			ip = nodePrimaryIPs.V4.IP
			mask = nodePrimaryIPs.V4.Net.Mask
		}
		if len(ip) == 0 || len(mask) == 0 {
			return nil, fmt.Errorf("failed to find Node %s primary Node IP and/or mask", node.Name)
		}
		ipNets = append(ipNets, &net.IPNet{IP: ip, Mask: mask})
	}
	return ipNets, nil
}

func isIPWithinAllSubnets(ipNets []*net.IPNet, ip net.IP) bool {
	if len(ipNets) == 0 {
		return false
	}
	for _, ipNet := range ipNets {
		if !ipNet.Contains(ip) {
			return false
		}
	}
	return true
}

func (pia *primaryIPAllocator) IncrementAndGetNextV4(times int) (net.IP, error) {
	var err error
	for i := 0; i < times; i++ {
		if _, err = pia.AllocateNextV4(); err != nil {
			return nil, err
		}
	}
	return pia.AllocateNextV4()
}

func (pia *primaryIPAllocator) AllocateNextV4() (net.IP, error) {
	if pia.v4 == nil {
		return nil, fmt.Errorf("IPv4 is not enable ")
	}
	if pia.v4.net == nil {
		return nil, fmt.Errorf("IPv4 is not enabled but Allocation request was called")
	}
	pia.mu.Lock()
	defer pia.mu.Unlock()
	return allocateIP(pia.nodeClient, pia.v4.AllocateNextIP)
}

func (pia *primaryIPAllocator) IncrementAndGetNextV6(times int) (net.IP, error) {
	var err error
	for i := 0; i < times; i++ {
		if _, err = pia.AllocateNextV6(); err != nil {
			return nil, err
		}
	}
	return pia.AllocateNextV6()
}

func (pia primaryIPAllocator) AllocateNextV6() (net.IP, error) {
	if pia.v6 == nil {
		return nil, fmt.Errorf("IPv6 is not enabled but Allocation request was called")
	}
	if pia.v6.net == nil {
		return nil, fmt.Errorf("ipv6 network is not set")
	}
	pia.mu.Lock()
	defer pia.mu.Unlock()
	return allocateIP(pia.nodeClient, pia.v6.AllocateNextIP)
}

type allocNextFn func() (net.IP, error)

// allocateIP allocates the next available IP from the reserved range (.200-.254)
// that doesn't conflict with existing node IPs. For serial execution, this provides
// up to ~54 IPs per test run.
func allocateIP(nodeClient v1.NodeInterface, allocateFn allocNextFn) (net.IP, error) {
	nodeList, err := nodeClient.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %v", err)
	}
	for {
		nextIP, err := allocateFn()
		if err != nil {
			return nil, fmt.Errorf("failed to allocate next IP address (reserved test range may be exhausted): %v", err)
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
