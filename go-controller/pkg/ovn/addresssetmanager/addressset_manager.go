// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package addresssetmanager

import (
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

// podSelectorAddressSet stores address set for modifications and used selectors that define this address set.
type podSelectorAddressSet struct {
	// backRefs is a map of objects that use this address set.
	// keys must be unique for all possible users, e.g. for NetworkPolicy use (np *networkPolicy) getKeyWithKind().
	backRefs map[string]bool

	podSelector       labels.Selector
	namespaceSelector labels.Selector
	// namespace is used when namespaceSelector is nil to set static namespace
	namespace string
	// nodeSelector decides which nodes' pods should be added to the address set; nil means all nodes
	nodeSelector labels.Selector

	addressSet addressset.AddressSet

	// selectedNamespaces is a cache for namespaces that were selected by this address set during the last reconciliation
	// used to optimize events processing.
	selectedNamespaces *selectedNamespaces
	// selectedNodes is a cache for nodes that were selected by this address set during the last reconciliation
	// used to optimize node event processing. Only set when nodeSelector is non-nil.
	selectedNodes sets.Set[string]

	// network-specific fields
	controllerName string
	netInfo        util.NetInfo

	// legacyNetpolMode makes nil and empty PodSelectors behave differently (it shouldn't be the case,
	// but this is a legacy behaviour that customers rely on).
	// when set to true hostNetwork pods aren't selected,
	// and config.Kubernetes.HostNetworkNamespace address set IPs will be included when that namespace is matched and
	// podSelector is empty.
	legacyNetpolMode bool

	// addresses is an authoritative in-memory mirror of the OVN address-set contents.
	// Incremental reconciles compute add/remove against this cache and skip OVN writes
	// when the delta is empty, avoiding a full pod list and OVN getAddresses on every event.
	addresses sets.Set[string]
	// podAddresses maps "namespace/name" to the IPs that pod currently contributes.
	// Used to handle IP reuse across pods: an IP is only removed when no remaining selected
	// pod still contributes it.
	podAddresses map[string][]string
	// selectedPods maps namespace name to the set of pod names currently selected into this
	// address set. Used to diff membership on namespace/node-scoped reconciles.
	selectedPods map[string]sets.Set[string]
	// podNodes maps "namespace/name" to the Spec.NodeName last recorded for that selected pod.
	// Used with podsByNode for O(pods-on-node) membership updates on node-selector changes.
	podNodes map[string]string
	// podsByNode maps Spec.NodeName to the set of selected pod keys last known on that node.
	podsByNode map[string]sets.Set[string]
	// pendingUpdates batches the triggering event context between enqueue (reconcilePod /
	// reconcileNamespace / reconcileNode) and reconcileAddressSet. Cleared at the start of
	// each reconcile under the per-key lock.
	pendingUpdates addrSetPendingUpdates
}

// addrSetPendingUpdates records which objects triggered a reconcile so reconcileAddressSet
// can take an incremental path instead of always rebuilding the full desired address set.
// Multiple pod-scoped classes (namespace, node-selector, pod) may be coalesced in one
// snapshot; reconcileAddressSetLocked falls back to full sync when more than one is present.
type addrSetPendingUpdates struct {
	// podKeys are "namespace/name" keys for pods that need incremental membership/IP checks.
	podKeys sets.Set[string]
	// namespaceKeys are namespaces whose membership may have changed due to label updates.
	namespaceKeys sets.Set[string]
	// nodeKeys are nodes that may affect nodeSelector membership or host-network IPs.
	nodeKeys sets.Set[string]
	// fullSync forces a full desired-state rebuild (first ensure, host-network namespace create, etc.).
	fullSync bool
}

func (u *addrSetPendingUpdates) addPodKey(podKey string) {
	if u.podKeys == nil {
		u.podKeys = sets.New[string]()
	}
	u.podKeys.Insert(podKey)
}

func (u *addrSetPendingUpdates) addNamespaceKey(nsKey string) {
	if u.namespaceKeys == nil {
		u.namespaceKeys = sets.New[string]()
	}
	u.namespaceKeys.Insert(nsKey)
}

func (u *addrSetPendingUpdates) addNodeKey(nodeKey string) {
	if u.nodeKeys == nil {
		u.nodeKeys = sets.New[string]()
	}
	u.nodeKeys.Insert(nodeKey)
}

// snapshot clones pending state for this reconcile. pendingUpdates itself is left
// intact until a successful reconcile clears it, so a failed attempt keeps the
// triggering keys for retry (and for coalescing with later events).
func (u *addrSetPendingUpdates) snapshot() addrSetPendingUpdates {
	return addrSetPendingUpdates{
		podKeys:       u.podKeys.Clone(),
		namespaceKeys: u.namespaceKeys.Clone(),
		nodeKeys:      u.nodeKeys.Clone(),
		fullSync:      u.fullSync,
	}
}

func (u *addrSetPendingUpdates) clear() {
	u.podKeys = nil
	u.namespaceKeys = nil
	u.nodeKeys = nil
	u.fullSync = false
}

// addressSetPodCacheSnapshot is a point-in-time copy of pod membership mirrors used to
// replace caches after OVN apply without re-listing Kubernetes objects.
type addressSetPodCacheSnapshot struct {
	selectedPods map[string]sets.Set[string]
	podAddresses map[string][]string
	podNodes     map[string]string
}

// podCacheEntry records the IPs and node a pod should contribute after a successful OVN apply.
type podCacheEntry struct {
	ips      []string
	nodeName string
}

// hostNetworkSnapshot is a compute-time snapshot of host-network IPs for this address set
// (from hostNetworkNamespaceIPsPerNode) plus whether the set currently selects the host-
// network namespace. Used for OVN desired-state union; not committed as per-AS cache state.
type hostNetworkSnapshot struct {
	selecting bool
	addresses sets.Set[string]
}

// addressSetCachePlan is the exact pod-cache mutation corresponding to a computed OVN delta.
// It is built during the compute phase and applied after OVN succeeds without re-listing.
type addressSetCachePlan struct {
	podCacheReplace *addressSetPodCacheSnapshot
	podRemoves      sets.Set[string]
	podSets         map[string]podCacheEntry
}

func newAddressSetCachePlan() *addressSetCachePlan {
	return &addressSetCachePlan{}
}

func (p *addressSetCachePlan) recordPodRemove(podKey string) {
	// Full-replace plans already encode membership; incremental records must not mix in.
	if p.podCacheReplace != nil {
		return
	}
	if p.podRemoves == nil {
		p.podRemoves = sets.New[string]()
	}
	p.podRemoves.Insert(podKey)
	delete(p.podSets, podKey)
}

func (p *addressSetCachePlan) recordPodSet(podKey string, ips []string, nodeName string) {
	// Full-replace plans already encode membership; incremental records must not mix in.
	if p.podCacheReplace != nil {
		return
	}
	if p.podSets == nil {
		p.podSets = make(map[string]podCacheEntry)
	}
	p.podRemoves.Delete(podKey)
	p.podSets[podKey] = podCacheEntry{ips: slices.Clone(ips), nodeName: nodeName}
}

const (
	ClusterNodeIPsAddrSetName                = "node-ips"
	ClusterNodeIPsEgressIPBackRef            = "egressip"
	ClusterNodeIPsEgressServiceBackRef       = "egressservice"
	ClusterNodeIPsRouteAdvertisementsBackRef = "route-advertisements"

	// podNodeNameIndex indexes pods by Spec.NodeName for node-scoped membership lookups.
	podNodeNameIndex = "spec.nodeName"
)

func podByNodeNameIndexFunc(obj interface{}) ([]string, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Spec.NodeName == "" {
		return []string{}, nil
	}
	return []string{pod.Spec.NodeName}, nil
}

// podNodeNameIndexer is the subset of SharedIndexInformer needed to install podNodeNameIndex.
type podNodeNameIndexer interface {
	GetIndexer() cache.Indexer
	AddIndexers(cache.Indexers) error
}

// ensurePodNodeNameIndexer installs podNodeNameIndex when absent. Concurrent constructors may
// race on AddIndexers; only fail if the index is still missing afterwards.
func ensurePodNodeNameIndexer(informer podNodeNameIndexer) error {
	if _, exists := informer.GetIndexer().GetIndexers()[podNodeNameIndex]; exists {
		return nil
	}
	if err := informer.AddIndexers(cache.Indexers{podNodeNameIndex: podByNodeNameIndexFunc}); err != nil {
		if _, exists := informer.GetIndexer().GetIndexers()[podNodeNameIndex]; !exists {
			return fmt.Errorf("failed to add %q indexer on pod informer: %w", podNodeNameIndex, err)
		}
	}
	return nil
}

// AddressSetManager manages shared address sets with pod IPs based on provided pod and namespace selectors.
// It shared across network controllers.
type AddressSetManager struct {
	name     string
	nbClient libovsdbclient.Client

	// address set factory ip modes only affect which IPs are getting selected for the operations
	// different networks may have different setups, so we need all combinations
	addressSetFactoryV4        addressset.AddressSetFactory
	addressSetFactoryV6        addressset.AddressSetFactory
	addressSetFactoryDualstack addressset.AddressSetFactory

	// addressSets stores all currently used address sets.
	addressSets *syncmap.SyncMap[*podSelectorAddressSet]

	podLister listers.PodLister
	// podIndexer is the shared pod informer indexer; includes podNodeNameIndex for
	// O(pods-on-node) lookups instead of cluster-wide list+filter.
	podIndexer      cache.Indexer
	namespaceLister listers.NamespaceLister
	nodeLister      listers.NodeLister

	podController        controller.Controller
	nsController         controller.Controller
	nodeController       controller.Controller
	addressSetReconciler controller.Reconciler

	// All network controllers are getting this function from the same networkmanager, so we can share it
	getNetworkNameForNADKey func(nadKey string) string

	// hostNetworkNamespaceExists, hostNetworkNamespaceIPsPerNode and hostNetworkSelectingAddrSets are protected by the same lock.
	// can only be taken after the addressSets key lock and never vice versa to avoid deadlocks.
	hostNetworkNamespaceLock   sync.RWMutex
	hostNetworkNamespaceExists bool
	// local cache of HostNetworkNamespace address set IPs
	hostNetworkNamespaceIPsPerNode map[string][]string
	// local cache of address sets that select HostNetworkNamespace. Membership is
	// registered when the host-network cache plan is built so node IP updates during
	// an in-flight OVN apply still enqueue the address set for reconcile.
	hostNetworkSelectingAddrSets sets.Set[string]

	clusterNodeIPsLock     sync.RWMutex
	clusterNodeIPsBackRefs sets.Set[string]
}

func NewAddressSetManager(podInformer coreinformers.PodInformer, namespaceInformer coreinformers.NamespaceInformer,
	nodeInformer coreinformers.NodeInformer, nbClient libovsdbclient.Client, getNetworkNameForNADKey func(nadKey string) string) (*AddressSetManager, error) {
	if err := ensurePodNodeNameIndexer(podInformer.Informer()); err != nil {
		return nil, fmt.Errorf("failed to ensure pod nodeName indexer: %w", err)
	}

	m := &AddressSetManager{
		name:                           "pod-selector-address-set-manager",
		nbClient:                       nbClient,
		addressSetFactoryV4:            addressset.NewOvnAddressSetFactory(nbClient, true, false),
		addressSetFactoryV6:            addressset.NewOvnAddressSetFactory(nbClient, false, true),
		addressSetFactoryDualstack:     addressset.NewOvnAddressSetFactory(nbClient, true, true),
		addressSets:                    syncmap.NewSyncMap[*podSelectorAddressSet](),
		podLister:                      podInformer.Lister(),
		podIndexer:                     podInformer.Informer().GetIndexer(),
		namespaceLister:                namespaceInformer.Lister(),
		nodeLister:                     nodeInformer.Lister(),
		getNetworkNameForNADKey:        getNetworkNameForNADKey,
		hostNetworkSelectingAddrSets:   sets.New[string](),
		hostNetworkNamespaceIPsPerNode: make(map[string][]string),
		clusterNodeIPsBackRefs:         sets.New[string](),
	}
	podCfg := &controller.ControllerConfig[corev1.Pod]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      m.reconcilePod,
		ObjNeedsUpdate: m.podNeedUpdate,
		MaxAttempts:    controller.InfiniteAttempts,
		Threadiness:    1,
		Informer:       podInformer.Informer(),
		Lister:         podInformer.Lister().List,
	}
	m.podController = controller.NewController[corev1.Pod](m.name+"-pod", podCfg)

	nsCfg := &controller.ControllerConfig[corev1.Namespace]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      m.reconcileNamespace,
		ObjNeedsUpdate: m.nsNeedUpdate,
		MaxAttempts:    controller.InfiniteAttempts,
		Threadiness:    1,
		Informer:       namespaceInformer.Informer(),
		Lister:         namespaceInformer.Lister().List,
	}
	m.nsController = controller.NewController[corev1.Namespace](m.name+"-namespace", nsCfg)

	nodeCfg := &controller.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      m.reconcileNode,
		ObjNeedsUpdate: m.nodeNeedUpdate,
		MaxAttempts:    controller.InfiniteAttempts,
		Threadiness:    1,
		Informer:       nodeInformer.Informer(),
		Lister:         nodeInformer.Lister().List,
	}
	m.nodeController = controller.NewController[corev1.Node](m.name+"-node", nodeCfg)

	// addressSetReconciler is fed from the pod, namespace and node controllers
	m.addressSetReconciler = controller.NewReconciler(
		m.name+"-addrset",
		&controller.ReconcilerConfig{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:   m.reconcileAddressSet,
			Threadiness: 1,
			MaxAttempts: controller.InfiniteAttempts,
		},
	)
	return m, nil
}

func (m *AddressSetManager) Start() error {
	klog.Infof("Starting %s controller", m.name)
	return controller.StartWithInitialSync(m.initialSync, m.podController, m.nsController, m.nodeController, m.addressSetReconciler)
}

func (m *AddressSetManager) Stop() {
	klog.Infof("Stopping %s controller", m.name)
	controller.Stop(m.podController, m.nsController, m.nodeController, m.addressSetReconciler)
}

// initialSync will clean up all address sets that don't have ACL reference
// Since addressset manager is started before its users, the cleanup for not-anymore-existing objects will be done
// after this function returns, so we technically clean up address sets that are not used anymore on the next restart only.
// There is no good way to know at this point which address sets will become unused after all the users finish their cleanup.
// Address sets don't have an informer, so we won't run reconcile for every address set, only when someone requests
// it through EnsureAddressSet, so if you look in the db directly some address sets may have stale IPs, but that only means
// that they are not used anymore.
func (m *AddressSetManager) initialSync() error {
	if config.Kubernetes.HostNetworkNamespace != "" {
		if err := m.updateHostNetworkNamespaceExists(); err != nil {
			return fmt.Errorf("failed to check if host network namespace %s exists: %v", config.Kubernetes.HostNetworkNamespace, err)
		}
	}
	return libovsdbutil.DeleteAddrSetsWithoutACLRefAnyController(libovsdbops.AddressSetPodSelector, m.nbClient)
}

// getClusterNodeIPsAddrSetDbIDs returns the DB IDs for the shared cluster node IP address set.
func getClusterNodeIPsAddrSetDbIDs() *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetClusterNodeIPs, ovntypes.DefaultNetworkControllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: ClusterNodeIPsAddrSetName,
		})
}

// EnsureClusterNodeIPsAddressSet registers a controller-lifetime user of the
// shared cluster node IP address set and returns its DB IDs. The address set is
// populated from node k8s.ovn.org/host-cidrs annotations through
// util.GetNodeAddresses. Users do not deregister because each backref
// represents a feature controller, not an individual object.
func (m *AddressSetManager) EnsureClusterNodeIPsAddressSet(backRef string) (*libovsdbops.DbObjectIDs, error) {
	dbIDs := getClusterNodeIPsAddrSetDbIDs()
	if backRef == "" {
		return nil, fmt.Errorf("cluster node IP address set backref is empty")
	}
	m.clusterNodeIPsLock.Lock()
	defer m.clusterNodeIPsLock.Unlock()

	if m.clusterNodeIPsBackRefs.Has(backRef) {
		return dbIDs, nil
	}

	m.clusterNodeIPsBackRefs.Insert(backRef)
	if err := m.syncClusterNodeIPsAddressSetLocked(); err != nil {
		m.clusterNodeIPsBackRefs.Delete(backRef)
		return nil, err
	}
	return dbIDs, nil
}

func (m *AddressSetManager) clusterNodeIPsAddressSetInUse() bool {
	m.clusterNodeIPsLock.RLock()
	defer m.clusterNodeIPsLock.RUnlock()
	return m.clusterNodeIPsBackRefs.Len() > 0
}

func (m *AddressSetManager) syncClusterNodeIPsAddressSet() error {
	m.clusterNodeIPsLock.Lock()
	defer m.clusterNodeIPsLock.Unlock()

	return m.syncClusterNodeIPsAddressSetLocked()
}

func (m *AddressSetManager) syncClusterNodeIPsAddressSetLocked() error {
	if m.clusterNodeIPsBackRefs.Len() == 0 {
		return nil
	}

	as, err := addressset.NewOvnAddressSetFactory(m.nbClient, config.IPv4Mode, config.IPv6Mode).EnsureAddressSet(getClusterNodeIPsAddrSetDbIDs())
	if err != nil {
		return fmt.Errorf("cannot ensure address set %s exists: %w", ClusterNodeIPsAddrSetName, err)
	}

	nodes, err := m.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}
	v4NodeAddrs, v6NodeAddrs, err := util.GetNodeAddresses(config.IPv4Mode, config.IPv6Mode, nodes...)
	if err != nil {
		return fmt.Errorf("failed to get node addresses: %w", err)
	}
	allAddresses := make([]net.IP, 0, len(v4NodeAddrs)+len(v6NodeAddrs))
	allAddresses = append(allAddresses, v4NodeAddrs...)
	allAddresses = append(allAddresses, v6NodeAddrs...)
	if err := as.SetAddresses(util.StringSlice(allAddresses)); err != nil {
		return fmt.Errorf("failed to set node IP address set addresses: %w", err)
	}
	return nil
}

// EnsureAddressSet returns address set for requested (podSelector, namespaceSelector, namespace, nodeSelector).
// If namespaceSelector is nil, namespace will be used with podSelector statically.
// podSelector should not be nil, use metav1.LabelSelector{} to match all pods.
// namespaceSelector can only be nil when namespace is set, use metav1.LabelSelector{} to match all namespaces.
// nodeSelector is optional; nil means pods on all nodes are included.
// podSelector = metav1.LabelSelector{} + static namespace may be replaced with namespace address set,
// podSelector = metav1.LabelSelector{} + namespaceSelector may be replaced with a set of namespace address sets,
// but both cases will work here too.
// legacyNetpolMode will not select hostnetwork pod IPs and will include config.Kubernetes.HostNetworkNamespace address set IPs
// when that namespace is matched with an empty pod selector.
//
// backRef is the key that should be used for cleanup.
// psAddrSetHashV4, psAddrSetHashV6 may be set to empty string if address set for that ipFamily wasn't created.
func (m *AddressSetManager) EnsureAddressSet(podSelector, namespaceSelector, nodeSelector *metav1.LabelSelector,
	namespace, backRef, controllerName string, netInfo util.NetInfo, legacyNetpolMode bool) (addrSetKey, psAddrSetHashV4, psAddrSetHashV6 string, err error) {
	nodeSelector = normalizeNodeSelector(nodeSelector)
	if podSelector == nil {
		err = fmt.Errorf("pod selector is nil")
		return
	}
	if namespaceSelector == nil && namespace == "" {
		err = fmt.Errorf("namespace selector is nil and namespace is empty")
		return
	}
	if namespaceSelector != nil {
		// namespace will be ignored in this case
		namespace = ""
	}
	var nsSel, podSel, nodeSel labels.Selector
	if namespaceSelector != nil {
		nsSel, err = metav1.LabelSelectorAsSelector(namespaceSelector)
		if err != nil {
			err = fmt.Errorf("can't parse namespace selector %v: %w", namespaceSelector, err)
			return
		}
	}

	podSel, err = metav1.LabelSelectorAsSelector(podSelector)
	if err != nil {
		err = fmt.Errorf("can't parse pod selector %v: %w", podSelector, err)
		return
	}
	if nodeSelector != nil {
		nodeSel, err = metav1.LabelSelectorAsSelector(nodeSelector)
		if err != nil {
			err = fmt.Errorf("can't parse node selector %v: %w", nodeSelector, err)
			return
		}
	}
	addrSetKey = getInternalKey(podSelector, namespaceSelector, nodeSelector, namespace, controllerName, legacyNetpolMode)

	var found bool
	err = m.addressSets.DoWithLock(addrSetKey, func(key string) error {
		var psAddrSet *podSelectorAddressSet
		psAddrSet, found = m.addressSets.Load(key)
		if !found {
			addrSetDbIDs := GetPodSelectorAddrSetDbIDs(podSelector, namespaceSelector, nodeSelector, namespace, controllerName, legacyNetpolMode)
			ipv4Mode, ipv6Mode := netInfo.IPMode()
			var addrSet addressset.AddressSet
			switch {
			case ipv4Mode && !ipv6Mode:
				addrSet, err = m.addressSetFactoryV4.EnsureAddressSet(addrSetDbIDs)
			case !ipv4Mode && ipv6Mode:
				addrSet, err = m.addressSetFactoryV6.EnsureAddressSet(addrSetDbIDs)
			case ipv4Mode && ipv6Mode:
				addrSet, err = m.addressSetFactoryDualstack.EnsureAddressSet(addrSetDbIDs)
			default:
				return fmt.Errorf("neither IPv4 nor IPv6 mode is enabled")
			}
			// if the first step of creating address set fails, return error since there is nothing to cleanup
			if err != nil {
				return err
			}
			psAddrSet = &podSelectorAddressSet{
				backRefs:          map[string]bool{},
				podSelector:       podSel,
				namespaceSelector: nsSel,
				namespace:         namespace,
				nodeSelector:      nodeSel,
				addressSet:        addrSet,
				controllerName:    controllerName,
				netInfo:           netInfo,
				legacyNetpolMode:  legacyNetpolMode,
				selectedNamespaces: &selectedNamespaces{
					set: sets.New[string](),
					// until the first reconcile, we assume all namespaces are selected to avoid missing any updates
					all: true,
				},
			}
			m.addressSets.LoadOrStore(key, psAddrSet)
		}
		// psAddrSet is successfully init-ed
		psAddrSet.backRefs[backRef] = true
		psAddrSetHashV4, psAddrSetHashV6 = psAddrSet.addressSet.GetASHashNames()
		return nil
	})
	if err != nil {
		return
	}
	if !found {
		// Populate a newly created address set before returning hashes to the caller.
		// Until reconcile runs, the NB object is empty while ACLs may already reference it
		// (e.g. on DB ID change or first ensure). Force fullSync so replaceAddressSetContents
		// builds the desired state and initializes the in-memory mirrors. Existing sets are
		// kept up to date by the async reconciler on pod/namespace/node events.
		if reconcileErr := m.addressSets.DoWithLock(addrSetKey, func(key string) error {
			psAddrSet, ok := m.addressSets.Load(key)
			if !ok {
				return nil
			}
			psAddrSet.pendingUpdates.fullSync = true
			return m.reconcileAddressSetLocked(key, psAddrSet)
		}); reconcileErr != nil {
			klog.Errorf("Failed to reconcile address set %s on ensure: %v", addrSetKey, reconcileErr)
			// this only puts key to the queue, no lock
			m.addressSetReconciler.Reconcile(addrSetKey)
		}
	}
	return
}

// CleanupForController destroys all address sets owned by the given controller
func (m *AddressSetManager) CleanupForController(controllerName string) error {
	var errs []error
	for _, key := range m.addressSets.GetKeys() {
		if err := m.addressSets.DoWithLock(key, func(key string) error {
			psAddrSet, found := m.addressSets.Load(key)
			if !found || psAddrSet.controllerName != controllerName {
				return nil
			}
			if err := psAddrSet.addressSet.Destroy(); err != nil {
				return fmt.Errorf("failed to destroy address set %s: %w", key, err)
			}
			m.addressSets.Delete(key)
			m.unregisterHostNetworkSelectingAddrSet(key)
			return nil
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.Join(errs...)
}

func (m *AddressSetManager) DeleteAddressSet(addrSetKey, backRef string) error {
	return m.addressSets.DoWithLock(addrSetKey, func(key string) error {
		psAddrSet, found := m.addressSets.Load(key)
		if !found {
			return nil
		}
		delete(psAddrSet.backRefs, backRef)
		if len(psAddrSet.backRefs) == 0 {
			err := psAddrSet.addressSet.Destroy()
			if err != nil {
				return fmt.Errorf("failed to destroy address set %s: %w", key, err)
			}
			m.addressSets.Delete(key)
			m.unregisterHostNetworkSelectingAddrSet(key)
		}
		return nil
	})
}

// unregisterHostNetworkSelectingAddrSet drops a destroyed address set from host-network
// tracking so host IP updates do not keep enqueueing stale keys.
func (m *AddressSetManager) unregisterHostNetworkSelectingAddrSet(key string) {
	m.hostNetworkNamespaceLock.Lock()
	defer m.hostNetworkNamespaceLock.Unlock()
	m.hostNetworkSelectingAddrSets.Delete(key)
}

func (m *AddressSetManager) podNeedUpdate(old, new *corev1.Pod) bool {
	if new == nil {
		return true
	}
	if new.Spec.NodeName == "" {
		// pod is not scheduled yet, no IPs should be assigned, but update event will be received when pod gets scheduled, so we can wait for that
		return false
	}
	if old == nil {
		// new pod, check if it has IPs already, if not, wait for update event when IPs are assigned
		return new.Annotations[ovntypes.OvnPodAnnotationName] != "" || len(new.Status.PodIPs) > 0
	}
	if new.Annotations[ovntypes.OvnPodAnnotationName] != old.Annotations[ovntypes.OvnPodAnnotationName] {
		// this annotation is set when pod gets its IPs, so if it changes, we need to reconcile to update address set with new IPs
		return true
	}
	if !slices.Equal(new.Status.PodIPs, old.Status.PodIPs) {
		// if pod IPs change, we need to reconcile to update address set with new IPs
		return true
	}
	if util.PodCompleted(new) != util.PodCompleted(old) {
		// if pod has completed, handle as delete event following retry framework logic
		return true
	}
	if !labels.Equals(new.Labels, old.Labels) {
		// labels updates affect selectors
		return true
	}
	return false
}

func (m *AddressSetManager) reconcilePod(podKey string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(podKey)
	if err != nil {
		return fmt.Errorf("failed to split meta namespace key %q: %v", podKey, err)
	}
	var pod *corev1.Pod
	// only reconcile if this pod is in a namespace that is selected by an address set
	// Get all existing keys, then lock address sets per key and check if they are affected.
	// If the new keys are added, it will always call reconcile for that new key, so there is no race.
	// If some keys are deleted, we just ignore it.
	existingAddrSets := m.addressSets.GetKeys()
	for _, addrSetKey := range existingAddrSets {
		// never returns error
		if err = m.addressSets.DoWithLock(addrSetKey, func(addrSetKey string) error {
			addrSet, found := m.addressSets.Load(addrSetKey)
			if !found {
				// nothing to do
				return nil
			}
			if addrSet.nodeSelector != nil {
				if pod == nil {
					pod, err = m.podLister.Pods(namespace).Get(name)
					if err != nil {
						if apierrors.IsNotFound(err) {
							// pod deleted
							pod = nil
						} else {
							return fmt.Errorf("failed to get pod %s in namespace %s: %v", name, namespace, err)
						}
					}
				}
				// check if pod's node matches address set's node selector
				if pod == nil || addrSet.selectedNodes == nil || addrSet.selectedNodes.Has(pod.Spec.NodeName) {
					// Record the pod key so reconcileAddressSet can update membership incrementally
					// without listing all matching pods.
					addrSet.pendingUpdates.addPodKey(podKey)
					m.addressSetReconciler.Reconcile(addrSetKey)
					return nil
				}
				return nil
			}
			// only check address sets that have previously matched pod's namespace to avoid extra reconciliations
			previouslyMatchedNamespaces := addrSet.selectedNamespaces
			if previouslyMatchedNamespaces == nil || previouslyMatchedNamespaces.Has(namespace) {
				// Record the pod key so reconcileAddressSet can update membership incrementally
				// without listing all matching pods.
				addrSet.pendingUpdates.addPodKey(podKey)
				m.addressSetReconciler.Reconcile(addrSetKey)
				return nil
			}
			return nil
		}); err != nil {
			return fmt.Errorf("failed to reconcile address set %s for pod %s: %v", addrSetKey, podKey, err)
		}
	}
	return nil
}

func (m *AddressSetManager) nsNeedUpdate(old, new *corev1.Namespace) bool {
	if new == nil || old == nil {
		return true
	}
	if !labels.Equals(new.Labels, old.Labels) {
		// if namespace labels change, we need to reconcile to check if this namespace still matches address set selectors
		return true
	}
	return false
}

func (m *AddressSetManager) updateHostNetworkNamespaceExists() error {
	_, err := m.namespaceLister.Get(config.Kubernetes.HostNetworkNamespace)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get host network namespace %s: %v", config.Kubernetes.HostNetworkNamespace, err)
		}
	}
	m.hostNetworkNamespaceLock.Lock()
	if err != nil {
		// Namespace deleted or never existed. Snapshot selecting sets under the lock so we can
		// enqueue them after clearing global host state (symmetric with the create path), rather
		// than relying only on reconcileNamespace's later membership loop.
		wasPresent := m.hostNetworkNamespaceExists
		addrSetKeys := m.hostNetworkSelectingAddrSets.UnsortedList()
		clear(m.hostNetworkNamespaceIPsPerNode)
		m.hostNetworkNamespaceExists = false
		m.hostNetworkNamespaceLock.Unlock()
		if wasPresent {
			// Host IPs are already cleared and exists=false, so the next reconcile sees
			// selecting=false. Enqueue a host-only pending update (not fullSync) so
			// deriveUnionAddressDelta drains host IPs without rebuilding all pod membership.
			return m.enqueueHostNetworkSelectingAddrSetUpdates(addrSetKeys, "", false)
		}
		return nil
	}
	needsFullSync := !m.hostNetworkNamespaceExists
	var addrSetKeys []string
	if needsFullSync {
		// namespace was just created, get all existing host network IPs
		ips, ipErr := m.getAllHostNamespaceAddresses()
		if ipErr != nil {
			m.hostNetworkNamespaceLock.Unlock()
			return fmt.Errorf("error getting host network namespace %s IPs: %v", config.Kubernetes.HostNetworkNamespace, ipErr)
		}
		m.hostNetworkNamespaceIPsPerNode = ips
		// Snapshot selecting address sets before releasing the lock; per-key locks are taken below.
		addrSetKeys = m.hostNetworkSelectingAddrSets.UnsortedList()
	}
	m.hostNetworkNamespaceExists = true
	m.hostNetworkNamespaceLock.Unlock()

	if needsFullSync {
		// Host-network namespace just appeared with a full IP set; force fullSync on sets
		// that already select it so host-network addresses are rebuilt from scratch.
		return m.enqueueHostNetworkSelectingAddrSetUpdates(addrSetKeys, "", true)
	}
	return nil
}

func (m *AddressSetManager) reconcileNamespace(nsKey string) error {
	if config.Kubernetes.HostNetworkNamespace != "" && nsKey == config.Kubernetes.HostNetworkNamespace {
		if err := m.updateHostNetworkNamespaceExists(); err != nil {
			return fmt.Errorf("failed to check if host network namespace %s exists: %v", config.Kubernetes.HostNetworkNamespace, err)
		}
	}
	// find address sets that could be affected by this namespace event
	// Get all existing keys, then lock address sets per key and check if they are affected.
	// If the new keys are added, it will always call reconcile for that new key, so there is no race.
	// If some keys are deleted, we just ignore it.
	existingAddrSets := m.addressSets.GetKeys()
	for _, addrSetKey := range existingAddrSets {
		err := m.addressSets.DoWithLock(addrSetKey, func(addrSetKey string) error {
			addrSet, found := m.addressSets.Load(addrSetKey)
			if !found {
				// nothing to do
				return nil
			}
			// first find namespaces that currently match this address set
			currentlyMatchedNamespaces, err := m.getSelectedNamespaces(addrSet)
			if err != nil {
				return err
			}
			if currentlyMatchedNamespaces.Has(nsKey) {
				// Namespace entered or still matches selection; record for namespace-scoped reconcile.
				addrSet.pendingUpdates.addNamespaceKey(nsKey)
				m.addressSetReconciler.Reconcile(addrSetKey)
				return nil
			}
			// now check if this address set was matching this namespace before, if yes, reconcile since it might not match anymore
			previouslyMatchedNamespaces := addrSet.selectedNamespaces
			if previouslyMatchedNamespaces.Has(nsKey) {
				// Namespace left selection; record for namespace-scoped reconcile to remove its pods.
				addrSet.pendingUpdates.addNamespaceKey(nsKey)
				m.addressSetReconciler.Reconcile(addrSetKey)
				return nil
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("failed to reconcile address set %s for namespace %s: %v", addrSetKey, nsKey, err)
		}

	}
	return nil
}

func (m *AddressSetManager) nodeNeedUpdate(old, new *corev1.Node) bool {
	if new == nil || old == nil {
		return true
	}
	if !labels.Equals(new.Labels, old.Labels) {
		// if node labels change, we need to reconcile address sets that use a node selector
		return true
	}
	if util.NodeHostCIDRsAnnotationChanged(old, new) && m.clusterNodeIPsAddressSetInUse() {
		return true
	}
	// only check annotations that are used in getHostNamespaceAddressesForNode
	if util.NodeSubnetAnnotationChangedForNetwork(old, new, ovntypes.DefaultNetworkName) || util.NodeIDAnnotationChanged(old, new) ||
		old.Annotations[util.OvnNodeIfAddr] != new.Annotations[util.OvnNodeIfAddr] {
		// hostnetwork namespace may need to be updated
		m.hostNetworkNamespaceLock.Lock()
		defer m.hostNetworkNamespaceLock.Unlock()
		return m.hostNetworkNamespaceExists
	}

	return false
}

func (m *AddressSetManager) reconcileNode(nodeKey string) error {
	// update host network IPs first to have fresh info for addr set reconcile
	// don't return error immediately to let other changes like node selector be propagated
	hostNetworkErr := m.updateHostNetworkIPs(nodeKey)
	clusterNodeIPsErr := m.syncClusterNodeIPsAddressSet()

	// find address sets that could be affected by this node event
	// Get all existing keys, then lock address sets per key and check if they are affected.
	// If the new keys are added, it will always call reconcile for that new key, so there is no race.
	// If some keys are deleted, we just ignore it.
	existingAddrSets := m.addressSets.GetKeys()
	for _, addrSetKey := range existingAddrSets {
		err := m.addressSets.DoWithLock(addrSetKey, func(addrSetKey string) error {
			addrSet, _ := m.addressSets.Load(addrSetKey)
			if addrSet == nil || addrSet.nodeSelector == nil || addrSet.nodeSelector.Empty() {
				// nothing to do
				return nil
			}
			// first find nodes that currently match this address set
			currentlyMatchedNodes, err := m.getSelectedNodes(addrSet.nodeSelector)
			if err != nil {
				return err
			}
			if currentlyMatchedNodes.Has(nodeKey) {
				// Node entered or still matches the node selector; record for node-scoped reconcile.
				addrSet.pendingUpdates.addNodeKey(nodeKey)
				m.addressSetReconciler.Reconcile(addrSetKey)
				return nil
			}
			// reconcile the address set if the node matches the previous selected nodes
			previouslyMatchedNodes := addrSet.selectedNodes
			// previouslyMatchedNodes == nil means the address set hasn't been reconciled yet, so need to reconcile
			if previouslyMatchedNodes == nil || previouslyMatchedNodes.Has(nodeKey) {
				// Node left selection (or first reconcile); record for node-scoped reconcile.
				addrSet.pendingUpdates.addNodeKey(nodeKey)
				m.addressSetReconciler.Reconcile(addrSetKey)
				return nil
			}
			return nil
		})
		if err != nil {
			return utilerrors.Join(hostNetworkErr, clusterNodeIPsErr,
				fmt.Errorf("failed to reconcile address set %s for node %s: %v", addrSetKey, nodeKey, err))
		}
	}
	return utilerrors.Join(hostNetworkErr, clusterNodeIPsErr)
}

func (m *AddressSetManager) updateHostNetworkIPs(nodeName string) error {
	m.hostNetworkNamespaceLock.Lock()
	if !m.hostNetworkNamespaceExists {
		m.hostNetworkNamespaceLock.Unlock()
		return nil
	}
	m.hostNetworkNamespaceLock.Unlock()

	node, err := m.nodeLister.Get(nodeName)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get node %s: %v", nodeName, err)
	}

	var addrSetKeys []string
	m.hostNetworkNamespaceLock.Lock()
	if !m.hostNetworkNamespaceExists {
		m.hostNetworkNamespaceLock.Unlock()
		return nil
	}
	if err != nil || config.HybridOverlay.Enabled && util.NoHostSubnet(node) {
		// delete event OR node started matching hybrid overlay and should be ignored for host network namespace
		// delete host network IPs for this node from host network namespace's address set
		updated := len(m.hostNetworkNamespaceIPsPerNode[nodeName]) != 0
		delete(m.hostNetworkNamespaceIPsPerNode, nodeName)
		if updated {
			addrSetKeys = m.hostNetworkSelectingAddrSets.UnsortedList()
		}
		m.hostNetworkNamespaceLock.Unlock()
		if !updated {
			return nil
		}
		return m.enqueueHostNetworkSelectingAddrSetUpdates(addrSetKeys, nodeName, false)
	}
	// add/update node event
	hostNetworkPolicyIPs, err := m.getHostNamespaceAddressesForNode(node)
	if err != nil {
		m.hostNetworkNamespaceLock.Unlock()
		return fmt.Errorf("error parsing annotation for node %s: %w", node.Name, err)
	}
	// add the host network IPs for this node to host network namespace's address set
	// hostNetworkPolicyIPs is built from the annotations and always preserves ips order
	if slices.Equal(m.hostNetworkNamespaceIPsPerNode[node.Name], hostNetworkPolicyIPs) {
		m.hostNetworkNamespaceLock.Unlock()
		return nil
	}
	m.hostNetworkNamespaceIPsPerNode[node.Name] = hostNetworkPolicyIPs
	addrSetKeys = m.hostNetworkSelectingAddrSets.UnsortedList()
	m.hostNetworkNamespaceLock.Unlock()

	// Host-network IPs for this node changed; enqueue selecting address sets with
	// a node-key pending update so only host-network delta is recomputed.
	return m.enqueueHostNetworkSelectingAddrSetUpdates(addrSetKeys, nodeName, false)
}

// enqueueHostNetworkSelectingAddrSetUpdates records pending updates on host-network selecting
// address sets and enqueues reconciliation. Must not be called while holding hostNetworkNamespaceLock.
// When fullSync is false, nodeName may be empty to mark a host-global change (e.g. host NS delete).
func (m *AddressSetManager) enqueueHostNetworkSelectingAddrSetUpdates(addrSetKeys []string, nodeName string, fullSync bool) error {
	for _, addrSetKey := range addrSetKeys {
		if err := m.addressSets.DoWithLock(addrSetKey, func(key string) error {
			psAddrSet, ok := m.addressSets.Load(key)
			if !ok {
				return nil
			}
			if fullSync {
				psAddrSet.pendingUpdates.fullSync = true
			} else {
				psAddrSet.pendingUpdates.addNodeKey(nodeName)
			}
			return nil
		}); err != nil {
			return err
		}
		m.addressSetReconciler.Reconcile(addrSetKey)
	}
	return nil
}

// getAllHostNamespaceAddresses retrieves management port and gateway router LRP
// IP for all nodes in the cluster
func (m *AddressSetManager) getAllHostNamespaceAddresses() (map[string][]string, error) {
	ips := make(map[string][]string)
	// add the mp0 interface addresses to this namespace.
	existingNodes, err := m.nodeLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to get all nodes (%v)", err)
	} else {
		for _, node := range existingNodes {
			if config.HybridOverlay.Enabled && util.NoHostSubnet(node) {
				// skip hybrid overlay nodes
				continue
			}
			hostNetworkIPs, err := m.getHostNamespaceAddressesForNode(node)
			if err != nil {
				klog.Errorf("Error parsing annotation for node %s: %v", node.Name, err)
			}
			ips[node.Name] = hostNetworkIPs
		}
	}
	return ips, nil
}

// getHostNamespaceAddressesForNode retrieves management port and gateway router LRP
// IP of a specific node
func (m *AddressSetManager) getHostNamespaceAddressesForNode(node *corev1.Node) ([]string, error) {
	var ips []string
	defaultNetInfo := &util.DefaultNetInfo{}
	hostSubnets, err := util.ParseNodeHostSubnetAnnotation(node, ovntypes.DefaultNetworkName)
	if err != nil {
		if !util.IsAnnotationNotSetError(err) {
			return nil, fmt.Errorf("failed to get node host subnets: %w", err)
		}
	}
	for _, hostSubnet := range hostSubnets {
		mgmtIfAddr := defaultNetInfo.GetNodeManagementIP(hostSubnet)
		if mgmtIfAddr == nil {
			return nil, fmt.Errorf("node %s has no management IP in subnet %s", node.Name, hostSubnet.String())
		}
		ips = append(ips, mgmtIfAddr.IP.String())
	}
	// for shared gateway mode we will use LRP IPs to SNAT host network traffic
	// so add these to the address set.
	lrpIPs, gwIPsErr := udn.GetGWRouterIPs(node, defaultNetInfo)
	if gwIPsErr != nil {
		if !util.IsAnnotationNotSetError(gwIPsErr) {
			return nil, gwIPsErr
		}
	}

	for _, lrpIP := range lrpIPs {
		ips = append(ips, lrpIP.IP.String())
	}
	// When NoOverlay mode is enabled, also include the node's primary physical interface IP
	if defaultNetInfo.Transport() == ovntypes.NetworkTransportNoOverlay {
		nodeIfAddr, err := util.GetNodeIfAddrAnnotation(node)
		if err != nil {
			if !util.IsAnnotationNotSetError(err) {
				return nil, fmt.Errorf("failed to get node primary interface address: %w", err)
			}
		} else {
			if nodeIfAddr.IPv4 != "" {
				ipv4, _, err := net.ParseCIDR(nodeIfAddr.IPv4)
				if err != nil {
					return nil, fmt.Errorf("failed to parse node primary IPv4 address %s: %w", nodeIfAddr.IPv4, err)
				}
				ips = append(ips, ipv4.String())
			}
			if nodeIfAddr.IPv6 != "" {
				ipv6, _, err := net.ParseCIDR(nodeIfAddr.IPv6)
				if err != nil {
					return nil, fmt.Errorf("failed to parse node primary IPv6 address %s: %w", nodeIfAddr.IPv6, err)
				}
				ips = append(ips, ipv6.String())
			}
		}
	}
	return ips, nil
}

// reconcileAddressSet is the workqueue handler for pod-selector address sets.
// It acquires the per-key lock and delegates to reconcileAddressSetLocked.
func (m *AddressSetManager) reconcileAddressSet(key string) error {
	return m.addressSets.DoWithLock(key, func(key string) error {
		psAddrSet, found := m.addressSets.Load(key)
		if !found {
			return nil
		}
		return m.reconcileAddressSetLocked(key, psAddrSet)
	})
}

// reconcileAddressSetLocked reconciles a pod-selector address set under the per-key lock.
// It prefers an incremental path driven by pendingUpdates (only the triggering
// pods/namespaces/nodes) and falls back to a full desired-state rebuild when needed.
// pendingUpdates is cleared only after a successful reconcile so a failed attempt
// keeps its triggering keys for retry and for coalescing with later events.
func (m *AddressSetManager) reconcileAddressSetLocked(key string, psAddrSet *podSelectorAddressSet) error {
	pending := psAddrSet.pendingUpdates.snapshot()

	matchedNamespaces, err := m.getSelectedNamespaces(psAddrSet)
	if err != nil {
		return fmt.Errorf("failed to get selected namespaces for address set %s: %w", key, err)
	}

	var selectedNodes sets.Set[string]
	if psAddrSet.nodeSelector != nil && !psAddrSet.nodeSelector.Empty() {
		selectedNodes, err = m.getSelectedNodes(psAddrSet.nodeSelector)
		if err != nil {
			return fmt.Errorf("failed to get selected nodes for address set %s: %w", key, err)
		}
	}

	var toAdd, toRemove []string
	var cachePlan *addressSetCachePlan
	fullSync := m.needsFullAddressSetSync(psAddrSet, pending)
	if !fullSync && m.pendingPodUpdateClassCount(pending, psAddrSet) > 1 {
		// Coalesced namespace/node/pod updates cannot be handled by a single incremental path.
		klog.V(4).Infof("Address set %s: forcing full sync due to coalesced pending update classes "+
			"(namespaces=%d pods=%d nodes=%d)", key, pending.namespaceKeys.Len(), pending.podKeys.Len(), pending.nodeKeys.Len())
		fullSync = true
	}
	// First reconcile for a newly created address set: replace OVN contents wholesale
	// and seed the in-memory mirrors. Avoids diffing against an empty/uninitialized cache.
	if fullSync && psAddrSet.addresses == nil {
		if err := m.replaceAddressSetContents(psAddrSet, matchedNamespaces, selectedNodes, key); err != nil {
			return fmt.Errorf("failed to replace address set contents for %s: %w", key, err)
		}
		psAddrSet.selectedNamespaces = matchedNamespaces
		if psAddrSet.nodeSelector != nil && !psAddrSet.nodeSelector.Empty() {
			psAddrSet.selectedNodes = selectedNodes
		}
		psAddrSet.pendingUpdates.clear()
		return nil
	}
	if fullSync {
		toAdd, toRemove, cachePlan, err = m.computeFullAddressSetDelta(psAddrSet, matchedNamespaces, selectedNodes, key)
		if err != nil {
			return fmt.Errorf("failed to compute full address set delta for %s: %w", key, err)
		}
	} else if pending.namespaceKeys.Len() > 0 {
		// Namespace label change: list pods only in the affected namespaces.
		cachePlan, err = m.computeNamespaceAddressSetCachePlan(psAddrSet, pending.namespaceKeys, matchedNamespaces, selectedNodes)
		if err != nil {
			return fmt.Errorf("failed to compute namespace address set cache plan for %s: %w", key, err)
		}
	} else if pending.nodeKeys.Len() > 0 && psAddrSet.nodeSelector != nil && !psAddrSet.nodeSelector.Empty() {
		// Node selector membership change: update only pods on the affected nodes.
		cachePlan, err = m.computeNodeAddressSetCachePlan(psAddrSet, pending.nodeKeys, matchedNamespaces, selectedNodes)
		if err != nil {
			return fmt.Errorf("failed to compute node address set cache plan for %s: %w", key, err)
		}
	} else if pending.podKeys.Len() > 0 {
		// Pod add/update/delete: process only the pending pod keys.
		cachePlan, err = m.computePodAddressSetCachePlan(psAddrSet, pending.podKeys, matchedNamespaces, selectedNodes)
		if err != nil {
			return fmt.Errorf("failed to compute pod address set cache plan for %s: %w", key, err)
		}
	} else if pending.nodeKeys.Len() == 0 {
		// No pending context (unexpected empty snapshot) — rebuild fully.
		klog.V(4).Infof("Address set %s: forcing full sync due to empty pending updates", key)
		fullSync = true
		toAdd, toRemove, cachePlan, err = m.computeFullAddressSetDelta(psAddrSet, matchedNamespaces, selectedNodes, key)
		if err != nil {
			return fmt.Errorf("failed to compute full address set delta for %s: %w", key, err)
		}
	}
	// Only nodeKeys are pending, if any, means a host-network IP change without a nodeSelector.
	// deriveUnionAddressDelta below will pick up the host-network update.
	if !fullSync {
		// Full sync already unions pod + host IPs against addresses in computeFullAddressSetDelta.
		// Incremental paths must do the same: never remove an IP from OVN while pods or the
		// current host-network snapshot still contribute it.
		hostSnapshot := m.buildHostNetworkSnapshot(psAddrSet, matchedNamespaces, key)
		toAdd, toRemove = m.deriveUnionAddressDelta(psAddrSet, cachePlan, hostSnapshot)
	}

	// Skip OVN transaction entirely when nothing changed.
	if err := m.applyAddressSetChanges(psAddrSet, toAdd, toRemove); err != nil {
		return fmt.Errorf("failed to apply address changes for address set %s: %w", key, err)
	}

	// Commit the pod-cache snapshot used to compute the OVN delta.
	m.applyAddressSetCachePlan(psAddrSet, cachePlan)

	psAddrSet.selectedNamespaces = matchedNamespaces
	if psAddrSet.nodeSelector != nil && !psAddrSet.nodeSelector.Empty() {
		psAddrSet.selectedNodes = selectedNodes
	}
	psAddrSet.pendingUpdates.clear()
	return nil
}

// needsFullAddressSetSync returns true when reconcile must rebuild desired state from all
// matching pods rather than applying an incremental delta.
func (m *AddressSetManager) needsFullAddressSetSync(psAddrSet *podSelectorAddressSet, pending addrSetPendingUpdates) bool {
	return pending.fullSync || psAddrSet.addresses == nil || psAddrSet.selectedPods == nil ||
		psAddrSet.podAddresses == nil || psAddrSet.podNodes == nil || psAddrSet.podsByNode == nil
}

// pendingPodUpdateClassCount returns how many distinct pod-scoped incremental update classes
// are present in pending. Host-network-only nodeKeys (no nodeSelector) are excluded because
// they are handled separately via deriveUnionAddressDelta / buildHostNetworkSnapshot.
func (m *AddressSetManager) pendingPodUpdateClassCount(pending addrSetPendingUpdates, psAddrSet *podSelectorAddressSet) int {
	count := 0
	if pending.namespaceKeys.Len() > 0 {
		count++
	}
	if pending.podKeys.Len() > 0 {
		count++
	}
	if pending.nodeKeys.Len() > 0 && psAddrSet.nodeSelector != nil && !psAddrSet.nodeSelector.Empty() {
		count++
	}
	return count
}

// computeAddressDelta returns addresses present in desired but not current (toAdd) and
// present in current but not desired (toRemove). Nil sets are treated as empty.
func (m *AddressSetManager) computeAddressDelta(current, desired sets.Set[string]) (toAdd, toRemove []string) {
	if current == nil {
		current = sets.New[string]()
	}
	if desired == nil {
		desired = sets.New[string]()
	}
	return desired.Difference(current).UnsortedList(), current.Difference(desired).UnsortedList()
}

// applyAddressSetChanges writes toAdd/toRemove to OVN via ApplyAddressChanges and updates
// the addresses mirror only on success. Returns immediately when both lists are empty.
func (m *AddressSetManager) applyAddressSetChanges(psAddrSet *podSelectorAddressSet, toAdd, toRemove []string) error {
	toAdd, toRemove = addressset.NormalizeAddressChanges(toAdd, toRemove)
	if len(toAdd) == 0 && len(toRemove) == 0 {
		return nil
	}
	if err := psAddrSet.addressSet.ApplyAddressChanges(toAdd, toRemove); err != nil {
		return err
	}
	if psAddrSet.addresses == nil {
		psAddrSet.addresses = sets.New[string]()
	}
	for _, addr := range toRemove {
		psAddrSet.addresses.Delete(addr)
	}
	for _, addr := range toAdd {
		psAddrSet.addresses.Insert(addr)
	}
	return nil
}

// podMatchesAddressSet returns whether pod should currently contribute IPs to the address set,
// applying namespace/node/pod selectors and legacyNetpolMode host-network / completed-pod rules.
func (m *AddressSetManager) podMatchesAddressSet(pod *corev1.Pod, psAddrSet *podSelectorAddressSet,
	matchedNamespaces *selectedNamespaces, selectedNodes sets.Set[string]) bool {
	if pod == nil {
		return false
	}
	if !matchedNamespaces.Has(pod.Namespace) {
		return false
	}
	if psAddrSet.legacyNetpolMode && pod.Spec.HostNetwork {
		return false
	}
	if util.PodCompleted(pod) {
		return false
	}
	if pod.Annotations[ovntypes.OvnPodAnnotationName] == "" && len(pod.Status.PodIPs) == 0 {
		return false
	}
	if !psAddrSet.podSelector.Matches(labels.Set(pod.Labels)) {
		return false
	}
	if psAddrSet.nodeSelector != nil && !psAddrSet.nodeSelector.Empty() {
		if pod.Spec.NodeName == "" || !selectedNodes.Has(pod.Spec.NodeName) {
			return false
		}
	}
	return true
}

// getPodIPsForPod returns the network IPs for a single pod using the same filtering as getPodIPs.
func (m *AddressSetManager) getPodIPsForPod(pod *corev1.Pod, netInfo util.NetInfo, noHostNetwork bool) ([]string, error) {
	ips, err := m.getPodIPs([]*corev1.Pod{pod}, netInfo, noHostNetwork)
	if err != nil {
		return nil, fmt.Errorf("failed to get IPs for pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return ips, nil
}

// ensurePodCacheInitialized lazily allocates the in-memory membership / address mirrors.
func (psAddrSet *podSelectorAddressSet) ensurePodCacheInitialized() {
	if psAddrSet.addresses == nil {
		psAddrSet.addresses = sets.New[string]()
	}
	if psAddrSet.podAddresses == nil {
		psAddrSet.podAddresses = make(map[string][]string)
	}
	if psAddrSet.selectedPods == nil {
		psAddrSet.selectedPods = make(map[string]sets.Set[string])
	}
	if psAddrSet.podNodes == nil {
		psAddrSet.podNodes = make(map[string]string)
	}
	if psAddrSet.podsByNode == nil {
		psAddrSet.podsByNode = make(map[string]sets.Set[string])
	}
}

// removePodFromCache drops podKey from the membership mirrors.
func (psAddrSet *podSelectorAddressSet) removePodFromCache(podKey string) {
	psAddrSet.ensurePodCacheInitialized()
	if _, hadPod := psAddrSet.podAddresses[podKey]; !hadPod {
		return
	}
	delete(psAddrSet.podAddresses, podKey)
	namespace, name, _ := cache.SplitMetaNamespaceKey(podKey)
	if podSet, ok := psAddrSet.selectedPods[namespace]; ok {
		podSet.Delete(name)
		if podSet.Len() == 0 {
			delete(psAddrSet.selectedPods, namespace)
		}
	}
	if nodeName, ok := psAddrSet.podNodes[podKey]; ok {
		delete(psAddrSet.podNodes, podKey)
		if nodePods, ok := psAddrSet.podsByNode[nodeName]; ok {
			nodePods.Delete(podKey)
			if nodePods.Len() == 0 {
				delete(psAddrSet.podsByNode, nodeName)
			}
		}
	}
}

// addPodToCache records that podKey currently contributes ips on nodeName to the address set.
func (psAddrSet *podSelectorAddressSet) addPodToCache(podKey string, ips []string, nodeName string) {
	psAddrSet.ensurePodCacheInitialized()
	namespace, name, _ := cache.SplitMetaNamespaceKey(podKey)
	if psAddrSet.selectedPods[namespace] == nil {
		psAddrSet.selectedPods[namespace] = sets.New[string]()
	}
	psAddrSet.selectedPods[namespace].Insert(name)
	psAddrSet.podAddresses[podKey] = slices.Clone(ips)

	if oldNode, ok := psAddrSet.podNodes[podKey]; ok && oldNode != nodeName {
		if nodePods, ok := psAddrSet.podsByNode[oldNode]; ok {
			nodePods.Delete(podKey)
			if nodePods.Len() == 0 {
				delete(psAddrSet.podsByNode, oldNode)
			}
		}
	}
	psAddrSet.podNodes[podKey] = nodeName
	if nodeName != "" {
		if psAddrSet.podsByNode[nodeName] == nil {
			psAddrSet.podsByNode[nodeName] = sets.New[string]()
		}
		psAddrSet.podsByNode[nodeName].Insert(podKey)
	}
}

// evaluatePodCacheUpdate determines whether podKey should remain in the membership mirrors
// after reconcile and which IPs it should contribute. Does not compute OVN address deltas.
func (m *AddressSetManager) evaluatePodCacheUpdate(psAddrSet *podSelectorAddressSet, pod *corev1.Pod,
	matchedNamespaces *selectedNamespaces, selectedNodes sets.Set[string]) (keepInCache bool, ips []string, err error) {
	if pod == nil || !m.podMatchesAddressSet(pod, psAddrSet, matchedNamespaces, selectedNodes) {
		return false, nil, nil
	}
	newIPs, err := m.getPodIPsForPod(pod, psAddrSet.netInfo, psAddrSet.legacyNetpolMode)
	if err != nil {
		return false, nil, fmt.Errorf("failed to evaluate pod cache update for %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return true, newIPs, nil
}

// postBatchPodIPSet returns the pod IP set after overlaying plan mutations onto podAddresses.
// A nil plan (or a plan with no pod changes) yields the current pod IP set. Shared-IP ownership
// is evaluated across the whole batch so concurrent removals/transfers in one reconcile produce
// a single correct membership set. Nil podAddresses is treated as empty; this path does not
// allocate or mutate the OVN addresses mirror.
func (m *AddressSetManager) postBatchPodIPSet(psAddrSet *podSelectorAddressSet, plan *addressSetCachePlan) sets.Set[string] {
	podIPs := sets.New[string]()

	if plan != nil && plan.podCacheReplace != nil {
		for _, ips := range plan.podCacheReplace.podAddresses {
			podIPs.Insert(ips...)
		}
		return podIPs
	}

	for podKey, ips := range psAddrSet.podAddresses {
		if plan != nil {
			if plan.podRemoves.Has(podKey) {
				continue
			}
			if entry, ok := plan.podSets[podKey]; ok {
				podIPs.Insert(entry.ips...)
				continue
			}
		}
		podIPs.Insert(ips...)
	}
	if plan != nil {
		for podKey, entry := range plan.podSets {
			if _, existed := psAddrSet.podAddresses[podKey]; existed {
				continue
			}
			podIPs.Insert(entry.ips...)
		}
	}
	return podIPs
}

// deriveUnionAddressDelta diffs psAddrSet.addresses against the union of post-batch pod IPs
// and the desired host-network IP set. Pod and host contributions must not be applied as
// independent OVN deltas: an IP contributed by both must remain in OVN until neither does.
func (m *AddressSetManager) deriveUnionAddressDelta(psAddrSet *podSelectorAddressSet, podPlan *addressSetCachePlan,
	hostSnapshot *hostNetworkSnapshot) (toAdd, toRemove []string) {
	desired := m.postBatchPodIPSet(psAddrSet, podPlan)
	if hostSnapshot != nil && hostSnapshot.selecting {
		desired = desired.Union(hostSnapshot.addresses)
	}
	return m.computeAddressDelta(psAddrSet.addresses, desired)
}

// computePodAddressSetCachePlan builds the pod-cache plan for the given pending pod keys.
// OVN add/remove is computed later via deriveUnionAddressDelta.
func (m *AddressSetManager) computePodAddressSetCachePlan(psAddrSet *podSelectorAddressSet, podKeys sets.Set[string],
	matchedNamespaces *selectedNamespaces, selectedNodes sets.Set[string]) (*addressSetCachePlan, error) {
	plan := newAddressSetCachePlan()
	for podKey := range podKeys {
		namespace, name, splitErr := cache.SplitMetaNamespaceKey(podKey)
		if splitErr != nil {
			return nil, fmt.Errorf("failed to split pod key %q: %w", podKey, splitErr)
		}
		pod, getErr := m.podLister.Pods(namespace).Get(name)
		if getErr != nil {
			if !apierrors.IsNotFound(getErr) {
				return nil, fmt.Errorf("failed to get pod %s: %w", podKey, getErr)
			}
			plan.recordPodRemove(podKey)
			continue
		}
		keepInCache, podIPs, evalErr := m.evaluatePodCacheUpdate(psAddrSet, pod, matchedNamespaces, selectedNodes)
		if evalErr != nil {
			return nil, fmt.Errorf("failed to evaluate pod %s for address set cache plan: %w", podKey, evalErr)
		}
		if keepInCache {
			plan.recordPodSet(podKey, podIPs, pod.Spec.NodeName)
		} else {
			plan.recordPodRemove(podKey)
		}
	}
	return plan, nil
}

// listMatchingPodsInNamespace lists pods in namespace that match the address set's pod
// selector (and node selector when set). Returns nil when the namespace is not selected.
func (m *AddressSetManager) listMatchingPodsInNamespace(psAddrSet *podSelectorAddressSet, namespace string,
	matchedNamespaces *selectedNamespaces, selectedNodes sets.Set[string]) ([]*corev1.Pod, error) {
	if !matchedNamespaces.Has(namespace) {
		return nil, nil
	}
	var pods []*corev1.Pod
	var err error
	if psAddrSet.podSelector.Empty() {
		pods, err = m.podLister.Pods(namespace).List(labels.Everything())
	} else {
		pods, err = m.podLister.Pods(namespace).List(psAddrSet.podSelector)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list pods in namespace %s: %w", namespace, err)
	}
	if psAddrSet.nodeSelector == nil || psAddrSet.nodeSelector.Empty() {
		return pods, nil
	}
	filtered := make([]*corev1.Pod, 0, len(pods))
	for _, pod := range pods {
		if pod.Spec.NodeName != "" && selectedNodes.Has(pod.Spec.NodeName) {
			filtered = append(filtered, pod)
		}
	}
	return filtered, nil
}

// computeNamespaceAddressSetCachePlan builds the pod-cache plan for the affected namespaces:
// add/update pods that now match, and remove cached pods that no longer match.
// OVN add/remove is computed later via deriveUnionAddressDelta.
func (m *AddressSetManager) computeNamespaceAddressSetCachePlan(psAddrSet *podSelectorAddressSet, namespaceKeys sets.Set[string],
	matchedNamespaces *selectedNamespaces, selectedNodes sets.Set[string]) (*addressSetCachePlan, error) {
	plan := newAddressSetCachePlan()
	for ns := range namespaceKeys {
		pods, listErr := m.listMatchingPodsInNamespace(psAddrSet, ns, matchedNamespaces, selectedNodes)
		if listErr != nil {
			return nil, fmt.Errorf("failed to list matching pods in namespace %s: %w", ns, listErr)
		}
		desiredPods := sets.New[string]()
		for _, pod := range pods {
			podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
			keepInCache, podIPs, evalErr := m.evaluatePodCacheUpdate(psAddrSet, pod, matchedNamespaces, selectedNodes)
			if evalErr != nil {
				return nil, fmt.Errorf("failed to evaluate pod %s for namespace address set cache plan: %w", podKey, evalErr)
			}
			if !keepInCache {
				continue
			}
			desiredPods.Insert(podKey)
			plan.recordPodSet(podKey, podIPs, pod.Spec.NodeName)
		}
		// Drop pods that were selected last reconcile but are no longer desired in this namespace.
		if cachedPods, ok := psAddrSet.selectedPods[ns]; ok {
			for podName := range cachedPods {
				fullPodKey := fmt.Sprintf("%s/%s", ns, podName)
				if desiredPods.Has(fullPodKey) {
					continue
				}
				plan.recordPodRemove(fullPodKey)
			}
		}
	}
	return plan, nil
}

// computeNodeAddressSetCachePlan builds the pod-cache plan for pods on the affected nodes when a
// nodeSelector is in use (node label changes entering/leaving the selection).
// Updates are driven from podIndexer.ByIndex for each pending node and from podsByNode for
// targeted removals, keeping work proportional to pods on those nodes.
// OVN add/remove is computed later via deriveUnionAddressDelta.
func (m *AddressSetManager) computeNodeAddressSetCachePlan(psAddrSet *podSelectorAddressSet, nodeKeys sets.Set[string],
	matchedNamespaces *selectedNamespaces, selectedNodes sets.Set[string]) (*addressSetCachePlan, error) {
	plan := newAddressSetCachePlan()
	psAddrSet.ensurePodCacheInitialized()
	for nodeKey := range nodeKeys {
		pods, listErr := m.listPodsOnNode(psAddrSet, nodeKey, matchedNamespaces)
		if listErr != nil {
			return nil, fmt.Errorf("failed to list pods on node %s for address set cache plan: %w", nodeKey, listErr)
		}
		seenOnNode := sets.New[string]()
		for _, pod := range pods {
			podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
			seenOnNode.Insert(podKey)
			keepInCache, podIPs, evalErr := m.evaluatePodCacheUpdate(psAddrSet, pod, matchedNamespaces, selectedNodes)
			if evalErr != nil {
				return nil, fmt.Errorf("failed to evaluate pod %s for node address set cache plan: %w", podKey, evalErr)
			}
			if keepInCache {
				plan.recordPodSet(podKey, podIPs, pod.Spec.NodeName)
			} else {
				plan.recordPodRemove(podKey)
			}
		}
		// Cached pods previously recorded on this node but missing from the index listing
		// (deleted, moved, or no longer matching) need an explicit membership decision.
		for podKey := range psAddrSet.podsByNode[nodeKey] {
			if seenOnNode.Has(podKey) {
				continue
			}
			namespace, name, splitErr := cache.SplitMetaNamespaceKey(podKey)
			if splitErr != nil {
				return nil, fmt.Errorf("failed to split pod key %q: %w", podKey, splitErr)
			}
			pod, getErr := m.podLister.Pods(namespace).Get(name)
			if getErr != nil {
				if apierrors.IsNotFound(getErr) {
					plan.recordPodRemove(podKey)
					continue
				}
				return nil, fmt.Errorf("failed to get pod %s: %w", podKey, getErr)
			}
			keepInCache, podIPs, evalErr := m.evaluatePodCacheUpdate(psAddrSet, pod, matchedNamespaces, selectedNodes)
			if evalErr != nil {
				return nil, fmt.Errorf("failed to evaluate pod %s for node address set cache plan: %w", podKey, evalErr)
			}
			if keepInCache {
				plan.recordPodSet(podKey, podIPs, pod.Spec.NodeName)
			} else {
				plan.recordPodRemove(podKey)
			}
		}
	}
	return plan, nil
}

// listPodsOnNode returns pods on nodeName that match the address set's namespace and pod selectors.
func (m *AddressSetManager) listPodsOnNode(psAddrSet *podSelectorAddressSet, nodeName string,
	matchedNamespaces *selectedNamespaces) ([]*corev1.Pod, error) {
	objs, err := m.podIndexer.ByIndex(podNodeNameIndex, nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to list pods on node %s: %w", nodeName, err)
	}
	pods := make([]*corev1.Pod, 0, len(objs))
	for _, obj := range objs {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			continue
		}
		if !matchedNamespaces.Has(pod.Namespace) {
			continue
		}
		if !psAddrSet.podSelector.Empty() && !psAddrSet.podSelector.Matches(labels.Set(pod.Labels)) {
			continue
		}
		pods = append(pods, pod)
	}
	return pods, nil
}

// listAllMatchingPods returns every pod that matches the address set's namespace, pod, and
// (when set) node selectors. Used for full-sync builds.
func (m *AddressSetManager) listAllMatchingPods(psAddrSet *podSelectorAddressSet, matchedNamespaces *selectedNamespaces,
	selectedNodes sets.Set[string]) ([]*corev1.Pod, error) {
	var pods []*corev1.Pod
	if matchedNamespaces.all {
		var err error
		if psAddrSet.podSelector.Empty() {
			pods, err = m.podLister.List(labels.Everything())
		} else {
			pods, err = m.podLister.List(psAddrSet.podSelector)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to list pods: %w", err)
		}
	} else {
		for ns := range matchedNamespaces.set {
			nsPods, err := m.listMatchingPodsInNamespace(psAddrSet, ns, matchedNamespaces, selectedNodes)
			if err != nil {
				return nil, fmt.Errorf("failed to list matching pods in namespace %s: %w", ns, err)
			}
			pods = append(pods, nsPods...)
		}
	}
	if psAddrSet.nodeSelector == nil || psAddrSet.nodeSelector.Empty() {
		return pods, nil
	}
	filtered := make([]*corev1.Pod, 0, len(pods))
	for _, pod := range pods {
		if pod.Spec.NodeName != "" && selectedNodes.Has(pod.Spec.NodeName) {
			filtered = append(filtered, pod)
		}
	}
	return filtered, nil
}

// computeFullAddressSetDelta builds the complete desired address set (all matching pod IPs
// plus host-network IPs when applicable) and diffs it against the in-memory addresses mirror.
func (m *AddressSetManager) computeFullAddressSetDelta(psAddrSet *podSelectorAddressSet, matchedNamespaces *selectedNamespaces,
	selectedNodes sets.Set[string], key string) (toAdd, toRemove []string, plan *addressSetCachePlan, err error) {
	pods, err := m.listAllMatchingPods(psAddrSet, matchedNamespaces, selectedNodes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to list matching pods for address set %s: %w", key, err)
	}

	podSnapshot, err := m.buildPodCacheSnapshot(psAddrSet, pods, matchedNamespaces, selectedNodes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to build pod cache snapshot for address set %s: %w", key, err)
	}

	desiredAddresses := sets.New[string]()
	for _, ips := range podSnapshot.podAddresses {
		desiredAddresses.Insert(ips...)
	}

	hostSnapshot := m.buildHostNetworkSnapshot(psAddrSet, matchedNamespaces, key)
	if hostSnapshot != nil && hostSnapshot.selecting {
		desiredAddresses.Insert(hostSnapshot.addresses.UnsortedList()...)
	}

	currentAddresses := psAddrSet.addresses
	toAdd, toRemove = m.computeAddressDelta(currentAddresses, desiredAddresses)
	plan = &addressSetCachePlan{
		podCacheReplace: podSnapshot,
	}
	return toAdd, toRemove, plan, nil
}

// buildPodCacheSnapshot builds a point-in-time pod membership snapshot from the given pods.
func (m *AddressSetManager) buildPodCacheSnapshot(psAddrSet *podSelectorAddressSet, pods []*corev1.Pod,
	matchedNamespaces *selectedNamespaces, selectedNodes sets.Set[string]) (*addressSetPodCacheSnapshot, error) {
	snapshot := &addressSetPodCacheSnapshot{
		selectedPods: make(map[string]sets.Set[string]),
		podAddresses: make(map[string][]string),
		podNodes:     make(map[string]string),
	}
	for _, pod := range pods {
		if !m.podMatchesAddressSet(pod, psAddrSet, matchedNamespaces, selectedNodes) {
			continue
		}
		podIPs, podErr := m.getPodIPsForPod(pod, psAddrSet.netInfo, psAddrSet.legacyNetpolMode)
		if podErr != nil {
			return nil, fmt.Errorf("failed to get IPs for pod %s/%s while building cache snapshot: %w", pod.Namespace, pod.Name, podErr)
		}
		podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
		snapshot.podAddresses[podKey] = slices.Clone(podIPs)
		snapshot.podNodes[podKey] = pod.Spec.NodeName
		if snapshot.selectedPods[pod.Namespace] == nil {
			snapshot.selectedPods[pod.Namespace] = sets.New[string]()
		}
		snapshot.selectedPods[pod.Namespace].Insert(pod.Name)
	}
	return snapshot, nil
}

// buildHostNetworkSnapshot snapshots host-network IPs from hostNetworkNamespaceIPsPerNode
// and registers selector membership so node IP updates during an in-flight OVN apply
// still enqueue this address set for reconcile.
func (m *AddressSetManager) buildHostNetworkSnapshot(psAddrSet *podSelectorAddressSet,
	matchedNamespaces *selectedNamespaces, key string) *hostNetworkSnapshot {
	if !(psAddrSet.legacyNetpolMode && psAddrSet.netInfo.IsDefault() && config.Kubernetes.HostNetworkNamespace != "" &&
		psAddrSet.podSelector.Empty()) {
		return nil
	}
	m.hostNetworkNamespaceLock.Lock()
	defer m.hostNetworkNamespaceLock.Unlock()
	selecting := m.hostNetworkNamespaceExists && matchedNamespaces.Has(config.Kubernetes.HostNetworkNamespace)
	if selecting {
		m.hostNetworkSelectingAddrSets.Insert(key)
	} else {
		m.hostNetworkSelectingAddrSets.Delete(key)
	}
	addresses := sets.New[string]()
	if selecting {
		for _, hostnetIPs := range m.hostNetworkNamespaceIPsPerNode {
			addresses.Insert(hostnetIPs...)
		}
	}
	return &hostNetworkSnapshot{
		selecting: selecting,
		addresses: addresses,
	}
}

// applyAddressSetCachePlan applies a precomputed pod-cache mutation after a successful OVN apply.
func (m *AddressSetManager) applyAddressSetCachePlan(psAddrSet *podSelectorAddressSet, plan *addressSetCachePlan) {
	if plan == nil {
		return
	}
	if plan.podCacheReplace != nil {
		psAddrSet.podAddresses = clonePodAddressesMap(plan.podCacheReplace.podAddresses)
		psAddrSet.selectedPods = cloneSelectedPodsMap(plan.podCacheReplace.selectedPods)
		psAddrSet.podNodes = clonePodNodesMap(plan.podCacheReplace.podNodes)
		psAddrSet.podsByNode = buildPodsByNode(psAddrSet.podNodes)
	} else {
		for podKey := range plan.podRemoves {
			psAddrSet.removePodFromCache(podKey)
		}
		for podKey, entry := range plan.podSets {
			psAddrSet.addPodToCache(podKey, entry.ips, entry.nodeName)
		}
	}
}

func clonePodAddressesMap(src map[string][]string) map[string][]string {
	if src == nil {
		return make(map[string][]string)
	}
	dst := make(map[string][]string, len(src))
	for podKey, ips := range src {
		dst[podKey] = slices.Clone(ips)
	}
	return dst
}

func cloneSelectedPodsMap(src map[string]sets.Set[string]) map[string]sets.Set[string] {
	if src == nil {
		return make(map[string]sets.Set[string])
	}
	dst := make(map[string]sets.Set[string], len(src))
	for ns, pods := range src {
		dst[ns] = pods.Clone()
	}
	return dst
}

func clonePodNodesMap(src map[string]string) map[string]string {
	if src == nil {
		return make(map[string]string)
	}
	dst := make(map[string]string, len(src))
	for podKey, nodeName := range src {
		dst[podKey] = nodeName
	}
	return dst
}

func buildPodsByNode(podNodes map[string]string) map[string]sets.Set[string] {
	podsByNode := make(map[string]sets.Set[string])
	for podKey, nodeName := range podNodes {
		if nodeName == "" {
			continue
		}
		if podsByNode[nodeName] == nil {
			podsByNode[nodeName] = sets.New[string]()
		}
		podsByNode[nodeName].Insert(podKey)
	}
	return podsByNode
}

// replaceAddressSetContents performs a one-shot full replace of OVN address-set contents and
// initializes the in-memory mirrors. Used on first ensure when addresses is nil so ACLs that
// already reference the set are populated immediately.
func (m *AddressSetManager) replaceAddressSetContents(psAddrSet *podSelectorAddressSet, matchedNamespaces *selectedNamespaces,
	selectedNodes sets.Set[string], key string) error {
	pods, err := m.listAllMatchingPods(psAddrSet, matchedNamespaces, selectedNodes)
	if err != nil {
		return fmt.Errorf("failed to list matching pods while replacing address set %s: %w", key, err)
	}

	podSnapshot, err := m.buildPodCacheSnapshot(psAddrSet, pods, matchedNamespaces, selectedNodes)
	if err != nil {
		return fmt.Errorf("failed to build pod cache snapshot while replacing address set %s: %w", key, err)
	}

	desiredAddresses := []string{}
	for _, ips := range podSnapshot.podAddresses {
		desiredAddresses = append(desiredAddresses, ips...)
	}

	hostSnapshot := m.buildHostNetworkSnapshot(psAddrSet, matchedNamespaces, key)
	if hostSnapshot != nil && hostSnapshot.selecting {
		desiredAddresses = append(desiredAddresses, hostSnapshot.addresses.UnsortedList()...)
	}
	desiredAddresses = addressset.GetUniqueAddresses(desiredAddresses)

	if err := psAddrSet.addressSet.SetAddresses(desiredAddresses); err != nil {
		return fmt.Errorf("failed to set addresses while replacing address set %s: %w", key, err)
	}
	psAddrSet.addresses = sets.New(desiredAddresses...)
	m.applyAddressSetCachePlan(psAddrSet, &addressSetCachePlan{
		podCacheReplace: podSnapshot,
	})
	return nil
}

type selectedNamespaces struct {
	set sets.Set[string]
	// if true, it means all namespaces are selected, so set won't be populated
	all bool
}

func (s *selectedNamespaces) Has(namespace string) bool {
	return s.all || s.set.Has(namespace)
}

// getSelectedNamespaces returns a set of namespaces that should be selected for a given podSelectorAddressSet.
// nil set means no namespace selector is set and all namespaces match.
func (m *AddressSetManager) getSelectedNamespaces(s *podSelectorAddressSet) (*selectedNamespaces, error) {
	matchedNamespaces := &selectedNamespaces{
		set: sets.New[string](),
	}
	if s.namespace != "" {
		// static namespace case
		matchedNamespaces.set.Insert(s.namespace)
	} else if s.namespaceSelector.Empty() {
		// any namespace
		matchedNamespaces.all = true
	} else {
		// selected namespaces
		namespaces, err := m.namespaceLister.List(s.namespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to list namespaces: %v", err)
		}
		for _, ns := range namespaces {
			matchedNamespaces.set.Insert(ns.Name)
		}
	}
	return matchedNamespaces, nil
}

// getSelectedNodes returns the set of node names that match the node selector.
func (m *AddressSetManager) getSelectedNodes(nodeSelector labels.Selector) (sets.Set[string], error) {
	nodes, err := m.nodeLister.List(nodeSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %v", err)
	}
	names := sets.New[string]()
	for _, n := range nodes {
		names.Insert(n.Name)
	}
	return names, nil
}

func (m *AddressSetManager) getPodIPs(pods []*corev1.Pod, netInfo util.NetInfo, noHostNetwork bool) ([]string, error) {
	ips := []string{}
	for _, pod := range pods {
		if noHostNetwork && pod.Spec.HostNetwork {
			// skip hostNetwork pods if requested, since they are not selected in legacyNetpolMode
			continue
		}
		if pod.Annotations[ovntypes.OvnPodAnnotationName] == "" && len(pod.Status.PodIPs) == 0 {
			// pod doesn't have IPs yet, skip it
			continue
		}
		// handle completed pods as deleted since their IPs may be already released and re-allocated to other pods
		// due to retry framework logic
		if util.PodCompleted(pod) {
			continue
		}
		podIPs, err := util.GetPodIPsOfNetwork(pod, netInfo, m.getNetworkNameForNADKey)
		if err != nil {
			// not finding pod IPs on a remote pod is common until the other node wires the pod, suppress it
			return nil, ovntypes.NewSuppressedError(err)
		}
		ips = append(ips, util.StringSlice(podIPs)...)
	}
	return ips, nil
}

func GetPodSelectorAddrSetDbIDs(podSelector, namespaceSelector, nodeSelector *metav1.LabelSelector, namespace, controller string, legacyNetpolMode bool) *libovsdbops.DbObjectIDs {
	nodeSelector = normalizeNodeSelector(nodeSelector)
	addrsetKey := getPodSelectorKey(podSelector, namespaceSelector, nodeSelector, namespace, legacyNetpolMode)
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetPodSelector, controller, map[libovsdbops.ExternalIDKey]string{
		// pod selector address sets are cluster-scoped, only need name
		libovsdbops.ObjectNameKey: addrsetKey,
	})
}

// sortedLSRString is based on *LabelSelectorRequirement.String(),
// but adds sorting for Values
func sortedLSRString(lsr *metav1.LabelSelectorRequirement) string {
	if lsr == nil {
		return "nil"
	}
	lsrValues := make([]string, 0, len(lsr.Values))
	lsrValues = append(lsrValues, lsr.Values...)
	sort.Strings(lsrValues)
	s := strings.Join([]string{`LSR{`,
		`Key:` + fmt.Sprintf("%v", lsr.Key) + `,`,
		`Operator:` + fmt.Sprintf("%v", lsr.Operator) + `,`,
		`Values:` + fmt.Sprintf("%v", lsrValues) + `,`,
		`}`,
	}, "")
	return s
}

// shortLabelSelectorString is based on *LabelSelector.String(),
// but makes sure to generate the same string for equivalent selectors (by additional sorting).
// It also tries to reduce return string length, since this string will be put to the db ad ExternalID.
func shortLabelSelectorString(sel *metav1.LabelSelector) string {
	if sel == nil {
		return "nil"
	}
	var repeatedStringForMatchExpressions, mapStringForMatchLabels string
	if len(sel.MatchExpressions) > 0 {
		repeatedStringForMatchExpressions = "ME:{"
		matchExpressions := make([]string, 0, len(sel.MatchExpressions))
		for _, f := range sel.MatchExpressions {
			matchExpressions = append(matchExpressions, sortedLSRString(&f))
		}
		// sort match expressions to not depend on MatchExpressions order
		sort.Strings(matchExpressions)
		repeatedStringForMatchExpressions += strings.Join(matchExpressions, ",")
		repeatedStringForMatchExpressions += "}"
	} else {
		repeatedStringForMatchExpressions = ""
	}
	keysForMatchLabels := make([]string, 0, len(sel.MatchLabels))
	for k := range sel.MatchLabels {
		keysForMatchLabels = append(keysForMatchLabels, k)
	}
	sort.Strings(keysForMatchLabels)
	if len(keysForMatchLabels) > 0 {
		mapStringForMatchLabels = "ML:{"
		for _, k := range keysForMatchLabels {
			mapStringForMatchLabels += fmt.Sprintf("%v: %v,", k, sel.MatchLabels[k])
		}
		mapStringForMatchLabels += "}"
	} else {
		mapStringForMatchLabels = ""
	}
	s := "LS{"
	if mapStringForMatchLabels != "" {
		s += mapStringForMatchLabels + ","
	}
	if repeatedStringForMatchExpressions != "" {
		s += repeatedStringForMatchExpressions + ","
	}
	s += "}"
	return s
}

// Since we have joined this manager for multiple controllers, we need to make keys unique across controllers.
// In the db it is already achieved by using controller name in ExternalIDs, but for internal map we also need to add controller name
func getInternalKey(podSelector, namespaceSelector, nodeSelector *metav1.LabelSelector, namespace, controllerName string, legacyNetpolMode bool) string {
	return controllerName + "_" + getPodSelectorKey(podSelector, namespaceSelector, nodeSelector, namespace, legacyNetpolMode)
}

func getPodSelectorKey(podSelector, namespaceSelector, nodeSelector *metav1.LabelSelector, namespace string, legacyNetpolMode bool) string {
	var namespaceKey string
	if namespaceSelector == nil {
		// namespace is static
		namespaceKey = namespace
	} else {
		namespaceKey = shortLabelSelectorString(namespaceSelector)
	}
	key := namespaceKey + "_" + shortLabelSelectorString(podSelector)
	if nodeSelector != nil {
		key += "_" + shortLabelSelectorString(nodeSelector)
	}
	if legacyNetpolMode {
		return key + "_LNM"
	} else {
		return key
	}
}

func normalizeNodeSelector(sel *metav1.LabelSelector) *metav1.LabelSelector {
	if sel != nil && len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0 {
		return nil
	}
	return sel
}
