// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	nqostype "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	nqoslister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/listers/networkqos/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func newEventTestController(tb testing.TB) (*Controller, cache.Indexer) {
	tb.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	c := &Controller{
		NetInfo:            &util.DefaultNetInfo{},
		nqosLister:         nqoslister.NewNetworkQoSLister(indexer),
		nqosPodQueue:       workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[*eventData[*corev1.Pod]]()),
		nqosNamespaceQueue: workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[*eventData[*corev1.Namespace]]()),
	}
	tb.Cleanup(c.nqosPodQueue.ShutDown)
	tb.Cleanup(c.nqosNamespaceQueue.ShutDown)
	return c, indexer
}

func TestEventsWithoutNetworkQoS(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", ResourceVersion: "1"}}
	updatedPod := pod.DeepCopy()
	updatedPod.ResourceVersion = "2"
	updatedPod.Labels = map[string]string{"app": "selected"}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns", ResourceVersion: "1"}}
	updatedNS := ns.DeepCopy()
	updatedNS.ResourceVersion = "2"
	updatedNS.Labels = map[string]string{"app": "selected"}
	tests := []struct {
		name  string
		event func(*Controller)
	}{
		{"pod add", func(c *Controller) { c.onNQOSPodAdd(pod) }},
		{"pod update", func(c *Controller) { c.onNQOSPodUpdate(pod, updatedPod) }},
		{"pod delete", func(c *Controller) { c.onNQOSPodDelete(pod) }},
		{"pod tombstone", func(c *Controller) { c.onNQOSPodDelete(cache.DeletedFinalStateUnknown{Obj: pod}) }},
		{"namespace add", func(c *Controller) { c.onNQOSNamespaceAdd(ns) }},
		{"namespace update", func(c *Controller) { c.onNQOSNamespaceUpdate(ns, updatedNS) }},
		{"namespace delete", func(c *Controller) { c.onNQOSNamespaceDelete(ns) }},
		{"namespace tombstone", func(c *Controller) { c.onNQOSNamespaceDelete(cache.DeletedFinalStateUnknown{Obj: ns}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, indexer := newEventTestController(t)
			policy := &nqostype.NetworkQoS{ObjectMeta: metav1.ObjectMeta{Name: "qos", Namespace: "ns"}}
			check := func(want int) {
				t.Helper()
				tt.event(c)
				if got := c.nqosPodQueue.Len() + c.nqosNamespaceQueue.Len(); got != want {
					t.Fatalf("queued %d events, want %d", got, want)
				}
				for c.nqosPodQueue.Len() > 0 {
					event, _ := c.nqosPodQueue.Get()
					c.nqosPodQueue.Done(event)
				}
				for c.nqosNamespaceQueue.Len() > 0 {
					event, _ := c.nqosNamespaceQueue.Get()
					c.nqosNamespaceQueue.Done(event)
				}
			}
			check(0)
			// The informer stores a policy before invoking its handler. Events
			// must resume even before onNQOSAdd or the first reconcile runs.
			if err := indexer.Add(policy); err != nil {
				t.Fatal(err)
			}
			check(1)
			if err := indexer.Delete(policy); err != nil {
				t.Fatal(err)
			}
			check(0)
			if err := indexer.Add(policy); err != nil {
				t.Fatal(err)
			}
			check(1)
		})
	}
}

func TestPodUpdateWithoutNetworkQoSSkipsNetworkLookup(t *testing.T) {
	c, _ := newEventTestController(t)
	// Any network/IP lookup would dereference NetInfo. An empty policy
	// informer should short-circuit before reaching that work.
	c.NetInfo = nil
	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", ResourceVersion: "1"}}
	newPod := oldPod.DeepCopy()
	newPod.ResourceVersion = "2"
	c.onNQOSPodUpdate(oldPod, newPod)
	if c.nqosPodQueue.Len() != 0 {
		t.Fatal("pod update queued with no policies")
	}
}

func TestQueuedPodEventAfterLastNetworkQoSDeleted(t *testing.T) {
	c, indexer := newEventTestController(t)
	policy := &nqostype.NetworkQoS{ObjectMeta: metav1.ObjectMeta{Name: "qos", Namespace: "ns"}}
	if err := indexer.Add(policy); err != nil {
		t.Fatal(err)
	}
	c.onNQOSPodDelete(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "deleted-ns"}})
	if c.nqosPodQueue.Len() != 1 {
		t.Fatal("expected a queued pod deletion")
	}
	if err := indexer.Delete(policy); err != nil {
		t.Fatal(err)
	}
	event, _ := c.nqosPodQueue.Get()
	defer c.nqosPodQueue.Done(event)
	// No namespace lister is installed: work queued before the last policy
	// disappeared must finish without looking up a possibly deleted namespace.
	if err := c.syncNetworkQoSPod(event); err != nil {
		t.Fatalf("empty policy informer should not cause retries: %v", err)
	}
}

type failingNetworkQoSLister struct {
	nqoslister.NetworkQoSLister
}

func (failingNetworkQoSLister) List(labels.Selector) ([]*nqostype.NetworkQoS, error) {
	return nil, fmt.Errorf("policy lookup failed")
}

func TestNetworkQoSLookupErrorPreservesEvents(t *testing.T) {
	c, _ := newEventTestController(t)
	c.nqosLister = failingNetworkQoSLister{}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns"}}
	c.onNQOSPodAdd(pod)
	c.onNQOSNamespaceAdd(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns"}})
	if c.nqosPodQueue.Len() != 1 || c.nqosNamespaceQueue.Len() != 1 {
		t.Fatal("lookup failure must preserve events for worker retries")
	}
	if err := c.syncNetworkQoSPod(newEventData(nil, pod)); err == nil {
		t.Fatal("expected policy lookup failure to reach worker")
	}
}

func BenchmarkPodEventsWithoutNetworkQoS(b *testing.B) {
	c, _ := newEventTestController(b)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", ResourceVersion: "1"}}
	updated := pod.DeepCopy()
	updated.ResourceVersion = "2"
	updated.Labels = map[string]string{"app": "selected"}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c.onNQOSPodAdd(pod)
		c.onNQOSPodUpdate(pod, updated)
		c.onNQOSPodDelete(updated)
	}
	if c.nqosPodQueue.Len() != 0 {
		b.Fatal("pod events queued with no policies")
	}
}
