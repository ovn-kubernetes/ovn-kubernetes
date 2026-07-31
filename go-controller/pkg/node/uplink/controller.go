// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package uplink

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"

	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/applyconfiguration/uplink/v1alpha1"
	uplinkclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	nodeutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/util"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

const (
	fieldManager         = "ovnkube-node-uplink-controller"
	dpuFieldManager      = "ovnkube-node-uplink-controller-dpu"
	dpuHostFieldManager  = "ovnkube-node-uplink-controller-dpu-host"
	ovsIntegrationBridge = "br-int"
)

type hostInterfaceState struct {
	macAddress      net.HardwareAddr
	ipAddresses     []*net.IPNet
	defaultGateways []net.IP
}

type hostInterfaceDiscoverer interface {
	Discover(hostInterfaceName string) (*hostInterfaceState, error)
}

type ovsBridgeResolver interface {
	// Resolve finds the OVS bridge for a host interface in Full mode.
	Resolve(hostInterfaceName string) (string, error)
	// ResolveByHostMAC finds the OVS bridge by its host-side MAC in DPU mode.
	ResolveByHostMAC(hostMAC net.HardwareAddr, nodeName string) (string, error)
	// BridgeUplink returns the physical uplink port attached to an OVS bridge.
	BridgeUplink(bridgeName string) (string, error)
}

type discoveryError struct {
	reason string
	err    error
}

func (e *discoveryError) Error() string {
	return e.err.Error()
}

func newDiscoveryError(reason string, err error) error {
	return &discoveryError{reason: reason, err: err}
}

// Controller publishes UplinkState discovery status for this node.
type Controller struct {
	nodeName string

	uplinkClient      uplinkclientset.Interface
	uplinkLister      uplinklisters.UplinkLister
	uplinkStateLister uplinklisters.UplinkStateLister
	nodeLister        corelisters.NodeLister

	hostDiscoverer hostInterfaceDiscoverer
	bridgeResolver ovsBridgeResolver

	uplinkController      controllerutil.Controller
	uplinkStateController controllerutil.Controller
	nodeController        controllerutil.Controller
}

// NewController creates an ovnkube-node Uplink controller.
func NewController(nodeName string, wf factory.NodeWatchFactory, ovnClient *util.OVNNodeClientset, ovsClient libovsdbclient.Client,
) *Controller {
	c := &Controller{
		nodeName:          nodeName,
		uplinkClient:      ovnClient.UplinkClient,
		uplinkLister:      wf.UplinkInformer().Lister(),
		uplinkStateLister: wf.UplinkStateInformer().Lister(),
		nodeLister:        wf.NodeCoreInformer().Lister(),
		hostDiscoverer:    netlinkHostInterfaceDiscoverer{},
		bridgeResolver:    defaultOVSBridgeResolver{ovsClient: ovsClient},
	}

	uplinkCfg := &controllerutil.ControllerConfig[uplinkv1alpha1.Uplink]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.UplinkInformer().Informer(),
		Lister:         c.uplinkLister.List,
		Reconcile:      c.reconcileUplink,
		ObjNeedsUpdate: uplinkNeedsUpdate,
		Threadiness:    1,
	}
	c.uplinkController = controllerutil.NewController(
		"ovnkube-node-uplink-controller",
		uplinkCfg,
	)

	uplinkStateCfg := &controllerutil.ControllerConfig[uplinkv1alpha1.UplinkState]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.UplinkStateInformer().Informer(),
		Lister:         c.uplinkStateLister.List,
		Reconcile:      c.reconcileUplinkState,
		ObjNeedsUpdate: uplinkStateNeedsUpdate,
		Threadiness:    1,
	}
	c.uplinkStateController = controllerutil.NewController(
		"ovnkube-node-uplink-state-controller",
		uplinkStateCfg,
	)

	nodeCfg := &controllerutil.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.NodeCoreInformer().Informer(),
		Lister:         c.nodeLister.List,
		Reconcile:      c.reconcileNode,
		ObjNeedsUpdate: c.nodeNeedsUpdate,
		Threadiness:    1,
	}
	c.nodeController = controllerutil.NewController(
		"ovnkube-node-uplink-node-controller",
		nodeCfg,
	)

	return c
}

func (c *Controller) Start() error {
	err := controllerutil.Start(
		c.uplinkController,
		c.uplinkStateController,
		c.nodeController,
	)
	if err != nil {
		return err
	}
	klog.Infof("OVN-Kubernetes node Uplink controller started")
	return nil
}

func (c *Controller) Stop() {
	controllerutil.Stop(
		c.uplinkController,
		c.uplinkStateController,
		c.nodeController,
	)
}

func (c *Controller) reconcileUplink(key string) error {
	uplink, err := c.uplinkLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.deleteUplinkState(uplinkutil.StateName(key, c.nodeName))
		}
		return fmt.Errorf("failed to get Uplink %s: %w", key, err)
	}

	node, err := c.nodeLister.Get(c.nodeName)
	if err != nil {
		return fmt.Errorf("failed to get local node %s: %w", c.nodeName, err)
	}

	nodeConfig, nodeConfigErr := selectedNodeConfigForNode(uplink, node)
	if nodeConfigErr == nil && nodeConfig == nil {
		return c.deleteUplinkState(uplinkutil.StateName(uplink.Name, c.nodeName))
	}

	state, err := c.ensureUplinkState(uplink, nodeConfig, nodeConfigErr)
	if err != nil {
		return err
	}
	if state == nil {
		return nil
	}
	if nodeConfigErr != nil {
		return c.updateUplinkStateStatus(
			state,
			string(state.Status.HostInterfaceName),
			nil,
			"",
			metav1.ConditionFalse,
			discoveryReason(nodeConfigErr),
			nodeConfigErr.Error(),
		)
	}
	// Creation also produces an UplinkState watch event, but existing states
	// need explicit rediscovery when the selected Uplink config changes.
	c.uplinkStateController.Reconcile(state.Name)
	return nil
}

func (c *Controller) reconcileNode(key string) error {
	// Delete events bypass nodeNeedsUpdate, so filter remote node deletes here.
	if key != c.nodeName {
		return nil
	}
	c.uplinkController.ReconcileAll()
	return nil
}

func (c *Controller) reconcileUplinkState(key string) error {
	state, err := c.uplinkStateLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get UplinkState %s: %w", key, err)
	}
	uplinkName, _ := uplinkutil.StateIdentity(state)

	uplink, err := c.uplinkLister.Get(uplinkName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get Uplink %s: %w", uplinkName, err)
	}

	node, err := c.nodeLister.Get(c.nodeName)
	if err != nil {
		return fmt.Errorf("failed to get local node %s: %w", c.nodeName, err)
	}

	nodeConfig, err := selectedNodeConfigForNode(uplink, node)
	if err != nil {
		return c.updateUplinkStateStatus(
			state,
			string(state.Status.HostInterfaceName),
			nil,
			"",
			metav1.ConditionFalse,
			discoveryReason(err),
			err.Error(),
		)
	}
	if nodeConfig == nil {
		return c.deleteUplinkState(state.Name)
	}

	hostInterfaceName := string(nodeConfig.HostInterfaceName)
	var hostState *hostInterfaceState
	if config.OvnKubeNode.Mode == ovntypes.NodeModeDPU {
		hostState, err = hostInterfaceStateFromStatus(state, hostInterfaceName)
		if err != nil {
			return c.updateUplinkStateStatus(
				state,
				hostInterfaceName,
				nil,
				"",
				metav1.ConditionFalse,
				uplinkv1alpha1.UplinkStateReasonWaitingForDPUHost,
				err.Error(),
			)
		}
	} else {
		hostState, err = c.hostDiscoverer.Discover(hostInterfaceName)
		if err != nil {
			return c.updateUplinkStateStatus(
				state,
				hostInterfaceName,
				nil,
				"",
				metav1.ConditionFalse,
				discoveryReason(err),
				err.Error(),
			)
		}
	}

	defaultBridgeName, err := defaultGatewayBridgeName(node)
	if err != nil {
		return c.updateUplinkStateStatus(
			state,
			hostInterfaceName,
			hostState,
			"",
			metav1.ConditionFalse,
			discoveryReason(err),
			err.Error(),
		)
	}

	if config.OvnKubeNode.Mode == ovntypes.NodeModeDPU {
		bridgeName, err := c.bridgeResolver.ResolveByHostMAC(hostState.macAddress, c.nodeName)
		if err != nil {
			return c.updateUplinkStateStatus(
				state,
				hostInterfaceName,
				hostState,
				"",
				metav1.ConditionFalse,
				discoveryReason(err),
				err.Error(),
			)
		}
		if err := c.validateBridgeUplink(bridgeName, hostInterfaceName, defaultBridgeName, false); err != nil {
			return c.updateUplinkStateStatus(
				state,
				hostInterfaceName,
				hostState,
				bridgeName,
				metav1.ConditionFalse,
				discoveryReason(err),
				err.Error(),
			)
		}
		return c.updateResolvedUplinkStateStatus(
			state,
			hostInterfaceName,
			hostState,
			bridgeName,
			"Uplink DPU bridge discovery succeeded",
		)
	}

	if config.OvnKubeNode.Mode == ovntypes.NodeModeDPUHost {
		bridgeName := ""
		if string(state.Status.HostInterfaceName) == hostInterfaceName && state.Status.OVSBridge != nil {
			bridgeName = state.Status.OVSBridge.Name
		}
		if err := validateDefaultGatewayBridge(bridgeName, defaultBridgeName); err != nil {
			return c.updateUplinkStateStatus(
				state,
				hostInterfaceName,
				hostState,
				"",
				metav1.ConditionFalse,
				discoveryReason(err),
				err.Error(),
			)
		}
		if dpuSideBridgeResolvedForHostInterface(state, hostInterfaceName) {
			// The DPU side has resolved the OVS bridge. The DPU-host side
			// now publishes host interface details under its field manager.
			return c.updateResolvedUplinkStateStatus(
				state,
				hostInterfaceName,
				hostState,
				"",
				"Uplink DPU bridge discovery succeeded",
			)
		}
		return c.updateUplinkStateStatus(
			state,
			hostInterfaceName,
			hostState,
			"",
			metav1.ConditionFalse,
			uplinkv1alpha1.UplinkStateReasonWaitingForDPU,
			"waiting for DPU-side bridge discovery",
		)
	}

	bridgeName, err := c.bridgeResolver.Resolve(hostInterfaceName)
	if err != nil {
		return c.updateUplinkStateStatus(
			state,
			hostInterfaceName,
			hostState,
			"",
			metav1.ConditionFalse,
			discoveryReason(err),
			err.Error(),
		)
	}
	if err := c.validateBridgeUplink(bridgeName, hostInterfaceName, defaultBridgeName, true); err != nil {
		return c.updateUplinkStateStatus(
			state,
			hostInterfaceName,
			hostState,
			bridgeName,
			metav1.ConditionFalse,
			discoveryReason(err),
			err.Error(),
		)
	}

	return c.updateResolvedUplinkStateStatus(
		state,
		hostInterfaceName,
		hostState,
		bridgeName,
		"Uplink discovery succeeded",
	)
}

func selectedNodeConfigForNode(
	uplink *uplinkv1alpha1.Uplink,
	node *corev1.Node,
) (*uplinkv1alpha1.UplinkNodeConfig, error) {
	selected, overlapping, err := uplinkutil.SelectNodeConfig(uplink, node)
	if err != nil {
		return nil, err
	}
	if overlapping {
		return nil, newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonNodeSelectorOverlap,
			fmt.Errorf("multiple Uplink %q nodeConfigs select node %q", uplink.Name, node.Name),
		)
	}
	return selected, nil
}

func defaultGatewayBridgeName(node *corev1.Node) (string, error) {
	gatewayConfig, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonGatewayInfoUnavailable,
			fmt.Errorf("failed to determine the default shared gateway bridge for node %q: %w",
				node.Name, err),
		)
	}
	if gatewayConfig.BridgeID == "" {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonGatewayInfoUnavailable,
			fmt.Errorf("default shared gateway bridge is not available for node %q", node.Name),
		)
	}
	return gatewayConfig.BridgeID, nil
}

func (c *Controller) validateBridgeUplink(
	bridgeName string,
	hostInterfaceName string,
	defaultGatewayBridgeName string,
	rejectHostInterfaceAsUplink bool,
) error {
	if bridgeName == ovsIntegrationBridge {
		return newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeInvalid,
			fmt.Errorf("OVN integration bridge %s cannot be used as an Uplink bridge",
				bridgeName),
		)
	}
	if err := validateDefaultGatewayBridge(bridgeName, defaultGatewayBridgeName); err != nil {
		return err
	}
	uplinkName, err := c.bridgeResolver.BridgeUplink(bridgeName)
	if err != nil {
		return err
	}
	if rejectHostInterfaceAsUplink && uplinkName == hostInterfaceName {
		return newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonInvalidHostInterface,
			fmt.Errorf("host interface %s is the physical uplink port for OVS bridge %s",
				hostInterfaceName, bridgeName),
		)
	}
	return nil
}

func validateDefaultGatewayBridge(bridgeName, defaultGatewayBridgeName string) error {
	if bridgeName != defaultGatewayBridgeName {
		return nil
	}
	return newDiscoveryError(
		uplinkv1alpha1.UplinkStateReasonDefaultGatewayBridgeUnsupported,
		fmt.Errorf("default shared gateway bridge %s cannot be used as an Uplink bridge", bridgeName),
	)
}

func (c *Controller) updateResolvedUplinkStateStatus(
	state *uplinkv1alpha1.UplinkState,
	hostInterfaceName string,
	hostState *hostInterfaceState,
	bridgeName string,
	message string,
) error {
	return c.updateUplinkStateStatus(
		state,
		hostInterfaceName,
		hostState,
		bridgeName,
		metav1.ConditionTrue,
		uplinkv1alpha1.UplinkStateReasonResolved,
		message,
	)
}

func (c *Controller) updateUplinkStateStatus(
	state *uplinkv1alpha1.UplinkState,
	hostInterfaceName string,
	hostState *hostInterfaceState,
	bridgeName string,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	condition := resolvedCondition(state, hostInterfaceName, status, reason, message)
	desiredStatus := desiredUplinkStateStatus(state, hostInterfaceName, hostState, bridgeName, condition)
	if reflect.DeepEqual(state.Status, desiredStatus) {
		return nil
	}

	statusApply := uplinkapply.UplinkStateStatus().
		WithType(uplinkv1alpha1.UplinkTypeOVSBridge).
		WithConditions(util.ConditionToApply(condition))

	if hostInterfaceName != "" {
		statusApply = statusApply.WithHostInterfaceName(
			uplinkv1alpha1.InterfaceName(hostInterfaceName),
		)
	}
	if config.OvnKubeNode.Mode != ovntypes.NodeModeDPU && hostState != nil {
		if hostState.macAddress != nil {
			statusApply = statusApply.WithMACAddress(
				uplinkv1alpha1.MACAddress(hostState.macAddress.String()),
			)
		}
		statusApply = statusApply.WithIPAddresses(ipAddressCIDRs(hostState.ipAddresses)...)
		statusApply = statusApply.WithDefaultGateways(ipAddresses(hostState.defaultGateways)...)
	}
	if config.OvnKubeNode.Mode != ovntypes.NodeModeDPUHost && bridgeName != "" {
		statusApply = statusApply.WithOVSBridge(
			uplinkapply.OVSBridgeStatus().WithName(bridgeName),
		)
	}

	_, err := c.uplinkClient.K8sV1alpha1().UplinkStates().Apply(
		context.Background(),
		uplinkapply.UplinkState(state.Name).WithStatus(
			statusApply,
		),
		metav1.ApplyOptions{
			FieldManager: StatusFieldManager(),
			Force:        true,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to update UplinkState %s status: %w",
			state.Name, err)
	}
	return nil
}

func desiredUplinkStateStatus(
	state *uplinkv1alpha1.UplinkState,
	hostInterfaceName string,
	hostState *hostInterfaceState,
	bridgeName string,
	condition metav1.Condition,
) uplinkv1alpha1.UplinkStateStatus {
	desired := uplinkv1alpha1.UplinkStateStatus{}
	// Split DPU modes preserve fields owned by the peer field manager while
	// comparing the status owned by this side.
	if config.OvnKubeNode.Mode == ovntypes.NodeModeDPU || config.OvnKubeNode.Mode == ovntypes.NodeModeDPUHost {
		desired = state.DeepCopy().Status
	}

	desired.Type = uplinkv1alpha1.UplinkTypeOVSBridge
	desired.HostInterfaceName = uplinkv1alpha1.InterfaceName(hostInterfaceName)
	desired.Conditions = append([]metav1.Condition(nil), state.Status.Conditions...)
	meta.SetStatusCondition(&desired.Conditions, condition)

	switch config.OvnKubeNode.Mode {
	case ovntypes.NodeModeDPU:
		setUplinkStateBridgeStatus(&desired, bridgeName)
	case ovntypes.NodeModeDPUHost:
		setUplinkStateHostStatus(&desired, hostState)
	default:
		setUplinkStateHostStatus(&desired, hostState)
		setUplinkStateBridgeStatus(&desired, bridgeName)
	}
	return desired
}

func setUplinkStateHostStatus(status *uplinkv1alpha1.UplinkStateStatus, hostState *hostInterfaceState) {
	status.MACAddress = ""
	status.IPAddresses = nil
	status.DefaultGateways = nil
	if hostState == nil {
		return
	}
	if hostState.macAddress != nil {
		status.MACAddress = uplinkv1alpha1.MACAddress(hostState.macAddress.String())
	}
	if ipAddresses := ipAddressCIDRs(hostState.ipAddresses); len(ipAddresses) > 0 {
		status.IPAddresses = ipAddresses
	}
	if defaultGateways := ipAddresses(hostState.defaultGateways); len(defaultGateways) > 0 {
		status.DefaultGateways = defaultGateways
	}
}

func setUplinkStateBridgeStatus(status *uplinkv1alpha1.UplinkStateStatus, bridgeName string) {
	status.OVSBridge = nil
	if bridgeName != "" {
		status.OVSBridge = &uplinkv1alpha1.OVSBridgeStatus{Name: bridgeName}
	}
}

func dpuSideBridgeResolvedForHostInterface(state *uplinkv1alpha1.UplinkState, hostInterfaceName string) bool {
	if state.Status.OVSBridge == nil || state.Status.OVSBridge.Name == "" {
		return false
	}
	resolvedCondition := resolvedConditionForHostInterface(state, hostInterfaceName)
	return resolvedCondition != nil &&
		resolvedCondition.Status == metav1.ConditionTrue &&
		resolvedCondition.Reason == uplinkv1alpha1.UplinkStateReasonResolved
}

// StatusFieldManager returns the field manager used by ovnkube-node when it
// applies UplinkState status.
func StatusFieldManager() string {
	switch config.OvnKubeNode.Mode {
	case ovntypes.NodeModeDPU:
		return dpuFieldManager
	case ovntypes.NodeModeDPUHost:
		return dpuHostFieldManager
	default:
		return fieldManager
	}
}

func resolvedCondition(
	state *uplinkv1alpha1.UplinkState,
	hostInterfaceName string,
	status metav1.ConditionStatus,
	reason string,
	message string,
) metav1.Condition {
	var existing []metav1.Condition
	if current := resolvedConditionForHostInterface(state, hostInterfaceName); current != nil {
		existing = []metav1.Condition{*current}
	}
	condition, _ := util.MergeStatusCondition(existing, metav1.Condition{
		Type:    uplinkv1alpha1.UplinkStateConditionResolved,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
	return condition
}

func resolvedConditionForHostInterface(state *uplinkv1alpha1.UplinkState, hostInterfaceName string) *metav1.Condition {
	if string(state.Status.HostInterfaceName) != hostInterfaceName {
		return nil
	}
	return meta.FindStatusCondition(
		state.Status.Conditions,
		uplinkv1alpha1.UplinkStateConditionResolved,
	)
}

func (c *Controller) ensureUplinkState(
	uplink *uplinkv1alpha1.Uplink,
	nodeConfig *uplinkv1alpha1.UplinkNodeConfig,
	nodeConfigErr error,
) (*uplinkv1alpha1.UplinkState, error) {
	name := uplinkutil.StateName(uplink.Name, c.nodeName)
	state, err := c.uplinkStateLister.Get(name)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get UplinkState %s: %w", name, err)
		}
		state = desiredUplinkState(uplink, c.nodeName, name, nodeConfig, nodeConfigErr)
		created, err := c.uplinkClient.K8sV1alpha1().UplinkStates().Create(
			context.Background(),
			state,
			metav1.CreateOptions{},
		)
		if err == nil {
			return created, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("failed to create UplinkState %s: %w", name, err)
		}
		state, err = c.uplinkClient.K8sV1alpha1().UplinkStates().Get(
			context.Background(),
			name,
			metav1.GetOptions{},
		)
		if err != nil {
			return nil, fmt.Errorf("failed to get existing UplinkState %s: %w", name, err)
		}
	}

	if err := uplinkutil.ValidateStateIdentity(state, name, uplink.Name, c.nodeName); err != nil {
		klog.Warningf("Ignoring Uplink %q on node %q: %v", uplink.Name, c.nodeName, err)
		return nil, nil
	}

	desired := desiredUplinkState(uplink, c.nodeName, name, nodeConfig, nodeConfigErr)
	if !reflect.DeepEqual(state.Labels, desired.Labels) ||
		!reflect.DeepEqual(state.Annotations, desired.Annotations) ||
		!reflect.DeepEqual(state.OwnerReferences, desired.OwnerReferences) {
		copy := state.DeepCopy()
		copy.Labels = desired.Labels
		copy.Annotations = desired.Annotations
		copy.OwnerReferences = desired.OwnerReferences
		if _, err := c.uplinkClient.K8sV1alpha1().UplinkStates().Update(
			context.Background(),
			copy,
			metav1.UpdateOptions{},
		); err != nil {
			return nil, fmt.Errorf("failed to update UplinkState %s metadata: %w",
				name, err)
		}
		state = copy
	}
	return state, nil
}

func desiredUplinkState(
	uplink *uplinkv1alpha1.Uplink,
	nodeName, name string,
	nodeConfig *uplinkv1alpha1.UplinkNodeConfig,
	nodeConfigErr error,
) *uplinkv1alpha1.UplinkState {
	state := &uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(
					uplink,
					uplinkv1alpha1.SchemeGroupVersion.WithKind("Uplink"),
				),
			},
		},
		Spec: uplinkv1alpha1.UplinkStateSpec{
			UplinkName: uplink.Name,
			NodeName:   nodeName,
		},
	}
	if nodeConfig != nil {
		state.Status.Type = nodeConfig.Type
		state.Status.HostInterfaceName = nodeConfig.HostInterfaceName
	}
	if nodeConfigErr != nil {
		state.Status.Conditions = []metav1.Condition{
			{
				Type:               uplinkv1alpha1.UplinkStateConditionResolved,
				Status:             metav1.ConditionFalse,
				Reason:             discoveryReason(nodeConfigErr),
				Message:            nodeConfigErr.Error(),
				LastTransitionTime: metav1.Now(),
			},
		}
	}
	return state
}

func (c *Controller) deleteUplinkState(name string) error {
	err := c.uplinkClient.K8sV1alpha1().UplinkStates().Delete(
		context.Background(),
		name,
		metav1.DeleteOptions{},
	)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete UplinkState %s: %w", name, err)
	}
	return nil
}

func hostInterfaceStateFromStatus(
	state *uplinkv1alpha1.UplinkState,
	hostInterfaceName string,
) (*hostInterfaceState, error) {
	if string(state.Status.HostInterfaceName) != hostInterfaceName {
		return nil, fmt.Errorf("waiting for DPU-host state for interface %s",
			hostInterfaceName)
	}
	macAddress, err := net.ParseMAC(string(state.Status.MACAddress))
	if err != nil {
		return nil, fmt.Errorf("waiting for DPU-host MAC address for interface %s: %w",
			hostInterfaceName, err)
	}
	ipAddresses := make([]*net.IPNet, 0, len(state.Status.IPAddresses))
	for _, ipAddress := range state.Status.IPAddresses {
		ip, cidr, err := net.ParseCIDR(string(ipAddress))
		if err != nil {
			return nil, fmt.Errorf("failed to parse DPU-host IP address %q: %w",
				ipAddress, err)
		}
		cidr.IP = ip
		ipAddresses = append(ipAddresses, cidr)
	}
	if len(ipAddresses) == 0 {
		return nil, fmt.Errorf("waiting for DPU-host IP addresses for interface %s",
			hostInterfaceName)
	}
	defaultGateways := make([]net.IP, 0, len(state.Status.DefaultGateways))
	for _, defaultGateway := range state.Status.DefaultGateways {
		ip := net.ParseIP(string(defaultGateway))
		if ip == nil {
			return nil, fmt.Errorf("failed to parse DPU-host default gateway %q",
				defaultGateway)
		}
		defaultGateways = append(defaultGateways, ip)
	}
	return &hostInterfaceState{
		macAddress:      macAddress,
		ipAddresses:     ipAddresses,
		defaultGateways: defaultGateways,
	}, nil
}

func discoveryReason(err error) string {
	var discoveryErr *discoveryError
	if errors.As(err, &discoveryErr) {
		return discoveryErr.reason
	}
	return uplinkv1alpha1.UplinkStateReasonGatewayInfoUnavailable
}

func ipAddressCIDRs(ipNets []*net.IPNet) []uplinkv1alpha1.IPAddressCIDR {
	out := make([]uplinkv1alpha1.IPAddressCIDR, 0, len(ipNets))
	for _, ipNet := range ipNets {
		out = append(out, uplinkv1alpha1.IPAddressCIDR(ipNet.String()))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func ipAddresses(ips []net.IP) []uplinkv1alpha1.IPAddress {
	out := make([]uplinkv1alpha1.IPAddress, 0, len(ips))
	for _, ip := range ips {
		out = append(out, uplinkv1alpha1.IPAddress(ip.String()))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func uplinkNeedsUpdate(oldObj, newObj *uplinkv1alpha1.Uplink) bool {
	if oldObj == nil {
		return true
	}
	return !reflect.DeepEqual(oldObj.Spec, newObj.Spec) ||
		oldObj.DeletionTimestamp.IsZero() != newObj.DeletionTimestamp.IsZero()
}

func uplinkStateNeedsUpdate(oldObj, newObj *uplinkv1alpha1.UplinkState) bool {
	if oldObj == nil {
		return true
	}
	return !reflect.DeepEqual(oldObj.Spec, newObj.Spec) ||
		!reflect.DeepEqual(oldObj.Status, newObj.Status) ||
		!reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		!reflect.DeepEqual(oldObj.Annotations, newObj.Annotations) ||
		!reflect.DeepEqual(oldObj.OwnerReferences, newObj.OwnerReferences)
}

func (c *Controller) nodeNeedsUpdate(oldObj, newObj *corev1.Node) bool {
	if newObj.Name != c.nodeName {
		return false
	}
	if oldObj == nil {
		return true
	}
	// ParseNodeL3GatewayAnnotation requires a chassis ID for enabled gateways.
	// Watch its initial population so reconciliation recovers from a missing
	// annotation even though Uplink discovery does not use the chassis ID itself.
	return !reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		util.NodeL3GatewayAnnotationChanged(oldObj, newObj) ||
		util.NodeChassisIDAnnotationChanged(oldObj, newObj) ||
		oldObj.DeletionTimestamp.IsZero() != newObj.DeletionTimestamp.IsZero()
}

type netlinkHostInterfaceDiscoverer struct{}

func (d netlinkHostInterfaceDiscoverer) Discover(hostInterfaceName string) (*hostInterfaceState, error) {
	link, err := util.GetNetLinkOps().LinkByName(hostInterfaceName)
	if err != nil {
		return nil, newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonHostInterfaceNotFound,
			fmt.Errorf("host interface %s was not found: %w", hostInterfaceName, err),
		)
	}
	addrs, err := nodeutil.GetNetworkInterfaceIPAddresses(hostInterfaceName)
	if err != nil {
		return nil, newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonGatewayInfoUnavailable,
			fmt.Errorf("failed to read IP addresses from %s: %w",
				hostInterfaceName, err),
		)
	}
	routes, err := util.GetNetLinkOps().RouteList(link, netlink.FAMILY_ALL)
	if err != nil {
		return nil, newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonGatewayInfoUnavailable,
			fmt.Errorf("failed to list routes for %s: %w", hostInterfaceName, err),
		)
	}

	defaultGateways := make([]net.IP, 0, len(routes))
	for _, route := range routes {
		if !isDefaultRoute(route) || route.Gw == nil {
			continue
		}
		defaultGateways = append(defaultGateways, route.Gw)
	}
	return &hostInterfaceState{
		macAddress:      link.Attrs().HardwareAddr,
		ipAddresses:     addrs,
		defaultGateways: defaultGateways,
	}, nil
}

func isDefaultRoute(route netlink.Route) bool {
	if route.Dst == nil {
		return true
	}
	ones, bits := route.Dst.Mask.Size()
	return bits != 0 && ones == 0
}

// defaultOVSBridgeResolver implements ovsBridgeResolver using OVS and device discovery.
type defaultOVSBridgeResolver struct {
	ovsClient libovsdbclient.Client
}

func (r defaultOVSBridgeResolver) Resolve(hostInterfaceName string) (string, error) {
	bridge, err := libovsdbops.GetBridge(r.ovsClient, hostInterfaceName)
	if err == nil {
		return bridge.Name, nil
	}
	if !errors.Is(err, libovsdbclient.ErrNotFound) {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeNotFound,
			fmt.Errorf("failed to look up OVS bridge %s: %w", hostInterfaceName, err),
		)
	}

	bridgeName, resolveErr := r.bridgeForPortOrInterface(hostInterfaceName)
	if resolveErr == nil {
		return bridgeName, nil
	}

	rep, repErr := util.GetNetdeviceRepresentorName(hostInterfaceName)
	if repErr != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeNotFound,
			fmt.Errorf("failed to resolve OVS bridge for %s: %w", hostInterfaceName, resolveErr),
		)
	}

	bridgeName, err = r.bridgeForPortOrInterface(rep)
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeNotFound,
			fmt.Errorf("failed to resolve OVS bridge for %s representor %s: %w", hostInterfaceName, rep, err),
		)
	}
	return bridgeName, nil
}

func (r defaultOVSBridgeResolver) BridgeUplink(bridgeName string) (string, error) {
	bridge, err := libovsdbops.GetBridge(r.ovsClient, bridgeName)
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound,
			fmt.Errorf("failed to get OVS bridge %s: %w", bridgeName, err),
		)
	}

	bridgePortIDs := make(map[string]struct{}, len(bridge.Ports))
	for _, portID := range bridge.Ports {
		bridgePortIDs[portID] = struct{}{}
	}
	ports, err := libovsdbops.FindOVSPortsWithPredicate(r.ovsClient, func(port *vswitchd.Port) bool {
		_, ok := bridgePortIDs[port.UUID]
		return ok
	})
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound,
			fmt.Errorf("failed to list ports for OVS bridge %s: %w", bridgeName, err),
		)
	}

	interfaceIDs := map[string]struct{}{}
	for _, port := range ports {
		for _, interfaceID := range port.Interfaces {
			interfaceIDs[interfaceID] = struct{}{}
		}
	}
	interfaces, err := libovsdbops.FindInterfacesWithPredicate(r.ovsClient, func(iface *vswitchd.Interface) bool {
		_, ok := interfaceIDs[iface.UUID]
		return ok
	})
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound,
			fmt.Errorf("failed to list interfaces for OVS bridge %s: %w", bridgeName, err),
		)
	}
	interfacesByID := make(map[string]*vswitchd.Interface, len(interfaces))
	for _, iface := range interfaces {
		interfacesByID[iface.UUID] = iface
	}

	systemPorts := []string{}
	for _, port := range ports {
		for _, interfaceID := range port.Interfaces {
			iface, ok := interfacesByID[interfaceID]
			if ok && iface.Type == "system" {
				systemPorts = append(systemPorts, port.Name)
			}
		}
	}

	var uplinkName string
	if len(systemPorts) == 1 {
		uplinkName = systemPorts[0]
	} else {
		if len(systemPorts) > 1 {
			klog.Infof("Found more than one system Type ports on the OVS bridge %s, so skipping "+
				"this method of determining the uplink port", bridgeName)
		}
		uplinkName = bridge.ExternalIDs["bridge-uplink"]
		if uplinkName == "" && strings.HasPrefix(bridgeName, "br") {
			uplinkName = strings.TrimPrefix(bridgeName, "br")
		}
	}
	if uplinkName == "" {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound,
			fmt.Errorf("failed to resolve physical uplink for OVS bridge %s", bridgeName),
		)
	}

	uplinkInterfaces, err := libovsdbops.FindInterfacesWithPredicate(r.ovsClient, func(iface *vswitchd.Interface) bool {
		return iface.Name == uplinkName
	})
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound,
			fmt.Errorf("failed to get interface for bridge %s physical uplink %s: %w", bridgeName, uplinkName, err),
		)
	}
	if len(uplinkInterfaces) == 0 {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound,
			fmt.Errorf("failed to find interface for bridge %s physical uplink %s", bridgeName, uplinkName),
		)
	}
	return uplinkName, nil
}

func (r defaultOVSBridgeResolver) ResolveByHostMAC(hostMAC net.HardwareAddr, nodeName string) (string, error) {
	bridges, err := libovsdbops.ListBridges(r.ovsClient)
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeNotFound,
			fmt.Errorf("failed to list OVS bridges: %w", err),
		)
	}
	sort.Slice(bridges, func(i, j int) bool {
		return bridges[i].Name < bridges[j].Name
	})
	for _, bridge := range bridges {
		if bridge.Name == ovsIntegrationBridge {
			continue
		}
		bridgeMAC, err := util.GetDPUOps().GetHostGatewayMACAddress(r.ovsClient, bridge.Name, nodeName)
		if err != nil {
			klog.V(5).Infof("Failed to read DPU host MAC for bridge %s: %v", bridge.Name, err)
			continue
		}
		if bytes.Equal(bridgeMAC, hostMAC) {
			return bridge.Name, nil
		}
	}
	return "", newDiscoveryError(
		uplinkv1alpha1.UplinkStateReasonBridgeNotFound,
		fmt.Errorf("failed to find DPU bridge for host MAC %s", hostMAC),
	)
}

func (r defaultOVSBridgeResolver) bridgeForPortOrInterface(name string) (string, error) {
	interfaces, err := libovsdbops.FindInterfacesWithPredicate(r.ovsClient, func(iface *vswitchd.Interface) bool {
		return iface.Name == name
	})
	if err != nil {
		return "", fmt.Errorf("failed to look up OVS interface %s: %w", name, err)
	}
	interfaceIDs := make(map[string]struct{}, len(interfaces))
	for _, iface := range interfaces {
		interfaceIDs[iface.UUID] = struct{}{}
	}

	ports, err := libovsdbops.FindOVSPortsWithPredicate(r.ovsClient, func(port *vswitchd.Port) bool {
		if port.Name == name {
			return true
		}
		for _, interfaceID := range port.Interfaces {
			if _, ok := interfaceIDs[interfaceID]; ok {
				return true
			}
		}
		return false
	})
	if err != nil {
		return "", fmt.Errorf("failed to look up OVS port for %s: %w", name, err)
	}
	if len(ports) == 0 {
		return "", fmt.Errorf("OVS port or interface %s not found: %w", name, libovsdbclient.ErrNotFound)
	}
	portIDs := make(map[string]struct{}, len(ports))
	for _, port := range ports {
		portIDs[port.UUID] = struct{}{}
	}

	bridges, err := libovsdbops.ListBridges(r.ovsClient)
	if err != nil {
		return "", fmt.Errorf("failed to list OVS bridges for %s: %w", name, err)
	}
	for _, bridge := range bridges {
		for _, portID := range bridge.Ports {
			if _, ok := portIDs[portID]; ok {
				return bridge.Name, nil
			}
		}
	}
	return "", fmt.Errorf("OVS port or interface %s is not attached to a bridge: %w", name, libovsdbclient.ErrNotFound)
}
