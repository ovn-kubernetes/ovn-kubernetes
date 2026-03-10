package observability

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	observabilityconfigv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// OVN SamplingApp IDs; must be < 255. Add new apps at the end.
const (
	DropSamplingID = iota + 1
	ACLNewTrafficSamplingID
	ACLEstTrafficSamplingID
)

// maxCollectorID is the OVN Sample_Collector row id limit (table column id).
const maxCollectorID = 255

// DefaultObservabilityCollectorSetID is the default collector set ID used when no ObservabilityConfig is applied.
// Used by observability-lib and tests.
const DefaultObservabilityCollectorSetID = 1

const collectorFeaturesExternalID = "sample-features"

// collectorConfig holds the configuration for a collector.
// It is allowed to set different probabilities for every feature.
// collectorSetID is used to set up sampling via OVSDB.
type collectorConfig struct {
	collectorSetID int
	// probability in percent, 0 to 100
	featuresProbability map[libovsdbops.SampleFeature]int
}

// applicableConfigEntry holds one ObservabilityConfig that applies to this node and its
// resolved feature->collector UUIDs. namespaces is from Filter.Namespaces (nil or empty = cluster-scoped).
type applicableConfigEntry struct {
	namespaces        []string // nil or empty = applies to all namespaces
	featureCollectors map[libovsdbops.SampleFeature][]string
}

type Manager struct {
	nbClient          libovsdbclient.Client
	sampConfig        *libovsdbops.SamplingConfig // cluster-wide default for backward compat
	applicableConfigs []applicableConfigEntry     // all configs that apply to this node, for context resolution
	collectorsLock    sync.RWMutex
	// nbdb Collectors have probability. To allow different probabilities for different features,
	// multiple nbdb Collectors will be created, one per probability.
	// getCollectorKey() => collector.UUID
	dbCollectors map[string]string
	// cleaning up unused collectors may take time and multiple retries, as all referencing samples must be removed first.
	// Therefore, we need to save state between those retries.
	// getCollectorKey() => collector.SetID
	unusedCollectors              map[string]int
	unusedCollectorsRetryInterval time.Duration
	collectorsCleanupRetries      int
	// Only maxCollectorID collectors are allowed, each should have unique ID.
	// this set is tracking already assigned IDs.
	takenCollectorIDs sets.Set[int]
}

func NewManager(nbClient libovsdbclient.Client) *Manager {
	return &Manager{
		nbClient:                      nbClient,
		collectorsLock:                sync.RWMutex{},
		dbCollectors:                  make(map[string]string),
		unusedCollectors:              make(map[string]int),
		unusedCollectorsRetryInterval: time.Minute,
		takenCollectorIDs:             sets.New[int](),
	}
}

// SamplingConfig returns the cluster-wide default sampling config (from configs with no
// Filter.Namespaces). Prefer SamplingConfigForContext when creating ACLs so namespace-scoped
// configs are applied correctly.
func (m *Manager) SamplingConfig() *libovsdbops.SamplingConfig {
	m.collectorsLock.RLock()
	defer m.collectorsLock.RUnlock()
	return m.sampConfig
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
	fc := m.resolveForContextLocked(namespace, feature)
	if fc == nil {
		return nil
	}
	return libovsdbops.NewSamplingConfig(fc)
}

// resolveForContextLocked returns a featureCollectors map that merges all configs
// that apply to (namespace, feature). Both namespace-scoped configs (Filter.Namespaces
// containing namespace) and cluster-scoped configs (no Filter.Namespaces) apply;
// the ACL's Sample will reference all matching collectors so each collector receives
// samples at its configured probability. Caller must hold m.collectorsLock (at least RLock).
func (m *Manager) resolveForContextLocked(namespace string, feature libovsdbops.SampleFeature) map[libovsdbops.SampleFeature][]string {
	var merged []string
	for _, e := range m.applicableConfigs {
		collectors := e.featureCollectors[feature]
		if len(collectors) == 0 {
			continue
		}
		applies := false
		if len(e.namespaces) == 0 {
			applies = true
		} else {
			for _, ns := range e.namespaces {
				if ns == namespace {
					applies = true
					break
				}
			}
		}
		if applies {
			merged = append(merged, collectors...)
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return map[libovsdbops.SampleFeature][]string{feature: merged}
}

// Init sets up sampling app IDs and loads existing collector state from the DB.
// It does not apply any ObservabilityConfig; config is applied when StartWatching
// receives an ObservabilityConfig that applies to this node.
func (m *Manager) Init() error {
	if err := m.setSamplingAppIDs(); err != nil {
		return err
	}
	return m.setDbCollectors()
}

// ObservabilityConfigInformer is the minimal interface needed to watch ObservabilityConfig CRs.
// Implemented by the generated informer from the observabilityconfig clientset.
type ObservabilityConfigInformer interface {
	Informer() cache.SharedIndexInformer
}

// NodeLister lists nodes; used to resolve the local zone node in interconnect mode.
// Implemented by factory.WatchFactory.
type NodeLister interface {
	GetNodes() ([]*corev1.Node, error)
}

// StartWatching watches ObservabilityConfig CRs and applies all that apply to this node.
// zone is this controller's zone (e.g. config.Default.Zone). In central mode (zone "" or types.OvnDefaultZone),
// node selectors are not honored and only configs with no Filter.NodeSelector apply. In interconnect mode,
// the local zone node is resolved from nodeLister and configs whose Filter.NodeSelector matches its labels apply.
// Multiple configs can apply (e.g. one cluster-wide, one namespace-scoped); use SamplingConfigForContext
// when creating ACLs so the correct config is chosen per (namespace, feature). Call after Init().
func (m *Manager) StartWatching(informer ObservabilityConfigInformer, nodeLister NodeLister, zone string, _ <-chan struct{}) {
	if informer == nil {
		return
	}
	applyFromStore := func() {
		store := informer.Informer().GetStore()
		objs := store.List()
		nodeLabelsMap := nodeLabelsForZone(nodeLister, zone)
		configs := allApplicableConfigs(objs, nodeLabelsMap)
		if len(configs) == 0 {
			m.clearConfig()
			return
		}
		if err := m.applyConfigs(configs); err != nil {
			klog.Errorf("Observability: failed to apply configs: %v", err)
		}
	}
	_, err := informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ interface{}) { applyFromStore() },
		UpdateFunc: func(_, _ interface{}) { applyFromStore() },
		DeleteFunc: func(_ interface{}) { applyFromStore() },
	})
	if err != nil {
		klog.Errorf("Observability: failed to add ObservabilityConfig event handler: %v", err)
		return
	}
	applyFromStore()
}

// allApplicableConfigs returns all ObservabilityConfigs that apply to this node, ordered
// for resolution: "default" first, then by name. Node labels nil means only configs
// with no NodeSelector apply (cluster-wide).
func allApplicableConfigs(objs []interface{}, nodeLabels map[string]string) []*observabilityconfigv1alpha1.ObservabilityConfig {
	var candidates []*observabilityconfigv1alpha1.ObservabilityConfig
	for _, obj := range objs {
		cfg, ok := obj.(*observabilityconfigv1alpha1.ObservabilityConfig)
		if !ok {
			continue
		}
		if configAppliesToNode(cfg, nodeLabels) {
			candidates = append(candidates, cfg)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	// Prefer "default" first, then stable order
	slices.SortFunc(candidates, func(a, b *observabilityconfigv1alpha1.ObservabilityConfig) int {
		if a.Name == "default" && b.Name != "default" {
			return -1
		}
		if a.Name != "default" && b.Name == "default" {
			return 1
		}
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return candidates
}

// nodeLabelsForZone returns the labels of the local zone node for filter matching.
// Central mode (zone "" or OvnDefaultZone): returns nil so only configs without NodeSelector apply.
// Interconnect mode: looks up the node in zone via nodeLister (same pattern as GetLocalZoneNodes) and returns its labels.
func nodeLabelsForZone(nodeLister NodeLister, zone string) map[string]string {
	if zone == "" || zone == types.OvnDefaultZone {
		return nil
	}
	if nodeLister == nil {
		return nil
	}
	nodes, err := nodeLister.GetNodes()
	if err != nil {
		return nil
	}
	for _, node := range nodes {
		if util.GetNodeZone(node) == zone {
			return node.Labels
		}
	}
	return nil
}

// configAppliesToNode returns true if the ObservabilityConfig applies to this node.
// When nodeLabels is nil (central mode), only configs with no Filter.NodeSelector apply.
// NodeSelector evaluation matches the pattern used in the Admin Network Policy controller
// (pkg/ovn/controller/admin_network_policy/admin_network_policy_node.go setNodeForANP):
// selector.Matches(labels.Set(node.Labels)).
func configAppliesToNode(cfg *observabilityconfigv1alpha1.ObservabilityConfig, nodeLabels map[string]string) bool {
	if cfg.Spec.Filter == nil || cfg.Spec.Filter.NodeSelector == nil {
		return true
	}
	if nodeLabels == nil {
		return false
	}
	selector, err := metav1.LabelSelectorAsSelector(cfg.Spec.Filter.NodeSelector)
	if err != nil {
		klog.Warningf("Observability: invalid NodeSelector on ObservabilityConfig %s: %v", cfg.Name, err)
		return false
	}
	return selector.Matches(labels.Set(nodeLabels))
}

// clearConfig clears the active sampling config and applicable configs, and triggers cleanup of collectors.
// SamplingConfig() and SamplingConfigForContext() will return nil until applicable configs are applied.
func (m *Manager) clearConfig() {
	m.collectorsLock.Lock()
	m.sampConfig = nil
	m.applicableConfigs = nil
	m.collectorsLock.Unlock()
	m.deleteStaleCollectorsWithRetry()
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
	if cr.Spec.CollectorID < 1 {
		return fmt.Errorf("ObservabilityConfig %s: collectorID (set_id) must be at least 1, got %d", cr.Name, cr.Spec.CollectorID)
	}
	for _, f := range cr.Spec.Features {
		if f.Probability < 0 || f.Probability > 100 {
			return fmt.Errorf("ObservabilityConfig %s: feature %s probability must be 0..100, got %d", cr.Name, f.Feature, f.Probability)
		}
	}
	// Namespace filter only applies to namespaced features (NetworkPolicy, EgressFirewall). Reject if any feature is cluster-scoped.
	if cr.Spec.Filter != nil && cr.Spec.Filter.Namespaces != nil && len(*cr.Spec.Filter.Namespaces) > 0 {
		for _, f := range cr.Spec.Features {
			if _, ok := namespacedObservabilityFeatures[f.Feature]; !ok {
				return fmt.Errorf("ObservabilityConfig %s: Filter.Namespaces can only be used with namespaced features (NetworkPolicy, EgressFirewall); feature %s is cluster-scoped", cr.Name, f.Feature)
			}
		}
	}
	return nil
}

// applyConfigs applies all given ObservabilityConfigs: ensures collectors exist for each,
// stores them for context resolution, and sets the default cluster-wide sampConfig.
func (m *Manager) applyConfigs(configs []*observabilityconfigv1alpha1.ObservabilityConfig) error {
	for _, cr := range configs {
		if err := validateObservabilityConfig(cr); err != nil {
			return err
		}
	}
	if err := m.setSamplingAppIDs(); err != nil {
		return err
	}
	if err := m.setDbCollectors(); err != nil {
		return err
	}

	m.collectorsLock.Lock()
	m.applicableConfigs = make([]applicableConfigEntry, 0, len(configs))
	mergedClusterScoped := make(map[libovsdbops.SampleFeature][]string)

	for _, cr := range configs {
		conf := collectorConfigFromCR(cr)
		featureCollectors, err := m.addCollectorLocked(conf)
		if err != nil {
			m.collectorsLock.Unlock()
			return err
		}
		var namespaces []string
		if cr.Spec.Filter != nil && cr.Spec.Filter.Namespaces != nil {
			namespaces = *cr.Spec.Filter.Namespaces
		}
		m.applicableConfigs = append(m.applicableConfigs, applicableConfigEntry{
			namespaces:        namespaces,
			featureCollectors: featureCollectors,
		})
		if len(namespaces) == 0 {
			for f, uuids := range featureCollectors {
				mergedClusterScoped[f] = append(mergedClusterScoped[f], uuids...)
			}
		}
	}

	if len(mergedClusterScoped) > 0 {
		m.sampConfig = libovsdbops.NewSamplingConfig(mergedClusterScoped)
	} else {
		m.sampConfig = nil
	}
	m.collectorsLock.Unlock() // release before deleteStaleCollectorsWithRetry (it needs the lock)
	m.deleteStaleCollectorsWithRetry()
	return nil
}

func (m *Manager) setDbCollectors() error {
	m.collectorsLock.Lock()
	defer m.collectorsLock.Unlock()
	clear(m.dbCollectors)
	collectors, err := libovsdbops.ListSampleCollectors(m.nbClient)
	if err != nil {
		return fmt.Errorf("error getting sample collectors: %w", err)
	}
	for _, collector := range collectors {
		collectorKey := getCollectorKey(collector.SetID, collector.Probability)
		m.dbCollectors[collectorKey] = collector.UUID
		m.takenCollectorIDs.Insert(collector.ID)
		// all collectors are unused, until we update existing configs
		m.unusedCollectors[collectorKey] = collector.ID
	}
	return nil
}

// Stale collectors can't be deleted until all referencing Samples are deleted.
// Samples will be deleted asynchronously by different controllers on their init with the new Manager.
// deleteStaleCollectorsWithRetry will retry, considering deletion should eventually succeed when all controllers
// update their db entries to use the latest observability config.
func (m *Manager) deleteStaleCollectorsWithRetry() {
	if err := m.deleteStaleCollectors(); err != nil {
		m.collectorsCleanupRetries += 1
		// allow retries for 1 hour, hopefully it will be enough for all handler to complete initial sync
		if m.collectorsCleanupRetries > 60 {
			m.collectorsCleanupRetries = 0
			klog.Errorf("Cleanup stale collectors failed after 30 retries: %v", err)
			return
		}
		time.AfterFunc(m.unusedCollectorsRetryInterval, m.deleteStaleCollectorsWithRetry)
		return
	}
	m.collectorsCleanupRetries = 0
	klog.Infof("Cleanup stale collectors succeeded.")
}

func (m *Manager) deleteStaleCollectors() error {
	m.collectorsLock.Lock()
	defer m.collectorsLock.Unlock()
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
			// update collector's features
			collector := &nbdb.SampleCollector{
				UUID: collectorUUID,
				ExternalIDs: map[string]string{
					collectorFeaturesExternalID: collectorFeatures,
				},
			}
			err := libovsdbops.UpdateSampleCollectorExternalIDs(m.nbClient, collector)
			if err != nil {
				return sampleFeaturesConfig, err
			}
			// collector is used, remove from unused Collectors
			delete(m.unusedCollectors, collectorKey)
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
