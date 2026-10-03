// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package renderer_test

import (
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/defaults"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/renderer"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// testEmbeddedConfig returns a minimal embedded config for testing
func testEmbeddedConfig() *defaults.EmbeddedConfig {
	return &defaults.EmbeddedConfig{
		Version:           "1.4.0",
		MinUpgradeVersion: "1.0.0",
		Modes: map[string]defaults.EmbeddedMode{
			k8sv1alpha1.DeploymentModeStandard: {Components: []string{k8sv1alpha1.ComponentOvnkubeControlPlane, k8sv1alpha1.ComponentOvnkubeNode}},
		},
		Components: []defaults.EmbeddedComponent{
			{
				ResourceName: k8sv1alpha1.ComponentOvnkubeControlPlane,
				ResourceType: "Deployment",
				ContainerImages: []defaults.EmbeddedContainerImage{
					{Name: defaults.DefaultImageName, Repository: "example.io/ovnkube", Tag: "v1.0", SHA256: "abc123"},
				},
				ContainerImageMapping: map[string][]string{
					defaults.DefaultImageName: {"ovnkubeClusterManager"},
				},
			},
			{
				ResourceName: k8sv1alpha1.ComponentOvnkubeNode,
				ResourceType: "DaemonSet",
				DependsOn:    []string{k8sv1alpha1.ComponentOvnkubeControlPlane},
				ContainerImages: []defaults.EmbeddedContainerImage{
					{Name: defaults.DefaultImageName, Repository: "example.io/ovnkube", Tag: "v1.0", SHA256: "abc123"},
				},
				ContainerImageMapping: map[string][]string{
					defaults.DefaultImageName: {"nbOvsdb", "sbOvsdb", "ovnNorthd", "ovnController", "ovnkubeController"},
				},
			},
		},
	}
}

// testCR returns a minimal OVNK8sOperator CR for testing
func testCR() *k8sv1alpha1.OVNK8sOperator {
	return &k8sv1alpha1.OVNK8sOperator{
		ObjectMeta: metav1.ObjectMeta{Name: "test-operator"},
		Spec: k8sv1alpha1.OVNK8sOperatorSpec{
			Global: k8sv1alpha1.GlobalConfig{
				K8sAPIServer:   "https://10.0.0.1:6443",
				PodNetwork:     []k8sv1alpha1.PodNetworkEntry{{CIDR: "10.192.0.0/16", HostPrefix: 26}},
				ServiceNetwork: []string{"10.96.0.0/12"},
				MTU:            1440,
				DeploymentMode: k8sv1alpha1.DeploymentModeStandard,
				Namespace:      "ovn-kubernetes",
			},
		},
	}
}

var _ = Describe("TemplateRenderer", func() {
	var (
		tr      *renderer.TemplateRenderer
		tempDir string
		config  *defaults.EmbeddedConfig
	)

	BeforeEach(func() {
		var err error
		tempDir, err = os.MkdirTemp("", "renderer-test-*")
		Expect(err).ToNot(HaveOccurred())
		for _, name := range []string{
			"global",
			k8sv1alpha1.ComponentOvnkubeControlPlane,
			k8sv1alpha1.ComponentOvnkubeNode,
		} {
			Expect(os.MkdirAll(filepath.Join(tempDir, name), 0755)).To(Succeed())
		}

		config = testEmbeddedConfig()
		tr = renderer.NewTemplateRenderer(tempDir, config)
	})

	AfterEach(func() {
		Expect(os.RemoveAll(tempDir)).To(Succeed())
	})

	Describe("SplitYAMLDocuments", func() {
		It("should split multiple YAML documents", func() {
			docs := tr.SplitYAMLDocuments("doc1\n---\ndoc2")
			Expect(docs).To(HaveLen(2))
		})

		It("should handle leading ---", func() {
			docs := tr.SplitYAMLDocuments("---\ndoc1\n---\ndoc2")
			Expect(docs).To(HaveLen(2))
		})

		It("should skip empty documents", func() {
			docs := tr.SplitYAMLDocuments("doc1\n---\n\n---\ndoc2")
			Expect(docs).To(HaveLen(2))
		})
	})

	Describe("Raw template inclusion", func() {
		It("should include a raw template without evaluating its contents", func() {
			rawDir := filepath.Join(tempDir, "raw")
			Expect(os.MkdirAll(rawDir, 0755)).To(Succeed())
			rawTemplate := "image: {{ .ovnkube_image }}\n{{- if .enable_uplink }}\nuplink: true\n{{- end }}\n"
			Expect(os.WriteFile(filepath.Join(rawDir, "payload.tpl"), []byte(rawTemplate), 0644)).To(Succeed())

			result, err := tr.RenderTemplateContent(
				`{{ includeRaw "raw/payload.tpl" }}`,
				testCR(),
			)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(Equal(rawTemplate))
		})

		It("should report a missing raw template", func() {
			_, err := tr.RenderTemplateContent(
				`{{ includeRaw "raw/missing.tpl" }}`,
				testCR(),
			)
			Expect(err).To(MatchError(ContainSubstring(
				`failed to include raw template "raw/missing.tpl"`,
			)))
		})

		It("should reject paths outside the template directory", func() {
			outsidePath := filepath.Join(
				filepath.Dir(tempDir),
				filepath.Base(tempDir)+"-outside.tpl",
			)
			Expect(os.WriteFile(outsidePath, []byte("outside"), 0644)).To(Succeed())
			DeferCleanup(os.Remove, outsidePath)

			relativePath, err := filepath.Rel(tempDir, outsidePath)
			Expect(err).ToNot(HaveOccurred())
			_, err = tr.RenderTemplateContent(
				`{{ includeRaw "`+relativePath+`" }}`,
				testCR(),
			)
			Expect(err).To(MatchError(ContainSubstring(
				`failed to include raw template "` + relativePath + `"`,
			)))
		})
	})

	Describe("RenderAllTemplates", func() {
		It("should render global and component templates", func() {
			// Create global template
			globalDir := filepath.Join(tempDir, "global")
			Expect(os.MkdirAll(globalDir, 0755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(globalDir, "ns.yaml"), []byte(
				`apiVersion: v1
kind: Namespace
metadata:
  name: {{ .global.namespace }}`), 0644)).To(Succeed())

			// Create component template
			cpDir := filepath.Join(tempDir, k8sv1alpha1.ComponentOvnkubeControlPlane)
			Expect(os.MkdirAll(cpDir, 0755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(cpDir, "deploy.yaml"), []byte(
				`apiVersion: apps/v1
kind: Deployment
metadata:
  name: ovnkube-control-plane
spec:
  replicas: {{or .ovnkubeControlPlane.replicas 3}}`), 0644)).To(Succeed())

			cr := testCR()
			manifests, err := tr.RenderAllTemplates(cr, renderer.RenderOptions{})
			Expect(err).ToNot(HaveOccurred())

			Expect(manifests).To(HaveKey("global"))
			Expect(manifests["global"][0].Content).To(ContainSubstring("name: ovn-kubernetes"))

			Expect(manifests).To(HaveKey(k8sv1alpha1.ComponentOvnkubeControlPlane))
			Expect(manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][0].Content).To(ContainSubstring("replicas: 3"))
		})

		It("should fail when a resolved component has no template directory", func() {
			missingDir := filepath.Join(tempDir, k8sv1alpha1.ComponentOvnkubeNode)
			Expect(os.Remove(missingDir)).To(Succeed())

			cr := testCR()
			manifests, err := tr.RenderAllTemplates(cr, renderer.RenderOptions{})
			Expect(err).To(MatchError(And(
				ContainSubstring(`resolved component "ovnkubeNode" has no template directory`),
				ContainSubstring(missingDir),
			)))
			Expect(manifests).To(BeNil())
		})

		It("should fail when the required global template directory is absent", func() {
			globalDir := filepath.Join(tempDir, "global")
			Expect(os.Remove(globalDir)).To(Succeed())

			cr := testCR()
			manifests, err := tr.RenderAllTemplates(cr, renderer.RenderOptions{})
			Expect(err).To(MatchError(And(
				ContainSubstring("failed to render global templates"),
				ContainSubstring("required global template directory does not exist"),
				ContainSubstring(globalDir),
			)))
			Expect(manifests).To(BeNil())
		})

		It("should not require template directories for components pruned by the resolved mode", func() {
			globalDir := filepath.Join(tempDir, "global")
			Expect(os.MkdirAll(globalDir, 0755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(globalDir, "components.yaml"), []byte(
				`components: {{ .global.components }}`), 0644)).To(Succeed())

			cpDir := filepath.Join(tempDir, k8sv1alpha1.ComponentOvnkubeControlPlane)
			Expect(os.MkdirAll(cpDir, 0755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(cpDir, "deploy.yaml"), []byte(
				`kind: Deployment`), 0644)).To(Succeed())

			nodeDir := filepath.Join(tempDir, k8sv1alpha1.ComponentOvnkubeNode)
			Expect(os.MkdirAll(nodeDir, 0755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(nodeDir, "ds.yaml"), []byte(
				`kind: DaemonSet`), 0644)).To(Succeed())

			// Restrict the default mode to ovnkubeControlPlane only.
			config.Modes[k8sv1alpha1.DeploymentModeStandard] = defaults.EmbeddedMode{
				Components: []string{k8sv1alpha1.ComponentOvnkubeControlPlane},
			}
			Expect(os.RemoveAll(nodeDir)).To(Succeed())

			cr := testCR()
			manifests, err := tr.RenderAllTemplates(cr, renderer.RenderOptions{})
			Expect(err).ToNot(HaveOccurred())

			Expect(manifests).To(HaveKey(k8sv1alpha1.ComponentOvnkubeControlPlane))
			Expect(manifests).ToNot(HaveKey(k8sv1alpha1.ComponentOvnkubeNode))

			Expect(manifests["global"][0].Content).To(ContainSubstring(
				"components: [" + k8sv1alpha1.ComponentOvnkubeControlPlane + "]"))
		})

		It("should not require a template directory for a disabled optional component", func() {
			config.Modes[k8sv1alpha1.DeploymentModeStandard] = defaults.EmbeddedMode{
				Components:         []string{k8sv1alpha1.ComponentOvnkubeControlPlane},
				OptionalComponents: []string{k8sv1alpha1.ComponentOvnkubeIdentity},
			}
			config.Components = append(config.Components, defaults.EmbeddedComponent{
				ResourceName: k8sv1alpha1.ComponentOvnkubeIdentity,
				ResourceType: "DaemonSet",
			})
			Expect(filepath.Join(tempDir, k8sv1alpha1.ComponentOvnkubeIdentity)).ToNot(BeADirectory())

			manifests, err := tr.RenderAllTemplates(testCR(), renderer.RenderOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(manifests).ToNot(HaveKey(k8sv1alpha1.ComponentOvnkubeIdentity))
		})

		It("should allow existing template directories that render no manifests", func() {
			manifests, err := tr.RenderAllTemplates(testCR(), renderer.RenderOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(manifests).To(BeEmpty())
		})
	})

	Describe("Template Data: Global Config", func() {
		It("should expose typed global fields directly", func() {
			cpDir := filepath.Join(tempDir, k8sv1alpha1.ComponentOvnkubeControlPlane)
			Expect(os.MkdirAll(cpDir, 0755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(cpDir, "test.yaml"), []byte(
				`ns: {{ .global.namespace }}
mtu: {{ .global.mtu }}
mode: {{ .global.deploymentMode }}`), 0644)).To(Succeed())

			cr := testCR()
			cr.Spec.Global.V4JoinSubnet = "100.64.0.0/16"

			manifests, err := tr.RenderAllTemplates(cr, renderer.RenderOptions{})
			Expect(err).ToNot(HaveOccurred())

			content := manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][0].Content
			Expect(content).To(ContainSubstring("ns: ovn-kubernetes"))
			Expect(content).To(ContainSubstring("mtu: 1440"))
			Expect(content).To(ContainSubstring("mode: standard"))
		})
	})

	Describe("Template Data: Feature Gates", func() {
		It("should produce featureGates as string list for has checks", func() {
			cpDir := filepath.Join(tempDir, k8sv1alpha1.ComponentOvnkubeControlPlane)
			Expect(os.MkdirAll(cpDir, 0755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(cpDir, "test.yaml"), []byte(
				`egressIP: {{ has "egress.ip" .global.featureGates }}
interconnect: {{ has "interconnect" .global.featureGates }}
noOverlay: {{ has "userDefinedNetworks.noOverlay" .global.featureGates }}`), 0644)).To(Succeed())

			cr := testCR()
			cr.Spec.FeatureGates = &k8sv1alpha1.FeatureGates{
				Egress:       &k8sv1alpha1.EgressConfig{IP: &k8sv1alpha1.EgressIPConfig{}},
				Interconnect: &k8sv1alpha1.InterconnectConfig{},
			}

			manifests, err := tr.RenderAllTemplates(cr, renderer.RenderOptions{})
			Expect(err).ToNot(HaveOccurred())

			content := manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][0].Content
			Expect(content).To(ContainSubstring("egressIP: true"))
			Expect(content).To(ContainSubstring("interconnect: true"))
			Expect(content).To(ContainSubstring("noOverlay: false"))
		})

		It("should expose feature gate config values", func() {
			cpDir := filepath.Join(tempDir, k8sv1alpha1.ComponentOvnkubeControlPlane)
			Expect(os.MkdirAll(cpDir, 0755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(cpDir, "test.yaml"), []byte(
				`subnet: {{ index .global.featureGateConfig "interconnect" "v4TransitSwitchSubnet" }}`), 0644)).To(Succeed())

			cr := testCR()
			cr.Spec.FeatureGates = &k8sv1alpha1.FeatureGates{
				Interconnect: &k8sv1alpha1.InterconnectConfig{
					V4TransitSwitchSubnet: "100.88.0.0/16",
				},
			}

			manifests, err := tr.RenderAllTemplates(cr, renderer.RenderOptions{})
			Expect(err).ToNot(HaveOccurred())

			content := manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][0].Content
			Expect(content).To(ContainSubstring("subnet: 100.88.0.0/16"))
		})

		It("should handle nil feature gates", func() {
			cpDir := filepath.Join(tempDir, k8sv1alpha1.ComponentOvnkubeControlPlane)
			Expect(os.MkdirAll(cpDir, 0755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(cpDir, "test.yaml"), []byte(
				`ic: {{ has "interconnect" .global.featureGates }}`), 0644)).To(Succeed())

			cr := testCR() // no featureGates
			manifests, err := tr.RenderAllTemplates(cr, renderer.RenderOptions{})
			Expect(err).ToNot(HaveOccurred())

			content := manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][0].Content
			Expect(content).To(ContainSubstring("ic: false"))
		})
	})

	Describe("Template Data: Component Images", func() {
		It("should resolve images from embedded config via containerImageMapping", func() {
			cpDir := filepath.Join(tempDir, k8sv1alpha1.ComponentOvnkubeControlPlane)
			Expect(os.MkdirAll(cpDir, 0755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(cpDir, "test.yaml"), []byte(
				`image: {{ .ovnkubeControlPlane.ovnkubeClusterManager.image.repository }}:{{ .ovnkubeControlPlane.ovnkubeClusterManager.image.tag }}`), 0644)).To(Succeed())

			cr := testCR()
			manifests, err := tr.RenderAllTemplates(cr, renderer.RenderOptions{})
			Expect(err).ToNot(HaveOccurred())

			content := manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][0].Content
			Expect(content).To(ContainSubstring("image: example.io/ovnkube:v1.0"))
		})

		It("should apply CR image overrides by logical name", func() {
			cpDir := filepath.Join(tempDir, k8sv1alpha1.ComponentOvnkubeControlPlane)
			Expect(os.MkdirAll(cpDir, 0755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(cpDir, "test.yaml"), []byte(
				`image: {{ .ovnkubeControlPlane.ovnkubeClusterManager.image.repository }}:{{ .ovnkubeControlPlane.ovnkubeClusterManager.image.tag }}`), 0644)).To(Succeed())

			cr := testCR()
			cr.Spec.ComponentOverrides = []k8sv1alpha1.ComponentOverride{
				{
					ResourceName: k8sv1alpha1.ComponentOvnkubeControlPlane,
					ContainerImages: []k8sv1alpha1.ContainerImage{
						{Name: defaults.DefaultImageName, Repository: "custom.io/ovnkube", Tag: "v2.0"},
					},
				},
			}

			manifests, err := tr.RenderAllTemplates(cr, renderer.RenderOptions{})
			Expect(err).ToNot(HaveOccurred())

			content := manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][0].Content
			Expect(content).To(ContainSubstring("image: custom.io/ovnkube:v2.0"))
		})
	})

	Describe("Template Data: Component Custom Variables", func() {
		It("should expose component custom variables", func() {
			nodeDir := filepath.Join(tempDir, k8sv1alpha1.ComponentOvnkubeNode)
			Expect(os.MkdirAll(nodeDir, 0755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(nodeDir, "test.yaml"), []byte(
				`firewalld: {{ .ovnkubeNode.customVariables.disableFirewalld }}`), 0644)).To(Succeed())

			cr := testCR()
			cr.Spec.ComponentOverrides = []k8sv1alpha1.ComponentOverride{
				{
					ResourceName: k8sv1alpha1.ComponentOvnkubeNode,
					CustomVariables: []k8sv1alpha1.CustomVariable{
						{Name: "disableFirewalld", Value: "true"},
					},
				},
			}

			manifests, err := tr.RenderAllTemplates(cr, renderer.RenderOptions{})
			Expect(err).ToNot(HaveOccurred())

			content := manifests[k8sv1alpha1.ComponentOvnkubeNode][0].Content
			Expect(content).To(ContainSubstring("firewalld: true"))
		})
	})

	Describe("Template Data: NodePlacement", func() {
		It("should render nodePlacement with toYaml and indent", func() {
			cpDir := filepath.Join(tempDir, k8sv1alpha1.ComponentOvnkubeControlPlane)
			Expect(os.MkdirAll(cpDir, 0755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(cpDir, "test.yaml"), []byte(
				`spec:
  template:
    spec:
      {{- if .ovnkubeControlPlane.nodePlacement }}
      {{- if .ovnkubeControlPlane.nodePlacement.tolerations }}
      tolerations:
{{ toYaml .ovnkubeControlPlane.nodePlacement.tolerations | indent 6 }}
      {{- end }}
      {{- end }}`), 0644)).To(Succeed())

			cr := testCR()
			cr.Spec.ComponentOverrides = []k8sv1alpha1.ComponentOverride{
				{
					ResourceName: k8sv1alpha1.ComponentOvnkubeControlPlane,
					NodePlacement: &k8sv1alpha1.NodePlacement{
						Tolerations: []corev1.Toleration{
							{Operator: corev1.TolerationOpExists},
						},
					},
				},
			}

			manifests, err := tr.RenderAllTemplates(cr, renderer.RenderOptions{})
			Expect(err).ToNot(HaveOccurred())

			content := manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][0].Content
			Expect(content).To(ContainSubstring("tolerations:"))
			Expect(content).To(ContainSubstring("operator: Exists"))
		})

		It("should render without nodePlacement when not set", func() {
			cpDir := filepath.Join(tempDir, k8sv1alpha1.ComponentOvnkubeControlPlane)
			Expect(os.MkdirAll(cpDir, 0755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(cpDir, "test.yaml"), []byte(
				`{{- if .ovnkubeControlPlane.nodePlacement }}
has-placement: true
{{- else }}
has-placement: false
{{- end }}`), 0644)).To(Succeed())

			cr := testCR() // no componentOverrides
			manifests, err := tr.RenderAllTemplates(cr, renderer.RenderOptions{})
			Expect(err).ToNot(HaveOccurred())

			content := manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][0].Content
			Expect(content).To(ContainSubstring("has-placement: false"))
		})
	})

	Describe("RenderTemplateContent", func() {
		It("should render inline template with CR data", func() {
			cr := testCR()
			result, err := tr.RenderTemplateContent(
				`name: {{ .OVNK8sOperator.Name }}
ns: {{ .global.namespace }}`,
				cr,
			)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(ContainSubstring("name: test-operator"))
			Expect(result).To(ContainSubstring("ns: ovn-kubernetes"))
		})

		It("should handle template functions", func() {
			cr := testCR()
			result, err := tr.RenderTemplateContent(
				`encoded: {{ "hello" | b64enc }}`,
				cr,
			)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(ContainSubstring("encoded: aGVsbG8="))
		})
	})
})
