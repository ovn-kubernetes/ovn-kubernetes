// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package evpn

import (
	"fmt"
	"net"
	"syscall"

	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	corelistersfake "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	netlinkMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	multinetworkmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks/multinetwork"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// newFakePodLister creates a fake pod lister from the given pods.
func newFakePodLister(pods ...*corev1.Pod) corelistersfake.PodLister {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, pod := range pods {
		_ = indexer.Add(pod)
	}
	return corelistersfake.NewPodLister(indexer)
}

var _ = Describe("EVPN pod controller", func() {
	const nodeName = "node1"

	var (
		ctrl   *Controller
		nlMock *netlinkMocks.NetLinkOps
		fakeNM *networkmanager.FakeNetworkManager
	)

	BeforeEach(func() {
		fakeNM = &networkmanager.FakeNetworkManager{}
		ctrl = &Controller{
			nodeName:     nodeName,
			networkMgr:   fakeNM,
			podNeighbors: make(map[string]*neighEntries),
			stopChan:     make(chan struct{}),
		}

		nlMock = &netlinkMocks.NetLinkOps{}
		util.SetNetLinkOpMockInst(nlMock)
	})

	AfterEach(func() {
		util.ResetNetLinkOpMockInst()
	})

	Describe("reconcilePod", func() {
		It("programs FDB and neighbor entries for a pod on an EVPN network", func() {
			netInfo := &multinetworkmocks.NetInfo{}
			netInfo.On("EVPNVTEPName").Return("vtep1")
			netInfo.On("EVPNMACVRFVID").Return(100)
			netInfo.On("EVPNMACVRFVNI").Return(int32(10100))
			netInfo.On("GetNetworkName").Return("mynet")
			netInfo.On("GetNetworkID").Return(5)
			netInfo.On("IsPrimaryNetwork").Return(true)
			const nadKey = "test-ns/test-nad"
			fakeNM.NADNetworks = map[string]util.NetInfo{nadKey: netInfo}
			fakeNM.PrimaryNetworks = map[string]util.NetInfo{"test-ns": netInfo}

			sviName := GetEVPNL2SVIName(netInfo)
			ovsPortName := GetEVPNOVSPortName(netInfo)
			sviLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: sviName, Index: 10}}
			ovsPortLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: ovsPortName, Index: 20}}

			nlMock.On("LinkByName", sviName).Return(sviLink, nil)
			nlMock.On("LinkByName", ovsPortName).Return(ovsPortLink, nil)
			nlMock.On("NeighAdd", mock.Anything).Return(nil)

			mac, _ := net.ParseMAC("0a:58:0a:00:00:05")
			podAnnotation := `{"test-ns/test-nad":{"ip_addresses":["10.0.0.5/24"],"mac_address":"0a:58:0a:00:00:05","ip_address":"10.0.0.5/24"}}`
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod", Namespace: "test-ns",
					UID:         "pod-uid-1",
					Annotations: map[string]string{types.OvnPodAnnotationName: podAnnotation},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			}
			ctrl.podLister = newFakePodLister(pod)

			Expect(ctrl.reconcilePod("test-ns/test-pod")).To(Succeed())

			By("verifying FDB entry was added on OVS port")
			nlMock.AssertCalled(GinkgoT(), "NeighAdd", mock.MatchedBy(func(n *netlink.Neigh) bool {
				return n.LinkIndex == 20 && n.HardwareAddr.String() == mac.String() && n.Vlan == 100
			}))

			By("verifying neighbor entry was added on SVI")
			nlMock.AssertCalled(GinkgoT(), "NeighAdd", mock.MatchedBy(func(n *netlink.Neigh) bool {
				return n.LinkIndex == 10 && n.IP.Equal(net.ParseIP("10.0.0.5"))
			}))

			By("verifying cache entry was created")
			ctrl.podNeighLock.Lock()
			entry, exists := ctrl.podNeighbors["test-ns/test-pod"]
			ctrl.podNeighLock.Unlock()
			Expect(exists).To(BeTrue())
			Expect(entry.uid).To(Equal(k8stypes.UID("pod-uid-1")))
		})

		It("cleans up entries when pod is deleted", func() {
			mac, _ := net.ParseMAC("0a:58:0a:00:00:05")
			key := "test-ns/test-pod"

			ctrl.podNeighbors[key] = &neighEntries{
				uid:         "pod-uid-1",
				sviName:     "svl2-test",
				ovsPortName: "evovs-test",
				macvrfVID:   100,
				ips:         []net.IP{net.ParseIP("10.0.0.5")},
				mac:         mac,
			}

			ovsLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "evovs-test", Index: 20}}
			sviLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "svl2-test", Index: 10}}

			nlMock.On("LinkByName", "evovs-test").Return(ovsLink, nil)
			nlMock.On("LinkByName", "svl2-test").Return(sviLink, nil)
			nlMock.On("NeighDel", mock.Anything).Return(nil)

			// Pod not in lister = deleted
			ctrl.podLister = newFakePodLister()

			Expect(ctrl.reconcilePod(key)).To(Succeed())

			By("verifying FDB entry was deleted")
			nlMock.AssertCalled(GinkgoT(), "NeighDel", mock.MatchedBy(func(n *netlink.Neigh) bool {
				return n.LinkIndex == 20 && n.HardwareAddr.String() == mac.String()
			}))

			By("verifying neighbor entry was deleted")
			nlMock.AssertCalled(GinkgoT(), "NeighDel", mock.MatchedBy(func(n *netlink.Neigh) bool {
				return n.LinkIndex == 10 && n.IP.Equal(net.ParseIP("10.0.0.5"))
			}))

			By("verifying cache was cleared")
			_, exists := ctrl.podNeighbors[key]
			Expect(exists).To(BeFalse())
		})

		It("handles cleanup gracefully when OVS port and SVI are already gone", func() {
			mac, _ := net.ParseMAC("0a:58:0a:00:00:05")
			key := "test-ns/stale-pod"

			ctrl.podNeighbors[key] = &neighEntries{
				uid:         "stale-uid",
				sviName:     "svl2-gone",
				ovsPortName: "evovs-gone",
				macvrfVID:   100,
				ips:         []net.IP{net.ParseIP("10.0.0.5")},
				mac:         mac,
			}

			nlMock.On("LinkByName", "evovs-gone").Return(nil, netlink.LinkNotFoundError{})
			nlMock.On("LinkByName", "svl2-gone").Return(nil, netlink.LinkNotFoundError{})

			ctrl.podLister = newFakePodLister()

			Expect(ctrl.reconcilePod(key)).To(Succeed())

			By("verifying cache is cleaned up even though OVS port and SVI were gone")
			_, exists := ctrl.podNeighbors[key]
			Expect(exists).To(BeFalse())
		})

		It("cleans up neighbor entries when OVS port is gone but SVI still exists", func() {
			mac, _ := net.ParseMAC("0a:58:0a:00:00:05")
			key := "test-ns/stale-pod"

			ctrl.podNeighbors[key] = &neighEntries{
				uid:         "stale-uid",
				sviName:     "svl2-test",
				ovsPortName: "evovs-gone",
				macvrfVID:   100,
				ips:         []net.IP{net.ParseIP("10.0.0.5")},
				mac:         mac,
			}

			sviLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "svl2-test", Index: 10}}

			nlMock.On("LinkByName", "evovs-gone").Return(nil, netlink.LinkNotFoundError{})
			nlMock.On("LinkByName", "svl2-test").Return(sviLink, nil)
			nlMock.On("NeighDel", mock.Anything).Return(nil)

			ctrl.podLister = newFakePodLister()

			Expect(ctrl.reconcilePod(key)).To(Succeed())

			By("verifying FDB was NOT attempted on the gone OVS port")
			nlMock.AssertNotCalled(GinkgoT(), "NeighDel", mock.MatchedBy(func(n *netlink.Neigh) bool {
				return n.LinkIndex == 20 // OVS port index from other tests
			}))

			By("verifying neighbor entry was deleted from SVI")
			nlMock.AssertCalled(GinkgoT(), "NeighDel", mock.MatchedBy(func(n *netlink.Neigh) bool {
				return n.LinkIndex == 10 && n.IP.Equal(net.ParseIP("10.0.0.5"))
			}))

			By("verifying cache was cleared")
			_, exists := ctrl.podNeighbors[key]
			Expect(exists).To(BeFalse())
		})

		It("skips pods whose EVPN network annotation is not yet set", func() {
			netInfo := &multinetworkmocks.NetInfo{}
			netInfo.On("EVPNMACVRFVNI").Return(int32(10100))
			netInfo.On("IsPrimaryNetwork").Return(true)
			netInfo.On("GetNetworkName").Return("mynet")
			const nadKey = "test-ns/test-nad"
			fakeNM.NADNetworks = map[string]util.NetInfo{nadKey: netInfo}
			fakeNM.PrimaryNetworks = map[string]util.NetInfo{"test-ns": netInfo}

			// Pod has default network annotation but not the CUDN annotation yet.
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod", Namespace: "test-ns",
					UID:         "pod-uid-1",
					Annotations: map[string]string{types.OvnPodAnnotationName: `{"default":{"ip_addresses":["10.244.0.5/24"],"mac_address":"0a:58:0a:f4:00:05"}}`},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			}
			ctrl.podLister = newFakePodLister(pod)

			Expect(ctrl.reconcilePod("test-ns/test-pod")).To(Succeed())
			nlMock.AssertNotCalled(GinkgoT(), "NeighAdd", mock.Anything)

			By("verifying no cache entry was created")
			_, exists := ctrl.podNeighbors["test-ns/test-pod"]
			Expect(exists).To(BeFalse())
		})

		It("skips pods on non-EVPN networks", func() {
			netInfo := &multinetworkmocks.NetInfo{}
			netInfo.On("EVPNMACVRFVNI").Return(int32(0))
			netInfo.On("IsPrimaryNetwork").Return(true)
			netInfo.On("GetNetworkName").Return("mynet")
			const nadKey = "test-ns/test-nad"
			fakeNM.NADNetworks = map[string]util.NetInfo{nadKey: netInfo}
			fakeNM.PrimaryNetworks = map[string]util.NetInfo{"test-ns": netInfo}

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "test-ns"},
				Spec:       corev1.PodSpec{NodeName: nodeName},
			}
			ctrl.podLister = newFakePodLister(pod)

			Expect(ctrl.reconcilePod("test-ns/test-pod")).To(Succeed())
			nlMock.AssertNotCalled(GinkgoT(), "NeighAdd", mock.Anything)
		})

		It("cleans up entries when pod completes", func() {
			mac, _ := net.ParseMAC("0a:58:0a:00:00:05")
			key := "test-ns/completed-pod"

			ctrl.podNeighbors[key] = &neighEntries{
				uid:         "pod-uid-1",
				sviName:     "svl2-test",
				ovsPortName: "evovs-test",
				macvrfVID:   100,
				ips:         []net.IP{net.ParseIP("10.0.0.5")},
				mac:         mac,
			}

			completedPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "completed-pod", Namespace: "test-ns"},
				Spec:       corev1.PodSpec{NodeName: nodeName},
				Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
			}
			ctrl.podLister = newFakePodLister(completedPod)

			ovsLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "evovs-test", Index: 20}}
			sviLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "svl2-test", Index: 10}}
			nlMock.On("LinkByName", "evovs-test").Return(ovsLink, nil)
			nlMock.On("LinkByName", "svl2-test").Return(sviLink, nil)
			nlMock.On("NeighDel", mock.Anything).Return(nil)

			Expect(ctrl.reconcilePod(key)).To(Succeed())

			_, exists := ctrl.podNeighbors[key]
			Expect(exists).To(BeFalse(), "cache should be cleared for completed pod")
		})

		It("tolerates already-deleted FDB and neighbor entries during pod cleanup", func() {
			mac, _ := net.ParseMAC("0a:58:0a:00:00:05")
			key := "test-ns/test-pod"

			ctrl.podNeighbors[key] = &neighEntries{
				uid:         "pod-uid-1",
				sviName:     "svl2-test",
				ovsPortName: "evovs-test",
				macvrfVID:   100,
				ips:         []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("fd00::5")},
				mac:         mac,
			}

			ovsLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "evovs-test", Index: 20}}
			sviLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "svl2-test", Index: 10}}

			nlMock.On("LinkByName", "evovs-test").Return(ovsLink, nil)
			nlMock.On("LinkByName", "svl2-test").Return(sviLink, nil)
			// FDB already gone
			nlMock.On("NeighDel", mock.MatchedBy(func(n *netlink.Neigh) bool {
				return n.LinkIndex == 20
			})).Return(fmt.Errorf("failed: %w", syscall.ENOENT))
			// Neighbor entries already gone
			nlMock.On("NeighDel", mock.MatchedBy(func(n *netlink.Neigh) bool {
				return n.LinkIndex == 10
			})).Return(fmt.Errorf("failed: %w", syscall.ENOENT))

			ctrl.podLister = newFakePodLister()

			Expect(ctrl.reconcilePod(key)).To(Succeed())

			By("verifying cache is cleaned up despite ENOENT errors")
			_, exists := ctrl.podNeighbors[key]
			Expect(exists).To(BeFalse())
		})

		It("skips pods on a different node", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "remote-pod", Namespace: "test-ns"},
				Spec:       corev1.PodSpec{NodeName: "other-node"},
			}
			ctrl.podLister = newFakePodLister(pod)

			Expect(ctrl.reconcilePod("test-ns/remote-pod")).To(Succeed())
			nlMock.AssertNotCalled(GinkgoT(), "NeighAdd", mock.Anything)
		})
	})
})
