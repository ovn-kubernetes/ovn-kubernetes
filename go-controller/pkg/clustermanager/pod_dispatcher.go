package clustermanager

import (
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// podDispatcher is a single event handler registered on the pod informer
// that dispatches pod events to the correct network controller's workqueue.
// This replaces per-network AddEventHandler registrations, reducing the number
// of processorListener goroutines from 40+ to 1 and eliminating API server
// watcher kills from back-pressure.
type podDispatcher struct {
	mu          sync.RWMutex
	controllers map[string]controller.Controller // namespace -> controller
	handler     cache.ResourceEventHandlerRegistration
}

func newPodDispatcher() *podDispatcher {
	return &podDispatcher{
		controllers: make(map[string]controller.Controller),
	}
}

// Start registers the single event handler on the pod informer.
func (d *podDispatcher) Start(wf *factory.WatchFactory) error {
	var err error
	d.handler, err = wf.PodCoreInformer().Informer().AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    d.onAdd,
			UpdateFunc: d.onUpdate,
			DeleteFunc: d.onDelete,
		}))
	if err != nil {
		return err
	}
	klog.Infof("Pod dispatcher started")
	return nil
}

// Stop removes the event handler.
func (d *podDispatcher) Stop(wf *factory.WatchFactory) {
	if d.handler != nil {
		if err := wf.PodCoreInformer().Informer().RemoveEventHandler(d.handler); err != nil {
			klog.Errorf("Failed to remove pod dispatcher handler: %v", err)
		}
	}
}

// AddController registers a network controller for the given namespaces.
func (d *podDispatcher) AddController(namespaces []string, ctrl controller.Controller) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ns := range namespaces {
		d.controllers[ns] = ctrl
	}
}

// RemoveController removes a network controller for the given namespaces.
func (d *podDispatcher) RemoveController(namespaces []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ns := range namespaces {
		delete(d.controllers, ns)
	}
}

func (d *podDispatcher) getController(namespace string) controller.Controller {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.controllers[namespace]
}

func (d *podDispatcher) onAdd(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	if !d.shouldEnqueue(nil, pod) {
		return
	}
	ctrl := d.getController(pod.Namespace)
	if ctrl == nil {
		return
	}
	ctrl.Reconcile(pod.Namespace + "/" + pod.Name)
}

func (d *podDispatcher) onUpdate(oldObj, newObj interface{}) {
	oldPod, ok := oldObj.(*corev1.Pod)
	if !ok {
		return
	}
	newPod, ok := newObj.(*corev1.Pod)
	if !ok {
		return
	}
	if !d.shouldEnqueue(oldPod, newPod) {
		return
	}
	ctrl := d.getController(newPod.Namespace)
	if ctrl == nil {
		return
	}
	ctrl.Reconcile(newPod.Namespace + "/" + newPod.Name)
}

func (d *podDispatcher) onDelete(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			return
		}
	}
	ctrl := d.getController(pod.Namespace)
	if ctrl == nil {
		return
	}
	ctrl.Reconcile(pod.Namespace + "/" + pod.Name)
}

func (d *podDispatcher) shouldEnqueue(oldPod, newPod *corev1.Pod) bool {
	if newPod == nil {
		return false
	}
	if !util.PodScheduled(newPod) {
		return false
	}
	if newPod.Annotations[util.OvnPodAnnotationName] == "" {
		return false
	}
	if oldPod == nil {
		return true
	}
	return oldPod.Annotations[util.OvnPodAnnotationName] != newPod.Annotations[util.OvnPodAnnotationName] ||
		oldPod.Spec.NodeName != newPod.Spec.NodeName
}
