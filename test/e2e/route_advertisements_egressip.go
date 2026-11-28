// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	rav1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	utilnet "k8s.io/utils/net"
)

const (
	// egressBGPExportLabelKey/Value label nodes that match RouteAdvertisements.spec.nodeSelector for EgressIP.
	egressBGPExportLabelKey   = "egress-nodes"
	egressBGPExportLabelValue = "bgp"

	bgpEgressIPClientPodLabelKey   = "bgp-eip-client"
	bgpEgressIPClientPodLabelValue = "enabled"

	bgpEgressIPLocalPodName  = "bgp-eip-client-local"
	bgpEgressIPRemotePodName = "bgp-eip-client-remote"

	bgpEgressIPPollTimeout         = 3 * retryTimeout
	bgpEgressIPResourceDeleteTimeout = retryTimeout
	bgpEgressIPRouteAbsentDuration = 10 * time.Second
	bgpEgressIPGRPCHealthCheckPort = "9107"

	// Matches FRRConfiguration created by contrib/kind-common.sh and the default-network RA from helm when advertiseDefaultNetwork is enabled.
	bgpE2EFRRConfigurationLabelKey   = "name"
	bgpE2EFRRConfigurationLabelValue = "receive-all"

	// bgpEgressIPMinWorkerNodes* is the minimum number of schedulable worker nodes required.
	bgpEgressIPMinWorkerNodesSingleStack = 2
	bgpEgressIPMinWorkerNodesDualStack   = 3

	egressAssignableLabel = "k8s.ovn.org/egress-assignable"

	// bgpEgressIPHostSubnet* bounds the host-route offset range used when allocating test EgressIP addresses.
	bgpEgressIPHostSubnetIPOffset = 200
	bgpEgressIPHostSubnetIPEnd    = 254
)

// FRR-K8s selector for receive-all applied by BGP Kind/Helm (contrib/kind-common.sh, helm default RouteAdvertisements).
var bgpE2EReceiveAllFRRConfigurationSelector = metav1.LabelSelector{
	MatchLabels: map[string]string{
		bgpE2EFRRConfigurationLabelKey: bgpE2EFRRConfigurationLabelValue,
	},
}

// bgpEgressIPNodeSet names schedulable workers used for egressIP assignment and failover (three for dual-stack) or two for single-stack):
//
//	egressNode1/egressNode2 host initial egressIP placement; nonInitialEgressNode is the intended node for egressIP failover to.
//	egressNodes is one or two names (single- or dual-stack); allScenarioNodeNames is nonInitialEgressNode plus egressNodes.
type bgpEgressIPNodeSet struct {
	nonInitialEgressNode corev1.Node
	egressNode1          corev1.Node
	egressNode2          corev1.Node // unset (zero value) on single-stack clusters
	dualStackCluster     bool
	egressNodes          []string
	allScenarioNodeNames []string
}

type bgpEgressIPScenario struct {
	useRouteAdvertisementsEgressIPNodeSelector bool
	failoverFromEgressIPAdvertisedNode         bool
}

var _ = ginkgo.Describe("BGP: EgressIP advertisement", feature.RouteAdvertisements, feature.EgressIP, ginkgo.Ordered, ginkgo.Serial, func() {
	f := wrappedTestFramework("bgp-egressip-advertisement")

	var (
		nodeSet            bgpEgressIPNodeSet
		serverContainerIPs []string
		raClient           raclientset.Interface
	)

	ginkgo.BeforeEach(func(ctx context.Context) {
		if os.Getenv("ENABLE_ROUTE_ADVERTISEMENTS") != "true" {
			e2eskipper.Skipf("route advertisements are not enabled")
		}
		var err error
		raClient, err = raclientset.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err, "failed to create RouteAdvertisements client")

		serverContainerIPs = getBGPServerContainerIPs(f)
		gomega.Expect(len(serverContainerIPs)).To(gomega.BeNumerically(">", 0), "no BGP server container IPs found")

		nodeSet, err = pickBGPEgressIPNodeSet(ctx, f.ClientSet)
		framework.ExpectNoError(err, "failed to pick BGP egress IP node set")
		if nodeSet.dualStackCluster {
			ginkgo.By("For dualstack, selecting 3 schedulable workers for BGP egress IP test (nonInitialEgressNode + two egress nodes)")
		} else {
			ginkgo.By("For singlestack, selecting 2 schedulable workers for BGP egress IP test (nonInitialEgressNode + one gress node)")
		}
		ensureBGPEgressIPHealthCheckReachable(nodeSet.allScenarioNodeNames...)
		waitNodesHostCIDRsForEgressIP(f.ClientSet, nodeSet.allScenarioNodeNames...)
	})

	ginkgo.It("on default network without RouteAdvertisements nodeSelector for egressIP", func() {
		runBGPEgressIPScenario(f, raClient, nodeSet, serverContainerIPs, nil, bgpEgressIPScenario{
			useRouteAdvertisementsEgressIPNodeSelector: false,
		})
	})

	ginkgo.It("on default network with RouteAdvertisements nodeSelector for egressIP", func() {
		runBGPEgressIPScenario(f, raClient, nodeSet, serverContainerIPs, nil, bgpEgressIPScenario{
			useRouteAdvertisementsEgressIPNodeSelector: true,
			failoverFromEgressIPAdvertisedNode:         true,
		})
	})

	ginkgo.It("on L3 primary CUDN without RouteAdvertisements nodeSelector for egressIP", feature.NetworkSegmentation, func() {
		runBGPEgressIPScenario(f, raClient, nodeSet, serverContainerIPs, createBGPEgressIPL3CUDN, bgpEgressIPScenario{
			useRouteAdvertisementsEgressIPNodeSelector: false,
		})
	})

	ginkgo.It("on L3 primary CUDN with RouteAdvertisements nodeSelector for egressIP", feature.NetworkSegmentation, func() {
		runBGPEgressIPScenario(f, raClient, nodeSet, serverContainerIPs, createBGPEgressIPL3CUDN, bgpEgressIPScenario{
			useRouteAdvertisementsEgressIPNodeSelector: true,
			failoverFromEgressIPAdvertisedNode:         false,
		})
	})

	ginkgo.It("rejects EgressIP RouteAdvertisements on Layer2 primary CUDN", feature.NetworkSegmentation, func() {
		suffix := framework.RandomSuffix()
		cudnLabel := "bgp-eip-l2-" + suffix
		raName := "bgp-eip-l2-ra-" + suffix

		udnNamespace, err := f.CreateNamespace(context.TODO(), f.BaseName+"-l2-"+suffix, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		framework.ExpectNoError(err, "failed to create namespace for L2 CUDN EgressIP test")
		ginkgo.DeferCleanup(func() {
			_ = f.ClientSet.CoreV1().Namespaces().Delete(context.TODO(), udnNamespace.Name, metav1.DeleteOptions{})
		})

		udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err, "failed to create UDN client")

		cudn := &udnv1.ClusterUserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "bgp-eip-l2-",
				Labels:       map[string]string{cudnLabel: ""},
			},
			Spec: udnv1.ClusterUserDefinedNetworkSpec{
				NamespaceSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "kubernetes.io/metadata.name",
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{udnNamespace.Name},
				}}},
				Network: udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer2,
					Layer2: &udnv1.Layer2Config{
						Role:    "Primary",
						Subnets: filterDualStackCIDRs(f.ClientSet, udnv1.DualStackCIDRs{"103.0.0.0/16", "2014:100::0/60"}),
					},
				},
			},
		}
		createdCUDN, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.Background(), cudn, metav1.CreateOptions{})
		framework.ExpectNoError(err, "failed to create L2 ClusterUserDefinedNetwork")
		ginkgo.DeferCleanup(deleteBGPEgressIPResourceForCleanup, "ClusterUserDefinedNetwork", createdCUDN.Name, func() error {
			return udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.Background(), createdCUDN.Name, metav1.DeleteOptions{})
		})
		ginkgo.DeferCleanup(func() error {
			ctx, cancel := context.WithTimeout(context.Background(), bgpEgressIPResourceDeleteTimeout)
			defer cancel()
			return f.ClientSet.CoreV1().Pods(udnNamespace.Name).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{})
		})
		gomega.Eventually(clusterUserDefinedNetworkReadyFunc(context.Background(), f.DynamicClient, createdCUDN.Name), 30*time.Second, time.Second).Should(gomega.Succeed(),
			"L2 ClusterUserDefinedNetwork %s did not become ready", createdCUDN.Name)

		ra := &rav1.RouteAdvertisements{
			ObjectMeta: metav1.ObjectMeta{Name: raName},
			Spec: rav1.RouteAdvertisementsSpec{
				NetworkSelectors: apitypes.NetworkSelectors{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{cudnLabel: ""},
							},
						},
					},
				},
				NodeSelector:             metav1.LabelSelector{},
				FRRConfigurationSelector: bgpE2EReceiveAllFRRConfigurationSelector,
				Advertisements:           []rav1.AdvertisementType{rav1.EgressIP},
			},
		}
		_, err = raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
		framework.ExpectNoError(err, "failed to create RouteAdvertisements %s", raName)
		ginkgo.DeferCleanup(deleteBGPEgressIPResourceForCleanup, "RouteAdvertisements", raName, func() error {
			return raClient.K8sV1().RouteAdvertisements().Delete(context.Background(), raName, metav1.DeleteOptions{})
		})

		waitRouteAdvertisementCondition(raClient, raName, "", "ConfigurationError", "Layer2")
	})
})

// pickBGPEgressIPNodeSet picks the first N workers for the BGP egress IP test (two for single-stack or three for dual-stack)
func pickBGPEgressIPNodeSet(ctx context.Context, cs clientset.Interface) (bgpEgressIPNodeSet, error) {
	nodeList, err := e2enode.GetReadySchedulableNodes(ctx, cs)
	if err != nil {
		return bgpEgressIPNodeSet{}, fmt.Errorf("listing ready schedulable nodes: %w", err)
	}
	schedulable := len(nodeList.Items)
	e2enode.Filter(nodeList, func(node corev1.Node) bool {
		return !isControlPlaneNode(node)
	})
	workers := nodeList.Items
	dualStackCluster := isIPv4Supported(cs) && isIPv6Supported(cs)
	minWorkers := bgpEgressIPMinWorkerNodesSingleStack
	if dualStackCluster {
		minWorkers = bgpEgressIPMinWorkerNodesDualStack
	}
	if len(workers) < minWorkers {
		e2eskipper.Skipf("need at least %d worker nodes for BGP egress IP tests, got %d (schedulable nodes: %d)",
			minWorkers, len(workers), schedulable)
	}
	sort.Slice(workers, func(i, j int) bool { return workers[i].Name < workers[j].Name })
	set := bgpEgressIPNodeSet{
		nonInitialEgressNode: workers[0],
		egressNode1:          workers[1],
		dualStackCluster:     dualStackCluster,
		egressNodes:          []string{workers[1].Name},
	}
	if dualStackCluster {
		set.egressNode2 = workers[2]
		set.egressNodes = append(set.egressNodes, workers[2].Name)
	}
	set.allScenarioNodeNames = append([]string{set.nonInitialEgressNode.Name}, set.egressNodes...)
	return set, nil
}

// ensureBGPEgressIPHealthCheckReachable opens the EgressIP gRPC health-check port on each node
func ensureBGPEgressIPHealthCheckReachable(nodeNames ...string) {
	for _, nodeName := range nodeNames {
		allowOrDropNodeInputTrafficOnPort("Allow", nodeName, "tcp", bgpEgressIPGRPCHealthCheckPort)
	}
}

// waitNodesHostCIDRsForEgressIP waits until each node has a non-empty k8s.ovn.org/host-cidrs annotation.
func waitNodesHostCIDRsForEgressIP(cs clientset.Interface, nodeNames ...string) {
	for _, nodeName := range nodeNames {
		gomega.Eventually(func() error {
			node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("getting node %s: %w", nodeName, err)
			}
			hostCIDRs, err := util.ParseNodeHostCIDRsDropNetMask(node)
			if err != nil {
				return fmt.Errorf("node %s host-cidrs not parseable: %w", nodeName, err)
			}
			if hostCIDRs.Len() == 0 {
				return fmt.Errorf("node %s has empty host-cidrs", nodeName)
			}
			return nil
		}, 2*time.Minute, 2*time.Second).Should(gomega.Succeed(),
			"timed out waiting for host-CIDRs annotation on node %s", nodeName)
	}
}

// runBGPEgressIPScenario creates RouteAdvertisement(s) and an EgressIP, checks FRR export and pod SNAT,
// fails over the assigned address, then re-checks routes and SNAT.
func runBGPEgressIPScenario(
	f *framework.Framework,
	raClient raclientset.Interface,
	nodeSet bgpEgressIPNodeSet,
	serverContainerIPs []string,
	setupNetwork func(*framework.Framework) (cudnMatchLabel, hostSubnetNetworkName string, cleanup func()),
	scenario bgpEgressIPScenario,
) {
	suffix := framework.RandomSuffix()
	raName := "bgp-eip-ra-" + suffix
	eipName := "bgp-eip-" + suffix
	if nodeSet.dualStackCluster {
		framework.Logf("BGP egress IP node roles: nonInitialEgressNode=%s egressNode1=%s egressNode2=%s",
			nodeSet.nonInitialEgressNode.Name, nodeSet.egressNode1.Name, nodeSet.egressNode2.Name)
	} else {
		framework.Logf("BGP egress IP node roles: nonInitialEgressNode=%s egressNode1=%s (single-stack)",
			nodeSet.nonInitialEgressNode.Name, nodeSet.egressNode1.Name)
	}

	var cudnMatchLabel, hostSubnetNetworkName string
	if setupNetwork != nil {
		var cleanup func()
		cudnMatchLabel, hostSubnetNetworkName, cleanup = setupNetwork(f)
		ginkgo.DeferCleanup(cleanup)
		ginkgo.DeferCleanup(func() {
			_ = f.ClientSet.CoreV1().Pods(f.Namespace.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})
		})
	} else {
		updateNamespaceLabels(f, f.Namespace, map[string]string{"name": f.Namespace.Name})
	}

	if scenario.useRouteAdvertisementsEgressIPNodeSelector && scenario.failoverFromEgressIPAdvertisedNode {
		e2enode.AddOrUpdateLabelOnNode(f.ClientSet, nodeSet.egressNode1.Name, egressBGPExportLabelKey, egressBGPExportLabelValue)
		if nodeSet.dualStackCluster {
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, nodeSet.egressNode2.Name, egressBGPExportLabelKey, egressBGPExportLabelValue)
		}
		ginkgo.DeferCleanup(func() {
			e2enode.RemoveLabelOffNode(f.ClientSet, nodeSet.egressNode1.Name, egressBGPExportLabelKey)
			if nodeSet.dualStackCluster {
				e2enode.RemoveLabelOffNode(f.ClientSet, nodeSet.egressNode2.Name, egressBGPExportLabelKey)
			}
		})
	}

	nodeSelector := metav1.LabelSelector{}
	if scenario.useRouteAdvertisementsEgressIPNodeSelector {
		nodeSelector = metav1.LabelSelector{
			MatchLabels: map[string]string{egressBGPExportLabelKey: egressBGPExportLabelValue},
		}
	}

	onPrimaryCUDN := cudnMatchLabel != ""
	if onPrimaryCUDN {
		podNetRAName := "bgp-eip-udn-podnet-ra-" + suffix
		createBGPL3CUDNPodNetworkRouteAdvertisement(raClient, podNetRAName, cudnMatchLabel)
		ginkgo.DeferCleanup(deleteBGPEgressIPResourceForCleanup, "RouteAdvertisements", podNetRAName, func() error {
			return raClient.K8sV1().RouteAdvertisements().Delete(context.Background(), podNetRAName, metav1.DeleteOptions{})
		})
		waitRouteAdvertisementCondition(raClient, podNetRAName, metav1.ConditionTrue, "", "")
	}

	createBGPEgressIPRouteAdvertisement(raClient, raName, cudnMatchLabel, nodeSelector)
	ginkgo.DeferCleanup(deleteBGPEgressIPResourceForCleanup, "RouteAdvertisements", raName, func() error {
		return raClient.K8sV1().RouteAdvertisements().Delete(context.Background(), raName, metav1.DeleteOptions{})
	})
	if scenario.useRouteAdvertisementsEgressIPNodeSelector && !scenario.failoverFromEgressIPAdvertisedNode {
		waitRouteAdvertisementCondition(raClient, raName, metav1.ConditionFalse, "ConfigurationPending", "no nodes selected")
	} else {
		waitRouteAdvertisementCondition(raClient, raName, metav1.ConditionTrue, "", "")
	}

	if onPrimaryCUDN {
		waitBGPEgressCUDNHostSubnetsOnNodes(f.ClientSet, hostSubnetNetworkName, nodeSet.egressNodes...)
	}
	ensureBGPEgressIPHealthCheckReachable(nodeSet.egressNodes...)
	waitNodesHostCIDRsForEgressIP(f.ClientSet, nodeSet.egressNodes...)

	var egressIPs []string
	var dualPlan bgpDualStackEgressIPPlan
	egressIPs, dualPlan = allocateBGPEgressTestIPsFromMachineNetwork(f.ClientSet, nodeSet, scenario)
	framework.Logf("BGP egress IP test using %d egress address(es): %v", len(egressIPs), egressIPs)

	dualStackEIP := len(egressIPs) > 1
	gomega.Expect(dualStackEIP).To(gomega.Equal(nodeSet.dualStackCluster), "cluster dual-stack capability must match allocated EgressIP spec")
	assignableEgressNodes := nodeSet.egressNodes
	framework.Logf("BGP egress IP assignable egress nodes: %v", assignableEgressNodes)

	prepareBGPEgressNodesForEgressIP(f.ClientSet, nodeSet)

	podLabels := map[string]string{bgpEgressIPClientPodLabelKey: bgpEgressIPClientPodLabelValue}
	nsLabels := map[string]string{"name": f.Namespace.Name}
	eipManifest := createEIPManifest(eipName, podLabels, nsLabels, egressIPs...)
	eipManifestPath := filepath.Join(os.TempDir(), "bgp-eip-"+framework.RandomSuffix()+".yaml")
	framework.ExpectNoError(os.WriteFile(eipManifestPath, []byte(eipManifest), 0o644), "failed to write EgressIP manifest %s", eipManifestPath)
	defer func() { _ = os.Remove(eipManifestPath) }()
	e2ekubectl.RunKubectlOrDie("default", "create", "-f", eipManifestPath)
	ginkgo.DeferCleanup(deleteBGPEgressIPResourceForCleanup, "EgressIP", eipName, func() error {
		_, err := e2ekubectl.RunKubectl("default", "delete", "eip", eipName, "--ignore-not-found")
		return err
	})
	assignments := waitForEgressIPAssignments(eipName, egressIPs)

	for egressIP, nodeName := range assignments {
		node := getClusterNode(f.ClientSet, nodeName)
		expectExported := !scenario.useRouteAdvertisementsEgressIPNodeSelector || node.Labels[egressBGPExportLabelKey] == egressBGPExportLabelValue
		expectEgressIPRouteInFRR(egressIP, expectExported, node, false)
	}

	localPod, err := createGenericPodWithLabel(f, bgpEgressIPLocalPodName, nodeSet.egressNode1.Name, f.Namespace.Name,
		getAgnHostHTTPPortBindFullCMD(netexecPort), podLabels)
	framework.ExpectNoError(err, "failed to create local client pod %s on node %s", bgpEgressIPLocalPodName, nodeSet.egressNode1.Name)
	remotePod, err := createGenericPodWithLabel(f, bgpEgressIPRemotePodName, nodeSet.nonInitialEgressNode.Name, f.Namespace.Name,
		getAgnHostHTTPPortBindFullCMD(netexecPort), podLabels)
	framework.ExpectNoError(err, "failed to create remote client pod %s on node %s", bgpEgressIPRemotePodName, nodeSet.nonInitialEgressNode.Name)
	err = e2epod.WaitTimeoutForPodReadyInNamespace(context.TODO(), f.ClientSet, localPod.Name, f.Namespace.Name, bgpEgressIPPollTimeout)
	framework.ExpectNoError(err, "local client pod %s/%s did not become ready", f.Namespace.Name, localPod.Name)
	err = e2epod.WaitTimeoutForPodReadyInNamespace(context.TODO(), f.ClientSet, remotePod.Name, f.Namespace.Name, bgpEgressIPPollTimeout)
	framework.ExpectNoError(err, "remote client pod %s/%s did not become ready", f.Namespace.Name, remotePod.Name)

	snatCheckPort := strconv.Itoa(netexecPort)
	serverIPByEgressIP := make(map[string]string, len(assignments))
	for egressIP := range assignments {
		serverIP := getFirstIPStringOfFamily(utilnet.IPFamilyOfString(egressIP), serverContainerIPs)
		if serverIP == "" {
			framework.Failf("no bgpserver IP for family of egress IP %s in %+v", egressIP, serverContainerIPs)
		}
		serverIPByEgressIP[egressIP] = serverIP
		gomega.Eventually(func() error {
			snatErr := curlAgnHostClientIPFromPod(f.Namespace.Name, localPod.Name, egressIP, serverIP, snatCheckPort)
			if snatErr != nil {
				return fmt.Errorf("SNAT check from local pod %s to server %s for egressIP %s: %w", localPod.Name, serverIP, egressIP, snatErr)
			}
			return nil
		}, bgpEgressIPPollTimeout, retryInterval).Should(gomega.Succeed(),
			"local pod %s did not SNAT to egressIP %s toward server %s", localPod.Name, egressIP, serverIP)
		gomega.Eventually(func() error {
			snatErr := curlAgnHostClientIPFromPod(f.Namespace.Name, remotePod.Name, egressIP, serverIP, snatCheckPort)
			if snatErr != nil {
				return fmt.Errorf("SNAT check from remote pod %s to server %s for egressIP %s: %w", remotePod.Name, serverIP, egressIP, snatErr)
			}
			return nil
		}, bgpEgressIPPollTimeout, retryInterval).Should(gomega.Succeed(),
			"remote pod %s did not SNAT to egressIP %s toward server %s", remotePod.Name, egressIP, serverIP)
	}

	var failoverIP, failoverFromEgressNode, failoverToEgressNode string
	failoverToEgressNode = nodeSet.nonInitialEgressNode.Name
	labelFailoverToNodeWithEgressBGPNodeselector := false
	if dualStackEIP {
		labelFailoverToNodeWithEgressBGPNodeselector = dualPlan.labelFailoverToNodeWithEgressBGPNodeselector
	} else {
		labelFailoverToNodeWithEgressBGPNodeselector = scenario.useRouteAdvertisementsEgressIPNodeSelector && !scenario.failoverFromEgressIPAdvertisedNode
	}
	prepareBGPEgressIPFailoverToNode(f, failoverToEgressNode, labelFailoverToNodeWithEgressBGPNodeselector)
	if scenario.useRouteAdvertisementsEgressIPNodeSelector && !scenario.failoverFromEgressIPAdvertisedNode {
		waitRouteAdvertisementCondition(raClient, raName, metav1.ConditionTrue, "", "")
	}
	failoverIP, failoverFromEgressNode = pickFailoverFromFirstEgressNode(f.ClientSet, assignments, nodeSet, failoverToEgressNode)
	otherAssignments := make(map[string]string, len(assignments))
	for k, v := range assignments {
		otherAssignments[k] = v
	}
	ginkgo.By(fmt.Sprintf("Failing over egress IP %s from node %s to node %s by removing egress-assignable", failoverIP, failoverFromEgressNode, failoverToEgressNode))
	egressAvailability := &egressNodeAvailabilityHandlerViaLabel{F: f}
	egressAvailability.Disable(failoverFromEgressNode)
	ginkgo.DeferCleanup(func() { egressAvailability.Enable(failoverFromEgressNode) })

	newAssignments := waitForEgressIPFailover(eipName, failoverIP, failoverFromEgressNode, failoverToEgressNode, otherAssignments, len(egressIPs))
	newNode := newAssignments[failoverIP]
	newNodeObj := getClusterNode(f.ClientSet, newNode)

	failoverExported := !scenario.useRouteAdvertisementsEgressIPNodeSelector ||
		newNodeObj.Labels[egressBGPExportLabelKey] == egressBGPExportLabelValue
	expectEgressIPRouteInFRR(failoverIP, failoverExported, newNodeObj, !failoverExported)

	failoverServerIP := serverIPByEgressIP[failoverIP]
	gomega.Eventually(func() error {
		snatErr := curlAgnHostClientIPFromPod(f.Namespace.Name, localPod.Name, failoverIP, failoverServerIP, snatCheckPort)
		if snatErr != nil {
			return fmt.Errorf("SNAT check from local pod %s to server %s for failover egressIP %s: %w", localPod.Name, failoverServerIP, failoverIP, snatErr)
		}
		return nil
	}, bgpEgressIPPollTimeout, retryInterval).Should(gomega.Succeed(),
		"local pod %s did not SNAT to failover egressIP %s toward server %s", localPod.Name, failoverIP, failoverServerIP)
	gomega.Eventually(func() error {
		snatErr := curlAgnHostClientIPFromPod(f.Namespace.Name, remotePod.Name, failoverIP, failoverServerIP, snatCheckPort)
		if snatErr != nil {
			return fmt.Errorf("SNAT check from remote pod %s to server %s for failover egressIP %s: %w", remotePod.Name, failoverServerIP, failoverIP, snatErr)
		}
		return nil
	}, bgpEgressIPPollTimeout, retryInterval).Should(gomega.Succeed(),
		"remote pod %s did not SNAT to failover egressIP %s toward server %s", remotePod.Name, failoverIP, failoverServerIP)
}

// createBGPEgressIPL3CUDN sets up L3 primary CUDN e2e fixtures: a UDN namespace, then a
// ClusterUserDefinedNetwork (Layer3 primary) selected by that namespace. Returns the CUDN label
// key for RouteAdvertisements networkSelectors, the OVN network name that is passed to
// waitBGPEgressCUDNHostSubnetsOnNodes (poll until egress nodes have host subnets on that UDN),
// and a cleanup func (CUDN deletion is registered via DeferCleanup as soon as create succeeds;
// namespace cleanup is handled by the test framework).
func createBGPEgressIPL3CUDN(f *framework.Framework) (string, string, func()) {
	suffix := framework.RandomSuffix()
	cudnLabel := "bgp-eip-l3-" + suffix

	namespace, err := f.CreateNamespace(context.TODO(), f.BaseName+"-"+suffix, map[string]string{
		"e2e-framework":           f.BaseName,
		RequiredUDNNamespaceLabel: "",
	})
	framework.ExpectNoError(err, "failed to create namespace for L3 CUDN EgressIP test")
	f.Namespace = namespace
	updateNamespaceLabels(f, namespace, map[string]string{"name": namespace.Name})

	udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
	framework.ExpectNoError(err, "failed to create UDN client")

	cudn := &udnv1.ClusterUserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "bgp-eip-l3-",
			Labels:       map[string]string{cudnLabel: ""},
		},
		Spec: udnv1.ClusterUserDefinedNetworkSpec{
			NamespaceSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{f.Namespace.Name},
			}}},
			Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role:    "Primary",
					Subnets: filterL3Subnets(f.ClientSet, cudnLayer3SubnetsB()),
				},
			},
		},
	}
	createdCUDN, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.Background(), cudn, metav1.CreateOptions{})
	framework.ExpectNoError(err, "failed to create L3 ClusterUserDefinedNetwork")
	ginkgo.DeferCleanup(deleteBGPEgressIPResourceForCleanup, "ClusterUserDefinedNetwork", createdCUDN.Name, func() error {
		return udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.Background(), createdCUDN.Name, metav1.DeleteOptions{})
	})
	gomega.Eventually(clusterUserDefinedNetworkReadyFunc(context.Background(), f.DynamicClient, createdCUDN.Name), 60*time.Second, time.Second).Should(gomega.Succeed(),
		"L3 ClusterUserDefinedNetwork %s did not become ready", createdCUDN.Name)

	return cudnLabel, util.GenerateCUDNNetworkName(createdCUDN.Name), func() {}
}

// createBGPL3CUDNPodNetworkRouteAdvertisement advertises pod CIDRs for the L3 CUDN (north-south for client pods).
func createBGPL3CUDNPodNetworkRouteAdvertisement(raClient raclientset.Interface, name, cudnMatchLabel string) {
	ra := &rav1.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: rav1.RouteAdvertisementsSpec{
			NetworkSelectors: apitypes.NetworkSelectors{
				{
					NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
					ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
						NetworkSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{cudnMatchLabel: ""},
						},
					},
				},
			},
			NodeSelector:             metav1.LabelSelector{},
			FRRConfigurationSelector: bgpE2EReceiveAllFRRConfigurationSelector,
			Advertisements:           []rav1.AdvertisementType{rav1.PodNetwork},
		},
	}
	_, err := raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
	framework.ExpectNoError(err, "failed to create pod-network RouteAdvertisements %s", ra.Name)
}

// createBGPEgressIPRouteAdvertisement creates an RA with Advertisements=[EgressIP] on default or CUDN network.
func createBGPEgressIPRouteAdvertisement(raClient raclientset.Interface, name, cudnMatchLabel string, nodeSelector metav1.LabelSelector) {
	networkSelectors := apitypes.NetworkSelectors{
		{NetworkSelectionType: apitypes.DefaultNetwork},
	}
	if cudnMatchLabel != "" {
		networkSelectors = apitypes.NetworkSelectors{
			{
				NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
				ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
					NetworkSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{cudnMatchLabel: ""},
					},
				},
			},
		}
	}
	ra := &rav1.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: rav1.RouteAdvertisementsSpec{
			NetworkSelectors:         networkSelectors,
			NodeSelector:             nodeSelector,
			FRRConfigurationSelector: bgpE2EReceiveAllFRRConfigurationSelector,
			Advertisements:           []rav1.AdvertisementType{rav1.EgressIP},
		},
	}
	_, err := raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
	framework.ExpectNoError(err, "failed to create EgressIP RouteAdvertisements %s", name)
}

func waitRouteAdvertisementCondition(raClient raclientset.Interface, name string, wantStatus metav1.ConditionStatus, wantReason, messageContains string) {
	gomega.Eventually(func() error {
		ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("getting RouteAdvertisements %s: %w", name, err)
		}
		c := meta.FindStatusCondition(ra.Status.Conditions, rav1.RouteAdvertisementsAccepted)
		if c == nil {
			return fmt.Errorf("no Accepted condition on RouteAdvertisements %s", name)
		}
		if wantStatus != "" && c.Status != wantStatus {
			return fmt.Errorf("RouteAdvertisements %s: want status %s, got %s (reason=%s message=%s)",
				name, wantStatus, c.Status, c.Reason, c.Message)
		}
		if wantReason != "" && c.Reason != wantReason {
			return fmt.Errorf("RouteAdvertisements %s: want reason %s, got %s (status=%s message=%s)",
				name, wantReason, c.Reason, c.Status, c.Message)
		}
		if messageContains != "" && !strings.Contains(c.Message, messageContains) {
			return fmt.Errorf("RouteAdvertisements %s: message %q does not contain %q", name, c.Message, messageContains)
		}
		return nil
	}, 2*time.Minute, time.Second).Should(gomega.Succeed(),
		"timed out waiting for RouteAdvertisements %s to reach expected condition", name)
}

// waitBGPEgressCUDNHostSubnetsOnNodes waits for L3 CUDN host subnets before EgressIP assignment on that network.
func waitBGPEgressCUDNHostSubnetsOnNodes(cs clientset.Interface, networkName string, nodeNames ...string) {
	for _, nodeName := range nodeNames {
		gomega.Eventually(func() error {
			node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("getting node %s: %w", nodeName, err)
			}
			ipNets, err := util.ParseNodeHostSubnetAnnotation(node, networkName)
			if err != nil {
				return fmt.Errorf("parsing host subnet annotation on node %s for network %s: %w", nodeName, networkName, err)
			}
			if len(ipNets) == 0 {
				return fmt.Errorf("node %s has no host subnets for network %s", nodeName, networkName)
			}
			return nil
		}, 2*time.Minute, 2*time.Second).Should(gomega.Succeed(),
			"timed out waiting for CUDN host subnets for network %s on node %s", networkName, nodeName)
	}
}

// egressIPListWithSpec is a minimal JSON shape for kubectl get eip -A -o json (occupied addresses).
type egressIPListWithSpec struct {
	Items []struct {
		Spec struct {
			EgressIPs []string `json:"egressIPs"`
		} `json:"spec"`
	} `json:"items"`
}

// clusterEgressIPSpecAddresses returns all spec.egressIPs already claimed in the cluster.
func clusterEgressIPSpecAddresses() (sets.Set[string], error) {
	stdout, err := e2ekubectl.RunKubectl("", "get", "eip", "-A", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("listing EgressIP objects: %w", err)
	}
	list := egressIPListWithSpec{}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		return nil, fmt.Errorf("unmarshalling EgressIP list: %w", err)
	}
	occupied := sets.New[string]()
	for _, item := range list.Items {
		for _, ip := range item.Spec.EgressIPs {
			if parsed := net.ParseIP(ip); parsed != nil {
				ip = parsed.String()
			}
			occupied.Insert(ip)
		}
	}
	return occupied, nil
}

// bgpDualStackEgressIPPlan holds dual-stack-only knobs for IP allocation and failover prep
type bgpDualStackEgressIPPlan struct {
	// labelFailoverToNodeWithEgressBGPNodeselector: when true, nonInitialEgressNode gets egress-nodes=bgp during prepareBGPEgressIPFailoverToNode.
	labelFailoverToNodeWithEgressBGPNodeselector bool
	failoverTo                                   string
}

// bgpDualStackEgressIPPlanForScenario maps dual-stack cases to failover-target labeling and allocation.
func bgpDualStackEgressIPPlanForScenario(scenario bgpEgressIPScenario, nodeSet bgpEgressIPNodeSet) bgpDualStackEgressIPPlan {
	plan := bgpDualStackEgressIPPlan{failoverTo: nodeSet.nonInitialEgressNode.Name}
	if scenario.useRouteAdvertisementsEgressIPNodeSelector && !scenario.failoverFromEgressIPAdvertisedNode {
		plan.labelFailoverToNodeWithEgressBGPNodeselector = true
	}
	return plan
}

// allocateBGPEgressTestIPsFromMachineNetwork matches egressip.go machine-network allocators.
func allocateBGPEgressTestIPsFromMachineNetwork(cs clientset.Interface, nodeSet bgpEgressIPNodeSet, scenario bgpEgressIPScenario) ([]string, bgpDualStackEgressIPPlan) {
	var egressIPs []string
	plan := bgpDualStackEgressIPPlan{}
	dualStack := nodeSet.dualStackCluster
	gomega.Eventually(func() error {
		occupied, err := clusterEgressIPSpecAddresses()
		if err != nil {
			return fmt.Errorf("fetching occupied EgressIP addresses: %w", err)
		}
		if dualStack {
			plan = bgpDualStackEgressIPPlanForScenario(scenario, nodeSet)
		}
		if err := allocateBGPEgressTestIPs(cs, nodeSet, plan, dualStack, occupied, &egressIPs); err != nil {
			return fmt.Errorf("allocating BGP egress test IPs from machine network: %w", err)
		}
		return nil
	}, 2*time.Minute, time.Second).Should(gomega.Succeed(),
		"timed out allocating BGP egress test IPs from machine network")
	return egressIPs, plan
}

// allocateBGPEgressTestIPs picks one machine-network egress IP (single-stack) or IPv4+IPv6 (dual-stack).
func allocateBGPEgressTestIPs(cs clientset.Interface, nodeSet bgpEgressIPNodeSet, plan bgpDualStackEgressIPPlan, dualStack bool, occupied sets.Set[string], egressIPs *[]string) error {
	maxAttempts := int(bgpEgressIPHostSubnetIPEnd - bgpEgressIPHostSubnetIPOffset + 1)
	if dualStack {
		v4Alloc, v6Alloc := nodeSet.egressNode1.Name, nodeSet.egressNode2.Name
		var lastAllocErr error
		for attempt := 0; attempt < maxAttempts; attempt++ {
			v4IP, err := allocators.NewPrimaryIPv4(v4Alloc)
			if err != nil {
				lastAllocErr = fmt.Errorf("allocating IPv4 egress test IP on node %s: %w", v4Alloc, err)
				break
			}
			v6IP, err := allocators.NewPrimaryIPv6(v6Alloc)
			if err != nil {
				lastAllocErr = fmt.Errorf("allocating IPv6 egress test IP on node %s: %w", v6Alloc, err)
				break
			}
			v4Str, v6Str := v4IP.String(), v6IP.String()
			if occupied.Has(v4Str) || occupied.Has(v6Str) {
				continue
			}
			if !bgpEgressIPAssignableOnNodeByName(cs, nodeSet.egressNode1.Name, v4Str) || !bgpEgressIPAssignableOnNodeByName(cs, nodeSet.egressNode2.Name, v4Str) ||
				!bgpEgressIPAssignableOnNodeByName(cs, nodeSet.egressNode1.Name, v6Str) || !bgpEgressIPAssignableOnNodeByName(cs, nodeSet.egressNode2.Name, v6Str) ||
				!bgpEgressIPAssignableOnNodeByName(cs, plan.failoverTo, v4Str) || !bgpEgressIPAssignableOnNodeByName(cs, plan.failoverTo, v6Str) {
				continue
			}
			*egressIPs = []string{v4Str, v6Str}
			framework.Logf("allocated BGP dual-stack egress test IPs %s and %s (assignable on egress nodes %s/%s, failover to %s)",
				v4Str, v6Str, nodeSet.egressNode1.Name, nodeSet.egressNode2.Name, plan.failoverTo)
			return nil
		}
		if lastAllocErr != nil {
			return fmt.Errorf("no dual-stack machine-network egress IPs assignable on egress nodes %s/%s: %w",
				nodeSet.egressNode1.Name, nodeSet.egressNode2.Name, lastAllocErr)
		}
		return fmt.Errorf("no dual-stack machine-network egress IPs assignable on egress nodes %s/%s (occupied: %v)",
			nodeSet.egressNode1.Name, nodeSet.egressNode2.Name, sets.List(occupied))
	}

	node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeSet.egressNode1.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting egress node %s: %w", nodeSet.egressNode1.Name, err)
	}
	useIPv6Allocator := isIPv6Supported(cs) && !isIPv4Supported(cs)
	for range maxAttempts {
		var ip net.IP
		if useIPv6Allocator {
			ip, err = allocators.NewPrimaryIPv6(node.Name)
		} else {
			ip, err = allocators.NewPrimaryIPv4(node.Name)
		}
		if err != nil {
			return fmt.Errorf("allocating candidate egress IP on node %s: %w", node.Name, err)
		}
		candidate := ip.String()
		if occupied.Has(candidate) {
			continue
		}
		if !egressIPAssignableOnNodeForNetwork(node, candidate, types.DefaultNetworkName) ||
			!bgpEgressIPAssignableOnNodeByName(cs, nodeSet.nonInitialEgressNode.Name, candidate) {
			continue
		}
		*egressIPs = []string{candidate}
		allocatorFamily := "IPv4"
		if useIPv6Allocator {
			allocatorFamily = "IPv6"
		}
		framework.Logf("allocated BGP egress test IP %s via machine-network %s allocator on node %s", candidate, allocatorFamily, node.Name)
		return nil
	}
	return fmt.Errorf("no assignable machine-network egress IP on node %s (occupied: %v)", nodeSet.egressNode1.Name, sets.List(occupied))
}

// bgpEgressIPAssignableOnNodeByName wraps egressIPAssignableOnNode with a live Node GET.
func bgpEgressIPAssignableOnNodeByName(cs clientset.Interface, nodeName, ipStr string) bool {
	node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return egressIPAssignableOnNodeForNetwork(node, ipStr, types.DefaultNetworkName)
}

// egressIPAssignableOnNodeForNetwork mirrors cluster-manager assignability for a given network name.
func egressIPAssignableOnNodeForNetwork(node *corev1.Node, ipStr, _ string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	eIPConfig, err := util.GetNodeEIPConfig(node)
	if err != nil {
		return false
	}
	network, err := util.GetEgressIPNetwork(node, eIPConfig, ip)
	return err == nil && network != ""
}

// prepareBGPEgressNodesForEgressIP clears egress-assignable on nodeSet.allScenarioNodeNames that are not
// in nodeSet.egressNodes, labels those egress nodes, and restores each scenario node's original label state on cleanup.
func prepareBGPEgressNodesForEgressIP(cs clientset.Interface, nodeSet bgpEgressIPNodeSet) {
	assignable := sets.New(nodeSet.egressNodes...)
	hadAssignable := make(map[string]bool, len(nodeSet.allScenarioNodeNames))
	for _, nodeName := range nodeSet.allScenarioNodeNames {
		node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get node %s", nodeName)
		_, hadAssignable[nodeName] = node.Labels[egressAssignableLabel]
	}
	ginkgo.DeferCleanup(func() {
		for _, nodeName := range nodeSet.allScenarioNodeNames {
			if hadAssignable[nodeName] {
				e2enode.AddOrUpdateLabelOnNode(cs, nodeName, egressAssignableLabel, "dummy")
			} else {
				e2enode.RemoveLabelOffNode(cs, nodeName, egressAssignableLabel)
			}
		}
	})
	for _, nodeName := range nodeSet.allScenarioNodeNames {
		if !assignable.Has(nodeName) && hadAssignable[nodeName] {
			e2enode.RemoveLabelOffNode(cs, nodeName, egressAssignableLabel)
		}
	}

	ensureBGPEgressIPHealthCheckReachable(nodeSet.egressNodes...)
	for _, nodeName := range nodeSet.egressNodes {
		e2enode.AddOrUpdateLabelOnNode(cs, nodeName, egressAssignableLabel, "dummy")
		e2enode.ExpectNodeHasLabel(context.TODO(), cs, nodeName, egressAssignableLabel, "dummy")
	}
}

// getClusterNode re-reads the Node from the API so label checks match cluster state (not cached List objects).
func getClusterNode(cs clientset.Interface, nodeName string) corev1.Node {
	node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	framework.ExpectNoError(err, "failed to get node %s", nodeName)
	return *node
}

// prepareBGPEgressIPFailoverToNode prepares nonInitialEgressNode as the failover target (egress-assignable; optional egress-nodes=bgp).
// When labelFailoverToNodeWithEgressBGPNodeselector is false, only egress-assignable is set on the failover target.
// When true, egress-nodes=bgp is also set so the RouteAdvertisements EgressIP nodeSelector matches after failover.
// Opens the EgressIP health-check port and waits for host-cidrs before a label transition so cluster-manager runs addEgressNode.
func prepareBGPEgressIPFailoverToNode(f *framework.Framework, failoverToNodeName string, labelFailoverToNodeWithEgressBGPNodeselector bool) {
	cs := f.ClientSet
	if labelFailoverToNodeWithEgressBGPNodeselector {
		ginkgo.By(fmt.Sprintf("Labeling failover target %s egress-assignable and egress-nodes=bgp", failoverToNodeName))
	} else {
		ginkgo.By(fmt.Sprintf("Labeling failover target %s egress-assignable only", failoverToNodeName))
	}
	node, err := cs.CoreV1().Nodes().Get(context.TODO(), failoverToNodeName, metav1.GetOptions{})
	framework.ExpectNoError(err, "failed to get failover target node %s", failoverToNodeName)
	origBGPExportValue, hadBGPExportLabel := node.Labels[egressBGPExportLabelKey]
	ginkgo.DeferCleanup(func() { e2enode.RemoveLabelOffNode(cs, failoverToNodeName, egressAssignableLabel) })
	ginkgo.DeferCleanup(func() {
		if hadBGPExportLabel {
			e2enode.AddOrUpdateLabelOnNode(cs, failoverToNodeName, egressBGPExportLabelKey, origBGPExportValue)
		} else {
			e2enode.RemoveLabelOffNode(cs, failoverToNodeName, egressBGPExportLabelKey)
		}
	})
	ensureBGPEgressIPHealthCheckReachable(failoverToNodeName)
	waitNodesHostCIDRsForEgressIP(cs, failoverToNodeName)
	e2enode.RemoveLabelOffNode(cs, failoverToNodeName, egressAssignableLabel)
	labelNodeForEgress(f, failoverToNodeName)
	if labelFailoverToNodeWithEgressBGPNodeselector {
		e2enode.AddOrUpdateLabelOnNode(cs, failoverToNodeName, egressBGPExportLabelKey, egressBGPExportLabelValue)
	} else {
		e2enode.RemoveLabelOffNode(cs, failoverToNodeName, egressBGPExportLabelKey)
	}
	e2enode.ExpectNodeHasLabel(context.TODO(), cs, failoverToNodeName, egressAssignableLabel, "dummy")
}

// getEgressIPStatusItems returns status.items from the EgressIP CR (kubectl json).
func getEgressIPStatusItems(eipName string) ([]egressIPStatus, error) {
	var egressIP egressIP
	stdout, err := e2ekubectl.RunKubectl("default", "get", "eip", eipName, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("running kubectl get EgressIP %s: %w", eipName, err)
	}
	if err := json.Unmarshal([]byte(stdout), &egressIP); err != nil {
		return nil, fmt.Errorf("decoding EgressIP %s JSON: %w", eipName, err)
	}
	return egressIP.Status.Items, nil
}

// egressIPAssignmentsByAddress maps spec egress IP string to assigned node name.
func egressIPAssignmentsByAddress(items []egressIPStatus) map[string]string {
	m := make(map[string]string, len(items))
	for _, item := range items {
		m[item.EgressIP] = item.Node
	}
	return m
}

// waitForEgressIPAssignments polls until every spec egress IP has a node in EgressIP status.
func waitForEgressIPAssignments(eipName string, expectedIPs []string) map[string]string {
	var assignments map[string]string
	var lastStatusErr error
	err := wait.PollImmediate(retryInterval, bgpEgressIPPollTimeout, func() (bool, error) {
		items, statusErr := getEgressIPStatusItems(eipName)
		if statusErr != nil {
			lastStatusErr = statusErr
			return false, nil // transient; retry
		}
		assignments = egressIPAssignmentsByAddress(items)
		if len(items) != len(expectedIPs) {
			return false, nil
		}
		for _, ip := range expectedIPs {
			if assignments[ip] == "" {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		framework.Failf("timed out waiting for egress IP assignments on %s (want %v): last map %v, last error: %v",
			eipName, expectedIPs, assignments, lastStatusErr)
	}
	return assignments
}

// pickFailoverFromFirstEgressNode walks egress nodes in order (egressNode1, then egressNode2 when dual-stack),
// picks the first that holds a spec egress IP assignable on failoverToEgressNode, and returns that IP and source node.
func pickFailoverFromFirstEgressNode(cs clientset.Interface, assignments map[string]string, nodeSet bgpEgressIPNodeSet, failoverToEgressNode string) (string, string) {
	for _, onNode := range nodeSet.egressNodes {
		var ip string
		for candidate, assignedNode := range assignments {
			if assignedNode == onNode {
				ip = candidate
				break
			}
		}
		if ip == "" {
			continue
		}
		if !bgpEgressIPAssignableOnNodeByName(cs, failoverToEgressNode, ip) {
			continue
		}
		return ip, onNode
	}
	framework.Failf("no egress IP on egress nodes %v can fail over to %s", nodeSet.egressNodes, failoverToEgressNode)
	return "", ""
}

// waitForEgressIPFailover polls until failoverIP moves to toNode.
func waitForEgressIPFailover(eipName, failoverIP, fromNode, toNode string, unchanged map[string]string, expectedStatusCount int) map[string]string {
	var assignments map[string]string
	var lastStatusErr error
	err := wait.PollImmediate(retryInterval, bgpEgressIPPollTimeout, func() (bool, error) {
		items, statusErr := getEgressIPStatusItems(eipName)
		if statusErr != nil {
			lastStatusErr = statusErr
			return false, nil // transient; retry
		}
		assignments = egressIPAssignmentsByAddress(items)
		if expectedStatusCount > 0 && len(items) != expectedStatusCount {
			return false, nil
		}
		if assignments[failoverIP] != toNode {
			return false, nil
		}
		for ip, node := range unchanged {
			if ip == failoverIP {
				continue
			}
			if assignments[ip] != node {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		framework.Failf("timed out waiting for egress IP %s to move from node %s to node %s (expected %d status items): %v, last error: %v",
			failoverIP, fromNode, toNode, expectedStatusCount, assignments, lastStatusErr)
	}
	return assignments
}

// hostRoutePrefix returns the BGP-advertised host route CIDR for an EgressIP address.
func hostRoutePrefix(egressIP string) string {
	if strings.Contains(egressIP, "/") {
		return egressIP
	}
	if utilnet.IsIPv6String(egressIP) {
		return egressIP + "/128"
	}
	return egressIP + "/32"
}

// egressIPFRRRouteCmd returns the external FRR container and the ip-route-show command
// for the given egress IP (handles both IPv4 /32 and IPv6 /128 host routes).
func egressIPFRRRouteCmd(egressIP string) (infraapi.ExternalContainer, []string) {
	ipVer := ""
	if utilnet.IsIPv6String(egressIP) {
		ipVer = " -6"
	}
	cmd := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, hostRoutePrefix(egressIP)), " ")
	return infraapi.ExternalContainer{Name: routerContainerName}, cmd
}

// bgpEgressIPNodeIPv6LinkLocalNexthop returns the IPv6 link-local address on the node's interface
// on the infrastructure primary network (BGP-facing interface for the provider).
func bgpEgressIPNodeIPv6LinkLocalNexthop(nodeName string) (string, error) {
	provider := infraprovider.Get()
	network, err := provider.PrimaryNetwork()
	if err != nil {
		return "", fmt.Errorf("getting primary infrastructure network: %w", err)
	}
	nodeIface, err := provider.GetK8NodeNetworkInterface(nodeName, network)
	if err != nil {
		return "", fmt.Errorf("getting node %s interface on network %s: %w", nodeName, network.Name(), err)
	}
	ifName := nodeIface.GetName()
	if ifName == "" {
		return "", fmt.Errorf("node %s has no interface name on network %s", nodeName, network.Name())
	}
	output, err := provider.ExecK8NodeCommand(nodeName, []string{"ip", "-6", "addr", "show", "dev", ifName, "scope", "link"})
	if err != nil {
		return "", fmt.Errorf("querying IPv6 link-local addresses on %s for node %s: %w", ifName, nodeName, err)
	}
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, "inet6") {
			continue
		}
		for _, part := range strings.Fields(line) {
			if !strings.Contains(part, "/") {
				continue
			}
			ipStr := strings.SplitN(part, "/", 2)[0]
			ip := net.ParseIP(ipStr)
			if ip != nil && ip.To4() == nil && ip.IsLinkLocalUnicast() {
				return ipStr, nil
			}
		}
	}
	return "", fmt.Errorf("no IPv6 link-local address on %s for node %s", ifName, nodeName)
}

// bgpEgressIPFRRNexthop is the nexthop FRR should show for an egress IP on node (IPv6 link-local or IPv4 InternalIP).
func bgpEgressIPFRRNexthop(node corev1.Node, isIPv6 bool) string {
	if isIPv6 {
		lla, err := bgpEgressIPNodeIPv6LinkLocalNexthop(node.Name)
		framework.ExpectNoError(err, "failed to get IPv6 link-local address for node %s", node.Name)
		return lla
	}
	ips := e2enode.GetAddressesByTypeAndFamily(&node, corev1.NodeInternalIP, corev1.IPv4Protocol)
	gomega.Expect(len(ips)).To(gomega.BeNumerically(">", 0), "must find a valid IPv4 nexthop for node %s", node.Name)
	return ips[0]
}

// expectEgressIPRouteInFRR checks whether the egress IP host route is exported on external FRR.
// When expectExported is true, node is the assigned egress node (nexthop is derived from it).
// For expectExported false, routeWithdrawal waits for FRR to drop a previously advertised route,
// then asserts it stays withdrawn; otherwise a bounded Consistently verifies the route never appears.
func expectEgressIPRouteInFRR(egressIP string, expectExported bool, node corev1.Node, routeWithdrawal bool) {
	container, cmd := egressIPFRRRouteCmd(egressIP)
	if expectExported {
		nexthop := bgpEgressIPFRRNexthop(node, utilnet.IsIPv6String(egressIP))
		gomega.Eventually(func() error {
			routes, err := infraprovider.Get().ExecExternalContainerCommand(container, cmd)
			if err != nil {
				return fmt.Errorf("querying FRR routes in container %s: %w", container, err)
			}
			fields := strings.Fields(routes)
			var vias []string
			for i := 0; i+1 < len(fields); i++ {
				if fields[i] == "via" {
					vias = append(vias, fields[i+1])
				}
			}
			if len(vias) != 1 || vias[0] != nexthop {
				return fmt.Errorf("route %s nexthops %v, want only %s", hostRoutePrefix(egressIP), vias, nexthop)
			}
			return nil
		}, 90*time.Second, 5*time.Second).Should(gomega.Succeed(),
			"expected FRR route for %s via %s on node %s", hostRoutePrefix(egressIP), nexthop, node.Name)
		return
	}
	routeAbsent := func() error {
		routes, err := infraprovider.Get().ExecExternalContainerCommand(container, cmd)
		if err != nil {
			return fmt.Errorf("querying FRR routes in container %s: %w", container, err)
		}
		if strings.TrimSpace(routes) != "" {
			return fmt.Errorf("FRR still shows route for %s: %s", hostRoutePrefix(egressIP), strings.TrimSpace(routes))
		}
		return nil
	}
	if routeWithdrawal {
		gomega.Eventually(routeAbsent, bgpEgressIPPollTimeout, 5*time.Second).Should(gomega.Succeed(),
			"expected FRR to withdraw route for egress IP prefix %s", hostRoutePrefix(egressIP))
	}
	gomega.Consistently(routeAbsent, bgpEgressIPRouteAbsentDuration, 5*time.Second).Should(gomega.Succeed(),
		"expected no FRR BGP route for egress IP prefix %s", hostRoutePrefix(egressIP))
}

func deleteBGPEgressIPResourceForCleanup(resourceKind, name string, deleteFn func() error) error {
	err := wait.PollImmediate(retryInterval, bgpEgressIPResourceDeleteTimeout, func() (bool, error) {
		delErr := deleteFn()
		if delErr == nil || apierrors.IsNotFound(delErr) {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("deleting %s %s: %w", resourceKind, name, err)
	}
	return nil
}
