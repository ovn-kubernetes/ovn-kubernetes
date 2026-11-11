// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package managementport

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"k8s.io/utils/ptr"

	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func TestCreateManagementPortOVSRepresentorWithClient(t *testing.T) {
	const bridgeUUID = "br-int-uuid"
	ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
		&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
	}})
	if err != nil {
		t.Fatalf("failed to create OVS test harness: %v", err)
	}
	t.Cleanup(cleanup.Cleanup)
	t.Cleanup(libovsdbtest.EmulateVSwitchdConfig(ovsClient))

	err = createManagementPortOVSRepresentor(ovsClient, "blue", "rep0", "k8s-node", 1400, map[string]string{
		types.NetworkExternalID:               "blue",
		types.OvnManagementPortNameExternalID: "ovn-k8s-mp1",
	})
	if err != nil {
		t.Fatalf("createManagementPortOVSRepresentor() error = %v", err)
	}
	iface := &vswitchd.Interface{Name: "rep0"}
	if err := ovsClient.Get(context.Background(), iface); err != nil {
		t.Fatalf("failed to get created interface: %v", err)
	}
	if iface.ExternalIDs["iface-id"] != "k8s-node" || iface.ExternalIDs[types.NetworkExternalID] != "blue" ||
		iface.ExternalIDs[types.OvnManagementPortNameExternalID] != "ovn-k8s-mp1" {
		t.Fatalf("unexpected interface external IDs: %#v", iface.ExternalIDs)
	}
}

func TestCreateManagementPortOVSRepresentorPreservesOptions(t *testing.T) {
	const devargs = "0000:08:00.0,representor=vf3"
	c, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "root", Bridges: []string{"bridge"}},
		&vswitchd.Bridge{UUID: "bridge", Name: "br-int", DatapathType: types.DatapathUserspace, Ports: []string{"port"}},
		&vswitchd.Port{UUID: "port", Name: "rep0", Interfaces: []string{"iface"}},
		&vswitchd.Interface{UUID: "iface", Name: "rep0", Type: "system", Options: map[string]string{"dpdk-devargs": devargs}},
	}})
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)
	done := make(chan error, 1)
	go func() { done <- createManagementPortOVSRepresentor(c, "blue", "rep0", "k8s-node", 1400, nil) }()
	// Start acknowledgement on failure too, so the worker always finishes.
	acknowledged := false
	t.Cleanup(func() {
		if !acknowledged {
			stop := libovsdbtest.EmulateVSwitchdConfig(c)
			defer stop()
			<-done
		}
	})
	require.Eventually(t, func() bool {
		root, err := ovsops.GetOpenvSwitch(c)
		return err == nil && root.NextCfg > root.CurCfg
	}, time.Second, time.Millisecond)
	select {
	case err := <-done:
		acknowledged = true
		t.Fatalf("representor creation returned before ovs-vswitchd acknowledgement: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	stop := libovsdbtest.EmulateVSwitchdConfig(c)
	defer stop()
	err = <-done
	acknowledged = true
	require.NoError(t, err)
	iface := &vswitchd.Interface{Name: "rep0"}
	require.NoError(t, c.Get(context.Background(), iface))
	require.Equal(t, devargs, iface.Options["dpdk-devargs"], "reconciling an existing representor must preserve its device selection")
	require.Equal(t, "dpdk", iface.Type)
	require.NotNil(t, iface.MTURequest)
	require.Equal(t, 1400, *iface.MTURequest)
	var rows []vswitchd.OpenvSwitch
	require.NoError(t, c.List(context.Background(), &rows))
	require.Len(t, rows, 1)
	require.Positive(t, rows[0].NextCfg)
	require.GreaterOrEqual(t, rows[0].CurCfg, rows[0].NextCfg)
}

func TestKernelRepresentorPreservesMTURequest(t *testing.T) {
	c, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "root", Bridges: []string{"bridge"}},
		&vswitchd.Bridge{UUID: "bridge", Name: "br-int", Ports: []string{"port"}},
		&vswitchd.Port{UUID: "port", Name: "rep0", Interfaces: []string{"iface"}},
		&vswitchd.Interface{UUID: "iface", Name: "rep0", MTURequest: ptr.To(9000)},
	}})
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)
	t.Cleanup(libovsdbtest.EmulateVSwitchdConfig(c))
	require.NoError(t, createManagementPortOVSRepresentor(c, "blue", "rep0", "k8s-node", 1400, nil))
	iface, err := ovsops.GetOVSInterface(c, "rep0")
	require.NoError(t, err)
	require.Equal(t, ptr.To(9000), iface.MTURequest)
}
