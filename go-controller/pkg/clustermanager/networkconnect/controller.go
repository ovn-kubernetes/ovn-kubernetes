package networkconnect

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/id"
	controllerutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	networkconnectclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/clientset/versioned"
	networkconnectlisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/listers/clusternetworkconnect/v1"
	apitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
	userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var (
	cudnGVK = userdefinednetworkv1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetwork")
	udnGVK  = userdefinednetworkv1.SchemeGroupVersion.WithKind("UserDefinedNetwork")
)

// clusterNetworkConnectState is the cache that keeps the state of a single
// cluster network connect in the cluster with name being unique
type clusterNetworkConnectState struct {
	// name of the cluster network connect (unique across cluster)
	name string
	// allocator for this CNC's subnet allocation
	allocator HybridConnectSubnetAllocator
	// map of NADs currently selected by this CNC's network selectors
	// {value: NAD namespace/name key}
	// this cache is mainly required to be able to detect when a
	// NAD is started or stopped matching the CNC. That way, we don't
	// need to call reconcile on all CNCs and only the specific ones
	// that select this NAD.
	// Specially, when a NAD is deleted, we need to release the subnets allocated for
	// matching CNCs and since nadKey is the only information we get
	// since NAD object itself will be nil since its deleted, we need
	// to keep track of NAD keys.
	selectedNADs sets.Set[string]
	// set of networks currently selected by this CNC's network selectors
	// {value: network owner key like "layer3_1" or "layer2_2"}
	// Owner keys are computed from topology type (layer3 or layer2) and network ID, enabling subnet release
	// without needing to re-discover network info.
	selectedNetworks sets.Set[string]
	// tunnelID for this CNC's connect router
	tunnelID int
}

type Controller struct {
	// wf is the watch factory for accessing informers
	wf *factory.WatchFactory
	// listers
	cncLister       networkconnectlisters.ClusterNetworkConnectLister
	namespaceLister corelisters.NamespaceLister
	nadLister       nadlisters.NetworkAttachmentDefinitionLister
	//clientset
	cncClient networkconnectclientset.Interface
	nadClient nadclientset.Interface
	// Controller for managing cluster-network-connect events
	cncController controllerutil.Controller
	// Controller for managing NetworkAttachmentDefinition events
	nadController  controllerutil.Controller
	networkManager networkmanager.Interface

	// Single global lock protecting all controller state
	// We can improve this later by using a more fine-grained lock based on performance testing
	sync.RWMutex
	// holds the state for each CNC keyed by CNC name
	cncCache            map[string]*clusterNetworkConnectState
	tunnelKeysAllocator *id.TunnelKeysAllocator
}

func NewController(
	wf *factory.WatchFactory,
	ovnClient *util.OVNClusterManagerClientset,
	networkManager networkmanager.Interface,
	tunnelKeysAllocator *id.TunnelKeysAllocator,
) *Controller {
	cncLister := wf.ClusterNetworkConnectInformer().Lister()
	nadLister := wf.NADInformer().Lister()

	c := &Controller{
		wf:                  wf,
		cncClient:           ovnClient.NetworkConnectClient,
		nadClient:           ovnClient.NetworkAttchDefClient,
		cncLister:           cncLister,
		nadLister:           nadLister,
		namespaceLister:     wf.NamespaceInformer().Lister(),
		networkManager:      networkManager,
		cncCache:            make(map[string]*clusterNetworkConnectState),
		tunnelKeysAllocator: tunnelKeysAllocator,
	}

	cncCfg := &controllerutil.ControllerConfig[networkconnectv1.ClusterNetworkConnect]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.ClusterNetworkConnectInformer().Informer(),
		Lister:         cncLister.List,
		Reconcile:      c.reconcileClusterNetworkConnect,
		ObjNeedsUpdate: cncNeedsUpdate,
		Threadiness:    1,
	}
	c.cncController = controllerutil.NewController(
		"clustermanager-network-connect-controller",
		cncCfg,
	)

	nadCfg := &controllerutil.ControllerConfig[nadv1.NetworkAttachmentDefinition]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.NADInformer().Informer(),
		Lister:         nadLister.List,
		Reconcile:      c.reconcileNAD,
		ObjNeedsUpdate: nadNeedsUpdate,
		Threadiness:    1,
	}
	c.nadController = controllerutil.NewController(
		"clustermanager-network-connect-network-attachment-definition-controller",
		nadCfg,
	)

	return c
}

func (c *Controller) Start() error {
	defer klog.Infof("Cluster manager network connect controllers started")
	return controllerutil.Start(
		c.cncController,
		c.nadController,
	)
}

func (c *Controller) Stop() {
	controllerutil.Stop(
		c.cncController,
		c.nadController,
	)
	klog.Infof("Cluster manager network connect controllers stopped")
}

func cncNeedsUpdate(oldObj, newObj *networkconnectv1.ClusterNetworkConnect) bool {
	// Case 1: CNC is being deleted
	// Case 2: CNC is being created
	if oldObj == nil || newObj == nil {
		return true
	}
	// Case 3: CNC is being updated
	// Only trigger updates when the Spec.NetworkSelectors changes
	// We only need to check for selector changes
	// and don't need to react on connectivity enabled field changes
	// from cluster manager.
	// connectSubnet is immutable so that can't change after creation.
	return !reflect.DeepEqual(oldObj.Spec.NetworkSelectors, newObj.Spec.NetworkSelectors)
}

func nadNeedsUpdate(oldObj, newObj *nadv1.NetworkAttachmentDefinition) bool {
	nadSupported := func(nad *nadv1.NetworkAttachmentDefinition) bool {
		if nad == nil {
			return false
		}
		// we don't support direct NADs anymore. CNC is only supported for CUDNs and UDNs
		controller := metav1.GetControllerOfNoCopy(nad)
		isCUDN := controller != nil && controller.Kind == cudnGVK.Kind && controller.APIVersion == cudnGVK.GroupVersion().String()
		isUDN := controller != nil && controller.Kind == udnGVK.Kind && controller.APIVersion == udnGVK.GroupVersion().String()
		if !isCUDN && !isUDN {
			return false
		}
		network, err := util.ParseNADInfo(nad)
		if err != nil {
			// cannot parse NAD info, so we take this as unsupported NAD error
			return false
		}
		if network.IsPrimaryNetwork() {
			// only layer3 and layer2 topology are supported
			// but since primary network is always layer3 or layer2,
			// we can ignored the need to check the topology
			return true
		}
		return false // we don't support secondary networks, so we can ignore it
	}
	// ignore if we don't support this NAD
	if !nadSupported(oldObj) && !nadSupported(newObj) {
		return false
	}
	// CASE1: NAD is being deleted (UDN or CUDN is being deleted)
	// CASE2: NAD is being created (UDN or CUDN is being created)
	if oldObj == nil || newObj == nil {
		return true
	}
	oldNADLabels := labels.Set(oldObj.Labels)
	newNADLabels := labels.Set(newObj.Labels)
	labelsChanged := !labels.Equals(oldNADLabels, newNADLabels)
	// safe spot check for ovn network id annotation add happening as update to NADs
	annotationsChanged := oldObj.Annotations[ovntypes.OvnNetworkIDAnnotation] != newObj.Annotations[ovntypes.OvnNetworkIDAnnotation]

	// CASE3: NAD is being updated (UDN or CUDN is being updated)
	//  3.1: NAD network id annotation changed
	//  3.2: NAD labels changed (only relevant for CUDNs->NADs)
	return labelsChanged || annotationsChanged
}

func (c *Controller) reconcileNAD(key string) error {
	// Use single global lock following ANP controller pattern
	c.Lock()
	defer c.Unlock()

	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("failed to split NAD key %s: %w", key, err)
	}

	klog.V(5).Infof("reconcileNAD %s", key)
	defer func() {
		klog.Infof("reconcileNAD %s took %v", key, time.Since(startTime))
	}()

	nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get NAD %s: %w", key, err)
	}

	existingCNCs, err := c.cncLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list CNCs: %w", err)
	}

	// Process each CNC to check if this NAD's matching state changed
	for _, cnc := range existingCNCs {
		if c.mustProcessCNCForNAD(nad, cnc, key) {
			c.cncController.Reconcile(cnc.Name)
		}
	}
	return nil
}

// mustProcessCNCForNAD checks if:
// 1. the provided NAD previously matched the given CNC and now it stopped matching OR
// 2. the provided NAD currently matches the given CNC and previously it didn't match
// 3. the provided NAD previously matched the given CNC and now it continues to match it
// Returns true if any of the above conditions are true.
// This function is READ-ONLY and does not update the cache.
// NOTE: Caller must hold the global lock.
func (c *Controller) mustProcessCNCForNAD(nad *nadv1.NetworkAttachmentDefinition, cnc *networkconnectv1.ClusterNetworkConnect, nadKey string) bool {
	cncState, cncExists := c.cncCache[cnc.Name]

	// If CNC state doesn't exist yet, we don't know the previous state
	// so we assume no change (cache will be populated during CNC reconciliation)
	if !cncExists {
		klog.V(5).Infof("CNC %s state not found in cache, assuming no matching state change for NAD %s", cnc.Name, nadKey)
		return false
	}

	// Check if NAD used to be selected (using cache)
	wasSelected := cncState.selectedNADs.Has(nadKey)

	// Determine if NAD started to be selected now
	isSelected := false
	if nad != nil {
		nadLabels := labels.Set(nad.Labels)
	selectorLoop: // break out of the loop if we find a match
		for _, networkSelector := range cnc.Spec.NetworkSelectors {
			switch networkSelector.NetworkSelectionType {
			case apitypes.ClusterUserDefinedNetworks:
				cudnSelector, err := metav1.LabelSelectorAsSelector(&networkSelector.ClusterUserDefinedNetworkSelector.NetworkSelector)
				if err != nil {
					klog.Errorf("Failed to create selector for CNC %s: %v", cnc.Name, err)
					continue
				}
				// labels on CUDN are copied to the corresponding NADs, so we can use the same selector
				if cudnSelector.Matches(nadLabels) {
					isSelected = true
					break selectorLoop
				}
			case apitypes.PrimaryUserDefinedNetworks:
				namespaceSelector, err := metav1.LabelSelectorAsSelector(&networkSelector.PrimaryUserDefinedNetworkSelector.NamespaceSelector)
				if err != nil {
					klog.Errorf("Failed to create selector for CNC %s: %v", cnc.Name, err)
					continue
				}
				namespaces, err := c.namespaceLister.List(namespaceSelector)
				if err != nil {
					klog.Errorf("Failed to list namespaces for CNC %s: %v", cnc.Name, err)
					continue
				}
				for _, namespace := range namespaces {
					primaryNAD, err := c.networkManager.GetActiveNetworkForNamespace(namespace.Name)
					if err != nil {
						if util.IsUnprocessedActiveNetworkError(err) || util.IsInvalidPrimaryNetworkError(err) {
							continue
						}
						klog.Errorf("Failed to get active network for namespace %s: %v", namespace.Name, err)
						continue
					}
					if primaryNAD.HasNAD(nadKey) {
						isSelected = true
						break selectorLoop
					}
				}
			default:
				klog.Errorf("Unsupported network selection type %s for CNC %s", networkSelector.NetworkSelectionType, cnc.Name)
				continue
			}
		}
	}

	// Log state changes
	stateChanged := wasSelected != isSelected
	if stateChanged {
		if isSelected && !wasSelected {
			klog.V(4).Infof("NAD %s started to match CNC %s, requeuing...", nadKey, cnc.Name)
		} else if !isSelected && wasSelected {
			klog.V(4).Infof("NAD %s used to match CNC %s, requeuing...", nadKey, cnc.Name)
		}
	}

	// reason we need to also process if the NAD simply continues to match is because
	// NAD could have had its network-id annotation update which we use in CNC reconciliation to
	// generate the subnet for the connect router corresponding to this CNC.
	return wasSelected || isSelected
}
