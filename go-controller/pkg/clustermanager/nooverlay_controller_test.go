package clustermanager

import (
	"context"
	"time"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ratypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	apitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
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
		stopCh   chan struct{}
		recorder *record.FakeRecorder
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		stopCh = make(chan struct{})
		recorder = record.NewFakeRecorder(100)
		// Enable multi-network and route advertisements features
		// These are required for the no-overlay controller to function
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableRouteAdvertisements = true
	})

	ginkgo.AfterEach(func() {
		close(stopCh)
	})

	ginkgo.Context("Controller creation", func() {
		ginkgo.It("should create controller when transport is no-overlay", func() {
			config.Default.Transport = config.TransportNoOverlay

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient: fake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			controller, err := newNoOverlayController(wf, recorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(controller).NotTo(gomega.BeNil())
			gomega.Expect(controller.wf).To(gomega.Equal(wf))
			gomega.Expect(controller.recorder).To(gomega.Equal(recorder))
			gomega.Expect(controller.nadController).NotTo(gomega.BeNil())
			gomega.Expect(controller.raController).NotTo(gomega.BeNil())
		})

		ginkgo.It("should have empty last validation error on creation", func() {
			config.Default.Transport = config.TransportNoOverlay

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient: fake.NewSimpleClientset(),
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
		ginkgo.It("should fail when RouteAdvertisements feature is not enabled", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableRouteAdvertisements = false

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient: fake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			controller, err := newNoOverlayController(wf, recorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = controller.validate()
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("RouteAdvertisements feature to be enabled"))
		})

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
				config.OVNKubernetesFeature.EnableRouteAdvertisements = true

				// Convert testRA to RouteAdvertisements objects
				var ras []runtime.Object
				for _, tra := range tt.ras {
					ras = append(ras, tra.RouteAdvertisements())
				}

				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: fake.NewSimpleClientset(ras...),
				}

				wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer wf.Shutdown()

				gomega.Expect(wf.Start()).To(gomega.Succeed())

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

	ginkgo.Context("NAD needsValidation logic", func() {
		var controller *noOverlayController

		ginkgo.BeforeEach(func() {
			config.Default.Transport = config.TransportNoOverlay

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient: fake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err = newNoOverlayController(wf, recorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should return true when route-advertisements annotation changes", func() {
			oldNAD := &nettypes.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ovntypes.DefaultNetworkName,
					Namespace: config.Kubernetes.OVNConfigNamespace,
					Annotations: map[string]string{
						ovntypes.OvnRouteAdvertisementsKey: `["ra1"]`,
					},
				},
			}
			newNAD := &nettypes.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ovntypes.DefaultNetworkName,
					Namespace: config.Kubernetes.OVNConfigNamespace,
					Annotations: map[string]string{
						ovntypes.OvnRouteAdvertisementsKey: `["ra1", "ra2"]`,
					},
				},
			}

			gomega.Expect(controller.nadNeedsValidation(oldNAD, newNAD)).To(gomega.BeTrue())
		})

		ginkgo.It("should return true when route-advertisements annotation is added", func() {
			oldNAD := &nettypes.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ovntypes.DefaultNetworkName,
					Namespace: config.Kubernetes.OVNConfigNamespace,
				},
			}
			newNAD := &nettypes.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ovntypes.DefaultNetworkName,
					Namespace: config.Kubernetes.OVNConfigNamespace,
					Annotations: map[string]string{
						ovntypes.OvnRouteAdvertisementsKey: `["ra1"]`,
					},
				},
			}

			gomega.Expect(controller.nadNeedsValidation(oldNAD, newNAD)).To(gomega.BeTrue())
		})

		ginkgo.It("should return true when route-advertisements annotation is removed", func() {
			oldNAD := &nettypes.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ovntypes.DefaultNetworkName,
					Namespace: config.Kubernetes.OVNConfigNamespace,
					Annotations: map[string]string{
						ovntypes.OvnRouteAdvertisementsKey: `["ra1"]`,
					},
				},
			}
			newNAD := &nettypes.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ovntypes.DefaultNetworkName,
					Namespace: config.Kubernetes.OVNConfigNamespace,
				},
			}

			gomega.Expect(controller.nadNeedsValidation(oldNAD, newNAD)).To(gomega.BeTrue())
		})

		ginkgo.It("should return false when other annotations change", func() {
			oldNAD := &nettypes.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ovntypes.DefaultNetworkName,
					Namespace: config.Kubernetes.OVNConfigNamespace,
					Annotations: map[string]string{
						ovntypes.OvnRouteAdvertisementsKey: `["ra1"]`,
						"other-annotation":                 "value1",
					},
				},
			}
			newNAD := &nettypes.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ovntypes.DefaultNetworkName,
					Namespace: config.Kubernetes.OVNConfigNamespace,
					Annotations: map[string]string{
						ovntypes.OvnRouteAdvertisementsKey: `["ra1"]`,
						"other-annotation":                 "value2",
					},
				},
			}

			gomega.Expect(controller.nadNeedsValidation(oldNAD, newNAD)).To(gomega.BeFalse())
		})

		ginkgo.It("should return false for non-default network NAD changes", func() {
			oldNAD := &nettypes.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "custom-network",
					Namespace: config.Kubernetes.OVNConfigNamespace,
					Annotations: map[string]string{
						ovntypes.OvnRouteAdvertisementsKey: `["ra1"]`,
					},
				},
			}
			newNAD := &nettypes.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "custom-network",
					Namespace: config.Kubernetes.OVNConfigNamespace,
					Annotations: map[string]string{
						ovntypes.OvnRouteAdvertisementsKey: `["ra2"]`,
					},
				},
			}

			gomega.Expect(controller.nadNeedsValidation(oldNAD, newNAD)).To(gomega.BeFalse())
		})
	})

	ginkgo.Context("RA needsValidation logic", func() {
		var controller *noOverlayController

		ginkgo.BeforeEach(func() {
			config.Default.Transport = config.TransportNoOverlay

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient: fake.NewSimpleClientset(),
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

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient: fake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, recorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Start controller which will run initial validation
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			// Wait a bit for the event to be emitted
			gomega.Eventually(func() int {
				return len(recorder.Events)
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeNumerically(">", 0))

			// Check event was emitted
			event := <-recorder.Events
			gomega.Expect(event).To(gomega.ContainSubstring("Warning"))
			gomega.Expect(event).To(gomega.ContainSubstring("NoRouteAdvertisements"))
			gomega.Expect(event).To(gomega.ContainSubstring("RouteAdvertisements"))
		})

		ginkgo.It("should not spam events when validation stays failed", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient: fake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, recorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Run validation multiple times with the same error
			controller.runValidation()
			controller.runValidation()
			controller.runValidation()

			// Should only have one event (first validation)
			gomega.Expect(recorder.Events).To(gomega.HaveLen(1))
		})

		ginkgo.It("should emit Ready event when validation passes after being failed", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient: fake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, recorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// First validation will fail (no RouteAdvertisements)
			controller.runValidation()
			gomega.Expect(recorder.Events).To(gomega.HaveLen(1))
			event1 := <-recorder.Events
			gomega.Expect(event1).To(gomega.ContainSubstring("Warning"))

			// Simulate fixing the configuration by changing transport mode
			// This will make validation pass (return nil)
			config.Default.Transport = config.TransportGeneve

			// Second validation will pass (transport is no longer no-overlay)
			controller.runValidation()
			gomega.Expect(recorder.Events).To(gomega.HaveLen(1))
			event2 := <-recorder.Events
			gomega.Expect(event2).To(gomega.ContainSubstring("Normal"))
			gomega.Expect(event2).To(gomega.ContainSubstring("NoOverlayConfigurationReady"))
		})
	})

	ginkgo.Context("Controller lifecycle", func() {
		ginkgo.It("should start and stop without errors", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			ra := &ratypes.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-ra",
				},
				Spec: ratypes.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{NetworkSelectionType: apitypes.DefaultNetwork},
					},
					Advertisements: []ratypes.AdvertisementType{
						ratypes.PodNetwork,
					},
				},
				Status: ratypes.RouteAdvertisementsStatus{
					Conditions: []metav1.Condition{
						{Type: "Accepted", Status: metav1.ConditionTrue},
					},
				},
			}

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient: fake.NewSimpleClientset(ra),
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

	ginkgo.Context("Integration with cluster manager", func() {
		ginkgo.It("should be created when cluster manager is created with no-overlay transport", func() {
			ctx := context.Background()
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			ra := &ratypes.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-ra",
				},
				Spec: ratypes.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{NetworkSelectionType: apitypes.DefaultNetwork},
					},
					Advertisements: []ratypes.AdvertisementType{
						ratypes.PodNetwork,
					},
				},
				Status: ratypes.RouteAdvertisementsStatus{
					Conditions: []metav1.Condition{
						{Type: "Accepted", Status: metav1.ConditionTrue},
					},
				},
			}

			nodes := []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node1",
					},
				},
			}

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient: fake.NewSimpleClientset(&corev1.NodeList{Items: nodes}, ra),
			}

			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			recorder := record.NewFakeRecorder(100)
			cm, err := NewClusterManager(
				fakeClient,
				wf,
				"test-identity",
				recorder,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cm.noOverlayController).NotTo(gomega.BeNil())

			err = cm.Start(ctx)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			cm.Stop()
		})

		ginkgo.It("should not be created when transport is not no-overlay", func() {
			config.Default.Transport = config.TransportGeneve // Not no-overlay

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient: fake.NewSimpleClientset(),
			}

			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			recorder := record.NewFakeRecorder(100)
			cm, err := NewClusterManager(
				fakeClient,
				wf,
				"test-identity",
				recorder,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cm.noOverlayController).To(gomega.BeNil())
		})
	})
})
