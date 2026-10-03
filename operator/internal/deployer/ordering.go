// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package deployer

import (
	"slices"

	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/kinds"
)

// ResourceOrder defines the deployment order for Kubernetes resources
// Resources are deployed in this order to ensure dependencies are satisfied
var ResourceOrder = []string{
	kinds.PriorityClass,
	kinds.Namespace,
	kinds.NetworkPolicy,
	kinds.ResourceQuota,
	kinds.LimitRange,
	kinds.PodSecurityPolicy,
	kinds.PodDisruptionBudget,
	kinds.ServiceAccount,
	kinds.Secret,
	"SecretList",
	kinds.ConfigMap,
	kinds.StorageClass,
	kinds.PersistentVolume,
	kinds.PersistentVolumeClaim,
	kinds.CustomResourceDefinition,
	kinds.ClusterRole,
	"ClusterRoleList",
	kinds.ClusterRoleBinding,
	"ClusterRoleBindingList",
	kinds.Role,
	"RoleList",
	kinds.RoleBinding,
	"RoleBindingList",
	kinds.Service,
	kinds.NetworkAttachmentDefinition,
	kinds.RouteAdvertisements,
	kinds.DaemonSet,
	kinds.Pod,
	kinds.ReplicationController,
	kinds.ReplicaSet,
	kinds.Deployment,
	kinds.HorizontalPodAutoscaler,
	kinds.StatefulSet,
	kinds.Job,
	kinds.CronJob,
	kinds.IngressClass,
	kinds.Ingress,
	kinds.APIService,
	kinds.MutatingWebhookConfiguration,
	kinds.ValidatingWebhookConfiguration,
	kinds.ValidatingAdmissionPolicy,
	kinds.ValidatingAdmissionPolicyBinding,
}

// IsKnownResourceType reports whether kind appears in ResourceOrder. Kinds
// absent from ResourceOrder are never visited by the ordered deploy loop, so
// the deployer uses this to fail fast rather than silently drop a rendered
// manifest.
func IsKnownResourceType(kind string) bool {
	return slices.Contains(ResourceOrder, kind)
}
