// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
	testscenarioobservability "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario/observabilityconfig"
)

var _ = Describe("ObservabilityConfig CRD", func() {
	DescribeTable("api-server should reject invalid CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(cleanupValidateCRsTest, scenarios)

			for _, s := range scenarios {
				By(s.Description)
				obj, err := testscenario.ValidateScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				err = k8sClient.Create(ctx, obj)
				Expect(err).To(HaveOccurred(), "should fail to create invalid CR")
				Expect(err.Error()).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("required fields", testscenarioobservability.RequiredFieldsInvalid),
		Entry("collectorID bounds", testscenarioobservability.CollectorIDInvalid),
		Entry("features", testscenarioobservability.FeaturesInvalid),
		Entry("probability bounds", testscenarioobservability.ProbabilityInvalid),
		Entry("filter", testscenarioobservability.FilterInvalid),
	)

	DescribeTable("api-server should accept valid CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(cleanupValidateCRsTest, scenarios)

			for _, s := range scenarios {
				By(s.Description)
				obj, err := testscenario.ValidateScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				Expect(k8sClient.Create(ctx, obj)).To(Succeed(), "should create valid CR successfully")
			}
		},
		Entry("valid configurations", testscenarioobservability.Valid),
	)
})
