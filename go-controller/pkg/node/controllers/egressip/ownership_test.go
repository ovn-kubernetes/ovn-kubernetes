// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/knftables"

	ovnconfig "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	eipfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	eipinformers "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/informers/externalversions"
	eipinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/informers/externalversions/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1"
	trafficfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1/apis/clientset/versioned/fake"
	trafficinformers "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1/apis/informers/externalversions"
	trafficinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1/apis/informers/externalversions/trafficclass/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/linkmanager"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/egressip"
)

type ownershipTestFixture struct {
	ipv6          bool
	controller    *Controller
	client        *fake.Clientset
	eipClient     *eipfake.Clientset
	trafficClient *trafficfake.Clientset
	core          coreinformers.Interface
	eips          eipinformer.EgressIPInformer
	classes       trafficinformer.TrafficClassInformer
	nft           *knftables.Fake
}

func newOwnershipTestFixture(t *testing.T) *ownershipTestFixture {
	t.Helper()
	return newOwnershipTestFixtureForFamily(t, false)
}

func newOwnershipTestFixtureForFamily(t *testing.T, ipv6 bool) *ownershipTestFixture {
	t.Helper()
	g := gomega.NewWithT(t)
	g.Expect(ovnconfig.PrepareTestConfig()).To(gomega.Succeed(), "prepare test configuration")
	t.Cleanup(func() { _ = ovnconfig.PrepareTestConfig() })
	ovnconfig.OVNKubernetesFeature.EnableTrafficClass = true
	clusterPrefix := "10.128.0.0/14"
	if ipv6 {
		clusterPrefix = "fd00:10:244::/48"
	}
	_, clusterCIDR, _ := net.ParseCIDR(clusterPrefix)
	ovnconfig.Default.ClusterSubnets = []ovnconfig.CIDRNetworkEntry{{CIDR: clusterCIDR}}
	f := &ownershipTestFixture{ipv6: ipv6, client: fake.NewSimpleClientset(), eipClient: eipfake.NewSimpleClientset(), trafficClient: trafficfake.NewSimpleClientset()}
	f.core = informers.NewSharedInformerFactory(f.client, 0).Core().V1()
	f.eips = eipinformers.NewSharedInformerFactory(f.eipClient, 0).K8s().V1().EgressIPs()
	f.classes = trafficinformers.NewSharedInformerFactory(f.trafficClient, 0).K8s().V1().TrafficClasses()
	f.nft = nodenft.SetFakeNFTablesHelper()
	f.controller = f.newController(t, g)
	return f
}

func (f *ownershipTestFixture) newController(t *testing.T, g gomega.Gomega) *Controller {
	t.Helper()
	c, err := NewController(&kube.Kube{KClient: f.client}, f.eips, f.classes, f.core.Nodes().Informer(), f.core.Namespaces(), f.core.Pods(),
		func(string) (util.NetInfo, error) { return &util.DefaultNetInfo{}, nil }, routemanager.NewController(), !f.ipv6, f.ipv6, node1Name, linkmanager.NewController(node1Name, !f.ipv6, f.ipv6, nil))
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the host EgressIP controller")
	g.Expect(c.initNFTables()).To(gomega.Succeed(), "initialize EgressIP nftables chains")
	t.Cleanup(func() {
		c.eIPQueue.ShutDown()
		c.podQueue.ShutDown()
		c.namespaceQueue.ShutDown()
	})
	return c
}

// Return the first assertion failure to the caller instead of exiting the
// namespace goroutine, which would make NetNS.Do report success.
func assertInNetNS(netns ns.NetNS, run func(gomega.Gomega) error) error {
	type assertionFailure struct{ error }
	return netns.Do(func(ns.NetNS) (err error) {
		defer func() {
			if value := recover(); value != nil {
				failure, ok := value.(assertionFailure)
				if !ok {
					panic(value)
				}
				err = failure.error
			}
		}()
		return run(gomega.NewGomega(func(message string, _ ...int) {
			panic(assertionFailure{errors.New(message)})
		}))
	})
}

func TestCatchAllEventsDoNotQueueSelectivePeers(t *testing.T) {
	f := newOwnershipTestFixture(t)
	c := f.controller
	g := gomega.NewWithT(t)
	catchAll := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "catch-all", Generation: 1}}
	peer := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "peer"}, Spec: eipv1.EgressIPSpec{
		TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"class": "peer"}},
	}}
	g.Expect(f.eips.Informer().GetStore().Add(peer)).To(gomega.Succeed(), "cache an unrelated selective owner")
	updated := catchAll.DeepCopy()
	updated.Generation++
	updated.Spec.EgressIPs = []string{"192.0.2.1"}
	check := func(event func(), want ...string) {
		t.Helper()
		event()
		var queued []string
		for c.eIPQueue.Len() != 0 {
			name, _ := c.eIPQueue.Get()
			queued = append(queued, name)
			c.eIPQueue.Done(name)
		}
		g.Expect(queued).To(gomega.ConsistOf(want), "queue only affected owners")
	}
	check(func() { c.onEIPAdd(catchAll) }, catchAll.Name)
	check(func() { c.onEIPUpdate(catchAll, updated) }, catchAll.Name)
	check(func() { c.onEIPDelete(cache.DeletedFinalStateUnknown{Key: catchAll.Name, Obj: catchAll}) }, catchAll.Name)
	selective := updated.DeepCopy()
	selective.Generation++
	selective.Spec.TrafficSelector = peer.Spec.TrafficSelector.DeepCopy()
	check(func() { c.onEIPUpdate(updated, selective) }, catchAll.Name, peer.Name)
	removed := selective.DeepCopy()
	removed.Generation++
	removed.Spec.TrafficSelector = nil
	check(func() { c.onEIPUpdate(selective, removed) }, catchAll.Name, peer.Name)
}

type rejectedMaxPortRuleManager struct {
	iprulemanager.Interface
	rejected int
}

func (m *rejectedMaxPortRuleManager) Add(rule iprulemanager.IPRule) error {
	if rule.Sport.End == 65535 || rule.Dport.End == 65535 {
		m.rejected++
		return unix.EINVAL
	}
	return m.Interface.Add(rule)
}

func TestRejectedHostPortEndpointLeavesMatchUnprogrammed(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	f, netns, _, classes := setupSelectiveHostOwners(t)
	c := f.controller
	g := gomega.NewWithT(t)
	c.kube = &nodeAnnotationInformer{Interface: c.kube, client: f.client, nodes: f.core.Nodes().Informer().GetStore()}
	rules := &rejectedMaxPortRuleManager{Interface: c.ruleManager}
	c.ruleManager = rules
	class := classes["one"].DeepCopy()
	first, last := int32(80), int32(65535)
	class.Spec.Matches[0].Protocols = []trafficv1.ProtocolMatch{
		{TCP: &trafficv1.PortProtocol{SourcePort: &trafficv1.Port{Number: &last}}},
		{UDP: &trafficv1.PortProtocol{DestinationPort: &trafficv1.Port{Number: &first}}},
	}
	g.Expect(f.classes.Informer().GetStore().Update(class)).To(gomega.Succeed(), "cache supported and unsupported matches")
	g.Expect(assertInNetNS(netns, func(g gomega.Gomega) error {
		g.Expect(c.syncEIP("one")).To(gomega.Succeed(), "leave the unsupported match unapplied and install the supported match")
		installed, err := netlink.RuleListFiltered(netlink.FAMILY_V4, &netlink.Rule{Priority: ruleTrafficPriority}, netlink.RT_FILTER_PRIORITY)
		if err != nil {
			return err
		}
		g.Expect(installed).To(gomega.HaveLen(1), "install only the rule with a supported port")
		g.Expect(installed[0].IPProto).To(gomega.Equal(unix.IPPROTO_UDP), "preserve the supported protocol and port")
		g.Expect(installed[0].Dport).To(gomega.Equal(netlink.NewRulePortRange(80, 80)), "retain the exact destination port")
		g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("65535"), "avoid publishing SNAT for rejected steering")
		g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("udp dport 80"), "publish the supported match")
		return nil
	})).To(gomega.Succeed(), "inspect rules in the test network namespace")
	g.Expect(rules.rejected).To(gomega.Equal(1), "attempt the rule once without a workqueue retry")
}

func TestConcurrentSharedHostResources(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	f := newOwnershipTestFixture(t)
	c := f.controller
	initial := nodeConfig{linkConfigs: []linkConfig{{linkName: "dummy1", address: []address{{cidr: "5.5.5.10/24"}}}}}
	netns, cleanup, err := setupFakeTestNode(initial)
	if err != nil {
		t.Fatalf("set up fake node: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("clean up fake node network namespace: %v", err)
		}
	})
	var route netlink.Route
	rule := iprulemanager.IPRule{Priority: rulePriority, Table: 1007, Family: netlink.FAMILY_V4, Src: netip.MustParsePrefix("10.128.0.2/32")}
	err = netns.Do(func(ns.NetNS) error {
		link, err := netlink.LinkByName("dummy1")
		if err != nil {
			return err
		}
		route = netlink.Route{LinkIndex: link.Attrs().Index, Table: util.CalculateRouteTableID(link.Attrs().Index), Gw: net.ParseIP("5.5.5.254")}
		return nil
	})
	if err != nil {
		t.Fatalf("read route interface: %v", err)
	}
	const owners = 16
	runOwners := func(add bool, count int) {
		t.Helper()
		start := make(chan struct{})
		results := make(chan error, count)
		var workers sync.WaitGroup
		defer workers.Wait()
		for i := 0; i < count; i++ {
			workers.Add(1)
			go func(owner string) {
				defer workers.Done()
				<-start
				results <- netns.Do(func(ns.NetNS) error {
					if add {
						// Repeating the same add must not create another reference.
						for attempt := 0; attempt < 2; attempt++ {
							if err := c.addOwnedIPRule(owner, rule); err != nil {
								return err
							}
							if err := c.addOwnedRoute(owner, route); err != nil {
								return err
							}
						}
						return nil
					}
					if err := c.deleteOwnedIPRule(owner, rule); err != nil {
						return err
					}
					return c.deleteOwnedRoute(owner, route)
				})
			}(fmt.Sprint(i))
		}
		close(start)
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for i := 0; i < count; i++ {
			select {
			case err := <-results:
				if err != nil {
					t.Fatalf("reconcile shared resources concurrently: %v", err)
				}
			case <-deadline.C:
				t.Fatal("timed out reconciling shared resources concurrently")
			}
		}
	}
	check := func(present bool) {
		t.Helper()
		err := netns.Do(func(ns.NetNS) error {
			rules, err := netlink.RuleList(netlink.FAMILY_V4)
			if err != nil {
				return err
			}
			count := 0
			for _, current := range rules {
				if iprulemanager.IPRuleFromNetlinkRule(&current) == rule {
					count++
				}
			}
			want := 0
			if present {
				want = 1
			}
			if count != want {
				return fmt.Errorf("rule count %d, want %d", count, want)
			}
			routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: route.Table}, netlink.RT_FILTER_TABLE)
			if err != nil {
				return err
			}
			if len(routes) != want {
				return fmt.Errorf("route count %d, want %d", len(routes), want)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("check shared rules and routes: %v", err)
		}
	}
	runOwners(true, owners)
	check(true)
	runOwners(false, owners-1)
	check(true)
	err = netns.Do(func(ns.NetNS) error {
		if err := c.deleteOwnedIPRule(fmt.Sprint(owners-1), rule); err != nil {
			return err
		}
		return c.deleteOwnedRoute(fmt.Sprint(owners-1), route)
	})
	if err != nil {
		t.Fatalf("release final owner: %v", err)
	}
	check(false)
}

func TestOwnerSNATPublication(t *testing.T) {
	f := newOwnershipTestFixture(t)
	c := f.controller
	states := make(map[string]*state)
	for i, owner := range []string{"one", "two"} {
		applied := newState()
		configs := generatePodConfig([]net.IP{net.ParseIP("10.128.0.2")}, &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy1", Index: 7}},
			net.ParseIP(fmt.Sprintf("5.5.5.%d", 100+i)), false, []egressip.Destination{{CIDR: netip.MustParsePrefix(fmt.Sprintf("203.0.%d.0/24", 113+i))}}, true)
		for _, config := range configs.elems {
			config.snatReady = true
		}
		applied.namespacesWithPodIPConfigs["work"] = map[ktypes.NamespacedName]*podIPConfigList{{Namespace: "work", Name: "pod"}: configs}
		states[owner] = applied
	}
	publish := func(owner string, applied *state) error {
		return c.cache.DoWithLock(owner, func(string) error { return c.syncSelectiveSNAT(owner, applied) })
	}
	start := make(chan struct{})
	results := make(chan error, len(states))
	var publishers sync.WaitGroup
	defer publishers.Wait()
	for owner, applied := range states {
		publishers.Go(func() { <-start; results <- publish(owner, applied) })
	}
	close(start)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	var publishErr error
	for range states {
		select {
		case err := <-results:
			publishErr = errors.Join(publishErr, err)
		case <-deadline.C:
			t.Fatal("timed out publishing concurrent SNAT owners")
		}
	}
	if publishErr != nil {
		t.Fatalf("publish concurrent SNAT owners: %v", publishErr)
	}
	check := func(one, two bool) {
		t.Helper()
		dump := f.nft.Dump()
		if strings.Contains(dump, "snat ip to 5.5.5.100") != one || strings.Contains(dump, "snat ip to 5.5.5.101") != two {
			t.Fatalf("wrong owner contributions:\n%s", dump)
		}
	}
	check(true, true)
	lastTransaction := f.nft.LastTransaction
	if err := publish("not-hosted", nil); err != nil {
		t.Fatalf("publish absent owner: %v", err)
	}
	if f.nft.LastTransaction != lastTransaction {
		t.Fatal("absent owner rewrote selective SNAT rules")
	}
	// A published contribution must not alias its owner's mutable pod configuration.
	config := states["one"].namespacesWithPodIPConfigs["work"][ktypes.NamespacedName{Namespace: "work", Name: "pod"}].elems[0]
	config.snatValue = "5.5.5.102"
	if err := publish("two", states["two"]); err != nil {
		t.Fatalf("republish peer SNAT: %v", err)
	}
	check(true, true)
	if f.nft.LastTransaction != lastTransaction {
		t.Fatal("unchanged owner rewrote selective SNAT rules")
	}
	peerConfig := states["two"].namespacesWithPodIPConfigs["work"][ktypes.NamespacedName{Namespace: "work", Name: "pod"}].elems[0]
	peerConfig.snatValue = "5.5.5.103"
	if err := publish("two", states["two"]); err != nil {
		t.Fatalf("publish changed peer SNAT: %v", err)
	}
	rules, err := f.nft.ListRules(context.Background(), selectiveSNATChain)
	g := gomega.NewWithT(t)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "list the selective SNAT rules after the peer rewrite")
	g.Expect(rules).To(gomega.ConsistOf(
		gomega.HaveField("Rule", gomega.HaveSuffix(" snat ip to 5.5.5.100")),
		gomega.HaveField("Rule", gomega.HaveSuffix(" snat ip to 5.5.5.103")),
	), "replace the old peer rule and preserve the published owner contribution")
	peerConfig.snatValue = "5.5.5.101"
	if err := publish("two", states["two"]); err != nil {
		t.Fatalf("restore peer SNAT: %v", err)
	}
	if err := publish("one", nil); err != nil {
		t.Fatalf("withdraw owner SNAT: %v", err)
	}
	check(false, true)
	// Losing the table makes publication fail. The next owner must retry the complete union.
	tx := f.nft.NewTransaction()
	tx.Delete(&knftables.Table{})
	if err := f.nft.Run(context.Background(), tx); err != nil {
		t.Fatalf("delete nftables table: %v", err)
	}
	if err := publish("one", states["one"]); err == nil {
		t.Fatal("expected publication to fail without its table")
	}
	tx = f.nft.NewTransaction()
	tx.Add(&knftables.Table{})
	if err := f.nft.Run(context.Background(), tx); err != nil {
		t.Fatalf("recreate nftables table: %v", err)
	}
	if err := c.initNFTables(); err != nil {
		t.Fatalf("initialize nftables: %v", err)
	}
	if err := publish("two", states["two"]); err != nil {
		t.Fatalf("retry owner SNAT publication: %v", err)
	}
	if dump := f.nft.Dump(); !strings.Contains(dump, "snat ip to 5.5.5.102") || !strings.Contains(dump, "snat ip to 5.5.5.101") {
		t.Fatalf("failed contribution was not retried:\n%s", dump)
	}
}

// Deliver annotation writes to the informer without running asynchronous watch workers.
type nodeAnnotationInformer struct {
	kube.Interface
	client *fake.Clientset
	nodes  cache.Store
}

func (k *nodeAnnotationInformer) PatchNodeStatusAnnotations(ctx context.Context, oldNode, newNode *corev1.Node) error {
	if err := k.Interface.PatchNodeStatusAnnotations(ctx, oldNode, newNode); err != nil {
		return err
	}
	updated, err := k.client.CoreV1().Nodes().Get(ctx, newNode.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	return k.nodes.Update(updated)
}

// Pause after the API write and informer update, outside the fake client's lock.
// A second owner's reconciliation must finish before the first one resumes.
type pausedNodeAnnotation struct {
	*nodeAnnotationInformer
	entered, release chan struct{}
	paused           atomic.Bool
}

func (k *pausedNodeAnnotation) PatchNodeStatusAnnotations(ctx context.Context, oldNode, newNode *corev1.Node) error {
	if err := k.nodeAnnotationInformer.PatchNodeStatusAnnotations(ctx, oldNode, newNode); err != nil {
		return err
	}
	if k.paused.CompareAndSwap(false, true) {
		close(k.entered)
		<-k.release
	}
	return nil
}

func setupSelectiveHostOwners(t *testing.T) (*ownershipTestFixture, ns.NetNS, map[string]*eipv1.EgressIP, map[string]*trafficv1.TrafficClass) {
	t.Helper()
	f := newOwnershipTestFixture(t)
	g := gomega.NewWithT(t)
	initial := nodeConfig{linkConfigs: []linkConfig{{linkName: "dummy1", address: []address{{cidr: "5.5.5.10/24"}}}}}
	netns, cleanup, err := setupFakeTestNode(initial)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "set up the fake node network namespace")
	t.Cleanup(func() { g.Expect(cleanup()).To(gomega.Succeed(), "clean up the fake node network namespace") })
	err = netns.Do(func(ns.NetNS) error {
		link, err := netlink.LinkByName("dummy1")
		if err != nil {
			return err
		}
		return netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP("5.5.5.254")})
	})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "add the fake node default route")
	node := getNodeObj(initial, false)
	g.Expect(f.core.Nodes().Informer().GetStore().Add(&node)).To(gomega.Succeed(), "cache the fake node")
	_, err = f.client.CoreV1().Nodes().Create(context.Background(), &node, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the fake node in the API")
	g.Expect(f.core.Namespaces().Informer().GetStore().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "work"}})).To(gomega.Succeed(), "cache the test namespace")
	g.Expect(f.core.Pods().Informer().GetStore().Add(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "work"}, Spec: corev1.PodSpec{NodeName: node1Name}, Status: corev1.PodStatus{PodIP: "10.128.0.2", PodIPs: []corev1.PodIP{{IP: "10.128.0.2"}}}})).To(gomega.Succeed(), "cache the test pod")
	owners := make(map[string]*eipv1.EgressIP)
	classes := make(map[string]*trafficv1.TrafficClass)
	for i, name := range []string{"one", "two"} {
		owner := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"class": name}}}, Status: eipv1.EgressIPStatus{Items: []eipv1.EgressIPStatusItem{{Node: node1Name, EgressIP: fmt.Sprintf("5.5.5.%d", 100+i)}}}}
		class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"class": name}}, Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: fmt.Sprintf("203.0.%d.0/24", 113+i)}}}}
		g.Expect(f.eips.Informer().GetStore().Add(owner)).To(gomega.Succeed(), "cache the test EgressIP")
		g.Expect(f.classes.Informer().GetStore().Add(class)).To(gomega.Succeed(), "cache the test TrafficClass")
		owners[name], classes[name] = owner, class
	}
	return f, netns, owners, classes
}

func TestConcurrentHostOwnerReconciliation(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	f, netns, _, _ := setupSelectiveHostOwners(t)
	g := gomega.NewWithT(t)
	c := f.controller
	paused := &pausedNodeAnnotation{nodeAnnotationInformer: &nodeAnnotationInformer{Interface: c.kube, client: f.client, nodes: f.core.Nodes().Informer().GetStore()}, entered: make(chan struct{}), release: make(chan struct{})}
	c.kube = paused
	var workers sync.WaitGroup
	var release sync.Once
	t.Cleanup(func() {
		release.Do(func() { close(paused.release) })
		stopped := make(chan struct{})
		go func() { workers.Wait(); close(stopped) }()
		g.Eventually(stopped, 5*time.Second).Should(gomega.BeClosed(), "host owner workers did not stop")
	})
	startOwner := func(name string) <-chan error {
		done := make(chan error, 1)
		workers.Add(1)
		go func() { defer workers.Done(); done <- netns.Do(func(ns.NetNS) error { return c.syncEIP(name) }) }()
		return done
	}
	one := startOwner("one")
	select {
	case <-paused.entered:
	case err := <-one:
		t.Fatalf("first owner did not reach its annotation write: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first owner did not reach its annotation write")
	}
	two := startOwner("two")
	select {
	case err := <-two:
		g.Expect(err).NotTo(gomega.HaveOccurred(), "reconcile the independent second owner")
	case <-time.After(5 * time.Second):
		t.Fatal("independent owner was blocked by the first reconciliation")
	}
	g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"))
	release.Do(func() { close(paused.release) })
	g.Eventually(one, 5*time.Second).Should(gomega.Receive(gomega.Succeed()), "first owner did not finish after its annotation write resumed")
	g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.100"))
	g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"))
}

func TestSharedHostRouteRejectsStaleUplink(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	f, netns, owners, _ := setupSelectiveHostOwners(t)
	g := gomega.NewWithT(t)
	c := f.controller
	c.kube = &nodeAnnotationInformer{Interface: c.kube, client: f.client, nodes: f.core.Nodes().Informer().GetStore()}
	g.Expect(assertInNetNS(netns, func(g gomega.Gomega) error {
		stale, err := c.getConfigAndUpdateRefs(owners["one"], false)
		if err != nil {
			return err
		}
		link, err := netlink.LinkByName("dummy1")
		if err != nil {
			return err
		}
		nextHop := net.ParseIP("5.5.5.253")
		g.Expect(netlink.RouteReplace(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: nextHop})).To(gomega.Succeed(), "change the uplink gateway at the same metric")
		g.Expect(c.syncEIP("two")).To(gomega.Succeed(), "apply the new gateway for the peer")
		g.Expect(c.updateEIP("one", newState(), stale)).To(gomega.MatchError(gomega.ContainSubstring("uplink route changed")), "reject the first owner's stale plan")
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: util.CalculateRouteTableID(link.Attrs().Index)}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return err
		}
		g.Expect(routes).To(gomega.ContainElement(gomega.HaveField("Gw", gomega.Equal(nextHop.To4()))), "keep the new gateway installed")
		g.Expect(c.syncEIP("one")).To(gomega.Succeed(), "retry from the current uplink")
		return nil
	})).To(gomega.Succeed(), "reconcile two snapshots in a real routing table")
}

func TestSharedHostRouteMetricReplacement(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	t.Run("prune before replacement", func(t *testing.T) { testSharedHostRouteMetricReplacement(t, true) })
	t.Run("prune after replacement", func(t *testing.T) { testSharedHostRouteMetricReplacement(t, false) })
}

func testSharedHostRouteMetricReplacement(t *testing.T, pruneFirst bool) {
	t.Helper()
	f, netns, _, _ := setupSelectiveHostOwners(t)
	c := f.controller
	g := gomega.NewWithT(t)
	c.kube = &nodeAnnotationInformer{Interface: c.kube, client: f.client, nodes: f.core.Nodes().Informer().GetStore()}
	g.Expect(assertInNetNS(netns, func(g gomega.Gomega) error {
		link, err := netlink.LinkByName("dummy1")
		if err != nil {
			return err
		}
		old := netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP("5.5.5.254")}
		newRoute := netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP("5.5.5.253"), Priority: 300}
		for _, owner := range []string{"one", "two"} {
			g.Expect(c.syncEIP(owner)).To(gomega.Succeed(), "install the original route for %s", owner)
		}
		checkGateways := func(gateways ...string) {
			routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: util.CalculateRouteTableID(link.Attrs().Index)}, netlink.RT_FILTER_TABLE)
			g.Expect(err).NotTo(gomega.HaveOccurred(), "read the shared policy routing table")
			var actual []string
			for _, route := range routes {
				if route.Gw != nil {
					actual = append(actual, route.Gw.String())
				}
			}
			g.Expect(actual).To(gomega.ConsistOf(gateways), "mirror the uplink gateways")
		}
		checkGateways(old.Gw.String())
		retire := func(name string) {
			owner, err := f.eips.Lister().Get(name)
			g.Expect(err).NotTo(gomega.HaveOccurred(), "read the owner before deleting %s", name)
			g.Expect(f.eips.Informer().GetStore().Delete(owner)).To(gomega.Succeed(), "delete owner %s", name)
			g.Expect(c.syncEIP(name)).To(gomega.Succeed(), "withdraw resources for %s", name)
			g.Expect(c.routeOwners).NotTo(gomega.HaveKey(name), "release every route claim for %s", name)
		}
		if pruneFirst {
			retire("two")
		}
		g.Expect(netlink.RouteDel(&old)).To(gomega.Succeed(), "withdraw the original metric from the uplink")
		g.Expect(netlink.RouteAdd(&newRoute)).To(gomega.Succeed(), "advertise the replacement metric")
		if pruneFirst {
			g.Expect(c.syncEIP("one")).To(gomega.Succeed(), "replace the retired uplink metric")
			checkGateways(newRoute.Gw.String())
		} else {
			g.Expect(c.syncEIP("two")).To(gomega.Succeed(), "replace the metric for both owners")
			checkGateways(newRoute.Gw.String())
			applied, found := c.cache.Load("one")
			g.Expect(found).To(gomega.BeTrue(), "load the first owner's pre-replacement snapshot")
			g.Expect(c.pruneOwnedResources("one", applied)).To(gomega.Succeed(), "prune using the earlier live snapshot")
			g.Expect(c.routeOwners["one"]).To(gomega.ContainElement(gomega.HaveField("Priority", 300)), "retain the transferred route")
			alternate := netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP("5.5.5.252"), Priority: 200}
			g.Expect(netlink.RouteAdd(&alternate)).To(gomega.Succeed(), "advertise an additional preferred metric")
			g.Expect(c.syncEIP("two")).To(gomega.Succeed(), "mirror both uplink metrics")
			g.Expect(c.pruneOwnedResources("one", applied)).To(gomega.Succeed(), "retain both metrics through stale live pruning")
			retire("two")
			checkGateways(newRoute.Gw.String(), alternate.Gw.String())
		}
		retire("one")
		checkGateways()
		return nil
	})).To(gomega.Succeed(), "replace a route shared by two live owners")
}

func TestSharedHostRouteWithdrawal(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	f, netns, _, _ := setupSelectiveHostOwners(t)
	c := f.controller
	g := gomega.NewWithT(t)
	c.kube = &nodeAnnotationInformer{Interface: c.kube, client: f.client, nodes: f.core.Nodes().Informer().GetStore()}
	g.Expect(assertInNetNS(netns, func(g gomega.Gomega) error {
		link, err := netlink.LinkByName("dummy1")
		if err != nil {
			return err
		}
		_, prefix, err := net.ParseCIDR("203.0.113.0/24")
		if err != nil {
			return err
		}
		specific := netlink.Route{LinkIndex: link.Attrs().Index, Dst: prefix, Gw: net.ParseIP("5.5.5.253")}
		g.Expect(netlink.RouteAdd(&specific)).To(gomega.Succeed(), "advertise a prefix in addition to the default")
		for _, owner := range []string{"one", "two"} {
			g.Expect(c.syncEIP(owner)).To(gomega.Succeed(), "copy the prefix for %s", owner)
		}
		specificClaim := func(route netlink.Route) bool { return route.Dst != nil && route.Dst.String() == prefix.String() }
		g.Expect(slices.ContainsFunc(c.routeOwners["two"], specificClaim)).To(gomega.BeTrue(), "exercise the idle peer's claim")
		g.Expect(netlink.RouteDel(&specific)).To(gomega.Succeed(), "withdraw the prefix without a replacement")
		g.Expect(c.syncEIP("one")).To(gomega.Succeed(), "remove the stale copy without waiting for the peer")
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: util.CalculateRouteTableID(link.Attrs().Index)}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return err
		}
		g.Expect(slices.ContainsFunc(routes, specificClaim)).To(gomega.BeFalse(), "let the withdrawn destination use the default route")
		g.Expect(routes).To(gomega.ContainElement(gomega.HaveField("Gw", net.ParseIP("5.5.5.254").To4())), "retain the shared default route")
		g.Expect(slices.ContainsFunc(c.routeOwners["two"], specificClaim)).To(gomega.BeFalse(), "clear the idle peer's obsolete claim")
		return nil
	})).To(gomega.Succeed(), "remove a withdrawn shared destination")
}

func TestHostOwnerChangesAfterRepair(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	f, netns, owners, classes := setupSelectiveHostOwners(t)
	g := gomega.NewWithT(t)
	c := f.controller
	c.kube = &nodeAnnotationInformer{Interface: c.kube, client: f.client, nodes: f.core.Nodes().Informer().GetStore()}
	err := netns.Do(func(ns.NetNS) error {
		for _, name := range []string{"one", "two"} {
			if err := c.syncEIP(name); err != nil {
				return err
			}
		}
		return nil
	})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "install both owners before restarting")
	// Startup claims must survive one owner being deleted before its first worker pass.
	err = assertInNetNS(netns, func(g gomega.Gomega) error {
		c = f.newController(t, g)
		if err := c.ruleManager.OwnPriority(rulePriority); err != nil {
			return err
		}
		if err := c.repairNode(); err != nil {
			return err
		}
		if err := f.eips.Informer().GetStore().Delete(owners["one"]); err != nil {
			return err
		}
		if err := c.syncEIP("one"); err != nil {
			return err
		}
		addresses, err := netlink.AddrList(nil, netlink.FAMILY_V4)
		if err != nil {
			return err
		}
		var ips []string
		for _, address := range addresses {
			ips = append(ips, address.IP.String())
		}
		g.Expect(ips).To(gomega.ContainElement("5.5.5.101"), "preserve the peer address after startup repair")
		g.Expect(ips).NotTo(gomega.ContainElement("5.5.5.100"), "remove the deleted owner's repaired address")
		node, err := f.client.CoreV1().Nodes().Get(context.Background(), node1Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		annotated, err := util.ParseNodeSecondaryHostEgressIPsAnnotation(node)
		if err != nil {
			return err
		}
		g.Expect(annotated.UnsortedList()).To(gomega.ConsistOf("5.5.5.101"), "remove only the deleted owner's annotation")
		return nil
	})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "remove the retired owner resources after startup repair")
	g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.100"))
	g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"), "startup must preserve the healthy peer")
	g.Expect(c.ruleOwners).NotTo(gomega.HaveKey("one"))
	g.Expect(c.routeOwners).NotTo(gomega.HaveKey("one"))
	// The first reconcile after a post-repair edit must remove the adopted rule.
	classes["two"] = classes["two"].DeepCopy()
	classes["two"].Spec.Matches[0].DestinationCIDR = "203.0.115.0/24"
	g.Expect(f.classes.Informer().GetStore().Update(classes["two"])).To(gomega.Succeed(), "update the cached TrafficClass")
	err = netns.Do(func(ns.NetNS) error {
		if err := c.syncEIP("two"); err != nil {
			return err
		}
		rules, err := netlink.RuleList(netlink.FAMILY_V4)
		if err != nil {
			return err
		}
		var destinations []string
		for _, rule := range rules {
			if rule.Priority == ruleTrafficPriority {
				destinations = append(destinations, rule.Dst.String())
			}
		}
		if len(destinations) != 1 || destinations[0] != "203.0.115.0/24" {
			return fmt.Errorf("stale repaired rules survived: %v", destinations)
		}
		return nil
	})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "prune repaired rules after an offline class edit")
	g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"), "publish after reinstalling the peer rule")
}

func TestDeleteCatchAllBeforeFirstReconcileAfterRepair(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	f, netns, owners, _ := setupSelectiveHostOwners(t)
	g := gomega.NewWithT(t)
	catchAll := owners["one"].DeepCopy()
	catchAll.Spec.TrafficSelector = nil
	g.Expect(f.eips.Informer().GetStore().Update(catchAll)).To(gomega.Succeed(), "cache the catch-all owner")
	c := f.controller
	c.kube = &nodeAnnotationInformer{Interface: c.kube, client: f.client, nodes: f.core.Nodes().Informer().GetStore()}
	g.Expect(assertInNetNS(netns, func(g gomega.Gomega) error {
		g.Expect(c.syncEIP(catchAll.Name)).To(gomega.Succeed(), "install the original catch-all")
		g.Expect(c.syncEIP("two")).To(gomega.Succeed(), "install the selective peer")
		restarted := f.newController(t, g)
		g.Expect(restarted.repairNode()).To(gomega.Succeed(), "repair the installed SNAT state")
		g.Expect(f.nft.Dump()).To(gomega.ContainSubstring(catchAll.Status.Items[0].EgressIP), "confirm repair retained catch-all SNAT")
		g.Expect(f.eips.Informer().GetStore().Delete(catchAll)).To(gomega.Succeed(), "delete the catch-all before its first worker event")
		g.Expect(restarted.syncEIP(catchAll.Name)).To(gomega.Succeed(), "remove the retired owner's startup state")
		g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring(catchAll.Status.Items[0].EgressIP), "remove the repaired catch-all SNAT element")
		g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"), "preserve the selective peer's SNAT")
		return nil
	})).To(gomega.Succeed(), "delete repaired state in the host network namespace")
}

func TestHostSelectivePeerWaitsForRuleWithdrawal(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	f, netns, _, classes := setupSelectiveHostOwners(t)
	g := gomega.NewWithT(t)
	c := f.controller
	c.kube = &nodeAnnotationInformer{Interface: c.kube, client: f.client, nodes: f.core.Nodes().Informer().GetStore()}
	updateCIDR := func(name, cidr string) {
		class := classes[name].DeepCopy()
		class.Spec.Matches[0].DestinationCIDR = cidr
		g.Expect(f.classes.Informer().GetStore().Update(class)).To(gomega.Succeed(), "cache the new destination for %s", name)
	}
	err := assertInNetNS(netns, func(g gomega.Gomega) error {
		updateCIDR("two", "203.0.113.128/25")
		g.Expect(c.syncEIP("one")).To(gomega.Succeed(), "install the wide match")
		g.Expect(c.syncEIP("two")).To(gomega.Succeed(), "process the initially rejected peer")
		updateCIDR("one", "203.0.113.0/25")
		g.Expect(c.syncEIP("two")).To(gomega.MatchError(gomega.ContainSubstring("overlapping IP rule")), "block the peer while the wide rule remains installed")
		g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.101"), "do not publish overlapping peer SNAT")

		g.Expect(c.ruleWaiters).To(gomega.HaveLen(1), "record the live waiter's dependency")
		for rule := range c.ruleWaiters {
			g.Expect(c.addOwnedIPRule("deleted-waiter", rule)).To(gomega.MatchError(gomega.ContainSubstring("overlapping IP rule")))
		}
		g.Expect(c.syncEIP("deleted-waiter")).To(gomega.Succeed(), "process a deleted waiter without a cache entry")
		rules := &failingOwnedRules{Interface: c.ruleManager, failDelete: true}
		c.ruleManager = rules
		g.Expect(c.syncEIP("one")).To(gomega.MatchError(gomega.ContainSubstring("failed to delete IP rule")), "leave the old steering installed after a failed withdrawal")
		g.Expect(c.syncEIP("two")).To(gomega.MatchError(gomega.ContainSubstring("overlapping IP rule")), "keep the peer blocked after failed withdrawal")
		g.Expect(c.eIPQueue.Len()).To(gomega.BeZero(), "wait until the blocker has been removed before waking the peer")
		rules.failDelete = false
		g.Expect(c.syncEIP("one")).To(gomega.Succeed(), "replace the wide match with the narrower rule")
		g.Expect(c.eIPQueue.Len()).To(gomega.Equal(1), "requeue the peer after withdrawing its blocker")
		owner, _ := c.eIPQueue.Get()
		defer c.eIPQueue.Done(owner)
		g.Expect(owner).To(gomega.Equal("two"), "retry the previously blocked owner")
		g.Expect(c.syncEIP(owner)).To(gomega.Succeed(), "install the disjoint peer match")
		g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"), "publish the peer's SNAT once the rules are disjoint")
		return nil
	})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "reconcile a host traffic handoff in the test network namespace")
}

func TestSelectiveRollbackKeepsClaimsUntilSNATWithdrawal(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	f, netns, _, _ := setupSelectiveHostOwners(t)
	g := gomega.NewWithT(t)
	c := f.controller
	g.Expect(assertInNetNS(netns, func(g gomega.Gomega) error {
		g.Expect(c.syncEIP("one")).To(gomega.Succeed(), "install selective steering and SNAT")
		applied, found := c.cache.Load("one")
		g.Expect(found).To(gomega.BeTrue())
		rules := c.ruleOwners["one"].UnsortedList()
		g.Expect(rules).To(gomega.HaveLen(1))
		c.snatRules["invalid"] = []string{"jump missing-chain"}
		g.Expect(c.rollbackSelectiveIPRules("one", applied, rules)).To(gomega.MatchError(gomega.ContainSubstring("missing-chain")))
		g.Expect(applied.rollbackRules).To(gomega.Equal(rules), "retry the SNAT withdrawal before removing steering")
		g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.100"), "the failed transaction keeps installed SNAT")
		g.Expect(c.addOwnedIPRule("two", rules[0])).To(gomega.MatchError(gomega.ContainSubstring("overlapping IP rule")))
		delete(c.snatRules, "invalid")
		g.Expect(c.rollbackSelectiveIPRules("one", applied, applied.rollbackRules)).To(gomega.Succeed())
		g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.100"))
		g.Expect(c.addOwnedIPRule("two", rules[0])).To(gomega.Succeed(), "the peer can claim the rule once SNAT is withdrawn")
		return nil
	})).To(gomega.Succeed(), "retry rollback in the test network namespace")
}

// Failed route operations must leave ownership retryable.
type failingOwnedRoutes struct {
	util.NetLinkOps
	failAdd, failDelete bool
	onListFailure       func()
}

func (m *failingOwnedRoutes) RouteListFiltered(family int, filter *netlink.Route, mask uint64) ([]netlink.Route, error) {
	if m.onListFailure != nil && mask == netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE {
		m.onListFailure()
		return nil, errors.New("injected route inventory failure")
	}
	return m.NetLinkOps.RouteListFiltered(family, filter, mask)
}

func (m *failingOwnedRoutes) RouteReplace(route *netlink.Route) error {
	if m.failAdd {
		m.failAdd = false
		return errors.New("injected route add failure")
	}
	return m.NetLinkOps.RouteReplace(route)
}

func (m *failingOwnedRoutes) RouteDel(route *netlink.Route) error {
	if m.failDelete {
		m.failDelete = false
		return errors.New("injected route delete failure")
	}
	return m.NetLinkOps.RouteDel(route)
}

func TestSharedHostResourceRetries(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	f := newOwnershipTestFixture(t)
	g := gomega.NewWithT(t)
	c := f.controller
	initial := nodeConfig{linkConfigs: []linkConfig{{linkName: "dummy1", address: []address{{cidr: "5.5.5.10/24"}}}}}
	netns, cleanup, err := setupFakeTestNode(initial)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "set up the fake node network namespace")
	t.Cleanup(func() { g.Expect(cleanup()).To(gomega.Succeed(), "clean up the fake node network namespace") })
	err = assertInNetNS(netns, func(g gomega.Gomega) error {
		rule := iprulemanager.IPRule{Priority: rulePriority, Table: 1007, Family: netlink.FAMILY_V4, Src: netip.MustParsePrefix("10.128.0.2/32")}
		invalid := rule
		invalid.Family = unix.AF_UNIX
		invalid.Src = netip.Prefix{}
		g.Expect(c.addOwnedIPRule("one", invalid)).NotTo(gomega.Succeed(), "reject an invalid rule without recording ownership")
		g.Expect(c.ruleOwners["one"].Has(invalid)).To(gomega.BeFalse(), "do not retain a claim for the rejected IP rule")
		g.Expect(c.addOwnedIPRule("one", rule)).To(gomega.Succeed(), "claim the shared IP rule")
		g.Expect(c.addOwnedIPRule("one", rule)).To(gomega.Succeed(), "claim the shared IP rule")
		g.Expect(c.addOwnedIPRule("two", rule)).To(gomega.Succeed(), "claim the shared IP rule")
		g.Expect(c.deleteOwnedIPRule("one", rule)).To(gomega.Succeed(), "release the owner IP rule claim")
		g.Expect(c.pruneOwnedResources("two", nil)).To(gomega.Succeed(), "prune obsolete owner resource claims")
		g.Expect(c.ruleOwners).To(gomega.BeEmpty())
		actualRules, err := netlink.RuleList(netlink.FAMILY_V4)
		if err != nil {
			return err
		}
		for _, actual := range actualRules {
			g.Expect(iprulemanager.IPRuleFromNetlinkRule(&actual)).NotTo(gomega.Equal(rule))
		}
		link, err := netlink.LinkByName("dummy1")
		if err != nil {
			return err
		}
		route := netlink.Route{LinkIndex: link.Attrs().Index, Table: util.CalculateRouteTableID(link.Attrs().Index), Gw: net.ParseIP("5.5.5.254")}
		original := util.GetNetLinkOps()
		defer util.SetNetLinkOpMockInst(original)
		routes := &failingOwnedRoutes{NetLinkOps: original, failAdd: true}
		util.SetNetLinkOpMockInst(routes)
		g.Expect(c.addOwnedRoute("one", route)).To(gomega.MatchError(gomega.ContainSubstring("injected route add failure")))
		g.Expect(c.routeOwners["one"]).To(gomega.BeEmpty())
		g.Expect(c.addOwnedRoute("one", route)).To(gomega.Succeed(), "claim the shared route")
		g.Expect(c.addOwnedRoute("one", route)).To(gomega.Succeed(), "claim the shared route")
		routes.failDelete = true
		g.Expect(c.deleteOwnedRoute("one", route)).To(gomega.MatchError(gomega.ContainSubstring("injected route delete failure")))
		g.Expect(c.routeOwners["one"]).To(gomega.HaveLen(1))
		g.Expect(c.addOwnedRoute("two", route)).To(gomega.Succeed(), "claim the shared route")
		g.Expect(c.deleteOwnedRoute("one", route)).To(gomega.Succeed(), "release the owner route claim")
		g.Expect(c.pruneOwnedResources("two", nil)).To(gomega.Succeed(), "prune obsolete owner resource claims")
		g.Expect(c.routeOwners).To(gomega.BeEmpty())
		actualRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: route.Table}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return err
		}
		g.Expect(actualRoutes).To(gomega.BeEmpty())
		g.Expect(c.addOwnedIPRule("one", rule)).To(gomega.Succeed(), "claim the shared IP rule")
		g.Expect(c.addOwnedRoute("one", route)).To(gomega.Succeed(), "claim the shared route")
		otherRoute := route
		otherRoute.Table++
		g.Expect(c.addOwnedRoute("one", otherRoute)).To(gomega.Succeed(), "claim the shared route")
		routes.failDelete = true
		pruneErr := c.pruneOwnedResources("one", nil)
		g.Expect(pruneErr).To(gomega.MatchError(gomega.ContainSubstring("injected route delete failure")))
		g.Expect(c.ruleOwners).To(gomega.BeEmpty())
		g.Expect(c.routeOwners["one"]).To(gomega.Equal([]netlink.Route{route}))
		g.Expect(c.pruneOwnedResources("one", nil)).To(gomega.Succeed(), "prune obsolete owner resource claims")
		g.Expect(c.ruleOwners).To(gomega.BeEmpty())
		g.Expect(c.routeOwners).To(gomega.BeEmpty())
		return nil
	})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "exercise shared resource failures and retries")
}

// Fail additions directly; deletions reach the real manager with kernel permissions denied.
type failingOwnedRules struct {
	iprulemanager.Interface
	failAdd, failDelete bool
}

func (m *failingOwnedRules) Add(rule iprulemanager.IPRule) error {
	if m.failAdd {
		return errors.New("injected rule add failure")
	}
	return m.Interface.Add(rule)
}

func (m *failingOwnedRules) Delete(rule iprulemanager.IPRule) error {
	return m.deleteRule(rule, m.Interface.Delete)
}

func (m *failingOwnedRules) DeleteWithoutRetry(rule iprulemanager.IPRule) error {
	return m.deleteRule(rule, m.Interface.DeleteWithoutRetry)
}

func (m *failingOwnedRules) deleteRule(rule iprulemanager.IPRule, delete func(iprulemanager.IPRule) error) error {
	if m.failDelete {
		goruntime.LockOSThread()
		defer goruntime.UnlockOSThread()
		header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
		var original [2]unix.CapUserData
		if err := unix.Capget(&header, &original[0]); err != nil {
			return err
		}
		denied := original
		denied[unix.CAP_NET_ADMIN/32].Effective &^= 1 << (unix.CAP_NET_ADMIN % 32)
		if err := unix.Capset(&header, &denied[0]); err != nil {
			return err
		}
		deleteErr := delete(rule)
		return errors.Join(deleteErr, unix.Capset(&header, &original[0]))
	}
	return delete(rule)
}

func TestSelectiveSNATPublishesAppliedState(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	f := newOwnershipTestFixture(t)
	g := gomega.NewWithT(t)
	c := f.controller
	c.kube = &nodeAnnotationInformer{Interface: c.kube, client: f.client, nodes: f.core.Nodes().Informer().GetStore()}
	initial := nodeConfig{linkConfigs: []linkConfig{{linkName: "dummy1", address: []address{{cidr: "5.5.5.10/24"}}}}}
	netns, cleanup, err := setupFakeTestNode(initial)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "set up the fake node network namespace")
	t.Cleanup(func() { g.Expect(cleanup()).To(gomega.Succeed()) })
	node := getNodeObj(initial, false)
	g.Expect(f.core.Nodes().Informer().GetStore().Add(&node)).To(gomega.Succeed())
	_, err = f.client.CoreV1().Nodes().Create(context.Background(), &node, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(f.core.Namespaces().Informer().GetStore().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "work"}})).To(gomega.Succeed())
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "work"}, Spec: corev1.PodSpec{NodeName: node1Name}, Status: corev1.PodStatus{PodIP: "10.128.0.2", PodIPs: []corev1.PodIP{{IP: "10.128.0.2"}}}}
	g.Expect(f.core.Pods().Informer().GetStore().Add(pod)).To(gomega.Succeed())
	catchAll := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "catch-all"}, Status: eipv1.EgressIPStatus{Items: []eipv1.EgressIPStatusItem{{Node: node1Name, EgressIP: "5.5.5.100"}}}}
	selective := catchAll.DeepCopy()
	selective.Name = "selective"
	selective.Spec.TrafficSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"class": "selected"}}
	selective.Status.Items[0].EgressIP = "5.5.5.101"
	class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "selected", Labels: map[string]string{"class": "selected"}}, Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: "203.0.113.0/24"}}}}
	g.Expect(f.eips.Informer().GetStore().Add(catchAll)).To(gomega.Succeed())
	g.Expect(f.eips.Informer().GetStore().Add(selective)).To(gomega.Succeed())
	g.Expect(f.classes.Informer().GetStore().Add(class)).To(gomega.Succeed())
	rules := &failingOwnedRules{Interface: c.ruleManager}
	c.ruleManager = rules
	err = assertInNetNS(netns, func(g gomega.Gomega) error {
		link, err := netlink.LinkByName("dummy1")
		if err != nil {
			return err
		}
		if err := netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP("5.5.5.254")}); err != nil {
			return err
		}
		g.Expect(c.syncEIP(catchAll.Name)).To(gomega.Succeed(), "install the catch-all on the shared interface")
		rules.failAdd = true
		g.Expect(c.syncEIP(selective.Name)).To(gomega.MatchError(gomega.ContainSubstring("injected rule add failure")))
		g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.101"), "a retry entry must not activate selective SNAT")
		g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("5.5.5.100"), "preserve catch-all SNAT")
		addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			return err
		}
		for _, addr := range addresses {
			g.Expect(addr.IP.String()).NotTo(gomega.Equal("5.5.5.101"))
		}
		rules.failAdd = false
		// Failure after rule installation must not publish until address and routes also succeed.
		failAnnotation := true
		f.client.PrependReactor("patch", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
			if failAnnotation {
				failAnnotation = false
				return true, nil, errors.New("injected annotation failure")
			}
			return false, nil, nil
		})
		g.Expect(c.syncEIP(selective.Name)).To(gomega.MatchError(gomega.ContainSubstring("injected annotation failure")))
		g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.101"))
		originalOps := util.GetNetLinkOps()
		addressesRejected := &failingEgressAddress{NetLinkOps: originalOps, ip: "5.5.5.101"}
		util.SetNetLinkOpMockInst(addressesRejected)
		defer util.SetNetLinkOpMockInst(originalOps)
		g.Expect(c.syncEIP(selective.Name)).To(gomega.MatchError(gomega.ContainSubstring("injected address failure")))
		g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.101"))
		attempted, _ := c.cache.Load(selective.Name)
		g.Expect(attempted.eIPConfig.addr).NotTo(gomega.BeNil(), "track failed address attempts for teardown")
		g.Expect(attempted.eIPConfig.addr.IP.String()).To(gomega.Equal("5.5.5.101"), "remember failed link-manager registration for teardown")
		// A failed peer stays registered on the link while a different selective owner succeeds.
		healthy := selective.DeepCopy()
		healthy.Name = "healthy"
		healthy.Spec.TrafficSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"class": "healthy"}}
		healthy.Status.Items[0].EgressIP = "5.5.5.102"
		healthyClass := class.DeepCopy()
		healthyClass.Name = "healthy"
		healthyClass.Labels = map[string]string{"class": "healthy"}
		healthyClass.Spec.Matches[0].DestinationCIDR = "203.0.114.0/24"
		g.Expect(f.eips.Informer().GetStore().Add(healthy)).To(gomega.Succeed())
		g.Expect(f.classes.Informer().GetStore().Add(healthyClass)).To(gomega.Succeed())
		g.Expect(c.syncEIP(healthy.Name)).To(gomega.Succeed(), "a rejected peer address must not block healthy selective SNAT")
		g.Expect(c.syncEIP(catchAll.Name)).To(gomega.Succeed(), "keep the installed catch-all healthy on the same interface")
		g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.102"))
		g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.101"))
		g.Expect(f.eips.Informer().GetStore().Delete(healthy)).To(gomega.Succeed())
		g.Expect(c.syncEIP(healthy.Name)).To(gomega.Succeed(), "clean up the healthy peer independently")
		g.Expect(f.eips.Informer().GetStore().Delete(selective)).To(gomega.Succeed())
		g.Expect(c.syncEIP(selective.Name)).To(gomega.Succeed(), "delete before retrying the rejected address")
		_, cachedAttempt := c.cache.Load(selective.Name)
		g.Expect(cachedAttempt).To(gomega.BeFalse())
		addresses, err = netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			return err
		}
		for _, address := range addresses {
			g.Expect(address.IP.String()).NotTo(gomega.Equal("5.5.5.101"))
		}
		currentNode, err := f.client.CoreV1().Nodes().Get(context.Background(), node1Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		annotated, err := util.ParseNodeSecondaryHostEgressIPsAnnotation(currentNode)
		if err != nil {
			return err
		}
		g.Expect(annotated.Has("5.5.5.101")).To(gomega.BeFalse(), "remove the failed attempt's annotation")
		util.SetNetLinkOpMockInst(originalOps)
		g.Expect(f.eips.Informer().GetStore().Add(selective)).To(gomega.Succeed())
		g.Expect(c.syncEIP(selective.Name)).To(gomega.Succeed(), "retry the complete selective configuration")
		g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"))
		g.Expect(f.eips.Informer().GetStore().Delete(selective)).To(gomega.Succeed())
		rules.failDelete = true
		g.Expect(c.syncEIP(selective.Name)).To(gomega.MatchError(gomega.ContainSubstring("failed to delete IP rule")))
		g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"), "retain selective SNAT while kernel steering is installed")
		// OwnPriority runs the same reconciliation as the manager's periodic pass.
		g.Expect(rules.OwnPriority(ruleTrafficPriority)).To(gomega.Succeed(), "reconcile the rule manager without independently deleting the live rule")
		active, err := netlink.RuleListFiltered(netlink.FAMILY_V4, &netlink.Rule{Priority: ruleTrafficPriority}, netlink.RT_FILTER_PRIORITY)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "read selective rules after rule-manager reconciliation")
		g.Expect(active).To(gomega.HaveLen(1), "retain the selective rule until the owner retries")
		rules.failDelete = false
		g.Expect(c.syncEIP(selective.Name)).To(gomega.Succeed(), "retry selective teardown")
		g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.101"))
		g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("5.5.5.100"))
		// Desired configuration alone must not activate SNAT during startup repair either.
		g.Expect(f.eips.Informer().GetStore().Add(selective)).To(gomega.Succeed())
		restarted := f.newController(t, g)
		g.Expect(restarted.repairNode()).To(gomega.Succeed(), "repair before the selective rule has been installed")
		g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.101"))
		g.Expect(restarted.syncEIP(selective.Name)).To(gomega.Succeed())
		g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"))
		// Repair can retain confirmed state, including deletion retry before the first event.
		currentNode, err = f.client.CoreV1().Nodes().Get(context.Background(), node1Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		g.Expect(f.core.Nodes().Informer().GetStore().Update(currentNode)).To(gomega.Succeed())
		retained := f.newController(t, g)
		g.Expect(retained.repairNode()).To(gomega.Succeed())
		g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"), "retain confirmed startup state")
		retainedRules := &failingOwnedRules{Interface: retained.ruleManager, failDelete: true}
		retained.ruleManager = retainedRules
		g.Expect(f.eips.Informer().GetStore().Delete(selective)).To(gomega.Succeed(), "delete the owner retained at startup")
		g.Expect(retained.syncEIP(selective.Name)).To(gomega.MatchError(gomega.ContainSubstring("failed to delete IP rule")), "report the failed deletion of retained steering")
		g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"), "keep the repaired path usable after the failed deletion")
		retainedRules.failDelete = false
		g.Expect(retained.syncEIP(selective.Name)).To(gomega.Succeed(), "complete teardown of the repaired owner")
		g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.101"), "remove the repaired SNAT after deleting its steering")
		return nil
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

type failingEgressAddress struct {
	util.NetLinkOps
	ip string
}

func (m *failingEgressAddress) AddrAdd(link netlink.Link, address *netlink.Addr) error {
	if address.IP.String() == m.ip {
		return errors.New("injected address failure")
	}
	return m.NetLinkOps.AddrAdd(link, address)
}

func TestSelectiveStartupPreservesHealthyPeers(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	t.Run("IPv4", func(t *testing.T) { testSelectiveStartupPreservesHealthyPeers(t, false) })
	t.Run("IPv6", func(t *testing.T) { testSelectiveStartupPreservesHealthyPeers(t, true) })
}

func testSelectiveStartupPreservesHealthyPeers(t *testing.T, ipv6 bool) {
	t.Helper()
	f := newOwnershipTestFixtureForFamily(t, ipv6)
	g := gomega.NewWithT(t)
	family := netlink.FAMILY_V4
	linkCIDR, gateway, podIP := "5.5.5.10/24", "5.5.5.254", "10.128.0.2"
	egressAddresses := []string{"5.5.5.100", "5.5.5.101"}
	destinations := []string{"203.0.113.0/24", "203.0.114.0/24"}
	if ipv6 {
		family = netlink.FAMILY_V6
		linkCIDR, gateway, podIP = "2001:db8:1::10/64", "2001:db8:1::fe", "fd00:10:244::2"
		egressAddresses = []string{"2001:db8:1::100", "2001:db8:1::101"}
		destinations = []string{"2001:db8:100::/64", "2001:db8:101::/64"}
	}
	initial := nodeConfig{linkConfigs: []linkConfig{{linkName: "dummy1", address: []address{{cidr: linkCIDR}}}}}
	netns, cleanup, err := setupFakeTestNode(initial)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(func() { g.Expect(cleanup()).To(gomega.Succeed()) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	node := getNodeObj(initial, false)
	_, err = f.client.CoreV1().Nodes().Create(ctx, &node, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(f.core.Nodes().Informer().GetStore().Add(&node)).To(gomega.Succeed())
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "work"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: namespace.Name}, Spec: corev1.PodSpec{NodeName: node1Name}, Status: corev1.PodStatus{PodIP: podIP, PodIPs: []corev1.PodIP{{IP: podIP}}}}
	_, err = f.client.CoreV1().Namespaces().Create(ctx, namespace, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = f.client.CoreV1().Pods(namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(f.core.Namespaces().Informer().GetStore().Add(namespace)).To(gomega.Succeed())
	g.Expect(f.core.Pods().Informer().GetStore().Add(pod)).To(gomega.Succeed())
	owners := []string{"one", "two"}
	for i, name := range owners {
		owner := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"class": name}}}, Status: eipv1.EgressIPStatus{Items: []eipv1.EgressIPStatusItem{{Node: node1Name, EgressIP: egressAddresses[i]}}}}
		class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"class": name}}, Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: destinations[i]}}}}
		_, err = f.eipClient.K8sV1().EgressIPs().Create(ctx, owner, metav1.CreateOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		_, err = f.trafficClient.K8sV1().TrafficClasses().Create(ctx, class, metav1.CreateOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(f.eips.Informer().GetStore().Add(owner)).To(gomega.Succeed())
		g.Expect(f.classes.Informer().GetStore().Add(class)).To(gomega.Succeed())
	}
	f.controller.kube = &nodeAnnotationInformer{Interface: f.controller.kube, client: f.client, nodes: f.core.Nodes().Informer().GetStore()}
	var before []iprulemanager.IPRule
	err = netns.Do(func(ns.NetNS) error {
		link, err := netlink.LinkByName("dummy1")
		if err != nil {
			return err
		}
		if err := netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP(gateway)}); err != nil {
			return err
		}
		for _, owner := range owners {
			if err := f.controller.syncEIP(owner); err != nil {
				return err
			}
		}
		rules, err := netlink.RuleListFiltered(family, &netlink.Rule{Priority: ruleTrafficPriority}, netlink.RT_FILTER_PRIORITY)
		if err != nil {
			return err
		}
		for _, rule := range rules {
			before = append(before, iprulemanager.IPRuleFromNetlinkRule(&rule))
		}
		return nil
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(before).To(gomega.HaveLen(2))
	informers := []cache.SharedIndexInformer{f.core.Nodes().Informer(), f.core.Namespaces().Informer(), f.core.Pods().Informer(), f.eips.Informer(), f.classes.Informer()}
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	var synced []cache.InformerSynced
	for _, informer := range informers {
		workers.Add(1)
		go func() { defer workers.Done(); informer.Run(ctx.Done()) }()
		synced = append(synced, informer.HasSynced)
	}
	g.Expect(cache.WaitForCacheSync(ctx.Done(), synced...)).To(gomega.BeTrue())
	restarted := f.newController(t, g)
	err = assertInNetNS(netns, func(g gomega.Gomega) error {
		link, err := netlink.LinkByName("dummy1")
		if err != nil {
			return err
		}
		tryStartup := func(startupCtx context.Context, controller *Controller) error {
			// Keep workers out so they cannot hide a delete/reinstall cycle.
			startupCtx, stopStartup := context.WithCancel(startupCtx)
			var startupWorkers sync.WaitGroup
			err := controller.Run(startupCtx.Done(), &startupWorkers, 0)
			stopStartup()
			startupWorkers.Wait()
			return err
		}
		failedStartup := f.newController(t, g)
		guard := &knftables.Chain{Name: types.EgressIPNFTablesSecondaryInterfaceChainName}
		tx := f.nft.NewTransaction()
		tx.Flush(guard)
		tx.Delete(guard)
		g.Expect(f.nft.Run(ctx, tx)).To(gomega.Succeed(), "simulate missing leak protection at startup")
		ops := util.GetNetLinkOps()
		failureCtx, cancelFailure := context.WithCancel(ctx)
		defer cancelFailure()
		util.SetNetLinkOpMockInst(&failingOwnedRoutes{NetLinkOps: ops, onListFailure: cancelFailure})
		startupErr := tryStartup(failureCtx, failedStartup)
		util.SetNetLinkOpMockInst(ops)
		g.Expect(startupErr).To(gomega.MatchError(gomega.ContainSubstring("injected route inventory failure")))
		guardRules, err := f.nft.ListRules(ctx, guard.Name)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "install leak protection before the route inventory")
		g.Expect(guardRules).To(gomega.ConsistOf(gomega.HaveField("Rule", gomega.And(
			gomega.ContainSubstring("meta mark "+types.EgressIPSecondaryInterfaceMark),
			gomega.ContainSubstring(" drop comment "),
		))), "drop marked pod traffic even when route inventory fails")
		for _, address := range egressAddresses {
			g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("to "+address), "preserve existing selective SNAT through startup failure")
		}
		oldDefault := netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP(gateway)}
		newDefault := oldDefault
		newDefault.Priority = 200
		g.Expect(netlink.RouteDel(&oldDefault)).To(gomega.Succeed(), "withdraw the old uplink metric")
		g.Expect(netlink.RouteAdd(&newDefault)).To(gomega.Succeed(), "advertise the new metric before restart")
		failedAdd := &failingOwnedRoutes{NetLinkOps: ops, failAdd: true}
		util.SetNetLinkOpMockInst(failedAdd)
		startupErr = tryStartup(ctx, restarted)
		util.SetNetLinkOpMockInst(ops)
		g.Expect(startupErr).NotTo(gomega.HaveOccurred(), "complete startup after a one-shot route replacement failure")
		g.Expect(failedAdd.failAdd).To(gomega.BeFalse(), "exercise the route replacement failure")
		rules, err := netlink.RuleListFiltered(family, &netlink.Rule{Priority: ruleTrafficPriority}, netlink.RT_FILTER_PRIORITY)
		if err != nil {
			return err
		}
		var after []iprulemanager.IPRule
		for _, rule := range rules {
			after = append(after, iprulemanager.IPRuleFromNetlinkRule(&rule))
		}
		g.Expect(after).To(gomega.ConsistOf(before), "retain both healthy owners before any worker can reinstall them")
		for _, owner := range owners {
			applied, found := restarted.cache.Load(owner)
			g.Expect(found).To(gomega.BeTrue(), "retain the startup cache for owner %s", owner)
			entries := applied.namespacesWithPodIPConfigs["work"][ktypes.NamespacedName{Namespace: "work", Name: "pod"}].elems
			g.Expect(entries).To(gomega.HaveLen(1), "retain one pod config for owner %s", owner)
			g.Expect(entries[0].snatReady).To(gomega.BeTrue(), "retain ready SNAT for owner %s", owner)
			g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("to "+entries[0].snatValue), "retain the SNAT rule for owner %s", owner)
		}
		// A later startup cleanup failure must not leave the retired owner's SNAT active.
		g.Expect(f.eipClient.K8sV1().EgressIPs().Delete(ctx, owners[0], metav1.DeleteOptions{})).To(gomega.Succeed())
		g.Eventually(func() ([]*eipv1.EgressIP, error) { return f.eips.Lister().List(labels.Everything()) }, "5s").Should(gomega.HaveLen(1))
		manager := restarted.ruleManager
		restarted.ruleManager = &failingOwnedRules{Interface: manager, failDelete: true}
		g.Expect(restarted.repairNode()).To(gomega.MatchError(gomega.ContainSubstring("remove stale managed selective EgressIP rules")))
		rules, err = netlink.RuleListFiltered(family, &netlink.Rule{Priority: ruleTrafficPriority}, netlink.RT_FILTER_PRIORITY)
		if err != nil {
			return err
		}
		g.Expect(rules).To(gomega.HaveLen(2), "keep installed steering when the kernel refuses deletion")
		for _, address := range egressAddresses {
			g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("to "+address), "keep SNAT while steering to %s is still installed", address)
		}
		restarted.ruleManager = manager
		original := util.GetNetLinkOps()
		rejected := &failedStartupAddressCleanup{NetLinkOps: original, nft: f.nft, ip: egressAddresses[0]}
		util.SetNetLinkOpMockInst(rejected)
		defer util.SetNetLinkOpMockInst(original)
		g.Expect(restarted.repairNode()).To(gomega.MatchError(gomega.ContainSubstring("injected startup address cleanup failure")))
		g.Expect(rejected.snatWasActive).To(gomega.BeFalse(), "withdraw SNAT before attempting address teardown")
		g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("to " + egressAddresses[0]))
		g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("to "+egressAddresses[1]), "preserve the healthy peer through cleanup failure")
		rules, err = netlink.RuleListFiltered(family, &netlink.Rule{Priority: ruleTrafficPriority}, netlink.RT_FILTER_PRIORITY)
		if err != nil {
			return err
		}
		g.Expect(rules).To(gomega.HaveLen(1), "sweep the retired rule even when address cleanup fails")
		g.Expect(rules[0].Dst.String()).To(gomega.Equal(destinations[1]), "retain the healthy peer's rule")
		util.SetNetLinkOpMockInst(original)
		g.Expect(restarted.repairNode()).To(gomega.Succeed(), "retry the retired owner's address cleanup")
		exists, err := util.LinkAddrExist(link, egressip.GetNetlinkAddress(net.ParseIP(egressAddresses[0]), link.Attrs().Index).IPNet)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "read the retired address from the kernel")
		g.Expect(exists).To(gomega.BeFalse(), "remove the retired address on retry")
		return nil
	})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "repair selective state after restart")
}

type failedStartupAddressCleanup struct {
	util.NetLinkOps
	nft           *knftables.Fake
	ip            string
	snatWasActive bool
}

func (m *failedStartupAddressCleanup) AddrDel(link netlink.Link, address *netlink.Addr) error {
	if address.IP.String() == m.ip {
		m.snatWasActive = strings.Contains(m.nft.Dump(), "to "+m.ip)
		return errors.New("injected startup address cleanup failure")
	}
	return m.NetLinkOps.AddrDel(link, address)
}

func TestStartupRouteMetricReplacement(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	f := newOwnershipTestFixture(t)
	g := gomega.NewWithT(t)
	netns, cleanup, err := setupFakeTestNode(nodeConfig{linkConfigs: []linkConfig{
		{linkName: "dummy1", address: []address{{cidr: "5.5.5.10/24"}}},
	}})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the route repair link")
	t.Cleanup(func() { g.Expect(cleanup()).To(gomega.Succeed(), "clean up the route repair namespace") })
	g.Expect(assertInNetNS(netns, func(g gomega.Gomega) error {
		link, err := netlink.LinkByName("dummy1")
		if err != nil {
			return err
		}
		stale := netlink.Route{LinkIndex: link.Attrs().Index, Dst: &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
			Gw: net.ParseIP("5.5.5.254"), Table: util.CalculateRouteTableID(link.Attrs().Index), Priority: 100}
		g.Expect(netlink.RouteAdd(&stale)).To(gomega.Succeed(), "install the old route metric")
		_, prefix, err := net.ParseCIDR("203.0.113.0/24")
		if err != nil {
			return err
		}
		independent := stale
		independent.Dst = prefix
		g.Expect(netlink.RouteAdd(&independent)).To(gomega.Succeed(), "install an unrelated stale destination")
		desired := stale
		desired.Priority = 200
		readRoutes := func() []netlink.Route {
			routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: stale.Table}, netlink.RT_FILTER_TABLE)
			g.Expect(err).NotTo(gomega.HaveOccurred(), "read the routing table during repair")
			return routes
		}
		installed := readRoutes()
		g.Expect(installed).To(gomega.HaveLen(2), "read both stale destinations")
		g.Expect(installed).NotTo(gomega.ContainElement(gomega.WithTransform(func(route netlink.Route) bool {
			return routemanager.Matches(desired, route)
		}, gomega.BeTrue())), "a stale metric cannot satisfy route readiness")
		ops := util.GetNetLinkOps()
		util.SetNetLinkOpMockInst(&failingOwnedRoutes{NetLinkOps: ops, failAdd: true})
		defer util.SetNetLinkOpMockInst(ops)
		g.Expect(f.controller.removeStaleIPRoutes([]netlink.Route{stale, independent}, []netlink.Route{desired})).To(gomega.MatchError(gomega.ContainSubstring("injected route add failure")), "report the failed replacement")
		metric := func(route netlink.Route) int { return route.Priority }
		g.Expect(readRoutes()).To(gomega.ConsistOf(gomega.WithTransform(metric, gomega.Equal(100))), "retain the old metric and remove the independent stale route")
		g.Expect(f.controller.removeStaleIPRoutes([]netlink.Route{stale}, []netlink.Route{desired})).To(gomega.Succeed(), "replace the old metric during repair")
		g.Expect(readRoutes()).To(gomega.ConsistOf(gomega.WithTransform(metric, gomega.Equal(200))), "only the desired route metric remains")
		return nil
	})).To(gomega.Succeed(), "repair a real kernel route")
}

func TestStartupKeepsSteeringOnRouteReplacementFailure(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	f, netns, _, _ := setupSelectiveHostOwners(t)
	c := f.controller
	g := gomega.NewWithT(t)
	c.kube = &nodeAnnotationInformer{Interface: c.kube, client: f.client, nodes: f.core.Nodes().Informer().GetStore()}
	g.Expect(assertInNetNS(netns, func(g gomega.Gomega) error {
		link, err := netlink.LinkByName("dummy1")
		if err != nil {
			return err
		}
		old := netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP("5.5.5.254")}
		g.Expect(c.syncEIP("one")).To(gomega.Succeed(), "install selective steering before restart")
		g.Expect(netlink.RouteDel(&old)).To(gomega.Succeed(), "withdraw the default uplink metric")
		newRoute := old
		newRoute.Priority = 200
		g.Expect(netlink.RouteAdd(&newRoute)).To(gomega.Succeed(), "change only the uplink metric")
		checkPath := func(metric int) {
			rules, err := netlink.RuleListFiltered(netlink.FAMILY_V4, &netlink.Rule{Priority: ruleTrafficPriority}, netlink.RT_FILTER_PRIORITY)
			g.Expect(err).NotTo(gomega.HaveOccurred(), "read selective rules during repair")
			g.Expect(rules).To(gomega.HaveLen(1), "keep installed selective steering")
			g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.100"), "keep selective SNAT")
			routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: util.CalculateRouteTableID(link.Attrs().Index)}, netlink.RT_FILTER_TABLE)
			g.Expect(err).NotTo(gomega.HaveOccurred(), "read the installed policy routes")
			routes = slices.DeleteFunc(routes, func(route netlink.Route) bool {
				return route.Gw == nil || route.Dst != nil && route.Dst.String() != "0.0.0.0/0"
			})
			g.Expect(routes).To(gomega.ConsistOf(gomega.HaveField("Priority", metric)), "retain the expected uplink metric")
		}
		restarted := f.newController(t, g)
		ops := util.GetNetLinkOps()
		failed := &failingOwnedRoutes{NetLinkOps: ops, failAdd: true}
		util.SetNetLinkOpMockInst(failed)
		defer util.SetNetLinkOpMockInst(ops)
		g.Expect(restarted.repairNode()).To(gomega.MatchError(gomega.ContainSubstring("injected route add failure")), "report failure before withdrawing working steering")
		checkPath(old.Priority)
		g.Expect(restarted.repairNode()).To(gomega.Succeed(), "replace the old route on retry")
		checkPath(newRoute.Priority)

		_, prefix, err := net.ParseCIDR("203.0.115.0/24")
		if err != nil {
			return err
		}
		specific := netlink.Route{LinkIndex: link.Attrs().Index, Dst: prefix, Gw: old.Gw}
		g.Expect(netlink.RouteAdd(&specific)).To(gomega.Succeed(), "advertise an unrelated prefix during downtime")
		failed.failAdd = true
		g.Expect(restarted.repairNode()).To(gomega.MatchError(gomega.ContainSubstring("injected route add failure")), "try to restore the new prefix before touching steering")
		checkPath(newRoute.Priority)
		g.Expect(restarted.repairNode()).To(gomega.Succeed(), "restore the new prefix on retry")
		checkPath(newRoute.Priority)
		specific.Table = util.CalculateRouteTableID(link.Attrs().Index)
		copied, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &specific, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_DST)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "list the recovered prefix")
		g.Expect(copied).To(gomega.HaveLen(1), "install the newly advertised prefix")
		return nil
	})).To(gomega.Succeed(), "exercise startup metric replacement with installed steering and SNAT")
}

func TestSelectiveActivationFailurePreservesFallback(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	f := newOwnershipTestFixture(t)
	g := gomega.NewWithT(t)
	c := f.controller
	c.kube = &nodeAnnotationInformer{Interface: c.kube, client: f.client, nodes: f.core.Nodes().Informer().GetStore()}
	initial := nodeConfig{linkConfigs: []linkConfig{
		{linkName: "dummy1", address: []address{{cidr: "5.5.5.10/24"}}},
		{linkName: "dummy2", address: []address{{cidr: "6.6.6.10/24"}}},
		{linkName: "pod-in", address: []address{{cidr: "10.128.0.1/24"}}},
	}}
	netns, cleanup, err := setupFakeTestNode(initial)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the two-interface node")
	t.Cleanup(func() { g.Expect(cleanup()).To(gomega.Succeed(), "remove the test namespace") })
	node := getNodeObj(initial, false)
	g.Expect(f.core.Nodes().Informer().GetStore().Add(&node)).To(gomega.Succeed(), "cache the node")
	_, err = f.client.CoreV1().Nodes().Create(context.Background(), &node, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the node")
	g.Expect(f.core.Namespaces().Informer().GetStore().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "work"}})).To(gomega.Succeed(), "cache the workload namespace")
	for i, name := range []string{"main", "peer"} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "work", Labels: map[string]string{"app": name}}, Spec: corev1.PodSpec{NodeName: node1Name}, Status: corev1.PodStatus{PodIP: fmt.Sprintf("10.128.0.%d", 2+i), PodIPs: []corev1.PodIP{{IP: fmt.Sprintf("10.128.0.%d", 2+i)}}}}
		g.Expect(f.core.Pods().Informer().GetStore().Add(pod)).To(gomega.Succeed(), "cache pod %s", name)
	}
	for _, owner := range []struct{ name, ip, pod, cidr string }{
		{"fallback", "5.5.5.100", "main", ""},
		{"peer", "6.6.6.101", "peer", "203.0.114.0/24"},
		{"selective", "6.6.6.102", "main", "203.0.113.0/24"},
	} {
		eip := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: owner.name}, Spec: eipv1.EgressIPSpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": owner.pod}}}, Status: eipv1.EgressIPStatus{Items: []eipv1.EgressIPStatusItem{{Node: node1Name, EgressIP: owner.ip}}}}
		if owner.cidr != "" {
			eip.Spec.TrafficSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"class": owner.name}}
			class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: owner.name, Labels: map[string]string{"class": owner.name}}, Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: owner.cidr}}}}
			g.Expect(f.classes.Informer().GetStore().Add(class)).To(gomega.Succeed(), "cache class %s", owner.name)
		}
		g.Expect(f.eips.Informer().GetStore().Add(eip)).To(gomega.Succeed(), "cache owner %s", owner.name)
	}
	var fallbackLink, selectiveLink int
	err = netns.Do(func(ns.NetNS) error {
		if err := util.SetSysctlNet("ipv4/ip_forward", "1"); err != nil {
			return err
		}
		for i, name := range []string{"dummy1", "dummy2"} {
			link, err := netlink.LinkByName(name)
			if err != nil {
				return err
			}
			if i == 0 {
				fallbackLink = link.Attrs().Index
			} else {
				selectiveLink = link.Attrs().Index
			}
			gateway := []string{"5.5.5.254", "6.6.6.254"}[i]
			if err := netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP(gateway), Priority: i * 100}); err != nil {
				return err
			}
		}
		return nil
	})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "configure both external routes")
	syncOwner := func(name string) error {
		return netns.Do(func(ns.NetNS) error { return c.syncEIP(name) })
	}
	routeLink := func(source, destination string) (int, error) {
		var index int
		err := netns.Do(func(ns.NetNS) error {
			routes, err := netlink.RouteGetWithOptions(net.ParseIP(destination), &netlink.RouteGetOptions{SrcAddr: net.ParseIP(source), Iif: "pod-in"})
			if err != nil {
				return err
			}
			if len(routes) != 1 {
				return fmt.Errorf("route lookup returned %d routes", len(routes))
			}
			index = routes[0].LinkIndex
			return nil
		})
		return index, err
	}
	g.Expect(syncOwner("fallback")).To(gomega.Succeed(), "install the catch-all on the first interface")
	g.Expect(syncOwner("peer")).To(gomega.Succeed(), "populate the second interface's routing table with an independent owner")
	failAnnotation := true
	f.client.PrependReactor("patch", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		if failAnnotation {
			return true, nil, errors.New("injected activation failure")
		}
		return false, nil, nil
	})
	blockedRules := &failingOwnedRules{Interface: c.ruleManager, failDelete: true}
	c.ruleManager = blockedRules
	g.Expect(syncOwner("selective")).To(gomega.MatchError(gomega.ContainSubstring("operation not permitted")), "report the kernel rollback failure")
	g.Expect(c.ruleOwners["selective"]).To(gomega.HaveLen(1), "retain the claim until rollback succeeds")
	failAnnotation = false
	g.Expect(syncOwner("selective")).To(gomega.MatchError(gomega.ContainSubstring("operation not permitted")), "complete rollback before retrying activation")
	g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 6.6.6.102"), "do not activate before rollback completes")
	blockedRules.failDelete = false
	failAnnotation = true
	for range 2 {
		g.Expect(syncOwner("selective")).To(gomega.MatchError(gomega.ContainSubstring("injected activation failure")), "report the failed activation")
		index, err := routeLink("10.128.0.2", "203.0.113.1")
		g.Expect(err).NotTo(gomega.HaveOccurred(), "look up the workload route after failed activation")
		g.Expect(index).To(gomega.Equal(fallbackLink), "keep the catch-all route active during retries")
		g.Expect(assertInNetNS(netns, func(g gomega.Gomega) error {
			rules, err := netlink.RuleListFiltered(netlink.FAMILY_V4, &netlink.Rule{Priority: rulePriority}, netlink.RT_FILTER_PRIORITY)
			g.Expect(err).NotTo(gomega.HaveOccurred(), "read catch-all policy routing rules")
			g.Expect(rules).To(gomega.ContainElement(gomega.WithTransform(
				func(rule netlink.Rule) iprulemanager.IPRule { return iprulemanager.IPRuleFromNetlinkRule(&rule) },
				gomega.Equal(generateIPRule(net.ParseIP("10.128.0.2"), false, fallbackLink)),
			)), "retain the catch-all rule independently of the main-table default")
			return nil
		})).To(gomega.Succeed(), "check the installed catch-all policy rule")

		g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 6.6.6.102"), "do not publish failed selective SNAT")
		g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 6.6.6.101"), "preserve the peer on the populated interface")
	}
	failAnnotation = false
	g.Expect(syncOwner("selective")).To(gomega.Succeed(), "activate the selective owner after recovery")
	index, err := routeLink("10.128.0.2", "203.0.113.1")
	g.Expect(err).NotTo(gomega.HaveOccurred(), "look up the recovered selective route")
	g.Expect(index).To(gomega.Equal(selectiveLink), "steer to the prepared selective interface after activation")
	g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 6.6.6.102"), "publish recovered selective SNAT")

	// Restart with routes and steering intact, but without this owner's egress address.
	err = netns.Do(func(ns.NetNS) error {
		link, err := netlink.LinkByName("dummy2")
		if err != nil {
			return err
		}
		return netlink.AddrDel(link, egressip.GetNetlinkAddress(net.ParseIP("6.6.6.102"), link.Attrs().Index))
	})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "remove only the selective owner's address before restart")
	c = f.newController(t, g)
	c.kube = &nodeAnnotationInformer{Interface: c.kube, client: f.client, nodes: f.core.Nodes().Informer().GetStore()}
	g.Expect(netns.Do(func(ns.NetNS) error { return c.repairNode() })).To(gomega.Succeed(), "repair installed rules before any worker can reinstall them")
	index, err = routeLink("10.128.0.2", "203.0.113.1")
	g.Expect(err).NotTo(gomega.HaveOccurred(), "look up traffic after missing-address startup repair")
	g.Expect(index).To(gomega.Equal(fallbackLink), "do not adopt steering whose selective SNAT cannot be retained")
	g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 6.6.6.102"), "omit SNAT for the missing address")
	g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 6.6.6.101"), "retain the ready peer's SNAT")
	originalOps := util.GetNetLinkOps()
	util.SetNetLinkOpMockInst(&failingEgressAddress{NetLinkOps: originalOps, ip: "6.6.6.102"})
	t.Cleanup(func() { util.SetNetLinkOpMockInst(originalOps) })
	g.Expect(syncOwner("selective")).To(gomega.MatchError(gomega.ContainSubstring("injected address failure")), "report persistent address rejection after startup")
	index, err = routeLink("10.128.0.2", "203.0.113.1")
	g.Expect(err).NotTo(gomega.HaveOccurred(), "look up traffic while address installation is rejected")
	g.Expect(index).To(gomega.Equal(fallbackLink), "keep fallback active during failed reinstallation")
	index, err = routeLink("10.128.0.3", "203.0.114.1")
	g.Expect(err).NotTo(gomega.HaveOccurred(), "look up the healthy peer's traffic")
	g.Expect(index).To(gomega.Equal(selectiveLink), "preserve the ready peer's steering")
	util.SetNetLinkOpMockInst(originalOps)
	g.Expect(syncOwner("selective")).To(gomega.Succeed(), "reinstall steering after the address recovers")
	index, err = routeLink("10.128.0.2", "203.0.113.1")
	g.Expect(err).NotTo(gomega.HaveOccurred(), "look up the recovered owner after restart")
	g.Expect(index).To(gomega.Equal(selectiveLink), "activate recovered selective steering")

	selected, err := f.eips.Lister().Get("selective")
	g.Expect(err).NotTo(gomega.HaveOccurred(), "read the recovered owner before deletion")
	g.Expect(f.eips.Informer().GetStore().Delete(selected)).To(gomega.Succeed(), "retire the selective owner")
	rules := &failingOwnedRules{Interface: c.ruleManager, failDelete: true}
	c.ruleManager = rules
	g.Expect(syncOwner("selective")).To(gomega.MatchError(gomega.ContainSubstring("failed to delete IP rule")), "report failed steering removal")
	index, err = routeLink("10.128.0.2", "203.0.113.1")
	g.Expect(err).NotTo(gomega.HaveOccurred(), "look up the retained path after the failed delete")
	g.Expect(index).To(gomega.Equal(selectiveLink), "the failed deletion leaves selective steering installed")
	g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 6.6.6.102"), "keep selective SNAT usable until its steering is removed")
	rules.failDelete = false
	g.Expect(syncOwner("selective")).To(gomega.Succeed(), "complete teardown on owner retry")
	index, err = routeLink("10.128.0.2", "203.0.113.1")
	g.Expect(err).NotTo(gomega.HaveOccurred(), "look up the fallback after successful teardown")
	g.Expect(index).To(gomega.Equal(fallbackLink), "use the catch-all after withdrawal")
	g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 6.6.6.102"), "withdraw retired selective SNAT")
}
