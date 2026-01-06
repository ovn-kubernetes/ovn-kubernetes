package topology

import (
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
)

// NodeHandler handles node reconciliation for a single network.
type NodeHandler interface {
	GetNetworkName() string
	ReconcileNode(oldNode, newNode *corev1.Node, oldState, newState *util.NodeAnnotationState) error
	DeleteNode(node *corev1.Node, state *util.NodeAnnotationState) error
	SyncNodes(nodes []*corev1.Node) error
}

// NodeController reconciles node topology for all active UDN networks.
type NodeController struct {
	name string

	nodeController controller.Controller
	nadReconciler  networkmanager.NADReconciler

	nadReconcilerID uint64
	networkManager  networkmanager.Interface
	nodeLister      v1.NodeLister

	handlerMu sync.RWMutex
	// handlers maps network name to node handler.
	handlers map[string]NodeHandler

	stateMu sync.RWMutex
	// nadToNetwork maps NAD key to network name.
	nadToNetwork map[string]string
	// networkRefs tracks how many NADs reference a network.
	networkRefs map[string]int
	// bootstrapped tracks whether we ran SyncNodes for a network.
	bootstrapped map[string]bool
	// bootstrapping tracks networks currently running SyncNodes to prevent
	// duplicate concurrent bootstrap attempts.
	bootstrapping map[string]bool
	// bootstrapNodes tracks nodes that should be treated as "new" per network.
	bootstrapNodes map[string]map[string]struct{}

	nodeCache nodeCache
	// annotationCache stores parsed annotation maps keyed by node.
	annotationCache *nodeAnnotationCache

	startMu sync.Mutex
	started bool
}

// NewNodeController builds a controller that handles node events for all UDNs.
func NewNodeController(wf *factory.WatchFactory, networkManager networkmanager.Interface) *NodeController {
	nodeInformer := wf.NodeCoreInformer()
	c := &NodeController{
		name:            "udn-node-topology",
		networkManager:  networkManager,
		nodeLister:      nodeInformer.Lister(),
		handlers:        map[string]NodeHandler{},
		nadToNetwork:    map[string]string{},
		networkRefs:     map[string]int{},
		bootstrapped:    map[string]bool{},
		bootstrapping:   map[string]bool{},
		bootstrapNodes:  map[string]map[string]struct{}{},
		nodeCache:       newNodeCache(),
		annotationCache: newNodeAnnotationCache(),
	}

	nodeControllerConfig := &controller.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.NewTypedItemFastSlowRateLimiter[string](time.Second, 5*time.Second, 5),
		Informer:       nodeInformer.Informer(),
		Lister:         nodeInformer.Lister().List,
		MaxAttempts:    controller.InfiniteAttempts,
		ObjNeedsUpdate: func(_, _ *corev1.Node) bool { return true },
		Reconcile:      c.reconcileNode,
		Threadiness:    15,
	}
	c.nodeController = controller.NewController(c.name+"-node", nodeControllerConfig)

	nadReconcilerConfig := &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   c.syncNAD,
		Threadiness: 15,
		MaxAttempts: controller.InfiniteAttempts,
	}
	c.nadReconciler = controller.NewReconciler(c.name+"-nad", nadReconcilerConfig)

	return c
}

// Start registers the NAD reconciler and starts node/NAD workers.
// If startup fails after registration, it deregisters the NAD reconciler before
// returning the startup error.
func (c *NodeController) Start() error {
	id, err := c.networkManager.RegisterNADReconciler(c.nadReconciler)
	if err != nil {
		return err
	}
	c.nadReconcilerID = id

	err = controller.StartWithInitialSync(c.initialSync, c.nodeController, c.nadReconciler)
	if err != nil {
		if deregErr := c.networkManager.DeRegisterNADReconciler(c.nadReconcilerID); deregErr != nil {
			klog.Warningf("%s: failed to deregister NAD reconciler after startup failure: %v", c.name, deregErr)
		}
		c.nadReconcilerID = 0
		return err
	}

	c.startMu.Lock()
	c.started = true
	c.startMu.Unlock()
	return nil
}

// Stop stops all workers and de-registers the NAD reconciler.
func (c *NodeController) Stop() {
	c.startMu.Lock()
	c.started = false
	c.startMu.Unlock()
	if c.nadReconcilerID != 0 {
		if err := c.networkManager.DeRegisterNADReconciler(c.nadReconcilerID); err != nil {
			klog.Warningf("%s: failed to deregister NAD reconciler: %v", c.name, err)
		}
	}
	controller.Stop(c.nodeController, c.nadReconciler)
	c.nadReconciler = nil
	c.nadReconcilerID = 0
}

// Reconcile queues a node key for reconciliation.
func (c *NodeController) Reconcile(key string) {
	c.nodeController.Reconcile(key)
}

// AnnotationCache returns the cache used for parsed node annotations.
func (c *NodeController) AnnotationCache() util.NodeAnnotationCache {
	return c.annotationCache
}

// RegisterNetworkController registers or replaces a per-network node handler.
// If the controller is already started and the network is active, it attempts
// an immediate bootstrap for that network.
func (c *NodeController) RegisterNetworkController(handler NodeHandler) {
	if handler == nil {
		return
	}
	netName := handler.GetNetworkName()
	c.handlerMu.Lock()
	c.handlers[netName] = handler
	c.handlerMu.Unlock()

	if c.isStarted() && c.claimBootstrap(netName) {
		defer c.releaseBootstrapClaim(netName)
		if err := c.bootstrapNetwork(netName); err != nil {
			klog.Errorf("%s: failed to bootstrap network %s: %v", c.name, netName, err)
		}
	}
}

// isStarted returns true if Start() has completed.
func (c *NodeController) isStarted() bool {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	return c.started
}

// reconcileNode handles node add/update/delete by comparing cached state.
func (c *NodeController) reconcileNode(key string) error {
	node, err := c.nodeLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.reconcileDelete(key)
		}
		return err
	}

	oldNode := c.nodeCache.Get(key)
	err = c.reconcileUpdate(oldNode, node)
	if err == nil {
		c.nodeCache.Set(node)
	}
	return err
}

// reconcileUpdate reconciles per-network node changes.
func (c *NodeController) reconcileUpdate(oldNode, newNode *corev1.Node) error {
	handlers := c.getHandlersSnapshot()
	if len(handlers) == 0 {
		return nil
	}

	oldState := util.BuildNodeAnnotationState(oldNode, c.annotationCache)
	newState := util.BuildNodeAnnotationState(newNode, c.annotationCache)

	var errs []error
	for _, handler := range handlers {
		netName := handler.GetNetworkName()
		if !c.isNetworkActive(netName) {
			continue
		}
		oldNodeForNetwork := oldNode
		oldStateForNetwork := oldState
		if c.isBootstrapNode(netName, newNode.Name) {
			oldNodeForNetwork = nil
			oldStateForNetwork = nil
		}
		if err := handler.ReconcileNode(oldNodeForNetwork, newNode, oldStateForNetwork, newState); err != nil {
			errs = append(errs, fmt.Errorf("network %s: %w", netName, err))
			continue
		}
		c.markBootstrapNodeDone(netName, newNode.Name)
	}
	return utilerrors.Join(errs...)
}

// reconcileDelete handles deletion using cached state.
func (c *NodeController) reconcileDelete(key string) error {
	oldNode := c.nodeCache.Get(key)
	if oldNode == nil {
		return nil
	}

	handlers := c.getHandlersSnapshot()
	if len(handlers) == 0 {
		return nil
	}

	oldState := util.BuildNodeAnnotationState(oldNode, c.annotationCache)

	var errs []error
	for _, handler := range handlers {
		netName := handler.GetNetworkName()
		if !c.isNetworkActive(netName) {
			continue
		}
		if err := handler.DeleteNode(oldNode, oldState); err != nil {
			errs = append(errs, fmt.Errorf("network %s: %w", netName, err))
			continue
		}
		c.markBootstrapNodeDone(netName, oldNode.Name)
	}
	if len(errs) == 0 {
		c.nodeCache.Delete(key)
		c.annotationCache.DeleteNode(key)
	}
	return utilerrors.Join(errs...)
}

// syncNAD updates network activity state from a NAD key change.
// Newly-active networks are bootstrapped once; otherwise all nodes are queued
// for reconcile to apply potential active-network changes.
func (c *NodeController) syncNAD(key string) error {
	netInfo := c.networkManager.GetNetInfoForNADKey(key)
	netName, becameActive := c.updateNetworkIndex(key, netInfo)
	if netName == "" {
		return nil
	}

	if becameActive && c.isStarted() && c.claimBootstrap(netName) {
		defer c.releaseBootstrapClaim(netName)
		return c.bootstrapNetwork(netName)
	}

	c.nodeController.ReconcileAll()
	return nil
}

// initialSync runs before worker start to bootstrap active networks.
func (c *NodeController) initialSync() error {
	return c.bootstrapActiveNetworks()
}

// bootstrapActiveNetworks runs SyncNodes for each active, non-bootstrapped
// network during initial startup.
// A network is marked bootstrapped only when its SyncNodes call succeeds.
// ReconcileAll runs only when all active networks bootstrap successfully.
func (c *NodeController) bootstrapActiveNetworks() error {
	handlers := c.getHandlersSnapshot()
	if len(handlers) == 0 {
		return nil
	}

	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return err
	}

	var errs []error
	for _, handler := range handlers {
		netName := handler.GetNetworkName()
		if !c.isNetworkActive(netName) || c.isBootstrapped(netName) {
			continue
		}
		if err := handler.SyncNodes(nodes); err != nil {
			errs = append(errs, fmt.Errorf("network %s: %w", netName, err))
			continue
		}
		c.setBootstrapState(netName, nodes)
	}
	if len(errs) == 0 {
		c.nodeController.ReconcileAll()
	}
	return utilerrors.Join(errs...)
}

// bootstrapNetwork runs SyncNodes for one network that just became active.
// On success it marks bootstrap state and triggers a full node reconcile.
func (c *NodeController) bootstrapNetwork(netName string) error {
	handler := c.getHandler(netName)
	if handler == nil {
		return nil
	}

	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return err
	}
	if err := handler.SyncNodes(nodes); err != nil {
		return err
	}

	c.setBootstrapState(netName, nodes)
	c.nodeController.ReconcileAll()
	return nil
}

// getHandlersSnapshot returns a stable copy of registered handlers.
func (c *NodeController) getHandlersSnapshot() []NodeHandler {
	c.handlerMu.RLock()
	defer c.handlerMu.RUnlock()
	handlers := make([]NodeHandler, 0, len(c.handlers))
	for _, handler := range c.handlers {
		handlers = append(handlers, handler)
	}
	return handlers
}

// getHandler returns the handler for a network if registered.
func (c *NodeController) getHandler(netName string) NodeHandler {
	c.handlerMu.RLock()
	defer c.handlerMu.RUnlock()
	return c.handlers[netName]
}

// isNetworkActive returns true if a network has at least one NAD reference.
func (c *NodeController) isNetworkActive(netName string) bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.networkRefs[netName] > 0
}

// isBootstrapped returns true if SyncNodes ran for a network.
func (c *NodeController) isBootstrapped(netName string) bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.bootstrapped[netName]
}

// setBootstrapState stores bootstrap node tracking and marks a network
// bootstrapped only when it is still active.
func (c *NodeController) setBootstrapState(netName string, nodes []*corev1.Node) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.networkRefs[netName] == 0 {
		return
	}
	if len(nodes) != 0 {
		nodeSet := make(map[string]struct{}, len(nodes))
		for _, node := range nodes {
			nodeSet[node.Name] = struct{}{}
		}
		c.bootstrapNodes[netName] = nodeSet
	}
	c.bootstrapped[netName] = true
}

// claimBootstrap atomically claims bootstrap work for an active, unbootstrapped
// network. Only one goroutine can hold the claim at a time.
func (c *NodeController) claimBootstrap(netName string) bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.networkRefs[netName] == 0 || c.bootstrapped[netName] {
		return false
	}
	if c.bootstrapping == nil {
		c.bootstrapping = map[string]bool{}
	}
	if c.bootstrapping[netName] {
		return false
	}
	c.bootstrapping[netName] = true
	return true
}

// releaseBootstrapClaim releases a previously acquired bootstrap claim.
func (c *NodeController) releaseBootstrapClaim(netName string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	delete(c.bootstrapping, netName)
}

// isBootstrapNode returns true if a node should be treated as new for a network.
func (c *NodeController) isBootstrapNode(netName, nodeName string) bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	nodes := c.bootstrapNodes[netName]
	if len(nodes) == 0 {
		return false
	}
	_, ok := nodes[nodeName]
	return ok
}

// markBootstrapNodeDone clears bootstrap tracking for a node.
func (c *NodeController) markBootstrapNodeDone(netName, nodeName string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	nodes := c.bootstrapNodes[netName]
	if len(nodes) == 0 {
		return
	}
	delete(nodes, nodeName)
	if len(nodes) == 0 {
		delete(c.bootstrapNodes, netName)
	}
}

// updateNetworkIndex updates NAD->network mapping and per-network NAD refcounts.
// It returns the current network name for the NAD key (or "" if removed) and a
// flag indicating whether that network transitioned from inactive to active.
func (c *NodeController) updateNetworkIndex(nadKey string, netInfo util.NetInfo) (string, bool) {
	var netName string
	if netInfo != nil {
		netName = netInfo.GetNetworkName()
	}

	c.stateMu.Lock()
	defer c.stateMu.Unlock()

	oldNet := c.nadToNetwork[nadKey]
	if netName == "" {
		if oldNet == "" {
			return "", false
		}
		delete(c.nadToNetwork, nadKey)
		c.decrementNetworkRef(oldNet)
		return "", false
	}

	if oldNet == netName {
		return netName, false
	}
	if oldNet != "" {
		c.decrementNetworkRef(oldNet)
	}
	c.nadToNetwork[nadKey] = netName
	becameActive := c.incrementNetworkRef(netName)
	return netName, becameActive
}

// incrementNetworkRef increments the NAD ref count for netName and returns
// true when the network becomes active (count transitions from 0 to 1).
func (c *NodeController) incrementNetworkRef(netName string) bool {
	c.networkRefs[netName]++
	return c.networkRefs[netName] == 1
}

// decrementNetworkRef decrements the NAD ref count and clears bootstrap state
// when the network becomes inactive.
func (c *NodeController) decrementNetworkRef(netName string) {
	count := c.networkRefs[netName] - 1
	if count <= 0 {
		delete(c.networkRefs, netName)
		delete(c.bootstrapped, netName)
		delete(c.bootstrapping, netName)
		delete(c.bootstrapNodes, netName)
		return
	}
	c.networkRefs[netName] = count
}

type nodeCache struct {
	mu    sync.RWMutex
	nodes map[string]*corev1.Node
}

// newNodeCache returns an empty node cache.
func newNodeCache() nodeCache {
	return nodeCache{nodes: map[string]*corev1.Node{}}
}

// Get returns a deep copy of a cached node by name.
func (c *nodeCache) Get(name string) *corev1.Node {
	c.mu.RLock()
	defer c.mu.RUnlock()
	node := c.nodes[name]
	if node == nil {
		return nil
	}
	return node.DeepCopy()
}

// Set stores a deep copy of a node.
func (c *nodeCache) Set(node *corev1.Node) {
	if node == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nodes[node.Name] = node.DeepCopy()
}

// Delete removes a node from the cache.
func (c *nodeCache) Delete(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.nodes, name)
}
