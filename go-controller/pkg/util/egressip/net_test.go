// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"math"
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestGetNetlinkAddress(t *testing.T) {
	tests := []struct {
		name             string
		ip               net.IP
		ifindex          int
		expectedProto    int
		expectedScope    int
		expectedMask     int
		expectedFlags    int
		expectedValidLft int
	}{
		{
			name:             "IPv4 address",
			ip:               net.ParseIP("192.168.1.100"),
			ifindex:          5,
			expectedProto:    ifaProtocolOVN,
			expectedScope:    int(netlink.SCOPE_UNIVERSE),
			expectedMask:     32,
			expectedFlags:    0,
			expectedValidLft: 0,
		},
		{
			name:             "IPv6 address",
			ip:               net.ParseIP("2001:db8::1"),
			ifindex:          10,
			expectedProto:    ifaProtocolOVN,
			expectedScope:    int(netlink.SCOPE_UNIVERSE),
			expectedMask:     128,
			expectedFlags:    unix.IFA_F_NODAD,
			expectedValidLft: math.MaxUint32,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := GetNetlinkAddress(tt.ip, tt.ifindex)

			// Verify Protocol is set to OVN (84)
			if addr.Protocol != tt.expectedProto {
				t.Errorf("Protocol = %d, want %d", addr.Protocol, tt.expectedProto)
			}

			// Verify Scope
			if addr.Scope != tt.expectedScope {
				t.Errorf("Scope = %d, want %d", addr.Scope, tt.expectedScope)
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

			// Verify Flags
			if addr.Flags != tt.expectedFlags {
				t.Errorf("Flags = %d, want %d", addr.Flags, tt.expectedFlags)
			}

			// Verify ValidLft
			if addr.ValidLft != tt.expectedValidLft {
				t.Errorf("ValidLft = %d, want %d", addr.ValidLft, tt.expectedValidLft)
			}
		})
	}
}
