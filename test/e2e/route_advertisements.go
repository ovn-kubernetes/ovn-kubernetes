package e2e

import (
	"context"
	"fmt"
	"net"
	"strings"

	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	utilnet "k8s.io/utils/net"

	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

var _ = ginkgo.Describe("BGP: Pod to external server when podNetwork advertised", func() {
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
	var nodes *v1.NodeList
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

		var clientPod *v1.Pod
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
		ginkgo.When("tests are run towards the external agnhost echo server", func() {
			ginkgo.It("routes from external bgp server are imported by nodes in the cluster", func() {
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

			})

			ginkgo.It("routes to the default pod network are be advertised to external frr router", func() {
				// Get the first element in the advertisements array (assuming you want to check the first one)
				gomega.Eventually(func() bool {
					podNetworkValue, err := e2ekubectl.RunKubectl("", "get", "ra", "default", "--template={{index .spec.advertisements 0}}")
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					return gomega.Expect(podNetworkValue).To(gomega.Equal("PodNetwork"))
				}).Should(gomega.BeTrue())

				gomega.Eventually(func() bool {
					reason, err := e2ekubectl.RunKubectl("", "get", "ra", "default", "-o", "jsonpath={.status.conditions[?(@.type=='Accepted')].reason}")
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					return gomega.Expect(reason).To(gomega.Equal("Accepted"))
				}).Should(gomega.BeTrue())
			})

			ginkgo.It("all 3 node's podSubnet routes are exported correctly to external FRR router by frr-k8s speakers", func() {
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
						nodeIP := e2enode.GetAddressesByTypeAndFamily(&node, v1.NodeInternalIP, v1.IPv4Protocol)
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
			})

			ginkgo.It("queries to the external server are not SNATed (uses podIP)", func() {
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
})
