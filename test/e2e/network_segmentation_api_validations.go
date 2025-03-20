package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/testdata"
	testdatacudn "github.com/ovn-org/ovn-kubernetes/test/e2e/testdata/cudn"
)

var _ = Describe("Network Segmentation: API validations", func() {
	DescribeTable("api-server should reject invalid CRs",
		func(scenarios []testdata.ValidateCRScenario) {
			DeferCleanup(func() {
				cleanupValidateCRsTest(scenarios)
			})
			for _, s := range scenarios {
				By(s.Description)
				_, stderr, err := runKubectlInputWithFullOutput("", s.Manifest, "create", "-f", "-")
				Expect(err).To(HaveOccurred(), "should fail to create invalid CR")
				Expect(stderr).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("ClusterUserDefinedNetwork, mismatch topology and config", testdatacudn.MismatchTopologyConfig),
		Entry("ClusterUserDefinedNetwork, localnet, invalid role", testdatacudn.LocalnetInvalidRole),
		Entry("ClusterUserDefinedNetwork, localnet, invalid physicalNetworkName", testdatacudn.LocalnetInvalidPhyNetName),
		Entry("ClusterUserDefinedNetwork, localnet, invalid subnets", testdatacudn.LocalnetInvalidSubnets),
		Entry("ClusterUserDefinedNetwork, localnet, invalid mtu", testdatacudn.LocalnetInvalidMTU),
		Entry("ClusterUserDefinedNetwork, localnet, invalid vlan", testdatacudn.LocalnetInvalidVLAN),
	)

	DescribeTable("api-server should accept valid CRs",
		func(scenarios []testdata.ValidateCRScenario) {
			DeferCleanup(func() {
				cleanupValidateCRsTest(scenarios)
			})
			for _, s := range scenarios {
				By(s.Description)
				_, err := e2ekubectl.RunKubectlInput("", s.Manifest, "apply", "-f", "-")
				Expect(err).NotTo(HaveOccurred(), "should create valid CR successfully")
			}
		},
		Entry("ClusterUserDefinedNetwork, localnet", testdatacudn.LocalnetValid),
	)
})

// runKubectlInputWithFullOutput is a convenience wrapper over kubectlBuilder that takes input to stdin
// It will also return the command's stderr.
func runKubectlInputWithFullOutput(namespace string, data string, args ...string) (string, string, error) {
	return e2ekubectl.NewKubectlCommand(namespace, args...).WithStdinData(data).ExecWithFullOutput()
}

func cleanupValidateCRsTest(scenarios []testdata.ValidateCRScenario) {
	for _, s := range scenarios {
		e2ekubectl.RunKubectlInput("", s.Manifest, "delete", "-f", "-")
	}
	_, stderr, err := e2ekubectl.RunKubectlWithFullOutput("", "get", "clusteruserdefinednetworks")
	Expect(err).NotTo(HaveOccurred())
	Expect(stderr).To(Equal("No resources found\n"))
}
