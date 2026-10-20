// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeadvertisements

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	nadtypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	frrapi "github.com/metallb/frr-k8s/api/v1beta1"
	frrfake "github.com/metallb/frr-k8s/pkg/client/clientset/versioned/fake"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctesting "k8s.io/client-go/testing"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestController_rejectedUpdateWithdrawsDeselectedNetwork(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	var old []*testFRRConfig
	for _, node := range []string{"node", "node1", "node2"} {
		cfg := generatedFRRConfig("ovnk-generated-"+node, node, true)
		cfg.Routers[0].Prefixes = []string{"1.1.0.0/24", "1.3.0.0/24", "fd03::/64"}
		cfg.Routers[0].Neighbors[0].Advertise = cfg.Routers[0].Prefixes
		cfg.Routers[0].Imports = []string{"blue"}
		cfg.Routers = append(cfg.Routers, &testRouter{ASN: 1, VRF: "blue", Imports: []string{"default"}})
		old = append(old, cfg)
	}
	tt := newFRRReconcileTest(t, g, "eth0", old...)
	config.Gateway.Mode = config.GatewayModeShared
	previousIPv6 := config.IPv6Mode
	t.Cleanup(func() { config.IPv6Mode = previousIPv6 })
	config.IPv6Mode = true
	networks := &networkmanager.FakeNetworkManager{PrimaryNetworks: map[string]util.NetInfo{}}
	tt.c.nm = networks
	for _, name := range []string{"blue", "green"} {
		subnet := "1.3.0.0/16,fd03::/48"
		annotation := `["ra","other"]`
		if name == "green" {
			subnet = "1.3.0.0/16"
			annotation = `["other"]`
		}
		nad := (&testNAD{Name: name, Namespace: name, Network: util.GenerateCUDNNetworkName(name), Topology: types.Layer3Topology, Subnet: subnet,
			Labels: map[string]string{"color": name}, Annotations: map[string]string{types.OvnRouteAdvertisementsKey: annotation}}).NAD()
		network, err := util.ParseNADInfo(nad)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		networks.PrimaryNetworks[name] = network
		_, err = tt.fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(name).Create(ctx, nad, metav1.CreateOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Eventually(func() error { _, err := tt.c.nadLister.NetworkAttachmentDefinitions(name).Get(name); return err }).Should(gomega.Succeed())
	}
	for _, name := range []string{"node1", "node2"} {
		node := (&testNode{Name: name, Labels: map[string]string{corev1.LabelHostname: name}, SubnetsAnnotation: `{"default":"1.1.0.0/24","cluster_udn_green":"1.3.0.0/24"}`}).Node()
		if name == "node2" {
			delete(node.Annotations, "k8s.ovn.org/node-subnets")
		}
		_, err := tt.fakeClientset.KubeClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Eventually(func() error { _, err := tt.c.nodeLister.Get(name); return err }).Should(gomega.Succeed())
	}
	node, err := tt.fakeClientset.KubeClient.CoreV1().Nodes().Get(ctx, "node", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	node.Annotations["k8s.ovn.org/node-subnets"] = `{"default":"1.1.0.0/24","cluster_udn_green":"1.3.0.0/24"}`
	_, err = tt.fakeClientset.KubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Eventually(func() string { n, _ := tt.c.nodeLister.Get("node"); return n.Annotations["k8s.ovn.org/node-subnets"] }).Should(gomega.Equal(node.Annotations["k8s.ovn.org/node-subnets"]))
	source := (&testFRRConfig{Name: "bad-template", Namespace: "frrNamespace", NodeSelector: map[string]string{corev1.LabelHostname: "node1"},
		Routers: []*testRouter{{ASN: 1, VRF: "unrelated", Neighbors: []*testNeighbor{{ASN: 1, Interface: "eth0"}}}}}).FRRConfiguration()
	_, err = tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(source.Namespace).Create(ctx, source, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Eventually(func() error {
		_, err := tt.c.frrLister.FRRConfigurations(source.Namespace).Get(source.Name)
		return err
	}).Should(gomega.Succeed())
	ra, err := tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(ctx, "ra", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	ra.Spec = (&testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true, NetworkSelector: map[string]string{"color": "green"}}).RouteAdvertisements().Spec
	_, err = tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Update(ctx, ra, metav1.UpdateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Eventually(func() int { r, _ := tt.c.raLister.Get("ra"); return len(r.Spec.NetworkSelectors) }).Should(gomega.Equal(2))
	references := func(namespace, name string) []string {
		nad, err := tt.fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Get(ctx, name, metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		var refs []string
		g.Expect(json.Unmarshal([]byte(nad.Annotations[types.OvnRouteAdvertisementsKey]), &refs)).To(gomega.Succeed())
		return refs
	}
	client := tt.fakeClientset.FRRClient.(*frrfake.Clientset)
	failUpdate := true
	client.PrependReactor("update", "frrconfigurations", func(action ctesting.Action) (bool, runtime.Object, error) {
		cfg := action.(ctesting.UpdateAction).GetObject().(*frrapi.FRRConfiguration)
		if failUpdate && cfg.Name == old[1].Name {
			return true, nil, errors.New("injected withdrawal update failure")
		}
		return false, nil, nil
	})
	g.Expect(tt.c.reconcile(ctx, "ra")).NotTo(gomega.Succeed())
	g.Expect(references("blue", "blue")).To(gomega.ConsistOf("ra", "other"), "keep reference until all FRR withdrawals succeed")
	g.Expect(references("green", "green")).To(gomega.ConsistOf("other"))
	failUpdate = false
	tt.failNADPatch.Store(true)
	g.Expect(tt.c.reconcile(ctx, "ra")).NotTo(gomega.Succeed())
	tt.failNADPatch.Store(false)
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
	g.Expect(references("blue", "blue")).To(gomega.ConsistOf("other"))
	g.Expect(references("green", "green")).To(gomega.ConsistOf("other"), "do not publish newly selected network")
	g.Expect(tt.defaultNADAnnotation()).To(gomega.ContainSubstring("ra"))
	for _, original := range old {
		cfg, err := client.ApiV1beta1().FRRConfigurations(original.Namespace).Get(ctx, original.Name, metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(cfg.Spec.BGP.Routers).To(gomega.HaveLen(1))
		g.Expect(cfg.Spec.BGP.Routers[0].Prefixes).To(gomega.Equal([]string{"1.1.0.0/24"}))
		g.Expect(cfg.Spec.BGP.Routers[0].Neighbors[0].ToAdvertise.Allowed.Prefixes).To(gomega.Equal([]string{"1.1.0.0/24"}))
		g.Expect(cfg.Spec.BGP.Routers[0].Imports).To(gomega.BeEmpty())
		g.Expect(cfg.Spec.Raw.Config).To(gomega.Equal(original.RawConfig))
	}
	ra, err = tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(ctx, "ra", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	accepted := meta.FindStatusCondition(ra.Status.Conditions, conditionTypeAccepted)
	g.Expect(accepted).NotTo(gomega.BeNil())
	g.Expect(accepted.Status).To(gomega.Equal(metav1.ConditionFalse))
	g.Expect(accepted.Reason).To(gomega.Equal("ConfigurationError"))
}

func TestNetworkWithdrawalPreservesSharedTargetAndEVPN(t *testing.T) {
	g := gomega.NewWithT(t)
	_ = newFRRReconcileTest(t, g, "eth0")
	makeNetwork := func(name, subnet string, ipVNI, macVNI int32) util.NetInfo {
		nad := (&testNAD{Name: name, Namespace: name, Network: util.GenerateCUDNNetworkName(name), Topology: types.Layer3Topology, Subnet: subnet,
			EVPNVTEPName: "vtep", EVPNIPVRFVNI: ipVNI, EVPNMACVRFVNI: macVNI}).NAD()
		network, err := util.ParseNADInfo(nad)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return network
	}
	red := makeNetwork("red", "10.1.0.0/16", 1000, 1001)
	blue := makeNetwork("blue", "10.2.0.0/16", 2000, 2001)
	redIP, err := newIPVRFConfig(red.GetNetworkName(), "red", 1000, "65000:1000", true, false)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	blueIP, err := newIPVRFConfig(blue.GetNetworkName(), "blue", 2000, "65000:2000", true, false)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	redMAC, err := newVRFConfig("MAC-VRF", red.GetNetworkName(), 1001, "65000:1001")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	blueMAC, err := newVRFConfig("MAC-VRF", blue.GetNetworkName(), 2001, "65000:2001")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	peer, err := newFRRNeighbor(frrapi.Neighbor{Address: "192.0.2.1", ASN: 64512})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	neighbors := map[string][]frrNeighbor{"": {peer}, "red": {peer}, "blue": {peer}}
	asns := map[string]uint32{"": 64512, "red": 64512, "blue": 64512}
	originalRaw := generateRawConfig(&selectedNetworks{ipVRFConfigs: []*ipVRFConfig{redIP, blueIP}, macVRFConfigs: []*vrfConfig{redMAC, blueMAC}}, neighbors, asns)
	spec := &frrapi.FRRConfigurationSpec{
		BGP: frrapi.BGPConfig{Routers: []frrapi.Router{
			{ASN: 64512, Prefixes: []string{"100.64.0.1/32"}},
			{ASN: 64512, VRF: "red", Prefixes: []string{"10.1.0.0/24"}},
			{ASN: 64512, VRF: "blue", Prefixes: []string{"10.2.0.0/24"}},
		}}, Raw: frrapi.RawConfig{Priority: 10, Config: originalRaw},
	}
	w := newNetworkWithdrawal(map[string]util.NetInfo{"red": red}, map[string]util.NetInfo{"blue": blue})
	w.prune(spec)
	g.Expect(spec.BGP.Routers).To(gomega.HaveLen(2))
	g.Expect(spec.BGP.Routers[0].Prefixes).To(gomega.Equal([]string{"100.64.0.1/32"}), "shared VTEP must survive")
	g.Expect(spec.Raw.Config).NotTo(gomega.ContainSubstring("vrf red"))
	g.Expect(spec.Raw.Config).NotTo(gomega.ContainSubstring("vni 1001"))
	g.Expect(spec.Raw.Config).To(gomega.ContainSubstring(genVRFVNISection(blueIP)))
	g.Expect(spec.Raw.Config).To(gomega.ContainSubstring(genNonDefaultVRFSection("blue", 64512, []frrNeighbor{peer}, blueIP)))
	g.Expect(spec.Raw.Config).To(gomega.ContainSubstring(genDefaultVRFSection(64512, []frrNeighbor{peer}, &selectedNetworks{ipVRFConfigs: []*ipVRFConfig{blueIP}, macVRFConfigs: []*vrfConfig{blueMAC}})))
	unchanged := spec.DeepCopy()
	w.prune(spec)
	g.Expect(spec).To(gomega.Equal(unchanged), "withdrawals must be idempotent")

	// A former tenant VRF may also be the explicit target for another network.
	// Keep its unicast peering and reverse imports, but remove its EVPN binding.
	shared := &frrapi.FRRConfigurationSpec{BGP: frrapi.BGPConfig{Routers: []frrapi.Router{
		{ASN: 64512, VRF: "red", Prefixes: []string{"10.1.0.0/24", "10.2.0.0/24"}, Imports: []frrapi.Import{{VRF: "blue"}}},
		{ASN: 64512, VRF: "blue", Imports: []frrapi.Import{{VRF: "red"}}},
	}}, Raw: frrapi.RawConfig{Config: originalRaw}}
	w.prune(shared)
	g.Expect(shared.BGP.Routers).To(gomega.HaveLen(2))
	g.Expect(shared.BGP.Routers[0].Prefixes).To(gomega.Equal([]string{"10.2.0.0/24"}))
	g.Expect(shared.BGP.Routers[1].Imports).To(gomega.Equal([]frrapi.Import{{VRF: "red"}}))
	g.Expect(shared.Raw.Config).To(gomega.ContainSubstring(genNonDefaultVRFSection("red", 64512, []frrNeighbor{peer}, nil)))
	g.Expect(shared.Raw.Config).NotTo(gomega.ContainSubstring(genVRFVNISection(redIP)))
	// Identical prefixes in isolated VRFs must not keep the removed VRF alive.
	overlapping := makeNetwork("overlap", "10.1.0.0/16", 0, 0)
	overlapWithdrawal := newNetworkWithdrawal(map[string]util.NetInfo{"red": red}, map[string]util.NetInfo{"overlap": overlapping})
	isolated := &frrapi.FRRConfigurationSpec{BGP: frrapi.BGPConfig{Routers: []frrapi.Router{
		{ASN: 64512, VRF: "red", Prefixes: []string{"10.1.0.0/24"}},
		{ASN: 64512, VRF: "overlap", Prefixes: []string{"10.1.0.0/24"}},
	}}}
	overlapWithdrawal.prune(isolated)
	g.Expect(isolated.BGP.Routers).To(gomega.Equal([]frrapi.Router{{ASN: 64512, VRF: "overlap", Prefixes: []string{"10.1.0.0/24"}}}))

	defaultWithdrawal := newNetworkWithdrawal(map[string]util.NetInfo{"default": &util.DefaultNetInfo{}}, map[string]util.NetInfo{"blue": blue})
	defaultTarget := &frrapi.FRRConfigurationSpec{BGP: frrapi.BGPConfig{Routers: []frrapi.Router{
		{ASN: 64512, Prefixes: []string{"1.1.0.0/24", "10.2.0.0/24"}, Imports: []frrapi.Import{{VRF: "blue"}}},
		{ASN: 64512, VRF: "blue", Imports: []frrapi.Import{{VRF: "default"}}},
	}}}
	defaultWithdrawal.prune(defaultTarget)
	g.Expect(defaultTarget.BGP.Routers[0].Prefixes).To(gomega.Equal([]string{"10.2.0.0/24"}))
	g.Expect(defaultTarget.BGP.Routers[1].Imports).To(gomega.Equal([]frrapi.Import{{VRF: "default"}}))

}

func TestController_withdrawDeselectedNetworkEgressIPs(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	old := generatedFRRConfig("ovnk-generated-node", "node", true)
	old.Routers[0].Prefixes = []string{"1.1.0.0/24", "1.3.0.0/24", "192.0.2.10/32", "192.0.2.20/32"}
	old.Routers[0].Neighbors[0].Advertise = old.Routers[0].Prefixes
	tt := newFRRReconcileTest(t, g, "eth0", old)
	nad := (&testNAD{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"), Topology: types.Layer3Topology, Subnet: "1.3.0.0/16", Annotations: map[string]string{types.OvnRouteAdvertisementsKey: `["ra"]`}}).NAD()
	_, err := tt.fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Create(ctx, nad, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Eventually(func() error {
		_, err := tt.c.nadLister.NetworkAttachmentDefinitions(nad.Namespace).Get(nad.Name)
		return err
	}).Should(gomega.Succeed())
	for name, address := range map[string]string{"retained": "192.0.2.20", "new": "192.0.2.30"} {
		_, err := tt.fakeClientset.EgressIPClient.K8sV1().EgressIPs().Create(ctx, (&testEIP{Name: name, EIPs: map[string]string{"node": address}}).EgressIP(), metav1.CreateOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Eventually(func() error { _, err := tt.c.eipLister.Get(name); return err }).Should(gomega.Succeed())
	}
	defaultNAD, err := tt.c.nadLister.NetworkAttachmentDefinitions(config.Default.ClusterDefaultNetworkNAD.Namespace).Get(config.Default.ClusterDefaultNetworkNAD.Name)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	ra := (&testRA{Name: "ra", SelectsDefault: true, AdvertisePods: true, AdvertiseEgressIPs: true}).RouteAdvertisements()
	_, err = tt.c.withdrawDeselectedNetworks(ctx, ra, []*nadtypes.NetworkAttachmentDefinition{defaultNAD})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	cfg, err := tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(old.Namespace).Get(ctx, old.Name, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(cfg.Spec.BGP.Routers[0].Prefixes).To(gomega.ConsistOf("1.1.0.0/24", "192.0.2.20/32"))
	g.Expect(cfg.Spec.BGP.Routers[0].Neighbors[0].ToAdvertise.Allowed.Prefixes).To(gomega.ConsistOf("1.1.0.0/24", "192.0.2.20/32"))
}

func TestController_withdrawalRetainsReferenceWhenNADChanges(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	tt := newFRRReconcileTest(t, g, "eth0")
	var selected []*nadtypes.NetworkAttachmentDefinition
	for _, name := range []string{"old", "new"} {
		nad := (&testNAD{Name: name, Namespace: name, Network: util.GenerateCUDNNetworkName("blue"), Topology: types.Layer3Topology, Subnet: "1.3.0.0/16"}).NAD()
		if name == "old" {
			nad.Annotations[types.OvnRouteAdvertisementsKey] = `["ra"]`
		} else {
			selected = append(selected, nad)
		}
		_, err := tt.fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(name).Create(ctx, nad, metav1.CreateOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Eventually(func() error { _, err := tt.c.nadLister.NetworkAttachmentDefinitions(name).Get(name); return err }).Should(gomega.Succeed())
	}
	defaultNAD, err := tt.c.nadLister.NetworkAttachmentDefinitions(config.Default.ClusterDefaultNetworkNAD.Namespace).Get(config.Default.ClusterDefaultNetworkNAD.Name)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	selected = append(selected, defaultNAD)
	ra := (&testRA{Name: "ra", AdvertisePods: true}).RouteAdvertisements()
	changed, err := tt.c.withdrawDeselectedNetworks(ctx, ra, selected)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(changed).To(gomega.BeFalse(), "no network was deselected")
	old, err := tt.fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("old").Get(ctx, "old", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(old.Annotations[types.OvnRouteAdvertisementsKey]).To(gomega.Equal(`["ra"]`))
	new, err := tt.fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("new").Get(ctx, "new", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(new.Annotations[types.OvnRouteAdvertisementsKey]).To(gomega.BeEmpty())
}
