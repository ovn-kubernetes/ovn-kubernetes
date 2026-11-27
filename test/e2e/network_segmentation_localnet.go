// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	utilnet "k8s.io/utils/net"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"

	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
)

var _ = Describe("Network Segmentation: Localnet", feature.NetworkSegmentation, func() {
	var (
		f           = wrappedTestFramework("network-segmentation-localnet")
		providerCtx infraapi.Context
	)
	f.SkipNamespaceCreation = true

	BeforeEach(func() {
		providerCtx = infraprovider.Get().NewTestContext()
	})

	// vlan == 0 exercises an untagged localnet (bridge without a VLAN
	// configuration); vlan > 0 exercises an access-VLAN localnet.
	DescribeTable("using ClusterUserDefinedNetwork CR, pods in different namespaces, should communicate over localnet topology", func(ctx SpecContext, vlan int) {
		const (
			testPort          = 9000
			subnetIPv4        = "192.168.100.0/24"
			subnetIPv6        = "2001:dbb::/64"
			excludeSubnetIPv4 = "192.168.100.0/29"
			excludeSubnetIPv6 = "2001:dbb::/120"
		)
		ovsBrName := "ovsbr-udn"
		// use unique names to avoid conflicts with tests running in parallel
		nsBlue := uniqueMetaName("blue")
		nsRed := uniqueMetaName("red")
		cudnName := uniqueMetaName("localnet-test")
		physicalNetworkName := uniqueMetaName("localnet1")

		By("setup the localnet underlay")
		c := networkAttachmentConfig{networkAttachmentConfigParams: networkAttachmentConfigParams{networkName: physicalNetworkName, vlanID: vlan}}
		Expect(providerCtx.SetupUnderlay(f, infraapi.Underlay{
			BridgeName:         ovsBrName,
			LogicalNetworkName: c.networkName,
			VlanID:             c.vlanID,
		})).To(Succeed())

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
			cidr:                joinStrings(subnetIPv4, subnetIPv6),
			excludeCIDRs:        []string{excludeSubnetIPv4, excludeSubnetIPv6},
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
		Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName)).WithTimeout(5*time.Second).WithPolling(time.Second).
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
	},
		Entry("with an access VLAN", 200),
		Entry("without a VLAN (untagged localnet)", 0),
	)

	// The startup pod sync deletes the logical switch ports it does not expect,
	// so a restart must keep the port of every running localnet pod: with no
	// IPAM, ovnkube-node writes the pod annotation; with subnets, the cluster
	// manager does.
	DescribeTable("should preserve the LSP of a localnet pod after ovnkube-node restart", func(ctx SpecContext, vlan int, ovsBrName, cidr string) {
		var (
			cudnName            = uniqueMetaName("restart-localnet")
			physicalNetworkName = uniqueMetaName("physnet-restart")
		)

		By("setup the localnet underlay")
		c := networkAttachmentConfig{networkAttachmentConfigParams: networkAttachmentConfigParams{networkName: physicalNetworkName, vlanID: vlan}}
		Expect(providerCtx.SetupUnderlay(f, infraapi.Underlay{
			BridgeName:         ovsBrName,
			LogicalNetworkName: c.networkName,
			VlanID:             c.vlanID,
		})).To(Succeed())

		By("create test namespace")
		namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework": f.BaseName,
		})
		f.Namespace = namespace
		Expect(err).NotTo(HaveOccurred())

		By("create the localnet CUDN CR")
		netConf := networkAttachmentConfigParams{
			name:                cudnName,
			physicalNetworkName: physicalNetworkName,
			vlanID:              vlan,
			cidr:                cidr, // empty: IPAM-less
			excludeCIDRs:        []string{},
		}

		cudnYAML := newLocalnetIPAMLessCUDNYaml(netConf, f.Namespace.Name)
		if cidr != "" {
			cudnYAML = newLocalnetCUDNYaml(netConf, f.Namespace.Name)
		}
		cleanup, err := createManifest("", cudnYAML)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			By("cleanup CUDN CR")
			cleanup()
			By(fmt.Sprintf("delete pods in namespace %q to unblock CUDN CR & associate NAD deletion", f.Namespace.Name))
			Expect(f.ClientSet.CoreV1().Pods(f.Namespace.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(Succeed())
			_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", cudnName, "--wait", fmt.Sprintf("--timeout=%ds", 120))
			Expect(err).NotTo(HaveOccurred())
		})
		Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName)).WithTimeout(5*time.Second).WithPolling(time.Second).
			Should(Succeed(), "CUDN CR is not ready")

		By("create test pod with the localnet network")
		podCfg := podConfiguration{
			name:        "test-restart-pod",
			namespace:   f.Namespace.Name,
			attachments: []nadapi.NetworkSelectionElement{{Name: cudnName}},
			labels: map[string]string{
				"app": "test-restart",
			},
		}
		testPod, err := f.ClientSet.CoreV1().Pods(podCfg.namespace).Create(context.Background(), generatePodSpec(podCfg), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			By("cleanup test pod")
			Expect(f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(context.Background(), testPod.Name, metav1.DeleteOptions{})).To(Succeed())
		})

		By("wait for pod to be running and annotated")
		Eventually(func() bool {
			pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), testPod.Name, metav1.GetOptions{})
			if err != nil {
				return false
			}
			// Check that pod has network annotation
			_, hasAnnotation := pod.Annotations["k8s.ovn.org/pod-networks"]
			return pod.Status.Phase == corev1.PodRunning && hasAnnotation
		}).WithTimeout(2*time.Minute).WithPolling(5*time.Second).Should(BeTrue(), "pod should be running and annotated")

		By("get node where pod is running")
		pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), testPod.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		nodeName := pod.Spec.NodeName
		framework.Logf("Pod is running on node: %s", nodeName)

		By("find ovnkube-node pod on the node where test pod is running")
		ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
		ovnkubeNodePods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.Background(), metav1.ListOptions{
			LabelSelector: "app=ovnkube-node",
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(ovnkubeNodePods.Items).NotTo(BeEmpty(), "should find ovnkube-node pod on node %s", nodeName)
		ovnkubeNodePod := ovnkubeNodePods.Items[0]
		framework.Logf("Found ovnkube-node pod: %s on node %s", ovnkubeNodePod.Name, nodeName)

		By("verify LSP exists in OVN NB database")
		// Expected LSP name format: GetUserDefinedNetworkPrefix(nadName) + composePortName(podNamespace, podName)
		// NAD name for CUDN is: <namespace>/<cudn-name>
		// GetUserDefinedNetworkPrefix replaces hyphens and slashes with dots and adds underscore
		nadName := fmt.Sprintf("%s/%s", f.Namespace.Name, cudnName)
		nadNameWithDots := strings.ReplaceAll(strings.ReplaceAll(nadName, "-", "."), "/", ".")
		expectedLSPName := fmt.Sprintf("%s_%s_%s", nadNameWithDots, f.Namespace.Name, testPod.Name)
		framework.Logf("Expected LSP name: %s", expectedLSPName)
		findLSP := func() (string, error) {
			stdout, stderr, err := ExecCommandInContainerWithFullOutput(f, ovnNamespace, ovnkubeNodePod.Name, "nb-ovsdb",
				"ovn-nbctl", "get", "logical-switch-port", expectedLSPName, "_uuid")
			if err != nil {
				return "", fmt.Errorf("failed to find LSP %w, stderr: %s", err, stderr)
			}
			return stdout, nil
		}
		expectedLSP, err := findLSP()
		Expect(err).ToNot(HaveOccurred())
		Expect(expectedLSP).ToNot(BeEmpty())

		By("restart ovnkube-node pod on the node where test pod is running")
		err = restartOVNKubeNodePod(f.ClientSet, deploymentconfig.Get().OVNKubernetesNamespace(), nodeName)
		Expect(err).NotTo(HaveOccurred(), "should successfully restart ovnkube-node pod")

		By("wait for ovnkube-node to complete sync")
		// Give ovnkube-node time to complete sync operation
		time.Sleep(30 * time.Second)

		By("find new ovnkube-node pod after restart")
		ovnkubeNodePods, err = f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.Background(), metav1.ListOptions{
			LabelSelector: "app=ovnkube-node",
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(ovnkubeNodePods.Items).NotTo(BeEmpty(), "should find ovnkube-node pod on node %s after restart", nodeName)
		ovnkubeNodePod = ovnkubeNodePods.Items[0]
		framework.Logf("Found new ovnkube-node pod after restart: %s on node %s", ovnkubeNodePod.Name, nodeName)

		By("verify the LSP was neither deleted nor re-created by the restart")
		Consistently(findLSP).WithTimeout(10*time.Second).WithPolling(2*time.Second).
			Should(Equal(expectedLSP), "LSP should not have being recreated after ovnkube-node restart")

		By("verify pod still has network annotation")
		pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), testPod.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, hasAnnotation := pod.Annotations["k8s.ovn.org/pod-networks"]
		Expect(hasAnnotation).To(BeTrue(), "pod should still have network annotation")
	},
		Entry("with no IPAM", 201, "ovsbr-ipamless", ""),
		Entry("with subnets", 202, "ovsbr-subnets", joinStrings("192.168.102.0/24", "2001:dbc::/64")),
	)

	// Two localnet CUDNs on different VLANs share a single trunk OVS bridge (a KinD
	// node has one secondary NIC, which cannot be a port on two bridges). Each VLAN
	// is a distinct physical network mapped to that same bridge; OVN performs the
	// per-CUDN VLAN tagging via the CUDN's vlanID.
	//
	// Both CUDNs deliberately use the SAME IP subnet prefix on DIFFERENT VLANs so a
	// pod on one VLAN is on-link with a pod's address on the other VLAN: the only
	// thing that can stop them from communicating is the VLAN separation itself. To
	// keep probes unambiguous, each VLAN allocates pod addresses from a DISJOINT
	// half of the shared prefix (VLAN 30 the lower half, VLAN 40 the upper half),
	// so a client can never probe its own address; the echo-server addresses are
	// excluded from both pools.
	When("two localnet CUDNs are on different VLANs over a shared trunk bridge", func() {
		const (
			testPort   = 9000
			egressPort = 9095

			underlayNetworkName = "underlay"
			trunkBridgeName     = "ovsbr-trunk"

			sharedSubnetV4 = "192.168.30.0/24"
			sharedSubnetV6 = "2001:db8:30::/120"

			vlan30 = 30
			vlan40 = 40

			vlan30EchoV4 = "192.168.30.1"
			vlan30EchoV6 = "2001:db8:30::1"
			vlan40EchoV4 = "192.168.30.2"
			vlan40EchoV6 = "2001:db8:30::2"
		)

		var (
			nsA, nsB                           string
			cudnVlan30, cudnVlan40             string
			server30, server40                 podConfiguration
			client30SameNode, client30DiffNode podConfiguration
			client40SameNode, client40DiffNode podConfiguration
			server30IPs, server40IPs           []string
			vlan30EchoIPs, vlan40EchoIPs       []string
		)

		// assertReachable asserts the client can reach the server over the same VLAN.
		assertReachable := func(server, client podConfiguration, serverIPs []string) {
			for _, serverIP := range serverIPs {
				By(fmt.Sprintf("asserting %s/%s can reach %s/%s at %s:%d (same VLAN)", client.namespace, client.name, server.namespace, server.name, serverIP, testPort))
				Eventually(func() error {
					return reachServerPodFromClient(f.ClientSet, server, client, serverIP, testPort)
				}).WithTimeout(2 * time.Minute).WithPolling(6 * time.Second).Should(Succeed())
			}
		}
		// resolveIface returns the localnet interface name for a client's attachment.
		resolveIface := func(client podConfiguration, cudnName string) string {
			pod, err := f.ClientSet.CoreV1().Pods(client.namespace).Get(context.Background(), client.name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "failed to get pod %s/%s", client.namespace, client.name)
			iface, err := getNetworkInterfaceName(pod, client, cudnName)
			Expect(err).NotTo(HaveOccurred(), "failed to resolve localnet interface for pod %s/%s", client.namespace, client.name)
			return iface
		}
		// assertIsolated probes over the client's own localnet interface. Because both
		// VLANs share one IP subnet, the target is on-link for the client, so the
		// probe exercises the L2 path and a failure is attributable to the VLAN
		// separation rather than to a missing route. It must fail with the curl
		// connection-timeout exit code (28) so an exec/pod failure cannot pass as
		// isolation, matching the established isolation checks in multihoming.go.
		assertIsolated := func(server, client podConfiguration, clientCUDN string, serverIPs []string) {
			iface := resolveIface(client, clientCUDN)
			for _, serverIP := range serverIPs {
				By(fmt.Sprintf("asserting %s/%s CANNOT reach %s/%s at %s:%d over %s (different VLAN, same subnet)", client.namespace, client.name, server.namespace, server.name, serverIP, testPort, iface))
				Consistently(func() error {
					return connectToServer(client, serverIP, testPort, "--interface", iface)
				}).WithTimeout(30*time.Second).WithPolling(6*time.Second).
					Should(MatchError(ContainSubstring("exit code 28")), "cross-VLAN probe must fail with a curl connection timeout, not an exec/pod error")
			}
		}
		// assertEgress asserts the client can reach an external echo server on its own VLAN.
		assertEgress := func(client podConfiguration, cudnName string, serverIPs []string) {
			iface := resolveIface(client, cudnName)
			for _, serverIP := range serverIPs {
				serverIP := serverIP
				By(fmt.Sprintf("asserting %s/%s can reach external echo server %s:%d over %s", client.namespace, client.name, serverIP, egressPort, iface))
				Eventually(func() error {
					return connectToServer(client, serverIP, egressPort, "--interface", iface)
				}).WithTimeout(2 * time.Minute).WithPolling(6 * time.Second).Should(Succeed())
			}
		}
		// assertEgressDenied is the negative counterpart: client must NOT reach the
		// other VLAN's external echo server. Per IP family, each denial poll also
		// confirms the denying client CAN still reach its own VLAN's echo server and
		// the destination echo server stays reachable from its own VLAN, so neither a
		// broken client egress path nor a crashed container can masquerade as
		// isolation. ownEchoIPs are the denying client's own-VLAN echo addresses,
		// family-aligned with serverIPs (both come from filterIPs in v4,v6 order).
		assertEgressDenied := func(client podConfiguration, cudnName string, ownEchoIPs []string, livenessClient podConfiguration, livenessCUDN string, serverIPs []string) {
			iface := resolveIface(client, cudnName)
			livenessIface := resolveIface(livenessClient, livenessCUDN)
			Expect(ownEchoIPs).To(HaveLen(len(serverIPs)), "own-VLAN and cross-VLAN echo IP lists must be family-aligned")
			for i, serverIP := range serverIPs {
				serverIP := serverIP
				ownEchoIP := ownEchoIPs[i]
				By(fmt.Sprintf("asserting %s/%s CANNOT reach the other VLAN's echo server %s:%d over %s (while both VLANs' echo servers stay reachable from their own VLAN)", client.namespace, client.name, serverIP, egressPort, iface))
				Consistently(func() error {
					if err := connectToServer(client, ownEchoIP, egressPort, "--interface", iface); err != nil {
						return fmt.Errorf("denying client %s/%s unexpectedly cannot reach its own VLAN echo server %s: %w", client.namespace, client.name, ownEchoIP, err)
					}
					if err := connectToServer(livenessClient, serverIP, egressPort, "--interface", livenessIface); err != nil {
						return fmt.Errorf("echo server %s unexpectedly unreachable from its own VLAN: %w", serverIP, err)
					}
					if err := connectToServer(client, serverIP, egressPort, "--interface", iface); !errorMatchesConnTimeout(err) {
						return fmt.Errorf("cross-VLAN probe to %s should fail with a curl connection timeout, got: %v", serverIP, err)
					}
					return nil
				}).WithTimeout(30 * time.Second).WithPolling(6 * time.Second).Should(Succeed())
			}
		}

		BeforeEach(func(ctx SpecContext) {
			By("getting two schedulable nodes to exercise same-node and different-node paths")
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, f.ClientSet, 2)
			Expect(err).NotTo(HaveOccurred(), "failed to list schedulable nodes")
			if len(nodes.Items) < 2 {
				e2eskipper.Skipf("this test requires at least 2 schedulable nodes, found %d", len(nodes.Items))
			}
			nodeA := nodes.Items[0].GetName()
			nodeB := nodes.Items[1].GetName()

			// use unique names to avoid conflicts with tests running in parallel
			nsA = uniqueMetaName("a1")
			nsB = uniqueMetaName("a2")
			cudnVlan30 = uniqueMetaName("sec-localnet-vlan30")
			cudnVlan40 = uniqueMetaName("sec-localnet-vlan40")
			physNetVlan30 := uniqueMetaName("mylocalnet-blue")
			physNetVlan40 := uniqueMetaName("mylocalnet-red")

			// VLAN 30 pods use the lower half of each family (IPv4 /25 of the /24,
			// IPv6 /121 of the /120); exclude the upper half plus the echo-server
			// addresses (which live in this lower half) so no pod is assigned them.
			vlan30Excludes := []string{
				"192.168.30.128/25", "2001:db8:30::80/121",
				vlan30EchoV4 + "/32", vlan30EchoV6 + "/128",
				vlan40EchoV4 + "/32", vlan40EchoV6 + "/128",
			}
			// VLAN 40 pods use the upper half of each family; exclude the lower half,
			// which already contains the echo-server addresses.
			vlan40Excludes := []string{"192.168.30.0/25", "2001:db8:30::/121"}

			// Set up a single trunk bridge carrying both VLANs, mapping both physical
			// networks to it. VlanID is left 0 so the OVS port stays a trunk; the VLAN
			// tagging is driven per-CUDN by OVN. SetupUnderlay is idempotent on the
			// bridge/port and merges bridge mappings, so the second call only adds the
			// second physical-network mapping to the same bridge.
			By("setting up the trunk localnet underlay, mapping the VLAN 30 physical network")
			Expect(providerCtx.SetupUnderlay(f, infraapi.Underlay{
				PhysicalNetworkName: underlayNetworkName,
				BridgeName:          trunkBridgeName,
				LogicalNetworkName:  physNetVlan30,
			})).To(Succeed())

			By("mapping the VLAN 40 physical network to the same trunk bridge")
			Expect(providerCtx.SetupUnderlay(f, infraapi.Underlay{
				PhysicalNetworkName: underlayNetworkName,
				BridgeName:          trunkBridgeName,
				LogicalNetworkName:  physNetVlan40,
			})).To(Succeed())

			By("creating the two test namespaces")
			for _, ns := range []string{nsA, nsB} {
				_, err := f.ClientSet.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred(), "failed to create namespace %s", ns)
				// Register cleanup right after each successful create so a partial
				// failure (e.g. the second namespace) still removes the first.
				ns := ns
				DeferCleanup(func() {
					By(fmt.Sprintf("cleaning up test namespace %q", ns))
					Expect(f.ClientSet.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})).To(Succeed())
				})
			}

			// Resolve the subnet families the cluster supports once, so the CUDN
			// config and the per-family address assertions stay consistent.
			supportedSubnets := filterCIDRs(f.ClientSet, sharedSubnetV4, sharedSubnetV6)
			Expect(supportedSubnets).NotTo(BeEmpty(), "cluster must support at least one IP family")
			sharedSubnetsJoined := joinStrings(supportedSubnets...)

			By("creating the two localnet CUDN CRs, one per VLAN, selecting both namespaces")
			cudnConfigs := map[string]networkAttachmentConfigParams{
				cudnVlan30: {
					name:                cudnVlan30,
					physicalNetworkName: physNetVlan30,
					vlanID:              vlan30,
					cidr:                sharedSubnetsJoined,
					excludeCIDRs:        filterCIDRs(f.ClientSet, vlan30Excludes...),
				},
				cudnVlan40: {
					name:                cudnVlan40,
					physicalNetworkName: physNetVlan40,
					vlanID:              vlan40,
					cidr:                sharedSubnetsJoined,
					excludeCIDRs:        filterCIDRs(f.ClientSet, vlan40Excludes...),
				},
			}
			for cudnName, netConf := range cudnConfigs {
				cudnName := cudnName
				cudnYAML := newLocalnetCUDNYaml(netConf, nsA, nsB)
				// Register CUDN (and pod) deletion BEFORE attempting creation, so the
				// resource is cleaned up even if the create response is lost after the
				// server already persisted it. The CUDN name is unique per spec, so a
				// delete can only ever affect this spec's own resource;
				// --ignore-not-found makes it a clean no-op if the create was rejected.
				DeferCleanup(func() {
					By(fmt.Sprintf("cleaning up CUDN CR %q", cudnName))
					// Delete pods first to unblock the CUDN/NAD finalizer, but do not let
					// a pod-cleanup failure abort before the cluster-scoped CUDN is
					// deleted: collect every error and assert them only afterwards.
					var cleanupErrs []error
					for _, ns := range []string{nsA, nsB} {
						if err := f.ClientSet.CoreV1().Pods(ns).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{}); err != nil {
							cleanupErrs = append(cleanupErrs, fmt.Errorf("failed to delete pods in namespace %q: %w", ns, err))
						}
					}
					if _, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", cudnName, "--ignore-not-found", "--wait", fmt.Sprintf("--timeout=%ds", 120)); err != nil {
						cleanupErrs = append(cleanupErrs, fmt.Errorf("failed to delete clusteruserdefinednetwork %q: %w", cudnName, err))
					}
					Expect(cleanupErrs).To(BeEmpty())
				})
				cleanup, err := createManifest("", cudnYAML)
				// Register the manifest file cleanup whenever it is non-nil, even if the
				// create was rejected, so the temp directory is never leaked.
				if cleanup != nil {
					DeferCleanup(cleanup)
				}
				Expect(err).NotTo(HaveOccurred(), "failed to create CUDN manifest for %s", cudnName)
				Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName)).WithTimeout(30*time.Second).WithPolling(time.Second).
					Should(Succeed(), "CUDN CR %q is not ready", cudnName)
			}

			// Create, per VLAN and per namespace, one http server pod and client pods
			// placed to cover same-node and different-node paths relative to the server.
			// Servers run in namespace nsA; clients run in nsB so we also exercise the
			// cross-namespace selection of the CUDN.
			mkPod := func(name, namespace, cudnName, node string, server bool) podConfiguration {
				cfg := podConfiguration{
					name:         name,
					namespace:    namespace,
					attachments:  []nadapi.NetworkSelectionElement{{Name: cudnName}},
					nodeSelector: map[string]string{"kubernetes.io/hostname": node},
				}
				if server {
					cfg.containerCmd = httpServerContainerCmd(testPort)
				}
				return cfg
			}

			By("creating http server and client pods on VLAN 30 and VLAN 40")
			server30 = mkPod("server-vlan30", nsA, cudnVlan30, nodeA, true)
			server40 = mkPod("server-vlan40", nsA, cudnVlan40, nodeA, true)
			client30SameNode = mkPod("client-vlan30-samenode", nsB, cudnVlan30, nodeA, false)
			client30DiffNode = mkPod("client-vlan30-diffnode", nsB, cudnVlan30, nodeB, false)
			client40SameNode = mkPod("client-vlan40-samenode", nsB, cudnVlan40, nodeA, false)
			client40DiffNode = mkPod("client-vlan40-diffnode", nsB, cudnVlan40, nodeB, false)

			allPods := []podConfiguration{server30, server40, client30SameNode, client30DiffNode, client40SameNode, client40DiffNode}
			for _, cfg := range allPods {
				_, err := f.ClientSet.CoreV1().Pods(cfg.namespace).Create(context.Background(), generatePodSpec(cfg), metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred(), "failed to create pod %s/%s on node %q", cfg.namespace, cfg.name, cfg.nodeSelector["kubernetes.io/hostname"])
			}
			for _, cfg := range allPods {
				Expect(e2epod.WaitForPodNameRunningInNamespace(context.Background(), f.ClientSet, cfg.name, cfg.namespace)).To(Succeed(),
					"pod %s/%s on node %q did not become running", cfg.namespace, cfg.name, cfg.nodeSelector["kubernetes.io/hostname"])
			}

			// expectV4/expectV6 reflect the families the CUDN subnets were configured
			// with, so a dual-stack cluster requires both an IPv4 and an IPv6 address
			// while single-stack requires only one.
			var expectV4, expectV6 bool
			for _, subnet := range supportedSubnets {
				if utilnet.IsIPv6CIDRString(subnet) {
					expectV6 = true
				} else {
					expectV4 = true
				}
			}
			assertAttachmentIPs := func(name string, ips []string) {
				var haveV4, haveV6 bool
				for _, ip := range ips {
					if utilnet.IsIPv6String(ip) {
						haveV6 = true
					} else {
						haveV4 = true
					}
				}
				if expectV4 {
					Expect(haveV4).To(BeTrue(), "%s should have an IPv4 address, got %v", name, ips)
				}
				if expectV6 {
					Expect(haveV6).To(BeTrue(), "%s should have an IPv6 address, got %v", name, ips)
				}
			}

			By("collecting the server pod IPs on their localnet attachment")
			server30IPs, err = podIPsForAttachment(f.ClientSet, server30.namespace, server30.name, cudnVlan30)
			Expect(err).NotTo(HaveOccurred(), "failed to list IPs for %s", server30.name)
			assertAttachmentIPs("server-vlan30", server30IPs)
			server40IPs, err = podIPsForAttachment(f.ClientSet, server40.namespace, server40.name, cudnVlan40)
			Expect(err).NotTo(HaveOccurred(), "failed to list IPs for %s", server40.name)
			assertAttachmentIPs("server-vlan40", server40IPs)

			// External echo servers (one per VLAN), each on the excluded address of
			// the shared subnet, reachable because the localnet bridges the VLAN onto
			// the underlay. Cover every IP family the cluster supports.
			vlan30EchoCIDRs := filterCIDRs(f.ClientSet, vlan30EchoV4+"/24", vlan30EchoV6+"/120")
			vlan40EchoCIDRs := filterCIDRs(f.ClientSet, vlan40EchoV4+"/24", vlan40EchoV6+"/120")
			vlan30EchoIPs = filterIPs(f.ClientSet, vlan30EchoV4, vlan30EchoV6)
			vlan40EchoIPs = filterIPs(f.ClientSet, vlan40EchoV4, vlan40EchoV6)

			By("starting an external echo server on each VLAN underlay")
			Expect(createLocalnetVLANEchoServer(providerCtx, underlayNetworkName, uniqueMetaName("echo-vlan30"), vlan30, vlan30EchoCIDRs, egressPort)).To(Succeed())
			Expect(createLocalnetVLANEchoServer(providerCtx, underlayNetworkName, uniqueMetaName("echo-vlan40"), vlan40, vlan40EchoCIDRs, egressPort)).To(Succeed())
		})

		It("allows pods on the same VLAN to communicate across nodes", func() {
			assertReachable(server30, client30SameNode, server30IPs)
			assertReachable(server30, client30DiffNode, server30IPs)
			assertReachable(server40, client40SameNode, server40IPs)
			assertReachable(server40, client40DiffNode, server40IPs)
		})

		It("isolates pods on different VLANs across nodes", func() {
			// VLAN 40 clients must not reach the VLAN 30 server...
			assertIsolated(server30, client40SameNode, cudnVlan40, server30IPs)
			assertIsolated(server30, client40DiffNode, cudnVlan40, server30IPs)
			// ...and VLAN 30 clients must not reach the VLAN 40 server.
			assertIsolated(server40, client30SameNode, cudnVlan30, server40IPs)
			assertIsolated(server40, client30DiffNode, cudnVlan30, server40IPs)
		})

		It("allows egress to the pod's own VLAN external echo server", func() {
			assertEgress(client30SameNode, cudnVlan30, vlan30EchoIPs)
			assertEgress(client30DiffNode, cudnVlan30, vlan30EchoIPs)
			assertEgress(client40SameNode, cudnVlan40, vlan40EchoIPs)
			assertEgress(client40DiffNode, cudnVlan40, vlan40EchoIPs)
		})

		It("denies egress to another VLAN's external echo server", func() {
			assertEgressDenied(client30SameNode, cudnVlan30, vlan30EchoIPs, client40SameNode, cudnVlan40, vlan40EchoIPs)
			assertEgressDenied(client30DiffNode, cudnVlan30, vlan30EchoIPs, client40SameNode, cudnVlan40, vlan40EchoIPs)
			assertEgressDenied(client40SameNode, cudnVlan40, vlan40EchoIPs, client30SameNode, cudnVlan30, vlan30EchoIPs)
			assertEgressDenied(client40DiffNode, cudnVlan40, vlan40EchoIPs, client30SameNode, cudnVlan30, vlan30EchoIPs)
		})

		// A NAD-based localnet on a SECOND physical network (mapped to the same
		// trunk bridge, but a distinct physicalNetworkName + VLAN + subnet) must be
		// isolated from the CUDN localnets. This proves localnet networks are
		// separated by their physical-network mapping, not merely co-located on one
		// bridge.
		When("a separate localnet NAD on a different physical network shares the bridge", func() {
			const (
				dnPhysNet   = "dn-mylocalnet"
				dnVlan      = 10
				dnSubnetV4  = "192.168.10.0/24"
				dnSubnetV6  = "2001:db8:10::/120"
				dnExcludeV4 = "192.168.10.1/32"
				dnExcludeV6 = "2001:db8:10::1/128"
			)
			var (
				nsDN     string
				dnNAD    string
				dnClient podConfiguration
				dnPodIPs []string
			)

			BeforeEach(func(ctx SpecContext) {
				nadClient, err := nadclient.NewForConfig(f.ClientConfig())
				Expect(err).NotTo(HaveOccurred(), "failed to build NAD client")

				By("mapping a second physical network to the same trunk bridge")
				Expect(providerCtx.SetupUnderlay(f, infraapi.Underlay{
					PhysicalNetworkName: underlayNetworkName,
					BridgeName:          trunkBridgeName,
					LogicalNetworkName:  dnPhysNet,
				})).To(Succeed())

				nsDN = uniqueMetaName("dn")
				_, err = f.ClientSet.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsDN}}, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred(), "failed to create namespace %s", nsDN)
				DeferCleanup(func() {
					By(fmt.Sprintf("cleaning up namespace %q", nsDN))
					Expect(f.ClientSet.CoreV1().Namespaces().Delete(context.Background(), nsDN, metav1.DeleteOptions{})).To(Succeed())
				})

				By("creating a localnet NAD on the second physical network")
				dnNAD = "sec-localnet-dn"
				nadCfg := newNetworkAttachmentConfig(networkAttachmentConfigParams{
					name:                dnNAD,
					namespace:           nsDN,
					topology:            "localnet",
					role:                "secondary",
					physicalNetworkName: dnPhysNet,
					vlanID:              dnVlan,
					cidr:                filterCIDRsAndJoin(f.ClientSet, joinStrings(dnSubnetV4, dnSubnetV6)),
					excludeCIDRs:        filterCIDRs(f.ClientSet, dnExcludeV4, dnExcludeV6),
					mtu:                 1500,
				})
				_, err = nadClient.NetworkAttachmentDefinitions(nsDN).Create(context.Background(), generateNAD(nadCfg, f.ClientSet), metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred(), "failed to create NAD %s/%s", nsDN, dnNAD)

				By("creating a pod on the second-physical-network localnet")
				dnClient = podConfiguration{
					name:        "dn-client",
					namespace:   nsDN,
					attachments: []nadapi.NetworkSelectionElement{{Name: dnNAD}},
				}
				_, err = f.ClientSet.CoreV1().Pods(dnClient.namespace).Create(context.Background(), generatePodSpec(dnClient), metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred(), "failed to create pod %s/%s", dnClient.namespace, dnClient.name)
				Expect(e2epod.WaitForPodNameRunningInNamespace(context.Background(), f.ClientSet, dnClient.name, dnClient.namespace)).To(Succeed(),
					"pod %s/%s did not become running", dnClient.namespace, dnClient.name)

				dnPodIPs, err = podIPsForAttachment(f.ClientSet, dnClient.namespace, dnClient.name, dnNAD)
				Expect(err).NotTo(HaveOccurred(), "failed to list IPs for %s", dnClient.name)
				Expect(dnPodIPs).NotTo(BeEmpty())
			})

			It("isolates the CUDN localnet pods from the other physical network's localnet pod", func() {
				// CUDN (VLAN 30 / VLAN 40) pods must not reach the other-physnet pod...
				assertIsolated(dnClient, client30SameNode, cudnVlan30, dnPodIPs)
				assertIsolated(dnClient, client40SameNode, cudnVlan40, dnPodIPs)
				// ...and the other-physnet pod must not reach the CUDN pods.
				assertIsolated(server30, dnClient, dnNAD, server30IPs)
				assertIsolated(server40, dnClient, dnNAD, server40IPs)
			})
		})
	})
})

// createLocalnetVLANEchoServer starts an agnhost echo server attached to the
// given underlay provider network with a VLAN subinterface on the container's
// primary interface holding the given serverCIDRs (one or both IP families, each
// "ip/prefix"). underlayNetworkName must match the PhysicalNetworkName the test
// passed to SetupUnderlay. Because the node's underlay NIC is attached to the
// localnet OVS bridge as a trunk port, the subinterface lands on the same
// L2/subnet as the localnet pods, so they can reach the server over their
// localnet interface. The provider context deletes the container on cleanup.
func createLocalnetVLANEchoServer(providerCtx infraapi.Context, underlayNetworkName, name string, vlanID int, serverCIDRs []string, port uint16) error {
	if len(serverCIDRs) == 0 {
		return fmt.Errorf("at least one server CIDR is required")
	}
	underlayNetwork, err := infraprovider.Get().GetNetwork(underlayNetworkName)
	if err != nil {
		return fmt.Errorf("failed to get underlay network %q: %w", underlayNetworkName, err)
	}
	parentIface := infraprovider.Get().ExternalContainerPrimaryInterfaceName()
	vlanIface := fmt.Sprintf("%s.%d", parentIface, vlanID)
	var cmd strings.Builder
	fmt.Fprintf(&cmd, "ip link add link %[1]s name %[2]s type vlan id %[3]d\n", parentIface, vlanIface, vlanID)
	fmt.Fprintf(&cmd, "ip link set dev %s up\n", vlanIface)
	for _, cidr := range serverCIDRs {
		fmt.Fprintf(&cmd, "ip addr add %s dev %s\n", cidr, vlanIface)
	}
	fmt.Fprintf(&cmd, "./agnhost netexec --http-port=%d\n", port)
	if _, err := providerCtx.CreateExternalContainer(infraapi.ExternalContainer{
		Name:       name,
		Image:      images.AgnHost(),
		Network:    underlayNetwork,
		Entrypoint: "bash",
		ExtPort:    port,
		CmdArgs:    []string{"-c", cmd.String()},
	}); err != nil {
		return fmt.Errorf("failed to create VLAN echo server container %s: %w", name, err)
	}
	return nil
}

func newLocalnetCUDNYaml(params networkAttachmentConfigParams, selectedNamespaces ...string) string {
	selectedNs := strings.Join(selectedNamespaces, ",")
	// The CRD rejects an empty excludeSubnets list, so leave the field out
	// when nothing is excluded.
	excludeSubnets := ""
	if len(params.excludeCIDRs) > 0 {
		excludeSubnets = "\n      excludeSubnets: [" + strings.Join(params.excludeCIDRs, ",") + "]"
	}
	// Emit the vlan block only when a VLAN is requested; vlanID 0 produces an
	// untagged localnet (bridge without a VLAN configuration).
	vlan := ""
	if params.vlanID > 0 {
		vlan = `
      vlan:
        mode: Access
        access: {id: ` + strconv.Itoa(params.vlanID) + `}`
	}
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
      subnets: [` + params.cidr + `]` + excludeSubnets + vlan + `
`
}

// newLocalnetIPAMLessCUDNYaml creates a ClusterUserDefinedNetwork YAML for IPAM-less localnet topology
func newLocalnetIPAMLessCUDNYaml(params networkAttachmentConfigParams, selectedNamespaces ...string) string {
	selectedNs := strings.Join(selectedNamespaces, ",")
	// For IPAM-less, we explicitly set ipam.mode to Disabled
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
      ipam:
        mode: Disabled
      vlan:
        mode: Access
        access: {id: ` + strconv.Itoa(params.vlanID) + `}
`
}

// errorMatchesConnTimeout reports whether err is a curl connection-timeout
// failure (exit code 28), i.e. the endpoint could not be reached — as opposed to
// a nil error (reached) or an exec/pod failure.
func errorMatchesConnTimeout(err error) bool {
	return err != nil && strings.Contains(err.Error(), "exit code 28")
}

// uniqueMetaName generate unique name from given string that complies with metadata object name.
func uniqueMetaName(originalName string) string {
	const randomStringLength = 5
	return fmt.Sprintf("%s-%s", originalName, rand.String(randomStringLength))
}
