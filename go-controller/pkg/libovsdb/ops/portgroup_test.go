// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"errors"
	"reflect"
	"testing"

	"github.com/onsi/gomega"

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
	// Another pod joins after the lookup; the UUID guard must allow concurrent
	// membership changes without overwriting them.
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

func TestAddPortsToPortGroupOpsRejectsDeletionBeforeTransaction(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "deleted"
		if replace {
			name = "replaced"
		}
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			pg := &nbdb.PortGroup{Name: "namespace", UUID: "pg-UUID"}
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{pg},
			}, nil)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(cleanup.Cleanup)

			// Include a new switch/port in the transaction: failure must roll back
			// port programming as well as reject missing namespace membership.
			port := &nbdb.LogicalSwitchPort{Name: "pod", UUID: "new-pod-port"}
			sw := &nbdb.LogicalSwitch{Name: "node", Ports: []string{port.UUID}}
			ops, err := nbClient.Create(port)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			switchOps, err := nbClient.Create(sw)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			ops = append(ops, switchOps...)
			ops, err = AddPortsToPortGroupOps(nbClient, ops, pg.Name, port.UUID)
			g.Expect(err).NotTo(gomega.HaveOccurred())

			g.Expect(DeletePortGroups(nbClient, pg.Name)).To(gomega.Succeed())
			g.Eventually(func() error {
				_, err := GetPortGroup(nbClient, &nbdb.PortGroup{Name: pg.Name})
				return err
			}).Should(gomega.MatchError(libovsdbclient.ErrNotFound))
			expected := []libovsdbtest.TestData{}
			if replace {
				replacement := &nbdb.PortGroup{Name: pg.Name, ExternalIDs: map[string]string{"owner": "replacement"}}
				g.Expect(CreatePortGroup(nbClient, replacement)).To(gomega.Succeed())
				expected = append(expected, replacement)
			}

			_, err = TransactAndCheck(nbClient, ops)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Eventually(nbClient).Should(libovsdbtest.HaveData(expected))
		})
	}
}

func TestAddPortsToPortGroupOpsRollsBackOtherMembership(t *testing.T) {
	g := gomega.NewWithT(t)
	port := &nbdb.LogicalSwitchPort{Name: "pod", UUID: "pod-UUID"}
	sw := &nbdb.LogicalSwitch{Name: "node", UUID: "node-UUID", Ports: []string{port.UUID}}
	policyPG := &nbdb.PortGroup{Name: "policy", UUID: "policy-UUID"}
	denyPG := &nbdb.PortGroup{Name: "default-deny", UUID: "deny-UUID"}
	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{port, sw, policyPG, denyPG},
	}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(cleanup.Cleanup)
	port, err = GetLogicalSwitchPort(nbClient, &nbdb.LogicalSwitchPort{Name: port.Name})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	sw, err = GetLogicalSwitch(nbClient, &nbdb.LogicalSwitch{Name: sw.Name})
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Policy callers combine membership in multiple groups. Losing one group
	// must not allow the other group's membership to commit on its own.
	ops, err := AddPortsToPortGroupOps(nbClient, nil, policyPG.Name, port.UUID)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	ops, err = AddPortsToPortGroupOps(nbClient, ops, denyPG.Name, port.UUID)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(DeletePortGroups(nbClient, denyPG.Name)).To(gomega.Succeed())

	_, err = TransactAndCheck(nbClient, ops)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Eventually(nbClient).Should(libovsdbtest.HaveData(port, sw, policyPG))

	// Cleanup remains idempotent even when the group has already disappeared.
	g.Expect(DeletePortsFromPortGroup(nbClient, denyPG.Name, port.UUID)).To(gomega.Succeed())
}
