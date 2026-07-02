// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package tracing

import "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"

// Supported values for the propagated-context-mode setting. The canonical
// declarations live in pkg/config because this package imports it, so config
// cannot import back; they are re-exported here so that tracing callers do not
// need to import config just to name a mode.
const (
	SpanRelationshipModeLinked = config.SpanRelationshipModeLinked
	SpanRelationshipModeParent = config.SpanRelationshipModeParent
)

const (
	// NodeNetworkControllerPodSpanPrefix prefixes pod spans emitted by ovnkube-node
	// network-controller paths.
	NodeNetworkControllerPodSpanPrefix = "ovnkube-node.network-controller.pod"

	// ClusterManagerNetworkControllerPodSpanPrefix prefixes pod spans emitted by
	// ovnkube-cluster-manager network-controller paths.
	ClusterManagerNetworkControllerPodSpanPrefix = "ovnkube-cluster-manager.network-controller.pod"

	// NodeCNIPodSpanPrefix prefixes pod spans emitted by the ovnkube-node CNI server.
	NodeCNIPodSpanPrefix = "ovnkube-node.cni.pod"

	ResourceAttrServiceName      = "service.name"
	ResourceAttrServiceComponent = "service.component"

	SpanNameConfigureInterface         = "configure-interface"
	SpanNameUnconfigureInterface       = "unconfigure-interface"
	SpanNameSetupLocalPodNetwork       = "setup-local-pod-network"
	SpanNameTeardownLocalPodNetwork    = "teardown-local-pod-network"
	SpanNameUpdatePodNetworkAnnotation = "update-pod-network-annotation"
	SpanNameAllocatePodNetwork         = "allocate-pod-network"
	SpanNameReleasePodNetwork          = "release-pod-network"

	SpanAttrCNICommand                 = "cni.command"
	SpanAttrCNIDeviceID                = "cni.device.id"
	SpanAttrCNIIfName                  = "cni.ifname"
	SpanAttrCNISandboxID               = "cni.sandbox.id"
	SpanAttrK8sNADKey                  = "k8s.nad.key"
	SpanAttrK8sNADName                 = "k8s.nad.name"
	SpanAttrK8sNADNamespace            = "k8s.nad.namespace"
	SpanAttrK8sNodeName                = "k8s.node.name"
	SpanAttrK8sPodDeleted              = "k8s.pod.deleted"
	SpanAttrK8sPodName                 = "k8s.pod.name"
	SpanAttrK8sPodNamespace            = "k8s.pod.namespace"
	SpanAttrK8sPodUID                  = "k8s.pod.uid"
	SpanAttrOvnK8sNetworkName          = "ovnk8s.network.name"
	SpanAttrOvnK8sNetworkPrimary       = "ovnk8s.network.primary"
	SpanAttrOvnK8sNetworkRole          = "ovnk8s.network.role"
	SpanAttrOvnK8sNetworkTopology      = "ovnk8s.network.topology"
	SpanAttrPodAnnotationPatchAttempts = "pod.annotation.patch.attempts"
	SpanAttrPodAnnotationUpdated       = "pod.annotation.updated"
	SpanAttrRetryLoop                  = "retry.loop"
)
