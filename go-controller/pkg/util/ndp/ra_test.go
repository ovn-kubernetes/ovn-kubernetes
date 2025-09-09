package ndp

import (
	"encoding/binary"
	"net"
	"testing"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreatePrefixInfoData(t *testing.T) {
	tests := []struct {
		name     string
		prefix   string
		expected struct {
			prefixLen         uint8
			flags             uint8
			validLifetime     uint32
			preferredLifetime uint32
		}
		prefixInfo PrefixInformation
	}{
		{
			name:   "fd99::/64 with both flags set",
			prefix: "fd99::/64",
			expected: struct {
				prefixLen         uint8
				flags             uint8
				validLifetime     uint32
				preferredLifetime uint32
			}{
				prefixLen:         64,
				flags:             0xC0, // L=1, A=1 (0x80 | 0x40)
				validLifetime:     65535,
				preferredLifetime: 0,
			},
			prefixInfo: PrefixInformation{
				ValidLifetime:     65535,
				PreferredLifetime: 0,
				OnLink:            true,
				Autonomous:        true,
			},
		},
		{
			name:   "2001:db8::/32 with only OnLink flag",
			prefix: "2001:db8::/32",
			expected: struct {
				prefixLen         uint8
				flags             uint8
				validLifetime     uint32
				preferredLifetime uint32
			}{
				prefixLen:         32,
				flags:             0x80, // L=1, A=0
				validLifetime:     3600,
				preferredLifetime: 1800,
			},
			prefixInfo: PrefixInformation{
				ValidLifetime:     3600,
				PreferredLifetime: 1800,
				OnLink:            true,
				Autonomous:        false,
			},
		},
		{
			name:   "::1/128 with no flags",
			prefix: "::1/128",
			expected: struct {
				prefixLen         uint8
				flags             uint8
				validLifetime     uint32
				preferredLifetime uint32
			}{
				prefixLen:         128,
				flags:             0x00, // L=0, A=0
				validLifetime:     0,
				preferredLifetime: 0,
			},
			prefixInfo: PrefixInformation{
				ValidLifetime:     0,
				PreferredLifetime: 0,
				OnLink:            false,
				Autonomous:        false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, prefixNet, err := net.ParseCIDR(tt.prefix)
			require.NoError(t, err)

			tt.prefixInfo.Prefix = *prefixNet
			data := createPrefixInfoData(&tt.prefixInfo)

			// Verify the data length (should be 30 bytes)
			assert.Len(t, data, 30, "PrefixInfo data should be 30 bytes")

			// Verify prefix length
			assert.Equal(t, tt.expected.prefixLen, data[0], "Prefix length mismatch")

			// Verify flags
			assert.Equal(t, tt.expected.flags, data[1], "Flags mismatch")

			// Verify valid lifetime
			actualValidLifetime := binary.BigEndian.Uint32(data[2:6])
			assert.Equal(t, tt.expected.validLifetime, actualValidLifetime, "Valid lifetime mismatch")

			// Verify preferred lifetime
			actualPreferredLifetime := binary.BigEndian.Uint32(data[6:10])
			assert.Equal(t, tt.expected.preferredLifetime, actualPreferredLifetime, "Preferred lifetime mismatch")

			// Verify reserved field is zero
			reserved := binary.BigEndian.Uint32(data[10:14])
			assert.Equal(t, uint32(0), reserved, "Reserved field should be zero")

			// Verify prefix IP
			expectedPrefix := prefixNet.IP.To16()
			actualPrefix := data[14:30]
			assert.Equal(t, []byte(expectedPrefix), actualPrefix, "Prefix IP mismatch")
		})
	}
}

func TestRouterAdvertisementSerialization(t *testing.T) {
	sourceMAC := net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}
	destinationMAC := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	sourceIP := net.ParseIP("fe80::211:22ff:fe33:4455")
	destinationIP := net.ParseIP("fe80::aabb:ccff:fedd:eeff")

	// Create test prefix information
	_, prefix1, err := net.ParseCIDR("fd99::/64")
	require.NoError(t, err)

	prefixInfos := []PrefixInformation{
		{
			Prefix:            *prefix1,
			ValidLifetime:     65535,
			PreferredLifetime: 0,
			OnLink:            true,
			Autonomous:        true,
		},
	}

	ra := RouterAdvertisement{
		SourceMAC:      sourceMAC,
		SourceIP:       sourceIP,
		DestinationMAC: destinationMAC,
		DestinationIP:  destinationIP,
		Lifetime:       65535,
		PrefixInfos:    prefixInfos,
	}

	// Test serialization logic (without actually sending)
	serializeBuffer := gopacket.NewSerializeBuffer()

	// Create the Ethernet layer
	ethernetLayer := layers.Ethernet{
		DstMAC:       ra.DestinationMAC,
		SrcMAC:       ra.SourceMAC,
		EthernetType: layers.EthernetTypeIPv6,
	}

	// Create the IPv6 layer
	ip6Layer := layers.IPv6{
		Version:    6,
		NextHeader: layers.IPProtocolICMPv6,
		HopLimit:   255,
		SrcIP:      ra.SourceIP,
		DstIP:      ra.DestinationIP,
	}

	// Create the ICMPv6 layer
	icmp6Layer := layers.ICMPv6{
		TypeCode: layers.CreateICMPv6TypeCode(layers.ICMPv6TypeRouterAdvertisement, 0),
	}
	err = icmp6Layer.SetNetworkLayerForChecksum(&ip6Layer)
	require.NoError(t, err)

	// Create options including PrefixInfos
	options := layers.ICMPv6Options{{
		Type: layers.ICMPv6OptSourceAddress,
		Data: ra.SourceMAC,
	}}

	for _, prefixInfo := range ra.PrefixInfos {
		prefixData := createPrefixInfoData(&prefixInfo)
		options = append(options, layers.ICMPv6Option{
			Type: layers.ICMPv6OptPrefixInfo,
			Data: prefixData,
		})
	}

	// Flags
	managedAddressFlag := uint8(0x80)
	defaultRoutePreferenceFlag := uint8(0x08)
	if ra.Lifetime == 0 {
		defaultRoutePreferenceFlag = uint8(0x00)
	}

	raLayer := layers.ICMPv6RouterAdvertisement{
		HopLimit:       255,
		Flags:          managedAddressFlag | defaultRoutePreferenceFlag,
		RouterLifetime: ra.Lifetime,
		ReachableTime:  0,
		RetransTimer:   0,
		Options:        options,
	}

	// Serialize the layers
	err = gopacket.SerializeLayers(serializeBuffer, gopacket.SerializeOptions{ComputeChecksums: true, FixLengths: true},
		&ethernetLayer,
		&ip6Layer,
		&icmp6Layer,
		&raLayer,
	)
	require.NoError(t, err)

	serializedData := serializeBuffer.Bytes()
	assert.NotEmpty(t, serializedData, "Serialized data should not be empty")

	// Parse the serialized data to verify structure
	packet := gopacket.NewPacket(serializedData, layers.LayerTypeEthernet, gopacket.Default)

	// Verify Ethernet layer
	ethLayer := packet.Layer(layers.LayerTypeEthernet)
	require.NotNil(t, ethLayer)
	eth := ethLayer.(*layers.Ethernet)
	assert.Equal(t, destinationMAC, eth.DstMAC)
	assert.Equal(t, sourceMAC, eth.SrcMAC)
	assert.Equal(t, layers.EthernetTypeIPv6, eth.EthernetType)

	// Verify IPv6 layer
	ipv6Layer := packet.Layer(layers.LayerTypeIPv6)
	require.NotNil(t, ipv6Layer)
	ipv6 := ipv6Layer.(*layers.IPv6)
	assert.Equal(t, sourceIP, ipv6.SrcIP)
	assert.Equal(t, destinationIP, ipv6.DstIP)
	assert.Equal(t, layers.IPProtocolICMPv6, ipv6.NextHeader)

	// Verify ICMPv6 layer
	icmpv6Layer := packet.Layer(layers.LayerTypeICMPv6)
	require.NotNil(t, icmpv6Layer)
	icmpv6 := icmpv6Layer.(*layers.ICMPv6)
	assert.Equal(t, uint8(layers.ICMPv6TypeRouterAdvertisement), uint8(icmpv6.TypeCode.Type()))

	// Verify Router Advertisement layer
	raLayerParsed := packet.Layer(layers.LayerTypeICMPv6RouterAdvertisement)
	require.NotNil(t, raLayerParsed)
	raParsed := raLayerParsed.(*layers.ICMPv6RouterAdvertisement)
	assert.Equal(t, uint16(65535), raParsed.RouterLifetime)

	// Verify we have the expected options (source address + prefix info)
	assert.Len(t, raParsed.Options, 2, "Should have 2 options: source address and prefix info")

	// Check for source address option
	foundSourceOpt := false
	foundPrefixOpt := false
	for _, opt := range raParsed.Options {
		if opt.Type == layers.ICMPv6OptSourceAddress {
			foundSourceOpt = true
			assert.Equal(t, sourceMAC, net.HardwareAddr(opt.Data))
		}
		if opt.Type == layers.ICMPv6OptPrefixInfo {
			foundPrefixOpt = true
			assert.Len(t, opt.Data, 30, "Prefix info data should be 30 bytes")
			// Verify prefix length
			assert.Equal(t, uint8(64), opt.Data[0])
			// Verify flags (OnLink=1, Autonomous=1)
			assert.Equal(t, uint8(0xC0), opt.Data[1])
		}
	}
	assert.True(t, foundSourceOpt, "Should have source address option")
	assert.True(t, foundPrefixOpt, "Should have prefix info option")
}

func TestMultiplePrefixInfosSerialization(t *testing.T) {
	sourceMAC := net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}
	destinationMAC := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	sourceIP := net.ParseIP("fe80::211:22ff:fe33:4455")
	destinationIP := net.ParseIP("fe80::aabb:ccff:fedd:eeff")

	// Create multiple prefix information entries
	_, prefix1, err := net.ParseCIDR("fd99::/64")
	require.NoError(t, err)
	_, prefix2, err := net.ParseCIDR("2001:db8::/32")
	require.NoError(t, err)
	_, prefix3, err := net.ParseCIDR("fc00::/7")
	require.NoError(t, err)

	prefixInfos := []PrefixInformation{
		{
			Prefix:            *prefix1,
			ValidLifetime:     65535,
			PreferredLifetime: 0,
			OnLink:            true,
			Autonomous:        true,
		},
		{
			Prefix:            *prefix2,
			ValidLifetime:     3600,
			PreferredLifetime: 1800,
			OnLink:            true,
			Autonomous:        false,
		},
		{
			Prefix:            *prefix3,
			ValidLifetime:     7200,
			PreferredLifetime: 3600,
			OnLink:            false,
			Autonomous:        true,
		},
	}

	ra := RouterAdvertisement{
		SourceMAC:      sourceMAC,
		SourceIP:       sourceIP,
		DestinationMAC: destinationMAC,
		DestinationIP:  destinationIP,
		Lifetime:       65535,
		PrefixInfos:    prefixInfos,
	}

	// Test serialization with multiple PIOs
	serializeBuffer := gopacket.NewSerializeBuffer()

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
	err = icmp6Layer.SetNetworkLayerForChecksum(&ip6Layer)
	require.NoError(t, err)

	// Create options including all PrefixInfos
	options := layers.ICMPv6Options{{
		Type: layers.ICMPv6OptSourceAddress,
		Data: ra.SourceMAC,
	}}

	for _, prefixInfo := range ra.PrefixInfos {
		prefixData := createPrefixInfoData(&prefixInfo)
		options = append(options, layers.ICMPv6Option{
			Type: layers.ICMPv6OptPrefixInfo,
			Data: prefixData,
		})
	}

	managedAddressFlag := uint8(0x80)
	defaultRoutePreferenceFlag := uint8(0x08)

	raLayer := layers.ICMPv6RouterAdvertisement{
		HopLimit:       255,
		Flags:          managedAddressFlag | defaultRoutePreferenceFlag,
		RouterLifetime: ra.Lifetime,
		ReachableTime:  0,
		RetransTimer:   0,
		Options:        options,
	}

	err = gopacket.SerializeLayers(serializeBuffer, gopacket.SerializeOptions{ComputeChecksums: true, FixLengths: true},
		&ethernetLayer,
		&ip6Layer,
		&icmp6Layer,
		&raLayer,
	)
	require.NoError(t, err)

	serializedData := serializeBuffer.Bytes()
	assert.NotEmpty(t, serializedData)

	// Parse and verify
	packet := gopacket.NewPacket(serializedData, layers.LayerTypeEthernet, gopacket.Default)
	raLayerParsed := packet.Layer(layers.LayerTypeICMPv6RouterAdvertisement)
	require.NotNil(t, raLayerParsed)
	raParsed := raLayerParsed.(*layers.ICMPv6RouterAdvertisement)

	// Should have 1 source address option + 3 prefix info options
	assert.Len(t, raParsed.Options, 4, "Should have 4 options: 1 source address + 3 prefix infos")

	prefixOptCount := 0
	for _, opt := range raParsed.Options {
		if opt.Type == layers.ICMPv6OptPrefixInfo {
			prefixOptCount++
			assert.Len(t, opt.Data, 30, "Each prefix info should be 30 bytes")
		}
	}
	assert.Equal(t, 3, prefixOptCount, "Should have exactly 3 prefix info options")
}
