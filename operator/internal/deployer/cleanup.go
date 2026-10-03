// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package deployer

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/kinds"
)

// CleanupResources deletes all resources created by the OVNK8sOperator instance.
// This uses a simple label-based approach: iterate through resource types from ResourceOrder
// and delete any that match our labels.
func (d *ResourceDeployer) CleanupResources(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator) error {
	logger := log.FromContext(ctx).WithValues("ovnk8sOperator", ovnk8sOperator.Name)
	logger.Info("Starting resource cleanup")

	// Build label selector for all resources created by this OVNK8sOperator
	labelSelector := client.MatchingLabels{
		LabelManagedBy: ManagedBy,
		LabelOwner:     ovnk8sOperator.GetName(),
	}

	deletedCount := 0
	errorCount := 0

	// Process each resource type from ResourceOrder in reverse order
	for _, resourceType := range slices.Backward(ResourceOrder) {
		// Skip list types as they're just for grouping
		if strings.HasSuffix(resourceType, "List") {
			continue
		}

		// Get minimal GVK info for this resource type
		gvk := getGVK(resourceType)
		if gvk == nil {
			// Skip unknown resource types
			logger.V(2).Info("Skipping unknown resource type", "type", resourceType)
			continue
		}

		// Create list with the GVK
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   gvk.Group,
			Version: gvk.Version,
			Kind:    gvk.Kind + "List",
		})

		listOptions := []client.ListOption{labelSelector}
		if isNamespacedResource(resourceType) {
			listOptions = append(listOptions, client.InNamespace(ovnk8sOperator.Spec.Global.Namespace))
		}

		// Try to list resources of this kind with our labels
		err := d.List(ctx, list, listOptions...)
		if err != nil {
			// Skip if resource type doesn't exist or we don't have permissions
			if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || apierrors.IsMethodNotSupported(err) {
				logger.V(2).Info("Skipping resource type", "type", resourceType, "reason", err)
				continue
			}
			logger.V(1).Info("Failed to list resources", "type", resourceType, "error", err)
			continue
		}

		// Delete each found resource
		for _, item := range list.Items {
			logger.V(2).Info("Deleting resource",
				"type", resourceType,
				"name", item.GetName(),
				"namespace", item.GetNamespace())

			if err := d.Delete(ctx, &item, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
				if !apierrors.IsNotFound(err) {
					logger.Error(err, "Failed to delete resource",
						"type", resourceType,
						"name", item.GetName(),
						"namespace", item.GetNamespace())
					errorCount++
					continue
				}
			}
			deletedCount++
		}
	}

	logger.Info("Resource cleanup completed",
		"deletedCount", deletedCount,
		"errorCount", errorCount)

	if errorCount > 0 {
		return fmt.Errorf("cleanup completed with %d errors (deleted %d resources)", errorCount, deletedCount)
	}

	return nil
}

func isNamespacedResource(kind string) bool {
	switch kind {
	case kinds.PriorityClass, kinds.Namespace, kinds.PodSecurityPolicy, kinds.StorageClass,
		kinds.PersistentVolume, kinds.CustomResourceDefinition, kinds.ClusterRole,
		kinds.ClusterRoleBinding, kinds.IngressClass, kinds.APIService,
		kinds.MutatingWebhookConfiguration, kinds.ValidatingWebhookConfiguration,
		kinds.ValidatingAdmissionPolicy, kinds.ValidatingAdmissionPolicyBinding,
		kinds.RouteAdvertisements:
		return false
	default:
		return true
	}
}

// getGVK returns GVK (Group, Version, Kind) info for common Kubernetes resources
func getGVK(kind string) *schema.GroupVersionKind {
	switch kind {
	// Core API (v1) - empty group
	case kinds.Namespace, kinds.ServiceAccount, kinds.Secret, kinds.ConfigMap, kinds.Service,
		kinds.PersistentVolume, kinds.PersistentVolumeClaim, kinds.Pod, kinds.ReplicationController,
		kinds.LimitRange, kinds.ResourceQuota:
		return &schema.GroupVersionKind{Group: "", Version: "v1", Kind: kind}

	// Apps API (apps/v1)
	case kinds.Deployment, kinds.DaemonSet, kinds.StatefulSet, kinds.ReplicaSet:
		return &schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: kind}

	// Batch API (batch/v1)
	case kinds.Job, kinds.CronJob:
		return &schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: kind}

	// RBAC API (rbac.authorization.k8s.io/v1)
	case kinds.ClusterRole, kinds.ClusterRoleBinding, kinds.Role, kinds.RoleBinding:
		return &schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: kind}

	// Networking API (networking.k8s.io/v1)
	case kinds.NetworkPolicy, kinds.Ingress, kinds.IngressClass:
		return &schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: kind}

	// Policy API (policy/v1)
	case kinds.PodDisruptionBudget:
		return &schema.GroupVersionKind{Group: "policy", Version: "v1", Kind: kind}

	// Storage API (storage.k8s.io/v1)
	case kinds.StorageClass:
		return &schema.GroupVersionKind{Group: "storage.k8s.io", Version: "v1", Kind: kind}

	// Scheduling API (scheduling.k8s.io/v1)
	case kinds.PriorityClass:
		return &schema.GroupVersionKind{Group: "scheduling.k8s.io", Version: "v1", Kind: kind}

	// Extensions (apiextensions.k8s.io/v1)
	case kinds.CustomResourceDefinition:
		return &schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: kind}

	// Admission (admissionregistration.k8s.io/v1)
	case kinds.MutatingWebhookConfiguration, kinds.ValidatingWebhookConfiguration,
		kinds.ValidatingAdmissionPolicy, kinds.ValidatingAdmissionPolicyBinding:
		return &schema.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: kind}

	// Autoscaling (autoscaling/v2)
	case kinds.HorizontalPodAutoscaler:
		return &schema.GroupVersionKind{Group: "autoscaling", Version: "v2", Kind: kind}

	// API registration (apiregistration.k8s.io/v1)
	case kinds.APIService:
		return &schema.GroupVersionKind{Group: "apiregistration.k8s.io", Version: "v1", Kind: kind}

	// Multus (k8s.cni.cncf.io/v1)
	case kinds.NetworkAttachmentDefinition:
		return &schema.GroupVersionKind{Group: "k8s.cni.cncf.io", Version: "v1", Kind: kind}

	// OVN-Kubernetes (k8s.ovn.org/v1)
	case kinds.RouteAdvertisements:
		return &schema.GroupVersionKind{Group: "k8s.ovn.org", Version: "v1", Kind: kind}

	default:
		// Unknown resource type
		return nil
	}
}
