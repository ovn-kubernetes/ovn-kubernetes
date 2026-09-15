// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	corev1 "k8s.io/api/core/v1"
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

func setNodeAnnotation(cs clientset.Interface, nodeName, annotationKey string, subnets []string) error {
	subnetsJSON, err := json.Marshal(subnets)
	if err != nil {
		return fmt.Errorf("failed to marshal subnets: %w", err)
	}

	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"annotations": map[string]string{
				annotationKey: string(subnetsJSON),
			},
		},
	}

	patchData, err := json.Marshal(&patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	_, err = cs.CoreV1().Nodes().Patch(context.Background(), nodeName, k8stypes.MergePatchType, patchData, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to patch node %s: %w", nodeName, err)
	}

	return nil
}

func setNodeSNATExcludeSubnetsAnnotation(cs clientset.Interface, nodeName string, subnets []string) error {
	return setNodeAnnotation(cs, nodeName, util.OvnNodeSNATExcludeSubnets, subnets)
}

func removeNodeSNATExcludeSubnetsAnnotation(cs clientset.Interface, nodeName string) error {
	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"annotations": map[string]*string{
				util.OvnNodeSNATExcludeSubnets: nil,
				util.OvnNodeDontSNATSubnets:    nil,
			},
		},
	}

	patchData, err := json.Marshal(&patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	_, err = cs.CoreV1().Nodes().Patch(context.Background(), nodeName, k8stypes.MergePatchType, patchData, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to patch node %s: %w", nodeName, err)
	}

	return nil
}

func getNodeAnnotation(cs clientset.Interface, nodeName, annotation string) (string, bool, error) {
	node, err := cs.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		return "", false, fmt.Errorf("failed to get node %s: %w", nodeName, err)
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

func checkNFTablesSetContainsElement(nodeName, setName, element string) wait.ConditionWithContextFunc {
	return func(_ context.Context) (bool, error) {
		output, err := getNFTablesSetElements(nodeName, setName)
		if err != nil {
			return false, fmt.Errorf("failed to list set %s on node %s: %w", setName, nodeName, err)
		}
		contains := strings.Contains(output, element)
		if !contains {
			framework.Logf("Set %s does not contain element %q. Current elements:\n%s", setName, element, output)
		}
		return contains, nil
	}
}

func checkNFTablesSetDoesNotContainElement(nodeName, setName, element string) wait.ConditionWithContextFunc {
	containsFunc := checkNFTablesSetContainsElement(nodeName, setName, element)
	return func(ctx context.Context) (bool, error) {
		contains, err := containsFunc(ctx)
		if err != nil {
			return false, fmt.Errorf("checking set %s on node %s does not contain %s: %w", setName, nodeName, element, err)
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

		node, err := e2enode.GetRandomReadySchedulableNode(context.Background(), cs)
		framework.ExpectNoError(err, "failed to get a schedulable node")
		nodeName = node.Name

		framework.Logf("Using node %s for local gateway SNAT test", nodeName)
		err = removeNodeSNATExcludeSubnetsAnnotation(cs, nodeName)
		if err != nil {
			framework.Logf("Note: failed to remove existing SNAT exclude subnets annotation (may not exist): %v", err)
		}
		if isIPv4Supported(cs) {
			err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, checkNFTablesSetDoesNotContainElement(
				nodeName, types.NFTMgmtPortNoSNATSubnetsV4, testSubnetV4_1))
			framework.ExpectNoError(err, "timed out waiting for %s to be removed from %s — test environment is dirty", testSubnetV4_1, types.NFTMgmtPortNoSNATSubnetsV4)
			err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, checkNFTablesSetDoesNotContainElement(
				nodeName, types.NFTMgmtPortNoSNATSubnetsV4, testSubnetV4_2))
			framework.ExpectNoError(err, "timed out waiting for %s to be removed from %s — test environment is dirty", testSubnetV4_2, types.NFTMgmtPortNoSNATSubnetsV4)
		}
	})

	ginkgo.AfterEach(func() {
		if nodeName != "" {
			err := removeNodeSNATExcludeSubnetsAnnotation(cs, nodeName)
			if err != nil {
				framework.Logf("Warning: failed to remove SNAT exclude subnets annotation: %v", err)
			}
		}
	})

	// TODO: add a traffic-level ingress SNAT test for shared gateway mode.
	// In shared GW, the pod's TCP response is SNATed by OVN GR independently
	// of the mgmtport-snat chain, requiring OVN LRP configuration to make
	// a full round-trip test work. The structural tests (nftables rules and
	// sets populated) cover shared GW regression in the meantime.
	ginkgo.It("should have return rules for no-snat-subnets in ovn-kube-pod-subnet-masq chain", func() {
		ginkgo.By("Verifying the ovn-kube-pod-subnet-masq chain exists")
		var chainRules string
		err := wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, func(_ context.Context) (bool, error) {
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

		if isIPv4Supported(cs) {
			ginkgo.By("Verifying the chain has return rule referencing mgmtport-no-snat-subnets-v4 set")
			gomega.Expect(chainRules).To(gomega.MatchRegexp(`ip\s+daddr\s+@mgmtport-no-snat-subnets-v4\s+return`),
				"chain should have 'ip daddr @mgmtport-no-snat-subnets-v4 return' rule")
		}

		if isIPv6Supported(cs) {
			ginkgo.By("Verifying the chain has return rule referencing mgmtport-no-snat-subnets-v6 set")
			gomega.Expect(chainRules).To(gomega.MatchRegexp(`ip6\s+daddr\s+@mgmtport-no-snat-subnets-v6\s+return`),
				"chain should have 'ip6 daddr @mgmtport-no-snat-subnets-v6 return' rule")
		}
	})

	ginkgo.It("should populate nftables sets when SNAT exclude annotation is added", func() {
		if !isIPv4Supported(cs) {
			e2eskipper.Skipf("Skipping IPv4 set population test - IPv4 not supported on this cluster")
		}

		ginkgo.By("Verifying mgmtport-no-snat-subnets-v4 set exists and test subnets are not present")
		var setElements string
		err := wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, func(_ context.Context) (bool, error) {
			var err error
			setElements, err = getNFTablesSetElements(nodeName, types.NFTMgmtPortNoSNATSubnetsV4)
			return err == nil, nil
		})
		framework.ExpectNoError(err, "mgmtport-no-snat-subnets-v4 set should exist")
		gomega.Expect(setElements).NotTo(gomega.ContainSubstring(testSubnetV4_1),
			"first test subnet should not be in set initially")
		gomega.Expect(setElements).NotTo(gomega.ContainSubstring(testSubnetV4_2),
			"second test subnet should not be in set initially")

		ginkgo.By(fmt.Sprintf("Adding single SNAT exclude subnet annotation with %s", testSubnetV4_1))
		err = setNodeSNATExcludeSubnetsAnnotation(cs, nodeName, []string{testSubnetV4_1})
		framework.ExpectNoError(err, "failed to set SNAT exclude subnets annotation")

		ginkgo.By("Verifying annotation was set on the node")
		err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, func(_ context.Context) (bool, error) {
			value, ok, err := getNodeAnnotation(cs, nodeName, util.OvnNodeSNATExcludeSubnets)
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
		err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, checkNFTablesSetContainsElement(
			nodeName, types.NFTMgmtPortNoSNATSubnetsV4, testSubnetV4_1))
		framework.ExpectNoError(err, "excluded subnet should be in mgmtport-no-snat-subnets-v4 set after annotation is added")

		ginkgo.By(fmt.Sprintf("Updating annotation to include multiple subnets: %s, %s", testSubnetV4_1, testSubnetV4_2))
		err = setNodeSNATExcludeSubnetsAnnotation(cs, nodeName, []string{testSubnetV4_1, testSubnetV4_2})
		framework.ExpectNoError(err, "failed to update SNAT exclude subnets annotation")

		ginkgo.By("Verifying both subnets are in the set")
		err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, checkNFTablesSetContainsElement(
			nodeName, types.NFTMgmtPortNoSNATSubnetsV4, testSubnetV4_1))
		framework.ExpectNoError(err, "first subnet should be in set")
		err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, checkNFTablesSetContainsElement(
			nodeName, types.NFTMgmtPortNoSNATSubnetsV4, testSubnetV4_2))
		framework.ExpectNoError(err, "second subnet should be in set")

		ipv6Supported := isIPv6Supported(cs)
		if ipv6Supported {
			ginkgo.By("Verifying test IPv6 subnet is NOT in the set initially")
			setElementsV6, err := getNFTablesSetElements(nodeName, types.NFTMgmtPortNoSNATSubnetsV6)
			framework.ExpectNoError(err, "should be able to list IPv6 set elements")
			gomega.Expect(setElementsV6).NotTo(gomega.ContainSubstring(testSubnetV6),
				"test IPv6 subnet should NOT be in set initially")

			ginkgo.By(fmt.Sprintf("Adding IPv6 subnet to annotation: %s, %s, %s", testSubnetV4_1, testSubnetV4_2, testSubnetV6))
			err = setNodeSNATExcludeSubnetsAnnotation(cs, nodeName, []string{testSubnetV4_1, testSubnetV4_2, testSubnetV6})
			framework.ExpectNoError(err, "failed to set SNAT exclude subnets annotation with IPv6")

			ginkgo.By("Verifying IPv6 subnet is in the v6 set")
			err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, checkNFTablesSetContainsElement(
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
			framework.Logf("Skipping IPv6 portion - IPv6 not supported on this cluster")
		}

		ginkgo.By("Removing the SNAT exclude subnet annotation")
		err = removeNodeSNATExcludeSubnetsAnnotation(cs, nodeName)
		framework.ExpectNoError(err, "failed to remove SNAT exclude subnets annotation")

		ginkgo.By("Verifying annotation was removed from the node")
		err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, func(_ context.Context) (bool, error) {
			_, ok, err := getNodeAnnotation(cs, nodeName, util.OvnNodeSNATExcludeSubnets)
			if err != nil {
				return false, nil
			}
			return !ok, nil
		})
		framework.ExpectNoError(err, "annotation should be removed from the node")

		ginkgo.By("Verifying all IPv4 subnets are removed from mgmtport-no-snat-subnets-v4 set")
		err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, checkNFTablesSetDoesNotContainElement(
			nodeName, types.NFTMgmtPortNoSNATSubnetsV4, testSubnetV4_1))
		framework.ExpectNoError(err, "first subnet should be removed from the set after annotation is removed")
		err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, checkNFTablesSetDoesNotContainElement(
			nodeName, types.NFTMgmtPortNoSNATSubnetsV4, testSubnetV4_2))
		framework.ExpectNoError(err, "second subnet should be removed from the set after annotation is removed")

		if ipv6Supported {
			ginkgo.By("Verifying IPv6 subnet is removed from mgmtport-no-snat-subnets-v6 set")
			err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, checkNFTablesSetDoesNotContainElement(
				nodeName, types.NFTMgmtPortNoSNATSubnetsV6, testSubnetV6))
			framework.ExpectNoError(err, "IPv6 subnet should be removed after cleanup")
		}
	})

	// TODO: remove this test when k8s.ovn.org/node-ingress-snat-exclude-subnets is removed.
	ginkgo.It("should populate nftables sets when deprecated node-ingress-snat-exclude-subnets annotation is used", func() {
		if !isIPv4Supported(cs) {
			e2eskipper.Skipf("Skipping IPv4 set population test - IPv4 not supported on this cluster")
		}
		ginkgo.By(fmt.Sprintf("Setting the deprecated annotation with subnet %s", testSubnetV4_1))
		err := setNodeAnnotation(cs, nodeName, util.OvnNodeDontSNATSubnets, []string{testSubnetV4_1})
		framework.ExpectNoError(err, "failed to set deprecated SNAT exclude subnets annotation")

		ginkgo.By("Verifying subnet is populated in mgmtport-no-snat-subnets-v4 set")
		err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
			checkNFTablesSetContainsElement(nodeName, types.NFTMgmtPortNoSNATSubnetsV4, testSubnetV4_1))
		framework.ExpectNoError(err, "deprecated annotation should still populate the nftables set")
	})

	// TODO: remove this test when k8s.ovn.org/node-ingress-snat-exclude-subnets is removed.
	ginkgo.It("should merge subnets from both old and new annotations into the nftables set", func() {
		if !isIPv4Supported(cs) {
			e2eskipper.Skipf("Skipping IPv4 set population test - IPv4 not supported on this cluster")
		}
		ginkgo.By(fmt.Sprintf("Setting new annotation with %s", testSubnetV4_1))
		err := setNodeSNATExcludeSubnetsAnnotation(cs, nodeName, []string{testSubnetV4_1})
		framework.ExpectNoError(err, "failed to set new SNAT exclude subnets annotation")

		ginkgo.By(fmt.Sprintf("Setting deprecated annotation with %s", testSubnetV4_2))
		err = setNodeAnnotation(cs, nodeName, util.OvnNodeDontSNATSubnets, []string{testSubnetV4_2})
		framework.ExpectNoError(err, "failed to set deprecated SNAT exclude subnets annotation")

		ginkgo.By("Verifying both subnets are in the nftables set")
		err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
			checkNFTablesSetContainsElement(nodeName, types.NFTMgmtPortNoSNATSubnetsV4, testSubnetV4_1))
		framework.ExpectNoError(err, "subnet from new annotation should be in the nftables set")
		err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
			checkNFTablesSetContainsElement(nodeName, types.NFTMgmtPortNoSNATSubnetsV4, testSubnetV4_2))
		framework.ExpectNoError(err, "subnet from deprecated annotation should be in the nftables set")
	})

	ginkgo.DescribeTable("should preserve pod source IP when destination is in SNAT exclude list",
		func(protocol corev1.IPFamily) {
			const (
				externalContainerName = "snat-test-server"
				testPodName           = "snat-test-client"
			)
			if protocol == corev1.IPv4Protocol && !isIPv4Supported(cs) {
				e2eskipper.Skipf("Skipping IPv4 test - IPv4 not supported on this cluster")
			}
			if protocol == corev1.IPv6Protocol && !isIPv6Supported(cs) {
				e2eskipper.Skipf("Skipping IPv6 test - IPv6 not supported on this cluster")
			}

			isIPv6 := protocol == corev1.IPv6Protocol
			prefix, noSNATSet, ipRouteCmd := "/32", types.NFTMgmtPortNoSNATSubnetsV4, "ip"
			if isIPv6 {
				prefix, noSNATSet, ipRouteCmd = "/128", types.NFTMgmtPortNoSNATSubnetsV6, "ip -6"
			}

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

			externalIP := externalContainer.GetIPv4()
			if isIPv6 {
				externalIP = externalContainer.GetIPv6()
			}
			if externalIP == "" {
				e2eskipper.Skipf("External container has no %s address", protocol)
			}
			framework.Logf("External container IP: %s, port: %d", externalIP, port)

			ginkgo.By("Creating test pod on the same node")
			pod := e2epod.NewAgnhostPod(f.Namespace.Name, testPodName, nil, nil, nil)
			pod.Spec.NodeName = nodeName
			pod, err = cs.CoreV1().Pods(f.Namespace.Name).Create(context.Background(), pod, metav1.CreateOptions{})
			framework.ExpectNoError(err, "failed to create test pod")
			ginkgo.DeferCleanup(func() {
				err := cs.CoreV1().Pods(f.Namespace.Name).Delete(context.Background(), testPodName, metav1.DeleteOptions{})
				framework.ExpectNoError(err, "failed to delete test pod")
			})

			err = e2epod.WaitForPodRunningInNamespace(context.Background(), cs, pod)
			framework.ExpectNoError(err, "test pod did not reach Running state")

			pod, err = cs.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), testPodName, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to get test pod")
			podIP := pod.Status.PodIP
			framework.Logf("Test pod IP: %s", podIP)

			// Select node IP matching the pod IP family to avoid mismatches on dual-stack clusters.
			node, err := cs.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to get node")
			podIsIPv6 := net.ParseIP(podIP) != nil && net.ParseIP(podIP).To4() == nil
			var nodeIP string
			for _, addr := range node.Status.Addresses {
				if addr.Type == corev1.NodeInternalIP {
					addrIsIPv6 := net.ParseIP(addr.Address) != nil && net.ParseIP(addr.Address).To4() == nil
					if addrIsIPv6 == podIsIPv6 {
						nodeIP = addr.Address
						break
					}
				}
			}
			framework.Logf("Node IP: %s", nodeIP)

			// IPv6 addresses need brackets in URLs.
			curlTarget := externalIP
			if isIPv6 {
				curlTarget = fmt.Sprintf("[%s]", externalIP)
			}
			curlCmd := fmt.Sprintf("curl -s --max-time 10 http://%s:%d/clientip", curlTarget, port)

			ginkgo.By("Verifying baseline connectivity from pod to external container before applying annotation")
			gomega.Eventually(func() bool {
				stdout, stderr, err := e2epod.ExecShellInPodWithFullOutput(context.Background(), f, testPodName, curlCmd)
				if err != nil {
					framework.Logf("baseline curl failed: %v, stderr: %s", err, stderr)
					return false
				}
				framework.Logf("baseline connectivity ok, response: %s", stdout)
				return true
			}, 60*time.Second, 2*time.Second).Should(gomega.BeTrue(), "pod should be able to reach external container before annotation is set")

			ginkgo.By(fmt.Sprintf("Adding external container IP %s%s to SNAT exclude annotation", externalIP, prefix))
			err = setNodeSNATExcludeSubnetsAnnotation(cs, nodeName, []string{externalIP + prefix})
			framework.ExpectNoError(err, "failed to set SNAT exclude subnets annotation")

			ginkgo.By("Waiting for nftables set to be updated")
			err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
				checkNFTablesSetContainsElement(nodeName, noSNATSet, externalIP))
			framework.ExpectNoError(err, "external container IP should be in %s set", noSNATSet)

			// Since we're bypassing SNAT, the external container will see the pod IP as source
			// and needs a route back via the node.
			podPrefix := "/32"
			if podIsIPv6 {
				podPrefix = "/128"
			}
			ginkgo.By(fmt.Sprintf("Adding route on external container for return traffic: %s via %s", podIP, nodeIP))
			_, err = infraprovider.Get().ExecExternalContainerCommand(externalContainer,
				[]string{"sh", "-c", fmt.Sprintf("%s route add %s via %s", ipRouteCmd, podIP+podPrefix, nodeIP)})
			framework.ExpectNoError(err, "failed to add return route on external container")

			ginkgo.By("Verifying source IP is pod IP, not node IP")
			gomega.Eventually(func() bool {
				stdout, stderr, err := e2epod.ExecShellInPodWithFullOutput(context.Background(), f, testPodName, curlCmd)
				if err != nil {
					framework.Logf("curl failed: %v, stderr: %s", err, stderr)
					return false
				}
				framework.Logf("External container saw source: %s", stdout)
				// Output format is "IP:port" — use SplitHostPort to handle IPv6 addresses correctly
				host, _, err := net.SplitHostPort(strings.TrimSpace(stdout))
				if err != nil {
					framework.Logf("Failed to parse client IP %q: %v", stdout, err)
					return false
				}
				framework.Logf("Source IP: %s, expected pod IP: %s, node IP: %s", host, podIP, nodeIP)
				return host == podIP
			}, 60*time.Second, 2*time.Second).Should(gomega.BeTrue(),
				fmt.Sprintf("source IP should be pod IP (%s), not node IP (%s)", podIP, nodeIP))
		},
		ginkgo.Entry("ipv4", corev1.IPv4Protocol),
		ginkgo.Entry("ipv6", corev1.IPv6Protocol),
	)

	// In local GW mode the same annotation bypasses both ingress SNAT (mgmtport-snat) and
	// egress SNAT (ovn-kube-pod-subnet-masq), so the TCP response from the pod can reach
	// the external container without being re-SNATed. This symmetry does not hold in shared
	// GW mode where the OVN GR SNATs the response path, so the test is local-GW-only.
	ginkgo.It("should preserve external source IP in ingress traffic when source is in SNAT exclude list", func() {
		const (
			externalContainerName = "snat-ingress-test-client"
			serverPodName         = "snat-ingress-test-server"
		)

		if !isIPv4Supported(cs) {
			e2eskipper.Skipf("Skipping ingress source IP preservation test - IPv4 not supported on this cluster")
		}

		providerCtx := infraprovider.Get().NewTestContext()

		ginkgo.By("Creating external container as traffic source")
		primaryNetwork, err := infraprovider.Get().PrimaryNetwork()
		framework.ExpectNoError(err, "failed to get primary network")
		port := infraprovider.Get().GetExternalContainerPort()

		externalContainer := infraapi.ExternalContainer{
			Name:    externalContainerName,
			Image:   images.AgnHost(),
			Network: primaryNetwork,
			CmdArgs: []string{"pause"},
			ExtPort: port,
		}
		externalContainer, err = providerCtx.CreateExternalContainer(externalContainer)
		framework.ExpectNoError(err, "failed to create external container")

		externalIP := externalContainer.GetIPv4()
		if externalIP == "" {
			e2eskipper.Skipf("External container has no IPv4 address")
		}
		framework.Logf("External container IP: %s", externalIP)

		ginkgo.By("Creating server pod running agnhost netexec")
		serverPod := e2epod.NewAgnhostPod(f.Namespace.Name, serverPodName, nil, nil, nil, "netexec", fmt.Sprintf("--http-port=%d", port))
		serverPod.Spec.NodeName = nodeName
		serverPod, err = cs.CoreV1().Pods(f.Namespace.Name).Create(context.Background(), serverPod, metav1.CreateOptions{})
		framework.ExpectNoError(err, "failed to create server pod")
		ginkgo.DeferCleanup(func() {
			err := cs.CoreV1().Pods(f.Namespace.Name).Delete(context.Background(), serverPodName, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "failed to delete server pod")
		})

		err = e2epod.WaitForPodRunningInNamespace(context.Background(), cs, serverPod)
		framework.ExpectNoError(err, "server pod did not reach Running state")

		serverPod, err = cs.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), serverPodName, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get server pod")
		podIP := serverPod.Status.PodIP
		framework.Logf("Server pod IP: %s", podIP)

		node, err := cs.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get node")
		var nodeIP string
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				if net.ParseIP(addr.Address).To4() != nil {
					nodeIP = addr.Address
					break
				}
			}
		}
		framework.Logf("Node IP: %s", nodeIP)

		ginkgo.By(fmt.Sprintf("Adding route on external container to reach pod IP %s via node %s", podIP, nodeIP))
		_, err = infraprovider.Get().ExecExternalContainerCommand(externalContainer,
			[]string{"ip", "route", "add", podIP + "/32", "via", nodeIP})
		framework.ExpectNoError(err, "failed to add route to pod on external container")

		curlCmd := fmt.Sprintf("curl -s --max-time 10 http://%s:%d/clientip", podIP, port)

		ginkgo.By("Verifying baseline connectivity from external container to server pod before applying annotation")
		gomega.Eventually(func() bool {
			stdout, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer,
				[]string{"sh", "-c", curlCmd})
			if err != nil {
				framework.Logf("baseline curl failed: %v", err)
				return false
			}
			framework.Logf("baseline connectivity ok, response: %s", stdout)
			return true
		}, 60*time.Second, 2*time.Second).Should(gomega.BeTrue(), "external container should be able to reach server pod")

		ginkgo.By(fmt.Sprintf("Adding external container IP %s/32 to SNAT exclude annotation", externalIP))
		err = setNodeSNATExcludeSubnetsAnnotation(cs, nodeName, []string{externalIP + "/32"})
		framework.ExpectNoError(err, "failed to set SNAT exclude subnets annotation")

		ginkgo.By("Waiting for nftables set to be updated")
		err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
			checkNFTablesSetContainsElement(nodeName, types.NFTMgmtPortNoSNATSubnetsV4, externalIP))
		framework.ExpectNoError(err, "external container IP should be in mgmtport-no-snat-subnets-v4 set")

		ginkgo.By("Verifying server pod sees external container's original source IP, not the mgmt port IP")
		gomega.Eventually(func() bool {
			stdout, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer,
				[]string{"sh", "-c", curlCmd})
			if err != nil {
				framework.Logf("curl failed: %v", err)
				return false
			}
			framework.Logf("Server pod saw source: %s", stdout)
			host, _, err := net.SplitHostPort(strings.TrimSpace(stdout))
			if err != nil {
				framework.Logf("Failed to parse client IP %q: %v", stdout, err)
				return false
			}
			framework.Logf("Source IP seen by pod: %s, expected: %s", host, externalIP)
			return host == externalIP
		}, 60*time.Second, 2*time.Second).Should(gomega.BeTrue(),
			fmt.Sprintf("pod should see external container IP (%s) as source, not mgmt port IP", externalIP))
	})
})

