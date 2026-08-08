// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package cni

import (
	"fmt"
	"net"
	"os"

	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
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

var _ = Describe("DPU pod interface recovery", func() {
	var (
		podLister v1mocks.PodLister
		server    *Server
	)

	BeforeEach(func() {
		podLister = v1mocks.PodLister{}
		server = &Server{
			clientSet: &ClientSet{
				podLister: &podLister,
			},
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

		It("skips pod with empty netnsPath", func() {
			pod := podWithDPUAnnotation("test-pod", "test-ns", &util.DPUConnectionDetails{
				PfId:      "0",
				VfId:      "2",
				SandboxId: "abc123",
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

	Context("recoverPodInterfacesScan", func() {
		It("returns 0 when no pods have DPU annotations", func() {
			pods := []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "pod1",
						Namespace:   "ns1",
						Annotations: map[string]string{},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "pod2",
						Namespace:   "ns2",
						Annotations: map[string]string{"other-annotation": "value"},
					},
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
})
