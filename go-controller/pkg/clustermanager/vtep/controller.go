package vtep

import (
	"context"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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
	vtepController controllerutil.Controller
	eventRecorder  record.EventRecorder
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

	return c
}

// Start begins the VTEP controller.
func (c *Controller) Start() error {
	defer klog.Infof("Cluster manager VTEP controller started")
	return controllerutil.StartWithInitialSync(
		nil,
		c.vtepController,
	)
}

// Stop shuts down the VTEP controller.
func (c *Controller) Stop() {
	controllerutil.Stop(c.vtepController)
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
