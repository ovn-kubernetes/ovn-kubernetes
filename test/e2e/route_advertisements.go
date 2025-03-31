package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"time"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	utilnet "k8s.io/utils/net"

	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

var _ = ginkgo.Describe("BGP: Pod to external server when default podNetwork is advertised", func() {
	const (
		serverContainerName    = "bgpserver"
		routerContainerName    = "frr"
		echoClientPodName      = "echo-client-pod"
		echoServerPodPortMin   = 9800
		echoServerPodPortMax   = 9899
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

var _ = ginkgo.Describe("BGP: Pod to external server when CUDN Layer3 Network is advertised", func() {
	const (
		serverContainerName    = "bgpserver"
		routerContainerName    = "frr"
		echoClientPodName      = "echo-client-pod"
		echoServerPodPortMin   = 9800
		echoServerPodPortMax   = 9899
		primaryNetworkName     = "kind"
		bgpExternalNetworkName = "bgpnet"
		testCudnName           = "bgp-udn-layer3-network"
		testRAName             = "udn-layer3-ra"
	)
	var serverContainerIPs []string
	var frrContainerIPv4, frrContainerIPv6 string
	var nodes *corev1.NodeList
	var cs clientset.Interface
	f := wrappedTestFramework("pod2external-route-advertisements")
	// disable automatic namespace creation, we need to add the required UDN label
	f.SkipNamespaceCreation = true

	ginkgo.BeforeEach(func() {
		cs = f.ClientSet

		var err error
		namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		f.Namespace = namespace
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

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
		var err error

		ginkgo.BeforeEach(func() {
			if !IsGatewayModeLocal() {
				const upstreamIssue = "https://github.com/ovn-kubernetes/ovn-kubernetes/issues/5134"
				e2eskipper.Skipf(
					"The import of routes into UDNs is broken on shared gateway mode. Upstream issue: %s", upstreamIssue,
				)
			}
			ginkgo.By("Selecting 3 schedulable nodes")
			nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 2))

			ginkgo.By("create layer3 ClusterUserDefinedNetwork")
			cudnManifest := generateBGPCUDNManifest(testCudnName, f.Namespace.Name)
			cleanup, err := createManifest("", cudnManifest)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, testCudnName), 5*time.Second, time.Second).Should(gomega.Succeed())

			conditionsJSON, err := e2ekubectl.RunKubectl("", "get", "clusteruserdefinednetwork", testCudnName, "-o", "jsonpath={.status.conditions}")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			var actualConditions []metav1.Condition
			gomega.Expect(json.Unmarshal([]byte(conditionsJSON), &actualConditions)).To(gomega.Succeed())
			ginkgo.DeferCleanup(func() {
				cleanup()
				ginkgo.By(fmt.Sprintf("delete pods in %s namespace to unblock CUDN CR & associate NAD deletion", f.Namespace.Name))
				gomega.Expect(cs.CoreV1().Pods(f.Namespace.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(gomega.Succeed())
				_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", testCudnName, "--wait", fmt.Sprintf("--timeout=%ds", 120))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})

			ginkgo.By("Creating client pod on the udn namespace")
			podConfig := *podConfig(echoClientPodName)
			podConfig.namespace = f.Namespace.Name
			clientPod = runUDNPod(cs, f.Namespace.Name, podConfig, nil)

			ginkgo.By("asserting the pod UDN interface on the network-status annotation")
			udnNetStat, err := podNetworkStatus(clientPod, func(status nadapi.NetworkStatus) bool {
				return status.Default
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			const (
				expectedDefaultNetStatusLen = 1
				ovnUDNInterface             = "ovn-udn1"
			)
			gomega.Expect(udnNetStat).To(gomega.HaveLen(expectedDefaultNetStatusLen))
			gomega.Expect(udnNetStat[0].Interface).To(gomega.Equal(ovnUDNInterface))

			gomega.Expect(len(serverContainerIPs)).To(gomega.BeNumerically(">", 0))
		})
		ginkgo.AfterEach(func() {
			e2ekubectl.RunKubectlOrDie("", "delete", "ra", testRAName, "--ignore-not-found=true")
		})
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

			ginkgo.By("routes to the CUDN network are advertised to external frr router")
			// Get the first element in the advertisements array (assuming you want to check the first one)
			ginkgo.By("create route advertisement matching CUDN Network")
			raManifest := generateRAManifest(testRAName)
			cleanup, err := createManifest(f.Namespace.Name, raManifest)
			ginkgo.DeferCleanup(cleanup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("ensure route advertisement matching CUDN was created successfully")
			gomega.Eventually(func() string {
				podNetworkValue, err := e2ekubectl.RunKubectl("", "get", "ra", testRAName, "--template={{index .spec.advertisements 0}}")
				if err != nil {
					return ""
				}
				return podNetworkValue
			}, 5*time.Second, time.Second).Should(gomega.Equal("PodNetwork"))

			gomega.Eventually(func() string {
				reason, err := e2ekubectl.RunKubectl("", "get", "ra", testRAName, "-o", "jsonpath={.status.conditions[?(@.type=='Accepted')].reason}")
				if err != nil {
					return ""
				}
				return reason
			}, 30*time.Second, time.Second).Should(gomega.Equal("Accepted"))

			ginkgo.By("queries to the external server are not SNATed (uses UDN podIP)")
			podIP, err := podIPsForUserDefinedPrimaryNetwork(cs, f.Namespace.Name, clientPod.Name, namespacedName(f.Namespace.Name, testCudnName), 0)
			framework.ExpectNoError(err, fmt.Sprintf("Getting podIPs for pod %s failed: %v", clientPod.Name, err))
			framework.Logf("Client pod IP address=%s", podIP)
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
				if isIPv6Supported() && utilnet.IsIPv6String(serverContainerIP) {
					podIP, err := podIPsForUserDefinedPrimaryNetwork(cs, f.Namespace.Name, clientPod.Name, namespacedName(f.Namespace.Name, testCudnName), 1)
					framework.ExpectNoError(err, fmt.Sprintf("Getting podIPs for pod %s failed: %v", clientPod.Name, err))
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
		})
	})
})

func generateBGPCUDNManifest(testCudnName string, targetNamespaces ...string) string {
	targetNs := strings.Join(targetNamespaces, ",")
	return `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: ` + testCudnName + `
  labels:
    k8s.ovn.org/bgp-network: ""
spec:
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values: [ ` + targetNs + ` ]
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets: ` + generateCIDRforClusterUDN("103.103.0.0/16", "2014:100:200::0/60")
}

func generateRAManifest(name string) string {
	return `
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: ` + name + `
spec:
  networkSelector:
    matchLabels:
      k8s.ovn.org/bgp-network: ""
  advertisements:
    - "PodNetwork"`
}
