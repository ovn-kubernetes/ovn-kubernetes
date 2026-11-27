// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/mod/semver"

	"sigs.k8s.io/controller-runtime/pkg/log"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
)

// versionAction represents the result of comparing the completed and target
// release versions.
type versionAction int

const (
	// versionActionRollout indicates an installation or upgrade rollout is needed.
	versionActionRollout versionAction = iota
	// versionActionReconcile indicates same-release reconciliation without a
	// rollout transition.
	versionActionReconcile
	// versionActionBlock indicates reconciliation is blocked by version constraints.
	versionActionBlock
)

type rolloutOperation int

const (
	rolloutOperationInstallation rolloutOperation = iota
	rolloutOperationUpgrade
	rolloutOperationRecovery
	rolloutOperationSteady
	rolloutOperationBlocked
)

func (o rolloutOperation) String() string {
	switch o {
	case rolloutOperationInstallation:
		return "installation"
	case rolloutOperationUpgrade:
		return "upgrade"
	case rolloutOperationRecovery:
		return "recovery"
	case rolloutOperationSteady:
		return "steady"
	case rolloutOperationBlocked:
		return "blocked"
	default:
		return "unknown"
	}
}

func (o rolloutOperation) isRollout() bool {
	return o == rolloutOperationInstallation || o == rolloutOperationUpgrade || o == rolloutOperationRecovery
}

func classifyRolloutOperation(ovnk8sOperator *k8sv1alpha1.OVNK8sOperator, targetVersion string, action versionAction) rolloutOperation {
	if action == versionActionBlock {
		return rolloutOperationBlocked
	}
	if ovnk8sOperator.Status.Version == "" {
		return rolloutOperationInstallation
	}
	if action == versionActionRollout {
		return rolloutOperationUpgrade
	}

	previousAttempt := ovnk8sOperator.Status.TargetVersion
	if previousAttempt != "" && previousAttempt != targetVersion {
		return rolloutOperationRecovery
	}
	if ovnk8sOperator.Status.UpgradeStatus == k8sv1alpha1.UpgradeStatusFailed ||
		ovnk8sOperator.Status.UpgradeStatus == k8sv1alpha1.UpgradeStatusInProgress {
		return rolloutOperationRecovery
	}

	return rolloutOperationSteady
}

// checkVersionAction checks version compatibility and returns the appropriate action.
// Target version and min upgrade version come from the embedded config, not the CR.
func (r *OVNK8sOperatorReconciler) checkVersionAction(ctx context.Context, ovnk8sOperator *k8sv1alpha1.OVNK8sOperator) (versionAction, error) {
	logger := log.FromContext(ctx)

	currentVersion, minUpgradeVersion, targetVersion, err := normalizeAndValidateVersions(
		ovnk8sOperator.Status.Version,
		r.EmbeddedConfig.MinUpgradeVersion,
		r.EmbeddedConfig.Version,
	)
	if err != nil {
		logger.Error(err, "Version normalization failed")
		return versionActionBlock, err
	}

	logger.V(1).Info("Checking version compatibility",
		"currentVersion", currentVersion,
		"targetVersion", targetVersion,
		"minUpgradeVersion", minUpgradeVersion)

	// Fresh installation - no current version
	if currentVersion == "" {
		logger.V(1).Info("Fresh installation detected")
		return versionActionRollout, nil
	}

	// Same release - reconcile without a rollout transition.
	if semver.Compare(currentVersion, targetVersion) == 0 {
		logger.V(2).Info("Current version matches target version")
		return versionActionReconcile, nil
	}

	// Validate version upgrade constraints
	if err := validateVersionUpgrade(currentVersion, minUpgradeVersion, targetVersion); err != nil {
		logger.Error(err, "Version upgrade validation failed")
		return versionActionBlock, err
	}

	logger.V(1).Info("Version upgrade validation passed")
	return versionActionRollout, nil
}

// validateVersionUpgrade validates whether currentVersion allows an upgrade to
// targetVersion.
func validateVersionUpgrade(currentVersion, minUpgradeVersion, targetVersion string) error {
	// Check if it is not a downgrade.
	if semver.Compare(targetVersion, currentVersion) < 0 {
		return fmt.Errorf("target version %s is less than current version %s, downgrade is not supported", targetVersion, currentVersion)
	}

	// Check if the current version is less than the minimum upgrade version for the given target version
	// Above condition ensures that the target version is greater than the current version
	// So, we can safely compare the current version with the minimum upgrade version
	if minUpgradeVersion != "" && semver.Compare(currentVersion, minUpgradeVersion) < 0 {
		return fmt.Errorf("current version %s is less than minimum upgrade version %s required to upgrade to %s",
			currentVersion, minUpgradeVersion, targetVersion)
	}
	return nil
}

// normalizeVersion ensures version has 'v' prefix required by semver package
func normalizeVersion(version string) string {
	if version == "" {
		return ""
	}
	// Handle uppercase 'V' prefix by converting to lowercase 'v'
	if strings.HasPrefix(version, "V") {
		return "v" + version[1:]
	}
	if !strings.HasPrefix(version, "v") {
		return "v" + version
	}
	return version
}

func normalizeAndValidateVersions(currentVersion, minUpgradeVersion, targetVersion string) (string, string, string, error) {
	current := normalizeVersion(currentVersion)
	minUpgrade := normalizeVersion(minUpgradeVersion)
	target := normalizeVersion(targetVersion)

	// Validate version formats
	if !semver.IsValid(target) {
		return "", "", "", fmt.Errorf("target version %s is not a valid semantic version", targetVersion)
	}
	if current != "" && !semver.IsValid(current) {
		return "", "", "", fmt.Errorf("current version %s is not a valid semantic version", currentVersion)
	}
	if minUpgrade != "" && !semver.IsValid(minUpgrade) {
		return "", "", "", fmt.Errorf("minimum upgrade version %s is not a valid semantic version", minUpgradeVersion)
	}

	return current, minUpgrade, target, nil
}
