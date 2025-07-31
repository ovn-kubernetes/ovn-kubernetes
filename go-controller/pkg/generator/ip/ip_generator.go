package ip

import (
	"fmt"
	"math/big"
	"net"

	iputils "github.com/containernetworking/plugins/pkg/ip"

	utilnet "k8s.io/utils/net"
)

// IPGenerator is used to generate an IP from the provided CIDR and the index.
// It is not an allocator and doesn't maintain any cache.
type IPGenerator struct {
	netCidr   *net.IPNet
	netBaseIP *big.Int
}

// NewIPGenerator returns an ipGenerator instance
func NewIPGenerator(subnet string) (*IPGenerator, error) {
	_, netCidr, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, fmt.Errorf("error parsing subnet string %s: %v", subnet, err)
	}

	return &IPGenerator{
		netCidr:   netCidr,
		netBaseIP: utilnet.BigForIP(netCidr.IP),
	}, nil
}

// GenerateIP generates an IP from the base ip and the provided 'idx'
// and returns the IPNet with the generated IP and the netmask of
// cidr. If suppose the subnet was - 100.88.0.0/16 and the specified
// index is 10, it will return IPNet { IP : 100.88.0.10, Mask : 16}
// Returns error if the generated IP is out of network range.
func (ipGenerator *IPGenerator) GenerateIP(idx int) (*net.IPNet, error) {
	ip := utilnet.AddIPOffset(ipGenerator.netBaseIP, idx)
	if ipGenerator.netCidr.Contains(ip) {
		return &net.IPNet{IP: ip, Mask: ipGenerator.netCidr.Mask}, nil
	}
	return nil, fmt.Errorf("generated ip %s from the idx %d is out of range in the network %s", ip.String(), idx, ipGenerator.netCidr.String())
}

// GenerateIPPair generates a pair of CIDRs in a subnet of size 2 (/31 or /127), carved from a supernet.
// idx determines the offset of subnet chosen. For example if the supernet was 100.88.0.0/16,
// the ordered list of subnets would be:
// [idx=0] 100.88.0.0 - 100.88.0.1 (100.88.0.0/31)
// [idx=1] 100.88.0.2 - 100.88.0.3 (100.88.0.2/31)
// [idx=2] 100.88.0.4 - 100.88.0.5 (100.88.0.4/31)
func (ipGenerator *IPGenerator) GenerateIPPair(idx int) (*net.IPNet, *net.IPNet, error) {
	netMask := net.CIDRMask(31, 32)
	if utilnet.IsIPv6CIDR(ipGenerator.netCidr) {
		netMask = net.CIDRMask(127, 128)
	}
	numberOfIPs := 2
	// nodeIDs start from 1, netIP is the first IP of the subnet
	firstIP := utilnet.AddIPOffset(ipGenerator.netBaseIP, idx*numberOfIPs)
	if !ipGenerator.netCidr.Contains(firstIP) {
		return nil, nil, fmt.Errorf("generated ip %s from the idx %d is out of range in the network %s", firstIP.String(), idx, ipGenerator.netCidr.String())
	}
	secondIP := iputils.NextIP(firstIP)
	if secondIP == nil || !ipGenerator.netCidr.Contains(secondIP) {
		return nil, nil, fmt.Errorf("generated ip %s from the idx %d is out of range in the network %s", secondIP.String(), idx, ipGenerator.netCidr.String())
	}
	return &net.IPNet{IP: firstIP, Mask: netMask}, &net.IPNet{IP: secondIP, Mask: netMask}, nil
}
