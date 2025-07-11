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

func TestK8sMgmtIntfName(t *testing.T) {
	tests := []struct {
		name        string
		chassisName string
		expected    string
	}{
		{
			name:        "without chassis name",
			chassisName: "",
			expected:    "ovn-k8s-mp0",
		},
		{
			name:        "with chassis name",
			chassisName: "test-chassis",
			expected:    "ovn-k8s-mp10",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config.Default.OvnChassisName = tt.chassisName
			if name := K8sMgmtIntfName(); name != tt.expected {
				t.Errorf("K8sMgmtIntfName() = %q, want %q", name, tt.expected)
			}
		})
	}
}
