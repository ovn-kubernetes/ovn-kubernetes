// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"math"
	"net"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// GetNetlinkAddress returns a netlink address configured with specific
// egress ip parameters. The address is marked with IFA_PROTO=85 (OVN-K)
// to indicate it is managed by OVN-Kubernetes. This allows external tools
// to identify and filter out OVN-Kubernetes-managed addresses when
// capturing interface state.
// Note: IFA_PROTO requires Linux kernel 5.18+; on older kernels, the
// attribute is silently ignored.
func GetNetlinkAddress(ip net.IP, ifindex int) *netlink.Addr {
	return &netlink.Addr{
		IPNet:     &net.IPNet{IP: ip, Mask: util.GetIPFullMask(ip)},
		Flags:     getNetlinkAddressFlag(ip),
		Scope:     int(netlink.SCOPE_UNIVERSE),
		ValidLft:  getNetlinkAddressValidLft(ip),
		LinkIndex: ifindex,
		Protocol:  types.IFAProtOVNK, // Mark as OVN-Kubernetes-managed for external tooling
	}
}

func getNetlinkAddressFlag(ip net.IP) int {
	// isV6?
	if ip != nil && ip.To4() == nil && ip.To16() != nil {
		return unix.IFA_F_NODAD
	}
	return 0
}

func getNetlinkAddressValidLft(ip net.IP) int {
	// isV6?
	if ip != nil && ip.To4() == nil && ip.To16() != nil {
		return math.MaxUint32
	}
	return 0
}
