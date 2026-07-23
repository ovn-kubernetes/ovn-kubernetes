// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"testing"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

func TestCreateOrAddPortsToPortGroupOpsCreatesMissingPortGroup(t *testing.T) {
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

	pg := &nbdb.PortGroup{
		UUID:        "pg-UUID",
		Name:        "pg",
		ExternalIDs: map[string]string{"owner": "test"},
		Ports:       []string{port.UUID},
	}
	ops, err := CreateOrAddPortsToPortGroupOps(nbClient, nil, pg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = TransactAndCheck(nbClient, ops); err != nil {
		t.Fatal(err)
	}

	matcher := libovsdbtest.HaveData(port, sw, pg)
	if success, err := matcher.Match(nbClient); err != nil || !success {
		t.Fatalf("unexpected database state: success=%t err=%v\n%s", success, err, matcher.FailureMessage(nbClient))
	}
}

func TestCreateOrAddPortsToPortGroupOpsMutatesExistingPortGroup(t *testing.T) {
	port1 := &nbdb.LogicalSwitchPort{
		UUID: "port-1-UUID",
		Name: "port-1",
	}
	port2 := &nbdb.LogicalSwitchPort{
		UUID: "port-2-UUID",
		Name: "port-2",
	}
	sw := &nbdb.LogicalSwitch{
		UUID:  "switch-UUID",
		Name:  "switch",
		Ports: []string{port1.UUID, port2.UUID},
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
		NBData: []libovsdbtest.TestData{port1, port2, sw, acl, existingPG, decoyPG},
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
	sw, err = GetLogicalSwitch(nbClient, &nbdb.LogicalSwitch{Name: sw.Name})
	if err != nil {
		t.Fatal(err)
	}

	updatePG := &nbdb.PortGroup{
		Name:        existingPG.Name,
		ExternalIDs: map[string]string{"owner": "replacement"},
		Ports:       []string{port2.UUID},
	}
	expectedPG := &nbdb.PortGroup{
		UUID:        existingPG.UUID,
		Name:        existingPG.Name,
		ExternalIDs: existingPG.ExternalIDs,
		Ports:       []string{port1.UUID, port2.UUID},
		ACLs:        existingPG.ACLs,
	}
	ops, err := CreateOrAddPortsToPortGroupOps(nbClient, nil, updatePG)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = TransactAndCheck(nbClient, ops); err != nil {
		t.Fatal(err)
	}

	matcher := libovsdbtest.HaveData(port1, port2, sw, acl, expectedPG, decoyPG)
	if success, err := matcher.Match(nbClient); err != nil || !success {
		t.Fatalf("unexpected database state: success=%t err=%v\n%s", success, err, matcher.FailureMessage(nbClient))
	}
}
