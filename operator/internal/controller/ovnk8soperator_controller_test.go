// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/defaults"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/deployer"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/kinds"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/renderer"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/status"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/testhook"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/validator"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// testEmbeddedConfig creates an EmbeddedConfig for testing with the given version.
func testEmbeddedConfig(version string) *defaults.EmbeddedConfig {
	return &defaults.EmbeddedConfig{
		Version:           version,
		MinUpgradeVersion: "",
		Modes: map[string]defaults.EmbeddedMode{
			k8sv1alpha1.DeploymentModeStandard: {Components: []string{k8sv1alpha1.ComponentOvnkubeControlPlane, k8sv1alpha1.ComponentOvnkubeNode}},
		},
		Components: []defaults.EmbeddedComponent{
			{
				ResourceName: k8sv1alpha1.ComponentOvnkubeControlPlane,
				ResourceType: kinds.Deployment,
				DependsOn:    []string{},
				ContainerImages: []defaults.EmbeddedContainerImage{
					{Name: defaults.DefaultImageName, Repository: "example.io/ovnkube", Tag: "v1.0"},
				},
				ContainerImageMapping: map[string][]string{
					defaults.DefaultImageName: {"ovnkubeClusterManager"},
				},
			},
			{
				ResourceName: k8sv1alpha1.ComponentOvnkubeNode,
				ResourceType: kinds.DaemonSet,
				DependsOn:    []string{k8sv1alpha1.ComponentOvnkubeControlPlane, k8sv1alpha1.ComponentOvnkubeIdentity},
				ContainerImages: []defaults.EmbeddedContainerImage{
					{Name: defaults.DefaultImageName, Repository: "example.io/ovnkube", Tag: "v1.0"},
				},
				ContainerImageMapping: map[string][]string{
					defaults.DefaultImageName: {"ovnkubeController", "nbOvsdb", "sbOvsdb", "ovnNorthd", "ovnController"},
				},
			},
		},
	}
}

// testCRSpec creates a minimal valid OVNK8sOperatorSpec for testing.
func testCRSpec() k8sv1alpha1.OVNK8sOperatorSpec {
	return k8sv1alpha1.OVNK8sOperatorSpec{
		Global: k8sv1alpha1.GlobalConfig{
			K8sAPIServer:   "https://10.0.0.1:6443",
			PodNetwork:     []k8sv1alpha1.PodNetworkEntry{{CIDR: "10.192.0.0/16", HostPrefix: 26}},
			ServiceNetwork: []string{"10.96.0.0/12"},
			MTU:            1440,
			DeploymentMode: k8sv1alpha1.DeploymentModeStandard,
			Namespace:      "ovn-kubernetes",
		},
	}
}

func completedOperatorStatus(generation int64) k8sv1alpha1.OVNK8sOperatorStatus {
	const version = "0.9.0"
	transitionTime := metav1.Now()
	return k8sv1alpha1.OVNK8sOperatorStatus{
		Version:       version,
		TargetVersion: version,
		UpgradeStatus: k8sv1alpha1.UpgradeStatusCompleted,
		Conditions: []metav1.Condition{
			{
				Type:               status.ConditionTypeUpgradeComplete,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: generation,
				LastTransitionTime: transitionTime,
				Reason:             status.ReasonUpgradeCompleted,
				Message:            "Previous rollout completed",
			},
			{
				Type:               status.ConditionTypeCNIHealthy,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: generation,
				LastTransitionTime: transitionTime,
				Reason:             status.ReasonCNIReady,
				Message:            "CNI is healthy",
			},
		},
	}
}

// Helper functions to create test templates and files for controlled error scenarios

func createEmptyDefaultTemplateDirs(templateDir string) {
	for _, component := range []string{
		globalComponentName,
		k8sv1alpha1.ComponentOvnkubeControlPlane,
		k8sv1alpha1.ComponentOvnkubeNode,
	} {
		Expect(os.MkdirAll(filepath.Join(templateDir, component), 0755)).To(Succeed())
	}
}

func createBadTemplateDir(tempDir string) string {
	badTemplateDir := filepath.Join(tempDir, "bad-templates")
	Expect(os.MkdirAll(badTemplateDir, 0755)).To(Succeed())

	// Create global directory with invalid template
	globalDir := filepath.Join(badTemplateDir, globalComponentName)
	Expect(os.MkdirAll(globalDir, 0755)).To(Succeed())

	// Create invalid template syntax
	invalidTemplate := `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .invalid.template.syntax {{`

	Expect(os.WriteFile(filepath.Join(globalDir, "invalid.yaml"), []byte(invalidTemplate), 0644)).To(Succeed())

	return badTemplateDir
}

func createValidTemplateDir(tempDir string) string {
	validTemplateDir := filepath.Join(tempDir, "valid-templates")
	Expect(os.MkdirAll(validTemplateDir, 0755)).To(Succeed())

	// Create global directory with valid template
	globalDir := filepath.Join(validTemplateDir, globalComponentName)
	Expect(os.MkdirAll(globalDir, 0755)).To(Succeed())

	validTemplate := `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-config
  namespace: default
data:
  version: "1.0.0"`

	Expect(os.WriteFile(filepath.Join(globalDir, "config.yaml"), []byte(validTemplate), 0644)).To(Succeed())

	// Create ovnkubeControlPlane directory with valid template
	cpDir := filepath.Join(validTemplateDir, k8sv1alpha1.ComponentOvnkubeControlPlane)
	Expect(os.MkdirAll(cpDir, 0755)).To(Succeed())

	cpTemplate := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: ovnkube-control-plane-config
  namespace: default
data:
  component: %q`, k8sv1alpha1.ComponentOvnkubeControlPlane)

	Expect(os.WriteFile(filepath.Join(cpDir, "config.yaml"), []byte(cpTemplate), 0644)).To(Succeed())

	// Create ovnkubeNode directory with valid template
	nodeDir := filepath.Join(validTemplateDir, k8sv1alpha1.ComponentOvnkubeNode)
	Expect(os.MkdirAll(nodeDir, 0755)).To(Succeed())

	nodeTemplate := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: ovnkube-node-config
  namespace: default
data:
  component: %q`, k8sv1alpha1.ComponentOvnkubeNode)

	Expect(os.WriteFile(filepath.Join(nodeDir, "config.yaml"), []byte(nodeTemplate), 0644)).To(Succeed())

	return validTemplateDir
}

const (
	testWorkloadName      = "test"
	compA                 = "comp-a"
	compB                 = "comp-b"
	modeRequiredComponent = "mode-required"
	modeOptionalComponent = "mode-optional"
	baseComponent         = "base"
	middleComponent       = "middle"

	testConfigMapManifest    = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test-config\n  namespace: default"
	ovnNodeConfigMapManifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: ovn-node-config\n  namespace: default"
	testManifestFile         = "test.yaml"
	ovnNodeManifestFile      = "ovn-node.yaml"
)

var _ = Describe("NetworkOperator Controller", func() {
	// These specs exercise the reconciler against a live apiserver (envtest).
	// Skip them when envtest was not provisioned (e.g. a direct `go test` run
	// without KUBEBUILDER_ASSETS). Both the shared `make check` and the
	// operator's `make test` provision envtest.
	BeforeEach(func() {
		if !envtestAvailable {
			Skip("envtest not available (KUBEBUILDER_ASSETS unset); skipping controller integration specs")
		}
	})

	// Note: operatorName/operatorNamespace deliberately use the production
	// singleton identity. This ensures tests exercise the same name guard
	// path as real deployments.
	const (
		operatorName      = k8sv1alpha1.SingletonName
		operatorNamespace = k8sv1alpha1.OperatorNamespace
	)

	var (
		ctx             context.Context
		reconciler      *OVNK8sOperatorReconciler
		networkOperator *k8sv1alpha1.OVNK8sOperator
		req             ctrl.Request
		tempDir         string
	)

	BeforeEach(func() {
		ctx = context.Background()

		// Create temporary directory for test templates
		var err error
		tempDir, err = os.MkdirTemp("", "controller-test-*")
		Expect(err).ToNot(HaveOccurred())

		// Create the operator namespace if it doesn't exist
		operatorNs := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: operatorNamespace,
			},
		}
		err = k8sClient.Create(ctx, operatorNs)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).ToNot(HaveOccurred())
		}

		// Create test CR with minimal required fields
		networkOperator = &k8sv1alpha1.OVNK8sOperator{
			ObjectMeta: metav1.ObjectMeta{
				Name:      operatorName,
				Namespace: operatorNamespace,
			},
			Spec: testCRSpec(),
			Status: k8sv1alpha1.OVNK8sOperatorStatus{
				Version: "",
			},
		}

		// Create request
		req = ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      operatorName,
				Namespace: operatorNamespace,
			},
		}

		// Create embedded config for tests
		embeddedConfig := testEmbeddedConfig(version1)

		// Create reconciler with real client (envtest provides this)
		reconciler = &OVNK8sOperatorReconciler{
			Client:         k8sClient,
			Scheme:         k8sClient.Scheme(),
			EmbeddedConfig: embeddedConfig,
			SkipFinalizers: true,
		}

		// Initialize controller dependencies
		reconciler.TemplateRenderer = renderer.NewTemplateRenderer(tempDir, embeddedConfig)
		reconciler.ResourceDeployer = deployer.NewResourceDeployer(k8sClient, k8sClient.Scheme())
		reconciler.ComponentValidator = validator.NewComponentValidatorWithConfig(k8sClient, &validator.ValidationConfig{
			Timeout:       5 * time.Minute,
			CheckInterval: 10 * time.Second,
			MaxRetries:    30,
		})
		reconciler.StatusManager = status.NewStatusManager(k8sClient, embeddedConfig)
		reconciler.TestHookRunner = testhook.NewTestHookRunner(k8sClient, k8sClient.Scheme(), filepath.Join(tempDir, "test-hooks"), reconciler.TemplateRenderer)
	})

	AfterEach(func() {
		if tempDir != "" {
			Expect(os.RemoveAll(tempDir)).To(Succeed())
		}
	})

	Describe("Reconcile", func() {
		Context("when NetworkOperator resource exists", func() {
			BeforeEach(func() {
				Expect(k8sClient.Create(ctx, networkOperator)).To(Succeed())
			})

			AfterEach(func() {
				Expect(k8sClient.Delete(ctx, networkOperator)).To(Succeed())
			})

			Context("with minimal setup (empty templates)", func() {
				It("should complete a fresh installation", func() {
					// Create minimal template structure with empty directories for all components
					// Don't create any files - let the template renderer handle empty directories
					createEmptyDefaultTemplateDirs(tempDir)

					result, err := reconciler.Reconcile(ctx, req)

					// The controller should handle empty templates gracefully
					// Components with no manifests are OK - they get marked as ready with no workloads
					Expect(err).ToNot(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))

					updated := &k8sv1alpha1.OVNK8sOperator{}
					Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
					Expect(updated.Status.Version).To(Equal(version1))
					Expect(updated.Status.TargetVersion).To(Equal(version1))
					Expect(updated.Status.UpgradeStatus).To(Equal(k8sv1alpha1.UpgradeStatusCompleted))
					Expect(updated.Status.Conditions).To(HaveLen(3))

					upgradeCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeUpgradeComplete)
					Expect(upgradeCondition).ToNot(BeNil())
					Expect(upgradeCondition.Status).To(Equal(metav1.ConditionTrue))
					Expect(upgradeCondition.Reason).To(Equal(status.ReasonInstallationCompleted))

					cniCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeCNIHealthy)
					Expect(cniCondition).ToNot(BeNil())
					Expect(cniCondition.Status).To(Equal(metav1.ConditionTrue))
					Expect(cniCondition.Reason).To(Equal(status.ReasonCNIReady))

					secretCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeImagePullSecretsSynced)
					Expect(secretCondition).ToNot(BeNil())
					Expect(secretCondition.Status).To(Equal(metav1.ConditionTrue))
					Expect(secretCondition.Reason).To(Equal(status.ReasonImagePullSecretsSynced))
				})
			})

			Context("with image-pull secret propagation disabled", func() {
				BeforeEach(func() {
					createEmptyDefaultTemplateDirs(tempDir)
					propagate := false
					networkOperator.Spec.Global.PropagateImagePullSecrets = &propagate
					networkOperator.Spec.Global.ImagePullSecrets = []corev1.LocalObjectReference{
						{Name: "admin-managed-secret"},
					}
					Expect(k8sClient.Update(ctx, networkOperator)).To(Succeed())
				})

				It("should report that synchronization was skipped", func() {
					result, err := reconciler.Reconcile(ctx, req)

					Expect(err).ToNot(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))

					updated := &k8sv1alpha1.OVNK8sOperator{}
					Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
					secretCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeImagePullSecretsSynced)
					Expect(secretCondition).ToNot(BeNil())
					Expect(secretCondition.Status).To(Equal(metav1.ConditionTrue))
					Expect(secretCondition.Reason).To(Equal(status.ReasonImagePullSecretsSkipped))
				})
			})

			Context("with template rendering failures", func() {
				BeforeEach(func() {
					networkOperator.Status = completedOperatorStatus(networkOperator.Generation)
					Expect(k8sClient.Status().Update(ctx, networkOperator)).To(Succeed())

					// Use template directory with invalid syntax
					badTemplateDir := createBadTemplateDir(tempDir)
					reconciler.TemplateRenderer = renderer.NewTemplateRenderer(badTemplateDir, reconciler.EmbeddedConfig)
				})

				It("should handle template rendering failures", func() {
					result, err := reconciler.Reconcile(ctx, req)

					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("failed to render templates"))
					Expect(result).To(Equal(ctrl.Result{}))

					updated := &k8sv1alpha1.OVNK8sOperator{}
					Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
					Expect(updated.Status.Version).To(Equal("0.9.0"))
					Expect(updated.Status.TargetVersion).To(Equal(version1))
					Expect(updated.Status.UpgradeStatus).To(Equal(k8sv1alpha1.UpgradeStatusFailed))
					upgradeCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeUpgradeComplete)
					Expect(upgradeCondition).ToNot(BeNil())
					Expect(upgradeCondition.Status).To(Equal(metav1.ConditionFalse))
					Expect(upgradeCondition.Reason).To(Equal(status.ReasonTemplateRenderingFailed))
					cniCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeCNIHealthy)
					Expect(cniCondition).ToNot(BeNil())
					Expect(cniCondition.Status).To(Equal(metav1.ConditionTrue))
					Expect(cniCondition.Reason).To(Equal(status.ReasonCNIReady))
				})
			})

			Context("with image-pull secret synchronization failures", func() {
				BeforeEach(func() {
					createEmptyDefaultTemplateDirs(tempDir)
					networkOperator.Spec.Global.ImagePullSecrets = []corev1.LocalObjectReference{
						{Name: "missing-pull-secret"},
					}
					Expect(k8sClient.Update(ctx, networkOperator)).To(Succeed())
				})

				It("should report the secret synchronization failure", func() {
					result, err := reconciler.Reconcile(ctx, req)

					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("source Secret"))
					Expect(result).To(Equal(ctrl.Result{}))

					updated := &k8sv1alpha1.OVNK8sOperator{}
					Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
					Expect(updated.Status.Version).To(BeEmpty())
					Expect(updated.Status.TargetVersion).To(Equal(version1))
					Expect(updated.Status.UpgradeStatus).To(Equal(k8sv1alpha1.UpgradeStatusFailed))

					upgradeCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeUpgradeComplete)
					Expect(upgradeCondition).ToNot(BeNil())
					Expect(upgradeCondition.Status).To(Equal(metav1.ConditionFalse))
					Expect(upgradeCondition.Reason).To(Equal(status.ReasonImagePullSecretSyncFailed))

					secretCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeImagePullSecretsSynced)
					Expect(secretCondition).ToNot(BeNil())
					Expect(secretCondition.Status).To(Equal(metav1.ConditionFalse))
					Expect(secretCondition.Reason).To(Equal(status.ReasonImagePullSecretSyncFailed))

					cniCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeCNIHealthy)
					Expect(cniCondition).ToNot(BeNil())
					Expect(cniCondition.Status).To(Equal(metav1.ConditionUnknown))
					Expect(cniCondition.Reason).To(Equal(status.ReasonCNIInitializing))
				})
			})

			Context("with component deployment failures", func() {
				BeforeEach(func() {
					networkOperator.Status = completedOperatorStatus(networkOperator.Generation)
					Expect(k8sClient.Status().Update(ctx, networkOperator)).To(Succeed())

					// Create templates with invalid Kubernetes resources that will fail deployment
					invalidTemplateDir := filepath.Join(tempDir, "invalid-resources")
					createEmptyDefaultTemplateDirs(invalidTemplateDir)

					// Create invalid resource (invalid apiVersion)
					invalidResource := `apiVersion: invalid/v999
kind: ConfigMap
metadata:
  name: test-config`

					Expect(os.WriteFile(filepath.Join(invalidTemplateDir, globalComponentName, "invalid.yaml"), []byte(invalidResource), 0644)).To(Succeed())
					reconciler.TemplateRenderer = renderer.NewTemplateRenderer(invalidTemplateDir, reconciler.EmbeddedConfig)
				})

				It("should handle component deployment failures", func() {
					result, err := reconciler.Reconcile(ctx, req)

					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("failed to deploy"))
					Expect(result).To(Equal(ctrl.Result{}))

					updated := &k8sv1alpha1.OVNK8sOperator{}
					Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
					Expect(updated.Status.Version).To(Equal("0.9.0"))
					Expect(updated.Status.TargetVersion).To(Equal(version1))
					Expect(updated.Status.UpgradeStatus).To(Equal(k8sv1alpha1.UpgradeStatusFailed))
					upgradeCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeUpgradeComplete)
					Expect(upgradeCondition).ToNot(BeNil())
					Expect(upgradeCondition.Reason).To(Equal(status.ReasonComponentDeploymentFailed))
					cniCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeCNIHealthy)
					Expect(cniCondition).ToNot(BeNil())
					Expect(cniCondition.Status).To(Equal(metav1.ConditionFalse))
					Expect(cniCondition.Reason).To(Equal(status.ReasonCNIDegraded))
				})
			})

			Context("with component validation failures", func() {
				BeforeEach(func() {
					networkOperator.Status = completedOperatorStatus(networkOperator.Generation)
					Expect(k8sClient.Status().Update(ctx, networkOperator)).To(Succeed())

					// Create templates with workloads that will fail validation
					validTemplateDir := createValidTemplateDir(tempDir)

					// Add a deployment template that will be deployed but fail validation
					ovnMasterDir := filepath.Join(validTemplateDir, k8sv1alpha1.ComponentOvnkubeControlPlane)
					Expect(os.MkdirAll(ovnMasterDir, 0755)).To(Succeed())

					deploymentTemplate := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: ovn-master
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ovn-master
  template:
    metadata:
      labels:
        app: ovn-master
    spec:
      containers:
      - name: ovn-master
        image: nonexistent:latest
        command: ["sleep", "infinity"]`

					Expect(os.WriteFile(filepath.Join(ovnMasterDir, "deployment.yaml"), []byte(deploymentTemplate), 0644)).To(Succeed())

					reconciler.TemplateRenderer = renderer.NewTemplateRenderer(validTemplateDir, reconciler.EmbeddedConfig)

					// Use a very short validation timeout to trigger timeout failures
					validatorConfig := &validator.ValidationConfig{
						Timeout:       100 * time.Millisecond, // Very short timeout
						CheckInterval: 50 * time.Millisecond,
						MaxRetries:    2,
					}
					reconciler.ComponentValidator = validator.NewComponentValidatorWithConfig(k8sClient, validatorConfig)
				})

				It("should handle component validation failures", func() {
					result, err := reconciler.Reconcile(ctx, req)

					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("validation"))
					Expect(result).To(Equal(ctrl.Result{}))

					updated := &k8sv1alpha1.OVNK8sOperator{}
					Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
					Expect(updated.Status.Version).To(Equal("0.9.0"))
					Expect(updated.Status.TargetVersion).To(Equal(version1))
					Expect(updated.Status.UpgradeStatus).To(Equal(k8sv1alpha1.UpgradeStatusFailed))
					upgradeCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeUpgradeComplete)
					Expect(upgradeCondition).ToNot(BeNil())
					Expect(upgradeCondition.Reason).To(Equal(status.ReasonComponentValidationFailed))
					cniCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeCNIHealthy)
					Expect(cniCondition).ToNot(BeNil())
					Expect(cniCondition.Status).To(Equal(metav1.ConditionFalse))
					Expect(cniCondition.Reason).To(Equal(status.ReasonCNIDegraded))
				})
			})

			Context("with test hook failures", func() {
				BeforeEach(func() {
					// Create valid templates for deployment
					validTemplateDir := createValidTemplateDir(tempDir)
					reconciler.TemplateRenderer = renderer.NewTemplateRenderer(validTemplateDir, reconciler.EmbeddedConfig)

					// Create test hooks directory with failing test hook
					testHooksDir := filepath.Join(tempDir, "test-hooks")
					Expect(os.MkdirAll(testHooksDir, 0755)).To(Succeed())

					// Create a job that will fail
					failingJobYaml := `apiVersion: batch/v1
kind: Job
metadata:
  name: failing-test-job
spec:
  template:
    spec:
      containers:
      - name: test
        image: nonexistent:latest
        command: ["exit", "1"]
      restartPolicy: Never`

					Expect(os.WriteFile(filepath.Join(testHooksDir, "failing-test.yaml"), []byte(failingJobYaml), 0644)).To(Succeed())

					// Enable test hooks with short timeout
					networkOperator.Spec.TestHooks = &k8sv1alpha1.TestHooksSpec{
						Enabled: true,
						Timeout: 1, // 1 second timeout
					}
					Expect(k8sClient.Update(ctx, networkOperator)).To(Succeed())
					networkOperator.Status = completedOperatorStatus(networkOperator.Generation)
					Expect(k8sClient.Status().Update(ctx, networkOperator)).To(Succeed())

					reconciler.TestHookRunner = testhook.NewTestHookRunner(k8sClient, k8sClient.Scheme(), testHooksDir, reconciler.TemplateRenderer)
				})

				It("should handle test hook execution failures", func() {
					result, err := reconciler.Reconcile(ctx, req)

					// This will timeout due to the failing job and short timeout
					Expect(err).To(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))

					updated := &k8sv1alpha1.OVNK8sOperator{}
					Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
					Expect(updated.Status.Version).To(Equal("0.9.0"))
					Expect(updated.Status.TargetVersion).To(Equal(version1))
					Expect(updated.Status.UpgradeStatus).To(Equal(k8sv1alpha1.UpgradeStatusFailed))
					upgradeCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeUpgradeComplete)
					Expect(upgradeCondition).ToNot(BeNil())
					Expect(upgradeCondition.Reason).To(Equal(status.ReasonTestHooksFailed))
					cniCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeCNIHealthy)
					Expect(cniCondition).ToNot(BeNil())
					Expect(cniCondition.Status).To(Equal(metav1.ConditionFalse))
					Expect(cniCondition.Reason).To(Equal(status.ReasonCNIDegraded))
					Expect(updated.Status.ComponentStatus).ToNot(BeEmpty())
					for _, component := range updated.Status.ComponentStatus {
						Expect(component.Phase).To(Equal(k8sv1alpha1.ComponentPhaseReady))
					}
				})
			})

			Context("with status update failures", func() {
				BeforeEach(func() {
					// Use an error client that fails status updates
					reconciler.Client = &errorClient{Client: k8sClient}
					reconciler.StatusManager = status.NewStatusManager(reconciler.Client, reconciler.EmbeddedConfig)

					// Use valid templates so the only failure is status updates
					validTemplateDir := createValidTemplateDir(tempDir)
					reconciler.TemplateRenderer = renderer.NewTemplateRenderer(validTemplateDir, reconciler.EmbeddedConfig)
				})

				It("should handle status update failures", func() {
					result, err := reconciler.Reconcile(ctx, req)

					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("client error"))
					Expect(result).To(Equal(ctrl.Result{}))
				})
			})

			Context("with version management scenarios", func() {
				Context("when versions match", func() {
					var originalTransition metav1.Time

					BeforeEach(func() {
						createEmptyDefaultTemplateDirs(tempDir)
						originalTransition = metav1.NewTime(time.Unix(1, 0))
						networkOperator.Status = k8sv1alpha1.OVNK8sOperatorStatus{
							Version:       version1,
							TargetVersion: version1,
							UpgradeStatus: k8sv1alpha1.UpgradeStatusCompleted,
							Conditions: []metav1.Condition{
								{
									Type:               status.ConditionTypeUpgradeComplete,
									Status:             metav1.ConditionTrue,
									ObservedGeneration: networkOperator.Generation,
									LastTransitionTime: originalTransition,
									Reason:             status.ReasonInstallationCompleted,
									Message:            "Fresh installation completed successfully",
								},
								{
									Type:               status.ConditionTypeCNIHealthy,
									Status:             metav1.ConditionTrue,
									ObservedGeneration: networkOperator.Generation,
									LastTransitionTime: originalTransition,
									Reason:             status.ReasonCNIReady,
									Message:            "All CNI components are ready and operational",
								},
							},
						}
						reconciler.EmbeddedConfig.Version = version1
						Expect(k8sClient.Status().Update(ctx, networkOperator)).To(Succeed())
					})

					It("should reconcile without starting a new rollout", func() {
						result, err := reconciler.Reconcile(ctx, req)

						Expect(err).ToNot(HaveOccurred())
						Expect(result).To(Equal(ctrl.Result{}))

						updated := &k8sv1alpha1.OVNK8sOperator{}
						Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
						Expect(updated.Status.Version).To(Equal(version1))
						Expect(updated.Status.TargetVersion).To(Equal(version1))
						Expect(updated.Status.UpgradeStatus).To(Equal(k8sv1alpha1.UpgradeStatusCompleted))
						upgradeCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeUpgradeComplete)
						Expect(upgradeCondition).ToNot(BeNil())
						Expect(upgradeCondition.Reason).To(Equal(status.ReasonInstallationCompleted))
						Expect(upgradeCondition.LastTransitionTime).To(Equal(originalTransition))
					})
				})

				Context("when an upgrade succeeds", func() {
					BeforeEach(func() {
						createEmptyDefaultTemplateDirs(tempDir)
						networkOperator.Status.Version = version1
						networkOperator.Status.TargetVersion = version1
						networkOperator.Status.UpgradeStatus = k8sv1alpha1.UpgradeStatusCompleted
						reconciler.EmbeddedConfig.Version = version2
						Expect(k8sClient.Status().Update(ctx, networkOperator)).To(Succeed())
					})

					It("should commit the target version after validation", func() {
						result, err := reconciler.Reconcile(ctx, req)

						Expect(err).ToNot(HaveOccurred())
						Expect(result).To(Equal(ctrl.Result{}))

						updated := &k8sv1alpha1.OVNK8sOperator{}
						Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
						Expect(updated.Status.Version).To(Equal(version2))
						Expect(updated.Status.TargetVersion).To(Equal(version2))
						Expect(updated.Status.UpgradeStatus).To(Equal(k8sv1alpha1.UpgradeStatusCompleted))
						upgradeCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeUpgradeComplete)
						Expect(upgradeCondition).ToNot(BeNil())
						Expect(upgradeCondition.Status).To(Equal(metav1.ConditionTrue))
						Expect(upgradeCondition.Reason).To(Equal(status.ReasonUpgradeCompleted))
					})
				})

				Context("when the operator image is reverted after a failed upgrade", func() {
					BeforeEach(func() {
						createEmptyDefaultTemplateDirs(tempDir)
						transitionTime := metav1.Now()
						networkOperator.Status = k8sv1alpha1.OVNK8sOperatorStatus{
							Version:       version1,
							TargetVersion: version2,
							UpgradeStatus: k8sv1alpha1.UpgradeStatusFailed,
							Conditions: []metav1.Condition{
								{
									Type:               status.ConditionTypeUpgradeComplete,
									Status:             metav1.ConditionFalse,
									ObservedGeneration: networkOperator.Generation,
									LastTransitionTime: transitionTime,
									Reason:             status.ReasonComponentDeploymentFailed,
									Message:            "Upgrade failed",
								},
								{
									Type:               status.ConditionTypeCNIHealthy,
									Status:             metav1.ConditionFalse,
									ObservedGeneration: networkOperator.Generation,
									LastTransitionTime: transitionTime,
									Reason:             status.ReasonCNIDegraded,
									Message:            "CNI is degraded",
								},
							},
						}
						reconciler.EmbeddedConfig.Version = version1
						Expect(k8sClient.Status().Update(ctx, networkOperator)).To(Succeed())
					})

					It("should recover the last completed release", func() {
						result, err := reconciler.Reconcile(ctx, req)

						Expect(err).ToNot(HaveOccurred())
						Expect(result).To(Equal(ctrl.Result{}))

						updated := &k8sv1alpha1.OVNK8sOperator{}
						Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
						Expect(updated.Status.Version).To(Equal(version1))
						Expect(updated.Status.TargetVersion).To(Equal(version1))
						Expect(updated.Status.UpgradeStatus).To(Equal(k8sv1alpha1.UpgradeStatusCompleted))
						upgradeCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeUpgradeComplete)
						Expect(upgradeCondition).ToNot(BeNil())
						Expect(upgradeCondition.Status).To(Equal(metav1.ConditionTrue))
						Expect(upgradeCondition.Reason).To(Equal(status.ReasonReconciliationCompleted))
						cniCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeCNIHealthy)
						Expect(cniCondition).ToNot(BeNil())
						Expect(cniCondition.Status).To(Equal(metav1.ConditionTrue))
					})
				})

				Context("when downgrade is attempted", func() {
					BeforeEach(func() {
						transitionTime := metav1.Now()
						networkOperator.Status = k8sv1alpha1.OVNK8sOperatorStatus{
							Version:       version2,
							TargetVersion: version2,
							UpgradeStatus: k8sv1alpha1.UpgradeStatusCompleted,
							Conditions: []metav1.Condition{{
								Type:               status.ConditionTypeCNIHealthy,
								Status:             metav1.ConditionTrue,
								ObservedGeneration: networkOperator.Generation,
								LastTransitionTime: transitionTime,
								Reason:             status.ReasonCNIReady,
								Message:            "CNI is healthy",
							}},
						}
						reconciler.EmbeddedConfig.Version = version1
						reconciler.EmbeddedConfig.MinUpgradeVersion = "" // Test that downgrades are blocked even without minUpgradeVersion
						Expect(k8sClient.Status().Update(ctx, networkOperator)).To(Succeed())
					})

					It("should block deployment", func() {
						result, err := reconciler.Reconcile(ctx, req)

						Expect(err).ToNot(HaveOccurred()) // Controller handles blocking gracefully
						Expect(result).To(Equal(ctrl.Result{}))

						updated := &k8sv1alpha1.OVNK8sOperator{}
						Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
						Expect(updated.Status.Version).To(Equal(version2))
						Expect(updated.Status.TargetVersion).To(Equal(version1))
						Expect(updated.Status.UpgradeStatus).To(Equal(k8sv1alpha1.UpgradeStatusFailed))
						upgradeCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeUpgradeComplete)
						Expect(upgradeCondition).ToNot(BeNil())
						Expect(upgradeCondition.Status).To(Equal(metav1.ConditionFalse))
						Expect(upgradeCondition.Reason).To(Equal(status.ReasonVersionValidationFailed))
						cniCondition := apimeta.FindStatusCondition(updated.Status.Conditions, status.ConditionTypeCNIHealthy)
						Expect(cniCondition).ToNot(BeNil())
						Expect(cniCondition.Status).To(Equal(metav1.ConditionTrue))
					})
				})

				Context("when current version is below minimum", func() {
					BeforeEach(func() {
						networkOperator.Status.Version = "0.9.0"
						reconciler.EmbeddedConfig.Version = version2
						reconciler.EmbeddedConfig.MinUpgradeVersion = version1
						Expect(k8sClient.Status().Update(ctx, networkOperator)).To(Succeed())

						// Create valid templates for testing version blocking
						validTemplateDir := createValidTemplateDir(tempDir)
						reconciler.TemplateRenderer = renderer.NewTemplateRenderer(validTemplateDir, reconciler.EmbeddedConfig)
					})

					It("should block deployment", func() {
						result, err := reconciler.Reconcile(ctx, req)

						Expect(err).ToNot(HaveOccurred()) // Should not error, just block
						Expect(result).To(Equal(ctrl.Result{}))
					})
				})

				Context("with invalid semantic versions", func() {
					BeforeEach(func() {
						networkOperator.Status.Version = invalidVersion
						reconciler.EmbeddedConfig.Version = version2
						reconciler.EmbeddedConfig.MinUpgradeVersion = version1
						Expect(k8sClient.Status().Update(ctx, networkOperator)).To(Succeed())
					})

					It("should block deployment with invalid current version", func() {
						result, err := reconciler.Reconcile(ctx, req)

						Expect(err).ToNot(HaveOccurred()) // Controller handles blocking gracefully
						Expect(result).To(Equal(ctrl.Result{}))
					})
				})
			})

			Context("with test hooks enabled but disabled in spec", func() {
				BeforeEach(func() {
					// Create test hooks directory and file
					testHooksDir := filepath.Join(tempDir, "test-hooks")
					Expect(os.MkdirAll(testHooksDir, 0755)).To(Succeed())

					jobYaml := `apiVersion: batch/v1
kind: Job
metadata:
  name: test-job
spec:
  template:
    spec:
      containers:
      - name: test
        image: busybox:latest
        command: ["echo", "hello"]
      restartPolicy: Never`

					Expect(os.WriteFile(filepath.Join(testHooksDir, testManifestFile), []byte(jobYaml), 0644)).To(Succeed())

					// Disable test hooks in spec
					networkOperator.Spec.TestHooks = &k8sv1alpha1.TestHooksSpec{
						Enabled: false,
					}
					Expect(k8sClient.Update(ctx, networkOperator)).To(Succeed())

					// Setup successful components
					validTemplateDir := createValidTemplateDir(tempDir)
					reconciler.TemplateRenderer = renderer.NewTemplateRenderer(validTemplateDir, reconciler.EmbeddedConfig)
				})

				It("should skip test hook execution when disabled", func() {
					result, err := reconciler.Reconcile(ctx, req)

					Expect(err).ToNot(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))
				})
			})
		})

		Context("when NetworkOperator resource does not exist", func() {
			It("should not return error", func() {
				result, err := reconciler.Reconcile(ctx, req)

				Expect(err).ToNot(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))
			})
		})
	})

	Describe("fetchNetworkOperator", func() {
		Context("when resource exists", func() {
			BeforeEach(func() {
				Expect(k8sClient.Create(ctx, networkOperator)).To(Succeed())
			})

			AfterEach(func() {
				Expect(k8sClient.Delete(ctx, networkOperator)).To(Succeed())
			})

			It("should return the NetworkOperator", func() {
				result, err := reconciler.fetchOVNK8sOperator(ctx, req)

				Expect(err).ToNot(HaveOccurred())
				Expect(result).ToNot(BeNil())
				Expect(result.Name).To(Equal(operatorName))
			})
		})

		Context("when resource does not exist", func() {
			It("should return NotFound error", func() {
				result, err := reconciler.fetchOVNK8sOperator(ctx, req)

				Expect(err).To(HaveOccurred())
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
				Expect(result).To(BeNil())
			})
		})

		Context("when client returns other errors", func() {
			It("should propagate client errors", func() {
				// Create a fake client that returns errors
				fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()

				reconciler := &OVNK8sOperatorReconciler{
					Client: &errorClient{Client: fakeClient},
					Scheme: scheme.Scheme,
				}

				result, err := reconciler.fetchOVNK8sOperator(ctx, req)

				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("client error"))
				Expect(result).To(BeNil())
			})
		})
	})

	Describe("renderComponentTemplates", func() {
		Context("with successful rendering", func() {
			BeforeEach(func() {
				validTemplateDir := createValidTemplateDir(tempDir)
				reconciler.TemplateRenderer = renderer.NewTemplateRenderer(validTemplateDir, reconciler.EmbeddedConfig)
			})

			It("should return component manifests", func() {
				manifests, err := reconciler.renderComponentTemplates(ctx, networkOperator)

				Expect(err).ToNot(HaveOccurred())
				Expect(manifests).To(HaveKey(globalComponentName))
				Expect(manifests[globalComponentName]).To(HaveLen(1))
			})
		})

		Context("with rendering failures", func() {
			BeforeEach(func() {
				badTemplateDir := createBadTemplateDir(tempDir)
				reconciler.TemplateRenderer = renderer.NewTemplateRenderer(badTemplateDir, reconciler.EmbeddedConfig)
			})

			It("should return error", func() {
				manifests, err := reconciler.renderComponentTemplates(ctx, networkOperator)

				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to render templates"))
				Expect(manifests).To(BeNil())
			})
		})
	})

	Describe("deployAndValidateComponents", func() {
		var componentManifests renderer.ComponentManifests

		BeforeEach(func() {
			componentManifests = renderer.ComponentManifests{
				globalComponentName: []*renderer.RenderedManifest{
					{Content: testConfigMapManifest, FileName: testManifestFile},
				},
				k8sv1alpha1.ComponentOvnkubeControlPlane: []*renderer.RenderedManifest{
					{Content: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: ovn-master-config\n  namespace: default", FileName: "ovn-master.yaml"},
				},
				k8sv1alpha1.ComponentOvnkubeNode: []*renderer.RenderedManifest{
					{Content: ovnNodeConfigMapManifest, FileName: ovnNodeManifestFile},
				},
			}
		})

		Context("with successful deployment and validation", func() {
			It("should succeed", func() {
				err := reconciler.deployAndValidateComponents(ctx, networkOperator, componentManifests, rolloutOperationInstallation)

				Expect(err).ToNot(HaveOccurred())
				componentNames := make([]string, 0, len(networkOperator.Status.ComponentStatus))
				for _, componentStatus := range networkOperator.Status.ComponentStatus {
					componentNames = append(componentNames, componentStatus.ResourceName)
				}
				Expect(componentNames).To(ConsistOf(
					k8sv1alpha1.ComponentOvnkubeControlPlane,
					k8sv1alpha1.ComponentOvnkubeNode,
				))
			})
		})

		Context("with deployment failures", func() {
			BeforeEach(func() {
				// Use invalid resources that will fail deployment
				componentManifests = renderer.ComponentManifests{
					globalComponentName: []*renderer.RenderedManifest{
						{Content: "apiVersion: invalid/v999\nkind: ConfigMap\nmetadata:\n  name: test-config\n  namespace: default", FileName: "invalid.yaml"},
					},
					k8sv1alpha1.ComponentOvnkubeControlPlane: []*renderer.RenderedManifest{
						{Content: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: ovn-master-config\n  namespace: default", FileName: "ovn-master.yaml"},
					},
					k8sv1alpha1.ComponentOvnkubeNode: []*renderer.RenderedManifest{
						{Content: ovnNodeConfigMapManifest, FileName: ovnNodeManifestFile},
					},
				}
			})

			It("should return error", func() {
				err := reconciler.deployAndValidateComponents(ctx, networkOperator, componentManifests, rolloutOperationInstallation)

				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to deploy"))
				Expect(networkOperator.Status.ComponentStatus).To(BeEmpty())
			})
		})

		Context("with validation failures", func() {
			BeforeEach(func() {
				// Create manifest with deployment that will fail validation due to timeout
				componentManifests = renderer.ComponentManifests{
					globalComponentName: []*renderer.RenderedManifest{
						{Content: testConfigMapManifest, FileName: testManifestFile},
					},
					k8sv1alpha1.ComponentOvnkubeControlPlane: []*renderer.RenderedManifest{
						{Content: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: ovn-master
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ovn-master
  template:
    metadata:
      labels:
        app: ovn-master
    spec:
      containers:
      - name: ovn-master
        image: nonexistent:latest`, FileName: "deployment.yaml"},
					},
					k8sv1alpha1.ComponentOvnkubeNode: []*renderer.RenderedManifest{
						{Content: ovnNodeConfigMapManifest, FileName: ovnNodeManifestFile},
					},
				}

				// Use short validation timeout
				validatorConfig := &validator.ValidationConfig{
					Timeout:       100 * time.Millisecond,
					CheckInterval: 50 * time.Millisecond,
					MaxRetries:    2,
				}
				reconciler.ComponentValidator = validator.NewComponentValidatorWithConfig(k8sClient, validatorConfig)
			})

			It("should return error", func() {
				err := reconciler.deployAndValidateComponents(ctx, networkOperator, componentManifests, rolloutOperationInstallation)

				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("validation"))
			})
		})
	})

	Describe("deployComponent", func() {
		var manifests []*renderer.RenderedManifest

		BeforeEach(func() {
			manifests = []*renderer.RenderedManifest{
				{Content: testConfigMapManifest, FileName: testManifestFile},
			}
		})

		Context("with successful deployment", func() {
			It("should succeed", func() {
				err := reconciler.deployComponent(ctx, "test-component", manifests, networkOperator, rolloutOperationInstallation)

				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("with empty manifests", func() {
			It("should handle empty manifests gracefully", func() {
				err := reconciler.deployComponent(ctx, "test-component", []*renderer.RenderedManifest{}, networkOperator, rolloutOperationInstallation)

				Expect(err).ToNot(HaveOccurred())
			})
		})
	})

	Describe("validateWorkloads", func() {
		Context("with empty workloads", func() {
			It("should return no error", func() {
				err := reconciler.validateWorkloads(ctx, "test-component", []deployer.WorkloadResource{}, networkOperator, rolloutOperationInstallation)

				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("with workloads that fail validation", func() {
			BeforeEach(func() {
				// Use short validation timeout to trigger failures
				validatorConfig := &validator.ValidationConfig{
					Timeout:       100 * time.Millisecond,
					CheckInterval: 50 * time.Millisecond,
					MaxRetries:    2,
				}
				reconciler.ComponentValidator = validator.NewComponentValidatorWithConfig(k8sClient, validatorConfig)
			})

			It("should return validation error", func() {
				// Create a deployment that won't be ready in time
				deployment := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: testWorkloadName, Namespace: metav1.NamespaceDefault},
					Spec: appsv1.DeploymentSpec{
						Replicas: new(int32(1)),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": testWorkloadName},
						},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"app": testWorkloadName},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: testWorkloadName, Image: "nonexistent:latest"},
								},
							},
						},
					},
				}

				// Create the deployment first
				Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

				workloads := []deployer.WorkloadResource{
					{
						Object: deployment,
						Type:   kinds.Deployment,
						Name:   testWorkloadName,
					},
				}

				err := reconciler.validateWorkloads(ctx, "test-component", workloads, networkOperator, rolloutOperationInstallation)

				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("validation"))

				// Cleanup
				_ = k8sClient.Delete(ctx, deployment)
			})
		})
	})

	Describe("buildNetworkOperatorDAG", func() {
		Context("with valid component dependencies", func() {
			It("should build DAG successfully", func() {
				dag := buildComponentDAG(reconciler.EmbeddedConfig.Components)

				Expect(dag).ToNot(BeNil())
			})
		})

		Context("with circular dependencies", func() {
			BeforeEach(func() {
				reconciler.EmbeddedConfig.Components = []defaults.EmbeddedComponent{
					{ResourceName: compA, ResourceType: kinds.Deployment, DependsOn: []string{compB}},
					{ResourceName: compB, ResourceType: kinds.Deployment, DependsOn: []string{compA}},
				}
			})

			It("should return error for circular dependencies", func() {
				dag := buildComponentDAG(reconciler.EmbeddedConfig.Components)

				Expect(dag).ToNot(BeNil()) // DAG creation succeeds

				// But getting upgrade order should fail
				_, err := dag.GetUpgradeOrder()
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("cycle"))
			})
		})

		Context("with dangling dependency edges", func() {
			BeforeEach(func() {
				reconciler.EmbeddedConfig.Components = []defaults.EmbeddedComponent{
					{ResourceName: compA, ResourceType: kinds.Deployment, DependsOn: []string{"missing-comp"}},
				}
			})

			It("should prune edges to missing components and succeed", func() {
				dag := buildComponentDAG(reconciler.EmbeddedConfig.Components)

				Expect(dag).ToNot(BeNil())

				// Missing dep edge is pruned, so GetUpgradeOrder succeeds
				order, err := dag.GetUpgradeOrder()
				Expect(err).ToNot(HaveOccurred())
				Expect(order).To(Equal([]string{compA}))
			})
		})
	})

	Describe("getUpgradePlan", func() {
		Context("with valid dependencies", func() {
			It("should return upgrade order with global first", func() {
				order, err := getUpgradePlan(ctx, networkOperator, reconciler.EmbeddedConfig)

				Expect(err).ToNot(HaveOccurred())
				Expect(order).To(Equal([]string{
					globalComponentName,
					k8sv1alpha1.ComponentOvnkubeControlPlane,
					k8sv1alpha1.ComponentOvnkubeNode,
				}))
			})
		})

		Context("with a circular dependency", func() {
			BeforeEach(func() {
				// Create components with a circular dependency (real DAG failure)
				reconciler.EmbeddedConfig.Components = []defaults.EmbeddedComponent{
					{ResourceName: compA, ResourceType: kinds.Deployment, DependsOn: []string{compB}},
					{ResourceName: compB, ResourceType: kinds.Deployment, DependsOn: []string{compA}},
				}
				reconciler.EmbeddedConfig.Modes = map[string]defaults.EmbeddedMode{
					k8sv1alpha1.DeploymentModeStandard: {Components: []string{compA, compB}},
				}
			})

			It("should return error", func() {
				_, err := getUpgradePlan(ctx, networkOperator, reconciler.EmbeddedConfig)

				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("cycle"))
			})
		})

		Context("with non-default mode and optional components", func() {
			BeforeEach(func() {
				reconciler.EmbeddedConfig.Components = []defaults.EmbeddedComponent{
					{ResourceName: modeRequiredComponent, ResourceType: kinds.Deployment, DependsOn: []string{}},
					{ResourceName: modeOptionalComponent, ResourceType: kinds.DaemonSet, DependsOn: []string{modeRequiredComponent}},
					{ResourceName: "other-mode-only", ResourceType: kinds.Deployment, DependsOn: []string{}},
				}
				reconciler.EmbeddedConfig.Modes = map[string]defaults.EmbeddedMode{
					k8sv1alpha1.DeploymentModeStandard: {Components: []string{"other-mode-only"}},
					k8sv1alpha1.DeploymentModeDpuHost: {
						Components:         []string{modeRequiredComponent},
						OptionalComponents: []string{modeOptionalComponent},
					},
				}
				networkOperator.Spec.Global.DeploymentMode = k8sv1alpha1.DeploymentModeDpuHost
				disabled := false
				networkOperator.Spec.ComponentOverrides = []k8sv1alpha1.ComponentOverride{
					{ResourceName: modeOptionalComponent, Enabled: &disabled},
				}
			})

			It("should prune to the selected mode and drop disabled optional components", func() {
				order, err := getUpgradePlan(ctx, networkOperator, reconciler.EmbeddedConfig)

				Expect(err).ToNot(HaveOccurred())
				Expect(order).To(HaveLen(2)) // global + mode-required
				Expect(order[0]).To(Equal(globalComponentName))
				Expect(order).To(ContainElement(modeRequiredComponent))
				Expect(order).ToNot(ContainElement(modeOptionalComponent))
				Expect(order).ToNot(ContainElement("other-mode-only"))
			})
		})

		Context("with complex dependency chain", func() {
			BeforeEach(func() {
				reconciler.EmbeddedConfig.Components = []defaults.EmbeddedComponent{
					{ResourceName: baseComponent, DependsOn: []string{}},
					{ResourceName: middleComponent, DependsOn: []string{baseComponent}},
					{ResourceName: "top", DependsOn: []string{middleComponent}},
					{ResourceName: "parallel", DependsOn: []string{baseComponent}},
				}
				reconciler.EmbeddedConfig.Modes = map[string]defaults.EmbeddedMode{
					k8sv1alpha1.DeploymentModeStandard: {Components: []string{baseComponent, middleComponent, "top", "parallel"}},
				}
			})

			It("should return correct dependency order", func() {
				order, err := getUpgradePlan(ctx, networkOperator, reconciler.EmbeddedConfig)

				Expect(err).ToNot(HaveOccurred())
				Expect(order).To(HaveLen(5)) // global + 4 components
				Expect(order[0]).To(Equal(globalComponentName))

				// Find indices of components
				indices := make(map[string]int)
				for i, comp := range order {
					indices[comp] = i
				}

				// Verify dependency order
				Expect(indices[baseComponent]).To(BeNumerically("<", indices[middleComponent]))
				Expect(indices[middleComponent]).To(BeNumerically("<", indices["top"]))
				Expect(indices[baseComponent]).To(BeNumerically("<", indices["parallel"]))
			})
		})
	})

	Describe("Edge Cases and Error Handling", func() {
		Context("with nil NetworkOperator", func() {
			It("should handle nil NetworkOperator in DAG building", func() {
				dag := buildComponentDAG(nil)

				Expect(dag).ToNot(BeNil())
			})
		})

		Context("with invalid component names", func() {
			BeforeEach(func() {
				reconciler.EmbeddedConfig.Components = []defaults.EmbeddedComponent{
					{ResourceName: "", DependsOn: []string{}}, // Empty name
				}
			})

			It("should handle empty component names", func() {
				dag := buildComponentDAG(reconciler.EmbeddedConfig.Components)

				Expect(dag).ToNot(BeNil())

				// Getting upgrade order may or may not fail depending on implementation
				_, _ = dag.GetUpgradeOrder()
				// We don't assert on the error since empty names might be handled differently
			})
		})

		Context("with very large component lists", func() {
			BeforeEach(func() {
				// Create a large number of components
				components := make([]defaults.EmbeddedComponent, 100)
				for i := range 100 {
					components[i] = defaults.EmbeddedComponent{
						ResourceName: fmt.Sprintf("component-%d", i),
						ResourceType: kinds.Deployment,
						DependsOn:    []string{},
					}
				}
				reconciler.EmbeddedConfig.Components = components
			})

			It("should handle large component lists", func() {
				dag := buildComponentDAG(reconciler.EmbeddedConfig.Components)

				Expect(dag).ToNot(BeNil())

				order, err := dag.GetUpgradeOrder()
				Expect(err).ToNot(HaveOccurred())
				Expect(order).To(HaveLen(100))
			})
		})
	})

	Describe("logComponentManifests", func() {
		It("should log manifests without errors", func() {
			manifests := []*renderer.RenderedManifest{
				{
					Content:      "test content",
					FileName:     testManifestFile,
					TemplatePath: "/path/to/template",
				},
			}

			// This should not panic or error
			reconciler.logComponentManifests(ctx, "test-component", manifests)
		})

		It("should handle empty manifests", func() {
			// This should not panic or error
			reconciler.logComponentManifests(ctx, "test-component", []*renderer.RenderedManifest{})
		})

		It("should handle nil manifests", func() {
			// This should not panic or error
			reconciler.logComponentManifests(ctx, "test-component", nil)
		})
	})

})

// errorClient is a mock client that returns errors for testing
type errorClient struct {
	client.Client
}

func (e *errorClient) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return fmt.Errorf("client error: failed to get object")
}

func (e *errorClient) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return fmt.Errorf("client error: failed to list objects")
}

func (e *errorClient) Create(_ context.Context, _ client.Object, _ ...client.CreateOption) error {
	return fmt.Errorf("client error: failed to create object")
}

func (e *errorClient) Delete(_ context.Context, _ client.Object, _ ...client.DeleteOption) error {
	return fmt.Errorf("client error: failed to delete object")
}

func (e *errorClient) Update(_ context.Context, _ client.Object, _ ...client.UpdateOption) error {
	return fmt.Errorf("client error: failed to update object")
}

func (e *errorClient) Patch(_ context.Context, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
	return fmt.Errorf("client error: failed to patch object")
}

func (e *errorClient) DeleteAllOf(_ context.Context, _ client.Object, _ ...client.DeleteAllOfOption) error {
	return fmt.Errorf("client error: failed to delete all")
}

func (e *errorClient) Status() client.StatusWriter {
	return &errorStatusWriter{}
}

type errorStatusWriter struct{}

func (e *errorStatusWriter) Update(_ context.Context, _ client.Object, _ ...client.SubResourceUpdateOption) error {
	return fmt.Errorf("client error: failed to update status")
}

func (e *errorStatusWriter) Patch(_ context.Context, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
	return fmt.Errorf("client error: failed to patch status")
}

func (e *errorStatusWriter) Create(_ context.Context, _ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
	return fmt.Errorf("client error: failed to create subresource")
}

func (e *errorStatusWriter) Apply(_ context.Context, _ runtime.ApplyConfiguration, _ ...client.SubResourceApplyOption) error {
	return fmt.Errorf("client error: failed to apply status")
}

func TestBuildComponentDAG_PrunesEdgesToAbsentComponents(t *testing.T) {
	// Dependencies outside the resolved set are pruned, while dependencies
	// between present components retain their ordering constraint.
	resolved := []defaults.EmbeddedComponent{
		{ResourceName: "ovnkubeControlPlane", DependsOn: nil},
		{ResourceName: "ovnkubeNode", DependsOn: []string{"ovnkubeControlPlane", "ovnkubeIdentity"}},
	}
	dag := buildComponentDAG(resolved)
	order, err := dag.GetUpgradeOrder()
	if err != nil {
		t.Fatalf("GetUpgradeOrder returned error (dangling edge not pruned): %v", err)
	}
	if len(order) != 2 {
		t.Fatalf("expected 2 components, got %v", order)
	}
	idxCP, idxNode := slices.Index(order, "ovnkubeControlPlane"), slices.Index(order, "ovnkubeNode")
	if idxCP == -1 || idxNode == -1 || idxCP > idxNode {
		t.Fatalf("expected control-plane before node, got %v", order)
	}
}

func TestGetUpgradePlanEmbeddedInstallationOrder(t *testing.T) {
	config, err := defaults.LoadEmbeddedConfig()
	if err != nil {
		t.Fatalf("LoadEmbeddedConfig: %v", err)
	}

	identityEnabled := true
	tests := []struct {
		name      string
		mode      string
		overrides []k8sv1alpha1.ComponentOverride
		want      []string
	}{
		{
			name: "standard without identity",
			mode: k8sv1alpha1.DeploymentModeStandard,
			want: []string{
				globalComponentName,
				k8sv1alpha1.ComponentOvnkubeControlPlane,
				k8sv1alpha1.ComponentOvnkubeNode,
			},
		},
		{
			name: "standard with identity",
			mode: k8sv1alpha1.DeploymentModeStandard,
			overrides: []k8sv1alpha1.ComponentOverride{
				{ResourceName: k8sv1alpha1.ComponentOvnkubeIdentity, Enabled: &identityEnabled},
			},
			want: []string{
				globalComponentName,
				k8sv1alpha1.ComponentOvnkubeControlPlane,
				k8sv1alpha1.ComponentOvnkubeIdentity,
				k8sv1alpha1.ComponentOvnkubeNode,
			},
		},
		{
			name: "dpu-host without identity",
			mode: k8sv1alpha1.DeploymentModeDpuHost,
			want: []string{
				globalComponentName,
				k8sv1alpha1.ComponentOvnkubeControlPlane,
				k8sv1alpha1.ComponentOvnkubeNodeDpuHost,
			},
		},
		{
			name: "dpu-host with identity",
			mode: k8sv1alpha1.DeploymentModeDpuHost,
			overrides: []k8sv1alpha1.ComponentOverride{
				{ResourceName: k8sv1alpha1.ComponentOvnkubeIdentity, Enabled: &identityEnabled},
			},
			want: []string{
				globalComponentName,
				k8sv1alpha1.ComponentOvnkubeControlPlane,
				k8sv1alpha1.ComponentOvnkubeIdentity,
				k8sv1alpha1.ComponentOvnkubeNodeDpuHost,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := &k8sv1alpha1.OVNK8sOperator{
				Spec: k8sv1alpha1.OVNK8sOperatorSpec{
					Global: k8sv1alpha1.GlobalConfig{
						DeploymentMode: tt.mode,
					},
					ComponentOverrides: tt.overrides,
				},
			}

			got, err := getUpgradePlan(context.Background(), cr, config)
			if err != nil {
				t.Fatalf("getUpgradePlan: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("order = %v, want %v", got, tt.want)
			}
		})
	}
}
