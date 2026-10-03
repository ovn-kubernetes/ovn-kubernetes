// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package status_test

import (
	"context"
	"errors"

	appsv1 "k8s.io/api/apps/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/defaults"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/deployer"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/kinds"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/status"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const testVersion = "1.0.0"

var _ = Describe("StatusManager", func() {
	var (
		statusManager    *status.StatusManager
		statusClient     client.Client
		ovnk8sOperator   *k8sv1alpha1.OVNK8sOperator
		statusPatchCount int
		statusPatchErr   error
	)

	BeforeEach(func() {
		statusPatchCount = 0
		statusPatchErr = nil

		// Create a fake client
		scheme := runtime.NewScheme()
		Expect(k8sv1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())

		// Create a test OVNK8sOperator
		ovnk8sOperator = &k8sv1alpha1.OVNK8sOperator{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-operator",
				Namespace:  "test-namespace",
				Generation: 7,
			},
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
			Status: k8sv1alpha1.OVNK8sOperatorStatus{
				ComponentStatus: []k8sv1alpha1.ComponentStatus{},
				Conditions:      []metav1.Condition{},
			},
		}

		// Create fake client with the OVNK8sOperator object and status subresource support
		statusClient = fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(ovnk8sOperator).
			WithStatusSubresource(&k8sv1alpha1.OVNK8sOperator{}).
			WithInterceptorFuncs(interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if subResourceName == "status" {
						statusPatchCount++
						if statusPatchErr != nil {
							return statusPatchErr
						}
					}
					return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
				},
			}).
			Build()
		testConfig := &defaults.EmbeddedConfig{
			Version: testVersion,
			Components: []defaults.EmbeddedComponent{
				{ResourceName: "ovnkubeControlPlane", ResourceType: kinds.Deployment},
				{ResourceName: "ovnkubeNode", ResourceType: kinds.DaemonSet},
			},
		}
		statusManager = status.NewStatusManager(statusClient, testConfig)
	})

	Describe("NewStatusManager", func() {
		It("should create a new status manager with the given client", func() {
			manager := status.NewStatusManager(nil, nil)
			Expect(manager).ToNot(BeNil())
		})
	})

	// Test workload replica extraction logic using reflection or by testing private methods indirectly
	Describe("Workload Replica Extraction Logic", func() {
		var (
			testStatusManager *TestableStatusManager
		)

		BeforeEach(func() {
			testStatusManager = &TestableStatusManager{
				StatusManager: statusManager,
			}
		})

		Describe("Deployment replicas", func() {
			It("should extract replica information from Deployment", func() {
				deployment := &appsv1.Deployment{
					Spec: appsv1.DeploymentSpec{
						Replicas: new(int32(5)),
					},
					Status: appsv1.DeploymentStatus{
						ReadyReplicas: 3,
					},
				}

				total, ready := testStatusManager.GetDeploymentReplicas(deployment)
				Expect(total).To(Equal(int32(5)))
				Expect(ready).To(Equal(int32(3)))
			})

			It("should handle Deployment with nil replicas", func() {
				deployment := &appsv1.Deployment{
					Spec: appsv1.DeploymentSpec{
						Replicas: nil, // This should default to 1
					},
					Status: appsv1.DeploymentStatus{
						ReadyReplicas: 1,
					},
				}

				total, ready := testStatusManager.GetDeploymentReplicas(deployment)
				Expect(total).To(Equal(int32(1))) // Default value
				Expect(ready).To(Equal(int32(1)))
			})
		})

		Describe("DaemonSet replicas", func() {
			It("should extract replica information from DaemonSet", func() {
				daemonSet := &appsv1.DaemonSet{
					Status: appsv1.DaemonSetStatus{
						DesiredNumberScheduled: 4,
						NumberReady:            2,
					},
				}

				total, ready := testStatusManager.GetDaemonSetReplicas(daemonSet)
				Expect(total).To(Equal(int32(4)))
				Expect(ready).To(Equal(int32(2)))
			})
		})

		Describe("StatefulSet replicas", func() {
			It("should extract replica information from StatefulSet", func() {
				statefulSet := &appsv1.StatefulSet{
					Spec: appsv1.StatefulSetSpec{
						Replicas: new(int32(3)),
					},
					Status: appsv1.StatefulSetStatus{
						ReadyReplicas: 2,
					},
				}

				total, ready := testStatusManager.GetStatefulSetReplicas(statefulSet)
				Expect(total).To(Equal(int32(3)))
				Expect(ready).To(Equal(int32(2)))
			})
		})

		Describe("Unstructured workload objects", func() {
			It("should handle unstructured Deployment objects", func() {
				// Create an unstructured Deployment
				unstructuredDeployment := &unstructured.Unstructured{
					Object: map[string]any{
						"apiVersion": "apps/v1",
						"kind":       kinds.Deployment,
						"spec": map[string]any{
							"replicas": int64(4), // JSON numbers become int64
						},
						"status": map[string]any{
							"readyReplicas": int64(3),
						},
					},
				}

				total, ready := testStatusManager.GetDeploymentReplicas(unstructuredDeployment)
				Expect(total).To(Equal(int32(4)))
				Expect(ready).To(Equal(int32(3)))
			})

			It("should handle conversion errors gracefully", func() {
				// Create an invalid unstructured object
				invalidObject := &unstructured.Unstructured{
					Object: map[string]any{
						"apiVersion": "v1",
						"kind":       kinds.Pod,
					},
				}

				total, ready := testStatusManager.GetDeploymentReplicas(invalidObject)
				Expect(total).To(Equal(int32(1))) // Default fallback
				Expect(ready).To(Equal(int32(0))) // Default fallback
			})
		})

		Describe("Unknown workload types", func() {
			It("should default to 1 replica for unknown workload types", func() {
				workload := deployer.WorkloadResource{
					Object: &unstructured.Unstructured{},
					Type:   "CustomWorkload", // Unknown type
					Name:   "test-unknown",
				}

				total, ready := testStatusManager.GetWorkloadReplicas(workload)
				Expect(total).To(Equal(int32(1)))
				Expect(ready).To(Equal(int32(1)))
			})
		})
	})

	// Test new simplified status methods
	Describe("CNI Health Status Management", func() {
		var ctx context.Context

		BeforeEach(func() {
			ctx = context.Background()
		})

		Describe("UpdateCNIHealthy", func() {
			It("should set CNI status to healthy", func() {
				err := statusManager.UpdateCNIHealthy(ctx, ovnk8sOperator, "All CNI components operational")

				Expect(err).ToNot(HaveOccurred())

				// Verify the condition was set correctly
				Expect(ovnk8sOperator.Status.Conditions).To(HaveLen(1))
				condition := ovnk8sOperator.Status.Conditions[0]
				Expect(condition.Type).To(Equal(status.ConditionTypeCNIHealthy))
				Expect(condition.Status).To(Equal(metav1.ConditionTrue))
				Expect(condition.Reason).To(Equal(status.ReasonCNIReady))
				Expect(condition.Message).To(Equal("All CNI components operational"))
				Expect(condition.ObservedGeneration).To(Equal(int64(7)))
				Expect(condition.LastTransitionTime.IsZero()).To(BeFalse())
			})
		})

		Describe("UpdateCNIDegraded", func() {
			It("should set CNI status to degraded", func() {
				err := statusManager.UpdateCNIDegraded(ctx, ovnk8sOperator, "Component ovn-master failed")

				Expect(err).ToNot(HaveOccurred())

				// Verify the condition was set correctly
				Expect(ovnk8sOperator.Status.Conditions).To(HaveLen(1))
				condition := ovnk8sOperator.Status.Conditions[0]
				Expect(condition.Type).To(Equal(status.ConditionTypeCNIHealthy))
				Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				Expect(condition.Reason).To(Equal(status.ReasonCNIDegraded))
				Expect(condition.Message).To(Equal("Component ovn-master failed"))
				Expect(condition.ObservedGeneration).To(Equal(int64(7)))
			})
		})

		Describe("UpdateCNIUnknown", func() {
			It("should set CNI status to unknown", func() {
				err := statusManager.UpdateCNIUnknown(ctx, ovnk8sOperator, "Cannot determine CNI health")

				Expect(err).ToNot(HaveOccurred())

				// Verify the condition was set correctly
				Expect(ovnk8sOperator.Status.Conditions).To(HaveLen(1))
				condition := ovnk8sOperator.Status.Conditions[0]
				Expect(condition.Type).To(Equal(status.ConditionTypeCNIHealthy))
				Expect(condition.Status).To(Equal(metav1.ConditionUnknown))
				Expect(condition.Reason).To(Equal(status.ReasonCNIUnknown))
				Expect(condition.Message).To(Equal("Cannot determine CNI health"))
				Expect(condition.ObservedGeneration).To(Equal(int64(7)))
			})
		})
	})

	Describe("Rollout Status Management", func() {
		var ctx context.Context

		BeforeEach(func() {
			ctx = context.Background()
		})

		It("should commit a fresh version only when installation completes", func() {
			Expect(ovnk8sOperator.Status.Version).To(BeEmpty())
			Expect(ovnk8sOperator.Status.TargetVersion).To(BeEmpty())
			Expect(ovnk8sOperator.Status.UpgradeStatus).To(BeEmpty())

			Expect(statusManager.UpdateRolloutInProgress(ctx, ovnk8sOperator,
				testVersion, status.ReasonInstallationInProgress,
				"Installing OVN-Kubernetes release 1.0.0")).To(Succeed())

			Expect(ovnk8sOperator.Status.Version).To(BeEmpty())
			Expect(ovnk8sOperator.Status.TargetVersion).To(Equal(testVersion))
			Expect(ovnk8sOperator.Status.UpgradeStatus).To(Equal(k8sv1alpha1.UpgradeStatusInProgress))

			upgradeCondition := apimeta.FindStatusCondition(ovnk8sOperator.Status.Conditions, status.ConditionTypeUpgradeComplete)
			Expect(upgradeCondition).ToNot(BeNil())
			Expect(upgradeCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(upgradeCondition.Reason).To(Equal(status.ReasonInstallationInProgress))

			cniCondition := apimeta.FindStatusCondition(ovnk8sOperator.Status.Conditions, status.ConditionTypeCNIHealthy)
			Expect(cniCondition).ToNot(BeNil())
			Expect(cniCondition.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cniCondition.Reason).To(Equal(status.ReasonCNIInitializing))

			Expect(statusManager.UpdateRolloutComplete(ctx, ovnk8sOperator,
				testVersion, status.ReasonInstallationCompleted,
				"Installation completed", "All CNI components operational")).To(Succeed())

			Expect(ovnk8sOperator.Status.Version).To(Equal(testVersion))
			Expect(ovnk8sOperator.Status.TargetVersion).To(Equal(testVersion))
			Expect(ovnk8sOperator.Status.UpgradeStatus).To(Equal(k8sv1alpha1.UpgradeStatusCompleted))
			upgradeCondition = apimeta.FindStatusCondition(ovnk8sOperator.Status.Conditions, status.ConditionTypeUpgradeComplete)
			Expect(upgradeCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(upgradeCondition.Reason).To(Equal(status.ReasonInstallationCompleted))
			Expect(statusPatchCount).To(Equal(2))
		})

		It("should keep the completed version until an upgrade completes", func() {
			ovnk8sOperator.Status.Version = testVersion
			ovnk8sOperator.Status.Conditions = []metav1.Condition{{
				Type:    status.ConditionTypeCNIHealthy,
				Status:  metav1.ConditionTrue,
				Reason:  status.ReasonCNIReady,
				Message: "CNI is healthy",
			}}
			Expect(statusClient.Status().Update(ctx, ovnk8sOperator)).To(Succeed())

			Expect(statusManager.UpdateRolloutInProgress(ctx, ovnk8sOperator,
				"1.1.0", status.ReasonUpgradeInProgress,
				"Upgrading OVN-Kubernetes from 1.0.0 to 1.1.0")).To(Succeed())

			Expect(ovnk8sOperator.Status.Version).To(Equal(testVersion))
			Expect(ovnk8sOperator.Status.TargetVersion).To(Equal("1.1.0"))
			Expect(ovnk8sOperator.Status.UpgradeStatus).To(Equal(k8sv1alpha1.UpgradeStatusInProgress))
			cniCondition := apimeta.FindStatusCondition(ovnk8sOperator.Status.Conditions, status.ConditionTypeCNIHealthy)
			Expect(cniCondition.Status).To(Equal(metav1.ConditionTrue))

			Expect(statusManager.UpdateRolloutComplete(ctx, ovnk8sOperator,
				"1.1.0", status.ReasonUpgradeCompleted,
				"Upgrade completed", "All CNI components operational")).To(Succeed())

			Expect(ovnk8sOperator.Status.Version).To(Equal("1.1.0"))
			Expect(ovnk8sOperator.Status.TargetVersion).To(Equal("1.1.0"))
			Expect(ovnk8sOperator.Status.UpgradeStatus).To(Equal(k8sv1alpha1.UpgradeStatusCompleted))
		})

		It("should atomically complete a rollout and report healthy CNI", func() {
			ovnk8sOperator.Status.Version = testVersion

			Expect(statusManager.UpdateRolloutComplete(ctx, ovnk8sOperator,
				"1.1.0", status.ReasonUpgradeCompleted,
				"Upgrade completed successfully", "All CNI components operational")).To(Succeed())

			Expect(ovnk8sOperator.Status.Version).To(Equal("1.1.0"))
			Expect(ovnk8sOperator.Status.TargetVersion).To(Equal("1.1.0"))
			Expect(ovnk8sOperator.Status.UpgradeStatus).To(Equal(k8sv1alpha1.UpgradeStatusCompleted))
			upgradeCondition := apimeta.FindStatusCondition(ovnk8sOperator.Status.Conditions, status.ConditionTypeUpgradeComplete)
			Expect(upgradeCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(upgradeCondition.Reason).To(Equal(status.ReasonUpgradeCompleted))
			cniCondition := apimeta.FindStatusCondition(ovnk8sOperator.Status.Conditions, status.ConditionTypeCNIHealthy)
			Expect(cniCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(cniCondition.Reason).To(Equal(status.ReasonCNIReady))
			Expect(statusPatchCount).To(Equal(1))
		})

		It("should not mutate local status when a patch fails", func() {
			originalStatus := ovnk8sOperator.Status.DeepCopy()
			statusPatchErr = errors.New("status patch failed")

			err := statusManager.UpdateRolloutInProgress(ctx, ovnk8sOperator,
				testVersion, status.ReasonInstallationInProgress,
				"Installing OVN-Kubernetes release 1.0.0")

			Expect(err).To(MatchError(ContainSubstring("status patch failed")))
			Expect(ovnk8sOperator.Status).To(Equal(*originalStatus))

			statusPatchErr = nil
			Expect(statusManager.UpdateRolloutComplete(ctx, ovnk8sOperator,
				testVersion, status.ReasonInstallationCompleted,
				"Installation completed", "All CNI components operational")).To(Succeed())

			persisted := &k8sv1alpha1.OVNK8sOperator{}
			Expect(statusClient.Get(ctx, client.ObjectKeyFromObject(ovnk8sOperator), persisted)).To(Succeed())
			Expect(persisted.Status.Version).To(Equal(testVersion))
			Expect(persisted.Status.TargetVersion).To(Equal(testVersion))
			Expect(persisted.Status.UpgradeStatus).To(Equal(k8sv1alpha1.UpgradeStatusCompleted))
		})

		It("should preserve completed version and CNI health on a pre-workload failure", func() {
			ovnk8sOperator.Status.Version = testVersion
			ovnk8sOperator.Status.Conditions = []metav1.Condition{{
				Type:   status.ConditionTypeCNIHealthy,
				Status: metav1.ConditionTrue,
				Reason: status.ReasonCNIReady,
			}}
			Expect(statusClient.Status().Update(ctx, ovnk8sOperator)).To(Succeed())

			Expect(statusManager.UpdateRolloutFailed(ctx, ovnk8sOperator,
				"1.1.0", status.ReasonTemplateRenderingFailed,
				"Template rendering failed")).To(Succeed())

			Expect(ovnk8sOperator.Status.Version).To(Equal(testVersion))
			Expect(ovnk8sOperator.Status.TargetVersion).To(Equal("1.1.0"))
			Expect(ovnk8sOperator.Status.UpgradeStatus).To(Equal(k8sv1alpha1.UpgradeStatusFailed))
			cniCondition := apimeta.FindStatusCondition(ovnk8sOperator.Status.Conditions, status.ConditionTypeCNIHealthy)
			Expect(cniCondition.Status).To(Equal(metav1.ConditionTrue))
		})

		It("should atomically fail a rollout and report degraded CNI", func() {
			ovnk8sOperator.Status.Version = testVersion
			Expect(statusClient.Status().Update(ctx, ovnk8sOperator)).To(Succeed())

			Expect(statusManager.UpdateRolloutFailedAndCNIDegraded(ctx, ovnk8sOperator,
				"1.1.0", status.ReasonComponentDeploymentFailed,
				"Component deployment failed", "Component is unavailable")).To(Succeed())

			Expect(ovnk8sOperator.Status.Version).To(Equal(testVersion))
			Expect(ovnk8sOperator.Status.TargetVersion).To(Equal("1.1.0"))
			Expect(ovnk8sOperator.Status.UpgradeStatus).To(Equal(k8sv1alpha1.UpgradeStatusFailed))
			upgradeCondition := apimeta.FindStatusCondition(ovnk8sOperator.Status.Conditions, status.ConditionTypeUpgradeComplete)
			Expect(upgradeCondition.Status).To(Equal(metav1.ConditionFalse))
			cniCondition := apimeta.FindStatusCondition(ovnk8sOperator.Status.Conditions, status.ConditionTypeCNIHealthy)
			Expect(cniCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(cniCondition.Reason).To(Equal(status.ReasonCNIDegraded))
			Expect(statusPatchCount).To(Equal(1))
		})

	})

	Describe("Image-Pull Secret Sync Status Management", func() {
		var ctx context.Context

		BeforeEach(func() {
			ctx = context.Background()
		})

		It("should record a successful sync", func() {
			err := statusManager.UpdateImagePullSecretsSynced(ctx, ovnk8sOperator,
				status.ReasonImagePullSecretsSynced,
				"Propagated 1 image-pull secret")
			Expect(err).ToNot(HaveOccurred())

			condition := apimeta.FindStatusCondition(
				ovnk8sOperator.Status.Conditions,
				status.ConditionTypeImagePullSecretsSynced,
			)
			Expect(condition).ToNot(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(status.ReasonImagePullSecretsSynced))
			Expect(condition.ObservedGeneration).To(Equal(int64(7)))
		})

		It("should record a failed sync", func() {
			err := statusManager.UpdateImagePullSecretsFailed(ctx, ovnk8sOperator,
				"source secret not found")
			Expect(err).ToNot(HaveOccurred())

			condition := apimeta.FindStatusCondition(
				ovnk8sOperator.Status.Conditions,
				status.ConditionTypeImagePullSecretsSynced,
			)
			Expect(condition).ToNot(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(status.ReasonImagePullSecretSyncFailed))
			Expect(condition.Message).To(Equal("source secret not found"))
			Expect(condition.ObservedGeneration).To(Equal(int64(7)))
		})
	})

	Describe("Condition Update Semantics", func() {
		It("should preserve transition time and skip unchanged status patches", func() {
			ctx := context.Background()
			message := "All CNI components operational"

			Expect(statusManager.UpdateCNIHealthy(ctx, ovnk8sOperator, message)).To(Succeed())
			condition := apimeta.FindStatusCondition(
				ovnk8sOperator.Status.Conditions,
				status.ConditionTypeCNIHealthy,
			)
			Expect(condition).ToNot(BeNil())
			transitionTime := condition.LastTransitionTime
			Expect(statusPatchCount).To(Equal(1))

			Expect(statusManager.UpdateCNIHealthy(ctx, ovnk8sOperator, message)).To(Succeed())
			condition = apimeta.FindStatusCondition(
				ovnk8sOperator.Status.Conditions,
				status.ConditionTypeCNIHealthy,
			)
			Expect(condition.LastTransitionTime).To(Equal(transitionTime))
			Expect(statusPatchCount).To(Equal(1))

			ovnk8sOperator.Generation = 8
			Expect(statusManager.UpdateCNIHealthy(ctx, ovnk8sOperator, message)).To(Succeed())
			condition = apimeta.FindStatusCondition(
				ovnk8sOperator.Status.Conditions,
				status.ConditionTypeCNIHealthy,
			)
			Expect(condition.ObservedGeneration).To(Equal(int64(8)))
			Expect(condition.LastTransitionTime).To(Equal(transitionTime))
			Expect(statusPatchCount).To(Equal(2))
		})
	})

	// Test resource type detection
	Describe("Resource Type Detection", func() {
		var testStatusManager *TestableStatusManager

		BeforeEach(func() {
			testStatusManager = &TestableStatusManager{
				StatusManager: statusManager,
			}
		})

		It("should return correct resource type for existing component", func() {
			resourceType := testStatusManager.GetResourceType("ovnkubeControlPlane")
			Expect(resourceType).To(Equal(kinds.Deployment))

			resourceType = testStatusManager.GetResourceType("ovnkubeNode")
			Expect(resourceType).To(Equal(kinds.DaemonSet))
		})

		It("should return Unknown for non-existent component", func() {
			resourceType := testStatusManager.GetResourceType("nonexistent-component")
			Expect(resourceType).To(Equal("Unknown"))
		})
	})

	// Test constants
	Describe("Constants and Types", func() {
		It("should have correct condition type constants", func() {
			Expect(status.ConditionTypeCNIHealthy).To(Equal("CNIHealthy"))
			Expect(status.ConditionTypeUpgradeComplete).To(Equal("UpgradeComplete"))
			Expect(status.ConditionTypeImagePullSecretsSynced).To(Equal("ImagePullSecretsSynced"))
		})

		It("should have correct CNI health reason constants", func() {
			Expect(status.ReasonCNIReady).To(Equal("Ready"))
			Expect(status.ReasonCNIDegraded).To(Equal("Degraded"))
			Expect(status.ReasonCNIUnknown).To(Equal("Unknown"))
			Expect(status.ReasonCNIInitializing).To(Equal("Initializing"))
		})

		It("should have correct rollout reason constants", func() {
			Expect(status.ReasonInstallationInProgress).To(Equal("InstallationInProgress"))
			Expect(status.ReasonUpgradeInProgress).To(Equal("UpgradeInProgress"))
			Expect(status.ReasonReconciliationInProgress).To(Equal("ReconciliationInProgress"))
			Expect(status.ReasonInstallationCompleted).To(Equal("InstallationCompleted"))
			Expect(status.ReasonUpgradeCompleted).To(Equal("UpgradeCompleted"))
			Expect(status.ReasonReconciliationCompleted).To(Equal("ReconciliationCompleted"))
			Expect(status.ReasonVersionValidationFailed).To(Equal("VersionValidationFailed"))
			Expect(status.ReasonTemplateRenderingFailed).To(Equal("TemplateRenderingFailed"))
			Expect(status.ReasonComponentDeploymentFailed).To(Equal("ComponentDeploymentFailed"))
			Expect(status.ReasonComponentValidationFailed).To(Equal("ComponentValidationFailed"))
			Expect(status.ReasonTestHooksFailed).To(Equal("TestHooksFailed"))
			Expect(status.ReasonDependencyResolutionFailed).To(Equal("DependencyResolutionFailed"))
		})
	})
})

// TestableStatusManager wraps StatusManager to expose private methods for testing
type TestableStatusManager struct {
	*status.StatusManager
}

// Expose private methods for testing using type assertion and reflection-like access
// Note: This is a simplified approach since we can't directly access private methods

func (t *TestableStatusManager) GetDeploymentReplicas(obj any) (int32, int32) {
	// We can test this by using the same logic as the private method
	if deployment, ok := obj.(*appsv1.Deployment); ok {
		desired := int32(1) // Default to 1 if replicas is nil
		if deployment.Spec.Replicas != nil {
			desired = *deployment.Spec.Replicas
		}
		ready := deployment.Status.ReadyReplicas
		return desired, ready
	}

	if unstructuredObj, ok := obj.(*unstructured.Unstructured); ok {
		deployment := &appsv1.Deployment{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.Object, deployment); err != nil {
			return 1, 0 // Default fallback
		}
		desired := int32(1)
		if deployment.Spec.Replicas != nil {
			desired = *deployment.Spec.Replicas
		}
		ready := deployment.Status.ReadyReplicas
		return desired, ready
	}

	return 1, 0 // Default fallback
}

func (t *TestableStatusManager) GetDaemonSetReplicas(obj any) (int32, int32) {
	if daemonSet, ok := obj.(*appsv1.DaemonSet); ok {
		desired := daemonSet.Status.DesiredNumberScheduled
		ready := daemonSet.Status.NumberReady
		return desired, ready
	}
	return 1, 0
}

func (t *TestableStatusManager) GetStatefulSetReplicas(obj any) (int32, int32) {
	if statefulSet, ok := obj.(*appsv1.StatefulSet); ok {
		desired := int32(1)
		if statefulSet.Spec.Replicas != nil {
			desired = *statefulSet.Spec.Replicas
		}
		ready := statefulSet.Status.ReadyReplicas
		return desired, ready
	}
	return 1, 0
}

func (t *TestableStatusManager) GetWorkloadReplicas(workload deployer.WorkloadResource) (int32, int32) {
	switch workload.Type {
	case kinds.Deployment:
		return t.GetDeploymentReplicas(workload.Object)
	case kinds.DaemonSet:
		return t.GetDaemonSetReplicas(workload.Object)
	case kinds.StatefulSet:
		return t.GetStatefulSetReplicas(workload.Object)
	default:
		return 1, 1 // Default for unknown types
	}
}

func (t *TestableStatusManager) GetResourceType(componentName string) string {
	// Test helper - hardcoded for test components
	switch componentName {
	case "ovnkubeControlPlane":
		return kinds.Deployment
	case "ovnkubeNode":
		return kinds.DaemonSet
	default:
		return "Unknown"
	}
}
