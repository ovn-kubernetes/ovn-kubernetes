// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	nqostype "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
)

func TestNetworkQoSDeletionEvents(t *testing.T) {
	policy := &nqostype.NetworkQoS{ObjectMeta: metav1.ObjectMeta{Name: "qos", Namespace: "ns"}}
	for _, tt := range []struct {
		name string
		obj  any
		want int
	}{
		{"object", policy, 1},
		{"tombstone", cache.DeletedFinalStateUnknown{Key: "ns/qos", Obj: policy}, 1},
		{"key-only tombstone", cache.DeletedFinalStateUnknown{Key: "ns/qos"}, 1},
		{"invalid object", struct{}{}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newEventTestController(t)
			// The policy has already left the informer, including when this
			// was the last policy. Its cleanup must still be queued.
			c.onNQOSDelete(tt.obj)
			if got := c.nqosQueue.Len(); got != tt.want {
				t.Fatalf("queued %d policies, want %d", got, tt.want)
			}
			if tt.want != 0 {
				key, _ := c.nqosQueue.Get()
				defer c.nqosQueue.Done(key)
				if key != "ns/qos" {
					t.Fatalf("queued key %q, want ns/qos", key)
				}
			}
		})
	}
}

func TestPodEventsAfterNamespaceDeletion(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "deleted-ns"}}
	for _, tt := range []struct {
		name string
		run  func(*Controller)
	}{
		{"queued add", func(c *Controller) { c.onNQOSPodAdd(pod) }},
		{"delete", func(c *Controller) { c.onNQOSPodDelete(pod) }},
		{"delete tombstone", func(c *Controller) {
			c.onNQOSPodDelete(cache.DeletedFinalStateUnknown{Key: "deleted-ns/pod", Obj: pod})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, policies := newEventTestController(t)
			for _, ns := range []string{"deleted-ns", "other-ns"} {
				if err := policies.Add(&nqostype.NetworkQoS{ObjectMeta: metav1.ObjectMeta{Name: "qos", Namespace: ns}}); err != nil {
					t.Fatal(err)
				}
			}
			// Even a policy in another namespace could have selected this
			// Pod as a destination using the now-unavailable namespace labels.
			namespaces := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			c.nqosNamespaceLister = corelisters.NewNamespaceLister(namespaces)
			tt.run(c)
			if c.nqosPodQueue.Len() != 1 {
				t.Fatal("expected one queued Pod event")
			}
			event, _ := c.nqosPodQueue.Get()
			defer c.nqosPodQueue.Done(event)
			if err := c.syncNetworkQoSPod(event); err != nil {
				t.Fatalf("missing namespace must trigger cleanup, not retries: %v", err)
			}
			got := sets.New[string]()
			for c.nqosQueue.Len() > 0 {
				key, _ := c.nqosQueue.Get()
				got.Insert(key)
				c.nqosQueue.Done(key)
			}
			if !got.Equal(sets.New("deleted-ns/qos", "other-ns/qos")) {
				t.Fatalf("unexpected policies queued for cleanup: %v", got)
			}
		})
	}
}

type failingNamespaceLister struct {
	corelisters.NamespaceLister
}

func (failingNamespaceLister) Get(string) (*corev1.Namespace, error) {
	return nil, fmt.Errorf("namespace lookup failed")
}

func TestPodDeletionNamespaceLookupError(t *testing.T) {
	c, policies := newEventTestController(t)
	if err := policies.Add(&nqostype.NetworkQoS{ObjectMeta: metav1.ObjectMeta{Name: "qos", Namespace: "ns"}}); err != nil {
		t.Fatal(err)
	}
	c.nqosNamespaceLister = failingNamespaceLister{}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns"}}
	if err := c.syncNetworkQoSPod(newEventData(pod, nil)); err == nil {
		t.Fatal("transient namespace errors must still be retried")
	}
	if c.nqosQueue.Len() != 0 {
		t.Fatal("transient error must not be treated as namespace deletion")
	}
}
