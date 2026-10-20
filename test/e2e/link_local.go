// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"

	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	deploymentconfigapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
)

const (
	// linkLocalProbeDestination is an IPv4 link-local address outside the
	// masquerade subnet that nothing in the test environment owns.
	linkLocalProbeDestination = "169.254.200.200"
	// routableProbeDestination is a TEST-NET-1 address: nothing owns it, but
	// it is not link-local, so the node always forwards it.
	routableProbeDestination = "192.0.2.1"
)

// nodeExternalGateway returns the IPv4 gateway of the node on the primary
// network, the first router past the node.
func nodeExternalGateway(nodeName string) string {
	primaryNetwork, err := infraprovider.Get().PrimaryNetwork()
	Expect(err).NotTo(HaveOccurred(), "failed to get the primary network of the infrastructure provider")
	nodeInterface, err := infraprovider.Get().GetK8NodeNetworkInterface(nodeName, primaryNetwork)
	Expect(err).NotTo(HaveOccurred(), "failed to get the primary network interface of node %s", nodeName)
	externalGateway := nodeInterface.GetIPv4Gateway()
	Expect(externalGateway).NotTo(BeEmpty(), "node %s has no IPv4 external gateway on the primary network", nodeName)
	return externalGateway
}

// probeLeavesNode sends TTL-limited pings from the pod to dst and reports
// whether any ICMP reply came from the node's external gateway. The TTL range
// covers the OVN routers between the pod and that gateway in every topology.
func probeLeavesNode(f *framework.Framework, podName, externalGateway, dst string) bool {
	for ttl := 2; ttl <= 5; ttl++ {
		// a probe that leaves the node is answered with an ICMP error by the
		// external gateway; a dropped probe times out and ping exits non-zero.
		// The exec itself is bounded too, a stalled transport must not hang
		// the spec until the suite timeout
		output, err := e2ekubectl.NewKubectlCommand(f.Namespace.Name, "exec", podName, "--",
			"ping", "-n", "-c", "1", "-W", "2", "-t", fmt.Sprint(ttl), dst).
			WithTimeout(time.After(30 * time.Second)).Exec()
		framework.Logf("ping %s ttl %d from %s/%s: %s", dst, ttl, f.Namespace.Name, podName, strings.TrimSpace(output))
		// ping prints its statistics footer whatever the outcome; without it
		// the command did not run and a miss would be mistaken for a drop
		if !strings.Contains(output, "ping statistics") {
			framework.Failf("ping %s from %s/%s did not run: %v", dst, f.Namespace.Name, podName, err)
		}
		// ping reports the responder as "From <address> ", the trailing space
		// keeps 172.18.0.1 from matching 172.18.0.10
		if strings.Contains(output, "From "+externalGateway+" ") {
			return true
		}
	}
	return false
}

// checkLinkLocalForwarding verifies from the given pod that a probe to a
// routable destination leaves the node, and that a probe to a link-local
// destination leaves it only when expectDrop is false.
func checkLinkLocalForwarding(f *framework.Framework, podName, nodeName string, expectDrop bool) {
	externalGateway := nodeExternalGateway(nodeName)
	By("checking that a probe to a routable destination leaves the node")
	Expect(probeLeavesNode(f, podName, externalGateway, routableProbeDestination)).To(BeTrue(),
		"the external gateway did not answer a TTL-limited probe to %s", routableProbeDestination)

	if expectDrop {
		By("checking that a probe to a link-local destination does not leave the node")
		Expect(probeLeavesNode(f, podName, externalGateway, linkLocalProbeDestination)).To(BeFalse(),
			"the external gateway answered a probe to link-local %s with link-local forwarding disabled", linkLocalProbeDestination)
		return
	}
	By("checking that a probe to a link-local destination leaves the node")
	Expect(probeLeavesNode(f, podName, externalGateway, linkLocalProbeDestination)).To(BeTrue(),
		"the external gateway did not answer a probe to link-local %s with link-local forwarding enabled", linkLocalProbeDestination)
}

func newLinkLocalProbePod(f *framework.Framework, name string) *corev1.Pod {
	pod := e2epod.NewAgnhostPod(f.Namespace.Name, name, nil, nil, nil)
	pod.Spec.Containers[0].Image = images.Netshoot()
	pod.Spec.Containers[0].Command = []string{"sleep"}
	pod.Spec.Containers[0].Args = []string{"infinity"}
	return e2epod.NewPodClient(f).CreateSync(context.TODO(), pod)
}

var _ = Describe("Link-local forwarding", func() {
	f := wrappedTestFramework("link-local")

	BeforeEach(func() {
		if !isIPv4Supported(f.ClientSet) {
			Skip("link-local forwarding applies to IPv4 only")
		}
	})

	It("forwards or drops default network pod traffic to link-local destinations according to the cluster setting", func() {
		// The external gateway answers to the source address it sees. When
		// default network pod egress is not SNATed to the node, its answers
		// never come back and the probe is blind.
		if deploymentconfig.Get().IsConfigurationEnabled(deploymentconfigapi.NoOverlay) ||
			isDefaultNetworkAdvertised(defaultNetworkRouteAdvertisementName()) {
			Skip("default network pod egress is not SNATed to the node")
		}
		pod := newLinkLocalProbePod(f, "link-local-probe")
		// In local gateway mode default network pod egress enters the host
		// through the management port and follows the host routing table,
		// which the option does not touch.
		linkLocalForwardingDisabled := deploymentconfig.Get().IsConfigurationEnabled(deploymentconfigapi.LinkLocalForwardingDisabled)
		checkLinkLocalForwarding(f, pod.Name, pod.Spec.NodeName, linkLocalForwardingDisabled && !isLocalGWModeEnabled())
	})
})

var _ = Describe("Network Segmentation: link-local forwarding", feature.NetworkSegmentation, func() {
	f := wrappedTestFramework("link-local-udn")
	f.SkipNamespaceCreation = true

	BeforeEach(func() {
		if !isIPv4Supported(f.ClientSet) {
			Skip("link-local forwarding applies to IPv4 only")
		}
		namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		Expect(err).NotTo(HaveOccurred(), "failed to create a namespace for a primary UDN")
		f.Namespace = namespace
	})

	DescribeTable("forwards or drops primary UDN pod traffic to link-local destinations according to the cluster setting",
		func(ctx SpecContext, topology udnv1.NetworkTopology) {
			udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
			Expect(err).NotTo(HaveOccurred(), "failed to create a UDN client")
			udn := &udnv1.UserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: "link-local-" + strings.ToLower(string(topology)), Namespace: f.Namespace.Name},
				Spec:       udnv1.UserDefinedNetworkSpec{Topology: topology},
			}
			subnets := filterDualStackCIDRs(f.ClientSet, []udnv1.CIDR{"103.103.0.0/16", "2014:100:200::0/60"})
			switch topology {
			case udnv1.NetworkTopologyLayer3:
				udn.Spec.Layer3 = &udnv1.Layer3Config{Role: udnv1.NetworkRolePrimary}
				for _, subnet := range subnets {
					udn.Spec.Layer3.Subnets = append(udn.Spec.Layer3.Subnets, udnv1.Layer3Subnet{CIDR: subnet})
				}
			case udnv1.NetworkTopologyLayer2:
				udn.Spec.Layer2 = &udnv1.Layer2Config{Role: udnv1.NetworkRolePrimary, Subnets: subnets}
			}
			By("creating a primary UDN and waiting for it to be ready")
			udn, err = udnClient.K8sV1().UserDefinedNetworks(f.Namespace.Name).Create(ctx, udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "failed to create UDN %s", udn.Name)
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, udn.Namespace, udn.Name), 5*time.Second, time.Second).Should(Succeed())

			pod := newLinkLocalProbePod(f, "link-local-probe")
			checkLinkLocalForwarding(f, pod.Name, pod.Spec.NodeName,
				deploymentconfig.Get().IsConfigurationEnabled(deploymentconfigapi.LinkLocalForwardingDisabled))
		},
		Entry("Layer3", udnv1.NetworkTopologyLayer3),
		Entry("Layer2", udnv1.NetworkTopologyLayer2),
	)
})
