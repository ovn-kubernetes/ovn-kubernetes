package notifier

import (
	"errors"

	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	vtepv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/informers/externalversions/vtep/v1"
)

// VTEPReconciler is the interface for controllers that need to react to VTEP events.
type VTEPReconciler interface {
	ReconcileVTEP(key string) error
}

// VTEPNotifier watches VTEP objects and notifies subscribers upon change.
// It enqueues the reconciled object keys in the subscribing controllers workqueue.
type VTEPNotifier struct {
	Controller controller.Controller

	subscribers []VTEPReconciler
}

// NewVTEPNotifier creates a new VTEPNotifier that watches VTEP CRs and notifies subscribers.
func NewVTEPNotifier(vtepInformer vtepinformer.VTEPInformer, subscribers ...VTEPReconciler) *VTEPNotifier {
	c := &VTEPNotifier{
		subscribers: subscribers,
	}

	vtepLister := vtepInformer.Lister()
	cfg := &controller.ControllerConfig[vtepv1.VTEP]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcile,
		ObjNeedsUpdate: c.needUpdate,
		Threadiness:    1,
		Informer:       vtepInformer.Informer(),
		Lister:         vtepLister.List,
	}
	c.Controller = controller.NewController("udn-vtep-controller", cfg)

	return c
}

// needUpdate returns true when the VTEP has been created or deleted.
// We notify on create/delete so that CUDNs referencing this VTEP can be re-queued.
// IMPORTANT: Before adding update notifications, verify that all subscribers
// can handle increased event frequency.
func (c *VTEPNotifier) needUpdate(old, new *vtepv1.VTEP) bool {
	vtepCreated := old == nil && new != nil
	vtepDeleted := old != nil && new == nil
	return vtepCreated || vtepDeleted
}

// reconcile notifies subscribers with the VTEP key following VTEP events.
func (c *VTEPNotifier) reconcile(key string) error {
	var errs []error
	for _, subscriber := range c.subscribers {
		if subscriber != nil {
			// enqueue the reconciled VTEP key in the subscribers workqueue to
			// enable the subscriber to act on VTEP changes
			if err := subscriber.ReconcileVTEP(key); err != nil {
				errs = append(errs, err)
			}
		}
	}

	return errors.Join(errs...)
}
