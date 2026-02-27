package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/api"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

const (
	ovnKubePodSubnetMasqChain = "ovn-kube-pod-subnet-masq"
)

func setNodeSNATExcludeSubnetsAnnotation(cs clientset.Interface, nodeName string, subnets []string) error {
	subnetsJSON, err := json.Marshal(subnets)
	if err != nil {
		return fmt.Errorf("failed to marshal subnets: %w", err)
	}

	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"annotations": map[string]string{
				util.OvnNodeDontSNATSubnets: string(subnetsJSON),
			},
		},
	}

	patchData, err := json.Marshal(&patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	_, err = cs.CoreV1().Nodes().Patch(context.TODO(), nodeName, k8stypes.MergePatchType, patchData, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to patch node %s: %w", nodeName, err)
	}

	return nil
}

func removeNodeSNATExcludeSubnetsAnnotation(cs clientset.Interface, nodeName string) error {
	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"annotations": map[string]*string{
				util.OvnNodeDontSNATSubnets: nil,
			},
		},
	}

	patchData, err := json.Marshal(&patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	_, err = cs.CoreV1().Nodes().Patch(context.TODO(), nodeName, k8stypes.MergePatchType, patchData, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to patch node %s: %w", nodeName, err)
	}

	return nil
}

func getNodeAnnotation(cs clientset.Interface, nodeName, annotation string) (string, bool, error) {
	node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return "", false, err
	}
	value, ok := node.Annotations[annotation]
	return value, ok, nil
}

func getNFTablesChainRules(nodeName, chainName string) (string, error) {
	nftCmd := []string{"nft", "list", "chain", "inet", "ovn-kubernetes", chainName}
	output, err := infraprovider.Get().ExecK8NodeCommand(nodeName, nftCmd)
	if err != nil {
		return "", fmt.Errorf("failed to list chain %s on node %s: %w", chainName, nodeName, err)
	}
	return output, nil
}

func getNFTablesSetElements(nodeName, setName string) (string, error) {
	nftCmd := []string{"nft", "list", "set", "inet", "ovn-kubernetes", setName}
	output, err := infraprovider.Get().ExecK8NodeCommand(nodeName, nftCmd)
	if err != nil {
		return "", fmt.Errorf("failed to list set %s on node %s: %w", setName, nodeName, err)
	}
	return output, nil
}

func checkNFTablesChainContainsRule(nodeName, chainName, rulePattern string) wait.ConditionFunc {
	return func() (bool, error) {
		rules, err := getNFTablesChainRules(nodeName, chainName)
		if err != nil {
			framework.Logf("Error getting chain rules: %v", err)
			return false, nil
		}
		contains := strings.Contains(rules, rulePattern)
		if !contains {
			framework.Logf("Chain %s does not contain rule pattern %q. Current rules:\n%s", chainName, rulePattern, rules)
		}
		return contains, nil
	}
}

func checkNFTablesSetContainsElement(nodeName, setName, element string) wait.ConditionFunc {
	return func() (bool, error) {
		output, err := getNFTablesSetElements(nodeName, setName)
		if err != nil {
			framework.Logf("Error getting set elements: %v", err)
			return false, nil
		}
		contains := strings.Contains(output, element)
		if !contains {
			framework.Logf("Set %s does not contain element %q. Current elements:\n%s", setName, element, output)
		}
		return contains, nil
	}
}

func checkNFTablesSetDoesNotContainElement(nodeName, setName, element string) wait.ConditionFunc {
	containsFunc := checkNFTablesSetContainsElement(nodeName, setName, element)
	return func() (bool, error) {
		contains, err := containsFunc()
		if err != nil {
			return false, err
		}
		return !contains, nil
	}
}

var _ = ginkgo.Describe("Local Gateway Pod Subnet SNAT", feature.Service, func() {
	const (
		retryInterval = 1 * time.Second
		retryTimeout  = 60 * time.Second

		testSubnetV4_1 = "198.51.100.0/24"
		testSubnetV4_2 = "203.0.113.0/24"
		testSubnetV6   = "2001:db8::/32"
	)

	f := wrappedTestFramework("local-gw-masq")
	var cs clientset.Interface
	var nodeName string

	ginkgo.BeforeEach(func() {
		cs = f.ClientSet

		if !IsGatewayModeLocal(cs) {
			e2eskipper.Skipf("Skipping test - not in local gateway mode")
		}

		node, err := e2enode.GetRandomReadySchedulableNode(context.TODO(), cs)
		framework.ExpectNoError(err, "failed to get a schedulable node")
		nodeName = node.Name

		framework.Logf("Using node %s for local gateway SNAT test", nodeName)
		err = removeNodeSNATExcludeSubnetsAnnotation(cs, nodeName)
		if err != nil {
			framework.Logf("Note: failed to remove existing SNAT exclude subnets annotation (may not exist): %v", err)
		}
		_ = wait.PollImmediate(retryInterval, retryTimeout, checkNFTablesSetDoesNotContainElement(
			nodeName, types.NFTMgmtPortNoSNATSubnetsV4, testSubnetV4_1))
		_ = wait.PollImmediate(retryInterval, retryTimeout, checkNFTablesSetDoesNotContainElement(
			nodeName, types.NFTMgmtPortNoSNATSubnetsV4, testSubnetV4_2))
	})

	ginkgo.AfterEach(func() {
		if nodeName != "" {
			err := removeNodeSNATExcludeSubnetsAnnotation(cs, nodeName)
			if err != nil {
				framework.Logf("Warning: failed to remove SNAT exclude subnets annotation: %v", err)
			}
		}
	})

	ginkgo.It("should have return rules for no-snat-subnets in ovn-kube-pod-subnet-masq chain", func() {
		ginkgo.By("Verifying the ovn-kube-pod-subnet-masq chain exists")
		var chainRules string
		err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			var err error
			chainRules, err = getNFTablesChainRules(nodeName, ovnKubePodSubnetMasqChain)
			if err != nil {
				framework.Logf("Chain not ready: %v", err)
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err, "ovn-kube-pod-subnet-masq chain should exist on node %s", nodeName)
		framework.Logf("Chain %s rules:\n%s", ovnKubePodSubnetMasqChain, chainRules)

		ginkgo.By("Verifying the chain has return rule referencing mgmtport-no-snat-subnets-v4 set")
		gomega.Expect(chainRules).To(gomega.MatchRegexp(`ip\s+daddr\s+@mgmtport-no-snat-subnets-v4\s+return`),
			"chain should have 'ip daddr @mgmtport-no-snat-subnets-v4 return' rule")
	})

	ginkgo.It("should populate nftables sets when SNAT exclude annotation is added", func() {
		ginkgo.By("Verifying mgmtport-no-snat-subnets-v4 set exists and test subnets are not present")
		var setElements string
		err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			var err error
			setElements, err = getNFTablesSetElements(nodeName, types.NFTMgmtPortNoSNATSubnetsV4)
			return err == nil, nil
		})
		framework.ExpectNoError(err, "mgmtport-no-snat-subnets-v4 set should exist")
		gomega.Expect(setElements).NotTo(gomega.ContainSubstring(testSubnetV4_1),
			"first test subnet should not be in set initially")
		gomega.Expect(setElements).NotTo(gomega.ContainSubstring(testSubnetV4_2),
			"second test subnet should not be in set initially")

		// Test single IPv4 subnet
		ginkgo.By(fmt.Sprintf("Adding single SNAT exclude subnet annotation with %s", testSubnetV4_1))
		err = setNodeSNATExcludeSubnetsAnnotation(cs, nodeName, []string{testSubnetV4_1})
		framework.ExpectNoError(err, "failed to set SNAT exclude subnets annotation")

		ginkgo.By("Verifying annotation was set on the node")
		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			value, ok, err := getNodeAnnotation(cs, nodeName, util.OvnNodeDontSNATSubnets)
			if err != nil {
				return false, nil
			}
			if !ok {
				framework.Logf("Annotation not yet set on node")
				return false, nil
			}
			framework.Logf("Annotation value on node: %s", value)
			return strings.Contains(value, testSubnetV4_1), nil
		})
		framework.ExpectNoError(err, "annotation should be set on the node")

		ginkgo.By("Verifying the excluded subnet is in mgmtport-no-snat-subnets-v4 set")
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNFTablesSetContainsElement(
			nodeName, types.NFTMgmtPortNoSNATSubnetsV4, testSubnetV4_1))
		framework.ExpectNoError(err, "excluded subnet should be in mgmtport-no-snat-subnets-v4 set after annotation is added")

		// Test multiple IPv4 subnets
		ginkgo.By(fmt.Sprintf("Updating annotation to include multiple subnets: %s, %s", testSubnetV4_1, testSubnetV4_2))
		err = setNodeSNATExcludeSubnetsAnnotation(cs, nodeName, []string{testSubnetV4_1, testSubnetV4_2})
		framework.ExpectNoError(err, "failed to update SNAT exclude subnets annotation")

		ginkgo.By("Verifying both subnets are in the set")
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNFTablesSetContainsElement(
			nodeName, types.NFTMgmtPortNoSNATSubnetsV4, testSubnetV4_1))
		framework.ExpectNoError(err, "first subnet should be in set")
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNFTablesSetContainsElement(
			nodeName, types.NFTMgmtPortNoSNATSubnetsV4, testSubnetV4_2))
		framework.ExpectNoError(err, "second subnet should be in set")

		// Test IPv6 subnet (if dual-stack)
		ginkgo.By("Checking if IPv6 set exists (indicates dual-stack)")
		_, ipv6Err := getNFTablesSetElements(nodeName, types.NFTMgmtPortNoSNATSubnetsV6)
		if ipv6Err == nil {
			ginkgo.By("Verifying test IPv6 subnet is NOT in the set initially")
			setElementsV6, err := getNFTablesSetElements(nodeName, types.NFTMgmtPortNoSNATSubnetsV6)
			framework.ExpectNoError(err, "should be able to list IPv6 set elements")
			gomega.Expect(setElementsV6).NotTo(gomega.ContainSubstring(testSubnetV6),
				"test IPv6 subnet should NOT be in set initially")

			ginkgo.By(fmt.Sprintf("Adding IPv6 subnet to annotation: %s, %s, %s", testSubnetV4_1, testSubnetV4_2, testSubnetV6))
			err = setNodeSNATExcludeSubnetsAnnotation(cs, nodeName, []string{testSubnetV4_1, testSubnetV4_2, testSubnetV6})
			framework.ExpectNoError(err, "failed to set SNAT exclude subnets annotation with IPv6")

			ginkgo.By("Verifying IPv6 subnet is in the v6 set")
			err = wait.PollImmediate(retryInterval, retryTimeout, checkNFTablesSetContainsElement(
				nodeName, types.NFTMgmtPortNoSNATSubnetsV6, testSubnetV6))
			framework.ExpectNoError(err, "IPv6 subnet should be in mgmtport-no-snat-subnets-v6 set")

			ginkgo.By("Verifying IPv6 subnet is NOT in the v4 set")
			setElementsV4, err := getNFTablesSetElements(nodeName, types.NFTMgmtPortNoSNATSubnetsV4)
			framework.ExpectNoError(err, "should be able to list v4 set elements")
			gomega.Expect(setElementsV4).NotTo(gomega.ContainSubstring(testSubnetV6),
				"IPv6 subnet should NOT be in v4 set")

			ginkgo.By("Verifying IPv4 subnets are still in the v4 set after adding IPv6")
			gomega.Expect(setElementsV4).To(gomega.ContainSubstring(testSubnetV4_1),
				"first IPv4 subnet should still be in v4 set")
			gomega.Expect(setElementsV4).To(gomega.ContainSubstring(testSubnetV4_2),
				"second IPv4 subnet should still be in v4 set")
		} else {
			framework.Logf("Skipping IPv6 portion - mgmtport-no-snat-subnets-v6 set does not exist (IPv4-only cluster)")
		}

		// Test removal
		ginkgo.By("Removing the SNAT exclude subnet annotation")
		err = removeNodeSNATExcludeSubnetsAnnotation(cs, nodeName)
		framework.ExpectNoError(err, "failed to remove SNAT exclude subnets annotation")

		ginkgo.By("Verifying annotation was removed from the node")
		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			_, ok, err := getNodeAnnotation(cs, nodeName, util.OvnNodeDontSNATSubnets)
			if err != nil {
				return false, nil
			}
			return !ok, nil
		})
		framework.ExpectNoError(err, "annotation should be removed from the node")

		ginkgo.By("Verifying all IPv4 subnets are removed from mgmtport-no-snat-subnets-v4 set")
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNFTablesSetDoesNotContainElement(
			nodeName, types.NFTMgmtPortNoSNATSubnetsV4, testSubnetV4_1))
		framework.ExpectNoError(err, "first subnet should be removed from the set after annotation is removed")
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNFTablesSetDoesNotContainElement(
			nodeName, types.NFTMgmtPortNoSNATSubnetsV4, testSubnetV4_2))
		framework.ExpectNoError(err, "second subnet should be removed from the set after annotation is removed")

		if ipv6Err == nil {
			ginkgo.By("Verifying IPv6 subnet is removed from mgmtport-no-snat-subnets-v6 set")
			err = wait.PollImmediate(retryInterval, retryTimeout, checkNFTablesSetDoesNotContainElement(
				nodeName, types.NFTMgmtPortNoSNATSubnetsV6, testSubnetV6))
			framework.ExpectNoError(err, "IPv6 subnet should be removed after cleanup")
		}
	})

	ginkgo.It("should preserve pod source IP when destination is in SNAT exclude list", func() {
		const (
			externalContainerName = "snat-test-server"
			testPodName           = "snat-test-client"
		)

		// Create infrastructure provider context for external container management
		providerCtx := infraprovider.Get().NewTestContext()

		ginkgo.By("Creating external container running agnhost netexec")
		primaryNetwork, err := infraprovider.Get().PrimaryNetwork()
		framework.ExpectNoError(err, "failed to get primary network")
		port := infraprovider.Get().GetExternalContainerPort()

		externalContainer := infraapi.ExternalContainer{
			Name:    externalContainerName,
			Image:   images.AgnHost(),
			Network: primaryNetwork,
			CmdArgs: []string{"netexec", fmt.Sprintf("--http-port=%d", port)},
			ExtPort: port,
		}
		externalContainer, err = providerCtx.CreateExternalContainer(externalContainer)
		framework.ExpectNoError(err, "failed to create external container")
		framework.Logf("External container created with IPv4: %s, port: %d", externalContainer.GetIPv4(), port)

		externalIP := externalContainer.GetIPv4()
		if externalIP == "" {
			e2eskipper.Skipf("External container has no IPv4 address")
		}

		ginkgo.By(fmt.Sprintf("Adding external container IP %s/32 to SNAT exclude annotation", externalIP))
		err = setNodeSNATExcludeSubnetsAnnotation(cs, nodeName, []string{externalIP + "/32"})
		framework.ExpectNoError(err, "failed to set SNAT exclude subnets annotation")

		ginkgo.By("Waiting for nftables set to be updated")
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNFTablesSetContainsElement(
			nodeName, types.NFTMgmtPortNoSNATSubnetsV4, externalIP))
		framework.ExpectNoError(err, "external container IP should be in mgmtport-no-snat-subnets-v4 set")

		ginkgo.By("Creating test pod on the same node")
		pod := e2epod.NewAgnhostPod(f.Namespace.Name, testPodName, nil, nil, nil)
		pod.Spec.NodeName = nodeName
		pod, err = cs.CoreV1().Pods(f.Namespace.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
		framework.ExpectNoError(err, "failed to create test pod")

		err = e2epod.WaitForPodRunningInNamespace(context.TODO(), cs, pod)
		framework.ExpectNoError(err, "test pod did not reach Running state")

		// Refresh pod to get IP
		pod, err = cs.CoreV1().Pods(f.Namespace.Name).Get(context.TODO(), testPodName, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get test pod")
		podIP := pod.Status.PodIP
		framework.Logf("Test pod IP: %s", podIP)

		// Get node IP to verify we're NOT seeing it (and to set up return route)
		node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get node")
		var nodeIP string
		for _, addr := range node.Status.Addresses {
			if addr.Type == "InternalIP" {
				nodeIP = addr.Address
				break
			}
		}
		framework.Logf("Node IP: %s", nodeIP)

		// Add route on external container for return traffic to pod
		// Since we're bypassing SNAT, the external container will see the pod IP as source
		// and needs to know how to route the response back via the node
		ginkgo.By(fmt.Sprintf("Adding route on external container for return traffic: %s via %s", podIP, nodeIP))
		_, err = infraprovider.Get().ExecExternalContainerCommand(externalContainer,
			[]string{"ip", "route", "add", podIP + "/32", "via", nodeIP})
		framework.ExpectNoError(err, "failed to add return route on external container")

		ginkgo.By("Curling external container from pod to verify source IP")
		// agnhost netexec /clientip returns the client IP:port, e.g., "10.244.0.5:54321"
		curlCmd := fmt.Sprintf("curl -s --max-time 10 http://%s:%d/clientip", externalIP, port)

		var sourceIP string
		gomega.Eventually(func() bool {
			stdout, stderr, err := e2epod.ExecShellInPodWithFullOutput(context.TODO(), f, testPodName, curlCmd)
			if err != nil {
				framework.Logf("curl failed: %v, stderr: %s", err, stderr)
				return false
			}
			framework.Logf("External container saw source: %s", stdout)
			// Output format is "IP:port", extract just the IP
			sourceIP = strings.Split(strings.TrimSpace(stdout), ":")[0]
			return sourceIP != ""
		}, 60*time.Second, 2*time.Second).Should(gomega.BeTrue(), "should be able to curl external container")

		ginkgo.By("Verifying source IP is pod IP, not node IP")
		framework.Logf("Source IP seen by external container: %s", sourceIP)
		framework.Logf("Expected pod IP: %s", podIP)
		framework.Logf("Should NOT be node IP: %s", nodeIP)

		gomega.Expect(sourceIP).To(gomega.Equal(podIP),
			fmt.Sprintf("source IP should be pod IP (%s), not node IP (%s)", podIP, nodeIP))
		gomega.Expect(sourceIP).NotTo(gomega.Equal(nodeIP),
			"source IP should NOT be node IP (SNAT should be bypassed)")

		ginkgo.By("Cleaning up test pod")
		err = cs.CoreV1().Pods(f.Namespace.Name).Delete(context.TODO(), testPodName, metav1.DeleteOptions{})
		framework.ExpectNoError(err, "failed to delete test pod")
	})
})
