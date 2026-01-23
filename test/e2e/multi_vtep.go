package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/api"
)

const (
	networkEncapIPMappingAnnotation = "test.k8s.ovn.org/pod-network-encap-ip-mapping"
)

type MultiVtepNode struct {
	Node     *v1.Node
	EncapIPs []string
}

var _ = Describe("Multi-VTEP", feature.MultiVTEP, func() {
	const (
		clientPod1Name = "client-pod-1"
		clientPod2Name = "client-pod-2"
		serverPod1Name = "server-pod-1"
		serverPod2Name = "server-pod-2"
		port           = 9000

		defaultNetworkName = "ovn-kubernetes"
		primaryNetworkCIDR = "172.30.0.0/16"
		primaryNetworkName = "tenant-red"

		secondaryNetworkCIDR       = "172.31.0.0/16" // last subnet in private range 172.16.0.0/12 (rfc1918)
		secondaryNetworkName       = "tenant-blue"
		secondaryFlatL2IgnoreCIDR  = "172.31.0.0/29"
		secondaryFlatL2NetworkCIDR = "172.31.0.0/24"
		netPrefixLengthPerNode     = 24
		secondaryIPv6CIDR          = "2010:100:200::0/60"
		netPrefixLengthIPv6PerNode = 64
	)
	f := wrappedTestFramework("multi-vtep")

	podIPForNetwork := func(k8sClient clientset.Interface, podNamespace string, podName string, networkName string, role string) (string, error) {
		pod, err := k8sClient.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		netStatus, err := podNetworkStatus(pod, func(status nadapi.NetworkStatus) bool {
			switch role {
			case types.NetworkRoleDefault:
				return status.Name == defaultNetworkName
			case types.NetworkRolePrimary:
				return status.Default
			default:
				return status.Name == namespacedName(podNamespace, networkName)
			}
		})
		if err != nil {
			return "", err
		}
		if len(netStatus) == 0 {
			return "", fmt.Errorf("no network status found for pod %s", podName)
		}
		if len(netStatus[0].IPs) == 0 {
			return "", fmt.Errorf("no IP found for pod %s", podName)
		}

		return netStatus[0].IPs[0], nil
	}

	var (
		cs          clientset.Interface
		nadClient   nadclient.K8sCniCncfIoV1Interface
		providerCtx infraapi.Context

		multiVtepNodes []MultiVtepNode
		vtepInterfaces = []string{"eth0", "vtep1"}
	)

	BeforeEach(func() {
		cs = f.ClientSet

		var err error
		nadClient, err = nadclient.NewForConfig(f.ClientConfig())
		Expect(err).NotTo(HaveOccurred())

		providerCtx = infraprovider.Get().NewTestContext()
		_ = nadClient
		_ = providerCtx

		By("Getting multi-vtep nodes")
		nodeList, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
		Expect(err).NotTo(HaveOccurred())
		for _, node := range nodeList.Items {
			encapIPs, _ := util.ParseNodeEncapIPsAnnotation(&node)
			if len(encapIPs) > 1 {
				GinkgoT().Logf("Found multi-vtep node: %s, encap IPs: %v", node.Name, encapIPs)
				multiVtepNodes = append(multiVtepNodes, MultiVtepNode{Node: &node, EncapIPs: encapIPs})
			}
		}
		Expect(len(multiVtepNodes)).To(BeNumerically(">", 1), "cluster should have at least 2  multi-vtep nodes for testing")

	})

	DescribeTableSubtree("Pods with",
		func(createNetworkFn func(podConfigs []podConfiguration) (string, string)) {

			f.SkipNamespaceCreation = true

			DescribeTable("can communicate -", func(podConfigs []podConfiguration, clientPodName string, serverPodName string) {
				networkName, networkRole := createNetworkFn(podConfigs)

				By("updating client and server Pod config configurations")
				podConfigByName := map[string]podConfiguration{}
				for _, podConfig := range podConfigs {
					podConfig.namespace = f.Namespace.Name
					encapIp := multiVtepNodes[podConfig.nodeIndex].EncapIPs[podConfig.vtepIndex]
					podConfig.annotations = map[string]string{
						networkEncapIPMappingAnnotation: fmt.Sprintf(`{"%s":"%s"}`, networkName, encapIp),
					}
					podConfig.nodeSelector = map[string]string{nodeHostnameKey: multiVtepNodes[podConfig.nodeIndex].Node.Name}
					podConfigByName[podConfig.name] = podConfig
				}

				By("instantiating the pods")
				podByName := map[string]*v1.Pod{}
				for _, pc := range podConfigByName {
					pod, err := cs.CoreV1().Pods(pc.namespace).Create(
						context.Background(),
						generatePodSpec(pc),
						metav1.CreateOptions{},
					)
					Expect(err).NotTo(HaveOccurred())
					Expect(pod).NotTo(BeNil())
					podByName[pod.Name] = pod
					Eventually(func() v1.PodPhase {
						updatedPod, err := cs.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
						if err != nil {
							return v1.PodFailed
						}
						return updatedPod.Status.Phase
					}, 2*time.Minute, 3*time.Second).Should(Equal(v1.PodRunning))
				}

				By("getting the ovnkube-node pods")
				ovnKubeNodePodByNodeName := map[string]string{}
				ovnKubernetesNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
				ovnkubeNodes, err := cs.CoreV1().Pods(ovnKubernetesNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: "app=ovnkube-node"})
				Expect(err).NotTo(HaveOccurred())
				for _, ovnkubeNode := range ovnkubeNodes.Items {
					GinkgoT().Logf("ovnkube-node: %s on node %s", ovnkubeNode.Name, ovnkubeNode.Spec.NodeName)
					ovnKubeNodePodByNodeName[ovnkubeNode.Spec.NodeName] = ovnkubeNode.Name
				}

				clientPodConfig := podConfigByName[clientPodName]
				serverPodConfig := podConfigByName[serverPodName]
				serverPod := podByName[serverPodName]
				serverNodeName := multiVtepNodes[serverPodConfig.nodeIndex].Node.Name
				expectedVtepInterface := vtepInterfaces[serverPodConfig.vtepIndex]

				By("getting the server IP")
				serverIP, err := podIPForNetwork(cs, f.Namespace.Name, serverPod.GetName(), networkName, networkRole)
				Expect(err).NotTo(HaveOccurred())
				GinkgoT().Logf("networkName: %s, serverIP: %s", networkName, serverIP)

				By("starting tcpdump on all VTEP interfaces on the server's ovnkube-node pod")
				ovnkubeNodePod := ovnKubeNodePodByNodeName[serverNodeName]
				Expect(ovnkubeNodePod).NotTo(BeEmpty(), "ovnkube-node pod not found for node %s", serverNodeName)

				ovnKubeContainerName := "ovnkube-node"
				if isInterconnectEnabled() {
					ovnKubeContainerName = "ovnkube-controller"
				}
				ovnkubeNodeExecutor := ForPod(ovnKubernetesNamespace, ovnkubeNodePod, ovnKubeContainerName)

				By("starting tcpdump on each VTEP interface in background")
				tcpdumpOutputFiles := map[string]string{}
				for _, iface := range vtepInterfaces {
					outputFile := fmt.Sprintf("/tmp/%s-%s.txt", f.Namespace.Name, iface)
					tcpdumpOutputFiles[iface] = outputFile
					tcpdumpCmd := fmt.Sprintf("nohup timeout 300 tcpdump -l -i %s -nne 'geneve and host %s' > %s 2>/dev/null &", iface, serverIP, outputFile)
					_, err := ovnkubeNodeExecutor.Exec("sh", "-c", tcpdumpCmd)
					if err != nil {
						GinkgoT().Logf("Warning: failed to start tcpdump on %s: %v", iface, err)
					}
				}
				DeferCleanup(func() {
					if framework.TestContext.DeleteNamespace {
						args := []string{"-f"}
						for _, outputFile := range tcpdumpOutputFiles {
							args = append(args, outputFile)
						}
						_, _ = ovnkubeNodeExecutor.Exec("rm", args...)
					}
				})

				By("Give tcpdump 5 seconds to by ready for capturing packets")
				time.Sleep(5 * time.Second)

				By("asserting the client pod can contact the server pod")
				Eventually(func() error {
					return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port)
				}, 2*time.Minute, 6*time.Second).Should(Succeed())

				By("stopping tcpdump")
				// Wait for packets to be captured and flushed to file
				time.Sleep(3 * time.Second)
				_, _ = ovnkubeNodeExecutor.Exec("sh", "-c", "pkill -15 -f tcpdump || true")
				time.Sleep(1 * time.Second)

				By("verifying packets captured on expected VTEP interface")
				for iface, outputFile := range tcpdumpOutputFiles {
					countPacketsCmd := fmt.Sprintf("grep Geneve %s | wc -l || echo 0", outputFile)
					output, err := ovnkubeNodeExecutor.Exec("sh", "-c", countPacketsCmd)
					Expect(err).NotTo(HaveOccurred(), "failed to read the output for %s: %v", iface, err)

					count, err := strconv.Atoi(strings.TrimSpace(output))
					Expect(err).NotTo(HaveOccurred())

					if iface == expectedVtepInterface {
						// In kind env, all the packets can be captured, so expect at least
						// 6 packets captured(haneshake and teardown packets)
						Expect(count).To(BeNumerically(">", 6), "expected packets on VTEP interface %s but got none", iface)
					} else {
						// sometime the first SYN packet could be flooded to another VTEP interface
						Expect(count).To(BeNumerically("<=", 1), "unexpected packets on VTEP interface %s", iface)
					}
				}
			},
				// basic cases, ensure the vteps are working
				Entry("one local Pod and one remote Pod - 1st VTEP to 1st VTEP",
					[]podConfiguration{
						{
							name:      clientPod1Name,
							vtepIndex: 0,
							nodeIndex: 0,
						},
						{
							name:         serverPod1Name,
							containerCmd: httpServerContainerCmd(port),
							vtepIndex:    0,
							nodeIndex:    1,
						},
					},
					clientPod1Name,
					serverPod1Name,
				),
				Entry("one local Pod and one remote Pod - 2nd VTEP to 2nd VTEP",
					[]podConfiguration{
						{
							name:      clientPod1Name,
							vtepIndex: 1,
							nodeIndex: 0,
						},
						{
							name:         serverPod1Name,
							containerCmd: httpServerContainerCmd(port),
							vtepIndex:    1,
							nodeIndex:    1,
						},
					},
					clientPod1Name,
					serverPod1Name,
				),
				// two remote Pods on same VTEP, use node subnet for routing
				Entry("one local Pod and two remote Pods on 1st VTEP - 1st VTEP to 1st VTEP",
					[]podConfiguration{
						{
							name:      clientPod1Name,
							vtepIndex: 0,
							nodeIndex: 0,
						},
						{
							name:         serverPod1Name,
							containerCmd: httpServerContainerCmd(port),
							vtepIndex:    0,
							nodeIndex:    1,
						},
						{
							name:         serverPod2Name,
							containerCmd: httpServerContainerCmd(port),
							vtepIndex:    0,
							nodeIndex:    1,
						},
					},
					clientPod1Name,
					serverPod1Name,
				),
				// two remote Pods on different VTEPs, use node subnet for routing traffic to 1st remote Pod
				Entry("one local Pod and two remote Pods on different VTEPs - 1st VTEP to 1st VTEP",
					[]podConfiguration{
						{
							name:      clientPod1Name,
							vtepIndex: 0,
							nodeIndex: 0,
						},
						{
							name:         serverPod1Name,
							containerCmd: httpServerContainerCmd(port),
							vtepIndex:    0,
							nodeIndex:    1,
						},
						{
							name:         serverPod2Name,
							containerCmd: httpServerContainerCmd(port),
							vtepIndex:    1,
							nodeIndex:    1,
						},
					},
					clientPod1Name,
					serverPod1Name,
				),

				// two remote Pods on different VTEPs, use pod-ip for routing traffic to 2nd remote Pod
				Entry("one local Pod and two remote Pods on different VTEPs - 1st VTEP to 2nd VTEP",
					[]podConfiguration{
						{
							name:      clientPod1Name,
							vtepIndex: 0,
							nodeIndex: 0,
						},
						{
							name:         serverPod1Name,
							containerCmd: httpServerContainerCmd(port),
							vtepIndex:    0,
							nodeIndex:    1,
						},
						{
							name:         serverPod2Name,
							containerCmd: httpServerContainerCmd(port),
							vtepIndex:    1,
							nodeIndex:    1,
						},
					},
					clientPod1Name,
					serverPod2Name,
				),

				// two local Pods on different VTEPs, 1st local Pod can communicate with the remote Pod via 1st VTEP
				Entry("two local Pods on different VTEPs and one remote Pod on 1st VTEP - 1st VTEP to 1st VTEP",
					[]podConfiguration{
						{
							name:      clientPod1Name,
							vtepIndex: 0,
							nodeIndex: 0,
						},
						{
							name:      clientPod2Name,
							vtepIndex: 1,
							nodeIndex: 0,
						},
						{
							name:         serverPod1Name,
							containerCmd: httpServerContainerCmd(port),
							vtepIndex:    0,
							nodeIndex:    1,
						},
					},
					clientPod1Name,
					serverPod1Name,
				),
				// two local Pods on different VTEPs, 2nd local Pod can communicate with the 2nd remote Pod via 2nd VTEP
				Entry("two local Pods on different VTEPs and two remote Pods on different VTEPs - 2nd VTEP to 2nd VTEP",
					[]podConfiguration{
						{
							name:      clientPod1Name,
							vtepIndex: 0,
							nodeIndex: 0,
						},
						{
							name:      clientPod2Name,
							vtepIndex: 1,
							nodeIndex: 0,
						},
						{
							name:         serverPod1Name,
							containerCmd: httpServerContainerCmd(port),
							vtepIndex:    0,
							nodeIndex:    1,
						},
						{
							name:         serverPod2Name,
							containerCmd: httpServerContainerCmd(port),
							vtepIndex:    1,
							nodeIndex:    1,
						},
					},
					clientPod2Name,
					serverPod2Name,
				),
			)
		},
		Entry("default network", func(podConfigs []podConfiguration) (string, string) {
			namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework": f.BaseName,
			})
			Expect(err).NotTo(HaveOccurred(), "Should create namespace for test")
			f.Namespace = namespace
			return types.DefaultNetworkName, types.NetworkRoleDefault
		}),
		Entry("Layer3 Primary network", func(podConfigs []podConfiguration) (string, string) {
			namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			Expect(err).NotTo(HaveOccurred(), "Should create namespace for test")
			f.Namespace = namespace
			netConfigParams := networkAttachmentConfigParams{
				name:     primaryNetworkName,
				topology: types.Layer3Topology,
				cidr:     netCIDR(primaryNetworkCIDR, netPrefixLengthPerNode),
				role:     types.NetworkRolePrimary,
			}
			netConfig := newNetworkAttachmentConfig(netConfigParams)
			netConfig.namespace = f.Namespace.Name
			By("creating Layer3 Primary network")
			_, err = nadClient.NetworkAttachmentDefinitions(netConfig.namespace).Create(
				context.Background(),
				generateNetAttachDef(netConfig.namespace, netConfig.name, generateNADSpec(netConfig)),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			return primaryNetworkName, types.NetworkRolePrimary
		}),
		Entry("Layer3 Secondary network", func(podConfigs []podConfiguration) (string, string) {
			namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework": f.BaseName,
			})
			Expect(err).NotTo(HaveOccurred(), "Should create namespace for test")
			f.Namespace = namespace

			netConfigParams := networkAttachmentConfigParams{
				name:     secondaryNetworkName,
				topology: types.Layer3Topology,
				cidr:     netCIDR(secondaryNetworkCIDR, netPrefixLengthPerNode),
			}
			netConfig := newNetworkAttachmentConfig(netConfigParams)
			netConfig.namespace = f.Namespace.Name
			By("creating Layer3 Secondary network")
			_, err = nadClient.NetworkAttachmentDefinitions(netConfig.namespace).Create(
				context.Background(),
				generateNetAttachDef(netConfig.namespace, netConfig.name, generateNADSpec(netConfig)),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			By("attatch Pod to secondary network")
			for i := range podConfigs {
				podConfigs[i].attachments = []nadapi.NetworkSelectionElement{{Name: netConfigParams.name}}
			}

			return secondaryNetworkName, types.NetworkRoleSecondary
		}),
		Entry("Layer2 Primary network", func(podConfigs []podConfiguration) (string, string) {
			namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			Expect(err).NotTo(HaveOccurred(), "Should create namespace for test")
			f.Namespace = namespace
			netConfigParams := networkAttachmentConfigParams{
				name:     primaryNetworkName,
				topology: types.Layer2Topology,
				cidr:     secondaryNetworkCIDR,
				role:     types.NetworkRolePrimary,
			}
			netConfig := newNetworkAttachmentConfig(netConfigParams)
			netConfig.namespace = f.Namespace.Name
			By("creating the Layer2primary network")
			_, err = nadClient.NetworkAttachmentDefinitions(netConfig.namespace).Create(
				context.Background(),
				generateNetAttachDef(netConfig.namespace, netConfig.name, generateNADSpec(netConfig)),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			return primaryNetworkName, types.NetworkRolePrimary
		}),
		Entry("Layer2 Secondary network", func(podConfigs []podConfiguration) (string, string) {
			namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework": f.BaseName,
			})
			Expect(err).NotTo(HaveOccurred(), "Should create namespace for test")
			f.Namespace = namespace

			netConfigParams := networkAttachmentConfigParams{
				name:     secondaryNetworkName,
				topology: types.Layer2Topology,
				cidr:     secondaryNetworkCIDR,
			}
			netConfig := newNetworkAttachmentConfig(netConfigParams)
			netConfig.namespace = f.Namespace.Name
			By("creating layer2 secondary network")
			_, err = nadClient.NetworkAttachmentDefinitions(netConfig.namespace).Create(
				context.Background(),
				generateNetAttachDef(netConfig.namespace, netConfig.name, generateNADSpec(netConfig)),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			By("attach Pod to secondary network")
			for i := range podConfigs {
				podConfigs[i].attachments = []nadapi.NetworkSelectionElement{{Name: netConfigParams.name}}
			}

			return secondaryNetworkName, types.NetworkRoleSecondary
		}),
	)

})
