// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package validator

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WorkloadValidator interface for validating different types of workload resources
type WorkloadValidator interface {
	ValidateReadiness(obj client.Object) (*ValidationResult, error)
}

// convertToTyped is a generic helper function that converts a client.Object to a typed object.
// It handles both unstructured and already-typed objects, returning the typed object or an error.
func convertToTyped[T any](obj client.Object, typeName string) (*T, error) {
	// Handle unstructured objects
	if unstructuredObj, ok := obj.(*unstructured.Unstructured); ok {
		result := new(T)
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.Object, result); err != nil {
			return nil, fmt.Errorf("failed to convert unstructured to %s: %w", typeName, err)
		}
		return result, nil
	}

	// Handle already typed objects
	if typed, ok := any(obj).(*T); ok {
		return typed, nil
	}

	return nil, fmt.Errorf("expected %s or Unstructured, got %T", typeName, obj)
}

// DeploymentValidator validates Deployment readiness
type DeploymentValidator struct{}

func (d *DeploymentValidator) ValidateReadiness(obj client.Object) (*ValidationResult, error) {
	deployment, err := convertToTyped[appsv1.Deployment](obj, "Deployment")
	if err != nil {
		return nil, err
	}

	desired := int32(1) // Default to 1 if replicas is nil
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}

	ready := deployment.Status.ReadyReplicas
	available := deployment.Status.AvailableReplicas
	updated := deployment.Status.UpdatedReplicas

	// Check if deployment is fully ready
	if ready == desired && available == desired && updated == desired {
		return &ValidationResult{
			WorkloadName: deployment.Name,
			WorkloadType: "Deployment",
			Ready:        true,
			Message:      fmt.Sprintf("Ready: %d/%d, Available: %d/%d, Updated: %d/%d", ready, desired, available, desired, updated, desired),
		}, nil
	}

	// Check for common failure conditions
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentProgressing && condition.Status == "False" {
			return &ValidationResult{
				WorkloadName: deployment.Name,
				WorkloadType: "Deployment",
				Ready:        false,
				Message:      fmt.Sprintf("Deployment failed to progress: %s", condition.Message),
			}, nil
		}
	}

	return &ValidationResult{
		WorkloadName: deployment.Name,
		WorkloadType: "Deployment",
		Ready:        false,
		Message:      fmt.Sprintf("Ready: %d/%d, Available: %d/%d, Updated: %d/%d", ready, desired, available, desired, updated, desired),
	}, nil
}

// DaemonSetValidator validates DaemonSet readiness
type DaemonSetValidator struct{}

func (d *DaemonSetValidator) ValidateReadiness(obj client.Object) (*ValidationResult, error) {
	daemonSet, err := convertToTyped[appsv1.DaemonSet](obj, "DaemonSet")
	if err != nil {
		return nil, err
	}

	desired := daemonSet.Status.DesiredNumberScheduled
	ready := daemonSet.Status.NumberReady
	available := daemonSet.Status.NumberAvailable
	updated := daemonSet.Status.UpdatedNumberScheduled

	// DaemonSet is ready when all desired instances are ready and available
	if desired > 0 && ready == desired && available == desired && updated == desired {
		return &ValidationResult{
			WorkloadName: daemonSet.Name,
			WorkloadType: "DaemonSet",
			Ready:        true,
			Message:      fmt.Sprintf("Ready: %d/%d, Available: %d/%d, Updated: %d/%d", ready, desired, available, desired, updated, desired),
		}, nil
	}

	// Check if there are any nodes that can't schedule the DaemonSet
	if daemonSet.Status.NumberMisscheduled > 0 {
		return &ValidationResult{
			WorkloadName: daemonSet.Name,
			WorkloadType: "DaemonSet",
			Ready:        false,
			Message:      fmt.Sprintf("Ready: %d/%d, Misscheduled: %d", ready, desired, daemonSet.Status.NumberMisscheduled),
		}, nil
	}

	return &ValidationResult{
		WorkloadName: daemonSet.Name,
		WorkloadType: "DaemonSet",
		Ready:        false,
		Message:      fmt.Sprintf("Ready: %d/%d, Available: %d/%d, Updated: %d/%d", ready, desired, available, desired, updated, desired),
	}, nil
}

// StatefulSetValidator validates StatefulSet readiness
type StatefulSetValidator struct{}

func (s *StatefulSetValidator) ValidateReadiness(obj client.Object) (*ValidationResult, error) {
	statefulSet, err := convertToTyped[appsv1.StatefulSet](obj, "StatefulSet")
	if err != nil {
		return nil, err
	}

	desired := int32(1) // Default to 1 if replicas is nil
	if statefulSet.Spec.Replicas != nil {
		desired = *statefulSet.Spec.Replicas
	}

	ready := statefulSet.Status.ReadyReplicas
	current := statefulSet.Status.CurrentReplicas
	updated := statefulSet.Status.UpdatedReplicas

	// StatefulSet is ready when all replicas are ready and current
	if ready == desired && current == desired && updated == desired {
		return &ValidationResult{
			WorkloadName: statefulSet.Name,
			WorkloadType: "StatefulSet",
			Ready:        true,
			Message:      fmt.Sprintf("Ready: %d/%d, Current: %d/%d, Updated: %d/%d", ready, desired, current, desired, updated, desired),
		}, nil
	}

	return &ValidationResult{
		WorkloadName: statefulSet.Name,
		WorkloadType: "StatefulSet",
		Ready:        false,
		Message:      fmt.Sprintf("Ready: %d/%d, Current: %d/%d, Updated: %d/%d", ready, desired, current, desired, updated, desired),
	}, nil
}

// GenericWorkloadValidator provides basic validation for unknown workload types
type GenericWorkloadValidator struct{}

func (g *GenericWorkloadValidator) ValidateReadiness(obj client.Object) (*ValidationResult, error) {
	// For unknown workload types, we assume they're ready if they exist
	// This is a fallback and should be enhanced based on specific needs
	return &ValidationResult{
		WorkloadName: obj.GetName(),
		WorkloadType: obj.GetObjectKind().GroupVersionKind().Kind,
		Ready:        true,
		Message:      "Generic validation - resource exists",
	}, nil
}
