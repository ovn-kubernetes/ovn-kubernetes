// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"testing"

	"github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestSyncStaleAddressSetIPsUnion(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "prepare test configuration")
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableEgressIP = true
	clients := util.GetOVNClientset().GetOVNKubeControllerClientset()
	wf, err := factory.NewOVNKubeControllerWatchFactory(clients, "node")
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the controller watch factory")
	t.Cleanup(wf.Shutdown)
	nb, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the northbound test database")
	t.Cleanup(cleanup.Cleanup)
	asf := addressset.NewOvnAddressSetFactory(nb, true, true)
	e := NewEIPController(nb, &kube.KubeOVN{EIPClient: clients.EgressIPClient}, wf, record.NewFakeRecorder(10), nil, &networkmanager.FakeNetworkManager{}, asf, nil, true, true, "node", types.DefaultNetworkControllerName)
	target, err := asf.NewAddressSet(getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, types.DefaultNetworkName, e.controllerName), []string{"10.128.0.99"})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the target network served-pod set")
	other, err := asf.NewAddressSet(getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, "other", e.controllerName), []string{"10.129.0.99", "fd11::99"})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the other network served-pod set")
	destinations, err := asf.NewAddressSet(getEgressIPDestNetworksAddrSetDbIDs("owner", e.controllerName), []string{"192.0.2.0/24", "2001:db8::/64"})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create a destination set that repair must preserve")
	foreign, err := asf.NewAddressSet(getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, types.DefaultNetworkName, "other-controller"), []string{"10.130.0.99", "fd12::99"})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create another controller's served-pod set")
	cache := egressIPCache{
		networkToRouter: map[string]string{types.DefaultNetworkName: "router", "other": "other-router"},
		egressIPNameToPods: map[string]map[string]selectedPods{
			"one": {types.DefaultNetworkName: {egressLocalPods: map[string]sets.Set[string]{"work_one": sets.New("10.128.0.1", "fd10::1")}}},
			"two": {types.DefaultNetworkName: {egressLocalPods: map[string]sets.Set[string]{"work_two": sets.New("10.128.0.2", "fd10::2")}, egressRemotePods: map[string]sets.Set[string]{"work_remote": sets.New("10.128.0.3")}}},
		},
	}
	include := func(network string) bool { return network == types.DefaultNetworkName }
	g.Expect(e.syncStaleAddressSetIPs(cache, include)).To(gomega.Succeed(), "repair the served-pod IP union")
	v4, v6 := target.GetAddresses()
	g.Expect(v4).To(gomega.ConsistOf("10.128.0.1", "10.128.0.2"))
	g.Expect(v6).To(gomega.ConsistOf("fd10::1", "fd10::2"))
	cache.egressIPNameToPods["one"][types.DefaultNetworkName] = selectedPods{}
	delete(cache.egressIPNameToPods, "two")
	g.Expect(e.syncStaleAddressSetIPs(cache, include)).To(gomega.Succeed(), "repair the served-pod IP union")
	v4, v6 = target.GetAddresses()
	g.Expect(append(v4, v6...)).To(gomega.BeEmpty())
	// Repair must clear both families even after the final owner disappears.
	g.Expect(target.AddAddresses([]string{"10.128.0.99", "fd10::99"})).To(gomega.Succeed(), "seed stale served-pod IPs")
	clear(cache.egressIPNameToPods)
	g.Expect(e.syncStaleAddressSetIPs(cache, include)).To(gomega.Succeed(), "repair the served-pod IP union")
	v4, v6 = target.GetAddresses()
	g.Expect(append(v4, v6...)).To(gomega.BeEmpty())
	v4, v6 = other.GetAddresses()
	g.Expect(v4).To(gomega.Equal([]string{"10.129.0.99"}))
	g.Expect(v6).To(gomega.Equal([]string{"fd11::99"}))
	// An empty cache is also valid when repairing all networks.
	g.Expect(e.syncStaleAddressSetIPs(egressIPCache{}, egressIPAllNetworks)).To(gomega.Succeed(), "clear all unowned served-pod sets")
	v4, v6 = other.GetAddresses()
	g.Expect(append(v4, v6...)).To(gomega.BeEmpty())
	v4, v6 = destinations.GetAddresses()
	g.Expect(v4).To(gomega.Equal([]string{"192.0.2.0/24"}))
	g.Expect(v6).To(gomega.Equal([]string{"2001:db8::/64"}))
	v4, v6 = foreign.GetAddresses()
	g.Expect(v4).To(gomega.Equal([]string{"10.130.0.99"}))
	g.Expect(v6).To(gomega.Equal([]string{"fd12::99"}))
	// A never-created network must not gain empty address sets during repair.
	cache.networkToRouter["never-created"] = "new-router"
	allAddressSets := func(*nbdb.AddressSet) bool { return true }
	before, err := libovsdbops.FindAddressSetsWithPredicate(nb, allAddressSets)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "list address sets before repeated repair")
	g.Expect(e.syncStaleAddressSetIPs(cache, egressIPAllNetworks)).To(gomega.Succeed(), "repeat repair without creating address sets")
	after, err := libovsdbops.FindAddressSetsWithPredicate(nb, allAddressSets)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "list address sets after repeated repair")
	g.Expect(after).To(gomega.ConsistOf(before))
}

func TestEgressPodIPFromMatch(t *testing.T) {
	for match, want := range map[string]string{
		"ip4.src == 10.128.0.1":                         "10.128.0.1",
		"ip6.src == fd10::1 && pkt.mark == 0":           "fd10::1",
		"ip4.src == 10.128.0.1 && ip4.dst == 192.0.2.1": "10.128.0.1",
		"ip4.dst == 192.0.2.1":                          "",
		"ip4.src != 10.128.0.1":                         "",
		"ip4.src == invalid":                            "",
		"":                                              "",
	} {
		ip := egressPodIPFromMatch(match)
		if want == "" {
			if ip != nil {
				t.Errorf("match %q: unexpected source %s", match, ip)
			}
		} else if ip.String() != want {
			t.Errorf("match %q: got %s, want %s", match, ip, want)
		}
	}
}
