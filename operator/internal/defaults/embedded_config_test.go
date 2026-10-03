// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"os"
	"reflect"
	"strings"
	"testing"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
)

const testImageRepository = "registry.example/test/ovnkube-c"

// TestMain stamps the image-default vars for the whole package's tests so
// existing tests behave as if the binary were built with -ldflags. Tests
// that exercise the unset-stamp path (like TestImageDefaultsAppliedWhenConfigOmitsImage)
// override these locally and restore via t.Cleanup.
func TestMain(m *testing.M) {
	OvnKubeImageRepository = "ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu"
	OvnKubeImageTag = "test"
	OvnKubeImageDigest = ""
	os.Exit(m.Run())
}

// crForMode builds a minimal CR with the given deploymentMode and overrides.
func crForMode(mode string, overrides ...k8sv1alpha1.ComponentOverride) *k8sv1alpha1.OVNK8sOperator {
	return &k8sv1alpha1.OVNK8sOperator{
		Spec: k8sv1alpha1.OVNK8sOperatorSpec{
			Global:             k8sv1alpha1.GlobalConfig{DeploymentMode: mode},
			ComponentOverrides: overrides,
		},
	}
}

func TestLoadEmbeddedConfig(t *testing.T) {
	config, err := LoadEmbeddedConfig()
	if err != nil {
		t.Fatalf("LoadEmbeddedConfig() failed: %v", err)
	}

	if config.Version == "" {
		t.Error("Version is empty")
	}
	// MinUpgradeVersion can be empty for first release (no prior version to constrain)

	requiredComponents := []string{
		k8sv1alpha1.ComponentOvnkubeControlPlane, k8sv1alpha1.ComponentOvnkubeNode,
		k8sv1alpha1.ComponentOvnkubeIdentity, k8sv1alpha1.ComponentOvnkubeNodeDpuHost, k8sv1alpha1.ComponentOvnkubeNodeDpu,
	}
	for _, name := range requiredComponents {
		if config.GetComponent(name) == nil {
			t.Errorf("component %q not found", name)
		}
	}
}

func TestEmbeddedModes(t *testing.T) {
	config, err := LoadEmbeddedConfig()
	if err != nil {
		t.Fatalf("LoadEmbeddedConfig() failed: %v", err)
	}

	for _, name := range []string{k8sv1alpha1.DeploymentModeStandard, k8sv1alpha1.DeploymentModeDpuHost, k8sv1alpha1.DeploymentModeDpu} {
		if _, ok := config.Modes[name]; !ok {
			t.Errorf("mode %q not declared", name)
		}
	}

	// Default mode resolves to its required components only when the user
	// doesn't opt into anything optional.
	resolved, err := config.ResolveComponents(crForMode(k8sv1alpha1.DeploymentModeStandard))
	if err != nil {
		t.Fatalf("ResolveComponents(default): %v", err)
	}
	if got, want := componentNames(resolved), []string{k8sv1alpha1.ComponentOvnkubeControlPlane, k8sv1alpha1.ComponentOvnkubeNode}; !equalUnordered(got, want) {
		t.Errorf("default mode required set: got %v, want %v", got, want)
	}

	if _, err := config.ResolveComponents(crForMode("nonexistent")); err == nil {
		t.Error("expected error for unknown mode, got nil")
	}
}

// TestResolveComponentsOptional covers the opt-in semantics of
// EmbeddedMode.OptionalComponents: off by default, on when the user sets
// componentOverrides[name].enabled = true. Required components ignore the
// override flag.
func TestResolveComponentsOptional(t *testing.T) {
	config, err := LoadEmbeddedConfig()
	if err != nil {
		t.Fatalf("LoadEmbeddedConfig() failed: %v", err)
	}
	tru, fals := true, false

	tests := []struct {
		name      string
		overrides []k8sv1alpha1.ComponentOverride
		want      []string
	}{
		{
			name: "no overrides → only required components",
			want: []string{k8sv1alpha1.ComponentOvnkubeControlPlane, k8sv1alpha1.ComponentOvnkubeNode},
		},
		{
			name: "opt in to ovnkubeIdentity",
			overrides: []k8sv1alpha1.ComponentOverride{
				{ResourceName: k8sv1alpha1.ComponentOvnkubeIdentity, Enabled: &tru},
			},
			want: []string{k8sv1alpha1.ComponentOvnkubeControlPlane, k8sv1alpha1.ComponentOvnkubeNode, k8sv1alpha1.ComponentOvnkubeIdentity},
		},
		{
			name: "explicit enabled=false on optional is a no-op (already off)",
			overrides: []k8sv1alpha1.ComponentOverride{
				{ResourceName: k8sv1alpha1.ComponentOvnkubeIdentity, Enabled: &fals},
			},
			want: []string{k8sv1alpha1.ComponentOvnkubeControlPlane, k8sv1alpha1.ComponentOvnkubeNode},
		},
		{
			name: "enabled=false on a required component is ignored",
			overrides: []k8sv1alpha1.ComponentOverride{
				{ResourceName: k8sv1alpha1.ComponentOvnkubeNode, Enabled: &fals},
			},
			want: []string{k8sv1alpha1.ComponentOvnkubeControlPlane, k8sv1alpha1.ComponentOvnkubeNode},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := config.ResolveComponents(crForMode(k8sv1alpha1.DeploymentModeStandard, tt.overrides...))
			if err != nil {
				t.Fatalf("ResolveComponents: %v", err)
			}
			if got := componentNames(resolved); !equalUnordered(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func componentNames(comps []EmbeddedComponent) []string {
	out := make([]string, 0, len(comps))
	for _, c := range comps {
		out = append(out, c.ResourceName)
	}
	return out
}

func equalUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}

func TestGetComponent(t *testing.T) {
	config, err := LoadEmbeddedConfig()
	if err != nil {
		t.Fatalf("LoadEmbeddedConfig() failed: %v", err)
	}

	t.Run("existing component", func(t *testing.T) {
		cp := config.GetComponent(k8sv1alpha1.ComponentOvnkubeControlPlane)
		if cp == nil {
			t.Fatal("expected ovnkubeControlPlane, got nil")
		}
		if cp.ResourceType != "Deployment" {
			t.Errorf("expected ResourceType=Deployment, got %q", cp.ResourceType)
		}
		if len(cp.ContainerImages) == 0 {
			t.Error("expected at least one container image")
		}
		if len(cp.ContainerImageMapping) == 0 {
			t.Error("expected non-empty containerImageMapping")
		}
	})

	t.Run("node dependencies", func(t *testing.T) {
		node := config.GetComponent(k8sv1alpha1.ComponentOvnkubeNode)
		if node == nil {
			t.Fatalf("expected ovnkubeNode component, got nil")
		}
		expectedDependencies := []string{k8sv1alpha1.ComponentOvnkubeControlPlane, k8sv1alpha1.ComponentOvnkubeIdentity}
		if !reflect.DeepEqual(node.DependsOn, expectedDependencies) {
			t.Errorf("ovnkubeNode.DependsOn = %v, want %v", node.DependsOn, expectedDependencies)
		}
	})

	t.Run("non-existent", func(t *testing.T) {
		if config.GetComponent("nonexistent") != nil {
			t.Error("expected nil for non-existent component")
		}
	})
}

func TestParseAndValidateConfig(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "invalid YAML syntax",
			yaml:    "{{bad: yaml: [",
			wantErr: "failed to parse config",
		},
		{
			name:    "valid YAML but missing version",
			yaml:    "components: []\n",
			wantErr: "version is empty",
		},
		{
			name: "invalid release version",
			yaml: `
version: "not-semver"
components: []
`,
			wantErr: `version "not-semver" is not a valid semantic version`,
		},
		{
			name: "invalid minimum upgrade version",
			yaml: `
version: "1.0.0"
minUpgradeVersion: "not-semver"
components: []
`,
			wantErr: `minimum upgrade version "not-semver" is not a valid semantic version`,
		},
		{
			name: "minimum upgrade version equals release version",
			yaml: `
version: "1.0.0"
minUpgradeVersion: "v1.0.0"
components: []
`,
			wantErr: `minimum upgrade version "v1.0.0" must be lower than version "1.0.0"`,
		},
		{
			name: "minimum upgrade version exceeds release version",
			yaml: `
version: "1.0.0"
minUpgradeVersion: "1.1.0"
components: []
`,
			wantErr: `minimum upgrade version "1.1.0" must be lower than version "1.0.0"`,
		},
		{
			name: "component with empty resourceName",
			yaml: `
version: "1.0.0"
components:
  - resourceType: Deployment
`,
			wantErr: "component has empty resourceName",
		},
		{
			name: "valid YAML but invalid image mapping",
			yaml: `
version: "1.0.0"
components:
  - resourceName: test
    containerImages:
      - name: img1
        repository: repo
        tag: v1
    containerImageMapping:
      missing: ["c1"]
`,
			wantErr: `containerImageMapping references image "missing"`,
		},
		{
			name: "valid config parses successfully",
			yaml: `
version: "1.0.0"
components:
  - resourceName: test
    resourceType: Deployment
    containerImages:
      - name: img1
        repository: repo
        tag: v1
    containerImageMapping:
      img1: ["c1"]
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := parseAndValidateConfig([]byte(tt.yaml))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
				if config == nil {
					t.Fatal("expected non-nil config")
				}
				if config.Version != "1.0.0" {
					t.Fatalf("expected version 1.0.0, got %q", config.Version)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

// TestImageDefaultsOverriddenByLdflags verifies that when the build-time
// linker vars are set, they override the fallback image declared in
// config.yaml for every component that has a containerImageMapping.
func TestImageDefaultsOverriddenByLdflags(t *testing.T) {
	prevRepo := OvnKubeImageRepository
	prevTag := OvnKubeImageTag
	prevDigest := OvnKubeImageDigest
	OvnKubeImageRepository = testImageRepository
	OvnKubeImageTag = "v9.9.9"
	OvnKubeImageDigest = "sha256:deadbeef"
	t.Cleanup(func() {
		OvnKubeImageRepository = prevRepo
		OvnKubeImageTag = prevTag
		OvnKubeImageDigest = prevDigest
	})

	cfg, err := LoadEmbeddedConfig()
	if err != nil {
		t.Fatalf("LoadEmbeddedConfig: %v", err)
	}

	for _, comp := range cfg.Components {
		if len(comp.ContainerImageMapping) == 0 {
			continue
		}
		if len(comp.ContainerImages) == 0 {
			t.Errorf("component %q: no default image present", comp.ResourceName)
			continue
		}
		img := comp.ContainerImages[0]
		if img.Name != DefaultImageName {
			t.Errorf("component %q: image name = %q, want \"default\"", comp.ResourceName, img.Name)
		}
		if img.Repository != testImageRepository {
			t.Errorf("component %q repo = %q, want ldflags override", comp.ResourceName, img.Repository)
		}
		if img.Tag != "v9.9.9" {
			t.Errorf("component %q tag = %q, want ldflags override", comp.ResourceName, img.Tag)
		}
		if img.SHA256 != "deadbeef" {
			t.Errorf("component %q sha256 = %q, want ldflags digest minus prefix", comp.ResourceName, img.SHA256)
		}
	}
}

// TestImageFallbackWhenUnstamped verifies that a default build (no -ldflags;
// linker vars empty) loads the fallback images declared in config.yaml, so
// the operator is functional out of the box.
func TestImageFallbackWhenUnstamped(t *testing.T) {
	prevRepo := OvnKubeImageRepository
	prevTag := OvnKubeImageTag
	prevDigest := OvnKubeImageDigest
	OvnKubeImageRepository = ""
	OvnKubeImageTag = ""
	OvnKubeImageDigest = ""
	t.Cleanup(func() {
		OvnKubeImageRepository = prevRepo
		OvnKubeImageTag = prevTag
		OvnKubeImageDigest = prevDigest
	})

	cfg, err := LoadEmbeddedConfig()
	if err != nil {
		t.Fatalf("LoadEmbeddedConfig with no ldflags should succeed using config.yaml fallback, got: %v", err)
	}

	for _, name := range []string{k8sv1alpha1.ComponentOvnkubeControlPlane, k8sv1alpha1.ComponentOvnkubeNode} {
		comp := cfg.GetComponent(name)
		if comp == nil {
			t.Fatalf("component %q not found", name)
		}
		if len(comp.ContainerImages) == 0 {
			t.Fatalf("component %q: expected fallback image from config.yaml, got none", name)
		}
		img := comp.ContainerImages[0]
		if img.Repository != "ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu" {
			t.Errorf("component %q repo = %q, want config.yaml fallback", name, img.Repository)
		}
		if img.Tag != "master" {
			t.Errorf("component %q tag = %q, want config.yaml fallback", name, img.Tag)
		}
	}
}

// TestImageStampedWithoutDigest verifies that stamping with only a repository
// and tag (no digest) still overrides the operand image. A CI build that stamps
// a tag without computing a digest must pin the operand by repository:tag rather
// than silently falling back to the config.yaml default.
func TestImageStampedWithoutDigest(t *testing.T) {
	prevRepo := OvnKubeImageRepository
	prevTag := OvnKubeImageTag
	prevDigest := OvnKubeImageDigest
	OvnKubeImageRepository = testImageRepository
	OvnKubeImageTag = "mr-12345"
	OvnKubeImageDigest = ""
	t.Cleanup(func() {
		OvnKubeImageRepository = prevRepo
		OvnKubeImageTag = prevTag
		OvnKubeImageDigest = prevDigest
	})

	cfg, err := LoadEmbeddedConfig()
	if err != nil {
		t.Fatalf("LoadEmbeddedConfig: %v", err)
	}

	for _, name := range []string{k8sv1alpha1.ComponentOvnkubeControlPlane, k8sv1alpha1.ComponentOvnkubeNode} {
		comp := cfg.GetComponent(name)
		if comp == nil {
			t.Fatalf("component %q not found", name)
		}
		if len(comp.ContainerImages) == 0 {
			t.Fatalf("component %q: no default image present", name)
		}
		img := comp.ContainerImages[0]
		if img.Repository != testImageRepository {
			t.Errorf("component %q repo = %q, want stamped override (not config.yaml fallback)", name, img.Repository)
		}
		if img.Tag != "mr-12345" {
			t.Errorf("component %q tag = %q, want stamped override", name, img.Tag)
		}
		if img.SHA256 != "" {
			t.Errorf("component %q sha256 = %q, want empty (no digest stamped)", name, img.SHA256)
		}
	}
}

func TestContainerImageMapping(t *testing.T) {
	config, err := LoadEmbeddedConfig()
	if err != nil {
		t.Fatalf("LoadEmbeddedConfig() failed: %v", err)
	}

	node := config.GetComponent(k8sv1alpha1.ComponentOvnkubeNode)
	if node == nil {
		t.Fatal("expected ovnkubeNode")
	}

	wantContainers := []string{
		"nbOvsdb", "sbOvsdb", "ovnNorthd", "ovnController", "ovnkubeController",
	}
	got := node.ContainerImageMapping[DefaultImageName]
	if !equalUnordered(got, wantContainers) {
		t.Errorf("default image mapping: got %v, want %v", got, wantContainers)
	}
}
