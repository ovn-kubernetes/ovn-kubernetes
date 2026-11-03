// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"net"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestEgressGatewayNeedsUpdate(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	client := util.GetOVNClientset().GetClusterManagerClientset()
	wf, err := factory.NewClusterManagerWatchFactory(client)
	if err != nil {
		t.Fatalf("failed to create watch factory: %v", err)
	}
	t.Cleanup(wf.Shutdown)
	c := newEgressGatewayClusterController(client, wf)
	t.Cleanup(c.Stop)

	gateway := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gateway",
			UID:    "gateway-uid",
			Labels: map[string]string{util.EgressGatewayLabel: ""},
			Annotations: map[string]string{
				types.NodeSubnetsAnnotation: `{"default":["10.244.1.0/24"]}`,
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	unselected := gateway.DeepCopy()
	unselected.Labels = nil
	withoutSubnets := gateway.DeepCopy()
	withoutSubnets.Annotations = nil

	tests := map[string]struct {
		old      *corev1.Node
		update   func(*corev1.Node)
		expected bool
	}{
		"startup gateway": {
			expected: true,
		},
		"startup unselected node": {
			update:   func(node *corev1.Node) { node.Labels = nil },
			expected: true,
		},
		"unchanged gateway": {
			old: gateway,
		},
		"unchanged unselected node": {
			old: unselected,
		},
		"gateway label added": {
			old: unselected,
			update: func(node *corev1.Node) {
				node.Labels = map[string]string{util.EgressGatewayLabel: ""}
			},
			expected: true,
		},
		"gateway label removed": {
			old:      gateway,
			update:   func(node *corev1.Node) { delete(node.Labels, util.EgressGatewayLabel) },
			expected: true,
		},
		"gateway label value ignored": {
			old:    gateway,
			update: func(node *corev1.Node) { node.Labels[util.EgressGatewayLabel] = "false" },
		},
		"gateway UID changed": {
			old:      gateway,
			update:   func(node *corev1.Node) { node.UID = "replacement-uid" },
			expected: true,
		},
		"gateway subnet change ignored": {
			old: gateway,
			update: func(node *corev1.Node) {
				node.Annotations[types.NodeSubnetsAnnotation] = `{"default":["10.244.2.0/24"]}`
			},
		},
		"gateway IPv6 subnet addition ignored": {
			old: gateway,
			update: func(node *corev1.Node) {
				node.Annotations[types.NodeSubnetsAnnotation] = `{"default":["10.244.1.0/24","fd00:10:244:1::/64"]}`
			},
		},
		"gateway subnet removal ignored": {
			old:    gateway,
			update: func(node *corev1.Node) { delete(node.Annotations, types.NodeSubnetsAnnotation) },
		},
		"gateway subnet availability ignored": {
			old: withoutSubnets,
			update: func(node *corev1.Node) {
				node.Annotations = map[string]string{
					types.NodeSubnetsAnnotation: `{"default":["10.244.1.0/24"]}`,
				}
			},
		},
		"unselected node UID and subnet changes ignored": {
			old: unselected,
			update: func(node *corev1.Node) {
				node.UID = "replacement-uid"
				node.Annotations[types.NodeSubnetsAnnotation] = `{"default":["10.244.2.0/24"]}`
			},
		},
		"unrelated label ignored": {
			old:    gateway,
			update: func(node *corev1.Node) { node.Labels["example.com/role"] = "worker" },
		},
		"unrelated annotation ignored": {
			old:    gateway,
			update: func(node *corev1.Node) { node.Annotations["example.com/owner"] = "test" },
		},
		"published readiness ignored": {
			old:    gateway,
			update: func(node *corev1.Node) { node.Annotations[util.EgressGatewayReady] = "true" },
		},
		"NodeReady change ignored": {
			old: gateway,
			update: func(node *corev1.Node) {
				node.Status.Conditions[0].Status = corev1.ConditionFalse
			},
		},
		"deletion timestamp ignored": {
			old: gateway,
			update: func(node *corev1.Node) {
				now := metav1.Now()
				node.DeletionTimestamp = &now
			},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			node := gateway.DeepCopy()
			if test.old != nil {
				node = test.old.DeepCopy()
			}
			if test.update != nil {
				test.update(node)
			}
			if got := c.needsUpdate(test.old, node); got != test.expected {
				t.Fatalf("needsUpdate(%s) returned %v; expected %v", name, got, test.expected)
			}
		})
	}
}

func TestEgressGatewayRecreatedNode(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}

	tests := map[string]struct {
		subnet       string
		managementIP string
	}{
		"same subnet":             {subnet: "127.0.0.0/24", managementIP: "127.0.0.2"},
		"different subnet":        {subnet: "127.0.1.0/24", managementIP: "127.0.1.2"},
		"subnet not yet assigned": {},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "gateway",
					UID:    "replacement-uid",
					Labels: map[string]string{util.EgressGatewayLabel: ""},
				},
			}
			if test.subnet != "" {
				node.Annotations = map[string]string{
					types.NodeSubnetsAnnotation: `{"default":["` + test.subnet + `"]}`,
				}
			}
			client := util.GetOVNClientset(node).GetClusterManagerClientset()
			wf, err := factory.NewClusterManagerWatchFactory(client)
			if err != nil {
				t.Fatalf("failed to create watch factory: %v", err)
			}
			t.Cleanup(wf.Shutdown)
			if err := wf.Start(); err != nil {
				t.Fatalf("failed to start watch factory: %v", err)
			}
			c := newEgressGatewayClusterController(client, wf)
			t.Cleanup(c.Stop)
			oldClient := &fakeEgressIPHealthClient{Connected: true}
			c.gatewayNodes[node.Name] = &egressGatewayNode{
				uid:           "old-uid",
				managementIPs: []net.IP{net.ParseIP("127.0.0.2")},
				client:        oldClient,
			}

			err = c.reconcile(t.Context(), node.Name)
			if oldClient.IsConnected() {
				t.Fatal("old health client remains connected after Node replacement")
			}
			gateway, exists := c.gatewayNodes[node.Name]
			if test.subnet == "" {
				if err == nil || exists {
					t.Fatalf("expected retry error and no tracked gateway without subnets; got error %v, tracked %v", err, exists)
				}
				return
			}
			if err != nil {
				t.Fatalf("failed to reconcile replacement Node: %v", err)
			}
			if !exists || gateway.uid != string(node.UID) || gateway.client == oldClient {
				t.Fatal("replacement Node did not receive a new entry and health client")
			}
			if len(gateway.managementIPs) != 1 || !gateway.managementIPs[0].Equal(net.ParseIP(test.managementIP)) {
				t.Fatalf("replacement management IPs: got %v, expected %s", gateway.managementIPs, test.managementIP)
			}
		})
	}
}
