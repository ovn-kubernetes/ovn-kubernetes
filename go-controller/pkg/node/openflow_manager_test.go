package node

import (
	"testing"
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
patch-br-ex_multi-homing-worker-0.maiqueb.org-to-br-int`,
			expectedBridgeName:    "br-ex",
			expectedPatchPortName: "patch-br-ex_multi-homing-worker-0.maiqueb.org-to-br-int",
		},
		{
			name: "input ports with a patch port for a localnet network",
			inputPortInfo: `
port1
port2
patch-vlan2003_ovn_localnet_port-to-br-int`,
			expectedBridgeName:    "",
			expectedPatchPortName: "",
		},
		{
			name: "input ports with a patch port for the default network and a localnet",
			inputPortInfo: `
port1
port2
patch-vlan2003_ovn_localnet_port-to-br-int
patch-br-ex_multi-homing-worker-0.maiqueb.org-to-br-int`,
			expectedBridgeName:    "br-ex",
			expectedPatchPortName: "patch-br-ex_multi-homing-worker-0.maiqueb.org-to-br-int",
		},
		{
			name: "input ports with a patch port for the default network, a localnet, and an extra primary UDN",
			inputPortInfo: `
port1
port2
patch-vlan2003_ovn_localnet_port-to-br-int
patch-br-ex_tenant-blue_multi-homing-worker-0.maiqueb.org-to-br-int
patch-br-ex_multi-homing-worker-0.maiqueb.org-to-br-int`,
			expectedBridgeName:    "br-ex",
			expectedPatchPortName: "patch-br-ex_multi-homing-worker-0.maiqueb.org-to-br-int",
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

func TestAddGroupCacheEntry(t *testing.T) {
	testCases := []struct {
		name          string
		initialCache  map[string]map[uint32]string
		key           string
		groupString   string
		expectedID    uint32
		expectedCache map[string]map[uint32]string
	}{
		{
			name:         "add first group to empty cache",
			initialCache: map[string]map[uint32]string{},
			key:          "service1",
			groupString:  "type=select,bucket=actions=output:1",
			expectedID:   0,
			expectedCache: map[string]map[uint32]string{
				"service1": {
					0: "type=select,bucket=actions=output:1",
				},
			},
		},
		{
			name: "add group with next available ID",
			initialCache: map[string]map[uint32]string{
				"service1": {
					0: "type=select,bucket=actions=output:1",
				},
			},
			key:         "service2",
			groupString: "type=select,bucket=actions=output:2",
			expectedID:  1,
			expectedCache: map[string]map[uint32]string{
				"service1": {
					0: "type=select,bucket=actions=output:1",
				},
				"service2": {
					1: "type=select,bucket=actions=output:2",
				},
			},
		},
		{
			name: "return existing ID for duplicate group string under same key",
			initialCache: map[string]map[uint32]string{
				"service1": {
					0: "type=select,bucket=actions=output:1",
					2: "type=select,bucket=actions=output:3",
				},
			},
			key:         "service1",
			groupString: "type=select,bucket=actions=output:3",
			expectedID:  2,
			expectedCache: map[string]map[uint32]string{
				"service1": {
					0: "type=select,bucket=actions=output:1",
					2: "type=select,bucket=actions=output:3",
				},
			},
		},
		{
			name: "add group to existing key",
			initialCache: map[string]map[uint32]string{
				"service1": {
					0: "type=select,bucket=actions=output:1",
				},
			},
			key:         "service1",
			groupString: "type=select,bucket=actions=output:2",
			expectedID:  1,
			expectedCache: map[string]map[uint32]string{
				"service1": {
					0: "type=select,bucket=actions=output:1",
					1: "type=select,bucket=actions=output:2",
				},
			},
		},
		{
			name: "find next available ID with gaps",
			initialCache: map[string]map[uint32]string{
				"service1": {
					0:   "type=select,bucket=actions=output:1",
					100: "type=select,bucket=actions=output:2",
				},
			},
			key:         "service2",
			groupString: "type=select,bucket=actions=output:3",
			expectedID:  1,
			expectedCache: map[string]map[uint32]string{
				"service1": {
					0:   "type=select,bucket=actions=output:1",
					100: "type=select,bucket=actions=output:2",
				},
				"service2": {
					1: "type=select,bucket=actions=output:3",
				},
			},
		},
		{
			name: "add group with multiple keys having groups",
			initialCache: map[string]map[uint32]string{
				"service1": {
					0: "type=select,bucket=actions=output:1",
					1: "type=select,bucket=actions=output:2",
				},
				"service2": {
					2: "type=select,bucket=actions=output:3",
				},
			},
			key:         "service3",
			groupString: "type=select,bucket=actions=output:4",
			expectedID:  3,
			expectedCache: map[string]map[uint32]string{
				"service1": {
					0: "type=select,bucket=actions=output:1",
					1: "type=select,bucket=actions=output:2",
				},
				"service2": {
					2: "type=select,bucket=actions=output:3",
				},
				"service3": {
					3: "type=select,bucket=actions=output:4",
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ofm := &openflowManager{
				groupCache: tc.initialCache,
			}

			groupID, err := ofm.addGroupCacheEntry(tc.key, tc.groupString)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
			if groupID != tc.expectedID {
				t.Errorf("Expected group ID %d but got %d", tc.expectedID, groupID)
			}

			// Verify cache state
			if len(ofm.groupCache) != len(tc.expectedCache) {
				t.Errorf("Expected cache to have %d keys but got %d", len(tc.expectedCache), len(ofm.groupCache))
			}

			for key, expectedGroups := range tc.expectedCache {
				actualGroups, ok := ofm.groupCache[key]
				if !ok {
					t.Errorf("Expected key %q to exist in cache but it doesn't", key)
					continue
				}
				if len(actualGroups) != len(expectedGroups) {
					t.Errorf("Expected %d groups for key %q but got %d", len(expectedGroups), key, len(actualGroups))
					continue
				}
				for gid, expectedStr := range expectedGroups {
					actualStr, ok := actualGroups[gid]
					if !ok {
						t.Errorf("Expected group ID %d to exist for key %q but it doesn't", gid, key)
						continue
					}
					if actualStr != expectedStr {
						t.Errorf("Expected group string %q for ID %d but got %q", expectedStr, gid, actualStr)
					}
				}
			}
		})
	}
}
