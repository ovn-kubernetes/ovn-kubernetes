// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkqosv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
)

var _ = Describe("NetworkQoS CRD", func() {
	const (
		protocolError = `supported values: "TCP", "UDP", "SCTP"`
		cidrError     = "must be a CIDR or an IP address"
	)
	classifierSchema := []string{"schema", "openAPIV3Schema", "properties", "spec", "properties", "egress",
		"items", "properties", "classifier", "properties"}
	protocolSchema := append(append([]string{}, classifierSchema...), "ports", "items", "properties", "protocol")
	cidrSchema := append(append([]string{}, classifierSchema...), "to", "items", "properties", "ipBlock", "properties", "cidr")

	// newNetworkQoS builds a NetworkQoS with the given classifier in its only egress rule.
	newNetworkQoS := func(classifier networkqosv1alpha1.Classifier) *networkqosv1alpha1.NetworkQoS {
		return &networkqosv1alpha1.NetworkQoS{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "test-classifier-", Namespace: "default"},
			Spec: networkqosv1alpha1.Spec{
				Priority: 10,
				Egress: []networkqosv1alpha1.Rule{{
					DSCP:       10,
					Bandwidth:  networkqosv1alpha1.Bandwidth{Rate: 1, Burst: 1},
					Classifier: classifier,
				}},
			},
		}
	}
	withProtocol := func(protocol string) networkqosv1alpha1.Classifier {
		return networkqosv1alpha1.Classifier{
			Ports: []*networkqosv1alpha1.Port{{Protocol: protocol, Port: ptr.To[int32](80)}},
		}
	}
	withIPBlock := func(cidr string, except ...string) networkqosv1alpha1.Classifier {
		ipBlock := &networkqosv1alpha1.IPBlock{CIDR: networkqosv1alpha1.CIDR(cidr)}
		for _, entry := range except {
			ipBlock.Except = append(ipBlock.Except, networkqosv1alpha1.CIDR(entry))
		}
		return networkqosv1alpha1.Classifier{To: []networkqosv1alpha1.Destination{{IPBlock: ipBlock}}}
	}

	expectAdmitted := func(classifier networkqosv1alpha1.Classifier) {
		GinkgoHelper()
		nqos := newNetworkQoS(classifier)
		Expect(k8sClient.Create(ctx, nqos)).To(Succeed(), "failed to create NetworkQoS with classifier %+v", classifier)
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, nqos) })
	}
	expectRejected := func(classifier networkqosv1alpha1.Classifier, message string) {
		GinkgoHelper()
		nqos := newNetworkQoS(classifier)
		err := k8sClient.Create(ctx, nqos)
		if err == nil {
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, nqos) })
		}
		Expect(err).To(MatchError(ContainSubstring(message)),
			"NetworkQoS with classifier %+v must be rejected", classifier)
	}

	// expectRatcheting stores a NetworkQoS with the given classifier under a
	// version of the CRD schema changed by relax, restores the schema and
	// checks which updates of the stored object are admitted.
	expectRatcheting := func(classifier networkqosv1alpha1.Classifier, message string, relax func(version map[string]interface{})) {
		GinkgoHelper()
		testCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		DeferCleanup(cancel)

		getCRD := func(ctx context.Context) (*unstructured.Unstructured, error) {
			crd := &unstructured.Unstructured{}
			crd.SetGroupVersionKind(schema.GroupVersionKind{
				Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
			return crd, k8sClient.Get(ctx, client.ObjectKey{Name: "networkqoses.k8s.ovn.org"}, crd)
		}
		crd, err := getCRD(testCtx)
		Expect(err).NotTo(HaveOccurred(), "failed to get the NetworkQoS CRD")
		currentSpec := crd.DeepCopy().Object["spec"]
		restoreValidation := func() error {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cleanupCancel()
			current, err := getCRD(cleanupCtx)
			if err != nil {
				return err
			}
			current.Object["spec"] = currentSpec
			return k8sClient.Update(cleanupCtx, current)
		}
		DeferCleanup(restoreValidation)

		By("installing the schema of an earlier version of the CRD")
		versions, found, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
		Expect(err).NotTo(HaveOccurred(), "failed to read the versions of the NetworkQoS CRD")
		Expect(found).To(BeTrue(), "the NetworkQoS CRD has no versions")
		version, ok := versions[0].(map[string]interface{})
		Expect(ok).To(BeTrue(), "unexpected type for the first version of the NetworkQoS CRD")
		relax(version)
		Expect(unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions")).
			To(Succeed(), "failed to set the versions of the NetworkQoS CRD")
		Expect(k8sClient.Update(testCtx, crd)).To(Succeed(), "failed to install the earlier schema")

		By("storing a NetworkQoS that only the earlier schema admits")
		nqos := newNetworkQoS(classifier)
		nqos.Spec.Egress = append(nqos.Spec.Egress, networkqosv1alpha1.Rule{
			DSCP:      20,
			Bandwidth: networkqosv1alpha1.Bandwidth{Rate: 1, Burst: 1},
		})
		Eventually(func() error {
			candidate := nqos.DeepCopy()
			if err := k8sClient.Create(testCtx, candidate); err != nil {
				return err
			}
			nqos = candidate
			return nil
		}).WithTimeout(5*time.Second).WithPolling(100*time.Millisecond).
			Should(Succeed(), "the earlier schema did not admit classifier %+v", classifier)
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), nqos) })

		By("restoring the current schema")
		Expect(restoreValidation()).To(Succeed(), "failed to restore the current schema")
		Eventually(func(g Gomega) {
			err := k8sClient.Create(testCtx, newNetworkQoS(classifier), client.DryRunAll)
			g.Expect(err).To(MatchError(ContainSubstring(message)))
		}).WithTimeout(5*time.Second).WithPolling(100*time.Millisecond).
			Should(Succeed(), "the current schema did not reject a new NetworkQoS with classifier %+v", classifier)

		By("updating the status of the stored NetworkQoS")
		meta.SetStatusCondition(&nqos.Status.Conditions, metav1.Condition{
			Type:    "Ready-In-Zone-node1",
			Status:  metav1.ConditionTrue,
			Reason:  "Success",
			Message: "NetworkQoS was applied successfully",
		})
		Expect(k8sClient.Status().Update(testCtx, nqos)).To(Succeed(),
			"a status update of the stored NetworkQoS must be admitted")

		By("changing another rule of the stored NetworkQoS")
		nqos.Spec.Egress[1].DSCP = 30
		Expect(k8sClient.Update(testCtx, nqos)).To(MatchError(ContainSubstring(message)),
			"a change to the egress rules must be validated against the current schema")
	}

	Context("spec.egress[].classifier.ports[].protocol", func() {
		DescribeTable("accepts a supported protocol",
			func(protocol string) { expectAdmitted(withProtocol(protocol)) },
			Entry("TCP", "TCP"),
			Entry("UDP", "UDP"),
			Entry("SCTP", "SCTP"),
		)

		DescribeTable("rejects any other protocol",
			func(protocol string) { expectRejected(withProtocol(protocol), protocolError) },
			Entry("lowercase", "tcp"),
			Entry("unknown protocol", "ICMP"),
			Entry("supported name as a prefix", "TCP || ip4"),
			Entry("supported name in the middle", "ip4 || UDP || ip6"),
			Entry("supported name as a suffix", "ip4 || SCTP"),
		)

		It("ratchets the enum for a NetworkQoS stored with another protocol", func() {
			expectRatcheting(withProtocol("TCP || ip4"), protocolError, func(version map[string]interface{}) {
				_, found, err := unstructured.NestedStringSlice(version, append(protocolSchema, "enum")...)
				Expect(err).NotTo(HaveOccurred(), "failed to read the protocol enum of the NetworkQoS CRD")
				Expect(found).To(BeTrue(), "the NetworkQoS CRD has no protocol enum")
				unstructured.RemoveNestedField(version, append(protocolSchema, "enum")...)
				Expect(unstructured.SetNestedField(version, "^TCP|UDP|SCTP$", append(protocolSchema, "pattern")...)).
					To(Succeed(), "failed to set the protocol pattern")
			})
		})
	})

	Context("spec.egress[].classifier.to[].ipBlock", func() {
		DescribeTable("accepts a CIDR or an IP address",
			func(cidr string, except ...string) { expectAdmitted(withIPBlock(cidr, except...)) },
			Entry("IPv4 CIDR", "192.168.1.0/24"),
			Entry("IPv6 CIDR", "2001:db8::/64"),
			Entry("any IPv4 destination with exceptions", "0.0.0.0/0", "10.0.0.0/8", "172.16.0.0/12"),
			Entry("IPv4 CIDR with host bits set", "10.1.2.3/8"),
			Entry("IPv4 address", "10.1.2.3"),
			Entry("IPv6 address", "2001:db8::1"),
			Entry("IPv6 CIDR of the maximum length", "2001:0db8:0000:0000:0000:0000:192.168.100.128/128"),
			Entry("IP addresses as exceptions", "128.116.0.0/17", "128.116.0.0", "128.116.0.255"),
		)

		DescribeTable("rejects any other value",
			func(cidr string, except ...string) { expectRejected(withIPBlock(cidr, except...), cidrError) },
			Entry("empty cidr", ""),
			Entry("not an address", "foo"),
			Entry("match terms after a CIDR", "10.0.0.0/8 || ip4"),
			Entry("prefix length out of range", "10.0.0.0/33"),
			Entry("IPv4-mapped IPv6 address", "::ffff:10.1.2.3"),
			Entry("match terms in an exception", "10.0.0.0/8", "10.1.0.0/16 || ip4"),
		)

		It("rejects a cidr longer than an IPv6 CIDR", func() {
			expectRejected(withIPBlock(strings.Repeat("1", 50)), "may not be more than 49")
		})

		It("accepts 25 exceptions and rejects 26", func() {
			exceptions := func(count int) []string {
				entries := make([]string, 0, count)
				for i := 0; i < count; i++ {
					entries = append(entries, fmt.Sprintf("10.0.%d.0/24", i))
				}
				return entries
			}
			expectAdmitted(withIPBlock("10.0.0.0/8", exceptions(25)...))
			expectRejected(withIPBlock("10.0.0.0/8", exceptions(26)...), "must have at most 25 items")
		})

		It("accepts 100 destinations and rejects 101", func() {
			destinations := func(count int) networkqosv1alpha1.Classifier {
				classifier := networkqosv1alpha1.Classifier{}
				for i := 0; i < count; i++ {
					classifier.To = append(classifier.To, withIPBlock(fmt.Sprintf("10.0.%d.0/24", i)).To...)
				}
				return classifier
			}
			expectAdmitted(destinations(100))
			expectRejected(destinations(101), "must have at most 100 items")
		})

		It("ratchets the validation for a NetworkQoS stored with another cidr", func() {
			expectRatcheting(withIPBlock("10.0.0.0/8 || ip4"), cidrError, func(version map[string]interface{}) {
				_, found, err := unstructured.NestedSlice(version, append(cidrSchema, "x-kubernetes-validations")...)
				Expect(err).NotTo(HaveOccurred(), "failed to read the cidr validation of the NetworkQoS CRD")
				Expect(found).To(BeTrue(), "the NetworkQoS CRD has no cidr validation")
				unstructured.RemoveNestedField(version, append(cidrSchema, "x-kubernetes-validations")...)
			})
		})
	})
})
