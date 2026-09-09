// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8stypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/applyconfiguration/uplink/v1alpha1"
	uplinkclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned"
	uplinkinformers "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/informers/externalversions/uplink/v1alpha1"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	uplinkGatewayFieldManager      = "ovnkube-node-uplink-gateway-controller"
	uplinkHostGatewayFieldManager  = "ovnkube-node-uplink-host-gateway-controller"
	maxGatewayConditionExamples    = 3
	maxGatewayConditionErrorLength = 160
	uplinkGatewayPendingMessage    = "gateway reconciliation is pending"
	uplinkGatewayAPITimeout        = 30 * time.Second
	uplinkGatewayStatusBatchDelay  = 100 * time.Millisecond
	uplinkGatewayStatusWorkers     = 2
)

type uplinkGatewayNetworkPhase string

const (
	uplinkGatewayNetworkPending uplinkGatewayNetworkPhase = "pending"
	uplinkGatewayNetworkReady   uplinkGatewayNetworkPhase = "ready"
	uplinkGatewayNetworkFailed  uplinkGatewayNetworkPhase = "failed"
)

type uplinkGatewayNetworkState struct {
	phase   uplinkGatewayNetworkPhase
	reason  string
	message string
}

func (s *uplinkGatewayNetworkState) markPending() {
	s.phase = uplinkGatewayNetworkPending
	s.reason = uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending
	s.message = uplinkGatewayPendingMessage
}

// uplinkGatewayState contains only the state needed to aggregate readiness.
// Dataplane lifecycle and serialization are owned by each UDN gateway.
type uplinkGatewayState struct {
	// conditionMutex serializes the readiness read, merge, and publication
	// sequence for this Uplink. lastAppliedCondition suppresses duplicate
	// writes while the informer cache catches up with a successful Apply.
	conditionMutex       sync.Mutex
	networks             map[string]*uplinkGatewayNetworkState
	lastCondition        *metav1.Condition
	lastAppliedCondition *metav1.Condition
	forcePublish         bool
	// stateUID binds this aggregate epoch to one UplinkState object. An empty
	// value means the object is currently absent or has not yet been observed.
	stateUID k8stypes.UID
	// publicationInvalid prevents queued work from restoring readiness while
	// its UplinkState object is absent. Only observing an add clears it.
	publicationInvalid bool
}

// UplinkStateGatewayStatusController aggregates readiness for all active CUDNs
// using an Uplink on the local node. It watches UplinkState lifecycle directly
// so discovery does not need gateway-specific callbacks. It deliberately does
// not own CUDN lifecycle callbacks or serialize dataplane operations: each UDN
// watches its UplinkState and serializes its own reconciliation.
type UplinkStateGatewayStatusController struct {
	nodeName            string
	uplinkClient        uplinkclientset.Interface
	uplinkStateLister   uplinklisters.UplinkStateLister
	uplinkStateInformer cache.SharedIndexInformer
	uplinkStateHandler  cache.ResourceEventHandlerRegistration
	// The DPU owns GatewayReady in split-DPU deployments. The DPU-host reports
	// its part of gateway programming through HostGatewayReady.
	conditionType string
	fieldManager  string

	// mutex protects short-lived aggregate readiness bookkeeping. Slow
	// dataplane operations are never performed while it is held.
	mutex               sync.Mutex
	uplinks             map[string]*uplinkGatewayState
	uplinkByNetworkName map[string]string

	// The queue coalesces readiness changes by Uplink name and retries API
	// failures independently of CUDN dataplane reconciliation.
	publishQueue workqueue.TypedRateLimitingInterface[string]
	publisherWG  sync.WaitGroup
	startOnce    sync.Once
	startErr     error
	shutdownOnce sync.Once
}

// NewUplinkStateGatewayStatusController creates the node-local readiness
// aggregator.
func NewUplinkStateGatewayStatusController(
	nodeName string,
	uplinkClient uplinkclientset.Interface,
	uplinkStateInformer uplinkinformers.UplinkStateInformer,
) *UplinkStateGatewayStatusController {
	conditionType := uplinkv1alpha1.UplinkStateConditionGatewayReady
	fieldManager := uplinkGatewayFieldManager
	if config.IsModeDPUHost() {
		conditionType = uplinkv1alpha1.UplinkStateConditionHostGatewayReady
		fieldManager = uplinkHostGatewayFieldManager
	}
	return &UplinkStateGatewayStatusController{
		nodeName:            nodeName,
		uplinkClient:        uplinkClient,
		uplinkStateLister:   uplinkStateInformer.Lister(),
		uplinkStateInformer: uplinkStateInformer.Informer(),
		conditionType:       conditionType,
		fieldManager:        fieldManager,
		uplinks:             map[string]*uplinkGatewayState{},
		uplinkByNetworkName: map[string]string{},
		publishQueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{
				Name: "uplink-gateway-status",
			},
		),
	}
}

// Start watches UplinkState lifecycle events and runs the asynchronous status
// publisher.
func (c *UplinkStateGatewayStatusController) Start() error {
	c.startOnce.Do(func() {
		c.uplinkStateHandler, c.startErr = c.uplinkStateInformer.AddEventHandler(
			cache.ResourceEventHandlerFuncs{
				AddFunc:    c.onUplinkStateAdd,
				UpdateFunc: c.onUplinkStateUpdate,
				DeleteFunc: c.onUplinkStateDelete,
			},
		)
		if c.startErr != nil {
			c.startErr = fmt.Errorf("failed to watch UplinkState gateway status: %w", c.startErr)
			return
		}
		for range uplinkGatewayStatusWorkers {
			c.publisherWG.Add(1)
			go func() {
				defer c.publisherWG.Done()
				for c.processNextGatewayCondition() {
				}
			}()
		}
	})
	return c.startErr
}

// Stop prevents new publications and waits for an in-flight API request.
func (c *UplinkStateGatewayStatusController) Stop() {
	c.shutdownOnce.Do(func() {
		if c.uplinkStateHandler != nil {
			if err := c.uplinkStateInformer.RemoveEventHandler(c.uplinkStateHandler); err != nil {
				klog.Errorf("Failed to stop watching UplinkState gateway status: %v", err)
			}
		}
		c.publishQueue.ShutDown()
	})
	c.publisherWG.Wait()
}

func (c *UplinkStateGatewayStatusController) onUplinkStateAdd(obj interface{}) {
	state, ok := obj.(*uplinkv1alpha1.UplinkState)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expected UplinkState add, got %T", obj))
		return
	}
	c.observeUplinkState(state)
}

func (c *UplinkStateGatewayStatusController) onUplinkStateUpdate(oldObj, newObj interface{}) {
	oldState, oldOK := oldObj.(*uplinkv1alpha1.UplinkState)
	newState, newOK := newObj.(*uplinkv1alpha1.UplinkState)
	if !oldOK || !newOK {
		utilruntime.HandleError(fmt.Errorf(
			"expected UplinkState update, got %T and %T", oldObj, newObj))
		return
	}

	oldUplinkName, oldNodeName := uplinkutil.StateIdentity(oldState)
	newUplinkName, newNodeName := uplinkutil.StateIdentity(newState)
	if oldNodeName == c.nodeName && (oldState.UID != newState.UID ||
		oldUplinkName != newUplinkName || oldNodeName != newNodeName) {
		c.retireUplinkState(oldState)
	}
	if newNodeName != c.nodeName {
		return
	}
	if oldState.UID != newState.UID || oldUplinkName != newUplinkName ||
		oldNodeName != newNodeName ||
		meta.FindStatusCondition(newState.Status.Conditions, c.conditionType) == nil {
		c.observeUplinkState(newState)
	}
}

func (c *UplinkStateGatewayStatusController) onUplinkStateDelete(obj interface{}) {
	state, ok := obj.(*uplinkv1alpha1.UplinkState)
	if !ok {
		var tombstoneObj interface{}
		switch tombstone := obj.(type) {
		case cache.DeletedFinalStateUnknown:
			tombstoneObj = tombstone.Obj
		case *cache.DeletedFinalStateUnknown:
			tombstoneObj = tombstone.Obj
		default:
			utilruntime.HandleError(fmt.Errorf("expected UplinkState delete, got %T", obj))
			return
		}
		state, ok = tombstoneObj.(*uplinkv1alpha1.UplinkState)
		if !ok {
			utilruntime.HandleError(fmt.Errorf(
				"expected UplinkState tombstone, got %T", tombstoneObj))
			return
		}
	}
	c.retireUplinkState(state)
}

// observeUplinkState starts publication for the current object lifecycle. A
// different UID is a recreation whose old in-flight CUDN completions must not
// update the new object's readiness.
func (c *UplinkStateGatewayStatusController) observeUplinkState(
	state *uplinkv1alpha1.UplinkState,
) {
	uplinkName, nodeName := uplinkutil.StateIdentity(state)
	if uplinkName == "" || nodeName != c.nodeName {
		return
	}

	c.mutex.Lock()
	uplinkState := c.ensureUplinkStateLocked(uplinkName)
	if uplinkState.stateUID != "" && state.UID != "" &&
		uplinkState.stateUID != state.UID {
		uplinkState = pendingGatewayState(uplinkState.networks)
		c.uplinks[uplinkName] = uplinkState
	}
	uplinkState.stateUID = state.UID
	uplinkState.publicationInvalid = false
	c.mutex.Unlock()

	if meta.FindStatusCondition(state.Status.Conditions, c.conditionType) == nil {
		uplinkState.conditionMutex.Lock()
		uplinkState.forcePublish = true
		uplinkState.conditionMutex.Unlock()
		c.enqueueGatewayCondition(uplinkName)
	}
}

// retireUplinkState ends one object lifecycle while retaining pending network
// membership for a possible recreation. Replacing the state pointer makes late
// completions from the deleted object harmless.
func (c *UplinkStateGatewayStatusController) retireUplinkState(
	state *uplinkv1alpha1.UplinkState,
) {
	uplinkName, nodeName := uplinkutil.StateIdentity(state)
	if uplinkName == "" || nodeName != c.nodeName {
		return
	}

	c.mutex.Lock()
	retired := c.uplinks[uplinkName]
	if retired == nil || (retired.stateUID != "" && state.UID != "" &&
		retired.stateUID != state.UID) {
		c.mutex.Unlock()
		return
	}
	replacement := pendingGatewayState(retired.networks)
	replacement.publicationInvalid = true
	c.uplinks[uplinkName] = replacement
	c.mutex.Unlock()
}

func pendingGatewayState(
	networks map[string]*uplinkGatewayNetworkState,
) *uplinkGatewayState {
	pending := &uplinkGatewayState{
		networks: make(map[string]*uplinkGatewayNetworkState, len(networks)),
	}
	for networkName := range networks {
		networkState := &uplinkGatewayNetworkState{}
		networkState.markPending()
		pending.networks[networkName] = networkState
	}
	return pending
}

// SyncNetworks seeds the complete active network set before individual network
// controllers start. Missing UplinkStates are tolerated because discovery may
// still be completing during node startup.
func (c *UplinkStateGatewayStatusController) SyncNetworks(networks ...util.NetInfo) error {
	desired := make(map[string]string)
	for _, network := range networks {
		if network.Uplink() != "" {
			desired[network.GetNetworkName()] = network.Uplink()
		}
	}

	states, err := c.uplinkStateLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list UplinkStates during gateway sync: %w", err)
	}

	affectedUplinks := map[string]struct{}{}
	c.mutex.Lock()
	for _, state := range states {
		uplinkName, nodeName := uplinkutil.StateIdentity(state)
		if uplinkName == "" || nodeName != c.nodeName {
			continue
		}
		uplinkState := c.ensureUplinkStateLocked(uplinkName)
		uplinkState.stateUID = state.UID
		uplinkState.publicationInvalid = false
		affectedUplinks[uplinkName] = struct{}{}
	}
	for networkName, uplinkName := range c.uplinkByNetworkName {
		if desiredUplink, found := desired[networkName]; found && desiredUplink == uplinkName {
			continue
		}
		if state := c.uplinks[uplinkName]; state != nil {
			delete(state.networks, networkName)
		}
		delete(c.uplinkByNetworkName, networkName)
		affectedUplinks[uplinkName] = struct{}{}
	}
	for networkName, uplinkName := range desired {
		c.markNetworkPendingLocked(networkName, uplinkName)
		affectedUplinks[uplinkName] = struct{}{}
	}
	c.mutex.Unlock()

	c.enqueueGatewayConditions(sortedMapKeys(affectedUplinks))
	return nil
}

// PrepareNetwork adds a newly discovered network to aggregate readiness before
// its gateway controller starts.
func (c *UplinkStateGatewayStatusController) PrepareNetwork(network util.NetInfo) error {
	if network.Uplink() == "" {
		return nil
	}
	_, affectedUplinks := c.markNetworkPending(network)
	c.enqueueGatewayConditions(affectedUplinks)
	return nil
}

// ReconcileNetwork records the result of one UDN-owned dataplane operation.
// It does not serialize callbacks for different CUDNs; each gateway controller
// serializes calls for its own CUDN. Status
// publication is coalesced and retried asynchronously; the returned error is
// only the callback's dataplane result.
func (c *UplinkStateGatewayStatusController) ReconcileNetwork(network util.NetInfo, reconcile func() error) error {
	if network.Uplink() == "" {
		return reconcile()
	}

	uplinkState, affectedUplinks := c.markNetworkPending(network)
	c.enqueueGatewayConditions(affectedUplinks)

	reconcileErr := reconcile()
	c.completeNetworkReconcile(network, uplinkState, reconcileErr)
	return reconcileErr
}

// DeleteNetwork records cleanup and removes the network from aggregate
// readiness only after cleanup succeeds. Status publication is coalesced and
// retried asynchronously.
func (c *UplinkStateGatewayStatusController) DeleteNetwork(network util.NetInfo, reconcile func() error) error {
	if network.Uplink() == "" {
		return reconcile()
	}

	uplinkState, affectedUplinks := c.markNetworkPending(network)
	c.enqueueGatewayConditions(affectedUplinks)

	reconcileErr := reconcile()
	c.completeNetworkDelete(network, uplinkState, reconcileErr)
	return reconcileErr
}

func (c *UplinkStateGatewayStatusController) markNetworkPending(
	network util.NetInfo,
) (*uplinkGatewayState, []string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	previousUplink := c.uplinkByNetworkName[network.GetNetworkName()]
	if previousUplink != "" && previousUplink != network.Uplink() {
		if previousState := c.uplinks[previousUplink]; previousState != nil {
			delete(previousState.networks, network.GetNetworkName())
		}
	}

	uplinkState := c.markNetworkPendingLocked(
		network.GetNetworkName(), network.Uplink())
	affectedUplinks := []string{network.Uplink()}
	if previousUplink != "" && previousUplink != network.Uplink() {
		affectedUplinks = append(affectedUplinks, previousUplink)
	}
	return uplinkState, affectedUplinks
}

func (c *UplinkStateGatewayStatusController) ensureUplinkStateLocked(uplinkName string) *uplinkGatewayState {
	uplinkState := c.uplinks[uplinkName]
	if uplinkState == nil {
		uplinkState = &uplinkGatewayState{networks: map[string]*uplinkGatewayNetworkState{}}
		c.uplinks[uplinkName] = uplinkState
	}
	return uplinkState
}

func (c *UplinkStateGatewayStatusController) markNetworkPendingLocked(
	networkName, uplinkName string,
) *uplinkGatewayState {
	uplinkState := c.ensureUplinkStateLocked(uplinkName)
	networkState := uplinkState.networks[networkName]
	if networkState == nil {
		networkState = &uplinkGatewayNetworkState{}
		uplinkState.networks[networkName] = networkState
	}
	networkState.markPending()
	c.uplinkByNetworkName[networkName] = uplinkName
	return uplinkState
}

func (c *UplinkStateGatewayStatusController) completeNetworkReconcile(
	network util.NetInfo,
	expectedUplinkState *uplinkGatewayState,
	reconcileErr error,
) {
	c.mutex.Lock()
	uplinkState := c.uplinks[network.Uplink()]
	// A deletion and recreation with the same Uplink name creates a new state
	// pointer, so completion from the retired lifecycle cannot affect it.
	if uplinkState != expectedUplinkState {
		c.mutex.Unlock()
		return
	}
	networkState := uplinkState.networks[network.GetNetworkName()]
	if networkState != nil {
		if reconcileErr == nil {
			networkState.phase = uplinkGatewayNetworkReady
			networkState.reason = uplinkv1alpha1.UplinkStateReasonGatewayConfigured
			networkState.message = ""
		} else {
			networkState.phase = uplinkGatewayNetworkFailed
			networkState.reason = uplinkGatewayFailureReason(reconcileErr)
			networkState.message = reconcileErr.Error()
		}
	}
	c.mutex.Unlock()
	c.enqueueGatewayCondition(network.Uplink())
}

func (c *UplinkStateGatewayStatusController) completeNetworkDelete(
	network util.NetInfo,
	expectedUplinkState *uplinkGatewayState,
	reconcileErr error,
) {
	c.mutex.Lock()
	uplinkState := c.uplinks[network.Uplink()]
	if uplinkState != expectedUplinkState {
		// Successful terminal network cleanup remains valid across an
		// UplinkState recreation. Remove the pending membership copied into the
		// new epoch, but never carry an old cleanup failure into that epoch.
		if reconcileErr == nil &&
			c.uplinkByNetworkName[network.GetNetworkName()] == network.Uplink() {
			if uplinkState != nil {
				delete(uplinkState.networks, network.GetNetworkName())
			}
			delete(c.uplinkByNetworkName, network.GetNetworkName())
		}
		c.mutex.Unlock()
		c.enqueueGatewayCondition(network.Uplink())
		return
	}
	networkState := uplinkState.networks[network.GetNetworkName()]
	if networkState != nil {
		if reconcileErr == nil {
			delete(uplinkState.networks, network.GetNetworkName())
			delete(c.uplinkByNetworkName, network.GetNetworkName())
		} else {
			networkState.phase = uplinkGatewayNetworkFailed
			networkState.reason = uplinkGatewayFailureReason(reconcileErr)
			networkState.message = reconcileErr.Error()
		}
	}
	c.mutex.Unlock()
	c.enqueueGatewayCondition(network.Uplink())
}

func (c *UplinkStateGatewayStatusController) enqueueGatewayConditions(uplinkNames []string) {
	sort.Strings(uplinkNames)
	for i, uplinkName := range uplinkNames {
		if i > 0 && uplinkName == uplinkNames[i-1] {
			continue
		}
		c.enqueueGatewayCondition(uplinkName)
	}
}

func (c *UplinkStateGatewayStatusController) enqueueGatewayCondition(uplinkName string) {
	c.publishQueue.AddAfter(uplinkName, uplinkGatewayStatusBatchDelay)
}

func (c *UplinkStateGatewayStatusController) processNextGatewayCondition() bool {
	uplinkName, shutdown := c.publishQueue.Get()
	if shutdown {
		return false
	}
	defer c.publishQueue.Done(uplinkName)

	if err := c.publishGatewayCondition(uplinkName); err != nil {
		if isUplinkStateNotFound(err) {
			// Discovery may not have created the node-local UplinkState yet.
			// Its add event requests publication again.
			c.publishQueue.Forget(uplinkName)
			return true
		}
		klog.Errorf("Failed to publish gateway readiness for Uplink %s: %v",
			uplinkName, err)
		c.publishQueue.AddRateLimited(uplinkName)
		return true
	}
	c.publishQueue.Forget(uplinkName)
	return true
}

// publishGatewayCondition writes aggregate readiness for all active CUDNs
// using this Uplink.
func (c *UplinkStateGatewayStatusController) publishGatewayCondition(uplinkName string) error {
	c.mutex.Lock()
	uplinkState := c.uplinks[uplinkName]
	c.mutex.Unlock()
	if uplinkState == nil {
		return nil
	}

	// Serialize the read, merge, and apply sequence and access to
	// lastCondition for this Uplink.
	uplinkState.conditionMutex.Lock()
	defer uplinkState.conditionMutex.Unlock()

	// An UplinkState delete or recreation may have retired the entry after this
	// publisher read it but before it acquired conditionMutex. Do not publish
	// readiness from the superseded object lifecycle.
	c.mutex.Lock()
	current := c.uplinks[uplinkName]
	c.mutex.Unlock()
	if current != uplinkState {
		return nil
	}

	desiredCondition, found := c.gatewayCondition(uplinkName, uplinkState)
	if !found {
		return nil
	}
	stateName := uplinkutil.StateName(uplinkName, c.nodeName)
	state, err := uplinkutil.GetState(c.uplinkStateLister, uplinkName, c.nodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			existing := []metav1.Condition(nil)
			if uplinkState.lastCondition != nil {
				existing = []metav1.Condition{*uplinkState.lastCondition}
			}
			condition, _ := util.MergeStatusCondition(existing, desiredCondition)
			uplinkState.lastCondition = condition.DeepCopy()
		}
		return fmt.Errorf("failed to get UplinkState %s from cache: %w", stateName, err)
	}
	c.mutex.Lock()
	current = c.uplinks[uplinkName]
	uidMatches := current == uplinkState &&
		(uplinkState.stateUID == "" || state.UID == uplinkState.stateUID)
	c.mutex.Unlock()
	if !uidMatches {
		return nil
	}

	if !uplinkState.forcePublish &&
		conditionsEqual(uplinkState.lastAppliedCondition, desiredCondition) {
		return nil
	}

	existingConditions := state.Status.Conditions
	if uplinkState.lastCondition != nil {
		existingConditions = []metav1.Condition{*uplinkState.lastCondition}
	}
	condition, _ := util.MergeStatusCondition(existingConditions, desiredCondition)
	// Keep the merge result as the retry base while the informer cache catches
	// up, preserving LastTransitionTime across duplicate publications.
	uplinkState.lastCondition = condition.DeepCopy()
	ctx, cancel := context.WithTimeout(context.Background(), uplinkGatewayAPITimeout)
	defer cancel()
	applyState := uplinkapply.UplinkState(stateName).WithStatus(
		uplinkapply.UplinkStateStatus().WithConditions(util.ConditionToApply(condition)),
	)
	if state.UID != "" {
		applyState = applyState.WithUID(state.UID)
	}
	_, err = c.uplinkClient.K8sV1alpha1().UplinkStates().Apply(
		ctx,
		applyState,
		metav1.ApplyOptions{FieldManager: c.fieldManager, Force: true},
	)
	if err != nil {
		return fmt.Errorf("failed to apply UplinkState %s status: %w", stateName, err)
	}
	uplinkState.lastAppliedCondition = condition.DeepCopy()
	uplinkState.forcePublish = false
	return nil
}

func (c *UplinkStateGatewayStatusController) gatewayCondition(
	uplinkName string,
	expectedUplinkState *uplinkGatewayState,
) (metav1.Condition, bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	uplinkState := c.uplinks[uplinkName]
	if uplinkState != expectedUplinkState {
		return metav1.Condition{}, false
	}
	if uplinkState.publicationInvalid {
		return metav1.Condition{}, false
	}
	if len(uplinkState.networks) == 0 {
		return metav1.Condition{
			Type:    c.conditionType,
			Status:  metav1.ConditionTrue,
			Reason:  uplinkv1alpha1.UplinkStateReasonGatewayConfigured,
			Message: "No active CUDNs require Uplink gateway programming",
		}, true
	}

	networkNames := make([]string, 0, len(uplinkState.networks))
	for networkName := range uplinkState.networks {
		networkNames = append(networkNames, networkName)
	}
	sort.Strings(networkNames)

	failureReasons := map[string]struct{}{}
	examples := make([]string, 0, maxGatewayConditionExamples)
	incomplete := 0
	for _, networkName := range networkNames {
		networkState := uplinkState.networks[networkName]
		if networkState.phase == uplinkGatewayNetworkReady {
			continue
		}
		incomplete++
		failureReasons[networkState.reason] = struct{}{}
		if len(examples) < maxGatewayConditionExamples {
			example := fmt.Sprintf("%s=%s", networkName, networkState.reason)
			if networkState.message != "" {
				example += ": " + truncateGatewayConditionError(networkState.message)
			}
			examples = append(examples, example)
		}
	}
	if incomplete == 0 {
		return metav1.Condition{
			Type:    c.conditionType,
			Status:  metav1.ConditionTrue,
			Reason:  uplinkv1alpha1.UplinkStateReasonGatewayConfigured,
			Message: fmt.Sprintf("Uplink gateway programming succeeded for %d active CUDN(s)", len(networkNames)),
		}, true
	}

	return metav1.Condition{
		Type:   c.conditionType,
		Status: metav1.ConditionFalse,
		Reason: aggregateGatewayFailureReason(failureReasons),
		Message: fmt.Sprintf(
			"%d of %d active CUDN(s) have incomplete Uplink gateway configuration; examples: %s",
			incomplete, len(networkNames), strings.Join(examples, ", ")),
	}, true
}

func aggregateGatewayFailureReason(reasons map[string]struct{}) string {
	for _, reason := range []string{
		uplinkv1alpha1.UplinkStateReasonConfigurationConflict,
		uplinkv1alpha1.UplinkStateReasonVRFAttachmentFailed,
		uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed,
		uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed,
		uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending,
	} {
		if _, found := reasons[reason]; found {
			return reason
		}
	}
	return uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed
}

func conditionsEqual(existing *metav1.Condition, desired metav1.Condition) bool {
	return existing != nil && existing.Status == desired.Status && existing.Reason == desired.Reason &&
		existing.Message == desired.Message
}

func truncateGatewayConditionError(message string) string {
	if len(message) <= maxGatewayConditionErrorLength {
		return message
	}
	return message[:maxGatewayConditionErrorLength]
}

func sortedMapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func isUplinkStateNotFound(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsNotFound(err) {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		unwrapped := joined.Unwrap()
		if len(unwrapped) == 0 {
			return false
		}
		for _, nested := range unwrapped {
			if !isUplinkStateNotFound(nested) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return isUplinkStateNotFound(wrapped.Unwrap())
	}
	return false
}

type uplinkGatewayError struct {
	reason string
	err    error
}

func (e *uplinkGatewayError) Error() string { return e.err.Error() }
func (e *uplinkGatewayError) Unwrap() error { return e.err }

func newUplinkGatewayError(reason string, err error) error {
	var gatewayErr *uplinkGatewayError
	if errors.As(err, &gatewayErr) {
		return err
	}
	return &uplinkGatewayError{reason: reason, err: err}
}

func uplinkGatewayFailureReason(err error) string {
	var gatewayErr *uplinkGatewayError
	if errors.As(err, &gatewayErr) {
		return gatewayErr.reason
	}
	return uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed
}
