// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package deployer

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/kinds"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/renderer"
)

// ResourceDeployer handles deployment of Kubernetes resources in the correct order
type ResourceDeployer struct {
	client.Client
	Scheme *runtime.Scheme
}

// Labels that identify the resources an OVNK8sOperator instance manages.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelOwner     = "ovnk8s-operator.ovn.org/ovnk8soperator"

	// ManagedBy is the LabelManagedBy value on operator-managed resources.
	ManagedBy = "ovnk8s-operator"
)

// WorkloadResource represents a main workload resource that needs validation
type WorkloadResource struct {
	Object client.Object
	Type   string
	Name   string
}

// NewResourceDeployer creates a new ResourceDeployer instance
func NewResourceDeployer(k8sClient client.Client, scheme *runtime.Scheme) *ResourceDeployer {
	return &ResourceDeployer{
		Client: k8sClient,
		Scheme: scheme,
	}
}

// DeployComponentManifests deploys all manifests for a component in the correct order
// Returns workload resources that need validation
func (d *ResourceDeployer) DeployComponentManifests(ctx context.Context, componentName string, manifests []*renderer.RenderedManifest, owner *k8sv1alpha1.OVNK8sOperator) ([]WorkloadResource, error) {
	logger := log.FromContext(ctx).WithValues("component", componentName)

	logger.Info("Starting component deployment", "manifestCount", len(manifests))

	// 1. Parse and classify manifests by resource type
	classified, err := d.classifyManifests(ctx, manifests)
	if err != nil {
		return nil, fmt.Errorf("failed to classify manifests: %w", err)
	}

	var workloadResources []WorkloadResource
	totalDeployed := 0

	// 2. Deploy resources in predefined order
	for _, resourceType := range ResourceOrder {
		if resources, exists := classified[resourceType]; exists {
			logger.V(1).Info("Deploying resource type",
				"resourceType", resourceType,
				"resourceCount", len(resources))

			for _, manifest := range resources {
				obj, err := d.deployManifest(ctx, manifest, owner, componentName)
				if err != nil {
					return nil, fmt.Errorf("failed to deploy %s: %w", resourceType, err)
				}
				totalDeployed++

				// Track workload resources for validation
				if isWorkloadResource(resourceType) {
					workloadResources = append(workloadResources, WorkloadResource{
						Object: obj,
						Type:   resourceType,
						Name:   obj.GetName(),
					})
				}
			}
		}
	}

	logger.Info("Component deployment completed",
		"totalDeployed", totalDeployed,
		"workloadCount", len(workloadResources))

	return workloadResources, nil
}

// deployManifest deploys a single manifest to the cluster
func (d *ResourceDeployer) deployManifest(ctx context.Context, manifest *renderer.RenderedManifest, owner *k8sv1alpha1.OVNK8sOperator, componentName string) (client.Object, error) {
	logger := log.FromContext(ctx).WithValues("manifest", manifest.FileName)

	// Parse YAML content into unstructured object
	obj := &unstructured.Unstructured{}
	decoder := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

	if _, _, err := decoder.Decode([]byte(manifest.Content), nil, obj); err != nil {
		return nil, fmt.Errorf("failed to decode manifest %s: %w", manifest.FileName, err)
	}

	// Add labels for identification and cleanup
	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[LabelManagedBy] = ManagedBy
	labels[LabelOwner] = owner.GetName()
	labels["ovnk8s-operator.ovn.org/component"] = componentName
	obj.SetLabels(labels)

	logger.V(2).Info("Applying labels to resource",
		"kind", obj.GetKind(),
		"name", obj.GetName(),
		"namespace", obj.GetNamespace())

	// Create or update the resource
	if err := d.createOrUpdate(ctx, obj); err != nil {
		return nil, fmt.Errorf("failed to create/update resource %s/%s: %w",
			obj.GetKind(), obj.GetName(), err)
	}

	return obj, nil
}

// createOrUpdate creates the resource if it doesn't exist, patches if it does
func (d *ResourceDeployer) createOrUpdate(ctx context.Context, obj client.Object) error {
	logger := log.FromContext(ctx).WithValues(
		"kind", obj.GetObjectKind().GroupVersionKind().Kind,
		"name", obj.GetName(),
		"namespace", obj.GetNamespace())

	// Try to get existing resource
	existing := obj.DeepCopyObject().(client.Object)
	err := d.Get(ctx, client.ObjectKeyFromObject(obj), existing)

	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to get existing resource: %w", err)
		}

		// Resource doesn't exist, create it
		logger.Info("Creating resource")
		return d.Create(ctx, obj)
	}

	// Resource exists, patch it using strategic merge
	logger.Info("Patching existing resource")

	// Copy the resourceVersion from existing to new object before patching
	obj.SetResourceVersion(existing.GetResourceVersion())

	// Create a strategic merge patch
	patch := client.MergeFrom(existing)
	return d.Patch(ctx, obj, patch)
}

// isWorkloadResource checks if the resource type is a main workload that needs validation
func isWorkloadResource(resourceType string) bool {
	switch resourceType {
	case kinds.Deployment, kinds.DaemonSet, kinds.StatefulSet:
		return true
	default:
		return false
	}
}
