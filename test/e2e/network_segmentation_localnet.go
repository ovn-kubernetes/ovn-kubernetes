package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
)

var _ = Describe("Network Segmentation: Localnet", func() {
	f := wrappedTestFramework("network-segmentation-localnet")
	f.SkipNamespaceCreation = true

	It("using ClusterUserDefinedNetwork CR, pods in different namespaces, should communicate over localnet topology", func() {
		const (
			vlan               = 200
			testPort           = 9000
			subnetIPv4         = "192.168.100.0/24"
			subnetIPv6         = "2001:dbb::/64"
			excludeSubnetIPv4  = "192.168.100.0/29"
			excludeSubnetIPv6  = "2001:dbb::/120"
			secondaryIfaceName = "eth1"
			ovsBrName          = "ovsbr-eth1"
		)
		// use unique names to avoid conflicts with tests running in parallel
		nsBlue := uniqueMetaName("blue")
		nsRed := uniqueMetaName("red")
		cudnName := uniqueMetaName("localnet-test")
		physicalNetworkName := uniqueMetaName("localnet1")

		By("setup the localnet underlay")
		ovsPods := ovsPods(f.ClientSet)
		Expect(ovsPods).NotTo(BeEmpty())
		DeferCleanup(func() {
			By("teardown the localnet underlay")
			Expect(teardownUnderlay(ovsPods, ovsBrName)).To(Succeed())
		})
		c := networkAttachmentConfig{networkAttachmentConfigParams: networkAttachmentConfigParams{networkName: physicalNetworkName, vlanID: vlan}}
		Expect(setupUnderlay(ovsPods, ovsBrName, secondaryIfaceName, c.networkName, c.vlanID)).To(Succeed())

		By("create test namespaces")
		_, err := f.ClientSet.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsRed}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = f.ClientSet.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsBlue}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			By("cleanup test namespaces")
			Expect(f.ClientSet.CoreV1().Namespaces().Delete(context.Background(), nsBlue, metav1.DeleteOptions{})).To(Succeed())
			Expect(f.ClientSet.CoreV1().Namespaces().Delete(context.Background(), nsRed, metav1.DeleteOptions{})).To(Succeed())
		})

		By("create CR selecting the test namespaces")
		netConf := networkAttachmentConfigParams{
			name:                cudnName,
			physicalNetworkName: physicalNetworkName,
			vlanID:              vlan,
			cidr:                correctCIDRFamily(subnetIPv4, subnetIPv6),
			excludeCIDRs:        selectCIDRs(excludeSubnetIPv4, excludeSubnetIPv6),
		}
		cudnYAML := newLocalnetCUDNYaml(netConf, nsBlue, nsRed)
		cleanup, err := createManifest("", cudnYAML)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			By("cleanup CUDN CR")
			cleanup()
			By(fmt.Sprintf("delete pods in namespace %q to unblock CUDN CR & associate NAD deletion", nsBlue))
			Expect(f.ClientSet.CoreV1().Pods(nsBlue).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(Succeed())
			By(fmt.Sprintf("delete pods in namespace %q to unblock CUDN CR & associate NAD deletion", nsRed))
			Expect(f.ClientSet.CoreV1().Pods(nsRed).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(Succeed())
			_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", cudnName, "--wait", fmt.Sprintf("--timeout=%ds", 120))
			Expect(err).NotTo(HaveOccurred())
		})
		Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName)).WithTimeout(5*time.Second).WithPolling(time.Second).
			Should(Succeed(), "CUDN CR is not ready")

		By("create test pods")
		serverPodCfg := podConfiguration{
			name:         "test-server",
			namespace:    nsBlue,
			attachments:  []nadapi.NetworkSelectionElement{{Name: cudnName}},
			containerCmd: httpServerContainerCmd(testPort),
		}
		clientPodCfg := podConfiguration{
			name:        "test-client",
			namespace:   nsRed,
			attachments: []nadapi.NetworkSelectionElement{{Name: cudnName}},
		}
		serverPod, err := f.ClientSet.CoreV1().Pods(serverPodCfg.namespace).Create(context.Background(), generatePodSpec(serverPodCfg), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		clientPod, err := f.ClientSet.CoreV1().Pods(clientPodCfg.namespace).Create(context.Background(), generatePodSpec(clientPodCfg), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(e2epod.WaitForPodNameRunningInNamespace(context.Background(), f.ClientSet, clientPod.Name, clientPod.Namespace)).To(Succeed())
		Expect(e2epod.WaitForPodNameRunningInNamespace(context.Background(), f.ClientSet, serverPod.Name, serverPod.Namespace)).To(Succeed())

		By("assert pods interface's MTU is set with default MTU (1500)")
		for _, cfg := range []podConfiguration{serverPodCfg, clientPodCfg} {
			mtuRAW, err := e2ekubectl.RunKubectl(cfg.namespace, "exec", cfg.name, "--", "cat", "/sys/class/net/net1/mtu")
			Expect(err).NotTo(HaveOccurred())
			Expect(mtuRAW).To(ContainSubstring("1500"))
		}

		By("assert pods IPs not in exclude range")
		serverIPs, err := podIPsForAttachment(f.ClientSet, serverPodCfg.namespace, serverPodCfg.name, cudnName)
		Expect(err).NotTo(HaveOccurred())
		clientIPs, err := podIPsForAttachment(f.ClientSet, clientPodCfg.namespace, clientPodCfg.name, cudnName)
		Expect(err).NotTo(HaveOccurred())
		podIPs := append(serverIPs, clientIPs...)
		for _, excludedRange := range netConf.excludeCIDRs {
			for _, podIP := range podIPs {
				Expect(inRange(excludedRange, podIP)).To(
					MatchError(fmt.Errorf("ip [%s] is NOT in range %s", podIP, excludedRange)))
			}
		}

		for _, serverIP := range serverIPs {
			By(fmt.Sprintf("asserting the *client* pod can contact the server pod exposed endpoint [%s:%d]", serverIP, testPort))
			Eventually(func() error {
				return reachServerPodFromClient(f.ClientSet, serverPodCfg, clientPodCfg, serverIP, testPort)
			}).WithTimeout(2 * time.Minute).WithPolling(6 * time.Second).Should(Succeed())
		}
	})
})

func newLocalnetCUDNYaml(params networkAttachmentConfigParams, selectedNamespaces ...string) string {
	selectedNs := strings.Join(selectedNamespaces, ",")
	excludeSubnets := strings.Join(params.excludeCIDRs, ",")
	return `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: ` + params.name + `
spec:
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values: [ ` + selectedNs + `]
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: ` + params.physicalNetworkName + `
      subnets: [` + params.cidr + `]
      excludeSubnets: [` + excludeSubnets + `]
      vlan:
        mode: Access
        access: {id: ` + strconv.Itoa(params.vlanID) + `}
`
}

// uniqueMetaName generate unique name from given string that complies with metadata object name.
func uniqueMetaName(originalName string) string {
	const randomStringLength = 5
	return fmt.Sprintf("%s-%s", originalName, rand.String(randomStringLength))
}
