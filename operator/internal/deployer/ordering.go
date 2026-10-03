// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package deployer

// ResourceOrder defines the deployment order for Kubernetes resources
// Resources are deployed in this order to ensure dependencies are satisfied
var ResourceOrder = []string{
	"PriorityClass",
	"Namespace",
	"NetworkPolicy",
	"ResourceQuota",
	"LimitRange",
	"PodSecurityPolicy",
	"PodDisruptionBudget",
	"ServiceAccount",
	"Secret",
	"SecretList",
	"ConfigMap",
	"StorageClass",
	"PersistentVolume",
	"PersistentVolumeClaim",
	"CustomResourceDefinition",
	"ClusterRole",
	"ClusterRoleList",
	"ClusterRoleBinding",
	"ClusterRoleBindingList",
	"Role",
	"RoleList",
	"RoleBinding",
	"RoleBindingList",
	"Service",
	"NetworkAttachmentDefinition",
	"RouteAdvertisements",
	"DaemonSet",
	"Pod",
	"ReplicationController",
	"ReplicaSet",
	"Deployment",
	"HorizontalPodAutoscaler",
	"StatefulSet",
	"Job",
	"CronJob",
	"IngressClass",
	"Ingress",
	"APIService",
	"MutatingWebhookConfiguration",
	"ValidatingWebhookConfiguration",
	"ValidatingAdmissionPolicy",
	"ValidatingAdmissionPolicyBinding",
}

// IsKnownResourceType reports whether kind appears in ResourceOrder. Kinds
// absent from ResourceOrder are never visited by the ordered deploy loop, so
// the deployer uses this to fail fast rather than silently drop a rendered
// manifest.
func IsKnownResourceType(kind string) bool {
	for _, k := range ResourceOrder {
		if k == kind {
			return true
		}
	}
	return false
}
