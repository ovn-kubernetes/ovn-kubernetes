// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes/fake"
	listers "k8s.io/client-go/listers/core/v1"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func coordinatorFixture() (*corev1.Pod, *fake.Clientset, listers.PodLister) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pod", UID: "uid", ResourceVersion: "1", Annotations: map[string]string{types.OvnPodAnnotationName: `{}`}}}
	client := fake.NewSimpleClientset(pod)
	// Deliberately never deliver informer updates.
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	_ = indexer.Add(pod.DeepCopy())
	return pod, client, listers.NewPodLister(indexer)
}

func addNetwork(name string, rollback func()) AllocateToPodWithRollbackFunc {
	return func(pod *corev1.Pod) (*corev1.Pod, func(), error) {
		networks := map[string]string{}
		if err := json.Unmarshal([]byte(pod.Annotations[types.OvnPodAnnotationName]), &networks); err != nil {
			return nil, nil, err
		}
		networks[name] = name
		data, err := json.Marshal(networks)
		if err != nil {
			return nil, nil, err
		}
		pod.Annotations[types.OvnPodAnnotationName] = string(data)
		return pod, rollback, nil
	}
}

func TestPodAnnotationCoordinatorSharesCommittedState(t *testing.T) {
	pod, client, lister := coordinatorFixture()
	c := NewPodAnnotationUpdateCoordinator(client)
	firstRead := make(chan struct{})
	releaseRead := make(chan struct{})
	var gets atomic.Int32
	client.PrependReactor("get", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		if gets.Add(1) == 1 {
			close(firstRead)
			<-releaseRead
		}
		return false, nil, nil
	})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Update(context.Background(), lister, &kube.Kube{KClient: client}, pod, addNetwork(fmt.Sprint(i), nil)); err != nil {
				t.Error(err)
			}
		}()
	}
	<-firstRead
	deadline := time.After(5 * time.Second)
	for {
		c.mu.Lock()
		users := c.operations[podAnnotationKey{namespace: pod.Namespace, name: pod.Name, uid: pod.UID}].users
		c.mu.Unlock()
		if users == 8 {
			break
		}
		select {
		case <-deadline:
			close(releaseRead)
			t.Fatal("writers did not queue")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(releaseRead)
	wg.Wait()
	if gets.Load() != 1 {
		t.Fatalf("expected one GET for batch, got %d", gets.Load())
	}
	current, err := client.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var networks map[string]string
	if err := json.Unmarshal([]byte(current.Annotations[types.OvnPodAnnotationName]), &networks); err != nil {
		t.Fatal(err)
	}
	if len(networks) != 8 {
		t.Fatalf("lost entries: %v", networks)
	}
	if len(c.operations) != 0 {
		t.Fatal("completed pod retained")
	}
}

func TestPodAnnotationCoordinatorExternalConflict(t *testing.T) {
	pod, client, lister := coordinatorFixture()
	c := NewPodAnnotationUpdateCoordinator(client)
	var calls, rollbacks int
	client.PrependReactor("patch", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls != 1 {
			return false, nil, nil
		}
		external := pod.DeepCopy()
		external.ResourceVersion = "2"
		external.Annotations[types.OvnPodAnnotationName] = `{"external":"external"}`
		if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), external, pod.Namespace); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, field.ErrorList{field.Invalid(field.NewPath("metadata", "annotations"), "old", "test failed")})
	})
	if err := c.Update(context.Background(), lister, &kube.Kube{KClient: client}, pod, addNetwork("manager", func() { rollbacks++ })); err != nil {
		t.Fatal(err)
	}
	current, err := client.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if current.Annotations[types.OvnPodAnnotationName] != `{"external":"external","manager":"manager"}` || calls != 2 || rollbacks != 1 {
		t.Fatalf("bad retry: %v calls=%d rollbacks=%d", current.Annotations, calls, rollbacks)
	}
}

func TestPodAnnotationCoordinatorReadsBackAmbiguousCommit(t *testing.T) {
	pod, client, lister := coordinatorFixture()
	c := NewPodAnnotationUpdateCoordinator(client)
	client.PrependReactor("patch", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		committed := pod.DeepCopy()
		committed.ResourceVersion = "2"
		committed.Annotations[types.OvnPodAnnotationName] = `{"manager":"manager","external":"external"}`
		if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), committed, pod.Namespace); err != nil {
			return true, nil, err
		}
		return true, nil, context.DeadlineExceeded
	})
	rolledBack := false
	if err := c.Update(context.Background(), lister, &kube.Kube{KClient: client}, pod, addNetwork("manager", func() { rolledBack = true })); err != nil {
		t.Fatal(err)
	}
	if rolledBack {
		t.Fatal("rolled back a committed allocation")
	}
}

func TestPodAnnotationCoordinatorCancellationAndPodIsolation(t *testing.T) {
	c := NewPodAnnotationUpdateCoordinator(nil)
	key := podAnnotationKey{namespace: "ns", name: "pod", uid: "uid"}
	_, release, err := c.acquire(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.acquire(ctx, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
	_, releaseOther, err := c.acquire(context.Background(), podAnnotationKey{namespace: "ns", name: "other", uid: "other"})
	if err != nil {
		t.Fatal(err)
	}
	releaseOther()
	release()
	if len(c.operations) != 0 {
		t.Fatal("waiter entry leaked")
	}
}

func TestPodAnnotationCoordinatorRejectsRecreatedPod(t *testing.T) {
	pod, client, lister := coordinatorFixture()
	recreated := pod.DeepCopy()
	recreated.UID = "new"
	if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), recreated, pod.Namespace); err != nil {
		t.Fatal(err)
	}
	c := NewPodAnnotationUpdateCoordinator(client)
	allocated := false
	err := c.Update(context.Background(), lister, &kube.Kube{KClient: client}, pod, func(p *corev1.Pod) (*corev1.Pod, func(), error) { allocated = true; return p, nil, nil })
	if !apierrors.IsNotFound(err) || allocated {
		t.Fatalf("recreated pod allocated: %v", err)
	}
}

func TestPodAnnotationCoordinatorNoopStillReconciles(t *testing.T) {
	pod, client, lister := coordinatorFixture()
	c := NewPodAnnotationUpdateCoordinator(client)
	called := false
	err := c.Update(context.Background(), lister, &kube.Kube{KClient: client}, pod, func(*corev1.Pod) (*corev1.Pod, func(), error) { called = true; return nil, nil, nil })
	if err != nil || !called {
		t.Fatalf("no-op callback not reconciled: %v", err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" {
			t.Fatal("no-op performed patch")
		}
	}
}

func TestPodAnnotationReadbackChecksOwnedChanges(t *testing.T) {
	annotation := func(value string) map[string]string { return map[string]string{types.OvnPodAnnotationName: value} }
	tests := []struct {
		name, before, after, current string
		committed                    bool
	}{
		{"sibling addition", `{}`, `{"a":{"ip":"one"}}`, `{"a":{"ip":"one"},"b":{"ip":"two"}}`, true},
		{"different allocation", `{}`, `{"a":{"ip":"one"}}`, `{"a":{"ip":"two"}}`, false},
		{"entry removed", `{"a":{}}`, `{}`, `{"b":{}}`, true},
		{"null is not deletion", `{"a":{}}`, `{}`, `{"a":null}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := podAnnotationChangesPresent(annotation(tt.before), annotation(tt.after), annotation(tt.current)); got != tt.committed {
				t.Fatalf("committed=%v, want %v", got, tt.committed)
			}
		})
	}
}
