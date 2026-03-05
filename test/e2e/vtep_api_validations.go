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
		e2ekubectl.RunKubectlInput("", s.Manifest, "delete", "--ignore-not-found", "-f", "-")
	}
	// Verify each named resource is gone individually — a global "no resources found"
	// check is not parallel-safe since other concurrent tests may have live VTEPs.
	for _, s := range scenarios {
		if s.Name == "" {
			continue
		}
		stdout, _, err := e2ekubectl.RunKubectlWithFullOutput("", "get", "vtep", s.Name, "--ignore-not-found")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(BeEmpty(), "VTEP %q should have been deleted", s.Name)
	}
}
