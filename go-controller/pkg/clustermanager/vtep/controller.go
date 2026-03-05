package vtep

import (
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/reference"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned"
	vtepscheme "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned/scheme"
	vteplisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/listers/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// Controller manages VTEP resources in the cluster manager.
type Controller struct {
	vtepClient     vtepclientset.Interface
	vtepLister     vteplisters.VTEPLister
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
			// VTEP was deleted; nothing to do yet (finalizer logic will be added later)
			return nil
		}
		return fmt.Errorf("failed to get VTEP %s: %w", vtepName, err)
	}

	if vtep.Spec.Mode == vtepv1.VTEPModeManaged {
		return c.handleManagedModeNotSupported(vtep)
	}

	return c.updateStatusCondition(vtep, conditionTypeAccepted, metav1.ConditionTrue,
		reasonAllocated, "VTEP allocation succeeded")
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
	return !reflect.DeepEqual(oldObj.Spec, newObj.Spec)
}
