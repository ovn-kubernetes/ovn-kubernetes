package ndp

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"

	"github.com/mdlayher/ndp"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv6"

	"k8s.io/klog/v2"
)

// NeighborAdvertisement represents a Neighbor Advertisement for an IPv6 address.
type NeighborAdvertisement interface {
	// IP returns the IPv6 address
	IP() net.IP
	// MAC returns the MAC address to advertise (nil means use interface MAC)
	MAC() *net.HardwareAddr
}

type neighborAdvertisement struct {
	ip  net.IP
	mac *net.HardwareAddr
}

// NewNeighborAdvertisement creates a new Unsolicited Neighbor Advertisement with validation that the IP is IPv6.
func NewNeighborAdvertisement(ip net.IP, mac *net.HardwareAddr) (NeighborAdvertisement, error) {
	if ip.To4() != nil {
		return nil, fmt.Errorf("only IPv6 addresses can be used for NeighborAdvertisement, got IPv4 %s", ip.String())
	}
	if ip.To16() == nil {
		return nil, fmt.Errorf("only IPv6 addresses can be used for NeighborAdvertisement, got %s", ip.String())
	}
	if ip.IsMulticast() || ip.IsUnspecified() {
		return nil, fmt.Errorf("invalid IPv6 NA target address: %s", ip.String())
	}

	return &neighborAdvertisement{
		ip:  ip.To16(),
		mac: mac,
	}, nil
}

// IP returns the IPv6 address
func (u *neighborAdvertisement) IP() net.IP {
	return u.ip
}

// MAC returns the MAC address to advertise
func (u *neighborAdvertisement) MAC() *net.HardwareAddr {
	return u.mac
}

// SendUnsolicitedNeighborAdvertisement sends an unsolicited neighbor advertisement for the given IPv6 address.
// If the mac address is not provided it will use the one from the interface.
// The advertisement is sent to the all-nodes multicast address (ff02::1).
// https://datatracker.ietf.org/doc/html/rfc4861#section-4.4
func SendUnsolicitedNeighborAdvertisement(interfaceName string, na NeighborAdvertisement) error {
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		return fmt.Errorf("failed finding interface %s: %v", interfaceName, err)
	}

	targetIP := na.IP()
	mac := na.MAC()
	if mac == nil {
		mac = &iface.HardwareAddr
	}

	targetAddr, ok := netip.AddrFromSlice(targetIP)
	if !ok {
		return fmt.Errorf("failed to convert IP %s to netip.Addr", targetIP.String())
	}

	// Use icmp.ListenPacket instead of ndp.Listen because ndp.Listen uses the interface name
	// for the IPv6 zone, which Go's net package caches. If the interface is recreated with the
	// same name but a different index, the cached zone becomes stale. Using the index directly
	// avoids this issue. Unspecified address handles cases where the IP isn't assigned to the interface.
	ic, err := icmp.ListenPacket("ip6:ipv6-icmp", netip.IPv6Unspecified().WithZone(strconv.Itoa(iface.Index)).String())
	if err != nil {
		return fmt.Errorf("failed to create NDP connection on %s: %w", interfaceName, err)
	}
	defer ic.Close()

	// Unsolicited neighbor advertisement from a host, should override any existing cache entries
	una := &ndp.NeighborAdvertisement{
		Router:        false,
		Solicited:     false,
		Override:      true,
		TargetAddress: targetAddr,
		Options: []ndp.Option{
			&ndp.LinkLayerAddress{
				Direction: ndp.Target,
				Addr:      *mac,
			},
		},
	}
	rawUNA, err := ndp.MarshalMessage(una)
	if err != nil {
		return fmt.Errorf("failed to marshal UNA message: %w", err)
	}

	// rfc4861 - hop Limit 255 for unsolicited neighbor advertisements as per RFC, send to all-nodes multicast address
	_, err = ic.IPv6PacketConn().WriteTo(rawUNA, &ipv6.ControlMessage{HopLimit: ndp.HopLimit}, &net.IPAddr{
		IP:   netip.IPv6LinkLocalAllNodes().AsSlice(),
		Zone: strconv.Itoa(iface.Index),
	})
	if err != nil {
		return fmt.Errorf("failed to send an unsolicited neighbor advertisement for IP %s over interface %s: %w", targetIP.String(), interfaceName, err)
	}

	klog.Infof("Sent an unsolicited neighbor advertisement for IP %s on interface %s with MAC: %s", targetIP.String(), interfaceName, mac.String())
	return nil
}
