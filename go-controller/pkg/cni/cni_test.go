// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"

	"github.com/ovn-kubernetes/libovsdb/client"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
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
				Name: "host_" + pr.IfName,
			},
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

// podRequestToHTTPRequest builds the *http.Request that cnishim would POST to
// the cniserver's "/" endpoint, given a PodRequest.
// The reverse mapping (HTTP body -> PodRequest) lives in
// cniserver.go:cniRequestToPodRequest; this helper must stay aligned with it.
func podRequestToHTTPRequest(pr *PodRequest) *http.Request {
	confBytes, err := json.Marshal(pr.CNIConf)
	Expect(err).NotTo(HaveOccurred())
	cniArgs := fmt.Sprintf("K8S_POD_NAMESPACE=%s;K8S_POD_NAME=%s", pr.PodNamespace, pr.PodName)
	if pr.PodUID != "" {
		cniArgs += ";K8S_POD_UID=" + pr.PodUID
	}
	wireReq := &Request{
		Env: map[string]string{
			"CNI_COMMAND":     string(pr.Command),
			"CNI_CONTAINERID": pr.SandboxID,
			"CNI_NETNS":       pr.Netns,
			"CNI_IFNAME":      pr.IfName,
			"CNI_ARGS":        cniArgs,
		},
		Config:     confBytes,
		DeviceInfo: pr.deviceInfo,
	}
	body, err := json.Marshal(wireReq)
	Expect(err).NotTo(HaveOccurred())
	res, err := http.NewRequest(http.MethodPost, "http://dummy/", bytes.NewReader(body))
	Expect(err).NotTo(HaveOccurred())
	return res
}

func getTestServer(factory factory.NodeWatchFactory, kclient kubernetes.Interface, networkManager networkmanager.Interface) *Server {
	cs := &ClientSet{
		podLister: factory.PodCoreInformer().Lister(),
		kclient:   kclient,
	}
	if util.IsNetworkSegmentationSupportEnabled() {
		cs.nadLister = factory.NADInformer().Lister()
	}
	return &Server{
		clientSet: cs,
		kubeAuth: &KubeAPIAuth{
			Kubeconfig:       config.Kubernetes.Kubeconfig,
			KubeAPIServer:    config.Kubernetes.APIServer,
			KubeAPIToken:     config.Kubernetes.Token,
			KubeAPITokenFile: config.Kubernetes.TokenFile,
		},
		networkManager: networkManager,
	}
}

var _ = Describe("Network Segmentation", func() {
	var (
		pr                 PodRequest
		pod                *corev1.Pod
		fakeNetworkManager *networkmanager.FakeNetworkManager
		prInterfaceOpsStub *podRequestInterfaceOpsStub
		cniServer          *Server
		wf                 factory.NodeWatchFactory
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.IPv4Mode = true
		config.IPv6Mode = true

		prInterfaceOpsStub = &podRequestInterfaceOpsStub{}
		podRequestInterfaceOps = prInterfaceOpsStub

		fakeNetworkManager = &networkmanager.FakeNetworkManager{
			PrimaryNetworks: make(map[string]util.NetInfo),
		}

		pr = PodRequest{
			Command:      CNIAdd,
			PodNamespace: "foo-ns",
			PodName:      "bar-pod",
			SandboxID:    "824bceff24af3",
			Netns:        "ns",
			IfName:       "eth0",
			CNIConf: &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{
					CNIVersion: "1.0.0",
					Type:       "ovn-k8s-cni-overlay",
				},
				DeviceID: "",
			},
			netName: ovntypes.DefaultNetworkName,
			nadName: ovntypes.DefaultNetworkName,
			nadKey:  ovntypes.DefaultNetworkName,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		DeferCleanup(cancel)
		pr.ctx = ctx
	})
	AfterEach(func() {
		podRequestInterfaceOps = &defaultPodRequestInterfaceOps{}
		if wf != nil {
			wf.Shutdown()
			wf = nil
		}
		cniServer = nil
	})

	startCNIServer := func(objects ...runtime.Object) {
		fakeClient := util.GetOVNClientset(objects...).GetNodeClientset()
		var err error
		wf, err = factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		Expect(wf.Start()).To(Succeed())

		cniServer = getTestServer(wf, fakeClient.KubeClient, fakeNetworkManager)
	}

	handlePodRequest := func() *Response {
		res, err := cniServer.handleCNIRequest(podRequestToHTTPRequest(&pr))
		Expect(err).NotTo(HaveOccurred())
		response := &Response{}
		err = json.Unmarshal(res, response)
		Expect(err).NotTo(HaveOccurred())
		return response
	}

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
			startCNIServer(testing.NewNamespace(pod.Namespace), pod)

			By("cmdAdd primary pod interface should be added")
			response := handlePodRequest()
			Expect(response.Result).NotTo(BeNil())
			Expect(response.Result.Interfaces).To(HaveLen(2))
			By("cmdDel primary pod interface should be removed")
			pr.Command = CNIDel
			handlePodRequest()
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
					startCNIServer(testing.NewNamespace(pod.Namespace), pod)
					By("cmdAdd primary pod interface should be added")
					response := handlePodRequest()

					Expect(response.Result).NotTo(BeNil())
					Expect(response.Result.Interfaces).To(HaveLen(2))
					Expect(response.PrimaryUDNPodInfo).To(BeNil())
					Expect(response.PrimaryUDNPodReq).To(BeNil())
					By("cmdDel primary pod interface should be removed")
					pr.Command = CNIDel
					handlePodRequest()
					Expect(prInterfaceOpsStub.unconfiguredInterfaces).To(HaveLen(1))
				})
			})

			Context("with CNI Unprivileged Mode", func() {
				BeforeEach(func() {
					config.UnprivilegedMode = true
				})
				It("should not fail at cmdAdd", func() {
					startCNIServer(testing.NewNamespace(pod.Namespace), pod)
					response := handlePodRequest()
					Expect(response.Result).To(BeNil())
					Expect(response.PodIFInfo).NotTo(BeNil())
					Expect(response.PrimaryUDNPodReq).To(BeNil())
					Expect(response.PrimaryUDNPodInfo).To(BeNil())
				})
				It("should not fail at cmdDel", func() {
					startCNIServer(testing.NewNamespace(pod.Namespace), pod)
					pr.Command = CNIDel
					response := handlePodRequest()
					Expect(response.Result).To(BeNil())
					Expect(response.PodIFInfo).NotTo(BeNil())
				})
			})
		})

		Context("pod with a user defined primary network", func() {
			const namespace = "foo-ns"

			var nadMegaNet runtime.Object

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
				nad := testing.GenerateNADWithConfig("meganet", namespace, dummyPrimaryUDNConfig(namespace, "meganet"))
				nadMegaNet = nad
				nadNetwork, err := util.ParseNADInfo(nad)
				Expect(err).NotTo(HaveOccurred())
				fakeNetworkManager.PrimaryNetworks[namespace] = nadNetwork
				fakeNetworkManager.NADNetworks = map[string]util.NetInfo{
					namespace + "/meganet": nadNetwork,
				}
			})

			Context("with CNI Privileged Mode", func() {
				It("should return the information of both the default net and the primary UDN in the result", func() {
					startCNIServer(testing.NewNamespace(pod.Namespace), pod, nadMegaNet)
					response := handlePodRequest()

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
					sandbox := "/var/run/netns/" + pod.Namespace + "_" + pod.Name
					Expect(response.Result).To(Equal(
						&current.Result{
							Interfaces: []*current.Interface{
								{Name: "host_eth0"},
								{Name: "eth0", Sandbox: sandbox},
								{Name: "host_ovn-udn1"},
								{Name: "ovn-udn1", Sandbox: sandbox},
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
					startCNIServer(testing.NewNamespace(pod.Namespace), pod, nadMegaNet)
					response := handlePodRequest()

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
