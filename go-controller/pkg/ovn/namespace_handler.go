// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	corev1 "k8s.io/api/core/v1"

	nscontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/namespace"
)

// Compile-time assertion: DefaultNetworkController satisfies NamespaceHandler.
var _ nscontroller.NamespaceHandler = (*DefaultNetworkController)(nil)

// ReconcileNamespace implements NamespaceHandler, mapping the level-driven shape
// onto the legacy add/update/delete methods, which read annotations off the
// object. The parsed NamespaceAnnotationState is intentionally ignored for now.
func (oc *DefaultNetworkController) ReconcileNamespace(oldNS, newNS *corev1.Namespace, _, _ *nscontroller.NamespaceAnnotationState) error {
	switch {
	case newNS == nil:
		// Delete pass. deleteNamespace only reads the name and cached nsInfo,
		// so a name-only stub oldNS is fine when no real object is available.
		if oldNS == nil {
			return nil
		}
		return oc.deleteNamespace(oldNS)
	case oldNS == nil:
		return oc.AddNamespace(newNS)
	default:
		return oc.updateNamespace(oldNS, newNS)
	}
}

// ClaimsNamespace implements NamespaceHandler. The default network claims every
// non-empty namespace; empty-name namespaces are excluded.
func (oc *DefaultNetworkController) ClaimsNamespace(nsName string) (bool, error) {
	return nsName != "", nil
}

// SyncNamespaces implements NamespaceHandler, converting to the legacy
// syncNamespaces []interface{} shape.
func (oc *DefaultNetworkController) SyncNamespaces(namespaces []*corev1.Namespace) error {
	objs := make([]interface{}, 0, len(namespaces))
	for _, ns := range namespaces {
		objs = append(objs, ns)
	}
	return oc.syncNamespaces(objs)
}
