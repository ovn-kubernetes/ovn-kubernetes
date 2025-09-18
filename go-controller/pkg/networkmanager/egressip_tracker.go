package networkmanager

import (
	"fmt"
	"reflect"
	"sync"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressiplisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/listers/egressip/v1"
)

type nsInfo struct {
	nad string
	// eip -> nodes
	eips map[string]map[string]struct{}
}

// EgressIPTrackerController tracks which NADs must be present on which nodes
// due to EgressIP assignments.
type EgressIPTrackerController struct {
	sync.Mutex

	// node -> nad -> set[eipName]
	cache map[string]map[string]map[string]struct{}
	// eipName -> node -> set[nad]
	reverse map[string]map[string]map[string]struct{}

	// nsName -> {nad, eips: eipName -> set[node]}
	nsCache map[string]*nsInfo

	onNetworkRefChange func(nodeName, nadName string, present bool)

	nsLister      v1.NamespaceLister
	eipLister     egressiplisters.EgressIPLister
	eipController controller.Controller
	nsController  controller.Controller
	nadController controller.Controller
	networkManger Interface
}

func NewEgressIPTrackerController(
	wf watchFactory,
	nm Interface,
	onNetworkRefChange func(nodeName, nadName string, present bool),
) *EgressIPTrackerController {
	t := &EgressIPTrackerController{
		cache:              make(map[string]map[string]map[string]struct{}),
		reverse:            make(map[string]map[string]map[string]struct{}),
		nsCache:            make(map[string]*nsInfo),
		networkManger:      nm,
		onNetworkRefChange: onNetworkRefChange,
		nsLister:           wf.NamespaceInformer().Lister(),
		eipLister:          wf.EgressIPInformer().Lister(),
	}

	cfg := &controller.ControllerConfig[egressipv1.EgressIP]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      t.reconcileEgressIP,
		ObjNeedsUpdate: t.egressIPNeedsUpdate,
		MaxAttempts:    controller.InfiniteAttempts,
		Threadiness:    1,
		Informer:       wf.EgressIPInformer().Informer(),
		Lister:         wf.EgressIPInformer().Lister().List,
	}
	t.eipController = controller.NewController[egressipv1.EgressIP]("egressip-tracker", cfg)

	ncfg := &controller.ControllerConfig[corev1.Namespace]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      t.reconcileNamespace,
		ObjNeedsUpdate: t.namespaceNeedsUpdate,
		MaxAttempts:    controller.InfiniteAttempts,
		Threadiness:    1,
		Informer:       wf.NamespaceInformer().Informer(),
		Lister:         wf.NamespaceInformer().Lister().List,
	}
	t.nsController = controller.NewController[corev1.Namespace]("egressip-namespace-tracker", ncfg)

	nadCfg := &controller.ControllerConfig[nettypes.NetworkAttachmentDefinition]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      t.reconcileNAD,
		ObjNeedsUpdate: t.nadNeedsUpdate,
		MaxAttempts:    controller.InfiniteAttempts,
		Threadiness:    1,
		Informer:       wf.NADInformer().Informer(),
		Lister:         wf.NADInformer().Lister().List,
	}
	t.nadController = controller.NewController[nettypes.NetworkAttachmentDefinition]("egressip-nad-tracker", nadCfg)

	return t
}

func (t *EgressIPTrackerController) Start() error {
	return controller.StartWithInitialSync(t.syncAll, t.eipController, t.nsController, t.nadController)
}

func (t *EgressIPTrackerController) Stop() {
	controller.Stop(t.eipController, t.nsController, t.nadController)
}

func (t *EgressIPTrackerController) NodeHasNAD(node, nad string) bool {
	t.Lock()
	defer t.Unlock()
	if _, ok := t.cache[node]; !ok {
		return false
	}
	if _, ok := t.cache[node][nad]; !ok {
		return false
	}
	return len(t.cache[node][nad]) > 0
}

func (t *EgressIPTrackerController) nadNeedsUpdate(oldObj, _ *nettypes.NetworkAttachmentDefinition) bool {
	return oldObj == nil
}

func (t *EgressIPTrackerController) egressIPNeedsUpdate(oldObj, newObj *egressipv1.EgressIP) bool {
	if oldObj == nil {
		return true // this is an Add
	}

	if !reflect.DeepEqual(oldObj.Spec.NamespaceSelector, newObj.Spec.NamespaceSelector) {
		return true
	}

	if !reflect.DeepEqual(oldObj.Status.Items, newObj.Status.Items) {
		return true
	}

	return false
}

func (t *EgressIPTrackerController) namespaceNeedsUpdate(oldObj, newObj *corev1.Namespace) bool {
	if oldObj == nil {
		return true // this is an Add
	}

	// Only trigger reconcile if the labels (used by EgressIP selectors) change
	return !reflect.DeepEqual(oldObj.Labels, newObj.Labels)
}

func (t *EgressIPTrackerController) reconcileNAD(key string) error {
	t.Lock()
	defer t.Unlock()

	namespace, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("invalid NAD key %q: %v", key, err)
	}

	// Determine current active primary NAD for this namespace
	primary, err := t.networkManger.GetActiveNetworkForNamespace(namespace)
	if err != nil {
		return fmt.Errorf("failed to get active network for namespace %q: %v", namespace, err)
	}

	var newNAD string
	if !primary.IsDefault() {
		nads := primary.GetNADs()
		if len(nads) > 0 {
			newNAD = nads[0]
		}
	}

	oldInfo, ok := t.nsCache[namespace]
	if !ok {
		// If no cached info, nothing to update yet.
		return nil
	}

	if oldInfo.nad == newNAD {
		// No change, nothing to do.
		return nil
	}

	// NAD changed. Remove old NAD associations and add with new NAD.
	for eip, nodes := range oldInfo.eips {
		for node := range nodes {
			// Remove old NAD
			t.removeNamespaceEIP(namespace, eip, node, oldInfo.nad)
			// Add new NAD if it exists
			if newNAD != "" {
				t.addNamespaceEIP(namespace, eip, node, newNAD)
			}
		}
	}

	// Update stored NAD value
	oldInfo.nad = newNAD
	return nil
}

func (t *EgressIPTrackerController) reconcileEgressIP(key string) error {
	t.Lock()
	defer t.Unlock()

	eip, err := t.eipLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Drive cleanup by walking nsCache; this updates all caches consistently
			for nsName, info := range t.nsCache {
				if nodes, ok := info.eips[key]; ok {
					for node := range nodes {
						t.removeNamespaceEIP(nsName, key, node, info.nad)
					}
					if len(info.eips) == 0 {
						delete(t.nsCache, nsName)
					}
				}
			}
			return nil
		}
		return fmt.Errorf("failed to get EgressIP %q from cache: %v", key, err)
	}

	// Build desired ns -> {nad, nodes} for this EIP
	desiredNS := make(map[string]struct {
		nad   string
		nodes map[string]struct{}
	})

	nsSelector, err := metav1.LabelSelectorAsSelector(&eip.Spec.NamespaceSelector)
	if err != nil {
		return fmt.Errorf("invalid namespaceSelector in EIP %s: %w", key, err)
	}
	nsList, err := t.nsLister.List(nsSelector)
	if err != nil {
		return fmt.Errorf("failed to list namespaces for EIP %s: %w", key, err)
	}

	for _, ns := range nsList {
		primary, err := t.networkManger.GetActiveNetworkForNamespace(ns.Name)
		if err != nil {
			return fmt.Errorf("failed to get primary network for namespace %q: %v", ns.Name, err)
		}
		if primary.IsDefault() {
			continue
		}
		nads := primary.GetNADs()
		if len(nads) == 0 {
			continue
		}
		nad := nads[0]

		nodes := make(map[string]struct{})
		for _, st := range eip.Status.Items {
			nodes[st.Node] = struct{}{}
		}
		desiredNS[ns.Name] = struct {
			nad   string
			nodes map[string]struct{}
		}{nad: nad, nodes: nodes}
	}

	// Current ns->nodes for this EIP from nsCache
	currentNS := make(map[string]map[string]struct{}) // ns -> node -> {}
	for nsName, info := range t.nsCache {
		if nodes, ok := info.eips[key]; ok {
			currentNS[nsName] = nodes
		}
	}

	// Add missing links
	for nsName, want := range desiredNS {
		for node := range want.nodes {
			if _, ok := currentNS[nsName]; !ok || !containsKey(currentNS[nsName], node) {
				t.addNamespaceEIP(nsName, key, node, want.nad)
			}
		}
	}

	// Remove stale links
	for nsName, curNodes := range currentNS {
		for node := range curNodes {
			want, ok := desiredNS[nsName]
			if !ok || !containsKey(want.nodes, node) {
				if info, ok := t.nsCache[nsName]; ok {
					t.removeNamespaceEIP(nsName, key, node, info.nad)
				}
			}
		}
	}

	return nil
}

func (t *EgressIPTrackerController) reconcileNamespace(key string) error {
	t.Lock()
	defer t.Unlock()

	ns, err := t.nsLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Namespace deleted → remove all cached eip->node->nad associations
			if info, ok := t.nsCache[key]; ok {
				for eip, nodes := range info.eips {
					for node := range nodes {
						t.removeNamespaceEIP(key, eip, node, info.nad)
					}
				}
				delete(t.nsCache, key)
			}
			return nil
		}
		return fmt.Errorf("failed to get namespace %q: %v", key, err)
	}

	// Step 1: determine the namespace's current primary UDN/NAD
	primaryNetwork, err := t.networkManger.GetActiveNetworkForNamespace(ns.Name)
	if err != nil {
		return fmt.Errorf("failed to get primary network for namespace %q: %v", ns.Name, err)
	}
	nad := ""
	if !primaryNetwork.IsDefault() {
		nads := primaryNetwork.GetNADs()
		if len(nads) > 0 {
			nad = nads[0] // only one primary NAD is valid
		}
	}

	// If no NAD, remove any stale cache and stop.
	if nad == "" {
		if info, ok := t.nsCache[ns.Name]; ok {
			for eip, nodes := range info.eips {
				for node := range nodes {
					t.removeNamespaceEIP(ns.Name, eip, node, info.nad)
				}
			}
		}
		return nil
	}

	// Step 2: find all EgressIPs selecting this namespace and collect node assignments
	desiredEIPs := make(map[string]map[string]struct{}) // eipName -> node -> {}
	eips, err := t.eipLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list EgressIPs: %v", err)
	}
	for _, eip := range eips {
		selector, err := metav1.LabelSelectorAsSelector(&eip.Spec.NamespaceSelector)
		if err != nil {
			continue // skip invalid selectors rather than fail
		}
		if selector.Matches(labels.Set(ns.Labels)) {
			nodes := make(map[string]struct{})
			for _, st := range eip.Status.Items {
				nodes[st.Node] = struct{}{}
			}
			desiredEIPs[eip.Name] = nodes
		}
	}

	// Step 3: determine current state from cache
	// map of eips -> nodes
	currentEIPs := make(map[string]map[string]struct{})
	if info, ok := t.nsCache[ns.Name]; ok {
		currentEIPs = info.eips
	}

	// Step 4: add missing (eip,node) pairs
	for eip, nodes := range desiredEIPs {
		for node := range nodes {
			if _, ok := currentEIPs[eip]; !ok || !containsKey(currentEIPs[eip], node) {
				t.addNamespaceEIP(ns.Name, eip, node, nad)
			}
		}
	}

	// Step 5: remove stale (eip,node) pairs
	if info, ok := t.nsCache[ns.Name]; ok {
		for eip, nodes := range currentEIPs {
			for node := range nodes {
				if _, ok := desiredEIPs[eip]; !ok || !containsKey(desiredEIPs[eip], node) {
					t.removeNamespaceEIP(ns.Name, eip, node, info.nad)
				}
			}
		}
	}

	// Step 6: refresh nsCache entry with updated nad and desired eips
	if _, ok := t.nsCache[ns.Name]; !ok {
		t.nsCache[ns.Name] = &nsInfo{
			nad:  nad,
			eips: make(map[string]map[string]struct{}),
		}
	}
	t.nsCache[ns.Name].nad = nad
	t.nsCache[ns.Name].eips = desiredEIPs

	return nil
}

func containsKey(m map[string]struct{}, key string) bool {
	_, ok := m[key]
	return ok
}

// Called when an EIP selects a namespace
func (t *EgressIPTrackerController) addNamespaceEIP(ns, eipName, node, nad string) {
	// Update nsCache: ns -> eip -> set[node], and ns.nad
	info, ok := t.nsCache[ns]
	if !ok {
		info = &nsInfo{
			nad:  nad,
			eips: make(map[string]map[string]struct{}),
		}
		t.nsCache[ns] = info
	}
	// If NAD changed (namespace recon), keep nsCache.nad current
	info.nad = nad
	if _, ok := info.eips[eipName]; !ok {
		info.eips[eipName] = make(map[string]struct{})
	}
	// If node already present for this (ns,eip), nothing more to do on ns view
	if _, exists := info.eips[eipName][node]; exists {
		return
	}
	info.eips[eipName][node] = struct{}{}

	// --- Update egressIP reverse cache: eipName -> node -> set[nad] ---
	if _, ok := t.reverse[eipName]; !ok {
		t.reverse[eipName] = make(map[string]map[string]struct{})
	}
	if _, ok := t.reverse[eipName][node]; !ok {
		t.reverse[eipName][node] = make(map[string]struct{})
	}
	t.reverse[eipName][node][nad] = struct{}{}

	// --- Update forward cache: node -> nad -> set[eipName] ---
	if _, ok := t.cache[node]; !ok {
		t.cache[node] = make(map[string]map[string]struct{})
	}
	if _, ok := t.cache[node][nad]; !ok {
		t.cache[node][nad] = make(map[string]struct{})
	}
	before := len(t.cache[node][nad])
	t.cache[node][nad][eipName] = struct{}{}
	after := len(t.cache[node][nad])

	// Fire callback only when this (node,nad) first becomes active
	if before == 0 && after == 1 && t.onNetworkRefChange != nil {
		t.onNetworkRefChange(node, nad, true)
	}
}

// Called when namespace is deleted or its NAD changes
func (t *EgressIPTrackerController) removeNamespaceEIP(ns, eipName, node, nad string) {
	// --- Update nsCache ---
	if info, ok := t.nsCache[ns]; ok {
		if nodes, ok := info.eips[eipName]; ok {
			delete(nodes, node)
			if len(nodes) == 0 {
				delete(info.eips, eipName)
			}
		}

		if len(info.eips) == 0 {
			delete(t.nsCache, ns)
		}
	}

	// --- Update eip reverse cache ---
	if nodes, ok := t.reverse[eipName]; ok {
		if nads, ok := nodes[node]; ok {
			delete(nads, nad)
			if len(nads) == 0 {
				delete(nodes, node)
			}
		}
		if len(nodes) == 0 {
			delete(t.reverse, eipName)
		}
	}

	// --- Update forward cache (node -> nad -> eip set) ---
	if nads, ok := t.cache[node]; ok {
		if eips, ok := nads[nad]; ok {
			before := len(eips)
			delete(eips, eipName)
			after := len(eips)

			if before > 0 && after == 0 && t.onNetworkRefChange != nil {
				// 1 → 0 transition
				t.onNetworkRefChange(node, nad, false)
			}
			if len(eips) == 0 {
				delete(nads, nad)
			}
		}
		if len(nads) == 0 {
			delete(t.cache, node)
		}
	}
}

func (t *EgressIPTrackerController) syncAll() error {
	t.Lock()
	defer t.Unlock()

	// Clear everything
	t.cache = make(map[string]map[string]map[string]struct{})
	t.reverse = make(map[string]map[string]map[string]struct{})
	t.nsCache = make(map[string]*nsInfo)

	// Pre-list EIPs and build an index: eipName -> set[node]
	eips, err := t.eipLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("syncAll: list EgressIPs: %v", err)
	}
	eipNodes := make(map[string]map[string]struct{})
	for _, eip := range eips {
		nodes := make(map[string]struct{})
		for _, st := range eip.Status.Items {
			nodes[st.Node] = struct{}{}
		}
		eipNodes[eip.Name] = nodes
	}

	// List namespaces; for each, find primary NAD and which EIPs select it
	namespaces, err := t.nsLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("syncAll: list Namespaces: %v", err)
	}

	for _, ns := range namespaces {
		nsName := ns.Name

		primary, err := t.networkManger.GetActiveNetworkForNamespace(nsName)
		if err != nil {
			return fmt.Errorf("syncAll: primary network for ns %q: %v", nsName, err)
		}
		if primary.IsDefault() {
			continue
		}
		nads := primary.GetNADs()
		if len(nads) == 0 {
			continue
		}
		nad := nads[0]

		for _, eip := range eips {
			selector, err := metav1.LabelSelectorAsSelector(&eip.Spec.NamespaceSelector)
			if err != nil {
				continue
			}
			if !selector.Matches(labels.Set(ns.Labels)) {
				continue
			}
			for node := range eipNodes[eip.Name] {
				t.addNamespaceEIP(nsName, eip.Name, node, nad)
			}
		}
	}

	return nil
}
