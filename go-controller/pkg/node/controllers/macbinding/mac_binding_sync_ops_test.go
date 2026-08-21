// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package macbinding

import (
	"fmt"
	"strings"
	"testing"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

func newSBTestHarnessWithMACBinding(t *testing.T, setup libovsdbtest.TestSetup) (libovsdbclient.Client, *libovsdbtest.TestOvsdbServer) {
	t.Helper()
	// NewSBTestHarness returns the production SB client, which monitors the
	// MAC_Binding table with a restricted column set (logical_port, mac, ip)
	// and therefore never caches datapath/timestamp. We must not add a second
	// monitor for the same table: that makes the server re-send existing rows
	// and corrupts the client cache with
	// "cache inconsistent: cannot create row ... as it already exists".
	// Instead, assert against the server database (see TestOvsdbServer.GetData),
	// which holds every column.
	sbClient, ctx, err := libovsdbtest.NewSBTestHarness(setup, nil)
	if err != nil {
		t.Fatalf("failed to set up test harness: %v", err)
	}
	t.Cleanup(ctx.Cleanup)
	return sbClient, ctx.SBServer
}

// expectSBData asserts that the MAC_Binding and Datapath_Binding tables in the
// server database consist exactly of expected (UUIDs ignored). Reading from the
// server rather than the client cache is required because the production SB
// client does not monitor the MAC_Binding datapath/timestamp columns.
func expectSBData(t *testing.T, sbServer *libovsdbtest.TestOvsdbServer, expected []libovsdbtest.TestData) {
	t.Helper()
	actual, err := sbServer.GetData(sbdb.MACBindingTable, sbdb.DatapathBindingTable)
	if err != nil {
		t.Fatalf("failed to read server data: %v", err)
	}
	matcher := libovsdbtest.ConsistOfIgnoringUUIDs(expected)
	match, err := matcher.Match(actual)
	if !match || err != nil {
		t.Fatal(fmt.Errorf("data mismatch: %s", matcher.FailureMessage(actual)))
	}
}

func testSBMACBinder(t *testing.T, newBinder func(libovsdbclient.Client) macBindingSyncOps, ip string) {
	t.Helper()

	const dpUUID1 = "10000000-0000-0000-0000-000000000001"
	const dpUUID2 = "10000000-0000-0000-0000-000000000002"

	datapaths := []libovsdbtest.TestData{
		&sbdb.DatapathBinding{UUID: dpUUID1, TunnelKey: 1},
		&sbdb.DatapathBinding{UUID: dpUUID2, TunnelKey: 2},
	}

	t.Run("AddMACBinding single port", func(t *testing.T) {
		sbClient, sbServer := newSBTestHarnessWithMACBinding(t, libovsdbtest.TestSetup{
			SBData: datapaths,
		})

		binder := newBinder(sbClient)
		ports := []portInfo{{LogicalPort: "lsp1", DatapathUUID: dpUUID1}}

		if err := binder.AddMACBinding(ip, "00:00:00:00:00:01", 1, ports); err != nil {
			t.Fatalf("AddMACBinding() error: %v", err)
		}

		expectSBData(t, sbServer, []libovsdbtest.TestData{
			&sbdb.DatapathBinding{UUID: dpUUID1, TunnelKey: 1},
			&sbdb.DatapathBinding{UUID: dpUUID2, TunnelKey: 2},
			&sbdb.MACBinding{
				IP:          ip,
				MAC:         "00:00:00:00:00:01",
				LogicalPort: "lsp1",
				Datapath:    dpUUID1,
				Timestamp:   1,
			},
		})
	})

	t.Run("AddMACBinding multiple ports", func(t *testing.T) {
		sbClient, sbServer := newSBTestHarnessWithMACBinding(t, libovsdbtest.TestSetup{
			SBData: datapaths,
		})

		binder := newBinder(sbClient)
		ports := []portInfo{
			{LogicalPort: "lsp1", DatapathUUID: dpUUID1},
			{LogicalPort: "lsp2", DatapathUUID: dpUUID2},
		}

		if err := binder.AddMACBinding(ip, "00:00:00:00:00:01", 1, ports); err != nil {
			t.Fatalf("AddMACBinding() error: %v", err)
		}

		expectSBData(t, sbServer, []libovsdbtest.TestData{
			&sbdb.DatapathBinding{UUID: dpUUID1, TunnelKey: 1},
			&sbdb.DatapathBinding{UUID: dpUUID2, TunnelKey: 2},
			&sbdb.MACBinding{
				IP:          ip,
				MAC:         "00:00:00:00:00:01",
				LogicalPort: "lsp1",
				Datapath:    dpUUID1,
				Timestamp:   1,
			},
			&sbdb.MACBinding{
				IP:          ip,
				MAC:         "00:00:00:00:00:01",
				LogicalPort: "lsp2",
				Datapath:    dpUUID2,
				Timestamp:   1,
			},
		})
	})

	t.Run("Update replaces MAC binding", func(t *testing.T) {
		initialData := libovsdbtest.TestSetup{
			SBData: append(datapaths,
				&sbdb.MACBinding{
					UUID:        "mb-uuid-1",
					IP:          ip,
					MAC:         "00:00:00:00:00:01",
					LogicalPort: "lsp1",
					Datapath:    dpUUID1,
					Timestamp:   1,
				},
				&sbdb.MACBinding{
					UUID:        "mb-uuid-2",
					IP:          ip,
					MAC:         "00:00:00:00:00:01",
					LogicalPort: "lsp2",
					Datapath:    dpUUID2,
					Timestamp:   1,
				},
			),
		}

		sbClient, sbServer := newSBTestHarnessWithMACBinding(t, initialData)

		binder := newBinder(sbClient)
		ports := []portInfo{
			{LogicalPort: "lsp1", DatapathUUID: dpUUID1},
			{LogicalPort: "lsp2", DatapathUUID: dpUUID2},
		}

		if err := binder.UpdateMACBinding(ip, "00:00:00:00:00:02", 2, ports); err != nil {
			t.Fatalf("Update() error: %v", err)
		}

		expectSBData(t, sbServer, []libovsdbtest.TestData{
			&sbdb.DatapathBinding{UUID: dpUUID1, TunnelKey: 1},
			&sbdb.DatapathBinding{UUID: dpUUID2, TunnelKey: 2},
			&sbdb.MACBinding{
				IP:          ip,
				MAC:         "00:00:00:00:00:02",
				LogicalPort: "lsp1",
				Datapath:    dpUUID1,
				Timestamp:   2,
			},
			&sbdb.MACBinding{
				IP:          ip,
				MAC:         "00:00:00:00:00:02",
				LogicalPort: "lsp2",
				Datapath:    dpUUID2,
				Timestamp:   2,
			},
		})
	})
	t.Run("Delete and add only specified port bindings", func(t *testing.T) {
		initialData := libovsdbtest.TestSetup{
			SBData: append(datapaths,
				&sbdb.MACBinding{
					UUID:        "mb-uuid-1",
					IP:          ip,
					MAC:         "00:00:00:00:00:01",
					LogicalPort: "lsp1",
					Datapath:    dpUUID1,
					Timestamp:   1,
				},
				&sbdb.MACBinding{
					UUID:        "mb-uuid-2",
					IP:          ip,
					MAC:         "00:00:00:00:00:01",
					LogicalPort: "lsp2",
					Datapath:    dpUUID2,
					Timestamp:   1,
				},
			),
		}

		sbClient, sbServer := newSBTestHarnessWithMACBinding(t, initialData)

		binder := newBinder(sbClient)
		ports := []portInfo{
			{LogicalPort: "lsp1", DatapathUUID: dpUUID1},
		}

		if err := binder.DeleteAndAddMACBinding(ip, "00:00:00:00:00:01", 2, ports); err != nil {
			t.Fatalf("Delete() error: %v", err)
		}

		expectSBData(t, sbServer, []libovsdbtest.TestData{
			&sbdb.DatapathBinding{UUID: dpUUID1, TunnelKey: 1},
			&sbdb.DatapathBinding{UUID: dpUUID2, TunnelKey: 2},
			&sbdb.MACBinding{
				UUID:        "mb-uuid-1",
				IP:          ip,
				MAC:         "00:00:00:00:00:01",
				LogicalPort: "lsp1",
				Datapath:    dpUUID1,
				Timestamp:   2,
			},
			&sbdb.MACBinding{
				UUID:        "mb-uuid-2",
				IP:          ip,
				MAC:         "00:00:00:00:00:01",
				LogicalPort: "lsp2",
				Datapath:    dpUUID2,
				Timestamp:   1,
			},
		})
	})
}

func TestIPv4SBMACBinder(t *testing.T) {
	testSBMACBinder(t, func(client libovsdbclient.Client) macBindingSyncOps {
		return newMACBindingSyncOps(client, nil)
	}, "192.168.1.10")
}

func TestIPv6SBMACBinder(t *testing.T) {
	testSBMACBinder(t, func(client libovsdbclient.Client) macBindingSyncOps {
		return newMACBindingSyncOps(client, nil)
	}, "fd00::1")
}

type mockFlowManager struct {
	updateCalls []flowManagerCall
	deleteCalls []flowManagerCall
	syncCount   int
}

type flowManagerCall struct {
	key   string
	flows []string
}

func (m *mockFlowManager) UpdateFlowCacheEntry(key string, flows []string) {
	m.updateCalls = append(m.updateCalls, flowManagerCall{key: key, flows: flows})
}

func (m *mockFlowManager) DeleteFlowsByKey(key string) {
	m.deleteCalls = append(m.deleteCalls, flowManagerCall{key: key})
}

func (m *mockFlowManager) RequestFlowSync() {
	m.syncCount++
}

func (m *mockFlowManager) Flowskeys() []string {
	return nil
}

func TestEnsureARPFlow(t *testing.T) {
	mock := &mockFlowManager{}
	syncer := newMACBindingSyncOps(nil, mock)

	if err := syncer.EnsureARPFlow("192.168.1.10", "0a:58:c0:a8:01:0a"); err != nil {
		t.Fatalf("EnsureARPFlow() error: %v", err)
	}

	if len(mock.updateCalls) != 1 {
		t.Fatalf("expected 1 UpdateExBridgeFlowCacheEntry call, got %d", len(mock.updateCalls))
	}

	call := mock.updateCalls[0]
	if call.key != "MAC_BINDING_ARP_192.168.1.10" {
		t.Errorf("key = %q, want %q", call.key, "MAC_BINDING_ARP_192.168.1.10")
	}
	if len(call.flows) != 1 {
		t.Fatalf("expected 1 flow, got %d", len(call.flows))
	}

	flow := call.flows[0]
	for _, want := range []string{
		"cookie=0x0306",
		"table=0",
		"priority=40",
		"arp,arp_op=1",
		"arp_tpa=192.168.1.10",
		"mod_dl_src:0a:58:c0:a8:01:0a",
		"load:0x0a58c0a8010a->NXM_NX_ARP_SHA[]",
		"move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[]",
		"load:0x2->NXM_OF_ARP_OP[]",
		"move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[]",
		"move:NXM_OF_ARP_TPA[]->NXM_NX_REG0[]",
		"move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[]",
		"move:NXM_NX_REG0[]->NXM_OF_ARP_SPA[]",
		"IN_PORT",
	} {
		if !strings.Contains(flow, want) {
			t.Errorf("flow missing %q\ngot: %s", want, flow)
		}
	}

	if mock.syncCount != 1 {
		t.Errorf("expected 1 RequestFlowSync call, got %d", mock.syncCount)
	}
}

func TestDeleteARPFlow(t *testing.T) {
	mock := &mockFlowManager{}
	syncer := newMACBindingSyncOps(nil, mock)

	if err := syncer.DeleteARPFlow("192.168.1.10"); err != nil {
		t.Fatalf("DeleteARPFlow() error: %v", err)
	}

	if len(mock.deleteCalls) != 1 {
		t.Fatalf("expected 1 DeleteExBridgeFlowsByKey call, got %d", len(mock.deleteCalls))
	}

	if mock.deleteCalls[0].key != "MAC_BINDING_ARP_192.168.1.10" {
		t.Errorf("key = %q, want %q", mock.deleteCalls[0].key, "MAC_BINDING_ARP_192.168.1.10")
	}

	if mock.syncCount != 1 {
		t.Errorf("expected 1 RequestFlowSync call, got %d", mock.syncCount)
	}
}
