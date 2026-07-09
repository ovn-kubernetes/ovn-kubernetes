// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

// udn_vip_bgp.go: e2e tests for LoadBalancerVIP BGP advertisement via
// RouteAdvertisements (OKEP-XXXX).
//
// The VIP /32 is advertised via plain ECMP from every node that has a
// local backend or proxy pod. No local-pref differentiation — BGP 5-tuple
// hash provides flow affinity across advertising nodes.
//
// Uses the same agnhost+FRR infrastructure as the existing CUDN RA tests
// (route_advertisements.go).
//
// Tests:
//  1. VIP /32 appears in the external FRR routing table from a node with
//     a local backend pod; disappears when the pod is deleted.
//  2. With multiple backend nodes, the /32 is visible from all of them
//     (ECMP paths).
//  3. VIP advertisement withdrawn when the service is deleted.

import (
	"context"
	"fmt"
	"net"
	"os"
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
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	utilnet "k8s.io/utils/net"
)

func init() {
	if os.Getenv("ENABLE_ROUTE_ADVERTISEMENTS") == "true" {
		images.Add(images.FRR())
	}
}

var _ = ginkgo.Describe("BGP: UDN VIP LoadBalancer advertisement",
	feature.RouteAdvertisements, func() {

		f := wrappedTestFramework("udn-vip-bgp")
		f.SkipNamespaceCreation = true

		var (
			cs        clientset.Interface
			udnClient udnclientset.Interface
			raClient  *raclientset.Clientset
			ns        *corev1.Namespace
			ictx      infraapi.Context
		)

		ginkgo.BeforeEach(func() {
			cs = f.ClientSet

			var err error
			udnClient, err = udnclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			raClient, err = raclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ns, err = f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			f.Namespace = ns

			ictx = infraprovider.Get().NewTestContext()
		})

		// ── helpers ───────────────────────────────────────────────────────────

		// setupBGPInfra allocates BGP subnets, creates the FRR router and agnhost
		// server, returns the FRR container reference and the CUDN.
		setupBGPInfra := func(nameSuffix string) (
			infraapi.ExternalContainer,
			*udnv1.ClusterUserDefinedNetwork,
			string, // VIP CIDR to watch
		) {
			ginkgo.GinkgoHelper()

			ipFamilySet := sets.New[utilnet.IPFamily]()
			if isIPv4Supported(cs) {
				ipFamilySet.Insert(utilnet.IPv4)
			}

			bgpAlloc, err := allocators.AllocateBGP(f, ictx)
			framework.ExpectNoError(err, "failed to allocate BGP subnets")

			netName := fmt.Sprintf("udn-vip-bgp-%s-%s", nameSuffix, ns.Name)

			err = runBGPNetworkAndServer(
				f, ictx, ipFamilySet, netName,
				netName+"-agnhost", netName+"-server",
				[]string{bgpAlloc.BGPPeerSubnet},
				[]string{bgpAlloc.UDNSubnet},
			)
			framework.ExpectNoError(err, "failed to deploy BGP network")

			frrContainer := infraapi.ExternalContainer{Name: netName + "-frr"}

			// Carve VIP serviceSubnets from the BGP-allocated UDN subnet.
			_, podNet, _ := net.ParseCIDR(bgpAlloc.UDNSubnet)
			vipSubnet := fmt.Sprintf("%d.%d.200.0/24", podNet.IP[0], podNet.IP[1])

			cudn := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: ns.Name + "-" + nameSuffix},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{
							"kubernetes.io/metadata.name": ns.Name,
						},
					},
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer2,
						Layer2: &udnv1.Layer2Config{
							Role:           udnv1.NetworkRolePrimary,
							Subnets:        udnv1.DualStackCIDRs{udnv1.CIDR(bgpAlloc.UDNSubnet)},
							ServiceSubnets: []udnv1.CIDR{udnv1.CIDR(vipSubnet)},
						},
					},
				},
			}
			_, err = udnClient.K8sV1().ClusterUserDefinedNetworks().Create(
				context.TODO(), cudn, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.DeferCleanup(func() {
				udnClient.K8sV1().ClusterUserDefinedNetworks().Patch(
					context.TODO(), cudn.Name,
					"application/merge-patch+json",
					[]byte(`{"metadata":{"finalizers":[]}}`),
					metav1.PatchOptions{})
				udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(
					context.TODO(), cudn.Name, metav1.DeleteOptions{})
			})
			gomega.Eventually(
				clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudn.Name),
				30*time.Second, time.Second,
			).Should(gomega.Succeed())

			return frrContainer, cudn, vipSubnet
		}

		// createLBService creates a UDN LoadBalancer service and waits for VIP.
		createLBService := func(name string, selector map[string]string) (*corev1.Service, string) {
			ginkgo.GinkgoHelper()
			lbClass := types.UDNLoadBalancerClass
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name},
				Spec: corev1.ServiceSpec{
					Type:              corev1.ServiceTypeLoadBalancer,
					LoadBalancerClass: &lbClass,
					Selector:          selector,
					Ports: []corev1.ServicePort{{
						Port:       80,
						TargetPort: intstr.FromInt32(8080),
						Protocol:   corev1.ProtocolTCP,
					}},
				},
			}
			svc, err := cs.CoreV1().Services(ns.Name).Create(
				context.TODO(), svc, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.DeferCleanup(func() {
				cs.CoreV1().Services(ns.Name).Delete(
					context.TODO(), svc.Name, metav1.DeleteOptions{})
			})

			var vip string
			gomega.Eventually(func() bool {
				s, err := cs.CoreV1().Services(ns.Name).Get(
					context.TODO(), svc.Name, metav1.GetOptions{})
				if err != nil || len(s.Status.LoadBalancer.Ingress) == 0 {
					return false
				}
				vip = s.Status.LoadBalancer.Ingress[0].IP
				return vip != ""
			}, 60*time.Second, time.Second).Should(gomega.BeTrue())
			framework.Logf("VIP allocated: %s", vip)
			return svc, vip
		}

		// createRA creates a RouteAdvertisements CR for this CUDN with LoadBalancerVIP.
		createRA := func(name string, cudnName string) {
			ginkgo.GinkgoHelper()
			ra := &rav1.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: rav1.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{
									"kubernetes.io/metadata.name": cudnName,
								},
							},
						},
					}},
					NodeSelector:             metav1.LabelSelector{},
					FRRConfigurationSelector: metav1.LabelSelector{},
					Advertisements:           []rav1.AdvertisementType{rav1.LoadBalancerVIP},
				},
			}
			_, err := raClient.K8sV1().RouteAdvertisements().Create(
				context.TODO(), ra, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.DeferCleanup(func() {
				raClient.K8sV1().RouteAdvertisements().Delete(
					context.TODO(), ra.Name, metav1.DeleteOptions{})
			})
			gomega.Eventually(
				routeAdvertisementsReadyFunc(*raClient, ra.Name),
				30*time.Second, time.Second,
			).Should(gomega.Succeed())
		}

		// ── Tests ─────────────────────────────────────────────────────────────

		ginkgo.It("advertises VIP /32 from a node with a local backend pod, withdraws after pod deletion", func() {
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 1)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">=", 1))
			workerNode := nodes.Items[0]

			frrContainer, cudn, _ := setupBGPInfra("single")
			_, vip := createLBService("vip-single-svc", map[string]string{"app": "bgp-backend"})
			createRA(ns.Name+"-single-ra", cudn.Name)

			vipCIDR := vip + "/32"
			if utilnet.IsIPv6String(vip) {
				vipCIDR = vip + "/128"
			}

			ginkgo.By("Deploying backend pod on " + workerNode.Name)
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "bgp-backend",
					Namespace: ns.Name,
					Labels:    map[string]string{"app": "bgp-backend"},
				},
				Spec: corev1.PodSpec{
					NodeName:      workerNode.Name,
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "srv",
						Image:   images.AgnHost(),
						Command: []string{"sleep", "3600"},
					}},
				},
			}
			pod, err = cs.CoreV1().Pods(ns.Name).Create(
				context.TODO(), pod, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(e2epod.WaitForPodRunningInNamespace(context.TODO(), cs, pod)).To(gomega.Succeed())

			ginkgo.By(fmt.Sprintf("Verifying VIP /32 %s appears in FRR routing table via node %s",
				vipCIDR, workerNode.Name))
			checkVIPRouteInFRRContainer(frrContainer, workerNode, vipCIDR, true)

			ginkgo.By("Deleting the backend pod")
			graceZero := int64(0)
			gomega.Expect(cs.CoreV1().Pods(ns.Name).Delete(
				context.TODO(), pod.Name,
				metav1.DeleteOptions{GracePeriodSeconds: &graceZero})).To(gomega.Succeed())
			gomega.Expect(e2epod.WaitForPodNotFoundInNamespace(
				context.TODO(), cs, pod.Name, ns.Name, 60*time.Second)).To(gomega.Succeed())

			ginkgo.By("Verifying VIP /32 is withdrawn after pod deletion")
			checkVIPRouteInFRRContainer(frrContainer, workerNode, vipCIDR, false)
		})

		ginkgo.It("advertises VIP /32 from all nodes with local backends (ECMP)", func() {
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">=", 2))

			frrContainer, cudn, _ := setupBGPInfra("ecmp")
			_, vip := createLBService("vip-ecmp-svc", map[string]string{"app": "ecmp-backend"})
			createRA(ns.Name+"-ecmp-ra", cudn.Name)

			vipCIDR := vip + "/32"

			for i, node := range nodes.Items[:2] {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("ecmp-backend-%d", i),
						Namespace: ns.Name,
						Labels:    map[string]string{"app": "ecmp-backend"},
					},
					Spec: corev1.PodSpec{
						NodeName:      node.Name,
						RestartPolicy: corev1.RestartPolicyNever,
						Containers: []corev1.Container{{
							Name:    "srv",
							Image:   images.AgnHost(),
							Command: []string{"sleep", "3600"},
						}},
					},
				}
				pod, err = cs.CoreV1().Pods(ns.Name).Create(
					context.TODO(), pod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(e2epod.WaitForPodRunningInNamespace(context.TODO(), cs, pod)).To(gomega.Succeed())
			}

			ginkgo.By("Verifying VIP /32 advertised from both nodes (plain ECMP, no local-pref)")
			for _, node := range nodes.Items[:2] {
				checkVIPRouteInFRRContainer(frrContainer, node, vipCIDR, true)
			}
		})

		ginkgo.It("withdraws the VIP advertisement when the service is deleted", func() {
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 1)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			workerNode := nodes.Items[0]

			frrContainer, cudn, _ := setupBGPInfra("del")
			svc, vip := createLBService("vip-del-svc", map[string]string{"app": "del-backend"})
			createRA(ns.Name+"-del-ra", cudn.Name)

			vipCIDR := vip + "/32"

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "del-backend",
					Namespace: ns.Name,
					Labels:    map[string]string{"app": "del-backend"},
				},
				Spec: corev1.PodSpec{
					NodeName:      workerNode.Name,
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "srv",
						Image:   images.AgnHost(),
						Command: []string{"sleep", "3600"},
					}},
				},
			}
			pod, err = cs.CoreV1().Pods(ns.Name).Create(
				context.TODO(), pod, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(e2epod.WaitForPodRunningInNamespace(context.TODO(), cs, pod)).To(gomega.Succeed())

			ginkgo.By("Confirming VIP is advertised before deletion")
			checkVIPRouteInFRRContainer(frrContainer, workerNode, vipCIDR, true)

			ginkgo.By("Deleting the service")
			gomega.Expect(cs.CoreV1().Services(ns.Name).Delete(
				context.TODO(), svc.Name, metav1.DeleteOptions{})).To(gomega.Succeed())

			ginkgo.By("Verifying VIP /32 is withdrawn after service deletion")
			checkVIPRouteInFRRContainer(frrContainer, workerNode, vipCIDR, false)
		})

		ginkgo.It("ETP=Cluster: advertises VIP /32 from all nodes regardless of local backends", func() {
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">=", 2))
			backendNode := nodes.Items[0]
			emptyNode := nodes.Items[1] // no backend pod here

			frrContainer, cudn, _ := setupBGPInfra("etp-cluster")

			// ETP=Cluster (default) — all nodes should advertise.
			_, vip := createLBService("etp-cluster-svc", map[string]string{"app": "etp-cluster-backend"})
			createRA(ns.Name+"-etp-cluster-ra", cudn.Name)

			vipCIDR := vip + "/32"

			ginkgo.By("Deploying backend pod on " + backendNode.Name + " only")
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "etp-cluster-backend", Namespace: ns.Name,
					Labels: map[string]string{"app": "etp-cluster-backend"},
				},
				Spec: corev1.PodSpec{
					NodeName: backendNode.Name, RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name: "srv", Image: images.AgnHost(), Command: []string{"sleep", "3600"},
					}},
				},
			}
			pod, err = cs.CoreV1().Pods(ns.Name).Create(
				context.TODO(), pod, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(e2epod.WaitForPodRunningInNamespace(context.TODO(), cs, pod)).To(gomega.Succeed())

			ginkgo.By("ETP=Cluster: both nodes (with and without backend) should advertise /32")
			checkVIPRouteInFRRContainer(frrContainer, backendNode, vipCIDR, true)
			checkVIPRouteInFRRContainer(frrContainer, emptyNode, vipCIDR, true)
		})

		ginkgo.It("ETP=Local: advertises VIP /32 only from nodes with local backends", func() {
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">=", 2))
			backendNode := nodes.Items[0]
			emptyNode := nodes.Items[1]

			frrContainer, cudn, _ := setupBGPInfra("etp-local")

			// ETP=Local — only nodes with local backends should advertise.
			lbClass := types.UDNLoadBalancerClass
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "etp-local-svc", Namespace: ns.Name},
				Spec: corev1.ServiceSpec{
					Type:                  corev1.ServiceTypeLoadBalancer,
					LoadBalancerClass:     &lbClass,
					ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal,
					Selector:              map[string]string{"app": "etp-local-backend"},
					Ports: []corev1.ServicePort{{
						Port: 80, TargetPort: intstr.FromInt32(8080),
					}},
				},
			}
			svc, err = cs.CoreV1().Services(ns.Name).Create(
				context.TODO(), svc, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.DeferCleanup(func() {
				cs.CoreV1().Services(ns.Name).Delete(
					context.TODO(), svc.Name, metav1.DeleteOptions{})
			})
			var vip string
			gomega.Eventually(func() bool {
				s, _ := cs.CoreV1().Services(ns.Name).Get(
					context.TODO(), svc.Name, metav1.GetOptions{})
				if len(s.Status.LoadBalancer.Ingress) == 0 {
					return false
				}
				vip = s.Status.LoadBalancer.Ingress[0].IP
				return vip != ""
			}, 60*time.Second, time.Second).Should(gomega.BeTrue())

			createRA(ns.Name+"-etp-local-ra", cudn.Name)
			vipCIDR := vip + "/32"

			ginkgo.By("Deploying backend pod on " + backendNode.Name + " only")
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "etp-local-backend", Namespace: ns.Name,
					Labels: map[string]string{"app": "etp-local-backend"},
				},
				Spec: corev1.PodSpec{
					NodeName: backendNode.Name, RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name: "srv", Image: images.AgnHost(), Command: []string{"sleep", "3600"},
					}},
				},
			}
			pod, err = cs.CoreV1().Pods(ns.Name).Create(
				context.TODO(), pod, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(e2epod.WaitForPodRunningInNamespace(context.TODO(), cs, pod)).To(gomega.Succeed())

			ginkgo.By("ETP=Local: only the node with a local backend should advertise")
			checkVIPRouteInFRRContainer(frrContainer, backendNode, vipCIDR, true)
			checkVIPRouteInFRRContainer(frrContainer, emptyNode, vipCIDR, false)

			ginkgo.By("Changing ETP to Cluster — empty node should now also advertise")
			svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
			_, err = cs.CoreV1().Services(ns.Name).Update(
				context.TODO(), svc, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			checkVIPRouteInFRRContainer(frrContainer, emptyNode, vipCIDR, true)

			ginkgo.By("Reverting to ETP=Local — empty node should stop advertising")
			svc, _ = cs.CoreV1().Services(ns.Name).Get(
				context.TODO(), svc.Name, metav1.GetOptions{})
			svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
			_, err = cs.CoreV1().Services(ns.Name).Update(
				context.TODO(), svc, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			checkVIPRouteInFRRContainer(frrContainer, emptyNode, vipCIDR, false)
		})
	})

// checkVIPRouteInFRRContainer asserts that a VIP CIDR is present or absent in
// the FRR routing table, reachable via the given node's IP.
func checkVIPRouteInFRRContainer(
	frrContainer infraapi.ExternalContainer,
	node corev1.Node,
	vipCIDR string,
	shouldExist bool,
) {
	ginkgo.GinkgoHelper()

	isIPv6 := utilnet.IsIPv6CIDRString(vipCIDR)
	ipFlag := ""
	if isIPv6 {
		ipFlag = " -6"
	}

	var nodeIP string
	if isIPv6 {
		lla, err := GetNodeIPv6LinkLocalAddressForEth0(node.Name)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nodeIP = lla
	} else {
		addrs := e2enode.GetAddressesByTypeAndFamily(&node,
			corev1.NodeInternalIP, corev1.IPv4Protocol)
		gomega.Expect(addrs).NotTo(gomega.BeEmpty())
		nodeIP = addrs[0]
	}

	cmd := strings.Split(
		fmt.Sprintf("ip%s route show %s", ipFlag, vipCIDR), " ")

	timeout := 60 * time.Second
	if !shouldExist {
		timeout = 90 * time.Second
	}

	framework.Logf("FRR route check: %s via %s (%s) shouldExist=%v",
		vipCIDR, node.Name, nodeIP, shouldExist)

	gomega.Eventually(func() bool {
		out, err := infraprovider.Get().ExecExternalContainerCommand(frrContainer, cmd)
		if err != nil {
			return !shouldExist
		}
		framework.Logf("FRR route table for %s: %s", vipCIDR, out)
		return strings.Contains(out, nodeIP)
	}, timeout, 3*time.Second).Should(gomega.Equal(shouldExist),
		"FRR route %s via %s shouldExist=%v", vipCIDR, node.Name, shouldExist)
}
