package e2e

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	rav1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
)

const (
	tcpdumpPodName = "tcpdump-pod-no-overlay"
	serverPodName  = "server-pod-no-overlay"
	clientPodName  = "client-pod-no-overlay"
	containerPort  = 8080
	raName         = "default"
)

var _ = ginkgo.Describe("BGP-no-overlay: connectivity between pods and nodes", func() {
	var f *framework.Framework
	var nodes *corev1.NodeList
	var clientPod, serverPod, tcpdumpPod *corev1.Pod
	var serverService *corev1.Service

	f = wrappedTestFramework("ovn-no-overlay-connectivity")

	ginkgo.BeforeEach(func() {
		if !isNoOverlayEnabled() {
			ginkgo.Skip("Test requires no-overlay mode to be enabled (OVN_NO_OVERLAY_ENABLE=true)")
		}
		var err error
		ginkgo.By("Verifying default RA exists and is accepted")
		exists, accepted, err := checkRAExistsAndAccepted(f, raName)
		framework.ExpectNoError(err, "Failed to check RouteAdvertisements")
		gomega.Expect(exists).To(gomega.BeTrue(), "Default RA should exist when cluster is ready")
		gomega.Expect(accepted).To(gomega.BeTrue(), "Default RA should be accepted when cluster is ready")

		ginkgo.By("Getting ready schedulable nodes")
		nodes, err = e2enode.GetReadySchedulableNodes(context.TODO(), f.ClientSet)
		framework.ExpectNoError(err, "failed to get ready schedulable nodes")
		gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">=", 2),
			"need at least 2 nodes for cross-node connectivity test")
	})

	ginkgo.When("When pods are created on different nodes", func() {
		ginkgo.BeforeEach(func() {
			ginkgo.By("Creating server pod on first node")
			serverPod = e2epod.NewAgnhostPod(f.Namespace.Name, serverPodName, nil, nil, []corev1.ContainerPort{{ContainerPort: containerPort}}, "netexec")
			serverPod.Labels = map[string]string{"app": "no-overlay-server"}
			serverPod.Spec.NodeName = nodes.Items[0].Name
			e2epod.NewPodClient(f).CreateSync(context.TODO(), serverPod)

			ginkgo.By("Creating client pod on second node")
			clientPod = e2epod.NewAgnhostPod(f.Namespace.Name, clientPodName, nil, nil, []corev1.ContainerPort{{ContainerPort: containerPort}}, "netexec")
			clientPod.Spec.NodeName = nodes.Items[1].Name
			e2epod.NewPodClient(f).CreateSync(context.TODO(), clientPod)

			// Wait for pods to be ready and refresh their status
			ginkgo.By("Waiting for server pod to be ready")
			err := e2epod.WaitTimeoutForPodReadyInNamespace(context.TODO(), f.ClientSet, serverPod.Name, f.Namespace.Name, 60*time.Second)
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
			serverService = &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "no-overlay-server-service",
				},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{"app": "no-overlay-server"},
					Ports: []corev1.ServicePort{
						{
							Protocol: corev1.ProtocolTCP,
							Port:     containerPort,
							TargetPort: intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: containerPort,
							},
						},
					},
					Type: corev1.ServiceTypeClusterIP,
				},
			}
			serverService, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.TODO(), serverService, metav1.CreateOptions{})
			framework.ExpectNoError(err, "Failed to create server service")
			framework.Logf("Created service %s with ClusterIP %s", serverService.Name, serverService.Spec.ClusterIP)

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

		ginkgo.It("pod2pod on different node should not capture Geneve overlay packages with no overlay", func() {

			for i, pip := range serverPod.Status.PodIPs {
				destIP := pip.IP
				ginkgo.By(fmt.Sprintf("curl server IP %s", destIP))
				curlCmd := fmt.Sprintf("curl -s -m 2 %s/clientip", net.JoinHostPort(destIP, fmt.Sprint(containerPort)))
				tcpdumpOut, curlOutput := getTcpdumpOnGenevSys(tcpdumpPod, clientPod, curlCmd, destIP)
				gomega.Expect(tcpdumpOut).To(gomega.ContainSubstring("0 packets captured"))
				gomega.Expect(curlOutput).To(gomega.ContainSubstring(clientPod.Status.PodIPs[i].IP))

			}

		})
		ginkgo.It("host2pod on different node should not capture Geneve overlay packages with no overlay", func() {

			for _, pip := range serverPod.Status.PodIPs {
				destIP := pip.IP
				ginkgo.By(fmt.Sprintf("curl server IP %s", destIP))
				curlCmd := fmt.Sprintf("curl -I -s -m 2 %s/clientip", net.JoinHostPort(destIP, fmt.Sprint(containerPort)))
				tcpdumpOut, curlOutput := getTcpdumpOnGenevSys(tcpdumpPod, tcpdumpPod, curlCmd, destIP)
				gomega.Expect(tcpdumpOut).To(gomega.ContainSubstring("0 packets captured"))
				gomega.Expect(curlOutput).To(gomega.ContainSubstring("200 OK"))

			}

		})

		ginkgo.It("pod2service on different node should work with no overlay", func() {
			ginkgo.By("Testing connectivity from client pod to server via service")
			gomega.Expect(serverService.Spec.ClusterIP).NotTo(gomega.BeEmpty(), "Service should have ClusterIP")

			serviceIP := serverService.Spec.ClusterIP
			ginkgo.By(fmt.Sprintf("curl service IP %s", serviceIP))
			curlCmd := fmt.Sprintf("curl -s -m 5 %s/clientip", net.JoinHostPort(serviceIP, fmt.Sprint(containerPort)))

			output, err := e2epodoutput.RunHostCmdWithRetries(clientPod.Namespace, clientPod.Name, curlCmd, framework.Poll, 30*time.Second)
			framework.ExpectNoError(err, "Failed to connect to service from client pod")
			framework.Logf("Service connectivity test output: %s", output)

			// Verify the response contains the client pod IP
			gomega.Expect(output).To(gomega.ContainSubstring(clientPod.Status.PodIPs[0].IP), "Should receive response from service")
		})
		ginkgo.It("host2service on different node should work with no overlay", func() {
			ginkgo.By("Testing connectivity from host to server via service")
			gomega.Expect(serverService.Spec.ClusterIP).NotTo(gomega.BeEmpty(), "Service should have ClusterIP")

			serviceIP := serverService.Spec.ClusterIP
			ginkgo.By(fmt.Sprintf("curl service IP %s from host", serviceIP))

			// Use tcpdump pod which runs on hostNetwork to test host-to-service connectivity
			curlCmd := fmt.Sprintf("curl -s -m 5 %s/clientip", net.JoinHostPort(serviceIP, fmt.Sprint(containerPort)))
			output, err := e2epodoutput.RunHostCmd(tcpdumpPod.Namespace, tcpdumpPod.Name, curlCmd)
			framework.ExpectNoError(err, "Failed to connect to service from host")
			framework.Logf("Host-to-service connectivity test output: %s", output)

			// Verify the response contains a valid IP (either tcpdump pod host IP or node IP)
			gomega.Expect(output).NotTo(gomega.BeEmpty(), "Should receive response from service")
			gomega.Expect(output).To(gomega.MatchRegexp(`\d+\.\d+\.\d+\.\d+:\d+`), "Response should contain IP:port format")
		})

		ginkgo.It("should maintain pod2pod connectivity when default RA is deleted and restored", func() {

			ginkgo.By("Testing pod2pod connectivity with default RA present")
			for _, pip := range serverPod.Status.PodIPs {
				serverIP := pip.IP
				ginkgo.By(fmt.Sprintf("Testing connectivity to server IP %s", serverIP))
				curlCmd := fmt.Sprintf("curl -s -m 5 %s/clientip", net.JoinHostPort(serverIP, fmt.Sprint(containerPort)))
				output, err := e2epodoutput.RunHostCmdWithRetries(clientPod.Namespace, clientPod.Name, curlCmd, framework.Poll, 30*time.Second)
				framework.ExpectNoError(err, "Pod2pod connectivity should work with default RA present")
				gomega.Expect(output).To(gomega.ContainSubstring(clientPod.Status.PodIPs[0].IP), "Should receive response from server")
			}

			ginkgo.By("Saving original RA configuration for restoration")
			originalRA, err := getRA(f, raName)
			framework.ExpectNoError(err, "Failed to get RouteAdvertisements")

			// Ensure RA is restored after test
			defer func() {
				ginkgo.By("Restoring original RouteAdvertisements")
				err := restoreRA(f, originalRA)
				framework.ExpectNoError(err, "Failed to restore RouteAdvertisements")
			}()

			ginkgo.By("Deleting default RA to test connectivity without RA")
			err = deleteRA(f, raName)
			framework.ExpectNoError(err, "Failed to delete RouteAdvertisements")
			time.Sleep(2 * time.Second)

			ginkgo.By("Verifying default RA is deleted")
			exists, _, err := checkRAExistsAndAccepted(f, raName)
			framework.ExpectNoError(err, "Failed to check RouteAdvertisements after deletion")
			gomega.Expect(exists).To(gomega.BeFalse(), "Default RA should be deleted")

			ginkgo.By("Restoring original RouteAdvertisements")
			err = restoreRA(f, originalRA)
			framework.ExpectNoError(err, "Failed to restore RouteAdvertisements")
			ginkgo.By("Testing pod2pod connectivity after RA is deleted")
			for _, pip := range serverPod.Status.PodIPs {
				serverIP := pip.IP
				ginkgo.By(fmt.Sprintf("Testing connectivity to server IP %s after RA deletion", serverIP))
				curlCmd := fmt.Sprintf("curl -s -m 5 %s/clientip", net.JoinHostPort(serverIP, fmt.Sprint(containerPort)))
				output, err := e2epodoutput.RunHostCmdWithRetries(clientPod.Namespace, clientPod.Name, curlCmd, framework.Poll, 30*time.Second)
				framework.ExpectNoError(err, "Pod2pod connectivity should work even without RA")
				gomega.Expect(output).To(gomega.ContainSubstring(clientPod.Status.PodIPs[0].IP), "Should receive response from server")
			}

			framework.Logf("Pod2pod connectivity maintained even when RA is deleted - test passed!")
		})

		ginkgo.It("should maintain pod2pod/pod2service connectivity when ovnpod restarted", func() {
			ginkgo.By("Testing pod2pod and pod2service connectivity before OVN pod restart")
			ginkgo.By("Testing pod2pod connectivity to server IP " + serverPod.Status.PodIPs[0].IP)
			curlCmd := fmt.Sprintf("curl -s -m 5 %s:8080/clientip", serverPod.Status.PodIPs[0].IP)
			output, err := e2epodoutput.RunHostCmd(clientPod.Namespace, clientPod.Name, curlCmd)
			framework.ExpectNoError(err, "Failed to curl server pod")
			gomega.Expect(output).To(gomega.ContainSubstring(clientPod.Status.PodIPs[0].IP), "Should receive response from server pod")

			ginkgo.By("Testing pod2service connectivity via service IP " + serverService.Spec.ClusterIP)
			curlServiceCmd := fmt.Sprintf("curl -s -m 5 %s:8080/clientip", serverService.Spec.ClusterIP)
			output, err = e2epodoutput.RunHostCmd(clientPod.Namespace, clientPod.Name, curlServiceCmd)
			framework.ExpectNoError(err, "Failed to curl service")
			gomega.Expect(output).To(gomega.ContainSubstring(clientPod.Status.PodIPs[0].IP), "Should receive response via service")

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
			ginkgo.By("Testing pod2pod connectivity to server IP " + serverPod.Status.PodIPs[0].IP)
			output, err = e2epodoutput.RunHostCmd(clientPod.Namespace, clientPod.Name, curlCmd)
			framework.ExpectNoError(err, "Failed to curl server pod after OVN restart")
			gomega.Expect(output).To(gomega.ContainSubstring(clientPod.Status.PodIPs[0].IP), "Should receive response from server pod after restart")

			ginkgo.By("Verifying pod2service connectivity after OVN pod restart")
			ginkgo.By("Testing pod2service connectivity via service IP " + serverService.Spec.ClusterIP)
			output, err = e2epodoutput.RunHostCmd(clientPod.Namespace, clientPod.Name, curlServiceCmd)
			framework.ExpectNoError(err, "Failed to curl service after OVN restart")
			gomega.Expect(output).To(gomega.ContainSubstring(clientPod.Status.PodIPs[0].IP), "Should receive response via service after restart")

			framework.Logf("Pod2pod and pod2service connectivity maintained after OVN pod restart - test passed!")
		})
	})
})

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

// checkNoOverlayEvent checks if a specific event exists in the ovn-kubernetes namespace
func checkNoOverlayEvent(f *framework.Framework, eventReason string, eventType string, timeout time.Duration) bool {
	found := false
	err := wait.PollUntilContextTimeout(context.TODO(), 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		events, err := f.ClientSet.CoreV1().Events(deploymentconfig.Get().OVNKubernetesNamespace()).List(ctx, metav1.ListOptions{})
		if err != nil {
			framework.Logf("Failed to list events: %v", err)
			return false, nil
		}

		for _, event := range events.Items {
			if event.Reason == eventReason && event.Type == eventType {
				framework.Logf("Found expected event: Reason=%s Type=%s Message=%s", event.Reason, event.Type, event.Message)
				found = true
				return true, nil
			}
		}
		return false, nil
	})

	if err != nil && !wait.Interrupted(err) {
		framework.Logf("Error while polling for event: %v", err)
	}

	return found
}

// deleteRA deletes a RouteAdvertisements CR
func deleteRA(f *framework.Framework, name string) error {
	raClient, err := raclientset.NewForConfig(f.ClientConfig())
	if err != nil {
		return fmt.Errorf("failed to create RouteAdvertisements client: %w", err)
	}
	err = raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// checkRAExistsAndAccepted checks if a RouteAdvertisements CR exists and has Accepted=True status
// Returns: (exists bool, accepted bool, error)
func checkRAExistsAndAccepted(f *framework.Framework, name string) (bool, bool, error) {
	raClient, err := raclientset.NewForConfig(f.ClientConfig())
	if err != nil {
		return false, false, fmt.Errorf("failed to create RouteAdvertisements client: %w", err)
	}

	ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, false, nil
		}
		return false, false, err
	}

	// RA exists, now check if it's accepted
	accepted := false
	for _, condition := range ra.Status.Conditions {
		if condition.Type == "Accepted" && condition.Status == metav1.ConditionTrue {
			accepted = true
			break
		}
	}

	return true, accepted, nil
}

// getRA retrieves a RouteAdvertisements CR
func getRA(f *framework.Framework, name string) (*rav1.RouteAdvertisements, error) {
	raClient, err := raclientset.NewForConfig(f.ClientConfig())
	if err != nil {
		return nil, fmt.Errorf("failed to create RouteAdvertisements client: %w", err)
	}

	return raClient.K8sV1().RouteAdvertisements().Get(context.TODO(), name, metav1.GetOptions{})
}

// restoreRA restores a RouteAdvertisements CR if it doesn't exist
func restoreRA(f *framework.Framework, ra *rav1.RouteAdvertisements) error {
	raClient, err := raclientset.NewForConfig(f.ClientConfig())
	if err != nil {
		return fmt.Errorf("failed to create RouteAdvertisements client: %w", err)
	}

	// Check if RA already exists
	existingRA, err := raClient.K8sV1().RouteAdvertisements().Get(context.TODO(), ra.Name, metav1.GetOptions{})
	if err == nil {
		// RA already exists, check if we need to update it
		framework.Logf("RouteAdvertisements %s already exists, skipping restoration", ra.Name)

		if len(existingRA.Status.Conditions) > 0 {
			framework.Logf("Existing RA status: %+v", existingRA.Status.Conditions[0])
		}
		return nil
	}

	// If error is not "not found", return the error
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check if RA exists: %w", err)
	}

	// RA doesn't exist, create it with the original spec
	framework.Logf("RouteAdvertisements %s does not exist, restoring it", ra.Name)

	// Clear metadata fields that shouldn't be set on create
	raCopy := ra.DeepCopy()
	raCopy.ResourceVersion = ""
	raCopy.UID = ""
	raCopy.Generation = 0
	raCopy.CreationTimestamp = metav1.Time{}
	raCopy.ManagedFields = nil

	createdRA, err := raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), raCopy, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create RouteAdvertisements: %w", err)
	}

	// Restore the original status
	createdRA.Status = ra.Status
	_, err = raClient.K8sV1().RouteAdvertisements().UpdateStatus(context.TODO(), createdRA, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update RouteAdvertisements status: %w", err)
	}

	// Wait for the restoration to take effect
	time.Sleep(2 * time.Second)

	// Verify the RA was restored successfully
	exists, accepted, err := checkRAExistsAndAccepted(f, ra.Name)
	if err != nil {
		return fmt.Errorf("failed to verify RouteAdvertisements after restoration: %w", err)
	}
	if !exists {
		return fmt.Errorf("RouteAdvertisements %s does not exist after restoration", ra.Name)
	}
	if !accepted {
		return fmt.Errorf("RouteAdvertisements %s is not accepted after restoration", ra.Name)
	}

	framework.Logf("Successfully restored RouteAdvertisements %s (exists=%v, accepted=%v)", ra.Name, exists, accepted)
	return nil
}

var _ = ginkgo.Describe("BGP-no-overlay: dynamic RouteAdvertisements validation", func() {
	var f *framework.Framework

	f = wrappedTestFramework("ovn-no-overlay-dynamic")

	ginkgo.BeforeEach(func() {
		if !isNoOverlayEnabled() {
			ginkgo.Skip("Test requires no-overlay mode to be enabled (OVN_NO_OVERLAY_ENABLE=true)")
		}
	})

	ginkgo.It("should validate and emit warning event when RA is deleted", func() {
		// RA default should be created by default when kind cluster is ready
		raName := "default"

		ginkgo.By("Checking if default RA exists and is accepted")
		exists, accepted, err := checkRAExistsAndAccepted(f, raName)
		framework.ExpectNoError(err, "Failed to check RouteAdvertisements")

		if !exists {
			ginkgo.Skip("Skipping test: default RA does not exist in the cluster")
		}

		if !accepted {
			framework.Logf("Warning: Default RA exists but is not accepted")
		}

		ginkgo.By("Saving original RA configuration for restoration")
		originalRA, err := getRA(f, raName)
		framework.ExpectNoError(err, "Failed to get RouteAdvertisements")

		// Restore RA after test completes
		defer func() {
			ginkgo.By("Restoring original RouteAdvertisements")
			err := restoreRA(f, originalRA)
			framework.ExpectNoError(err, "Failed to restore RouteAdvertisements")
		}()

		ginkgo.By("Deleting RouteAdvertisements")
		err = deleteRA(f, raName)
		framework.ExpectNoError(err, "Failed to delete RouteAdvertisements")

		// Wait for controller to detect deletion
		time.Sleep(2 * time.Second)

		ginkgo.By("Verifying warning event is emitted after deletion")
		found := checkNoOverlayEvent(f, "NoRouteAdvertisements", corev1.EventTypeWarning, 30*time.Second)
		gomega.Expect(found).To(gomega.BeTrue(), "Should emit warning event after RA deletion")
	})

})
