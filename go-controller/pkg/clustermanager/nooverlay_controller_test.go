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
	udntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
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

type testCUDN struct {
	Name            string
	Labels          map[string]string
	Transport       udntypes.TransportOption
	OutboundSNAT    udntypes.SNATOption
	Routing         udntypes.RoutingOption
	NetworkTopology udntypes.NetworkTopology
	NetworkRole     udntypes.NetworkRole
	Layer3Subnets   []string
	JoinSubnet      string
	MTU             int32
}

func (tcudn testCUDN) ClusterUserDefinedNetwork() *udntypes.ClusterUserDefinedNetwork {
	cudn := &udntypes.ClusterUserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:   tcudn.Name,
			Labels: tcudn.Labels,
		},
		Spec: udntypes.ClusterUserDefinedNetworkSpec{
			NamespaceSelector: metav1.LabelSelector{},
			Network: udntypes.NetworkSpec{
				Topology: tcudn.NetworkTopology,
			},
		},
	}

	// Set transport
	if tcudn.Transport != "" {
		cudn.Spec.Network.Transport = tcudn.Transport
	}

	// Set no-overlay options if transport is NoOverlay
	if tcudn.Transport == udntypes.TransportOptionNoOverlay {
		cudn.Spec.Network.NoOverlay = &udntypes.NoOverlayConfig{
			OutboundSNAT: tcudn.OutboundSNAT,
			Routing:      tcudn.Routing,
		}
	}

	// Set Layer3 config if topology is Layer3
	if tcudn.NetworkTopology == udntypes.NetworkTopologyLayer3 {
		layer3Config := &udntypes.Layer3Config{
			Role: tcudn.NetworkRole,
		}
		if len(tcudn.Layer3Subnets) > 0 {
			for _, subnet := range tcudn.Layer3Subnets {
				layer3Config.Subnets = append(layer3Config.Subnets, udntypes.Layer3Subnet{
					CIDR: udntypes.CIDR(subnet),
				})
			}
		}
		if tcudn.JoinSubnet != "" {
			layer3Config.JoinSubnets = udntypes.DualStackCIDRs{udntypes.CIDR(tcudn.JoinSubnet)}
		}
		if tcudn.MTU > 0 {
			layer3Config.MTU = tcudn.MTU
		}
		cudn.Spec.Network.Layer3 = layer3Config
	}

	return cudn
}

var _ = ginkgo.Describe("No-Overlay Controller", func() {
	var (
		recorder *record.FakeRecorder
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		recorder = record.NewFakeRecorder(100)
		// Enable multi-network, network segmentation, and route advertisements features
		// These are required for the no-overlay controller to function
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableRouteAdvertisements = true
	})

	ginkgo.Context("Controller creation", func() {
		ginkgo.It("should create controller when transport is no-overlay", func() {
			config.Default.Transport = types.NetworkTransportNoOverlay

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			controller, err := newNoOverlayController(wf, recorder, fakeClient.UserDefinedNetworkClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(controller).NotTo(gomega.BeNil())
			gomega.Expect(controller.wf).To(gomega.Equal(wf))
			gomega.Expect(controller.recorder).To(gomega.Equal(recorder))
			gomega.Expect(controller.udnClient).To(gomega.Equal(fakeClient.UserDefinedNetworkClient))
			gomega.Expect(controller.raController).NotTo(gomega.BeNil())
			gomega.Expect(controller.cudnController).NotTo(gomega.BeNil())
		})

		ginkgo.It("should have empty last validation error on creation", func() {
			config.Default.Transport = types.NetworkTransportNoOverlay

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			controller, err := newNoOverlayController(wf, recorder, fakeClient.UserDefinedNetworkClient)
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
				config.Default.Transport = types.NetworkTransportNoOverlay

				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient:                fake.NewSimpleClientset(),
					NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
					RouteAdvertisementsClient: rafake.NewSimpleClientset(),
					FRRClient:                 frrfake.NewSimpleClientset(),
					UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
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

				controller, err := newNoOverlayController(wf, recorder, fakeClient.UserDefinedNetworkClient)
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
			config.Default.Transport = types.NetworkTransportNoOverlay

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err = newNoOverlayController(wf, recorder, fakeClient.UserDefinedNetworkClient)
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
			config.Default.Transport = types.NetworkTransportNoOverlay
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			localRecorder := record.NewFakeRecorder(100)

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, localRecorder, fakeClient.UserDefinedNetworkClient)
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
			config.Default.Transport = types.NetworkTransportNoOverlay
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			localRecorder := record.NewFakeRecorder(100)

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, localRecorder, fakeClient.UserDefinedNetworkClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Run validation multiple times with the same error
			controller.runValidation()
			controller.runValidation()
			controller.runValidation()

			// Should only have one event (first validation)
			gomega.Expect(localRecorder.Events).To(gomega.HaveLen(1))
		})

		ginkgo.It("should emit Ready event when validation passes after being failed", func() {
			config.Default.Transport = types.NetworkTransportNoOverlay
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			localRecorder := record.NewFakeRecorder(100)

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, localRecorder, fakeClient.UserDefinedNetworkClient)
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
			config.Default.Transport = types.NetworkTransportNoOverlay
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
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, recorder, fakeClient.UserDefinedNetworkClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Stop should not panic
			gomega.Expect(func() { controller.Stop() }).NotTo(gomega.Panic())
		})
	})

	ginkgo.Context("ClusterUserDefinedNetwork validation", func() {
		tests := []struct {
			name                 string
			cudns                []*testCUDN
			ras                  []*testRA
			expectError          bool
			expectErrorSubstring string
		}{
			{
				name: "should pass when no CUDNs with no-overlay transport exist",
				cudns: []*testCUDN{
					{
						Name:            "test-cudn-geneve",
						Labels:          map[string]string{"app": "test"},
						Transport:       udntypes.TransportOptionGeneve,
						NetworkTopology: udntypes.NetworkTopologyLayer3,
						NetworkRole:     udntypes.NetworkRolePrimary,
					},
				},
				ras:         []*testRA{},
				expectError: false,
			},
			{
				name: "should fail when CUDN with no-overlay has no RouteAdvertisements",
				cudns: []*testCUDN{
					{
						Name:            "test-cudn-nooverlay",
						Labels:          map[string]string{"app": "test"},
						Transport:       udntypes.TransportOptionNoOverlay,
						OutboundSNAT:    udntypes.SNATEnabled,
						Routing:         udntypes.RoutingManaged,
						NetworkTopology: udntypes.NetworkTopologyLayer3,
						NetworkRole:     udntypes.NetworkRolePrimary,
					},
				},
				ras:                  []*testRA{},
				expectError:          true,
				expectErrorSubstring: "no RouteAdvertisements CR is advertising pod networks for ClusterUserDefinedNetwork",
			},
			{
				name: "should pass when CUDN with no-overlay has accepted RouteAdvertisements",
				cudns: []*testCUDN{
					{
						Name:            "test-cudn-nooverlay",
						Labels:          map[string]string{"app": "test"},
						Transport:       udntypes.TransportOptionNoOverlay,
						OutboundSNAT:    udntypes.SNATEnabled,
						Routing:         udntypes.RoutingManaged,
						NetworkTopology: udntypes.NetworkTopologyLayer3,
						NetworkRole:     udntypes.NetworkRolePrimary,
					},
				},
				ras: []*testRA{
					{
						Name: "test-ra",
						NetworkSelectors: []apitypes.NetworkSelector{
							{
								NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
								ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
									NetworkSelector: metav1.LabelSelector{
										MatchLabels: map[string]string{"app": "test"},
									},
								},
							},
						},
						AdvertisePods:  true,
						AcceptedStatus: ptr.To(metav1.ConditionTrue),
					},
				},
				expectError: false,
			},
			{
				name: "should fail when CUDN with no-overlay has RouteAdvertisements but not accepted",
				cudns: []*testCUDN{
					{
						Name:            "test-cudn-nooverlay",
						Labels:          map[string]string{"app": "test"},
						Transport:       udntypes.TransportOptionNoOverlay,
						OutboundSNAT:    udntypes.SNATEnabled,
						Routing:         udntypes.RoutingManaged,
						NetworkTopology: udntypes.NetworkTopologyLayer3,
						NetworkRole:     udntypes.NetworkRolePrimary,
					},
				},
				ras: []*testRA{
					{
						Name: "test-ra",
						NetworkSelectors: []apitypes.NetworkSelector{
							{
								NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
								ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
									NetworkSelector: metav1.LabelSelector{
										MatchLabels: map[string]string{"app": "test"},
									},
								},
							},
						},
						AdvertisePods:  true,
						AcceptedStatus: ptr.To(metav1.ConditionFalse),
					},
				},
				expectError:          true,
				expectErrorSubstring: "but none have status Accepted=True",
			},
			{
				name: "should fail when CUDN RouteAdvertisements doesn't advertise PodNetwork",
				cudns: []*testCUDN{
					{
						Name:            "test-cudn-nooverlay",
						Labels:          map[string]string{"app": "test"},
						Transport:       udntypes.TransportOptionNoOverlay,
						OutboundSNAT:    udntypes.SNATEnabled,
						Routing:         udntypes.RoutingManaged,
						NetworkTopology: udntypes.NetworkTopologyLayer3,
						NetworkRole:     udntypes.NetworkRolePrimary,
					},
				},
				ras: []*testRA{
					{
						Name: "test-ra",
						NetworkSelectors: []apitypes.NetworkSelector{
							{
								NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
								ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
									NetworkSelector: metav1.LabelSelector{
										MatchLabels: map[string]string{"app": "test"},
									},
								},
							},
						},
						Advertisements: []ratypes.AdvertisementType{ratypes.EgressIP},
						AcceptedStatus: ptr.To(metav1.ConditionTrue),
					},
				},
				expectError:          true,
				expectErrorSubstring: "no RouteAdvertisements CR is advertising pod networks for ClusterUserDefinedNetwork",
			},
			{
				name: "should pass when RouteAdvertisements matches CUDN via label selector",
				cudns: []*testCUDN{
					{
						Name:            "test-cudn-1",
						Labels:          map[string]string{"env": "prod", "team": "backend"},
						Transport:       udntypes.TransportOptionNoOverlay,
						OutboundSNAT:    udntypes.SNATEnabled,
						Routing:         udntypes.RoutingManaged,
						NetworkTopology: udntypes.NetworkTopologyLayer3,
						NetworkRole:     udntypes.NetworkRolePrimary,
					},
				},
				ras: []*testRA{
					{
						Name: "test-ra",
						NetworkSelectors: []apitypes.NetworkSelector{
							{
								NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
								ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
									NetworkSelector: metav1.LabelSelector{
										MatchLabels: map[string]string{"env": "prod"},
									},
								},
							},
						},
						AdvertisePods:  true,
						AcceptedStatus: ptr.To(metav1.ConditionTrue),
					},
				},
				expectError: false,
			},
			{
				name: "should fail when RouteAdvertisements doesn't match CUDN labels",
				cudns: []*testCUDN{
					{
						Name:            "test-cudn-1",
						Labels:          map[string]string{"env": "dev"},
						Transport:       udntypes.TransportOptionNoOverlay,
						OutboundSNAT:    udntypes.SNATEnabled,
						Routing:         udntypes.RoutingManaged,
						NetworkTopology: udntypes.NetworkTopologyLayer3,
						NetworkRole:     udntypes.NetworkRolePrimary,
					},
				},
				ras: []*testRA{
					{
						Name: "test-ra",
						NetworkSelectors: []apitypes.NetworkSelector{
							{
								NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
								ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
									NetworkSelector: metav1.LabelSelector{
										MatchLabels: map[string]string{"env": "prod"},
									},
								},
							},
						},
						AdvertisePods:  true,
						AcceptedStatus: ptr.To(metav1.ConditionTrue),
					},
				},
				expectError:          true,
				expectErrorSubstring: "no RouteAdvertisements CR is advertising pod networks for ClusterUserDefinedNetwork",
			},
		}

		for _, tt := range tests {
			ginkgo.It(tt.name, func() {
				// No need to set config.Default.Transport for CUDN tests
				// CUDNs can have NoOverlay transport independently

				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient:                fake.NewSimpleClientset(),
					NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
					RouteAdvertisementsClient: rafake.NewSimpleClientset(),
					FRRClient:                 frrfake.NewSimpleClientset(),
					UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
				}

				// Create CUDNs
				for _, tcudn := range tt.cudns {
					cudn := tcudn.ClusterUserDefinedNetwork()
					_, err := fakeClient.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Create(
						context.Background(), cudn, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}

				// Create RAs
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

				controller, err := newNoOverlayController(wf, recorder, fakeClient.UserDefinedNetworkClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Give time for informers to sync
				gomega.Eventually(func() bool {
					return wf.ClusterUserDefinedNetworkInformer().Informer().HasSynced() &&
						wf.RouteAdvertisementsInformer().Informer().HasSynced()
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

	ginkgo.Context("CUDN needsValidation logic", func() {
		var controller *noOverlayController

		ginkgo.BeforeEach(func() {
			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err = newNoOverlayController(wf, recorder, fakeClient.UserDefinedNetworkClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		tests := []struct {
			name           string
			oldCUDN        *testCUDN
			newCUDN        *testCUDN
			expectValidate bool
		}{
			{
				name: "should return true when transport changes to NoOverlay",
				oldCUDN: &testCUDN{
					Name:            "test-cudn",
					Transport:       udntypes.TransportOptionGeneve,
					NetworkTopology: udntypes.NetworkTopologyLayer3,
					NetworkRole:     udntypes.NetworkRolePrimary,
				},
				newCUDN: &testCUDN{
					Name:            "test-cudn",
					Transport:       udntypes.TransportOptionNoOverlay,
					OutboundSNAT:    udntypes.SNATEnabled,
					Routing:         udntypes.RoutingManaged,
					NetworkTopology: udntypes.NetworkTopologyLayer3,
					NetworkRole:     udntypes.NetworkRolePrimary,
				},
				expectValidate: true,
			},
			{
				name: "should return true when transport changes from NoOverlay",
				oldCUDN: &testCUDN{
					Name:            "test-cudn",
					Transport:       udntypes.TransportOptionNoOverlay,
					OutboundSNAT:    udntypes.SNATEnabled,
					Routing:         udntypes.RoutingManaged,
					NetworkTopology: udntypes.NetworkTopologyLayer3,
					NetworkRole:     udntypes.NetworkRolePrimary,
				},
				newCUDN: &testCUDN{
					Name:            "test-cudn",
					Transport:       udntypes.TransportOptionGeneve,
					NetworkTopology: udntypes.NetworkTopologyLayer3,
					NetworkRole:     udntypes.NetworkRolePrimary,
				},
				expectValidate: true,
			},
			{
				name: "should return true when NoOverlay config changes",
				oldCUDN: &testCUDN{
					Name:            "test-cudn",
					Transport:       udntypes.TransportOptionNoOverlay,
					OutboundSNAT:    udntypes.SNATEnabled,
					Routing:         udntypes.RoutingManaged,
					NetworkTopology: udntypes.NetworkTopologyLayer3,
					NetworkRole:     udntypes.NetworkRolePrimary,
				},
				newCUDN: &testCUDN{
					Name:            "test-cudn",
					Transport:       udntypes.TransportOptionNoOverlay,
					OutboundSNAT:    udntypes.SNATDisabled,
					Routing:         udntypes.RoutingManaged,
					NetworkTopology: udntypes.NetworkTopologyLayer3,
					NetworkRole:     udntypes.NetworkRolePrimary,
				},
				expectValidate: true,
			},
			{
				name: "should return false when neither transport nor NoOverlay config change for Geneve",
				oldCUDN: &testCUDN{
					Name:            "test-cudn",
					Transport:       udntypes.TransportOptionGeneve,
					NetworkTopology: udntypes.NetworkTopologyLayer3,
					NetworkRole:     udntypes.NetworkRolePrimary,
				},
				newCUDN: &testCUDN{
					Name:            "test-cudn",
					Transport:       udntypes.TransportOptionGeneve,
					NetworkTopology: udntypes.NetworkTopologyLayer3,
					NetworkRole:     udntypes.NetworkRolePrimary,
				},
				expectValidate: false,
			},
			{
				name:    "should return true when old CUDN is nil",
				oldCUDN: nil,
				newCUDN: &testCUDN{
					Name:            "test-cudn",
					Transport:       udntypes.TransportOptionNoOverlay,
					OutboundSNAT:    udntypes.SNATEnabled,
					Routing:         udntypes.RoutingManaged,
					NetworkTopology: udntypes.NetworkTopologyLayer3,
					NetworkRole:     udntypes.NetworkRolePrimary,
				},
				expectValidate: true,
			},
			{
				name: "should return false when no-overlay CUDN doesn't change",
				oldCUDN: &testCUDN{
					Name:            "test-cudn",
					Transport:       udntypes.TransportOptionNoOverlay,
					OutboundSNAT:    udntypes.SNATEnabled,
					Routing:         udntypes.RoutingManaged,
					NetworkTopology: udntypes.NetworkTopologyLayer3,
					NetworkRole:     udntypes.NetworkRolePrimary,
				},
				newCUDN: &testCUDN{
					Name:            "test-cudn",
					Transport:       udntypes.TransportOptionNoOverlay,
					OutboundSNAT:    udntypes.SNATEnabled,
					Routing:         udntypes.RoutingManaged,
					NetworkTopology: udntypes.NetworkTopologyLayer3,
					NetworkRole:     udntypes.NetworkRolePrimary,
				},
				expectValidate: false,
			},
		}

		for _, tt := range tests {
			tt := tt // capture range variable
			ginkgo.It(tt.name, func() {
				var oldCUDN, newCUDN *udntypes.ClusterUserDefinedNetwork
				if tt.oldCUDN != nil {
					oldCUDN = tt.oldCUDN.ClusterUserDefinedNetwork()
				}
				if tt.newCUDN != nil {
					newCUDN = tt.newCUDN.ClusterUserDefinedNetwork()
				}
				result := controller.cudnNeedsValidation(oldCUDN, newCUDN)
				gomega.Expect(result).To(gomega.Equal(tt.expectValidate))
			})
		}
	})

	ginkgo.Context("isRASelectingCUDN logic", func() {
		var controller *noOverlayController

		ginkgo.BeforeEach(func() {
			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err = newNoOverlayController(wf, recorder, fakeClient.UserDefinedNetworkClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should return true when RA label selector matches CUDN labels", func() {
			cudn := testCUDN{
				Name:   "test-cudn",
				Labels: map[string]string{"env": "prod", "team": "backend"},
			}.ClusterUserDefinedNetwork()

			ra := testRA{
				Name: "test-ra",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"env": "prod"},
							},
						},
					},
				},
			}.RouteAdvertisements()

			result := controller.isRASelectingCUDN(ra, cudn)
			gomega.Expect(result).To(gomega.BeTrue())
		})

		ginkgo.It("should return false when RA label selector doesn't match CUDN labels", func() {
			cudn := testCUDN{
				Name:   "test-cudn",
				Labels: map[string]string{"env": "dev"},
			}.ClusterUserDefinedNetwork()

			ra := testRA{
				Name: "test-ra",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"env": "prod"},
							},
						},
					},
				},
			}.RouteAdvertisements()

			result := controller.isRASelectingCUDN(ra, cudn)
			gomega.Expect(result).To(gomega.BeFalse())
		})

		ginkgo.It("should return false when RA doesn't select ClusterUserDefinedNetworks", func() {
			cudn := testCUDN{
				Name:   "test-cudn",
				Labels: map[string]string{"env": "prod"},
			}.ClusterUserDefinedNetwork()

			ra := testRA{
				Name:           "test-ra",
				SelectsDefault: true,
			}.RouteAdvertisements()

			result := controller.isRASelectingCUDN(ra, cudn)
			gomega.Expect(result).To(gomega.BeFalse())
		})

		ginkgo.It("should return true with empty label selector (matches all)", func() {
			cudn := testCUDN{
				Name:   "test-cudn",
				Labels: map[string]string{"env": "prod"},
			}.ClusterUserDefinedNetwork()

			ra := testRA{
				Name: "test-ra",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{},
						},
					},
				},
			}.RouteAdvertisements()

			result := controller.isRASelectingCUDN(ra, cudn)
			gomega.Expect(result).To(gomega.BeTrue())
		})
	})

	ginkgo.Context("CUDN event emission", func() {
		ginkgo.It("should emit event for CUDN validation failure", func() {
			localRecorder := record.NewFakeRecorder(100)

			cudn := testCUDN{
				Name:            "test-cudn-nooverlay",
				Labels:          map[string]string{"app": "test"},
				Transport:       udntypes.TransportOptionNoOverlay,
				OutboundSNAT:    udntypes.SNATEnabled,
				Routing:         udntypes.RoutingManaged,
				NetworkTopology: udntypes.NetworkTopologyLayer3,
				NetworkRole:     udntypes.NetworkRolePrimary,
			}

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(cudn.ClusterUserDefinedNetwork()),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, localRecorder, fakeClient.UserDefinedNetworkClient)
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
			gomega.Expect(event).To(gomega.ContainSubstring("ClusterUserDefinedNetwork"))
			gomega.Expect(event).To(gomega.ContainSubstring("test-cudn-nooverlay"))
		})
	})

	ginkgo.Context("CUDN TransportAccepted status updates", func() {
		ginkgo.It("should update status to True with GeneveTransportAccepted for Geneve transport", func() {
			cudn := testCUDN{
				Name:            "test-cudn-geneve",
				Labels:          map[string]string{"app": "test"},
				Transport:       udntypes.TransportOptionGeneve,
				NetworkTopology: udntypes.NetworkTopologyLayer3,
				NetworkRole:     udntypes.NetworkRolePrimary,
			}

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(cudn.ClusterUserDefinedNetwork()),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, recorder, fakeClient.UserDefinedNetworkClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Give time for informers to sync
			gomega.Eventually(func() bool {
				return wf.ClusterUserDefinedNetworkInformer().Informer().HasSynced()
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())

			// Run validation which should update status
			err = controller.validateClusterUserDefinedNetworks()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Check that the status was updated
			gomega.Eventually(func() bool {
				updatedCUDN, err := fakeClient.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(
					context.Background(), "test-cudn-geneve", metav1.GetOptions{})
				if err != nil {
					return false
				}
				for _, cond := range updatedCUDN.Status.Conditions {
					if cond.Type == "TransportAccepted" &&
						cond.Status == metav1.ConditionTrue &&
						cond.Reason == "GeneveTransportAccepted" &&
						cond.Message == "Geneve transport has been configured." {
						return true
					}
				}
				return false
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
		})

		ginkgo.It("should update status to True for empty transport (defaults to Geneve)", func() {
			cudn := testCUDN{
				Name:            "test-cudn-default",
				Labels:          map[string]string{"app": "test"},
				Transport:       "",
				NetworkTopology: udntypes.NetworkTopologyLayer3,
				NetworkRole:     udntypes.NetworkRolePrimary,
			}

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(cudn.ClusterUserDefinedNetwork()),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, recorder, fakeClient.UserDefinedNetworkClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Give time for informers to sync
			gomega.Eventually(func() bool {
				return wf.ClusterUserDefinedNetworkInformer().Informer().HasSynced()
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())

			// Run validation which should update status
			err = controller.validateClusterUserDefinedNetworks()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Check that the status was updated
			gomega.Eventually(func() bool {
				updatedCUDN, err := fakeClient.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(
					context.Background(), "test-cudn-default", metav1.GetOptions{})
				if err != nil {
					return false
				}
				for _, cond := range updatedCUDN.Status.Conditions {
					if cond.Type == "TransportAccepted" &&
						cond.Status == metav1.ConditionTrue &&
						cond.Reason == "GeneveTransportAccepted" {
						return true
					}
				}
				return false
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
		})

		ginkgo.It("should update status to True with NoOverlayTransportAccepted when RA is accepted", func() {
			cudn := testCUDN{
				Name:            "test-cudn-nooverlay",
				Labels:          map[string]string{"app": "test"},
				Transport:       udntypes.TransportOptionNoOverlay,
				OutboundSNAT:    udntypes.SNATEnabled,
				Routing:         udntypes.RoutingManaged,
				NetworkTopology: udntypes.NetworkTopologyLayer3,
				NetworkRole:     udntypes.NetworkRolePrimary,
			}

			ra := testRA{
				Name: "test-ra",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"app": "test"},
							},
						},
					},
				},
				AdvertisePods:  true,
				AcceptedStatus: ptr.To(metav1.ConditionTrue),
			}

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(cudn.ClusterUserDefinedNetwork()),
			}

			// Create RA
			_, err := fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(
				context.Background(), ra.RouteAdvertisements(), metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().UpdateStatus(
				context.Background(), ra.RouteAdvertisements(), metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, recorder, fakeClient.UserDefinedNetworkClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Give time for informers to sync
			gomega.Eventually(func() bool {
				return wf.ClusterUserDefinedNetworkInformer().Informer().HasSynced() &&
					wf.RouteAdvertisementsInformer().Informer().HasSynced()
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())

			// Run validation which should update status
			err = controller.validateClusterUserDefinedNetworks()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Check that the status was updated
			gomega.Eventually(func() bool {
				updatedCUDN, err := fakeClient.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(
					context.Background(), "test-cudn-nooverlay", metav1.GetOptions{})
				if err != nil {
					return false
				}
				for _, cond := range updatedCUDN.Status.Conditions {
					if cond.Type == "TransportAccepted" &&
						cond.Status == metav1.ConditionTrue &&
						cond.Reason == "NoOverlayTransportAccepted" &&
						cond.Message == "Transport has been configured as 'no-overlay'." {
						return true
					}
				}
				return false
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
		})

		ginkgo.It("should update status to False when no RouteAdvertisements exists", func() {
			cudn := testCUDN{
				Name:            "test-cudn-nooverlay",
				Labels:          map[string]string{"app": "test"},
				Transport:       udntypes.TransportOptionNoOverlay,
				OutboundSNAT:    udntypes.SNATEnabled,
				Routing:         udntypes.RoutingManaged,
				NetworkTopology: udntypes.NetworkTopologyLayer3,
				NetworkRole:     udntypes.NetworkRolePrimary,
			}

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(cudn.ClusterUserDefinedNetwork()),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, recorder, fakeClient.UserDefinedNetworkClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Give time for informers to sync
			gomega.Eventually(func() bool {
				return wf.ClusterUserDefinedNetworkInformer().Informer().HasSynced()
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())

			// Run validation which should update status
			err = controller.validateClusterUserDefinedNetworks()
			gomega.Expect(err).To(gomega.HaveOccurred()) // Should return error

			// Check that the status was updated
			gomega.Eventually(func() bool {
				updatedCUDN, err := fakeClient.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(
					context.Background(), "test-cudn-nooverlay", metav1.GetOptions{})
				if err != nil {
					return false
				}
				for _, cond := range updatedCUDN.Status.Conditions {
					if cond.Type == "TransportAccepted" &&
						cond.Status == metav1.ConditionFalse &&
						cond.Reason == "NoOverlayRouteAdvertisementsIsMissing" &&
						cond.Message == "No RouteAdvertisements CR is advertising the pod networks." {
						return true
					}
				}
				return false
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
		})

		ginkgo.It("should update status to False when RouteAdvertisements exists but not accepted", func() {
			cudn := testCUDN{
				Name:            "test-cudn-nooverlay",
				Labels:          map[string]string{"app": "test"},
				Transport:       udntypes.TransportOptionNoOverlay,
				OutboundSNAT:    udntypes.SNATEnabled,
				Routing:         udntypes.RoutingManaged,
				NetworkTopology: udntypes.NetworkTopologyLayer3,
				NetworkRole:     udntypes.NetworkRolePrimary,
			}

			ra := testRA{
				Name: "test-ra",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"app": "test"},
							},
						},
					},
				},
				AdvertisePods:  true,
				AcceptedStatus: ptr.To(metav1.ConditionFalse),
			}

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(cudn.ClusterUserDefinedNetwork()),
			}

			// Create RA
			_, err := fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(
				context.Background(), ra.RouteAdvertisements(), metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().UpdateStatus(
				context.Background(), ra.RouteAdvertisements(), metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, recorder, fakeClient.UserDefinedNetworkClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Give time for informers to sync
			gomega.Eventually(func() bool {
				return wf.ClusterUserDefinedNetworkInformer().Informer().HasSynced() &&
					wf.RouteAdvertisementsInformer().Informer().HasSynced()
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())

			// Run validation which should update status
			err = controller.validateClusterUserDefinedNetworks()
			gomega.Expect(err).To(gomega.HaveOccurred()) // Should return error

			// Check that the status was updated
			gomega.Eventually(func() bool {
				updatedCUDN, err := fakeClient.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(
					context.Background(), "test-cudn-nooverlay", metav1.GetOptions{})
				if err != nil {
					return false
				}
				for _, cond := range updatedCUDN.Status.Conditions {
					if cond.Type == "TransportAccepted" &&
						cond.Status == metav1.ConditionFalse &&
						cond.Reason == "NoOverlayRouteAdvertisementsNotAccepted" {
						return true
					}
				}
				return false
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
		})

		ginkgo.It("should not update status when condition already matches", func() {
			cudn := testCUDN{
				Name:            "test-cudn-geneve",
				Labels:          map[string]string{"app": "test"},
				Transport:       udntypes.TransportOptionGeneve,
				NetworkTopology: udntypes.NetworkTopologyLayer3,
				NetworkRole:     udntypes.NetworkRolePrimary,
			}

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(cudn.ClusterUserDefinedNetwork()),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, recorder, fakeClient.UserDefinedNetworkClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Give time for informers to sync
			gomega.Eventually(func() bool {
				return wf.ClusterUserDefinedNetworkInformer().Informer().HasSynced()
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())

			// Run validation first time
			err = controller.validateClusterUserDefinedNetworks()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Get the updated CUDN
			updatedCUDN, err := fakeClient.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(
				context.Background(), "test-cudn-geneve", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Get the resource version after first update
			firstResourceVersion := updatedCUDN.ResourceVersion

			// Run validation second time
			err = controller.validateClusterUserDefinedNetworks()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Get the updated CUDN again
			updatedCUDN, err = fakeClient.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(
				context.Background(), "test-cudn-geneve", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Resource version should be the same (no update happened)
			gomega.Expect(updatedCUDN.ResourceVersion).To(gomega.Equal(firstResourceVersion))
		})
	})
})
