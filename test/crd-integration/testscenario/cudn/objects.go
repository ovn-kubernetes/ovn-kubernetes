// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
)

// newCUDN wraps a network configuration in a cluster network selecting namespace red.
func newCUDN(name string, network udnv1.NetworkSpec) *udnv1.ClusterUserDefinedNetwork {
	return &udnv1.ClusterUserDefinedNetwork{
		TypeMeta:   metav1.TypeMeta{APIVersion: udnv1.SchemeGroupVersion.String(), Kind: "ClusterUserDefinedNetwork"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: udnv1.ClusterUserDefinedNetworkSpec{
			NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "red"}},
			Network:           network,
		},
	}
}

// newCUDNForNamespaces selects the given namespaces by name.
func newCUDNForNamespaces(name string, network udnv1.NetworkSpec, namespaces ...string) *udnv1.ClusterUserDefinedNetwork {
	obj := newCUDN(name, network)
	if len(namespaces) == 1 {
		obj.Spec.NamespaceSelector = metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespaces[0]}}
	} else {
		obj.Spec.NamespaceSelector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key: "kubernetes.io/metadata.name", Operator: metav1.LabelSelectorOpIn, Values: namespaces,
		}}}
	}
	return obj
}

// newUDN wraps a network configuration in a network in the default namespace.
func newUDN(name string, spec udnv1.UserDefinedNetworkSpec) *udnv1.UserDefinedNetwork {
	return &udnv1.UserDefinedNetwork{
		TypeMeta:   metav1.TypeMeta{APIVersion: udnv1.SchemeGroupVersion.String(), Kind: "UserDefinedNetwork"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spec,
	}
}
