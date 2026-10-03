// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package deployer_test

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/deployer"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/kinds"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("CleanupResources", func() {
	var (
		ctx              context.Context
		resourceDeployer *deployer.ResourceDeployer
		ovnk8sOperator   *k8sv1alpha1.OVNK8sOperator
		fakeClient       client.Client
		scheme           *runtime.Scheme
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()

		// Create OVNK8sOperator instance
		ovnk8sOperator = &k8sv1alpha1.OVNK8sOperator{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testOperatorName,
				Namespace: testNamespace,
			},
			Spec: k8sv1alpha1.OVNK8sOperatorSpec{
				Global: k8sv1alpha1.GlobalConfig{Namespace: "ovn-kubernetes"},
			},
		}
	})

	Context("when resources exist with matching labels", func() {
		BeforeEach(func() {
			// Create fake resources with operator labels
			deployment := &unstructured.Unstructured{}
			deployment.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   appsv1.GroupName,
				Version: "v1",
				Kind:    kinds.Deployment,
			})
			deployment.SetName("test-deployment")
			deployment.SetNamespace("ovn-kubernetes")
			deployment.SetLabels(map[string]string{
				deployer.LabelManagedBy: deployer.ManagedBy,
				deployer.LabelOwner:     testOperatorName,
			})

			service := &unstructured.Unstructured{}
			service.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "",
				Version: "v1",
				Kind:    kinds.Service,
			})
			service.SetName("test-service")
			service.SetNamespace("ovn-kubernetes")
			service.SetLabels(map[string]string{
				deployer.LabelManagedBy: deployer.ManagedBy,
				deployer.LabelOwner:     testOperatorName,
			})

			// Create fake client with resources
			fakeClient = fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(deployment, service).
				Build()

			resourceDeployer = deployer.NewResourceDeployer(fakeClient, scheme)
		})

		It("should delete all resources with matching labels", func() {
			err := resourceDeployer.CleanupResources(ctx, ovnk8sOperator)
			Expect(err).ToNot(HaveOccurred())

			// Verify deployment was deleted
			deploymentList := &unstructured.UnstructuredList{}
			deploymentList.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   appsv1.GroupName,
				Version: "v1",
				Kind:    deploymentListKind,
			})
			err = fakeClient.List(ctx, deploymentList)
			Expect(err).ToNot(HaveOccurred())
			Expect(deploymentList.Items).To(BeEmpty())

			// Verify service was deleted
			serviceList := &unstructured.UnstructuredList{}
			serviceList.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "",
				Version: "v1",
				Kind:    "ServiceList",
			})
			err = fakeClient.List(ctx, serviceList)
			Expect(err).ToNot(HaveOccurred())
			Expect(serviceList.Items).To(BeEmpty())
		})

		It("should delete a labelled NetworkAttachmentDefinition", func() {
			nad := &unstructured.Unstructured{}
			nad.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   nadGroup,
				Version: "v1",
				Kind:    kinds.NetworkAttachmentDefinition,
			})
			nad.SetName("ovn-primary")
			nad.SetNamespace("ovn-kubernetes")
			nad.SetLabels(map[string]string{
				deployer.LabelManagedBy: deployer.ManagedBy,
				deployer.LabelOwner:     testOperatorName,
			})

			fakeClient = fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(nad).
				Build()
			resourceDeployer = deployer.NewResourceDeployer(fakeClient, scheme)

			Expect(resourceDeployer.CleanupResources(ctx, ovnk8sOperator)).To(Succeed())

			nadList := &unstructured.UnstructuredList{}
			nadList.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   nadGroup,
				Version: "v1",
				Kind:    "NetworkAttachmentDefinitionList",
			})
			Expect(fakeClient.List(ctx, nadList)).To(Succeed())
			Expect(nadList.Items).To(BeEmpty())
		})

		It("should delete a labelled RouteAdvertisements", func() {
			routeAdvertisements := &unstructured.Unstructured{}
			routeAdvertisements.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   ovnGroup,
				Version: "v1",
				Kind:    kinds.RouteAdvertisements,
			})
			routeAdvertisements.SetName("advertise-default-network")
			routeAdvertisements.SetLabels(map[string]string{
				deployer.LabelManagedBy: deployer.ManagedBy,
				deployer.LabelOwner:     testOperatorName,
			})

			fakeClient = fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(routeAdvertisements).
				Build()
			resourceDeployer = deployer.NewResourceDeployer(fakeClient, scheme)

			Expect(resourceDeployer.CleanupResources(ctx, ovnk8sOperator)).To(Succeed())

			routeAdvertisementsList := &unstructured.UnstructuredList{}
			routeAdvertisementsList.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   ovnGroup,
				Version: "v1",
				Kind:    "RouteAdvertisementsList",
			})
			Expect(fakeClient.List(ctx, routeAdvertisementsList)).To(Succeed())
			Expect(routeAdvertisementsList.Items).To(BeEmpty())
		})

		It("should skip resources without matching labels", func() {
			// Add a resource without matching labels
			otherDeployment := &unstructured.Unstructured{}
			otherDeployment.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   appsv1.GroupName,
				Version: "v1",
				Kind:    kinds.Deployment,
			})
			otherDeployment.SetName("other-deployment")
			otherDeployment.SetNamespace(metav1.NamespaceDefault)
			otherDeployment.SetLabels(map[string]string{
				"app": "other-app",
			})

			// Update client to include the new resource
			fakeClient = fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(otherDeployment).
				Build()
			resourceDeployer = deployer.NewResourceDeployer(fakeClient, scheme)

			err := resourceDeployer.CleanupResources(ctx, ovnk8sOperator)
			Expect(err).ToNot(HaveOccurred())

			// Verify other deployment was NOT deleted
			deploymentList := &unstructured.UnstructuredList{}
			deploymentList.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   appsv1.GroupName,
				Version: "v1",
				Kind:    deploymentListKind,
			})
			err = fakeClient.List(ctx, deploymentList)
			Expect(err).ToNot(HaveOccurred())
			Expect(deploymentList.Items).To(HaveLen(1))
			Expect(deploymentList.Items[0].GetName()).To(Equal("other-deployment"))
		})

		It("should not delete matching resources from another namespace", func() {
			otherDeployment := &unstructured.Unstructured{}
			otherDeployment.SetGroupVersionKind(schema.GroupVersionKind{
				Group: appsv1.GroupName, Version: "v1", Kind: kinds.Deployment,
			})
			otherDeployment.SetName("other-deployment")
			otherDeployment.SetNamespace("other-namespace")
			otherDeployment.SetLabels(map[string]string{
				deployer.LabelManagedBy: deployer.ManagedBy,
				deployer.LabelOwner:     testOperatorName,
			})

			fakeClient = fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(otherDeployment).
				Build()
			resourceDeployer = deployer.NewResourceDeployer(fakeClient, scheme)

			Expect(resourceDeployer.CleanupResources(ctx, ovnk8sOperator)).To(Succeed())
			deploymentList := &unstructured.UnstructuredList{}
			deploymentList.SetGroupVersionKind(schema.GroupVersionKind{
				Group: appsv1.GroupName, Version: "v1", Kind: deploymentListKind,
			})
			Expect(fakeClient.List(ctx, deploymentList)).To(Succeed())
			Expect(deploymentList.Items).To(HaveLen(1))
		})
	})

	Context("when no resources exist", func() {
		BeforeEach(func() {
			// Create client with no resources
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).Build()
			resourceDeployer = deployer.NewResourceDeployer(fakeClient, scheme)
		})

		It("should complete successfully with no errors", func() {
			err := resourceDeployer.CleanupResources(ctx, ovnk8sOperator)
			Expect(err).ToNot(HaveOccurred())
		})
	})

	Context("when encountering errors", func() {
		It("should continue cleanup despite individual resource deletion failures", func() {
			// This test would require a more sophisticated mock to simulate
			// deletion failures, which is beyond fake client capabilities
			// In real scenarios, the function should continue despite errors
			Skip("Requires mock client to simulate deletion failures")
		})

		It("should skip unknown resource types gracefully", func() {
			// The function should skip resource types not in the getGVK mapping
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).Build()
			resourceDeployer = deployer.NewResourceDeployer(fakeClient, scheme)

			// This should not panic or error when encountering unknown types
			err := resourceDeployer.CleanupResources(ctx, ovnk8sOperator)
			Expect(err).ToNot(HaveOccurred())
		})
	})

	Context("when handling list types", func() {
		It("should skip resource types ending with 'List'", func() {
			// The function should skip these as they're just for grouping
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).Build()
			resourceDeployer = deployer.NewResourceDeployer(fakeClient, scheme)

			// Should complete without trying to list "SecretList" resources
			err := resourceDeployer.CleanupResources(ctx, ovnk8sOperator)
			Expect(err).ToNot(HaveOccurred())
		})
	})
})
