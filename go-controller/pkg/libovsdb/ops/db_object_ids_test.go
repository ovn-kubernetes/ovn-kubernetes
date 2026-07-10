// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import "testing"

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
