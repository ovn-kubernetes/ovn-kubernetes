package ndp

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/mdlayher/socket"
	"golang.org/x/sys/unix"
)

// PrefixInformation represents a Prefix Information Option for Router Advertisements
type PrefixInformation struct {
	Prefix            net.IPNet
	ValidLifetime     uint32
	PreferredLifetime uint32
	OnLink            bool
	Autonomous        bool
}

// RouterAdvertisement with mac, ips and lifetime field to send
type RouterAdvertisement struct {
	SourceMAC, DestinationMAC net.HardwareAddr
	SourceIP, DestinationIP   net.IP
	Lifetime                  uint16
	PrefixInfos               []PrefixInformation
}

// SendRouterAdvertisements sends one or more Router Advertisements (RAs) on the specified network interface.
// This function requires raw socket capabilities because the source MAC and IP addresses in the RAs
// are not the ones from the interface used to send the packets.
//
// Parameters:
// - interfaceName: The name of the network interface to send the RAs on.
// - ras: A variadic list of RouterAdvertisement objects containing the details of each RA to be sent.
//
// Returns:
// - error: An error object if an error occurs, otherwise nil.
//
// The function performs the following steps:
// 1. Retrieves the network interface by name.
// 2. Creates a raw socket for sending packets.
// 3. Serializes each Router Advertisement into a byte slice.
// 4. Sends the serialized RAs using the raw socket.
func SendRouterAdvertisements(interfaceName string, ras ...RouterAdvertisement) error {
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		return fmt.Errorf("failed to find interface %s: %w", interfaceName, err)
	}
	c, err := socket.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, syscall.ETH_P_ALL, "ra", nil)
	if err != nil {
		return fmt.Errorf("failed to create raw socket to send unsolicited RAs: %w", err)
	}
	defer c.Close()

	serializedRAs, err := generateRouterAdvertisements(ras...)
	if err != nil {
		return fmt.Errorf("failed to generate Router Advertisements: %w", err)
	}

	// Send each serialized Router Advertisement using the raw socket.
	for _, serializedRA := range serializedRAs {
		if err := c.Sendto(serializedRA, &unix.SockaddrLinklayer{Ifindex: iface.Index}, 0); err != nil {
			return err
		}
	}
	return nil
}

func generateRouterAdvertisements(ras ...RouterAdvertisement) ([][]byte, error) {
	serializedRAs := [][]byte{}
	for _, ra := range ras {
		serializeBuffer := gopacket.NewSerializeBuffer()

		// Create the Ethernet layer with destination and source MAC addresses.
		ethernetLayer := layers.Ethernet{
			DstMAC:       ra.DestinationMAC,
			SrcMAC:       ra.SourceMAC,
			EthernetType: layers.EthernetTypeIPv6,
		}

		// Create the IPv6 layer with source and destination IP addresses.
		ip6Layer := layers.IPv6{
			Version:    6,
			NextHeader: layers.IPProtocolICMPv6,
			HopLimit:   255,
			SrcIP:      ra.SourceIP,
			DstIP:      ra.DestinationIP,
		}

		// Create the ICMPv6 layer for the Router Advertisement.
		icmp6Layer := layers.ICMPv6{
			TypeCode: layers.CreateICMPv6TypeCode(layers.ICMPv6TypeRouterAdvertisement, 0),
		}
		if err := icmp6Layer.SetNetworkLayerForChecksum(&ip6Layer); err != nil {
			return nil, err
		}

		// https://datatracker.ietf.org/doc/html/rfc4861#section-4.2
		// Managed address configuration flag.
		managedAddressFlag := uint8(0x80)

		// https://datatracker.ietf.org/doc/html/rfc4191#section-2.2
		// Prf (Default Router Preference)
		//   2-bit signed integer.  Indicates whether to prefer this
		//    router over other default routers.  If the Router Lifetime
		//    is zero, the preference value MUST be set to (00) by the
		//    sender and MUST be ignored by the receiver.  If the Reserved
		//    (10) value is received, the receiver MUST treat the value as
		//    if it were (00).
		defaultRoutePreferenceFlag := uint8(0x08)
		if ra.Lifetime == 0 {
			defaultRoutePreferenceFlag = uint8(0x00)
		}

		// Create the ICMPv6 Router Advertisement layer.
		options := layers.ICMPv6Options{{
			Type: layers.ICMPv6OptSourceAddress,
			Data: ra.SourceMAC,
		}}

		// Add Prefix Information Options if specified
		for _, prefixInfo := range ra.PrefixInfos {
			prefixData := createPrefixInfoData(&prefixInfo)
			options = append(options, layers.ICMPv6Option{
				Type: layers.ICMPv6OptPrefixInfo,
				Data: prefixData,
			})
		}

		raLayer := layers.ICMPv6RouterAdvertisement{
			HopLimit:       255,
			Flags:          managedAddressFlag | defaultRoutePreferenceFlag,
			RouterLifetime: ra.Lifetime,
			ReachableTime:  0,
			RetransTimer:   0,
			Options:        options,
		}

		// Serialize the layers into a byte slice.
		if err := gopacket.SerializeLayers(serializeBuffer, gopacket.SerializeOptions{ComputeChecksums: true, FixLengths: true},
			&ethernetLayer,
			&ip6Layer,
			&icmp6Layer,
			&raLayer,
		); err != nil {
			return nil, err
		}
		serializedRAs = append(serializedRAs, serializeBuffer.Bytes())
	}
	return serializedRAs, nil
}

// createPrefixInfoData creates the data payload for a Prefix Information Option
// according to RFC 4861 Section 4.6.2
func createPrefixInfoData(prefixInfo *PrefixInformation) []byte {
	data := make([]byte, 30)

	// Prefix Length (8 bits)
	prefixLen, _ := prefixInfo.Prefix.Mask.Size()
	data[0] = uint8(prefixLen)

	// Flags (8 bits)
	var flags uint8
	if prefixInfo.OnLink {
		flags |= 0x80 // L flag
	}
	if prefixInfo.Autonomous {
		flags |= 0x40 // A flag
	}
	data[1] = flags

	// Valid Lifetime (32 bits)
	binary.BigEndian.PutUint32(data[2:6], prefixInfo.ValidLifetime)

	// Preferred Lifetime (32 bits)
	binary.BigEndian.PutUint32(data[6:10], prefixInfo.PreferredLifetime)

	// Reserved (32 bits) - already zero from make

	// Prefix (128 bits)
	copy(data[14:], prefixInfo.Prefix.IP.To16())

	return data
}
