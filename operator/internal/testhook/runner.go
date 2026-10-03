// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package testhook

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/renderer"
)

// TestHookRunner executes test hooks after CNI deployment
type TestHookRunner struct {
	client.Client
	Scheme           *runtime.Scheme
	TestHooksDir     string                     // Configurable directory path for test hooks
	TemplateRenderer *renderer.TemplateRenderer // Reuse existing renderer functionality
}

// TestHookFile represents a test hook file
type TestHookFile struct {
	Name    string
	Content string
}

// NewTestHookRunner creates a new test hook runner.
// Shares the controller's TemplateRenderer for consistent template data context.
func NewTestHookRunner(k8sClient client.Client, scheme *runtime.Scheme, testHooksDir string, templateRenderer *renderer.TemplateRenderer) *TestHookRunner {
	return &TestHookRunner{
		Client:           k8sClient,
		Scheme:           scheme,
		TestHooksDir:     testHooksDir,
		TemplateRenderer: templateRenderer,
	}
}

// RunTestHooks executes test hooks based on OVNK8sOperator configuration
func (r *TestHookRunner) RunTestHooks(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator) error {
	logger := log.FromContext(ctx).WithValues("ovnk8sOperator", ovnk8sOperator.Name)

	// Check if test hooks are enabled
	if ovnk8sOperator.Spec.TestHooks == nil || !ovnk8sOperator.Spec.TestHooks.Enabled {
		logger.Info("Test hooks are disabled, skipping execution")
		return nil
	}

	// Create timeout context from user configuration
	timeout := time.Duration(ovnk8sOperator.Spec.TestHooks.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute // Default fallback
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	logger.Info("Starting test hook execution", "timeout", timeout)

	// Load all available test hook files
	allTestFiles, err := r.loadTestHookFiles()
	if err != nil {
		return fmt.Errorf("failed to load test hook files: %w", err)
	}

	if len(allTestFiles) == 0 {
		logger.Info("No test hooks found in test-hooks directory")
		return nil
	}

	// Filter test files based on configuration
	testFiles := r.filterTestFiles(allTestFiles, ovnk8sOperator.Spec.TestHooks.Tests)

	if len(testFiles) == 0 {
		logger.Info("No test hooks match the specified configuration")
		return nil
	}

	logger.Info("Found test hooks to execute", "count", len(testFiles), "files", r.getTestFileNames(testFiles))

	// Execute each test hook with timeout context
	for _, testFile := range testFiles {
		logger.Info("Executing test hook", "file", testFile.Name)

		if err := r.executeTestHook(timeoutCtx, ovnk8sOperator, testFile); err != nil {
			return fmt.Errorf("test hook %s failed: %w", testFile.Name, err)
		}

		logger.Info("Test hook completed successfully", "file", testFile.Name)
	}

	logger.Info("All test hooks completed successfully")
	return nil
}

// filterTestFiles filters test files based on the specified test names, run only the specified tests or all if no tests are specified.
func (r *TestHookRunner) filterTestFiles(allFiles []TestHookFile, specifiedTests []string) []TestHookFile {
	// If no specific tests are specified, return all files
	if len(specifiedTests) == 0 {
		return allFiles
	}

	// Create a map for quick lookup
	testMap := make(map[string]bool)
	for _, testName := range specifiedTests {
		testMap[testName] = true
	}

	// Filter files
	var filteredFiles []TestHookFile
	for _, file := range allFiles {
		if testMap[file.Name] {
			filteredFiles = append(filteredFiles, file)
		}
	}

	return filteredFiles
}

// getTestFileNames returns a slice of test file names for logging
func (r *TestHookRunner) getTestFileNames(testFiles []TestHookFile) []string {
	names := make([]string, len(testFiles))
	for i, file := range testFiles {
		names[i] = file.Name
	}
	return names
}

// loadTestHookFiles loads all YAML files from test-hooks directory
func (r *TestHookRunner) loadTestHookFiles() ([]TestHookFile, error) {
	var testFiles []TestHookFile

	// Check if directory exists
	if _, err := os.Stat(r.TestHooksDir); os.IsNotExist(err) {
		return testFiles, nil // Return empty list if directory doesn't exist
	}

	entries, err := os.ReadDir(r.TestHooksDir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		filePath := filepath.Join(r.TestHooksDir, entry.Name())
		content, err := os.ReadFile(filePath)
		if err != nil {
			return nil, err
		}

		testFiles = append(testFiles, TestHookFile{
			Name:    entry.Name(),
			Content: string(content),
		})
	}

	return testFiles, nil
}

// executeTestHook executes a single test hook file
func (r *TestHookRunner) executeTestHook(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, testFile TestHookFile) error {
	logger := log.FromContext(ctx).WithValues("testHook", testFile.Name)

	// Render template using the renderer package
	renderedYAML, err := r.TemplateRenderer.RenderTemplateContent(testFile.Content, ovnk8sOperator)
	if err != nil {
		return fmt.Errorf("failed to render template: %w", err)
	}

	// Parse YAML into resources
	resources, err := r.parseYAMLResources(renderedYAML)
	if err != nil {
		return fmt.Errorf("failed to parse YAML: %w", err)
	}

	logger.Info("Creating test resources", "count", len(resources))

	// Apply all resources
	var testJob *batchv1.Job
	createdResources := []client.Object{}

	for _, resource := range resources {
		// Note: No owner references needed for test resources since we manually clean them up.

		// Create resource
		if err := r.Create(ctx, resource); err != nil {
			// Cleanup already created resources on failure
			r.cleanupResources(ctx, createdResources)
			return fmt.Errorf("failed to create resource %s: %w", resource.GetName(), err)
		}

		createdResources = append(createdResources, resource)

		// Track the test job for monitoring
		if job, ok := resource.(*batchv1.Job); ok {
			testJob = job
			logger.Info("Found test job", "jobName", job.Name)
		}
	}

	// Monitor test job if found
	if testJob != nil {
		if err := r.monitorTestJob(ctx, testJob); err != nil {
			return fmt.Errorf("test job %s failed: %w", testJob.Name, err)
		}
		logger.Info("Test job completed successfully", "jobName", testJob.Name)
	}

	// Cleanup resources (simple approach - always cleanup)
	logger.Info("Cleaning up test resources")
	r.cleanupResources(ctx, createdResources)

	return nil
}

// parseYAMLResources parses multi-document YAML into Kubernetes resources using renderer functionality
func (r *TestHookRunner) parseYAMLResources(yamlContent string) ([]client.Object, error) {
	var resources []client.Object

	// Use the renderer's YAML splitting functionality
	documents := r.TemplateRenderer.SplitYAMLDocuments(yamlContent)

	for _, doc := range documents {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}

		// Parse into unstructured first
		var obj unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			return nil, err
		}

		if obj.GetKind() == "" {
			continue // Skip empty objects
		}

		// Convert to typed object
		resource, err := r.convertToTypedResource(&obj)
		if err != nil {
			return nil, err
		}

		resources = append(resources, resource)
	}

	return resources, nil
}

// convertToTypedResource converts unstructured to typed Kubernetes resource
func (r *TestHookRunner) convertToTypedResource(obj *unstructured.Unstructured) (client.Object, error) {
	switch obj.GetKind() {
	case "Job":
		var job batchv1.Job
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &job); err != nil {
			return nil, err
		}
		return &job, nil
	case "ServiceAccount":
		var sa corev1.ServiceAccount
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &sa); err != nil {
			return nil, err
		}
		return &sa, nil
	default:
		// For other types, return as unstructured
		return obj, nil
	}
}

// monitorTestJob monitors a test job until completion
func (r *TestHookRunner) monitorTestJob(ctx context.Context, job *batchv1.Job) error {
	logger := log.FromContext(ctx).WithValues("job", job.Name)

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("job monitoring cancelled: %w", ctx.Err())
		case <-ticker.C:
			// Get current job status
			currentJob := &batchv1.Job{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(job), currentJob); err != nil {
				return fmt.Errorf("failed to get job status: %w", err)
			}

			// Check job conditions
			for _, condition := range currentJob.Status.Conditions {
				switch condition.Type {
				case batchv1.JobComplete:
					logger.Info("Test job completed successfully")
					return nil
				case batchv1.JobFailed:
					return fmt.Errorf("test job failed: %s", condition.Message)
				}
			}

			logger.V(1).Info("Test job still running", "active", currentJob.Status.Active)
		}
	}
}

// cleanupResources deletes all provided resources with proper propagation policy
func (r *TestHookRunner) cleanupResources(ctx context.Context, resources []client.Object) {
	logger := log.FromContext(ctx)

	// Delete in reverse order
	for _, resource := range slices.Backward(resources) {
		// Use background propagation policy for Jobs to ensure child Pods are also deleted
		// This prevents the warning about preserved child pods
		deleteOptions := &client.DeleteOptions{}
		if _, isJob := resource.(*batchv1.Job); isJob {
			propagationPolicy := metav1.DeletePropagationBackground
			deleteOptions.PropagationPolicy = &propagationPolicy
		}

		if err := r.Delete(ctx, resource, deleteOptions); err != nil {
			logger.Error(err, "Failed to cleanup resource", "name", resource.GetName(), "kind", resource.GetObjectKind())
		} else {
			logger.V(1).Info("Cleaned up resource", "name", resource.GetName(), "kind", resource.GetObjectKind())
		}
	}
}
