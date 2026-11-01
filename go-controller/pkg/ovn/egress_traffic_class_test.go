// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
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
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

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
	f.g.Expect(f.controller.reconcileEgressIP(nil, f.one)).To(gomega.Succeed(), "apply the original selective owner")
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
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "register the TrafficClass retry handler")
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
	f.g.Expect(f.watchFactory.TrafficClassInformer().Informer().GetStore().Update(changed)).To(gomega.Succeed(), "publish the unmatched TrafficClass labels")
	handler.OnUpdate(f.class, changed)
	f.g.Expect(retry.CheckRetryObj(f.class.Name, r)).To(gomega.BeTrue(), "retain the failed class update for retry")
	// Another label update must not replace the still-pending original labels.
	later := changed.DeepCopy()
	later.Labels["unrelated"] = "new"
	f.g.Expect(f.watchFactory.TrafficClassInformer().Informer().GetStore().Update(later)).To(gomega.Succeed(), "publish the subsequent metadata update")
	r.DoWithLock(f.class.Name, func(string) { failures.failNew = true })
	handler.OnUpdate(changed, later)
	f.g.Expect(retry.GetOldObjFromRetryObj(f.class.Name, r)).To(gomega.Equal(f.class), "preserve the original labels across update retries")
	retry.SetRetryObjWithNoBackoff(f.class.Name, r)
	r.RequestRetryObjs()
	f.g.Eventually(func() bool { return retry.CheckRetryObj(f.class.Name, r) }, 5*time.Second).Should(gomega.BeFalse(), "complete the pending TrafficClass retry")
	state, _ := f.controller.podAssignment.Load(getPodKey(f.pod))
	f.g.Expect(state.activeEgressIPs).NotTo(gomega.HaveKey(f.one.Name), "withdraw the owner selected by the original labels")
	policies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(f.nb, func(policy *nbdb.LogicalRouterPolicy) bool {
		name, _ := getEIPLRPObjK8MetaData(policy.ExternalIDs)
		return name == f.one.Name
	})
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list the original owner policies after retry")
	f.g.Expect(policies).To(gomega.BeEmpty(), "remove policies selected by the original labels")
}

func TestTrafficClassUnchangedAssignments(t *testing.T) {
	f := newSelectiveEgressFixture(t, "default", "IPv4")
	f.g.Expect(f.controller.reconcileEgressIPPod(nil, f.pod)).To(gomega.Succeed(), "reconcile all owners for the pod")
	f.assertOwners(3)
	f.restartNetwork()
	policies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(f.nb, func(*nbdb.LogicalRouterPolicy) bool { return true })
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list policies before repeated class events")
	nats, err := libovsdbops.FindNATsWithPredicate(f.nb, func(*nbdb.NAT) bool { return true })
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list NATs before repeated class events")
	// Initial class notifications and repeated events must preserve applied rows, including UUIDs.
	for _, class := range []*trafficv1.TrafficClass{f.class, f.peerClass, f.class} {
		f.g.Expect(f.controller.reconcileTrafficClass(class)).To(gomega.Succeed(), "apply TrafficClass selection changes")
	}
	afterPolicies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(f.nb, func(*nbdb.LogicalRouterPolicy) bool { return true })
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list policies after repeated class events")
	f.g.Expect(afterPolicies).To(gomega.ConsistOf(policies))
	afterNATs, err := libovsdbops.FindNATsWithPredicate(f.nb, func(*nbdb.NAT) bool { return true })
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list NATs after repeated class events")
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
		f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Add(owner)).To(gomega.Succeed(), "cache the test EgressIP")
	}
	f.g.Expect(f.controller.reconcileEgressIP(nil, active)).To(gomega.Succeed(), "reconcile EgressIP assignments")
	// In the egress zone, NAT alone can identify the active owner after restart.
	f.g.Expect(libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool {
		name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
		return name == active.Name
	})).To(gomega.Succeed(), "remove reroute policies to test NAT-only owner recovery")
	f.controller.podAssignment = syncmap.NewSyncMap[*podAssignmentState]()
	cache, err := f.controller.generateCacheForEgressIP(egressIPAllNetworks)
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "generate the restart assignment cache")
	f.g.Expect(f.controller.syncPodAssignmentCache(cache, egressIPAllNetworks)).To(gomega.Succeed(), "restore owner assignments from the restart cache")
	state, ok := f.controller.podAssignment.Load(getPodKey(f.pod))
	f.g.Expect(ok).To(gomega.BeTrue(), "restore the pod assignment state")
	f.g.Expect(state.activeEgressIPs).To(gomega.HaveKey(active.Name))
	f.g.Expect(state.activeEgressIPs).NotTo(gomega.HaveKey(standby.Name))
	f.g.Expect(state.standbyEgressIPNames.Has(standby.Name)).To(gomega.BeTrue(), "retain the standby catch-all owner")
}

func TestInvalidWorkloadSelectorWithdrawsSelectiveOwner(t *testing.T) {
	for _, field := range []string{"namespace", "pod"} {
		t.Run(field, func(t *testing.T) {
			f := newSelectiveEgressFixture(t, "default", "IPv4")
			for _, owner := range []*eipv1.EgressIP{f.one, f.two, f.fallback} {
				f.g.Expect(f.controller.reconcileEgressIP(nil, owner)).To(gomega.Succeed(), "reconcile EgressIP assignments")
			}
			invalid := f.one.DeepCopy()
			selector := metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: "Invalid"}}}
			if field == "namespace" {
				invalid.Spec.NamespaceSelector = selector
			} else {
				invalid.Spec.PodSelector = selector
			}
			f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(invalid)).To(gomega.Succeed(), "update the cached EgressIP")
			// A healthy peer's reconciliation must withdraw the invalid applied owner.
			f.g.Expect(f.controller.addPodEgressIPAssignmentsWithLock(f.network, f.two.Name, f.two.Status.Items,
				getEgressIPPktMark(f.two.Name, f.two.Annotations), f.pod)).To(gomega.Succeed(), "reconcile the healthy peer with an invalid owner")
			f.assertOwners(2)
			f.restartNetwork()
			f.g.Expect(f.controller.addPodEgressIPAssignmentsWithLock(f.network, f.two.Name, f.two.Status.Items,
				getEgressIPPktMark(f.two.Name, f.two.Annotations), f.pod)).To(gomega.Succeed(), "restore the healthy peer after restart")
			f.assertOwners(2)
		})
	}
}

func TestLegacyOwnerlessRerouteCleanup(t *testing.T) {
	f := newSelectiveEgressFixture(t, "default", "IPv4")
	_, subnet, err := net.ParseCIDR("10.128.0.0/14")
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "parse the cluster subnet")
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: subnet}}
	stale := &nbdb.LogicalRouterPolicy{
		Priority: types.EgressIPLegacyReroutePriority, Action: nbdb.LogicalRouterPolicyActionReroute,
		Match: "ip4.src == 10.128.0.99", Nexthops: []string{"100.64.0.2"}, ExternalIDs: map[string]string{"name": "deleted-owner"},
	}
	ops, err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(f.nb, nil, f.router, stale, func(*nbdb.LogicalRouterPolicy) bool { return false })
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "stage the legacy ownerless policy")
	_, err = libovsdbops.TransactAndCheck(f.nb, ops)
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "create the legacy ownerless policy")
	f.g.Expect(logical_router_policy.NewLRPSyncer(f.nb, f.controller.controllerName).Sync()).To(gomega.Succeed(), "adopt legacy EgressIP policies")
	policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(policy *nbdb.LogicalRouterPolicy) bool { return policy.Match == stale.Match })
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "find the adopted policy")
	f.g.Expect(policies).To(gomega.HaveLen(1))
	f.g.Expect(policies[0].ExternalIDs[libovsdbops.OwnerControllerKey.String()]).To(gomega.Equal(f.controller.controllerName))
	f.g.Expect(f.controller.migrateEgressIPReroutePriorities(egressIPAllNetworks)).To(gomega.Succeed(), "migrate legacy EgressIP policies")
	policies, err = libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(policy *nbdb.LogicalRouterPolicy) bool { return policy.Match == stale.Match })
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "find policies after legacy cleanup")
	f.g.Expect(policies).To(gomega.BeEmpty())
}

func TestSelectiveEgressReconciliation(t *testing.T) {
	for _, family := range []string{"IPv4", "IPv6", "dual-stack"} {
		for _, topology := range []string{"default", types.Layer2Topology, types.Layer3Topology} {
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
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "prepare test configuration")
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.IPv4Mode, config.IPv6Mode = !ipv6, ipv6 || dualStack
	config.OVNKubernetesFeature.EnableEgressIP = true
	config.OVNKubernetesFeature.EnableTrafficClass = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
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
		g.Expect(err).NotTo(gomega.HaveOccurred(), "parse the primary network attachment")
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
	g.Expect(err).NotTo(gomega.HaveOccurred(), "annotate the test pod network")
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
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the controller watch factory")
	t.Cleanup(wf.Shutdown)
	for _, eip := range []*eipv1.EgressIP{one, two, fallback} {
		g.Expect(wf.EgressIPInformer().Informer().GetStore().Add(eip)).To(gomega.Succeed(), "cache the test EgressIP")
	}
	g.Expect(wf.TrafficClassInformer().Informer().GetStore().Add(class)).To(gomega.Succeed(), "cache the test TrafficClass")
	g.Expect(wf.TrafficClassInformer().Informer().GetStore().Add(peerClass)).To(gomega.Succeed(), "cache the test TrafficClass")
	g.Expect(wf.PodCoreInformer().Informer().GetStore().Add(pod)).To(gomega.Succeed(), "cache the test pod")
	g.Expect(wf.NamespaceCoreInformer().Informer().GetStore().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "work", Labels: map[string]string{corev1.LabelMetadataName: "work"}}})).To(gomega.Succeed(), "cache the test namespace")
	g.Expect(wf.NodeCoreInformer().Informer().GetStore().Add(node)).To(gomega.Succeed(), "cache the test node")
	router := ni.GetNetworkScopedClusterRouterName()
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
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the northbound test database")
	t.Cleanup(cleanup.Cleanup)
	asf := addressset.NewOvnAddressSetFactory(nb, !ipv6, ipv6 || dualStack)
	_, err = asf.EnsureAddressSet(getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, ni.GetNetworkName(), types.DefaultNetworkControllerName))
	g.Expect(err).NotTo(gomega.HaveOccurred(), "initialize the served-pod address set")
	pc := NewPortCache(make(chan struct{}))
	pc.add(pod, "", nadKey, "", mac, ips)
	nm := &networkmanager.FakeNetworkManager{PrimaryNetworks: map[string]util.NetInfo{"work": ni}, NADNetworks: map[string]util.NetInfo{nadKey: ni}}
	e := NewEIPController(nb, &kube.KubeOVN{EIPClient: clients.EgressIPClient}, wf, record.NewFakeRecorder(10), pc, nm, asf, nil, !ipv6, ipv6 || dualStack, "node", types.DefaultNetworkControllerName)
	g.Expect(e.SyncNodeCache()).To(gomega.Succeed(), "populate the controller node cache")
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

func (f *selectiveEgressFixture) destinationSetIDs(owner string) *libovsdbops.DbObjectIDs {
	traffic, ok := f.controller.trafficMatches.Load(owner)
	f.g.Expect(ok).To(gomega.BeTrue(), "resolve the staged destination set for %s", owner)
	return getEgressIPDestNetworksAddrSetDbIDs(traffic.(egressTrafficMatches).addressSetName, f.controller.controllerName)
}

func (f *selectiveEgressFixture) restartNetwork() {
	f.t.Helper()
	f.controller.UnregisterNetwork(f.network.GetNetworkName())
	f.controller.ensureNetworkPending(f.network.GetNetworkName())
	f.g.Expect(f.controller.SyncNetworkReady(f.network.GetNetworkName())).To(gomega.Succeed(), "repair resources for the active network")
}

func (f *selectiveEgressFixture) assertGatewayMarks(expected int) {
	f.t.Helper()
	marks, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.gateway, func(p *nbdb.LogicalRouterPolicy) bool { return isEgressIPMarkPriority(p.Priority) })
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list gateway mark policies")
	if f.topology == "default" {
		f.g.Expect(marks).To(gomega.BeEmpty())
		return
	}
	f.g.Expect(marks).To(gomega.HaveLen(expected))
	for _, mark := range marks {
		f.g.Expect(mark.Match).To(gomega.ContainSubstring("pkt.mark == 0"))
		name, _ := getEIPLRPObjK8MetaData(mark.ExternalIDs)
		traffic, err := f.controller.egressTrafficMatchesFor(name)
		match := traffic.match(strings.HasPrefix(mark.Match, "ip6"))
		f.g.Expect(err).NotTo(gomega.HaveOccurred(), "get the mark policy owner traffic match")
		f.g.Expect(mark.Priority).To(gomega.Equal(egressIPMarkPriority(match)))
		f.g.Expect(mark.Match).To(gomega.HaveSuffix(match))
	}
}

func (f *selectiveEgressFixture) assertServedPodIPs(expected []*net.IPNet) {
	f.t.Helper()
	served, err := f.addressSets.GetAddressSet(getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, f.network.GetNetworkName(), f.controller.controllerName))
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "get the served-pod address set")
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
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list owner reroute policies")
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
	f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(unselected)).To(gomega.Succeed(), "update the cached EgressIP")
	f.g.Expect(f.controller.addPodEgressIPAssignmentsWithLock(f.network, f.one.Name, f.one.Status.Items, getEgressIPPktMark(f.one.Name, f.one.Annotations), f.pod)).To(gomega.Succeed(), "apply the owner pod assignments")
	_, hasPodState := f.controller.podAssignment.Load(getPodKey(f.pod))
	f.g.Expect(hasPodState).To(gomega.BeFalse(), "leave an unselected pod without applied state")
	f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(f.one)).To(gomega.Succeed(), "update the cached EgressIP")
	// A status update must recover if traffic resolution failed on the add event.
	unassigned := f.one.DeepCopy()
	unassigned.Status.Items = nil
	f.controller.trafficMatches.Delete(f.one.Name)
	f.controller.addressSetFactory = &failingTrafficAddressSets{AddressSetFactory: f.addressSets, failNew: true}
	f.g.Expect(f.controller.reconcileEgressIP(nil, unassigned)).To(gomega.MatchError(gomega.ContainSubstring("injected traffic address-set failure")))
	f.g.Expect(f.controller.reconcileEgressIP(unassigned, f.one)).To(gomega.Succeed(), "reconcile EgressIP assignments")
	f.controller.addressSetFactory = f.addressSets
	for _, owner := range []*eipv1.EgressIP{f.fallback, f.one, f.two} {
		f.g.Expect(f.controller.reconcileEgressIP(nil, owner)).To(gomega.Succeed(), "reconcile EgressIP assignments")
	}
	f.assertOwners(3)
}

func (f *selectiveEgressFixture) testProgramming() {
	// Empty namespace selectors also match namespaces with only the API-server name label.
	f.g.Expect(f.controller.deletePodEgressIPAssignmentsUsingCache(f.one.Name, f.pod, false)).To(gomega.Succeed(), "remove the cached owner pod assignments")
	f.assertOwners(2)
	f.g.Expect(f.controller.reconcileEgressIPPod(nil, f.pod)).To(gomega.Succeed(), "reconcile all owners for the pod")
	f.assertOwners(3)
	for _, owner := range []*eipv1.EgressIP{f.one, f.two} {
		as, err := f.addressSets.GetAddressSet(f.destinationSetIDs(owner.Name))
		f.g.Expect(err).NotTo(gomega.HaveOccurred(), "get the owner destination address set")
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
		f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list owner reroute policies after reconciliation")
		f.g.Expect(policies).To(gomega.HaveLen(1))
		f.g.Expect(policies[0].Match).To(gomega.Equal(ownerFamily + ".src == " + ownerPodIP + " && " + want))
		if f.network.IsUserDefinedNetwork() && (f.topology == types.Layer3Topology || f.topology == types.Layer2Topology) {
			marks, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.gateway, func(p *nbdb.LogicalRouterPolicy) bool {
				name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
				return name == owner.Name && isEgressIPMarkPriority(p.Priority)
			})
			f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list owner gateway mark policies")
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
			f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list owner NAT rules")
			f.g.Expect(nats).To(gomega.HaveLen(1))
			f.g.Expect(nats[0].Match).To(gomega.Equal(want))
			f.g.Expect(nats[0].Priority).To(gomega.Equal(100))
		}
	}
	if f.network.IsDefault() {
		nats, err := libovsdbops.FindNATsWithPredicate(f.nb, func(n *nbdb.NAT) bool { return n.Type == nbdb.NATTypeSNAT })
		f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list cluster-default SNAT rules")
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
	f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(pendingCatchAll)).To(gomega.Succeed(), "update the cached EgressIP")
	applied, _ := f.controller.podAssignment.Load(getPodKey(f.pod))
	f.g.Expect(applied.activeEgressIPs[f.one.Name].traffic.selective()).To(gomega.BeTrue(), "retain the selective snapshot until reconciliation")
	// Retire stale applied traffic even while the pod's port cache entry is unavailable.
	f.portCache.remove(f.pod, f.nadKey)
	f.g.Expect(f.controller.addPodEgressIPAssignmentsWithLock(f.network, f.one.Name, f.one.Status.Items, getEgressIPPktMark(f.one.Name, f.one.Annotations), f.pod)).To(gomega.Succeed(), "apply the owner pod assignments")
	f.assertOwners(2)
	f.portCache.add(f.pod, "", f.nadKey, "", f.mac, f.podIPs)
	f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(f.one)).To(gomega.Succeed(), "update the cached EgressIP")
	f.g.Expect(f.controller.addPodEgressIPAssignmentsWithLock(f.network, f.one.Name, f.one.Status.Items, getEgressIPPktMark(f.one.Name, f.one.Annotations), f.pod)).To(gomega.Succeed(), "apply the owner pod assignments")
	f.assertOwners(3)
}

func (f *selectiveEgressFixture) testInvalidSelectors() {
	// Invalid legacy selectors withdraw the owner, including at startup.
	for _, selector := range []*metav1.LabelSelector{
		{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "class", Operator: "Invalid"}}}, {},
	} {
		invalid := f.one.DeepCopy()
		invalid.Spec.TrafficSelector = selector
		f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(invalid)).To(gomega.Succeed(), "update the cached EgressIP")
		f.g.Expect(f.controller.reconcileEgressIP(f.one, invalid)).To(gomega.Succeed(), "reconcile EgressIP assignments")
		f.assertOwners(2)
		f.restartNetwork()
		f.g.Expect(f.controller.reconcileTrafficClass(f.class)).To(gomega.Succeed(), "apply TrafficClass selection changes")
		as, err := f.addressSets.GetAddressSet(f.destinationSetIDs(f.one.Name))
		f.g.Expect(err).NotTo(gomega.HaveOccurred(), "get the destination set after resolution retry")
		v4, v6 := as.GetAddresses()
		f.g.Expect(append(v4, v6...)).To(gomega.BeEmpty())
		v4Name, v6Name := as.GetASHashNames()
		asName := v4Name
		if f.ipv6 {
			asName = v6Name
		}
		traffic, err := f.controller.egressTrafficMatchesFor(f.one.Name)
		match := traffic.match(f.ipv6)
		f.g.Expect(err).NotTo(gomega.HaveOccurred(), "get the resolved owner traffic match")
		f.g.Expect(match).To(gomega.Equal(fmt.Sprintf("(%s.dst == $%s)", f.family, asName)))
		f.assertOwners(2)
		state, _ := f.controller.podAssignment.Load(getPodKey(f.pod))
		f.g.Expect(state.activeEgressIPs).NotTo(gomega.HaveKey(f.one.Name))
		f.g.Expect(state.standbyEgressIPNames.Has(f.one.Name)).To(gomega.BeFalse(), "keep invalid selective owners out of the standby set")
		policies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(f.nb, func(p *nbdb.LogicalRouterPolicy) bool {
			name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
			return name == f.one.Name
		})
		f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list policies after invalid selection")
		f.g.Expect(policies).To(gomega.BeEmpty())
		nats, err := libovsdbops.FindNATsWithPredicate(f.nb, func(n *nbdb.NAT) bool {
			name, _ := getEIPLRPObjK8MetaData(n.ExternalIDs)
			return name == f.one.Name
		})
		f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list NATs after invalid selection")
		f.g.Expect(nats).To(gomega.BeEmpty())
		f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(f.one)).To(gomega.Succeed(), "update the cached EgressIP")
		f.g.Expect(f.controller.reconcileEgressIP(invalid, f.one)).To(gomega.Succeed(), "reconcile EgressIP assignments")
		f.assertOwners(3)
	}
}

func (f *selectiveEgressFixture) testFamilyChange() {
	f.restartNetwork()
	f.assertOwners(3)
	// Change the actual assignment family while both pod families are present.
	updated := f.one.DeepCopy()
	updated.Status.Items[0].EgressIP = "2001:db8:1::10"
	f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(updated)).To(gomega.Succeed(), "update the cached EgressIP")
	f.g.Expect(f.controller.reconcileEgressIP(f.one, updated)).To(gomega.Succeed(), "reconcile EgressIP assignments")
	policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool {
		name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
		return name == f.one.Name && isEgressIPReroutePriority(p.Priority)
	})
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list policies after owner update")
	f.g.Expect(policies).To(gomega.HaveLen(1))
	f.g.Expect(policies[0].Match).To(gomega.HavePrefix("ip6.src == " + f.podIPs[1].IP.String() + " && "))
	f.assertOwners(3)
	if f.network.IsDefault() {
		// Reassign through the real EgressIP handler to a remote egress node.
		remote := f.node.DeepCopy()
		remote.Name = "remote"
		remote.Annotations["k8s.ovn.org/node-transit-switch-port-ifaddr"] = `{"ipv4":"100.88.0.3/16","ipv6":"fd97::3/64"}`
		f.g.Expect(f.watchFactory.NodeCoreInformer().Informer().GetStore().Add(remote)).To(gomega.Succeed(), "cache the test node")
		f.g.Expect(f.controller.SyncNodeCache()).To(gomega.Succeed(), "populate the controller node cache")
		moved := updated.DeepCopy()
		moved.Status.Items[0].Node = remote.Name
		f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(moved)).To(gomega.Succeed(), "update the cached EgressIP")
		f.g.Expect(f.controller.reconcileEgressIP(updated, moved)).To(gomega.Succeed(), "reconcile EgressIP assignments")
		policies, err = libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool {
			name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
			return name == f.one.Name && isEgressIPReroutePriority(p.Priority)
		})
		f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list policies after owner reassignment")
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
	f.g.Expect(f.watchFactory.NodeCoreInformer().Informer().GetStore().Add(remote)).To(gomega.Succeed(), "cache the test node")
	f.g.Expect(f.controller.SyncNodeCache()).To(gomega.Succeed(), "populate the controller node cache")
	remotePod := f.pod.DeepCopy()
	remotePod.Spec.NodeName = remote.Name
	traffic, err := f.controller.egressTrafficMatchesFor(f.one.Name)
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "get traffic matches before moving the pod")
	f.g.Expect(f.controller.deletePodEgressIPAssignment(f.network, f.one.Name, f.one.Status.Items[0], f.pod)).To(gomega.Succeed(), "remove the pod assignment status")
	f.g.Expect(f.controller.addPodEgressIPAssignment(f.network, f.one.Name, f.one.Status.Items[0], getEgressIPPktMark(f.one.Name, f.one.Annotations), remotePod, f.podIPs, traffic)).To(gomega.Succeed(), "apply the pod assignment status")
	filtered, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool {
		name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
		return name == f.one.Name && p.Priority == types.EgressIPSelectiveReroutePriority
	})
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list policies after moving the pod")
	f.g.Expect(filtered).To(gomega.HaveLen(1))
	f.g.Expect(filtered[0].Nexthops).To(gomega.Equal([]string{strings.Split(f.joinIP, "/")[0]}))
	f.g.Expect(f.controller.deletePodEgressIPAssignment(f.network, f.one.Name, f.one.Status.Items[0], remotePod)).To(gomega.Succeed(), "remove the pod assignment status")
	f.g.Expect(f.controller.addPodEgressIPAssignment(f.network, f.one.Name, f.one.Status.Items[0], getEgressIPPktMark(f.one.Name, f.one.Annotations), f.pod, f.podIPs, traffic)).To(gomega.Succeed(), "apply the pod assignment status")
}

func (f *selectiveEgressFixture) testConflictRecovery() {
	// An overlap withdraws the losing owner's whole assignment and can recover.
	originalPeer := f.peerClass.DeepCopy()
	f.peerClass = f.peerClass.DeepCopy()
	f.peerClass.Spec.Matches = append(f.peerClass.Spec.Matches, trafficv1.TrafficMatch{DestinationCIDR: f.cidr})
	f.g.Expect(f.watchFactory.TrafficClassInformer().Informer().GetStore().Update(f.peerClass)).To(gomega.Succeed(), "update the cached TrafficClass")
	f.g.Expect(f.controller.reconcileTrafficClass(f.peerClass)).To(gomega.Succeed(), "apply TrafficClass selection changes")
	f.assertOwners(2)
	f.restartNetwork()
	f.assertOwners(2)
	f.g.Expect(f.watchFactory.TrafficClassInformer().Informer().GetStore().Update(originalPeer)).To(gomega.Succeed(), "update the cached TrafficClass")
	f.g.Expect(f.controller.reconcileTrafficClass(originalPeer)).To(gomega.Succeed(), "apply TrafficClass selection changes")
	f.assertOwners(3)
}

func (f *selectiveEgressFixture) testClassUpdates() {
	// L4 edits rebuild the owner; a peer's assignment survives.
	f.class = f.class.DeepCopy()
	f.class.Spec.Matches[1].Protocols[0].UDP.DestinationPort = &trafficv1.Port{Number: ptr.To[int32](5060)}
	f.g.Expect(f.watchFactory.TrafficClassInformer().Informer().GetStore().Update(f.class)).To(gomega.Succeed(), "update the cached TrafficClass")
	f.controller.addressSetFactory = &failingTrafficAddressSets{AddressSetFactory: f.addressSets, failAdd: true}
	f.g.Expect(f.controller.reconcileTrafficClass(f.class)).To(gomega.MatchError(gomega.ContainSubstring("injected served-pod address-set failure")))
	f.g.Expect(f.controller.reconcileTrafficClass(f.class)).To(gomega.Succeed(), "apply TrafficClass selection changes")
	f.controller.addressSetFactory = f.addressSets
	f.assertOwners(3)
	policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool {
		return strings.HasPrefix(p.ExternalIDs[libovsdbops.ObjectNameKey.String()], "one_")
	})
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list policies after class label change")
	f.g.Expect(policies[0].Match).To(gomega.ContainSubstring("udp.dst == 5060"))
	f.g.Expect(f.watchFactory.TrafficClassInformer().Informer().GetStore().Delete(f.class)).To(gomega.Succeed(), "remove the cached TrafficClass")
	f.g.Expect(f.controller.reconcileTrafficClass(f.class)).To(gomega.Succeed(), "apply TrafficClass selection changes")
	f.assertOwners(2)
	empty, err := f.addressSets.GetAddressSet(f.destinationSetIDs(f.one.Name))
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "get the empty destination set after class deletion")
	v4, v6 := empty.GetAddresses()
	f.g.Expect(append(v4, v6...)).To(gomega.BeEmpty())
	f.g.Expect(f.watchFactory.TrafficClassInformer().Informer().GetStore().Add(f.class)).To(gomega.Succeed(), "cache the test TrafficClass")
	f.g.Expect(f.controller.reconcileTrafficClass(f.class)).To(gomega.Succeed(), "apply TrafficClass selection changes")
	catchAll := f.one.DeepCopy()
	catchAll.Spec.TrafficSelector = nil
	f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(catchAll)).To(gomega.Succeed(), "update the cached EgressIP")
	f.g.Expect(f.controller.reconcileEgressIP(f.one, catchAll)).To(gomega.Succeed(), "reconcile EgressIP assignments")
	f.assertOwners(2)
	destinationSets, err := libovsdbops.FindAddressSetsWithPredicate(f.nb, func(as *nbdb.AddressSet) bool {
		return strings.HasPrefix(as.ExternalIDs[libovsdbops.ObjectNameKey.String()], egressIPDestinationNetworksPrefix+f.one.Name+"/")
	})
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "find destination sets after switching to catch-all")
	f.g.Expect(destinationSets).To(gomega.BeEmpty())
	f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(f.one)).To(gomega.Succeed(), "update the cached EgressIP")
	f.g.Expect(f.controller.reconcileEgressIP(catchAll, f.one)).To(gomega.Succeed(), "reconcile EgressIP assignments")
	f.assertOwners(3)
}

func (f *selectiveEgressFixture) testPodIPReplacement() {
	var err error
	// Rebuild all owners after an IP replacement.
	oldIPs := f.podIPs
	f.podIPs = ovntest.MustParseIPNets(f.podPrefix + "20" + f.podMask)
	f.pod.Annotations, err = util.MarshalPodAnnotation(nil, &util.PodAnnotation{IPs: f.podIPs, MAC: f.mac, Role: types.NetworkRolePrimary}, f.nadKey)
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "annotate changed pod IPs")
	f.pod.Status.PodIP = f.podIPs[0].IP.String()
	f.pod.Status.PodIPs = []corev1.PodIP{{IP: f.pod.Status.PodIP}}
	f.g.Expect(f.watchFactory.PodCoreInformer().Informer().GetStore().Update(f.pod)).To(gomega.Succeed(), "update the cached pod")
	f.portCache.add(f.pod, "", f.nadKey, "", f.mac, f.podIPs)
	ownersToUpdate := []*eipv1.EgressIP{f.fallback, f.one, f.two}
	for i, owner := range ownersToUpdate {
		f.g.Expect(f.controller.addPodEgressIPAssignmentsWithLock(f.network, owner.Name, owner.Status.Items, getEgressIPPktMark(owner.Name, owner.Annotations), f.pod)).To(gomega.Succeed(), "apply the owner pod assignments")
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
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "find catch-all policies before migration")
	f.g.Expect(legacyPolicies).To(gomega.HaveLen(1))
	legacy := *legacyPolicies[0]
	legacy.UUID, legacy.Priority = "", types.EgressIPLegacyReroutePriority
	legacyIDs, err := libovsdbops.NewDbObjectIDsFromExternalIDs(libovsdbops.LogicalRouterPolicyEgressIP, legacy.ExternalIDs)
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "decode legacy policy ownership IDs")
	legacyIDs = legacyIDs.AddIDs(map[libovsdbops.ExternalIDKey]string{libovsdbops.PriorityKey: fmt.Sprint(types.EgressIPLegacyReroutePriority)})
	legacy.ExternalIDs = legacyIDs.GetExternalIDs()
	ops, err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(f.nb, nil, f.router, &legacy, libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](legacyIDs, nil))
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "stage a legacy-priority policy")
	_, err = libovsdbops.TransactAndCheck(f.nb, ops)
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "create the legacy-priority policy")
	f.controller.podAssignment = syncmap.NewSyncMap[*podAssignmentState]()
	f.g.Expect(f.controller.migrateEgressIPReroutePriorities(egressIPAllNetworks)).To(gomega.Succeed(), "migrate legacy EgressIP policies")
	cache, err := f.controller.generateCacheForEgressIP(egressIPAllNetworks)
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "generate the restart repair cache")
	// Repair must recreate a missing mark even when all remaining marks are valid.
	f.g.Expect(libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(f.nb, f.gateway, func(p *nbdb.LogicalRouterPolicy) bool {
		name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
		return isEgressIPMarkPriority(p.Priority) && name == f.one.Name
	})).To(gomega.Succeed(), "remove a gateway mark to test startup repair")
	f.g.Expect(f.controller.syncStaleGWMarkRules(cache, egressIPAllNetworks)).To(gomega.Succeed(), "repair stale gateway mark policies")
	f.g.Expect(f.controller.syncStaleGWMarkRules(cache, egressIPAllNetworks)).To(gomega.Succeed(), "repair stale gateway mark policies")
	f.restartNetwork()
	f.assertOwners(3)
	// A peer's local assignment must not keep another owner's gateway mark alive.
	cache.egressIPNameToAssignedNodes[f.one.Name] = []string{"other-node"}
	cache.egressIPToAssignedNodes[f.one.Name] = map[string]string{f.one.Status.Items[0].EgressIP: "other-node"}
	f.g.Expect(f.controller.syncStaleGWMarkRules(cache, egressIPAllNetworks)).To(gomega.Succeed(), "repair stale gateway mark policies")
	f.assertGatewayMarks(2)
	cache, err = f.controller.generateCacheForEgressIP(egressIPAllNetworks)
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "regenerate the cache after clearing assignments")
	f.g.Expect(f.controller.syncStaleGWMarkRules(cache, egressIPAllNetworks)).To(gomega.Succeed(), "repair stale gateway mark policies")
	f.assertGatewayMarks(3)
	otherFamily := "2001:db8::10"
	if f.ipv6 {
		otherFamily = "198.51.100.10"
	}
	cache.egressIPToAssignedNodes[f.one.Name] = map[string]string{otherFamily: "node"}
	f.g.Expect(f.controller.syncStaleGWMarkRules(cache, egressIPAllNetworks)).To(gomega.Succeed(), "repair stale gateway mark policies")
	f.assertGatewayMarks(2)
	cache, err = f.controller.generateCacheForEgressIP(egressIPAllNetworks)
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "regenerate the cache after removing an owner")
	f.g.Expect(f.controller.syncStaleGWMarkRules(cache, egressIPAllNetworks)).To(gomega.Succeed(), "repair stale gateway mark policies")
	f.assertGatewayMarks(3)
}

func (f *selectiveEgressFixture) testGateDisableAndNetworkCleanup() {
	f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Delete(f.one)).To(gomega.Succeed(), "remove the cached EgressIP")
	f.g.Expect(f.controller.reconcileEgressIP(f.one, nil)).To(gomega.Succeed(), "reconcile EgressIP assignments")
	f.assertOwners(2)
	// With the gate off, selective peers withdraw without becoming catch-alls.
	config.OVNKubernetesFeature.EnableTrafficClass = false
	f.g.Expect(f.controller.syncEgressTraffic()).To(gomega.Succeed(), "resolve selective owners at startup")
	f.g.Expect(f.controller.migrateEgressIPReroutePriorities(egressIPAllNetworks)).To(gomega.Succeed(), "migrate legacy EgressIP policies")
	f.g.Expect(f.controller.addPodEgressIPAssignmentsWithLock(f.network, f.fallback.Name, f.fallback.Status.Items, getEgressIPPktMark(f.fallback.Name, f.fallback.Annotations), f.pod)).To(gomega.Succeed(), "apply the owner pod assignments")
	f.g.Expect(f.controller.addPodEgressIPAssignmentsWithLock(f.network, f.two.Name, f.two.Status.Items, getEgressIPPktMark(f.two.Name, f.two.Annotations), f.pod)).To(gomega.Succeed(), "apply the owner pod assignments")
	state, _ := f.controller.podAssignment.Load(getPodKey(f.pod))
	f.g.Expect(state.activeEgressIPs).To(gomega.HaveLen(1))
	f.g.Expect(state.standbyEgressIPNames).To(gomega.BeEmpty())
	remaining, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool { return isEgressIPReroutePriority(p.Priority) })
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list remaining catch-all policies with the gate disabled")
	f.g.Expect(remaining).To(gomega.HaveLen(1))
	f.g.Expect(remaining[0].Priority).To(gomega.Equal(99))
	f.g.Expect(remaining[0].Match).To(gomega.Equal(f.family + ".src == " + f.podIPs[0].IP.String()))
	// Restart reconstruction must include every namespace sharing a network.
	peerPod := f.pod.DeepCopy()
	peerPod.Namespace = "peer"
	f.networkManager.PrimaryNetworks[peerPod.Namespace] = f.network
	f.g.Expect(f.watchFactory.NamespaceCoreInformer().Informer().GetStore().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: peerPod.Namespace, Labels: map[string]string{corev1.LabelMetadataName: peerPod.Namespace}}})).To(gomega.Succeed(), "cache the test namespace")
	f.g.Expect(f.watchFactory.PodCoreInformer().Informer().GetStore().Add(peerPod)).To(gomega.Succeed(), "cache the test pod")
	f.portCache.add(peerPod, "", f.nadKey, "", f.mac, f.podIPs)
	cache, err := f.controller.generateCacheForEgressIP(egressIPAllNetworks)
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "generate the cache across namespaces sharing a network")
	f.g.Expect(cache.egressIPNameToPods[f.fallback.Name][f.network.GetNetworkName()].egressLocalPods).To(gomega.HaveLen(2))
	// Startup repair clears shared membership when owners select no pods.
	owners, err := f.watchFactory.GetEgressIPs()
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list owners before workload deselection")
	for _, owner := range owners {
		updated := owner.DeepCopy()
		updated.Spec.PodSelector = metav1.LabelSelector{MatchLabels: map[string]string{"missing": "pod"}}
		f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(updated)).To(gomega.Succeed(), "update the cached EgressIP")
	}
	f.controller.podAssignment = syncmap.NewSyncMap[*podAssignmentState]()
	f.restartNetwork()
	stale, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool { return isEgressIPReroutePriority(p.Priority) })
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "find stale policies after workload deselection")
	f.g.Expect(stale).To(gomega.BeEmpty())
	served, err := f.addressSets.GetAddressSet(getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, f.network.GetNetworkName(), f.controller.controllerName))
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "get the served-pod set after workload deselection")
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
		owner, _, _ := strings.Cut(strings.TrimPrefix(ids.GetObjectID(libovsdbops.ObjectNameKey), egressIPDestinationNetworksPrefix), "/")
		f.entered <- owner
		<-f.release
	}
	return f.AddressSetFactory.NewAddressSet(ids, addresses)
}

func TestSelectiveTrafficClassConcurrentEgressIPUpdate(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted=%t", deleted), func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "prepare test configuration")
			t.Cleanup(func() { _ = config.PrepareTestConfig() })
			config.OVNKubernetesFeature.EnableEgressIP = true
			config.OVNKubernetesFeature.EnableTrafficClass = true
			clients := util.GetOVNClientset().GetOVNKubeControllerClientset()
			wf, err := factory.NewOVNKubeControllerWatchFactory(clients, "node")
			g.Expect(err).NotTo(gomega.HaveOccurred(), "create the controller watch factory")
			t.Cleanup(wf.Shutdown)
			owners := map[string]*eipv1.EgressIP{}
			for _, name := range []string{"one", "two"} {
				owner := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"traffic": "old"}}}}
				owners[name] = owner
				g.Expect(wf.EgressIPInformer().Informer().GetStore().Add(owner)).To(gomega.Succeed(), "cache the test EgressIP")
			}
			class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "old", Labels: map[string]string{"traffic": "old"}}, Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: "192.0.2.0/24"}}}}
			newClass := class.DeepCopy()
			newClass.Name, newClass.Labels = "new", map[string]string{"traffic": "new"}
			newClass.Spec.Matches[0].DestinationCIDR = "203.0.113.0/24"
			for _, class := range []*trafficv1.TrafficClass{class, newClass} {
				g.Expect(wf.TrafficClassInformer().Informer().GetStore().Add(class)).To(gomega.Succeed(), "cache the test TrafficClass")
			}
			nb, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
			g.Expect(err).NotTo(gomega.HaveOccurred(), "create the northbound test database")
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
				g.Expect(wf.EgressIPInformer().Informer().GetStore().Delete(old)).To(gomega.Succeed(), "remove the cached EgressIP")
				g.Expect(e.reconcileEgressIP(old, nil)).To(gomega.Succeed(), "reconcile EgressIP assignments")
			} else {
				updated := old.DeepCopy()
				updated.Spec.TrafficSelector = &metav1.LabelSelector{MatchLabels: newClass.Labels}
				g.Expect(wf.EgressIPInformer().Informer().GetStore().Update(updated)).To(gomega.Succeed(), "update the cached EgressIP")
				g.Expect(e.reconcileEgressIP(old, updated)).To(gomega.Succeed(), "reconcile EgressIP assignments")
			}
			release.Do(func() { close(asf.release) })
			g.Eventually(done, 5*time.Second).Should(gomega.Receive(gomega.Succeed()))
			if deleted {
				rows, err := libovsdbops.FindAddressSetsWithPredicate(nb, func(as *nbdb.AddressSet) bool {
					setName := as.ExternalIDs[libovsdbops.ObjectNameKey.String()]
					return as.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == e.controllerName &&
						(setName == egressIPDestinationNetworksPrefix+name || strings.HasPrefix(setName, egressIPDestinationNetworksPrefix+name+"/"))
				})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "find the deleted owner destination sets after the race")
				g.Expect(rows).To(gomega.BeEmpty(), "deleted owner was recreated by stale class work")
				_, cached := e.trafficMatches.Load(name)
				g.Expect(cached).To(gomega.BeFalse(), "discard the retired owner traffic snapshot")
			} else {
				traffic, ok := e.trafficMatches.Load(name)
				g.Expect(ok).To(gomega.BeTrue(), "retain the current owner's staged set")
				ids := getEgressIPDestNetworksAddrSetDbIDs(traffic.(egressTrafficMatches).addressSetName, e.controllerName)
				as, err := asf.GetAddressSet(ids)
				g.Expect(err).NotTo(gomega.HaveOccurred(), "get the updated owner destination set after the race")
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
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "prepare test configuration")
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableEgressIP = true
	config.OVNKubernetesFeature.EnableTrafficClass = true
	clients := util.GetOVNClientset().GetOVNKubeControllerClientset()
	wf, err := factory.NewOVNKubeControllerWatchFactory(clients, "node")
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the controller watch factory")
	t.Cleanup(wf.Shutdown)
	nb, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{&nbdb.LogicalRouter{Name: "pending-network"}},
	}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the northbound test database")
	t.Cleanup(cleanup.Cleanup)
	asf := addressset.NewOvnAddressSetFactory(nb, true, false)
	ids := getEgressIPDestNetworksAddrSetDbIDs("deleted-owner", types.DefaultNetworkControllerName)
	as, err := asf.NewAddressSet(ids, []string{"203.0.113.0/24"})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the destination address set")
	v4, _ := as.GetASHashNames()
	policy := &nbdb.LogicalRouterPolicy{Priority: 100, Match: "ip4.dst == $" + v4, Action: nbdb.LogicalRouterPolicyActionAllow}
	ops, err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(nb, nil, "pending-network", policy, func(p *nbdb.LogicalRouterPolicy) bool {
		return p.Match == policy.Match
	})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "stage the pending network policy")
	_, err = libovsdbops.TransactAndCheck(nb, ops)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "publish the pending network policy")
	e := NewEIPController(nb, &kube.KubeOVN{EIPClient: clients.EgressIPClient}, wf, record.NewFakeRecorder(10), nil, &networkmanager.FakeNetworkManager{}, asf, nil, true, false, "node", types.DefaultNetworkControllerName)
	e.trafficMatches.Store("deleted-owner", egressTrafficMatches{v4: "ip4.dst == $" + v4})
	g.Expect(e.deleteEgressTraffic("deleted-owner")).To(gomega.Succeed(), "retire the deleted owner traffic matches")
	_, cached := e.trafficMatches.Load("deleted-owner")
	g.Expect(cached).To(gomega.BeFalse(), "discard the deleted owner traffic snapshot")
	g.Expect(e.pruneEgressTraffic("")).To(gomega.Succeed(), "prune unreferenced destination sets")
	sets, err := libovsdbops.FindAddressSetsWithPredicate(nb, func(a *nbdb.AddressSet) bool { return a.Name == v4 })
	g.Expect(err).NotTo(gomega.HaveOccurred(), "find the destination set while still referenced")
	g.Expect(sets).To(gomega.HaveLen(1))
	g.Expect(libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(nb, "pending-network", func(p *nbdb.LogicalRouterPolicy) bool {
		return p.Match == policy.Match
	})).To(gomega.Succeed(), "remove the pending network policy reference")
	g.Expect(e.pruneEgressTraffic("")).To(gomega.Succeed(), "prune unreferenced destination sets")
	sets, err = libovsdbops.FindAddressSetsWithPredicate(nb, func(a *nbdb.AddressSet) bool { return a.Name == v4 })
	g.Expect(err).NotTo(gomega.HaveOccurred(), "find the destination set after removing references")
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
	g.Expect(err).NotTo(gomega.HaveOccurred(), "find the owner policy before staging a nexthop")
	g.Expect(policies).To(gomega.HaveLen(1))
	policy := policies[0]
	// Keep one other hop so status cleanup mutates the existing row rather than deletes it.
	policy.Nexthops = []string{"100.64.0.2", "100.64.0.3"}
	ops, err := libovsdbops.UpdateLogicalRouterPoliciesOps(f.nb, nil, policy)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "stage the initial nexthops")
	_, err = libovsdbops.TransactAndCheck(f.nb, ops)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "publish the initial nexthops")
	traffic, err := f.controller.egressTrafficMatchesFor(f.one.Name)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "get owner traffic for nexthop insertion")
	ops, err = f.controller.createReroutePolicyOps(f.network, nil, f.podIPs, f.one.Status.Items[0], "", f.one.Name,
		"100.64.0.4", f.router, f.pod.Namespace, f.pod.Name, traffic.v4)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "stage insertion of a new nexthop")
	removeOps, err := libovsdbops.DeleteNextHopsFromLogicalRouterPolicyOps(f.nb, nil, f.router, policies, "100.64.0.2")
	g.Expect(err).NotTo(gomega.HaveOccurred(), "stage concurrent nexthop removal")
	_, err = libovsdbops.TransactAndCheck(f.nb, removeOps)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "publish concurrent nexthop removal")
	_, err = libovsdbops.TransactAndCheck(f.nb, ops)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "publish the staged nexthop insertion")
	g.Eventually(func() []string {
		current, lookupErr := libovsdbops.GetLogicalRouterPolicy(f.nb, &nbdb.LogicalRouterPolicy{UUID: policy.UUID})
		g.Expect(lookupErr).NotTo(gomega.HaveOccurred(), "read the policy after concurrent mutations")
		return current.Nexthops
	}, "5s").Should(gomega.ConsistOf("100.64.0.3", "100.64.0.4"))
}

func TestEgressTrafficMigrationSkipsUnchangedRows(t *testing.T) {
	f := newSelectiveEgressFixture(t, "default", "IPv4")
	f.testSelectionAndResolutionRetry()
	f.testProgramming()
	f.g.Expect(f.controller.migrateEgressIPReroutePriorities(egressIPAllNetworks)).To(gomega.Succeed(), "migrate legacy EgressIP policies")
	recording := libovsdbtest.NewRecordingClient(f.nb)
	f.controller.nbClient = recording
	f.g.Expect(f.controller.migrateEgressIPReroutePriorities(egressIPAllNetworks)).To(gomega.Succeed(), "migrate legacy EgressIP policies")
	f.g.Expect(recording.Transactions).To(gomega.BeEmpty(), "unchanged migration must not rewrite policy or NAT rows")
}

func TestNamespaceRetriesRequireSelectiveOwners(t *testing.T) {
	for _, selective := range []bool{false, true} {
		t.Run(fmt.Sprintf("selective=%t", selective), func(t *testing.T) {
			f := newSelectiveEgressFixture(t, "default", "IPv4")
			f.g.Expect(f.watchFactory.PodInformer().GetStore().Add(f.pod)).To(gomega.Succeed(), "cache the test pod")
			if !selective {
				for _, owner := range []*eipv1.EgressIP{f.one, f.two} {
					f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Delete(owner)).To(gomega.Succeed(), "remove the cached EgressIP")
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
			f.g.Expect(err).NotTo(gomega.HaveOccurred(), "get the pod namespace")
			f.g.Expect(f.controller.reconcileEgressIPNamespace(nil, namespace)).To(gomega.Succeed(), "reconcile namespace ownership changes")
			key, err := retry.GetResourceKey(f.pod)
			f.g.Expect(err).NotTo(gomega.HaveOccurred(), "get the pod retry key")
			f.g.Expect(retry.CheckRetryObj(key, f.controller.retryEgressIPPods)).To(gomega.Equal(selective))
		})
	}
}

func TestExternalSNATRestorePreservesPeerFamily(t *testing.T) {
	for _, tc := range []struct {
		name, removedIP, peerIP string
		wantSources             []string
	}{
		{"IPv6 peer", "198.51.100.10", "2001:db8:1::12", []string{"198.51.100.2", "2001:db8:1::12"}},
		{"IPv4 peer", "2001:db8:1::10", "198.51.100.12", []string{"198.51.100.12", "2001:db8:1::2"}},
		{"no peer", "198.51.100.10", "", []string{"198.51.100.2", "2001:db8:1::2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSelectiveEgressFixture(t, "default", "dual-stack")
			f.pod.Status.PodIPs = []corev1.PodIP{{IP: f.podIPs[0].IP.String()}, {IP: f.podIPs[1].IP.String()}}
			f.g.Expect(f.watchFactory.PodCoreInformer().Informer().GetStore().Update(f.pod)).To(gomega.Succeed(), "cache both pod IP families")
			config.Gateway.DisableSNATMultipleGWs = true
			f.one.Status.Items[0].EgressIP = tc.removedIP
			f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(f.one)).To(gomega.Succeed(), "cache the selective owner's family")
			f.g.Expect(f.controller.reconcileEgressIP(nil, f.one)).To(gomega.Succeed(), "program the selective owner")
			if tc.peerIP != "" {
				f.fallback.Status.Items[0].EgressIP = tc.peerIP
				f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(f.fallback)).To(gomega.Succeed(), "cache the catch-all peer family")
				f.g.Expect(f.controller.reconcileEgressIP(nil, f.fallback)).To(gomega.Succeed(), "program the catch-all peer")
			}
			f.g.Expect(f.controller.deletePodEgressIPAssignmentsUsingCache(f.one.Name, f.pod, false)).To(gomega.Succeed(), "withdraw the selective owner")
			nats, err := libovsdbops.GetRouterNATs(f.nb, &nbdb.LogicalRouter{Name: f.gateway})
			f.g.Expect(err).NotTo(gomega.HaveOccurred(), "read gateway SNAT rules after owner withdrawal")
			var sources []string
			for _, nat := range nats {
				f.g.Expect(nat.Type).To(gomega.Equal(nbdb.NATTypeSNAT), "retain only SNAT rules")
				sources = append(sources, nat.ExternalIP)
			}
			f.g.Expect(sources).To(gomega.ConsistOf(tc.wantSources), "restore node SNAT only for families without a catch-all peer")
		})
	}
}

type failingDestinationSetCleanup struct {
	addressset.AddressSetFactory
	attempts int
}

func (f *failingDestinationSetCleanup) DestroyAddressSet(ids *libovsdbops.DbObjectIDs) error {
	f.attempts++
	if f.attempts == 1 {
		return fmt.Errorf("injected destination-set deletion failure")
	}
	return f.AddressSetFactory.DestroyAddressSet(ids)
}

func TestPruneEgressTrafficContinuesAfterFailure(t *testing.T) {
	f := newSelectiveEgressFixture(t, "default", "IPv4")
	for _, name := range []string{"stale-one", "stale-two"} {
		_, err := f.addressSets.NewAddressSet(getEgressIPDestNetworksAddrSetDbIDs(name, f.controller.controllerName), []string{"203.0.113.0/24"})
		f.g.Expect(err).NotTo(gomega.HaveOccurred(), "create stale destination set %s", name)
	}
	failing := &failingDestinationSetCleanup{AddressSetFactory: f.addressSets}
	f.controller.addressSetFactory = failing
	f.g.Expect(f.controller.pruneEgressTraffic("")).To(gomega.MatchError(gomega.ContainSubstring("injected destination-set deletion failure")), "report the failed cleanup")
	f.g.Expect(failing.attempts).To(gomega.Equal(2), "attempt both stale sets despite the first failure")
	f.g.Expect(f.controller.pruneEgressTraffic("")).To(gomega.Succeed(), "retry the failed destination set")
	f.g.Expect(failing.attempts).To(gomega.Equal(3), "retry only the set that failed")
	remaining, err := libovsdbops.FindAddressSetsWithPredicate(f.nb, func(as *nbdb.AddressSet) bool {
		return strings.HasPrefix(as.ExternalIDs[libovsdbops.ObjectNameKey.String()], egressIPDestinationNetworksPrefix+"stale-")
	})
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list stale destination sets after retry")
	f.g.Expect(remaining).To(gomega.BeEmpty(), "remove every stale destination set after retry")
}

type failingEgressStatusClient struct {
	libovsdbclient.Client
	fail         bool
	transactions int
}

func (c *failingEgressStatusClient) Transact(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	c.transactions++
	if c.fail {
		c.fail = false
		return nil, fmt.Errorf("injected status deletion failure")
	}
	return c.Client.Transact(ctx, ops...)
}

func TestEgressStatusDeletionContinuesAfterFailure(t *testing.T) {
	f := newSelectiveEgressFixture(t, "default", "IPv4")
	remote := f.node.DeepCopy()
	remote.Name = "remote"
	remote.Annotations["k8s.ovn.org/node-transit-switch-port-ifaddr"] = `{"ipv4":"100.88.0.3/16"}`
	f.g.Expect(f.watchFactory.NodeCoreInformer().Informer().GetStore().Add(remote)).To(gomega.Succeed(), "cache the remote egress node")
	f.g.Expect(f.controller.SyncNodeCache()).To(gomega.Succeed(), "observe the remote egress node")
	updated := f.one.DeepCopy()
	updated.Status.Items = append(updated.Status.Items, eipv1.EgressIPStatusItem{Node: remote.Name, EgressIP: "198.51.100.13"})
	f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(updated)).To(gomega.Succeed(), "assign a second egress node")
	f.g.Expect(f.controller.reconcileEgressIP(nil, updated)).To(gomega.Succeed(), "program both egress statuses")
	state, _ := f.controller.podAssignment.Load(getPodKey(f.pod))
	assignment := state.activeEgressIPs[f.one.Name]
	f.g.Expect(assignment.egressStatuses.statusMap).To(gomega.HaveLen(2), "apply both statuses before deletion")
	client := &failingEgressStatusClient{Client: f.nb, fail: true}
	f.controller.nbClient = client
	remove := func() error {
		return f.controller.podAssignment.DoWithLock(getPodKey(f.pod), func(string) error {
			return f.controller.deletePodEgressIPAssignmentStatuses(f.network, updated.Name, updated.Status.Items, f.pod, assignment)
		})
	}
	f.g.Expect(remove()).To(gomega.MatchError(gomega.ContainSubstring("injected status deletion failure")), "report the failed status while cleaning the independent one")
	f.g.Expect(client.transactions).To(gomega.Equal(2), "attempt both statuses despite the first failure")
	f.g.Expect(assignment.egressStatuses.statusMap).To(gomega.Equal(statusMap{updated.Status.Items[0]: ""}), "retain only the failed status for retry")
	policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool {
		name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
		return name == updated.Name && isEgressIPReroutePriority(p.Priority)
	})
	f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list the owner policies after partial cleanup")
	f.g.Expect(policies).To(gomega.HaveLen(1), "retain the failed status policy")
	f.g.Expect(policies[0].Nexthops).To(gomega.ConsistOf("100.64.0.2"), "remove the successful remote status nexthop")
	f.g.Expect(remove()).To(gomega.Succeed(), "retry the failed status")
	f.g.Expect(client.transactions).To(gomega.Equal(3), "retry only the status that failed")
	f.g.Expect(assignment.egressStatuses.statusMap).To(gomega.BeEmpty(), "clear statuses after successful retry")
}

func TestEgressRestartAssignmentFamilyChange(t *testing.T) {
	for _, topology := range []string{"default", types.Layer2Topology, types.Layer3Topology} {
		t.Run(topology, func(t *testing.T) {
			f := newSelectiveEgressFixture(t, topology, "dual-stack")
			pod := f.pod.DeepCopy()
			pod.Status.PodIPs = []corev1.PodIP{{IP: f.podIPs[0].IP.String()}, {IP: f.podIPs[1].IP.String()}}
			f.g.Expect(f.watchFactory.PodCoreInformer().Informer().GetStore().Update(pod)).To(gomega.Succeed(), "publish both pod address families")
			f.g.Expect(f.controller.reconcileEgressIPPod(nil, pod)).To(gomega.Succeed(), "program the original owners")
			peers, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool {
				name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
				return name != f.one.Name && isEgressIPReroutePriority(p.Priority)
			})
			f.g.Expect(err).NotTo(gomega.HaveOccurred(), "snapshot unaffected peer policies")
			f.g.Expect(peers).To(gomega.HaveLen(2), "program both selective and catch-all peers")
			for _, assignment := range []struct {
				address string
				podIP   net.IP
			}{
				{address: "2001:db8:1::10", podIP: f.podIPs[1].IP},
				{address: "198.51.100.10", podIP: f.podIPs[0].IP},
			} {
				updated := f.one.DeepCopy()
				updated.Status.Items[0].EgressIP = assignment.address
				// No EgressIP event: startup sees only the latest assignment on the same node.
				f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(updated)).To(gomega.Succeed(), "change the assigned family while the controller is down")
				f.controller.podAssignment = syncmap.NewSyncMap[*podAssignmentState]()
				f.restartNetwork()
				f.g.Expect(f.controller.reconcileEgressIPPod(nil, pod)).To(gomega.Succeed(), "program the current assignment after startup repair")
				policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(f.nb, f.router, func(p *nbdb.LogicalRouterPolicy) bool {
					return isEgressIPReroutePriority(p.Priority)
				})
				f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list reroute policies after the family change")
				f.g.Expect(policies).To(gomega.HaveLen(3), "replace the stale family without adding a fourth policy")
				for _, peer := range peers {
					f.g.Expect(policies).To(gomega.ContainElement(peer), "preserve the peer policy, including its UUID")
				}
				for _, policy := range policies {
					name, _ := getEIPLRPObjK8MetaData(policy.ExternalIDs)
					if name == f.one.Name {
						f.g.Expect(egressPodIPFromMatch(policy.Match).String()).To(gomega.Equal(assignment.podIP.String()), "retain only the currently assigned family")
					}
				}
			}
		})
	}
}

func TestEgressRestartReassignedSNATAddress(t *testing.T) {
	for _, family := range []string{"IPv4", "IPv6"} {
		t.Run(family, func(t *testing.T) {
			f := newSelectiveEgressFixture(t, "default", family)
			f.g.Expect(f.controller.reconcileEgressIPPod(nil, f.pod)).To(gomega.Succeed(), "program all owners before restart")
			oldAddress := f.one.Status.Items[0].EgressIP
			updated := f.one.DeepCopy()
			updated.Status.Items[0].EgressIP = "198.51.100.99"
			if f.ipv6 {
				updated.Status.Items[0].EgressIP = "2001:db8:1::99"
			}
			peer := f.two.DeepCopy()
			peer.Status.Items[0].EgressIP = oldAddress
			for _, owner := range []*eipv1.EgressIP{updated, peer} {
				f.g.Expect(f.watchFactory.EgressIPInformer().Informer().GetStore().Update(owner)).To(gomega.Succeed(), "reassign addresses on the same node while down")
			}
			f.controller.podAssignment = syncmap.NewSyncMap[*podAssignmentState]()
			f.restartNetwork()
			f.g.Expect(f.controller.reconcileEgressIPPod(nil, f.pod)).To(gomega.Succeed(), "program current assignments after restart")
			nats, err := libovsdbops.FindNATsWithPredicate(f.nb, func(n *nbdb.NAT) bool { return n.Type == nbdb.NATTypeSNAT })
			f.g.Expect(err).NotTo(gomega.HaveOccurred(), "read NATs after startup repair")
			f.g.Expect(nats).To(gomega.HaveLen(3), "retain exactly one NAT per owner")
			addresses := map[string]string{updated.Name: updated.Status.Items[0].EgressIP, peer.Name: oldAddress, f.fallback.Name: f.fallback.Status.Items[0].EgressIP}
			for _, nat := range nats {
				owner, _ := getEIPLRPObjK8MetaData(nat.ExternalIDs)
				f.g.Expect(nat.ExternalIP).To(gomega.Equal(addresses[owner]), "NAT address must belong to %s", owner)
			}
		})
	}
}

// Reject cleanup transactions while allowing a replacement destination set to be staged.
type blockedTrafficWithdrawal struct {
	libovsdbclient.Client
	fail bool
}

func (c *blockedTrafficWithdrawal) Transact(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	for _, op := range ops {
		if c.fail && op.Op == ovsdb.OperationDelete && (op.Table == "NAT" || op.Table == "Logical_Router_Policy") {
			return nil, fmt.Errorf("injected owner withdrawal failure")
		}
	}
	return c.Client.Transact(ctx, ops...)
}

func TestSelectiveCIDREditPreservesLiveMatchesUntilWithdrawal(t *testing.T) {
	for _, family := range []string{"IPv4", "IPv6"} {
		for _, startup := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/startup=%t", family, startup), func(t *testing.T) {
				f := newSelectiveEgressFixture(t, "default", family)
				// Start with disjoint CIDR-only sets referenced by both owners' live rules.
				oldCIDR := "198.18.0.0/24"
				if f.ipv6 {
					oldCIDR = "2001:db8:ffff::/64"
				}
				original := f.peerClass.DeepCopy()
				original.Spec.Matches = []trafficv1.TrafficMatch{{DestinationCIDR: oldCIDR}}
				f.g.Expect(f.watchFactory.TrafficClassInformer().Informer().GetStore().Update(original)).To(gomega.Succeed(), "cache disjoint destination CIDRs")
				f.g.Expect(f.controller.reconcileTrafficClass(original)).To(gomega.Succeed(), "prepare the disjoint peer")
				f.g.Expect(f.controller.reconcileEgressIPPod(nil, f.pod)).To(gomega.Succeed(), "program all three owners")
				oldTraffic, err := f.controller.egressTrafficMatchesFor(f.two.Name)
				f.g.Expect(err).NotTo(gomega.HaveOccurred(), "capture the live peer match")
				oldIDs := f.destinationSetIDs(f.two.Name)
				oldSet, err := f.addressSets.GetAddressSet(oldIDs)
				f.g.Expect(err).NotTo(gomega.HaveOccurred(), "capture the live peer destinations")
				policies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(f.nb, func(p *nbdb.LogicalRouterPolicy) bool {
					owner, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
					return owner == f.two.Name
				})
				f.g.Expect(err).NotTo(gomega.HaveOccurred(), "capture live peer policies")
				nats, err := libovsdbops.FindNATsWithPredicate(f.nb, func(n *nbdb.NAT) bool {
					owner, _ := getEIPLRPObjK8MetaData(n.ExternalIDs)
					return owner == f.two.Name
				})
				f.g.Expect(err).NotTo(gomega.HaveOccurred(), "capture live peer NATs")
				f.g.Expect(policies).NotTo(gomega.BeEmpty(), "exercise a live peer policy")
				f.g.Expect(nats).NotTo(gomega.BeEmpty(), "exercise live peer SNAT")
				overlapping := original.DeepCopy()
				overlapping.Spec.Matches = []trafficv1.TrafficMatch{{DestinationCIDR: f.cidr}}
				f.g.Expect(f.watchFactory.TrafficClassInformer().Informer().GetStore().Update(overlapping)).To(gomega.Succeed(), "make the peer overlap the older owner")
				failure := &blockedTrafficWithdrawal{Client: f.nb, fail: true}
				f.controller.nbClient = failure
				if startup {
					f.controller.lock.Lock()
					err = f.controller.syncNetworkStaleEntries(f.network.GetNetworkName())
					f.controller.lock.Unlock()
				} else {
					err = f.controller.reconcileTrafficClass(overlapping)
				}
				f.g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("injected owner withdrawal failure")), "hold the losing owner's live rules through a failed withdrawal")
				v4, v6 := oldSet.GetAddresses()
				f.g.Expect(append(v4, v6...)).To(gomega.Equal([]string{oldCIDR}), "live rules must retain the disjoint CIDR throughout the failed update")
				for _, p := range policies {
					current := &nbdb.LogicalRouterPolicy{UUID: p.UUID}
					f.g.Expect(f.nb.Get(context.Background(), current)).To(gomega.Succeed(), "retain the blocked peer policy")
					f.g.Expect(current.Match).To(gomega.Equal(p.Match), "do not rebind a blocked policy to overlapping destinations")
				}
				for _, n := range nats {
					current := &nbdb.NAT{UUID: n.UUID}
					f.g.Expect(f.nb.Get(context.Background(), current)).To(gomega.Succeed(), "retain blocked peer SNAT")
					f.g.Expect(current.Match).To(gomega.Equal(n.Match), "do not rebind blocked SNAT to overlapping destinations")
				}
				staged, err := f.controller.egressTrafficMatchesFor(f.two.Name)
				f.g.Expect(err).NotTo(gomega.HaveOccurred(), "read staged peer matches")
				f.g.Expect(staged).NotTo(gomega.Equal(oldTraffic), "stage a distinct destination-set identity")
				failure.fail = false
				f.g.Expect(f.controller.reconcileTrafficClass(overlapping)).To(gomega.Succeed(), "complete conflict cleanup on retry")
				f.assertOwners(2)
				retired, err := libovsdbops.FindAddressSetsWithPredicate(f.nb, libovsdbops.GetPredicate[*nbdb.AddressSet](oldIDs, nil))
				f.g.Expect(err).NotTo(gomega.HaveOccurred(), "list the retired destination set")
				f.g.Expect(retired).To(gomega.BeEmpty(), "prune the old version after its live references are withdrawn")
			})
		}
	}
}
