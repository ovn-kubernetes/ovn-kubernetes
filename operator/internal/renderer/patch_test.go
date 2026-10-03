// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package renderer_test

import (
	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/renderer"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const controlPlaneManifestFile = "deployment.yaml[0]"

var _ = Describe("ApplyPatches", func() {
	var baseManifest string

	BeforeEach(func() {
		baseManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: ovnkube-control-plane
  namespace: ovn-kubernetes
spec:
  replicas: 3
  template:
    spec:
      containers:
      - name: ovnkube-cluster-manager
        resources:
          requests:
            cpu: 100m
            memory: 300Mi`
	})

	Describe("JSON Patch (RFC 6902)", func() {
		It("should replace a value", func() {
			manifests := renderer.ComponentManifests{
				k8sv1alpha1.ComponentOvnkubeControlPlane: []*renderer.RenderedManifest{
					{Content: baseManifest, FileName: controlPlaneManifestFile},
				},
			}

			overrides := []k8sv1alpha1.ComponentOverride{
				{
					ResourceName: k8sv1alpha1.ComponentOvnkubeControlPlane,
					Patches: []k8sv1alpha1.ComponentPatch{
						{
							Type: k8sv1alpha1.JSONPatchType,
							Patch: `[
								{"op": "replace", "path": "/spec/replicas", "value": 5}
							]`,
						},
					},
				},
			}

			err := renderer.ApplyPatches(manifests, overrides)
			Expect(err).ToNot(HaveOccurred())
			Expect(manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][0].Content).To(ContainSubstring("replicas: 5"))
		})

		It("should add a field", func() {
			manifests := renderer.ComponentManifests{
				k8sv1alpha1.ComponentOvnkubeControlPlane: []*renderer.RenderedManifest{
					{Content: baseManifest, FileName: controlPlaneManifestFile},
				},
			}

			overrides := []k8sv1alpha1.ComponentOverride{
				{
					ResourceName: k8sv1alpha1.ComponentOvnkubeControlPlane,
					Patches: []k8sv1alpha1.ComponentPatch{
						{
							Type: k8sv1alpha1.JSONPatchType,
							Patch: `[
								{"op": "add", "path": "/metadata/labels", "value": {"custom": "label"}}
							]`,
						},
					},
				},
			}

			err := renderer.ApplyPatches(manifests, overrides)
			Expect(err).ToNot(HaveOccurred())
			Expect(manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][0].Content).To(ContainSubstring("custom: label"))
		})

		It("should replace container resources", func() {
			manifests := renderer.ComponentManifests{
				k8sv1alpha1.ComponentOvnkubeControlPlane: []*renderer.RenderedManifest{
					{Content: baseManifest, FileName: controlPlaneManifestFile},
				},
			}

			overrides := []k8sv1alpha1.ComponentOverride{
				{
					ResourceName: k8sv1alpha1.ComponentOvnkubeControlPlane,
					Patches: []k8sv1alpha1.ComponentPatch{
						{
							Type: k8sv1alpha1.JSONPatchType,
							Patch: `[
								{"op": "replace", "path": "/spec/template/spec/containers/0/resources/requests/cpu", "value": "200m"},
								{"op": "replace", "path": "/spec/template/spec/containers/0/resources/requests/memory", "value": "512Mi"},
								{"op": "add", "path": "/spec/template/spec/containers/0/resources/limits", "value": {"cpu": "1", "memory": "1Gi"}}
							]`,
						},
					},
				},
			}

			err := renderer.ApplyPatches(manifests, overrides)
			Expect(err).ToNot(HaveOccurred())
			content := manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][0].Content
			Expect(content).To(ContainSubstring("cpu: 200m"))
			Expect(content).To(ContainSubstring("memory: 512Mi"))
			Expect(content).To(ContainSubstring("memory: 1Gi"))
		})

		It("should return error for invalid JSON patch", func() {
			manifests := renderer.ComponentManifests{
				k8sv1alpha1.ComponentOvnkubeControlPlane: []*renderer.RenderedManifest{
					{Content: baseManifest, FileName: controlPlaneManifestFile},
				},
			}

			overrides := []k8sv1alpha1.ComponentOverride{
				{
					ResourceName: k8sv1alpha1.ComponentOvnkubeControlPlane,
					Patches: []k8sv1alpha1.ComponentPatch{
						{Type: k8sv1alpha1.JSONPatchType, Patch: `not valid json`},
					},
				},
			}

			err := renderer.ApplyPatches(manifests, overrides)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid JSON patch"))
		})
	})

	Describe("Merge Patch (RFC 7386)", func() {
		It("should merge fields", func() {
			manifests := renderer.ComponentManifests{
				k8sv1alpha1.ComponentOvnkubeControlPlane: []*renderer.RenderedManifest{
					{Content: baseManifest, FileName: controlPlaneManifestFile},
				},
			}

			overrides := []k8sv1alpha1.ComponentOverride{
				{
					ResourceName: k8sv1alpha1.ComponentOvnkubeControlPlane,
					Patches: []k8sv1alpha1.ComponentPatch{
						{
							Type:  k8sv1alpha1.MergePatchType,
							Patch: `{"spec": {"replicas": 7}}`,
						},
					},
				},
			}

			err := renderer.ApplyPatches(manifests, overrides)
			Expect(err).ToNot(HaveOccurred())
			Expect(manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][0].Content).To(ContainSubstring("replicas: 7"))
		})

		It("should accept YAML-formatted patch", func() {
			manifests := renderer.ComponentManifests{
				k8sv1alpha1.ComponentOvnkubeControlPlane: []*renderer.RenderedManifest{
					{Content: baseManifest, FileName: controlPlaneManifestFile},
				},
			}

			overrides := []k8sv1alpha1.ComponentOverride{
				{
					ResourceName: k8sv1alpha1.ComponentOvnkubeControlPlane,
					Patches: []k8sv1alpha1.ComponentPatch{
						{
							Type: k8sv1alpha1.MergePatchType,
							Patch: `
spec:
  replicas: 10
metadata:
  annotations:
    custom: "annotation"`,
						},
					},
				},
			}

			err := renderer.ApplyPatches(manifests, overrides)
			Expect(err).ToNot(HaveOccurred())
			content := manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][0].Content
			Expect(content).To(ContainSubstring("replicas: 10"))
			Expect(content).To(ContainSubstring("custom: annotation"))
		})
	})

	Describe("Default patch type", func() {
		It("should default to JSON patch when type is empty", func() {
			manifests := renderer.ComponentManifests{
				k8sv1alpha1.ComponentOvnkubeControlPlane: []*renderer.RenderedManifest{
					{Content: baseManifest, FileName: controlPlaneManifestFile},
				},
			}

			overrides := []k8sv1alpha1.ComponentOverride{
				{
					ResourceName: k8sv1alpha1.ComponentOvnkubeControlPlane,
					Patches: []k8sv1alpha1.ComponentPatch{
						{
							// Type omitted — should default to JSON patch
							Patch: `[{"op": "replace", "path": "/spec/replicas", "value": 9}]`,
						},
					},
				},
			}

			err := renderer.ApplyPatches(manifests, overrides)
			Expect(err).ToNot(HaveOccurred())
			Expect(manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][0].Content).To(ContainSubstring("replicas: 9"))
		})
	})

	Describe("Multiple patches", func() {
		It("should apply patches in order", func() {
			manifests := renderer.ComponentManifests{
				k8sv1alpha1.ComponentOvnkubeControlPlane: []*renderer.RenderedManifest{
					{Content: baseManifest, FileName: controlPlaneManifestFile},
				},
			}

			overrides := []k8sv1alpha1.ComponentOverride{
				{
					ResourceName: k8sv1alpha1.ComponentOvnkubeControlPlane,
					Patches: []k8sv1alpha1.ComponentPatch{
						{
							Type:  k8sv1alpha1.MergePatchType,
							Patch: `{"spec": {"replicas": 5}}`,
						},
						{
							Type: k8sv1alpha1.JSONPatchType,
							Patch: `[
								{"op": "add", "path": "/metadata/labels", "value": {"env": "prod"}}
							]`,
						},
					},
				},
			}

			err := renderer.ApplyPatches(manifests, overrides)
			Expect(err).ToNot(HaveOccurred())
			content := manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][0].Content
			Expect(content).To(ContainSubstring("replicas: 5"))
			Expect(content).To(ContainSubstring("env: prod"))
		})
	})

	Describe("Edge cases", func() {
		It("should skip components with no patches", func() {
			manifests := renderer.ComponentManifests{
				k8sv1alpha1.ComponentOvnkubeControlPlane: []*renderer.RenderedManifest{
					{Content: baseManifest, FileName: controlPlaneManifestFile},
				},
			}

			overrides := []k8sv1alpha1.ComponentOverride{
				{ResourceName: k8sv1alpha1.ComponentOvnkubeControlPlane},
			}

			err := renderer.ApplyPatches(manifests, overrides)
			Expect(err).ToNot(HaveOccurred())
			// Content should be unchanged
			Expect(manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][0].Content).To(Equal(baseManifest))
		})

		It("should skip non-existent components", func() {
			manifests := renderer.ComponentManifests{
				k8sv1alpha1.ComponentOvnkubeControlPlane: []*renderer.RenderedManifest{
					{Content: baseManifest, FileName: controlPlaneManifestFile},
				},
			}

			overrides := []k8sv1alpha1.ComponentOverride{
				{
					ResourceName: "nonexistent",
					Patches: []k8sv1alpha1.ComponentPatch{
						{Type: k8sv1alpha1.MergePatchType, Patch: `{"spec": {"replicas": 1}}`},
					},
				},
			}

			err := renderer.ApplyPatches(manifests, overrides)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should handle empty overrides", func() {
			manifests := renderer.ComponentManifests{
				k8sv1alpha1.ComponentOvnkubeControlPlane: []*renderer.RenderedManifest{
					{Content: baseManifest, FileName: controlPlaneManifestFile},
				},
			}

			err := renderer.ApplyPatches(manifests, nil)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should only patch workload resources, not Services", func() {
			serviceManifest := `apiVersion: v1
kind: Service
metadata:
  name: ovnkube-control-plane-prometheus-discovery
  namespace: ovn-kubernetes
spec:
  selector:
    name: ovnkube-control-plane
  ports:
  - port: 9411`

			manifests := renderer.ComponentManifests{
				k8sv1alpha1.ComponentOvnkubeControlPlane: []*renderer.RenderedManifest{
					{Content: baseManifest, FileName: controlPlaneManifestFile},
					{Content: serviceManifest, FileName: "deployment.yaml[1]"},
				},
			}

			overrides := []k8sv1alpha1.ComponentOverride{
				{
					ResourceName: k8sv1alpha1.ComponentOvnkubeControlPlane,
					Patches: []k8sv1alpha1.ComponentPatch{
						{
							Type:  k8sv1alpha1.MergePatchType,
							Patch: `{"spec": {"replicas": 5}}`,
						},
					},
				},
			}

			err := renderer.ApplyPatches(manifests, overrides)
			Expect(err).ToNot(HaveOccurred())

			// Deployment should be patched
			Expect(manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][0].Content).To(ContainSubstring("replicas: 5"))

			// Service should be untouched
			Expect(manifests[k8sv1alpha1.ComponentOvnkubeControlPlane][1].Content).To(Equal(serviceManifest))
		})

		It("should patch DaemonSet but not ConfigMap", func() {
			dsManifest := `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: ovnkube-node
spec:
  template:
    spec:
      containers:
      - name: ovnkube-controller
        resources:
          requests:
            cpu: 100m`

			cmManifest := `apiVersion: v1
kind: ConfigMap
metadata:
  name: ovn-config
data:
  key: value`

			manifests := renderer.ComponentManifests{
				"ovnkubeNode": []*renderer.RenderedManifest{
					{Content: cmManifest, FileName: "config.yaml[0]"},
					{Content: dsManifest, FileName: "daemonset.yaml[0]"},
				},
			}

			overrides := []k8sv1alpha1.ComponentOverride{
				{
					ResourceName: "ovnkubeNode",
					Patches: []k8sv1alpha1.ComponentPatch{
						{
							Type: k8sv1alpha1.JSONPatchType,
							Patch: `[
								{"op": "replace", "path": "/spec/template/spec/containers/0/resources/requests/cpu", "value": "500m"}
							]`,
						},
					},
				},
			}

			err := renderer.ApplyPatches(manifests, overrides)
			Expect(err).ToNot(HaveOccurred())

			// ConfigMap untouched
			Expect(manifests["ovnkubeNode"][0].Content).To(Equal(cmManifest))

			// DaemonSet patched
			Expect(manifests["ovnkubeNode"][1].Content).To(ContainSubstring("cpu: 500m"))
		})
	})
})
