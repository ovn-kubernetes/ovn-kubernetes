// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/defaults"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/dependency"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/deployer"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/renderer"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/status"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/testhook"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/validator"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/webhookcert"
)

const (
	TemplatesDir            = "./templates"
	TestHooksDir            = "./test-hooks"
	OVNK8sOperatorFinalizer = "ovnk8s-operator.ovn.org/cleanup"
	globalComponentName     = "global"
)

// OVNK8sOperatorReconciler reconciles a OVNK8sOperator object
type OVNK8sOperatorReconciler struct {
	client.Client
	Scheme             *runtime.Scheme
	EmbeddedConfig     *defaults.EmbeddedConfig
	TemplateRenderer   *renderer.TemplateRenderer
	ResourceDeployer   *deployer.ResourceDeployer
	ComponentValidator *validator.ComponentValidator
	StatusManager      *status.StatusManager
	TestHookRunner     *testhook.TestHookRunner
	ValidationTimeout  time.Duration
	SkipFinalizers     bool // Skip finalizer logic for unit testing
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *OVNK8sOperatorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues(
		"ovnk8sOperator", req.Name,
		"namespace", req.Namespace)

	// Fetch the OVNK8sOperator instance
	ovnk8sOperator, err := r.fetchOVNK8sOperator(ctx, req)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("OVNK8sOperator resource not found, ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		logger.Error(err, "Failed to get OVNK8sOperator")
		return ctrl.Result{}, err
	}

	// Skip finalizer logic if configured (useful for testing)
	if !r.SkipFinalizers {
		// Check if the OVNK8sOperator is being deleted
		if !ovnk8sOperator.ObjectMeta.DeletionTimestamp.IsZero() {
			return r.handleDeletion(ctx, ovnk8sOperator)
		}

		// Ensure finalizer is added
		if !controllerutil.ContainsFinalizer(ovnk8sOperator, OVNK8sOperatorFinalizer) {
			logger.V(1).Info("Adding finalizer to OVNK8sOperator")
			controllerutil.AddFinalizer(ovnk8sOperator, OVNK8sOperatorFinalizer)
			if err := r.Update(ctx, ovnk8sOperator); err != nil {
				logger.Error(err, "Failed to add finalizer")
				return ctrl.Result{}, err
			}
			// Requeue to ensure we have the updated object
			return ctrl.Result{Requeue: true}, nil
		}
	}

	logger.Info("Reconciling OVNK8sOperator",
		"version", r.EmbeddedConfig.Version,
		"components", len(r.EmbeddedConfig.Components))

	// Check release constraints. Same-version reconciliations still run to heal
	// drift, but they do not start a new rollout transition.
	action, err := r.checkVersionAction(ctx, ovnk8sOperator)
	operation := classifyRolloutOperation(ovnk8sOperator, r.EmbeddedConfig.Version, action)
	logger = logger.WithValues("operation", operation.String())

	if action == versionActionBlock {
		logger.Error(err, "Upgrade blocked due to version constraints")

		r.recordRolloutFailed(ctx, ovnk8sOperator, operation,
			status.ReasonVersionValidationFailed,
			fmt.Sprintf("Version validation failed: %s", err.Error()))

		// Don't requeue - user needs to fix the version conflict
		return ctrl.Result{}, nil
	}

	logger.Info("Reconciliation classified",
		"currentVersion", ovnk8sOperator.Status.Version,
		"targetVersion", r.EmbeddedConfig.Version)

	if operation.isRollout() {
		reason, message := rolloutStartStatus(operation, ovnk8sOperator, r.EmbeddedConfig.Version)
		if err := r.StatusManager.UpdateRolloutInProgress(ctx, ovnk8sOperator,
			r.EmbeddedConfig.Version, reason, message); err != nil {
			logger.Error(err, "Failed to update rollout status")
			// Continue with deployment even if status update fails.
		}
	}

	// Render templates for all components
	componentManifests, err := r.renderComponentTemplates(ctx, ovnk8sOperator)
	if err != nil {
		logger.Error(err, "Failed to render component templates")

		// Update upgrade status to failed with template rendering error, does not affect CNI health.
		r.recordRolloutFailed(ctx, ovnk8sOperator, operation,
			status.ReasonTemplateRenderingFailed,
			fmt.Sprintf("Template rendering failed: %s", err.Error()))

		return ctrl.Result{}, err
	}

	// Apply user-defined patches to rendered manifests
	if err := renderer.ApplyPatches(componentManifests, ovnk8sOperator.Spec.ComponentOverrides); err != nil {
		logger.Error(err, "Failed to apply patches to rendered manifests")

		r.recordRolloutFailed(ctx, ovnk8sOperator, operation,
			status.ReasonTemplateRenderingFailed,
			fmt.Sprintf("Patch application failed: %s", err.Error()))

		return ctrl.Result{}, err
	}

	// Propagate image-pull Secrets from the operator namespace to the
	// OVN-Kubernetes namespace before deploying, so pods find the Secrets
	// they reference on first schedule.
	if err := r.SyncImagePullSecrets(ctx, ovnk8sOperator); err != nil {
		logger.Error(err, "Failed to propagate image-pull secrets")

		if statusErr := r.StatusManager.UpdateImagePullSecretsFailed(ctx, ovnk8sOperator, err.Error()); statusErr != nil {
			logger.Error(statusErr, "Failed to update ImagePullSecretsSynced status")
		}
		r.recordRolloutFailed(ctx, ovnk8sOperator, operation,
			status.ReasonImagePullSecretSyncFailed,
			fmt.Sprintf("Image-pull secret propagation failed: %s", err.Error()))
		return ctrl.Result{}, err
	}
	if statusErr := r.imagePullSecretsSyncedStatus(ctx, ovnk8sOperator); statusErr != nil {
		logger.Error(statusErr, "Failed to update ImagePullSecretsSynced status")
	}

	// Deploy and validate all components in dependency order
	logger.V(1).Info("Starting dependency-based component deployment")
	if err := r.deployAndValidateComponents(ctx, ovnk8sOperator, componentManifests, operation); err != nil {
		logger.Error(err, "Failed to deploy and validate components")
		// Status is already updated in deployAndValidateComponents
		return ctrl.Result{}, err
	}

	// Execute test hooks after all components are deployed and validated (if enabled)
	if ovnk8sOperator.Spec.TestHooks != nil && ovnk8sOperator.Spec.TestHooks.Enabled {
		logger.V(1).Info("Starting test hook execution", "enabled", ovnk8sOperator.Spec.TestHooks.Enabled)
		if err := r.TestHookRunner.RunTestHooks(ctx, ovnk8sOperator); err != nil {
			logger.Error(err, "Failed to execute test hooks")

			// Update upgrade status to failed with test hooks error
			r.recordRolloutFailedAndCNIDegraded(ctx, ovnk8sOperator, operation,
				status.ReasonTestHooksFailed,
				fmt.Sprintf("Test hooks execution failed: %s", err.Error()),
				fmt.Sprintf("Post-deployment tests failed: %s", err.Error()))

			return ctrl.Result{}, err
		}
	} else {
		logger.V(1).Info("Test hooks are disabled, skipping execution")
	}

	const cniReadyMessage = "All CNI components are ready and operational"
	if operation.isRollout() {
		reason, message := rolloutCompleteStatus(operation, r.EmbeddedConfig.Version)
		if err := r.StatusManager.UpdateRolloutComplete(ctx, ovnk8sOperator,
			r.EmbeddedConfig.Version, reason, message, cniReadyMessage); err != nil {
			logger.Error(err, "Failed to update rollout status")
			return ctrl.Result{}, err
		}
	} else if err := r.StatusManager.UpdateCNIHealthy(ctx, ovnk8sOperator, cniReadyMessage); err != nil {
		logger.Error(err, "Failed to update CNI health status")
		return ctrl.Result{}, err
	}

	logger.Info("OVNK8sOperator reconciliation completed successfully")
	return ctrl.Result{}, nil
}

// fetchOVNK8sOperator retrieves the OVNK8sOperator resource
func (r *OVNK8sOperatorReconciler) fetchOVNK8sOperator(ctx context.Context, req ctrl.Request) (*k8sv1alpha1.OVNK8sOperator, error) {
	var ovnk8sOperator k8sv1alpha1.OVNK8sOperator
	if err := r.Get(ctx, req.NamespacedName, &ovnk8sOperator); err != nil {
		return nil, err
	}
	return &ovnk8sOperator, nil
}

func rolloutStartStatus(operation rolloutOperation, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, targetVersion string) (string, string) {
	switch operation {
	case rolloutOperationInstallation:
		return status.ReasonInstallationInProgress,
			fmt.Sprintf("Installing OVN-Kubernetes release %s", targetVersion)
	case rolloutOperationUpgrade:
		return status.ReasonUpgradeInProgress,
			fmt.Sprintf("Upgrading OVN-Kubernetes from %s to %s", ovnk8sOperator.Status.Version, targetVersion)
	case rolloutOperationRecovery:
		if previousTarget := ovnk8sOperator.Status.TargetVersion; previousTarget != "" && previousTarget != targetVersion {
			return status.ReasonReconciliationInProgress,
				fmt.Sprintf("Recovering OVN-Kubernetes release %s after failed rollout to %s", targetVersion, previousTarget)
		}
		return status.ReasonReconciliationInProgress,
			fmt.Sprintf("Reconciling OVN-Kubernetes release %s after a failed rollout", targetVersion)
	default:
		return "", ""
	}
}

func rolloutCompleteStatus(operation rolloutOperation, targetVersion string) (string, string) {
	switch operation {
	case rolloutOperationInstallation:
		return status.ReasonInstallationCompleted, "Fresh installation completed successfully"
	case rolloutOperationUpgrade:
		return status.ReasonUpgradeCompleted,
			fmt.Sprintf("Upgraded to OVN-Kubernetes release %s", targetVersion)
	case rolloutOperationRecovery:
		return status.ReasonReconciliationCompleted,
			fmt.Sprintf("Recovered OVN-Kubernetes release %s", targetVersion)
	default:
		return "", ""
	}
}

// recordRolloutFailed records an active or blocked rollout failure on a
// best-effort basis. A steady reconciliation does not rewrite rollout state.
func (r *OVNK8sOperatorReconciler) recordRolloutFailed(ctx context.Context, op *k8sv1alpha1.OVNK8sOperator, operation rolloutOperation, reason, message string) {
	if operation == rolloutOperationSteady {
		return
	}
	if err := r.StatusManager.UpdateRolloutFailed(ctx, op, r.EmbeddedConfig.Version, reason, message); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update rollout status")
	}
}

// recordRolloutFailedAndCNIDegraded atomically records failures that reached
// deployed workloads. A steady reconciliation only changes CNI health.
func (r *OVNK8sOperatorReconciler) recordRolloutFailedAndCNIDegraded(ctx context.Context, op *k8sv1alpha1.OVNK8sOperator, operation rolloutOperation, reason, message, cniMessage string) {
	var err error
	if operation.isRollout() {
		err = r.StatusManager.UpdateRolloutFailedAndCNIDegraded(
			ctx, op, r.EmbeddedConfig.Version, reason, message, cniMessage)
	} else {
		err = r.StatusManager.UpdateCNIDegraded(ctx, op, cniMessage)
	}
	if err != nil {
		log.FromContext(ctx).Error(err, "Failed to update rollout and CNI status")
	}
}

// renderComponentTemplates renders all templates for all components
func (r *OVNK8sOperatorReconciler) renderComponentTemplates(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator) (renderer.ComponentManifests, error) {
	logger := log.FromContext(ctx)

	opts := renderer.RenderOptions{}
	resolved, err := r.EmbeddedConfig.ResolveComponents(ovnk8sOperator)
	if err != nil {
		return nil, fmt.Errorf("resolve components: %w", err)
	}
	identityEnabled := false
	for _, comp := range resolved {
		if comp.ResourceName == k8sv1alpha1.ComponentOvnkubeIdentity {
			identityEnabled = true
			break
		}
	}
	if identityEnabled {
		caBundle, err := webhookcert.EnsureWebhookCert(ctx, r.Client, ovnk8sOperator.Spec.Global.Namespace, ovnk8sOperator.Name)
		if err != nil {
			logger.Error(err, "Failed to ensure ovnkube-identity webhook certificate")
			return nil, fmt.Errorf("ensure webhook cert: %w", err)
		}
		opts.WebhookCABundle = string(caBundle)
	}

	componentManifests, err := r.TemplateRenderer.RenderAllTemplates(ovnk8sOperator, opts)
	if err != nil {
		logger.Error(err, "Failed to render component templates")
		return nil, fmt.Errorf("failed to render templates: %w", err)
	}

	// Log rendering summary at debug level
	totalManifests := 0
	for _, manifests := range componentManifests {
		totalManifests += len(manifests)
	}
	logger.V(1).Info("Template rendering completed",
		"componentCount", len(componentManifests),
		"totalManifests", totalManifests)

	return componentManifests, nil
}

// deployAndValidateComponents deploys and validates all component manifests in dependency order
func (r *OVNK8sOperatorReconciler) deployAndValidateComponents(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, componentManifests renderer.ComponentManifests, operation rolloutOperation) error {
	logger := log.FromContext(ctx)

	// Get the upgrade plan based on component dependencies from embedded config
	upgradeOrder, err := getUpgradePlan(ctx, ovnk8sOperator, r.EmbeddedConfig)
	if err != nil {
		logger.Error(err, "Failed to calculate upgrade plan")

		r.recordRolloutFailed(ctx, ovnk8sOperator, operation,
			status.ReasonDependencyResolutionFailed,
			fmt.Sprintf("Dependency resolution failed: %s", err.Error()))

		return fmt.Errorf("failed to get upgrade plan: %w", err)
	}

	// Process each component in the calculated upgrade order
	for _, componentName := range upgradeOrder {
		manifests, exists := componentManifests[componentName]
		if !exists {
			// Component has no manifests - this is OK, not all components need resources
			logger.V(1).Info("No manifests found for component, skipping deployment",
				"component", componentName)

			// Update real component status to ready with no workloads. "global" is
			// a deployment grouping and is intentionally not user-facing status.
			if componentName != globalComponentName {
				if err := r.StatusManager.UpdateComponentReady(ctx, ovnk8sOperator, componentName, []deployer.WorkloadResource{}); err != nil {
					logger.Error(err, "Failed to update component status", "component", componentName)
				}
			}
			continue
		}

		if err := r.deployComponent(ctx, componentName, manifests, ovnk8sOperator, operation); err != nil {
			return fmt.Errorf("failed to deploy component %s: %w", componentName, err)
		}
	}

	return nil
}

// deployComponent deploys a single component's manifests and validates workloads
func (r *OVNK8sOperatorReconciler) deployComponent(ctx context.Context, componentName string, manifests []*renderer.RenderedManifest, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, operation rolloutOperation) error {
	logger := log.FromContext(ctx).WithValues("component", componentName)
	reportComponentStatus := componentName != globalComponentName

	logger.V(1).Info("Deploying component", "manifestCount", len(manifests))

	// Update component status to deploying
	if reportComponentStatus {
		if err := r.StatusManager.UpdateComponentDeploying(ctx, ovnk8sOperator, componentName); err != nil {
			logger.Error(err, "Failed to update component status to deploying")
			// Continue with deployment even if status update fails
		}
	}

	// Log manifests at trace level
	if logger.V(2).Enabled() {
		r.logComponentManifests(ctx, componentName, manifests)
	}

	// Deploy component manifests and get workload resources
	workloads, err := r.ResourceDeployer.DeployComponentManifests(ctx, componentName, manifests, ovnk8sOperator)
	if err != nil {
		logger.Error(err, "Failed to deploy component")
		if reportComponentStatus {
			if statusErr := r.StatusManager.UpdateComponentFailed(ctx, ovnk8sOperator, componentName, err); statusErr != nil {
				logger.Error(statusErr, "Failed to update component status to failed")
			}
		}
		r.recordRolloutFailedAndCNIDegraded(ctx, ovnk8sOperator, operation,
			status.ReasonComponentDeploymentFailed,
			fmt.Sprintf("Component %s deployment failed: %s", componentName, err.Error()),
			fmt.Sprintf("Component %s failed: %s", componentName, err.Error()))
		return err
	}

	// Log workload summary at debug level
	if len(workloads) > 0 {
		logger.V(1).Info("Component workloads deployed",
			"workloadCount", len(workloads))
		if logger.V(2).Enabled() {
			for i, workload := range workloads {
				logger.V(2).Info("Deployed workload",
					"index", i+1,
					"type", workload.Type,
					"name", workload.Name)
			}
		}
	}

	// Validate workload resources
	if err := r.validateWorkloads(ctx, componentName, workloads, ovnk8sOperator, operation); err != nil {
		return fmt.Errorf("validation failed for component %s: %w", componentName, err)
	}

	// Update component status to ready after successful validation
	if reportComponentStatus {
		if err := r.StatusManager.UpdateComponentReady(ctx, ovnk8sOperator, componentName, workloads); err != nil {
			logger.Error(err, "Failed to update component status to ready")
			// Continue as the component is actually ready
		}
	}

	logger.Info("Component deployed successfully")
	return nil
}

// validateWorkloads validates all workload resources for a component
func (r *OVNK8sOperatorReconciler) validateWorkloads(ctx context.Context, componentName string, workloads []deployer.WorkloadResource, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, operation rolloutOperation) error {
	if len(workloads) == 0 {
		return nil // No workloads to validate
	}

	logger := log.FromContext(ctx).WithValues("component", componentName)
	logger.V(1).Info("Starting workload validation", "workloadCount", len(workloads))

	// Validate component workloads
	if err := r.ComponentValidator.ValidateComponent(ctx, componentName, workloads); err != nil {
		logger.Error(err, "Workload validation failed")

		// Update component status to failed
		if statusErr := r.StatusManager.UpdateComponentFailed(ctx, ovnk8sOperator, componentName, err); statusErr != nil {
			logger.Error(statusErr, "Failed to update component status to failed")
		}

		r.recordRolloutFailedAndCNIDegraded(ctx, ovnk8sOperator, operation,
			status.ReasonComponentValidationFailed,
			fmt.Sprintf("Component %s validation failed: %s", componentName, err.Error()),
			fmt.Sprintf("Component %s validation failed: %s", componentName, err.Error()))

		return err
	}

	logger.V(1).Info("Workload validation completed successfully")
	return nil
}

// logComponentManifests logs all manifests for a component at trace level
func (r *OVNK8sOperatorReconciler) logComponentManifests(ctx context.Context, componentName string, manifests []*renderer.RenderedManifest) {
	logger := log.FromContext(ctx)

	for i, manifest := range manifests {
		logger.V(2).Info("Component manifest",
			"component", componentName,
			"index", i+1,
			"fileName", manifest.FileName,
			"templatePath", manifest.TemplatePath,
			"contentLength", len(manifest.Content))
	}
}

// buildComponentDAG builds a dependency DAG from a resolved component list.
// Caller passes the result of EmbeddedConfig.ResolveComponents(cr) so the DAG
// only spans components actually in scope for this CR's mode + enabled flags.
func buildComponentDAG(components []defaults.EmbeddedComponent) (*dependency.DAG, error) {
	present := make(map[string]bool, len(components))
	for _, comp := range components {
		present[comp.ResourceName] = true
	}
	dagInstance := dependency.NewDAG()
	for _, comp := range components {
		// Drop dependsOn edges to components not in the resolved set (e.g. an
		// optional sibling that is disabled), so the DAG stays valid.
		deps := make([]string, 0, len(comp.DependsOn))
		for _, dep := range comp.DependsOn {
			if present[dep] {
				deps = append(deps, dep)
			}
		}
		dagInstance.AddComponent(comp.ResourceName, deps)
	}
	return dagInstance, nil
}

// getUpgradePlan returns the deployment order of components for this CR.
// Always includes "global" as the first element. Honors the CR's deploymentMode
// and componentOverrides[].enabled flags via ResolveComponents.
func getUpgradePlan(ctx context.Context, cr *k8sv1alpha1.OVNK8sOperator, embeddedConfig *defaults.EmbeddedConfig) ([]string, error) {
	logger := log.FromContext(ctx)

	resolved, err := embeddedConfig.ResolveComponents(cr)
	if err != nil {
		return nil, fmt.Errorf("resolve components for mode %q: %w", cr.Spec.Global.DeploymentMode, err)
	}

	dagInstance, err := buildComponentDAG(resolved)
	if err != nil {
		return nil, err
	}

	componentOrder, err := dagInstance.GetUpgradeOrder()
	if err != nil {
		return nil, err
	}

	// Always prepend "global" to ensure it's deployed first
	upgradeOrder := append([]string{globalComponentName}, componentOrder...)

	// Log the upgrade plan at debug level
	logger.V(1).Info("Component deployment order calculated",
		"componentCount", len(upgradeOrder),
		"order", strings.Join(upgradeOrder, " → "))

	return upgradeOrder, nil
}

// handleDeletion handles the deletion of OVNK8sOperator resources
func (r *OVNK8sOperatorReconciler) handleDeletion(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("ovnk8sOperator", ovnk8sOperator.Name)

	// Check if our finalizer is present
	if controllerutil.ContainsFinalizer(ovnk8sOperator, OVNK8sOperatorFinalizer) {
		// Update status to indicate we're cleaning up
		if err := r.StatusManager.UpdateCNIUnknown(ctx, ovnk8sOperator, "OVNK8sOperator is being deleted, cleaning up resources"); err != nil {
			logger.Error(err, "Failed to update status")
			// Continue with cleanup even if status update fails
		}

		logger.Info("Starting resource cleanup")

		// Perform cleanup using the ResourceDeployer
		if err := r.ResourceDeployer.CleanupResources(ctx, ovnk8sOperator); err != nil {
			logger.Error(err, "Failed to cleanup resources, will retry")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		logger.Info("Resource cleanup completed, removing finalizer")

		// Remove our finalizer
		controllerutil.RemoveFinalizer(ovnk8sOperator, OVNK8sOperatorFinalizer)
		if err := r.Update(ctx, ovnk8sOperator); err != nil {
			logger.Error(err, "Failed to remove finalizer")
			return ctrl.Result{}, err
		}

		logger.Info("OVNK8sOperator cleanup completed successfully")
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *OVNK8sOperatorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Load embedded config (versions, images, dependencies) — fail fast if invalid
	if r.EmbeddedConfig == nil {
		embeddedConfig, err := defaults.LoadEmbeddedConfig()
		if err != nil {
			return fmt.Errorf("failed to load embedded config: %w", err)
		}
		r.EmbeddedConfig = embeddedConfig
	}

	// Initialize template renderer with embedded config
	if r.TemplateRenderer == nil {
		r.TemplateRenderer = renderer.NewTemplateRenderer(TemplatesDir, r.EmbeddedConfig)
	}

	// Initialize resource deployer
	if r.ResourceDeployer == nil {
		r.ResourceDeployer = deployer.NewResourceDeployer(mgr.GetClient(), mgr.GetScheme())
	}

	// Initialize component validator
	if r.ComponentValidator == nil {
		timeout := r.ValidationTimeout
		if timeout == 0 {
			timeout = 10 * time.Minute
		}
		config := &validator.ValidationConfig{
			Timeout:       timeout,
			CheckInterval: 10 * time.Second,
			MaxRetries:    30,
		}
		r.ComponentValidator = validator.NewComponentValidatorWithConfig(mgr.GetClient(), config)
	}

	// Initialize status manager with embedded config for resource type lookups
	if r.StatusManager == nil {
		r.StatusManager = status.NewStatusManager(mgr.GetClient(), r.EmbeddedConfig)
	}

	// Initialize test hook runner — shares the controller's template renderer
	if r.TestHookRunner == nil {
		r.TestHookRunner = testhook.NewTestHookRunner(mgr.GetClient(), mgr.GetScheme(), TestHooksDir, r.TemplateRenderer)
	}

	// NOTE: image-pull Secret rotations in the operator namespace are not
	// watched. Propagation re-runs on the next CR reconcile during installation,
	// upgrades or any CR changes.
	// Adding a Watch on corev1.Secret filtered to OperatorNamespace would close the
	// rotation-MTTR gap, but no current use case justifies the extra surface area.
	// Tracked in phase 2; revisit if rotationcadence becomes an operational concern.

	return ctrl.NewControllerManagedBy(mgr).
		For(&k8sv1alpha1.OVNK8sOperator{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(r)
}
