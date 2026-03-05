package vtep

import (
	"context"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
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
		controller   *Controller
		fakeVTEP     *vtepfake.Clientset
		wf           *factory.WatchFactory
		fakeRecorder *record.FakeRecorder
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
		fakeRecorder = record.NewFakeRecorder(10)

		fakeClientset := util.GetOVNClientset(otherObjects...).GetClusterManagerClientset()
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
})
