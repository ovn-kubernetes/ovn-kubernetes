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
//   - an unconditional gateway-router SNAT for the advertised subnet.
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

	ginkgo.It("programs every node before accepting traffic, without restarting ovnkube-node", func(ctx ginkgo.SpecContext) {
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

		ginkgo.By("checking both advertised-subnet OpenFlow rules on every node")
		bridgeName := deploymentconfig.Get().ExternalBridgeName()
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

			gomega.Eventually(func() error {
				flows, err := e2epodoutput.RunHostCmdWithRetries(
					ovnkPod.Namespace,
					ovnkPod.Name,
					fmt.Sprintf("ovs-ofctl dump-flows %s", bridgeName),
					2*time.Second,
					5*time.Second,
				)
				if err != nil {
					return err
				}
				for _, subnet := range advertisedSubnets {
					for _, priority := range []string{"300", "104"} {
						if !hasAdvertisedRouteFlow(flows, priority, subnet) {
							return fmt.Errorf("node %s is missing priority=%s output flow for %s on %s; flows:\n%s",
								nodeName, priority, subnet, bridgeName, flows)
						}
					}
				}
				return nil
			}, 2*time.Minute, 2*time.Second).Should(gomega.Succeed())
		}

		ginkgo.By("checking the Layer2 switch-to-transit-router port")
		networkName := ovntypes.CUDNPrefix + cUDN.Name
		switchName := ovnutil.GetNetworkScopedNameForNetwork(networkName, ovntypes.OVNLayer2Switch)
		stotrPortName := ovntypes.SwitchToTransitRouterPrefix + switchName
		gomega.Eventually(func() error {
			portName, err := runOVNNBCTL(
				f,
				f.ClientSet,
				"--data=bare",
				"--no-heading",
				"--columns=name",
				"find",
				"Logical_Switch_Port",
				"name="+stotrPortName,
			)
			if err != nil {
				return err
			}
			if strings.TrimSpace(portName) != stotrPortName {
				return fmt.Errorf("switch-to-transit-router port %s is missing; ovn-nbctl returned %q", stotrPortName, portName)
			}
			return nil
		}, 2*time.Minute, 2*time.Second).Should(gomega.Succeed())

		ginkgo.By("checking that every gateway router has advertisement-aware SNAT")
		for _, node := range nodes.Items {
			nodeName := node.Name
			routerName := cudnGatewayRouterName(cUDN.Name, nodeName)
			gomega.Eventually(func() error {
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
					return fmt.Errorf("failed to list NATs for gateway router %s in node %s's OVN database: %w, stderr: %s",
						routerName, nodeName, err, stderr)
				}
				for _, subnet := range advertisedSubnets {
					if !hasConditionalSNAT(nats, subnet) {
						return fmt.Errorf("gateway router %s has no conditional SNAT for %s; NATs:\n%s", routerName, subnet, nats)
					}
				}
				return nil
			}, 2*time.Minute, 2*time.Second).Should(gomega.Succeed())
		}

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

func hasAdvertisedRouteFlow(flows, priority, subnet string) bool {
	for _, flow := range strings.Split(flows, "\n") {
		if strings.Contains(flow, "priority="+priority) &&
			strings.Contains(flow, subnet) &&
			strings.Contains(flow, "actions=output:") {
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
