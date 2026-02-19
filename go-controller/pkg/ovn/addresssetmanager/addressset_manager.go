package addresssetmanager

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// PodSelectorAddressSet stores address set for modifications and used selectors that define this address set.
type PodSelectorAddressSet struct {
	// backRefs is a map of objects that use this address set.
	// keys must be unique for all possible users, e.g. for NetworkPolicy use (np *networkPolicy) getKeyWithKind().
	// Must only be changed with oc.podSelectorAddressSets Lock.
	backRefs map[string]bool

	podSelector       labels.Selector
	namespaceSelector labels.Selector
	// namespace is used when namespaceSelector is nil to set static namespace
	namespace string

	addressSet addressset.AddressSet

	// selectedNamespaces is a cache for namespaces that were selected by this address set during the last reconciliation
	// used to optimize events processing.
	selectedNamespaces sets.Set[string]
}

// AddressSetManager manages shared address sets with pod IPs based on provided pod and namespace selectors.
type AddressSetManager struct {
	name     string
	nbClient libovsdbclient.Client

	addressSetFactory addressset.AddressSetFactory

	// addressSets stores all currently used address sets.
	addressSets *syncmap.SyncMap[*PodSelectorAddressSet]

	podLister       listers.PodLister
	namespaceLister listers.NamespaceLister

	podController        controller.Controller
	nsController         controller.Controller
	addressSetReconciler controller.Reconciler

	// network-specific fields
	// Warning: when we switch to the global manager instead of per-network
	// these attributes will be needed per address set request, but also make sure to update
	// getPodSelectorKey to include controllerName, fortunately it is already in the dbIDs and they don't need to change.
	controllerName          string
	netInfo                 util.NetInfo
	getNetworkNameForNADKey func(nadKey string) string
}

func NewAddressSetManager(podInformer coreinformers.PodInformer, namespaceInformer coreinformers.NamespaceInformer,
	nbClient libovsdbclient.Client, factory addressset.AddressSetFactory, controllerName string,
	netInfo util.NetInfo, getNetworkNameForNADKey func(nadKey string) string) *AddressSetManager {
	m := &AddressSetManager{
		name:                    "pod-selector-address-set-manager",
		nbClient:                nbClient,
		addressSetFactory:       factory,
		addressSets:             syncmap.NewSyncMap[*PodSelectorAddressSet](),
		podLister:               podInformer.Lister(),
		namespaceLister:         namespaceInformer.Lister(),
		controllerName:          controllerName,
		netInfo:                 netInfo,
		getNetworkNameForNADKey: getNetworkNameForNADKey,
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

	// addressSetReconciler is fed from the pod and namespace controllers
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
	return controller.StartWithInitialSync(m.initialSync, m.podController, m.nsController, m.addressSetReconciler)
}

func (m *AddressSetManager) Stop() {
	klog.Infof("Stopping %s controller", m.name)
	controller.Stop(m.podController, m.nsController, m.addressSetReconciler)
}

func (m *AddressSetManager) initialSync() error {
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetPodSelector, m.controllerName, nil)
	return libovsdbutil.DeleteAddrSetsWithoutACLRef(predicateIDs, m.nbClient)
}

// EnsureAddressSet returns address set for requested (podSelector, namespaceSelector, namespace).
// If namespaceSelector is nil, namespace will be used with podSelector statically.
// podSelector should not be nil, use metav1.LabelSelector{} to match all pods.
// namespaceSelector can only be nil when namespace is set, use metav1.LabelSelector{} to match all namespaces.
// podSelector = metav1.LabelSelector{} + static namespace may be replaced with namespace address set,
// podSelector = metav1.LabelSelector{} + namespaceSelector may be replaced with a set of namespace address sets,
// but both cases will work here too.
//
// backRef is the key that should be used for cleanup.
// psAddrSetHashV4, psAddrSetHashV6 may be set to empty string if address set for that ipFamily wasn't created.
func (m *AddressSetManager) EnsureAddressSet(podSelector, namespaceSelector *metav1.LabelSelector,
	namespace, backRef string) (addrSetKey, psAddrSetHashV4, psAddrSetHashV6 string, err error) {
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
	var nsSel, podSel labels.Selector
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
	addrSetKey = getPodSelectorKey(podSelector, namespaceSelector, namespace)
	err = m.addressSets.DoWithLock(addrSetKey, func(key string) error {
		psAddrSet, found := m.addressSets.Load(key)
		if !found {
			addrSetDbIDs := getPodSelectorAddrSetDbIDs(addrSetKey, m.controllerName)
			addrSet, err := m.addressSetFactory.NewAddressSet(addrSetDbIDs, nil)
			// if the first step of creating address set fails, return error since there is nothing to cleanup
			if err != nil {
				return err
			}
			psAddrSet = &PodSelectorAddressSet{
				backRefs:          map[string]bool{},
				podSelector:       podSel,
				namespaceSelector: nsSel,
				namespace:         namespace,
				addressSet:        addrSet,
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
			// only delete from map after successful cleanup
			m.addressSets.Delete(key)
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
	namespace, _, err := cache.SplitMetaNamespaceKey(podKey)
	if err != nil {
		return fmt.Errorf("failed to split meta namespace key %q: %v", podKey, err)
	}
	// only reconcile if this pod is in a namespace that is selected by an address set
	// Get all existing keys, then lock address sets per key and check if they are affected.
	// If the new keys are added, it will always call reconcile for that new key, so there is no race.
	// If some keys are deleted, we just ignore it.
	existingAddrSets := m.addressSets.GetKeys()
	for _, addrSetKey := range existingAddrSets {
		// never returns error
		_ = m.addressSets.DoWithLock(addrSetKey, func(addrSetKey string) error {
			addrSet, found := m.addressSets.Load(addrSetKey)
			if !found {
				// nothing to do
				return nil
			}
			// only check address sets that have previously matched pod's namespace to avoid extra reconciliations
			previouslyMatchedNamespaces := addrSet.selectedNamespaces
			if previouslyMatchedNamespaces == nil || previouslyMatchedNamespaces.Has(namespace) {
				m.addressSetReconciler.Reconcile(addrSetKey)
				return nil
			}
			return nil
		})
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
		ips, err := m.getPodIPs(pods)
		if err != nil {
			return fmt.Errorf("failed to get pod IPs: %v", err)
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

// getSelectedNamespaces returns a set of namespaces that should be selected for a given PodSelectorAddressSet.
// nil set means no namespace selector is set and all namespaces match.
func (m *AddressSetManager) getSelectedNamespaces(s *PodSelectorAddressSet) (sets.Set[string], error) {
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

func (m *AddressSetManager) getPodIPs(pods []*corev1.Pod) ([]string, error) {
	ips := []string{}
	for _, pod := range pods {
		if pod.Annotations[util.OvnPodAnnotationName] == "" && len(pod.Status.PodIPs) == 0 {
			// pod doesn't have IPs yet, skip it
			continue
		}
		// handle completed pods as deleted since their IPs may be already released and re-allocated to other pods
		// due to retry framework logic
		if util.PodCompleted(pod) {
			continue
		}
		podIPs, err := util.GetPodIPsOfNetwork(pod, m.netInfo, m.getNetworkNameForNADKey)
		if err != nil {
			// not finding pod IPs on a remote pod is common until the other node wires the pod, suppress it
			return nil, ovntypes.NewSuppressedError(err)
		}
		ips = append(ips, util.StringSlice(podIPs)...)
	}
	return ips, nil
}

func getPodSelectorAddrSetDbIDs(psasKey, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetPodSelector, controller, map[libovsdbops.ExternalIDKey]string{
		// pod selector address sets are cluster-scoped, only need name
		libovsdbops.ObjectNameKey: psasKey,
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

func getPodSelectorKey(podSelector, namespaceSelector *metav1.LabelSelector, namespace string) string {
	var namespaceKey string
	if namespaceSelector == nil {
		// namespace is static
		namespaceKey = namespace
	} else {
		namespaceKey = shortLabelSelectorString(namespaceSelector)
	}
	return namespaceKey + "_" + shortLabelSelectorString(podSelector)
}
