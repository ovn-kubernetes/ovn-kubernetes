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
	uplinkGatewayPendingMessage    = "gateway reconciliation is pending"
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
	configuration uplinkGatewayConfiguration
	lifecycle     uplinkGatewayNetworkLifecycle
	// cleanupLifecycle owns partial programming left by a failed Start, but is
	// not eligible for configuration reconciliation like an active lifecycle.
	cleanupLifecycle uplinkGatewayNetworkLifecycle
	// deleting makes terminal cleanup win over discovery and background
	// reconciliation that would otherwise advance this network's generation.
	deleting bool
	// startupAbandoned identifies a failed Start that has no programming or
	// lifecycle callback requiring cleanup from a retired cache entry.
	startupAbandoned bool
}

func (s *uplinkGatewayNetworkState) markPending() {
	s.generation++
	s.phase = uplinkGatewayNetworkPending
	s.reason = uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending
	s.message = uplinkGatewayPendingMessage
}

type uplinkGatewayState struct {
	operationMutex *sync.Mutex
	conditionMutex sync.Mutex
	// operationGeneration prevents an operation queued on operationMutex from
	// applying lifecycle changes after a newer reconciliation supersedes it.
	operationGeneration uint64
	networks            map[string]*uplinkGatewayNetworkState
	lastCondition       *metav1.Condition
	configuration       uplinkGatewayConfiguration
	resolved            *resolvedUplinkGateway
	invalidated         bool
}

type uplinkGatewayNetworkLifecycle interface {
	reconcileUplinkGateway(*resolvedUplinkGateway) error
	withdrawUplinkGateway() error
}

type uplinkGatewayNetworkLifecycleOperation struct {
	lifecycle   uplinkGatewayNetworkLifecycle
	cleanupOnly bool
}

// uplinkGatewayConfiguration is comparable so configuration identity does not
// depend on delimiters that are also valid in interface and bridge names.
type uplinkGatewayConfiguration struct {
	hostInterfaceName string
	bridgeName        string
	macAddress        string
	ipAddresses       string
	defaultGateways   string
}

type uplinkGatewayReconcileResult struct {
	uplinkState          *uplinkGatewayState
	programmingErr       error
	nonProgrammingErr    error
	programmingSucceeded bool
	cleanupRequired      bool
	previousLifecycle    uplinkGatewayNetworkLifecycle
}

func (r uplinkGatewayReconcileResult) err() error {
	return utilerrors.Join(r.programmingErr, r.nonProgrammingErr)
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
	// operationMutexes outlive the corresponding Uplink cache entry while
	// network controllers from that lifecycle still need to clean up. This
	// preserves shared-Uplink serialization after DeleteGatewayState.
	operationMutexes map[string]*sync.Mutex
	retiredNetworks  map[string]map[string]uplinkGatewayNetworkLifecycle
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
		operationMutexes:    map[string]*sync.Mutex{},
		retiredNetworks:     map[string]map[string]uplinkGatewayNetworkLifecycle{},
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
	if err := c.cleanupRetiredNetwork(network, nil); err != nil {
		return err
	}
	_, _, affectedUplinks, _, err := c.markNetworkPending(network)
	if err != nil {
		return err
	}
	return c.publishGatewayConditions(affectedUplinks)
}

// ReconcileNetwork serializes gateway programming for networks sharing an
// Uplink and folds the result into the aggregate GatewayReady condition.
func (c *UplinkGatewayController) ReconcileNetwork(network util.NetInfo, reconcile func() error) error {
	return c.reconcileNetwork(network, func(*resolvedUplinkGateway) error {
		return reconcile()
	}, false).err()
}

func (c *UplinkGatewayController) reconcileNetwork(
	network util.NetInfo,
	reconcile func(*resolvedUplinkGateway) error,
	replaceExisting bool,
) uplinkGatewayReconcileResult {
	if network.Uplink() == "" {
		reconcileErr := reconcile(nil)
		return uplinkGatewayReconcileResult{
			programmingErr:       reconcileErr,
			programmingSucceeded: reconcileErr == nil,
			cleanupRequired:      reconcileErr != nil,
		}
	}

	uplinkState, generation, affectedUplinks, previousLifecycle, err := c.markNetworkPending(network)
	if err != nil {
		return uplinkGatewayReconcileResult{nonProgrammingErr: err}
	}
	if err := c.publishGatewayConditions(affectedUplinks); err != nil && !isUplinkStateNotFound(err) {
		return uplinkGatewayReconcileResult{
			uplinkState:       uplinkState,
			nonProgrammingErr: err,
			previousLifecycle: previousLifecycle,
		}
	}

	uplinkState.operationMutex.Lock()
	c.mutex.Lock()
	networkState := uplinkState.networks[network.GetNetworkName()]
	valid := c.uplinks[network.Uplink()] == uplinkState && !uplinkState.invalidated &&
		networkState != nil && networkState.generation == generation && !networkState.deleting
	resolved := uplinkState.resolved
	c.mutex.Unlock()
	if !valid {
		uplinkState.operationMutex.Unlock()
		return uplinkGatewayReconcileResult{
			uplinkState:       uplinkState,
			previousLifecycle: previousLifecycle,
		}
	}

	var reconcileErr error
	programmingAttempted := false
	if replaceExisting && previousLifecycle != nil {
		reconcileErr = previousLifecycle.withdrawUplinkGateway()
		if reconcileErr != nil {
			reconcileErr = fmt.Errorf(
				"failed to clean up previous gateway lifecycle for network %s: %w",
				network.GetNetworkName(),
				reconcileErr,
			)
		} else {
			c.completePreviousNetworkLifecycle(network, uplinkState)
			c.mutex.Lock()
			networkState = uplinkState.networks[network.GetNetworkName()]
			valid = c.uplinks[network.Uplink()] == uplinkState && !uplinkState.invalidated &&
				networkState != nil && networkState.generation == generation && !networkState.deleting
			resolved = uplinkState.resolved
			c.mutex.Unlock()
		}
	}
	if reconcileErr == nil && valid {
		programmingAttempted = true
		reconcileErr = reconcile(resolved)
	}
	uplinkState.operationMutex.Unlock()

	statusErr := c.completeNetworkReconcile(network, uplinkState, generation, reconcileErr)
	if isUplinkStateNotFound(statusErr) {
		statusErr = nil
	}
	return uplinkGatewayReconcileResult{
		uplinkState:          uplinkState,
		programmingErr:       reconcileErr,
		nonProgrammingErr:    statusErr,
		programmingSucceeded: programmingAttempted && reconcileErr == nil,
		cleanupRequired:      programmingAttempted && reconcileErr != nil,
		previousLifecycle:    previousLifecycle,
	}
}

// StartNetwork performs initial gateway programming and registers its
// lifecycle callbacks. A failed initial reconciliation may have partially
// programmed the gateway, so cleanup is serialized and attempted before the
// failed network controller is discarded.
func (c *UplinkGatewayController) StartNetwork(
	network util.NetInfo,
	lifecycle uplinkGatewayNetworkLifecycle,
	reconcile func() error,
) error {
	return c.startNetwork(network, lifecycle, func(*resolvedUplinkGateway) error {
		return reconcile()
	})
}

func (c *UplinkGatewayController) startNetwork(
	network util.NetInfo,
	lifecycle uplinkGatewayNetworkLifecycle,
	reconcile func(*resolvedUplinkGateway) error,
) error {
	if err := c.cleanupRetiredNetwork(network, lifecycle); err != nil {
		return err
	}
	result := c.reconcileNetwork(network, reconcile, true)
	reconcileErr := result.err()
	if reconcileErr == nil {
		return c.ActivateNetwork(network, lifecycle)
	}
	if result.programmingSucceeded {
		// The failed status update does not invalidate successful gateway
		// programming. Register its lifecycle so it continues to own cleanup and
		// configuration changes while the network manager retries Start.
		return utilerrors.Join(reconcileErr, c.ActivateNetwork(network, lifecycle))
	}
	if !result.cleanupRequired {
		if result.uplinkState != nil {
			c.abandonNetworkStartup(network, result.uplinkState, result.previousLifecycle)
		}
		return reconcileErr
	}

	cleanupErr := c.cleanupFailedNetworkStart(network, lifecycle)
	if cleanupErr != nil {
		cleanupErr = fmt.Errorf("failed to clean up gateway after initial programming failure: %w", cleanupErr)
	}
	return utilerrors.Join(reconcileErr, cleanupErr)
}

// cleanupFailedNetworkStart withdraws partial initial programming without
// removing the still-desired network from aggregate readiness. Successful
// cleanup releases any tombstone created by a concurrent UplinkState deletion;
// failed cleanup remains owned by the retained failed-start controller.
func (c *UplinkGatewayController) cleanupFailedNetworkStart(
	network util.NetInfo,
	lifecycle uplinkGatewayNetworkLifecycle,
) error {
	if network.Uplink() == "" {
		return lifecycle.withdrawUplinkGateway()
	}

	c.mutex.Lock()
	uplinkState := c.uplinks[network.Uplink()]
	trackedUplink := c.uplinkByNetworkName[network.GetNetworkName()]
	if uplinkState != nil && trackedUplink == network.Uplink() {
		if networkState := uplinkState.networks[network.GetNetworkName()]; networkState != nil {
			// ReconcileNetwork records classified programming failures before
			// cleanup. Preserve them so withdrawing partial programming does not
			// hide the failure from UplinkState consumers. Publication-only
			// failures remain pending because cleanup withdraws their programming.
			if networkState.phase != uplinkGatewayNetworkFailed {
				networkState.markPending()
			}
			generation := networkState.generation
			c.mutex.Unlock()

			publishErr := c.publishGatewayCondition(network.Uplink())
			if isUplinkStateNotFound(publishErr) {
				publishErr = nil
			}

			uplinkState.operationMutex.Lock()
			reconcileErr := lifecycle.withdrawUplinkGateway()
			uplinkState.operationMutex.Unlock()

			c.mutex.Lock()
			if current := c.uplinks[network.Uplink()]; current == uplinkState {
				if currentNetwork := current.networks[network.GetNetworkName()]; currentNetwork != nil &&
					currentNetwork.generation == generation && !currentNetwork.deleting {
					currentNetwork.lifecycle = nil
					currentNetwork.cleanupLifecycle = nil
					currentNetwork.startupAbandoned = reconcileErr == nil
					if reconcileErr != nil {
						currentNetwork.cleanupLifecycle = lifecycle
						if currentNetwork.phase != uplinkGatewayNetworkFailed {
							currentNetwork.phase = uplinkGatewayNetworkFailed
							currentNetwork.reason = uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed
							currentNetwork.message = reconcileErr.Error()
						}
					}
				}
			} else if reconcileErr == nil {
				c.completeRetiredNetworkCleanupLocked(network.Uplink(), network.GetNetworkName())
			} else if _, retired := c.retiredNetworks[network.Uplink()][network.GetNetworkName()]; retired {
				c.retiredNetworks[network.Uplink()][network.GetNetworkName()] = lifecycle
			}
			c.mutex.Unlock()

			statusErr := c.publishGatewayCondition(network.Uplink())
			if isUplinkStateNotFound(statusErr) {
				statusErr = nil
			}
			return utilerrors.Join(publishErr, reconcileErr, statusErr)
		}
	}
	operationMutex := c.operationMutexes[network.Uplink()]
	_, retired := c.retiredNetworks[network.Uplink()][network.GetNetworkName()]
	c.mutex.Unlock()

	if operationMutex != nil {
		operationMutex.Lock()
	}
	reconcileErr := lifecycle.withdrawUplinkGateway()
	if operationMutex != nil {
		operationMutex.Unlock()
	}
	if retired {
		c.mutex.Lock()
		if _, stillRetired := c.retiredNetworks[network.Uplink()][network.GetNetworkName()]; stillRetired {
			if reconcileErr == nil {
				c.completeRetiredNetworkCleanupLocked(network.Uplink(), network.GetNetworkName())
			} else {
				c.retiredNetworks[network.Uplink()][network.GetNetworkName()] = lifecycle
			}
		}
		c.mutex.Unlock()
	}
	return reconcileErr
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
	if networkState.deleting {
		c.mutex.Unlock()
		return c.failNetworkActivation(
			network,
			lifecycle,
			fmt.Errorf("gateway lifecycle for network %s is being deleted", network.GetNetworkName()),
		)
	}
	networkState.lifecycle = lifecycle
	networkState.cleanupLifecycle = nil
	invalidated := uplinkState.invalidated
	configurationChanged := networkState.configuration != uplinkState.configuration
	desiredConfiguration := uplinkState.configuration
	resolved := uplinkState.resolved
	c.mutex.Unlock()

	if !invalidated && (!configurationChanged || resolved == nil) {
		return nil
	}

	var reconcileErr, publishErr error
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
		publishErr = c.publishGatewayCondition(network.Uplink())
	}

	if !invalidated && reconcileErr == nil {
		if publishErr == nil || isUplinkStateNotFound(publishErr) {
			return nil
		}
		// Programming succeeded. Keep its callback registered so the retained
		// failed-start controller continues to own cleanup and configuration
		// changes until a replacement Start activates.
		return publishErr
	}
	cleanupSucceeded := reconcileErr == nil
	if !invalidated {
		cleanupErr := lifecycle.withdrawUplinkGateway()
		cleanupSucceeded = cleanupErr == nil
		if cleanupErr != nil {
			reconcileErr = utilerrors.Join(
				reconcileErr,
				fmt.Errorf("failed to clean up gateway after activation failure: %w", cleanupErr),
			)
		}
	}

	// Release lifecycle ownership only after cleanup. If cleanup failed, the
	// retained failed-start controller must remain able to retry it.
	c.mutex.Lock()
	if current := c.uplinks[network.Uplink()]; current == uplinkState {
		if currentNetwork := current.networks[network.GetNetworkName()]; currentNetwork != nil {
			if cleanupSucceeded {
				currentNetwork.lifecycle = nil
				currentNetwork.startupAbandoned = true
			}
		}
	} else if cleanupSucceeded {
		// DeleteGatewayState may have retired the network while activation was
		// finishing. Cleanup has completed, so release that tombstone here.
		c.completeRetiredNetworkCleanupLocked(network.Uplink(), network.GetNetworkName())
	}
	c.mutex.Unlock()
	if reconcileErr != nil {
		return reconcileErr
	}
	return fmt.Errorf("uplink %s gateway state was invalidated during network startup", network.Uplink())
}

func (c *UplinkGatewayController) abandonNetworkStartup(
	network util.NetInfo,
	expectedUplinkState *uplinkGatewayState,
	previousLifecycle uplinkGatewayNetworkLifecycle,
) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if current := c.uplinks[network.Uplink()]; current == expectedUplinkState {
		if networkState := current.networks[network.GetNetworkName()]; networkState != nil && !networkState.deleting {
			// The failing Start did not attach a lifecycle. Preserve a callback
			// retained by an earlier status-only failure and abandon only a startup
			// that truly has no programming owner.
			networkState.startupAbandoned = networkState.lifecycle == nil && networkState.cleanupLifecycle == nil
		}
		return
	}
	// State deletion may race the failed status publication. Release a
	// tombstone only when it cannot belong to a previously retained lifecycle.
	if previousLifecycle == nil {
		c.completeRetiredNetworkCleanupLocked(network.Uplink(), network.GetNetworkName())
	}
}

func (c *UplinkGatewayController) failNetworkActivation(
	network util.NetInfo,
	lifecycle uplinkGatewayNetworkLifecycle,
	activationErr error,
) error {
	cleanupErr := lifecycle.withdrawUplinkGateway()
	if cleanupErr == nil {
		// Cleanup completed before the failed controller is retained, so it does
		// not need to own a retired cache entry.
		c.completeRetiredNetworkCleanup(network.Uplink(), network.GetNetworkName())
		return activationErr
	}
	c.retainFailedActivationCleanup(network, lifecycle, cleanupErr)
	return utilerrors.Join(
		activationErr,
		fmt.Errorf("failed to clean up gateway after activation failure: %w", cleanupErr),
	)
}

func (c *UplinkGatewayController) retainFailedActivationCleanup(
	network util.NetInfo,
	lifecycle uplinkGatewayNetworkLifecycle,
	cleanupErr error,
) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if uplinkState := c.uplinks[network.Uplink()]; uplinkState != nil {
		if networkState := uplinkState.networks[network.GetNetworkName()]; networkState != nil {
			// The activation's lifecycle owns the most recent programming. Keep it
			// cleanup-only so configuration reconciliation cannot add more state
			// before the failed withdrawal succeeds.
			networkState.lifecycle = nil
			networkState.cleanupLifecycle = lifecycle
			networkState.startupAbandoned = false
			networkState.phase = uplinkGatewayNetworkFailed
			networkState.reason = uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed
			networkState.message = cleanupErr.Error()
			return
		}
	}

	retired := c.retiredNetworks[network.Uplink()]
	if retired == nil {
		retired = map[string]uplinkGatewayNetworkLifecycle{}
		c.retiredNetworks[network.Uplink()] = retired
	}
	retired[network.GetNetworkName()] = lifecycle
	if c.operationMutexes[network.Uplink()] == nil {
		c.operationMutexes[network.Uplink()] = &sync.Mutex{}
	}
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
			networkState.markPending()
			networkState.deleting = true
			networkState.startupAbandoned = false
			cleanup := reconcile
			if networkState.lifecycle != nil {
				cleanup = networkState.lifecycle.withdrawUplinkGateway
			} else if networkState.cleanupLifecycle != nil {
				cleanup = networkState.cleanupLifecycle.withdrawUplinkGateway
			}
			c.mutex.Unlock()

			publishErr := c.publishGatewayCondition(network.Uplink())
			if isUplinkStateNotFound(publishErr) {
				publishErr = nil
			}

			uplinkState.operationMutex.Lock()
			reconcileErr := cleanup()
			uplinkState.operationMutex.Unlock()

			statusErr := c.completeNetworkDelete(network, uplinkState, reconcileErr)
			if isUplinkStateNotFound(statusErr) {
				statusErr = nil
			}
			return utilerrors.Join(publishErr, reconcileErr, statusErr)
		}
	}
	operationMutex := c.operationMutexes[network.Uplink()]
	retiredLifecycle, retired := c.retiredNetworks[network.Uplink()][network.GetNetworkName()]
	c.mutex.Unlock()
	if retired && operationMutex != nil {
		operationMutex.Lock()
		cleanup := reconcile
		if retiredLifecycle != nil {
			cleanup = retiredLifecycle.withdrawUplinkGateway
		}
		reconcileErr := cleanup()
		operationMutex.Unlock()
		if reconcileErr == nil {
			c.completeRetiredNetworkCleanup(network.Uplink(), network.GetNetworkName())
		}
		statusErr := c.publishGatewayCondition(network.Uplink())
		if isUplinkStateNotFound(statusErr) {
			statusErr = nil
		}
		return utilerrors.Join(reconcileErr, statusErr)
	}
	// The network is not part of either an active or a retired lifecycle.
	// Cleanup is still idempotently required, but there is no cached operation
	// with which it can race and no cache entry should be recreated.
	reconcileErr := reconcile()
	statusErr := c.publishGatewayCondition(network.Uplink())
	if isUplinkStateNotFound(statusErr) {
		statusErr = nil
	}
	return utilerrors.Join(reconcileErr, statusErr)
}

// InvalidateGatewayState starts a new node-local gateway lifecycle for an
// Uplink that still exists but no longer selects this node. Preserve the
// active network set so aggregate readiness cannot become true until every
// affected CUDN has reconciled again.
func (c *UplinkGatewayController) InvalidateGatewayState(uplinkName string) error {
	return c.invalidateGatewayState(uplinkName, nil)
}

// InvalidateGatewayStateIfCurrent invalidates an Uplink lifecycle only while
// the discovery inputs that requested it are still current. The callback is
// checked again under the operation lock so an older discovery result cannot
// withdraw programming installed by a newer reconciliation.
func (c *UplinkGatewayController) InvalidateGatewayStateIfCurrent(
	uplinkName string,
	isCurrent func() bool,
) error {
	return c.invalidateGatewayState(uplinkName, isCurrent)
}

func (c *UplinkGatewayController) invalidateGatewayState(
	uplinkName string,
	isCurrent func() bool,
) error {
	if !gatewayStateInputsAreCurrent(isCurrent) {
		return nil
	}

	c.mutex.Lock()
	if !gatewayStateInputsAreCurrent(isCurrent) {
		c.mutex.Unlock()
		return nil
	}
	uplinkState := c.uplinks[uplinkName]
	if uplinkState == nil {
		c.mutex.Unlock()
		return nil
	}
	uplinkState.operationGeneration++
	operationGeneration := uplinkState.operationGeneration
	uplinkState.invalidated = true
	uplinkState.configuration = uplinkGatewayConfiguration{}
	uplinkState.resolved = nil
	for _, networkState := range uplinkState.networks {
		if networkState.deleting {
			continue
		}
		networkState.markPending()
	}
	c.mutex.Unlock()

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
	if !gatewayStateInputsAreCurrent(isCurrent) {
		uplinkState.operationMutex.Unlock()
		return utilerrors.Join(errs...)
	}
	c.mutex.Lock()
	valid := c.uplinks[uplinkName] == uplinkState &&
		uplinkState.operationGeneration == operationGeneration
	c.mutex.Unlock()
	if !valid {
		uplinkState.operationMutex.Unlock()
		return utilerrors.Join(errs...)
	}
	lifecycles := c.networkLifecycleOperations(uplinkName, uplinkState)
	for _, networkName := range sortedLifecycleOperationKeys(lifecycles) {
		operation := lifecycles[networkName]
		if err := operation.lifecycle.withdrawUplinkGateway(); err != nil {
			c.setNetworkFailure(
				uplinkState,
				networkName,
				uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed,
				err,
			)
			errs = append(errs, fmt.Errorf("failed to withdraw gateway for network %s: %w", networkName, err))
			continue
		}
		if operation.cleanupOnly {
			c.completeCleanupOnlyLifecycle(uplinkName, uplinkState, networkName)
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
	return c.reconcileGatewayState(state, nil)
}

// ReconcileGatewayStateIfCurrent reconciles a discovered UplinkState only
// while the Uplink and node inputs used to discover it remain current.
func (c *UplinkGatewayController) ReconcileGatewayStateIfCurrent(
	state *uplinkv1alpha1.UplinkState,
	isCurrent func() bool,
) error {
	return c.reconcileGatewayState(state, isCurrent)
}

func (c *UplinkGatewayController) reconcileGatewayState(
	state *uplinkv1alpha1.UplinkState,
	isCurrent func() bool,
) error {
	uplinkName, nodeName := uplinkutil.StateIdentity(state)
	if uplinkName == "" || nodeName != c.nodeName {
		return nil
	}
	if !gatewayStateInputsAreCurrent(isCurrent) {
		return nil
	}

	resolved, err := c.resolvedGateway(state)
	if err != nil {
		invalidateErr := c.invalidateGatewayState(uplinkName, isCurrent)
		return utilerrors.Join(err, invalidateErr)
	}
	configuration := resolvedUplinkGatewayConfiguration(resolved)

	c.mutex.Lock()
	if !gatewayStateInputsAreCurrent(isCurrent) {
		c.mutex.Unlock()
		return nil
	}
	uplinkState := c.uplinks[uplinkName]
	if uplinkState == nil {
		uplinkState = c.newUplinkGatewayStateLocked(uplinkName)
		c.uplinks[uplinkName] = uplinkState
	}
	if !uplinkState.invalidated && uplinkState.configuration == configuration {
		c.mutex.Unlock()
		return c.RepublishGatewayCondition(uplinkName)
	}
	uplinkState.operationGeneration++
	operationGeneration := uplinkState.operationGeneration
	uplinkState.invalidated = true
	for _, networkState := range uplinkState.networks {
		if networkState.deleting {
			continue
		}
		networkState.markPending()
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
	if !gatewayStateInputsAreCurrent(isCurrent) {
		uplinkState.operationMutex.Unlock()
		return utilerrors.Join(errs...)
	}
	c.mutex.Lock()
	valid := c.uplinks[uplinkName] == uplinkState &&
		uplinkState.operationGeneration == operationGeneration
	c.mutex.Unlock()
	if !valid {
		uplinkState.operationMutex.Unlock()
		return utilerrors.Join(errs...)
	}
	lifecycles := c.networkLifecycleOperations(uplinkName, uplinkState)

	programmingFailed := false
	for _, networkName := range sortedLifecycleOperationKeys(lifecycles) {
		operation := lifecycles[networkName]
		if operation.cleanupOnly {
			cleanupErr := operation.lifecycle.withdrawUplinkGateway()
			if cleanupErr != nil {
				c.setNetworkFailure(
					uplinkState,
					networkName,
					uplinkGatewayFailureReason(cleanupErr),
					cleanupErr,
				)
				programmingFailed = true
				errs = append(errs, fmt.Errorf(
					"failed to clean up gateway for network %s before configuration reconciliation: %w",
					networkName,
					cleanupErr,
				))
				continue
			}
			c.completeCleanupOnlyLifecycle(uplinkName, uplinkState, networkName)
			continue
		}

		reconcileErr := operation.lifecycle.reconcileUplinkGateway(resolved)
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
	if !programmingFailed && c.uplinks[uplinkName] == uplinkState &&
		uplinkState.operationGeneration == operationGeneration {
		uplinkState.configuration = configuration
		uplinkState.resolved = resolved
		uplinkState.invalidated = false
	}
	c.mutex.Unlock()
	uplinkState.operationMutex.Unlock()

	if statusErr := c.publishGatewayCondition(uplinkName); statusErr != nil && !isUplinkStateNotFound(statusErr) {
		errs = append(errs, statusErr)
	}
	return utilerrors.Join(errs...)
}

func gatewayStateInputsAreCurrent(isCurrent func() bool) bool {
	return isCurrent == nil || isCurrent()
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

func resolvedUplinkGatewayConfiguration(resolved *resolvedUplinkGateway) uplinkGatewayConfiguration {
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
	bridgeName := resolved.bridgeName
	if config.IsModeDPUHost() {
		// The bridge is discovered and owned by the DPU. Host-side gateway
		// programming does not consume it and must not restart when it changes.
		bridgeName = ""
	}
	return uplinkGatewayConfiguration{
		hostInterfaceName: resolved.hostInterfaceName,
		bridgeName:        bridgeName,
		macAddress:        resolved.macAddress.String(),
		ipAddresses:       strings.Join(ipAddresses, ","),
		defaultGateways:   strings.Join(defaultGateways, ","),
	}
}

func (c *UplinkGatewayController) setNetworkReady(
	uplinkState *uplinkGatewayState,
	networkName string,
	configuration uplinkGatewayConfiguration,
) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	for _, current := range c.uplinks {
		if current != uplinkState {
			continue
		}
		if networkState := current.networks[networkName]; networkState != nil && !networkState.deleting {
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
		if networkState := current.networks[networkName]; networkState != nil && !networkState.deleting {
			networkState.phase = uplinkGatewayNetworkFailed
			networkState.reason = reason
			networkState.message = err.Error()
		}
		return
	}
}

func (c *UplinkGatewayController) networkLifecycleOperations(
	uplinkName string,
	expectedUplinkState *uplinkGatewayState,
) map[string]uplinkGatewayNetworkLifecycleOperation {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	operations := map[string]uplinkGatewayNetworkLifecycleOperation{}
	if c.uplinks[uplinkName] != expectedUplinkState {
		return operations
	}
	for networkName, networkState := range expectedUplinkState.networks {
		if networkState.deleting {
			continue
		}
		if networkState.lifecycle != nil {
			operations[networkName] = uplinkGatewayNetworkLifecycleOperation{
				lifecycle: networkState.lifecycle,
			}
			continue
		}
		if networkState.cleanupLifecycle != nil {
			operations[networkName] = uplinkGatewayNetworkLifecycleOperation{
				lifecycle:   networkState.cleanupLifecycle,
				cleanupOnly: true,
			}
		}
	}
	return operations
}

func (c *UplinkGatewayController) completeCleanupOnlyLifecycle(
	uplinkName string,
	expectedUplinkState *uplinkGatewayState,
	networkName string,
) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if current := c.uplinks[uplinkName]; current == expectedUplinkState {
		if networkState := current.networks[networkName]; networkState != nil && !networkState.deleting {
			networkState.cleanupLifecycle = nil
			networkState.startupAbandoned = networkState.lifecycle == nil
		}
		return
	}
	c.completeRetiredNetworkCleanupLocked(uplinkName, networkName)
}

// completePreviousNetworkLifecycle releases the callback that owned the
// preceding compatible Start attempt after its programming has been
// successfully withdrawn. The replacement callback is not invoked until this
// transition completes, so at most one generation owns dataplane resources.
func (c *UplinkGatewayController) completePreviousNetworkLifecycle(
	network util.NetInfo,
	expectedUplinkState *uplinkGatewayState,
) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if current := c.uplinks[network.Uplink()]; current == expectedUplinkState {
		if networkState := current.networks[network.GetNetworkName()]; networkState != nil {
			networkState.lifecycle = nil
			networkState.cleanupLifecycle = nil
		}
		return
	}
	c.completeRetiredNetworkCleanupLocked(network.Uplink(), network.GetNetworkName())
}

func sortedLifecycleOperationKeys(values map[string]uplinkGatewayNetworkLifecycleOperation) []string {
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
// longer exists. It retains the operation lock and cleanup owners for networks
// that still owe cleanup, so a later Uplink with the same name starts with
// fresh readiness without racing the retiring lifecycle.
func (c *UplinkGatewayController) DeleteGatewayState(uplinkName string) {
	c.mutex.Lock()
	uplinkState := c.uplinks[uplinkName]
	delete(c.uplinks, uplinkName)
	if uplinkState != nil {
		for networkName, networkState := range uplinkState.networks {
			if networkState.startupAbandoned {
				continue
			}
			retired := c.retiredNetworks[uplinkName]
			if retired == nil {
				retired = map[string]uplinkGatewayNetworkLifecycle{}
				c.retiredNetworks[uplinkName] = retired
			}
			retired[networkName] = networkState.lifecycle
			if retired[networkName] == nil {
				retired[networkName] = networkState.cleanupLifecycle
			}
		}
	}
	if len(c.retiredNetworks[uplinkName]) == 0 {
		delete(c.retiredNetworks, uplinkName)
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

// cleanupRetiredNetwork completes cleanup from an Uplink lifecycle that was
// removed while a network controller still owned gateway programming. The
// replacement lifecycle is only used when retirement raced before the old
// callback could be registered.
func (c *UplinkGatewayController) cleanupRetiredNetwork(
	network util.NetInfo,
	replacement uplinkGatewayNetworkLifecycle,
) error {
	if network.Uplink() == "" {
		return nil
	}

	c.mutex.Lock()
	retired := c.retiredNetworks[network.Uplink()]
	_, found := retired[network.GetNetworkName()]
	operationMutex := c.operationMutexes[network.Uplink()]
	if found && operationMutex == nil {
		operationMutex = &sync.Mutex{}
		c.operationMutexes[network.Uplink()] = operationMutex
	}
	c.mutex.Unlock()
	if !found {
		return nil
	}

	operationMutex.Lock()
	defer operationMutex.Unlock()

	c.mutex.Lock()
	retired = c.retiredNetworks[network.Uplink()]
	lifecycle, found := retired[network.GetNetworkName()]
	if found && lifecycle == nil && replacement != nil {
		lifecycle = replacement
		retired[network.GetNetworkName()] = lifecycle
	}
	c.mutex.Unlock()
	if !found {
		return nil
	}
	if lifecycle == nil {
		return fmt.Errorf(
			"gateway lifecycle for network %s on Uplink %s is awaiting cleanup",
			network.GetNetworkName(), network.Uplink())
	}

	if err := lifecycle.withdrawUplinkGateway(); err != nil {
		return fmt.Errorf(
			"failed to clean up retired gateway lifecycle for network %s on Uplink %s: %w",
			network.GetNetworkName(), network.Uplink(), err)
	}
	c.completeRetiredNetworkCleanup(network.Uplink(), network.GetNetworkName())
	return nil
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
) (*uplinkGatewayState, uint64, []string, uplinkGatewayNetworkLifecycle, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if _, retired := c.retiredNetworks[network.Uplink()][network.GetNetworkName()]; retired {
		return nil, 0, nil, nil, fmt.Errorf(
			"gateway lifecycle for network %s on Uplink %s is awaiting cleanup",
			network.GetNetworkName(), network.Uplink())
	}
	if uplinkState := c.uplinks[network.Uplink()]; uplinkState != nil {
		if networkState := uplinkState.networks[network.GetNetworkName()]; networkState != nil && networkState.deleting {
			return nil, 0, nil, nil, fmt.Errorf(
				"gateway lifecycle for network %s on Uplink %s is awaiting cleanup",
				network.GetNetworkName(), network.Uplink())
		}
	}

	previousUplink := c.uplinkByNetworkName[network.GetNetworkName()]
	if previousUplink != "" && previousUplink != network.Uplink() {
		if previousUplinkState := c.uplinks[previousUplink]; previousUplinkState != nil {
			delete(previousUplinkState.networks, network.GetNetworkName())
		}
	}

	uplinkState, generation := c.markNetworkPendingLocked(network.GetNetworkName(), network.Uplink())
	previousLifecycle := uplinkState.networks[network.GetNetworkName()].lifecycle
	if previousLifecycle == nil {
		previousLifecycle = uplinkState.networks[network.GetNetworkName()].cleanupLifecycle
	}
	affectedUplinks := []string{network.Uplink()}
	if previousUplink != "" && previousUplink != network.Uplink() {
		affectedUplinks = append(affectedUplinks, previousUplink)
	}
	return uplinkState, generation, affectedUplinks, previousLifecycle, nil
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
	networkState.markPending()
	networkState.startupAbandoned = false
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
	if networkState != nil && networkState.generation == generation && !networkState.deleting {
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
	if networkState != nil && networkState.deleting {
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

	desiredCondition, found := c.gatewayCondition(uplinkName, uplinkState)
	if !found {
		return nil
	}
	stateName := uplinkutil.StateName(uplinkName, c.nodeName)
	state, err := uplinkutil.GetState(c.uplinkStateLister, uplinkName, c.nodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			existingConditions := []metav1.Condition(nil)
			if uplinkState.lastCondition != nil {
				existingConditions = []metav1.Condition{*uplinkState.lastCondition}
			}
			condition, _ := util.MergeStatusCondition(existingConditions, desiredCondition)
			uplinkState.lastCondition = condition.DeepCopy()
		}
		return fmt.Errorf("failed to get UplinkState %s from cache: %w", stateName, err)
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
	// Cache the desired condition before applying it. If deletion races the
	// request, recreation can republish the condition without another gateway
	// state transition.
	uplinkState.lastCondition = condition.DeepCopy()
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
	retiredNetworks := c.retiredNetworks[uplinkName]
	if len(uplinkState.networks) == 0 && len(retiredNetworks) == 0 {
		return metav1.Condition{
			Type:    c.conditionType,
			Status:  metav1.ConditionTrue,
			Reason:  uplinkv1alpha1.UplinkStateReasonGatewayConfigured,
			Message: "No active CUDNs require Uplink gateway programming",
		}, true
	}

	networkNameSet := make(map[string]struct{}, len(uplinkState.networks)+len(retiredNetworks))
	for networkName := range uplinkState.networks {
		networkNameSet[networkName] = struct{}{}
	}
	for networkName := range retiredNetworks {
		networkNameSet[networkName] = struct{}{}
	}
	networkNames := make([]string, 0, len(networkNameSet))
	for networkName := range networkNameSet {
		networkNames = append(networkNames, networkName)
	}
	sort.Strings(networkNames)

	failureReasons := map[string]struct{}{}
	examples := make([]string, 0, maxGatewayConditionExamples)
	incomplete := 0
	for _, networkName := range networkNames {
		networkState := uplinkState.networks[networkName]
		_, retired := retiredNetworks[networkName]
		if networkState != nil && networkState.phase == uplinkGatewayNetworkReady && !retired {
			continue
		}
		incomplete++
		reason := uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending
		message := "retired gateway lifecycle is awaiting cleanup"
		if networkState != nil && !retired {
			reason = networkState.reason
			message = networkState.message
		}
		failureReasons[reason] = struct{}{}
		if len(examples) < maxGatewayConditionExamples {
			example := fmt.Sprintf("%s=%s", networkName, reason)
			if message != "" {
				example += ": " + truncateGatewayConditionError(message)
			}
			examples = append(examples, example)
		}
	}
	if incomplete == 0 && uplinkState.invalidated {
		return metav1.Condition{
			Type:    c.conditionType,
			Status:  metav1.ConditionFalse,
			Reason:  uplinkv1alpha1.UplinkStateReasonGatewayConfigurationPending,
			Message: "Uplink gateway lifecycle is awaiting fresh configuration",
		}, true
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
