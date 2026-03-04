package vtep

import (
	"reflect"

	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned"
	vteplisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/listers/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// Controller manages VTEP resources in the cluster manager.
type Controller struct {
	vtepClient     vtepclientset.Interface
	vtepLister     vteplisters.VTEPLister
	vtepController controllerutil.Controller
}

// NewController creates a new VTEP controller.
func NewController(
	wf *factory.WatchFactory,
	ovnClient *util.OVNClusterManagerClientset,
) *Controller {
	vtepLister := wf.VTEPInformer().Lister()
	c := &Controller{
		vtepClient: ovnClient.VTEPClient,
		vtepLister: vtepLister,
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
	return nil
}

func vtepNeedsUpdate(oldObj, newObj *vtepv1.VTEP) bool {
	if oldObj == nil || newObj == nil {
		return true
	}
	return !reflect.DeepEqual(oldObj.Spec, newObj.Spec)
}
