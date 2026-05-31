// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package namespace

import (
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
)

type fakeNamespaceHandler struct {
	netName        string
	syncErr        error
	reconcileErr   error
	deleteErr      error
	claimsFn       func(string) (bool, error)
	syncCalls      int
	reconcileCalls int
	deleteCalls    int
	lastOldNS      *corev1.Namespace
}

func (f *fakeNamespaceHandler) GetNetworkName() string { return f.netName }

func (f *fakeNamespaceHandler) ClaimsNamespace(nsName string) (bool, error) {
	if f.claimsFn != nil {
		return f.claimsFn(nsName)
	}
	return nsName != "", nil
}

func (f *fakeNamespaceHandler) ReconcileNamespace(oldNS, newNS *corev1.Namespace, _, _ *NamespaceAnnotationState) error {
	if oldNS != nil {
		f.lastOldNS = oldNS.DeepCopy()
	} else {
		f.lastOldNS = nil
	}
	if newNS == nil {
		f.deleteCalls++
		return f.deleteErr
	}
	f.reconcileCalls++
	return f.reconcileErr
}

func (f *fakeNamespaceHandler) SyncNamespaces(_ []*corev1.Namespace) error {
	f.syncCalls++
	return f.syncErr
}

func newNamespaceLister(t *testing.T, nss ...*corev1.Namespace) corelisters.NamespaceLister {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, ns := range nss {
		if err := indexer.Add(ns); err != nil {
			t.Fatalf("failed to add namespace to indexer: %v", err)
		}
	}
	return corelisters.NewNamespaceLister(indexer)
}

func newNamespaceControllerForTest(threadiness int) controller.Controller {
	return controller.NewController("topology-test-namespace-controller", &controller.ControllerConfig[corev1.Namespace]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      func(string) error { return nil },
		Threadiness:    threadiness,
		Lister:         func(labels.Selector) ([]*corev1.Namespace, error) { return nil, nil },
		ObjNeedsUpdate: func(_, _ *corev1.Namespace) bool { return true },
	})
}

func newTestController(t *testing.T) *NamespaceController {
	t.Helper()
	return &NamespaceController{
		name:                  "topology-test",
		networkManager:        networkmanager.Default().Interface(),
		nsLister:              newNamespaceLister(t),
		handlers:              syncmap.NewSyncMap[NamespaceHandler](),
		nsReconciliation:      map[string]map[string]struct{}{},
		bootstrapPending:      map[string]map[string]struct{}{},
		nsActive:              map[string]map[string]struct{}{},
		nsNetworks:            map[string]map[string]struct{}{},
		nsCache:               map[string]map[string]*corev1.Namespace{},
		latestInformerNsCache: map[string]map[string]*corev1.Namespace{},
		annotationCache:       NewNamespaceAnnotationCache(),
		nsController:          newNamespaceControllerForTest(0),
	}
}

func TestScopedNamespaceQueueKeyRoundtrip(t *testing.T) {
	cases := []struct{ ns, net string }{
		{"alpha", "default"},
		{"beta", "tenant-a"},
		{"with-dash", "udn-1"},
		// Empty namespace name must roundtrip too — kubevirt tests
		// historically create namespace fixtures with zero-valued names,
		// and a non-roundtripping parse would loop the fan-out
		// indefinitely ("|net" → bare → enqueue "|net|net" → ...).
		{"", "default"},
	}
	for _, tc := range cases {
		key := scopedNamespaceQueueKey(tc.ns, tc.net)
		ns, net := parseScopedNamespaceQueueKey(key)
		if ns != tc.ns || net != tc.net {
			t.Fatalf("roundtrip for (%q,%q) yielded (%q,%q)", tc.ns, tc.net, ns, net)
		}
	}
}

func TestParseScopedNamespaceQueueKey_FanOut(t *testing.T) {
	// An empty net name signals fan-out: the parser returns net == "".
	ns, net := parseScopedNamespaceQueueKey("alpha")
	if ns != "alpha" || net != "" {
		t.Fatalf("unexpected fan-out parse: ns=%q net=%q", ns, net)
	}
}

func TestWaitForBootstrap(t *testing.T) {
	c := newTestController(t)
	// Empty bootstrapPending drains immediately.
	if err := c.WaitForBootstrap("default", 100*time.Millisecond); err != nil {
		t.Fatalf("empty bootstrapPending should drain immediately; got %v", err)
	}
	// A pending bootstrap entry blocks until drained; the timeout
	// expires here because nothing clears it.
	c.stateMu.Lock()
	c.bootstrapPending["default"] = map[string]struct{}{"alpha": {}}
	c.stateMu.Unlock()
	if err := c.WaitForBootstrap("default", 50*time.Millisecond); err == nil {
		t.Fatal("WaitForBootstrap should time out while bootstrapPending has entries")
	}
	// markBootstrapAttempted lets it drain (the public path; equivalent
	// to what reconcileNamespace's defer does).
	c.markBootstrapAttempted("default", "alpha")
	if err := c.WaitForBootstrap("default", 100*time.Millisecond); err != nil {
		t.Fatalf("after markBootstrapAttempted, bootstrap should drain; got %v", err)
	}
}

func TestWaitForBootstrap_DrainsOnHandlerError(t *testing.T) {
	// Regression: previously the bootstrap entry was cleared only on
	// SUCCESSFUL handler return, so one transient reconcile error
	// (e.g., a malformed external-gateway annotation crashing parse, a
	// transient NBDB hiccup) would brick controller startup after the
	// 30s timeout. WaitForBootstrap must now return as soon as every
	// bootstrap namespace has been ATTEMPTED, regardless of outcome —
	// failed namespaces stay in the workqueue's retry path with normal
	// backoff but no longer block dependent watchers.
	c := newTestController(t)
	wantErr := errors.New("transient handler failure")
	h := &fakeNamespaceHandler{netName: "default", reconcileErr: wantErr}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "alpha"}}
	c.nsLister = newNamespaceLister(t, ns)
	c.handlers.Store("default", h)
	c.setBootstrapNamespaces("default", []*corev1.Namespace{ns})

	// Sanity: the namespace is pending before any reconcile attempt.
	c.stateMu.RLock()
	if _, pending := c.bootstrapPending["default"]["alpha"]; !pending {
		c.stateMu.RUnlock()
		t.Fatal("test premise: bootstrapPending must contain alpha before the reconcile")
	}
	c.stateMu.RUnlock()

	// Run the reconcile. The handler returns an error, so the
	// reconcileNamespace return propagates the error to the workqueue
	// for retry. But the bootstrap entry MUST be cleared by the defer.
	err := c.reconcileNamespace(scopedNamespaceQueueKey("alpha", "default"))
	if err == nil {
		t.Fatal("expected handler error to propagate to the workqueue")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped %v, got %v", wantErr, err)
	}

	// WaitForBootstrap must return without timing out — the namespace
	// has been ATTEMPTED, that's all the drain cares about.
	if err := c.WaitForBootstrap("default", 100*time.Millisecond); err != nil {
		t.Fatalf("bootstrap should drain after one attempt even on handler error; got %v", err)
	}

	// nsReconciliation entry persists (still needs the bootstrap
	// fresh-add semantic on the next retry) — only bootstrapPending
	// gets cleared by the attempt.
	c.stateMu.RLock()
	if _, stillPending := c.nsReconciliation["default"]["alpha"]; !stillPending {
		t.Error("nsReconciliation entry must persist on handler error so the retry treats it as fresh-add")
	}
	c.stateMu.RUnlock()
}

func TestWaitForBootstrap_TimeoutErrorIncludesSample(t *testing.T) {
	// The timeout error message must include a sample of pending
	// namespaces so operators debugging a hung startup can see which
	// namespaces never had a worker pick them up.
	c := newTestController(t)
	c.stateMu.Lock()
	c.bootstrapPending["default"] = map[string]struct{}{
		"alpha": {}, "beta": {}, "gamma": {},
	}
	c.stateMu.Unlock()
	err := c.WaitForBootstrap("default", 30*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "3 namespaces pending") {
		t.Errorf("error must include count of pending namespaces: %q", msg)
	}
	// At least one of the pending names should appear in the message
	// (we don't assert exact ordering since map iteration is unordered).
	hasSample := false
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if strings.Contains(msg, name) {
			hasSample = true
			break
		}
	}
	if !hasSample {
		t.Errorf("error must include at least one pending namespace name: %q", msg)
	}
}

func TestReconcileNamespace_EmptyNameIsNoop(t *testing.T) {
	// scopedNamespaceQueueKey("", "default") encodes as "|default";
	// parseScopedNamespaceQueueKey returns ("", "default"); reconcile
	// short-circuits on the empty nsName so fan-out doesn't loop and
	// no handler is invoked. Asserting nil return (no error) is the
	// observable contract.
	c := newTestController(t)
	h := &fakeNamespaceHandler{netName: "default"}
	if err := c.RegisterNetworkController(h); err != nil {
		t.Fatalf("RegisterNetworkController failed: %v", err)
	}
	if err := c.reconcileNamespace(scopedNamespaceQueueKey("", "default")); err != nil {
		t.Fatalf("reconcileNamespace on empty-name key should be a no-op; got %v", err)
	}
	if h.reconcileCalls != 0 {
		t.Fatalf("handler.ReconcileNamespace should not have been called for empty-name key; got %d calls", h.reconcileCalls)
	}
}

func TestRegisterDeregisterNetworkController(t *testing.T) {
	c := newTestController(t)
	h := &fakeNamespaceHandler{netName: "net-a"}

	if err := c.RegisterNetworkController(h); err != nil {
		t.Fatalf("RegisterNetworkController failed: %v", err)
	}
	if h.syncCalls != 1 {
		t.Fatalf("expected SyncNamespaces to be called once on register; got %d", h.syncCalls)
	}

	if got, ok := c.handlers.Load("net-a"); !ok || got != h {
		t.Fatalf("handler not stored after register")
	}

	c.DeregisterNetworkController("net-a")
	if _, ok := c.handlers.Load("net-a"); ok {
		t.Fatalf("handler still present after deregister")
	}
}

func TestBootstrapNetwork_FiltersEmptyNameNamespaces(t *testing.T) {
	// Empty-name namespaces (test fixtures only — Kubernetes rejects
	// them in production) must NOT be enqueued during bootstrap, and
	// must NOT contribute to nsReconciliation. scopedNamespaceQueueKey
	// produces "|net" for empty nsName which the parser then loops on
	// during fan-out; the bootstrap-side filter is the upstream
	// defense for the reconcileNamespace short-circuit.
	c := newTestController(t)
	c.nsLister = newNamespaceLister(t,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "real-ns"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ""}},
	)
	h := &fakeNamespaceHandler{netName: "net-a"}
	if err := c.RegisterNetworkController(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	// SyncNamespaces is called with the unfiltered list (it's the
	// handler's call); the bootstrap drain set is what matters.
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	pending := c.nsReconciliation["net-a"]
	if _, ok := pending[""]; ok {
		t.Fatalf("empty-name namespace must not be in nsReconciliation; got %v", pending)
	}
	if _, ok := pending["real-ns"]; !ok {
		t.Fatalf("real-ns must be in nsReconciliation; got %v", pending)
	}
}

func TestRegisterNetworkControllerBootstrapFailure(t *testing.T) {
	c := newTestController(t)
	h := &fakeNamespaceHandler{netName: "net-a", syncErr: errors.New("sync failed")}

	err := c.RegisterNetworkController(h)
	if err == nil {
		t.Fatal("expected RegisterNetworkController to return the bootstrap error")
	}
	if _, ok := c.handlers.Load("net-a"); ok {
		t.Fatal("handler must not be retained when bootstrap fails")
	}
}

func TestDuplicateRegistrationPanics(t *testing.T) {
	c := newTestController(t)
	h := &fakeNamespaceHandler{netName: "net-a"}
	if err := c.RegisterNetworkController(h); err != nil {
		t.Fatalf("first register should succeed: %v", err)
	}

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected duplicate registration to panic")
		}
	}()
	_ = c.RegisterNetworkController(&fakeNamespaceHandler{netName: "net-a"})
}

func TestRegisterIsNilSafe(t *testing.T) {
	c := newTestController(t)
	if err := c.RegisterNetworkController(nil); err == nil {
		t.Fatal("expected nil-handler registration to error")
	}
}

func TestBootstrapNamespacesAreTreatedAsFreshAdd(t *testing.T) {
	// setBootstrapNamespaces seeds nsReconciliation so the gate's
	// fall-through nulls oldNS (fresh-add semantics) for namespaces
	// enqueued by bootstrap. A successful reconcile clears the entry.
	c := newTestController(t)
	nsA := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns1"}}
	c.setBootstrapNamespaces("net-a", []*corev1.Namespace{nsA})

	if !c.namespaceNeedsReconciliation("net-a", "ns1") {
		t.Fatalf("bootstrap namespace ns1 should be flagged for fresh-add reconciliation")
	}
	// A namespace not seeded by bootstrap is not flagged.
	if c.namespaceNeedsReconciliation("net-a", "other") {
		t.Fatalf("non-bootstrap namespace must not be flagged")
	}
	// Clearing (what reconcileUpdate/reconcileDelete do on success)
	// removes the flag.
	c.deleteNamespaceReconciliation("net-a", "ns1")
	if c.namespaceNeedsReconciliation("net-a", "ns1") {
		t.Fatalf("flag must be cleared after deleteNamespaceReconciliation")
	}
}

func TestSetAndDeleteNamespaceActive(t *testing.T) {
	c := newTestController(t)
	c.setNamespaceActive("net-a", "ns1")
	c.setNamespaceActive("net-b", "ns1")

	if !c.namespaceHasAnyNetwork("ns1") {
		t.Fatal("ns1 should be active on at least one network")
	}

	c.deleteNamespaceActive("net-a", "ns1")
	if !c.namespaceHasAnyNetwork("ns1") {
		t.Fatal("ns1 should still be active on net-b after removing net-a")
	}
	c.deleteNamespaceActive("net-b", "ns1")
	if c.namespaceHasAnyNetwork("ns1") {
		t.Fatal("ns1 should not be active on any network after both removed")
	}
}

func TestNamespaceHasNetwork(t *testing.T) {
	c := newTestController(t)

	if c.namespaceHasNetwork("net-a", "ns1") {
		t.Fatal("namespaceHasNetwork must return false before any active set")
	}

	c.setNamespaceActive("net-a", "ns1")
	if !c.namespaceHasNetwork("net-a", "ns1") {
		t.Fatal("namespaceHasNetwork must return true after setNamespaceActive")
	}
	// Scoping: setting (net-a, ns1) active must not leak to (net-b, ns1)
	// or (net-a, ns2). The transition gate relies on the per-pair
	// answer being correct.
	if c.namespaceHasNetwork("net-b", "ns1") {
		t.Fatal("namespaceHasNetwork must not leak across networks")
	}
	if c.namespaceHasNetwork("net-a", "ns2") {
		t.Fatal("namespaceHasNetwork must not leak across namespaces")
	}

	c.deleteNamespaceActive("net-a", "ns1")
	if c.namespaceHasNetwork("net-a", "ns1") {
		t.Fatal("namespaceHasNetwork must return false after deleteNamespaceActive")
	}
}

// transitionGateFixture wires a NamespaceController against a single
// registered fake handler and a lister seeded with the given namespaces,
// then returns helpers the per-case tests use to drive reconcileNamespace
// directly. Bootstrap is intentionally bypassed (handler is stored
// without RegisterNetworkController) so individual tests can control the
// pre-conditions of the gate (had / claims-fn / lister content) without
// the full registration pass interfering.
func transitionGateFixture(t *testing.T, netName string, claims func(string) (bool, error), nss ...*corev1.Namespace) (*NamespaceController, *fakeNamespaceHandler) {
	t.Helper()
	c := newTestController(t)
	c.nsLister = newNamespaceLister(t, nss...)
	h := &fakeNamespaceHandler{netName: netName, claimsFn: claims}
	c.handlers.Store(netName, h)
	return c, h
}

func TestReconcileNamespace_TransitionGate(t *testing.T) {
	const netName = "net-a"
	const nsName = "ns1"
	makeNS := func() *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
	}

	t.Run("!had && !has → no dispatch, no caching", func(t *testing.T) {
		c, h := transitionGateFixture(t, netName, func(string) (bool, error) { return false, nil }, makeNS())
		if err := c.reconcileNamespace(scopedNamespaceQueueKey(nsName, netName)); err != nil {
			t.Fatalf("reconcileNamespace: %v", err)
		}
		if h.reconcileCalls != 0 || h.deleteCalls != 0 {
			t.Fatalf("expected zero handler calls; got reconcile=%d delete=%d", h.reconcileCalls, h.deleteCalls)
		}
		if c.namespaceHasNetwork(netName, nsName) {
			t.Fatal("namespace must not be marked active for an unclaimed namespace")
		}
		if c.getCachedNamespace(netName, nsName) != nil {
			t.Fatal("namespace must not be cached for an unclaimed namespace")
		}
	})

	t.Run("!had && has → fresh add, oldNS nil", func(t *testing.T) {
		c, h := transitionGateFixture(t, netName, func(string) (bool, error) { return true, nil }, makeNS())
		if err := c.reconcileNamespace(scopedNamespaceQueueKey(nsName, netName)); err != nil {
			t.Fatalf("reconcileNamespace: %v", err)
		}
		if h.reconcileCalls != 1 || h.deleteCalls != 0 {
			t.Fatalf("expected exactly one add; got reconcile=%d delete=%d", h.reconcileCalls, h.deleteCalls)
		}
		if h.lastOldNS != nil {
			t.Fatalf("force-add path must dispatch with oldNS=nil; got %#v", h.lastOldNS)
		}
		if !c.namespaceHasNetwork(netName, nsName) {
			t.Fatal("namespace must be active after a successful add")
		}
		if c.getCachedNamespace(netName, nsName) == nil {
			t.Fatal("namespace must be cached after a successful add")
		}
	})

	t.Run("had && has → normal update, oldNS from cache", func(t *testing.T) {
		c, h := transitionGateFixture(t, netName, func(string) (bool, error) { return true, nil }, makeNS())
		// Seed prior applied state.
		prior := makeNS()
		prior.Annotations = map[string]string{"prior": "yes"}
		c.setCachedNamespace(netName, prior)
		c.setNamespaceActive(netName, nsName)

		if err := c.reconcileNamespace(scopedNamespaceQueueKey(nsName, netName)); err != nil {
			t.Fatalf("reconcileNamespace: %v", err)
		}
		if h.reconcileCalls != 1 {
			t.Fatalf("expected one update dispatch; got %d", h.reconcileCalls)
		}
		if h.lastOldNS == nil || h.lastOldNS.Annotations["prior"] != "yes" {
			t.Fatalf("update path must pass cached oldNS; got %#v", h.lastOldNS)
		}
	})

	t.Run("had && !has → delete leg, cache + active cleared", func(t *testing.T) {
		c, h := transitionGateFixture(t, netName, func(string) (bool, error) { return false, nil }, makeNS())
		c.setCachedNamespace(netName, makeNS())
		c.setNamespaceActive(netName, nsName)

		if err := c.reconcileNamespace(scopedNamespaceQueueKey(nsName, netName)); err != nil {
			t.Fatalf("reconcileNamespace: %v", err)
		}
		if h.deleteCalls != 1 || h.reconcileCalls != 0 {
			t.Fatalf("expected exactly one delete; got reconcile=%d delete=%d", h.reconcileCalls, h.deleteCalls)
		}
		if c.namespaceHasNetwork(netName, nsName) {
			t.Fatal("namespace must be cleared from active set after delete")
		}
		if c.getCachedNamespace(netName, nsName) != nil {
			t.Fatal("namespace cache must be cleared after delete")
		}
	})

	t.Run("deletion event + had → delete leg fires", func(t *testing.T) {
		// No namespace in lister → deletion event.
		c, h := transitionGateFixture(t, netName, func(string) (bool, error) {
			t.Fatal("ClaimsNamespace must not be called on deletion path")
			return false, nil
		})
		c.setCachedNamespace(netName, makeNS())
		c.setNamespaceActive(netName, nsName)

		if err := c.reconcileNamespace(scopedNamespaceQueueKey(nsName, netName)); err != nil {
			t.Fatalf("reconcileNamespace: %v", err)
		}
		if h.deleteCalls != 1 {
			t.Fatalf("expected exactly one delete on deletion event; got %d", h.deleteCalls)
		}
		if c.namespaceHasNetwork(netName, nsName) || c.getCachedNamespace(netName, nsName) != nil {
			t.Fatal("active + cache must be cleared on deletion")
		}
	})

	t.Run("deletion event + !had → no dispatch", func(t *testing.T) {
		c, h := transitionGateFixture(t, netName, func(string) (bool, error) {
			t.Fatal("ClaimsNamespace must not be called on deletion path")
			return false, nil
		})

		if err := c.reconcileNamespace(scopedNamespaceQueueKey(nsName, netName)); err != nil {
			t.Fatalf("reconcileNamespace: %v", err)
		}
		if h.reconcileCalls != 0 || h.deleteCalls != 0 {
			t.Fatalf("expected zero handler calls; got reconcile=%d delete=%d", h.reconcileCalls, h.deleteCalls)
		}
	})

	t.Run("ClaimsNamespace error → no state mutation", func(t *testing.T) {
		// Load-bearing test for the (bool, error) signature: the gate
		// must NOT change cache or active state on the error path,
		// otherwise a transient lookup failure during NAD-cache warmup
		// would either spuriously force-add or spuriously delete.
		want := errors.New("lookup transient failure")
		c, h := transitionGateFixture(t, netName, func(string) (bool, error) { return false, want }, makeNS())
		c.setCachedNamespace(netName, makeNS())
		c.setNamespaceActive(netName, nsName)

		err := c.reconcileNamespace(scopedNamespaceQueueKey(nsName, netName))
		if err == nil {
			t.Fatal("expected reconcile to surface ClaimsNamespace error")
		}
		if !errors.Is(err, want) {
			t.Fatalf("expected wrapped %v, got %v", want, err)
		}
		if h.reconcileCalls != 0 || h.deleteCalls != 0 {
			t.Fatalf("handler must not be dispatched on ClaimsNamespace error; got reconcile=%d delete=%d", h.reconcileCalls, h.deleteCalls)
		}
		if !c.namespaceHasNetwork(netName, nsName) {
			t.Fatal("active state must be preserved on ClaimsNamespace error")
		}
		if c.getCachedNamespace(netName, nsName) == nil {
			t.Fatal("cache must be preserved on ClaimsNamespace error")
		}
	})

	t.Run("deletion + reconcileDelete error → state preserved for retry", func(t *testing.T) {
		c, h := transitionGateFixture(t, netName, nil)
		c.setCachedNamespace(netName, makeNS())
		c.setNamespaceActive(netName, nsName)
		// Force the delete leg to fail. reconcileDelete dispatches
		// ReconcileNamespace(oldNS, nil, ...); to fail it, override
		// the handler's ReconcileNamespace behavior via reconcileErr.
		// (deleteCalls increments before the nil-newNS branch returns,
		// but reconcileErr is only returned on the add path; need a
		// different signal — extend the fake.)
		h.deleteErr = errors.New("ovn down")

		err := c.reconcileNamespace(scopedNamespaceQueueKey(nsName, netName))
		if err == nil {
			t.Fatal("expected reconcileDelete error to bubble")
		}
		if !errors.Is(err, h.deleteErr) {
			t.Fatalf("expected wrapped %v, got %v", h.deleteErr, err)
		}
		// On error, cache + active must NOT be cleared so the next
		// reconcile retries the delete.
		if !c.namespaceHasNetwork(netName, nsName) {
			t.Fatal("active state must be preserved when delete leg errors")
		}
		if c.getCachedNamespace(netName, nsName) == nil {
			t.Fatal("cache must be preserved when delete leg errors")
		}
	})
}

func TestCachedNamespaceRoundtrip(t *testing.T) {
	c := newTestController(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns1"}}

	if got := c.getCachedNamespace("net-a", "ns1"); got != nil {
		t.Fatal("expected nil for unknown cache lookup")
	}
	c.setCachedNamespace("net-a", ns)
	got := c.getCachedNamespace("net-a", "ns1")
	if got == nil || got.Name != "ns1" {
		t.Fatalf("expected cached namespace name=ns1, got %#v", got)
	}
	// Mutating the returned object must not affect the cache (deep-copy).
	got.Annotations = map[string]string{"injected": "yes"}
	again := c.getCachedNamespace("net-a", "ns1")
	if _, present := again.Annotations["injected"]; present {
		t.Fatal("mutation of returned namespace leaked into cache")
	}
	c.deleteCachedNamespace("net-a", "ns1")
	if c.getCachedNamespace("net-a", "ns1") != nil {
		t.Fatal("expected nil after deleteCachedNamespace")
	}
}
