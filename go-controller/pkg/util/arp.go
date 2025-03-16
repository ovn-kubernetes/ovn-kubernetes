package util

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/mdlayher/arp"
)

type GARP struct {
	// IP to advertise the MAC address
	IP net.IP
	// MAC to advertise (optional), default: link mac address
	MAC *net.HardwareAddr
}

// BroadcastGARP send a pair of GARPs with "request" and "reply" operations
// since some system response to request and others to reply.
// If "garp.MAC" is not passed the link form "interfaceName" mac will be
// advertise
func BroadcastGARP(interfaceName string, garp GARP) error {
	srcIP := netip.AddrFrom4([4]byte(garp.IP))

	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		return fmt.Errorf("failed finding interface %s: %v", interfaceName, err)
	}

	if garp.MAC == nil {
		garp.MAC = &iface.HardwareAddr
	}

	c, err := arp.Dial(iface)
	if err != nil {
		return fmt.Errorf("failed dialing %q: %v", interfaceName, err)
	}
	defer c.Close()

	// Note that some devices will respond to the gratuitous request and some
	// will respond to the gratuitous reply. If one is trying to write
	// software for moving IP addresses around that works with all routers,
	// switches and IP stacks, it is best to send both the request and the reply.
	// These are documented by [RFC 2002](https://tools.ietf.org/html/rfc2002)
	// and [RFC 826](https://tools.ietf.org/html/rfc826). Software implementing
	// the gratuitious ARP function can be found
	// [in the Linux-HA source tree](http://hg.linux-ha.org/lha-2.1/file/1d5b54f0a2e0/heartbeat/libnet_util/send_arp.c).
	//
	// ref: https://wiki.wireshark.org/Gratuitous_ARP
	for _, op := range []arp.Operation{arp.OperationRequest, arp.OperationReply} {
		// At at GARP the source and target IP should be the same and point to the
		// the IP we want to reconcile -> https://wiki.wireshark.org/Gratuitous_ARP
		p, err := arp.NewPacket(op, *garp.MAC /* srcHw */, srcIP, net.HardwareAddr{0, 0, 0, 0, 0, 0}, srcIP)
		if err != nil {
			return fmt.Errorf("failed creating %q GARP %+v: %w", op, garp, err)
		}

		if err := c.WriteTo(p, net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}); err != nil {
			return fmt.Errorf("failed sending %q GARP %+v:  %w", op, garp, err)
		}
	}

	return nil
}
