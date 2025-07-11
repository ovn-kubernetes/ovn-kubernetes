//go:build unit
// +build unit

package types

import "testing"

func TestGetPatchPortSuffix(t *testing.T) {
	tests := []struct {
		bridgeName string
		expected   string
	}{
		{"br-int", "-to-br-int"},
		{"custom-bridge", "-to-custom-bridge"},
		{"br0", "-to-br0"},
	}

	for _, tt := range tests {
		suffix := GetPatchPortSuffix(tt.bridgeName)
		if suffix != tt.expected {
			t.Errorf("GetPatchPortSuffix(%q) = %q, want %q", tt.bridgeName, suffix, tt.expected)
		}
	}
}
