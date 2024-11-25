package ndp

import (
	"net"
	"syscall"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/mdlayher/socket"
	"golang.org/x/sys/unix"
)

// RouterAdvertisment with mac, ips and lifetime field to send
type RouterAdvertisment struct {
	SourceMAC, DestinationMAC net.HardwareAddr
	SourceIP, DestinationIP   net.IP
	Lifetime                  uint16
}

// SendRouterAdvertisment will send and RA and cook the src mac and ip, to do
// so sended need raw socket privileged
func SendRouterAdvertisments(interfaceName string, ras ...RouterAdvertisment) error {
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		return err
	}
	c, err := socket.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, syscall.ETH_P_ALL, "ra", nil)
	if err != nil {
		return err
	}
	defer c.Close()
	for _, ra := range ras {
		ethernetLayer := layers.Ethernet{
			DstMAC:       ra.DestinationMAC,
			SrcMAC:       ra.SourceMAC,
			EthernetType: layers.EthernetTypeIPv6,
		}
		ip6Layer := layers.IPv6{
			Version:    6,
			NextHeader: layers.IPProtocolICMPv6,
			HopLimit:   255,
			SrcIP:      ra.SourceIP,
			DstIP:      ra.DestinationIP,
		}
		icmp6Layer := layers.ICMPv6{
			TypeCode: layers.CreateICMPv6TypeCode(layers.ICMPv6TypeRouterAdvertisement, 0),
		}
		if err := icmp6Layer.SetNetworkLayerForChecksum(&ip6Layer); err != nil {
			return err
		}
		managedAddressFlag := uint8(0x80)
		defaultRoutePreferenceFlag := uint8(0x08)
		raLayer := layers.ICMPv6RouterAdvertisement{
			HopLimit:       255,
			Flags:          managedAddressFlag | defaultRoutePreferenceFlag,
			RouterLifetime: ra.Lifetime,
			ReachableTime:  0,
			RetransTimer:   0,
			Options: layers.ICMPv6Options{{
				Type: layers.ICMPv6OptSourceAddress,
				Data: ra.SourceMAC,
			}},
		}
		serializeBuffer := gopacket.NewSerializeBuffer()
		if err := gopacket.SerializeLayers(serializeBuffer, gopacket.SerializeOptions{ComputeChecksums: true, FixLengths: true},
			&ethernetLayer,
			&ip6Layer,
			&icmp6Layer,
			&raLayer,
		); err != nil {
			return err
		}
		if err := c.Sendto(serializeBuffer.Bytes(), &unix.SockaddrLinklayer{Ifindex: iface.Index}, 0); err != nil {
			return err
		}
	}
	return nil
}
