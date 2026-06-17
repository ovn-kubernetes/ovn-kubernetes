// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package vtep

import (
	"fmt"
	"net"

	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/node"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
)

const (
	ipv4HostSubnetLen = 32
	ipv6HostSubnetLen = 128
)

// vtepIPAllocator allocates individual VTEP IPs from a set of CIDRs for
// managed-mode VTEPs. It wraps node.SubnetAllocator with /32 (IPv4) and
// /128 (IPv6) host subnet lengths, so each "subnet" allocation is exactly
// one IP address. Multiple CIDRs of the same family are supported with
// natural overflow: the first range is exhausted before the next is used.
type vtepIPAllocator struct {
	allocator node.SubnetAllocator
	// cidrs records the CIDRs the allocator was built with in their original
	// spec order. It is a slice (not a set) because CEL rules on VTEPSpec
	// validate CIDRs positionally: existing entries can only be widened
	// in-place (same index), and new ones can only be appended. This means
	// cidrs[i] always corresponds to the i-th network range that was added
	// to the underlying allocator for its IP family. We cannot use
	// SubnetAllocator.ListAllIPv4/IPv6Networks() as a substitute because
	// those return ranges grouped by family (all v4, then all v6), which
	// loses the interleaved ordering of a dual-stack spec (e.g.
	// [v4, v6, v4]). Without the original ordering we could not map
	// spec[i] to the correct allocator range for replaceRange.
	cidrs []vtepv1.CIDR
}

// newVTEPIPAllocator creates an allocator from the given VTEP CIDRs. Each
// CIDR is added as a network range with a single-IP host prefix (/32 or /128).
func newVTEPIPAllocator(cidrs []vtepv1.CIDR) (*vtepIPAllocator, error) {
	a := &vtepIPAllocator{
		allocator: node.NewSubnetAllocator(),
		cidrs:     make([]vtepv1.CIDR, 0, len(cidrs)),
	}
	for _, cidr := range cidrs {
		if err := a.addCIDR(cidr); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// cidrsMatch returns true if the allocator's current CIDRs are identical
// (same length, same values in order) to the given slice.
func (a *vtepIPAllocator) cidrsMatch(cidrs []vtepv1.CIDR) bool {
	if len(a.cidrs) != len(cidrs) {
		return false
	}
	for i := range cidrs {
		if a.cidrs[i] != cidrs[i] {
			return false
		}
	}
	return true
}

func (a *vtepIPAllocator) addCIDR(cidr vtepv1.CIDR) error {
	_, ipNet, err := net.ParseCIDR(string(cidr))
	if err != nil {
		return fmt.Errorf("invalid CIDR %q: %w", cidr, err)
	}
	hostLen := ipv4HostSubnetLen
	if utilnet.IsIPv6CIDR(ipNet) {
		hostLen = ipv6HostSubnetLen
	}
	if err := a.allocator.AddNetworkRange(ipNet, hostLen); err != nil {
		return fmt.Errorf("failed to add CIDR %q to allocator: %w", cidr, err)
	}
	a.cidrs = append(a.cidrs, cidr)
	return nil
}

// allocateForNode allocates the next available IP per family for the given
// node. The underlying allocator is idempotent: if the node already has an
// allocation for a family it is returned as-is. Returns one /32 or /128
// net.IPNet per configured family (IPv4 first, then IPv6).
func (a *vtepIPAllocator) allocateForNode(nodeName string) ([]*net.IPNet, error) {
	return a.allocator.AllocateNetworks(nodeName)
}

// markAllocatedForNode reserves specific IPs for a node. Each IP is
// wrapped in a /32 or /128 net.IPNet before being passed to the
// underlying allocator. Used during initial sync to restore allocations
// from node annotations.
func (a *vtepIPAllocator) markAllocatedForNode(nodeName string, ips []net.IP) error {
	ipNets := make([]*net.IPNet, 0, len(ips))
	for _, ip := range ips {
		mask := net.CIDRMask(ipv4HostSubnetLen, ipv4HostSubnetLen)
		if utilnet.IsIPv6(ip) {
			mask = net.CIDRMask(ipv6HostSubnetLen, ipv6HostSubnetLen)
		}
		ipNets = append(ipNets, &net.IPNet{IP: ip, Mask: mask})
	}
	return a.allocator.MarkAllocatedNetworks(nodeName, ipNets...)
}

// replaceRange replaces an existing CIDR in the allocator with a wider one.
// The new CIDR must be a supernet of the old one (guaranteed by CEL validation
// in managed mode). Existing allocations from the old range are preserved.
// The cidrs tracking slice is updated at the same index.
func (a *vtepIPAllocator) replaceRange(idx int, oldCIDR, newCIDR vtepv1.CIDR) error {
	_, oldNet, err := net.ParseCIDR(string(oldCIDR))
	if err != nil {
		return fmt.Errorf("invalid old CIDR %q: %w", oldCIDR, err)
	}
	_, newNet, err := net.ParseCIDR(string(newCIDR))
	if err != nil {
		return fmt.Errorf("invalid new CIDR %q: %w", newCIDR, err)
	}
	hostLen := ipv4HostSubnetLen
	if utilnet.IsIPv6CIDR(newNet) {
		hostLen = ipv6HostSubnetLen
	}
	if err := a.allocator.ReplaceNetworkRange(oldNet, newNet, hostLen); err != nil {
		return err
	}
	a.cidrs[idx] = newCIDR
	return nil
}

// releaseNode frees all allocations for the given node.
func (a *vtepIPAllocator) releaseNode(nodeName string) {
	a.allocator.ReleaseAllNetworks(nodeName)
}
