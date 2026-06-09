// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

// TestOnDeleteUnwrapsTombstone verifies onDelete extracts the key from a
// cache.DeletedFinalStateUnknown tombstone (delivered on a missed delete or
// relist) and enqueues it, rather than dropping the delete. Without
// DeletionHandlingMetaNamespaceKeyFunc the key func errors on the tombstone
// and the delete is silently lost.
func TestOnDeleteUnwrapsTombstone(t *testing.T) {
	c := NewController("tombstone-test", &ControllerConfig[corev1.Pod]{
		Reconcile: func(string) error { return nil },
	}).(*controller[corev1.Pod])

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p1"}}
	c.onDelete(cache.DeletedFinalStateUnknown{Key: "ns/p1", Obj: pod})

	if got := c.queue.Len(); got != 1 {
		t.Fatalf("tombstone delete: expected 1 enqueued key, got %d", got)
	}
	key, _ := c.queue.Get()
	if key != "ns/p1" {
		t.Fatalf("tombstone delete: expected key %q, got %q", "ns/p1", key)
	}
}
