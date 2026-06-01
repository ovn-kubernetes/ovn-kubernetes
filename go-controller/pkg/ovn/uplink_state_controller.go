// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"errors"
	"fmt"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	nodecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/informers/externalversions/uplink/v1alpha1"
	uplinklister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/routeimport"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	uplinkStateControllerName = "uplink-state-controller"
)

// uplinkStateController bridges typed UplinkState changes back into the
// existing node/network reconciliation path. It does not discover or publish
// UplinkState; ovnkube-node owns that. This controller watches the published
// state and asks the node reconciler to re-run gateway reconciliation for CUDNs
// that use the changed uplink on the affected node.
type uplinkStateController struct {
	uplinkStateLister uplinklister.UplinkStateLister
	nodeLister        corelisters.NodeLister

	networkManager     networkmanager.Interface
	nodeReconciler     *nodecontroller.NodeController
	routeImportManager routeimport.Manager
	zone               string

	uplinkStateController controllerutil.Controller
}

type gatewaySyncHandler interface {
	MarkGatewaySyncNeeded(nodeName string)
}

func newUplinkStateController(
	uplinkStateInformer uplinkinformer.UplinkStateInformer,
	nodeInformer coreinformers.NodeInformer,
	networkManager networkmanager.Interface,
	nodeReconciler *nodecontroller.NodeController,
	routeImportManager routeimport.Manager,
	zone string,
) *uplinkStateController {
	c := &uplinkStateController{
		uplinkStateLister:  uplinkStateInformer.Lister(),
		nodeLister:         nodeInformer.Lister(),
		networkManager:     networkManager,
		nodeReconciler:     nodeReconciler,
		routeImportManager: routeImportManager,
		zone:               zone,
	}

	uplinkStateConfig := &controllerutil.ControllerConfig[uplinkv1alpha1.UplinkState]{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:    uplinkStateInformer.Informer(),
		Lister:      uplinkStateInformer.Lister().List,
		Reconcile:   c.syncUplinkState,
		ObjNeedsUpdate: func(oldObj, newObj *uplinkv1alpha1.UplinkState) bool {
			return oldObj == nil || newObj == nil ||
				!reflect.DeepEqual(oldObj.Status, newObj.Status) ||
				!reflect.DeepEqual(oldObj.Annotations, newObj.Annotations)
		},
		Threadiness: 1,
		MaxAttempts: controllerutil.InfiniteAttempts,
	}
	c.uplinkStateController = controllerutil.NewController(
		uplinkStateControllerName,
		uplinkStateConfig,
	)

	return c
}

func (c *uplinkStateController) Start() (err error) {
	klog.Infof("Starting %s", uplinkStateControllerName)
	return controllerutil.StartWithInitialSync(
		c.reconcileAllUplinkNetworks,
		c.uplinkStateController,
	)
}

func (c *uplinkStateController) Stop() {
	klog.Infof("Stopping %s", uplinkStateControllerName)
	controllerutil.Stop(
		c.uplinkStateController,
	)
}

func (c *uplinkStateController) syncUplinkState(key string) error {
	state, err := c.uplinkStateLister.Get(key)
	if apierrors.IsNotFound(err) {
		return c.reconcileAllUplinkNetworks()
	}
	if err != nil {
		return err
	}
	uplinkName, nodeName := uplinkStateIdentity(state)
	if uplinkName == "" || nodeName == "" {
		return nil
	}
	localNodeName, err := c.localNodeName()
	if err != nil {
		return err
	}
	if nodeName != localNodeName {
		return nil
	}
	return c.reconcileNetworksForUplink(nodeName, uplinkName)
}

func (c *uplinkStateController) reconcileNetworksForUplink(nodeName, uplinkName string) error {
	networkNames, err := c.uplinkNetworkNames(uplinkName)
	if err != nil {
		return err
	}
	for _, networkName := range networkNames {
		if c.routeImportManager != nil {
			if err := c.routeImportManager.ReconcileNetwork(networkName); err != nil {
				return err
			}
		}
		if !c.networkManager.NodeHasNetwork(nodeName, networkName) {
			continue
		}
		c.requestNetworkGatewaySync(nodeName, networkName)
	}
	return nil
}

func (c *uplinkStateController) reconcileAllUplinkNetworks() error {
	networkNames, err := c.uplinkNetworkNames("")
	if err != nil {
		return err
	}
	var errs []error
	for _, networkName := range networkNames {
		if err := c.reconcileNetworkByNameForLocalNode(networkName); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *uplinkStateController) reconcileNetworkByNameForLocalNode(networkName string) error {
	netInfo := c.networkManager.GetNetwork(networkName)
	if netInfo == nil {
		return nil
	}
	return c.reconcileNetworkForLocalNode(netInfo)
}

func (c *uplinkStateController) reconcileNetworkForLocalNode(netInfo util.NetInfo) error {
	if netInfo == nil || netInfo.Uplink() == "" {
		return nil
	}
	nodeName, err := c.localNodeName()
	if err != nil {
		return err
	}
	if !c.networkManager.NodeHasNetwork(nodeName, netInfo.GetNetworkName()) {
		return nil
	}
	c.requestNetworkGatewaySync(nodeName, netInfo.GetNetworkName())
	return nil
}

func (c *uplinkStateController) localNodeName() (string, error) {
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return "", err
	}
	for _, node := range nodes {
		if util.GetNodeZone(node) == c.zone {
			return node.Name, nil
		}
	}
	return "", fmt.Errorf("failed to find node for zone %q", c.zone)
}

func (c *uplinkStateController) requestNetworkGatewaySync(nodeName, networkName string) {
	c.nodeReconciler.ReconcileNetworkWithPreflight(nodeName, networkName, func(handler nodecontroller.NodeHandler) {
		if gatewayHandler, ok := handler.(gatewaySyncHandler); ok {
			gatewayHandler.MarkGatewaySyncNeeded(nodeName)
		}
	})
}

func (c *uplinkStateController) uplinkNetworkNames(uplinkName string) ([]string, error) {
	networkNames := sets.New[string]()
	err := c.networkManager.DoWithLock(func(netInfo util.NetInfo) error {
		if netInfo == nil || netInfo.Uplink() == "" {
			return nil
		}
		if uplinkName != "" && netInfo.Uplink() != uplinkName {
			return nil
		}
		networkNames.Insert(netInfo.GetNetworkName())
		return nil
	})
	if err != nil {
		return nil, err
	}
	return networkNames.UnsortedList(), nil
}

func uplinkStateIdentity(state *uplinkv1alpha1.UplinkState) (uplinkName, nodeName string) {
	return uplinkutil.StateIdentity(state)
}
