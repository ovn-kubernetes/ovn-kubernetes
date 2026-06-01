// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package uplink

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
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
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
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

func TestOVSPortBridgeResolverResolvesSmartNICRepresentor(t *testing.T) {
	g := gomega.NewWithT(t)
	fexec := ovntest.NewFakeExec()
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: "ovs-vsctl --timeout=15 --if-exists get Bridge pf0vf1 name",
	})
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

	bridgeName, err := ovsPortBridgeResolver{}.Resolve("pf0vf1")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(bridgeName).To(gomega.Equal("ovsbr1"))
	g.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc())
}

func TestNodeUplinkControllerPublishesReadyState(t *testing.T) {
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
	g.Expect(state.Status.OVSBridge.Name).To(gomega.Equal("br-blue"))
	g.Expect(state.Status.Conditions).To(gomega.ContainElement(gomega.And(
		gomega.HaveField("Type", uplinkv1alpha1.UplinkStateConditionReady),
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", uplinkv1alpha1.UplinkStateReasonReady),
	)))
}

func TestNodeUplinkControllerPreservesGatewayProgrammingFailure(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())

	state := newUplinkState("br-blue.node-a", "br-blue", "node-a")
	state.Status.Type = uplinkv1alpha1.UplinkTypeOVSBridge
	state.Status.HostInterfaceName = uplinkv1alpha1.InterfaceName("breth0")
	state.Status.Conditions = []metav1.Condition{
		{
			Type:    uplinkv1alpha1.UplinkStateConditionReady,
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
		gomega.HaveField("Type", uplinkv1alpha1.UplinkStateConditionReady),
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
	g.Expect(state.Annotations).To(gomega.HaveKeyWithValue(uplinkutil.StateAnnotationUplink, "br-blue"))
	g.Expect(state.Annotations).To(gomega.HaveKeyWithValue(uplinkutil.StateAnnotationNode, "node-a"))
	g.Expect(state.OwnerReferences).To(gomega.HaveLen(1))
	g.Expect(state.Status.UplinkName).To(gomega.Equal("br-blue"))
	g.Expect(state.Status.NodeName).To(gomega.Equal("node-a"))
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
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
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
			Annotations: map[string]string{
				uplinkutil.StateAnnotationUplink: uplinkName,
				uplinkutil.StateAnnotationNode:   nodeName,
			},
		},
		Status: uplinkv1alpha1.UplinkStateStatus{
			UplinkName: uplinkName,
			NodeName:   nodeName,
		},
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

type fakeBridgeResolver struct {
	bridgeName      string
	bridgeUplink    string
	err             error
	bridgeUplinkErr error
}

func (r fakeBridgeResolver) Resolve(_ string) (string, error) {
	return r.bridgeName, r.err
}

func (r fakeBridgeResolver) BridgeUplink(_ string) (string, error) {
	return r.bridgeUplink, r.bridgeUplinkErr
}
