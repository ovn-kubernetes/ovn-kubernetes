// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"reflect"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

func TestQueueMapEntryRemainsCompact(t *testing.T) {
	if size := reflect.TypeOf(queueMapEntry{}).Size(); size != 8 {
		t.Fatalf("per-object bookkeeping grew to %d bytes", size)
	}
}

func cleanupTestQueueMap() *queueMap {
	return newQueueMap(10, 2, &sync.WaitGroup{}, make(chan struct{}))
}

func TestInactiveSlotForgetsDeletedPod(t *testing.T) {
	for _, tombstone := range []bool{false, true} {
		qm := cleanupTestQueueMap()
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns"}}
		key, entry := qm.getQueueMapEntry(PodType, pod)
		qm.releaseQueueMapEntry(key, entry, false)
		if len(qm.entries) != 1 {
			t.Fatal("expected idle bookkeeping after add")
		}
		inf := &informer{oType: PodType, internalInformers: []*internalInformer{{queueMap: qm}}}
		var deleted interface{} = pod
		if tombstone {
			deleted = cache.DeletedFinalStateUnknown{Key: "ns/pod", Obj: pod}
		}
		inf.newFederatedQueuedHandler(0).OnDelete(deleted)
		if len(qm.entries) != 0 {
			t.Fatal("inactive slot retained deleted Pod name")
		}
		for _, queue := range qm.queues {
			if len(queue) != 0 {
				t.Fatal("inactive slot enqueued a callback")
			}
		}
	}
}

func TestDeleteRetainsInFlightSerialization(t *testing.T) {
	qm := cleanupTestQueueMap()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns"}}
	key, first := qm.getQueueMapEntry(PodType, pod)
	qm.forgetDeletedObject(PodType, pod)
	_, reused := qm.getQueueMapEntry(PodType, pod)
	if first != reused || first.queue != reused.queue {
		t.Fatal("name reuse bypassed the still-running callback's queue")
	}
	qm.releaseQueueMapEntry(key, first, false)
	if qm.entries[key] != reused {
		t.Fatal("deleted mapping with an event still in flight")
	}
	qm.releaseQueueMapEntry(key, reused, false)
	if len(qm.entries) != 0 {
		t.Fatal("final non-delete event retained a deleted mapping")
	}
}

func TestQueuedDeleteFollowedByAddReleasesMapping(t *testing.T) {
	qm := cleanupTestQueueMap()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns"}}
	key, deleted := qm.getQueueMapEntry(PodType, pod)
	_, added := qm.getQueueMapEntry(PodType, pod)
	qm.releaseQueueMapEntry(key, deleted, true)
	if qm.entries[key] != added {
		t.Fatal("delete discarded a mapping before the following add finished")
	}
	qm.releaseQueueMapEntry(key, added, false)
	if len(qm.entries) != 0 {
		t.Fatal("delete followed by add retained bookkeeping")
	}
}

func TestQueueMapConcurrentDeleteAndRelease(t *testing.T) {
	qm := cleanupTestQueueMap()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns"}}
	for n := 0; n < 1000; n++ {
		key, entry := qm.getQueueMapEntry(PodType, pod)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			qm.forgetDeletedObject(PodType, pod)
		}()
		go func() {
			defer wg.Done()
			_, next := qm.getQueueMapEntry(PodType, pod)
			qm.releaseQueueMapEntry(key, next, false)
			qm.releaseQueueMapEntry(key, entry, false)
		}()
		wg.Wait()
		if len(qm.entries) != 0 {
			t.Fatalf("iteration %d retained a deleted name", n)
		}
	}
}
