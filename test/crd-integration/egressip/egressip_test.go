// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
)

var _ = Describe("EgressIP CRD", func() {
	Context("spec.egressNodeSelector defaulting", func() {
		// The +kubebuilder:default marker on EgressNodeSelector means the API
		// server must fill in the egress-assignable default when the field is
		// omitted.  This preserves backwards-compatible behaviour: before
		// egressNodeSelector was introduced, the controller selected nodes by
		// checking the k8s.ovn.org/egress-assignable label directly.

		It("fills in the egress-assignable default when egressNodeSelector is omitted", func() {
			// WHY unstructured instead of the typed egressipv1.EgressIP client:
			//
			// CRD admission defaulting fires only when the field is ABSENT from
			// the JSON payload sent to the API server.  Go's encoding/json never
			// omits a plain (non-pointer) struct field — even at its zero value —
			// because omitempty has no effect on structs.  EgressNodeSelector is
			// defined as:
			//
			//   EgressNodeSelector metav1.LabelSelector `json:"egressNodeSelector,omitempty"`
			//
			// so the typed client always serialises it as `"egressNodeSelector":{}`
			// even when the caller leaves it unset.  The API server sees the field
			// as explicitly present (empty) and skips the default.
			//
			// Using an Unstructured object lets us build a JSON payload that
			// truly omits the key, exactly as `kubectl apply` does when a user
			// writes an EgressIP YAML without egressNodeSelector.  That is the
			// real-world path the default is designed to protect.
			//
			// YAML / kubectl / Helm users are unaffected: those tools serialise
			// only fields that appear in the manifest, so an absent
			// egressNodeSelector produces a payload with no "egressNodeSelector"
			// key and the API server correctly injects the default.  This
			// limitation is not specific to egressNodeSelector — it applies to
			// every CRD field that is a plain struct type (not a pointer) with a
			// +kubebuilder:default marker.  NamespaceSelector and PodSelector
			// on this very type have the same property; their zero values are
			// also always serialised.  The difference is that those fields are
			// required or intentionally default to "match nothing" / "match all",
			// so the absence-vs-zero ambiguity does not matter there.
			//
			// Keeping the field as a non-pointer struct is intentional and
			// consistent with other selector fields in this codebase (e.g.
			// NetworkSelector in pkg/crd/types/networkselector.go).
			//
			// Semantically there are three cases:
			//   1. absent / unspecified → default (egress-assignable: Exists)
			//   2. explicit empty {}    → match all nodes
			//   3. custom selector      → specific node pool
			//
			// For Go JSON (encoding/json), cases 1 and 2 are indistinguishable
			// in Go client code: both result in the zero-value struct after
			// unmarshalling.  YAML/kubectl/Helm users CAN distinguish them —
			// omitting the field triggers the CRD default (egress-assignable),
			// while setting {} explicitly stores an empty selector (match all).
			// There is no real-world need to distinguish "unspecified" from
			// "empty" at the Go-client level, so a pointer type is not warranted.
			//
			// A Go operator that wants the backwards-compatible behaviour does
			// NOT rely on the CRD default mechanism — it sets the selector
			// explicitly:
			//
			//   eip.Spec.EgressNodeSelector = metav1.LabelSelector{
			//       MatchExpressions: []metav1.LabelSelectorRequirement{{
			//           Key:      "k8s.ovn.org/egress-assignable",
			//           Operator: metav1.LabelSelectorOpExists,
			//       }},
			//   }
			//
			// This is the recommended pattern for Go operators: because the
			// typed client always serialises the zero-value struct, it can
			// never distinguish "not set" from "set to empty", so relying on
			// CRD defaults from Go code is fragile.  See
			// https://ahmet.im/blog/crd-generation-pitfalls/ ("A major pitfall
			// … your tests will not fail") and the Kubernetes API conventions
			// discussion at https://github.com/kubernetes/community/pull/8486.
			obj := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "k8s.ovn.org/v1",
					"kind":       "EgressIP",
					"metadata": map[string]interface{}{
						"generateName": "test-default-",
					},
					"spec": map[string]interface{}{
						"egressIPs": []interface{}{"192.0.2.1"},
						"namespaceSelector": map[string]interface{}{
							"matchLabels": map[string]interface{}{
								"kubernetes.io/metadata.name": "default",
							},
						},
						// egressNodeSelector is intentionally absent from this
						// map so that the API server applies the CRD default:
						// {matchExpressions: [{key: k8s.ovn.org/egress-assignable, operator: Exists}]}
					},
					// status.items is required by the CRD schema (no omitempty
					// on the Go field); provide an empty list to satisfy it.
					"status": map[string]interface{}{
						"items": []interface{}{},
					},
				},
			}
			obj.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "k8s.ovn.org",
				Version: "v1",
				Kind:    "EgressIP",
			})
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, obj) })

			// Read the stored object back so we get the server-applied default.
			stored := &egressipv1.EgressIP{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: obj.GetName()}, stored)).To(Succeed())

			Expect(stored.Spec.EgressNodeSelector.MatchExpressions).To(ConsistOf(
				metav1.LabelSelectorRequirement{
					Key:      "k8s.ovn.org/egress-assignable",
					Operator: metav1.LabelSelectorOpExists,
				},
			))
			Expect(stored.Spec.EgressNodeSelector.MatchLabels).To(BeEmpty())
		})

		It("typed client does NOT trigger the default when egressNodeSelector is left at zero value", func() {
			// This test is intentionally expected to fail: it demonstrates that
			// the typed Go client always serialises the zero-value
			// metav1.LabelSelector as "egressNodeSelector":{} (omitempty has no
			// effect on struct types), so the API server sees the field as
			// explicitly present and skips the CRD default.
			// Use the unstructured path (test above) when you need defaulting.
			eip := &egressipv1.EgressIP{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-typed-default-",
				},
				Spec: egressipv1.EgressIPSpec{
					EgressIPs: []string{"192.0.2.10"},
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": "default"},
					},
					// EgressNodeSelector intentionally left unset (zero value).
				},
				Status: egressipv1.EgressIPStatus{Items: []egressipv1.EgressIPStatusItem{}},
			}
			Expect(k8sClient.Create(ctx, eip)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, eip) })

			// The typed client serialised EgressNodeSelector as {} so the API
			// server never injected the default — both fields are empty/nil.
			// This is intentional: it proves the point that Go operators must
			// not rely on CRD defaulting for struct-typed fields.
			Expect(eip.Spec.EgressNodeSelector.MatchExpressions).To(BeEmpty())
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
					EgressNodeSelector: metav1.LabelSelector{
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
					// Explicit empty selector — matches all nodes.
					// The API server must NOT replace it with the default.
					EgressNodeSelector: metav1.LabelSelector{},
				},
				Status: egressipv1.EgressIPStatus{Items: []egressipv1.EgressIPStatusItem{}},
			}
			Expect(k8sClient.Create(ctx, eip)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, eip) })

			Expect(eip.Spec.EgressNodeSelector.MatchLabels).To(BeEmpty())
			Expect(eip.Spec.EgressNodeSelector.MatchExpressions).To(BeEmpty())
		})
	})
})
