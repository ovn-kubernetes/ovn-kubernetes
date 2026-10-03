// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
)

var _ = Describe("OVNK8sOperator Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = k8sv1alpha1.SingletonName
			resourceNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		ovnk8soperator := &k8sv1alpha1.OVNK8sOperator{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind OVNK8sOperator")
			err := k8sClient.Get(ctx, typeNamespacedName, ovnk8soperator)
			if err != nil && errors.IsNotFound(err) {
				resource := &k8sv1alpha1.OVNK8sOperator{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: k8sv1alpha1.OVNK8sOperatorSpec{
						Global: k8sv1alpha1.GlobalConfig{
							K8sAPIServer:   "https://172.25.0.2:6443",
							PodNetwork:     []k8sv1alpha1.PodNetworkEntry{{CIDR: "10.244.0.0/16", HostPrefix: 24}},
							ServiceNetwork: []string{"10.96.0.0/16"},
							MTU:            1400,
							DeploymentMode: k8sv1alpha1.DeploymentModeStandard,
							Namespace:      "ovn-kubernetes",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &k8sv1alpha1.OVNK8sOperator{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance OVNK8sOperator")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &OVNK8sOperatorReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})
})
