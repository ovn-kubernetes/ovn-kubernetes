// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// The operator creates the components' ClusterRoles and Roles, and Kubernetes
// only lets it do so if it already holds every permission they grant (RBAC
// escalation prevention). These tests render the real templates with every
// feature gate on and check that the operator's generated role
// (config/rbac/role.yaml, from the markers in internal/controller/rbac.go):
//
//   - grants every rule in the rendered ClusterRoles and Roles,
//   - can get, list, create, patch and delete every rendered kind,
//   - has no wildcard, bind or escalate permissions, and
//   - cannot create or delete Namespaces, which the installer owns.
//
// A failure usually means a template gained a permission or a new kind: add
// the matching +kubebuilder:rbac marker and run `make manifests`.

package renderer_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/kinds"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/renderer"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type generatedOperatorRBAC struct {
	clusterRules  []rbacv1.PolicyRule
	targetRules   []rbacv1.PolicyRule
	operatorRules []rbacv1.PolicyRule
}

func loadGeneratedOperatorRBAC() generatedOperatorRBAC {
	GinkgoHelper()

	content, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	Expect(err).ToNot(HaveOccurred())

	var result generatedOperatorRBAC
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(content), 4096)
	for {
		obj := &unstructured.Unstructured{}
		err := decoder.Decode(obj)
		if errors.Is(err, io.EOF) {
			break
		}
		Expect(err).ToNot(HaveOccurred())
		if obj.GetName() != "manager-role" {
			continue
		}

		switch obj.GetKind() {
		case kinds.ClusterRole:
			var role rbacv1.ClusterRole
			Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &role)).To(Succeed())
			result.clusterRules = role.Rules
		case kinds.Role:
			var role rbacv1.Role
			Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &role)).To(Succeed())
			switch role.Namespace {
			case "ovn-kubernetes":
				result.targetRules = role.Rules
			case k8sv1alpha1.OperatorNamespace:
				result.operatorRules = role.Rules
			}
		}
	}

	Expect(result.clusterRules).ToNot(BeEmpty())
	Expect(result.targetRules).ToNot(BeEmpty())
	Expect(result.operatorRules).ToNot(BeEmpty())
	return result
}

func ruleAllows(rules []rbacv1.PolicyRule, group, resource, verb string, resourceNames []string) bool {
	for _, rule := range rules {
		if !slices.Contains(rule.APIGroups, group) || !slices.Contains(rule.Resources, resource) ||
			!slices.Contains(rule.Verbs, verb) {
			continue
		}
		if len(resourceNames) == 0 {
			if len(rule.ResourceNames) == 0 {
				return true
			}
			continue
		}
		if len(rule.ResourceNames) == 0 || containsAll(rule.ResourceNames, resourceNames) {
			return true
		}
	}
	return false
}

func containsAll(values, required []string) bool {
	for _, value := range required {
		if !slices.Contains(values, value) {
			return false
		}
	}
	return true
}

func expectRulesCovered(required, granted []rbacv1.PolicyRule) {
	GinkgoHelper()
	for _, rule := range required {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, verb := range rule.Verbs {
					Expect(ruleAllows(granted, group, resource, verb, rule.ResourceNames)).To(
						BeTrue(),
						"missing permission group=%q resource=%q verb=%q resourceNames=%v",
						group, resource, verb, rule.ResourceNames,
					)
				}
			}
		}
	}
}

func renderedRBAC(objects []*unstructured.Unstructured) (clusterRules, roleRules []rbacv1.PolicyRule) {
	GinkgoHelper()
	for _, obj := range objects {
		switch obj.GetKind() {
		case kinds.ClusterRole:
			var role rbacv1.ClusterRole
			Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &role)).To(Succeed())
			clusterRules = append(clusterRules, role.Rules...)
		case kinds.Role:
			var role rbacv1.Role
			Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &role)).To(Succeed())
			roleRules = append(roleRules, role.Rules...)
		}
	}
	return clusterRules, roleRules
}

type resourcePermission struct {
	group      string
	resource   string
	namespaced bool
}

// deployedResources lists every kind the templates render. A new kind fails
// the test until its RBAC is checked and added here.
var deployedResources = map[string]resourcePermission{
	kinds.ServiceAccount:                   {resource: "serviceaccounts", namespaced: true},
	kinds.ConfigMap:                        {resource: "configmaps", namespaced: true},
	kinds.CustomResourceDefinition:         {group: "apiextensions.k8s.io", resource: "customresourcedefinitions"},
	kinds.ClusterRole:                      {group: rbacv1.GroupName, resource: "clusterroles"},
	kinds.ClusterRoleBinding:               {group: rbacv1.GroupName, resource: "clusterrolebindings"},
	kinds.Role:                             {group: rbacv1.GroupName, resource: "roles", namespaced: true},
	kinds.RoleBinding:                      {group: rbacv1.GroupName, resource: "rolebindings", namespaced: true},
	kinds.RouteAdvertisements:              {group: "k8s.ovn.org", resource: "routeadvertisements"},
	kinds.Deployment:                       {group: "apps", resource: "deployments", namespaced: true},
	kinds.DaemonSet:                        {group: "apps", resource: "daemonsets", namespaced: true},
	kinds.ValidatingWebhookConfiguration:   {group: admissionregistrationv1.GroupName, resource: "validatingwebhookconfigurations"},
	kinds.ValidatingAdmissionPolicy:        {group: admissionregistrationv1.GroupName, resource: "validatingadmissionpolicies"},
	kinds.ValidatingAdmissionPolicyBinding: {group: admissionregistrationv1.GroupName, resource: "validatingadmissionpolicybindings"},
}

var _ = Describe("Operator RBAC", func() {
	var (
		operatorRBAC generatedOperatorRBAC
		objects      []*unstructured.Unstructured
	)

	BeforeEach(func() {
		operatorRBAC = loadGeneratedOperatorRBAC()
		objects = renderRealTemplates(decodeCR(maximalCR), renderer.RenderOptions{})
	})

	It("contains no wildcard or escalation permissions", func() {
		for _, rules := range [][]rbacv1.PolicyRule{
			operatorRBAC.clusterRules,
			operatorRBAC.targetRules,
			operatorRBAC.operatorRules,
		} {
			for _, rule := range rules {
				Expect(rule.APIGroups).ToNot(ContainElement("*"))
				Expect(rule.Resources).ToNot(ContainElement("*"))
				Expect(rule.Verbs).ToNot(ContainElements("*", "bind", "escalate"))
			}
		}
	})

	It("leaves namespace creation to the installer", func() {
		Expect(ruleAllows(operatorRBAC.clusterRules, "", "namespaces", "create", nil)).To(BeFalse())
		Expect(ruleAllows(operatorRBAC.clusterRules, "", "namespaces", "delete", nil)).To(BeFalse())
		for _, obj := range objects {
			Expect(obj.GetKind()).ToNot(Equal(kinds.Namespace))
		}
	})

	It("covers every component RBAC rule", func() {
		componentClusterRules, componentRoleRules := renderedRBAC(objects)
		expectRulesCovered(componentClusterRules, operatorRBAC.clusterRules)
		expectRulesCovered(componentRoleRules, slices.Concat(operatorRBAC.clusterRules, operatorRBAC.targetRules))
	})

	It("can reconcile every rendered resource kind", func() {
		for _, obj := range objects {
			permission, ok := deployedResources[obj.GetKind()]
			Expect(ok).To(BeTrue(), "missing RBAC audit mapping for rendered kind %q", obj.GetKind())

			rules := operatorRBAC.clusterRules
			if permission.namespaced {
				rules = slices.Concat(rules, operatorRBAC.targetRules)
			}
			for _, verb := range []string{"get", "list", "create", "patch", "delete"} {
				Expect(ruleAllows(rules, permission.group, permission.resource, verb, nil)).To(
					BeTrue(),
					"missing deployer permission kind=%q group=%q resource=%q verb=%q",
					obj.GetKind(), permission.group, permission.resource, verb,
				)
			}
		}
	})
})
