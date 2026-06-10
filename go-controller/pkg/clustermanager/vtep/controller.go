// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package vtep

import (
	"context"
	"errors"
	"fmt"
	"net"
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
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/reference"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	k8scontrollerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnlisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/applyconfiguration/vtep/v1"
	vtepclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned"
	vtepscheme "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned/scheme"
	vteplisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/listers/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	finalizerVTEP = "k8s.ovn.org/vtep-protection"
)

// Controller manages VTEP resources in the cluster manager.
type Controller struct {
	vtepClient     vtepclientset.Interface
	vtepLister     vteplisters.VTEPLister
	cudnLister     udnlisters.ClusterUserDefinedNetworkLister
	nodeLister     corelisters.NodeLister
	kube           kube.Interface
	vtepController controllerutil.Controller
	cudnController controllerutil.Controller
	nodeController controllerutil.Controller
	eventRecorder  record.EventRecorder

	// allocators holds per-VTEP IP allocators for managed-mode VTEPs.
	// Keyed by VTEP name. Created on first reconcile, updated on CIDR
	// changes, removed on VTEP deletion. Currently only accessed from
	// vtepController (Threadiness=1), but protected by a mutex for
	// safety if threadiness changes or cross-controller access is added.
	allocatorsMu sync.Mutex
	allocators   map[string]*vtepIPAllocator

	// cudnVTEPIndex tracks which VTEP each EVPN-enabled CUDN references
	// (cudnName → vtepName). Populated on CUDN create, consulted on CUDN
	// delete so we can re-queue only the specific VTEP instead of scanning
	// all VTEPs for every single CUDN deletion (since onDelete simply requeues
	// we don't even get to evaluate whether it was an EVPN-enabled CUDN or not).
	// Currently only accessed from cudnController, but protected by a mutex
	// since vtepController and cudnController run on separate goroutines and
	// future work may require cross-controller access.
	cudnVTEPIndexMu sync.RWMutex
	cudnVTEPIndex   map[string]string
}

// NewController creates a new VTEP controller.
func NewController(
	wf *factory.WatchFactory,
	ovnClient *util.OVNClusterManagerClientset,
	recorder record.EventRecorder,
) *Controller {
	vtepLister := wf.VTEPInformer().Lister()
	c := &Controller{
		vtepClient:    ovnClient.VTEPClient,
		vtepLister:    vtepLister,
		cudnLister:    wf.ClusterUserDefinedNetworkInformer().Lister(),
		nodeLister:    wf.NodeCoreInformer().Lister(),
		kube:          &kube.Kube{KClient: ovnClient.KubeClient},
		eventRecorder: recorder,
		allocators:    make(map[string]*vtepIPAllocator),
		cudnVTEPIndex: make(map[string]string),
	}

	vtepCfg := &controllerutil.ControllerConfig[vtepv1.VTEP]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.VTEPInformer().Informer(),
		Lister:         vtepLister.List,
		Reconcile:      c.reconcileVTEP,
		ObjNeedsUpdate: vtepNeedsUpdate,
		Threadiness:    1,
	}
	c.vtepController = controllerutil.NewController(
		"clustermanager-vtep-controller",
		vtepCfg,
	)

	cudnLister := wf.ClusterUserDefinedNetworkInformer().Lister()
	cudnCfg := &controllerutil.ControllerConfig[udnv1.ClusterUserDefinedNetwork]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.ClusterUserDefinedNetworkInformer().Informer(),
		Lister:         cudnLister.List,
		Reconcile:      c.reconcileCUDN,
		ObjNeedsUpdate: cudnNeedsUpdate,
		Threadiness:    1,
	}
	c.cudnController = controllerutil.NewController(
		"clustermanager-vtep-cudn-controller",
		cudnCfg,
	)

	nodeLister := wf.NodeCoreInformer().Lister()
	nodeCfg := &controllerutil.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.NodeCoreInformer().Informer(),
		Lister:         nodeLister.List,
		Reconcile:      c.reconcileNode,
		ObjNeedsUpdate: nodeNeedsUpdate,
		Threadiness:    1,
	}
	c.nodeController = controllerutil.NewController(
		"clustermanager-vtep-node-controller",
		nodeCfg,
	)

	return c
}

// Start begins the VTEP controller. It uses StartWithInitialSync so that
// event handlers are registered first (queuing events), then the initial
// sync restores allocator state from existing node annotations, and finally
// workers start processing the queue. This ensures IPs allocated before a
// restart are preserved and not reassigned.
func (c *Controller) Start() error {
	defer klog.Infof("Cluster manager VTEP controller started")
	return controllerutil.StartWithInitialSync(
		c.syncManagedAllocators,
		c.vtepController,
		c.cudnController,
		c.nodeController,
	)
}

// syncManagedAllocators restores allocator state for all managed VTEPs from
// existing node annotations. This runs once at startup before the controller
// workers begin, ensuring that previously allocated IPs are reserved in the
// allocator and won't be reassigned to different nodes.
func (c *Controller) syncManagedAllocators() error {
	vteps, err := c.vtepLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list VTEPs: %w", err)
	}

	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	var errs []error
	for _, vtep := range vteps {
		if vtep.Spec.Mode != vtepv1.VTEPModeManaged {
			continue
		}
		allocator, err := newVTEPIPAllocator(vtep.Spec.CIDRs)
		if err != nil {
			errs = append(errs, fmt.Errorf("VTEP %s: failed to create allocator: %w", vtep.Name, err))
			continue
		}
		markExistingAllocationsFromNodes(vtep.Name, allocator, nodes)
		c.allocatorsMu.Lock()
		c.allocators[vtep.Name] = allocator
		c.allocatorsMu.Unlock()
		klog.Infof("Synced allocator for managed VTEP %s", vtep.Name)
	}
	return errors.Join(errs...)
}

// Stop shuts down the VTEP controller.
func (c *Controller) Stop() {
	controllerutil.Stop(c.vtepController, c.cudnController, c.nodeController)
}

func (c *Controller) reconcileVTEP(key string) error {
	startTime := time.Now()
	vtepName := key
	klog.V(5).Infof("Reconciling VTEP %s", vtepName)
	defer func() {
		klog.V(5).Infof("Reconciling VTEP %s took %v", vtepName, time.Since(startTime))
	}()

	vtep, err := c.vtepLister.Get(vtepName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.requeueConflictingVTEPs(vtepName)
			return nil
		}
		return fmt.Errorf("failed to get VTEP %s: %w", vtepName, err)
	}

	if !vtep.DeletionTimestamp.IsZero() {
		return c.handleVTEPDeletion(vtep)
	}

	if err := c.ensureFinalizer(vtep); err != nil {
		return err
	}

	if err := c.validateCIDRsAcrossVTEPs(vtep); err != nil {
		existingCond := meta.FindStatusCondition(vtep.Status.Conditions, conditionTypeAccepted)
		if existingCond == nil || existingCond.Status != metav1.ConditionFalse || existingCond.Reason != reasonCIDROverlap || existingCond.Message != err.Error() {
			vtepRef, refErr := reference.GetReference(vtepscheme.Scheme, vtep)
			if refErr != nil {
				return fmt.Errorf("failed to get object reference for VTEP %s: %w", vtep.Name, refErr)
			}
			c.eventRecorder.Event(vtepRef, corev1.EventTypeWarning, reasonCIDROverlap, err.Error())
		}
		return c.updateStatusCondition(vtep, conditionTypeAccepted, metav1.ConditionFalse,
			reasonCIDROverlap, err.Error())
	}

	if err := c.validateNoIPv6VTEPsForEVPN(vtep); err != nil {
		existingCond := meta.FindStatusCondition(vtep.Status.Conditions, conditionTypeAccepted)
		if existingCond == nil || existingCond.Status != metav1.ConditionFalse || existingCond.Reason != reasonEVPNIPv6NotSupported || existingCond.Message != err.Error() {
			vtepRef, refErr := reference.GetReference(vtepscheme.Scheme, vtep)
			if refErr != nil {
				return fmt.Errorf("failed to get object reference for VTEP %s: %w", vtep.Name, refErr)
			}
			c.eventRecorder.Event(vtepRef, corev1.EventTypeWarning, reasonEVPNIPv6NotSupported, err.Error())
		}
		return c.updateStatusCondition(vtep, conditionTypeAccepted, metav1.ConditionFalse,
			reasonEVPNIPv6NotSupported, err.Error())
	}

	if err := c.validateManagedCIDRsAgainstNodeIPs(vtep); err != nil {
		existingCond := meta.FindStatusCondition(vtep.Status.Conditions, conditionTypeAccepted)
		if existingCond == nil || existingCond.Status != metav1.ConditionFalse || existingCond.Reason != reasonCIDROverlap || existingCond.Message != err.Error() {
			vtepRef, refErr := reference.GetReference(vtepscheme.Scheme, vtep)
			if refErr != nil {
				return fmt.Errorf("failed to get object reference for VTEP %s: %w", vtep.Name, refErr)
			}
			c.eventRecorder.Event(vtepRef, corev1.EventTypeWarning, reasonCIDROverlap, err.Error())
		}
		return c.updateStatusCondition(vtep, conditionTypeAccepted, metav1.ConditionFalse,
			reasonCIDROverlap, err.Error())
	}

	// When a VTEP transitions from Managed to Unmanaged, clean up the
	// CM-written annotations and drop the allocator so the node-side
	// controller can take over.
	//
	// User action required: after switching to Unmanaged, the user must
	// configure VTEP IPs on dedicated interfaces (e.g. dummy devices) on
	// each node. ovnkube-node will discover them. The CM-allocated IPs
	// on the evlo-* dummy devices (created by the managed-mode node
	// controller) are cleaned up automatically by the node side.
	//
	// There is a transient window between CM cleanup and node-side write
	// where the VTEP annotation entry is absent. During this window
	// validateNodeVTEPIPs reports AllocationFailed. The system self-heals
	// once the node writes its discovered IP.
	//
	// Race with node-side during the cleanup window: the CM removes the VTEP
	// entry from each node's annotation using RetryOnConflict+UpdateNodeStatus,
	// while ovnkube-node concurrently writes its discovered IP using the same
	// pattern (TODO: node-side should also use RetryOnConflict+UpdateNodeStatus;
	// currently it uses strategic merge patch). Both sides may 409-conflict
	// during cleanup. This is safe: the wasManaged guard ensures the CM only
	// runs cleanup once per mode switch. After cleanup the allocator is gone so
	// wasManaged is false on all subsequent reconciles, and the CM never touches
	// the annotation again. Node-side eventually wins and the system converges.
	// NOTE: Design isn't ideal but this is what happens when two processes share the
	// same annotation resource. Moving modes is expected to be a rare operation so we
	// can live with this.
	if vtep.Spec.Mode != vtepv1.VTEPModeManaged {
		c.allocatorsMu.Lock()
		_, wasManaged := c.allocators[vtep.Name]
		c.allocatorsMu.Unlock()
		if wasManaged {
			klog.Infof("VTEP %s switched from Managed to Unmanaged, cleaning up annotations", vtep.Name)
			if err := c.cleanupManagedVTEPAnnotations(vtep.Name); err != nil {
				return fmt.Errorf("failed to clean up managed VTEP annotations for %s: %w", vtep.Name, err)
			}
			c.allocatorsMu.Lock()
			delete(c.allocators, vtep.Name)
			c.allocatorsMu.Unlock()
		}
	}

	var allocationErr error
	if vtep.Spec.Mode == vtepv1.VTEPModeManaged {
		allocationErr = c.handleManagedMode(vtep)
	} else {
		allocationErr = c.validateNodeVTEPIPs(vtep)
	}
	if allocationErr != nil {
		existingCond := meta.FindStatusCondition(vtep.Status.Conditions, conditionTypeAccepted)
		if existingCond == nil || existingCond.Status != metav1.ConditionFalse || existingCond.Reason != reasonAllocationFailed || existingCond.Message != allocationErr.Error() {
			vtepRef, refErr := reference.GetReference(vtepscheme.Scheme, vtep)
			if refErr != nil {
				return fmt.Errorf("failed to get object reference for VTEP %s: %w", vtep.Name, refErr)
			}
			c.eventRecorder.Event(vtepRef, corev1.EventTypeWarning, reasonAllocationFailed, allocationErr.Error())
		}
		return c.updateStatusCondition(vtep, conditionTypeAccepted, metav1.ConditionFalse,
			reasonAllocationFailed, allocationErr.Error())
	}

	return c.updateStatusCondition(vtep, conditionTypeAccepted, metav1.ConditionTrue,
		reasonAllocated, "VTEP allocation succeeded")
}

// ensureFinalizer adds the VTEP protection finalizer unconditionally on every
// non-deleted VTEP. We always add it rather than waiting for a CUDN (or future
// consumer) to reference the VTEP, to avoid a race where the VTEP is deleted
// between the time a consumer starts referencing it and the controller notices.
// The actual deletion policy (checking CUDN references) is enforced in
// handleVTEPDeletion when the finalizer gates the delete.
func (c *Controller) ensureFinalizer(vtep *vtepv1.VTEP) error {
	if !k8scontrollerutil.ContainsFinalizer(vtep, finalizerVTEP) {
		_, err := c.vtepClient.K8sV1().VTEPs().Apply(
			context.Background(),
			vtepapply.VTEP(vtep.Name).WithFinalizers(finalizerVTEP),
			metav1.ApplyOptions{FieldManager: fieldManager, Force: true},
		)
		if err != nil {
			return fmt.Errorf("failed to add finalizer to VTEP %s: %w", vtep.Name, err)
		}
		klog.Infof("Added finalizer to VTEP %s", vtep.Name)
	}
	return nil
}

func (c *Controller) handleVTEPDeletion(vtep *vtepv1.VTEP) error {
	if !k8scontrollerutil.ContainsFinalizer(vtep, finalizerVTEP) {
		return nil
	}

	referencingCUDNs, err := c.getCUDNsReferencingVTEP(vtep.Name)
	if err != nil {
		return fmt.Errorf("failed to check CUDN references for VTEP %s: %w", vtep.Name, err)
	}

	if len(referencingCUDNs) > 0 {
		klog.Infof("VTEP %s is still referenced by CUDNs [%s], blocking deletion", vtep.Name, strings.Join(referencingCUDNs, ", "))
		return nil
	}

	c.allocatorsMu.Lock()
	_, wasManaged := c.allocators[vtep.Name]
	c.allocatorsMu.Unlock()

	if wasManaged || vtep.Spec.Mode == vtepv1.VTEPModeManaged {
		if err := c.cleanupManagedVTEPAnnotations(vtep.Name); err != nil {
			return fmt.Errorf("failed to clean up managed VTEP annotations for %s: %w", vtep.Name, err)
		}
		c.allocatorsMu.Lock()
		delete(c.allocators, vtep.Name)
		c.allocatorsMu.Unlock()
	}

	_, err = c.vtepClient.K8sV1().VTEPs().Apply(
		context.Background(),
		vtepapply.VTEP(vtep.Name),
		metav1.ApplyOptions{FieldManager: fieldManager, Force: true},
	)
	if err != nil {
		return fmt.Errorf("failed to remove finalizer from VTEP %s: %w", vtep.Name, err)
	}
	klog.Infof("Removed finalizer from VTEP %s, deletion unblocked", vtep.Name)
	return nil
}

// cleanupManagedVTEPAnnotations removes the VTEP entry from the
// k8s.ovn.org/vteps annotation on all nodes.
func (c *Controller) cleanupManagedVTEPAnnotations(vtepName string) error {
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	var errs []error
	for _, node := range nodes {
		if err := util.RemoveNodeVTEPEntry(node.Name, vtepName, c.nodeLister.Get, c.kube.UpdateNodeStatus); err != nil {
			errs = append(errs, fmt.Errorf("node %s: %w", node.Name, err))
		}
	}
	return errors.Join(errs...)
}

// getCUDNsReferencingVTEP returns the names of CUDNs that reference the given VTEP.
func (c *Controller) getCUDNsReferencingVTEP(vtepName string) ([]string, error) {
	cudns, err := c.cudnLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list CUDNs: %w", err)
	}
	var referencing []string
	for _, cudn := range cudns {
		if cudnReferencesVTEP(cudn, vtepName) {
			referencing = append(referencing, cudn.Name)
		}
	}
	return referencing, nil
}

func cudnReferencesVTEP(cudn *udnv1.ClusterUserDefinedNetwork, vtepName string) bool {
	if cudn.Spec.Network.EVPN != nil && cudn.Spec.Network.EVPN.VTEP == vtepName {
		return true
	}
	return false
}

// validateNoIPv6ForEVPN rejects VTEPs that contain IPv6 CIDRs when they are
// referenced by an EVPN CUDN. FRR does not support IPv6 VTEPs for EVPN
// transport. VTEPs not referenced by any EVPN CUDN are unaffected.
func (c *Controller) validateNoIPv6VTEPsForEVPN(vtep *vtepv1.VTEP) error {
	referencingCUDNs, err := c.getCUDNsReferencingVTEP(vtep.Name)
	if err != nil {
		return fmt.Errorf("failed to check CUDN references for VTEP %s: %w", vtep.Name, err)
	}
	if len(referencingCUDNs) == 0 {
		return nil
	}

	var errs []error
	for _, cidr := range vtep.Spec.CIDRs {
		_, ipNet, err := net.ParseCIDR(string(cidr))
		if err != nil {
			// shouldn't happen since CIDRs are validated by CEL
			errs = append(errs, fmt.Errorf("invalid CIDR %q in VTEP %s: %w", cidr, vtep.Name, err))
			continue
		}
		if ipNet.IP.To4() == nil {
			errs = append(errs, fmt.Errorf("VTEP %s contains IPv6 CIDR %s but is referenced by EVPN CUDNs [%s]; "+
				"IPv6 VTEPs are not supported for EVPN transport", vtep.Name, cidr, strings.Join(referencingCUDNs, ", ")))
		}
	}
	return errors.Join(errs...)
}

// validateCIDRsAcrossVTEPs checks that none of this VTEP's CIDRs overlap with
// any other VTEP's CIDRs. All overlapping VTEPs are re-queued so both sides
// of a conflict discover it and set Accepted=False.
//
// NOTE: this parses every VTEP's CIDRs on each reconciliation. If the number
// of VTEPs grows large we may need to cache parsed CIDRs to avoid repeated
// parsing overhead.
func (c *Controller) validateCIDRsAcrossVTEPs(vtep *vtepv1.VTEP) error {
	currentCIDRs, err := parseVTEPCIDRs(vtep)
	if err != nil {
		return fmt.Errorf("failed to parse CIDRs for VTEP %s: %w", vtep.Name, err)
	}

	allVTEPs, err := c.vtepLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list VTEPs: %w", err)
	}

	var conflicting []string
	var errs []error
	for _, other := range allVTEPs {
		if other.Name == vtep.Name {
			continue
		}
		// A VTEP with DeletionTimestamp and no finalizers is being garbage
		// collected by the API server. The informer cache may briefly lag
		// behind, so skip it to avoid false overlap with a dying VTEP.
		if !other.DeletionTimestamp.IsZero() && len(other.Finalizers) == 0 {
			continue
		}
		otherCIDRs, err := parseVTEPCIDRs(other)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to parse CIDRs for VTEP %s: %w", other.Name, err))
			continue
		}
		otherCond := meta.FindStatusCondition(other.Status.Conditions, conditionTypeAccepted)
		if util.NetworksOverlap(currentCIDRs, otherCIDRs) {
			conflicting = append(conflicting, other.Name)
			if otherCond == nil || otherCond.Status != metav1.ConditionFalse || otherCond.Reason != reasonCIDROverlap ||
				!vtepNameInMessage(otherCond.Message, vtep.Name) {
				c.vtepController.Reconcile(other.Name)
			}
		} else if otherCond != nil && otherCond.Status == metav1.ConditionFalse && otherCond.Reason == reasonCIDROverlap &&
			vtepNameInMessage(otherCond.Message, vtep.Name) {
			c.vtepController.Reconcile(other.Name)
		}
	}
	if len(conflicting) > 0 {
		sort.Strings(conflicting)
		errs = append(errs, fmt.Errorf("CIDRs overlap with VTEPs: [%s]",
			strings.Join(conflicting, ", ")))
	}
	return errors.Join(errs...)
}

// validateManagedCIDRsAgainstNodeIPs checks that a managed VTEP's CIDRs do
// not overlap with any node's host IPs (from k8s.ovn.org/host-cidrs). If the
// allocator hands out an IP that is already assigned to a node's primary
// interface, two nodes would claim the same IP on the same L2 segment.
// Unmanaged VTEPs are skipped because the user controls IP assignment.
func (c *Controller) validateManagedCIDRsAgainstNodeIPs(vtep *vtepv1.VTEP) error {
	if vtep.Spec.Mode != vtepv1.VTEPModeManaged {
		return nil
	}

	vtepCIDRs, err := parseVTEPCIDRs(vtep)
	if err != nil {
		return err
	}

	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	// We check every node because nodes can be on different subnets
	// (multi-rack, multi-AZ). A VTEP CIDR might overlap with one
	// subnet but not another, so we can't short-circuit after one node.
	var overlapping []string
	for _, node := range nodes {
		hostCIDRStrs, err := util.ParseNodeHostCIDRs(node)
		if err != nil {
			continue
		}
		hostNets := make([]*net.IPNet, 0, hostCIDRStrs.Len())
		for cidr := range hostCIDRStrs {
			_, ipNet, err := net.ParseCIDR(cidr)
			if err != nil {
				continue
			}
			hostNets = append(hostNets, ipNet)
		}
		if util.NetworksOverlap(vtepCIDRs, hostNets) {
			overlapping = append(overlapping, node.Name)
		}
	}

	if len(overlapping) > 0 {
		sort.Strings(overlapping)
		return fmt.Errorf("managed VTEP CIDRs overlap with node host IPs on nodes: [%s]",
			strings.Join(overlapping, ", "))
	}
	return nil
}

// requeueConflictingVTEPs re-queues VTEPs whose Accepted=False/CIDROverlap
// condition message mentions the deleted VTEP by name, so they can
// re-evaluate whether their conflicts are resolved.
func (c *Controller) requeueConflictingVTEPs(deletedVTEPName string) {
	allVTEPs, err := c.vtepLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list VTEPs during conflict requeue: %v", err)
		return
	}
	for _, other := range allVTEPs {
		cond := meta.FindStatusCondition(other.Status.Conditions, conditionTypeAccepted)
		if cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == reasonCIDROverlap &&
			vtepNameInMessage(cond.Message, deletedVTEPName) {
			klog.V(4).Infof("Re-queuing VTEP %s after deletion of conflicting VTEP %s", other.Name, deletedVTEPName)
			c.vtepController.Reconcile(other.Name)
		}
	}
}

// vtepNameInMessage checks whether name appears as an exact entry in the
// bracketed, comma-separated list embedded in the condition message.
// Message format: "CIDRs overlap with VTEPs: [vtep-a, vtep-b]"
func vtepNameInMessage(message, name string) bool {
	start := strings.Index(message, "[")
	end := strings.LastIndex(message, "]")
	if start == -1 || end == -1 || end <= start {
		return false
	}
	for _, entry := range strings.Split(message[start+1:end], ", ") {
		if entry == name {
			return true
		}
	}
	return false
}

func parseVTEPCIDRs(vtep *vtepv1.VTEP) ([]*net.IPNet, error) {
	cidrs := make([]*net.IPNet, 0, len(vtep.Spec.CIDRs))
	for _, c := range vtep.Spec.CIDRs {
		_, ipNet, err := net.ParseCIDR(string(c))
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", c, err)
		}
		cidrs = append(cidrs, ipNet)
	}
	return cidrs, nil
}

// validateNodeVTEPIPs validates that every node has a VTEP IP entry in the
// k8s.ovn.org/vteps annotation for this VTEP. For unmanaged mode, ovnkube-node
// discovers IPs from local interfaces and writes the annotation; this
// controller only checks that each node has a non-empty entry.
//
// We intentionally do not verify that the annotated IPs fall within the VTEP's
// configured CIDRs. The node-side controller is the authority for unmanaged
// VTEPs: it has visibility into local interfaces and filters out VIPs /
// secondary addresses before writing the annotation. If the node writes an
// out-of-range IP, that is a node-side misconfiguration, not something the CM
// should reject — doing so would create a fight between the two controllers.
// The RA controller reads the same annotation when generating FRR
// configurations.
func (c *Controller) validateNodeVTEPIPs(vtep *vtepv1.VTEP) error {
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	var errs []error
	for _, node := range nodes {
		vteps, err := util.ParseNodeVTEPs(node)
		if err != nil {
			if util.IsAnnotationNotSetError(err) {
				errs = append(errs, fmt.Errorf("node %s is missing the %s annotation", node.Name, util.OVNNodeVTEPs))
				continue
			}
			errs = append(errs, fmt.Errorf("failed to parse %s annotation on node %s: %w", util.OVNNodeVTEPs, node.Name, err))
			continue
		}
		entry, ok := vteps[vtep.Name]
		if !ok {
			errs = append(errs, fmt.Errorf("node %s has no entry for VTEP %s in %s annotation", node.Name, vtep.Name, util.OVNNodeVTEPs))
			continue
		}
		if len(entry.IPs) == 0 {
			errs = append(errs, fmt.Errorf("node %s has an empty IP list for VTEP %s", node.Name, vtep.Name))
			continue
		}
	}

	return errors.Join(errs...)
}

// handleManagedMode ensures every node has an allocated VTEP IP and the
// annotation is up to date. Each reconcile does two things in order:
//
// NOTE: this iterates all nodes on every VTEP reconcile. At large scale
// (e.g. 5000 nodes × 20 VTEPs) this is 100k in-memory lister lookups +
// annotation parses per event burst, which is acceptable since the lister
// is an in-memory cache and each parse is a small JSON unmarshal. Nodes
// that already have the correct annotation are skipped via an early-exit
// check, so no API calls are made for them. TODO: If scale becomes a concern, a
// per-VTEP "pending nodes" set could reduce redundant work.
//  1. getOrUpdateAllocator: get the existing allocator, create one if this is
//     the first managed reconcile (e.g. Unmanaged->Managed switch), or update
//     it if the CIDRs changed. On first creation, existing node annotations
//     are immediately marked so in-range IPs are preserved within this same
//     reconcile.
//  2. allocateAndAnnotateNode for every node: for nodes whose IPs were
//     successfully marked above, allocateForNode is idempotent and returns the
//     same IP. For nodes with stale/out-of-range IPs (mark was skipped), a
//     fresh IP is allocated and the annotation is overwritten -- all in this
//     same reconcile, not deferred.
//
// Mode switch: Unmanaged -> Managed
//
// On the first managed reconcile after a mode switch, getOrUpdateAllocator
// creates a new allocator and calls markExistingAllocationsFromNodes to
// reserve any in-range IPs already present in node annotations (written by
// the node-side controller during the Unmanaged phase). This preserves IP
// stability — nodes keep the same VTEP IPs they had before the switch.
// Out-of-range IPs (e.g. CIDRs changed before the switch) are skipped and
// a fresh IP is allocated in step 2.
//
// User action required before switching to Managed: the user must remove
// any VTEP IPs from custom dummy/loopback interfaces on the nodes. If
// those IPs remain, they appear in k8s.ovn.org/host-cidrs and
// validateManagedCIDRsAgainstNodeIPs will reject the VTEP with
// CIDROverlap. The managed-mode node controller (evlo-* devices) handles
// IP configuration automatically once the CM accepts the VTEP.
func (c *Controller) handleManagedMode(vtep *vtepv1.VTEP) error {
	allocator, err := c.getOrUpdateAllocator(vtep)
	if err != nil {
		return fmt.Errorf("failed to get/create allocator for VTEP %s: %w", vtep.Name, err)
	}

	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	var errs []error
	for _, node := range nodes {
		if err := c.allocateAndAnnotateNode(vtep.Name, node.Name, allocator); err != nil {
			errs = append(errs, fmt.Errorf("node %s: %w", node.Name, err))
		}
	}
	return errors.Join(errs...)
}

// markExistingAllocationsFromNodes reads node annotations and marks any
// existing IPs for vtepName in the allocator so they are not reassigned.
// IPs that are out of range (e.g. stale after a CIDR change while the CM was
// down) are skipped with a warning. The caller (handleManagedMode) will
// allocate a fresh IP and overwrite the stale annotation for those nodes
// within the same reconcile.
func markExistingAllocationsFromNodes(vtepName string, allocator *vtepIPAllocator, nodes []*corev1.Node) {
	for _, node := range nodes {
		nodeVTEPs, err := util.ParseNodeVTEPs(node)
		if err != nil {
			if !util.IsAnnotationNotSetError(err) {
				klog.Warningf("VTEP %s: failed to parse annotation on node %s, skipping: %v", vtepName, node.Name, err)
			}
			continue
		}
		entry, ok := nodeVTEPs[vtepName]
		if !ok || len(entry.IPs) == 0 {
			continue
		}
		ips := make([]net.IP, 0, len(entry.IPs))
		for _, ipStr := range entry.IPs {
			ip := net.ParseIP(ipStr)
			if ip == nil {
				klog.Warningf("VTEP %s: invalid IP %q in annotation on node %s, skipping", vtepName, ipStr, node.Name)
				continue
			}
			ips = append(ips, ip)
		}
		if err := allocator.markAllocatedForNode(node.Name, ips); err != nil {
			// IP is out of the current CIDR range (e.g. CIDRs changed while
			// CM was down). Warn and skip -- handleManagedMode will allocate
			// a fresh IP and overwrite the stale annotation.
			klog.Warningf("VTEP %s: could not mark IPs %v for node %s (possibly stale after CIDR change): %v", vtepName, ips, node.Name, err)
		}
	}
}

// getOrUpdateAllocator returns the allocator for this VTEP, creating it if
// it doesn't exist (e.g. first reconcile or mode switch from Unmanaged) or
// updating it when CIDRs have changed (append or widen).
//
// On first creation, existing node annotations are marked in the allocator so
// that IPs written by the node-side controller during Unmanaged phase are
// preserved rather than reassigned.
//
// On CIDR change, new CIDRs are appended and widened CIDRs are replaced
// in-place. Since CEL rules guarantee CIDRs can only be appended or widened
// in managed mode, existing allocations always remain valid.
func (c *Controller) getOrUpdateAllocator(vtep *vtepv1.VTEP) (*vtepIPAllocator, error) {
	c.allocatorsMu.Lock()
	defer c.allocatorsMu.Unlock()

	existing, ok := c.allocators[vtep.Name]
	if !ok {
		// First time seeing this VTEP in managed mode (fresh start or
		// Unmanaged->Managed switch). Create allocator and mark any existing
		// node annotations so we don't reassign IPs already in use.
		a, err := newVTEPIPAllocator(vtep.Spec.CIDRs)
		if err != nil {
			return nil, err
		}
		nodes, err := c.nodeLister.List(labels.Everything())
		if err != nil {
			return nil, fmt.Errorf("failed to list nodes: %w", err)
		}
		markExistingAllocationsFromNodes(vtep.Name, a, nodes)
		c.allocators[vtep.Name] = a
		return a, nil
	}

	if existing.cidrsMatch(vtep.Spec.CIDRs) {
		return existing, nil
	}

	// CIDRs have changed. CEL guarantees only appends and widenings are
	// allowed in managed mode, so:
	//  - indices < len(existing.cidrs): check if widened, replace if so
	//  - indices >= len(existing.cidrs): new CIDRs, append them
	klog.Infof("VTEP %s: CIDRs changed, updating allocator", vtep.Name)
	for i, newCIDR := range vtep.Spec.CIDRs {
		if i < len(existing.cidrs) {
			if existing.cidrs[i] == newCIDR {
				continue
			}
			// Existing CIDR was widened.
			if err := existing.replaceRange(i, existing.cidrs[i], newCIDR); err != nil {
				return nil, fmt.Errorf("failed to replace CIDR %q with %q: %w", existing.cidrs[i], newCIDR, err)
			}
			klog.Infof("VTEP %s: widened CIDR[%d] from %s to %s", vtep.Name, i, existing.cidrs[i], newCIDR)
		} else {
			// New CIDR appended to the spec.
			if err := existing.addCIDR(newCIDR); err != nil {
				return nil, fmt.Errorf("failed to add new CIDR %q: %w", newCIDR, err)
			}
			klog.Infof("VTEP %s: appended new CIDR %s", vtep.Name, newCIDR)
		}
	}
	return existing, nil
}

// allocateAndAnnotateNode allocates a VTEP IP for the node and writes the
// k8s.ovn.org/vteps annotation using retry-on-conflict to avoid races with
// the node-side controller that writes entries for unmanaged VTEPs into the
// same annotation.
func (c *Controller) allocateAndAnnotateNode(vtepName, nodeName string, allocator *vtepIPAllocator) error {
	allocated, err := allocator.allocateForNode(nodeName)
	if err != nil {
		return fmt.Errorf("failed to allocate IP: %w", err)
	}

	ips := make([]string, 0, len(allocated))
	for _, ipNet := range allocated {
		ips = append(ips, ipNet.IP.String())
	}

	node, err := c.nodeLister.Get(nodeName)
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}
	// Writing the annotation triggers the node watcher which re-queues all
	// VTEPs. Skip the write if the annotation already matches so we don't
	// produce an infinite reconcile loop.
	vteps, err := util.ParseNodeVTEPs(node)
	if err == nil {
		if existing, ok := vteps[vtepName]; ok && ipsEqual(existing.IPs, ips) {
			return nil
		}
	}

	return util.SetNodeVTEPEntry(nodeName, vtepName, ips, c.nodeLister.Get, c.kube.UpdateNodeStatus)
}

// ipsEqual returns true if two string slices contain the same IPs
// (order-sensitive, matching the allocator's deterministic output).
func ipsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// reconcileCUDN handles CUDN create and delete events relevant to VTEP
// management. On create, it populates the reverse index and re-queues the
// referenced VTEP so validations like IPv6 rejection can fire. On delete,
// it re-queues the VTEP to allow finalizer removal and re-evaluation of
// EVPN-specific constraints (e.g. clearing IPv6NotSupported when no EVPN
// CUDNs reference the VTEP any longer).
func (c *Controller) reconcileCUDN(key string) error {
	cudnName := key
	cudn, err := c.cudnLister.Get(cudnName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.cudnVTEPIndexMu.Lock()
			vtepName, ok := c.cudnVTEPIndex[cudnName]
			if ok {
				delete(c.cudnVTEPIndex, cudnName)
			}
			c.cudnVTEPIndexMu.Unlock()
			if !ok {
				return nil
			}
			// Re-queue the VTEP so reconcileVTEP can re-evaluate whether
			// the finalizer can be removed now that this CUDN is gone,
			// or if the VTEP had Accepted=False due to IPv6 CIDRs
			// referenced by this EVPN CUDN, it can recover.
			klog.V(5).Infof("CUDN %s deleted, re-queuing VTEP %s", cudnName, vtepName)
			c.vtepController.Reconcile(vtepName)
			return nil
		}
		return fmt.Errorf("failed to get CUDN %s: %w", cudnName, err)
	}

	if cudn.Spec.Network.EVPN != nil && cudn.Spec.Network.EVPN.VTEP != "" {
		vtepName := cudn.Spec.Network.EVPN.VTEP
		c.cudnVTEPIndexMu.Lock()
		_, alreadyIndexed := c.cudnVTEPIndex[cudnName]
		c.cudnVTEPIndex[cudnName] = vtepName
		c.cudnVTEPIndexMu.Unlock()
		if !alreadyIndexed {
			// fresh create event
			klog.V(5).Infof("Indexed CUDN %s -> VTEP %s, re-queuing VTEP", cudnName, vtepName)
			c.vtepController.Reconcile(vtepName)
		}
	}
	return nil
}

// cudnNeedsUpdate determines if a CUDN event is relevant for VTEP finalizer
// management. We allow creates of EVPN-enabled CUDNs through so reconcileCUDN
// can populate the reverse index (cudnName → vtepName). The spec is immutable
// so updates never matter. Deletions bypass ObjNeedsUpdate entirely.
func cudnNeedsUpdate(oldObj, newObj *udnv1.ClusterUserDefinedNetwork) bool {
	if oldObj == nil && newObj != nil {
		return newObj.Spec.Network.EVPN != nil
	}
	return false
}

// reconcileNode is called when a node's k8s.ovn.org/vteps annotation changes
// (or on node create/delete). It re-queues all VTEPs so validateNodeVTEPIPs
// can re-validate VTEP IPs for the affected node.
//
// On node delete, the node is absent from the lister. For managed-mode VTEPs
// we release the node's allocation so the IP can be reused by a future node.
//
// NOTE: currently we re-queue all VTEPs because every node participates in
// every VTEP (FRR runs on all nodes). If partial VTEP participation is
// supported in the future, we could compare the node's IPs against each
// VTEP's CIDRs and only re-queue overlapping VTEPs.
func (c *Controller) reconcileNode(nodeName string) error {
	_, err := c.nodeLister.Get(nodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.releaseNodeFromAllocators(nodeName)
		} else {
			klog.Warningf("Failed to get node %s during reconcile: %v", nodeName, err)
		}
	}
	c.vtepController.ReconcileAll()
	return nil
}

// releaseNodeFromAllocators frees the node's IP allocation from all managed
// VTEP allocators so the IP can be reused by a new node.
func (c *Controller) releaseNodeFromAllocators(nodeName string) {
	c.allocatorsMu.Lock()
	defer c.allocatorsMu.Unlock()
	for vtepName, allocator := range c.allocators {
		allocator.releaseNode(nodeName)
		klog.V(4).Infof("Released VTEP %s allocation for deleted node %s", vtepName, nodeName)
	}
}

// nodeNeedsUpdate triggers VTEP reconciliation on node creates and when the
// k8s.ovn.org/vteps annotation changes.
//
// Creates (oldObj==nil) are accepted because managed-mode VTEPs need the
// cluster-manager to allocate IPs for newly joined nodes. Without this, a
// node joining a running cluster would not get a VTEP IP until an unrelated
// event (e.g. a VTEP spec change) happened to trigger a VTEP reconcile.
// For unmanaged mode the extra reconcile from a node create is harmless:
// validateNodeVTEPIPs will report AllocationFailed until the node-side
// controller writes the annotation, which then triggers the annotation-
// change path.
//
// Deletes (newObj==nil) bypass ObjNeedsUpdate in the controller framework,
// so that branch is unreachable here.
func nodeNeedsUpdate(oldObj, newObj *corev1.Node) bool {
	if newObj == nil {
		return false
	}
	if oldObj == nil {
		return true
	}
	return util.NodeVTEPsAnnotationChanged(oldObj, newObj)
}

func vtepNeedsUpdate(oldObj, newObj *vtepv1.VTEP) bool {
	if oldObj == nil || newObj == nil {
		return true
	}
	if !reflect.DeepEqual(oldObj.Spec, newObj.Spec) {
		return true
	}
	// Delete comes as an update event with a non-zero DeletionTimestamp.
	// Always reconcile deleting VTEPs so finalizer removal can be retried.
	if !newObj.DeletionTimestamp.IsZero() {
		return true
	}
	return false
}
