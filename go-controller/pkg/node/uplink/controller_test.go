// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package uplink

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/k8snetworkplumbingwg/sriovnet"
	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned/fake"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func TestIsDefaultRoute(t *testing.T) {
	tests := []struct {
		name     string
		route    netlink.Route
		expected bool
	}{
		{
			name:     "nil destination",
			route:    netlink.Route{},
			expected: true,
		},
		{
			name: "IPv4 zero prefix",
			route: netlink.Route{
				Dst: ovntest.MustParseIPNet("0.0.0.0/0"),
			},
			expected: true,
		},
		{
			name: "IPv6 zero prefix",
			route: netlink.Route{
				Dst: ovntest.MustParseIPNet("::/0"),
			},
			expected: true,
		},
		{
			name: "IPv6 non-default prefix",
			route: netlink.Route{
				Dst: ovntest.MustParseIPNet("2001:db8::/32"),
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(isDefaultRoute(tt.route)).To(gomega.Equal(tt.expected))
		})
	}
}

func TestNodeNeedsUpdate(t *testing.T) {
	g := gomega.NewWithT(t)
	controller := &Controller{nodeName: "node-a"}
	localNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
	remoteNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}}

	g.Expect(controller.nodeNeedsUpdate(nil, localNode)).To(gomega.BeTrue())
	g.Expect(controller.nodeNeedsUpdate(nil, remoteNode)).To(gomega.BeFalse())
	g.Expect(controller.nodeNeedsUpdate(localNode, localNode.DeepCopy())).To(gomega.BeFalse())

	updatedLocalNode := localNode.DeepCopy()
	updatedLocalNode.Labels = map[string]string{"example.com/uplink": "blue"}
	g.Expect(controller.nodeNeedsUpdate(localNode, updatedLocalNode)).To(gomega.BeTrue())

	updatedLocalNode = localNode.DeepCopy()
	updatedLocalNode.Annotations = map[string]string{
		util.OvnNodeL3GatewayConfig: `{"default":{"mode":"shared","bridge-id":"breth0"}}`,
	}
	g.Expect(controller.nodeNeedsUpdate(localNode, updatedLocalNode)).To(gomega.BeTrue())

	updatedLocalNode = localNode.DeepCopy()
	updatedLocalNode.Annotations = map[string]string{util.OvnNodeChassisID: "chassis-id"}
	g.Expect(controller.nodeNeedsUpdate(localNode, updatedLocalNode)).To(gomega.BeTrue())

	updatedRemoteNode := remoteNode.DeepCopy()
	updatedRemoteNode.Labels = map[string]string{"example.com/uplink": "blue"}
	g.Expect(controller.nodeNeedsUpdate(remoteNode, updatedRemoteNode)).To(gomega.BeFalse())
}

func TestDefaultOVSBridgeResolverUsesOVSDB(t *testing.T) {
	g := gomega.NewWithT(t)
	const (
		bridge1UUID    = "bridge-ovsbr1-uuid"
		bridge2UUID    = "bridge-ovsbr2-uuid"
		port1UUID      = "port-eth0-uuid"
		port2UUID      = "port-eth1-uuid"
		interface1UUID = "interface-eth0-uuid"
		interface2UUID = "interface-eth1-uuid"
	)
	ovsClient, testCtx, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridge1UUID, bridge2UUID}},
			&vswitchd.Bridge{UUID: bridge1UUID, Name: "ovsbr1", Ports: []string{port1UUID}},
			&vswitchd.Bridge{
				UUID: bridge2UUID, Name: "ovsbr2", Ports: []string{port2UUID},
				ExternalIDs: map[string]string{"bridge-uplink": "eth1"},
			},
			&vswitchd.Port{UUID: port1UUID, Name: "eth0", Interfaces: []string{interface1UUID}},
			&vswitchd.Port{UUID: port2UUID, Name: "bond0", Interfaces: []string{interface2UUID}},
			&vswitchd.Interface{UUID: interface1UUID, Name: "eth0", Type: "system"},
			&vswitchd.Interface{UUID: interface2UUID, Name: "eth1", Type: "internal"},
		},
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(testCtx.Cleanup)

	resolver := defaultOVSBridgeResolver{ovsClient: ovsClient}
	bridgeName, err := resolver.Resolve("ovsbr1")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(bridgeName).To(gomega.Equal("ovsbr1"))

	bridgeName, err = resolver.Resolve("eth0")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(bridgeName).To(gomega.Equal("ovsbr1"))

	bridgeName, err = resolver.Resolve("bond0")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(bridgeName).To(gomega.Equal("ovsbr2"))

	bridgeName, err = resolver.Resolve("eth1")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(bridgeName).To(gomega.Equal("ovsbr2"))

	uplinkName, err := resolver.BridgeUplink("ovsbr1")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(uplinkName).To(gomega.Equal("eth0"))

	uplinkName, err = resolver.BridgeUplink("ovsbr2")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(uplinkName).To(gomega.Equal("eth1"))
}

func TestDefaultOVSBridgeResolverResolvesSmartNICRepresentor(t *testing.T) {
	g := gomega.NewWithT(t)
	const (
		bridgeUUID    = "bridge-ovsbr1-uuid"
		portUUID      = "port-pf0vf1-rep-uuid"
		interfaceUUID = "interface-pf0vf1-rep-uuid"
	)
	ovsClient, testCtx, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
			&vswitchd.Bridge{UUID: bridgeUUID, Name: "ovsbr1", Ports: []string{portUUID}},
			&vswitchd.Port{UUID: portUUID, Name: "pf0vf1_rep", Interfaces: []string{interfaceUUID}},
			&vswitchd.Interface{UUID: interfaceUUID, Name: "pf0vf1_rep"},
		},
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(testCtx.Cleanup)

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

	bridgeName, err := (defaultOVSBridgeResolver{ovsClient: ovsClient}).Resolve("pf0vf1")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(bridgeName).To(gomega.Equal("ovsbr1"))
}

// newDPUBridgeResolverHarness models a BlueField style DPU: br-host carries the
// host PF representor that backs the default gateway bridge, br-vm carries host
// VF representors, and br-int is the OVN integration bridge.
func newDPUBridgeResolverHarness(t *testing.T) defaultOVSBridgeResolver {
	t.Helper()
	g := gomega.NewWithT(t)
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})

	ovsData := []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"br-int-uuid", "br-host-uuid", "br-vm-uuid"}},
		&vswitchd.Bridge{UUID: "br-int-uuid", Name: "br-int", Ports: []string{"mp0-port-uuid"}},
		&vswitchd.Port{UUID: "mp0-port-uuid", Name: "ovn-k8s-mp0", Interfaces: []string{"mp0-iface-uuid"}},
		&vswitchd.Interface{UUID: "mp0-iface-uuid", Name: "ovn-k8s-mp0"},

		&vswitchd.Bridge{UUID: "br-host-uuid", Name: "br-host", Ports: []string{"p0-port-uuid", "pf0hpf-port-uuid"}},
		&vswitchd.Port{UUID: "p0-port-uuid", Name: "p0", Interfaces: []string{"p0-iface-uuid"}},
		&vswitchd.Interface{UUID: "p0-iface-uuid", Name: "p0", Type: "system"},
		&vswitchd.Port{UUID: "pf0hpf-port-uuid", Name: "pf0hpf", Interfaces: []string{"pf0hpf-iface-uuid"}},
		&vswitchd.Interface{UUID: "pf0hpf-iface-uuid", Name: "pf0hpf", Type: "system"},

		&vswitchd.Bridge{UUID: "br-vm-uuid", Name: "br-vm", Ports: []string{"p1-port-uuid", "pf0vf0-port-uuid", "pf0vf7-port-uuid"}},
		&vswitchd.Port{UUID: "p1-port-uuid", Name: "p1", Interfaces: []string{"p1-iface-uuid"}},
		&vswitchd.Interface{UUID: "p1-iface-uuid", Name: "p1", Type: "system"},
		&vswitchd.Port{UUID: "pf0vf0-port-uuid", Name: "pf0vf0", Interfaces: []string{"pf0vf0-iface-uuid"}},
		&vswitchd.Interface{UUID: "pf0vf0-iface-uuid", Name: "pf0vf0", Type: "system"},
		&vswitchd.Port{UUID: "pf0vf7-port-uuid", Name: "pf0vf7", Interfaces: []string{"pf0vf7-iface-uuid"}},
		&vswitchd.Interface{UUID: "pf0vf7-iface-uuid", Name: "pf0vf7", Type: "system"},
	}
	ovsClient, testCtx, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: ovsData})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(testCtx.Cleanup)

	sriovOps := utilmocks.NewSriovnetOps(t)
	origSriovOps := util.GetSriovnetOps()
	util.SetSriovnetOpsInst(sriovOps)
	t.Cleanup(func() {
		util.SetSriovnetOpsInst(origSriovOps)
	})

	// The physical uplinks are not representors at all.
	for _, uplink := range []string{"p0", "p1", "ovn-k8s-mp0"} {
		sriovOps.On("GetRepresentorPortFlavour", uplink).
			Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_UNKNOWN),
				fmt.Errorf("not a representor")).Maybe()
	}
	sriovOps.On("GetRepresentorPortFlavour", "pf0hpf").
		Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_PCI_PF), nil).Maybe()
	sriovOps.On("GetDevlinkPortFunctionMacAddress", "pf0hpf").
		Return(ovntest.MustParseMAC(dpuHostPFMAC), nil).Maybe()
	for rep, mac := range map[string]string{"pf0vf0": dpuHostVF0MAC, "pf0vf7": dpuHostVF7MAC} {
		sriovOps.On("GetRepresentorPortFlavour", rep).
			Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_PCI_VF), nil).Maybe()
		sriovOps.On("GetDevlinkPortFunctionMacAddress", rep).
			Return(ovntest.MustParseMAC(mac), nil).Maybe()
	}

	return defaultOVSBridgeResolver{ovsClient: ovsClient}
}

const (
	dpuHostPFMAC  = "00:73:58:6d:a1:b3"
	dpuHostVF0MAC = "00:07:3d:f2:76:4a"
	dpuHostVF7MAC = "00:07:3d:f2:76:51"
)

func TestResolveByHostMACSucceeds(t *testing.T) {
	tests := []struct {
		name           string
		hostMAC        string
		expectedBridge string
		description    string
	}{
		{
			// The Uplink selects host VF enp4s0f0v0, whose DPU representor
			// pf0vf0 sits on br-vm. Matching only PF representors used to
			// miss this entirely.
			name:           "VF representor",
			hostMAC:        dpuHostVF0MAC,
			expectedBridge: "br-vm",
			description:    "the VF0 representor pf0vf0 is attached to br-vm",
		},
		{
			name:           "correct VF on a shared bridge",
			hostMAC:        dpuHostVF7MAC,
			expectedBridge: "br-vm",
			description:    "pf0vf7 shares br-vm with pf0vf0 and the physical port p1",
		},
		{
			name:           "PF representor",
			hostMAC:        dpuHostPFMAC,
			expectedBridge: "br-host",
			description:    "the PF representor pf0hpf is attached to br-host",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			config.OvnKubeNode.Mode = ovntypes.NodeModeDPU

			resolver := newDPUBridgeResolverHarness(t)

			bridgeName, err := resolver.ResolveByHostMAC(ovntest.MustParseMAC(tt.hostMAC), "node-a")
			g.Expect(err).NotTo(gomega.HaveOccurred(), "resolving the host MAC of the %s must succeed", tt.name)
			g.Expect(bridgeName).To(gomega.Equal(tt.expectedBridge), tt.description)
		})
	}
}

func TestResolveByHostMACFallsBackToSriovnetForPF(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.OvnKubeNode.Mode = ovntypes.NodeModeDPU

	const (
		bridgeUUID    = "br-host-uuid"
		portUUID      = "pf0hpf-port-uuid"
		interfaceUUID = "pf0hpf-iface-uuid"
	)
	ovsClient, testCtx, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
			&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-host", Ports: []string{portUUID}},
			&vswitchd.Port{UUID: portUUID, Name: "pf0hpf", Interfaces: []string{interfaceUUID}},
			&vswitchd.Interface{UUID: interfaceUUID, Name: "pf0hpf", Type: "system"},
		},
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(testCtx.Cleanup)

	sriovOps := utilmocks.NewSriovnetOps(t)
	origSriovOps := util.GetSriovnetOps()
	util.SetSriovnetOpsInst(sriovOps)
	t.Cleanup(func() {
		util.SetSriovnetOpsInst(origSriovOps)
	})
	sriovOps.On("GetRepresentorPortFlavour", "pf0hpf").
		Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_PCI_PF), nil)
	// Older kernels do not report devlink port function attributes; the PF path
	// must still resolve through the sysfs aware sriovnet helper.
	sriovOps.On("GetDevlinkPortFunctionMacAddress", "pf0hpf").
		Return(nil, fmt.Errorf("devlink port has no function attributes"))
	sriovOps.On("GetRepresentorPeerMacAddress", "pf0hpf").
		Return(ovntest.MustParseMAC(dpuHostPFMAC), nil)

	bridgeName, err := (defaultOVSBridgeResolver{ovsClient: ovsClient}).
		ResolveByHostMAC(ovntest.MustParseMAC(dpuHostPFMAC), "node-a")
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"PF resolution must fall back to sriovnet when devlink reports no function attributes")
	g.Expect(bridgeName).To(gomega.Equal("br-host"), "the PF representor resolved via the fallback is attached to br-host")
}

func TestResolveByHostMACReportsBridgeNotFound(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OvnKubeNode.Mode = ovntypes.NodeModeDPU

	resolver := newDPUBridgeResolverHarness(t)

	_, err := resolver.ResolveByHostMAC(ovntest.MustParseMAC("02:00:00:00:00:99"), "node-a")
	g.Expect(err).To(gomega.HaveOccurred(), "a host MAC no representor peers with must not resolve to a bridge")
	g.Expect(discoveryReason(err)).To(gomega.Equal(uplinkv1alpha1.UplinkStateReasonBridgeNotFound),
		"the total miss must surface as BridgeNotFound")
}

func TestBridgeUplinkIgnoresHostRepresentorsOnDPU(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OvnKubeNode.Mode = ovntypes.NodeModeDPU

	resolver := newDPUBridgeResolverHarness(t)

	// br-vm holds the physical port p1 plus two VF representors, all
	// system-type in OVS. Without filtering out the representors the uplink
	// would be ambiguous and fall back to the useless "br"-prefix heuristic.
	uplinkName, err := resolver.BridgeUplink("br-vm")
	g.Expect(err).NotTo(gomega.HaveOccurred(), "deriving br-vm's uplink must succeed once VF representors are ignored")
	g.Expect(uplinkName).To(gomega.Equal("p1"), "br-vm's only non-representor system port is p1")

	uplinkName, err = resolver.BridgeUplink("br-host")
	g.Expect(err).NotTo(gomega.HaveOccurred(), "deriving br-host's uplink must succeed once the PF representor is ignored")
	g.Expect(uplinkName).To(gomega.Equal("p0"), "br-host's only non-representor system port is p0")
}

func TestBridgeUplinkRejectsRepresentorOnlyBridgeOnDPU(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.OvnKubeNode.Mode = ovntypes.NodeModeDPU

	ovsClient, testCtx, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"br-hostvf0-uuid"}},
			&vswitchd.Bridge{UUID: "br-hostvf0-uuid", Name: "br-hostvf0", Ports: []string{"pf0vf7-port-uuid"}},
			&vswitchd.Port{UUID: "pf0vf7-port-uuid", Name: "pf0vf7", Interfaces: []string{"pf0vf7-iface-uuid"}},
			&vswitchd.Interface{UUID: "pf0vf7-iface-uuid", Name: "pf0vf7", Type: "system"},
		},
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(testCtx.Cleanup)

	sriovOps := utilmocks.NewSriovnetOps(t)
	origSriovOps := util.GetSriovnetOps()
	util.SetSriovnetOpsInst(sriovOps)
	t.Cleanup(func() {
		util.SetSriovnetOpsInst(origSriovOps)
	})
	sriovOps.On("GetRepresentorPortFlavour", "pf0vf7").
		Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_PCI_VF), nil)

	// A bridge whose only system-type port is a VF representor has no physical
	// uplink; it must not silently pass validation with the representor itself
	// selected as the uplink.
	_, err = (defaultOVSBridgeResolver{ovsClient: ovsClient}).BridgeUplink("br-hostvf0")
	g.Expect(err).To(gomega.HaveOccurred(), "a representor-only bridge must not resolve an uplink")
	g.Expect(discoveryReason(err)).To(gomega.Equal(uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound),
		"the failure must surface as BridgeUplinkNotFound")
}

func TestBridgeUplinkFallsBackToExternalIDForBondPort(t *testing.T) {
	g := gomega.NewWithT(t)
	ovsClient, testCtx, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"br-bond-uuid"}},
			&vswitchd.Bridge{
				UUID: "br-bond-uuid", Name: "br-bond", Ports: []string{"bond0-port-uuid"},
				ExternalIDs: map[string]string{"bridge-uplink": "eth0"},
			},
			&vswitchd.Port{UUID: "bond0-port-uuid", Name: "bond0", Interfaces: []string{"eth0-iface-uuid", "eth1-iface-uuid"}},
			&vswitchd.Interface{UUID: "eth0-iface-uuid", Name: "eth0", Type: "system"},
			&vswitchd.Interface{UUID: "eth1-iface-uuid", Name: "eth1", Type: "system"},
		},
	})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "failed to build the OVS test harness for the bond bridge")
	t.Cleanup(testCtx.Cleanup)

	// bond0 is the bridge's only system-type port, but it has no same-named
	// Interface row, so it cannot be the uplink itself; the resolver must
	// fall back to the bridge-uplink external-id.
	uplinkName, err := (defaultOVSBridgeResolver{ovsClient: ovsClient}).BridgeUplink("br-bond")
	g.Expect(err).NotTo(gomega.HaveOccurred(), "a bond bridge with bridge-uplink set must resolve")
	g.Expect(uplinkName).To(gomega.Equal("eth0"), "the bridge-uplink external-id must be used for a bond port")
}

func TestNodeUplinkControllerPublishesResolvedState(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	controller, client := newTestController(t,
		fakeHostDiscoverer{state: &hostInterfaceState{
			macAddress: net.HardwareAddr{0x02, 0x42, 0xac, 0x12, 0x00, 0x02},
			ipAddresses: []*net.IPNet{
				ovntest.MustParseIPNet("192.0.2.10/24"),
			},
			defaultGateways: []net.IP{ovntest.MustParseIP("192.0.2.1")},
		}},
		fakeBridgeResolver{bridgeName: "br-blue", bridgeUplink: "eth0"},
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "breth0"),
		newUplinkState("br-blue.node-a", "br-blue", "node-a"),
	)

	g.Expect(controller.reconcileUplinkState("br-blue.node-a")).To(gomega.Succeed())

	state := getUplinkState(g, client, "br-blue.node-a")
	g.Expect(state.Status.Type).To(gomega.Equal(uplinkv1alpha1.UplinkTypeOVSBridge))
	g.Expect(state.Status.HostInterfaceName).To(gomega.Equal(uplinkv1alpha1.InterfaceName("breth0")))
	g.Expect(state.Status.MACAddress).To(gomega.Equal(uplinkv1alpha1.MACAddress("02:42:ac:12:00:02")))
	g.Expect(state.Status.IPAddresses).To(gomega.Equal([]uplinkv1alpha1.IPAddressCIDR{"192.0.2.10/24"}))
	g.Expect(state.Status.DefaultGateways).To(gomega.Equal([]uplinkv1alpha1.IPAddress{"192.0.2.1"}))
	g.Expect(state.Status.OVSBridge).NotTo(gomega.BeNil())
	g.Expect(state.Status.OVSBridge.Name).To(gomega.Equal("br-blue"))
	g.Expect(state.Status.Conditions).To(gomega.ContainElement(gomega.And(
		gomega.HaveField("Type", uplinkv1alpha1.UplinkStateConditionResolved),
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", uplinkv1alpha1.UplinkStateReasonResolved),
	)))
}

func TestNodeUplinkControllerRejectsDefaultGatewayBridge(t *testing.T) {
	tests := []struct {
		name string
		mode string
	}{
		{
			name: "Full",
			mode: ovntypes.NodeModeFull,
		},
		{
			// The DPU-host does not validate the bridge: the DPU side owns
			// bridge validation and the Resolved condition.
			name: "DPU",
			mode: ovntypes.NodeModeDPU,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			config.OvnKubeNode.Mode = test.mode

			state := newUplinkState("br-blue.node-a", "br-blue", "node-a")
			if test.mode == ovntypes.NodeModeDPU {
				state = newHostResolvedUplinkState("br-blue.node-a", "br-blue", "node-a", "uplink0")
			}

			controller, client := newTestController(t,
				fakeHostDiscoverer{state: newStatusTestHostState()},
				fakeBridgeResolver{bridgeName: "br-default"},
				newNodeWithDefaultGatewayBridge("node-a", map[string]string{"role": "blue"}, "br-default"),
				newUplink("br-blue", "role", "blue", "uplink0"),
				state,
			)

			g.Expect(controller.reconcileUplinkState("br-blue.node-a")).To(gomega.Succeed())

			state = getUplinkState(g, client, "br-blue.node-a")
			g.Expect(state.Status.OVSBridge).NotTo(gomega.BeNil())
			g.Expect(state.Status.OVSBridge.Name).To(gomega.Equal("br-default"))
			g.Expect(state.Status.Conditions).To(gomega.ContainElement(gomega.And(
				gomega.HaveField("Type", uplinkv1alpha1.UplinkStateConditionResolved),
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", uplinkv1alpha1.UplinkStateReasonDefaultGatewayBridgeUnsupported),
				gomega.HaveField("Message", gomega.ContainSubstring("default shared gateway bridge br-default")),
			)))
		})
	}
}

func TestNodeUplinkControllerSkipsUnchangedStatus(t *testing.T) {
	tests := []struct {
		name          string
		mode          string
		bridgeName    string
		conditionType string
		reason        string
		message       string
	}{
		{
			name:          "Full",
			mode:          ovntypes.NodeModeFull,
			bridgeName:    "br-blue",
			conditionType: uplinkv1alpha1.UplinkStateConditionResolved,
			reason:        uplinkv1alpha1.UplinkStateReasonResolved,
			message:       "Uplink discovery succeeded",
		},
		{
			name:          "DPU host preserves DPU bridge",
			mode:          ovntypes.NodeModeDPUHost,
			conditionType: uplinkv1alpha1.UplinkStateConditionHostDataReady,
			reason:        uplinkv1alpha1.UplinkStateReasonHostDataDiscovered,
			message:       "Uplink host interface data discovered",
		},
		{
			name:          "DPU preserves host gateway data",
			mode:          ovntypes.NodeModeDPU,
			bridgeName:    "br-blue",
			conditionType: uplinkv1alpha1.UplinkStateConditionResolved,
			reason:        uplinkv1alpha1.UplinkStateReasonResolved,
			message:       "Uplink DPU bridge discovery succeeded",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			config.OvnKubeNode.Mode = test.mode
			state := newResolvedStatusTestUplinkState(test.conditionType, test.reason, test.message)
			controller, client := newTestController(t, fakeHostDiscoverer{}, fakeBridgeResolver{}, state)

			g.Expect(controller.updateUplinkStateStatus(
				state,
				"breth0",
				newStatusTestHostState(),
				test.bridgeName,
				metav1.ConditionTrue,
				test.reason,
				test.message,
			)).To(gomega.Succeed())
			g.Expect(client.UplinkClient.(*uplinkfake.Clientset).Actions()).To(gomega.BeEmpty())
		})
	}
}

func TestNodeUplinkControllerAppliesOwnedStatusClears(t *testing.T) {
	tests := []struct {
		name          string
		mode          string
		hostState     *hostInterfaceState
		bridgeName    string
		conditionType string
		reason        string
		message       string
	}{
		{
			name:          "Full",
			mode:          ovntypes.NodeModeFull,
			conditionType: uplinkv1alpha1.UplinkStateConditionResolved,
			reason:        uplinkv1alpha1.UplinkStateReasonResolved,
			message:       "Uplink discovery succeeded",
		},
		{
			name:          "DPU host clears host gateway data",
			mode:          ovntypes.NodeModeDPUHost,
			conditionType: uplinkv1alpha1.UplinkStateConditionHostDataReady,
			reason:        uplinkv1alpha1.UplinkStateReasonHostDataDiscovered,
			message:       "Uplink host interface data discovered",
		},
		{
			name:          "DPU clears bridge data",
			mode:          ovntypes.NodeModeDPU,
			hostState:     newStatusTestHostState(),
			conditionType: uplinkv1alpha1.UplinkStateConditionResolved,
			reason:        uplinkv1alpha1.UplinkStateReasonResolved,
			message:       "Uplink DPU bridge discovery succeeded",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			config.OvnKubeNode.Mode = test.mode
			state := newResolvedStatusTestUplinkState(test.conditionType, test.reason, test.message)
			controller, client := newTestController(t, fakeHostDiscoverer{}, fakeBridgeResolver{}, state)

			g.Expect(controller.updateUplinkStateStatus(
				state,
				"breth0",
				test.hostState,
				test.bridgeName,
				metav1.ConditionTrue,
				test.reason,
				test.message,
			)).To(gomega.Succeed())
			actions := client.UplinkClient.(*uplinkfake.Clientset).Actions()
			g.Expect(actions).To(gomega.HaveLen(1))
			g.Expect(actions[0].GetVerb()).To(gomega.Equal("patch"))
			g.Expect(actions[0].GetResource().Resource).To(gomega.Equal("uplinkstates"))
		})
	}
}

func TestNodeUplinkControllerPreservesGatewayReadyCondition(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())

	state := newUplinkState("br-blue.node-a", "br-blue", "node-a")
	state.Status.Type = uplinkv1alpha1.UplinkTypeOVSBridge
	state.Status.HostInterfaceName = uplinkv1alpha1.InterfaceName("breth0")
	state.Status.Conditions = []metav1.Condition{
		{
			Type:    uplinkv1alpha1.UplinkStateConditionResolved,
			Status:  metav1.ConditionFalse,
			Reason:  uplinkv1alpha1.UplinkStateReasonBridgeNotFound,
			Message: "could not resolve Uplink bridge",
		},
		{
			Type:    uplinkv1alpha1.UplinkStateConditionGatewayReady,
			Status:  metav1.ConditionFalse,
			Reason:  uplinkv1alpha1.UplinkStateReasonVRFAttachmentFailed,
			Message: "could not add Uplink gateway interface to VRF",
		},
	}

	controller, client := newTestController(t,
		fakeHostDiscoverer{state: &hostInterfaceState{
			macAddress: net.HardwareAddr{0x02, 0x42, 0xac, 0x12, 0x00, 0x02},
			ipAddresses: []*net.IPNet{
				ovntest.MustParseIPNet("192.0.2.10/24"),
			},
			defaultGateways: []net.IP{ovntest.MustParseIP("192.0.2.1")},
		}},
		fakeBridgeResolver{bridgeName: "br-blue", bridgeUplink: "eth0"},
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "breth0"),
		state,
	)

	g.Expect(controller.reconcileUplinkState("br-blue.node-a")).To(gomega.Succeed())

	state = getUplinkState(g, client, "br-blue.node-a")
	g.Expect(state.Status.OVSBridge.Name).To(gomega.Equal("br-blue"))
	g.Expect(state.Status.Conditions).To(gomega.ContainElement(gomega.And(
		gomega.HaveField("Type", uplinkv1alpha1.UplinkStateConditionResolved),
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", uplinkv1alpha1.UplinkStateReasonResolved),
	)))
	g.Expect(state.Status.Conditions).To(gomega.ContainElement(gomega.And(
		gomega.HaveField("Type", uplinkv1alpha1.UplinkStateConditionGatewayReady),
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", uplinkv1alpha1.UplinkStateReasonVRFAttachmentFailed),
	)))
}

func TestNodeUplinkControllerReportsHostInterfaceFailure(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	controller, client := newTestController(t,
		fakeHostDiscoverer{
			err: newDiscoveryError(
				uplinkv1alpha1.UplinkStateReasonHostInterfaceNotFound,
				fmt.Errorf("host interface missing"),
			),
		},
		fakeBridgeResolver{},
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "breth0"),
		newUplinkState("br-blue.node-a", "br-blue", "node-a"),
	)

	g.Expect(controller.reconcileUplinkState("br-blue.node-a")).To(gomega.Succeed())

	state := getUplinkState(g, client, "br-blue.node-a")
	g.Expect(state.Status.Conditions).To(gomega.ContainElement(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", uplinkv1alpha1.UplinkStateReasonHostInterfaceNotFound),
	)))
}

func TestNodeUplinkControllerReportsBridgeUplinkFailure(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	controller, client := newTestController(t,
		fakeHostDiscoverer{state: &hostInterfaceState{
			macAddress:  net.HardwareAddr{0x02, 0x42, 0xac, 0x12, 0x00, 0x02},
			ipAddresses: []*net.IPNet{ovntest.MustParseIPNet("192.0.2.10/24")},
		}},
		fakeBridgeResolver{
			bridgeName: "br-blue",
			bridgeUplinkErr: newDiscoveryError(
				uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound,
				fmt.Errorf("missing bridge uplink"),
			),
		},
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "breth0"),
		newUplinkState("br-blue.node-a", "br-blue", "node-a"),
	)

	g.Expect(controller.reconcileUplinkState("br-blue.node-a")).To(gomega.Succeed())

	state := getUplinkState(g, client, "br-blue.node-a")
	g.Expect(state.Status.OVSBridge.Name).To(gomega.Equal("br-blue"))
	g.Expect(state.Status.Conditions).To(gomega.ContainElement(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound),
	)))
}

func TestNodeUplinkControllerRejectsBridgeUplinkAsHostInterface(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	controller, client := newTestController(t,
		fakeHostDiscoverer{state: &hostInterfaceState{
			macAddress:  net.HardwareAddr{0x02, 0x42, 0xac, 0x12, 0x00, 0x02},
			ipAddresses: []*net.IPNet{ovntest.MustParseIPNet("192.0.2.10/24")},
		}},
		fakeBridgeResolver{bridgeName: "br-blue", bridgeUplink: "eth1"},
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "eth1"),
		newUplinkState("br-blue.node-a", "br-blue", "node-a"),
	)

	g.Expect(controller.reconcileUplinkState("br-blue.node-a")).To(gomega.Succeed())

	state := getUplinkState(g, client, "br-blue.node-a")
	g.Expect(state.Status.HostInterfaceName).To(gomega.Equal(uplinkv1alpha1.InterfaceName("eth1")))
	g.Expect(state.Status.OVSBridge.Name).To(gomega.Equal("br-blue"))
	g.Expect(state.Status.Conditions).To(gomega.ContainElement(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", uplinkv1alpha1.UplinkStateReasonInvalidHostInterface),
	)))
}

func TestNodeUplinkControllerCreatesSelectedNodeState(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	controller, client := newTestController(t,
		fakeHostDiscoverer{},
		fakeBridgeResolver{},
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "breth0"),
	)

	g.Expect(controller.reconcileUplink("br-blue")).To(gomega.Succeed())

	state := getUplinkState(g, client, uplinkutil.StateName("br-blue", "node-a"))
	g.Expect(state.Labels).To(gomega.BeEmpty())
	g.Expect(state.Annotations).To(gomega.BeEmpty())
	g.Expect(state.OwnerReferences).To(gomega.HaveLen(1))
	g.Expect(state.Spec.UplinkName).To(gomega.Equal("br-blue"))
	g.Expect(state.Spec.NodeName).To(gomega.Equal("node-a"))
	g.Expect(state.Status.Type).To(gomega.Equal(uplinkv1alpha1.UplinkTypeOVSBridge))
	g.Expect(state.Status.HostInterfaceName).To(gomega.Equal(uplinkv1alpha1.InterfaceName("breth0")))
}

func TestNodeUplinkControllerCreatesOverlappingNodeStateWithInitialStatus(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	uplink := newUplink("br-blue", "role", "blue", "breth0")
	uplink.Spec.NodeConfigs = append(uplink.Spec.NodeConfigs, uplink.Spec.NodeConfigs[0])
	controller, client := newTestController(t,
		fakeHostDiscoverer{},
		fakeBridgeResolver{},
		newNode("node-a", map[string]string{"role": "blue"}),
		uplink,
	)

	g.Expect(controller.reconcileUplink("br-blue")).To(gomega.Succeed())

	state := getUplinkState(g, client, uplinkutil.StateName("br-blue", "node-a"))
	g.Expect(state.Spec.UplinkName).To(gomega.Equal("br-blue"))
	g.Expect(state.Spec.NodeName).To(gomega.Equal("node-a"))
	g.Expect(state.Status.Conditions).To(gomega.ContainElement(gomega.And(
		gomega.HaveField("Type", uplinkv1alpha1.UplinkStateConditionResolved),
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", uplinkv1alpha1.UplinkStateReasonNodeSelectorOverlap),
		gomega.HaveField("LastTransitionTime", gomega.Not(gomega.Equal(metav1.Time{}))),
	)))
}

func TestNodeUplinkControllerDeletesUnselectedNodeState(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	stateName := uplinkutil.StateName("br-blue", "node-a")
	controller, client := newTestController(t,
		fakeHostDiscoverer{},
		fakeBridgeResolver{},
		newNode("node-a", map[string]string{"role": "red"}),
		newUplink("br-blue", "role", "blue", "breth0"),
		newUplinkState(stateName, "br-blue", "node-a"),
	)

	g.Expect(controller.reconcileUplinkState(stateName)).To(gomega.Succeed())

	_, err := client.UplinkClient.K8sV1alpha1().UplinkStates().Get(
		context.Background(), stateName, metav1.GetOptions{})
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
}

func TestEnsureUplinkStateGetsExistingObjectAfterCreateRace(t *testing.T) {
	g := gomega.NewWithT(t)
	uplink := newUplink("br-blue", "role", "blue", "breth0")
	controller, client := newTestController(t, fakeHostDiscoverer{}, fakeBridgeResolver{})
	name := uplinkutil.StateName(uplink.Name, "node-a")
	nodeConfig := &uplink.Spec.NodeConfigs[0]
	existing := desiredUplinkState(uplink, "node-a", name, nodeConfig, nil)

	_, err := client.UplinkClient.K8sV1alpha1().UplinkStates().Create(
		context.Background(),
		existing,
		metav1.CreateOptions{},
	)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	fakeClient := client.UplinkClient.(*uplinkfake.Clientset)
	fakeClient.ClearActions()

	state, err := controller.ensureUplinkState(uplink, nodeConfig, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(state.Spec.UplinkName).To(gomega.Equal("br-blue"))
	g.Expect(state.Spec.NodeName).To(gomega.Equal("node-a"))
	actions := fakeClient.Actions()
	g.Expect(actions).To(gomega.HaveLen(2))
	g.Expect(actions[0].GetVerb()).To(gomega.Equal("create"))
	g.Expect(actions[1].GetVerb()).To(gomega.Equal("get"))
}

func TestNodeUplinkControllerIgnoresMismatchedStateIdentity(t *testing.T) {
	g := gomega.NewWithT(t)
	uplink := newUplink("br-blue", "role", "blue", "breth0")
	name := uplinkutil.StateName(uplink.Name, "node-a")
	controller, client := newTestController(
		t,
		fakeHostDiscoverer{},
		fakeBridgeResolver{},
		newNode("node-a", map[string]string{"role": "blue"}),
		uplink,
		newUplinkState(name, "br-red", "node-a"),
	)
	fakeClient := client.UplinkClient.(*uplinkfake.Clientset)
	fakeClient.ClearActions()

	g.Expect(controller.reconcileUplink(uplink.Name)).To(gomega.Succeed())
	g.Expect(fakeClient.Actions()).To(gomega.BeEmpty())
	g.Expect(controller.uplinkStateController.(*controllerutil.FakeController).Reconciles).To(gomega.BeEmpty())
}

func TestNodeUplinkControllerDPUHostPublishesHostData(t *testing.T) {
	tests := []struct {
		name                string
		ipAddresses         []*net.IPNet
		expectedIPAddresses []uplinkv1alpha1.IPAddressCIDR
	}{
		{
			name:                "single stack",
			ipAddresses:         []*net.IPNet{ovntest.MustParseIPNet("192.0.2.11/24")},
			expectedIPAddresses: []uplinkv1alpha1.IPAddressCIDR{"192.0.2.11/24"},
		},
		{
			name: "dual stack",
			ipAddresses: []*net.IPNet{
				ovntest.MustParseIPNet("192.0.2.11/24"),
				ovntest.MustParseIPNet("2001:db8::11/64"),
			},
			expectedIPAddresses: []uplinkv1alpha1.IPAddressCIDR{"192.0.2.11/24", "2001:db8::11/64"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			config.OvnKubeNode.Mode = ovntypes.NodeModeDPUHost

			controller, client := newTestController(t,
				fakeHostDiscoverer{state: &hostInterfaceState{
					macAddress:  net.HardwareAddr{0x02, 0x42, 0xac, 0x12, 0x00, 0x03},
					ipAddresses: test.ipAddresses,
				}},
				failingBridgeResolver{t: t},
				newNode("node-a", map[string]string{"role": "blue"}),
				newUplink("br-blue", "role", "blue", "breth0"),
				newUplinkState("br-blue.node-a", "br-blue", "node-a"),
			)

			g.Expect(controller.reconcileUplinkState("br-blue.node-a")).To(gomega.Succeed())

			state := getUplinkState(g, client, "br-blue.node-a")
			g.Expect(state.Status.OVSBridge).To(gomega.BeNil())
			g.Expect(state.Status.MACAddress).To(gomega.Equal(uplinkv1alpha1.MACAddress("02:42:ac:12:00:03")))
			g.Expect(state.Status.IPAddresses).To(gomega.Equal(test.expectedIPAddresses))
			g.Expect(state.Status.Conditions).To(gomega.ContainElement(gomega.And(
				gomega.HaveField("Type", uplinkv1alpha1.UplinkStateConditionHostDataReady),
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", uplinkv1alpha1.UplinkStateReasonHostDataDiscovered),
			)))
			// The DPU side is the only writer of the Resolved condition.
			g.Expect(meta.FindStatusCondition(
				state.Status.Conditions,
				uplinkv1alpha1.UplinkStateConditionResolved,
			)).To(gomega.BeNil())
		})
	}
}

func TestNodeUplinkControllerDPUHostLeavesResolvedToDPU(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OvnKubeNode.Mode = ovntypes.NodeModeDPUHost

	// A DPU-authored bridge discovery failure is already recorded: the
	// DPU-host publishes its data and HostDataReady without touching it.
	state := newUplinkState("br-blue.node-a", "br-blue", "node-a")
	state.Status.Type = uplinkv1alpha1.UplinkTypeOVSBridge
	state.Status.HostInterfaceName = "breth0"
	state.Status.OVSBridge = &uplinkv1alpha1.OVSBridgeStatus{Name: "br-blue"}
	state.Status.Conditions = []metav1.Condition{
		{
			Type:               uplinkv1alpha1.UplinkStateConditionResolved,
			Status:             metav1.ConditionFalse,
			Reason:             uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound,
			Message:            "failed to resolve physical uplink for OVS bridge br-blue",
			LastTransitionTime: metav1.NewTime(time.Unix(1, 0)),
		},
	}

	hostDiscoverer := fakeHostDiscoverer{state: &hostInterfaceState{
		macAddress:  net.HardwareAddr{0x02, 0x42, 0xac, 0x12, 0x00, 0x03},
		ipAddresses: []*net.IPNet{ovntest.MustParseIPNet("192.0.2.11/24")},
	}}
	node := newNode("node-a", map[string]string{"role": "blue"})
	uplink := newUplink("br-blue", "role", "blue", "breth0")
	controller, client := newTestController(t, hostDiscoverer, failingBridgeResolver{t: t}, node, uplink, state)

	g.Expect(controller.reconcileUplinkState("br-blue.node-a")).To(gomega.Succeed())

	state = getUplinkState(g, client, "br-blue.node-a")
	g.Expect(state.Status.MACAddress).To(gomega.Equal(uplinkv1alpha1.MACAddress("02:42:ac:12:00:03")))
	g.Expect(state.Status.Conditions).To(gomega.ContainElement(gomega.And(
		gomega.HaveField("Type", uplinkv1alpha1.UplinkStateConditionResolved),
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound),
		gomega.HaveField("Message", "failed to resolve physical uplink for OVS bridge br-blue"),
	)))
	g.Expect(state.Status.Conditions).To(gomega.ContainElement(gomega.And(
		gomega.HaveField("Type", uplinkv1alpha1.UplinkStateConditionHostDataReady),
		gomega.HaveField("Status", metav1.ConditionTrue),
	)))

	// A reconcile of the published status must not write again.
	controller, client = newTestController(t, hostDiscoverer, failingBridgeResolver{t: t}, node, uplink, state)
	g.Expect(controller.reconcileUplinkState("br-blue.node-a")).To(gomega.Succeed())
	g.Expect(client.UplinkClient.(*uplinkfake.Clientset).Actions()).To(gomega.BeEmpty())
}

func TestNodeUplinkControllerDPUHostIgnoresStaleDPUBridge(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OvnKubeNode.Mode = ovntypes.NodeModeDPUHost

	state := newHostResolvedUplinkState("br-blue.node-a", "br-blue", "node-a", "breth1")
	state.Status.OVSBridge = &uplinkv1alpha1.OVSBridgeStatus{Name: "br-old"}

	controller, client := newTestController(t,
		fakeHostDiscoverer{state: &hostInterfaceState{
			macAddress:  net.HardwareAddr{0x02, 0x42, 0xac, 0x12, 0x00, 0x05},
			ipAddresses: []*net.IPNet{ovntest.MustParseIPNet("192.0.2.13/24")},
		}},
		failingBridgeResolver{t: t},
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "breth1"),
		state,
	)

	g.Expect(controller.reconcileUplinkState("br-blue.node-a")).To(gomega.Succeed())

	state = getUplinkState(g, client, "br-blue.node-a")
	g.Expect(state.Status.HostInterfaceName).To(gomega.Equal(uplinkv1alpha1.InterfaceName("breth1")))
	// The stale DPU-owned bridge is left for the DPU side to refresh.
	g.Expect(state.Status.OVSBridge).NotTo(gomega.BeNil())
	g.Expect(state.Status.OVSBridge.Name).To(gomega.Equal("br-old"))
	g.Expect(state.Status.Conditions).To(gomega.ContainElement(gomega.And(
		gomega.HaveField("Type", uplinkv1alpha1.UplinkStateConditionHostDataReady),
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", uplinkv1alpha1.UplinkStateReasonHostDataDiscovered),
	)))
}

func TestNodeUplinkControllerDPUDoesNotAdoptStaleHostState(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OvnKubeNode.Mode = ovntypes.NodeModeDPU

	// The Uplink spec changed breth0->breth1 but the DPU-host has not
	// republished yet: status still carries breth0 and its MAC. The DPU must
	// not bump status.hostInterfaceName itself, or the next reconcile would
	// validate the stale MAC as belonging to breth1 and resolve the old
	// bridge.
	node := newNode("node-a", map[string]string{"role": "blue"})
	uplink := newUplink("br-blue", "role", "blue", "breth1")
	state := newHostResolvedUplinkState("br-blue.node-a", "br-blue", "node-a", "breth0")
	hostDiscoverer := fakeHostDiscoverer{err: fmt.Errorf("must not inspect host interface")}
	bridgeResolver := fakeBridgeResolver{bridgeName: "br-old", bridgeUplink: "p0"}

	controller, client := newTestController(t, hostDiscoverer, bridgeResolver, node, uplink, state)

	g.Expect(controller.reconcileUplinkState("br-blue.node-a")).To(gomega.Succeed())

	state = getUplinkState(g, client, "br-blue.node-a")
	g.Expect(state.Status.HostInterfaceName).To(gomega.Equal(uplinkv1alpha1.InterfaceName("breth0")))
	g.Expect(state.Status.Conditions).To(gomega.ContainElement(gomega.And(
		gomega.HaveField("Type", uplinkv1alpha1.UplinkStateConditionResolved),
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", uplinkv1alpha1.UplinkStateReasonWaitingForDPUHost),
		gomega.HaveField("Message", gomega.ContainSubstring("waiting for DPU-host state for interface breth1")),
	)))

	// A second reconcile on the published state must be a no-op, not a
	// resolve of br-old against the stale breth0 MAC.
	controller, client = newTestController(t, hostDiscoverer, bridgeResolver, node, uplink, state)
	g.Expect(controller.reconcileUplinkState("br-blue.node-a")).To(gomega.Succeed())
	g.Expect(client.UplinkClient.(*uplinkfake.Clientset).Actions()).To(gomega.BeEmpty())
}

func TestNodeUplinkControllerDPULeavesHostDataReadyToDPUHost(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OvnKubeNode.Mode = ovntypes.NodeModeDPU

	// Host discovery failed on the DPU-host: the root cause lives in the
	// host-owned HostDataReady condition, and the blocked DPU reports its
	// own state on Resolved without touching it.
	state := newUplinkState("br-blue.node-a", "br-blue", "node-a")
	state.Status.Type = uplinkv1alpha1.UplinkTypeOVSBridge
	state.Status.HostInterfaceName = "breth0"
	state.Status.Conditions = []metav1.Condition{
		{
			Type:               uplinkv1alpha1.UplinkStateConditionHostDataReady,
			Status:             metav1.ConditionFalse,
			Reason:             uplinkv1alpha1.UplinkStateReasonHostInterfaceNotFound,
			Message:            "host interface breth0 was not found: link not found",
			LastTransitionTime: metav1.NewTime(time.Unix(1, 0)),
		},
	}

	controller, client := newTestController(t,
		fakeHostDiscoverer{err: fmt.Errorf("must not inspect host interface")},
		fakeBridgeResolver{bridgeName: "br-blue", bridgeUplink: "p0"},
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "breth0"),
		state,
	)

	g.Expect(controller.reconcileUplinkState("br-blue.node-a")).To(gomega.Succeed())

	state = getUplinkState(g, client, "br-blue.node-a")
	g.Expect(state.Status.Conditions).To(gomega.ContainElement(gomega.And(
		gomega.HaveField("Type", uplinkv1alpha1.UplinkStateConditionHostDataReady),
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", uplinkv1alpha1.UplinkStateReasonHostInterfaceNotFound),
		gomega.HaveField("Message", "host interface breth0 was not found: link not found"),
	)))
	g.Expect(state.Status.Conditions).To(gomega.ContainElement(gomega.And(
		gomega.HaveField("Type", uplinkv1alpha1.UplinkStateConditionResolved),
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", uplinkv1alpha1.UplinkStateReasonWaitingForDPUHost),
	)))
}

func TestNodeUplinkControllerDPUUsesHostState(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OvnKubeNode.Mode = ovntypes.NodeModeDPU

	controller, client := newTestController(t,
		fakeHostDiscoverer{err: fmt.Errorf("must not inspect host interface")},
		fakeBridgeResolver{bridgeName: "br-blue", bridgeUplink: "p0"},
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "breth0"),
		newHostResolvedUplinkState("br-blue.node-a", "br-blue", "node-a", "breth0"),
	)

	g.Expect(controller.reconcileUplinkState("br-blue.node-a")).To(gomega.Succeed())

	state := getUplinkState(g, client, "br-blue.node-a")
	g.Expect(state.Status.HostInterfaceName).To(gomega.Equal(uplinkv1alpha1.InterfaceName("breth0")))
	g.Expect(state.Status.MACAddress).To(gomega.Equal(uplinkv1alpha1.MACAddress("02:42:ac:12:00:04")))
	g.Expect(state.Status.IPAddresses).To(gomega.Equal([]uplinkv1alpha1.IPAddressCIDR{"192.0.2.12/24"}))
	g.Expect(state.Status.DefaultGateways).To(gomega.Equal([]uplinkv1alpha1.IPAddress{"192.0.2.1"}))
	g.Expect(state.Status.OVSBridge.Name).To(gomega.Equal("br-blue"))
	g.Expect(state.Status.Conditions).To(gomega.ContainElement(gomega.And(
		gomega.HaveField("Type", uplinkv1alpha1.UplinkStateConditionResolved),
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", uplinkv1alpha1.UplinkStateReasonResolved),
	)))
}

func newTestController(
	t *testing.T,
	hostDiscoverer hostInterfaceDiscoverer,
	bridgeResolver ovsBridgeResolver,
	objects ...runtime.Object,
) (*Controller, *util.OVNNodeClientset) {
	t.Helper()

	g := gomega.NewWithT(t)

	client := util.GetOVNClientset(objects...).GetNodeClientset()
	ovntest.AddUplinkApplyReactor(client.UplinkClient.(*uplinkfake.Clientset))

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	uplinkIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	uplinkStateIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, obj := range objects {
		switch typed := obj.(type) {
		case *corev1.Node:
			g.Expect(nodeIndexer.Add(typed)).To(gomega.Succeed())
		case *uplinkv1alpha1.Uplink:
			g.Expect(uplinkIndexer.Add(typed)).To(gomega.Succeed())
		case *uplinkv1alpha1.UplinkState:
			g.Expect(uplinkStateIndexer.Add(typed)).To(gomega.Succeed())
		}
	}

	return &Controller{
		nodeName:              "node-a",
		uplinkClient:          client.UplinkClient,
		uplinkLister:          uplinklisters.NewUplinkLister(uplinkIndexer),
		uplinkStateLister:     uplinklisters.NewUplinkStateLister(uplinkStateIndexer),
		nodeLister:            corelisters.NewNodeLister(nodeIndexer),
		hostDiscoverer:        hostDiscoverer,
		bridgeResolver:        bridgeResolver,
		uplinkController:      &controllerutil.FakeController{},
		uplinkStateController: &controllerutil.FakeController{},
	}, client
}

func newNode(name string, nodeLabels map[string]string) *corev1.Node {
	return newNodeWithDefaultGatewayBridge(name, nodeLabels, "breth0")
}

func newNodeWithDefaultGatewayBridge(name string, nodeLabels map[string]string, bridgeName string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				util.OvnNodeChassisID: "chassis-id",
				util.OvnNodeL3GatewayConfig: fmt.Sprintf(
					`{"default":{"mode":"shared","bridge-id":%q,"interface-id":"breth0_node-a",`+
						`"mac-address":"02:42:ac:12:00:01","ip-addresses":["192.0.2.2/24"],`+
						`"next-hops":["192.0.2.1"],"node-port-enable":"true"}}`,
					bridgeName,
				),
			},
			Labels: nodeLabels,
		},
	}
}

func newUplink(name string, selectorKey string, selectorValue string, hostInterfaceName string) *uplinkv1alpha1.Uplink {
	return &uplinkv1alpha1.Uplink{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: uplinkv1alpha1.UplinkSpec{
			NodeConfigs: []uplinkv1alpha1.UplinkNodeConfig{
				{
					Type: uplinkv1alpha1.UplinkTypeOVSBridge,
					NodeSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{selectorKey: selectorValue},
					},
					HostInterfaceName: uplinkv1alpha1.InterfaceName(hostInterfaceName),
				},
			},
		},
	}
}

func newUplinkState(name, uplinkName, nodeName string) *uplinkv1alpha1.UplinkState {
	return &uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: uplinkv1alpha1.UplinkStateSpec{
			UplinkName: uplinkName,
			NodeName:   nodeName,
		},
	}
}

func newHostResolvedUplinkState(name, uplinkName, nodeName, hostInterfaceName string) *uplinkv1alpha1.UplinkState {
	state := newUplinkState(name, uplinkName, nodeName)
	state.Status.Type = uplinkv1alpha1.UplinkTypeOVSBridge
	state.Status.HostInterfaceName = uplinkv1alpha1.InterfaceName(hostInterfaceName)
	state.Status.MACAddress = uplinkv1alpha1.MACAddress("02:42:ac:12:00:04")
	state.Status.IPAddresses = []uplinkv1alpha1.IPAddressCIDR{"192.0.2.12/24"}
	state.Status.DefaultGateways = []uplinkv1alpha1.IPAddress{"192.0.2.1"}
	state.Status.Conditions = []metav1.Condition{
		{
			Type:               uplinkv1alpha1.UplinkStateConditionHostDataReady,
			Status:             metav1.ConditionTrue,
			Reason:             uplinkv1alpha1.UplinkStateReasonHostDataDiscovered,
			Message:            "Uplink host interface data discovered",
			LastTransitionTime: metav1.NewTime(time.Unix(1, 0)),
		},
	}
	return state
}

func newResolvedStatusTestUplinkState(conditionType, reason, message string) *uplinkv1alpha1.UplinkState {
	state := newHostResolvedUplinkState("br-blue.node-a", "br-blue", "node-a", "breth0")
	state.Status.OVSBridge = &uplinkv1alpha1.OVSBridgeStatus{Name: "br-blue"}
	state.Status.Conditions = []metav1.Condition{
		{
			Type:               conditionType,
			Status:             metav1.ConditionTrue,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.NewTime(time.Unix(1, 0)),
		},
	}
	return state
}

func newStatusTestHostState() *hostInterfaceState {
	return &hostInterfaceState{
		macAddress:      net.HardwareAddr{0x02, 0x42, 0xac, 0x12, 0x00, 0x04},
		ipAddresses:     []*net.IPNet{ovntest.MustParseIPNet("192.0.2.12/24")},
		defaultGateways: []net.IP{ovntest.MustParseIP("192.0.2.1")},
	}
}

func getUplinkState(g gomega.Gomega, client *util.OVNNodeClientset, name string) *uplinkv1alpha1.UplinkState {
	state, err := client.UplinkClient.K8sV1alpha1().UplinkStates().Get(
		context.Background(),
		name,
		metav1.GetOptions{},
	)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	return state
}

type fakeHostDiscoverer struct {
	state *hostInterfaceState
	err   error
}

func (d fakeHostDiscoverer) Discover(_ string) (*hostInterfaceState, error) {
	return d.state, d.err
}

// failingBridgeResolver pins the invariant that the DPU-host side never
// queries local OVS: it runs no OVS, the bridge is resolved on the DPU.
type failingBridgeResolver struct {
	t *testing.T
}

func (r failingBridgeResolver) fail() error {
	r.t.Helper()
	err := fmt.Errorf("DPU-host must not query local OVS")
	r.t.Error(err)
	return err
}

func (r failingBridgeResolver) Resolve(string) (string, error) {
	return "", r.fail()
}

func (r failingBridgeResolver) ResolveByHostMAC(net.HardwareAddr, string) (string, error) {
	return "", r.fail()
}

func (r failingBridgeResolver) BridgeUplink(string) (string, error) {
	return "", r.fail()
}

type fakeBridgeResolver struct {
	bridgeName      string
	bridgeUplink    string
	err             error
	bridgeUplinkErr error
}

func (r fakeBridgeResolver) Resolve(_ string) (string, error) {
	return r.bridgeName, r.err
}

func (r fakeBridgeResolver) ResolveByHostMAC(_ net.HardwareAddr, _ string) (string, error) {
	return r.bridgeName, r.err
}

func (r fakeBridgeResolver) BridgeUplink(_ string) (string, error) {
	return r.bridgeUplink, r.bridgeUplinkErr
}
