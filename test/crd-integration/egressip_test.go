// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
)

var _ = Describe("EgressIP CRD", func() {
	Context("spec.egressNodeSelector defaulting", func() {
		// The +kubebuilder:default marker on EgressNodeSelector means the API
		// server fills in the egress-assignable default when the field is
		// omitted.  EgressNodeSelector is a *metav1.LabelSelector: a nil pointer
		// is omitted by encoding/json (omitempty works correctly for pointers),
		// so the typed Go client and kubectl/Helm all trigger the CRD default
		// the same way — by leaving the field nil/absent.

		It("fills in the egress-assignable default when egressNodeSelector is omitted", func() {
			eip := &egressipv1.EgressIP{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-default-",
				},
				Spec: egressipv1.EgressIPSpec{
					EgressIPs: []string{"192.0.2.1"},
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": "default"},
					},
					// EgressNodeSelector left nil — API server injects the default:
					// {matchExpressions: [{key: k8s.ovn.org/egress-assignable, operator: Exists}]}
				},
				Status: egressipv1.EgressIPStatus{Items: []egressipv1.EgressIPStatusItem{}},
			}
			Expect(k8sClient.Create(ctx, eip)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, eip) })

			Expect(eip.Spec.EgressNodeSelector.MatchExpressions).To(ConsistOf(
				metav1.LabelSelectorRequirement{
					Key:      "k8s.ovn.org/egress-assignable",
					Operator: metav1.LabelSelectorOpExists,
				},
			))
			Expect(eip.Spec.EgressNodeSelector.MatchLabels).To(BeEmpty())
		})

		It("preserves a custom egressNodeSelector when one is explicitly provided", func() {
			eip := &egressipv1.EgressIP{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-custom-selector-",
				},
				Spec: egressipv1.EgressIPSpec{
					EgressIPs: []string{"192.0.2.2"},
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": "default"},
					},
					EgressNodeSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"pool": "egress"},
					},
				},
				Status: egressipv1.EgressIPStatus{Items: []egressipv1.EgressIPStatusItem{}},
			}
			Expect(k8sClient.Create(ctx, eip)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, eip) })

			Expect(eip.Spec.EgressNodeSelector.MatchLabels).To(Equal(map[string]string{"pool": "egress"}))
			Expect(eip.Spec.EgressNodeSelector.MatchExpressions).To(BeEmpty())
		})

		It("accepts an explicit empty selector ({}) that matches all nodes", func() {
			eip := &egressipv1.EgressIP{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-empty-selector-",
				},
				Spec: egressipv1.EgressIPSpec{
					EgressIPs: []string{"192.0.2.3"},
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": "default"},
					},
					// Explicit non-nil empty selector — matches all nodes.
					// The API server must NOT replace it with the default.
					EgressNodeSelector: &metav1.LabelSelector{},
				},
				Status: egressipv1.EgressIPStatus{Items: []egressipv1.EgressIPStatusItem{}},
			}
			Expect(k8sClient.Create(ctx, eip)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, eip) })

			Expect(eip.Spec.EgressNodeSelector.MatchLabels).To(BeEmpty())
			Expect(eip.Spec.EgressNodeSelector.MatchExpressions).To(BeEmpty())
		})
	})

	Context("spec.egressNodeSelector updates", func() {
		// These lock down the mutation semantics of the pointer + default
		// design. CRD structural-schema defaulting runs on every write,
		// updates included. Because EgressNodeSelector is a *metav1.LabelSelector
		// with omitempty, a nil pointer is dropped from the PUT body, so the API
		// server re-injects the default on update — there is no way to clear the
		// selector back to nil.

		It("re-applies the default when the selector is unset (nil) on update", func() {
			eip := &egressipv1.EgressIP{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-update-unset-",
				},
				Spec: egressipv1.EgressIPSpec{
					EgressIPs: []string{"192.0.2.10"},
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": "default"},
					},
					EgressNodeSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"pool": "egress"},
					},
				},
				Status: egressipv1.EgressIPStatus{Items: []egressipv1.EgressIPStatusItem{}},
			}
			Expect(k8sClient.Create(ctx, eip)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, eip) })
			Expect(eip.Spec.EgressNodeSelector.MatchLabels).To(Equal(map[string]string{"pool": "egress"}))

			// Unset the field, then update: omitempty drops it from the PUT
			// body, so the API server re-injects the egress-assignable default.
			eip.Spec.EgressNodeSelector = nil
			Expect(k8sClient.Update(ctx, eip)).To(Succeed())

			Expect(eip.Spec.EgressNodeSelector.MatchExpressions).To(ConsistOf(
				metav1.LabelSelectorRequirement{
					Key:      "k8s.ovn.org/egress-assignable",
					Operator: metav1.LabelSelectorOpExists,
				},
			))
			Expect(eip.Spec.EgressNodeSelector.MatchLabels).To(BeEmpty())
		})

		It("preserves an explicit empty selector ({}) set on update", func() {
			eip := &egressipv1.EgressIP{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-update-empty-",
				},
				Spec: egressipv1.EgressIPSpec{
					EgressIPs: []string{"192.0.2.11"},
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": "default"},
					},
					EgressNodeSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"pool": "egress"},
					},
				},
				Status: egressipv1.EgressIPStatus{Items: []egressipv1.EgressIPStatusItem{}},
			}
			Expect(k8sClient.Create(ctx, eip)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, eip) })

			// A non-nil empty selector is serialized as {}, so it survives the
			// update untouched — the default must NOT overwrite it.
			eip.Spec.EgressNodeSelector = &metav1.LabelSelector{}
			Expect(k8sClient.Update(ctx, eip)).To(Succeed())

			Expect(eip.Spec.EgressNodeSelector.MatchLabels).To(BeEmpty())
			Expect(eip.Spec.EgressNodeSelector.MatchExpressions).To(BeEmpty())
		})

		It("allows switching from the default to a custom selector on update", func() {
			eip := &egressipv1.EgressIP{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-update-custom-",
				},
				Spec: egressipv1.EgressIPSpec{
					EgressIPs: []string{"192.0.2.12"},
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": "default"},
					},
					// Omitted — starts life with the injected default.
				},
				Status: egressipv1.EgressIPStatus{Items: []egressipv1.EgressIPStatusItem{}},
			}
			Expect(k8sClient.Create(ctx, eip)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, eip) })
			Expect(eip.Spec.EgressNodeSelector.MatchExpressions).To(ConsistOf(
				metav1.LabelSelectorRequirement{
					Key:      "k8s.ovn.org/egress-assignable",
					Operator: metav1.LabelSelectorOpExists,
				},
			))

			eip.Spec.EgressNodeSelector = &metav1.LabelSelector{
				MatchLabels: map[string]string{"pool": "egress"},
			}
			Expect(k8sClient.Update(ctx, eip)).To(Succeed())

			Expect(eip.Spec.EgressNodeSelector.MatchLabels).To(Equal(map[string]string{"pool": "egress"}))
			Expect(eip.Spec.EgressNodeSelector.MatchExpressions).To(BeEmpty())
		})
	})
})
