// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package renderer

import (
	"encoding/json"
	"fmt"

	jsonpatch "github.com/evanphx/json-patch/v5"

	"sigs.k8s.io/yaml"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/kinds"
)

// workloadKinds are the resource kinds that patches can target.
// Patches only apply to main workload resources, not sibling Services/ConfigMaps.
var workloadKinds = map[string]bool{
	kinds.Deployment:  true,
	kinds.DaemonSet:   true,
	kinds.StatefulSet: true,
}

// ApplyPatches applies user-defined patches to rendered manifests.
// Patches are defined per-component in ComponentOverride.Patches.
// Only workload resources (Deployment, DaemonSet, StatefulSet) are patched —
// sibling objects like Services and ConfigMaps are left untouched.
func ApplyPatches(manifests ComponentManifests, overrides []k8sv1alpha1.ComponentOverride) error {
	for _, override := range overrides {
		if len(override.Patches) == 0 {
			continue
		}
		componentManifests, exists := manifests[override.ResourceName]
		if !exists {
			continue
		}
		for i, manifest := range componentManifests {
			kind := extractKind(manifest.Content)
			if !workloadKinds[kind] {
				continue // skip non-workload resources
			}
			patched, err := applyPatchesToContent(manifest.Content, override.Patches)
			if err != nil {
				return fmt.Errorf("failed to apply patches to %s %s/%s: %w",
					kind, override.ResourceName, manifest.FileName, err)
			}
			componentManifests[i].Content = patched
		}
	}
	return nil
}

// extractKind extracts the "kind" field from a YAML manifest.
func extractKind(yamlContent string) string {
	var obj struct {
		Kind string `json:"kind"`
	}
	if err := yaml.Unmarshal([]byte(yamlContent), &obj); err != nil {
		return ""
	}
	return obj.Kind
}

// applyPatchesToContent applies a list of patches to YAML content.
func applyPatchesToContent(yamlContent string, patches []k8sv1alpha1.ComponentPatch) (string, error) {
	// Convert YAML to JSON for patching
	originalJSON, err := yaml.YAMLToJSON([]byte(yamlContent))
	if err != nil {
		return "", fmt.Errorf("failed to convert YAML to JSON: %w", err)
	}

	current := originalJSON
	for idx, patch := range patches {
		var patched []byte
		var patchErr error

		switch patch.Type {
		case k8sv1alpha1.JSONPatchType, "":
			// Default is JSON patch (precise, array-index-aware)
			patched, patchErr = applyJSONPatch(current, patch.Patch)
		case k8sv1alpha1.MergePatchType:
			patched, patchErr = applyMergePatch(current, patch.Patch)
		default:
			patchErr = fmt.Errorf("unknown patch type: %q", patch.Type)
		}

		if patchErr != nil {
			return "", fmt.Errorf("patch[%d] (type=%s) failed: %w", idx, patch.Type, patchErr)
		}
		current = patched
	}

	// Convert back to YAML
	result, err := yaml.JSONToYAML(current)
	if err != nil {
		return "", fmt.Errorf("failed to convert patched JSON back to YAML: %w", err)
	}

	return string(result), nil
}

// applyJSONPatch applies an RFC 6902 JSON Patch to a JSON document.
func applyJSONPatch(doc []byte, patchContent string) ([]byte, error) {
	patch, err := jsonpatch.DecodePatch([]byte(patchContent))
	if err != nil {
		return nil, fmt.Errorf("invalid JSON patch: %w", err)
	}
	return patch.Apply(doc)
}

// applyMergePatch applies an RFC 7386 JSON Merge Patch to a JSON document.
func applyMergePatch(doc []byte, patchContent string) ([]byte, error) {
	// Convert patch from YAML to JSON (users may write patches in YAML)
	patchJSON, err := yaml.YAMLToJSON([]byte(patchContent))
	if err != nil {
		return nil, fmt.Errorf("invalid merge patch: %w", err)
	}

	// Use json-patch library's merge patch implementation
	result, err := jsonpatch.MergePatch(doc, patchJSON)
	if err != nil {
		return nil, fmt.Errorf("merge patch failed: %w", err)
	}

	// Re-indent for clean output
	var pretty json.RawMessage
	if err := json.Unmarshal(result, &pretty); err != nil {
		return result, nil // return unformatted if indent fails
	}
	formatted, err := json.Marshal(pretty)
	if err != nil {
		return result, nil
	}
	return formatted, nil
}
