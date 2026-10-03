// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package validator

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/deployer"
)

// ComponentValidator validates workload resources for components
type ComponentValidator struct {
	client.Client
	Config *ValidationConfig
}

// ValidationConfig holds configuration for validation behavior
type ValidationConfig struct {
	Timeout       time.Duration // Maximum time to wait for validation
	CheckInterval time.Duration // Interval between validation checks
	MaxRetries    int           // Maximum number of retry attempts
}

// ValidationResult represents the result of a workload validation
type ValidationResult struct {
	WorkloadName string
	WorkloadType string
	Ready        bool
	Message      string
	Error        error
}

// NewComponentValidatorWithConfig creates a new ComponentValidator with custom configuration
func NewComponentValidatorWithConfig(client client.Client, config *ValidationConfig) *ComponentValidator {
	return &ComponentValidator{
		Client: client,
		Config: config,
	}
}

// ValidateComponent validates all workload resources for a component
// Returns error if any workload fails validation within the timeout period
func (v *ComponentValidator) ValidateComponent(ctx context.Context, componentName string, workloads []deployer.WorkloadResource) error {
	logger := log.FromContext(ctx).WithValues("component", componentName)

	if len(workloads) == 0 {
		logger.V(1).Info("No workload resources to validate")
		return nil
	}

	logger.V(1).Info("Starting workload validation",
		"workloadCount", len(workloads),
		"timeout", v.Config.Timeout)

	// Validate each workload resource
	for _, workload := range workloads {
		if err := v.validateWorkload(ctx, componentName, workload); err != nil {
			return fmt.Errorf("validation failed for %s/%s: %w",
				workload.Type, workload.Name, err)
		}
	}

	logger.V(1).Info("All workloads validated successfully",
		"workloadCount", len(workloads))

	return nil
}

// validateWorkload validates a single workload resource with retry logic
func (v *ComponentValidator) validateWorkload(ctx context.Context, componentName string, workload deployer.WorkloadResource) error {
	logger := log.FromContext(ctx).WithValues(
		"component", componentName,
		"workloadType", workload.Type,
		"workloadName", workload.Name)

	workloadValidator := v.getValidatorForWorkload(workload.Type)

	// Create timeout context
	timeoutCtx, cancel := context.WithTimeout(ctx, v.Config.Timeout)
	defer cancel()

	ticker := time.NewTicker(v.Config.CheckInterval)
	defer ticker.Stop()

	retryCount := 0

	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("validation timeout exceeded (%v) for %s/%s",
				v.Config.Timeout, workload.Type, workload.Name)

		case <-ticker.C:
			// Refresh the workload object to get latest status
			if err := v.Get(timeoutCtx, client.ObjectKeyFromObject(workload.Object), workload.Object); err != nil {
				retryCount++
				if retryCount >= v.Config.MaxRetries {
					return fmt.Errorf("failed to fetch workload status after %d retries: %w",
						v.Config.MaxRetries, err)
				}
				logger.V(2).Info("Failed to fetch workload status, retrying",
					"retry", retryCount,
					"error", err.Error())
				continue
			}

			// Validate workload readiness
			result, err := workloadValidator.ValidateReadiness(workload.Object)
			if err != nil {
				return fmt.Errorf("validation check failed: %w", err)
			}

			if result.Ready {
				logger.Info("Workload ready", "status", result.Message)
				return nil
			}

			// Log progress at trace level to avoid spam
			logger.V(2).Info("Workload not ready yet", "status", result.Message)
		}
	}
}

// getValidatorForWorkload returns the appropriate validator for the workload type
func (v *ComponentValidator) getValidatorForWorkload(workloadType string) WorkloadValidator {
	switch workloadType {
	case "Deployment":
		return &DeploymentValidator{}
	case "DaemonSet":
		return &DaemonSetValidator{}
	case "StatefulSet":
		return &StatefulSetValidator{}
	default:
		return &GenericWorkloadValidator{}
	}
}
