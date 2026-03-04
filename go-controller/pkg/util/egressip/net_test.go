package egressip

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestGetNetlinkAddress(t *testing.T) {
	tests := []struct {
		name          string
		ip            net.IP
		ifindex       int
		expectedProto int
		expectedScope int
		expectedMask  int // prefix length
	}{
		{
			name:          "IPv4 address should have OVN protocol (84)",
			ip:            net.ParseIP("192.168.1.100"),
			ifindex:       5,
			expectedProto: ifaProtocolOVN,
			expectedScope: int(netlink.SCOPE_UNIVERSE),
			expectedMask:  32, // /32 for host address
		},
		{
			name:          "IPv6 address should have OVN protocol (84)",
			ip:            net.ParseIP("2001:db8::1"),
			ifindex:       10,
			expectedProto: ifaProtocolOVN,
			expectedScope: int(netlink.SCOPE_UNIVERSE),
			expectedMask:  128, // /128 for host address
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := GetNetlinkAddress(tt.ip, tt.ifindex)

			// Verify Protocol is set to OVN (84)
			if addr.Protocol != tt.expectedProto {
				t.Errorf("Protocol = %d, want %d (OVN)", addr.Protocol, tt.expectedProto)
			}

			// Verify Scope
			if addr.Scope != tt.expectedScope {
				t.Errorf("Scope = %d, want %d (SCOPE_UNIVERSE)", addr.Scope, tt.expectedScope)
			}

			// Verify LinkIndex
			if addr.LinkIndex != tt.ifindex {
				t.Errorf("LinkIndex = %d, want %d", addr.LinkIndex, tt.ifindex)
			}

			// Verify IP
			if !addr.IP.Equal(tt.ip) {
				t.Errorf("IP = %v, want %v", addr.IP, tt.ip)
			}

			// Verify mask (should be full mask for EgressIP)
			ones, _ := addr.Mask.Size()
			if ones != tt.expectedMask {
				t.Errorf("Mask prefix length = %d, want %d", ones, tt.expectedMask)
			}
		})
	}
}

func TestGetNetlinkAddressFlag(t *testing.T) {
	tests := []struct {
		name          string
		ip            net.IP
		expectedFlags int
	}{
		{
			name:          "IPv4 should have no special flags",
			ip:            net.ParseIP("192.168.1.100"),
			expectedFlags: 0,
		},
		{
			name:          "IPv6 should have IFA_F_NODAD flag",
			ip:            net.ParseIP("2001:db8::1"),
			expectedFlags: 0x2, // unix.IFA_F_NODAD
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags := getNetlinkAddressFlag(tt.ip)
			if flags != tt.expectedFlags {
				t.Errorf("getNetlinkAddressFlag() = %d, want %d", flags, tt.expectedFlags)
			}
		})
	}
}

func TestIfaProtocolOVN_Constant(t *testing.T) {
	// Verify ifaProtocolOVN is set to 84 (matching RTPROT_OVN)
	const expectedValue = 84
	if ifaProtocolOVN != expectedValue {
		t.Errorf("ifaProtocolOVN = %d, want %d", ifaProtocolOVN, expectedValue)
	}
}
