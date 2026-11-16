// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/yaml"

	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned"
	uplinkscheme "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned/scheme"
	uplinktyped "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned/typed/uplink/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovnwebhook"
)

var _ = Describe("UplinkState CRD", func() {
	// maxDefaultGateways mirrors the MaxItems marker on status.defaultGateways.
	const maxDefaultGateways = 256

	// uplinkStateWithDefaultGateways builds an UplinkState whose status carries
	// n distinct default gateway addresses. The status has no subresource, so
	// it is validated on Create.
	uplinkStateWithDefaultGateways := func(n int) *uplinkv1alpha1.UplinkState {
		gateways := make([]uplinkv1alpha1.IPAddress, 0, n)
		for i := 0; i < n; i++ {
			if i%2 == 0 {
				gateways = append(gateways, uplinkv1alpha1.IPAddress(fmt.Sprintf("192.0.2.%d", i/2+1)))
			} else {
				gateways = append(gateways, uplinkv1alpha1.IPAddress(fmt.Sprintf("2001:db8::%x", i/2+1)))
			}
		}
		return &uplinkv1alpha1.UplinkState{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "test-default-gateways-"},
			Spec: uplinkv1alpha1.UplinkStateSpec{
				UplinkName: "uplink",
				NodeName:   "node",
			},
			Status: uplinkv1alpha1.UplinkStateStatus{DefaultGateways: gateways},
		}
	}

	Context("status.defaultGateways", func() {
		It("accepts no default gateways", func() {
			state := uplinkStateWithDefaultGateways(0)
			Expect(k8sClient.Create(ctx, state)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, state) })
			Expect(state.Status.DefaultGateways).To(BeEmpty())
		})

		It("accepts the maximum number of default gateways across both IP families", func() {
			state := uplinkStateWithDefaultGateways(maxDefaultGateways)
			expectedGateways := append([]uplinkv1alpha1.IPAddress(nil), state.Status.DefaultGateways...)
			Expect(k8sClient.Create(ctx, state)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, state) })
			Expect(state.Status.DefaultGateways).To(Equal(expectedGateways))
		})

		It("rejects more default gateways than the maximum", func() {
			state := uplinkStateWithDefaultGateways(maxDefaultGateways + 1)
			err := k8sClient.Create(ctx, state)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected a CRD validation error: %v", err)
			Expect(err.Error()).To(ContainSubstring("Too many"))
			Expect(err.Error()).To(ContainSubstring("status.defaultGateways"))
			Expect(err.Error()).To(ContainSubstring("256"))
		})
	})
})

var _ = Describe("UplinkState ownership admission", func() {
	It("enforces ownership for create, update, patch and delete", func() {
		kube, err := kubernetes.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		adminClient, err := uplinkclient.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		admin := adminClient.K8sV1alpha1().UplinkStates()
		uplink, err := adminClient.K8sV1alpha1().Uplinks().Create(ctx, &uplinkv1alpha1.Uplink{
			ObjectMeta: metav1.ObjectMeta{Name: "admission-test"},
			Spec: uplinkv1alpha1.UplinkSpec{NodeConfigs: []uplinkv1alpha1.UplinkNodeConfig{{
				Type:              uplinkv1alpha1.UplinkTypeOVSBridge,
				NodeSelector:      metav1.LabelSelector{MatchLabels: map[string]string{"role": "blue"}},
				HostInterfaceName: "br-blue",
			}}},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			Expect(adminClient.K8sV1alpha1().Uplinks().Delete(ctx, uplink.Name, metav1.DeleteOptions{})).To(Succeed())
		})
		state := func(nodeName string) *uplinkv1alpha1.UplinkState {
			return &uplinkv1alpha1.UplinkState{
				ObjectMeta: metav1.ObjectMeta{Name: "admission-test." + nodeName, OwnerReferences: []metav1.OwnerReference{{
					APIVersion: uplinkv1alpha1.SchemeGroupVersion.String(), Kind: "Uplink", Name: uplink.Name, UID: uplink.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(false),
				}}},
				Spec: uplinkv1alpha1.UplinkStateSpec{UplinkName: "admission-test", NodeName: nodeName},
			}
		}
		// Seed pre-upgrade objects before installing admission. Existing
		// references may block owner deletion or omit the flag entirely.
		legacyStates := map[string]*uplinkv1alpha1.UplinkState{}
		for _, method := range []string{"update", "merge", "json", "apply"} {
			for _, blocking := range []bool{true, false} {
				name := "legacy-" + method
				if !blocking {
					name += "-unset"
				}
				legacy := state(name)
				legacy.OwnerReferences[0].BlockOwnerDeletion = nil
				if blocking {
					legacy.OwnerReferences[0].BlockOwnerDeletion = ptr.To(true)
				}
				legacy, err = admin.Create(ctx, legacy, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				legacyStates[name] = legacy
				DeferCleanup(func() { Expect(admin.Delete(ctx, legacy.Name, metav1.DeleteOptions{})).To(Succeed()) })
			}
		}

		validator := admission.WithValidator(uplinkscheme.Scheme,
			ovnwebhook.NewUplinkStateAdmissionWebhook())
		var webhookCalls atomic.Int32
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			webhookCalls.Add(1)
			validator.ServeHTTP(w, r)
		}))
		DeferCleanup(server.Close)
		template, err := os.ReadFile(filepath.Join(crdDir(), "..", "charts", "ovnkube-identity", "templates", "webhook.yaml"))
		Expect(err).NotTo(HaveOccurred())
		_, document, found := strings.Cut(string(template), "name: ovn-kubernetes-admission-webhook-uplinkstate\n")
		Expect(found).To(BeTrue())
		document = "apiVersion: admissionregistration.k8s.io/v1\nkind: ValidatingWebhookConfiguration\nmetadata:\n  name: ovn-kubernetes-admission-webhook-uplinkstate\n" + strings.TrimSuffix(document, "{{- end }}\n")
		document = strings.ReplaceAll(document, "{{ $ca.Cert | b64enc | quote }}", "''")
		webhook := &admissionregistrationv1.ValidatingWebhookConfiguration{}
		Expect(yaml.Unmarshal([]byte(document), webhook)).To(Succeed())
		webhook.Webhooks[0].ClientConfig.URL = ptr.To(server.URL + "/uplinkstate")
		webhook.Webhooks[0].ClientConfig.CABundle = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
		Expect(webhook.Webhooks[0].FailurePolicy).To(HaveValue(Equal(admissionregistrationv1.Fail)))
		Expect(webhook.Webhooks[0].Rules).To(HaveLen(1))
		Expect(webhook.Webhooks[0].Rules[0].Operations).To(Equal([]admissionregistrationv1.OperationType{admissionregistrationv1.Create}))
		_, err = kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(ctx, webhook, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			Expect(kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().Delete(ctx, webhook.Name, metav1.DeleteOptions{})).To(Succeed())
		})

		policyTemplate, err := os.ReadFile(filepath.Join(crdDir(), "..", "charts", "ovnkube-identity", "templates", "uplinkstate-admission-policy.yaml"))
		Expect(err).NotTo(HaveOccurred())
		_, policyDocuments, found := strings.Cut(string(policyTemplate), "\n")
		Expect(found).To(BeTrue())
		policyDocuments = strings.TrimSuffix(policyDocuments, "{{- end }}\n")
		policyYAML, bindingYAML, found := strings.Cut(policyDocuments, "\n---\n")
		Expect(found).To(BeTrue())
		policy := &admissionregistrationv1.ValidatingAdmissionPolicy{}
		policyBinding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
		Expect(yaml.Unmarshal([]byte(policyYAML), policy)).To(Succeed())
		Expect(yaml.Unmarshal([]byte(bindingYAML), policyBinding)).To(Succeed())
		Expect(policy.Spec.FailurePolicy).To(HaveValue(Equal(admissionregistrationv1.Fail)))
		Expect(policyBinding.Spec.ValidationActions).To(ConsistOf(admissionregistrationv1.Deny))
		_, err = kube.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, policy, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			Expect(kube.AdmissionregistrationV1().ValidatingAdmissionPolicies().Delete(ctx, policy.Name, metav1.DeleteOptions{})).To(Succeed())
		})
		_, err = kube.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, policyBinding, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			Expect(kube.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Delete(ctx, policyBinding.Name, metav1.DeleteOptions{})).To(Succeed())
		})

		// Exercise the deployed node lifecycle verbs, including create/delete.
		nodeRBAC, err := os.ReadFile(filepath.Join(crdDir(), "..", "templates", "rbac-ovnkube-node.yaml"))
		Expect(err).NotTo(HaveOccurred())
		_, nodeRule, found := strings.Cut(string(nodeRBAC), "resources:\n          - uplinkstates\n")
		Expect(found).To(BeTrue())
		nodeRule, _, _ = strings.Cut(nodeRule, "\n    {{-")
		writeRule := rbacv1.PolicyRule{}
		Expect(yaml.Unmarshal([]byte("apiGroups: [k8s.ovn.org]\nresources: [uplinkstates]\n"+strings.TrimSpace(nodeRule)), &writeRule)).To(Succeed())
		role := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: "uplinkstate-ownership-test"},
			Rules: []rbacv1.PolicyRule{writeRule, {
				APIGroups: []string{"k8s.ovn.org"}, Resources: []string{"uplinkstates"}, Verbs: []string{"get"},
			}},
		}
		_, err = kube.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			Expect(kube.RbacV1().ClusterRoles().Delete(ctx, role.Name, metav1.DeleteOptions{})).To(Succeed())
		})
		binding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: role.Name},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
			Subjects: []rbacv1.Subject{
				{APIGroup: rbacv1.GroupName, Kind: "Group", Name: "system:ovn-nodes"},
				{APIGroup: rbacv1.GroupName, Kind: "User", Name: "uplinkstate-other-writer"},
				{APIGroup: rbacv1.GroupName, Kind: "User", Name: "system:serviceaccount:ovn-kubernetes:ovnkube-cluster-manager"},
				{APIGroup: rbacv1.GroupName, Kind: "User", Name: "system:serviceaccount:kube-system:generic-garbage-collector"},
			},
		}
		_, err = kube.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			Expect(kube.RbacV1().ClusterRoleBindings().Delete(ctx, binding.Name, metav1.DeleteOptions{})).To(Succeed())
		})

		nodeConfig := rest.CopyConfig(cfg)
		nodeConfig.Impersonate = rest.ImpersonationConfig{UserName: "system:ovn-node:node-a", Groups: []string{"system:ovn-nodes"}}
		nodeClient, err := uplinkclient.NewForConfig(nodeConfig)
		Expect(err).NotTo(HaveOccurred())
		node := nodeClient.K8sV1alpha1().UplinkStates()
		otherConfig := rest.CopyConfig(cfg)
		otherConfig.Impersonate = rest.ImpersonationConfig{UserName: "uplinkstate-other-writer"}
		otherClient, err := uplinkclient.NewForConfig(otherConfig)
		Expect(err).NotTo(HaveOccurred())
		victimConfig := rest.CopyConfig(nodeConfig)
		victimConfig.Impersonate.UserName = "system:ovn-node:node-b"
		victimClient, err := uplinkclient.NewForConfig(victimConfig)
		Expect(err).NotTo(HaveOccurred())
		var victim *uplinkv1alpha1.UplinkState
		Eventually(func() error {
			var createErr error
			victim, createErr = victimClient.K8sV1alpha1().UplinkStates().Create(ctx, state("node-b"), metav1.CreateOptions{})
			return createErr
		}).WithTimeout(10 * time.Second).Should(Succeed())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			err := admin.Delete(ctx, victim.Name, metav1.DeleteOptions{})
			Expect(err == nil || apierrors.IsNotFound(err)).To(BeTrue())
		})
		own := state("node-a")
		_, err = node.Create(ctx, own, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			err := admin.Delete(ctx, own.Name, metav1.DeleteOptions{})
			Expect(err == nil || apierrors.IsNotFound(err)).To(BeTrue())
		})

		// Wait for admission registration, and require the policy's denial
		// message so RBAC propagation cannot masquerade as successful validation.
		Eventually(func() string {
			current, err := admin.Get(ctx, victim.Name, metav1.GetOptions{})
			if err != nil {
				return err.Error()
			}
			_, err = node.Update(ctx, current, metav1.UpdateOptions{})
			if err == nil {
				return ""
			}
			return err.Error()
		}).WithTimeout(10 * time.Second).Should(ContainSubstring("only the owning OVN node identity may write UplinkState"))

		// Even an object claiming node-a in spec must not reserve another
		// node's canonical name. Apply-to-create must reach the same validation.
		for _, name := range []string{"admission-test.node-c", "uplinkstate-a123456789"} {
			forged := state("node-a")
			forged.Name = name
			forged.APIVersion = "k8s.ovn.org/v1alpha1"
			forged.Kind = "UplinkState"
			_, err = node.Create(ctx, forged, metav1.CreateOptions{})
			Expect(apierrors.IsForbidden(err)).To(BeTrue())
			Expect(err).To(MatchError(ContainSubstring("UplinkState name must match its Uplink and node identity")))
			data, err := json.Marshal(forged)
			Expect(err).NotTo(HaveOccurred())
			_, err = node.Patch(ctx, name, types.ApplyPatchType, data,
				metav1.PatchOptions{FieldManager: "ovnkube-node-uplink-controller", Force: ptr.To(true)})
			Expect(apierrors.IsForbidden(err)).To(BeTrue())
			Expect(err).To(MatchError(ContainSubstring("UplinkState name must match its Uplink and node identity")))
			_, err = admin.Get(ctx, name, metav1.GetOptions{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}

		By("rejecting create-time finalizers and privileged owner references")
		poisoned := state("node-a")
		Expect(node.Delete(ctx, own.Name, metav1.DeleteOptions{})).To(Succeed())
		poisoned.Finalizers = []string{"example.com/block-cleanup"}
		_, err = node.Create(ctx, poisoned, metav1.CreateOptions{})
		Expect(err).To(MatchError(ContainSubstring("may not set UplinkState finalizers")))
		poisoned.Finalizers = nil
		poisoned.OwnerReferences[0].BlockOwnerDeletion = ptr.To(true)
		_, err = node.Create(ctx, poisoned, metav1.CreateOptions{})
		Expect(err).To(MatchError(ContainSubstring("cannot set blockOwnerDeletion")))
		_, err = node.Create(ctx, own, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		callsBeforeUpdates := webhookCalls.Load()
		By("enforcing spec identity immutability through the CRD for the owning node")
		for _, field := range []string{"nodeName", "uplinkName"} {
			current, err := node.Get(ctx, own.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			if field == "nodeName" {
				current.Spec.NodeName = "node-b"
			} else {
				current.Spec.UplinkName = "other-uplink"
			}
			_, err = node.Update(ctx, current, metav1.UpdateOptions{})
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected CRD validation: %v", err)
			Expect(err).To(MatchError(ContainSubstring(field + " is immutable")))
			for _, patchType := range []types.PatchType{types.MergePatchType, types.JSONPatchType, types.ApplyPatchType} {
				var payload interface{} = map[string]interface{}{
					"apiVersion": uplinkv1alpha1.SchemeGroupVersion.String(),
					"kind":       "UplinkState",
					"metadata":   map[string]interface{}{"name": own.Name},
					"spec":       current.Spec,
				}
				if patchType == types.JSONPatchType {
					payload = []map[string]interface{}{{"op": "replace", "path": "/spec", "value": current.Spec}}
				}
				data, err := json.Marshal(payload)
				Expect(err).NotTo(HaveOccurred())
				options := metav1.PatchOptions{FieldManager: "ovnkube-node-uplink-controller"}
				if patchType == types.ApplyPatchType {
					options.Force = ptr.To(true)
				}
				_, err = node.Patch(ctx, own.Name, patchType, data, options)
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected CRD validation: %v", err)
				Expect(err).To(MatchError(ContainSubstring(field + " is immutable")))
			}
		}
		for _, writer := range []struct {
			name   string
			client uplinktyped.UplinkStateInterface
		}{
			{"another node", node},
			{"administrator", admin},
			{"non-node writer", otherClient.K8sV1alpha1().UplinkStates()},
		} {
			By("rejecting updates and patches from " + writer.name)
			current, err := admin.Get(ctx, victim.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			current.Status.DefaultGateways = []uplinkv1alpha1.IPAddress{"192.0.2.99"}
			_, err = writer.client.Update(ctx, current, metav1.UpdateOptions{})
			Expect(err).To(MatchError(ContainSubstring("only the owning OVN node identity may write UplinkState")))
			for _, patch := range []struct {
				typeName types.PatchType
				data     string
			}{
				{types.MergePatchType, `{"status":{"defaultGateways":["192.0.2.99"]}}`},
				{types.JSONPatchType, `[{"op":"add","path":"/status","value":{"defaultGateways":["192.0.2.99"]}}]`},
				{types.ApplyPatchType, `{"apiVersion":"k8s.ovn.org/v1alpha1","kind":"UplinkState","metadata":{"name":"admission-test.node-b"},"status":{"defaultGateways":["192.0.2.99"]}}`},
			} {
				_, err = writer.client.Patch(ctx, victim.Name, patch.typeName, []byte(patch.data), metav1.PatchOptions{FieldManager: "ovnkube-node-uplink-controller"})
				Expect(err).To(MatchError(ContainSubstring("only the owning OVN node identity may write UplinkState")))
			}
		}
		Expect(node.Delete(ctx, victim.Name, metav1.DeleteOptions{})).To(MatchError(ContainSubstring("OVN node identities may only delete their own UplinkState")))
		stored, err := admin.Get(ctx, victim.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(stored.Status.DefaultGateways).To(BeEmpty())

		By("rejecting finalizer additions even from the owning node")
		current, err := node.Get(ctx, own.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		current.Finalizers = []string{"example.com/block-cleanup"}
		_, err = node.Update(ctx, current, metav1.UpdateOptions{})
		Expect(err).To(MatchError(ContainSubstring("OVN node identities may not change UplinkState finalizers")))
		for _, patch := range []struct {
			typeName types.PatchType
			data     string
		}{
			{types.MergePatchType, `{"metadata":{"finalizers":["example.com/block-cleanup"]}}`},
			{types.JSONPatchType, `[{"op":"add","path":"/metadata/finalizers","value":["example.com/block-cleanup"]}]`},
			{types.ApplyPatchType, `{"apiVersion":"k8s.ovn.org/v1alpha1","kind":"UplinkState","metadata":{"name":"admission-test.node-a","finalizers":["example.com/block-cleanup"]}}`},
		} {
			options := metav1.PatchOptions{FieldManager: "ovnkube-node-uplink-controller"}
			if patch.typeName == types.ApplyPatchType {
				options.Force = ptr.To(true)
			}
			_, err = node.Patch(ctx, own.Name, patch.typeName, []byte(patch.data), options)
			Expect(err).To(MatchError(ContainSubstring("OVN node identities may not change UplinkState finalizers")))
		}
		stored, err = admin.Get(ctx, own.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(stored.Finalizers).To(BeEmpty())

		By("treating absent and empty finalizers equally")
		for _, patch := range []string{`{"metadata":{"finalizers":[]}}`, `{"metadata":{"finalizers":null}}`} {
			_, err = node.Patch(ctx, own.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		updated, err := node.Patch(ctx, own.Name, types.MergePatchType, []byte(`{"status":{"defaultGateways":["192.0.2.1"]}}`), metav1.PatchOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(updated.Status.DefaultGateways).To(Equal([]uplinkv1alpha1.IPAddress{"192.0.2.1"}))
		updated.Status.MACAddress = "02:00:00:00:00:01"
		_, err = node.Update(ctx, updated, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		// Discovery publishes status with server-side apply and no spec in
		// the patch. Admission must validate the merged object successfully.
		_, err = node.Patch(ctx, own.Name, types.ApplyPatchType,
			[]byte(`{"apiVersion":"k8s.ovn.org/v1alpha1","kind":"UplinkState","metadata":{"name":"admission-test.node-a"},"status":{"defaultGateways":["192.0.2.2"]}}`),
			metav1.PatchOptions{FieldManager: "ovnkube-node-uplink-controller", Force: ptr.To(true)})
		Expect(err).NotTo(HaveOccurred())
		Expect(webhookCalls.Load()).To(Equal(callsBeforeUpdates), "status writes, finalizer denials and deletes must not invoke the webhook")

		By("rejecting owner-reference changes through VAP without calling the webhook")
		const ownerDenial = "UplinkState owner references are immutable"
		for _, patch := range []string{
			`{"metadata":{"ownerReferences":[]}}`,
			`{"metadata":{"ownerReferences":null}}`,
		} {
			_, err = node.Patch(ctx, own.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
			Expect(err).To(MatchError(ContainSubstring(ownerDenial)))
		}
		for _, mutate := range []func(*uplinkv1alpha1.UplinkState){
			func(s *uplinkv1alpha1.UplinkState) { s.OwnerReferences[0].UID = "forged-owner" },
			func(s *uplinkv1alpha1.UplinkState) { s.OwnerReferences[0].Name = "other-uplink" },
			func(s *uplinkv1alpha1.UplinkState) { s.OwnerReferences[0].Kind = "Node" },
			func(s *uplinkv1alpha1.UplinkState) { s.OwnerReferences[0].APIVersion = "v1" },
			func(s *uplinkv1alpha1.UplinkState) { s.OwnerReferences[0].Controller = ptr.To(false) },
			func(s *uplinkv1alpha1.UplinkState) { s.OwnerReferences[0].Controller = nil },
			func(s *uplinkv1alpha1.UplinkState) { s.OwnerReferences[0].BlockOwnerDeletion = nil },
			func(s *uplinkv1alpha1.UplinkState) {
				extra := s.OwnerReferences[0]
				extra.UID = "extra-owner"
				extra.Controller = ptr.To(false)
				s.OwnerReferences = append(s.OwnerReferences, extra)
			},
		} {
			current, err = node.Get(ctx, own.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			mutate(current)
			_, err = node.Update(ctx, current, metav1.UpdateOptions{})
			Expect(err).To(MatchError(ContainSubstring(ownerDenial)))
		}
		Expect(webhookCalls.Load()).To(Equal(callsBeforeUpdates))

		// Own-state deletion and recreation remain available to node agents.
		Expect(node.Delete(ctx, own.Name, metav1.DeleteOptions{})).To(Succeed())
		own.APIVersion = "k8s.ovn.org/v1alpha1"
		own.Kind = "UplinkState"
		data, err := json.Marshal(own)
		Expect(err).NotTo(HaveOccurred())
		_, err = node.Patch(ctx, own.Name, types.ApplyPatchType, data,
			metav1.PatchOptions{FieldManager: "ovnkube-node-uplink-controller", Force: ptr.To(true)})
		Expect(err).NotTo(HaveOccurred())
		By("allowing ordinary updates and cleanup while the webhook is unavailable")
		server.Close()
		_, err = node.Patch(ctx, own.Name, types.MergePatchType, []byte(`{"status":{"defaultGateways":["192.0.2.3"]}}`), metav1.PatchOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = node.Patch(ctx, victim.Name, types.MergePatchType, []byte(`{"status":{"defaultGateways":["192.0.2.3"]}}`), metav1.PatchOptions{})
		Expect(err).To(MatchError(ContainSubstring("only the owning OVN node identity may write UplinkState")))
		Expect(node.Delete(ctx, victim.Name, metav1.DeleteOptions{})).To(MatchError(ContainSubstring("may only delete their own UplinkState")))
		// Owner changes are rejected by VAP even while the webhook is offline.
		_, err = node.Patch(ctx, own.Name, types.MergePatchType, []byte(`{"metadata":{"ownerReferences":[]}}`), metav1.PatchOptions{})
		Expect(err).To(MatchError(ContainSubstring(ownerDenial)))
		By("clearing legacy blockOwnerDeletion through VAP while the webhook is offline")
		for _, method := range []string{"update", "merge", "json", "apply"} {
			for _, blocking := range []bool{true, false} {
				name := "legacy-" + method
				if !blocking {
					name += "-unset"
				}
				legacy := legacyStates[name].DeepCopy()
				ownerConfig := rest.CopyConfig(nodeConfig)
				ownerConfig.Impersonate.UserName = "system:ovn-node:" + name
				ownerClient, err := uplinkclient.NewForConfig(ownerConfig)
				Expect(err).NotTo(HaveOccurred())
				owner := ownerClient.K8sV1alpha1().UplinkStates()
				// Unchanged legacy references must not block ordinary status writes.
				legacy.Status.MACAddress = "02:00:00:00:00:01"
				legacy, err = owner.Update(ctx, legacy, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
				legacy.OwnerReferences[0].BlockOwnerDeletion = ptr.To(false)
				for _, forbiddenWriter := range []uplinktyped.UplinkStateInterface{node, admin} {
					_, err = forbiddenWriter.Update(ctx, legacy, metav1.UpdateOptions{})
					Expect(err).To(MatchError(ContainSubstring("only the owning OVN node identity")))
				}
				switch method {
				case "update":
					_, err = owner.Update(ctx, legacy, metav1.UpdateOptions{})
				case "merge", "apply":
					payload, marshalErr := json.Marshal(map[string]interface{}{
						"apiVersion": uplinkv1alpha1.SchemeGroupVersion.String(),
						"kind":       "UplinkState",
						"metadata":   map[string]interface{}{"name": legacy.Name, "ownerReferences": legacy.OwnerReferences},
					})
					Expect(marshalErr).NotTo(HaveOccurred())
					patchType := types.MergePatchType
					if method == "apply" {
						patchType = types.ApplyPatchType
					}
					options := metav1.PatchOptions{FieldManager: "ovnkube-node-uplink-controller"}
					if method == "apply" {
						options.Force = ptr.To(true)
					}
					_, err = owner.Patch(ctx, legacy.Name, patchType, payload, options)
				case "json":
					_, err = owner.Patch(ctx, legacy.Name, types.JSONPatchType,
						[]byte(`[{"op":"add","path":"/metadata/ownerReferences/0/blockOwnerDeletion","value":false}]`), metav1.PatchOptions{})
				}
				Expect(err).NotTo(HaveOccurred())
				stored, err := owner.Get(ctx, legacy.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(stored.OwnerReferences).To(Equal(legacy.OwnerReferences))
				// Turning blocking back on must fail even for this owning node.
				stored.OwnerReferences[0].BlockOwnerDeletion = ptr.To(true)
				_, err = owner.Update(ctx, stored, metav1.UpdateOptions{})
				Expect(err).To(HaveOccurred())
			}
		}
		// Give the owning identity permission to modify owner finalizers so
		// this rejection must come from VAP, not OwnerReferencesPermissionEnforcement.
		privilegedConfig := rest.CopyConfig(cfg)
		privilegedConfig.Impersonate = rest.ImpersonationConfig{
			UserName: "system:ovn-node:legacy-update", Groups: []string{"system:masters"},
		}
		privilegedClient, err := uplinkclient.NewForConfig(privilegedConfig)
		Expect(err).NotTo(HaveOccurred())
		legacy, err := admin.Get(ctx, legacyStates["legacy-update"].Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		legacy.OwnerReferences[0].BlockOwnerDeletion = ptr.To(true)
		_, err = privilegedClient.K8sV1alpha1().UplinkStates().Update(ctx, legacy, metav1.UpdateOptions{})
		Expect(err).To(MatchError(ContainSubstring(ownerDenial)))

		for _, cleanup := range []struct{ user, stateName string }{
			{"system:serviceaccount:ovn-kubernetes:ovnkube-cluster-manager", own.Name},
			{"system:serviceaccount:kube-system:generic-garbage-collector", victim.Name},
		} {
			cleanupConfig := rest.CopyConfig(cfg)
			cleanupConfig.Impersonate = rest.ImpersonationConfig{UserName: cleanup.user}
			cleanupClient, err := uplinkclient.NewForConfig(cleanupConfig)
			Expect(err).NotTo(HaveOccurred())
			Expect(cleanupClient.K8sV1alpha1().UplinkStates().Delete(ctx, cleanup.stateName, metav1.DeleteOptions{})).To(Succeed())
		}
		_, err = node.Create(ctx, own, metav1.CreateOptions{})
		Expect(err).To(MatchError(ContainSubstring("failed calling webhook")))
		_, err = admin.Get(ctx, own.Name, metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})
})
