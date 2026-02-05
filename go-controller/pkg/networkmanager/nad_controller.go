package networkmanager

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	userdefinednetworklister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
	utiludn "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/udn"
)

// nadController handles namespaced scoped NAD events and
// manages cluster scoped networks defined in those NADs. NADs are mostly
// referenced from pods to give them access to the network. Different NADs can
// define the same network as long as those definitions are actually equal.
// Unexpected situations are handled on best effort basis but improper NAD
// administration can lead to undefined behavior if referenced by running pods.
type nadController struct {
	sync.RWMutex
	// reconcilers keyed by registration ID.
	reconcilers      map[uint64]reconcilerRegistration
	nextReconcilerID uint64

	name            string
	stopChan        chan struct{}
	stopOnce        sync.Once
	nadLister       nadlisters.NetworkAttachmentDefinitionLister
	udnLister       userdefinednetworklister.UserDefinedNetworkLister
	cudnLister      userdefinednetworklister.ClusterUserDefinedNetworkLister
	namespaceLister corelisters.NamespaceLister
	nodeLister      corelisters.NodeLister

	controller controller.Controller
	recorder   record.EventRecorder

	// networkController reconciles network specific controllers
	networkController *networkController

	// nads to network mapping
	nads map[string]string
	// nadsByNetwork tracks NAD keys grouped by network name.
	nadsByNetwork map[string]sets.Set[string]

	// primaryNADs holds a mapping of namespace to NAD of primary UDNs
	primaryNADs map[string]string

	// networkIDAllocator used by cluster-manager to allocate new IDs, zone/node mode only uses as a cache
	networkIDAllocator  id.Allocator
	tunnelKeysAllocator *id.TunnelKeysAllocator
	nadClient           nadclientset.Interface

	markedForRemoval map[string]time.Time

	// filterNADsOnNode is used by Dynamic UDN and refers the node name for which we consider that node local to
	// where OVN-Kubernetes is running.
	// For node/zone it is the name of the local node.
	// For cluster-manager it is empty.
	filterNADsOnNode string

	podTracker      *PodTrackerController
	egressIPTracker *EgressIPTrackerController
	podReconcilerID uint64
	eipReconcilerID uint64
}

type reconcilerRegistration struct {
	id uint64
	r  NADReconciler
}

func newController(
	name string,
	zone string,
	node string,
	cm ControllerManager,
	wf watchFactory,
	ovnClient *util.OVNClusterManagerClientset,
	recorder record.EventRecorder,
	tunnelKeysAllocator *id.TunnelKeysAllocator,
	filterNADsOnNode string,
) (*nadController, error) {
	networkController := newNetworkController(name, zone, node, cm, wf)
	c := &nadController{
		name:              fmt.Sprintf("[%s NAD controller]", name),
		stopChan:          make(chan struct{}),
		recorder:          recorder,
		nadLister:         wf.NADInformer().Lister(),
		nodeLister:        wf.NodeCoreInformer().Lister(),
		networkController: networkController,
		reconcilers:       map[uint64]reconcilerRegistration{},
		nads:              map[string]string{},
		nadsByNetwork:     map[string]sets.Set[string]{},
		primaryNADs:       map[string]string{},
		markedForRemoval:  map[string]time.Time{},
	}
	networkController.getNADKeysForNetwork = c.GetNADKeysForNetwork

	if cm != nil && config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
		c.podTracker = NewPodTrackerController("pod-tracker", wf, c.OnNetworkRefChange, c.GetPrimaryNADForNamespace)
		podID, err := c.RegisterNADReconciler(c.podTracker.NADReconciler())
		if err != nil {
			return nil, fmt.Errorf("failed to register pod tracker NAD reconciler: %w", err)
		}
		c.podReconcilerID = podID
		if config.OVNKubernetesFeature.EnableEgressIP {
			c.egressIPTracker = NewEgressIPTrackerController("egress-ip-tracker", wf, c.OnNetworkRefChange, c.GetPrimaryNADForNamespace)
			eipID, err := c.RegisterNADReconciler(c.egressIPTracker.NADReconciler())
			if err != nil {
				return nil, fmt.Errorf("failed to register egress IP tracker NAD reconciler: %w", err)
			}
			c.eipReconcilerID = eipID
		}
		c.filterNADsOnNode = filterNADsOnNode
	}

	c.networkController.nodeHasNetwork = c.NodeHasNetwork

	if ovnClient != nil {
		c.nadClient = ovnClient.NetworkAttchDefClient
	}

	c.networkIDAllocator = id.NewIDAllocator("NetworkIDs", MaxNetworks)
	// Reserve the ID of the default network
	err := c.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate default network ID: %w", err)
	}
	if zone == "" && node == "" {
		// tunnelKeysAllocator must be passed for cluster manager
		c.tunnelKeysAllocator = tunnelKeysAllocator
	}

	config := &controller.ControllerConfig[nettypes.NetworkAttachmentDefinition]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.NADInformer().Informer(),
		Lister:         c.nadLister.List,
		Reconcile:      c.sync,
		ObjNeedsUpdate: c.nadNeedsUpdate,
		Threadiness:    1,
	}

	if util.IsNetworkSegmentationSupportEnabled() {
		if udnInformer := wf.UserDefinedNetworkInformer(); udnInformer != nil {
			c.udnLister = udnInformer.Lister()
		}
		if cudnInformer := wf.ClusterUserDefinedNetworkInformer(); cudnInformer != nil {
			c.cudnLister = cudnInformer.Lister()
		}
		if nsInformer := wf.NamespaceInformer(); nsInformer != nil {
			c.namespaceLister = nsInformer.Lister()
		}
	}
	c.controller = controller.NewController(
		c.name,
		config,
	)

	return c, nil
}

func (c *nadController) nodeHasNAD(node, nad string) bool {
	if !config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
		return true
	}
	if c.podTracker != nil && c.podTracker.NodeHasNAD(node, nad) {
		return true
	}
	if c.egressIPTracker != nil && c.egressIPTracker.NodeHasNAD(node, nad) {
		return true
	}
	return false
}

func (c *nadController) NodeHasNetwork(node, networkName string) bool {
	if !config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
		return true
	}
	if networkName == "" {
		return false
	}
	if networkName == types.DefaultNetworkName {
		return true
	}
	c.RLock()
	nadSet := c.nadsByNetwork[networkName]
	var nads []string
	if len(nadSet) != 0 {
		nads = nadSet.UnsortedList()
	}
	c.RUnlock()
	for _, nad := range nads {
		if c.nodeHasNAD(node, nad) {
			return true
		}
	}
	return false
}

// addNADToNetworkLocked must be called with nadController locked
func (c *nadController) addNADToNetworkLocked(networkName, nadKey string) {
	if networkName == "" {
		return
	}
	if c.nadsByNetwork == nil {
		c.nadsByNetwork = map[string]sets.Set[string]{}
	}
	nadSet := c.nadsByNetwork[networkName]
	if nadSet == nil {
		nadSet = sets.New[string]()
		c.nadsByNetwork[networkName] = nadSet
	}
	nadSet.Insert(nadKey)
}

// deleteNADFromNetworkLocked must be called with nadController locked
func (c *nadController) deleteNADFromNetworkLocked(networkName, nadKey string) {
	if networkName == "" {
		return
	}
	nadSet := c.nadsByNetwork[networkName]
	if nadSet == nil {
		return
	}
	nadSet.Delete(nadKey)
	if len(nadSet) == 0 {
		delete(c.nadsByNetwork, networkName)
	}
}

// OnNetworkRefChange is a callback function used to signal an action to this controller when
// a network needs to be added or removed or just updated.
// Used as a callback for pod/egress IP events when dynamic UDN allocation is enabled.
// This callback is invoked by pod/egress IP trackers and is blocking. Therefore it's work should be as lightweight
// as possible.
// The function handles:
//  1. Queuing local node event NADs to the NAD Controller for reconciliation later in the NAD Controller worker.
//  2. Queuing remote node event networks to the Network Manager for reconciliation later in the Network Manager worker.
//
// This function should never call into the trackers (i.e. nodeHasNAD) as it would cause deadlock.
func (c *nadController) OnNetworkRefChange(node, nadNamespacedName string, active bool) {
	klog.V(4).Infof("Network change for zone controller triggered by pod/egress IP events "+
		"on node: %s, NAD: %s, active: %t", node, nadNamespacedName, active)

	namespace, name, err := cache.SplitMetaNamespaceKey(nadNamespacedName)
	if err != nil {
		klog.Errorf("Failed splitting key %q, falling back to normal network reconcile: %v", nadNamespacedName, err)
		// fallback to regular reconcile
		c.reconcile(nadNamespacedName)
		return
	}

	nadNetwork := c.GetNetInfoForNADKey(nadNamespacedName)
	if nadNetwork == nil {
		nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
		if err != nil {
			klog.Errorf("Failed to find NAD %q in informer, falling back to normal network reconcile: %v", nadNamespacedName, err)
			// fallback to regular reconcile
			c.reconcile(nadNamespacedName)
			return
		}

		ownerRef := metav1.GetControllerOf(nad)
		if ownerRef == nil {
			return
		}

		if ownerRef.Kind != "ClusterUserDefinedNetwork" && ownerRef.Kind != "UserDefinedNetwork" {
			return
		}

		nadNetwork, err = util.ParseNADInfo(nad)
		if err != nil || nadNetwork == nil {
			klog.Errorf("Failed to parse NAD %q info, falling back to normal network reconcile: %v", nadNamespacedName, err)
			// fallback to regular reconcile
			c.reconcile(nadNamespacedName)
			return
		}
	} else if !nadNetwork.IsUserDefinedNetwork() {
		return
	}

	isLocal := node == c.filterNADsOnNode
	networkName := nadNetwork.GetNetworkName()
	// Enqueue a network reconcile for remote nodes (non-blocking).
	if !isLocal {
		c.networkController.NotifyNetworkRefChange(networkName, node)
	}
	// Let the NAD controller handle lifecycle/teardown decisions asynchronously for local networks only.
	if isLocal {
		c.updateNADState(nadNamespacedName, active)
	}

}

// filter should only be called if cm.filterNADsOnNode is set
func (c *nadController) filter(nad *nettypes.NetworkAttachmentDefinition) (bool, error) {
	ownerRef := metav1.GetControllerOf(nad)
	if ownerRef == nil {
		return false, nil
	}

	ourNode := c.filterNADsOnNode

	if ownerRef.Kind != "ClusterUserDefinedNetwork" && ownerRef.Kind != "UserDefinedNetwork" {
		return false, nil
	}

	// we don't support multiple nodes per zone, assume zone name is node name
	if c.nodeHasNAD(ourNode, util.GetNADName(nad.Namespace, nad.Name)) {
		return false, nil
	}

	return true, nil
}

func (c *nadController) Interface() Interface {
	return c
}

func (c *nadController) Start() error {
	// initial sync here will ensure networks in network manager
	// network manager will use this initial set of ensured networks to consider
	// any other network stale on its own sync
	err := controller.StartWithInitialSync(
		c.syncAll,
		c.controller,
	)
	if err != nil {
		return err
	}

	// Pod and Egress IP Trackers start and process existing pods/egress IPs.
	// The trackers warm up their cache and trigger OnNetworkRefChange to queue keys
	// to NAD Controller.
	if c.podTracker != nil {
		if err := c.podTracker.Start(); err != nil {
			return fmt.Errorf("failed to start pod tracker: %w", err)
		}
	}

	if c.egressIPTracker != nil {
		if err := c.egressIPTracker.Start(); err != nil {
			return fmt.Errorf("failed to start egress ip tracker: %w", err)
		}
	}

	// NetworkController starts last and starts to process network keys to spin up network controllers.
	// At this point the tracker cache's are warm to get accurate information for filtering.
	err = c.networkController.Start()
	if err != nil {
		return err
	}

	klog.Infof("%s: started", c.name)
	return nil
}

func (c *nadController) Stop() {
	klog.Infof("%s: shutting down", c.name)
	c.stopOnce.Do(func() {
		close(c.stopChan)
	})
	controller.Stop(c.controller)
	c.networkController.Stop()
	if c.podReconcilerID != 0 {
		if err := c.DeRegisterNADReconciler(c.podReconcilerID); err != nil {
			klog.Warningf("Failed to deregister pod tracker NAD reconciler: %v", err)
		}
	}
	if c.podTracker != nil {
		c.podTracker.Stop()
	}
	if c.eipReconcilerID != 0 {
		if err := c.DeRegisterNADReconciler(c.eipReconcilerID); err != nil {
			klog.Warningf("Failed to deregister egress IP tracker NAD reconciler: %v", err)
		}
	}
	if c.egressIPTracker != nil {
		c.egressIPTracker.Stop()
	}
}

// RegisterNADReconciler registers a reconciler to receive NAD keys for reconciliation.
func (c *nadController) RegisterNADReconciler(r NADReconciler) (uint64, error) {
	c.Lock()
	defer c.Unlock()
	if c.reconcilers == nil {
		c.reconcilers = map[uint64]reconcilerRegistration{}
	}
	c.nextReconcilerID++
	id := c.nextReconcilerID
	c.reconcilers[id] = reconcilerRegistration{id: id, r: r}
	return id, nil
}

// DeRegisterNADReconciler removes a previously registered reconciler by ID.
func (c *nadController) DeRegisterNADReconciler(id uint64) error {
	c.Lock()
	defer c.Unlock()
	if _, ok := c.reconcilers[id]; !ok {
		return fmt.Errorf("reconciler id %d not found", id)
	}
	delete(c.reconcilers, id)
	return nil
}

// notifyReconcilers enqueues the NAD key to all registered reconcilers
// Must be called with nadController Mutex locked
func (c *nadController) notifyReconcilers(key string) {
	for _, entry := range c.reconcilers {
		entry.r.Reconcile(key)
	}
}

func (c *nadController) reconcile(key string) {
	c.controller.Reconcile(key)
}

// updateNADState enqueues a sync for a given NAD.
// "active" defines if the network is actively being used by a dynamic resource
//   - For local events, we either want to wait for grace period before tearing down an inactive network
//     or clear any removal timer, but both conditions should lead to the network being reconciled (nad sync)
func (c *nadController) updateNADState(key string, active bool) {
	if active { // if local and active, clear the mark for removal
		c.removeMarkedForRemoval(key)
	} else { // inactive start timer for removal
		c.setMarkedForRemoval(key)
	}
	// always requeue to nad controller to syncNAD again
	c.controller.Reconcile(key)
}

func (c *nadController) setMarkedForRemoval(key string) {
	c.Lock()
	if _, ok := c.markedForRemoval[key]; ok {
		c.Unlock()
		return
	}
	removalTime := time.Now().Add(config.OVNKubernetesFeature.UDNDeletionGracePeriod)
	c.markedForRemoval[key] = removalTime
	c.Unlock()

	// ensure we reconcile later
	stopCh := c.stopChan
	go func() {
		klog.V(5).Infof("Scheduling to remove nad %q after %v", key, removalTime)
		timer := time.NewTimer(time.Until(removalTime))
		defer timer.Stop()

		select {
		case <-stopCh:
			return
		case <-timer.C:
			shouldReconcile := false
			c.Lock()
			if rt, ok := c.markedForRemoval[key]; ok && time.Now().After(rt) {
				shouldReconcile = true
			}
			c.Unlock()
			if shouldReconcile {
				c.reconcile(key)
			}
		}
	}()
}

func (c *nadController) removeMarkedForRemoval(key string) {
	c.Lock()
	defer c.Unlock()
	delete(c.markedForRemoval, key)
}

func (c *nadController) syncAll() (err error) {
	existingNADs, err := c.nadLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("%s: failed to list NADs: %w", c.name, err)
	}

	syncNAD := func(nad *nettypes.NetworkAttachmentDefinition) error {
		key, err := cache.MetaNamespaceKeyFunc(nad)
		if err != nil {
			klog.Errorf("%s: failed to sync %v: %v", c.name, nad, err)
			return nil
		}
		err = c.syncNAD(key, nad)
		if err != nil {
			return fmt.Errorf("%s: failed to sync %s: %v", c.name, key, err)
		}
		return nil
	}

	// create all networks with their updated list of NADs and only then start
	// the corresponding controllers so as to avoid to the extent possible the
	// errors and retries that would result if the controller attempted to
	// process pods attached with NADs we wouldn't otherwise know about yet
	nadsWithoutID := []*nettypes.NetworkAttachmentDefinition{}
	for _, nad := range existingNADs {
		// skip NADs that are not annotated with an ID
		if nad.Annotations[types.OvnNetworkIDAnnotation] == "" {
			nadsWithoutID = append(nadsWithoutID, nad)
			continue
		}
		err := syncNAD(nad)
		if err != nil {
			return err
		}
	}

	if len(nadsWithoutID) == 0 {
		return nil
	}

	// preallocate all node IDs to avoid new NADs taking them post start up
	// If we are missing IDs, get them from the nodes which is where we
	// originally had them
	klog.Infof("%s: %d NADs are missing the network ID annotation, fetching from nodes", c.name, len(nadsWithoutID))
	for _, nad := range nadsWithoutID {
		nadNetwork, err := util.ParseNADInfo(nad)
		if err != nil {
			// in case the type for the NAD is not ovn-k we should not record the error event
			if err.Error() != util.ErrorAttachDefNotOvnManaged.Error() {
				klog.Errorf("%s: failed parsing NAD %s/%s: %v", c.name, nad.Namespace, nad.Name, err)
			}
			continue
		}
		netID, err := c.getNetworkIDFromNode(nadNetwork)
		if err != nil {
			return fmt.Errorf("%s: failed to fetch network ID from nodes for nad %s/%s: %v",
				c.name, nad.Namespace, nad.Name, err)
		}
		if netID != types.InvalidID {
			// Reserve the id for the network name. We can safely
			// ignore any errors if there are duplicate ids or if
			// two networks have the same id. We will assign network
			// IDs anyway on sync.
			_ = c.networkIDAllocator.ReserveID(nadNetwork.GetNetworkName(), netID)
		}
	}

	// finally process the pending NADs
	for _, nad := range nadsWithoutID {
		err := syncNAD(nad)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *nadController) sync(key string) error {
	startTime := time.Now()
	klog.V(5).Infof("%s: sync NAD %s", c.name, key)
	defer func() {
		klog.V(4).Infof("%s: finished syncing NAD %s, took %v", c.name, key, time.Since(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("%s: failed splitting key %s: %v", c.name, key, err)
		return nil
	}

	nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	return c.syncNAD(key, nad)
}

func (c *nadController) syncNAD(key string, nad *nettypes.NetworkAttachmentDefinition) (syncErr error) {
	var nadNetworkName string
	var nadNetwork util.NetInfo
	var oldNetwork, ensureNetwork util.MutableNetInfo
	var err error
	dynamicDelete := false

	c.Lock()
	defer c.Unlock()

	namespace, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("%s: failed splitting key %s: %v", c.name, key, err)
	}
	previousNetworkName := c.nads[key]

	deleteTime, setforDeletion := c.markedForRemoval[key]
	if setforDeletion && time.Now().After(deleteTime) {
		// Grace period expired. Force a local teardown, but keep caches aligned to informer state.
		klog.Infof("%s: NAD %q: marked for deletion and time has expired, will remove locally", c.name, key)
		dynamicDelete = nad != nil
		// Act like a delete for rendering/ensure paths
		nad = nil
		defer func() {
			if syncErr == nil {
				delete(c.markedForRemoval, key)
			}
		}()
	}

	if nad != nil {
		nadNetwork, err = util.ParseNADInfo(nad)
		if err != nil {
			// in case the type for the NAD is not ovn-k we should not record the error event
			if err.Error() == util.ErrorAttachDefNotOvnManaged.Error() {
				return nil
			}

			if c.recorder != nil {
				c.recorder.Eventf(&corev1.ObjectReference{Kind: nad.Kind, Namespace: nad.Namespace, Name: nad.Name}, corev1.EventTypeWarning,
					"InvalidConfig", "Failed to parse network config: %v", err.Error())
			}
			klog.Errorf("%s: failed parsing NAD %s: %v", c.name, key, err)
			return nil
		}
		nadNetworkName = nadNetwork.GetNetworkName()
	}

	defer func() {
		c.notifyReconcilers(key) // notify reconcilers after the sync runs with the latest information
	}()

	// We can only have one primary NAD per namespace
	primaryNAD := c.primaryNADs[namespace]
	if nadNetwork != nil && nadNetwork.IsPrimaryNetwork() && primaryNAD != "" && primaryNAD != key {
		return fmt.Errorf("%s: NAD %s is primary for the namespace, NAD %s can't be primary", c.name, primaryNAD, key)
	}

	// As multiple NADs may define networks with the same name, these networks
	// should also have the same config to be considered compatible. If an
	// incompatible network change happens on NAD update, we can:
	// - Re-create network with the same name but different config, if it is not
	//   referred to by any other NAD
	// - Return an error AND clean up NAD from the old network

	// the NAD refers to a different network than before
	if nadNetworkName != c.nads[key] {
		oldNetwork = c.networkController.getNetwork(c.nads[key])
	}

	currentNetwork := c.networkController.getNetwork(nadNetworkName)

	switch {
	case currentNetwork == nil:
		// the NAD refers to a new network, ensure it
		ensureNetwork = util.NewMutableNetInfo(nadNetwork)
	case util.AreNetworksCompatible(currentNetwork, nadNetwork):
		// the NAD refers to an existing compatible network, ensure that
		// existing network holds a reference to this NAD
		ensureNetwork = currentNetwork
	case func() bool {
		nadSet := c.nadsByNetwork[nadNetworkName]
		return len(nadSet) == 1 && nadSet.Has(key)
	}():
		// the NAD is the only NAD referring to an existing incompatible
		// network, remove the reference from the old network and ensure that
		// existing network holds a reference to this NAD
		oldNetwork = currentNetwork
		ensureNetwork = util.NewMutableNetInfo(nadNetwork)
	// the NAD refers to an existing incompatible network referred by other
	// NADs, return error
	case oldNetwork == nil:
		// the NAD refers to the same network as before but with different
		// incompatible configuration, remove the NAD reference from the network
		oldNetwork = currentNetwork
		fallthrough
	default:
		err = fmt.Errorf("%s: NAD %s CNI config does not match that of network %s", c.name, key, nadNetworkName)
	}

	// remove the NAD reference from the old network and delete the network if
	// it is no longer referenced
	if oldNetwork != nil {
		klog.V(5).Infof("%s: removing NAD %q reference for network %q", c.name, key, oldNetwork.GetNetworkName())
		oldNetworkName := oldNetwork.GetNetworkName()
		oldNetwork.DeleteNADs(key)
		if !c.networkReferencedLocked(oldNetworkName, key) {
			c.networkController.DeleteNetwork(oldNetworkName)
		} else {
			c.networkController.EnsureNetwork(oldNetwork)
		}
		if !dynamicDelete && c.primaryNADs[namespace] == key {
			delete(c.primaryNADs, namespace)
		}
	}

	// handleNetworkAnnotations prevents duplicated IDs from being allocated, so we call it even
	// if the NAD/network is filtered by Dynamic UDN while we are ensuring the network.
	// handleNetworkAnnotations also handles deletion and releasing based on NAD cache state
	// IDs are not released during dynamicDeletes (going inactive) and are only released on a true
	// NAD/network delete
	if !dynamicDelete {
		if err := c.handleNetworkAnnotations(ensureNetwork, nad, key, previousNetworkName); err != nil {
			return err
		}
	}

	// this was a nad delete
	if ensureNetwork == nil {
		// On a true delete (incoming nad nil or expired grace period) we must clean caches,
		// except for dynamicDelete where we keep informer-derived state.
		if !dynamicDelete {
			delete(c.markedForRemoval, key)
			// clean up primary mapping even if we never had an oldNetwork rendered
			if c.primaryNADs[namespace] == key {
				delete(c.primaryNADs, namespace)
			}
			networkName := previousNetworkName
			delete(c.nads, key)
			if networkName != "" {
				c.deleteNADFromNetworkLocked(networkName, key)
			}
		}
		return err
	}

	// if network ID has not been set and this is not the well known default
	// network, need to wait until cluster nad controller allocates an ID for
	// the network
	if ensureNetwork.GetNetworkID() == types.InvalidID {
		klog.V(4).Infof("%s: will wait for cluster manager to allocate an ID before ensuring network %s, NAD: %s",
			c.name, nadNetworkName, key)
		return nil
	}

	klog.V(5).Infof("%s: ensuring NAD %q reference for network %q with id %d",
		c.name, key, ensureNetwork.GetNetworkName(), ensureNetwork.GetNetworkID())

	networkName := ensureNetwork.GetNetworkName()
	if previousNetworkName != "" && previousNetworkName != networkName {
		c.deleteNADFromNetworkLocked(previousNetworkName, key)
	}
	c.nads[key] = networkName
	c.addNADToNetworkLocked(networkName, key)
	// track primary NAD
	switch {
	case ensureNetwork.IsPrimaryNetwork():
		c.primaryNADs[namespace] = key
	default:
		if c.primaryNADs[namespace] == key {
			delete(c.primaryNADs, namespace)
		}
	}

	shouldNetworkExist := true
	if c.filterNADsOnNode != "" {
		shouldFilter, err := c.filter(nad)
		if err != nil {
			return fmt.Errorf("%s: failed filtering NAD %s: %w", c.name, key, err)
		}
		if shouldFilter {
			shouldNetworkExist = false
		}
	}
	if shouldNetworkExist {
		// ensure the network is associated with the NAD
		ensureNetwork.AddNADs(key)
		// reconcile the network
		c.networkController.EnsureNetwork(ensureNetwork)
	} else {
		klog.V(4).Infof("%s: Network is filtered and will not be rendered: %s", c.name, ensureNetwork.GetNetworkName())
	}
	return nil
}

// isOwnUpdate checks if an object was updated by us last, as indicated by its
// managed fields. Used to avoid reconciling an update that we made ourselves.
func isOwnUpdate(manager string, managedFields []metav1.ManagedFieldsEntry) bool {
	return util.IsLastUpdatedByManager(manager, managedFields)
}

func (c *nadController) nadNeedsUpdate(oldNAD, newNAD *nettypes.NetworkAttachmentDefinition) (needsUpdate bool) {
	if oldNAD == nil || newNAD == nil {
		return true
	}

	// don't process resync or objects that are marked for deletion
	if oldNAD.ResourceVersion == newNAD.ResourceVersion ||
		!newNAD.GetDeletionTimestamp().IsZero() {
		return false
	}

	if isOwnUpdate(c.name, newNAD.ManagedFields) {
		return false
	}

	// notifyReconcilers during sync happens after netInfo is updated, so controllers receive the latest info,
	// and it is safe to ignore own updates
	defer func() {
		if !needsUpdate { // ensure we send the NAD event to registered handlers anyway
			var key string
			var err error
			if newNAD != nil {
				key, err = cache.MetaNamespaceKeyFunc(newNAD)
				if err != nil && oldNAD != nil {
					key, err = cache.MetaNamespaceKeyFunc(oldNAD)
				}
			}
			if err != nil || len(key) == 0 {
				klog.Errorf("Failed to parse nad key during update, error: %v", err)
			} else {
				c.Lock()
				defer c.Unlock()
				c.notifyReconcilers(key)
			}
		}
	}()

	// also reconcile the network in case its route advertisements changed
	return !reflect.DeepEqual(oldNAD.Spec, newNAD.Spec) ||
		oldNAD.Annotations[types.OvnRouteAdvertisementsKey] != newNAD.Annotations[types.OvnRouteAdvertisementsKey] ||
		oldNAD.Annotations[types.OvnNetworkIDAnnotation] != newNAD.Annotations[types.OvnNetworkIDAnnotation] ||
		oldNAD.Annotations[types.OvnNetworkNameAnnotation] != newNAD.Annotations[types.OvnNetworkNameAnnotation]
}

func (c *nadController) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	if !util.IsNetworkSegmentationSupportEnabled() {
		return &util.DefaultNetInfo{}, nil
	}

	// check if required UDN label is on namespace
	ns, err := c.namespaceLister.Get(namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to get namespace %q: %w", namespace, err)
	}
	if _, exists := ns.Labels[types.RequiredUDNNamespaceLabel]; !exists {
		// UDN required label not set on namespace, assume default network
		return &util.DefaultNetInfo{}, nil
	}

	network, nad := c.getActiveNetworkForNamespace(namespace)
	if network != nil && network.IsPrimaryNetwork() {
		// primary UDN found
		copy := util.NewMutableNetInfo(network)
		copy.SetNADs(nad)
		return copy, nil
	}

	// no primary UDN found, make sure we just haven't processed it yet and no UDN / CUDN exists
	udns, err := c.udnLister.UserDefinedNetworks(namespace).List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("error getting user defined networks: %w", err)
	}
	for _, udn := range udns {
		if utiludn.IsPrimaryNetwork(&udn.Spec) {
			return nil, util.NewUnprocessedActiveNetworkError(namespace, udn.Name)
		}
	}
	cudns, err := c.cudnLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list CUDNs: %w", err)
	}
	for _, cudn := range cudns {
		if !utiludn.IsPrimaryNetwork(&cudn.Spec.Network) {
			continue
		}
		// check the subject namespace referred by the specified namespace-selector
		cudnNamespaceSelector, err := metav1.LabelSelectorAsSelector(&cudn.Spec.NamespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to convert CUDN %q namespaceSelector: %w", cudn.Name, err)
		}
		selectedNamespaces, err := c.namespaceLister.List(cudnNamespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to list namespaces using selector %q: %w", cudnNamespaceSelector, err)
		}
		for _, ns := range selectedNamespaces {
			if ns.Name == namespace {
				return nil, util.NewUnprocessedActiveNetworkError(namespace, cudn.Name)
			}
		}
	}

	// namespace has required UDN label, but no UDN was found
	return nil, util.NewInvalidPrimaryNetworkError(namespace)
}

func (c *nadController) GetActiveNetworkForNamespaceFast(namespace string) util.NetInfo {
	network, _ := c.getActiveNetworkForNamespace(namespace)
	return network
}

// GetPrimaryNADForNamespace returns the full namespaced key of the
// primary NAD for the given namespace, if one exists.
// Returns default network if namespace has no primary UDN or Network Segmentation is disabled
func (c *nadController) GetPrimaryNADForNamespace(namespace string) (string, error) {
	if !util.IsNetworkSegmentationSupportEnabled() {
		return types.DefaultNetworkName, nil
	}
	c.RLock()
	primary := c.primaryNADs[namespace]
	c.RUnlock()
	if primary != "" {
		return primary, nil
	}

	// Double-check if the namespace *requires* a primary UDN.
	ns, err := c.namespaceLister.Get(namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Namespace is gone — no primary NAD by definition.
			return "", nil
		}
		return "", fmt.Errorf("failed to fetch namespace %q: %w", namespace, err)
	}
	if _, exists := ns.Labels[types.RequiredUDNNamespaceLabel]; exists {
		// Namespace promises a primary UDN, but we haven't cached one yet.
		return "", util.NewUnprocessedActiveNetworkError(namespace, "")
	}

	// No required label: means default network only.
	return types.DefaultNetworkName, nil
}

func (c *nadController) getActiveNetworkForNamespace(namespace string) (util.NetInfo, string) {
	c.RLock()
	defer c.RUnlock()

	var network util.NetInfo
	primaryNAD := c.primaryNADs[namespace]
	switch primaryNAD {
	case "":
		// default network
		network = c.networkController.getNetwork(types.DefaultNetworkName)
		if network == nil {
			network = &util.DefaultNetInfo{}
		}
	default:
		// we have a primary network
		netName := c.nads[primaryNAD]
		if netName == "" {
			// this should never happen where we have a nad keyed in the primaryNADs
			// map, but it doesn't exist in the nads map
			panic("NAD Controller broken consistency between primary NADs and cached NADs")
		}
		network = c.networkController.getNetwork(netName)
	}

	return network, primaryNAD
}

func (c *nadController) GetNetwork(name string) util.NetInfo {
	network := c.networkController.getNetwork(name)
	if network == nil && name == types.DefaultNetworkName {
		return &util.DefaultNetInfo{}
	}
	return network
}

func (c *nadController) GetNetInfoForNADKey(nadKey string) util.NetInfo {
	c.RLock()
	networkName := c.nads[nadKey]
	c.RUnlock()
	if networkName == "" {
		return nil
	}
	network := c.networkController.getNetwork(networkName)
	if network == nil && networkName == types.DefaultNetworkName {
		return &util.DefaultNetInfo{}
	}
	if network == nil {
		return nil
	}
	// Return a copy so callers can safely read fields without depending on controller locks.
	return util.NewMutableNetInfo(network)
}

func (c *nadController) GetNetworkNameForNADKey(nadKey string) string {
	c.RLock()
	defer c.RUnlock()
	return c.nads[nadKey]
}

func (c *nadController) GetNADKeysForNetwork(networkName string) []string {
	if networkName == "" {
		return nil
	}
	c.RLock()
	defer c.RUnlock()
	nadSet := c.nadsByNetwork[networkName]
	if len(nadSet) == 0 {
		return nil
	}
	return nadSet.UnsortedList()
}

func (c *nadController) GetActiveNetworkNamespaces(networkName string) ([]string, error) {
	if !util.IsNetworkSegmentationSupportEnabled() {
		return []string{"default"}, nil
	}
	namespaces := make([]string, 0)
	c.RLock()
	defer c.RUnlock()
	for namespaceName, primaryNAD := range c.primaryNADs {
		nadNetworkName := c.nads[primaryNAD]
		if nadNetworkName != networkName {
			continue
		}
		namespaces = append(namespaces, namespaceName)
	}
	return namespaces, nil
}

// DoWithLock iterates over all role primary user defined networks and executes the given fn with each network as input.
// An error will not block execution and instead all errors will be aggregated and returned when all networks are processed.
func (c *nadController) DoWithLock(f func(network util.NetInfo) error) error {
	if !util.IsNetworkSegmentationSupportEnabled() {
		defaultNetwork := &util.DefaultNetInfo{}
		return f(defaultNetwork)
	}
	c.RLock()
	defer c.RUnlock()

	var errs []error
	for _, primaryNAD := range c.primaryNADs {
		if primaryNAD == "" {
			continue
		}
		netName := c.nads[primaryNAD]
		if netName == "" {
			// this should never happen where we have a nad keyed in the primaryNADs
			// map, but it doesn't exist in the nads map
			panic("NAD Controller broken consistency between primary NADs and cached NADs")
		}
		network := c.networkController.getNetwork(netName)
		if network == nil {
			// network may not always be rendered with Dynamic UDN
			// otherwise this should never happen
			if config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
				continue
			}
			panic("NAD Controller broken consistency between primary NADs and network controller cache")
		}
		n := util.NewMutableNetInfo(network)
		// update the returned netInfo copy to only have the primary NAD for this namespace
		n.SetNADs(primaryNAD)
		if err := f(n); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// handleNetworkAnnotations assigns or reads info from the NAD annotations.
// We store network ID and tunnel keys in the AND annotation. This function
// finds out what these values should be for a new network and
// sets it on 'new'. If not annotated, it means it is still to be allocated.
// If this is not the NAD controller running in cluster manager, then we don't
// do anything as we are expected to wait until it happens.
// If this is the NAD controller running in cluster manager then a new ID
// is allocated and annotated on the NAD. The NAD controller running in
// cluster manager also releases here the network ID of a network that is being deleted.
func (c *nadController) handleNetworkAnnotations(new util.MutableNetInfo, nad *nettypes.NetworkAttachmentDefinition, nadKey, previousNetworkName string) (err error) {
	newNetworkName := ""
	if new != nil {
		newNetworkName = new.GetNetworkName()
	}
	if previousNetworkName != "" && previousNetworkName != types.DefaultNetworkName &&
		(new == nil || previousNetworkName != newNetworkName) {
		if c.networkReferencedLocked(previousNetworkName, nadKey) {
			klog.V(5).Infof("%s: NADs still reference network %s; skipping ID release", c.name, previousNetworkName)
		} else {
			c.networkIDAllocator.ReleaseID(previousNetworkName)
			if c.isClusterManagerMode() {
				c.tunnelKeysAllocator.ReleaseKeys(previousNetworkName)
			}
		}
	}

	if new != nil && new.IsDefault() {
		return nil
	}

	id := types.InvalidID

	// check if in cache first
	if new != nil {
		id = c.networkIDAllocator.GetID(new.GetNetworkName())
		if id != types.InvalidID {
			klog.V(5).Infof("Previously cached network ID %d found for network: %s", id, new.GetNetworkName())
		}
	}
	if nad != nil && id == types.InvalidID {
		// check what ID is currently annotated
		if nad.Annotations[types.OvnNetworkIDAnnotation] != "" {
			annotated := nad.Annotations[types.OvnNetworkIDAnnotation]
			id, err = strconv.Atoi(annotated)
			if err != nil {
				return fmt.Errorf("failed to parse annotated network ID: %w", err)
			}
			klog.V(5).Infof("Previously annotated network ID %d found for NAD: %s/%s", id, nad.Namespace, nad.Name)
		}
	}

	tunnelKeys := []int{}
	// check what tunnel keys are currently annotated
	if nad != nil && nad.Annotations[types.OvnNetworkTunnelKeysAnnotation] != "" {
		tunnelKeys, err = util.ParseTunnelKeysAnnotation(nad.Annotations[types.OvnNetworkTunnelKeysAnnotation])
		if err != nil {
			return fmt.Errorf("failed to parse annotated tunnel keys: %w", err)
		}
	}

	// nothing to allocate, delete case
	if new == nil {
		return nil
	}
	name := new.GetNetworkName()

	// a network ID was annotated, check if it is free to use or stale
	if id != types.InvalidID {
		err = c.networkIDAllocator.ReserveID(name, id)
		if err != nil {
			// already reserved for a different network, allocate a new id
			id = types.InvalidID
		}
	}

	// this is not the cluster manager nad controller, and we are not allocating
	// so just return what ids we already found
	if !c.isClusterManagerMode() {
		new.SetNetworkID(id)
		new.SetTunnelKeys(tunnelKeys)
		return nil
	}

	// tunnel key annotation doesn't need the same check ^ because it is initialized outside the
	// nad controller and has already assured that all annotated tunnel keys are reserved.

	// we are about to allocate resources, so prepare a cleanup function
	// in case of error to release them.
	var allocatedNetworkID, allocatedTunnelKeys bool
	defer func() {
		if err != nil {
			if allocatedNetworkID {
				c.networkIDAllocator.ReleaseID(name)
			}
			if allocatedTunnelKeys {
				c.tunnelKeysAllocator.ReleaseKeys(name)
			}
		}
	}()
	// we don't have an ID, allocate a new one
	if id == types.InvalidID {
		id, err = c.networkIDAllocator.AllocateID(name)
		if err != nil {
			return fmt.Errorf("failed to allocate network ID: %w", err)
		}
		allocatedNetworkID = true

		// check if there is still a network running with that ID in the process
		// of being stopped
		other := c.networkController.getRunningNetwork(id)
		if other != "" && c.networkController.getNetwork(other) == nil {
			return fmt.Errorf("found other network %s being stopped with allocated ID %d, will retry", other, id)
		}
	}

	// allocate tunnel keys
	if len(tunnelKeys) != getNumberOfTunnelKeys(new) {
		if len(tunnelKeys) > 0 {
			// this should never happen, but if it does
			// AllocateKeys will add missing keys, when len(tunnelKeys) < getNumberOfTunnelKeys(new)
			// and will return an error when len(tunnelKeys) > getNumberOfTunnelKeys(new)
			// log an error for visibility
			klog.Errorf("Unexpected number of tunnel keys annotated on NAD for network %s, expected %d got %d",
				name, getNumberOfTunnelKeys(new), len(tunnelKeys))
		}
		tunnelKeys, err = c.tunnelKeysAllocator.AllocateKeys(name, id, getNumberOfTunnelKeys(new))
		if err != nil {
			return fmt.Errorf("failed to allocate tunnel keys: %w", err)
		}
		allocatedTunnelKeys = true
	}

	// set and annotate the network ID
	tunnelKeyAnno, err := util.FormatTunnelKeysAnnotation(tunnelKeys)
	if err != nil {
		return fmt.Errorf("failed to format tunnel keys annotation: %w", err)
	}
	annotations := map[string]string{
		types.OvnNetworkNameAnnotation:       name,
		types.OvnNetworkIDAnnotation:         strconv.Itoa(id),
		types.OvnNetworkTunnelKeysAnnotation: tunnelKeyAnno,
	}
	if nad.Annotations[types.OvnNetworkNameAnnotation] == annotations[types.OvnNetworkNameAnnotation] {
		delete(annotations, types.OvnNetworkNameAnnotation)
	}
	if nad.Annotations[types.OvnNetworkIDAnnotation] == annotations[types.OvnNetworkIDAnnotation] {
		delete(annotations, types.OvnNetworkIDAnnotation)
	}
	if nad.Annotations[types.OvnNetworkTunnelKeysAnnotation] == annotations[types.OvnNetworkTunnelKeysAnnotation] {
		delete(annotations, types.OvnNetworkTunnelKeysAnnotation)
	}
	if len(annotations) == 0 {
		new.SetNetworkID(id)
		new.SetTunnelKeys(tunnelKeys)
		return nil
	}

	k := kube.KubeOVN{
		NADClient: c.nadClient,
	}

	err = k.SetAnnotationsOnNAD(
		nad.Namespace,
		nad.Name,
		annotations,
		c.name,
	)
	if err != nil {
		return fmt.Errorf("failed to annotate network ID and/or tunnel keys on NAD: %w", err)
	}
	new.SetNetworkID(id)
	new.SetTunnelKeys(tunnelKeys)

	return nil
}

// networkReferencedLocked reports whether any NADs still reference a network.
// When excludeNAD is set, it is ignored in the reference check.
// Must be called with nadController locked.
func (c *nadController) networkReferencedLocked(networkName, excludeNAD string) bool {
	if networkName == "" || networkName == types.DefaultNetworkName {
		return false
	}
	nadSet := c.nadsByNetwork[networkName]
	if len(nadSet) == 0 {
		return false
	}
	if excludeNAD == "" {
		return len(nadSet) > 0
	}
	if nadSet.Has(excludeNAD) {
		return len(nadSet) > 1
	}
	return len(nadSet) > 0
}

func (c *nadController) getNetworkIDFromNode(nadNetwork util.NetInfo) (int, error) {
	// check if the node has a legacy ID
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return types.InvalidID, fmt.Errorf("failed to list nodes: %w", err)
	}
	// sort to make retrieval semi-consistent across nodes
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].CreationTimestamp.Before(&nodes[j].CreationTimestamp)
	})
	netName := nadNetwork.GetNetworkName()
	// Find from node annotations
	for _, node := range nodes {
		idMap, err := util.GetNodeNetworkIDsAnnotationNetworkIDs(node)
		if err != nil {
			continue
		}
		if v, ok := idMap[netName]; ok && v != types.InvalidID {
			return v, nil
		}
	}
	return types.InvalidID, nil
}

func (c *nadController) GetActiveNetwork(network string) util.NetInfo {
	c.RLock()
	defer c.RUnlock()
	state := c.networkController.getNetworkState(network)
	if state == nil {
		return nil
	}
	return state.controller
}

func (c *nadController) GetNetworkByID(id int) util.NetInfo {
	if id == types.InvalidID {
		return nil
	}
	netInfo := c.networkController.GetNetworkByID(id)
	if netInfo != nil {
		return netInfo
	}
	c.RLock()
	nadKeys := make([]string, 0, len(c.nads))
	for key := range c.nads {
		nadKeys = append(nadKeys, key)
	}
	c.RUnlock()
	// Handles the case where there is a cache miss. This is needed for filtered networks with
	// Dynamic UDN as there will be no network manager cache entry.
	// TODO (trozet): this is slow. We should optimize this by potentially storing the cache
	// of netInfos even for filtered entries.
	for _, key := range nadKeys {
		namespace, name, err := cache.SplitMetaNamespaceKey(key)
		if err != nil {
			continue
		}
		nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
		if err != nil {
			continue
		}
		nadNetwork, err := util.ParseNADInfo(nad)
		if err != nil || nadNetwork == nil {
			continue
		}
		if nadNetwork.GetNetworkID() == id {
			return nadNetwork
		}
	}
	return nil
}

func (c *nadController) isClusterManagerMode() bool {
	return c.tunnelKeysAllocator != nil
}

func getNumberOfTunnelKeys(netInfo util.NetInfo) int {
	if netInfo.IsDefault() {
		// default network does not need tunnel keys allocation because it always uses network ID 0.
		return 0
	}
	// Layer3, Secondary Layer2 and Localnet topologies need only 1 tunnel key for now that is derived from the network ID
	// and is limited by the MaxNetworks. Don't annotate any tunnel keys in that case until we decide to
	// increase the MaxNetworks.
	if netInfo.TopologyType() != types.Layer2Topology || !netInfo.IsPrimaryNetwork() {
		return 0
	}
	// Primary Layer2 UDNs need 2 tunnel keys: one for the layer2 switch and one for the transit router
	return 2
}
