// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package pod

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
)

type fakePodHandler struct {
	netName string
	expect  map[string]bool
	state   map[string]interface{}

	syncCalls      int
	reconcileCalls int
	deleteCalls    int
	events         []string
	lastOldPod     *corev1.Pod
	lastDeletePod  *corev1.Pod
	lastState      interface{}
	reconcileErr   error
	recordedErrors []string
}

func (f *fakePodHandler) GetNetworkName() string {
	return f.netName
}

func (f *fakePodHandler) SyncPods(_ []*corev1.Pod) error {
	f.syncCalls++
	return nil
}

func (f *fakePodHandler) PodExpectedOnNetwork(pod *corev1.Pod) (bool, error) {
	key := pod.Namespace + "/" + pod.Name
	return f.expect[key], nil
}

func (f *fakePodHandler) ReconcilePod(oldPod, newPod *corev1.Pod, lastState interface{}) (interface{}, error) {
	if f.reconcileErr != nil {
		return nil, f.reconcileErr
	}
	if newPod == nil {
		f.deleteCalls++
		f.events = append(f.events, "delete:"+string(oldPod.UID))
		f.lastDeletePod = oldPod
		f.lastState = lastState
		return nil, nil
	}
	f.reconcileCalls++
	f.events = append(f.events, "reconcile:"+string(newPod.UID))
	f.lastOldPod = oldPod
	key := newPod.Namespace + "/" + newPod.Name
	if f.state != nil {
		return f.state[key], nil
	}
	return string(newPod.UID), nil
}

func (f *fakePodHandler) RecordPodError(pod *corev1.Pod, reason string, err error) {
	f.recordedErrors = append(f.recordedErrors, reason+":"+string(pod.UID)+":"+err.Error())
}

func newPod(namespace, name, uid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			UID:       types.UID(uid),
		},
	}
}

func newPodLister(t *testing.T, pods ...*corev1.Pod) corelisters.PodLister {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, pod := range pods {
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("failed to add pod to indexer: %v", err)
		}
	}
	return corelisters.NewPodLister(indexer)
}

func newPodControllerForTest(t *testing.T, pods ...*corev1.Pod) *Controller {
	t.Helper()
	return &Controller{
		name:      "pod-test",
		podLister: newPodLister(t, pods...),
		podController: controller.NewController("pod-test-controller", &controller.ControllerConfig[corev1.Pod]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:   func(string) error { return nil },
			Threadiness: 1,
			Lister: func(labels.Selector) ([]*corev1.Pod, error) {
				return nil, nil
			},
			ObjNeedsUpdate: func(_, _ *corev1.Pod) bool { return true },
		}),
		podLocks: syncmap.NewSyncMap[struct{}](),
		networks: map[string]*activeNetwork{},
		entries:  map[string]map[string]*podEntry{},
	}
}

func (c *Controller) entryForTest(netName, podKey string) *podEntry {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.entries[netName][podKey]
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestRegisterNetworkControllerQueuesAndSignalsInitialReconcile(t *testing.T) {
	podA := newPod("ns", "a", "uid-a")
	podB := newPod("ns", "b", "uid-b")
	c := newPodControllerForTest(t, podA, podB)
	handler := &fakePodHandler{
		netName: "net-a",
		expect: map[string]bool{
			"ns/a": true,
			"ns/b": false,
		},
	}

	done, err := c.RegisterNetworkController(handler)
	if err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}
	if handler.syncCalls != 1 {
		t.Fatalf("expected one sync call, got %d", handler.syncCalls)
	}
	// Registration only queues work; nothing is reconciled inline.
	if handler.reconcileCalls != 0 {
		t.Fatalf("expected no inline reconcile during registration, got %d", handler.reconcileCalls)
	}
	if isClosed(done) {
		t.Fatal("expected initial reconcile barrier to be open")
	}

	// Drain the queued keys the way a worker would.
	if err := c.reconcilePod("ns/a|net-a"); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if isClosed(done) {
		t.Fatal("expected barrier to stay open until every pod is attempted")
	}
	if err := c.reconcilePod("ns/b|net-a"); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if !isClosed(done) {
		t.Fatal("expected barrier to close after every pod was attempted")
	}

	if handler.reconcileCalls != 1 {
		t.Fatalf("expected only the expected pod to be reconciled, got %d", handler.reconcileCalls)
	}
	if entry := c.entryForTest("net-a", "ns/a"); entry == nil || entry.applied == nil || entry.state != "uid-a" {
		t.Fatalf("expected applied entry with state for ns/a, got %#v", entry)
	}
	if entry := c.entryForTest("net-a", "ns/b"); entry != nil {
		t.Fatalf("expected no entry for unexpected pod, got %#v", entry)
	}
}

func TestRegisterNetworkControllerBarrierClosesOnFailedAttempts(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{
		netName:      "net-a",
		expect:       map[string]bool{"ns/pod": true},
		reconcileErr: errors.New("boom"),
	}

	done, err := c.RegisterNetworkController(handler)
	if err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}
	if err := c.reconcilePod("ns/pod|net-a"); err == nil {
		t.Fatal("expected reconcile error")
	}
	// A failed first attempt still counts: a wedged pod must not block
	// dependent controllers forever; it stays on the retry queue.
	if !isClosed(done) {
		t.Fatal("expected barrier to close after failed attempt")
	}
}

func TestDeregisterNetworkControllerClearsState(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{netName: "net-a", expect: map[string]bool{"ns/pod": true}}

	if _, err := c.RegisterNetworkController(handler); err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}
	if err := c.reconcilePod("ns/pod|net-a"); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	c.DeregisterNetworkController("net-a")
	if c.getNetwork("net-a") != nil {
		t.Fatal("expected network to be deregistered")
	}
	if entry := c.entryForTest("net-a", "ns/pod"); entry != nil {
		t.Fatalf("expected entries to be cleared, got %#v", entry)
	}
	// Reconciles for a deregistered network are silent no-ops.
	if err := c.reconcilePod("ns/pod|net-a"); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if handler.reconcileCalls != 1 {
		t.Fatalf("expected no reconcile after deregistration, got %d", handler.reconcileCalls)
	}
}

func TestReconcilePodUIDChangeDeletesBeforeReconcile(t *testing.T) {
	oldPod := newPod("ns", "pod", "old")
	newerPod := newPod("ns", "pod", "new")
	c := newPodControllerForTest(t, newerPod)
	handler := &fakePodHandler{
		netName: "net-a",
		expect:  map[string]bool{"ns/pod": true},
	}
	c.markApplied(handler.netName, "ns/pod", oldPod, "old-state")

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	if handler.deleteCalls != 1 || handler.reconcileCalls != 1 {
		t.Fatalf("expected one delete and one reconcile, got deletes=%d reconciles=%d", handler.deleteCalls, handler.reconcileCalls)
	}
	if got := handler.events; len(got) != 2 || got[0] != "delete:old" || got[1] != "reconcile:new" {
		t.Fatalf("unexpected event order: %#v", got)
	}
	if handler.lastState != "old-state" {
		t.Fatalf("expected old cached state, got %#v", handler.lastState)
	}
	entry := c.entryForTest(handler.netName, "ns/pod")
	if entry == nil || entry.applied == nil || entry.applied.UID != newerPod.UID {
		t.Fatalf("expected applied entry updated to new UID, got %#v", entry)
	}
}

func TestReconcilePodDeleteClearsEntry(t *testing.T) {
	oldPod := newPod("ns", "pod", "old")
	c := newPodControllerForTest(t)
	handler := &fakePodHandler{netName: "net-a", expect: map[string]bool{}}
	c.markApplied(handler.netName, "ns/pod", oldPod, "old-state")

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	if handler.deleteCalls != 1 {
		t.Fatalf("expected one delete, got %d", handler.deleteCalls)
	}
	if handler.lastDeletePod.UID != oldPod.UID || handler.lastState != "old-state" {
		t.Fatalf("unexpected delete input pod=%#v state=%#v", handler.lastDeletePod, handler.lastState)
	}
	if entry := c.entryForTest(handler.netName, "ns/pod"); entry != nil {
		t.Fatal("expected entry to be cleared")
	}
}

func TestReconcilePodRecordsUpdateError(t *testing.T) {
	oldPod := newPod("ns", "pod", "uid")
	currentPod := newPod("ns", "pod", "uid")
	c := newPodControllerForTest(t, currentPod)
	handler := &fakePodHandler{
		netName:      "net-a",
		expect:       map[string]bool{"ns/pod": true},
		reconcileErr: errors.New("boom"),
	}
	c.markApplied(handler.netName, "ns/pod", oldPod, "old-state")

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err == nil {
		t.Fatal("expected reconcile error")
	}

	expected := "ErrorUpdatingResource:uid:boom"
	if len(handler.recordedErrors) != 1 || handler.recordedErrors[0] != expected {
		t.Fatalf("expected recorded error %q, got %#v", expected, handler.recordedErrors)
	}
}

func TestReconcilePodRecordsErrorOncePerFailureStreak(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{
		netName:      "net-a",
		expect:       map[string]bool{"ns/pod": true},
		reconcileErr: errors.New("boom"),
	}

	// Reconciles retry indefinitely; repeated identical failures must not
	// flood the recorder with duplicate events.
	for i := 0; i < 3; i++ {
		if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err == nil {
			t.Fatal("expected reconcile error")
		}
	}
	if len(handler.recordedErrors) != 1 {
		t.Fatalf("expected one recorded error for repeated failure, got %#v", handler.recordedErrors)
	}

	// A different error is recorded again.
	handler.reconcileErr = errors.New("bang")
	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err == nil {
		t.Fatal("expected reconcile error")
	}
	if len(handler.recordedErrors) != 2 {
		t.Fatalf("expected second recorded error after error change, got %#v", handler.recordedErrors)
	}

	// After a successful reconcile the next failure starts a new streak.
	handler.reconcileErr = nil
	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	handler.reconcileErr = errors.New("bang")
	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err == nil {
		t.Fatal("expected reconcile error")
	}
	if len(handler.recordedErrors) != 3 {
		t.Fatalf("expected recorded error after new failure streak, got %#v", handler.recordedErrors)
	}
}

func TestReconcilePodRecordsDeleteError(t *testing.T) {
	oldPod := newPod("ns", "pod", "old")
	c := newPodControllerForTest(t)
	handler := &fakePodHandler{
		netName:      "net-a",
		expect:       map[string]bool{},
		reconcileErr: errors.New("boom"),
	}
	c.markApplied(handler.netName, "ns/pod", oldPod, "old-state")

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err == nil {
		t.Fatal("expected reconcile error")
	}

	expected := "ErrorDeletingResource:old:boom"
	if len(handler.recordedErrors) != 1 || handler.recordedErrors[0] != expected {
		t.Fatalf("expected recorded error %q, got %#v", expected, handler.recordedErrors)
	}
}

func TestReconcilePodAbsentWithoutEntryDoesNotLeak(t *testing.T) {
	c := newPodControllerForTest(t)
	handler := &fakePodHandler{netName: "net-a", expect: map[string]bool{}}

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if handler.deleteCalls != 0 || len(c.entries) != 0 {
		t.Fatalf("expected no delete and no entries, got deletes=%d entries=%#v", handler.deleteCalls, c.entries)
	}
}

func TestReconcilePodDeleteAfterFailedAddRunsTeardown(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{
		netName:      "net-a",
		expect:       map[string]bool{"ns/pod": true},
		reconcileErr: errors.New("boom"),
	}

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err == nil {
		t.Fatal("expected reconcile error")
	}

	// The pod is deleted before a retry succeeds. Partially applied state
	// from the failed add must still be torn down.
	c.podLister = newPodLister(t)
	handler.reconcileErr = nil

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if handler.deleteCalls != 1 {
		t.Fatalf("expected teardown after failed add, got %d deletes", handler.deleteCalls)
	}
	if handler.lastDeletePod.UID != pod.UID || handler.lastState != nil {
		t.Fatalf("unexpected delete input pod=%#v state=%#v", handler.lastDeletePod, handler.lastState)
	}
	if entry := c.entryForTest(handler.netName, "ns/pod"); entry != nil {
		t.Fatal("expected entry to be cleared")
	}
}

func TestReconcileCompletedUnexpectedPodWithoutEntryRunsDelete(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	pod.Status.Phase = corev1.PodFailed
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{netName: "net-a", expect: map[string]bool{"ns/pod": false}}

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	if handler.deleteCalls != 1 {
		t.Fatalf("expected one delete, got %d", handler.deleteCalls)
	}
	if handler.lastDeletePod.UID != pod.UID || handler.lastState != nil {
		t.Fatalf("unexpected delete input pod=%#v state=%#v", handler.lastDeletePod, handler.lastState)
	}
	if entry := c.entryForTest(handler.netName, "ns/pod"); entry != nil {
		t.Fatal("expected no entry after fallback delete")
	}
}

func TestPodNeedsUpdateRefreshesLastSeenForSameUID(t *testing.T) {
	oldPod := newPod("ns", "pod", "uid")
	oldPod.Status.Phase = corev1.PodRunning
	latestPod := oldPod.DeepCopy()
	latestPod.Status.Phase = corev1.PodSucceeded
	c := newPodControllerForTest(t)
	handler := &fakePodHandler{netName: "net-a", expect: map[string]bool{}}
	c.markApplied(handler.netName, "ns/pod", oldPod, "old-state")

	if !c.podNeedsUpdate(oldPod, latestPod) {
		t.Fatal("expected podNeedsUpdate to be true")
	}

	deletePod, state := c.teardownPod(handler.netName, "ns/pod")
	if deletePod == nil || deletePod.Status.Phase != corev1.PodSucceeded {
		t.Fatalf("expected teardown to use latest informer pod, got %#v", deletePod)
	}
	if state != "old-state" {
		t.Fatalf("expected applied state to survive refresh, got %#v", state)
	}
}

func TestPodNeedsUpdateDoesNotRefreshLastSeenForDifferentUID(t *testing.T) {
	oldPod := newPod("ns", "pod", "old")
	replacementPod := newPod("ns", "pod", "new")
	c := newPodControllerForTest(t)
	handler := &fakePodHandler{netName: "net-a", expect: map[string]bool{}}
	c.markApplied(handler.netName, "ns/pod", oldPod, "old-state")

	if !c.podNeedsUpdate(oldPod, replacementPod) {
		t.Fatal("expected podNeedsUpdate to be true")
	}

	deletePod, _ := c.teardownPod(handler.netName, "ns/pod")
	if deletePod == nil || deletePod.UID != oldPod.UID {
		t.Fatalf("expected teardown to keep the applied pod for a different UID, got %#v", deletePod)
	}
}
