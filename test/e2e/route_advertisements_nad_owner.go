// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net"
	"time"

	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
	frrapi "github.com/metallb/frr-k8s/api/v1beta1"
	frrclient "github.com/metallb/frr-k8s/pkg/client/clientset/versioned"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	rav1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	"k8s.io/utils/ptr"
)

var _ = ginkgo.Describe("BGP: NADs not rendered by an advertised CUDN", feature.RouteAdvertisements, func() {
	f := wrappedTestFramework("ra-nad-owner")
	f.SkipNamespaceCreation = true

	ginkgo.It("are not advertised when they copy the owner reference of the CUDN NAD", func(ctx ginkgo.SpecContext) {
		ictx := infraprovider.Get().NewTestContext()
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, f.ClientSet, 1)
		framework.ExpectNoError(err, "failed to get a schedulable node")
		gomega.Expect(nodes.Items).NotTo(gomega.BeEmpty(), "no schedulable node found")
		suffix := framework.RandomSuffix()
		cudnName := "nad-owner-" + suffix
		const labelKey = "e2e-ra-nad-owner"
		networkLabels := map[string]string{labelKey: suffix}

		selectedNamespace, err := f.CreateNamespace(ctx, "ra-nad-owner-selected", map[string]string{RequiredUDNNamespaceLabel: ""})
		framework.ExpectNoError(err, "failed to create the namespace selected by the CUDN")
		f.Namespace = selectedNamespace
		otherNamespace, err := f.CreateNamespace(ctx, "ra-nad-owner-other", map[string]string{RequiredUDNNamespaceLabel: ""})
		framework.ExpectNoError(err, "failed to create the namespace not selected by the CUDN")

		// canonical form of the CIDRs of the cluster IP families, as found in
		// the generated FRRConfigurations
		supportedCIDRs := func(cidrs ...string) []string {
			var supported []string
			for _, cidr := range filterCIDRs(f.ClientSet, cidrs...) {
				_, ipNet, err := net.ParseCIDR(cidr)
				framework.ExpectNoError(err, "failed to parse CIDR %s", cidr)
				supported = append(supported, ipNet.String())
			}
			return supported
		}
		cudnAllocation, err := allocators.AllocateBGP(f, ictx)
		framework.ExpectNoError(err, "failed to allocate the CUDN subnets")
		cudnCIDRs := supportedCIDRs(cudnAllocation.UDNSubnet, cudnAllocation.UDNSubnet6)
		nadAllocation, err := allocators.AllocateBGP(f, ictx)
		framework.ExpectNoError(err, "failed to allocate the NAD subnets")
		nadCIDRs := supportedCIDRs(nadAllocation.UDNSubnet, nadAllocation.UDNSubnet6)

		ginkgo.By("creating a CUDN selecting one of the two namespaces")
		var cudnSubnets udnv1.DualStackCIDRs
		for _, cidr := range cudnCIDRs {
			cudnSubnets = append(cudnSubnets, udnv1.CIDR(cidr))
		}
		framework.ExpectNoError(createUserDefinedNetwork(f, ictx, selectedNamespace, cudnName, true, &udnv1.NetworkSpec{
			Topology: udnv1.NetworkTopologyLayer2,
			Layer2:   &udnv1.Layer2Config{Role: udnv1.NetworkRolePrimary, Subnets: cudnSubnets},
		}, networkLabels), "failed to create CUDN %s", cudnName)
		ginkgo.DeferCleanup(func() error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := f.ClientSet.CoreV1().Pods(selectedNamespace.Name).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{}); err != nil {
				return fmt.Errorf("failed to delete the pods of namespace %s: %w", selectedNamespace.Name, err)
			}
			return nil
		})
		// Keep the network active on a node with dynamic allocation.
		_, err = createPod(f, "keep-network-active", nodes.Items[0].Name, selectedNamespace.Name, []string{"bash", "-c", "sleep infinity"}, nil)
		framework.ExpectNoError(err, "failed to create the pod keeping the network active")

		ginkgo.By("creating a NAD in the other namespace with the name, labels and owner reference of the CUDN NAD")
		cudn, err := f.DynamicClient.Resource(clusterUDNGVR).Get(ctx, cudnName, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get CUDN %s", cudnName)
		nad := generateNAD(newNetworkAttachmentConfig(networkAttachmentConfigParams{
			name:      cudnName,
			namespace: otherNamespace.Name,
			topology:  "layer2",
			cidr:      joinStrings(nadCIDRs...),
			role:      "primary",
		}), f.ClientSet)
		nad.Labels = networkLabels
		nad.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: cudn.GetAPIVersion(),
			Kind:       cudn.GetKind(),
			Name:       cudn.GetName(),
			UID:        cudn.GetUID(),
			Controller: ptr.To(true),
		}}
		nadClient, err := nadclient.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err, "failed to create the NAD client")
		_, err = nadClient.NetworkAttachmentDefinitions(otherNamespace.Name).Create(ctx, nad, metav1.CreateOptions{})
		framework.ExpectNoError(err, "failed to create NAD %s/%s", otherNamespace.Name, cudnName)

		// Keep the NAD network active on the same node, so that only the
		// owner check keeps it from being advertised with dynamic allocation.
		_, err = createPod(f, "keep-nad-network-active", nodes.Items[0].Name, otherNamespace.Name, []string{"bash", "-c", "sleep infinity"}, nil)
		framework.ExpectNoError(err, "failed to create the pod keeping the NAD network active")

		ginkgo.By("creating a RouteAdvertisements selecting the CUDN")
		// No live peer is needed: this test checks the generated FRRConfigurations.
		var neighbors []frrapi.Neighbor
		for _, address := range []string{"192.0.2.1", "2001:db8::1"} {
			if isIPFamilySupported(f.ClientSet, address) {
				neighbors = append(neighbors, frrapi.Neighbor{ASN: 64513, Address: address})
			}
		}
		frr, err := frrclient.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err, "failed to create the FRR client")
		frrConfigurations := frr.ApiV1beta1().FRRConfigurations(deploymentconfig.Get().FRRK8sNamespace())
		source, err := frrConfigurations.Create(ctx, &frrapi.FRRConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: cudnName, Labels: networkLabels},
			Spec:       frrapi.FRRConfigurationSpec{BGP: frrapi.BGPConfig{Routers: []frrapi.Router{{ASN: 64512, Neighbors: neighbors}}}},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err, "failed to create FRRConfiguration %s", cudnName)
		ginkgo.DeferCleanup(func() error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := frrConfigurations.Delete(ctx, source.Name, metav1.DeleteOptions{}); err != nil {
				return fmt.Errorf("failed to delete FRRConfiguration %s: %w", source.Name, err)
			}
			return nil
		})
		client, err := raclient.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err, "failed to create the RouteAdvertisements client")
		ra, err := client.K8sV1().RouteAdvertisements().Create(ctx, &rav1.RouteAdvertisements{
			ObjectMeta: metav1.ObjectMeta{Name: cudnName},
			Spec: rav1.RouteAdvertisementsSpec{Advertisements: []rav1.AdvertisementType{rav1.PodNetwork},
				FRRConfigurationSelector: metav1.LabelSelector{MatchLabels: networkLabels},
				NetworkSelectors: apitypes.NetworkSelectors{{NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
					ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{NetworkSelector: metav1.LabelSelector{MatchLabels: networkLabels}}},
				},
			},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err, "failed to create RouteAdvertisements %s", cudnName)
		ginkgo.DeferCleanup(func() error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := client.K8sV1().RouteAdvertisements().Delete(ctx, ra.Name, metav1.DeleteOptions{}); err != nil {
				return fmt.Errorf("failed to delete RouteAdvertisements %s: %w", ra.Name, err)
			}
			return nil
		})
		gomega.Eventually(ctx, routeAdvertisementsReadyFunc(*client, ra.Name), time.Minute, time.Second).Should(gomega.Succeed(), "RouteAdvertisements %s is not accepted", ra.Name)

		advertisedPrefixes := func(g gomega.Gomega) sets.Set[string] {
			generated, err := frrConfigurations.List(ctx, metav1.ListOptions{
				LabelSelector: labels.Set{types.OvnRouteAdvertisementsKey: ra.Name}.String(),
			})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "failed to list the FRRConfigurations generated for RouteAdvertisements %s", ra.Name)
			prefixes := sets.New[string]()
			for _, frrConfiguration := range generated.Items {
				for _, router := range frrConfiguration.Spec.BGP.Routers {
					prefixes.Insert(router.Prefixes...)
				}
			}
			return prefixes
		}

		ginkgo.By("observing the CUDN subnets in the generated FRRConfigurations")
		gomega.Eventually(ctx, func(g gomega.Gomega) {
			g.Expect(advertisedPrefixes(g).HasAll(cudnCIDRs...)).To(gomega.BeTrue(), "CUDN subnets %v must be advertised", cudnCIDRs)
		}, time.Minute, time.Second).Should(gomega.Succeed())

		ginkgo.By("observing that the NAD subnets do not get to the generated FRRConfigurations")
		gomega.Consistently(ctx, func(g gomega.Gomega) {
			g.Expect(advertisedPrefixes(g).HasAny(nadCIDRs...)).To(gomega.BeFalse(), "NAD subnets %v must not be advertised", nadCIDRs)
		}, 15*time.Second, time.Second).Should(gomega.Succeed())
	})
})
