// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	observabilityconfigv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
)

// OVN SamplingApp IDs; must be < 255. Add new apps at the end.
const (
	DropSamplingID = iota + 1
	ACLNewTrafficSamplingID
	ACLEstTrafficSamplingID
)

// maxCollectorID is the OVN Sample_Collector row id limit (table column id).
const maxCollectorID = 255

const collectorFeaturesExternalID = "sample-features"

// maxSyncRetries bounds how many times the sampling sync is retried before giving up.
// Combined with retryInterval (default 1 minute) this allows roughly one hour to converge.
const maxSyncRetries = 60

// collectorConfig holds the configuration for a collector.
// It is allowed to set different probabilities for every feature.
// collectorSetID is used to set up sampling via OVSDB.
type collectorConfig struct {
	collectorSetID int
	// probability in percent, 0 to 100
	featuresProbability map[libovsdbops.SampleFeature]int
}

// featureResolution captures, for a single feature, the collectors that apply cluster-wide
// (from configs with no Filter.Namespaces) and the per-namespace collectors (from
// namespace-scoped configs). Collector identity is a UUID; storing sets deduplicates
// automatically, since the same collector may be declared by several configs.
type featureResolution struct {
	cluster sets.Set[string]            // collectors applied to every namespace
	perNS   map[string]sets.Set[string] // namespace -> collectors applied only there
}

type Manager struct {
	nbClient libovsdbclient.Client
	// controllerName is the OwnerControllerKey of the ACLs whose sampling this Manager
	// governs, i.e. the controller it was handed to. The sampling sync must not touch any
	// other controller's ACLs: a controller without a Manager creates its ACLs unsampled on
	// purpose and would strip any Sample attached behind its back on the next reconcile.
	controllerName string
	// resolution is the published per-feature collector assignment for this node, rebuilt
	// on every applyConfigs/clearConfig. It is both what SamplingConfigForContext resolves
	// against and the basis for computing resync deltas. nil means nothing applies.
	resolution     map[libovsdbops.SampleFeature]*featureResolution
	collectorsLock sync.RWMutex
	// nbdb Collectors have probability. To allow different probabilities for different features,
	// multiple nbdb Collectors will be created, one per probability.
	// getCollectorKey() => collector.UUID
	dbCollectors map[string]string
	// A collector cannot be deleted while a Sample still references it. The sampling sync
	// detaches those Samples in the same pass, just before cleanup runs, so cleanup normally
	// succeeds immediately; state is kept here for the cases where it has to be retried.
	// getCollectorKey() => collector.SetID
	unusedCollectors map[string]int
	retryInterval    time.Duration
	// Only maxCollectorID collectors are allowed, each should have unique ID.
	// this set is tracking already assigned IDs.
	takenCollectorIDs sets.Set[int]

	// Retry state for the sampling sync. A single timer is armed at a time; retryLock guards
	// all of these fields. retryLock and collectorsLock are never held simultaneously.
	retryLock  sync.Mutex
	retryTimer *time.Timer
	retries    int
	stopped    bool
	// generation counts resolution changes. A retry pass captures it before it starts and hands
	// it back with its result, so a result produced for a resolution that has since been
	// superseded can be discarded instead of acted on. Guarded by retryLock, deliberately not by
	// collectorsLock: that keeps the two locks disjoint rather than nested.
	generation uint64
}

// NewManager returns a Manager governing the sampling of the ACLs owned by controllerName.
func NewManager(nbClient libovsdbclient.Client, controllerName string) *Manager {
	return &Manager{
		nbClient:          nbClient,
		controllerName:    controllerName,
		collectorsLock:    sync.RWMutex{},
		dbCollectors:      make(map[string]string),
		unusedCollectors:  make(map[string]int),
		retryInterval:     time.Minute,
		takenCollectorIDs: sets.New[int](),
	}
}

// SamplingConfigForContext returns the sampling config to use for an ACL in the given
// namespace and feature. All configs that apply are merged: both namespace-scoped
// configs (Filter.Namespaces containing namespace) and cluster-scoped configs (no
// Filter.Namespaces), so the ACL's Sample references every matching collector and
// each receives samples at its configured probability. Returns nil if no config
// applies for that context.
func (m *Manager) SamplingConfigForContext(namespace string, feature libovsdbops.SampleFeature) *libovsdbops.SamplingConfig {
	m.collectorsLock.RLock()
	defer m.collectorsLock.RUnlock()
	collectors := m.collectorsForLocked(namespace, feature)
	if len(collectors) == 0 {
		return nil
	}
	return libovsdbops.NewSamplingConfig(map[libovsdbops.SampleFeature][]string{feature: collectors})
}

// collectorsForLocked returns the collectors that apply to (namespace, feature): the feature's
// cluster-wide collectors (from configs with no Filter.Namespaces) merged with the collectors
// scoped to this namespace. Collector UUIDs land in the Sample.Collectors OVSDB set column, so
// storing them as sets deduplicates naturally; the result is sorted for a deterministic
// Sample.Collectors. Returns nil if nothing applies.
// Caller must hold m.collectorsLock (at least RLock).
func (m *Manager) collectorsForLocked(namespace string, feature libovsdbops.SampleFeature) []string {
	fr := m.resolution[feature]
	if fr == nil {
		return nil
	}
	merged := fr.cluster.Union(fr.perNS[namespace])
	if merged.Len() == 0 {
		return nil
	}
	return sets.List(merged)
}

// Init sets up sampling app IDs and loads existing collector state from the DB.
// It does not apply any ObservabilityConfig; config is applied when StartWatching
// receives an ObservabilityConfig that applies to this node.
func (m *Manager) Init() error {
	if err := m.setSamplingAppIDs(); err != nil {
		return err
	}
	m.collectorsLock.Lock()
	defer m.collectorsLock.Unlock()
	return m.retrieveDbCollectorsLocked()
}

// StartWatching watches ObservabilityConfig CRs and applies all that apply to this node.
// In details: configs whose Filter.NodeSelector matches the local node labels apply;
// configs with no Filter.NodeSelector always apply. nodeWatcher is required (non-nil): a change
// to the local node's labels re-triggers reconciliation so a NodeSelector that starts or stops
// matching is honored. If the local node cannot be resolved, reconciliation is requeued.
// Multiple configs can apply (e.g. one cluster-wide, one namespace-scoped); use SamplingConfigForContext
// when creating ACLs so the correct config is chosen per (namespace, feature). Call after Init().
//
// The k8s watching and reconcile loop live in configReconciler (config_reconciler.go); this
// Manager provides the apply/clear engine it drives. ACLs that already exist when a config
// changes are re-pointed by syncSamplingLocked, so no other controller has to take part.
func (m *Manager) StartWatching(informer ObservabilityConfigInformer, nodeWatcher NodeWatcher, nodeName string, stopChan <-chan struct{}) {
	if informer == nil {
		return
	}
	if nodeWatcher == nil || nodeWatcher.NodeInformer() == nil {
		klog.Errorf("Observability: cannot start ObservabilityConfig reconciler: node watcher/informer is nil")
		return
	}
	r := newConfigReconciler(m, informer, nodeWatcher, nodeName)
	if err := r.start(); err != nil {
		klog.Errorf("Observability: failed to start ObservabilityConfig reconciler: %v", err)
		return
	}
	// Tear everything down when the stop channel closes: the reconciler (event handler +
	// worker) and this Manager's stale-collector cleanup timer.
	go func() {
		<-stopChan
		r.stop()
		m.stopRetryTimer()
	}()
}

// clearConfig clears the published resolution and triggers cleanup of collectors.
// SamplingConfigForContext() will return nil until configs are applied again, and the sampling
// sync drops the Sample references from every ACL that had them, which is what then allows the
// collectors themselves to be deleted.
func (m *Manager) clearConfig() {
	m.collectorsLock.Lock()
	m.resolution = nil
	// Detach sampling from every ACL first: a collector cannot be deleted while a Sample still
	// references it.
	_, syncErr := m.syncSamplingLocked()
	// Rebuild the DB snapshot and delete the collectors nothing refers to any more. The
	// resolution is nil by now, so that is all of them. Done under the same lock as retrieval
	// so a concurrent reader never observes a half-cleared state.
	staleErr := m.refreshCollectorsSnapshotLocked()
	if staleErr == nil {
		staleErr = m.deleteStaleCollectorsLocked()
	}
	m.collectorsLock.Unlock()
	// As in applyConfigs, verify afterwards: a write still in flight here would land carrying
	// the Samples this pass has just detached everywhere else.
	m.scheduleVerification(errors.Join(syncErr, staleErr))
}

// addToResolution folds one config's resolved feature->collectors into res, routing collectors
// to the cluster-wide set (no namespaces) or to each listed namespace's set. Sets deduplicate
// collectors declared by several configs.
func addToResolution(res map[libovsdbops.SampleFeature]*featureResolution, namespaces []string, featureCollectors map[libovsdbops.SampleFeature][]string) {
	for feature, collectors := range featureCollectors {
		if len(collectors) == 0 {
			continue
		}
		fr := res[feature]
		if fr == nil {
			fr = &featureResolution{cluster: sets.New[string](), perNS: map[string]sets.Set[string]{}}
			res[feature] = fr
		}
		if len(namespaces) == 0 {
			fr.cluster.Insert(collectors...)
			continue
		}
		for _, ns := range namespaces {
			s := fr.perNS[ns]
			if s == nil {
				s = sets.New[string]()
				fr.perNS[ns] = s
			}
			s.Insert(collectors...)
		}
	}
}

func collectorConfigFromCR(cr *observabilityconfigv1alpha1.ObservabilityConfig) *collectorConfig {
	c := &collectorConfig{
		collectorSetID:      int(cr.Spec.CollectorID),
		featuresProbability: make(map[libovsdbops.SampleFeature]int),
	}
	for _, f := range cr.Spec.Features {
		sf := observabilityFeatureToSampleFeature(f.Feature)
		if sf != "" {
			c.featuresProbability[sf] = int(f.Probability)
		} else {
			klog.Warningf("Observability: unknown feature in %s: %s", cr.Name, f.Feature)
		}
	}
	return c
}

func observabilityFeatureToSampleFeature(f observabilityconfigv1alpha1.ObservabilityFeature) libovsdbops.SampleFeature {
	switch f {
	case observabilityconfigv1alpha1.NetworkPolicy:
		return libovsdbops.NetworkPolicySample
	case observabilityconfigv1alpha1.AdminNetworkPolicy:
		return libovsdbops.AdminNetworkPolicySample
	case observabilityconfigv1alpha1.EgressFirewall:
		return libovsdbops.EgressFirewallSample
	case observabilityconfigv1alpha1.UDNIsolation:
		return libovsdbops.UDNIsolationSample
	case observabilityconfigv1alpha1.MulticastIsolation:
		return libovsdbops.MulticastSample
	default:
		return ""
	}
}

// namespacedObservabilityFeatures are the only features that support Filter.Namespaces (per-namespace filtering).
var namespacedObservabilityFeatures = map[observabilityconfigv1alpha1.ObservabilityFeature]struct{}{
	observabilityconfigv1alpha1.NetworkPolicy:  {},
	observabilityconfigv1alpha1.EgressFirewall: {},
}

// validateObservabilityConfig returns an error if the CR fails validation (e.g. collectorID/set_id out of range or probability not 0..100).
// API server CRD validation should enforce these too; this is defense in depth.
func validateObservabilityConfig(cr *observabilityconfigv1alpha1.ObservabilityConfig) error {
	if cr.Spec.CollectorID < 1 || cr.Spec.CollectorID > math.MaxUint32 {
		return fmt.Errorf("ObservabilityConfig %s: collectorID (set_id) must be between 1 and %d, got %d", cr.Name, int64(math.MaxUint32), cr.Spec.CollectorID)
	}
	for _, f := range cr.Spec.Features {
		if f.Probability < 0 || f.Probability > 100 {
			return fmt.Errorf("ObservabilityConfig %s: feature %s probability must be 0..100, got %d", cr.Name, f.Feature, f.Probability)
		}
	}
	// Namespace filter only applies to namespaced features (NetworkPolicy, EgressFirewall). Reject if any feature is cluster-scoped.
	if cr.Spec.Filter != nil && len(cr.Spec.Filter.Namespaces) > 0 {
		for _, f := range cr.Spec.Features {
			if _, ok := namespacedObservabilityFeatures[f.Feature]; !ok {
				return fmt.Errorf("ObservabilityConfig %s: Filter.Namespaces can only be used with namespaced features (NetworkPolicy, EgressFirewall); feature %s is cluster-scoped", cr.Name, f.Feature)
			}
		}
	}
	return nil
}

// applyConfigs applies all given ObservabilityConfigs: ensures collectors exist for each,
// stores them for context resolution, and publishes the resolved set for context lookups.
//
// Retrieval of the current DB collectors, selection of active collectors and stale cleanup
// all happen under a single hold of collectorsLock, so a concurrent stale-cleanup pass can
// never delete a collector between retrieval and reuse (items 6/7/11).
//
// A single invalid or failing config does not abort the others, and the two failure classes
// are treated differently:
//   - Invalid configs are skipped with a warning and do NOT fail the reconcile: they can only
//     be fixed by the user editing the CR, so requeuing would never converge. Surfacing these
//     to the user via status conditions is planned as a follow-up.
//   - Transient failures (e.g. OVSDB errors) are collected and returned joined, so the reconcile
//     path requeues them with backoff.
//
// Either way the published set reflects every config that could be applied.
func (m *Manager) applyConfigs(configs []*observabilityconfigv1alpha1.ObservabilityConfig) error {
	m.collectorsLock.Lock()

	// Retrieve current active collectors to rebuild the unused list. A failure here is
	// retriable and leaves the previously published set untouched.
	if err := m.retrieveDbCollectorsLocked(); err != nil {
		m.collectorsLock.Unlock()
		return err
	}

	var applyErrs []error
	newResolution := make(map[libovsdbops.SampleFeature]*featureResolution)
	for _, cr := range configs {
		if err := validateObservabilityConfig(cr); err != nil {
			// Non-convergent: skip and log, but don't fail the reconcile.
			klog.Warningf("Observability: skipping invalid ObservabilityConfig %s: %v", cr.Name, err)
			continue
		}
		conf := collectorConfigFromCR(cr)
		featureCollectors, err := m.addCollectorLocked(conf)
		if err != nil {
			// Transient: collect so the reconcile requeues.
			applyErrs = append(applyErrs, fmt.Errorf("ObservabilityConfig %s: %w", cr.Name, err))
			continue
		}
		var namespaces []string
		if cr.Spec.Filter != nil {
			namespaces = cr.Spec.Filter.Namespaces
		}
		addToResolution(newResolution, namespaces, featureCollectors)
	}
	// Publish the fully-built resolution atomically: readers never observe a partially-applied
	// state, and any ACL built from here on already picks up the new collectors.
	m.resolution = newResolution
	// Then re-point the ACLs that were built before the change. This runs before stale cleanup
	// because a collector cannot be deleted while a Sample still references it.
	_, syncErr := m.syncSamplingLocked()
	staleErr := m.deleteStaleCollectorsLocked()
	m.collectorsLock.Unlock()

	// Always schedule a verification pass: an ACL whose ops were built against the previous
	// config may not have been transacted yet, so this sync cannot have seen it.
	m.scheduleVerification(errors.Join(syncErr, staleErr))
	// syncErr is returned as well as handed to the retry timer: the timer is the backstop for
	// changes that land with no further event, while returning it lets the reconciler requeue
	// a transient OVSDB failure with its own backoff. Both paths are idempotent.
	// staleErr is deliberately not returned: a collector that cannot be deleted yet is expected
	// and is exactly what the retry timer is for.
	return errors.Join(append(applyErrs, syncErr)...)
}

// retrieveDbCollectorsLocked rebuilds the DB collector snapshot (dbCollectors,
// unusedCollectors, takenCollectorIDs) and marks every observability-owned collector as unused
// until the caller claims the ones that are still active.
//
// Every Sample_Collector row is listed, not just the observability-owned ones, because
// Sample_Collector.id is a table-wide schema index: allocating an id another component already
// holds would not merely fail, it would resolve to that row and overwrite it, since collectors
// are created by index lookup. So all ids are reserved in takenCollectorIDs, while only rows
// carrying the collectorFeaturesExternalID external ID are registered in dbCollectors and
// unusedCollectors, keeping foreign rows out of both reuse and cleanup.
//
// All three are rebuilt from scratch so that a failed retrieval leaves them consistently empty.
// Leaving unusedCollectors populated while dbCollectors is empty would make
// deleteStaleCollectorsLocked look up an empty UUID, issue a delete that matches nothing,
// take that for success and drop the entry, leaking the collector row for good.
//
// Caller must hold m.collectorsLock.
func (m *Manager) retrieveDbCollectorsLocked() error {
	clear(m.dbCollectors)
	clear(m.unusedCollectors)
	m.takenCollectorIDs.Clear()
	collectors, err := libovsdbops.ListSampleCollectors(m.nbClient)
	if err != nil {
		return fmt.Errorf("error getting sample collectors: %w", err)
	}
	for _, collector := range collectors {
		// Reserve the id whoever owns the row, so it is never handed out again.
		m.takenCollectorIDs.Insert(collector.ID)
		if _, owned := collector.ExternalIDs[collectorFeaturesExternalID]; !owned {
			continue
		}
		collectorKey := getCollectorKey(collector.SetID, collector.Probability)
		m.dbCollectors[collectorKey] = collector.UUID
		// all collectors are unused, until we update existing configs
		m.unusedCollectors[collectorKey] = collector.ID
	}
	return nil
}

// refreshCollectorsSnapshotLocked rebuilds the collector snapshot and then re-claims the
// collectors the published resolution still refers to, so that only genuinely unreferenced ones
// are left for deleteStaleCollectorsLocked.
//
// applyConfigs does the claiming as a side effect of walking the configs (addCollectorLocked
// un-marks each collector it reuses), but the retry path has no configs to walk: it must derive
// the active set from the resolution instead. Refreshing without that step would mark every
// collector unused and delete the ones currently in use.
//
// Caller must hold m.collectorsLock.
func (m *Manager) refreshCollectorsSnapshotLocked() error {
	if err := m.retrieveDbCollectorsLocked(); err != nil {
		return err
	}
	active := sets.New[string]()
	for _, fr := range m.resolution {
		active = active.Union(fr.cluster)
		for _, nsCollectors := range fr.perNS {
			active = active.Union(nsCollectors)
		}
	}
	for collectorKey, collectorUUID := range m.dbCollectors {
		if active.Has(collectorUUID) {
			delete(m.unusedCollectors, collectorKey)
		}
	}
	return nil
}

// currentGeneration returns the generation a retry pass should report its result against.
// It must be read before the pass starts: a resolution published while the pass is running
// makes its result say nothing about the state that is now current.
func (m *Manager) currentGeneration() uint64 {
	m.retryLock.Lock()
	defer m.retryLock.Unlock()
	return m.generation
}

// scheduleVerification advances the generation and arms a verification pass. It is what a
// resolution change calls, and the generation bump discards any retry still in flight for the
// resolution that was just replaced.
//
// A verification pass is armed unconditionally, even when the sync that just ran found nothing
// to do, which is what closes the race between the sync and the controllers that create sampled
// ACLs. A controller reads the sampling config while assembling its ops and transacts
// afterwards, so a write already in flight when the config changed can land after the sync has
// run, carrying the collectors the config had back when the ops were built — and an ACL that
// has not been transacted yet cannot be seen by that sync at all. Nothing else would notice:
// ovnkube-controller runs its informers with resyncInterval 0, and the retry framework only
// revisits objects that failed, so there is no periodic re-reconcile to repair it.
//
// It must be called without holding collectorsLock; retryLock and collectorsLock are never held
// together.
func (m *Manager) scheduleVerification(err error) {
	m.retryLock.Lock()
	defer m.retryLock.Unlock()
	m.generation++
	m.armRetryLocked(true, err)
}

// scheduleRetry records the outcome of the retry pass that ran for generation gen, and arms
// another only if that pass still changed something, so the loop stops as soon as a pass is a
// no-op. Since the sync compares desired against actual, that is a reliable signal that
// everything has converged.
//
// A result from a superseded generation is discarded. The pass releases collectorsLock before
// reporting, so a configuration change can land in between; acting on the older result would
// cancel the verification pass that change just armed, and leave an ACL written against the
// previous config permanently unsampled. Discarding is safe because the newer generation has
// already armed its own pass.
//
// It must be called without holding collectorsLock.
func (m *Manager) scheduleRetry(gen uint64, rearm bool, err error) {
	m.retryLock.Lock()
	defer m.retryLock.Unlock()
	if gen != m.generation {
		klog.V(5).Infof("Observability: discarding sampling sync result from generation %d, current is %d",
			gen, m.generation)
		return
	}
	m.armRetryLocked(rearm, err)
}

// armRetryLocked (re)arms at most one retry timer. Caller must hold retryLock.
func (m *Manager) armRetryLocked(rearm bool, err error) {
	// Only one pending retry timer at a time.
	if m.retryTimer != nil {
		m.retryTimer.Stop()
		m.retryTimer = nil
	}
	if err == nil && !rearm {
		if m.retries > 0 {
			klog.Infof("Observability: sampling sync converged after %d retries", m.retries)
		}
		m.retries = 0
		return
	}
	m.retries++
	// allow retries for ~1 hour, hopefully enough for all controllers to settle
	if m.retries > maxSyncRetries {
		m.retries = 0
		if err != nil {
			klog.Errorf("Observability: giving up on sampling sync after %d retries, last error: %v", maxSyncRetries, err)
		} else {
			klog.Errorf("Observability: sampling sync still not converged after %d retries, giving up; "+
				"some ACLs may reference stale collectors until their object is reconciled again", maxSyncRetries)
		}
		return
	}
	if m.stopped {
		return
	}
	m.retryTimer = time.AfterFunc(m.retryInterval, m.retrySync)
}

// retrySync runs one sampling sync plus stale-collector cleanup pass and reschedules based on
// its outcome. It is invoked from the retry timer.
//
// The snapshot is refreshed first: the pass that armed this retry may have failed to retrieve
// it, and it is the only chance to pick that up again. It also keeps cleanup working from what
// the DB holds now rather than from what it held when the config was last applied.
func (m *Manager) retrySync() {
	// Captured before the work starts, so that a resolution published while this pass runs
	// makes the result stale rather than silently overwriting the newer pass's bookkeeping.
	gen := m.currentGeneration()
	m.collectorsLock.Lock()
	changed, syncErr := m.syncSamplingLocked()
	staleErr := m.refreshCollectorsSnapshotLocked()
	if staleErr == nil {
		staleErr = m.deleteStaleCollectorsLocked()
	}
	m.collectorsLock.Unlock()
	m.scheduleRetry(gen, changed, errors.Join(syncErr, staleErr))
}

// stopRetryTimer cancels any pending retry and prevents future ones.
func (m *Manager) stopRetryTimer() {
	m.retryLock.Lock()
	defer m.retryLock.Unlock()
	m.stopped = true
	if m.retryTimer != nil {
		m.retryTimer.Stop()
		m.retryTimer = nil
	}
}

// syncSamplingLocked re-applies the Sample references of every sampled ACL in the DB so they
// match the currently published resolution, and reports whether it had to change anything.
//
// This is the whole mechanism by which an ObservabilityConfig change reaches ACLs that already
// exist. It deliberately touches nothing but the SampleNew/SampleEst columns: the controllers
// that own those ACLs are not involved, no Kubernetes object is re-reconciled, and no other NB
// state (port groups, address sets, the ACL's match or action) is rebuilt.
//
// The ACL's own external IDs say which feature and namespace it belongs to
// (GetACLSampleFeature / GetACLSampleNamespace), so every sampled ACL is covered by
// construction, whichever controller created it and whether or not it corresponds to a
// Kubernetes object at all (node management-port ACLs and UDN open-port ACLs, for instance).
//
// Cost: this is a full pass over the ACL table, so it is deliberately driven only by
// ObservabilityConfig changes (a rare operator action) and the bounded retries that follow one,
// never by pod, policy or node events. The listing and the comparison against the current state
// are served from the libovsdb cache and are linear in the number of ACLs, with a map lookup per
// Sample; only ACLs that actually differ make it into the transaction, so a converged pass does
// no OVSDB round-trip at all.
//
// Caller must hold m.collectorsLock for writing, which means the lock is held across the
// transaction on the passes that do change something. Should that block ACL creation for too
// long on very large clusters, the fix is to snapshot the resolution (it is replaced wholesale,
// never mutated) and run the transaction outside collectorsLock under a dedicated sync lock.
func (m *Manager) syncSamplingLocked() (bool, error) {
	acls, err := libovsdbops.FindACLsWithPredicate(m.nbClient, m.isSampledACL)
	if err != nil {
		return false, fmt.Errorf("error listing sampled ACLs: %w", err)
	}
	samples, err := libovsdbops.ListSamples(m.nbClient)
	if err != nil {
		return false, fmt.Errorf("error listing samples: %w", err)
	}
	samplesByUUID := make(map[string]*nbdb.Sample, len(samples))
	for _, sample := range samples {
		samplesByUUID[sample.UUID] = sample
	}

	var updates []libovsdbops.ACLSampling
	for _, acl := range acls {
		feature := libovsdbops.GetACLSampleFeature(acl)
		collectors := m.collectorsForLocked(libovsdbops.GetACLSampleNamespace(acl), feature)
		if aclSamplingUpToDate(acl, collectors, samplesByUUID) {
			continue
		}
		updates = append(updates, libovsdbops.ACLSampling{ACL: acl, Collectors: collectors})
	}
	if len(updates) == 0 {
		return false, nil
	}

	ops, err := libovsdbops.UpdateACLsSamplingOps(m.nbClient, nil, updates...)
	if err != nil {
		return true, fmt.Errorf("error building sampling update ops for %d ACLs: %w", len(updates), err)
	}
	if _, err := libovsdbops.TransactAndCheck(m.nbClient, ops); err != nil {
		return true, fmt.Errorf("error updating sampling on %d ACLs: %w", len(updates), err)
	}
	klog.V(5).Infof("Observability: updated sampling on %d ACLs", len(updates))
	return true, nil
}

// isSampledACL reports whether an ACL belongs to a sampled feature and is owned by the
// controller this Manager governs. EgressFirewall and ANP/BANP stamp the default network
// controller on their ACLs as well, including for UDN namespaces, so those are covered.
func (m *Manager) isSampledACL(acl *nbdb.ACL) bool {
	if acl.ExternalIDs[libovsdbops.OwnerControllerKey.String()] != m.controllerName {
		return false
	}
	return libovsdbops.GetACLSampleFeature(acl) != ""
}

// aclSamplingUpToDate reports whether the ACL already references exactly the collectors it
// should, which is what lets syncSamplingLocked tell a converged pass from one that changed
// something. An empty collectors slice means the ACL must not be sampled at all.
func aclSamplingUpToDate(acl *nbdb.ACL, collectors []string, samplesByUUID map[string]*nbdb.Sample) bool {
	if len(collectors) == 0 {
		return acl.SampleNew == nil && acl.SampleEst == nil
	}
	if acl.SampleNew == nil || acl.SampleEst == nil || *acl.SampleNew != *acl.SampleEst {
		return false
	}
	sample, found := samplesByUUID[*acl.SampleNew]
	if !found {
		return false
	}
	// The sample ID is derived from the ACL's identity, match and action, so a Match or Action
	// change must produce a new Sample even when the collectors are unchanged.
	if sample.Metadata != int(libovsdbops.GetACLSampleID(acl)) {
		return false
	}
	// Collectors is an OVSDB set, so the DB order carries no meaning.
	return sets.New(sample.Collectors...).Equal(sets.New(collectors...))
}

// deleteStaleCollectorsLocked deletes every collector currently marked unused, continuing
// past individual failures and returning the last error. Caller must hold m.collectorsLock.
func (m *Manager) deleteStaleCollectorsLocked() error {
	var lastErr error
	for collectorKey, collectorSetID := range m.unusedCollectors {
		collectorUUID := m.dbCollectors[collectorKey]
		err := libovsdbops.DeleteSampleCollector(m.nbClient, &nbdb.SampleCollector{
			UUID: collectorUUID,
		})
		if err != nil {
			lastErr = err
			klog.Infof("Error deleting collector with ID=%d: %v", collectorSetID, lastErr)
			continue
		}
		delete(m.unusedCollectors, collectorKey)
		delete(m.dbCollectors, collectorKey)
		delete(m.takenCollectorIDs, collectorSetID)
	}
	return lastErr
}

// Cleanup must be called when observability is no longer needed.
// It will return an error if some samples still exist in the db.
// This is expected, and Cleanup may be retried on the next restart.
func Cleanup(nbClient libovsdbclient.Client) error {
	// Do the opposite of init
	err := libovsdbops.DeleteSamplingAppsWithPredicate(nbClient, func(_ *nbdb.SamplingApp) bool {
		return true
	})
	if err != nil {
		return fmt.Errorf("error deleting sampling apps: %w", err)
	}

	err = libovsdbops.DeleteSampleCollectorWithPredicate(nbClient, func(_ *nbdb.SampleCollector) bool {
		return true
	})
	if err != nil {
		return fmt.Errorf("error deleting sample collectors: %w", err)
	}
	return nil
}

func (m *Manager) setSamplingAppIDs() error {
	var ops []ovsdb.Operation
	var err error
	for _, appConfig := range []struct {
		id      int
		appType nbdb.SamplingAppType
	}{
		{
			id:      DropSamplingID,
			appType: nbdb.SamplingAppTypeDrop,
		},
		{
			id:      ACLNewTrafficSamplingID,
			appType: nbdb.SamplingAppTypeACLNew,
		},
		{
			id:      ACLEstTrafficSamplingID,
			appType: nbdb.SamplingAppTypeACLEst,
		},
	} {
		samplingApp := &nbdb.SamplingApp{
			ID:   appConfig.id,
			Type: appConfig.appType,
		}
		ops, err = libovsdbops.CreateOrUpdateSamplingAppsOps(m.nbClient, ops, samplingApp)
		if err != nil {
			return fmt.Errorf("error creating or updating sampling app %s: %w", appConfig.appType, err)
		}
	}
	_, err = libovsdbops.TransactAndCheck(m.nbClient, ops)
	return err
}

func groupByProbability(c *collectorConfig) map[int][]libovsdbops.SampleFeature {
	probabilities := make(map[int][]libovsdbops.SampleFeature)
	for feature, percentProbability := range c.featuresProbability {
		probability := percentToProbability(percentProbability)
		probabilities[probability] = append(probabilities[probability], feature)
	}
	return probabilities
}

func getCollectorKey(collectorID int, probability int) string {
	return fmt.Sprintf("%d-%d", collectorID, probability)
}

func (m *Manager) getFreeCollectorID() (int, error) {
	for i := 1; i <= maxCollectorID; i++ {
		if !m.takenCollectorIDs.Has(i) {
			return i, nil
		}
	}
	return 0, fmt.Errorf("no free collector IDs")
}

// addCollectorLocked ensures collectors exist in the DB for the config and returns feature->collector UUIDs.
// Caller must hold m.collectorsLock.
func (m *Manager) addCollectorLocked(conf *collectorConfig) (map[libovsdbops.SampleFeature][]string, error) {
	sampleFeaturesConfig := make(map[libovsdbops.SampleFeature][]string)
	probabilityConfig := groupByProbability(conf)

	for probability, features := range probabilityConfig {
		collectorKey := getCollectorKey(conf.collectorSetID, probability)
		var collectorUUID string
		var ok bool
		// ensure predictable externalID
		slices.Sort(features)
		collectorFeatures := strings.Join(features, ",")
		if collectorUUID, ok = m.dbCollectors[collectorKey]; !ok {
			collectorID, err := m.getFreeCollectorID()
			if err != nil {
				return sampleFeaturesConfig, err
			}
			collector := &nbdb.SampleCollector{
				ID:          collectorID,
				SetID:       conf.collectorSetID,
				Probability: probability,
				ExternalIDs: map[string]string{
					collectorFeaturesExternalID: collectorFeatures,
				},
			}
			err = libovsdbops.CreateOrUpdateSampleCollector(m.nbClient, collector)
			if err != nil {
				return sampleFeaturesConfig, err
			}
			collectorUUID = collector.UUID
			m.dbCollectors[collectorKey] = collectorUUID
			m.takenCollectorIDs.Insert(collectorID)
		} else {
			// The collector already exists, so it is used regardless of what follows: mark it
			// used up front. Otherwise a later failure (e.g. the metadata update below) would
			// leave it flagged unused and the stale-cleanup sweep at the end of applyConfigs
			// would delete a collector we still want (only to recreate it on the next retry).
			delete(m.unusedCollectors, collectorKey)
			// update collector's features
			collector := &nbdb.SampleCollector{
				UUID: collectorUUID,
				ExternalIDs: map[string]string{
					collectorFeaturesExternalID: collectorFeatures,
				},
			}
			if err := libovsdbops.UpdateSampleCollectorExternalIDs(m.nbClient, collector); err != nil {
				return sampleFeaturesConfig, err
			}
		}
		for _, feature := range features {
			sampleFeaturesConfig[feature] = append(sampleFeaturesConfig[feature], collectorUUID)
		}
	}
	return sampleFeaturesConfig, nil
}

func percentToProbability(percent int) int {
	return 65535 * percent / 100
}
