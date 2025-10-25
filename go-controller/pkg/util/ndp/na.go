package ndp

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/mdlayher/ndp"
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

	c, _, err := ndp.Listen(iface, ndp.Addr(na.IP().String()))
	if err != nil {
		return fmt.Errorf("failed to create NDP connection on %s: %w", interfaceName, err)
	}
	defer c.Close()

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

	// rfc4861 - hop Limit 255 as per RFC, send to all-nodes multicast address
	if err := c.WriteTo(una, &ipv6.ControlMessage{HopLimit: ndp.HopLimit}, netip.IPv6LinkLocalAllNodes()); err != nil {
		return fmt.Errorf("failed to send an unsolicited neighbor advertisement for IP %s over interface %s: %w", targetIP.String(), interfaceName, err)
	}

	klog.Infof("Sent an unsolicited neighbor advertisement for IP %s on interface %s with MAC: %s", targetIP.String(), interfaceName, mac.String())
	return nil
}
