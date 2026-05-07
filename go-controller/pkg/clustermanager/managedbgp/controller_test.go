// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package managedbgp

import (
	"context"
	"fmt"
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
			gomega.Expect(controller.managedRAController).NotTo(gomega.BeNil())
			gomega.Expect(controller.managedFRRController).NotTo(gomega.BeNil())
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
			baseConfig, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName(), metav1.GetOptions{})
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

			baseConfig, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName(), metav1.GetOptions{})
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

			baseConfig, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName(), metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// 2 nodes × 2 addresses each = 4 neighbors
			gomega.Expect(baseConfig.Spec.BGP.Routers[0].Neighbors).To(gomega.HaveLen(4))

			addresses := make([]string, 4)
			for i, neighbor := range baseConfig.Spec.BGP.Routers[0].Neighbors {
				addresses[i] = neighbor.Address
			}
			gomega.Expect(addresses).To(gomega.ConsistOf("10.0.0.1", "fd00::1", "10.0.0.2", "fd00::2"))
		})

		ginkgo.It("should remove stale managed base FRRConfigurations", func() {
			node1 := createNode("node1", "10.0.0.1", "")
			staleBaseConfig := &frrtypes.FRRConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ovnk-managed-stale",
					Namespace: config.ManagedBGP.FRRNamespace,
					Labels: map[string]string{
						FRRConfigManagedLabel: FRRConfigManagedValue,
					},
				},
				Spec: frrtypes.FRRConfigurationSpec{},
			}

			fakeClient := fake.NewSimpleClientset(node1)
			frrFakeClient := frrfake.NewSimpleClientset(staleBaseConfig)
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

			gomega.Eventually(func() bool {
				_, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), "ovnk-managed-stale", metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}, 2*time.Second).Should(gomega.BeTrue())

			baseConfig, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName(), metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(baseConfig.Labels).To(gomega.HaveKeyWithValue(FRRConfigManagedLabel, FRRConfigManagedValue))
			gomega.Expect(baseConfig.Spec.BGP.Routers[0].Neighbors).To(gomega.HaveLen(1))
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
				bc, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName(), metav1.GetOptions{})
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
				bc, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName(), metav1.GetOptions{})
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
				bc, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName(), metav1.GetOptions{})
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
				bc, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName(), metav1.GetOptions{})
				if err != nil {
					return -1
				}
				return len(bc.Spec.BGP.Routers[0].Neighbors)
			}, 2*time.Second).Should(gomega.Equal(1))

			baseConfig, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName(), metav1.GetOptions{})
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
				bc, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName(), metav1.GetOptions{})
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
				bc, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName(), metav1.GetOptions{})
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
					Name:      BaseFRRConfigName(),
					Namespace: config.ManagedBGP.FRRNamespace,
					Labels: map[string]string{
						FRRConfigManagedLabel: FRRConfigManagedValue,
					},
				},
				Spec: frrtypes.FRRConfigurationSpec{},
			}
			managedRA := &ratypes.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: ManagedRouteAdvertisementName(types.DefaultNetworkName),
					Labels: map[string]string{
						ManagedRANetworkLabel: types.DefaultNetworkName,
					},
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
			defer controller.Stop()

			// Base FRRConfiguration should be deleted
			gomega.Eventually(func() bool {
				_, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName(), metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}, 2*time.Second).Should(gomega.BeTrue())

			// Managed RouteAdvertisement should be deleted
			gomega.Eventually(func() bool {
				_, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), ManagedRouteAdvertisementName(types.DefaultNetworkName), metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}, 2*time.Second).Should(gomega.BeTrue())
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

			waitForBaseNeighbors := func(expected ...string) {
				gomega.Eventually(func() error {
					baseConfig, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName(), metav1.GetOptions{})
					if err != nil {
						return err
					}
					if len(baseConfig.Spec.BGP.Routers) != 1 {
						return fmt.Errorf("expected one BGP router, got %d", len(baseConfig.Spec.BGP.Routers))
					}
					neighbors := baseConfig.Spec.BGP.Routers[0].Neighbors
					if len(neighbors) != len(expected) {
						return fmt.Errorf("expected neighbors %v, got %v", expected, neighbors)
					}
					for i, expectedAddress := range expected {
						if neighbors[i].Address != expectedAddress {
							return fmt.Errorf("expected neighbor %d to be %q, got %q", i, expectedAddress, neighbors[i].Address)
						}
					}
					return nil
				}, 2*time.Second).Should(gomega.Succeed())
			}

			// Should create the base config with no neighbors when no nodes exist.
			waitForBaseNeighbors()

			// Add a node and wait until the controller has reconciled it before
			// deleting it. Otherwise the test can race with the queued add event.
			tempNode := createNode("temp", "10.0.0.99", "")
			_, err = fakeClient.CoreV1().Nodes().Create(context.TODO(), tempNode, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			waitForBaseNeighbors("10.0.0.99")

			err = fakeClient.CoreV1().Nodes().Delete(context.TODO(), "temp", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Should return to no neighbors after the last node is deleted.
			waitForBaseNeighbors()
		})
	})

	ginkgo.Context("Non-full-mesh topology", func() {
		ginkgo.It("should fail to start when topology is not full-mesh", func() {
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
			gomega.Expect(err).To(gomega.MatchError("initial sync failed: unsupported managed BGP topology: "))

			// Start should fail before creating the base FRRConfiguration
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
				_, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), ManagedRouteAdvertisementName(types.DefaultNetworkName), metav1.GetOptions{})
				return err
			}, 2*time.Second).Should(gomega.Succeed())

			ra, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), ManagedRouteAdvertisementName(types.DefaultNetworkName), metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(ra.Labels).To(gomega.HaveKeyWithValue(ManagedRANetworkLabel, types.DefaultNetworkName))
			gomega.Expect(ra.Spec.FRRConfigurationSelector.MatchLabels).To(gomega.HaveKeyWithValue(FRRConfigManagedLabel, FRRConfigManagedValue))
			gomega.Expect(ra.Spec.Advertisements).To(gomega.ConsistOf(ratypes.PodNetwork))
			gomega.Expect(ra.Spec.NetworkSelectors).To(gomega.HaveLen(1))
			gomega.Expect(ra.Spec.NetworkSelectors[0].NetworkSelectionType).To(gomega.Equal(apitypes.DefaultNetwork))
		})

		ginkgo.It("should recreate managed RouteAdvertisement when deleted", func() {
			node := createNode("node1", "10.0.0.1", "")
			fakeClient := fake.NewSimpleClientset(node)
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

			raName := ManagedRouteAdvertisementName(types.DefaultNetworkName)
			gomega.Eventually(func() error {
				_, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), raName, metav1.GetOptions{})
				return err
			}, 2*time.Second).Should(gomega.Succeed())

			err = raFakeClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), raName, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() error {
				ra, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), raName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if ra.Labels[ManagedRANetworkLabel] != types.DefaultNetworkName {
					return fmt.Errorf("managed label not restored")
				}
				if ra.Spec.NetworkSelectors[0].NetworkSelectionType != apitypes.DefaultNetwork {
					return fmt.Errorf("default network selector not restored")
				}
				return nil
			}, 2*time.Second).Should(gomega.Succeed())
		})

		ginkgo.It("should repair managed RouteAdvertisement drift", func() {
			node := createNode("node1", "10.0.0.1", "")
			fakeClient := fake.NewSimpleClientset(node)
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

			raName := ManagedRouteAdvertisementName(types.DefaultNetworkName)
			ra, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), raName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			drifted := ra.DeepCopy()
			drifted.Labels = map[string]string{"drifted": "true"}
			drifted.Spec.FRRConfigurationSelector.MatchLabels = map[string]string{"drifted": "true"}
			_, err = raFakeClient.K8sV1().RouteAdvertisements().Update(context.TODO(), drifted, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() error {
				ra, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), raName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if ra.Labels[ManagedRANetworkLabel] != types.DefaultNetworkName {
					return fmt.Errorf("managed label not restored")
				}
				if ra.Spec.FRRConfigurationSelector.MatchLabels[FRRConfigManagedLabel] != FRRConfigManagedValue {
					return fmt.Errorf("frr selector not restored")
				}
				return nil
			}, 2*time.Second).Should(gomega.Succeed())
		})
	})

	ginkgo.Context("Base FRRConfiguration self-healing", func() {
		ginkgo.It("should recreate base FRRConfiguration when deleted", func() {
			node := createNode("node1", "10.0.0.1", "")
			fakeClient := fake.NewSimpleClientset(node)
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

			baseName := BaseFRRConfigName()
			gomega.Eventually(func() error {
				_, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), baseName, metav1.GetOptions{})
				return err
			}, 2*time.Second).Should(gomega.Succeed())

			err = frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Delete(context.TODO(), baseName, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() error {
				cfg, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), baseName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if cfg.Labels[FRRConfigManagedLabel] != FRRConfigManagedValue {
					return fmt.Errorf("managed label not restored")
				}
				if len(cfg.Spec.BGP.Routers) != 1 || len(cfg.Spec.BGP.Routers[0].Neighbors) != 1 {
					return fmt.Errorf("expected single-node full-mesh config to be restored")
				}
				return nil
			}, 2*time.Second).Should(gomega.Succeed())
		})

		ginkgo.It("should repair base FRRConfiguration drift", func() {
			node := createNode("node1", "10.0.0.1", "")
			fakeClient := fake.NewSimpleClientset(node)
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

			baseName := BaseFRRConfigName()
			baseConfig, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), baseName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			drifted := baseConfig.DeepCopy()
			drifted.Labels = map[string]string{"drifted": "true"}
			drifted.Spec.BGP.Routers[0].Neighbors = []frrtypes.Neighbor{{
				Address:   "10.0.0.99",
				ASN:       config.ManagedBGP.ASNumber,
				DisableMP: false,
			}}
			_, err = frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Update(context.TODO(), drifted, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() error {
				cfg, err := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), baseName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if cfg.Labels[FRRConfigManagedLabel] != FRRConfigManagedValue {
					return fmt.Errorf("managed label not restored")
				}
				if len(cfg.Spec.BGP.Routers) != 1 || len(cfg.Spec.BGP.Routers[0].Neighbors) != 1 {
					return fmt.Errorf("expected single-node full-mesh config to be restored")
				}
				neighbor := cfg.Spec.BGP.Routers[0].Neighbors[0]
				if neighbor.Address != "10.0.0.1" || !neighbor.DisableMP {
					return fmt.Errorf("base FRRConfiguration not restored")
				}
				return nil
			}, 2*time.Second).Should(gomega.Succeed())
		})
	})

	ginkgo.Context("ensureManagedRouteAdvertisement for CUDN", func() {
		ginkgo.It("should build correct CUDN network selector", func() {
			config.ManagedBGP.Topology = config.ManagedBGPTopologyFullMesh

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
			err = controller.ensureManagedRouteAdvertisement("blue")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ra, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), ManagedRouteAdvertisementName("blue"), metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(ra.Labels).To(gomega.HaveKeyWithValue(ManagedRANetworkLabel, "blue"))
			gomega.Expect(ra.Spec.NetworkSelectors).To(gomega.HaveLen(1))
			gomega.Expect(ra.Spec.NetworkSelectors[0].NetworkSelectionType).To(gomega.Equal(apitypes.ClusterUserDefinedNetworks))
			gomega.Expect(ra.Spec.NetworkSelectors[0].ClusterUserDefinedNetworkSelector).NotTo(gomega.BeNil())
			gomega.Expect(ra.Spec.NetworkSelectors[0].ClusterUserDefinedNetworkSelector.NetworkSelector.MatchLabels).To(
				gomega.HaveKeyWithValue(ManagedRANetworkLabel, "blue"),
			)
			gomega.Expect(ra.Spec.Advertisements).To(gomega.ConsistOf(ratypes.PodNetwork))
			gomega.Expect(ra.Spec.FRRConfigurationSelector.MatchLabels).To(gomega.HaveKeyWithValue(FRRConfigManagedLabel, FRRConfigManagedValue))
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
