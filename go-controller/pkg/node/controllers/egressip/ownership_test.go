// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
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
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes/fake"
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
	controller *Controller
	client     *fake.Clientset
	core       coreinformers.Interface
	eips       eipinformer.EgressIPInformer
	classes    trafficinformer.TrafficClassInformer
	nft        *knftables.Fake
}

func newOwnershipTestFixture(t *testing.T) *ownershipTestFixture {
	t.Helper()
	g := gomega.NewWithT(t)
	g.Expect(ovnconfig.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() { _ = ovnconfig.PrepareTestConfig() })
	ovnconfig.OVNKubernetesFeature.EnableTrafficClass = true
	_, clusterCIDR, _ := net.ParseCIDR("10.128.0.0/14")
	ovnconfig.Default.ClusterSubnets = []ovnconfig.CIDRNetworkEntry{{CIDR: clusterCIDR}}
	f := &ownershipTestFixture{client: fake.NewSimpleClientset()}
	f.core = informers.NewSharedInformerFactory(f.client, 0).Core().V1()
	f.eips = eipinformers.NewSharedInformerFactory(eipfake.NewSimpleClientset(), 0).K8s().V1().EgressIPs()
	f.classes = trafficinformers.NewSharedInformerFactory(trafficfake.NewSimpleClientset(), 0).K8s().V1().TrafficClasses()
	f.nft = nodenft.SetFakeNFTablesHelper()
	f.controller = f.newController(t)
	return f
}

func (f *ownershipTestFixture) newController(t *testing.T) *Controller {
	t.Helper()
	g := gomega.NewWithT(t)
	c, err := NewController(&kube.Kube{KClient: f.client}, f.eips, f.classes, f.core.Nodes().Informer(), f.core.Namespaces(), f.core.Pods(),
		func(string) (util.NetInfo, error) { return &util.DefaultNetInfo{}, nil }, routemanager.NewController(), true, false, node1Name, linkmanager.NewController(node1Name, true, false, nil))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(c.initNFTables()).To(gomega.Succeed())
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

// Pause after the API write and informer update, outside the fake client's lock.
// A second owner's reconciliation must finish before the first one resumes.
type pausedNodeAnnotation struct {
	kube.Interface
	client  *fake.Clientset
	nodes   cache.Store
	entered chan struct{}
	release chan struct{}
	paused  atomic.Bool
}

func (k *pausedNodeAnnotation) PatchNodeStatusAnnotations(ctx context.Context, oldNode, newNode *corev1.Node) error {
	if err := k.Interface.PatchNodeStatusAnnotations(ctx, oldNode, newNode); err != nil {
		return err
	}
	updated, err := k.client.CoreV1().Nodes().Get(ctx, newNode.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if err := k.nodes.Update(updated); err != nil {
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
	g.Expect(err).NotTo(gomega.HaveOccurred())
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
	g.Expect(f.core.Nodes().Informer().GetStore().Add(&node)).To(gomega.Succeed())
	_, err = f.client.CoreV1().Nodes().Create(context.Background(), &node, metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(f.core.Namespaces().Informer().GetStore().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "work"}})).To(gomega.Succeed())
	g.Expect(f.core.Pods().Informer().GetStore().Add(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "work"}, Spec: corev1.PodSpec{NodeName: node1Name}, Status: corev1.PodStatus{PodIP: "10.128.0.2", PodIPs: []corev1.PodIP{{IP: "10.128.0.2"}}}})).To(gomega.Succeed())
	owners := make(map[string]*eipv1.EgressIP)
	classes := make(map[string]*trafficv1.TrafficClass)
	for i, name := range []string{"one", "two"} {
		owner := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"class": name}}}, Status: eipv1.EgressIPStatus{Items: []eipv1.EgressIPStatusItem{{Node: node1Name, EgressIP: fmt.Sprintf("5.5.5.%d", 100+i)}}}}
		class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"class": name}}, Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: fmt.Sprintf("203.0.%d.0/24", 113+i)}}}}
		g.Expect(f.eips.Informer().GetStore().Add(owner)).To(gomega.Succeed())
		g.Expect(f.classes.Informer().GetStore().Add(class)).To(gomega.Succeed())
		owners[name], classes[name] = owner, class
	}
	paused := &pausedNodeAnnotation{Interface: c.kube, client: f.client, nodes: f.core.Nodes().Informer().GetStore(), entered: make(chan struct{}), release: make(chan struct{})}
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
		g.Expect(err).NotTo(gomega.HaveOccurred())
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
		if err := c.ruleManager.OwnPriority(ruleTrafficPriority); err != nil {
			return err
		}
		if err := c.ruleManager.OwnPriority(rulePriority); err != nil {
			return err
		}
		if err := c.repairNode(); err != nil {
			return err
		}
		if err := f.eips.Informer().GetStore().Delete(owners["one"]); err != nil {
			return err
		}
		return c.syncEIP("one")
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(f.nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.100"))
	g.Expect(f.nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"))
	g.Expect(c.ruleOwners).NotTo(gomega.HaveKey("one"))
	g.Expect(c.routeOwners).NotTo(gomega.HaveKey("one"))
	// An offline matcher edit must prune the repaired owner's old rule on its first event.
	classes["two"].Spec.Matches[0].DestinationCIDR = "203.0.115.0/24"
	g.Expect(f.classes.Informer().GetStore().Update(classes["two"])).To(gomega.Succeed())
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
	g.Expect(err).NotTo(gomega.HaveOccurred())
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
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(func() { g.Expect(cleanup()).To(gomega.Succeed()) })
	err = netns.Do(func(ns.NetNS) error {
		rule := iprulemanager.IPRule{Priority: ruleTrafficPriority, Table: 1007, Family: netlink.FAMILY_V4, Src: netip.MustParsePrefix("10.128.0.2/32")}
		invalid := rule
		invalid.Family = unix.AF_UNIX
		invalid.Src = netip.Prefix{}
		g.Expect(c.addOwnedIPRule("one", invalid)).NotTo(gomega.Succeed())
		g.Expect(c.ruleOwners["one"].Has(invalid)).To(gomega.BeFalse())
		g.Expect(c.addOwnedIPRule("one", rule)).To(gomega.Succeed())
		g.Expect(c.addOwnedIPRule("one", rule)).To(gomega.Succeed())
		g.Expect(c.addOwnedIPRule("two", rule)).To(gomega.Succeed())
		g.Expect(c.deleteOwnedIPRule("one", rule)).To(gomega.Succeed())
		g.Expect(c.pruneOwnedResources("two", nil)).To(gomega.Succeed())
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
		g.Expect(c.addOwnedRoute("one", route)).To(gomega.Succeed())
		g.Expect(c.addOwnedRoute("one", route)).To(gomega.Succeed())
		routes.failDelete = true
		g.Expect(c.deleteOwnedRoute("one", route)).To(gomega.MatchError(gomega.ContainSubstring("injected route delete failure")))
		g.Expect(c.routeOwners["one"]).To(gomega.HaveLen(1))
		g.Expect(c.addOwnedRoute("two", route)).To(gomega.Succeed())
		g.Expect(c.deleteOwnedRoute("one", route)).To(gomega.Succeed())
		g.Expect(c.pruneOwnedResources("two", nil)).To(gomega.Succeed())
		g.Expect(c.routeOwners).To(gomega.BeEmpty())
		actualRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: route.Table}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return err
		}
		g.Expect(actualRoutes).To(gomega.BeEmpty())
		g.Expect(c.addOwnedIPRule("one", rule)).To(gomega.Succeed())
		g.Expect(c.addOwnedRoute("one", route)).To(gomega.Succeed())
		otherRoute := route
		otherRoute.Table++
		g.Expect(c.addOwnedRoute("one", otherRoute)).To(gomega.Succeed())
		routes.failDelete = true
		pruneErr := c.pruneOwnedResources("one", nil)
		g.Expect(pruneErr).To(gomega.MatchError(gomega.ContainSubstring("injected route delete failure")))
		g.Expect(c.ruleOwners).To(gomega.BeEmpty())
		g.Expect(c.routeOwners["one"]).To(gomega.Equal([]netlink.Route{route}))
		g.Expect(c.pruneOwnedResources("one", nil)).To(gomega.Succeed())
		g.Expect(c.ruleOwners).To(gomega.BeEmpty())
		g.Expect(c.routeOwners).To(gomega.BeEmpty())
		return nil
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
}
