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
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/egressip"
)

type ownershipTestFixture struct {
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
	g := gomega.NewWithT(t)
	g.Expect(ovnconfig.PrepareTestConfig()).To(gomega.Succeed(), "prepare test configuration")
	t.Cleanup(func() { _ = ovnconfig.PrepareTestConfig() })
	ovnconfig.OVNKubernetesFeature.EnableTrafficClass = true
	_, clusterCIDR, _ := net.ParseCIDR("10.128.0.0/14")
	ovnconfig.Default.ClusterSubnets = []ovnconfig.CIDRNetworkEntry{{CIDR: clusterCIDR}}
	f := &ownershipTestFixture{client: fake.NewSimpleClientset(), eipClient: eipfake.NewSimpleClientset(), trafficClient: trafficfake.NewSimpleClientset()}
	f.core = informers.NewSharedInformerFactory(f.client, 0).Core().V1()
	f.eips = eipinformers.NewSharedInformerFactory(f.eipClient, 0).K8s().V1().EgressIPs()
	f.classes = trafficinformers.NewSharedInformerFactory(f.trafficClient, 0).K8s().V1().TrafficClasses()
	f.nft = nodenft.SetFakeNFTablesHelper()
	f.controller = f.newController(t)
	return f
}

func (f *ownershipTestFixture) newController(t *testing.T) *Controller {
	t.Helper()
	g := gomega.NewWithT(t)
	c, err := NewController(&kube.Kube{KClient: f.client}, f.eips, f.classes, f.core.Nodes().Informer(), f.core.Namespaces(), f.core.Pods(),
		func(string) (util.NetInfo, error) { return &util.DefaultNetInfo{}, nil }, routemanager.NewController(), true, false, node1Name, linkmanager.NewController(node1Name, true, false, nil))
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the host EgressIP controller")
	g.Expect(c.initNFTables()).To(gomega.Succeed(), "initialize EgressIP nftables chains")
	t.Cleanup(func() {
		c.eIPQueue.ShutDown()
		c.podQueue.ShutDown()
		c.namespaceQueue.ShutDown()
	})
	return c
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
			t.Error(err)
		}
	})
	var route netlink.Route
	rule := iprulemanager.IPRule{Priority: ruleTrafficPriority, Table: 1007, Family: netlink.FAMILY_V4, Src: netip.MustParsePrefix("10.128.0.2/32")}
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
		for i := 0; i < count; i++ {
			go func(owner string) {
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
	for owner, applied := range states {
		go func() { <-start; results <- publish(owner, applied) }()
	}
	close(start)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for range states {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("publish concurrent SNAT owners: %v", err)
			}
		case <-deadline.C:
			t.Fatal("timed out publishing concurrent SNAT owners")
		}
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
	if strings.Contains(f.nft.Dump(), "5.5.5.102") {
		t.Fatal("peer publication read mutable owner state")
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

func TestConcurrentHostOwnerReconciliation(t *testing.T) {
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
	// Startup claims must survive one owner being deleted before its first worker pass.
	err = netns.Do(func(ns.NetNS) error {
		c = f.newController(t)
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
	// An offline matcher edit must prune the repaired owner's old rule on its first event.
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

// Failed route operations must leave ownership retryable.
type failingOwnedRoutes struct {
	util.NetLinkOps
	failAdd, failDelete bool
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
	err = netns.Do(func(ns.NetNS) error {
		rule := iprulemanager.IPRule{Priority: ruleTrafficPriority, Table: 1007, Family: netlink.FAMILY_V4, Src: netip.MustParsePrefix("10.128.0.2/32")}
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
		deleteErr := m.Interface.Delete(rule)
		return errors.Join(deleteErr, unix.Capset(&header, &original[0]))
	}
	return m.Interface.Delete(rule)
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
	err = netns.Do(func(ns.NetNS) error {
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
		g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.101"), "withdraw SNAT before the rule manager can finish a deferred delete")
		// No EgressIP retries run here: let the real rule manager finish its pending deletion.
		stopRules := make(chan struct{})
		rulesDone := make(chan error, 1)
		go func() {
			rulesDone <- netns.Do(func(ns.NetNS) error { rules.Run(stopRules, time.Millisecond); return nil })
		}()
		var stopRulesOnce sync.Once
		defer stopRulesOnce.Do(func() { close(stopRules) })
		g.Eventually(func() ([]netlink.Rule, error) {
			return netlink.RuleListFiltered(netlink.FAMILY_V4, &netlink.Rule{Priority: ruleTrafficPriority}, netlink.RT_FILTER_PRIORITY)
		}, "5s").Should(gomega.BeEmpty(), "the real manager must complete its deferred deletion")
		g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.101"))
		cached, ok := c.cache.Load(selective.Name)
		g.Expect(ok).To(gomega.BeTrue())
		entries := cached.namespacesWithPodIPConfigs["work"][ktypes.NamespacedName{Namespace: "work", Name: "pod"}].elems
		g.Expect(entries).To(gomega.HaveLen(1))
		g.Expect(entries[0].failed && !entries[0].snatReady).To(gomega.BeTrue(), "retain retry state after withdrawing selective SNAT")
		stopRulesOnce.Do(func() { close(stopRules) })
		g.Eventually(rulesDone, "5s").Should(gomega.Receive(gomega.Succeed()))
		rules.failDelete = false
		g.Expect(c.syncEIP(selective.Name)).To(gomega.Succeed(), "retry selective teardown")
		g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.101"))
		g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("5.5.5.100"))
		// Desired configuration alone must not activate SNAT during startup repair either.
		g.Expect(f.eips.Informer().GetStore().Add(selective)).To(gomega.Succeed())
		restarted := f.newController(t)
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
		retained := f.newController(t)
		g.Expect(retained.repairNode()).To(gomega.Succeed())
		g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"), "retain confirmed startup state")
		retained.ruleManager = &failingOwnedRules{Interface: retained.ruleManager, failDelete: true}
		g.Expect(f.eips.Informer().GetStore().Delete(selective)).To(gomega.Succeed())
		g.Expect(retained.syncEIP(selective.Name)).To(gomega.MatchError(gomega.ContainSubstring("failed to delete IP rule")))
		g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.101"))
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
	f := newOwnershipTestFixture(t)
	g := gomega.NewWithT(t)
	initial := nodeConfig{linkConfigs: []linkConfig{{linkName: "dummy1", address: []address{{cidr: "5.5.5.10/24"}}}}}
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
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: namespace.Name}, Spec: corev1.PodSpec{NodeName: node1Name}, Status: corev1.PodStatus{PodIP: "10.128.0.2", PodIPs: []corev1.PodIP{{IP: "10.128.0.2"}}}}
	_, err = f.client.CoreV1().Namespaces().Create(ctx, namespace, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = f.client.CoreV1().Pods(namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(f.core.Namespaces().Informer().GetStore().Add(namespace)).To(gomega.Succeed())
	g.Expect(f.core.Pods().Informer().GetStore().Add(pod)).To(gomega.Succeed())
	owners := []string{"one", "two"}
	for i, name := range owners {
		owner := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"class": name}}}, Status: eipv1.EgressIPStatus{Items: []eipv1.EgressIPStatusItem{{Node: node1Name, EgressIP: fmt.Sprintf("5.5.5.%d", 100+i)}}}}
		class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"class": name}}, Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: fmt.Sprintf("203.0.%d.0/24", 113+i)}}}}
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
		if err := netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP("5.5.5.254")}); err != nil {
			return err
		}
		for _, owner := range owners {
			if err := f.controller.syncEIP(owner); err != nil {
				return err
			}
		}
		rules, err := netlink.RuleListFiltered(netlink.FAMILY_V4, &netlink.Rule{Priority: ruleTrafficPriority}, netlink.RT_FILTER_PRIORITY)
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
	restarted := f.newController(t)
	err = netns.Do(func(ns.NetNS) error {
		// Exercise Run's real startup, without workers that could hide a delete/reinstall cycle.
		if err := restarted.Run(ctx.Done(), &workers, 0); err != nil {
			return err
		}
		rules, err := netlink.RuleListFiltered(netlink.FAMILY_V4, &netlink.Rule{Priority: ruleTrafficPriority}, netlink.RT_FILTER_PRIORITY)
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
			g.Expect(found).To(gomega.BeTrue())
			entries := applied.namespacesWithPodIPConfigs["work"][ktypes.NamespacedName{Namespace: "work", Name: "pod"}].elems
			g.Expect(entries).To(gomega.HaveLen(1))
			g.Expect(entries[0].snatReady).To(gomega.BeTrue())
			g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to " + entries[0].snatValue))
		}
		// A later startup cleanup failure must not leave the retired owner's SNAT active.
		g.Expect(f.eipClient.K8sV1().EgressIPs().Delete(ctx, owners[0], metav1.DeleteOptions{})).To(gomega.Succeed())
		g.Eventually(func() ([]*eipv1.EgressIP, error) { return f.eips.Lister().List(labels.Everything()) }, "5s").Should(gomega.HaveLen(1))
		original := util.GetNetLinkOps()
		rejected := &failedStartupAddressCleanup{NetLinkOps: original, nft: f.nft, ip: "5.5.5.100"}
		util.SetNetLinkOpMockInst(rejected)
		defer util.SetNetLinkOpMockInst(original)
		g.Expect(restarted.repairNode()).To(gomega.MatchError(gomega.ContainSubstring("injected startup address cleanup failure")))
		g.Expect(rejected.snatWasActive).To(gomega.BeFalse(), "withdraw SNAT before attempting address teardown")
		g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.100"))
		g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"), "preserve the healthy peer through cleanup failure")
		return nil
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

type failedStartupAddressCleanup struct {
	util.NetLinkOps
	nft           *knftables.Fake
	ip            string
	snatWasActive bool
}

func (m *failedStartupAddressCleanup) AddrDel(link netlink.Link, address *netlink.Addr) error {
	if address.IP.String() == m.ip {
		m.snatWasActive = strings.Contains(m.nft.Dump(), "snat ip to "+m.ip)
		return errors.New("injected startup address cleanup failure")
	}
	return m.NetLinkOps.AddrDel(link, address)
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
	for range 2 {
		g.Expect(syncOwner("selective")).To(gomega.MatchError(gomega.ContainSubstring("injected activation failure")), "report the failed activation")
		index, err := routeLink("10.128.0.2", "203.0.113.1")
		g.Expect(err).NotTo(gomega.HaveOccurred(), "look up the workload route after failed activation")
		g.Expect(index).To(gomega.Equal(fallbackLink), "keep the catch-all route active during retries")
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
	c = f.newController(t)
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
}
