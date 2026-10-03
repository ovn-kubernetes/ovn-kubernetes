// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"embed"
	"fmt"
	"strings"

	"golang.org/x/mod/semver"

	"sigs.k8s.io/yaml"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
)

//go:embed config.yaml
var configFS embed.FS

// EmbeddedConfig holds the operator's internal configuration loaded from config.yaml.
// Contains versions, mode → component composition, and per-component definitions.
// Feature gates and global networking config are entirely user-controlled via the CR.
type EmbeddedConfig struct {
	Version           string                  `json:"version"`
	MinUpgradeVersion string                  `json:"minUpgradeVersion"`
	Modes             map[string]EmbeddedMode `json:"modes"`
	Components        []EmbeddedComponent     `json:"components"`
}

// EmbeddedMode declares the component composition for a deployment mode.
// Mode owns the list of components, nothing else — per-component defaults
// live in EmbeddedComponent.
type EmbeddedMode struct {
	// Components is the required component set for this mode. Always
	// included; componentOverrides[name].enabled is ignored. Each entry must
	// reference an EmbeddedComponent in EmbeddedConfig.Components.
	Components []string `json:"components"`

	// OptionalComponents lists components available in this mode but off by
	// default. Users opt in via componentOverrides[name].enabled = true. Used
	// for components that only fit a subset of clusters in a mode (e.g.
	// ovnkubeIdentity when the user wants the CSR webhook).
	// +optional
	OptionalComponents []string `json:"optionalComponents,omitempty"`
}

// EmbeddedComponent defines a deployable component with its images,
// dependencies, and operator-internal metadata.
type EmbeddedComponent struct {
	ResourceName          string                   `json:"resourceName"`
	ResourceType          string                   `json:"resourceType"`
	DependsOn             []string                 `json:"dependsOn"`
	ContainerImages       []EmbeddedContainerImage `json:"containerImages"`
	ContainerImageMapping map[string][]string      `json:"containerImageMapping"`
}

// EmbeddedContainerImage defines a container image by logical name.
type EmbeddedContainerImage struct {
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	SHA256     string `json:"sha256"`
}

// LoadEmbeddedConfig reads and parses the embedded config.yaml.
func LoadEmbeddedConfig() (*EmbeddedConfig, error) {
	data, err := configFS.ReadFile("config.yaml")
	if err != nil {
		return nil, fmt.Errorf("failed to read embedded config.yaml: %w", err)
	}
	return parseAndValidateConfig(data)
}

// parseAndValidateConfig unmarshals YAML data into an EmbeddedConfig and validates it.
func parseAndValidateConfig(data []byte) (*EmbeddedConfig, error) {
	var config EmbeddedConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	config.applyImageDefaults()

	if err := config.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &config, nil
}

// DefaultImageName is the logical image name that covers every container of a
// component unless the component maps containers to other images.
const DefaultImageName = "default"

// applyImageDefaults overrides each component's "default" container image
// with the build-time linker-stamped values in images.go, so release/CI
// builds pin the exact ovnkube-c (repository/tag/digest) produced for the
// branch.
//
// When the linker vars are unset (a plain `go build` with no -ldflags),
// this is a no-op and the fallback images declared in config.yaml are used,
// keeping default builds functional.
func (c *EmbeddedConfig) applyImageDefaults() {
	if OvnKubeImageRepository == "" || OvnKubeImageTag == "" {
		return
	}
	digest := OvnKubeImageDigest
	digest = strings.TrimPrefix(digest, "sha256:")
	for i := range c.Components {
		comp := &c.Components[i]
		// Only touch the "default" image, and only where the component maps
		// containers to it. Components using other image names are untouched.
		if _, ok := comp.ContainerImageMapping[DefaultImageName]; !ok {
			continue
		}
		updated := false
		for j := range comp.ContainerImages {
			if comp.ContainerImages[j].Name == DefaultImageName {
				comp.ContainerImages[j].Repository = OvnKubeImageRepository
				comp.ContainerImages[j].Tag = OvnKubeImageTag
				comp.ContainerImages[j].SHA256 = digest
				updated = true
				break
			}
		}
		if !updated {
			comp.ContainerImages = append(comp.ContainerImages, EmbeddedContainerImage{
				Name:       DefaultImageName,
				Repository: OvnKubeImageRepository,
				Tag:        OvnKubeImageTag,
				SHA256:     digest,
			})
		}
	}
}

// validate checks that the embedded config is internally consistent.
func (c *EmbeddedConfig) validate() error {
	if c.Version == "" {
		return fmt.Errorf("version is empty")
	}
	version := "v" + strings.TrimPrefix(c.Version, "v")
	if !semver.IsValid(version) {
		return fmt.Errorf("version %q is not a valid semantic version", c.Version)
	}
	if c.MinUpgradeVersion != "" {
		minUpgradeVersion := "v" + strings.TrimPrefix(c.MinUpgradeVersion, "v")
		if !semver.IsValid(minUpgradeVersion) {
			return fmt.Errorf("minimum upgrade version %q is not a valid semantic version", c.MinUpgradeVersion)
		}
		if semver.Compare(minUpgradeVersion, version) >= 0 {
			return fmt.Errorf("minimum upgrade version %q must be lower than version %q", c.MinUpgradeVersion, c.Version)
		}
	}

	// Build the set of known component resourceNames first so mode
	// declarations can be validated against it.
	known := make(map[string]struct{}, len(c.Components))
	for _, comp := range c.Components {
		if comp.ResourceName == "" {
			return fmt.Errorf("component has empty resourceName")
		}
		known[comp.ResourceName] = struct{}{}

		imageNames := make(map[string]bool, len(comp.ContainerImages))
		for _, img := range comp.ContainerImages {
			imageNames[img.Name] = true
		}
		for imageName := range comp.ContainerImageMapping {
			if !imageNames[imageName] {
				return fmt.Errorf("component %q: containerImageMapping references image %q, but no containerImage with that name exists",
					comp.ResourceName, imageName)
			}
		}
	}

	// Each mode's component lists must reference declared components, and a
	// component cannot appear in both required and optional sets.
	for modeName, mode := range c.Modes {
		if len(mode.Components) == 0 {
			return fmt.Errorf("mode %q declares no required components", modeName)
		}
		required := make(map[string]struct{}, len(mode.Components))
		for _, name := range mode.Components {
			if _, ok := known[name]; !ok {
				return fmt.Errorf("mode %q references undeclared component %q", modeName, name)
			}
			required[name] = struct{}{}
		}
		for _, name := range mode.OptionalComponents {
			if _, ok := known[name]; !ok {
				return fmt.Errorf("mode %q references undeclared optional component %q", modeName, name)
			}
			if _, dup := required[name]; dup {
				return fmt.Errorf("mode %q lists component %q as both required and optional", modeName, name)
			}
		}
	}
	return nil
}

// ResolveComponents returns the effective component set for the given CR.
//
// Resolution:
//  1. Pick the mode profile by spec.global.deploymentMode.
//  2. Always include the mode's required components.
//  3. Include the mode's optional components only when the user opts in via
//     componentOverrides[name].enabled = true.
//
// Required components ignore the override's enabled flag — they're required.
// Single source of truth used by the renderer and the deployer DAG. Returns
// an error if the CR's deploymentMode is not declared.
func (c *EmbeddedConfig) ResolveComponents(cr *k8sv1alpha1.OVNK8sOperator) ([]EmbeddedComponent, error) {
	mode, ok := c.Modes[cr.Spec.Global.DeploymentMode]
	if !ok {
		return nil, fmt.Errorf("mode %q is not declared in the embedded config", cr.Spec.Global.DeploymentMode)
	}

	enabled := make(map[string]bool, len(cr.Spec.ComponentOverrides))
	for _, ov := range cr.Spec.ComponentOverrides {
		if ov.Enabled != nil {
			enabled[ov.ResourceName] = *ov.Enabled
		}
	}

	out := make([]EmbeddedComponent, 0, len(mode.Components)+len(mode.OptionalComponents))
	for _, name := range mode.Components {
		if comp := c.GetComponent(name); comp != nil {
			out = append(out, *comp)
		}
	}
	for _, name := range mode.OptionalComponents {
		if !enabled[name] {
			continue
		}
		if comp := c.GetComponent(name); comp != nil {
			out = append(out, *comp)
		}
	}
	return out, nil
}

// GetComponent returns the component with the given resourceName, or nil if not found.
func (c *EmbeddedConfig) GetComponent(resourceName string) *EmbeddedComponent {
	for i := range c.Components {
		if c.Components[i].ResourceName == resourceName {
			return &c.Components[i]
		}
	}
	return nil
}
