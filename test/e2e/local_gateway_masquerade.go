package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

const (
	ovnKubePodSubnetMasqChain = "ovn-kube-pod-subnet-masq"
	mgmtportNoSNATSubnetsV4   = "mgmtport-no-snat-subnets-v4"
	mgmtportNoSNATSubnetsV6   = "mgmtport-no-snat-subnets-v6"

	ovnNodeDontSNATSubnetsAnnotation = "k8s.ovn.org/node-ingress-snat-exclude-subnets"
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
				ovnNodeDontSNATSubnetsAnnotation: string(subnetsJSON),
			},
		},
	}

	patchData, err := json.Marshal(&patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	_, err = cs.CoreV1().Nodes().Patch(context.TODO(), nodeName, types.MergePatchType, patchData, metav1.PatchOptions{})
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
				ovnNodeDontSNATSubnetsAnnotation: nil,
			},
		},
	}

	patchData, err := json.Marshal(&patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	_, err = cs.CoreV1().Nodes().Patch(context.TODO(), nodeName, types.MergePatchType, patchData, metav1.PatchOptions{})
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
			nodeName, mgmtportNoSNATSubnetsV4, testSubnetV4_1))
		_ = wait.PollImmediate(retryInterval, retryTimeout, checkNFTablesSetDoesNotContainElement(
			nodeName, mgmtportNoSNATSubnetsV4, testSubnetV4_2))
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

	ginkgo.It("should populate mgmtport-no-snat-subnets-v4 set when annotation is added", func() {
		ginkgo.By("Verifying mgmtport-no-snat-subnets-v4 set exists")
		var setElements string
		err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			var err error
			setElements, err = getNFTablesSetElements(nodeName, mgmtportNoSNATSubnetsV4)
			return err == nil, nil
		})
		framework.ExpectNoError(err, "mgmtport-no-snat-subnets-v4 set should exist")
		ginkgo.By("Verifying test subnet is not in the set before adding annotation")
		gomega.Expect(setElements).NotTo(gomega.ContainSubstring(testSubnetV4_1),
			"test subnet should not be in set before annotation is added")

		ginkgo.By(fmt.Sprintf("Adding SNAT exclude subnet annotation with %s", testSubnetV4_1))
		err = setNodeSNATExcludeSubnetsAnnotation(cs, nodeName, []string{testSubnetV4_1})
		framework.ExpectNoError(err, "failed to set SNAT exclude subnets annotation")

		ginkgo.By("Verifying annotation was set on the node")
		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			value, ok, err := getNodeAnnotation(cs, nodeName, ovnNodeDontSNATSubnetsAnnotation)
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
			nodeName, mgmtportNoSNATSubnetsV4, testSubnetV4_1))
		framework.ExpectNoError(err, "excluded subnet should be in mgmtport-no-snat-subnets-v4 set after annotation is added")

		ginkgo.By("Removing the SNAT exclude subnet annotation")
		err = removeNodeSNATExcludeSubnetsAnnotation(cs, nodeName)
		framework.ExpectNoError(err, "failed to remove SNAT exclude subnets annotation")

		ginkgo.By("Verifying annotation was removed from the node")
		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			_, ok, err := getNodeAnnotation(cs, nodeName, ovnNodeDontSNATSubnetsAnnotation)
			if err != nil {
				return false, nil
			}
			return !ok, nil
		})
		framework.ExpectNoError(err, "annotation should be removed from the node")

		ginkgo.By("Verifying the excluded subnet is removed from mgmtport-no-snat-subnets-v4 set")
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNFTablesSetDoesNotContainElement(
			nodeName, mgmtportNoSNATSubnetsV4, testSubnetV4_1))
		framework.ExpectNoError(err, "excluded subnet should be removed from the set after annotation is removed")
	})

	ginkgo.It("should handle multiple subnets in annotation", func() {
		ginkgo.By("Verifying test subnets are NOT in the set")
		setElements, err := getNFTablesSetElements(nodeName, mgmtportNoSNATSubnetsV4)
		framework.ExpectNoError(err, "should be able to list set elements")
		gomega.Expect(setElements).NotTo(gomega.ContainSubstring(testSubnetV4_1),
			"first test subnet should NOT be in set initially")
		gomega.Expect(setElements).NotTo(gomega.ContainSubstring(testSubnetV4_2),
			"second test subnet should NOT be in set initially")

		ginkgo.By(fmt.Sprintf("Adding multiple subnets to annotation: %s, %s", testSubnetV4_1, testSubnetV4_2))
		err = setNodeSNATExcludeSubnetsAnnotation(cs, nodeName, []string{testSubnetV4_1, testSubnetV4_2})
		framework.ExpectNoError(err, "failed to set SNAT exclude subnets annotation")

		ginkgo.By("Verifying both subnets are in the set")
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNFTablesSetContainsElement(
			nodeName, mgmtportNoSNATSubnetsV4, testSubnetV4_1))
		framework.ExpectNoError(err, "first subnet should be in set")
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNFTablesSetContainsElement(
			nodeName, mgmtportNoSNATSubnetsV4, testSubnetV4_2))
		framework.ExpectNoError(err, "second subnet should be in set")

		ginkgo.By("Cleaning up: removing annotation")
		err = removeNodeSNATExcludeSubnetsAnnotation(cs, nodeName)
		framework.ExpectNoError(err, "failed to remove annotation")

		ginkgo.By("Verifying all subnets are removed")
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNFTablesSetDoesNotContainElement(
			nodeName, mgmtportNoSNATSubnetsV4, testSubnetV4_1))
		framework.ExpectNoError(err, "first subnet should be removed after cleanup")
	})

	ginkgo.It("should handle IPv6 subnets in dual-stack clusters", func() {
		ginkgo.By("Checking if IPv6 set exists (indicates dual-stack)")
		_, err := getNFTablesSetElements(nodeName, mgmtportNoSNATSubnetsV6)
		if err != nil {
			e2eskipper.Skipf("Skipping IPv6 test - mgmtport-no-snat-subnets-v6 set does not exist (IPv4-only cluster)")
		}

		ginkgo.By("Verifying test IPv6 subnet is NOT in the set")
		setElements, err := getNFTablesSetElements(nodeName, mgmtportNoSNATSubnetsV6)
		framework.ExpectNoError(err, "should be able to list IPv6 set elements")
		gomega.Expect(setElements).NotTo(gomega.ContainSubstring(testSubnetV6),
			"test IPv6 subnet should NOT be in set initially")

		ginkgo.By(fmt.Sprintf("Adding IPv6 subnet to annotation: %s", testSubnetV6))
		err = setNodeSNATExcludeSubnetsAnnotation(cs, nodeName, []string{testSubnetV6})
		framework.ExpectNoError(err, "failed to set SNAT exclude subnets annotation with IPv6")

		ginkgo.By("Verifying IPv6 subnet is in the v6 set")
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNFTablesSetContainsElement(
			nodeName, mgmtportNoSNATSubnetsV6, testSubnetV6))
		framework.ExpectNoError(err, "IPv6 subnet should be in mgmtport-no-snat-subnets-v6 set")

		ginkgo.By("Verifying IPv6 subnet is NOT in the v4 set")
		setElementsV4, err := getNFTablesSetElements(nodeName, mgmtportNoSNATSubnetsV4)
		framework.ExpectNoError(err, "should be able to list v4 set elements")
		gomega.Expect(setElementsV4).NotTo(gomega.ContainSubstring(testSubnetV6),
			"IPv6 subnet should NOT be in v4 set")

		ginkgo.By("Cleaning up: removing annotation")
		err = removeNodeSNATExcludeSubnetsAnnotation(cs, nodeName)
		framework.ExpectNoError(err, "failed to remove annotation")

		ginkgo.By("Verifying IPv6 subnet is removed from the set")
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNFTablesSetDoesNotContainElement(
			nodeName, mgmtportNoSNATSubnetsV6, testSubnetV6))
		framework.ExpectNoError(err, "IPv6 subnet should be removed after cleanup")
	})
})
