// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package uplink

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	metaapply "k8s.io/client-go/applyconfigurations/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	k8scontrollerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/applyconfiguration/uplink/v1alpha1"
	uplinkclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/applyconfiguration/userdefinednetwork/v1"
	udnclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	udnlisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	fieldManager    = "clustermanager-uplink-controller"
	finalizerUplink = "k8s.ovn.org/uplink-protection"

	reasonInvalidSpec              = "InvalidSpec"
	reasonNodeSelectorOverlap      = "NodeSelectorOverlap"
	reasonNoMatchingNodes          = "NoMatchingNodes"
	reasonMissingUplinkState       = "MissingUplinkState"
	reasonUplinkStateNotReady      = "UplinkStateNotReady"
	reasonUplinkStateIdentityError = "UplinkStateIdentityError"
	reasonReady                    = "Ready"

	obsoleteUplinkConditionReferenced = "Referenced"

	conditionTypeUplinksReady          = "UplinksReady"
	reasonUplinksReady                 = "UplinksReady"
	reasonUplinkUnsupportedGatewayMode = "UplinkUnsupportedGatewayMode"
	reasonUplinkUnsupportedTransport   = "UplinkUnsupportedTransport"
	reasonUplinkNotFound               = "UplinkNotFound"
	reasonUplinkOverlapOnNode          = "UplinkOverlapOnNode"
	reasonUplinkNotFoundForNode        = "UplinkNotFoundForNode"
	reasonUplinkNotReadyForNode        = "UplinkNotReadyForNode"
	reasonUplinkVRFAttachmentFailed    = "UplinkVRFAttachmentFailed"
	reasonUplinkBridgeMappingFailed    = "UplinkBridgeMappingFailed"

	networkRefSeparator = "\x00"
)

// Controller reconciles Uplink API objects in cluster-manager. It resolves
// per-node Uplink nodeConfigs, deletes stale UplinkStates, protects referenced
// Uplinks with a finalizer, aggregates UplinkState readiness into Uplink
// status, and updates CUDN UplinksReady status from the states for the nodes
// selected by that CUDN.
type Controller struct {
	uplinkClient      uplinkclientset.Interface
	udnClient         udnclientset.Interface
	uplinkLister      uplinklisters.UplinkLister
	uplinkStateLister uplinklisters.UplinkStateLister
	cudnLister        udnlisters.ClusterUserDefinedNetworkLister
	nodeLister        corelisters.NodeLister
	networkManager    networkmanager.Interface

	uplinkController      controllerutil.Controller
	uplinkStateController controllerutil.Controller
	cudnController        controllerutil.Controller
	nodeController        controllerutil.Controller
	networkRefController  controllerutil.Reconciler
	networkRefReconciler  networkmanager.NetworkRefReconciler
	networkRefID          uint64

	cudnUplinkIndexMu sync.RWMutex
	cudnUplinkIndex   map[string][]string

	uplinkStateIdentityMu     sync.RWMutex
	uplinkStateIdentityByName map[string]uplinkStateIdentity
}

type uplinkStateIdentity struct {
	uplinkName string
	nodeName   string
}

type cudnUplinkFailure struct {
	uplink string
	reason string
	node   string
}

// NewController creates a new cluster-manager Uplink controller.
func NewController(
	wf *factory.WatchFactory,
	ovnClient *util.OVNClusterManagerClientset,
	networkManager networkmanager.Interface,
) *Controller {
	c := &Controller{
		uplinkClient:              ovnClient.UplinkClient,
		udnClient:                 ovnClient.UserDefinedNetworkClient,
		uplinkLister:              wf.UplinkInformer().Lister(),
		uplinkStateLister:         wf.UplinkStateInformer().Lister(),
		cudnLister:                wf.ClusterUserDefinedNetworkInformer().Lister(),
		nodeLister:                wf.NodeCoreInformer().Lister(),
		networkManager:            networkManager,
		cudnUplinkIndex:           make(map[string][]string),
		uplinkStateIdentityByName: make(map[string]uplinkStateIdentity),
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
		"clustermanager-uplink-controller",
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
		"clustermanager-uplink-state-controller",
		uplinkStateCfg,
	)

	cudnCfg := &controllerutil.ControllerConfig[udnv1.ClusterUserDefinedNetwork]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.ClusterUserDefinedNetworkInformer().Informer(),
		Lister:         c.cudnLister.List,
		Reconcile:      c.reconcileCUDN,
		ObjNeedsUpdate: cudnNeedsUpdate,
		Threadiness:    1,
	}
	c.cudnController = controllerutil.NewController(
		"clustermanager-uplink-cudn-controller",
		cudnCfg,
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
		"clustermanager-uplink-node-controller",
		nodeCfg,
	)

	networkRefCfg := &controllerutil.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   c.reconcileNetworkRef,
		Threadiness: 1,
	}
	c.networkRefController = controllerutil.NewReconciler(
		"clustermanager-uplink-network-ref-controller",
		networkRefCfg,
	)
	networkRefController := c.networkRefController
	c.networkRefReconciler = networkRefReconcilerFunc(
		func(nodeName, networkName string) {
			if nodeName == "" || networkName == "" {
				return
			}
			networkRefController.Reconcile(networkRefKey(nodeName, networkName))
		},
	)

	return c
}

// Start registers for network-reference updates, repairs stale UplinkStates,
// and starts the Uplink, UplinkState, CUDN, node, and network-reference
// reconcilers.
func (c *Controller) Start() error {
	c.networkRefID = c.networkManager.RegisterNetworkRefReconciler(c.networkRefReconciler)
	err := controllerutil.StartWithInitialSync(
		c.initialSync,
		c.uplinkController,
		c.uplinkStateController,
		c.cudnController,
		c.nodeController,
		c.networkRefController,
	)
	if err != nil {
		c.networkManager.DeRegisterNetworkRefReconciler(c.networkRefID)
		c.networkRefID = 0
		return err
	}
	klog.Infof("Cluster manager Uplink controller started")
	return nil
}

func (c *Controller) initialSync() error {
	states, err := c.uplinkStateLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list UplinkStates for initial sync: %w", err)
	}

	var errs []error
	for _, state := range states {
		uplinkName, nodeName := c.rememberUplinkStateIdentity(state)
		stale, reason, err := c.staleUplinkState(state, uplinkName, nodeName)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !stale {
			continue
		}
		klog.Infof("Deleting stale UplinkState %s during initial sync: %s",
			state.Name, reason)
		if err := c.deleteUplinkState(state.Name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Stop shuts down the Uplink controller.
func (c *Controller) Stop() {
	if c.networkRefID != 0 {
		c.networkManager.DeRegisterNetworkRefReconciler(c.networkRefID)
		c.networkRefID = 0
	}
	controllerutil.Stop(
		c.uplinkController,
		c.uplinkStateController,
		c.cudnController,
		c.nodeController,
		c.networkRefController,
	)
}

func (c *Controller) reconcileUplink(key string) error {
	uplink, err := c.uplinkLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.deleteUplinkStatesFor(key)
		}
		return fmt.Errorf("failed to get Uplink %s: %w", key, err)
	}

	referencingCUDNs, err := c.getCUDNsReferencingUplink(uplink.Name)
	if err != nil {
		return fmt.Errorf("failed to check CUDN references for Uplink %s: %w", uplink.Name, err)
	}
	referenced := len(referencingCUDNs) > 0

	if !uplink.DeletionTimestamp.IsZero() {
		return c.handleUplinkDeletion(uplink, referenced, referencingCUDNs)
	}

	if err := c.syncFinalizer(uplink, referenced); err != nil {
		return err
	}

	selected, conflicts, validationErr := c.resolveSelectedNodeConfigs(uplink)
	if validationErr != nil {
		if err := c.updateStatus(uplink,
			metav1.ConditionTrue, reasonInvalidSpec,
			"Uplink is degraded because its spec is not accepted"); err != nil {
			return err
		}
		c.reconcileCUDNNames(referencingCUDNs)
		return nil
	}

	degradedStatus, degradedReason, degradedMessage := c.degradedCondition(
		uplink,
		selected,
		conflicts,
	)
	if err := c.updateStatus(uplink,
		degradedStatus, degradedReason, degradedMessage); err != nil {
		return err
	}
	c.reconcileCUDNNames(referencingCUDNs)
	return nil
}

func (c *Controller) reconcileUplinkState(key string) error {
	uplinkState, err := c.uplinkStateLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.reconcileDeletedUplinkState(key)
		}
		return fmt.Errorf("failed to get UplinkState %s: %w", key, err)
	}

	if uplinkName, _ := c.rememberUplinkStateIdentity(uplinkState); uplinkName != "" {
		c.uplinkController.Reconcile(uplinkName)
		return c.reconcileCUDNsReferencingUplink(uplinkName)
	}

	c.uplinkController.ReconcileAll()
	return nil
}

func (c *Controller) reconcileDeletedUplinkState(key string) error {
	identity, ok := c.forgetUplinkStateIdentity(key)
	if ok && identity.uplinkName != "" {
		c.uplinkController.Reconcile(identity.uplinkName)
		return c.reconcileCUDNsReferencingUplink(identity.uplinkName)
	}

	uplinkName, ok, err := c.uplinkNameForStateName(key)
	if err != nil {
		return err
	}
	if ok {
		c.uplinkController.Reconcile(uplinkName)
		return c.reconcileCUDNsReferencingUplink(uplinkName)
	}

	c.uplinkController.ReconcileAll()
	return nil
}

func (c *Controller) reconcileNode(_ string) error {
	c.uplinkController.ReconcileAll()
	c.cudnController.ReconcileAll()
	return nil
}

func (c *Controller) reconcileCUDN(key string) error {
	cudn, err := c.cudnLister.Get(key)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get CUDN %s: %w", key, err)
		}
		uplinkNames := c.forgetCUDNUplinks(key)
		if len(uplinkNames) == 0 {
			c.reconcileAllUplinks()
			return nil
		}
		c.reconcileUplinkNames(uplinkNames)
		return nil
	}

	oldUplinkNames := c.indexCUDNUplinks(cudn.Name, cudn.Spec.Uplinks)
	if !reflect.DeepEqual(oldUplinkNames, cudn.Spec.Uplinks) {
		c.reconcileUplinkNames(oldUplinkNames)
		c.reconcileUplinkNames(cudn.Spec.Uplinks)
	}
	return c.updateCUDNUplinksReady(cudn)
}

func (c *Controller) reconcileNetworkRef(key string) error {
	_, networkName, err := parseNetworkRefKey(key)
	if err != nil {
		klog.Warningf("Skipping invalid Uplink network-ref key %q: %v", key, err)
		return nil
	}
	cudns, err := c.cudnLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list CUDNs: %w", err)
	}
	for _, cudn := range cudns {
		if util.GenerateCUDNNetworkName(cudn.Name) == networkName {
			c.cudnController.Reconcile(cudn.Name)
			break
		}
	}
	return nil
}

func (c *Controller) handleUplinkDeletion(
	uplink *uplinkv1alpha1.Uplink,
	referenced bool,
	referencingCUDNs []string,
) error {
	if referenced {
		if k8scontrollerutil.ContainsFinalizer(uplink, finalizerUplink) {
			klog.Infof("Uplink %s is still referenced by CUDNs [%s], blocking deletion",
				uplink.Name, strings.Join(referencingCUDNs, ", "))
		}
		return nil
	}

	if err := c.deleteUplinkStatesFor(uplink.Name); err != nil {
		return err
	}
	return c.removeFinalizer(uplink)
}

func (c *Controller) syncFinalizer(uplink *uplinkv1alpha1.Uplink, referenced bool) error {
	if referenced {
		return c.ensureFinalizer(uplink)
	}
	return c.removeFinalizer(uplink)
}

func (c *Controller) ensureFinalizer(uplink *uplinkv1alpha1.Uplink) error {
	if k8scontrollerutil.ContainsFinalizer(uplink, finalizerUplink) {
		return nil
	}
	finalizers := append([]string{}, uplink.Finalizers...)
	finalizers = append(finalizers, finalizerUplink)
	if err := c.applyUplinkFinalizers(uplink.Name, finalizers); err != nil {
		return fmt.Errorf("failed to add finalizer to Uplink %s: %w", uplink.Name, err)
	}
	klog.Infof("Added finalizer to Uplink %s", uplink.Name)
	return nil
}

func (c *Controller) removeFinalizer(uplink *uplinkv1alpha1.Uplink) error {
	if !k8scontrollerutil.ContainsFinalizer(uplink, finalizerUplink) {
		return nil
	}
	finalizers := make([]string, 0, len(uplink.Finalizers))
	for _, finalizer := range uplink.Finalizers {
		if finalizer != finalizerUplink {
			finalizers = append(finalizers, finalizer)
		}
	}
	if err := c.applyUplinkFinalizers(uplink.Name, finalizers); err != nil {
		return fmt.Errorf("failed to remove finalizer from Uplink %s: %w", uplink.Name, err)
	}
	klog.Infof("Removed finalizer from Uplink %s", uplink.Name)
	return nil
}

func (c *Controller) applyUplinkFinalizers(uplinkName string, finalizers []string) error {
	apply := uplinkapply.Uplink(uplinkName)
	if len(finalizers) > 0 {
		apply = apply.WithFinalizers(finalizers...)
	}
	_, err := c.uplinkClient.K8sV1alpha1().Uplinks().Apply(
		context.Background(),
		apply,
		metav1.ApplyOptions{FieldManager: fieldManager, Force: true},
	)
	return err
}

func (c *Controller) getCUDNsReferencingUplink(uplinkName string) ([]string, error) {
	cudns, err := c.cudnLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list CUDNs: %w", err)
	}
	referencing := make([]string, 0)
	for _, cudn := range cudns {
		if cudnReferencesUplink(cudn, uplinkName) {
			referencing = append(referencing, cudn.Name)
		}
	}
	sort.Strings(referencing)
	return referencing, nil
}

func (c *Controller) reconcileCUDNsReferencingUplink(uplinkName string) error {
	cudns, err := c.cudnLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list CUDNs: %w", err)
	}
	var errs []error
	for _, cudn := range cudns {
		if !cudnReferencesUplink(cudn, uplinkName) {
			continue
		}
		if err := c.updateCUDNUplinksReady(cudn); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func cudnReferencesUplink(cudn *udnv1.ClusterUserDefinedNetwork, uplinkName string) bool {
	for _, referencedUplink := range cudn.Spec.Uplinks {
		if referencedUplink == uplinkName {
			return true
		}
	}
	return false
}

func (c *Controller) indexCUDNUplinks(cudnName string, uplinkNames []string) []string {
	c.cudnUplinkIndexMu.Lock()
	defer c.cudnUplinkIndexMu.Unlock()

	oldUplinkNames := c.cudnUplinkIndex[cudnName]
	if len(uplinkNames) == 0 {
		delete(c.cudnUplinkIndex, cudnName)
		return oldUplinkNames
	}
	c.cudnUplinkIndex[cudnName] = append([]string(nil), uplinkNames...)
	return oldUplinkNames
}

func (c *Controller) forgetCUDNUplinks(cudnName string) []string {
	c.cudnUplinkIndexMu.Lock()
	defer c.cudnUplinkIndexMu.Unlock()

	uplinkNames := c.cudnUplinkIndex[cudnName]
	delete(c.cudnUplinkIndex, cudnName)
	return uplinkNames
}

func (c *Controller) reconcileUplinkNames(uplinkNames []string) {
	for _, uplinkName := range uplinkNames {
		c.uplinkController.Reconcile(uplinkName)
	}
}

func (c *Controller) reconcileCUDNNames(cudnNames []string) {
	for _, cudnName := range cudnNames {
		c.cudnController.Reconcile(cudnName)
	}
}

func (c *Controller) reconcileAllUplinks() {
	c.uplinkController.ReconcileAll()
}

func (c *Controller) rememberUplinkStateIdentity(state *uplinkv1alpha1.UplinkState) (uplinkName, nodeName string) {
	uplinkName, nodeName = uplinkutil.StateIdentity(state)
	if state == nil || state.Name == "" || (uplinkName == "" && nodeName == "") {
		return uplinkName, nodeName
	}

	c.uplinkStateIdentityMu.Lock()
	defer c.uplinkStateIdentityMu.Unlock()
	c.uplinkStateIdentityByName[state.Name] = uplinkStateIdentity{
		uplinkName: uplinkName,
		nodeName:   nodeName,
	}
	return uplinkName, nodeName
}

func (c *Controller) forgetUplinkStateIdentity(name string) (uplinkStateIdentity, bool) {
	c.uplinkStateIdentityMu.Lock()
	defer c.uplinkStateIdentityMu.Unlock()
	identity, ok := c.uplinkStateIdentityByName[name]
	delete(c.uplinkStateIdentityByName, name)
	return identity, ok
}

func (c *Controller) uplinkNameForStateName(stateName string) (string, bool, error) {
	uplinks, err := c.uplinkLister.List(labels.Everything())
	if err != nil {
		return "", false, fmt.Errorf("failed to list Uplinks for UplinkState %s: %w", stateName, err)
	}
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return "", false, fmt.Errorf("failed to list Nodes for UplinkState %s: %w", stateName, err)
	}

	var matchedUplink string
	matches := 0
	for _, uplink := range uplinks {
		for _, node := range nodes {
			if uplinkutil.StateName(uplink.Name, node.Name) != stateName {
				continue
			}
			matchedUplink = uplink.Name
			matches++
		}
	}
	return matchedUplink, matches == 1, nil
}

func (c *Controller) staleUplinkState(
	state *uplinkv1alpha1.UplinkState,
	uplinkName, nodeName string,
) (bool, string, error) {
	if uplinkName == "" || nodeName == "" {
		return true, "missing UplinkState identity", nil
	}

	uplink, err := c.uplinkLister.Get(uplinkName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return true, fmt.Sprintf("Uplink %s is missing", uplinkName), nil
		}
		return false, "", fmt.Errorf("failed to get Uplink %s for UplinkState %s: %w",
			uplinkName, state.Name, err)
	}

	node, err := c.nodeLister.Get(nodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return true, fmt.Sprintf("Node %s is missing", nodeName), nil
		}
		return false, "", fmt.Errorf("failed to get Node %s for UplinkState %s: %w",
			nodeName, state.Name, err)
	}

	selected, err := uplinkSelectsNode(uplink, node)
	if err != nil {
		return false, "", fmt.Errorf("failed to resolve Uplink %s selectors for UplinkState %s: %w",
			uplinkName, state.Name, err)
	}
	if !selected {
		return true, fmt.Sprintf("Uplink %s no longer selects Node %s",
			uplinkName, nodeName), nil
	}
	return false, "", nil
}

func uplinkSelectsNode(uplink *uplinkv1alpha1.Uplink, node *corev1.Node) (bool, error) {
	for i, nodeConfig := range uplink.Spec.NodeConfigs {
		selector, err := metav1.LabelSelectorAsSelector(&nodeConfig.NodeSelector)
		if err != nil {
			return false, fmt.Errorf("nodeConfig %d has invalid nodeSelector: %w",
				i, err)
		}
		if selector.Matches(labels.Set(node.Labels)) {
			return true, nil
		}
	}
	return false, nil
}

func (c *Controller) resolveSelectedNodeConfigs(
	uplink *uplinkv1alpha1.Uplink,
) (map[string]uplinkv1alpha1.UplinkNodeConfig, []string, error) {
	selected := map[string]uplinkv1alpha1.UplinkNodeConfig{}
	conflictSet := sets.New[string]()

	for i, nodeConfig := range uplink.Spec.NodeConfigs {
		selector, err := metav1.LabelSelectorAsSelector(&nodeConfig.NodeSelector)
		if err != nil {
			return nil, nil, fmt.Errorf("nodeConfig %d has invalid nodeSelector: %w", i, err)
		}
		nodes, err := c.nodeLister.List(selector)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to list nodes for nodeConfig %d: %w", i, err)
		}
		for _, node := range nodes {
			if conflictSet.Has(node.Name) {
				continue
			}
			if _, alreadySelected := selected[node.Name]; alreadySelected {
				conflictSet.Insert(node.Name)
				delete(selected, node.Name)
				continue
			}
			selected[node.Name] = nodeConfig
		}
	}

	return selected, sets.List(conflictSet), nil
}

func (c *Controller) deleteUplinkStatesFor(uplinkName string) error {
	states, err := c.uplinkStateLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list UplinkStates: %w", err)
	}
	for _, state := range states {
		if !uplinkutil.StateBelongsTo(state, uplinkName) {
			continue
		}
		if err := c.deleteUplinkState(state.Name); err != nil {
			return err
		}
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

func (c *Controller) degradedCondition(
	uplink *uplinkv1alpha1.Uplink,
	selected map[string]uplinkv1alpha1.UplinkNodeConfig,
	conflicts []string,
) (metav1.ConditionStatus, string, string) {
	if len(conflicts) > 0 {
		return metav1.ConditionTrue, reasonNodeSelectorOverlap,
			fmt.Sprintf("multiple Uplink nodeConfigs select nodes: %s",
				strings.Join(conflicts, ","))
	}
	if len(selected) == 0 {
		return metav1.ConditionTrue, reasonNoMatchingNodes,
			"no nodes match any Uplink nodeConfig"
	}

	nodeNames := make([]string, 0, len(selected))
	for nodeName := range selected {
		nodeNames = append(nodeNames, nodeName)
	}
	sort.Strings(nodeNames)

	for _, nodeName := range nodeNames {
		entry := selected[nodeName]
		stateName := uplinkutil.StateName(uplink.Name, nodeName)
		state, err := c.uplinkStateLister.Get(stateName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return metav1.ConditionTrue, reasonMissingUplinkState,
					fmt.Sprintf("UplinkState %s is missing", stateName)
			}
			return metav1.ConditionTrue, reasonMissingUplinkState,
				fmt.Sprintf("failed to get UplinkState %s: %v", stateName, err)
		}

		stateUplink, stateNode := uplinkutil.StateIdentity(state)
		if stateUplink != uplink.Name {
			return metav1.ConditionTrue, reasonUplinkStateIdentityError,
				fmt.Sprintf("UplinkState %s reports uplinkName %q",
					stateName, stateUplink)
		}
		if stateNode != nodeName {
			return metav1.ConditionTrue, reasonUplinkStateIdentityError,
				fmt.Sprintf("UplinkState %s reports nodeName %q",
					stateName, stateNode)
		}

		if !resolvedUplinkReady(state, entry) {
			reason := propagatedUplinkStateFailureReason(state, entry)
			if reason == "" {
				reason = reasonUplinkStateNotReady
			}
			return metav1.ConditionTrue, reason,
				fmt.Sprintf("UplinkState %s is not ready", stateName)
		}
	}
	return metav1.ConditionFalse, reasonReady, "all selected nodes are ready"
}

func resolvedUplinkReady(state *uplinkv1alpha1.UplinkState, nodeConfig uplinkv1alpha1.UplinkNodeConfig) bool {
	if state.Status.Type != nodeConfig.Type {
		return false
	}
	if state.Status.HostInterfaceName != nodeConfig.HostInterfaceName {
		return false
	}
	cond := meta.FindStatusCondition(
		state.Status.Conditions,
		uplinkv1alpha1.UplinkStateConditionReady,
	)
	return cond != nil && cond.Status == metav1.ConditionTrue
}

func propagatedUplinkStateFailureReason(
	state *uplinkv1alpha1.UplinkState,
	nodeConfig uplinkv1alpha1.UplinkNodeConfig,
) string {
	if state.Status.Type != nodeConfig.Type ||
		state.Status.HostInterfaceName != nodeConfig.HostInterfaceName {
		return ""
	}
	condition := meta.FindStatusCondition(
		state.Status.Conditions,
		uplinkv1alpha1.UplinkStateConditionReady,
	)
	if condition == nil {
		return ""
	}
	switch condition.Reason {
	case uplinkv1alpha1.UplinkStateReasonVRFAttachmentFailed:
		return reasonUplinkVRFAttachmentFailed
	case uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed:
		return reasonUplinkBridgeMappingFailed
	default:
		return ""
	}
}

func cudnUplinkStateNotReadyReason(
	state *uplinkv1alpha1.UplinkState,
	nodeConfig uplinkv1alpha1.UplinkNodeConfig,
) string {
	if reason := propagatedUplinkStateFailureReason(state, nodeConfig); reason != "" {
		return reason
	}
	return reasonUplinkNotReadyForNode
}

func (c *Controller) updateCUDNUplinksReady(cudn *udnv1.ClusterUserDefinedNetwork) error {
	condition := c.cudnUplinksReadyCondition(cudn)
	existing := meta.FindStatusCondition(cudn.Status.Conditions, condition.Type)
	if existing != nil &&
		existing.Status == condition.Status &&
		existing.Reason == condition.Reason &&
		existing.Message == condition.Message {
		return nil
	}

	_, err := c.udnClient.K8sV1().ClusterUserDefinedNetworks().ApplyStatus(
		context.Background(),
		udnapply.ClusterUserDefinedNetwork(cudn.Name).WithStatus(
			udnapply.ClusterUserDefinedNetworkStatus().
				WithConditions(conditionApply(
					cudn.Status.Conditions,
					condition.Type,
					condition.Status,
					condition.Reason,
					condition.Message,
				)),
		),
		metav1.ApplyOptions{
			FieldManager: fieldManager,
			Force:        true,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to update CUDN %s UplinksReady condition: %w",
			cudn.Name, err)
	}
	return nil
}

func (c *Controller) cudnUplinksReadyCondition(cudn *udnv1.ClusterUserDefinedNetwork) metav1.Condition {
	status, reason, message := c.cudnUplinksReadyStatus(cudn)
	return metav1.Condition{
		Type:               conditionTypeUplinksReady,
		Status:             status,
		Reason:             reason,
		Message:            trimConditionMessage(message),
		LastTransitionTime: metav1.NewTime(time.Now()),
	}
}

func (c *Controller) cudnUplinksReadyStatus(
	cudn *udnv1.ClusterUserDefinedNetwork,
) (metav1.ConditionStatus, string, string) {
	if len(cudn.Spec.Uplinks) == 0 {
		return metav1.ConditionTrue, reasonUplinksReady,
			"CUDN does not reference an Uplink"
	}
	if config.Gateway.Mode != config.GatewayModeShared {
		return metav1.ConditionFalse, reasonUplinkUnsupportedGatewayMode,
			fmt.Sprintf("Uplink requires shared gateway mode, got %s",
				config.Gateway.Mode)
	}
	if cudn.Spec.Network.Transport == udnv1.TransportOptionEVPN {
		return metav1.ConditionFalse, reasonUplinkUnsupportedTransport,
			"Uplink is not supported with EVPN transport"
	}

	activeNodes, err := c.activeCUDNNodes(cudn)
	if err != nil {
		return metav1.ConditionFalse, reasonUplinkNotReadyForNode,
			fmt.Sprintf("failed to list active nodes for CUDN %s: %v",
				cudn.Name, err)
	}
	failures := make([]cudnUplinkFailure, 0)

	for _, uplinkName := range cudn.Spec.Uplinks {
		uplink, err := c.uplinkLister.Get(uplinkName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return metav1.ConditionFalse, reasonUplinkNotFound,
					fmt.Sprintf("Uplink %s is missing", uplinkName)
			}
			return metav1.ConditionFalse, reasonUplinkNotFound,
				fmt.Sprintf("failed to get Uplink %s: %v", uplinkName, err)
		}

		selected, conflicts, err := c.resolveSelectedNodeConfigs(uplink)
		if err != nil {
			return metav1.ConditionFalse, reasonUplinkNotReadyForNode,
				fmt.Sprintf("Uplink %s has invalid nodeConfigs: %v",
					uplinkName, err)
		}
		conflictSet := sets.New[string](conflicts...)

		for _, node := range activeNodes {
			if conflictSet.Has(node.Name) {
				failures = append(failures, cudnUplinkFailure{
					uplink: uplinkName,
					reason: reasonUplinkOverlapOnNode,
					node:   node.Name,
				})
				continue
			}
			nodeConfig, ok := selected[node.Name]
			if !ok {
				failures = append(failures, cudnUplinkFailure{
					uplink: uplinkName,
					reason: reasonUplinkNotFoundForNode,
					node:   node.Name,
				})
				continue
			}
			stateName := uplinkutil.StateName(uplinkName, node.Name)
			state, err := c.uplinkStateLister.Get(stateName)
			if err != nil {
				failures = append(failures, cudnUplinkFailure{
					uplink: uplinkName,
					reason: reasonUplinkNotReadyForNode,
					node:   node.Name,
				})
				continue
			}
			stateUplink, stateNode := uplinkutil.StateIdentity(state)
			if stateUplink != uplinkName || stateNode != node.Name {
				failures = append(failures, cudnUplinkFailure{
					uplink: uplinkName,
					reason: reasonUplinkNotReadyForNode,
					node:   node.Name,
				})
				continue
			}
			if !resolvedUplinkReady(state, nodeConfig) {
				failures = append(failures, cudnUplinkFailure{
					uplink: uplinkName,
					reason: cudnUplinkStateNotReadyReason(state, nodeConfig),
					node:   node.Name,
				})
			}
		}
	}
	if len(failures) > 0 {
		reason, message := summarizeCUDNUplinkFailures(failures)
		return metav1.ConditionFalse, reason, message
	}
	return metav1.ConditionTrue, reasonUplinksReady,
		"Uplinks are ready for all active CUDN nodes"
}

func summarizeCUDNUplinkFailures(failures []cudnUplinkFailure) (string, string) {
	reasonPriority := []string{
		reasonUplinkOverlapOnNode,
		reasonUplinkBridgeMappingFailed,
		reasonUplinkVRFAttachmentFailed,
		reasonUplinkNotFoundForNode,
		reasonUplinkNotReadyForNode,
	}
	counts := make(map[string]int, len(reasonPriority))
	nodeSet := sets.New[string]()
	uplinkSet := sets.New[string]()
	for _, failure := range failures {
		counts[failure.reason]++
		if failure.node != "" {
			nodeSet.Insert(failure.node)
		}
		if failure.uplink != "" {
			uplinkSet.Insert(failure.uplink)
		}
	}

	selectedReason := failures[0].reason
	for _, reason := range reasonPriority {
		if counts[reason] > 0 {
			selectedReason = reason
			break
		}
	}

	reasonParts := make([]string, 0, len(counts))
	for _, reason := range reasonPriority {
		if counts[reason] == 0 {
			continue
		}
		reasonParts = append(reasonParts, fmt.Sprintf("%s=%d", reason, counts[reason]))
	}

	nodes := sets.List(nodeSet)
	const maxNodeExamples = 5
	if len(nodes) > maxNodeExamples {
		nodes = nodes[:maxNodeExamples]
	}

	uplinks := sets.List(uplinkSet)
	const maxUplinkExamples = 3
	if len(uplinks) > maxUplinkExamples {
		uplinks = uplinks[:maxUplinkExamples]
	}

	return selectedReason, fmt.Sprintf(
		"%d active node/uplink readiness check(s) failed; reasons: %s; nodes: %s; uplinks: %s",
		len(failures),
		strings.Join(reasonParts, ", "),
		strings.Join(nodes, ", "),
		strings.Join(uplinks, ", "),
	)
}

func (c *Controller) activeCUDNNodes(cudn *udnv1.ClusterUserDefinedNetwork) ([]*corev1.Node, error) {
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	networkName := util.GenerateCUDNNetworkName(cudn.Name)
	activeNodes := make([]*corev1.Node, 0, len(nodes))
	for _, node := range nodes {
		if c.networkManager.NodeHasNetwork(node.Name, networkName) {
			activeNodes = append(activeNodes, node)
		}
	}
	sort.Slice(activeNodes, func(i, j int) bool {
		return activeNodes[i].Name < activeNodes[j].Name
	})
	return activeNodes, nil
}

func (c *Controller) updateStatus(
	uplink *uplinkv1alpha1.Uplink,
	degradedStatus metav1.ConditionStatus,
	degradedReason string,
	degradedMessage string,
) error {
	degraded := conditionApply(
		uplink.Status.Conditions,
		uplinkv1alpha1.UplinkConditionDegraded,
		degradedStatus,
		degradedReason,
		degradedMessage,
	)

	if statusConditionEqual(
		meta.FindStatusCondition(
			uplink.Status.Conditions,
			uplinkv1alpha1.UplinkConditionDegraded,
		),
		degraded,
	) && meta.FindStatusCondition(
		uplink.Status.Conditions,
		obsoleteUplinkConditionReferenced,
	) == nil {
		return nil
	}

	_, err := c.uplinkClient.K8sV1alpha1().Uplinks().ApplyStatus(
		context.Background(),
		uplinkapply.Uplink(uplink.Name).WithStatus(
			uplinkapply.UplinkStatus().WithConditions(degraded),
		),
		metav1.ApplyOptions{
			FieldManager: fieldManager,
			Force:        true,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to update Uplink %s status: %w", uplink.Name, err)
	}
	return nil
}

func conditionApply(
	existing []metav1.Condition,
	conditionType string,
	status metav1.ConditionStatus,
	reason, message string,
) *metaapply.ConditionApplyConfiguration {
	condition := metaapply.Condition().
		WithType(conditionType).
		WithStatus(status).
		WithReason(reason).
		WithMessage(trimConditionMessage(message))

	now := metav1.NewTime(time.Now())
	if current := meta.FindStatusCondition(existing, conditionType); current != nil &&
		current.Status == status {
		now = current.LastTransitionTime
	}
	return condition.WithLastTransitionTime(now)
}

func statusConditionEqual(existing *metav1.Condition, next *metaapply.ConditionApplyConfiguration) bool {
	return existing != nil &&
		next != nil &&
		next.Type != nil &&
		next.Status != nil &&
		next.Reason != nil &&
		next.Message != nil &&
		existing.Type == *next.Type &&
		existing.Status == *next.Status &&
		existing.Reason == *next.Reason &&
		existing.Message == *next.Message
}

func trimConditionMessage(message string) string {
	const maxMessageLen = 32768
	if len(message) >= maxMessageLen {
		return message[:maxMessageLen-1]
	}
	return message
}

type networkRefReconcilerFunc func(nodeName, networkName string)

func (f networkRefReconcilerFunc) Reconcile(nodeName, networkName string) {
	f(nodeName, networkName)
}

func networkRefKey(nodeName, networkName string) string {
	return nodeName + networkRefSeparator + networkName
}

func parseNetworkRefKey(key string) (nodeName, networkName string, err error) {
	nodeName, networkName, found := strings.Cut(key, networkRefSeparator)
	if !found || nodeName == "" || networkName == "" {
		return "", "", fmt.Errorf("invalid network-ref key %q", key)
	}
	return nodeName, networkName, nil
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
		!reflect.DeepEqual(oldObj.Annotations, newObj.Annotations)
}

func cudnNeedsUpdate(oldObj, newObj *udnv1.ClusterUserDefinedNetwork) bool {
	if oldObj == nil {
		return newObj != nil
	}
	if newObj == nil {
		return true
	}
	return !reflect.DeepEqual(oldObj.Spec.Uplinks, newObj.Spec.Uplinks) ||
		oldObj.DeletionTimestamp.IsZero() != newObj.DeletionTimestamp.IsZero()
}

func nodeNeedsUpdate(oldObj, newObj *corev1.Node) bool {
	if oldObj == nil {
		return true
	}
	return !reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		oldObj.DeletionTimestamp.IsZero() != newObj.DeletionTimestamp.IsZero()
}
