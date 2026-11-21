// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// Package kinds names the Kubernetes resource kinds that the operator renders,
// deploys, and validates.
package kinds

const (
	APIService                       = "APIService"
	ClusterRole                      = "ClusterRole"
	ClusterRoleBinding               = "ClusterRoleBinding"
	ConfigMap                        = "ConfigMap"
	CronJob                          = "CronJob"
	CustomResourceDefinition         = "CustomResourceDefinition"
	DaemonSet                        = "DaemonSet"
	Deployment                       = "Deployment"
	HorizontalPodAutoscaler          = "HorizontalPodAutoscaler"
	Ingress                          = "Ingress"
	IngressClass                     = "IngressClass"
	Job                              = "Job"
	LimitRange                       = "LimitRange"
	MutatingWebhookConfiguration     = "MutatingWebhookConfiguration"
	Namespace                        = "Namespace"
	NetworkAttachmentDefinition      = "NetworkAttachmentDefinition"
	NetworkPolicy                    = "NetworkPolicy"
	PersistentVolume                 = "PersistentVolume"
	PersistentVolumeClaim            = "PersistentVolumeClaim"
	Pod                              = "Pod"
	PodDisruptionBudget              = "PodDisruptionBudget"
	PodSecurityPolicy                = "PodSecurityPolicy"
	PriorityClass                    = "PriorityClass"
	ReplicaSet                       = "ReplicaSet"
	ReplicationController            = "ReplicationController"
	ResourceQuota                    = "ResourceQuota"
	Role                             = "Role"
	RoleBinding                      = "RoleBinding"
	RouteAdvertisements              = "RouteAdvertisements"
	Secret                           = "Secret"
	Service                          = "Service"
	ServiceAccount                   = "ServiceAccount"
	StatefulSet                      = "StatefulSet"
	StorageClass                     = "StorageClass"
	ValidatingAdmissionPolicy        = "ValidatingAdmissionPolicy"
	ValidatingAdmissionPolicyBinding = "ValidatingAdmissionPolicyBinding"
	ValidatingWebhookConfiguration   = "ValidatingWebhookConfiguration"
)
