// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import "testing"

func TestNADKeyRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		name      string
		namespace string
		nad       string
		index     int
		want      string
	}{
		{"first attachment", "tenant", "network", 0, "tenant/network"},
		{"duplicate attachment", "tenant", "network", 1, "tenant/network/1"},
		{"large index", "tenant", "network", 1234, "tenant/network/1234"},
		{"negative index", "tenant", "network", -1, "tenant/network/-1"},
		{"empty namespace", "", "network", 2, "/network/2"},
		{"empty name", "tenant", "", 2, "tenant//2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			nadKey := GetNADName(tt.namespace, tt.nad)
			indexedKey := GetIndexedNADKey(nadKey, tt.index)
			if indexedKey != tt.want {
				t.Fatalf("indexed key = %q, want %q", indexedKey, tt.want)
			}
			gotKey, gotIndex, err := GetNadFromIndexedNADKey(indexedKey)
			if err != nil || gotKey != nadKey || gotIndex != tt.index {
				t.Fatalf("round trip = (%q, %d, %v), want (%q, %d, nil)", gotKey, gotIndex, err, nadKey, tt.index)
			}
		})
	}
}
