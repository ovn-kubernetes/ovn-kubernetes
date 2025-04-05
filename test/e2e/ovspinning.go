package e2e

import (
	"context"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deployment"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/provider"

	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
)

var _ = ginkgo.Describe("OVS CPU affinity pinning", func() {

	f := wrappedTestFramework("ovspinning")

	ginkgo.It("can be enabled on specific nodes by creating enable_dynamic_cpu_affinity file", func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
		framework.ExpectNoError(err)
		gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 1))
		nodeWithEnabledOvsAffinityPinning := nodes.Items[0].Name
		nodeWithDisabledOvsAffinityPinning := nodes.Items[1].Name

		_, err = provider.Get().ExecK8NodeCommand(nodeWithEnabledOvsAffinityPinning, []string{"bash", "-c", "echo 1 > /etc/openvswitch/enable_dynamic_cpu_affinity"})
		gomega.Expect(err).ToNot(gomega.HaveOccurred())

		err = restartOVNKubeNodePodsInParallel(f.ClientSet, deployment.Get().OVNKubernetesNamespace(), nodeWithEnabledOvsAffinityPinning, nodeWithDisabledOvsAffinityPinning)
		gomega.Expect(err).ToNot(gomega.HaveOccurred())

		enabledNodeLogs, err := getOVNKubePodLogsFiltered(f.ClientSet, deployment.Get().OVNKubernetesNamespace(),
			nodeWithEnabledOvsAffinityPinning, ".*ovspinning_linux.go.*$")
		gomega.Expect(err).ToNot(gomega.HaveOccurred())

		gomega.Expect(enabledNodeLogs).To(gomega.ContainSubstring("Starting OVS daemon CPU pinning"))

		disabledNodeLogs, err := getOVNKubePodLogsFiltered(f.ClientSet, deployment.Get().OVNKubernetesNamespace(),
			nodeWithDisabledOvsAffinityPinning, ".*ovspinning_linux.go.*$")
		gomega.Expect(err).ToNot(gomega.HaveOccurred())
		gomega.Expect(disabledNodeLogs).To(gomega.ContainSubstring("OVS CPU affinity pinning disabled"))
	})
})
