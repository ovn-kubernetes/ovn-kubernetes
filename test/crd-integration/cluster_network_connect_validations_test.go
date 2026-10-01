// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario/clusternetworkconnect"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/feature"
)

var _ = Describe("ClusterNetworkConnect: API validations", feature.NetworkConnect, func() {
	DescribeTable("api-server should reject invalid ClusterNetworkConnect CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(func() {
				cleanupClusterNetworkConnectCRsTest(scenarios)
			})
			for _, s := range scenarios {
				By(s.Description)
				_, stderr, err := e2ekubectl.NewKubectlCommand("", "apply", "-f", "-").WithStdinData(s.Manifest).ExecWithFullOutput()
				Expect(err).To(HaveOccurred(), "should fail to create invalid ClusterNetworkConnect CR")
				Expect(stderr).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("Invalid network selector types", clusternetworkconnect.InvalidScenarios),
	)

	DescribeTable("api-server should accept valid ClusterNetworkConnect CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(func() {
				cleanupClusterNetworkConnectCRsTest(scenarios)
			})
			for _, s := range scenarios {
				By(s.Description)
				_, err := e2ekubectl.RunKubectlInput("", s.Manifest, "apply", "-f", "-")
				Expect(err).NotTo(HaveOccurred(), "should create valid ClusterNetworkConnect CR successfully")
			}
		},
		Entry("Valid ClusterNetworkConnect configurations", clusternetworkconnect.ValidScenarios),
	)
})

func cleanupClusterNetworkConnectCRsTest(scenarios []testscenario.ValidateCRScenario) {
	for _, s := range scenarios {
		e2ekubectl.RunKubectlInput("", s.Manifest, "delete", "-f", "-")
	}
	_, stderr, err := e2ekubectl.RunKubectlWithFullOutput("", "get", "clusternetworkconnects")
	Expect(err).NotTo(HaveOccurred())
	Expect(stderr).To(Equal("No resources found\n"))
}
