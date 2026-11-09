// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package managementport

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	multinetworkmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks/multinetwork"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func TestSyncUDNManagementPortMultipleRepresentors(t *testing.T) {
	for _, scenario := range []string{"DPU", "full", "lookup error", "cleanup error"} {
		t.Run(scenario, func(t *testing.T) {
			require.NoError(t, config.PrepareTestConfig())
			t.Cleanup(func() { require.NoError(t, config.PrepareTestConfig()) })
			config.OvnKubeNode.Mode = types.NodeModeDPU
			if scenario == "full" {
				config.OvnKubeNode.Mode = types.NodeModeFull
			}
			bridge := &vswitchd.Bridge{UUID: "bridge", Name: "br-int"}
			data := []libovsdbtest.TestData{&vswitchd.OpenvSwitch{UUID: "root", Bridges: []string{bridge.UUID}}, bridge}
			for _, name := range []string{"current", "stale1", "stale2"} {
				bridge.Ports = append(bridge.Ports, name+"-port")
				data = append(data, &vswitchd.Port{UUID: name + "-port", Name: name, Interfaces: []string{name + "-iface"}},
					&vswitchd.Interface{UUID: name + "-iface", Name: name, ExternalIDs: map[string]string{types.OvnManagementPortNameExternalID: "ovn-k8s-mp1"}})
			}
			c, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: data})
			require.NoError(t, err)
			t.Cleanup(cleanup.Cleanup)
			t.Cleanup(libovsdbtest.EmulateVSwitchdConfig(c))
			netInfo := &multinetworkmocks.NetInfo{}
			netInfo.On("GetNetworkName").Return("blue")
			netlinkOps := new(utilmocks.NetLinkOps)
			old := util.GetNetLinkOps()
			util.SetNetLinkOpMockInst(netlinkOps)
			t.Cleanup(func() { util.SetNetLinkOpMockInst(old) })
			missing := errors.New("link absent")
			for _, name := range []string{"current", "stale1", "stale2"} {
				netlinkOps.On("LinkByName", name).Return(nil, missing)
			}
			netlinkOps.On("IsLinkNotFoundError", missing).Return(scenario != "cleanup error")
			sriovOps := new(utilmocks.SriovnetOps)
			oldSriov := util.GetSriovnetOps()
			util.SetSriovnetOpsInst(sriovOps)
			t.Cleanup(func() { util.SetSriovnetOpsInst(oldSriov) })
			var lookupErr error
			if scenario == "lookup error" {
				lookupErr = errors.New("representor unavailable")
			}
			sriovOps.On("GetVfRepresentorDPU", "0", "1").Return("current", lookupErr)
			err = syncUDNManagementPort(&udnManagementPortConfig{NetInfo: netInfo, ovsClient: c}, "ovn-k8s-mp1", &util.NetworkDeviceDetails{PfId: 0, FuncId: 1})
			if lookupErr != nil {
				require.ErrorIs(t, err, lookupErr)
			} else if scenario == "cleanup error" {
				require.ErrorContains(t, err, "failed to lookup management port representor")
			} else {
				require.NoError(t, err)
			}
			for _, name := range []string{"current", "stale1", "stale2"} {
				_, err := ovsops.GetOVSPort(c, name)
				if lookupErr != nil || (name == "current" && scenario != "full") {
					require.NoError(t, err)
					netlinkOps.AssertNotCalled(t, "LinkByName", name)
				} else {
					require.ErrorIs(t, err, libovsdbclient.ErrNotFound)
					netlinkOps.AssertCalled(t, "LinkByName", name)
				}
			}
		})
	}
}
