package vtep

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func newVTEP(name string, mode vtepv1.VTEPMode, cidrs ...string) *vtepv1.VTEP {
	dsCIDRs := make(vtepv1.DualStackCIDRs, len(cidrs))
	for i, c := range cidrs {
		dsCIDRs[i] = vtepv1.CIDR(c)
	}
	return &vtepv1.VTEP{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: vtepv1.VTEPSpec{
			CIDRs: dsCIDRs,
			Mode:  mode,
		},
	}
}

func newCUDNWithEVPN(name, vtepName string) *udnv1.ClusterUserDefinedNetwork {
	return &udnv1.ClusterUserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: udnv1.ClusterUserDefinedNetworkSpec{
			NamespaceSelector: metav1.LabelSelector{},
			Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "10.0.0.0/16"},
					},
				},
				EVPN: &udnv1.EVPNConfig{
					VTEP:  vtepName,
					IPVRF: &udnv1.VRFConfig{VNI: 100},
				},
			},
		},
	}
}

func getVTEPFinalizers(client *vtepfake.Clientset, vtepName string) []string {
	vtep, err := client.K8sV1().VTEPs().Get(context.Background(), vtepName, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return vtep.Finalizers
}

func newNodeWithHostCIDRs(name string, cidrs ...string) *corev1.Node {
	annotation, _ := json.Marshal(cidrs)
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				"k8s.ovn.org/host-cidrs": string(annotation),
			},
		},
	}
}

func getNodeAllocations(client *vtepfake.Clientset, vtepName string) []vtepv1.NodeVTEPAllocation {
	vtep, err := client.K8sV1().VTEPs().Get(context.Background(), vtepName, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return vtep.Status.NodeAllocations
}

func getVTEPCondition(client *vtepfake.Clientset, vtepName, conditionType string) *metav1.Condition {
	vtep, err := client.K8sV1().VTEPs().Get(context.Background(), vtepName, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	for i := range vtep.Status.Conditions {
		if vtep.Status.Conditions[i].Type == conditionType {
			return &vtep.Status.Conditions[i]
		}
	}
	return nil
}

var _ = ginkgo.Describe("VTEP Controller", func() {
	var (
		controller    *Controller
		fakeVTEP      *vtepfake.Clientset
		fakeClientset *util.OVNClusterManagerClientset
		wf            *factory.WatchFactory
		fakeRecorder  *record.FakeRecorder
	)

	start := func(objects ...runtime.Object) {
		vtepObjects := []runtime.Object{}
		otherObjects := []runtime.Object{}
		for _, obj := range objects {
			switch obj.(type) {
			case *vtepv1.VTEP:
				vtepObjects = append(vtepObjects, obj)
			default:
				otherObjects = append(otherObjects, obj)
			}
		}

		fakeVTEP = vtepfake.NewSimpleClientset(vtepObjects...)
		ovntest.AddVTEPApplyReactor(fakeVTEP)
		ovntest.AddVTEPGarbageCollectionReactor(fakeVTEP)
		fakeRecorder = record.NewFakeRecorder(100)

		fakeClientset = util.GetOVNClientset(otherObjects...).GetClusterManagerClientset()
		fakeClientset.VTEPClient = fakeVTEP

		var err error
		wf, err = factory.NewClusterManagerWatchFactory(fakeClientset)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		controller = NewController(wf, fakeClientset, fakeRecorder)

		err = wf.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = controller.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	ginkgo.BeforeEach(func() {
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableRouteAdvertisements = true
		config.OVNKubernetesFeature.EnableEVPN = true
		config.Gateway.Mode = config.GatewayModeLocal
	})

	ginkgo.AfterEach(func() {
		if controller != nil {
			controller.Stop()
		}
		if wf != nil {
			wf.Shutdown()
		}
	})

	ginkgo.Context("Managed mode gate", func() {
		ginkgo.It("sets Accepted=False for a VTEP with mode Managed", func() {
			vtep := newVTEP("managed-vtep", vtepv1.VTEPModeManaged, "100.64.0.0/24")
			start(vtep)

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "managed-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).ShouldNot(gomega.BeNil())

			cond := getVTEPCondition(fakeVTEP, "managed-vtep", conditionTypeAccepted)
			gomega.Expect(cond.Status).To(gomega.Equal(metav1.ConditionFalse))
			gomega.Expect(cond.Reason).To(gomega.Equal("ManagedModeNotSupported"))

			gomega.Eventually(fakeRecorder.Events).Should(gomega.Receive(
				gomega.ContainSubstring("ManagedModeNotSupported"),
			))
		})

		ginkgo.It("sets Accepted=True for a VTEP with mode Unmanaged", func() {
			vtep := newVTEP("unmanaged-vtep", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep)

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "unmanaged-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal("Allocated")),
			))
		})

		ginkgo.It("sets Accepted=False when mode changes from Unmanaged to Managed", func() {
			vtep := newVTEP("mode-change-vtep", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep)

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "mode-change-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			vtep, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "mode-change-vtep", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtep.Spec.Mode = vtepv1.VTEPModeManaged
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), vtep, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "mode-change-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal("ManagedModeNotSupported")),
			))
		})

		ginkgo.It("sets Accepted=True when mode changes from Managed to Unmanaged", func() {
			vtep := newVTEP("mode-recover-vtep", vtepv1.VTEPModeManaged, "100.64.0.0/24")
			start(vtep)

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "mode-recover-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal("ManagedModeNotSupported")),
			))

			vtep, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "mode-recover-vtep", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtep.Spec.Mode = vtepv1.VTEPModeUnmanaged
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), vtep, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "mode-recover-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal("Allocated")),
			))
		})
	})

	ginkgo.Context("Finalizer management", func() {
		ginkgo.It("adds finalizer to a new VTEP", func() {
			vtep := newVTEP("finalize-vtep", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep)

			gomega.Eventually(func() []string {
				return getVTEPFinalizers(fakeVTEP, "finalize-vtep")
			}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))
		})

		ginkgo.It("removes finalizer and allows deletion when no CUDNs reference the VTEP", func() {
			vtep := newVTEP("delete-vtep", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep)

			gomega.Eventually(func() []string {
				return getVTEPFinalizers(fakeVTEP, "delete-vtep")
			}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "delete-vtep", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			now := metav1.Now()
			v.DeletionTimestamp = &now
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() []string {
				return getVTEPFinalizers(fakeVTEP, "delete-vtep")
			}).WithTimeout(5 * time.Second).ShouldNot(gomega.ContainElement(finalizerVTEP))
		})

		ginkgo.It("blocks deletion when a CUDN references the VTEP", func() {
			cudn := newCUDNWithEVPN("test-cudn", "blocked-vtep")
			vtep := newVTEP("blocked-vtep", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep, cudn)

			gomega.Eventually(func() []string {
				return getVTEPFinalizers(fakeVTEP, "blocked-vtep")
			}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "blocked-vtep", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			now := metav1.Now()
			v.DeletionTimestamp = &now
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Consistently(func() []string {
				return getVTEPFinalizers(fakeVTEP, "blocked-vtep")
			}).WithTimeout(3 * time.Second).Should(gomega.ContainElement(finalizerVTEP))
		})
	})

	ginkgo.Context("Cross-VTEP CIDR overlap validation", func() {
		ginkgo.It("sets Accepted=True when VTEPs have non-overlapping CIDRs", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/24")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.1.0.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))
		})

		ginkgo.It("sets Accepted=False on both VTEPs when CIDRs overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.1.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			condA := getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			gomega.Expect(condA.Reason).To(gomega.Equal(reasonCIDROverlap))
			gomega.Expect(condA.Message).To(gomega.ContainSubstring("vtep-b"))

			condB := getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			gomega.Expect(condB.Reason).To(gomega.Equal(reasonCIDROverlap))
			gomega.Expect(condB.Message).To(gomega.ContainSubstring("vtep-a"))
		})

		ginkgo.It("converges without infinite re-queue loop when VTEPs overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.1.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			// After both VTEPs settle, the API action count must stabilize.
			// An infinite re-queue ping-pong would cause repeated reconciles;
			// while updateStatusCondition guards against redundant writes, the
			// re-queue guard in validateCIDRsAcrossVTEPs is what actually
			// prevents the loop. This verifies no further API calls are made.
			settled := len(fakeVTEP.Actions())
			gomega.Consistently(func() int {
				return len(fakeVTEP.Actions())
			}).WithTimeout(2 * time.Second).Should(gomega.Equal(settled))
		})

		ginkgo.It("emits a CIDROverlap warning event when VTEPs overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.1.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(fakeRecorder.Events).Should(gomega.Receive(
				gomega.ContainSubstring(reasonCIDROverlap),
			))
		})

		ginkgo.It("sets Accepted=False on all three VTEPs when CIDRs overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/8")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.1.0.0/24")
			vtepC := newVTEP("vtep-c", vtepv1.VTEPModeUnmanaged, "10.2.0.0/24")
			start(vtepA, vtepB, vtepC)

			for _, name := range []string{"vtep-a", "vtep-b", "vtep-c"} {
				gomega.Eventually(func() *metav1.Condition {
					return getVTEPCondition(fakeVTEP, name, conditionTypeAccepted)
				}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

				cond := getVTEPCondition(fakeVTEP, name, conditionTypeAccepted)
				gomega.Expect(cond.Reason).To(gomega.Equal(reasonCIDROverlap))
			}
		})

		ginkgo.It("updates conflict message when a new overlapping VTEP joins an existing conflict", func() {
			// vtep-b and vtep-c overlap via vtep-b's /8
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.0.0/8")
			vtepC := newVTEP("vtep-c", vtepv1.VTEPModeUnmanaged, "10.2.0.0/24")
			start(vtepB, vtepC)

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonCIDROverlap)),
			))

			// vtep-b's message should mention vtep-c but not vtep-a (doesn't exist yet)
			condB := getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			gomega.Expect(condB.Message).To(gomega.ContainSubstring("vtep-c"))
			gomega.Expect(condB.Message).NotTo(gomega.ContainSubstring("vtep-a"))

			// Create vtep-a which also overlaps with vtep-b
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.1.0.0/24")
			_, err := fakeVTEP.K8sV1().VTEPs().Create(context.Background(), vtepA, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// vtep-b's message should now mention both vtep-a and vtep-c
			gomega.Eventually(func() string {
				cond := getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
				if cond == nil {
					return ""
				}
				return cond.Message
			}).WithTimeout(5 * time.Second).Should(gomega.And(
				gomega.ContainSubstring("vtep-a"),
				gomega.ContainSubstring("vtep-c"),
			))
		})

		ginkgo.It("sets Accepted=False when a new overlapping VTEP is created after startup", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/16")
			start(vtepA)

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Create an overlapping VTEP
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.1.0/24")
			_, err := fakeVTEP.K8sV1().VTEPs().Create(context.Background(), vtepB, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Both should eventually be Accepted=False
			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))
		})

		ginkgo.It("sets Accepted=False on both when a mask expansion causes overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/24")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.1.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Expand vtep-a's mask from /24 to /16, now it contains 10.0.1.0/24
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-a", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.CIDRs = vtepv1.DualStackCIDRs{"10.0.0.0/16"}
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Both should eventually be Accepted=False
			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))
		})

		ginkgo.It("sets Accepted=False on both when a newly appended CIDR causes overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/24")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.1.0.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Append a new CIDR to vtep-a that overlaps with vtep-b
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-a", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.CIDRs = append(v.Spec.CIDRs, "10.1.0.0/16")
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Both should eventually be Accepted=False
			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))
		})

		ginkgo.It("clears Accepted=False only when all conflicts are resolved", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/8")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.1.0.0/24")
			vtepC := newVTEP("vtep-c", vtepv1.VTEPModeUnmanaged, "10.2.0.0/24")
			start(vtepA, vtepB, vtepC)

			// All three should be Accepted=False due to overlap
			for _, name := range []string{"vtep-a", "vtep-b", "vtep-c"} {
				gomega.Eventually(func() *metav1.Condition {
					return getVTEPCondition(fakeVTEP, name, conditionTypeAccepted)
				}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))
			}

			// Delete vtep-c: vtep-a and vtep-b still overlap, both stay Accepted=False
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-c", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			now := metav1.Now()
			v.DeletionTimestamp = &now
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Consistently(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(2 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Consistently(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(2 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			// Delete vtep-b: vtep-a is the only one left, no more conflicts
			v, err = fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-b", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			now = metav1.Now()
			v.DeletionTimestamp = &now
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})
	})

	ginkgo.Context("Node VTEP IP discovery", func() {
		ginkgo.It("discovers a single IPv4 VTEP IP and populates NodeAllocations", func() {
			node := newNodeWithHostCIDRs("node-1", "100.64.0.5/24", "192.168.1.10/24")
			vtep := newVTEP("vtep-discover", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() []vtepv1.NodeVTEPAllocation {
				return getNodeAllocations(fakeVTEP, "vtep-discover")
			}).WithTimeout(5 * time.Second).Should(gomega.ConsistOf(
				vtepv1.NodeVTEPAllocation{
					NodeName: "node-1",
					VTEPIPs:  vtepv1.DualStackIPs{"100.64.0.5"},
				},
			))

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-discover", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("skips nodes without host-cidrs annotation", func() {
			nodeNoAnnotation := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "bare-node"},
			}
			vtep := newVTEP("vtep-skip", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, nodeNoAnnotation)

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-skip", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			allocs := getNodeAllocations(fakeVTEP, "vtep-skip")
			gomega.Expect(allocs).To(gomega.BeEmpty())
		})

		ginkgo.It("sets Accepted=False when a node has no IP matching any VTEP CIDR", func() {
			node := newNodeWithHostCIDRs("node-nomatch", "192.168.1.10/24")
			vtep := newVTEP("vtep-nomatch", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-nomatch", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))

			cond := getVTEPCondition(fakeVTEP, "vtep-nomatch", conditionTypeAccepted)
			gomega.Expect(cond.Message).To(gomega.ContainSubstring("node-nomatch"))

			gomega.Eventually(fakeRecorder.Events).Should(gomega.Receive(
				gomega.ContainSubstring(reasonAllocationFailed),
			))
		})

		ginkgo.It("sets Accepted=False when a node has ambiguous VTEP IPs", func() {
			node := newNodeWithHostCIDRs("node-ambiguous", "100.64.0.5/24", "100.64.1.10/24")
			vtep := newVTEP("vtep-ambig", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-ambig", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))

			cond := getVTEPCondition(fakeVTEP, "vtep-ambig", conditionTypeAccepted)
			gomega.Expect(cond.Message).To(gomega.ContainSubstring("ambiguous"))
			gomega.Expect(cond.Message).To(gomega.ContainSubstring("node-ambiguous"))

			gomega.Eventually(fakeRecorder.Events).Should(gomega.Receive(
				gomega.ContainSubstring(reasonAllocationFailed),
			))
		})

		ginkgo.It("discovers VTEP IPs from multiple nodes", func() {
			node1 := newNodeWithHostCIDRs("node-1", "100.64.0.1/24")
			node2 := newNodeWithHostCIDRs("node-2", "100.64.0.2/24")
			node3 := newNodeWithHostCIDRs("node-3", "100.64.0.3/24")
			vtep := newVTEP("vtep-multi", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node1, node2, node3)

			gomega.Eventually(func() []vtepv1.NodeVTEPAllocation {
				return getNodeAllocations(fakeVTEP, "vtep-multi")
			}).WithTimeout(5 * time.Second).Should(gomega.ConsistOf(
				vtepv1.NodeVTEPAllocation{NodeName: "node-1", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.1"}},
				vtepv1.NodeVTEPAllocation{NodeName: "node-2", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.2"}},
				vtepv1.NodeVTEPAllocation{NodeName: "node-3", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.3"}},
			))

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-multi", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))
		})

		ginkgo.It("still updates valid nodes when some nodes fail discovery", func() {
			nodeGood := newNodeWithHostCIDRs("node-good", "100.64.0.1/24")
			nodeBad := newNodeWithHostCIDRs("node-bad", "100.64.0.5/24", "100.64.1.10/24")
			vtep := newVTEP("vtep-partial", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, nodeGood, nodeBad)

			gomega.Eventually(func() []vtepv1.NodeVTEPAllocation {
				return getNodeAllocations(fakeVTEP, "vtep-partial")
			}).WithTimeout(5 * time.Second).Should(gomega.ConsistOf(
				vtepv1.NodeVTEPAllocation{NodeName: "node-good", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.1"}},
			))

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-partial", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))
		})
	})

	ginkgo.Context("CUDN watch for finalizer re-evaluation", func() {
		ginkgo.It("indexes EVPN CUDNs on create and ignores non-EVPN CUDNs", func() {
			evpnCUDN := newCUDNWithEVPN("cudn-evpn", "vtep-indexed")
			nonEVPNCUDN := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: "cudn-plain"},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					NamespaceSelector: metav1.LabelSelector{},
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer3,
						Layer3: &udnv1.Layer3Config{
							Subnets: []udnv1.Layer3Subnet{{CIDR: "10.0.0.0/16"}},
						},
					},
				},
			}
			vtep := newVTEP("vtep-indexed", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep, evpnCUDN, nonEVPNCUDN)

			// EVPN CUDN should be indexed
			gomega.Eventually(func() bool {
				_, ok := controller.cudnVTEPIndex.Load("cudn-evpn")
				return ok
			}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())

			val, _ := controller.cudnVTEPIndex.Load("cudn-evpn")
			gomega.Expect(val).To(gomega.Equal("vtep-indexed"))

			// Non-EVPN CUDN should NOT be indexed
			_, ok := controller.cudnVTEPIndex.Load("cudn-plain")
			gomega.Expect(ok).To(gomega.BeFalse())
		})

		ginkgo.It("unblocks VTEP deletion when the referencing CUDN is deleted", func() {
			cudn := newCUDNWithEVPN("cudn-ref", "vtep-cudn-del")
			vtep := newVTEP("vtep-cudn-del", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep, cudn)

			gomega.Eventually(func() []string {
				return getVTEPFinalizers(fakeVTEP, "vtep-cudn-del")
			}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			// Simulate deletion of the VTEP (sets DeletionTimestamp, blocked by finalizer)
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-cudn-del", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			now := metav1.Now()
			v.DeletionTimestamp = &now
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Finalizer should remain because CUDN still references the VTEP
			gomega.Consistently(func() []string {
				return getVTEPFinalizers(fakeVTEP, "vtep-cudn-del")
			}).WithTimeout(3 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			// Delete the CUDN -- this should trigger the CUDN controller
			// which re-queues the deleting VTEP
			err = fakeClientset.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Delete(
				context.Background(), "cudn-ref", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Now the VTEP's finalizer should be removed
			gomega.Eventually(func() []string {
				return getVTEPFinalizers(fakeVTEP, "vtep-cudn-del")
			}).WithTimeout(5 * time.Second).ShouldNot(gomega.ContainElement(finalizerVTEP))

			// Index entry should be cleaned up
			_, ok := controller.cudnVTEPIndex.Load("cudn-ref")
			gomega.Expect(ok).To(gomega.BeFalse())
		})

		ginkgo.It("keeps VTEP blocked until all referencing CUDNs are deleted", func() {
			cudn1 := newCUDNWithEVPN("cudn-one", "vtep-multi-ref")
			cudn2 := newCUDNWithEVPN("cudn-two", "vtep-multi-ref")
			vtep := newVTEP("vtep-multi-ref", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep, cudn1, cudn2)

			gomega.Eventually(func() []string {
				return getVTEPFinalizers(fakeVTEP, "vtep-multi-ref")
			}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			// Request VTEP deletion
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-multi-ref", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			now := metav1.Now()
			v.DeletionTimestamp = &now
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Delete first CUDN -- VTEP should still be blocked by cudn-two
			err = fakeClientset.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Delete(
				context.Background(), "cudn-one", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Consistently(func() []string {
				return getVTEPFinalizers(fakeVTEP, "vtep-multi-ref")
			}).WithTimeout(3 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			// Delete second CUDN -- now VTEP should be unblocked
			err = fakeClientset.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Delete(
				context.Background(), "cudn-two", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() []string {
				return getVTEPFinalizers(fakeVTEP, "vtep-multi-ref")
			}).WithTimeout(5 * time.Second).ShouldNot(gomega.ContainElement(finalizerVTEP))
		})

		ginkgo.It("does not re-queue VTEPs when a non-EVPN CUDN is deleted", func() {
			evpnCUDN := newCUDNWithEVPN("cudn-evpn", "vtep-norequeue")
			nonEVPNCUDN := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: "cudn-plain"},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					NamespaceSelector: metav1.LabelSelector{},
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer3,
						Layer3: &udnv1.Layer3Config{
							Subnets: []udnv1.Layer3Subnet{{CIDR: "10.0.0.0/16"}},
						},
					},
				},
			}
			vtep := newVTEP("vtep-norequeue", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep, evpnCUDN, nonEVPNCUDN)

			gomega.Eventually(func() []string {
				return getVTEPFinalizers(fakeVTEP, "vtep-norequeue")
			}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			// Request VTEP deletion
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-norequeue", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			now := metav1.Now()
			v.DeletionTimestamp = &now
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Delete the non-EVPN CUDN -- should NOT unblock the VTEP
			err = fakeClientset.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Delete(
				context.Background(), "cudn-plain", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// VTEP should remain blocked (the EVPN CUDN still references it)
			gomega.Consistently(func() []string {
				return getVTEPFinalizers(fakeVTEP, "vtep-norequeue")
			}).WithTimeout(3 * time.Second).Should(gomega.ContainElement(finalizerVTEP))
		})

	})

	ginkgo.Context("Node watch for VTEP IP re-discovery", func() {
		ginkgo.It("updates NodeAllocations when a node's host-cidrs annotation is added", func() {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-late"}}
			vtep := newVTEP("vtep-node-add", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			// Node has no host-cidrs yet — it's silently skipped (not yet
			// OVN-managed), so VTEP is Accepted with empty allocations.
			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-node-add", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			gomega.Expect(getNodeAllocations(fakeVTEP, "vtep-node-add")).To(gomega.BeEmpty())

			// Simulate host-cidrs appearing on the node
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-late", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			annotation, _ := json.Marshal([]string{"100.64.0.10/24"})
			n.Annotations = map[string]string{"k8s.ovn.org/host-cidrs": string(annotation)}
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Node controller detects host-cidrs change, re-queues VTEP,
			// which now discovers the IP.
			gomega.Eventually(func() []vtepv1.NodeVTEPAllocation {
				return getNodeAllocations(fakeVTEP, "vtep-node-add")
			}).WithTimeout(5 * time.Second).Should(gomega.ConsistOf(
				vtepv1.NodeVTEPAllocation{NodeName: "node-late", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.10"}},
			))
		})

		ginkgo.It("updates NodeAllocations when a node's host-cidrs annotation changes", func() {
			node := newNodeWithHostCIDRs("node-change", "100.64.0.1/24")
			vtep := newVTEP("vtep-node-change", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() []vtepv1.NodeVTEPAllocation {
				return getNodeAllocations(fakeVTEP, "vtep-node-change")
			}).WithTimeout(5 * time.Second).Should(gomega.ConsistOf(
				vtepv1.NodeVTEPAllocation{NodeName: "node-change", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.1"}},
			))

			// Change the node's host-cidrs to a different IP
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-change", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			annotation, _ := json.Marshal([]string{"100.64.0.99/24"})
			n.Annotations["k8s.ovn.org/host-cidrs"] = string(annotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// NodeAllocations should reflect the new IP
			gomega.Eventually(func() []vtepv1.NodeVTEPAllocation {
				return getNodeAllocations(fakeVTEP, "vtep-node-change")
			}).WithTimeout(5 * time.Second).Should(gomega.ConsistOf(
				vtepv1.NodeVTEPAllocation{NodeName: "node-change", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.99"}},
			))
		})

		ginkgo.It("removes node from NodeAllocations when node is deleted", func() {
			node1 := newNodeWithHostCIDRs("node-keep", "100.64.0.1/24")
			node2 := newNodeWithHostCIDRs("node-remove", "100.64.0.2/24")
			vtep := newVTEP("vtep-node-del", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node1, node2)

			gomega.Eventually(func() []vtepv1.NodeVTEPAllocation {
				return getNodeAllocations(fakeVTEP, "vtep-node-del")
			}).WithTimeout(5 * time.Second).Should(gomega.HaveLen(2))

			// Delete node-remove
			err := fakeClientset.KubeClient.CoreV1().Nodes().Delete(
				context.Background(), "node-remove", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Only node-keep should remain
			gomega.Eventually(func() []vtepv1.NodeVTEPAllocation {
				return getNodeAllocations(fakeVTEP, "vtep-node-del")
			}).WithTimeout(5 * time.Second).Should(gomega.ConsistOf(
				vtepv1.NodeVTEPAllocation{NodeName: "node-keep", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.1"}},
			))
		})

		ginkgo.It("recovers from AllocationFailed when no-match host-cidrs are fixed", func() {
			// Node starts with an IP outside the VTEP CIDR
			node := newNodeWithHostCIDRs("node-nomatch", "192.168.1.10/24")
			vtep := newVTEP("vtep-nomatch-fix", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-nomatch-fix", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))

			// Fix the node: add a matching IP
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-nomatch", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			annotation, _ := json.Marshal([]string{"100.64.0.20/24"})
			n.Annotations["k8s.ovn.org/host-cidrs"] = string(annotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-nomatch-fix", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			gomega.Eventually(func() []vtepv1.NodeVTEPAllocation {
				return getNodeAllocations(fakeVTEP, "vtep-nomatch-fix")
			}).WithTimeout(5 * time.Second).Should(gomega.ConsistOf(
				vtepv1.NodeVTEPAllocation{NodeName: "node-nomatch", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.20"}},
			))
		})

		ginkgo.It("recovers from AllocationFailed when ambiguous host-cidrs are fixed", func() {
			// Node starts with two IPs in the VTEP CIDR — ambiguous
			node := newNodeWithHostCIDRs("node-ambig", "100.64.0.5/24", "100.64.1.10/24")
			vtep := newVTEP("vtep-recover", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-recover", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))

			// Fix the node: update host-cidrs to a single matching IP
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-ambig", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			annotation, _ := json.Marshal([]string{"100.64.0.5/24"})
			n.Annotations["k8s.ovn.org/host-cidrs"] = string(annotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// VTEP should recover to Accepted=True
			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-recover", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			gomega.Eventually(func() []vtepv1.NodeVTEPAllocation {
				return getNodeAllocations(fakeVTEP, "vtep-recover")
			}).WithTimeout(5 * time.Second).Should(gomega.ConsistOf(
				vtepv1.NodeVTEPAllocation{NodeName: "node-ambig", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.5"}},
			))
		})

		ginkgo.It("does not issue any VTEP API update when host-cidrs change is unrelated", func() {
			var patchCount atomic.Int32
			node := newNodeWithHostCIDRs("node-stable", "100.64.0.1/24", "192.168.1.10/24")
			vtep := newVTEP("vtep-stable", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			fakeVTEP.PrependReactor("patch", "vteps", func(_ ktesting.Action) (bool, runtime.Object, error) {
				patchCount.Add(1)
				return false, nil, nil
			})

			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-stable", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
			gomega.Eventually(func() []vtepv1.NodeVTEPAllocation {
				return getNodeAllocations(fakeVTEP, "vtep-stable")
			}).WithTimeout(5 * time.Second).Should(gomega.ConsistOf(
				vtepv1.NodeVTEPAllocation{NodeName: "node-stable", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.1"}},
			))

			// Confirm the initial reconcile did issue API updates
			initialPatches := patchCount.Load()
			gomega.Expect(initialPatches).To(gomega.BeNumerically(">", int32(0)))

			// Reset counter before the unrelated change
			patchCount.Store(0)

			// Update host-cidrs with a different non-matching IP (VTEP IP unchanged)
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-stable", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			annotation, _ := json.Marshal([]string{"100.64.0.1/24", "10.0.0.50/24"})
			n.Annotations["k8s.ovn.org/host-cidrs"] = string(annotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// The diff guards should prevent any VTEP status API calls
			gomega.Consistently(func() int32 {
				return patchCount.Load()
			}).WithTimeout(3 * time.Second).Should(gomega.Equal(int32(0)))
		})

		ginkgo.It("reconciles multiple VTEPs when a single node's host-cidrs changes", func() {
			node := newNodeWithHostCIDRs("node-shared", "100.64.0.1/24", "200.10.0.1/24")
			vtepA := newVTEP("vtep-a-multi", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			vtepB := newVTEP("vtep-b-multi", vtepv1.VTEPModeUnmanaged, "200.10.0.0/16")
			start(vtepA, vtepB, node)

			// Both VTEPs should settle with allocations from the same node
			for _, name := range []string{"vtep-a-multi", "vtep-b-multi"} {
				gomega.Eventually(func() *metav1.Condition {
					return getVTEPCondition(fakeVTEP, name, conditionTypeAccepted)
				}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
					gomega.HaveField("Status", metav1.ConditionTrue),
					gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
				))
			}
			gomega.Expect(getNodeAllocations(fakeVTEP, "vtep-a-multi")).To(gomega.ConsistOf(
				vtepv1.NodeVTEPAllocation{NodeName: "node-shared", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.1"}},
			))
			gomega.Expect(getNodeAllocations(fakeVTEP, "vtep-b-multi")).To(gomega.ConsistOf(
				vtepv1.NodeVTEPAllocation{NodeName: "node-shared", VTEPIPs: vtepv1.DualStackIPs{"200.10.0.1"}},
			))

			// Change the node IPs: both VTEP ranges get new addresses
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-shared", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			annotation, _ := json.Marshal([]string{"100.64.0.99/24", "200.10.0.99/24"})
			n.Annotations["k8s.ovn.org/host-cidrs"] = string(annotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Both VTEPs should pick up the new IPs
			gomega.Eventually(func() []vtepv1.NodeVTEPAllocation {
				return getNodeAllocations(fakeVTEP, "vtep-a-multi")
			}).WithTimeout(5 * time.Second).Should(gomega.ConsistOf(
				vtepv1.NodeVTEPAllocation{NodeName: "node-shared", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.99"}},
			))
			gomega.Eventually(func() []vtepv1.NodeVTEPAllocation {
				return getNodeAllocations(fakeVTEP, "vtep-b-multi")
			}).WithTimeout(5 * time.Second).Should(gomega.ConsistOf(
				vtepv1.NodeVTEPAllocation{NodeName: "node-shared", VTEPIPs: vtepv1.DualStackIPs{"200.10.0.99"}},
			))
		})

		ginkgo.It("allocates a dynamically created node with host-cidrs already set", func() {
			vtep := newVTEP("vtep-dynnode", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep)

			// No nodes exist yet — VTEP should be Accepted with empty allocations
			gomega.Eventually(func() *metav1.Condition {
				return getVTEPCondition(fakeVTEP, "vtep-dynnode", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
			gomega.Expect(getNodeAllocations(fakeVTEP, "vtep-dynnode")).To(gomega.BeEmpty())

			// Dynamically create a node that already has host-cidrs
			node := newNodeWithHostCIDRs("node-late", "100.64.0.77/24")
			_, err := fakeClientset.KubeClient.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() []vtepv1.NodeVTEPAllocation {
				return getNodeAllocations(fakeVTEP, "vtep-dynnode")
			}).WithTimeout(5 * time.Second).Should(gomega.ConsistOf(
				vtepv1.NodeVTEPAllocation{NodeName: "node-late", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.77"}},
			))
		})

		ginkgo.It("updates only the affected node's allocation when one of many nodes changes", func() {
			node1 := newNodeWithHostCIDRs("node-m1", "100.64.0.1/24")
			node2 := newNodeWithHostCIDRs("node-m2", "100.64.0.2/24")
			node3 := newNodeWithHostCIDRs("node-m3", "100.64.0.3/24")
			vtep := newVTEP("vtep-mnodes", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node1, node2, node3)

			gomega.Eventually(func() []vtepv1.NodeVTEPAllocation {
				return getNodeAllocations(fakeVTEP, "vtep-mnodes")
			}).WithTimeout(5 * time.Second).Should(gomega.ConsistOf(
				vtepv1.NodeVTEPAllocation{NodeName: "node-m1", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.1"}},
				vtepv1.NodeVTEPAllocation{NodeName: "node-m2", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.2"}},
				vtepv1.NodeVTEPAllocation{NodeName: "node-m3", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.3"}},
			))

			// Change only node-m2's IP
			n2, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-m2", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			annotation, _ := json.Marshal([]string{"100.64.0.22/24"})
			n2.Annotations["k8s.ovn.org/host-cidrs"] = string(annotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n2, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// node-m2 should get the new IP; node-m1 and node-m3 stay unchanged
			gomega.Eventually(func() []vtepv1.NodeVTEPAllocation {
				return getNodeAllocations(fakeVTEP, "vtep-mnodes")
			}).WithTimeout(5 * time.Second).Should(gomega.ConsistOf(
				vtepv1.NodeVTEPAllocation{NodeName: "node-m1", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.1"}},
				vtepv1.NodeVTEPAllocation{NodeName: "node-m2", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.22"}},
				vtepv1.NodeVTEPAllocation{NodeName: "node-m3", VTEPIPs: vtepv1.DualStackIPs{"100.64.0.3"}},
			))

			// Condition should remain Accepted=True throughout
			gomega.Expect(getVTEPCondition(fakeVTEP, "vtep-mnodes", conditionTypeAccepted)).To(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})
	})
})

var _ = ginkgo.Describe("vtepNameInMessage", func() {
	ginkgo.It("returns false for empty brackets", func() {
		gomega.Expect(vtepNameInMessage("CIDRs overlap with VTEPs: []", "vtep-a")).To(gomega.BeFalse())
	})

	ginkgo.It("returns false when no brackets present", func() {
		gomega.Expect(vtepNameInMessage("no brackets here", "vtep-a")).To(gomega.BeFalse())
	})

	ginkgo.It("matches single entry", func() {
		msg := "CIDRs overlap with VTEPs: [vtep-a]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-a")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeFalse())
	})

	ginkgo.It("matches in two entries", func() {
		msg := "CIDRs overlap with VTEPs: [vtep-a, vtep-b]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-a")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-c")).To(gomega.BeFalse())
	})

	ginkgo.It("matches in three entries", func() {
		msg := "CIDRs overlap with VTEPs: [vtep-a, vtep-b, vtep-c]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-a")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-c")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-d")).To(gomega.BeFalse())
	})

	ginkgo.It("does not substring match", func() {
		msg := "CIDRs overlap with VTEPs: [vtep-bb]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeFalse())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bb")).To(gomega.BeTrue())
	})

	ginkgo.It("does not substring match in multi-entry list", func() {
		msg := "CIDRs overlap with VTEPs: [vtep-aa, vtep-bb]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-a")).To(gomega.BeFalse())
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeFalse())
		gomega.Expect(vtepNameInMessage(msg, "vtep-aa")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bb")).To(gomega.BeTrue())
	})

	ginkgo.It("does not substring match vtep-b against vtep-bb and vtep-bbb", func() {
		msg := "CIDRs overlap with VTEPs: [vtep-b, vtep-bb, vtep-bbb]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bb")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bbb")).To(gomega.BeTrue())

		msg = "CIDRs overlap with VTEPs: [vtep-bb, vtep-bbb]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeFalse())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bb")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bbb")).To(gomega.BeTrue())

		msg = "CIDRs overlap with VTEPs: [vtep-bbb]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeFalse())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bb")).To(gomega.BeFalse())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bbb")).To(gomega.BeTrue())
	})
})
