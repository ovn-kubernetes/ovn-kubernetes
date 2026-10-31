// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

type nodeInfo struct {
	name   string
	nodeIP string
}

var _ = ginkgo.Describe("Multicast", feature.Multicast, func() {

	fr := wrappedTestFramework("multicast")

	var (
		clientNodeInfo, serverNodeInfo nodeInfo
	)
	ginkgo.BeforeEach(func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), fr.ClientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			e2eskipper.Skipf(
				"Test requires >= 2 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}

		ips := e2enode.CollectAddresses(nodes, v1.NodeInternalIP)

		clientNodeInfo = nodeInfo{
			name:   nodes.Items[0].Name,
			nodeIP: ips[0],
		}

		serverNodeInfo = nodeInfo{
			name:   nodes.Items[1].Name,
			nodeIP: ips[1],
		}
	})
	ginkgo.Context("when multicast enabled for namespace", func() {
		ginkgo.BeforeEach(func() {
			enableMulticastForNamespace(fr)
		})

		ginkgo.It("should be able to send multicast UDP traffic between nodes", func() {
			testMulticastUDPTraffic(fr, clientNodeInfo, serverNodeInfo, defaultPodInterface)
		})
		ginkgo.It("should be able to receive multicast IGMP query", func() {
			testMulticastIGMPQuery(fr, clientNodeInfo, serverNodeInfo)
		})
		ginkgo.It("should preserve namespace and policy port-group membership when a pod is recreated", func(ctx ginkgo.SpecContext) {
			const podName = "recreated-pod"
			const policyName = "deny-recreated-pod"
			const pollTimeout = 60 * time.Second
			const pollInterval = time.Second
			namespace := fr.Namespace.Name
			ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
			dbPod, err := findOVNNBDBPod(fr.ClientSet, ovnNamespace)
			framework.ExpectNoError(err, "find the NBDB used for membership assertions")
			// In interconnect mode each node has its own database. Keep both
			// incarnations on the node whose NBDB we inspect.
			runNBCTL := func(args ...string) (string, error) {
				return runOVNNBCTLOnPod(fr, dbPod, append([]string{"--timeout=10"}, args...)...)
			}
			findGroup := func(ownerType, name, column string) (string, error) {
				return runNBCTL("--data=bare", "--no-heading", "--columns="+column, "find", "Port_Group",
					"external_ids:k8s.ovn.org/owner-controller=default-network-controller",
					"external_ids:k8s.ovn.org/owner-type="+ownerType, "external_ids:k8s.ovn.org/name="+name)
			}
			_, err = fr.ClientSet.NetworkingV1().NetworkPolicies(namespace).Create(ctx, &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: policyName},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": podName}},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err, "create the policy before either pod incarnation")
			gomega.Eventually(func() (string, error) {
				return findGroup("NetworkPolicy", namespace+":"+policyName, "_uuid")
			}, pollTimeout, pollInterval).ShouldNot(gomega.BeEmpty(), "wait for the selecting policy to be applied")

			create := func() *v1.Pod {
				pod, err := createGenericPodWithLabel(fr, podName, dbPod.Spec.NodeName, namespace,
					[]string{"/agnhost", "pause"}, map[string]string{"app": podName})
				framework.ExpectNoError(err, "create the pod on the inspected NBDB's node")
				return pod
			}
			assertMembership := func(pod *v1.Pod, staleUUID string) string {
				ginkgo.GinkgoHelper()
				var portUUID string
				gomega.Eventually(func(g gomega.Gomega) {
					var err error
					portUUID, err = runNBCTL("--data=bare", "--no-heading", "--columns=_uuid", "find", "Logical_Switch_Port",
						"name="+namespace+"_"+podName, "options:iface-id-ver="+string(pod.UID))
					g.Expect(err).NotTo(gomega.HaveOccurred(), "query the LSP belonging to pod UID %s", pod.UID)
					g.Expect(portUUID).NotTo(gomega.BeEmpty(), "the LSP must belong to the current pod UID")
					for ownerType, name := range map[string]string{"Namespace": namespace, "NetworkPolicy": namespace + ":" + policyName} {
						ports, lookupErr := findGroup(ownerType, name, "ports")
						g.Expect(lookupErr).NotTo(gomega.HaveOccurred(), "query %s port-group membership", ownerType)
						g.Expect(ports).To(gomega.ContainSubstring(portUUID), "%s must contain the current port", ownerType)
						if staleUUID != "" && staleUUID != portUUID {
							g.Expect(ports).NotTo(gomega.ContainSubstring(staleUUID), "%s must not retain the deleted port", ownerType)
						}
					}
				}, pollTimeout, pollInterval).Should(gomega.Succeed(), "reconcile namespace and policy membership for pod UID %s", pod.UID)
				return portUUID
			}
			oldPod := create()
			oldPortUUID := assertMembership(oldPod, "")
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, fr.ClientSet, oldPod), "delete the original pod")
			newPod := create()
			gomega.Expect(newPod.UID).NotTo(gomega.Equal(oldPod.UID))
			assertMembership(newPod, oldPortUUID)
		})
	})
})

func testMulticastUDPTraffic(fr *framework.Framework, clientNodeInfo, serverNodeInfo nodeInfo, iface string) {
	ginkgo.GinkgoHelper()
	const (
		mcastSource  = "pod-client"
		mcastServer1 = "pod-server1"
		mcastServer2 = "pod-server2"
		mcastServer3 = "pod-server3"
	)
	var (
		mcastGroupIPv4    = "224.3.3.3"
		mcastGroupIPv4Bad = "224.5.5.5"
		mcastGroupIPv6    = "ff3e::4321:1234"
		mcastGroupIPv6Bad = "ff3e::4321:1235"
	)

	mcastGroup := mcastGroupIPv4
	mcastGroupBad := mcastGroupIPv4Bad
	if IsIPv6Cluster(fr.ClientSet) {
		mcastGroup = mcastGroupIPv6
		mcastGroupBad = mcastGroupIPv6Bad
	}
	ns := fr.Namespace.Name
	// Start the multicast source (iperf client is the sender in multicast)
	ginkgo.By("creating a pod as a multicast source in node " + clientNodeInfo.name)
	// multicast group (-c 224.3.3.3), UDP (-u), TTL (-T 3), during (-t 3000) seconds, report every (-i 5) seconds
	iperf := fmt.Sprintf("iperf -c %s%%%s -u -T 3 -t 3000 -i 5", mcastGroup, iface)
	if IsIPv6Cluster(fr.ClientSet) {
		iperf = iperf + " -V"
	}
	cmd := []string{"/bin/sh", "-c", iperf}
	clientPod := newLatestAgnhostPod(fr.Namespace.Name, mcastSource, cmd...)
	clientPod.Spec.NodeName = clientNodeInfo.name
	e2epod.NewPodClient(fr).CreateSync(context.TODO(), clientPod)

	// Start a multicast listener on the same groups and verify it received the traffic (iperf server is the multicast listener)
	// join multicast group (-B 224.3.3.3), UDP (-u), during (-t 30) seconds, report every (-i 1) seconds
	ginkgo.By("creating first multicast listener pod in node " + serverNodeInfo.name)
	iperf = fmt.Sprintf("iperf -s -B %s%%%s -u -t 180 -i 5", mcastGroup, iface)
	if IsIPv6Cluster(fr.ClientSet) {
		iperf = iperf + " -V"
	}
	cmd = []string{"/bin/sh", "-c", iperf}
	mcastServerPod1 := newLatestAgnhostPod(fr.Namespace.Name, mcastServer1, cmd...)
	mcastServerPod1.Spec.NodeName = serverNodeInfo.name
	e2epod.NewPodClient(fr).CreateSync(context.TODO(), mcastServerPod1)

	// Start a multicast listener on on other group and verify it does not receive the traffic (iperf server is the multicast listener)
	// join multicast group (-B 224.4.4.4), UDP (-u), during (-t 30) seconds, report every (-i 1) seconds
	ginkgo.By("creating second multicast listener pod in node " + serverNodeInfo.name)
	iperf = fmt.Sprintf("iperf -s -B %s%%%s -u -t 180 -i 5", mcastGroupBad, iface)
	if IsIPv6Cluster(fr.ClientSet) {
		iperf = iperf + " -V"
	}
	cmd = []string{"/bin/sh", "-c", iperf}
	mcastServerPod2 := newLatestAgnhostPod(fr.Namespace.Name, mcastServer2, cmd...)
	mcastServerPod2.Spec.NodeName = serverNodeInfo.name
	e2epod.NewPodClient(fr).CreateSync(context.TODO(), mcastServerPod2)

	// Start a multicast listener on the same groups and verify it received the traffic (iperf server is the multicast listener)
	// join multicast group (-B 224.3.3.3), UDP (-u), during (-t 30) seconds, report every (-i 1) seconds
	ginkgo.By("creating first multicast listener pod in node " + clientNodeInfo.name)
	iperf = fmt.Sprintf("iperf -s -B %s%%%s -u -t 180 -i 5", mcastGroup, iface)
	if IsIPv6Cluster(fr.ClientSet) {
		iperf = iperf + " -V"
	}
	cmd = []string{"/bin/sh", "-c", iperf}
	mcastServerPod3 := newLatestAgnhostPod(fr.Namespace.Name, mcastServer3, cmd...)
	mcastServerPod3.Spec.NodeName = clientNodeInfo.name
	e2epod.NewPodClient(fr).CreateSync(context.TODO(), mcastServerPod3)

	ginkgo.By("checking if pod server1 received multicast traffic")
	gomega.Eventually(func() (string, error) {
		return e2epod.GetPodLogs(context.TODO(), fr.ClientSet, ns, mcastServer1, mcastServer1)
	},
		30*time.Second, 1*time.Second).Should(gomega.ContainSubstring("connected"))

	ginkgo.By("checking if pod server2 does not received multicast traffic")
	gomega.Eventually(func() (string, error) {
		return e2epod.GetPodLogs(context.TODO(), fr.ClientSet, ns, mcastServer2, mcastServer2)
	},
		30*time.Second, 1*time.Second).ShouldNot(gomega.ContainSubstring("connected"))

	ginkgo.By("checking if pod server3 received multicast traffic")
	gomega.Eventually(func() (string, error) {
		return e2epod.GetPodLogs(context.TODO(), fr.ClientSet, ns, mcastServer3, mcastServer3)
	},
		30*time.Second, 1*time.Second).Should(gomega.ContainSubstring("connected"))

}

func testMulticastIGMPQuery(f *framework.Framework, clientNodeInfo, serverNodeInfo nodeInfo) {
	ginkgo.GinkgoHelper()
	const (
		mcastGroup           string = "224.1.1.1"
		mcastV6Group         string = "ff3e::4321:1234"
		multicastListenerPod string = "multicast-listener-test-pod"
		multicastSourcePod   string = "multicast-source-test-pod"
		tcpdumpFileName      string = "tcpdump.txt"
		retryTimeout                = 5 * time.Minute // polling timeout
	)
	var (
		tcpDumpCommand = []string{"bash", "-c",
			fmt.Sprintf("apk update; apk add tcpdump ; tcpdump -i any igmp > %s", tcpdumpFileName)}
		// Multicast group (-c 224.1.1.1), UDP (-u), TTL (-T 2), during (-t 3000) seconds, report every (-i 5) seconds
		multicastSourceCommand = []string{"bash", "-c",
			fmt.Sprintf("iperf -c %s -u -T 2 -t 3000 -i 5", mcastGroup)}
	)

	// FIXME(trozet): the tcpdump filter is not correct for ipv6, it should be
	// 'icmp6 and (ip6[40] == 0x8a or ip6[40] == 0x8b or ip6[40] == 0x8c or ip6[40] == 0x8d)'
	// additionally this function needs to be modified like testMulticastUDPTraffic to specify correct pod interfaces
	if IsIPv6Cluster(f.ClientSet) {
		// Multicast group (-c ff3e::4321:1234), UDP (-u), TTL (-T 2), during (-t 3000) seconds, report every (-i 5) seconds, -V (Set the domain to IPv6)
		multicastSourceCommand = []string{"bash", "-c",
			fmt.Sprintf("iperf -c %s -u -T 2 -t 3000 -i 5 -V", mcastV6Group)}
	}
	// Create a multicast source pod
	ginkgo.By("creating a multicast source pod in node " + clientNodeInfo.name)
	_, err := createGenericPod(f, multicastSourcePod, clientNodeInfo.name, f.Namespace.Name, multicastSourceCommand)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	// Create a multicast listener pod
	ginkgo.By("creating a multicast listener pod in node " + serverNodeInfo.name)
	_, err = createGenericPod(f, multicastListenerPod, serverNodeInfo.name, f.Namespace.Name, tcpDumpCommand)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	// Wait for tcpdump on listener pod to be ready
	err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true /*immediate*/, func(context.Context) (bool, error) {
		kubectlOut, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", multicastListenerPod, "--", "/bin/bash", "-c", "ls")
		if err != nil {
			framework.Failf("failed to retrieve multicast IGMP query: %v", err.Error())
		}
		if !strings.Contains(kubectlOut, tcpdumpFileName) {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		framework.Failf("failed to retrieve multicast IGMP query: %v", err.Error())
	}

	ginkgo.By("verifying that the IGMP query has been received")
	// ovn-k leaves both mcast_idle_timeout and mcast_query_interval to default which means that
	// ovn-controller will send IGMP queries every 150 seconds.
	gomega.Eventually(func() (string, error) {
		return e2ekubectl.RunKubectl(f.Namespace.Name, "exec", multicastListenerPod, "--", "/bin/bash", "-c", fmt.Sprintf("grep igmp %s", tcpdumpFileName))
	}, 2*150*time.Second, 5*time.Second).Should(gomega.ContainSubstring("igmp"), "Failed to retrieve multicast IGMP query within the expected time")
}

func enableMulticastForNamespace(fr *framework.Framework) {
	ginkgo.GinkgoHelper()
	// annotate namespace to enable multicast
	namespace, err := fr.ClientSet.CoreV1().Namespaces().Get(context.Background(), fr.Namespace.Name, metav1.GetOptions{})
	framework.ExpectNoError(err, "Error getting Namespace %v: %v", fr.Namespace.Name, err)
	if namespace.ObjectMeta.Annotations == nil {
		namespace.ObjectMeta.Annotations = map[string]string{}
	}
	namespace.ObjectMeta.Annotations["k8s.ovn.org/multicast-enabled"] = "true"
	_, err = fr.ClientSet.CoreV1().Namespaces().Update(context.Background(), namespace, metav1.UpdateOptions{})
	framework.ExpectNoError(err, "Error updating Namespace %v: %v", fr.Namespace.Name, err)
}
