// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"fmt"
	"strings"
	"sync"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
)

const (
	// podAllocationWorkers is how many pods have their allocation reconciled at
	// the same time, all networks together. Each one may wait for the API
	// server to accept the pod annotation.
	podAllocationWorkers = 15

	networkObjectKeySeparator = "|"
)

// allocationControllers are the controllers shared by the networks that
// allocate to pods.
type allocationControllers struct {
	pods *networkObjectController[corev1.Pod]
	// ipamClaims is only there with persistent IPs enabled
	ipamClaims *networkObjectController[ipamclaimsapi.IPAMClaim]
}

func newAllocationControllers(wf *factory.WatchFactory) *allocationControllers {
	podInformer := wf.PodCoreInformer()
	c := &allocationControllers{
		pods: newNetworkObjectController("pod-allocation", podInformer.Informer(),
			podInformer.Lister().List, podNeedsAllocation, podAllocationWorkers),
	}
	if config.OVNKubernetesFeature.EnablePersistentIPs {
		ipamClaimInformer := wf.IPAMClaimsInformer()
		c.ipamClaims = newNetworkObjectController("ipamclaim-release", ipamClaimInformer.Informer(),
			ipamClaimInformer.Lister().List,
			// IPAMClaims only matter once deleted, to release their IPs
			func(_, _ *ipamclaimsapi.IPAMClaim) bool { return false }, 1)
	}
	return c
}

func (c *allocationControllers) Start() error {
	if c.ipamClaims != nil {
		if err := c.ipamClaims.Start(); err != nil {
			return err
		}
	}
	return c.pods.Start()
}

func (c *allocationControllers) Stop() {
	if c.ipamClaims != nil {
		c.ipamClaims.Stop()
	}
	c.pods.Stop()
}

// networkObjectHandler is what a network does with the objects of a
// networkObjectController.
type networkObjectHandler[T any] struct {
	// attached tells if an object may be on the network, and held if a deleted
	// object may have something to release on it. Most objects are not on most
	// networks: these are cheap ways to skip them, that say yes when they
	// can't tell.
	attached func(obj *T) bool
	held     func(obj *T) bool
	// reconcile makes sure the object that exists with this key, if any, has
	// what it needs on the network
	reconcile func(ctx context.Context, key string) error
	// release releases what a deleted object had on the network
	release func(ctx context.Context, obj *T) error
}

// registeredNetwork is a network registered to a networkObjectController.
type registeredNetwork[T any] struct {
	handler networkObjectHandler[T]
	// deleted are the deleted objects still to be released for the network
	deleted deletedObjects[T]
	// lock is held for reading while a key is reconciled for the network
	lock sync.RWMutex
	// active is set once the initial sync of the network is done, and until
	// the network is deregistered
	active bool
}

// networkObjectController reconciles one kind of object for all the networks
// registered to it. One queue and one set of workers serve all the networks.
// An event queues the key of its object, which is then queued again for each
// network the object is on, so that a network failing on an object is retried
// on its own.
type networkObjectController[T any] struct {
	controller     controller.Controller
	indexer        cache.Indexer
	lister         func(selector labels.Selector) ([]*T, error)
	objNeedsUpdate func(oldObj, newObj *T) bool

	// objectLocks serializes the reconciles of an object across networks:
	// the networks all write to the same object, as the annotation of a pod.
	objectLocks *syncmap.SyncMap[struct{}]

	lock     sync.RWMutex
	networks map[string]*registeredNetwork[T]
}

func newNetworkObjectController[T any](
	name string,
	informer cache.SharedIndexInformer,
	lister func(selector labels.Selector) ([]*T, error),
	objNeedsUpdate func(oldObj, newObj *T) bool,
	threadiness int,
) *networkObjectController[T] {
	c := &networkObjectController[T]{
		indexer:        informer.GetIndexer(),
		lister:         lister,
		objNeedsUpdate: objNeedsUpdate,
		objectLocks:    syncmap.NewSyncMap[struct{}](),
		networks:       map[string]*registeredNetwork[T]{},
	}
	c.controller = controller.NewController(name, &controller.ControllerConfig[T]{
		Informer:       informer,
		ObjNeedsUpdate: objNeedsUpdate,
		OnDelete:       c.onDelete,
		Reconcile:      c.reconcile,
		// Nothing else retries an allocation or a release that keeps failing.
		MaxAttempts: controller.InfiniteAttempts,
		Threadiness: threadiness,
	})
	return c
}

func (c *networkObjectController[T]) Start() error {
	return controller.Start(c.controller)
}

func (c *networkObjectController[T]) Stop() {
	controller.Stop(c.controller)
}

// Register makes the controller reconcile its objects for a network. The
// initial sync of the network runs before any object is reconciled for it.
func (c *networkObjectController[T]) Register(netName string, handler networkObjectHandler[T], initialSync func() error) error {
	network := &registeredNetwork[T]{handler: handler}
	c.lock.Lock()
	c.networks[netName] = network
	c.lock.Unlock()

	// The objects deleted from now on are kept for the network, so the ones
	// the initial sync sees and that are deleted right after are released.
	if err := initialSync(); err != nil {
		c.Deregister(netName)
		return fmt.Errorf("initial sync failed: %w", err)
	}
	network.lock.Lock()
	network.active = true
	network.lock.Unlock()

	// The keys queued for the network during its initial sync were skipped.
	objs, err := c.lister(labels.Everything())
	if err != nil {
		c.Deregister(netName)
		return fmt.Errorf("failed to list objects: %w", err)
	}
	for _, obj := range objs {
		if !c.objNeedsUpdate(nil, obj) || !handler.attached(obj) {
			continue
		}
		key, err := cache.MetaNamespaceKeyFunc(obj)
		if err != nil {
			c.Deregister(netName)
			return fmt.Errorf("failed to get the key of an object: %w", err)
		}
		c.ReconcileNetwork(key, netName)
	}
	for _, key := range network.deleted.keys() {
		c.ReconcileNetwork(key, netName)
	}
	return nil
}

// Deregister makes the controller stop reconciling its objects for a network.
// It returns once nothing is being reconciled for the network anymore.
func (c *networkObjectController[T]) Deregister(netName string) {
	c.lock.Lock()
	network := c.networks[netName]
	delete(c.networks, netName)
	c.lock.Unlock()
	if network == nil {
		return
	}
	network.lock.Lock()
	network.active = false
	network.lock.Unlock()
}

// ReconcileNetwork queues an object to be reconciled for one network.
func (c *networkObjectController[T]) ReconcileNetwork(key, netName string) {
	c.controller.Reconcile(key + networkObjectKeySeparator + netName)
}

// onDelete runs for every deleted object, before its key is reconciled:
// releasing needs what the object said, and it is gone from the informer by
// then.
func (c *networkObjectController[T]) onDelete(obj *T) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		klog.Errorf("Failed to get the key of a deleted object: %v", err)
		return
	}
	c.lock.RLock()
	defer c.lock.RUnlock()
	for _, network := range c.networks {
		if network.handler.held(obj) {
			network.deleted.add(key, obj)
		}
	}
}

// get returns the object that exists with this key, if any.
func (c *networkObjectController[T]) get(key string) (*T, error) {
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil || !exists {
		return nil, err
	}
	return obj.(*T), nil
}

// concerns tells if a network has something to do for a key.
func (n *registeredNetwork[T]) concerns(key string, obj *T) bool {
	return n.deleted.has(key) || (obj != nil && n.handler.attached(obj))
}

func (c *networkObjectController[T]) reconcile(ctx context.Context, queueKey string) error {
	key, netName, scoped := strings.Cut(queueKey, networkObjectKeySeparator)
	obj, err := c.get(key)
	if err != nil {
		return fmt.Errorf("failed to get object %s: %w", key, err)
	}
	if !scoped {
		// the key of an event: queue it for the networks it concerns
		c.lock.RLock()
		defer c.lock.RUnlock()
		for netName, network := range c.networks {
			if network.concerns(key, obj) {
				c.ReconcileNetwork(key, netName)
			}
		}
		return nil
	}

	c.lock.RLock()
	network := c.networks[netName]
	c.lock.RUnlock()
	if network == nil {
		return nil
	}
	network.lock.RLock()
	defer network.lock.RUnlock()
	if !network.active {
		return nil
	}
	// checked again, without waiting for the object: it may have changed
	if !network.concerns(key, obj) {
		return nil
	}
	c.objectLocks.LockKey(key)
	defer c.objectLocks.UnlockKey(key)

	// The deleted objects are released before the object is read: an object
	// deleted in between is then either not found, or released the next time
	// its key is reconciled, after what is allocated to it here.
	for _, deleted := range network.deleted.get(key) {
		if err := network.handler.release(ctx, deleted); err != nil {
			return fmt.Errorf("network %s: failed to release deleted object %s: %w", netName, key, err)
		}
		network.deleted.remove(key, deleted)
	}
	if err := network.handler.reconcile(ctx, key); err != nil {
		return fmt.Errorf("network %s: %w", netName, err)
	}
	return nil
}

// deletedObjects holds, per key, the objects that were deleted from the API
// server until what they held is released. An object deleted and created
// again under the same name before being reconciled leaves several of them
// under one key.
type deletedObjects[T any] struct {
	lock    sync.Mutex
	objects map[string][]*T
}

func (d *deletedObjects[T]) add(key string, obj *T) {
	d.lock.Lock()
	defer d.lock.Unlock()
	if d.objects == nil {
		d.objects = map[string][]*T{}
	}
	d.objects[key] = append(d.objects[key], obj)
}

// get returns the objects of a key, to be released and then removed.
func (d *deletedObjects[T]) get(key string) []*T {
	d.lock.Lock()
	defer d.lock.Unlock()
	return append([]*T(nil), d.objects[key]...)
}

// has tells if a key has objects.
func (d *deletedObjects[T]) has(key string) bool {
	d.lock.Lock()
	defer d.lock.Unlock()
	return len(d.objects[key]) > 0
}

// keys returns the keys that have objects.
func (d *deletedObjects[T]) keys() []string {
	d.lock.Lock()
	defer d.lock.Unlock()
	keys := make([]string, 0, len(d.objects))
	for key := range d.objects {
		keys = append(keys, key)
	}
	return keys
}

// remove drops an object get returned.
func (d *deletedObjects[T]) remove(key string, obj *T) {
	d.lock.Lock()
	defer d.lock.Unlock()
	objs := d.objects[key]
	for i := range objs {
		if objs[i] != obj {
			continue
		}
		objs = append(objs[:i:i], objs[i+1:]...)
		if len(objs) == 0 {
			delete(d.objects, key)
		} else {
			d.objects[key] = objs
		}
		return
	}
}
