// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"

	hotypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
)

var _ = ginkgo.Describe("Hybrid-overlay subnet claims", ginkgo.Label("HybridOverlay"), func() {
	f := wrappedTestFramework("hybrid-overlay-subnet")

	ginkgo.It("rejects a remote node claiming an arbitrary subnet", func() {
		if os.Getenv("OVN_HYBRID_OVERLAY_ENABLE") != "true" {
			ginkgo.Skip("hybrid overlay is not enabled")
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		nodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, f.ClientSet, 1)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(nodes.Items).NotTo(gomega.BeEmpty())
		nodeName := nodes.Items[0].Name

		const ipv4ClaimedSubnet = "198.19.0.0/24"
		const ipv6ClaimedSubnet = "2001:db8:198:19::/64"
		const targetIP = "198.19.0.20"
		claimedSubnets := []string{ipv4ClaimedSubnet, ipv6ClaimedSubnet}
		remoteNodeName := "hybrid-overlay-subnet-" + framework.RandomSuffix()

		config := rest.CopyConfig(f.ClientConfig())
		config.Impersonate = rest.ImpersonationConfig{
			UserName: "system:node:" + remoteNodeName,
			Groups:   []string{"system:nodes", "system:authenticated"},
		}
		remoteNodeClient, err := kubernetes.NewForConfig(config)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		for _, subnet := range claimedSubnets {
			maliciousNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: remoteNodeName,
				Annotations: map[string]string{
					hotypes.HybridOverlayNodeSubnet: subnet,
				},
			}}
			_, err = remoteNodeClient.CoreV1().Nodes().Create(ctx, maliciousNode, metav1.CreateOptions{})
			if err == nil {
				gomega.Expect(f.ClientSet.CoreV1().Nodes().Delete(ctx, remoteNodeName,
					metav1.DeleteOptions{})).To(gomega.Succeed())
			}
			gomega.Expect(apierrors.IsForbidden(err)).To(gomega.BeTrue(),
				"node subnet claim %s during node creation was not forbidden: %v", subnet, err)
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
				"not allowed to modify the following controller-owned annotations")))
		}

		remoteNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: remoteNodeName,
			Labels: map[string]string{
				corev1.LabelOSStable: "windows",
			},
			Annotations: map[string]string{
				hotypes.HybridOverlayDRMAC: "0a:58:c6:13:00:01",
			},
		}}
		_, err = f.ClientSet.CoreV1().Nodes().Create(ctx, remoteNode, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			gomega.Expect(f.ClientSet.CoreV1().Nodes().Delete(cleanupCtx, remoteNodeName,
				metav1.DeleteOptions{})).To(gomega.Succeed())
		})

		statusPatch := []byte(`{"status":{"addresses":[{"type":"InternalIP","address":"192.0.2.10"}]}}`)
		_, err = f.ClientSet.CoreV1().Nodes().Patch(ctx, remoteNodeName, types.MergePatchType,
			statusPatch, metav1.PatchOptions{}, "status")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		for _, subnet := range claimedSubnets {
			annotationPatch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`,
				hotypes.HybridOverlayNodeSubnet, subnet))
			for _, subresource := range []string{"", "status"} {
				_, err = remoteNodeClient.CoreV1().Nodes().Patch(ctx, remoteNodeName,
					types.MergePatchType, annotationPatch, metav1.PatchOptions{}, subresource)
				gomega.Expect(apierrors.IsForbidden(err)).To(gomega.BeTrue(),
					"node subnet claim %s through nodes/%s was not forbidden: %v", subnet, subresource, err)
				gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
					"not allowed to modify the following controller-owned annotations")))
			}
		}

		updatedNode, err := f.ClientSet.CoreV1().Nodes().Get(ctx, remoteNodeName, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(updatedNode.Annotations).NotTo(gomega.HaveKey(hotypes.HybridOverlayNodeSubnet))

		pods, err := f.ClientSet.CoreV1().Pods(deploymentconfig.Get().OVNKubernetesNamespace()).List(ctx,
			metav1.ListOptions{
				LabelSelector: "app=ovnkube-node",
				FieldSelector: "spec.nodeName=" + nodeName,
			})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pods.Items).To(gomega.HaveLen(1))

		provider := infraprovider.Get()
		// Hybrid overlay allocation and its Linux OpenFlow datapath are IPv4-only,
		// so this route/flow check deliberately uses an IPv4 target and nw_dst.
		gomega.Consistently(func() (string, error) {
			route, err := provider.ExecK8NodeCommand(nodeName, []string{"ip", "route", "get", targetIP})
			if err != nil {
				return "", err
			}

			flows, err := e2epodoutput.RunHostCmd(pods.Items[0].Namespace, pods.Items[0].Name,
				fmt.Sprintf("ovs-ofctl dump-flows %s", deploymentconfig.Get().ExternalBridgeName()))
			return strings.Join([]string{route, flows}, "\n"), err
		}, 5*time.Second, time.Second).ShouldNot(gomega.Or(
			gomega.ContainSubstring("ovn-k8s-mp0"),
			gomega.ContainSubstring("nw_dst="+ipv4ClaimedSubnet),
		))
	})
})
