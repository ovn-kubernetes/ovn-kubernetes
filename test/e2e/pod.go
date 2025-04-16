package e2e

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/api"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	"k8s.io/utils/net"
)

var _ = ginkgo.Describe("Pod to external server PMTUD", func() {
	const (
		echoServerNameTemplate = "echo-server-%d"
		echoClientName         = "echo-client"
	)
	var providerCtx infraapi.Context

	f := wrappedTestFramework("pod2external-pmtud")

	ginkgo.BeforeEach(func() {
		providerCtx = infraprovider.Get().NewTestContext()
	})

	cleanupFn := func() {}

	ginkgo.AfterEach(func() {
		cleanupFn()
	})

	// The below series of tests queries a server running as a hostNetwork:true pod on nodeB from a client pod running as hostNetwork:false on nodeA
	// This traffic scenario mimics a pod2external setup where large packets and needs frag is involved.
	// for both HTTP and UDP and different ingress and egress payload sizes.
	// Steps:
	// * Set up a hostNetwork:false client pod (agnhost echo server) on nodeA
	// * Set up a external docker container as a server
	// * Query from client pod to server pod
	// Traffic Flow:
	// Req: podA on nodeA -> nodeA switch -> nodeA cluster-router -> nodeA join switch -> nodeA GR -> nodeA ext switch -> nodeA br-ex -> underlay
	// -> server
	// Res: server sends large packet -> br-ex on nodeA -> nodeA ext-switch -> rtoe-GR port sends back needs frag thanks to gateway_mtu option
	// ICMP needs frag goes back to external server
	// server now fragments packets correctly.
	// NOTE: on LGW, the pkt exits via mp0 on nodeA and path is different than what is described above
	// Frag needed is sent by nodeA using ovn-k8s-mp0 interface mtu and not OVN's GR for flows where services are not involved in LGW
	ginkgo.When("a client ovnk pod targeting an external server is created", func() {
		var externalContainer infraapi.ExternalContainer
		var externalContainerIPs []string
		var clientPod *v1.Pod
		var clientPodNodeName string

		var echoPayloads = map[string]string{
			"small": fmt.Sprintf("%010d", 1),
			"large": fmt.Sprintf("%01420d", 1),
		}
		var echoMtuRegex = regexp.MustCompile(`cache expires.*mtu.*`)
		ginkgo.BeforeEach(func() {
			ginkgo.By("Selecting 3 schedulable nodes")
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 2))

			ginkgo.By("Selecting nodes for client pod and server host-networked pod")
			clientPodNodeName = nodes.Items[1].Name

			ginkgo.By("Creating hostNetwork:false (ovnk) client pod")
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

			ginkgo.By("Creating the external server")
			externalContainerPort := infraprovider.Get().GetExternalContainerPort()
			externalContainerName := fmt.Sprintf(echoServerNameTemplate, externalContainerPort)
			framework.Logf("Creating external container server pod listening on TCP and UDP port %d", externalContainerPort)
			providerPrimaryNetwork, err := infraprovider.Get().PrimaryNetwork()
			framework.ExpectNoError(err, "failed to get provider primary network")
			externalContainer = infraapi.ExternalContainer{Name: externalContainerName, Image: images.AgnHost(), Network: providerPrimaryNetwork,
				Args:    []string{"netexec", "--http-port", fmt.Sprintf("%d", externalContainerPort), "--udp-port", fmt.Sprintf("%d", externalContainerPort)},
				ExtPort: externalContainerPort}
			externalContainer, err = providerCtx.CreateExternalContainer(externalContainer)
			framework.ExpectNoError(err, "failed to create external container (%s)", externalContainer)
			if isIPv4Supported() {
				gomega.Expect(externalContainer.GetIPv4()).ToNot(gomega.BeEmpty())
				externalContainerIPs = append(externalContainerIPs, externalContainer.GetIPv4())
			}
			if isIPv6Supported() {
				gomega.Expect(externalContainer.GetIPv6()).ToNot(gomega.BeEmpty())
				externalContainerIPs = append(externalContainerIPs, fmt.Sprintf("[%s]", externalContainer.GetIPv6()))
			}
		})

		// Run queries against the server both with a small (10 bytes + overhead for echo service) and
		// a large (1420 bytes + overhead for echo service) payload.
		// The payload is transmitted to and echoed from the echo service for both HTTP and UDP tests.
		ginkgo.When("tests are run towards the agnhost echo server", func() {
			ginkgo.It("queries to the hostNetworked server pod on another node shall work for TCP", func() {
				gomega.Expect(len(externalContainerIPs)).Should(gomega.BeNumerically(">", 0))
				for _, size := range []string{"small", "large"} {
					for _, externalContainerIP := range externalContainerIPs {
						if externalContainerIP == "" {
							framework.Failf("expected to retrieve external container %s IP but it wasnt found", externalContainer.Name)
						}
						ginkgo.By(fmt.Sprintf("Sending TCP %s payload to container IP %s "+
							"and expecting to receive the same payload", size, externalContainerIP))
						cmd := fmt.Sprintf("curl --max-time 10 -g -q -s http://%s:%s/echo?msg=%s",
							externalContainerIP,
							externalContainer.GetPortStr(),
							echoPayloads[size],
						)
						framework.Logf("Testing TCP %s with command %q", size, cmd)
						stdout, err := e2epodoutput.RunHostCmdWithRetries(
							clientPod.Namespace,
							clientPod.Name,
							cmd,
							framework.Poll,
							60*time.Second)
						framework.ExpectNoError(err, fmt.Sprintf("Testing TCP with %s payload failed", size))
						gomega.Expect(stdout).To(gomega.Equal(echoPayloads[size]), fmt.Sprintf("Testing TCP with %s payload failed", size))
					}
				}
			})
			ginkgo.It("queries to the hostNetworked server pod on another node shall work for UDP", func() {
				providerPrimaryNetwork, err := infraprovider.Get().PrimaryNetwork()
				framework.ExpectNoError(err, "failed to get primary network")
				primaryInf, err := infraprovider.Get().GetK8NodeNetworkInterface(clientPodNodeName, providerPrimaryNetwork)
				framework.ExpectNoError(err, "failed to get provider primary network interface info")
				clientnodeIP := primaryInf.IPv4
				if IsIPv6Cluster(f.ClientSet) && isIPv6Supported() {
					clientnodeIP = fmt.Sprintf("[%s]", primaryInf.IPv6)
				}
				gomega.Expect(clientnodeIP).NotTo(gomega.BeEmpty())
				for _, size := range []string{"small", "large"} {
					for _, externalContainerIP := range externalContainerIPs {
						if size == "large" {
							// Flushing the IP route cache will remove any routes in the cache
							// that are a result of receiving a "need to frag" packet.
							ginkgo.By("Flushing the ip route cache")
							stdout, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer, []string{"ip", "route", "flush", "cache"})
							framework.ExpectNoError(err, "Flushing the ip route cache failed")
							framework.Logf("Flushed cache on %s", externalContainer.GetName())
							// List the current IP route cache for informative purposes.
							cmd := fmt.Sprintf("ip route get %s", clientnodeIP)
							stdout, err = infraprovider.Get().ExecExternalContainerCommand(externalContainer, []string{"ip", "route", "get", clientnodeIP})
							framework.ExpectNoError(err, "Listing IP route cache")
							framework.Logf("%s: %s", cmd, stdout)
						}
						// We expect the following to fail at least once for large payloads and non-hostNetwork
						// endpoints: the first request will fail as we have to receive a "need to frag" ICMP
						// message, subsequent requests then should succeed.
						gomega.Eventually(func() error {
							ginkgo.By(fmt.Sprintf("Sending UDP %s payload to server IP %s "+
								"and expecting to receive the same payload", size, externalContainerIP))
							// Send payload via UDP.
							cmd := fmt.Sprintf("echo 'echo %s' | nc -w2 -u %s %s",
								echoPayloads[size],
								externalContainerIP,
								externalContainer.GetPortStr(),
							)
							framework.Logf("Testing UDP %s with command %q", size, cmd)
							stdout, err := e2epodoutput.RunHostCmd(
								clientPod.Namespace,
								clientPod.Name,
								cmd)
							if err != nil {
								return err
							}
							// Compare received payload vs sent payload.
							if stdout != echoPayloads[size] {
								return fmt.Errorf("stdout does not match payloads[%s], %s != %s", size, stdout, echoPayloads[size])
							}
							if size == "large" {
								ginkgo.By("Making sure that the ip route cache contains an MTU route")
								// Get IP route cache and make sure that it contains an MTU route on the server side.
								stdout, err = infraprovider.Get().ExecExternalContainerCommand(externalContainer, []string{"ip", "route", "get", clientnodeIP})
								if err != nil {
									return fmt.Errorf("could not list IP route cache using cmd: %s, err: %q", cmd, err)
								}
								framework.Logf("Route cache on server pod %s", stdout)
								if !echoMtuRegex.Match([]byte(stdout)) {
									return fmt.Errorf("cannot find MTU cache entry in route: %s", stdout)
								}
							}
							return nil
						}, 60*time.Second, 1*time.Second).Should(gomega.Succeed())
						// Flushing the IP route cache will remove any routes in the cache
						// that are a result of receiving a "need to frag" packet. Let's
						// flush this on all 3 nodes else we will run into the
						// bug: https://issues.redhat.com/browse/OCPBUGS-7609.
						// TODO: Revisit this once https://bugzilla.redhat.com/show_bug.cgi?id=2169839 is fixed.
						ovnKubeNodePods, err := f.ClientSet.CoreV1().Pods(deploymentconfig.Get().OVNKubernetesNamespace()).List(context.TODO(), metav1.ListOptions{
							LabelSelector: "name=ovnkube-node",
						})
						if err != nil {
							framework.Failf("could not get ovnkube-node pods: %v", err)
						}
						for _, ovnKubeNodePod := range ovnKubeNodePods.Items {
							framework.Logf("Flushing the ip route cache on %s", ovnKubeNodePod.Name)
							containerName := "ovnkube-node"
							if isInterconnectEnabled() {
								containerName = "ovnkube-controller"
							}
							_, err := e2ekubectl.RunKubectl(deploymentconfig.Get().OVNKubernetesNamespace(), "exec", ovnKubeNodePod.Name, "--container", containerName, "--",
								"ip", "route", "flush", "cache")
							framework.ExpectNoError(err, "Flushing the ip route cache failed")
						}
						framework.Logf("Flushing the ip route cache on %s", externalContainer.GetName())
						_, err = infraprovider.Get().ExecExternalContainerCommand(externalContainer, []string{"ip", "route", "flush", "cache"})
						framework.ExpectNoError(err, "Flushing the ip route cache failed")
					}
				}
			})
		})
	})
})

var _ = ginkgo.Describe("Pod to pod TCP with low MTU", func() {
	const (
		echoServerPodNameTemplate = "echo-server-pod-%d"
		echoClientPodName         = "echo-client-pod"
		mtu                       = 1400
	)

	f := wrappedTestFramework("pod2pod-tcp-low-mtu")
	cleanupFn := func() {}

	ginkgo.AfterEach(func() {
		cleanupFn()
	})

	ginkgo.When("a client ovnk pod targeting an ovnk pod server(running on another node) with low mtu", func() {
		var serverPod *v1.Pod
		var serverPodNodeName string
		var serverPodName string
		var serverNode v1.Node
		var clientNode v1.Node
		var serverNodeInternalIPs []string
		var clientNodeInternalIPs []string

		var clientPod *v1.Pod
		var clientPodNodeName string

		payload := fmt.Sprintf("%01360d", 1)

		ginkgo.BeforeEach(func() {
			ginkgo.By("Selecting 2 schedulable nodes")
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 1))

			ginkgo.By("Selecting nodes for client pod and server host-networked pod")
			serverPodNodeName = nodes.Items[0].Name
			serverNode = nodes.Items[0]
			clientPodNodeName = nodes.Items[1].Name
			clientNode = nodes.Items[1]

			ginkgo.By("Creating hostNetwork:false (ovnk) client pod")
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

			ginkgo.By("Creating hostNetwork:false (ovnk) server pod")
			serverPodName = fmt.Sprintf(echoServerPodNameTemplate, 8080)
			serverPod = e2epod.NewAgnhostPod(f.Namespace.Name, serverPodName, nil, nil, nil,
				"netexec",
				"--http-port", fmt.Sprintf("8080"),
				"--udp-port", fmt.Sprintf("8080"),
			)
			serverPod.ObjectMeta.Labels = map[string]string{
				"app": serverPodName,
			}
			serverPod.Spec.NodeName = serverPodNodeName
			serverPod = e2epod.NewPodClient(f).CreateSync(context.TODO(), serverPod)

			ginkgo.By("Getting all InternalIP addresses of the server node")
			serverNodeInternalIPs = e2enode.GetAddresses(&serverNode, v1.NodeInternalIP)
			gomega.Expect(len(serverNodeInternalIPs)).To(gomega.BeNumerically(">", 0))

			ginkgo.By("Getting all InternalIP addresses of the client node")
			clientNodeInternalIPs = e2enode.GetAddresses(&clientNode, v1.NodeInternalIP)
			gomega.Expect(len(serverNodeInternalIPs)).To(gomega.BeNumerically(">", 0))

			ginkgo.By("Lowering the MTU route from server -> client")
			fmt.Println(clientNodeInternalIPs)
			err = addRouteToNode(serverPodNodeName, clientNodeInternalIPs, mtu)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("Lowering the MTU route from client -> server")
			fmt.Println(serverNodeInternalIPs)
			err = addRouteToNode(clientPodNodeName, serverNodeInternalIPs, mtu)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			cleanupFn = func() {
				err = delRouteToNode(serverPodNodeName, clientNodeInternalIPs)
				err = delRouteToNode(clientPodNodeName, serverNodeInternalIPs)
			}

		})

		// Lower the MTU between the two nodes to 1400, this will cause pod->pod 1400 byte packets
		// to be too big for geneve encapsulation
		ginkgo.When("MTU is lowered between the two nodes", func() {
			ginkgo.It("large queries to the server pod on another node shall work for TCP", func() {
				for _, serverPodIP := range serverPod.Status.PodIPs {
					if net.IsIPv6String(serverPodIP.IP) {
						serverPodIP.IP = fmt.Sprintf("[%s]", serverPodIP)
					}

					ginkgo.By(fmt.Sprintf("Sending TCP large payload to server IP %s "+
						"and expecting to receive the same payload", serverPodIP))
					cmd := fmt.Sprintf("curl --max-time 10 -g -q -s http://%s:%d/echo?msg=%s",
						serverPodIP.IP,
						8080,
						payload,
					)
					framework.Logf("Testing large TCP segments with command %q", cmd)
					stdout, err := e2epodoutput.RunHostCmdWithRetries(
						clientPod.Namespace,
						clientPod.Name,
						cmd,
						framework.Poll,
						30*time.Second)
					framework.ExpectNoError(err, "Sending large TCP payload from client failed")
					gomega.Expect(stdout).To(gomega.Equal(payload),
						"Received TCP payload from server does not equal expected payload")

					cmd = fmt.Sprintf("ip route get %s", serverPodIP.IP)
					stdout, err = e2epodoutput.RunHostCmd(
						clientPod.Namespace,
						clientPod.Name,
						cmd)
					framework.ExpectNoError(err, "Checking ip route cache output failed")
					framework.Logf(" ip route output in client pod: %s", stdout)
					gomega.Expect(stdout).To(gomega.MatchRegexp("mtu 1342"))
				}
			})
		})
	})
})
