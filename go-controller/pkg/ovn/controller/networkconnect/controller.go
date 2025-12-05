package networkconnect

import (
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
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
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// Controller manages network connectivity between (C)UDNs based on ClusterNetworkConnect CRs.
// It runs in each ovnkube-controller and creates OVN topology:
// - 1 connect-router for each CNC
// - ports connecting each selected (C)UDN network routers to connect-router
// - logical router policies on each selected (C)UDN's network router steering traffic to the connect-router
// - logical router static routes on the connect-router routing traffic to the corresponding selected (C)UDNs
type Controller struct {
	// zone is the name of the zone that this controller manages
	zone string

	// nbClient is the libovsdb northbound client interface
	nbClient libovsdbclient.Client

	// wf is the watch factory for accessing informers
	wf *factory.WatchFactory

	// listers
	cncLister  networkconnectlisters.ClusterNetworkConnectLister
	nodeLister corev1listers.NodeLister

	// networkManager provides access to network information
	networkManager networkmanager.Interface

	// cncController handles ClusterNetworkConnect events
	cncController controllerutil.Controller

	// nodeController handles Node events (for updating routes when nodes change)
	nodeController controllerutil.Controller
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

	c := &Controller{
		zone:           zone,
		nbClient:       nbClient,
		wf:             wf,
		cncLister:      cncLister,
		nodeLister:     nodeLister,
		networkManager: networkManager,
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

	return c
}

// Start starts the controller.
func (c *Controller) Start() error {
	klog.Infof("Starting ovnkube network connect controller for zone %s", c.zone)
	return controllerutil.Start(
		c.cncController,
		c.nodeController,
	)
}

// Stop stops the controller.
func (c *Controller) Stop() {
	controllerutil.Stop(
		c.cncController,
		c.nodeController,
	)
	klog.Infof("Stopped ovnkube network connect controller for zone %s", c.zone)
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

// reconcileCNC reconciles a ClusterNetworkConnect object.
func (c *Controller) reconcileCNC(key string) error {

	startTime := time.Now()
	_, cncName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	klog.V(5).Infof("Reconciling CNC %s", cncName)
	defer func() {
		klog.V(4).Infof("Reconciling CNC %s took %v", cncName, time.Since(startTime))
	}()

	_, err = c.cncLister.Get(cncName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// CNC was deleted, clean up OVN resources
			return c.cleanupCNC()
		}
		return err
	}

	return c.syncCNC()
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
	return c.requeueAllCNCs()
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
func (c *Controller) syncCNC() error {
	// STEP1: Create the connect router for the CNC using tunnel ID from CNC annotation
	// STEP2: Create the patch ports connecting network router's to the connect router
	// using IPs from the network subnet CNC annotation.
	// STEP3: If PodNetworkConnect is enabled, create the logical router policies on network router's
	// to steer traffic to the connect router for other connected networks.
	// STEP4: If PodNetworkConnect is enabled, add static routes to connect router towards
	// each of the connected networks.
	return nil
}

// cleanupCNC removes OVN resources for a deleted CNC.
func (c *Controller) cleanupCNC() error {
	return nil
}
