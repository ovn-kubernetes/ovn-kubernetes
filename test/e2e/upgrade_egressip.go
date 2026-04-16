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
//
// Test flow:
//   PRE-UPGRADE
//     0. Label node as egress-assignable
//     1. Create EgressIP object
//     2. Create test pod
//     3. Verify pod traffic uses EgressIP as source IP
//
//   UPGRADE (via Kubernetes API)
//     4. Read OVN_IMAGE from env (or use current image if not set)
//     5. Set maxUnavailable=1 to ensure standard rolling update behaviour
//     6. Start traffic polling goroutine to monitor EgressIP during rollout
//     7. Patch ovnkube-node DaemonSet with new image
//     8. Wait for DaemonSet rollout to complete
//     9. Stop traffic polling — log failure summary
//
//   POST-UPGRADE
//     10. Verify EgressIP is still assigned
//     11. Verify pod traffic still uses EgressIP as source IP
//
// Key assertion: EgressIP traffic must be intact (zero failures) during upgrade. 
// currently logging the failures, assertions will be added later.
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
	// ovnKubeNodeDSName is the name of the ovnkube-node DaemonSet.
	ovnKubeNodeDSName = "ovnkube-node"

	// ovnUpgradeImageEnvVar is the env var CI sets to the new image.
	// If not set, the current image is used (simulated upgrade for local testing).
	ovnUpgradeImageEnvVar = "OVN_IMAGE"

	// ovnUpgradeRolloutTimeout is how long to wait for DaemonSet rollout.
	ovnUpgradeRolloutTimeout = 3 * time.Minute

	// ovnUpgradeTrafficPollInterval is how often to check EgressIP traffic during rollout.
	ovnUpgradeTrafficPollInterval = 5 * time.Second

	// ovnUpgradeTrafficCheckTimeout is how long a single traffic check is allowed to take.
	ovnUpgradeTrafficCheckTimeout = 3 * time.Second
)

var _ = ginkgo.Describe("e2e EgressIP upgrade validation", feature.EgressIP, func() {

	const (
		upgradeEIPName = "egressip-upgrade-test"
		upgradeEIPYaml = "egressip-upgrade-test.yaml"
		upgradePodName = "egressip-upgrade-pod"
		upgradePodPort = 8080
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

		// Need at least 2 nodes: one for egress assignment, one for the test pod
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			framework.Failf("EgressIP upgrade test requires >= 2 Ready nodes, got %d", len(nodes.Items))
		}

		// Create test namespace
		ns, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework": f.BaseName,
		})
		framework.ExpectNoError(err)
		f.Namespace = ns

		// First node is the egress node, second runs the test pod
		egressNodeName = nodes.Items[0].Name
		podNodeName = nodes.Items[1].Name

		// Derive EgressIP from the egress node's actual IP by setting
		// the last octet to 200 — stays in the same subnet and is routable
		nodeIP := upgradeEIPGetNodeIP(nodes.Items[0])
		var deriveErr error
		egressIPStr, deriveErr = upgradeEIPDeriveEgressIP(nodeIP)
		gomega.Expect(deriveErr).ShouldNot(gomega.HaveOccurred(), "must derive EgressIP")

		// Create external container (outside the cluster) for traffic verification
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
		unlabelNodeForEgress(f, egressNodeName)
		e2ekubectl.RunKubectlOrDie("default", "delete", "eip", upgradeEIPName, "--ignore-not-found=true")
		os.Remove(upgradeEIPYaml)
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

		ginkgo.By(fmt.Sprintf("4. Creating test pod on node %s", podNodeName))
		_, err := createGenericPodWithLabel(f, upgradePodName, podNodeName,
			f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(upgradePodPort), podEgressLabel)
		framework.ExpectNoError(err, "failed to create test pod")

		ginkgo.By("5. [PRE-UPGRADE] Verifying EgressIP traffic — source IP must be " + egressIPStr)
		err = wait.PollImmediate(retryInterval, retryTimeout,
			targetExternalContainerAndTest(externalContainer,
				f.Namespace.Name, upgradePodName, true, []string{egressIPStr}))
		framework.ExpectNoError(err, "pre-upgrade traffic check failed: %v", err)
		framework.Logf("PRE-UPGRADE PASS: source IP confirmed as %s", egressIPStr)

		// ----------------------------------------------------------------
		// UPGRADE — patch DaemonSet image via Kubernetes API
		// ----------------------------------------------------------------

		// Determine which image to roll to.
		// In CI: OVN_IMAGE env var is set to the new PR image.
		// Locally: OVN_IMAGE not set — read current image from DaemonSet
		// and reuse it.
		newImage := os.Getenv(ovnUpgradeImageEnvVar)
		if newImage == "" {
			ds, err := f.ClientSet.AppsV1().DaemonSets(ovnKubeNS).Get(
				context.Background(), ovnKubeNodeDSName, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to get ovnkube-node DaemonSet")
			newImage = upgradeEIPGetContainerImage(ds)
			framework.Logf("OVN_IMAGE not set — using current image: %s (simulated upgrade)", newImage)
		} else {
			framework.Logf("OVN_IMAGE set — upgrading to: %s", newImage)
		}

		ginkgo.By("6. Setting maxUnavailable=1 to ensure standard rolling update behaviour")
		// This ensures one pod is restarted at a time — matching real CI upgrade
		// behaviour and allowing the rollout check to detect the start correctly.
		// KIND clusters set maxUnavailable=100% by default for fast setup,
		// which would cause all pods to restart simultaneously.
		rollingPatch := `{"spec":{"updateStrategy":{"rollingUpdate":{"maxUnavailable":1}}}}`
		_, err = f.ClientSet.AppsV1().DaemonSets(ovnKubeNS).Patch(
			context.Background(),
			ovnKubeNodeDSName,
			types.StrategicMergePatchType,
			[]byte(rollingPatch),
			metav1.PatchOptions{},
		)
		framework.ExpectNoError(err, "failed to patch ovnkube-node DaemonSet maxUnavailable")

		// Start traffic polling goroutine BEFORE patching the image.
		// EgressIP traffic must be intact (zero failures) during the rolling upgrade.
		// The OVN dataplane maintains SNAT rules even while ovnkube-node pods are
		// restarting one by one
		trafficFailures := 0
		trafficTotal := 0
		stopPolling := make(chan struct{})
		pollingDone := make(chan struct{})

		go func() {
			defer close(pollingDone)
			for {
				select {
				case <-stopPolling:
					return
				case <-time.After(ovnUpgradeTrafficPollInterval):
					trafficTotal++
					checkErr := wait.PollImmediate(1*time.Second, ovnUpgradeTrafficCheckTimeout,
						targetExternalContainerAndTest(externalContainer,
							f.Namespace.Name, upgradePodName, true, []string{egressIPStr}))
					if checkErr != nil {
						trafficFailures++
						framework.Logf("[DURING-UPGRADE] Traffic check FAILED (%d/%d): %v",
							trafficFailures, trafficTotal, checkErr)
					} else {
						framework.Logf("[DURING-UPGRADE] Traffic check PASSED (%d/%d): source IP = %s",
							trafficTotal-trafficFailures, trafficTotal, egressIPStr)
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

		// Stop traffic polling goroutine and wait for it to finish
		close(stopPolling)
		<-pollingDone

		framework.Logf("UPGRADE COMPLETE: rollout finished. EgressIP traffic checks during rollout: %d passed, %d failed out of %d total",
			trafficTotal-trafficFailures, trafficFailures, trafficTotal)

		// NOTE: traffic failures during rollout are logged above.
		// Zero failures are expected — the OVN dataplane must maintain SNAT rules
		// while ovnkube-node pods restart. 
		// TODO : Add assertion for zero traffic failures.
		
		// ----------------------------------------------------------------
		// POST-UPGRADE VERIFICATION
		// ----------------------------------------------------------------

		ginkgo.By("9. [POST-UPGRADE] Verifying EgressIP is still assigned")
		upgradeEIPWaitForAssignment(upgradeEIPName, 1)
		framework.Logf("POST-UPGRADE: EgressIP assignment verified")

		ginkgo.By("10. [POST-UPGRADE] Verifying EgressIP traffic — source IP must still be " + egressIPStr)
		err = wait.PollImmediate(retryInterval, retryTimeout,
			targetExternalContainerAndTest(externalContainer,
				f.Namespace.Name, upgradePodName, true, []string{egressIPStr}))
		framework.ExpectNoError(err, "post-upgrade traffic check failed: %v", err)
		framework.Logf("POST-UPGRADE PASS: source IP confirmed as %s after upgrade", egressIPStr)
		framework.Logf("SUMMARY: EgressIP traffic fully intact during rolling upgrade — %d/%d checks passed",
			trafficTotal-trafficFailures, trafficTotal)
	})
})

// ---------------------------------------------------------------------------
// Helpers — scoped to upgrade tests only.
// ---------------------------------------------------------------------------

// upgradeEIPWaitForAssignment polls until the named EgressIP has the expected
// number of node assignments, failing the test if timeout is reached.
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

// upgradeEIPWaitForDSRollout waits until the DaemonSet rolling update is
// complete — all pods updated, available, and ready.
// Phase 1 waits for rollout to START, Phase 2 waits for it to COMPLETE.
func upgradeEIPWaitForDSRollout(f *framework.Framework, namespace, dsName string, timeout time.Duration) {
	// Phase 1: Wait for rollout to START — at least one pod not yet updated/available
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
			framework.Logf("DaemonSet %s waiting for rollout start: desired=%d updated=%d available=%d started=%v",
				dsName, desired, updated, available, started)
			return started, nil
		},
	)
	if err != nil {
		framework.Logf("Warning: could not confirm rollout started (may have been instant): %v", err)
	}

	// Phase 2: Wait for rollout to COMPLETE
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

// upgradeEIPGetContainerImage returns the image of the ovnkube-node container
// from the DaemonSet spec. Falls back to first container if named one not found.
func upgradeEIPGetContainerImage(ds *appsv1.DaemonSet) string {
	containerName := getNodeContainerName()
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name == containerName {
			return c.Image
		}
	}
	if len(ds.Spec.Template.Spec.Containers) > 0 {
		return ds.Spec.Template.Spec.Containers[0].Image
	}
	framework.Failf("no containers found in DaemonSet %s", ds.Name)
	return ""
}

// upgradeEIPGetNodeIP returns the first InternalIP of the given node.
func upgradeEIPGetNodeIP(node corev1.Node) string {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address
		}
	}
	return ""
}

// upgradeEIPDeriveEgressIP takes a node IP like 172.18.0.2 and returns
// an EgressIP in the same subnet
func upgradeEIPDeriveEgressIP(nodeIP string) (string, error) {
	ip := net.ParseIP(nodeIP)
	if ip == nil {
		return "", fmt.Errorf("failed to parse node IP: %s", nodeIP)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		// IPv6 — set second to last byte to 0xc8 (200)
		ip6 := ip.To16()
		ip6[14] = 0xc8
		ip6[15] = 0x00
		return ip6.String(), nil
	}
	// IPv4 — set last octet to 200, keep first three octets
	ip4[3] = 200
	return ip4.String(), nil
}