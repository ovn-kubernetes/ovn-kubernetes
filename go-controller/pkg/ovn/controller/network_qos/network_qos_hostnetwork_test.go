// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	nqostype "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
)

func TestHostNetworkPodEvents(t *testing.T) {
	for _, tt := range []struct {
		name        string
		hostNetwork bool
		node        string
		want        int
	}{
		{"host network", true, "local", 0},
		{"ordinary local pod", false, "local", 1},
		{"ordinary remote pod", false, "remote", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", ResourceVersion: "1"},
				Spec:       corev1.PodSpec{HostNetwork: tt.hostNetwork, NodeName: tt.node},
			}
			updated := pod.DeepCopy()
			updated.ResourceVersion = "2"
			updated.Labels = map[string]string{"app": "selected"}
			for _, event := range []struct {
				name string
				run  func(*Controller)
			}{
				{"add", func(c *Controller) { c.onNQOSPodAdd(pod) }},
				{"update", func(c *Controller) { c.onNQOSPodUpdate(pod, updated) }},
				{"delete", func(c *Controller) { c.onNQOSPodDelete(pod) }},
				{"tombstone", func(c *Controller) {
					c.onNQOSPodDelete(cache.DeletedFinalStateUnknown{Key: "ns/pod", Obj: pod})
				}},
			} {
				t.Run(event.name, func(t *testing.T) {
					c, policies := newEventTestController(t)
					c.nodeName = "local"
					if err := policies.Add(&nqostype.NetworkQoS{ObjectMeta: metav1.ObjectMeta{Name: "qos", Namespace: "ns"}}); err != nil {
						t.Fatal(err)
					}
					if tt.hostNetwork {
						// A network/IP lookup would panic. Host-network events
						// must be filtered even when policies exist.
						c.NetInfo = nil
					}
					event.run(c)
					if got := c.nqosPodQueue.Len(); got != tt.want {
						t.Fatalf("queued %d events, want %d", got, tt.want)
					}
				})
			}
		})
	}
}

func TestPodUpdateHostNetworkFilterChecksBothVersions(t *testing.T) {
	for _, oldHostNetwork := range []bool{false, true} {
		c, policies := newEventTestController(t)
		if err := policies.Add(&nqostype.NetworkQoS{ObjectMeta: metav1.ObjectMeta{Name: "qos", Namespace: "ns"}}); err != nil {
			t.Fatal(err)
		}
		oldPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", ResourceVersion: "1"},
			Spec:       corev1.PodSpec{HostNetwork: oldHostNetwork},
		}
		newPod := oldPod.DeepCopy()
		newPod.ResourceVersion = "2"
		newPod.Spec.HostNetwork = !oldHostNetwork
		newPod.Labels = map[string]string{"app": "selected"}
		c.onNQOSPodUpdate(oldPod, newPod)
		if c.nqosPodQueue.Len() != 1 {
			t.Fatalf("update with old HostNetwork=%v must preserve ordinary Pod processing", oldHostNetwork)
		}
	}
}
