// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	ipgenerator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/ip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestSyncNodeIDsOutOfRange(t *testing.T) {
	for _, invalid := range []int{0, 1, -2, types.MaxNodeIDs + 1, math.MaxInt} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			config.IPv4Mode = true
			invalidNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: "invalid", Annotations: map[string]string{util.OvnNodeID: strconv.Itoa(invalid)},
			}}
			healthyNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: "healthy", Annotations: map[string]string{util.OvnNodeID: "2"},
			}}
			client := util.GetOVNClientset(invalidNode, healthyNode).GetClusterManagerClientset()
			wf, err := factory.NewClusterManagerWatchFactory(client)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(wf.Shutdown)
			g.Expect(wf.Start()).To(gomega.Succeed())
			nac, err := newNodeAllocationController(client, wf)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(nac.Stop)
			g.Expect(nac.Start(context.Background())).To(gomega.Succeed())
			allocator := nac.nodeIDAllocator
			// Wait for the informer-driven handlers to persist both annotations.
			g.Eventually(func(g gomega.Gomega) {
				for _, name := range []string{"invalid", "healthy"} {
					node, getErr := client.KubeClient.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
					g.Expect(getErr).NotTo(gomega.HaveOccurred())
					nodeID, parseErr := util.GetNodeID(node)
					g.Expect(parseErr).NotTo(gomega.HaveOccurred())
					g.Expect(nodeID).To(gomega.BeNumerically(">=", 2))
					g.Expect(nodeID).To(gomega.BeNumerically("<=", types.MaxNodeIDs))
					g.Expect(nodeID).To(gomega.Equal(allocator.GetID(name)))
					g.Expect(node.Annotations[util.OvnTransitSwitchPortAddr]).NotTo(gomega.BeEmpty())
					if name == "healthy" {
						g.Expect(nodeID).To(gomega.Equal(2))
					}
				}
			}, 5*time.Second, 10*time.Millisecond).Should(gomega.Succeed())
			nodes := []interface{}{invalidNode, healthyNode}
			replacement := allocator.GetID("invalid")
			if replacement < 2 || replacement > types.MaxNodeIDs || replacement == 2 {
				t.Fatalf("invalid replacement: %d", replacement)
			}
			if allocator.GetID("healthy") != 2 {
				t.Fatal("healthy node ID changed")
			}
			if err = nac.syncNodeIDs(nodes); err != nil {
				t.Fatal(err)
			}
			if allocator.GetID("invalid") != replacement {
				t.Fatal("replacement changed on resync")
			}
			next, err := allocator.AllocateID("another")
			if err != nil || next == replacement || next == 2 {
				t.Fatalf("subsequent allocation: %d, %v", next, err)
			}
		})
	}
}

// newTestNodeAllocationController returns a dual-stack node allocation
// controller writing to the given client, with the system node IDs reserved.
func newTestNodeAllocationController(t *testing.T, client *fake.Clientset) *nodeAllocationController {
	t.Helper()
	oldV4, oldV6, oldHybridOverlay := config.IPv4Mode, config.IPv6Mode, config.HybridOverlay.Enabled
	config.IPv4Mode, config.IPv6Mode, config.HybridOverlay.Enabled = true, true, false
	t.Cleanup(func() {
		config.IPv4Mode, config.IPv6Mode, config.HybridOverlay.Enabled = oldV4, oldV6, oldHybridOverlay
	})
	allocator := id.NewIDAllocator("NodeIDs", types.MaxNodeIDs+1)
	for name, reserved := range map[string]int{"zero": 0, "one": 1} {
		if err := allocator.ReserveID(name, reserved); err != nil {
			t.Fatalf("failed to reserve system node ID %d: %v", reserved, err)
		}
	}
	v4, err := ipgenerator.NewIPGenerator("100.88.0.0/16")
	if err != nil {
		t.Fatalf("failed to create IPv4 generator: %v", err)
	}
	v6, err := ipgenerator.NewIPGenerator("fd97::/64")
	if err != nil {
		t.Fatalf("failed to create IPv6 generator: %v", err)
	}
	return &nodeAllocationController{
		kube:                       &kube.Kube{KClient: client},
		nodeIDAllocator:            allocator,
		transitSwitchIPv4Generator: v4,
		transitSwitchIPv6Generator: v6,
	}
}

func TestNeedsNodeAllocation(t *testing.T) {
	nac := newTestNodeAllocationController(t, fake.NewClientset())
	// Transit switch port addresses derived from node ID 2.
	const derivedTransit = `{"ipv4":"100.88.0.2/16","ipv6":"fd97::2/64"}`
	maxIDv4, maxIDv6, err := nac.transitSwitchPortAddrs(types.MaxNodeIDs)
	if err != nil {
		t.Fatalf("failed to derive the transit switch port addresses of the maximum node ID: %v", err)
	}
	maxIDTransit := fmt.Sprintf(`{"ipv4":%q,"ipv6":%q}`, maxIDv4, maxIDv6)
	for _, tc := range []struct {
		name, nodeID, transit           string
		missingID, missingTransit, want bool
	}{
		{name: "missing ID", missingID: true, want: true},
		{name: "empty ID", want: true},
		{name: "malformed ID", nodeID: "invalid", want: true},
		{name: "negative ID", nodeID: "-2", want: true},
		{name: "reserved zero ID", nodeID: "0", want: true},
		{name: "reserved one ID", nodeID: "1", want: true},
		{name: "ID above limit", nodeID: strconv.Itoa(types.MaxNodeIDs + 1), want: true},
		{name: "oversized ID", nodeID: "1099511627776", want: true},
		{name: "minimum valid ID", nodeID: strconv.Itoa(types.MinNodeID)},
		{name: "maximum valid ID", nodeID: strconv.Itoa(types.MaxNodeIDs), transit: maxIDTransit},
		{name: "missing transit address", nodeID: "2", missingTransit: true, want: true},
		{name: "malformed transit address", nodeID: "2", transit: "invalid", want: true},
		{name: "transit address of another node ID", nodeID: "2",
			transit: `{"ipv4":"100.88.0.3/16","ipv6":"fd97::3/64"}`, want: true},
		{name: "transit address of another node ID in one IP family", nodeID: "2",
			transit: `{"ipv4":"100.88.0.2/16","ipv6":"fd97::3/64"}`, want: true},
		{name: "transit address with another prefix length", nodeID: "2",
			transit: `{"ipv4":"100.88.0.2/24","ipv6":"fd97::2/64"}`, want: true},
		{name: "transit address missing an IP family", nodeID: "2",
			transit: `{"ipv4":"100.88.0.2/16"}`, want: true},
		{name: "derived transit address in another notation", nodeID: "2",
			transit: `{"ipv6": "fd97:0::2/64", "ipv4": "100.88.0.2/16"}`},
		{name: "derived transit address as IPv4-mapped IPv6", nodeID: "2",
			transit: `{"ipv4":"::ffff:100.88.0.2/16","ipv6":"fd97::2/64"}`, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transit := tc.transit
			if transit == "" {
				transit = derivedTransit
			}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				util.OvnNodeID: tc.nodeID, util.OvnTransitSwitchPortAddr: transit,
			}}}
			if tc.missingID {
				delete(node.Annotations, util.OvnNodeID)
			}
			if tc.missingTransit {
				delete(node.Annotations, util.OvnTransitSwitchPortAddr)
			}
			if got := nac.needsNodeAllocation(node); got != tc.want {
				t.Fatalf("needsNodeAllocation() = %t, want %t", got, tc.want)
			}
		})
	}
}

// In a single-stack cluster only the address of the configured IP family is
// expected: an address of the other family is not one cluster manager wrote.
func TestNeedsNodeAllocationSingleStack(t *testing.T) {
	for _, tc := range []struct {
		name, transit string
		ipv6Only      bool
		want          bool
	}{
		{name: "IPv4 cluster with the derived address", transit: `{"ipv4":"100.88.0.2/16"}`},
		{name: "IPv4 cluster with an additional IPv6 address",
			transit: `{"ipv4":"100.88.0.2/16","ipv6":"fd97::2/64"}`, want: true},
		{name: "IPv4 cluster with only an IPv6 address", transit: `{"ipv6":"fd97::2/64"}`, want: true},
		{name: "IPv6 cluster with the derived address", transit: `{"ipv6":"fd97::2/64"}`, ipv6Only: true},
		{name: "IPv6 cluster with an additional IPv4 address",
			transit: `{"ipv4":"100.88.0.2/16","ipv6":"fd97::2/64"}`, ipv6Only: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nac := newTestNodeAllocationController(t, fake.NewClientset())
			// The helper restores both IP modes when the subtest ends.
			config.IPv4Mode, config.IPv6Mode = !tc.ipv6Only, tc.ipv6Only
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				util.OvnNodeID: "2", util.OvnTransitSwitchPortAddr: tc.transit,
			}}}
			if got := nac.needsNodeAllocation(node); got != tc.want {
				t.Fatalf("needsNodeAllocation() = %t, want %t", got, tc.want)
			}
		})
	}
}

// A transit switch port address annotation that no longer matches the node ID
// is rewritten on the next node update, and the node keeps its ID.
func TestNodeAllocationUpdateRepairsTransitSwitchPortAddr(t *testing.T) {
	const derivedTransit = `{"ipv4":"100.88.0.5/16","ipv6":"fd97::5/64"}`
	for _, tc := range []struct{ name, transit string }{
		{name: "address of another node", transit: `{"ipv4":"100.88.0.9/16","ipv6":"fd97::9/64"}`},
		{name: "address of another node in one IP family", transit: `{"ipv4":"100.88.0.5/16","ipv6":"fd97::9/64"}`},
		{name: "address outside the transit subnet", transit: `{"ipv4":"10.0.0.5/16","ipv6":"fd00::5/64"}`},
		{name: "malformed address", transit: "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1", Annotations: map[string]string{
				util.OvnNodeID:                "5",
				util.OvnTransitSwitchPortAddr: tc.transit,
			}}}
			client := fake.NewClientset(node)
			nac := newTestNodeAllocationController(t, client)
			handler := &nodeAllocationControllerEventHandler{objType: factory.NodeType, nac: nac}
			if err := nac.syncNodeIDs([]interface{}{node}); err != nil {
				t.Fatalf("failed to restore node ID allocations: %v", err)
			}
			if err := handler.UpdateResource(context.Background(), node, node, false); err != nil {
				t.Fatalf("failed to reconcile the node update: %v", err)
			}
			updated, err := client.CoreV1().Nodes().Get(context.Background(), node.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("failed to get the reconciled node: %v", err)
			}
			if got := updated.Annotations[util.OvnNodeID]; got != "5" {
				t.Fatalf("node ID changed to %q, want 5", got)
			}
			if got := updated.Annotations[util.OvnTransitSwitchPortAddr]; got != derivedTransit {
				t.Fatalf("transit switch port addresses are %q, want %q", got, derivedTransit)
			}
			if nac.needsNodeAllocation(updated) {
				t.Fatalf("repaired node still needs allocation: %v", updated.Annotations)
			}
		})
	}
}

func TestNodeAllocationUpdateRepairsInvalidID(t *testing.T) {
	for _, invalidID := range []string{"", "invalid", "-2", "0", "1", strconv.Itoa(types.MaxNodeIDs + 1), "1099511627776"} {
		t.Run(invalidID, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1", Annotations: map[string]string{
				util.OvnNodeID: invalidID,
				// Both annotations must exist to reproduce the update being skipped.
				util.OvnTransitSwitchPortAddr: `{"ipv4":"100.88.0.99/16","ipv6":"fd97::99/64"}`,
			}}}
			client := fake.NewClientset(node)
			nac := newTestNodeAllocationController(t, client)
			allocator := nac.nodeIDAllocator
			v4, v6 := nac.transitSwitchIPv4Generator, nac.transitSwitchIPv6Generator
			handler := &nodeAllocationControllerEventHandler{objType: factory.NodeType, nac: nac}
			if err := nac.syncNodeIDs([]interface{}{node}); err != nil {
				t.Fatalf("restore node ID allocations: %v", err)
			}
			if err := handler.UpdateResource(context.Background(), node, node, false); err != nil {
				t.Fatalf("reconcile invalid node ID update: %v", err)
			}
			updated, err := client.CoreV1().Nodes().Get(context.Background(), node.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get reconciled node: %v", err)
			}
			assigned, err := util.GetNodeID(updated)
			if err != nil || assigned < types.MinNodeID || assigned > types.MaxNodeIDs {
				t.Fatalf("invalid replacement node ID %d: %v", assigned, err)
			}
			if got := allocator.GetID(node.Name); got != assigned {
				t.Fatalf("reserved node ID %d differs from annotation %d", got, assigned)
			}
			addresses, err := util.ParseNodeTransitSwitchPortAddrs(updated)
			if err != nil || len(addresses) != 2 {
				t.Fatalf("expected dual-stack transit addresses, got %v: %v", addresses, err)
			}
			for _, generator := range []*ipgenerator.IPGenerator{v4, v6} {
				expected, err := generator.GenerateIP(assigned)
				if err != nil {
					t.Fatalf("derive transit address from replacement ID: %v", err)
				}
				found := false
				for _, address := range addresses {
					if address.String() == expected.String() {
						found = true
					}
				}
				if !found {
					t.Fatalf("transit addresses %v do not contain replacement address %s", addresses, expected)
				}
			}
		})
	}
}
