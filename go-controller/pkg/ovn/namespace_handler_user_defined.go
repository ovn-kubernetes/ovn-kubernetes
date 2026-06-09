// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	corev1 "k8s.io/api/core/v1"

	nscontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/namespace"
)

// Compile-time assertions: every concrete UDN controller satisfies the shared
// NamespaceHandler interface via methods on *BaseUserDefinedNetworkController.
var (
	_ nscontroller.NamespaceHandler = (*Layer3UserDefinedNetworkController)(nil)
	_ nscontroller.NamespaceHandler = (*Layer2UserDefinedNetworkController)(nil)
	_ nscontroller.NamespaceHandler = (*LocalnetUserDefinedNetworkController)(nil)
)

// RegisterNamespaceHandler registers the UDN controller as the namespace handler
// for its network. Defined on the shared base so it is promoted to all three UDN
// topologies. Mirrors RegisterNodeHandler.
func (oc *BaseUserDefinedNetworkController) RegisterNamespaceHandler() error {
	return oc.registerNamespaceReconciler(oc)
}

// ReconcileNamespace implements NamespaceHandler for User Defined Networks. Pure
// applier: it trusts the (oldNS, newNS) the controller's gate decided. The
// (oldNS, nil) delete leg programs the delete unconditionally rather than
// re-checking membership, since on an active-to-inactive NAD transition a
// re-check would short-circuit and orphan the previous owner's OVN state.
func (oc *BaseUserDefinedNetworkController) ReconcileNamespace(oldNS, newNS *corev1.Namespace, _, _ *nscontroller.NamespaceAnnotationState) error {
	switch {
	case newNS == nil && oldNS == nil:
		return nil
	case newNS == nil:
		return oc.deleteNamespaceForUserDefinedNetwork(oldNS)
	case oldNS == nil:
		return oc.AddNamespaceForUserDefinedNetwork(newNS)
	default:
		return oc.updateNamespaceForUserDefinedNetwork(oldNS, newNS)
	}
}

// SyncNamespaces implements NamespaceHandler, converting to the legacy
// syncNamespaces []interface{} shape. Called with every namespace in the
// cluster; cleanup is keyed by controllerName, which scopes it to this UDN.
func (oc *BaseUserDefinedNetworkController) SyncNamespaces(namespaces []*corev1.Namespace) error {
	objs := make([]interface{}, 0, len(namespaces))
	for _, ns := range namespaces {
		objs = append(objs, ns)
	}
	return oc.syncNamespaces(objs)
}
