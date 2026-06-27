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
	"time"

	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	metaapply "k8s.io/client-go/applyconfigurations/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/applyconfiguration/uplink/v1alpha1"
	uplinkclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	nodeutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/util"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
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
	Resolve(hostInterfaceName string) (string, error)
	ResolveByHostMAC(hostMAC net.HardwareAddr, nodeName string) (string, error)
	BridgeUplink(bridgeName string) (string, error)
}

type discoveryError struct {
	reason string
	err    error
}

func (e *discoveryError) Error() string {
	return e.err.Error()
}

func (e *discoveryError) Unwrap() error {
	return e.err
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
func NewController(nodeName string, wf factory.NodeWatchFactory, ovnClient *util.OVNNodeClientset) *Controller {
	c := &Controller{
		nodeName:          nodeName,
		uplinkClient:      ovnClient.UplinkClient,
		uplinkLister:      wf.UplinkInformer().Lister(),
		uplinkStateLister: wf.UplinkStateInformer().Lister(),
		nodeLister:        wf.NodeCoreInformer().Lister(),
		hostDiscoverer:    netlinkHostInterfaceDiscoverer{},
		bridgeResolver:    ovsPortBridgeResolver{},
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
		ObjNeedsUpdate: nodeNeedsUpdate,
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

	nodeConfig, err := selectedNodeConfigForNode(uplink, node)
	if err != nil {
		state, ensureErr := c.ensureUplinkState(uplink)
		if ensureErr != nil {
			return ensureErr
		}
		return c.updateUplinkStateStatus(
			state,
			c.currentHostInterfaceName(state),
			nil,
			"",
			metav1.ConditionFalse,
			discoveryReason(err),
			err.Error(),
		)
	}
	if nodeConfig == nil {
		return c.deleteUplinkState(uplinkutil.StateName(uplink.Name, c.nodeName))
	}

	state, err := c.ensureUplinkState(uplink)
	if err != nil {
		return err
	}
	// Creation also produces an UplinkState watch event, but existing states
	// need explicit rediscovery when the selected Uplink config changes.
	c.uplinkStateController.Reconcile(state.Name)
	return nil
}

func (c *Controller) reconcileNode(key string) error {
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
	if !c.isUplinkStateForNode(state) {
		return nil
	}

	uplinkName, _ := uplinkutil.StateIdentity(state)
	if uplinkName == "" {
		return nil
	}

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
			c.currentHostInterfaceName(state),
			nil,
			"",
			metav1.ConditionFalse,
			discoveryReason(err),
			err.Error(),
		)
	}
	if nodeConfig == nil {
		return c.updateUplinkStateStatus(
			state,
			c.currentHostInterfaceName(state),
			nil,
			"",
			metav1.ConditionFalse,
			uplinkv1alpha1.UplinkStateReasonNodeNotSelected,
			fmt.Sprintf("Uplink %q does not select node %q", uplink.Name, c.nodeName),
		)
	}

	hostInterfaceName := string(nodeConfig.HostInterfaceName)
	if config.OvnKubeNode.Mode == ovntypes.NodeModeDPU {
		hostState, err := hostInterfaceStateFromStatus(state, hostInterfaceName)
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
		if err := c.validateBridgeUplink(bridgeName, hostInterfaceName, false); err != nil {
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
		return c.updateReadyUplinkStateStatus(
			state,
			hostInterfaceName,
			hostState,
			bridgeName,
			"Uplink DPU bridge discovery succeeded",
		)
	}

	hostState, err := c.hostDiscoverer.Discover(hostInterfaceName)
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

	if config.OvnKubeNode.Mode == ovntypes.NodeModeDPUHost {
		if dpuSideBridgeReadyForHostInterface(state, hostInterfaceName) {
			// The DPU side has resolved the OVS bridge. The DPU-host side
			// now publishes host interface details under its field manager.
			return c.updateReadyUplinkStateStatus(
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
	if err := c.validateBridgeUplink(bridgeName, hostInterfaceName, true); err != nil {
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

	return c.updateReadyUplinkStateStatus(
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
	var selected *uplinkv1alpha1.UplinkNodeConfig
	for i := range uplink.Spec.NodeConfigs {
		nodeConfig := &uplink.Spec.NodeConfigs[i]
		if nodeConfig.Type != uplinkv1alpha1.UplinkTypeOVSBridge {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(&nodeConfig.NodeSelector)
		if err != nil {
			return nil, fmt.Errorf("nodeConfig %d has invalid nodeSelector: %w", i, err)
		}
		if !selector.Matches(labels.Set(node.Labels)) {
			continue
		}
		if selected != nil {
			return nil,
				newDiscoveryError(
					uplinkv1alpha1.UplinkStateReasonNodeSelectorOverlap,
					fmt.Errorf("multiple Uplink %q nodeConfigs select node %q",
						uplink.Name, node.Name),
				)
		}
		selected = nodeConfig
	}
	return selected, nil
}

func (c *Controller) validateBridgeUplink(
	bridgeName string,
	hostInterfaceName string,
	rejectHostInterfaceAsUplink bool,
) error {
	if bridgeName == ovsIntegrationBridge {
		return newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeInvalid,
			fmt.Errorf("OVN integration bridge %s cannot be used as an Uplink bridge",
				bridgeName),
		)
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

func (c *Controller) updateReadyUplinkStateStatus(
	state *uplinkv1alpha1.UplinkState,
	hostInterfaceName string,
	hostState *hostInterfaceState,
	bridgeName string,
	message string,
) error {
	status := metav1.ConditionTrue
	reason := uplinkv1alpha1.UplinkStateReasonReady
	if condition := gatewayProgrammingFailureCondition(state, hostInterfaceName); condition != nil {
		status = metav1.ConditionFalse
		reason = condition.Reason
		message = condition.Message
	}
	return c.updateUplinkStateStatus(
		state,
		hostInterfaceName,
		hostState,
		bridgeName,
		status,
		reason,
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
	uplinkName, nodeName := uplinkutil.StateIdentity(state)
	statusApply := uplinkapply.UplinkStateStatus().
		WithUplinkName(uplinkName).
		WithNodeName(nodeName).
		WithType(uplinkv1alpha1.UplinkTypeOVSBridge).
		WithConditions(readyCondition(state, hostInterfaceName, status, reason, message))

	if hostInterfaceName != "" {
		statusApply = statusApply.WithHostInterfaceName(
			uplinkv1alpha1.InterfaceName(hostInterfaceName),
		)
	}
	if hostState != nil {
		if hostState.macAddress != nil {
			statusApply = statusApply.WithMACAddress(
				uplinkv1alpha1.MACAddress(hostState.macAddress.String()),
			)
		}
		statusApply = statusApply.WithIPAddresses(ipAddressCIDRs(hostState.ipAddresses)...)
		statusApply = statusApply.WithDefaultGateways(ipAddresses(hostState.defaultGateways)...)
	}
	if bridgeName != "" {
		statusApply = statusApply.WithOVSBridge(
			uplinkapply.OVSBridgeStatus().WithName(bridgeName),
		)
	}

	_, err := c.uplinkClient.K8sV1alpha1().UplinkStates().ApplyStatus(
		context.Background(),
		uplinkapply.UplinkState(state.Name).WithStatus(
			statusApply,
		),
		metav1.ApplyOptions{
			FieldManager: statusFieldManager(),
			Force:        true,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to update UplinkState %s status: %w",
			state.Name, err)
	}
	return nil
}

func dpuSideBridgeReadyForHostInterface(state *uplinkv1alpha1.UplinkState, hostInterfaceName string) bool {
	if state.Status.OVSBridge == nil || state.Status.OVSBridge.Name == "" {
		return false
	}
	readyCondition := readyConditionForHostInterface(state, hostInterfaceName)
	return readyCondition != nil &&
		readyCondition.Status == metav1.ConditionTrue &&
		readyCondition.Reason == uplinkv1alpha1.UplinkStateReasonReady
}

func gatewayProgrammingFailureCondition(
	state *uplinkv1alpha1.UplinkState,
	hostInterfaceName string,
) *metav1.Condition {
	condition := readyConditionForHostInterface(state, hostInterfaceName)
	if condition == nil || condition.Status != metav1.ConditionFalse {
		return nil
	}
	switch condition.Reason {
	case uplinkv1alpha1.UplinkStateReasonVRFAttachmentFailed,
		uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed:
		return condition
	default:
		return nil
	}
}

func statusFieldManager() string {
	switch config.OvnKubeNode.Mode {
	case ovntypes.NodeModeDPU:
		return dpuFieldManager
	case ovntypes.NodeModeDPUHost:
		return dpuHostFieldManager
	default:
		return fieldManager
	}
}

func readyCondition(
	state *uplinkv1alpha1.UplinkState,
	hostInterfaceName string,
	status metav1.ConditionStatus,
	reason string,
	message string,
) *metaapply.ConditionApplyConfiguration {
	condition := metaapply.Condition().
		WithType(uplinkv1alpha1.UplinkStateConditionReady).
		WithStatus(status).
		WithReason(reason).
		WithMessage(trimConditionMessage(message))

	now := metav1.NewTime(time.Now())
	if current := readyConditionForHostInterface(state, hostInterfaceName); current != nil &&
		current.Status == status {
		now = current.LastTransitionTime
	}
	return condition.WithLastTransitionTime(now)
}

func readyConditionForHostInterface(state *uplinkv1alpha1.UplinkState, hostInterfaceName string) *metav1.Condition {
	if string(state.Status.HostInterfaceName) != hostInterfaceName {
		return nil
	}
	return meta.FindStatusCondition(
		state.Status.Conditions,
		uplinkv1alpha1.UplinkStateConditionReady,
	)
}

func (c *Controller) isUplinkStateForNode(state *uplinkv1alpha1.UplinkState) bool {
	_, nodeName := uplinkutil.StateIdentity(state)
	return nodeName == c.nodeName
}

func (c *Controller) ensureUplinkState(uplink *uplinkv1alpha1.Uplink) (*uplinkv1alpha1.UplinkState, error) {
	name := uplinkutil.StateName(uplink.Name, c.nodeName)
	state, err := c.uplinkStateLister.Get(name)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get UplinkState %s: %w", name, err)
		}
		state = desiredUplinkState(uplink, c.nodeName, name)
		if _, err := c.uplinkClient.K8sV1alpha1().UplinkStates().Create(
			context.Background(),
			state,
			metav1.CreateOptions{},
		); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("failed to create UplinkState %s: %w", name, err)
		}
		return state, c.applyUplinkStateIdentity(uplink.Name, c.nodeName, name)
	}

	desired := desiredUplinkState(uplink, c.nodeName, name)
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
	if state.Status.UplinkName != uplink.Name || state.Status.NodeName != c.nodeName {
		if err := c.applyUplinkStateIdentity(uplink.Name, c.nodeName, name); err != nil {
			return nil, err
		}
		state = state.DeepCopy()
		state.Status.UplinkName = uplink.Name
		state.Status.NodeName = c.nodeName
	}
	return state, nil
}

func desiredUplinkState(uplink *uplinkv1alpha1.Uplink, nodeName, name string) *uplinkv1alpha1.UplinkState {
	return &uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				uplinkutil.StateAnnotationUplink: uplink.Name,
				uplinkutil.StateAnnotationNode:   nodeName,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(
					uplink,
					uplinkv1alpha1.SchemeGroupVersion.WithKind("Uplink"),
				),
			},
		},
		Status: uplinkv1alpha1.UplinkStateStatus{
			UplinkName: uplink.Name,
			NodeName:   nodeName,
		},
	}
}

func (c *Controller) applyUplinkStateIdentity(uplinkName, nodeName, uplinkStateName string) error {
	_, err := c.uplinkClient.K8sV1alpha1().UplinkStates().ApplyStatus(
		context.Background(),
		uplinkapply.UplinkState(uplinkStateName).WithStatus(
			uplinkapply.UplinkStateStatus().
				WithUplinkName(uplinkName).
				WithNodeName(nodeName),
		),
		metav1.ApplyOptions{
			FieldManager: fieldManager,
			Force:        true,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to apply UplinkState %s identity: %w",
			uplinkStateName, err)
	}
	return nil
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

func (c *Controller) currentHostInterfaceName(state *uplinkv1alpha1.UplinkState) string {
	return string(state.Status.HostInterfaceName)
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

func trimConditionMessage(message string) string {
	const maxMessageLen = 32768
	if len(message) >= maxMessageLen {
		return message[:maxMessageLen-1]
	}
	return message
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
	return !reflect.DeepEqual(oldObj.Status, newObj.Status) ||
		!reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		!reflect.DeepEqual(oldObj.Annotations, newObj.Annotations) ||
		!reflect.DeepEqual(oldObj.OwnerReferences, newObj.OwnerReferences)
}

func nodeNeedsUpdate(oldObj, newObj *corev1.Node) bool {
	if oldObj == nil {
		return true
	}
	return oldObj.Name == newObj.Name &&
		(!reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
			oldObj.DeletionTimestamp.IsZero() != newObj.DeletionTimestamp.IsZero())
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

type ovsPortBridgeResolver struct{}

func (r ovsPortBridgeResolver) Resolve(hostInterfaceName string) (string, error) {
	stdout, _, err := util.RunOVSVsctl("--if-exists", "get", "Bridge", hostInterfaceName, "name")
	if err == nil && strings.TrimSpace(stdout) == hostInterfaceName {
		return hostInterfaceName, nil
	}

	stdout, stderr, err := util.RunOVSVsctl("port-to-br", hostInterfaceName)
	if err == nil {
		bridgeName := strings.TrimSpace(stdout)
		if bridgeName != "" {
			return bridgeName, nil
		}
	}

	rep, repErr := util.GetNetdeviceRepresentorName(hostInterfaceName)
	if repErr != nil {
		if err != nil {
			return "", newDiscoveryError(
				uplinkv1alpha1.UplinkStateReasonBridgeNotFound,
				fmt.Errorf("failed to resolve OVS bridge for %s: %v: %s",
					hostInterfaceName, err, stderr),
			)
		}
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeNotFound,
			fmt.Errorf("failed to resolve OVS bridge for %s", hostInterfaceName),
		)
	}

	stdout, stderr, err = util.RunOVSVsctl("port-to-br", rep)
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeNotFound,
			fmt.Errorf("failed to resolve OVS bridge for %s representor %s: %v: %s",
				hostInterfaceName, rep, err, stderr),
		)
	}
	bridgeName := strings.TrimSpace(stdout)
	if bridgeName == "" {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeNotFound,
			fmt.Errorf("failed to resolve OVS bridge for %s representor %s",
				hostInterfaceName, rep),
		)
	}
	return bridgeName, nil
}

func (r ovsPortBridgeResolver) BridgeUplink(bridgeName string) (string, error) {
	uplinkName, err := util.GetNicName(bridgeName)
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound,
			fmt.Errorf("failed to resolve physical uplink for OVS bridge %s: %w",
				bridgeName, err),
		)
	}
	uplinkName = strings.TrimSpace(uplinkName)
	if uplinkName == "" {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound,
			fmt.Errorf("failed to resolve physical uplink for OVS bridge %s",
				bridgeName),
		)
	}

	_, stderr, err := util.RunOVSVsctl("get", "interface", uplinkName, "ofport")
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound,
			fmt.Errorf("failed to get ofport for bridge %s physical uplink %s: %v: %s",
				bridgeName, uplinkName, err, stderr),
		)
	}
	return uplinkName, nil
}

func (r ovsPortBridgeResolver) ResolveByHostMAC(hostMAC net.HardwareAddr, nodeName string) (string, error) {
	stdout, stderr, err := util.RunOVSVsctl("list-br")
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeNotFound,
			fmt.Errorf("failed to list OVS bridges: %v: %s", err, stderr),
		)
	}
	for _, bridgeName := range strings.Fields(stdout) {
		if bridgeName == ovsIntegrationBridge {
			continue
		}
		bridgeMAC, err := util.GetDPUOps().GetHostGatewayMACAddress(bridgeName, nodeName)
		if err != nil {
			klog.V(5).Infof("Failed to read DPU host MAC for bridge %s: %v", bridgeName, err)
			continue
		}
		if bytes.Equal(bridgeMAC, hostMAC) {
			return bridgeName, nil
		}
	}
	return "", newDiscoveryError(
		uplinkv1alpha1.UplinkStateReasonBridgeNotFound,
		fmt.Errorf("failed to find DPU bridge for host MAC %s", hostMAC),
	)
}
