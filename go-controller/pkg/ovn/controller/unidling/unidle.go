package unidling

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	libovsdbcache "github.com/ovn-kubernetes/libovsdb/cache"
	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// unidlingController checks periodically the OVN events db
// and generates a Kubernetes NeedPods events with the Service
// associated to the VIP
type unidlingController struct {
	eventQueue    chan sbdb.ControllerEvent
	eventRecorder record.EventRecorder
	// Map of load balancers to service namespace
	serviceVIPToName     map[ServiceVIPKey]types.NamespacedName
	serviceVIPToNameLock sync.Mutex
	sbClient             libovsdbclient.Client
}

// NewController creates a new unidling controller
func NewController(recorder record.EventRecorder, serviceInformer cache.SharedIndexInformer, sbClient libovsdbclient.Client) (*unidlingController, error) {
	uc := &unidlingController{
		eventQueue:       make(chan sbdb.ControllerEvent),
		eventRecorder:    recorder,
		serviceVIPToName: map[ServiceVIPKey]types.NamespacedName{},
		sbClient:         sbClient,
	}

	klog.Info("Registering OVN SB ControllerEvent handler")
	// add all empty lb backend events to a channel
	sbClient.Cache().AddEventHandler(
		&libovsdbcache.EventHandlerFuncs{
			AddFunc: func(_ string, m model.Model) {
				if event, ok := m.(*sbdb.ControllerEvent); ok {
					if event.EventType == sbdb.ControllerEventEventTypeEmptyLbBackends {
						uc.eventQueue <- *event
					}
				}
			},
		},
	)

	// FIXME: libovsdb event handlers should be added before the Monitor is set
	// manually populate the current events. we may get an event twice, but this
	// shouldn't cause issues as we'll just log an error message
	klog.Info("Populating Initial ContollerEvent events")

	var controllerEvents []sbdb.ControllerEvent
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	err := sbClient.List(ctx, &controllerEvents)
	if err != nil {
		return nil, err
	}
	go func() {
		for _, event := range controllerEvents {
			uc.eventQueue <- event
		}
	}()

	// we only process events on unidling, there is no reconcilation
	klog.Info("Setting up event handlers for services")
	_, err = serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: uc.onServiceAdd,
		UpdateFunc: func(old, new interface{}) {
			uc.onServiceDelete(old)
			uc.onServiceAdd(new)
		},
		DeleteFunc: uc.onServiceDelete,
	})
	if err != nil {
		return nil, err
	}
	return uc, nil
}

func (uc *unidlingController) onServiceAdd(obj interface{}) {
	svc := obj.(*corev1.Service)
	if util.ServiceTypeHasClusterIP(svc) && util.IsClusterIPSet(svc) {
		for _, ip := range util.GetClusterIPs(svc) {
			for _, svcPort := range svc.Spec.Ports {
				vip := util.JoinHostPortInt32(ip, svcPort.Port)
				uc.AddServiceVIPToName(vip, svcPort.Protocol, svc.Namespace, svc.Name)
			}
		}
	}
}

func (uc *unidlingController) onServiceDelete(obj interface{}) {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		svc, ok = tombstone.Obj.(*corev1.Service)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Service: %#v", obj))
			return
		}
	}

	if util.ServiceTypeHasClusterIP(svc) && util.IsClusterIPSet(svc) {
		for _, ip := range util.GetClusterIPs(svc) {
			for _, svcPort := range svc.Spec.Ports {
				vip := util.JoinHostPortInt32(ip, svcPort.Port)
				uc.DeleteServiceVIPToName(vip, svcPort.Protocol)
			}
		}
	}
}

// ServiceVIPKey is used for looking up service namespace information for a
// particular load balancer
type ServiceVIPKey struct {
	// Load balancer VIP in the form "ip:port"
	vip string
	// Protocol used by the load balancer
	protocol corev1.Protocol
}

// AddServiceVIPToName associates a k8s service name with a load balancer VIP
func (uc *unidlingController) AddServiceVIPToName(vip string, protocol corev1.Protocol, namespace, name string) {
	uc.serviceVIPToNameLock.Lock()
	defer uc.serviceVIPToNameLock.Unlock()
	uc.serviceVIPToName[ServiceVIPKey{vip, protocol}] = types.NamespacedName{Namespace: namespace, Name: name}
}

// GetServiceVIPToName retrieves the associated k8s service name for a load balancer VIP
func (uc *unidlingController) GetServiceVIPToName(vip string, protocol corev1.Protocol) (types.NamespacedName, bool) {
	uc.serviceVIPToNameLock.Lock()
	defer uc.serviceVIPToNameLock.Unlock()
	namespace, ok := uc.serviceVIPToName[ServiceVIPKey{vip, protocol}]
	return namespace, ok
}

// DeleteServiceVIPToName retrieves the associated k8s service name for a load balancer VIP
func (uc *unidlingController) DeleteServiceVIPToName(vip string, protocol corev1.Protocol) {
	uc.serviceVIPToNameLock.Lock()
	defer uc.serviceVIPToNameLock.Unlock()
	delete(uc.serviceVIPToName, ServiceVIPKey{vip, protocol})
}

func (uc *unidlingController) Run(stopCh <-chan struct{}) {
	for {
		select {
		case event := <-uc.eventQueue:
			if err := uc.handleLbEmptyBackendsEvent(event); err != nil {
				klog.Error(err)
			}
		case <-stopCh:
			return
		}
	}
}

func (uc *unidlingController) handleLbEmptyBackendsEvent(event sbdb.ControllerEvent) error {
	op, err := uc.sbClient.Where(
		&event,
	).Delete()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	result, err := uc.sbClient.Transact(ctx, op...)
	if err != nil {
		return err
	}
	_, err = ovsdb.CheckOperationResults(result, op)
	if err != nil {
		return err
	}

	vip, ok := event.EventInfo["vip"]
	if !ok {
		return err
	}
	proto := event.EventInfo["protocol"]
	var protocol corev1.Protocol
	if proto == "udp" {
		protocol = corev1.ProtocolUDP
	} else if proto == "sctp" {
		protocol = corev1.ProtocolSCTP
	} else {
		protocol = corev1.ProtocolTCP
	}

	serviceName, ok := uc.GetServiceVIPToName(vip, protocol)

	if !ok {
		return fmt.Errorf("can't find service for vip %s:%s", protocol, vip)
	}

	serviceRef := corev1.ObjectReference{
		Kind:      "Service",
		Namespace: serviceName.Namespace,
		Name:      serviceName.Name,
	}
	klog.V(5).Infof("Sending a NeedPods event for service %s in namespace %s.", serviceName.Name, serviceName.Namespace)
	uc.eventRecorder.Eventf(&serviceRef, corev1.EventTypeNormal, "NeedPods", "The service %s needs pods", serviceName.Name)

	return nil
}
