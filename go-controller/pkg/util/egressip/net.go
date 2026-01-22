package egressip

import (
	"math"
	"net"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// ifaProtocolOVN is the IFA_PROTO value used to mark addresses as OVN-managed.
// This matches RTPROT_OVN (84) used for OVN-managed routes.
const ifaProtocolOVN = 84

// GetNetlinkAddress returns a netlink address configured with specific
// egress ip parameters. The address is marked with IFA_PROTO=84 (OVN)
// to indicate it is managed by OVN-Kubernetes. This allows tools like
// nmstate to identify and filter out OVN-managed addresses when capturing
// interface state, preventing them from being persisted to .nmconnection files.
// Note: IFA_PROTO requires Linux kernel 5.18+; on older kernels, the
// attribute is silently ignored.
func GetNetlinkAddress(ip net.IP, ifindex int) *netlink.Addr {
	return &netlink.Addr{
		IPNet:     &net.IPNet{IP: ip, Mask: util.GetIPFullMask(ip)},
		Flags:     getNetlinkAddressFlag(ip),
		Scope:     int(netlink.SCOPE_UNIVERSE),
		ValidLft:  getNetlinkAddressValidLft(ip),
		LinkIndex: ifindex,
		Protocol:  ifaProtocolOVN, // Mark as OVN-managed for nmstate filtering
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
