// Package informer provides a wrapper around a client-go SharedIndexInformer
// for event handling. It removes a lot of boilerplate code that is required
// when using workqueues
package informer

import (
	"fmt"
	"regexp"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	addEvent    = "add"
	updateEvent = "upd"
	deleteEvent = "del"
)

// EventHandler is an event handler that responds to
// Add/Update/Delete events from the informer cache
// Add and Updates use the provided add function
// Deletes use the provided delete function
type EventHandler interface {
	Run(threadiness int, stopChan <-chan struct{}) error
	GetIndexer() cache.Indexer
	Synced() bool
}

type eventHandler struct {
	// name of the handler. used in log messages
	name string
	// informer is the underlying SharedIndexInformer that we're wrapping
	informer cache.SharedIndexInformer
	// updatedIndexer stores copies of objects *before* they were updated
	// this allows for an update handler to have access to the old/new object
	updatedIndexer cache.Indexer
	// deletedIndexer stores copies of objects that have been deleted from
	// the cache. This is needed as the OVN controller Delete functions expect
	// to have a copy of the object that needs deleting
	deletedIndexer cache.Indexer
	// workqueue is the queue we use to store work
	workqueue workqueue.RateLimitingInterface
	// add is the handler function that gets called when something has been added
	add func(obj interface{}) error
	// update is the handler function that gets called when something has been updated
	update func(old, new interface{}) error
	// delete is handler function that gets called when something has been deleted
	delete func(obj interface{}) error
	// updateFilter is the function that we use to evaluate whether an update
	// should be enqueued or not. This is required to avoid an unrelated annotation
	// change triggering hundreds of OVN commands being run
	updateFilter UpdateFilterFunction
}

// UpdateFilterFunction returns true if the update is interesting
type UpdateFilterFunction func(old, new interface{}) bool

// ReceiveAllUpdates always returns true
// meaning that all updates will be enqueued
func ReceiveAllUpdates(old, new interface{}) bool {
	return true
}

// DiscardAllUpdates always returns false, discarding updates
func DiscardAllUpdates(old, new interface{}) bool {
	return false
}

// EventHandlerCreateFunction is function that creates new event handlers
type EventHandlerCreateFunction func(
	name string,
	informer cache.SharedIndexInformer,
	addFunc, deleteFunc func(obj interface{}) error,
	updateFunc func(old, new interface{}) error,
	updateFilterFunc UpdateFilterFunction,
) EventHandler

// NewDefaultEventHandler returns a new default event handler.
// The default event handler:
// - Enqueue Adds to the workqueue
// - Enqueue Updates to the workqueue if they match the predicate function, and stores a copy of the old object in the updatedIndexer
// - Enqueue Deletes to the workqueue, and places the deleted object on the deletedIndexer
// As the workqueue doesn't carry type information, adds/updates/deletes for a key are all colllapsed.
func NewDefaultEventHandler(
	name string,
	informer cache.SharedIndexInformer,
	addFunc, deleteFunc func(obj interface{}) error,
	updateFunc func(old, new interface{}) error,
	updateFilterFunc UpdateFilterFunction,
) EventHandler {
	e := &eventHandler{
		name:           name,
		informer:       informer,
		deletedIndexer: cache.NewIndexer(cache.DeletionHandlingMetaNamespaceKeyFunc, cache.Indexers{}),
		updatedIndexer: cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{}),
		workqueue:      workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), name),
		add:            addFunc,
		delete:         deleteFunc,
		update:         updateFunc,
		updateFilter:   updateFilterFunc,
	}
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// always enqueue adds
			e.enqueue(addEvent, obj)
		},
		UpdateFunc: func(old, new interface{}) {
			oldObj := old.(metav1.Object)
			newObj := new.(metav1.Object)
			// Make sure object was actually changed.
			if oldObj.GetResourceVersion() == newObj.GetResourceVersion() {
				return
			}
			// check the update aginst the predicate functions
			if e.updateFilter(old, new) {
				if err := e.updatedIndexer.Add(old); err != nil {
					utilruntime.HandleError(err)
				}
				// enqueue if it matches
				e.enqueue(updateEvent, new)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// always enqueue deletes
			e.enqueue(deleteEvent, obj)
		},
	})
	return e
}

// NewTestEventHandler returns an event handler similar to NewEventHandler.
// The only difference is that it ignores the ResourceVersion check.
func NewTestEventHandler(
	name string,
	informer cache.SharedIndexInformer,
	addFunc, deleteFunc func(obj interface{}) error,
	updateFunc func(old, new interface{}) error,
	updateFilterFunc UpdateFilterFunction,
) EventHandler {
	e := &eventHandler{
		name:           name,
		informer:       informer,
		deletedIndexer: cache.NewIndexer(cache.DeletionHandlingMetaNamespaceKeyFunc, cache.Indexers{}),
		updatedIndexer: cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{}),
		workqueue:      workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		add:            addFunc,
		delete:         deleteFunc,
		update:         updateFunc,
		updateFilter:   updateFilterFunc,
	}
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// always enqueue adds
			e.enqueue(addEvent, obj)
		},
		UpdateFunc: func(old, new interface{}) {
			newObj := new.(metav1.Object)
			// check the update aginst the predicate functions
			if e.updateFilter(old, new) {
				if err := e.updatedIndexer.Add(old); err != nil {
					utilruntime.HandleError(err)
				}
				// enqueue if it matches
				e.enqueue(updateEvent, newObj)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// always enqueue deletes
			e.enqueue(deleteEvent, obj)
		},
	})
	return e
}

// GetIndexer returns the indexer that is associated with
// the SharedInformer. This is required for a consumer of
// this wrapper to create Listers
func (e *eventHandler) GetIndexer() cache.Indexer {
	return e.informer.GetIndexer()
}

func (e *eventHandler) Synced() bool {
	return e.informer.HasSynced()
}

// Run starts event processing for the eventHandler.
// It waits for the informer cache to sync before starting the provided number of threads.
// It will block until stopCh is closed, at which point it will shutdown
// the workqueue and wait for workers to finish processing their current work items.
func (e *eventHandler) Run(threadiness int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()

	klog.Infof("Starting %s informer queue", e.name)

	klog.Infof("Waiting for %s informer caches to sync", e.name)
	// wait for caches to be in sync before we start the workers
	if ok := cache.WaitForCacheSync(stopCh, e.informer.HasSynced); !ok {
		return fmt.Errorf("failed to wait for %s caches to sync", e.name)
	}

	klog.Infof("Starting %d %s queue workers", threadiness, e.name)
	// start our worker threads
	wg := &sync.WaitGroup{}
	for j := 0; j < threadiness; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(e.runWorker, time.Second, stopCh)
		}()
	}

	klog.Infof("Started %s queue workers", e.name)
	// wait until the channel is closed
	<-stopCh

	klog.Infof("Shutting down %s queue workers", e.name)
	e.workqueue.ShutDown()
	wg.Wait()
	klog.Infof("Shut down %s queue workers", e.name)

	return nil
}

// enqueue adds an item to the workqueue
func (e *eventHandler) enqueue(event string, obj interface{}) {
	var key string
	var err error

	if event != deleteEvent {
		// ignore objects that are already set for deletion
		if !obj.(metav1.Object).GetDeletionTimestamp().IsZero() {
			return
		}
		// get the key for our object
		if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
			utilruntime.HandleError(err)
			return
		}
	} else {
		// get the key for our object, which may have been deleted already
		if key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err != nil {
			utilruntime.HandleError(err)
			return
		}
		// add to the deletedIndexer
		if err := e.deletedIndexer.Add(obj); err != nil {
			utilruntime.HandleError(err)
			return
		}
	}

	key = fmt.Sprintf("%s#%s", event, key)
	// enqueue
	e.workqueue.Add(key)
}

// runWorker is invoked by our worker threads
// it executes processNextWorkItem until the queue is shutdown
func (e *eventHandler) runWorker() {
	for e.processNextWorkItem() {
	}
}

// processNextWorkItem processes work items from the queue
func (e *eventHandler) processNextWorkItem() bool {
	// get item from the queue
	obj, shutdown := e.workqueue.Get()

	// if we have to shutdown, return now
	if shutdown {
		return false
	}

	// process the item
	err := func(obj interface{}) error {
		// make sure we call Done on the object once we've finshed processing
		defer e.workqueue.Done(obj)

		var key string
		var ok bool
		// items on the queue should always be strings
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			e.workqueue.Forget(obj)
			return fmt.Errorf("expected string in workqueue but got %#v", obj)
		}

		// Run the syncHandler, passing it the namespace/name string of the
		// resource to be synced.
		if err := e.syncHandler(key); err != nil {
			// Put the item back on the workqueue to handle any transient errors
			// if it hasn't already been requeued more times than our MaxRetries
			if e.workqueue.NumRequeues(key) < MaxRetries {
				e.workqueue.AddRateLimited(key)
				return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
			}
			// if we've exceeded MaxRetries, remove the item from the queue
			e.workqueue.Forget(obj)
			return fmt.Errorf("dropping %s from %s queue as it has failed more than %d times", key, e.name, MaxRetries)
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		e.workqueue.Forget(obj)
		klog.Infof("Successfully synced '%s'", key)

		return nil
	}(obj)

	// handle any errors that occurred
	if err != nil {
		utilruntime.HandleError(err)
	}

	// work complete!
	return true
}

// syncHandler fetches an item from the informer's cache
// and dispatches to the relevant handler function
func (e *eventHandler) syncHandler(key string) error {
	// split the event type from the key
	r := regexp.MustCompile(`^(add|upd|del)#(.*)$`)
	matches := r.FindStringSubmatch(key)
	if len(matches) != 3 {
		return fmt.Errorf("%s is not a valid key", key)
	}
	eventType := matches[1]
	objectKey := matches[2]

	if eventType == deleteEvent {
		// get the deleted object from the deletedIndexer
		// this shouldn't error, or fail to exist but we handle these cases to be thorough
		obj, exists, err := e.deletedIndexer.GetByKey(objectKey)
		if err != nil {
			return fmt.Errorf("error getting object with key %s from deletedIndexer: %v", objectKey, err)
		}
		if !exists {
			return fmt.Errorf("key %s doesn't exist in deletedIndexer: %v", objectKey, err)
		}
		// call the eventHandler's delete function for this object
		if err := e.delete(obj); err != nil {
			return err
		}
		// finally, remove the deleted object from the deletedIndexer
		return e.deletedIndexer.Delete(obj)
	}

	// get the object from the informer cache
	obj, _, err := e.informer.GetIndexer().GetByKey(objectKey)
	if err != nil {
		return fmt.Errorf("error fetching object with key %s from store: %v", key, err)
	}

	if eventType == updateEvent {
		// get the old object from the updatedIndexer
		oldObj, exists, err := e.updatedIndexer.GetByKey(objectKey)
		if err != nil {
			return fmt.Errorf("error getting object with key %s from updatedIndexer: %v", objectKey, err)
		}
		if !exists {
			return fmt.Errorf("key %s doesn't exist in updatedIndexer: %v", objectKey, err)
		}
		// call the eventHandler update function for this object
		if err := e.update(oldObj, obj); err != nil {
			return err
		}
		// finally, remove the deleted object from the deletedIndexer
		return e.updatedIndexer.Delete(oldObj)
	}
	// call the eventHandler's add function in the case of add/update
	return e.add(obj)
}
