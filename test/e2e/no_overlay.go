package e2e

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	utilnet "k8s.io/utils/net"
)

var _ = ginkgo.Describe("No-Overlay: Default network is enabled with no-overlay", feature.NoOverlay, func() {
	f := wrappedTestFramework("no-overlay-default-network")

	ginkgo.It("should have pod subnet routes in GR but not in ovn_cluster_router for no-overlay", func() {
		if !isNoOverlayEnabled() {
			ginkgo.Skip("Test requires no-overlay mode to be enabled")
		}

		ginkgo.By("Getting all nodes and their pod subnets")
		nodes, err := f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
		framework.ExpectNoError(err, "Failed to list nodes")
		gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">=", 2), "Test requires at least 2 nodes")

		ginkgo.By("Verifying pod subnet routes are in GR but not in ovn_cluster_router for each node")
		for i, node := range nodes.Items {
			// Get pod CIDR for this node from OVN annotation (this is what OVN actually uses)
			podCIDRs, err := getNodePodSubnets(&node)
			framework.ExpectNoError(err, "Failed to get node pod subnets for %s", node.Name)
			gomega.Expect(len(podCIDRs)).To(gomega.BeNumerically(">", 0), "Node %s should have pod CIDRs", node.Name)
			framework.Logf("Checking node %s (pod CIDRs: %v)", node.Name, podCIDRs)
			// For each node, check that OTHER nodes' pod subnets appear in its GR
			for j, otherNode := range nodes.Items {
				if i == j {
					// Skip checking the node's own subnet in its own GR
					continue
				}
				otherPodCIDRs, err := getNodePodSubnets(&otherNode)
				framework.ExpectNoError(err, "Failed to get node pod subnets for %s", otherNode.Name)
				gomega.Expect(len(otherPodCIDRs)).To(gomega.BeNumerically(">", 0), "Node %s should have pod CIDRs", otherNode.Name)

				for _, otherPodCIDR := range otherPodCIDRs {
					framework.Logf("Verifying %s's pod subnet %s appears in %s's GR but not in ovn_cluster_router",
						otherNode.Name, otherPodCIDR, node.Name)
					// - Routes to remote pod subnets SHOULD exist in each node's GR (imported from BGP)
					// - Routes to remote pod subnets should NOT exist in ovn_cluster_router (BGP handles routing)
					err := checkOVNLogicalRouterRoutes(otherPodCIDR, node.Name, true, false)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
			}
		}
	})
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
			if !isNoOverlayEnabled() {
				ginkgo.Skip("Test requires no-overlay mode to be enabled (OVN_NO_OVERLAY_ENABLE=true)")
			}

			var err error
			ginkgo.By("Selecting nodes")
			nodes, err = f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 2))

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
			gomega.Expect(len(serverPod.Status.PodIPs)).To(gomega.BeNumerically(">", 0), "Server pod should have at least one IP address")
			gomega.Expect(len(clientPod.Status.PodIPs)).To(gomega.BeNumerically(">", 0), "Client pod should have at least one IP address")

			ginkgo.By("Creating service to select server pod")
			familyPolicy := corev1.IPFamilyPolicyPreferDualStack
			serverService = &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "no-overlay-server-service",
				},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{"app": "no-overlay-server"},
					Ports: []corev1.ServicePort{
						{
							Protocol: corev1.ProtocolTCP,
							Port:     netexecPort,
							TargetPort: intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: netexecPort,
							},
						},
					},
					Type:           corev1.ServiceTypeClusterIP,
					IPFamilyPolicy: &familyPolicy,
				},
			}
			serverService, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.TODO(), serverService, metav1.CreateOptions{})
			framework.ExpectNoError(err, "Failed to create server service")
			framework.Logf("Created service %s with ClusterIPs %v", serverService.Name, serverService.Spec.ClusterIPs)

			ginkgo.By("Creating tcpdump pod")
			tcpdumpPod, err = createPod(f, tcpdumpPodName, nodes.Items[1].Name, f.Namespace.Name, []string{"bash", "-c", "apk update; apk add tcpdump; sleep 20000"}, map[string]string{}, func(p *corev1.Pod) {
				p.Spec.HostNetwork = true
			})
			framework.ExpectNoError(err)
			framework.Logf("tcpdumpPod pod IPs: %v", tcpdumpPod.Status.PodIPs)

			ginkgo.By("Verifying tcpdump is installed successfully")
			gomega.Eventually(func() bool {
				checkCmd := "which tcpdump"
				output, err := e2epodoutput.RunHostCmd(tcpdumpPod.Namespace, tcpdumpPod.Name, checkCmd)
				if err != nil {
					framework.Logf("tcpdump check failed: %v", err)
					return false
				}
				framework.Logf("tcpdump check output: %s", output)
				return output != "" && err == nil
			}, 30*time.Second, 1*time.Second).Should(gomega.BeTrue(), "tcpdump should be installed within 30 seconds")
		})

		ginkgo.It("should maintain pod2pod/pod2service/host2pod/host2service connectivity without overlay before and after ovnpod restarted", func() {
			// test traffic for pod2pod, host2pod, pod2service, host2service and verify no overlay traffic is captured by tcpdump
			ginkgo.By("Testing pod2pod connectivity without overlay on different node before OVN pod restart")
			checkPod2PodConnectivityWithoutOverlay(serverPod, clientPod, tcpdumpPod)

			ginkgo.By("Testing host2pod connectivity without overlay on different node before OVN pod restart")
			// here use tcpdumpPod as the client (since it's host networked)
			checkPod2PodConnectivityWithoutOverlay(serverPod, tcpdumpPod, tcpdumpPod)

			ginkgo.By("Testing pod2service connectivity without overlay via service IPs before OVN pod restart")
			checkPod2ServiceConnectivityWithoutOverlay(serverService, serverPod, clientPod, tcpdumpPod)

			ginkgo.By("Testing host2service connectivity without overlay via service IPs before OVN pod restart")
			// here use tcpdumpPod as the client (since it's host networked)
			checkPod2ServiceConnectivityWithoutOverlay(serverService, serverPod, tcpdumpPod, tcpdumpPod)

			ginkgo.By("Getting ovnkube-node pod on worker node")
			ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
			ovnPodList, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
				LabelSelector: "app=ovnkube-node",
				FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodes.Items[0].Name),
			})
			framework.ExpectNoError(err, "Failed to list ovnkube-node pods")
			gomega.Expect(ovnPodList.Items).NotTo(gomega.BeEmpty(), "Should find ovnkube-node pod")
			ovnPod := &ovnPodList.Items[0]
			framework.Logf("Found ovnkube-node pod: %s on node %s", ovnPod.Name, nodes.Items[0].Name)

			ginkgo.By("Deleting ovnkube-node pod to trigger restart")
			err = f.ClientSet.CoreV1().Pods(ovnNamespace).Delete(context.TODO(), ovnPod.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "Failed to delete ovnkube-node pod")

			ginkgo.By("Waiting for new ovnkube-node pod to be ready")
			gomega.Eventually(func() bool {
				newOvnPodList, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
					LabelSelector: "app=ovnkube-node",
					FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodes.Items[0].Name),
				})
				if err != nil {
					framework.Logf("Failed to list ovnkube-node pods: %v", err)
					return false
				}
				if len(newOvnPodList.Items) == 0 {
					framework.Logf("No ovnkube-node pod found yet")
					return false
				}
				newOvnPod := &newOvnPodList.Items[0]
				// Check if it's a new pod (different UID)
				if newOvnPod.UID == ovnPod.UID {
					framework.Logf("Still the old pod, waiting for deletion to complete")
					return false
				}
				// Check if all containers are ready
				for _, condition := range newOvnPod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						framework.Logf("New ovnkube-node pod %s is ready", newOvnPod.Name)
						return true
					}
				}
				framework.Logf("New ovnkube-node pod %s is not ready yet", newOvnPod.Name)
				return false
			}, 120*time.Second, 2*time.Second).Should(gomega.BeTrue(), "New ovnkube-node pod should be ready within 120 seconds")

			ginkgo.By("Verifying pod2pod connectivity after OVN pod restart")
			checkPod2PodConnectivityWithoutOverlay(serverPod, clientPod, tcpdumpPod)

			ginkgo.By("Verifying host2pod connectivity after OVN pod restart")
			checkPod2PodConnectivityWithoutOverlay(serverPod, tcpdumpPod, tcpdumpPod)

			ginkgo.By("Verifying pod2service connectivity after OVN pod restart")
			checkPod2ServiceConnectivityWithoutOverlay(serverService, serverPod, clientPod, tcpdumpPod)

			ginkgo.By("Verifying host2service connectivity after OVN pod restart")
			checkPod2ServiceConnectivityWithoutOverlay(serverService, serverPod, tcpdumpPod, tcpdumpPod)

			framework.Logf("Pod2pod and pod2service connectivity maintained after OVN pod restart - test passed!")
		})

		ginkgo.It("should reconcile RA CR if manually deleted in managed mode", func() {
			if !isManagedRoutingEnabled() {
				ginkgo.Skip("Test requires managed routing mode to be enabled")
			}

			raName := "ovnk-default-network-advertisement"

			ginkgo.By("Verifying auto-created RA exists before deletion")
			_, err := e2ekubectl.RunKubectl("", "get", "ra", raName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Auto-created RA should exist in managed routing mode")

			ginkgo.By("Testing connectivity before RA deletion")
			checkPod2PodConnectivityWithoutOverlay(serverPod, clientPod, tcpdumpPod)
			checkPod2ServiceConnectivityWithoutOverlay(serverService, serverPod, clientPod, tcpdumpPod)

			ginkgo.By("Manually deleting the auto-created RA CR")
			_, err = e2ekubectl.RunKubectl("", "delete", "ra", raName, "--ignore-not-found=true")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("Verifying RA is auto-recreated by the noOverlayController")
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl("", "get", "ra", raName)
				return err
			}, 30*time.Second, 1*time.Second).Should(gomega.Succeed(), "Auto-created RA should be recreated by the system within 30 seconds")

			ginkgo.By("Verifying RA has correct labels after recreation")
			raJSON, err := e2ekubectl.RunKubectl("", "get", "ra", raName, "-o", "json")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(raJSON).To(gomega.ContainSubstring("k8s.ovn.org/managed-internal-fabric"), "RA should have managed internal fabric label")

			ginkgo.By("Verifying pod2pod connectivity works after RA recreation")
			checkPod2PodConnectivityWithoutOverlay(serverPod, clientPod, tcpdumpPod)

			ginkgo.By("Verifying pod2service connectivity works after RA recreation")
			checkPod2ServiceConnectivityWithoutOverlay(serverService, serverPod, clientPod, tcpdumpPod)

			ginkgo.By("Verifying host2pod connectivity works after RA recreation")
			checkPod2PodConnectivityWithoutOverlay(serverPod, tcpdumpPod, tcpdumpPod)

			ginkgo.By("Verifying host2service connectivity works after RA recreation")
			checkPod2ServiceConnectivityWithoutOverlay(serverService, serverPod, tcpdumpPod, tcpdumpPod)

			framework.Logf("RA reconciliation test passed - auto-created RA was successfully recreated and connectivity maintained!")
		})
	})

})

// getTcpdumpOnGenevSys starts tcpdump on genev_sys_6081 interface, runs a curl command, and returns the tcpdump output and curl output.
func getTcpdumpOnGenevSys(tcpdumpPod *corev1.Pod, clientPod *corev1.Pod, curlCmd string, serverPodIP string) (string, string) {
	ginkgo.By("start tcpdump on genev_sys_6081 interface to capture traffic")
	startCmd := fmt.Sprintf("sh -lc 'rm -f /tmp/tcpdump.log /tmp/tcpdump.pid; tcpdump -ni genev_sys_6081 tcp and host %s -n -vv -s 0 -l > /tmp/tcpdump.log 2>&1 & echo $! > /tmp/tcpdump.pid'", serverPodIP)
	_, tcpdumpErr := e2epodoutput.RunHostCmdWithRetries(
		tcpdumpPod.Namespace,
		tcpdumpPod.Name, startCmd,
		framework.Poll,
		10*time.Second)
	framework.ExpectNoError(tcpdumpErr, "run tcpdump failed on genev_sys_6081 interface")
	// Wait 2 seconds to let the tcpdump ready for capturing traffic
	time.Sleep(2 * time.Second)

	ginkgo.By("Generating tcp traffic")
	framework.Logf("Testing connectivity with command %q", curlCmd)
	curlOutput, curlErr := e2epodoutput.RunHostCmdWithRetries(
		clientPod.Namespace,
		clientPod.Name,
		curlCmd,
		framework.Poll,
		10*time.Second)
	framework.ExpectNoError(curlErr, "Curl server from client failed")
	framework.Logf("curl output:\n%s", curlOutput)

	collectCmd := "sh -lc 'kill -INT $(cat /tmp/tcpdump.pid) >/dev/null 2>&1 || true; sleep 1; cat /tmp/tcpdump.log'"
	tcpdumpOut, _ := e2epodoutput.RunHostCmdWithRetries(tcpdumpPod.Namespace, tcpdumpPod.Name, collectCmd, framework.Poll, 10*time.Second)
	framework.Logf("tcpdump output:\n%s", tcpdumpOut)
	return tcpdumpOut, curlOutput
}

// checkPod2PodConnectivityWithoutOverlay checks that the client pod can connect to the server pod without overlay
// and the client IP is received in the response without any overlay traffic.
func checkPod2PodConnectivityWithoutOverlay(serverPod, clientPod, tcpdumpPod *corev1.Pod) {
	for _, pip := range serverPod.Status.PodIPs {
		destIP := pip.IP
		ginkgo.By(fmt.Sprintf("curl server IP %s", destIP))
		curlCmd := fmt.Sprintf("curl -s -m 2 %s/clientip", net.JoinHostPort(destIP, fmt.Sprint(netexecPort)))
		tcpdumpOut, curlOutput := getTcpdumpOnGenevSys(tcpdumpPod, clientPod, curlCmd, destIP)
		gomega.Expect(tcpdumpOut).To(gomega.ContainSubstring("0 packets captured"), "Should not capture Geneve packets for no-overlay")

		var clientIP string
		isIPv6 := utilnet.IsIPv6String(destIP)
		for _, pip := range clientPod.Status.PodIPs {
			if utilnet.IsIPv6String(pip.IP) == isIPv6 {
				clientIP = pip.IP
				break
			}
		}
		gomega.Expect(clientIP).NotTo(gomega.BeEmpty())
		gomega.Expect(curlOutput).To(gomega.ContainSubstring(clientIP), "Should receive client IP %s", clientIP)

	}
}

// checkPod2ServiceConnectivityWithoutOverlay checks that the client pod can connect to the server service without overlay
func checkPod2ServiceConnectivityWithoutOverlay(serverService *corev1.Service, serverPod, clientPod, tcpdumpPod *corev1.Pod) {
	for _, serviceIP := range serverService.Spec.ClusterIPs {
		ginkgo.By(fmt.Sprintf("curl service IP %s", serviceIP))
		curlCmd := fmt.Sprintf("curl -I -s -m 2 %s/clientip", net.JoinHostPort(serviceIP, fmt.Sprint(netexecPort)))

		// Determine server pod IP based on service IP family for filtering
		isIPv6 := utilnet.IsIPv6String(serviceIP)
		var serverIP string
		for _, pip := range serverPod.Status.PodIPs {
			if utilnet.IsIPv6String(pip.IP) == isIPv6 {
				serverIP = pip.IP
				break
			}
		}
		gomega.Expect(serverIP).NotTo(gomega.BeEmpty(), "Could not find matching server pod IP family")

		// Use getTcpdumpOnGenevSys to verify no overlay traffic (filtering by backend server IP)
		tcpdumpOut, output := getTcpdumpOnGenevSys(tcpdumpPod, clientPod, curlCmd, serverIP)
		gomega.Expect(tcpdumpOut).To(gomega.ContainSubstring("0 packets captured"), "Should not capture Geneve packets for no-overlay")

		framework.Logf("Service connectivity test output for %s: %s", serviceIP, output)
		// Verify the response contains 200 OK
		gomega.Expect(output).To(gomega.ContainSubstring("200 OK"), fmt.Sprintf("Should receive 200 OK from service %s", serviceIP))
	}
}

// checkOVNLogicalRouterRoutes verifies that routes exist or don't exist in OVN logical routers.
func checkOVNLogicalRouterRoutes(podCIDR, nodeName string, shouldExistInGR, shouldExistInClusterRouter bool) error {
	ovnKubeNamespace := deploymentconfig.Get().OVNKubernetesNamespace()

	// Get the ovnkube-node pod for this node
	ovnkubeNodePodName, err := e2ekubectl.RunKubectl(ovnKubeNamespace, "get", "pods", "-l", "app=ovnkube-node", "--field-selector", fmt.Sprintf("spec.nodeName=%s", nodeName), "-o=jsonpath='{.items[0].metadata.name}'")
	if err != nil {
		return fmt.Errorf("failed to get ovnkube-node pod for node %s: %w", nodeName, err)
	}

	ovnkubeNodePodName = strings.Trim(ovnkubeNodePodName, "'")
	if ovnkubeNodePodName == "" {
		return fmt.Errorf("no ovnkube-node pod found for node %s", nodeName)
	}
	framework.Logf("Using ovnkube-node pod: %s on node %s", ovnkubeNodePodName, nodeName)

	// Check GR (Gateway Router)
	grName := "GR_" + nodeName
	grRoutes, err := e2ekubectl.RunKubectl(ovnKubeNamespace, "exec", ovnkubeNodePodName, "--", "ovn-nbctl", "--no-leader-only", "lr-route-list", grName)
	if err != nil {
		return fmt.Errorf("failed to list routes in %s: %w", grName, err)
	}

	grContainsRoute := strings.Contains(grRoutes, podCIDR)
	if shouldExistInGR && !grContainsRoute {
		return fmt.Errorf("route %s should exist in %s but was not found. Routes:\n%s", podCIDR, grName, grRoutes)
	}
	if !shouldExistInGR && grContainsRoute {
		return fmt.Errorf("route %s should NOT exist in %s but was found. Routes:\n%s", podCIDR, grName, grRoutes)
	}
	framework.Logf("✓ Route %s in %s: expected=%v, found=%v", podCIDR, grName, shouldExistInGR, grContainsRoute)

	// Check ovn_cluster_router
	clusterRouterRoutes, err := e2ekubectl.RunKubectl(ovnKubeNamespace, "exec", ovnkubeNodePodName, "--", "ovn-nbctl", "--no-leader-only", "lr-route-list", "ovn_cluster_router")
	if err != nil {
		return fmt.Errorf("failed to list routes in ovn_cluster_router: %w", err)
	}

	clusterRouterContainsRoute := strings.Contains(clusterRouterRoutes, podCIDR)
	if shouldExistInClusterRouter && !clusterRouterContainsRoute {
		return fmt.Errorf("route %s should exist in ovn_cluster_router but was not found. Routes:\n%s", podCIDR, clusterRouterRoutes)
	}
	if !shouldExistInClusterRouter && clusterRouterContainsRoute {
		return fmt.Errorf("route %s should NOT exist in ovn_cluster_router but was found. Routes:\n%s", podCIDR, clusterRouterRoutes)
	}
	framework.Logf("✓ Route %s in ovn_cluster_router: expected=%v, found=%v", podCIDR, shouldExistInClusterRouter, clusterRouterContainsRoute)

	return nil
}
