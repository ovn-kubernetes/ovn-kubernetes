// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package pod

import (
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
}

func (f *fakePodHandler) GetNetworkName() string {
	return f.netName
}

func (f *fakePodHandler) SyncPods(_ []*corev1.Pod) error {
	f.syncCalls++
	return nil
}

func (f *fakePodHandler) GetPodState(pod *corev1.Pod) interface{} {
	key := pod.Namespace + "/" + pod.Name
	if f.state != nil {
		return f.state[key]
	}
	return string(pod.UID)
}

func (f *fakePodHandler) PodExpectedOnNetwork(pod *corev1.Pod) (bool, error) {
	key := pod.Namespace + "/" + pod.Name
	return f.expect[key], nil
}

func (f *fakePodHandler) ReconcilePod(oldPod, newPod *corev1.Pod, cachedState interface{}) error {
	if newPod == nil {
		f.deleteCalls++
		f.events = append(f.events, "delete:"+string(oldPod.UID))
		f.lastDeletePod = oldPod.DeepCopy()
		f.lastState = cachedState
		return nil
	}
	f.reconcileCalls++
	f.events = append(f.events, "reconcile:"+string(newPod.UID))
	if oldPod != nil {
		f.lastOldPod = oldPod.DeepCopy()
	} else {
		f.lastOldPod = nil
	}
	return nil
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
		handlers:    syncmap.NewSyncMap[NetworkHandler](),
		podLocks:    syncmap.NewSyncMap[struct{}](),
		appliedPods: map[string]map[string]*appliedPodState{},
	}
}

func TestRegisterNetworkControllerBootstrapsExpectedPodsOnly(t *testing.T) {
	podA := newPod("ns", "a", "uid-a")
	podB := newPod("ns", "b", "uid-b")
	podC := newPod("ns", "c", "uid-c")
	c := newPodControllerForTest(t, podA, podB, podC)
	handler := &fakePodHandler{
		netName: "net-a",
		expect: map[string]bool{
			"ns/a": true,
			"ns/b": false,
			"ns/c": true,
		},
		state: map[string]interface{}{
			"ns/a": "state-a",
		},
	}

	if err := c.RegisterNetworkController(handler); err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}

	if handler.syncCalls != 1 {
		t.Fatalf("expected one sync call, got %d", handler.syncCalls)
	}
	if applied := c.getAppliedPod(handler.netName, "ns/a"); applied == nil {
		t.Fatal("expected ns/a to be recorded as applied")
	}
	if applied := c.getAppliedPod(handler.netName, "ns/b"); applied != nil {
		t.Fatal("expected ns/b to be skipped")
	}
	if applied := c.getAppliedPod(handler.netName, "ns/c"); applied != nil {
		t.Fatal("expected ns/c without existing state to be reconciled as a new add")
	}
	if err := c.reconcilePodForNetwork(handler, "ns/c", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if handler.lastOldPod != nil {
		t.Fatalf("expected ns/c to reconcile as add, got old pod UID %s", handler.lastOldPod.UID)
	}
}

func TestReconcilePodUIDChangeDeletesBeforeReconcile(t *testing.T) {
	oldPod := newPod("ns", "pod", "old")
	newPod := newPod("ns", "pod", "new")
	c := newPodControllerForTest(t, newPod)
	handler := &fakePodHandler{
		netName: "net-a",
		expect:  map[string]bool{"ns/pod": true},
	}
	c.setAppliedPod(handler.netName, "ns/pod", oldPod, "old-state")

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
	applied := c.getAppliedPod(handler.netName, "ns/pod")
	if applied == nil || applied.pod.UID != newPod.UID {
		t.Fatalf("expected applied pod to be updated to new UID, got %#v", applied)
	}
}

func TestReconcilePodDeleteClearsAppliedState(t *testing.T) {
	oldPod := newPod("ns", "pod", "old")
	c := newPodControllerForTest(t)
	handler := &fakePodHandler{netName: "net-a", expect: map[string]bool{}}
	c.setAppliedPod(handler.netName, "ns/pod", oldPod, "old-state")

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	if handler.deleteCalls != 1 {
		t.Fatalf("expected one delete, got %d", handler.deleteCalls)
	}
	if handler.lastDeletePod.UID != oldPod.UID || handler.lastState != "old-state" {
		t.Fatalf("unexpected delete input pod=%#v state=%#v", handler.lastDeletePod, handler.lastState)
	}
	if applied := c.getAppliedPod(handler.netName, "ns/pod"); applied != nil {
		t.Fatal("expected applied state to be cleared")
	}
}

func TestReconcilePodAbsentWithoutAppliedStateDoesNotLeak(t *testing.T) {
	c := newPodControllerForTest(t)
	handler := &fakePodHandler{netName: "net-a", expect: map[string]bool{}}

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if handler.deleteCalls != 0 || len(c.appliedPods) != 0 {
		t.Fatalf("expected no delete and no applied state, got deletes=%d applied=%#v", handler.deleteCalls, c.appliedPods)
	}
}

func TestPodNeedsUpdateRefreshesDeletePodForSameUID(t *testing.T) {
	oldPod := newPod("ns", "pod", "uid")
	oldPod.Status.Phase = corev1.PodRunning
	latestPod := oldPod.DeepCopy()
	latestPod.Status.Phase = corev1.PodSucceeded
	c := newPodControllerForTest(t)
	handler := &fakePodHandler{netName: "net-a", expect: map[string]bool{}}
	c.setAppliedPod(handler.netName, "ns/pod", oldPod, "old-state")

	if !c.podNeedsUpdate(oldPod, latestPod) {
		t.Fatal("expected pod update to require reconcile")
	}
	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	if handler.lastDeletePod.Status.Phase != corev1.PodSucceeded {
		t.Fatalf("expected delete to use latest same-UID pod status, got %s", handler.lastDeletePod.Status.Phase)
	}
}

func TestPodNeedsUpdateDoesNotRefreshDeletePodForDifferentUID(t *testing.T) {
	oldPod := newPod("ns", "pod", "old")
	oldPod.Status.Phase = corev1.PodRunning
	newPod := newPod("ns", "pod", "new")
	newPod.Status.Phase = corev1.PodSucceeded
	c := newPodControllerForTest(t)
	handler := &fakePodHandler{netName: "net-a", expect: map[string]bool{}}
	c.setAppliedPod(handler.netName, "ns/pod", oldPod, "old-state")

	if !c.podNeedsUpdate(oldPod, newPod) {
		t.Fatal("expected pod update to require reconcile")
	}
	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	if handler.lastDeletePod.UID != oldPod.UID || handler.lastDeletePod.Status.Phase != corev1.PodRunning {
		t.Fatalf("expected delete to keep old applied pod, got uid=%s phase=%s", handler.lastDeletePod.UID, handler.lastDeletePod.Status.Phase)
	}
}
