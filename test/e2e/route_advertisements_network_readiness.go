// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"time"

	frrclient "github.com/metallb/frr-k8s/pkg/client/clientset/versioned"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	rav1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/kubernetes/test/e2e/framework"
	utilnet "k8s.io/utils/net"
)

// A CUDN's generated NAD and its network-manager state are delivered through
// different asynchronous paths. This test creates the RouteAdvertisements
// first, then watches the first Accepted=True transition after creating the
// selected CUDN. Acceptance is only valid once the generated FRRConfiguration
// objects already contain the selected network prefixes.
var _ = ginkgo.Describe("BGP: RouteAdvertisements waits for selected network state", feature.RouteAdvertisements, func() {
	f := wrappedTestFramework("ra-network-readiness")
	f.SkipNamespaceCreation = true

	ginkgo.It("generates the selected CUDN routes before reporting Accepted", ginkgo.SpecTimeout(10*time.Minute), func(ctx ginkgo.SpecContext) {
		if isDynamicUDNEnabled() {
			ginkgo.Skip("requires the Layer2 network to be rendered on every node")
		}

		ictx := infraprovider.Get().NewTestContext()
		suffix := framework.RandomSuffix()
		name := "ra-ready-" + suffix
		const networkLabel = "e2e-route-advertisements-network-readiness"

		namespace, err := f.CreateNamespace(ctx, name, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		framework.ExpectNoError(err, "creating the CUDN namespace")
		f.Namespace = namespace

		allocation, err := allocators.AllocateBGP(f, ictx)
		framework.ExpectNoError(err, "allocating BGP test networks")
		ipFamilies := sets.New(getSupportedIPFamiliesSlice(f.ClientSet)...)
		udnSubnets := filterDualStackCIDRs(f.ClientSet, udnv1.DualStackCIDRs{
			udnv1.CIDR(allocation.UDNSubnet),
			udnv1.CIDR(allocation.UDNSubnet6),
		})
		gomega.Expect(udnSubnets).NotTo(gomega.BeEmpty(), "cluster reports no supported IP families")

		ginkgo.By("creating isolated BGP peers before the RouteAdvertisements")
		framework.ExpectNoError(runBGPNetworkAndServerWithFRRVRF(
			f,
			ictx,
			ipFamilies,
			name,
			name+"-server",
			name+"-server-net",
			[]string{allocation.BGPPeerSubnet, allocation.BGPPeerSubnet6},
			[]string{allocation.IPVRFSubnet, allocation.IPVRFSubnet6},
			"",
		), "creating the BGP test infrastructure")

		raClient, err := raclient.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err, "creating the RouteAdvertisements client")
		udnClient, err := udnclient.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err, "creating the ClusterUserDefinedNetwork client")
		frrClient, err := frrclient.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err, "creating the FRRConfiguration client")

		ginkgo.By("creating RouteAdvertisements before its selected CUDN exists")
		ra, err := raClient.K8sV1().RouteAdvertisements().Create(ctx, &rav1.RouteAdvertisements{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: rav1.RouteAdvertisementsSpec{
				NetworkSelectors: apitypes.NetworkSelectors{{
					NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
					ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
						NetworkSelector: metav1.LabelSelector{MatchLabels: map[string]string{networkLabel: name}},
					},
				}},
				NodeSelector:             metav1.LabelSelector{},
				FRRConfigurationSelector: metav1.LabelSelector{MatchLabels: map[string]string{"network": name}},
				Advertisements:           []rav1.AdvertisementType{rav1.PodNetwork},
			},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err, "creating RouteAdvertisements %s", name)
		ictx.AddCleanUpFn(func() error {
			err := raClient.K8sV1().RouteAdvertisements().Delete(context.Background(), ra.Name, metav1.DeleteOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		})

		ginkgo.By("confirming the RouteAdvertisements is pending without a selected network")
		gomega.Eventually(ctx, func(g gomega.Gomega) {
			current, err := raClient.K8sV1().RouteAdvertisements().Get(ctx, ra.Name, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			condition := meta.FindStatusCondition(current.Status.Conditions, rav1.RouteAdvertisementsAccepted)
			g.Expect(condition).NotTo(gomega.BeNil(), "RouteAdvertisements conditions: %+v", current.Status.Conditions)
			g.Expect(condition.ObservedGeneration).To(gomega.Equal(current.Generation))
			g.Expect(condition.Status).To(gomega.Equal(metav1.ConditionFalse), condition.Message)
			g.Expect(condition.Reason).To(gomega.Equal("ConfigurationPending"), condition.Message)
			g.Expect(condition.Message).To(gomega.ContainSubstring("no networks selected"))
		}, time.Minute, time.Second).Should(gomega.Succeed())

		// Start watching after observing the pending generation. If the old code
		// briefly reports Accepted=True with an empty generated configuration and
		// later repairs itself, checking only the final state would miss it.
		watchCtx, cancelWatch := context.WithTimeout(ctx, 2*time.Minute)
		defer cancelWatch()
		raWatch, err := raClient.K8sV1().RouteAdvertisements().Watch(watchCtx, metav1.ListOptions{
			FieldSelector: "metadata.name=" + ra.Name,
		})
		framework.ExpectNoError(err, "watching RouteAdvertisements %s", ra.Name)
		defer raWatch.Stop()

		ginkgo.By("creating the selected Layer2 primary CUDN")
		cudn, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Create(ctx, &udnv1.ClusterUserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: map[string]string{networkLabel: name},
			},
			Spec: udnv1.ClusterUserDefinedNetworkSpec{
				NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{
					"kubernetes.io/metadata.name": namespace.Name,
				}},
				Network: udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer2,
					Layer2: &udnv1.Layer2Config{
						Role:    udnv1.NetworkRolePrimary,
						IPAM:    &udnv1.IPAMConfig{Lifecycle: udnv1.IPAMLifecyclePersistent},
						Subnets: udnSubnets,
					},
				},
			},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err, "creating ClusterUserDefinedNetwork %s", name)
		ictx.AddCleanUpFn(func() error {
			err := udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.Background(), cudn.Name, metav1.DeleteOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		})

		ginkgo.By("checking generated FRRConfigurations at the first Accepted=True transition")
		var accepted *rav1.RouteAdvertisements
		for accepted == nil {
			select {
			case <-watchCtx.Done():
				ginkgo.Fail(fmt.Sprintf("RouteAdvertisements %s did not become accepted: %v", ra.Name, watchCtx.Err()))
			case event, ok := <-raWatch.ResultChan():
				if !ok {
					ginkgo.Fail(fmt.Sprintf("RouteAdvertisements %s watch closed before acceptance", ra.Name))
				}
				if event.Type == watch.Error {
					ginkgo.Fail(fmt.Sprintf("RouteAdvertisements %s watch failed: %v", ra.Name, event.Object))
				}
				current, ok := event.Object.(*rav1.RouteAdvertisements)
				if !ok {
					continue
				}
				condition := meta.FindStatusCondition(current.Status.Conditions, rav1.RouteAdvertisementsAccepted)
				if condition != nil && condition.ObservedGeneration == current.Generation && condition.Status == metav1.ConditionTrue {
					accepted = current
				}
			}
		}

		generatedSelector := labels.Set{ovntypes.OvnRouteAdvertisementsKey: accepted.Name}.AsSelector().String()
		generated, err := frrClient.ApiV1beta1().FRRConfigurations(deploymentconfig.Get().FRRK8sNamespace()).List(
			ctx, metav1.ListOptions{LabelSelector: generatedSelector})
		framework.ExpectNoError(err, "listing FRRConfigurations generated for RouteAdvertisements %s", accepted.Name)

		nodes, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		framework.ExpectNoError(err, "listing cluster nodes")
		expectedNodes := sets.New[string]()
		for _, node := range nodes.Items {
			expectedNodes.Insert(node.Name)
		}
		generatedNodes := sets.New[string]()
		for _, config := range generated.Items {
			nodeName := config.Spec.NodeSelector.MatchLabels["kubernetes.io/hostname"]
			gomega.Expect(nodeName).NotTo(gomega.BeEmpty(), "generated FRRConfiguration %s has no node selector", config.Name)
			generatedNodes.Insert(nodeName)
			prefixes := sets.New[string]()
			for _, router := range config.Spec.BGP.Routers {
				prefixes.Insert(router.Prefixes...)
			}
			for _, subnet := range udnSubnets {
				gomega.Expect(prefixes.Has(string(subnet))).To(gomega.BeTrue(),
					"generated FRRConfiguration %s for node %s does not advertise %s; prefixes=%v",
					config.Name, nodeName, subnet, sets.List(prefixes))
			}
		}
		gomega.Expect(sets.List(generatedNodes)).To(gomega.ConsistOf(sets.List(expectedNodes)),
			"RouteAdvertisements became Accepted before every node had a generated FRRConfiguration")

		ginkgo.By("waiting for the selected CUDN to finish provisioning")
		gomega.Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudn.Name),
			time.Minute, time.Second).Should(gomega.Succeed())

		ginkgo.By("checking that the external FRR router learns the selected Layer2 CUDN prefixes")
		routerName := name + "-frr"
		for _, node := range nodes.Items {
			for _, subnet := range udnSubnets {
				checkRouteInFRR(node, string(subnet), routerName, utilnet.IsIPv6CIDRString(string(subnet)))
			}
		}
	})
})
