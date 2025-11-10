package clustermanager

import (
	"context"
	"time"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	frrfake "github.com/metallb/frr-k8s/pkg/client/clientset/versioned/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	ratypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	rafake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned/fake"
	apitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

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
	})

	ginkgo.AfterEach(func() {
		close(stopCh)
	})

	ginkgo.Context("Controller creation", func() {
		ginkgo.It("should create controller when transport is no-overlay", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				EgressIPClient:            egressipfake.NewSimpleClientset(),
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
			gomega.Expect(controller.nadController).NotTo(gomega.BeNil())
			gomega.Expect(controller.raController).NotTo(gomega.BeNil())
		})

		ginkgo.It("should have empty last validation error on creation", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				EgressIPClient:            egressipfake.NewSimpleClientset(),
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
		ginkgo.It("should fail when RouteAdvertisements feature is not enabled", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			// Enable RouteAdvertisements temporarily to create watch factory with initialized factories
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				EgressIPClient:            egressipfake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			// Now disable RouteAdvertisements to test the validation failure
			config.OVNKubernetesFeature.EnableRouteAdvertisements = false

			controller, err := newNoOverlayController(wf, recorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = controller.validate()
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("RouteAdvertisements feature to be enabled"))
		})

		ginkgo.It("should fail when no RouteAdvertisements CR exists", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				EgressIPClient:            egressipfake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			controller, err := newNoOverlayController(wf, recorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = controller.validate()
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("no RouteAdvertisements CR is advertising the default network"))
		})

		ginkgo.It("should pass when valid RouteAdvertisements CR exists", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				EgressIPClient:            egressipfake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			// Start the watch factory to populate the informer caches
			gomega.Expect(wf.Start()).To(gomega.Succeed())

			// Create the RouteAdvertisements CR after starting the watch factory
			ra := &ratypes.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-ra",
				},
				Spec: ratypes.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{
							NetworkSelectionType: apitypes.DefaultNetwork,
						},
					},
					Advertisements: []ratypes.AdvertisementType{
						ratypes.PodNetwork,
					},
				},
				Status: ratypes.RouteAdvertisementsStatus{
					Conditions: []metav1.Condition{
						{
							Type:   "Accepted",
							Status: metav1.ConditionTrue,
						},
					},
				},
			}
			_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err := newNoOverlayController(wf, recorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Wait for the informer cache to be populated with retry
			gomega.Eventually(func() error {
				return controller.validate()
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.Succeed())
		})

		ginkgo.It("should fail when RouteAdvertisements CR exists but not Accepted", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				EgressIPClient:            egressipfake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			// Create the RouteAdvertisements CR after starting the watch factory
			ra := &ratypes.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-ra",
				},
				Spec: ratypes.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{
							NetworkSelectionType: apitypes.DefaultNetwork,
						},
					},
					Advertisements: []ratypes.AdvertisementType{
						ratypes.PodNetwork,
					},
				},
				Status: ratypes.RouteAdvertisementsStatus{
					Conditions: []metav1.Condition{
						{
							Type:   "Accepted",
							Status: metav1.ConditionFalse,
						},
					},
				},
			}
			_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err := newNoOverlayController(wf, recorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Wait for the informer cache to be populated with retry
			gomega.Eventually(func() error {
				return controller.validate()
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.MatchError(gomega.ContainSubstring("but none have status Accepted=True")))
		})

		ginkgo.It("should fail when RouteAdvertisements CR doesn't advertise PodNetwork", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				EgressIPClient:            egressipfake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			gomega.Expect(wf.Start()).To(gomega.Succeed())

			// Create the RouteAdvertisements CR after starting the watch factory
			ra := &ratypes.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-ra",
				},
				Spec: ratypes.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{
							NetworkSelectionType: apitypes.DefaultNetwork,
						},
					},
					Advertisements: []ratypes.AdvertisementType{
						ratypes.EgressIP, // Not PodNetwork
					},
				},
				Status: ratypes.RouteAdvertisementsStatus{
					Conditions: []metav1.Condition{
						{
							Type:   "Accepted",
							Status: metav1.ConditionTrue,
						},
					},
				},
			}
			_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err := newNoOverlayController(wf, recorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Wait for the informer cache to be populated with retry
			gomega.Eventually(func() error {
				return controller.validate()
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.MatchError(gomega.ContainSubstring("no RouteAdvertisements CR is advertising the default network")))
		})

		ginkgo.It("should pass when multiple RouteAdvertisements exist and one is valid", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			// Invalid RA - not for default network
			ra1 := &ratypes.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-ra-1",
				},
				Spec: ratypes.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{
							NetworkSelectionType: apitypes.NetworkSelectionType("CustomNetwork"),
						},
					},
					Advertisements: []ratypes.AdvertisementType{
						ratypes.PodNetwork,
					},
				},
				Status: ratypes.RouteAdvertisementsStatus{
					Conditions: []metav1.Condition{
						{
							Type:   "Accepted",
							Status: metav1.ConditionTrue,
						},
					},
				},
			}

			// Valid RA - for default network
			ra2 := &ratypes.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-ra-2",
				},
				Spec: ratypes.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{
							NetworkSelectionType: apitypes.DefaultNetwork,
						},
					},
					Advertisements: []ratypes.AdvertisementType{
						ratypes.PodNetwork,
					},
				},
				Status: ratypes.RouteAdvertisementsStatus{
					Conditions: []metav1.Condition{
						{
							Type:   "Accepted",
							Status: metav1.ConditionTrue,
						},
					},
				},
			}

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				EgressIPClient:            egressipfake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			// Start the watch factory to populate the informer caches
			gomega.Expect(wf.Start()).To(gomega.Succeed())

			// Create the RouteAdvertisements CRs after starting the watch factory
			_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra1, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra2, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err := newNoOverlayController(wf, recorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Wait for the informer cache to be populated with retry
			gomega.Eventually(func() error {
				return controller.validate()
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.Succeed())
		})
	})

	ginkgo.Context("NAD needsValidation logic", func() {
		var controller *noOverlayController

		ginkgo.BeforeEach(func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				EgressIPClient:            egressipfake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
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
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				EgressIPClient:            egressipfake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err = newNoOverlayController(wf, recorder)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should return true when NetworkSelectors change", func() {
			oldRA := &ratypes.RouteAdvertisements{
				Spec: ratypes.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{NetworkSelectionType: apitypes.DefaultNetwork},
					},
				},
			}
			newRA := &ratypes.RouteAdvertisements{
				Spec: ratypes.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{NetworkSelectionType: apitypes.DefaultNetwork},
						apitypes.NetworkSelector{NetworkSelectionType: apitypes.NetworkSelectionType("CustomNetwork")},
					},
				},
			}

			gomega.Expect(controller.raNeedsValidation(oldRA, newRA)).To(gomega.BeTrue())
		})

		ginkgo.It("should return true when Advertisements change", func() {
			oldRA := &ratypes.RouteAdvertisements{
				Spec: ratypes.RouteAdvertisementsSpec{
					Advertisements: []ratypes.AdvertisementType{ratypes.PodNetwork},
				},
			}
			newRA := &ratypes.RouteAdvertisements{
				Spec: ratypes.RouteAdvertisementsSpec{
					Advertisements: []ratypes.AdvertisementType{ratypes.PodNetwork, ratypes.EgressIP},
				},
			}

			gomega.Expect(controller.raNeedsValidation(oldRA, newRA)).To(gomega.BeTrue())
		})

		ginkgo.It("should return true when Accepted condition changes", func() {
			oldRA := &ratypes.RouteAdvertisements{
				Status: ratypes.RouteAdvertisementsStatus{
					Conditions: []metav1.Condition{
						{Type: "Accepted", Status: metav1.ConditionFalse},
					},
				},
			}
			newRA := &ratypes.RouteAdvertisements{
				Status: ratypes.RouteAdvertisementsStatus{
					Conditions: []metav1.Condition{
						{Type: "Accepted", Status: metav1.ConditionTrue},
					},
				},
			}

			gomega.Expect(controller.raNeedsValidation(oldRA, newRA)).To(gomega.BeTrue())
		})

		ginkgo.It("should return false when nothing relevant changes", func() {
			oldRA := &ratypes.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-ra",
					Labels: map[string]string{
						"key1": "value1",
					},
				},
				Spec: ratypes.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{NetworkSelectionType: apitypes.DefaultNetwork},
					},
					Advertisements: []ratypes.AdvertisementType{ratypes.PodNetwork},
				},
				Status: ratypes.RouteAdvertisementsStatus{
					Conditions: []metav1.Condition{
						{Type: "Accepted", Status: metav1.ConditionTrue},
					},
				},
			}
			newRA := &ratypes.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-ra",
					Labels: map[string]string{
						"key1": "value1",
						"key2": "value2", // Only labels changed
					},
				},
				Spec: ratypes.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{NetworkSelectionType: apitypes.DefaultNetwork},
					},
					Advertisements: []ratypes.AdvertisementType{ratypes.PodNetwork},
				},
				Status: ratypes.RouteAdvertisementsStatus{
					Conditions: []metav1.Condition{
						{Type: "Accepted", Status: metav1.ConditionTrue},
					},
				},
			}

			gomega.Expect(controller.raNeedsValidation(oldRA, newRA)).To(gomega.BeFalse())
		})
	})

	ginkgo.Context("Event emission", func() {
		ginkgo.It("should emit event on validation failure", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				EgressIPClient:            egressipfake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
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
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				EgressIPClient:            egressipfake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
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

		ginkgo.It("should emit Ready event when validation passes", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				EgressIPClient:            egressipfake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
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

			// Create a valid RouteAdvertisements CR
			ra := &ratypes.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-ra",
				},
				Spec: ratypes.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{
							NetworkSelectionType: apitypes.DefaultNetwork,
						},
					},
					Advertisements: []ratypes.AdvertisementType{
						ratypes.PodNetwork,
					},
				},
				Status: ratypes.RouteAdvertisementsStatus{
					Conditions: []metav1.Condition{
						{
							Type:   "Accepted",
							Status: metav1.ConditionTrue,
						},
					},
				},
			}
			_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Second validation will pass (valid RouteAdvertisements exists)
			gomega.Eventually(func() int {
				controller.runValidation()
				return len(recorder.Events)
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.Equal(1))
			event2 := <-recorder.Events
			gomega.Expect(event2).To(gomega.ContainSubstring("Normal"))
			gomega.Expect(event2).To(gomega.ContainSubstring("NoOverlayConfigurationReady"))
		})
	})

	ginkgo.Context("Controller lifecycle", func() {
		ginkgo.It("should start and stop without errors", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableMultiNetwork = true
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
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(ra),
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

	ginkgo.Context("Integration with cluster manager", func() {
		ginkgo.It("should be created when cluster manager is created with no-overlay transport", func() {
			ctx := context.Background()
			config.Default.Transport = config.TransportNoOverlay
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true
			config.OVNKubernetesFeature.EnableEgressIP = true

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
				KubeClient:                fake.NewSimpleClientset(&corev1.NodeList{Items: nodes}),
				EgressIPClient:            egressipfake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}

			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			// Start the watch factory first
			gomega.Expect(wf.Start()).To(gomega.Succeed())

			// Create the RouteAdvertisements CR after starting the watch factory
			_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(ctx, ra, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

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
				KubeClient:                fake.NewSimpleClientset(),
				EgressIPClient:            egressipfake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
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
