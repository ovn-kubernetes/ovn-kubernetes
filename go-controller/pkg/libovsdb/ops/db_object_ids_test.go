// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import "testing"

func TestPortGroupNetworkQoSExternalIDs(t *testing.T) {
	ids := NewDbObjectIDs(PortGroupNetworkQoS, "default-network-controller", map[ExternalIDKey]string{
		ObjectNameKey: "network-qos-test:policy",
	})

	want := map[string]string{
		OwnerControllerKey.String(): "default-network-controller",
		OwnerTypeKey.String():       string(NetworkQoSOwnerType),
		ObjectNameKey.String():      "network-qos-test:policy",
		PrimaryIDKey.String():       "default-network-controller:NetworkQoS:network-qos-test:policy",
	}
	for key, value := range want {
		if got := ids.GetExternalIDs()[key]; got != value {
			t.Fatalf("external ID %s = %q, want %q", key, got, value)
		}
	}
}

func TestBuildCIDRKey(t *testing.T) {
	tests := []struct {
		name string
		cidr string
		want string
	}{
		{
			name: "IPv4 CIDR is unchanged",
			cidr: "10.244.0.0/16",
			want: "10.244.0.0/16",
		},
		{
			// IPv6 ":" must be replaced: it is the delimiter used to build the
			// primary ID from external ID values.
			name: "IPv6 CIDR colons are replaced with dots",
			cidr: "fd01::/48",
			want: "fd01../48",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := BuildCIDRKey(tc.cidr); got != tc.want {
				t.Fatalf("BuildCIDRKey(%q) = %q, want %q", tc.cidr, got, tc.want)
			}
		})
	}
}
