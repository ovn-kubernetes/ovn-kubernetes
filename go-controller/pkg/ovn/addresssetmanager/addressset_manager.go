// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package addresssetmanager

import (
	"fmt"
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
	selectedNamespaces sets.Set[string]
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

	podLister       listers.PodLister
	namespaceLister listers.NamespaceLister
	nodeLister      listers.NodeLister

	podController        controller.Controller
	nsController         controller.Controller
	nodeController       controller.Controller
	addressSetReconciler controller.Reconciler

	// All network controllers are getting this function from the same networkmanager, so we can share it
	getNetworkNameForNADKey func(nadKey string) string

	// both hostNetworkNamespaceIps and hostNetworkSelectingAddrSets are protected by the same lock.
	// can only be taken after the addressSets key lock and never vice versa to avoid deadlocks.
	hostNetworkNamespaceLock sync.RWMutex
	// local cache of HostNetworkNamespace address set IPs
	hostNetworkNamespaceIps []string
	// local cache of address sets that select HostNetworkNamespace
	hostNetworkSelectingAddrSets sets.Set[string]
}

func NewAddressSetManager(podInformer coreinformers.PodInformer, namespaceInformer coreinformers.NamespaceInformer,
	nodeInformer coreinformers.NodeInformer, nbClient libovsdbclient.Client, getNetworkNameForNADKey func(nadKey string) string) *AddressSetManager {
	m := &AddressSetManager{
		name:                         "pod-selector-address-set-manager",
		nbClient:                     nbClient,
		addressSetFactoryV4:          addressset.NewOvnAddressSetFactory(nbClient, true, false),
		addressSetFactoryV6:          addressset.NewOvnAddressSetFactory(nbClient, false, true),
		addressSetFactoryDualstack:   addressset.NewOvnAddressSetFactory(nbClient, true, true),
		addressSets:                  syncmap.NewSyncMap[*podSelectorAddressSet](),
		podLister:                    podInformer.Lister(),
		namespaceLister:              namespaceInformer.Lister(),
		nodeLister:                 nodeInformer.Lister(),
		getNetworkNameForNADKey:      getNetworkNameForNADKey,
		hostNetworkSelectingAddrSets: sets.New[string](),
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
	return m
}

func (m *AddressSetManager) Start() error {
	klog.Infof("Starting %s controller", m.name)
	return controller.StartWithInitialSync(m.initialSync, m.podController, m.nsController, m.nodeController, m.addressSetReconciler)
}

func (m *AddressSetManager) Stop() {
	klog.Infof("Stopping %s controller", m.name)
	controller.Stop(m.podController, m.nsController, m.nodeController, m.addressSetReconciler)
}

func (m *AddressSetManager) initialSync() error {
	return libovsdbutil.DeleteAddrSetsWithoutACLRefAnyController(libovsdbops.AddressSetPodSelector, m.nbClient)
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

	err = m.addressSets.DoWithLock(addrSetKey, func(key string) error {
		psAddrSet, found := m.addressSets.Load(key)
		if !found {
			addrSetDbIDs := GetPodSelectorAddrSetDbIDs(podSelector, namespaceSelector, nodeSelector, namespace, controllerName, legacyNetpolMode)
			ipv4Mode, ipv6Mode := netInfo.IPMode()
			var addrSet addressset.AddressSet
			switch {
			case ipv4Mode && !ipv6Mode:
				addrSet, err = m.addressSetFactoryV4.NewAddressSet(addrSetDbIDs, nil)
			case !ipv4Mode && ipv6Mode:
				addrSet, err = m.addressSetFactoryV6.NewAddressSet(addrSetDbIDs, nil)
			case ipv4Mode && ipv6Mode:
				addrSet, err = m.addressSetFactoryDualstack.NewAddressSet(addrSetDbIDs, nil)
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
			}
			m.addressSets.LoadOrStore(key, psAddrSet)
			// this only puts key to the queue, no lock
			m.addressSetReconciler.Reconcile(key)
		}
		// psAddrSet is successfully init-ed
		psAddrSet.backRefs[backRef] = true
		psAddrSetHashV4, psAddrSetHashV6 = psAddrSet.addressSet.GetASHashNames()
		return nil
	})
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
				return err
			}
			m.addressSets.Delete(key)
			m.hostNetworkNamespaceLock.Lock()
			m.hostNetworkSelectingAddrSets.Delete(key)
			m.hostNetworkNamespaceLock.Unlock()
		}
		return nil
	})
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
		return new.Annotations[util.OvnPodAnnotationName] != "" || len(new.Status.PodIPs) > 0
	}
	if new.Annotations[util.OvnPodAnnotationName] != old.Annotations[util.OvnPodAnnotationName] {
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
					m.addressSetReconciler.Reconcile(addrSetKey)
					return nil
				}
				return nil
			}
			// only check address sets that have previously matched pod's namespace to avoid extra reconciliations
			previouslyMatchedNamespaces := addrSet.selectedNamespaces
			if previouslyMatchedNamespaces == nil || previouslyMatchedNamespaces.Has(namespace) {
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

func (m *AddressSetManager) reconcileNamespace(nsKey string) error {
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
			if currentlyMatchedNamespaces == nil || currentlyMatchedNamespaces.Has(nsKey) {
				// this namespace is relevant for this address set, reconcile
				m.addressSetReconciler.Reconcile(addrSetKey)
				return nil
			}
			// now check if this address set was matching this namespace before, if yes, reconcile since it might not match anymore
			previouslyMatchedNamespaces := addrSet.selectedNamespaces
			if previouslyMatchedNamespaces == nil || previouslyMatchedNamespaces.Has(nsKey) {
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
	return false
}

func (m *AddressSetManager) reconcileNode(nodeKey string) error {
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
				// this node is relevant for this address set, reconcile
				m.addressSetReconciler.Reconcile(addrSetKey)
				return nil
			}
			// reconcile the address set if the node matches the previous selected nodes
			previouslyMatchedNodes := addrSet.selectedNodes
			// previouslyMatchedNodes == nil means the address set hasn't been reconciled yet, so need to reconcile
			if previouslyMatchedNodes == nil || previouslyMatchedNodes.Has(nodeKey) {
				m.addressSetReconciler.Reconcile(addrSetKey)
				return nil
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("failed to reconcile address set %s for node %s: %v", addrSetKey, nodeKey, err)
		}
	}
	return nil
}

func (m *AddressSetManager) SetHostNetworkNamespaceIPs(ips []string) {
	m.hostNetworkNamespaceLock.Lock()
	defer m.hostNetworkNamespaceLock.Unlock()
	m.hostNetworkNamespaceIps = ips
	for addrSetKey := range m.hostNetworkSelectingAddrSets {
		m.addressSetReconciler.Reconcile(addrSetKey)
	}
}

func (m *AddressSetManager) AddHostNetworkNamespaceIPs(ips []string) {
	m.hostNetworkNamespaceLock.Lock()
	defer m.hostNetworkNamespaceLock.Unlock()
	updated := false
	for _, ip := range ips {
		if !slices.Contains(m.hostNetworkNamespaceIps, ip) {
			updated = true
			m.hostNetworkNamespaceIps = append(m.hostNetworkNamespaceIps, ip)
		}
	}
	if !updated {
		return
	}
	for addrSetKey := range m.hostNetworkSelectingAddrSets {
		m.addressSetReconciler.Reconcile(addrSetKey)
	}
}

func (m *AddressSetManager) DeleteHostNetworkNamespaceIPs(ips []string) {
	m.hostNetworkNamespaceLock.Lock()
	defer m.hostNetworkNamespaceLock.Unlock()
	updated := false
	for _, ip := range ips {
		ipIdx := slices.Index(m.hostNetworkNamespaceIps, ip)
		if ipIdx == -1 {
			continue
		}
		m.hostNetworkNamespaceIps = slices.Delete(m.hostNetworkNamespaceIps, ipIdx, ipIdx+1)
		updated = true
	}
	if !updated {
		return
	}
	for addrSetKey := range m.hostNetworkSelectingAddrSets {
		m.addressSetReconciler.Reconcile(addrSetKey)
	}
}

func (m *AddressSetManager) reconcileAddressSet(key string) error {
	return m.addressSets.DoWithLock(key, func(key string) error {
		psAddrSet, found := m.addressSets.Load(key)
		if !found {
			return nil
		}
		matchedNamespaces, err := m.getSelectedNamespaces(psAddrSet)
		if err != nil {
			return fmt.Errorf("failed to get selected namespaces for address set %s: %v", key, err)
		}
		var pods []*corev1.Pod
		if matchedNamespaces == nil {
			// no namespace selector, use pod selector only
			if psAddrSet.podSelector.Empty() {
				// all cluster pods
				pods, err = m.podLister.List(labels.Everything())
				if err != nil {
					return fmt.Errorf("failed to list pods: %v", err)
				}
			} else {
				// global pod selector
				pods, err = m.podLister.List(psAddrSet.podSelector)
				if err != nil {
					return fmt.Errorf("failed to list pods: %v", err)
				}
			}
		} else {
			// namespace selector is set, apply pod selector in every namespace
			for ns := range matchedNamespaces {
				if psAddrSet.podSelector.Empty() {
					// empty selector means no filtering, select all pods in a given namespace
					nsPods, err := m.podLister.Pods(ns).List(labels.Everything())
					if err != nil {
						return fmt.Errorf("failed to list pods in namespace %s: %v", ns, err)
					}
					pods = append(pods, nsPods...)
				} else {
					// namespaced pod selector, select matching pods in a given namespace
					nsPods, err := m.podLister.Pods(ns).List(psAddrSet.podSelector)
					if err != nil {
						return fmt.Errorf("failed to list pods in namespace %s: %v", ns, err)
					}
					pods = append(pods, nsPods...)
				}
			}
		}
		// apply node selector filter if it's not empty
		if psAddrSet.nodeSelector != nil && !psAddrSet.nodeSelector.Empty() {
			selectedNodes, err := m.getSelectedNodes(psAddrSet.nodeSelector)
			if err != nil {
				return fmt.Errorf("failed to get selected nodes for address set %s: %v", key, err)
			}
			filtered := make([]*corev1.Pod, 0, len(pods))
			for _, pod := range pods {
				if pod.Spec.NodeName != "" && selectedNodes.Has(pod.Spec.NodeName) {
					filtered = append(filtered, pod)
				}
			}
			pods = filtered
			psAddrSet.selectedNodes = selectedNodes
		}
		ips, err := m.getPodIPs(pods, psAddrSet.netInfo, psAddrSet.legacyNetpolMode)
		if err != nil {
			return fmt.Errorf("failed to get pod IPs: %v", err)
		}
		// now check if this address set should add hostNetworkNamespace IPs
		// it only makes sense for the default network
		if psAddrSet.legacyNetpolMode && psAddrSet.netInfo.IsDefault() && config.Kubernetes.HostNetworkNamespace != "" &&
			psAddrSet.podSelector.Empty() {
			// update m.hostNetworkSelectingAddrSets
			m.hostNetworkNamespaceLock.Lock()
			if matchedNamespaces == nil || matchedNamespaces.Has(config.Kubernetes.HostNetworkNamespace) {
				ips = append(ips, m.hostNetworkNamespaceIps...)
				m.hostNetworkSelectingAddrSets.Insert(key)
			} else {
				m.hostNetworkSelectingAddrSets.Delete(key)
			}
			m.hostNetworkNamespaceLock.Unlock()
		}

		// this operation doesn't check the contents on the address set and will run a db transaction
		// every time, may be improved.
		err = psAddrSet.addressSet.SetAddresses(ips)
		if err != nil {
			return fmt.Errorf("failed to set addresses for address set %s: %v", key, err)
		}
		psAddrSet.selectedNamespaces = matchedNamespaces
		return nil
	})
}

// getSelectedNamespaces returns a set of namespaces that should be selected for a given podSelectorAddressSet.
// nil set means no namespace selector is set and all namespaces match.
func (m *AddressSetManager) getSelectedNamespaces(s *podSelectorAddressSet) (sets.Set[string], error) {
	matchedNamespaces := sets.New[string]()
	if s.namespace != "" {
		// static namespace case
		matchedNamespaces.Insert(s.namespace)
	} else if s.namespaceSelector.Empty() {
		// any namespace
		matchedNamespaces = nil
	} else {
		// selected namespaces
		namespaces, err := m.namespaceLister.List(s.namespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to list namespaces: %v", err)
		}
		for _, ns := range namespaces {
			matchedNamespaces.Insert(ns.Name)
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
		if pod.Annotations[util.OvnPodAnnotationName] == "" && len(pod.Status.PodIPs) == 0 {
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
