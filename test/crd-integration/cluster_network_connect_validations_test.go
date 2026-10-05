// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario/clusternetworkconnect"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/feature"
)

var _ = Describe("ClusterNetworkConnect: API validations", feature.NetworkConnect, func() {
	DescribeTable("api-server should reject invalid ClusterNetworkConnect CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(func() {
				cleanupValidateCRsTest(ctx, k8sClient, scenarios)
			})
			for _, s := range scenarios {
				By(s.Description)
				obj, err := testscenario.ValidateScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				err = k8sClient.Create(ctx, obj)
				Expect(err).To(HaveOccurred(), "should fail to create invalid ClusterNetworkConnect CR")
				Expect(err.Error()).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("Invalid network selector types", clusternetworkconnect.InvalidScenarios),
	)

	DescribeTable("api-server should accept valid ClusterNetworkConnect CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(func() {
				cleanupValidateCRsTest(ctx, k8sClient, scenarios)
			})
			for _, s := range scenarios {
				By(s.Description)
				obj, err := testscenario.ValidateScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				err = k8sClient.Create(ctx, obj)
				Expect(err).NotTo(HaveOccurred(), "should create valid ClusterNetworkConnect CR successfully")
			}
		},
		Entry("Valid ClusterNetworkConnect configurations", clusternetworkconnect.ValidScenarios),
	)
})
