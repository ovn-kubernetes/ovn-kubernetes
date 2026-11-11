// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	rav1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	ovnutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	utilnet "k8s.io/utils/net"
)

// This test is the upstream OVN-Kubernetes form of the reproducer from:
// https://gist.github.com/frobware/3561697d5f602cdeee4767a7d86ccbd4
//
// The RouteAdvertisements is deliberately created first and the Layer2
// primary CUDN five seconds later. The unfixed code can then leave a node with:
//   - no priority=300/104 advertised-subnet flows on the external bridge;
//   - no switch-to-transit-router logical port; or
//   - an unconditional gateway- or transit-router SNAT for the advertised subnet.
//
// In addition to checking those three signatures directly, the test sends
// traffic from the upstream KIND external BGP test server, through the FRR
// router, to one pod on every node. It uses only OVN-Kubernetes test
// infrastructure; bgp-cloud-connector and OpenShift resources are intentionally
// not dependencies of this upstream regression.
var _ = ginkgo.Describe("BGP: advertised Layer2 CUDN creation race", feature.RouteAdvertisements, func() {
	const (
		cudnLabel       = "bgp-recreate-race"
		raName          = "bgp-recreate-race-ra"
		subnetIPv4      = "108.108.0.0/16"
		subnetIPv6      = "2001:db8:108:108::/64"
		raceNetexecPort = "8080"
	)

	f := wrappedTestFramework("route-advertisements-recreate-race")
	f.SkipNamespaceCreation = true

	ginkgo.It("programs every node before accepting traffic, without restarting ovnkube-node", ginkgo.MustPassRepeatedly(10), func(ctx ginkgo.SpecContext) {
		namespace, err := f.CreateNamespace(ctx, f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		f.Namespace = namespace

		nodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, f.ClientSet, 3)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(nodes.Items).To(gomega.HaveLen(3), "the original reproducer requires three schedulable nodes")

		udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		raClient, err := raclientset.NewForConfig(f.ClientConfig())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		subnetsByFamily := map[utilnet.IPFamily]string{
			utilnet.IPv4: subnetIPv4,
			utilnet.IPv6: subnetIPv6,
		}
		var udnSubnets udnv1.DualStackCIDRs
		var advertisedSubnets []string
		supportedIPFamilies := getSupportedIPFamiliesSlice(f.ClientSet)
		for _, family := range supportedIPFamilies {
			subnet := subnetsByFamily[family]
			udnSubnets = append(udnSubnets, udnv1.CIDR(subnet))
			advertisedSubnets = append(advertisedSubnets, subnet)
		}
		gomega.Expect(advertisedSubnets).NotTo(gomega.BeEmpty(), "cluster reports no supported IP families")

		ginkgo.By("creating RouteAdvertisements before the selected network exists")
		ra := &rav1.RouteAdvertisements{
			ObjectMeta: metav1.ObjectMeta{Name: raName},
			Spec: rav1.RouteAdvertisementsSpec{
				NetworkSelectors: apitypes.NetworkSelectors{
					apitypes.NetworkSelector{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{MatchLabels: map[string]string{cudnLabel: ""}},
						},
					},
				},
				NodeSelector:             metav1.LabelSelector{},
				FRRConfigurationSelector: metav1.LabelSelector{},
				Advertisements:           []rav1.AdvertisementType{rav1.PodNetwork},
			},
		}
		_, err = raClient.K8sV1().RouteAdvertisements().Create(ctx, ra, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func(cleanupCtx context.Context) error {
			err := raClient.K8sV1().RouteAdvertisements().Delete(cleanupCtx, raName, metav1.DeleteOptions{})
			if err == nil || apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("delete RouteAdvertisements %s: %w", raName, err)
		})

		ginkgo.By("waiting five seconds before creating the selected CUDN, matching the reproducer")
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			ginkgo.Fail(fmt.Sprintf("context ended while waiting to create the CUDN: %v", ctx.Err()))
		}

		ginkgo.By("creating the Layer2 primary CUDN")
		cudn := &udnv1.ClusterUserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "bgr-",
				Labels:       map[string]string{cudnLabel: ""},
			},
			Spec: udnv1.ClusterUserDefinedNetworkSpec{
				NamespaceSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "kubernetes.io/metadata.name",
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{f.Namespace.Name},
				}}},
				Network: udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer2,
					Layer2: &udnv1.Layer2Config{
						Role:    "Primary",
						IPAM:    &udnv1.IPAMConfig{Lifecycle: udnv1.IPAMLifecyclePersistent},
						Subnets: udnSubnets,
					},
				},
			},
		}
		cUDN, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Create(ctx, cudn, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func(cleanupCtx context.Context) error {
			err := udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(cleanupCtx, cUDN.Name, metav1.DeleteOptions{})
			if err == nil || apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("delete ClusterUserDefinedNetwork %s: %w", cUDN.Name, err)
		})

		ginkgo.By("waiting for the CUDN and RouteAdvertisements API conditions")
		gomega.Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cUDN.Name), 2*time.Minute, time.Second).
			Should(gomega.Succeed(), "CUDN %s did not become ready", cUDN.Name)
		gomega.Eventually(func() error {
			current, err := raClient.K8sV1().RouteAdvertisements().Get(ctx, raName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			condition := meta.FindStatusCondition(current.Status.Conditions, "Accepted")
			if condition == nil {
				return fmt.Errorf("RouteAdvertisements %s has no Accepted condition", raName)
			}
			if condition.Status != metav1.ConditionTrue || condition.Reason != "Accepted" {
				return fmt.Errorf("RouteAdvertisements %s Accepted condition is %+v", raName, *condition)
			}
			return nil
		}, 2*time.Minute, time.Second).Should(gomega.Succeed())

		ginkgo.By("creating a headless Service and one netexec pod per node to trigger endpoint and flow-cache updates")
		_, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(ctx, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "churn-headless"},
			Spec: corev1.ServiceSpec{
				ClusterIP: corev1.ClusterIPNone,
				Selector:  map[string]string{"app": "bgp-recreate-race"},
				Ports:     []corev1.ServicePort{{Name: "http", Port: 8080}},
			},
		}, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		pods := make([]*corev1.Pod, 0, len(nodes.Items))
		for i, node := range nodes.Items {
			pod, err := createPod(
				f,
				fmt.Sprintf("bgp-race-%d", i),
				node.Name,
				f.Namespace.Name,
				[]string{"/agnhost", "netexec", "--http-port=" + raceNetexecPort},
				map[string]string{"app": "bgp-recreate-race"},
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to create a test pod on node %s", node.Name)
			pods = append(pods, pod)
		}
		ginkgo.DeferCleanup(func(cleanupCtx context.Context) error {
			err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).DeleteCollection(cleanupCtx, metav1.DeleteOptions{}, metav1.ListOptions{
				LabelSelector: "app=bgp-recreate-race",
			})
			if err == nil || apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("delete BGP recreation test pods: %w", err)
		})

		ginkgo.By("waiting for the bridge flows, Layer2 topology, and router SNATs to converge")
		bridgeName := deploymentconfig.Get().ExternalBridgeName()
		localGateway := IsGatewayModeLocal(f.ClientSet)
		ovnkNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
		ovnDBPodsByNode := make(map[string]string, len(nodes.Items))
		for _, node := range nodes.Items {
			nodeName := node.Name
			ovnkPods, err := f.ClientSet.CoreV1().Pods(ovnkNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app=ovnkube-node",
				FieldSelector: "spec.nodeName=" + nodeName,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(ovnkPods.Items).To(gomega.HaveLen(1), "expected one ovnkube-node pod on %s", nodeName)
			ovnkPod := ovnkPods.Items[0]
			ovnDBPodsByNode[nodeName] = ovnkPod.Name
		}

		networkName := ovntypes.CUDNPrefix + cUDN.Name
		switchName := ovnutil.GetNetworkScopedNameForNetwork(networkName, ovntypes.OVNLayer2Switch)
		stotrPortName := ovntypes.SwitchToTransitRouterPrefix + switchName
		transitRouterName := ovnutil.GetNetworkScopedNameForNetwork(networkName, ovntypes.TransitRouter)
		gomega.Eventually(func() []string {
			var diagnostics []string

			// Inspect every signature in one poll. Do not stop at the first
			// missing bridge flow: one unfixed attempt may contain the missing
			// flow, missing topology, and stale SNAT failures together.
			for _, node := range nodes.Items {
				nodeName := node.Name
				flows, err := e2epodoutput.RunHostCmdWithRetries(
					ovnkNamespace,
					ovnDBPodsByNode[nodeName],
					fmt.Sprintf("ovs-ofctl dump-flows %s", bridgeName),
					2*time.Second,
					5*time.Second,
				)
				if err != nil {
					diagnostics = append(diagnostics, fmt.Sprintf(
						"INSPECTION_ERROR bridge-flows node=%s: %v", nodeName, err))
					continue
				}
				for _, subnet := range advertisedSubnets {
					for _, priority := range []string{"300", "104"} {
						if !hasAdvertisedRouteFlow(flows, priority, subnet, localGateway) {
							diagnostics = append(diagnostics, fmt.Sprintf(
								"BUG1_MISSING_BRIDGE_FLOW node=%s priority=%s subnet=%s bridge=%s",
								nodeName, priority, subnet, bridgeName))
						}
					}
				}
			}

			// Every KIND node is its own interconnect zone with a separate
			// northbound database. Check the switch-to-transit-router port in
			// every zone; querying an arbitrary database can hide a worker-local
			// first-Add failure when another zone created its copy of the port.
			for _, node := range nodes.Items {
				nodeName := node.Name
				portName, stderr, err := ExecCommandInContainerWithFullOutput(
					f,
					ovnkNamespace,
					ovnDBPodsByNode[nodeName],
					"nb-ovsdb",
					"ovn-nbctl",
					"--data=bare",
					"--no-heading",
					"--columns=name",
					"find",
					"Logical_Switch_Port",
					"name="+stotrPortName,
				)
				if err != nil {
					diagnostics = append(diagnostics, fmt.Sprintf(
						"INSPECTION_ERROR stotr-port node=%s name=%s: %v, stderr=%s",
						nodeName, stotrPortName, err, stderr))
				} else if strings.TrimSpace(portName) != stotrPortName {
					diagnostics = append(diagnostics, fmt.Sprintf(
						"BUG2_MISSING_STOTR_PORT node=%s name=%s returned=%q",
						nodeName, stotrPortName, strings.TrimSpace(portName)))
				}
			}

			for _, node := range nodes.Items {
				nodeName := node.Name
				routerName := cudnGatewayRouterName(cUDN.Name, nodeName)
				// In interconnect deployments each zone has its own northbound
				// database. Query the database on this node: an arbitrary zone's
				// database does not contain this node's gateway router.
				nats, stderr, err := ExecCommandInContainerWithFullOutput(
					f,
					ovnkNamespace,
					ovnDBPodsByNode[nodeName],
					"nb-ovsdb",
					"ovn-nbctl",
					"lr-nat-list",
					routerName,
				)
				if err != nil {
					diagnostics = append(diagnostics, fmt.Sprintf(
						"INSPECTION_ERROR gateway-nat router=%s node=%s: %v, stderr=%s",
						routerName, nodeName, err, stderr))
					continue
				}
				for _, subnet := range advertisedSubnets {
					if !hasConditionalSNAT(nats, subnet) {
						diagnostics = append(diagnostics, fmt.Sprintf(
							"BUG3_STALE_SNAT router=%s node=%s subnet=%s nats=%q",
							routerName, nodeName, subnet, strings.TrimSpace(nats)))
					}
				}

				// Gateway synchronization also writes a per-node subnet SNAT to
				// this zone's transit router. A healthy local gateway router can
				// therefore hide stale transit-router state unless both routers
				// are checked separately.
				nats, stderr, err = ExecCommandInContainerWithFullOutput(
					f,
					ovnkNamespace,
					ovnDBPodsByNode[nodeName],
					"nb-ovsdb",
					"ovn-nbctl",
					"lr-nat-list",
					transitRouterName,
				)
				if err != nil {
					diagnostics = append(diagnostics, fmt.Sprintf(
						"INSPECTION_ERROR transit-router-nat router=%s node=%s: %v, stderr=%s",
						transitRouterName, nodeName, err, stderr))
					continue
				}
				for _, subnet := range advertisedSubnets {
					if !hasConditionalSNAT(nats, subnet) {
						diagnostics = append(diagnostics, fmt.Sprintf(
							"BUG3_STALE_TRANSIT_ROUTER_SNAT router=%s node=%s subnet=%s nats=%q",
							transitRouterName, nodeName, subnet, strings.TrimSpace(nats)))
					}
				}
			}

			return diagnostics
		}, 2*time.Minute, 2*time.Second).Should(gomega.BeEmpty(),
			"the advertised Layer2 CUDN dataplane did not converge")

		ginkgo.By("checking that the external KIND FRR router learns every node path")
		for _, node := range nodes.Items {
			for _, subnet := range advertisedSubnets {
				checkRouteInFRR(node, subnet, routerContainerName, utilnet.IsIPv6CIDRString(subnet))
			}
		}

		ginkgo.By("checking external-to-pod traffic for one pod on every node")
		// The agnhost server sits behind FRR and provides curl. The FRR image is
		// the intermediary router only and intentionally has no HTTP client.
		externalClient := infraapi.ExternalContainer{Name: bgpExternalServerContainerName}
		for _, pod := range pods {
			for _, family := range supportedIPFamilies {
				var podIP string
				gomega.Eventually(func() error {
					var err error
					podIP, err = getPodAnnotationIPsForPrimaryNetworkByIPFamily(
						f.ClientSet,
						pod.Namespace,
						pod.Name,
						cUDN.Name,
						family,
					)
					if err != nil {
						return err
					}
					if podIP == "" {
						return fmt.Errorf("pod has no %s address for primary CUDN %s", family, cUDN.Name)
					}
					return nil
				}, 30*time.Second, time.Second).Should(gomega.Succeed(),
					"could not find pod %s/%s's %s address for primary CUDN %s", pod.Namespace, pod.Name, family, cUDN.Name)

				gomega.Eventually(func() error {
					output, err := infraprovider.Get().ExecExternalContainerCommand(externalClient, []string{
						"curl", "--max-time", "2", "-g", "-q", "-s",
						fmt.Sprintf("http://%s/hostname", net.JoinHostPort(podIP, raceNetexecPort)),
					})
					if err != nil {
						return err
					}
					if got := strings.TrimSpace(output); got != pod.Name {
						return fmt.Errorf("external client reached pod IP %s but got hostname %q instead of %q", podIP, got, pod.Name)
					}
					return nil
				}, 2*time.Minute, 2*time.Second).Should(gomega.Succeed(),
					"external client could not reach pod %s/%s at %s", pod.Namespace, pod.Name, podIP)
			}
		}
	})
})

func hasAdvertisedRouteFlow(flows, priority, subnet string, localGateway bool) bool {
	expectedAction := "actions=output:"
	if priority == "300" && localGateway {
		// In local gateway mode ingress traffic enters the host network stack
		// instead of the OVN patch port. ovs-ofctl renders output:LOCAL as LOCAL.
		expectedAction = "actions=LOCAL"
	}
	for _, flow := range strings.Split(flows, "\n") {
		if strings.Contains(flow, "priority="+priority) &&
			strings.Contains(flow, subnet) &&
			strings.Contains(flow, expectedAction) {
			return true
		}
	}
	return false
}

func hasConditionalSNAT(nats, subnet string) bool {
	for _, nat := range strings.Split(nats, "\n") {
		if strings.Contains(nat, "snat") &&
			strings.Contains(nat, subnet) &&
			strings.Contains(nat, "==") {
			return true
		}
	}
	return false
}
