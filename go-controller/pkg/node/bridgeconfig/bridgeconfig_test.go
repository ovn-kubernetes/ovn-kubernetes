// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package bridgeconfig

import (
	"fmt"
	"testing"

	"github.com/k8snetworkplumbingwg/sriovnet"
	"github.com/onsi/gomega"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func TestBridgedGatewayNodeSetupRejectsMappingSeparators(t *testing.T) {
	g := gomega.NewWithT(t)
	ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(
		libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{UUID: "root-ovs", ExternalIDs: map[string]string{"ovn-bridge-mappings": "physnet:br-ex"}},
		}},
	)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(cleanup.Cleanup)

	for _, name := range []string{"net:br-evil", "net,evil:br-evil", "physnet:br-evil,net"} {
		_, err := bridgedGatewayNodeSetup(ovsClient, "node-a", "br-a", name)
		g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("cannot contain")), name)
	}
	ovs, err := ovsops.GetOpenvSwitch(ovsClient)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(ovs.ExternalIDs["ovn-bridge-mappings"]).To(gomega.Equal("physnet:br-ex"))
}

func TestGetStaticFDBPort(t *testing.T) {
	tests := []struct {
		name     string
		bridge   *BridgeConfiguration
		expected string
	}{
		{
			name: "uses bridge when representor is absent",
			bridge: &BridgeConfiguration{
				bridgeName: "br-ex",
			},
			expected: "br-ex",
		},
		{
			name: "uses representor when present",
			bridge: &BridgeConfiguration{
				bridgeName: "ovsbr1",
				gwIfaceRep: "pf0hpf",
			},
			expected: "pf0hpf",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.bridge.GetStaticFDBPort(); got != tc.expected {
				t.Fatalf("expected static FDB port %q, got %q", tc.expected, got)
			}
		})
	}
}

func TestGatewayHostOVSInterfaceResolvesSmartNICRepresentor(t *testing.T) {
	g := gomega.NewWithT(t)
	fexec := ovntest.NewFakeExec()
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 port-to-br pf0vf1",
		Stderr: "no bridge for pf0vf1",
		Err:    fmt.Errorf("not an OVS port"),
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 port-to-br pf0vf1_rep",
		Output: "ovsbr1",
	})
	g.Expect(util.SetExec(fexec)).To(gomega.Succeed())
	t.Cleanup(util.ResetRunner)

	fsOps := utilmocks.NewFileSystemOps(t)
	origFSOps := util.GetFileSystemOps()
	util.SetFileSystemOps(fsOps)
	t.Cleanup(func() {
		util.SetFileSystemOps(origFSOps)
	})
	fsOps.On("Readlink", "/sys/class/net/pf0vf1/device").
		Return("../../0000:00:00.1", nil)

	sriovOps := utilmocks.NewSriovnetOps(t)
	origSriovOps := util.GetSriovnetOps()
	util.SetSriovnetOpsInst(sriovOps)
	t.Cleanup(func() {
		util.SetSriovnetOpsInst(origSriovOps)
	})
	sriovOps.On("GetUplinkRepresentor", "0000:00:00.1").Return("pf0", nil)
	sriovOps.On("GetVfIndexByPciAddress", "0000:00:00.1").Return(1, nil)
	sriovOps.On("GetVfRepresentor", "pf0", 1).Return("pf0vf1_rep", nil)

	rep, err := gatewayHostOVSInterface("ovsbr1", "pf0vf1")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(rep).To(gomega.Equal("pf0vf1_rep"))
	g.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc())
}

// newDPUUnmanagedBridgeHarness prepares the scaffolding shared by the
// unmanaged bridge configuration tests: a DPU-mode node config restored on
// cleanup, an OVSDB with one bridge holding the eth1 uplink and one
// (Port, Interface) pair per representor name, a fake exec that answers the
// eth1 ofport probe, and a fresh sriovnet mock installed for the test.
func newDPUUnmanagedBridgeHarness(t *testing.T, bridgeName string, bridgeExternalIDs map[string]string,
	repNames ...string) (libovsdbclient.Client, *ovntest.FakeExec, *utilmocks.SriovnetOps) {
	t.Helper()
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
		util.ResetRunner()
	})
	config.IPv4Mode = false
	config.OvnKubeNode.Mode = ovntypes.NodeModeDPU

	bridgeUUID := bridgeName + "-uuid"
	portUUIDs := []string{"eth1-port-uuid"}
	ovsData := []libovsdbtest.TestData{
		&vswitchd.Port{UUID: "eth1-port-uuid", Name: "eth1", Interfaces: []string{"eth1-interface-uuid"}},
		&vswitchd.Interface{UUID: "eth1-interface-uuid", Name: "eth1", Type: "system"},
	}
	for _, repName := range repNames {
		portUUID := repName + "-port-uuid"
		interfaceUUID := repName + "-interface-uuid"
		portUUIDs = append(portUUIDs, portUUID)
		ovsData = append(ovsData,
			&vswitchd.Port{UUID: portUUID, Name: repName, Interfaces: []string{interfaceUUID}},
			&vswitchd.Interface{UUID: interfaceUUID, Name: repName, Type: "system"},
		)
	}
	ovsData = append(ovsData,
		&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
		&vswitchd.Bridge{
			UUID:        bridgeUUID,
			Name:        bridgeName,
			Ports:       portUUIDs,
			ExternalIDs: bridgeExternalIDs,
		},
	)
	ovsClient, ovsCleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: ovsData})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(ovsCleanup.Cleanup)

	fexec := ovntest.NewLooseCompareFakeExec()
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 get interface eth1 ofport",
		Output: "7",
	})
	g.Expect(util.SetExec(fexec)).To(gomega.Succeed())

	sriovOps := utilmocks.NewSriovnetOps(t)
	origSriovOps := util.GetSriovnetOps()
	util.SetSriovnetOpsInst(sriovOps)
	t.Cleanup(func() {
		util.SetSriovnetOpsInst(origSriovOps)
	})
	return ovsClient, fexec, sriovOps
}

func TestNewUnmanagedBridgeConfigurationResolvesDPUHostRepresentor(t *testing.T) {
	g := gomega.NewWithT(t)
	ovsClient, fexec, sriovOps := newDPUUnmanagedBridgeHarness(t, "ovsbr1",
		map[string]string{"bridge-uplink": "eth1"}, "pfhpf0")
	// FindHostRepresentorByPeerMAC walks the bridge ports in map order and
	// returns on the first match, so expectations for the other ports may
	// legitimately go unused.
	sriovOps.On("GetRepresentorPortFlavour", "eth1").
		Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_UNKNOWN), fmt.Errorf("not a representor")).
		Maybe()
	sriovOps.On("GetRepresentorPortFlavour", "pfhpf0").
		Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_PCI_PF), nil)
	sriovOps.On("GetDevlinkPortFunctionMacAddress", "pfhpf0").
		Return(ovntest.MustParseMAC("00:11:22:33:44:55"), nil)

	bridge, err := NewUnmanagedBridgeConfiguration(
		ovsClient,
		"ovsbr1",
		"pf0",
		"node-a",
		"physnet-blue",
		ovntest.MustParseIPNets("172.28.0.2/24"),
		ovntest.MustParseMAC("00:11:22:33:44:55"),
	)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "the PF-backed unmanaged bridge configuration must succeed")
	g.Expect(bridge.GetGatewayIfaceRep()).To(gomega.Equal("pfhpf0"),
		"the PF representor must be selected as the gateway representor")
	g.Expect(bridge.GetStaticFDBPort()).To(gomega.Equal("pfhpf0"),
		"the static FDB entry must be pinned to the PF representor")
	g.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc())
}

func TestNewUnmanagedBridgeConfigurationResolvesDPUHostVFRepresentor(t *testing.T) {
	g := gomega.NewWithT(t)
	// No bridge-uplink external-id: the physical uplink must be derived by
	// skipping the VF representors.
	ovsClient, fexec, sriovOps := newDPUUnmanagedBridgeHarness(t, "br-hostvf0", nil, "pf0vf0", "pf0vf7")
	// The walk returns on the first match (pf0vf7), so the probes of the
	// other ports may legitimately go unused depending on map order.
	sriovOps.On("GetRepresentorPortFlavour", "eth1").
		Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_UNKNOWN), fmt.Errorf("not a representor")).
		Maybe()
	sriovOps.On("GetRepresentorPortFlavour", "pf0vf0").
		Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_PCI_VF), nil).
		Maybe()
	sriovOps.On("GetDevlinkPortFunctionMacAddress", "pf0vf0").
		Return(ovntest.MustParseMAC("00:11:22:33:44:66"), nil).
		Maybe()
	sriovOps.On("GetRepresentorPortFlavour", "pf0vf7").
		Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_PCI_VF), nil)
	sriovOps.On("GetDevlinkPortFunctionMacAddress", "pf0vf7").
		Return(ovntest.MustParseMAC("00:11:22:33:44:55"), nil)

	bridge, err := NewUnmanagedBridgeConfiguration(
		ovsClient,
		"br-hostvf0",
		"enp4s0f0v0",
		"node-a",
		"physnet-blue",
		ovntest.MustParseIPNets("172.28.0.2/24"),
		ovntest.MustParseMAC("00:11:22:33:44:55"),
	)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "the VF-backed unmanaged bridge configuration must succeed")
	g.Expect(bridge.GetGatewayIfaceRep()).To(gomega.Equal("pf0vf7"),
		"the VF representor whose peer MAC matches must be selected as the gateway representor")
	g.Expect(bridge.GetStaticFDBPort()).To(gomega.Equal("pf0vf7"),
		"the static FDB entry must be pinned to the matching VF representor")
	g.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc())
}
