// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package deployer

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/renderer"
)

// ClassifiedManifests groups manifests by their Kubernetes resource type
type ClassifiedManifests map[string][]*renderer.RenderedManifest

// classifyManifests parses manifests and groups them by resource type (Kind)
func (d *ResourceDeployer) classifyManifests(ctx context.Context, manifests []*renderer.RenderedManifest) (ClassifiedManifests, error) {
	logger := log.FromContext(ctx)
	classified := make(ClassifiedManifests)

	logger.V(2).Info("Classifying manifests", "manifestCount", len(manifests))

	for _, manifest := range manifests {
		resourceType, err := d.extractResourceType(manifest)
		if err != nil {
			logger.Error(err, "Failed to extract resource type",
				"manifest", manifest.FileName,
				"templatePath", manifest.TemplatePath)
			return nil, fmt.Errorf("failed to extract resource type from %s: %w", manifest.FileName, err)
		}

		// A kind missing from ResourceOrder is never visited by the ordered
		// deploy loop, so the rendered manifest would be silently dropped.
		// Fail fast instead so the gap is caught at deploy time.
		if !IsKnownResourceType(resourceType) {
			return nil, fmt.Errorf("manifest %s has resource type %q which is missing from ResourceOrder; add it to deployer/ordering.go", manifest.FileName, resourceType)
		}

		classified[resourceType] = append(classified[resourceType], manifest)
		logger.V(3).Info("Classified manifest",
			"manifest", manifest.FileName,
			"resourceType", resourceType)
	}

	// Log summary of classification
	if logger.V(2).Enabled() {
		for resourceType, resources := range classified {
			logger.V(2).Info("Resource type summary",
				"resourceType", resourceType,
				"count", len(resources))
		}
	}

	return classified, nil
}

// extractResourceType parses a YAML manifest and extracts the Kubernetes resource type (Kind)
func (d *ResourceDeployer) extractResourceType(manifest *renderer.RenderedManifest) (string, error) {
	// Skip empty manifests
	content := strings.TrimSpace(manifest.Content)
	if content == "" {
		return "", fmt.Errorf("empty manifest content")
	}

	// Parse YAML to extract the Kind field
	obj := &unstructured.Unstructured{}
	decoder := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

	if _, _, err := decoder.Decode([]byte(content), nil, obj); err != nil {
		return "", fmt.Errorf("failed to decode YAML: %w", err)
	}

	kind := obj.GetKind()
	if kind == "" {
		return "", fmt.Errorf("missing 'kind' field in manifest")
	}

	return kind, nil
}
