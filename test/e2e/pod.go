package e2e

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/images"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = ginkgo.Describe("Pod to external server PMTUD", func() {
	const (
		echoServerPodNameTemplate = "echo-server-pod-%d"
		echoClientPodName         = "echo-client-pod"
		echoServerPodPortMin      = 9800
		echoServerPodPortMax      = 9899
		primaryNetworkName        = "kind"
	)

	f := wrappedTestFramework("pod2external-pmtud")
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
	// Req: podA on nodeA -> nodeA switch -> nodeA cluster-route -> nodeA transit switch -> nodeA join switch -> nodeA GR -> nodeA ext switch -> nodeA br-ex -> underlay
	// underlay -> server
	// Res: server sends large packet -> br-ex on nodeA -> nodeA ext-switch -> rtoe-GR port sends back needs frag thanks to gateway_mtu option
	// ICMP needs frag goes back to external server
	// server now fragments packets correctly.
	// NOTE: on LGW, the pkt exits via mp0 on nodeA and path is different than what is described above
	// Frag needed is sent by nodeA using ovn-k8s-mp0 interface mtu and not OVN's GR for flows where services are not involved in LGW
	ginkgo.When("a client ovnk pod targeting an external server is created", func() {
		var serverPodPort int
		var serverPodName string
		var serverNodeInternalIPs []string

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
			serverPodPort = rand.Intn(echoServerPodPortMax-echoServerPodPortMin) + echoServerPodPortMin
			serverPodName = fmt.Sprintf(echoServerPodNameTemplate, serverPodPort)
			framework.Logf("Creating server pod listening on TCP and UDP port %d", serverPodPort)
			agntHostCmds := []string{"netexec", "--http-port", fmt.Sprintf("%d", serverPodPort), "--udp-port", fmt.Sprintf("%d", serverPodPort)}
			externalIpv4, externalIpv6 := createClusterExternalContainer(serverPodName, images.AgnHost(),
				[]string{"--network", "kind", "-P", "--cap-add", "NET_ADMIN"},
				agntHostCmds,
			)

			if isIPv4Supported() {
				serverNodeInternalIPs = append(serverNodeInternalIPs, externalIpv4)
			}

			if isIPv6Supported() {
				serverNodeInternalIPs = append(serverNodeInternalIPs, externalIpv6)
			}

			gomega.Expect(len(serverNodeInternalIPs)).To(gomega.BeNumerically(">", 0))
		})

		ginkgo.AfterEach(func() {
			ginkgo.By("Removing external container")
			if len(serverPodName) > 0 {
				deleteClusterExternalContainer(serverPodName)
			}
		})

		// Run queries against the server both with a small (10 bytes + overhead for echo service) and
		// a large (1420 bytes + overhead for echo service) payload.
		// The payload is transmitted to and echoed from the echo service for both HTTP and UDP tests.
		ginkgo.When("tests are run towards the agnhost echo server", func() {
			ginkgo.It("queries to the hostNetworked server pod on another node shall work for TCP", func() {
				for _, size := range []string{"small", "large"} {
					for _, serverNodeIP := range serverNodeInternalIPs {
						ginkgo.By(fmt.Sprintf("Sending TCP %s payload to node IP %s "+
							"and expecting to receive the same payload", size, serverNodeIP))
						cmd := fmt.Sprintf("curl --max-time 10 -g -q -s http://%s:%d/echo?msg=%s",
							serverNodeIP,
							serverPodPort,
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
				clientNodeIPv4, clientNodeIPv6 := getContainerAddressesForNetwork(clientPodNodeName, primaryNetworkName) // we always want to fetch from primary network
				clientnodeIP := clientNodeIPv4
				if IsIPv6Cluster(f.ClientSet) {
					clientnodeIP = clientNodeIPv6
				}
				for _, size := range []string{"small", "large"} {
					for _, serverNodeIP := range serverNodeInternalIPs {
						if size == "large" {
							// Flushing the IP route cache will remove any routes in the cache
							// that are a result of receiving a "need to frag" packet.
							ginkgo.By("Flushing the ip route cache")
							stdout, err := runCommand(containerRuntime, "exec", "-i", serverPodName, "ip", "route", "flush", "cache")
							framework.ExpectNoError(err, "Flushing the ip route cache failed")
							framework.Logf("Flushed cache on %s", serverPodName)
							// List the current IP route cache for informative purposes.
							cmd := fmt.Sprintf("ip route get %s", clientnodeIP)
							stdout, err = runCommand(containerRuntime, "exec", "-i", serverPodName, "ip", "route", "get", clientnodeIP)
							framework.ExpectNoError(err, "Listing IP route cache")
							framework.Logf("%s: %s", cmd, stdout)
						}
						// We expect the following to fail at least once for large payloads and non-hostNetwork
						// endpoints: the first request will fail as we have to receive a "need to frag" ICMP
						// message, subsequent requests then should succeed.
						gomega.Eventually(func() error {
							ginkgo.By(fmt.Sprintf("Sending UDP %s payload to server IP %s "+
								"and expecting to receive the same payload", size, serverNodeIP))
							// Send payload via UDP.
							cmd := fmt.Sprintf("echo 'echo %s' | nc -w2 -u %s %d",
								echoPayloads[size],
								serverNodeIP,
								serverPodPort,
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
								stdout, err = runCommand(containerRuntime, "exec", "-i", serverPodName, "ip", "route", "get", clientnodeIP)
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
						ovnKubeNodePods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
							LabelSelector: "app=ovnkube-node",
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
							_, err := e2ekubectl.RunKubectl(ovnNamespace, "exec", ovnKubeNodePod.Name, "--container", containerName, "--",
								"ip", "route", "flush", "cache")
							framework.ExpectNoError(err, "Flushing the ip route cache failed")
						}
						framework.Logf("Flushing the ip route cache on %s", serverPodName)
						_, err = runCommand(containerRuntime, "exec", "-i", serverPodName, "ip", "route", "flush", "cache")
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
		serverPodPort             = 9899
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
			serverPodName = fmt.Sprintf(echoServerPodNameTemplate, serverPodPort)
			serverPod = e2epod.NewAgnhostPod(f.Namespace.Name, serverPodName, nil, nil, nil,
				"netexec",
				"--http-port", fmt.Sprintf("%d", serverPodPort),
				"--udp-port", fmt.Sprintf("%d", serverPodPort),
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
					ginkgo.By(fmt.Sprintf("Sending TCP large payload to server IP %s "+
						"and expecting to receive the same payload", serverPodIP))
					cmd := fmt.Sprintf("curl --max-time 10 -g -q -s http://%s:%d/echo?msg=%s",
						serverPodIP.IP,
						serverPodPort,
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

var _ = ginkgo.Describe("blocking ICMP needs frag", func() {
	const (
		echoServerPodNameTemplate = "echo-server-pod-%d"
		echoClientPodName         = "echo-client-pod"
		serverPodPort             = 80
		mtu                       = 1500
		serviceName               = "testservice"
		echoServicePortMin        = 31200
		echoServicePortMax        = 31299
	)

	var ipCmd = []string{"ip"}
	var cs clientset.Interface
	var echoMtuRegex = regexp.MustCompile(`expires.*mtu.*1400`)
	f := wrappedTestFramework("icmp-needs-frag")
	cleanupFn := func() {
		ovnKubeNodePods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
			LabelSelector: "app=ovnkube-node",
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
			_, err := e2ekubectl.RunKubectl(ovnNamespace, "exec", ovnKubeNodePod.Name, "--container", containerName, "--",
				"ip", "route", "flush", "cache")
			framework.ExpectNoError(err, "Flushing the ip route cache failed")
		}
	}

	ginkgo.BeforeEach(func() {
		cs = f.ClientSet
		if IsIPv6Cluster(f.ClientSet) {
			ipCmd = []string{"ip", "-6"}
		}
	})

	ginkgo.AfterEach(func() {
		cleanupFn()
	})

	ginkgo.When("a client host networked pod with targets a proxy node nodeport service with ovnk networked backend", func() {
		var serverPod *v1.Pod
		var serverPodNodeName string
		var serverPodName string
		var clientNode v1.Node
		var nodePortNode v1.Node

		var clientPod *v1.Pod
		var clientPodNodeName string
		var nodePort int
		payload := fmt.Sprintf("%01420d", 1)

		ginkgo.BeforeEach(func() {
			ginkgo.By("Selecting 3 schedulable nodes")
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 2))

			ginkgo.By("Selecting nodes for client pod and server host-networked pod")
			serverPodNodeName = nodes.Items[0].Name
			clientPodNodeName = nodes.Items[1].Name
			clientNode = nodes.Items[1]
			nodePortNode = nodes.Items[2]
			nodePort = rand.Intn(echoServicePortMax-echoServicePortMin) + echoServicePortMin

			ginkgo.By("Creating hostNetwork:true (ovnk) client pod")
			clientPod = e2epod.NewAgnhostPod(f.Namespace.Name, echoClientPodName, nil, nil, nil)
			clientPod.Spec.NodeName = clientPodNodeName
			clientPod.Spec.HostNetwork = true
			for k := range clientPod.Spec.Containers {
				if clientPod.Spec.Containers[k].Name == "agnhost-container" {
					clientPod.Spec.Containers[k].Command = []string{
						"sleep",
						"infinity",
					}
				}
				clientPod.Spec.Containers[k].SecurityContext = &v1.SecurityContext{
					Capabilities: &v1.Capabilities{
						Add: []v1.Capability{"NET_ADMIN"},
					},
				}
			}
			e2epod.NewPodClient(f).CreateSync(context.TODO(), clientPod)

			ginkgo.By(fmt.Sprintf("Creating nodeport service with port: %d", nodePort))
			jig := e2eservice.NewTestJig(cs, f.Namespace.Name, serviceName)
			_, err = jig.CreateUDPService(context.TODO(), func(svc *v1.Service) {
				svc.Spec.Type = v1.ServiceTypeNodePort
				svc.Spec.Ports[0].NodePort = int32(nodePort)
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("Creating an ovnk server pod")
			serverPodName = fmt.Sprintf(echoServerPodNameTemplate, serverPodPort)
			serverPod = e2epod.NewAgnhostPod(f.Namespace.Name, serverPodName, nil, nil, nil,
				"netexec",
				"--http-port", fmt.Sprintf("%d", serverPodPort),
				"--udp-port", fmt.Sprintf("%d", serverPodPort),
			)
			serverPod.ObjectMeta.Labels = map[string]string{
				"app": serverPodName,
			}
			serverPod.Spec.NodeName = serverPodNodeName
			serverPod.Labels = jig.Labels
			serverPod = e2epod.NewPodClient(f).CreateSync(context.TODO(), serverPod)
		})

		ginkgo.It("should be able to send large UDP packet and not get a route cache entry", func() {
			// Flushing the IP route cache will remove any routes in the cache
			// that are a result of receiving a "need to frag" packet.
			ginkgo.By("Flushing the ip route cache")
			cmd := append(ipCmd, "route", "flush", "cache")
			flushCmd := append([]string{containerRuntime, "exec", "-i", clientNode.Name}, cmd...)
			stdout, err := runCommand(flushCmd...)
			framework.ExpectNoError(err, "Flushing the ip route cache failed")
			framework.Logf("Flushed cache on %s", clientNode.Name)
			proxyIP := nodePortNode.Status.Addresses[0].Address
			// List the current IP route cache for informative purposes.
			cmd = append(ipCmd, "route", "get", proxyIP)
			fullCmd := append([]string{containerRuntime, "exec", "-i", clientNode.Name}, cmd...)
			stdout, err = runCommand(fullCmd...)
			framework.ExpectNoError(err, "Listing IP route cache")
			framework.Logf("%s: %s", cmd, stdout)

			ginkgo.By(fmt.Sprintf("Sending UDP large payload to server IP %s ", proxyIP))
			// Send payload via UDP.
			udpCmd := fmt.Sprintf("echo 'echo %s' | nc -w2 -u %s %d",
				payload,
				proxyIP,
				nodePort,
			)
			stdout, err = e2epodoutput.RunHostCmd(
				clientPod.Namespace,
				clientPod.Name,
				udpCmd)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(stdout).To(gomega.BeEmpty())

			ginkgo.By(fmt.Sprintf("Making sure that the ip route cache does not contain an MTU route on node: %s", clientNode.Name))
			// Get IP route cache and make sure that it contains an MTU route on the server side.
			stdout, err = runCommand(fullCmd...)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			framework.Logf("Route cache on server node %s", stdout)
			if echoMtuRegex.Match([]byte(stdout)) {
				ginkgo.Fail(fmt.Sprintf("Route cache has PMTUD value for proxy IP: %s, output: %s", proxyIP, stdout))
			}
		})
	})

	ginkgo.When("a client VM pod with 1500 MTU targets a host networked pod", func() {
		var serverPod *v1.Pod
		var serverPodNodeName string
		var serverPodName string
		var serverNode v1.Node
		var clientNode v1.Node

		var clientPod *v1.Pod
		var clientPodNodeName string
		payload := fmt.Sprintf("%01420d", 1)

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

			ginkgo.By(fmt.Sprintf("Creating ovnk client pod on node: %s", clientNode.Name))
			clientPod = e2epod.NewAgnhostPod(f.Namespace.Name, echoClientPodName, nil, nil, nil)
			clientPod.Spec.NodeName = clientPodNodeName
			for k := range clientPod.Spec.Containers {
				if clientPod.Spec.Containers[k].Name == "agnhost-container" {
					clientPod.Spec.Containers[k].Command = []string{
						"sleep",
						"infinity",
					}
				}
				clientPod.Spec.Containers[k].SecurityContext = &v1.SecurityContext{
					Capabilities: &v1.Capabilities{
						Add: []v1.Capability{"NET_ADMIN"},
					},
				}
			}
			clientPod = e2epod.NewPodClient(f).CreateSync(context.TODO(), clientPod)

			getPodIPWithRetry := func(clientSet clientset.Interface, v6 bool, namespace, name string) (net.IP, error) {
				var srcPodIP net.IP
				err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
					pod, err := clientSet.CoreV1().Pods(namespace).Get(context.Background(), name, metav1.GetOptions{})
					if err != nil {
						return false, err
					}
					ips, err := util.DefaultNetworkPodIPs(pod)
					if err != nil {
						return false, err
					}
					srcPodIP, err = util.MatchFirstIPFamily(v6, ips)
					if err != nil {
						return false, err
					}
					return true, nil
				})
				if err != nil || srcPodIP == nil {
					return srcPodIP, fmt.Errorf("unable to fetch pod %s/%s IP after retrying: %v", namespace, name, err)
				}
				return srcPodIP, nil
			}

			var clientPodIP net.IP
			isV6 := IsIPv6Cluster(f.ClientSet)
			clientPodIP, err = getPodIPWithRetry(f.ClientSet, isV6, f.Namespace.Name, clientPod.Name)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(clientPodIP)).To(gomega.BeNumerically(">", 0))
			framework.Logf("Client pod IP is %s", clientPodIP)
			prefix := "/24"
			if isV6 {
				prefix = "/64"
			}
			clientPodCIDR := clientPodIP.String() + prefix
			clientMAC, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", echoClientPodName, "--", "cat", "/sys/class/net/eth0/address")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			clientMAC = strings.TrimSpace(clientMAC)
			dummyMAC := "0a:58:0a:13:13:17"
			externalServer := "8.8.8.8"
			if isV6 {
				externalServer = "2001:4860:4860::8888"
			}
			routeCmd := append(ipCmd, "route", "get", externalServer)
			fullCmd := append([]string{"exec", echoClientPodName, "--"}, routeCmd...)
			routeOutput, err := e2ekubectl.RunKubectl(f.Namespace.Name, fullCmd...)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			parsedRouteOutput := strings.Fields(routeOutput)
			gomega.Expect(len(parsedRouteOutput)).To(gomega.BeNumerically(">", 3))
			gw := parsedRouteOutput[2]
			if isV6 {
				gw = parsedRouteOutput[4]
			}

			ginkgo.By("Setting up a VM with linux bridge and veth in the pod")
			cmds := [][]string{
				{"ip", "link", "add", "name", "br0", "type", "bridge"},
				{"ip", "link", "set", "br0", "up"},
				{"ip", "link", "set", "br0", "mtu", "1500"},
				{"ip", "link", "add", "veth0", "type", "veth", "peer", "name", "veth1"},
				{"ip", "link", "set", "veth0", "mtu", "1500"},
				{"ip", "link", "set", "veth1", "mtu", "1500"},
				{"ip", "link", "set", "eth0", "master", "br0"},
				{"ip", "link", "set", "veth0", "master", "br0"},
				{"ip", "addr", "flush", "dev", "eth0"},
				{"ip", "link", "set", "dev", "veth1", "down"},
				{"ip", "link", "set", "dev", "eth0", "down"},
				{"ip", "link", "set", "dev", "veth1", "address", clientMAC},
				{"ip", "link", "set", "dev", "eth0", "address", dummyMAC},
				{"ip", "link", "set", "dev", "veth1", "up"},
				{"ip", "link", "set", "dev", "eth0", "up"},
				append(ipCmd, "addr", "add", clientPodCIDR, "dev", "veth1"),
				append(ipCmd, "route", "add", "default", "via", gw),
				{"ip", "link", "set", "dev", "veth0", "up"},
			}
			for _, cmd := range cmds {
				fullCmd := []string{"exec", echoClientPodName, "--"}
				fullCmd = append(fullCmd, cmd...)
				stdout, err := e2ekubectl.RunKubectl(f.Namespace.Name, fullCmd...)
				framework.ExpectNoError(err, fmt.Sprintf("setting up linux bridge failed, output: %s", stdout))
			}

			ginkgo.By(fmt.Sprintf("Creating an host networked server pod on node: %s", serverNode.Name))
			serverPodName = fmt.Sprintf(echoServerPodNameTemplate, serverPodPort)
			serverPod = e2epod.NewAgnhostPod(f.Namespace.Name, serverPodName, nil, nil, nil,
				"netexec",
				"--http-port", fmt.Sprintf("%d", serverPodPort),
				"--udp-port", fmt.Sprintf("%d", serverPodPort),
			)
			serverPod.ObjectMeta.Labels = map[string]string{
				"app": serverPodName,
			}
			serverPod.Spec.NodeName = serverPodNodeName
			serverPod.Spec.HostNetwork = true
			serverPod = e2epod.NewPodClient(f).CreateSync(context.TODO(), serverPod)
		})

		ginkgo.It("should be able to send large TCP packet and not get a route cache entry", func() {
			// Flushing the IP route cache will remove any routes in the cache
			// that are a result of receiving a "need to frag" packet.
			ginkgo.By("Flushing the ip route cache")
			flushCmd := append(ipCmd, "route", "flush", "cache")
			fullCmd := append([]string{containerRuntime, "exec", "-i", serverNode.Name}, flushCmd...)
			stdout, err := runCommand(fullCmd...)
			framework.ExpectNoError(err, "Flushing the ip route cache failed")
			framework.Logf("Flushed cache on %s", serverNode.Name)
			clientNodeIP := clientNode.Status.Addresses[0].Address
			serverIP := serverNode.Status.Addresses[0].Address
			// List the current IP route cache for informative purposes.
			routeCmd := append(ipCmd, "route", "get", clientNodeIP)
			fullCmd = append([]string{containerRuntime, "exec", "-i", serverNode.Name}, routeCmd...)
			stdout, err = runCommand(fullCmd...)
			framework.ExpectNoError(err, "Listing IP route cache")
			framework.Logf("%s: %s", fullCmd, stdout)

			curlDest := serverIP
			isV6 := IsIPv6Cluster(f.ClientSet)
			if isV6 {
				curlDest = "[" + curlDest + "]"
			}
			ginkgo.By(fmt.Sprintf("Sending TCP large payload to server IP %s ", serverIP))
			cmd := fmt.Sprintf("curl --max-time 10 -g -q -s http://%s:%d/echo?msg=%s",
				curlDest,
				serverPodPort,
				payload,
			)
			// when the curl happens OVN will generate a needs frag towards the VM, and second curl should work
			stdout, err = e2epodoutput.RunHostCmdWithRetries(
				clientPod.Namespace,
				clientPod.Name,
				cmd,
				framework.Poll,
				60*time.Second)

			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(stdout).NotTo(gomega.BeEmpty())
			ginkgo.By(fmt.Sprintf("Making sure that the ip route cache does not contain an MTU route on node: %s", serverNode.Name))
			// Get IP route cache and make sure that it contains an MTU route on the server side.
			stdout, err = runCommand(fullCmd...)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			framework.Logf("Route cache on server node %s", stdout)
			if echoMtuRegex.Match([]byte(stdout)) {
				ginkgo.Fail(fmt.Sprintf("Route cache has PMTUD value for client node IP: %s, output: %s", clientNodeIP, stdout))
			}
		})
	})

	ginkgo.When("an ovnk pod targets a host networked pod with large UDP", func() {
		var serverPod *v1.Pod
		var serverPodNodeName string
		var serverPodName string
		var serverNode v1.Node
		var clientNode v1.Node

		var clientPod *v1.Pod
		var clientPodNodeName string
		payload := fmt.Sprintf("%01420d", 1)

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

			ginkgo.By(fmt.Sprintf("Creating ovnk client pod on node: %s", clientNode.Name))
			clientPod = e2epod.NewAgnhostPod(f.Namespace.Name, echoClientPodName, nil, nil, nil)
			clientPod.Spec.NodeName = clientPodNodeName
			for k := range clientPod.Spec.Containers {
				if clientPod.Spec.Containers[k].Name == "agnhost-container" {
					clientPod.Spec.Containers[k].Command = []string{
						"sleep",
						"infinity",
					}
				}
				clientPod.Spec.Containers[k].SecurityContext = &v1.SecurityContext{
					Capabilities: &v1.Capabilities{
						Add: []v1.Capability{"NET_ADMIN"},
					},
				}
			}
			clientPod = e2epod.NewPodClient(f).CreateSync(context.TODO(), clientPod)

			ginkgo.By(fmt.Sprintf("Creating an host networked server pod on node: %s", serverNode.Name))
			serverPodName = fmt.Sprintf(echoServerPodNameTemplate, serverPodPort)
			serverPod = e2epod.NewAgnhostPod(f.Namespace.Name, serverPodName, nil, nil, nil,
				"netexec",
				"--http-port", fmt.Sprintf("%d", serverPodPort),
				"--udp-port", fmt.Sprintf("%d", serverPodPort),
			)
			serverPod.ObjectMeta.Labels = map[string]string{
				"app": serverPodName,
			}
			serverPod.Spec.NodeName = serverPodNodeName
			serverPod.Spec.HostNetwork = true
			serverPod = e2epod.NewPodClient(f).CreateSync(context.TODO(), serverPod)

		})

		ginkgo.It("should be able to send large UDP packet and not get a route cache entry", func() {
			// Flushing the IP route cache will remove any routes in the cache
			// that are a result of receiving a "need to frag" packet.
			ginkgo.By("Flushing the ip route cache")
			flushCmd := append(ipCmd, "route", "flush", "cache")
			fullCmd := append([]string{containerRuntime, "exec", "-i", serverNode.Name}, flushCmd...)
			stdout, err := runCommand(fullCmd...)
			framework.ExpectNoError(err, "Flushing the ip route cache failed")
			framework.Logf("Flushed cache on %s", serverNode.Name)
			clientNodeIP := clientNode.Status.Addresses[0].Address
			// List the current IP route cache for informative purposes.
			routeGetCmd := append(ipCmd, "route", "get", clientNodeIP)
			fullCmd = append([]string{containerRuntime, "exec", "-i", serverNode.Name}, routeGetCmd...)
			stdout, err = runCommand(fullCmd...)
			framework.ExpectNoError(err, "Listing IP route cache")
			framework.Logf("%s: %s", fullCmd, stdout)
			serverIP := serverNode.Status.Addresses[0].Address

			ginkgo.By(fmt.Sprintf("Sending UDP large payload to server IP %s ", serverIP))
			// Send payload via UDP.
			cmd := fmt.Sprintf("echo 'echo %s' | nc -w2 -u %s %d",
				payload,
				serverIP,
				serverPodPort,
			)
			stdout, err = e2epodoutput.RunHostCmd(
				clientPod.Namespace,
				clientPod.Name,
				cmd)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(stdout).To(gomega.BeEmpty())
			ginkgo.By(fmt.Sprintf("Making sure that the ip route cache does not contain an MTU route on node: %s", serverNode.Name))
			// Get IP route cache and make sure that it does not contain an MTU cached route on the server side for client node.
			stdout, err = runCommand(fullCmd...)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			framework.Logf("Route cache on server node %s", stdout)
			if echoMtuRegex.Match([]byte(stdout)) {
				ginkgo.Fail(fmt.Sprintf("Route cache has PMTUD value for proxy IP: %s, output: %s", clientNodeIP, stdout))
			}
		})

	})
})
