package networkmanager

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressiplisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/listers/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// EgressIPTrackerController tracks which NADs must be present on which nodes
// due to EgressIP assignments.
type EgressIPTrackerController struct {
	sync.Mutex
	name string
	// node -> nad that has EIP
	cache map[string]map[string]struct{}

	onNetworkRefChange func(nodeName, nadName string, present bool)

	nsLister      v1.NamespaceLister
	eipLister     egressiplisters.EgressIPLister
	nadLister     nadlisters.NetworkAttachmentDefinitionLister
	eipController controller.Controller
	nsController  controller.Controller
	nadController controller.Controller
}

func NewEgressIPTrackerController(
	name string, wf watchFactory,
	onNetworkRefChange func(nodeName, nadName string, present bool),
) *EgressIPTrackerController {
	t := &EgressIPTrackerController{
		name:               name,
		cache:              make(map[string]map[string]struct{}),
		onNetworkRefChange: onNetworkRefChange,
		nsLister:           wf.NamespaceInformer().Lister(),
		eipLister:          wf.EgressIPInformer().Lister(),
		nadLister:          wf.NADInformer().Lister(),
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
	return true
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
	klog.V(5).Infof("%s - reconciling NAD key: %q", t.name, key)
	namespace, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("invalid NAD key %q: %v", key, err)
	}

	t.nsController.Reconcile(namespace)
	return nil
}

func (t *EgressIPTrackerController) reconcileEgressIP(key string) error {
	klog.V(5).Infof("%s - reconciling egress IP key: %q", t.name, key)

	eip, err := t.eipLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// EgressIP deleted
			namespacesToReconcile := make(map[string]struct{})
			t.Lock()
			for _, nads := range t.cache {
				for nad := range nads {
					nsName, _, err := cache.SplitMetaNamespaceKey(nad)
					if err != nil {
						klog.Errorf("%s - Invalid NAD key in cache %q: %v", t.name, nad, err)
						continue
					}
					namespacesToReconcile[nsName] = struct{}{}
				}
			}
			t.Unlock()

			for nsName := range namespacesToReconcile {
				t.nsController.Reconcile(nsName)
			}
			return nil
		}
		return fmt.Errorf("failed to get EgressIP %q from cache: %v", key, err)
	}

	nsSelector, err := metav1.LabelSelectorAsSelector(&eip.Spec.NamespaceSelector)
	if err != nil {
		return fmt.Errorf("invalid namespaceSelector in EIP %s: %w", key, err)
	}
	nsList, err := t.nsLister.List(nsSelector)
	if err != nil {
		return fmt.Errorf("failed to list namespaces for EIP %s: %w", key, err)
	}

	for _, ns := range nsList {
		t.nsController.Reconcile(ns.Name)
	}

	return nil
}

func (t *EgressIPTrackerController) reconcileNamespace(key string) error {
	klog.V(5).Infof("%s - reconciling namespace key: %q", t.name, key)
	ns, err := t.nsLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Namespace deleted → drop any cache
			t.Lock()
			defer t.Unlock()
			for node, nads := range t.cache {
				for nad := range nads {
					nadNamespace, _, err := cache.SplitMetaNamespaceKey(nad)
					if err != nil {
						klog.Errorf("%s - Invalid NAD key in cache %q: %v", t.name, key, err)
						delete(nads, nad)
					} else {
						if nadNamespace == key {
							delete(nads, nad)
							if t.onNetworkRefChange != nil {
								t.onNetworkRefChange(node, nad, false)
							}
						}
					}
				}
				if len(nads) == 0 {
					delete(t.cache, node)
				}
			}
			return nil
		}
		return err
	}

	primaryNAD, err := t.getPrimaryNADForNamespace(ns.Name)
	if err != nil {
		return fmt.Errorf("failed to get primary NAD for namespace %q: %w", ns.Name, err)
	}
	// Gather the new set of (node,nad) pairs implied by this namespace's EIPs
	newActive := map[string]string{} // node -> nad
	if primaryNAD != "" {
		eips, _ := t.eipLister.List(labels.Everything())
		for _, eip := range eips {
			sel, _ := metav1.LabelSelectorAsSelector(&eip.Spec.NamespaceSelector)
			if sel.Matches(labels.Set(ns.Labels)) {
				for _, st := range eip.Status.Items {
					newActive[st.Node] = primaryNAD
				}
			}
		}
	}

	// Diff against cache
	t.Lock()
	defer t.Unlock()

	// Removals first
	for node, nads := range t.cache {
		for nad := range nads {
			nsName, _, err := cache.SplitMetaNamespaceKey(nad)
			if err != nil {
				klog.Errorf("%s - Invalid NAD key in cache %q: %v", t.name, key, err)
				delete(nads, nad)
			}
			if nsName == ns.Name {
				if newActive[node] != nad {
					delete(nads, nad)
					if t.onNetworkRefChange != nil {
						t.onNetworkRefChange(node, nad, false)
					}
				}
			}
		}
		if len(nads) == 0 {
			delete(t.cache, node)
		}
	}

	// Additions second
	for node, nad := range newActive {
		if _, ok := t.cache[node]; !ok {
			t.cache[node] = map[string]struct{}{}
		}
		if _, exists := t.cache[node][nad]; !exists {
			t.cache[node][nad] = struct{}{}
			if t.onNetworkRefChange != nil {
				t.onNetworkRefChange(node, nad, true)
			}
		}
	}
	return nil
}

func (t *EgressIPTrackerController) syncAll() error {
	start := time.Now()
	defer func() {
		klog.V(5).Infof("%s - syncAll took %v", t.name, time.Since(start))
	}()

	// handling all namespaces will handle setting up the egress IP tracker
	namespaces, err := t.nsLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("syncAll: list Namespaces: %v", err)
	}

	for _, ns := range namespaces {
		nsName := ns.Name
		if err := t.reconcileNamespace(nsName); err != nil {
			klog.Errorf("%s - Failed to sync namespace %q: %v", t.name, nsName, err)
			continue
		}
	}

	return nil
}

func (t *EgressIPTrackerController) getPrimaryNADForNamespace(namespace string) (string, error) {
	requiresUDN := false
	// check if required UDN label is on namespace
	ns, err := t.nsLister.Get(namespace)
	if err != nil {
		return "", fmt.Errorf("failed to get namespace %q: %w", namespace, err)
	}
	if _, exists := ns.Labels[types.RequiredUDNNamespaceLabel]; exists {
		requiresUDN = true
	}
	nads, err := t.nadLister.NetworkAttachmentDefinitions(namespace).List(labels.Everything())
	if err != nil {
		return "", fmt.Errorf("failed to list network attachment definitions: %w", err)
	}
	for _, nad := range nads {
		if nad.Name == types.DefaultNetworkName {
			continue
		}
		nadInfo, err := util.ParseNADInfo(nad)
		if err != nil {
			klog.Warningf("%s - Failed to parse network attachment definition %q: %v", t.name, nad.Name, err)
			continue
		}
		if nadInfo.IsPrimaryNetwork() {
			return util.GetNADName(nad.Namespace, nad.Name), nil
		}
	}

	if requiresUDN {
		return "", util.NewInvalidPrimaryNetworkError(namespace)
	}

	return "", nil
}
