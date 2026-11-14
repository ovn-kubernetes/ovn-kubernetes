// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/knftables"

	ovnconfig "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	eipfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	eipinformers "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/informers/externalversions"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1"
	trafficfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1/apis/clientset/versioned/fake"
	trafficinformers "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1/apis/informers/externalversions"
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

func TestSelectiveNodeOwnerLifecycle(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	g := gomega.NewWithT(t)
	g.Expect(ovnconfig.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() { _ = ovnconfig.PrepareTestConfig() })
	ovnconfig.OVNKubernetesFeature.EnableTrafficClass = true
	_, clusterCIDR, _ := net.ParseCIDR("10.128.0.0/14")
	ovnconfig.Default.ClusterSubnets = []ovnconfig.CIDRNetworkEntry{{CIDR: clusterCIDR}}
	initial := nodeConfig{linkConfigs: []linkConfig{
		{linkName: "dummy1", address: []address{{cidr: "5.5.5.10/24"}}},
		{linkName: "dummy2", address: []address{{cidr: "6.6.6.10/24"}}},
	}}
	netns, cleanup, err := setupFakeTestNode(initial)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(func() { g.Expect(cleanup()).To(gomega.Succeed()) })
	err = netns.Do(func(ns.NetNS) error {
		link, err := netlink.LinkByName("dummy1")
		if err != nil {
			return err
		}
		if err = netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP("5.5.5.254")}); err != nil {
			return err
		}
		secondLink, err := netlink.LinkByName("dummy2")
		if err != nil {
			return err
		}
		if err = netlink.RouteAdd(&netlink.Route{LinkIndex: secondLink.Attrs().Index, Gw: net.ParseIP("6.6.6.254"), Priority: 100}); err != nil {
			return err
		}
		node := getNodeObj(initial, false)
		client := fake.NewSimpleClientset(&node)
		core := informers.NewSharedInformerFactory(client, 0).Core().V1()
		eips := eipinformers.NewSharedInformerFactory(eipfake.NewSimpleClientset(), 0).K8s().V1().EgressIPs()
		classes := trafficinformers.NewSharedInformerFactory(trafficfake.NewSimpleClientset(), 0).K8s().V1().TrafficClasses()
		g.Expect(core.Nodes().Informer().GetStore().Add(&node)).To(gomega.Succeed())
		g.Expect(core.Namespaces().Informer().GetStore().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "work"}})).To(gomega.Succeed())
		g.Expect(core.Pods().Informer().GetStore().Add(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "work"}, Spec: corev1.PodSpec{NodeName: node1Name}, Status: corev1.PodStatus{PodIP: "10.128.0.2", PodIPs: []corev1.PodIP{{IP: "10.128.0.2"}}}})).To(gomega.Succeed())
		one := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "one"}, Spec: eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"class": "one"}}}, Status: eipv1.EgressIPStatus{Items: []eipv1.EgressIPStatusItem{{Node: node1Name, EgressIP: "5.5.5.100"}}}}
		two := one.DeepCopy()
		two.Name = "two"
		two.Spec.TrafficSelector.MatchLabels["class"] = "two"
		two.Status.Items[0].EgressIP = "5.5.5.101"
		class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "one", Labels: map[string]string{"class": "one"}}, Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: "203.0.113.0/24", Protocols: []trafficv1.ProtocolMatch{{UDP: &trafficv1.PortProtocol{DestinationPort: &trafficv1.Port{Number: ptr.To[int32](5060)}}}}}}}}
		peerClass := class.DeepCopy()
		peerClass.Name = "two"
		peerClass.Labels["class"] = "two"
		peerClass.Spec.Matches[0].Protocols = []trafficv1.ProtocolMatch{{TCP: &trafficv1.PortProtocol{DestinationPort: &trafficv1.Port{Number: ptr.To[int32](22)}}}}
		for _, owner := range []*eipv1.EgressIP{one, two} {
			g.Expect(eips.Informer().GetStore().Add(owner)).To(gomega.Succeed())
		}
		for _, tc := range []*trafficv1.TrafficClass{class, peerClass} {
			g.Expect(classes.Informer().GetStore().Add(tc)).To(gomega.Succeed())
		}
		nft := nodenft.SetFakeNFTablesHelper()
		refreshNode := func() {
			t.Helper()
			updated, err := client.CoreV1().Nodes().Get(context.Background(), node1Name, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(core.Nodes().Informer().GetStore().Update(updated)).To(gomega.Succeed())
		}
		newController := func() *Controller {
			refreshNode()
			c, err := NewController(&kube.Kube{KClient: client}, eips, classes, core.Nodes().Informer(), core.Namespaces(), core.Pods(), func(string) (util.NetInfo, error) { return &util.DefaultNetInfo{}, nil }, routemanager.NewController(), true, false, node1Name, linkmanager.NewController(node1Name, true, false, nil))
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(c.initNFTables()).To(gomega.Succeed())
			g.Expect(c.ruleManager.OwnPriority(ruleTrafficPriority)).To(gomega.Succeed())
			g.Expect(c.ruleManager.OwnPriority(rulePriority)).To(gomega.Succeed())
			t.Cleanup(func() { c.eIPQueue.ShutDown(); c.podQueue.ShutDown(); c.namespaceQueue.ShutDown() })
			return c
		}
		c := newController()
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		g.Expect(c.syncEIP("two")).To(gomega.Succeed())
		checkRoute := func(targetLink netlink.Link, cidr string, present bool) {
			t.Helper()
			routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: util.CalculateRouteTableID(targetLink.Attrs().Index)}, netlink.RT_FILTER_TABLE)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			found := false
			for _, route := range routes {
				if (route.Dst == nil && cidr == "0.0.0.0/0") || (route.Dst != nil && route.Dst.String() == cidr) {
					found = true
				}
			}
			g.Expect(found).To(gomega.Equal(present), "route %s", cidr)
		}
		checkRoute(link, "0.0.0.0/0", true)
		g.Expect(nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.100"))
		g.Expect(nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"))
		for _, field := range []string{"namespace", "pod"} {
			invalid := one.DeepCopy()
			selector := metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: "Invalid"}}}
			if field == "namespace" {
				invalid.Spec.NamespaceSelector = selector
			} else {
				invalid.Spec.PodSelector = selector
			}
			g.Expect(eips.Informer().GetStore().Update(invalid)).To(gomega.Succeed())
			g.Expect(c.syncEIP("two")).To(gomega.Succeed())
			g.Expect(c.syncEIP("one")).To(gomega.Succeed())
			g.Expect(nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.100"))
			g.Expect(nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"))
			g.Expect(c.repairNode()).To(gomega.Succeed())
			g.Expect(nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.100"))
			g.Expect(eips.Informer().GetStore().Update(one)).To(gomega.Succeed())
			g.Expect(c.syncEIP("one")).To(gomega.Succeed())
			g.Expect(nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.100"))
		}
		checkRule := func(expected iprulemanager.IPRule, present bool) {
			t.Helper()
			rules, err := netlink.RuleList(netlink.FAMILY_V4)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			count := 0
			for _, rule := range rules {
				if iprulemanager.IPRuleFromNetlinkRule(&rule) == expected {
					count++
				}
			}
			if present {
				g.Expect(count).To(gomega.Equal(1), "shared policy rule must survive exactly once")
			} else {
				g.Expect(count).To(gomega.BeZero(), "last owner must remove the policy rule")
			}
		}
		originalProtocols := peerClass.DeepCopy().Spec.Matches[0].Protocols
		peerClass.Spec.Matches[0].Protocols = class.DeepCopy().Spec.Matches[0].Protocols
		g.Expect(classes.Informer().GetStore().Update(peerClass)).To(gomega.Succeed())
		g.Expect(c.syncEIP("two")).To(gomega.Succeed())
		g.Expect(nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.101"))
		state, _ := c.cache.Load("one")
		sharedRule := state.namespacesWithPodIPConfigs["work"][getPodNamespacedName(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "work", Name: "pod"}})].elems[0].ipRule
		checkRule(sharedRule, true)
		// Resolving an overlap by changing one owner must preserve the other's rule.
		class.Spec.Matches[0].DestinationCIDR = "203.0.114.0/24"
		g.Expect(classes.Informer().GetStore().Update(class)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		g.Expect(c.syncEIP("two")).To(gomega.Succeed())
		checkRule(sharedRule, true)
		class.Spec.Matches[0].DestinationCIDR = "203.0.113.0/24"
		g.Expect(classes.Informer().GetStore().Update(class)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		// A rejected owner recovers when its conflicting winner is deleted.
		g.Expect(eips.Informer().GetStore().Delete(one)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		g.Expect(c.syncEIP("two")).To(gomega.Succeed())
		checkRule(sharedRule, true)
		peerClass.Spec.Matches[0].Protocols = originalProtocols
		g.Expect(classes.Informer().GetStore().Update(peerClass)).To(gomega.Succeed())
		g.Expect(c.syncEIP("two")).To(gomega.Succeed())
		checkRule(sharedRule, false)
		g.Expect(eips.Informer().GetStore().Add(one)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		// An invalid stored selector removes its old datapath without affecting peers.
		invalid := one.DeepCopy()
		invalid.Spec.TrafficSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "class", Operator: "Invalid"}}}
		g.Expect(eips.Informer().GetStore().Update(invalid)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		checkRule(sharedRule, false)
		g.Expect(nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.100"))
		g.Expect(nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"))
		g.Expect(c.repairNode()).To(gomega.Succeed())
		g.Expect(eips.Informer().GetStore().Update(one)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		class.Spec.Matches[0].DestinationCIDR = "203.0.114.0/24"
		g.Expect(classes.Informer().GetStore().Update(class)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		checkRoute(link, "0.0.0.0/0", true)
		checkRoute(link, "203.0.114.0/24", false)
		// Moving one owner must preserve the peer's old table, including across a failed API write.
		rejectPatch := true
		client.PrependReactor("patch", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
			if rejectPatch {
				rejectPatch = false
				return true, nil, errors.New("temporary annotation failure")
			}
			return false, nil, nil
		})
		one.Status.Items[0].EgressIP = "6.6.6.100"
		g.Expect(eips.Informer().GetStore().Update(one)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).NotTo(gomega.Succeed())
		g.Expect(nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"))
		netlinkOps := util.GetNetLinkOps()
		defer util.SetNetLinkOpMockInst(netlinkOps)
		util.SetNetLinkOpMockInst(&failingTrafficRouteOps{NetLinkOps: netlinkOps, linkIndex: secondLink.Attrs().Index})
		g.Expect(c.syncEIP("one")).To(gomega.MatchError(gomega.ContainSubstring("injected interface route failure")))
		checkRoute(link, "5.5.5.0/24", true)
		checkRoute(secondLink, "203.0.114.0/24", false)
		applied, _ := c.cache.Load("one")
		g.Expect(applied.eIPConfig.routes).NotTo(gomega.BeEmpty())
		for _, route := range applied.eIPConfig.routes {
			g.Expect(route.LinkIndex).To(gomega.Equal(secondLink.Attrs().Index))
		}
		// Deletion after a partial move must remove the routes already installed on the new link.
		g.Expect(eips.Informer().GetStore().Delete(one)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		checkRoute(secondLink, "6.6.6.0/24", false)
		checkRoute(link, "5.5.5.0/24", true)
		checkRoute(link, "0.0.0.0/0", true)
		one.Status.Items[0].EgressIP = "5.5.5.100"
		g.Expect(eips.Informer().GetStore().Add(one)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		one.Status.Items[0].EgressIP = "6.6.6.100"
		g.Expect(eips.Informer().GetStore().Update(one)).To(gomega.Succeed())
		util.SetNetLinkOpMockInst(&failingTrafficRouteOps{NetLinkOps: netlinkOps, linkIndex: secondLink.Attrs().Index})
		g.Expect(c.syncEIP("one")).To(gomega.MatchError(gomega.ContainSubstring("injected interface route failure")))
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		checkRoute(link, "5.5.5.0/24", true)
		checkRoute(link, "0.0.0.0/0", true)
		checkRoute(link, "203.0.114.0/24", false)
		checkRoute(secondLink, "0.0.0.0/0", true)
		g.Expect(nft.Dump()).To(gomega.ContainSubstring("snat ip to 6.6.6.100"))
		g.Expect(nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.100"))
		g.Expect(eips.Informer().GetStore().Delete(one)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		checkRoute(link, "0.0.0.0/0", true)
		checkRoute(secondLink, "203.0.114.0/24", false)
		g.Expect(nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 6.6.6.100"))
		g.Expect(nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"))
		updatedNode, err := client.CoreV1().Nodes().Get(context.Background(), node1Name, metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(core.Nodes().Informer().GetStore().Update(updatedNode)).To(gomega.Succeed())
		// Rebuild destination rules and SNAT after an offline matcher edit.
		peerClass.Spec.Matches[0].Protocols[0].TCP.DestinationPort.Number = ptr.To[int32](80)
		g.Expect(classes.Informer().GetStore().Update(peerClass)).To(gomega.Succeed())
		restarted := newController()
		g.Expect(restarted.repairNode()).To(gomega.Succeed())
		g.Expect(restarted.syncEIP("two")).To(gomega.Succeed())
		rules, err := netlink.RuleList(netlink.FAMILY_V4)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		var ports []uint16
		for _, rule := range rules {
			if rule.Priority == ruleTrafficPriority && rule.Dport != nil {
				ports = append(ports, rule.Dport.Start)
			}
		}
		g.Expect(ports).To(gomega.Equal([]uint16{80}))
		g.Expect(nft.Dump()).To(gomega.ContainSubstring("tcp dport 80"))
		g.Expect(nft.Dump()).NotTo(gomega.ContainSubstring("tcp dport 22"))
		// Deliver node annotation writes before the next event, as a running informer would.
		syncOwner := func(controller *Controller, name string) {
			t.Helper()
			refreshNode()
			g.Expect(controller.syncEIP(name)).To(gomega.Succeed())
		}
		// Selector transitions must switch between filtered rules and catch-all maps.
		catchAll := two.DeepCopy()
		catchAll.Spec.TrafficSelector = nil
		g.Expect(eips.Informer().GetStore().Update(catchAll)).To(gomega.Succeed())
		syncOwner(restarted, two.Name)
		checkCatchAll := func(expected string) {
			t.Helper()
			elements, err := nft.ListElements(context.Background(), "map", types.EgressIPNFTablesMapV4)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			if expected == "" {
				g.Expect(elements).To(gomega.BeEmpty())
				return
			}
			g.Expect(elements).To(gomega.HaveLen(1))
			g.Expect(elements[0].Key).To(gomega.Equal([]string{"10.128.0.2", "dummy1"}))
			g.Expect(elements[0].Value).To(gomega.Equal([]string{expected}))
		}
		checkCatchAll("5.5.5.101")
		g.Expect(nft.Dump()).NotTo(gomega.ContainSubstring("tcp dport 80"))
		g.Expect(eips.Informer().GetStore().Update(two)).To(gomega.Succeed())
		syncOwner(restarted, two.Name)
		checkCatchAll("")
		g.Expect(nft.Dump()).To(gomega.ContainSubstring("tcp dport 80"))
		// A catch-all must survive disabling TrafficClass on the next controller startup.
		fallback := catchAll.DeepCopy()
		fallback.Name = "fallback"
		fallback.Status.Items[0].EgressIP = "5.5.5.102"
		g.Expect(eips.Informer().GetStore().Add(fallback)).To(gomega.Succeed())
		syncOwner(restarted, fallback.Name)
		g.Expect(nft.Dump()).To(gomega.ContainSubstring("tcp dport 80"))
		// Leave enabled programming behind, then restart without a TrafficClass informer.
		ovnconfig.OVNKubernetesFeature.EnableTrafficClass = false
		classes = nil
		disabled := newController()
		g.Expect(disabled.repairNode()).To(gomega.Succeed())
		syncOwner(disabled, two.Name)
		syncOwner(disabled, fallback.Name)
		checkCatchAll("5.5.5.102")
		g.Expect(nft.Dump()).NotTo(gomega.ContainSubstring(selectiveSNATChain))
		checkRoute(link, "0.0.0.0/0", true)
		rules, err = netlink.RuleList(netlink.FAMILY_V4)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		for _, rule := range rules {
			g.Expect(rule.Priority).NotTo(gomega.Equal(ruleTrafficPriority))
		}
		return nil
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

type failingTrafficRouteOps struct {
	util.NetLinkOps
	linkIndex    int
	replacements int
}

func (ops *failingTrafficRouteOps) RouteReplace(route *netlink.Route) error {
	if route.LinkIndex == ops.linkIndex {
		ops.replacements++
		if ops.replacements == 2 {
			return errors.New("injected interface route failure")
		}
	}
	return ops.NetLinkOps.RouteReplace(route)
}

func TestSelectiveSNATAndRules(t *testing.T) {
	before := ovnconfig.OVNKubernetesFeature.EnableTrafficClass
	t.Cleanup(func() { ovnconfig.OVNKubernetesFeature.EnableTrafficClass = before })
	ovnconfig.OVNKubernetesFeature.EnableTrafficClass = true
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth-mgmt", Index: 7}}
	dest := egressip.Destination{CIDR: netip.MustParsePrefix("192.0.2.0/24"), Protocol: egressip.ProtocolUDP,
		DestinationPort: egressip.PortRange{Start: 5060, End: 5062}, SourcePort: egressip.PortRange{Start: 16384, End: 32767}}
	peer := dest
	peer.DestinationPort = egressip.PortRange{Start: 5080, End: 5080}
	configs := generatePodConfig([]net.IP{net.ParseIP("10.128.0.2"), net.ParseIP("fd00::2")}, link, net.ParseIP("198.51.100.1"), false, []egressip.Destination{dest}, true)
	other := generatePodConfig([]net.IP{net.ParseIP("10.128.0.2")}, link, net.ParseIP("198.51.100.2"), false, []egressip.Destination{peer}, true)
	if configs.len() != 1 || other.len() != 1 {
		t.Fatal("wrong address-family filtering")
	}
	if r := configs.elems[0].ipRule; r.Priority != 6000 || r.IPProto != 17 || r.Dst != dest.CIDR || r.Dport.Start != 5060 || r.Sport.End != 32767 {
		t.Fatalf("incorrect steering rule: %v", r)
	}
	fake := nodenft.SetFakeNFTablesHelper()
	c := &Controller{}
	if err := c.syncSelectiveSNATRules([]string{selectiveSNATRule(configs.elems[0]), selectiveSNATRule(other.elems[0])}); err != nil {
		t.Fatalf("publish selective SNAT rules: %v", err)
	}
	wantOne := `ip saddr 10.128.0.2 oifname "eth-mgmt" ip daddr 192.0.2.0/24 meta l4proto udp udp dport 5060-5062 udp sport 16384-32767 snat ip to 198.51.100.1`
	wantTwo := `ip saddr 10.128.0.2 oifname "eth-mgmt" ip daddr 192.0.2.0/24 meta l4proto udp udp dport 5080 udp sport 16384-32767 snat ip to 198.51.100.2`
	dump := fake.Dump()
	if !strings.Contains(dump, wantOne) || !strings.Contains(dump, wantTwo) {
		t.Fatalf("wrong selective SNAT rules:\n%s", dump)
	}
	if err := c.syncSelectiveSNATRules([]string{selectiveSNATRule(other.elems[0])}); err != nil {
		t.Fatalf("remove selective SNAT owner: %v", err)
	}
	if strings.Contains(fake.Dump(), wantOne) || !strings.Contains(fake.Dump(), wantTwo) {
		t.Fatal("removing one owner affected its peer")
	}
	if n := generatePodConfig([]net.IP{net.ParseIP("10.128.0.2")}, link, net.ParseIP("198.51.100.1"), false, nil, true).len(); n != 0 {
		t.Fatal("unresolved selector installed rules")
	}
}

func TestSharedRoutesKeepGatewayAndPeers(t *testing.T) {
	c := newOwnershipTestFixture(t).controller
	_, specific, _ := net.ParseCIDR("203.0.113.0/24")
	routes := []netlink.Route{{LinkIndex: 7, Table: 1007, Gw: net.ParseIP("198.51.100.254")}, {LinkIndex: 7, Table: 1007, Dst: specific, Gw: net.ParseIP("198.51.100.253")}}
	c.recordOwnedRoute("one", routes[0])
	c.recordOwnedRoute("one", routes[1])
	c.recordOwnedRoute("two", routes[1])
	// Releasing one owner's specific route must preserve its gateway and the peer's claim.
	if err := c.deleteOwnedRoute("one", routes[1]); err != nil {
		t.Fatalf("release shared route: %v", err)
	}
	if peers := c.routeOwners["two"]; len(peers) != 1 || !containsRoute(peers, routes[1]) || containsRoute(peers, routes[0]) {
		t.Fatal("route ownership is not independent")
	}
	if own := c.routeOwners["one"]; len(own) != 1 || !containsRoute(own, routes[0]) {
		t.Fatal("releasing a shared route removed the owner's gateway")
	}
	// A partial interface move keeps the old route independently of the new table.
	movedRoute := routes[1]
	movedRoute.LinkIndex, movedRoute.Table = 8, 1008
	if containsRoute(c.routeOwners["two"], movedRoute) {
		t.Fatal("route ownership ignored the route's interface and table")
	}
	c.recordOwnedRoute("two", movedRoute)
	if peers := c.routeOwners["two"]; len(peers) != 2 || !containsRoute(peers, routes[1]) || !containsRoute(peers, movedRoute) {
		t.Fatal("an interface move lost the old route before cleanup")
	}
	// Ownership follows the route manager's normalized identity, including IPv6's default metric.
	_, prefix, _ := net.ParseCIDR("2001:db8::/64")
	v6Route := netlink.Route{LinkIndex: 7, Table: 1007, Dst: prefix}
	c.recordOwnedRoute("one", v6Route)
	v6Route.Priority = 1024
	c.recordOwnedRoute("two", v6Route)
	v6Route.Priority = 0
	if err := c.deleteOwnedRoute("one", v6Route); err != nil {
		t.Fatalf("release IPv6 route: %v", err)
	}
}

func TestSelectiveSNATKernelSyntax(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires Linux root and network namespaces")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft tool unavailable")
	}
	netns, err := testutils.NewNS()
	if err != nil {
		t.Fatalf("create network namespace: %v", err)
	}
	t.Cleanup(func() { _ = netns.Close(); _ = testutils.UnmountNS(netns) })
	err = netns.Do(func(ns.NetNS) error {
		nft, err := knftables.New(knftables.InetFamily, "traffic-test")
		if err != nil {
			return err
		}
		tx := nft.NewTransaction()
		tx.Add(&knftables.Table{})
		tx.Add(&knftables.Chain{Name: selectiveSNATChain, Type: knftables.PtrTo(knftables.NATType), Hook: knftables.PtrTo(knftables.PostroutingHook), Priority: knftables.PtrTo(knftables.SNATPriority)})
		for _, family := range []struct {
			src, dst, eip string
			v6            bool
		}{{"10.128.0.2", "192.0.2.0/24", "198.51.100.1", false}, {"fd00::2", "2001:db8::/32", "2001:db8:1::1", true}} {
			for _, protocol := range []egressip.Protocol{0, egressip.ProtocolTCP, egressip.ProtocolUDP, egressip.ProtocolSCTP} {
				match := egressip.Destination{CIDR: netip.MustParsePrefix(family.dst), Protocol: protocol}
				if protocol != 0 {
					match.DestinationPort = egressip.PortRange{Start: 80, End: 90}
					match.SourcePort = egressip.PortRange{Start: 5060, End: 5060}
				}
				config := &podIPConfig{trafficMatch: match, snatKey: family.src, snatValue: family.eip, ifName: "eth-mgmt", v6: family.v6}
				tx.Add(&knftables.Rule{Chain: selectiveSNATChain, Rule: selectiveSNATRule(config)})
			}
		}
		if err = nft.Run(context.Background(), tx); err != nil {
			return fmt.Errorf("kernel rejected selective SNAT: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("validate selective SNAT kernel syntax: %v", err)
	}
}

func TestTrafficClassQueuesSelectivePeers(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(ovnconfig.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() { _ = ovnconfig.PrepareTestConfig() })
	ovnconfig.OVNKubernetesFeature.EnableTrafficClass = true
	client := fake.NewSimpleClientset()
	core := informers.NewSharedInformerFactory(client, 0).Core().V1()
	eips := eipinformers.NewSharedInformerFactory(eipfake.NewSimpleClientset(), 0).K8s().V1().EgressIPs()
	classes := trafficinformers.NewSharedInformerFactory(trafficfake.NewSimpleClientset(), 0).K8s().V1().TrafficClasses()
	for _, owner := range []*eipv1.EgressIP{
		{ObjectMeta: metav1.ObjectMeta{Name: "catch-all"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "old-selection"}, Spec: eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"class": "old"}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "peer"}, Spec: eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"class": "peer"}}}},
	} {
		g.Expect(eips.Informer().GetStore().Add(owner)).To(gomega.Succeed())
	}
	c, err := NewController(&kube.Kube{KClient: client}, eips, classes, core.Nodes().Informer(), core.Namespaces(), core.Pods(),
		func(string) (util.NetInfo, error) { return &util.DefaultNetInfo{}, nil }, routemanager.NewController(), true, false, node1Name, linkmanager.NewController(node1Name, true, false, nil))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(c.eIPQueue.ShutDown)
	t.Cleanup(c.namespaceQueue.ShutDown)
	t.Cleanup(c.podQueue.ShutDown)
	before := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "class", Labels: map[string]string{"class": "old"}}}
	after := before.DeepCopy()
	after.Labels["class"] = "new"
	c.onTrafficClassUpdate(before, after)
	g.Expect(c.eIPQueue.Len()).To(gomega.Equal(2))
	queued := sets.New[string]()
	for c.eIPQueue.Len() > 0 {
		name, _ := c.eIPQueue.Get()
		queued.Insert(name)
		c.eIPQueue.Done(name)
	}
	g.Expect(sets.List(queued)).To(gomega.Equal([]string{"old-selection", "peer"}))
	c.onTrafficClassUpdate(after, after.DeepCopy())
	g.Expect(c.eIPQueue.Len()).To(gomega.BeZero())
}

func TestSelectiveHostConfigRequiresMatchingFamily(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("requires root privileges")
	}
	for _, tc := range []struct {
		name, destination, podIP string
		selective, wantConfig    bool
	}{
		{"matching family", "203.0.113.0/24", "10.128.0.2", true, true},
		{"opposite destination family", "2001:db8::/64", "10.128.0.2", true, false},
		{"opposite pod family", "203.0.113.0/24", "fd00::2", true, false},
		{"legacy opposite pod family", "", "fd00::2", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			f := newOwnershipTestFixture(t)
			initial := nodeConfig{linkConfigs: []linkConfig{{linkName: "dummy1", address: []address{{cidr: "5.5.5.10/24"}}}}}
			netns, cleanup, err := setupFakeTestNode(initial)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(func() { g.Expect(cleanup()).To(gomega.Succeed()) })
			node := getNodeObj(initial, false)
			g.Expect(f.core.Nodes().Informer().GetStore().Add(&node)).To(gomega.Succeed())
			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "work"}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: namespace.Name},
				Spec: corev1.PodSpec{NodeName: node1Name}, Status: corev1.PodStatus{PodIP: tc.podIP, PodIPs: []corev1.PodIP{{IP: tc.podIP}}}}
			g.Expect(f.core.Namespaces().Informer().GetStore().Add(namespace)).To(gomega.Succeed())
			g.Expect(f.core.Pods().Informer().GetStore().Add(pod)).To(gomega.Succeed())
			owner := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "owner"},
				Status: eipv1.EgressIPStatus{Items: []eipv1.EgressIPStatusItem{{Node: node1Name, EgressIP: "5.5.5.100"}}}}
			if tc.selective {
				owner.Spec.TrafficSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"traffic": "test"}}
				class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "traffic", Labels: map[string]string{"traffic": "test"}},
					Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: tc.destination}}}}
				g.Expect(f.classes.Informer().GetStore().Add(class)).To(gomega.Succeed())
			}
			g.Expect(f.eips.Informer().GetStore().Add(owner)).To(gomega.Succeed())
			err = netns.Do(func(ns.NetNS) error {
				config, namespaces, pods, assignments, err := f.controller.processEIP(owner)
				if err != nil {
					return err
				}
				if tc.wantConfig {
					g.Expect(config).NotTo(gomega.BeNil(), "generate the applicable owner configuration")
					g.Expect(assignments).To(gomega.HaveLen(1), "retain the selected namespace configuration")
				} else {
					g.Expect(config).To(gomega.BeNil(), "no owner address/routes without applicable rules")
					g.Expect(assignments).To(gomega.BeEmpty(), "do not store empty selective pod configurations")
				}
				g.Expect(namespaces.Has(namespace.Name)).To(gomega.BeTrue(), "retain namespace references for future events")
				g.Expect(pods.Has(ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})).To(gomega.BeTrue(), "retain pod references for family changes")
				return nil
			})
			g.Expect(err).NotTo(gomega.HaveOccurred())
		})
	}
}
