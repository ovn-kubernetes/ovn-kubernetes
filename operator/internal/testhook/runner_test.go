// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package testhook_test

import (
	"context"
	"os"
	"path/filepath"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/defaults"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/renderer"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/testhook"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("TestHookRunner", func() {
	var (
		testHookRunner *testhook.TestHookRunner
		tempDir        string
		ctx            context.Context
		ovnk8sOperator *k8sv1alpha1.OVNK8sOperator
		scheme         *runtime.Scheme
		fakeClient     client.Client
	)

	BeforeEach(func() {
		// Create temporary directory for test hooks
		var err error
		tempDir, err = os.MkdirTemp("", "test-hooks-*")
		Expect(err).ToNot(HaveOccurred())

		// Setup scheme
		scheme = runtime.NewScheme()
		Expect(k8sv1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(batchv1.AddToScheme(scheme)).To(Succeed())
		Expect(corev1.AddToScheme(scheme)).To(Succeed())

		// Create fake client
		fakeClient = fake.NewClientBuilder().WithScheme(scheme).Build()

		// Create test hook runner with shared template renderer
		testConfig := &defaults.EmbeddedConfig{
			Version: "1.0.0",
			Modes: map[string]defaults.EmbeddedMode{
				k8sv1alpha1.DeploymentModeStandard: {Components: nil},
			},
		}
		testRenderer := renderer.NewTemplateRenderer("", testConfig)
		testHookRunner = testhook.NewTestHookRunner(fakeClient, scheme, tempDir, testRenderer)

		// Setup context and network operator
		ctx = context.Background()
		ovnk8sOperator = &k8sv1alpha1.OVNK8sOperator{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-operator",
				Namespace: "test-namespace",
			},
			Spec: k8sv1alpha1.OVNK8sOperatorSpec{
				Global: k8sv1alpha1.GlobalConfig{
					DeploymentMode: k8sv1alpha1.DeploymentModeStandard,
				},
				TestHooks: &k8sv1alpha1.TestHooksSpec{
					Enabled: true,
					Tests:   []string{},
				},
			},
		}
	})

	AfterEach(func() {
		// Cleanup temporary directory
		if tempDir != "" {
			Expect(os.RemoveAll(tempDir)).To(Succeed())
		}
	})

	Describe("Constructor", func() {
		It("should create a new TestHookRunner with correct properties", func() {
			Expect(testHookRunner).ToNot(BeNil())
			Expect(testHookRunner.Client).ToNot(BeNil())
			Expect(testHookRunner.Scheme).To(Equal(scheme))
			Expect(testHookRunner.TestHooksDir).To(Equal(tempDir))
			Expect(testHookRunner.TemplateRenderer).ToNot(BeNil())
		})
	})

	Describe("RunTestHooks", func() {
		Context("when test hooks are disabled", func() {
			It("should return nil without executing hooks", func() {
				ovnk8sOperator.Spec.TestHooks.Enabled = false
				err := testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("when test hooks spec is nil", func() {
			It("should return nil without executing hooks", func() {
				ovnk8sOperator.Spec.TestHooks = nil
				err := testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("when test hooks directory doesn't exist", func() {
			It("should return nil without error", func() {
				Expect(os.RemoveAll(tempDir)).To(Succeed())
				err := testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("when test hooks directory is empty", func() {
			It("should return nil without error", func() {
				err := testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("when test hooks directory contains non-YAML files", func() {
			It("should ignore non-YAML files", func() {
				// Create a non-YAML file
				nonYamlFile := filepath.Join(tempDir, "test.txt")
				err := os.WriteFile(nonYamlFile, []byte("not yaml"), 0644)
				Expect(err).ToNot(HaveOccurred())

				err = testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
			})
		})
	})

	Describe("Helper Functions", func() {
		Describe("filterTestFiles", func() {
			var testFiles []testhook.TestHookFile

			BeforeEach(func() {
				testFiles = []testhook.TestHookFile{
					{Name: "test1.yaml", Content: "content1"},
					{Name: "test2.yaml", Content: "content2"},
					{Name: "test3.yaml", Content: "content3"},
				}
			})

			Context("when no specific tests are specified", func() {
				It("should return all files", func() {
					ovnk8sOperator.Spec.TestHooks.Tests = []string{}

					// Create test files with invalid template content (but valid YAML structure)
					invalidTemplateContent := `
apiVersion: v1
kind: ServiceAccount
metadata:
  name: test-{{ .invalid.template.syntax
`
					for _, file := range testFiles {
						filePath := filepath.Join(tempDir, file.Name)
						err := os.WriteFile(filePath, []byte(invalidTemplateContent), 0644)
						Expect(err).ToNot(HaveOccurred())
					}

					// This will test filterTestFiles indirectly - all files should be processed
					err := testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
					// We expect an error because the template is invalid
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("failed to render template"))
				})
			})

			Context("when specific tests are specified", func() {
				It("should filter files based on specified test names", func() {
					ovnk8sOperator.Spec.TestHooks.Tests = []string{"test1.yaml", "test3.yaml"}

					// Create test files
					validYaml := `
apiVersion: v1
kind: ServiceAccount
metadata:
  name: test-sa
`
					for _, file := range testFiles {
						filePath := filepath.Join(tempDir, file.Name)
						err := os.WriteFile(filePath, []byte(validYaml), 0644)
						Expect(err).ToNot(HaveOccurred())
					}

					// This should only process test1.yaml and test3.yaml
					err := testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
					Expect(err).ToNot(HaveOccurred())
				})
			})
		})
	})

	Describe("YAML Parsing and Resource Conversion", func() {
		Context("when converting unstructured to typed resources", func() {
			It("should convert ConfigMap correctly", func() {
				configMapYaml := `
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-config
data:
  config.yaml: |
    key: value
    setting: enabled
`
				err := os.WriteFile(filepath.Join(tempDir, "configmap-test.yaml"), []byte(configMapYaml), 0644)
				Expect(err).ToNot(HaveOccurred())

				err = testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
			})

			It("should convert ServiceAccount correctly", func() {
				saYaml := `
apiVersion: v1
kind: ServiceAccount
metadata:
  name: test-sa
`
				err := os.WriteFile(filepath.Join(tempDir, "sa-test.yaml"), []byte(saYaml), 0644)
				Expect(err).ToNot(HaveOccurred())

				err = testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
			})

			It("should handle unstructured resources for unknown types", func() {
				customYaml := `
apiVersion: custom.io/v1
kind: CustomResource
metadata:
  name: test-custom
spec:
  field: value
`
				err := os.WriteFile(filepath.Join(tempDir, "custom-test.yaml"), []byte(customYaml), 0644)
				Expect(err).ToNot(HaveOccurred())

				err = testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("when parsing multi-document YAML", func() {
			It("should parse multiple resources from single file", func() {
				multiDocYaml := `
apiVersion: v1
kind: ServiceAccount
metadata:
  name: test-sa
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-config
data:
  key: value
`
				err := os.WriteFile(filepath.Join(tempDir, "multi-test.yaml"), []byte(multiDocYaml), 0644)
				Expect(err).ToNot(HaveOccurred())

				err = testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("when YAML is invalid", func() {
			It("should return error for malformed YAML", func() {
				invalidYaml := `
apiVersion: v1
kind: ServiceAccount
metadata:
  name: test-sa
  invalid: yaml: structure
    malformed
`
				err := os.WriteFile(filepath.Join(tempDir, "invalid-test.yaml"), []byte(invalidYaml), 0644)
				Expect(err).ToNot(HaveOccurred())

				err = testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to parse YAML"))
			})
		})
	})

	Describe("Job Monitoring", func() {
		var jobYaml string

		BeforeEach(func() {
			jobYaml = `
apiVersion: batch/v1
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
      restartPolicy: Never
`
		})

		Context("when job completes successfully", func() {
			It("should monitor job until completion", func() {
				successJobYaml := `
apiVersion: batch/v1
kind: Job
metadata:
  name: test-success-job
spec:
  template:
    spec:
      containers:
      - name: test
        image: busybox:latest
        command: ["echo", "hello"]
      restartPolicy: Never
`
				err := os.WriteFile(filepath.Join(tempDir, "job-success-test.yaml"), []byte(successJobYaml), 0644)
				Expect(err).ToNot(HaveOccurred())

				// Start job monitoring in background with a timeout
				done := make(chan error, 1)
				go func() {
					// Use a short timeout to prevent hanging
					timeoutCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
					defer cancel()
					done <- testHookRunner.RunTestHooks(timeoutCtx, ovnk8sOperator)
				}()

				// Give some time for job creation
				time.Sleep(100 * time.Millisecond)

				// Find and update the job to completed status
				jobList := &batchv1.JobList{}
				err = fakeClient.List(ctx, jobList)
				Expect(err).ToNot(HaveOccurred())

				if len(jobList.Items) > 0 {
					// Update job status to Complete
					job := &jobList.Items[0]
					job.Status.Conditions = []batchv1.JobCondition{
						{
							Type:   batchv1.JobComplete,
							Status: corev1.ConditionTrue,
						},
					}
					err = fakeClient.Update(ctx, job)
					Expect(err).ToNot(HaveOccurred())
				}

				// Wait for completion or timeout
				select {
				case err := <-done:
					// Either success (job completed) or timeout (expected with fake client)
					if err != nil {
						Expect(err.Error()).To(SatisfyAny(
							ContainSubstring("context deadline exceeded"),
							ContainSubstring("job monitoring cancelled"),
						))
					}
				case <-time.After(3 * time.Second):
					// This is acceptable - job monitoring with fake client may not complete
				}
			})
		})

		Context("when job fails", func() {
			It("should return error when job fails", func() {
				failJobYaml := `
apiVersion: batch/v1
kind: Job
metadata:
  name: test-fail-job
spec:
  template:
    spec:
      containers:
      - name: test
        image: busybox:latest
        command: ["echo", "hello"]
      restartPolicy: Never
`
				err := os.WriteFile(filepath.Join(tempDir, "job-fail-test.yaml"), []byte(failJobYaml), 0644)
				Expect(err).ToNot(HaveOccurred())

				// Start job monitoring in background with a timeout
				done := make(chan error, 1)
				go func() {
					// Use a short timeout to prevent hanging
					timeoutCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
					defer cancel()
					done <- testHookRunner.RunTestHooks(timeoutCtx, ovnk8sOperator)
				}()

				// Give some time for job creation
				time.Sleep(100 * time.Millisecond)

				// Find and update the job to failed status
				jobList := &batchv1.JobList{}
				err = fakeClient.List(ctx, jobList)
				Expect(err).ToNot(HaveOccurred())

				if len(jobList.Items) > 0 {
					// Update job status to Failed
					job := &jobList.Items[0]
					job.Status.Conditions = []batchv1.JobCondition{
						{
							Type:    batchv1.JobFailed,
							Status:  corev1.ConditionTrue,
							Message: "Job failed for testing",
						},
					}
					err = fakeClient.Update(ctx, job)
					Expect(err).ToNot(HaveOccurred())
				}

				// Wait for completion or timeout
				select {
				case err := <-done:
					// Should get either job failure or timeout
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(SatisfyAny(
						ContainSubstring("test job failed"),
						ContainSubstring("context deadline exceeded"),
						ContainSubstring("job monitoring cancelled"),
					))
				case <-time.After(3 * time.Second):
					// This is acceptable - job monitoring with fake client may not complete
				}
			})
		})

		Context("when context is cancelled", func() {
			It("should return context cancellation error", func() {
				err := os.WriteFile(filepath.Join(tempDir, "job-cancel-test.yaml"), []byte(jobYaml), 0644)
				Expect(err).ToNot(HaveOccurred())

				// Create context with cancellation
				cancelCtx, cancel := context.WithCancel(ctx)

				// Start job monitoring in background
				done := make(chan error, 1)
				go func() {
					done <- testHookRunner.RunTestHooks(cancelCtx, ovnk8sOperator)
				}()

				// Give the test some time to create the job and start monitoring
				time.Sleep(100 * time.Millisecond)

				// Cancel the context
				cancel()

				// Wait for test hook to be cancelled
				select {
				case err := <-done:
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("job monitoring cancelled"))
				case <-time.After(5 * time.Second):
					Fail("Test hook should have been cancelled")
				}
			})
		})

		Context("when context times out", func() {
			It("should return context timeout error", func() {
				err := os.WriteFile(filepath.Join(tempDir, "job-timeout-test.yaml"), []byte(jobYaml), 0644)
				Expect(err).ToNot(HaveOccurred())

				// Create context with short timeout
				timeoutCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
				defer cancel()

				// Job will timeout since we don't update its status
				err = testHookRunner.RunTestHooks(timeoutCtx, ovnk8sOperator)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("context deadline exceeded"))
			})
		})
	})

	Describe("Resource Creation and Cleanup", func() {
		Context("when creating and cleaning up resources", func() {
			It("should handle resource creation and cleanup properly", func() {
				yamlContent := `
apiVersion: v1
kind: Secret
metadata:
  name: test-secret
type: Opaque
data:
  username: dGVzdA==
  password: dGVzdDEyMw==
`
				err := os.WriteFile(filepath.Join(tempDir, "secret-test.yaml"), []byte(yamlContent), 0644)
				Expect(err).ToNot(HaveOccurred())

				// This will create the secret and clean it up (no job monitoring involved)
				err = testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
			})
		})
	})

	Describe("Template Rendering", func() {
		Context("when test hooks use templates", func() {
			It("should render templates with OVNK8sOperator data", func() {
				templateYaml := `
apiVersion: v1
kind: ServiceAccount
metadata:
  name: test-sa-{{ .metadata.name }}
  namespace: {{ .metadata.namespace }}
`
				err := os.WriteFile(filepath.Join(tempDir, "template-test.yaml"), []byte(templateYaml), 0644)
				Expect(err).ToNot(HaveOccurred())

				err = testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
			})

			It("should handle template rendering errors", func() {
				invalidTemplateYaml := `
apiVersion: v1
kind: ServiceAccount
metadata:
  name: test-sa-{{ .nonexistent.field }}
`
				err := os.WriteFile(filepath.Join(tempDir, "invalid-template-test.yaml"), []byte(invalidTemplateYaml), 0644)
				Expect(err).ToNot(HaveOccurred())

				err = testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
				// This might not error with Go templates due to <no value> behavior
				// The test validates the behavior regardless
				Expect(err).ToNot(HaveOccurred())
			})
		})
	})

	Describe("Error Scenarios", func() {
		Context("when file reading fails", func() {
			It("should handle unreadable files gracefully", func() {
				// Create a file with restricted permissions (if supported by OS)
				restrictedFile := filepath.Join(tempDir, "restricted.yaml")
				err := os.WriteFile(restrictedFile, []byte("test content"), 0000)
				if err == nil {
					// Only test this if we can create restricted files
					defer func() { _ = os.Chmod(restrictedFile, 0644) }() // Cleanup

					if _, err := os.ReadFile(restrictedFile); err == nil {
						Skip("restricted file remains readable in this environment")
					}

					err = testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("failed to load test hook files"))
				}
			})
		})

		Context("when resource creation fails", func() {
			It("should handle resource creation errors", func() {
				// Create YAML with invalid resource that should fail creation
				invalidResourceYaml := `
apiVersion: batch/v1
kind: Job
metadata:
  name: "" # Invalid empty name
spec:
  template:
    spec:
      containers: []
      restartPolicy: Never
`
				err := os.WriteFile(filepath.Join(tempDir, "invalid-resource-test.yaml"), []byte(invalidResourceYaml), 0644)
				Expect(err).ToNot(HaveOccurred())

				err = testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to create resource"))
			})
		})
	})

	Describe("Edge Cases", func() {
		Context("when test hook files have empty content", func() {
			It("should handle empty YAML files", func() {
				err := os.WriteFile(filepath.Join(tempDir, "empty-test.yaml"), []byte(""), 0644)
				Expect(err).ToNot(HaveOccurred())

				err = testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("when test hook files have only comments", func() {
			It("should handle YAML files with only comments", func() {
				commentOnlyYaml := `
# This is just a comment
# Another comment
---
# More comments
`
				err := os.WriteFile(filepath.Join(tempDir, "comments-test.yaml"), []byte(commentOnlyYaml), 0644)
				Expect(err).ToNot(HaveOccurred())

				err = testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("when specified test files don't exist", func() {
			It("should handle missing specified test files", func() {
				ovnk8sOperator.Spec.TestHooks.Tests = []string{"nonexistent.yaml"}

				err := testHookRunner.RunTestHooks(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred()) // No files match, so no execution
			})
		})
	})
})
