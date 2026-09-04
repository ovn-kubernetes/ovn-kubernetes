// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package udn

import "testing"

func TestPickDeviceID(t *testing.T) {
	tests := []struct {
		name      string
		deviceIDs []string
		excluded  string
		expected  string
	}{
		{
			name:      "no exclusion picks last",
			deviceIDs: []string{"eth0-34", "eth0-53"},
			excluded:  "",
			expected:  "eth0-53",
		},
		{
			name:      "excluded last picks previous",
			deviceIDs: []string{"eth0-34", "eth0-53"},
			excluded:  "eth0-53",
			expected:  "eth0-34",
		},
		{
			name:      "single device excluded picks none",
			deviceIDs: []string{"eth0-34"},
			excluded:  "eth0-34",
			expected:  "",
		},
		{
			name:      "empty list picks none",
			deviceIDs: nil,
			excluded:  "",
			expected:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pickDeviceID(tt.deviceIDs, tt.excluded); got != tt.expected {
				t.Errorf("pickDeviceID(%v, %q) = %q, expected %q", tt.deviceIDs, tt.excluded, got, tt.expected)
			}
		})
	}
}
