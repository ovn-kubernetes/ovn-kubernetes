// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	utilnet "k8s.io/utils/net"
)

const (
	uplinkPoll                      = time.Second
	uplinkShortTimeout              = 60 * time.Second
	uplinkTimeout                   = 240 * time.Second
	uplinkCurlMaxTime               = 1
	uplinkDPUGatewayNetworkEnv      = "OVN_TEST_DPU_UPLINK_NETWORK"
	uplinkDPUHostInterfaceNameEnv   = "OVN_TEST_DPU_UPLINK_HOST_INTERFACE"
	uplinkDPUExpectedBridgeEnv      = "OVN_TEST_DPU_UPLINK_EXPECTED_BRIDGE"
	uplinkDPUResourceNameEnv        = "OVN_TEST_DPU_UPLINK_RESOURCE_NAME"
	uplinkDPUResourceNameAnnotation = "k8s.v1.cni.cncf.io/resourceName"
	uplinkDPUHostAddrAnnotation     = "k8s.ovn.org/primary-dpu-host-addr"
	uplinkDPUHostNodeLabel          = "k8s.ovn.org/dpu-host"
	uplinkBGPServerIPv4CIDREnv      = "OVN_TEST_BGP_SERVER_NET_SUBNET_IPV4"
	uplinkBGPServerIPv6CIDREnv      = "OVN_TEST_BGP_SERVER_NET_SUBNET_IPV6"
	uplinkDefaultDPUResourceName    = "dpusim.io/vf"
	uplinkDefaultBGPServerIPv4CIDR  = "172.27.0.0/16"
	uplinkDefaultBGPServerIPv6CIDR  = "fc00:f853:ccd:e797::/64"
)

var uplinkGVR = schema.GroupVersionResource{
	Group:    "k8s.ovn.org",
	Version:  "v1alpha1",
	Resource: "uplinks",
}

var uplinkStateGVR = schema.GroupVersionResource{
	Group:    "k8s.ovn.org",
	Version:  "v1alpha1",
	Resource: "uplinkstates",
}

var uplinkFRRConfigurationGVR = schema.GroupVersionResource{
	Group:    "frrk8s.metallb.io",
	Version:  "v1beta1",
	Resource: "frrconfigurations",
}

type dpuHostAddrAnnotation struct {
	IPv4 string `json:"ipv4"`
	IPv6 string `json:"ipv6"`
}

var _ = ginkgo.Describe("Network Segmentation Uplink default-VRF egress", feature.NetworkSegmentation, func() {
	f := wrappedTestFramework("uplink-default")
	f.SkipNamespaceCreation = true

	var ictx infraapi.Context
	var ipFamilySet sets.Set[utilnet.IPFamily]
	var testSuffix string

	ginkgo.BeforeEach(func() {
		if IsGatewayModeLocal(f.ClientSet) {
			e2eskipper.Skipf("Uplink CUDN gateway plumbing is only supported in shared gateway mode")
		}
		if isDPUUplinkE2E() {
			e2eskipper.Skipf("default-VRF Uplink e2e uses regular KIND bridge provisioning")
		}
		ipFamilySet = sets.New(getSupportedIPFamiliesSlice(f.ClientSet)...)
		ictx = infraprovider.Get().NewTestContext()
		testSuffix = framework.RandomSuffix()
	})

	ginkgo.It("maps multiple CUDNs to the same Uplink bridge", func() {
		nodes, err := f.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		schedulableNodes, err := e2enode.GetBoundedReadySchedulableNodes(context.Background(), f.ClientSet, 2)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(schedulableNodes.Items).NotTo(gomega.BeEmpty())

		uplinkAlloc, err := allocators.AllocateBGP(f, ictx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		uplinkNetwork, nodeIfaces := setupUplinkNetwork(
			ictx,
			nodes.Items,
			ipFamilySet,
			"upnet"+testSuffix,
			[]string{uplinkAlloc.BGPPeerSubnet, uplinkAlloc.BGPPeerSubnet6},
		)

		bridgeName := uplinkBridgeName("updef" + testSuffix)
		gomega.Expect(configureUplinkBridge(f, ictx, bridgeName, nodeIfaces)).To(gomega.Succeed())
		gomega.Expect(configureUplinkBridgeDefaultRoutes(
			ictx,
			bridgeName,
			nodeIfaces,
		)).To(gomega.Succeed())

		uplinkName := "uplink" + testSuffix
		createUplink(f, ictx, uplinkName, nodes.Items, nodeIfaces, bridgeName)
		waitForUplinkStatesResolved(f, uplinkName, bridgeName, nodes.Items)
		waitForUplinkStatesDefaultGateways(f, uplinkName, nodes.Items, ipFamilySet)

		serverName := "upsrv" + testSuffix
		server, err := ictx.CreateExternalContainer(infraapi.ExternalContainer{
			Name:    serverName,
			Image:   images.AgnHost(),
			CmdArgs: []string{"netexec"},
			Network: uplinkNetwork,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		for i, networkName := range []string{"updefa" + testSuffix, "updefb" + testSuffix} {
			bgpAlloc, err := allocators.AllocateBGP(f, ictx)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			networkSpec := uplinkLayer3NetworkSpec(ipFamilySet, bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6)
			namespace, err := createUplinkNamespace(
				f,
				ictx,
				"uplink-default",
				networkName,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(createUplinkCUDN(
				f,
				ictx,
				namespace,
				networkName,
				networkSpec,
				nil,
				uplinkName,
			)).To(gomega.Succeed())

			pod := createUplinkNetexecPod(
				f,
				namespace.Name,
				"client-"+networkName,
				schedulableNodes.Items[i%len(schedulableNodes.Items)].Name,
			)
			for _, family := range ipFamilySet.UnsortedList() {
				serverIP := getFirstIPStringOfFamily(family, []string{server.IPv4, server.IPv6})
				gomega.Expect(serverIP).NotTo(gomega.BeEmpty())
				nodeIface, ok := nodeIfaces[pod.Spec.NodeName]
				gomega.Expect(ok).To(gomega.BeTrue(), "expected Uplink interface for node %s", pod.Spec.NodeName)
				expectedSourceIP := getFirstIPStringOfFamily(family, []string{nodeIface.IPv4, nodeIface.IPv6})
				gomega.Expect(expectedSourceIP).NotTo(gomega.BeEmpty())
				uplinkPodToClientIPAndExpect(pod, serverIP, expectedSourceIP)
			}
		}
	})
})

var _ = ginkgo.Describe("Network Segmentation Uplink route advertisements", feature.NetworkSegmentation, feature.RouteAdvertisements, func() {
	f := wrappedTestFramework("uplink-bgp")
	f.SkipNamespaceCreation = true

	var ictx infraapi.Context
	var ipFamilySet sets.Set[utilnet.IPFamily]
	var testSuffix string

	ginkgo.BeforeEach(func() {
		if IsGatewayModeLocal(f.ClientSet) {
			e2eskipper.Skipf("Uplink CUDN gateway plumbing is only supported in shared gateway mode")
		}
		ipFamilySet = sets.New(getSupportedIPFamiliesSlice(f.ClientSet)...)
		ictx = infraprovider.Get().NewTestContext()
		testSuffix = framework.RandomSuffix()
	})

	ginkgo.It("uses the Uplink interface as the targetVRF auto BGP peering path", func() {
		nodes, err := f.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		if isDPUUplinkE2E() {
			schedulableNodes, err := e2enode.GetReadySchedulableNodes(context.Background(), f.ClientSet)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(schedulableNodes.Items).NotTo(gomega.BeEmpty())
			runDPUUplinkVRFLiteRouteAdvertisements(
				f,
				ictx,
				schedulableNodes.Items,
				ipFamilySet,
				testSuffix,
			)
			return
		}

		schedulableNodes, err := e2enode.GetBoundedReadySchedulableNodes(context.Background(), f.ClientSet, 2)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(schedulableNodes.Items).NotTo(gomega.BeEmpty())

		bgpAlloc, err := allocators.AllocateBGP(f, ictx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		networkName := "upvrf" + testSuffix
		serverName := networkName + "-srv"
		serverNetworkName := serverName
		peerCIDRs := []string{bgpAlloc.BGPPeerSubnet, bgpAlloc.BGPPeerSubnet6}
		serverCIDRs := []string{bgpAlloc.IPVRFSubnet, bgpAlloc.IPVRFSubnet6}

		gomega.Expect(runBGPNetworkAndServer(
			f,
			ictx,
			ipFamilySet,
			networkName,
			serverName,
			serverNetworkName,
			peerCIDRs,
			serverCIDRs,
		)).To(gomega.Succeed())

		peerNetwork, err := infraprovider.Get().GetNetwork(networkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nodeIfaces := collectNodeNetworkInterfaces(nodes.Items, peerNetwork)

		bridgeName := uplinkBridgeName("upvrf" + testSuffix)
		gomega.Expect(configureUplinkBridge(f, ictx, bridgeName, nodeIfaces)).To(gomega.Succeed())

		createUplink(f, ictx, networkName, nodes.Items, nodeIfaces, bridgeName)
		waitForUplinkStatesResolved(f, networkName, bridgeName, nodes.Items)

		networkSpec := uplinkLayer3NetworkSpec(ipFamilySet, bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6)
		namespace, err := createUplinkAdvertisedCUDN(
			f,
			ictx,
			networkName,
			networkSpec,
			networkName,
			"auto",
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		var pods []*corev1.Pod
		for _, node := range schedulableNodes.Items {
			pods = append(pods, createUplinkNetexecPod(
				f,
				namespace.Name,
				"client-"+networkName+"-"+node.Name,
				node.Name,
			))
		}
		pod := pods[0]
		serverNetwork, err := infraprovider.Get().GetNetwork(serverNetworkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		serverIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(
			infraapi.ExternalContainer{Name: serverName},
			serverNetwork,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		frrIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(
			infraapi.ExternalContainer{Name: networkName + "-frr"},
			peerNetwork,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		for _, family := range ipFamilySet.UnsortedList() {
			serverCIDR := getFirstCIDRStringOfFamily(family, serverCIDRs)
			gomega.Expect(serverCIDR).NotTo(gomega.BeEmpty())
			frrIP := getFirstIPStringOfFamily(family, []string{frrIface.IPv4, frrIface.IPv6})
			gomega.Expect(frrIP).NotTo(gomega.BeEmpty())
			for _, node := range schedulableNodes.Items {
				gomega.Eventually(func() (bool, error) {
					return hasRouteInCUDNVRF(node, networkName, serverCIDR, frrIP)
				}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
					gomega.BeTrue(),
					"expected node %s to learn %s via %s in CUDN VRF %s",
					node.Name,
					serverCIDR,
					frrIP,
					networkName,
				)
			}

			serverIP := getFirstIPStringOfFamily(family, []string{serverIface.IPv4, serverIface.IPv6})
			gomega.Expect(serverIP).NotTo(gomega.BeEmpty())
			uplinkPodToHostnameAndExpect(pod, serverIP, serverName)
			podIP, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(
				f.ClientSet,
				pod.Namespace,
				pod.Name,
				networkName,
				family,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(podIP).NotTo(gomega.BeEmpty())
			uplinkPodToClientIPAndExpect(pod, serverIP, podIP)
		}
	})
})

var _ = ginkgo.Describe("Uplink route advertisements with Dynamic UDN allocation", feature.RouteAdvertisementsDynamicUDN, func() {
	f := wrappedTestFramework("uplink-dynamic-bgp")
	f.SkipNamespaceCreation = true

	var ictx infraapi.Context
	var ipFamilySet sets.Set[utilnet.IPFamily]
	var testSuffix string

	ginkgo.BeforeEach(func() {
		if IsGatewayModeLocal(f.ClientSet) {
			e2eskipper.Skipf("Uplink CUDN gateway plumbing is only supported in shared gateway mode")
		}
		ipFamilySet = sets.New(getSupportedIPFamiliesSlice(f.ClientSet)...)
		ictx = infraprovider.Get().NewTestContext()
		testSuffix = framework.RandomSuffix()
	})

	ginkgo.It("allows node-disjoint Dynamic CUDNs to share a targetVRF auto Uplink and rejects overlap", func() {
		if !isDynamicUDNEnabled() {
			e2eskipper.Skipf("test requires Dynamic UDN allocation")
		}

		schedulableNodes, err := e2enode.GetReadySchedulableNodes(context.Background(), f.ClientSet)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		availableNodes := schedulableNodes.Items
		if isDPUUplinkE2E() {
			availableNodes = filterNodesByLabel(availableNodes, uplinkDPUHostNodeLabel)
		}
		if len(availableNodes) < 2 {
			e2eskipper.Skipf("test requires at least two ready schedulable nodes")
		}
		nodes := availableNodes[:2]

		var nodeIfaces map[string]infraapi.NetworkInterface
		frrNeighborIPsByNode := map[string][]string{}
		var bridgeName, hostInterfaceName string
		if isDPUUplinkE2E() {
			nodeIfaces = collectDPUHostUplinkInterfaces(nodes)
			bridgeName = os.Getenv(uplinkDPUExpectedBridgeEnv)
			gomega.Expect(bridgeName).NotTo(gomega.BeEmpty(), "expected the DPU Uplink bridge name")

			dpuGatewayNetwork, err := infraprovider.Get().GetNetwork(os.Getenv(uplinkDPUGatewayNetworkEnv))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			frrIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(
				infraapi.ExternalContainer{Name: routerContainerName},
				dpuGatewayNetwork,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			frrNeighborIPs := matchIPStringsByIPFamilySet([]string{frrIface.IPv4, frrIface.IPv6}, ipFamilySet)
			gomega.Expect(frrNeighborIPs).NotTo(gomega.BeEmpty(), "expected an external FRR address on the DPU Uplink network")
			for _, node := range nodes {
				frrNeighborIPsByNode[node.Name] = frrNeighborIPs
			}
		} else {
			uplinkAlloc, err := allocators.AllocateBGP(f, ictx)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, nodeIfaces = setupUplinkNetwork(
				ictx,
				nodes,
				ipFamilySet,
				"updyn"+testSuffix,
				[]string{uplinkAlloc.BGPPeerSubnet, uplinkAlloc.BGPPeerSubnet6},
			)
			bridgeName = uplinkBridgeName("updyn" + testSuffix)
			hostInterfaceName = bridgeName
			gomega.Expect(configureUplinkBridge(f, ictx, bridgeName, nodeIfaces)).To(gomega.Succeed())
			for _, node := range nodes {
				iface := nodeIfaces[node.Name]
				ipv4Gateway, err := interfaceGateway(iface.IPv4Gateway, iface.IPv4, iface.IPv4Prefix)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ipv6Gateway, err := interfaceGateway(iface.IPv6Gateway, iface.IPv6, iface.IPv6Prefix)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				frrNeighborIPsByNode[node.Name] = matchIPStringsByIPFamilySet(
					[]string{ipv4Gateway, ipv6Gateway},
					ipFamilySet,
				)
			}
		}

		uplinkName := "updyn" + testSuffix
		createUplink(f, ictx, uplinkName, nodes, nodeIfaces, hostInterfaceName)
		waitForUplinkStatesResolved(f, uplinkName, bridgeName, nodes)

		type networkOnNode struct {
			name      string
			namespace string
			node      corev1.Node
		}
		networks := []networkOnNode{
			{name: "upda" + testSuffix, node: nodes[0]},
			{name: "updb" + testSuffix, node: nodes[1]},
		}
		ginkgo.By(fmt.Sprintf(
			"creating node-disjoint Dynamic CUDNs %s on %s and %s on %s using Uplink %s with targetVRF auto",
			networks[0].name,
			networks[0].node.Name,
			networks[1].name,
			networks[1].node.Name,
			uplinkName,
		))
		for i := range networks {
			network := &networks[i]
			bgpAlloc, err := allocators.AllocateBGP(f, ictx)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			networkLabels := map[string]string{"advertise": network.name}
			namespace, err := createUplinkNamespace(f, ictx, "uplink-bgp", network.name)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			network.namespace = namespace.Name
			gomega.Expect(createUplinkCUDN(
				f,
				ictx,
				namespace,
				network.name,
				uplinkLayer3NetworkSpec(ipFamilySet, bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6),
				networkLabels,
				uplinkName,
			)).To(gomega.Succeed())

			createUplinkNetexecPod(f, namespace.Name, "client-"+network.name, network.node.Name)
			gomega.Expect(createNodeScopedUplinkFRRConfiguration(
				f,
				ictx,
				network.name,
				network.name,
				network.node,
				frrNeighborIPsByNode[network.node.Name],
			)).To(gomega.Succeed())
			gomega.Expect(createRouteAdvertisements(
				f,
				ictx,
				network.name,
				"auto",
				networkLabels,
				map[string]string{"network": network.name},
			)).To(gomega.Succeed())
		}

		ginkgo.By("verifying both node-disjoint Dynamic CUDNs use the shared Uplink without conflict")
		for _, network := range networks {
			gomega.Eventually(func() (string, error) {
				return getUplinkBridgeVRF(network.node.Name, bridgeName)
			}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
				gomega.Equal(network.name),
				"expected Uplink bridge %s on node %s to be attached to CUDN VRF %s",
				bridgeName,
				network.node.Name,
				network.name,
			)
			waitForCUDNUplinksReady(f, network.name)
		}

		owner := networks[0]
		conflicting := networks[1]
		ginkgo.By(fmt.Sprintf(
			"making CUDN %s active and advertised on node %s already used by CUDN %s",
			conflicting.name,
			owner.node.Name,
			owner.name,
		))
		createUplinkNetexecPod(
			f,
			conflicting.namespace,
			"client-"+conflicting.name+"-overlap",
			owner.node.Name,
		)
		gomega.Expect(createNodeScopedUplinkFRRConfiguration(
			f,
			ictx,
			conflicting.name+"-overlap",
			conflicting.name,
			owner.node,
			frrNeighborIPsByNode[owner.node.Name],
		)).To(gomega.Succeed())

		waitForUplinkStateGatewayCondition(
			f,
			uplinkName,
			owner.node.Name,
			metav1.ConditionFalse,
			"UplinkConfigurationConflict",
		)
		waitForCUDNUplinksCondition(
			f,
			conflicting.name,
			metav1.ConditionFalse,
			"UplinkConfigurationConflict",
		)
		gomega.Eventually(func() (string, error) {
			return getUplinkBridgeVRF(owner.node.Name, bridgeName)
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
			gomega.Equal(owner.name),
			"expected conflicting CUDN %s not to move Uplink bridge %s on node %s from VRF %s",
			conflicting.name,
			bridgeName,
			owner.node.Name,
			owner.name,
		)
	})
})

var _ = ginkgo.Describe("Network Segmentation Uplink route advertisements", feature.NetworkSegmentation, feature.RouteAdvertisements, func() {
	f := wrappedTestFramework("uplink-bgp")
	f.SkipNamespaceCreation = true

	var ictx infraapi.Context
	var ipFamilySet sets.Set[utilnet.IPFamily]
	var testSuffix string

	ginkgo.BeforeEach(func() {
		if IsGatewayModeLocal(f.ClientSet) {
			e2eskipper.Skipf("Uplink CUDN gateway plumbing is only supported in shared gateway mode")
		}
		ipFamilySet = sets.New(getSupportedIPFamiliesSlice(f.ClientSet)...)
		ictx = infraprovider.Get().NewTestContext()
		testSuffix = framework.RandomSuffix()
	})

	ginkgo.It("uses the default VRF as the BGP peering path", func() {
		if isDPUUplinkE2E() {
			e2eskipper.Skipf("default-VRF Uplink route advertisements use regular KIND bridge provisioning")
		}

		nodes, err := f.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		schedulableNodes, err := e2enode.GetBoundedReadySchedulableNodes(context.Background(), f.ClientSet, 2)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(schedulableNodes.Items).NotTo(gomega.BeEmpty())

		bgpAlloc, err := allocators.AllocateBGP(f, ictx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		networkName := "updefra" + testSuffix
		serverName := networkName + "-srv"
		serverNetworkName := serverName
		peerCIDRs := []string{bgpAlloc.BGPPeerSubnet, bgpAlloc.BGPPeerSubnet6}
		serverCIDRs := []string{bgpAlloc.IPVRFSubnet, bgpAlloc.IPVRFSubnet6}

		gomega.Expect(runBGPNetworkAndServerWithFRRVRF(
			f,
			ictx,
			ipFamilySet,
			networkName,
			serverName,
			serverNetworkName,
			peerCIDRs,
			serverCIDRs,
			"",
		)).To(gomega.Succeed())

		peerNetwork, err := infraprovider.Get().GetNetwork(networkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nodeIfaces := collectNodeNetworkInterfaces(nodes.Items, peerNetwork)

		bridgeName := uplinkBridgeName("updefra" + testSuffix)
		gomega.Expect(configureUplinkBridge(f, ictx, bridgeName, nodeIfaces)).To(gomega.Succeed())

		createUplink(f, ictx, networkName, nodes.Items, nodeIfaces, bridgeName)
		waitForUplinkStatesResolved(f, networkName, bridgeName, nodes.Items)

		networkSpec := uplinkLayer3NetworkSpec(ipFamilySet, bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6)
		namespace, err := createUplinkAdvertisedCUDN(
			f,
			ictx,
			networkName,
			networkSpec,
			networkName,
			"",
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		pod := createUplinkNetexecPod(
			f,
			namespace.Name,
			"client-"+networkName,
			schedulableNodes.Items[0].Name,
		)

		serverNetwork, err := infraprovider.Get().GetNetwork(serverNetworkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		serverIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(
			infraapi.ExternalContainer{Name: serverName},
			serverNetwork,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		frrIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(
			infraapi.ExternalContainer{Name: networkName + "-frr"},
			peerNetwork,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		for _, node := range nodes.Items {
			uplinkBridgeVRF, err := getNodeInterfaceVRF(node.Name, bridgeName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(uplinkBridgeVRF).To(gomega.BeEmpty(),
				"expected Uplink bridge %s on node %s to remain in the default VRF",
				bridgeName,
				node.Name,
			)
		}

		for _, family := range ipFamilySet.UnsortedList() {
			serverCIDR := getFirstCIDRStringOfFamily(family, serverCIDRs)
			gomega.Expect(serverCIDR).NotTo(gomega.BeEmpty())
			frrIP := getFirstIPStringOfFamily(family, []string{frrIface.IPv4, frrIface.IPv6})
			gomega.Expect(frrIP).NotTo(gomega.BeEmpty())
			for _, node := range schedulableNodes.Items {
				gomega.Eventually(func() (bool, error) {
					return hasRouteInDefaultVRF(node, serverCIDR, frrIP)
				}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
					gomega.BeTrue(),
					"expected node %s to learn %s via %s in the default VRF",
					node.Name,
					serverCIDR,
					frrIP,
				)
				gomega.Eventually(func() (bool, error) {
					return hasRouteInCUDNVRF(node, networkName, serverCIDR, frrIP)
				}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
					gomega.BeTrue(),
					"expected node %s to leak %s via %s into CUDN VRF %s",
					node.Name,
					serverCIDR,
					frrIP,
					networkName,
				)
			}

			serverIP := getFirstIPStringOfFamily(family, []string{serverIface.IPv4, serverIface.IPv6})
			gomega.Expect(serverIP).NotTo(gomega.BeEmpty())
			uplinkPodToHostnameAndExpect(pod, serverIP, serverName)
			podIP, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(
				f.ClientSet,
				pod.Namespace,
				pod.Name,
				networkName,
				family,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(podIP).NotTo(gomega.BeEmpty())
			uplinkPodToClientIPAndExpect(pod, serverIP, podIP)
			uplinkExternalToPodAndExpect(
				infraapi.ExternalContainer{Name: serverName},
				podIP,
				pod.Name,
			)
		}

		servicePod := createUplinkServicePod(
			f,
			namespace.Name,
			"server-"+networkName,
			schedulableNodes.Items[0].Name,
		)
		service := createUplinkNodePortService(f, namespace.Name, servicePod.Labels)
		primaryNetwork, err := infraprovider.Get().PrimaryNetwork()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		primaryIface, err := infraprovider.Get().GetK8NodeNetworkInterface(
			servicePod.Spec.NodeName,
			primaryNetwork,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		primaryClient, err := ictx.CreateExternalContainer(infraapi.ExternalContainer{
			Name:    "upclient" + testSuffix,
			Image:   images.AgnHost(),
			Network: primaryNetwork,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		peerClient, err := ictx.CreateExternalContainer(infraapi.ExternalContainer{
			Name:    "uppeer" + testSuffix,
			Image:   images.AgnHost(),
			Network: peerNetwork,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		defaultNamespace, err := createUplinkPlainNamespace(
			f,
			ictx,
			"uplink-bgp",
			"default-"+networkName,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defaultServicePod := createUplinkServicePod(
			f,
			defaultNamespace.Name,
			"default-server-"+networkName,
			schedulableNodes.Items[0].Name,
		)
		defaultService := createUplinkNodePortService(
			f,
			defaultNamespace.Name,
			defaultServicePod.Labels,
		)

		for _, family := range ipFamilySet.UnsortedList() {
			nodeIP := getFirstIPStringOfFamily(family, []string{primaryIface.IPv4, primaryIface.IPv6})
			if nodeIP != "" {
				uplinkExternalToNodePortAndExpect(
					primaryClient,
					nodeIP,
					service.Spec.Ports[0].NodePort,
					servicePod.Name,
				)
			}

			uplinkNodeIface, ok := nodeIfaces[servicePod.Spec.NodeName]
			gomega.Expect(ok).To(gomega.BeTrue(), "expected Uplink interface for node %s", servicePod.Spec.NodeName)
			nodeIP = getFirstIPStringOfFamily(family, []string{uplinkNodeIface.IPv4, uplinkNodeIface.IPv6})
			if nodeIP != "" {
				uplinkExternalToNodePortAndExpect(
					peerClient,
					nodeIP,
					service.Spec.Ports[0].NodePort,
					servicePod.Name,
				)
			}

			defaultUplinkNodeIface, ok := nodeIfaces[defaultServicePod.Spec.NodeName]
			gomega.Expect(ok).To(gomega.BeTrue(), "expected Uplink interface for node %s", defaultServicePod.Spec.NodeName)
			nodeIP = getFirstIPStringOfFamily(family, []string{defaultUplinkNodeIface.IPv4, defaultUplinkNodeIface.IPv6})
			if nodeIP != "" {
				uplinkExternalToNodePortAndExpect(
					peerClient,
					nodeIP,
					defaultService.Spec.Ports[0].NodePort,
					defaultServicePod.Name,
				)
			}
		}
	})
})

func runDPUUplinkVRFLiteRouteAdvertisements(
	f *framework.Framework,
	ictx infraapi.Context,
	schedulableNodes []corev1.Node,
	ipFamilySet sets.Set[utilnet.IPFamily],
	testSuffix string,
) {
	ginkgo.GinkgoHelper()

	if !ipFamilySet.Has(utilnet.IPv4) {
		e2eskipper.Skipf("DPU Uplink e2e requires IPv4 on the DPU simulator gateway network")
	}

	networkName := "upvrf" + testSuffix
	dpuGatewayNetworkName := os.Getenv(uplinkDPUGatewayNetworkEnv)
	dpuGatewayNetwork, err := infraprovider.Get().GetNetwork(dpuGatewayNetworkName)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	dpuHostNodes := filterNodesByLabel(schedulableNodes, uplinkDPUHostNodeLabel)
	gomega.Expect(dpuHostNodes).NotTo(gomega.BeEmpty(), "expected at least one ready schedulable DPU host node")
	nodeIfaces := collectDPUHostUplinkInterfaces(dpuHostNodes)

	createUplink(f, ictx, networkName, dpuHostNodes, nodeIfaces, "")
	waitForUplinkStatesResolved(f, networkName, os.Getenv(uplinkDPUExpectedBridgeEnv), dpuHostNodes)

	serverCIDRs := []string{
		envOrDefault(uplinkBGPServerIPv4CIDREnv, uplinkDefaultBGPServerIPv4CIDR),
		envOrDefault(uplinkBGPServerIPv6CIDREnv, uplinkDefaultBGPServerIPv6CIDR),
	}
	frrIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(
		infraapi.ExternalContainer{Name: routerContainerName},
		dpuGatewayNetwork,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(frrIface.IPv4).NotTo(gomega.BeEmpty())
	applyUplinkFRRK8sConfiguration(
		ictx,
		networkName,
		[]string{frrIface.IPv4},
		[]string{serverCIDRs[0]},
	)

	bgpAlloc, err := allocators.AllocateBGP(f, ictx)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	networkSpec := uplinkLayer3NetworkSpec(ipFamilySet, bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6)
	namespace, err := createUplinkAdvertisedCUDN(
		f,
		ictx,
		networkName,
		networkSpec,
		networkName,
		"auto",
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	var pods []*corev1.Pod
	for _, node := range dpuHostNodes {
		pods = append(pods, createUplinkNetexecPod(
			f,
			namespace.Name,
			"client-"+networkName+"-"+node.Name,
			node.Name,
		))
	}
	pod := pods[0]
	serverNetwork, err := infraprovider.Get().GetNetwork(bgpExternalNetworkName)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	serverIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(
		infraapi.ExternalContainer{Name: serverContainerName},
		serverNetwork,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	serverCIDR := getFirstCIDRStringOfFamily(utilnet.IPv4, serverCIDRs)
	gomega.Expect(serverCIDR).NotTo(gomega.BeEmpty())
	for _, node := range dpuHostNodes {
		gomega.Eventually(func() (bool, error) {
			return hasRouteInDPUCUDNVRF(node, networkName, serverCIDR, frrIface.IPv4)
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
			gomega.BeTrue(),
			"expected DPU for host node %s to learn %s via %s in CUDN VRF %s",
			node.Name,
			serverCIDR,
			frrIface.IPv4,
			networkName,
		)
	}

	gomega.Expect(serverIface.IPv4).NotTo(gomega.BeEmpty())
	uplinkPodToHostnameAndExpectReachable(pod, serverIface.IPv4)
	podIP, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(
		f.ClientSet,
		pod.Namespace,
		pod.Name,
		networkName,
		utilnet.IPv4,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(podIP).NotTo(gomega.BeEmpty())
	uplinkPodToClientIPAndExpect(pod, serverIface.IPv4, podIP)
}

func applyUplinkFRRK8sConfiguration(
	ictx infraapi.Context,
	networkName string,
	neighborIPs []string,
	receiveNetworks []string,
) {
	ginkgo.GinkgoHelper()

	frrK8sConfig, err := generateFRRk8sConfiguration(networkName, neighborIPs, receiveNetworks)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ictx.AddCleanUpFn(func() error { return os.RemoveAll(frrK8sConfig) })
	_, err = e2ekubectl.RunKubectl(
		deploymentconfig.Get().FRRK8sNamespace(),
		"create",
		"-f",
		frrK8sConfig,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ictx.AddCleanUpFn(func() error {
		_, err = e2ekubectl.RunKubectl(
			deploymentconfig.Get().FRRK8sNamespace(),
			"delete",
			"-f",
			frrK8sConfig,
		)
		return err
	})
}

func setupUplinkNetwork(
	ictx infraapi.Context,
	nodes []corev1.Node,
	ipFamilySet sets.Set[utilnet.IPFamily],
	fallbackNetworkName string,
	fallbackSubnets []string,
) (infraapi.Network, map[string]infraapi.NetworkInterface) {
	ginkgo.GinkgoHelper()

	network, err := ictx.CreateNetwork(
		fallbackNetworkName,
		matchCIDRStringsByIPFamilySet(fallbackSubnets, ipFamilySet)...,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	nodeIfaces := map[string]infraapi.NetworkInterface{}
	for _, node := range nodes {
		iface, err := ictx.AttachNetwork(network, node.Name)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nodeIfaces[node.Name] = iface
	}
	return network, nodeIfaces
}

func collectNodeNetworkInterfaces(nodes []corev1.Node, network infraapi.Network) map[string]infraapi.NetworkInterface {
	ginkgo.GinkgoHelper()

	nodeIfaces := map[string]infraapi.NetworkInterface{}
	for _, node := range nodes {
		iface, err := infraprovider.Get().GetK8NodeNetworkInterface(node.Name, network)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nodeIfaces[node.Name] = iface
	}
	return nodeIfaces
}

func filterNodesByLabel(nodes []corev1.Node, label string) []corev1.Node {
	out := make([]corev1.Node, 0, len(nodes))
	for _, node := range nodes {
		if _, ok := node.Labels[label]; ok {
			out = append(out, node)
		}
	}
	return out
}

func collectDPUHostUplinkInterfaces(nodes []corev1.Node) map[string]infraapi.NetworkInterface {
	ginkgo.GinkgoHelper()

	nodeIfaces := map[string]infraapi.NetworkInterface{}
	if hostInterfaceName := os.Getenv(uplinkDPUHostInterfaceNameEnv); hostInterfaceName != "" {
		for _, node := range nodes {
			nodeIfaces[node.Name] = infraapi.NetworkInterface{InfName: hostInterfaceName}
		}
		return nodeIfaces
	}

	for _, node := range nodes {
		rawAnnotation := node.Annotations[uplinkDPUHostAddrAnnotation]
		gomega.Expect(rawAnnotation).NotTo(
			gomega.BeEmpty(),
			"expected node %s to have annotation %s",
			node.Name,
			uplinkDPUHostAddrAnnotation,
		)
		var dpuHostAddr dpuHostAddrAnnotation
		gomega.Expect(json.Unmarshal([]byte(rawAnnotation), &dpuHostAddr)).To(gomega.Succeed())

		var iface infraapi.NetworkInterface
		if dpuHostAddr.IPv4 != "" {
			ip, prefix, cidr := parseDPUHostCIDR(dpuHostAddr.IPv4)
			iface.InfName = findNodeInterfaceByCIDR(node.Name, cidr)
			iface.IPv4 = ip
			iface.IPv4Prefix = prefix
		}
		if dpuHostAddr.IPv6 != "" {
			ip, prefix, cidr := parseDPUHostCIDR(dpuHostAddr.IPv6)
			if iface.InfName == "" {
				iface.InfName = findNodeInterfaceByCIDR(node.Name, cidr)
			}
			iface.IPv6 = ip
			iface.IPv6Prefix = prefix
		}
		gomega.Expect(iface.InfName).NotTo(
			gomega.BeEmpty(),
			"expected node %s annotation %s to resolve to a host interface",
			node.Name,
			uplinkDPUHostAddrAnnotation,
		)
		nodeIfaces[node.Name] = iface
	}
	return nodeIfaces
}

func parseDPUHostCIDR(cidr string) (string, string, string) {
	ginkgo.GinkgoHelper()

	ip, ipNet, err := net.ParseCIDR(cidr)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	prefix, _ := ipNet.Mask.Size()
	ipString := ip.String()
	prefixString := fmt.Sprint(prefix)
	return ipString, prefixString, ipString + "/" + prefixString
}

func findNodeInterfaceByCIDR(nodeName, cidr string) string {
	ginkgo.GinkgoHelper()

	out, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{
		"sh",
		"-c",
		fmt.Sprintf("ip -o addr show | awk '$4 == \"%s\" {print $2; exit}'", cidr),
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return strings.TrimSpace(out)
}

func configureUplinkBridge(
	f *framework.Framework,
	ictx infraapi.Context,
	bridgeName string,
	nodeIfaces map[string]infraapi.NetworkInterface,
) error {
	ginkgo.GinkgoHelper()

	ovsPods, err := uplinkOVSPodsByNode(f)
	if err != nil {
		return err
	}
	cleanupUplinkBridge := func() error {
		var errs []error
		ovsPods, err := uplinkOVSPodsByNode(f)
		if err != nil {
			return err
		}
		for nodeName := range nodeIfaces {
			ovsPod, ok := ovsPods[nodeName]
			if !ok {
				errs = append(errs, fmt.Errorf("failed to find ovnkube-node pod on node %q", nodeName))
				continue
			}
			if iface, ok := nodeIfaces[nodeName]; ok {
				if err := runNodeCommand(
					nodeName,
					"if ip link show dev %[1]s >/dev/null 2>&1 && "+
						"ip link show dev %[2]s >/dev/null 2>&1; then "+
						"for addr in $(ip -o -4 addr show dev %[1]s scope global | awk '{print $4}'); do "+
						"ip addr add $addr dev %[2]s 2>/dev/null || true; "+
						"ip addr del $addr dev %[1]s 2>/dev/null || true; done; "+
						"for addr in $(ip -o -6 addr show dev %[1]s scope global | awk '{print $4}'); do "+
						"ip addr add $addr dev %[2]s 2>/dev/null || true; "+
						"ip addr del $addr dev %[1]s 2>/dev/null || true; done; fi",
					bridgeName,
					iface.InfName,
				); err != nil {
					errs = append(errs, err)
				}
			}
			if err := runOVSCommand(
				ovsPod,
				"if ovs-vsctl --timeout=15 br-exists %[1]s; then "+
					"ovs-vsctl --timeout=15 del-br %[1]s; fi",
				bridgeName,
			); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	ictx.AddCleanUpFn(cleanupUplinkBridge)
	cleanupOnError := func(setupErr error) error {
		return errors.Join(setupErr, cleanupUplinkBridge())
	}

	for nodeName, iface := range nodeIfaces {
		ovsPod, ok := ovsPods[nodeName]
		if !ok {
			return cleanupOnError(fmt.Errorf("failed to find ovnkube-node pod on node %q", nodeName))
		}
		if err := runOVSCommand(ovsPod, "ovs-vsctl --timeout=15 --may-exist add-br %s", bridgeName); err != nil {
			return cleanupOnError(err)
		}
		if err := runOVSCommand(
			ovsPod,
			"ovs-vsctl --timeout=15 --may-exist add-port %s %s",
			bridgeName,
			iface.InfName,
		); err != nil {
			return cleanupOnError(err)
		}
		if err := runOVSCommand(
			ovsPod,
			"ovs-vsctl --timeout=15 br-set-external-id %s bridge-uplink %s",
			bridgeName,
			iface.InfName,
		); err != nil {
			return cleanupOnError(err)
		}
		if err := runNodeCommand(
			nodeName,
			"ip link set dev %[1]s up; "+
				"for addr in $(ip -o -4 addr show dev %[2]s scope global | awk '{print $4}'); do "+
				"ip addr add $addr dev %[1]s 2>/dev/null || true; "+
				"ip addr del $addr dev %[2]s 2>/dev/null || true; done; "+
				"for addr in $(ip -o -6 addr show dev %[2]s scope global | awk '{print $4}'); do "+
				"ip addr add $addr dev %[1]s 2>/dev/null || true; "+
				"ip addr del $addr dev %[2]s 2>/dev/null || true; done",
			bridgeName,
			iface.InfName,
		); err != nil {
			return cleanupOnError(err)
		}
	}
	return nil
}

func configureUplinkBridgeDefaultRoutes(
	ictx infraapi.Context,
	bridgeName string,
	nodeIfaces map[string]infraapi.NetworkInterface,
) error {
	ginkgo.GinkgoHelper()

	for nodeName, iface := range nodeIfaces {
		ipv4Gateway, err := interfaceGateway(
			iface.IPv4Gateway,
			iface.IPv4,
			iface.IPv4Prefix,
		)
		if err != nil {
			return fmt.Errorf("failed to resolve IPv4 gateway for node %s: %w",
				nodeName,
				err,
			)
		}
		if ipv4Gateway != "" {
			if err := runNodeCommand(
				nodeName,
				"ip route replace default via %s dev %s metric 50000",
				ipv4Gateway,
				bridgeName,
			); err != nil {
				return err
			}
			if err := runNodeCommand(
				nodeName,
				"ip route show default via %s dev %s metric 50000 | grep -q .",
				ipv4Gateway,
				bridgeName,
			); err != nil {
				return fmt.Errorf("failed to verify IPv4 default route via %s on %s/%s: %w",
					ipv4Gateway,
					nodeName,
					bridgeName,
					err,
				)
			}
		}
		ipv6Gateway, err := interfaceGateway(
			iface.IPv6Gateway,
			iface.IPv6,
			iface.IPv6Prefix,
		)
		if err != nil {
			return fmt.Errorf("failed to resolve IPv6 gateway for node %s: %w",
				nodeName,
				err,
			)
		}
		if ipv6Gateway != "" {
			if err := runNodeCommand(
				nodeName,
				"ip -6 route replace default via %s dev %s metric 50000",
				ipv6Gateway,
				bridgeName,
			); err != nil {
				return err
			}
			if err := runNodeCommand(
				nodeName,
				"ip -6 route show default via %s dev %s metric 50000 | grep -q .",
				ipv6Gateway,
				bridgeName,
			); err != nil {
				return fmt.Errorf("failed to verify IPv6 default route via %s on %s/%s: %w",
					ipv6Gateway,
					nodeName,
					bridgeName,
					err,
				)
			}
		}
	}

	ictx.AddCleanUpFn(func() error {
		var errs []error
		for nodeName, iface := range nodeIfaces {
			ipv4Gateway, err := interfaceGateway(
				iface.IPv4Gateway,
				iface.IPv4,
				iface.IPv4Prefix,
			)
			if err != nil {
				errs = append(errs, err)
			}
			if ipv4Gateway != "" {
				if err := runNodeCommand(
					nodeName,
					"ip route del default via %s dev %s metric 50000 2>/dev/null || true",
					ipv4Gateway,
					bridgeName,
				); err != nil {
					errs = append(errs, err)
				}
			}
			ipv6Gateway, err := interfaceGateway(
				iface.IPv6Gateway,
				iface.IPv6,
				iface.IPv6Prefix,
			)
			if err != nil {
				errs = append(errs, err)
			}
			if ipv6Gateway != "" {
				if err := runNodeCommand(
					nodeName,
					"ip -6 route del default via %s dev %s metric 50000 2>/dev/null || true",
					ipv6Gateway,
					bridgeName,
				); err != nil {
					errs = append(errs, err)
				}
			}
		}
		return errors.Join(errs...)
	})
	return nil
}

func interfaceGateway(gateway, ip, prefix string) (string, error) {
	if gateway != "" || ip == "" {
		return gateway, nil
	}
	if prefix == "" {
		return "", fmt.Errorf("interface has IP %s without a prefix", ip)
	}
	return firstUsableIP(fmt.Sprintf("%s/%s", ip, prefix))
}

func firstUsableIP(cidr string) (string, error) {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", err
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		ipNet.IP = ipNet.IP.To4()
	}
	gateway := append(net.IP(nil), ipNet.IP...)
	incrementUplinkIP(gateway)
	if !ipNet.Contains(gateway) || gateway.Equal(ip) {
		return "", fmt.Errorf("failed to derive gateway from %s", cidr)
	}
	return gateway.String(), nil
}

func incrementUplinkIP(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			return
		}
	}
}

func uplinkOVSPodsByNode(f *framework.Framework) (map[string]corev1.Pod, error) {
	pods, err := f.ClientSet.CoreV1().Pods(deploymentconfig.Get().OVNKubernetesNamespace()).List(
		context.Background(),
		metav1.ListOptions{LabelSelector: "app=ovnkube-node"},
	)
	if err != nil {
		return nil, err
	}
	byNode := map[string]corev1.Pod{}
	for _, pod := range pods.Items {
		byNode[pod.Spec.NodeName] = pod
	}
	return byNode, nil
}

func runOVSCommand(pod corev1.Pod, format string, args ...any) error {
	ginkgo.GinkgoHelper()
	cmd := fmt.Sprintf(format, args...)
	_, err := e2epodoutput.RunHostCmdWithRetries(
		pod.Namespace,
		pod.Name,
		cmd,
		uplinkPoll,
		uplinkShortTimeout,
	)
	return err
}

func runNodeCommand(nodeName, format string, args ...any) error {
	ginkgo.GinkgoHelper()
	cmd := fmt.Sprintf(format, args...)
	_, err := ForContainer(nodeName).Exec("sh", "-c", cmd)
	return err
}

func createUplink(
	f *framework.Framework,
	ictx infraapi.Context,
	name string,
	nodes []corev1.Node,
	nodeIfaces map[string]infraapi.NetworkInterface,
	hostInterfaceName string,
) {
	ginkgo.GinkgoHelper()

	nodeConfigs := make([]interface{}, 0, len(nodes))
	for _, node := range nodes {
		iface, ok := nodeIfaces[node.Name]
		gomega.Expect(ok).To(gomega.BeTrue(), "expected Uplink interface for node %s", node.Name)
		hostname, ok := node.Labels[corev1.LabelHostname]
		gomega.Expect(ok).To(gomega.BeTrue(), "expected node %s to have label %q", node.Name, corev1.LabelHostname)
		nodeHostInterfaceName := hostInterfaceName
		if nodeHostInterfaceName == "" {
			nodeHostInterfaceName = iface.InfName
		}
		nodeConfigs = append(nodeConfigs, map[string]interface{}{
			"type": "OVSBridge",
			"nodeSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					corev1.LabelHostname: hostname,
				},
			},
			"hostInterfaceName": nodeHostInterfaceName,
		})
	}

	_, err := f.DynamicClient.Resource(uplinkGVR).Create(
		context.Background(),
		&unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "k8s.ovn.org/v1alpha1",
				"kind":       "Uplink",
				"metadata": map[string]interface{}{
					"name": name,
				},
				"spec": map[string]interface{}{
					"nodeConfigs": nodeConfigs,
				},
			},
		},
		metav1.CreateOptions{},
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ictx.AddCleanUpFn(func() error {
		return f.DynamicClient.Resource(uplinkGVR).Delete(context.Background(), name, metav1.DeleteOptions{})
	})
}

func waitForUplinkStatesResolved(f *framework.Framework, uplinkName string, bridgeName string, nodes []corev1.Node) {
	ginkgo.GinkgoHelper()

	for _, node := range nodes {
		node := node
		gomega.Eventually(func() error {
			state, err := getUplinkState(f, uplinkName, node.Name)
			if err != nil {
				return err
			}
			conditions, err := getConditions(state)
			if err != nil {
				return err
			}
			var resolved *metav1.Condition
			for i := range conditions {
				if conditions[i].Type == uplinkv1alpha1.UplinkStateConditionResolved {
					resolved = &conditions[i]
					break
				}
			}
			if resolved == nil || resolved.Status != metav1.ConditionTrue {
				return fmt.Errorf("UplinkState %s is not resolved: %#v", state.GetName(), resolved)
			}
			resolvedBridge, _, err := unstructured.NestedString(state.Object, "status", "ovsBridge", "name")
			if err != nil {
				return err
			}
			if bridgeName != "" && resolvedBridge != bridgeName {
				return fmt.Errorf("UplinkState %s resolved bridge %q, expected %q",
					state.GetName(),
					resolvedBridge,
					bridgeName,
				)
			}
			if bridgeName == "" && resolvedBridge == "" {
				return fmt.Errorf("UplinkState %s has no resolved bridge", state.GetName())
			}
			ipAddresses, _, err := unstructured.NestedStringSlice(state.Object, "status", "ipAddresses")
			if err != nil {
				return err
			}
			if len(ipAddresses) == 0 {
				return fmt.Errorf("UplinkState %s has no IP addresses", state.GetName())
			}
			return nil
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
			gomega.Succeed(),
			"expected UplinkState for uplink %q on node %q to become resolved",
			uplinkName,
			node.Name,
		)
	}
}

func waitForUplinkStatesDefaultGateways(
	f *framework.Framework,
	uplinkName string,
	nodes []corev1.Node,
	ipFamilySet sets.Set[utilnet.IPFamily],
) {
	ginkgo.GinkgoHelper()

	for _, node := range nodes {
		node := node
		gomega.Eventually(func() error {
			state, err := getUplinkState(f, uplinkName, node.Name)
			if err != nil {
				return err
			}
			defaultGateways, _, err := unstructured.NestedStringSlice(
				state.Object,
				"status",
				"defaultGateways",
			)
			if err != nil {
				return err
			}
			families := sets.New[utilnet.IPFamily]()
			for _, defaultGateway := range defaultGateways {
				ip := net.ParseIP(defaultGateway)
				if ip == nil {
					return fmt.Errorf(
						"UplinkState %s has invalid default gateway %q",
						state.GetName(),
						defaultGateway,
					)
				}
				if utilnet.IsIPv6(ip) {
					families.Insert(utilnet.IPv6)
				} else {
					families.Insert(utilnet.IPv4)
				}
			}
			for family := range ipFamilySet {
				if !families.Has(family) {
					return fmt.Errorf(
						"UplinkState %s default gateways %v are missing %s",
						state.GetName(),
						defaultGateways,
						family,
					)
				}
			}
			return nil
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
			gomega.Succeed(),
			"expected UplinkState for uplink %q on node %q to publish default gateways",
			uplinkName,
			node.Name,
		)
	}
}

func getUplinkState(f *framework.Framework, uplinkName string, nodeName string) (*unstructured.Unstructured, error) {
	fieldSelector := fields.AndSelectors(
		fields.OneTermEqualSelector("spec.uplinkName", uplinkName),
		fields.OneTermEqualSelector("spec.nodeName", nodeName),
	).String()
	stateList, err := f.DynamicClient.Resource(uplinkStateGVR).List(
		context.Background(),
		metav1.ListOptions{FieldSelector: fieldSelector},
	)
	if err != nil {
		return nil, err
	}
	if len(stateList.Items) == 0 {
		return nil, fmt.Errorf("failed to find UplinkState for uplink %q on node %q", uplinkName, nodeName)
	}
	return &stateList.Items[0], nil
}

func createNodeScopedUplinkFRRConfiguration(
	f *framework.Framework,
	ictx infraapi.Context,
	configurationName string,
	networkName string,
	node corev1.Node,
	neighborIPs []string,
) error {
	hostname := node.Labels[corev1.LabelHostname]
	if hostname == "" {
		return fmt.Errorf("node %s has no %s label", node.Name, corev1.LabelHostname)
	}
	if len(neighborIPs) == 0 {
		return fmt.Errorf("no BGP neighbor addresses found for node %s", node.Name)
	}
	neighbors := make([]interface{}, 0, len(neighborIPs))
	for _, neighborIP := range neighborIPs {
		neighbors = append(neighbors, map[string]interface{}{
			"address": neighborIP,
			"asn":     int64(64512),
		})
	}
	client := f.DynamicClient.Resource(uplinkFRRConfigurationGVR).Namespace(
		deploymentconfig.Get().FRRK8sNamespace(),
	)
	_, err := client.Create(context.Background(), &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "frrk8s.metallb.io/v1beta1",
		"kind":       "FRRConfiguration",
		"metadata": map[string]interface{}{
			"name":   configurationName,
			"labels": map[string]interface{}{"network": networkName},
		},
		"spec": map[string]interface{}{
			"nodeSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{corev1.LabelHostname: hostname},
			},
			"bgp": map[string]interface{}{
				"routers": []interface{}{map[string]interface{}{
					"asn":       int64(64512),
					"vrf":       networkName,
					"neighbors": neighbors,
				}},
			},
		},
	}}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create node-scoped FRRConfiguration %s: %w", configurationName, err)
	}
	ictx.AddCleanUpFn(func() error {
		return client.Delete(context.Background(), configurationName, metav1.DeleteOptions{})
	})
	return nil
}

func waitForUplinkStateGatewayCondition(
	f *framework.Framework,
	uplinkName string,
	nodeName string,
	expectedStatus metav1.ConditionStatus,
	expectedReason string,
) {
	ginkgo.GinkgoHelper()

	gomega.Eventually(func() error {
		state, err := getUplinkState(f, uplinkName, nodeName)
		if err != nil {
			return err
		}
		conditions, err := getConditions(state)
		if err != nil {
			return err
		}
		for _, condition := range conditions {
			if condition.Type != "GatewayReady" {
				continue
			}
			if condition.Status == expectedStatus && condition.Reason == expectedReason {
				return nil
			}
			return fmt.Errorf("UplinkState %s GatewayReady condition is %s/%s, expected %s/%s",
				state.GetName(), condition.Status, condition.Reason, expectedStatus, expectedReason)
		}
		return fmt.Errorf("UplinkState %s has no GatewayReady condition", state.GetName())
	}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(gomega.Succeed())
}

func waitForCUDNUplinksReady(f *framework.Framework, cudnName string) {
	ginkgo.GinkgoHelper()

	waitForCUDNUplinksCondition(
		f,
		cudnName,
		metav1.ConditionTrue,
		"UplinksReady",
	)
}

func waitForCUDNUplinksCondition(
	f *framework.Framework,
	cudnName string,
	expectedStatus metav1.ConditionStatus,
	expectedReason string,
) {
	ginkgo.GinkgoHelper()

	client := f.DynamicClient.Resource(clusterUDNGVR)
	gomega.Eventually(func() error {
		cudn, err := client.Get(context.Background(), cudnName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		conditions, err := getConditions(cudn)
		if err != nil {
			return err
		}
		for _, condition := range conditions {
			if condition.Type != "UplinksReady" {
				continue
			}
			if condition.Status == expectedStatus && condition.Reason == expectedReason {
				return nil
			}
			return fmt.Errorf("CUDN %s UplinksReady condition is %s/%s, expected %s/%s",
				cudnName, condition.Status, condition.Reason, expectedStatus, expectedReason)
		}
		return fmt.Errorf("CUDN %s has no UplinksReady condition", cudnName)
	}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(gomega.Succeed())
}

func createUplinkAdvertisedCUDN(
	f *framework.Framework,
	ictx infraapi.Context,
	networkName string,
	networkSpec *udnv1.NetworkSpec,
	uplinkName string,
	targetVRF string,
) (*corev1.Namespace, error) {
	ginkgo.GinkgoHelper()

	networkLabels := map[string]string{"advertise": networkName}
	namespace, err := createUplinkNamespace(f, ictx, "uplink-bgp", networkName)
	if err != nil {
		return nil, err
	}
	if err := createUplinkCUDN(f, ictx, namespace, networkName, networkSpec, networkLabels, uplinkName); err != nil {
		return nil, err
	}
	if err := createRouteAdvertisements(
		f,
		ictx,
		networkName,
		targetVRF,
		networkLabels,
		map[string]string{"network": networkName},
	); err != nil {
		return nil, err
	}
	return namespace, nil
}

func createUplinkNamespace(
	f *framework.Framework,
	ictx infraapi.Context,
	testName string,
	networkName string,
) (*corev1.Namespace, error) {
	nsLabels := map[string]string{
		"e2e-framework":           testName,
		RequiredUDNNamespaceLabel: "",
	}
	namespace, err := f.CreateNamespace(context.Background(), networkName, nsLabels)
	if err != nil {
		return nil, fmt.Errorf("failed to create namespace: %w", err)
	}
	ictx.AddCleanUpFn(func() error {
		return f.ClientSet.CoreV1().Namespaces().Delete(context.Background(), namespace.Name, metav1.DeleteOptions{})
	})
	return namespace, nil
}

func createUplinkPlainNamespace(
	f *framework.Framework,
	ictx infraapi.Context,
	testName string,
	networkName string,
) (*corev1.Namespace, error) {
	nsLabels := map[string]string{
		"e2e-framework": testName,
	}
	namespace, err := f.CreateNamespace(context.Background(), networkName, nsLabels)
	if err != nil {
		return nil, fmt.Errorf("failed to create namespace: %w", err)
	}
	ictx.AddCleanUpFn(func() error {
		return f.ClientSet.CoreV1().Namespaces().Delete(context.Background(), namespace.Name, metav1.DeleteOptions{})
	})
	return namespace, nil
}

func createUplinkCUDN(
	f *framework.Framework,
	ictx infraapi.Context,
	namespace *corev1.Namespace,
	name string,
	networkSpec *udnv1.NetworkSpec,
	networkLabels map[string]string,
	uplinkName string,
) error {
	networkSpecMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(networkSpec)
	if err != nil {
		return fmt.Errorf("failed to convert network spec to unstructured: %w", err)
	}
	metadata := map[string]interface{}{"name": name}
	if len(networkLabels) > 0 {
		metadata["labels"] = stringMapToInterfaceMap(networkLabels)
	}
	if isDPUUplinkE2E() {
		metadata["annotations"] = map[string]interface{}{
			uplinkDPUResourceNameAnnotation: dpuUplinkResourceName(),
		}
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.ovn.org/v1",
		"kind":       "ClusterUserDefinedNetwork",
		"metadata":   metadata,
		"spec": map[string]interface{}{
			"namespaceSelector": map[string]interface{}{
				"matchExpressions": []interface{}{map[string]interface{}{
					"key":      "kubernetes.io/metadata.name",
					"operator": "In",
					"values":   []interface{}{namespace.Name},
				}},
			},
			"network": networkSpecMap,
			"uplinks": []interface{}{uplinkName},
		},
	}}
	client := f.DynamicClient.Resource(clusterUDNGVR)
	if _, err := client.Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create CUDN %s: %w", name, err)
	}
	ictx.AddCleanUpFn(func() error {
		return client.Delete(context.Background(), name, metav1.DeleteOptions{})
	})
	gomega.Eventually(networkReadyFunc(client, name)).
		WithTimeout(uplinkTimeout).
		WithPolling(uplinkPoll).
		Should(gomega.Succeed(), "expected CUDN %s to become ready", name)
	return nil
}

func stringMapToInterfaceMap(in map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func uplinkLayer3NetworkSpec(
	ipFamilySet sets.Set[utilnet.IPFamily],
	ipv4Subnet string,
	ipv6Subnet string,
) *udnv1.NetworkSpec {
	return &udnv1.NetworkSpec{
		Topology: udnv1.NetworkTopologyLayer3,
		Layer3: &udnv1.Layer3Config{
			Role: udnv1.NetworkRolePrimary,
			Subnets: matchL3SubnetsByIPFamilies(
				ipFamilySet,
				udnv1.Layer3Subnet{CIDR: udnv1.CIDR(ipv4Subnet)},
				udnv1.Layer3Subnet{CIDR: udnv1.CIDR(ipv6Subnet)},
			),
		},
	}
}

func createUplinkNetexecPod(f *framework.Framework, namespace string, name string, nodeName string) *corev1.Pod {
	ginkgo.GinkgoHelper()

	pod := e2epod.NewAgnhostPod(namespace, name, nil, nil, nil)
	pod.Spec.Containers[0].Args = []string{"netexec"}
	if nodeName != "" {
		pod.Spec.NodeName = nodeName
	}
	addDPUUplinkResourceRequest(pod)
	return e2epod.PodClientNS(f, namespace).CreateSync(context.Background(), pod)
}

func createUplinkServicePod(f *framework.Framework, namespace string, name string, nodeName string) *corev1.Pod {
	ginkgo.GinkgoHelper()

	labels := map[string]string{"app": name}
	pod := e2epod.NewAgnhostPod(
		namespace,
		name,
		nil,
		nil,
		[]corev1.ContainerPort{{ContainerPort: netexecPort}},
		"netexec",
	)
	pod.Labels = labels
	pod.Spec.NodeName = nodeName
	addDPUUplinkResourceRequest(pod)
	return e2epod.PodClientNS(f, namespace).CreateSync(context.Background(), pod)
}

func addDPUUplinkResourceRequest(pod *corev1.Pod) {
	ginkgo.GinkgoHelper()

	if !isDPUUplinkE2E() {
		return
	}

	resourceName := corev1.ResourceName(dpuUplinkResourceName())
	quantity := resource.MustParse("2")
	container := &pod.Spec.Containers[0]
	if container.Resources.Requests == nil {
		container.Resources.Requests = corev1.ResourceList{}
	}
	if container.Resources.Limits == nil {
		container.Resources.Limits = corev1.ResourceList{}
	}
	container.Resources.Requests[resourceName] = quantity
	container.Resources.Limits[resourceName] = quantity
}

func createUplinkNodePortService(f *framework.Framework, namespace string, selector map[string]string) *corev1.Service {
	ginkgo.GinkgoHelper()

	service := e2eservice.CreateServiceSpec("server", "", false, selector)
	service.Spec.Type = corev1.ServiceTypeNodePort
	service.Spec.Ports[0].Port = netexecPort
	service.Spec.Ports[0].TargetPort = intstr.FromInt32(netexecPort)
	service, err := f.ClientSet.CoreV1().Services(namespace).Create(
		context.Background(),
		service,
		metav1.CreateOptions{},
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return service
}

func uplinkPodToHostnameAndExpect(src *corev1.Pod, dstIP, expect string) {
	ginkgo.GinkgoHelper()

	hostname, err := e2epodoutput.RunHostCmdWithRetries(
		src.Namespace,
		src.Name,
		fmt.Sprintf("curl --max-time %d -g -q -s http://%s/hostname", uplinkCurlMaxTime, net.JoinHostPort(dstIP, fmt.Sprintf("%d", netexecPort))),
		uplinkPoll,
		uplinkTimeout,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(hostname).To(gomega.Equal(expect))
}

func uplinkPodToHostnameAndExpectReachable(src *corev1.Pod, dstIP string) {
	ginkgo.GinkgoHelper()

	hostname, err := e2epodoutput.RunHostCmdWithRetries(
		src.Namespace,
		src.Name,
		fmt.Sprintf("curl --max-time %d -g -q -s http://%s/hostname", uplinkCurlMaxTime, net.JoinHostPort(dstIP, fmt.Sprintf("%d", netexecPort))),
		uplinkPoll,
		uplinkTimeout,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(strings.TrimSpace(hostname)).NotTo(gomega.BeEmpty())
}

func uplinkPodToClientIPAndExpect(src *corev1.Pod, dstIP, expect string) {
	ginkgo.GinkgoHelper()

	ip, err := e2epodoutput.RunHostCmdWithRetries(
		src.Namespace,
		src.Name,
		fmt.Sprintf("curl --max-time %d -g -q -s http://%s/clientip", uplinkCurlMaxTime, net.JoinHostPort(dstIP, fmt.Sprintf("%d", netexecPort))),
		uplinkPoll,
		uplinkTimeout,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ip, _, err = net.SplitHostPort(ip)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(ip).To(gomega.Equal(expect))
}

func uplinkExternalToNodePortAndExpect(
	container infraapi.ExternalContainer,
	dstIP string,
	nodePort int32,
	expect string,
) {
	ginkgo.GinkgoHelper()

	target := fmt.Sprintf("http://%s/hostname", net.JoinHostPort(dstIP, fmt.Sprint(nodePort)))
	gomega.Eventually(func() (string, error) {
		return infraprovider.Get().ExecExternalContainerCommand(container, []string{
			"curl",
			"--max-time",
			fmt.Sprint(uplinkCurlMaxTime),
			"-g",
			"-q",
			"-s",
			target,
		})
	}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(gomega.Equal(expect))
}

func uplinkExternalToPodAndExpect(container infraapi.ExternalContainer, dstIP string, expect string) {
	ginkgo.GinkgoHelper()

	target := fmt.Sprintf("http://%s/hostname", net.JoinHostPort(dstIP, fmt.Sprint(netexecPort)))
	gomega.Eventually(func() (string, error) {
		return infraprovider.Get().ExecExternalContainerCommand(container, []string{
			"curl",
			"--max-time",
			fmt.Sprint(uplinkCurlMaxTime),
			"-g",
			"-q",
			"-s",
			target,
		})
	}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(gomega.Equal(expect))
}

func getFirstCIDRStringOfFamily(family utilnet.IPFamily, cidrs []string) string {
	for _, cidr := range cidrs {
		if utilnet.IPFamilyOfCIDRString(cidr) == family {
			return cidr
		}
	}
	return ""
}

func getNodeInterfaceVRF(nodeName, interfaceName string) (string, error) {
	out, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", "-j", "link", "show", "dev", interfaceName})
	if err != nil {
		return "", fmt.Errorf("failed to get interface %s on node %s: %w", interfaceName, nodeName, err)
	}
	return interfaceMaster(out, interfaceName)
}

func getUplinkBridgeVRF(hostNodeName, bridgeName string) (string, error) {
	if !isDPUUplinkE2E() {
		return getNodeInterfaceVRF(hostNodeName, bridgeName)
	}

	dpuNodeName, err := dpuNodeNameForHostNode(hostNodeName)
	if err != nil {
		return "", err
	}
	out, err := ForContainer(dpuNodeName).Exec("ip", "-j", "link", "show", "dev", bridgeName)
	if err != nil {
		return "", fmt.Errorf("failed to get bridge %s on DPU node %s: %w", bridgeName, dpuNodeName, err)
	}
	return interfaceMaster(out, bridgeName)
}

func interfaceMaster(output, interfaceName string) (string, error) {
	links := []struct {
		Master string `json:"master"`
	}{}
	if err := json.Unmarshal([]byte(output), &links); err != nil {
		return "", fmt.Errorf("failed to parse interface %s: %w; output: %s", interfaceName, err, output)
	}
	if len(links) != 1 {
		return "", fmt.Errorf("expected one link for interface %s, got %d; output: %s", interfaceName, len(links), output)
	}
	return links[0].Master, nil
}

func hasRouteInDefaultVRF(node corev1.Node, cidr, nextHop string) (bool, error) {
	routeCommand := []string{"ip", "--json", "route", "show", cidr}
	if utilnet.IsIPv6CIDRString(cidr) {
		routeCommand = []string{"ip", "-6", "--json", "route", "show", cidr}
	}

	out, err := infraprovider.Get().ExecK8NodeCommand(node.GetName(), routeCommand)
	if err != nil {
		return false, fmt.Errorf("failed to get routes on node %s: %w", node.Name, err)
	}
	routes := []kernelRoute{}
	if err := json.Unmarshal([]byte(out), &routes); err != nil {
		return false, fmt.Errorf("failed to parse routes on node %s: %w; output: %s", node.Name, err, out)
	}
	framework.Logf("Routes in default VRF on node %s for %s: %s", node.Name, cidr, out)
	return hasBGPRoute(routes, cidr, nextHop), nil
}

func hasRouteInDPUCUDNVRF(hostNode corev1.Node, cudnName, cidr, nextHop string) (bool, error) {
	if len(cudnName) > 15 {
		return false, fmt.Errorf("CUDN name %q is too long to be used as the Linux VRF device name", cudnName)
	}

	dpuNodeName, err := dpuNodeNameForHostNode(hostNode.Name)
	if err != nil {
		return false, err
	}

	routeCommand := []string{"ip", "--json", "route", "show", "vrf", cudnName, cidr}
	if utilnet.IsIPv6CIDRString(cidr) {
		routeCommand = []string{"ip", "-6", "--json", "route", "show", "vrf", cudnName, cidr}
	}

	out, err := ForContainer(dpuNodeName).Exec(routeCommand[0], routeCommand[1:]...)
	if err != nil {
		return false, fmt.Errorf(
			"failed to get routes from CUDN VRF %s on DPU node %s for host node %s: %w, output: %s",
			cudnName,
			dpuNodeName,
			hostNode.Name,
			err,
			out,
		)
	}
	routes := []kernelRoute{}
	if err := json.Unmarshal([]byte(out), &routes); err != nil {
		return false, fmt.Errorf(
			"failed to parse routes from CUDN VRF %s on DPU node %s for host node %s: %w; output: %s",
			cudnName,
			dpuNodeName,
			hostNode.Name,
			err,
			out,
		)
	}
	framework.Logf(
		"Routes in CUDN VRF %s on DPU node %s for host node %s and %s: %s",
		cudnName,
		dpuNodeName,
		hostNode.Name,
		cidr,
		out,
	)
	return hasBGPRoute(routes, cidr, nextHop), nil
}

func dpuNodeNameForHostNode(hostNodeName string) (string, error) {
	if !strings.Contains(hostNodeName, "-host-") {
		return "", fmt.Errorf("failed to derive DPU node name from host node %q", hostNodeName)
	}
	return strings.Replace(hostNodeName, "-host-", "-dpu-", 1), nil
}

func isDPUUplinkE2E() bool {
	return os.Getenv(uplinkDPUGatewayNetworkEnv) != ""
}

func dpuUplinkResourceName() string {
	return envOrDefault(uplinkDPUResourceNameEnv, uplinkDefaultDPUResourceName)
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func uplinkBridgeName(name string) string {
	name = strings.ReplaceAll(name, "-", "")
	if len(name) > 13 {
		name = name[len(name)-13:]
	}
	return "u" + name
}
