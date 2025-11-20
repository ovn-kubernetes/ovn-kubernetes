package e2e

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
)

var _ = ginkgo.Describe("BGP-no-overlay: connectivity between pods and nodes", func() {
	var f *framework.Framework
	var nodes *corev1.NodeList
	var clientPod, serverPod, tcpdumpPod *corev1.Pod

	f = wrappedTestFramework("ovn-no-overlay-connectivity")

	ginkgo.BeforeEach(func() {
		if !isNoOverlayEnabled() {
			ginkgo.Skip("Test requires no-overlay mode to be enabled (OVN_NO_OVERLAY_ENABLE=true)")
		}

		ginkgo.By("Getting ready schedulable nodes")
		var err error
		nodes, err = e2enode.GetReadySchedulableNodes(context.TODO(), f.ClientSet)
		framework.ExpectNoError(err, "failed to get ready schedulable nodes")
		gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">=", 2),
			"need at least 2 nodes for cross-node connectivity test")
	})

	ginkgo.When("When pods are created on different nodes", func() {
		ginkgo.BeforeEach(func() {
			ginkgo.By("Creating server pod on first node")
			serverPod = e2epod.NewAgnhostPod(f.Namespace.Name, serverPodName, nil, nil, []corev1.ContainerPort{{ContainerPort: containerPort}}, "netexec")
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
			}, 10*time.Second, 1*time.Second).Should(gomega.BeTrue(), "tcpdump should be installed within 10 seconds")

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
