package e2e

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
	utilnet "k8s.io/utils/net"
)

var _ = ginkgo.Describe("No-Overlay: Default network is enabled with no-overlay", feature.NoOverlay, func() {
	f := wrappedTestFramework("no-overlay-default-network")

	ginkgo.When("connectivity tests", func() {
		var clientPod, serverPod, tcpdumpPod *corev1.Pod
		var serverService *corev1.Service
		var nodes *corev1.NodeList

		const (
			tcpdumpPodName = "tcpdump-pod-no-overlay"
			serverPodName  = "server-pod-no-overlay"
			clientPodName  = "client-pod-no-overlay"
		)

		ginkgo.BeforeEach(func() {
			var err error
			ginkgo.By("Selecting nodes")
			nodes, err = e2enode.GetReadySchedulableNodes(context.TODO(), f.ClientSet)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			if len(nodes.Items) < 2 {
				ginkgo.Skip("requires at least 2 Nodes")
			}

			ginkgo.By("Creating server pod on first node")
			serverPod = e2epod.NewAgnhostPod(f.Namespace.Name, serverPodName, nil, nil, []corev1.ContainerPort{{ContainerPort: netexecPort}}, "netexec")
			serverPod.Labels = map[string]string{"app": "no-overlay-server"}
			serverPod.Spec.NodeName = nodes.Items[0].Name
			e2epod.NewPodClient(f).CreateSync(context.TODO(), serverPod)

			ginkgo.By("Creating client pod on second node")
			clientPod = e2epod.NewAgnhostPod(f.Namespace.Name, clientPodName, nil, nil, []corev1.ContainerPort{{ContainerPort: netexecPort}}, "netexec")
			clientPod.Spec.NodeName = nodes.Items[1].Name
			e2epod.NewPodClient(f).CreateSync(context.TODO(), clientPod)

			// Wait for pods to be ready and refresh their status
			ginkgo.By("Waiting for server pod to be ready")
			err = e2epod.WaitTimeoutForPodReadyInNamespace(context.TODO(), f.ClientSet, serverPod.Name, f.Namespace.Name, 60*time.Second)
			framework.ExpectNoError(err, "Server pod failed to become ready")

			ginkgo.By("Waiting for client pod to be ready")
			err = e2epod.WaitTimeoutForPodReadyInNamespace(context.TODO(), f.ClientSet, clientPod.Name, f.Namespace.Name, 60*time.Second)
			framework.ExpectNoError(err, "Client pod failed to become ready")

			// Refresh pod status to get IP addresses
			serverPod, err = e2epod.NewPodClient(f).Get(context.TODO(), serverPod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err, "Failed to get server pod status")

			clientPod, err = e2epod.NewPodClient(f).Get(context.TODO(), clientPod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err, "Failed to get client pod status")

			framework.Logf("Server pod IPs: %v", serverPod.Status.PodIPs)
			framework.Logf("Client pod IPs: %v", clientPod.Status.PodIPs)

			// Verify pods have IP addresses
			gomega.Expect(serverPod.Status.PodIPs).NotTo(gomega.BeEmpty(), "Server pod should have at least one IP address")
			gomega.Expect(clientPod.Status.PodIPs).NotTo(gomega.BeEmpty(), "Client pod should have at least one IP address")

			ginkgo.By("Creating service to select server pod")
			familyPolicy := corev1.IPFamilyPolicyPreferDualStack
			serverService = e2eservice.CreateServiceSpec("no-overlay-server-service", "", false, map[string]string{"app": "no-overlay-server"})
			serverService.Spec.Ports = []corev1.ServicePort{{Protocol: corev1.ProtocolTCP, Port: netexecPort}}
			serverService.Spec.IPFamilyPolicy = &familyPolicy
			serverService, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.TODO(), serverService, metav1.CreateOptions{})
			framework.ExpectNoError(err, "Failed to create server service")
			framework.Logf("Created service %s with ClusterIPs %v", serverService.Name, serverService.Spec.ClusterIPs)

			// Create tcpdump pod as host networked pod to capture traffic on the physical interface
			// In no-overlay mode pod IPs are routed directly and appear unencapsulated on the physical NIC.
			ginkgo.By("Creating tcpdump pod")
			tcpdumpPod, err = createPod(f, tcpdumpPodName, nodes.Items[1].Name, f.Namespace.Name,
				[]string{"sh", "-c", "sleep 20000"},
				map[string]string{},
				func(p *corev1.Pod) {
					p.Spec.HostNetwork = true
					p.Spec.Containers[0].Image = images.Netshoot()
					p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
						Capabilities: &corev1.Capabilities{
							Add: []corev1.Capability{"NET_RAW", "NET_ADMIN"},
						},
					}
				})
			framework.ExpectNoError(err, "Failed to create tcpdump pod")
			framework.Logf("tcpdumpPod pod IPs: %v", tcpdumpPod.Status.PodIPs)
		})

		ginkgo.It("should maintain pod2pod/pod2service/host2pod/host2service connectivity without overlay before and after ovnkube-node pod restarted", func() {
			// test traffic for pod2pod, host2pod, pod2service, host2service and verify no overlay traffic is captured by tcpdump
			ginkgo.By("Testing pod2pod connectivity without overlay on different node before ovnkube-node pod restart")
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, nil, clientPod, tcpdumpPod)

			ginkgo.By("Testing host2pod connectivity without overlay on different node before ovnkube-node pod restart")
			// here use tcpdumpPod as the client (since it's host networked)
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, nil, tcpdumpPod, tcpdumpPod)

			ginkgo.By("Testing pod2service connectivity without overlay via service IPs before ovnkube-node pod restart")
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, serverService.Spec.ClusterIPs, clientPod, tcpdumpPod)

			ginkgo.By("Testing host2service connectivity without overlay via service IPs before ovnkube-node pod restart")
			// here use tcpdumpPod as the client (since it's host networked)
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, serverService.Spec.ClusterIPs, tcpdumpPod, tcpdumpPod)

			ginkgo.By("Getting ovnkube-node pod on worker node")
			ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
			ovnkubeNodePodList, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
				LabelSelector: "app=ovnkube-node",
				FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodes.Items[0].Name),
			})
			framework.ExpectNoError(err, "Failed to list ovnkube-node pods")
			gomega.Expect(ovnkubeNodePodList.Items).NotTo(gomega.BeEmpty(), "Should find ovnkube-node pod")
			ovnkubeNodePod := &ovnkubeNodePodList.Items[0]
			framework.Logf("Found ovnkube-node pod: %s on node %s", ovnkubeNodePod.Name, nodes.Items[0].Name)

			ginkgo.By("Deleting ovnkube-node pod to trigger restart")
			err = f.ClientSet.CoreV1().Pods(ovnNamespace).Delete(context.TODO(), ovnkubeNodePod.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "Failed to delete ovnkube-node pod")

			ginkgo.By("Waiting for new ovnkube-node pod to be ready")
			gomega.Eventually(func() bool {
				newOvnkubeNodePodList, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
					LabelSelector: "app=ovnkube-node",
					FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodes.Items[0].Name),
				})
				if err != nil {
					framework.Logf("Failed to list ovnkube-node pods: %v", err)
					return false
				}
				if len(newOvnkubeNodePodList.Items) == 0 {
					framework.Logf("No ovnkube-node pod found yet")
					return false
				}
				newOvnkubeNodePod := &newOvnkubeNodePodList.Items[0]
				// Check if it's a new pod (different UID)
				if newOvnkubeNodePod.UID == ovnkubeNodePod.UID {
					framework.Logf("Still the old pod, waiting for deletion to complete")
					return false
				}
				// Check if all containers are ready
				for _, condition := range newOvnkubeNodePod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						framework.Logf("New ovnkube-node pod %s is ready", newOvnkubeNodePod.Name)
						return true
					}
				}
				framework.Logf("New ovnkube-node pod %s is not ready yet", newOvnkubeNodePod.Name)
				return false
			}, 120*time.Second, 2*time.Second).Should(gomega.BeTrue(), "New ovnkube-node pod should be ready within 120 seconds")

			ginkgo.By("Verifying pod2pod connectivity after ovnkube-node pod restart")
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, nil, clientPod, tcpdumpPod)

			ginkgo.By("Verifying host2pod connectivity after ovnkube-node pod restart")
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, nil, tcpdumpPod, tcpdumpPod)

			ginkgo.By("Verifying pod2service connectivity after ovnkube-node pod restart")
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, serverService.Spec.ClusterIPs, clientPod, tcpdumpPod)

			ginkgo.By("Verifying host2service connectivity after ovnkube-node pod restart")
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, serverService.Spec.ClusterIPs, tcpdumpPod, tcpdumpPod)

			framework.Logf("Pod2pod and pod2service connectivity maintained after ovnkube-node pod restart - test passed!")
		})

	})

})

// getTcpdumpOnPhysicalIface starts tcpdump on the physical NIC (from deploymentconfig) filtered
// by pod IP, runs curlCmd, and returns the tcpdump output and curl output.
// In no-overlay mode pod IPs are routed directly and appear unencapsulated on the physical NIC.
// enp2s0 Out IP 10.131.0.123.49084 > 10.128.3.208.8080:
// In overlay mode pod IPs are going through genev_sys_6081
// genev_sys_6081 Out IP 10.131.0.23.39410 > 10.129.2.15.8080:
func getTcpdumpOnPhysicalIface(tcpdumpPod *corev1.Pod, clientPod *corev1.Pod, curlCmd string, podIP string) (string, string, error) {
	iface := deploymentconfig.Get().PrimaryInterfaceName()
	ginkgo.By(fmt.Sprintf("start tcpdump on physical interface %s to capture traffic", iface))
	// Save PID then verify process is still alive after a brief delay, so an immediate
	// tcpdump crash causes a non-zero exit code rather than silently succeeding.
	startCmd := fmt.Sprintf("sh -c 'rm -f /tmp/tcpdump.log /tmp/tcpdump.pid; tcpdump -ni %s tcp and host %s -n -s 0 -l > /tmp/tcpdump.log 2>&1 & PID=$!; echo $PID > /tmp/tcpdump.pid; sleep 0.2; kill -0 $PID'", iface, podIP)
	_, tcpdumpErr := e2epodoutput.RunHostCmdWithRetries(
		tcpdumpPod.Namespace,
		tcpdumpPod.Name, startCmd,
		framework.Poll,
		10*time.Second)
	framework.ExpectNoError(tcpdumpErr, "tcpdump failed to start on interface %s", iface)
	// Give tcpdump time to initialize its capture buffer before sending traffic
	// Without this sleep, we hit a race where packets match the filter but aren't
	// captured because tcpdump hasn't fully initialized yet.
	time.Sleep(500 * time.Millisecond)
	ginkgo.By("Generating tcp traffic")
	framework.Logf("Testing connectivity with command %q", curlCmd)
	curlOutput, curlErr := e2epodoutput.RunHostCmdWithRetries(
		clientPod.Namespace,
		clientPod.Name,
		curlCmd,
		framework.Poll,
		10*time.Second)

	// Always stop tcpdump and collect output before returning, so the background
	// process is not left running even when curl failed.
	collectCmd := "sh -c 'kill -INT $(cat /tmp/tcpdump.pid) >/dev/null 2>&1 || true; sleep 1; cat /tmp/tcpdump.log'"
	tcpdumpOut, err := e2epodoutput.RunHostCmdWithRetries(tcpdumpPod.Namespace, tcpdumpPod.Name, collectCmd, framework.Poll, 10*time.Second)
	framework.ExpectNoError(err, "Failed to collect tcpdump output")
	framework.Logf("tcpdump output:\n%s", tcpdumpOut)
	framework.Logf("curl output:\n%s", curlOutput)
	return tcpdumpOut, curlOutput, curlErr
}

// checkConnectivityWithoutOverlay verifies no-overlay connectivity from clientPod and asserts
// that traffic is visible unencapsulated on the physical NIC via tcpdump.
//
// serverPodIPs is always the real server pod IPs; it is used both as the curl destination
// (pod-to-pod) and as the tcpdump filter (the actual endpoint seen on the wire).
// When serviceClusterIPs is non-nil the curl destination switches to those VIPs instead
// (pod-to-service), while tcpdump still filters on the server pod IP backing the VIP.
func checkConnectivityWithoutOverlay(serverPodIPs []corev1.PodIP, serviceClusterIPs []string, clientPod, tcpdumpPod *corev1.Pod) {
	// destIPs: what to curl to.
	var destIPs []string
	if len(serviceClusterIPs) > 0 {
		destIPs = serviceClusterIPs
	} else {
		for _, pip := range serverPodIPs {
			destIPs = append(destIPs, pip.IP)
		}
	}

	for _, destIP := range destIPs {
		// tcpdump always filters on the server pod IP.
		// For pod-to-pod this equals destIP; for pod-to-service it is the pod backing the VIP.
		var filterIP string
		for _, pip := range serverPodIPs {
			if utilnet.IsIPv6String(pip.IP) == utilnet.IsIPv6String(destIP) {
				filterIP = pip.IP
				break
			}
		}
		gomega.Expect(filterIP).NotTo(gomega.BeEmpty(), "Could not find server pod IP matching family of %s", destIP)

		ginkgo.By(fmt.Sprintf("curl %s", destIP))
		curlCmd := fmt.Sprintf("curl -s -m 2 %s/clientip", net.JoinHostPort(destIP, fmt.Sprint(netexecPort)))
		// In no-overlay mode pod IPs are routed directly; capturing on the physical NIC with a
		// pod IP filter proves the traffic is unencapsulated (overlay would hide it inside Geneve).
		tcpdumpOut, _, curlErr := getTcpdumpOnPhysicalIface(tcpdumpPod, clientPod, curlCmd, filterIP)
		framework.ExpectNoError(curlErr, "curl to %s failed", destIP)
		gomega.Expect(tcpdumpOut).To(gomega.MatchRegexp(`(?m)^[1-9][0-9]* packets captured`),
			"Should capture unencapsulated pod traffic on the physical interface")
	}
}
