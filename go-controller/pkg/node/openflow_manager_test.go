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

func TestUpdateFlowCacheEntry(t *testing.T) {
	testCases := []struct {
		name               string
		initialFlowCache   map[string][]string
		initialGroupCache  map[string]map[uint32]string
		key                string
		flows              []string
		expectedFlowCache  map[string][]string
		expectedGroupCache map[string]map[uint32]string
	}{
		{
			name:              "add flows to empty cache",
			initialFlowCache:  map[string][]string{},
			initialGroupCache: map[string]map[uint32]string{},
			key:               "service1",
			flows: []string{
				"table=0,priority=100,in_port=1,actions=output:2",
				"table=0,priority=200,in_port=2,actions=output:1",
			},
			expectedFlowCache: map[string][]string{
				"service1": {
					"table=0,priority=100,in_port=1,actions=output:2",
					"table=0,priority=200,in_port=2,actions=output:1",
				},
			},
			expectedGroupCache: map[string]map[uint32]string{},
		},
		{
			name: "update existing flows",
			initialFlowCache: map[string][]string{
				"service1": {
					"table=0,priority=100,in_port=1,actions=output:2",
				},
			},
			initialGroupCache: map[string]map[uint32]string{},
			key:               "service1",
			flows: []string{
				"table=0,priority=100,in_port=1,actions=output:3",
				"table=0,priority=200,in_port=3,actions=output:1",
			},
			expectedFlowCache: map[string][]string{
				"service1": {
					"table=0,priority=100,in_port=1,actions=output:3",
					"table=0,priority=200,in_port=3,actions=output:1",
				},
			},
			expectedGroupCache: map[string]map[uint32]string{},
		},
		{
			name:             "remove superfluous groups when updating flows",
			initialFlowCache: map[string][]string{},
			initialGroupCache: map[string]map[uint32]string{
				"service1": {
					0: "type=select,bucket=actions=output:1",
					1: "type=select,bucket=actions=output:2",
					2: "type=select,bucket=actions=output:3",
					5: "type=select,bucket=actions=output:4",
				},
			},
			key: "service1",
			flows: []string{
				"table=0,priority=100,in_port=1,actions=group:1",
			},
			expectedFlowCache: map[string][]string{
				"service1": {
					"table=0,priority=100,in_port=1,actions=group:1",
				},
			},
			expectedGroupCache: map[string]map[uint32]string{
				"service1": {
					1: "type=select,bucket=actions=output:2",
				},
			},
		},
		{
			name:             "keep groups referenced in multiple flows",
			initialFlowCache: map[string][]string{},
			initialGroupCache: map[string]map[uint32]string{
				"service1": {
					0: "type=select,bucket=actions=output:1",
					1: "type=select,bucket=actions=output:2",
				},
			},
			key: "service1",
			flows: []string{
				"table=0,priority=100,in_port=1,actions=group:0",
				"table=0,priority=200,in_port=2,actions=group:0",
				"table=0,priority=300,in_port=3,actions=group:0",
			},
			expectedFlowCache: map[string][]string{
				"service1": {
					"table=0,priority=100,in_port=1,actions=group:0",
					"table=0,priority=200,in_port=2,actions=group:0",
					"table=0,priority=300,in_port=3,actions=group:0",
				},
			},
			expectedGroupCache: map[string]map[uint32]string{
				"service1": {
					0: "type=select,bucket=actions=output:1",
				},
			},
		},
		{
			name: "preserve groups for other keys",
			initialFlowCache: map[string][]string{
				"service2": {
					"table=0,priority=100,in_port=5,actions=group:10",
				},
			},
			initialGroupCache: map[string]map[uint32]string{
				"service1": {
					0: "type=select,bucket=actions=output:1",
					1: "type=select,bucket=actions=output:2",
				},
				"service2": {
					10: "type=select,bucket=actions=output:5",
				},
			},
			key: "service1",
			flows: []string{
				"table=0,priority=100,in_port=1,actions=group:0",
			},
			expectedFlowCache: map[string][]string{
				"service1": {
					"table=0,priority=100,in_port=1,actions=group:0",
				},
				"service2": {
					"table=0,priority=100,in_port=5,actions=group:10",
				},
			},
			expectedGroupCache: map[string]map[uint32]string{
				"service1": {
					0: "type=select,bucket=actions=output:1",
				},
				"service2": {
					10: "type=select,bucket=actions=output:5",
				},
			},
		},
		{
			name:             "remove all groups when flows have no group references",
			initialFlowCache: map[string][]string{},
			initialGroupCache: map[string]map[uint32]string{
				"service1": {
					0: "type=select,bucket=actions=output:1",
					1: "type=select,bucket=actions=output:2",
				},
			},
			key: "service1",
			flows: []string{
				"table=0,priority=100,in_port=1,actions=output:2",
				"table=0,priority=200,in_port=2,actions=output:1",
			},
			expectedFlowCache: map[string][]string{
				"service1": {
					"table=0,priority=100,in_port=1,actions=output:2",
					"table=0,priority=200,in_port=2,actions=output:1",
				},
			},
			expectedGroupCache: map[string]map[uint32]string{},
		},
		{
			name:             "remove all flows that point to non-existing groups for the key",
			initialFlowCache: map[string][]string{},
			initialGroupCache: map[string]map[uint32]string{
				"service1": {
					0: "type=select,bucket=actions=output:1",
					1: "type=select,bucket=actions=output:2",
				},
				"service2": {
					2: "type=select,bucket=actions=output:2",
				},
			},
			key: "service1",
			flows: []string{
				"table=0,priority=100,in_port=1,actions=group:1",
				"table=0,priority=200,in_port=2,actions=group:2",
			},
			expectedFlowCache: map[string][]string{
				"service1": {
					"table=0,priority=100,in_port=1,actions=group:1",
				},
			},
			expectedGroupCache: map[string]map[uint32]string{
				"service1": {
					1: "type=select,bucket=actions=output:2",
				},
				"service2": {
					2: "type=select,bucket=actions=output:2",
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ofm := &openflowManager{
				flowCache:  tc.initialFlowCache,
				groupCache: tc.initialGroupCache,
			}

			ofm.updateFlowCacheEntry(tc.key, tc.flows)

			// Verify flow cache state
			if len(ofm.flowCache) != len(tc.expectedFlowCache) {
				t.Errorf("Expected flow cache to have %d keys but got %d", len(tc.expectedFlowCache), len(ofm.flowCache))
			}

			for key, expectedFlows := range tc.expectedFlowCache {
				actualFlows, ok := ofm.flowCache[key]
				if !ok {
					t.Errorf("Expected key %q to exist in flow cache but it doesn't", key)
					continue
				}
				if len(actualFlows) != len(expectedFlows) {
					t.Errorf("Expected %d flows for key %q but got %d", len(expectedFlows), key, len(actualFlows))
					continue
				}
				for i, expectedFlow := range expectedFlows {
					if actualFlows[i] != expectedFlow {
						t.Errorf("Expected flow %q at index %d but got %q", expectedFlow, i, actualFlows[i])
					}
				}
			}

			// Verify group cache state
			if len(ofm.groupCache) != len(tc.expectedGroupCache) {
				t.Errorf("Expected group cache to have %d keys but got %d", len(tc.expectedGroupCache), len(ofm.groupCache))
			}

			for key, expectedGroups := range tc.expectedGroupCache {
				actualGroups, ok := ofm.groupCache[key]
				if !ok {
					t.Errorf("Expected key %q to exist in group cache but it doesn't", key)
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
