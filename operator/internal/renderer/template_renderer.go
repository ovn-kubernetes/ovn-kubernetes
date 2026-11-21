// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package renderer

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"

	sigyaml "sigs.k8s.io/yaml"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/defaults"
)

// RenderOptions carries per-reconcile values that are not derived from the CR.
type RenderOptions struct {
	// WebhookCABundle is the PEM CA cert injected into the ovnkube-identity
	// webhook configs' caBundle. Empty when identity is not enabled.
	WebhookCABundle string
}

// TemplateData represents the data structure passed to templates
type TemplateData map[string]any

// RenderedManifest represents a single rendered template with metadata
type RenderedManifest struct {
	Content      string // The rendered YAML content
	TemplatePath string // Original template file path
	FileName     string // Base name of the template file
}

// ComponentManifests maps component names to their rendered manifests
type ComponentManifests map[string][]*RenderedManifest

// TemplateRenderer handles rendering of Kubernetes manifests from templates
type TemplateRenderer struct {
	templatesDir   string
	embeddedConfig *defaults.EmbeddedConfig
}

// NewTemplateRenderer creates a new TemplateRenderer instance
func NewTemplateRenderer(templatesDir string, embeddedConfig *defaults.EmbeddedConfig) *TemplateRenderer {
	return &TemplateRenderer{
		templatesDir:   templatesDir,
		embeddedConfig: embeddedConfig,
	}
}

// RenderAllTemplates renders all templates for all components.
// Component list comes from the embedded config, not the CR.
func (r *TemplateRenderer) RenderAllTemplates(cr *k8sv1alpha1.OVNK8sOperator, opts RenderOptions) (ComponentManifests, error) {
	templateData, err := r.buildTemplateData(cr, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to build template data: %w", err)
	}

	componentManifests := make(ComponentManifests)

	// Always render global templates first
	globalManifests, err := r.renderGlobalTemplates(templateData)
	if err != nil {
		return nil, fmt.Errorf("failed to render global templates: %w", err)
	}
	if len(globalManifests) > 0 {
		componentManifests["global"] = globalManifests
	}

	// Resolve component set from the CR's deploymentMode + componentOverrides
	resolved, err := r.embeddedConfig.ResolveComponents(cr)
	if err != nil {
		return nil, fmt.Errorf("resolve components for mode %q: %w", cr.Spec.Global.DeploymentMode, err)
	}

	// Process each resolved component
	for _, comp := range resolved {
		componentTemplateDir := filepath.Join(r.templatesDir, comp.ResourceName)

		info, err := os.Stat(componentTemplateDir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("resolved component %q has no template directory: %s", comp.ResourceName, componentTemplateDir)
			}
			return nil, fmt.Errorf("failed to access template directory %s: %w", componentTemplateDir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("resolved component %q template path is not a directory: %s", comp.ResourceName, componentTemplateDir)
		}

		manifests, err := r.renderComponentTemplates(componentTemplateDir, templateData)
		if err != nil {
			return nil, fmt.Errorf("failed to render templates for component %s: %w", comp.ResourceName, err)
		}

		if len(manifests) > 0 {
			componentManifests[comp.ResourceName] = manifests
		}
	}

	return componentManifests, nil
}

// buildTemplateData creates the template data structure from the CR + embedded config.
//
// Produces:
//
//	data["global"]                                    — GlobalConfig fields (typed, direct access)
//	data["global"]["featureGates"]                    — []string of enabled gate JSON tag names
//	data["global"]["featureGateConfig"]               — map of gate config values for gates with config
//	data["ovnkubeControlPlane"]                       — component data with resolved images + customVariables
//	data["ovnkubeNode"]                               — same
//	data["ovnkubeIdentity"]["webhookCABundle"]         — set from opts.WebhookCABundle when the identity component is resolved
func (r *TemplateRenderer) buildTemplateData(cr *k8sv1alpha1.OVNK8sOperator, opts RenderOptions) (TemplateData, error) {
	data := make(TemplateData)

	// 1. Global config — typed fields accessed directly as .global.v4JoinSubnet etc.
	globalMap, err := structToMap(cr.Spec.Global)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal global config: %w", err)
	}

	// Pre-compute comma-separated network CIDRs for templates
	globalMap["podNetworkCIDR"] = buildPodNetworkCIDR(cr.Spec.Global.PodNetwork)
	globalMap["serviceNetworkCIDR"] = strings.Join(cr.Spec.Global.ServiceNetwork, ",")

	// Always provide the nested map so optional metrics fields are safe to read
	// from templates when spec.global.metrics is omitted.
	if cr.Spec.Global.Metrics == nil {
		globalMap["metrics"] = map[string]any{}
	}

	// 2. Feature gates
	featureGates, featureGateConfig, err := buildFeatureGateData(cr.Spec.FeatureGates)
	if err != nil {
		return nil, fmt.Errorf("failed to build feature gate data: %w", err)
	}
	globalMap["featureGates"] = featureGates
	globalMap["featureGateConfig"] = featureGateConfig

	// 3. Component data — images resolved from embedded config + CR overrides.
	// Iterates the resolved component set so disabled/out-of-mode components
	// don't leak into template data.
	resolved, err := r.embeddedConfig.ResolveComponents(cr)
	if err != nil {
		return nil, fmt.Errorf("resolve components for mode %q: %w", cr.Spec.Global.DeploymentMode, err)
	}
	resolvedNames := make([]string, 0, len(resolved))
	for _, comp := range resolved {
		compData, err := r.buildComponentData(&comp, cr.Spec.ComponentOverrides)
		if err != nil {
			return nil, fmt.Errorf("failed to build data for component %s: %w", comp.ResourceName, err)
		}
		data[comp.ResourceName] = compData
		resolvedNames = append(resolvedNames, comp.ResourceName)
	}
	// Surface the resolved component list so global/cross-component templates
	// can gate sections on whether a sibling is in scope (e.g. ovs-node RBAC).
	globalMap["components"] = resolvedNames

	data["global"] = globalMap

	// Inject the webhook CA bundle into the identity component data, when present.
	if opts.WebhookCABundle != "" {
		if comp, ok := data["ovnkubeIdentity"].(map[string]any); ok {
			comp["webhookCABundle"] = opts.WebhookCABundle
		}
	}

	return data, nil
}

// buildPodNetworkCIDR joins pod network entries into a comma-separated string
// of "cidr/hostPrefix" pairs (e.g., "10.192.0.0/16/26,fd01::/48/64").
func buildPodNetworkCIDR(entries []k8sv1alpha1.PodNetworkEntry) string {
	parts := make([]string, len(entries))
	for i, e := range entries {
		parts[i] = fmt.Sprintf("%s/%d", e.CIDR, e.HostPrefix)
	}
	return strings.Join(parts, ",")
}

const (
	gateGroupRouting       = "routing"
	gateGroupEgress        = "egress"
	gateGroupNetworkPolicy = "networkPolicy"
)

// gateContainers lists the dotted paths under featureGates whose map values
// hold nested gates rather than gate configuration. The flatten recurses only
// into these; any other map value is the gate's configuration, verbatim.
// Must be kept in sync with the FeatureGates type — asserted by
// TestBuildFeatureGateDataFullyPopulated.
var gateContainers = map[string]bool{
	"userDefinedNetworks":           true,
	"userDefinedNetworks.noOverlay": true,
	gateGroupRouting:                true,
	gateGroupEgress:                 true,
	gateGroupNetworkPolicy:          true,
}

// buildFeatureGateData converts the typed FeatureGates struct into:
//  1. A sorted []string of enabled gate names for template `has` checks.
//     Nested gates use dotted paths ("egress.firewall"); group containers
//     appear too (presence of "userDefinedNetworks" is the UDN gate itself).
//  2. A map of gate config values keyed by dotted gate name, for gates that
//     have configuration. Scalar fields inside a container belong to the
//     container's own config (e.g. userDefinedNetworks.advertisedIsolationMode).
func buildFeatureGateData(fg *k8sv1alpha1.FeatureGates) ([]string, map[string]any, error) {
	enabledGates := []string{}
	gateConfig := map[string]any{}
	if fg == nil {
		return enabledGates, gateConfig, nil
	}

	// JSON-marshal the struct, then walk keys — non-nil fields become enabled gates
	fgMap, err := structToMap(fg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal feature gates: %w", err)
	}

	var walk func(prefix string, node map[string]any)
	walk = func(prefix string, node map[string]any) {
		ownConfig := map[string]any{}
		for key, value := range node {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			childMap, isMap := value.(map[string]any)
			switch {
			case isMap && gateContainers[path]:
				enabledGates = append(enabledGates, path)
				walk(path, childMap)
			case isMap:
				enabledGates = append(enabledGates, path)
				if len(childMap) > 0 {
					gateConfig[path] = childMap
				}
			default:
				// Scalar field of the enclosing container's config.
				ownConfig[key] = value
			}
		}
		if prefix != "" && len(ownConfig) > 0 {
			existing, ok := gateConfig[prefix].(map[string]any)
			if !ok {
				existing = map[string]any{}
				gateConfig[prefix] = existing
			}
			maps.Copy(existing, ownConfig)
		}
	}
	walk("", fgMap)

	slices.Sort(enabledGates)
	return enabledGates, gateConfig, nil
}

// buildComponentData creates the template data for a single component by:
// 1. Resolving container images (CR override by logical name, or embedded default)
// 2. Applying containerImageMapping to get per-container image data
// 3. Merging replicas and customVariables from the CR's ComponentOverride
func (r *TemplateRenderer) buildComponentData(comp *defaults.EmbeddedComponent, overrides []k8sv1alpha1.ComponentOverride) (map[string]any, error) {
	compData := make(map[string]any)

	// Find user override for this component (if any)
	var override *k8sv1alpha1.ComponentOverride
	for i := range overrides {
		if overrides[i].ResourceName == comp.ResourceName {
			override = &overrides[i]
			break
		}
	}

	// Resolve images: CR overrides win, else embedded defaults
	imagesByLogicalName := make(map[string]defaults.EmbeddedContainerImage)
	for _, img := range comp.ContainerImages {
		imagesByLogicalName[img.Name] = img
	}

	// Apply CR image overrides by logical name
	if override != nil {
		for _, crImg := range override.ContainerImages {
			imagesByLogicalName[crImg.Name] = defaults.EmbeddedContainerImage{
				Name:       crImg.Name,
				Repository: crImg.Repository,
				Tag:        crImg.Tag,
				SHA256:     crImg.SHA256,
			}
		}
	}

	// Apply containerImageMapping: logical image → container names
	for logicalName, containerNames := range comp.ContainerImageMapping {
		img, ok := imagesByLogicalName[logicalName]
		if !ok {
			return nil, fmt.Errorf("component %q: containerImageMapping references logical image %q, but no matching containerImage was found in embedded config or CR overrides", comp.ResourceName, logicalName)
		}
		imageMap := map[string]any{
			"repository": img.Repository,
			"tag":        img.Tag,
			"sha256":     img.SHA256,
		}
		for _, containerName := range containerNames {
			compData[containerName] = map[string]any{
				"image": imageMap,
			}
		}
	}

	// Replicas — only set when the user provides one. Templates fall back
	// to a hardcoded sane default when this key is absent.
	if override != nil && override.Replicas != nil {
		compData["replicas"] = *override.Replicas
	}

	// Custom variables — component-level key-value pairs
	customVars := make(map[string]any)
	if override != nil {
		for _, cv := range override.CustomVariables {
			customVars[cv.Name] = maybeConvertBooleanString(cv.Value)
		}
	}
	compData["customVariables"] = customVars

	// NodePlacement — CR override wins, else embedded default (already merged by FillDefaults)
	if override != nil && override.NodePlacement != nil {
		npMap, err := structToMap(override.NodePlacement)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal NodePlacement for component %s: %w", comp.ResourceName, err)
		}
		compData["nodePlacement"] = npMap
	}

	return compData, nil
}

// RenderTemplateContent renders template content with OVNK8sOperator data.
// Used by test hooks to render inline template strings.
func (r *TemplateRenderer) RenderTemplateContent(templateContent string, cr *k8sv1alpha1.OVNK8sOperator) (string, error) {
	templateData, err := r.buildTemplateData(cr, RenderOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to build template data: %w", err)
	}

	// Add CR metadata for test hook templates
	templateData["OVNK8sOperator"] = map[string]any{
		"Name":      cr.Name,
		"Namespace": cr.Namespace,
	}

	tmpl, err := template.New("test-hook").
		Funcs(templateFuncs(r.templatesDir)).
		Parse(templateContent)
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, templateData); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return buf.String(), nil
}

// renderGlobalTemplates renders all templates in the global directory
func (r *TemplateRenderer) renderGlobalTemplates(templateData TemplateData) ([]*RenderedManifest, error) {
	globalTemplateDir := filepath.Join(r.templatesDir, "global")

	info, err := os.Stat(globalTemplateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("required global template directory does not exist: %s", globalTemplateDir)
		}
		return nil, fmt.Errorf("failed to access global template directory %s: %w", globalTemplateDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("required global template path is not a directory: %s", globalTemplateDir)
	}

	return r.renderComponentTemplates(globalTemplateDir, templateData)
}

// renderComponentTemplates renders all templates in a component's directory
func (r *TemplateRenderer) renderComponentTemplates(templateDir string, templateData TemplateData) ([]*RenderedManifest, error) {
	var manifests []*RenderedManifest

	err := filepath.Walk(templateDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() || (!strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml")) {
			return nil
		}

		renderedContent, err := r.renderTemplate(path, templateData)
		if err != nil {
			return fmt.Errorf("failed to render template %s: %w", path, err)
		}

		individualManifests := r.SplitYAMLDocuments(renderedContent)

		for i, manifestContent := range individualManifests {
			if strings.TrimSpace(manifestContent) == "" {
				continue
			}

			manifest := &RenderedManifest{
				Content:      strings.TrimSpace(manifestContent),
				TemplatePath: path,
				FileName:     fmt.Sprintf("%s[%d]", filepath.Base(path), i),
			}
			manifests = append(manifests, manifest)
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	return manifests, nil
}

// renderTemplate renders a Go template file with the provided data
func (r *TemplateRenderer) renderTemplate(templatePath string, data TemplateData) (string, error) {
	templateContent, err := os.ReadFile(templatePath)
	if err != nil {
		return "", fmt.Errorf("failed to read template file %s: %w", templatePath, err)
	}

	tmpl, err := template.New(filepath.Base(templatePath)).
		Funcs(templateFuncs(r.templatesDir)).
		Parse(string(templateContent))
	if err != nil {
		return "", fmt.Errorf("failed to parse template %s: %w", templatePath, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template %s: %w", templatePath, err)
	}

	return buf.String(), nil
}

// templateFuncs returns the template function map. includeRaw reads files
// relative to templatesDir without evaluating their contents. OpenInRoot
// prevents paths and symlinks from escaping that directory.
func templateFuncs(templatesDir string) template.FuncMap {
	return template.FuncMap{
		"includeRaw": func(path string) (string, error) {
			file, err := os.OpenInRoot(templatesDir, path)
			if err != nil {
				return "", fmt.Errorf("failed to include raw template %q: %w", path, err)
			}
			defer func() { _ = file.Close() }()

			content, err := io.ReadAll(file)
			if err != nil {
				return "", fmt.Errorf("failed to read raw template %q: %w", path, err)
			}
			return string(content), nil
		},
		"b64enc": func(s string) string {
			return base64.StdEncoding.EncodeToString([]byte(s))
		},
		"b64dec": func(s string) string {
			data, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				return ""
			}
			return string(data)
		},
		"has": func(needle string, haystack any) bool {
			switch h := haystack.(type) {
			case []any:
				for _, v := range h {
					if str, ok := v.(string); ok && str == needle {
						return true
					}
				}
			case []string:
				if slices.Contains(h, needle) {
					return true
				}
			}
			return false
		},
		// get safely traverses nested maps by keys, returning "" if any key is missing.
		// Usage: get .global.featureGateConfig "interconnect" "v4TransitSwitchSubnet"
		"get": func(data any, keys ...string) any {
			current := data
			for _, key := range keys {
				m, ok := current.(map[string]any)
				if !ok || m == nil {
					return ""
				}
				current, ok = m[key]
				if !ok {
					return ""
				}
			}
			return current
		},
		// toYaml serializes a value to YAML string. Returns "" on error.
		// Usage: {{ .nodePlacement.affinity | toYaml | indent 8 }}
		"toYaml": func(v any) string {
			out, err := sigyaml.Marshal(v)
			if err != nil {
				return ""
			}
			return strings.TrimSuffix(string(out), "\n")
		},
		// indent prepends each line with N spaces.
		// Usage: {{ .data | toYaml | indent 8 }}
		"indent": func(spaces int, v string) string {
			pad := strings.Repeat(" ", spaces)
			return pad + strings.ReplaceAll(v, "\n", "\n"+pad)
		},
	}
}

// SplitYAMLDocuments splits a YAML string containing multiple documents into individual documents
func (r *TemplateRenderer) SplitYAMLDocuments(yamlContent string) []string {
	documents := strings.Split(yamlContent, "\n---\n")

	var result []string
	for _, doc := range documents {
		doc = strings.TrimPrefix(doc, "---\n")
		doc = strings.TrimPrefix(doc, "---")

		if strings.TrimSpace(doc) != "" {
			result = append(result, doc)
		}
	}

	return result
}

// structToMap converts any struct to map[string]any using JSON marshaling.
// JSON tags determine the key names. Nil/omitempty fields are excluded.
func structToMap(obj any) (map[string]any, error) {
	jsonBytes, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal struct: %w", err)
	}

	var result map[string]any
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal to map: %w", err)
	}

	return result, nil
}

// maybeConvertBooleanString converts "true"/"false" strings to actual booleans.
// Needed because CustomVariables store values as strings but templates need booleans.
func maybeConvertBooleanString(value string) any {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true":
		return true
	case "false":
		return false
	default:
		return value
	}
}
