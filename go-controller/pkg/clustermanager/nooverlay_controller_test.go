package clustermanager

import (
	"context"
	"time"

	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	frrfake "github.com/metallb/frr-k8s/pkg/client/clientset/versioned/fake"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ratypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	rafake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned/fake"
	apitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// Test helper types
type testRA struct {
	Name             string
	SelectsDefault   bool
	AdvertisePods    bool
	AcceptedStatus   *metav1.ConditionStatus
	NetworkSelectors []apitypes.NetworkSelector
	Advertisements   []ratypes.AdvertisementType
}

func (tra testRA) RouteAdvertisements() *ratypes.RouteAdvertisements {
	ra := &ratypes.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{
			Name: tra.Name,
		},
		Spec: ratypes.RouteAdvertisementsSpec{
			NetworkSelectors: tra.NetworkSelectors,
			Advertisements:   tra.Advertisements,
		},
	}

	// Handle simple case flags
	if tra.SelectsDefault && len(tra.NetworkSelectors) == 0 {
		ra.Spec.NetworkSelectors = append(ra.Spec.NetworkSelectors, apitypes.NetworkSelector{
			NetworkSelectionType: apitypes.DefaultNetwork,
		})
	}
	if tra.AdvertisePods && len(tra.Advertisements) == 0 {
		ra.Spec.Advertisements = append(ra.Spec.Advertisements, ratypes.PodNetwork)
	}

	if tra.AcceptedStatus != nil {
		ra.Status.Conditions = []metav1.Condition{
			{
				Type:   "Accepted",
				Status: *tra.AcceptedStatus,
			},
		}
	}

	return ra
}

var _ = ginkgo.Describe("No-Overlay Controller", func() {
	var (
		recorder *record.FakeRecorder
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		recorder = record.NewFakeRecorder(100)
		// Enable multi-network and route advertisements features
		// These are required for the no-overlay controller to function
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableRouteAdvertisements = true
	})

	ginkgo.Context("Controller creation", func() {
		ginkgo.It("should create controller when transport is no-overlay", func() {
			config.Default.Transport = config.TransportNoOverlay

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			controller, err := newNoOverlayController(wf, recorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(controller).NotTo(gomega.BeNil())
			gomega.Expect(controller.wf).To(gomega.Equal(wf))
			gomega.Expect(controller.recorder).To(gomega.Equal(recorder))
			gomega.Expect(controller.raController).NotTo(gomega.BeNil())
		})

		ginkgo.It("should have empty last validation error on creation", func() {
			config.Default.Transport = config.TransportNoOverlay

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			controller, err := newNoOverlayController(wf, recorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(controller.lastValidationError).To(gomega.BeEmpty())
		})
	})

	ginkgo.Context("Validation logic", func() {
		tests := []struct {
			name                 string
			ras                  []*testRA
			expectError          bool
			expectErrorSubstring string
		}{
			{
				name:                 "should fail when no RouteAdvertisements CR exists",
				ras:                  []*testRA{},
				expectError:          true,
				expectErrorSubstring: "no RouteAdvertisements CR is advertising the default network",
			},
			{
				name: "should pass when valid RouteAdvertisements CR exists",
				ras: []*testRA{
					{
						Name:           "test-ra",
						SelectsDefault: true,
						AdvertisePods:  true,
						AcceptedStatus: ptr.To(metav1.ConditionTrue),
					},
				},
				expectError: false,
			},
			{
				name: "should fail when RouteAdvertisements CR exists but not Accepted",
				ras: []*testRA{
					{
						Name:           "test-ra",
						SelectsDefault: true,
						AdvertisePods:  true,
						AcceptedStatus: ptr.To(metav1.ConditionFalse),
					},
				},
				expectError:          true,
				expectErrorSubstring: "but none have status Accepted=True",
			},
			{
				name: "should fail when RouteAdvertisements CR doesn't advertise PodNetwork",
				ras: []*testRA{
					{
						Name:           "test-ra",
						SelectsDefault: true,
						AdvertisePods:  false,
						Advertisements: []ratypes.AdvertisementType{ratypes.EgressIP},
						AcceptedStatus: ptr.To(metav1.ConditionTrue),
					},
				},
				expectError:          true,
				expectErrorSubstring: "no RouteAdvertisements CR is advertising the default network",
			},
			{
				name: "should pass when multiple RouteAdvertisements exist and one is valid",
				ras: []*testRA{
					{
						Name: "test-ra-1",
						NetworkSelectors: []apitypes.NetworkSelector{
							{NetworkSelectionType: apitypes.NetworkSelectionType("CustomNetwork")},
						},
						AdvertisePods:  true,
						AcceptedStatus: ptr.To(metav1.ConditionTrue),
					},
					{
						Name:           "test-ra-2",
						SelectsDefault: true,
						AdvertisePods:  true,
						AcceptedStatus: ptr.To(metav1.ConditionTrue),
					},
				},
				expectError: false,
			},
		}

		for _, tt := range tests {
			ginkgo.It(tt.name, func() {
				config.Default.Transport = config.TransportNoOverlay

				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient:                fake.NewSimpleClientset(),
					NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
					RouteAdvertisementsClient: rafake.NewSimpleClientset(),
					FRRClient:                 frrfake.NewSimpleClientset(),
				}

				// Create RAs with both spec and status
				for _, tra := range tt.ras {
					ra := tra.RouteAdvertisements()
					_, err := fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(
						context.Background(), ra, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// Update status separately if it exists
					if tra.AcceptedStatus != nil {
						ra.Status.Conditions = []metav1.Condition{
							{
								Type:   "Accepted",
								Status: *tra.AcceptedStatus,
							},
						}
						_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().UpdateStatus(
							context.Background(), ra, metav1.UpdateOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}
				}

				wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer wf.Shutdown()

				err = wf.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				controller, err := newNoOverlayController(wf, recorder)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Give time for informers to sync
				gomega.Eventually(func() bool {
					return wf.RouteAdvertisementsInformer().Informer().HasSynced()
				}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())

				err = controller.validate()
				if tt.expectError {
					gomega.Expect(err).To(gomega.HaveOccurred())
					if tt.expectErrorSubstring != "" {
						gomega.Expect(err.Error()).To(gomega.ContainSubstring(tt.expectErrorSubstring))
					}
				} else {
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
			})
		}
	})

	ginkgo.Context("RA needsValidation logic", func() {
		var controller *noOverlayController

		ginkgo.BeforeEach(func() {
			config.Default.Transport = config.TransportNoOverlay

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err = newNoOverlayController(wf, recorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		tests := []struct {
			name           string
			oldRA          *testRA
			newRA          *testRA
			expectValidate bool
		}{
			{
				name: "should return true when NetworkSelectors change",
				oldRA: &testRA{
					NetworkSelectors: []apitypes.NetworkSelector{
						{NetworkSelectionType: apitypes.DefaultNetwork},
					},
				},
				newRA: &testRA{
					NetworkSelectors: []apitypes.NetworkSelector{
						{NetworkSelectionType: apitypes.DefaultNetwork},
						{NetworkSelectionType: apitypes.NetworkSelectionType("CustomNetwork")},
					},
				},
				expectValidate: true,
			},
			{
				name: "should return true when Advertisements change",
				oldRA: &testRA{
					SelectsDefault: true,
					Advertisements: []ratypes.AdvertisementType{ratypes.PodNetwork},
				},
				newRA: &testRA{
					SelectsDefault: true,
					Advertisements: []ratypes.AdvertisementType{ratypes.PodNetwork, ratypes.EgressIP},
				},
				expectValidate: true,
			},
			{
				name: "should return true when Accepted condition changes",
				oldRA: &testRA{
					SelectsDefault: true,
					AdvertisePods:  true,
					AcceptedStatus: ptr.To(metav1.ConditionFalse),
				},
				newRA: &testRA{
					SelectsDefault: true,
					AdvertisePods:  true,
					AcceptedStatus: ptr.To(metav1.ConditionTrue),
				},
				expectValidate: true,
			},
			{
				name: "should return false when nothing relevant changes",
				oldRA: &testRA{
					Name:           "test-ra",
					SelectsDefault: true,
					AdvertisePods:  true,
					AcceptedStatus: ptr.To(metav1.ConditionTrue),
				},
				newRA: &testRA{
					Name:           "test-ra",
					SelectsDefault: true,
					AdvertisePods:  true,
					AcceptedStatus: ptr.To(metav1.ConditionTrue),
				},
				expectValidate: false,
			},
		}

		for _, tt := range tests {
			tt := tt // capture range variable
			ginkgo.It(tt.name, func() {
				oldRA := tt.oldRA.RouteAdvertisements()
				newRA := tt.newRA.RouteAdvertisements()
				result := controller.raNeedsValidation(oldRA, newRA)
				gomega.Expect(result).To(gomega.Equal(tt.expectValidate))
			})
		}
	})

	ginkgo.Context("Event emission", func() {
		ginkgo.It("should emit event on validation failure", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			localRecorder := record.NewFakeRecorder(100)

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, localRecorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Start controller which will run initial validation
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			// Wait a bit for the event to be emitted
			gomega.Eventually(func() int {
				return len(localRecorder.Events)
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeNumerically(">", 0))

			// Check event was emitted
			event := <-localRecorder.Events
			gomega.Expect(event).To(gomega.ContainSubstring("Warning"))
			gomega.Expect(event).To(gomega.ContainSubstring("NoRouteAdvertisements"))
			gomega.Expect(event).To(gomega.ContainSubstring("RouteAdvertisements"))
		})

		ginkgo.It("should not spam events when validation stays failed", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			localRecorder := record.NewFakeRecorder(100)

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, localRecorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Run validation multiple times with the same error
			controller.runValidation()
			controller.runValidation()
			controller.runValidation()

			// Should only have one event (first validation)
			gomega.Expect(localRecorder.Events).To(gomega.HaveLen(1))
		})

		ginkgo.It("should emit Ready event when validation passes after being failed", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			localRecorder := record.NewFakeRecorder(100)

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, localRecorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// First validation will fail (no RouteAdvertisements)
			controller.runValidation()
			gomega.Expect(localRecorder.Events).To(gomega.HaveLen(1))
			event1 := <-localRecorder.Events
			gomega.Expect(event1).To(gomega.ContainSubstring("Warning"))

			// Now, create a valid RA to fix the configuration
			validRA := testRA{
				Name:           "test-ra",
				SelectsDefault: true,
				AdvertisePods:  true,
				AcceptedStatus: ptr.To(metav1.ConditionTrue),
			}
			ra := validRA.RouteAdvertisements()
			_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), ra, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().UpdateStatus(context.Background(), ra, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Wait for informer to see the new RA
			gomega.Eventually(func() bool {
				ra, err := wf.RouteAdvertisementsInformer().Lister().Get("test-ra")
				return err == nil && ra != nil
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())

			// Second validation will pass
			controller.runValidation()
			gomega.Expect(localRecorder.Events).To(gomega.HaveLen(1))
			event2 := <-localRecorder.Events
			gomega.Expect(event2).To(gomega.ContainSubstring("Normal"))
			gomega.Expect(event2).To(gomega.ContainSubstring("NoOverlayConfigurationReady"))
		})
	})

	ginkgo.Context("Controller lifecycle", func() {
		ginkgo.It("should start and stop without errors", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			ra := testRA{
				Name:           "test-ra",
				SelectsDefault: true,
				AdvertisePods:  true,
				AcceptedStatus: ptr.To(metav1.ConditionTrue),
			}

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(ra.RouteAdvertisements()),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, recorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Stop should not panic
			gomega.Expect(func() { controller.Stop() }).NotTo(gomega.Panic())
		})
	})
})
