// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package unidling

import (
	"context"
	"fmt"
	"sync"
	"time"

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

const controllerEventResyncInterval = 30 * time.Second

// unidlingController checks periodically the OVN events db
// and generates a Kubernetes NeedPods events with the Service
// associated to the VIP
type unidlingController struct {
	wakeupCh      chan struct{}
	eventRecorder record.EventRecorder
	// Map of load balancers to service namespace
	serviceVIPToName     map[ServiceVIPKey]types.NamespacedName
	serviceVIPToNameLock sync.Mutex
	sbClient             libovsdbclient.Client
}

// NewController creates a new unidling controller
func NewController(recorder record.EventRecorder, serviceInformer cache.SharedIndexInformer, sbClient libovsdbclient.Client) (*unidlingController, error) {
	uc := &unidlingController{
		wakeupCh:         make(chan struct{}, 1),
		eventRecorder:    recorder,
		serviceVIPToName: map[ServiceVIPKey]types.NamespacedName{},
		sbClient:         sbClient,
	}

	klog.Info("Registering OVN SB ControllerEvent handler")
	sbCache := sbClient.Cache()
	if sbCache == nil {
		// the client cache is only available once the client is connected
		return nil, fmt.Errorf("SB DB client is not connected, no cache to register the ControllerEvent handler on")
	}
	// Keep the rows in SB as the backlog. The shared cache event processor must
	// never wait for this controller to finish a transaction or start running.
	sbCache.AddEventHandler(
		&libovsdbcache.EventHandlerFuncs{
			AddFunc: func(_ string, m model.Model) {
				if event, ok := m.(*sbdb.ControllerEvent); ok {
					if event.EventType == sbdb.ControllerEventEventTypeEmptyLbBackends {
						uc.requestSync()
					}
				}
			},
		},
	)

	klog.Info("Setting up event handlers for services")
	_, err := serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
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

// requestSync requests a scan without waiting for the worker. Pending signals
// are coalesced because the rows remain in SB. The channel stays open so cache
// callbacks remain safe after Run returns.
func (uc *unidlingController) requestSync() {
	select {
	case uc.wakeupCh <- struct{}{}:
	default:
	}
}

// Run processes pending SB events at startup, on cache notifications, and
// periodically until stopCh is closed.
func (uc *unidlingController) Run(stopCh <-chan struct{}) {
	// Failed deletions and dropped libovsdb notifications may leave pending
	// rows without another wakeup.
	ticker := time.NewTicker(controllerEventResyncInterval)
	defer ticker.Stop()

	// Include rows that existed before the cache handler was registered.
	uc.requestSync()
	for {
		select {
		case <-stopCh:
			return
		case <-uc.wakeupCh:
		case <-ticker.C:
		}
		uc.syncControllerEvents(stopCh)
	}
}

// syncControllerEvents processes a snapshot of the monitored Controller_Event
// cache and leaves failed deletions in SB for a later scan. It checks stopCh
// between events so shutdown does not drain the remaining backlog.
func (uc *unidlingController) syncControllerEvents(stopCh <-chan struct{}) {
	var controllerEvents []sbdb.ControllerEvent
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	err := uc.sbClient.List(ctx, &controllerEvents)
	cancel()
	if err != nil {
		klog.Errorf("Failed to list unidling controller events: %v", err)
		return
	}
	for _, event := range controllerEvents {
		select {
		case <-stopCh:
			return
		default:
		}
		if event.EventType == sbdb.ControllerEventEventTypeEmptyLbBackends {
			if err := uc.handleLbEmptyBackendsEvent(event); err != nil {
				klog.Errorf("Failed to process unidling controller event %s: %v", event.UUID, err)
			}
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
		return fmt.Errorf("vip not found in controller event %s with event_info %v", event.UUID, event.EventInfo)
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
