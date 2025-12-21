package networkconnect

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	controllerutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	networkconnectlisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/listers/clusternetworkconnect/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	controllerName = "ovnkube-network-connect-controller"
)

// Controller manages network connectivity between (C)UDNs based on ClusterNetworkConnect CRs.
// It runs in each ovnkube-controller and creates OVN topology:
// - 1 connect-router for each CNC
// - ports connecting each selected (C)UDN network routers to connect-router
// - logical router policies on each selected (C)UDN's network router steering traffic to the connect-router
// - logical router static routes on the connect-router routing traffic to the corresponding selected (C)UDNs
// - load balancer attachments for ClusterIPServiceNetwork connectivity enabled CNCs
type Controller struct {
	// zone is the name of the zone that this controller manages
	zone string

	// nbClient is the libovsdb northbound client interface
	nbClient libovsdbclient.Client

	// wf is the watch factory for accessing informers
	wf *factory.WatchFactory

	// listers
	cncLister     networkconnectlisters.ClusterNetworkConnectLister
	nodeLister    corev1listers.NodeLister
	nadLister     nadlisters.NetworkAttachmentDefinitionLister
	serviceLister corev1listers.ServiceLister

	// networkManager provides access to network information
	networkManager networkmanager.Interface

	// cncController handles ClusterNetworkConnect events
	cncController controllerutil.Controller

	// nodeController handles Node events (for updating routes when nodes change)
	nodeController controllerutil.Controller

	// nadReconciler handles NAD-triggered CNC requeues
	nadReconciler   networkmanager.NADReconciler
	nadReconcilerID uint64
	// serviceController handles Service events (for ClusterIPServiceNetwork connectivity)
	serviceController controllerutil.Controller

	// Single global lock protecting all controller state
	sync.RWMutex

	// cncCache holds the state for each CNC keyed by CNC name
	cncCache map[string]*networkConnectState

	// localZoneNode is the node in this controller's zone.
	// We only support 1 node per zone for this feature.
	// Updated during each CNC reconciliation via computeNodeInfo().
	localZoneNode *corev1.Node
}

// networkConnectState tracks the state of a single ClusterNetworkConnect
type networkConnectState struct {
	// name of the ClusterNetworkConnect
	name string
	// tunnelID for the connect router
	tunnelID int
	// connectedNetworks is the set of owner keys (e.g., "layer3_1", "layer2_2") for networks
	// connected by this CNC. Used to track OVN resources created and detect NAD matching changes.
	connectedNetworks sets.Set[string]
	// clusterIPServiceNetworkEnabled tracks whether ClusterIPServiceNetwork connectivity is enabled
	// Used for cleanup when the connectivity type is removed from the CNC spec.
	clusterIPServiceNetworkEnabled bool
}

// NewController creates a new network connect controller for ovnkube-controller.
func NewController(
	zone string,
	nbClient libovsdbclient.Client,
	wf *factory.WatchFactory,
	networkManager networkmanager.Interface,
) *Controller {
	cncLister := wf.ClusterNetworkConnectInformer().Lister()
	nodeLister := wf.NodeCoreInformer().Lister()
	nadLister := wf.NADInformer().Lister()
	serviceLister := wf.ServiceCoreInformer().Lister()

	c := &Controller{
		zone:           zone,
		nbClient:       nbClient,
		wf:             wf,
		cncLister:      cncLister,
		nodeLister:     nodeLister,
		nadLister:      nadLister,
		serviceLister:  serviceLister,
		networkManager: networkManager,
		cncCache:       make(map[string]*networkConnectState),
	}

	cncCfg := &controllerutil.ControllerConfig[networkconnectv1.ClusterNetworkConnect]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.ClusterNetworkConnectInformer().Informer(),
		Lister:         cncLister.List,
		Reconcile:      c.reconcileCNC,
		ObjNeedsUpdate: cncNeedsUpdate,
		Threadiness:    1,
	}
	c.cncController = controllerutil.NewController(
		"ovnkube-network-connect-controller",
		cncCfg,
	)

	nodeCfg := &controllerutil.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.NodeCoreInformer().Informer(),
		Lister:         nodeLister.List,
		Reconcile:      c.reconcileNode,
		ObjNeedsUpdate: nodeNeedsUpdate,
		Threadiness:    1,
	}
	c.nodeController = controllerutil.NewController(
		"ovnkube-network-connect-node-controller",
		nodeCfg,
	)

	// this controller does not feed from an informer, nads are added
	// to the queue by NAD Controller
	nadReconcilerConfig := &controllerutil.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   c.syncNAD,
		Threadiness: 1,
		MaxAttempts: controllerutil.InfiniteAttempts,
	}
	c.nadReconciler = controllerutil.NewReconciler(
		"ovnkube-network-connect-nad",
		nadReconcilerConfig,
	)

	serviceCfg := &controllerutil.ControllerConfig[corev1.Service]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.ServiceCoreInformer().Informer(),
		Lister:         serviceLister.List,
		Reconcile:      c.reconcileService,
		ObjNeedsUpdate: serviceNeedsUpdate,
		Threadiness:    1,
	}
	c.serviceController = controllerutil.NewController(
		"ovnkube-network-connect-service-controller",
		serviceCfg,
	)

	return c
}

// Start starts the controller.
func (c *Controller) Start() error {
	klog.Infof("Starting ovnkube network connect controller for zone %s", c.zone)
	if c.nadReconciler == nil {
		return controllerutil.Start(
			c.cncController,
			c.nodeController,
			c.serviceController,
		)
	}
	id, err := c.networkManager.RegisterNADReconciler(c.nadReconciler)
	if err != nil {
		return err
	}
	c.nadReconcilerID = id
	return controllerutil.Start(
		c.cncController,
		c.nodeController,
		c.nadReconciler,
		c.serviceController,
	)
}

// Stop stops the controller.
func (c *Controller) Stop() {
	if c.nadReconcilerID != 0 {
		if err := c.networkManager.DeRegisterNADReconciler(c.nadReconcilerID); err != nil {
			klog.Warningf("Failed to deregister CNC NAD reconciler: %v", err)
		}
	}
	if c.nadReconciler != nil {
		controllerutil.Stop(
			c.cncController,
			c.nodeController,
			c.nadReconciler,
			c.serviceController,
		)
	} else {
		controllerutil.Stop(
			c.cncController,
			c.nodeController,
			c.serviceController,
		)
	}
	c.nadReconciler = nil
	c.nadReconcilerID = 0
	klog.Infof("Stopped ovnkube network connect controller for zone %s", c.zone)
}

func (c *Controller) syncNAD(key string) error {
	if c.nadLister == nil || c.networkManager == nil {
		return nil
	}
	nadNetwork := c.networkManager.GetNetInfoForNADKey(key)
	if nadNetwork == nil {
		return nil
	}
	networkID := nadNetwork.GetNetworkID()
	if networkID == types.InvalidID {
		return nil
	}
	cncs, err := c.cncLister.List(labels.Everything())
	if err != nil {
		return err
	}
	for _, cnc := range cncs {
		subnets, err := util.ParseNetworkConnectSubnetAnnotation(cnc)
		if err != nil {
			klog.Warningf("Failed parsing CNC %s subnet annotation: %v", cnc.Name, err)
			continue
		}
		shouldReconcile := false
		for owner := range subnets {
			_, ownerNetworkID, err := util.ParseNetworkOwner(owner)
			if err != nil {
				continue
			}
			if ownerNetworkID == networkID {
				shouldReconcile = true
				break
			}
		}
		if shouldReconcile {
			c.cncController.Reconcile(cnc.Name)
		}
	}
	return nil
}

// cncNeedsUpdate determines if a CNC update requires reconciliation.
func cncNeedsUpdate(oldObj, newObj *networkconnectv1.ClusterNetworkConnect) bool {
	// Always process create/delete
	if oldObj == nil || newObj == nil {
		return true
	}

	// Process if annotations changed (subnet or tunnel key updates from cluster manager)
	// this event is triggered when the cluster manager updates the annotations on the CNC object
	// based on CNC or NAD or namespace changes.
	// Since we watch for the annotation updates on CNC objects, we don't need to directly
	// watch for NAD or namespace changes or even CNC network selectors changes as part of this controller.
	if util.NetworkConnectSubnetAnnotationChanged(oldObj, newObj) || util.NetworkConnectTunnelKeyAnnotationsChanged(oldObj, newObj) {
		return true
	}

	// Process if connectivity changed
	if !reflect.DeepEqual(oldObj.Spec.Connectivity, newObj.Spec.Connectivity) {
		return true
	}

	return false
}

// nodeNeedsUpdate determines if a node update requires reconciliation.
func nodeNeedsUpdate(oldObj, newObj *corev1.Node) bool {
	// Always process create/delete
	if oldObj == nil || newObj == nil {
		return true
	}

	// Process if zone annotation changed (affects router port creation for l3 network connectivity
	// since the port needs to be toggled between a remote and local port on the connect router)
	if util.NodeZoneAnnotationChanged(oldObj, newObj) {
		return true
	}

	// Process if node subnet annotation changed (affects static routes)
	if util.NodeSubnetAnnotationChanged(oldObj, newObj) {
		return true
	}

	// Process if node ID annotation changed (the only supported scenario
	// is when the node is added and get's an annotation update once the nodeID is allocated)
	return util.NodeIDAnnotationChanged(oldObj, newObj) && oldObj.Annotations[util.OvnNodeID] == ""
}

// serviceNeedsUpdate determines if a service change requires reconciliation.
// We only care about service CREATE for ClusterIPServiceNetwork connectivity.
// Service deletes don't need processing because:
//   - LogicalSwitch.load_balancer is a weak reference with min=0 and refType=weak
//   - When the LB is deleted by services controller, OVSDB automatically
//     garbage-collects the UUID from all switches (including cross-network attachments)
//
// Service updates (like endpoint changes) are handled by the services controller.
// We only need to care about load balancer attachments. LB Updates or deattachments
// are handled by the services controller.
func serviceNeedsUpdate(oldObj, newObj *corev1.Service) bool {
	// Only process create (oldObj == nil)
	// Delete events are handled automatically by OVN weak reference cleanup
	if oldObj == nil && newObj != nil {
		return true
	}
	// Skip updates and deletes- services controller handles LB content updates,
	// we only care about service existence for LB attachment purposes.
	return false
}

// reconcileCNC reconciles a ClusterNetworkConnect object.
func (c *Controller) reconcileCNC(key string) error {
	c.Lock()
	defer c.Unlock()

	startTime := time.Now()
	_, cncName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	klog.V(5).Infof("Reconciling CNC %s", cncName)
	defer func() {
		klog.V(4).Infof("Reconciling CNC %s took %v", cncName, time.Since(startTime))
	}()

	cnc, err := c.cncLister.Get(cncName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// CNC was deleted, clean up OVN resources
			return c.cleanupCNC(cncName)
		}
		return err
	}

	return c.syncCNC(cnc)
}

// reconcileNode reconciles node changes that might affect network connectivity.
func (c *Controller) reconcileNode(key string) error {
	startTime := time.Now()
	klog.V(5).Infof("Reconciling node %s for network connect", key)
	defer func() {
		klog.V(4).Infof("Reconciling node %s for network connect took %v", key, time.Since(startTime))
	}()

	_, err := c.nodeLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Node was deleted, requeue all CNCs to update routes
			return c.requeueAllCNCs()
		}
		return err
	}

	// Requeue all CNCs to update routes for this node.
	// We process ALL nodes (not just our zone) because the connect-router
	// needs static routes to all node subnets across all zones.
	// TODO (trozet): we should check what changed in the node and only reconcile affected CNCs
	return c.requeueAllCNCs()
}

// reconcileService reconciles service changes for ClusterIPServiceNetwork connectivity.
// When a service is created/deleted, we check if its network is connected by any CNC
// with ClusterIPServiceNetwork enabled and requeue those CNCs.
// This follows the same pattern as reconcileNAD in cluster manager.
func (c *Controller) reconcileService(key string) error {
	c.RLock()
	defer c.RUnlock()

	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	klog.V(5).Infof("Reconciling service %s/%s for network connect", namespace, name)
	defer func() {
		klog.V(4).Infof("Reconciling service %s/%s for network connect took %v", namespace, name, time.Since(startTime))
	}()

	// Get the service
	svc, err := c.serviceLister.Services(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// Service may be nil if deleted (this shouldn't happen since we only process create events)
	if svc == nil {
		klog.V(5).Infof("Service %s/%s deleted, skipping network connect reconciliation", namespace, name)
		return nil
	}

	// Get the network owner key for this service's namespace.
	// This determines which network the service belongs to.
	networkOwnerKey, err := c.getNetworkOwnerKeyForNamespace(namespace)
	if err != nil {
		// Log and skip - namespace might not have a primary UDN
		klog.V(5).Infof("Could not get network owner key for namespace %s: %v", namespace, err)
		return nil
	}
	if networkOwnerKey == "" {
		// Namespace uses default network, which we don't handle for network-connect
		klog.V(5).Infof("Namespace %s uses default network, skipping service %s", namespace, name)
		return nil
	}

	// List all CNCs and check which ones need to be reconciled for this service
	existingCNCs, err := c.cncLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list CNCs: %w", err)
	}

	// Process each CNC to check if this service's network matching state changed
	for _, cnc := range existingCNCs {
		if c.mustProcessCNCForService(svc, cnc, networkOwnerKey) {
			c.cncController.Reconcile(cnc.Name)
		}
	}

	return nil
}

// getNetworkOwnerKeyForNamespace returns the owner key (e.g., "layer3_5") for the primary network
// of the given namespace. Returns empty string if namespace uses default network.
func (c *Controller) getNetworkOwnerKeyForNamespace(namespace string) (string, error) {
	// Get the primary network for this namespace
	primaryNetwork, err := c.networkManager.GetActiveNetworkForNamespace(namespace)
	if err != nil {
		if util.IsUnprocessedActiveNetworkError(err) {
			// Network not yet processed, skip for now. When the network is processed,
			// syncNAD requeues the CNC and syncNetworkConnections will do a full DB scan
			// to find all _cluster LBs (including any created while the network was unprocessed).
			return "", nil
		}
		return "", fmt.Errorf("failed to get primary network for namespace %s: %w", namespace, err)
	}

	// Check if it's the default network
	if primaryNetwork.IsDefault() {
		return "", nil
	}

	// Build owner key: "layer3_{networkID}" or "layer2_{networkID}"
	return util.ComputeNetworkOwner(primaryNetwork.TopologyType(), primaryNetwork.GetNetworkID()), nil
}

// mustProcessCNCForService checks if:
// 1. the service's network is currently in the CNC's connectedNetworks AND
// 2. the CNC has ClusterIPServiceNetwork enabled
// Returns true if both conditions are met, meaning we need to attach this service's LB
// to the other connected networks' switches.
// This function is READ-ONLY and does not update the cache.
// NOTE: Caller must hold at least RLock.
func (c *Controller) mustProcessCNCForService(svc *corev1.Service, cnc *networkconnectv1.ClusterNetworkConnect, networkOwnerKey string) bool {
	// Check if CNC has ClusterIPServiceNetwork enabled - if not, skip entirely
	if !serviceConnectivityEnabled(cnc) {
		return false
	}

	cncState, cncExists := c.cncCache[cnc.Name]

	// If CNC state doesn't exist yet, we don't know the connected networks
	// Cache will be populated during CNC reconciliation and that will trigger
	// service reconciliation as well from reconcileCNC.
	if !cncExists {
		klog.V(5).Infof("CNC %s state not found in cache, skipping service %s/%s", cnc.Name, svc.Namespace, svc.Name)
		return false
	}

	// Check if service's network is connected by this CNC
	isConnected := cncState.connectedNetworks.Has(networkOwnerKey)

	if isConnected {
		klog.V(5).Infof("Service %s/%s network %s is connected by CNC %s with ClusterIPServiceNetwork",
			svc.Namespace, svc.Name, networkOwnerKey, cnc.Name)
	}

	return isConnected
}

// serviceConnectivityEnabled checks if the CNC has ClusterIPServiceNetwork in its connectivity spec.
func serviceConnectivityEnabled(cnc *networkconnectv1.ClusterNetworkConnect) bool {
	for _, ct := range cnc.Spec.Connectivity {
		if ct == networkconnectv1.ClusterIPServiceNetwork {
			return true
		}
	}
	return false
}

// requeueAllCNCs requeues all CNCs for reconciliation.
func (c *Controller) requeueAllCNCs() error {
	cncs, err := c.cncLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list CNCs: %v", err)
	}

	for _, cnc := range cncs {
		c.cncController.Reconcile(cnc.Name)
	}
	return nil
}

// syncCNC synchronizes the OVN topology for a CNC.
func (c *Controller) syncCNC(cnc *networkconnectv1.ClusterNetworkConnect) error {
	// Get or create CNC state
	cncState, exists := c.cncCache[cnc.Name]
	if !exists {
		// this means its CNC create event
		cncState = &networkConnectState{
			name:              cnc.Name,
			connectedNetworks: sets.New[string](),
		}
		c.cncCache[cnc.Name] = cncState
	}
	// STEP1: Create the connect router for the CNC using tunnel ID from CNC annotation
	// This is always a one time operation - Every CNC has exactly one connect router.
	// tunnelID is set by cluster manager during CNC creation and is considered immutable
	if cncState.tunnelID == 0 {
		// Parse tunnel key from annotation (set by cluster manager)
		tunnelID, err := util.ParseNetworkConnectTunnelKeyAnnotation(cnc)
		if err != nil {
			return fmt.Errorf("failed to parse tunnel key annotation for CNC %s: %v", cnc.Name, err)
		}
		if tunnelID == 0 {
			klog.V(4).Infof("CNC %s does not have tunnel key annotation yet, waiting for cluster manager", cnc.Name)
			// we don't return error here because we want to wait for the cluster manager to set the annotation
			// and cncUpdate event will trigger the reconciliation again.
			return nil
		}
		// Create the connect router
		if err := c.ensureConnectRouter(cnc, tunnelID); err != nil {
			return fmt.Errorf("failed to ensure connect router for CNC %s: %v", cnc.Name, err)
		}
		cncState.tunnelID = tunnelID
	}
	allocatedSubnets, err := util.ParseNetworkConnectSubnetAnnotation(cnc)
	if err != nil {
		return fmt.Errorf("failed to parse subnet annotation for CNC %s: %w", cnc.Name, err)
	}

	if err := c.syncNetworkConnections(cnc, allocatedSubnets); err != nil {
		return fmt.Errorf("failed to sync network connections for CNC %s: %v", cnc.Name, err)
	}
	return nil
}

// cleanupCNC removes OVN resources for a deleted CNC.
func (c *Controller) cleanupCNC(cncName string) error {
	klog.V(4).Infof("Cleaning up CNC %s", cncName)

	cncState, exists := c.cncCache[cncName]
	if !exists {
		klog.V(4).Infof("CNC %s not found in cache, nothing to clean up", cncName)
		return nil
	}

	// Cleanup network connections (includes service connectivity, ports, and policies)
	if err := c.cleanupNetworkConnections(cncName, cncState.clusterIPServiceNetworkEnabled); err != nil {
		return fmt.Errorf("failed to cleanup network connections for CNC %s: %v", cncName, err)
	}

	// Remove the connect router
	if err := c.deleteConnectRouter(cncName); err != nil {
		return fmt.Errorf("failed to delete connect router for CNC %s: %v", cncName, err)
	}

	// Remove from cache
	delete(c.cncCache, cncName)
	klog.V(4).Infof("Cleaned up CNC %s", cncName)

	return nil
}
