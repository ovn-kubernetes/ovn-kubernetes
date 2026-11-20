// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
	"time"

	nadtypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	ralisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/listers/routeadvertisements/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// defaultNetworkReconcilerThreadiness defines the number of worker goroutines
	// for processing network reconciliation events. Value of 5 provides good
	// parallelism for bulk UDN creation while controlling resource consumption.
	// Aligns with Kubernetes controller-manager conventions and UDN service controller.
	defaultNetworkReconcilerThreadiness = 5
)

func newNetworkController(name, node string, cm ControllerManager, wf watchFactory, asyncNetworkStart bool) *networkController {
	nc := &networkController{
		name:                   fmt.Sprintf("[%s network controller]", name),
		node:                   node,
		cm:                     cm,
		asyncNetworkStart:      asyncNetworkStart,
		networks:               map[string]util.MutableNetInfo{},
		networksByID:           map[int]string{},
		networkControllers:     map[string]*networkControllerState{},
		pendingNetworkRefNodes: map[string]sets.Set[string]{},
		networkRefStates:       map[string]map[string]bool{},
	}

	// this controller does not feed from an informer, networks are manually
	// added to the queue for processing
	networkConfig := &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   nc.syncNetwork,
		Threadiness: defaultNetworkReconcilerThreadiness,
		MaxAttempts: controller.InfiniteAttempts,
	}
	nc.networkReconciler = controller.NewReconciler(
		nc.name,
		networkConfig,
	)

	// Both node agents use their local FRR configurations to program the datapath.
	if node != "" && util.IsRouteAdvertisementsEnabled() {
		nc.nadLister = wf.NADInformer().Lister()
		nadIndexer := wf.NADInformer().Informer().GetIndexer()
		nc.getNADsForRouteAdvertisement = func(raName string) ([]*nadtypes.NetworkAttachmentDefinition, error) {
			return factory.GetNADsForRouteAdvertisement(nadIndexer, raName)
		}
		nc.nodeLister = wf.NodeCoreInformer().Lister()
		nc.frrIndexer = wf.FRRConfigurationsInformer().Informer().GetIndexer()
		nc.lastLegacyIntents = make(map[string]advertisementIntents)
		nc.legacyRALister = wf.RouteAdvertisementsInformer().Lister()
		nc.legacyRAController = controller.NewController(nc.name+" legacy RA", &controller.ControllerConfig[ratypes.RouteAdvertisements]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Informer:    wf.RouteAdvertisementsInformer().Informer(),
			Lister:      nc.legacyRALister.List,
			Reconcile:   nc.syncRouteAdvertisements,
			ObjNeedsUpdate: func(old, new *ratypes.RouteAdvertisements) bool {
				if old != nil && old.UID != new.UID {
					return true
				}
				before, wasPending := legacyRAIntent(old)
				after, pending := legacyRAIntent(new)
				return pending != wasPending || !reflect.DeepEqual(before, after)
			},
			Threadiness: 1,
		})
		nc.frrController = controller.NewController(nc.name+" FRR", &controller.ControllerConfig[frrtypes.FRRConfiguration]{
			RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
			Informer:       wf.FRRConfigurationsInformer().Informer(),
			Lister:         wf.FRRConfigurationsInformer().Lister().List,
			Reconcile:      func(_ context.Context, _ string) error { return nc.syncNetworks() },
			ObjNeedsUpdate: frrNeedsUpdate,
			Threadiness:    1,
		})

		nodeConfig := &controller.ControllerConfig[corev1.Node]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Informer:    wf.NodeCoreInformer().Informer(),
			Lister:      nc.nodeLister.List,
			Reconcile: func(_ context.Context, nodeName string) error {
				if nodeName != node {
					return nil
				}
				return nc.syncNetworks()
			},
			ObjNeedsUpdate: nodeNeedsUpdate,
			Threadiness:    1,
		}
		nc.nodeController = controller.NewController(
			nc.name,
			nodeConfig,
		)
	}

	return nc
}

type networkControllerState struct {
	controller         NetworkController
	stoppedAndDeleting bool
	// startFailed retains a stopped controller whose Cleanup method still owns
	// any resources left by Start. Cleanup must succeed before deletion or a
	// replacement Start so two controllers never own the same network state.
	startFailed bool
}

type advertisementIntents map[string]util.RouteAdvertisementsIntent

type networkController struct {
	sync.RWMutex

	name string
	node string
	// asyncNetworkStart, when true, skips inline network start during initial
	// sync so reconciler workers start networks with bounded concurrency.
	// Used by ovnkube-controller; ovnkube-node starts networks inline so they
	// are ready before node setup continues.
	asyncNetworkStart bool

	nadLister                    nadlisters.NetworkAttachmentDefinitionLister
	nodeLister                   corelisters.NodeLister
	frrIndexer                   cache.Indexer
	frrController                controller.Controller
	legacyRALister               ralisters.RouteAdvertisementsLister
	legacyRAController           controller.Controller
	getNADsForRouteAdvertisement func(raName string) ([]*nadtypes.NetworkAttachmentDefinition, error)

	// Last successfully reconciled name-only RA intents, keyed by network and RA.
	lastLegacyIntents map[string]advertisementIntents

	networkReconciler controller.Reconciler
	nodeController    controller.Controller

	cm                 ControllerManager
	networks           map[string]util.MutableNetInfo
	networksByID       map[int]string // reverse index: network ID -> network name for O(1) lookup
	networkControllers map[string]*networkControllerState
	// getNADKeysForNetwork returns NAD keys associated with a network name.
	getNADKeysForNetwork func(networkName string) []string

	// networkRefLock is used to protect pendingNetworkRefNodes and networkRefStates access
	networkRefLock sync.Mutex
	// pendingNetworkRefNodes is used to temporarily hold network name -> nodes mapping for future reconciliation
	// Dynamic UDN asks network controller to reconcile and stores nodes that have changed state in this map
	pendingNetworkRefNodes map[string]sets.Set[string]
	// networkRefStates is used to hold network name -> node (active/inactive) state as it is currently configured
	// This is used by Dynamic UDN to avoid calling the network controller's HandleNetworkRefChange for nodes that have
	// not changed state.
	networkRefStates map[string]map[string]bool
	// nodeHasNetwork is a function used to query if a node has a resource (pod or egress IP) that would inform the
	// network controller to either add or remove the remote resources for that network.
	nodeHasNetwork func(node, networkName string) bool
}

// Start will cleanup stale networks that have not been ensured via
// EnsuredNetwork before this call
func (c *networkController) Start() error {
	controllers := []controller.Reconciler{c.networkReconciler}
	if c.frrController != nil {
		controllers = append(controllers, c.frrController, c.legacyRAController)
	}
	if c.nodeController != nil {
		controllers = append(controllers, c.nodeController)
	}
	return controller.StartWithInitialSync(
		c.syncAll,
		controllers...,
	)
}

func (c *networkController) Stop() {
	controllers := []controller.Reconciler{c.networkReconciler}
	if c.frrController != nil {
		controllers = append(controllers, c.frrController, c.legacyRAController)
	}
	if c.nodeController != nil {
		controllers = append(controllers, c.nodeController)
	}
	controller.Stop(controllers...)
	for _, networkControllerState := range c.getAllNetworkStates() {
		if networkControllerState.controller.GetNetworkName() == types.DefaultNetworkName {
			// we don't own the lifecycle of the default network, so don't stop
			// it
			continue
		}
		if networkControllerState.stoppedAndDeleting || networkControllerState.startFailed {
			continue
		}
		networkControllerState.controller.Stop()
	}
}

func (c *networkController) EnsureNetwork(network util.MutableNetInfo) {
	c.setNetwork(network.GetNetworkName(), network)
	c.networkReconciler.Reconcile(network.GetNetworkName())
}

func (c *networkController) DeleteNetwork(network string) {
	switch network {
	case types.DefaultNetworkName:
		// for the default network however ensure it runs with the default
		// config
		c.setNetwork(network, &util.DefaultNetInfo{})
	default:
		c.setNetwork(network, nil)
	}
	c.networkReconciler.Reconcile(network)
}

func (c *networkController) setNetwork(network string, netInfo util.MutableNetInfo) {
	c.Lock()
	defer c.Unlock()
	// Remove old ID mapping if network existed
	if old, exists := c.networks[network]; exists {
		if oldID := old.GetNetworkID(); oldID != types.InvalidID {
			delete(c.networksByID, oldID)
		}
	}
	if netInfo == nil {
		delete(c.networks, network)
		return
	}
	c.networks[network] = netInfo
	// Add new ID mapping
	if id := netInfo.GetNetworkID(); id != types.InvalidID {
		c.networksByID[id] = network
	}
}

func (c *networkController) getNetwork(network string) util.MutableNetInfo {
	c.RLock()
	defer c.RUnlock()
	return c.networks[network]
}

func (c *networkController) getAllNetworks() []util.NetInfo {
	c.RLock()
	defer c.RUnlock()
	networks := make([]util.NetInfo, 0, len(c.networks))
	for _, network := range c.networks {
		networks = append(networks, network)
	}
	return networks
}

func (c *networkController) setNetworkState(network string, state *networkControllerState) {
	c.Lock()
	defer c.Unlock()
	if state == nil {
		delete(c.networkControllers, network)
		return
	}
	c.networkControllers[network] = state
}

func (c *networkController) getNetworkState(network string) *networkControllerState {
	c.RLock()
	defer c.RUnlock()
	state := c.networkControllers[network]
	if state == nil {
		return &networkControllerState{}
	}
	return state
}

func (c *networkController) getReconcilableNetworkState(network string) (ReconcilableNetworkController, bool, bool) {
	if network == types.DefaultNetworkName {
		return c.cm.GetDefaultNetworkController(), false, false
	}
	c.RLock()
	defer c.RUnlock()
	state := c.networkControllers[network]
	if state == nil {
		return nil, false, false
	}
	return state.controller, state.stoppedAndDeleting, state.startFailed
}

func (c *networkController) getControllerForNotify(networkName string) (NetworkController, bool) {
	c.RLock()
	defer c.RUnlock()
	state := c.networkControllers[networkName]
	if state == nil || state.controller == nil || state.stoppedAndDeleting || state.startFailed {
		return nil, false
	}
	return state.controller, true
}

// NotifyNetworkRefChange enqueues a network reconcile so node-level state can be recomputed.
func (c *networkController) NotifyNetworkRefChange(networkName, node string) {
	c.queueNetworkRefChange(networkName, node)
	c.networkReconciler.Reconcile(networkName)
}

func (c *networkController) queueNetworkRefChange(networkName, node string) {
	if networkName == "" || node == "" {
		return
	}
	c.networkRefLock.Lock()
	defer c.networkRefLock.Unlock()
	if c.pendingNetworkRefNodes == nil {
		c.pendingNetworkRefNodes = map[string]sets.Set[string]{}
	}
	nodes := c.pendingNetworkRefNodes[networkName]
	if nodes == nil {
		nodes = sets.New[string]()
		c.pendingNetworkRefNodes[networkName] = nodes
	}
	nodes.Insert(node)
}

func (c *networkController) popPendingNetworkRefNodes(networkName string) []string {
	c.networkRefLock.Lock()
	defer c.networkRefLock.Unlock()
	nodes := c.pendingNetworkRefNodes[networkName]
	if len(nodes) == 0 {
		return nil
	}
	delete(c.pendingNetworkRefNodes, networkName)
	return nodes.UnsortedList()
}

func (c *networkController) clearPendingNetworkRefNodes(networkName string) {
	c.networkRefLock.Lock()
	defer c.networkRefLock.Unlock()
	delete(c.pendingNetworkRefNodes, networkName)
}

func (c *networkController) clearNetworkRefState(networkName string) {
	c.networkRefLock.Lock()
	defer c.networkRefLock.Unlock()
	delete(c.networkRefStates, networkName)
}

func (c *networkController) shouldHandleNetworkRefChange(networkName, node string, active bool) bool {
	c.networkRefLock.Lock()
	defer c.networkRefLock.Unlock()
	if c.networkRefStates == nil {
		c.networkRefStates = map[string]map[string]bool{}
	}
	nodeStates := c.networkRefStates[networkName]
	if nodeStates == nil {
		nodeStates = map[string]bool{}
		c.networkRefStates[networkName] = nodeStates
	}
	prev, ok := nodeStates[node]
	if ok && prev == active {
		return false
	}
	nodeStates[node] = active
	return true
}

func (c *networkController) reconcilePendingNetworkRefChanges(ctx context.Context, networkName string) {
	ctrl, ok := c.getControllerForNotify(networkName)
	if !ok || c.nodeHasNetwork == nil {
		return
	}
	nodes := c.popPendingNetworkRefNodes(networkName)
	if len(nodes) == 0 {
		return
	}
	for _, node := range nodes {
		active := c.nodeHasNetwork(node, networkName)
		if !c.shouldHandleNetworkRefChange(networkName, node, active) {
			continue
		}
		ctrl.HandleNetworkRefChange(ctx, node, active)
	}
}

func (c *networkController) getAllNetworkStates() []*networkControllerState {
	c.RLock()
	defer c.RUnlock()
	networkStates := make([]*networkControllerState, 0, len(c.networks))
	for _, state := range c.networkControllers {
		networkStates = append(networkStates, state)
	}
	return networkStates
}

func (c *networkController) syncAll() error {
	ctx := context.Background()

	// as we sync upon start, consider networks that have not been ensured as
	// stale and clean them up
	validNetworks := c.getAllNetworks()
	if err := c.cm.CleanupStaleNetworks(validNetworks...); err != nil {
		return err
	}

	// For async start, skip inline sync and let reconciler workers handle networks
	// asynchronously.
	if c.asyncNetworkStart {
		klog.Infof("%s: skipping inline sync for async start", c.name)
		return nil
	}

	// sync all known networks. There is no informer for networks. Keys are added by NAD controller.
	// Certain downstream controllers that handle configuration for multiple networks depend on being
	// aware of all the existing networks on initialization. To achieve that, we need to start existing
	// networks synchronously. Otherwise, these controllers might incorrectly assess valid configuration
	// as stale.
	start := time.Now()
	klog.Infof("%s: syncing all networks", c.name)
	for _, network := range validNetworks {
		err := c.syncNetwork(ctx, network.GetNetworkName())
		if err != nil {
			return fmt.Errorf("failed to sync network %s: %w", network.GetNetworkName(), err)
		}
	}
	klog.Infof("%s: finished syncing all networks. Time taken: %s", c.name, time.Since(start))
	return nil
}

func (c *networkController) syncNetworks() error {
	c.networkReconciler.Reconcile(types.DefaultNetworkName)
	// Controllers are registered only after Start returns. Include desired
	// networks so updates during startup remain queued for reconciliation.
	for _, network := range c.getAllNetworks() {
		c.networkReconciler.Reconcile(network.GetNetworkName())
	}

	return nil
}

// syncRouteAdvertisements queues only networks whose NADs reference raName.
// NAD annotation changes independently queue their networks through EnsureNetwork,
// so this lookup only needs the current references to the advertisement.
func (c *networkController) syncRouteAdvertisements(_ context.Context, raName string) error {
	nads, err := c.getNADsForRouteAdvertisement(raName)
	if err != nil {
		return fmt.Errorf("failed to find NADs for route advertisement %s: %w", raName, err)
	}
	// Several NADs can belong to the same network, particularly for a CUDN
	// spanning namespaces. Deduplicate before calling the network reconciler.
	networks := sets.New[string]()
	for _, nad := range nads {
		network := util.GetAnnotatedNetworkName(nad)
		// A NAD may not be annotated yet, or its network may be filtered
		// locally. EnsureNetwork will gather the latest RA state when that
		// network is created. The default controller is managed separately
		// through ControllerManager and remains eligible even without an
		// entry in c.networks.
		if network == "" || (network != types.DefaultNetworkName && c.getNetwork(network) == nil) {
			continue
		}
		networks.Insert(network)
	}
	for network := range networks {
		c.networkReconciler.Reconcile(network)
	}
	return nil
}

// syncNetwork must be called with nm mutex locked
func (c *networkController) syncNetwork(ctx context.Context, network string) error {
	startTime := time.Now()
	klog.V(5).Infof("%s: sync network %s", c.name, network)
	defer func() {
		klog.V(4).Infof("%s: finished syncing network %s, took %v", c.name, network, time.Since(startTime))
	}()

	have, stoppedAndDeleting, startFailed := c.getReconcilableNetworkState(network)
	c.RLock()
	want := c.networks[network]
	lastLegacy := c.lastLegacyIntents[network]
	c.RUnlock()
	compatible := util.AreNetworksCompatible(have, want)

	// we will dispose of the old network if deletion is in progress or if
	// non-reconcilable configuration changed. A stopped controller whose Start
	// failed remains the cleanup owner and must release that state before a new
	// controller may attempt to program the same network.
	previousLegacy := lastLegacy
	dispose := startFailed || stoppedAndDeleting || !compatible
	if dispose {
		err := c.deleteNetwork(ctx, network)
		if err != nil {
			return err
		}
		have = nil
		previousLegacy = nil
	}

	legacy, err := c.setAdvertisements(want, previousLegacy)
	if err != nil {
		return fmt.Errorf("failed to determine route advertisements for network %s: %w", network, err)
	}

	ensureNetwork := !compatible || util.DoesNetworkNeedReconciliation(have, want)
	if ensureNetwork {
		// inform controller manager of upcoming changes so other controllers are
		// aware
		err = c.cm.Reconcile(network, have, want)
		if err != nil {
			return fmt.Errorf("failed to reconcile controller manager for network %s: %w", network, err)
		}

		// ensure the network controller
		err = c.ensureNetwork(ctx, want)
		if err != nil {
			return fmt.Errorf("%s: failed to ensure network %s: %w", c.name, network, err)
		}
	}

	if !maps.Equal(lastLegacy, legacy) {
		c.Lock()
		if len(legacy) == 0 {
			delete(c.lastLegacyIntents, network)
		} else {
			c.lastLegacyIntents[network] = legacy
		}
		c.Unlock()
	}
	c.reconcilePendingNetworkRefChanges(ctx, network)

	return nil
}

func (c *networkController) ensureNetwork(ctx context.Context, network util.MutableNetInfo) error {
	if network == nil {
		return nil
	}

	networkName := network.GetNetworkName()
	reconcilable, _, startFailed := c.getReconcilableNetworkState(networkName)

	// this might just be an update of reconcilable network configuration
	if reconcilable != nil && !startFailed {
		err := reconcilable.Reconcile(ctx, network)
		if err != nil {
			return fmt.Errorf("failed to reconcile controller for network %s: %w", networkName, err)
		}
		return nil
	}

	// otherwise setup & start the new network controller
	nc, err := c.cm.NewNetworkController(network)
	if err != nil {
		if errors.Is(err, ErrNetworkControllerTopologyNotManaged) {
			klog.V(5).Infof("Network %q of topology %s is not managed by %s, skipping it",
				networkName, network.TopologyType(), c.name)
			return nil
		}
		return fmt.Errorf("failed to create network %s: %w", networkName, err)
	}

	err = nc.Start(ctx)
	if err != nil {
		nc.Stop()
		c.setNetworkState(networkName, &networkControllerState{
			controller:  nc,
			startFailed: true,
		})
		return fmt.Errorf("failed to start network %s: %w", networkName, err)
	}
	c.setNetworkState(network.GetNetworkName(), &networkControllerState{controller: nc})

	return nil
}

func (c *networkController) deleteNetwork(ctx context.Context, network string) error {
	c.Lock()
	have := c.networkControllers[network]
	if have == nil || have.controller == nil {
		c.Unlock()
		c.clearPendingNetworkRefNodes(network)
		c.clearNetworkRefState(network)
		return nil
	}
	ctrl := have.controller
	alreadyStopping := have.stoppedAndDeleting
	startFailed := have.startFailed
	have.stoppedAndDeleting = true
	c.Unlock()

	if !alreadyStopping && !startFailed {
		ctrl.Stop()
	}

	if err := ctrl.Cleanup(ctx); err != nil {
		return fmt.Errorf("%s: failed to cleanup network %s: %w", c.name, network, err)
	}

	c.setNetworkState(network, nil)
	c.clearPendingNetworkRefNodes(network)
	c.clearNetworkRefState(network)
	return nil
}

func (c *networkController) setAdvertisements(network util.MutableNetInfo, previousLegacy advertisementIntents) (advertisementIntents, error) {
	if c.frrController == nil || network == nil || (!network.IsDefault() && !network.IsPrimaryNetwork()) {
		return nil, nil
	}

	// Cluster manager records on the NADs what each applied RA asks for. An
	// entry only changes once a new state is applied, so an RA that is being
	// edited, rejected or deleted keeps its last applied intent until then.
	intents := advertisementIntents{}
	conflicts := sets.New[string]()
	legacyRefs := sets.New[string]()
	for _, nadNamespacedName := range c.getNADKeysForNetwork(network.GetNetworkName()) {
		namespace, name, err := cache.SplitMetaNamespaceKey(nadNamespacedName)
		if err != nil {
			return nil, err
		}

		nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
		if err != nil {
			return nil, err
		}

		if raw := nad.Annotations[types.OvnRouteAdvertisementsKey]; raw != "" {
			var refs []string
			if err := json.Unmarshal([]byte(raw), &refs); err != nil {
				return nil, fmt.Errorf("reading legacy RA references on NAD %s/%s: %w", namespace, name, err)
			}
			legacyRefs.Insert(refs...)
		}
		if nad.Annotations[types.OvnRouteAdvertisementsIntentKey] == "" {
			continue
		}
		var nadIntents advertisementIntents
		if err := json.Unmarshal([]byte(nad.Annotations[types.OvnRouteAdvertisementsIntentKey]), &nadIntents); err != nil {
			return nil, err
		}
		for raName, intent := range nadIntents {
			current, seen := intents[raName]
			switch {
			case !seen || (current.UID == "" && intent.UID != ""):
				// A versioned copy supersedes any disagreements between older,
				// unversioned annotations during an upgrade.
				intents[raName] = intent
				conflicts.Delete(raName)
			case current.UID == intent.UID:
				// Backfilling ownership changes NADs one at a time without advancing the RA generation.
				if intent.Generation > current.Generation || (intent.Generation == current.Generation && intent.RequireFRROwner && !current.RequireFRROwner) {
					intents[raName] = intent
				} else if current.UID == "" && current != intent {
					conflicts.Insert(raName)
				}
			case intent.UID != "":
				// After a delete and recreate, the old RA can have the higher
				// generation. Wait for its NAD copies to disappear.
				conflicts.Insert(raName)
			}
		}
	}

	podAdvertisements := map[string][]string{}
	eipAdvertisements := map[string][]string{}
	var legacy advertisementIntents
	node, err := c.nodeLister.Get(c.node)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if node != nil {
		for name := range intents {
			legacyRefs.Delete(name)
		}
		legacy, err = c.getLegacyRAIntents(legacyRefs, previousLegacy)
		if err != nil {
			return nil, err
		}
		maps.Copy(intents, legacy)
		for raName, intent := range intents {
			if conflicts.Has(raName) {
				continue
			}
			// No-overlay and EVPN keep their routing domain while the node
			// is degraded. Only overlay can fall back to normal external SNAT.
			if !legacyRefs.Has(raName) && network.Transport() != types.NetworkTransportNoOverlay && network.Transport() != types.NetworkTransportEVPN {
				ready, err := c.nodeAdvertisementsReady(raName, intent, network, node)
				if err != nil {
					return nil, err
				}
				if !ready {
					continue
				}
			}
			vrf := intent.TargetVRF
			if vrf == "" {
				vrf = types.DefaultNetworkName
			}
			podAdvertisements[c.node] = append(podAdvertisements[c.node], vrf)
			if intent.EgressIP {
				eipAdvertisements[c.node] = append(eipAdvertisements[c.node], vrf)
			}
		}
	}
	network.SetPodNetworkAdvertisedVRFs(podAdvertisements)
	network.SetEgressIPAdvertisedVRFs(eipAdvertisements)
	return legacy, nil
}

// During migration, one NAD can already carry blue's new intent while red
// still has only a name. While red's next generation is pending, retain the
// VRF last applied for that RA on this network. A cold start has no such history.
func (c *networkController) getLegacyRAIntents(refs sets.Set[string], previous advertisementIntents) (advertisementIntents, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	legacy := make(advertisementIntents, len(refs))
	for name := range refs {
		ra, err := c.legacyRALister.Get(name)
		missing := apierrors.IsNotFound(err)
		if err != nil && !missing {
			return nil, err
		}
		intent, pending := legacyRAIntent(ra)
		if intent != nil {
			legacy[name] = *intent
		} else if old, ok := previous[name]; ok && (missing || (pending && old.UID == ra.UID)) {
			// A deleted RA can leave a name on the NAD until its routes are
			// withdrawn. A replacement with the same name owns different routes.
			legacy[name] = old
		}
	}
	return legacy, nil
}

// A name-only NAD reference can start pod advertisement from the old cluster
// manager's Accepted condition. Once the new manager reports Degraded, the
// node may only keep a VRF it already applied until the NAD carries its intent.
// Remove this compatibility path after one release.
func legacyRAIntent(ra *ratypes.RouteAdvertisements) (intent *util.RouteAdvertisementsIntent, pending bool) {
	if ra == nil {
		return nil, false
	}
	if degraded := meta.FindStatusCondition(ra.Status.Conditions, "Degraded"); degraded != nil {
		// EgressIP-only RAs keep their NAD reference without a pod intent.
		// A successful reconcile of that generation confirms pod withdrawal.
		withdrawn := !slices.Contains(ra.Spec.Advertisements, ratypes.PodNetwork) &&
			degraded.ObservedGeneration == ra.Generation && degraded.Status == metav1.ConditionFalse
		return nil, !withdrawn
	}
	accepted := meta.FindStatusCondition(ra.Status.Conditions, ratypes.RouteAdvertisementsAccepted)
	if accepted == nil {
		return nil, false
	}
	if accepted.Status != metav1.ConditionTrue || accepted.ObservedGeneration != ra.Generation {
		return nil, true
	}
	return util.NewRouteAdvertisementsIntent(ra), false
}

func nodeNeedsUpdate(oldNode, newNode *corev1.Node) bool {
	if oldNode == nil || newNode == nil {
		return true
	}

	// don't process resync or objects that are marked for deletion
	if oldNode.ResourceVersion == newNode.ResourceVersion ||
		!newNode.GetDeletionTimestamp().IsZero() {
		return false
	}

	return !reflect.DeepEqual(oldNode.Labels, newNode.Labels) || oldNode.Annotations[types.NodeSubnetsAnnotation] != newNode.Annotations[types.NodeSubnetsAnnotation]
}

func (c *networkController) getRunningNetwork(id int) string {
	if id == types.DefaultNetworkID {
		return types.DefaultNetworkName
	}
	for _, state := range c.getAllNetworkStates() {
		if state.controller.GetNetworkID() == id {
			return state.controller.GetNetworkName()
		}
	}
	return ""
}

// GetNetworkByID returns the network with the given ID or nil if not found.
// This is an O(1) lookup using an internal index.
func (c *networkController) GetNetworkByID(id int) util.NetInfo {
	if id == types.DefaultNetworkID {
		return &util.DefaultNetInfo{}
	}
	c.RLock()
	defer c.RUnlock()
	if name, ok := c.networksByID[id]; ok {
		return c.networks[name]
	}
	return nil
}

// FRR merges configurations on the node. IPv4 and IPv6 can use different
// peering templates, so each current pod prefix needs an export in the
// intended VRF somewhere among this RA's generated configurations.
func (c *networkController) nodeAdvertisementsReady(raName string, intent util.RouteAdvertisementsIntent, network util.NetInfo, node *corev1.Node) (bool, error) {
	configs, err := c.frrIndexer.ByIndex(types.OvnRouteAdvertisementsKey, raName)
	if err != nil {
		return false, fmt.Errorf("failed to look up FRRConfigurations for RouteAdvertisements %q on node %q: %w", raName, node.Name, err)
	}
	prefixes, err := util.GetRouteAdvertisementsPodPrefixes(network, node)
	if err != nil {
		// This network's node prefixes are not ready yet.
		return false, nil
	}
	vrf := intent.ResolvedVRF(network)
	for _, obj := range configs {
		cfg := obj.(*frrtypes.FRRConfiguration)
		// Before ownership was recorded, matching routes could have no UID.
		// A known different owner can never satisfy this RA's versioned intent.
		owner := cfg.Labels[types.OvnRouteAdvertisementsUIDKey]
		if intent.UID != "" && (intent.RequireFRROwner || owner != "") && owner != string(intent.UID) {
			continue
		}
		if !cfg.DeletionTimestamp.IsZero() {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(&cfg.Spec.NodeSelector)
		if err != nil || !selector.Matches(labels.Set(node.Labels)) {
			continue
		}
		prefixes = slices.DeleteFunc(prefixes, func(prefix string) bool {
			return util.RouteAdvertisementsConfigurationExportsPrefix(cfg, vrf, prefix)
		})
		if len(prefixes) == 0 {
			return true, nil
		}
	}
	return false, nil
}

func frrNeedsUpdate(old, new *frrtypes.FRRConfiguration) bool {
	if old != nil && new != nil && reflect.DeepEqual(old.Labels, new.Labels) && reflect.DeepEqual(old.Spec, new.Spec) &&
		reflect.DeepEqual(old.DeletionTimestamp, new.DeletionTimestamp) {
		return false
	}
	// The informer already filters objects to this node.
	return (old != nil && old.Labels[types.OvnRouteAdvertisementsKey] != "") ||
		(new != nil && new.Labels[types.OvnRouteAdvertisementsKey] != "")
}
