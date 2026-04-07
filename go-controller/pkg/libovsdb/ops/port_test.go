package ops

import (
	"fmt"
	"testing"

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
