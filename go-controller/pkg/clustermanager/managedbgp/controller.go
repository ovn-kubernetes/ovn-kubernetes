package managedbgp

import (
	"context"
	"fmt"
	"reflect"
	"sort"

	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"
	frrclientset "github.com/metallb/frr-k8s/pkg/client/clientset/versioned"
	frrlisters "github.com/metallb/frr-k8s/pkg/client/listers/api/v1beta1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	ralisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/listers/routeadvertisements/v1"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// ControllerName is the name of the managed BGP controller
	ControllerName = "managed-bgp-controller"
	// FRRConfigManagedLabel is the label used to identify FRRConfigurations managed by this controller
	FRRConfigManagedLabel = "k8s.ovn.org/managed-internal-fabric"
	// FRRConfigManagedValue is the value used for the FRRConfigManagedLabel
	FRRConfigManagedValue = "bgp"
	// BaseFRRConfigName is the name of the base FRRConfiguration
	BaseFRRConfigName = "ovnk-managed-base"
	// DefaultRouteAdvertisementName is the name of the managed RouteAdvertisement for no-overlay managed routing mode
	DefaultRouteAdvertisementName = "ovnk-managed-default-network"
)

// Controller manages the BGP topology for no-overlay networks with managed routing
type Controller struct {
	frrClient      frrclientset.Interface
	frrLister      frrlisters.FRRConfigurationLister
	raClient       raclientset.Interface
	raLister       ralisters.RouteAdvertisementsLister
	nodeController controllerutil.Controller
	raController   controllerutil.Controller
	wf             *factory.WatchFactory
	recorder       record.EventRecorder
}

// NewController creates a new managed BGP controller
func NewController(
	wf *factory.WatchFactory,
	frrClient frrclientset.Interface,
	raClient raclientset.Interface,
	recorder record.EventRecorder,
) *Controller {
	c := &Controller{
		frrClient: frrClient,
		frrLister: wf.FRRConfigurationsInformer().Lister(),
		raClient:  raClient,
		raLister:  wf.RouteAdvertisementsInformer().Lister(),
		wf:        wf,
		recorder:  recorder,
	}

	nodeConfig := &controllerutil.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileNode,
		Threadiness:    1,
		Informer:       wf.NodeCoreInformer().Informer(),
		Lister:         wf.NodeCoreInformer().Lister().List,
		ObjNeedsUpdate: c.nodeNeedsUpdate,
	}
	c.nodeController = controllerutil.NewController(ControllerName, nodeConfig)

	raConfig := &controllerutil.ControllerConfig[ratypes.RouteAdvertisements]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileRA,
		Threadiness:    1,
		Informer:       wf.RouteAdvertisementsInformer().Informer(),
		Lister:         wf.RouteAdvertisementsInformer().Lister().List,
		ObjNeedsUpdate: c.raNeedsUpdate,
	}
	c.raController = controllerutil.NewController(ControllerName+"-ra", raConfig)

	return c
}

// Start starts the managed BGP controller.
// If managed routing mode is not active, it cleans up any previously created
// managed resources and returns without starting the controllers.
func (c *Controller) Start() error {
	klog.Infof("Starting managed BGP controller")

	if config.Default.Transport != types.NetworkTransportNoOverlay || config.NoOverlay.Routing != config.NoOverlayRoutingManaged {
		klog.Infof("Managed routing mode is not active, cleaning up any stale managed resources")
		return c.cleanupManagedResources()
	}

	// Ensure managed RouteAdvertisement exists
	if err := c.ensureManagedRouteAdvertisement(); err != nil {
		return fmt.Errorf("failed to ensure managed RouteAdvertisement: %w", err)
	}

	return controllerutil.Start(c.nodeController, c.raController)
}

// Stop stops the managed BGP controller
func (c *Controller) Stop() {
	klog.Infof("Stopping managed BGP controller")
	controllerutil.Stop(c.nodeController, c.raController)
}

func (c *Controller) nodeNeedsUpdate(oldNode, newNode *corev1.Node) bool {
	if oldNode == nil || newNode == nil {
		return true
	}
	// We care about node IP changes
	oldV4, oldV6 := util.GetNodeInternalAddrs(oldNode)
	newV4, newV6 := util.GetNodeInternalAddrs(newNode)
	return !reflect.DeepEqual(oldV4, newV4) || !reflect.DeepEqual(oldV6, newV6)
}

func (c *Controller) reconcileRA(key string) error {
	// Restore the managed RouteAdvertisement if it was deleted or modified
	if key == DefaultRouteAdvertisementName {
		if err := c.ensureManagedRouteAdvertisement(); err != nil {
			klog.Errorf("Failed to ensure managed RouteAdvertisement: %v", err)
			return err
		}
	}
	return nil
}

func (c *Controller) raNeedsUpdate(oldRA, newRA *ratypes.RouteAdvertisements) bool {
	// Only care about our managed RA
	if newRA != nil && newRA.Name != DefaultRouteAdvertisementName {
		return false
	}
	if oldRA != nil && oldRA.Name != DefaultRouteAdvertisementName {
		return false
	}

	// RA was deleted
	if oldRA != nil && newRA == nil {
		return true
	}

	// RA was added
	if oldRA == nil && newRA != nil {
		return true
	}

	// Spec was modified
	return !reflect.DeepEqual(oldRA.Spec, newRA.Spec)
}

func (c *Controller) reconcileNode(_ string) error {
	if config.ManagedBGP.Topology != config.ManagedBGPTopologyFullMesh {
		return nil
	}

	nodes, err := c.wf.GetNodes()
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	// For full-mesh, we ensure there is a single base FRRConfiguration peering with all nodes.
	// The RouteAdvertisements controller will then generate per-node configs based on this,
	// excluding self-peering.
	if err := c.ensureBaseFRRConfiguration(nodes); err != nil {
		klog.Errorf("Failed to ensure base FRRConfiguration: %v", err)
		return err
	}

	return nil
}

func (c *Controller) ensureBaseFRRConfiguration(allNodes []*corev1.Node) error {
	neighbors := []frrtypes.Neighbor{}
	for _, node := range allNodes {
		v4, v6 := util.GetNodeInternalAddrs(node)
		if v4 != nil {
			neighbors = append(neighbors, frrtypes.Neighbor{
				Address:   v4.String(),
				ASN:       config.ManagedBGP.ASNumber,
				DisableMP: true,
			})
		}
		if v6 != nil {
			neighbors = append(neighbors, frrtypes.Neighbor{
				Address:   v6.String(),
				ASN:       config.ManagedBGP.ASNumber,
				DisableMP: true,
			})
		}
	}

	sort.Slice(neighbors, func(i, j int) bool {
		return neighbors[i].Address < neighbors[j].Address
	})

	frrConfig := &frrtypes.FRRConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BaseFRRConfigName,
			Namespace: config.ManagedBGP.FRRNamespace,
			Labels: map[string]string{
				FRRConfigManagedLabel: FRRConfigManagedValue,
			},
		},
		Spec: frrtypes.FRRConfigurationSpec{
			// Empty NodeSelector means it applies as a base for all nodes by RouteAdvertisements controller
			NodeSelector: metav1.LabelSelector{},
			BGP: frrtypes.BGPConfig{
				Routers: []frrtypes.Router{
					{
						ASN:       config.ManagedBGP.ASNumber,
						Neighbors: neighbors,
					},
				},
			},
		},
	}

	existing, err := c.frrClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.Infof("Creating base FRRConfiguration %s", BaseFRRConfigName)
			_, err = c.frrClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Create(context.TODO(), frrConfig, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return err
			}
			return nil
		}
		return err
	}

	if !reflect.DeepEqual(existing.Spec, frrConfig.Spec) {
		klog.Infof("Updating base FRRConfiguration %s", BaseFRRConfigName)
		updated := existing.DeepCopy()
		updated.Spec = frrConfig.Spec
		_, err = c.frrClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Update(context.TODO(), updated, metav1.UpdateOptions{})
		return err
	}

	return nil
}

// ensureManagedRouteAdvertisement ensures that the managed RouteAdvertisement exists with the correct spec.
// This RA selects the base FRRConfiguration and advertises pod networks for the default network.
func (c *Controller) ensureManagedRouteAdvertisement() error {
	var netSelector apitypes.NetworkSelectors
	if config.Default.Transport == types.NetworkTransportNoOverlay && config.NoOverlay.Routing == config.NoOverlayRoutingManaged {
		netSelector = apitypes.NetworkSelectors{{NetworkSelectionType: apitypes.DefaultNetwork}}
	} else {
		// not in managed mode, nothing to do
		// TODO: Add CUDN managed mode support
		return nil
	}

	ra := &ratypes.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{
			Name: DefaultRouteAdvertisementName,
		},
		Spec: ratypes.RouteAdvertisementsSpec{
			NetworkSelectors: netSelector,
			Advertisements: []ratypes.AdvertisementType{
				ratypes.PodNetwork,
			},
			FRRConfigurationSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					FRRConfigManagedLabel: FRRConfigManagedValue,
				},
			},
			// nodeSelector must select all nodes for PodNetwork
			NodeSelector: metav1.LabelSelector{},
		},
	}

	existing, err := c.raLister.Get(DefaultRouteAdvertisementName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.Infof("Creating managed RouteAdvertisement %s", DefaultRouteAdvertisementName)
			_, err = c.raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return err
			}
			return nil
		}
		return err
	}

	if !reflect.DeepEqual(existing.Spec, ra.Spec) {
		klog.Infof("Updating managed RouteAdvertisement %s", DefaultRouteAdvertisementName)
		updated := existing.DeepCopy()
		updated.Spec = ra.Spec
		_, err = c.raClient.K8sV1().RouteAdvertisements().Update(context.TODO(), updated, metav1.UpdateOptions{})
		return err
	}

	return nil
}

// cleanupManagedResources removes the managed RouteAdvertisement and, if no
// other RouteAdvertisement selects the base FRRConfiguration, removes it too.
func (c *Controller) cleanupManagedResources() error {
	// Delete the managed RouteAdvertisement first
	err := c.raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), DefaultRouteAdvertisementName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete managed RouteAdvertisement %s: %w", DefaultRouteAdvertisementName, err)
	}

	// Only delete the base FRRConfiguration if no remaining RouteAdvertisement selects it.
	// Other CUDNs in managed mode may still reference it.
	baseFRRConfig, err := c.frrLister.FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(BaseFRRConfigName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get base FRRConfiguration %s: %w", BaseFRRConfigName, err)
	}

	allRAs, err := c.raLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list RouteAdvertisements: %w", err)
	}

	for _, ra := range allRAs {
		// Skip the managed RA we just deleted; the informer cache may not
		// have processed the deletion yet.
		if ra.Name == DefaultRouteAdvertisementName {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(&ra.Spec.FRRConfigurationSelector)
		if err != nil {
			continue
		}
		if selector.Matches(labels.Set(baseFRRConfig.Labels)) {
			klog.Infof("Base FRRConfiguration %s is still selected by RouteAdvertisement %s, skipping deletion", BaseFRRConfigName, ra.Name)
			return nil
		}
	}

	klog.Infof("No RouteAdvertisement selects base FRRConfiguration %s, deleting", BaseFRRConfigName)
	err = c.frrClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Delete(context.TODO(), BaseFRRConfigName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete base FRRConfiguration %s: %w", BaseFRRConfigName, err)
	}

	return nil
}
