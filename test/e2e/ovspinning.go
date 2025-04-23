package e2e

import (
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/testframework"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/inclustercommands"
)

var _ = ginkgo.Describe("OVS CPU affinity pinning", func() {

	f := testframework.WrappedTestFramework("ovspinning")

	ginkgo.It("can be enabled on specific nodes by creating enable_dynamic_cpu_affinity file", func() {

		nodeWithEnabledOvsAffinityPinning := "ovn-worker2"

		_, err := inclustercommands.RunCommand(testframework.ContainerRuntime, "exec", nodeWithEnabledOvsAffinityPinning, "bash", "-c", "echo 1 > /etc/openvswitch/enable_dynamic_cpu_affinity")
		gomega.Expect(err).ToNot(gomega.HaveOccurred())

		restartOVNKubeNodePodsInParallel(f.ClientSet, inclustercommands.OvnNamespace, "ovn-worker", "ovn-worker2")

		enabledNodeLogs, err := getOVNKubePodLogsFiltered(f.ClientSet, inclustercommands.OvnNamespace, "ovn-worker2", ".*ovspinning_linux.go.*$")
		gomega.Expect(err).ToNot(gomega.HaveOccurred())

		gomega.Expect(enabledNodeLogs).To(gomega.ContainSubstring("Starting OVS daemon CPU pinning"))

		disabledNodeLogs, err := getOVNKubePodLogsFiltered(f.ClientSet, inclustercommands.OvnNamespace, "ovn-worker", ".*ovspinning_linux.go.*$")
		gomega.Expect(err).ToNot(gomega.HaveOccurred())
		gomega.Expect(disabledNodeLogs).To(gomega.ContainSubstring("OVS CPU affinity pinning disabled"))
	})
})
