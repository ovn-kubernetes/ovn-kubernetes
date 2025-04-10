package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	rav1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	apitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	utilnet "k8s.io/utils/net"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
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
  networkSelectors:
    - networkSelectionType: ClusterUserDefinedNetworks
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            k8s.ovn.org/bgp-network: ""
  advertisements:
    - "PodNetwork"
  nodeSelector: {}
  frrConfigurationSelector: {}`
}

var _ = ginkgo.DescribeTableSubtree("BGP: isolation between advertised networks",
	func(cudnATemplate, cudnBTemplate *udnv1.ClusterUserDefinedNetwork) {
		const curlConnectionTimeoutCode = "28"

		f := wrappedTestFramework("bpp-network-isolation")
		f.SkipNamespaceCreation = true
		var udnNamespaceA, udnNamespaceB *corev1.Namespace
		var nodes *corev1.NodeList
		// podsNetA has 3 pods in cudnA, two are on nodes[0] and the last one is on nodes[1] - done in BeforeEach
		var podsNetA []*corev1.Pod

		// podNetB is in cudnB hosted on nodes[1], podNetDefault is in the default network hosted on nodes[1] - done in BeforeEach
		var podNetB, podNetDefault *corev1.Pod
		var svcNetA, svcNetB, svcNetDefault *corev1.Service
		var cudnA, cudnB *udnv1.ClusterUserDefinedNetwork
		var ra *rav1.RouteAdvertisements

		ginkgo.BeforeEach(func() {
			ginkgo.By("Configuring primary UDN namespaces")
			var err error
			udnNamespaceA, err = f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			f.Namespace = udnNamespaceA
			udnNamespaceB, err = f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("Configuring networks")
			cudnATemplate.Spec.NamespaceSelector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{udnNamespaceA.Name},
			}}}
			cudnBTemplate.Spec.NamespaceSelector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{udnNamespaceB.Name},
			}}}

			// set a common label used to advertise both networks with one RA
			cudnATemplate.Labels["advertised-networks-isolation"] = ""
			cudnBTemplate.Labels["advertised-networks-isolation"] = ""

			udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			cudnA, err = udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.Background(), cudnATemplate, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			cudnB, err = udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.Background(), cudnBTemplate, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("Waiting for networks to be ready")
			gomega.Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnA.Name), 5*time.Second, time.Second).Should(gomega.Succeed())
			gomega.Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnB.Name), 5*time.Second, time.Second).Should(gomega.Succeed())

			ginkgo.By("Selecting 3 schedulable nodes")
			nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 2))

			ginkgo.By("Setting up pods and services")
			podsNetA = []*corev1.Pod{}
			pod := e2epod.NewAgnhostPod(udnNamespaceA.Name, fmt.Sprintf("pod-1-%s-net-%s", nodes.Items[0].Name, cudnA.Name), nil, nil, []corev1.ContainerPort{{ContainerPort: 8080}}, "netexec")
			pod.Spec.NodeName = nodes.Items[0].Name
			pod.Labels = map[string]string{"network": cudnA.Name}
			podsNetA = append(podsNetA, e2epod.NewPodClient(f).CreateSync(context.TODO(), pod))

			pod.Name = fmt.Sprintf("pod-2-%s-net-%s", nodes.Items[0].Name, cudnA.Name)
			podsNetA = append(podsNetA, e2epod.NewPodClient(f).CreateSync(context.TODO(), pod))

			pod.Name = fmt.Sprintf("pod-3-%s-net-%s", nodes.Items[1].Name, cudnA.Name)
			pod.Spec.NodeName = nodes.Items[1].Name
			podsNetA = append(podsNetA, e2epod.NewPodClient(f).CreateSync(context.TODO(), pod))

			svc := e2eservice.CreateServiceSpec(fmt.Sprintf("service-%s", cudnA.Name), "", false, pod.Labels)
			svc.Spec.Ports = []corev1.ServicePort{{Port: 8080}}
			familyPolicy := corev1.IPFamilyPolicyPreferDualStack
			svc.Spec.IPFamilyPolicy = &familyPolicy
			svcNetA, err = f.ClientSet.CoreV1().Services(pod.Namespace).Create(context.Background(), svc, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			pod.Name = fmt.Sprintf("pod-1-%s-net-%s", nodes.Items[1].Name, cudnB.Name)
			pod.Namespace = udnNamespaceB.Name
			pod.Labels = map[string]string{"network": cudnB.Name}
			podNetB = e2epod.PodClientNS(f, udnNamespaceB.Name).CreateSync(context.TODO(), pod)

			svc.Name = fmt.Sprintf("service-%s", cudnB.Name)
			svc.Namespace = pod.Namespace
			svc.Spec.Selector = pod.Labels
			svcNetB, err = f.ClientSet.CoreV1().Services(pod.Namespace).Create(context.Background(), svc, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			pod.Name = fmt.Sprintf("pod-1-%s-net-default", nodes.Items[1].Name)
			pod.Namespace = "default"
			pod.Labels = map[string]string{"network": "default"}
			podNetDefault = e2epod.PodClientNS(f, "default").CreateSync(context.TODO(), pod)

			svc.Name = fmt.Sprintf("service-default")
			svc.Namespace = "default"
			svc.Spec.Selector = pod.Labels
			svcNetDefault, err = f.ClientSet.CoreV1().Services(pod.Namespace).Create(context.Background(), svc, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("Expose networks")
			ra := &rav1.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "advertised-networks-isolation-ra",
				},
				Spec: rav1.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{
							NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"advertised-networks-isolation": ""},
								},
							},
						},
					},
					NodeSelector:             metav1.LabelSelector{},
					FRRConfigurationSelector: metav1.LabelSelector{},
					Advertisements: []rav1.AdvertisementType{
						rav1.PodNetwork,
					},
				},
			}

			raClient, err := raclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ra, err = raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("ensure route advertisement matching both networks was created successfully")
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
		})

		ginkgo.AfterEach(func() {
			gomega.Expect(f.ClientSet.CoreV1().Pods(udnNamespaceA.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(gomega.Succeed())
			gomega.Expect(f.ClientSet.CoreV1().Pods(udnNamespaceB.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(gomega.Succeed())

			udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			if cudnB != nil {
				err = udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), cudnB.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() bool {
					_, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Get(context.TODO(), cudnB.Name, metav1.GetOptions{})
					return apierrors.IsNotFound(err)
				}, time.Second*30).Should(gomega.BeTrue())
				cudnB = nil
			}
			if cudnA != nil {
				err = udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), cudnA.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() bool {
					_, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Get(context.TODO(), cudnA.Name, metav1.GetOptions{})
					return apierrors.IsNotFound(err)
				}, time.Second*30).Should(gomega.BeTrue())
				cudnA = nil
			}

			if podNetDefault != nil {
				err = f.ClientSet.CoreV1().Pods(podNetDefault.Namespace).Delete(context.Background(), podNetDefault.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				podNetDefault = nil
			}
			if svcNetDefault != nil {
				err = f.ClientSet.CoreV1().Services(svcNetDefault.Namespace).Delete(context.Background(), svcNetDefault.Name, metav1.DeleteOptions{})
				svcNetDefault = nil
			}

			raClient, err := raclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			if ra != nil {
				err = raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), ra.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ra = nil
			}
		})

		ginkgo.DescribeTable("connectivity between networks",
			func(connInfo func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool)) {
				// checkConnectivity performs a curl command from a specified client (pod or node)
				// to targetAddress. If clientNamespace is empty the function assumes clientName is a node that will be used as the
				// client.
				var checkConnectivity = func(clientName, clientNamespace, targetAddress string) (string, error) {
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
						// use ipFamilyIndex of 1 to pick the IPv6 addresses
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
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					// podsNetA[0] and podsNetA[1] are on the same node
					clientPod := podsNetA[0]
					srvPod := podsNetA[1]

					clientPodStatus, err := userDefinedNetworkStatus(clientPod, namespacedName(clientPod.Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					srvPodStatus, err := userDefinedNetworkStatus(srvPod, namespacedName(srvPod.Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					return clientPod.Name, clientPod.Namespace, net.JoinHostPort(srvPodStatus.IPs[ipFamilyIndex].IP.String(), "8080") + "/clientip", clientPodStatus.IPs[ipFamilyIndex].IP.String(), false
				}),
			ginkgo.Entry("pod to pod on the same network and different nodes should work",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					// podsNetA[0] and podsNetA[2] are on different nodes
					clientPod := podsNetA[0]
					srvPod := podsNetA[2]

					clientPodStatus, err := userDefinedNetworkStatus(clientPod, namespacedName(clientPod.Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					srvPodStatus, err := userDefinedNetworkStatus(srvPod, namespacedName(srvPod.Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					return clientPod.Name, clientPod.Namespace, net.JoinHostPort(srvPodStatus.IPs[ipFamilyIndex].IP.String(), "8080") + "/clientip", clientPodStatus.IPs[ipFamilyIndex].IP.String(), false
				}),
			ginkgo.Entry("pod to pod on different networks and same node not should work",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					// podsNetA[2] and podNetB are on the same node
					clientPod := podsNetA[2]
					srvPod := podNetB

					srvPodStatus, err := userDefinedNetworkStatus(srvPod, namespacedName(srvPod.Namespace, cudnBTemplate.Name))
					framework.ExpectNoError(err)
					return clientPod.Name, clientPod.Namespace, net.JoinHostPort(srvPodStatus.IPs[ipFamilyIndex].IP.String(), "8080") + "/clientip", curlConnectionTimeoutCode, true
				}),

			ginkgo.Entry("pod to pod on different networks and different nodes not should work",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					// podsNetA[0] and podNetB are on different nodes
					clientPod := podsNetA[0]
					srvPod := podNetB

					srvPodStatus, err := userDefinedNetworkStatus(srvPod, namespacedName(srvPod.Namespace, cudnBTemplate.Name))
					framework.ExpectNoError(err)
					// FIXME: L3 - pod to pod on different networks and different nodes should NOT work
					return clientPod.Name, clientPod.Namespace, net.JoinHostPort(srvPodStatus.IPs[ipFamilyIndex].IP.String(), "8080") + "/clientip", "", false
				}),
			ginkgo.Entry("pod in the default network should not be able to access a UDN service",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					return podNetDefault.Name, podNetDefault.Namespace, net.JoinHostPort(svcNetA.Spec.ClusterIPs[ipFamilyIndex], "8080") + "/clientip", curlConnectionTimeoutCode, true
				}),
			ginkgo.Entry("pod in the UDN should be able to access a service in the same network",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					return podsNetA[0].Name, podsNetA[0].Namespace, net.JoinHostPort(svcNetA.Spec.ClusterIPs[ipFamilyIndex], "8080") + "/clientip", "", false
				}),
			ginkgo.Entry("pod in the UDN should not be able to access a default network service",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					err := true
					out := curlConnectionTimeoutCode
					if isLocalGWModeEnabled() {
						// FIXME: L3 - pod in the UDN should NOT be able to access a default network service
						err = false
						out = ""
					}
					return podsNetA[0].Name, podsNetA[0].Namespace, net.JoinHostPort(svcNetDefault.Spec.ClusterIPs[ipFamilyIndex], "8080") + "/clientip", out, err
				}),
			ginkgo.Entry("pod in the UDN should not be able to access a service in a different UDN",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					return podsNetA[0].Name, podsNetA[0].Namespace, net.JoinHostPort(svcNetB.Spec.ClusterIPs[ipFamilyIndex], "8080") + "/clientip", curlConnectionTimeoutCode, true
				}),
			ginkgo.Entry("host to a local UDN pod should not work",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					clientNode := podsNetA[0].Spec.NodeName
					srvPod := podsNetA[0]

					srvPodStatus, err := userDefinedNetworkStatus(srvPod, namespacedName(srvPod.Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					// FIXME: L3 - curl returns code 7, the request works partially
					// The client starts a connection from the default VRF. An IP rule sends this traffic into a specific VRF to reach the server pod.
					// When the server pod replies (SYN-ACK), the reply packet is handled by the VRF. This stack doesn't contain the original client socket information,
					// so it sees no active connection matching the reply and terminates the attempt with a RST.
					return clientNode, "", net.JoinHostPort(srvPodStatus.IPs[ipFamilyIndex].IP.String(), "8080") + "/clientip", "", true
				}),
			ginkgo.Entry("host to an different node UDN pod should not work",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					// podsNetA[0] and podsNetA[2] are on different nodes
					clientNode := podsNetA[2].Spec.NodeName
					srvPod := podsNetA[0]

					srvPodStatus, err := userDefinedNetworkStatus(srvPod, namespacedName(srvPod.Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					// FIXME: L3 - host to UDN pod on different node should not work
					return clientNode, "", net.JoinHostPort(srvPodStatus.IPs[ipFamilyIndex].IP.String(), "8080") + "/clientip", "", false
				}),
		)

	},
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
