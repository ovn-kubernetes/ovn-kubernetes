package e2e

import (
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"

	"k8s.io/kubernetes/test/e2e/framework"
)

var _ = ginkgo.Describe("Network Policy: ICMP bypass", feature.NetworkPolicy, func() {
	f := wrappedTestFramework("network-policy-icmp")

	ginkgo.BeforeEach(func() {
		if !isICMPNetworkPolicyBypassEnabled() {
			ginkgo.Skip("Allow ICMP bypass with NetworkPolicy is not enabled, skipping ICMP bypass network policy tests")
		}
	})

	ginkgo.It("allows ICMP between pods with default deny policy on the default network", func() {
		namespace := f.Namespace.Name

		ginkgo.By("creating a \"default deny\" network policy")
		_, err := makeDenyAllPolicy(f, namespace, "deny-all")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("creating server and client pods")
		serverPodName := "icmp-server"
		clientPodName := "icmp-client"
		serverCmd := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
		clientCmd := []string{"/agnhost", "pause"}

		serverPod, err := createGenericPod(f, serverPodName, "", namespace, serverCmd)
		framework.ExpectNoError(err, "failed to create server pod")
		_, err = createGenericPod(f, clientPodName, "", namespace, clientCmd)
		framework.ExpectNoError(err, "failed to create client pod")

		clientConfig := podConfiguration{name: clientPodName, namespace: namespace}
		serverConfig := podConfiguration{name: serverPodName, namespace: namespace}

		ginkgo.By("verifying TCP is denied by the default deny policy")
		gomega.Eventually(func() error {
			return pokePod(f, clientPodName, serverPod.Status.PodIP)
		}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

		ginkgo.By("verifying ICMP is allowed between pods")
		serverIPs, err := podIPsFromStatus(f.ClientSet, namespace, serverPodName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		for _, serverIP := range serverIPs {
			gomega.Eventually(func() error {
				return pingServerPodFromClient(f.ClientSet, serverConfig, clientConfig, serverIP)
			}, 1*time.Minute, 6*time.Second).Should(gomega.Succeed())
		}
	})
})
