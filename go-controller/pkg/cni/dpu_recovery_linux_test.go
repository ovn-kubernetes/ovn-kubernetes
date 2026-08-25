// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package cni

import (
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"
	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	cni_ns_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/containernetworking/plugins/pkg/ns"
	netlink_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	v1mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func podWithDPUAnnotation(name, namespace string, dcd *util.DPUConnectionDetails, nadKey string) *corev1.Pod {
	annotations := map[string]string{}
	if dcd != nil {
		var err error
		annotations, err = util.MarshalPodDPUConnDetails(annotations, dcd, nadKey)
		Expect(err).NotTo(HaveOccurred())
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
		},
	}
}

func withOVNPodAnnotation(pod *corev1.Pod, nadKey string, ann *util.PodAnnotation) *corev1.Pod {
	var err error
	pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations, ann, nadKey)
	Expect(err).NotTo(HaveOccurred())
	return pod
}

func testPodAnnotation() *util.PodAnnotation {
	return &util.PodAnnotation{
		MAC:      ovntest.MustParseMAC("0a:58:0a:00:00:05"),
		IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
		Gateways: []net.IP{net.ParseIP("192.168.0.1")},
	}
}

func forwardingNetNS() *cni_ns_mocks.NetNS {
	mockNS := &cni_ns_mocks.NetNS{}
	mockNS.On("Close").Return(nil)
	mockNS.On("Fd").Return(uintptr(123456)).Maybe()
	mockNS.On("Do", mock.AnythingOfType("func(ns.NetNS) error")).Return(
		func(toRun func(ns.NetNS) error) error {
			return toRun(nil)
		},
	)
	return mockNS
}

var _ = Describe("DPU pod interface recovery", func() {
	var (
		podLister v1mocks.PodLister
		server    *Server
	)

	BeforeEach(func() {
		origMode := config.OvnKubeNode.Mode
		origMTU := config.Default.RoutableMTU
		DeferCleanup(func() {
			config.OvnKubeNode.Mode = origMode
			config.Default.RoutableMTU = origMTU
			recoverSriovIf = nil
		})

		podLister = v1mocks.PodLister{}
		server = &Server{
			clientSet: &ClientSet{
				podLister: &podLister,
			},
			nodeName: "test-node",
		}
		config.OvnKubeNode.Mode = ovntypes.NodeModeDPUHost
	})

	Context("recoverPodInterface", func() {
		It("skips pod with no DPU annotations", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-pod",
					Namespace:   "test-ns",
					Annotations: map[string]string{},
				},
			}
			err := server.recoverPodInterface(pod)
			Expect(err).NotTo(HaveOccurred())
		})

		It("skips pod with nil annotations", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "test-ns",
				},
			}
			err := server.recoverPodInterface(pod)
			Expect(err).NotTo(HaveOccurred())
		})

		It("skips host-network pods", func() {
			pod := podWithDPUAnnotation("test-pod", "test-ns", &util.DPUConnectionDetails{
				PfId:         "0",
				VfId:         "2",
				SandboxId:    "abc123",
				VfNetdevName: "enp3s0f0v2",
				NetnsPath:    "/var/run/netns/pod-abc",
			}, ovntypes.DefaultNetworkName)
			pod.Spec.HostNetwork = true

			recoverSriovIf = func(_ *Server, _ *corev1.Pod, _ string, _ util.DPUConnectionDetails, _ string) error {
				Fail("recovery should not be invoked for host-network pods")
				return nil
			}

			err := server.recoverPodInterface(pod)
			Expect(err).NotTo(HaveOccurred())
		})

		It("skips pod when netnsPath is empty", func() {
			pod := podWithDPUAnnotation("test-pod", "test-ns", &util.DPUConnectionDetails{
				PfId:         "0",
				VfId:         "2",
				SandboxId:    "abc123",
				VfNetdevName: "enp3s0f0v2",
			}, ovntypes.DefaultNetworkName)

			err := server.recoverPodInterface(pod)
			Expect(err).NotTo(HaveOccurred())
		})

		It("skips pod when VfNetdevName is empty", func() {
			pod := podWithDPUAnnotation("test-pod", "test-ns", &util.DPUConnectionDetails{
				PfId:      "0",
				VfId:      "2",
				SandboxId: "abc123",
				NetnsPath: "/var/run/netns/pod-abc",
			}, ovntypes.DefaultNetworkName)

			err := server.recoverPodInterface(pod)
			Expect(err).NotTo(HaveOccurred())
		})

		It("skips non-default network NADs", func() {
			pod := podWithDPUAnnotation("test-pod", "test-ns", &util.DPUConnectionDetails{
				PfId:         "0",
				VfId:         "2",
				SandboxId:    "abc123",
				VfNetdevName: "enp3s0f0v2",
				NetnsPath:    "/var/run/netns/test",
			}, "test-ns/my-secondary-net")

			err := server.recoverPodInterface(pod)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("setupNetworkRecovery", func() {
		var (
			netlinkOpsMock *utilMocks.NetLinkOps
			mockLink       *netlink_mocks.Link
		)

		BeforeEach(func() {
			netlinkOpsMock = &utilMocks.NetLinkOps{}
			mockLink = &netlink_mocks.Link{}
			util.SetNetLinkOpMockInst(netlinkOpsMock)
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst()
		})

		It("configures IPs and routes", func() {
			podAnnotation := &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
				Gateways: []net.IP{net.ParseIP("192.168.0.1")},
				Routes: []util.PodRoute{
					{
						Dest:    ovntest.MustParseIPNet("10.0.0.0/8"),
						NextHop: net.ParseIP("192.168.0.1"),
					},
				},
			}

			config.Default.RoutableMTU = 1400

			mockLink.On("Attrs").Return(&netlink.LinkAttrs{Name: "eth0", Index: 7})
			netlinkOpsMock.On("AddrAdd", mockLink, mock.Anything).Return(nil)
			netlinkOpsMock.On("RouteReplace", mock.Anything).Return(nil)

			err := setupNetworkRecovery(mockLink, podAnnotation)
			Expect(err).NotTo(HaveOccurred())

			netlinkOpsMock.AssertNumberOfCalls(GinkgoT(), "AddrAdd", 1)
			netlinkOpsMock.AssertNumberOfCalls(GinkgoT(), "RouteReplace", 2)
		})

		It("configures dual-stack IPs and routes", func() {
			podAnnotation := &util.PodAnnotation{
				IPs: ovntest.MustParseIPNets("192.168.0.5/24", "fd00::5/64"),
				Gateways: []net.IP{
					net.ParseIP("192.168.0.1"),
					net.ParseIP("fd00::1"),
				},
			}

			config.Default.RoutableMTU = 1400

			mockLink.On("Attrs").Return(&netlink.LinkAttrs{Name: "eth0", Index: 7})
			netlinkOpsMock.On("AddrAdd", mockLink, mock.Anything).Return(nil)
			netlinkOpsMock.On("RouteReplace", mock.Anything).Return(nil)

			err := setupNetworkRecovery(mockLink, podAnnotation)
			Expect(err).NotTo(HaveOccurred())

			netlinkOpsMock.AssertNumberOfCalls(GinkgoT(), "AddrAdd", 2)
			netlinkOpsMock.AssertNumberOfCalls(GinkgoT(), "RouteReplace", 2)
		})

		It("tolerates EEXIST on AddrAdd", func() {
			podAnnotation := &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
				Gateways: []net.IP{net.ParseIP("192.168.0.1")},
			}

			config.Default.RoutableMTU = 1400

			mockLink.On("Attrs").Return(&netlink.LinkAttrs{Name: "eth0", Index: 7})
			netlinkOpsMock.On("AddrAdd", mockLink, mock.Anything).Return(os.ErrExist)
			netlinkOpsMock.On("RouteReplace", mock.Anything).Return(nil)

			err := setupNetworkRecovery(mockLink, podAnnotation)
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns error on non-EEXIST AddrAdd failure", func() {
			podAnnotation := &util.PodAnnotation{
				IPs: ovntest.MustParseIPNets("192.168.0.5/24"),
			}

			mockLink.On("Attrs").Return(&netlink.LinkAttrs{Name: "eth0", Index: 7})
			netlinkOpsMock.On("AddrAdd", mockLink, mock.Anything).Return(fmt.Errorf("permission denied"))

			err := setupNetworkRecovery(mockLink, podAnnotation)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("permission denied"))
		})

		It("returns error on RouteReplace failure for gateway", func() {
			podAnnotation := &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
				Gateways: []net.IP{net.ParseIP("192.168.0.1")},
			}

			config.Default.RoutableMTU = 1400

			mockLink.On("Attrs").Return(&netlink.LinkAttrs{Name: "eth0", Index: 7})
			netlinkOpsMock.On("AddrAdd", mockLink, mock.Anything).Return(nil)
			netlinkOpsMock.On("RouteReplace", mock.Anything).Return(fmt.Errorf("network unreachable"))

			err := setupNetworkRecovery(mockLink, podAnnotation)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("network unreachable"))
		})

		It("returns error on RouteReplace failure for pod route", func() {
			podAnnotation := &util.PodAnnotation{
				IPs: ovntest.MustParseIPNets("192.168.0.5/24"),
				Routes: []util.PodRoute{
					{
						Dest:    ovntest.MustParseIPNet("10.0.0.0/8"),
						NextHop: net.ParseIP("192.168.0.1"),
					},
				},
			}

			config.Default.RoutableMTU = 1400

			mockLink.On("Attrs").Return(&netlink.LinkAttrs{Name: "eth0", Index: 7})
			netlinkOpsMock.On("AddrAdd", mockLink, mock.Anything).Return(nil)
			netlinkOpsMock.On("RouteReplace", mock.Anything).Return(fmt.Errorf("route error"))

			err := setupNetworkRecovery(mockLink, podAnnotation)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("route error"))
		})

		It("succeeds with no IPs, gateways, or routes", func() {
			podAnnotation := &util.PodAnnotation{}

			err := setupNetworkRecovery(mockLink, podAnnotation)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("recoverPodInterface with recoverSriovIf injection", func() {
		var origRecoverFn func(*Server, *corev1.Pod, string, util.DPUConnectionDetails, string) error

		BeforeEach(func() {
			origRecoverFn = recoverSriovIf
		})

		AfterEach(func() {
			recoverSriovIf = origRecoverFn
		})

		It("triggers VF recovery for a default-network pod", func() {
			var recoveredPod string
			var recoveredNAD string
			var recoveredDCD util.DPUConnectionDetails
			recoverSriovIf = func(_ *Server, pod *corev1.Pod, nadKey string, dcd util.DPUConnectionDetails, ifName string) error {
				recoveredPod = pod.Namespace + "/" + pod.Name
				recoveredNAD = nadKey
				recoveredDCD = dcd
				Expect(ifName).To(Equal("eth0"))
				return nil
			}

			dcd := &util.DPUConnectionDetails{
				PfId:         "0",
				VfId:         "2",
				SandboxId:    "abc123",
				VfNetdevName: "enp3s0f0v2",
				NetnsPath:    "/var/run/netns/pod-abc",
			}
			pod := podWithDPUAnnotation("test-pod", "test-ns", dcd, ovntypes.DefaultNetworkName)

			err := server.recoverPodInterface(pod)
			Expect(err).NotTo(HaveOccurred())
			Expect(recoveredPod).To(Equal("test-ns/test-pod"))
			Expect(recoveredNAD).To(Equal(ovntypes.DefaultNetworkName))
			Expect(recoveredDCD.VfNetdevName).To(Equal("enp3s0f0v2"))
			Expect(recoveredDCD.SandboxId).To(Equal("abc123"))
			Expect(recoveredDCD.NetnsPath).To(Equal("/var/run/netns/pod-abc"))
		})

		It("returns recovery failure from recoverPodInterface", func() {
			recoverSriovIf = func(_ *Server, _ *corev1.Pod, _ string, _ util.DPUConnectionDetails, _ string) error {
				return fmt.Errorf("VF not found on host")
			}

			pod := podWithDPUAnnotation("test-pod", "test-ns", &util.DPUConnectionDetails{
				PfId:         "0",
				VfId:         "2",
				SandboxId:    "abc123",
				VfNetdevName: "enp3s0f0v2",
				NetnsPath:    "/var/run/netns/pod-abc",
			}, ovntypes.DefaultNetworkName)

			err := server.recoverPodInterface(pod)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("VF not found on host"))
		})

		It("full scan detects missing interface and triggers recovery", func() {
			recoverCount := 0
			recoverSriovIf = func(_ *Server, _ *corev1.Pod, _ string, _ util.DPUConnectionDetails, _ string) error {
				recoverCount++
				return nil
			}

			pod := podWithDPUAnnotation("pod1", "ns1", &util.DPUConnectionDetails{
				PfId:         "0",
				VfId:         "3",
				SandboxId:    "sandbox1",
				VfNetdevName: "enp3s0f0v3",
				NetnsPath:    "/var/run/netns/pod1",
			}, ovntypes.DefaultNetworkName)
			pod.Spec.NodeName = "test-node"

			podLister.On("List", labels.Everything()).Return([]*corev1.Pod{pod}, nil)

			failed := server.recoverPodInterfacesScan()
			Expect(failed).To(Equal(0))
			Expect(recoverCount).To(Equal(1))
		})

		It("full scan still recovers when interface is already present", func() {
			recoverCount := 0
			recoverSriovIf = func(_ *Server, _ *corev1.Pod, _ string, _ util.DPUConnectionDetails, _ string) error {
				recoverCount++
				return nil
			}

			pod := podWithDPUAnnotation("pod1", "ns1", &util.DPUConnectionDetails{
				PfId:         "0",
				VfId:         "3",
				SandboxId:    "sandbox1",
				VfNetdevName: "enp3s0f0v3",
				NetnsPath:    "/var/run/netns/pod1",
			}, ovntypes.DefaultNetworkName)
			pod.Spec.NodeName = "test-node"

			podLister.On("List", labels.Everything()).Return([]*corev1.Pod{pod}, nil)

			failed := server.recoverPodInterfacesScan()
			Expect(failed).To(Equal(0))
			Expect(recoverCount).To(Equal(1), "recovery should reconcile even when eth0 may already be present")
		})

		It("full scan increments failed when recovery returns an error", func() {
			recoverSriovIf = func(_ *Server, _ *corev1.Pod, _ string, _ util.DPUConnectionDetails, _ string) error {
				return fmt.Errorf("VF not found on host")
			}

			pod := podWithDPUAnnotation("pod1", "ns1", &util.DPUConnectionDetails{
				PfId:         "0",
				VfId:         "3",
				SandboxId:    "sandbox1",
				VfNetdevName: "enp3s0f0v3",
				NetnsPath:    "/var/run/netns/pod1",
			}, ovntypes.DefaultNetworkName)
			pod.Spec.NodeName = "test-node"

			podLister.On("List", labels.Everything()).Return([]*corev1.Pod{pod}, nil)

			failed := server.recoverPodInterfacesScan()
			Expect(failed).To(Equal(1))
		})

		It("full scan does not increment failed for skipped pods (missing netnsPath)", func() {
			recoverCalled := false
			recoverSriovIf = func(_ *Server, _ *corev1.Pod, _ string, _ util.DPUConnectionDetails, _ string) error {
				recoverCalled = true
				return nil
			}

			pod := podWithDPUAnnotation("pod1", "ns1", &util.DPUConnectionDetails{
				PfId:         "0",
				VfId:         "3",
				SandboxId:    "sandbox1",
				VfNetdevName: "enp3s0f0v3",
			}, ovntypes.DefaultNetworkName)
			pod.Spec.NodeName = "test-node"

			podLister.On("List", labels.Everything()).Return([]*corev1.Pod{pod}, nil)

			failed := server.recoverPodInterfacesScan()
			Expect(failed).To(Equal(0))
			Expect(recoverCalled).To(BeFalse())
		})
	})

	Context("recoverPodInterfacesScan", func() {
		It("returns 0 when no pods have DPU annotations", func() {
			pods := []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "pod1",
						Namespace:   "ns1",
						Annotations: map[string]string{},
					},
					Spec: corev1.PodSpec{NodeName: "test-node"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "pod2",
						Namespace:   "ns2",
						Annotations: map[string]string{"other-annotation": "value"},
					},
					Spec: corev1.PodSpec{NodeName: "test-node"},
				},
			}

			podLister.On("List", labels.Everything()).Return(pods, nil)

			failed := server.recoverPodInterfacesScan()
			Expect(failed).To(Equal(0))
		})

		It("skips pods on other nodes", func() {
			pods := []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "pod1",
						Namespace:   "ns1",
						Annotations: map[string]string{},
					},
					Spec: corev1.PodSpec{NodeName: "other-node"},
				},
			}

			podLister.On("List", labels.Everything()).Return(pods, nil)

			failed := server.recoverPodInterfacesScan()
			Expect(failed).To(Equal(0))
		})

		It("returns 0 when pod list is empty", func() {
			podLister.On("List", labels.Everything()).Return([]*corev1.Pod{}, nil)

			failed := server.recoverPodInterfacesScan()
			Expect(failed).To(Equal(0))
		})

		It("returns 1 when pod listing fails", func() {
			podLister.On("List", labels.Everything()).Return(nil, fmt.Errorf("lister error"))

			failed := server.recoverPodInterfacesScan()
			Expect(failed).To(Equal(1))
		})
	})

	Context("recoverSriovInterface", func() {
		const vfName = "enp3s0f0v2"

		var (
			netlinkOpsMock *utilMocks.NetLinkOps
			mockLink       *netlink_mocks.Link
			mockNS         *cni_ns_mocks.NetNS
			origOpenNetNS  func(string) (ns.NetNS, error)
			linkNotFound   = errors.New("link not found")
			pod            *corev1.Pod
			dcd            util.DPUConnectionDetails
		)

		BeforeEach(func() {
			netlinkOpsMock = &utilMocks.NetLinkOps{}
			mockLink = &netlink_mocks.Link{}
			mockNS = forwardingNetNS()
			util.SetNetLinkOpMockInst(netlinkOpsMock)
			origOpenNetNS = openNetNS
			openNetNS = func(path string) (ns.NetNS, error) {
				Expect(path).To(Equal("/var/run/netns/pod-abc"))
				return mockNS, nil
			}

			config.Default.MTU = 1500
			config.Default.RoutableMTU = 1400

			netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(func(err error) bool {
				return errors.Is(err, linkNotFound)
			})

			ann := testPodAnnotation()
			mockLink.On("Attrs").Return(&netlink.LinkAttrs{
				Name:         "eth0",
				Index:        7,
				HardwareAddr: ann.MAC,
			})

			dcd = util.DPUConnectionDetails{
				PfId:         "0",
				VfId:         "2",
				SandboxId:    "abc123",
				VfNetdevName: vfName,
				NetnsPath:    "/var/run/netns/pod-abc",
			}
			pod = withOVNPodAnnotation(podWithDPUAnnotation("test-pod", "test-ns", &dcd, ovntypes.DefaultNetworkName),
				ovntypes.DefaultNetworkName, ann)
		})

		AfterEach(func() {
			openNetNS = origOpenNetNS
			util.ResetNetLinkOpMockInst()
		})

		It("moves the VF from the host and reconciles network config", func() {
			netlinkOpsMock.On("LinkByName", "eth0").Return(nil, linkNotFound).Once()
			netlinkOpsMock.On("LinkByName", vfName).Return(nil, linkNotFound).Once()
			netlinkOpsMock.On("LinkByName", vfName).Return(mockLink, nil)
			netlinkOpsMock.On("LinkSetNsFd", mockLink, mock.Anything).Return(nil)
			netlinkOpsMock.On("LinkSetDown", mockLink).Return(nil)
			netlinkOpsMock.On("LinkSetName", mockLink, "eth0").Return(nil)
			netlinkOpsMock.On("LinkSetUp", mockLink).Return(nil)
			netlinkOpsMock.On("LinkByName", "eth0").Return(mockLink, nil)
			netlinkOpsMock.On("LinkSetHardwareAddr", mockLink, mock.Anything).Return(nil)
			netlinkOpsMock.On("LinkSetMTU", mockLink, 1500).Return(nil)
			netlinkOpsMock.On("AddrAdd", mockLink, mock.Anything).Return(nil)
			netlinkOpsMock.On("RouteReplace", mock.Anything).Return(nil)

			err := server.recoverSriovInterface(pod, ovntypes.DefaultNetworkName, dcd, "eth0")
			Expect(err).NotTo(HaveOccurred())
			netlinkOpsMock.AssertCalled(GinkgoT(), "LinkSetNsFd", mockLink, mock.Anything)
			netlinkOpsMock.AssertCalled(GinkgoT(), "LinkSetName", mockLink, "eth0")
			netlinkOpsMock.AssertCalled(GinkgoT(), "AddrAdd", mockLink, mock.Anything)
		})

		It("renames an already-moved VF and reconciles", func() {
			netlinkOpsMock.On("LinkByName", "eth0").Return(nil, linkNotFound).Once()
			netlinkOpsMock.On("LinkByName", vfName).Return(mockLink, nil)
			netlinkOpsMock.On("LinkSetDown", mockLink).Return(nil)
			netlinkOpsMock.On("LinkSetName", mockLink, "eth0").Return(nil)
			netlinkOpsMock.On("LinkSetUp", mockLink).Return(nil)
			netlinkOpsMock.On("LinkByName", "eth0").Return(mockLink, nil)
			netlinkOpsMock.On("LinkSetHardwareAddr", mockLink, mock.Anything).Return(nil)
			netlinkOpsMock.On("LinkSetMTU", mockLink, 1500).Return(nil)
			netlinkOpsMock.On("AddrAdd", mockLink, mock.Anything).Return(nil)
			netlinkOpsMock.On("RouteReplace", mock.Anything).Return(nil)

			err := server.recoverSriovInterface(pod, ovntypes.DefaultNetworkName, dcd, "eth0")
			Expect(err).NotTo(HaveOccurred())
			netlinkOpsMock.AssertNotCalled(GinkgoT(), "LinkSetNsFd")
			netlinkOpsMock.AssertCalled(GinkgoT(), "LinkSetName", mockLink, "eth0")
		})

		It("reconciles when eth0 is already present", func() {
			netlinkOpsMock.On("LinkByName", "eth0").Return(mockLink, nil)
			netlinkOpsMock.On("LinkSetHardwareAddr", mockLink, mock.Anything).Return(nil)
			netlinkOpsMock.On("LinkSetMTU", mockLink, 1500).Return(nil)
			netlinkOpsMock.On("LinkSetUp", mockLink).Return(nil)
			netlinkOpsMock.On("AddrAdd", mockLink, mock.Anything).Return(nil)
			netlinkOpsMock.On("RouteReplace", mock.Anything).Return(nil)

			err := server.recoverSriovInterface(pod, ovntypes.DefaultNetworkName, dcd, "eth0")
			Expect(err).NotTo(HaveOccurred())
			netlinkOpsMock.AssertNotCalled(GinkgoT(), "LinkSetNsFd")
			netlinkOpsMock.AssertNotCalled(GinkgoT(), "LinkSetName")
			netlinkOpsMock.AssertCalled(GinkgoT(), "LinkSetHardwareAddr", mockLink, mock.Anything)
			netlinkOpsMock.AssertCalled(GinkgoT(), "AddrAdd", mockLink, mock.Anything)
		})

		It("skips IP and route configuration for live-migratable pods", func() {
			pod.Annotations[kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation] = ""

			netlinkOpsMock.On("LinkByName", "eth0").Return(mockLink, nil)
			netlinkOpsMock.On("LinkSetHardwareAddr", mockLink, mock.Anything).Return(nil)
			netlinkOpsMock.On("LinkSetMTU", mockLink, 1500).Return(nil)
			netlinkOpsMock.On("LinkSetUp", mockLink).Return(nil)

			err := server.recoverSriovInterface(pod, ovntypes.DefaultNetworkName, dcd, "eth0")
			Expect(err).NotTo(HaveOccurred())
			netlinkOpsMock.AssertNotCalled(GinkgoT(), "AddrAdd")
			netlinkOpsMock.AssertNotCalled(GinkgoT(), "RouteReplace")
		})

		It("returns a retryable error when the VF is missing from host and pod netns", func() {
			netlinkOpsMock.On("LinkByName", "eth0").Return(nil, linkNotFound)
			netlinkOpsMock.On("LinkByName", vfName).Return(nil, linkNotFound)

			err := server.recoverSriovInterface(pod, ovntypes.DefaultNetworkName, dcd, "eth0")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found on host or in pod netns"))
		})
	})
})
