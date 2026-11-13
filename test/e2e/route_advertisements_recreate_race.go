// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	rav1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	utilnet "k8s.io/utils/net"
	kubevirtv1 "kubevirt.io/api/core/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// This test reproduces the exact production scenario: a CUDN, its
// RouteAdvertisements, and many VMs booting together. priority=300/104
// OpenFlow advertised-route rules can go permanently missing on breth0 and
// never appear until something restarts ovnkube-node. It mirrors the
// original bug report's own reproduction script. That script used a CUDN
// with Layer2/Primary/Persistent IPAM, a RouteAdvertisements selecting it,
// and many VMs created at once. It hit the race roughly 30-40% of the time
// on unfixed code.
//
// A single pass through the scenario is not a reliable check at that rate.
// Repeat the full create/verify/delete cycle several times in one spec run
// to raise the odds of catching a regression. See the two races this guards
// against:
//   - bridgeconfig: a flow-cache rebuild unrelated to the advertised-state
//     reconcile could read a stale, cached-false Advertised flag. Service
//     and Endpoint churn from many booting VMs triggers such rebuilds.
//   - networkmanager: a network created around the same time its RA becomes
//     Accepted could be skipped by the one-shot resync that only reconciles
//     already-started network controllers.
var _ = ginkgo.Describe("Kubevirt Virtual Machines: advertised flows survive CUDN, RA, and VM recreation", feature.RouteAdvertisements, ginkgo.Serial, func() {
	const (
		cudnLabel     = "bgp-vm-recreate-race"
		raName        = "bgp-vm-recreate-race-ra"
		subnetIPv4    = "108.108.0.0/16"
		subnetIPv6    = "2001:db8:108:108::/64"
		numVMs        = 8
		numIterations = 5
	)

	f := wrappedTestFramework("route-advertisements-vm-recreate-race")
	f.SkipNamespaceCreation = true

	ginkgo.It("programs advertised-route OpenFlow rules on every node without restarting ovnkube-node", func(ctx ginkgo.SpecContext) {
		crClient, err := newControllerRuntimeClient()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		namespace, err := f.CreateNamespace(ctx, f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		f.Namespace = namespace

		nodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, f.ClientSet, 3)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 0))

		bridgeName := deploymentconfig.Get().ExternalBridgeName()
		ovnkNs := deploymentconfig.Get().OVNKubernetesNamespace()

		udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		raClient, err := raclientset.NewForConfig(f.ClientConfig())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Only request a subnet, and only check for its advertised-route flow,
		// for IP families the cluster actually supports.
		subnetsByFamily := map[utilnet.IPFamily]string{
			utilnet.IPv4: subnetIPv4,
			utilnet.IPv6: subnetIPv6,
		}
		// OpenFlow match field name for each family, as `ovs-ofctl dump-flows`
		// reports it ("nw_dst"/"nw_src" for IPv4, "ipv6_dst"/"ipv6_src" for IPv6).
		flowFieldPrefixByFamily := map[utilnet.IPFamily]string{
			utilnet.IPv4: "nw",
			utilnet.IPv6: "ipv6",
		}
		var udnSubnets udnv1.DualStackCIDRs
		var advertisedSubnets, advertisedFlowFieldPrefixes []string
		for _, family := range getSupportedIPFamiliesSlice(f.ClientSet) {
			udnSubnets = append(udnSubnets, udnv1.CIDR(subnetsByFamily[family]))
			advertisedSubnets = append(advertisedSubnets, subnetsByFamily[family])
			advertisedFlowFieldPrefixes = append(advertisedFlowFieldPrefixes, flowFieldPrefixByFamily[family])
		}
		gomega.Expect(advertisedSubnets).NotTo(gomega.BeEmpty(), "cluster reports no supported IP families")

		// Headless Service, matching the original bug report, so VM boot
		// produces Endpoint churn too. Created once: every iteration's VMs
		// churn its endpoints.
		_, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(ctx, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "churn-headless"},
			Spec: corev1.ServiceSpec{
				ClusterIP: corev1.ClusterIPNone,
				Selector:  map[string]string{"app": "churn"},
				Ports:     []corev1.ServicePort{{Port: 8091, Name: "rest"}},
			},
		}, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		for iter := 1; iter <= numIterations; iter++ {
			// Declared here, before the CUDN and RA creation below. The
			// DeferCleanup closures registered for them capture this variable.
			// They wait for this iteration's VMs to disappear before deleting
			// the cluster-scoped network resources those VMs depend on.
			var vms []*kubevirtv1.VirtualMachine

			ginkgo.By(fmt.Sprintf("[%d/%d] creating a CUDN with Layer2/Primary/Persistent IPAM, matching the original bug report", iter, numIterations))
			cudn := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: cudnLabel + "-",
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
			// DeferCleanup runs after the It node SpecContext is canceled, so it
			// needs its own context rather than the (by-then stale) ctx captured above.
			ginkgo.DeferCleanup(func(cleanupCtx context.Context) {
				waitForVMPodsGone(cleanupCtx, f, vms)
				deleteAndWaitGone(cleanupCtx, fmt.Sprintf("CUDN %s", cUDN.Name),
					func() error {
						return udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(cleanupCtx, cUDN.Name, metav1.DeleteOptions{})
					},
					func() error {
						_, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Get(cleanupCtx, cUDN.Name, metav1.GetOptions{})
						return err
					})
			})

			// Create the RA right away, without waiting for the CUDN to report
			// Ready. Production hit this race when the network's own controller
			// did not finish starting before its RA transitioned to Accepted.
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
			ginkgo.DeferCleanup(func(cleanupCtx context.Context) {
				waitForVMPodsGone(cleanupCtx, f, vms)
				deleteAndWaitGone(cleanupCtx, fmt.Sprintf("RouteAdvertisements %s", raName),
					func() error {
						return raClient.K8sV1().RouteAdvertisements().Delete(cleanupCtx, raName, metav1.DeleteOptions{})
					},
					func() error {
						_, err := raClient.K8sV1().RouteAdvertisements().Get(cleanupCtx, raName, metav1.GetOptions{})
						return err
					})
			})

			ginkgo.By(fmt.Sprintf("[%d/%d] booting %d VMs together, without waiting for the CUDN or RA to settle first", iter, numIterations, numVMs))
			vms = make([]*kubevirtv1.VirtualMachine, numVMs)
			for i := 0; i < numVMs; i++ {
				vm := composeFedoraWithTestToolingVM(
					f.Namespace.Name,
					map[string]string{"app": "churn"},
					nil,
					nil,
					kubevirtv1.NetworkSource{Pod: &kubevirtv1.PodNetwork{}},
					"#cloud-config\npassword: fedora\nchpasswd: { expire: False }\n",
					"version: 2\nethernets:\n  eth0:\n    dhcp4: true\n    dhcp6: true\n",
				)
				vm.Name = fmt.Sprintf("churn-vm-%d", i)
				vm.Spec.Template.Spec.TerminationGracePeriodSeconds = int64Ptr(5)
				// Spread VMs across nodes, matching the original bug report.
				vm.Spec.Template.Spec.Affinity = &corev1.Affinity{
					PodAntiAffinity: &corev1.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
							Weight: 1,
							PodAffinityTerm: corev1.PodAffinityTerm{
								LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
									Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"churn"},
								}}},
								TopologyKey: "kubernetes.io/hostname",
							},
						}},
					},
				}
				vms[i] = vm
				createVirtualMachineWithClient(crClient, vm)
				ginkgo.DeferCleanup(func(cleanupCtx context.Context) {
					crClient.Delete(cleanupCtx, vm)
				})
			}

			ginkgo.By(fmt.Sprintf("[%d/%d] waiting for every VM's instance to start running", iter, numIterations))
			vmHostNodes := sets.New[string]()
			for _, vm := range vms {
				vmi := &kubevirtv1.VirtualMachineInstance{ObjectMeta: metav1.ObjectMeta{Name: vm.Name, Namespace: f.Namespace.Name}}
				gomega.Eventually(func() kubevirtv1.VirtualMachineInstancePhase {
					if err := crClient.Get(ctx, crclient.ObjectKeyFromObject(vmi), vmi); err != nil {
						return ""
					}
					return vmi.Status.Phase
				}, 6*time.Minute, 2*time.Second).Should(gomega.Equal(kubevirtv1.Running), "VM %s never started running", vm.Name)
				vmHostNodes.Insert(vmi.Status.NodeName)
			}

			ginkgo.By(fmt.Sprintf("[%d/%d] waiting for the RA to become Accepted", iter, numIterations))
			gomega.Eventually(func() string {
				got, err := raClient.K8sV1().RouteAdvertisements().Get(ctx, raName, metav1.GetOptions{})
				if err != nil {
					return ""
				}
				c := meta.FindStatusCondition(got.Status.Conditions, "Accepted")
				if c == nil {
					return ""
				}
				return c.Reason
			}, 60*time.Second, time.Second).Should(gomega.Equal("Accepted"))

			ginkgo.By(fmt.Sprintf("[%d/%d] verifying advertised-route OpenFlow rules appear on every node hosting a VM, without restarting ovnkube-node", iter, numIterations))
			for _, nodeName := range sets.List(vmHostNodes) {
				ovnkPods, err := f.ClientSet.CoreV1().Pods(ovnkNs).List(ctx, metav1.ListOptions{
					LabelSelector: "app=ovnkube-node",
					FieldSelector: "spec.nodeName=" + nodeName,
				})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ovnkPods.Items).To(gomega.HaveLen(1),
					"expected exactly one ovnkube-node pod on VM host node %s", nodeName)
				ovnkPod := ovnkPods.Items[0]
				// priority=300 matches on the destination subnet (ingress into the
				// UDN), priority=104 matches on the source subnet (egress out of
				// it). Match the complete subnet value in its match field, not a
				// bare substring, so only this CUDN's own advertised subnet can
				// satisfy the check.
				for priorityDir, dir := range map[string]string{"priority=300": "dst", "priority=104": "src"} {
					flowMatchers := make([]gomega.OmegaMatcher, 0, len(advertisedSubnets))
					for i, subnet := range advertisedSubnets {
						flowMatchers = append(flowMatchers,
							gomega.ContainSubstring(fmt.Sprintf("%s_%s=%s", advertisedFlowFieldPrefixes[i], dir, subnet)))
					}
					// Check priority=300 and priority=104 independently: a match for one
					// priority must not let a missing rule at the other priority pass.
					gomega.Eventually(func() string {
						out, _ := e2epodoutput.RunHostCmdWithRetries(ovnkPod.Namespace, ovnkPod.Name,
							fmt.Sprintf("ovs-ofctl dump-flows %s | grep '%s'", bridgeName, priorityDir),
							2*time.Second, 5*time.Second)
						return out
					}, 120*time.Second, 2*time.Second).Should(gomega.And(flowMatchers...),
						"advertised-route flows at %s for %v never appeared on %s on node %s", priorityDir, advertisedSubnets, bridgeName, nodeName)
				}
			}

			ginkgo.By(fmt.Sprintf("[%d/%d] cleaning up", iter, numIterations))
			for _, vm := range vms {
				gomega.Expect(crClient.Delete(ctx, vm)).To(gomega.Succeed())
			}
			waitForVMPodsGone(ctx, f, vms)

			// Wait for the RA to fully go away too. It has a fixed name. The next
			// iteration recreates it, and a lingering object would fail that
			// Create with AlreadyExists.
			deleteAndWaitGone(ctx, fmt.Sprintf("RouteAdvertisements %s", raName),
				func() error {
					return raClient.K8sV1().RouteAdvertisements().Delete(ctx, raName, metav1.DeleteOptions{})
				},
				func() error {
					_, err := raClient.K8sV1().RouteAdvertisements().Get(ctx, raName, metav1.GetOptions{})
					return err
				})
			deleteAndWaitGone(ctx, fmt.Sprintf("CUDN %s", cUDN.Name),
				func() error {
					return udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(ctx, cUDN.Name, metav1.DeleteOptions{})
				},
				func() error {
					_, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Get(ctx, cUDN.Name, metav1.GetOptions{})
					return err
				})
		}
	})
})

// The explicit deletes above run first on success. DeferCleanup registers
// the same deletes again as a safety net for a failed run. Deletion is
// idempotent, so running it twice is safe.

// waitForVMPodsGone waits until every VM's virt-launcher pod is gone from the
// namespace. It skips entries not yet populated. That happens when a
// DeferCleanup safety net fires before the VM creation loop finishes.
func waitForVMPodsGone(ctx context.Context, f *framework.Framework, vms []*kubevirtv1.VirtualMachine) {
	for _, vm := range vms {
		if vm == nil {
			continue
		}
		vmName := vm.Name
		gomega.Eventually(func() bool {
			pods, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).List(ctx, metav1.ListOptions{
				LabelSelector: "kubevirt.io=virt-launcher",
				FieldSelector: "status.phase!=Failed,status.phase!=Succeeded",
			})
			if err != nil {
				return false
			}
			for _, p := range pods.Items {
				if p.Labels["vm.kubevirt.io/name"] == vmName {
					return false
				}
			}
			return true
		}, 2*time.Minute, 2*time.Second).Should(gomega.BeTrue(), "virt-launcher pod for %s never went away", vmName)
	}
}

// deleteAndWaitGone deletes a resource, reporting any failure other than
// NotFound, then waits for it to disappear.
func deleteAndWaitGone(ctx context.Context, resourceDesc string, del, get func() error) {
	err := del()
	gomega.Expect(err == nil || apierrors.IsNotFound(err)).To(gomega.BeTrue(),
		"failed to delete %s during cleanup: %v", resourceDesc, err)
	gomega.Eventually(func() bool {
		return apierrors.IsNotFound(get())
	}, 60*time.Second, time.Second).Should(gomega.BeTrue(), "%s never disappeared during cleanup", resourceDesc)
}

func int64Ptr(v int64) *int64 { return &v }
