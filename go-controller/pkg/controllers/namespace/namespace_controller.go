// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// Package namespace provides a level-driven, shared Namespace controller
// mirroring pkg/controllers/node/NodeController. Per-network handlers register
// and the controller dispatches reconcile keys of shape "<ns>|<net>".
package namespace

import (
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	v1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
)

// NamespaceHandler handles namespace reconciliation for a single network.
type NamespaceHandler interface {
	// GetNetworkName returns the network this handler reconciles.
	GetNetworkName() string
	// ClaimsNamespace reports whether this handler would program any OVN state
	// for nsName, letting the controller detect membership transitions and pick
	// the reconcile leg. A non-nil error means "unknown" and the controller
	// requeues without mutating state. Must be cheap and side-effect-free.
	ClaimsNamespace(nsName string) (bool, error)
	// ReconcileNamespace reconciles the network-specific state for a namespace.
	// oldNS and oldState may be nil on first sight or reactivation. For delete,
	// oldNS is the best available prior object (cached namespace, else a
	// name-only stub) and oldState may be nil. newNS and newState are nil when
	// the namespace's state should be deleted.
	ReconcileNamespace(oldNS, newNS *corev1.Namespace, oldState, newState *NamespaceAnnotationState) error
	// SyncNamespaces performs the initial full-network sync before per-namespace
	// reconciliation is queued. A handler keeping an applied-state snapshot for
	// restart safety must seed it here.
	SyncNamespaces(namespaces []*corev1.Namespace) error
}

// NamespaceController reconciles namespace state for all registered
// networks. Mirrors NodeController in shape.
type NamespaceController struct {
	name string

	nsController   controller.Controller
	networkManager networkmanager.Interface
	nsLister       v1listers.NamespaceLister

	// handlers maps network name to namespace handler.
	handlers *syncmap.SyncMap[NamespaceHandler]

	// stateMu protects nsReconciliation, bootstrapPending, nsActive,
	// nsNetworks, nsCache, and latestInformerNsCache.
	stateMu sync.RWMutex
	// nsReconciliation marks namespaces to treat as a fresh add on their next
	// reconcile (the gate nulls oldNS). Populated by bootstrapNetwork, cleared
	// on success. No delete-pending dimension (unlike NodeController): delete
	// clears active state only after the handler delete succeeds, so the cached
	// "had" state survives a failed delete and the (had && !has) branch retries.
	// keyed by network -> namespaces
	nsReconciliation map[string]map[string]struct{}
	// bootstrapPending tracks namespaces enqueued by bootstrapNetwork that
	// haven't had their first reconcile attempt complete (any outcome).
	// WaitForBootstrap watches this set, not nsReconciliation, so a
	// persistently-failing namespace stays in retry without blocking startup.
	// keyed by network -> namespaces
	bootstrapPending map[string]map[string]struct{}
	// nsActive tracks whether a namespace/network is active (presence = active).
	// keyed by network -> namespaces
	nsActive map[string]map[string]struct{}
	// nsNetworks is a reverse index of active networks per namespace.
	// keyed by namespace -> networks
	nsNetworks map[string]map[string]struct{}
	// nsCache contains the last-applied namespace state per network.
	// keyed by network -> nsName
	nsCache map[string]map[string]*corev1.Namespace
	// latestInformerNsCache holds the latest informer object seen even if
	// reconciliation failed. Delete fallback when nsCache has no state.
	// keyed by network -> nsName
	latestInformerNsCache map[string]map[string]*corev1.Namespace
	// annotationCache stores parsed annotation values keyed by namespace.
	annotationCache *NamespaceAnnotationCache

	startMu sync.Mutex
	started bool
}

const scopedNamespaceQueueKeySeparator = "|"

// NewController builds a shared namespace controller.
func NewController(wf *factory.WatchFactory, name string, networkManager networkmanager.Interface) *NamespaceController {
	if networkManager == nil {
		panic("namespace controller network manager must not be nil")
	}
	nsInformer := wf.NamespaceCoreInformer()
	c := &NamespaceController{
		name:                  name,
		networkManager:        networkManager,
		nsLister:              nsInformer.Lister(),
		handlers:              syncmap.NewSyncMap[NamespaceHandler](),
		nsReconciliation:      map[string]map[string]struct{}{},
		bootstrapPending:      map[string]map[string]struct{}{},
		nsActive:              map[string]map[string]struct{}{},
		nsNetworks:            map[string]map[string]struct{}{},
		nsCache:               map[string]map[string]*corev1.Namespace{},
		latestInformerNsCache: map[string]map[string]*corev1.Namespace{},
		annotationCache:       NewNamespaceAnnotationCache(),
	}

	cfg := &controller.ControllerConfig[corev1.Namespace]{
		RateLimiter:    workqueue.NewTypedItemFastSlowRateLimiter[string](time.Second, 5*time.Second, 5),
		Informer:       nsInformer.Informer(),
		Lister:         nsInformer.Lister().List,
		MaxAttempts:    controller.InfiniteAttempts,
		ObjNeedsUpdate: func(_, _ *corev1.Namespace) bool { return true },
		Reconcile:      c.reconcileNamespace,
		Threadiness:    15,
	}
	c.nsController = controller.NewController(c.name+"-namespace", cfg)

	return c
}

// NewNamespaceController builds a controller that handles namespace events
// for all UDNs.
func NewNamespaceController(wf *factory.WatchFactory, networkManager networkmanager.Interface) *NamespaceController {
	return NewController(wf, "namespace-topology", networkManager)
}

// Start starts the namespace worker.
func (c *NamespaceController) Start() error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.started {
		return nil
	}
	if err := controller.Start(c.nsController); err != nil {
		return err
	}
	c.started = true
	return nil
}

// Stop stops the namespace worker.
func (c *NamespaceController) Stop() {
	c.startMu.Lock()
	c.started = false
	c.startMu.Unlock()
	controller.Stop(c.nsController)
}

// ReconcileNetwork queues reconciliation for a single namespace/network
// pair. Mirrors NodeController.ReconcileNetwork.
func (c *NamespaceController) ReconcileNetwork(nsName, netName string) {
	c.nsController.Reconcile(scopedNamespaceQueueKey(nsName, netName))
}

// WaitForBootstrap blocks until every namespace enqueued by
// bootstrapNetwork(netName) has had its first reconcile attempt complete, or
// the deadline elapses. Semantics are "attempted once," not "applied": a
// failing namespace stays in the retry path. Callers use it so dependent
// watchers (e.g. NetworkPolicy) don't race a namespace's add.
func (c *NamespaceController) WaitForBootstrap(netName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		c.stateMu.RLock()
		pendingCount := len(c.bootstrapPending[netName])
		var sample []string
		if pendingCount > 0 {
			// sample up to 5 pending namespaces for the error message
			for ns := range c.bootstrapPending[netName] {
				sample = append(sample, ns)
				if len(sample) == 5 {
					break
				}
			}
		}
		c.stateMu.RUnlock()
		if pendingCount == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: bootstrap drain for network %q exceeded %s (%d namespaces pending; sample: %v)",
				c.name, netName, timeout, pendingCount, sample)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// RegisterNetworkController registers a per-network namespace handler and runs
// a bootstrap pass: every known namespace is sync'd via SyncNamespaces and then
// queued for an individual reconcile.
func (c *NamespaceController) RegisterNetworkController(handler NamespaceHandler) error {
	if handler == nil {
		return fmt.Errorf("%s: nil namespace handler registration", c.name)
	}
	netName := handler.GetNetworkName()
	return c.handlers.DoWithLock(netName, func(key string) error {
		if existing, ok := c.handlers.Load(key); ok && existing != nil {
			panic(fmt.Sprintf("%s: duplicate namespace handler registration for network %q", c.name, key))
		}
		c.handlers.Store(key, handler)
		if err := c.bootstrapNetwork(key, handler); err != nil {
			c.handlers.Delete(key)
			return fmt.Errorf("%s: failed to bootstrap network %s: %w", c.name, netName, err)
		}
		return nil
	})
}

// DeregisterNetworkController removes a per-network namespace handler and
// clears associated network state. OVN cleanup is the handler's responsibility.
func (c *NamespaceController) DeregisterNetworkController(netName string) {
	_ = c.handlers.DoWithLock(netName, func(key string) error {
		c.handlers.Delete(key)
		c.stateMu.Lock()
		delete(c.nsReconciliation, key)
		delete(c.bootstrapPending, key)
		if nss, ok := c.nsActive[key]; ok {
			for nsName := range nss {
				if networks, ok := c.nsNetworks[nsName]; ok {
					delete(networks, key)
					if len(networks) == 0 {
						delete(c.nsNetworks, nsName)
					}
				}
			}
		}
		delete(c.nsActive, key)
		delete(c.nsCache, key)
		delete(c.latestInformerNsCache, key)
		c.stateMu.Unlock()
		return nil
	})
}

// reconcileNamespace runs the right leg by comparing cached applied state
// ("had") to the handler's current claim ("has"):
//
//   - newNS == nil (deleted): delete leg only if had; skip the predicate, its
//     data sources may be gone.
//   - ClaimsNamespace errors: requeue, mutate nothing.
//   - (had, has): !had&&has = add; had&&!has = delete (e.g. NAD moved the
//     namespace out); had&&has = update; !had&&!has = no-op.
//
// Membership is a per-handler predicate (ClaimsNamespace), not a single
// network-manager fact as in NodeController.
func (c *NamespaceController) reconcileNamespace(key string) error {
	nsName, netName := parseScopedNamespaceQueueKey(key)
	// Empty namespace names aren't valid and would loop the fan-out below
	// indefinitely ("|net" re-encodes as "|net|net", ...). Skip them.
	if nsName == "" {
		return nil
	}
	// An empty netName fans out to every registered network.
	if netName == "" {
		for _, networkName := range c.handlers.GetKeys() {
			c.ReconcileNetwork(nsName, networkName)
		}
		return nil
	}

	return c.handlers.DoWithLock(netName, func(handlerKey string) error {
		// Mark on every exit path so WaitForBootstrap unblocks even when the
		// reconcile errored. Idempotent for non-bootstrap namespaces.
		defer c.markBootstrapAttempted(netName, nsName)

		handler, ok := c.handlers.Load(handlerKey)
		if !ok || handler == nil {
			return nil
		}

		newNS, err := c.nsLister.Get(nsName)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}

		oldNS := c.getCachedNamespace(netName, nsName)
		had := c.namespaceHasNetwork(netName, nsName)

		// Deletion special-case. Skip the predicate — namespace is gone.
		if newNS == nil {
			var oldState *NamespaceAnnotationState
			if oldNS != nil {
				oldState = c.annotationCache.UpdateNamespaceAnnotationState(oldNS, true)
			}
			if had {
				if err := c.reconcileDelete(handler, nsName, netName, oldNS, oldState); err != nil {
					return fmt.Errorf("%s: failed to delete namespace %s for network %s: %w", c.name, nsName, netName, err)
				}
			}
			c.deleteNamespaceReconciliation(netName, nsName)
			return nil
		}

		has, claimErr := handler.ClaimsNamespace(nsName)
		if claimErr != nil {
			// No state mutation on error; the workqueue retries.
			return fmt.Errorf("%s: ClaimsNamespace(%q) for network %q: %w", c.name, nsName, netName, claimErr)
		}

		switch {
		case !had && !has:
			// Not ours, never was. Drain the bootstrap entry and return.
			c.deleteNamespaceReconciliation(netName, nsName)
			return nil

		case had && !has:
			// Active → inactive (e.g. NAD moved the namespace out): delete the
			// state programmed for the previous network so it doesn't leak.
			var oldState *NamespaceAnnotationState
			if oldNS != nil {
				oldState = c.annotationCache.UpdateNamespaceAnnotationState(oldNS, false)
			}
			if err := c.reconcileDelete(handler, nsName, netName, oldNS, oldState); err != nil {
				return fmt.Errorf("%s: failed to delete namespace %s for network %s: %w", c.name, nsName, netName, err)
			}
			c.deleteNamespaceReconciliation(netName, nsName)
			return nil

		case !had && has:
			// Inactive → active: clear the cached prior view for a clean add.
			oldNS = nil
		}

		// had && has (update), or force-add. A bootstrap namespace is treated
		// as a fresh add even if an informer event already marked it active.
		if c.namespaceNeedsReconciliation(netName, nsName) {
			oldNS = nil
		}

		var oldState *NamespaceAnnotationState
		if oldNS != nil {
			oldState = c.annotationCache.UpdateNamespaceAnnotationState(oldNS, false)
		}
		newState := c.annotationCache.UpdateNamespaceAnnotationState(newNS, true)

		c.setNamespaceActive(netName, nsName)
		return c.reconcileUpdate(handler, oldNS, newNS, netName, oldState, newState)
	})
}

func (c *NamespaceController) reconcileUpdate(handler NamespaceHandler, oldNS, newNS *corev1.Namespace, netName string, oldState, newState *NamespaceAnnotationState) error {
	err := handler.ReconcileNamespace(oldNS, newNS, oldState, newState)
	c.setLatestInformerNamespace(netName, newNS)
	if err != nil {
		return err
	}
	c.setCachedNamespace(netName, newNS)
	c.deleteNamespaceReconciliation(netName, newNS.Name)
	return nil
}

func (c *NamespaceController) reconcileDelete(handler NamespaceHandler, nsName, netName string, oldNS *corev1.Namespace, oldState *NamespaceAnnotationState) error {
	if oldNS == nil {
		oldNS = c.getLatestInformerNamespace(netName, nsName)
		if oldNS != nil {
			oldState = c.annotationCache.UpdateNamespaceAnnotationState(oldNS, false)
		}
	}
	if oldNS == nil {
		oldNS = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
	}
	if err := handler.ReconcileNamespace(oldNS, nil, oldState, nil); err != nil {
		return err
	}

	c.deleteNamespaceActive(netName, nsName)
	c.deleteCachedNamespace(netName, nsName)
	c.deleteLatestInformerNamespace(netName, nsName)

	if c.namespaceHasAnyNetwork(nsName) {
		return nil
	}
	c.annotationCache.DeleteNamespace(nsName)
	return nil
}

func (c *NamespaceController) bootstrapNetwork(netName string, handler NamespaceHandler) error {
	nss, err := c.nsLister.List(labels.Everything())
	if err != nil {
		return err
	}
	if err := handler.SyncNamespaces(nss); err != nil {
		return err
	}
	// Empty-name namespaces can't roundtrip the queue key encoding and would
	// never reconcile, hanging the bootstrap drain. Filter them out.
	valid := nss[:0]
	for _, ns := range nss {
		if ns.Name == "" {
			continue
		}
		valid = append(valid, ns)
	}
	c.setBootstrapNamespaces(netName, valid)
	for _, ns := range valid {
		c.nsController.Reconcile(scopedNamespaceQueueKey(ns.Name, netName))
	}
	return nil
}

func (c *NamespaceController) setBootstrapNamespaces(netName string, nss []*corev1.Namespace) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if len(nss) == 0 {
		return
	}
	recon := make(map[string]struct{}, len(nss))
	pending := make(map[string]struct{}, len(nss))
	for _, ns := range nss {
		recon[ns.Name] = struct{}{}
		pending[ns.Name] = struct{}{}
	}
	c.nsReconciliation[netName] = recon
	c.bootstrapPending[netName] = pending
}

// markBootstrapAttempted records that nsName has had its first reconcile
// attempt for netName. Idempotent; called via defer for every attempt.
func (c *NamespaceController) markBootstrapAttempted(netName, nsName string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	pending, ok := c.bootstrapPending[netName]
	if !ok {
		return
	}
	delete(pending, nsName)
	if len(pending) == 0 {
		delete(c.bootstrapPending, netName)
	}
}

func (c *NamespaceController) namespaceNeedsReconciliation(netName, nsName string) bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	nss := c.nsReconciliation[netName]
	if len(nss) == 0 {
		return false
	}
	_, ok := nss[nsName]
	return ok
}

func (c *NamespaceController) deleteNamespaceReconciliation(netName, nsName string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	nss := c.nsReconciliation[netName]
	if len(nss) == 0 {
		return
	}
	delete(nss, nsName)
	if len(nss) == 0 {
		delete(c.nsReconciliation, netName)
	}
}

func (c *NamespaceController) namespaceHasAnyNetwork(nsName string) bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return len(c.nsNetworks[nsName]) > 0
}

// namespaceHasNetwork reports whether the last successfully-applied state for
// nsName under netName was active ("had" in the transition gate).
func (c *NamespaceController) namespaceHasNetwork(netName, nsName string) bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	nss := c.nsActive[netName]
	if len(nss) == 0 {
		return false
	}
	_, ok := nss[nsName]
	return ok
}

func (c *NamespaceController) setNamespaceActive(netName, nsName string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.nsActive == nil {
		c.nsActive = map[string]map[string]struct{}{}
	}
	if c.nsNetworks == nil {
		c.nsNetworks = map[string]map[string]struct{}{}
	}
	nss := c.nsActive[netName]
	if nss == nil {
		nss = map[string]struct{}{}
		c.nsActive[netName] = nss
	}
	nss[nsName] = struct{}{}
	networks := c.nsNetworks[nsName]
	if networks == nil {
		networks = map[string]struct{}{}
		c.nsNetworks[nsName] = networks
	}
	networks[netName] = struct{}{}
}

func (c *NamespaceController) deleteNamespaceActive(netName, nsName string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if nss, ok := c.nsActive[netName]; ok {
		delete(nss, nsName)
		if len(nss) == 0 {
			delete(c.nsActive, netName)
		}
	}
	if networks, ok := c.nsNetworks[nsName]; ok {
		delete(networks, netName)
		if len(networks) == 0 {
			delete(c.nsNetworks, nsName)
		}
	}
}

func scopedNamespaceQueueKey(nsName, netName string) string {
	if netName == "" {
		return nsName
	}
	return nsName + scopedNamespaceQueueKeySeparator + netName
}

func parseScopedNamespaceQueueKey(key string) (nsName, netName string) {
	parts := strings.SplitN(key, scopedNamespaceQueueKeySeparator, 2)
	if len(parts) != 2 {
		// Bare key, no separator: an informer event for the namespace
		// resource itself (network unscoped).
		return key, ""
	}
	return parts[0], parts[1]
}

func (c *NamespaceController) getCachedNamespace(netName, nsName string) *corev1.Namespace {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	ns := c.nsCache[netName][nsName]
	if ns == nil {
		return nil
	}
	return ns.DeepCopy()
}

func (c *NamespaceController) setCachedNamespace(netName string, ns *corev1.Namespace) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.nsCache[netName] == nil {
		c.nsCache[netName] = map[string]*corev1.Namespace{}
	}
	c.nsCache[netName][ns.Name] = ns.DeepCopy()
}

func (c *NamespaceController) deleteCachedNamespace(netName, nsName string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	nss := c.nsCache[netName]
	if nss == nil {
		return
	}
	delete(nss, nsName)
	if len(nss) == 0 {
		delete(c.nsCache, netName)
	}
}

func (c *NamespaceController) getLatestInformerNamespace(netName, nsName string) *corev1.Namespace {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	ns := c.latestInformerNsCache[netName][nsName]
	if ns == nil {
		return nil
	}
	return ns.DeepCopy()
}

func (c *NamespaceController) setLatestInformerNamespace(netName string, ns *corev1.Namespace) {
	if ns == nil {
		return
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.latestInformerNsCache[netName] == nil {
		c.latestInformerNsCache[netName] = map[string]*corev1.Namespace{}
	}
	c.latestInformerNsCache[netName][ns.Name] = ns.DeepCopy()
}

func (c *NamespaceController) deleteLatestInformerNamespace(netName, nsName string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	nss := c.latestInformerNsCache[netName]
	if nss == nil {
		return
	}
	delete(nss, nsName)
	if len(nss) == 0 {
		delete(c.latestInformerNsCache, netName)
	}
}
