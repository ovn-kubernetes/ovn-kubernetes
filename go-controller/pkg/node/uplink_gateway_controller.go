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
	generation    uint64
	phase         uplinkGatewayNetworkPhase
	reason        string
	message       string
	configuration string
	lifecycle     uplinkGatewayNetworkLifecycle
}

type uplinkGatewayState struct {
	operationMutex *sync.Mutex
	conditionMutex sync.Mutex
	networks       map[string]*uplinkGatewayNetworkState
	lastCondition  *metav1.Condition
	configuration  string
	resolved       *resolvedUplinkGateway
	invalidated    bool
}

type uplinkGatewayNetworkLifecycle interface {
	reconcileUplinkGateway(*resolvedUplinkGateway) error
	withdrawUplinkGateway() error
}

// UplinkGatewayController coordinates gateway programming and readiness for
// all active CUDNs using an Uplink on the local node.
type UplinkGatewayController struct {
	nodeName          string
	uplinkClient      uplinkclientset.Interface
	uplinkStateLister uplinklisters.UplinkStateLister
	// The DPU owns GatewayReady in split-DPU deployments because it is the
	// component that waits for OVN patch ports and programs OVS/OpenFlow.
	// DPU-host gateway reconciliation still runs, but must not race the DPU
	// for ownership of the shared condition.
	publishStatus       bool
	mutex               sync.Mutex
	uplinks             map[string]*uplinkGatewayState
	uplinkByNetworkName map[string]string
	// operationMutexes outlive the corresponding Uplink cache entry while
	// network controllers from that lifecycle still need to clean up. This
	// preserves shared-Uplink serialization after DeleteGatewayState.
	operationMutexes map[string]*sync.Mutex
	retiredNetworks  map[string]map[string]struct{}
}

// NewUplinkGatewayController creates the node-local Uplink gateway coordinator.
func NewUplinkGatewayController(
	nodeName string,
	uplinkClient uplinkclientset.Interface,
	uplinkStateLister uplinklisters.UplinkStateLister,
) *UplinkGatewayController {
	return &UplinkGatewayController{
		nodeName:            nodeName,
		uplinkClient:        uplinkClient,
		uplinkStateLister:   uplinkStateLister,
		publishStatus:       !config.IsModeDPUHost(),
		uplinks:             map[string]*uplinkGatewayState{},
		uplinkByNetworkName: map[string]string{},
		operationMutexes:    map[string]*sync.Mutex{},
		retiredNetworks:     map[string]map[string]struct{}{},
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
			c.uplinks[uplinkName] = c.newUplinkGatewayStateLocked(uplinkName)
		}
		if resolved, err := c.resolvedGateway(state); err == nil {
			c.uplinks[uplinkName].configuration = resolvedUplinkGatewayConfiguration(resolved)
			c.uplinks[uplinkName].resolved = resolved
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
		if err := c.publishGatewayCondition(uplinkName); err != nil && !isUplinkStateNotFound(err) {
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
	_, _, affectedUplinks, err := c.markNetworkPending(network)
	if err != nil {
		return err
	}
	return c.publishGatewayConditions(affectedUplinks)
}

// ReconcileNetwork serializes gateway programming for networks sharing an
// Uplink and folds the result into the aggregate GatewayReady condition.
func (c *UplinkGatewayController) ReconcileNetwork(network util.NetInfo, reconcile func() error) error {
	if network.Uplink() == "" {
		return reconcile()
	}

	uplinkState, generation, affectedUplinks, err := c.markNetworkPending(network)
	if err != nil {
		return err
	}
	if err := c.publishGatewayConditions(affectedUplinks); err != nil && !isUplinkStateNotFound(err) {
		return err
	}

	uplinkState.operationMutex.Lock()
	reconcileErr := reconcile()
	uplinkState.operationMutex.Unlock()

	statusErr := c.completeNetworkReconcile(network, uplinkState, generation, reconcileErr)
	return utilerrors.Join(reconcileErr, statusErr)
}

// ActivateNetwork registers the non-terminal Uplink lifecycle callbacks after
// the network's initial gateway programming succeeds. If the Uplink was
// invalidated concurrently, withdraw the just-created programming before
// returning so the network manager retries against the next resolved state.
func (c *UplinkGatewayController) ActivateNetwork(
	network util.NetInfo,
	lifecycle uplinkGatewayNetworkLifecycle,
) error {
	if network.Uplink() == "" {
		return nil
	}

	c.mutex.Lock()
	uplinkState := c.uplinks[network.Uplink()]
	operationMutex := c.operationMutexes[network.Uplink()]
	c.mutex.Unlock()
	if uplinkState == nil {
		if operationMutex != nil {
			operationMutex.Lock()
			defer operationMutex.Unlock()
		}
		return c.failNetworkActivation(
			network,
			lifecycle,
			fmt.Errorf("missing gateway state for Uplink %s", network.Uplink()),
		)
	}

	uplinkState.operationMutex.Lock()
	defer uplinkState.operationMutex.Unlock()

	c.mutex.Lock()
	if c.uplinks[network.Uplink()] != uplinkState {
		c.mutex.Unlock()
		return c.failNetworkActivation(
			network,
			lifecycle,
			fmt.Errorf("gateway state for Uplink %s was replaced", network.Uplink()),
		)
	}
	networkState := uplinkState.networks[network.GetNetworkName()]
	if networkState == nil {
		c.mutex.Unlock()
		return c.failNetworkActivation(
			network,
			lifecycle,
			fmt.Errorf("missing gateway state for network %s", network.GetNetworkName()),
		)
	}
	networkState.lifecycle = lifecycle
	invalidated := uplinkState.invalidated
	configurationChanged := networkState.configuration != uplinkState.configuration
	desiredConfiguration := uplinkState.configuration
	resolved := uplinkState.resolved
	c.mutex.Unlock()

	if !invalidated && (!configurationChanged || resolved == nil) {
		return nil
	}

	var reconcileErr error
	if invalidated {
		reconcileErr = lifecycle.withdrawUplinkGateway()
	} else {
		reconcileErr = lifecycle.reconcileUplinkGateway(resolved)
	}
	if reconcileErr != nil {
		c.setNetworkFailure(
			uplinkState,
			network.GetNetworkName(),
			uplinkGatewayFailureReason(reconcileErr),
			reconcileErr,
		)
	} else if !invalidated {
		c.setNetworkReady(
			uplinkState,
			network.GetNetworkName(),
			desiredConfiguration,
		)
		if err := c.publishGatewayCondition(network.Uplink()); err != nil {
			reconcileErr = err
		}
	}

	if !invalidated && reconcileErr == nil {
		return nil
	}
	if !invalidated {
		cleanupErr := lifecycle.withdrawUplinkGateway()
		if cleanupErr != nil {
			reconcileErr = utilerrors.Join(
				reconcileErr,
				fmt.Errorf("failed to clean up gateway after activation failure: %w", cleanupErr),
			)
		}
	}

	// Start did not succeed, so this gateway object will not become the active
	// network controller. Avoid retaining a callback to it across the retry.
	c.mutex.Lock()
	if current := c.uplinks[network.Uplink()]; current == uplinkState {
		if currentNetwork := current.networks[network.GetNetworkName()]; currentNetwork != nil {
			currentNetwork.lifecycle = nil
		}
	}
	c.mutex.Unlock()
	if reconcileErr != nil {
		return reconcileErr
	}
	return fmt.Errorf("Uplink %s gateway state was invalidated during network startup", network.Uplink())
}

func (c *UplinkGatewayController) failNetworkActivation(
	network util.NetInfo,
	lifecycle uplinkGatewayNetworkLifecycle,
	activationErr error,
) error {
	cleanupErr := lifecycle.withdrawUplinkGateway()
	// Start will discard this gateway object, so it cannot service a later
	// cleanup retry for a retired cache entry. Release the tombstone and let
	// the network manager's next Start reconcile any partial cleanup.
	c.completeRetiredNetworkCleanup(network.Uplink(), network.GetNetworkName())
	if cleanupErr == nil {
		return activationErr
	}
	return utilerrors.Join(
		activationErr,
		fmt.Errorf("failed to clean up gateway after activation failure: %w", cleanupErr),
	)
}

// DeleteNetwork serializes gateway cleanup and removes the network from the
// aggregate desired set only after cleanup succeeds.
func (c *UplinkGatewayController) DeleteNetwork(network util.NetInfo, reconcile func() error) error {
	if network.Uplink() == "" {
		return reconcile()
	}
	c.mutex.Lock()
	uplinkState := c.uplinks[network.Uplink()]
	trackedUplink := c.uplinkByNetworkName[network.GetNetworkName()]
	if uplinkState != nil && trackedUplink == network.Uplink() {
		if networkState := uplinkState.networks[network.GetNetworkName()]; networkState != nil {
			networkState.generation++
			networkState.phase = uplinkGatewayNetworkPending
			networkState.reason = uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending
			networkState.message = "gateway reconciliation is pending"
			generation := networkState.generation
			c.mutex.Unlock()

			if err := c.publishGatewayCondition(network.Uplink()); err != nil && !isUplinkStateNotFound(err) {
				return err
			}

			uplinkState.operationMutex.Lock()
			reconcileErr := reconcile()
			uplinkState.operationMutex.Unlock()

			statusErr := c.completeNetworkDelete(network, uplinkState, generation, reconcileErr)
			if isUplinkStateNotFound(statusErr) {
				statusErr = nil
			}
			return utilerrors.Join(reconcileErr, statusErr)
		}
	}
	operationMutex := c.operationMutexes[network.Uplink()]
	_, retired := c.retiredNetworks[network.Uplink()][network.GetNetworkName()]
	c.mutex.Unlock()
	if retired && operationMutex != nil {
		operationMutex.Lock()
		reconcileErr := reconcile()
		operationMutex.Unlock()
		if reconcileErr == nil {
			c.completeRetiredNetworkCleanup(network.Uplink(), network.GetNetworkName())
		}
		return reconcileErr
	}
	// The network is not part of either an active or a retired lifecycle.
	// Cleanup is still idempotently required, but there is no cached operation
	// with which it can race and no cache entry should be recreated.
	return reconcile()
}

// InvalidateGatewayState starts a new node-local gateway lifecycle for an
// Uplink that still exists but no longer selects this node. Preserve the
// active network set so aggregate readiness cannot become true until every
// affected CUDN has reconciled again.
func (c *UplinkGatewayController) InvalidateGatewayState(uplinkName string) error {
	c.mutex.Lock()
	uplinkState := c.uplinks[uplinkName]
	if uplinkState != nil {
		uplinkState.invalidated = true
		uplinkState.configuration = ""
		uplinkState.resolved = nil
		for _, networkState := range uplinkState.networks {
			networkState.generation++
			networkState.phase = uplinkGatewayNetworkPending
			networkState.reason = uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending
			networkState.message = "gateway reconciliation is pending"
		}
	}
	c.mutex.Unlock()

	if uplinkState == nil {
		return nil
	}
	// Wait for an already-started publish before clearing lastCondition. The
	// caller deletes the UplinkState only after this method returns, so no old
	// publish can race with recreation of the node-local state.
	uplinkState.conditionMutex.Lock()
	uplinkState.lastCondition = nil
	uplinkState.conditionMutex.Unlock()

	var errs []error
	if err := c.publishGatewayCondition(uplinkName); err != nil && !isUplinkStateNotFound(err) {
		errs = append(errs, err)
	}

	uplinkState.operationMutex.Lock()
	c.mutex.Lock()
	lifecycles := make(map[string]uplinkGatewayNetworkLifecycle, len(uplinkState.networks))
	if c.uplinks[uplinkName] == uplinkState {
		for networkName, networkState := range uplinkState.networks {
			if networkState.lifecycle != nil {
				lifecycles[networkName] = networkState.lifecycle
			}
		}
	}
	c.mutex.Unlock()
	for _, networkName := range sortedLifecycleKeys(lifecycles) {
		if err := lifecycles[networkName].withdrawUplinkGateway(); err != nil {
			c.setNetworkFailure(
				uplinkState,
				networkName,
				uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed,
				err,
			)
			errs = append(errs, fmt.Errorf("failed to withdraw gateway for network %s: %w", networkName, err))
		}
	}
	uplinkState.operationMutex.Unlock()

	if err := c.publishGatewayCondition(uplinkName); err != nil && !isUplinkStateNotFound(err) {
		errs = append(errs, err)
	}
	return utilerrors.Join(errs...)
}

// ReconcileGatewayState consumes discovery's freshly computed status. A
// configuration equal to the active one is deletion recovery and only needs
// its condition republished. A new lifecycle or changed configuration must
// rebuild every registered gateway before GatewayReady can become true.
func (c *UplinkGatewayController) ReconcileGatewayState(state *uplinkv1alpha1.UplinkState) error {
	uplinkName, nodeName := uplinkutil.StateIdentity(state)
	if uplinkName == "" || nodeName != c.nodeName {
		return nil
	}

	resolved, err := c.resolvedGateway(state)
	if err != nil {
		invalidateErr := c.InvalidateGatewayState(uplinkName)
		return utilerrors.Join(err, invalidateErr)
	}
	configuration := resolvedUplinkGatewayConfiguration(resolved)

	c.mutex.Lock()
	uplinkState := c.uplinks[uplinkName]
	if uplinkState == nil {
		uplinkState = c.newUplinkGatewayStateLocked(uplinkName)
		c.uplinks[uplinkName] = uplinkState
	}
	if !uplinkState.invalidated && uplinkState.configuration == configuration {
		c.mutex.Unlock()
		return c.RepublishGatewayCondition(uplinkName)
	}
	uplinkState.invalidated = true
	for _, networkState := range uplinkState.networks {
		networkState.generation++
		networkState.phase = uplinkGatewayNetworkPending
		networkState.reason = uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending
		networkState.message = "gateway reconciliation is pending"
	}
	c.mutex.Unlock()

	// A changed configuration must not merge against readiness from the old
	// programming while the new gateway is being installed.
	uplinkState.conditionMutex.Lock()
	uplinkState.lastCondition = nil
	uplinkState.conditionMutex.Unlock()

	var errs []error
	if err := c.publishGatewayCondition(uplinkName); err != nil && !isUplinkStateNotFound(err) {
		errs = append(errs, err)
	}

	uplinkState.operationMutex.Lock()
	c.mutex.Lock()
	lifecycles := make(map[string]uplinkGatewayNetworkLifecycle, len(uplinkState.networks))
	if c.uplinks[uplinkName] == uplinkState {
		for networkName, networkState := range uplinkState.networks {
			if networkState.lifecycle != nil {
				lifecycles[networkName] = networkState.lifecycle
			}
		}
	}
	c.mutex.Unlock()

	programmingFailed := false
	for _, networkName := range sortedLifecycleKeys(lifecycles) {
		reconcileErr := lifecycles[networkName].reconcileUplinkGateway(resolved)
		if reconcileErr == nil {
			c.setNetworkReady(uplinkState, networkName, configuration)
			continue
		}
		c.setNetworkFailure(
			uplinkState,
			networkName,
			uplinkGatewayFailureReason(reconcileErr),
			reconcileErr,
		)
		programmingFailed = true
		errs = append(errs, fmt.Errorf("failed to reconcile gateway for network %s: %w", networkName, reconcileErr))
	}

	c.mutex.Lock()
	if !programmingFailed && c.uplinks[uplinkName] == uplinkState {
		uplinkState.configuration = configuration
		uplinkState.resolved = resolved
		uplinkState.invalidated = false
	}
	c.mutex.Unlock()
	uplinkState.operationMutex.Unlock()

	if statusErr := c.publishGatewayCondition(uplinkName); statusErr != nil {
		errs = append(errs, statusErr)
	}
	return utilerrors.Join(errs...)
}

func (c *UplinkGatewayController) resolvedGateway(
	state *uplinkv1alpha1.UplinkState,
) (*resolvedUplinkGateway, error) {
	conditionType := uplinkv1alpha1.UplinkStateConditionResolved
	requireResolvedBridge := config.IsModeDPU() || config.IsModeFull()
	if config.IsModeDPUHost() {
		conditionType = uplinkv1alpha1.UplinkStateConditionHostDataReady
	}
	condition := meta.FindStatusCondition(state.Status.Conditions, conditionType)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		return nil, fmt.Errorf("uplink state %s is not ready for gateway programming", state.Name)
	}
	uplinkName, nodeName := uplinkutil.StateIdentity(state)
	return resolvedUplinkGatewayFromState(state, uplinkName, nodeName, requireResolvedBridge)
}

func resolvedUplinkGatewayConfiguration(resolved *resolvedUplinkGateway) string {
	ipAddresses := make([]string, 0, len(resolved.ipAddresses))
	for _, ipAddress := range resolved.ipAddresses {
		ipAddresses = append(ipAddresses, ipAddress.String())
	}
	defaultGateways := make([]string, 0, len(resolved.defaultGateways))
	for _, defaultGateway := range resolved.defaultGateways {
		defaultGateways = append(defaultGateways, defaultGateway.String())
	}
	sort.Strings(ipAddresses)
	sort.Strings(defaultGateways)
	return strings.Join([]string{
		resolved.hostInterfaceName,
		resolved.bridgeName,
		resolved.macAddress.String(),
		strings.Join(ipAddresses, ","),
		strings.Join(defaultGateways, ","),
	}, "|")
}

func (c *UplinkGatewayController) setNetworkReady(
	uplinkState *uplinkGatewayState,
	networkName, configuration string,
) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	for _, current := range c.uplinks {
		if current != uplinkState {
			continue
		}
		if networkState := current.networks[networkName]; networkState != nil {
			networkState.phase = uplinkGatewayNetworkReady
			networkState.reason = uplinkv1alpha1.UplinkStateReasonGatewayConfigured
			networkState.message = ""
			networkState.configuration = configuration
		}
		return
	}
}

func (c *UplinkGatewayController) setNetworkFailure(
	uplinkState *uplinkGatewayState,
	networkName, reason string,
	err error,
) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	for _, current := range c.uplinks {
		if current != uplinkState {
			continue
		}
		if networkState := current.networks[networkName]; networkState != nil {
			networkState.phase = uplinkGatewayNetworkFailed
			networkState.reason = reason
			networkState.message = err.Error()
		}
		return
	}
}

func sortedLifecycleKeys(values map[string]uplinkGatewayNetworkLifecycle) []string {
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

// DeleteGatewayState forgets readiness and configuration for an Uplink that no
// longer exists. It retains only the operation lock and names of networks that
// still owe cleanup, so a later Uplink with the same name starts with fresh
// readiness without racing the retiring lifecycle.
func (c *UplinkGatewayController) DeleteGatewayState(uplinkName string) {
	c.mutex.Lock()
	uplinkState := c.uplinks[uplinkName]
	delete(c.uplinks, uplinkName)
	if uplinkState != nil && len(uplinkState.networks) > 0 {
		retired := c.retiredNetworks[uplinkName]
		if retired == nil {
			retired = map[string]struct{}{}
			c.retiredNetworks[uplinkName] = retired
		}
		for networkName := range uplinkState.networks {
			retired[networkName] = struct{}{}
		}
	} else if len(c.retiredNetworks[uplinkName]) == 0 {
		delete(c.operationMutexes, uplinkName)
	}
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

func (c *UplinkGatewayController) completeRetiredNetworkCleanup(uplinkName, networkName string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.completeRetiredNetworkCleanupLocked(uplinkName, networkName)
}

func (c *UplinkGatewayController) completeRetiredNetworkCleanupLocked(uplinkName, networkName string) {
	retired := c.retiredNetworks[uplinkName]
	if _, found := retired[networkName]; !found {
		return
	}
	delete(retired, networkName)
	if len(retired) != 0 {
		return
	}
	delete(c.retiredNetworks, uplinkName)
	if c.uplinks[uplinkName] == nil {
		delete(c.operationMutexes, uplinkName)
	}
}

func (c *UplinkGatewayController) newUplinkGatewayStateLocked(uplinkName string) *uplinkGatewayState {
	operationMutex := c.operationMutexes[uplinkName]
	if operationMutex == nil {
		operationMutex = &sync.Mutex{}
		c.operationMutexes[uplinkName] = operationMutex
	}
	return &uplinkGatewayState{
		operationMutex: operationMutex,
		networks:       map[string]*uplinkGatewayNetworkState{},
	}
}

func (c *UplinkGatewayController) markNetworkPending(
	network util.NetInfo,
) (*uplinkGatewayState, uint64, []string, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if _, retired := c.retiredNetworks[network.Uplink()][network.GetNetworkName()]; retired {
		return nil, 0, nil, fmt.Errorf(
			"gateway lifecycle for network %s on Uplink %s is awaiting cleanup",
			network.GetNetworkName(), network.Uplink())
	}

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
	return uplinkState, generation, affectedUplinks, nil
}

func (c *UplinkGatewayController) markNetworkPendingLocked(
	networkName, uplinkName string,
) (*uplinkGatewayState, uint64) {
	uplinkState := c.uplinks[uplinkName]
	if uplinkState == nil {
		uplinkState = c.newUplinkGatewayStateLocked(uplinkName)
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
	expectedUplinkState *uplinkGatewayState,
	generation uint64,
	reconcileErr error,
) error {
	c.mutex.Lock()
	uplinkState := c.uplinks[network.Uplink()]
	if uplinkState != expectedUplinkState {
		c.mutex.Unlock()
		return nil
	}
	networkState := uplinkState.networks[network.GetNetworkName()]
	if networkState != nil && networkState.generation == generation {
		if reconcileErr == nil {
			if !uplinkState.invalidated {
				networkState.phase = uplinkGatewayNetworkReady
				networkState.reason = uplinkv1alpha1.UplinkStateReasonGatewayConfigured
				networkState.message = ""
				networkState.configuration = uplinkState.configuration
			}
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
	expectedUplinkState *uplinkGatewayState,
	generation uint64,
	reconcileErr error,
) error {
	c.mutex.Lock()
	uplinkState := c.uplinks[network.Uplink()]
	if uplinkState != expectedUplinkState {
		if reconcileErr == nil {
			c.completeRetiredNetworkCleanupLocked(network.Uplink(), network.GetNetworkName())
		}
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

// RepublishGatewayCondition restores this controller's GatewayReady condition
// on an UplinkState recreated after an out-of-band deletion. It only restores
// a condition that was already published once: on a first creation the
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

// publishGatewayCondition writes the aggregate GatewayReady condition for all
// active CUDNs using the node-local UplinkState.
func (c *UplinkGatewayController) publishGatewayCondition(uplinkName string) error {
	if !c.publishStatus {
		return nil
	}

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
			FieldManager: uplinkGatewayFieldManager,
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
			Type:    uplinkv1alpha1.UplinkStateConditionGatewayReady,
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
	if incomplete == 0 && uplinkState.invalidated {
		return metav1.Condition{
			Type:    uplinkv1alpha1.UplinkStateConditionGatewayReady,
			Status:  metav1.ConditionFalse,
			Reason:  uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending,
			Message: "Uplink gateway lifecycle is awaiting fresh configuration",
		}, true
	}
	if incomplete == 0 {
		return metav1.Condition{
			Type:    uplinkv1alpha1.UplinkStateConditionGatewayReady,
			Status:  metav1.ConditionTrue,
			Reason:  uplinkv1alpha1.UplinkStateReasonGatewayConfigured,
			Message: fmt.Sprintf("Uplink gateway programming succeeded for %d active CUDN(s)", len(networkNames)),
		}, true
	}

	return metav1.Condition{
		Type:   uplinkv1alpha1.UplinkStateConditionGatewayReady,
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
