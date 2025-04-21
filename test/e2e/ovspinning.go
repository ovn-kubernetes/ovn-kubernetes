package e2e

import (
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/helpers"
)

var _ = ginkgo.Describe("OVS CPU affinity pinning", func() {

	f := helpers.WrappedTestFramework("ovspinning")

	ginkgo.It("can be enabled on specific nodes by creating enable_dynamic_cpu_affinity file", func() {

		nodeWithEnabledOvsAffinityPinning := "ovn-worker2"

		_, err := helpers.RunCommand(helpers.ContainerRuntime, "exec", nodeWithEnabledOvsAffinityPinning, "bash", "-c", "echo 1 > /etc/openvswitch/enable_dynamic_cpu_affinity")
		gomega.Expect(err).ToNot(gomega.HaveOccurred())

		helpers.RestartOVNKubeNodePodsInParallel(f.ClientSet, helpers.OvnNamespace, "ovn-worker", "ovn-worker2")

		enabledNodeLogs, err := helpers.GetOVNKubePodLogsFiltered(f.ClientSet, helpers.OvnNamespace, "ovn-worker2", ".*ovspinning_linux.go.*$")
		gomega.Expect(err).ToNot(gomega.HaveOccurred())

		gomega.Expect(enabledNodeLogs).To(gomega.ContainSubstring("Starting OVS daemon CPU pinning"))

		disabledNodeLogs, err := helpers.GetOVNKubePodLogsFiltered(f.ClientSet, helpers.OvnNamespace, "ovn-worker", ".*ovspinning_linux.go.*$")
		gomega.Expect(err).ToNot(gomega.HaveOccurred())
		gomega.Expect(disabledNodeLogs).To(gomega.ContainSubstring("OVS CPU affinity pinning disabled"))
	})
})
