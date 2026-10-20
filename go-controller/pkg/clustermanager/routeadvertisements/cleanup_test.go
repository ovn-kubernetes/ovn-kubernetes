// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeadvertisements

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"testing"

	nadtypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	frrapi "github.com/metallb/frr-k8s/api/v1beta1"
	frrfake "github.com/metallb/frr-k8s/pkg/client/clientset/versioned/fake"
	frrlisters "github.com/metallb/frr-k8s/pkg/client/listers/api/v1beta1"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	ctesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestController_rejectedUpdateCleansUpDeselectedNetwork(t *testing.T) {
	for _, tc := range []struct {
		name, peerInterface, targetVRF string
		dualStack                      bool
	}{
		{name: "unmatched router", peerInterface: "eth0"},
		{name: "unsafe template", peerInterface: unsafeInputValue},
		{name: "unsafe target VRF", peerInterface: "eth0", targetVRF: unsafeInputValue},
		{name: "dual-stack peer", peerInterface: "eth0", dualStack: true},
		{name: "peer without address or interface"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRejectedUpdateCleansUpDeselectedNetwork(t, tc.peerInterface, tc.targetVRF, tc.dualStack)
		})
	}
}

func testRejectedUpdateCleansUpDeselectedNetwork(t *testing.T, peerInterface, targetVRF string, dualStack bool) {
	t.Helper()
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
	tt := newFRRReconcileTest(t, g, peerInterface, old...)
	if dualStack {
		source, err := tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations("frrNamespace").Get(ctx, "frrConfig", metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		source.Spec.BGP.Routers[0].Neighbors[0].DualStackAddressFamily = true
		_, err = tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(source.Namespace).Update(ctx, source, metav1.UpdateOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Eventually(func() bool {
			cfg, _ := tt.c.frrLister.FRRConfigurations(source.Namespace).Get(source.Name)
			return cfg.Spec.BGP.Routers[0].Neighbors[0].DualStackAddressFamily
		}).Should(gomega.BeTrue())
	}
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
	ra.Spec = (&testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true, TargetVRF: targetVRF, NetworkSelector: map[string]string{"color": "green"}}).RouteAdvertisements().Spec
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
			return true, nil, errors.New("injected cleanup update failure")
		}
		return false, nil, nil
	})
	err = tt.c.reconcile(ctx, "ra")
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("injected cleanup update failure")))
	g.Expect(errors.Is(err, errConfig)).To(gomega.BeFalse(), "cleanup failures must be retried")
	g.Expect(references("blue", "blue")).To(gomega.ConsistOf("ra", "other"), "keep reference until all FRR updates succeed")
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

func TestNetworkCleanupPreservesSharedTargetAndEVPN(t *testing.T) {
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
	cleanup := newNetworkCleanup(map[string]util.NetInfo{"red": red}, map[string]util.NetInfo{"blue": blue})
	cleanup.prune(spec, nil)
	g.Expect(spec.BGP.Routers).To(gomega.HaveLen(2))
	g.Expect(spec.BGP.Routers[0].Prefixes).To(gomega.Equal([]string{"100.64.0.1/32"}), "shared VTEP must survive")
	g.Expect(spec.Raw.Config).NotTo(gomega.ContainSubstring("vrf red"))
	g.Expect(spec.Raw.Config).NotTo(gomega.ContainSubstring("vni 1001"))
	g.Expect(spec.Raw.Config).To(gomega.ContainSubstring(genVRFVNISection(blueIP)))
	g.Expect(spec.Raw.Config).To(gomega.ContainSubstring(genNonDefaultVRFSection("blue", 64512, []frrNeighbor{peer}, blueIP)))
	g.Expect(spec.Raw.Config).To(gomega.ContainSubstring(genDefaultVRFSection(64512, []frrNeighbor{peer}, &selectedNetworks{ipVRFConfigs: []*ipVRFConfig{blueIP}, macVRFConfigs: []*vrfConfig{blueMAC}})))
	unchanged := spec.DeepCopy()
	cleanup.prune(spec, nil)
	g.Expect(spec).To(gomega.Equal(unchanged), "cleanup must be idempotent")

	// A former tenant VRF may also be the explicit target for another network.
	// Keep its unicast peering and reverse imports, but remove its EVPN binding.
	shared := &frrapi.FRRConfigurationSpec{BGP: frrapi.BGPConfig{Routers: []frrapi.Router{
		{ASN: 64512, VRF: "red", Prefixes: []string{"10.1.0.0/24", "10.2.0.0/24"}, Imports: []frrapi.Import{{VRF: "blue"}}},
		{ASN: 64512, VRF: "blue", Imports: []frrapi.Import{{VRF: "red"}}},
	}}, Raw: frrapi.RawConfig{Config: originalRaw}}
	cleanup.prune(shared, nil)
	g.Expect(shared.BGP.Routers).To(gomega.HaveLen(2))
	g.Expect(shared.BGP.Routers[0].Prefixes).To(gomega.Equal([]string{"10.2.0.0/24"}))
	g.Expect(shared.BGP.Routers[1].Imports).To(gomega.Equal([]frrapi.Import{{VRF: "red"}}))
	g.Expect(shared.Raw.Config).To(gomega.ContainSubstring(genNonDefaultVRFSection("red", 64512, []frrNeighbor{peer}, nil)))
	g.Expect(shared.Raw.Config).NotTo(gomega.ContainSubstring(genVRFVNISection(redIP)))
	// Identical prefixes in isolated VRFs must not keep the removed VRF alive.
	overlapping := makeNetwork("overlap", "10.1.0.0/16", 0, 0)
	overlapCleanup := newNetworkCleanup(map[string]util.NetInfo{"red": red}, map[string]util.NetInfo{"overlap": overlapping})
	isolated := &frrapi.FRRConfigurationSpec{BGP: frrapi.BGPConfig{Routers: []frrapi.Router{
		{ASN: 64512, VRF: "red", Prefixes: []string{"10.1.0.0/24"}},
		{ASN: 64512, VRF: "overlap", Prefixes: []string{"10.1.0.0/24"}},
	}}}
	overlapCleanup.prune(isolated, nil)
	g.Expect(isolated.BGP.Routers).To(gomega.Equal([]frrapi.Router{{ASN: 64512, VRF: "overlap", Prefixes: []string{"10.1.0.0/24"}}}))

	defaultCleanup := newNetworkCleanup(map[string]util.NetInfo{"default": &util.DefaultNetInfo{}}, map[string]util.NetInfo{"blue": blue})
	defaultTarget := &frrapi.FRRConfigurationSpec{BGP: frrapi.BGPConfig{Routers: []frrapi.Router{
		{ASN: 64512, Prefixes: []string{"1.1.0.0/24", "10.2.0.0/24"}, Imports: []frrapi.Import{{VRF: "blue"}}},
		{ASN: 64512, VRF: "blue", Imports: []frrapi.Import{{VRF: "default"}}},
	}}}
	defaultCleanup.prune(defaultTarget, nil)
	g.Expect(defaultTarget.BGP.Routers[0].Prefixes).To(gomega.Equal([]string{"10.2.0.0/24"}))
	g.Expect(defaultTarget.BGP.Routers[1].Imports).To(gomega.Equal([]frrapi.Import{{VRF: "default"}}))

	// With blue as the target, the default router only imports blue. Once
	// the default network is deselected, remove that router and both imports.
	blueTarget := &frrapi.FRRConfigurationSpec{BGP: frrapi.BGPConfig{Routers: []frrapi.Router{
		{ASN: 64512, VRF: "blue", Prefixes: []string{"1.1.0.0/24", "10.2.0.0/24"}, Imports: []frrapi.Import{{VRF: "default"}}},
		{ASN: 64512, Imports: []frrapi.Import{{VRF: "blue"}}},
	}}}
	defaultCleanup.prune(blueTarget, nil)
	g.Expect(blueTarget.BGP.Routers).To(gomega.Equal([]frrapi.Router{
		{ASN: 64512, VRF: "blue", Prefixes: []string{"10.2.0.0/24"}, Imports: []frrapi.Import{}},
	}))
}

func TestNetworkCleanupScopesRetainedVTEPHosts(t *testing.T) {
	cleanup := newNetworkCleanup(nil, nil)
	cleanup.removedPrefixes = []netip.Prefix{netip.MustParsePrefix("100.64.0.0/16"), netip.MustParsePrefix("fd00:100::/64")}
	// Also exercise the pruning rule for a deselected default network.
	cleanup.removedVRFs.Insert(types.DefaultNetworkName)
	hosts := []string{"100.64.0.1/32", "100.64.0.2/32", "fd00:100::1/128", "fd00:100::2/128"}
	sharedReceive := []frrapi.PrefixSelector{
		{Prefix: "100.64.0.0/16", GE: 32, LE: 32},
		{Prefix: "100.64.0.0/16", GE: 32},
		{Prefix: "100.64.0.0/16", LE: 32},
		{Prefix: "fd00:100::/64", GE: 128, LE: 128},
	}
	// Reuse the cleanup across nodes: preservation must stay per-node.
	for _, retained := range [][]string{{hosts[0], hosts[2]}, {hosts[1], hosts[3]}} {
		t.Run(retained[0], func(t *testing.T) {
			g := gomega.NewWithT(t)
			receive := append([]frrapi.PrefixSelector{
				{Prefix: hosts[0]}, {Prefix: hosts[1]}, {Prefix: hosts[2]}, {Prefix: hosts[3]},
				{Prefix: "100.64.0.0/16"}, {Prefix: "100.64.0.0/16", LE: 24},
				{Prefix: "fd00:100::/64", LE: 96},
			}, sharedReceive...)
			router := frrapi.Router{ASN: 64512, Prefixes: hosts, Neighbors: []frrapi.Neighbor{{
				Interface: "eth0", ASN: 64512,
				ToAdvertise: frrapi.Advertise{Allowed: frrapi.AllowedOutPrefixes{Mode: frrapi.AllowRestricted, Prefixes: hosts}},
				ToReceive:   frrapi.Receive{Allowed: frrapi.AllowedInPrefixes{Mode: frrapi.AllowRestricted, Prefixes: receive}},
			}}}
			tenant := router.DeepCopy()
			tenant.VRF = "blue"
			spec := (&frrapi.FRRConfigurationSpec{BGP: frrapi.BGPConfig{Routers: []frrapi.Router{router, *tenant}}}).DeepCopy()
			cleanup.prune(spec, sets.New(retained...))
			g.Expect(spec.BGP.Routers).To(gomega.HaveLen(2))
			g.Expect(spec.BGP.Routers[0].Prefixes).To(gomega.ConsistOf(retained))
			g.Expect(spec.BGP.Routers[0].Neighbors[0].ToAdvertise.Allowed.Prefixes).To(gomega.ConsistOf(retained))
			expectedReceive := append([]frrapi.PrefixSelector{{Prefix: retained[0]}, {Prefix: retained[1]}}, sharedReceive...)
			g.Expect(spec.BGP.Routers[0].Neighbors[0].ToReceive.Allowed.Prefixes).To(gomega.ConsistOf(expectedReceive))
			g.Expect(spec.BGP.Routers[1].Prefixes).To(gomega.BeEmpty(), "VTEP preservation must not protect tenant routes")
			g.Expect(spec.BGP.Routers[1].Neighbors[0].ToAdvertise.Allowed.Prefixes).To(gomega.BeEmpty())
			g.Expect(spec.BGP.Routers[1].Neighbors[0].ToReceive.Allowed.Prefixes).To(gomega.BeEmpty())
			unchanged := spec.DeepCopy()
			cleanup.prune(spec, sets.New(retained...))
			g.Expect(spec).To(gomega.Equal(unchanged))
		})
	}
}

func TestController_cleanupDeselectedNetworkEgressIPs(t *testing.T) {
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
	_, err = tt.c.cleanupDeselectedNetworks(ctx, ra, []*nadtypes.NetworkAttachmentDefinition{defaultNAD})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	cfg, err := tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(old.Namespace).Get(ctx, old.Name, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(cfg.Spec.BGP.Routers[0].Prefixes).To(gomega.ConsistOf("1.1.0.0/24", "192.0.2.20/32"))
	g.Expect(cfg.Spec.BGP.Routers[0].Neighbors[0].ToAdvertise.Allowed.Prefixes).To(gomega.ConsistOf("1.1.0.0/24", "192.0.2.20/32"))
}

func TestController_rejectedEgressIPUpdatePreservesRetainedVTEPs(t *testing.T) {
	testRejectedEgressIPUpdatePreservesRetainedVTEPs(t, true, false)
}

func TestController_rejectedUpdateRemovesEgressIPAdvertisement(t *testing.T) {
	t.Run("deselected network", func(t *testing.T) {
		testRejectedEgressIPUpdatePreservesRetainedVTEPs(t, false, false)
	})
	t.Run("unchanged network selection", func(t *testing.T) {
		testRejectedEgressIPUpdatePreservesRetainedVTEPs(t, false, true)
	})
}

func testRejectedEgressIPUpdatePreservesRetainedVTEPs(t *testing.T, advertiseEgressIPs, selectsDefault bool) {
	t.Helper()
	g := gomega.NewWithT(t)
	ctx := context.Background()
	vtepPrefixes := []string{"100.64.0.1/32", "fd00:100::1/128"}
	old := generatedFRRConfig("ovnk-generated-node", "node", true)
	old.Routers[0].Prefixes = append([]string{"1.1.0.0/24", "100.64.0.10/32", "fd00:100::10/128"}, vtepPrefixes...)
	old.Routers[0].Neighbors[0].Advertise = old.Routers[0].Prefixes
	old.Routers = append(old.Routers, &testRouter{ASN: 1, VRF: "blue",
		Prefixes:  []string{"1.3.0.0/24", "fd03::/64", "100.64.0.10/32", "fd00:100::10/128", "100.64.0.20/32", "fd00:100::20/128"},
		Neighbors: []*testNeighbor{{ASN: 1, Interface: "eth0"}},
	})
	// These same addresses in the tenant VRF are stale EgressIPs, not VTEPs.
	old.Routers[1].Prefixes = append(old.Routers[1].Prefixes, vtepPrefixes...)
	old.Routers[1].Neighbors[0].Advertise = old.Routers[1].Prefixes
	tt := newUnsafeInputTest(t, g, old)
	previousIPv6 := config.IPv6Mode
	t.Cleanup(func() { config.IPv6Mode = previousIPv6 })
	config.IPv6Mode = true
	nad := (&testNAD{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"), Topology: types.Layer3Topology,
		Subnet: "1.3.0.0/16,fd03::/48", EVPNVTEPName: "vtep", EVPNIPVRFVNI: 2000,
		Labels: map[string]string{"selected": "true"}, Annotations: map[string]string{types.OvnRouteAdvertisementsKey: `["ra"]`}}).NAD()
	network, err := util.ParseNADInfo(nad)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	networks := &networkmanager.FakeNetworkManager{PrimaryNetworks: map[string]util.NetInfo{"blue": network}}
	networks.SetNodeActive(network.GetNetworkName(), "node", true)
	tt.c.nm = networks
	_, err = tt.fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Create(ctx, nad, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Eventually(func() error {
		_, err := tt.c.nadLister.NetworkAttachmentDefinitions(nad.Namespace).Get(nad.Name)
		return err
	}).Should(gomega.Succeed())
	_, err = tt.fakeClientset.KubeClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "blue"}}, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Eventually(func() error { _, err := tt.c.namespaceLister.Get("blue"); return err }).Should(gomega.Succeed())
	vtep := &vtepv1.VTEP{ObjectMeta: metav1.ObjectMeta{Name: "vtep"}, Spec: vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16", "fd00:100::/64"}}}
	_, err = tt.fakeClientset.VTEPClient.K8sV1().VTEPs().Create(ctx, vtep, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Eventually(func() error { _, err := tt.c.vtepLister.Get(vtep.Name); return err }).Should(gomega.Succeed())
	node, err := tt.fakeClientset.KubeClient.CoreV1().Nodes().Get(ctx, "node", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	node.Annotations[util.OVNNodeVTEPs] = `{"vtep":{"ips":["100.64.0.1","fd00:100::1"]},"unrelated":{"ips":["100.64.0.10","fd00:100::10"]}}`
	_, err = tt.fakeClientset.KubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Eventually(func() string { n, _ := tt.c.nodeLister.Get("node"); return n.Annotations[util.OVNNodeVTEPs] }).Should(gomega.Equal(node.Annotations[util.OVNNodeVTEPs]))
	for name, address := range map[string]string{"retained-v4": "100.64.0.20", "retained-v6": "fd00:100::20", "new-v4": "100.64.0.30", "new-v6": "fd00:100::30"} {
		_, err = tt.fakeClientset.EgressIPClient.K8sV1().EgressIPs().Create(ctx, (&testEIP{Name: name, EIPs: map[string]string{"node": address}}).EgressIP(), metav1.CreateOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Eventually(func() error { _, err := tt.c.eipLister.Get(name); return err }).Should(gomega.Succeed())
	}
	ra, err := tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(ctx, "ra", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	targetVRF := "blue"
	if !advertiseEgressIPs {
		// Reject before generation can replace the previously validated
		// PodNetwork+EgressIP configuration with the new pod-only selection.
		targetVRF = unsafeInputValue
	}
	ra.Spec = (&testRA{Name: "ra", AdvertisePods: true, AdvertiseEgressIPs: advertiseEgressIPs, SelectsDefault: selectsDefault,
		TargetVRF: targetVRF, NetworkSelector: map[string]string{"selected": "true"}}).RouteAdvertisements().Spec
	_, err = tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Update(ctx, ra, metav1.UpdateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Eventually(func() string { r, _ := tt.c.raLister.Get("ra"); return r.Spec.TargetVRF }).Should(gomega.Equal(targetVRF))

	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
	if advertiseEgressIPs {
		expectRANotAccepted(g, tt)
	} else {
		ra, err = tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(ctx, "ra", metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		accepted := meta.FindStatusCondition(ra.Status.Conditions, conditionTypeAccepted)
		g.Expect(accepted).NotTo(gomega.BeNil())
		g.Expect(accepted.Status).To(gomega.Equal(metav1.ConditionFalse))
		g.Expect(accepted.Message).To(gomega.ContainSubstring("invalid TargetVRF"))
	}
	cfg, err := tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(old.Namespace).Get(ctx, old.Name, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(cfg.Spec.BGP.Routers).To(gomega.HaveLen(2))
	defaultPrefixes := vtepPrefixes
	defaultReferences := `["other"]`
	if selectsDefault {
		defaultPrefixes = append([]string{"1.1.0.0/24"}, vtepPrefixes...)
		defaultReferences = `["ra","other"]`
	}
	g.Expect(cfg.Spec.BGP.Routers[0].Prefixes).To(gomega.ConsistOf(defaultPrefixes))
	g.Expect(cfg.Spec.BGP.Routers[0].Neighbors[0].ToAdvertise.Allowed.Prefixes).To(gomega.ConsistOf(defaultPrefixes))
	retainedPrefixes := []string{"1.3.0.0/24", "fd03::/64"}
	if advertiseEgressIPs {
		retainedPrefixes = append(retainedPrefixes, "100.64.0.20/32", "fd00:100::20/128")
	}
	g.Expect(cfg.Spec.BGP.Routers[1].Prefixes).To(gomega.ConsistOf(retainedPrefixes))
	g.Expect(cfg.Spec.BGP.Routers[1].Neighbors[0].ToAdvertise.Allowed.Prefixes).To(gomega.ConsistOf(retainedPrefixes))
	g.Expect(cfg.Spec.Raw).To(gomega.Equal(old.FRRConfiguration().Spec.Raw))
	g.Expect(tt.defaultNADAnnotation()).To(gomega.Equal(defaultReferences))
	retainedNAD, err := tt.fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Get(ctx, nad.Name, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(retainedNAD.Annotations[types.OvnRouteAdvertisementsKey]).To(gomega.Equal(`["ra"]`))
	// Let informer caches observe the cleanup before another rejected
	// reconcile; removed NAD references must not strand or restore old routes.
	g.Eventually(func() frrapi.FRRConfigurationSpec {
		cached, _ := tt.c.frrLister.FRRConfigurations(cfg.Namespace).Get(cfg.Name)
		return cached.Spec
	}).Should(gomega.Equal(cfg.Spec))
	g.Eventually(func() string {
		nad, _ := tt.c.nadLister.NetworkAttachmentDefinitions(config.Default.ClusterDefaultNetworkNAD.Namespace).Get(config.Default.ClusterDefaultNetworkNAD.Name)
		return nad.Annotations[types.OvnRouteAdvertisementsKey]
	}).Should(gomega.Equal(defaultReferences))
	g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
	afterRetry, err := tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(cfg.Namespace).Get(ctx, cfg.Name, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(afterRetry.Spec).To(gomega.Equal(cfg.Spec))
}

func TestController_rejectedUpdateCleansUpChangedVTEP(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		egressIP              bool
		keepDeletedAnnotation bool
		lastVTEP              bool
		overlappingCIDRs      bool
	}{
		{name: "node still references deleted VTEP", keepDeletedAnnotation: true},
		{name: "node no longer references deleted VTEP"},
		{name: "preserves retained EgressIPs", egressIP: true},
		{name: "last EVPN network deselected", lastVTEP: true},
		{name: "removed VTEP overlaps retained VTEP", overlappingCIDRs: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			ctx := context.Background()
			retained := []string{"1.1.0.0/24"}
			if !tc.lastVTEP {
				retained = append(retained, "100.65.0.1/32", "fd00:101::1/128")
			}
			old := generatedFRRConfig("ovnk-generated-node", "node", true)
			old.Routers[0].Prefixes = append([]string{"100.64.0.1/32", "fd00:100::1/128"}, retained...)
			if tc.egressIP {
				retained = append(retained, "192.0.2.20/32", "2001:db8::20/128")
				old.Routers[0].Prefixes = append(old.Routers[0].Prefixes, "192.0.2.20/32", "2001:db8::20/128", "192.0.2.10/32")
			}
			old.Routers[0].Neighbors[0].Advertise = old.Routers[0].Prefixes
			if tc.overlappingCIDRs {
				old.Routers[0].Neighbors[0].Receive = []testPrefixSelector{
					{Prefix: "100.64.0.0/16", GE: 32, LE: 32}, {Prefix: "fd00:100::/64", GE: 128, LE: 128},
					{Prefix: "100.65.0.0/16", GE: 32, LE: 32}, {Prefix: "fd00:101::/64", GE: 128, LE: 128},
				}
			}
			old.Routers = append(old.Routers,
				&testRouter{ASN: 1, VRF: "blue", Prefixes: []string{"1.3.0.0/24", "fd03::/64"}},
				&testRouter{ASN: 1, VRF: "green", Prefixes: []string{"1.4.0.0/24", "fd04::/64"}},
			)
			old.RawConfig += "vrf blue\n vni 1000\nexit-vrf\n!\n"
			if !tc.lastVTEP {
				old.RawConfig += "vrf green\n vni 2000\nexit-vrf\n!\n"
			}
			tt := newUnsafeInputTest(t, g, old)
			previousIPv6 := config.IPv6Mode
			t.Cleanup(func() { config.IPv6Mode = previousIPv6 })
			config.IPv6Mode = true
			networks := &networkmanager.FakeNetworkManager{PrimaryNetworks: map[string]util.NetInfo{}}
			tt.c.nm = networks
			for _, nadSpec := range []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"), Topology: types.Layer3Topology,
					Subnet: "1.3.0.0/16,fd03::/48", EVPNVTEPName: "deleted", EVPNIPVRFVNI: 1000},
				{Name: "green", Namespace: "green", Network: util.GenerateCUDNNetworkName("green"), Topology: types.Layer3Topology,
					Subnet: "1.4.0.0/16,fd04::/48", EVPNVTEPName: "retained", EVPNIPVRFVNI: 2000, Labels: map[string]string{"selected": "true"}},
			} {
				if tc.lastVTEP && nadSpec.Name == "green" {
					nadSpec.EVPNVTEPName, nadSpec.EVPNIPVRFVNI = "", 0
				}
				nadSpec.Annotations = map[string]string{types.OvnRouteAdvertisementsKey: `["ra","other"]`}
				nad := nadSpec.NAD()
				network, err := util.ParseNADInfo(nad)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				networks.PrimaryNetworks[nad.Namespace] = network
				networks.SetNodeActive(network.GetNetworkName(), "node", true)
				_, err = tt.fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Create(ctx, nad, metav1.CreateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Eventually(func() error {
					_, err := tt.c.nadLister.NetworkAttachmentDefinitions(nad.Namespace).Get(nad.Name)
					return err
				}).Should(gomega.Succeed())
			}
			for name, cidrs := range map[string][]vtepv1.CIDR{
				"deleted": {"100.64.0.0/16", "fd00:100::/64"}, "retained": {"100.65.0.0/16", "fd00:101::/64"},
			} {
				vtep := &vtepv1.VTEP{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: vtepv1.VTEPSpec{Mode: vtepv1.VTEPModeUnmanaged, CIDRs: cidrs}}
				_, err := tt.fakeClientset.VTEPClient.K8sV1().VTEPs().Create(ctx, vtep, metav1.CreateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Eventually(func() error { _, err := tt.c.vtepLister.Get(name); return err }).Should(gomega.Succeed())
			}
			if tc.overlappingCIDRs {
				// An unmanaged VTEP can expand into another VTEP's CIDRs.
				// Overlap rejection freezes existing NADs and configurations.
				vtep, err := tt.fakeClientset.VTEPClient.K8sV1().VTEPs().Get(ctx, "deleted", metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				vtep.Spec.Mode = vtepv1.VTEPModeUnmanaged
				vtep.Spec.CIDRs = []vtepv1.CIDR{"100.64.0.0/15", "fd00:100::/31"}
				_, err = tt.fakeClientset.VTEPClient.K8sV1().VTEPs().Update(ctx, vtep, metav1.UpdateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Eventually(func() []vtepv1.CIDR { v, _ := tt.c.vtepLister.Get("deleted"); return v.Spec.CIDRs }).Should(gomega.Equal(vtep.Spec.CIDRs))
			} else {
				// The CUDN's new VTEP failed validation, leaving this old NAD in
				// place after the former VTEP lost its CUDN reference and was deleted.
				g.Expect(tt.fakeClientset.VTEPClient.K8sV1().VTEPs().Delete(ctx, "deleted", metav1.DeleteOptions{})).To(gomega.Succeed())
				g.Eventually(func() bool { _, err := tt.c.vtepLister.Get("deleted"); return apierrors.IsNotFound(err) }).Should(gomega.BeTrue())
			}
			node, err := tt.fakeClientset.KubeClient.CoreV1().Nodes().Get(ctx, "node", metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			vteps := map[string]util.VTEPNodeAnnotation{"retained": {IPs: []string{"100.65.0.1", "fd00:101::1"}}}
			if tc.keepDeletedAnnotation {
				vteps["deleted"] = util.VTEPNodeAnnotation{IPs: []string{"100.64.0.1", "fd00:100::1"}}
			}
			annotation, err := json.Marshal(vteps)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			node.Annotations[util.OVNNodeVTEPs] = string(annotation)
			_, err = tt.fakeClientset.KubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Eventually(func() string { n, _ := tt.c.nodeLister.Get("node"); return n.Annotations[util.OVNNodeVTEPs] }).Should(gomega.Equal(string(annotation)))
			if tc.egressIP {
				for name, address := range map[string]string{"retained-v4": "192.0.2.20", "retained-v6": "2001:db8::20", "new": "192.0.2.30"} {
					_, err = tt.fakeClientset.EgressIPClient.K8sV1().EgressIPs().Create(ctx, (&testEIP{Name: name, EIPs: map[string]string{"node": address}}).EgressIP(), metav1.CreateOptions{})
					g.Expect(err).NotTo(gomega.HaveOccurred())
					g.Eventually(func() error { _, err := tt.c.eipLister.Get(name); return err }).Should(gomega.Succeed())
				}
			}
			ra, err := tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(ctx, "ra", metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			ra.Spec = (&testRA{Name: "ra", AdvertisePods: true, AdvertiseEgressIPs: tc.egressIP, SelectsDefault: true,
				TargetVRF: "green", NetworkSelector: map[string]string{"selected": "true"}}).RouteAdvertisements().Spec
			if tc.lastVTEP {
				ra.Spec.TargetVRF = ""
			}
			_, err = tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Update(ctx, ra, metav1.UpdateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Eventually(func() int { r, _ := tt.c.raLister.Get("ra"); return len(r.Spec.NetworkSelectors) }).Should(gomega.Equal(2))

			g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
			expectRANotAccepted(g, tt)
			cfg, err := tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(old.Namespace).Get(ctx, old.Name, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(cfg.Spec.BGP.Routers).To(gomega.HaveLen(2))
			g.Expect(cfg.Spec.BGP.Routers[0].Prefixes).To(gomega.ConsistOf(retained))
			g.Expect(cfg.Spec.BGP.Routers[0].Neighbors[0].ToAdvertise.Allowed.Prefixes).To(gomega.ConsistOf(retained))
			if tc.overlappingCIDRs {
				g.Expect(cfg.Spec.BGP.Routers[0].Neighbors[0].ToReceive.Allowed.Prefixes).To(gomega.ConsistOf(
					frrapi.PrefixSelector{Prefix: "100.65.0.0/16", GE: 32, LE: 32},
					frrapi.PrefixSelector{Prefix: "fd00:101::/64", GE: 128, LE: 128},
				))
			}
			g.Expect(cfg.Spec.BGP.Routers[1]).To(gomega.Equal(old.FRRConfiguration().Spec.BGP.Routers[2]))
			g.Expect(cfg.Spec.Raw.Config).NotTo(gomega.ContainSubstring("vrf blue"))
			if !tc.lastVTEP {
				g.Expect(cfg.Spec.Raw.Config).To(gomega.ContainSubstring("vrf green\n vni 2000\nexit-vrf"))
			}
			for _, name := range []string{"blue", "green"} {
				nad, err := tt.fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(name).Get(ctx, name, metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				expected := `["other"]`
				if name == "green" {
					expected = `["ra","other"]`
				}
				g.Expect(nad.Annotations[types.OvnRouteAdvertisementsKey]).To(gomega.Equal(expected))
			}
		})
	}
}

func TestController_rejectedUpdateCleansUpUnavailableNodeVTEPs(t *testing.T) {
	for _, tc := range []struct {
		name, annotation string
		deletedNode      bool
		keepHealthyNode  bool
		egressIP         bool
		retainedVTEPs    []string
	}{
		{name: "deleted node with EgressIP", deletedNode: true, egressIP: true},
		{name: "deleted node with missing VTEP", deletedNode: true},
		{name: "deleted node alongside healthy node", deletedNode: true, keepHealthyNode: true, egressIP: true},
		{name: "malformed entry before valid entry", egressIP: true,
			annotation: `{"green":"invalid","yellow":{"ips":["100.66.0.1"]}}`, retainedVTEPs: []string{"100.66.0.1/32"}},
		{name: "invalid IP after valid entry",
			annotation: `{"green":{"ips":["100.65.0.1"]},"yellow":{"ips":["not-an-ip"]}}`, retainedVTEPs: []string{"100.65.0.1/32"}},
		{name: "out of range IP after valid entry", egressIP: true,
			annotation: `{"green":{"ips":["100.65.0.1"]},"yellow":{"ips":["192.0.2.1"]}}`, retainedVTEPs: []string{"100.65.0.1/32"}},
		{name: "malformed annotation", annotation: "not-json"},
		{name: "missing annotation", egressIP: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			ctx := context.Background()
			old := generatedFRRConfig("ovnk-generated-node", "node", true)
			old.Routers[0].Prefixes = []string{"1.1.0.0/24", "1.3.0.0/24", "100.65.0.1/32", "100.66.0.1/32"}
			old.Routers[0].Neighbors[0].Advertise = old.Routers[0].Prefixes
			generated := []*testFRRConfig{old}
			if tc.keepHealthyNode {
				healthy := generatedFRRConfig("ovnk-generated-healthy", "healthy", true)
				healthy.Routers[0].Prefixes = []string{"1.1.0.0/24", "1.3.0.0/24", "100.65.0.2/32", "100.66.0.2/32"}
				healthy.Routers[0].Neighbors[0].Advertise = healthy.Routers[0].Prefixes
				generated = append(generated, healthy)
			}
			tt := newFRRReconcileTest(t, g, "eth0", generated...)
			if tc.keepHealthyNode {
				node := (&testNode{Name: "healthy", SubnetsAnnotation: `{"default":"1.1.0.0/24"}`,
					VTEPIPs: map[string][]string{"green": {"100.65.0.2"}, "yellow": {"100.66.0.2"}}}).Node()
				_, err := tt.fakeClientset.KubeClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Eventually(func() error { _, err := tt.c.nodeLister.Get(node.Name); return err }).Should(gomega.Succeed())
			}
			for i, name := range []string{"blue", "green", "yellow"} {
				nadSpec := &testNAD{Name: name, Namespace: name, Network: util.GenerateCUDNNetworkName(name),
					Topology: types.Layer3Topology, Subnet: "1.3.0.0/16",
					Annotations: map[string]string{types.OvnRouteAdvertisementsKey: `["ra","other"]`}}
				if name != "blue" {
					nadSpec.Subnet = "1.4.0.0/16"
					nadSpec.Labels = map[string]string{"selected": "true"}
				}
				if name != "blue" || !tc.egressIP {
					nadSpec.EVPNVTEPName, nadSpec.EVPNIPVRFVNI = name, int32(1000+i)
				}
				nad := nadSpec.NAD()
				_, err := tt.fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(name).Create(ctx, nad, metav1.CreateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Eventually(func() error { _, err := tt.c.nadLister.NetworkAttachmentDefinitions(name).Get(name); return err }).Should(gomega.Succeed())
			}
			for name, cidr := range map[string]vtepv1.CIDR{"green": "100.65.0.0/16", "yellow": "100.66.0.0/16"} {
				vtep := &vtepv1.VTEP{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{cidr}}}
				_, err := tt.fakeClientset.VTEPClient.K8sV1().VTEPs().Create(ctx, vtep, metav1.CreateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Eventually(func() error { _, err := tt.c.vtepLister.Get(name); return err }).Should(gomega.Succeed())
			}
			if tc.deletedNode {
				g.Expect(tt.fakeClientset.KubeClient.CoreV1().Nodes().Delete(ctx, "node", metav1.DeleteOptions{})).To(gomega.Succeed())
				g.Eventually(func() bool { _, err := tt.c.nodeLister.Get("node"); return apierrors.IsNotFound(err) }).Should(gomega.BeTrue())
			} else if tc.annotation != "" {
				node, err := tt.fakeClientset.KubeClient.CoreV1().Nodes().Get(ctx, "node", metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				node.Annotations[util.OVNNodeVTEPs] = tc.annotation
				_, err = tt.fakeClientset.KubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Eventually(func() string { n, _ := tt.c.nodeLister.Get("node"); return n.Annotations[util.OVNNodeVTEPs] }).Should(gomega.Equal(tc.annotation))
			}
			ra, err := tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(ctx, "ra", metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			// Reject before node validation so validated configurations reach
			// cleanup even when their node has disappeared or changed.
			ra.Spec = (&testRA{Name: "ra", AdvertisePods: true, AdvertiseEgressIPs: tc.egressIP, SelectsDefault: true,
				TargetVRF: unsafeInputValue, NetworkSelector: map[string]string{"selected": "true"}}).RouteAdvertisements().Spec
			_, err = tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Update(ctx, ra, metav1.UpdateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Eventually(func() string { r, _ := tt.c.raLister.Get("ra"); return r.Spec.TargetVRF }).Should(gomega.Equal(unsafeInputValue))
			if tc.deletedNode {
				failDelete := true
				tt.fakeClientset.FRRClient.(Fake).PrependReactor("delete", "frrconfigurations", func(action ctesting.Action) (bool, runtime.Object, error) {
					if failDelete && action.(ctesting.DeleteAction).GetName() == old.Name {
						return true, nil, errors.New("injected orphan deletion failure")
					}
					return false, nil, nil
				})
				g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.MatchError(gomega.ContainSubstring("injected orphan deletion failure")))
				nad, getErr := tt.fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("blue").Get(ctx, "blue", metav1.GetOptions{})
				g.Expect(getErr).NotTo(gomega.HaveOccurred())
				g.Expect(nad.Annotations[types.OvnRouteAdvertisementsKey]).To(gomega.Equal(`["ra","other"]`))
				failDelete = false
			}
			g.Expect(tt.c.reconcile(ctx, "ra")).To(gomega.Succeed())
			cfg, err := tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(old.Namespace).Get(ctx, old.Name, metav1.GetOptions{})
			if tc.deletedNode {
				g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
				if tc.keepHealthyNode {
					cfg, err = tt.fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(old.Namespace).Get(ctx, "ovnk-generated-healthy", metav1.GetOptions{})
					g.Expect(err).NotTo(gomega.HaveOccurred())
					g.Expect(cfg.Spec.BGP.Routers[0].Prefixes).To(gomega.ConsistOf("1.1.0.0/24", "100.65.0.2/32", "100.66.0.2/32"))
					g.Expect(cfg.Spec.BGP.Routers[0].Neighbors[0].ToAdvertise.Allowed.Prefixes).To(gomega.ConsistOf("1.1.0.0/24", "100.65.0.2/32", "100.66.0.2/32"))
					g.Expect(tt.defaultNADAnnotation()).To(gomega.Equal(`["ra","other"]`))
				} else {
					g.Expect(tt.defaultNADAnnotation()).To(gomega.Equal(`["other"]`))
				}
			} else {
				g.Expect(err).NotTo(gomega.HaveOccurred())
				prefixes := append([]string{"1.1.0.0/24"}, tc.retainedVTEPs...)
				g.Expect(cfg.Spec.BGP.Routers[0].Prefixes).To(gomega.ConsistOf(prefixes))
				g.Expect(cfg.Spec.BGP.Routers[0].Neighbors[0].ToAdvertise.Allowed.Prefixes).To(gomega.ConsistOf(prefixes))
				g.Expect(cfg.Spec.Raw.Config).To(gomega.Equal(old.RawConfig))
			}
			for _, name := range []string{"blue", "green", "yellow"} {
				nad, getErr := tt.fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(name).Get(ctx, name, metav1.GetOptions{})
				g.Expect(getErr).NotTo(gomega.HaveOccurred())
				expected := `["other"]`
				if name != "blue" && (!tc.deletedNode || tc.keepHealthyNode) {
					expected = `["ra","other"]`
				}
				g.Expect(nad.Annotations[types.OvnRouteAdvertisementsKey]).To(gomega.Equal(expected))
			}
			ra, err = tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(ctx, "ra", metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			accepted := meta.FindStatusCondition(ra.Status.Conditions, conditionTypeAccepted)
			g.Expect(accepted).NotTo(gomega.BeNil())
			g.Expect(accepted.Status).To(gomega.Equal(metav1.ConditionFalse))
			g.Expect(accepted.Message).To(gomega.ContainSubstring("invalid TargetVRF"))
		})
	}
}

func TestController_cleanupRetainsReferenceWhenNADChanges(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	generated := generatedFRRConfig("ovnk-generated-old", "node", true)
	generated.Routers[0].Prefixes = []string{"1.1.0.0/24", "1.3.0.0/24"}
	tt := newFRRReconcileTest(t, g, "eth0", generated)
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
	changed, err := tt.c.cleanupDeselectedNetworks(ctx, ra, selected)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(changed).To(gomega.BeFalse(), "no network was deselected")
	old, err := tt.fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("old").Get(ctx, "old", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(old.Annotations[types.OvnRouteAdvertisementsKey]).To(gomega.Equal(`["ra"]`))
	new, err := tt.fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("new").Get(ctx, "new", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(new.Annotations[types.OvnRouteAdvertisementsKey]).To(gomega.BeEmpty())
}

func TestController_cleanupClearsReferencesWithoutConfigurations(t *testing.T) {
	for _, scenario := range []string{"empty list", "earlier deletion with stale cache", "update NotFound", "update failure", "list failure"} {
		t.Run(scenario, func(t *testing.T) {
			g := gomega.NewWithT(t)
			ctx := context.Background()
			var generated []*testFRRConfig
			if scenario != "empty list" && scenario != "list failure" {
				cfg := generatedFRRConfig("ovnk-generated-old", "node", true)
				cfg.Routers[0].Prefixes = []string{"1.1.0.0/24"}
				if scenario != "earlier deletion with stale cache" {
					cfg.Routers[0].Prefixes = append(cfg.Routers[0].Prefixes, "192.0.2.1/32")
				}
				generated = append(generated, cfg)
			}
			tt := newFRRReconcileTest(t, g, "eth0", generated...)
			nad, err := tt.c.nadLister.NetworkAttachmentDefinitions(config.Default.ClusterDefaultNetworkNAD.Namespace).Get(config.Default.ClusterDefaultNetworkNAD.Name)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			selected := []*nadtypes.NetworkAttachmentDefinition{nad}
			ra, err := tt.c.raLister.Get("ra")
			g.Expect(err).NotTo(gomega.HaveOccurred())
			client := tt.fakeClientset.FRRClient.(*frrfake.Clientset)
			if scenario == "list failure" {
				client.PrependReactor("list", "frrconfigurations", func(ctesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("injected list failure")
				})
			}
			if scenario == "update NotFound" || scenario == "update failure" {
				client.PrependReactor("update", "frrconfigurations", func(action ctesting.Action) (bool, runtime.Object, error) {
					if scenario == "update failure" {
						return true, nil, errors.New("injected update failure")
					}
					cfg := action.(ctesting.UpdateAction).GetObject().(*frrapi.FRRConfiguration)
					resource := schema.GroupVersionResource{Group: "frrk8s.metallb.io", Version: "v1beta1", Resource: "frrconfigurations"}
					g.Expect(client.Tracker().Delete(resource, cfg.Namespace, cfg.Name)).To(gomega.Succeed())
					return true, nil, apierrors.NewNotFound(resource.GroupResource(), cfg.Name)
				})
			}
			var changed bool
			if scenario == "earlier deletion with stale cache" {
				// Freeze the lister before reconciliation deletes the last object.
				// Its retained pod routes would otherwise require no pruning update.
				indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
				g.Expect(indexer.Add(generated[0].FRRConfiguration())).To(gomega.Succeed())
				tt.c.frrLister = frrlisters.NewFRRConfigurationLister(indexer)
				changed, err = tt.c.reconcileRouteAdvertisements(ctx, ra.Name, ra, frrGenerationResult{nads: selected, err: errInvalidVTEP})
			} else {
				changed, err = tt.c.cleanupDeselectedNetworks(ctx, ra, selected)
			}
			var references []string
			g.Expect(json.Unmarshal([]byte(tt.defaultNADAnnotation()), &references)).To(gomega.Succeed())
			if scenario == "update failure" || scenario == "list failure" {
				g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("injected")))
				g.Expect(references).To(gomega.ConsistOf("ra", "other"), "retain references until cleanup succeeds")
				return
			}
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(changed).To(gomega.BeTrue())
			g.Expect(references).To(gomega.ConsistOf("other"), "remove only this RA's reference")
		})
	}
}
