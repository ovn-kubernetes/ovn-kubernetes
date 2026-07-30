// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package bridgeconfig

import (
	"fmt"
	"testing"

	"github.com/k8snetworkplumbingwg/sriovnet"
	"github.com/onsi/gomega"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

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

func TestNewUnmanagedBridgeConfigurationResolvesDPUHostRepresentor(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
		util.ResetRunner()
	})
	config.IPv4Mode = false
	config.OvnKubeNode.Mode = ovntypes.NodeModeDPU

	bridgeUUID := "ovsbr1-uuid"
	uplinkPortUUID := "eth1-port-uuid"
	uplinkInterfaceUUID := "eth1-interface-uuid"
	hostRepPortUUID := "pfhpf0-port-uuid"
	hostRepInterfaceUUID := "pfhpf0-interface-uuid"
	ovsClient, ovsCleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
			&vswitchd.Bridge{
				UUID:        bridgeUUID,
				Name:        "ovsbr1",
				Ports:       []string{uplinkPortUUID, hostRepPortUUID},
				ExternalIDs: map[string]string{"bridge-uplink": "eth1"},
			},
			&vswitchd.Port{UUID: uplinkPortUUID, Name: "eth1", Interfaces: []string{uplinkInterfaceUUID}},
			&vswitchd.Interface{UUID: uplinkInterfaceUUID, Name: "eth1", Type: "system"},
			&vswitchd.Port{UUID: hostRepPortUUID, Name: "pfhpf0", Interfaces: []string{hostRepInterfaceUUID}},
			&vswitchd.Interface{UUID: hostRepInterfaceUUID, Name: "pfhpf0", Type: "system"},
		},
	})
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
	// GetDPUHostRepInterface iterates bridge ports in map order and returns on
	// the first PF representor, so eth1 may or may not be probed before pfhpf0.
	sriovOps.On("GetRepresentorPortFlavour", "eth1").
		Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_UNKNOWN), fmt.Errorf("not a PF representor")).
		Maybe()
	sriovOps.On("GetRepresentorPortFlavour", "pfhpf0").
		Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_PCI_PF), nil)

	bridge, err := NewUnmanagedBridgeConfiguration(
		ovsClient,
		"ovsbr1",
		"pf0",
		"node-a",
		"physnet-blue",
		ovntest.MustParseIPNets("172.28.0.2/24"),
		ovntest.MustParseMAC("00:11:22:33:44:55"),
	)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(bridge.GetGatewayIfaceRep()).To(gomega.Equal("pfhpf0"))
	g.Expect(bridge.GetStaticFDBPort()).To(gomega.Equal("pfhpf0"))
	g.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc())
}
