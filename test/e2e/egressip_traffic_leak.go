// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	utilnet "k8s.io/utils/net"

	ovnutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
)

var _ = ginkgo.Describe("[secondary-host-eip] Egress IP Traffic Leak Prevention For Existing Pods with Pcap Logs", func() {
	const (
		egressIPName         = "egressip-existing-pods-test"
		egressIPYaml         = "/tmp/egressip-existing-pods.yaml"
		monitorPodName       = "traffic-monitor-existing"
		podNamePrefix        = "existing-pod"
		numTestPods          = 20 // Create multiple pods to stress reconciliation
		retryInterval        = 1 * time.Second
		retryTimeout         = 120 * time.Second
		monitorCheckInterval = 500 * time.Millisecond
		monitorTimeout       = 60 * time.Second
	)

	f := wrappedTestFramework("egressip-existing-pods")

	// Helper function to get pod IP with retry
	getPodIPWithRetry := func(clientSet clientset.Interface, v6 bool, namespace, name string) (net.IP, error) {
		var srcPodIP net.IP
		err := wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, func(ctx context.Context) (bool, error) {
			pod, err := clientSet.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			ips, err := ovnutil.DefaultNetworkPodIPs(pod)
			if err != nil {
				return false, err
			}
			srcPodIP, err = ovnutil.MatchFirstIPFamily(v6, ips)
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

	// Helper type for egressIP status
	type egressIPStatus struct {
		Node     string `json:"node"`
		EgressIP string `json:"egressIP"`
	}

	type egressIPObject struct {
		Status struct {
			Items []egressIPStatus `json:"items"`
		} `json:"status"`
	}

	type egressIPObjects struct {
		Items []egressIPObject `json:"items"`
	}

	// Helper to get egress IP status items
	getEgressIPStatusItems := func() []egressIPStatus {
		egressIPObjs := &egressIPObjects{}
		egressIPStdout, err := e2ekubectl.RunKubectl("default", "get", "egressips", "-o", "json")
		if err != nil {
			framework.Failf("Error: failed to get the EgressIP object, err: %v", err)
		}
		err = json.Unmarshal([]byte(egressIPStdout), egressIPObjs)
		if err != nil {
			framework.Failf("Error: failed to unmarshal EgressIP object, err: %v", err)
		}
		if len(egressIPObjs.Items) == 0 {
			framework.Failf("Error: no EgressIP objects found")
		}
		return egressIPObjs.Items[0].Status.Items
	}

	// Helper to verify egress IP status length
	verifyEgressIPStatusLengthEquals := func(statusLength int) []egressIPStatus {
		var statuses []egressIPStatus
		err := wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, func(ctx context.Context) (bool, error) {
			statuses = getEgressIPStatusItems()
			return len(statuses) == statusLength, nil
		})
		if err != nil {
			framework.Failf("Error: expected to have %v egress IP assignment, got: %v", statusLength, len(statuses))
		}
		return statuses
	}

	var (
		// Cluster nodes for testing
		egressNode  node
		podNode     node
		monitorNode node

		// External container on secondary network
		secondaryTargetContainer infraapi.ExternalContainer

		// Provider context for cleanup
		providerCtx infraapi.Context

		// Pod IPs to check for leaks
		podIPs     []string
		podIPsLock sync.Mutex

		// Egress IP address
		egressIPAddr string

		// Test run parameters
		isIPv6TestRun bool
	)

	ginkgo.BeforeEach(func() {
		// Determine if this is an IPv6 test run
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 3 {
			framework.Failf("Test requires >= 3 nodes, but there are only %d", len(nodes.Items))
		}

		// Select nodes for different roles, if needed.
		egressNode = node{
			name:   nodes.Items[1].Name,
			nodeIP: e2enode.GetAddresses(&nodes.Items[1], v1.NodeInternalIP)[0],
		}
		podNode = node{
			name:   nodes.Items[1].Name,
			nodeIP: e2enode.GetAddresses(&nodes.Items[1], v1.NodeInternalIP)[0],
		}
		monitorNode = node{
			name:   nodes.Items[1].Name,
			nodeIP: e2enode.GetAddresses(&nodes.Items[1], v1.NodeInternalIP)[0],
		}

		isIPv6TestRun = utilnet.IsIPv6String(egressNode.nodeIP)

		// Initialize provider context for resource management
		providerCtx = infraprovider.Get().NewTestContext()

		// Cleanup function to be called at the end of the test
		ginkgo.DeferCleanup(func() {
			ginkgo.By("Cleaning up test resources")
			if _, err := os.Stat(egressIPYaml); err == nil {
				_ = os.Remove(egressIPYaml)
			}
		})
	})

	ginkgo.It("Should prevent pod IP traffic leak when EgressIP is applied to existing running pods", func() {
		// Test Overview:
		// This test validates a different race condition scenario:
		// 1. Pods are created FIRST and are actively sending traffic
		// 2. Then an EgressIP object is created and applied to them
		// 3. During the reconciliation window, traffic should NOT leak with pod IPs
		//
		// This tests the transition from "normal routing" to "egress IP routing"
		// which is common in production when EgressIP policies are added to existing workloads.

		ginkgo.By("Step 0: Setting up secondary network infrastructure")

		// Determine secondary network subnet based on IP family
		secondarySubnet := secondaryIPV4Subnet
		if isIPv6TestRun {
			secondarySubnet = secondaryIPV6Subnet
			egressIPAddr = "2001:db8:abcd:1234::200" // IPv6 egress IP (different from other test)
		} else {
			egressIPAddr = "10.10.10.200" // IPv4 egress IP (different from other test)
		}

		// Create secondary provider network
		framework.Logf("Creating secondary network %s with subnet %s", secondaryNetworkName, secondarySubnet)
		secondaryNetwork, err := providerCtx.CreateNetwork(secondaryNetworkName, secondarySubnet)
		framework.ExpectNoError(err, "Failed to create secondary network %s", secondaryNetworkName)

		// Attach secondary network to egress node
		// NOT NEEDED IF THEY ARE THE SAME
		// framework.Logf("Attaching secondary network to egress node %s", egressNode.name)
		// _, err = providerCtx.AttachNetwork(secondaryNetwork, egressNode.name)
		// framework.ExpectNoError(err, "Failed to attach secondary network to egress node %s", egressNode.name)

		// Attach secondary network to monitor node for traffic capture
		framework.Logf("Attaching secondary network to monitor node %s", monitorNode.name)
		_, err = providerCtx.AttachNetwork(secondaryNetwork, monitorNode.name)
		framework.ExpectNoError(err, "Failed to attach secondary network to monitor node %s", monitorNode.name)

		// Create external container on secondary network as ping target
		externalContainerPort := infraprovider.Get().GetExternalContainerPort()
		secondaryTargetContainer = infraapi.ExternalContainer{
			Name:    "secondary-target-existing-pods",
			Image:   images.AgnHost(),
			Network: secondaryNetwork,
			CmdArgs: getAgnHostHTTPPortBindCMDArgs(externalContainerPort),
			ExtPort: externalContainerPort,
		}
		secondaryTargetContainer, err = providerCtx.CreateExternalContainer(secondaryTargetContainer)
		framework.ExpectNoError(err, "Failed to create external container on secondary network")

		targetIP := secondaryTargetContainer.GetIPv4()
		if isIPv6TestRun {
			targetIP = secondaryTargetContainer.GetIPv6()
		}
		framework.Logf("External target container created with IP %s", targetIP)

		ginkgo.By("Step 1: Label egress node for future egress IP assignment")
		labelNodeForEgress(f, egressNode.name)
		defer unlabelNodeForEgress(f, egressNode.name)

		ginkgo.By("Step 2: Label namespace for egress IP selection")
		podNamespace := f.Namespace
		labels := map[string]string{
			"egress-test": "existing-pods",
		}
		updateNamespaceLabels(f, podNamespace, labels)

		ginkgo.By("Step 5: Start traffic monitoring BEFORE creating EgressIP and pods")
		// This pod will capture traffic during the EgressIP application
		monitorPod := createExistingPodsTrafficMonitorPodLogs(f, monitorPodName, monitorNode.name, targetIP)

		// Wait for monitor pod to be ready
		err = e2epod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, monitorPod)
		framework.ExpectNoError(err, "Monitor pod failed to start")
		framework.Logf("Traffic monitor pod started on node %s", monitorNode.name)

		ginkgo.By(fmt.Sprintf("Step 3: Create %d pods FIRST (before EgressIP exists)", numTestPods))
		// This is the KEY DIFFERENCE: pods are created before EgressIP
		// Pods will initially use normal routing (their pod IPs as source)

		podEgressLabel := map[string]string{
			"egress-existing-pod": "true",
		}

		// Create pods with continuous traffic generation
		// These pods will keep sending traffic throughout the test
		var wg sync.WaitGroup
		podCreationErrors := make(chan error, numTestPods)

		for i := 0; i < numTestPods; i++ {
			wg.Add(1)
			podName := fmt.Sprintf("%s-%d", podNamePrefix, i)

			go func(name string) {
				defer wg.Done()

				// Main container continuously pings the target
				// This ensures traffic is flowing when EgressIP is applied
				mainCommand := []string{
					"/bin/sh",
					"-c",
					fmt.Sprintf("while true; do ping -c 1 -W 1 %s || true; sleep 0.5; done", targetIP),
				}

				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: podNamespace.Name,
						Labels:    podEgressLabel,
					},
					Spec: v1.PodSpec{
						// Enable this if pinnning to particular node is needed
						// NodeSelector: map[string]string{
						// 	"kubernetes.io/hostname": podNode.name,
						// },
						Containers: []v1.Container{
							{
								Name:    "continuous-ping",
								Image:   images.AgnHost(),
								Command: mainCommand,
							},
						},
						RestartPolicy: v1.RestartPolicyNever,
					},
				}

				createdPod, err := f.ClientSet.CoreV1().Pods(podNamespace.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
				if err != nil {
					podCreationErrors <- fmt.Errorf("failed to create pod %s: %v", name, err)
					return
				}

				// Wait for pod to be running
				err = e2epod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, createdPod)
				if err != nil {
					podCreationErrors <- fmt.Errorf("failed waiting for pod %s to be running: %v", name, err)
					return
				}

				// Get pod IP
				podIP, err := getPodIPWithRetry(f.ClientSet, isIPv6TestRun, podNamespace.Name, name)
				if err != nil {
					podCreationErrors <- fmt.Errorf("failed to get IP for pod %s: %v", name, err)
					return
				}

				podIPsLock.Lock()
				podIPs = append(podIPs, podIP.String())
				podIPsLock.Unlock()

				framework.Logf("Created pod %s with IP %s on node %s", name, podIP.String(), podNode.name)
			}(podName)
		}

		// Wait for all pod creation goroutines to complete
		wg.Wait()
		close(podCreationErrors)

		// Check for any pod creation errors
		var creationErrors []error
		for err := range podCreationErrors {
			creationErrors = append(creationErrors, err)
		}
		if len(creationErrors) > 0 {
			framework.Failf("Failed to create some pods: %v", creationErrors)
		}

		framework.Logf("Successfully created %d pods, collected %d pod IPs", numTestPods, len(podIPs))

		// Wait a moment for pods to start pinging
		time.Sleep(1 * time.Second)

		ginkgo.By("Step 6: Create EgressIP object and apply it to existing running pods")
		// THIS IS THE CRITICAL MOMENT: EgressIP is created while pods are actively sending traffic
		// We want to detect any leaks during the reconciliation window

		// Create the EgressIP CRD
		egressIPConfig := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
  name: %s
spec:
  egressIPs:
  - "%s"
  podSelector:
    matchLabels:
      egress-existing-pod: "true"
  namespaceSelector:
    matchLabels:
      egress-test: existing-pods
`, egressIPName, egressIPAddr)

		// Normalize IPv6 address for comparison
		normalizedEgressIP := egressIPAddr
		if isIPv6TestRun {
			normalizedEgressIP = net.ParseIP(egressIPAddr).String()
		}

		err = os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644)
		framework.ExpectNoError(err, "Failed to write EgressIP YAML")

		framework.Logf("Creating EgressIP object with IP %s for existing running pods", normalizedEgressIP)
		e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)
		defer func() {
			framework.Logf("Deleting EgressIP object")
			e2ekubectl.RunKubectlOrDie("default", "delete", "-f", egressIPYaml, "--ignore-not-found=true")
		}()

		ginkgo.By("Step 7: Verify EgressIP status is assigned to egress node")
		var egressIPStatusItems []egressIPStatus
		err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, func(ctx context.Context) (bool, error) {
			egressIPStatusItems = verifyEgressIPStatusLengthEquals(1)
			if len(egressIPStatusItems) != 1 {
				return false, nil
			}
			if egressIPStatusItems[0].Node != egressNode.name {
				framework.Logf("EgressIP not yet assigned to correct node, current: %s, expected: %s",
					egressIPStatusItems[0].Node, egressNode.name)
				return false, nil
			}
			if egressIPStatusItems[0].EgressIP != normalizedEgressIP {
				framework.Logf("EgressIP address mismatch, current: %s, expected: %s",
					egressIPStatusItems[0].EgressIP, normalizedEgressIP)
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err, "Failed to verify EgressIP status")
		framework.Logf("EgressIP %s assigned to node %s", normalizedEgressIP, egressNode.name)

		ginkgo.By("Step 8: Wait for reconciliation to complete and verify egress IP is used")

		// Verify pods are now using the egress IP
		for i := 0; i < 3 && i < numTestPods; i++ {
			podName := fmt.Sprintf("%s-%d", podNamePrefix, i)
			conditionFunc := targetExternalContainerAndTest(secondaryTargetContainer, podNamespace.Name, podName, true, []string{normalizedEgressIP})
			err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, func(ctx context.Context) (bool, error) {
				return conditionFunc()
			})
			framework.ExpectNoError(err, "Pod %s should be using egress IP after reconciliation", podName)
		}
		framework.Logf("Verified pods are now using egress IP %s correctly", normalizedEgressIP)

		ginkgo.By("Step 9: Analyze traffic capture for pod IP leaks during EgressIP application")
		// This is the critical check: did any traffic leak with pod IPs during the transition?

		leakedPodIPs := checkExistingPodsTrafficForLeaksLogs(f, monitorPodName, podNamespace.Name, podIPs, targetIP)

		if len(leakedPodIPs) > 0 {
			framework.Failf("TRAFFIC LEAK DETECTED! The following pod IPs were seen in traffic to %s: %v\n"+
				"This indicates traffic leaked with pod source IPs instead of egress IP %s during EgressIP application to existing pods.",
				targetIP, leakedPodIPs, normalizedEgressIP)
		}

		framework.Logf("SUCCESS: No pod IP traffic leaks detected during EgressIP application. All traffic transitioned to egress IP %s", normalizedEgressIP)

		ginkgo.By("Step 10: Cleanup - Delete test pods")
		for i := 0; i < numTestPods; i++ {
			podName := fmt.Sprintf("%s-%d", podNamePrefix, i)
			err := f.ClientSet.CoreV1().Pods(podNamespace.Name).Delete(context.TODO(), podName, metav1.DeleteOptions{})
			if err != nil {
				framework.Logf("Warning: failed to delete pod %s: %v", podName, err)
			}
		}
	})
})

// createExistingPodsTrafficMonitorPodLogs creates a pod that monitors traffic on the secondary network interface
// This version outputs tcpdump directly to pod logs (stdout) instead of saving to files
func createExistingPodsTrafficMonitorPodLogs(f *framework.Framework, name, nodeName, targetIP string) *v1.Pod {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: f.Namespace.Name,
		},
		Spec: v1.PodSpec{
			NodeSelector: map[string]string{
				"kubernetes.io/hostname": nodeName,
			},
			HostNetwork: true,
			Containers: []v1.Container{
				{
					Name:  "traffic-monitor",
					Image: "docker.io/nicolaka/netshoot:latest",
					Command: []string{
						"/bin/bash",
						"-c",
						fmt.Sprintf(`
							# Find the interface with IP in the secondary network range
							IFACE=$(ip -o addr show | grep -E '10\.10\.10\.|2001:db8:abcd:1234' | awk '{print $2}' | head -1)
							if [ -z "$IFACE" ]; then
								echo "ERROR: Could not find secondary network interface"
								exit 1
							fi
							echo "=== Starting traffic monitoring on interface $IFACE for target IP %s ==="
							echo "=== tcpdump will capture all traffic to/from %s ==="
							echo "=== Looking for pattern: IP <pod_ip> > %s ==="
							echo ""

							# Run tcpdump and output directly to stdout (pod logs)
							# -n: Don't resolve hostnames (faster, shows IPs)
							# -vv: Very verbose output
							# -l: Line buffered (immediate output to logs)
							# host: Filter traffic to/from target IP only
							exec tcpdump -i $IFACE -n -vv -l host %s
						`, targetIP, targetIP, targetIP, targetIP),
					},
					SecurityContext: &v1.SecurityContext{
						Privileged: func(b bool) *bool { return &b }(true),
					},
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}

	createdPod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
	framework.ExpectNoError(err, "Failed to create traffic monitor pod")
	return createdPod
}

// checkExistingPodsTrafficForLeaksLogs analyzes the tcpdump output from pod logs to detect any pod IPs in the traffic
func checkExistingPodsTrafficForLeaksLogs(f *framework.Framework, monitorPodName, namespace string, podIPs []string, targetIP string) []string {
	framework.Logf("Analyzing traffic from monitor pod logs for %d pod IPs during EgressIP application", len(podIPs))
	framework.Logf("Pod IPs to check: %v", podIPs)

	// Give tcpdump a moment to capture and flush logs
	time.Sleep(2 * time.Second)

	leakedIPs := []string{}

	// Get pod logs directly (tcpdump output)
	// Use kubectl logs to retrieve the container logs
	output, err := e2ekubectl.RunKubectl(namespace, "logs", monitorPodName)
	if err != nil {
		framework.Logf("Warning: failed to read monitor pod logs: %v", err)
		framework.Logf("Attempting to read logs via exec...")

		// Fallback: try to get logs via exec and dmesg/journalctl if available
		execCmd := "dmesg"
		output, err = e2ekubectl.RunKubectl(namespace, "exec", monitorPodName, "--", "/bin/sh", "-c", execCmd)
		if err != nil {
			framework.Logf("Warning: failed to read logs via exec: %v", err)
			return leakedIPs
		}
	}

	framework.Logf("Retrieved %d lines of tcpdump output from monitor pod logs", len(strings.Split(output, "\n")))

	// Check if each pod IP appears in the traffic log
	// tcpdump output format examples:
	// "10.244.1.79 > 10.10.10.3: ICMP echo request, id 26, seq 0, length 64"
	// "IP 10.244.1.79.12345 > 10.10.10.3.80: Flags [S], seq 123, length 0"
	for _, podIP := range podIPs {
		if strings.Contains(output, podIP) && strings.Contains(output, targetIP) {
			lines := strings.Split(output, "\n")
			for _, line := range lines {
				// Look for lines where pod IP appears as source going to target
				// No need to check traffic direction, pods are pinging external node
				if strings.Contains(line, podIP) && strings.Contains(line, targetIP) {
					framework.Logf("LEAK DETECTED: Pod IP %s found as source in traffic capture during EgressIP application", podIP)
					framework.Logf("  Matching line: %s", line)
					leakedIPs = append(leakedIPs, podIP)
					break // Only count each pod IP once
					// }
				}
			}
		}
	}

	if len(leakedIPs) == 0 {
		framework.Logf("Traffic analysis complete: No pod IPs detected as source in %d lines of tcpdump output",
			len(strings.Split(output, "\n")))
		framework.Logf("Sample of captured traffic (first 10 lines):")
		lines := strings.Split(output, "\n")
		for i, line := range lines {
			if i >= 10 {
				break
			}
			if strings.TrimSpace(line) != "" {
				framework.Logf("  %s", line)
			}
		}
	} else {
		framework.Logf("LEAK SUMMARY: Found %d pod IPs leaking traffic: %v", len(leakedIPs), leakedIPs)
	}

	return leakedIPs
}
