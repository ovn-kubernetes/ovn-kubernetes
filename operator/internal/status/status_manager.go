// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package status

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/defaults"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/deployer"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/kinds"
)

// Standard condition types for OVNK8sOperator
const (
	// ConditionTypeCNIHealthy indicates the operational health of the CNI system
	ConditionTypeCNIHealthy = "CNIHealthy"

	// ConditionTypeUpgradeComplete indicates whether the release rollout completed.
	ConditionTypeUpgradeComplete = "UpgradeComplete"

	// ConditionTypeImagePullSecretsSynced indicates whether the controller
	// has successfully propagated image-pull Secrets to the OVN-Kubernetes
	// namespace on the latest reconcile.
	ConditionTypeImagePullSecretsSynced = "ImagePullSecretsSynced"
)

// CNI Health condition reasons
const (
	// ReasonCNIReady indicates CNI is fully operational
	ReasonCNIReady = "Ready"

	// ReasonCNIDegraded indicates CNI is partially working, some components failed
	ReasonCNIDegraded = "Degraded"

	// ReasonCNIUnknown indicates CNI health status cannot be determined
	ReasonCNIUnknown = "Unknown"

	// ReasonCNIInitializing indicates that the first installation is in progress.
	ReasonCNIInitializing = "Initializing"
)

// Upgrade condition reasons
const (
	// ReasonInstallationInProgress indicates that the first installation is running.
	ReasonInstallationInProgress = "InstallationInProgress"

	// ReasonUpgradeInProgress indicates that a release upgrade is running.
	ReasonUpgradeInProgress = "UpgradeInProgress"

	// ReasonReconciliationInProgress indicates recovery of the completed release.
	ReasonReconciliationInProgress = "ReconciliationInProgress"

	// ReasonInstallationCompleted indicates fresh installation completed successfully
	ReasonInstallationCompleted = "InstallationCompleted"

	// ReasonUpgradeCompleted indicates version upgrade completed successfully
	ReasonUpgradeCompleted = "UpgradeCompleted"

	// ReasonReconciliationCompleted indicates recovery reconciliation completed.
	ReasonReconciliationCompleted = "ReconciliationCompleted"

	// ReasonVersionValidationFailed indicates version constraints violated
	ReasonVersionValidationFailed = "VersionValidationFailed"

	// ReasonTemplateRenderingFailed indicates template processing failed
	ReasonTemplateRenderingFailed = "TemplateRenderingFailed"

	// ReasonComponentDeploymentFailed indicates component deployment failed
	ReasonComponentDeploymentFailed = "ComponentDeploymentFailed"

	// ReasonComponentValidationFailed indicates component validation failed
	ReasonComponentValidationFailed = "ComponentValidationFailed"

	// ReasonTestHooksFailed indicates post-deployment test hooks failed
	ReasonTestHooksFailed = "TestHooksFailed"

	// ReasonDependencyResolutionFailed indicates dependency calculation failed
	ReasonDependencyResolutionFailed = "DependencyResolutionFailed"
)

// Image-pull secret sync condition reasons
const (
	// ReasonImagePullSecretsSynced indicates every listed secret is present
	// in the OVN-Kubernetes namespace with up-to-date data.
	ReasonImagePullSecretsSynced = "Synced"

	// ReasonImagePullSecretsSkipped indicates propagation is disabled
	// (spec.global.propagateImagePullSecrets=false) and the admin manages
	// Secret placement in the workload namespace out-of-band.
	ReasonImagePullSecretsSkipped = "SkippedByUser"

	// ReasonImagePullSecretSyncFailed indicates the controller could not
	// sync one or more image-pull secrets on the latest reconcile.
	ReasonImagePullSecretSyncFailed = "SyncFailed"
)

// StatusManager handles all status updates for the OVNK8sOperator
type StatusManager struct {
	client.Client
	embeddedConfig *defaults.EmbeddedConfig
}

// NewStatusManager creates a new StatusManager
func NewStatusManager(k8sClient client.Client, embeddedConfig *defaults.EmbeddedConfig) *StatusManager {
	return &StatusManager{
		Client:         k8sClient,
		embeddedConfig: embeddedConfig,
	}
}

// UpdateComponentDeploying marks a component as deploying
func (s *StatusManager) UpdateComponentDeploying(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, componentName string) error {
	logger := log.FromContext(ctx).WithValues("component", componentName)
	logger.V(1).Info("Updating component status", "phase", "deploying")

	return s.updateComponentStatus(ctx, ovnk8sOperator, componentName, k8sv1alpha1.ComponentStatus{
		ResourceName:   componentName,
		ResourceType:   s.getResourceType(componentName),
		Phase:          k8sv1alpha1.ComponentPhaseDeploying,
		Ready:          false,
		Message:        "Component deployment in progress",
		Error:          "",
		LastUpdateTime: &metav1.Time{Time: time.Now()},
	})
}

// UpdateComponentReady marks a component as ready after successful validation
func (s *StatusManager) UpdateComponentReady(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, componentName string, workloads []deployer.WorkloadResource) error {
	logger := log.FromContext(ctx).WithValues("component", componentName)

	var totalReplicas, readyReplicas int32
	for _, workload := range workloads {
		replicas, ready := s.getWorkloadReplicas(workload)
		totalReplicas += replicas
		readyReplicas += ready
	}

	logger.Info("Component ready",
		"workloadCount", len(workloads),
		"totalReplicas", totalReplicas,
		"readyReplicas", readyReplicas)

	return s.updateComponentStatus(ctx, ovnk8sOperator, componentName, k8sv1alpha1.ComponentStatus{
		ResourceName:   componentName,
		ResourceType:   s.getResourceType(componentName),
		Phase:          k8sv1alpha1.ComponentPhaseReady,
		Ready:          true,
		Replicas:       totalReplicas,
		ReadyReplicas:  readyReplicas,
		Message:        fmt.Sprintf("Component is ready with %d/%d replicas", readyReplicas, totalReplicas),
		Error:          "",
		LastUpdateTime: &metav1.Time{Time: time.Now()},
	})
}

// UpdateComponentFailed marks a component as failed with error details
func (s *StatusManager) UpdateComponentFailed(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, componentName string, err error) error {
	logger := log.FromContext(ctx).WithValues("component", componentName)
	logger.Error(err, "Component failed")

	return s.updateComponentStatus(ctx, ovnk8sOperator, componentName, k8sv1alpha1.ComponentStatus{
		ResourceName:   componentName,
		ResourceType:   s.getResourceType(componentName),
		Phase:          k8sv1alpha1.ComponentPhaseFailed,
		Ready:          false,
		Message:        fmt.Sprintf("Component deployment failed: %s", err.Error()),
		Error:          err.Error(),
		LastUpdateTime: &metav1.Time{Time: time.Now()},
	})
}

// ========================================
// CNI Health Status Management
// ========================================

// UpdateCNIHealthy sets CNI status to healthy (Ready)
func (s *StatusManager) UpdateCNIHealthy(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, message string) error {
	logger := log.FromContext(ctx).WithValues("ovnk8sOperator", ovnk8sOperator.Name)
	logger.V(1).Info("Updating CNI health status", "status", "healthy")

	condition := metav1.Condition{
		Type:    ConditionTypeCNIHealthy,
		Status:  metav1.ConditionTrue,
		Reason:  ReasonCNIReady,
		Message: message,
	}

	return s.updateOverallStatus(ctx, ovnk8sOperator, []metav1.Condition{condition})
}

// UpdateCNIDegraded sets CNI status to degraded (some components failed)
func (s *StatusManager) UpdateCNIDegraded(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, message string) error {
	logger := log.FromContext(ctx).WithValues("ovnk8sOperator", ovnk8sOperator.Name)
	logger.Error(nil, "CNI degraded", "message", message)

	condition := metav1.Condition{
		Type:    ConditionTypeCNIHealthy,
		Status:  metav1.ConditionFalse,
		Reason:  ReasonCNIDegraded,
		Message: message,
	}

	return s.updateOverallStatus(ctx, ovnk8sOperator, []metav1.Condition{condition})
}

// UpdateCNIUnknown sets CNI status to unknown (health cannot be determined)
func (s *StatusManager) UpdateCNIUnknown(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, message string) error {
	logger := log.FromContext(ctx).WithValues("ovnk8sOperator", ovnk8sOperator.Name)
	logger.V(1).Info("CNI health unknown", "message", message)

	condition := metav1.Condition{
		Type:    ConditionTypeCNIHealthy,
		Status:  metav1.ConditionUnknown,
		Reason:  ReasonCNIUnknown,
		Message: message,
	}

	return s.updateOverallStatus(ctx, ovnk8sOperator, []metav1.Condition{condition})
}

// ========================================
// Rollout Status Management
// ========================================

// UpdateRolloutInProgress records the attempted release without changing the
// last completed version. A fresh installation also initializes CNI health.
func (s *StatusManager) UpdateRolloutInProgress(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, targetVersion, reason, message string) error {
	logger := log.FromContext(ctx).WithValues("ovnk8sOperator", ovnk8sOperator.Name)
	logger.V(1).Info("Rollout in progress", "targetVersion", targetVersion, "reason", reason, "message", message)

	conditions := []metav1.Condition{{
		Type:    ConditionTypeUpgradeComplete,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	}}
	if ovnk8sOperator.Status.Version == "" && apimeta.FindStatusCondition(ovnk8sOperator.Status.Conditions, ConditionTypeCNIHealthy) == nil {
		conditions = append(conditions, metav1.Condition{
			Type:    ConditionTypeCNIHealthy,
			Status:  metav1.ConditionUnknown,
			Reason:  ReasonCNIInitializing,
			Message: "OVN-Kubernetes installation is in progress",
		})
	}

	return s.updateOverallStatus(ctx, ovnk8sOperator, conditions, func(status *k8sv1alpha1.OVNK8sOperatorStatus) {
		status.TargetVersion = targetVersion
		status.UpgradeStatus = k8sv1alpha1.UpgradeStatusInProgress
	})
}

// UpdateRolloutComplete atomically records a completed release and healthy CNI.
func (s *StatusManager) UpdateRolloutComplete(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, targetVersion, reason, message, cniMessage string) error {
	logger := log.FromContext(ctx).WithValues("ovnk8sOperator", ovnk8sOperator.Name, "version", targetVersion)
	logger.Info("Rollout complete", "reason", reason)

	conditions := []metav1.Condition{
		{
			Type:    ConditionTypeUpgradeComplete,
			Status:  metav1.ConditionTrue,
			Reason:  reason,
			Message: message,
		},
		{
			Type:    ConditionTypeCNIHealthy,
			Status:  metav1.ConditionTrue,
			Reason:  ReasonCNIReady,
			Message: cniMessage,
		},
	}

	return s.updateOverallStatus(ctx, ovnk8sOperator, conditions, func(status *k8sv1alpha1.OVNK8sOperatorStatus) {
		status.Version = targetVersion
		status.TargetVersion = targetVersion
		status.UpgradeStatus = k8sv1alpha1.UpgradeStatusCompleted
	})
}

// UpdateRolloutFailed records a failed attempt while preserving the completed
// version and the latest CNI health observation.
func (s *StatusManager) UpdateRolloutFailed(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, targetVersion, reason, message string) error {
	return s.updateRolloutFailed(ctx, ovnk8sOperator, targetVersion, reason, message, "")
}

// UpdateRolloutFailedAndCNIDegraded records a failed attempt that affected
// deployed workloads or functional CNI validation.
func (s *StatusManager) UpdateRolloutFailedAndCNIDegraded(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, targetVersion, reason, message, cniMessage string) error {
	return s.updateRolloutFailed(ctx, ovnk8sOperator, targetVersion, reason, message, cniMessage)
}

func (s *StatusManager) updateRolloutFailed(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, targetVersion, reason, message, cniMessage string) error {
	logger := log.FromContext(ctx).WithValues("ovnk8sOperator", ovnk8sOperator.Name)
	logger.Error(nil, "Rollout failed", "targetVersion", targetVersion, "reason", reason, "message", message)

	conditions := []metav1.Condition{{
		Type:    ConditionTypeUpgradeComplete,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	}}
	if cniMessage != "" {
		conditions = append(conditions, metav1.Condition{
			Type:    ConditionTypeCNIHealthy,
			Status:  metav1.ConditionFalse,
			Reason:  ReasonCNIDegraded,
			Message: cniMessage,
		})
	}

	return s.updateOverallStatus(ctx, ovnk8sOperator, conditions, func(status *k8sv1alpha1.OVNK8sOperatorStatus) {
		status.TargetVersion = targetVersion
		status.UpgradeStatus = k8sv1alpha1.UpgradeStatusFailed
	})
}

// ========================================
// Image-Pull Secret Sync Status Management
// ========================================

// UpdateImagePullSecretsSynced records a successful image-pull secret
// propagation pass. Message is appended verbatim — typically the count of
// secrets synced, or a note that propagation was skipped by configuration.
func (s *StatusManager) UpdateImagePullSecretsSynced(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, reason, message string) error {
	logger := log.FromContext(ctx).WithValues("ovnk8sOperator", ovnk8sOperator.Name)
	logger.V(1).Info("Image-pull secrets synced", "reason", reason, "message", message)

	condition := metav1.Condition{
		Type:    ConditionTypeImagePullSecretsSynced,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: message,
	}

	return s.updateOverallStatus(ctx, ovnk8sOperator, []metav1.Condition{condition})
}

// UpdateImagePullSecretsFailed records a failed image-pull secret sync.
// Message should carry the underlying error for operator visibility.
func (s *StatusManager) UpdateImagePullSecretsFailed(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, message string) error {
	logger := log.FromContext(ctx).WithValues("ovnk8sOperator", ovnk8sOperator.Name)
	logger.Error(nil, "Image-pull secret sync failed", "message", message)

	condition := metav1.Condition{
		Type:    ConditionTypeImagePullSecretsSynced,
		Status:  metav1.ConditionFalse,
		Reason:  ReasonImagePullSecretSyncFailed,
		Message: message,
	}

	return s.updateOverallStatus(ctx, ovnk8sOperator, []metav1.Condition{condition})
}

// updateComponentStatus updates the status of a specific component
func (s *StatusManager) updateComponentStatus(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, componentName string, newStatus k8sv1alpha1.ComponentStatus) error {
	logger := log.FromContext(ctx)

	// Create a patch to update the component status
	patch := client.MergeFrom(ovnk8sOperator.DeepCopy())

	// Find and update the component status
	found := false
	for i, compStatus := range ovnk8sOperator.Status.ComponentStatus {
		if compStatus.ResourceName == componentName {
			ovnk8sOperator.Status.ComponentStatus[i] = newStatus
			found = true
			logger.V(2).Info("Updated existing component status entry", "component", componentName)
			break
		}
	}

	// If component not found, add it
	if !found {
		ovnk8sOperator.Status.ComponentStatus = append(ovnk8sOperator.Status.ComponentStatus, newStatus)
		logger.V(2).Info("Added new component status entry", "component", componentName)
	}

	// Update the status
	if err := s.Status().Patch(ctx, ovnk8sOperator, patch); err != nil {
		return fmt.Errorf("failed to update component status for %s: %w", componentName, err)
	}

	return nil
}

// updateOverallStatus updates the overall operator status and conditions
func (s *StatusManager) updateOverallStatus(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, conditions []metav1.Condition, statusUpdaters ...func(*k8sv1alpha1.OVNK8sOperatorStatus)) error {
	logger := log.FromContext(ctx)

	base := ovnk8sOperator.DeepCopy()
	updated := ovnk8sOperator.DeepCopy()

	// Update conditions
	for _, condition := range conditions {
		condition.ObservedGeneration = ovnk8sOperator.Generation
		apimeta.SetStatusCondition(&updated.Status.Conditions, condition)
	}

	// Apply any additional status updates
	for _, updater := range statusUpdaters {
		updater(&updated.Status)
	}

	// Log conditions being set
	if logger.V(2).Enabled() {
		for _, condition := range conditions {
			logger.V(2).Info("Setting condition",
				"type", condition.Type,
				"status", condition.Status,
				"reason", condition.Reason)
		}
	}

	if apiequality.Semantic.DeepEqual(base.Status, updated.Status) {
		return nil
	}

	// Update the status
	if err := s.Status().Patch(ctx, updated, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("failed to update overall status: %w", err)
	}
	*ovnk8sOperator = *updated

	return nil
}

// getResourceType returns the resource type for a component from embedded config.
func (s *StatusManager) getResourceType(componentName string) string {
	if s.embeddedConfig != nil {
		if comp := s.embeddedConfig.GetComponent(componentName); comp != nil {
			return comp.ResourceType
		}
	}
	return "Unknown"
}

// getWorkloadReplicas extracts replica information from workload resources
func (s *StatusManager) getWorkloadReplicas(workload deployer.WorkloadResource) (int32, int32) {
	switch workload.Type {
	case kinds.Deployment:
		return s.getDeploymentReplicas(workload.Object)
	case kinds.DaemonSet:
		return s.getDaemonSetReplicas(workload.Object)
	case kinds.StatefulSet:
		return s.getStatefulSetReplicas(workload.Object)
	default:
		// For unknown workload types, assume 1 replica
		return 1, 1
	}
}

// getDeploymentReplicas extracts replica information from a Deployment
func (s *StatusManager) getDeploymentReplicas(obj client.Object) (int32, int32) {
	deployment, err := s.convertToDeployment(obj)
	if err != nil {
		return 1, 0 // Default to 1 desired, 0 ready if conversion fails
	}

	desired := int32(1) // Default to 1 if replicas is nil
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}

	ready := deployment.Status.ReadyReplicas
	return desired, ready
}

// getDaemonSetReplicas extracts replica information from a DaemonSet
func (s *StatusManager) getDaemonSetReplicas(obj client.Object) (int32, int32) {
	daemonSet, err := s.convertToDaemonSet(obj)
	if err != nil {
		return 1, 0 // Default to 1 desired, 0 ready if conversion fails
	}

	desired := daemonSet.Status.DesiredNumberScheduled
	ready := daemonSet.Status.NumberReady
	return desired, ready
}

// getStatefulSetReplicas extracts replica information from a StatefulSet
func (s *StatusManager) getStatefulSetReplicas(obj client.Object) (int32, int32) {
	statefulSet, err := s.convertToStatefulSet(obj)
	if err != nil {
		return 1, 0 // Default to 1 desired, 0 ready if conversion fails
	}

	desired := int32(1) // Default to 1 if replicas is nil
	if statefulSet.Spec.Replicas != nil {
		desired = *statefulSet.Spec.Replicas
	}

	ready := statefulSet.Status.ReadyReplicas
	return desired, ready
}

// convertToDeployment converts a client.Object to a Deployment
func (s *StatusManager) convertToDeployment(obj client.Object) (*appsv1.Deployment, error) {
	if unstructuredObj, ok := obj.(*unstructured.Unstructured); ok {
		deployment := &appsv1.Deployment{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.Object, deployment); err != nil {
			return nil, fmt.Errorf("failed to convert unstructured to Deployment: %w", err)
		}
		return deployment, nil
	}

	if deployment, ok := obj.(*appsv1.Deployment); ok {
		return deployment, nil
	}

	return nil, fmt.Errorf("expected Deployment or Unstructured, got %T", obj)
}

// convertToDaemonSet converts a client.Object to a DaemonSet
func (s *StatusManager) convertToDaemonSet(obj client.Object) (*appsv1.DaemonSet, error) {
	if unstructuredObj, ok := obj.(*unstructured.Unstructured); ok {
		daemonSet := &appsv1.DaemonSet{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.Object, daemonSet); err != nil {
			return nil, fmt.Errorf("failed to convert unstructured to DaemonSet: %w", err)
		}
		return daemonSet, nil
	}

	if daemonSet, ok := obj.(*appsv1.DaemonSet); ok {
		return daemonSet, nil
	}

	return nil, fmt.Errorf("expected DaemonSet or Unstructured, got %T", obj)
}

// convertToStatefulSet converts a client.Object to a StatefulSet
func (s *StatusManager) convertToStatefulSet(obj client.Object) (*appsv1.StatefulSet, error) {
	if unstructuredObj, ok := obj.(*unstructured.Unstructured); ok {
		statefulSet := &appsv1.StatefulSet{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.Object, statefulSet); err != nil {
			return nil, fmt.Errorf("failed to convert unstructured to StatefulSet: %w", err)
		}
		return statefulSet, nil
	}

	if statefulSet, ok := obj.(*appsv1.StatefulSet); ok {
		return statefulSet, nil
	}

	return nil, fmt.Errorf("expected StatefulSet or Unstructured, got %T", obj)
}
