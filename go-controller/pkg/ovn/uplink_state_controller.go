// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

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
	uplinkNetworkRefSeparator = "\x00"
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

	uplinkStateController controllerutil.Controller
	nadReconciler         networkmanager.NADReconciler
	nadReconcilerID       uint64
	networkRefQueue       controllerutil.Reconciler
	networkRefReconciler  networkmanager.NetworkRefReconciler
	networkRefID          uint64
}

type uplinkNetworkRefReconcilerFunc func(node, networkName string)

func (f uplinkNetworkRefReconcilerFunc) Reconcile(node, networkName string) {
	f(node, networkName)
}

func newUplinkStateController(
	uplinkStateInformer uplinkinformer.UplinkStateInformer,
	nodeInformer coreinformers.NodeInformer,
	networkManager networkmanager.Interface,
	nodeReconciler *nodecontroller.NodeController,
	routeImportManager routeimport.Manager,
) *uplinkStateController {
	c := &uplinkStateController{
		uplinkStateLister:  uplinkStateInformer.Lister(),
		nodeLister:         nodeInformer.Lister(),
		networkManager:     networkManager,
		nodeReconciler:     nodeReconciler,
		routeImportManager: routeImportManager,
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

	nadReconcilerConfig := &controllerutil.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   c.syncNAD,
		Threadiness: 1,
		MaxAttempts: controllerutil.InfiniteAttempts,
	}
	c.nadReconciler = controllerutil.NewReconciler(
		uplinkStateControllerName+"-nad",
		nadReconcilerConfig,
	)

	networkRefConfig := &controllerutil.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   c.syncNetworkRef,
		Threadiness: 1,
		MaxAttempts: controllerutil.InfiniteAttempts,
	}
	c.networkRefQueue = controllerutil.NewReconciler(
		uplinkStateControllerName+"-network-ref",
		networkRefConfig,
	)
	networkRefQueue := c.networkRefQueue
	c.networkRefReconciler = uplinkNetworkRefReconcilerFunc(
		func(node, networkName string) {
			if node == "" || networkName == "" {
				return
			}
			networkRefQueue.Reconcile(uplinkNetworkRefKey(node, networkName))
		},
	)

	return c
}

func (c *uplinkStateController) Start() (err error) {
	klog.Infof("Starting %s", uplinkStateControllerName)
	c.nadReconcilerID = c.networkManager.RegisterNADReconciler(c.nadReconciler)
	c.networkRefID = c.networkManager.RegisterNetworkRefReconciler(c.networkRefReconciler)
	defer func() {
		if err == nil {
			return
		}
		c.deregister()
	}()
	return controllerutil.StartWithInitialSync(
		c.reconcileAllUplinkNetworks,
		c.uplinkStateController,
		c.nadReconciler,
		c.networkRefQueue,
	)
}

func (c *uplinkStateController) Stop() {
	klog.Infof("Stopping %s", uplinkStateControllerName)
	c.deregister()
	controllerutil.Stop(
		c.uplinkStateController,
		c.nadReconciler,
		c.networkRefQueue,
	)
	c.nadReconcilerID = 0
	c.networkRefID = 0
}

func (c *uplinkStateController) deregister() {
	if c.nadReconcilerID != 0 {
		c.networkManager.DeRegisterNADReconciler(c.nadReconcilerID)
		c.nadReconcilerID = 0
	}
	if c.networkRefID != 0 {
		c.networkManager.DeRegisterNetworkRefReconciler(c.networkRefID)
		c.networkRefID = 0
	}
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
	return c.reconcileNetworksForUplink(nodeName, uplinkName)
}

func (c *uplinkStateController) syncNAD(key string) error {
	netInfo := c.networkManager.GetNetInfoForNADKey(key)
	if netInfo != nil {
		return c.reconcileNetworkAcrossNodes(netInfo)
	}
	networkName := c.networkManager.GetNetworkNameForNADKey(key)
	if networkName == "" {
		return c.reconcileAllUplinkNetworks()
	}
	return c.reconcileNetworkByNameAcrossNodes(networkName)
}

func (c *uplinkStateController) syncNetworkRef(key string) error {
	nodeName, networkName, err := parseUplinkNetworkRefKey(key)
	if err != nil {
		klog.Warningf("Skipping invalid Uplink network-ref key %q: %v", key, err)
		return nil
	}
	netInfo := c.networkManager.GetNetwork(networkName)
	if netInfo == nil {
		return nil
	}
	if netInfo.Uplink() == "" {
		return nil
	}
	if !c.networkManager.NodeHasNetwork(nodeName, netInfo.GetNetworkName()) {
		return nil
	}
	c.nodeReconciler.ReconcileNetworkGateway(nodeName, netInfo.GetNetworkName())
	return nil
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
		c.nodeReconciler.ReconcileNetworkGateway(nodeName, networkName)
	}
	return nil
}

func (c *uplinkStateController) reconcileNetworkByNameAcrossNodes(networkName string) error {
	netInfo := c.networkManager.GetNetwork(networkName)
	if netInfo == nil {
		return nil
	}
	return c.reconcileNetworkAcrossNodes(netInfo)
}

func (c *uplinkStateController) reconcileAllUplinkNetworks() error {
	networkNames, err := c.uplinkNetworkNames("")
	if err != nil {
		return err
	}
	var errs []error
	for _, networkName := range networkNames {
		if err := c.reconcileNetworkByNameAcrossNodes(networkName); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *uplinkStateController) reconcileNetworkAcrossNodes(netInfo util.NetInfo) error {
	if netInfo == nil || netInfo.Uplink() == "" {
		return nil
	}
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if !c.networkManager.NodeHasNetwork(node.Name, netInfo.GetNetworkName()) {
			continue
		}
		c.nodeReconciler.ReconcileNetworkGateway(node.Name, netInfo.GetNetworkName())
	}
	return nil
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

func uplinkNetworkRefKey(nodeName, networkName string) string {
	return nodeName + uplinkNetworkRefSeparator + networkName
}

func parseUplinkNetworkRefKey(key string) (string, string, error) {
	nodeName, networkName, found := strings.Cut(key, uplinkNetworkRefSeparator)
	if !found || nodeName == "" || networkName == "" {
		return "", "", fmt.Errorf("invalid network-ref key %q", key)
	}
	return nodeName, networkName, nil
}
