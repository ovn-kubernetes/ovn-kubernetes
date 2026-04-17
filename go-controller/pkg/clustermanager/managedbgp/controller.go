package managedbgp

import (
	"context"
	"fmt"
	"hash/fnv"
	"reflect"
	"sort"

	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"
	frrclientset "github.com/metallb/frr-k8s/pkg/client/clientset/versioned"
	frrlisters "github.com/metallb/frr-k8s/pkg/client/listers/api/v1beta1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	ralisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/listers/routeadvertisements/v1"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	cudnclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	cudnlisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// controllerName is the name of the managed BGP controller
	controllerName = "managed-bgp-controller"
	// fieldManager identifies this controller as the server-side author of owned resources.
	// Used with FieldManager in Create/Update options so that isOwnUpdate can
	// filter out informer events that result from our own writes.
	fieldManager = "clustermanager-managed-bgp-controller"
	// frrConfigManagedLabel is the label used to identify FRRConfigurations managed by this controller
	frrConfigManagedLabel = "k8s.ovn.org/managed-internal-fabric"
	// frrConfigManagedValue is the value used for the frrConfigManagedLabel
	frrConfigManagedValue = "bgp"
	// managedRANetworkLabel is the label used to identify managed resources by network name.
	// It is set on managed RouteAdvertisements and on CUDNs (to enable RA network selection).
	managedRANetworkLabel = "k8s.ovn.org/managed-network"
	// managedNamePrefix is the prefix for managed resource names
	managedNamePrefix = "ovnk-managedbgp-"
	// managedFRRConfigurationName is the static name for the managed base FRRConfiguration.
	managedFRRConfigurationName = "ovnk-managedbgp-nooverlay"
)

func managedHashedName(s string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%s%x", managedNamePrefix, h.Sum64())
}

// managedRouteAdvertisementName returns the name of the managed RouteAdvertisement
// for the given network name. The name follows the pattern "ovnk-managedbgp-<hash>"
// where <hash> is derived from the network name.
func managedRouteAdvertisementName(networkName string) string {
	return managedHashedName(networkName)
}

// Controller manages the BGP topology for no-overlay networks with managed routing
type Controller struct {
	frrClient      frrclientset.Interface
	frrLister      frrlisters.FRRConfigurationLister
	raClient       raclientset.Interface
	raLister       ralisters.RouteAdvertisementsLister
	cudnClient     cudnclientset.Interface
	cudnLister     cudnlisters.ClusterUserDefinedNetworkLister
	nodeController controllerutil.Controller
	raController   controllerutil.Controller
	frrController  controllerutil.Controller
	cudnController controllerutil.Controller
	wf             *factory.WatchFactory
}

// NewController creates a new managed BGP controller
func NewController(
	wf *factory.WatchFactory,
	frrClient frrclientset.Interface,
	raClient raclientset.Interface,
	cudnClient cudnclientset.Interface,
) *Controller {
	c := &Controller{
		frrClient:  frrClient,
		frrLister:  wf.FRRConfigurationsInformer().Lister(),
		raClient:   raClient,
		raLister:   wf.RouteAdvertisementsInformer().Lister(),
		cudnClient: cudnClient,
		wf:         wf,
	}

	nodeConfig := &controllerutil.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileNode,
		Threadiness:    1,
		Informer:       wf.NodeCoreInformer().Informer(),
		Lister:         wf.NodeCoreInformer().Lister().List,
		ObjNeedsUpdate: c.nodeNeedsUpdate,
	}
	c.nodeController = controllerutil.NewController(controllerName, nodeConfig)

	raConfig := &controllerutil.ControllerConfig[ratypes.RouteAdvertisements]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileRA,
		Threadiness:    1,
		Informer:       wf.RouteAdvertisementsInformer().Informer(),
		Lister:         wf.RouteAdvertisementsInformer().Lister().List,
		ObjNeedsUpdate: c.raNeedsUpdate,
	}
	c.raController = controllerutil.NewController(controllerName+"-ra", raConfig)

	frrConfig := &controllerutil.ControllerConfig[frrtypes.FRRConfiguration]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileFRRConfiguration,
		Threadiness:    1,
		Informer:       wf.FRRConfigurationsInformer().Informer(),
		Lister:         wf.FRRConfigurationsInformer().Lister().List,
		ObjNeedsUpdate: c.frrNeedsUpdate,
	}
	c.frrController = controllerutil.NewController(controllerName+"-frr", frrConfig)

	if util.IsNetworkSegmentationSupportEnabled() {
		c.cudnLister = wf.ClusterUserDefinedNetworkInformer().Lister()
		cudnConfig := &controllerutil.ControllerConfig[userdefinednetworkv1.ClusterUserDefinedNetwork]{
			RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:      c.reconcileNetwork,
			Threadiness:    1,
			Informer:       wf.ClusterUserDefinedNetworkInformer().Informer(),
			Lister:         wf.ClusterUserDefinedNetworkInformer().Lister().List,
			ObjNeedsUpdate: c.cudnNeedsUpdate,
		}
		c.cudnController = controllerutil.NewController(controllerName+"-cudn", cudnConfig)
	}

	return c
}

// isCDNManaged returns true if the cluster default network is configured for
// no-overlay mode with managed routing.
func isCDNManaged() bool {
	return config.Default.Transport == types.NetworkTransportNoOverlay &&
		config.NoOverlay.Routing == config.NoOverlayRoutingManaged
}

// isCUDNManaged returns true if the given CUDN is configured for
// no-overlay transport with managed routing.
func isCUDNManaged(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork) bool {
	if cudn == nil {
		return false
	}
	return cudn.Spec.Network.Transport == userdefinednetworkv1.TransportOptionNoOverlay &&
		cudn.Spec.Network.NoOverlay != nil &&
		cudn.Spec.Network.NoOverlay.Routing == userdefinednetworkv1.RoutingManaged
}

// getManagedCUDNs returns all CUDNs that are configured for no-overlay with managed routing.
func (c *Controller) getManagedCUDNs() ([]*userdefinednetworkv1.ClusterUserDefinedNetwork, error) {
	if c.cudnLister == nil {
		return nil, nil
	}
	allCUDNs, err := c.cudnLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list CUDNs: %w", err)
	}
	var managed []*userdefinednetworkv1.ClusterUserDefinedNetwork
	for _, cudn := range allCUDNs {
		if isCUDNManaged(cudn) {
			managed = append(managed, cudn)
		}
	}
	return managed, nil
}

// Start starts the managed BGP controller.
func (c *Controller) Start() error {
	klog.Infof("Starting managed BGP controller")

	controllers := []controllerutil.Reconciler{c.nodeController, c.raController, c.frrController}
	if c.cudnController != nil {
		controllers = append(controllers, c.cudnController)
	}

	if err := controllerutil.Start(controllers...); err != nil {
		return err
	}

	if isCDNManaged() {
		// Trigger initial reconciliation for the CDN managed RouteAdvertisement so
		// it is created on startup even before any RA informer events arrive.
		c.raController.Reconcile(managedRouteAdvertisementName(types.DefaultNetworkName))
	}

	return nil
}

// Stop stops the managed BGP controller
func (c *Controller) Stop() {
	klog.Infof("Stopping managed BGP controller")
	controllers := []controllerutil.Reconciler{c.nodeController, c.raController, c.frrController}
	if c.cudnController != nil {
		controllers = append(controllers, c.cudnController)
	}
	controllerutil.Stop(controllers...)
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

// isOwnUpdate checks if an object was last updated by this controller,
// as indicated by its managed fields. Used to avoid reconciling events
// that result from our own writes.
func isOwnUpdate(managedFields []metav1.ManagedFieldsEntry) bool {
	return util.IsLastUpdatedByManager(fieldManager, managedFields)
}

func isManagedRA(ra *ratypes.RouteAdvertisements) bool {
	if ra == nil {
		return false
	}
	_, ok := ra.Labels[managedRANetworkLabel]
	return ok
}

func isManagedBaseFRRConfiguration(cfg *frrtypes.FRRConfiguration) bool {
	if cfg == nil {
		return false
	}
	return cfg.Name == managedFRRConfigurationName
}

func (c *Controller) cudnNeedsUpdate(oldCUDN, newCUDN *userdefinednetworkv1.ClusterUserDefinedNetwork) bool {
	if newCUDN == nil {
		// Delete events bypass ObjNeedsUpdate and are always reconciled.
		return false
	}
	if oldCUDN == nil {
		return isCUDNManaged(newCUDN)
	}

	if !isCUDNManaged(oldCUDN) && !isCUDNManaged(newCUDN) {
		return false
	}
	// Ignore events from our own writes (e.g. ensureManagedNetworkLabel)
	if isOwnUpdate(newCUDN.ManagedFields) {
		return false
	}

	// CUDN spec is immutable, only managedRANetworkLabel changes matter.
	return oldCUDN.Labels[managedRANetworkLabel] != newCUDN.Labels[managedRANetworkLabel]
}

func (c *Controller) raNeedsUpdate(oldRA, newRA *ratypes.RouteAdvertisements) bool {
	if !isManagedRA(oldRA) && !isManagedRA(newRA) {
		return false
	}
	// Ignore events from our own writes.
	// The nil check is a safety guard; newRA should never be nil here as delete events bypass ObjNeedsUpdate.
	if newRA == nil || isOwnUpdate(newRA.ManagedFields) {
		return false
	}
	return oldRA == nil ||
		!reflect.DeepEqual(oldRA.Spec, newRA.Spec) ||
		oldRA.Labels[managedRANetworkLabel] != newRA.Labels[managedRANetworkLabel]
}

func (c *Controller) frrNeedsUpdate(oldFRR, newFRR *frrtypes.FRRConfiguration) bool {
	if !isManagedBaseFRRConfiguration(oldFRR) && !isManagedBaseFRRConfiguration(newFRR) {
		return false
	}
	// Ignore events from our own writes.
	// The nil check is a safety guard; newFRR should never be nil here as delete events bypass ObjNeedsUpdate.
	if newFRR == nil || isOwnUpdate(newFRR.ManagedFields) {
		return false
	}
	return oldFRR == nil ||
		!reflect.DeepEqual(oldFRR.Spec, newFRR.Spec) ||
		oldFRR.Labels[frrConfigManagedLabel] != newFRR.Labels[frrConfigManagedLabel]
}

// reconcileNetwork reconciles managed resources for a network (CDN or CUDN).
// For CUDNs, the key is the CUDN name from the informer. For the CDN, the key
// is types.DefaultNetworkName, enqueued on startup.
func (c *Controller) reconcileNetwork(key string) error {
	var cudn *userdefinednetworkv1.ClusterUserDefinedNetwork
	var managed bool

	if key == types.DefaultNetworkName {
		managed = isCDNManaged()
	} else if c.cudnLister != nil {
		var err error
		cudn, err = c.cudnLister.Get(key)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get CUDN %s: %w", key, err)
		}
		if cudn == nil {
			klog.Infof("CUDN %s was deleted, cleaning up managed RouteAdvertisement", key)
		}
		managed = isCUDNManaged(cudn)
	}

	if managed {
		return c.ensureManagedNetworkResources(key, cudn)
	}
	return c.cleanupManagedNetworkResources(key)
}

func (c *Controller) ensureManagedNetworkResources(networkName string, cudn *userdefinednetworkv1.ClusterUserDefinedNetwork) error {
	networkType := "CDN"
	if cudn != nil {
		networkType = "CUDN"
	}
	if err := c.ensureBaseFRRConfiguration(); err != nil {
		return fmt.Errorf("failed to ensure base FRRConfiguration for %s %s: %w", networkType, networkName, err)
	}
	if cudn != nil {
		if err := c.ensureManagedNetworkLabel(cudn); err != nil {
			return err
		}
	}
	return c.ensureManagedRouteAdvertisement(networkName)
}

func (c *Controller) cleanupManagedNetworkResources(networkName string) error {
	if err := c.deleteManagedRA(networkName); err != nil {
		return err
	}
	return c.cleanupBaseFRRConfigurationsIfUnused()
}

func (c *Controller) cleanupBaseFRRConfigurationsIfUnused() error {
	managedCUDNs, err := c.getManagedCUDNs()
	if err != nil {
		return err
	}
	if !isCDNManaged() && len(managedCUDNs) == 0 {
		return c.cleanupBaseFRRConfigurations()
	}
	return nil
}

func (c *Controller) reconcileRA(key string) error {
	networkName, err := c.resolveNetworkForRA(key)
	if err != nil {
		return err
	}
	if networkName == "" {
		return nil
	}
	return c.reconcileNetwork(networkName)
}

func (c *Controller) reconcileFRRConfiguration(_ string) error {
	managedCUDNs, err := c.getManagedCUDNs()
	if err != nil {
		return err
	}
	if isCDNManaged() || len(managedCUDNs) > 0 {
		return c.ensureBaseFRRConfiguration()
	}
	return c.cleanupBaseFRRConfigurations()
}

// resolveNetworkForRA resolves the network name for the given RA key.
func (c *Controller) resolveNetworkForRA(key string) (string, error) {
	// Get the network name from the RA label if it exists.
	ra, err := c.raLister.Get(key)
	if err == nil {
		if networkName, ok := ra.Labels[managedRANetworkLabel]; ok {
			return networkName, nil
		}
	}

	// If the label is missing, determine the network name by matching the RA hash name.
	if key == managedRouteAdvertisementName(types.DefaultNetworkName) {
		return types.DefaultNetworkName, nil
	}
	if c.cudnLister != nil {
		allCUDNs, err := c.cudnLister.List(labels.Everything())
		if err != nil {
			return "", fmt.Errorf("failed to list CUDNs: %w", err)
		}
		for _, cudn := range allCUDNs {
			if key == managedRouteAdvertisementName(cudn.Name) {
				return cudn.Name, nil
			}
		}
	}
	return "", nil
}

func (c *Controller) reconcileNode(_ string) error {
	// Node IPs only affect the FRRConfiguration peer list.
	managedCUDNs, err := c.getManagedCUDNs()
	if err != nil {
		return err
	}
	if !isCDNManaged() && len(managedCUDNs) == 0 {
		return nil
	}

	if err := c.ensureBaseFRRConfiguration(); err != nil {
		return fmt.Errorf("failed to ensure base FRRConfiguration: %w", err)
	}

	return nil
}

func (c *Controller) ensureBaseFRRConfiguration() error {
	if config.ManagedBGP.Topology != config.ManagedBGPTopologyFullMesh {
		return fmt.Errorf("unsupported managed BGP topology %q, only %q is supported", config.ManagedBGP.Topology, config.ManagedBGPTopologyFullMesh)
	}

	allNodes, err := c.wf.GetNodes()
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}
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

	desiredSpec := frrtypes.FRRConfigurationSpec{
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
	}

	frrConfigName := managedFRRConfigurationName
	existing, err := c.frrLister.FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(frrConfigName)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		frrConfig := &frrtypes.FRRConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      frrConfigName,
				Namespace: config.ManagedBGP.FRRNamespace,
				Labels: map[string]string{
					frrConfigManagedLabel: frrConfigManagedValue,
				},
			},
			Spec: desiredSpec,
		}
		klog.Infof("Creating base FRRConfiguration")
		_, err = c.frrClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Create(context.TODO(), frrConfig, metav1.CreateOptions{FieldManager: fieldManager})
		if err != nil {
			return fmt.Errorf("failed to create base FRRConfiguration: %w", err)
		}
	} else {
		needsUpdate := !reflect.DeepEqual(existing.Spec, desiredSpec) ||
			existing.Labels[frrConfigManagedLabel] != frrConfigManagedValue
		if needsUpdate {
			klog.Infof("Updating base FRRConfiguration %s", existing.Name)
			updated := existing.DeepCopy()
			if updated.Labels == nil {
				updated.Labels = map[string]string{}
			}
			updated.Labels[frrConfigManagedLabel] = frrConfigManagedValue
			updated.Spec = desiredSpec
			_, err = c.frrClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Update(context.TODO(), updated, metav1.UpdateOptions{FieldManager: fieldManager})
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// ensureManagedNetworkLabel sets managedRANetworkLabel=cudn.Name on the CUDN so that
// the managed RA's ClusterUserDefinedNetworkSelector can match the CUDN's NADs by label.
// The RA cannot select by CUDN name directly, so we stamp this label onto the CUDN and
// reference it from the RA's NetworkSelector.
func (c *Controller) ensureManagedNetworkLabel(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork) error {
	if cudn.Labels != nil && cudn.Labels[managedRANetworkLabel] == cudn.Name {
		return nil
	}
	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, managedRANetworkLabel, cudn.Name)
	_, err := c.cudnClient.K8sV1().ClusterUserDefinedNetworks().Patch(
		context.TODO(), cudn.Name, k8stypes.MergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: fieldManager})
	if err != nil {
		return fmt.Errorf("failed to set %s label on CUDN %s: %w", managedRANetworkLabel, cudn.Name, err)
	}
	klog.Infof("Set %s=%s label on CUDN %s", managedRANetworkLabel, cudn.Name, cudn.Name)
	return nil
}

// ensureManagedRouteAdvertisement ensures that the managed RouteAdvertisement for the given
// network exists with the correct spec. It selects the base FRRConfiguration and advertises
// pod networks for the specified network.
func (c *Controller) ensureManagedRouteAdvertisement(networkName string) error {
	var netSelector apitypes.NetworkSelectors
	if networkName == types.DefaultNetworkName {
		if !isCDNManaged() {
			return nil
		}
		netSelector = apitypes.NetworkSelectors{{NetworkSelectionType: apitypes.DefaultNetwork}}
	} else {
		netSelector = apitypes.NetworkSelectors{{
			NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
			ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
				NetworkSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						managedRANetworkLabel: networkName,
					},
				},
			},
		}}
	}

	raName := managedRouteAdvertisementName(networkName)
	ra := &ratypes.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{
			Name: raName,
			Labels: map[string]string{
				managedRANetworkLabel: networkName,
			},
		},
		Spec: ratypes.RouteAdvertisementsSpec{
			NetworkSelectors: netSelector,
			Advertisements: []ratypes.AdvertisementType{
				ratypes.PodNetwork,
			},
			FRRConfigurationSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					frrConfigManagedLabel: frrConfigManagedValue,
				},
			},
			NodeSelector: metav1.LabelSelector{},
		},
	}

	existing, err := c.raLister.Get(raName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.Infof("Creating managed RouteAdvertisement %s", raName)
			_, err = c.raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{FieldManager: fieldManager})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return err
			}
			return nil
		}
		return err
	}

	needsUpdate := !reflect.DeepEqual(existing.Spec, ra.Spec) ||
		existing.Labels[managedRANetworkLabel] != networkName
	if needsUpdate {
		klog.Infof("Updating managed RouteAdvertisement %s", raName)
		updated := existing.DeepCopy()
		if updated.Labels == nil {
			updated.Labels = map[string]string{}
		}
		updated.Labels[managedRANetworkLabel] = networkName
		updated.Spec = ra.Spec
		_, err = c.raClient.K8sV1().RouteAdvertisements().Update(context.TODO(), updated, metav1.UpdateOptions{FieldManager: fieldManager})
		return err
	}

	return nil
}

// deleteManagedRA deletes the managed RouteAdvertisement for the given network name.
func (c *Controller) deleteManagedRA(networkName string) error {
	raName := managedRouteAdvertisementName(networkName)
	err := c.raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), raName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete managed RouteAdvertisement %s: %w", raName, err)
	}
	return nil
}

// cleanupBaseFRRConfigurations deletes the managed base FRRConfiguration.
func (c *Controller) cleanupBaseFRRConfigurations() error {
	klog.Infof("Deleting managed base FRRConfiguration %s", managedFRRConfigurationName)
	if err := c.frrClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Delete(
		context.TODO(), managedFRRConfigurationName, metav1.DeleteOptions{},
	); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete base FRRConfiguration %s: %w", managedFRRConfigurationName, err)
	}
	return nil
}
