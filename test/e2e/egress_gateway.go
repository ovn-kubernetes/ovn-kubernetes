// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/gcustom"
	"github.com/onsi/gomega/types"
	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/typed/egressip/v1"
	egressservicev1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	egressserviceclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/typed/egressservice/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
)

const (
	egressGatewayLabel      = "k8s.ovn.org/egress-gateway"
	egressAssignableLabel   = "k8s.ovn.org/egress-assignable"
	egressGatewayHealthPort = "9108"
)

// Node labels are shared cluster state.
var _ = ginkgo.Describe("EgressGateway", ginkgo.Label("Feature:EgressGateway"), ginkgo.Serial, func() {
	f := newPrivelegedTestFramework("egress-gateway")
	var (
		nodes        *v1.NodeList
		gatewayNodes []v1.Node
		families     []v1.IPFamily
		endpoints    []string
		egClient     *appsv1.DaemonSet
	)

	ginkgo.BeforeEach(func(ctx context.Context) {
		tCtx := f.TContext(ctx)
		var err error
		nodes, err = f.ClientSet.CoreV1().Nodes().List(tCtx, metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">=", 3), "requires at least three nodes")
		slices.SortFunc(nodes.Items, func(a, b v1.Node) int { return strings.Compare(a.Name, b.Name) })
		gatewayNodes = nodes.Items[1:3]
		switch {
		case isDualStackCluster(nodes):
			families = []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol}
		case IsIPv6Cluster(f.ClientSet):
			families = []v1.IPFamily{v1.IPv6Protocol}
		default:
			families = []v1.IPFamily{v1.IPv4Protocol}
		}
		if endpoint := os.Getenv("EGRESS_GATEWAY_ENDPOINT"); endpoint != "" {
			endpoints = []string{endpoint}
		} else {
			endpoints = setupIPInfoServerInKind(families)
		}
		ginkgo.By("Creating gateway client pods on every node")
		egClient = deployClientDS(tCtx, f, "gateway-clients", len(nodes.Items))
	})

	ginkgo.Context("lifecycle", func() {
		ginkgo.It("routes every pod through the full gateway lifecycle", ginkgo.SpecTimeout(12*time.Minute), func(ctx context.Context) {
			tCtx := f.TContext(ctx)
			gateway1, gateway2 := gatewayNodes[0], gatewayNodes[1]
			getNodeIPs := func(node v1.Node) []string {
				var ips []string
				for _, family := range families {
					addresses := e2enode.GetAddressesByTypeAndFamily(&node, v1.NodeInternalIP, family)
					gomega.Expect(addresses).NotTo(gomega.BeEmpty(), "node %s needs an %s InternalIP", node.Name, family)
					ips = append(ips, net.ParseIP(addresses[0]).String())
				}
				return ips
			}
			var labeledGateways []string
			withGatewayDiagnostics := func(matcher types.GomegaMatcher) types.GomegaMatcher {
				return gcustom.MakeMatcher(matcher.Match).WithTemplate(
					"Nodes labeled "+egressGatewayLabel+" by this test: {{.Data}}\nObserved IPs at IP-info server: {{.Actual}}", labeledGateways)
			}

			ginkgo.By("With no labeled gateways, every client uses its own node IP")
			var allNodeIPs []string
			var ownNodeMatchers []types.GomegaMatcher
			for _, node := range nodes.Items {
				nodeIPs := getNodeIPs(node)
				allNodeIPs = append(allNodeIPs, nodeIPs...)
				ownNodeMatchers = append(ownNodeMatchers, gomega.ContainElement(gomega.BeElementOf(nodeIPs)))
			}
			// A DNS endpoint may choose either family; every node must still appear.
			ownNodeMatchers = append(ownNodeMatchers,
				gomega.HaveEach(gomega.BeElementOf(allNodeIPs)),
				gomega.HaveLen(len(nodes.Items)*len(endpoints)))
			ownNodeSources := gomega.And(ownNodeMatchers...)
			gomega.Eventually(tCtx, fetchProbeSourceIPs, 19*time.Second, time.Second).
				WithArguments(tCtx, egClient, endpoints).Should(withGatewayDiagnostics(ownNodeSources))
			gomega.Consistently(tCtx, fetchProbeSourceIPs, 5*time.Second, time.Second).
				WithArguments(tCtx, egClient, endpoints).Should(withGatewayDiagnostics(ownNodeSources))

			ginkgo.By("Labeling gateway 1 and routing every client through it")
			defer e2enode.RemoveLabelOffNode(f.ClientSet, gateway1.Name, egressGatewayLabel)
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, gateway1.Name, egressGatewayLabel, "true")
			for _, ip := range getNodeIPs(gateway1) {
				labeledGateways = append(labeledGateways, gateway1.Name+"/"+ip)
			}
			want := getNodeIPs(gateway1)
			gomega.Eventually(tCtx, fetchProbeSourceIPs, 19*time.Second, time.Second).
				WithArguments(tCtx, egClient, endpoints).
				Should(withGatewayDiagnostics(gomega.HaveEach(gomega.BeElementOf(want))))
			gomega.Consistently(tCtx, fetchProbeSourceIPs, 5*time.Second, time.Second).
				WithArguments(tCtx, egClient, endpoints).
				Should(withGatewayDiagnostics(gomega.HaveEach(gomega.BeElementOf(want))))

			ginkgo.By("Labeling gateway 2 and accepting either gateway IP")
			defer e2enode.RemoveLabelOffNode(f.ClientSet, gateway2.Name, egressGatewayLabel)
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, gateway2.Name, egressGatewayLabel, "true")
			for _, ip := range getNodeIPs(gateway2) {
				labeledGateways = append(labeledGateways, gateway2.Name+"/"+ip)
			}
			want = append(getNodeIPs(gateway1), getNodeIPs(gateway2)...)
			gomega.Eventually(tCtx, fetchProbeSourceIPs, 19*time.Second, time.Second).
				WithArguments(tCtx, egClient, endpoints).Should(withGatewayDiagnostics(gomega.HaveEach(gomega.BeElementOf(want))))
			gomega.Consistently(tCtx, fetchProbeSourceIPs, 5*time.Second, time.Second).
				WithArguments(tCtx, egClient, endpoints).Should(withGatewayDiagnostics(gomega.HaveEach(gomega.BeElementOf(want))))

			ginkgo.By("Blocking gateway 1 health probes and using gateway 2")
			allowOrDropNodeInputTrafficOnPort("Drop", gateway1.Name, "tcp", egressGatewayHealthPort)
			ginkgo.DeferCleanup(func() { allowOrDropNodeInputTrafficOnPort("Allow", gateway1.Name, "tcp", egressGatewayHealthPort) })
			want = getNodeIPs(gateway2)
			gomega.Eventually(tCtx, fetchProbeSourceIPs, 25*time.Second, time.Second).
				WithArguments(tCtx, egClient, endpoints).
				MustPassRepeatedly(10).
				Should(withGatewayDiagnostics(gomega.HaveEach(gomega.BeElementOf(want))))

			ginkgo.By("Blocking gateway 2 health probes and falling back to each pod's node")
			allowOrDropNodeInputTrafficOnPort("Drop", gateway2.Name, "tcp", egressGatewayHealthPort)
			ginkgo.DeferCleanup(func() { allowOrDropNodeInputTrafficOnPort("Allow", gateway2.Name, "tcp", egressGatewayHealthPort) })
			gomega.Eventually(tCtx, fetchProbeSourceIPs, 19*time.Second, time.Second).
				WithArguments(tCtx, egClient, endpoints).Should(withGatewayDiagnostics(ownNodeSources))
			gomega.Consistently(tCtx, fetchProbeSourceIPs, 5*time.Second, time.Second).
				WithArguments(tCtx, egClient, endpoints).Should(withGatewayDiagnostics(ownNodeSources))

			ginkgo.By("Unblocking health probes on both gateways and routing clients through either gateway again")
			allowOrDropNodeInputTrafficOnPort("Allow", gateway1.Name, "tcp", egressGatewayHealthPort)
			allowOrDropNodeInputTrafficOnPort("Allow", gateway2.Name, "tcp", egressGatewayHealthPort)
			want = append(getNodeIPs(gateway1), getNodeIPs(gateway2)...)
			gomega.Eventually(tCtx, fetchProbeSourceIPs, 19*time.Second, time.Second).
				WithArguments(tCtx, egClient, endpoints).Should(withGatewayDiagnostics(gomega.HaveEach(gomega.BeElementOf(want))))
			gomega.Consistently(tCtx, fetchProbeSourceIPs, 5*time.Second, time.Second).
				WithArguments(tCtx, egClient, endpoints).Should(withGatewayDiagnostics(gomega.HaveEach(gomega.BeElementOf(want))))
		})
	})

	ginkgo.Context("coexistence", feature.EgressIP, feature.EgressService, func() {
		var (
			gatewayIPs []string
			eiClient   *appsv1.DaemonSet
			esClient   *appsv1.DaemonSet
			eipIPs     []string
			serviceIPs []string
		)

		ginkgo.BeforeEach(func(ctx context.Context) {
			tCtx := f.TContext(ctx)
			eipNodes := nodes.Items[:2]

			ginkgo.By("Labeling EgressGateway nodes")
			gatewayIPs = nil
			for _, node := range gatewayNodes {
				ginkgo.DeferCleanup(e2enode.RemoveLabelOffNode, f.ClientSet, node.Name, egressGatewayLabel)
				e2enode.AddOrUpdateLabelOnNode(f.ClientSet, node.Name, egressGatewayLabel, "true")
				for _, family := range families {
					addresses := e2enode.GetAddressesByTypeAndFamily(&node, v1.NodeInternalIP, family)
					gomega.Expect(addresses).NotTo(gomega.BeEmpty(), "node %s needs an %s InternalIP", node.Name, family)
					gatewayIPs = append(gatewayIPs, net.ParseIP(addresses[0]).String())
				}
			}
			ginkgo.By("Labeling EgressIP nodes and creating EgressIP CR")
			for _, node := range eipNodes {
				ginkgo.DeferCleanup(e2enode.RemoveLabelOffNode, f.ClientSet, node.Name, egressAssignableLabel)
				e2enode.AddOrUpdateLabelOnNode(f.ClientSet, node.Name, egressAssignableLabel, "true")
			}

			eipIPs = nil
			for _, family := range families {
				var (
					ip  net.IP
					err error
				)
				if family == v1.IPv4Protocol {
					ip, err = allocators.NewPrimaryIPv4(eipNodes[0].Name)
				} else {
					ip, err = allocators.NewPrimaryIPv6(eipNodes[0].Name)
				}
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				eipIPs = append(eipIPs, ip.String())
			}
			config, err := framework.LoadConfig()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			eipClient := egressipclient.NewForConfigOrDie(config).EgressIPs()
			eip, err := eipClient.Create(tCtx, &egressipv1.EgressIP{
				ObjectMeta: metav1.ObjectMeta{Name: f.Namespace.Name},
				Status:     egressipv1.EgressIPStatus{Items: []egressipv1.EgressIPStatusItem{}},
				Spec: egressipv1.EgressIPSpec{
					EgressIPs:         eipIPs,
					NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": f.Namespace.Name}},
					PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "eip-clients"}},
				},
			}, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.DeferCleanup(func(ctx context.Context) error {
				err := eipClient.Delete(ctx, eip.Name, metav1.DeleteOptions{})
				if apierrors.IsNotFound(err) {
					return nil
				}
				return err
			})
			eiClient = deployClientDS(tCtx, f, "eip-clients", len(nodes.Items))

			ginkgo.By("Creating EgressService and LoadBalancer CRs")
			egressServiceClient := egressserviceclient.NewForConfigOrDie(config).EgressServices(f.Namespace.Name)
			var services []*v1.Service
			serviceIPs = nil
			for _, family := range families {
				name := "egress-service-" + strings.ToLower(string(family))
				_, err := egressServiceClient.Create(tCtx, &egressservicev1.EgressService{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.Namespace.Name},
					Spec: egressservicev1.EgressServiceSpec{
						SourceIPBy: egressservicev1.SourceIPLoadBalancer,
						NodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{
							v1.LabelHostname: eipNodes[0].Labels[v1.LabelHostname],
						}},
					},
				}, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				service := createLBServiceWithIngressIP(f.ClientSet, f.Namespace.Name, name, family,
					map[string]string{"app": "es-clients"}, "8080")
				services = append(services, service)
				serviceIPs = append(serviceIPs, net.ParseIP(service.Status.LoadBalancer.Ingress[0].IP).String())
			}
			esClient = deployClientDS(tCtx, f, "es-clients", len(nodes.Items))

			if os.Getenv("EGRESS_GATEWAY_ENDPOINT") == "" {
				ginkgo.By("Adding IP-info server return routes through the selected EgressService hosts")
				for _, service := range services {
					_, hostV4, hostV6 := getEgressSVCHost(f.ClientSet, f.Namespace.Name, service.Name)
					setSVCRouteOnExternalContainer(
						infraapi.ExternalContainer{Name: "ipinfo-server"},
						service.Status.LoadBalancer.Ingress[0].IP,
						hostV4, hostV6,
					)
				}
			}
		})

		ginkgo.It("preserves EgressIP and EgressService precedence with EgressGateway", ginkgo.SpecTimeout(12*time.Minute), func(ctx context.Context) {
			tCtx := f.TContext(ctx)

			ginkgo.By("Verifying EgressIP clients use their EgressIP")
			gomega.Eventually(tCtx, fetchProbeSourceIPs, 30*time.Second, time.Second).
				WithArguments(tCtx, eiClient, endpoints).
				MustPassRepeatedly(3).
				Should(gomega.HaveEach(gomega.BeElementOf(eipIPs)), "unexpected egress sources for %s", eiClient.Name)

			ginkgo.By("Verifying EgressService clients use their LoadBalancer IPs")
			gomega.Eventually(tCtx, fetchProbeSourceIPs, 30*time.Second, time.Second).
				WithArguments(tCtx, esClient, endpoints).
				MustPassRepeatedly(3).
				Should(gomega.HaveEach(gomega.BeElementOf(serviceIPs)), "unexpected egress sources for %s", esClient.Name)

			ginkgo.By("Verifying EgressGateway clients use their EgressGateway node IPs")
			gomega.Eventually(tCtx, fetchProbeSourceIPs, 30*time.Second, time.Second).
				WithArguments(tCtx, egClient, endpoints).
				MustPassRepeatedly(3).
				Should(gomega.HaveEach(gomega.BeElementOf(gatewayIPs)), "unexpected egress sources for %s", egClient.Name)
		})
	})
})
