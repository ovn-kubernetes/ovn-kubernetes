// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	gomegatypes "github.com/onsi/gomega/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/csrapprover"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
)

var _ = ginkgo.Describe("Check whether gateway-mtu-support annotation on node is set based on disable-pkt-mtu-check value", feature.DisablePacketMTUCheck, func() {
	var nodes *v1.NodeList
	f := wrappedTestFramework("gateway-mtu-support")

	ginkgo.BeforeEach(func() {
		var err error
		ginkgo.By("Get all nodes")
		nodes, err = e2enode.GetReadySchedulableNodes(context.TODO(), f.ClientSet)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
	ginkgo.When("DisablePacketMTUCheck is either not set or set to false", func() {
		ginkgo.It("Verify whether gateway-mtu-support annotation is not set on nodes when DisablePacketMTUCheck is either not set or set to false", func() {
			if !isDisablePacketMTUCheckEnabled() {
				for _, node := range nodes.Items {
					supported := getGatewayMTUSupport(&node)
					gomega.Expect(supported).To(gomega.Equal(true))

				}
			} else {
				ginkgo.Skip("DisablePacketMTUCheck is set to true")
			}
		})
	})

	ginkgo.When("the gateway-mtu-support annotation changes on a node", func() {
		// Serial: this mutates a shared node annotation and its router ports, which other specs observe.
		ginkgo.It("Verify options:gateway_mtu is removed from and restored on the cluster and gateway router ports", ginkgo.Serial, func(ctx ginkgo.SpecContext) {
			gomega.Expect(nodes.Items).NotTo(gomega.BeEmpty(), "gateway MTU reconciliation requires a ready schedulable node")
			node := nodes.Items[0]
			if node.Annotations[ovnGatewayMTUSupport] == "false" {
				ginkgo.Skip(fmt.Sprintf("node %s reports gateway MTU support disabled", node.Name))
			}
			ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()

			lrpName := ovntypes.RouterToSwitchPrefix + node.Name
			gatewayPortName := ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node.Name
			routerPorts := []string{lrpName, gatewayPortName}
			getLRPColumn := func(ctx context.Context, portName, column string) (string, error) {
				execCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				ovnkubeNodePods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(execCtx, metav1.ListOptions{
					LabelSelector: "app=ovnkube-node",
					FieldSelector: fmt.Sprintf("spec.nodeName=%s", node.Name),
				})
				if err != nil {
					return "", fmt.Errorf("failed to list ovnkube-node pods on node %s: %w", node.Name, err)
				}
				dbPodName := ""
				for _, pod := range ovnkubeNodePods.Items {
					if pod.Status.Phase == v1.PodRunning && pod.DeletionTimestamp == nil {
						dbPodName = pod.Name
						break
					}
				}
				if dbPodName == "" {
					return "", fmt.Errorf("no running, non-deleting ovnkube-node pod on node %s", node.Name)
				}
				stdout, stderr, err := e2epod.ExecWithOptionsContext(execCtx, f, e2epod.ExecOptions{
					Namespace:     ovnNamespace,
					PodName:       dbPodName,
					ContainerName: deploymentconfig.Get().NBDBContainerName(),
					// get fails for a missing port and preserves {} for an empty options map.
					Command:       []string{"ovn-nbctl", "get", "logical_router_port", portName, column},
					CaptureStdout: true,
					CaptureStderr: true,
				})
				if err != nil {
					return "", fmt.Errorf("failed to read %s of %s: %w, stderr: %s", column, portName, err, stderr)
				}
				return strings.Trim(strings.TrimSpace(stdout), "\""), nil
			}
			// Guard against a renamed port silently turning the assertions below into no-ops.
			ginkgo.By("Verify the cluster router port " + lrpName + " exists")
			name, err := getLRPColumn(ctx, lrpName, "name")
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to read router port %s", lrpName)
			gomega.Expect(name).To(gomega.Equal(lrpName), "unexpected cluster router port on node %s", node.Name)

			gatewayMTU := func(ctx context.Context, portName string) (string, error) {
				options, err := getLRPColumn(ctx, portName, "options")
				if err != nil {
					return "", err
				}
				_, value, found := strings.Cut(options, "gateway_mtu=")
				if !found {
					return "", nil
				}
				mtu := strings.Trim(strings.SplitN(value, ",", 2)[0], " \"}")
				if mtu == "" {
					return "", fmt.Errorf("gateway_mtu key is present with an empty value on router port %s", portName)
				}
				return mtu, nil
			}
			assertGatewayMTU := func(portName string, matcher gomegatypes.GomegaMatcher) {
				pollCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
				defer cancel()
				gomega.Eventually(pollCtx, gatewayMTU, 60*time.Second, 2*time.Second).WithArguments(portName).Should(matcher,
					"unexpected gateway_mtu on router port %s", portName)
			}

			// ovnkube-node owns this annotation and writes it through nodes/status.
			nodeConfig := rest.CopyConfig(f.ClientConfig())
			nodeConfig.Impersonate = rest.ImpersonationConfig{
				UserName: csrapprover.NamePrefix + ":" + node.Name,
				Groups:   []string{"system:ovn-nodes", "system:authenticated"},
			}
			nodeClient, err := clientset.NewForConfig(nodeConfig)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to create OVN node impersonation client")
			patchNode := func(patch string) {
				// Use a fresh deadline so cleanup can restore the annotation even if the spec context was canceled.
				patchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_, err := nodeClient.CoreV1().Nodes().Patch(patchCtx, node.Name,
					types.MergePatchType, []byte(patch), metav1.PatchOptions{}, "status")
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to patch node %s with %s", node.Name, patch)
			}
			setGatewayMTUSupport := func(value string) {
				patchNode(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, ovnGatewayMTUSupport, value))
			}
			// A null value deletes the annotation, and succeeds whether or not it is present.
			removeGatewayMTUSupport := func() {
				patchNode(fmt.Sprintf(`{"metadata":{"annotations":{%q:null}}}`, ovnGatewayMTUSupport))
			}

			// Put back whatever ovnkube-node decided for this node when it started.
			originalValue, hadAnnotation := node.Annotations[ovnGatewayMTUSupport]
			ginkgo.DeferCleanup(func() {
				if hadAnnotation {
					setGatewayMTUSupport(originalValue)
					return
				}
				removeGatewayMTUSupport()
			})

			ginkgo.By("Enable gateway MTU support and capture the router port MTU before disabling it")
			removeGatewayMTUSupport()
			assertGatewayMTU(lrpName, gomega.Not(gomega.BeEmpty()))
			expectedMTU, err := gatewayMTU(ctx, lrpName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to read enabled gateway_mtu on %s", lrpName)
			gomega.Expect(expectedMTU).NotTo(gomega.BeEmpty(), "gateway_mtu must be set on %s before disabling support", lrpName)
			assertGatewayMTU(gatewayPortName, gomega.Equal(expectedMTU))

			ginkgo.By("Annotate the node as not supporting gateway MTU")
			setGatewayMTUSupport("false")

			ginkgo.By("Verify options:gateway_mtu is removed from both router ports")
			for _, portName := range routerPorts {
				assertGatewayMTU(portName, gomega.BeEmpty())
			}

			ginkgo.By("Remove the annotation so the node reports gateway MTU support again")
			removeGatewayMTUSupport()

			ginkgo.By("Verify options:gateway_mtu is restored on both router ports")
			for _, portName := range routerPorts {
				assertGatewayMTU(portName, gomega.Equal(expectedMTU))
			}
		})
	})
})
