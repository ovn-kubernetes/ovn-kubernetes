package egressip

import (
	"fmt"
	"net"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	netlink_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/egressip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
)

// This test file contains tests specifically for the stale IP cleanup bug fix
// Bug: When SyncEgressIP encounters stale IPs in annotation that are not on the bridge
// (e.g., after node reboot with capacity changes), it fails to clean up the annotation
// because deleteIPBridge returns an error.
//
// Fix: Make sync resilient to bridge deletion failures by logging warnings instead of
// returning errors, ensuring annotation cleanup always executes.

var _ = ginkgo.Describe("Gateway EgressIP - Stale IP Cleanup", func() {

	const (
		nodeName        = "ovn-worker"
		bridgeName      = "breth0"
		bridgeLinkIndex = 10

		// Real IPs (currently assigned to node)
		realIP1 = "192.168.1.10"
		realIP2 = "192.168.1.11"
		realIP3 = "192.168.1.12"
		realIP4 = "192.168.1.13"

		// Stale IPs (were assigned before reboot, now not on bridge)
		staleIP1 = "192.168.1.100"
		staleIP2 = "192.168.1.101"
		staleIP3 = "192.168.1.102"
		staleIP4 = "192.168.1.103"

		mark1 = "50001"
		mark2 = "50002"
		mark3 = "50003"
		mark4 = "50004"
	)

	var (
		nlMock     *mocks.NetLinkOps
		nlLinkMock *netlink_mocks.Link
	)

	ginkgo.BeforeEach(func() {
		err := config.PrepareTestConfig()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nlMock = new(mocks.NetLinkOps)
		nlLinkMock = new(netlink_mocks.Link)
		util.SetNetLinkOpMockInst(nlMock)
		nlMock.On("IsLinkNotFoundError", mock.Anything).
			Maybe().
			Return(false)
	})

	ginkgo.AfterEach(func() {
		util.ResetNetLinkOpMockInst()
	})

	ginkgo.Context("SyncEgressIP with stale IPs in annotation", func() {

		ginkgo.It("removes stale IPs from annotation even when bridge deletion fails", func() {
			// This test simulates the bug scenario:
			// 1. Node had 4 IPs assigned: staleIP1-4
			// 2. Node rebooted, capacity caused reassignment to: realIP1-4
			// 3. Annotation has all 8 IPs (4 stale + 4 real)
			// 4. Bridge only has 4 real IPs (stale ones not present)
			// 5. SyncEgressIP should clean annotation even though bridge deletion fails

			ginkgo.By("Setting up netlink mocks")
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)

			ginkgo.By("Setting up successful AddrAdd for real IPs")
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(realIP1), bridgeLinkIndex)).Return(nil)
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(realIP2), bridgeLinkIndex)).Return(nil)
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(realIP3), bridgeLinkIndex)).Return(nil)
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(realIP4), bridgeLinkIndex)).Return(nil)

			ginkgo.By("Setting up FAILING AddrDel for stale IPs (they're not on bridge)")
			// This simulates the bug: attempting to delete IPs that don't exist on bridge fails
			notFoundErr := fmt.Errorf("RTNETLINK answers: Cannot assign requested address")
			nlMock.On("AddrDel", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(staleIP1), bridgeLinkIndex)).Return(notFoundErr)
			nlMock.On("AddrDel", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(staleIP2), bridgeLinkIndex)).Return(notFoundErr)
			nlMock.On("AddrDel", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(staleIP3), bridgeLinkIndex)).Return(notFoundErr)
			nlMock.On("AddrDel", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(staleIP4), bridgeLinkIndex)).Return(notFoundErr)

			ginkgo.By("Initializing manager with annotation containing both stale and real IPs (8 total)")
			// This simulates the state after node reboot before SyncEgressIP runs
			initialAnnotation := generateAnnotFromIPs(staleIP1, staleIP2, staleIP3, staleIP4, realIP1, realIP2, realIP3, realIP4)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, initialAnnotation)
			defer stopFn()

			ginkgo.By("Verifying initial state has 8 IPs in annotation")
			node, err := addrMgr.nodeLister.Get(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			gomega.Expect(parseEIPsFromAnnotation(node)).Should(gomega.HaveLen(8), "annotation should have 8 IPs initially (4 stale + 4 real)")

			ginkgo.By("Creating EgressIP objects for only the 4 real IPs (simulating new assignments)")
			realEIP1 := getEIPAssignedToNode(nodeName, mark1, realIP1)
			realEIP2 := getEIPAssignedToNode(nodeName, mark2, realIP2)
			realEIP3 := getEIPAssignedToNode(nodeName, mark3, realIP3)
			realEIP4 := getEIPAssignedToNode(nodeName, mark4, realIP4)

			ginkgo.By("Running SyncEgressIP with only real IPs")
			// This should:
			// 1. Add real IPs to annotation and bridge (success)
			// 2. Try to delete stale IPs from bridge (FAILS - not on bridge)
			// 3. WITH FIX: Log warnings and continue, clean annotation
			// 4. WITHOUT FIX: Return error, never clean annotation
			err = addrMgr.SyncEgressIP([]interface{}{realEIP1, realEIP2, realEIP3, realEIP4})

			ginkgo.By("Verifying sync succeeded despite bridge deletion failures")
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "SyncEgressIP should succeed even when bridge deletion fails for stale IPs")

			ginkgo.By("Verifying stale IPs were removed from annotation (THE FIX)")
			gomega.Eventually(func() []string {
				node, err := addrMgr.nodeLister.Get(nodeName)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				return parseEIPsFromAnnotation(node)
			}).Should(gomega.ConsistOf(realIP1, realIP2, realIP3, realIP4),
				"annotation should ONLY contain the 4 real IPs, stale ones should be removed")

			ginkgo.By("Verifying real IPs were added to bridge")
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(realIP1), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(realIP2), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(realIP3), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(realIP4), bridgeLinkIndex))).Should(gomega.BeTrue())

			ginkgo.By("Verifying attempted deletion of stale IPs from bridge (expected to fail)")
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(staleIP1), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(staleIP2), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(staleIP3), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(staleIP4), bridgeLinkIndex))).Should(gomega.BeTrue())

			ginkgo.By("Verifying cache contains only real IPs")
			gomega.Expect(addrMgr.cache.IsIPPresent(net.ParseIP(realIP1))).Should(gomega.BeTrue())
			gomega.Expect(addrMgr.cache.IsIPPresent(net.ParseIP(realIP2))).Should(gomega.BeTrue())
			gomega.Expect(addrMgr.cache.IsIPPresent(net.ParseIP(realIP3))).Should(gomega.BeTrue())
			gomega.Expect(addrMgr.cache.IsIPPresent(net.ParseIP(realIP4))).Should(gomega.BeTrue())
			gomega.Expect(addrMgr.cache.IsIPPresent(net.ParseIP(staleIP1))).Should(gomega.BeFalse())
			gomega.Expect(addrMgr.cache.IsIPPresent(net.ParseIP(staleIP2))).Should(gomega.BeFalse())
			gomega.Expect(addrMgr.cache.IsIPPresent(net.ParseIP(staleIP3))).Should(gomega.BeFalse())
			gomega.Expect(addrMgr.cache.IsIPPresent(net.ParseIP(staleIP4))).Should(gomega.BeFalse())
		})

		ginkgo.It("handles partial bridge deletion failures gracefully", func() {
			// This test verifies that even if some stale IPs can be deleted from bridge
			// and others fail, the annotation is still fully cleaned

			ginkgo.By("Setting up netlink mocks")
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)

			ginkgo.By("Setting up successful AddrAdd for real IP")
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(realIP1), bridgeLinkIndex)).Return(nil)

			ginkgo.By("Setting up mixed results for stale IP deletion")
			nlMock.On("AddrDel", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(staleIP1), bridgeLinkIndex)).Return(nil)                     // Success
			nlMock.On("AddrDel", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(staleIP2), bridgeLinkIndex)).Return(fmt.Errorf("not found")) // Failure

			ginkgo.By("Initializing manager with stale IPs in annotation")
			initialAnnotation := generateAnnotFromIPs(staleIP1, staleIP2)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, initialAnnotation)
			defer stopFn()

			ginkgo.By("Running SyncEgressIP with only real IP")
			realEIP := getEIPAssignedToNode(nodeName, mark1, realIP1)
			err := addrMgr.SyncEgressIP([]interface{}{realEIP})

			ginkgo.By("Verifying sync succeeded")
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			ginkgo.By("Verifying ALL stale IPs removed from annotation despite mixed deletion results")
			gomega.Eventually(func() []string {
				node, err := addrMgr.nodeLister.Get(nodeName)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				return parseEIPsFromAnnotation(node)
			}).Should(gomega.ConsistOf(realIP1), "all stale IPs should be removed from annotation")
		})

		ginkgo.It("handles sync when annotation has only stale IPs and no new assignments", func() {
			// Edge case: Node had IPs assigned, but after reboot has none assigned
			// Annotation should be completely cleaned

			ginkgo.By("Setting up netlink mocks")
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)

			ginkgo.By("Setting up FAILING AddrDel for stale IPs")
			notFoundErr := fmt.Errorf("not found")
			nlMock.On("AddrDel", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(staleIP1), bridgeLinkIndex)).Return(notFoundErr)
			nlMock.On("AddrDel", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(staleIP2), bridgeLinkIndex)).Return(notFoundErr)

			ginkgo.By("Initializing manager with only stale IPs in annotation")
			initialAnnotation := generateAnnotFromIPs(staleIP1, staleIP2)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, initialAnnotation)
			defer stopFn()

			ginkgo.By("Running SyncEgressIP with NO assignments (empty list)")
			err := addrMgr.SyncEgressIP([]interface{}{})

			ginkgo.By("Verifying sync succeeded")
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			ginkgo.By("Verifying annotation is completely empty")
			gomega.Eventually(func() []string {
				node, err := addrMgr.nodeLister.Get(nodeName)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				return parseEIPsFromAnnotation(node)
			}).Should(gomega.BeEmpty(), "annotation should be empty when no IPs are assigned")

			ginkgo.By("Verifying cache is empty")
			gomega.Expect(addrMgr.cache.IsIPPresent(net.ParseIP(staleIP1))).Should(gomega.BeFalse())
			gomega.Expect(addrMgr.cache.IsIPPresent(net.ParseIP(staleIP2))).Should(gomega.BeFalse())
		})
	})
})
