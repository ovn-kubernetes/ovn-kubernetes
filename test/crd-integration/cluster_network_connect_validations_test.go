// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario/clusternetworkconnect"
)

const clusterNetworkConnectSpecTimeout = 30 * time.Second

var _ = Describe("ClusterNetworkConnect: API validations", func() {
	DescribeTable("api-server should reject invalid ClusterNetworkConnect CRs",
		func(ctx SpecContext, scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(cleanupValidateCRsTest, scenarios, NodeTimeout(clusterNetworkConnectSpecTimeout))
			for _, s := range scenarios {
				By(s.Description)
				obj, err := testscenario.ValidateScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				err = k8sClient.Create(ctx, obj)
				Expect(err).To(HaveOccurred(), "should fail to create invalid ClusterNetworkConnect CR")
				Expect(err.Error()).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("Invalid network selector types", clusternetworkconnect.InvalidScenarios, SpecTimeout(clusterNetworkConnectSpecTimeout)),
	)

	DescribeTable("api-server should accept valid ClusterNetworkConnect CRs",
		func(ctx SpecContext, scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(cleanupValidateCRsTest, scenarios, NodeTimeout(clusterNetworkConnectSpecTimeout))
			for _, s := range scenarios {
				By(s.Description)
				obj, err := testscenario.ValidateScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				err = k8sClient.Create(ctx, obj)
				Expect(err).NotTo(HaveOccurred(), "should create valid ClusterNetworkConnect CR successfully")
			}
		},
		Entry("Valid ClusterNetworkConnect configurations", clusternetworkconnect.ValidScenarios, SpecTimeout(clusterNetworkConnectSpecTimeout)),
	)
})
