// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/defaults"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	version1       = "1.0.0"
	version2       = "2.0.0"
	invalidVersion = "invalid-version"
)

var _ = Describe("Version Management", func() {
	var (
		reconciler     *OVNK8sOperatorReconciler
		ctx            context.Context
		ovnk8sOperator *k8sv1alpha1.OVNK8sOperator
	)

	BeforeEach(func() {
		reconciler = &OVNK8sOperatorReconciler{
			EmbeddedConfig: &defaults.EmbeddedConfig{
				Version:           version1,
				MinUpgradeVersion: "",
			},
		}
		ctx = context.Background()
		ovnk8sOperator = &k8sv1alpha1.OVNK8sOperator{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-operator",
				Namespace: "test-namespace",
			},
		}
	})

	Describe("checkVersionAction", func() {
		Context("when current version is empty (fresh installation)", func() {
			It("should require a rollout", func() {
				ovnk8sOperator.Status.Version = ""
				reconciler.EmbeddedConfig.Version = version1

				action, err := reconciler.checkVersionAction(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
				Expect(action).To(Equal(versionActionRollout))
			})
		})

		Context("when current version matches target version", func() {
			It("should reconcile without a rollout", func() {
				ovnk8sOperator.Status.Version = version1
				reconciler.EmbeddedConfig.Version = version1

				action, err := reconciler.checkVersionAction(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
				Expect(action).To(Equal(versionActionReconcile))
			})
		})

		Context("when no minimum upgrade version is specified", func() {
			It("should require a rollout for upgrades", func() {
				ovnk8sOperator.Status.Version = version1
				reconciler.EmbeddedConfig.Version = version2
				reconciler.EmbeddedConfig.MinUpgradeVersion = ""

				action, err := reconciler.checkVersionAction(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
				Expect(action).To(Equal(versionActionRollout))
			})

			It("should block downgrades even when no minimum upgrade version is specified", func() {
				ovnk8sOperator.Status.Version = version2
				reconciler.EmbeddedConfig.Version = version1
				reconciler.EmbeddedConfig.MinUpgradeVersion = ""

				action, err := reconciler.checkVersionAction(ctx, ovnk8sOperator)
				Expect(err).To(HaveOccurred())
				Expect(action).To(Equal(versionActionBlock))
				Expect(err.Error()).To(ContainSubstring("target version v1.0.0 is less than current version v2.0.0, downgrade is not supported"))
			})
		})

		Context("when upgrade is valid", func() {
			It("should require a rollout", func() {
				ovnk8sOperator.Status.Version = "1.5.0"
				reconciler.EmbeddedConfig.Version = version2
				reconciler.EmbeddedConfig.MinUpgradeVersion = version1

				action, err := reconciler.checkVersionAction(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
				Expect(action).To(Equal(versionActionRollout))
			})
		})

		Context("when current version is below minimum upgrade version", func() {
			It("should block the upgrade", func() {
				ovnk8sOperator.Status.Version = "0.9.0"
				reconciler.EmbeddedConfig.Version = version2
				reconciler.EmbeddedConfig.MinUpgradeVersion = version1

				action, err := reconciler.checkVersionAction(ctx, ovnk8sOperator)
				Expect(err).To(HaveOccurred())
				Expect(action).To(Equal(versionActionBlock))
				Expect(err.Error()).To(ContainSubstring("current version v0.9.0 is less than minimum upgrade version v1.0.0 required to upgrade to v2.0.0"))
			})
		})

		Context("when attempting a downgrade", func() {
			It("should block the downgrade", func() {
				ovnk8sOperator.Status.Version = version2
				reconciler.EmbeddedConfig.Version = version1
				reconciler.EmbeddedConfig.MinUpgradeVersion = version1

				action, err := reconciler.checkVersionAction(ctx, ovnk8sOperator)
				Expect(err).To(HaveOccurred())
				Expect(action).To(Equal(versionActionBlock))
				Expect(err.Error()).To(ContainSubstring("target version v1.0.0 is less than current version v2.0.0, downgrade is not supported"))
			})
		})

		Context("when versions have invalid format", func() {
			It("should block an invalid current version", func() {
				ovnk8sOperator.Status.Version = invalidVersion
				reconciler.EmbeddedConfig.Version = version2
				reconciler.EmbeddedConfig.MinUpgradeVersion = version1

				action, err := reconciler.checkVersionAction(ctx, ovnk8sOperator)
				Expect(err).To(HaveOccurred())
				Expect(action).To(Equal(versionActionBlock))
				Expect(err.Error()).To(ContainSubstring("current version invalid-version is not a valid semantic version"))
			})

			It("should block an invalid target version", func() {
				ovnk8sOperator.Status.Version = version1
				reconciler.EmbeddedConfig.Version = invalidVersion
				reconciler.EmbeddedConfig.MinUpgradeVersion = version1

				action, err := reconciler.checkVersionAction(ctx, ovnk8sOperator)
				Expect(err).To(HaveOccurred())
				Expect(action).To(Equal(versionActionBlock))
				Expect(err.Error()).To(ContainSubstring("target version invalid-version is not a valid semantic version"))
			})

			It("should block an invalid minimum upgrade version", func() {
				ovnk8sOperator.Status.Version = version1
				reconciler.EmbeddedConfig.Version = version2
				reconciler.EmbeddedConfig.MinUpgradeVersion = invalidVersion

				action, err := reconciler.checkVersionAction(ctx, ovnk8sOperator)
				Expect(err).To(HaveOccurred())
				Expect(action).To(Equal(versionActionBlock))
				Expect(err.Error()).To(ContainSubstring("minimum upgrade version invalid-version is not a valid semantic version"))
			})
		})
	})

	DescribeTable("classifying rollout operations",
		func(currentVersion, previousTarget string, upgradeStatus k8sv1alpha1.UpgradeStatus, targetVersion string, action versionAction, expected rolloutOperation) {
			ovnk8sOperator.Status.Version = currentVersion
			ovnk8sOperator.Status.TargetVersion = previousTarget
			ovnk8sOperator.Status.UpgradeStatus = upgradeStatus

			Expect(classifyRolloutOperation(ovnk8sOperator, targetVersion, action)).To(Equal(expected))
		},
		Entry("fresh installation", "", "", k8sv1alpha1.UpgradeStatus(""), version1, versionActionRollout, rolloutOperationInstallation),
		Entry("version upgrade", version1, version1, k8sv1alpha1.UpgradeStatusCompleted, "1.1.0", versionActionRollout, rolloutOperationUpgrade),
		Entry("ordinary same-version reconciliation", version1, version1, k8sv1alpha1.UpgradeStatusCompleted, version1, versionActionReconcile, rolloutOperationSteady),
		Entry("recovery after reverting a failed upgrade", version1, "1.1.0", k8sv1alpha1.UpgradeStatusFailed, version1, versionActionReconcile, rolloutOperationRecovery),
		Entry("retry of a failed same-version recovery", version1, version1, k8sv1alpha1.UpgradeStatusFailed, version1, versionActionReconcile, rolloutOperationRecovery),
		Entry("blocked completed-release rollback", version2, version2, k8sv1alpha1.UpgradeStatusCompleted, version1, versionActionBlock, rolloutOperationBlocked),
	)

	Describe("validateVersionUpgrade", func() {
		Context("when versions are valid", func() {
			It("should allow upgrade when current version meets minimum requirement", func() {
				err := validateVersionUpgrade("v1.5.0", "v1.0.0", "v2.0.0")
				Expect(err).ToNot(HaveOccurred())
			})

			It("should allow upgrade when current version equals minimum requirement", func() {
				err := validateVersionUpgrade("v1.0.0", "v1.0.0", "v2.0.0")
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("when current version is below minimum", func() {
			It("should return error", func() {
				err := validateVersionUpgrade("v0.9.0", "v1.0.0", "v2.0.0")
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("current version v0.9.0 is less than minimum upgrade version v1.0.0 required to upgrade to v2.0.0"))
			})
		})

		Context("when attempting a downgrade", func() {
			It("should return error for downgrade", func() {
				err := validateVersionUpgrade("v2.0.0", "v1.0.0", "v1.0.0")
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("target version v1.0.0 is less than current version v2.0.0, downgrade is not supported"))
			})
		})

	})

	Describe("normalizeVersion", func() {
		Context("when version has 'v' prefix", func() {
			It("should return version as-is", func() {
				result := normalizeVersion("v1.0.0")
				Expect(result).To(Equal("v1.0.0"))
			})
		})

		Context("when version doesn't have 'v' prefix", func() {
			It("should add 'v' prefix", func() {
				result := normalizeVersion(version1)
				Expect(result).To(Equal("v1.0.0"))
			})
		})

		Context("when version is empty", func() {
			It("should return empty string", func() {
				result := normalizeVersion("")
				Expect(result).To(Equal(""))
			})
		})

		Context("when version has uppercase 'V' prefix", func() {
			It("should convert to lowercase 'v' prefix", func() {
				result := normalizeVersion("V1.0.0")
				Expect(result).To(Equal("v1.0.0"))
			})
		})

		Context("when version has complex format", func() {
			It("should handle pre-release versions", func() {
				result := normalizeVersion("1.0.0-alpha.1")
				Expect(result).To(Equal("v1.0.0-alpha.1"))
			})

			It("should handle build metadata", func() {
				result := normalizeVersion("1.0.0+build.1")
				Expect(result).To(Equal("v1.0.0+build.1"))
			})
		})
	})

	Describe("Edge Cases and Integration", func() {
		Context("when dealing with semantic version edge cases", func() {
			It("should handle three-part versions correctly", func() {
				reconciler.EmbeddedConfig.Version = "1.2.4"
				reconciler.EmbeddedConfig.MinUpgradeVersion = version1
				ovnk8sOperator.Status.Version = "1.2.3"

				action, err := reconciler.checkVersionAction(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
				Expect(action).To(Equal(versionActionRollout))
			})

			It("should handle major version upgrades", func() {
				reconciler.EmbeddedConfig.Version = version2
				reconciler.EmbeddedConfig.MinUpgradeVersion = version1
				ovnk8sOperator.Status.Version = "1.9.9"

				action, err := reconciler.checkVersionAction(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
				Expect(action).To(Equal(versionActionRollout))
			})
		})

		Context("when dealing with pre-release versions", func() {
			It("should handle pre-release version upgrades", func() {
				reconciler.EmbeddedConfig.Version = version1
				reconciler.EmbeddedConfig.MinUpgradeVersion = "1.0.0-alpha.1"
				ovnk8sOperator.Status.Version = "1.0.0-alpha.1"

				action, err := reconciler.checkVersionAction(ctx, ovnk8sOperator)
				Expect(err).ToNot(HaveOccurred())
				Expect(action).To(Equal(versionActionRollout))
			})
		})
	})
})
