package cni

import (
	"context"
	"fmt"
	"net"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	v1nadmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	v1mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type podRequestInterfaceOpsStub struct {
	unconfiguredInterfaces []*PodInterfaceInfo
}

func (stub *podRequestInterfaceOpsStub) ConfigureInterface(pr *PodRequest, _ PodInfoGetter, pii *PodInterfaceInfo) ([]*current.Interface, error) {
	if len(pii.IPs) > 0 {
		return []*current.Interface{
			{
				Name:    pr.IfName,
				Sandbox: "/var/run/netns/" + pr.PodNamespace + "_" + pr.PodName,
			},
		}, nil
	}
	return nil, nil
}
func (stub *podRequestInterfaceOpsStub) UnconfigureInterface(_ *PodRequest, ifInfo *PodInterfaceInfo) error {
	stub.unconfiguredInterfaces = append(stub.unconfiguredInterfaces, ifInfo)
	return nil
}

var _ = Describe("Network Segmentation", func() {
	var (
		fakeClientset            *fake.Clientset
		pr                       PodRequest
		pod                      *corev1.Pod
		podLister                v1mocks.PodLister
		podNamespaceLister       v1mocks.PodNamespaceLister
		nadLister                v1nadmocks.NetworkAttachmentDefinitionLister
		clientSet                *ClientSet
		kubeAuth                 *KubeAPIAuth
		obtainedPodIterfaceInfos []*PodInterfaceInfo
		getCNIResultStub         = func(_ *PodRequest, _ PodInfoGetter, podInterfaceInfo *PodInterfaceInfo) (*current.Result, error) {
			obtainedPodIterfaceInfos = append(obtainedPodIterfaceInfos, podInterfaceInfo)
			return &current.Result{}, nil
		}
		prInterfaceOpsStub *podRequestInterfaceOpsStub
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.IPv4Mode = true
		config.IPv6Mode = true

		prInterfaceOpsStub = &podRequestInterfaceOpsStub{}
		podRequestInterfaceOps = prInterfaceOpsStub
		obtainedPodIterfaceInfos = []*PodInterfaceInfo{}

		fakeClientset = fake.NewSimpleClientset()
		pr = PodRequest{
			Command:      CNIAdd,
			PodNamespace: "foo-ns",
			PodName:      "bar-pod",
			SandboxID:    "824bceff24af3",
			Netns:        "ns",
			IfName:       "eth0",
			CNIConf: &types.NetConf{
				NetConf:  cnitypes.NetConf{},
				DeviceID: "",
			},
			timestamp: time.Time{},
			IsVFIO:    false,
			netName:   ovntypes.DefaultNetworkName,
			nadName:   ovntypes.DefaultNetworkName,
			nadKey:    ovntypes.DefaultNetworkName,
		}
		pr.ctx, pr.cancel = context.WithTimeout(context.Background(), 2*time.Minute)

		podNamespaceLister = v1mocks.PodNamespaceLister{}
		podLister = v1mocks.PodLister{}
		nadLister = v1nadmocks.NetworkAttachmentDefinitionLister{}
		clientSet = &ClientSet{
			podLister: &podLister,
			nadLister: &nadLister,
			kclient:   fakeClientset,
		}
		kubeAuth = &KubeAPIAuth{
			Kubeconfig:       config.Kubernetes.Kubeconfig,
			KubeAPIServer:    config.Kubernetes.APIServer,
			KubeAPIToken:     config.Kubernetes.Token,
			KubeAPITokenFile: config.Kubernetes.TokenFile,
		}
		podLister.On("Pods", pr.PodNamespace).Return(&podNamespaceLister)
	})
	AfterEach(func() {

		podRequestInterfaceOps = &defaultPodRequestInterfaceOps{}
	})

	Context("with network segmentation fg disabled and annotation without role field", func() {
		BeforeEach(func() {
			config.OVNKubernetesFeature.EnableMultiNetwork = false
			config.OVNKubernetesFeature.EnableNetworkSegmentation = false
			pod = &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pr.PodName,
					Namespace: pr.PodNamespace,
					Annotations: map[string]string{
						"k8s.ovn.org/pod-networks": `{"default":{"ip_address":"100.10.10.3/24","mac_address":"0a:58:fd:98:00:01"}}`,
					},
				},
			}
		})
		It("should not fail at cmdAdd or cmdDel", func() {
			podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)

			ovsClient, err := newOVSClientWithExternalIDs(map[string]string{})
			Expect(err).NotTo(HaveOccurred())
			By("cmdAdd primary pod interface should be added")
			Expect(pr.cmdAddWithGetCNIResultFunc(kubeAuth, clientSet, getCNIResultStub, networkmanager.Default().Interface(), ovsClient)).NotTo(BeNil())
			Expect(obtainedPodIterfaceInfos).ToNot(BeEmpty())
			By("cmdDel primary pod interface should be removed")
			podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)
			Expect(pr.cmdDel(clientSet)).NotTo(BeNil())
			Expect(prInterfaceOpsStub.unconfiguredInterfaces).To(HaveLen(1))
		})
	})
	Context("with network segmentation fg enabled and annotation with role field", func() {
		BeforeEach(func() {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		})

		Context("pod with default primary network", func() {
			BeforeEach(func() {
				pod = &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      pr.PodName,
						Namespace: pr.PodNamespace,
						Annotations: map[string]string{
							"k8s.ovn.org/pod-networks": `{"default":{"ip_address":"100.10.10.3/24","mac_address":"0a:58:fd:98:00:01", "role":"primary"}}`,
						},
					},
				}
			})

			Context("with CNI Privileged Mode", func() {
				It("should not fail at cmdAdd or cmdDel", func() {
					podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)
					ovsClient, err := newOVSClientWithExternalIDs(map[string]string{})
					Expect(err).NotTo(HaveOccurred())
					By("cmdAdd primary pod interface should be added")
					response, err := pr.cmdAddWithGetCNIResultFunc(kubeAuth, clientSet, getCNIResultStub, networkmanager.Default().Interface(), ovsClient)
					Expect(err).NotTo(HaveOccurred())
					Expect(response.Result).NotTo(BeNil())
					Expect(obtainedPodIterfaceInfos).ToNot(BeEmpty())
					Expect(response.PrimaryUDNPodInfo).To(BeNil())
					Expect(response.PrimaryUDNPodReq).To(BeNil())
					By("cmdDel primary pod interface should be removed")
					podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)
					Expect(pr.cmdDel(clientSet)).NotTo(BeNil())
					Expect(prInterfaceOpsStub.unconfiguredInterfaces).To(HaveLen(1))
				})
			})

			Context("with CNI Unprivileged Mode", func() {
				BeforeEach(func() {
					config.UnprivilegedMode = true
				})
				It("should not fail at cmdAdd", func() {
					podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)
					ovsClient, err := newOVSClientWithExternalIDs(map[string]string{})
					Expect(err).NotTo(HaveOccurred())
					response, err := pr.cmdAddWithGetCNIResultFunc(kubeAuth, clientSet, getCNIResultStub, networkmanager.Default().Interface(), ovsClient)
					Expect(err).NotTo(HaveOccurred())
					Expect(response.Result).To(BeNil())
					Expect(obtainedPodIterfaceInfos).To(BeEmpty())
					Expect(response.PrimaryUDNPodReq).To(BeNil())
					Expect(response.PrimaryUDNPodInfo).To(BeNil())
				})
				It("should not fail at cmdDel", func() {
					podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)
					response, err := pr.cmdDel(clientSet)
					Expect(err).NotTo(HaveOccurred())
					Expect(response.Result).To(BeNil())
					Expect(response.PodIFInfo).ToNot(BeNil())
				})
			})
		})

		Context("pod with a user defined primary network", func() {
			const (
				dummyMACHostSide = "07:06:05:04:03:02"
				nadName          = "tenantred"
				namespace        = "foo-ns"
			)

			var fakeNetworkManager *networkmanager.FakeNetworkManager

			dummyGetCNIResult := func(request *PodRequest, _ PodInfoGetter, podInterfaceInfo *PodInterfaceInfo) (*current.Result, error) {
				var gatewayIP net.IP
				if len(podInterfaceInfo.Gateways) > 0 {
					gatewayIP = podInterfaceInfo.Gateways[0]
				}
				var ips []*current.IPConfig
				ifaceIdx := 1 // host side of the veth is 0; pod side of the veth is 1
				for _, ip := range podInterfaceInfo.IPs {
					ips = append(ips, &current.IPConfig{Address: *ip, Gateway: gatewayIP, Interface: &ifaceIdx})
				}
				ifaceName := "eth0"
				if request.netName != "default" {
					ifaceName = "ovn-udn1"
				}

				interfaces := []*current.Interface{
					{
						Name: "host_" + ifaceName,
						Mac:  dummyMACHostSide,
					},
					{
						Name:    ifaceName,
						Mac:     podInterfaceInfo.MAC.String(),
						Sandbox: "bobloblaw",
					},
				}
				return &current.Result{
					CNIVersion: "0.3.1",
					Interfaces: interfaces,
					IPs:        ips,
				}, nil
			}

			BeforeEach(func() {
				pod = &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      pr.PodName,
						Namespace: pr.PodNamespace,
						Annotations: map[string]string{
							"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["100.10.10.3/24","fd44::33/64"],"mac_address":"0a:58:fd:98:00:01", "role":"infrastructure-locked"}, "foo-ns/meganet":{"ip_addresses":["10.10.10.30/24","fd10::3/64"],"mac_address":"02:03:04:05:06:07", "role":"primary"}}`,
						},
					},
				}
				nadMegaNet := testing.GenerateNADWithConfig("meganet", namespace, dummyPrimaryUDNConfig(namespace, "meganet"))
				nadNamespaceLister := &v1nadmocks.NetworkAttachmentDefinitionNamespaceLister{}
				nadLister.On("NetworkAttachmentDefinitions", "foo-ns").Return(nadNamespaceLister)
				nadNamespaceLister.On("Get", "meganet").Return(nadMegaNet, nil)
				nadNetwork, err := util.ParseNADInfo(nadMegaNet)
				Expect(err).NotTo(HaveOccurred())
				fakeNetworkManager = &networkmanager.FakeNetworkManager{
					PrimaryNetworks: make(map[string]util.NetInfo),
				}
				fakeNetworkManager.PrimaryNetworks[nadMegaNet.Namespace] = nadNetwork
			})

			Context("with CNI Privileged Mode", func() {
				It("should return the information of both the default net and the primary UDN in the result", func() {
					podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)
					ovsClient, err := newOVSClientWithExternalIDs(map[string]string{})
					Expect(err).NotTo(HaveOccurred())
					response, err := pr.cmdAddWithGetCNIResultFunc(kubeAuth, clientSet, dummyGetCNIResult, fakeNetworkManager, ovsClient)
					Expect(err).NotTo(HaveOccurred())
					// for every interface added, we return 2 interfaces; the host side of the
					// veth, then the pod side of the veth.
					// thus, the UDN interface idx will be 3:
					// idx: iface
					//   0: host side primary UDN
					//   1: pod side default network
					//   2: host side default network
					//   3: pod side primary UDN
					podDefaultClusterNetIfaceIDX := 1
					podUDNIfaceIDX := 3
					Expect(response.Result).To(Equal(
						&current.Result{
							CNIVersion: "0.3.1",
							Interfaces: []*current.Interface{
								{
									Name: "host_eth0",
									Mac:  dummyMACHostSide,
								},
								{
									Name:    "eth0",
									Mac:     "0a:58:fd:98:00:01",
									Sandbox: "bobloblaw",
								},
								{
									Name: "host_ovn-udn1",
									Mac:  dummyMACHostSide,
								},
								{
									Name:    "ovn-udn1",
									Mac:     "02:03:04:05:06:07",
									Sandbox: "bobloblaw",
								},
							},
							IPs: []*current.IPConfig{
								{
									Address: net.IPNet{
										IP:   net.ParseIP("100.10.10.3"),
										Mask: net.CIDRMask(24, 32),
									},
									Interface: &podDefaultClusterNetIfaceIDX,
								},
								{
									Address: net.IPNet{
										IP:   net.ParseIP("fd44::33"),
										Mask: net.CIDRMask(64, 128),
									},
									Interface: &podDefaultClusterNetIfaceIDX,
								},
								{
									Address: net.IPNet{
										IP:   net.ParseIP("10.10.10.30"),
										Mask: net.CIDRMask(24, 32),
									},
									Interface: &podUDNIfaceIDX,
								},
								{
									Address: net.IPNet{
										IP:   net.ParseIP("fd10::3"),
										Mask: net.CIDRMask(64, 128),
									},
									Interface: &podUDNIfaceIDX,
								},
							},
						},
					))
				})
			})
			Context("with CNI Unprivileged Mode", func() {
				BeforeEach(func() {
					config.UnprivilegedMode = true
				})
				It("should return the information of both the default net and the primary UDN in the result", func() {
					podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)
					ovsClient, err := newOVSClientWithExternalIDs(map[string]string{})
					Expect(err).NotTo(HaveOccurred())
					response, err := pr.cmdAddWithGetCNIResultFunc(kubeAuth, clientSet, dummyGetCNIResult, fakeNetworkManager, ovsClient)
					Expect(err).NotTo(HaveOccurred())
					Expect(response.Result).To(BeNil())
					podNADAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, "foo-ns/meganet")
					Expect(err).NotTo(HaveOccurred())
					Expect(response.PrimaryUDNPodInfo).To(Equal(
						&PodInterfaceInfo{
							PodAnnotation: *podNADAnnotation,
							MTU:           1400,
							NetName:       "tenantred",
							NADKey:        "foo-ns/meganet",
						}))
					Expect(response.PrimaryUDNPodReq.IfName).To(Equal("ovn-udn1"))
					Expect(response.PodIFInfo.NetName).To(Equal("default"))
				})
			})

		})
	})

})

func dummyPrimaryUDNConfig(ns, nadName string) string {
	namespacedName := fmt.Sprintf("%s/%s", ns, nadName)
	return fmt.Sprintf(`
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
            "subnets": "10.10.0.0/16,fd10::0/64",
            "netAttachDefName": %q,
            "role": "primary"
    }
`, namespacedName)
}

var _ = Describe("checkBridgeMapping", func() {
	const networkName = "test-network"

	Context("when topology is not localnet", func() {
		It("should return nil without checking bridge mappings", func() {
			ovsClient, err := newOVSClientWithExternalIDs(map[string]string{})
			Expect(err).NotTo(HaveOccurred())
			Expect(checkBridgeMapping(ovsClient, ovntypes.Layer2Topology, networkName)).To(Succeed())
		})
	})

	Context("when using default network", func() {
		It("should return nil without checking bridge mappings", func() {
			ovsClient, err := newOVSClientWithExternalIDs(map[string]string{})
			Expect(err).NotTo(HaveOccurred())
			Expect(checkBridgeMapping(ovsClient, ovntypes.LocalnetTopology, ovntypes.DefaultNetworkName)).To(Succeed())
		})
	})

	Context("when bridge mapping exists in external IDs", func() {
		It("should return nil if the bridge mapping is found", func() {
			ovsClient, err := newOVSClientWithExternalIDs(map[string]string{
				"ovn-bridge-mappings": "test-network:br-int",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(checkBridgeMapping(ovsClient, ovntypes.LocalnetTopology, networkName)).To(Succeed())
		})

		It("should return error if the bridge mapping isn't found", func() {
			ovsClient, err := newOVSClientWithExternalIDs(map[string]string{
				"ovn-bridge-mappings": "other-network:br-int",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(checkBridgeMapping(ovsClient, ovntypes.LocalnetTopology, networkName).Error()).To(
				Equal(`failed to find OVN bridge-mapping for network: "test-network"`))
		})
	})
})

func newOVSClientWithExternalIDs(externalIDs map[string]string) (client.Client, error) {
	ovsClient, _, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{
				ExternalIDs: externalIDs,
			},
		},
	})
	return ovsClient, err
}
