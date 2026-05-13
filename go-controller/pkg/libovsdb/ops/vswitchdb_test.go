// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"errors"
	"fmt"
	"testing"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func TestGetOVSPort(t *testing.T) {
	bridgeUUID := buildNamedUUID()
	portUUID := buildNamedUUID()
	ifaceUUID := buildNamedUUID()

	ovs := vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}}
	bridge := vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{portUUID}}
	port := vswitchd.Port{UUID: portUUID, Name: "test-port", Interfaces: []string{ifaceUUID}}
	iface := vswitchd.Interface{UUID: ifaceUUID, Name: "test-port", Type: "internal"}

	tests := []struct {
		desc       string
		portName   string
		expectErr  bool
		initialOvs libovsdbtest.TestSetup
	}{
		{
			desc:      "returns existing port",
			portName:  "test-port",
			expectErr: false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(), bridge.DeepCopy(), port.DeepCopy(), iface.DeepCopy(),
				},
			},
		},
		{
			desc:      "returns error for non-existent port",
			portName:  "no-such-port",
			expectErr: true,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(tt.initialOvs)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			got, err := GetOVSPort(ovsClient, tt.portName)
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("GetOVSPort() error = %v", err))
			}
			if err == nil && tt.expectErr {
				t.Fatal("expected error but got nil")
			}
			if !tt.expectErr && got.Name != tt.portName {
				t.Fatalf("expected port name %q, got %q", tt.portName, got.Name)
			}
		})
	}
}

func TestFindOVSPortsWithPredicate(t *testing.T) {
	bridgeUUID := buildNamedUUID()
	port1UUID := buildNamedUUID()
	port2UUID := buildNamedUUID()
	iface1UUID := buildNamedUUID()
	iface2UUID := buildNamedUUID()

	ovs := vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}}
	bridge := vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{port1UUID, port2UUID}}
	port1 := vswitchd.Port{UUID: port1UUID, Name: "evpn-port", ExternalIDs: map[string]string{"evpn-vtep": "vtep1"}, Interfaces: []string{iface1UUID}}
	port2 := vswitchd.Port{UUID: port2UUID, Name: "other-port", ExternalIDs: map[string]string{"other": "val"}, Interfaces: []string{iface2UUID}}
	iface1 := vswitchd.Interface{UUID: iface1UUID, Name: "evpn-port", Type: "internal"}
	iface2 := vswitchd.Interface{UUID: iface2UUID, Name: "other-port", Type: "internal"}

	tests := []struct {
		desc          string
		predicate     ovsPortPredicate
		expectedCount int
		initialOvs    libovsdbtest.TestSetup
	}{
		{
			desc:          "finds ports matching predicate",
			predicate:     func(p *vswitchd.Port) bool { return p.ExternalIDs["evpn-vtep"] == "vtep1" },
			expectedCount: 1,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(), bridge.DeepCopy(),
					port1.DeepCopy(), port2.DeepCopy(),
					iface1.DeepCopy(), iface2.DeepCopy(),
				},
			},
		},
		{
			desc:          "returns empty for no matches",
			predicate:     func(_ *vswitchd.Port) bool { return false },
			expectedCount: 0,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(), bridge.DeepCopy(),
					port1.DeepCopy(), port2.DeepCopy(),
					iface1.DeepCopy(), iface2.DeepCopy(),
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(tt.initialOvs)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			ports, err := FindOVSPortsWithPredicate(ovsClient, tt.predicate)
			if err != nil {
				t.Fatal(fmt.Errorf("FindOVSPortsWithPredicate() error = %v", err))
			}
			if len(ports) != tt.expectedCount {
				t.Fatalf("expected %d ports, got %d", tt.expectedCount, len(ports))
			}
		})
	}
}

func TestCreateOrUpdatePortWithInterface(t *testing.T) {
	bridgeUUID := buildNamedUUID()

	ovs := vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}}
	bridge := vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"}

	tests := []struct {
		desc              string
		bridgeName        string
		portName          string
		portExternalIDs   map[string]string
		expectErr         bool
		initialOvs        libovsdbtest.TestSetup
		verifyExternalIDs map[string]string
	}{
		{
			desc:            "creates new port and interface",
			bridgeName:      "br-int",
			portName:        "new-port",
			portExternalIDs: map[string]string{"evpn-vtep": "vtep1"},
			expectErr:       false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(), bridge.DeepCopy(),
				},
			},
			verifyExternalIDs: map[string]string{"evpn-vtep": "vtep1"},
		},
		{
			desc:            "updates existing port external IDs",
			bridgeName:      "br-int",
			portName:        "upd-port",
			portExternalIDs: map[string]string{"k": "v2"},
			expectErr:       false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(), bridge.DeepCopy(),
				},
			},
			verifyExternalIDs: map[string]string{"k": "v2"},
		},
		{
			desc:            "fails for non-existent bridge",
			bridgeName:      "no-bridge",
			portName:        "port",
			portExternalIDs: nil,
			expectErr:       true,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(), bridge.DeepCopy(),
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(tt.initialOvs)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			// For update test, create the port first with initial external IDs
			if tt.desc == "updates existing port external IDs" {
				err = CreateOrUpdatePortWithInterface(ovsClient, tt.bridgeName, tt.portName, map[string]string{"k": "v1"}, nil)
				if err != nil {
					t.Fatalf("setup create failed: %v", err)
				}
			}

			err = CreateOrUpdatePortWithInterface(ovsClient, tt.bridgeName, tt.portName, tt.portExternalIDs, nil)
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("CreateOrUpdatePortWithInterface() error = %v", err))
			}
			if err == nil && tt.expectErr {
				t.Fatal("expected error but got nil")
			}

			if !tt.expectErr {
				got, err := GetOVSPort(ovsClient, tt.portName)
				if err != nil {
					t.Fatalf("port not found after create: %v", err)
				}
				for k, v := range tt.verifyExternalIDs {
					if got.ExternalIDs[k] != v {
						t.Fatalf("expected external ID %q=%q, got %v", k, v, got.ExternalIDs)
					}
				}
			}
		})
	}
}

func TestDeletePortWithInterfaces(t *testing.T) {
	bridgeUUID := buildNamedUUID()
	portUUID := buildNamedUUID()
	ifaceUUID := buildNamedUUID()

	ovs := vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}}
	bridge := vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{portUUID}}
	port := vswitchd.Port{UUID: portUUID, Name: "del-port", Interfaces: []string{ifaceUUID}}
	iface := vswitchd.Interface{UUID: ifaceUUID, Name: "del-port", Type: "internal"}

	tests := []struct {
		desc        string
		portName    string
		expectErr   bool
		initialOvs  libovsdbtest.TestSetup
		expectedOvs libovsdbtest.TestSetup
	}{
		{
			desc:      "deletes existing port and interface",
			portName:  "del-port",
			expectErr: false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(), bridge.DeepCopy(), port.DeepCopy(), iface.DeepCopy(),
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(),
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
				},
			},
		},
		{
			desc:      "is idempotent for non-existent port",
			portName:  "no-such-port",
			expectErr: false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(tt.initialOvs)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			err = DeletePortWithInterfaces(ovsClient, "br-int", tt.portName)
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("DeletePortWithInterfaces() error = %v", err))
			}
			if err == nil && tt.expectErr {
				t.Fatal("expected error but got nil")
			}

			matcher := libovsdbtest.HaveData(tt.expectedOvs.OVSData)
			success, err := matcher.Match(ovsClient)
			if !success {
				t.Fatal(fmt.Errorf("test: \"%s\" didn't match expected with actual, err: %v", tt.desc, matcher.FailureMessage(ovsClient)))
			}
			if err != nil {
				t.Fatal(fmt.Errorf("test: \"%s\" encountered error: %v", tt.desc, err))
			}
		})
	}
}

func TestDeleteBridge(t *testing.T) {
	otherBridgeUUID := buildNamedUUID()
	bridgeUUID := buildNamedUUID()
	port1UUID := buildNamedUUID()
	port2UUID := buildNamedUUID()
	iface1UUID := buildNamedUUID()
	iface2UUID := buildNamedUUID()

	otherBridge := vswitchd.Bridge{UUID: otherBridgeUUID, Name: "br-other"}
	ovs := vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{otherBridgeUUID, bridgeUUID}}
	bridge := vswitchd.Bridge{UUID: bridgeUUID, Name: "br-doomed", Ports: []string{port1UUID, port2UUID}}
	port1 := vswitchd.Port{UUID: port1UUID, Name: "p1", Interfaces: []string{iface1UUID}}
	port2 := vswitchd.Port{UUID: port2UUID, Name: "p2", Interfaces: []string{iface2UUID}}
	iface1 := vswitchd.Interface{UUID: iface1UUID, Name: "p1", Type: "internal"}
	iface2 := vswitchd.Interface{UUID: iface2UUID, Name: "p2", Type: "internal"}

	tests := []struct {
		desc        string
		bridgeName  string
		expectErr   bool
		initialOvs  libovsdbtest.TestSetup
		expectedOvs libovsdbtest.TestSetup
	}{
		{
			desc:       "deletes bridge with all ports and interfaces",
			bridgeName: "br-doomed",
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(), otherBridge.DeepCopy(), bridge.DeepCopy(),
					port1.DeepCopy(), port2.DeepCopy(),
					iface1.DeepCopy(), iface2.DeepCopy(),
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{otherBridgeUUID}},
					otherBridge.DeepCopy(),
				},
			},
		},
		{
			desc:       "is idempotent for a non-existent bridge",
			bridgeName: "no-such-bridge",
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{otherBridgeUUID}},
					otherBridge.DeepCopy(),
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{otherBridgeUUID}},
					otherBridge.DeepCopy(),
				},
			},
		},
		{
			desc:       "deletes empty bridge (no ports)",
			bridgeName: "br-empty",
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-empty"},
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs"},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(tt.initialOvs)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			err = DeleteBridge(ovsClient, tt.bridgeName)
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("DeleteBridge() error = %v", err))
			}
			if err == nil && tt.expectErr {
				t.Fatal("expected error but got nil")
			}

			matcher := libovsdbtest.HaveData(tt.expectedOvs.OVSData)
			success, err := matcher.Match(ovsClient)
			if !success {
				t.Fatal(fmt.Errorf("test: \"%s\" didn't match expected with actual, err: %v", tt.desc, matcher.FailureMessage(ovsClient)))
			}
			if err != nil {
				t.Fatal(fmt.Errorf("test: \"%s\" encountered error: %v", tt.desc, err))
			}
		})
	}
}

func TestUpdateOpenvSwitchExternalIDs(t *testing.T) {
	tests := []struct {
		desc                string
		initialExternalIDs  map[string]string
		update              map[string]string
		expectedExternalIDs map[string]string
		setupNoOpenvSwitch  bool
		expectErrIs         error
	}{
		{
			desc:                "sets a new key on empty external_ids",
			initialExternalIDs:  nil,
			update:              map[string]string{"ovn-encap-ip": "10.0.0.1"},
			expectedExternalIDs: map[string]string{"ovn-encap-ip": "10.0.0.1"},
		},
		{
			desc:                "overwrites an existing key, preserves unrelated keys",
			initialExternalIDs:  map[string]string{"ovn-encap-ip": "10.0.0.1", "system-id": "node-a"},
			update:              map[string]string{"ovn-encap-ip": "10.0.0.2"},
			expectedExternalIDs: map[string]string{"ovn-encap-ip": "10.0.0.2", "system-id": "node-a"},
		},
		{
			desc:                "no-op for empty update",
			initialExternalIDs:  map[string]string{"system-id": "node-a"},
			update:              nil,
			expectedExternalIDs: map[string]string{"system-id": "node-a"},
		},
		{
			desc:               "returns ErrNotFound when no Open_vSwitch row exists",
			update:             map[string]string{"ovn-encap-ip": "10.0.0.1"},
			setupNoOpenvSwitch: true,
			expectErrIs:        libovsdbclient.ErrNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			setup := libovsdbtest.TestSetup{}
			if !tt.setupNoOpenvSwitch {
				setup.OVSData = []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", ExternalIDs: tt.initialExternalIDs},
				}
			}

			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(setup)
			if err != nil {
				t.Fatalf("failed to set up test harness: %v", err)
			}
			t.Cleanup(cleanup.Cleanup)

			err = UpdateOpenvSwitchExternalIDs(ovsClient, tt.update)
			if tt.expectErrIs != nil {
				if !errors.Is(err, tt.expectErrIs) {
					t.Fatalf("expected error %v, got %v", tt.expectErrIs, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("UpdateOpenvSwitchExternalIDs() error = %v", err)
			}

			expected := []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", ExternalIDs: tt.expectedExternalIDs},
			}
			matcher := libovsdbtest.HaveData(expected)
			success, err := matcher.Match(ovsClient)
			if !success {
				t.Fatalf("post-condition mismatch: %v", matcher.FailureMessage(ovsClient))
			}
			if err != nil {
				t.Fatalf("matcher encountered error: %v", err)
			}
		})
	}
}

func TestRemoveOpenvSwitchExternalIDs(t *testing.T) {
	tests := []struct {
		desc                string
		initialExternalIDs  map[string]string
		removeKeys          []string
		expectedExternalIDs map[string]string
		setupNoOpenvSwitch  bool
	}{
		{
			desc:                "removes a single existing key, preserves unrelated keys",
			initialExternalIDs:  map[string]string{"ovn-bridge-mappings": "physnet1:br1", "system-id": "node-a"},
			removeKeys:          []string{"ovn-bridge-mappings"},
			expectedExternalIDs: map[string]string{"system-id": "node-a"},
		},
		{
			desc:                "removes multiple keys",
			initialExternalIDs:  map[string]string{"a": "1", "b": "2", "c": "3"},
			removeKeys:          []string{"a", "c"},
			expectedExternalIDs: map[string]string{"b": "2"},
		},
		{
			desc:                "removing a non-existent key is a no-op",
			initialExternalIDs:  map[string]string{"system-id": "node-a"},
			removeKeys:          []string{"ovn-bridge-mappings"},
			expectedExternalIDs: map[string]string{"system-id": "node-a"},
		},
		{
			desc:                "no-op for empty key list",
			initialExternalIDs:  map[string]string{"system-id": "node-a"},
			removeKeys:          nil,
			expectedExternalIDs: map[string]string{"system-id": "node-a"},
		},
		{
			desc:               "missing Open_vSwitch row is not an error (matches --if-exists)",
			removeKeys:         []string{"ovn-bridge-mappings"},
			setupNoOpenvSwitch: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			setup := libovsdbtest.TestSetup{}
			if !tt.setupNoOpenvSwitch {
				setup.OVSData = []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", ExternalIDs: tt.initialExternalIDs},
				}
			}

			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(setup)
			if err != nil {
				t.Fatalf("failed to set up test harness: %v", err)
			}
			t.Cleanup(cleanup.Cleanup)

			if err := RemoveOpenvSwitchExternalIDs(ovsClient, tt.removeKeys...); err != nil {
				t.Fatalf("RemoveOpenvSwitchExternalIDs() error = %v", err)
			}

			if tt.setupNoOpenvSwitch {
				return
			}
			expected := []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", ExternalIDs: tt.expectedExternalIDs},
			}
			matcher := libovsdbtest.HaveData(expected)
			success, err := matcher.Match(ovsClient)
			if !success {
				t.Fatalf("post-condition mismatch: %v", matcher.FailureMessage(ovsClient))
			}
			if err != nil {
				t.Fatalf("matcher encountered error: %v", err)
			}
		})
	}
}
