package e2e

import (
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"k8s.io/kubernetes/test/e2e/framework"
)

var _ = ginkgo.Describe("OVS CPU affinity pinning", func() {

	f := wrappedTestFramework("ovspinning")

	ginkgo.It("can be enabled on specific nodes by creating enable_dynamic_cpu_affinity file", func() {

		nodeWithEnabledOvsAffinityPinning := "ovn-worker2"

		_, err := runCommand(containerRuntime, "exec", nodeWithEnabledOvsAffinityPinning, "bash", "-c", "echo 1 > /etc/openvswitch/enable_dynamic_cpu_affinity")
		gomega.Expect(err).ToNot(gomega.HaveOccurred())
		ovnKubeNs, err := getOVNKubeNamespaceName(f.ClientSet.CoreV1().Namespaces())
		framework.ExpectNoError(err, "failed to get ovn-kubernetes namespace")
		restartOVNKubeNodePodsInParallel(f.ClientSet, ovnKubeNs, "ovn-worker", "ovn-worker2")

		enabledNodeLogs, err := getOVNKubePodLogsFiltered(f.ClientSet, ovnKubeNs, "ovn-worker2", ".*ovspinning_linux.go.*$")
		gomega.Expect(err).ToNot(gomega.HaveOccurred())

		gomega.Expect(enabledNodeLogs).To(gomega.ContainSubstring("Starting OVS daemon CPU pinning"))

		disabledNodeLogs, err := getOVNKubePodLogsFiltered(f.ClientSet, ovnKubeNs, "ovn-worker", ".*ovspinning_linux.go.*$")
		gomega.Expect(err).ToNot(gomega.HaveOccurred())
		gomega.Expect(disabledNodeLogs).To(gomega.ContainSubstring("OVS CPU affinity pinning disabled"))
	})
})
