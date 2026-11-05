// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package userdefinednetwork

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
)

type statusTestClock struct{ now time.Time }

func (c *statusTestClock) Now() time.Time       { return c.now }
func (c *statusTestClock) Step(d time.Duration) { c.now = c.now.Add(d) }

func nodeCountCondition(now time.Time, count int) *metav1.Condition {
	status := metav1.ConditionTrue
	if count == 0 {
		status = metav1.ConditionFalse
	}
	return &metav1.Condition{Type: "NodesSelected", Status: status, Reason: "DynamicAllocation", Message: fmt.Sprintf("%d nodes", count), LastTransitionTime: metav1.NewTime(now)}
}

func statusPublisherFixture(t *testing.T) (*nodeCountStatusPublisher, *statusTestClock, *[]metav1.Condition, *[]time.Duration) {
	t.Helper()
	clock := &statusTestClock{now: time.Unix(100, 0)}
	published := []metav1.Condition{}
	delays := []time.Duration{}
	p := newNodeCountStatusPublisher(func(_ context.Context, _ string, _ types.UID, _ int64, condition *metav1.Condition) error {
		published = append(published, *condition)
		return nil
	})
	p.now = clock.Now
	p.enqueue = func(_ string, delay time.Duration) { delays = append(delays, delay) }
	t.Cleanup(func() { p.cancel(); controller.Stop(p.reconciler) })
	return p, clock, &published, &delays
}

func TestNodeCountStatusCoalescesAndFlushes(t *testing.T) {
	p, clock, published, delays := statusPublisherFixture(t)
	p.update("network", "uid", 1, nodeCountCondition(clock.Now(), 1), nil)
	if err := p.reconcile(context.Background(), "network"); err != nil {
		t.Fatal(err)
	}
	transition := (*published)[0].LastTransitionTime
	for count := 2; count <= 100; count++ {
		clock.Step(5 * time.Millisecond)
		p.update("network", "uid", 1, nodeCountCondition(clock.Now(), count), nil)
		if err := p.reconcile(context.Background(), "network"); err != nil {
			t.Fatal(err)
		}
	}
	if len(*published) != 1 {
		t.Fatalf("published every count: %d", len(*published))
	}
	if (*delays)[len(*delays)-1] >= nodeCountStatusInterval {
		t.Fatal("deadline reset by count updates")
	}
	clock.Step(time.Second)
	if err := p.reconcile(context.Background(), "network"); err != nil {
		t.Fatal(err)
	}
	if len(*published) != 2 || (*published)[1].Message != "100 nodes" {
		t.Fatalf("final count not flushed: %v", *published)
	}
	if !(*published)[1].LastTransitionTime.Equal(&transition) {
		t.Fatal("count changed transition time")
	}
	if err := p.reconcile(context.Background(), "network"); err != nil {
		t.Fatal(err)
	}
	if len(*published) != 2 {
		t.Fatal("delayed stale queue item duplicated write")
	}
}

func TestNodeCountStatusTransitionsBypassDelay(t *testing.T) {
	p, clock, published, delays := statusPublisherFixture(t)
	p.update("network", "uid", 1, nodeCountCondition(clock.Now(), 1), nil)
	if err := p.reconcile(context.Background(), "network"); err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{0, 1} {
		clock.Step(time.Millisecond)
		p.update("network", "uid", 1, nodeCountCondition(clock.Now(), count), nil)
		if (*delays)[len(*delays)-1] != 0 {
			t.Fatal("status transition delayed")
		}
		if err := p.reconcile(context.Background(), "network"); err != nil {
			t.Fatal(err)
		}
	}
	if len(*published) != 3 {
		t.Fatal("missing transition")
	}
	previousTime := (*published)[2].LastTransitionTime
	p.update("network", "uid", 2, nodeCountCondition(clock.Now(), 2), nil)
	if (*delays)[len(*delays)-1] != 0 {
		t.Fatal("generation change delayed")
	}
	if err := p.reconcile(context.Background(), "network"); err != nil {
		t.Fatal(err)
	}
	if (*published)[3].ObservedGeneration != 2 || !(*published)[3].LastTransitionTime.Equal(&previousTime) {
		t.Fatal("generation transition semantics lost")
	}
}

func TestNodeCountStatusErrorSupersedesPendingCount(t *testing.T) {
	p, clock, published, delays := statusPublisherFixture(t)
	p.update("network", "uid", 1, nodeCountCondition(clock.Now(), 1), nil)
	if err := p.reconcile(context.Background(), "network"); err != nil {
		t.Fatal(err)
	}
	p.update("network", "uid", 1, nodeCountCondition(clock.Now(), 2), nil)
	failure := nodeCountCondition(clock.Now(), 0)
	failure.Reason = "AllocationFailed"
	p.update("network", "uid", 1, failure, nil)
	if (*delays)[len(*delays)-1] != 0 {
		t.Fatal("error transition delayed")
	}
	if err := p.reconcile(context.Background(), "network"); err != nil {
		t.Fatal(err)
	}
	clock.Step(time.Second)
	if err := p.reconcile(context.Background(), "network"); err != nil {
		t.Fatal(err)
	}
	if len(*published) != 2 || (*published)[1].Reason != "AllocationFailed" {
		t.Fatal("pending count overwrote error")
	}
}

func TestNodeCountStatusRetriesLatestAfterFailure(t *testing.T) {
	p, clock, published, _ := statusPublisherFixture(t)
	original := p.publish
	p.publish = func(context.Context, string, types.UID, int64, *metav1.Condition) error {
		return errors.New("API unavailable")
	}
	p.update("network", "uid", 1, nodeCountCondition(clock.Now(), 1), nil)
	if err := p.reconcile(context.Background(), "network"); err == nil {
		t.Fatal("failure not propagated for retry")
	}
	p.update("network", "uid", 1, nodeCountCondition(clock.Now(), 7), nil)
	p.publish = original
	if err := p.reconcile(context.Background(), "network"); err != nil {
		t.Fatal(err)
	}
	if len(*published) != 1 || (*published)[0].Message != "7 nodes" {
		t.Fatalf("retry used stale count: %v", *published)
	}
}

func TestNodeCountStatusUpdateDuringPublish(t *testing.T) {
	p, clock, published, _ := statusPublisherFixture(t)
	p.update("network", "uid", 1, nodeCountCondition(clock.Now(), 1), nil)
	if err := p.reconcile(context.Background(), "network"); err != nil {
		t.Fatal(err)
	}
	original := p.publish
	p.publish = func(ctx context.Context, network string, uid types.UID, generation int64, condition *metav1.Condition) error {
		// Return to the previously published state while the opposite transition
		// is in flight. It must not be mistaken for a no-op after the write lands.
		p.update(network, uid, generation, nodeCountCondition(clock.Now(), 1), nil)
		return original(ctx, network, uid, generation, condition)
	}
	p.update("network", "uid", 1, nodeCountCondition(clock.Now(), 0), nil)
	if err := p.reconcile(context.Background(), "network"); err != nil {
		t.Fatal(err)
	}
	p.publish = original
	if err := p.reconcile(context.Background(), "network"); err != nil {
		t.Fatal(err)
	}
	if len(*published) != 3 || (*published)[2].Status != metav1.ConditionTrue {
		t.Fatalf("in-flight update lost: %v", *published)
	}
}

func TestNodeCountStatusRecreationAndShutdown(t *testing.T) {
	p, clock, published, _ := statusPublisherFixture(t)
	p.update("network", "old", 1, nodeCountCondition(clock.Now(), 1), nil)
	if err := p.reconcile(context.Background(), "network"); err != nil {
		t.Fatal(err)
	}
	p.update("network", "old", 1, nodeCountCondition(clock.Now(), 2), nil)
	p.forget("network")
	clock.Step(time.Second)
	if err := p.reconcile(context.Background(), "network"); err != nil {
		t.Fatal(err)
	}
	if len(*published) != 1 {
		t.Fatal("deleted resource published")
	}
	p.update("network", "new", 1, nodeCountCondition(clock.Now(), 9), nil)
	if err := p.reconcile(context.Background(), "network"); err != nil {
		t.Fatal(err)
	}
	if len(*published) != 2 || (*published)[1].Message != "9 nodes" {
		t.Fatal("new UID inherited old pending count")
	}
	p.update("network", "new", 1, nodeCountCondition(clock.Now(), 10), nil)
	p.cancel()
	clock.Step(time.Second)
	if err := p.reconcile(context.Background(), "network"); err != nil {
		t.Fatal(err)
	}
	if len(*published) != 2 {
		t.Fatal("published after shutdown")
	}
}
