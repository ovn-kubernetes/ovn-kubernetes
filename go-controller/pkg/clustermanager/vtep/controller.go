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
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/reference"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	k8scontrollerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnlisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned"
	vtepscheme "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned/scheme"
	vteplisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/listers/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
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
	vtepController controllerutil.Controller
	cudnController controllerutil.Controller
	nodeController controllerutil.Controller
	eventRecorder  record.EventRecorder

	// cudnVTEPIndex tracks which VTEP each EVPN-enabled CUDN references
	// (cudnName → vtepName). Populated on CUDN create, consulted on CUDN
	// delete so we can re-queue only the specific VTEP instead of scanning
	// all VTEPs for every single CUDN deletion (since onDelete simply requeues
	// we don't even get to evaluate whether it was an EVPN-enabled CUDN or not).
	// since the controller is single-threaded, we can use a sync.Map without
	// worrying about concurrent map writes.
	cudnVTEPIndex sync.Map
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
		eventRecorder: recorder,
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

// Start begins the VTEP controller.
func (c *Controller) Start() error {
	defer klog.Infof("Cluster manager VTEP controller started")
	return controllerutil.StartWithInitialSync(
		nil,
		c.vtepController,
		c.cudnController,
		c.nodeController,
	)
}

// Stop shuts down the VTEP controller.
func (c *Controller) Stop() {
	controllerutil.Stop(c.vtepController, c.cudnController, c.nodeController)
}

func (c *Controller) reconcileVTEP(key string) error {
	startTime := time.Now()
	_, vtepName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("failed to split VTEP key %s: %w", key, err)
	}
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

	// We must work on a copy since we may mutate finalizers.
	vtep = vtep.DeepCopy()

	if !vtep.DeletionTimestamp.IsZero() {
		return c.handleVTEPDeletion(vtep)
	}

	if err := c.ensureFinalizer(vtep); err != nil {
		return err
	}

	if vtep.Spec.Mode == vtepv1.VTEPModeManaged {
		return c.handleManagedModeNotSupported(vtep)
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

	if err := c.reconcileNodeAllocations(vtep); err != nil {
		vtepRef, refErr := reference.GetReference(vtepscheme.Scheme, vtep)
		if refErr != nil {
			return fmt.Errorf("failed to get object reference for VTEP %s: %w", vtep.Name, refErr)
		}
		c.eventRecorder.Event(vtepRef, corev1.EventTypeWarning, reasonAllocationFailed, err.Error())
		statusErr := c.updateStatusCondition(vtep, conditionTypeAccepted, metav1.ConditionFalse,
			reasonAllocationFailed, err.Error())
		return errors.Join(statusErr, fmt.Errorf("failed to allocate VTEP IPs for VTEP %s: %w", vtep.Name, err))
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
	if k8scontrollerutil.AddFinalizer(vtep, finalizerVTEP) {
		_, err := c.vtepClient.K8sV1().VTEPs().Update(context.Background(), vtep, metav1.UpdateOptions{})
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
		// we don't retry here since when a CUDN is deleted, this controller will be notified and will re-queue the VTEP
		klog.Infof("VTEP %s is still referenced by CUDNs %v, blocking deletion", vtep.Name, referencingCUDNs)
		return nil
	}

	k8scontrollerutil.RemoveFinalizer(vtep, finalizerVTEP)
	_, err = c.vtepClient.K8sV1().VTEPs().Update(context.Background(), vtep, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to remove finalizer from VTEP %s: %w", vtep.Name, err)
	}
	klog.Infof("Removed finalizer from VTEP %s, deletion unblocked", vtep.Name)
	return nil
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

// validateCIDRsAcrossVTEPs checks that none of this VTEP's CIDRs overlap with
// any other VTEP's CIDRs. All overlapping VTEPs are re-queued so both sides
// of a conflict discover it and set Accepted=False.
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
			klog.Errorf("Failed to parse CIDRs for VTEP %s, skipping overlap check: %v", other.Name, err)
			continue
		}
		if util.NetworksOverlap(currentCIDRs, otherCIDRs) {
			conflicting = append(conflicting, other.Name)
			otherCond := meta.FindStatusCondition(other.Status.Conditions, conditionTypeAccepted)
			if otherCond == nil || otherCond.Status != metav1.ConditionFalse || otherCond.Reason != reasonCIDROverlap ||
				!vtepNameInMessage(otherCond.Message, vtep.Name) {
				c.vtepController.Reconcile(other.Name)
			}
		}
	}
	if len(conflicting) > 0 {
		sort.Strings(conflicting)
		return fmt.Errorf("CIDRs overlap with VTEPs: [%s]",
			strings.Join(conflicting, ", "))
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

// discoverNodeVTEPIPs finds IPs on the node that fall within the VTEP's CIDRs.
// For unmanaged VTEPs, an external provider has already assigned IPs to the node;
// this discovers them from the host-cidrs annotation.
// Returns at most one IPv4 and one IPv6 address. If multiple IPs of the same
// family match, an error is returned (ambiguous assignment).
func discoverNodeVTEPIPs(vtepCIDRs []*net.IPNet, node *corev1.Node) (net.IP, net.IP, error) {
	hostCIDRs, err := util.ParseNodeHostCIDRs(node)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse host-cidrs on node %s: %w", node.Name, err)
	}

	var v4Matches, v6Matches []net.IP
	for hostCIDR := range hostCIDRs {
		ip, _, err := net.ParseCIDR(hostCIDR)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid host CIDR %q on node %s: %w", hostCIDR, node.Name, err)
		}
		for _, cidr := range vtepCIDRs {
			if cidr.Contains(ip) {
				if ip.To4() != nil {
					v4Matches = append(v4Matches, ip)
				} else {
					v6Matches = append(v6Matches, ip)
				}
				break
			}
		}
	}

	if len(v4Matches) > 1 {
		return nil, nil, fmt.Errorf("ambiguous VTEP IPv4 on node %s: found %v", node.Name, v4Matches)
	}
	if len(v6Matches) > 1 {
		return nil, nil, fmt.Errorf("ambiguous VTEP IPv6 on node %s: found %v", node.Name, v6Matches)
	}

	var v4, v6 net.IP
	if len(v4Matches) == 1 {
		v4 = v4Matches[0]
	}
	if len(v6Matches) == 1 {
		v6 = v6Matches[0]
	}
	return v4, v6, nil
}

// reconcileNodeAllocations discovers VTEP IPs from all nodes and updates
// VTEPStatus.NodeAllocations via SSA. Returns an aggregated error listing
// nodes where discovery failed, so the controller retries.
func (c *Controller) reconcileNodeAllocations(vtep *vtepv1.VTEP) error {
	vtepCIDRs, err := parseVTEPCIDRs(vtep)
	if err != nil {
		return fmt.Errorf("failed to parse CIDRs for VTEP %s: %w", vtep.Name, err)
	}

	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	desired := make(map[string]vtepv1.DualStackIPs, len(nodes))
	var errs []error
	for _, node := range nodes {
		v4, v6, err := discoverNodeVTEPIPs(vtepCIDRs, node)
		if err != nil {
			// Node without host-cidrs is not yet OVN-managed; skip it.
			// A future node update event will re-queue the VTEP when the
			// annotation appears.
			if util.IsAnnotationNotSetError(err) {
				continue
			}
			klog.Warningf("VTEP %s: skipping node %s due to discovery error: %v", vtep.Name, node.Name, err)
			errs = append(errs, err)
			continue
		}
		if v4 == nil && v6 == nil {
			klog.Warningf("VTEP %s: node %s has no IP matching any VTEP CIDR", vtep.Name, node.Name)
			errs = append(errs, fmt.Errorf("node %s has no IP matching any VTEP %s CIDR", node.Name, vtep.Name))
			continue
		}
		ips := vtepv1.DualStackIPs{}
		if v4 != nil {
			ips = append(ips, vtepv1.IP(v4.String()))
		}
		if v6 != nil {
			ips = append(ips, vtepv1.IP(v6.String()))
		}
		desired[node.Name] = ips
	}

	updateErr := c.updateNodeAllocations(vtep, desired)

	if len(errs) > 0 {
		return errors.Join(updateErr, fmt.Errorf("VTEP %s: node allocation errors: %v", vtep.Name, errs))
	}
	return updateErr
}

func (c *Controller) handleManagedModeNotSupported(vtep *vtepv1.VTEP) error {
	vtepRef, err := reference.GetReference(vtepscheme.Scheme, vtep)
	if err != nil {
		return fmt.Errorf("failed to get object reference for VTEP %s: %w", vtep.Name, err)
	}
	c.eventRecorder.Event(vtepRef, corev1.EventTypeWarning, reasonManagedModeNotSupported,
		"Managed VTEP mode is not yet implemented; only Unmanaged mode is currently supported")

	return c.updateStatusCondition(vtep, conditionTypeAccepted, metav1.ConditionFalse,
		reasonManagedModeNotSupported,
		"Managed VTEP mode is not yet implemented; only Unmanaged mode is currently supported")
}

// reconcileCUDN handles CUDN create and delete events relevant to VTEP
// finalizer management. On create, it populates the reverse index so the
// VTEP name is available when the CUDN is later deleted. On delete, it
// uses the index to re-queue only the specific referenced VTEP.
func (c *Controller) reconcileCUDN(key string) error {
	_, cudnName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("failed to split CUDN key %s: %w", key, err)
	}

	cudn, err := c.cudnLister.Get(cudnName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			vtepName, ok := c.cudnVTEPIndex.LoadAndDelete(cudnName)
			if !ok {
				return nil
			}
			vtep, vtepErr := c.vtepLister.Get(vtepName.(string))
			// only re-queue if the VTEP is being deleted
			if vtepErr != nil || vtep.DeletionTimestamp.IsZero() {
				return nil
			}
			klog.V(5).Infof("CUDN %s deleted, re-queuing deleting VTEP %s", cudnName, vtepName)
			c.vtepController.Reconcile(vtepName.(string))
			return nil
		}
		return fmt.Errorf("failed to get CUDN %s: %w", cudnName, err)
	}

	if cudn.Spec.Network.EVPN != nil && cudn.Spec.Network.EVPN.VTEP != "" {
		c.cudnVTEPIndex.Store(cudnName, cudn.Spec.Network.EVPN.VTEP)
		klog.V(5).Infof("Indexed CUDN %s -> VTEP %s", cudnName, cudn.Spec.Network.EVPN.VTEP)
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

// reconcileNode is called when a node's host-cidrs annotation changes (or on
// node create/delete). It re-queues all VTEPs so reconcileNodeAllocations can
// rediscover VTEP IPs for the affected node.
//
// NOTE: currently we re-queue all VTEPs because every node participates in
// every VTEP (FRR runs on all nodes). If partial VTEP participation is
// supported in the future, we could compare the node's IPs against each
// VTEP's CIDRs and only re-queue overlapping VTEPs.
func (c *Controller) reconcileNode(_ string) error {
	c.vtepController.ReconcileAll()
	return nil
}

// nodeNeedsUpdate triggers VTEP reconciliation only when the host-cidrs
// annotation changes. This avoids unnecessary re-queues from node heartbeats,
// status updates, label changes, etc.
func nodeNeedsUpdate(oldObj, newObj *corev1.Node) bool {
	if oldObj == nil || newObj == nil {
		return true
	}
	return util.NodeHostCIDRsAnnotationChanged(oldObj, newObj)
}

func vtepNeedsUpdate(oldObj, newObj *vtepv1.VTEP) bool {
	if oldObj == nil || newObj == nil {
		return true
	}
	if !reflect.DeepEqual(oldObj.Spec, newObj.Spec) {
		return true
	}
	// delete comes as an update event with a non-zero DeletionTimestamp
	// so we need to check whether the finalizers can be removed or not
	if oldObj.DeletionTimestamp.IsZero() != newObj.DeletionTimestamp.IsZero() {
		return true
	}
	return false
}
