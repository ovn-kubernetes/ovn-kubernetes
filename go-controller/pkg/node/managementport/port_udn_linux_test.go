// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package managementport

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	multinetworkmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks/multinetwork"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("UDN management port controller", func() {
	const (
		udnNodeName    = "worker-node"
		udnNetworkName = "bluenet"
		udnNetworkID   = 3
		udnMTU         = 1400
		udnMgmtIntf    = "k8s.worker-node.bluenet"
		// The representor names follow the format the simulated DPU derives
		// from the PF and function IDs, so that the representor published by
		// the node annotation resolves to the same name under either DPUOps
		// implementation. Which one the singleton picked is decided by the
		// first caller in the test binary and is not ours to choose.
		stalePortRep = "rep0-1"
		freshPortRep = "rep0-3"
	)

	var (
		origNetlinkOps  = util.GetNetLinkOps()
		origSriovnetOps = util.GetSriovnetOps()

		netlinkOpsMock  *utilMocks.NetLinkOps
		sriovnetOpsMock *utilMocks.SriovnetOps
		execMock        *ovntest.FakeExec
		freshLink       *mocks.Link

		controller *UDNManagementPortController
	)

	// publishDevice returns the node annotation naming the device the host
	// reserved for this network's management port.
	publishDevice := func(pfID, funcID int) map[string]string {
		return map[string]string{
			util.OvnNodeManagementPort: fmt.Sprintf(
				`{%q:{"DeviceId":"0000:05:00.%d","PfId":%d,"FuncId":%d}}`,
				udnNetworkName, funcID, pfID, funcID),
		}
	}

	// expectRepResolves stubs both DPUOps implementations to resolve the
	// device published as pfID/funcID to repName.
	expectRepResolves := func(pfID, funcID int, repName string) {
		sriovnetOpsMock.On("GetVfRepresentorDPU", fmt.Sprintf("%d", pfID), fmt.Sprintf("%d", funcID)).
			Return(repName, nil)
	}

	// expectRepDeleted stubs the teardown of a representor that is no longer
	// on the host, which is the path a re-plumb takes.
	expectRepDeleted := func(repName string) {
		execMock.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 --if-exists del-port br-int " + repName,
		})
		notFound := fmt.Errorf("link %s not found", repName)
		netlinkOpsMock.On("LinkByName", repName).Return(nil, notFound)
		netlinkOpsMock.On("IsLinkNotFoundError", notFound).Return(true)
	}

	// expectRepCreated stubs plumbing repName onto br-int.
	expectRepCreated := func(repName string) {
		execMock.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get bridge br-int datapath_type",
			Output: "",
		})
		execMock.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovs-vsctl --timeout=15 -- --may-exist add-port br-int %s -- set interface %s "+
				"external-ids:iface-id=%s external-ids:%s=%s external-ids:%s=%s",
				repName, repName, udnMgmtIntf,
				types.NetworkExternalID, udnNetworkName,
				types.OvnManagementPortNameExternalID, util.GetNetworkScopedK8sMgmtHostIntfName(udnNetworkID)),
		})
	}

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.OvnKubeNode.Mode = types.NodeModeDPU
		util.ResetRunner()
		DeferCleanup(util.ResetRunner)

		execMock = ovntest.NewLooseCompareFakeExec()
		Expect(util.SetExec(execMock)).To(Succeed())

		netlinkOpsMock = &utilMocks.NetLinkOps{}
		util.SetNetLinkOpMockInst(netlinkOpsMock)
		DeferCleanup(func() { util.SetNetLinkOpMockInst(origNetlinkOps) })

		sriovnetOpsMock = &utilMocks.SriovnetOps{}
		util.SetSriovnetOpsInst(sriovnetOpsMock)
		DeferCleanup(func() { util.SetSriovnetOpsInst(origSriovnetOps) })

		// The replacement representor is already named and sized as wanted, so
		// bringing it up is the only link change a re-plumb makes.
		freshLink = &mocks.Link{}
		freshLink.On("Attrs").Return(&netlink.LinkAttrs{Name: freshPortRep, MTU: udnMTU})
		netlinkOpsMock.On("LinkByName", freshPortRep).Return(freshLink, nil)

		subnets := []*net.IPNet{ovntest.MustParseIPNet("10.1.1.0/24")}
		netInfo := &multinetworkmocks.NetInfo{}
		netInfo.On("GetNetworkName").Return(udnNetworkName)
		netInfo.On("GetNetworkID").Return(udnNetworkID)
		netInfo.On("MTU").Return(udnMTU)
		netInfo.On("GetNodeManagementIP", subnets[0]).Return(util.GetNodeManagementIfAddr(subnets[0]))
		netInfo.On("GetNetworkScopedK8sMgmtIntfName", udnNodeName).Return(udnMgmtIntf)

		cfg, err := newUDNManagementPortConfig(udnNodeName, subnets, netInfo)
		Expect(err).NotTo(HaveOccurred())

		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		Expect(indexer.Add(&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:        udnNodeName,
				Annotations: publishDevice(0, 3),
			},
		})).To(Succeed())

		controller = &UDNManagementPortController{
			cfg:        cfg,
			nodeLister: listers.NewNodeLister(indexer),
			ports: map[string]udnManagementPort{
				representorPort: newUDNManagementPortRep(cfg, stalePortRep),
			},
		}
	})

	Context("Reconcile", func() {
		It("re-plumbs the representor when the published device changes", func() {
			expectRepResolves(0, 3, freshPortRep)
			expectRepDeleted(stalePortRep)
			expectRepCreated(freshPortRep)
			netlinkOpsMock.On("LinkSetUp", freshLink).Return(nil)

			Expect(controller.Reconcile()).To(Succeed())

			rep, ok := controller.ports[representorPort].(*udnManagementPortRep)
			Expect(ok).To(BeTrue())
			Expect(rep.repDevice).To(Equal(freshPortRep))
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
		})

		It("leaves the representor alone when the published device is unchanged", func() {
			controller.ports[representorPort] = newUDNManagementPortRep(controller.cfg, freshPortRep)
			expectRepResolves(0, 3, freshPortRep)
			expectRepCreated(freshPortRep)
			netlinkOpsMock.On("LinkSetUp", freshLink).Return(nil)

			Expect(controller.Reconcile()).To(Succeed())

			// No delete: the representor is re-asserted, not replaced.
			netlinkOpsMock.AssertNotCalled(GinkgoT(), "LinkByName", stalePortRep)
		})

		It("tracks the replacement so a failed re-plumb is retried and torn down", func() {
			expectRepResolves(0, 3, freshPortRep)
			expectRepDeleted(stalePortRep)
			// Bringing the link up fails before the port reaches br-int, so
			// this attempt runs no OVS command of its own.
			netlinkOpsMock.On("LinkSetUp", freshLink).Return(fmt.Errorf("link set up failed")).Once()

			Expect(controller.Reconcile()).NotTo(Succeed())

			// The replacement is tracked even though it is not plumbed, so a
			// teardown arriving now removes it rather than the device that was
			// just deleted.
			rep, ok := controller.ports[representorPort].(*udnManagementPortRep)
			Expect(ok).To(BeTrue())
			Expect(rep.repDevice).To(Equal(freshPortRep))

			// The retry completes the create it did not get through.
			expectRepCreated(freshPortRep)
			netlinkOpsMock.On("LinkSetUp", freshLink).Return(nil)

			Expect(controller.Reconcile()).To(Succeed())
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
		})

		It("tears down the replacement of a failed re-plumb", func() {
			expectRepResolves(0, 3, freshPortRep)
			expectRepDeleted(stalePortRep)
			netlinkOpsMock.On("LinkSetUp", freshLink).Return(fmt.Errorf("link set up failed")).Once()

			Expect(controller.Reconcile()).NotTo(Succeed())

			// The replacement is on the host even though the reconcile did not
			// get to plumb it, so the teardown takes it down rather than
			// leaving it behind. The representor it replaced is already gone,
			// and deleting it a second time would run an OVS command the fake
			// exec does not expect.
			execMock.AddFakeCmdsNoOutputNoError([]string{
				"ovs-vsctl --timeout=15 --if-exists del-port br-int " + freshPortRep,
			})
			netlinkOpsMock.On("AddrList", freshLink, netlink.FAMILY_ALL).Return([]netlink.Addr{}, nil)
			netlinkOpsMock.On("LinkSetDown", freshLink).Return(nil)

			Expect(controller.Delete()).To(Succeed())
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			netlinkOpsMock.AssertCalled(GinkgoT(), "LinkSetDown", freshLink)
		})

		It("does nothing when no device is published for the network", func() {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			Expect(indexer.Add(&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: udnNodeName},
			})).To(Succeed())
			controller.nodeLister = listers.NewNodeLister(indexer)

			Expect(controller.Reconcile()).To(Succeed())

			rep, ok := controller.ports[representorPort].(*udnManagementPortRep)
			Expect(ok).To(BeTrue())
			Expect(rep.repDevice).To(Equal(stalePortRep))
		})

		It("does nothing when the network has no representor port", func() {
			controller.ports = map[string]udnManagementPort{}
			Expect(controller.Reconcile()).To(Succeed())
		})
	})
})
