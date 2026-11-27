// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package validator_test

import (
	"errors"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/kinds"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/validator"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("WorkloadValidators", func() {

	Describe("DeploymentValidator", func() {
		var deploymentValidator *validator.DeploymentValidator

		BeforeEach(func() {
			deploymentValidator = &validator.DeploymentValidator{}
		})

		Context("when deployment is ready", func() {
			It("should return ready for fully deployed deployment", func() {
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

				result, err := deploymentValidator.ValidateReadiness(deployment)
				Expect(err).ToNot(HaveOccurred())
				Expect(result).ToNot(BeNil())
				Expect(result.Ready).To(BeTrue())
				Expect(result.WorkloadName).To(Equal(testDeploymentName))
				Expect(result.WorkloadType).To(Equal(kinds.Deployment))
				Expect(result.Message).To(ContainSubstring("Ready: 3/3"))
				Expect(result.Message).To(ContainSubstring("Available: 3/3"))
				Expect(result.Message).To(ContainSubstring("Updated: 3/3"))
			})

			It("should handle deployment with nil replicas (defaults to 1)", func() {
				deployment := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name: testDeploymentName,
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: nil, // Should default to 1
					},
					Status: appsv1.DeploymentStatus{
						ReadyReplicas:     1,
						AvailableReplicas: 1,
						UpdatedReplicas:   1,
					},
				}

				result, err := deploymentValidator.ValidateReadiness(deployment)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeTrue())
				Expect(result.Message).To(ContainSubstring("Ready: 1/1"))
			})

			It("should handle zero replicas deployment", func() {
				deployment := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name: testDeploymentName,
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: new(int32(0)),
					},
					Status: appsv1.DeploymentStatus{
						ReadyReplicas:     0,
						AvailableReplicas: 0,
						UpdatedReplicas:   0,
					},
				}

				result, err := deploymentValidator.ValidateReadiness(deployment)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeTrue())
				Expect(result.Message).To(ContainSubstring("Ready: 0/0"))
			})
		})

		Context("when deployment is not ready", func() {
			It("should return not ready when replicas don't match", func() {
				deployment := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name: testDeploymentName,
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: new(int32(3)),
					},
					Status: appsv1.DeploymentStatus{
						ReadyReplicas:     2, // Not all ready
						AvailableReplicas: 2,
						UpdatedReplicas:   2,
					},
				}

				result, err := deploymentValidator.ValidateReadiness(deployment)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeFalse())
				Expect(result.Message).To(ContainSubstring("Ready: 2/3"))
			})

			It("should return not ready when available replicas don't match", func() {
				deployment := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name: testDeploymentName,
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: new(int32(3)),
					},
					Status: appsv1.DeploymentStatus{
						ReadyReplicas:     3,
						AvailableReplicas: 2, // Not all available
						UpdatedReplicas:   3,
					},
				}

				result, err := deploymentValidator.ValidateReadiness(deployment)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeFalse())
				Expect(result.Message).To(ContainSubstring("Available: 2/3"))
			})

			It("should return not ready when updated replicas don't match", func() {
				deployment := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name: testDeploymentName,
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: new(int32(3)),
					},
					Status: appsv1.DeploymentStatus{
						ReadyReplicas:     3,
						AvailableReplicas: 3,
						UpdatedReplicas:   2, // Not all updated
					},
				}

				result, err := deploymentValidator.ValidateReadiness(deployment)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeFalse())
				Expect(result.Message).To(ContainSubstring("Updated: 2/3"))
			})

			It("should handle deployment with progressing=false condition", func() {
				deployment := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name: testDeploymentName,
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

				result, err := deploymentValidator.ValidateReadiness(deployment)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeFalse())
				Expect(result.Message).To(ContainSubstring("Deployment failed to progress"))
			})
		})

		Context("when using unstructured objects", func() {
			It("should handle unstructured deployment objects", func() {
				unstructuredDeployment := &unstructured.Unstructured{
					Object: map[string]any{
						fieldAPIVersion: appsv1.SchemeGroupVersion.String(),
						fieldKind:       kinds.Deployment,
						fieldMetadata: map[string]any{
							fieldName:      testDeploymentName,
							fieldNamespace: metav1.NamespaceDefault,
						},
						fieldSpec: map[string]any{
							fieldReplicas: int64(2), // JSON numbers become int64
						},
						fieldStatus: map[string]any{
							fieldReadyReplicas:   int64(2),
							"availableReplicas":  int64(2),
							fieldUpdatedReplicas: int64(2),
						},
					},
				}

				result, err := deploymentValidator.ValidateReadiness(unstructuredDeployment)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeTrue())
				Expect(result.WorkloadName).To(Equal(testDeploymentName))
				Expect(result.Message).To(ContainSubstring("Ready: 2/2"))
			})

			It("should handle conversion errors gracefully", func() {
				// Create an invalid unstructured object
				invalidObject := &unstructured.Unstructured{
					Object: map[string]any{
						fieldAPIVersion: "v1",
						fieldKind:       kinds.Pod,
						fieldMetadata: map[string]any{
							fieldName: testPodName,
						},
					},
				}

				result, err := deploymentValidator.ValidateReadiness(invalidObject)

				// The conversion might succeed but produce an invalid Deployment object
				// In that case, the validator should still fail validation because
				// the resulting object won't have proper Deployment fields
				if err != nil {
					// Expected: conversion error
					Expect(result).To(BeNil())
					Expect(err.Error()).To(ContainSubstring("failed to convert unstructured to Deployment"))
				} else {
					// Alternative: conversion succeeds but validation fails due to missing fields
					Expect(result).ToNot(BeNil())
					Expect(result.Ready).To(BeFalse())
					// The resulting object will have zero values for deployment fields
				}
			})
		})

		Context("when using wrong object types", func() {
			It("should return error for non-deployment objects", func() {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: testPodName,
					},
				}

				result, err := deploymentValidator.ValidateReadiness(pod)
				Expect(err).To(HaveOccurred())
				Expect(result).To(BeNil())
				Expect(err.Error()).To(ContainSubstring("expected Deployment or Unstructured"))
			})
		})
	})

	Describe("DaemonSetValidator", func() {
		var daemonSetValidator *validator.DaemonSetValidator

		BeforeEach(func() {
			daemonSetValidator = &validator.DaemonSetValidator{}
		})

		Context("when daemonset is ready", func() {
			It("should return ready for fully deployed daemonset", func() {
				daemonSet := &appsv1.DaemonSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testDaemonSetName,
						Namespace: metav1.NamespaceDefault,
					},
					Status: appsv1.DaemonSetStatus{
						DesiredNumberScheduled: 3,
						NumberReady:            3,
						NumberAvailable:        3,
						UpdatedNumberScheduled: 3,
					},
				}

				result, err := daemonSetValidator.ValidateReadiness(daemonSet)
				Expect(err).ToNot(HaveOccurred())
				Expect(result).ToNot(BeNil())
				Expect(result.Ready).To(BeTrue())
				Expect(result.WorkloadName).To(Equal(testDaemonSetName))
				Expect(result.WorkloadType).To(Equal(kinds.DaemonSet))
				Expect(result.Message).To(ContainSubstring("Ready: 3/3"))
				Expect(result.Message).To(ContainSubstring("Available: 3/3"))
				Expect(result.Message).To(ContainSubstring("Updated: 3/3"))
			})
		})

		Context("when daemonset is not ready", func() {
			It("should return not ready when desired is zero", func() {
				daemonSet := &appsv1.DaemonSet{
					ObjectMeta: metav1.ObjectMeta{
						Name: testDaemonSetName,
					},
					Status: appsv1.DaemonSetStatus{
						DesiredNumberScheduled: 0, // No nodes to schedule on
						NumberReady:            0,
						NumberAvailable:        0,
						UpdatedNumberScheduled: 0,
					},
				}

				result, err := daemonSetValidator.ValidateReadiness(daemonSet)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeFalse())
				Expect(result.Message).To(ContainSubstring("Ready: 0/0"))
			})

			It("should return not ready when not all pods are ready", func() {
				daemonSet := &appsv1.DaemonSet{
					ObjectMeta: metav1.ObjectMeta{
						Name: testDaemonSetName,
					},
					Status: appsv1.DaemonSetStatus{
						DesiredNumberScheduled: 3,
						NumberReady:            2, // Not all ready
						NumberAvailable:        2,
						UpdatedNumberScheduled: 3,
					},
				}

				result, err := daemonSetValidator.ValidateReadiness(daemonSet)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeFalse())
				Expect(result.Message).To(ContainSubstring("Ready: 2/3"))
			})

			It("should return not ready when not all pods are available", func() {
				daemonSet := &appsv1.DaemonSet{
					ObjectMeta: metav1.ObjectMeta{
						Name: testDaemonSetName,
					},
					Status: appsv1.DaemonSetStatus{
						DesiredNumberScheduled: 3,
						NumberReady:            3,
						NumberAvailable:        2, // Not all available
						UpdatedNumberScheduled: 3,
					},
				}

				result, err := daemonSetValidator.ValidateReadiness(daemonSet)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeFalse())
				Expect(result.Message).To(ContainSubstring("Available: 2/3"))
			})

			It("should return not ready when not all pods are updated", func() {
				daemonSet := &appsv1.DaemonSet{
					ObjectMeta: metav1.ObjectMeta{
						Name: testDaemonSetName,
					},
					Status: appsv1.DaemonSetStatus{
						DesiredNumberScheduled: 3,
						NumberReady:            3,
						NumberAvailable:        3,
						UpdatedNumberScheduled: 2, // Not all updated
					},
				}

				result, err := daemonSetValidator.ValidateReadiness(daemonSet)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeFalse())
				Expect(result.Message).To(ContainSubstring("Updated: 2/3"))
			})

			It("should handle misscheduled pods", func() {
				daemonSet := &appsv1.DaemonSet{
					ObjectMeta: metav1.ObjectMeta{
						Name: testDaemonSetName,
					},
					Status: appsv1.DaemonSetStatus{
						DesiredNumberScheduled: 3,
						NumberReady:            2,
						NumberAvailable:        2,
						UpdatedNumberScheduled: 3,
						NumberMisscheduled:     1, // Some pods are misscheduled
					},
				}

				result, err := daemonSetValidator.ValidateReadiness(daemonSet)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeFalse())
				Expect(result.Message).To(ContainSubstring("Misscheduled: 1"))
			})
		})

		Context("when using unstructured objects", func() {
			It("should handle unstructured daemonset objects", func() {
				unstructuredDaemonSet := &unstructured.Unstructured{
					Object: map[string]any{
						fieldAPIVersion: appsv1.SchemeGroupVersion.String(),
						fieldKind:       kinds.DaemonSet,
						fieldMetadata: map[string]any{
							fieldName:      testDaemonSetName,
							fieldNamespace: metav1.NamespaceDefault,
						},
						fieldStatus: map[string]any{
							"desiredNumberScheduled": int64(2),
							"numberReady":            int64(2),
							"numberAvailable":        int64(2),
							"updatedNumberScheduled": int64(2),
						},
					},
				}

				result, err := daemonSetValidator.ValidateReadiness(unstructuredDaemonSet)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeTrue())
				Expect(result.WorkloadName).To(Equal(testDaemonSetName))
				Expect(result.Message).To(ContainSubstring("Ready: 2/2"))
			})
		})

		Context("when using wrong object types", func() {
			It("should return error for non-daemonset objects", func() {
				deployment := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name: testDeploymentName,
					},
				}

				result, err := daemonSetValidator.ValidateReadiness(deployment)
				Expect(err).To(HaveOccurred())
				Expect(result).To(BeNil())
				Expect(err.Error()).To(ContainSubstring("expected DaemonSet or Unstructured"))
			})
		})
	})

	Describe("StatefulSetValidator", func() {
		var statefulSetValidator *validator.StatefulSetValidator

		BeforeEach(func() {
			statefulSetValidator = &validator.StatefulSetValidator{}
		})

		Context("when statefulset is ready", func() {
			It("should return ready for fully deployed statefulset", func() {
				statefulSet := &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testStatefulSetName,
						Namespace: metav1.NamespaceDefault,
					},
					Spec: appsv1.StatefulSetSpec{
						Replicas: new(int32(3)),
					},
					Status: appsv1.StatefulSetStatus{
						ReadyReplicas:   3,
						CurrentReplicas: 3,
						UpdatedReplicas: 3,
					},
				}

				result, err := statefulSetValidator.ValidateReadiness(statefulSet)
				Expect(err).ToNot(HaveOccurred())
				Expect(result).ToNot(BeNil())
				Expect(result.Ready).To(BeTrue())
				Expect(result.WorkloadName).To(Equal(testStatefulSetName))
				Expect(result.WorkloadType).To(Equal(kinds.StatefulSet))
				Expect(result.Message).To(ContainSubstring("Ready: 3/3"))
				Expect(result.Message).To(ContainSubstring("Current: 3/3"))
				Expect(result.Message).To(ContainSubstring("Updated: 3/3"))
			})

			It("should handle statefulset with nil replicas (defaults to 1)", func() {
				statefulSet := &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Name: testStatefulSetName,
					},
					Spec: appsv1.StatefulSetSpec{
						Replicas: nil, // Should default to 1
					},
					Status: appsv1.StatefulSetStatus{
						ReadyReplicas:   1,
						CurrentReplicas: 1,
						UpdatedReplicas: 1,
					},
				}

				result, err := statefulSetValidator.ValidateReadiness(statefulSet)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeTrue())
				Expect(result.Message).To(ContainSubstring("Ready: 1/1"))
			})

			It("should handle zero replicas statefulset", func() {
				statefulSet := &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Name: testStatefulSetName,
					},
					Spec: appsv1.StatefulSetSpec{
						Replicas: new(int32(0)),
					},
					Status: appsv1.StatefulSetStatus{
						ReadyReplicas:   0,
						CurrentReplicas: 0,
						UpdatedReplicas: 0,
					},
				}

				result, err := statefulSetValidator.ValidateReadiness(statefulSet)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeTrue())
				Expect(result.Message).To(ContainSubstring("Ready: 0/0"))
			})
		})

		Context("when statefulset is not ready", func() {
			It("should return not ready when ready replicas don't match", func() {
				statefulSet := &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Name: testStatefulSetName,
					},
					Spec: appsv1.StatefulSetSpec{
						Replicas: new(int32(3)),
					},
					Status: appsv1.StatefulSetStatus{
						ReadyReplicas:   2, // Not all ready
						CurrentReplicas: 3,
						UpdatedReplicas: 3,
					},
				}

				result, err := statefulSetValidator.ValidateReadiness(statefulSet)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeFalse())
				Expect(result.Message).To(ContainSubstring("Ready: 2/3"))
			})

			It("should return not ready when current replicas don't match", func() {
				statefulSet := &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Name: testStatefulSetName,
					},
					Spec: appsv1.StatefulSetSpec{
						Replicas: new(int32(3)),
					},
					Status: appsv1.StatefulSetStatus{
						ReadyReplicas:   3,
						CurrentReplicas: 2, // Not all current
						UpdatedReplicas: 3,
					},
				}

				result, err := statefulSetValidator.ValidateReadiness(statefulSet)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeFalse())
				Expect(result.Message).To(ContainSubstring("Current: 2/3"))
			})

			It("should return not ready when updated replicas don't match", func() {
				statefulSet := &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Name: testStatefulSetName,
					},
					Spec: appsv1.StatefulSetSpec{
						Replicas: new(int32(3)),
					},
					Status: appsv1.StatefulSetStatus{
						ReadyReplicas:   3,
						CurrentReplicas: 3,
						UpdatedReplicas: 2, // Not all updated
					},
				}

				result, err := statefulSetValidator.ValidateReadiness(statefulSet)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeFalse())
				Expect(result.Message).To(ContainSubstring("Updated: 2/3"))
			})
		})

		Context("when using unstructured objects", func() {
			It("should handle unstructured statefulset objects", func() {
				unstructuredStatefulSet := &unstructured.Unstructured{
					Object: map[string]any{
						fieldAPIVersion: appsv1.SchemeGroupVersion.String(),
						fieldKind:       kinds.StatefulSet,
						fieldMetadata: map[string]any{
							fieldName:      testStatefulSetName,
							fieldNamespace: metav1.NamespaceDefault,
						},
						fieldSpec: map[string]any{
							fieldReplicas: int64(2),
						},
						fieldStatus: map[string]any{
							fieldReadyReplicas:   int64(2),
							"currentReplicas":    int64(2),
							fieldUpdatedReplicas: int64(2),
						},
					},
				}

				result, err := statefulSetValidator.ValidateReadiness(unstructuredStatefulSet)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeTrue())
				Expect(result.WorkloadName).To(Equal(testStatefulSetName))
				Expect(result.Message).To(ContainSubstring("Ready: 2/2"))
			})
		})

		Context("when using wrong object types", func() {
			It("should return error for non-statefulset objects", func() {
				deployment := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name: testDeploymentName,
					},
				}

				result, err := statefulSetValidator.ValidateReadiness(deployment)
				Expect(err).To(HaveOccurred())
				Expect(result).To(BeNil())
				Expect(err.Error()).To(ContainSubstring("expected StatefulSet or Unstructured"))
			})
		})
	})

	Describe("GenericWorkloadValidator", func() {
		var genericValidator *validator.GenericWorkloadValidator

		BeforeEach(func() {
			genericValidator = &validator.GenericWorkloadValidator{}
		})

		It("should always return ready for any object", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testPodName,
					Namespace: metav1.NamespaceDefault,
				},
			}

			result, err := genericValidator.ValidateReadiness(pod)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			Expect(result.Ready).To(BeTrue())
			Expect(result.WorkloadName).To(Equal(testPodName))
			Expect(result.WorkloadType).To(Equal("")) // GVK Kind will be empty for typed objects in tests
			Expect(result.Message).To(Equal("Generic validation - resource exists"))
		})

		It("should handle unstructured objects", func() {
			customResource := &unstructured.Unstructured{
				Object: map[string]any{
					fieldAPIVersion: "custom.io/v1",
					fieldKind:       customResourceKind,
					fieldMetadata: map[string]any{
						fieldName:      testCustomName,
						fieldNamespace: metav1.NamespaceDefault,
					},
				},
			}

			result, err := genericValidator.ValidateReadiness(customResource)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Ready).To(BeTrue())
			Expect(result.WorkloadName).To(Equal(testCustomName))
			Expect(result.WorkloadType).To(Equal(customResourceKind))
		})

		It("should handle objects without namespace", func() {
			clusterRole := &unstructured.Unstructured{
				Object: map[string]any{
					fieldAPIVersion: "rbac.authorization.k8s.io/v1",
					fieldKind:       kinds.ClusterRole,
					fieldMetadata: map[string]any{
						fieldName: "test-cluster-role",
					},
				},
			}

			result, err := genericValidator.ValidateReadiness(clusterRole)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Ready).To(BeTrue())
			Expect(result.WorkloadName).To(Equal("test-cluster-role"))
			Expect(result.WorkloadType).To(Equal(kinds.ClusterRole))
		})
	})

	Describe("Type Conversion Helper", func() {
		// Note: We can't directly test the generic convertToTyped function since it's not exported
		// But we can test its behavior through the validators that use it

		Context("convertToTyped behavior through validators", func() {
			It("should handle valid type conversion", func() {
				validator := &validator.DeploymentValidator{}

				// Test with proper typed object
				deployment := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: testName},
					Spec:       appsv1.DeploymentSpec{Replicas: new(int32(1))},
					Status:     appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1},
				}

				result, err := validator.ValidateReadiness(deployment)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeTrue())
			})

			It("should handle valid unstructured conversion", func() {
				validator := &validator.DeploymentValidator{}

				// Test with unstructured object
				unstructured := &unstructured.Unstructured{
					Object: map[string]any{
						fieldAPIVersion: appsv1.SchemeGroupVersion.String(),
						fieldKind:       kinds.Deployment,
						fieldMetadata:   map[string]any{fieldName: testName},
						fieldSpec:       map[string]any{fieldReplicas: int64(1)},
						fieldStatus: map[string]any{
							fieldReadyReplicas:   int64(1),
							"availableReplicas":  int64(1),
							fieldUpdatedReplicas: int64(1),
						},
					},
				}

				result, err := validator.ValidateReadiness(unstructured)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Ready).To(BeTrue())
			})

			It("should handle invalid type conversion", func() {
				validator := &validator.DeploymentValidator{}

				// Test with wrong object type
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testName}}

				result, err := validator.ValidateReadiness(pod)
				Expect(err).To(HaveOccurred())
				Expect(result).To(BeNil())
				Expect(err.Error()).To(ContainSubstring("expected Deployment or Unstructured"))
			})

			It("should handle invalid unstructured conversion", func() {
				validator := &validator.DeploymentValidator{}

				// Test with unstructured object that can't be converted
				unstructured := &unstructured.Unstructured{
					Object: map[string]any{
						fieldAPIVersion: "v1",
						fieldKind:       kinds.Pod,
						fieldMetadata:   map[string]any{fieldName: testName},
					},
				}

				result, err := validator.ValidateReadiness(unstructured)

				// The conversion might succeed but produce an invalid Deployment object
				// In that case, the validator should still handle it gracefully
				if err != nil {
					// Expected: conversion error
					Expect(result).To(BeNil())
					Expect(err.Error()).To(ContainSubstring("failed to convert unstructured to Deployment"))
				} else {
					// Alternative: conversion succeeds but produces a deployment with zero values
					Expect(result).ToNot(BeNil())
					Expect(result.Ready).To(BeFalse())
					// The resulting deployment object will have zero replicas and zero ready replicas
				}
			})
		})
	})

	Describe("ValidationResult", func() {
		It("should create proper validation result structure", func() {
			result := &validator.ValidationResult{
				WorkloadName: "test-workload",
				WorkloadType: "TestType",
				Ready:        true,
				Message:      "All systems operational",
				Error:        nil,
			}

			Expect(result.WorkloadName).To(Equal("test-workload"))
			Expect(result.WorkloadType).To(Equal("TestType"))
			Expect(result.Ready).To(BeTrue())
			Expect(result.Message).To(Equal("All systems operational"))
			Expect(result.Error).ToNot(HaveOccurred())
		})

		It("should handle error conditions", func() {
			testError := errors.New("validation failed")
			result := &validator.ValidationResult{
				WorkloadName: "test-workload",
				WorkloadType: "TestType",
				Ready:        false,
				Message:      "Validation failed",
				Error:        testError,
			}

			Expect(result.Ready).To(BeFalse())
			Expect(result.Error).To(Equal(testError))
		})
	})
})
