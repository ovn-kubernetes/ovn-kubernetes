// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	eipclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned"
)

var _ = Describe("EgressIP node condition admission policy", func() {
	It("allows condition reports while protecting specs and assignments", func() {
		data, err := os.ReadFile(filepath.Join(crdDir(), "..", "templates", "egressip-node-condition-ownership.yaml"))
		Expect(err).NotTo(HaveOccurred(), "read the deployed ownership policy")
		_, manifests, found := strings.Cut(string(data), "*/ -}}\n")
		Expect(found).To(BeTrue(), "strip the Helm comment")
		policyYAML, bindingYAML, found := strings.Cut(manifests, "\n---\n")
		Expect(found).To(BeTrue(), "separate the policy and binding")
		var policy admissionv1.ValidatingAdmissionPolicy
		var binding admissionv1.ValidatingAdmissionPolicyBinding
		Expect(yaml.Unmarshal([]byte(policyYAML), &policy)).To(Succeed(), "decode the policy")
		Expect(yaml.Unmarshal([]byte(bindingYAML), &binding)).To(Succeed(), "decode the binding")
		kube, err := kubernetes.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred(), "create the admission registration client")
		_, err = kube.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, &policy, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "install the ownership policy")
		DeferCleanup(kube.AdmissionregistrationV1().ValidatingAdmissionPolicies().Delete, ctx, policy.Name, metav1.DeleteOptions{})
		_, err = kube.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, &binding, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "bind the ownership policy")
		DeferCleanup(kube.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Delete, ctx, binding.Name, metav1.DeleteOptions{})

		admin, err := eipclient.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred(), "create the EgressIP client")
		owner, err := admin.K8sV1().EgressIPs().Create(ctx, &eipv1.EgressIP{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "condition-ownership-"},
			Spec:       eipv1.EgressIPSpec{EgressIPs: []string{"192.0.2.42"}},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "create an EgressIP without status")
		DeferCleanup(admin.K8sV1().EgressIPs().Delete, ctx, owner.Name, metav1.DeleteOptions{})

		// Grant authorization in the fixture so a Forbidden result tests admission.
		for _, identity := range []rest.ImpersonationConfig{
			{UserName: "system:ovn-node:node1", Groups: []string{"system:masters", "system:ovn-nodes"}},
			{UserName: "system:serviceaccount:ovn-kubernetes:ovnkube-node", Groups: []string{"system:masters"}},
		} {
			impersonated := rest.CopyConfig(cfg)
			impersonated.Impersonate = identity
			node, err := eipclient.NewForConfig(impersonated)
			Expect(err).NotTo(HaveOccurred(), "create the node EgressIP client")
			patch := func(body string) error {
				_, err := node.K8sV1().EgressIPs().Patch(ctx, owner.Name, types.MergePatchType, []byte(body), metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}})
				return err
			}
			Eventually(func() bool {
				return apierrors.IsForbidden(patch(`{"spec":{"egressIPs":["192.0.2.43"]}}`))
			}, 10*time.Second).Should(BeTrue(), "deny spec edits from %s", identity.UserName)
			Expect(apierrors.IsForbidden(patch(`{"status":{"items":[{"node":"node1","egressIP":"192.0.2.42"}]}}`))).To(BeTrue(), "deny assignment edits from %s", identity.UserName)
			Expect(patch(`{"status":{"conditions":[{"type":"TrafficConflict","status":"False","reason":"NoOverlappingTraffic","message":"No conflict","lastTransitionTime":"2026-01-01T00:00:00Z"}]}}`)).To(Succeed(), "allow condition reports from %s", identity.UserName)
		}

		_, err = admin.K8sV1().EgressIPs().Patch(ctx, owner.Name, types.MergePatchType,
			[]byte(`{"status":{"items":[{"node":"node1","egressIP":"192.0.2.42"}]}}`), metav1.PatchOptions{})
		Expect(err).NotTo(HaveOccurred(), "allow an authorized non-node writer to set assignments")
	})
})
