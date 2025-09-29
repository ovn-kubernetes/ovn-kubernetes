package ovn

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/pod"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/udn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	svccontroller "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/services"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/routeimport"
	zoneinterconnect "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/zone_interconnect"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/persistentips"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
)

// method/structure shared by all layer 2 network controller, including localnet and layer2 network controllres.

type layer2UserDefinedNetworkControllerEventHandler struct {
	baseHandler  baseNetworkControllerEventHandler
	watchFactory *factory.WatchFactory
	objType      reflect.Type
	oc           *Layer2UserDefinedNetworkController
	syncFunc     func([]interface{}) error
}

func (h *layer2UserDefinedNetworkControllerEventHandler) FilterOutResource(obj interface{}) bool {
	return h.oc.FilterOutResource(h.objType, obj)
}

// AreResourcesEqual returns true if, given two objects of a known resource type, the update logic for this resource
// type considers them equal and therefore no update is needed. It returns false when the two objects are not considered
// equal and an update needs be executed. This is regardless of how the update is carried out (whether with a dedicated update
// function or with a delete on the old obj followed by an add on the new obj).
func (h *layer2UserDefinedNetworkControllerEventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	return h.baseHandler.areResourcesEqual(h.objType, obj1, obj2)
}

// GetInternalCacheEntry returns the internal cache entry for this object, given an object and its type.
// This is now used only for pods, which will get their the logical port cache entry.
func (h *layer2UserDefinedNetworkControllerEventHandler) GetInternalCacheEntry(obj interface{}) interface{} {
	return h.oc.GetInternalCacheEntryForUserDefinedNetwork(h.objType, obj)
}

// GetResourceFromInformerCache returns the latest state of the object, given an object key and its type.
// from the informers cache.
func (h *layer2UserDefinedNetworkControllerEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	return h.baseHandler.getResourceFromInformerCache(h.objType, h.watchFactory, key)
}

// RecordAddEvent records the add event on this given object.
func (h *layer2UserDefinedNetworkControllerEventHandler) RecordAddEvent(obj interface{}) {
	h.baseHandler.recordAddEvent(h.objType, obj)
}

// RecordUpdateEvent records the udpate event on this given object.
func (h *layer2UserDefinedNetworkControllerEventHandler) RecordUpdateEvent(obj interface{}) {
	h.baseHandler.recordUpdateEvent(h.objType, obj)
}

// RecordDeleteEvent records the delete event on this given object.
func (h *layer2UserDefinedNetworkControllerEventHandler) RecordDeleteEvent(obj interface{}) {
	h.baseHandler.recordDeleteEvent(h.objType, obj)
}

// RecordSuccessEvent records the success event on this given object.
func (h *layer2UserDefinedNetworkControllerEventHandler) RecordSuccessEvent(obj interface{}) {
	h.baseHandler.recordSuccessEvent(h.objType, obj)
}

// RecordErrorEvent records the error event on this given object.
func (h *layer2UserDefinedNetworkControllerEventHandler) RecordErrorEvent(_ interface{}, _ string, _ error) {
}

// IsResourceScheduled returns true if the given object has been scheduled.
// Only applied to pods for now. Returns true for all other types.
func (h *layer2UserDefinedNetworkControllerEventHandler) IsResourceScheduled(obj interface{}) bool {
	return h.baseHandler.isResourceScheduled(h.objType, obj)
}

// AddResource adds the specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation.
// Given an object to add and a boolean specifying if the function was executed from iterateRetryResources
func (h *layer2UserDefinedNetworkControllerEventHandler) AddResource(obj interface{}, fromRetryLoop bool) error {
	switch h.objType {
	case factory.NodeType:
		node, ok := obj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to Node", obj)
		}
		if h.oc.isLocalZoneNode(node) {
			var nodeParams *nodeSyncs
			if fromRetryLoop {
				_, syncMgmtPort := h.oc.mgmtPortFailed.Load(node.Name)
				_, syncGw := h.oc.gatewaysFailed.Load(node.Name)
				_, syncReroute := h.oc.syncEIPNodeRerouteFailed.Load(node.Name)
				nodeParams = &nodeSyncs{
					syncMgmtPort: syncMgmtPort,
					syncGw:       syncGw,
					syncReroute:  syncReroute,
				}
			} else {
				nodeParams = &nodeSyncs{
					syncMgmtPort: true,
					syncGw:       true,
					syncReroute:  true,
				}
			}
			if config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
				nads := h.oc.GetNADs()
				hasNad := false
				for _, nadName := range nads {
					if h.oc.nodeNADTracker != nil && h.oc.nodeNADTracker.NodeHasNAD(node.Name, nadName) {
						hasNad = true
						break
					}
				}
				if !hasNad {
					klog.V(5).Infof("Ignoring processing remote node: %s as it has no active NAD for network: %s",
						node.Name, h.oc.GetNetworkName())
					return nil
				}
			}
			return h.oc.addUpdateLocalNodeEvent(node, nodeParams)
		}
		return h.oc.addUpdateRemoteNodeEvent(node, config.OVNKubernetesFeature.EnableInterconnect)
	default:
		return h.oc.AddUserDefinedNetworkResourceCommon(h.objType, obj)
	}
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// Given an object and optionally a cachedObj; cachedObj is the internal cache entry for this object,
// used for now for pods and network policies.
func (h *layer2UserDefinedNetworkControllerEventHandler) DeleteResource(obj, cachedObj interface{}) error {
	switch h.objType {
	case factory.NodeType:
		node, ok := obj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to Node", obj)
		}
		return h.oc.deleteNodeEvent(node)
	default:
		return h.oc.DeleteUserDefinedNetworkResourceCommon(h.objType, obj, cachedObj)
	}
}

// UpdateResource updates the specified object in the cluster to its version in newObj according to its
// type and returns the error, if any, yielded during the object update.
// Given an old and a new object; The inRetryCache boolean argument is to indicate if the given resource
// is in the retryCache or not.
func (h *layer2UserDefinedNetworkControllerEventHandler) UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error {
	switch h.objType {
	case factory.NodeType:
		newNode, ok := newObj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to Node", newObj)
		}
		oldNode, ok := oldObj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast oldObj of type %T to *kapi.Node", oldObj)
		}
		newNodeIsLocalZoneNode := h.oc.isLocalZoneNode(newNode)
		nodeSubnetChange := nodeSubnetChanged(oldNode, newNode, h.oc.GetNetworkName())
		if newNodeIsLocalZoneNode {
			var nodeSyncsParam *nodeSyncs
			if h.oc.isLocalZoneNode(oldNode) {
				// determine what actually changed in this update and combine that with what failed previously
				_, mgmtUpdateFailed := h.oc.mgmtPortFailed.Load(newNode.Name)
				shouldSyncMgmtPort := mgmtUpdateFailed || nodeSubnetChange
				_, gwUpdateFailed := h.oc.gatewaysFailed.Load(newNode.Name)
				shouldSyncGW := gwUpdateFailed ||
					gatewayChanged(oldNode, newNode) ||
					hostCIDRsChanged(oldNode, newNode) ||
					nodeGatewayMTUSupportChanged(oldNode, newNode)
				_, syncRerouteFailed := h.oc.syncEIPNodeRerouteFailed.Load(newNode.Name)
				shouldSyncReroute := syncRerouteFailed || util.NodeHostCIDRsAnnotationChanged(oldNode, newNode)
				nodeSyncsParam = &nodeSyncs{
					syncMgmtPort: shouldSyncMgmtPort,
					syncGw:       shouldSyncGW,
					syncReroute:  shouldSyncReroute,
				}
			} else {
				klog.Infof("Node %s moved from the remote zone %s to local zone %s.",
					newNode.Name, util.GetNodeZone(oldNode), util.GetNodeZone(newNode))
				// The node is now a local zone node. Trigger a full node sync.
				nodeSyncsParam = &nodeSyncs{
					syncMgmtPort: true,
					syncGw:       true,
					syncReroute:  true,
				}
			}

			return h.oc.addUpdateLocalNodeEvent(newNode, nodeSyncsParam)
		} else {
			if config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
				nads := h.oc.GetNADs()
				hasNad := false
				for _, nadName := range nads {
					if h.oc.nodeNADTracker != nil && h.oc.nodeNADTracker.NodeHasNAD(newNode.Name, nadName) {
						hasNad = true
						break
					}
				}
				if !hasNad {
					klog.V(5).Infof("Ignoring processing remote node: %s as it has no active NAD for network: %s",
						newNode.Name, h.oc.GetNetworkName())
					return nil
				}
			}
			_, syncZoneIC := h.oc.syncZoneICFailed.Load(newNode.Name)
			return h.oc.addUpdateRemoteNodeEvent(newNode, syncZoneIC)
		}
	case factory.PodType:
		newPod := newObj.(*corev1.Pod)
		oldPod := oldObj.(*corev1.Pod)
		if err := h.oc.ensurePodForUserDefinedNetwork(newPod, shouldAddPort(oldPod, newPod, inRetryCache)); err != nil {
			return err
		}

		if h.oc.isPodScheduledinLocalZone(newPod) {
			return h.oc.updateLocalPodEvent(newPod)
		}
		return nil
	default:
		return h.oc.UpdateUserDefinedNetworkResourceCommon(h.objType, oldObj, newObj, inRetryCache)
	}
}

func (h *layer2UserDefinedNetworkControllerEventHandler) SyncFunc(objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {
		switch h.objType {
		case factory.NodeType:
			syncFunc = h.oc.syncNodes

		case factory.PodType:
			syncFunc = h.oc.syncPodsForUserDefinedNetwork

		case factory.NamespaceType:
			syncFunc = h.oc.syncNamespaces

		case factory.PolicyType:
			syncFunc = h.oc.syncNetworkPolicies

		case factory.MultiNetworkPolicyType:
			syncFunc = h.oc.syncMultiNetworkPolicies

		case factory.IPAMClaimsType:
			syncFunc = h.oc.syncIPAMClaims

		default:
			return fmt.Errorf("no sync function for object type %s", h.objType)
		}
	}
	if syncFunc == nil {
		return nil
	}
	return syncFunc(objs)
}

// IsObjectInTerminalState returns true if the given object is a in terminal state.
// This is used now for pods that are either in a PodSucceeded or in a PodFailed state.
func (h *layer2UserDefinedNetworkControllerEventHandler) IsObjectInTerminalState(obj interface{}) bool {
	return h.baseHandler.isObjectInTerminalState(h.objType, obj)
}

// Layer2UserDefinedNetworkController is created for logical network infrastructure and policy
// for a layer2 UDN
type Layer2UserDefinedNetworkController struct {
	BaseLayer2UserDefinedNetworkController

	// Node-specific syncMaps used by node event handler
	mgmtPortFailed           sync.Map
	gatewaysFailed           sync.Map
	syncZoneICFailed         sync.Map
	syncEIPNodeRerouteFailed sync.Map

	// Cluster-wide router default Control Plane Protection (COPP) UUID
	defaultCOPPUUID string

	gatewayManagers sync.Map

	// Cluster wide Load_Balancer_Group UUID.
	// Includes the cluster switch and all node gateway routers.
	clusterLoadBalancerGroupUUID string

	// Cluster wide switch Load_Balancer_Group UUID.
	// Includes the cluster switch.
	switchLoadBalancerGroupUUID string

	// Cluster wide router Load_Balancer_Group UUID.
	// Includes all node gateway routers.
	routerLoadBalancerGroupUUID string

	// Controller in charge of services
	svcController *svccontroller.Controller

	// EgressIP controller utilized only to initialize a network with OVN polices to support EgressIP functionality.
	eIPController *EgressIPController

	// reconcile the virtual machine default gateway sending GARPs and RAs
	defaultGatewayReconciler *kubevirt.DefaultGatewayReconciler
}

// NewLayer2UserDefinedNetworkController create a new OVN controller for the given layer2 NAD
func NewLayer2UserDefinedNetworkController(
	cnci *CommonNetworkControllerInfo,
	netInfo util.NetInfo,
	networkManager networkmanager.Interface,
	routeImportManager routeimport.Manager,
	portCache *PortCache,
	eIPController *EgressIPController,
	nodeNADTracker networkmanager.Tracker,
) (*Layer2UserDefinedNetworkController, error) {

	stopChan := make(chan struct{})

	ipv4Mode, ipv6Mode := netInfo.IPMode()
	addressSetFactory := addressset.NewOvnAddressSetFactory(cnci.nbClient, ipv4Mode, ipv6Mode)

	lsManager := lsm.NewL2SwitchManager()
	if netInfo.IsPrimaryNetwork() {
		var gatewayIPs, mgmtIPs []*net.IPNet
		for _, subnet := range netInfo.Subnets() {
			if gwIP := netInfo.GetNodeGatewayIP(subnet.CIDR); gwIP != nil {
				gatewayIPs = append(gatewayIPs, gwIP)
			}
			if mgmtIP := netInfo.GetNodeManagementIP(subnet.CIDR); mgmtIP != nil {
				mgmtIPs = append(mgmtIPs, mgmtIP)
			}
		}

		lsManager = lsm.NewL2SwitchManagerForUserDefinedPrimaryNetwork(gatewayIPs, mgmtIPs)
	}

	oc := &Layer2UserDefinedNetworkController{
		BaseLayer2UserDefinedNetworkController: BaseLayer2UserDefinedNetworkController{

			BaseUserDefinedNetworkController: BaseUserDefinedNetworkController{
				BaseNetworkController: BaseNetworkController{
					CommonNetworkControllerInfo: *cnci,
					controllerName:              getNetworkControllerName(netInfo.GetNetworkName()),
					ReconcilableNetInfo:         util.NewReconcilableNetInfo(netInfo),
					lsManager:                   lsManager,
					logicalPortCache:            portCache,
					namespaces:                  make(map[string]*namespaceInfo),
					namespacesMutex:             sync.Mutex{},
					addressSetFactory:           addressSetFactory,
					networkPolicies:             syncmap.NewSyncMap[*networkPolicy](),
					sharedNetpolPortGroups:      syncmap.NewSyncMap[*defaultDenyPortGroups](),
					podSelectorAddressSets:      syncmap.NewSyncMap[*PodSelectorAddressSet](),
					stopChan:                    stopChan,
					wg:                          &sync.WaitGroup{},
					localZoneNodes:              &sync.Map{},
					cancelableCtx:               util.NewCancelableContext(),
					networkManager:              networkManager,
					routeImportManager:          routeImportManager,
					nodeNADTracker:              nodeNADTracker,
				},
			},
		},
		mgmtPortFailed:   sync.Map{},
		syncZoneICFailed: sync.Map{},
		gatewayManagers:  sync.Map{},
		eIPController:    eIPController,
	}

	if config.OVNKubernetesFeature.EnableInterconnect {
		oc.zoneICHandler = zoneinterconnect.NewZoneInterconnectHandler(oc.GetNetInfo(), oc.nbClient, oc.sbClient, oc.watchFactory)
	}

	if util.IsNetworkSegmentationSupportEnabled() && netInfo.IsPrimaryNetwork() {
		var err error
		oc.svcController, err = svccontroller.NewController(
			cnci.client, cnci.nbClient,
			cnci.watchFactory.ServiceCoreInformer(),
			cnci.watchFactory.EndpointSliceCoreInformer(),
			cnci.watchFactory.NodeCoreInformer(),
			networkManager,
			cnci.recorder,
			oc.GetNetInfo(),
		)
		if err != nil {
			return nil, fmt.Errorf("unable to create new service controller while creating new layer2 network controller: %w", err)
		}
		oc.defaultGatewayReconciler = kubevirt.NewDefaultGatewayReconciler(oc.watchFactory, oc.GetNetInfo(), util.GetNetworkScopedK8sMgmtHostIntfName(uint(oc.GetNetworkID())))
	}

	if oc.allocatesPodAnnotation() {
		var claimsReconciler persistentips.PersistentAllocations
		if oc.allowPersistentIPs() {
			ipamClaimsReconciler := persistentips.NewIPAMClaimReconciler(
				oc.kube,
				oc.GetNetInfo(),
				oc.watchFactory.IPAMClaimsInformer().Lister(),
			)
			oc.ipamClaimsReconciler = ipamClaimsReconciler
			claimsReconciler = ipamClaimsReconciler
		}
		oc.podAnnotationAllocator = pod.NewPodAnnotationAllocator(
			oc.GetNetInfo(),
			cnci.watchFactory.PodCoreInformer().Lister(),
			cnci.kube,
			claimsReconciler)
	}

	// enable multicast support for UDN only for primaries + multicast enabled
	// TBD: changes needs to be made to support multicast beyond primary UDN
	oc.multicastSupport = oc.IsPrimaryNetwork() && util.IsNetworkSegmentationSupportEnabled() && config.EnableMulticast

	oc.initRetryFramework()
	return oc, nil
}

// Start starts the layer2 UDN controller, handles all events and creates all needed logical entities
func (oc *Layer2UserDefinedNetworkController) Start(_ context.Context) error {
	klog.Infof("Starting controller for UDN %s", oc.GetNetworkName())

	start := time.Now()
	defer func() {
		klog.Infof("Starting controller for UDN %s took %v", oc.GetNetworkName(), time.Since(start))
	}()

	if err := oc.init(); err != nil {
		return err
	}

	return oc.run()
}

func (oc *Layer2UserDefinedNetworkController) run() error {
	err := oc.BaseLayer2UserDefinedNetworkController.run()
	if err != nil {
		return err
	}
	if oc.svcController != nil {
		startSvc := time.Now()

		err := oc.StartServiceController(oc.wg, true)
		endSvc := time.Since(startSvc)

		metrics.MetricOVNKubeControllerSyncDuration.WithLabelValues("service_" + oc.GetNetworkName()).Set(endSvc.Seconds())
		if err != nil {
			return err
		}
	}
	return nil
}

// Cleanup cleans up logical entities for the given network, called from net-attach-def routine
// could be called from a dummy Controller (only has CommonNetworkControllerInfo set)
func (oc *Layer2UserDefinedNetworkController) Cleanup() error {
	networkName := oc.GetNetworkName()
	if err := oc.BaseLayer2UserDefinedNetworkController.cleanup(); err != nil {
		return fmt.Errorf("failed to cleanup network %q: %w", networkName, err)
	}

	oc.gatewayManagers.Range(func(nodeName, value any) bool {
		gwManager, isGWManagerType := value.(*GatewayManager)
		if !isGWManagerType {
			klog.Errorf(
				"Failed to cleanup GW manager for network %q on node %s: could not retrieve GWManager",
				networkName,
				nodeName,
			)
			return true
		}
		if err := gwManager.Cleanup(); err != nil {
			klog.Errorf("Failed to cleanup GW manager for network %q on node %s: %v", networkName, nodeName, err)
		}
		return true
	})

	// remove load balancer groups
	lbGroups := make([]*nbdb.LoadBalancerGroup, 0, 3)
	for _, lbGroupUUID := range []string{oc.switchLoadBalancerGroupUUID, oc.clusterLoadBalancerGroupUUID, oc.routerLoadBalancerGroupUUID} {
		lbGroups = append(lbGroups, &nbdb.LoadBalancerGroup{UUID: lbGroupUUID})
	}
	if err := libovsdbops.DeleteLoadBalancerGroups(oc.nbClient, lbGroups); err != nil {
		klog.Errorf("Failed to delete load balancer groups on network: %q, error: %v", oc.GetNetworkName(), err)
	}

	return nil
}

func (oc *Layer2UserDefinedNetworkController) init() error {
	// Create default Control Plane Protection (COPP) entry for routers
	defaultCOPPUUID, err := EnsureDefaultCOPP(oc.nbClient)
	if err != nil {
		return fmt.Errorf("unable to create router control plane protection: %w", err)
	}
	oc.defaultCOPPUUID = defaultCOPPUUID

	clusterLBGroupUUID, switchLBGroupUUID, routerLBGroupUUID, err := initLoadBalancerGroups(oc.nbClient, oc.GetNetInfo())
	if err != nil {
		return err
	}
	oc.clusterLoadBalancerGroupUUID = clusterLBGroupUUID
	oc.switchLoadBalancerGroupUUID = switchLBGroupUUID
	oc.routerLoadBalancerGroupUUID = routerLBGroupUUID
	excludeSubnets := oc.ExcludeSubnets()
	excludeSubnets = append(excludeSubnets, oc.InfrastructureSubnets()...)

	_, err = oc.initializeLogicalSwitch(
		oc.GetNetworkScopedSwitchName(types.OVNLayer2Switch),
		oc.Subnets(),
		excludeSubnets,
		oc.ReservedSubnets(),
		oc.clusterLoadBalancerGroupUUID,
		oc.switchLoadBalancerGroupUUID,
	)
	if err != nil {
		return err
	}

	// Configure cluster port groups and multicast default policies for user defined primary networks.
	if oc.IsPrimaryNetwork() && util.IsNetworkSegmentationSupportEnabled() {
		if err := oc.setupClusterPortGroups(); err != nil {
			return fmt.Errorf("failed to create cluster port groups for network %q: %w", oc.GetNetworkName(), err)
		}

		if err := oc.syncDefaultMulticastPolicies(); err != nil {
			return fmt.Errorf("failed to sync default multicast policies for network %q: %w", oc.GetNetworkName(), err)
		}
	}

	return err
}

func (oc *Layer2UserDefinedNetworkController) Stop() {
	klog.Infof("Stoping controller for UDN %s", oc.GetNetworkName())
	oc.BaseLayer2UserDefinedNetworkController.stop()
}

func (oc *Layer2UserDefinedNetworkController) Reconcile(netInfo util.NetInfo) error {
	return oc.BaseNetworkController.reconcile(
		netInfo,
		func(node string) { oc.gatewaysFailed.Store(node, true) },
	)
}

func (oc *Layer2UserDefinedNetworkController) initRetryFramework() {
	oc.retryNodes = oc.newRetryFramework(factory.NodeType)
	oc.retryPods = oc.newRetryFramework(factory.PodType)
	if oc.allocatesPodAnnotation() && oc.AllowsPersistentIPs() {
		oc.retryIPAMClaims = oc.newRetryFramework(factory.IPAMClaimsType)
	}

	// When a user-defined network is enabled as a primary network for namespace,
	// then watch for namespace and network policy events.
	if oc.IsPrimaryNetwork() {
		oc.retryNamespaces = oc.newRetryFramework(factory.NamespaceType)
		oc.retryNetworkPolicies = oc.newRetryFramework(factory.PolicyType)
	}

	// For secondary networks, we don't have to watch namespace events if
	// multi-network policy support is not enabled. We don't support
	// multi-network policy for IPAM-less secondary networks either.
	if util.IsMultiNetworkPoliciesSupportEnabled() {
		oc.retryNamespaces = oc.newRetryFramework(factory.NamespaceType)
		oc.retryMultiNetworkPolicies = oc.newRetryFramework(factory.MultiNetworkPolicyType)
	}
}

// newRetryFramework builds and returns a retry framework for the input resource type;
func (oc *Layer2UserDefinedNetworkController) newRetryFramework(
	objectType reflect.Type) *retry.RetryFramework {
	eventHandler := &layer2UserDefinedNetworkControllerEventHandler{
		baseHandler:  baseNetworkControllerEventHandler{},
		objType:      objectType,
		watchFactory: oc.watchFactory,
		oc:           oc,
		syncFunc:     nil,
	}
	resourceHandler := &retry.ResourceHandler{
		HasUpdateFunc:          hasResourceAnUpdateFunc(objectType),
		NeedsUpdateDuringRetry: needsUpdateDuringRetry(objectType),
		ObjType:                objectType,
		EventHandler:           eventHandler,
	}
	return retry.NewRetryFramework(
		oc.stopChan,
		oc.wg,
		oc.watchFactory,
		resourceHandler,
	)
}

func (oc *Layer2UserDefinedNetworkController) addUpdateLocalNodeEvent(node *corev1.Node, nSyncs *nodeSyncs) error {
	var errs []error

	if util.IsNetworkSegmentationSupportEnabled() && oc.IsPrimaryNetwork() {
		if nSyncs.syncGw {
			gwManager := oc.gatewayManagerForNode(node.Name)
			oc.gatewayManagers.Store(node.Name, gwManager)

			err := func() error {
				gwConfig, err := oc.nodeGatewayConfig(node)
				if err != nil {
					return err
				}
				if err := gwManager.SyncGateway(
					node,
					gwConfig,
				); err != nil {
					return err
				}
				isUDNAdvertised := util.IsPodNetworkAdvertisedAtNode(oc, node.Name)
				err = oc.addOrUpdateUDNClusterSubnetEgressSNAT(gwConfig.hostSubnets, gwManager.gwRouterName, isUDNAdvertised)
				if err != nil {
					return err
				}
				shouldIsolate := isUDNAdvertised && config.OVNKubernetesFeature.AdvertisedUDNIsolationMode == config.AdvertisedUDNIsolationModeStrict
				if shouldIsolate {
					if err = oc.addAdvertisedNetworkIsolation(node.Name); err != nil {
						return err
					}
				} else {
					if err = oc.deleteAdvertisedNetworkIsolation(node.Name); err != nil {
						return err
					}
				}
				oc.gatewaysFailed.Delete(node.Name)
				return nil
			}()

			if err != nil {
				errs = append(errs, err)
				oc.gatewaysFailed.Store(node.Name, true)
			}
		}

		if nSyncs.syncMgmtPort {
			// Layer 2 networks have a single, large subnet, that's the one
			// associated to the controller.  Take the management port IP from
			// there.
			subnets := oc.Subnets()
			hostSubnets := make([]*net.IPNet, 0, len(subnets))
			for _, subnet := range oc.Subnets() {
				hostSubnets = append(hostSubnets, subnet.CIDR)
			}
			if _, err := oc.syncNodeManagementPort(node, oc.GetNetworkScopedSwitchName(types.OVNLayer2Switch),
				oc.GetNetworkScopedGWRouterName(node.Name), hostSubnets); err != nil {
				errs = append(errs, err)
				oc.mgmtPortFailed.Store(node.Name, true)
			} else {
				oc.mgmtPortFailed.Delete(node.Name)
			}
		}

		if config.OVNKubernetesFeature.EnableEgressIP && nSyncs.syncReroute {
			rerouteFailed := false
			if err := oc.eIPController.ensureRouterPoliciesForNetwork(oc.GetNetInfo(), node); err != nil {
				errs = append(errs, fmt.Errorf("failed to ensure EgressIP router policies for network %s: %v", oc.GetNetworkName(), err))
				rerouteFailed = true
			}
			if err := oc.eIPController.ensureSwitchPoliciesForNode(oc.GetNetInfo(), node.Name); err != nil {
				errs = append(errs, fmt.Errorf("failed to ensure EgressIP switch policies for network %s: %v", oc.GetNetworkName(), err))
				rerouteFailed = true
			}
			if rerouteFailed {
				oc.syncEIPNodeRerouteFailed.Store(node.Name, true)
			} else {
				oc.syncEIPNodeRerouteFailed.Delete(node.Name)
			}
		}
	}

	errs = append(errs, oc.BaseLayer2UserDefinedNetworkController.addUpdateLocalNodeEvent(node))

	err := utilerrors.Join(errs...)
	if err != nil {
		oc.recordNodeErrorEvent(node, err)
	}
	return err
}

func (oc *Layer2UserDefinedNetworkController) addUpdateRemoteNodeEvent(node *corev1.Node, syncZoneIC bool) error {
	var errs []error

	if util.IsNetworkSegmentationSupportEnabled() && oc.IsPrimaryNetwork() {
		if syncZoneIC && config.OVNKubernetesFeature.EnableInterconnect {
			if err := oc.addPortForRemoteNodeGR(node); err != nil {
				err = fmt.Errorf("failed to add the remote zone node %s's remote LRP, %w", node.Name, err)
				errs = append(errs, err)
				oc.syncZoneICFailed.Store(node.Name, true)
			} else {
				oc.syncZoneICFailed.Delete(node.Name)
			}
		}
	}

	errs = append(errs, oc.BaseLayer2UserDefinedNetworkController.addUpdateRemoteNodeEvent(node))

	err := utilerrors.Join(errs...)
	if err != nil {
		oc.recordNodeErrorEvent(node, err)
	}
	return err
}

func (oc *Layer2UserDefinedNetworkController) addPortForRemoteNodeGR(node *corev1.Node) error {
	nodeJoinSubnetIPs, err := udn.GetGWRouterIPs(node, oc.GetNetInfo())
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			// remote node may not have the annotation yet, suppress it
			return types.NewSuppressedError(err)
		}
		return fmt.Errorf("failed to get the node %s join subnet IPs: %w", node.Name, err)
	}
	if len(nodeJoinSubnetIPs) == 0 {
		return fmt.Errorf("annotation on the node %s had empty join subnet IPs", node.Name)
	}

	remoteGRPortMac := util.IPAddrToHWAddr(nodeJoinSubnetIPs[0].IP)
	var remoteGRPortNetworks []string
	for _, ip := range nodeJoinSubnetIPs {
		remoteGRPortNetworks = append(remoteGRPortNetworks, ip.String())
	}

	remotePortAddr := remoteGRPortMac.String() + " " + strings.Join(remoteGRPortNetworks, " ")
	klog.V(5).Infof("The remote port addresses for node %s in network %s are %s", node.Name, oc.GetNetworkName(), remotePortAddr)
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      types.SwitchToRouterPrefix + oc.GetNetworkScopedSwitchName(types.OVNLayer2Switch) + "_" + node.Name,
		Type:      "remote",
		Addresses: []string{remotePortAddr},
	}
	logicalSwitchPort.ExternalIDs = map[string]string{
		types.NetworkExternalID:  oc.GetNetworkName(),
		types.TopologyExternalID: oc.TopologyType(),
		types.NodeExternalID:     node.Name,
	}
	tunnelID, err := util.ParseUDNLayer2NodeGRLRPTunnelIDs(node, oc.GetNetworkName())
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			// remote node may not have the annotation yet, suppress it
			return types.NewSuppressedError(err)
		}
		// Don't consider this node as cluster-manager has not allocated node id yet.
		return fmt.Errorf("failed to fetch tunnelID annotation from the node %s for network %s, err: %w",
			node.Name, oc.GetNetworkName(), err)
	}
	logicalSwitchPort.Options = map[string]string{
		libovsdbops.RequestedTnlKey:  strconv.Itoa(tunnelID),
		libovsdbops.RequestedChassis: node.Name,
	}
	sw := nbdb.LogicalSwitch{Name: oc.GetNetworkScopedSwitchName(types.OVNLayer2Switch)}
	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(oc.nbClient, &sw, &logicalSwitchPort)
	if err != nil {
		return fmt.Errorf("failed to create port %v on logical switch %q: %v", logicalSwitchPort, sw.Name, err)
	}
	return nil
}

func (oc *Layer2UserDefinedNetworkController) deleteNodeEvent(node *corev1.Node) error {
	if err := oc.gatewayManagerForNode(node.Name).Cleanup(); err != nil {
		return fmt.Errorf("failed to cleanup gateway on node %q: %w", node.Name, err)
	}
	oc.gatewayManagers.Delete(node.Name)
	oc.localZoneNodes.Delete(node.Name)
	oc.mgmtPortFailed.Delete(node.Name)
	oc.syncEIPNodeRerouteFailed.Delete(node.Name)
	return nil
}

// addOrUpdateUDNClusterSubnetEgressSNAT adds or updates the SNAT on each node's GR in L2 networks for each UDN
// Based on the isUDNAdvertised flag, the SNAT matches are slightly different
// snat eth.dst == d6:cf:fd:2c:a6:44 169.254.0.12 10.128.0.0/14
// snat eth.dst == d6:cf:fd:2c:a6:44 169.254.0.12 2010:100:200::/64
// these SNATs are required for pod2Egress traffic in LGW mode and pod2SameNode traffic in SGW mode to function properly on UDNs
// SNAT Breakdown:
// match = "eth.dst == d6:cf:fd:2c:a6:44"; the MAC here is the mpX interface MAC address for this UDN
// logicalIP = "10.128.0.0/14"; which is the clustersubnet for this L2 UDN
// externalIP = "169.254.0.12"; which is the masqueradeIP for this L2 UDN
// so all in all we want to condionally SNAT all packets that are coming from pods hosted on this node,
// which are leaving via UDN's mpX interface to the UDN's masqueradeIP.
// If isUDNAdvertised is true, then we want to SNAT all packets that are coming from pods on this network
// leaving towards nodeIPs on the cluster to masqueradeIP. If network is advertise then the SNAT looks like this:
// "eth.dst == 0a:58:5d:5d:00:02 && (ip4.dst == $a712973235162149816)" "169.254.0.36" "93.93.0.0/16"
func (oc *Layer2UserDefinedNetworkController) addOrUpdateUDNClusterSubnetEgressSNAT(localPodSubnets []*net.IPNet, gwRouterName string, isUDNAdvertised bool) error {
	outputPort := types.GWRouterToJoinSwitchPrefix + gwRouterName
	nats, err := oc.buildUDNEgressSNAT(localPodSubnets, outputPort, isUDNAdvertised)
	if err != nil {
		return err
	}
	if len(nats) == 0 {
		return nil // nothing to do
	}
	gwRouter := &nbdb.LogicalRouter{
		Name: gwRouterName,
	}
	if err := libovsdbops.CreateOrUpdateNATs(oc.nbClient, gwRouter, nats...); err != nil {
		return fmt.Errorf("failed to update SNAT for cluster on router: %q for network %q, error: %w",
			gwRouterName, oc.GetNetworkName(), err)
	}
	return nil
}

func (oc *Layer2UserDefinedNetworkController) nodeGatewayConfig(node *corev1.Node) (*GatewayConfig, error) {
	l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s network %s L3 gateway config: %v", node.Name, oc.GetNetworkName(), err)
	}

	networkName := oc.GetNetworkName()
	networkID := oc.GetNetworkID()

	masqIPs, err := udn.GetUDNGatewayMasqueradeIPs(networkID)
	if err != nil {
		return nil, fmt.Errorf("failed to get masquerade IPs, network %s (%d): %v", networkName, networkID, err)
	}

	l3GatewayConfig.IPAddresses = append(l3GatewayConfig.IPAddresses, masqIPs...)

	// Always SNAT to the per network masquerade IP.
	var externalIPs []net.IP
	for _, masqIP := range masqIPs {
		if masqIP == nil {
			continue
		}
		externalIPs = append(externalIPs, masqIP.IP)
	}

	// Use the host subnets present in the network attachment definition.
	hostSubnets := make([]*net.IPNet, 0, len(oc.Subnets()))
	for _, subnet := range oc.Subnets() {
		hostSubnets = append(hostSubnets, subnet.CIDR)
	}

	// at layer2 the GR LRP should be different per node same we do for layer3
	// since they should not collide at the distributed switch later on
	gwLRPJoinIPs, err := udn.GetGWRouterIPs(node, oc.GetNetInfo())
	if err != nil {
		return nil, fmt.Errorf("failed composing LRP addresses for layer2 network %s: %w", oc.GetNetworkName(), err)
	}

	// Overwrite the primary interface ID with the correct, per-network one.
	l3GatewayConfig.InterfaceID = oc.GetNetworkScopedExtPortName(l3GatewayConfig.BridgeID, node.Name)
	return &GatewayConfig{
		annoConfig:                 l3GatewayConfig,
		hostSubnets:                hostSubnets,
		clusterSubnets:             hostSubnets,
		gwLRPJoinIPs:               gwLRPJoinIPs,
		hostAddrs:                  nil,
		externalIPs:                externalIPs,
		ovnClusterLRPToJoinIfAddrs: nil,
	}, nil
}

func (oc *Layer2UserDefinedNetworkController) newGatewayManager(nodeName string) *GatewayManager {
	return NewGatewayManagerForLayer2Topology(
		nodeName,
		oc.defaultCOPPUUID,
		oc.kube,
		oc.nbClient,
		oc.GetNetInfo(),
		oc.watchFactory,
		oc.gatewayOptions()...,
	)
}

func (oc *Layer2UserDefinedNetworkController) gatewayManagerForNode(nodeName string) *GatewayManager {
	obj, isFound := oc.gatewayManagers.Load(nodeName)
	if !isFound {
		return oc.newGatewayManager(nodeName)
	} else {
		gwManager, isGWManagerType := obj.(*GatewayManager)
		if !isGWManagerType {
			klog.Errorf(
				"failed to extract a gateway manager from the network %q on node %s; creating new one",
				oc.GetNetworkName(),
				nodeName,
			)
			return oc.newGatewayManager(nodeName)
		}
		return gwManager
	}
}

func (oc *Layer2UserDefinedNetworkController) gatewayOptions() []GatewayOption {
	var opts []GatewayOption
	if oc.clusterLoadBalancerGroupUUID != "" {
		opts = append(opts, WithLoadBalancerGroups(
			oc.routerLoadBalancerGroupUUID,
			oc.clusterLoadBalancerGroupUUID,
			oc.switchLoadBalancerGroupUUID,
		))
	}
	return opts
}

func (oc *Layer2UserDefinedNetworkController) StartServiceController(wg *sync.WaitGroup, runRepair bool) error {
	useLBGroups := oc.clusterLoadBalancerGroupUUID != ""
	// use 5 workers like most of the kubernetes controllers in the kubernetes controller-manager
	// do not use LB templates for UDNs - OVN bug https://issues.redhat.com/browse/FDP-988
	err := oc.svcController.Run(5, oc.stopChan, wg, runRepair, useLBGroups, false)
	if err != nil {
		return fmt.Errorf("error running OVN Kubernetes Services controller for network %s: %v", oc.GetNetworkName(), err)
	}
	return nil
}

func (oc *Layer2UserDefinedNetworkController) updateLocalPodEvent(pod *corev1.Pod) error {
	if kubevirt.IsPodAllowedForMigration(pod, oc.GetNetInfo()) {
		kubevirtLiveMigrationStatus, err := kubevirt.DiscoverLiveMigrationStatus(oc.watchFactory, pod)
		if err != nil {
			return err
		}
		if kubevirtLiveMigrationStatus != nil && kubevirtLiveMigrationStatus.TargetPod.Name == pod.Name {
			if err := oc.reconcileLiveMigrationTargetZone(kubevirtLiveMigrationStatus); err != nil {
				return err
			}
		}
	}
	return nil
}

func (oc *Layer2UserDefinedNetworkController) reconcileLiveMigrationTargetZone(kubevirtLiveMigrationStatus *kubevirt.LiveMigrationStatus) error {
	if oc.defaultGatewayReconciler == nil {
		return nil
	}
	hasIPv4Subnet, hasIPv6Subnet := oc.IPMode()
	if hasIPv4Subnet {
		if err := oc.defaultGatewayReconciler.ReconcileIPv4AfterLiveMigration(kubevirtLiveMigrationStatus); err != nil {
			return fmt.Errorf("failed reconciling IPv4 default gw after live migration at target pod '%s/%s': %w",
				kubevirtLiveMigrationStatus.TargetPod.Namespace, kubevirtLiveMigrationStatus.TargetPod.Name, err)
		}
	}
	if hasIPv6Subnet {
		if err := oc.defaultGatewayReconciler.ReconcileIPv6AfterLiveMigration(kubevirtLiveMigrationStatus); err != nil {
			return fmt.Errorf("failed reconciling IPv6 default gw after live migration at target pod '%s/%s': %w",
				kubevirtLiveMigrationStatus.TargetPod.Namespace, kubevirtLiveMigrationStatus.TargetPod.Name, err)
		}
	}
	return nil
}
