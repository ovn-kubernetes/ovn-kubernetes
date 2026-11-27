// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net"
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
	utilnet "k8s.io/utils/net"

	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
)

var _ = ginkgo.Describe("Node subnet claims", func() {
	f := wrappedTestFramework("node-subnet-claim")

	ginkgo.It("rejects a node claiming controller-owned network allocations", func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		nodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, f.ClientSet, 1)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(nodes.Items).NotTo(gomega.BeEmpty())

		victimSubnets, err := util.ParseNodeHostSubnetAnnotation(&nodes.Items[0], ovntypes.DefaultNetworkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(victimSubnets).NotTo(gomega.BeEmpty())

		victimSubnet := victimSubnets[0]
		prefixLen, addressLen := victimSubnet.Mask.Size()
		gomega.Expect(prefixLen).To(gomega.BeNumerically("<", addressLen))
		claimedSubnet := &net.IPNet{
			IP:   victimSubnet.IP,
			Mask: net.CIDRMask(prefixLen+1, addressLen),
		}
		annotations, err := util.UpdateNodeHostSubnetAnnotation(nil, []*net.IPNet{claimedSubnet}, ovntypes.DefaultNetworkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		annotations[util.OvnNodeID] = nodes.Items[0].Annotations[util.OvnNodeID]
		annotations[util.OvnTransitSwitchPortAddr] = nodes.Items[0].Annotations[util.OvnTransitSwitchPortAddr]
		annotations[ovntypes.UDNLayer2NodeGRLRPTunnelIDAnnotation] = `{"test-network":"1"}`

		remoteNodeName := "node-subnet-claim-" + framework.RandomSuffix()
		config := rest.CopyConfig(f.ClientConfig())
		config.Impersonate = rest.ImpersonationConfig{
			UserName: "system:node:" + remoteNodeName,
			Groups:   []string{"system:nodes", "system:authenticated"},
		}
		remoteNodeClient, err := kubernetes.NewForConfig(config)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Test each annotation separately so one rejection cannot mask a missing rule.
		for annotation, value := range annotations {
			maliciousNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name:        remoteNodeName,
				Annotations: map[string]string{annotation: value},
			}}
			_, err = remoteNodeClient.CoreV1().Nodes().Create(ctx, maliciousNode, metav1.CreateOptions{})
			if err == nil {
				gomega.Expect(f.ClientSet.CoreV1().Nodes().Delete(ctx, remoteNodeName,
					metav1.DeleteOptions{})).To(gomega.Succeed())
			}
			gomega.Expect(apierrors.IsForbidden(err)).To(gomega.BeTrue(),
				"allocation claim %s during node creation was not forbidden: %v", annotation, err)
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
				"not allowed to modify the following controller-owned annotations")))
		}

		_, err = f.ClientSet.CoreV1().Nodes().Create(ctx, &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: remoteNodeName},
		}, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			gomega.Expect(f.ClientSet.CoreV1().Nodes().Delete(cleanupCtx, remoteNodeName,
				metav1.DeleteOptions{})).To(gomega.Succeed())
		})

		for annotation, value := range annotations {
			annotationPatch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, annotation, value))
			for _, subresource := range []string{"", "status"} {
				_, err = remoteNodeClient.CoreV1().Nodes().Patch(ctx, remoteNodeName,
					types.MergePatchType, annotationPatch, metav1.PatchOptions{}, subresource)
				gomega.Expect(apierrors.IsForbidden(err)).To(gomega.BeTrue(),
					"allocation claim %s through nodes/%s was not forbidden: %v", annotation, subresource, err)
				gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
					"not allowed to modify the following controller-owned annotations")))
			}
		}
	})

	ginkgo.It("restores transit switch port addresses changed after allocation", ginkgo.SpecTimeout(2*time.Minute), func(ctx ginkgo.SpecContext) {
		const (
			timeout  = 30 * time.Second
			interval = time.Second
		)

		ginkgo.By("creating a node and waiting for its transit switch port addresses")
		nodeName := "transit-addr-repair-" + framework.RandomSuffix()
		_, err := f.ClientSet.CoreV1().Nodes().Create(ctx, &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		}, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to create node %s", nodeName)
		ginkgo.DeferCleanup(func(ctx ginkgo.SpecContext) {
			gomega.Expect(f.ClientSet.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})).
				To(gomega.Succeed(), "failed to delete node %s", nodeName)
		}, ginkgo.NodeTimeout(time.Minute))
		var node *corev1.Node
		gomega.Eventually(ctx, func(g gomega.Gomega) {
			var err error
			node, err = f.ClientSet.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "failed to get node %s", nodeName)
			g.Expect(node.Annotations).To(gomega.HaveKey(util.OvnTransitSwitchPortAddr),
				"node %s has no transit switch port addresses", nodeName)
		}, timeout, interval).Should(gomega.Succeed())
		allocated := node.Annotations[util.OvnTransitSwitchPortAddr]

		ginkgo.By("changing the addresses as cluster manager, the only allowed writer")
		addrs, err := util.ParseNodeTransitSwitchPortAddrs(node)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to parse the transit switch port addresses %s", allocated)
		// Addresses far from the allocated ones belong to no node in the test
		// cluster, so no real node loses traffic while they are in place.
		var v4Addr, v6Addr *net.IPNet
		for _, addr := range addrs {
			addr.IP = utilnet.AddIPOffset(utilnet.BigForIP(addr.IP), 4000)
			if utilnet.IsIPv6(addr.IP) {
				v6Addr = addr
			} else {
				v4Addr = addr
			}
		}
		changedAnnotations, err := util.CreateNodeTransitSwitchPortAddrAnnotation(nil, v4Addr, v6Addr)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to build the changed transit switch port addresses")
		changed := changedAnnotations[util.OvnTransitSwitchPortAddr].(string)
		gomega.Expect(changed).NotTo(gomega.Equal(allocated), "changed addresses must differ from the allocated ones")

		clusterManagerConfig := rest.CopyConfig(f.ClientConfig())
		clusterManagerConfig.Impersonate = rest.ImpersonationConfig{
			UserName: fmt.Sprintf("system:serviceaccount:%s:ovnkube-cluster-manager",
				deploymentconfig.Get().OVNKubernetesNamespace()),
		}
		clusterManagerClient, err := kubernetes.NewForConfig(clusterManagerConfig)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to create a client impersonating cluster manager")
		annotationPatch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, util.OvnTransitSwitchPortAddr, changed))
		// Cluster manager may only write Nodes through the status subresource.
		patched, err := clusterManagerClient.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType,
			annotationPatch, metav1.PatchOptions{}, "status")
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to change the transit switch port addresses of node %s", nodeName)
		gomega.Expect(patched.Annotations[util.OvnTransitSwitchPortAddr]).To(gomega.Equal(changed),
			"the changed addresses were not stored on node %s", nodeName)

		ginkgo.By("waiting for cluster manager to restore the allocated addresses")
		gomega.Eventually(ctx, func(g gomega.Gomega) {
			node, err := f.ClientSet.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "failed to get node %s", nodeName)
			g.Expect(node.Annotations[util.OvnTransitSwitchPortAddr]).To(gomega.Equal(allocated),
				"transit switch port addresses of node %s were not restored", nodeName)
		}, timeout, interval).Should(gomega.Succeed())
	})
})
