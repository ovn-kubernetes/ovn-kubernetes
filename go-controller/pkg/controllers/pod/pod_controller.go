// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package pod

import (
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// NetworkHandler reconciles pod state for a single network.
type NetworkHandler interface {
	GetNetworkName() string
	SyncPods(pods []*corev1.Pod) error
	GetPodState(pod *corev1.Pod) interface{}
	PodExpectedOnNetwork(pod *corev1.Pod) (bool, error)
	ReconcilePod(oldPod, newPod *corev1.Pod, cachedState interface{}) error
}

// ErrorRecorder records reconcile failures for handlers that expose
// user-visible Kubernetes events.
type ErrorRecorder interface {
	RecordPodError(pod *corev1.Pod, reason string, err error)
}

// RelatedPodHandler requeues pods that share external state with the current pod.
type RelatedPodHandler interface {
	RelatedPodKeys(pod *corev1.Pod) ([]string, error)
}

type appliedPodState struct {
	pod       *corev1.Pod
	latestPod *corev1.Pod
	state     interface{}
}

// Controller reconciles pod state for all registered networks.
type Controller struct {
	name string

	podController controller.Controller
	podLister     corev1listers.PodLister

	handlers *syncmap.SyncMap[NetworkHandler]
	podLocks *syncmap.SyncMap[struct{}]

	stateMu     sync.RWMutex
	appliedPods map[string]map[string]*appliedPodState

	startMu sync.Mutex
	started bool
}

const scopedPodQueueKeySeparator = "|"

// NewController builds a shared pod controller.
func NewController(wf *factory.WatchFactory, name string) *Controller {
	podInformer := wf.PodCoreInformer()
	c := &Controller{
		name:        name,
		podLister:   podInformer.Lister(),
		handlers:    syncmap.NewSyncMap[NetworkHandler](),
		podLocks:    syncmap.NewSyncMap[struct{}](),
		appliedPods: map[string]map[string]*appliedPodState{},
	}

	podControllerConfig := &controller.ControllerConfig[corev1.Pod]{
		Informer:       podInformer.Informer(),
		Lister:         podInformer.Lister().List,
		MaxAttempts:    controller.InfiniteAttempts,
		ObjNeedsUpdate: c.podNeedsUpdate,
		Reconcile:      c.reconcilePod,
		Threadiness:    15,
	}
	c.podController = controller.NewController(c.name+"-pod", podControllerConfig)

	return c
}

// NewPodController builds a controller that handles pod events for all networks.
func NewPodController(wf *factory.WatchFactory) *Controller {
	return NewController(wf, "pod-network")
}

// Start starts the pod worker.
func (c *Controller) Start() error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.started {
		return nil
	}

	if err := controller.Start(c.podController); err != nil {
		return err
	}

	c.started = true
	return nil
}

// Stop stops the pod worker.
func (c *Controller) Stop() {
	c.startMu.Lock()
	c.started = false
	c.startMu.Unlock()
	controller.Stop(c.podController)
}

// ReconcileNetwork queues reconciliation for a single pod/network pair.
func (c *Controller) ReconcileNetwork(podKey, netName string) {
	c.podController.Reconcile(scopedPodQueueKey(podKey, netName))
}

// ReconcilePendingPods queues pending pods for a single network.
func (c *Controller) ReconcilePendingPods(netName string) error {
	pods, err := c.podLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list all pods: %w", err)
	}
	for _, pod := range pods {
		if !util.PodScheduled(pod) || pod.Status.Phase != corev1.PodPending {
			continue
		}
		key, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			return fmt.Errorf("failed to get pod key for %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		c.ReconcileNetwork(key, netName)
	}
	return nil
}

func (c *Controller) podNeedsUpdate(_, newPod *corev1.Pod) bool {
	// Keep same-UID delete teardown using the latest informer state while still
	// enqueueing every pod update.
	c.refreshAppliedPodFromInformer(newPod)
	return true
}

// RegisterNetworkController registers or replaces a per-network pod handler.
func (c *Controller) RegisterNetworkController(handler NetworkHandler) error {
	if handler == nil {
		return fmt.Errorf("%s: nil pod handler registration", c.name)
	}
	netName := handler.GetNetworkName()
	return c.handlers.DoWithLock(netName, func(key string) error {
		if existing, ok := c.handlers.Load(key); ok && existing != nil {
			panic(fmt.Sprintf("%s: duplicate pod handler registration for network %q", c.name, key))
		}
		c.handlers.Store(key, handler)
		if err := c.bootstrapNetwork(key, handler); err != nil {
			c.handlers.Delete(key)
			return fmt.Errorf("%s: failed to bootstrap network %s: %w", c.name, netName, err)
		}
		return nil
	})
}

// DeregisterNetworkController removes a per-network pod handler and clears
// associated applied-state tracking.
func (c *Controller) DeregisterNetworkController(netName string) {
	_ = c.handlers.DoWithLock(netName, func(key string) error {
		c.handlers.Delete(key)
		c.stateMu.Lock()
		delete(c.appliedPods, key)
		c.stateMu.Unlock()
		return nil
	})
}

func (c *Controller) reconcilePod(key string) error {
	podKey, netName := parseScopedPodQueueKey(key)
	if netName == "" {
		for _, networkName := range c.handlers.GetKeys() {
			c.ReconcileNetwork(podKey, networkName)
		}
		return c.reconcileRelatedPods(podKey)
	}

	return c.handlers.DoWithLock(netName, func(handlerKey string) error {
		handler, ok := c.handlers.Load(handlerKey)
		if !ok || handler == nil {
			return nil
		}
		return c.reconcilePodForNetworkWithPodLock(handler, podKey, netName)
	})
}

func (c *Controller) reconcilePodForNetworkWithPodLock(handler NetworkHandler, podKey, netName string) error {
	// A pod's network controllers all patch the same pod-networks annotation.
	// Serialize by pod key so per-network reconciles cannot clobber each other.
	c.podLocks.LockKey(podKey)
	defer c.podLocks.UnlockKey(podKey)
	return c.reconcilePodForNetwork(handler, podKey, netName)
}

func (c *Controller) reconcileRelatedPods(podKey string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(podKey)
	if err != nil {
		return fmt.Errorf("failed to split pod key %s: %w", podKey, err)
	}
	pod, err := c.podLister.Pods(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	for _, networkName := range c.handlers.GetKeys() {
		handler, ok := c.handlers.Load(networkName)
		if !ok || handler == nil {
			continue
		}
		relatedHandler, ok := handler.(RelatedPodHandler)
		if !ok {
			continue
		}
		relatedPodKeys, err := relatedHandler.RelatedPodKeys(pod)
		if err != nil {
			return err
		}
		for _, relatedPodKey := range relatedPodKeys {
			if relatedPodKey == "" || relatedPodKey == podKey {
				continue
			}
			c.ReconcileNetwork(relatedPodKey, networkName)
		}
	}
	return nil
}

func (c *Controller) reconcilePodForNetwork(handler NetworkHandler, podKey, netName string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(podKey)
	if err != nil {
		return fmt.Errorf("failed to split pod key %s: %w", podKey, err)
	}

	pod, err := c.podLister.Pods(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	podFound := err == nil

	applied := c.getAppliedPod(netName, podKey)
	if !podFound {
		if applied == nil {
			return nil
		}
		return c.reconcileDelete(handler, podKey, netName, applied)
	}

	expected, err := handler.PodExpectedOnNetwork(pod)
	if err != nil {
		c.recordPodError(handler, pod, "ErrorUpdatingResource", err)
		return err
	}

	if applied != nil && applied.pod.UID != pod.UID {
		if err := c.reconcileDelete(handler, podKey, netName, applied); err != nil {
			return err
		}
		applied = nil
	}

	if !expected {
		if applied == nil {
			if util.PodCompleted(pod) {
				return c.reconcileAbsent(handler, podKey, netName, nil, pod)
			}
			return nil
		}
		return c.reconcileAbsent(handler, podKey, netName, applied, nil)
	}

	var oldPod *corev1.Pod
	if applied != nil {
		oldPod = applied.pod
	}
	if err := handler.ReconcilePod(oldPod, pod, nil); err != nil {
		reason := "ErrorAddingResource"
		if oldPod != nil {
			reason = "ErrorUpdatingResource"
		}
		c.recordPodError(handler, pod, reason, err)
		return err
	}
	c.setAppliedPod(netName, podKey, pod, handler.GetPodState(pod))
	return nil
}

func (c *Controller) reconcileDelete(handler NetworkHandler, podKey, netName string, applied *appliedPodState) error {
	return c.reconcileAbsent(handler, podKey, netName, applied, nil)
}

func (c *Controller) reconcileAbsent(handler NetworkHandler, podKey, netName string, applied *appliedPodState, fallbackPod *corev1.Pod) error {
	var state interface{}
	deletePod := fallbackPod
	if applied == nil || applied.pod == nil {
		if deletePod != nil {
			if err := handler.ReconcilePod(deletePod, nil, nil); err != nil {
				c.recordPodError(handler, deletePod, "ErrorDeletingResource", err)
				return err
			}
		}
		c.deleteAppliedPod(netName, podKey)
		return nil
	}
	deletePod = applied.pod
	state = applied.state
	if applied.latestPod != nil && applied.latestPod.UID == applied.pod.UID {
		deletePod = applied.latestPod
	}
	if err := handler.ReconcilePod(deletePod, nil, state); err != nil {
		c.recordPodError(handler, deletePod, "ErrorDeletingResource", err)
		return err
	}
	c.deleteAppliedPod(netName, podKey)
	return nil
}

func (c *Controller) recordPodError(handler NetworkHandler, pod *corev1.Pod, reason string, err error) {
	if pod == nil || err == nil {
		return
	}
	recorder, ok := handler.(ErrorRecorder)
	if !ok {
		return
	}
	recorder.RecordPodError(pod, reason, err)
}

func (c *Controller) bootstrapNetwork(netName string, handler NetworkHandler) error {
	pods, err := c.podLister.List(labels.Everything())
	if err != nil {
		return err
	}
	if err := handler.SyncPods(pods); err != nil {
		return err
	}

	for _, pod := range pods {
		expected, err := handler.PodExpectedOnNetwork(pod)
		if err != nil {
			return err
		}
		if !expected {
			continue
		}
		key, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			return fmt.Errorf("failed to get pod key for %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		if state := handler.GetPodState(pod); state != nil {
			c.setAppliedPod(netName, key, pod, state)
		}
		if err := c.reconcilePodForNetworkWithPodLock(handler, key, netName); err != nil {
			// Preserve the normal retry contract for pods that cannot be
			// reconciled during startup while still applying successful pods
			// synchronously for dependent controller bootstrap.
			c.ReconcileNetwork(key, netName)
		}
	}
	return nil
}

func (c *Controller) getAppliedPod(netName, podKey string) *appliedPodState {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	pods := c.appliedPods[netName]
	if pods == nil {
		return nil
	}
	applied := pods[podKey]
	if applied == nil {
		return nil
	}
	var latestPod *corev1.Pod
	if applied.latestPod != nil {
		latestPod = applied.latestPod.DeepCopy()
	}
	return &appliedPodState{
		pod:       applied.pod.DeepCopy(),
		latestPod: latestPod,
		state:     applied.state,
	}
}

func (c *Controller) setAppliedPod(netName, podKey string, pod *corev1.Pod, state interface{}) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	pods := c.appliedPods[netName]
	if pods == nil {
		pods = map[string]*appliedPodState{}
		c.appliedPods[netName] = pods
	}
	pods[podKey] = &appliedPodState{
		pod:       pod.DeepCopy(),
		latestPod: pod.DeepCopy(),
		state:     state,
	}
}

func (c *Controller) refreshAppliedPodFromInformer(pod *corev1.Pod) {
	if pod == nil {
		return
	}
	podKey, err := cache.MetaNamespaceKeyFunc(pod)
	if err != nil {
		return
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	for _, pods := range c.appliedPods {
		applied := pods[podKey]
		if applied == nil || applied.pod == nil || applied.pod.UID != pod.UID {
			continue
		}
		applied.latestPod = pod.DeepCopy()
	}
}

func (c *Controller) deleteAppliedPod(netName, podKey string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	pods := c.appliedPods[netName]
	if pods == nil {
		return
	}
	delete(pods, podKey)
	if len(pods) == 0 {
		delete(c.appliedPods, netName)
	}
}

func scopedPodQueueKey(podKey, netName string) string {
	if netName == "" {
		return podKey
	}
	return podKey + scopedPodQueueKeySeparator + netName
}

func parseScopedPodQueueKey(key string) (podKey, netName string) {
	parts := strings.SplitN(key, scopedPodQueueKeySeparator, 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return key, ""
	}
	return parts[0], parts[1]
}
