// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"testing"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestOpenFlowManagerDeletesGroupCacheWithFlowCache(t *testing.T) {
	ofm := &openflowManager{
		defaultBridge: newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig()),
		uplinkBridges: map[string]*openflowBridge{},
	}
	key := "NodePort_namespace1_service1_tcp_31111"

	ofm.updateFlowCacheEntry(key, []string{"cookie=0x123, priority=110, actions=group:100"})
	ofm.updateGroupCacheEntry(key, []string{"group_id=100,type=select,bucket=actions=output:LOCAL"})

	ofm.deleteFlowsByKey(key)

	if flows := ofm.getFlowsByKey(key); flows != nil {
		t.Fatalf("expected flow cache entry to be deleted, got %#v", flows)
	}
	if groups := ofm.getGroupsByKey(key); groups != nil {
		t.Fatalf("expected group cache entry to be deleted, got %#v", groups)
	}
}

func TestOpenFlowManagerCleansUnusedUplinkBridgeFlows(t *testing.T) {
	fexec := ovntest.NewFakeExec()
	if err := util.SetExec(fexec); err != nil {
		t.Fatalf("failed to set fake exec: %v", err)
	}
	t.Cleanup(util.ResetRunner)
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: "ovs-ofctl -O OpenFlow13 --bundle replace-flows breth0 -",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: "ovs-ofctl -O OpenFlow13 del-groups breth0 group_id=100",
	})

	bridge := newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig())
	bridge.updateFlowCacheEntry("stale", []string{"table=0,priority=100,actions=drop"})
	bridge.updateGroupCacheEntry("stale", []string{"group_id=100,type=select,bucket=actions=drop"})
	bridge.installedGroups = map[string]struct{}{"100": {}}
	ofm := &openflowManager{
		defaultBridge: newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig()),
		uplinkBridges: map[string]*openflowBridge{
			"breth0": bridge,
		},
	}

	if err := ofm.delNetwork(&util.DefaultNetInfo{}, "breth0"); err != nil {
		t.Fatalf("failed to delete network from uplink bridge: %v", err)
	}

	if len(ofm.uplinkBridges) != 0 {
		t.Fatalf("expected unused uplink bridge to be removed, got %d entries", len(ofm.uplinkBridges))
	}
	if flows := bridge.getFlowsByKey("NORMAL"); len(flows) != 1 {
		t.Fatalf("expected NORMAL flow to remain, got %#v", flows)
	}
	if flows := bridge.getFlowsByKey("stale"); flows != nil {
		t.Fatalf("expected stale flow cache entry to be deleted, got %#v", flows)
	}
	if groups := bridge.getGroupsByKey("stale"); groups != nil {
		t.Fatalf("expected stale group cache entry to be deleted, got %#v", groups)
	}
	if len(bridge.installedGroups) != 0 {
		t.Fatalf("expected stale installed groups to be deleted, got %#v", bridge.installedGroups)
	}
	if bridge.GetNetConfigLen() != 0 {
		t.Fatalf("expected %s network config to be removed", types.DefaultNetworkName)
	}
	if !fexec.CalledMatchesExpected() {
		t.Fatalf("expected cleanup to replace flows on the unused uplink bridge")
	}
}

func TestOpenFlowManagerDefaultTargetUsesDefaultBridgeSet(t *testing.T) {
	defaultBridge := bridgeconfig.TestDefaultBridgeConfig()
	externalGatewayBridge := bridgeconfig.TestDefaultBridgeConfig()
	defaultBridge.DelNetworkConfig(&util.DefaultNetInfo{})
	externalGatewayBridge.DelNetworkConfig(&util.DefaultNetInfo{})
	ofm := &openflowManager{
		defaultBridge:         newOpenflowBridge(defaultBridge),
		externalGatewayBridge: newOpenflowBridge(externalGatewayBridge),
		uplinkBridges:         map[string]*openflowBridge{},
	}

	if err := ofm.addNetwork(defaultOpenFlowBridgeTarget(), &util.DefaultNetInfo{}, nil, nil, 0, 0, nil, nil); err != nil {
		t.Fatalf("failed to add network to default bridge set: %v", err)
	}

	if netConfig := ofm.defaultBridge.GetNetworkConfig(types.DefaultNetworkName); netConfig == nil {
		t.Fatalf("expected default bridge network config to be added")
	}
	if netConfig := ofm.externalGatewayBridge.GetNetworkConfig(types.DefaultNetworkName); netConfig == nil {
		t.Fatalf("expected external gateway bridge network config to be added")
	}
	if len(ofm.uplinkBridges) != 0 {
		t.Fatalf("expected default bridge target to avoid uplink bridge cache, got %d entries", len(ofm.uplinkBridges))
	}

	if err := ofm.delNetwork(&util.DefaultNetInfo{}, defaultOpenFlowBridgeSetName); err != nil {
		t.Fatalf("failed to delete network from default bridge set: %v", err)
	}
	if netConfig := ofm.defaultBridge.GetNetworkConfig(types.DefaultNetworkName); netConfig != nil {
		t.Fatalf("expected default bridge network config to be removed")
	}
	if netConfig := ofm.externalGatewayBridge.GetNetworkConfig(types.DefaultNetworkName); netConfig != nil {
		t.Fatalf("expected external gateway bridge network config to be removed")
	}
}

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
