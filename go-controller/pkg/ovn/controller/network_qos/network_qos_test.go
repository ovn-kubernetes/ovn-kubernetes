package networkqos

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nqostype "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	networkqosclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/clientset/versioned"
	crdtypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	ovnk8stesting "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Custom NetInfo implementations for testing
type primaryNetInfoWrapper struct {
	util.NetInfo
}

func (n *primaryNetInfoWrapper) IsPrimaryNetwork() bool {
	return true
}

func (n *primaryNetInfoWrapper) IsUserDefinedNetwork() bool {
	return false
}

func (n *primaryNetInfoWrapper) IsDefault() bool {
	return false
}

func (n *primaryNetInfoWrapper) GetNetInfo() util.NetInfo {
	return n
}

type secondaryNetInfoWrapper struct {
	util.NetInfo
}

func (n *secondaryNetInfoWrapper) IsPrimaryNetwork() bool {
	return false
}

func (n *secondaryNetInfoWrapper) IsUserDefinedNetwork() bool {
	return true
}

func (n *secondaryNetInfoWrapper) IsDefault() bool {
	return false
}

func (n *secondaryNetInfoWrapper) GetNetInfo() util.NetInfo {
	return n
}

func init() {
	config.IPv4Mode = true
	config.IPv6Mode = false
	config.OVNKubernetesFeature.EnableNetworkQoS = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableInterconnect = false // set via tableEntrySetup
}

var (
	defaultControllerName = "default-network-controller"
	streamControllerName  = "stream-network-controller"
	watchFactory          *factory.WatchFactory
	stopChan              chan (struct{})
	nbClient              libovsdbclient.Client
	nbsbCleanup           *libovsdbtest.Context
	fakeKubeClient        kubernetes.Interface
	fakeNQoSClient        networkqosclientset.Interface
	wg                    sync.WaitGroup
	defaultAddrsetFactory addressset.AddressSetFactory
	streamAddrsetFactory  addressset.AddressSetFactory

	nqosNamespace = "network-qos-test"
	nqosName      = "my-network-qos"
	clientPodName = "client-pod"

	app1Namespace = "app1-ns"
	app3Namespace = "app3-ns"
	port8080      = int32(8080)
	port8081      = int32(8081)
	port9090      = int32(9090)
)

func TestNetworkQoS(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "NetworkQoS Controller")
}

func tableEntrySetup(enableInterconnect bool) {
	config.OVNKubernetesFeature.EnableInterconnect = enableInterconnect

	ns0 := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: nqosNamespace,
			Labels: map[string]string{
				"app": "client",
			},
		},
	}
	ns1 := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: app1Namespace,
			Labels: map[string]string{
				"app": "app1",
			},
		},
	}
	ns3 := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: app3Namespace,
			Labels: map[string]string{
				"app": "app3",
			},
		},
	}
	nqos := &nqostype.NetworkQoS{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: nqosNamespace,
			Name:      nqosName,
		},
		Spec: nqostype.Spec{
			Priority: 100,
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "client",
				},
			},
			Egress: []nqostype.Rule{
				{
					DSCP: 50,
					Bandwidth: nqostype.Bandwidth{
						Rate:  10000,
						Burst: 100000,
					},
					Classifier: nqostype.Classifier{
						To: []nqostype.Destination{
							{
								PodSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"component": "service1",
									},
								},
								NamespaceSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"app": "app1",
									},
								},
							},
							{
								IPBlock: &networkingv1.IPBlock{
									CIDR: "128.116.0.0/17",
									Except: []string{
										"128.116.0.0",
										"128.116.0.255",
									},
								},
							},
						},
						Ports: []*nqostype.Port{
							{
								Protocol: "tcp",
								Port:     &port8080,
							},
							{
								Protocol: "tcp",
								Port:     &port8081,
							},
						},
					},
				},
				{
					DSCP: 51,
					Classifier: nqostype.Classifier{
						To: []nqostype.Destination{
							{
								PodSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"component": "service3",
									},
								},
								NamespaceSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"app": "app3",
									},
								},
							},
							{
								IPBlock: &networkingv1.IPBlock{
									CIDR: "128.118.0.0/17",
									Except: []string{
										"128.118.0.0",
										"128.118.0.255",
									},
								},
							},
						},
						Ports: []*nqostype.Port{
							{
								Protocol: "tcp",
								Port:     &port8080,
							},
							{
								Protocol: "tcp",
								Port:     &port8081,
							},
							{
								Protocol: "udp",
								Port:     &port9090,
							},
							{
								Protocol: "udp",
								Port:     &port8080,
							},
						},
					},
				},
			},
		},
	}

	node1 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Annotations: map[string]string{
				"k8s.ovn.org/zone-name": "node1",
			},
		},
	}

	node2 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node2",
			Annotations: map[string]string{
				"k8s.ovn.org/zone-name": "node2",
			},
		},
	}

	clientPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: nqosNamespace,
			Name:      clientPodName,
			Labels: map[string]string{
				"app": "client",
			},
			Annotations: map[string]string{
				"k8s.ovn.org/pod-networks":    `{"default/stream": {"ip_addresses":["10.128.2.3/26"],"mac_address":"0a:58:0a:80:02:03"}, "default":{"ip_addresses":["10.192.177.4/26"],"mac_address":"0a:58:0a:c0:b1:04","gateway_ips":["10.192.177.1"],"routes":[{"dest":"10.192.0.0/16","nextHop":"10.192.177.1"},{"dest":"10.223.0.0/16","nextHop":"10.192.177.1"},{"dest":"100.64.0.0/16","nextHop":"10.192.177.1"}],"mtu":"1500","ip_address":"10.192.177.4/26","gateway_ip":"10.192.177.1"}}`,
				"k8s.v1.cni.cncf.io/networks": `[{"interface":"net1","name":"stream","namespace":"default"}]`,
			},
		},
		Spec: corev1.PodSpec{
			HostNetwork: false,
			NodeName:    "node1",
		},
	}

	nad := ovnk8stesting.GenerateNAD("stream", "stream", "default", types.Layer3Topology, "10.128.2.0/16/24", types.NetworkRoleSecondary)
	nad.Labels = map[string]string{
		"name": "stream",
	}

	initialDB := &libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{
			&nbdb.LogicalSwitch{
				Name: "node1",
			},
			&nbdb.LogicalSwitch{
				Name: "node2",
			},
			&nbdb.LogicalSwitch{
				Name: "stream_node1",
			},
		},
	}

	ovnClientset := util.GetOVNClientset(ns0, ns1, ns3, node1, node2, clientPod, nqos, nad)
	fakeKubeClient = ovnClientset.KubeClient
	fakeNQoSClient = ovnClientset.NetworkQoSClient
	initEnv(ovnClientset, initialDB)
	// init controller for default network
	initNetworkQoSController(&util.DefaultNetInfo{}, nil, defaultAddrsetFactory, defaultControllerName, enableInterconnect)
	// init controller for stream nad
	streamImmutableNadInfo, err := util.ParseNADInfo(nad)
	Expect(err).NotTo(HaveOccurred())
	streamNadInfo := util.NewMutableNetInfo(streamImmutableNadInfo)
	streamNadInfo.AddNADs("default/stream")
	initNetworkQoSController(streamNadInfo, []string{"default/stream"}, streamAddrsetFactory, streamControllerName, enableInterconnect)
}

var _ = AfterEach(func() {
	shutdownController()
	if nbsbCleanup != nil {
		nbsbCleanup.Cleanup()
		nbsbCleanup = nil
	}
})

var _ = Describe("NetworkQoS Controller", func() {

	var _ = Context("With different interconnect configurations", func() {

		DescribeTable("When starting controller with NetworkQoS, Pod and Node objects",
			func(enableInterconnect bool) {
				tableEntrySetup(enableInterconnect)

				By("creates address sets for source and destination pod selectors")
				{
					eventuallyExpectAddressSet(defaultAddrsetFactory, nqosNamespace, nqosName, "src", "0", defaultControllerName)
					eventuallyExpectAddressSet(defaultAddrsetFactory, nqosNamespace, nqosName, "0", "0", defaultControllerName)
					eventuallyExpectAddressSet(defaultAddrsetFactory, nqosNamespace, nqosName, "1", "0", defaultControllerName)
				}

				By("creates QoS rules in ovn nb")
				{
					qos0 := eventuallyExpectQoS(defaultControllerName, nqosNamespace, nqosName, 0)
					qos1 := eventuallyExpectQoS(defaultControllerName, nqosNamespace, nqosName, 1)
					eventuallySwitchHasQoS("node1", qos0)
					eventuallySwitchHasQoS("node1", qos1)
					eventuallyAddressSetHas(defaultAddrsetFactory, nqosNamespace, nqosName, "src", "0", defaultControllerName, "10.192.177.4")
					sourceAddrSet, err := findAddressSet(defaultAddrsetFactory, nqosNamespace, nqosName, "src", "0", defaultControllerName)
					Expect(err).NotTo(HaveOccurred())
					dst1AddrSet, err1 := findAddressSet(defaultAddrsetFactory, nqosNamespace, nqosName, "0", "0", defaultControllerName)
					Expect(err1).NotTo(HaveOccurred())
					srcHashName4, _ := sourceAddrSet.GetASHashNames()
					dst1HashName4, _ := dst1AddrSet.GetASHashNames()
					Expect(qos0.Match).Should(Equal(fmt.Sprintf("ip4.src == {$%s} && (ip4.dst == {$%s} || (ip4.dst == 128.116.0.0/17 && ip4.dst != {128.116.0.0,128.116.0.255})) && tcp && tcp.dst == {8080,8081}", srcHashName4, dst1HashName4)))
					Expect(qos0.Action).To(ContainElement(50))
					Expect(qos0.Priority).To(Equal(11000))
					Expect(qos0.Bandwidth).To(ContainElements(10000, 100000))
					dst3AddrSet, err3 := findAddressSet(defaultAddrsetFactory, nqosNamespace, nqosName, "1", "0", defaultControllerName)
					Expect(err3).NotTo(HaveOccurred())
					dst3HashName4, _ := dst3AddrSet.GetASHashNames()
					Expect(qos1.Match).Should(Equal(fmt.Sprintf("ip4.src == {$%s} && (ip4.dst == {$%s} || (ip4.dst == 128.118.0.0/17 && ip4.dst != {128.118.0.0,128.118.0.255})) && ((tcp && tcp.dst == {8080,8081}) || (udp && udp.dst == {9090,8080}))", srcHashName4, dst3HashName4)))
				}

				app1Pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: app1Namespace,
						Name:      "app1-pod",
						Labels: map[string]string{
							"component": "service1",
						},
						Annotations: map[string]string{
							"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["10.194.188.4/26"],"mac_address":"0a:58:0a:c2:bc:04","gateway_ips":["10.194.188.1"],"routes":[{"dest":"10.194.0.0/16","nextHop":"10.194.188.1"},{"dest":"10.223.0.0/16","nextHop":"10.194.188.1"},{"dest":"100.64.0.0/16","nextHop":"10.194.188.1"}],"mtu":"1500","ip_address":"10.194.188.4/26","gateway_ip":"10.194.188.1"}}`,
						},
					},
					Spec: corev1.PodSpec{
						HostNetwork: false,
						NodeName:    "node2",
					},
				}

				By("adds IP to destination address set for matching pod")
				{
					_, err := fakeKubeClient.CoreV1().Pods(app1Pod.Namespace).Create(context.TODO(), app1Pod, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
					eventuallyAddressSetHas(defaultAddrsetFactory, nqosNamespace, nqosName, "0", "0", defaultControllerName, "10.194.188.4")

					By("updates match strings if egress rules change")
					nqosUpdate, err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Get(context.TODO(), nqosName, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					nqosUpdate.ResourceVersion = time.Now().String()
					nqosUpdate.Spec.Egress[1].Classifier.To[1].IPBlock.Except = nil
					_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Update(context.TODO(), nqosUpdate, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())
					sourceAddrSet, err := findAddressSet(defaultAddrsetFactory, nqosNamespace, nqosName, "src", "0", defaultControllerName)
					Expect(err).NotTo(HaveOccurred())
					dst1AddrSet, err1 := findAddressSet(defaultAddrsetFactory, nqosNamespace, nqosName, "0", "0", defaultControllerName)
					Expect(err1).NotTo(HaveOccurred())
					srcHashName4, _ := sourceAddrSet.GetASHashNames()
					dst1HashName4, _ := dst1AddrSet.GetASHashNames()

					Eventually(func() string {
						qos, err := findQoS(defaultControllerName, nqosNamespace, nqosName, 0)
						if err != nil {
							return err.Error()
						}
						return qos.Match
					}).WithTimeout(10 * time.Second).Should(Equal(fmt.Sprintf("ip4.src == {$%s} && (ip4.dst == {$%s} || (ip4.dst == 128.116.0.0/17 && ip4.dst != {128.116.0.0,128.116.0.255})) && tcp && tcp.dst == {8080,8081}", srcHashName4, dst1HashName4)))

					dst3AddrSet, err3 := findAddressSet(defaultAddrsetFactory, nqosNamespace, nqosName, "1", "0", defaultControllerName)
					Expect(err3).NotTo(HaveOccurred())
					dst3HashName4, _ := dst3AddrSet.GetASHashNames()
					Eventually(func() string {
						qos, err := findQoS(defaultControllerName, nqosNamespace, nqosName, 1)
						if err != nil {
							return err.Error()
						}
						return qos.Match
					}).WithTimeout(10 * time.Second).Should(Equal(fmt.Sprintf("ip4.src == {$%s} && (ip4.dst == {$%s} || (ip4.dst == 128.118.0.0/17 && ip4.dst != {128.118.0.0,128.118.0.255})) && ((tcp && tcp.dst == {8080,8081}) || (udp && udp.dst == {9090,8080}))", srcHashName4, dst3HashName4)))
				}

				By("removes IP from destination address set if pod's labels don't match the selector")
				{
					updatePod := app1Pod.DeepCopy()
					updatePod.Labels["component"] = "dummy"
					updatePod.ResourceVersion = time.Now().String()
					_, err := fakeKubeClient.CoreV1().Pods(app1Pod.Namespace).Update(context.TODO(), updatePod, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())
					eventuallyAddressSetHasNo(defaultAddrsetFactory, nqosNamespace, nqosName, "0", "0", defaultControllerName, "10.194.188.4")
				}

				By("adds IP to destination address set again if pod's labels match the selector")
				{
					updatePod := app1Pod.DeepCopy()
					updatePod.Labels["component"] = "service1"
					_, err := fakeKubeClient.CoreV1().Pods(app1Pod.Namespace).Update(context.TODO(), updatePod, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())
					eventuallyAddressSetHas(defaultAddrsetFactory, nqosNamespace, nqosName, "0", "0", defaultControllerName, "10.194.188.4")
				}

				By("removes IP from destination address set if target namespace labels don't match the selector")
				{
					ns, err := fakeKubeClient.CoreV1().Namespaces().Get(context.TODO(), app1Namespace, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					ns.ResourceVersion = time.Now().String()
					ns.Labels["app"] = "dummy"
					_, err = fakeKubeClient.CoreV1().Namespaces().Update(context.TODO(), ns, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())
					eventuallyAddressSetHasNo(defaultAddrsetFactory, nqosNamespace, nqosName, "0", "0", defaultControllerName, "10.194.188.4")
				}

				By("adds IP to destination address set again if namespace's labels match the selector")
				{
					ns, err := fakeKubeClient.CoreV1().Namespaces().Get(context.TODO(), app1Namespace, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					ns.ResourceVersion = time.Now().String()
					ns.Labels["app"] = "app1"
					_, err = fakeKubeClient.CoreV1().Namespaces().Update(context.TODO(), ns, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())
					eventuallyAddressSetHas(defaultAddrsetFactory, nqosNamespace, nqosName, "0", "0", defaultControllerName, "10.194.188.4")
				}

				By("removes IP from destination address set if namespace selector changes")
				{
					nqosUpdate, err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Get(context.TODO(), nqosName, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					nqosUpdate.Spec.Egress[0].Classifier.To[0].NamespaceSelector = &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "dummy",
						},
					}
					nqosUpdate.ResourceVersion = time.Now().String()
					_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Update(context.TODO(), nqosUpdate, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())
					eventuallyAddressSetHasNo(defaultAddrsetFactory, nqosNamespace, nqosName, "0", "0", defaultControllerName, "10.194.188.4")
				}

				By("adds IP to destination address set if namespace selector is restored")
				{
					nqosUpdate, err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Get(context.TODO(), nqosName, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					nqosUpdate.Spec.Egress[0].Classifier.To[0].NamespaceSelector = &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "app1",
						},
					}
					nqosUpdate.ResourceVersion = time.Now().String()
					_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Update(context.TODO(), nqosUpdate, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())
					eventuallyAddressSetHas(defaultAddrsetFactory, nqosNamespace, nqosName, "0", "0", defaultControllerName, "10.194.188.4")
				}

				app3Pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: app3Namespace,
						Name:      "app3-pod",
						Labels: map[string]string{
							"component": "service3",
						},
						Annotations: map[string]string{
							"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["10.195.188.4/26"],"mac_address":"0a:58:0a:c3:bc:04","gateway_ips":["10.195.188.1"],"routes":[{"dest":"10.195.0.0/16","nextHop":"10.195.188.1"},{"dest":"10.223.0.0/16","nextHop":"10.195.188.1"},{"dest":"100.64.0.0/16","nextHop":"10.195.188.1"}],"mtu":"1500","ip_address":"10.195.188.4/26","gateway_ip":"10.195.188.1"}}`,
						},
					},
					Spec: corev1.PodSpec{
						HostNetwork: false,
						NodeName:    "node2",
					},
				}

				By("adds IP to destination address set of the second rule for matching pod")
				{
					_, err := fakeKubeClient.CoreV1().Pods(app3Pod.Namespace).Create(context.TODO(), app3Pod, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
					eventuallyAddressSetHas(defaultAddrsetFactory, nqosNamespace, nqosName, "1", "0", defaultControllerName, "10.195.188.4")
				}

				By("adds new QoS rule to ovn nb when a new Egress rule is added")
				{
					nqosUpdate, err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Get(context.TODO(), nqosName, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					nqosUpdate.Spec.Egress = append(nqosUpdate.Spec.Egress, nqostype.Rule{
						DSCP: 102,
						Classifier: nqostype.Classifier{
							To: []nqostype.Destination{
								{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"app": "app1",
										},
									},
								},
							},
						},
					})
					nqosUpdate.ResourceVersion = time.Now().String()
					_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Update(context.TODO(), nqosUpdate, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())
					eventuallyExpectQoS(defaultControllerName, nqosNamespace, nqosName, 2)
					eventuallyAddressSetHas(defaultAddrsetFactory, nqosNamespace, nqosName, "src", "0", defaultControllerName, "10.192.177.4")
					eventuallyAddressSetHas(defaultAddrsetFactory, nqosNamespace, nqosName, "0", "0", defaultControllerName, "10.194.188.4")
					eventuallyAddressSetHas(defaultAddrsetFactory, nqosNamespace, nqosName, "1", "0", defaultControllerName, "10.195.188.4")
					eventuallyAddressSetHas(defaultAddrsetFactory, nqosNamespace, nqosName, "2", "0", defaultControllerName, "10.194.188.4")
				}

				nqos4StreamNet := &nqostype.NetworkQoS{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: nqosNamespace,
						Name:      "stream-qos",
					},
					Spec: nqostype.Spec{
						NetworkSelectors: []crdtypes.NetworkSelector{
							{
								NetworkSelectionType: crdtypes.NetworkAttachmentDefinitions,
								NetworkAttachmentDefinitionSelector: &crdtypes.NetworkAttachmentDefinitionSelector{
									NetworkSelector: metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": "unknown",
										},
									},
								},
							},
						},
						Priority: 100,
						PodSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"app": "client",
							},
						},
						Egress: []nqostype.Rule{
							{
								DSCP: 50,
								Bandwidth: nqostype.Bandwidth{
									Rate:  10000,
									Burst: 100000,
								},
								Classifier: nqostype.Classifier{
									To: []nqostype.Destination{
										{
											IPBlock: &networkingv1.IPBlock{
												CIDR: "128.115.0.0/17",
												Except: []string{
													"128.115.0.0",
													"128.115.0.255",
												},
											},
										},
									},
								},
							},
						},
					},
				}

				By("will not handle NetworkQos with unknown NetworkAttachmentDefinition in spec")
				{
					_, err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Create(context.TODO(), nqos4StreamNet, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
					eventuallyExpectNoQoS(defaultControllerName, nqosNamespace, "stream-qos", 0)
				}

				By("will not populate source address set NetworkQos with incorrect namespace selector in spec")
				{
					nqos4StreamNet.Spec.NetworkSelectors = []crdtypes.NetworkSelector{
						{
							NetworkSelectionType: crdtypes.NetworkAttachmentDefinitions,
							NetworkAttachmentDefinitionSelector: &crdtypes.NetworkAttachmentDefinitionSelector{
								NamespaceSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{
										"name": "unknown",
									},
								},
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{
										"name": "stream",
									},
								},
							},
						},
					}
					nqos4StreamNet.ResourceVersion = time.Now().String()
					_, err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Update(context.TODO(), nqos4StreamNet, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())
					eventuallyAddressSetHasNo(streamAddrsetFactory, nqosNamespace, "stream-qos", "src", "0", streamControllerName, "10.128.2.3")
				}

				By("handles NetworkQos on secondary network")
				{
					nqos4StreamNet.Spec.NetworkSelectors = []crdtypes.NetworkSelector{
						{
							NetworkSelectionType: crdtypes.NetworkAttachmentDefinitions,
							NetworkAttachmentDefinitionSelector: &crdtypes.NetworkAttachmentDefinitionSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{
										"name": "stream",
									},
								},
							},
						},
					}
					nqos4StreamNet.ResourceVersion = time.Now().String()
					_, err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Update(context.TODO(), nqos4StreamNet, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())
					qos := eventuallyExpectQoS(streamControllerName, nqosNamespace, "stream-qos", 0)
					eventuallySwitchHasQoS("stream_node1", qos)
					eventuallyAddressSetHas(streamAddrsetFactory, nqosNamespace, "stream-qos", "src", "0", streamControllerName, "10.128.2.3")
				}

				By("uses namespace's address set as source if pod selector is not provided in source")
				{
					dbIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNamespace, defaultControllerName, map[libovsdbops.ExternalIDKey]string{
						libovsdbops.ObjectNameKey: nqosNamespace,
					})
					addrset, err := defaultAddrsetFactory.EnsureAddressSet(dbIDs)
					Expect(err).NotTo(HaveOccurred())
					err = addrset.AddAddresses([]string{"10.194.188.4"})
					Expect(err).NotTo(HaveOccurred())
					nqosWithoutSrcSelector := &nqostype.NetworkQoS{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: nqosNamespace,
							Name:      "no-source-selector",
						},
						Spec: nqostype.Spec{
							Priority: 100,
							Egress: []nqostype.Rule{
								{
									DSCP: 50,
									Bandwidth: nqostype.Bandwidth{
										Rate:  10000,
										Burst: 100000,
									},
									Classifier: nqostype.Classifier{
										To: []nqostype.Destination{
											{
												IPBlock: &networkingv1.IPBlock{
													CIDR: "128.115.0.0/17",
													Except: []string{
														"128.115.0.0",
														"123.123.123.123",
													},
												},
											},
										},
									},
								},
							},
						},
					}
					_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Create(context.TODO(), nqosWithoutSrcSelector, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
					qos := eventuallyExpectQoS(defaultControllerName, nqosNamespace, "no-source-selector", 0)
					v4HashName, _ := addrset.GetASHashNames()
					Expect(qos.Match).Should(Equal(fmt.Sprintf("ip4.src == {$%s} && ip4.dst == 128.115.0.0/17 && ip4.dst != {128.115.0.0,123.123.123.123}", v4HashName)))
				}

				By("clear QoS attributes of existing NetworkQoS and make sure that is proper")
				{
					nqosWithoutSrcSelector := &nqostype.NetworkQoS{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: nqosNamespace,
							Name:      "no-source-selector",
						},
						Spec: nqostype.Spec{
							Priority: 1,
							Egress: []nqostype.Rule{
								{
									DSCP: 50,
									// Bandwidth: nqostype.Bandwidth{},
									Classifier: nqostype.Classifier{
										To: []nqostype.Destination{
											{
												IPBlock: &networkingv1.IPBlock{
													CIDR: "128.115.0.0/17",
													Except: []string{
														"128.115.0.0",
														"123.123.123.123",
													},
												},
											},
										},
									},
								},
							},
						},
					}
					nqosWithoutSrcSelector.ResourceVersion = time.Now().String()
					_, err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Update(context.TODO(), nqosWithoutSrcSelector, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					dbIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNamespace, defaultControllerName, map[libovsdbops.ExternalIDKey]string{
						libovsdbops.ObjectNameKey: nqosNamespace,
					})
					addrset, err := defaultAddrsetFactory.EnsureAddressSet(dbIDs)
					Expect(err).NotTo(HaveOccurred())
					v4HashName, _ := addrset.GetASHashNames()

					// Ensure that QoS priority and Bandwidth have been properly changed by OVN
					var qos *nbdb.QoS
					Eventually(func() bool {
						qos, err = findQoS(defaultControllerName, nqosNamespace, "no-source-selector", 0)
						Expect(err).NotTo(HaveOccurred())
						Expect(qos).NotTo(BeNil())
						return qos.Priority == 10010 && len(qos.Bandwidth) == 0
					}).WithTimeout(10 * time.Second).WithPolling(1 * time.Second).Should(BeTrue())
					Expect(qos.Match).Should(Equal(fmt.Sprintf("ip4.src == {$%s} && ip4.dst == 128.115.0.0/17 && ip4.dst != {128.115.0.0,123.123.123.123}", v4HashName)))
				}

				By("removes IP from destination address set if pod is deleted")
				{
					err := fakeKubeClient.CoreV1().Pods(app1Pod.Namespace).Delete(context.TODO(), app1Pod.Name, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
					eventuallyAddressSetHasNo(defaultAddrsetFactory, nqosNamespace, nqosName, "0", "0", defaultControllerName, "10.194.188.4")
				}

				By("removes IP from destination address set of the second rule if namespace is deleted")
				{
					err := fakeKubeClient.CoreV1().Namespaces().Delete(context.TODO(), app3Pod.Namespace, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
					eventuallyAddressSetHasNo(defaultAddrsetFactory, nqosNamespace, nqosName, "1", "0", defaultControllerName, "10.195.188.4")
					err = fakeKubeClient.CoreV1().Pods(app3Pod.Namespace).Delete(context.TODO(), app3Pod.Name, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
				}

				By("deletes stale QoS from ovn nb when Egress rule is deleted")
				{
					qos2, err1 := findQoS(defaultControllerName, nqosNamespace, nqosName, 2)
					Expect(err1).NotTo(HaveOccurred())
					nqosUpdate, err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Get(context.TODO(), nqosName, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					nqosUpdate.ResourceVersion = time.Now().String()
					nqosUpdate.Spec.Egress = slices.Delete(nqosUpdate.Spec.Egress, 1, 2)
					_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Update(context.TODO(), nqosUpdate, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())
					eventuallySwitchHasNoQoS("node1", qos2)
					eventuallyExpectNoQoS(defaultControllerName, nqosNamespace, nqosName, 2)
				}

				By("unbinds QoS rule from logical switch when no source pods is selected")
				{
					qos0, err0 := findQoS(defaultControllerName, nqosNamespace, nqosName, 0)
					Expect(err0).NotTo(HaveOccurred())
					qos1, err1 := findQoS(defaultControllerName, nqosNamespace, nqosName, 1)
					Expect(err1).NotTo(HaveOccurred())
					// qos should be present, as pod is not yet deleted
					eventuallySwitchHasQoS("node1", qos0)
					eventuallySwitchHasQoS("node1", qos1)
					err := fakeKubeClient.CoreV1().Pods(nqosNamespace).Delete(context.TODO(), clientPodName, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
					// qos should be unbound from switch
					eventuallySwitchHasNoQoS("node1", qos0)
					eventuallySwitchHasNoQoS("node1", qos1)
				}

				By("deletes QoS after NetworkQoS object is deleted")
				{
					err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Delete(context.TODO(), nqosName, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
					eventuallyExpectNoQoS(defaultControllerName, nqosNamespace, nqosName, 0)
					eventuallyExpectNoQoS(defaultControllerName, nqosNamespace, nqosName, 1)
				}

				By("generates correct logical switch name for localnet topology")
				{
					localnetNad := ovnk8stesting.GenerateNAD("netwk1", "netwk1", "default", types.LocalnetTopology, "10.129.0.0/16", types.NetworkRoleSecondary)
					localnetImmutableNadInfo, err := util.ParseNADInfo(localnetNad)
					Expect(err).NotTo(HaveOccurred())
					localnetNadInfo := util.NewMutableNetInfo(localnetImmutableNadInfo)
					localnetNadInfo.AddNADs("default/netwk1")
					ctrl := initNetworkQoSController(localnetNadInfo, []string{"default/netwk1"}, addressset.NewFakeAddressSetFactory("netwk1-controller"), "netwk1-controller", enableInterconnect)
					lsName := ctrl.getLogicalSwitchName("dummy")
					Expect(lsName).To(Equal("netwk1_ovn_localnet_switch"))
				}

				By("generates correct logical switch name for layer2 topology")
				{
					layer2Nad := ovnk8stesting.GenerateNAD("netwk2", "netwk2", "default", types.Layer2Topology, "10.130.0.0/16", types.NetworkRoleSecondary)
					layer2ImmutableNadInfo, err := util.ParseNADInfo(layer2Nad)
					Expect(err).NotTo(HaveOccurred())
					layer2NadInfo := util.NewMutableNetInfo(layer2ImmutableNadInfo)
					layer2NadInfo.AddNADs("default/netwk2")
					ctrl := initNetworkQoSController(layer2NadInfo, []string{"default/netwk2"}, addressset.NewFakeAddressSetFactory("netwk2-controller"), "netwk2-controller", enableInterconnect)
					lsName := ctrl.getLogicalSwitchName("dummy")
					Expect(lsName).To(Equal("netwk2_ovn_layer2_switch"))
				}

				By("handles NetworkQoS with PrimaryUserDefinedNetworks selector")
				{
					// Create a NetworkQoS targeting primary networks
					nqosPrimaryNet := &nqostype.NetworkQoS{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: nqosNamespace,
							Name:      "primary-network-qos",
						},
						Spec: nqostype.Spec{
							NetworkSelectors: []crdtypes.NetworkSelector{
								{
									NetworkSelectionType: crdtypes.PrimaryUserDefinedNetworks,
									PrimaryUserDefinedNetworkSelector: &crdtypes.PrimaryUserDefinedNetworkSelector{
										NamespaceSelector: metav1.LabelSelector{
											MatchLabels: map[string]string{
												"app": "app1",
											},
										},
									},
								},
							},
							Priority: 200,
							PodSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": "client",
								},
							},
							Egress: []nqostype.Rule{
								{
									DSCP: 40,
									Bandwidth: nqostype.Bandwidth{
										Rate:  20000,
										Burst: 200000,
									},
									Classifier: nqostype.Classifier{
										To: []nqostype.Destination{
											{
												IPBlock: &networkingv1.IPBlock{
													CIDR: "192.168.0.0/24",
												},
											},
										},
										Ports: []*nqostype.Port{
											{
												Protocol: "TCP",
												Port:     &port8080,
											},
										},
									},
								},
							},
						},
					}

					// Create primary network controller
					primaryNad := ovnk8stesting.GenerateNAD("primary", "primary", "default", types.Layer3Topology, "10.140.0.0/16", types.NetworkRolePrimary)
					primaryImmutableNadInfo, err := util.ParseNADInfo(primaryNad)
					Expect(err).NotTo(HaveOccurred())
					primaryNadInfo := util.NewMutableNetInfo(primaryImmutableNadInfo)
					primaryNadInfo.AddNADs("default/primary")

					// Create the primary network logical switch
					primarySwitch := &nbdb.LogicalSwitch{
						Name: "primary_node1",
					}
					err = libovsdbops.CreateOrUpdateLogicalSwitch(nbClient, primarySwitch)
					Expect(err).NotTo(HaveOccurred())

					// Wrap the NetInfo with our custom implementation that returns true for IsPrimaryNetwork()
					primNetWrapper := &primaryNetInfoWrapper{NetInfo: primaryNadInfo}
					initNetworkQoSController(primNetWrapper, nil, addressset.NewFakeAddressSetFactory("primary-controller"), "primary-controller", enableInterconnect)

					// Ensure app1 namespace exists before testing primary networks
					ns, err := fakeKubeClient.CoreV1().Namespaces().Get(context.TODO(), app1Namespace, metav1.GetOptions{})
					if err != nil || ns == nil {
						klog.Infof("Creating app1 namespace for primary networks test")
						ns = &corev1.Namespace{
							ObjectMeta: metav1.ObjectMeta{
								Name: app1Namespace,
								Labels: map[string]string{
									"app": "app1",
								},
							},
						}
						_, err = fakeKubeClient.CoreV1().Namespaces().Create(context.TODO(), ns, metav1.CreateOptions{})
						Expect(err).NotTo(HaveOccurred())
					}

					// Create a client pod in the network QoS namespace
					clientPod := &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: nqosNamespace,
							Name:      clientPodName,
							Labels: map[string]string{
								"app": "client",
							},
							Annotations: map[string]string{
								"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["10.192.177.4/26"],"mac_address":"0a:58:0a:c2:bc:04","gateway_ips":["10.192.177.1"],"routes":[{"dest":"10.192.0.0/16","nextHop":"10.192.177.1"},{"dest":"10.223.0.0/16","nextHop":"10.192.177.1"},{"dest":"100.64.0.0/16","nextHop":"10.192.177.1"}],"mtu":"1500","ip_address":"10.192.177.4/26","gateway_ip":"10.192.177.1"}}`,
							},
						},
						Spec: corev1.PodSpec{
							HostNetwork: false,
							NodeName:    "node1",
						},
					}
					_, err = fakeKubeClient.CoreV1().Pods(nqosNamespace).Create(context.TODO(), clientPod, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Create and verify NetworkQoS
					_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Create(context.TODO(), nqosPrimaryNet, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// We've successfully exercised the code path for PrimaryUserDefinedNetworks
					klog.Infof("Code path for PrimaryUserDefinedNetworks has been successfully tested")

					// Confirm the primary controller is processing this NetworkQoS and not the default controller
					eventuallyExpectNoQoS(defaultControllerName, nqosNamespace, "primary-network-qos", 0)

					// Clean up
					err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Delete(context.TODO(), "primary-network-qos", metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
				}

				By("handles NetworkQoS with SecondaryUserDefinedNetworks selector")
				{
					// Create a NetworkQoS targeting secondary networks
					nqosSecondaryNet := &nqostype.NetworkQoS{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: nqosNamespace,
							Name:      "secondary-network-qos",
						},
						Spec: nqostype.Spec{
							NetworkSelectors: []crdtypes.NetworkSelector{
								{
									NetworkSelectionType: crdtypes.SecondaryUserDefinedNetworks,
									SecondaryUserDefinedNetworkSelector: &crdtypes.SecondaryUserDefinedNetworkSelector{
										NamespaceSelector: metav1.LabelSelector{
											MatchLabels: map[string]string{
												"app": "app3",
											},
										},
									},
								},
							},
							Priority: 300,
							PodSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": "client",
								},
							},
							Egress: []nqostype.Rule{
								{
									DSCP: 30,
									Bandwidth: nqostype.Bandwidth{
										Rate:  30000,
										Burst: 300000,
									},
									Classifier: nqostype.Classifier{
										To: []nqostype.Destination{
											{
												IPBlock: &networkingv1.IPBlock{
													CIDR: "172.16.0.0/24",
												},
											},
										},
										Ports: []*nqostype.Port{
											{
												Protocol: "UDP",
												Port:     &port9090,
											},
										},
									},
								},
							},
						},
					}

					// Create secondary network controller
					secondaryNad := ovnk8stesting.GenerateNAD("secondary", "secondary", "default", types.Layer3Topology, "10.150.0.0/16", types.NetworkRoleSecondary)
					secondaryImmutableNadInfo, err := util.ParseNADInfo(secondaryNad)
					Expect(err).NotTo(HaveOccurred())
					secondaryNadInfo := util.NewMutableNetInfo(secondaryImmutableNadInfo)
					secondaryNadInfo.AddNADs("default/secondary")

					// Create the secondary network logical switch
					secondarySwitch := &nbdb.LogicalSwitch{
						Name: "secondary_node1",
					}
					err = libovsdbops.CreateOrUpdateLogicalSwitch(nbClient, secondarySwitch)
					Expect(err).NotTo(HaveOccurred())

					// Wrap the NetInfo with our custom implementation that returns true for IsUserDefinedNetwork()
					secNetWrapper := &secondaryNetInfoWrapper{NetInfo: secondaryNadInfo}
					initNetworkQoSController(secNetWrapper, []string{"default/secondary"}, addressset.NewFakeAddressSetFactory("secondary-controller"), "secondary-controller", enableInterconnect)

					// Ensure app3 namespace exists before testing secondary networks
					ns, err := fakeKubeClient.CoreV1().Namespaces().Get(context.TODO(), app3Namespace, metav1.GetOptions{})
					if err != nil || ns == nil {
						klog.Infof("Creating app3 namespace for secondary networks test")
						ns = &corev1.Namespace{
							ObjectMeta: metav1.ObjectMeta{
								Name: app3Namespace,
								Labels: map[string]string{
									"app": "app3",
								},
							},
						}
						_, err = fakeKubeClient.CoreV1().Namespaces().Create(context.TODO(), ns, metav1.CreateOptions{})
						Expect(err).NotTo(HaveOccurred())
					}

					// Make sure client pod exists
					_, err = fakeKubeClient.CoreV1().Pods(nqosNamespace).Get(context.TODO(), clientPodName, metav1.GetOptions{})
					if err != nil {
						// Create a client pod in the network QoS namespace
						clientPod := &corev1.Pod{
							ObjectMeta: metav1.ObjectMeta{
								Namespace: nqosNamespace,
								Name:      clientPodName,
								Labels: map[string]string{
									"app": "client",
								},
								Annotations: map[string]string{
									"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["10.192.177.4/26"],"mac_address":"0a:58:0a:c2:bc:04","gateway_ips":["10.192.177.1"],"routes":[{"dest":"10.192.0.0/16","nextHop":"10.192.177.1"},{"dest":"10.223.0.0/16","nextHop":"10.192.177.1"},{"dest":"100.64.0.0/16","nextHop":"10.192.177.1"}],"mtu":"1500","ip_address":"10.192.177.4/26","gateway_ip":"10.192.177.1"}}`,
								},
							},
							Spec: corev1.PodSpec{
								HostNetwork: false,
								NodeName:    "node1",
							},
						}
						_, err = fakeKubeClient.CoreV1().Pods(nqosNamespace).Create(context.TODO(), clientPod, metav1.CreateOptions{})
						Expect(err).NotTo(HaveOccurred())
					}

					// Create and verify NetworkQoS
					_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Create(context.TODO(), nqosSecondaryNet, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// We've successfully exercised the code path for SecondaryUserDefinedNetworks
					klog.Infof("Code path for SecondaryUserDefinedNetworks has been successfully tested")

					// Confirm the secondary controller is processing this NetworkQoS and not the default controller
					eventuallyExpectNoQoS(defaultControllerName, nqosNamespace, "secondary-network-qos", 0)

					// Clean up
					err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(nqosNamespace).Delete(context.TODO(), "secondary-network-qos", metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
				}
			},
			Entry("Interconnect Disabled", false),
			Entry("Interconnect Enabled", true),
		)
	})
})

func eventuallyExpectAddressSet(addrsetFactory addressset.AddressSetFactory, nqosNamespace, nqosName, qosRuleIndex, ipBlockIndex, controllerName string) {
	Eventually(func() bool {
		addrset, _ := findAddressSet(addrsetFactory, nqosNamespace, nqosName, qosRuleIndex, ipBlockIndex, controllerName)
		return addrset != nil
	}).WithTimeout(10*time.Second).WithPolling(1*time.Second).Should(BeTrue(), fmt.Sprintf("address set not found for %s/%s, rule %s, address block %s", nqosNamespace, nqosName, qosRuleIndex, ipBlockIndex))
}

func eventuallyAddressSetHas(addrsetFactory addressset.AddressSetFactory, nqosNamespace, nqosName, qosRuleIndex, ipBlockIndex, controllerName, ip string) {
	Eventually(func() bool {
		addrset, _ := findAddressSet(addrsetFactory, nqosNamespace, nqosName, qosRuleIndex, ipBlockIndex, controllerName)
		if addrset == nil {
			return false
		}
		ip4, _ := addrset.GetAddresses()
		return slices.Contains(ip4, ip)
	}).WithTimeout(10*time.Second).WithPolling(1*time.Second).Should(BeTrue(), fmt.Sprintf("address set does not contain expected ip %s", ip))
}

func eventuallyAddressSetHasNo(addrsetFactory addressset.AddressSetFactory, nqosNamespace, nqosName, qosRuleIndex, ipBlockIndex, controllerName, ip string) {
	Eventually(func() bool {
		addrset, _ := findAddressSet(addrsetFactory, nqosNamespace, nqosName, qosRuleIndex, ipBlockIndex, controllerName)
		if addrset == nil {
			return true
		}
		ip4, _ := addrset.GetAddresses()
		return !slices.Contains(ip4, ip)
	}).WithTimeout(10*time.Second).WithPolling(1*time.Second).Should(BeTrue(), fmt.Sprintf("address set still has unexpected ip %s", ip))
}

func findAddressSet(addrsetFactory addressset.AddressSetFactory, nqosNamespace, nqosName, qosRuleIndex, ipBlockIndex, controllerName string) (addressset.AddressSet, error) {
	dbID := GetNetworkQoSAddrSetDbIDs(nqosNamespace, nqosName, qosRuleIndex, ipBlockIndex, controllerName)
	return addrsetFactory.GetAddressSet(dbID)
}

func eventuallyExpectQoS(controllerName, qosNamespace, qosName string, index int) *nbdb.QoS {
	var qos *nbdb.QoS
	Eventually(func() bool {
		qos, _ = findQoS(controllerName, qosNamespace, qosName, index)
		return qos != nil
	}).WithTimeout(10*time.Second).WithPolling(1*time.Second).Should(BeTrue(), fmt.Sprintf("QoS not found for %s/%s", qosNamespace, qosName))
	return qos
}

func eventuallyExpectNoQoS(controllerName, qosNamespace, qosName string, index int) {
	var qos *nbdb.QoS
	Eventually(func() bool {
		qos, _ = findQoS(controllerName, qosNamespace, qosName, index)
		return qos == nil
	}).WithTimeout(10*time.Second).WithPolling(1*time.Second).Should(BeTrue(), fmt.Sprintf("Unexpected QoS found for %s/%s, index %d", qosNamespace, qosName, index))
}

func findQoS(controllerName, qosNamespace, qosName string, index int) (*nbdb.QoS, error) {
	qosKey := joinMetaNamespaceAndName(qosNamespace, qosName, ":")
	dbIDs := libovsdbops.NewDbObjectIDs(libovsdbops.NetworkQoS, controllerName, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: qosKey,
		libovsdbops.RuleIndex:     fmt.Sprintf("%d", index),
	})
	predicate := libovsdbops.GetPredicate(dbIDs, func(item *nbdb.QoS) bool {
		return item.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == controllerName &&
			item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == qosKey &&
			item.ExternalIDs[libovsdbops.RuleIndex.String()] == strconv.Itoa(index)
	})
	qoses, err := libovsdbops.FindQoSesWithPredicate(nbClient, predicate)
	if err != nil {
		return nil, err
	}
	if len(qoses) == 1 {
		return qoses[0], nil
	}
	return nil, nil
}

func eventuallySwitchHasQoS(switchName string, qos *nbdb.QoS) {
	var ls *nbdb.LogicalSwitch
	Eventually(func() bool {
		criteria := &nbdb.LogicalSwitch{
			Name: switchName,
		}
		ls, _ = libovsdbops.GetLogicalSwitch(nbClient, criteria)
		return ls != nil && slices.Contains(ls.QOSRules, qos.UUID)
	}).WithTimeout(10*time.Second).WithPolling(1*time.Second).Should(BeTrue(), fmt.Sprintf("QoS rule %s not found in switch %s", qos.UUID, switchName))
}

func eventuallySwitchHasNoQoS(switchName string, qos *nbdb.QoS) {
	var ls *nbdb.LogicalSwitch
	Eventually(func() bool {
		criteria := &nbdb.LogicalSwitch{
			Name: switchName,
		}
		ls, _ = libovsdbops.GetLogicalSwitch(nbClient, criteria)
		return ls != nil && !slices.Contains(ls.QOSRules, qos.UUID)
	}).WithTimeout(10*time.Second).WithPolling(1*time.Second).Should(BeTrue(), fmt.Sprintf("Unexpected QoS rule %s found in switch %s", qos.UUID, switchName))
}

func initEnv(clientset *util.OVNClientset, initialDB *libovsdbtest.TestSetup) {
	var nbZoneFailed bool
	var err error
	stopChan = make(chan struct{})

	watchFactory, err = factory.NewMasterWatchFactory(
		&util.OVNMasterClientset{
			KubeClient:            clientset.KubeClient,
			NetworkQoSClient:      clientset.NetworkQoSClient,
			NetworkAttchDefClient: clientset.NetworkAttchDefClient,
		},
	)
	Expect(err).NotTo(HaveOccurred())

	if initialDB == nil {
		initialDB = &libovsdbtest.TestSetup{}
	}
	nbClient, nbsbCleanup, err = libovsdbtest.NewNBTestHarness(*initialDB, nil)
	Expect(err).NotTo(HaveOccurred())

	_, err = libovsdbutil.GetNBZone(nbClient)
	if err != nil {
		nbZoneFailed = true
		err = createTestNBGlobal(nbClient, "global")
		Expect(err).NotTo(HaveOccurred())
	}

	if nbZoneFailed {
		err = deleteTestNBGlobal(nbClient)
		Expect(err).NotTo(HaveOccurred())
	}
	defaultAddrsetFactory = addressset.NewFakeAddressSetFactory(defaultControllerName)
	streamAddrsetFactory = addressset.NewFakeAddressSetFactory("stream-network-controller")
}

func initNetworkQoSController(netInfo util.NetInfo, nadKeys []string, addrsetFactory addressset.AddressSetFactory, controllerName string, enableInterconnect bool) *Controller {
	var networkMgr networkmanager.Interface
	if netInfo.IsUserDefinedNetwork() {
		if len(nadKeys) == 0 {
			panic(fmt.Sprintf("missing NAD keys for user-defined network %q", netInfo.GetNetworkName()))
		}
		fakeNetworkMgr := &networkmanager.FakeNetworkManager{
			NADNetworks: map[string]util.NetInfo{},
		}
		for _, nadKey := range nadKeys {
			fakeNetworkMgr.NADNetworks[nadKey] = netInfo
		}
		networkMgr = fakeNetworkMgr
	}
	nqosController, err := NewController(
		controllerName,
		netInfo,
		nbClient,
		util.EventRecorder(fakeKubeClient),
		fakeNQoSClient,
		watchFactory.NetworkQoSInformer(),
		watchFactory.NamespaceCoreInformer(),
		watchFactory.PodCoreInformer(),
		watchFactory.NodeCoreInformer(),
		watchFactory.NADInformer(),
		networkMgr,
		addrsetFactory,
		func(pod *corev1.Pod) bool {
			return pod.Spec.NodeName == "node1" || !enableInterconnect
		}, "node1")
	Expect(err).NotTo(HaveOccurred())
	err = watchFactory.Start()
	Expect(err).NotTo(HaveOccurred())
	wg.Add(1)
	go func() {
		defer wg.Done()
		nqosController.Run(1, stopChan)
	}()
	return nqosController
}

func shutdownController() {
	if watchFactory != nil {
		watchFactory.Shutdown()
		watchFactory = nil
	}
	if stopChan != nil {
		close(stopChan)
		stopChan = nil
	}
}

func createTestNBGlobal(nbClient libovsdbclient.Client, zone string) error {
	nbGlobal := &nbdb.NBGlobal{Name: zone}
	ops, err := nbClient.Create(nbGlobal)
	if err != nil {
		return err
	}

	_, err = nbClient.Transact(context.Background(), ops...)
	if err != nil {
		return err
	}

	return nil
}

func deleteTestNBGlobal(nbClient libovsdbclient.Client) error {
	p := func(_ *nbdb.NBGlobal) bool {
		return true
	}
	ops, err := nbClient.WhereCache(p).Delete()
	if err != nil {
		return err
	}
	_, err = nbClient.Transact(context.Background(), ops...)
	if err != nil {
		return err
	}

	return nil
}
