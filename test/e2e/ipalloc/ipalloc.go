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
	return b[len(b)-16:], nil
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
