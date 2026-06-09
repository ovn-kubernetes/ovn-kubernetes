// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package allocators

import (
	"sync"
)

var (
	udnOnce      sync.Once
	udnV4, udnV6 subnetSpec
)

func initSubnetSpecs() {
	udnOnce.Do(func() {
		udnV4 = newSubnetSpec(udnSubnets, nil)
		udnV6 = newSubnetSpec(udnSubnets6, nil)
	})
}

// GetFirstUDNSubnets always allocates the first UDN IPv4 and IPv6 subnet
// within the dedicated UDN subnet broader range. Used when overlaps across UDNs
// are not a concern but still prevents overlaps with other subnets.
func GetFirstUDNSubnets() (ipv4, ipv6 string) {
	subnets4, subnets6 := GetNthFirstUDNSubnets(1)
	return subnets4[0], subnets6[0]
}

// GetNthFirstUDNSubnets returns the first n UDN IPv4 and IPv6 subnets within
// the dedicated UDN subnet broader range. Used when overlaps across UDNs are
// not a concern but still prevents overlaps with other subnets.
func GetNthFirstUDNSubnets(n int) (ipv4, ipv6 []string) {
	if n < 1 {
		panic("GetNthFirstUDNSubnets: n must be >= 1")
	}
	initSubnetSpecs()
	if n > udnV4.usable() || n > udnV6.usable() {
		panic("GetNthFirstUDNSubnets: not enough free subnets available")
	}

	ipv4 = make([]string, 0, n)
	ipv6 = make([]string, 0, n)
	for i := 1; i < n+1; i++ {
		udnV4Idx := udnV4.nthFree(i)
		udnV6Idx := udnV6.nthFree(i)
		ipv4 = append(ipv4, udnV4.cidr(udnV4Idx))
		ipv6 = append(ipv6, udnV6.cidr(udnV6Idx))
	}
	return ipv4, ipv6
}
