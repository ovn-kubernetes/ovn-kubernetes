package managedbgp

import (
	"context"
	"testing"
	"time"

	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"
	frrfake "github.com/metallb/frr-k8s/pkg/client/clientset/versioned/fake"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	rafake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned/fake"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestManagedBGPController(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "Managed BGP Controller Suite")
}

var _ = ginkgo.Describe("Managed BGP Controller", func() {
	var (
		recorder *record.FakeRecorder

		// Save original config
		oldTopology             string
		oldASNumber             uint32
		oldFRRNamespace         string
		oldOVNConfigNamespace   string
		oldEnableMultiNetwork   bool
		oldEnableRouteAdvertise bool
		oldTransport            string
		oldNoOverlayRouting     string
	)

	ginkgo.BeforeEach(func() {
		// Save original config
		oldTopology = config.ManagedBGP.Topology
		oldASNumber = config.ManagedBGP.ASNumber
		oldFRRNamespace = config.ManagedBGP.FRRNamespace
		oldOVNConfigNamespace = config.Kubernetes.OVNConfigNamespace
		oldEnableMultiNetwork = config.OVNKubernetesFeature.EnableMultiNetwork
		oldEnableRouteAdvertise = config.OVNKubernetesFeature.EnableRouteAdvertisements
		oldTransport = config.Default.Transport
		oldNoOverlayRouting = config.NoOverlay.Routing

		// Set test config
		config.Default.Transport = types.NetworkTransportNoOverlay
		config.NoOverlay.Routing = config.NoOverlayRoutingManaged
		config.ManagedBGP.Topology = config.ManagedBGPTopologyFullMesh
		config.ManagedBGP.ASNumber = 64512
		config.ManagedBGP.FRRNamespace = "frr-k8s-system"
		config.Kubernetes.OVNConfigNamespace = "ovn-kubernetes"
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableRouteAdvertisements = true

		recorder = record.NewFakeRecorder(100)
	})

	ginkgo.AfterEach(func() {
		// Restore original config
		config.ManagedBGP.Topology = oldTopology
		config.ManagedBGP.ASNumber = oldASNumber
		config.ManagedBGP.FRRNamespace = oldFRRNamespace
		config.Kubernetes.OVNConfigNamespace = oldOVNConfigNamespace
		config.OVNKubernetesFeature.EnableMultiNetwork = oldEnableMultiNetwork
		config.OVNKubernetesFeature.EnableRouteAdvertisements = oldEnableRouteAdvertise
		config.Default.Transport = oldTransport
		config.NoOverlay.Routing = oldNoOverlayRouting
	})

	ginkgo.Context("Controller initialization", func() {
		ginkgo.It("should create a new controller", func() {
			fakeClient := fake.NewSimpleClientset()
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			controller := NewController(wf, frrFakeClient, raFakeClient, recorder)
			gomega.Expect(controller).NotTo(gomega.BeNil())
			gomega.Expect(controller.frrClient).To(gomega.Equal(frrFakeClient))
			gomega.Expect(controller.wf).To(gomega.Equal(wf))
			gomega.Expect(controller.recorder).To(gomega.Equal(recorder))
			gomega.Expect(controller.nodeController).NotTo(gomega.BeNil())
		})
	})

	ginkgo.Context("Full-mesh topology", func() {
		ginkgo.BeforeEach(func() {
			config.ManagedBGP.Topology = config.ManagedBGPTopologyFullMesh
		})

		ginkgo.It("should create base FRRConfiguration with IPv4 nodes", func() {
			node1 := createNode("node1", "10.0.0.1", "")
			node2 := createNode("node2", "10.0.0.2", "")

			fakeClient := fake.NewSimpleClientset(node1, node2)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller := NewController(wf, frrFakeClient, raFakeClient, recorder)
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			// Should create one base FRRConfiguration
			gomega.Eventually(func() int {
				list, _ := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).List(context.TODO(), metav1.ListOptions{})
				return len(list.Items)
			}, 2*time.Second).Should(gomega.Equal(1))

			// Verify base configuration
			baseConfig, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(baseConfig.Labels).To(gomega.HaveKeyWithValue(FRRConfigManagedLabel, FRRConfigManagedValue))
			gomega.Expect(baseConfig.Spec.BGP.Routers).To(gomega.HaveLen(1))
			gomega.Expect(baseConfig.Spec.BGP.Routers[0].ASN).To(gomega.Equal(uint32(64512)))
			gomega.Expect(baseConfig.Spec.BGP.Routers[0].Neighbors).To(gomega.HaveLen(2))

			addresses := []string{baseConfig.Spec.BGP.Routers[0].Neighbors[0].Address, baseConfig.Spec.BGP.Routers[0].Neighbors[1].Address}
			gomega.Expect(addresses).To(gomega.ConsistOf("10.0.0.1", "10.0.0.2"))

			// Verify DisableMP is set
			for _, neighbor := range baseConfig.Spec.BGP.Routers[0].Neighbors {
				gomega.Expect(neighbor.DisableMP).To(gomega.BeTrue())
				gomega.Expect(neighbor.ASN).To(gomega.Equal(uint32(64512)))
			}
		})

		ginkgo.It("should create base FRRConfiguration with IPv6 nodes", func() {
			node1 := createNode("node1", "", "fd00::1")
			node2 := createNode("node2", "", "fd00::2")

			fakeClient := fake.NewSimpleClientset(node1, node2)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller := NewController(wf, frrFakeClient, raFakeClient, recorder)
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			gomega.Eventually(func() int {
				list, _ := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).List(context.TODO(), metav1.ListOptions{})
				return len(list.Items)
			}, 2*time.Second).Should(gomega.Equal(1))

			baseConfig, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(baseConfig.Spec.BGP.Routers[0].Neighbors).To(gomega.HaveLen(2))

			addresses := []string{baseConfig.Spec.BGP.Routers[0].Neighbors[0].Address, baseConfig.Spec.BGP.Routers[0].Neighbors[1].Address}
			gomega.Expect(addresses).To(gomega.ConsistOf("fd00::1", "fd00::2"))
		})

		ginkgo.It("should create base FRRConfiguration with dual-stack nodes", func() {
			node1 := createNode("node1", "10.0.0.1", "fd00::1")
			node2 := createNode("node2", "10.0.0.2", "fd00::2")

			fakeClient := fake.NewSimpleClientset(node1, node2)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller := NewController(wf, frrFakeClient, raFakeClient, recorder)
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			gomega.Eventually(func() int {
				list, _ := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).List(context.TODO(), metav1.ListOptions{})
				return len(list.Items)
			}, 2*time.Second).Should(gomega.Equal(1))

			baseConfig, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// 2 nodes × 2 addresses each = 4 neighbors
			gomega.Expect(baseConfig.Spec.BGP.Routers[0].Neighbors).To(gomega.HaveLen(4))

			addresses := make([]string, 4)
			for i, neighbor := range baseConfig.Spec.BGP.Routers[0].Neighbors {
				addresses[i] = neighbor.Address
			}
			gomega.Expect(addresses).To(gomega.ConsistOf("10.0.0.1", "fd00::1", "10.0.0.2", "fd00::2"))
		})

		ginkgo.It("should update base FRRConfiguration when a node is added", func() {
			node1 := createNode("node1", "10.0.0.1", "")
			node2 := createNode("node2", "10.0.0.2", "")

			fakeClient := fake.NewSimpleClientset(node1, node2)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller := NewController(wf, frrFakeClient, raFakeClient, recorder)
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			// Wait for initial configuration
			gomega.Eventually(func() int {
				bc, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName, metav1.GetOptions{})
				if err != nil {
					return 0
				}
				return len(bc.Spec.BGP.Routers[0].Neighbors)
			}, 2*time.Second).Should(gomega.Equal(2))

			// Add a third node
			node3 := createNode("node3", "10.0.0.3", "")
			_, err = fakeClient.CoreV1().Nodes().Create(context.TODO(), node3, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Should update to include node3
			gomega.Eventually(func() []string {
				bc, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName, metav1.GetOptions{})
				if err != nil {
					return nil
				}
				addresses := make([]string, len(bc.Spec.BGP.Routers[0].Neighbors))
				for i, n := range bc.Spec.BGP.Routers[0].Neighbors {
					addresses[i] = n.Address
				}
				return addresses
			}, 2*time.Second).Should(gomega.ConsistOf("10.0.0.1", "10.0.0.2", "10.0.0.3"))
		})

		ginkgo.It("should update base FRRConfiguration when a node is deleted", func() {
			node1 := createNode("node1", "10.0.0.1", "")
			node2 := createNode("node2", "10.0.0.2", "")

			fakeClient := fake.NewSimpleClientset(node1, node2)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller := NewController(wf, frrFakeClient, raFakeClient, recorder)
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			gomega.Eventually(func() int {
				bc, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName, metav1.GetOptions{})
				if err != nil {
					return 0
				}
				return len(bc.Spec.BGP.Routers[0].Neighbors)
			}, 2*time.Second).Should(gomega.Equal(2))

			// Delete node2
			err = fakeClient.CoreV1().Nodes().Delete(context.TODO(), "node2", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Should update to exclude node2
			gomega.Eventually(func() int {
				bc, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName, metav1.GetOptions{})
				if err != nil {
					return -1
				}
				return len(bc.Spec.BGP.Routers[0].Neighbors)
			}, 2*time.Second).Should(gomega.Equal(1))

			baseConfig, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(baseConfig.Spec.BGP.Routers[0].Neighbors[0].Address).To(gomega.Equal("10.0.0.1"))
		})

		ginkgo.It("should update base FRRConfiguration when a node IP changes", func() {
			node1 := createNode("node1", "10.0.0.1", "")

			fakeClient := fake.NewSimpleClientset(node1)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller := NewController(wf, frrFakeClient, raFakeClient, recorder)
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			gomega.Eventually(func() string {
				bc, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName, metav1.GetOptions{})
				if err != nil || len(bc.Spec.BGP.Routers) == 0 || len(bc.Spec.BGP.Routers[0].Neighbors) == 0 {
					return ""
				}
				return bc.Spec.BGP.Routers[0].Neighbors[0].Address
			}, 2*time.Second).Should(gomega.Equal("10.0.0.1"))

			// Update node IP
			node1Updated := createNode("node1", "10.0.0.99", "")
			_, err = fakeClient.CoreV1().Nodes().Update(context.TODO(), node1Updated, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Should update to new IP
			gomega.Eventually(func() string {
				bc, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName, metav1.GetOptions{})
				if err != nil || len(bc.Spec.BGP.Routers) == 0 || len(bc.Spec.BGP.Routers[0].Neighbors) == 0 {
					return ""
				}
				return bc.Spec.BGP.Routers[0].Neighbors[0].Address
			}, 2*time.Second).Should(gomega.Equal("10.0.0.99"))
		})

		ginkgo.It("should clean up managed resources when managed mode is disabled", func() {
			// Pre-create the resources that would have been created by a previous managed-mode run
			baseFRRConfig := &frrtypes.FRRConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      BaseFRRConfigName,
					Namespace: config.ManagedBGP.FRRNamespace,
					Labels: map[string]string{
						FRRConfigManagedLabel: FRRConfigManagedValue,
					},
				},
				Spec: frrtypes.FRRConfigurationSpec{},
			}
			managedRA := &ratypes.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: DefaultRouteAdvertisementName,
				},
			}

			fakeClient := fake.NewSimpleClientset()
			frrFakeClient := frrfake.NewSimpleClientset(baseFRRConfig)
			raFakeClient := rafake.NewSimpleClientset(managedRA)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Switch to unmanaged mode
			config.NoOverlay.Routing = config.NoOverlayRoutingUnmanaged

			controller := NewController(wf, frrFakeClient, raFakeClient, recorder)
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Base FRRConfiguration should be deleted
			_, err = frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName, metav1.GetOptions{})
			gomega.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())

			// Managed RouteAdvertisement should be deleted
			_, err = raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), DefaultRouteAdvertisementName, metav1.GetOptions{})
			gomega.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
		})

		ginkgo.It("should handle empty cluster (no nodes)", func() {
			fakeClient := fake.NewSimpleClientset()
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller := NewController(wf, frrFakeClient, raFakeClient, recorder)
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			// Create and immediately delete a node to trigger reconciliation
			tempNode := createNode("temp", "10.0.0.99", "")
			_, err = fakeClient.CoreV1().Nodes().Create(context.TODO(), tempNode, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = fakeClient.CoreV1().Nodes().Delete(context.TODO(), "temp", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Should still create base config, just with no neighbors
			gomega.Eventually(func() bool {
				bc, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName, metav1.GetOptions{})
				return err == nil && bc != nil
			}, 2*time.Second).Should(gomega.BeTrue())

			baseConfig, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(baseConfig.Spec.BGP.Routers).To(gomega.HaveLen(1))
			gomega.Expect(baseConfig.Spec.BGP.Routers[0].Neighbors).To(gomega.BeEmpty())
		})
	})

	ginkgo.Context("Non-full-mesh topology", func() {
		ginkgo.It("should not create FRRConfiguration when topology is not full-mesh", func() {
			config.ManagedBGP.Topology = ""

			node1 := createNode("node1", "10.0.0.1", "")

			fakeClient := fake.NewSimpleClientset(node1)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller := NewController(wf, frrFakeClient, raFakeClient, recorder)
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			// Should not create any FRRConfigurations
			gomega.Consistently(func() int {
				list, _ := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).List(context.TODO(), metav1.ListOptions{})
				return len(list.Items)
			}, 1*time.Second, 100*time.Millisecond).Should(gomega.Equal(0))
		})
	})

	ginkgo.Context("nodeNeedsUpdate", func() {
		var controller *Controller

		ginkgo.BeforeEach(func() {
			fakeClient := fake.NewSimpleClientset()
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller = NewController(wf, frrFakeClient, raFakeClient, recorder)
		})

		ginkgo.It("should return true when old node is nil", func() {
			newNode := createNode("node1", "10.0.0.1", "")
			gomega.Expect(controller.nodeNeedsUpdate(nil, newNode)).To(gomega.BeTrue())
		})

		ginkgo.It("should return true when new node is nil", func() {
			oldNode := createNode("node1", "10.0.0.1", "")
			gomega.Expect(controller.nodeNeedsUpdate(oldNode, nil)).To(gomega.BeTrue())
		})

		ginkgo.It("should return true when IPv4 address changes", func() {
			oldNode := createNode("node1", "10.0.0.1", "")
			newNode := createNode("node1", "10.0.0.2", "")
			gomega.Expect(controller.nodeNeedsUpdate(oldNode, newNode)).To(gomega.BeTrue())
		})

		ginkgo.It("should return true when IPv6 address changes", func() {
			oldNode := createNode("node1", "", "fd00::1")
			newNode := createNode("node1", "", "fd00::2")
			gomega.Expect(controller.nodeNeedsUpdate(oldNode, newNode)).To(gomega.BeTrue())
		})

		ginkgo.It("should return false when addresses are the same", func() {
			oldNode := createNode("node1", "10.0.0.1", "fd00::1")
			newNode := createNode("node1", "10.0.0.1", "fd00::1")
			gomega.Expect(controller.nodeNeedsUpdate(oldNode, newNode)).To(gomega.BeFalse())
		})

		ginkgo.It("should return false when only labels change", func() {
			oldNode := createNode("node1", "10.0.0.1", "")
			newNode := createNode("node1", "10.0.0.1", "")
			newNode.Labels["foo"] = "bar"
			gomega.Expect(controller.nodeNeedsUpdate(oldNode, newNode)).To(gomega.BeFalse())
		})
	})

	ginkgo.Context("RouteAdvertisements management", func() {
		ginkgo.BeforeEach(func() {
			config.ManagedBGP.Topology = config.ManagedBGPTopologyFullMesh
			config.Default.Transport = types.NetworkTransportNoOverlay
			config.NoOverlay.Routing = config.NoOverlayRoutingManaged
		})

		ginkgo.It("should create managed RouteAdvertisement on controller start", func() {
			fakeClient := fake.NewSimpleClientset()
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller := NewController(wf, frrFakeClient, raFakeClient, recorder)
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			// Verify managed RouteAdvertisement was created
			gomega.Eventually(func() error {
				_, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), DefaultRouteAdvertisementName, metav1.GetOptions{})
				return err
			}, 2*time.Second).Should(gomega.Succeed())

			ra, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), DefaultRouteAdvertisementName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(ra.Spec.FRRConfigurationSelector.MatchLabels).To(gomega.HaveKeyWithValue(FRRConfigManagedLabel, FRRConfigManagedValue))
			gomega.Expect(ra.Spec.Advertisements).To(gomega.ConsistOf(ratypes.PodNetwork))
		})
	})

	ginkgo.Context("raNeedsUpdate", func() {
		var controller *Controller

		ginkgo.BeforeEach(func() {
			fakeClient := fake.NewSimpleClientset()
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller = NewController(wf, frrFakeClient, raFakeClient, recorder)
		})

		ginkgo.It("should return false for non-managed RouteAdvertisement", func() {
			ra := createTestRouteAdvertisement("some-other-ra")
			gomega.Expect(controller.raNeedsUpdate(nil, ra)).To(gomega.BeFalse())
			gomega.Expect(controller.raNeedsUpdate(ra, nil)).To(gomega.BeFalse())
		})

		ginkgo.It("should return true when managed RouteAdvertisement spec changes", func() {
			oldRA := createTestRouteAdvertisement(DefaultRouteAdvertisementName)
			newRA := oldRA.DeepCopy()
			newRA.Spec.Advertisements = []ratypes.AdvertisementType{ratypes.EgressIP}
			gomega.Expect(controller.raNeedsUpdate(oldRA, newRA)).To(gomega.BeTrue())
		})
	})
})

// Helper function to create test nodes
func createNode(name, ipv4, ipv6 string) *corev1.Node {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"kubernetes.io/hostname": name,
			},
			Annotations: map[string]string{},
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{},
		},
	}

	// Build annotation
	var annotation string
	if ipv4 != "" && ipv6 != "" {
		annotation = `{"ipv4":"` + ipv4 + `/24","ipv6":"` + ipv6 + `/64"}`
		node.Status.Addresses = append(node.Status.Addresses,
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ipv4},
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ipv6},
		)
	} else if ipv4 != "" {
		annotation = `{"ipv4":"` + ipv4 + `/24"}`
		node.Status.Addresses = append(node.Status.Addresses,
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ipv4},
		)
	} else if ipv6 != "" {
		annotation = `{"ipv6":"` + ipv6 + `/64"}`
		node.Status.Addresses = append(node.Status.Addresses,
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ipv6},
		)
	}

	if annotation != "" {
		node.Annotations[util.OvnNodeIfAddr] = annotation
	}

	return node
}

// Helper function to create test RouteAdvertisement
func createTestRouteAdvertisement(name string) *ratypes.RouteAdvertisements {
	return &ratypes.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: ratypes.RouteAdvertisementsSpec{
			NetworkSelectors: apitypes.NetworkSelectors{{NetworkSelectionType: apitypes.DefaultNetwork}},
			Advertisements: []ratypes.AdvertisementType{
				ratypes.PodNetwork,
			},
			FRRConfigurationSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					FRRConfigManagedLabel: FRRConfigManagedValue,
				},
			},
			NodeSelector: metav1.LabelSelector{},
		},
	}
}
