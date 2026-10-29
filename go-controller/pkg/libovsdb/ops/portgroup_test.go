// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"errors"
	"reflect"
	"testing"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

func TestAddPortsToPortGroupOpsRejectsMissingPortGroup(t *testing.T) {
	port := &nbdb.LogicalSwitchPort{
		UUID: "port-1-UUID",
		Name: "port-1",
	}
	sw := &nbdb.LogicalSwitch{
		UUID:  "switch-UUID",
		Name:  "switch",
		Ports: []string{port.UUID},
	}
	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{port, sw},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup.Cleanup)

	port, err = GetLogicalSwitchPort(nbClient, &nbdb.LogicalSwitchPort{Name: port.Name})
	if err != nil {
		t.Fatal(err)
	}
	sw, err = GetLogicalSwitch(nbClient, &nbdb.LogicalSwitch{Name: sw.Name})
	if err != nil {
		t.Fatal(err)
	}

	_, err = AddPortsToPortGroupOps(nbClient, nil, "pg", port.UUID)
	if !errors.Is(err, libovsdbclient.ErrNotFound) {
		t.Fatalf("expected missing dependency, got %v", err)
	}
	if err = AddPortsToPortGroup(nbClient, "pg", port.UUID); !errors.Is(err, libovsdbclient.ErrNotFound) {
		t.Fatalf("expected wrapper to report missing dependency, got %v", err)
	}

	matcher := libovsdbtest.HaveData(port, sw)
	if success, err := matcher.Match(nbClient); err != nil || !success {
		t.Fatalf("unexpected database state: success=%t err=%v\n%s", success, err, matcher.FailureMessage(nbClient))
	}
}

func TestAddPortsToPortGroupOpsMutatesExistingPortGroup(t *testing.T) {
	port1 := &nbdb.LogicalSwitchPort{
		UUID: "port-1-UUID",
		Name: "port-1",
	}
	port2 := &nbdb.LogicalSwitchPort{
		UUID: "port-2-UUID",
		Name: "port-2",
	}
	port3 := &nbdb.LogicalSwitchPort{UUID: "port-3-UUID", Name: "port-3"}
	sw := &nbdb.LogicalSwitch{
		UUID:  "switch-UUID",
		Name:  "switch",
		Ports: []string{port1.UUID, port2.UUID, port3.UUID},
	}
	acl := &nbdb.ACL{
		UUID:      "acl-UUID",
		Direction: nbdb.ACLDirectionToLport,
		Priority:  1001,
		Match:     "outport == @pg",
		Action:    nbdb.ACLActionAllow,
	}
	existingPG := &nbdb.PortGroup{
		UUID:        "pg-UUID",
		Name:        "pg",
		ExternalIDs: map[string]string{"owner": "original"},
		Ports:       []string{port1.UUID},
		ACLs:        []string{acl.UUID},
	}
	decoyPG := &nbdb.PortGroup{
		UUID:        "decoy-pg-UUID",
		Name:        "decoy-pg",
		ExternalIDs: map[string]string{"owner": "replacement"},
	}
	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{port1, port2, port3, sw, acl, existingPG, decoyPG},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup.Cleanup)

	port1, err = GetLogicalSwitchPort(nbClient, &nbdb.LogicalSwitchPort{Name: port1.Name})
	if err != nil {
		t.Fatal(err)
	}
	port2, err = GetLogicalSwitchPort(nbClient, &nbdb.LogicalSwitchPort{Name: port2.Name})
	if err != nil {
		t.Fatal(err)
	}
	port3, err = GetLogicalSwitchPort(nbClient, &nbdb.LogicalSwitchPort{Name: port3.Name})
	if err != nil {
		t.Fatal(err)
	}
	sw, err = GetLogicalSwitch(nbClient, &nbdb.LogicalSwitch{Name: sw.Name})
	if err != nil {
		t.Fatal(err)
	}

	expectedPG := &nbdb.PortGroup{
		UUID:        existingPG.UUID,
		Name:        existingPG.Name,
		ExternalIDs: existingPG.ExternalIDs,
		Ports:       []string{port1.UUID, port2.UUID, port3.UUID},
		ACLs:        existingPG.ACLs,
	}
	ops, err := AddPortsToPortGroupOps(nbClient, nil, existingPG.Name, port2.UUID)
	if err != nil {
		t.Fatal(err)
	}
	// Another pod joins after the lookup; preserve its membership too.
	if err = AddPortsToPortGroup(nbClient, existingPG.Name, port3.UUID); err != nil {
		t.Fatal(err)
	}
	if _, err = TransactAndCheck(nbClient, ops); err != nil {
		t.Fatal(err)
	}

	matcher := libovsdbtest.HaveData(port1, port2, port3, sw, acl, expectedPG, decoyPG)
	if success, err := matcher.Match(nbClient); err != nil || !success {
		t.Fatalf("unexpected database state: success=%t err=%v\n%s", success, err, matcher.FailureMessage(nbClient))
	}
}

func TestAddPortsToPortGroupOpsNoop(t *testing.T) {
	for _, ports := range [][]string{nil, {}} {
		ops := []ovsdb.Operation{{Op: ovsdb.OperationComment}}
		got, err := AddPortsToPortGroupOps(nil, ops, "empty", ports...)
		if err != nil || !reflect.DeepEqual(got, ops) {
			t.Fatalf("expected unchanged operations, got %v, %v", got, err)
		}
		if err = AddPortsToPortGroup(nil, "empty", ports...); err != nil {
			t.Fatalf("expected wrapper to skip empty addition, got %v", err)
		}
	}
}
