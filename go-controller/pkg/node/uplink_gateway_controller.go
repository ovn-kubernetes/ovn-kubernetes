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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/applyconfiguration/uplink/v1alpha1"
	uplinkclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

const (
	uplinkGatewayFieldManager      = "ovnkube-node-uplink-gateway-controller"
	uplinkHostGatewayFieldManager  = "ovnkube-node-uplink-host-gateway-controller"
	maxGatewayConditionExamples    = 3
	maxGatewayConditionErrorLength = 160
)

type uplinkGatewayNetworkPhase string

const (
	uplinkGatewayNetworkPending uplinkGatewayNetworkPhase = "pending"
	uplinkGatewayNetworkReady   uplinkGatewayNetworkPhase = "ready"
	uplinkGatewayNetworkFailed  uplinkGatewayNetworkPhase = "failed"
)

type uplinkGatewayNetworkState struct {
	generation uint64
	phase      uplinkGatewayNetworkPhase
	reason     string
	message    string
}

type uplinkGatewayState struct {
	operationMutex sync.Mutex
	conditionMutex sync.Mutex
	networks       map[string]*uplinkGatewayNetworkState
	lastCondition  *metav1.Condition
}

// UplinkGatewayController coordinates gateway programming and readiness for
// all active CUDNs using an Uplink on the local node.
type UplinkGatewayController struct {
	nodeName          string
	uplinkClient      uplinkclientset.Interface
	uplinkStateLister uplinklisters.UplinkStateLister
	// The DPU owns GatewayReady in split-DPU deployments because it is the
	// component that waits for OVN patch ports and programs OVS/OpenFlow.
	// DPU-host gateway reconciliation covers the host side, the VRF
	// attachment of the Uplink gateway interface, and publishes it through
	// its own HostGatewayReady condition so that it does not race the DPU
	// for ownership of the shared condition.
	conditionType       string
	fieldManager        string
	mutex               sync.Mutex
	uplinks             map[string]*uplinkGatewayState
	uplinkByNetworkName map[string]string
}

// NewUplinkGatewayController creates the node-local Uplink gateway coordinator.
func NewUplinkGatewayController(
	nodeName string,
	uplinkClient uplinkclientset.Interface,
	uplinkStateLister uplinklisters.UplinkStateLister,
) *UplinkGatewayController {
	conditionType := uplinkv1alpha1.UplinkStateConditionGatewayReady
	fieldManager := uplinkGatewayFieldManager
	if config.IsModeDPUHost() {
		conditionType = uplinkv1alpha1.UplinkStateConditionHostGatewayReady
		fieldManager = uplinkHostGatewayFieldManager
	}
	return &UplinkGatewayController{
		nodeName:            nodeName,
		uplinkClient:        uplinkClient,
		uplinkStateLister:   uplinkStateLister,
		conditionType:       conditionType,
		fieldManager:        fieldManager,
		uplinks:             map[string]*uplinkGatewayState{},
		uplinkByNetworkName: map[string]string{},
	}
}

// SyncNetworks seeds the complete active network set before individual network
// controllers start. Missing UplinkStates are tolerated because discovery may
// still be completing during node startup.
func (c *UplinkGatewayController) SyncNetworks(networks ...util.NetInfo) error {
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
		if c.uplinks[uplinkName] == nil {
			c.uplinks[uplinkName] = &uplinkGatewayState{networks: map[string]*uplinkGatewayNetworkState{}}
		}
		affectedUplinks[uplinkName] = struct{}{}
	}
	for networkName, uplinkName := range c.uplinkByNetworkName {
		if desiredUplink, found := desired[networkName]; found && desiredUplink == uplinkName {
			continue
		}
		delete(c.uplinks[uplinkName].networks, networkName)
		delete(c.uplinkByNetworkName, networkName)
		affectedUplinks[uplinkName] = struct{}{}
	}
	for networkName, uplinkName := range desired {
		c.markNetworkPendingLocked(networkName, uplinkName)
		affectedUplinks[uplinkName] = struct{}{}
	}
	c.mutex.Unlock()

	var errs []error
	for _, uplinkName := range sortedMapKeys(affectedUplinks) {
		if err := c.publishGatewayCondition(uplinkName); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return utilerrors.Join(errs...)
}

// PrepareNetwork marks the changed active set as pending before the network
// manager starts or reconfigures the per-network gateway.
func (c *UplinkGatewayController) PrepareNetwork(network util.NetInfo) error {
	if network.Uplink() == "" {
		return nil
	}
	_, _, affectedUplinks := c.markNetworkPending(network)
	return c.publishGatewayConditions(affectedUplinks)
}

// ReconcileNetwork serializes gateway programming for networks sharing an
// Uplink and folds the result into the aggregate GatewayReady condition.
func (c *UplinkGatewayController) ReconcileNetwork(network util.NetInfo, reconcile func() error) error {
	if network.Uplink() == "" {
		return reconcile()
	}

	uplinkState, generation, affectedUplinks := c.markNetworkPending(network)
	if err := c.publishGatewayConditions(affectedUplinks); err != nil {
		return err
	}

	uplinkState.operationMutex.Lock()
	reconcileErr := reconcile()
	uplinkState.operationMutex.Unlock()

	statusErr := c.completeNetworkReconcile(network, generation, reconcileErr)
	return utilerrors.Join(reconcileErr, statusErr)
}

// DeleteNetwork serializes gateway cleanup and removes the network from the
// aggregate desired set only after cleanup succeeds.
func (c *UplinkGatewayController) DeleteNetwork(network util.NetInfo, reconcile func() error) error {
	if network.Uplink() == "" {
		return reconcile()
	}

	uplinkState, generation, affectedUplinks := c.markNetworkPending(network)
	if err := c.publishGatewayConditions(affectedUplinks); err != nil {
		return err
	}

	uplinkState.operationMutex.Lock()
	reconcileErr := reconcile()
	uplinkState.operationMutex.Unlock()

	statusErr := c.completeNetworkDelete(network, generation, reconcileErr)
	return utilerrors.Join(reconcileErr, statusErr)
}

// InvalidateGatewayState starts a new node-local gateway lifecycle for an
// Uplink that still exists but no longer selects this node. Preserve the
// active network set so aggregate readiness cannot become true until every
// affected CUDN has reconciled again.
func (c *UplinkGatewayController) InvalidateGatewayState(uplinkName string) {
	c.mutex.Lock()
	uplinkState := c.uplinks[uplinkName]
	if uplinkState != nil {
		for _, networkState := range uplinkState.networks {
			networkState.generation++
			networkState.phase = uplinkGatewayNetworkPending
			networkState.reason = uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending
			networkState.message = "gateway reconciliation is pending"
		}
	}
	c.mutex.Unlock()

	if uplinkState == nil {
		return
	}
	// Wait for an already-started publish before clearing lastCondition. The
	// caller deletes the UplinkState only after this method returns, so no old
	// publish can race with recreation of the node-local state.
	uplinkState.conditionMutex.Lock()
	uplinkState.lastCondition = nil
	uplinkState.conditionMutex.Unlock()
}

// DeleteGatewayState forgets all cached state for an Uplink resource that no
// longer exists. A later Uplink with the same name must start with a fresh
// gateway lifecycle.
func (c *UplinkGatewayController) DeleteGatewayState(uplinkName string) {
	c.mutex.Lock()
	uplinkState := c.uplinks[uplinkName]
	delete(c.uplinks, uplinkName)
	for networkName, networkUplinkName := range c.uplinkByNetworkName {
		if networkUplinkName == uplinkName {
			delete(c.uplinkByNetworkName, networkName)
		}
	}
	c.mutex.Unlock()

	if uplinkState == nil {
		return
	}
	// Drain any status publish that captured the old cache entry before it was
	// removed. Subsequent publishers revalidate the entry and return without
	// applying a condition for the deleted lifecycle.
	uplinkState.conditionMutex.Lock()
	uplinkState.lastCondition = nil
	uplinkState.conditionMutex.Unlock()
}

func (c *UplinkGatewayController) markNetworkPending(
	network util.NetInfo,
) (*uplinkGatewayState, uint64, []string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	previousUplink := c.uplinkByNetworkName[network.GetNetworkName()]
	if previousUplink != "" && previousUplink != network.Uplink() {
		if previousUplinkState := c.uplinks[previousUplink]; previousUplinkState != nil {
			delete(previousUplinkState.networks, network.GetNetworkName())
		}
	}

	uplinkState, generation := c.markNetworkPendingLocked(network.GetNetworkName(), network.Uplink())
	affectedUplinks := []string{network.Uplink()}
	if previousUplink != "" && previousUplink != network.Uplink() {
		affectedUplinks = append(affectedUplinks, previousUplink)
	}
	return uplinkState, generation, affectedUplinks
}

func (c *UplinkGatewayController) markNetworkPendingLocked(
	networkName, uplinkName string,
) (*uplinkGatewayState, uint64) {
	uplinkState := c.uplinks[uplinkName]
	if uplinkState == nil {
		uplinkState = &uplinkGatewayState{networks: map[string]*uplinkGatewayNetworkState{}}
		c.uplinks[uplinkName] = uplinkState
	}
	networkState := uplinkState.networks[networkName]
	if networkState == nil {
		networkState = &uplinkGatewayNetworkState{}
		uplinkState.networks[networkName] = networkState
	}
	networkState.generation++
	networkState.phase = uplinkGatewayNetworkPending
	networkState.reason = uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending
	networkState.message = "gateway reconciliation is pending"
	c.uplinkByNetworkName[networkName] = uplinkName
	return uplinkState, networkState.generation
}

func (c *UplinkGatewayController) completeNetworkReconcile(
	network util.NetInfo,
	generation uint64,
	reconcileErr error,
) error {
	c.mutex.Lock()
	uplinkState := c.uplinks[network.Uplink()]
	if uplinkState == nil {
		c.mutex.Unlock()
		return nil
	}
	networkState := uplinkState.networks[network.GetNetworkName()]
	if networkState != nil && networkState.generation == generation {
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
	return c.publishGatewayCondition(network.Uplink())
}

func (c *UplinkGatewayController) completeNetworkDelete(
	network util.NetInfo,
	generation uint64,
	reconcileErr error,
) error {
	c.mutex.Lock()
	uplinkState := c.uplinks[network.Uplink()]
	if uplinkState == nil {
		c.mutex.Unlock()
		return nil
	}
	networkState := uplinkState.networks[network.GetNetworkName()]
	if networkState != nil && networkState.generation == generation {
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
	return c.publishGatewayCondition(network.Uplink())
}

// ConditionType is the UplinkState condition this controller publishes:
// GatewayReady, or HostGatewayReady on the DPU-host.
func (c *UplinkGatewayController) ConditionType() string {
	return c.conditionType
}

// RepublishGatewayCondition restores this controller's gateway condition on
// an UplinkState recreated after an out-of-band deletion. It only restores a
// condition that was already published once: on a first creation the
// condition is published by network reconciliation.
func (c *UplinkGatewayController) RepublishGatewayCondition(uplinkName string) error {
	c.mutex.Lock()
	uplinkState := c.uplinks[uplinkName]
	c.mutex.Unlock()
	if uplinkState == nil {
		return nil
	}
	uplinkState.conditionMutex.Lock()
	published := uplinkState.lastCondition != nil
	uplinkState.conditionMutex.Unlock()
	if !published {
		return nil
	}
	return c.publishGatewayCondition(uplinkName)
}

func (c *UplinkGatewayController) publishGatewayConditions(uplinkNames []string) error {
	var errs []error
	sort.Strings(uplinkNames)
	for i, uplinkName := range uplinkNames {
		if i > 0 && uplinkName == uplinkNames[i-1] {
			continue
		}
		if err := c.publishGatewayCondition(uplinkName); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.Join(errs...)
}

// publishGatewayCondition writes this controller's aggregate gateway
// condition (GatewayReady, or HostGatewayReady on the DPU-host) for all
// active CUDNs using the node-local UplinkState.
func (c *UplinkGatewayController) publishGatewayCondition(uplinkName string) error {
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

	// DeleteGatewayState may have removed the entry after this publisher read
	// it but before it acquired conditionMutex. Do not publish readiness from a
	// superseded Uplink lifecycle.
	c.mutex.Lock()
	current := c.uplinks[uplinkName]
	c.mutex.Unlock()
	if current != uplinkState {
		return nil
	}

	stateName := uplinkutil.StateName(uplinkName, c.nodeName)
	state, err := uplinkutil.GetState(c.uplinkStateLister, uplinkName, c.nodeName)
	if err != nil {
		return fmt.Errorf("failed to get UplinkState %s from cache: %w", stateName, err)
	}

	desiredCondition, found := c.gatewayCondition(uplinkName, uplinkState)
	if !found {
		return nil
	}
	cachedCondition := meta.FindStatusCondition(state.Status.Conditions, desiredCondition.Type)
	if conditionsEqual(uplinkState.lastCondition, desiredCondition) && conditionsEqual(cachedCondition, desiredCondition) {
		return nil
	}

	existingConditions := state.Status.Conditions
	if uplinkState.lastCondition != nil {
		// The informer may lag the preceding apply. Merge against the last
		// condition published here to preserve its transition time.
		existingConditions = []metav1.Condition{*uplinkState.lastCondition}
	}
	condition, _ := util.MergeStatusCondition(existingConditions, desiredCondition)
	_, err = c.uplinkClient.K8sV1alpha1().UplinkStates().Apply(
		context.Background(),
		uplinkapply.UplinkState(stateName).WithStatus(
			uplinkapply.UplinkStateStatus().WithConditions(util.ConditionToApply(condition)),
		),
		metav1.ApplyOptions{
			FieldManager: c.fieldManager,
			Force:        true,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to apply UplinkState %s status: %w", stateName, err)
	}
	uplinkState.lastCondition = condition.DeepCopy()
	return nil
}

// gatewayCondition aggregates gateway programming for every active CUDN using
// the Uplink. GatewayReady remains false while any CUDN is pending or failed.
func (c *UplinkGatewayController) gatewayCondition(
	uplinkName string,
	expectedUplinkState *uplinkGatewayState,
) (metav1.Condition, bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	uplinkState := c.uplinks[uplinkName]
	if uplinkState != expectedUplinkState {
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
			incomplete,
			len(networkNames),
			strings.Join(examples, ", "),
		),
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

type uplinkGatewayError struct {
	reason string
	err    error
}

func (e *uplinkGatewayError) Error() string {
	return e.err.Error()
}

func (e *uplinkGatewayError) Unwrap() error {
	return e.err
}

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
