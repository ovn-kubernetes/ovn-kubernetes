// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"time"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	ipamclaimsclient "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/clientset/versioned"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
)

var _ = Describe("Multi Homing persistent IPAM claims", feature.MultiHoming, func() {
	f := wrappedTestFramework("ipam-claim-scope")

	var (
		claimsClient *ipamclaimsclient.Clientset
		nadClient    *nadclient.Clientset
	)

	BeforeEach(func() {
		var err error
		claimsClient, err = ipamclaimsclient.NewForConfig(f.ClientConfig())
		Expect(err).NotTo(HaveOccurred())
		nadClient, err = nadclient.NewForConfig(f.ClientConfig())
		Expect(err).NotTo(HaveOccurred())
	})

	createNAD := func(ctx SpecContext, namespace, name, network string) {
		GinkgoHelper()
		conf := newNetworkAttachmentConfig(networkAttachmentConfigParams{
			namespace: namespace, name: name, networkName: network,
			topology: "layer2", cidr: filterCIDRsAndJoin(f.ClientSet, "172.31.0.0/24,2010:100:200::/64"),
			allowPersistentIPs: true,
		})
		_, err := nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(ctx,
			generateNetAttachDef(namespace, name, generateNADSpec(conf)), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	}
	createClaim := func(ctx SpecContext, namespace, claimName, network string) {
		GinkgoHelper()
		_, err := claimsClient.K8sV1alpha1().IPAMClaims(namespace).Create(ctx, &ipamclaimsapi.IPAMClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace},
			Spec:       ipamclaimsapi.IPAMClaimSpec{Network: network, Interface: "net1"},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	}
	createPod := func(ctx SpecContext, namespace, name, nadNamespace, nadName, claimName string) *corev1.Pod {
		GinkgoHelper()
		p, err := f.ClientSet.CoreV1().Pods(namespace).Create(ctx, generatePodSpec(podConfiguration{
			namespace: namespace, name: name, containerCmd: []string{"netexec"},
			attachments: []nadapi.NetworkSelectionElement{{Name: nadName, Namespace: nadNamespace, IPAMClaimReference: claimName}},
		}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		return p
	}
	claimIPs := func(ctx SpecContext, namespace, claimName string) []string {
		GinkgoHelper()
		var ips []string
		Eventually(ctx, func(g Gomega) {
			claim, err := claimsClient.K8sV1alpha1().IPAMClaims(namespace).Get(ctx, claimName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(claim.Status.IPs).NotTo(BeEmpty())
			ips = claim.Status.IPs
		}, 2*time.Minute, time.Second).Should(Succeed())
		return ips
	}

	DescribeTable("enforces claim scope", func(ctx SpecContext, crossNamespace bool) {
		const claimName = "claim"
		podNamespace := f.Namespace.Name
		network := podNamespace + "-blue"

		By("creating the original pod and populating its persistent claim in the pod namespace")
		createNAD(ctx, podNamespace, "blue", network)
		createClaim(ctx, podNamespace, claimName, network)
		original := createPod(ctx, podNamespace, "original", podNamespace, "blue", claimName)
		originalClaimIPs := claimIPs(ctx, podNamespace, claimName)

		if crossNamespace {
			// The original and replacement pods both live in podNamespace. Only the
			// replacement's NAD is in nadNamespace. Two claims with the same name
			// but different IPs make a lookup in the wrong namespace detectable.
			By("creating another namespace with a NAD for the same network and a claim with the same name")
			nadNamespace, err := f.CreateNamespace(ctx, f.BaseName+"-nad", nil)
			Expect(err).NotTo(HaveOccurred())
			createNAD(ctx, nadNamespace.Name, "blue", network)
			createClaim(ctx, nadNamespace.Name, claimName, network)

			By("populating the other namespace's claim and checking that its IPs differ from the original claim's IPs")
			createPod(ctx, nadNamespace.Name, "nad-namespace-pod", nadNamespace.Name, "blue", claimName)
			nadNamespaceClaimIPs := claimIPs(ctx, nadNamespace.Name, claimName)
			Expect(nadNamespaceClaimIPs).NotTo(ConsistOf(originalClaimIPs))

			By("deleting the original pod while retaining its claim and reserved IPs")
			Expect(f.ClientSet.CoreV1().Pods(original.Namespace).Delete(ctx, original.Name, metav1.DeleteOptions{})).To(Succeed())
			Eventually(ctx, func() error {
				_, err := f.ClientSet.CoreV1().Pods(original.Namespace).Get(ctx, original.Name, metav1.GetOptions{})
				return err
			}, 2*time.Minute, time.Second).Should(MatchError(apierrors.IsNotFound, "IsNotFound"))

			By("creating a replacement in the original pod namespace that selects the NAD in the other namespace")
			replacement := createPod(ctx, podNamespace, "replacement", nadNamespace.Name, "blue", claimName)

			By("checking that the replacement receives the original claim's IPs from its own namespace")
			// Multus namespace isolation may block attachment to a NAD in another
			// namespace. OVN processes the pod independently, so check its annotation
			// without requiring successful CNI setup. The IPs must come from the
			// claim in the pod's namespace.
			Eventually(ctx, func(g Gomega) {
				ips, err := getPodAnnotationIPsForAttachment(f.ClientSet, replacement.Namespace, replacement.Name, nadNamespace.Name+"/blue")
				g.Expect(err).NotTo(HaveOccurred())
				actual := make([]string, 0, len(ips))
				for _, ip := range ips {
					actual = append(actual, ip.String())
				}
				g.Expect(actual).To(ConsistOf(originalClaimIPs))
			}, 2*time.Minute, time.Second).Should(Succeed())
			By("checking that the claim in the NAD namespace retains its separate IP reservation")
			Expect(claimIPs(ctx, nadNamespace.Name, claimName)).To(ConsistOf(nadNamespaceClaimIPs))
		} else {
			By("requesting the populated claim through a different network")
			createNAD(ctx, podNamespace, "red", podNamespace+"-red")
			wrongNetworkPod := createPod(ctx, podNamespace, "wrong-network", podNamespace, "red", claimName)
			Eventually(ctx, func(g Gomega) {
				events, err := f.ClientSet.CoreV1().Events(wrongNetworkPod.Namespace).SearchWithContext(ctx, scheme.Scheme, wrongNetworkPod)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(events.Items).To(ContainElement(SatisfyAll(
					HaveField("Reason", "ErrorAllocatingPod"),
					HaveField("Message", ContainSubstring("ignored IPAMClaim")),
				)))
			}, 2*time.Minute, time.Second).Should(Succeed())
			By("checking that the rejected pod receives no OVN annotation for the requested network")
			Consistently(ctx, func(g Gomega) {
				current, err := f.ClientSet.CoreV1().Pods(wrongNetworkPod.Namespace).Get(ctx, wrongNetworkPod.Name, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				_, err = getPodAnnotationForAttachment(current, fmt.Sprintf("%s/red", current.Namespace))
				g.Expect(err).To(HaveOccurred())
			}, 5*time.Second, time.Second).Should(Succeed())
		}
		By("checking that the original claim retains its IP reservation")
		Expect(claimIPs(ctx, podNamespace, claimName)).To(ConsistOf(originalClaimIPs))
	},
		Entry("resolves claims in the pod namespace when the NAD is elsewhere", true),
		Entry("rejects a claim from another network", false),
	)
})
