package egressip

import (
	"math"
	"net"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// GetNetlinkAddress returns a netlink address configured with specific
// egress ip parameters
func GetNetlinkAddress(ip net.IP, ifindex int) *netlink.Addr {
	return &netlink.Addr{
		IPNet:     &net.IPNet{IP: ip, Mask: util.GetIPFullMask(ip)},
		Flags:     getNetlinkAddressFlag(ip),
		Scope:     int(netlink.SCOPE_UNIVERSE),
		ValidLft:  getNetlinkAddressValidLft(ip),
		LinkIndex: ifindex,
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
