// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package deployer_test

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/deployer"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/kinds"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/renderer"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	deploymentManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-deployment
  namespace: default
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
        image: nginx`

	configMapManifest = `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-config
  namespace: default
data:
  key: value`
)

var _ = Describe("ResourceDeployer", func() {
	var (
		resourceDeployer *deployer.ResourceDeployer
		fakeClient       client.Client
		scheme           *runtime.Scheme
		ctx              context.Context
		ovnk8sOperator   *k8sv1alpha1.OVNK8sOperator
	)

	BeforeEach(func() {
		scheme = runtime.NewScheme()
		Expect(k8sv1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		Expect(corev1.AddToScheme(scheme)).To(Succeed())

		fakeClient = fake.NewClientBuilder().WithScheme(scheme).Build()
		resourceDeployer = deployer.NewResourceDeployer(fakeClient, scheme)
		ctx = context.Background()

		ovnk8sOperator = &k8sv1alpha1.OVNK8sOperator{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testOperatorName,
				Namespace: testNamespace,
			},
		}
	})

	Describe("NewResourceDeployer", func() {
		It("should create a new resource deployer with provided client and scheme", func() {
			deployer := deployer.NewResourceDeployer(fakeClient, scheme)

			Expect(deployer).ToNot(BeNil())
			Expect(deployer.Client).To(Equal(fakeClient))
			Expect(deployer.Scheme).To(Equal(scheme))
		})

		It("should work with nil client", func() {
			deployer := deployer.NewResourceDeployer(nil, scheme)

			Expect(deployer).ToNot(BeNil())
			Expect(deployer.Client).To(BeNil())
			Expect(deployer.Scheme).To(Equal(scheme))
		})

		It("should work with nil scheme", func() {
			deployer := deployer.NewResourceDeployer(fakeClient, nil)

			Expect(deployer).ToNot(BeNil())
			Expect(deployer.Client).To(Equal(fakeClient))
			Expect(deployer.Scheme).To(BeNil())
		})
	})

	Describe("DeployComponentManifests", func() {
		Context("when manifests are empty", func() {
			It("should return empty workload resources without error", func() {
				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test-component", []*renderer.RenderedManifest{}, ovnk8sOperator)

				Expect(err).ToNot(HaveOccurred())
				Expect(workloads).To(BeEmpty())
			})

			It("should handle nil manifests", func() {
				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test-component", nil, ovnk8sOperator)

				Expect(err).ToNot(HaveOccurred())
				Expect(workloads).To(BeEmpty())
			})
		})

		Context("when manifests contain workload resources", func() {
			It("should deploy deployment and return workload resource", func() {
				manifests := []*renderer.RenderedManifest{
					{
						Content:      deploymentManifest,
						TemplatePath: deploymentTemplatePath,
						FileName:     deploymentFileName,
					},
				}

				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test-component", manifests, ovnk8sOperator)

				Expect(err).ToNot(HaveOccurred())
				Expect(workloads).To(HaveLen(1))
				Expect(workloads[0].Type).To(Equal(kinds.Deployment))
				Expect(workloads[0].Name).To(Equal("test-deployment"))
				Expect(workloads[0].Object).ToNot(BeNil())

				// Verify the resource was created in the fake client
				deployment := &appsv1.Deployment{}
				err = fakeClient.Get(ctx, client.ObjectKey{Name: "test-deployment", Namespace: metav1.NamespaceDefault}, deployment)
				Expect(err).ToNot(HaveOccurred())
				Expect(deployment.Name).To(Equal("test-deployment"))
			})

			It("should deploy multiple workload types and return all workload resources", func() {
				manifests := []*renderer.RenderedManifest{
					{
						Content:      deploymentManifest,
						TemplatePath: deploymentTemplatePath,
						FileName:     deploymentFileName,
					},
					{
						Content: `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: test-daemonset
  namespace: default
spec:
  selector:
    matchLabels:
      app: test-ds
  template:
    metadata:
      labels:
        app: test-ds
    spec:
      containers:
      - name: test
        image: nginx`,
						TemplatePath: "templates/daemonset.yaml",
						FileName:     "daemonset.yaml",
					},
				}

				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test-component", manifests, ovnk8sOperator)

				Expect(err).ToNot(HaveOccurred())
				Expect(workloads).To(HaveLen(2))

				// Check both workload resources are returned
				workloadTypes := make(map[string]string)
				for _, wl := range workloads {
					workloadTypes[wl.Type] = wl.Name
				}
				Expect(workloadTypes[kinds.Deployment]).To(Equal("test-deployment"))
				Expect(workloadTypes[kinds.DaemonSet]).To(Equal("test-daemonset"))
			})
		})

		Context("when manifests contain non-workload resources", func() {
			It("should deploy non-workload resources but not return them as workloads", func() {
				manifests := []*renderer.RenderedManifest{
					{
						Content:      configMapManifest,
						TemplatePath: configMapTemplatePath,
						FileName:     configMapFileName,
					},
					{
						Content: `apiVersion: v1
kind: Service
metadata:
  name: test-service
  namespace: default
spec:
  selector:
    app: test
  ports:
  - port: 80`,
						TemplatePath: "templates/service.yaml",
						FileName:     "service.yaml",
					},
				}

				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test-component", manifests, ovnk8sOperator)

				Expect(err).ToNot(HaveOccurred())
				Expect(workloads).To(BeEmpty()) // No workload resources

				// Verify the non-workload resources were created
				configMap := &corev1.ConfigMap{}
				err = fakeClient.Get(ctx, client.ObjectKey{Name: testConfigName, Namespace: metav1.NamespaceDefault}, configMap)
				Expect(err).ToNot(HaveOccurred())

				service := &corev1.Service{}
				err = fakeClient.Get(ctx, client.ObjectKey{Name: "test-service", Namespace: metav1.NamespaceDefault}, service)
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("when manifests contain NetworkAttachmentDefinition", func() {
			It("should deploy the NAD (custom kind, handled via unstructured)", func() {
				manifests := []*renderer.RenderedManifest{
					{
						Content: `apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: ovn-primary
  namespace: default
spec:
  config: '{"cniVersion":"0.4.0","name":"ovn-primary","type":"ovn-k8s-cni-overlay"}'`,
						TemplatePath: "templates/global/ovn-primary-nad.yaml",
						FileName:     "ovn-primary-nad.yaml",
					},
				}

				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "global", manifests, ovnk8sOperator)

				Expect(err).ToNot(HaveOccurred())
				Expect(workloads).To(BeEmpty())

				nad := &unstructured.Unstructured{}
				nad.SetGroupVersionKind(schema.GroupVersionKind{
					Group: nadGroup, Version: "v1", Kind: kinds.NetworkAttachmentDefinition,
				})
				Expect(fakeClient.Get(ctx, client.ObjectKey{Name: "ovn-primary", Namespace: metav1.NamespaceDefault}, nad)).To(Succeed())
				Expect(nad.GetName()).To(Equal("ovn-primary"))
			})
		})

		Context("when manifests contain RouteAdvertisements", func() {
			It("should deploy the cluster-scoped custom resource", func() {
				manifests := []*renderer.RenderedManifest{
					{
						Content: `apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: advertise-default-network
spec:
  networkSelectors:
  - networkSelectionType: DefaultNetwork
  nodeSelector: {}
  advertisements:
  - PodNetwork`,
						TemplatePath: "templates/global/ovnkube-setup.yaml",
						FileName:     "ovnkube-setup.yaml",
					},
				}

				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "global", manifests, ovnk8sOperator)

				Expect(err).ToNot(HaveOccurred())
				Expect(workloads).To(BeEmpty())

				routeAdvertisements := &unstructured.Unstructured{}
				routeAdvertisements.SetGroupVersionKind(schema.GroupVersionKind{
					Group: ovnGroup, Version: "v1", Kind: kinds.RouteAdvertisements,
				})
				Expect(fakeClient.Get(ctx, client.ObjectKey{Name: "advertise-default-network"}, routeAdvertisements)).To(Succeed())
				Expect(routeAdvertisements.GetLabels()).To(HaveKeyWithValue(
					"ovnk8s-operator.ovn.org/component", "global",
				))
			})
		})

		Context("when manifests contain mixed resources", func() {
			It("should deploy all resources and return only workload resources", func() {
				manifests := []*renderer.RenderedManifest{
					{
						Content:      configMapManifest,
						TemplatePath: configMapTemplatePath,
						FileName:     configMapFileName,
					},
					{
						Content:      deploymentManifest,
						TemplatePath: deploymentTemplatePath,
						FileName:     deploymentFileName,
					},
				}

				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test-component", manifests, ovnk8sOperator)

				Expect(err).ToNot(HaveOccurred())
				Expect(workloads).To(HaveLen(1)) // Only the deployment
				Expect(workloads[0].Type).To(Equal(kinds.Deployment))

				// Verify both resources were created
				configMap := &corev1.ConfigMap{}
				err = fakeClient.Get(ctx, client.ObjectKey{Name: testConfigName, Namespace: metav1.NamespaceDefault}, configMap)
				Expect(err).ToNot(HaveOccurred())

				deployment := &appsv1.Deployment{}
				err = fakeClient.Get(ctx, client.ObjectKey{Name: "test-deployment", Namespace: metav1.NamespaceDefault}, deployment)
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("when manifest parsing fails", func() {
			It("should return error for invalid YAML", func() {
				manifests := []*renderer.RenderedManifest{
					{
						Content:      "invalid: yaml: content: [unclosed",
						TemplatePath: "templates/invalid.yaml",
						FileName:     "invalid.yaml",
					},
				}

				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test-component", manifests, ovnk8sOperator)

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

				workloads, err := resourceDeployer.DeployComponentManifests(ctx, "test-component", manifests, ovnk8sOperator)

				Expect(err).To(HaveOccurred())
				Expect(workloads).To(BeNil())
				Expect(err.Error()).To(ContainSubstring("failed to classify manifests"))
			})
		})

		Context("when client operations fail", func() {
			// Note: For this test, we would need a mock client that can simulate failures
			// The fake client doesn't easily simulate Create/Update failures
			// But we can test the path where it would fail by using invalid objects
		})
	})

	Describe("Label management", func() {
		It("should add proper labels to deployed resources", func() {
			manifests := []*renderer.RenderedManifest{
				{
					Content:      configMapManifest,
					TemplatePath: configMapTemplatePath,
					FileName:     configMapFileName,
				},
			}

			_, err := resourceDeployer.DeployComponentManifests(ctx, "test-component", manifests, ovnk8sOperator)
			Expect(err).ToNot(HaveOccurred())

			// Verify labels were added
			configMap := &corev1.ConfigMap{}
			err = fakeClient.Get(ctx, client.ObjectKey{Name: testConfigName, Namespace: metav1.NamespaceDefault}, configMap)
			Expect(err).ToNot(HaveOccurred())

			labels := configMap.GetLabels()
			Expect(labels).To(HaveKey(deployer.LabelManagedBy))
			Expect(labels[deployer.LabelManagedBy]).To(Equal(deployer.ManagedBy))
			Expect(labels).To(HaveKey(deployer.LabelOwner))
			Expect(labels[deployer.LabelOwner]).To(Equal(testOperatorName))
			Expect(labels).To(HaveKey("ovnk8s-operator.ovn.org/component"))
			Expect(labels["ovnk8s-operator.ovn.org/component"]).To(Equal("test-component"))
		})

		It("should preserve existing labels while adding operator labels", func() {
			manifests := []*renderer.RenderedManifest{
				{
					Content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-config
  namespace: default
  labels:
    existing-label: existing-value
data:
  key: value`,
					TemplatePath: configMapTemplatePath,
					FileName:     configMapFileName,
				},
			}

			_, err := resourceDeployer.DeployComponentManifests(ctx, "test-component", manifests, ovnk8sOperator)
			Expect(err).ToNot(HaveOccurred())

			// Verify existing labels are preserved and new ones added
			configMap := &corev1.ConfigMap{}
			err = fakeClient.Get(ctx, client.ObjectKey{Name: testConfigName, Namespace: metav1.NamespaceDefault}, configMap)
			Expect(err).ToNot(HaveOccurred())

			labels := configMap.GetLabels()
			Expect(labels).To(HaveKey("existing-label"))
			Expect(labels["existing-label"]).To(Equal("existing-value"))
			Expect(labels).To(HaveKey(deployer.LabelManagedBy))
			Expect(labels[deployer.LabelManagedBy]).To(Equal(deployer.ManagedBy))
		})
	})

	Describe("Resource update behavior", func() {
		It("should update existing resources when deploying again", func() {
			// First deployment
			manifests := []*renderer.RenderedManifest{
				{
					Content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-config
  namespace: default
data:
  key: value1`,
					TemplatePath: configMapTemplatePath,
					FileName:     configMapFileName,
				},
			}

			_, err := resourceDeployer.DeployComponentManifests(ctx, "test-component", manifests, ovnk8sOperator)
			Expect(err).ToNot(HaveOccurred())

			// Verify initial value
			configMap := &corev1.ConfigMap{}
			err = fakeClient.Get(ctx, client.ObjectKey{Name: testConfigName, Namespace: metav1.NamespaceDefault}, configMap)
			Expect(err).ToNot(HaveOccurred())
			Expect(configMap.Data["key"]).To(Equal("value1"))

			// Second deployment with updated content
			manifestsUpdated := []*renderer.RenderedManifest{
				{
					Content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-config
  namespace: default
data:
  key: value2`,
					TemplatePath: configMapTemplatePath,
					FileName:     configMapFileName,
				},
			}

			_, err = resourceDeployer.DeployComponentManifests(ctx, "test-component", manifestsUpdated, ovnk8sOperator)
			Expect(err).ToNot(HaveOccurred())

			// Verify the resource was updated
			err = fakeClient.Get(ctx, client.ObjectKey{Name: testConfigName, Namespace: metav1.NamespaceDefault}, configMap)
			Expect(err).ToNot(HaveOccurred())
			Expect(configMap.Data["key"]).To(Equal("value2"))
		})

		It("should own the fields it applies and remove fields it no longer renders", func() {
			ownedClient := fake.NewClientBuilder().WithScheme(scheme).WithReturnManagedFields().Build()
			resourceDeployer = deployer.NewResourceDeployer(ownedClient, scheme)
			key := client.ObjectKey{Name: testConfigName, Namespace: metav1.NamespaceDefault}
			deploy := func(data string) {
				GinkgoHelper()
				manifests := []*renderer.RenderedManifest{{
					Content:      "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test-config\n  namespace: default\ndata:\n" + data,
					TemplatePath: configMapTemplatePath,
					FileName:     configMapFileName,
				}}
				_, err := resourceDeployer.DeployComponentManifests(ctx, "test-component", manifests, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
			}

			deploy("  key: value\n  extra: value\n")
			configMap := &corev1.ConfigMap{}
			Expect(ownedClient.Get(ctx, key, configMap)).To(Succeed())
			configMap.Data["key"] = "edited"
			Expect(ownedClient.Update(ctx, configMap, client.FieldOwner("someone-else"))).To(Succeed())

			deploy("  key: value\n")

			Expect(ownedClient.Get(ctx, key, configMap)).To(Succeed())
			Expect(configMap.Data).To(Equal(map[string]string{"key": "value"}))
			Expect(configMap.ManagedFields).To(ContainElement(SatisfyAll(
				HaveField("Manager", deployer.FieldManager),
				HaveField("Operation", metav1.ManagedFieldsOperationApply),
			)))
		})
	})
})

var _ = Describe("Helper Functions", func() {
	Describe("isWorkloadResource", func() {
		It("should return true for workload resource types", func() {
			// Note: We can't directly test the private function, but we can test its behavior
			// through the DeployComponentManifests function by checking which resources
			// are returned as workloads

			fakeClient := fake.NewClientBuilder().Build()
			deployer := deployer.NewResourceDeployer(fakeClient, runtime.NewScheme())
			ctx := context.Background()
			ovnk8sOperator := &k8sv1alpha1.OVNK8sOperator{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
			}

			// Test Deployment (should be workload)
			deploymentManifest := []*renderer.RenderedManifest{{
				Content: `apiVersion: apps/v1
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
			}}

			workloads, err := deployer.DeployComponentManifests(ctx, "test", deploymentManifest, ovnk8sOperator)
			Expect(err).ToNot(HaveOccurred())
			Expect(workloads).To(HaveLen(1)) // Deployment should be returned as workload

			// Test ConfigMap (should not be workload)
			configMapManifest := []*renderer.RenderedManifest{{
				Content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test
data:
  key: value`,
			}}

			workloads, err = deployer.DeployComponentManifests(ctx, "test", configMapManifest, ovnk8sOperator)
			Expect(err).ToNot(HaveOccurred())
			Expect(workloads).To(BeEmpty()) // ConfigMap should not be returned as workload
		})
	})
})
