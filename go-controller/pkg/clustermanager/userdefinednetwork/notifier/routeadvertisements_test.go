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
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	rafake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned/fake"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1fake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	vtepv1fake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("RouteAdvertisementsNotifier", func() {
	var (
		raClient     *rafake.Clientset
		wf           *factory.WatchFactory
		testNotifier *RouteAdvertisementsNotifier
	)

	BeforeEach(func() {
		raClient = rafake.NewSimpleClientset()

		// enable features to make watch-factory start the RouteAdvertisements informer
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableRouteAdvertisements = true
		fakeClient := &util.OVNClusterManagerClientset{
			KubeClient:                fake.NewSimpleClientset(),
			NetworkAttchDefClient:     netv1fake.NewSimpleClientset(),
			UserDefinedNetworkClient:  udnv1fake.NewSimpleClientset(),
			RouteAdvertisementsClient: raClient,
			FRRClient:                 frrfake.NewSimpleClientset(),
			VTEPClient:                vtepv1fake.NewSimpleClientset(),
		}
		var err error
		wf, err = factory.NewClusterManagerWatchFactory(fakeClient)
		Expect(err).NotTo(HaveOccurred())
		Expect(wf.Start()).To(Succeed())
	})

	AfterEach(func() {
		wf.Shutdown()
	})

	var s *testRASubscriber

	BeforeEach(func() {
		s = &testRASubscriber{reconciledKeys: map[string]int64{}}
		testNotifier = NewRouteAdvertisementsNotifier(wf.RouteAdvertisementsInformer(), s)
		Expect(controller.Start(testNotifier.Controller)).Should(Succeed())

		// create test RouteAdvertisements
		for i := 0; i < 3; i++ {
			raName := "test-ra-" + strconv.Itoa(i)
			_, err := raClient.K8sV1().RouteAdvertisements().Create(context.Background(), testRouteAdvertisement(raName, false), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
	})

	AfterEach(func() {
		if testNotifier != nil {
			controller.Stop(testNotifier.Controller)
		}
	})

	It("should notify RouteAdvertisements create events", func() {
		Eventually(func() map[string]int64 {
			return s.GetReconciledKeys()
		}).Should(Equal(map[string]int64{
			"test-ra-0": 1,
			"test-ra-1": 1,
			"test-ra-2": 1,
		}))
	})

	It("should notify RouteAdvertisements delete events", func() {
		Eventually(func() map[string]int64 {
			return s.GetReconciledKeys()
		}).Should(Equal(map[string]int64{
			"test-ra-0": 1,
			"test-ra-1": 1,
			"test-ra-2": 1,
		}))

		Expect(raClient.K8sV1().RouteAdvertisements().Delete(context.Background(), "test-ra-2", metav1.DeleteOptions{})).To(Succeed())
		Expect(raClient.K8sV1().RouteAdvertisements().Delete(context.Background(), "test-ra-0", metav1.DeleteOptions{})).To(Succeed())

		Eventually(func() map[string]int64 {
			return s.GetReconciledKeys()
		}).Should(Equal(map[string]int64{
			"test-ra-0": 2,
			"test-ra-1": 1,
			"test-ra-2": 2,
		}), "should record additional two events, following RouteAdvertisements deletion")
	})

	Context("needUpdate logic", func() {
		It("should notify when NetworkSelectors change", func() {
			Eventually(func() map[string]int64 {
				return s.GetReconciledKeys()
			}).Should(Equal(map[string]int64{
				"test-ra-0": 1,
				"test-ra-1": 1,
				"test-ra-2": 1,
			}))

			// Update NetworkSelectors
			ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.Background(), "test-ra-1", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			ra.Spec.NetworkSelectors = []apitypes.NetworkSelector{
				{
					NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
					ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
						NetworkSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{"new-label": "new-value"},
						},
					},
				},
			}
			_, err = raClient.K8sV1().RouteAdvertisements().Update(context.Background(), ra, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() map[string]int64 {
				return s.GetReconciledKeys()
			}).Should(Equal(map[string]int64{
				"test-ra-0": 1,
				"test-ra-1": 2,
				"test-ra-2": 1,
			}), "should notify when NetworkSelectors change")
		})

		It("should notify when Advertisements change", func() {
			Eventually(func() map[string]int64 {
				return s.GetReconciledKeys()
			}).Should(Equal(map[string]int64{
				"test-ra-0": 1,
				"test-ra-1": 1,
				"test-ra-2": 1,
			}))

			// Update Advertisements
			ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.Background(), "test-ra-1", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			ra.Spec.Advertisements = []ratypes.AdvertisementType{ratypes.PodNetwork, ratypes.EgressIP}
			_, err = raClient.K8sV1().RouteAdvertisements().Update(context.Background(), ra, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() map[string]int64 {
				return s.GetReconciledKeys()
			}).Should(Equal(map[string]int64{
				"test-ra-0": 1,
				"test-ra-1": 2,
				"test-ra-2": 1,
			}), "should notify when Advertisements change")
		})

		It("should notify when Accepted condition changes from False to True", func() {
			Eventually(func() map[string]int64 {
				return s.GetReconciledKeys()
			}).Should(Equal(map[string]int64{
				"test-ra-0": 1,
				"test-ra-1": 1,
				"test-ra-2": 1,
			}))

			// Update Status to add Accepted=True condition
			ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.Background(), "test-ra-1", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			ra.Status.Conditions = []metav1.Condition{
				{
					Type:               ratypes.RouteAdvertisementsAccepted,
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					Reason:             "Valid",
					Message:            "RouteAdvertisements configuration is valid",
				},
			}
			_, err = raClient.K8sV1().RouteAdvertisements().UpdateStatus(context.Background(), ra, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() map[string]int64 {
				return s.GetReconciledKeys()
			}).Should(Equal(map[string]int64{
				"test-ra-0": 1,
				"test-ra-1": 2,
				"test-ra-2": 1,
			}), "should notify when Accepted condition changes from missing to True")
		})

		It("should notify when Accepted condition changes from True to False", func() {
			// Create RA with Accepted=True
			_, err := raClient.K8sV1().RouteAdvertisements().Create(context.Background(), testRouteAdvertisement("test-ra-accepted", true), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() map[string]int64 {
				return s.GetReconciledKeys()
			}).Should(HaveKey("test-ra-accepted"))

			// Change Accepted to False
			ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.Background(), "test-ra-accepted", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			ra.Status.Conditions = []metav1.Condition{
				{
					Type:               ratypes.RouteAdvertisementsAccepted,
					Status:             metav1.ConditionFalse,
					LastTransitionTime: metav1.Now(),
					Reason:             "Invalid",
					Message:            "Configuration is invalid",
				},
			}
			_, err = raClient.K8sV1().RouteAdvertisements().UpdateStatus(context.Background(), ra, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() int64 {
				return s.GetReconciledKeys()["test-ra-accepted"]
			}).Should(BeEquivalentTo(2), "should notify when Accepted condition changes from True to False")
		})

		It("should NOT notify when irrelevant spec fields change", func() {
			Eventually(func() map[string]int64 {
				return s.GetReconciledKeys()
			}).Should(Equal(map[string]int64{
				"test-ra-0": 1,
				"test-ra-1": 1,
				"test-ra-2": 1,
			}))

			// Update TargetVRF (should NOT trigger notification)
			ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.Background(), "test-ra-1", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			ra.Spec.TargetVRF = "new-vrf"
			_, err = raClient.K8sV1().RouteAdvertisements().Update(context.Background(), ra, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Consistently(func() map[string]int64 {
				return s.GetReconciledKeys()
			}).Should(Equal(map[string]int64{
				"test-ra-0": 1,
				"test-ra-1": 1,
				"test-ra-2": 1,
			}), "should NOT notify when irrelevant fields change")
		})

		It("should NOT notify when non-Accepted conditions change", func() {
			Eventually(func() map[string]int64 {
				return s.GetReconciledKeys()
			}).Should(Equal(map[string]int64{
				"test-ra-0": 1,
				"test-ra-1": 1,
				"test-ra-2": 1,
			}))

			// Add a different condition (should NOT trigger notification)
			ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.Background(), "test-ra-1", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			ra.Status.Conditions = []metav1.Condition{
				{
					Type:               "SomeOtherCondition",
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					Reason:             "Test",
					Message:            "This is a test condition",
				},
			}
			_, err = raClient.K8sV1().RouteAdvertisements().UpdateStatus(context.Background(), ra, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Consistently(func() map[string]int64 {
				return s.GetReconciledKeys()
			}).Should(Equal(map[string]int64{
				"test-ra-0": 1,
				"test-ra-1": 1,
				"test-ra-2": 1,
			}), "should NOT notify when non-Accepted conditions change")
		})
	})

	It("should notify multiple subscribers", func() {
		// Stop the single-subscriber notifier
		controller.Stop(testNotifier.Controller)

		// Create a second subscriber
		s2 := &testRASubscriber{reconciledKeys: map[string]int64{}}

		// Create a new notifier with multiple subscribers
		testNotifier = NewRouteAdvertisementsNotifier(wf.RouteAdvertisementsInformer(), s, s2)
		Expect(controller.Start(testNotifier.Controller)).Should(Succeed())

		// Create a new RouteAdvertisement
		_, err := raClient.K8sV1().RouteAdvertisements().Create(context.Background(), testRouteAdvertisement("test-ra-new", false), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Both subscribers should be notified exactly once
		Eventually(func(g Gomega) {
			keys1 := s.GetReconciledKeys()
			keys2 := s2.GetReconciledKeys()
			g.Expect(keys1["test-ra-new"]).To(BeEquivalentTo(1), "subscriber 1 should be notified exactly once")
			g.Expect(keys2["test-ra-new"]).To(BeEquivalentTo(1), "subscriber 2 should be notified exactly once")
		}).Should(Succeed())
	})
})

func testRouteAdvertisement(name string, accepted bool) *ratypes.RouteAdvertisements {
	ra := &ratypes.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: ratypes.RouteAdvertisementsSpec{
			NetworkSelectors: []apitypes.NetworkSelector{
				{
					NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
					ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
						NetworkSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{"test-label": "test-value"},
						},
					},
				},
			},
			NodeSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"node-label": "node-value"},
			},
			FRRConfigurationSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"frr-label": "frr-value"},
			},
			Advertisements: []ratypes.AdvertisementType{ratypes.PodNetwork},
		},
	}

	if accepted {
		ra.Status.Conditions = []metav1.Condition{
			{
				Type:               ratypes.RouteAdvertisementsAccepted,
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
				Reason:             "Valid",
				Message:            "RouteAdvertisements configuration is valid",
			},
		}
	}

	return ra
}

type testRASubscriber struct {
	err            error
	reconciledKeys map[string]int64
	lock           sync.RWMutex
}

func (s *testRASubscriber) ReconcileRouteAdvertisements(key string) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.reconciledKeys[key]++
	return s.err
}

func (s *testRASubscriber) GetReconciledKeys() map[string]int64 {
	s.lock.RLock()
	defer s.lock.RUnlock()

	cp := map[string]int64{}
	maps.Copy(cp, s.reconciledKeys)
	return cp
}
