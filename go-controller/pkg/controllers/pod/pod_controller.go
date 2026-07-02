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
	// SyncPods performs the initial full-network sync before per-pod
	// reconciliation is queued for the handler.
	SyncPods(pods []*corev1.Pod) error
	// PodExpectedOnNetwork returns whether the pod should exist on the network.
	PodExpectedOnNetwork(pod *corev1.Pod) (bool, error)
	// ReconcilePod is the single level-driven entry point. newPod == nil means
	// teardown, with lastState carrying the state returned by the last
	// successful reconcile of the pod, if any. On success it returns the state
	// to cache for the eventual teardown.
	ReconcilePod(oldPod, newPod *corev1.Pod, lastState interface{}) (interface{}, error)
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

// podEntry is the level-driven record for a pod on a network. All fields are
// guarded by Controller.stateMu. Pod objects are shared informer references
// and MUST NOT be modified.
type podEntry struct {
	// applied is the last successfully reconciled pod, nil while no reconcile
	// has succeeded yet.
	applied *corev1.Pod
	// lastSeen is the newest informer object observed for the pod, recorded on
	// every reconcile attempt and informer update so teardown always has a pod
	// object even when every reconcile failed.
	lastSeen *corev1.Pod
	// state is the handler state returned by the last successful reconcile.
	state interface{}
	// lastErr dedups error events to one per failure streak.
	lastErr string
}

// podEntryUID returns the UID the entry tracks, preferring the last
// successfully applied pod and falling back to the last seen one.
func podEntryUID(entry *podEntry) apitypes.UID {
	if entry.applied != nil {
		return entry.applied.UID
	}
	if entry.lastSeen != nil {
		return entry.lastSeen.UID
	}
	return ""
}

// activeNetwork tracks a registered handler and its lifecycle: in-flight
// reconciles for deregistration draining, and the initial-listing barrier.
type activeNetwork struct {
	handler NetworkHandler

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
	// pending tracks initial-listing pods not yet attempted; done is closed
	// once every one of them has been reconciled at least once, successfully
	// or not. Failed pods stay on the retry queue like any other.
	pending map[string]struct{}
	done    chan struct{}
}

// enter reserves an in-flight reconcile slot; it fails once the network is
// being deregistered.
func (n *activeNetwork) enter() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return false
	}
	n.wg.Add(1)
	return true
}

func (n *activeNetwork) leave() {
	n.wg.Done()
}

// markAttempted records that a pod from the initial listing has been
// reconciled at least once and closes the barrier when the listing is done.
func (n *activeNetwork) markAttempted(podKey string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pending == nil {
		return
	}
	delete(n.pending, podKey)
	if len(n.pending) == 0 {
		n.pending = nil
		close(n.done)
	}
}

// close marks the network deregistered and releases any barrier waiters.
func (n *activeNetwork) close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closed = true
	if n.pending != nil {
		n.pending = nil
		close(n.done)
	}
}

// Controller reconciles pod state for all registered networks.
type Controller struct {
	name string

	podController controller.Controller
	podLister     corev1listers.PodLister

	// podLocks serializes reconciles per pod across networks: a pod's network
	// controllers all patch the same pod-networks annotation.
	podLocks *syncmap.SyncMap[struct{}]

	// networksMu guards networks.
	networksMu sync.RWMutex
	networks   map[string]*activeNetwork

	// stateMu guards entries and every podEntry.
	stateMu sync.RWMutex
	// entries is keyed by network name, then pod key.
	entries map[string]map[string]*podEntry

	startMu sync.Mutex
	started bool
}

const scopedPodQueueKeySeparator = "|"

// NewController builds a shared pod controller.
func NewController(wf *factory.WatchFactory, name string) *Controller {
	podInformer := wf.PodCoreInformer()
	c := &Controller{
		name:      name,
		podLister: podInformer.Lister(),
		podLocks:  syncmap.NewSyncMap[struct{}](),
		networks:  map[string]*activeNetwork{},
		entries:   map[string]map[string]*podEntry{},
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

// Start starts the pod workers.
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

// Stop stops the pod workers.
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
	// Keep same-UID teardown using the latest informer state while still
	// enqueueing every pod update.
	c.refreshLastSeenFromInformer(newPod)
	return true
}

// RegisterNetworkController registers a per-network pod handler. It syncs the
// handler against a full pod listing, publishes it and queues every listed pod
// for reconciliation. The returned channel is closed once every listed pod has
// been reconciled at least once, successfully or not; callers that depend on
// pods being applied before their own bootstrap may wait on it, provided the
// controller has been started.
func (c *Controller) RegisterNetworkController(handler NetworkHandler) (<-chan struct{}, error) {
	if handler == nil {
		return nil, fmt.Errorf("%s: nil pod handler registration", c.name)
	}
	netName := handler.GetNetworkName()

	c.networksMu.Lock()
	if _, ok := c.networks[netName]; ok {
		c.networksMu.Unlock()
		panic(fmt.Sprintf("%s: duplicate pod handler registration for network %q", c.name, netName))
	}
	c.networksMu.Unlock()

	pods, err := c.podLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("%s: failed to list pods to bootstrap network %s: %w", c.name, netName, err)
	}
	if err := handler.SyncPods(pods); err != nil {
		return nil, fmt.Errorf("%s: failed to bootstrap network %s: %w", c.name, netName, err)
	}

	net := &activeNetwork{
		handler: handler,
		pending: map[string]struct{}{},
		done:    make(chan struct{}),
	}
	podKeys := make([]string, 0, len(pods))
	for _, pod := range pods {
		key, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			return nil, fmt.Errorf("%s: failed to get pod key for %s/%s: %w", c.name, pod.Namespace, pod.Name, err)
		}
		podKeys = append(podKeys, key)
		net.pending[key] = struct{}{}
	}
	if len(net.pending) == 0 {
		net.pending = nil
		close(net.done)
	}

	c.networksMu.Lock()
	if _, ok := c.networks[netName]; ok {
		c.networksMu.Unlock()
		panic(fmt.Sprintf("%s: duplicate pod handler registration for network %q", c.name, netName))
	}
	c.networks[netName] = net
	c.networksMu.Unlock()

	for _, key := range podKeys {
		c.ReconcileNetwork(key, netName)
	}
	return net.done, nil
}

// DeregisterNetworkController removes a per-network pod handler, waits for its
// in-flight reconciles to drain and clears associated pod state tracking.
func (c *Controller) DeregisterNetworkController(netName string) {
	c.networksMu.Lock()
	net := c.networks[netName]
	delete(c.networks, netName)
	c.networksMu.Unlock()
	if net == nil {
		return
	}
	net.close()
	net.wg.Wait()

	c.stateMu.Lock()
	delete(c.entries, netName)
	c.stateMu.Unlock()
}

func (c *Controller) getNetwork(netName string) *activeNetwork {
	c.networksMu.RLock()
	defer c.networksMu.RUnlock()
	return c.networks[netName]
}

func (c *Controller) networkNames() []string {
	c.networksMu.RLock()
	defer c.networksMu.RUnlock()
	names := make([]string, 0, len(c.networks))
	for name := range c.networks {
		names = append(names, name)
	}
	return names
}

func (c *Controller) reconcilePod(key string) error {
	podKey, netName := parseScopedPodQueueKey(key)
	if netName == "" {
		for _, networkName := range c.networkNames() {
			c.ReconcileNetwork(podKey, networkName)
		}
		return c.reconcileRelatedPods(podKey)
	}

	net := c.getNetwork(netName)
	if net == nil || !net.enter() {
		return nil
	}
	defer net.leave()
	defer net.markAttempted(podKey)

	// A pod's network controllers all patch the same pod-networks annotation.
	// Serialize by pod key so per-network reconciles cannot clobber each other.
	c.podLocks.LockKey(podKey)
	defer c.podLocks.UnlockKey(podKey)
	return c.reconcilePodForNetwork(net.handler, podKey, netName)
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

	for _, networkName := range c.networkNames() {
		net := c.getNetwork(networkName)
		if net == nil {
			continue
		}
		relatedHandler, ok := net.handler.(RelatedPodHandler)
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

	if !podFound {
		return c.teardown(handler, podKey, netName, nil)
	}

	expected, err := handler.PodExpectedOnNetwork(pod)
	if err != nil {
		c.markSeen(netName, podKey, pod)
		c.recordPodError(handler, netName, podKey, pod, "ErrorUpdatingResource", err)
		return err
	}

	if uid := c.entryUID(netName, podKey); uid != "" && uid != pod.UID {
		// The pod was recreated under the same name; tear the old instance
		// down before reconciling the new one.
		if err := c.teardown(handler, podKey, netName, nil); err != nil {
			return err
		}
	}

	if !expected {
		// Tear down whatever the entry knows about; completed pods without an
		// entry still get a best-effort teardown with the informer object.
		var fallbackPod *corev1.Pod
		if util.PodCompleted(pod) {
			fallbackPod = pod
		}
		return c.teardown(handler, podKey, netName, fallbackPod)
	}

	applied, state := c.markSeen(netName, podKey, pod)
	newState, err := handler.ReconcilePod(applied, pod, state)
	if err != nil {
		reason := "ErrorAddingResource"
		if applied != nil {
			reason = "ErrorUpdatingResource"
		}
		c.recordPodError(handler, netName, podKey, pod, reason, err)
		return err
	}
	c.markApplied(netName, podKey, pod, newState)
	return nil
}

// teardown reconciles the pod as absent using the entry's best available pod
// object, or fallbackPod when no entry exists. On success it requeues related
// pods and clears the entry.
func (c *Controller) teardown(handler NetworkHandler, podKey, netName string, fallbackPod *corev1.Pod) error {
	deletePod, state := c.teardownPod(netName, podKey)
	if deletePod == nil {
		deletePod = fallbackPod
	}
	if deletePod != nil {
		if _, err := handler.ReconcilePod(deletePod, nil, state); err != nil {
			c.recordPodError(handler, netName, podKey, deletePod, "ErrorDeletingResource", err)
			return err
		}
		// Requeue pods sharing external state with the deleted pod so state
		// that referenced it (e.g. VM routes flipped during live migration)
		// is restored promptly.
		c.requeueRelatedPods(handler, netName, podKey, deletePod)
	}
	c.deleteEntry(netName, podKey)
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

// recordPodError records a reconcile failure once per failure streak:
// reconciles retry indefinitely and must not flood the recorder with
// duplicate events.
func (c *Controller) recordPodError(handler NetworkHandler, netName, podKey string, pod *corev1.Pod, reason string, err error) {
	if pod == nil || err == nil {
		return
	}
	recorder, ok := handler.(ErrorRecorder)
	if !ok {
		return
	}
	errMsg := reason + ": " + err.Error()
	c.stateMu.Lock()
	entry := c.ensureEntryLocked(netName, podKey)
	if entry.lastErr == errMsg {
		c.stateMu.Unlock()
		return
	}
	entry.lastErr = errMsg
	c.stateMu.Unlock()
	recorder.RecordPodError(pod, reason, err)
}

// ensureEntryLocked returns the entry for the pod on the network, creating it
// when needed. Callers must hold stateMu.
func (c *Controller) ensureEntryLocked(netName, podKey string) *podEntry {
	pods := c.entries[netName]
	if pods == nil {
		pods = map[string]*podEntry{}
		c.entries[netName] = pods
	}
	entry := pods[podKey]
	if entry == nil {
		entry = &podEntry{}
		pods[podKey] = entry
	}
	return entry
}

// entryUID returns the UID tracked for the pod on the network, empty when
// nothing is tracked.
func (c *Controller) entryUID(netName, podKey string) apitypes.UID {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	entry := c.entries[netName][podKey]
	if entry == nil {
		return ""
	}
	return podEntryUID(entry)
}

// markSeen records the pod as the latest observed object before a reconcile
// attempt, so a later teardown always has a pod object even when every
// reconcile failed. It returns the last applied pod and state for the attempt.
func (c *Controller) markSeen(netName, podKey string, pod *corev1.Pod) (*corev1.Pod, interface{}) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	entry := c.ensureEntryLocked(netName, podKey)
	entry.lastSeen = pod
	return entry.applied, entry.state
}

// markApplied records a successful reconcile and resets error-event dedup.
func (c *Controller) markApplied(netName, podKey string, pod *corev1.Pod, state interface{}) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	entry := c.ensureEntryLocked(netName, podKey)
	entry.applied = pod
	entry.lastSeen = pod
	entry.state = state
	entry.lastErr = ""
}

// teardownPod returns the best pod object and state to tear down with: the
// latest observed object when it matches the applied pod's UID, otherwise the
// applied pod itself, otherwise the last attempted object with no state.
func (c *Controller) teardownPod(netName, podKey string) (*corev1.Pod, interface{}) {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	entry := c.entries[netName][podKey]
	if entry == nil {
		return nil, nil
	}
	if entry.applied == nil {
		return entry.lastSeen, nil
	}
	if entry.lastSeen != nil && entry.lastSeen.UID == entry.applied.UID {
		return entry.lastSeen, entry.state
	}
	return entry.applied, entry.state
}

func (c *Controller) deleteEntry(netName, podKey string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	pods := c.entries[netName]
	if pods == nil {
		return
	}
	delete(pods, podKey)
	if len(pods) == 0 {
		delete(c.entries, netName)
	}
}

// refreshLastSeenFromInformer keeps entries tracking the pod's current UID
// pointed at the newest informer object.
func (c *Controller) refreshLastSeenFromInformer(pod *corev1.Pod) {
	if pod == nil {
		return
	}
	podKey, err := cache.MetaNamespaceKeyFunc(pod)
	if err != nil {
		return
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	for _, pods := range c.entries {
		entry := pods[podKey]
		if entry == nil || podEntryUID(entry) != pod.UID {
			continue
		}
		entry.lastSeen = pod
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
