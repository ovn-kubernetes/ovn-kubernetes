package node

import (
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func TestOpenFlowManagerDefaultNetOVSBridgeFinder(t *testing.T) {
	const nodeName = "multi-homing-worker-0.maiqueb.org"

	testCases := []struct {
		name                  string
		desc                  string
		inputPortInfo         string
		expectedBridgeName    string
		expectedPatchPortName string
	}{
		{
			name:                  "empty input ports",
			inputPortInfo:         "",
			expectedBridgeName:    "",
			expectedPatchPortName: "",
		},
		{
			name:                  "input ports without patch ports",
			inputPortInfo:         "port1",
			expectedBridgeName:    "",
			expectedPatchPortName: "",
		},
		{
			name: "input ports with a patch port",
			inputPortInfo: `
port1
port2
patch-br-ex_multi-homing-worker-0.maiqueb.org` + types.GetPatchPortSuffix(types.DefaultBridgeName),
			expectedBridgeName:    "br-ex",
			expectedPatchPortName: "patch-br-ex_multi-homing-worker-0.maiqueb.org" + types.GetPatchPortSuffix(types.DefaultBridgeName),
		},
		{
			name: "input ports with a patch port for a localnet network",
			inputPortInfo: `
port1
port2
patch-vlan2003_ovn_localnet_port` + types.GetPatchPortSuffix(types.DefaultBridgeName),
			expectedBridgeName:    "",
			expectedPatchPortName: "",
		},
		{
			name: "input ports with a patch port for the default network and a localnet",
			inputPortInfo: `
port1
port2
patch-vlan2003_ovn_localnet_port` + types.GetPatchPortSuffix(types.DefaultBridgeName) +
`patch-br-ex_multi-homing-worker-0.maiqueb.org` + types.GetPatchPortSuffix(types.DefaultBridgeName),
			expectedBridgeName:    "br-ex",
			expectedPatchPortName: "patch-br-ex_multi-homing-worker-0.maiqueb.org" + types.GetPatchPortSuffix(types.DefaultBridgeName),
		},
		{
			name: "input ports with a patch port for the default network, a localnet, and an extra primary UDN",
			inputPortInfo: `
port1
port2
patch-vlan2003_ovn_localnet_port` + types.GetPatchPortSuffix(types.DefaultBridgeName) +
`patch-br-ex_tenant-blue_multi-homing-worker-0.maiqueb.org" + types.GetPatchPortSuffix(types.DefaultBridgeName) + "
patch-br-ex_multi-homing-worker-0.maiqueb.org` + types.GetPatchPortSuffix(types.DefaultBridgeName),
			expectedBridgeName:    "br-ex",
			expectedPatchPortName: "patch-br-ex_multi-homing-worker-0.maiqueb.org" + types.GetPatchPortSuffix(types.DefaultBridgeName),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			bridgeName, patchPortName := localnetPortInfo(nodeName, tc.inputPortInfo)
			if bridgeName != tc.expectedBridgeName {
				t.Errorf("Expected bridge name %q got %q", tc.expectedBridgeName, bridgeName)
			}
			if patchPortName != tc.expectedPatchPortName {
				t.Errorf("Expected patch port name %q got %q", tc.expectedPatchPortName, patchPortName)
			}
		})
	}
}
