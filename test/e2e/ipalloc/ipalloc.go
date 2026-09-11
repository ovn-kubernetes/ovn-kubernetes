// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ipalloc

import (
	"fmt"
	"math/big"
	"net"
)

type ipAllocator struct {
	net *net.IPNet
	// base is a cached version of the start IP in the CIDR range as a *big.Int
	base *big.Int
	// max is the maximum size of the usable addresses in the range
	max   int
	count int
}

func newIPAllocator(cidr *net.IPNet) *ipAllocator {
	return &ipAllocator{net: cidr, base: getBaseInt(cidr.IP), max: limit(cidr)}
}

func (n *ipAllocator) AllocateNextIP() (net.IP, error) {
	if n.count >= n.max {
		return net.IP{}, fmt.Errorf("limit of %d reached", n.max)
	}
	n.base.Add(n.base, big.NewInt(1))
	n.count += 1
	b := n.base.Bytes()
	b = append(make([]byte, 16), b...)
	next := net.IP(b[len(b)-16:])
	// max is measured from the network address, not from the start IP, so it alone lets the walk leave the range.
	if !n.net.Contains(next) {
		return net.IP{}, fmt.Errorf("next address %s is outside %s", next, n.net)
	}
	if ipv4Broadcast(n.net, next) {
		return net.IP{}, fmt.Errorf("next address %s is the broadcast address of %s", next, n.net)
	}
	return next, nil
}

func ipv4Broadcast(ipNet *net.IPNet, ip net.IP) bool {
	v4, net4 := ip.To4(), ipNet.IP.To4()
	if v4 == nil || net4 == nil {
		return false
	}
	mask := ipNet.Mask
	if len(mask) == net.IPv6len {
		mask = mask[12:]
	}
	if len(mask) != net.IPv4len {
		return false
	}
	last := make(net.IP, net.IPv4len)
	for i := range last {
		last[i] = net4[i] | ^mask[i]
	}
	return v4.Equal(last)
}

func getBaseInt(ip net.IP) *big.Int {
	return big.NewInt(0).SetBytes(ip.To16())
}

func limit(subnet *net.IPNet) int {
	ones, bits := subnet.Mask.Size()
	if bits == 32 && (bits-ones) >= 31 || bits == 128 && (bits-ones) >= 127 {
		return 0
	}
	// limit to 2^8 (256) IPs for e2es
	if bits == 128 && (bits-ones) >= 8 {
		return int(1) << uint(8)
	}
	return int(1) << uint(bits-ones)
}
