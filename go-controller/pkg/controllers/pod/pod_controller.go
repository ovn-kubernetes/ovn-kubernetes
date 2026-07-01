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
	apitypes "k8s.io/apimachinery/pkg/types"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// NetworkHandler reconciles pod state for a single network.
type NetworkHandler interface {
	GetNetworkName() string
	SyncPods(pods []*corev1.Pod) error
	// GetPodState returns the handler's cached state for the pod. It must
	// return an untyped nil when it has no state for the pod, so callers can
	// compare the result against nil without knowing the concrete type.
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
	// pod is the last successfully applied pod, nil when every reconcile
	// attempt for the pod has failed so far.
	pod *corev1.Pod
	// latestPod is the latest informer object seen for the pod. It is kept
	// even when reconciliation fails so delete teardown always has a pod
	// object to clean up partially applied state with.
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

	// netLocksMu guards netLocks. Each network's RWMutex serializes handler
	// registration/deregistration (write lock) against in-flight reconciles
	// (read lock) while still allowing reconciles of the same network to run
	// concurrently.
	netLocksMu sync.Mutex
	netLocks   map[string]*sync.RWMutex

	stateMu     sync.RWMutex
	appliedPods map[string]map[string]*appliedPodState
	// recordedPodErrors tracks the last recorded reconcile error per network
	// and pod. Reconciles retry indefinitely, so events are recorded only
	// when a pod's failure streak starts or its error changes; recorders may
	// block or flood otherwise. Guarded by stateMu.
	recordedPodErrors map[string]map[string]string

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
		netLocks:    map[string]*sync.RWMutex{},
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

// netLock returns the RWMutex for a network, creating it when needed.
// Entries are never removed: deleting one could let a reconcile holding the
// old lock run concurrently with a re-registration under a fresh lock. The
// map is bounded by the set of network names ever registered.
func (c *Controller) netLock(netName string) *sync.RWMutex {
	c.netLocksMu.Lock()
	defer c.netLocksMu.Unlock()
	if c.netLocks == nil {
		c.netLocks = map[string]*sync.RWMutex{}
	}
	lock := c.netLocks[netName]
	if lock == nil {
		lock = &sync.RWMutex{}
		c.netLocks[netName] = lock
	}
	return lock
}

// RegisterNetworkController registers or replaces a per-network pod handler.
func (c *Controller) RegisterNetworkController(handler NetworkHandler) error {
	if handler == nil {
		return fmt.Errorf("%s: nil pod handler registration", c.name)
	}
	netName := handler.GetNetworkName()
	lock := c.netLock(netName)
	lock.Lock()
	defer lock.Unlock()
	if existing, ok := c.handlers.Load(netName); ok && existing != nil {
		panic(fmt.Sprintf("%s: duplicate pod handler registration for network %q", c.name, netName))
	}
	c.handlers.Store(netName, handler)
	if err := c.bootstrapNetwork(netName, handler); err != nil {
		c.handlers.Delete(netName)
		return fmt.Errorf("%s: failed to bootstrap network %s: %w", c.name, netName, err)
	}
	return nil
}

// DeregisterNetworkController removes a per-network pod handler and clears
// associated applied-state tracking.
func (c *Controller) DeregisterNetworkController(netName string) {
	lock := c.netLock(netName)
	lock.Lock()
	defer lock.Unlock()
	c.handlers.Delete(netName)
	c.stateMu.Lock()
	delete(c.appliedPods, netName)
	delete(c.recordedPodErrors, netName)
	c.stateMu.Unlock()
}

func (c *Controller) reconcilePod(key string) error {
	podKey, netName := parseScopedPodQueueKey(key)
	if netName == "" {
		for _, networkName := range c.handlers.GetKeys() {
			c.ReconcileNetwork(podKey, networkName)
		}
		return c.reconcileRelatedPods(podKey)
	}

	// Take a read lock so reconciles of the same network run concurrently
	// while handler (de)registration is excluded. Try-lock so queue workers
	// requeue instead of blocking behind a long synchronous bootstrap.
	lock := c.netLock(netName)
	if !lock.TryRLock() {
		return fmt.Errorf("%s: network %s is busy registering, requeueing pod %s", c.name, netName, podKey)
	}
	defer lock.RUnlock()
	handler, ok := c.handlers.Load(netName)
	if !ok || handler == nil {
		return nil
	}
	return c.reconcilePodForNetworkWithPodLock(handler, podKey, netName)
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
		c.recordPodError(handler, netName, podKey, pod, "ErrorUpdatingResource", err)
		return err
	}

	if applied != nil && appliedPodUID(applied) != pod.UID {
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
		c.recordPodError(handler, netName, podKey, pod, reason, err)
		// Remember the attempted pod so a delete arriving before a successful
		// retry still tears down partially applied state.
		c.setAttemptedPod(netName, podKey, pod)
		return err
	}
	c.setAppliedPod(netName, podKey, pod, handler.GetPodState(pod))
	return nil
}

// appliedPodUID returns the UID the applied-state entry tracks, preferring the
// last successfully applied pod and falling back to the last attempted one.
func appliedPodUID(applied *appliedPodState) apitypes.UID {
	if applied.pod != nil {
		return applied.pod.UID
	}
	if applied.latestPod != nil {
		return applied.latestPod.UID
	}
	return ""
}

func (c *Controller) reconcileDelete(handler NetworkHandler, podKey, netName string, applied *appliedPodState) error {
	return c.reconcileAbsent(handler, podKey, netName, applied, nil)
}

func (c *Controller) reconcileAbsent(handler NetworkHandler, podKey, netName string, applied *appliedPodState, fallbackPod *corev1.Pod) error {
	var state interface{}
	deletePod := fallbackPod
	if applied != nil {
		switch {
		case applied.pod != nil:
			deletePod = applied.pod
			state = applied.state
			if applied.latestPod != nil && applied.latestPod.UID == applied.pod.UID {
				deletePod = applied.latestPod
			}
		case applied.latestPod != nil:
			// The pod was attempted but never successfully applied. Tear down
			// best-effort so partially applied state does not leak.
			deletePod = applied.latestPod
		}
	}
	if deletePod != nil {
		if err := handler.ReconcilePod(deletePod, nil, state); err != nil {
			c.recordPodError(handler, netName, podKey, deletePod, "ErrorDeletingResource", err)
			return err
		}
		// Requeue pods sharing external state with the deleted pod so state
		// that referenced it (e.g. VM routes flipped during live migration)
		// is restored promptly.
		c.requeueRelatedPods(handler, netName, podKey, deletePod)
	}
	c.deleteAppliedPod(netName, podKey)
	return nil
}

// requeueRelatedPods requeues pods that share external state with a deleted
// pod on the handler's network.
func (c *Controller) requeueRelatedPods(handler NetworkHandler, netName, podKey string, pod *corev1.Pod) {
	relatedHandler, ok := handler.(RelatedPodHandler)
	if !ok {
		return
	}
	relatedPodKeys, err := relatedHandler.RelatedPodKeys(pod)
	if err != nil {
		klog.Errorf("%s: failed to get related pod keys for deleted pod %s on network %s: %v", c.name, podKey, netName, err)
		return
	}
	for _, relatedPodKey := range relatedPodKeys {
		if relatedPodKey == "" || relatedPodKey == podKey {
			continue
		}
		c.ReconcileNetwork(relatedPodKey, netName)
	}
}

func (c *Controller) recordPodError(handler NetworkHandler, netName, podKey string, pod *corev1.Pod, reason string, err error) {
	if pod == nil || err == nil {
		return
	}
	recorder, ok := handler.(ErrorRecorder)
	if !ok {
		return
	}
	if !c.markRecordedPodError(netName, podKey, reason+": "+err.Error()) {
		return
	}
	recorder.RecordPodError(pod, reason, err)
}

// markRecordedPodError returns true when the error starts a failure streak or
// differs from the last recorded error for the pod on the network.
func (c *Controller) markRecordedPodError(netName, podKey, errMsg string) bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.recordedPodErrors == nil {
		c.recordedPodErrors = map[string]map[string]string{}
	}
	pods := c.recordedPodErrors[netName]
	if pods == nil {
		pods = map[string]string{}
		c.recordedPodErrors[netName] = pods
	}
	if pods[podKey] == errMsg {
		return false
	}
	pods[podKey] = errMsg
	return true
}

// clearRecordedPodError resets error-event dedup after a successful reconcile.
// Callers must hold stateMu.
func (c *Controller) clearRecordedPodError(netName, podKey string) {
	pods := c.recordedPodErrors[netName]
	if pods == nil {
		return
	}
	delete(pods, podKey)
	if len(pods) == 0 {
		delete(c.recordedPodErrors, netName)
	}
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
	c.clearRecordedPodError(netName, podKey)
}

// setAttemptedPod records the pod for delete teardown when a reconcile attempt
// fails before any state was successfully applied.
func (c *Controller) setAttemptedPod(netName, podKey string, pod *corev1.Pod) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	pods := c.appliedPods[netName]
	if pods == nil {
		pods = map[string]*appliedPodState{}
		c.appliedPods[netName] = pods
	}
	if applied := pods[podKey]; applied != nil {
		applied.latestPod = pod.DeepCopy()
		return
	}
	pods[podKey] = &appliedPodState{latestPod: pod.DeepCopy()}
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
		if applied == nil || appliedPodUID(applied) != pod.UID {
			continue
		}
		applied.latestPod = pod.DeepCopy()
	}
}

func (c *Controller) deleteAppliedPod(netName, podKey string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.clearRecordedPodError(netName, podKey)
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
