package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/testscenario"
	testscenariocudn "github.com/ovn-org/ovn-kubernetes/test/e2e/testscenario/cudn"
)

var _ = Describe("Network Segmentation: API validations", feature.NetworkSegmentation, func() {
	DescribeTable("api-server should reject invalid CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
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
		Entry("ClusterUserDefinedNetwork, mismatch topology and config", testscenariocudn.MismatchTopologyConfig),
		Entry("ClusterUserDefinedNetwork, localnet, invalid role", testscenariocudn.LocalnetInvalidRole),
		Entry("ClusterUserDefinedNetwork, localnet, invalid physicalNetworkName", testscenariocudn.LocalnetInvalidPhyNetName),
		Entry("ClusterUserDefinedNetwork, localnet, invalid subnets", testscenariocudn.LocalnetInvalidSubnets),
		Entry("ClusterUserDefinedNetwork, localnet, invalid mtu", testscenariocudn.LocalnetInvalidMTU),
		Entry("ClusterUserDefinedNetwork, localnet, invalid vlan", testscenariocudn.LocalnetInvalidVLAN),
		Entry("ClusterUserDefinedNetwork, layer2", testscenariocudn.Layer2CUDNInvalid),
		Entry("ClusterUserDefinedNetwork, evpn", feature.RouteAdvertisements, feature.EVPN, testscenariocudn.EVPNCUDNInvalid),
		Entry("UserDefinedNetwork, layer2", testscenariocudn.Layer2UDNInvalid),
		Entry("ClusterUserDefinedNetwork, no-overlay, invalid", testscenariocudn.NoOverlayInvalid),
	)

	DescribeTable("api-server should accept valid CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(func() {
				cleanupValidateCRsTest(scenarios)
			})
			for _, s := range scenarios {
				By(s.Description)
				_, err := e2ekubectl.RunKubectlInput("", s.Manifest, "apply", "-f", "-")
				Expect(err).NotTo(HaveOccurred(), "should create valid CR successfully")
			}
		},
		Entry("ClusterUserDefinedNetwork, localnet", testscenariocudn.LocalnetValid),
		Entry("ClusterUserDefinedNetwork, layer2", testscenariocudn.Layer2CUDNValid),
		Entry("ClusterUserDefinedNetwork, evpn", feature.RouteAdvertisements, feature.EVPN, testscenariocudn.EVPNCUDNValid),
		Entry("UserDefinedNetwork, layer2", testscenariocudn.Layer2UDNValid),
		Entry("ClusterUserDefinedNetwork, no-overlay, valid", testscenariocudn.NoOverlayValid),
	)
})

// runKubectlInputWithFullOutput is a convenience wrapper over kubectlBuilder that takes input to stdin
// It will also return the command's stderr.
func runKubectlInputWithFullOutput(namespace string, data string, args ...string) (string, string, error) {
	return e2ekubectl.NewKubectlCommand(namespace, args...).WithStdinData(data).ExecWithFullOutput()
}

func cleanupValidateCRsTest(scenarios []testscenario.ValidateCRScenario) {
	for _, s := range scenarios {
		e2ekubectl.RunKubectlInput("", s.Manifest, "delete", "-f", "-")
	}
	_, stderr, err := e2ekubectl.RunKubectlWithFullOutput("", "get", "clusteruserdefinednetworks")
	Expect(err).NotTo(HaveOccurred())
	Expect(stderr).To(Equal("No resources found\n"))
}
