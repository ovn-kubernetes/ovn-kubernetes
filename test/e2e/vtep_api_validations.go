package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/testscenario"
	testscenariovtep "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/testscenario/vtep"
)

var _ = Describe("EVPN: VTEP API validations", feature.RouteAdvertisements, feature.EVPN, func() {
	DescribeTable("api-server should reject invalid VTEP CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(func() {
				cleanupVTEPCRsTest(scenarios)
			})
			for _, s := range scenarios {
				By(s.Description)
				_, stderr, err := runKubectlInputWithFullOutput("", s.Manifest, "create", "-f", "-")
				Expect(err).To(HaveOccurred(), "should fail to create invalid VTEP CR")
				Expect(stderr).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("Invalid VTEP configurations", testscenariovtep.Invalid),
	)

	DescribeTable("api-server should accept valid VTEP CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(func() {
				cleanupVTEPCRsTest(scenarios)
			})
			for _, s := range scenarios {
				By(s.Description)
				_, err := e2ekubectl.RunKubectlInput("", s.Manifest, "apply", "-f", "-")
				Expect(err).NotTo(HaveOccurred(), "should create valid VTEP CR successfully")
			}
		},
		Entry("Valid VTEP configurations", testscenariovtep.Valid),
	)
})

func cleanupVTEPCRsTest(scenarios []testscenario.ValidateCRScenario) {
	for _, s := range scenarios {
		e2ekubectl.RunKubectlInput("", s.Manifest, "delete", "-f", "-")
	}
	_, stderr, err := e2ekubectl.RunKubectlWithFullOutput("", "get", "vteps")
	Expect(err).NotTo(HaveOccurred())
	Expect(stderr).To(Equal("No resources found\n"))
}
