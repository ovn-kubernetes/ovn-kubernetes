package util

import (
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
)

func TestGetPatchPortNameWithCustomBridgeName(t *testing.T) {
	tests := []struct {
		bridgeID   string
		nodeName   string
		bridgeName string
		expected   string
	}{
		{"br-ex", "node1", "br-int", "patch-br-ex_node1-to-br-int"},
		{"br-ex", "node2", "custom-bridge", "patch-br-ex_node2-to-custom-bridge"},
	}

	for _, tt := range tests {
		config.Default.BridgeName = tt.bridgeName
		name := GetPatchPortName(tt.bridgeID, tt.nodeName)
		if name != tt.expected {
			t.Errorf("GetPatchPortName(%q, %q) with bridgeName=%q = %q, want %q", tt.bridgeID, tt.nodeName, tt.bridgeName, name, tt.expected)
		}
	}
}
