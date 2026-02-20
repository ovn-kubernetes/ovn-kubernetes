package clustermanager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"time"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	cache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	k8snodeutil "k8s.io/component-helpers/node/util"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip/subnet"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/mac"
	annotationalloc "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/pod"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/node"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/pod"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/persistentips"
	objretry "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type NetworkStatusReporter func(networkName string, fieldManager string, condition *metav1.Condition, events ...*util.EventDetails) error

// networkClusterController is the cluster controller for the networks. An
// instance of this struct is expected to be created for each network. A network
// is identified by its name and its unique id. It handles events at a cluster
// level to support the necessary configuration for the cluster networks.
type networkClusterController struct {
	watchFactory *factory.WatchFactory
	kube         kube.InterfaceOVN
	stopChan     chan struct{}
	wg           *sync.WaitGroup

	// node events factory handler
	nodeHandler *factory.Handler

	// retry framework for nodes
	retryNodes *objretry.RetryFramework

	// retry framework for L2 pod ip allocation
	podHandler *factory.Handler
	retryPods  *objretry.RetryFramework

	// retry framework for persistent ip allocation
	ipamClaimHandler *factory.Handler
	retryIPAMClaims  *objretry.RetryFramework
	// tunnelIDAllocator of tunnelIDs within the network
	tunnelIDAllocator   id.Allocator
	podAllocator        *pod.PodAllocator
	nodeAllocator       *node.NodeAllocator
	ipamClaimReconciler *persistentips.IPAMClaimReconciler
	subnetAllocator     subnet.Allocator

	networkManager networkmanager.Interface

	// event recorder used to post events to k8s
	recorder record.EventRecorder

	statusReporter NetworkStatusReporter

	// nodeName: errMessage
	nodeErrors     map[string]string
	nodeErrorsLock sync.Mutex
	// Error condition only reports one of the failed nodes.
	// To avoid changing that error report with every update, we store reported error node.
	reportedErrorNode string

	// dynamicUDNNodeRefs tracks active nodes for dynamic UDN allocation.
	dynamicUDNNodeRefsLock sync.Mutex
	dynamicUDNNodeRefs     map[string]bool
	dynamicUDNNodeCount    int

	nadKeysLock sync.Mutex
	lastNADKeys sets.Set[string]

	util.ReconcilableNetInfo
}

// HandleNetworkRefChange satisfies the NetworkController interface; it updates dynamic UDN metrics and status.
func (ncc *networkClusterController) HandleNetworkRefChange(nodeName string, active bool) {
	if !config.OVNKubernetesFeature.EnableDynamicUDNAllocation || !ncc.IsUserDefinedNetwork() {
		return
	}

	nodeCount, changed := ncc.updateDynamicUDNNodeRefs(nodeName, active)
	if !changed {
		return
	}
	networkName := ncc.GetNetworkName()
	metrics.SetDynamicUDNNodeCount(networkName, float64(nodeCount))
	klog.V(5).Infof("Updated metric: network=%s nodes=%d", networkName, nodeCount)

	var cond *metav1.Condition
	if nodeCount == 0 {
		msg := "no nodes currently rendered with network"
		cond = &metav1.Condition{
			Type:               "NodesSelected",
			Status:             metav1.ConditionFalse,
			Reason:             "DynamicAllocation",
			Message:            msg,
			LastTransitionTime: metav1.Now(),
		}
	} else {
		msg := fmt.Sprintf("%d node(s) rendered with network", nodeCount)
		cond = &metav1.Condition{
			Type:               "NodesSelected",
			Status:             metav1.ConditionTrue,
			Reason:             "DynamicAllocation",
			Message:            msg,
			LastTransitionTime: metav1.Now(),
		}
	}
	if ncc.statusReporter != nil {
		if err := ncc.statusReporter(
			networkName,
			"ClusterManager", // FieldManager - must be unique per subsystem
			cond,
		); err != nil {
			klog.Errorf("Failed to update NodesSelected condition for %s: %v", networkName, err)
		} else {
			klog.V(4).Infof("Updated Dynamic Allocation NodesSelected condition for %s: %s", networkName, cond.Message)
		}
	}
}

func (ncc *networkClusterController) updateDynamicUDNNodeRefs(nodeName string, active bool) (int, bool) {
	ncc.dynamicUDNNodeRefsLock.Lock()
	defer ncc.dynamicUDNNodeRefsLock.Unlock()

	if ncc.dynamicUDNNodeRefs == nil {
		ncc.dynamicUDNNodeRefs = map[string]bool{}
	}

	current := ncc.dynamicUDNNodeRefs[nodeName]
	if active == current {
		return ncc.dynamicUDNNodeCount, false
	}

	if active {
		ncc.dynamicUDNNodeRefs[nodeName] = true
		ncc.dynamicUDNNodeCount++
		return ncc.dynamicUDNNodeCount, true
	}

	delete(ncc.dynamicUDNNodeRefs, nodeName)
	if ncc.dynamicUDNNodeCount > 0 {
		ncc.dynamicUDNNodeCount--
	}
	return ncc.dynamicUDNNodeCount, true
}

func newNetworkClusterController(
	netInfo util.NetInfo,
	ovnClient *util.OVNClusterManagerClientset,
	wf *factory.WatchFactory,
	recorder record.EventRecorder,
	networkManager networkmanager.Interface,
	errorReporter NetworkStatusReporter,
) *networkClusterController {
	kube := &kube.KubeOVN{
		Kube: kube.Kube{
			KClient: ovnClient.KubeClient,
		},
		IPAMClaimsClient: ovnClient.IPAMClaimsClient,
	}

	wg := &sync.WaitGroup{}

	ncc := &networkClusterController{
		ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
		watchFactory:        wf,
		kube:                kube,
		stopChan:            make(chan struct{}),
		wg:                  wg,
		recorder:            recorder,
		networkManager:      networkManager,
		statusReporter:      errorReporter,
		nodeErrors:          make(map[string]string),
		nodeErrorsLock:      sync.Mutex{},
	}

	return ncc
}

func newDefaultNetworkClusterController(netInfo util.NetInfo, ovnClient *util.OVNClusterManagerClientset, wf *factory.WatchFactory, recorder record.EventRecorder) *networkClusterController {
	// use an allocator that can only allocate a single network ID for the
	// defaiult network
	networkIDAllocator := id.NewIDAllocator(types.DefaultNetworkName, 1)
	// Reserve the id 0 for the default network.
	err := networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)
	if err != nil {
		panic(fmt.Errorf("could not reserve default network ID: %w", err))
	}

	return newNetworkClusterController(netInfo, ovnClient, wf, recorder, networkmanager.Default().Interface(), nil)
}

func (ncc *networkClusterController) hasPodAllocation() bool {
	// we only do pod allocation on L2 topologies with interconnect
	switch ncc.TopologyType() {
	case types.Layer2Topology:
		// We need to allocate the PodAnnotation
		return config.OVNKubernetesFeature.EnableInterconnect
	case types.LocalnetTopology:
		// We need to allocate the PodAnnotation if there is IPAM
		return config.OVNKubernetesFeature.EnableInterconnect && len(ncc.Subnets()) > 0
	}
	return false
}

func (ncc *networkClusterController) hasNodeAllocation() bool {
	// we only do node allocation on L3 or default network, and L2 on
	// interconnect
	switch ncc.TopologyType() {
	case types.Layer3Topology:
		// we need to allocate network IDs and subnets
		return true
	case types.Layer2Topology:
		// we need to allocate network IDs
		return config.OVNKubernetesFeature.EnableInterconnect
	default:
		// we need to allocate network IDs and subnets
		return !ncc.IsUserDefinedNetwork()
	}
}

func (ncc *networkClusterController) allowPersistentIPs() bool {
	return config.OVNKubernetesFeature.EnablePersistentIPs &&
		util.DoesNetworkRequireIPAM(ncc.GetNetInfo()) &&
		util.AllowsPersistentIPs(ncc.GetNetInfo())
}

func (ncc *networkClusterController) init() error {
	// report no errors on restart, then propagate any new errors by the started handlers
	if err := ncc.resetStatus(); err != nil {
		return fmt.Errorf("failed to reset network status: %w", err)
	}

	networkID := ncc.GetNetworkID()

	var err error
	if util.DoesNetworkRequireTunnelIDs(ncc.GetNetInfo()) {
		ncc.tunnelIDAllocator = id.NewIDAllocator(ncc.GetNetworkName(), types.MaxLogicalPortTunnelKey)
		// Reserve the id 0. We don't want to assign this id to any of the pods or nodes.
		if err = ncc.tunnelIDAllocator.ReserveID("zero", types.NoTunnelID); err != nil {
			return err
		}
		if util.IsNetworkSegmentationSupportEnabled() && ncc.IsPrimaryNetwork() {
			// if the network is a primary L2 UDN network, then we need to reserve
			// the IDs used by each node in this network's pod allocator
			nodes, err := ncc.watchFactory.GetNodes()
			if err != nil {
				return fmt.Errorf("failed to list node objects: %w", err)
			}
			for _, node := range nodes {
				tunnelID, err := util.ParseUDNLayer2NodeGRLRPTunnelIDs(node, ncc.GetNetworkName())
				if err != nil {
					if util.IsAnnotationNotSetError(err) {
						klog.Warningf("tunnelID annotation does not exist for the node %s for network %s, err: %v; we need to allocate it...",
							node.Name, ncc.GetNetworkName(), err)
					} else {
						return fmt.Errorf("failed to fetch tunnelID annotation from the node %s for network %s, err: %v",
							node.Name, ncc.GetNetworkName(), err)
					}
				}
				if tunnelID != types.InvalidID {
					if err := ncc.tunnelIDAllocator.ReserveID(ncc.GetNetworkName()+"_"+node.Name, tunnelID); err != nil {
						return fmt.Errorf("unable to reserve id for network %s, node %s: %w", ncc.GetNetworkName(), node.Name, err)
					}
				}
			}
		}
	}

	if ncc.hasNodeAllocation() {
		ncc.retryNodes = ncc.newRetryFramework(factory.NodeType, true)

		ncc.nodeAllocator = node.NewNodeAllocator(networkID, ncc.GetNetInfo(), ncc.watchFactory.NodeCoreInformer().Lister(), ncc.kube, ncc.tunnelIDAllocator)
		err := ncc.nodeAllocator.Init()
		if err != nil {
			return fmt.Errorf("failed to initialize host subnet ip allocator: %w", err)
		}
	}

	if ncc.hasPodAllocation() {
		ncc.retryPods = ncc.newRetryFramework(factory.PodType, true)
		ipAllocator, err := newIPAllocatorForNetwork(ncc.GetNetInfo())
		if err != nil {
			return fmt.Errorf("could not initialize the IP allocator for network %q: %w", ncc.GetNetworkName(), err)
		}
		ncc.subnetAllocator = ipAllocator

		var (
			podAllocationAnnotator *annotationalloc.PodAnnotationAllocator
			ipamClaimsReconciler   persistentips.PersistentAllocations
		)

		persistentIPsEnabled := ncc.allowPersistentIPs()
		if persistentIPsEnabled {
			ncc.retryIPAMClaims = ncc.newRetryFramework(factory.IPAMClaimsType, true)
			ncc.ipamClaimReconciler = persistentips.NewIPAMClaimReconciler(
				ncc.kube,
				ncc.GetNetInfo(),
				ncc.watchFactory.IPAMClaimsInformer().Lister(),
			)
			ipamClaimsReconciler = ncc.ipamClaimReconciler
		}

		var podAllocOpts []annotationalloc.AllocatorOption
		if util.IsPreconfiguredUDNAddressesEnabled() &&
			ncc.IsPrimaryNetwork() &&
			ncc.TopologyType() == types.Layer2Topology {
			podAllocOpts = append(podAllocOpts, annotationalloc.WithMACRegistry(mac.NewManager()))
		}

		podAllocationAnnotator = annotationalloc.NewPodAnnotationAllocator(
			ncc.GetNetInfo(),
			ncc.watchFactory.PodCoreInformer().Lister(),
			ncc.kube,
			ipamClaimsReconciler,
			podAllocOpts...,
		)

		ncc.podAllocator = pod.NewPodAllocator(
			ncc.GetNetInfo(),
			podAllocationAnnotator,
			ipAllocator,
			ipamClaimsReconciler,
			ncc.networkManager,
			ncc.recorder,
			ncc.tunnelIDAllocator,
			ncc.watchFactory.NodeCoreInformer().Lister(),
		)
		if err := ncc.podAllocator.Init(); err != nil {
			return fmt.Errorf("failed to initialize pod ip allocator: %w", err)
		}
	}

	return nil
}

// updateNetworkStatus allows to report a status for networkClusterController's network via a UDN status condition
// of type "NetworkAllocationSucceeded", if the network was created by UDN.
// When at least one node reports an error, condition will be set to false and an event with node-specific error will be
// generated.
// Call this function after every node event handling, set handlerErr to nil to report no error.
// There are potential optimization to when an error should be reported, see https://github.com/ovn-kubernetes/ovn-kubernetes/pull/4647#discussion_r1763352619.
func (ncc *networkClusterController) updateNetworkStatus(nodeName string, handlerErr error) error {
	if ncc.statusReporter == nil {
		return nil
	}
	errorMsg := ""
	if handlerErr != nil {
		errorMsg = handlerErr.Error()
	}

	ncc.nodeErrorsLock.Lock()
	defer ncc.nodeErrorsLock.Unlock()

	if ncc.nodeErrors[nodeName] == errorMsg {
		// error message didn't change for that node, no need to update
		return nil
	}

	// identify current error node or set the currently reported error node as this node
	reportedErrorNode := ncc.reportedErrorNode
	if ncc.reportedErrorNode == "" && errorMsg != "" {
		reportedErrorNode = nodeName
	}

	if ncc.reportedErrorNode == nodeName && errorMsg == "" {
		// error for this node is fixed, find next error node.
		// used *only* for updating the condition.
		reportedErrorNode = ""
		for errorNode := range ncc.nodeErrors {
			if errorNode != nodeName {
				reportedErrorNode = errorNode
				break
			}
		}
	}

	var condition *metav1.Condition
	if reportedErrorNode != ncc.reportedErrorNode {
		// We know condition only changes if ncc.reportedErrorNode value changes.
		// Otherwise, condition will stay nil and the error message will be reflected in an event.
		condition = getNetworkAllocationUDNCondition(reportedErrorNode)
	}

	// Event update is only for original node if it had an error message.
	// No event reported if this node has no error now.
	events := make([]*util.EventDetails, 0, 1)
	if errorMsg != "" {
		events = append(events, &util.EventDetails{
			EventType: util.EventTypeWarning,
			Reason:    "NetworkAllocationFailed",
			Note:      fmt.Sprintf("Error occurred for node %s: %s", nodeName, errorMsg),
		})
	}

	netName := ncc.GetNetworkName()
	if err := ncc.statusReporter(netName, "NetworkClusterController", condition, events...); err != nil {
		return fmt.Errorf("failed to report network status: %w", err)
	}

	if errorMsg == "" {
		delete(ncc.nodeErrors, nodeName)
	} else {
		ncc.nodeErrors[nodeName] = errorMsg

	}
	ncc.reportedErrorNode = reportedErrorNode

	return nil
}

// resetStatus should be called on startup before any handler is started to avoid status race.
func (ncc *networkClusterController) resetStatus() error {
	if ncc.statusReporter == nil {
		return nil
	}
	netName := ncc.GetNetworkName()
	return ncc.statusReporter(netName, "NetworkClusterController", getNetworkAllocationUDNCondition(""))
}

// We only report one failed node in condition to avoid too long messages and too many condition updates.
// The node to be reported is passed as errorNode, if empty, all nodes are considered to be succeeded.
func getNetworkAllocationUDNCondition(errorNode string) *metav1.Condition {
	condition := &metav1.Condition{
		Type:               "NetworkAllocationSucceeded",
		LastTransitionTime: metav1.Now(),
	}
	if errorNode == "" {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "NetworkAllocationSucceeded"
		condition.Message = "Network allocation succeeded for all synced nodes."
	} else {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "InternalError"
		condition.Message = fmt.Sprintf("Network allocation failed for at least one node: %v, check UDN events for more info.", errorNode)
	}
	return condition
}

// Start the network cluster controller. Depending on the cluster configuration
// and type of network, it does the following:
//   - initializes the node allocator and starts listening to node events
//   - initializes the persistent ip allocator and starts listening to IPAMClaim events
//   - initializes the pod ip allocator and starts listening to pod events
func (ncc *networkClusterController) Start(_ context.Context) error {
	start := time.Now()
	klog.Infof("Initializing cluster manager network controller %q ...", ncc.GetNetworkName())
	err := ncc.init()
	if err != nil {
		return err
	}

	klog.Infof("Cluster manager network controller %q initialized. Took: %v", ncc.GetNetworkName(), time.Since(start))

	if ncc.hasNodeAllocation() {
		start = time.Now()
		klog.Infof("Cluster manager network controller %q starting node watcher...", ncc.GetNetworkName())
		nodeHandler, err := ncc.retryNodes.WatchResource()
		if err != nil {
			return fmt.Errorf("cluster manager network controller %q - unable to watch nodes: %w", ncc.GetNetworkName(), err)
		}
		klog.Infof("Cluster manager network controller %q completed watch nodes. Took: %v", ncc.GetNetworkName(), time.Since(start))
		ncc.nodeHandler = nodeHandler
	}

	if ncc.hasPodAllocation() {
		if ncc.allowPersistentIPs() {
			start = time.Now()
			klog.Infof("Cluster manager network controller %q starting IPAMClaim watcher...", ncc.GetNetworkName())
			// we need to start listening to IPAMClaim events before pod events, to
			// ensure we don't start processing pod allocations before having the
			// existing IPAMClaim allocations reserved in the in-memory IP pool.
			ipamClaimHandler, err := ncc.retryIPAMClaims.WatchResource()
			if err != nil {
				return fmt.Errorf("unable to watch IPAMClaims: %w", err)
			}
			ncc.ipamClaimHandler = ipamClaimHandler
			klog.Infof("Cluster manager network controller %q completed watch IPAMClaims. Took: %v", ncc.GetNetworkName(), time.Since(start))
		}

		start = time.Now()
		klog.Infof("Cluster manager network controller %q starting Pod watcher...", ncc.GetNetworkName())
		podHandler, err := ncc.retryPods.WatchResource()
		if err != nil {
			return fmt.Errorf("unable to watch pods: %w", err)
		}
		ncc.podHandler = podHandler
		klog.Infof("Cluster manager network controller %q completed watch Pods. Took: %v", ncc.GetNetworkName(), time.Since(start))
	}

	return nil
}

func (ncc *networkClusterController) Stop() {
	close(ncc.stopChan)
	ncc.wg.Wait()

	if ncc.ipamClaimHandler != nil {
		ncc.watchFactory.RemoveIPAMClaimsHandler(ncc.ipamClaimHandler)
	}

	if ncc.nodeHandler != nil {
		ncc.watchFactory.RemoveNodeHandler(ncc.nodeHandler)
	}

	if ncc.podHandler != nil {
		ncc.watchFactory.RemovePodHandler(ncc.podHandler)
	}
}

func (ncc *networkClusterController) newRetryFramework(objectType reflect.Type, hasUpdateFunc bool) *objretry.RetryFramework {
	resourceHandler := &objretry.ResourceHandler{
		HasUpdateFunc:          hasUpdateFunc,
		NeedsUpdateDuringRetry: false,
		ObjType:                objectType,
		EventHandler: &networkClusterControllerEventHandler{
			objType:  objectType,
			ncc:      ncc,
			syncFunc: nil,
		},
	}
	return objretry.NewRetryFramework(ncc.stopChan, ncc.wg, ncc.watchFactory, resourceHandler)
}

// Cleanup the subnet annotations from the node for the User Defined Networks
func (ncc *networkClusterController) Cleanup() error {
	if !ncc.IsUserDefinedNetwork() {
		return fmt.Errorf("default network cannot be cleaned up")
	}

	if ncc.hasNodeAllocation() {
		err := ncc.nodeAllocator.Cleanup()
		if err != nil {
			return err
		}
	}

	return nil
}

func (ncc *networkClusterController) Reconcile(netInfo util.NetInfo) error {
	nadKeys := ncc.networkManager.GetNADKeysForNetwork(netInfo.GetNetworkName())
	reconcilePendingPods := ncc.updateNADKeysChanged(nadKeys)
	// update network information, point of no return
	err := util.ReconcileNetInfo(ncc.ReconcilableNetInfo, netInfo)
	if err != nil {
		klog.Errorf("Failed to reconcile network %s: %v", ncc.GetNetworkName(), err)
	}
	if reconcilePendingPods && ncc.retryPods != nil {
		if err := objretry.RequeuePendingPods(ncc.watchFactory, ncc.GetNetInfo(), ncc.retryPods); err != nil {
			klog.Errorf("Failed to requeue pending pods for network %s: %v", ncc.GetNetworkName(), err)
		}
	}
	return nil
}

func (ncc *networkClusterController) updateNADKeysChanged(nadKeys []string) bool {
	ncc.nadKeysLock.Lock()
	defer ncc.nadKeysLock.Unlock()

	next := sets.New(nadKeys...)
	changed := ncc.lastNADKeys == nil || !next.Equal(ncc.lastNADKeys)
	ncc.lastNADKeys = next
	return changed
}

// networkClusterControllerEventHandler object handles the events
// from retry framework.
type networkClusterControllerEventHandler struct {
	objretry.DefaultEventHandler

	objType  reflect.Type
	ncc      *networkClusterController
	syncFunc func([]interface{}) error

	nodeSyncFailed sync.Map
}

func (h *networkClusterControllerEventHandler) FilterOutResource(_ interface{}) bool {
	return false
}

// networkClusterControllerEventHandler functions

// AddResource adds the specified object to the cluster according to its type and
// returns the error, if any, yielded during object creation.
func (h *networkClusterControllerEventHandler) AddResource(obj interface{}, _ bool) error {
	var err error

	switch h.objType {
	case factory.PodType:
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return fmt.Errorf("could not cast %T object to *corev1.Pod", obj)
		}
		err := h.ncc.podAllocator.Reconcile(nil, pod)
		if err != nil {
			return err
		}
	case factory.NodeType:
		node, ok := obj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to *corev1.Node", obj)
		}
		err = h.ncc.nodeAllocator.HandleAddUpdateNodeEvent(node)
		if err == nil {
			h.clearInitialNodeNetworkUnavailableCondition(node)
			h.nodeSyncFailed.Delete(node.Name)
		} else {
			h.nodeSyncFailed.Store(node.Name, true)
		}
		statusErr := h.ncc.updateNetworkStatus(node.Name, err)
		joinedErr := errors.Join(err, statusErr)
		if joinedErr != nil {
			klog.Infof("Cluster Manager Network Controller %q: Node add failed for %s, will try again later: %v",
				h.ncc.GetNetworkName(), node.Name, joinedErr)
			return joinedErr
		}
	case factory.IPAMClaimsType:
		return nil
	default:
		return fmt.Errorf("no add function for object type %s", h.objType)
	}
	return nil
}

// UpdateResource updates the specified object in the cluster to its version in newObj according
// to its type and returns the error, if any, yielded during the object update.
// The inRetryCache boolean argument is to indicate if the given resource is in the retryCache or not.
func (h *networkClusterControllerEventHandler) UpdateResource(oldObj, newObj interface{}, _ bool) error {
	var err error

	switch h.objType {
	case factory.PodType:
		old, ok := oldObj.(*corev1.Pod)
		if !ok {
			return fmt.Errorf("could not cast %T old object to *corev1.Pod", oldObj)
		}
		new, ok := newObj.(*corev1.Pod)
		if !ok {
			return fmt.Errorf("could not cast %T new object to *corev1.Pod", newObj)
		}
		err := h.ncc.podAllocator.Reconcile(old, new)
		if err != nil {
			return err
		}
	case factory.NodeType:
		oldNode, ok := oldObj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to *corev1.Node", oldObj)
		}
		newNode, ok := newObj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to *corev1.Node", newObj)
		}
		_, nodeFailed := h.nodeSyncFailed.Load(newNode.GetName())
		// Note: (trozet) It might be pedantic to check if the NeedsNodeAllocation. This assumes one of the following:
		// 1. we missed an add event (bug in kapi informer code)
		// 2. a user removed the annotation on the node
		// Either way to play it safe for now do a partial json unmarshal check
		_, nodeCondition := k8snodeutil.GetNodeCondition(&newNode.Status, corev1.NodeNetworkUnavailable)
		nodeNetworkUnavailable := nodeCondition != nil && nodeCondition.Status == corev1.ConditionTrue
		if !nodeFailed && util.NoHostSubnet(oldNode) == util.NoHostSubnet(newNode) &&
			!h.ncc.nodeAllocator.NeedsNodeAllocation(newNode) && !nodeNetworkUnavailable {
			// no other node updates would require us to reconcile again
			return nil
		}
		err = h.ncc.nodeAllocator.HandleAddUpdateNodeEvent(newNode)
		if err == nil {
			h.clearInitialNodeNetworkUnavailableCondition(newNode)
			h.nodeSyncFailed.Delete(newNode.GetName())
		} else {
			h.nodeSyncFailed.Store(newNode.Name, true)
		}
		statusErr := h.ncc.updateNetworkStatus(newNode.Name, err)
		joinedErr := errors.Join(err, statusErr)
		if joinedErr != nil {
			klog.Infof("Cluster Manager Network Controller %q: Node update failed for %s, will try again later: %v",
				h.ncc.GetNetworkName(), newNode.Name, err)
			return err
		}
	case factory.IPAMClaimsType:
		return nil
	default:
		return fmt.Errorf("no update function for object type %s", h.objType)
	}
	return nil
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// cachedObj is the internal cache entry for this object, used for now for pods and network policies.
func (h *networkClusterControllerEventHandler) DeleteResource(obj, _ interface{}) error {
	switch h.objType {
	case factory.PodType:
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return fmt.Errorf("could not cast %T object to *corev1.Pod", obj)
		}
		err := h.ncc.podAllocator.Reconcile(pod, nil)
		if err != nil {
			return err
		}
	case factory.NodeType:
		node, ok := obj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast obj of type %T to *knet.Node", obj)
		}
		err := h.ncc.nodeAllocator.HandleDeleteNode(node)
		statusErr := h.ncc.updateNetworkStatus(node.Name, err)
		jErr := errors.Join(err, statusErr)
		if jErr != nil {
			return jErr
		}
		h.nodeSyncFailed.Delete(node.Name)
		return nil
	case factory.IPAMClaimsType:
		ipamClaim, ok := obj.(*ipamclaimsapi.IPAMClaim)
		if !ok {
			return fmt.Errorf("could not cast obj of type %T to *ipamclaimsapi.IPAMClaim", obj)
		}

		ipAllocator := h.ncc.subnetAllocator.ForSubnet(h.ncc.GetNetworkName())
		err := h.ncc.ipamClaimReconciler.Reconcile(ipamClaim, nil, ipAllocator)
		if err != nil && !errors.Is(err, persistentips.ErrIgnoredIPAMClaim) {
			return fmt.Errorf("error deleting IPAMClaim: %w", err)
		} else if errors.Is(err, persistentips.ErrIgnoredIPAMClaim) {
			return nil // let's avoid the log below, since nothing was released.
		}
		klog.Infof("Released IPs %q for network %q", ipamClaim.Status.IPs, ipamClaim.Spec.Network)
	}
	return nil
}

func (h *networkClusterControllerEventHandler) SyncFunc(objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {
		switch h.objType {
		case factory.PodType:
			syncFunc = h.ncc.podAllocator.Sync
		case factory.NodeType:
			syncFunc = h.ncc.nodeAllocator.Sync
		case factory.IPAMClaimsType:
			syncFunc = func(claims []interface{}) error {
				return h.ncc.ipamClaimReconciler.Sync(
					claims,
					h.ncc.subnetAllocator.ForSubnet(h.ncc.GetNetworkName()),
				)
			}

		default:
			return fmt.Errorf("no sync function for object type %s", h.objType)
		}
	}
	if syncFunc == nil {
		return nil
	}
	return syncFunc(objs)
}

func (h *networkClusterControllerEventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	// switch based on type
	if h.objType == factory.NodeType {
		node1, ok := obj1.(*corev1.Node)
		if !ok {
			return false, fmt.Errorf("could not cast obj1 of type %T to *corev1.Node", obj1)
		}
		node2, ok := obj2.(*corev1.Node)
		if !ok {
			return false, fmt.Errorf("could not cast obj2 of type %T to *corev1.Node", obj2)
		}

		// network cluster controller only updates the node/hybrid subnet annotations.
		// Check if the annotations have changed.
		return reflect.DeepEqual(node1.Annotations, node2.Annotations), nil
	}

	return false, nil
}

// GetResourceFromInformerCache returns the latest state of the object from the informers cache
// given an object key and its type
func (h *networkClusterControllerEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	var obj interface{}
	var namespace, name string
	var err error

	namespace, name, err = cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to split key %s: %v", key, err)
	}

	switch h.objType {
	case factory.NodeType:
		obj, err = h.ncc.watchFactory.GetNode(name)
	case factory.PodType:
		obj, err = h.ncc.watchFactory.GetPod(namespace, name)
	case factory.IPAMClaimsType:
		obj, err = h.ncc.watchFactory.GetIPAMClaim(namespace, name)
	default:
		err = fmt.Errorf("object type %s not supported, cannot retrieve it from informers cache",
			h.objType)
	}
	return obj, err
}

// OVN uses an overlay and doesn't need GCE Routes, we need to
// clear the NetworkUnavailable condition that kubelet adds to initial node
// status when using GCE (done here: https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/cloud/node_controller.go#L237).
// See discussion surrounding this here: https://github.com/kubernetes/kubernetes/pull/34398.
// TODO: make upstream kubelet more flexible with overlays and GCE so this
// condition doesn't get added for network plugins that don't want it, and then
// we can remove this function.
func (h *networkClusterControllerEventHandler) clearInitialNodeNetworkUnavailableCondition(origNode *corev1.Node) {
	// If it is not a Cloud Provider node, then nothing to do.
	if origNode.Spec.ProviderID == "" {
		return
	}

	cleared := false
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var err error

		oldNode, err := h.ncc.watchFactory.GetNode(origNode.Name)
		if err != nil {
			return err
		}
		// Informer cache should not be mutated, so get a copy of the object
		node := oldNode.DeepCopy()

		for i := range node.Status.Conditions {
			if node.Status.Conditions[i].Type == corev1.NodeNetworkUnavailable {
				condition := &node.Status.Conditions[i]
				if condition.Status != corev1.ConditionFalse && condition.Reason == "NoRouteCreated" {
					condition.Status = corev1.ConditionFalse
					condition.Reason = "RouteCreated"
					condition.Message = "ovn-kube cleared kubelet-set NoRouteCreated"
					condition.LastTransitionTime = metav1.Now()
					if err = h.ncc.kube.UpdateNodeStatus(node); err == nil {
						cleared = true
					}
				}
				break
			}
		}
		return err
	})
	if resultErr != nil {
		klog.Errorf("Status update failed for local node %s: %v", origNode.Name, resultErr)
	} else if cleared {
		klog.Infof("Cleared node NetworkUnavailable/NoRouteCreated condition for %s", origNode.Name)
	}
}

// newIPAllocatorForNetwork returns an initialized subnet allocator for the
// subnets / excluded subnets provided in `netInfo`
func newIPAllocatorForNetwork(netInfo util.NetInfo) (subnet.Allocator, error) {
	ipAllocator := subnet.NewAllocator()

	subnets := netInfo.Subnets()
	ipNets := make([]*net.IPNet, 0, len(subnets))
	excludeSubnets := append(netInfo.ExcludeSubnets(), netInfo.InfrastructureSubnets()...)

	for _, subnet := range subnets {
		ipNets = append(ipNets, subnet.CIDR)
	}

	if isLayer2UserDefinedPrimaryNetwork(netInfo) && len(netInfo.InfrastructureSubnets()) == 0 {
		excludeSubnets = append(excludeSubnets, infrastructureExcludeCIDRs(netInfo)...)
	}

	if err := ipAllocator.AddOrUpdateSubnet(subnet.SubnetConfig{
		Name:            netInfo.GetNetworkName(),
		Subnets:         ipNets,
		ReservedSubnets: netInfo.ReservedSubnets(),
		ExcludeSubnets:  excludeSubnets,
	}); err != nil {
		return nil, err
	}

	return ipAllocator, nil
}

func isLayer2UserDefinedPrimaryNetwork(netInfo util.NetInfo) bool {
	return netInfo.IsPrimaryNetwork() && netInfo.TopologyType() == types.Layer2Topology
}

// infrastructureExcludeCIDRs returns a list of IPs that should be excluded from IP allocation (gateway and management port IPs)
func infrastructureExcludeCIDRs(netInfo util.NetInfo) []*net.IPNet {
	var excludeCIDRs []*net.IPNet

	for _, subnet := range netInfo.Subnets() {
		gwIP := netInfo.GetNodeGatewayIP(subnet.CIDR).IP
		mgmtPortIP := netInfo.GetNodeManagementIP(subnet.CIDR).IP
		excludeCIDRs = append(excludeCIDRs,
			&net.IPNet{IP: gwIP, Mask: util.GetIPFullMask(gwIP)},
			&net.IPNet{IP: mgmtPortIP, Mask: util.GetIPFullMask(mgmtPortIP)},
		)
	}
	return excludeCIDRs
}
