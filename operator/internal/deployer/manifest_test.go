// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package deployer_test

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/deployer"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/renderer"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Manifest Classification", func() {
	var (
		resourceDeployer *deployer.ResourceDeployer
		ctx              context.Context
		ovnk8sOperator   *k8sv1alpha1.OVNK8sOperator
	)

	BeforeEach(func() {
		scheme := runtime.NewScheme()
		Expect(k8sv1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		Expect(corev1.AddToScheme(scheme)).To(Succeed())

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		resourceDeployer = deployer.NewResourceDeployer(fakeClient, scheme)
		ctx = context.Background()

		ovnk8sOperator = &k8sv1alpha1.OVNK8sOperator{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-operator",
				Namespace: "test-namespace",
			},
		}
	})

	Describe("classifyManifests", func() {
		Context("when manifests are valid", func() {
			It("should classify single manifest correctly", func() {
				manifests := []*renderer.RenderedManifest{
					{
						Content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-config
data:
  key: value`,
						TemplatePath: "templates/configmap.yaml",
						FileName:     "configmap.yaml",
					},
				}

				// We can't directly test the private classifyManifests method,
				// but we can test its behavior through DeployComponentManifests
				// by verifying it correctly processes different resource types
				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test", manifests, ovnk8sOperator)

				Expect(err).ToNot(HaveOccurred())
				Expect(workloads).To(BeEmpty()) // ConfigMap is not a workload
			})

			It("should classify multiple different resource types", func() {
				manifests := []*renderer.RenderedManifest{
					{
						Content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-config
data:
  key: value`,
						TemplatePath: "templates/configmap.yaml",
						FileName:     "configmap.yaml",
					},
					{
						Content: `apiVersion: v1
kind: Service
metadata:
  name: test-service
spec:
  selector:
    app: test
  ports:
  - port: 80`,
						TemplatePath: "templates/service.yaml",
						FileName:     "service.yaml",
					},
					{
						Content: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-deployment
spec:
  replicas: 1
  selector:
    matchLabels:
      app: test
  template:
    metadata:
      labels:
        app: test
    spec:
      containers:
      - name: test
        image: nginx`,
						TemplatePath: "templates/deployment.yaml",
						FileName:     "deployment.yaml",
					},
				}

				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test", manifests, ovnk8sOperator)

				Expect(err).ToNot(HaveOccurred())
				Expect(workloads).To(HaveLen(1)) // Only Deployment is a workload
				Expect(workloads[0].Type).To(Equal("Deployment"))
			})

			It("should classify multiple manifests of the same type", func() {
				manifests := []*renderer.RenderedManifest{
					{
						Content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-config-1
data:
  key: value1`,
						TemplatePath: "templates/configmap1.yaml",
						FileName:     "configmap1.yaml",
					},
					{
						Content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-config-2
data:
  key: value2`,
						TemplatePath: "templates/configmap2.yaml",
						FileName:     "configmap2.yaml",
					},
				}

				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test", manifests, ovnk8sOperator)

				Expect(err).ToNot(HaveOccurred())
				Expect(workloads).To(BeEmpty()) // ConfigMaps are not workloads
			})
		})

		Context("when manifests have parsing errors", func() {
			It("should return error for invalid YAML", func() {
				manifests := []*renderer.RenderedManifest{
					{
						Content:      "invalid: yaml: content: [unclosed",
						TemplatePath: "templates/invalid.yaml",
						FileName:     "invalid.yaml",
					},
				}

				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test", manifests, ovnk8sOperator)

				Expect(err).To(HaveOccurred())
				Expect(workloads).To(BeNil())
				Expect(err.Error()).To(ContainSubstring("failed to classify manifests"))
			})

			It("should return error for manifest without kind", func() {
				manifests := []*renderer.RenderedManifest{
					{
						Content: `apiVersion: v1
metadata:
  name: test-config`,
						TemplatePath: "templates/no-kind.yaml",
						FileName:     "no-kind.yaml",
					},
				}

				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test", manifests, ovnk8sOperator)

				Expect(err).To(HaveOccurred())
				Expect(workloads).To(BeNil())
				Expect(err.Error()).To(ContainSubstring("failed to classify manifests"))
			})

			It("should return error for empty manifest content", func() {
				manifests := []*renderer.RenderedManifest{
					{
						Content:      "",
						TemplatePath: "templates/empty.yaml",
						FileName:     "empty.yaml",
					},
				}

				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test", manifests, ovnk8sOperator)

				Expect(err).To(HaveOccurred())
				Expect(workloads).To(BeNil())
				Expect(err.Error()).To(ContainSubstring("failed to classify manifests"))
			})

			It("should return error for whitespace-only manifest content", func() {
				manifests := []*renderer.RenderedManifest{
					{
						Content:      "   \n\t  \n  ",
						TemplatePath: "templates/whitespace.yaml",
						FileName:     "whitespace.yaml",
					},
				}

				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test", manifests, ovnk8sOperator)

				Expect(err).To(HaveOccurred())
				Expect(workloads).To(BeNil())
				Expect(err.Error()).To(ContainSubstring("failed to classify manifests"))
			})
		})

		Context("when manifests have missing required fields", func() {
			It("should return error for manifest without apiVersion", func() {
				manifests := []*renderer.RenderedManifest{
					{
						Content: `kind: ConfigMap
metadata:
  name: test-config`,
						TemplatePath: "templates/no-apiversion.yaml",
						FileName:     "no-apiversion.yaml",
					},
				}

				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test", manifests, ovnk8sOperator)

				Expect(err).To(HaveOccurred())
				Expect(workloads).To(BeNil())
			})
		})
	})

	Describe("extractResourceType", func() {
		Context("when testing resource type extraction indirectly", func() {
			It("should correctly extract types for different Kubernetes resources", func() {
				testCases := []struct {
					name         string
					content      string
					expectedType string
					isWorkload   bool
				}{
					{
						name: "ConfigMap",
						content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test`,
						expectedType: "ConfigMap",
						isWorkload:   false,
					},
					{
						name: "Service",
						content: `apiVersion: v1
kind: Service
metadata:
  name: test`,
						expectedType: "Service",
						isWorkload:   false,
					},
					{
						name: "Deployment",
						content: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test
spec:
  replicas: 1
  selector:
    matchLabels:
      app: test
  template:
    metadata:
      labels:
        app: test
    spec:
      containers:
      - name: test
        image: nginx`,
						expectedType: "Deployment",
						isWorkload:   true,
					},
					{
						name: "DaemonSet",
						content: `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: test
spec:
  selector:
    matchLabels:
      app: test
  template:
    metadata:
      labels:
        app: test
    spec:
      containers:
      - name: test
        image: nginx`,
						expectedType: "DaemonSet",
						isWorkload:   true,
					},
					{
						name: "StatefulSet",
						content: `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: test
spec:
  replicas: 1
  selector:
    matchLabels:
      app: test
  template:
    metadata:
      labels:
        app: test
    spec:
      containers:
      - name: test
        image: nginx`,
						expectedType: "StatefulSet",
						isWorkload:   true,
					},
					{
						name: "Secret",
						content: `apiVersion: v1
kind: Secret
metadata:
  name: test
data:
  key: dmFsdWU=`,
						expectedType: "Secret",
						isWorkload:   false,
					},
					{
						name: "ServiceAccount",
						content: `apiVersion: v1
kind: ServiceAccount
metadata:
  name: test`,
						expectedType: "ServiceAccount",
						isWorkload:   false,
					},
					{
						name: "ClusterRole",
						content: `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: test
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list"]`,
						expectedType: "ClusterRole",
						isWorkload:   false,
					},
				}

				for _, tc := range testCases {
					By(fmt.Sprintf("Testing %s resource type extraction", tc.name), func() {
						manifests := []*renderer.RenderedManifest{
							{
								Content:      tc.content,
								TemplatePath: fmt.Sprintf("templates/%s.yaml", tc.name),
								FileName:     fmt.Sprintf("%s.yaml", tc.name),
							},
						}

						workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test", manifests, ovnk8sOperator)

						Expect(err).ToNot(HaveOccurred())

						if tc.isWorkload {
							Expect(workloads).To(HaveLen(1))
							Expect(workloads[0].Type).To(Equal(tc.expectedType))
						} else {
							Expect(workloads).To(BeEmpty())
						}
					})
				}
			})

			It("should handle resources with different API versions", func() {
				manifests := []*renderer.RenderedManifest{
					{
						Content: `apiVersion: extensions/v1beta1
kind: Deployment
metadata:
  name: test-legacy-deployment
spec:
  replicas: 1
  selector:
    matchLabels:
      app: test
  template:
    metadata:
      labels:
        app: test
    spec:
      containers:
      - name: test
        image: nginx`,
						TemplatePath: "templates/legacy-deployment.yaml",
						FileName:     "legacy-deployment.yaml",
					},
				}

				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test", manifests, ovnk8sOperator)

				Expect(err).ToNot(HaveOccurred())
				Expect(workloads).To(HaveLen(1))
				Expect(workloads[0].Type).To(Equal("Deployment"))
			})

			It("should fail fast on a kind missing from ResourceOrder", func() {
				// A kind absent from ResourceOrder is never visited by the
				// ordered deploy loop, so it must be rejected up front rather
				// than silently dropped.
				manifests := []*renderer.RenderedManifest{
					{
						Content: `apiVersion: custom.io/v1
kind: CustomResource
metadata:
  name: test-custom
spec:
  customField: value`,
						TemplatePath: "templates/custom.yaml",
						FileName:     "custom.yaml",
					},
				}

				_, err := resourceDeployer.DeployComponentManifests(ctx, "test", manifests, ovnk8sOperator)

				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("ResourceOrder"))
			})
		})
	})

	Describe("Edge cases", func() {
		It("should handle manifests with comments", func() {
			manifests := []*renderer.RenderedManifest{
				{
					Content: `# This is a comment
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-config # Another comment
data:
  key: value`,
					TemplatePath: "templates/with-comments.yaml",
					FileName:     "with-comments.yaml",
				},
			}

			workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test", manifests, ovnk8sOperator)

			Expect(err).ToNot(HaveOccurred())
			Expect(workloads).To(BeEmpty()) // ConfigMap is not a workload
		})

		It("should handle manifests with extra whitespace", func() {
			manifests := []*renderer.RenderedManifest{
				{
					Content: `


apiVersion: v1
kind: ConfigMap


metadata:
  name: test-config

data:
  key: value


`,
					TemplatePath: "templates/extra-whitespace.yaml",
					FileName:     "extra-whitespace.yaml",
				},
			}

			workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test", manifests, ovnk8sOperator)

			Expect(err).ToNot(HaveOccurred())
			Expect(workloads).To(BeEmpty()) // ConfigMap is not a workload
		})

		It("should handle manifests with special characters in names", func() {
			manifests := []*renderer.RenderedManifest{
				{
					Content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-config-with-dashes_and_underscores.and.dots
data:
  key: value`,
					TemplatePath: "templates/special-chars.yaml",
					FileName:     "special-chars.yaml",
				},
			}

			workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test", manifests, ovnk8sOperator)

			Expect(err).ToNot(HaveOccurred())
			Expect(workloads).To(BeEmpty()) // ConfigMap is not a workload
		})
	})
})
