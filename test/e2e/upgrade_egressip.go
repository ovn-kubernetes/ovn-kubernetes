package e2e

//
// Baseline EgressIP upgrade test
//
// Verifies that EgressIP works correctly through a rolling upgrade of
// ovnkube-node pods. The upgrade is triggered by patching the DaemonSet
// image via the Kubernetes API.
//
// Two modes depending on OVN_IMAGE env var:
//
//   Real upgrade (CI):
//     OVN_IMAGE is set to the PR image by the CI job.
//     DaemonSet is patched to that image → real version change → rollout.
//
//   Simulated upgrade (local KIND):
//     OVN_IMAGE is not set — the current DaemonSet image is read and reused.
//     Exercises rollout mechanics without a real version change.
//
// Test flow:
//   PRE-UPGRADE
//     0. Label node as egress-assignable
//     1. Create EgressIP object
//     2. Create two test pods — one on egress node, one on a different node
//     3. Verify both pods traffic uses EgressIP as source IP
//
//   UPGRADE (via Kubernetes API)
//     4. Read OVN_IMAGE from env (or use current image if not set)
//     5. Record original image and maxUnavailable for revert after upgrade
//     6. Set maxUnavailable=1 to ensure standard rolling update behaviour
//     7. Start traffic polling goroutine to monitor EgressIP during rollout
//     8. Patch ovnkube-node DaemonSet with new image
//     9. Wait for DaemonSet rollout to complete
//     10. Stop traffic polling — log failure summary
//
//   POST-UPGRADE
//     11. Verify EgressIP is still assigned
//     12. Verify both pods traffic still uses EgressIP as source IP
//
//   REVERT
//     13. Restore original DaemonSet image and maxUnavailable
//     14. Wait for revert rollout to complete
//
//
// How to run locally:
//   cd test && make control-plane WHAT="EgressIP survives ovnkube-node rolling upgrade"
//
// CI wiring (added to test.yml after the "ovn upgrade" step):
//   make -C test control-plane WHAT="EgressIP survives ovnkube-node rolling upgrade"

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
)

const (
	ovnKubeNodeDSName             = "ovnkube-node"
	ovnUpgradeImageEnvVar         = "OVN_IMAGE"
	ovnUpgradeRolloutTimeout      = 5 * time.Minute
	ovnUpgradeTrafficPollInterval = 5 * time.Second
	ovnUpgradeTrafficCheckTimeout = 3 * time.Second
)

var _ = ginkgo.Describe("e2e EgressIP upgrade validation", feature.EgressIP, func() {

	const (
		upgradeEIPName     = "egressip-upgrade-test"
		upgradeEIPYaml     = "egressip-upgrade-test.yaml"
		upgradePodSameName = "egressip-upgrade-pod-same-node"
		upgradePodDiffName = "egressip-upgrade-pod-diff-node"
		upgradePodPort     = 8080
	)

	podEgressLabel := map[string]string{"wants": "egress"}

	f := wrappedTestFramework("egressip-upgrade")
	f.SkipNamespaceCreation = true

	var (
		providerCtx       infraapi.Context
		externalContainer infraapi.ExternalContainer
		egressNodeName    string
		podNodeName       string
		egressIPStr       string
		ovnKubeNS         string
	)

	ginkgo.BeforeEach(func() {
		providerCtx = infraprovider.Get().NewTestContext()

		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			framework.Failf("EgressIP upgrade test requires >= 2 Ready nodes, got %d", len(nodes.Items))
		}

		ns, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework": f.BaseName,
		})
		framework.ExpectNoError(err)
		f.Namespace = ns

		egressNodeName = nodes.Items[0].Name
		podNodeName = nodes.Items[1].Name

		nodeIP := upgradeEIPGetNodeIP(nodes.Items[0])
		var deriveErr error
		egressIPStr, deriveErr = upgradeEIPDeriveEgressIP(nodeIP)
		gomega.Expect(deriveErr).ShouldNot(gomega.HaveOccurred(), "must derive EgressIP")

		primaryNetwork, err := infraprovider.Get().PrimaryNetwork()
		framework.ExpectNoError(err, "failed to get primary provider network")
		port := infraprovider.Get().GetExternalContainerPort()
		externalContainer, err = providerCtx.CreateExternalContainer(infraapi.ExternalContainer{
			Name:    "upgrade-eip-target",
			Image:   images.AgnHost(),
			Network: primaryNetwork,
			CmdArgs: getAgnHostHTTPPortBindCMDArgs(port),
			ExtPort: port,
		})
		framework.ExpectNoError(err, "failed to create external target container")

		ovnKubeNS = deploymentconfig.Get().OVNKubernetesNamespace()
	})

	ginkgo.AfterEach(func() {
		if egressNodeName != "" {
			unlabelNodeForEgress(f, egressNodeName)
		}
		e2ekubectl.RunKubectlOrDie("default", "delete", "eip", upgradeEIPName, "--ignore-not-found=true")
	})

	ginkgo.It("EgressIP survives ovnkube-node rolling upgrade", func() {

		// ----------------------------------------------------------------
		// PRE-UPGRADE SETUP
		// ----------------------------------------------------------------

		ginkgo.By(fmt.Sprintf("0. Labeling node %s as egress-assignable", egressNodeName))
		labelNodeForEgress(f, egressNodeName)

		ginkgo.By("1. Setting namespace label for EgressIP namespace selector")
		updateNamespaceLabels(f, f.Namespace, map[string]string{"name": f.Namespace.Name})

		ginkgo.By(fmt.Sprintf("2. Creating EgressIP object %s with IP %s", upgradeEIPName, egressIPStr))
		eipConfig := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: %s
spec:
    egressIPs:
    - %s
    podSelector:
        matchLabels:
            wants: egress
    namespaceSelector:
        matchLabels:
            name: %s
`, upgradeEIPName, egressIPStr, f.Namespace.Name)

		if err := os.WriteFile(upgradeEIPYaml, []byte(eipConfig), 0644); err != nil {
			framework.Failf("failed to write EgressIP yaml: %v", err)
		}
		defer os.Remove(upgradeEIPYaml)
		e2ekubectl.RunKubectlOrDie("default", "create", "-f", upgradeEIPYaml)

		ginkgo.By("3. Waiting for EgressIP to be assigned to a node")
		upgradeEIPWaitForAssignment(upgradeEIPName, 1)

		// Create two pods:
		// - same node as egress node (tests local traffic path)
		// - different node (tests remote traffic path)
		ginkgo.By(fmt.Sprintf("4a. Creating test pod on egress node %s (same node)", egressNodeName))
		_, err := createGenericPodWithLabel(f, upgradePodSameName, egressNodeName,
			f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(upgradePodPort), podEgressLabel)
		framework.ExpectNoError(err, "failed to create same-node test pod")

		ginkgo.By(fmt.Sprintf("4b. Creating test pod on non-egress node %s (different node)", podNodeName))
		_, err = createGenericPodWithLabel(f, upgradePodDiffName, podNodeName,
			f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(upgradePodPort), podEgressLabel)
		framework.ExpectNoError(err, "failed to create diff-node test pod")

		ginkgo.By("5. [PRE-UPGRADE] Verifying EgressIP traffic for both pods — source IP must be " + egressIPStr)
		for _, podName := range []string{upgradePodSameName, upgradePodDiffName} {
			err = wait.PollImmediate(retryInterval, retryTimeout,
				targetExternalContainerAndTest(externalContainer,
					f.Namespace.Name, podName, true, []string{egressIPStr}))
			framework.ExpectNoError(err, "pre-upgrade traffic check failed for pod %s: %v", podName, err)
			framework.Logf("PRE-UPGRADE PASS [%s]: source IP confirmed as %s", podName, egressIPStr)
		}

		// ----------------------------------------------------------------
		// UPGRADE
		// ----------------------------------------------------------------

		newImage := os.Getenv(ovnUpgradeImageEnvVar)

		// Read current state before patching so we can revert after upgrade
		currentDS, err := f.ClientSet.AppsV1().DaemonSets(ovnKubeNS).Get(
			context.Background(), ovnKubeNodeDSName, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get current ovnkube-node DaemonSet")
		originalImage := upgradeEIPGetContainerImage(currentDS)
		originalMaxUnavailable := currentDS.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable

		if newImage == "" {
			newImage = originalImage
			framework.Logf("OVN_IMAGE not set — using current image: %s (simulated upgrade)", newImage)
		} else {
			framework.Logf("OVN_IMAGE set — upgrading from %s to: %s", originalImage, newImage)
		}

		ginkgo.By("6. Setting maxUnavailable=1 to ensure standard rolling update behaviour")
		rollingPatch := `{"spec":{"updateStrategy":{"rollingUpdate":{"maxUnavailable":1}}}}`
		_, err = f.ClientSet.AppsV1().DaemonSets(ovnKubeNS).Patch(
			context.Background(),
			ovnKubeNodeDSName,
			types.StrategicMergePatchType,
			[]byte(rollingPatch),
			metav1.PatchOptions{},
		)
		framework.ExpectNoError(err, "failed to patch ovnkube-node DaemonSet maxUnavailable")

		// Start goroutine to monitor EgressIP traffic during rollout.
		// Failures are logged — zero failures are expected.
		trafficFailures := 0
		trafficTotal := 0
		stopPolling := make(chan struct{})
		pollingDone := make(chan struct{})
		pollStopped := false
		stopPollOnce := func() {
			if !pollStopped {
				close(stopPolling)
				<-pollingDone
				pollStopped = true
			}
		}
		defer stopPollOnce()

		go func() {
			defer ginkgo.GinkgoRecover()
			defer close(pollingDone)
			for {
				select {
				case <-stopPolling:
					return
				case <-time.After(ovnUpgradeTrafficPollInterval):
					trafficTotal++
					// Check both pods during rollout
					for _, podName := range []string{upgradePodSameName, upgradePodDiffName} {
						checkErr := wait.PollImmediate(1*time.Second, ovnUpgradeTrafficCheckTimeout,
							targetExternalContainerAndTest(externalContainer,
								f.Namespace.Name, podName, true, []string{egressIPStr}))
						if checkErr != nil {
							trafficFailures++
							framework.Logf("[DURING-UPGRADE] Traffic check FAILED pod=%s (%d/%d): %v",
								podName, trafficFailures, trafficTotal, checkErr)
						} else {
							framework.Logf("[DURING-UPGRADE] Traffic check PASSED pod=%s (%d/%d): source IP = %s",
								podName, trafficTotal-trafficFailures, trafficTotal, egressIPStr)
						}
					}
				}
			}
		}()

		ginkgo.By(fmt.Sprintf("7. Patching ovnkube-node DaemonSet to image: %s", newImage))
		patch := fmt.Sprintf(
			`{"spec":{"template":{"spec":{"containers":[{"name":"%s","image":"%s"}]}}}}`,
			getNodeContainerName(), newImage,
		)
		_, err = f.ClientSet.AppsV1().DaemonSets(ovnKubeNS).Patch(
			context.Background(),
			ovnKubeNodeDSName,
			types.StrategicMergePatchType,
			[]byte(patch),
			metav1.PatchOptions{},
		)
		framework.ExpectNoError(err, "failed to patch ovnkube-node DaemonSet")

		ginkgo.By("8. Waiting for ovnkube-node DaemonSet rollout to complete")
		upgradeEIPWaitForDSRollout(f, ovnKubeNS, ovnKubeNodeDSName, ovnUpgradeRolloutTimeout)

		stopPollOnce()

		framework.Logf("UPGRADE COMPLETE: rollout finished. Traffic checks during rollout: %d passed, %d failed out of %d total",
			trafficTotal-trafficFailures, trafficFailures, trafficTotal)

		// ----------------------------------------------------------------
		// POST-UPGRADE VERIFICATION
		// ----------------------------------------------------------------

		ginkgo.By("9. [POST-UPGRADE] Verifying EgressIP is still assigned")
		upgradeEIPWaitForAssignment(upgradeEIPName, 1)
		framework.Logf("POST-UPGRADE: EgressIP assignment verified")

		ginkgo.By("10. [POST-UPGRADE] Verifying EgressIP traffic for both pods — source IP must still be " + egressIPStr)
		for _, podName := range []string{upgradePodSameName, upgradePodDiffName} {
			err = wait.PollImmediate(retryInterval, retryTimeout,
				targetExternalContainerAndTest(externalContainer,
					f.Namespace.Name, podName, true, []string{egressIPStr}))
			framework.ExpectNoError(err, "post-upgrade traffic check failed for pod %s: %v", podName, err)
			framework.Logf("POST-UPGRADE PASS [%s]: source IP confirmed as %s after upgrade", podName, egressIPStr)
		}

		framework.Logf("SUMMARY: EgressIP traffic during rolling upgrade — %d/%d checks passed",
			trafficTotal-trafficFailures, trafficTotal)

		// ----------------------------------------------------------------
		// REVERT — restore original DaemonSet state
		// ----------------------------------------------------------------

		ginkgo.By(fmt.Sprintf("11. Reverting ovnkube-node DaemonSet to original image: %s", originalImage))
		revertPatch := fmt.Sprintf(
			`{"spec":{"template":{"spec":{"containers":[{"name":"%s","image":"%s"}]}}}}`,
			getNodeContainerName(), originalImage,
		)
		_, err = f.ClientSet.AppsV1().DaemonSets(ovnKubeNS).Patch(
			context.Background(),
			ovnKubeNodeDSName,
			types.StrategicMergePatchType,
			[]byte(revertPatch),
			metav1.PatchOptions{},
		)
		framework.ExpectNoError(err, "failed to revert ovnkube-node DaemonSet image")

		ginkgo.By("12. Restoring original maxUnavailable setting")
		var revertRollingPatch string
		if originalMaxUnavailable == nil || originalMaxUnavailable.Type == 0 {
			intVal := int32(1)
			if originalMaxUnavailable != nil {
				intVal = originalMaxUnavailable.IntVal
			}
			revertRollingPatch = fmt.Sprintf(
				`{"spec":{"updateStrategy":{"rollingUpdate":{"maxUnavailable":%d}}}}`,
				intVal,
			)
		} else {
			revertRollingPatch = fmt.Sprintf(
				`{"spec":{"updateStrategy":{"rollingUpdate":{"maxUnavailable":%q}}}}`,
				originalMaxUnavailable.StrVal,
			)
		}
		_, err = f.ClientSet.AppsV1().DaemonSets(ovnKubeNS).Patch(
			context.Background(),
			ovnKubeNodeDSName,
			types.StrategicMergePatchType,
			[]byte(revertRollingPatch),
			metav1.PatchOptions{},
		)
		framework.ExpectNoError(err, "failed to revert ovnkube-node DaemonSet maxUnavailable")

		ginkgo.By("13. Waiting for revert rollout to complete")
		upgradeEIPWaitForDSRollout(f, ovnKubeNS, ovnKubeNodeDSName, ovnUpgradeRolloutTimeout)
		framework.Logf("REVERT COMPLETE: DaemonSet restored to original image %s", originalImage)
	})
})

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func upgradeEIPWaitForAssignment(eipName string, expectedCount int) {
	err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
		stdout, err := e2ekubectl.RunKubectl("default", "get", "eip", eipName, "-o", "json")
		if err != nil {
			return false, nil
		}
		eip := struct {
			Status struct {
				Items []egressIPStatus `json:"items"`
			} `json:"status"`
		}{}
		if err := json.Unmarshal([]byte(stdout), &eip); err != nil {
			return false, nil
		}
		return len(eip.Status.Items) == expectedCount, nil
	})
	if err != nil {
		framework.Failf("EgressIP %q: expected %d assignments, timed out: %v",
			eipName, expectedCount, err)
	}
}

func upgradeEIPWaitForDSRollout(f *framework.Framework, namespace, dsName string, timeout time.Duration) {
	framework.Logf("Waiting for DaemonSet %s rollout to start...", dsName)
	err := wait.PollUntilContextTimeout(
		context.Background(),
		2*time.Second,
		30*time.Second,
		true,
		func(ctx context.Context) (bool, error) {
			ds, err := f.ClientSet.AppsV1().DaemonSets(namespace).Get(
				ctx, dsName, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			desired := ds.Status.DesiredNumberScheduled
			updated := ds.Status.UpdatedNumberScheduled
			available := ds.Status.NumberAvailable
			started := updated < desired || available < desired
			framework.Logf("DaemonSet %s rollout start check: desired=%d updated=%d available=%d started=%v",
				dsName, desired, updated, available, started)
			return started, nil
		},
	)
	if err != nil {
		framework.Logf("Warning: could not confirm rollout started (may have been instant): %v", err)
	}

	framework.Logf("Waiting for DaemonSet %s rollout to complete...", dsName)
	err = wait.PollUntilContextTimeout(
		context.Background(),
		5*time.Second,
		timeout,
		true,
		func(ctx context.Context) (bool, error) {
			ds, err := f.ClientSet.AppsV1().DaemonSets(namespace).Get(
				ctx, dsName, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			desired := ds.Status.DesiredNumberScheduled
			if desired == 0 {
				framework.Logf("DaemonSet %s reports DesiredNumberScheduled==0 (possible nodeSelector/stale status)", dsName)
				return false, nil
			}
			updated := ds.Status.UpdatedNumberScheduled
			available := ds.Status.NumberAvailable
			ready := ds.Status.NumberReady
			framework.Logf("DaemonSet %s rollout: desired=%d updated=%d available=%d ready=%d",
				dsName, desired, updated, available, ready)
			return updated == desired && available == desired && ready == desired, nil
		},
	)
	if err != nil {
		framework.Failf("DaemonSet %q rollout did not complete within %v: %v",
			dsName, timeout, err)
	}
}

func upgradeEIPGetContainerImage(ds *appsv1.DaemonSet) string {
	containerName := getNodeContainerName()
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name == containerName {
			return c.Image
		}
	}
	framework.Failf("container %q not found in DaemonSet %s", containerName, ds.Name)
	return ""
}

func upgradeEIPGetNodeIP(node corev1.Node) string {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address
		}
	}
	return ""
}

// upgradeEIPDeriveEgressIP derives an EgressIP from a node IP by setting
// the last octet to 200 (or 201 if the node IP already ends in 200).
// For IPv6, the last byte is set to 0xc8 (200).
func upgradeEIPDeriveEgressIP(nodeIP string) (string, error) {
	ip := net.ParseIP(nodeIP)
	if ip == nil {
		return "", fmt.Errorf("failed to parse node IP: %s", nodeIP)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		ip6 := make([]byte, len(ip.To16()))
		copy(ip6, ip.To16())
		if ip6[15] == 0xc8 {
			ip6[15] = 0xc9
		} else {
			ip6[15] = 0xc8
		}
		return net.IP(ip6).String(), nil
	}
	ip4copy := make([]byte, len(ip4))
	copy(ip4copy, ip4)
	if ip4copy[3] == 200 {
		ip4copy[3] = 201
	} else {
		ip4copy[3] = 200
	}
	return net.IP(ip4copy).String(), nil
}
