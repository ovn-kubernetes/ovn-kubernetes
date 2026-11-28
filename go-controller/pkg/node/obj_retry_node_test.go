// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestNodeEventHandlerResourcesEqual(t *testing.T) {
	h := &nodeEventHandler{
		objType: factory.NodeType,
		nc: &DefaultNodeNetworkController{BaseNodeNetworkController: BaseNodeNetworkController{
			CommonNodeNetworkControllerInfo: CommonNodeNetworkControllerInfo{name: "local"},
		}},
	}
	for _, tc := range []struct {
		name      string
		local     bool
		prepare   func(*corev1.Node)
		change    func(*corev1.Node)
		wantEqual bool
	}{
		{name: "unchanged remote node", wantEqual: true},
		{name: "remote unrelated annotation added", change: func(n *corev1.Node) { n.Annotations["example.com/state"] = "updated" }, wantEqual: true},
		{name: "remote unrelated annotation changed", prepare: func(n *corev1.Node) { n.Annotations["example.com/state"] = "old" }, change: func(n *corev1.Node) { n.Annotations["example.com/state"] = "new" }, wantEqual: true},
		{name: "remote unrelated annotation removed", prepare: func(n *corev1.Node) { n.Annotations["example.com/state"] = "old" }, change: func(n *corev1.Node) { delete(n.Annotations, "example.com/state") }, wantEqual: true},
		{name: "remote IPv4 CIDR changed", change: func(n *corev1.Node) { n.Annotations[util.OVNNodeHostCIDRs] = `["192.0.2.2/24","2001:db8::1/64"]` }},
		{name: "remote IPv6 CIDR changed", change: func(n *corev1.Node) { n.Annotations[util.OVNNodeHostCIDRs] = `["192.0.2.1/24","2001:db8::2/64"]` }},
		{name: "remote CIDRs removed", change: func(n *corev1.Node) { delete(n.Annotations, util.OVNNodeHostCIDRs) }},
		{name: "remote CIDRs added", prepare: func(n *corev1.Node) { delete(n.Annotations, util.OVNNodeHostCIDRs) }, change: func(n *corev1.Node) { n.Annotations[util.OVNNodeHostCIDRs] = `["192.0.2.1/24"]` }},
		{name: "remote missing to empty CIDRs", prepare: func(n *corev1.Node) { delete(n.Annotations, util.OVNNodeHostCIDRs) }, change: func(n *corev1.Node) { n.Annotations[util.OVNNodeHostCIDRs] = "" }},
		{name: "remote empty to missing CIDRs", prepare: func(n *corev1.Node) { n.Annotations[util.OVNNodeHostCIDRs] = "" }, change: func(n *corev1.Node) { delete(n.Annotations, util.OVNNodeHostCIDRs) }},
		{name: "remote malformed CIDRs reach validation", change: func(n *corev1.Node) { n.Annotations[util.OVNNodeHostCIDRs] = "invalid-json" }},
		{name: "remote address changed", change: func(n *corev1.Node) { n.Status.Addresses[0].Address = "192.0.2.2" }},
		{name: "remote absent CIDRs and nil annotation map", prepare: func(n *corev1.Node) { n.Annotations = nil }, change: func(n *corev1.Node) { n.Annotations = map[string]string{} }, wantEqual: true},
		{name: "local unchanged", local: true, wantEqual: true},
		{name: "local arbitrary annotation changed", local: true, change: func(n *corev1.Node) { n.Annotations["example.com/state"] = "updated" }},
		{name: "local SNAT annotation changed", local: true, change: func(n *corev1.Node) { n.Annotations[util.OvnNodeDontSNATSubnets] = `["192.0.2.0/24"]` }},
		{name: "local DPU host annotation changed", local: true, change: func(n *corev1.Node) { n.Annotations[util.OVNNodePrimaryDPUHostAddr] = `{"ipv4":"192.0.2.2/24"}` }},
		{name: "local CIDRs changed", local: true, change: func(n *corev1.Node) { n.Annotations[util.OVNNodeHostCIDRs] = `["192.0.2.2/24"]` }},
		{name: "local address changed", local: true, change: func(n *corev1.Node) { n.Status.Addresses[0].Address = "192.0.2.2" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "remote", Annotations: map[string]string{util.OVNNodeHostCIDRs: `["192.0.2.1/24","2001:db8::1/64"]`}},
				Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.1"}}},
			}
			if tc.local {
				old.Name = "local"
			}
			if tc.prepare != nil {
				tc.prepare(old)
			}
			newNode := old.DeepCopy()
			if tc.change != nil {
				tc.change(newNode)
			}
			for _, pair := range [][2]*corev1.Node{{old, newNode}, {newNode, old}} {
				equal, err := h.AreResourcesEqual(pair[0], pair[1])
				if err != nil || equal != tc.wantEqual {
					t.Fatalf("equal=%t error=%v, want equal=%t", equal, err, tc.wantEqual)
				}
			}
		})
	}
}
