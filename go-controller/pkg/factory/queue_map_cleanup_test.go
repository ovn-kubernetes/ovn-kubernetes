// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

func cleanupTestQueueMap() *queueMap {
	return newQueueMap(10, 2, &sync.WaitGroup{}, make(chan struct{}))
}

func stopQueueMapOnCleanup(t *testing.T, qm *queueMap, beforeStop func()) func() {
	t.Helper()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			if beforeStop != nil {
				beforeStop()
			}
			shutdownQueueMap(t, qm)
		})
	}
	t.Cleanup(stop)
	return stop
}

type queueMapDoneGate struct {
	workqueue.TypedInterface[types.NamespacedName]
	firstDone        chan struct{}
	releaseFirstDone chan struct{}
	secondDone       chan struct{}
	doneCalls        int
}

func (q *queueMapDoneGate) Done(key types.NamespacedName) {
	q.TypedInterface.Done(key)
	q.doneCalls++
	switch q.doneCalls {
	case 1:
		close(q.firstDone)
		<-q.releaseFirstDone
	case 2:
		close(q.secondDone)
	}
}

type queueMapGetGate struct {
	workqueue.TypedInterface[types.NamespacedName]
	firstGet   chan struct{}
	releaseGet chan struct{}
	getOnce    sync.Once
}

func (q *queueMapGetGate) Get() (types.NamespacedName, bool) {
	q.getOnce.Do(func() {
		close(q.firstGet)
		<-q.releaseGet
	})
	return q.TypedInterface.Get()
}

func TestInactiveSlotForgetsDeletedPod(t *testing.T) {
	for _, tombstone := range []bool{false, true} {
		qm := cleanupTestQueueMap()
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns"}}
		key := types.NamespacedName{Namespace: "ns", Name: "pod"}
		qm.Lock()
		qm.entries[key] = &queueMapEntry{queue: 0}
		qm.Unlock()

		inf := &informer{oType: PodType, internalInformers: []*internalInformer{{queueMap: qm}}}
		var deleted interface{} = pod
		if tombstone {
			deleted = cache.DeletedFinalStateUnknown{Key: "ns/pod", Obj: pod}
		}
		inf.newFederatedQueuedHandler(0).OnDelete(deleted)

		qm.Lock()
		entryCount := len(qm.entries)
		qm.Unlock()
		if entryCount != 0 {
			t.Fatal("inactive slot retained deleted Pod name")
		}
		for _, queue := range qm.queues {
			if queue.Len() != 0 {
				t.Fatal("inactive slot enqueued a callback")
			}
		}
	}
}

func TestDeleteRetainsInFlightSerialization(t *testing.T) {
	qm := cleanupTestQueueMap()
	qm.start()
	release := make(chan struct{})
	var releaseOnce sync.Once
	stop := stopQueueMapOnCleanup(t, qm, func() { releaseOnce.Do(func() { close(release) }) })

	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", UID: types.UID("old")}}
	started := make(chan struct{})
	processed := make(chan types.UID, 2)
	qm.enqueueEvent(nil, oldPod, PodType, false, func(e *event) {
		close(started)
		<-release
		processed <- e.obj.(metav1.Object).GetUID()
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("initial callback did not start")
	}

	key := types.NamespacedName{Namespace: "ns", Name: "pod"}
	qm.Lock()
	entry := qm.entries[key]
	queue := entry.queue
	processing := entry.processing
	qm.Unlock()
	if !processing {
		t.Fatal("queue entry was not marked as processing")
	}

	qm.forgetDeletedObject(PodType, oldPod)
	newPod := oldPod.DeepCopy()
	newPod.UID = types.UID("new")
	qm.enqueueEvent(nil, newPod, PodType, false, func(e *event) {
		processed <- e.obj.(metav1.Object).GetUID()
	})

	qm.Lock()
	gotEntry := qm.entries[key]
	retained := gotEntry == entry && gotEntry.queue == queue && len(gotEntry.pending) == 1
	qm.Unlock()
	if !retained {
		t.Fatal("name reuse did not retain the in-flight queue mapping")
	}

	releaseOnce.Do(func() { close(release) })
	for _, want := range []types.UID{"old", "new"} {
		select {
		case got := <-processed:
			if got != want {
				t.Fatalf("processed UID = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for UID %q", want)
		}
	}
	stop()

	qm.Lock()
	entryCount := len(qm.entries)
	qm.Unlock()
	if entryCount != 0 {
		t.Fatal("deleted name remained after queued callbacks completed")
	}
}

func TestQueuedDeleteFollowedByAddReleasesMapping(t *testing.T) {
	qm := cleanupTestQueueMap()
	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", UID: types.UID("old")}}
	newPod := oldPod.DeepCopy()
	newPod.UID = types.UID("new")
	processed := make(chan eventType, 2)
	qm.enqueueEvent(nil, oldPod, PodType, true, func(e *event) { processed <- e.eventType })
	qm.enqueueEvent(nil, newPod, PodType, false, func(e *event) { processed <- e.eventType })
	qm.start()
	stop := stopQueueMapOnCleanup(t, qm, nil)

	for _, want := range []eventType{eventTypeDelete, eventTypeAdd} {
		select {
		case got := <-processed:
			if got != want {
				t.Fatalf("processed event type = %v, want %v", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event type %v", want)
		}
	}
	stop()

	qm.Lock()
	entryCount := len(qm.entries)
	qm.Unlock()
	if entryCount != 0 {
		t.Fatal("delete followed by add retained queue bookkeeping")
	}
}

func TestQueueMapIgnoresStaleTokenOnFormerQueue(t *testing.T) {
	qm := cleanupTestQueueMap()
	queueA := &queueMapDoneGate{
		TypedInterface:   qm.queues[0],
		firstDone:        make(chan struct{}),
		releaseFirstDone: make(chan struct{}),
		secondDone:       make(chan struct{}),
	}
	queueB := &queueMapGetGate{
		TypedInterface: qm.queues[1],
		firstGet:       make(chan struct{}),
		releaseGet:     make(chan struct{}),
	}
	qm.queues[0] = queueA
	qm.queues[1] = queueB

	releaseInitial := make(chan struct{})
	releaseCallbacks := make(chan struct{})
	var releaseInitialOnce, releaseFirstDoneOnce, releaseGetOnce, releaseCallbacksOnce sync.Once
	stop := stopQueueMapOnCleanup(t, qm, func() {
		releaseInitialOnce.Do(func() { close(releaseInitial) })
		releaseFirstDoneOnce.Do(func() { close(queueA.releaseFirstDone) })
		releaseGetOnce.Do(func() { close(queueB.releaseGet) })
		releaseCallbacksOnce.Do(func() { close(releaseCallbacks) })
	})
	qm.start()

	select {
	case <-queueB.firstGet:
	case <-time.After(time.Second):
		t.Fatal("queue B worker did not reach its gate")
	}

	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", UID: types.UID("old")}}
	initialStarted := make(chan struct{})
	qm.enqueueEvent(nil, oldPod, PodType, false, func(*event) {
		close(initialStarted)
		<-releaseInitial
	})
	select {
	case <-initialStarted:
	case <-time.After(time.Second):
		t.Fatal("initial callback did not start")
	}

	oldUpdate := oldPod.DeepCopy()
	oldUpdate.ResourceVersion = "2"
	oldUpdateProcessed := make(chan struct{}, 1)
	qm.enqueueEvent(oldPod, oldUpdate, PodType, false, func(*event) {
		oldUpdateProcessed <- struct{}{}
	})
	releaseInitialOnce.Do(func() { close(releaseInitial) })
	select {
	case <-oldUpdateProcessed:
	case <-time.After(time.Second):
		t.Fatal("queued update did not drain")
	}
	select {
	case <-queueA.firstDone:
	case <-time.After(time.Second):
		t.Fatal("first queue item did not finish")
	}

	key := types.NamespacedName{Namespace: "ns", Name: "pod"}
	qm.Lock()
	_, oldEntryRetained := qm.entries[key]
	qm.Unlock()
	if oldEntryRetained {
		t.Fatal("idle queue entry was not removed before its dirty token was requeued")
	}
	if got := queueA.Len(); got != 1 {
		t.Fatalf("stale token count on queue A = %d, want 1", got)
	}

	newPod := oldPod.DeepCopy()
	newPod.UID = types.UID("new")
	callbackStarted := []chan struct{}{make(chan struct{}, 1), make(chan struct{}, 1)}
	callbacksDone := make(chan struct{}, 2)
	var activeCallbacks, maxActiveCallbacks atomic.Int32
	newCallback := func(started chan struct{}) func(*event) {
		return func(*event) {
			active := activeCallbacks.Add(1)
			for {
				previousMax := maxActiveCallbacks.Load()
				if active <= previousMax || maxActiveCallbacks.CompareAndSwap(previousMax, active) {
					break
				}
			}
			started <- struct{}{}
			<-releaseCallbacks
			activeCallbacks.Add(-1)
			callbacksDone <- struct{}{}
		}
	}
	qm.enqueueEvent(nil, newPod, PodType, false, newCallback(callbackStarted[0]))
	qm.enqueueEvent(nil, newPod, PodType, true, newCallback(callbackStarted[1]))

	qm.Lock()
	newEntry := qm.entries[key]
	if newEntry == nil {
		qm.Unlock()
		t.Fatal("replacement queue entry was not created")
	}
	newQueue, pending := newEntry.queue, len(newEntry.pending)
	qm.Unlock()
	if newQueue != 1 {
		t.Fatalf("replacement queue = %d, want queue B (1)", newQueue)
	}
	if pending != 2 {
		t.Fatalf("replacement pending events = %d, want 2", pending)
	}

	// Release queue A first while queue B is held. A stale worker must complete
	// the token without taking either callback from the new entry.
	releaseFirstDoneOnce.Do(func() { close(queueA.releaseFirstDone) })
	select {
	case <-queueA.secondDone:
		releaseGetOnce.Do(func() { close(queueB.releaseGet) })
		select {
		case <-callbackStarted[0]:
		case <-time.After(time.Second):
			t.Fatal("replacement add callback did not start")
		}
		select {
		case <-callbackStarted[1]:
			t.Error("replacement delete callback started concurrently with the add callback")
		default:
		}
	case <-callbackStarted[0]:
		// On the buggy path, queue A has stolen the first callback. Let queue B
		// consume its own token so the overlap becomes observable.
		releaseGetOnce.Do(func() { close(queueB.releaseGet) })
		select {
		case <-callbackStarted[1]:
		case <-time.After(time.Second):
			t.Error("queue B did not expose concurrent draining of the replacement entry")
		}
	case <-time.After(time.Second):
		t.Fatal("stale queue token was neither completed nor processed")
	}
	releaseCallbacksOnce.Do(func() { close(releaseCallbacks) })
	for i := 0; i < 2; i++ {
		select {
		case <-callbacksDone:
		case <-time.After(time.Second):
			t.Fatal("replacement callback did not complete")
		}
	}
	if got := maxActiveCallbacks.Load(); got != 1 {
		t.Errorf("maximum concurrent callbacks for one key = %d, want 1", got)
	}

	stop()
	qm.Lock()
	entryCount := len(qm.entries)
	qm.Unlock()
	if entryCount != 0 {
		t.Fatal("replacement entry remained after queued callbacks completed")
	}
}

func TestQueueMapConcurrentDeleteAndRelease(t *testing.T) {
	qm := cleanupTestQueueMap()
	qm.start()
	release := make(chan struct{})
	var releaseOnce sync.Once
	stop := stopQueueMapOnCleanup(t, qm, func() { releaseOnce.Do(func() { close(release) }) })

	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", UID: types.UID("old")}}
	newPod := oldPod.DeepCopy()
	newPod.UID = types.UID("new")
	started := make(chan struct{})
	processed := make(chan types.UID, 2)
	qm.enqueueEvent(nil, oldPod, PodType, false, func(e *event) {
		close(started)
		<-release
		processed <- e.obj.(metav1.Object).GetUID()
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("initial callback did not start")
	}

	key := types.NamespacedName{Namespace: "ns", Name: "pod"}
	qm.Lock()
	entry := qm.entries[key]
	queue := entry.queue
	qm.Unlock()

	var ops sync.WaitGroup
	ops.Add(2)
	go func() {
		defer ops.Done()
		qm.forgetDeletedObject(PodType, oldPod)
	}()
	go func() {
		defer ops.Done()
		qm.enqueueEvent(nil, newPod, PodType, false, func(e *event) {
			processed <- e.obj.(metav1.Object).GetUID()
		})
	}()
	ops.Wait()

	qm.Lock()
	gotEntry := qm.entries[key]
	retained := gotEntry == entry && gotEntry.queue == queue && gotEntry.processing && len(gotEntry.pending) == 1
	qm.Unlock()
	if !retained {
		t.Fatal("concurrent delete and enqueue lost the in-flight queue mapping")
	}

	releaseOnce.Do(func() { close(release) })
	for _, want := range []types.UID{"old", "new"} {
		select {
		case got := <-processed:
			if got != want {
				t.Fatalf("processed UID = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for UID %q", want)
		}
	}
	stop()

	qm.Lock()
	entryCount := len(qm.entries)
	qm.Unlock()
	if entryCount != 0 {
		t.Fatal("deleted name remained after concurrent queue activity")
	}
}
