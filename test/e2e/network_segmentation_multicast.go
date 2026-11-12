// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"strconv"
	"time"

	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
)

var _ = ginkgo.Describe("Network Segmentation: multicast", feature.NetworkSegmentation, func() {
	f := wrappedTestFramework("udn-multicast")
	f.SkipNamespaceCreation = true

	var (
		cs                             clientset.Interface
		nadClient                      nadclient.K8sCniCncfIoV1Interface
		clientNodeInfo, serverNodeInfo nodeInfo
	)

	ginkgo.BeforeEach(func() {
		cs = f.ClientSet

		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			ginkgo.Skip("requires at least 2 Nodes")
		}
		clientNodeInfo = nodeInfo{name: nodes.Items[0].Name}
		serverNodeInfo = nodeInfo{name: nodes.Items[1].Name}

		nadClient, err = nadclient.NewForConfig(f.ClientConfig())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		f.Namespace = namespace
	})

	// Layer3 only: the multicast-enabled annotation gates multicast on a Layer3
	// UDN. A flat Layer2 network floods multicast regardless of the annotation,
	// so the enable/disable gating does not apply there.
	ginkgo.DescribeTable("udn pods should receive multicast traffic only while multicast is enabled on the namespace", ginkgo.Serial, func(topology string) {
		ns := f.Namespace.Name
		createPrimaryUDNNAD(cs, nadClient, ns, "tenant-red", topology)
		group, _, _ := multicastGroups(cs)

		ginkgo.By("starting a multicast source on the UDN")
		newMulticastSourcePod(f, ns, "mcast-source", clientNodeInfo.name, group, udnPodInterface)

		ginkgo.By("verifying a listener does not receive traffic while multicast is disabled")
		newMulticastListenerPod(f, ns, "mcast-listener-disabled", serverNodeInfo.name, group, udnPodInterface)
		expectMulticastReceipt(f, ns, "mcast-listener-disabled", false)

		ginkgo.By("enabling multicast and verifying a listener now receives traffic")
		setNamespaceMulticastEnabled(cs, ns, true)
		newMulticastListenerPod(f, ns, "mcast-listener-enabled", serverNodeInfo.name, group, udnPodInterface)
		expectMulticastReceipt(f, ns, "mcast-listener-enabled", true)

		ginkgo.By("disabling multicast and verifying a listener no longer receives traffic")
		setNamespaceMulticastEnabled(cs, ns, false)
		newMulticastListenerPod(f, ns, "mcast-listener-redisabled", serverNodeInfo.name, group, udnPodInterface)
		expectMulticastReceipt(f, ns, "mcast-listener-redisabled", false)
	},
		ginkgo.Entry("on a primary layer3 UDN", "layer3"),
	)

	ginkgo.DescribeTable("udn pods can join different multicast groups at the same time", ginkgo.Serial, func(topology string) {
		ns := f.Namespace.Name
		createPrimaryUDNNAD(cs, nadClient, ns, "tenant-red", topology)
		groupA, groupB, groupC := multicastGroups(cs)
		setNamespaceMulticastEnabled(cs, ns, true)

		ginkgo.By("starting multicast sources on two different groups")
		newMulticastSourcePod(f, ns, "mcast-source-a", clientNodeInfo.name, groupA, udnPodInterface)
		newMulticastSourcePod(f, ns, "mcast-source-b", clientNodeInfo.name, groupB, udnPodInterface)

		ginkgo.By("verifying a listener on each group receives its own traffic")
		newMulticastListenerPod(f, ns, "mcast-listener-a", serverNodeInfo.name, groupA, udnPodInterface)
		newMulticastListenerPod(f, ns, "mcast-listener-b", serverNodeInfo.name, groupB, udnPodInterface)
		expectMulticastReceipt(f, ns, "mcast-listener-a", true)
		expectMulticastReceipt(f, ns, "mcast-listener-b", true)

		ginkgo.By("verifying a listener on a group with no source receives nothing")
		newMulticastListenerPod(f, ns, "mcast-listener-c", serverNodeInfo.name, groupC, udnPodInterface)
		expectMulticastReceipt(f, ns, "mcast-listener-c", false)
	},
		ginkgo.Entry("on a primary layer2 UDN", "layer2"),
		ginkgo.Entry("on a primary layer3 UDN", "layer3"),
	)

	ginkgo.DescribeTable("the same multicast group is isolated between separate udn namespaces", ginkgo.Serial, func(topology string) {
		ctx := context.Background()
		ns1 := f.Namespace.Name
		createPrimaryUDNNAD(cs, nadClient, ns1, "tenant-red", topology)

		ns2 := createMulticastUDNNamespace(ctx, cs, f, ns1+"-2")
		createPrimaryUDNNAD(cs, nadClient, ns2, "tenant-blue", topology)

		group, _, _ := multicastGroups(cs)
		setNamespaceMulticastEnabled(cs, ns1, true)
		setNamespaceMulticastEnabled(cs, ns2, true)

		ginkgo.By("verifying multicast works within the first namespace")
		newMulticastSourcePod(f, ns1, "mcast-source-ns1", clientNodeInfo.name, group, udnPodInterface)
		newMulticastListenerPod(f, ns1, "mcast-listener-ns1", serverNodeInfo.name, group, udnPodInterface)
		expectMulticastReceipt(f, ns1, "mcast-listener-ns1", true)

		ginkgo.By("verifying the second namespace does not receive the first namespace's traffic on the same group")
		newMulticastListenerPod(f, ns2, "mcast-listener-ns2", serverNodeInfo.name, group, udnPodInterface)
		expectMulticastReceipt(f, ns2, "mcast-listener-ns2", false)

		ginkgo.By("verifying the second namespace can independently use the same multicast group")
		newMulticastSourcePod(f, ns2, "mcast-source-ns2", clientNodeInfo.name, group, udnPodInterface)
		newMulticastListenerPod(f, ns2, "mcast-listener-ns2-own", serverNodeInfo.name, group, udnPodInterface)
		expectMulticastReceipt(f, ns2, "mcast-listener-ns2-own", true)
	},
		ginkgo.Entry("on primary layer2 UDNs", "layer2"),
		ginkgo.Entry("on primary layer3 UDNs", "layer3"),
	)

	// Layer3 only: a Layer3 CUDN keeps per-namespace multicast isolation. A
	// Layer2 CUDN is a single broadcast domain, so multicast floods across its
	// namespaces and this isolation does not hold.
	ginkgo.DescribeTable("multicast does not cross namespaces sharing a CUDN", ginkgo.Serial, func(topology string) {
		ctx := context.Background()
		ns1 := f.Namespace.Name
		ns2 := createMulticastUDNNamespace(ctx, cs, f, ns1+"-2")

		ginkgo.By("creating a primary CUDN spanning both namespaces")
		cudnName := fmt.Sprintf("cudn-multicast-%s", rand.String(5))
		cudnParams := networkAttachmentConfigParams{
			name:      cudnName,
			namespace: ns1 + "," + ns2,
			topology:  topology,
			cidr:      udnCIDRForTopology(topology),
			role:      "primary",
		}
		cleanupCUDN, err := createManifest("", generateClusterUserDefinedNetworkManifest(&cudnParams, cs))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func() {
			for _, ns := range []string{ns1, ns2} {
				gomega.Expect(cs.CoreV1().Pods(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{})).To(gomega.Succeed())
			}
			_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", cudnName, "--wait", "--timeout=120s")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			cleanupCUDN()
		})
		gomega.Eventually(clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(gomega.Succeed())

		group, _, _ := multicastGroups(cs)
		setNamespaceMulticastEnabled(cs, ns1, true)
		setNamespaceMulticastEnabled(cs, ns2, true)

		ginkgo.By("verifying multicast works within the first CUDN namespace")
		newMulticastSourcePod(f, ns1, "mcast-source-ns1", clientNodeInfo.name, group, udnPodInterface)
		newMulticastListenerPod(f, ns1, "mcast-listener-ns1", serverNodeInfo.name, group, udnPodInterface)
		expectMulticastReceipt(f, ns1, "mcast-listener-ns1", true)

		ginkgo.By("verifying a pod in the other CUDN namespace does not receive the traffic")
		newMulticastListenerPod(f, ns2, "mcast-listener-ns2", serverNodeInfo.name, group, udnPodInterface)
		expectMulticastReceipt(f, ns2, "mcast-listener-ns2", false)
	},
		ginkgo.Entry("on a layer3 CUDN", "layer3"),
	)
})

// multicastGroups returns three distinct multicast group addresses for the
// cluster's primary IP family.
func multicastGroups(cs clientset.Interface) (string, string, string) {
	if IsIPv6Cluster(cs) {
		return "ff3e::4321:1234", "ff3e::4321:1235", "ff3e::4321:1236"
	}
	return "224.3.3.3", "224.3.3.4", "224.3.3.5"
}

// setNamespaceMulticastEnabled toggles the ovn-kubernetes multicast annotation
// on the given namespace.
func setNamespaceMulticastEnabled(cs clientset.Interface, ns string, enabled bool) {
	ginkgo.GinkgoHelper()
	namespace, err := cs.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{})
	framework.ExpectNoError(err)
	if namespace.Annotations == nil {
		namespace.Annotations = map[string]string{}
	}
	namespace.Annotations["k8s.ovn.org/multicast-enabled"] = strconv.FormatBool(enabled)
	_, err = cs.CoreV1().Namespaces().Update(context.Background(), namespace, metav1.UpdateOptions{})
	framework.ExpectNoError(err)
}

// createMulticastUDNNamespace creates a namespace labeled for a primary user defined
// network and registers it for deletion.
func createMulticastUDNNamespace(ctx context.Context, cs clientset.Interface, f *framework.Framework, name string) string {
	ginkgo.GinkgoHelper()
	nsObj := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{RequiredUDNNamespaceLabel: ""},
		},
	}
	f.AddNamespacesToDelete(nsObj)
	_, err := cs.CoreV1().Namespaces().Create(ctx, nsObj, metav1.CreateOptions{})
	framework.ExpectNoError(err)
	return name
}

// udnCIDRForTopology returns the dual-stack CIDR string for a primary UDN of the
// given topology. 172.16.0.0/16 is used for IPv4 (not 172.18.0.0/16, which
// overlaps the KIND node network).
func udnCIDRForTopology(topology string) string {
	if topology == "layer2" {
		return joinStrings("172.16.0.0/16", "2014:100:200::0/60")
	}
	return primaryLayer3MultiCIDRs()
}

// createPrimaryUDNNAD creates a primary UDN NetworkAttachmentDefinition of the
// given topology in the given namespace.
func createPrimaryUDNNAD(cs clientset.Interface, nadClient nadclient.K8sCniCncfIoV1Interface, ns, name, topology string) {
	ginkgo.GinkgoHelper()
	params := networkAttachmentConfigParams{
		name:     name,
		topology: topology,
		cidr:     udnCIDRForTopology(topology),
		role:     "primary",
	}
	filterSupportedNetworkConfig(cs, &params)
	netConfig := newNetworkAttachmentConfig(params)
	netConfig.namespace = ns
	_, err := nadClient.NetworkAttachmentDefinitions(ns).Create(
		context.Background(),
		generateNAD(netConfig, cs),
		metav1.CreateOptions{},
	)
	framework.ExpectNoError(err)
}

// newMulticastSourcePod starts an iperf UDP multicast source for the given group
// on the given node and pod interface.
func newMulticastSourcePod(f *framework.Framework, ns, name, node, group, iface string) {
	ginkgo.GinkgoHelper()
	iperf := fmt.Sprintf("iperf -c %s%%%s -u -T 3 -t 3000 -i 5", group, iface)
	if IsIPv6Cluster(f.ClientSet) {
		iperf += " -V"
	}
	pod := newLatestAgnhostPod(ns, name, "/bin/sh", "-c", iperf)
	pod.Spec.NodeName = node
	e2epod.PodClientNS(f, ns).CreateSync(context.TODO(), pod)
}

// newMulticastListenerPod starts an iperf UDP multicast listener for the given
// group on the given node and pod interface.
func newMulticastListenerPod(f *framework.Framework, ns, name, node, group, iface string) {
	ginkgo.GinkgoHelper()
	iperf := fmt.Sprintf("iperf -s -B %s%%%s -u -t 180 -i 5", group, iface)
	if IsIPv6Cluster(f.ClientSet) {
		iperf += " -V"
	}
	pod := newLatestAgnhostPod(ns, name, "/bin/sh", "-c", iperf)
	pod.Spec.NodeName = node
	e2epod.PodClientNS(f, ns).CreateSync(context.TODO(), pod)
}

// expectMulticastReceipt asserts whether the named iperf listener pod receives
// multicast traffic (its log reports a "connected" client).
func expectMulticastReceipt(f *framework.Framework, ns, listener string, shouldReceive bool) {
	ginkgo.GinkgoHelper()
	logs := func() (string, error) {
		return e2epod.GetPodLogs(context.TODO(), f.ClientSet, ns, listener, listener)
	}
	if shouldReceive {
		gomega.Eventually(logs, 60*time.Second, 2*time.Second).Should(gomega.ContainSubstring("connected"))
	} else {
		gomega.Consistently(logs, 30*time.Second, 5*time.Second).ShouldNot(gomega.ContainSubstring("connected"))
	}
}
