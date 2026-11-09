// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// Keep real OVS setup and rollback while bypassing container link creation.
type failedPrimaryUDNInterfaceOps struct {
	podRequestInterfaceOpsStub
	t *testing.T
}

func (s *failedPrimaryUDNInterfaceOps) ConfigureInterface(pr *PodRequest, c client.Client, _ PodInfoGetter, info *PodInterfaceInfo) ([]*current.Interface, error) {
	if info.NetName == types.DefaultNetworkName {
		return nil, nil
	}
	err := ConfigureOVS(pr.ctx, c, pr.PodNamespace, pr.PodName, pr.IfName, "rep0", info, pr.SandboxID, "", false, nil)
	require.ErrorContains(s.t, err, "failed to find interface rep0")
	_, lookupErr := ovsops.GetOVSPort(c, "rep0")
	require.NoError(s.t, lookupErr, "ADD must have created its port before failing")
	pr.deletePort(c, "rep0", pr.PodNamespace, pr.PodName)
	_, lookupErr = ovsops.GetOVSPort(c, "rep0")
	require.ErrorIs(s.t, lookupErr, client.ErrNotFound, "rollback must remove its primary UDN port")
	return nil, err
}

func TestCmdAddPrimaryUDNRollbackAfterJSON(t *testing.T) {
	withCNIEnv(t, func() {
		require.NoError(t, config.PrepareTestConfig())
		t.Cleanup(func() { require.NoError(t, config.PrepareTestConfig()) })
		setupShimOVS(t)
		oldOps := podRequestInterfaceOps
		podRequestInterfaceOps = &failedPrimaryUDNInterfaceOps{t: t}
		t.Cleanup(func() { podRequestInterfaceOps = oldOps })
		netlinkOps := new(utilmocks.NetLinkOps)
		netlinkOps.On("LinkByName", "rep0").Return(nil, errors.New("link absent"))
		oldNetlink := util.GetNetLinkOps()
		util.SetNetLinkOpMockInst(netlinkOps)
		t.Cleanup(func() { util.SetNetLinkOpMockInst(oldNetlink) })
		body, err := json.Marshal(&Response{
			PodIFInfo: &PodInterfaceInfo{NetName: types.DefaultNetworkName, NADKey: types.DefaultNetworkName},
			PrimaryUDNPodReq: &PodRequest{Command: CNIAdd, PodNamespace: "ns", PodName: "pod", IfName: "eth1",
				netName: "blue", nadKey: "ns/blue", SandboxID: "sandbox", CNIConf: &ovncnitypes.NetConf{}},
			PrimaryUDNPodInfo: &PodInterfaceInfo{NetName: "blue", NADKey: "ns/blue", Egress: 1000},
			KubeAuth:          &KubeAPIAuth{},
		})
		require.NoError(t, err)
		p := &Plugin{doCNIFunc: func(_ string, _ interface{}) ([]byte, error) { return body, nil }}
		err = p.CmdAdd(&skel.CmdArgs{StdinData: []byte(`{"cniVersion":"1.1.0","name":"default","type":"ovn-k8s-cni-overlay"}`)})
		require.ErrorContains(t, err, "failed to find interface rep0")
	})
}

func TestUnconfigureInterfaceOrphanQoS(t *testing.T) {
	for _, segmented := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "segmented"}[segmented], func(t *testing.T) {
			require.NoError(t, config.PrepareTestConfig())
			t.Cleanup(func() { require.NoError(t, config.PrepareTestConfig()) })
			config.OVNKubernetesFeature.EnableMultiNetwork = segmented
			config.OVNKubernetesFeature.EnableNetworkSegmentation = segmented
			const sandbox = "123456789012345"
			qos := "live-qos"
			server, _ := newShimOVSTestServer(t, []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root", Bridges: []string{"bridge", "other"}},
				&vswitchd.Bridge{UUID: "bridge", Name: "br-int"},
				&vswitchd.Bridge{UUID: "other", Name: "br-other", Ports: []string{"port"}},
				&vswitchd.Port{UUID: "port", Name: sandbox, Interfaces: []string{"iface"}, QOS: &qos},
				&vswitchd.Interface{UUID: "iface", Name: sandbox, ExternalIDs: map[string]string{"sandbox": sandbox}},
				&vswitchd.QoS{UUID: qos, ExternalIDs: map[string]string{"sandbox": sandbox}, OtherConfig: map[string]string{"max-rate": "1000"}},
				&vswitchd.QoS{UUID: "orphan", ExternalIDs: map[string]string{"sandbox": sandbox}},
			})
			c := newShimOVSClientForTest(t, server, CNIDel)
			netlinkOps := new(utilmocks.NetLinkOps)
			old := util.GetNetLinkOps()
			util.SetNetLinkOpMockInst(netlinkOps)
			t.Cleanup(func() { util.SetNetLinkOpMockInst(old) })
			pr := &PodRequest{Command: CNIDel, SandboxID: sandbox, netName: types.DefaultNetworkName,
				nadKey: types.DefaultNetworkName, IsVFIO: true, CNIConf: &ovncnitypes.NetConf{}}
			require.NoError(t, (&defaultPodRequestInterfaceOps{}).UnconfigureInterface(pr, c, &PodInterfaceInfo{}, nil, nil))
			checkPodIngress(t, c, sandbox, "1000")
			rows, err := ovsops.TransactAndCheck(c, []ovsdb.Operation{{Op: ovsdb.OperationSelect, Table: vswitchd.QoSTable}})
			require.NoError(t, err)
			require.Len(t, rows[0].Rows, 1, "only referenced QoS should remain")
			netlinkOps.AssertNotCalled(t, "LinkByName", mock.Anything)
			// Also cover retry after Port/Interface have already disappeared.
			require.NoError(t, ovsops.DeletePortWithInterfaces(c, "br-other", sandbox))
			netlinkOps.On("LinkByName", sandbox).Return(nil, errors.New("link absent"))
			require.NoError(t, (&defaultPodRequestInterfaceOps{}).UnconfigureInterface(pr, c, &PodInterfaceInfo{}, nil, nil))
			rows, err = ovsops.TransactAndCheck(c, []ovsdb.Operation{{Op: ovsdb.OperationSelect, Table: vswitchd.QoSTable}})
			require.NoError(t, err)
			require.Empty(t, rows[0].Rows)
		})
	}
}

func TestBandwidthCleanupPreservesUnrelatedSameNamedPort(t *testing.T) {
	qos := "qos"
	server, _ := newShimOVSTestServer(t, []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "root", Bridges: []string{"bridge"}},
		&vswitchd.Bridge{UUID: "bridge", Name: "br-int", Ports: []string{"bond", "port"}},
		&vswitchd.Port{UUID: "bond", Name: "bond0", Interfaces: []string{"member"}},
		&vswitchd.Interface{UUID: "member", Name: "rep0", ExternalIDs: map[string]string{"sandbox": "sandbox"}},
		&vswitchd.Port{UUID: "port", Name: "rep0", Interfaces: []string{"other"}, QOS: &qos},
		&vswitchd.Interface{UUID: "other", Name: "other"},
		&vswitchd.QoS{UUID: qos, ExternalIDs: map[string]string{"sandbox": "sandbox"}, OtherConfig: map[string]string{"max-rate": "1000"}},
	})
	c := newShimOVSClientForTest(t, server, CNIDel)
	require.NoError(t, clearPodBandwidthForTest(c, "sandbox", "rep0"))
	require.NoError(t, clearOrphanedPodQoS(c, "sandbox"))
	checkPodIngress(t, c, "rep0", "1000")
	iface := &vswitchd.Interface{Name: "rep0"}
	require.NoError(t, c.Get(context.Background(), iface))
	require.Equal(t, "sandbox", iface.ExternalIDs["sandbox"])
}

func TestConfigureOVSPositiveBandwidth(t *testing.T) {
	server, _ := newShimOVSTestServer(t, []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "root", Bridges: []string{"bridge"}},
		&vswitchd.Bridge{UUID: "bridge", Name: "br-int"},
	})
	emulateShimOVSReadiness(t, server)
	c := newShimOVSClientForTest(t, server, CNIAdd)
	netns, err := testutils.NewNS()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, netns.Close())
		require.NoError(t, testutils.UnmountNS(netns))
	})
	require.NoError(t, netns.Do(func(_ ns.NetNS) error {
		return netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "pod-port"}})
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, limited := range []bool{true, false} {
		info := &PodInterfaceInfo{NetName: types.DefaultNetworkName, NADKey: types.DefaultNetworkName}
		if limited {
			info.Ingress, info.Egress = 10_000_000, 2_000_000
		}
		require.NoError(t, netns.Do(func(_ ns.NetNS) error {
			return ConfigureOVS(ctx, c, "ns", "pod", "eth0", "pod-port", info, "sandbox", "", false, nil)
		}))
		// Policing columns are intentionally omitted from the shim monitor.
		require.Eventually(t, func() bool {
			iface, err := ovsops.GetOVSInterface(server, "pod-port")
			return err == nil && iface.IngressPolicingRate == int(info.Egress/1000) &&
				iface.IngressPolicingBurst == int(info.Egress/10000)
		}, time.Second, time.Millisecond)
		if limited {
			checkPodIngress(t, c, "pod-port", "10000000")
		} else {
			port, err := ovsops.GetOVSPort(c, "pod-port")
			require.NoError(t, err)
			require.Nil(t, port.QOS)
			rows, err := ovsops.TransactAndCheck(c, []ovsdb.Operation{{Op: ovsdb.OperationSelect, Table: vswitchd.QoSTable}})
			require.NoError(t, err)
			require.Empty(t, rows[0].Rows)
		}
	}
}
