// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/external_ids_syncer/logical_router_policy"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestSelectiveEgressIPSelectionChanged(t *testing.T) {
	catchAll := &eipv1.EgressIP{}
	selective := catchAll.DeepCopy()
	selective.Spec.TrafficSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"traffic": "one"}}
	statusUpdate := selective.DeepCopy()
	statusUpdate.Status.Items = []eipv1.EgressIPStatusItem{{Node: "node", EgressIP: "192.0.2.10"}}
	labelUpdate := selective.DeepCopy()
	labelUpdate.Labels = map[string]string{"label": "new"}
	podSelectorUpdate := selective.DeepCopy()
	podSelectorUpdate.Spec.PodSelector.MatchLabels = map[string]string{"app": "new"}
	trafficSelectorUpdate := selective.DeepCopy()
	trafficSelectorUpdate.Spec.TrafficSelector.MatchLabels["traffic"] = "two"
	namespaceSelectorUpdate := selective.DeepCopy()
	namespaceSelectorUpdate.Spec.NamespaceSelector.MatchLabels = map[string]string{"team": "new"}
	addressUpdate := selective.DeepCopy()
	addressUpdate.Spec.EgressIPs = []string{"192.0.2.10"}
	nodeSelectorUpdate := selective.DeepCopy()
	nodeSelectorUpdate.Spec.EgressNodeSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"node": "new"}}
	addressAndPodSelectorUpdate := podSelectorUpdate.DeepCopy()
	addressAndPodSelectorUpdate.Spec.EgressIPs = addressUpdate.Spec.EgressIPs
	emptyCollections := selective.DeepCopy()
	emptyCollections.Spec.TrafficSelector.MatchExpressions = []metav1.LabelSelectorRequirement{}
	emptyCollections.Spec.PodSelector.MatchLabels = map[string]string{}
	emptyCollections.Spec.NamespaceSelector.MatchExpressions = []metav1.LabelSelectorRequirement{}
	emptySelector := catchAll.DeepCopy()
	emptySelector.Spec.TrafficSelector = &metav1.LabelSelector{}
	existsSelector := selective.DeepCopy()
	existsSelector.Spec.TrafficSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "traffic", Operator: metav1.LabelSelectorOpExists}}}
	emptyValues := existsSelector.DeepCopy()
	emptyValues.Spec.TrafficSelector.MatchExpressions[0].Values = []string{}

	for _, tc := range []struct {
		name     string
		old, new *eipv1.EgressIP
		changed  bool
	}{
		{"catch-all added", nil, catchAll, false},
		{"catch-all deleted", catchAll, nil, false},
		{"selective added", nil, selective, true},
		{"selective deleted", selective, nil, true},
		{"unchanged", selective, selective.DeepCopy(), false},
		{"nil and empty collections", selective, emptyCollections, false},
		{"nil and empty requirement values", existsSelector, emptyValues, false},
		{"absent to empty selector", catchAll, emptySelector, true},
		{"empty to absent selector", emptySelector, catchAll, true},
		{"status update", selective, statusUpdate, false},
		{"label update", selective, labelUpdate, false},
		{"selector added", catchAll, selective, true},
		{"selector removed", selective, catchAll, true},
		{"traffic selector changed", selective, trafficSelectorUpdate, true},
		{"pod selector changed", selective, podSelectorUpdate, true},
		{"namespace selector changed", selective, namespaceSelectorUpdate, true},
		{"addresses changed", selective, addressUpdate, false},
		{"node selector changed", selective, nodeSelectorUpdate, false},
		{"addresses and pod selector changed", selective, addressAndPodSelectorUpdate, true},
		{"catch-all spec changed", catchAll, &eipv1.EgressIP{Spec: eipv1.EgressIPSpec{PodSelector: podSelectorUpdate.Spec.PodSelector}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(selectiveEgressIPSelectionChanged(tc.old, tc.new)).To(gomega.Equal(tc.changed))
		})
	}
}

func TestTrafficClassRetryRetainsOldLabels(t *testing.T) {
	f := newSelectiveEgressFixture(t, "default", "IPv4")
	f.g.Expect(f.controller.reconcileEgressIP(nil, f.one)).To(gomega.Succeed())
	stop := make(chan struct{})
	var workers sync.WaitGroup
	oc := &DefaultNetworkController{
		BaseNetworkController: BaseNetworkController{
			CommonNetworkControllerInfo: CommonNetworkControllerInfo{watchFactory: f.watchFactory},
			ReconcilableNetInfo:         &util.DefaultNetInfo{}, stopChan: stop, wg: &workers,
		},
		eIPC: f.controller,
	}
	r := oc.newRetryFramework(factory.TrafficClassType)
	handler, err := r.WatchResource()
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(func() {
		close(stop)
		stopped := make(chan struct{})
		go func() { workers.Wait(); close(stopped) }()
		f.g.Eventually(stopped, 5*time.Second).Should(gomega.BeClosed(), "TrafficClass retry workers did not stop")
	})
	failures := &failingTrafficAddressSets{AddressSetFactory: f.addressSets, failNew: true}
	f.controller.addressSetFactory = failures
	changed := f.class.DeepCopy()
	changed.Labels["traffic"] = "unmatched"
	f.g.Expect(f.watchFactory.TrafficClassInformer().Informer().GetStore().Update(changed)).To(gomega.Succeed())
	handler.OnUpdate(f.class, changed)
	f.g.Expect(retry.CheckRetryObj(f.class.Name, r)).To(gomega.BeTrue())
	// Another label update must not replace the still-pending original labels.
	later := changed.DeepCopy()
	later.Labels["unrelated"] = "new"
	f.g.Expect(f.watchFactory.TrafficClassInformer().Informer().GetStore().Update(later)).To(gomega.Succeed())
	r.DoWithLock(f.class.Name, func(string) { failures.failNew = true })
	handler.OnUpdate(changed, later)
	f.g.Expect(retry.GetOldObjFromRetryObj(f.class.Name, r)).To(gomega.Equal(f.class))
	retry.SetRetryObjWithNoBackoff(f.class.Name, r)
	r.RequestRetryObjs()
	f.g.Eventually(func() bool { return retry.CheckRetryObj(f.class.Name, r) }, 5*time.Second).Should(gomega.BeFalse())
	state, _ := f.controller.podAssignment.Load(getPodKey(f.pod))
	f.g.Expect(state.activeEgressIPs).NotTo(gomega.HaveKey(f.one.Name))
	policies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(f.nb, func(policy *nbdb.LogicalRouterPolicy) bool {
		name, _ := getEIPLRPObjK8MetaData(policy.ExternalIDs)
		return name == f.one.Name
	})
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(policies).To(gomega.BeEmpty())
}

func TestTrafficClassUnchangedAssignments(t *testing.T) {
	f := newSelectiveEgressFixture(t, "default", "IPv4")
	f.g.Expect(f.controller.reconcileEgressIPPod(nil, f.pod)).To(gomega.Succeed())
	f.assertOwners(3)
	f.restartNetwork()
	policies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(f.nb, func(*nbdb.LogicalRouterPolicy) bool { return true })
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	nats, err := libovsdbops.FindNATsWithPredicate(f.nb, func(*nbdb.NAT) bool { return true })
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	// Initial class notifications and repeated events must preserve applied rows, including UUIDs.
	for _, class := range []*trafficv1.TrafficClass{f.class, f.peerClass, f.class} {
		f.g.Expect(f.controller.reconcileTrafficClass(class)).To(gomega.Succeed())
	}
	afterPolicies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(f.nb, func(*nbdb.LogicalRouterPolicy) bool { return true })
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(afterPolicies).To(gomega.ConsistOf(policies))
	afterNATs, err := libovsdbops.FindNATsWithPredicate(f.nb, func(*nbdb.NAT) bool { return true })
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(afterNATs).To(gomega.ConsistOf(nats))
}

func TestTrafficClassRelevantUpdates(t *testing.T) {
	old := &trafficv1.TrafficClass{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"traffic": "one"}},
		Spec:       trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: "192.0.2.0/24"}}},
	}
	for _, tc := range []struct {
		name   string
		update func(*trafficv1.TrafficClass)
		equal  bool
	}{
		{"unchanged", func(*trafficv1.TrafficClass) {}, true},
		{"annotations", func(c *trafficv1.TrafficClass) { c.Annotations = map[string]string{"note": "new"} }, true},
		{"labels", func(c *trafficv1.TrafficClass) { c.Labels["traffic"] = "two" }, false},
		{"matches", func(c *trafficv1.TrafficClass) { c.Spec.Matches[0].DestinationCIDR = "203.0.113.0/24" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updated := old.DeepCopy()
			tc.update(updated)
			handler := &baseNetworkControllerEventHandler{}
			equal, err := handler.areResourcesEqual(factory.TrafficClassType, old, updated)
			if err != nil {
				t.Fatalf("compare TrafficClass update: %v", err)
			}
			if equal != tc.equal {
				t.Fatalf("TrafficClass update equality = %v, want %v", equal, tc.equal)
			}
		})
	}
}

func TestRestartNATOwnerBoundary(t *testing.T) {
	f := newSelectiveEgressFixture(t, "default", "IPv4")
	standby := f.fallback.DeepCopy()
	standby.Name = "egressip"
	standby.Spec.EgressIPs = []string{"198.51.100.20"}
	standby.Status.Items[0].EgressIP = standby.Spec.EgressIPs[0]
	active := standby.DeepCopy()
	active.Name = "egressip-2"
	active.Spec.EgressIPs = []string{"198.51.100.21"}
	active.Status.Items[0].EgressIP = active.Spec.EgressIPs[0]
	for _, owner := range []*eipv1.EgressIP{standby, active} {
		f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Add(owner)).To(gomega.Succeed())
	}
	f.g.Expect(f.controller.reconcileEgressIP(nil, active)).To(gomega.Succeed())
	// In the egress zone, NAT alone can identify the active owner after restart.
	f.g.Expect(libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool {
		name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
		return name == active.Name
	})).To(gomega.Succeed())
	f.controller.podAssignment = syncmap.NewSyncMap[*podAssignmentState]()
	cache, err := f.controller.generateCacheForEgressIP(egressIPAllNetworks)
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(f.controller.syncPodAssignmentCache(cache, egressIPAllNetworks)).To(gomega.Succeed())
	state, ok := f.controller.podAssignment.Load(getPodKey(f.pod))
	f.g.Expect(ok).To(gomega.BeTrue())
	f.g.Expect(state.activeEgressIPs).To(gomega.HaveKey(active.Name))
	f.g.Expect(state.activeEgressIPs).NotTo(gomega.HaveKey(standby.Name))
	f.g.Expect(state.standbyEgressIPNames.Has(standby.Name)).To(gomega.BeTrue())
}

func TestInvalidWorkloadSelectorWithdrawsSelectiveOwner(t *testing.T) {
	for _, field := range []string{"namespace", "pod"} {
		t.Run(field, func(t *testing.T) {
			f := newSelectiveEgressFixture(t, "default", "IPv4")
			for _, owner := range []*eipv1.EgressIP{f.one, f.two, f.fallback} {
				f.g.Expect(f.controller.reconcileEgressIP(nil, owner)).To(gomega.Succeed())
			}
			invalid := f.one.DeepCopy()
			selector := metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: "Invalid"}}}
			if field == "namespace" {
				invalid.Spec.NamespaceSelector = selector
			} else {
				invalid.Spec.PodSelector = selector
			}
			f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(invalid)).To(gomega.Succeed())
			// A healthy peer's reconciliation must withdraw the invalid applied owner.
			f.g.Expect(f.controller.addPodEgressIPAssignmentsWithLock(f.network, f.two.Name, f.two.Status.Items,
				getEgressIPPktMark(f.two.Name, f.two.Annotations), f.pod)).To(gomega.Succeed())
			f.assertOwners(2)
			f.restartNetwork()
			f.g.Expect(f.controller.addPodEgressIPAssignmentsWithLock(f.network, f.two.Name, f.two.Status.Items,
				getEgressIPPktMark(f.two.Name, f.two.Annotations), f.pod)).To(gomega.Succeed())
			f.assertOwners(2)
		})
	}
}

func TestLegacyOwnerlessRerouteCleanup(t *testing.T) {
	f := newSelectiveEgressFixture(t, "default", "IPv4")
	_, subnet, err := net.ParseCIDR("10.128.0.0/14")
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: subnet}}
	stale := &nbdb.LogicalRouterPolicy{
		Priority: types.EgressIPLegacyReroutePriority, Action: nbdb.LogicalRouterPolicyActionReroute,
		Match: "ip4.src == 10.128.0.99", Nexthops: []string{"100.64.0.2"}, ExternalIDs: map[string]string{"name": "deleted-owner"},
	}
	ops, err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(f.nb, nil, f.router, stale, func(*nbdb.LogicalRouterPolicy) bool { return false })
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = libovsdbops.TransactAndCheck(f.nb, ops)
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(logical_router_policy.NewLRPSyncer(f.nb, f.controller.controllerName).Sync()).To(gomega.Succeed())
	policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(policy *nbdb.LogicalRouterPolicy) bool { return policy.Match == stale.Match })
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(policies).To(gomega.HaveLen(1))
	f.g.Expect(policies[0].ExternalIDs[libovsdbops.OwnerControllerKey.String()]).To(gomega.Equal(f.controller.controllerName))
	f.g.Expect(f.controller.migrateEgressIPReroutePriorities(egressIPAllNetworks)).To(gomega.Succeed())
	policies, err = libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(policy *nbdb.LogicalRouterPolicy) bool { return policy.Match == stale.Match })
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(policies).To(gomega.BeEmpty())
}

func TestSelectiveEgressReconciliation(t *testing.T) {
	for _, family := range []string{"IPv4", "IPv6", "dual-stack"} {
		for _, topology := range []string{"default", types.Layer2Topology, "layer2-transit", types.Layer3Topology} {
			t.Run(fmt.Sprintf("%s/%s", topology, family), func(t *testing.T) {
				f := newSelectiveEgressFixture(t, topology, family)
				f.testSelectionAndResolutionRetry()
				f.testProgramming()
				f.testSelectorRetry()
				f.testInvalidSelectors()
				if f.dualStack {
					f.testFamilyChange()
					return
				}
				if f.network.IsDefault() {
					f.testRemotePod()
				}
				f.testConflictRecovery()
				f.testClassUpdates()
				f.testPodIPReplacement()
				f.testStartupRepair()
				f.testGateDisableAndNetworkCleanup()
			})
		}
	}
}

// The phases share one fixture to exercise the lifecycle in the same order as the controller events.
type selectiveEgressFixture struct {
	t *testing.T
	g *gomega.WithT

	ipv6, dualStack bool
	topology        string
	family          string
	podPrefix       string
	podMask         string
	joinIP          string
	cidr            string
	l4CIDR          string
	nadKey          string

	network            util.NetInfo
	pod                *corev1.Pod
	node               *corev1.Node
	podIPs             []*net.IPNet
	mac                net.HardwareAddr
	one, two, fallback *eipv1.EgressIP
	class, peerClass   *trafficv1.TrafficClass

	router, gateway string

	nb             libovsdbclient.Client
	watchFactory   *factory.WatchFactory
	addressSets    addressset.AddressSetFactory
	portCache      *PortCache
	networkManager *networkmanager.FakeNetworkManager
	controller     *EgressIPController
}

func newSelectiveEgressFixture(t *testing.T, topologyCase, familyMode string) *selectiveEgressFixture {
	t.Helper()
	ipv6, dualStack := familyMode == "IPv6", familyMode == "dual-stack"
	topology := topologyCase
	if topology == "layer2-transit" {
		topology = types.Layer2Topology
	}
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.IPv4Mode, config.IPv6Mode = !ipv6, ipv6 || dualStack
	config.OVNKubernetesFeature.EnableEgressIP = true
	config.OVNKubernetesFeature.EnableTrafficClass = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.Layer2UsesTransitRouter = topologyCase == "layer2-transit"
	podPrefix, podMask, primarySubnet := "10.128.0.", "/24", "20.128.0.0/16"
	externalPrefix, externalMask, joinIP, gatewayIP := "198.51.100.", "/24", "100.64.0.2/16", "198.51.100.254"
	cidr, l4CIDR, family := "203.0.113.0/24", "192.0.2.0/24", "ip4"
	if ipv6 {
		podPrefix, podMask, primarySubnet = "fd10::", "/64", "fd20::/48"
		externalPrefix, externalMask, joinIP, gatewayIP = "2001:db8:1::", "/64", "fd98::2/64", "2001:db8:1::fe"
		cidr, l4CIDR, family = "2001:db8:100::/64", "2001:db8:101::/64", "ip6"
	}
	if dualStack {
		primarySubnet += ",fd20::/48"
	}
	var ni util.NetInfo = &util.DefaultNetInfo{}
	nadKey := types.DefaultNetworkName
	if topology != "default" {
		nad := &nadapi.NetworkAttachmentDefinition{ObjectMeta: metav1.ObjectMeta{Name: "blue", Namespace: "work"}, Spec: nadapi.NetworkAttachmentDefinitionSpec{Config: fmt.Sprintf(`{"cniVersion":"0.4.0","type":"ovn-k8s-cni-overlay","name":"blue","netAttachDefName":"work/blue","topology":%q,"role":"primary","subnets":%q}`, topology, primarySubnet)}}
		var err error
		ni, err = util.ParseNADInfo(nad)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		nadKey = "work/blue"
	}
	if ni.IsUserDefinedNetwork() {
		podPrefix = "20.128.0."
		if ipv6 {
			podPrefix = "fd20::"
		}
	}
	pod := ovntest.NewPodWithLabels("work", "pod", "node", podPrefix+"10", map[string]string{"app": "test"})
	ips := ovntest.MustParseIPNets(podPrefix + "10" + podMask)
	if dualStack {
		v6Pod := "fd10::10/64"
		if ni.IsUserDefinedNetwork() {
			v6Pod = "fd20::10/64"
		}
		ips = append(ips, ovntest.MustParseIPNets(v6Pod)...)
	}

	mac, _ := net.ParseMAC("0a:58:0a:80:00:0a")
	var err error
	pod.Annotations, err = util.MarshalPodAnnotation(nil, &util.PodAnnotation{IPs: ips, MAC: mac, Role: types.NetworkRolePrimary}, nadKey)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Annotations: map[string]string{
		"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf(`{%q:%q}`, map[bool]string{false: "ipv4", true: "ipv6"}[ipv6], externalPrefix+"2"+externalMask),
		util.OVNNodeHostCIDRs:             fmt.Sprintf(`[%q]`, externalPrefix+"2"+externalMask),
		"k8s.ovn.org/node-chassis-id":     "chassis",
		util.OvnNodeID:                    fmt.Sprint(types.MinNodeID),
		util.OvnNodeL3GatewayConfig:       fmt.Sprintf(`{"default":{"mode":"shared","interface-id":"node","mac-address":"0a:58:c6:33:64:02","ip-addresses":[%q],"next-hops":[%q],"node-port-enable":"true"}}`, externalPrefix+"2"+externalMask, gatewayIP),
	}}}
	if dualStack {
		node.Annotations["k8s.ovn.org/node-primary-ifaddr"] = `{"ipv4":"198.51.100.2/24","ipv6":"2001:db8:1::2/64"}`
		node.Annotations[util.OVNNodeHostCIDRs] = `["198.51.100.2/24","2001:db8:1::2/64"]`
		node.Annotations[util.OvnNodeL3GatewayConfig] = `{"default":{"mode":"shared","interface-id":"node","mac-address":"0a:58:c6:33:64:02","ip-addresses":["198.51.100.2/24","2001:db8:1::2/64"],"next-hops":["198.51.100.254","2001:db8:1::fe"],"node-port-enable":"true"}}`
	}
	one := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "one", Annotations: map[string]string{util.EgressIPMarkAnnotation: "50001"}}, Spec: eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"traffic": "one"}}}, Status: eipv1.EgressIPStatus{Items: []eipv1.EgressIPStatusItem{{Node: "node", EgressIP: externalPrefix + "10"}}}}
	two := one.DeepCopy()
	two.Name = "two"
	two.Annotations[util.EgressIPMarkAnnotation] = "50002"
	two.Spec.TrafficSelector.MatchLabels["traffic"] = "two"
	two.Status.Items[0].EgressIP = externalPrefix + "11"
	fallback := one.DeepCopy()
	fallback.Name = "fallback"
	fallback.Annotations[util.EgressIPMarkAnnotation] = "50003"
	fallback.Spec.TrafficSelector = nil
	fallback.Status.Items[0].EgressIP = externalPrefix + "12"
	class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "one", Labels: map[string]string{"traffic": "one"}}, Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{
		{DestinationCIDR: cidr},
		{DestinationCIDR: l4CIDR, Protocols: []trafficv1.ProtocolMatch{{UDP: &trafficv1.PortProtocol{SourcePort: &trafficv1.Port{Range: &trafficv1.PortRange{Start: 16384, End: 32767}}}}}},
	}}}
	peerClass := class.DeepCopy()
	peerClass.Name = "two"
	peerClass.Labels["traffic"] = "two"
	peerClass.Spec.Matches = []trafficv1.TrafficMatch{{DestinationCIDR: l4CIDR, Protocols: []trafficv1.ProtocolMatch{{TCP: &trafficv1.PortProtocol{DestinationPort: &trafficv1.Port{Number: ptr.To[int32](22)}}}}}}
	if dualStack {
		two.Status.Items[0].EgressIP = "2001:db8:1::11"
		peerClass.Spec.Matches[0].DestinationCIDR = "2001:db8:101::/64"
		class.Spec.Matches = append(class.Spec.Matches, trafficv1.TrafficMatch{DestinationCIDR: "2001:db8:100::/64"})
	}
	clients := util.GetOVNClientset().GetOVNKubeControllerClientset()
	wf, err := factory.NewOVNKubeControllerWatchFactory(clients, "node")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(wf.Stop)
	for _, eip := range []*eipv1.EgressIP{one, two, fallback} {
		g.Expect(wf.EgressIPInformer().Informer().GetStore().Add(eip)).To(gomega.Succeed())
	}
	g.Expect(wf.TrafficClassInformer().Informer().GetStore().Add(class)).To(gomega.Succeed())
	g.Expect(wf.TrafficClassInformer().Informer().GetStore().Add(peerClass)).To(gomega.Succeed())
	g.Expect(wf.PodCoreInformer().Informer().GetStore().Add(pod)).To(gomega.Succeed())
	g.Expect(wf.NamespaceCoreInformer().Informer().GetStore().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "work", Labels: map[string]string{corev1.LabelMetadataName: "work"}}})).To(gomega.Succeed())
	g.Expect(wf.NodeCoreInformer().Informer().GetStore().Add(node)).To(gomega.Succeed())
	router, _ := getTopologyScopedRouterName(ni, "node")
	gateway := ni.GetNetworkScopedGWRouterName("node")
	joinNetworks := []string{joinIP}
	if dualStack {
		joinNetworks = append(joinNetworks, "fd98::2/64")
	}
	nbData := []libovsdbtest.TestData{&nbdb.LogicalRouter{Name: gateway, UUID: "gw", Ports: []string{"rtoj"}}, &nbdb.LogicalRouterPort{UUID: "rtoj", Name: types.GWRouterToJoinSwitchPrefix + gateway, Networks: joinNetworks}}
	if router != gateway {
		nbData = append(nbData, &nbdb.LogicalRouter{Name: router, UUID: "cr"})
	}
	nb, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: nbData}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(cleanup.Cleanup)
	asf := addressset.NewOvnAddressSetFactory(nb, !ipv6, ipv6 || dualStack)
	_, err = asf.EnsureAddressSet(getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, ni.GetNetworkName(), types.DefaultNetworkControllerName))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	pc := NewPortCache(make(chan struct{}))
	pc.add(pod, "", nadKey, "", mac, ips)
	nm := &networkmanager.FakeNetworkManager{PrimaryNetworks: map[string]util.NetInfo{"work": ni}, NADNetworks: map[string]util.NetInfo{nadKey: ni}}
	e := NewEIPController(nb, &kube.KubeOVN{EIPClient: clients.EgressIPClient}, wf, record.NewFakeRecorder(10), pc, nm, asf, nil, !ipv6, ipv6 || dualStack, "node", types.DefaultNetworkControllerName)
	g.Expect(e.SyncNodeCache()).To(gomega.Succeed())
	f := &selectiveEgressFixture{
		t: t, g: g, ipv6: ipv6, dualStack: dualStack, topology: topology,
		podPrefix: podPrefix, podMask: podMask, joinIP: joinIP, cidr: cidr, l4CIDR: l4CIDR, family: family, nadKey: nadKey,
		network: ni, pod: pod, node: node, podIPs: ips, mac: mac, router: router, gateway: gateway,
		one: one, two: two, fallback: fallback, class: class, peerClass: peerClass,
		nb: nb, watchFactory: wf, addressSets: asf, portCache: pc, networkManager: nm, controller: e,
	}
	f.restartNetwork()
	return f
}

func (f *selectiveEgressFixture) restartNetwork() {
	f.t.Helper()
	f.controller.UnregisterNetwork(f.network.GetNetworkName())
	f.controller.ensureNetworkPending(f.network.GetNetworkName())
	f.g.Expect(f.controller.SyncNetworkReady(f.network.GetNetworkName())).To(gomega.Succeed())
}

func (f *selectiveEgressFixture) assertGatewayMarks(expected int) {
	f.t.Helper()
	marks, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.gateway, func(p *nbdb.LogicalRouterPolicy) bool { return isEgressIPMarkPriority(p.Priority) })
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	if f.topology == "default" || f.topology == types.Layer2Topology && !config.Layer2UsesTransitRouter {
		f.g.Expect(marks).To(gomega.BeEmpty())
		return
	}
	f.g.Expect(marks).To(gomega.HaveLen(expected))
	for _, mark := range marks {
		f.g.Expect(mark.Match).To(gomega.ContainSubstring("pkt.mark == 0"))
		name, _ := getEIPLRPObjK8MetaData(mark.ExternalIDs)
		traffic, err := f.controller.egressTrafficMatchesFor(name)
		match := traffic.match(strings.HasPrefix(mark.Match, "ip6"))
		f.g.Expect(err).NotTo(gomega.HaveOccurred())
		f.g.Expect(mark.Priority).To(gomega.Equal(egressIPMarkPriority(match)))
		f.g.Expect(mark.Match).To(gomega.HaveSuffix(match))
	}
}

func (f *selectiveEgressFixture) assertServedPodIPs(expected []*net.IPNet) {
	f.t.Helper()
	served, err := f.addressSets.GetAddressSet(getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, f.network.GetNetworkName(), f.controller.controllerName))
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	v4, v6 := served.GetAddresses()
	var want []string
	for _, ip := range expected {
		want = append(want, ip.IP.String())
	}
	f.g.Expect(append(v4, v6...)).To(gomega.ConsistOf(want), "served pod addresses must outlive individual owners")
}

func (f *selectiveEgressFixture) assertOwners(expected int) {
	f.t.Helper()
	policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool { return isEgressIPReroutePriority(p.Priority) })
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(policies).To(gomega.HaveLen(expected))
	for _, policy := range policies {
		name, _ := getEIPLRPObjK8MetaData(policy.ExternalIDs)
		if name == "fallback" {
			f.g.Expect(policy.Priority).To(gomega.Equal(99))
			f.g.Expect(policy.Match).To(gomega.Equal(f.family + ".src == " + f.podIPs[0].IP.String()))
		} else {
			f.g.Expect(policy.Priority).To(gomega.Equal(100))
			f.g.Expect(policy.Match).To(gomega.ContainSubstring(" && ("))
		}
		if f.network.IsUserDefinedNetwork() {
			f.g.Expect(policy.Options["pkt_mark"]).NotTo(gomega.BeEmpty())
		}
	}
	state, _ := f.controller.podAssignment.Load(getPodKey(f.pod))
	f.g.Expect(state.activeEgressIPs).To(gomega.HaveLen(expected))
	f.assertGatewayMarks(expected)
	f.assertServedPodIPs(f.podIPs)
}

func (f *selectiveEgressFixture) testSelectionAndResolutionRetry() {
	// A new pod has no applied state; rejecting an owner must not create any.
	f.controller.podAssignment.Delete(getPodKey(f.pod))
	unselected := f.one.DeepCopy()
	unselected.Spec.PodSelector.MatchLabels = map[string]string{"unmatched": "pod"}
	f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(unselected)).To(gomega.Succeed())
	f.g.Expect(f.controller.addPodEgressIPAssignmentsWithLock(f.network, f.one.Name, f.one.Status.Items, getEgressIPPktMark(f.one.Name, f.one.Annotations), f.pod)).To(gomega.Succeed())
	_, hasPodState := f.controller.podAssignment.Load(getPodKey(f.pod))
	f.g.Expect(hasPodState).To(gomega.BeFalse())
	f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(f.one)).To(gomega.Succeed())
	// A status update must recover if traffic resolution failed on the add event.
	unassigned := f.one.DeepCopy()
	unassigned.Status.Items = nil
	f.controller.trafficMatches.Delete(f.one.Name)
	f.controller.addressSetFactory = &failingTrafficAddressSets{AddressSetFactory: f.addressSets, failNew: true}
	f.g.Expect(f.controller.reconcileEgressIP(nil, unassigned)).To(gomega.MatchError(gomega.ContainSubstring("injected traffic address-set failure")))
	f.g.Expect(f.controller.reconcileEgressIP(unassigned, f.one)).To(gomega.Succeed())
	f.controller.addressSetFactory = f.addressSets
	for _, owner := range []*eipv1.EgressIP{f.fallback, f.one, f.two} {
		f.g.Expect(f.controller.reconcileEgressIP(nil, owner)).To(gomega.Succeed())
	}
	f.assertOwners(3)
}

func (f *selectiveEgressFixture) testProgramming() {
	// Empty namespace selectors also match namespaces with only the API-server name label.
	f.g.Expect(f.controller.deletePodEgressIPAssignmentsUsingCache(f.one.Name, f.pod, false)).To(gomega.Succeed())
	f.assertOwners(2)
	f.g.Expect(f.controller.reconcileEgressIPPod(nil, f.pod)).To(gomega.Succeed())
	f.assertOwners(3)
	for _, owner := range []*eipv1.EgressIP{f.one, f.two} {
		as, err := f.addressSets.GetAddressSet(getEgressIPDestNetworksAddrSetDbIDs(owner.Name, f.controller.controllerName))
		f.g.Expect(err).NotTo(gomega.HaveOccurred())
		v4Name, v6Name := as.GetASHashNames()
		ownerFamily, ownerPodIP, ownerL4CIDR := f.family, f.podIPs[0].IP.String(), f.l4CIDR
		ownerIPv6 := f.ipv6 || f.dualStack && owner.Name == f.two.Name
		if f.dualStack && owner.Name == f.two.Name {
			ownerFamily, ownerPodIP, ownerL4CIDR = "ip6", f.podIPs[1].IP.String(), "2001:db8:101::/64"
		}
		asName := v4Name
		if ownerIPv6 {
			asName = v6Name
		}
		l4 := "udp && (udp.src >= 16384 && udp.src <= 32767)"
		if owner.Name == f.two.Name {
			l4 = "tcp && tcp.dst == 22"
		}
		want := fmt.Sprintf("(%s.dst == $%s || (%s.dst == %s && %s))", ownerFamily, asName, ownerFamily, ownerL4CIDR, l4)
		policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool {
			name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
			return name == owner.Name && isEgressIPReroutePriority(p.Priority)
		})
		f.g.Expect(err).NotTo(gomega.HaveOccurred())
		f.g.Expect(policies).To(gomega.HaveLen(1))
		f.g.Expect(policies[0].Match).To(gomega.Equal(ownerFamily + ".src == " + ownerPodIP + " && " + want))
		if f.network.IsUserDefinedNetwork() && (f.topology == types.Layer3Topology || config.Layer2UsesTransitRouter) {
			marks, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.gateway, func(p *nbdb.LogicalRouterPolicy) bool {
				name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
				return name == owner.Name && isEgressIPMarkPriority(p.Priority)
			})
			f.g.Expect(err).NotTo(gomega.HaveOccurred())
			f.g.Expect(marks).To(gomega.HaveLen(1))
			f.g.Expect(marks[0].Match).To(gomega.Equal(ownerFamily + ".src == " + ownerPodIP + " && pkt.mark == 0 && " + want))
			f.g.Expect(marks[0].Priority).To(gomega.Equal(96))
			f.g.Expect(marks[0].Options["pkt_mark"]).To(gomega.Equal(owner.Annotations[util.EgressIPMarkAnnotation]))
		}
		if f.network.IsDefault() {
			nats, err := libovsdbops.FindNATsWithPredicate(f.nb, func(n *nbdb.NAT) bool {
				name, _ := getEIPLRPObjK8MetaData(n.ExternalIDs)
				return name == owner.Name
			})
			f.g.Expect(err).NotTo(gomega.HaveOccurred())
			f.g.Expect(nats).To(gomega.HaveLen(1))
			f.g.Expect(nats[0].Match).To(gomega.Equal(want))
			f.g.Expect(nats[0].Priority).To(gomega.Equal(100))
		}
	}
	if f.network.IsDefault() {
		nats, err := libovsdbops.FindNATsWithPredicate(f.nb, func(n *nbdb.NAT) bool { return n.Type == nbdb.NATTypeSNAT })
		f.g.Expect(err).NotTo(gomega.HaveOccurred())
		f.g.Expect(nats).To(gomega.HaveLen(3))
		for _, nat := range nats {
			name, _ := getEIPLRPObjK8MetaData(nat.ExternalIDs)
			if name != "fallback" {
				f.g.Expect(nat.Match).NotTo(gomega.BeEmpty())
				f.g.Expect(nat.Priority).To(gomega.BeNumerically(">", 0))
			}
		}
	}
}

func (f *selectiveEgressFixture) testSelectorRetry() {
	// Pod retries can observe a selector edit before its EgressIP handler runs.
	pendingCatchAll := f.one.DeepCopy()
	pendingCatchAll.Spec.TrafficSelector = nil
	f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(pendingCatchAll)).To(gomega.Succeed())
	applied, _ := f.controller.podAssignment.Load(getPodKey(f.pod))
	f.g.Expect(applied.activeEgressIPs[f.one.Name].traffic.selective()).To(gomega.BeTrue())
	// Retire stale applied traffic even while the pod's port cache entry is unavailable.
	f.portCache.remove(f.pod, f.nadKey)
	f.g.Expect(f.controller.addPodEgressIPAssignmentsWithLock(f.network, f.one.Name, f.one.Status.Items, getEgressIPPktMark(f.one.Name, f.one.Annotations), f.pod)).To(gomega.Succeed())
	f.assertOwners(2)
	f.portCache.add(f.pod, "", f.nadKey, "", f.mac, f.podIPs)
	f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(f.one)).To(gomega.Succeed())
	f.g.Expect(f.controller.addPodEgressIPAssignmentsWithLock(f.network, f.one.Name, f.one.Status.Items, getEgressIPPktMark(f.one.Name, f.one.Annotations), f.pod)).To(gomega.Succeed())
	f.assertOwners(3)
}

func (f *selectiveEgressFixture) testInvalidSelectors() {
	// Invalid legacy selectors withdraw the owner, including at startup.
	for _, selector := range []*metav1.LabelSelector{
		{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "class", Operator: "Invalid"}}}, {},
	} {
		invalid := f.one.DeepCopy()
		invalid.Spec.TrafficSelector = selector
		f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(invalid)).To(gomega.Succeed())
		f.g.Expect(f.controller.reconcileEgressIP(f.one, invalid)).To(gomega.Succeed())
		f.assertOwners(2)
		f.restartNetwork()
		f.g.Expect(f.controller.reconcileTrafficClass(f.class)).To(gomega.Succeed())
		as, err := f.addressSets.GetAddressSet(getEgressIPDestNetworksAddrSetDbIDs(f.one.Name, f.controller.controllerName))
		f.g.Expect(err).NotTo(gomega.HaveOccurred())
		v4, v6 := as.GetAddresses()
		f.g.Expect(append(v4, v6...)).To(gomega.BeEmpty())
		v4Name, v6Name := as.GetASHashNames()
		asName := v4Name
		if f.ipv6 {
			asName = v6Name
		}
		traffic, err := f.controller.egressTrafficMatchesFor(f.one.Name)
		match := traffic.match(f.ipv6)
		f.g.Expect(err).NotTo(gomega.HaveOccurred())
		f.g.Expect(match).To(gomega.Equal(fmt.Sprintf("(%s.dst == $%s)", f.family, asName)))
		f.assertOwners(2)
		state, _ := f.controller.podAssignment.Load(getPodKey(f.pod))
		f.g.Expect(state.activeEgressIPs).NotTo(gomega.HaveKey(f.one.Name))
		f.g.Expect(state.standbyEgressIPNames.Has(f.one.Name)).To(gomega.BeFalse())
		policies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(f.nb, func(p *nbdb.LogicalRouterPolicy) bool {
			name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
			return name == f.one.Name
		})
		f.g.Expect(err).NotTo(gomega.HaveOccurred())
		f.g.Expect(policies).To(gomega.BeEmpty())
		nats, err := libovsdbops.FindNATsWithPredicate(f.nb, func(n *nbdb.NAT) bool {
			name, _ := getEIPLRPObjK8MetaData(n.ExternalIDs)
			return name == f.one.Name
		})
		f.g.Expect(err).NotTo(gomega.HaveOccurred())
		f.g.Expect(nats).To(gomega.BeEmpty())
		f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(f.one)).To(gomega.Succeed())
		f.g.Expect(f.controller.reconcileEgressIP(invalid, f.one)).To(gomega.Succeed())
		f.assertOwners(3)
	}
}

func (f *selectiveEgressFixture) testFamilyChange() {
	f.restartNetwork()
	f.assertOwners(3)
	// Change the actual assignment family while both pod families are present.
	updated := f.one.DeepCopy()
	updated.Status.Items[0].EgressIP = "2001:db8:1::10"
	f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(updated)).To(gomega.Succeed())
	f.g.Expect(f.controller.reconcileEgressIP(f.one, updated)).To(gomega.Succeed())
	policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool {
		name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
		return name == f.one.Name && isEgressIPReroutePriority(p.Priority)
	})
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(policies).To(gomega.HaveLen(1))
	f.g.Expect(policies[0].Match).To(gomega.HavePrefix("ip6.src == " + f.podIPs[1].IP.String() + " && "))
	f.assertOwners(3)
	if f.network.IsDefault() {
		// Reassign through the real EgressIP handler to a remote egress node.
		remote := f.node.DeepCopy()
		remote.Name = "remote"
		remote.Annotations["k8s.ovn.org/node-transit-switch-port-ifaddr"] = `{"ipv4":"100.88.0.3/16","ipv6":"fd97::3/64"}`
		f.g.Expect(f.watchFactory.NodeCoreInformer().Informer().GetStore().Add(remote)).To(gomega.Succeed())
		f.g.Expect(f.controller.SyncNodeCache()).To(gomega.Succeed())
		moved := updated.DeepCopy()
		moved.Status.Items[0].Node = remote.Name
		f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(moved)).To(gomega.Succeed())
		f.g.Expect(f.controller.reconcileEgressIP(updated, moved)).To(gomega.Succeed())
		policies, err = libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool {
			name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
			return name == f.one.Name && isEgressIPReroutePriority(p.Priority)
		})
		f.g.Expect(err).NotTo(gomega.HaveOccurred())
		f.g.Expect(policies).To(gomega.HaveLen(1))
		f.g.Expect(policies[0].Nexthops).To(gomega.Equal([]string{"fd97::3"}))
		f.assertOwners(3)
	}
}

func (f *selectiveEgressFixture) testRemotePod() {
	// A remote pod needs filtered steering in the egress node's zone too.
	remote := f.node.DeepCopy()
	remote.Name = "remote-pod-node"
	remote.Annotations["k8s.ovn.org/node-transit-switch-port-ifaddr"] = `{"ipv4":"100.88.0.3/16","ipv6":"fd97::3/64"}`
	f.g.Expect(f.watchFactory.NodeCoreInformer().Informer().GetStore().Add(remote)).To(gomega.Succeed())
	f.g.Expect(f.controller.SyncNodeCache()).To(gomega.Succeed())
	remotePod := f.pod.DeepCopy()
	remotePod.Spec.NodeName = remote.Name
	traffic, err := f.controller.egressTrafficMatchesFor(f.one.Name)
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(f.controller.deletePodEgressIPAssignment(f.network, f.one.Name, f.one.Status.Items[0], f.pod)).To(gomega.Succeed())
	f.g.Expect(f.controller.addPodEgressIPAssignment(f.network, f.one.Name, f.one.Status.Items[0], getEgressIPPktMark(f.one.Name, f.one.Annotations), remotePod, f.podIPs, traffic)).To(gomega.Succeed())
	filtered, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool {
		name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
		return name == f.one.Name && p.Priority == types.EgressIPSelectiveReroutePriority
	})
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(filtered).To(gomega.HaveLen(1))
	f.g.Expect(filtered[0].Nexthops).To(gomega.Equal([]string{strings.Split(f.joinIP, "/")[0]}))
	f.g.Expect(f.controller.deletePodEgressIPAssignment(f.network, f.one.Name, f.one.Status.Items[0], remotePod)).To(gomega.Succeed())
	f.g.Expect(f.controller.addPodEgressIPAssignment(f.network, f.one.Name, f.one.Status.Items[0], getEgressIPPktMark(f.one.Name, f.one.Annotations), f.pod, f.podIPs, traffic)).To(gomega.Succeed())
}

func (f *selectiveEgressFixture) testConflictRecovery() {
	// An overlap withdraws the losing owner's whole assignment and can recover.
	originalPeer := f.peerClass.DeepCopy()
	f.peerClass.Spec.Matches = append(f.peerClass.Spec.Matches, trafficv1.TrafficMatch{DestinationCIDR: f.cidr})
	f.g.Expect(f.watchFactory.TrafficClassInformer().Informer().GetStore().Update(f.peerClass)).To(gomega.Succeed())
	f.g.Expect(f.controller.reconcileTrafficClass(f.peerClass)).To(gomega.Succeed())
	f.assertOwners(2)
	f.restartNetwork()
	f.assertOwners(2)
	f.g.Expect(f.watchFactory.TrafficClassInformer().Informer().GetStore().Update(originalPeer)).To(gomega.Succeed())
	f.g.Expect(f.controller.reconcileTrafficClass(originalPeer)).To(gomega.Succeed())
	f.assertOwners(3)
}

func (f *selectiveEgressFixture) testClassUpdates() {
	// L4 edits rebuild the owner; a peer's assignment survives.
	f.class.Spec.Matches[1].Protocols[0].UDP.DestinationPort = &trafficv1.Port{Number: ptr.To[int32](5060)}
	f.g.Expect(f.watchFactory.TrafficClassInformer().Informer().GetStore().Update(f.class)).To(gomega.Succeed())
	f.controller.addressSetFactory = &failingTrafficAddressSets{AddressSetFactory: f.addressSets, failAdd: true}
	f.g.Expect(f.controller.reconcileTrafficClass(f.class)).To(gomega.MatchError(gomega.ContainSubstring("injected served-pod address-set failure")))
	f.g.Expect(f.controller.reconcileTrafficClass(f.class)).To(gomega.Succeed())
	f.controller.addressSetFactory = f.addressSets
	f.assertOwners(3)
	policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool {
		return strings.HasPrefix(p.ExternalIDs[libovsdbops.ObjectNameKey.String()], "one_")
	})
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(policies[0].Match).To(gomega.ContainSubstring("udp.dst == 5060"))
	f.g.Expect(f.watchFactory.TrafficClassInformer().Informer().GetStore().Delete(f.class)).To(gomega.Succeed())
	f.g.Expect(f.controller.reconcileTrafficClass(f.class)).To(gomega.Succeed())
	f.assertOwners(2)
	empty, err := f.addressSets.GetAddressSet(getEgressIPDestNetworksAddrSetDbIDs(f.one.Name, f.controller.controllerName))
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	v4, v6 := empty.GetAddresses()
	f.g.Expect(append(v4, v6...)).To(gomega.BeEmpty())
	f.g.Expect(f.watchFactory.TrafficClassInformer().Informer().GetStore().Add(f.class)).To(gomega.Succeed())
	f.g.Expect(f.controller.reconcileTrafficClass(f.class)).To(gomega.Succeed())
	catchAll := f.one.DeepCopy()
	catchAll.Spec.TrafficSelector = nil
	f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(catchAll)).To(gomega.Succeed())
	f.g.Expect(f.controller.reconcileEgressIP(f.one, catchAll)).To(gomega.Succeed())
	f.assertOwners(2)
	destinationIDs := getEgressIPDestNetworksAddrSetDbIDs(f.one.Name, f.controller.controllerName)
	destinationSets, err := libovsdbops.FindAddressSetsWithPredicate(f.nb, libovsdbops.GetPredicate[*nbdb.AddressSet](destinationIDs, nil))
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(destinationSets).To(gomega.BeEmpty())
	f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(f.one)).To(gomega.Succeed())
	f.g.Expect(f.controller.reconcileEgressIP(catchAll, f.one)).To(gomega.Succeed())
	f.assertOwners(3)
}

func (f *selectiveEgressFixture) testPodIPReplacement() {
	var err error
	// Rebuild all owners after an IP replacement.
	oldIPs := f.podIPs
	f.podIPs = ovntest.MustParseIPNets(f.podPrefix + "20" + f.podMask)
	f.pod.Annotations, err = util.MarshalPodAnnotation(nil, &util.PodAnnotation{IPs: f.podIPs, MAC: f.mac, Role: types.NetworkRolePrimary}, f.nadKey)
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.pod.Status.PodIP = f.podIPs[0].IP.String()
	f.pod.Status.PodIPs = []corev1.PodIP{{IP: f.pod.Status.PodIP}}
	f.g.Expect(f.watchFactory.PodCoreInformer().Informer().GetStore().Update(f.pod)).To(gomega.Succeed())
	f.portCache.add(f.pod, "", f.nadKey, "", f.mac, f.podIPs)
	ownersToUpdate := []*eipv1.EgressIP{f.fallback, f.one, f.two}
	for i, owner := range ownersToUpdate {
		f.g.Expect(f.controller.addPodEgressIPAssignmentsWithLock(f.network, owner.Name, owner.Status.Items, getEgressIPPktMark(owner.Name, owner.Annotations), f.pod)).To(gomega.Succeed())
		if i < len(ownersToUpdate)-1 {
			// Peers still use the old IP until their own assignments are replaced.
			f.assertServedPodIPs(append(oldIPs, f.podIPs...))
		} else {
			f.assertServedPodIPs(f.podIPs)
		}
	}
	f.assertOwners(3)
}

func (f *selectiveEgressFixture) testStartupRepair() {
	// A cache restart must reconstruct each persisted owner, including legacy priority 100.
	legacyPolicies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool {
		name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
		return name == "fallback"
	})
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(legacyPolicies).To(gomega.HaveLen(1))
	legacy := *legacyPolicies[0]
	legacy.UUID, legacy.Priority = "", types.EgressIPLegacyReroutePriority
	legacyIDs, err := libovsdbops.NewDbObjectIDsFromExternalIDs(libovsdbops.LogicalRouterPolicyEgressIP, legacy.ExternalIDs)
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	legacyIDs = legacyIDs.AddIDs(map[libovsdbops.ExternalIDKey]string{libovsdbops.PriorityKey: fmt.Sprint(types.EgressIPLegacyReroutePriority)})
	legacy.ExternalIDs = legacyIDs.GetExternalIDs()
	ops, err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(f.nb, nil, f.router, &legacy, libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](legacyIDs, nil))
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = libovsdbops.TransactAndCheck(f.nb, ops)
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.controller.podAssignment = syncmap.NewSyncMap[*podAssignmentState]()
	f.g.Expect(f.controller.migrateEgressIPReroutePriorities(egressIPAllNetworks)).To(gomega.Succeed())
	cache, err := f.controller.generateCacheForEgressIP(egressIPAllNetworks)
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	// Repair must recreate a missing mark even when all remaining marks are valid.
	f.g.Expect(libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(f.nb, f.gateway, func(p *nbdb.LogicalRouterPolicy) bool {
		name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
		return isEgressIPMarkPriority(p.Priority) && name == f.one.Name
	})).To(gomega.Succeed())
	f.g.Expect(f.controller.syncStaleGWMarkRules(cache, egressIPAllNetworks)).To(gomega.Succeed())
	f.g.Expect(f.controller.syncStaleGWMarkRules(cache, egressIPAllNetworks)).To(gomega.Succeed())
	f.restartNetwork()
	f.assertOwners(3)
	// A peer's local assignment must not keep another owner's gateway mark alive.
	cache.egressIPNameToAssignedNodes[f.one.Name] = []string{"other-node"}
	cache.egressIPToAssignedNodes[f.one.Name] = map[string]string{f.one.Status.Items[0].EgressIP: "other-node"}
	f.g.Expect(f.controller.syncStaleGWMarkRules(cache, egressIPAllNetworks)).To(gomega.Succeed())
	f.assertGatewayMarks(2)
	cache, err = f.controller.generateCacheForEgressIP(egressIPAllNetworks)
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(f.controller.syncStaleGWMarkRules(cache, egressIPAllNetworks)).To(gomega.Succeed())
	f.assertGatewayMarks(3)
	otherFamily := "2001:db8::10"
	if f.ipv6 {
		otherFamily = "198.51.100.10"
	}
	cache.egressIPToAssignedNodes[f.one.Name] = map[string]string{otherFamily: "node"}
	f.g.Expect(f.controller.syncStaleGWMarkRules(cache, egressIPAllNetworks)).To(gomega.Succeed())
	f.assertGatewayMarks(2)
	cache, err = f.controller.generateCacheForEgressIP(egressIPAllNetworks)
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(f.controller.syncStaleGWMarkRules(cache, egressIPAllNetworks)).To(gomega.Succeed())
	f.assertGatewayMarks(3)
}

func (f *selectiveEgressFixture) testGateDisableAndNetworkCleanup() {
	f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Delete(f.one)).To(gomega.Succeed())
	f.g.Expect(f.controller.reconcileEgressIP(f.one, nil)).To(gomega.Succeed())
	f.assertOwners(2)
	// With the gate off, selective peers withdraw without becoming catch-alls.
	config.OVNKubernetesFeature.EnableTrafficClass = false
	f.g.Expect(f.controller.syncEgressTraffic()).To(gomega.Succeed())
	f.g.Expect(f.controller.migrateEgressIPReroutePriorities(egressIPAllNetworks)).To(gomega.Succeed())
	f.g.Expect(f.controller.addPodEgressIPAssignmentsWithLock(f.network, f.fallback.Name, f.fallback.Status.Items, getEgressIPPktMark(f.fallback.Name, f.fallback.Annotations), f.pod)).To(gomega.Succeed())
	f.g.Expect(f.controller.addPodEgressIPAssignmentsWithLock(f.network, f.two.Name, f.two.Status.Items, getEgressIPPktMark(f.two.Name, f.two.Annotations), f.pod)).To(gomega.Succeed())
	state, _ := f.controller.podAssignment.Load(getPodKey(f.pod))
	f.g.Expect(state.activeEgressIPs).To(gomega.HaveLen(1))
	f.g.Expect(state.standbyEgressIPNames).To(gomega.BeEmpty())
	remaining, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool { return isEgressIPReroutePriority(p.Priority) })
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(remaining).To(gomega.HaveLen(1))
	f.g.Expect(remaining[0].Priority).To(gomega.Equal(99))
	f.g.Expect(remaining[0].Match).To(gomega.Equal(f.family + ".src == " + f.podIPs[0].IP.String()))
	// Restart reconstruction must include every namespace sharing a network.
	peerPod := f.pod.DeepCopy()
	peerPod.Namespace = "peer"
	f.networkManager.PrimaryNetworks[peerPod.Namespace] = f.network
	f.g.Expect(f.watchFactory.NamespaceCoreInformer().Informer().GetStore().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: peerPod.Namespace, Labels: map[string]string{corev1.LabelMetadataName: peerPod.Namespace}}})).To(gomega.Succeed())
	f.g.Expect(f.watchFactory.PodCoreInformer().Informer().GetStore().Add(peerPod)).To(gomega.Succeed())
	f.portCache.add(peerPod, "", f.nadKey, "", f.mac, f.podIPs)
	cache, err := f.controller.generateCacheForEgressIP(egressIPAllNetworks)
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(cache.egressIPNameToPods[f.fallback.Name][f.network.GetNetworkName()].egressLocalPods).To(gomega.HaveLen(2))
	// Startup repair clears shared membership when owners select no pods.
	owners, err := f.watchFactory.GetEgressIPs()
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	for _, owner := range owners {
		updated := owner.DeepCopy()
		updated.Spec.PodSelector = metav1.LabelSelector{MatchLabels: map[string]string{"missing": "pod"}}
		f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(updated)).To(gomega.Succeed())
	}
	f.controller.podAssignment = syncmap.NewSyncMap[*podAssignmentState]()
	f.restartNetwork()
	stale, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool { return isEgressIPReroutePriority(p.Priority) })
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	f.g.Expect(stale).To(gomega.BeEmpty())
	served, err := f.addressSets.GetAddressSet(getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, f.network.GetNetworkName(), f.controller.controllerName))
	f.g.Expect(err).NotTo(gomega.HaveOccurred())
	servedV4, servedV6 := served.GetAddresses()
	f.g.Expect(append(servedV4, servedV6...)).To(gomega.BeEmpty())
}

// Block an address-set operation after the class handler has listed all EgressIPs.
type blockingTrafficAddressSets struct {
	addressset.AddressSetFactory
	first   atomic.Bool
	entered chan string
	release chan struct{}
}

func (f *blockingTrafficAddressSets) NewAddressSet(ids *libovsdbops.DbObjectIDs, addresses []string) (addressset.AddressSet, error) {
	if f.first.CompareAndSwap(false, true) {
		f.entered <- strings.TrimPrefix(ids.GetObjectID(libovsdbops.ObjectNameKey), egressIPDestinationNetworksPrefix)
		<-f.release
	}
	return f.AddressSetFactory.NewAddressSet(ids, addresses)
}

func TestSelectiveTrafficClassConcurrentEgressIPUpdate(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted=%t", deleted), func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			t.Cleanup(func() { _ = config.PrepareTestConfig() })
			config.OVNKubernetesFeature.EnableEgressIP = true
			config.OVNKubernetesFeature.EnableTrafficClass = true
			clients := util.GetOVNClientset().GetOVNKubeControllerClientset()
			wf, err := factory.NewOVNKubeControllerWatchFactory(clients, "node")
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(wf.Stop)
			owners := map[string]*eipv1.EgressIP{}
			for _, name := range []string{"one", "two"} {
				owner := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"traffic": "old"}}}}
				owners[name] = owner
				g.Expect(wf.EgressIPInformer().Informer().GetStore().Add(owner)).To(gomega.Succeed())
			}
			class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "old", Labels: map[string]string{"traffic": "old"}}, Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: "192.0.2.0/24"}}}}
			newClass := class.DeepCopy()
			newClass.Name, newClass.Labels = "new", map[string]string{"traffic": "new"}
			newClass.Spec.Matches[0].DestinationCIDR = "203.0.113.0/24"
			for _, class := range []*trafficv1.TrafficClass{class, newClass} {
				g.Expect(wf.TrafficClassInformer().Informer().GetStore().Add(class)).To(gomega.Succeed())
			}
			nb, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(cleanup.Cleanup)
			asf := &blockingTrafficAddressSets{AddressSetFactory: addressset.NewOvnAddressSetFactory(nb, true, false), entered: make(chan string, 1), release: make(chan struct{})}
			e := NewEIPController(nb, &kube.KubeOVN{EIPClient: clients.EgressIPClient}, wf, record.NewFakeRecorder(10), nil, &networkmanager.FakeNetworkManager{}, asf, nil, true, false, "node", types.DefaultNetworkControllerName)
			var release sync.Once
			var handlers sync.WaitGroup
			t.Cleanup(func() {
				release.Do(func() { close(asf.release) })
				stopped := make(chan struct{})
				go func() { handlers.Wait(); close(stopped) }()
				g.Eventually(stopped, 5*time.Second).Should(gomega.BeClosed(), "TrafficClass handlers did not stop")
			})
			done := make(chan error, 1)
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				done <- e.reconcileTrafficClass(class)
			}()
			var blocked string
			g.Eventually(asf.entered, 5*time.Second).Should(gomega.Receive(&blocked))
			name := "one"
			if blocked == name {
				name = "two"
			}
			old := owners[name]
			if deleted {
				g.Expect(wf.EgressIPInformer().Informer().GetStore().Delete(old)).To(gomega.Succeed())
				g.Expect(e.reconcileEgressIP(old, nil)).To(gomega.Succeed())
			} else {
				updated := old.DeepCopy()
				updated.Spec.TrafficSelector = &metav1.LabelSelector{MatchLabels: newClass.Labels}
				g.Expect(wf.EgressIPInformer().Informer().GetStore().Update(updated)).To(gomega.Succeed())
				g.Expect(e.reconcileEgressIP(old, updated)).To(gomega.Succeed())
			}
			release.Do(func() { close(asf.release) })
			g.Eventually(done, 5*time.Second).Should(gomega.Receive(gomega.Succeed()))
			ids := getEgressIPDestNetworksAddrSetDbIDs(name, e.controllerName)
			if deleted {
				rows, err := libovsdbops.FindAddressSetsWithPredicate(nb, libovsdbops.GetPredicate[*nbdb.AddressSet](ids, nil))
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(rows).To(gomega.BeEmpty(), "deleted owner was recreated by stale class work")
				_, cached := e.trafficMatches.Load(name)
				g.Expect(cached).To(gomega.BeFalse())
			} else {
				as, err := asf.GetAddressSet(ids)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				v4, _ := as.GetAddresses()
				g.Expect(v4).To(gomega.Equal([]string{"203.0.113.0/24"}))
			}
		})
	}
}

type failingTrafficAddressSets struct {
	addressset.AddressSetFactory
	failAdd bool
	failNew bool
}

func (f *failingTrafficAddressSets) NewAddressSet(ids *libovsdbops.DbObjectIDs, addresses []string) (addressset.AddressSet, error) {
	if f.failNew {
		f.failNew = false
		return nil, fmt.Errorf("injected traffic address-set failure")
	}
	return f.AddressSetFactory.NewAddressSet(ids, addresses)
}

func (f *failingTrafficAddressSets) GetAddressSet(ids *libovsdbops.DbObjectIDs) (addressset.AddressSet, error) {
	as, err := f.AddressSetFactory.GetAddressSet(ids)
	if err != nil {
		return nil, err
	}
	return &failingTrafficAddressSet{AddressSet: as, factory: f}, nil
}

type failingTrafficAddressSet struct {
	addressset.AddressSet
	factory *failingTrafficAddressSets
}

func (as *failingTrafficAddressSet) AddAddresses(addresses []string) error {
	if as.factory.failAdd {
		as.factory.failAdd = false
		return fmt.Errorf("injected served-pod address-set failure")
	}
	return as.AddressSet.AddAddresses(addresses)
}

// Per-network startup must not prune destination sets still used by a pending network.
func TestPruneEgressTrafficPreservesReferencedSets(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableEgressIP = true
	config.OVNKubernetesFeature.EnableTrafficClass = true
	clients := util.GetOVNClientset().GetOVNKubeControllerClientset()
	wf, err := factory.NewOVNKubeControllerWatchFactory(clients, "node")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(wf.Stop)
	nb, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{&nbdb.LogicalRouter{Name: "pending-network"}},
	}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(cleanup.Cleanup)
	asf := addressset.NewOvnAddressSetFactory(nb, true, false)
	ids := getEgressIPDestNetworksAddrSetDbIDs("deleted-owner", types.DefaultNetworkControllerName)
	as, err := asf.NewAddressSet(ids, []string{"203.0.113.0/24"})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	v4, _ := as.GetASHashNames()
	policy := &nbdb.LogicalRouterPolicy{Priority: 100, Match: "ip4.dst == $" + v4, Action: nbdb.LogicalRouterPolicyActionAllow}
	ops, err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(nb, nil, "pending-network", policy, func(p *nbdb.LogicalRouterPolicy) bool {
		return p.Match == policy.Match
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = libovsdbops.TransactAndCheck(nb, ops)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	e := NewEIPController(nb, &kube.KubeOVN{EIPClient: clients.EgressIPClient}, wf, record.NewFakeRecorder(10), nil, &networkmanager.FakeNetworkManager{}, asf, nil, true, false, "node", types.DefaultNetworkControllerName)
	e.trafficMatches.Store("deleted-owner", egressTrafficMatches{v4: "ip4.dst == $" + v4})
	g.Expect(e.deleteEgressTraffic("deleted-owner")).To(gomega.Succeed())
	_, cached := e.trafficMatches.Load("deleted-owner")
	g.Expect(cached).To(gomega.BeFalse())
	g.Expect(e.pruneEgressTraffic()).To(gomega.Succeed())
	sets, err := libovsdbops.FindAddressSetsWithPredicate(nb, func(a *nbdb.AddressSet) bool { return a.Name == v4 })
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(sets).To(gomega.HaveLen(1))
	g.Expect(libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(nb, "pending-network", func(p *nbdb.LogicalRouterPolicy) bool {
		return p.Match == policy.Match
	})).To(gomega.Succeed())
	g.Expect(e.pruneEgressTraffic()).To(gomega.Succeed())
	sets, err = libovsdbops.FindAddressSetsWithPredicate(nb, func(a *nbdb.AddressSet) bool { return a.Name == v4 })
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(sets).To(gomega.BeEmpty())
}

func TestEgressRerouteInsertionPreservesConcurrentRemoval(t *testing.T) {
	f := newSelectiveEgressFixture(t, "default", "IPv4")
	f.testSelectionAndResolutionRetry()
	f.testProgramming()
	g := f.g
	policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool {
		return p.ExternalIDs[libovsdbops.ObjectNameKey.String()] == f.one.Name+"_"+f.pod.Namespace+"/"+f.pod.Name
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(policies).To(gomega.HaveLen(1))
	policy := policies[0]
	// Keep one other hop so status cleanup mutates the existing row rather than deletes it.
	policy.Nexthops = []string{"100.64.0.2", "100.64.0.3"}
	ops, err := libovsdbops.UpdateLogicalRouterPoliciesOps(f.nb, nil, policy)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = libovsdbops.TransactAndCheck(f.nb, ops)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	traffic, err := f.controller.egressTrafficMatchesFor(f.one.Name)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	ops, err = f.controller.createReroutePolicyOps(f.network, nil, f.podIPs, f.one.Status.Items[0], "", f.one.Name,
		"100.64.0.4", f.router, f.pod.Namespace, f.pod.Name, traffic.v4)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	removeOps, err := libovsdbops.DeleteNextHopsFromLogicalRouterPolicyOps(f.nb, nil, f.router, policies, "100.64.0.2")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = libovsdbops.TransactAndCheck(f.nb, removeOps)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = libovsdbops.TransactAndCheck(f.nb, ops)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Eventually(func() []string {
		current, lookupErr := libovsdbops.GetLogicalRouterPolicy(f.nb, &nbdb.LogicalRouterPolicy{UUID: policy.UUID})
		g.Expect(lookupErr).NotTo(gomega.HaveOccurred())
		return current.Nexthops
	}, "5s").Should(gomega.ConsistOf("100.64.0.3", "100.64.0.4"))
}

func TestEgressTrafficMigrationSkipsUnchangedRows(t *testing.T) {
	f := newSelectiveEgressFixture(t, "default", "IPv4")
	f.testSelectionAndResolutionRetry()
	f.testProgramming()
	f.g.Expect(f.controller.migrateEgressIPReroutePriorities(egressIPAllNetworks)).To(gomega.Succeed())
	recording := libovsdbtest.NewRecordingClient(f.nb)
	f.controller.nbClient = recording
	f.g.Expect(f.controller.migrateEgressIPReroutePriorities(egressIPAllNetworks)).To(gomega.Succeed())
	f.g.Expect(recording.Transactions).To(gomega.BeEmpty(), "unchanged migration must not rewrite policy or NAT rows")
}

func TestNamespaceRetriesRequireSelectiveOwners(t *testing.T) {
	for _, selective := range []bool{false, true} {
		t.Run(fmt.Sprintf("selective=%t", selective), func(t *testing.T) {
			f := newSelectiveEgressFixture(t, "default", "IPv4")
			f.g.Expect(f.watchFactory.PodInformer().GetStore().Add(f.pod)).To(gomega.Succeed())
			if !selective {
				for _, owner := range []*eipv1.EgressIP{f.one, f.two} {
					f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Delete(owner)).To(gomega.Succeed())
				}
			}
			stop := make(chan struct{})
			defer close(stop)
			var workers sync.WaitGroup
			oc := &DefaultNetworkController{
				BaseNetworkController: BaseNetworkController{
					CommonNetworkControllerInfo: CommonNetworkControllerInfo{watchFactory: f.watchFactory},
					ReconcilableNetInfo:         &util.DefaultNetInfo{}, stopChan: stop, wg: &workers,
				},
				eIPC: f.controller,
			}
			f.controller.retryEgressIPPods = oc.newRetryFramework(factory.EgressIPPodType)
			namespace, err := f.watchFactory.GetNamespace(f.pod.Namespace)
			f.g.Expect(err).NotTo(gomega.HaveOccurred())
			f.g.Expect(f.controller.reconcileEgressIPNamespace(nil, namespace)).To(gomega.Succeed())
			key, err := retry.GetResourceKey(f.pod)
			f.g.Expect(err).NotTo(gomega.HaveOccurred())
			f.g.Expect(retry.CheckRetryObj(key, f.controller.retryEgressIPPods)).To(gomega.Equal(selective))
		})
	}
}
