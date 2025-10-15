package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/testscenario"
	testscenariocnc "github.com/ovn-org/ovn-kubernetes/test/e2e/testscenario/clusternetworkconnect"
)

var _ = Describe("ClusterNetworkConnect: API validations", feature.NetworkSegmentation, func() {
	DescribeTable("api-server should reject invalid ClusterNetworkConnect CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(func() {
				cleanupClusterNetworkConnectCRsTest(scenarios)
			})
			for _, s := range scenarios {
				By(s.Description)
				_, stderr, err := runKubectlInputWithFullOutput("", s.Manifest, "create", "-f", "-")
				Expect(err).To(HaveOccurred(), "should fail to create invalid ClusterNetworkConnect CR")
				Expect(stderr).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("Invalid network selector types", testscenariocnc.InvalidScenarios),
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
		Entry("Valid ClusterNetworkConnect configurations", testscenariocnc.ValidScenarios),
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
