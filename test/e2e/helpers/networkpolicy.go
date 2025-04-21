package helpers

import (
	"context"
	"fmt"
	"os"

	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
)

func MakeDenyAllPolicy(f *framework.Framework, ns string, policyName string) (*knet.NetworkPolicy, error) {
	policy := &knet.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: policyName,
		},
		Spec: knet.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []knet.PolicyType{knet.PolicyTypeEgress, knet.PolicyTypeIngress},
			Ingress:     []knet.NetworkPolicyIngressRule{},
			Egress:      []knet.NetworkPolicyEgressRule{},
		},
	}
	return f.ClientSet.NetworkingV1().NetworkPolicies(ns).Create(context.TODO(), policy, metav1.CreateOptions{})
}

func MakeAdminNetworkPolicy(anpName, priority, anpSubjectNS string) error {
	anpYaml := "anp.yaml"
	var anpConfig = fmt.Sprintf(`apiVersion: policy.networking.k8s.io/v1alpha1
kind: AdminNetworkPolicy
metadata:
  name: %s
spec:
  priority: %s
  subject:
    namespaces:
      matchLabels:
        kubernetes.io/metadata.name: %s
  egress:
  - name: "allow-to-restricted"
    action: "Allow"
    to:
    - namespaces:
        matchLabels:
          kubernetes.io/metadata.name: anp-peer-restricted
  - name: "deny-to-open"
    action: "Deny"
    to:
    - namespaces:
        matchLabels:
          kubernetes.io/metadata.name: anp-peer-open
  - name: "pass-to-unknown"
    action: "Pass"
    to:
    - namespaces:
        matchLabels:
          kubernetes.io/metadata.name: anp-peer-unknown
`, anpName, priority, anpSubjectNS)

	if err := os.WriteFile(anpYaml, []byte(anpConfig), 0644); err != nil {
		framework.Failf("Unable to write CRD config to disk: %v", err)
	}

	defer func() {
		if err := os.Remove(anpYaml); err != nil {
			framework.Logf("Unable to remove the CRD config from disk: %v", err)
		}
	}()

	_, err := e2ekubectl.RunKubectl("default", "create", "-f", anpYaml)
	return err
}

func MakeBaselineAdminNetworkPolicy(banpSubjectNS string) error {
	banpYaml := "banp.yaml"
	var banpConfig = fmt.Sprintf(`apiVersion: policy.networking.k8s.io/v1alpha1
kind: BaselineAdminNetworkPolicy
metadata:
  name: default
spec:
  subject:
    namespaces:
      matchLabels:
        kubernetes.io/metadata.name: %s
  egress:
  - name: "allow-to-restricted"
    action: "Allow"
    to:
    - namespaces:
        matchLabels:
          kubernetes.io/metadata.name: anp-peer-restricted
  - name: "deny-to-unknown"
    action: "Deny"
    to:
    - namespaces:
        matchLabels:
          kubernetes.io/metadata.name: anp-peer-unknown
`, banpSubjectNS)

	if err := os.WriteFile(banpYaml, []byte(banpConfig), 0644); err != nil {
		framework.Failf("Unable to write CRD config to disk: %v", err)
	}

	defer func() {
		if err := os.Remove(banpYaml); err != nil {
			framework.Logf("Unable to remove the CRD config from disk: %v", err)
		}
	}()

	_, err := e2ekubectl.RunKubectl("default", "create", "-f", banpYaml)
	return err
}
