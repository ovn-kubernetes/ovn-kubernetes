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

// AllocateUDNSubnets always allocates the same fixed UDN IPv4 and IPv6 subnet
// within the dedicated UDN subnet broader range. Used when overlaps across UDNs
// are not a concern but still prevents overlaps with other subnets.
func AllocateUDNSubnets() (ipv4, ipv6 string) {
	udnOnce.Do(func() {
		udnV4 = newSubnetSpec(udnSubnets, nil)
		udnV6 = newSubnetSpec(udnSubnets6, nil)
	})

	udnV4Idx := udnV4.nthFree(1)
	udnV6Idx := udnV6.nthFree(1)
	return udnV4.cidr(udnV4Idx), udnV6.cidr(udnV6Idx)
}
