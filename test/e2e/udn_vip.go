// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

// UDN VIP LoadBalancer e2e tests (OKEP-XXXX)
//
// loadBalancerClass: k8s.ovn.org/udn-loadbalancer
//
// Design summary
// ──────────────
// The VIP is allocated from serviceSubnets (a CIDR within the UDN pod subnet)
// and placed in the standard clusterLBGroup — the same group that carries the
// ClusterIP. This means:
//
//   • Pod → VIP:      OVN switch LB fires (DNAT) — same as ClusterIP.
//   • External → VIP: BGP routes to the node, GR LB fires (DNAT) — same as
//                     any other service. No br-ex flows or /32 routes needed.
//   • External → ClusterIP: same as standard k8s, works via the GR.
//   • Cross-UDN:      isolated by L2 — no route from another UDN to the
//                     VIP address space.
//
// The zone controller watches local pods (proxy-annotated or selector-matched)
// and writes a per-network node annotation listing the active VIP CIDRs.
// The RouteAdvertisements controller reads that annotation to advertise VIP
// /32s as plain ECMP (no local-pref) from every node with local capacity.
//
// BGP advertisement is tested in udn_vip_bgp.go.

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
)

const (
	udnVIPPodSubnetV4   = "10.200.0.0/16"
	udnVIPServiceSubnet = "10.200.200.0/24" // within udnVIPPodSubnetV4
	udnVIPTestPort      = 7777
)

// nodeExec runs a shell command inside a KIND node container and returns stdout.
func nodeExec(nodeName string, args ...string) (string, error) {
	cmd := append([]string{"docker", "exec", nodeName}, args...)
	out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

var _ = Describe("UDN VIP LoadBalancer", feature.UDNVIPLoadBalancer, func() {
	f := wrappedTestFramework("udn-vip")
	f.SkipNamespaceCreation = true

	var (
		cs        clientset.Interface
		udnClient udnclientset.Interface
		ns        *corev1.Namespace
	)

	BeforeEach(func() {
		cs = f.ClientSet
		var err error
		udnClient, err = udnclientset.NewForConfig(f.ClientConfig())
		Expect(err).NotTo(HaveOccurred())

		ns, err = f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		Expect(err).NotTo(HaveOccurred())
		f.Namespace = ns
	})

	// ── helpers ───────────────────────────────────────────────────────────────

	createCUDN := func(name string) *udnv1.ClusterUserDefinedNetwork {
		GinkgoHelper()
		cudn := &udnv1.ClusterUserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: udnv1.ClusterUserDefinedNetworkSpec{
				NamespaceSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns.Name},
				},
				Network: udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer2,
					Layer2: &udnv1.Layer2Config{
						Role:           udnv1.NetworkRolePrimary,
						Subnets:        udnv1.DualStackCIDRs{udnv1.CIDR(udnVIPPodSubnetV4)},
						ServiceSubnets: []udnv1.CIDR{udnv1.CIDR(udnVIPServiceSubnet)},
					},
				},
			},
		}
		_, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Create(
			context.TODO(), cudn, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			udnClient.K8sV1().ClusterUserDefinedNetworks().Patch(
				context.TODO(), cudn.Name, "application/merge-patch+json",
				[]byte(`{"metadata":{"finalizers":[]}}`), metav1.PatchOptions{})
			udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(
				context.TODO(), cudn.Name, metav1.DeleteOptions{})
		})
		Eventually(
			clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudn.Name),
			30*time.Second, time.Second,
		).Should(Succeed())
		return cudn
	}

	createVIPService := func(name string, anns map[string]string, selector map[string]string) *corev1.Service {
		GinkgoHelper()
		lbClass := types.UDNLoadBalancerClass
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name, Annotations: anns},
			Spec: corev1.ServiceSpec{
				Type: corev1.ServiceTypeLoadBalancer, LoadBalancerClass: &lbClass,
				Selector: selector,
				Ports:    []corev1.ServicePort{{Port: udnVIPTestPort, TargetPort: intstr.FromInt32(udnVIPTestPort)}},
			},
		}
		svc, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cs.CoreV1().Services(ns.Name).Delete(context.TODO(), svc.Name, metav1.DeleteOptions{})
		})
		return svc
	}

	waitVIP := func(svcName string) string {
		GinkgoHelper()
		var vip string
		Eventually(func() bool {
			s, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcName, metav1.GetOptions{})
			if err != nil || len(s.Status.LoadBalancer.Ingress) == 0 {
				return false
			}
			vip = s.Status.LoadBalancer.Ingress[0].IP
			return vip != ""
		}, 60*time.Second, time.Second).Should(BeTrue())
		framework.Logf("VIP: %s", vip)
		return vip
	}

	waitVIPAndClusterIP := func(svcName string) (vip, clusterIP string) {
		GinkgoHelper()
		vip = waitVIP(svcName)
		svc, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		clusterIP = svc.Spec.ClusterIP
		Expect(clusterIP).NotTo(BeEmpty())
		framework.Logf("ClusterIP: %s", clusterIP)
		return
	}

	agnhostPod := func(name, nodeName string, labels map[string]string, serve bool) *corev1.Pod {
		var cmd []string
		if serve {
			cmd = []string{"netexec", fmt.Sprintf("--http-port=%d", udnVIPTestPort)}
		} else {
			cmd = []string{"sleep", "3600"}
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name, Labels: labels},
			Spec: corev1.PodSpec{
				NodeName: nodeName, RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{{Name: "app", Image: images.AgnHost(), Command: cmd}},
			},
		}
	}

	// ── VIP Allocation ────────────────────────────────────────────────────────

	Context("VIP allocation", func() {
		It("allocates a VIP from serviceSubnets and writes it to service status", func() {
			createCUDN(ns.Name)
			svc := createVIPService("alloc-svc", nil, map[string]string{"app": "b"})
			vip := waitVIP(svc.Name)
			_, svcNet, _ := net.ParseCIDR(udnVIPServiceSubnet)
			Expect(svcNet.Contains(net.ParseIP(vip))).To(BeTrue(),
				"VIP %s should be in %s", vip, udnVIPServiceSubnet)
		})

		It("honours the k8s.ovn.org/udn-vip annotation for a specific VIP", func() {
			createCUDN(ns.Name)
			want := "10.200.200.42"
			svc := createVIPService("specific-svc",
				map[string]string{types.UDNVIPAnnotation: want},
				map[string]string{"app": "b"})
			Expect(waitVIP(svc.Name)).To(Equal(want))
		})

		It("re-allocates the same VIP after the service is deleted", func() {
			createCUDN(ns.Name)
			svc1 := createVIPService("rel-1", nil, nil)
			vip1 := waitVIP(svc1.Name)
			Expect(cs.CoreV1().Services(ns.Name).Delete(context.TODO(), svc1.Name, metav1.DeleteOptions{})).To(Succeed())
			svc2 := createVIPService("rel-2", nil, nil)
			Expect(waitVIP(svc2.Name)).To(Equal(vip1))
		})

		It("accepts serviceSubnets outside the pod subnet (external VIP range)", func() {
			// serviceSubnets may be entirely outside the pod subnet (e.g. a
			// customer-managed IP range). VIPs are reachable via BGP /32 routes;
			// pods get a gateway route for the external VIP range.
			cudn := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: ns.Name + "-ext"},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns.Name},
					},
					Network: udnv1.NetworkSpec{Topology: udnv1.NetworkTopologyLayer2,
						Layer2: &udnv1.Layer2Config{
							Role:           udnv1.NetworkRolePrimary,
							Subnets:        udnv1.DualStackCIDRs{"10.200.0.0/16"},
							ServiceSubnets: []udnv1.CIDR{"192.168.100.0/24"}, // external range
						}},
				},
			}
			_, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Create(
				context.TODO(), cudn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(),
				"CEL should accept serviceSubnets outside the pod subnet")
			DeferCleanup(func() {
				udnClient.K8sV1().ClusterUserDefinedNetworks().Patch(
					context.TODO(), cudn.Name, "application/merge-patch+json",
					[]byte(`{"metadata":{"finalizers":[]}}`), metav1.PatchOptions{})
				udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(
					context.TODO(), cudn.Name, metav1.DeleteOptions{})
			})
		})

		It("allocates a VIP from an external serviceSubnets CIDR", func() {
			cudn := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: ns.Name + "-ext-vip"},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns.Name},
					},
					Network: udnv1.NetworkSpec{Topology: udnv1.NetworkTopologyLayer2,
						Layer2: &udnv1.Layer2Config{
							Role:           udnv1.NetworkRolePrimary,
							Subnets:        udnv1.DualStackCIDRs{"10.200.0.0/16"},
							ServiceSubnets: []udnv1.CIDR{"192.168.100.0/24"}, // external
						}},
				},
			}
			_, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Create(
				context.TODO(), cudn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				udnClient.K8sV1().ClusterUserDefinedNetworks().Patch(
					context.TODO(), cudn.Name, "application/merge-patch+json",
					[]byte(`{"metadata":{"finalizers":[]}}`), metav1.PatchOptions{})
				udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(
					context.TODO(), cudn.Name, metav1.DeleteOptions{})
			})
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudn.Name),
				30*time.Second, time.Second).Should(Succeed())

			lbClass := types.UDNLoadBalancerClass
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "ext-vip-svc", Namespace: ns.Name},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeLoadBalancer, LoadBalancerClass: &lbClass,
					Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080)}},
				},
			}
			svc, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				cs.CoreV1().Services(ns.Name).Delete(context.TODO(), svc.Name, metav1.DeleteOptions{})
			})

			vip := waitVIP(svc.Name)
			_, extNet, _ := net.ParseCIDR("192.168.100.0/24")
			Expect(extNet.Contains(net.ParseIP(vip))).To(BeTrue(),
				"VIP %s should be from external serviceSubnets 192.168.100.0/24", vip)
		})
	})

	// ── Pod → VIP connectivity ─────────────────────────────────────────────────

	Context("Pod → VIP → backend (OVN switch LB)", func() {
		It("delivers traffic from a UDN pod to the VIP via OVN DNAT", func() {
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(nodes.Items)).To(BeNumerically(">=", 2))

			createCUDN(ns.Name)
			svc := createVIPService("conn-svc", nil, map[string]string{"app": "backend"})

			backend, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(),
				agnhostPod("backend", nodes.Items[0].Name, map[string]string{"app": "backend"}, true),
				metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(e2epod.WaitForPodRunningInNamespace(context.TODO(), cs, backend)).To(Succeed())

			client, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(),
				agnhostPod("client", nodes.Items[1].Name, nil, false),
				metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(e2epod.WaitForPodRunningInNamespace(context.TODO(), cs, client)).To(Succeed())

			vip, clusterIP := waitVIPAndClusterIP(svc.Name)

			By(fmt.Sprintf("pod → VIP %s", vip))
			Eventually(func() error {
				_, err := e2epodoutput.RunHostCmd(ns.Name, client.Name,
					fmt.Sprintf("wget -qO- --timeout=5 http://%s:%d/hostname", vip, udnVIPTestPort))
				return err
			}, 60*time.Second, 3*time.Second).Should(Succeed())

			By(fmt.Sprintf("pod → ClusterIP %s", clusterIP))
			Eventually(func() error {
				_, err := e2epodoutput.RunHostCmd(ns.Name, client.Name,
					fmt.Sprintf("wget -qO- --timeout=5 http://%s:%d/hostname", clusterIP, udnVIPTestPort))
				return err
			}, 30*time.Second, 3*time.Second).Should(Succeed())
		})
	})

	// ── External access ────────────────────────────────────────────────────────

	Context("External → ClusterIP (via GR LB)", func() {
		// The ClusterIP is in clusterLBGroup so the GR has an LB rule for it.
		// Traffic arriving at the node (e.g. from docker exec / host netns)
		// goes through the GR and gets DNAT'd to a backend.
		It("is reachable from outside the UDN (node netns → ClusterIP)", func() {
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 1)
			Expect(err).NotTo(HaveOccurred())
			worker := nodes.Items[0].Name

			createCUDN(ns.Name)
			svc := createVIPService("ext-clusterip-svc", nil, map[string]string{"app": "ext-backend"})

			backend, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(),
				agnhostPod("ext-backend", worker, map[string]string{"app": "ext-backend"}, true),
				metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(e2epod.WaitForPodRunningInNamespace(context.TODO(), cs, backend)).To(Succeed())

			_, clusterIP := waitVIPAndClusterIP(svc.Name)

			By(fmt.Sprintf("node netns on %s → ClusterIP %s", worker, clusterIP))
			// docker exec simulates external traffic entering via the node's GR
			Eventually(func() string {
				out, _ := nodeExec(worker,
					"curl", "-s", "--max-time", "5",
					fmt.Sprintf("http://%s:%d/hostname", clusterIP, udnVIPTestPort))
				return out
			}, 60*time.Second, 3*time.Second).Should(ContainSubstring("ext-backend"),
				"ClusterIP should be reachable from node netns via GR LB")
		})
	})

	// ── Cross-UDN isolation ────────────────────────────────────────────────────

	Context("Cross-UDN isolation", func() {
		It("blocks a pod in a different UDN from reaching the VIP", func() {
			createCUDN(ns.Name)
			svc := createVIPService("xnet-svc", nil, nil)
			vip := waitVIP(svc.Name)

			// Second isolated UDN
			ns2, err := f.CreateNamespace(context.TODO(), f.BaseName+"-iso",
				map[string]string{RequiredUDNNamespaceLabel: ""})
			Expect(err).NotTo(HaveOccurred())
			iso := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: ns2.Name},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns2.Name},
					},
					Network: udnv1.NetworkSpec{Topology: udnv1.NetworkTopologyLayer2,
						Layer2: &udnv1.Layer2Config{
							Role: udnv1.NetworkRolePrimary, Subnets: udnv1.DualStackCIDRs{"10.202.0.0/16"},
						}},
				},
			}
			_, err = udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.TODO(), iso, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				udnClient.K8sV1().ClusterUserDefinedNetworks().Patch(context.TODO(), iso.Name,
					"application/merge-patch+json", []byte(`{"metadata":{"finalizers":[]}}`), metav1.PatchOptions{})
				udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), iso.Name, metav1.DeleteOptions{})
			})
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, iso.Name),
				30*time.Second, time.Second).Should(Succeed())

			attacker := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "attacker", Namespace: ns2.Name},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "a", Image: images.AgnHost(), Command: []string{"sleep", "120"}}},
				},
			}
			attacker, err = cs.CoreV1().Pods(ns2.Name).Create(context.TODO(), attacker, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(e2epod.WaitForPodRunningInNamespace(context.TODO(), cs, attacker)).To(Succeed())

			Consistently(func() string {
				out, _ := e2epodoutput.RunHostCmd(ns2.Name, attacker.Name,
					fmt.Sprintf("wget -qO- --timeout=3 http://%s:%d/hostname 2>&1 || echo timeout",
						vip, udnVIPTestPort))
				return out
			}, 10*time.Second, 3*time.Second).Should(ContainSubstring("timeout"),
				"cross-UDN pod should not reach the VIP")
		})
	})

	// ── Node annotation ────────────────────────────────────────────────────────

	Context("Zone controller node annotation", func() {
		It("writes active VIP CIDRs to the node annotation when a local backend exists", func() {
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 1)
			Expect(err).NotTo(HaveOccurred())
			worker := nodes.Items[0].Name

			createCUDN(ns.Name)
			svc := createVIPService("ann-svc", nil, map[string]string{"app": "ann-backend"})

			backend, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(),
				agnhostPod("ann-backend", worker, map[string]string{"app": "ann-backend"}, false),
				metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(e2epod.WaitForPodRunningInNamespace(context.TODO(), cs, backend)).To(Succeed())

			vip := waitVIP(svc.Name)

			By("Verifying node annotation lists VIP CIDR for this network")
			Eventually(func() string {
				node, err := cs.CoreV1().Nodes().Get(context.TODO(), worker, metav1.GetOptions{})
				if err != nil {
					return ""
				}
				for k, v := range node.Annotations {
					if strings.Contains(k, "udn-lb-active-vips") {
						return v
					}
				}
				return ""
			}, 30*time.Second, 2*time.Second).Should(ContainSubstring(vip),
				"node annotation should include the VIP CIDR")
		})
	})

})