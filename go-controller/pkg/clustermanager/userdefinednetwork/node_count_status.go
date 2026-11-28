// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package userdefinednetwork

import (
	"context"
	"reflect"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const nodeCountStatusInterval = time.Second

type nodeCountStatus struct {
	uid         types.UID
	generation  int64
	desired     metav1.Condition
	published   *metav1.Condition
	publishedAt time.Time
	dirty       bool
}

type nodeCountStatusPublisher struct {
	mu         sync.Mutex
	entries    map[string]*nodeCountStatus
	now        func() time.Time
	enqueue    func(string, time.Duration)
	publish    func(context.Context, string, types.UID, int64, *metav1.Condition) error
	ctx        context.Context
	cancel     context.CancelFunc
	reconciler controller.Reconciler
}

func newNodeCountStatusPublisher(publish func(context.Context, string, types.UID, int64, *metav1.Condition) error) *nodeCountStatusPublisher {
	ctx, cancel := context.WithCancel(context.Background())
	p := &nodeCountStatusPublisher{entries: make(map[string]*nodeCountStatus), now: time.Now, publish: publish, ctx: ctx, cancel: cancel}
	p.reconciler = controller.NewReconciler("udn-node-count-status", &controller.ReconcilerConfig{
		Reconcile: p.reconcile, Threadiness: 2, MaxAttempts: controller.InfiniteAttempts,
	})
	p.enqueue = func(key string, delay time.Duration) {
		if delay > 0 {
			p.reconciler.ReconcileAfter(key, delay)
		} else {
			p.reconciler.Reconcile(key)
		}
	}
	return p
}

func sameNodeCountState(a, b *metav1.Condition) bool {
	return a != nil && b != nil && a.Status == b.Status && a.Reason == b.Reason && a.ObservedGeneration == b.ObservedGeneration && a.Type == b.Type
}

func (p *nodeCountStatusPublisher) update(network string, uid types.UID, generation int64, condition *metav1.Condition, previous *metav1.Condition) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil {
		return
	}
	entry := p.entries[network]
	if entry == nil || entry.uid != uid {
		entry = &nodeCountStatus{uid: uid, generation: generation}
		if previous != nil {
			entry.published = previous.DeepCopy()
		}
		p.entries[network] = entry
	}
	desired := *condition
	desired.ObservedGeneration = generation
	// Preserve transition time across count, reason and generation changes.
	reference := entry.published
	if entry.dirty {
		reference = &entry.desired
	}
	if reference != nil && reference.Status == desired.Status {
		desired.LastTransitionTime = reference.LastTransitionTime
	}
	if entry.dirty && reflect.DeepEqual(entry.desired, desired) {
		return
	}
	entry.desired = desired
	entry.generation = generation
	entry.dirty = entry.published == nil || !reflect.DeepEqual(*entry.published, desired)
	if !entry.dirty {
		return
	}
	p.enqueue(network, p.delay(entry))
}

func (p *nodeCountStatusPublisher) delay(entry *nodeCountStatus) time.Duration {
	// Only a True/True NodesSelected message change is count-only. Real status,
	// reason and generation transitions bypass the publication interval.
	if entry.desired.Status != metav1.ConditionTrue || entry.desired.Reason != "DynamicAllocation" || !sameNodeCountState(entry.published, &entry.desired) {
		return 0
	}
	remaining := entry.publishedAt.Add(nodeCountStatusInterval).Sub(p.now())
	if remaining > 0 {
		return remaining
	}
	return 0
}

func (p *nodeCountStatusPublisher) reconcile(_ context.Context, network string) error {
	p.mu.Lock()
	entry := p.entries[network]
	if entry == nil || !entry.dirty || p.ctx.Err() != nil {
		p.mu.Unlock()
		return nil
	}
	if delay := p.delay(entry); delay > 0 {
		p.enqueue(network, delay)
		p.mu.Unlock()
		return nil
	}
	snapshot := *entry
	p.mu.Unlock()

	if err := p.publish(p.ctx, network, snapshot.uid, snapshot.generation, &snapshot.desired); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	entry = p.entries[network]
	if entry == nil || entry.uid != snapshot.uid {
		return nil
	}
	entry.published = snapshot.desired.DeepCopy()
	entry.publishedAt = p.now()
	entry.dirty = !reflect.DeepEqual(entry.desired, *entry.published)
	if entry.dirty {
		p.enqueue(network, p.delay(entry))
	}
	return nil
}

func (p *nodeCountStatusPublisher) forget(network string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, network)
}

func (c *Controller) publishNodeCountStatus(ctx context.Context, network string, uid types.UID, generation int64, condition *metav1.Condition) error {
	namespace, name := util.ParseNetworkName(network)
	if namespace == "" {
		obj, err := c.cudnLister.Get(name)
		if apierrors.IsNotFound(err) {
			c.nodeCountStatus.forget(network)
			return nil
		}
		if err != nil {
			return err
		}
		if obj.UID != uid || obj.Generation != generation {
			return nil
		}
	} else {
		obj, err := c.udnLister.UserDefinedNetworks(namespace).Get(name)
		if apierrors.IsNotFound(err) {
			c.nodeCountStatus.forget(network)
			return nil
		}
		if err != nil {
			return err
		}
		if obj.UID != uid || obj.Generation != generation {
			return nil
		}
	}
	return c.applySubsystemCondition(ctx, network, uid, "ClusterManager", condition)
}
