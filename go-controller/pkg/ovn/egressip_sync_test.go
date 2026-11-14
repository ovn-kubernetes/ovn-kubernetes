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
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestSyncStaleAddressSetIPsUnion(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableEgressIP = true
	clients := util.GetOVNClientset().GetOVNKubeControllerClientset()
	wf, err := factory.NewOVNKubeControllerWatchFactory(clients, "node")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(wf.Stop)
	nb, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(cleanup.Cleanup)
	asf := addressset.NewOvnAddressSetFactory(nb, true, true)
	e := NewEIPController(nb, &kube.KubeOVN{EIPClient: clients.EgressIPClient}, wf, record.NewFakeRecorder(10), nil, &networkmanager.FakeNetworkManager{}, asf, nil, true, true, "node", types.DefaultNetworkControllerName)
	target, err := asf.NewAddressSet(getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, types.DefaultNetworkName, e.controllerName), []string{"10.128.0.99"})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	other, err := asf.NewAddressSet(getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, "other", e.controllerName), []string{"10.129.0.99"})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	cache := egressIPCache{
		networkToRouter: map[string]string{types.DefaultNetworkName: "router", "other": "other-router"},
		egressIPNameToPods: map[string]map[string]selectedPods{
			"one": {types.DefaultNetworkName: {egressLocalPods: map[string]sets.Set[string]{"work_one": sets.New("10.128.0.1", "fd10::1")}}},
			"two": {types.DefaultNetworkName: {egressLocalPods: map[string]sets.Set[string]{"work_two": sets.New("10.128.0.2", "fd10::2")}, egressRemotePods: map[string]sets.Set[string]{"work_remote": sets.New("10.128.0.3")}}},
		},
	}
	include := func(network string) bool { return network == types.DefaultNetworkName }
	g.Expect(e.syncStaleAddressSetIPs(cache, include)).To(gomega.Succeed())
	v4, v6 := target.GetAddresses()
	g.Expect(v4).To(gomega.ConsistOf("10.128.0.1", "10.128.0.2"))
	g.Expect(v6).To(gomega.ConsistOf("fd10::1", "fd10::2"))
	cache.egressIPNameToPods["one"][types.DefaultNetworkName] = selectedPods{}
	delete(cache.egressIPNameToPods, "two")
	g.Expect(e.syncStaleAddressSetIPs(cache, include)).To(gomega.Succeed())
	v4, v6 = target.GetAddresses()
	g.Expect(append(v4, v6...)).To(gomega.BeEmpty())
	// Preserve legacy behavior for networks no longer selected by any owner.
	g.Expect(target.AddAddresses([]string{"10.128.0.99"})).To(gomega.Succeed())
	clear(cache.egressIPNameToPods)
	g.Expect(e.syncStaleAddressSetIPs(cache, include)).To(gomega.Succeed())
	v4, _ = target.GetAddresses()
	g.Expect(v4).To(gomega.Equal([]string{"10.128.0.99"}))
	v4, _ = other.GetAddresses()
	g.Expect(v4).To(gomega.Equal([]string{"10.129.0.99"}))
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
