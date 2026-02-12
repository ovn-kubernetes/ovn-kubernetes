package notifier

import (
	"context"
	"maps"
	"strconv"
	"sync"

	netv1fake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	frrfake "github.com/metallb/frr-k8s/pkg/client/clientset/versioned/fake"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	rafake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned/fake"
	udnv1fake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepv1fake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("VTEPNotifier", func() {
	var (
		vtepClient       *vtepv1fake.Clientset
		wf               *factory.WatchFactory
		testVTEPNotifier *VTEPNotifier
	)

	BeforeEach(func() {
		vtepClient = vtepv1fake.NewSimpleClientset()

		// enable features to make watch-factory start the VTEP informer
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableRouteAdvertisements = true
		config.OVNKubernetesFeature.EnableEVPN = true
		fakeClient := &util.OVNClusterManagerClientset{
			KubeClient:                fake.NewSimpleClientset(),
			NetworkAttchDefClient:     netv1fake.NewSimpleClientset(),
			UserDefinedNetworkClient:  udnv1fake.NewSimpleClientset(),
			RouteAdvertisementsClient: rafake.NewSimpleClientset(),
			FRRClient:                 frrfake.NewSimpleClientset(),
			VTEPClient:                vtepClient,
		}
		var err error
		wf, err = factory.NewClusterManagerWatchFactory(fakeClient)
		Expect(err).NotTo(HaveOccurred())
		Expect(wf.Start()).To(Succeed())
	})

	AfterEach(func() {
		wf.Shutdown()
	})

	var s *testVTEPSubscriber

	BeforeEach(func() {
		s = &testVTEPSubscriber{reconciledKeys: map[string]int64{}}
		testVTEPNotifier = NewVTEPNotifier(wf.VTEPInformer(), s)
		Expect(controller.Start(testVTEPNotifier.Controller)).Should(Succeed())

		// create test VTEPs
		for i := 0; i < 3; i++ {
			vtepName := "test-vtep-" + strconv.Itoa(i)
			_, err := vtepClient.K8sV1().VTEPs().Create(context.Background(), testVTEP(vtepName), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
	})

	AfterEach(func() {
		if testVTEPNotifier != nil {
			controller.Stop(testVTEPNotifier.Controller)
		}
	})

	It("should notify VTEP create events", func() {
		Eventually(func() map[string]int64 {
			return s.GetReconciledKeys()
		}).Should(Equal(map[string]int64{
			"test-vtep-0": 1,
			"test-vtep-1": 1,
			"test-vtep-2": 1,
		}))
	})

	It("should notify VTEP delete events", func() {
		Eventually(func() map[string]int64 {
			return s.GetReconciledKeys()
		}).Should(Equal(map[string]int64{
			"test-vtep-0": 1,
			"test-vtep-1": 1,
			"test-vtep-2": 1,
		}))

		Expect(vtepClient.K8sV1().VTEPs().Delete(context.Background(), "test-vtep-2", metav1.DeleteOptions{})).To(Succeed())
		Expect(vtepClient.K8sV1().VTEPs().Delete(context.Background(), "test-vtep-0", metav1.DeleteOptions{})).To(Succeed())

		Eventually(func() map[string]int64 {
			return s.GetReconciledKeys()
		}).Should(Equal(map[string]int64{
			"test-vtep-0": 2,
			"test-vtep-1": 1,
			"test-vtep-2": 2,
		}), "should record additional two events, following VTEP deletion")
	})

	It("should NOT notify VTEP update events (spec/status changes)", func() {
		Eventually(func() map[string]int64 {
			return s.GetReconciledKeys()
		}).Should(Equal(map[string]int64{
			"test-vtep-0": 1,
			"test-vtep-1": 1,
			"test-vtep-2": 1,
		}))

		// Update VTEP spec (change CIDRs)
		vtep, err := vtepClient.K8sV1().VTEPs().Get(context.Background(), "test-vtep-1", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		vtep.Spec.CIDRs = vtepv1.DualStackCIDRs{"192.168.0.0/24"}
		_, err = vtepClient.K8sV1().VTEPs().Update(context.Background(), vtep, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Updates should NOT trigger notification (needUpdate returns false for updates)
		Consistently(func() map[string]int64 {
			return s.GetReconciledKeys()
		}).Should(Equal(map[string]int64{
			"test-vtep-0": 1,
			"test-vtep-1": 1,
			"test-vtep-2": 1,
		}), "should NOT record additional events following VTEP update")
	})

	It("should notify multiple subscribers", func() {
		// Stop the single-subscriber notifier
		controller.Stop(testVTEPNotifier.Controller)

		// Create a second subscriber
		s2 := &testVTEPSubscriber{reconciledKeys: map[string]int64{}}

		// Create a new notifier with multiple subscribers
		testVTEPNotifier = NewVTEPNotifier(wf.VTEPInformer(), s, s2)
		Expect(controller.Start(testVTEPNotifier.Controller)).Should(Succeed())

		// Create a new VTEP
		_, err := vtepClient.K8sV1().VTEPs().Create(context.Background(), testVTEP("test-vtep-new"), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Both subscribers should be notified exactly once
		Eventually(func(g Gomega) {
			keys1 := s.GetReconciledKeys()
			keys2 := s2.GetReconciledKeys()
			g.Expect(keys1["test-vtep-new"]).To(BeEquivalentTo(1), "subscriber 1 should be notified exactly once")
			g.Expect(keys2["test-vtep-new"]).To(BeEquivalentTo(1), "subscriber 2 should be notified exactly once")
		}).Should(Succeed())
	})
})

func testVTEP(name string) *vtepv1.VTEP {
	return &vtepv1.VTEP{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: vtepv1.VTEPSpec{
			CIDRs: vtepv1.DualStackCIDRs{"10.10.10.0/24"},
			Mode:  vtepv1.VTEPModeManaged,
		},
	}
}

type testVTEPSubscriber struct {
	err            error
	reconciledKeys map[string]int64
	lock           sync.RWMutex
}

func (s *testVTEPSubscriber) ReconcileVTEP(key string) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.reconciledKeys[key]++
	return s.err
}

func (s *testVTEPSubscriber) GetReconciledKeys() map[string]int64 {
	s.lock.RLock()
	defer s.lock.RUnlock()

	cp := map[string]int64{}
	maps.Copy(cp, s.reconciledKeys)
	return cp
}
