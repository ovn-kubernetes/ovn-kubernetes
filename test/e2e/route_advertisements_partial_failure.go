// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
	frrapi "github.com/metallb/frr-k8s/api/v1beta1"
	frrclient "github.com/metallb/frr-k8s/pkg/client/clientset/versioned"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	rav1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	utilnet "k8s.io/utils/net"
)

// Temporary nodes let us exercise controller failures without damaging the
// subnet annotations used by running workloads. They do not run FRR daemons:
// this verifies API reconciliation, not BGP convergence or datapath behavior.
var _ = ginkgo.Describe("BGP partial node configuration failure", feature.RouteAdvertisements, ginkgo.Serial, func() {
	f := wrappedTestFramework("ra-partial-failure")
	ginkgo.It("withdraws failed node configurations and recovers without blocking healthy nodes", ginkgo.SpecTimeout(10*time.Minute), func(ctx ginkgo.SpecContext) {
		raf, err := raclient.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err, "create RouteAdvertisements client")
		frr, err := frrclient.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err, "create FRRConfiguration client")
		const testLabel = "e2e.ovn.org/ra-partial-failure"
		selector := metav1.LabelSelector{MatchLabels: map[string]string{testLabel: f.Namespace.Name}}
		nodeNames := []string{f.Namespace.Name + "-healthy", f.Namespace.Name + "-failing"}
		for _, name := range nodeNames {
			_, err = f.ClientSet.CoreV1().Nodes().Create(ctx, &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
					testLabel: f.Namespace.Name, corev1.LabelHostname: name,
				}},
				Spec: corev1.NodeSpec{Unschedulable: true},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err, "create temporary node %s", name)
			ginkgo.DeferCleanup(func(ctx ginkgo.SpecContext) {
				framework.ExpectNoError(f.ClientSet.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{}), "delete temporary node %s", name)
			}, ginkgo.NodeTimeout(time.Minute))
		}
		// Let cluster-manager allocate real subnets so this test does not
		// introduce duplicate or out-of-range allocations.
		for _, name := range nodeNames {
			gomega.Eventually(ctx, func() error {
				node, err := f.ClientSet.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				subnets, err := util.ParseNodeHostSubnetsAnnotation(node)
				if err != nil {
					return err
				}
				if len(subnets[types.DefaultNetworkName]) == 0 {
					return fmt.Errorf("node %s has no default subnet", name)
				}
				return nil
			}, 60*time.Second, time.Second).Should(gomega.Succeed(), "allocate subnet for temporary node %s", name)
		}
		var neighbors []frrapi.Neighbor
		for _, address := range []string{"192.0.2.1", "2001:db8::1"} {
			if isIPFamilySupported(f.ClientSet, address) {
				neighbors = append(neighbors, frrapi.Neighbor{ASN: 64513, Address: address})
			}
		}
		_, err = frr.ApiV1beta1().FRRConfigurations(f.Namespace.Name).Create(ctx, &frrapi.FRRConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "peers", Labels: selector.MatchLabels},
			Spec: frrapi.FRRConfigurationSpec{NodeSelector: selector, BGP: frrapi.BGPConfig{Routers: []frrapi.Router{{
				ASN: 64512, Neighbors: neighbors,
			}}}},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err, "create isolated peering template")
		ginkgo.DeferCleanup(func(ctx ginkgo.SpecContext) {
			framework.ExpectNoError(frr.ApiV1beta1().FRRConfigurations(f.Namespace.Name).Delete(ctx, "peers", metav1.DeleteOptions{}), "delete isolated peering template")
		}, ginkgo.NodeTimeout(time.Minute))
		ra, err := raf.K8sV1().RouteAdvertisements().Create(ctx, &rav1.RouteAdvertisements{
			ObjectMeta: metav1.ObjectMeta{Name: f.Namespace.Name},
			Spec: rav1.RouteAdvertisementsSpec{
				NetworkSelectors:         apitypes.NetworkSelectors{{NetworkSelectionType: apitypes.DefaultNetwork}},
				FRRConfigurationSelector: selector,
				Advertisements:           []rav1.AdvertisementType{rav1.PodNetwork},
			},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err, "create isolated advertisement")
		ginkgo.DeferCleanup(func(ctx ginkgo.SpecContext) {
			framework.ExpectNoError(raf.K8sV1().RouteAdvertisements().Delete(ctx, ra.Name, metav1.DeleteOptions{}), "delete RA %s", ra.Name)
		}, ginkgo.NodeTimeout(time.Minute))
		waitForState := func(failure string, wantNodes ...string) map[string]frrapi.FRRConfiguration {
			expectedStatus := metav1.ConditionFalse
			if failure != "" {
				expectedStatus = metav1.ConditionTrue
			}
			var byNode map[string]frrapi.FRRConfiguration
			gomega.Eventually(ctx, func(g gomega.Gomega) {
				current, err := raf.K8sV1().RouteAdvertisements().Get(ctx, ra.Name, metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "get RA %s while waiting for Degraded=%s", ra.Name, expectedStatus)
				accepted := meta.FindStatusCondition(current.Status.Conditions, "Accepted")
				g.Expect(accepted).NotTo(gomega.BeNil(), "RA %s must report Accepted", ra.Name)
				g.Expect(accepted.ObservedGeneration).To(gomega.Equal(current.Generation), "Accepted must reflect the current RA generation")
				g.Expect(accepted.Status).To(gomega.Equal(metav1.ConditionTrue), "node generation failures must not reject a valid RA")
				condition := meta.FindStatusCondition(current.Status.Conditions, "Degraded")
				g.Expect(condition).NotTo(gomega.BeNil(), "RA %s must report Degraded", ra.Name)
				g.Expect(condition.ObservedGeneration).To(gomega.Equal(current.Generation), "Degraded must reflect the current RA generation")
				g.Expect(condition.Status).To(gomega.Equal(expectedStatus), "RA %s must report Degraded=%s: %s", ra.Name, expectedStatus, condition.Message)
				configs, err := frr.ApiV1beta1().FRRConfigurations(f.Namespace.Name).List(ctx, metav1.ListOptions{
					LabelSelector: types.OvnRouteAdvertisementsKey + "=" + ra.Name,
				})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "list generated configurations for RA %s", ra.Name)
				if failure != "" {
					g.Expect(condition.Message).To(gomega.ContainSubstring(failure), "identify the failed node or template")
				}
				g.Expect(configs.Items).To(gomega.HaveLen(len(wantNodes)), "one working template per expected node")

				byNode = map[string]frrapi.FRRConfiguration{}
				for _, cfg := range configs.Items {
					for _, name := range nodeNames {
						if cfg.Labels[types.OvnRouteAdvertisementsNodeKey] == util.RouteAdvertisementsNodeLabel(name) {
							byNode[name] = cfg
						}
					}
				}
				nodes := make(map[string]*corev1.Node, len(nodeNames))
				for _, name := range nodeNames {
					node, err := f.ClientSet.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
					g.Expect(err).NotTo(gomega.HaveOccurred(), "get node %s to verify generated configuration selection", name)
					nodes[name] = node
				}
				for _, name := range wantNodes {
					g.Expect(byNode).To(gomega.HaveKey(name), "generate configuration for healthy node %s", name)
					cfg := byNode[name]
					subnets, err := util.ParseNodeHostSubnetAnnotation(nodes[name], types.DefaultNetworkName)
					g.Expect(err).NotTo(gomega.HaveOccurred(), "read current pod subnets for %s", name)
					for _, subnet := range subnets {
						g.Expect(util.RouteAdvertisementsConfigurationExportsPrefix(&cfg, "", subnet.String())).To(gomega.BeTrue(), "advertise pod subnet %s for %s", subnet, name)
					}
					selector, err := metav1.LabelSelectorAsSelector(&cfg.Spec.NodeSelector)
					g.Expect(err).NotTo(gomega.HaveOccurred(), "parse generated configuration selector for node %s", name)
					for _, node := range nodes {
						g.Expect(selector.Matches(labels.Set(node.Labels))).To(gomega.Equal(node.Name == name),
							"configuration %s for %s checked against node %s", cfg.Name, name, node.Name)
					}
				}
			}, 60*time.Second, time.Second).Should(gomega.Succeed(), "RA %s must converge to Accepted=True and Degraded=%s", ra.Name, expectedStatus)
			return byNode
		}
		original := waitForState("", nodeNames...)
		node, err := f.ClientSet.CoreV1().Nodes().Get(ctx, nodeNames[1], metav1.GetOptions{})
		framework.ExpectNoError(err, "get node %s before removing its hostname label", nodeNames[1])
		originalValue := node.Labels[corev1.LabelHostname]
		patchHostname := func(ctx context.Context, value any) {
			patch, err := json.Marshal(map[string]any{
				"metadata": map[string]any{"labels": map[string]any{corev1.LabelHostname: value}},
			})
			framework.ExpectNoError(err, "marshal hostname patch for node %s", node.Name)
			_, err = f.ClientSet.CoreV1().Nodes().Patch(ctx, node.Name, ktypes.MergePatchType, patch, metav1.PatchOptions{})
			framework.ExpectNoError(err, "patch hostname label on node %s", node.Name)
		}
		// Register cleanup before removing the label. No kubelet runs on this
		// temporary node, so it stays missing until restored here.
		restore := func(ctx context.Context) { patchHostname(ctx, originalValue) }
		ginkgo.DeferCleanup(func(ctx ginkgo.SpecContext) { restore(ctx) }, ginkgo.NodeTimeout(time.Minute))
		patchHostname(ctx, nil)
		degraded := waitForState(nodeNames[1], nodeNames[0])
		gomega.Expect(degraded[nodeNames[0]].UID).To(gomega.Equal(original[nodeNames[0]].UID), "keep the healthy configuration")
		gomega.Expect(degraded[nodeNames[0]].Spec).To(gomega.Equal(original[nodeNames[0]].Spec), "the other node's failure must not change the healthy node's configuration")
		// Force new work on the healthy node while the other node still fails.
		// Merely observing its original configuration would not detect a
		// controller that aborted generation for every node.
		framework.ExpectNoError(frr.ApiV1beta1().FRRConfigurations(f.Namespace.Name).Delete(
			ctx, degraded[nodeNames[0]].Name, metav1.DeleteOptions{}), "delete the healthy node's configuration to require regeneration")
		regenerated := waitForState(nodeNames[1], nodeNames[0])
		gomega.Expect(regenerated[nodeNames[0]].UID).NotTo(gomega.Equal(original[nodeNames[0]].UID), "regenerate the healthy node's configuration while the other node still fails")
		gomega.Expect(regenerated).NotTo(gomega.HaveKey(nodeNames[1]), "the failed node must remain withdrawn during healthy-node regeneration")
		restore(ctx)
		recovered := waitForState("", nodeNames...)
		gomega.Expect(recovered[nodeNames[1]].UID).NotTo(gomega.Equal(original[nodeNames[1]].UID), "recreate the recovered node configuration")

		// A rejected edit withdraws even the healthy node's previous object.
		// Fixing it must recreate both nodes without needing a node event.
		ginkgo.By("withdrawing the whole RA on an invalid target VRF")
		current, err := raf.K8sV1().RouteAdvertisements().Get(ctx, ra.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "get RA %s before selecting an invalid target VRF", ra.Name)
		current.Spec.TargetVRF = "invalid/vrf"
		_, err = raf.K8sV1().RouteAdvertisements().Update(ctx, current, metav1.UpdateOptions{})
		framework.ExpectNoError(err, "update RA %s with an invalid target VRF", ra.Name)
		gomega.Eventually(ctx, func(g gomega.Gomega) {
			current, err := raf.K8sV1().RouteAdvertisements().Get(ctx, ra.Name, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "get RA %s while waiting for rejection", ra.Name)
			accepted := meta.FindStatusCondition(current.Status.Conditions, "Accepted")
			g.Expect(accepted).NotTo(gomega.BeNil(), "RA %s must report Accepted after the invalid edit", ra.Name)
			g.Expect(accepted.ObservedGeneration).To(gomega.Equal(current.Generation), "rejection must reflect the invalid RA generation")
			g.Expect(accepted.Status).To(gomega.Equal(metav1.ConditionFalse), "reject the invalid target VRF interface name")
			configs, err := frr.ApiV1beta1().FRRConfigurations(f.Namespace.Name).List(ctx, metav1.ListOptions{
				LabelSelector: types.OvnRouteAdvertisementsKey + "=" + ra.Name,
			})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "list configurations during withdrawal of rejected RA %s", ra.Name)
			g.Expect(configs.Items).To(gomega.BeEmpty(), "withdraw configurations on all nodes after rejecting the RA")
		}, 60*time.Second, time.Second).Should(gomega.Succeed(), "RA %s must be rejected and all its configurations withdrawn", ra.Name)
		ginkgo.By("recreating both nodes after correcting the RA")
		current, err = raf.K8sV1().RouteAdvertisements().Get(ctx, ra.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "get RA %s before restoring the target VRF", ra.Name)
		current.Spec.TargetVRF = ""
		_, err = raf.K8sV1().RouteAdvertisements().Update(ctx, current, metav1.UpdateOptions{})
		framework.ExpectNoError(err, "restore the valid target VRF on RA %s", ra.Name)
		recreated := waitForState("", nodeNames...)
		for _, name := range nodeNames {
			gomega.Expect(recreated[name].UID).NotTo(gomega.Equal(recovered[name].UID), "recreate configuration for node %s after correcting the RA", name)
		}
		ginkgo.By("retaining the working peer when another selected template has the wrong VRF")
		templates := frr.ApiV1beta1().FRRConfigurations(f.Namespace.Name)
		const badPeer = "wrong-vrf"
		_, err = templates.Create(ctx, &frrapi.FRRConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: badPeer, Labels: selector.MatchLabels},
			Spec: frrapi.FRRConfigurationSpec{NodeSelector: selector, BGP: frrapi.BGPConfig{Routers: []frrapi.Router{{
				ASN: 64512, VRF: "unused", Neighbors: neighbors,
			}}}},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err, "create the mismatched peering template")
		ginkgo.DeferCleanup(func(ctx ginkgo.SpecContext) {
			framework.ExpectNoError(templates.Delete(ctx, badPeer, metav1.DeleteOptions{}), "delete the mismatched peering template")
		}, ginkgo.NodeTimeout(time.Minute))
		withFailedPeer := waitForState(badPeer, nodeNames...)
		for _, name := range nodeNames {
			gomega.Expect(withFailedPeer[name].UID).To(gomega.Equal(recreated[name].UID), "retain the working peer on %s", name)
			gomega.Expect(withFailedPeer[name].Spec).To(gomega.Equal(recreated[name].Spec), "preserve the working peer's routing configuration on %s", name)
		}

	})
})

func testOverlaySNATRecovery(ctx ginkgo.SpecContext, f *framework.Framework, nodes []corev1.Node, routerIPv4, routerIPv6 string) {
	name := f.Namespace.Name
	selector := map[string]string{"advertise": name}
	frr, err := frrclient.NewForConfig(f.ClientConfig())
	framework.ExpectNoError(err, "create FRRConfiguration client")
	configs := frr.ApiV1beta1().FRRConfigurations(deploymentconfig.Get().FRRK8sNamespace())
	ra, err := raclient.NewForConfig(f.ClientConfig())
	framework.ExpectNoError(err, "create RouteAdvertisements client")
	nads, err := nadclient.NewForConfig(f.ClientConfig())
	framework.ExpectNoError(err, "create NAD client")
	allocation, release, err := allocators.ReserveBGP(ctx, f)
	framework.ExpectNoError(err, "allocate overlay subnets")
	// Register before the deletes so this runs last. A parallel spec can
	// reuse these subnets as soon as the allocation is released.
	ginkgo.DeferCleanup(func(ctx ginkgo.SpecContext) {
		gomega.Eventually(ctx, func(g gomega.Gomega) {
			// A create can reach the API even if setup timed out before it
			// registered its delete. Both names belong to this test namespace.
			err := ra.K8sV1().RouteAdvertisements().Delete(ctx, name, metav1.DeleteOptions{})
			g.Expect(err).To(gomega.MatchError(apierrors.IsNotFound, "NotFound"), "wait for RA %s to disappear", name)
			err = f.DynamicClient.Resource(clusterUDNGVR).Delete(ctx, name, metav1.DeleteOptions{})
			g.Expect(err).To(gomega.MatchError(apierrors.IsNotFound, "NotFound"), "wait for CUDN %s to disappear", name)
			_, err = nads.NetworkAttachmentDefinitions(name).Get(ctx, name, metav1.GetOptions{})
			g.Expect(err).To(gomega.MatchError(apierrors.IsNotFound, "NotFound"), "wait for NAD %s/%s to disappear", name, name)
			exports, err := frr.ApiV1beta1().FRRConfigurations(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: types.OvnRouteAdvertisementsKey + "=" + name})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "list exports for RA %s", name)
			g.Expect(exports.Items).To(gomega.BeEmpty(), "withdraw all exports for RA %s", name)
		}, 2*time.Minute, time.Second).Should(gomega.Succeed(), "remove users of the BGP subnets before releasing them")
		framework.ExpectNoError(release(ctx), "release overlay subnets")
	}, ginkgo.NodeTimeout(3*time.Minute))
	ictx := infraprovider.Get().NewTestContext()
	families := sets.New[utilnet.IPFamily]()
	for _, node := range nodes {
		for _, ip := range e2enode.GetAddresses(&node, corev1.NodeInternalIP) {
			families.Insert(utilnet.IPFamilyOfString(ip))
		}
	}
	network := &udnv1.NetworkSpec{
		Topology: udnv1.NetworkTopologyLayer3,
		Layer3: &udnv1.Layer3Config{
			Role: udnv1.NetworkRolePrimary,
			Subnets: matchL3SubnetsByIPFamilies(families,
				udnv1.Layer3Subnet{CIDR: udnv1.CIDR(allocation.UDNSubnet), HostSubnet: 24},
				udnv1.Layer3Subnet{CIDR: udnv1.CIDR(allocation.UDNSubnet6), HostSubnet: 64}),
		},
	}
	framework.ExpectNoError(createUserDefinedNetwork(ctx, f, ictx, f.Namespace, name, true, network, selector), "create overlay CUDN")
	ginkgo.DeferCleanup(func(ctx ginkgo.SpecContext) error {
		return f.ClientSet.CoreV1().Pods(f.Namespace.Name).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{})
	}, ginkgo.NodeTimeout(time.Minute))
	// A pod on each node keeps the CUDN active on both during withdrawal.
	pods := e2epod.NewPodClient(f)
	clientPod := pods.CreateSync(ctx, newAgnhostPodOnNode("client-0", nodes[0].Name, nil, "sleep", "infinity"))
	pods.CreateSync(ctx, newAgnhostPodOnNode("client-1", nodes[1].Name, nil, "sleep", "infinity"))
	framework.ExpectNoError(createRouteAdvertisements(ctx, f, ictx, name, "", selector, map[string]string{"name": "receive-all"}), "advertise %s through the external peer", name)

	// A server on the nodes' own subnet stays reachable after the UDN loses
	// its imported routes. Send replies to pod addresses through the BGP peer.
	primary, err := infraprovider.Get().PrimaryNetwork()
	framework.ExpectNoError(err, "get node infrastructure network")
	server, err := ictx.CreateExternalContainer(infraapi.ExternalContainer{Name: name, Image: images.AgnHost(), Network: primary,
		ExtPort: netexecPort, CmdArgs: getAgnHostHTTPPortBindCMDArgs(netexecPort)})
	framework.ExpectNoError(err, "start external source-IP server")
	type probe struct{ serverIP, podIP, nodeIP string }
	var probes []probe
	attachment, err := getPodAnnotationForAttachment(clientPod, namespacedName(name, name))
	framework.ExpectNoError(err, "read the client pod's overlay addresses")
	for _, family := range sets.List(families) {
		podCIDR := getFirstCIDROfFamily(family, attachment.IPs)
		gomega.Expect(podCIDR).NotTo(gomega.BeNil(), "client pod must have a %s address", family)
		podIP := podCIDR.IP.String()
		serverIP := getFirstIPStringOfFamily(family, []string{server.IPv4, server.IPv6})
		nodeIP := getFirstIPStringOfFamily(family, e2enode.GetAddresses(&nodes[0], corev1.NodeInternalIP))
		gomega.Expect(serverIP).NotTo(gomega.BeEmpty(), "server must have a %s address", family)
		gomega.Expect(nodeIP).NotTo(gomega.BeEmpty(), "node %s must have a %s address", nodes[0].Name, family)
		setSVCRouteOnContainer(server, podIP, routerIPv4, routerIPv6)
		probes = append(probes, probe{serverIP: serverIP, podIP: podIP, nodeIP: nodeIP})
	}
	gomega.Expect(probes).NotTo(gomega.BeEmpty(), "probe at least one address family")
	expectSource := func(snat bool) {
		ginkgo.GinkgoHelper()
		gomega.Eventually(ctx, func(g gomega.Gomega) {
			attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			for _, p := range probes {
				want := p.podIP
				if snat {
					want = p.nodeIP
				}
				url := "http://" + net.JoinHostPort(p.serverIP, server.GetPortStr()) + "/clientip"
				out, stderr, err := e2epod.Exec(f.TContext(attempt), e2epod.ExecOptions{
					Namespace: name, PodName: clientPod.Name, ContainerName: clientPod.Spec.Containers[0].Name,
					Command: []string{"curl", "-fsS", "--max-time", "2", url}, CaptureStdout: true, CaptureStderr: true,
				})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "probe %s from %s: %s", url, clientPod.Name, stderr)
				source, _, err := net.SplitHostPort(out)
				g.Expect(err).NotTo(gomega.HaveOccurred(), "parse client address %q from %s", out, url)
				g.Expect(source).To(gomega.Equal(want), "source seen by %s", p.serverIP)
			}
		}, 90*time.Second, time.Second).Should(gomega.Succeed(), "verify the source IP for every supported family")
	}

	ginkgo.By("observing the pod IP at the external server before withdrawal")
	expectSource(false)
	degradedMessage := func(g gomega.Gomega) string {
		ginkgo.GinkgoHelper()
		current, err := ra.K8sV1().RouteAdvertisements().Get(ctx, name, metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred(), "get RA %s", name)
		g.Expect(meta.IsStatusConditionTrue(current.Status.Conditions, "Accepted")).To(gomega.BeTrue(), "node failures must leave the RA accepted")
		degraded := meta.FindStatusCondition(current.Status.Conditions, "Degraded")
		g.Expect(degraded).NotTo(gomega.BeNil(), "RA must report degradation")
		g.Expect(degraded.ObservedGeneration).To(gomega.Equal(current.Generation), "Degraded must reflect the current RA generation")
		g.Expect(degraded.Status).To(gomega.Equal(metav1.ConditionTrue), "the other node still fails")
		g.Expect(degraded.Message).To(gomega.ContainSubstring(deniedExportMessage), "report the injected admission failure")
		return degraded.Message
	}

	ginkgo.By("withdrawing only this RA's exports on the two nodes")
	blockedNodes := []string{util.RouteAdvertisementsNodeLabel(nodes[0].Name), util.RouteAdvertisementsNodeLabel(nodes[1].Name)}
	blocked := metav1.LabelSelector{MatchLabels: map[string]string{types.OvnRouteAdvertisementsKey: name}, MatchExpressions: []metav1.LabelSelectorRequirement{{
		Key: types.OvnRouteAdvertisementsNodeKey, Operator: metav1.LabelSelectorOpIn, Values: blockedNodes,
	}}}
	listOpts := metav1.ListOptions{LabelSelector: metav1.FormatLabelSelector(&blocked)}
	binding := denyTestFRRCreates(ctx, f, name, blocked)
	dryRun := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	var clientExport *frrapi.FRRConfiguration
	gomega.Eventually(ctx, func(g gomega.Gomega) {
		// Wait for admission to enforce the binding before deleting either
		// export. Otherwise the controller could recreate it immediately.
		current, err := configs.List(ctx, listOpts)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "list the initial exports")
		g.Expect(current.Items).To(gomega.HaveLen(2), "each node must have an export from the selected peering template")
		for _, cfg := range current.Items {
			cfg.ObjectMeta = metav1.ObjectMeta{GenerateName: cfg.GenerateName, Labels: cfg.Labels}
			nodeLabel := cfg.Labels[types.OvnRouteAdvertisementsNodeKey]
			_, err := configs.Create(ctx, &cfg, dryRun)
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(deniedExportMessage)), "block recreation on %s", nodeLabel)
			if nodeLabel == blockedNodes[0] {
				clientExport = &cfg
			}
		}
		g.Expect(clientExport).NotTo(gomega.BeNil(), "find the client node's export before withdrawal")
	}, time.Minute, time.Second).Should(gomega.Succeed(), "activate the scoped admission policy")
	framework.ExpectNoError(configs.DeleteCollection(ctx, metav1.DeleteOptions{}, listOpts), "withdraw both test exports")
	gomega.Eventually(ctx, func(g gomega.Gomega) {
		current, err := configs.List(ctx, listOpts)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "list withdrawn exports")
		g.Expect(current.Items).To(gomega.BeEmpty(), "both test exports must be withdrawn")
		message := degradedMessage(g)
		for _, node := range nodes {
			g.Expect(message).To(gomega.ContainSubstring("/"+node.Name+":"), "recreation must fail on %s", node.Name)
		}
	}, 90*time.Second, time.Second).Should(gomega.Succeed(), "observe both node failures")
	expectSource(true)

	ginkgo.By("allowing the client node to recover while the other node still fails")
	binding.Spec.MatchResources.ObjectSelector = &metav1.LabelSelector{MatchLabels: map[string]string{types.OvnRouteAdvertisementsNodeKey: blockedNodes[1]}}
	_, err = f.ClientSet.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Update(ctx, binding, metav1.UpdateOptions{})
	framework.ExpectNoError(err, "allow creation of the client node's export")
	gomega.Eventually(ctx, func() error {
		_, err := configs.Create(ctx, clientExport, dryRun)
		return err
	}, time.Minute, time.Second).Should(gomega.Succeed(), "wait for admission to allow the client node's export")

	// The SNAT probes can outlast the RA's retries, and admission binding
	// changes don't wake the controller. An equivalent selector queues this
	// RA again while keeping the same peering template.
	patch := []byte(`{"spec":{"frrConfigurationSelector":{
		"matchLabels":null,"matchExpressions":[{"key":"name","operator":"In","values":["receive-all"]}]
	}}}`)
	_, err = ra.K8sV1().RouteAdvertisements().Patch(ctx, name, ktypes.MergePatchType, patch, metav1.PatchOptions{})
	framework.ExpectNoError(err, "requeue RA %s after admission allows the client export", name)
	gomega.Eventually(ctx, func(g gomega.Gomega) {
		current, err := configs.List(ctx, listOpts)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "list exports after recovery")
		g.Expect(current.Items).To(gomega.HaveLen(1), "only the client node should recover")
		g.Expect(current.Items[0].Labels[types.OvnRouteAdvertisementsNodeKey]).To(gomega.Equal(blockedNodes[0]), "client node must have its export back")
		message := degradedMessage(g)
		g.Expect(message).To(gomega.ContainSubstring("/"+nodes[1].Name+":"), "the second node still fails")
		g.Expect(message).NotTo(gomega.ContainSubstring("/"+nodes[0].Name+":"), "the client node recovered")
	}, 2*time.Minute, time.Second).Should(gomega.Succeed(), "restore one export while the RA remains degraded")
	expectSource(false)
}

const deniedExportMessage = "test export temporarily blocked"

// The policy selects this RA and its binding selects the blocked nodes.
// Other RAs can keep using the shared peering template throughout the test.
func denyTestFRRCreates(ctx context.Context, f *framework.Framework, name string, selector metav1.LabelSelector) *admissionv1.ValidatingAdmissionPolicyBinding {
	admission := f.ClientSet.AdmissionregistrationV1()
	policy := &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: admissionv1.ValidatingAdmissionPolicySpec{
		MatchConstraints: &admissionv1.MatchResources{
			ObjectSelector: &metav1.LabelSelector{MatchLabels: selector.MatchLabels},
			ResourceRules: []admissionv1.NamedRuleWithOperations{{RuleWithOperations: admissionv1.RuleWithOperations{
				Operations: []admissionv1.OperationType{admissionv1.Create},
				Rule:       admissionv1.Rule{APIGroups: []string{frrapi.GroupVersion.Group}, APIVersions: []string{frrapi.GroupVersion.Version}, Resources: []string{"frrconfigurations"}},
			}}},
		},
		Validations: []admissionv1.Validation{{Expression: "false", Message: deniedExportMessage}},
	}}
	_, err := admission.ValidatingAdmissionPolicies().Create(ctx, policy, metav1.CreateOptions{})
	framework.ExpectNoError(err, "create policy for RA %s", name)
	ginkgo.DeferCleanup(func(ctx ginkgo.SpecContext) error {
		return admission.ValidatingAdmissionPolicies().Delete(ctx, name, metav1.DeleteOptions{})
	}, ginkgo.NodeTimeout(time.Minute))
	binding, err := admission.ValidatingAdmissionPolicyBindings().Create(ctx, &admissionv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName: name, ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny},
			MatchResources: &admissionv1.MatchResources{ObjectSelector: &selector},
		},
	}, metav1.CreateOptions{})
	framework.ExpectNoError(err, "bind policy to the test nodes")
	ginkgo.DeferCleanup(func(ctx ginkgo.SpecContext) error {
		return admission.ValidatingAdmissionPolicyBindings().Delete(ctx, name, metav1.DeleteOptions{})
	}, ginkgo.NodeTimeout(time.Minute))
	return binding
}
