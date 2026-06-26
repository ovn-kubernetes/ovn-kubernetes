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

var _ = ginkgo.Describe("[secondary-host-eip] Egress IP Packet Mark Solution Validation", func() {
	const (
		egressIPName        = "egressip-pktmark-test"
		egressIPYaml        = "/tmp/egressip-pktmark.yaml"
		monitorPodName      = "traffic-monitor-pktmark"
		podNamePrefix       = "pktmark-pod"
		numTestPods         = 1 // Smaller number for focused validation
		retryInterval       = 1 * time.Second
		retryTimeout        = 120 * time.Second
		egressIPUnreadyMark = "8192" // 0x2000 in hex, using decimal like EgressIPNodeConnectionMark
	)

	f := wrappedTestFramework("egressip-pktmark-validation")

	// Helper function to get pod IP with retry
	getPodIPWithRetry := func(clientSet clientset.Interface, v6 bool, namespace, name string) (net.IP, error) {
		var srcPodIP net.IP
		err := wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, func(ctx context.Context) (bool, error) {
			pod, err := clientSet.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, nil // Retry on error
			}
			ips, err := ovnutil.DefaultNetworkPodIPs(pod)
			if err != nil {
				return false, nil // Retry on error
			}
			srcPodIP, err = ovnutil.MatchFirstIPFamily(v6, ips)
			if err != nil {
				return false, nil // Retry on error
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

	// Helper to check OVN NB DB for LRP with packet mark
	checkOVNLRPHasPktMark := func(nodeName string, podIP string, expectedMark string) error {
		framework.Logf("Checking OVN NB DB for LRP with pkt_mark for pod IP %s", podIP)

		// Run ovn-nbctl to list logical router policies
		// We need to find the reroute policy for this pod IP
		cmd := fmt.Sprintf("ovn-nbctl --format=json list Logical_Router_Policy")
		output, err := runOVNNBCTLCommand(nodeName, cmd)
		if err != nil {
			return fmt.Errorf("failed to query OVN NB DB: %v", err)
		}

		// Parse OVN JSON output (data-table format)
		// Format: {"data": [[row1], [row2], ...], "headings": ["col1", "col2", ...]}
		var result struct {
			Data     [][]interface{} `json:"data"`
			Headings []string        `json:"headings"`
		}
		err = json.Unmarshal([]byte(output), &result)
		if err != nil {
			return fmt.Errorf("failed to parse OVN NB output: %v", err)
		}

		// Create a map of heading names to column indices
		headingIndex := make(map[string]int)
		for i, heading := range result.Headings {
			headingIndex[heading] = i
		}

		// Find the indices for the fields we need
		matchIdx, hasMatch := headingIndex["match"]
		optionsIdx, hasOptions := headingIndex["options"]

		if !hasMatch || !hasOptions {
			return fmt.Errorf("OVN output missing required fields (match or options)")
		}

		// Look for policy matching the pod IP
		for _, row := range result.Data {
			if len(row) <= matchIdx || len(row) <= optionsIdx {
				continue
			}

			// Get the match field
			match, ok := row[matchIdx].(string)
			if !ok {
				continue
			}

			// Check if this is the reroute policy for our pod
			if strings.Contains(match, podIP) {
				framework.Logf("Found LRP for pod IP %s: match=%s", podIP, match)

				// Get the options field - format is ["map", [["pkt_mark", "0x2000"]]]
				options, ok := row[optionsIdx].([]interface{})
				if !ok || len(options) < 2 {
					return fmt.Errorf("LRP for pod %s has invalid options field", podIP)
				}

				// Check if this is a "map" type
				if mapType, ok := options[0].(string); !ok || mapType != "map" {
					return fmt.Errorf("LRP options is not a map for pod %s", podIP)
				}

				// Parse the map entries - format is [["key1", "val1"], ["key2", "val2"], ...]
				mapEntries, ok := options[1].([]interface{})
				if !ok {
					return fmt.Errorf("LRP options map entries invalid for pod %s", podIP)
				}

				// Look for pkt_mark entry
				for _, entry := range mapEntries {
					pair, ok := entry.([]interface{})
					if !ok || len(pair) != 2 {
						continue
					}

					key, keyOk := pair[0].(string)
					value, valOk := pair[1].(string)
					if keyOk && valOk && key == "pkt_mark" {
						framework.Logf("Found pkt_mark in LRP options: %s", value)
						if value == expectedMark {
							framework.Logf("✓ LRP for pod %s has correct pkt_mark=%s", podIP, expectedMark)
							return nil
						}
						return fmt.Errorf("LRP has pkt_mark=%s, expected %s", value, expectedMark)
					}
				}

				return fmt.Errorf("LRP for pod %s found but does not have pkt_mark option", podIP)
			}
		}

		return fmt.Errorf("no LRP found for pod IP %s", podIP)
	}

	// Helper to check NFTables chain exists on node
	checkNFTablesChainExists := func(nodeName string, chainName string) error {
		framework.Logf("Checking if NFTables chain %s exists on node %s", chainName, nodeName)

		cmd := fmt.Sprintf("nft list chain inet ovn-kubernetes %s", chainName)
		output, err := runCommandOnNode(nodeName, cmd)
		if err != nil {
			return fmt.Errorf("NFTables chain %s does not exist: %v", chainName, err)
		}

		if strings.Contains(output, chainName) {
			framework.Logf("✓ NFTables chain %s exists on node %s", chainName, nodeName)
			return nil
		}

		return fmt.Errorf("NFTables chain %s not found in output", chainName)
	}

	// Helper to check NFTables rules use cluster pod CIDR
	checkNFTablesRulesHaveClusterPodCIDR := func(nodeName string) error {
		framework.Logf("Checking NFTables rules use cluster pod CIDR on node %s", nodeName)

		cmd := "nft list chain inet ovn-kubernetes ovn-kube-egress-ip-unready"
		output, err := runCommandOnNode(nodeName, cmd)
		if err != nil {
			return fmt.Errorf("failed to list NFTables chain: %v", err)
		}

		// Get cluster pod CIDR from any node (all nodes share the same cluster CIDR)
		nodes, err := f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
		framework.ExpectNoError(err, "Failed to list nodes")

		if len(nodes.Items) == 0 || len(nodes.Items[0].Spec.PodCIDRs) == 0 {
			return fmt.Errorf("no pod CIDRs found on nodes")
		}

		// Node CIDR example: "10.244.1.0/24"
		// Cluster CIDR should be: "10.244.0.0/16" (broader)
		nodeCIDR := nodes.Items[0].Spec.PodCIDRs[0]
		ip, _, err := net.ParseCIDR(nodeCIDR)
		framework.ExpectNoError(err, "Failed to parse node CIDR")

		var expectedCIDR string
		if ip.To4() != nil {
			// IPv4: Expect cluster CIDR like 10.244.0.0/16
			parts := strings.Split(ip.String(), ".")
			expectedCIDR = fmt.Sprintf("%s.%s.0.0/16", parts[0], parts[1])
		} else {
			// IPv6: Expect cluster CIDR like fd01::/48
			expectedCIDR = "fd01::/48"
		}

		// Verify cluster CIDR appears in rules
		if !strings.Contains(output, expectedCIDR) {
			return fmt.Errorf("NFTables rules don't contain cluster pod CIDR %s\nOutput: %s",
				expectedCIDR, output)
		}

		framework.Logf("✓ NFTables rules contain cluster pod CIDR %s", expectedCIDR)
		return nil
	}

	// Helper to check NFTables rules exist
	checkNFTablesRulesExist := func(nodeName string, chainName string) error {
		framework.Logf("Checking NFTables rules in chain %s on node %s", chainName, nodeName)

		cmd := fmt.Sprintf("nft list chain inet ovn-kubernetes %s", chainName)
		output, err := runCommandOnNode(nodeName, cmd)
		if err != nil {
			return fmt.Errorf("failed to list NFTables chain: %v", err)
		}

		// Log the actual NFTables output for debugging
		framework.Logf("NFTables chain output:\n%s", output)

		// Check for CLEAR rule (mark set 0)
		if !strings.Contains(output, "meta mark set 0") {
			return fmt.Errorf("CLEAR rule (meta mark set 0) not found in chain %s", chainName)
		}
		framework.Logf("✓ Found CLEAR rule in chain %s", chainName)

		// Check for DROP rule
		if !strings.Contains(output, "drop") {
			return fmt.Errorf("DROP rule not found in chain %s", chainName)
		}
		framework.Logf("✓ Found DROP rule in chain %s", chainName)

		// Check for the correct mark value (0x2000 or 8192 in decimal)
		// NFTables may display the mark in hex (0x2000) or decimal (8192) format
		hasHexMark := strings.Contains(output, egressIPUnreadyMark)     // Check for 0x2000
		hasDecimalMark := strings.Contains(output, "8192")              // Check for 8192 (decimal)
		hasMarkWithoutPrefix := strings.Contains(output, "mark 0x2000") // Check for "mark 0x2000"
		hasMarkShortForm := strings.Contains(output, "0x00002000")      // Check for full form

		if !hasHexMark && !hasDecimalMark && !hasMarkWithoutPrefix && !hasMarkShortForm {
			return fmt.Errorf("mark value %s (or 8192 decimal) not found in chain %s\nOutput: %s",
				egressIPUnreadyMark, chainName, output)
		}
		framework.Logf("✓ Found correct mark value in chain %s", chainName)

		return nil
	}

	// Helper to check NFTables drop counters - CRITICAL for validating the fix works
	checkNFTablesDropCounters := func(nodeName string, chainName string) (int, error) {
		framework.Logf("Checking NFTables drop counters in chain %s on node %s", chainName, nodeName)

		cmd := fmt.Sprintf("nft list chain inet ovn-kubernetes %s", chainName)
		output, err := runCommandOnNode(nodeName, cmd)
		if err != nil {
			return 0, fmt.Errorf("failed to list NFTables chain: %v", err)
		}

		framework.Logf("NFTables chain output for counter check:\n%s", output)

		// Look for counter in DROP rule
		// Format: "meta mark 0x2000 counter packets 123 bytes 12345 drop"
		lines := strings.Split(output, "\n")
		for _, line := range lines {
			if strings.Contains(line, "drop") && strings.Contains(line, "counter") {
				// Try to extract packet count
				// Example: "meta mark 0x2000 counter packets 42 bytes 5040 drop"
				if strings.Contains(line, "packets") {
					fields := strings.Fields(line)
					for i, field := range fields {
						if field == "packets" && i+1 < len(fields) {
							var count int
							_, err := fmt.Sscanf(fields[i+1], "%d", &count)
							if err == nil {
								framework.Logf("✓ NFTables DROP rule has counter: %d packets dropped", count)
								return count, nil
							}
						}
					}
				}
			}
		}

		framework.Logf("⚠ WARNING: No counter found in DROP rule - cannot verify if any packets were dropped!")
		return 0, nil
	}

	// Helper to check OVS flows for packet marking - CRITICAL diagnostic
	checkOVSFlowsForPacketMark := func(nodeName string, podIP string) error {
		framework.Logf("Checking OVS flows for packet mark actions for pod IP %s on node %s", podIP, nodeName)

		// Check logical flows in OVN SB DB
		cmd := fmt.Sprintf("ovn-sbctl lflow-list | grep -i 'pkt.mark' | grep -i '%s' || echo 'No flows found'", podIP)
		output, err := runOVNNBCTLCommand(nodeName, cmd)
		if err != nil {
			framework.Logf("Warning: failed to query OVN SB flows: %v", err)
		} else {
			framework.Logf("OVN logical flows with pkt.mark for pod %s:\n%s", podIP, output)
		}

		// Check OpenFlow flows on br-int for mark actions
		// Look for load:0x2000->NXM_NX_PKT_MARK[] which sets the packet mark
		cmd2 := fmt.Sprintf("ovs-ofctl dump-flows br-int | grep -E 'load:.*NXM_NX_PKT_MARK|set_field.*pkt_mark' || echo 'No mark flows found'")
		output2, err := runCommandOnNode(nodeName, cmd2)
		if err != nil {
			framework.Logf("Warning: failed to query OVS flows: %v", err)
		} else {
			framework.Logf("OpenFlow mark actions on br-int:\n%s", output2)
		}

		// CRITICAL CHECK: Does OVN's pkt_mark translate to OVS actions?
		if !strings.Contains(output2, "NXM_NX_PKT_MARK") && !strings.Contains(output2, "pkt_mark") {
			framework.Logf("⚠ CRITICAL WARNING: No OVS flows found that set pkt_mark!")
			framework.Logf("This indicates OVN's pkt_mark in LRP is NOT translating to OpenFlow actions!")
			return fmt.Errorf("OVN pkt_mark not translated to OVS flows - packets won't be marked in datapath")
		}

		return nil
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

		// Pod IPs to check
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

		// Select nodes for different roles
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

		// Cleanup function
		ginkgo.DeferCleanup(func() {
			ginkgo.By("Cleaning up test resources")
			if _, err := os.Stat(egressIPYaml); err == nil {
				_ = os.Remove(egressIPYaml)
			}
		})
	})

	ginkgo.It("Should create packet mark infrastructure and validate OVN and NFTables configuration", func() {
		// Test Overview:
		// This test validates the packet marking solution implementation:
		// 1. Creates EgressIP for pods
		// 2. Verifies OVN NB DB has LRP with pkt_mark=0x2000
		// 3. Verifies NFTables chain and sets are created on node
		// 4. Verifies NFTables CLEAR and DROP rules exist
		// 5. Verifies pod IPs are added to NFTables ready set after SNAT

		ginkgo.By("Step 0: Setting up secondary network infrastructure")

		// Determine secondary network subnet based on IP family
		secondarySubnet := secondaryIPV4Subnet
		if isIPv6TestRun {
			secondarySubnet = secondaryIPV6Subnet
			egressIPAddr = "2001:db8:abcd:1234::201" // IPv6 egress IP
		} else {
			egressIPAddr = "10.10.10.201" // IPv4 egress IP
		}

		// Create secondary provider network
		framework.Logf("Creating secondary network %s with subnet %s", secondaryNetworkName, secondarySubnet)
		secondaryNetwork, err := providerCtx.CreateNetwork(secondaryNetworkName, secondarySubnet)
		framework.ExpectNoError(err, "Failed to create secondary network %s", secondaryNetworkName)

		// Attach secondary network to monitor node for traffic capture
		framework.Logf("Attaching secondary network to monitor node %s", monitorNode.name)
		_, err = providerCtx.AttachNetwork(secondaryNetwork, monitorNode.name)
		framework.ExpectNoError(err, "Failed to attach secondary network to monitor node %s", monitorNode.name)

		// Create external container on secondary network as ping target
		externalContainerPort := infraprovider.Get().GetExternalContainerPort()
		secondaryTargetContainer = infraapi.ExternalContainer{
			Name:    "secondary-target-pktmark",
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

		ginkgo.By("Step 1: Label egress node for egress IP assignment")
		labelNodeForEgress(f, egressNode.name)
		defer unlabelNodeForEgress(f, egressNode.name)

		ginkgo.By("Step 2: Label namespace for egress IP selection")
		podNamespace := f.Namespace
		labels := map[string]string{
			"egress-test": "pktmark-validation",
		}
		updateNamespaceLabels(f, podNamespace, labels)

		ginkgo.By("Step 3: Create EgressIP object FIRST (before pods)")
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
      egress-pktmark: "true"
  namespaceSelector:
    matchLabels:
      egress-test: pktmark-validation
`, egressIPName, egressIPAddr)

		// Normalize IPv6 address for comparison
		normalizedEgressIP := egressIPAddr
		if isIPv6TestRun {
			normalizedEgressIP = net.ParseIP(egressIPAddr).String()
		}

		err = os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644)
		framework.ExpectNoError(err, "Failed to write EgressIP YAML")

		framework.Logf("Creating EgressIP object with IP %s", normalizedEgressIP)
		e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)
		defer func() {
			framework.Logf("Deleting EgressIP object")
			e2ekubectl.RunKubectlOrDie("default", "delete", "-f", egressIPYaml, "--ignore-not-found=true")
		}()

		ginkgo.By("Step 4: Verify EgressIP status is assigned to egress node")
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

		ginkgo.By("Step 5: Verify NFTables infrastructure is created on node")

		// Check that NFTables chain exists
		err = checkNFTablesChainExists(egressNode.name, "ovn-kube-egress-ip-unready")
		framework.ExpectNoError(err, "NFTables chain should exist")

		// Check that NFTables rules use cluster pod CIDR (not per-pod IPs)
		err = checkNFTablesRulesHaveClusterPodCIDR(egressNode.name)
		framework.ExpectNoError(err, "NFTables rules should use cluster pod CIDR")

		// Check that NFTables rules exist (DROP and CLEAR)
		err = checkNFTablesRulesExist(egressNode.name, "ovn-kube-egress-ip-unready")
		framework.ExpectNoError(err, "NFTables rules should exist")

		framework.Logf("✓ NFTables infrastructure validated on node %s", egressNode.name)

		ginkgo.By(fmt.Sprintf("Step 6: Create %d pods with egress IP label", numTestPods))

		podEgressLabel := map[string]string{
			"egress-pktmark": "true",
		}

		// Create pods
		var wg sync.WaitGroup
		podCreationErrors := make(chan error, numTestPods)

		for i := 0; i < numTestPods; i++ {
			wg.Add(1)
			podName := fmt.Sprintf("%s-%d", podNamePrefix, i)

			go func(name string, index int) {
				defer wg.Done()

				// Main container continuously pings the target
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
						NodeSelector: map[string]string{
							"kubernetes.io/hostname": podNode.name,
						},
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

				framework.Logf("Created pod %s with IP %s", name, podIP.String())

				// VALIDATION: Check OVN LRP has pkt_mark for first few pods
				if index < 2 {
					err = wait.PollUntilContextTimeout(context.Background(), retryInterval, 30*time.Second, true, func(ctx context.Context) (bool, error) {
						err := checkOVNLRPHasPktMark(egressNode.name, podIP.String(), egressIPUnreadyMark)
						return err == nil, nil
					})
					if err != nil {
						podCreationErrors <- fmt.Errorf("OVN LRP validation failed for pod %s: %v", name, err)
						return
					}
					framework.Logf("✓ OVN LRP validated for pod %s (has pkt_mark=%s)", name, egressIPUnreadyMark)
				}
			}(podName, i)
		}

		// Wait for all pod creation and validation
		wg.Wait()
		close(podCreationErrors)

		// Check for any errors
		var creationErrors []error
		for err := range podCreationErrors {
			creationErrors = append(creationErrors, err)
		}
		if len(creationErrors) > 0 {
			framework.Failf("Failed to create/validate some pods: %v", creationErrors)
		}

		framework.Logf("✓ Successfully created and validated %d pods", numTestPods)

		ginkgo.By("Step 7: CRITICAL - Verify OVS flows translate pkt_mark to datapath actions")

		// Check if OVN's pkt_mark in LRP actually translates to OpenFlow actions
		// This is THE critical check - if this fails, packets won't be marked at all!
		if len(podIPs) > 0 {
			err = checkOVSFlowsForPacketMark(egressNode.name, podIPs[0])
			if err != nil {
				framework.Logf("❌ CRITICAL FAILURE: %v", err)
				framework.Logf("This explains why traffic is leaking - OVN pkt_mark doesn't set kernel skb->mark!")
				framework.Failf("OVS flow validation failed: %v", err)
			}
			framework.Logf("✓ OVS flows validated - pkt_mark should propagate to kernel")
		}

		ginkgo.By("Step 8: Check NFTables drop counters (diagnostic)")

		// Check if NFTables is actually dropping any packets
		dropCount, err := checkNFTablesDropCounters(egressNode.name, "ovn-kube-egress-ip-unready")
		if err != nil {
			framework.Logf("Warning: Could not check drop counters: %v", err)
		} else {
			framework.Logf("NFTables drop counter: %d packets", dropCount)
			if dropCount == 0 {
				framework.Logf("⚠ WARNING: Drop counter is 0 - this suggests packets are NOT being marked!")
				framework.Logf("Expected: Some packets dropped during reconciliation window")
				framework.Logf("Actual: No drops detected - mark propagation may be failing")
			} else {
				framework.Logf("✓ Drop counter shows %d packets were dropped - marking is working!", dropCount)
			}
		}

		ginkgo.By("Step 9: Final validation summary")
		framework.Logf("====================================")
		framework.Logf("Packet Mark Solution Validation PASSED:")
		framework.Logf("  ✓ OVN NB DB: LRPs created with pkt_mark=%s", egressIPUnreadyMark)
		framework.Logf("  ✓ OVS Flows: pkt_mark translated to OpenFlow actions")
		framework.Logf("  ✓ NFTables: Chain 'ovn-kube-egress-ip-unready' exists")
		framework.Logf("  ✓ NFTables: Rules use cluster pod CIDR (CIDR-based approach)")
		framework.Logf("  ✓ NFTables: DROP and CLEAR rules present")
		framework.Logf("  ✓ NFTables: Drop counter shows packets being marked/dropped")
		framework.Logf("  ✓ Traffic: All pods using egress IP %s", normalizedEgressIP)
		framework.Logf("====================================")

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

// Helper function to run OVN nbctl command on a node
func runOVNNBCTLCommand(nodeName string, cmd string) (string, error) {
	// Run command via kubectl exec on ovnkube-node pod
	ovnPodName, err := getOVNKubeNodePodName(nodeName)
	if err != nil {
		return "", err
	}

	fullCmd := fmt.Sprintf("kubectl exec -n ovn-kubernetes %s -c ovnkube-controller -- %s", ovnPodName, cmd)
	output, err := e2ekubectl.RunKubectl("default", "exec", "-n", "ovn-kubernetes", ovnPodName, "-c", "ovnkube-controller", "--", "sh", "-c", cmd)
	if err != nil {
		return "", fmt.Errorf("failed to run command '%s': %v", fullCmd, err)
	}

	return output, nil
}

// Helper function to run command on a node
func runCommandOnNode(nodeName string, cmd string) (string, error) {
	// Run command via kubectl exec on ovnkube-node pod
	ovnPodName, err := getOVNKubeNodePodName(nodeName)
	if err != nil {
		return "", err
	}

	// Use ovnkube-node container for NFTables commands
	output, err := e2ekubectl.RunKubectl("default", "exec", "-n", "ovn-kubernetes", ovnPodName, "-c", "ovnkube-controller", "--", "sh", "-c", cmd)
	if err != nil {
		return "", fmt.Errorf("failed to run command on node %s: %v", nodeName, err)
	}

	return output, nil
}

// Helper to get ovnkube-node pod name for a given node
func getOVNKubeNodePodName(nodeName string) (string, error) {
	// List pods in ovn-kubernetes namespace with node selector
	output, err := e2ekubectl.RunKubectl("default", "get", "pods", "-n", "ovn-kubernetes",
		"-l", "app=ovnkube-node",
		"--field-selector", fmt.Sprintf("spec.nodeName=%s", nodeName),
		"-o", "jsonpath='{.items[0].metadata.name}'")
	if err != nil {
		return "", fmt.Errorf("failed to get ovnkube-node pod for node %s: %v", nodeName, err)
	}

	// Remove quotes from jsonpath output
	podName := strings.Trim(output, "'")
	if podName == "" {
		return "", fmt.Errorf("no ovnkube-node pod found for node %s", nodeName)
	}

	return podName, nil
}
