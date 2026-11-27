// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clusternetworkconnect

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cncv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	crdtypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
)

// newClusterNetworkConnect wraps a connectivity configuration with its resource metadata.
func newClusterNetworkConnect(name string, spec cncv1.ClusterNetworkConnectSpec) *cncv1.ClusterNetworkConnect {
	return &cncv1.ClusterNetworkConnect{
		TypeMeta:   metav1.TypeMeta{APIVersion: cncv1.SchemeGroupVersion.String(), Kind: "ClusterNetworkConnect"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
}

// cudnSelector selects cluster networks with the given name label.
func cudnSelector(name string) crdtypes.NetworkSelector {
	return crdtypes.NetworkSelector{
		NetworkSelectionType: crdtypes.ClusterUserDefinedNetworks,
		ClusterUserDefinedNetworkSelector: &crdtypes.ClusterUserDefinedNetworkSelector{
			NetworkSelector: metav1.LabelSelector{MatchLabels: map[string]string{"name": name}},
		},
	}
}

// primaryUDNSelector selects primary networks in namespaces with the given name label.
func primaryUDNSelector(name string) crdtypes.NetworkSelector {
	return crdtypes.NetworkSelector{
		NetworkSelectionType: crdtypes.PrimaryUserDefinedNetworks,
		PrimaryUserDefinedNetworkSelector: &crdtypes.PrimaryUserDefinedNetworkSelector{
			NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"name": name}},
		},
	}
}
