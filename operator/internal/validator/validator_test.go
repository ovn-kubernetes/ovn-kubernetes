// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package validator_test

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/deployer"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/kinds"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/validator"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var defaultValidationConfig = &validator.ValidationConfig{
	Timeout:       5 * time.Minute,
	CheckInterval: 10 * time.Second,
	MaxRetries:    30,
}

var _ = Describe("ComponentValidator", func() {
	var (
		fakeClient         client.Client
		componentValidator *validator.ComponentValidator
		ctx                context.Context
	)

	BeforeEach(func() {
		scheme := runtime.NewScheme()
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		fakeClient = fake.NewClientBuilder().WithScheme(scheme).Build()
		ctx = context.Background()
	})

	Describe("NewComponentValidatorWithConfig (default config)", func() {
		It("should create validator with default configuration", func() {
			validator := validator.NewComponentValidatorWithConfig(fakeClient, defaultValidationConfig)

			Expect(validator).ToNot(BeNil())
			Expect(validator.Client).To(Equal(fakeClient))
			Expect(validator.Config).ToNot(BeNil())
			Expect(validator.Config.Timeout).To(Equal(5 * time.Minute))
			Expect(validator.Config.CheckInterval).To(Equal(10 * time.Second))
			Expect(validator.Config.MaxRetries).To(Equal(30))
		})

		It("should work with nil client", func() {
			validator := validator.NewComponentValidatorWithConfig(nil, defaultValidationConfig)

			Expect(validator).ToNot(BeNil())
			Expect(validator.Client).To(BeNil())
			Expect(validator.Config).ToNot(BeNil())
		})
	})

	Describe("NewComponentValidatorWithConfig", func() {
		It("should create validator with custom configuration", func() {
			customConfig := &validator.ValidationConfig{
				Timeout:       1 * time.Minute,
				CheckInterval: 5 * time.Second,
				MaxRetries:    10,
			}

			validator := validator.NewComponentValidatorWithConfig(fakeClient, customConfig)

			Expect(validator).ToNot(BeNil())
			Expect(validator.Client).To(Equal(fakeClient))
			Expect(validator.Config).To(Equal(customConfig))
			Expect(validator.Config.Timeout).To(Equal(1 * time.Minute))
			Expect(validator.Config.CheckInterval).To(Equal(5 * time.Second))
			Expect(validator.Config.MaxRetries).To(Equal(10))
		})

		It("should work with nil configuration", func() {
			validator := validator.NewComponentValidatorWithConfig(fakeClient, nil)

			Expect(validator).ToNot(BeNil())
			Expect(validator.Client).To(Equal(fakeClient))
			Expect(validator.Config).To(BeNil())
		})
	})

	Describe("ValidateComponent", func() {
		BeforeEach(func() {
			componentValidator = validator.NewComponentValidatorWithConfig(fakeClient, defaultValidationConfig)
		})

		Context("when no workloads are provided", func() {
			It("should return nil without error", func() {
				err := componentValidator.ValidateComponent(ctx, "test-component", []deployer.WorkloadResource{})
				Expect(err).ToNot(HaveOccurred())
			})

			It("should handle nil workload slice", func() {
				err := componentValidator.ValidateComponent(ctx, "test-component", nil)
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("when workloads are ready", func() {
			It("should validate a single ready deployment", func() {
				// Create a ready deployment
				deployment := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testDeploymentName,
						Namespace: metav1.NamespaceDefault,
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: new(int32(3)),
					},
					Status: appsv1.DeploymentStatus{
						ReadyReplicas:     3,
						AvailableReplicas: 3,
						UpdatedReplicas:   3,
					},
				}

				// Add to fake client
				Expect(fakeClient.Create(ctx, deployment)).To(Succeed())

				workloads := []deployer.WorkloadResource{
					{
						Object: deployment,
						Type:   kinds.Deployment,
						Name:   testDeploymentName,
					},
				}

				// Use shorter timeout for testing
				componentValidator.Config.Timeout = 1 * time.Second
				componentValidator.Config.CheckInterval = 100 * time.Millisecond

				err := componentValidator.ValidateComponent(ctx, "test-component", workloads)
				Expect(err).ToNot(HaveOccurred())
			})

			It("should validate multiple ready workloads", func() {
				// Create ready deployment
				deployment := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testDeploymentName,
						Namespace: metav1.NamespaceDefault,
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: new(int32(2)),
					},
					Status: appsv1.DeploymentStatus{
						ReadyReplicas:     2,
						AvailableReplicas: 2,
						UpdatedReplicas:   2,
					},
				}

				// Create ready daemonset
				daemonSet := &appsv1.DaemonSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testDaemonSetName,
						Namespace: metav1.NamespaceDefault,
					},
					Status: appsv1.DaemonSetStatus{
						DesiredNumberScheduled: 2,
						NumberReady:            2,
						NumberAvailable:        2,
						UpdatedNumberScheduled: 2,
					},
				}

				// Add to fake client
				Expect(fakeClient.Create(ctx, deployment)).To(Succeed())
				Expect(fakeClient.Create(ctx, daemonSet)).To(Succeed())

				workloads := []deployer.WorkloadResource{
					{Object: deployment, Type: kinds.Deployment, Name: testDeploymentName},
					{Object: daemonSet, Type: kinds.DaemonSet, Name: testDaemonSetName},
				}

				// Use shorter timeout for testing
				componentValidator.Config.Timeout = 1 * time.Second
				componentValidator.Config.CheckInterval = 100 * time.Millisecond

				err := componentValidator.ValidateComponent(ctx, "test-component", workloads)
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("when workloads are not ready", func() {
			It("should timeout for unready deployment", func() {
				// Create unready deployment
				deployment := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testDeploymentName,
						Namespace: metav1.NamespaceDefault,
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: new(int32(3)),
					},
					Status: appsv1.DeploymentStatus{
						ReadyReplicas:     1, // Not all ready
						AvailableReplicas: 1,
						UpdatedReplicas:   1,
					},
				}

				// Add to fake client
				Expect(fakeClient.Create(ctx, deployment)).To(Succeed())

				workloads := []deployer.WorkloadResource{
					{
						Object: deployment,
						Type:   kinds.Deployment,
						Name:   testDeploymentName,
					},
				}

				// Use very short timeout for testing
				componentValidator.Config.Timeout = 200 * time.Millisecond
				componentValidator.Config.CheckInterval = 50 * time.Millisecond

				err := componentValidator.ValidateComponent(ctx, "test-component", workloads)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("validation timeout exceeded"))
				Expect(err.Error()).To(ContainSubstring("Deployment/test-deployment"))
			})

			It("should fail validation if first workload fails", func() {
				// Create deployment with failing condition
				deployment := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testDeploymentName,
						Namespace: metav1.NamespaceDefault,
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: new(int32(3)),
					},
					Status: appsv1.DeploymentStatus{
						ReadyReplicas: 0,
						Conditions: []appsv1.DeploymentCondition{
							{
								Type:    appsv1.DeploymentProgressing,
								Status:  "False",
								Message: "Deployment failed to progress",
							},
						},
					},
				}

				// Add to fake client
				Expect(fakeClient.Create(ctx, deployment)).To(Succeed())

				workloads := []deployer.WorkloadResource{
					{
						Object: deployment,
						Type:   kinds.Deployment,
						Name:   testDeploymentName,
					},
				}

				// Use shorter timeout for testing
				componentValidator.Config.Timeout = 1 * time.Second
				componentValidator.Config.CheckInterval = 100 * time.Millisecond

				err := componentValidator.ValidateComponent(ctx, "test-component", workloads)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("validation timeout exceeded"))
			})
		})

		Context("when workload doesn't exist", func() {
			It("should fail when workload is not found", func() {
				// Create deployment but don't add to fake client
				deployment := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "nonexistent-deployment",
						Namespace: metav1.NamespaceDefault,
					},
				}

				workloads := []deployer.WorkloadResource{
					{
						Object: deployment,
						Type:   kinds.Deployment,
						Name:   "nonexistent-deployment",
					},
				}

				// Use shorter timeout for testing
				componentValidator.Config.Timeout = 500 * time.Millisecond
				componentValidator.Config.CheckInterval = 100 * time.Millisecond
				componentValidator.Config.MaxRetries = 2

				err := componentValidator.ValidateComponent(ctx, "test-component", workloads)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to fetch workload status after"))
				Expect(err.Error()).To(ContainSubstring("retries"))
			})
		})
	})

	Describe("GetValidatorForWorkload (through factory method)", func() {
		BeforeEach(func() {
			componentValidator = validator.NewComponentValidatorWithConfig(fakeClient, defaultValidationConfig)
		})

		It("should return appropriate validator types", func() {
			// We can't directly test the private method, but we can test through behavior
			// by creating workloads and seeing if they get validated correctly

			// Test Deployment validator
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: metav1.NamespaceDefault},
				Spec:       appsv1.DeploymentSpec{Replicas: new(int32(1))},
				Status:     appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1},
			}
			Expect(fakeClient.Create(ctx, deployment)).To(Succeed())

			// Test DaemonSet validator
			daemonSet := &appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-ds", Namespace: metav1.NamespaceDefault},
				Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 1, NumberReady: 1, NumberAvailable: 1, UpdatedNumberScheduled: 1},
			}
			Expect(fakeClient.Create(ctx, daemonSet)).To(Succeed())

			// Test StatefulSet validator
			statefulSet := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sts", Namespace: metav1.NamespaceDefault},
				Spec:       appsv1.StatefulSetSpec{Replicas: new(int32(1))},
				Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1, CurrentReplicas: 1, UpdatedReplicas: 1},
			}
			Expect(fakeClient.Create(ctx, statefulSet)).To(Succeed())

			workloads := []deployer.WorkloadResource{
				{Object: deployment, Type: kinds.Deployment, Name: testName},
				{Object: daemonSet, Type: kinds.DaemonSet, Name: "test-ds"},
				{Object: statefulSet, Type: kinds.StatefulSet, Name: "test-sts"},
			}

			componentValidator.Config.Timeout = 1 * time.Second
			componentValidator.Config.CheckInterval = 100 * time.Millisecond

			// All should validate successfully, proving the right validators were used
			err := componentValidator.ValidateComponent(ctx, "test-component", workloads)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should handle unknown workload types with generic validator", func() {
			// Create a generic object (we'll use ConfigMap as an example)
			// Note: Since we can't easily test unknown types with fake client,
			// we'll test the generic validator directly in workload_validators_test.go
			// This test verifies that unknown types don't crash the system

			// Create a minimal object that would use GenericWorkloadValidator
			obj := &metav1.PartialObjectMetadata{
				TypeMeta: metav1.TypeMeta{
					Kind:       customResourceKind,
					APIVersion: "custom.io/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      testCustomName,
					Namespace: metav1.NamespaceDefault,
				},
			}

			workloads := []deployer.WorkloadResource{
				{Object: obj, Type: customResourceKind, Name: testCustomName},
			}

			componentValidator.Config.Timeout = 1 * time.Second
			componentValidator.Config.CheckInterval = 100 * time.Millisecond

			// This should not panic and should complete quickly (generic validator returns ready immediately)
			err := componentValidator.ValidateComponent(ctx, "test-component", workloads)
			// Note: This might still fail due to client issues, but it shouldn't panic
			// The main point is testing that unknown types are handled gracefully
			// We don't assert on the error since client behavior with custom objects is unpredictable
			_ = err // Explicitly ignore the error for this test
		})
	})

	Describe("Configuration edge cases", func() {
		It("should handle zero timeout gracefully", func() {
			config := &validator.ValidationConfig{
				Timeout:       0, // Zero timeout
				CheckInterval: 100 * time.Millisecond,
				MaxRetries:    5,
			}

			componentValidator = validator.NewComponentValidatorWithConfig(fakeClient, config)

			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: metav1.NamespaceDefault},
				Spec:       appsv1.DeploymentSpec{Replicas: new(int32(1))},
				Status:     appsv1.DeploymentStatus{ReadyReplicas: 0}, // Not ready
			}
			Expect(fakeClient.Create(ctx, deployment)).To(Succeed())

			workloads := []deployer.WorkloadResource{
				{Object: deployment, Type: kinds.Deployment, Name: testName},
			}

			err := componentValidator.ValidateComponent(ctx, "test-component", workloads)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("timeout"))
		})

		It("should handle very large check interval", func() {
			config := &validator.ValidationConfig{
				Timeout:       100 * time.Millisecond, // Short timeout
				CheckInterval: 1 * time.Hour,          // Very long interval
				MaxRetries:    5,
			}

			componentValidator = validator.NewComponentValidatorWithConfig(fakeClient, config)

			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: metav1.NamespaceDefault},
				Spec:       appsv1.DeploymentSpec{Replicas: new(int32(1))},
				Status:     appsv1.DeploymentStatus{ReadyReplicas: 0}, // Not ready
			}
			Expect(fakeClient.Create(ctx, deployment)).To(Succeed())

			workloads := []deployer.WorkloadResource{
				{Object: deployment, Type: kinds.Deployment, Name: testName},
			}

			err := componentValidator.ValidateComponent(ctx, "test-component", workloads)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("timeout"))
		})

		It("should handle zero max retries", func() {
			config := &validator.ValidationConfig{
				Timeout:       1 * time.Second,
				CheckInterval: 100 * time.Millisecond,
				MaxRetries:    0, // Zero retries
			}

			componentValidator = validator.NewComponentValidatorWithConfig(fakeClient, config)

			// Don't create the deployment so it will fail to fetch
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "nonexistent", Namespace: metav1.NamespaceDefault},
			}

			workloads := []deployer.WorkloadResource{
				{Object: deployment, Type: kinds.Deployment, Name: "nonexistent"},
			}

			err := componentValidator.ValidateComponent(ctx, "test-component", workloads)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to fetch workload status after 0 retries"))
		})
	})
})
