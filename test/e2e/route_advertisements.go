package e2e

import (
	"context"
	"fmt"
	"net"
	"strings"

	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	rav1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	applycfgrav1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/applyconfiguration/routeadvertisements/v1"
	raclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	udnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	v1 "k8s.io/client-go/applyconfigurations/meta/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilnet "k8s.io/utils/net"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

var _ = ginkgo.Describe("BGP: Pod to external server when default podNetwork is advertised", func() {
	const (
		serverContainerName    = "bgpserver"
		routerContainerName    = "frr"
		echoClientPodName      = "echo-client-pod"
		primaryNetworkName     = "kind"
		bgpExternalNetworkName = "bgpnet"
	)
	var serverContainerIPs []string
	var frrContainerIPv4, frrContainerIPv6 string
	var nodes *corev1.NodeList
	f := wrappedTestFramework("pod2external-route-advertisements")

	ginkgo.BeforeEach(func() {
		serverContainerIPs = []string{}

		bgpServerIPv4, bgpServerIPv6 := getContainerAddressesForNetwork(serverContainerName, bgpExternalNetworkName)
		if isIPv4Supported() {
			serverContainerIPs = append(serverContainerIPs, bgpServerIPv4)
		}

		if isIPv6Supported() {
			serverContainerIPs = append(serverContainerIPs, bgpServerIPv6)
		}
		framework.Logf("The external server IPs are: %+v", serverContainerIPs)

		frrContainerIPv4, frrContainerIPv6 = getContainerAddressesForNetwork(routerContainerName, primaryNetworkName)
		framework.Logf("The frr router container IPs are: %s/%s", frrContainerIPv4, frrContainerIPv6)
	})

	ginkgo.When("a client ovnk pod targeting an external server is created", func() {

		var clientPod *corev1.Pod
		var clientPodNodeName string
		var err error

		ginkgo.BeforeEach(func() {
			if !isDefaultNetworkAdvertised() {
				e2eskipper.Skipf(
					"skipping pod to external server tests when podNetwork is not advertised",
				)
			}
			ginkgo.By("Selecting 3 schedulable nodes")
			nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 2))

			ginkgo.By("Selecting node for client pod")
			clientPodNodeName = nodes.Items[1].Name

			ginkgo.By("Creating client pod")
			clientPod = e2epod.NewAgnhostPod(f.Namespace.Name, echoClientPodName, nil, nil, nil)
			clientPod.Spec.NodeName = clientPodNodeName
			for k := range clientPod.Spec.Containers {
				if clientPod.Spec.Containers[k].Name == "agnhost-container" {
					clientPod.Spec.Containers[k].Command = []string{
						"sleep",
						"infinity",
					}
				}
			}
			e2epod.NewPodClient(f).CreateSync(context.TODO(), clientPod)

			gomega.Expect(len(serverContainerIPs)).To(gomega.BeNumerically(">", 0))
		})
		// -----------------               ------------------                         ---------------------
		// |               | 172.26.0.0/16 |                |       172.18.0.0/16     | ovn-control-plane |
		// |   external    |<------------- |   FRR router   |<------ KIND cluster --  ---------------------
		// |    server     |               |                |                         |    ovn-worker     |   (client pod advertised
		// -----------------               ------------------                         ---------------------    using RouteAdvertisements
		//                                                                            |    ovn-worker2    |    from default pod network)
		//                                                                            ---------------------
		// The client pod inside the KIND cluster on the default network exposed using default network Router
		// Advertisement will curl the external server container sitting outside the cluster via a FRR router
		// This test ensures the north-south connectivity is happening through podIP
		ginkgo.It("tests are run towards the external agnhost echo server", func() {
			ginkgo.By("routes from external bgp server are imported by nodes in the cluster")
			externalServerV4CIDR, externalServerV6CIDR := getContainerNetworkCIDRs(bgpExternalNetworkName)
			framework.Logf("the network cidrs to be imported are v4=%s and v6=%s", externalServerV4CIDR, externalServerV6CIDR)
			for _, node := range nodes.Items {
				ipVer := ""
				cmd := []string{containerRuntime, "exec", node.Name}
				bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, externalServerV4CIDR), " ")
				cmd = append(cmd, bgpRouteCommand...)
				framework.Logf("Checking for server's route in node %s", node.Name)
				gomega.Eventually(func() bool {
					routes, err := runCommand(cmd...)
					framework.ExpectNoError(err, "failed to get BGP routes from node")
					framework.Logf("Routes in node %s", routes)
					return strings.Contains(routes, frrContainerIPv4)
				}, 30*time.Second).Should(gomega.BeTrue())
				if isDualStackCluster(nodes) {
					ipVer = " -6"
					nodeIPv6LLA, err := GetNodeIPv6LinkLocalAddressForEth0(routerContainerName)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					cmd := []string{containerRuntime, "exec", node.Name}
					bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, externalServerV6CIDR), " ")
					cmd = append(cmd, bgpRouteCommand...)
					framework.Logf("Checking for server's route in node %s", node.Name)
					gomega.Eventually(func() bool {
						routes, err := runCommand(cmd...)
						framework.ExpectNoError(err, "failed to get BGP routes from node")
						framework.Logf("Routes in node %s", routes)
						return strings.Contains(routes, nodeIPv6LLA)
					}, 30*time.Second).Should(gomega.BeTrue())
				}
			}

			ginkgo.By("routes to the default pod network are advertised to external frr router")
			// Get the first element in the advertisements array (assuming you want to check the first one)
			gomega.Eventually(func() string {
				podNetworkValue, err := e2ekubectl.RunKubectl("", "get", "ra", "default", "--template={{index .spec.advertisements 0}}")
				if err != nil {
					return ""
				}
				return podNetworkValue
			}, 5*time.Second, time.Second).Should(gomega.Equal("PodNetwork"))

			gomega.Eventually(func() string {
				reason, err := e2ekubectl.RunKubectl("", "get", "ra", "default", "-o", "jsonpath={.status.conditions[?(@.type=='Accepted')].reason}")
				if err != nil {
					return ""
				}
				return reason
			}, 30*time.Second, time.Second).Should(gomega.Equal("Accepted"))

			ginkgo.By("all 3 node's podSubnet routes are exported correctly to external FRR router by frr-k8s speakers")
			// sample
			//10.244.0.0/24 nhid 27 via 172.18.0.3 dev eth0 proto bgp metric 20
			//10.244.1.0/24 nhid 30 via 172.18.0.2 dev eth0 proto bgp metric 20
			//10.244.2.0/24 nhid 25 via 172.18.0.4 dev eth0 proto bgp metric 20
			for _, serverContainerIP := range serverContainerIPs {
				for _, node := range nodes.Items {
					podv4CIDR, podv6CIDR, err := getNodePodCIDRs(node.Name)
					if err != nil {
						framework.Failf("Error retrieving the pod cidr from %s %v", node.Name, err)
					}
					framework.Logf("the pod cidr for node %s-%s is %s", node.Name, podv4CIDR, podv6CIDR)
					ipVer := ""
					podCIDR := podv4CIDR
					nodeIP := e2enode.GetAddressesByTypeAndFamily(&node, corev1.NodeInternalIP, corev1.IPv4Protocol)
					if utilnet.IsIPv6String(serverContainerIP) {
						ipVer = " -6"
						podCIDR = podv6CIDR
						// BGP by default uses LLA as nexthops in its routes
						nodeIPv6LLA, err := GetNodeIPv6LinkLocalAddressForEth0(node.Name)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						nodeIP = []string{nodeIPv6LLA}
					}
					gomega.Expect(len(nodeIP)).To(gomega.BeNumerically(">", 0))
					framework.Logf("the nodeIP for node %s is %+v", node.Name, nodeIP)
					cmd := []string{containerRuntime, "exec", routerContainerName}
					bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, podCIDR), " ")
					cmd = append(cmd, bgpRouteCommand...)
					framework.Logf("Checking for node %s's route for pod subnet %s", node.Name, podCIDR)
					gomega.Eventually(func() bool {
						routes, err := runCommand(cmd...)
						framework.ExpectNoError(err, "failed to get BGP routes from intermediary router")
						framework.Logf("Routes in FRR %s", routes)
						return strings.Contains(routes, nodeIP[0])
					}, 30*time.Second).Should(gomega.BeTrue())
				}
			}

			ginkgo.By("queries to the external server are not SNATed (uses podIP)")
			podv4IP, podv6IP, err := podIPsForDefaultNetwork(f.ClientSet, f.Namespace.Name, clientPod.Name)
			framework.ExpectNoError(err, fmt.Sprintf("Getting podIPs for pod %s failed: %v", clientPod.Name, err))
			framework.Logf("Client pod IP address v4=%s, v6=%s", podv4IP, podv6IP)
			for _, serverContainerIP := range serverContainerIPs {
				ginkgo.By(fmt.Sprintf("Sending request to node IP %s "+
					"and expecting to receive the same payload", serverContainerIP))
				cmd := fmt.Sprintf("curl --max-time 10 -g -q -s http://%s/clientip",
					net.JoinHostPort(serverContainerIP, "8080"),
				)
				framework.Logf("Testing pod to external traffic with command %q", cmd)
				stdout, err := e2epodoutput.RunHostCmdWithRetries(
					clientPod.Namespace,
					clientPod.Name,
					cmd,
					framework.Poll,
					60*time.Second)
				framework.ExpectNoError(err, fmt.Sprintf("Testing pod to external traffic failed: %v", err))
				expectedPodIP := podv4IP
				if isIPv6Supported() && utilnet.IsIPv6String(serverContainerIP) {
					expectedPodIP = podv6IP
					// For IPv6 addresses, need to handle the brackets in the output
					outputIP := strings.TrimPrefix(strings.Split(stdout, "]:")[0], "[")
					gomega.Expect(outputIP).To(gomega.Equal(expectedPodIP),
						fmt.Sprintf("Testing pod %s to external traffic failed while analysing output %v", echoClientPodName, stdout))
				} else {
					// Original IPv4 handling
					gomega.Expect(strings.Split(stdout, ":")[0]).To(gomega.Equal(expectedPodIP),
						fmt.Sprintf("Testing pod %s to external traffic failed while analysing output %v", echoClientPodName, stdout))
				}
			}
		})
	})
})

var _ = ginkgo.Describe("BGP: Pod to external server when CUDN network is advertised", func() {
	const (
		serverContainerName    = "bgpserver"
		routerContainerName    = "frr"
		echoClientPodName      = "echo-client-pod"
		primaryNetworkName     = "kind"
		bgpExternalNetworkName = "bgpnet"
		placeholder            = "PLACEHOLDER_NAMESPACE"
	)
	var serverContainerIPs []string
	var frrContainerIPv4, frrContainerIPv6 string
	var nodes *corev1.NodeList
	var clientPod *corev1.Pod

	f := wrappedTestFramework("pod2external-route-advertisements")
	f.SkipNamespaceCreation = true

	ginkgo.BeforeEach(func() {
		var err error
		namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		f.Namespace = namespace

		serverContainerIPs = []string{}

		bgpServerIPv4, bgpServerIPv6 := getContainerAddressesForNetwork(serverContainerName, bgpExternalNetworkName)
		if isIPv4Supported() {
			serverContainerIPs = append(serverContainerIPs, bgpServerIPv4)
		}

		if isIPv6Supported() {
			serverContainerIPs = append(serverContainerIPs, bgpServerIPv6)
		}
		framework.Logf("The external server IPs are: %+v", serverContainerIPs)

		frrContainerIPv4, frrContainerIPv6 = getContainerAddressesForNetwork(routerContainerName, primaryNetworkName)
		framework.Logf("The frr router container IPs are: %s/%s", frrContainerIPv4, frrContainerIPv6)

		// Select nodes here so they're available for all tests
		ginkgo.By("Selecting 3 schedulable nodes")
		nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 2))
	})

	ginkgo.DescribeTable("Route Advertisements",
		func(cudnTemplate *udnv1.ClusterUserDefinedNetwork, raApplyCfg *applycfgrav1.RouteAdvertisementsApplyConfiguration) {
			// set the exact selector
			cudnTemplate.Spec.NamespaceSelector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{f.Namespace.Name},
			}}}

			if IsGatewayModeLocal() && cudnTemplate.Spec.Network.Topology == udnv1.NetworkTopologyLayer2 {
				e2eskipper.Skipf(
					"BGP for L2 networks on LGW is currently unsupported",
				)
			}
			// Create CUDN
			ginkgo.By("create ClusterUserDefinedNetwork")
			udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			cUDN, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.Background(), cudnTemplate, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.DeferCleanup(func() {
				udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), cUDN.Name, metav1.DeleteOptions{})
			})
			gomega.Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cUDN.Name), 5*time.Second, time.Second).Should(gomega.Succeed())

			ginkgo.DeferCleanup(func() {
				ginkgo.By(fmt.Sprintf("delete pods in %s namespace to unblock CUDN CR & associate NAD deletion", f.Namespace.Name))
				gomega.Expect(f.ClientSet.CoreV1().Pods(f.Namespace.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(gomega.Succeed())
			})

			// Create client pod
			ginkgo.By("Creating client pod")
			podSpec := e2epod.NewAgnhostPod(f.Namespace.Name, echoClientPodName, nil, nil, nil)
			podSpec.Spec.NodeName = nodes.Items[1].Name
			for k := range podSpec.Spec.Containers {
				if podSpec.Spec.Containers[k].Name == "agnhost-container" {
					podSpec.Spec.Containers[k].Command = []string{
						"sleep",
						"infinity",
					}
				}
			}
			clientPod = e2epod.NewPodClient(f).CreateSync(context.TODO(), podSpec)

			// Create route advertisement
			ginkgo.By("create router advertisement")
			raClient, err := raclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ra, err := raClient.K8sV1().RouteAdvertisements().Apply(context.TODO(), raApplyCfg, metav1.ApplyOptions{
				FieldManager: f.Namespace.Name,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.DeferCleanup(func() { raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), ra.Name, metav1.DeleteOptions{}) })
			ginkgo.By("ensure route advertisement matching CUDN was created successfully")
			gomega.Eventually(func() string {
				ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.TODO(), ra.Name, metav1.GetOptions{})
				if err != nil {
					return ""
				}
				condition := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
				if condition == nil {
					return ""
				}
				return condition.Reason
			}, 30*time.Second, time.Second).Should(gomega.Equal("Accepted"))

			gomega.Expect(len(serverContainerIPs)).To(gomega.BeNumerically(">", 0))

			// -----------------               ------------------                         ---------------------
			// |               | 172.26.0.0/16 |                |       172.18.0.0/16     | ovn-control-plane |
			// |   external    |<------------- |   FRR router   |<------ KIND cluster --  ---------------------
			// |    server     |               |                |                         |    ovn-worker     |   (client UDN pod advertised
			// -----------------               ------------------                         ---------------------    using RouteAdvertisements
			//                                                                            |    ovn-worker2    |    from default pod network)
			//                                                                            ---------------------
			// The client pod inside the KIND cluster on the default network exposed using default network Router
			// Advertisement will curl the external server container sitting outside the cluster via a FRR router
			// This test ensures the north-south connectivity is happening through podIP
			ginkgo.By("routes from external bgp server are imported by nodes in the cluster")
			externalServerV4CIDR, externalServerV6CIDR := getContainerNetworkCIDRs(bgpExternalNetworkName)
			framework.Logf("the network cidrs to be imported are v4=%s and v6=%s", externalServerV4CIDR, externalServerV6CIDR)
			for _, node := range nodes.Items {
				ipVer := ""
				cmd := []string{containerRuntime, "exec", node.Name}
				bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, externalServerV4CIDR), " ")
				cmd = append(cmd, bgpRouteCommand...)
				framework.Logf("Checking for server's route in node %s", node.Name)
				gomega.Eventually(func() bool {
					routes, err := runCommand(cmd...)
					framework.ExpectNoError(err, "failed to get BGP routes from node")
					framework.Logf("Routes in node %s", routes)
					return strings.Contains(routes, frrContainerIPv4)
				}, 30*time.Second).Should(gomega.BeTrue())
				if isDualStackCluster(nodes) {
					ipVer = " -6"
					nodeIPv6LLA, err := GetNodeIPv6LinkLocalAddressForEth0(routerContainerName)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					cmd := []string{containerRuntime, "exec", node.Name}
					bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, externalServerV6CIDR), " ")
					cmd = append(cmd, bgpRouteCommand...)
					framework.Logf("Checking for server's route in node %s", node.Name)
					gomega.Eventually(func() bool {
						routes, err := runCommand(cmd...)
						framework.ExpectNoError(err, "failed to get BGP routes from node")
						framework.Logf("Routes in node %s", routes)
						return strings.Contains(routes, nodeIPv6LLA)
					}, 30*time.Second).Should(gomega.BeTrue())
				}
			}

			ginkgo.By("queries to the external server are not SNATed (uses podIP)")
			for _, serverContainerIP := range serverContainerIPs {
				podIP, err := podIPsForUserDefinedPrimaryNetwork(f.ClientSet, f.Namespace.Name, clientPod.Name, namespacedName(f.Namespace.Name, cUDN.Name), 0)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				framework.ExpectNoError(err, fmt.Sprintf("Getting podIPs for pod %s failed: %v", clientPod.Name, err))
				framework.Logf("Client pod IP address=%s", podIP)

				ginkgo.By(fmt.Sprintf("Sending request to node IP %s "+
					"and expecting to receive the same payload", serverContainerIP))
				cmd := fmt.Sprintf("curl --max-time 10 -g -q -s http://%s/clientip",
					net.JoinHostPort(serverContainerIP, "8080"),
				)
				framework.Logf("Testing pod to external traffic with command %q", cmd)
				stdout, err := e2epodoutput.RunHostCmdWithRetries(
					clientPod.Namespace,
					clientPod.Name,
					cmd,
					framework.Poll,
					60*time.Second)
				framework.ExpectNoError(err, fmt.Sprintf("Testing pod to external traffic failed: %v", err))
				if isIPv6Supported() && utilnet.IsIPv6String(serverContainerIP) {
					podIP, err = podIPsForUserDefinedPrimaryNetwork(f.ClientSet, f.Namespace.Name, clientPod.Name, namespacedName(f.Namespace.Name, cUDN.Name), 1)
					// For IPv6 addresses, need to handle the brackets in the output
					outputIP := strings.TrimPrefix(strings.Split(stdout, "]:")[0], "[")
					gomega.Expect(outputIP).To(gomega.Equal(podIP),
						fmt.Sprintf("Testing pod %s to external traffic failed while analysing output %v", echoClientPodName, stdout))
				} else {
					// Original IPv4 handling
					gomega.Expect(strings.Split(stdout, ":")[0]).To(gomega.Equal(podIP),
						fmt.Sprintf("Testing pod %s to external traffic failed while analysing output %v", echoClientPodName, stdout))
				}
			}
		},
		ginkgo.Entry("layer3",
			&udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "bgp-udn-layer3-network",
					Labels:       map[string]string{"bgp-udn-layer3-network": ""},
				},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer3,
						Layer3: &udnv1.Layer3Config{
							Role: "Primary",
							Subnets: generateL3Subnets(udnv1.Layer3Subnet{
								CIDR:       "103.103.0.0/16",
								HostSubnet: 24,
							}, udnv1.Layer3Subnet{
								CIDR:       "2014:100:200::0/60",
								HostSubnet: 64,
							}),
						},
					},
				},
			},
			applycfgrav1.RouteAdvertisements("bgp-udn-layer3-network-ra").
				WithSpec(
					applycfgrav1.RouteAdvertisementsSpec().
						WithAdvertisements(rav1.PodNetwork).
						WithNetworkSelector(
							v1.LabelSelector().WithMatchLabels(map[string]string{"bgp-udn-layer3-network": ""}),
						),
				),
		),
		ginkgo.Entry("layer2",
			&udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "bgp-udn-layer2-network",
					Labels:       map[string]string{"bgp-udn-layer2-network": ""},
				},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer2,
						Layer2: &udnv1.Layer2Config{
							Role:    "Primary",
							Subnets: generateL2Subnets("103.0.0.0/16", "2014:100::0/60"),
						},
					},
				},
			},
			applycfgrav1.RouteAdvertisements("bgp-udn-layer2-network-ra").
				WithSpec(
					applycfgrav1.RouteAdvertisementsSpec().
						WithAdvertisements(rav1.PodNetwork).
						WithNetworkSelector(
							v1.LabelSelector().WithMatchLabels(map[string]string{"bgp-udn-layer2-network": ""}),
						),
				),
		),
	)
})

var _ = ginkgo.DescribeTableSubtree("BGP: isolation between advertised CUDN networks",
	func(cudnATemplate, cudnBTemplate *udnv1.ClusterUserDefinedNetwork) {
		f := wrappedTestFramework("bpp-network-isolation")
		f.SkipNamespaceCreation = true
		var primaryUDNNamespace *corev1.Namespace
		var nodes *corev1.NodeList
		// podsNetA has 3 pods in cudnA, two are on nodes[0] and the last one is on nodes[1]
		var podsNetA []*corev1.Pod

		// podNetB is in cudnA hosted on nodes[1], podNetDefault is in the default network hosted on nodes[1]
		var podNetB, podNetDefault *corev1.Pod
		var svcNetA, svcNetB, svcNetDefault *corev1.Service

		ginkgo.BeforeEach(func() {
			ginkgo.By("Configuring primary UDN namespaces")
			namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			f.Namespace = namespace
			primaryUDNNamespace, err = f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("Configuring networks")
			cudnATemplate.Spec.NamespaceSelector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{f.Namespace.Name},
			}}}
			cudnBTemplate.Spec.NamespaceSelector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{primaryUDNNamespace.Name},
			}}}

			udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			cUDNA, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.Background(), cudnATemplate, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.DeferCleanup(func() {
				udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), cUDNA.Name, metav1.DeleteOptions{})
				gomega.Eventually(func() bool {
					_, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Get(context.TODO(), cUDNA.Name, metav1.GetOptions{})
					return apierrors.IsNotFound(err)
				}, time.Second*30).Should(gomega.BeTrue())
			})

			cUDNB, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.Background(), cudnBTemplate, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.DeferCleanup(func() {
				udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), cUDNB.Name, metav1.DeleteOptions{})
				gomega.Eventually(func() bool {
					_, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Get(context.TODO(), cUDNB.Name, metav1.GetOptions{})
					return apierrors.IsNotFound(err)
				}, time.Second*30).Should(gomega.BeTrue())
			})

			ginkgo.By("Waiting for networks to be ready")
			gomega.Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cUDNA.Name), 5*time.Second, time.Second).Should(gomega.Succeed())
			gomega.Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cUDNB.Name), 5*time.Second, time.Second).Should(gomega.Succeed())
			ginkgo.DeferCleanup(func() {
				ginkgo.By(fmt.Sprintf("delete pods in %s and %s namespaces to unblock CUDN CR & associate NAD deletion", f.Namespace.Name, primaryUDNNamespace.Name))
				gomega.Expect(f.ClientSet.CoreV1().Pods(f.Namespace.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(gomega.Succeed())
				gomega.Expect(f.ClientSet.CoreV1().Pods(primaryUDNNamespace.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(gomega.Succeed())
			})

			ginkgo.By("Selecting 3 schedulable nodes")
			nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 2))

			ginkgo.By("Setting up pods and services")
			podsNetA = []*corev1.Pod{}
			pod := e2epod.NewAgnhostPod(f.Namespace.Name, fmt.Sprintf("pod-a-%s-net-%s", nodes.Items[0].Name, cUDNA.Name), nil, nil, []corev1.ContainerPort{{ContainerPort: 8080}}, "netexec")
			pod.Spec.NodeName = nodes.Items[0].Name
			pod.Labels = map[string]string{"network": cUDNA.Name}
			podsNetA = append(podsNetA, e2epod.NewPodClient(f).CreateSync(context.TODO(), pod))

			pod.Name = fmt.Sprintf("pod-b-%s-net-%s", nodes.Items[0].Name, cUDNA.Name)
			podsNetA = append(podsNetA, e2epod.NewPodClient(f).CreateSync(context.TODO(), pod))

			pod.Name = fmt.Sprintf("pod-c-%s-net-%s", nodes.Items[1].Name, cUDNA.Name)
			pod.Spec.NodeName = nodes.Items[1].Name
			podsNetA = append(podsNetA, e2epod.NewPodClient(f).CreateSync(context.TODO(), pod))

			svc := e2eservice.CreateServiceSpec(fmt.Sprintf("service-%s", cUDNA.Name), "", false, pod.Labels)
			svc.Spec.Ports = []corev1.ServicePort{{Port: 8080}}
			familyPolicy := corev1.IPFamilyPolicyPreferDualStack
			svc.Spec.IPFamilyPolicy = &familyPolicy
			svcNetA, err = f.ClientSet.CoreV1().Services(pod.Namespace).Create(context.Background(), svc, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			pod.Name = fmt.Sprintf("pod-a-%s-net-%s", nodes.Items[1].Name, cUDNB.Name)
			pod.Namespace = primaryUDNNamespace.Name
			pod.Labels = map[string]string{"network": cUDNB.Name}
			podNetB = e2epod.PodClientNS(f, primaryUDNNamespace.Name).CreateSync(context.TODO(), pod)

			svc.Name = fmt.Sprintf("service-%s", cUDNB.Name)
			svc.Namespace = pod.Namespace
			svc.Spec.Selector = pod.Labels
			svcNetB, err = f.ClientSet.CoreV1().Services(pod.Namespace).Create(context.Background(), svc, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			pod.Name = fmt.Sprintf("pod-a-%s-net-default", nodes.Items[1].Name)
			pod.Namespace = "default"
			pod.Labels = map[string]string{"network": "default"}
			podNetDefault = e2epod.PodClientNS(f, "default").CreateSync(context.TODO(), pod)
			ginkgo.DeferCleanup(func() {
				f.ClientSet.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
			})

			svc.Name = fmt.Sprintf("service-default")
			svc.Namespace = "default"
			svc.Spec.Selector = pod.Labels
			svcNetDefault, err = f.ClientSet.CoreV1().Services(pod.Namespace).Create(context.Background(), svc, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.DeferCleanup(func() {
				f.ClientSet.CoreV1().Services(pod.Namespace).Delete(context.Background(), svc.Name, metav1.DeleteOptions{})
			})

			ginkgo.By("Expose networks")
			raCfg := applycfgrav1.RouteAdvertisements(cUDNA.Name + "-ra").
				WithSpec(
					applycfgrav1.RouteAdvertisementsSpec().
						WithAdvertisements(rav1.PodNetwork).
						WithNetworkSelector(
							v1.LabelSelector().WithMatchLabels(cUDNA.Labels),
						),
				)

			raClient, err := raclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			raCUDNA, err := raClient.K8sV1().RouteAdvertisements().Apply(context.TODO(), raCfg, metav1.ApplyOptions{
				FieldManager: f.Namespace.Name,
				Force:        true,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.DeferCleanup(func() { raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), raCUDNA.Name, metav1.DeleteOptions{}) })
			ginkgo.By(fmt.Sprintf("ensure route advertisement matching %s was created successfully", cUDNA.Name))
			gomega.Eventually(func() string {
				ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.TODO(), raCUDNA.Name, metav1.GetOptions{})
				if err != nil {
					return ""
				}
				condition := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
				if condition == nil {
					return ""
				}
				return condition.Reason
			}, 30*time.Second, time.Second).Should(gomega.Equal("Accepted"))

			raCfg.WithName(cUDNB.Name + "-ra")
			raCfg.Spec.NetworkSelector.MatchLabels = cUDNB.Labels
			raCUDNB, err := raClient.K8sV1().RouteAdvertisements().Apply(context.TODO(), raCfg, metav1.ApplyOptions{
				FieldManager: f.Namespace.Name,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.DeferCleanup(func() { raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), raCUDNB.Name, metav1.DeleteOptions{}) })
			ginkgo.By(fmt.Sprintf("ensure route advertisement matching %s was created successfully", cUDNB.Name))
			gomega.Eventually(func() string {
				ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.TODO(), raCUDNB.Name, metav1.GetOptions{})
				if err != nil {
					return ""
				}
				condition := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
				if condition == nil {
					return ""
				}
				return condition.Reason
			}, 30*time.Second, time.Second).Should(gomega.Equal("Accepted"))

		})

		ginkgo.DescribeTable("connectivity between networks",
			func(connInfo func(ipIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool)) {
				gomega.Eventually(func() error {
					clientName, clientNamespace, dst, expectedOutput, expectErr := connInfo(0)
					out, err := checkConnectivity(clientName, clientNamespace, dst)
					if expectErr != (err != nil) {
						return fmt.Errorf("expected connectivity check to return error(%t), got %v, output %v", expectErr, err, out)
					}
					if expectedOutput != "" {
						if !strings.Contains(out, expectedOutput) {
							return fmt.Errorf("expected connectivity check to contain %q, got %q", expectedOutput, out)
						}
					}
					if isIPv6Supported() && isIPv4Supported() {
						clientName, clientNamespace, dst, expectedOutput, expectErr := connInfo(1)
						out, err := checkConnectivity(clientName, clientNamespace, dst)
						if expectErr != (err != nil) {
							return fmt.Errorf("expected connectivity check to return error(%t), got %v, output %v", expectErr, err, out)
						}
						if expectedOutput != "" {
							if !strings.Contains(out, expectedOutput) {
								return fmt.Errorf("expected connectivity check to contain %q, got %q", expectedOutput, out)
							}
						}
					}
					return nil
				}, 30*time.Second).Should(gomega.BeNil())
			},
			ginkgo.Entry("pod to pod on the same network and same node should work",
				func(ipIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					// podsNetA[0] and podsNetA[1] are on the same node
					statusPodA, err := userDefinedNetworkStatus(podsNetA[0], namespacedName(podsNetA[0].Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					statusPodB, err := userDefinedNetworkStatus(podsNetA[1], namespacedName(podsNetA[1].Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					return podsNetA[0].Name, podsNetA[0].Namespace, net.JoinHostPort(statusPodB.IPs[ipIndex].IP.String(), "8080") + "/clientip", statusPodA.IPs[ipIndex].IP.String(), false
				}),
			ginkgo.Entry("pod to pod on the same network and different nodes should work",
				func(ipIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					// podsNetA[0] and podsNetA[2] are on different nodes
					statusPodA, err := userDefinedNetworkStatus(podsNetA[0], namespacedName(podsNetA[0].Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					statusPodB, err := userDefinedNetworkStatus(podsNetA[2], namespacedName(podsNetA[2].Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					return podsNetA[0].Name, podsNetA[0].Namespace, net.JoinHostPort(statusPodB.IPs[ipIndex].IP.String(), "8080") + "/clientip", statusPodA.IPs[ipIndex].IP.String(), false
				}),
			ginkgo.Entry("pod to pod on different networks and same node not should work",
				func(ipIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					// podsNetA[2] and podNetB are on the same node
					statusPodB, err := userDefinedNetworkStatus(podNetB, namespacedName(podNetB.Namespace, cudnBTemplate.Name))
					framework.ExpectNoError(err)
					return podsNetA[2].Name, podsNetA[2].Namespace, net.JoinHostPort(statusPodB.IPs[ipIndex].IP.String(), "8080") + "/clientip", "28", true
				}),

			ginkgo.Entry("pod to pod on different networks and different nodes not should work",
				func(ipIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					// podsNetA[0] and podNetB are on different nodes
					statusPodB, err := userDefinedNetworkStatus(podNetB, namespacedName(podNetB.Namespace, cudnBTemplate.Name))
					framework.ExpectNoError(err)
					expectedOutput = "28"
					expectErr = true
					if cudnATemplate.Spec.Network.Topology == types.Layer3Topology {
						// FIXME: pod to pod on different networks and different nodes should NOT work
						expectedOutput = ""
						expectErr = false
					}
					return podsNetA[0].Name, podsNetA[0].Namespace, net.JoinHostPort(statusPodB.IPs[ipIndex].IP.String(), "8080") + "/clientip", expectedOutput, expectErr
				}),
			ginkgo.Entry("pod in the default network should not be able to access a UDN service",
				func(ipIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					expectedOutput = "28"
					expectErr = true
					if cudnATemplate.Spec.Network.Topology == types.Layer3Topology {
						// FIXME: pod to pod on different networks and different nodes should NOT work
						expectedOutput = ""
						expectErr = false
					}
					return podNetDefault.Name, podNetDefault.Namespace, net.JoinHostPort(svcNetA.Spec.ClusterIPs[ipIndex], "8080") + "/clientip", expectedOutput, expectErr
				}),
			ginkgo.Entry("pod in the UDN should be able to access a service in the same network",
				func(ipIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					return podsNetA[0].Name, podsNetA[0].Namespace, net.JoinHostPort(svcNetA.Spec.ClusterIPs[ipIndex], "8080") + "/clientip", "", false
				}),
			ginkgo.Entry("pod in the UDN should not be able to access a default network service",
				func(ipIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					expectedOutput = "28"
					expectErr = true
					if cudnATemplate.Spec.Network.Topology == types.Layer2Topology {
						// FIXME: pod to pod on different networks and different nodes should NOT work
						expectedOutput = ""
						expectErr = false
					}
					return podsNetA[0].Name, podsNetA[0].Namespace, net.JoinHostPort(svcNetDefault.Spec.ClusterIPs[ipIndex], "8080") + "/clientip", "", false
				}),
			ginkgo.Entry("pod in the UDN should not be able to access a service in a different UDN",
				func(ipIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					expectedOutput = "28"
					expectErr = true
					if cudnATemplate.Spec.Network.Topology == types.Layer3Topology {
						// FIXME: pod to pod on different networks and different nodes should NOT work
						expectedOutput = ""
						expectErr = false
					}
					return podsNetA[0].Name, podsNetA[0].Namespace, net.JoinHostPort(svcNetB.Spec.ClusterIPs[ipIndex], "8080") + "/clientip", expectedOutput, expectErr
				}),
			ginkgo.Entry("host to a local UDN pod should not work",
				func(ipIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					statusPodA, err := userDefinedNetworkStatus(podsNetA[0], namespacedName(podsNetA[0].Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					// TODO: What is going on with L3 - curl returns code 7
					// TODO: L2 sends a reset as a reply to the syn-ack:
					//13:16:03.547673 ovn-k8s-mp283 Out ifindex 1138 0a:58:66:00:00:02 ethertype IPv4 (0x0800), length 80: 102.0.0.2.47900 > 102.0.0.4.8080: Flags [S], seq 373351623, win 65280, options [mss 1360,sackOK,TS val 3311603644 ecr 0,nop,wscale 7], length 0
					//13:16:03.548144 75c707c7ed298_3 Out ifindex 1143 0a:58:66:00:00:02 ethertype IPv4 (0x0800), length 80: 102.0.0.2.47900 > 102.0.0.4.8080: Flags [S], seq 373351623, win 65280, options [mss 1360,sackOK,TS val 3311603644 ecr 0,nop,wscale 7], length 0
					//13:16:03.548164 75c707c7ed298_3 P   ifindex 1143 0a:58:66:00:00:04 ethertype IPv4 (0x0800), length 80: 102.0.0.4.8080 > 102.0.0.2.47900: Flags [S.], seq 1318151057, ack 373351624, win 64704, options [mss 1360,sackOK,TS val 1288395600 ecr 3311603644,nop,wscale 7], length 0
					//13:16:03.548320 ovn-k8s-mp283 In  ifindex 1138 0a:58:66:00:00:04 ethertype IPv4 (0x0800), length 80: 102.0.0.4.8080 > 102.0.0.2.47900: Flags [S.], seq 1318151057, ack 373351624, win 64704, options [mss 1360,sackOK,TS val 1288395600 ecr 3311603644,nop,wscale 7], length 0
					//13:16:03.548340 ovn-k8s-mp283 Out ifindex 1138 0a:58:66:00:00:02 ethertype IPv4 (0x0800), length 60: 102.0.0.2.47900 > 102.0.0.4.8080: Flags [R], seq 373351624, win 0, length 0
					return podsNetA[0].Spec.NodeName, "", net.JoinHostPort(statusPodA.IPs[ipIndex].IP.String(), "8080") + "/clientip", "", true
				}),
			ginkgo.Entry("host to an external UDN pod should not work",
				func(ipIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					// podsNetA[0] and podsNetA[2] are on different nodes
					statusPodA, err := userDefinedNetworkStatus(podsNetA[0], namespacedName(podsNetA[0].Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					expectedOutput = "28"
					expectErr = true
					if cudnATemplate.Spec.Network.Topology == types.Layer3Topology {
						// FIXME: host to UDN pod on different node should not work
						expectedOutput = ""
						expectErr = false
					}
					return podsNetA[2].Spec.NodeName, "", net.JoinHostPort(statusPodA.IPs[ipIndex].IP.String(), "8080") + "/clientip", expectedOutput, expectErr
				}),
		)

	},
	ginkgo.Entry("Layer2",
		&udnv1.ClusterUserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "bgp-udn-layer2-network-a",
				Labels: map[string]string{"bgp-udn-layer2-network-a": ""},
			},
			Spec: udnv1.ClusterUserDefinedNetworkSpec{
				Network: udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer2,
					Layer2: &udnv1.Layer2Config{
						Role:    "Primary",
						Subnets: generateL2Subnets("102.0.0.0/16", "2013:100::0/60"),
					},
				},
			},
		}, &udnv1.ClusterUserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "bgp-udn-layer2-network-b",
				Labels: map[string]string{"bgp-udn-layer2-network-b": ""},
			},
			Spec: udnv1.ClusterUserDefinedNetworkSpec{
				Network: udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer2,
					Layer2: &udnv1.Layer2Config{
						Role:    "Primary",
						Subnets: generateL2Subnets("103.0.0.0/16", "2014:100::0/60"),
					},
				},
			},
		},
	),
	ginkgo.Entry("Layer3",
		&udnv1.ClusterUserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "bgp-udn-layer3-network-a",
				Labels: map[string]string{"bgp-udn-layer3-network-a": ""},
			},
			Spec: udnv1.ClusterUserDefinedNetworkSpec{
				Network: udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer3,
					Layer3: &udnv1.Layer3Config{
						Role: "Primary",
						Subnets: generateL3Subnets(udnv1.Layer3Subnet{
							CIDR:       "102.102.0.0/16",
							HostSubnet: 24,
						}, udnv1.Layer3Subnet{
							CIDR:       "2013:100:200::0/60",
							HostSubnet: 64,
						}),
					},
				},
			},
		}, &udnv1.ClusterUserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "bgp-udn-layer3-network-b",
				Labels: map[string]string{"bgp-udn-layer3-network-b": ""},
			},
			Spec: udnv1.ClusterUserDefinedNetworkSpec{
				Network: udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer3,
					Layer3: &udnv1.Layer3Config{
						Role: "Primary",
						Subnets: generateL3Subnets(udnv1.Layer3Subnet{
							CIDR:       "103.103.0.0/16",
							HostSubnet: 24,
						}, udnv1.Layer3Subnet{
							CIDR:       "2014:100:200::0/60",
							HostSubnet: 64,
						}),
					},
				},
			},
		},
	),
)

func generateL3Subnets(v4, v6 udnv1.Layer3Subnet) []udnv1.Layer3Subnet {
	var subnets []udnv1.Layer3Subnet
	if isIPv4Supported() {
		subnets = append(subnets, v4)
	}
	if isIPv6Supported() {
		subnets = append(subnets, v6)
	}
	return subnets
}

func generateL2Subnets(v4, v6 string) udnv1.DualStackCIDRs {
	var subnets udnv1.DualStackCIDRs
	if isIPv4Supported() {
		subnets = append(subnets, udnv1.CIDR(v4))
	}
	if isIPv6Supported() {
		subnets = append(subnets, udnv1.CIDR(v6))
	}
	return subnets
}

// checkConnectivity performs a curl command from a specified client (pod or node)
// to targetAddress. If clientNamespace is empty the function assumes clientName is a node that will be used as the
// client.
func checkConnectivity(clientName, clientNamespace, targetAddress string) (string, error) {
	curlCmd := []string{"curl", "-g", "-q", "-s", "--max-time", "5", targetAddress}
	var out string
	var err error
	if clientNamespace != "" {
		framework.Logf("Attempting connectivity from pod: %s/%s -> %s", clientNamespace, clientName, targetAddress)
		stdout, stderr, err := e2epodoutput.RunHostCmdWithFullOutput(clientNamespace, clientName, strings.Join(curlCmd, " "))
		out = stdout + "\n" + stderr
		if err != nil {
			return out, fmt.Errorf("connectivity check failed from Pod %s/%s to %s: %w", clientNamespace, clientName, targetAddress, err)
		}
	} else {
		framework.Logf("Attempting connectivity from node: %s -> %s", clientName, targetAddress)
		nodeCmd := []string{containerRuntime, "exec", clientName}
		nodeCmd = append(nodeCmd, curlCmd...)
		out, err = runCommand(nodeCmd...)
		if err != nil {
			// out is empty on error and error contains out...
			return err.Error(), fmt.Errorf("connectivity check failed from node %s to %s: %w", clientName, targetAddress, err)
		}
	}

	client := clientNamespace
	if clientNamespace != "" {
		client = clientNamespace + "/" + client
	}
	framework.Logf("Connectivity check successful:'%s' -> %s", client, targetAddress)
	return out, nil
}
