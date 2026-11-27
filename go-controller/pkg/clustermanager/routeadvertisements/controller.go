// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeadvertisements

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	nadtypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"
	frrclientset "github.com/metallb/frr-k8s/pkg/client/clientset/versioned"
	frrlisters "github.com/metallb/frr-k8s/pkg/client/listers/api/v1beta1"

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
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	eiptypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressiplisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/listers/egressip/v1"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/applyconfiguration/routeadvertisements/v1"
	raclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	ralisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/listers/routeadvertisements/v1"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	vteplisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/listers/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	generateName          = "ovnk-generated-"
	fieldManager          = "clustermanager-routeadvertisements-controller"
	conditionTypeAccepted = "Accepted"
	conditionTypeDegraded = "Degraded"
	cacheRecheckInterval  = 2 * time.Second
	// Maximum concurrent FRRConfiguration writes within one RA reconciliation.
	frrConfigurationWriteWorkers = 4
	// rawConfigPriority is set to an arbitrary value that still allows users to
	// override if needed.
	rawConfigPriority = 10

	// reconcile outcomes, used as the "result" label of the reconcile duration
	// metric
	reconcileResultSuccess     = "success"
	reconcileResultError       = "error"
	reconcileResultConfigError = "config_error"
	reconcileResultPending     = "pending"

	// generated FRRConfiguration operations, used as the "op" label of the
	// write counter
	frrConfigOpCreate    = "create"
	frrConfigOpUpdate    = "update"
	frrConfigOpDelete    = "delete"
	frrConfigOpUnchanged = "unchanged"
)

var (
	errConfig      = errors.New("configuration error")
	errPending     = errors.New("configuration pending")
	errInvalidVTEP = fmt.Errorf("%w: invalid VTEP annotation", errConfig)
	// Node annotations are operational state, not invalid RA/template input.
	errNodeNotReady = fmt.Errorf("%w: node configuration unavailable", errPending)
	cudnController  = userdefinednetworkv1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetwork")
)

// Controller reconciles RouteAdvertisements
type Controller struct {
	wf *factory.WatchFactory

	eipLister         egressiplisters.EgressIPLister
	frrLister         frrlisters.FRRConfigurationLister
	nadLister         nadlisters.NetworkAttachmentDefinitionLister
	nodeLister        corelisters.NodeLister
	raLister          ralisters.RouteAdvertisementsLister
	namespaceLister   corelisters.NamespaceLister
	uplinkStateLister uplinklisters.UplinkStateLister
	vtepLister        vteplisters.VTEPLister

	frrClient frrclientset.Interface
	nadClient nadclientset.Interface
	raClient  raclientset.Interface

	eipController         controllerutil.Controller
	frrController         controllerutil.Controller
	nadController         controllerutil.Controller
	nodeController        controllerutil.Controller
	raController          controllerutil.Controller
	nsController          controllerutil.Controller
	uplinkStateController controllerutil.Controller

	nm networkmanager.Interface
	// networkRefReconcilerID identifies our registration with the network
	// manager for network activity change notifications
	networkRefReconcilerID uint64

	// raNetworks caches the networks each RouteAdvertisements selects, so
	// that a network activity change only reconciles the RouteAdvertisements
	// selecting that network
	raNetworksLock sync.RWMutex
	raNetworks     map[string]sets.Set[string]

	pendingFRR, pendingNAD *pendingWrites
}

// networkRefReconcilerFunc adapts a function to the
// networkmanager.NetworkRefReconciler interface.
type networkRefReconcilerFunc func(node, networkName string)

func (f networkRefReconcilerFunc) Reconcile(node, networkName string) { f(node, networkName) }

// setRANetworks caches the networks selected by a RouteAdvertisements.
func (c *Controller) setRANetworks(ra string, networks []string) {
	c.raNetworksLock.Lock()
	defer c.raNetworksLock.Unlock()
	c.raNetworks[ra] = sets.New(networks...)
}

// deleteRANetworks removes a RouteAdvertisements from the selected networks
// cache.
func (c *Controller) deleteRANetworks(ra string) {
	c.raNetworksLock.Lock()
	defer c.raNetworksLock.Unlock()
	delete(c.raNetworks, ra)
}

// getRAsForNetwork returns the RouteAdvertisements known to select the
// network. A RouteAdvertisements not in the cache has not been reconciled
// yet: its pending reconcile will read the current activity state of its
// networks, so it doesn't need this notification.
func (c *Controller) getRAsForNetwork(network string) []string {
	c.raNetworksLock.RLock()
	defer c.raNetworksLock.RUnlock()
	var ras []string
	for ra, networks := range c.raNetworks {
		if networks.Has(network) {
			ras = append(ras, ra)
		}
	}
	return ras
}

// NewController builds a controller that reconciles RouteAdvertisements
func NewController(
	nm networkmanager.Interface,
	wf *factory.WatchFactory,
	ovnClient *util.OVNClusterManagerClientset,
) *Controller {
	c := &Controller{
		wf:              wf,
		frrLister:       wf.FRRConfigurationsInformer().Lister(),
		nadLister:       wf.NADInformer().Lister(),
		nodeLister:      wf.NodeCoreInformer().Lister(),
		raLister:        wf.RouteAdvertisementsInformer().Lister(),
		namespaceLister: wf.NamespaceInformer().Lister(),
		frrClient:       ovnClient.FRRClient,
		nadClient:       ovnClient.NetworkAttchDefClient,
		raClient:        ovnClient.RouteAdvertisementsClient,
		nm:              nm,
		raNetworks:      map[string]sets.Set[string]{},
	}
	c.pendingFRR = newPendingWrites(wf.FRRConfigurationsInformer().Informer().GetStore())
	c.pendingNAD = newPendingWrites(wf.NADInformer().Informer().GetStore())
	if util.IsUplinkEnabled() {
		c.uplinkStateLister = wf.UplinkStateInformer().Lister()
	}

	raConfig := &controllerutil.ControllerConfig[ratypes.RouteAdvertisements]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcile,
		Threadiness:    4,
		Informer:       wf.RouteAdvertisementsInformer().Informer(),
		Lister:         wf.RouteAdvertisementsInformer().Lister().List,
		ObjNeedsUpdate: c.raNeedsUpdate,
		OnDelete:       c.onRADelete,
		HandleError:    c.handleReconcileError,
	}
	c.raController = controllerutil.NewController("clustermanager routeadvertisements controller", raConfig)

	frrConfig := &controllerutil.ControllerConfig[frrtypes.FRRConfiguration]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileFRRConfiguration,
		Threadiness:    1,
		Informer:       wf.FRRConfigurationsInformer().Informer(),
		Lister:         wf.FRRConfigurationsInformer().Lister().List,
		ObjNeedsUpdate: c.frrConfigurationNeedsUpdate,
	}
	c.frrController = controllerutil.NewController("clustermanager routeadvertisements frrconfiguration controller", frrConfig)

	nadConfig := &controllerutil.ControllerConfig[nadtypes.NetworkAttachmentDefinition]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileNAD,
		Threadiness:    1,
		Informer:       wf.NADInformer().Informer(),
		Lister:         wf.NADInformer().Lister().List,
		ObjNeedsUpdate: c.nadNeedsUpdate,
	}
	c.nadController = controllerutil.NewController("clustermanager routeadvertisements nad controller", nadConfig)

	nodeConfig := &controllerutil.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      func(_ context.Context, _ string) error { c.raController.ReconcileAll(); return nil },
		Threadiness:    1,
		Informer:       wf.NodeCoreInformer().Informer(),
		Lister:         wf.NodeCoreInformer().Lister().List,
		ObjNeedsUpdate: nodeNeedsUpdate,
	}
	c.nodeController = controllerutil.NewController("clustermanager routeadvertisements node controller", nodeConfig)

	if util.IsUplinkEnabled() {
		uplinkStateConfig := &controllerutil.ControllerConfig[uplinkv1alpha1.UplinkState]{
			RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:      func(_ context.Context, _ string) error { c.raController.ReconcileAll(); return nil },
			Threadiness:    1,
			Informer:       wf.UplinkStateInformer().Informer(),
			Lister:         c.uplinkStateLister.List,
			ObjNeedsUpdate: uplinkStateNeedsUpdate,
		}
		c.uplinkStateController = controllerutil.NewController(
			"clustermanager routeadvertisements uplinkstate controller",
			uplinkStateConfig,
		)
	}

	if config.OVNKubernetesFeature.EnableEgressIP {
		c.eipLister = wf.EgressIPInformer().Lister()

		eipConfig := &controllerutil.ControllerConfig[eiptypes.EgressIP]{
			RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:      c.reconcileEgressIPs,
			Threadiness:    1,
			Informer:       wf.EgressIPInformer().Informer(),
			Lister:         wf.EgressIPInformer().Lister().List,
			ObjNeedsUpdate: egressIPNeedsUpdate,
		}
		c.eipController = controllerutil.NewController("clustermanager routeadvertisements egressip controller", eipConfig)

		nsConfig := &controllerutil.ControllerConfig[corev1.Namespace]{
			RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:      c.reconcileEgressIPs,
			Threadiness:    1,
			Informer:       wf.NamespaceInformer().Informer(),
			Lister:         wf.NamespaceInformer().Lister().List,
			ObjNeedsUpdate: nsNeedsUpdate,
		}
		c.nsController = controllerutil.NewController("clustermanager routeadvertisements namespace controller", nsConfig)
	}

	if util.IsEVPNEnabled() {
		c.vtepLister = wf.VTEPInformer().Lister()
	}

	return c
}

func (c *Controller) Start() error {
	defer klog.Infof("Cluster manager routeadvertisements started")
	// reconcile when a network goes active or inactive on a node: some of
	// these changes, like a network going active again during the deletion
	// grace period, update no object we watch
	c.networkRefReconcilerID = c.nm.RegisterNetworkRefReconciler(networkRefReconcilerFunc(func(_, networkName string) {
		ras := c.getRAsForNetwork(networkName)
		if len(ras) == 0 {
			klog.V(5).Infof("No RouteAdvertisements select network %s, ignoring its activity change", networkName)
			return
		}
		for _, ra := range ras {
			c.raController.Reconcile(ra)
		}
	}))
	controllers := []controllerutil.Reconciler{
		c.frrController,
		c.nadController,
		c.nodeController,
		c.raController,
	}
	if util.IsUplinkEnabled() {
		controllers = append(controllers, c.uplinkStateController)
	}
	if config.OVNKubernetesFeature.EnableEgressIP {
		controllers = append(controllers, c.eipController, c.nsController)
	}
	return controllerutil.Start(controllers...)
}

func (c *Controller) Stop() {
	c.nm.DeRegisterNetworkRefReconciler(c.networkRefReconcilerID)
	controllers := []controllerutil.Reconciler{
		c.frrController,
		c.nadController,
		c.nodeController,
		c.raController,
	}
	if util.IsUplinkEnabled() {
		controllers = append(controllers, c.uplinkStateController)
	}
	if config.OVNKubernetesFeature.EnableEgressIP {
		controllers = append(controllers, c.eipController, c.nsController)
	}
	controllerutil.Stop(controllers...)
	klog.Infof("Cluster manager routeadvertisements stopped")
}

func (c *Controller) ReconcileNetwork(_ string, old, new util.NetInfo) {
	// This controller already listens on NAD events but there is two additional
	// scenarios we need to cover for:
	// - for newly created networks, we need to wait until network manager is
	// aware of them.
	// - if the namespaces served by a network change.
	oldNamespaces, newNamespaces := sets.New[string](), sets.New[string]()
	if old != nil {
		oldNamespaces.Insert(old.GetNADNamespaces()...)
	}
	if new != nil {
		newNamespaces.Insert(new.GetNADNamespaces()...)
	}
	if new != nil && !newNamespaces.Equal(oldNamespaces) {
		// we use one of the NADs of the network to reconcile it
		nads := c.nm.GetNADKeysForNetwork(new.GetNetworkName())
		if len(nads) > 0 {
			c.nadController.Reconcile(nads[0])
		}
		// if the namespaces served by a network changed, it is possible that
		// those namespaces are served or no longer served by the default
		// network, so reconcile it as well
		c.nadController.Reconcile(config.Default.ClusterDefaultNADName)
	}
}

// reportedReconcileError carries a retryable failure whose status was already
// published. A node error alone does not prove this: cleanup can fail before
// reconciliation reaches the status update.
type reportedReconcileError struct {
	error
}

func (e *reportedReconcileError) Unwrap() error { return e.error }

func (c *Controller) handleReconcileError(ctx context.Context, key string, errorstatus error) error {
	// Preserve the published acceptance/degradation decision instead of
	// replacing it with a global failure when retries are exhausted.
	var reported *reportedReconcileError
	if errors.As(errorstatus, &reported) {
		return nil
	}
	ra, err := c.raLister.Get(key)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot get RouteAdvertisements %q to report error %v in status: %v",
			key,
			errorstatus,
			err,
		)
	}

	return c.updateRAStatus(ctx, ra, false, frrGenerationResult{err: errorstatus})
}

// Reconcile RouteAdvertisements. For each selected FRRConfiguration and node,
// another FRRConfiguration might be generated:
//
// - If pod network advertisements are enabled, the generated FRRConfiguration
// will announce from the node the selected network prefixes for that node on
// the matching target VRFs.
//
// - If EgressIP advertisements are enabled, the generated FRRConfiguration will
// announce from the node the EgressIPs allocated to it on the matching target
// VRFs. Selected EgressIP are those that serve the same namespaces as the
// selected networks. Target VRF `auto` is not supported for EgressIPs.
//
// - If pod network advertisements are enabled, the generated FRRConfiguration
// will import the target VRFs on the selected networks as required.
//
// - The generated FRRConfiguration will be labeled with the RouteAdvertisements
// name and annotated with an internal key to facilitate updating it when
// needed.
//
// The controller will also annotate the NADs of the selected networks with the
// RouteAdvertisements that select them to facilitate processing for downstream
// zone/node controllers.
//
// Finally, it will update the status of the RouteAdvertisements.
//
// The controller processes selected events of RouteAdvertisements,
// FRRConfigurations, Nodes, EgressIPs, NADs and namespaces.
func (c *Controller) reconcile(ctx context.Context, name string) error {
	// The cache must show this RA's own last writes before we compare it
	// with the desired state again. Their events requeue the RA.
	if c.pendingFRR.waiting(name) || c.pendingNAD.waiting(name) {
		// A relist may skip an event. Recheck even if no handler wakes us.
		c.raController.ReconcileAfter(name, cacheRecheckInterval)
		return nil
	}
	startTime := time.Now()
	result := reconcileResultSuccess
	deleted := false
	klog.V(5).Infof("Syncing routeadvertisements %q", name)
	defer func() {
		if deleted {
			// drop the per-resource timeseries last: the cleanup reconcile
			// below still records against them
			metrics.DeleteRouteAdvertisementMetrics(name)
		} else {
			metrics.RecordRouteAdvertisementReconcile(name, result, time.Since(startTime))
		}
		klog.V(4).Infof("Finished syncing routeadvertisements %q, took %v", name, time.Since(startTime))
	}()

	ra, err := c.raLister.Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		result = reconcileResultError
		return fmt.Errorf("failed to get RouteAdvertisements %q: %w", name, err)
	}

	if ra == nil {
		deleted = true
		c.deleteRANetworks(name)
	}

	generated := c.generateFRRConfigurations(ctx, ra)
	hadUpdates, err := c.reconcileRouteAdvertisements(ctx, name, ra, &generated)
	if err != nil {
		result = reconcileResultError
		return fmt.Errorf("failed to reconcile RouteAdvertisements %q: %w", name, err)
	}
	switch failure := generated.failureError(); {
	case failure == nil:
	case errors.Is(failure, errConfig):
		result = reconcileResultConfigError
	case errors.Is(failure, errPending):
		result = reconcileResultPending
	default:
		result = reconcileResultError
	}
	if err := c.updateRAStatus(ctx, ra, hadUpdates, generated); err != nil {
		result = reconcileResultError
		return err
	}
	// Pending or invalid input on one node must not suppress another node's
	// transient failure. Keep all diagnostics when returning for retry.
	if generated.hasRetryableNodeError() {
		result = reconcileResultError
		return &reportedReconcileError{generated.failureError()}
	}
	return nil
}

// A template that failed generation has no desired object on that node. Withdraw
// its old routes while continuing to publish the other templates. A rejected RA
// withdraws all its generated objects before dropping its NAD references.
func (c *Controller) reconcileRouteAdvertisements(ctx context.Context, name string, ra *ratypes.RouteAdvertisements, result *frrGenerationResult) (bool, error) {
	cfgErr := result.failureError()
	if result.err != nil && !errors.Is(result.err, errConfig) && !errors.Is(result.err, errPending) {
		if len(result.invalidVTEPNodes) == 0 {
			return false, result.err
		}
		// A shared read failed after we identified invalid VTEPs. Withdraw
		// those nodes and leave the others in place until generation succeeds.
		changed, err := c.deleteIncompleteVTEPConfigurations(ctx, name, result.invalidVTEPNodes)
		return changed, errors.Join(result.err, err)
	}
	if result.err != nil {
		// Invalid RA input and missing shared inputs withdraw the entire RA.
		result.frrConfigs, result.nads = nil, nil
	}
	if len(result.invalidVTEPNodes) > 0 && len(result.frrConfigs) == 0 {
		result.nads = nil
	}
	var updates []nadUpdate
	var nadErr error
	intent := util.NewRouteAdvertisementsIntent(ra)
	if len(result.nads) > 0 {
		updates, nadErr = c.getNADUpdates(name, intent, result.nads)
	}
	hadFRRUpdates, frrErr := c.updateFRRConfigurations(ctx, name, result)
	if frrErr != nil {
		local, ok := frrErr.(*nodeGenerationError)
		if !ok || len(result.nads) == 0 || result.vtepErr != nil || nadErr != nil {
			return hadFRRUpdates, fmt.Errorf("failed updating FRRConfigurations for RouteAdvertisements %q (generation: %v): %w", name, cfgErr, errors.Join(frrErr, nadErr))
		}
		// A failed delete can leave blue advertised on one node while green
		// is ready on another. Keep blue's intent and let green publish its own.
		result.nodeErrors = append(result.nodeErrors, local.errs...)
	}
	if len(result.nads) == 0 {
		// Drop the NAD references only once the FRR cleanup succeeded.
		updates, nadErr = c.getNADUpdates(name, nil, nil)
	} else if frrErr != nil || result.failedFRRUpdate {
		var err error
		updates, err = c.independentNADUpdates(name, intent, updates)
		nadErr = errors.Join(nadErr, err)
	}
	hadNADUpdates, err := c.applyNADUpdates(ctx, name, updates)
	if err := errors.Join(nadErr, err); err != nil {
		return hadFRRUpdates || hadNADUpdates, fmt.Errorf("failed annotating NADs for RouteAdvertisements %q (generation: %v): %w", name, cfgErr, errors.Join(frrErr, err))
	}
	return hadFRRUpdates || hadNADUpdates, nil
}

// frrGenerationResult holds the configurations we can publish and the reasons
// other routes could not be generated.
type frrGenerationResult struct {
	frrConfigs []*frrtypes.FRRConfiguration
	// Selected NADs keep their RA references during partial failures. An empty
	// set withdraws this RA's references after its FRR cleanup succeeds.
	nads []*nadtypes.NetworkAttachmentDefinition
	// A shared error affects the whole RA. Invalid or pending input withdraws
	// its exports. A failed read leaves them in place except on invalid VTEP nodes.
	err error
	// Node and template failures leave independent exports eligible to publish.
	// On their own they degrade the RA while it remains accepted.
	nodeErrors []error
	// A rejected update leaves the old export installed. Hold NAD withdrawals
	// that depend on that export changing. A failed create has no old export.
	failedFRRUpdate bool
	// A VTEP conflict affects RA acceptance. Keep the invalid nodes separately
	// so their exports can be withdrawn even if a later shared read fails.
	invalidVTEPNodes sets.Set[string]
	vtepErr          error
	// Wait for the old owner to withdraw before advertising a moved EgressIP.
	// Non-nil whenever EgressIP is enabled, even before an address is assigned.
	egressIPOwners map[netip.Prefix]string
}

func (r frrGenerationResult) failureError() error {
	var nodeErr error
	if len(r.nodeErrors) > 0 {
		nodeErr = &nodeGenerationError{errs: r.nodeErrors}
	}
	return errors.Join(r.vtepErr, r.err, nodeErr)
}

func (r frrGenerationResult) partialNodeFailure() bool {
	return len(r.nodeErrors) > 0 && r.err == nil && r.vtepErr == nil
}

func (r frrGenerationResult) hasRetryableNodeError() bool {
	return slices.ContainsFunc(r.nodeErrors, func(err error) bool {
		return !errors.Is(err, errConfig) && !errors.Is(err, errPending)
	})
}

// nodeGenerationError bounds diagnostics without losing the underlying errors.
// Reconciliation state belongs to frrGenerationResult, not this error wrapper.
type nodeGenerationError struct {
	errs []error
}

// Limit status and log text without discarding failures used for retries.
const maxReportedNodeErrors = 10

func (e *nodeGenerationError) Error() string {
	if len(e.errs) <= maxReportedNodeErrors {
		joined := errors.Join(e.errs...)
		if joined == nil {
			return ""
		}
		return joined.Error()
	}
	return fmt.Sprintf("%v\n... and %d more node errors",
		errors.Join(e.errs[:maxReportedNodeErrors]...), len(e.errs)-maxReportedNodeErrors)
}
func (e *nodeGenerationError) Unwrap() []error { return e.errs }

// selectedNetworks is a helper struct that stores information about networks
// that have been selected by a RouteAdvertisements. It is important that prefix
// lists are ordered to generate consistent FRRConfigurations.
type selectedNetworks struct {
	// networks is an ordered list of selected network names
	networks    []string
	networkInfo map[string]util.NetInfo
	// vrfs is an ordered list of selected networks VRF's
	vrfs []string
	// networkVRFs is a mapping of VRF to corresponding network
	networkVRFs map[string]string
	// subnets is an ordered list of all selected network subnets
	subnets []string
	// hostSubnets is an ordered list of all selected network subnets specific to a node
	hostSubnets []string
	// networkSubnets is a map of selected network names to their ordered network subnets
	networkSubnets map[string][]string
	// vtepIPsByNode maps node name to an ordered list of VTEP IP prefixes (/32 or /128)
	// to advertise in the default-VRF router for EVPN underlay reachability.
	// The value should mostly be a single /32 for IPv4 and /128 for IPv6, but its a string of IPs if we
	// need to support dual-stack tunnels in the future which seems really uncommon and
	// only has a rare migration use case OR we have 1 RA selecting more than one CUDN with each
	// CUDN having a different VTEPs which is also rare.
	vtepIPsByNode map[string][]string
	// vtepCIDRs is the deduplicated, ordered list of VTEP CIDRs from the
	// referenced VTEP resources.  Used as the ToReceive prefix filter so that
	// each node accepts routes for all VTEP IPs within these ranges.
	vtepCIDRs []string
	// hostNetworkSubnets is a map of selected network names to their ordered network subnets specific for a node
	hostNetworkSubnets map[string][]string
	// prefixLength is a map of selected network to their prefix length
	prefixLength map[string]uint32
	// macVRFConfigs is an ordered list of MAC-VRF EVPN configurations for selected networks
	macVRFConfigs []*vrfConfig
	// ipVRFConfigs is an ordered list of IP-VRF EVPN configurations for selected networks
	ipVRFConfigs []*ipVRFConfig
	// Every router uses these network-wide pod subnets for no-overlay receive filters.
	noOverlayPodSubnets []string
	// networkUplinks is a map of selected network to its Uplink name.
	networkUplinks map[string]string
}

// vrfConfig holds base VRF EVPN configuration for a network. It is only
// built through newVRFConfig, which validates every field.
type vrfConfig struct {
	// networkName is the name of the network this config belongs to
	networkName string
	// vni is the VXLAN Network Identifier
	vni int32
	// routeTarget is the BGP route target, empty means use FRR defaults
	routeTarget string
}

// ipVRFConfig holds IP-VRF EVPN configuration for a network. It is only
// built through newIPVRFConfig, which validates every field.
type ipVRFConfig struct {
	vrfConfig
	// vrfName is the Linux VRF name
	vrfName string
	// hasIPv4 indicates if the network has IPv4 subnets
	hasIPv4 bool
	// hasIPv6 indicates if the network has IPv6 subnets
	hasIPv6 bool
}

// generateFRRConfigurations generates FRRConfigurations for the route
// advertisements, including selection and generation failures.
func (c *Controller) generateFRRConfigurations(ctx context.Context, ra *ratypes.RouteAdvertisements) (result frrGenerationResult) {
	if ra == nil {
		return frrGenerationResult{}
	}

	advertisements := sets.New(ra.Spec.Advertisements...)
	if advertisements.Has(ratypes.EgressIP) && !config.OVNKubernetesFeature.EnableEgressIP {
		return frrGenerationResult{err: fmt.Errorf("%w: advertising EgressIP requires EgressIP feature to be enabled", errConfig)}
	}
	if advertisements.Has(ratypes.EgressIP) && ra.Spec.TargetVRF == "auto" {
		return frrGenerationResult{err: fmt.Errorf("%w: advertising EgressIP not supported with TargetVRF set to 'auto'", errConfig)}
	}
	if ra.Spec.TargetVRF != "" && ra.Spec.TargetVRF != "auto" {
		if err := validateInterfaceName(ra.Spec.TargetVRF); err != nil {
			return frrGenerationResult{err: fmt.Errorf("%w: invalid TargetVRF %s: %w", errUnsafeFRRInput, quoteForError(ra.Spec.TargetVRF), err)}
		}
	}

	// if we are matching on the well known default network label, create an
	// internal nad for it if it doesn't exist
	nads, err := c.getSelectedNADs(ctx, ra.Spec.NetworkSelectors)
	if err != nil {
		return frrGenerationResult{err: err}
	}
	if len(nads) == 0 {
		return frrGenerationResult{err: fmt.Errorf("%w: no networks selected", errPending)}
	}
	// ordered, so that processing and error messages are deterministic across
	// reconciles regardless of lister iteration order
	slices.SortFunc(nads, func(a, b *nadtypes.NetworkAttachmentDefinition) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})

	// validate and gather information about the networks
	networkSet := sets.New[string]()
	vtepNames := sets.New[string]()
	selectedNetworks := &selectedNetworks{
		networkInfo:    map[string]util.NetInfo{},
		networkVRFs:    map[string]string{},
		networkSubnets: map[string][]string{},
		prefixLength:   map[string]uint32{},
		networkUplinks: map[string]string{},
	}
	for _, nad := range nads {
		networkName := util.GetAnnotatedNetworkName(nad)
		network := c.nm.GetNetwork(networkName)
		if network == nil {
			// network not yet known by network manager, skip
			continue
		}
		if networkSet.Has(networkName) {
			continue
		}
		if !network.IsDefault() && !network.IsPrimaryNetwork() {
			return frrGenerationResult{err: fmt.Errorf("%w: selected network %q is not the default nor a primary network", errConfig, networkName)}
		}
		if network.TopologyType() != types.Layer3Topology && network.TopologyType() != types.Layer2Topology {
			return frrGenerationResult{err: fmt.Errorf("%w: selected network %q has unsupported topology %q", errConfig, networkName, network.TopologyType())}
		}

		if advertisements.Has(ratypes.EgressIP) && network.TopologyType() == types.Layer2Topology {
			return frrGenerationResult{err: fmt.Errorf("%w: EgressIP advertisement is currently not supported for Layer2 networks, network: %s", errConfig, network.GetNetworkName())}
		}

		vrf := util.GetNetworkVRFName(network)
		if err := validateNetworkVRFName(networkName, vrf); err != nil {
			return frrGenerationResult{err: fmt.Errorf("%w: %w", errUnsafeFRRInput, err)}
		}
		if vfrNet, hasVFR := selectedNetworks.networkVRFs[vrf]; hasVFR && vfrNet != networkName {
			return frrGenerationResult{err: fmt.Errorf("%w: vrf %q found to be mapped to multiple networks %v", errConfig, vrf, []string{vfrNet, networkName})}
		}
		networkSet.Insert(networkName)
		selectedNetworks.networkInfo[networkName] = network
		selectedNetworks.vrfs = append(selectedNetworks.vrfs, vrf)
		selectedNetworks.networkVRFs[vrf] = networkName
		selectedNetworks.networkUplinks[networkName] = network.Uplink()

		// MAC-VRF configuration. Zero means no MAC-VRF; any other value,
		// negative included, is range-checked by newVRFConfig.
		if macVNI := network.EVPNMACVRFVNI(); macVNI != 0 {
			cfg, err := newVRFConfig("MAC-VRF", networkName, macVNI, network.EVPNMACVRFRouteTarget())
			if err != nil {
				return frrGenerationResult{err: fmt.Errorf("%w: %w", errUnsafeFRRInput, err)}
			}
			selectedNetworks.macVRFConfigs = append(selectedNetworks.macVRFConfigs, cfg)
		}

		// IP-VRF configuration, same zero convention as the MAC-VRF above.
		if ipVNI := network.EVPNIPVRFVNI(); ipVNI != 0 {
			// Compute IP families from network subnets
			hasIPv4, hasIPv6 := false, false
			for _, subnet := range network.Subnets() {
				if subnet.CIDR.IP.To4() == nil {
					hasIPv6 = true
				} else {
					hasIPv4 = true
				}
			}
			cfg, err := newIPVRFConfig(networkName, vrf, ipVNI, network.EVPNIPVRFRouteTarget(), hasIPv4, hasIPv6)
			if err != nil {
				return frrGenerationResult{err: fmt.Errorf("%w: %w", errUnsafeFRRInput, err)}
			}
			selectedNetworks.ipVRFConfigs = append(selectedNetworks.ipVRFConfigs, cfg)
		}
		hasEVPNConfig := network.EVPNMACVRFVNI() > 0 || network.EVPNIPVRFVNI() > 0
		if vtepName := network.EVPNVTEPName(); hasEVPNConfig && vtepName != "" {
			vtepNames.Insert(vtepName)
		}
		if hasEVPNConfig && ra.Spec.TargetVRF != "auto" && ra.Spec.TargetVRF != vrf {
			return frrGenerationResult{err: fmt.Errorf("%w: EVPN network %q with VRF %q requires TargetVRF to be 'auto' or %q, got %q",
				errConfig, networkName, vrf, vrf, ra.Spec.TargetVRF)}
		}
		// TODO check overlaps?
		for _, cidr := range network.Subnets() {
			subnet := cidr.CIDR.String()
			len := uint32(cidr.HostSubnetLength)
			selectedNetworks.networkSubnets[networkName] = append(selectedNetworks.networkSubnets[networkName], subnet)
			selectedNetworks.subnets = append(selectedNetworks.subnets, subnet)
			selectedNetworks.prefixLength[subnet] = len
		}
		// ordered
		slices.Sort(selectedNetworks.networkSubnets[networkName])
	}
	// ordered
	slices.Sort(selectedNetworks.vrfs)
	slices.Sort(selectedNetworks.subnets)
	slices.SortFunc(selectedNetworks.macVRFConfigs, func(a, b *vrfConfig) int { return int(a.vni - b.vni) })
	slices.SortFunc(selectedNetworks.ipVRFConfigs, func(a, b *ipVRFConfig) int { return int(a.vni - b.vni) })
	selectedNetworks.networks = sets.List(networkSet)
	for _, name := range selectedNetworks.networks {
		if selectedNetworks.networkInfo[name].Transport() == types.NetworkTransportNoOverlay {
			selectedNetworks.noOverlayPodSubnets = append(selectedNetworks.noOverlayPodSubnets, selectedNetworks.networkSubnets[name]...)
		}
	}
	// cache the selected networks so that network activity changes only
	// reconcile the RouteAdvertisements selecting them. Returning early above
	// keeps the previous entry, which matches the FRRConfigurations still
	// deployed from the last successful reconcile; the errors returned above
	// are only resolved by RA/NAD/node updates, which we watch and which
	// refresh this cache.
	c.setRANetworks(ra.Name, selectedNetworks.networks)

	// gather selected nodes
	nodeSelector, err := metav1.LabelSelectorAsSelector(&ra.Spec.NodeSelector)
	if err != nil {
		return frrGenerationResult{err: fmt.Errorf("%w: invalid nodeSelector: %w", errConfig, err)}
	}
	if !nodeSelector.Empty() && advertisements.Has(ratypes.PodNetwork) {
		return frrGenerationResult{err: fmt.Errorf("%w: node selector has to select all nodes if pod network is advertised", errConfig)}
	}
	allNodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return frrGenerationResult{err: fmt.Errorf("failed to list nodes for RouteAdvertisements %q: %w", ra.Name, err)}
	}
	// FRR matches the generated hostname selector on every node, including
	// nodes outside this RA or its peering templates.
	hostnameCounts := make(map[string]int, len(allNodes))
	var nodes []*corev1.Node
	for _, node := range allNodes {
		hostnameCounts[node.Labels[corev1.LabelHostname]]++
		if nodeSelector.Matches(labels.Set(node.Labels)) {
			nodes = append(nodes, node)
		}
	}
	if len(nodes) == 0 {
		return frrGenerationResult{err: fmt.Errorf("%w: no nodes selected", errPending)}
	}
	// ordered, so that processing and error messages are deterministic across
	// reconciles regardless of lister iteration order
	slices.SortFunc(nodes, func(a, b *corev1.Node) int { return strings.Compare(a.Name, b.Name) })
	// prepare a map of selected nodes to the FRRConfigurations that apply to
	// them
	nodeTemplates := map[string][]*frrTemplate{}
	// Use one activity snapshot for both template validation and generation.
	nodeToNetworks := make(map[string][]string, len(nodes))
	for _, node := range nodes {
		for _, network := range selectedNetworks.networks {
			if !c.nm.NodeHasNetwork(node.Name, network) {
				// only advertise a network on nodes where it is active, that
				// is, with pods or EgressIPs attached to it; NodeHasNetwork
				// returns false only with dynamic UDN allocation enabled.
				// The network manager notifies us when a network goes active
				// or inactive on a node.
				continue
			}
			nodeToNetworks[node.Name] = append(nodeToNetworks[node.Name], network)
		}
	}

	// gather selected FRRConfigurations, map them to the selected nodes
	frrSelector, err := metav1.LabelSelectorAsSelector(&ra.Spec.FRRConfigurationSelector)
	if err != nil {
		return frrGenerationResult{err: fmt.Errorf("%w: invalid frrConfigurationSelector: %w", errConfig, err)}
	}
	frrConfigs, err := c.frrLister.List(frrSelector)
	if err != nil {
		return frrGenerationResult{err: err}
	}
	if len(frrConfigs) == 0 {
		return frrGenerationResult{err: fmt.Errorf("%w: no FRRConfigurations selected", errPending)}
	}
	// Drop generated configurations before sorting. With an empty or broad
	// selector they can vastly outnumber the user-provided sources, and only
	// the sources need to be ordered below.
	frrConfigs = slices.DeleteFunc(frrConfigs, func(frrConfig *frrtypes.FRRConfiguration) bool {
		return strings.HasPrefix(frrConfig.Name, generateName)
	})
	// ordered, so that processing and error messages are deterministic across
	// reconciles regardless of lister iteration order
	slices.SortFunc(frrConfigs, func(a, b *frrtypes.FRRConfiguration) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})

	frrRouterVRFs := sets.New[string]()
	var templates []*frrTemplate
	hasDefaultRouter := false
	for _, frrConfig := range frrConfigs {
		nodeSelector, err := metav1.LabelSelectorAsSelector(&frrConfig.Spec.NodeSelector)
		if err != nil {
			result.nodeErrors = append(result.nodeErrors, fmt.Errorf("%w: FRRConfiguration %s/%s has an invalid node selector: %w",
				errConfig, frrConfig.Namespace, frrConfig.Name, err))
			continue
		}
		var template *frrTemplate
		var usable bool
		for _, node := range nodes {
			networks := nodeToNetworks[node.Name]
			if len(networks) == 0 || !nodeSelector.Matches(labels.Set(node.Labels)) {
				continue
			}
			if template == nil {
				template = prepareFRRTemplate(ra.Spec.TargetVRF, frrConfig, selectedNetworks)
				templates = append(templates, template)
			}
			nodeTemplates[node.Name] = append(nodeTemplates[node.Name], template)
			usable = usable || template.validationError(ra.Spec.TargetVRF, networks) == nil
		}
		for i, router := range frrConfig.Spec.BGP.Routers {
			hasDefaultRouter = hasDefaultRouter || router.VRF == ""
			// An explicit router takes precedence even when its template selects
			// other nodes. A rejected router cannot supply the EVPN tenant ASN.
			if template == nil || (usable && template.routerErrors[i] == nil) {
				frrRouterVRFs.Insert(router.VRF)
			}
		}
	}

	// Validate EVPN configuration requirements
	hasEVPNConfig := len(selectedNetworks.macVRFConfigs) > 0 || len(selectedNetworks.ipVRFConfigs) > 0
	if hasEVPNConfig && !config.OVNKubernetesFeature.EnableEVPN {
		return frrGenerationResult{err: fmt.Errorf("%w: EVPN networks selected but EVPN feature is not enabled", errConfig)}
	}
	if hasEVPNConfig && config.Gateway.Mode != config.GatewayModeLocal {
		return frrGenerationResult{err: fmt.Errorf("%w: EVPN networks selected but EVPN feature is only supported in local gateway mode", errConfig)}
	}
	// Require a router with default VRF for any EVPN configuration, since the
	// global EVPN section with advertise-all-vni is required for EVPN to work properly.
	if hasEVPNConfig && !hasDefaultRouter {
		return frrGenerationResult{err: fmt.Errorf("%w: EVPN requires a router with default VRF but none were found in selected FRRConfigurations", errConfig)}
	}
	// Read per-node VTEP IPs from the k8s.ovn.org/vteps annotation written by
	// ovnkube-node. This runs after EVPN validation so c.vtepLister (only
	// initialized when EVPN is enabled) is safe to dereference.
	vtepCIDRSet := sets.New[string]()
	vtepCIDRs := map[string][]*net.IPNet{}
	for _, vtepName := range sets.List(vtepNames) {
		vtep, err := c.vtepLister.Get(vtepName)
		if err != nil {
			return frrGenerationResult{err: fmt.Errorf("%w: VTEP %q referenced by EVPN network not found: %w", errConfig, vtepName, err)}
		}
		for _, cidr := range vtep.Spec.CIDRs {
			vtepCIDRSet.Insert(string(cidr))
		}
		vtepCIDRs[vtepName], err = util.ParseIPNets(vtep.Spec.CIDRs)
		if err != nil {
			return frrGenerationResult{err: fmt.Errorf("%w: failed to parse VTEP %q CIDRs: %w", errConfig, vtepName, err)}
		}
	}
	vtepIPsByNode := map[string]sets.Set[string]{}
	result.invalidVTEPNodes = sets.New[string]()
	var vtepErrors []error
	if len(vtepNames) > 0 {
		for _, node := range nodes {
			prefixes, validationErr := getNodeVTEPPrefixes(node, vtepCIDRs)
			if validationErr != nil {
				if util.IsAnnotationNotSetError(validationErr) {
					continue
				}
				result.invalidVTEPNodes.Insert(node.Name)
				vtepErrors = append(vtepErrors, fmt.Errorf("%w: %w", errInvalidVTEP, validationErr))
				continue
			}
			vtepIPsByNode[node.Name] = prefixes
		}
	}
	result.vtepErr = errors.Join(vtepErrors...)

	if len(vtepIPsByNode) > 0 {
		existing, listErr := c.frrLister.List(labels.Everything())
		if listErr != nil {
			result.err = fmt.Errorf("failed listing existing FRRConfigurations for VTEP conflict detection in RouteAdvertisements %q: %w", ra.Name, listErr)
			return result
		}
		claims, err := c.vtepClaimsByRA(ctx, ra.Name, vtepIPsByNode, existing)
		if err != nil {
			result.err = err
			return result
		}
		rejected, conflictErr := conflictingVTEPNodes(ra.Name, claims, existing)
		result.invalidVTEPNodes.Insert(rejected.UnsortedList()...)
		result.vtepErr = errors.Join(result.vtepErr, conflictErr)
	}
	selectedNetworks.vtepIPsByNode = make(map[string][]string, len(vtepIPsByNode))
	for node, ips := range vtepIPsByNode {
		selectedNetworks.vtepIPsByNode[node] = sets.List(ips)
	}
	selectedNetworks.vtepCIDRs = sets.List(vtepCIDRSet)

	for _, template := range templates {
		template.resolveEVPNRouters(selectedNetworks, frrRouterVRFs)
	}

	// EgressIP assignments are shared by every node. A lookup failure must
	// abort before applying any changes, rather than remove configurations
	// as if each node had independently failed generation.
	var eipsByNodesByNetworks map[string]map[string]sets.Set[string]
	if advertisements.Has(ratypes.EgressIP) {
		result.egressIPOwners = map[netip.Prefix]string{}
		eipsByNodesByNetworks, err = c.getEgressIPsByNodesByNetworks(networkSet)
		if err != nil {
			result.err = fmt.Errorf("failed to collect EgressIP assignments: %w", err)
			return result
		}
	}

	generated := []*frrtypes.FRRConfiguration{}
	// iterate nodes in their sorted order rather than ranging over the map, so
	// that processing and error messages are deterministic across reconciles
	for _, node := range nodes {
		nodeName := node.Name
		// Exclude invalid nodes from desired state so previously generated
		// advertisements are removed instead of retained on validation errors.
		if result.invalidVTEPNodes.Has(nodeName) {
			continue
		}
		nodeNetworks := nodeToNetworks[nodeName]
		if len(nodeNetworks) == 0 {
			continue
		}
		nodeEgressIPs := eipsByNodesByNetworks[nodeName]
		hasEgressIPs := len(nodeEgressIPs) > 0 && slices.ContainsFunc(nodeNetworks, func(name string) bool {
			return len(nodeEgressIPs[name]) > 0
		})
		if !advertisements.Has(ratypes.PodNetwork) && !hasEgressIPs {
			continue
		}

		if hasEgressIPs && len(nodeTemplates[nodeName]) == 0 {
			result.nodeErrors = append(result.nodeErrors, fmt.Errorf("node %q: %w: no peering template selects the assigned EgressIP owner", nodeName, errConfig))
		}

		// Auto requires template coverage for every active network. An explicit
		// target only needs prefixes on nodes where a template will use them.
		valid, errs := selectNodeTemplates(ra, nodeTemplates[nodeName], selectedNetworks, nodeNetworks)
		for _, err := range errs {
			result.nodeErrors = append(result.nodeErrors, fmt.Errorf("node %q: %w", nodeName, err))
		}
		var podPrefixes map[string][]string
		var subnetErr error
		if len(valid) > 0 && advertisements.Has(ratypes.PodNetwork) {
			podPrefixes, subnetErr = selectedNetworks.podPrefixesForNode(node, nodeNetworks)
			if subnetErr != nil {
				result.nodeErrors = append(result.nodeErrors, fmt.Errorf("node %q: %w", nodeName, subnetErr))
				// A shared target imports every selected network. In auto mode
				// each VRF can keep advertising its own allocated prefixes.
				if ra.Spec.TargetVRF != "auto" || len(podPrefixes) == 0 {
					continue
				}
				nodeNetworks = slices.Sorted(maps.Keys(podPrefixes))
			}
		}
		selectedNetworks.hostNetworkSubnets = make(map[string][]string, len(nodeNetworks))
		allPrefixes := sets.New[string]()
		for _, network := range nodeNetworks {
			egressPrefixes := nodeEgressIPs[network].UnsortedList()
			for _, eip := range egressPrefixes {
				if prefix, err := netip.ParsePrefix(eip); err == nil {
					result.egressIPOwners[prefix] = nodeName
				}
			}
			prefixes := append(egressPrefixes, podPrefixes[network]...)
			slices.Sort(prefixes)
			selectedNetworks.hostNetworkSubnets[network] = prefixes
			allPrefixes.Insert(prefixes...)
		}
		selectedNetworks.hostSubnets = sets.List(allPrefixes)
		hostname := node.Labels[corev1.LabelHostname]
		if count := hostnameCounts[hostname]; len(valid) > 0 && hostname != "" && count > 1 {
			result.nodeErrors = append(result.nodeErrors, fmt.Errorf("%w: node %q: %s=%s matches %d nodes", errNodeNotReady, nodeName, corev1.LabelHostname, hostname, count))
			continue
		}
		nodeHasEVPNPeers := false
		var covered sets.Set[string]
		if ra.Spec.TargetVRF == "auto" {
			covered = sets.New[string]()
		}
		for _, template := range valid {
			cfg, err := c.generateFRRConfiguration(ra, template, node, selectedNetworks, covered)
			if err == nil && cfg == nil {
				// A template dedicated to a missing subnet can be valid and
				// empty. The subnet error already explains its withdrawal.
				if subnetErr != nil && frrConfigOnlyMatchesInactiveNetworks(template.source, selectedNetworks, nodeNetworks) {
					continue
				}
				err = fmt.Errorf("%w: no routes or peers match the target VRF", errConfig)
			}
			if err != nil {
				result.nodeErrors = append(result.nodeErrors, fmt.Errorf("node %q, FRRConfiguration %s/%s: %w",
					nodeName, template.source.Namespace, template.source.Name, err))
				continue
			}
			generated = append(generated, cfg)
			nodeHasEVPNPeers = nodeHasEVPNPeers || template.hasEVPNPeers()
		}
		// FRR merges templates on the same node. A tenant router may come
		// from a different template than its EVPN peers, but peers on another
		// node cannot advertise this node's tenant routes.
		if !nodeHasEVPNPeers && slices.ContainsFunc(selectedNetworks.ipVRFConfigs, func(cfg *ipVRFConfig) bool {
			return len(selectedNetworks.hostNetworkSubnets[cfg.networkName]) > 0
		}) {
			result.nodeErrors = append(result.nodeErrors, fmt.Errorf("node %q: %w: EVPN IP-VRF requires a default-VRF router with EVPN peers on this node", nodeName, errConfig))
		}
		if covered != nil && !covered.HasAll(nodeNetworks...) {
			result.nodeErrors = append(result.nodeErrors, fmt.Errorf("node %q: %w: selected FRRConfigurations don't match all selected networks with target VRF 'auto': missing %v",
				nodeName, errConfig, sets.List(sets.New(nodeNetworks...).Difference(covered))))
		}
	}
	result.frrConfigs = generated
	result.nads = nads
	return result
}

// A layer 2 network uses the same pod subnets on every node. Layer 3 networks
// share one node annotation, so read it once for all selected networks.
func (s *selectedNetworks) podPrefixesForNode(node *corev1.Node, networks []string) (map[string][]string, error) {
	prefixes := make(map[string][]string, len(networks))
	var layer3 []string
	var errs []error
	for _, name := range networks {
		if s.networkInfo[name].TopologyType() == types.Layer3Topology {
			layer3 = append(layer3, name)
			continue
		}
		if len(s.networkSubnets[name]) == 0 {
			errs = append(errs, fmt.Errorf("%w: no layer2 subnets found for network %q", errConfig, name))
			continue
		}
		prefixes[name] = s.networkSubnets[name]
	}
	if len(layer3) > 0 {
		hostSubnets, err := util.ParseNodeHostSubnetsForNetworks(node, layer3)
		if err != nil {
			errs = append(errs, fmt.Errorf("%w: waiting for subnet annotation on node %q: %w", errNodeNotReady, node.Name, err))
		}
		for name, subnets := range hostSubnets {
			prefixes[name] = util.StringSlice(subnets)
		}
	}
	return prefixes, errors.Join(errs...)
}

// vtepClaimsByRA collects claims only from nodes selected by an EVPN RA with
// a default-VRF source router. Tenant VRFs share the default-VRF underlay, so
// their VTEP host prefixes must be checked together. Annotations outside every
// such RA do not reserve addresses.
func (c *Controller) vtepClaimsByRA(ctx context.Context, current string, currentClaims map[string]sets.Set[string], configs []*frrtypes.FRRConfiguration) (map[string]map[string]sets.Set[string], error) {
	claims := map[string]map[string]sets.Set[string]{current: currentClaims}
	ras, err := c.raLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed listing RouteAdvertisements for VTEP conflict detection: %w", err)
	}
	slices.SortFunc(ras, func(a, b *ratypes.RouteAdvertisements) int { return strings.Compare(a.Name, b.Name) })
	for _, ra := range ras {
		if ra.Name == current {
			continue
		}
		if slices.Contains(ra.Spec.Advertisements, ratypes.EgressIP) &&
			(!config.OVNKubernetesFeature.EnableEgressIP || ra.Spec.TargetVRF == "auto") {
			continue
		}
		nodeSelector, err := metav1.LabelSelectorAsSelector(&ra.Spec.NodeSelector)
		if err != nil {
			continue // Invalid selections cannot introduce an advertised claim.
		}
		if slices.Contains(ra.Spec.Advertisements, ratypes.PodNetwork) && !nodeSelector.Empty() {
			continue
		}
		frrSelector, err := metav1.LabelSelectorAsSelector(&ra.Spec.FRRConfigurationSelector)
		if err != nil {
			continue
		}
		nads, err := c.getSelectedNADs(ctx, ra.Spec.NetworkSelectors)
		if errors.Is(err, errConfig) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("failed selecting networks for RouteAdvertisements %q during VTEP conflict detection: %w", ra.Name, err)
		}
		cidrs := map[string][]*net.IPNet{}
		for _, nad := range nads {
			network := c.nm.GetNetwork(util.GetAnnotatedNetworkName(nad))
			if network == nil || (!network.IsDefault() && !network.IsPrimaryNetwork()) ||
				(network.EVPNMACVRFVNI() <= 0 && network.EVPNIPVRFVNI() <= 0) {
				continue
			}
			if ra.Spec.TargetVRF != "auto" && ra.Spec.TargetVRF != util.GetNetworkVRFName(network) {
				continue
			}
			name := network.EVPNVTEPName()
			if name == "" {
				continue
			}
			vtep, err := c.vtepLister.Get(name)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("failed getting VTEP %q for conflict detection: %w", name, err)
			}
			prefixes, err := util.ParseIPNets(vtep.Spec.CIDRs)
			if err == nil {
				cidrs[name] = prefixes
			}
		}
		if len(cidrs) == 0 {
			continue
		}
		nodes, err := c.nodeLister.List(nodeSelector)
		if err != nil {
			return nil, fmt.Errorf("failed listing nodes for RouteAdvertisements %q during VTEP conflict detection: %w", ra.Name, err)
		}
		// Filter sources and parse selectors once per RA, rather than scanning
		// every generated configuration again for each selected node.
		var underlaySelectors []labels.Selector
		for _, cfg := range configs {
			if strings.HasPrefix(cfg.Name, generateName) || !frrSelector.Matches(labels.Set(cfg.Labels)) {
				continue
			}
			if !slices.ContainsFunc(cfg.Spec.BGP.Routers, func(router frrtypes.Router) bool {
				return router.VRF == "" && router.ASN != 0 && len(router.Neighbors) > 0
			}) {
				continue
			}
			selector, err := metav1.LabelSelectorAsSelector(&cfg.Spec.NodeSelector)
			if err != nil {
				continue
			}
			underlaySelectors = append(underlaySelectors, selector)
		}
		if len(underlaySelectors) == 0 {
			continue
		}
		claims[ra.Name] = map[string]sets.Set[string]{}
		for _, node := range nodes {
			if !slices.ContainsFunc(underlaySelectors, func(selector labels.Selector) bool {
				return selector.Matches(labels.Set(node.Labels))
			}) {
				continue
			}
			prefixes, err := getNodeVTEPPrefixes(node, cidrs)
			if err == nil {
				claims[ra.Name][node.Name] = prefixes
			}
		}
	}
	return claims, nil
}

// conflictingVTEPNodes compares VTEP claims across RAs sharing the default
// underlay. Existing configurations establish continuity, not authenticated
// ownership. Only conflicts involving the current RA are reported.
func conflictingVTEPNodes(ra string, claimsByRA map[string]map[string]sets.Set[string], existing []*frrtypes.FRRConfiguration) (sets.Set[string], error) {
	claimants := map[string]sets.Set[string]{}
	for _, prefixesByNode := range claimsByRA {
		for node, prefixes := range prefixesByNode {
			for prefix := range prefixes {
				if claimants[prefix] == nil {
					claimants[prefix] = sets.New[string]()
				}
				claimants[prefix].Insert(node)
			}
		}
	}
	localPrefixes := sets.New[string]()
	for _, prefixes := range claimsByRA[ra] {
		localPrefixes.Insert(prefixes.UnsortedList()...)
	}
	established := map[string]sets.Set[string]{}
	for _, cfg := range existing {
		if !strings.HasPrefix(cfg.Name, generateName) {
			continue
		}
		key := strings.Split(cfg.Annotations[types.OvnRouteAdvertisementsKey], "/")
		if len(key) != 3 || key[0] == "" || key[0] != cfg.Labels[types.OvnRouteAdvertisementsKey] || key[1] == "" || key[2] == "" {
			continue
		}
		// The annotation identifies the node; its hostname selector may differ.
		node := key[2]
		for _, router := range cfg.Spec.BGP.Routers {
			if router.VRF != "" {
				continue
			}
			for _, prefix := range router.Prefixes {
				if !claimsByRA[key[0]][node].Has(prefix) {
					continue
				}
				if established[prefix] == nil {
					established[prefix] = sets.New[string]()
				}
				established[prefix].Insert(node)
			}
		}
	}
	rejected := sets.New[string]()
	var conflicts []error
	// Status messages must not change solely because map iteration changes.
	for _, prefix := range slices.Sorted(maps.Keys(claimants)) {
		nodes := claimants[prefix]
		if nodes.Len() < 2 || !localPrefixes.Has(prefix) {
			continue
		}
		owners := established[prefix]
		if owners.Len() == 1 {
			losers := nodes.Difference(owners)
			rejected.Insert(losers.UnsortedList()...)
			conflicts = append(conflicts, fmt.Errorf("%w: VTEP prefix %s conflicts between nodes %v; preserving established advertiser %s, excluding nodes %v", errInvalidVTEP, prefix, sets.List(nodes), sets.List(owners)[0], sets.List(losers)))
		} else {
			rejected.Insert(nodes.UnsortedList()...)
			conflicts = append(conflicts, fmt.Errorf("%w: VTEP prefix %s conflicts between nodes %v with no unique established advertiser; excluding all claimants", errInvalidVTEP, prefix, sets.List(nodes)))
		}
	}
	localRejected := sets.New[string]()
	for node, prefixes := range claimsByRA[ra] {
		if rejected.Has(node) {
			for prefix := range prefixes {
				if claimants[prefix].Len() > 1 && (established[prefix].Len() != 1 || !established[prefix].Has(node)) {
					localRejected.Insert(node)
				}
			}
		}
	}
	return localRejected, errors.Join(conflicts...)
}

// getNodeVTEPPrefixes validates selected VTEPs against their own CIDRs and
// returns canonical host prefixes suitable for FRR advertisement.
func getNodeVTEPPrefixes(node *corev1.Node, vtepCIDRs map[string][]*net.IPNet) (sets.Set[string], error) {
	vteps, err := util.ValidateNodeVTEPs(node, sets.New(slices.Collect(maps.Keys(vtepCIDRs))...))
	if err != nil {
		return nil, fmt.Errorf("failed validating VTEP prefixes for node %q: %w", node.Name, err)
	}
	prefixes := sets.New[string]()
	// Keep the first CIDR validation error stable across reconciliations.
	for _, name := range slices.Sorted(maps.Keys(vtepCIDRs)) {
		for _, address := range vteps[name].IPs {
			ip := net.ParseIP(address)
			if !util.IsIPContainedInAnyCIDR(ip, vtepCIDRs[name]...) {
				return nil, fmt.Errorf("node %q VTEP %q IP %q is outside its configured CIDRs", node.Name, name, address)
			}
			if ip.To4() == nil {
				prefixes.Insert(address + "/128")
			} else {
				prefixes.Insert(address + "/32")
			}
		}
	}
	return prefixes, nil
}

// After a failed write, one peer can still have the old prefix while another
// has the new one. Count the RA's cached objects instead of the desired ones.
// The retry picks up any writes from this pass that reach the cache later.
func (c *Controller) recordFRRConfigurationMetrics(ra string, generated []*frrtypes.FRRConfiguration, writeFailed bool) {
	if !config.Metrics.EnableScaleMetrics {
		return
	}
	if writeFailed {
		persisted, err := c.listExistingFRRConfigurations(ra)
		if err != nil {
			klog.V(4).Infof("Could not refresh FRR configuration metrics for RouteAdvertisements %q: %v", ra, err)
			return
		}
		generated = persisted
	}
	metrics.RecordRouteAdvertisementFRRConfigurations(ra, len(generated))
	v4, v6 := countAdvertisedPrefixes(generated)
	metrics.RecordRouteAdvertisementAdvertisedPrefixes(ra, "ipv4", v4)
	metrics.RecordRouteAdvertisementAdvertisedPrefixes(ra, "ipv6", v6)
}

// countAdvertisedPrefixes counts the distinct prefixes across the generated
// FRRConfigurations, by IP family. The same prefix advertised from several
// nodes, or by several routers, counts once.
func countAdvertisedPrefixes(generated []*frrtypes.FRRConfiguration) (v4, v6 int) {
	prefixes := sets.New[string]()
	for _, frrConfig := range generated {
		for _, router := range frrConfig.Spec.BGP.Routers {
			prefixes.Insert(router.Prefixes...)
		}
	}
	for prefix := range prefixes {
		if utilnet.IsIPv6CIDRString(prefix) {
			v6++
			continue
		}
		v4++
	}
	return v4, v6
}

// frrTemplate holds node-independent selection and validated peers. The same
// router and EVPN choices are used before prefix allocation and during rendering.
type frrTemplate struct {
	source         *frrtypes.FRRConfiguration
	routerErrors   map[int]error
	routerNetworks map[int]string
	peers          map[int][]frrNeighbor
	globalRouter   *frrtypes.Router
	globalPeers    []frrNeighbor
	ipVRFRouters   []templateIPVRF
	macVRFNetworks []string
}

type templateIPVRF struct {
	config    *ipVRFConfig
	asn       uint32
	inherited bool
}

func prepareFRRTemplate(targetVRF string, source *frrtypes.FRRConfiguration, selected *selectedNetworks) *frrTemplate {
	template := &frrTemplate{source: source, routerNetworks: map[int]string{}, peers: map[int][]frrNeighbor{}}
	hasEVPN := len(selected.ipVRFConfigs) > 0 || len(selected.macVRFConfigs) > 0
	for i := range source.Spec.BGP.Routers {
		router := &source.Spec.BGP.Routers[i]
		network, matches := matchingRouterNetwork(targetVRF, router.VRF, selected)
		if matches {
			template.routerNetworks[i] = network
		}
		if !matches && !(hasEVPN && router.VRF == "") {
			continue
		}
		peers, err := validateRouterPeers(source, router.Neighbors)
		if err != nil {
			if template.routerErrors == nil {
				template.routerErrors = map[int]error{}
			}
			template.routerErrors[i] = err
			continue
		}
		template.peers[i] = peers
		if hasEVPN && router.VRF == "" && template.globalRouter == nil {
			template.globalRouter = router
			template.globalPeers = peers
		}
	}
	return template
}

func validateRouterPeers(source *frrtypes.FRRConfiguration, neighbors []frrtypes.Neighbor) ([]frrNeighbor, error) {
	peers := make([]frrNeighbor, 0, len(neighbors))
	for _, neighbor := range neighbors {
		if neighborKey(neighbor) == "" {
			return nil, fmt.Errorf("%w: neighbor with neither address nor interface on FRRConfiguration %s/%s",
				errConfig, source.Namespace, source.Name)
		}
		peer, err := newFRRNeighbor(neighbor)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid neighbor on FRRConfiguration %s/%s: %w",
				errUnsafeFRRInput, source.Namespace, source.Name, err)
		}
		// Dual-stack sessions can choose the masquerade IPv6 next hop
		// over an IPv4 session. Require address-family-specific sessions.
		if neighbor.DualStackAddressFamily {
			return nil, fmt.Errorf("%w: DualStackAddressFamily==true not supported, seen on FRRConfiguration %s/%s neighbor %s",
				errConfig, source.Namespace, source.Name, neighbor.Address)
		}
		peers = append(peers, peer)
	}
	return peers, nil
}

// In auto mode, blue's bad peer only affects nodes that use blue's router.
// An EVPN underlay router is shared even when the RA only selects tenants.
func (template *frrTemplate) validationError(targetVRF string, networks []string) error {
	if len(template.routerErrors) == 0 {
		return nil
	}
	for i, router := range template.source.Spec.BGP.Routers {
		err := template.routerErrors[i]
		if err == nil {
			continue
		}
		if targetVRF != "auto" || router.VRF == "" || slices.Contains(networks, template.routerNetworks[i]) {
			return err
		}
	}
	return nil
}

func (template *frrTemplate) hasEVPNPeers() bool {
	return template.globalRouter != nil && template.globalRouter.ASN > 0 && len(template.globalPeers) > 0
}

func (template *frrTemplate) resolveEVPNRouters(selected *selectedNetworks, frrRouterVRFs sets.Set[string]) {
	ipVRFNetworks := sets.New[string]()
	for _, cfg := range selected.ipVRFConfigs {
		ipVRFNetworks.Insert(cfg.networkName)
		if frrRouterVRFs.Has(cfg.vrfName) {
			// An explicit router in any source prevents global ASN fallback.
			for i, router := range template.source.Spec.BGP.Routers {
				if router.VRF == cfg.vrfName && template.routerErrors[i] == nil {
					template.ipVRFRouters = append(template.ipVRFRouters, templateIPVRF{config: cfg, asn: router.ASN})
					break
				}
			}
		} else if template.globalRouter != nil && template.globalRouter.ASN > 0 {
			template.ipVRFRouters = append(template.ipVRFRouters, templateIPVRF{config: cfg, asn: template.globalRouter.ASN, inherited: true})
		}
	}
	if template.hasEVPNPeers() {
		for _, cfg := range selected.macVRFConfigs {
			if !ipVRFNetworks.Has(cfg.networkName) {
				template.macVRFNetworks = append(template.macVRFNetworks, cfg.networkName)
			}
		}
	}
}

// Select applicable templates before looking up pod subnets. A node outside
// the peering selectors can have no subnet yet and still need no configuration.
func selectNodeTemplates(ra *ratypes.RouteAdvertisements, templates []*frrTemplate, selected *selectedNetworks, networks []string) ([]*frrTemplate, []error) {
	var valid []*frrTemplate
	var errs []error
	for _, template := range templates {
		source := template.source
		if err := template.validationError(ra.Spec.TargetVRF, networks); err != nil {
			errs = append(errs, err)
			continue
		}
		// EVPN may use a global router in one template and an IP-VRF in another.
		matched := template.hasEVPNPeers() ||
			slices.ContainsFunc(template.ipVRFRouters, func(r templateIPVRF) bool {
				return r.inherited && slices.Contains(networks, r.config.networkName)
			})
		for _, network := range template.routerNetworks {
			if ra.Spec.TargetVRF != "auto" || slices.Contains(networks, network) {
				matched = true
				break
			}
		}
		if matched {
			valid = append(valid, template)
		} else if ra.Spec.TargetVRF != "auto" || !frrConfigOnlyMatchesInactiveNetworks(source, selected, networks) {
			errs = append(errs, fmt.Errorf("%w: FRRConfiguration %s/%s has no VRF matching the RouteAdvertisements target VRF or any selected network",
				errConfig, source.Namespace, source.Name))
		}
	}
	return valid, errs
}

// frrConfigOnlyMatchesInactiveNetworks helps decide whether an
// FRRConfiguration that matched no router for a node is a configuration
// error. It is not an error if the FRRConfiguration is dedicated to selected
// networks that are simply not active on the node: that is, its router
// VRFs reference at least one selected network, and none of the referenced
// networks are among the ones advertised on the node (nodeNetworks). Used
// with target VRF 'auto' to skip such FRRConfigurations for the node.
func frrConfigOnlyMatchesInactiveNetworks(frrConfig *frrtypes.FRRConfiguration, selected *selectedNetworks, nodeNetworks []string) bool {
	advertised := sets.New(nodeNetworks...)
	referencesSelected := false
	for _, router := range frrConfig.Spec.BGP.Routers {
		network := selected.networkVRFs[router.VRF]
		if router.VRF == "" && slices.Contains(selected.networks, types.DefaultNetworkName) {
			network = types.DefaultNetworkName
		}
		if network == "" {
			continue
		}
		referencesSelected = true
		if advertised.Has(network) {
			return false
		}
	}
	return referencesSelected
}

// matchingRouterNetwork selects routers independently of node readiness so
// validation and generation agree even before node prefixes are available.
func matchingRouterNetwork(targetVRF, routerVRF string, selected *selectedNetworks) (string, bool) {
	if targetVRF == "auto" {
		if routerVRF == "" {
			return types.DefaultNetworkName, slices.Contains(selected.networks, types.DefaultNetworkName)
		}
		network, found := selected.networkVRFs[routerVRF]
		return network, found
	}
	if targetVRF != routerVRF {
		return "", false
	}
	if targetVRF == "" {
		return types.DefaultNetworkName, true
	}
	return selected.networkVRFs[targetVRF], true
}

// generateFRRConfiguration generates a FRRConfiguration from a source for a
// specific node using the already validated template selection.
func (c *Controller) generateFRRConfiguration(
	ra *ratypes.RouteAdvertisements,
	template *frrTemplate,
	node *corev1.Node,
	selectedNetworks *selectedNetworks,
	covered sets.Set[string],
) (*frrtypes.FRRConfiguration, error) {
	// The source lives in the informer cache and is reused for every node, so copy it before appending.
	source := template.source.DeepCopy()
	nodeName := node.Name
	var routers []frrtypes.Router
	var renderedNetworks []string

	// track neighbors and ASNs to generate raw config later on
	vrfNeighbors := map[string][]frrNeighbor{}
	vrfASNs := map[string]uint32{}

	// go over the source routers
	for i, router := range source.Spec.BGP.Routers {

		matchedNetwork, matches := template.routerNetworks[i]
		if !matches {
			continue
		}
		matchedVRF := router.VRF
		advertisePrefixes := selectedNetworks.hostSubnets
		if ra.Spec.TargetVRF == "auto" {
			advertisePrefixes = selectedNetworks.hostNetworkSubnets[matchedNetwork]
		}
		if len(advertisePrefixes) == 0 {
			continue
		}

		// if this router's VRF matches the target VRF, copy it and set the
		// prefixes as appropriate
		targetRouter := router
		targetRouter.Prefixes = advertisePrefixes

		// The managed full-mesh template uses Kubernetes InternalIPs. Filter
		// against that same source, not the primary interface annotation.
		// User-provided templates own their peer lists, which may use other
		// interfaces or VRFs where these addresses are not local.
		var nodeAddrs []netip.Addr
		if source.Name == types.ManagedBGPConfigurationName && source.Namespace == config.ManagedBGP.FRRNamespace {
			v4, v6 := util.GetNodeInternalAddrs(node)
			for _, ip := range []net.IP{v4, v6} {
				if addr, ok := netip.AddrFromSlice(ip); ok {
					nodeAddrs = append(nodeAddrs, addr.Unmap())
				}
			}
			if len(nodeAddrs) == 0 {
				// errPending applies an empty desired set in the caller. Retry instead
				// so one initializing node cannot remove existing advertisements.
				return nil, fmt.Errorf("waiting for InternalIP addresses for node %q", nodeName)
			}
		}

		dpuHostGatewayNextHops, err := c.getDPUHostGatewayNextHops(node, selectedNetworks, matchedNetwork)
		if err != nil {
			return nil, err
		}

		targetRouter.Neighbors = make([]frrtypes.Neighbor, 0, len(source.Spec.BGP.Routers[i].Neighbors))
		for j, neighbor := range source.Spec.BGP.Routers[i].Neighbors {
			peer := template.peers[i][j]
			if slices.ContainsFunc(nodeAddrs, peer.is) {
				continue
			}

			// FRR establishes an unnumbered session over an IPv4 address
			// derived from a /30 or /31 on the interface or, failing that,
			// over the peer's IPv6 link-local address. In either case the
			// session can carry prefixes of both families (RFC 8950), so
			// don't filter by family.
			isUnnumbered := isUnnumberedNeighbor(neighbor)
			isIPV6 := utilnet.IsIPv6String(neighbor.Address)
			advertisePrefixesForNeighbor := advertisePrefixes
			if !isUnnumbered {
				advertisePrefixesForNeighbor = util.MatchAllIPNetsStringFamily(isIPV6, advertisePrefixes)
			}
			if len(advertisePrefixesForNeighbor) == 0 {
				continue
			}

			neighbor.ToAdvertise = frrtypes.Advertise{
				Allowed: frrtypes.AllowedOutPrefixes{
					Mode:     frrtypes.AllowRestricted,
					Prefixes: advertisePrefixesForNeighbor,
				},
			}
			if isUnnumbered {
				neighbor.ToAdvertise.NextHop.IPv4 = dpuHostGatewayNextHops[false]
				neighbor.ToAdvertise.NextHop.IPv6 = dpuHostGatewayNextHops[true]
			} else if nextHop := dpuHostGatewayNextHops[isIPV6]; nextHop != "" {
				if isIPV6 {
					neighbor.ToAdvertise.NextHop.IPv6 = nextHop
				} else {
					neighbor.ToAdvertise.NextHop.IPv4 = nextHop
				}
			}

			// For no-overlay networks, add routes to pod subnets to the accepted routes list
			// frr-k8s will merge the prefixes from both the generated and the base FRRConfiguration
			if len(selectedNetworks.noOverlayPodSubnets) > 0 {
				// Filter pod subnets by IP family to match the neighbor;
				// unnumbered neighbors carry both families
				filteredPodSubnets := selectedNetworks.noOverlayPodSubnets
				if !isUnnumbered {
					filteredPodSubnets = util.MatchAllIPNetsStringFamily(isIPV6, filteredPodSubnets)
				}
				if len(filteredPodSubnets) > 0 {
					neighbor.ToReceive = frrtypes.Receive{
						Allowed: frrtypes.AllowedInPrefixes{
							Mode: frrtypes.AllowRestricted,
						},
					}
					for _, subnet := range filteredPodSubnets {
						neighbor.ToReceive.Allowed.Prefixes = append(neighbor.ToReceive.Allowed.Prefixes, frrtypes.PrefixSelector{
							Prefix: subnet,
							LE:     selectedNetworks.prefixLength[subnet],
							GE:     selectedNetworks.prefixLength[subnet],
						})
					}
				}
			}

			vrfNeighbors[matchedVRF] = append(vrfNeighbors[matchedVRF], peer)
			targetRouter.Neighbors = append(targetRouter.Neighbors, neighbor)
		}
		if len(targetRouter.Neighbors) == 0 {
			// we ended up with no neighbor
			continue
		}

		// append this router to the list of routers we will include in the
		// generated FRR config and track its index as we might need to add
		// imports to it
		vrfASNs[matchedVRF] = router.ASN
		routers = append(routers, targetRouter)
		targetRouterIndex := len(routers) - 1

		// VRFs are isolated in "auto" so no need to handle imports
		if ra.Spec.TargetVRF == "auto" {
			renderedNetworks = append(renderedNetworks, matchedNetwork)
			continue
		}

		// before handling imports, lets normalize the VRF for the default
		// network: when doing imports, the default VRF is is referred to as
		// "default" instead of ""
		if matchedVRF == "" {
			matchedVRF = types.DefaultNetworkName
		}

		// handle imports: when the target VRF is not "auto" we need to leak
		// between the target VRF and the selected networks, reciprocally
		// importing from each
		for _, vrf := range selectedNetworks.vrfs { // ordered
			// skip self
			if vrf == matchedVRF {
				continue
			}

			// import all other selected networks into this router's network.
			routers[targetRouterIndex].Imports = append(routers[targetRouterIndex].Imports, frrtypes.Import{VRF: vrf})

			// add an additional router to import the target VRF into selected
			// network
			importRouter := frrtypes.Router{
				ASN:     router.ASN,
				ID:      router.ID,
				Imports: []frrtypes.Import{{VRF: matchedVRF}},
			}
			if vrf != types.DefaultNetworkName {
				importRouter.VRF = vrf
			}
			routers = append(routers, importRouter)
		}
	}

	if template.globalRouter != nil && vrfASNs[""] == 0 {
		vrfASNs[""] = template.globalRouter.ASN
		vrfNeighbors[""] = template.globalPeers
	}

	// IP-VRF routers either use an explicit source router or inherit the
	// default router's ASN, as resolved during template validation.
	for _, planned := range template.ipVRFRouters {
		cfg := planned.config
		if covered != nil && planned.asn != 0 && len(selectedNetworks.hostNetworkSubnets[cfg.networkName]) > 0 {
			renderedNetworks = append(renderedNetworks, cfg.networkName)
		}
		vrfASNs[cfg.vrfName] = planned.asn
		if planned.inherited || !slices.ContainsFunc(routers, func(r frrtypes.Router) bool { return r.VRF == cfg.vrfName }) {
			routers = append(routers, frrtypes.Router{
				ASN: planned.asn, VRF: cfg.vrfName,
				Prefixes: selectedNetworks.hostNetworkSubnets[cfg.networkName],
			})
		}
	}

	// For EVPN underlay reachability, advertise VTEP IPs as /32 (or /128) in
	// the default-VRF router's address-family ipv4/ipv6 unicast.
	//
	// When targetVRF is "auto" or "", the main router matching loop above
	// already includes the default-VRF router in the generated routers, so we
	// append the VTEP IPs to it. When targetVRF is a specific VRF name (e.g.
	// "red"), only that VRF's router is in the generated set — the default-VRF
	// router is not included even though it exists in the source (confirmed by globalRouterASN > 0 above).
	// In that case we create a new default-VRF router from the source
	// to carry the VTEP IPs.
	if vtepIPs := selectedNetworks.vtepIPsByNode[nodeName]; len(vtepIPs) > 0 && vrfASNs[""] > 0 {
		// Build ToReceive prefix selectors from the VTEP CIDRs so each
		// node accepts routes for all VTEP IPs within these ranges.
		vtepReceiveSelectors := vtepCIDRPrefixSelectors(selectedNetworks.vtepCIDRs)

		defaultIdx := slices.IndexFunc(routers, func(r frrtypes.Router) bool { return r.VRF == "" })
		if defaultIdx >= 0 {
			// dedup, ordered
			// router level - injects the prefix into BGP
			routers[defaultIdx].Prefixes = sets.List(sets.New(routers[defaultIdx].Prefixes...).Insert(vtepIPs...))
			// neighbor level - advertise this node's VTEP IPs and accept VTEP
			// CIDRs; unnumbered neighbors carry both families
			for i := range routers[defaultIdx].Neighbors {
				allPrefixes := routers[defaultIdx].Prefixes
				isUnnumbered := isUnnumberedNeighbor(routers[defaultIdx].Neighbors[i])
				isIPV6 := utilnet.IsIPv6String(routers[defaultIdx].Neighbors[i].Address)
				routers[defaultIdx].Neighbors[i].ToAdvertise.Allowed.Prefixes = allPrefixes
				if !isUnnumbered {
					routers[defaultIdx].Neighbors[i].ToAdvertise.Allowed.Prefixes =
						util.MatchAllIPNetsStringFamily(isIPV6, allPrefixes)
				}
				for _, ps := range vtepReceiveSelectors {
					if isUnnumbered || utilnet.IsIPv6CIDRString(ps.Prefix) == isIPV6 {
						routers[defaultIdx].Neighbors[i].ToReceive.Allowed.Prefixes = append(
							routers[defaultIdx].Neighbors[i].ToReceive.Allowed.Prefixes, ps)
					}
				}
				if len(routers[defaultIdx].Neighbors[i].ToReceive.Allowed.Prefixes) > 0 {
					routers[defaultIdx].Neighbors[i].ToReceive.Allowed.Mode = frrtypes.AllowRestricted
				}
			}
		} else {
			// No default-VRF router in the generated set; clone the source's
			// default-VRF router to carry VTEP IPs, preserving all router-level
			// settings (ID, Imports, etc.) from the source.
			for _, router := range source.Spec.BGP.Routers {
				if router.VRF != "" {
					continue
				}
				vtepRouter := router
				// Only VTEP IPs go into Prefixes — the source's original
				// default-VRF prefixes are not this RA's responsibility
				// (they'd be advertised by a separate RA with targetVRF=""
				// or "auto" that includes the default-VRF router).
				vtepRouter.Prefixes = vtepIPs
				vtepRouter.Neighbors = nil // will rebuild below
				for _, neighbor := range router.Neighbors {
					// unnumbered neighbors carry both families
					isUnnumbered := isUnnumberedNeighbor(neighbor)
					isIPV6 := utilnet.IsIPv6String(neighbor.Address)
					filteredVTEPIPs := vtepIPs
					if !isUnnumbered {
						filteredVTEPIPs = util.MatchAllIPNetsStringFamily(isIPV6, vtepIPs)
					}
					if len(filteredVTEPIPs) == 0 {
						continue
					}
					n := neighbor
					n.ToAdvertise = frrtypes.Advertise{
						Allowed: frrtypes.AllowedOutPrefixes{
							Mode:     frrtypes.AllowRestricted,
							Prefixes: filteredVTEPIPs,
						},
					}
					for _, ps := range vtepReceiveSelectors {
						if isUnnumbered || utilnet.IsIPv6CIDRString(ps.Prefix) == isIPV6 {
							n.ToReceive.Allowed.Prefixes = append(n.ToReceive.Allowed.Prefixes, ps)
						}
					}
					if len(n.ToReceive.Allowed.Prefixes) > 0 {
						n.ToReceive.Allowed.Mode = frrtypes.AllowRestricted
					}
					vtepRouter.Neighbors = append(vtepRouter.Neighbors, n)
				}
				if len(vtepRouter.Neighbors) > 0 {
					routers = append(routers, vtepRouter)
				}
				break
			}
		}
	}

	// Generate raw config, if any.
	// TODO: once frr-k8s provides a typed API for this config, we can use that instead of raw config
	rawConfig := generateRawConfig(selectedNetworks, vrfNeighbors, vrfASNs)
	if len(routers) == 0 && rawConfig == "" {
		// we ended up with no routers and no raw config to generate, bail out
		return nil, nil
	}

	// FRR matches labels. Even a short node name cannot select a node whose
	// hostname label is missing.
	hostname := node.Labels[corev1.LabelHostname]
	if hostname == "" {
		return nil, fmt.Errorf("%w: waiting for %s label on node %q", errNodeNotReady, corev1.LabelHostname, nodeName)
	}

	new := &frrtypes.FRRConfiguration{}
	new.GenerateName = generateName
	new.Namespace = source.Namespace
	// label the FRRConfigurations with the RA name, we use this to find the
	// existing set of FRRConfigurations that need to be reconciled for a given
	// RA
	new.Labels = map[string]string{
		types.OvnRouteAdvertisementsKey:     ra.Name,
		types.OvnRouteAdvertisementsNodeKey: util.RouteAdvertisementsNodeLabel(nodeName),
	}
	new.Annotations = map[string]string{
		types.OvnRouteAdvertisementsKey: fmt.Sprintf("%s/%s/%s", ra.Name, source.Name, nodeName),
		// every value written below was validated when read
		types.OvnRouteAdvertisementsValidatedKey: "true",
	}
	new.Spec = source.Spec
	new.Spec.BGP.Routers = routers
	new.Spec.NodeSelector = metav1.LabelSelector{
		MatchLabels: map[string]string{
			corev1.LabelHostname: hostname,
		},
	}
	if rawConfig != "" {
		new.Spec.Raw = frrtypes.RawConfig{
			Priority: rawConfigPriority,
			Config:   rawConfig,
		}
	}

	// A matching VRF can lose all its peers during rendering. Count only
	// configurations we can publish. MAC-only EVPN uses the global peers
	// and has no per-network unicast router to count.
	if covered != nil {
		covered.Insert(renderedNetworks...)
		covered.Insert(template.macVRFNetworks...)
	}
	return new, nil
}

func getDPUHostGatewayNextHops(node *corev1.Node) (map[bool]string, error) {
	// ParseNodeL3GatewayAnnotation also requires the chassis ID for enabled
	// gateways, but reports a missing chassis ID as a config error. Check it
	// first so a DPU host that is still initializing leaves the RA pending.
	if _, err := util.ParseNodeChassisIDAnnotation(node); err != nil {
		if util.IsAnnotationNotSetError(err) {
			return nil, fmt.Errorf("%w: waiting for chassis ID annotation to be set for DPU host node %q: %w",
				errPending, node.Name, err)
		}
		return nil, fmt.Errorf("%w: failed to parse chassis ID annotation for DPU host node %q: %w",
			errNodeNotReady, node.Name, err)
	}

	gatewayConfig, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			return nil, fmt.Errorf("%w: waiting for L3 gateway annotation to be set for DPU host node %q: %w",
				errPending, node.Name, err)
		}
		return nil, fmt.Errorf("%w: failed to parse L3 gateway annotation for DPU host node %q: %w",
			errNodeNotReady, node.Name, err)
	}
	nextHops := map[bool]string{}
	for _, ipNet := range gatewayConfig.IPAddresses {
		if ipNet == nil || ipNet.IP == nil {
			continue
		}
		nextHops[ipNet.IP.To4() == nil] = ipNet.IP.String()
	}
	if len(nextHops) == 0 {
		return nil, fmt.Errorf("%w: no shared gateway IP addresses found for DPU host node %q", errNodeNotReady, node.Name)
	}
	return nextHops, nil
}

func (c *Controller) getDPUHostGatewayNextHops(
	node *corev1.Node,
	selectedNetworks *selectedNetworks,
	networkName string,
) (map[bool]string, error) {
	if config.Gateway.Mode != config.GatewayModeShared {
		return nil, nil
	}
	if _, err := util.GetNodePrimaryDPUHostAddrAnnotation(node); err != nil {
		if util.IsAnnotationNotSetError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: failed to parse primary DPU host address annotation for node %q: %w",
			errNodeNotReady, node.Name, err)
	}

	uplinkName := selectedNetworks.networkUplinks[networkName]
	if uplinkName == "" || c.uplinkStateLister == nil {
		return getDPUHostGatewayNextHops(node)
	}
	// This network uses an explicit uplink, so an unresolved UplinkState must
	// not fall back to the primary gateway's address: that would generate a
	// next hop on the wrong uplink. Treat it as a node readiness failure;
	// UplinkState status changes requeue the RA when resolution completes.
	stateName := uplinkutil.StateName(uplinkName, node.Name)
	state, err := uplinkutil.GetState(c.uplinkStateLister, uplinkName, node.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: waiting for UplinkState %q to be set for DPU host node %q",
				errPending, stateName, node.Name)
		}
		return nil, fmt.Errorf("%w: failed to get UplinkState %q for DPU host node %q: %w",
			errNodeNotReady, stateName, node.Name, err)
	}
	if !meta.IsStatusConditionTrue(state.Status.Conditions, uplinkv1alpha1.UplinkStateConditionResolved) {
		return nil, fmt.Errorf("%w: waiting for UplinkState %q to become resolved for DPU host node %q",
			errPending, stateName, node.Name)
	}

	nextHops := map[bool]string{}
	for _, ipCIDR := range state.Status.IPAddresses {
		ip, _, err := net.ParseCIDR(string(ipCIDR))
		if err != nil {
			return nil, fmt.Errorf("%w: failed to parse UplinkState %q IP address %q: %w",
				errNodeNotReady, stateName, ipCIDR, err)
		}
		nextHops[ip.To4() == nil] = ip.String()
	}
	if len(nextHops) == 0 {
		return nil, fmt.Errorf("%w: no IP addresses found in UplinkState %q for DPU host node %q",
			errNodeNotReady, stateName, node.Name)
	}
	return nextHops, nil
}

// vtepCIDRPrefixSelectors converts VTEP CIDRs into FRR PrefixSelectors that
// accept host routes (/32 for IPv4, /128 for IPv6) within each CIDR range.
func vtepCIDRPrefixSelectors(cidrs []string) []frrtypes.PrefixSelector {
	selectors := make([]frrtypes.PrefixSelector, 0, len(cidrs))
	for _, cidr := range cidrs {
		var le uint32
		if utilnet.IsIPv6CIDRString(cidr) {
			le = 128
		} else {
			le = 32
		}
		selectors = append(selectors, frrtypes.PrefixSelector{Prefix: cidr, LE: le, GE: le})
	}
	return selectors
}

// A reconcile only runs once the informer has delivered this RA's own writes,
// so the cache is current with respect to them.
func (c *Controller) listExistingFRRConfigurations(ra string) ([]*frrtypes.FRRConfiguration, error) {
	configs, err := c.frrLister.List(labels.Set{types.OvnRouteAdvertisementsKey: ra}.AsSelector())
	if err != nil {
		return nil, fmt.Errorf("failed to list FRRConfigurations for RouteAdvertisements %q: %w", ra, err)
	}
	// The label alone does not make an object one we generated.
	return slices.DeleteFunc(configs, func(cfg *frrtypes.FRRConfiguration) bool {
		return cfg.Annotations[types.OvnRouteAdvertisementsKey] == ""
	}), nil
}

// deleteIncompleteVTEPConfigurations removes rejected generated configurations
// while retaining other nodes' validated configurations on incomplete generation.
func (c *Controller) deleteIncompleteVTEPConfigurations(ctx context.Context, name string, invalidNodes sets.Set[string]) (bool, error) {
	existing, err := c.listExistingFRRConfigurations(name)
	if err != nil {
		return false, err
	}
	var hadUpdates bool
	var errs []error
	var remaining []*frrtypes.FRRConfiguration
	for _, config := range existing {
		key := strings.Split(config.Annotations[types.OvnRouteAdvertisementsKey], "/")
		nodeName := ""
		if len(key) == 3 {
			nodeName = key[2]
		}
		if !invalidNodes.Has(nodeName) {
			remaining = append(remaining, config)
			continue
		}
		err = c.frrClient.ApiV1beta1().FRRConfigurations(config.Namespace).Delete(ctx, config.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("failed withdrawing FRRConfiguration %s/%s: %w", config.Namespace, config.Name, err))
			continue
		}
		c.pendingFRR.deleted(name, config)
		hadUpdates = true
	}
	c.recordFRRConfigurationMetrics(name, remaining, len(errs) > 0)
	return hadUpdates, errors.Join(errs...)
}

// A node can keep advertising pod subnets after an EgressIP moves away.
// Remove the moved addresses from its existing objects before allowing any
// new owner to publish. Starting with the old spec also handles two nodes
// swapping addresses: neither withdrawal can introduce the incoming address.
func (c *Controller) withdrawMovedEgressIPs(ctx context.Context, configs []*frrtypes.FRRConfiguration, node string, owners map[netip.Prefix]string) (bool, error) {
	moved := func(cidr string) bool {
		prefix, err := netip.ParsePrefix(cidr)
		owner, assigned := owners[prefix]
		return err == nil && assigned && owner != node
	}
	var changed bool
	var errs []error
	for i, cfg := range configs {
		if !slices.ContainsFunc(cfg.Spec.BGP.Routers, func(r frrtypes.Router) bool {
			return slices.ContainsFunc(r.Prefixes, moved)
		}) {
			continue
		}
		withdrawal := cfg.DeepCopy()
		for j := range withdrawal.Spec.BGP.Routers {
			router := &withdrawal.Spec.BGP.Routers[j]
			router.Prefixes = slices.DeleteFunc(router.Prefixes, moved)
			for k := range router.Neighbors {
				allowed := &router.Neighbors[k].ToAdvertise.Allowed
				allowed.Prefixes = slices.DeleteFunc(allowed.Prefixes, moved)
			}
		}
		updated, err := c.frrClient.ApiV1beta1().FRRConfigurations(cfg.Namespace).Update(ctx, withdrawal, metav1.UpdateOptions{FieldManager: fieldManager})
		if err != nil {
			errs = append(errs, fmt.Errorf("failed withdrawing moved EgressIPs from FRRConfiguration %s/%s: %w", cfg.Namespace, cfg.Name, err))
			continue
		}
		configs[i] = updated
		c.pendingFRR.written(cfg.Labels[types.OvnRouteAdvertisementsKey], updated)
		metrics.RecordRouteAdvertisementFRRConfigurationProcess(cfg.Labels[types.OvnRouteAdvertisementsKey], frrConfigOpUpdate)
		changed = true
	}
	return changed, errors.Join(errs...)
}

// updateFRRConfigurations withdraws old objects for any missing desired key,
// including failed templates on nodes where another template still works.
// A failed withdrawal blocks replacements on that node. An EgressIP can move
// between two healthy nodes right after the old owner's object was written.
// The reconcile waits for that write to reach the cache, so its withdrawal
// can block publication on the new owner.
func (c *Controller) updateFRRConfigurations(ctx context.Context, ra string, result *frrGenerationResult) (bool, error) {
	var hadUpdates bool
	frrConfigs, err := c.listExistingFRRConfigurations(ra)
	if err != nil {
		return false, err
	}
	// Same-named templates can share an RA/template/node annotation. Each
	// generated object inherits its template's namespace, so use both to match it.
	keyFor := func(cfg *frrtypes.FRRConfiguration) cache.ObjectName {
		return cache.NewObjectName(cfg.Namespace, cfg.Annotations[types.OvnRouteAdvertisementsKey])
	}
	existing := make(map[cache.ObjectName][]*frrtypes.FRRConfiguration, len(frrConfigs))
	for _, frrConfig := range frrConfigs {
		key := keyFor(frrConfig)
		if key.Name == "" {
			continue
		}
		existing[key] = append(existing[key], frrConfig)
	}

	// Delete obsolete configurations before publishing replacements, particularly
	// when an EgressIP moves away from a node whose configuration cannot be generated.
	// FRR applies these changes asynchronously; deletion does not confirm BGP
	// route withdrawal.
	desiredKeys := sets.New[cache.ObjectName]()
	for _, cfg := range result.frrConfigs {
		desiredKeys.Insert(keyFor(cfg))
	}
	var withdrawalErrors []error
	generationErrors := len(result.nodeErrors)
	defer func() {
		writeFailed := len(withdrawalErrors) > 0 || len(result.nodeErrors) > generationErrors
		c.recordFRRConfigurationMetrics(ra, result.frrConfigs, writeFailed)
	}()
	blockedNodes := sets.New[string]()
	unknownNode := false
	for key, configs := range existing {
		if desiredKeys.Has(key) {
			if len(result.egressIPOwners) > 0 {
				node := key.Name[strings.LastIndexByte(key.Name, '/')+1:]
				changed, err := c.withdrawMovedEgressIPs(ctx, configs, node, result.egressIPOwners)
				hadUpdates = hadUpdates || changed
				if err != nil {
					withdrawalErrors = append(withdrawalErrors, err)
				}
			}
			continue
		}
		for _, cfg := range configs {
			if err := c.frrClient.ApiV1beta1().FRRConfigurations(cfg.Namespace).Delete(
				ctx, cfg.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				withdrawalErrors = append(withdrawalErrors, fmt.Errorf("failed to delete FRRConfiguration %s/%s for %s: %w", cfg.Namespace, cfg.Name, key.Name, err))
				parts := strings.Split(key.Name, "/")
				if len(parts) != 3 || parts[0] != ra || parts[1] == "" || parts[2] == "" {
					unknownNode = true
				} else {
					blockedNodes.Insert(parts[2])
				}
				continue
			}
			c.pendingFRR.deleted(ra, cfg)
			metrics.RecordRouteAdvertisementFRRConfigurationProcess(ra, frrConfigOpDelete)
			hadUpdates = true
		}
		delete(existing, key)
	}
	// Pod-only writes on other nodes are independent of these withdrawals.
	// EgressIP replacements must wait for the old owner; an unreadable node
	// key also prevents us from establishing that the writes are independent.
	if len(withdrawalErrors) > 0 && (len(result.egressIPOwners) > 0 || unknownNode) {
		return hadUpdates, errors.Join(withdrawalErrors...)
	}
	type writeOperation struct {
		config  *frrtypes.FRRConfiguration
		create  bool
		written *frrtypes.FRRConfiguration
		err     error
	}
	var writes []writeOperation
	// Match objects serially so each parallel write owns its configuration.
	for _, newFRRConfig := range result.frrConfigs {
		key := keyFor(newFRRConfig)
		node := key.Name[strings.LastIndexByte(key.Name, '/')+1:]
		if blockedNodes.Has(node) {
			// FRR merges templates targeting one node. Keep that node's current
			// set, including duplicates, until its obsolete template is removed.
			delete(existing, key)
			continue
		}
		oldFRRConfigs := existing[key]

		if len(oldFRRConfigs) == 0 {
			writes = append(writes, writeOperation{config: newFRRConfig, create: true})
			continue
		}

		// If it already exists, update. Unexpected user actions can lead us to
		// have multiple FRRConfigurations with the same key, in that case we
		// pick one to update and delete the rest.
		oldFRRConfig := oldFRRConfigs[len(oldFRRConfigs)-1]
		existing[key] = oldFRRConfigs[:len(oldFRRConfigs)-1]

		// no changes needed so skip
		if reflect.DeepEqual(newFRRConfig.Spec, oldFRRConfig.Spec) &&
			oldFRRConfig.Annotations[types.OvnRouteAdvertisementsValidatedKey] == newFRRConfig.Annotations[types.OvnRouteAdvertisementsValidatedKey] &&
			oldFRRConfig.Labels[types.OvnRouteAdvertisementsNodeKey] == newFRRConfig.Labels[types.OvnRouteAdvertisementsNodeKey] {
			metrics.RecordRouteAdvertisementFRRConfigurationProcess(ra, frrConfigOpUnchanged)
			continue
		}

		// otherwise update
		newFRRConfig.Name = oldFRRConfig.Name
		newFRRConfig.ResourceVersion = oldFRRConfig.ResourceVersion
		writes = append(writes, writeOperation{config: newFRRConfig})
	}

	// Overlap admission webhook latency, while allowing independent writes to
	// finish after another fails. Collect results serially before deleting duplicates.
	var wg sync.WaitGroup
	writeSlots := make(chan struct{}, frrConfigurationWriteWorkers)
	for i := range writes {
		op := &writes[i]
		writeSlots <- struct{}{}
		wg.Go(func() {
			defer func() { <-writeSlots }()
			client := c.frrClient.ApiV1beta1().FRRConfigurations(op.config.Namespace)
			if op.create {
				op.written, op.err = client.Create(ctx, op.config, metav1.CreateOptions{FieldManager: fieldManager})
			} else {
				op.written, op.err = client.Update(ctx, op.config, metav1.UpdateOptions{FieldManager: fieldManager})
			}
		})
	}
	wg.Wait()

	for _, op := range writes {
		key := keyFor(op.config)
		operation := frrConfigOpUpdate
		if op.create {
			operation = frrConfigOpCreate
		}
		if op.err != nil {
			result.nodeErrors = append(result.nodeErrors, fmt.Errorf("failed to %s FRRConfiguration %s/%s for %s: %w",
				operation, op.config.Namespace, op.config.Name, key, op.err))
			if !op.create {
				result.failedFRRUpdate = true
				delete(existing, key) // Keep duplicates until an update succeeds.
			}
			continue
		}
		c.pendingFRR.written(ra, op.written)
		metrics.RecordRouteAdvertisementFRRConfigurationProcess(ra, operation)
		hadUpdates = true
	}

	// Remove duplicate objects for keys that were updated above.
	for key, obsoleteFRRConfigs := range existing {
		for _, obsoleteFRRConfig := range obsoleteFRRConfigs {
			err := c.frrClient.ApiV1beta1().FRRConfigurations(obsoleteFRRConfig.Namespace).Delete(
				ctx,
				obsoleteFRRConfig.Name,
				metav1.DeleteOptions{},
			)
			if err != nil && !apierrors.IsNotFound(err) {
				withdrawalErrors = append(withdrawalErrors, fmt.Errorf("failed to delete duplicate FRRConfiguration %s/%s for %s: %w", obsoleteFRRConfig.Namespace, obsoleteFRRConfig.Name, key.Name, err))
				continue
			}
			c.pendingFRR.deleted(ra, obsoleteFRRConfig)
			metrics.RecordRouteAdvertisementFRRConfigurationProcess(ra, frrConfigOpDelete)
			hadUpdates = true
		}
	}

	if len(withdrawalErrors) > 0 && len(result.egressIPOwners) == 0 {
		return hadUpdates, &nodeGenerationError{errs: withdrawalErrors}
	}
	return hadUpdates, errors.Join(withdrawalErrors...)
}

type nadUpdate struct {
	nad         *nadtypes.NetworkAttachmentDefinition
	annotations map[string]string
	waitForFRR  bool
}

func intentDependsOnFRR(ra string, refs []string, intents map[string]util.RouteAdvertisementsIntent, next *util.RouteAdvertisementsIntent) bool {
	old, known := intents[ra]
	if !known {
		// A legacy reference names the RA but omits its VRF. Its first
		// intent could move an existing export to a different VRF.
		return slices.Contains(refs, ra)
	}
	return next == nil || old.TargetVRF != next.TargetVRF || (old.EgressIP && !next.EgressIP)
}

// A network can have NADs in several namespaces. Advancing even one to a
// newer generation can switch the entire network's VRF or clear its EgressIP
// intent while FRR still has the old export. Independent networks can proceed.
func (c *Controller) independentNADUpdates(ra string, intent *util.RouteAdvertisementsIntent, updates []nadUpdate) ([]nadUpdate, error) {
	updates = slices.DeleteFunc(updates, func(u nadUpdate) bool { return u.waitForFRR })
	pendingCUDNs := sets.New[string]()
	for _, update := range updates {
		if name := owningCUDN(update.nad); name != "" {
			pendingCUDNs.Insert(name)
		}
	}
	if intent == nil || len(pendingCUDNs) == 0 {
		return updates, nil
	}

	// A sibling NAD has its own resource version, so the conditional patch
	// does not cover it. The cache has this RA's last patch to it.
	nads, err := c.nadLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list NADs before advancing intent for RouteAdvertisements %q: %w", ra, err)
	}
	held := sets.New[string]()
	for _, nad := range nads {
		name := owningCUDN(nad)
		if !pendingCUDNs.Has(name) {
			continue
		}
		refs, intents, err := parseNADAdvertisements(nad.Annotations)
		if err != nil {
			return nil, fmt.Errorf("failed to read RouteAdvertisements references on NAD %s/%s: %w", nad.Namespace, nad.Name, err)
		}
		if intentDependsOnFRR(ra, refs, intents, intent) {
			held.Insert(name)
		}
	}
	return slices.DeleteFunc(updates, func(u nadUpdate) bool { return held.Has(owningCUDN(u.nad)) }), nil
}

func (c *Controller) getNADUpdates(ra string, intent *util.RouteAdvertisementsIntent, nads []*nadtypes.NetworkAttachmentDefinition) ([]nadUpdate, error) {
	var updates []nadUpdate
	selected := sets.New[string]()
	for _, nad := range nads {
		selected.Insert(nad.Namespace + "/" + nad.Name)
	}

	// An RA can be deleted right after its last NAD patch. The reconcile
	// waited for that patch, so the cache cannot leave its intent behind.
	nads, err := c.nadLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list NADs for RouteAdvertisements %q: %w", ra, err)
	}

	for _, nad := range nads {
		ras, intents, err := parseNADAdvertisements(nad.Annotations)
		if err != nil {
			return updates, fmt.Errorf("failed to read RouteAdvertisements references on NAD %s/%s: %w", nad.Namespace, nad.Name, err)
		}
		raSet := sets.New(ras...)
		nadName := nad.Namespace + "/" + nad.Name
		next := intent
		if selected.Has(nadName) {
			raSet.Insert(ra)
			selected.Delete(nadName)
		} else {
			raSet.Delete(ra)
			next = nil
		}
		waitForFRR := intentDependsOnFRR(ra, ras, intents, next)
		oldIntent, hadIntent := intents[ra]
		if next != nil {
			intents[ra] = *next
		} else {
			delete(intents, ra)
		}
		newIntent, hasIntent := intents[ra]

		annotations := map[string]string{}
		if len(ras) != raSet.Len() {
			nadRAjson, err := json.Marshal(raSet.UnsortedList())
			if err != nil {
				return updates, err
			}
			annotations[types.OvnRouteAdvertisementsKey] = string(nadRAjson)
		}
		if hadIntent != hasIntent || oldIntent != newIntent {
			intentJSON, err := json.Marshal(intents)
			if err != nil {
				return updates, err
			}
			annotations[types.OvnRouteAdvertisementsIntentKey] = string(intentJSON)
		}
		if len(annotations) > 0 {
			updates = append(updates, nadUpdate{
				nad:         nad,
				annotations: annotations,
				waitForFRR:  waitForFRR,
			})
		}
	}
	if selected.Len() != 0 {
		return updates, fmt.Errorf("failed to annotate NADs that were not found %v", selected.UnsortedList())
	}
	return updates, nil
}

func (c *Controller) applyNADUpdates(ctx context.Context, ra string, updates []nadUpdate) (bool, error) {
	k := kube.KubeOVN{NADClient: c.nadClient}
	for i, update := range updates {
		// Two RAs can select the same NAD before the informer sees the first
		// patch. Retry from a newer version to preserve the other RA's entry.
		nad := update.nad
		patched, err := k.SetAnnotationsOnNADAtVersion(
			ctx,
			nad.Namespace,
			nad.Name,
			nad.ResourceVersion,
			update.annotations,
			fieldManager,
		)
		if err != nil {
			return i > 0, fmt.Errorf("failed to annotate NAD %q: %w", nad.Name, err)
		}
		c.pendingNAD.written(ra, patched)
		metrics.RecordRouteAdvertisementNADUpdate(ra)
	}
	return len(updates) > 0, nil
}

func parseNADAdvertisements(annotations map[string]string) ([]string, map[string]util.RouteAdvertisementsIntent, error) {
	var refs []string
	if raw := annotations[types.OvnRouteAdvertisementsKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &refs); err != nil {
			return nil, nil, err
		}
	}
	intents := map[string]util.RouteAdvertisementsIntent{}
	if raw := annotations[types.OvnRouteAdvertisementsIntentKey]; raw != "" {
		// A selected pod advertisement can replace malformed intent when it writes its entry.
		if err := json.Unmarshal([]byte(raw), &intents); err != nil || intents == nil {
			intents = map[string]util.RouteAdvertisementsIntent{}
		}
	}
	return refs, intents, nil
}

// updateRAStatus reports input acceptance separately from programming failures.
func (c *Controller) updateRAStatus(ctx context.Context, ra *ratypes.RouteAdvertisements, hadUpdates bool, result frrGenerationResult) error {
	if ra == nil {
		return nil
	}

	err := result.failureError()
	partial := result.partialNodeFailure()
	cstatus := metav1.ConditionTrue
	if err != nil && !partial {
		cstatus = metav1.ConditionFalse
	}

	status := "Accepted"
	reason := "Accepted"
	msg := "ovn-kubernetes cluster-manager validated the resource and requested the necessary configuration changes"
	if partial {
		msg = "ovn-kubernetes cluster-manager accepted the resource; some node configurations are incomplete (see Degraded)"
	}
	if err != nil && !partial {
		status = fmt.Sprintf("Not Accepted: %v", err)
		msg = err.Error()
		switch {
		case result.hasRetryableNodeError():
			reason = "InternalError"
		case errors.Is(err, errConfig):
			reason = "ConfigurationError"
		case errors.Is(err, errPending):
			reason = "ConfigurationPending"
		default:
			reason = "InternalError"
		}
	}

	// MergeStatusCondition reports whether the condition differs from the
	// existing one (in status, reason, message or observed generation),
	// preserves the last transition time when the condition status is
	// unchanged and trims the message to the maximum length allowed for
	// conditions
	condition, changed := util.MergeStatusCondition(ra.Status.Conditions, metav1.Condition{
		Type:               conditionTypeAccepted,
		Status:             cstatus,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: ra.Generation,
	})
	degradedStatus := metav1.ConditionFalse
	degradedReason := "ConfigurationSucceeded"
	degradedMessage := "All requested node configurations were applied"
	if err != nil {
		degradedStatus = metav1.ConditionTrue
		degradedReason = "ReconciliationFailed"
		degradedMessage = err.Error()
		if partial {
			degradedReason = "NodeConfigurationFailed"
		}
	}
	degraded, degradedChanged := util.MergeStatusCondition(ra.Status.Conditions, metav1.Condition{
		Type:               conditionTypeDegraded,
		Status:             degradedStatus,
		Reason:             degradedReason,
		Message:            degradedMessage,
		ObservedGeneration: ra.Generation,
	})
	updateStatus := changed || degradedChanged || hadUpdates
	if !updateStatus {
		// Record the metric from the existing API-confirmed condition so it is
		// populated after controller restarts, where the informer fires synthetic
		// creates for all RAs but the condition hasn't changed.
		metrics.RecordRouteAdvertisementCondition(ra.Name, conditionTypeAccepted, cstatus)
		metrics.RecordRouteAdvertisementCondition(ra.Name, conditionTypeDegraded, degradedStatus)
		return nil
	}

	if (changed || degradedChanged) && err != nil {
		// errConfig/errPending are not retried, so make them visible in the
		// logs and not just in the status; log only when the condition changes
		// to avoid repeating the same warning on every reconcile of a steady
		// error state, which can otherwise still update the status through
		// hadUpdates
		klog.Warningf("Reconciling RouteAdvertisements %q failed: %v", ra.Name, err)
	}

	_, err = c.raClient.K8sV1().RouteAdvertisements().ApplyStatus(
		ctx,
		raapply.RouteAdvertisements(ra.Name).WithStatus(
			raapply.RouteAdvertisementsStatus().WithStatus(status).WithConditions(
				util.ConditionToApply(condition),
				util.ConditionToApply(degraded),
			),
		),
		metav1.ApplyOptions{
			FieldManager: fieldManager,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to apply status for RouteAdvertisements %q: %w", ra.Name, err)
	}
	metrics.RecordRouteAdvertisementCondition(ra.Name, conditionTypeAccepted, cstatus)
	metrics.RecordRouteAdvertisementCondition(ra.Name, conditionTypeDegraded, degradedStatus)

	return nil
}

func owningCUDN(nad *nadtypes.NetworkAttachmentDefinition) string {
	owner := metav1.GetControllerOfNoCopy(nad)
	if owner != nil && owner.Kind == cudnController.Kind && owner.APIVersion == cudnController.GroupVersion().String() {
		return owner.Name
	}
	return ""
}

func (c *Controller) getSelectedNADs(ctx context.Context, networkSelectors apitypes.NetworkSelectors) ([]*nadtypes.NetworkAttachmentDefinition, error) {
	var selected []*nadtypes.NetworkAttachmentDefinition
	for _, networkSelector := range networkSelectors {
		switch networkSelector.NetworkSelectionType {
		case apitypes.DefaultNetwork:
			// if we are selecting the default networkdefault network label,
			// make sure a NAD exists for it
			nad, err := util.EnsureDefaultNetworkNAD(ctx, c.nadLister, c.nadClient)
			if err != nil {
				return nil, fmt.Errorf("failed to ensure default network NAD: %w", err)
			}
			selected = append(selected, nad)
		case apitypes.ClusterUserDefinedNetworks:
			nadSelector, err := metav1.LabelSelectorAsSelector(&networkSelector.ClusterUserDefinedNetworkSelector.NetworkSelector)
			if err != nil {
				return nil, fmt.Errorf("%w: invalid clusterUserDefinedNetworkSelector.networkSelector: %w", errConfig, err)
			}
			nads, err := c.nadLister.List(nadSelector)
			if err != nil {
				return nil, err
			}
			for _, nad := range nads {
				if owningCUDN(nad) != "" {
					selected = append(selected, nad)
				}
			}
		default:
			return nil, fmt.Errorf("%w: unsupported network selection type %s", errConfig, networkSelector.NetworkSelectionType)
		}
	}

	return selected, nil
}

// getEgressIPsByNodesByNetworks iterates all existing egress IPs that apply to
// any of the provided networks and returns a "node -> network -> eips"
// map.
func (c *Controller) getEgressIPsByNodesByNetworks(networks sets.Set[string]) (map[string]map[string]sets.Set[string], error) {
	eipsByNodesByNetworks := map[string]map[string]sets.Set[string]{}
	addEgressIPsByNodesByNetwork := func(eipsByNodes map[string]string, network string) {
		for node, eip := range eipsByNodes {
			if eipsByNodesByNetworks[node] == nil {
				eipsByNodesByNetworks[node] = map[string]sets.Set[string]{}
			}
			if eipsByNodesByNetworks[node][network] == nil {
				eipsByNodesByNetworks[node][network] = sets.New[string]()
			}
			eipsByNodesByNetworks[node][network].Insert(eip)
		}
	}

	addEgressIPsByNodesByNetworkSelector := func(eipsByNodes map[string]string, namespaceSelector *metav1.LabelSelector) error {
		nsSelector, err := metav1.LabelSelectorAsSelector(namespaceSelector)
		if err != nil {
			return err
		}
		selected, err := c.namespaceLister.List(nsSelector)
		if err != nil {
			return err
		}
		for _, namespace := range selected {
			namespaceNetwork := c.nm.GetActiveNetworkForNamespaceFast(namespace.Name)
			networkName := namespaceNetwork.GetNetworkName()
			if networks.Has(networkName) {
				addEgressIPsByNodesByNetwork(eipsByNodes, networkName)
			}
		}
		return nil
	}

	eips, err := c.eipLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, eip := range eips {
		eipsByNodes := make(map[string]string, len(eip.Status.Items))
		for _, item := range eip.Status.Items {
			// skip unassigned EIPs
			if item.EgressIP == "" || item.Node == "" {
				continue
			}

			ip := item.EgressIP + util.GetIPFullMaskString(item.EgressIP)
			eipsByNodes[item.Node] = ip
		}
		if len(eipsByNodes) == 0 {
			continue
		}

		err = addEgressIPsByNodesByNetworkSelector(eipsByNodes, &eip.Spec.NamespaceSelector)
		if err != nil {
			return nil, err
		}
	}

	return eipsByNodesByNetworks, nil
}

// isOwnUpdate checks if an object was updated by us last, as indicated by its
// managed fields. Used to avoid reconciling an update that we made ourselves.
func isOwnUpdate(managedFields []metav1.ManagedFieldsEntry) bool {
	return util.IsLastUpdatedByManager(fieldManager, managedFields)
}

func (c *Controller) raNeedsUpdate(oldObj, newObj *ratypes.RouteAdvertisements) bool {
	if raSelectionChanged(oldObj, newObj) {
		if util.IsEVPNEnabled() {
			c.raController.ReconcileAll()
		}
		return true
	}
	// Recovery can precede the watch event for Degraded=True. Once the
	// cache sees the transition, recheck this RA. Changes to an error
	// message alone follow the normal retry backoff.
	for _, kind := range []string{conditionTypeAccepted, conditionTypeDegraded} {
		if meta.IsStatusConditionTrue(oldObj.Status.Conditions, kind) != meta.IsStatusConditionTrue(newObj.Status.Conditions, kind) {
			return true
		}
	}
	return false
}

// onRADelete releases claims even when the deleted RA had no generated config.
func (c *Controller) onRADelete(_ *ratypes.RouteAdvertisements) {
	if util.IsEVPNEnabled() {
		c.raController.ReconcileAll()
	}
}

func raSelectionChanged(oldObj, newObj *ratypes.RouteAdvertisements) bool {
	return oldObj == nil || newObj == nil || oldObj.Generation != newObj.Generation
}

func (c *Controller) frrConfigurationNeedsUpdate(oldObj, newObj *frrtypes.FRRConfiguration) bool {
	generated := (oldObj != nil && strings.HasPrefix(oldObj.Name, generateName)) ||
		(newObj != nil && strings.HasPrefix(newObj.Name, generateName))
	// With EVPN, other RAs use generated prefixes to identify the VTEP owner.
	sharedUnderlay := generated && util.IsEVPNEnabled()
	if newObj != nil {
		// Our own write coming back needs no reconcile, unless its RA was
		// held back until the cache had it.
		ra, held, own := c.pendingFRR.observed(newObj)
		if held {
			c.raController.Reconcile(ra)
		}
		if own && !sharedUnderlay {
			return false
		}
	}
	if newObj != nil && !sharedUnderlay {
		own := isOwnUpdate(newObj.ManagedFields)
		// The RA may have dropped a template or prefix while the informer
		// still showed the spec from before this write.
		ra := newObj.Labels[types.OvnRouteAdvertisementsKey]
		if generated && ra != "" && (oldObj == nil || own && oldObj.Generation != newObj.Generation) {
			c.raController.Reconcile(ra)
			return false
		}
		if own {
			return false
		}
	}
	return oldObj == nil || newObj == nil || oldObj.Generation != newObj.Generation ||
		!reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		oldObj.Annotations[types.OvnRouteAdvertisementsKey] != newObj.Annotations[types.OvnRouteAdvertisementsKey]
}

func (c *Controller) nadNeedsUpdate(oldObj, newObj *nadtypes.NetworkAttachmentDefinition) bool {
	var patchOwner string
	if newObj != nil {
		ra, held, own := c.pendingNAD.observed(newObj)
		if held {
			c.raController.Reconcile(ra)
		}
		if own {
			patchOwner = ra
		}
	}
	if newObj != nil && (patchOwner != "" || isOwnUpdate(newObj.ManagedFields)) && (oldObj == nil || reflect.DeepEqual(oldObj.Labels, newObj.Labels)) {
		var oldAnnotations map[string]string
		if oldObj != nil {
			oldAnnotations = oldObj.Annotations
		}
		if oldAnnotations[types.OvnRouteAdvertisementsKey] == newObj.Annotations[types.OvnRouteAdvertisementsKey] &&
			oldAnnotations[types.OvnRouteAdvertisementsIntentKey] == newObj.Annotations[types.OvnRouteAdvertisementsIntentKey] {
			return false
		}
		// An RA can drop this NAD before its last patch reaches the cache.
		// Other RAs sharing the NAD only need a pass if their entry changed.
		oldRefs, oldIntents, oldErr := parseNADAdvertisements(oldAnnotations)
		newRefs, newIntents, newErr := parseNADAdvertisements(newObj.Annotations)
		if oldErr != nil || newErr != nil {
			return true
		}
		changed := sets.New(oldRefs...).SymmetricDifference(sets.New(newRefs...))
		for ra := range sets.KeySet(oldIntents).Union(sets.KeySet(newIntents)) {
			before, wasPresent := oldIntents[ra]
			after, isPresent := newIntents[ra]
			if wasPresent != isPresent || before != after {
				changed.Insert(ra)
			}
		}
		// A relist can combine our patch with an earlier change to another
		// RA's entry. Only our own entry is already accounted for.
		changed.Delete(patchOwner)
		for ra := range changed {
			c.raController.Reconcile(ra)
		}
		return false
	}
	nadSupported := func(nad *nadtypes.NetworkAttachmentDefinition) bool {
		if nad == nil {
			return false
		}
		network, err := util.ParseNADInfo(nad)
		if err != nil {
			return true
		}
		return network.IsDefault() ||
			(network.IsPrimaryNetwork() && (network.TopologyType() == types.Layer3Topology || network.TopologyType() == types.Layer2Topology))
	}
	// ignore if we don't support this NAD
	if !nadSupported(oldObj) && !nadSupported(newObj) {
		return false
	}

	return oldObj == nil || newObj == nil ||
		!reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		oldObj.Annotations[types.OvnRouteAdvertisementsKey] != newObj.Annotations[types.OvnRouteAdvertisementsKey] ||
		oldObj.Annotations[types.OvnRouteAdvertisementsIntentKey] != newObj.Annotations[types.OvnRouteAdvertisementsIntentKey]
}

// nodeNeedsUpdate tracks inputs to advertisement generation, including the
// InternalIPs used to filter managed self-peers.
func nodeNeedsUpdate(oldObj, newObj *corev1.Node) bool {
	return oldObj == nil || newObj == nil ||
		!reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		!reflect.DeepEqual(oldObj.Status.Addresses, newObj.Status.Addresses) ||
		util.NodeSubnetAnnotationChanged(oldObj, newObj) ||
		util.NodePrimaryDPUHostAddrAnnotationChanged(oldObj, newObj) ||
		util.NodeL3GatewayAnnotationChanged(oldObj, newObj) ||
		util.NodeChassisIDAnnotationChanged(oldObj, newObj) ||
		util.NodeVTEPsAnnotationChanged(oldObj, newObj)
}

func uplinkStateNeedsUpdate(oldObj, newObj *uplinkv1alpha1.UplinkState) bool {
	return oldObj == nil || newObj == nil || !reflect.DeepEqual(oldObj.Status, newObj.Status)
}

func egressIPNeedsUpdate(oldObj, newObj *eiptypes.EgressIP) bool {
	if oldObj != nil && newObj != nil {
		return !reflect.DeepEqual(oldObj.Status, newObj.Status) || !reflect.DeepEqual(oldObj.Spec.NamespaceSelector, newObj.Spec.NamespaceSelector)
	}
	if oldObj != nil && len(oldObj.Status.Items) > 0 {
		return true
	}
	if newObj != nil && len(newObj.Status.Items) > 0 {
		return true
	}
	return false
}

func nsNeedsUpdate(oldObj, newObj *corev1.Namespace) bool {
	// we only care about label changes, added/deleted namespaces served by a
	// UDN will already be reflected in a network update
	return oldObj != nil && newObj != nil && !reflect.DeepEqual(oldObj.Labels, newObj.Labels)
}

func (c *Controller) reconcileFRRConfiguration(_ context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("Failed splitting FRFConfiguration reconcile key %q: %v", key, err)
		return nil
	}

	frrConfig, err := c.frrLister.FRRConfigurations(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	// A configuration we deleted ourselves affects no other RA, unless EVPN
	// RAs use its prefixes to identify the VTEP owner.
	if frrConfig == nil {
		ra, held, own := c.pendingFRR.deletionObserved(key)
		if held {
			c.raController.Reconcile(ra)
		}
		if own && !util.IsEVPNEnabled() {
			return nil
		}
	}

	// A template label or node-selector edit can affect both RAs that used it
	// before and RAs that match it now. Requeue all RAs so each recomputes its
	// selection and removes obsolete generated objects as well as adding new ones.
	c.raController.ReconcileAll()

	// on startup, we might be syncing a FRRConfiguration generated by us for a
	// RouteAdvertisements that does not exist, so make sure to reconcile it so
	// that the FRRConfiguration is deleted if needed
	if frrConfig != nil && frrConfig.Labels[types.OvnRouteAdvertisementsKey] != "" {
		c.raController.Reconcile(frrConfig.Labels[types.OvnRouteAdvertisementsKey])
	}

	return nil
}

func (c *Controller) reconcileNAD(_ context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("Failed splitting NAD reconcile key %q: %v", key, err)
		return nil
	}

	nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if nad == nil {
		if ra, held, _ := c.pendingNAD.deletionObserved(key); held {
			c.raController.Reconcile(ra)
		}
	}

	// A network can enter or leave an RA's selection without an RA spec edit.
	// Requeue all RAs so creation, deletion, and relabeling of NADs trigger both
	// generation for new matches and cleanup of advertisements that lost a match.
	c.raController.ReconcileAll()

	// on startup, we might be syncing a NAD annotated by us with a
	// RouteAdvertisements that does not longer exist, so make sure to reconcile
	// annotated RouteAdvertisements so that the annotation is updated
	// accordingly
	if nad != nil && nad.Annotations[types.OvnRouteAdvertisementsKey] != "" {
		var ras []string
		err := json.Unmarshal([]byte(nad.Annotations[types.OvnRouteAdvertisementsKey]), &ras)
		if err != nil {
			return err
		}
		for _, ra := range ras {
			c.raController.Reconcile(ra)
		}
	}

	return nil
}

// neighborKey returns the identifier of a neighbor as used in raw FRR config:
// the session address or, for unnumbered neighbors, the interface name.
func neighborKey(neighbor frrtypes.Neighbor) string {
	if neighbor.Address != "" {
		return neighbor.Address
	}
	return neighbor.Interface
}

// isUnnumberedNeighbor returns whether the neighbor is unnumbered, that is
// defined by interface rather than by address. Coherent with neighborKey, a
// neighbor that specifies both is considered defined by address.
func isUnnumberedNeighbor(neighbor frrtypes.Neighbor) bool {
	return neighbor.Address == "" && neighbor.Interface != ""
}

func (c *Controller) reconcileEgressIPs(_ context.Context, _ string) error {
	// reconcile RAs that advertise EIPs
	ras, err := c.raLister.List(labels.Everything())
	if err != nil {
		return err
	}

	for _, ra := range ras {
		if sets.New(ra.Spec.Advertisements...).Has(ratypes.EgressIP) {
			c.raController.Reconcile(ra.Name)
		}
	}

	return nil
}
