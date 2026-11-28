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
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	kubevirtv1 "kubevirt.io/api/core/v1"

	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var _ = ginkgo.Describe("Pod IP claims", ginkgo.Serial, func() {
	const podWebhookConfiguration = "ovn-kubernetes-admission-webhook-pod"

	f := wrappedTestFramework("pod-ip-claim")

	ginkgo.It("lets a node write an IP of another node on a live-migratable pod only when a pod of the same virtual machine holds it", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		_, err := f.ClientSet.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx,
			podWebhookConfiguration, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			ginkgo.Skip("node identity is not enabled")
		}
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to get validating webhook configuration %s", podWebhookConfiguration)

		// The nodes have no kubelet, so no pod bound to them is ever wired and
		// the annotations written below stay inert.
		suffix := framework.RandomSuffix()
		claimingNode := "pod-ip-claim-a-" + suffix
		holdingNode := "pod-ip-claim-b-" + suffix
		for _, nodeName := range []string{claimingNode, holdingNode} {
			_, err = f.ClientSet.CoreV1().Nodes().Create(ctx, &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName},
			}, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to create node %s", nodeName)
			ginkgo.DeferCleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()

				err := f.ClientSet.CoreV1().Nodes().Delete(cleanupCtx, nodeName, metav1.DeleteOptions{})
				if apierrors.IsNotFound(err) {
					return
				}
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to delete node %s", nodeName)
			})
		}

		nodeSubnets := func(nodeName string) ([]*net.IPNet, error) {
			node, err := f.ClientSet.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
			}
			subnets, err := util.ParseNodeHostSubnetAnnotation(node, ovntypes.DefaultNetworkName)
			if err != nil {
				return nil, fmt.Errorf("failed to parse the subnets of node %s: %w", nodeName, err)
			}
			return subnets, nil
		}
		var holdingSubnets []*net.IPNet
		gomega.Eventually(func() error {
			var err error
			holdingSubnets, err = nodeSubnets(holdingNode)
			if err != nil {
				return fmt.Errorf("node %s has no subnets yet: %w", holdingNode, err)
			}
			return nil
		}).WithTimeout(time.Minute).WithPolling(time.Second).Should(gomega.Succeed(),
			"node %s did not get its subnets", holdingNode)
		gomega.Eventually(func() error {
			if _, err := nodeSubnets(claimingNode); err != nil {
				return fmt.Errorf("node %s has no subnets yet: %w", claimingNode, err)
			}
			return nil
		}).WithTimeout(time.Minute).WithPolling(time.Second).Should(gomega.Succeed(),
			"node %s did not get its subnets", claimingNode)

		createPod := func(name, nodeName, vmName string) {
			pod := e2epod.NewAgnhostPod(f.Namespace.Name, name, nil, nil, nil)
			pod.Spec.NodeName = nodeName
			pod.Labels = map[string]string{kubevirtv1.AppLabel: "virt-launcher"}
			pod.Annotations = map[string]string{
				kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
				kubevirtv1.DomainAnnotation:                             vmName,
			}
			_, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to create pod %s", name)
			ginkgo.DeferCleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()

				// Without a kubelet a graceful deletion never completes.
				err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(cleanupCtx, name, *metav1.NewDeleteOptions(0))
				if apierrors.IsNotFound(err) {
					return
				}
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to delete pod %s", name)
			})
		}
		createPod("claimer", claimingNode, "vm-a")
		createPod("same-vm", holdingNode, "vm-a")
		createPod("other-vm", holdingNode, "vm-b")

		// podNetworks returns a default network entry with one IP per subnet of
		// the holding node.
		podNetworks := func(hostOffset byte) string {
			podAnnotation := &util.PodAnnotation{Role: ovntypes.NetworkRolePrimary}
			for _, subnet := range holdingSubnets {
				ip := make(net.IP, len(subnet.IP))
				copy(ip, subnet.IP)
				ip[len(ip)-1] += hostOffset
				podAnnotation.IPs = append(podAnnotation.IPs, &net.IPNet{IP: ip, Mask: subnet.Mask})
			}
			podAnnotation.MAC = util.IPAddrToHWAddr(podAnnotation.IPs[0].IP)
			annotations, err := util.MarshalPodAnnotation(nil, podAnnotation, ovntypes.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to marshal the pod networks annotation")
			return annotations[ovntypes.OvnPodAnnotationName]
		}
		sameVMPodNetworks := podNetworks(10)
		otherVMPodNetworks := podNetworks(11)

		writePodNetworks := func(nodeName, podName, value string) error {
			config := rest.CopyConfig(f.ClientConfig())
			config.Impersonate = rest.ImpersonationConfig{
				UserName: "system:ovn-node:" + nodeName,
				Groups:   []string{"system:ovn-nodes", "system:authenticated"},
			}
			nodeClient, err := kubernetes.NewForConfig(config)
			if err != nil {
				return fmt.Errorf("failed to create a client for node %s: %w", nodeName, err)
			}
			patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, ovntypes.OvnPodAnnotationName, value))
			_, err = nodeClient.CoreV1().Pods(f.Namespace.Name).Patch(ctx, podName,
				types.MergePatchType, patch, metav1.PatchOptions{}, "status")
			if err != nil {
				return fmt.Errorf("failed to patch pod %s as node %s: %w", podName, nodeName, err)
			}
			return nil
		}
		// The webhook reads nodes and pods from its own caches, which may
		// trail the API server.
		eventually := func(write func() error) gomega.AsyncAssertion {
			return gomega.Eventually(write).WithTimeout(30 * time.Second).WithPolling(time.Second)
		}
		beDenied := gomega.And(
			gomega.WithTransform(apierrors.IsForbidden, gomega.BeTrue()),
			gomega.MatchError(gomega.ContainSubstring("is not held by another pod of the same virtual machine")),
		)

		ginkgo.By("writing IPs of the holding node on the pods bound to it")
		eventually(func() error {
			return writePodNetworks(holdingNode, "same-vm", sameVMPodNetworks)
		}).Should(gomega.Succeed(), "the holding node must be able to set its own IPs on pod same-vm")
		eventually(func() error {
			return writePodNetworks(holdingNode, "other-vm", otherVMPodNetworks)
		}).Should(gomega.Succeed(), "the holding node must be able to set its own IPs on pod other-vm")

		ginkgo.By("claiming the IPs of another virtual machine from the claiming node")
		eventually(func() error {
			return writePodNetworks(claimingNode, "claimer", otherVMPodNetworks)
		}).Should(beDenied, "the IPs of another virtual machine must be denied on the claiming node")

		ginkgo.By("claiming the IPs of a pod of the same virtual machine from the claiming node")
		eventually(func() error {
			return writePodNetworks(claimingNode, "claimer", sameVMPodNetworks)
		}).Should(gomega.Succeed(), "the IPs of a pod of the same virtual machine must be accepted on the claiming node")

		ginkgo.By("replacing them with the IPs of another virtual machine")
		gomega.Expect(writePodNetworks(claimingNode, "claimer", otherVMPodNetworks)).To(beDenied,
			"the IPs of another virtual machine must be denied once the pod has the IPs of its own")
	})
})
