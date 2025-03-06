package e2e

import (
	"context"
	"fmt"
	"strings"

	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"

	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

var _ = ginkgo.Describe("[Feature:BGP] Pod to external server when podNetwork advertised", func() {
	const (
		serverContainerName  = "bgpserver"
		echoClientPodName    = "echo-client-pod"
		echoServerPodPortMin = 9800
		echoServerPodPortMax = 9899
		primaryNetworkName   = "kind"
	)

	f := wrappedTestFramework("pod2external-route-advertisements")
	cleanupFn := func() {}

	ginkgo.AfterEach(func() {
		cleanupFn()
	})

	ginkgo.When("a client ovnk pod targeting an external server is created", func() {
		//var serverContainerPort int
		var serverContainerIPs []string

		var clientPod *v1.Pod
		var clientPodNodeName string
		var frrRouterIPv4, frrRouterIPV6 string

		ginkgo.BeforeEach(func() {
			if !isDefaultNetworkAdvertised() {
				e2eskipper.Skipf(
					"skipping pod to external server tests when podNetwork is not advertised",
				)
			}
			ginkgo.By("Selecting 3 schedulable nodes")
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 2))

			ginkgo.By("Selecting nodes for client pod")
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
			// NOTE: The BGP server is already created during KIND cluster install time that is connected with
			// the FRR router container already
			ginkgo.By("Get the server IPs from the external bgp server (created during install time)")
			bgpServerIPv4, bgpServerIPv6 := getContainerAddressesForNetwork(serverContainerName, "bgpnet")
			if isIPv4Supported() {
				serverContainerIPs = append(serverContainerIPs, bgpServerIPv4)
			}

			if isIPv6Supported() {
				serverContainerIPs = append(serverContainerIPs, bgpServerIPv6)
			}
			gomega.Expect(len(serverContainerIPs)).To(gomega.BeNumerically(">", 0))
			framework.Logf("The external server IPs are: %+v", serverContainerIPs)

			ginkgo.By("For return traffic to work add a route in the bgpserver for the client pod IP towards frr container")
			ipCmd := []string{"ip"}
			mask := 32
			cmd := []string{"docker", "exec", serverContainerName}
			podIP := getPodAddress(echoClientPodName, f.Namespace.Name)
			frrRouterIPv4, frrRouterIPV6 = getContainerAddressesForNetwork("frr", "bgpnet")
			containerIP := frrRouterIPv4
			if isIPv6Supported() {
				mask = 128
				ipCmd = []string{"ip", "-6"}
				containerIP = frrRouterIPV6
			}
			cmd = append(cmd, ipCmd...)
			cmd = append(cmd, "route", "add", fmt.Sprintf("%s/%d", podIP, mask), "via", containerIP)
			_, err = runCommand(cmd...)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		})

		ginkgo.AfterEach(func() {
			ginkgo.By("Removing external container")
			if len(serverContainerName) > 0 {
				ginkgo.By("Remove the podIP from the external container which is used for other tests in this suite")
				ipCmd := []string{"ip"}
				mask := 32
				cmd := []string{"docker", "exec", serverContainerName}
				podIP := getPodAddress(echoClientPodName, f.Namespace.Name)
				containerIP := frrRouterIPv4
				if isIPv6Supported() {
					mask = 128
					ipCmd = []string{"ip", "-6"}
					containerIP = frrRouterIPV6
				}
				cmd = append(cmd, ipCmd...)
				cmd = append(cmd, "route", "del", fmt.Sprintf("%s/%d", podIP, mask), "via", containerIP)
				_, err := runCommand(cmd...)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
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
			ginkgo.It("routes to the default pod network shall be advertised to external", func() {
				// Get the first element in the advertisements array (assuming you want to check the first one)
				podNetworkValue, err := e2ekubectl.RunKubectl("", "get", "ra", "default", "--template={{index .spec.advertisements 0}}")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(podNetworkValue).To(gomega.Equal("PodNetwork"))

				reason, err := e2ekubectl.RunKubectl("", "get", "ra", "default", "-o", "jsonpath={.status.conditions[?(@.type=='Accepted')].reason}")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(reason).To(gomega.Equal("Accepted"))
			})

			ginkgo.It("queries to the external server shall not be SNATed (uses podIP)", func() {
				podIP := getPodAddress(clientPod.Name, clientPod.Namespace)
				framework.Logf("get client pod IP address %s", podIP)
				for _, serverContainerIP := range serverContainerIPs {
					ginkgo.By(fmt.Sprintf("Sending request to node IP %s "+
						"and expecting to receive the same payload", serverContainerIP))
					cmd := fmt.Sprintf("curl --max-time 10 -g -q -s http://%s:8080/clientip",
						serverContainerIP,
					)
					framework.Logf("Testing pod to external traffic with command %q", cmd)
					stdout, err := e2epodoutput.RunHostCmdWithRetries(
						clientPod.Namespace,
						clientPod.Name,
						cmd,
						framework.Poll,
						60*time.Second)
					framework.ExpectNoError(err, fmt.Sprintf("Testing pod to external traffic failed: %v", err))
					gomega.Expect(strings.Split(stdout, ":")[0]).To(gomega.Equal(podIP),
						fmt.Sprintf("Testing pod %s to external traffic failed while analysing output %v", echoClientPodName, stdout))
				}
			})
		})
	})
})
