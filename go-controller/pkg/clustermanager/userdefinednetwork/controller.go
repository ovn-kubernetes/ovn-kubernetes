package userdefinednetwork

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	netv1clientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	netv1infomer "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"
	netv1lister "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	corev1informer "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/reference"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/notifier"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/template"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnapplyconfkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/applyconfiguration/userdefinednetwork/v1"
	userdefinednetworkclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	userdefinednetworkscheme "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/scheme"
	userdefinednetworkinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/informers/externalversions/userdefinednetwork/v1"
	userdefinednetworklister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	vtepinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/informers/externalversions/vtep/v1"
	vteplister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/listers/vtep/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	conditionTypeNetworkCreated = "NetworkCreated"

	// Condition reasons
	reasonNADCreated   = "NetworkAttachmentDefinitionCreated"
	reasonSyncError    = "SyncError"
	reasonVTEPNotFound = "VTEPNotFound"
	reasonNADDeleted   = "NetworkAttachmentDefinitionDeleted"
	reasonNADSyncError = "NetworkAttachmentDefinitionSyncError"

	// MaxEVPNVIDs is the maximum number of VIDs available for EVPN networks (0-4094, but 0 and 1 are reserved).
	MaxEVPNVIDs = 4095
	// reservedVIDZeroKey is the key used to reserve VID 0 (reserved per IEEE 802.1Q for priority tagging).
	reservedVIDZeroKey = "__vid_zero_reserved__"
	// reservedVIDOneKey is the key used to reserve VID 1 (default VLAN on many switches, avoided by convention).
	reservedVIDOneKey = "__vid_one_reserved__"
)

// macVRFKey returns the VID allocator key for a network's MAC-VRF.
func macVRFKey(networkName string) string {
	return networkName + "/macvrf"
}

// ipVRFKey returns the VID allocator key for a network's IP-VRF.
func ipVRFKey(networkName string) string {
	return networkName + "/ipvrf"
}

type RenderNetAttachDefManifest func(obj client.Object, targetNamespace string, opts ...template.RenderOption) (*netv1.NetworkAttachmentDefinition, error)

type networkInUseError struct {
	err error
}

func (n *networkInUseError) Error() string {
	return n.err.Error()
}

// vtepNotFoundError indicates that a required VTEP CR does not exist.
type vtepNotFoundError struct {
	vtepName string
}

func (e *vtepNotFoundError) Error() string {
	return fmt.Sprintf("VTEP %q does not exist", e.vtepName)
}

type Controller struct {
	// cudnController manage ClusterUserDefinedNetwork CRs.
	cudnController controller.Controller
	// udnController manage UserDefinedNetwork CRs.
	udnController controller.Controller
	// nadNotifier notifies subscribing controllers about NetworkAttachmentDefinition events.
	nadNotifier *notifier.NetAttachDefNotifier
	// namespaceInformer notifies subscribing controllers about Namespace events.
	namespaceNotifier *notifier.NamespaceNotifier
	// namespaceTracker tracks each CUDN CRs affected namespaces, enable finding stale NADs.
	// Keys are CR name, value is affected namespace names slice.
	namespaceTracker     map[string]sets.Set[string]
	namespaceTrackerLock sync.RWMutex
	// renderNadFn render NAD manifest from given object, enable replacing in tests.
	renderNadFn RenderNetAttachDefManifest
	// createNetworkLock lock should be held when NAD is created to avoid having two components
	// trying to create an object with the same name.
	createNetworkLock sync.Mutex

	networkManager networkmanager.Interface

	// vidAllocator allocates cluster-wide VLAN IDs for EVPN networks.
	// VIDs are allocated per network name and stored in the NAD config JSON.
	vidAllocator id.Allocator

	udnClient         userdefinednetworkclientset.Interface
	udnLister         userdefinednetworklister.UserDefinedNetworkLister
	cudnLister        userdefinednetworklister.ClusterUserDefinedNetworkLister
	nadClient         netv1clientset.Interface
	nadLister         netv1lister.NetworkAttachmentDefinitionLister
	podInformer       corev1informer.PodInformer
	namespaceInformer corev1informer.NamespaceInformer
	// vtepLister provides read access to VTEP CRs for validating EVPN configuration.
	vtepLister vteplister.VTEPLister
	// vtepNotifier notifies subscribing controllers about VTEP events.
	vtepNotifier *notifier.VTEPNotifier

	networkInUseRequeueInterval time.Duration
	eventRecorder               record.EventRecorder
}

func New(
	nadClient netv1clientset.Interface,
	nadInfomer netv1infomer.NetworkAttachmentDefinitionInformer,
	udnClient userdefinednetworkclientset.Interface,
	udnInformer userdefinednetworkinformer.UserDefinedNetworkInformer,
	cudnInformer userdefinednetworkinformer.ClusterUserDefinedNetworkInformer,
	renderNadFn RenderNetAttachDefManifest,
	networkManager networkmanager.Interface,
	podInformer corev1informer.PodInformer,
	namespaceInformer corev1informer.NamespaceInformer,
	vtepInformer vtepinformer.VTEPInformer,
	eventRecorder record.EventRecorder,
) *Controller {
	udnLister := udnInformer.Lister()
	cudnLister := cudnInformer.Lister()

	// Allocates VIDs in range 1-4094 (0 is reserved per IEEE 802.1Q).
	vidAllocator := id.NewIDAllocator("EVPN-VIDs", MaxEVPNVIDs)

	c := &Controller{
		nadClient:         nadClient,
		nadLister:         nadInfomer.Lister(),
		udnClient:         udnClient,
		udnLister:         udnLister,
		cudnLister:        cudnLister,
		renderNadFn:       renderNadFn,
		podInformer:       podInformer,
		namespaceInformer: namespaceInformer,
		networkManager:    networkManager,
		namespaceTracker:  map[string]sets.Set[string]{},
		vidAllocator:      vidAllocator,
		eventRecorder:     eventRecorder,
	}
	udnCfg := &controller.ControllerConfig[userdefinednetworkv1.UserDefinedNetwork]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileUDN,
		ObjNeedsUpdate: c.udnNeedUpdate,
		Threadiness:    1,
		Informer:       udnInformer.Informer(),
		Lister:         udnLister.List,
	}
	c.udnController = controller.NewController[userdefinednetworkv1.UserDefinedNetwork]("user-defined-network-controller", udnCfg)

	cudnCfg := &controller.ControllerConfig[userdefinednetworkv1.ClusterUserDefinedNetwork]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileCUDN,
		ObjNeedsUpdate: c.cudnNeedUpdate,
		Threadiness:    1,
		Informer:       cudnInformer.Informer(),
		Lister:         cudnLister.List,
	}
	c.cudnController = controller.NewController[userdefinednetworkv1.ClusterUserDefinedNetwork]("cluster-user-defined-network-controller", cudnCfg)

	c.nadNotifier = notifier.NewNetAttachDefNotifier(nadInfomer, c)
	c.namespaceNotifier = notifier.NewNamespaceNotifier(namespaceInformer, c)

	// Setup EVPN components only when EVPN is enabled.
	if util.IsEVPNEnabled() && vtepInformer != nil {
		// Setup VTEP watching for EVPN support.
		c.vtepLister = vtepInformer.Lister()
		c.vtepNotifier = notifier.NewVTEPNotifier(vtepInformer, c)
	}

	return c
}

func (c *Controller) Run() error {
	klog.Infof("Starting user-defined network controllers")

	controllers := []controller.Reconciler{
		c.cudnController,
		c.udnController,
		c.nadNotifier.Controller,
		c.namespaceNotifier.Controller,
	}
	if c.vtepNotifier != nil {
		controllers = append(controllers, c.vtepNotifier.Controller)
	}

	if err := controller.StartWithInitialSync(c.initializeController, controllers...); err != nil {
		return fmt.Errorf("unable to start user-defined network controller: %v", err)
	}

	if util.IsPreconfiguredUDNAddressesEnabled() {
		if _, err := util.EnsureDefaultNetworkNAD(c.nadLister, c.nadClient); err != nil {
			return fmt.Errorf("failed to ensure default network nad exists: %w", err)
		}
	}

	return nil
}

// initializeController performs all startup initialization before controllers begin processing.
func (c *Controller) initializeController() error {
	// Reserve VID 0 and VID 1 to ensure they're never allocated to any network.
	// VID 0 is reserved per IEEE 802.1Q standard.
	// VID 1 is the default VLAN on many switches and avoided by convention.
	if err := c.vidAllocator.ReserveID(reservedVIDZeroKey, 0); err != nil {
		return fmt.Errorf("failed to reserve VID 0: %w", err)
	}
	if err := c.vidAllocator.ReserveID(reservedVIDOneKey, 1); err != nil {
		return fmt.Errorf("failed to reserve VID 1: %w", err)
	}

	cudnNADs, err := c.buildCUDNToNADs()
	if err != nil {
		return err
	}
	if len(cudnNADs) == 0 {
		return nil
	}

	c.initializeNamespaceTracker(cudnNADs)
	if util.IsEVPNEnabled() {
		// Recover VID allocations from existing EVPN CUDNs.
		// Recovery failures are logged and the affected CUDNs are enqueued for reconciliation,
		// but don't block startup - this prevents a DoS where a malicious NAD could
		// crash the entire cluster-manager.
		c.recoverEVPNVIDs(cudnNADs)
	}

	return nil
}

// cudnWithNADs pairs a CUDN with its owned NADs.
type cudnWithNADs struct {
	cudn *userdefinednetworkv1.ClusterUserDefinedNetwork
	nads []netv1.NetworkAttachmentDefinition
}

// cudnToNADs maps CUDN name to its object and owned NADs.
type cudnToNADs map[string]*cudnWithNADs

// buildCUDNToNADs builds an index of CUDNs to their owned NADs.
// It returns an entry for every existing CUDN, including CUDNs that currently own no NADs
func (c *Controller) buildCUDNToNADs() (cudnToNADs, error) {
	cudns, err := c.cudnLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	if len(cudns) == 0 {
		return nil, nil
	}

	nads, err := c.nadLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	cudnByUID := make(map[types.UID]*userdefinednetworkv1.ClusterUserDefinedNetwork, len(cudns))
	index := make(cudnToNADs, len(cudns))
	for _, cudn := range cudns {
		cudnByUID[cudn.UID] = cudn
		index[cudn.Name] = &cudnWithNADs{cudn: cudn}
	}

	for _, nad := range nads {
		if nad == nil {
			continue
		}
		controllerRef := metav1.GetControllerOfNoCopy(nad)
		if controllerRef == nil {
			continue
		}
		if cudn, ok := cudnByUID[controllerRef.UID]; ok {
			index[cudn.Name].nads = append(index[cudn.Name].nads, *nad.DeepCopy())
		}
	}

	return index, nil
}

// initializeNamespaceTracker populates the namespace tracker with NAD namespaces owned by each CUDN.
func (c *Controller) initializeNamespaceTracker(cudnNADs cudnToNADs) {
	for cudnName, entry := range cudnNADs {
		c.namespaceTracker[cudnName] = sets.New[string]()
		for _, nad := range entry.nads {
			c.namespaceTracker[cudnName].Insert(nad.Namespace)
		}
	}
}

// recoverEVPNVIDs recovers VID allocations from existing EVPN CUDNs using
// NetworkManager's cached NetInfo. NetworkManager has already processed all NADs
// by the time this function is called (it starts before UDN controller).
//
// CUDNs are processed in order of creation timestamp (oldest first) to ensure
// deterministic VID assignment when conflicts occur. If two CUDNs have NADs
// claiming the same VID, the oldest CUDN wins ("first come, first served").
// CUDN name is used as tie-breaker when timestamps are equal.
//
// If VID recovery fails for a CUDN (e.g., NetworkManager couldn't parse the NAD),
// this logs an error and enqueues the CUDN for reconciliation.
func (c *Controller) recoverEVPNVIDs(cudnNADs cudnToNADs) {
	// Extract EVPN CUDNs with NADs into a slice for deterministic ordering.
	evpnCUDNs := make([]*cudnWithNADs, 0, len(cudnNADs))
	for _, entry := range cudnNADs {
		if entry.cudn.Spec.Network.Transport != userdefinednetworkv1.TransportOptionEVPN {
			continue
		}
		if len(entry.nads) == 0 {
			klog.V(4).Infof("EVPN CUDN %s has no NADs, skipping VID recovery", entry.cudn.Name)
			continue
		}
		evpnCUDNs = append(evpnCUDNs, entry)
	}

	// Sort by creation timestamp (oldest first) for deterministic conflict resolution.
	// When two CUDNs have conflicting VIDs, the oldest one wins.
	// Use name as tie-breaker when timestamps are equal for consistent ordering.
	slices.SortFunc(evpnCUDNs, func(a, b *cudnWithNADs) int {
		if a.cudn.CreationTimestamp.Before(&b.cudn.CreationTimestamp) {
			return -1
		}
		if b.cudn.CreationTimestamp.Before(&a.cudn.CreationTimestamp) {
			return 1
		}
		return strings.Compare(a.cudn.Name, b.cudn.Name)
	})

	for _, entry := range evpnCUDNs {
		if err := c.recoverEVPNVIDsForCUDN(entry.cudn.Name); err != nil {
			klog.Errorf("VID recovery failed for EVPN CUDN %s: %v. "+
				"The CUDN will be reconciled and existing NAD VIDs will be preserved if possible.",
				entry.cudn.Name, err)
			c.cudnController.Reconcile(entry.cudn.Name)
		}
	}
}

// recoverEVPNVIDsForCUDN attempts to recover VIDs for a single CUDN using NetworkManager's cache.
// Returns nil if VIDs were successfully recovered or if no VIDs are allocated yet.
// Returns error if VID reservation fails (e.g., conflict with another network).
func (c *Controller) recoverEVPNVIDsForCUDN(cudnName string) error {
	networkName := util.GenerateCUDNNetworkName(cudnName)

	// Use NetworkManager's cached NetInfo - it has already parsed the NAD
	netInfo := c.networkManager.GetNetwork(networkName)
	if netInfo == nil {
		// NetworkManager doesn't have this network cached. This can happen if:
		// - NetworkManager failed to parse the NAD (corrupted)
		// - NAD doesn't exist yet
		return fmt.Errorf("network %s not found in NetworkManager cache", networkName)
	}

	macVRFVID := netInfo.EVPNMACVRFVID()
	ipVRFVID := netInfo.EVPNIPVRFVID()

	// Check if this network has EVPN VIDs allocated
	if macVRFVID == 0 && ipVRFVID == 0 {
		klog.V(4).Infof("EVPN CUDN %s has no VIDs allocated yet, skipping recovery", cudnName)
		return nil // No VIDs to recover
	}

	if err := c.reserveRecoveredVIDs(cudnName, macVRFVID, ipVRFVID); err != nil {
		return fmt.Errorf("failed to reserve VIDs for cudn %s: %w", cudnName, err)
	}

	klog.V(4).Infof("Recovered VIDs for CUDN %s (macVRF=%d, ipVRF=%d)", cudnName, macVRFVID, ipVRFVID)
	return nil
}

// reserveRecoveredVIDs reserves the given VIDs in the allocator for a network.
// VIDs of 0 are skipped (not allocated).
//
// Both VIDs are attempted even if one fails - this maximizes recovery and protects
// as many VIDs as possible. We don't release successfully reserved VIDs on partial
// failure because they represent state that already exists in NADs; releasing them
// could allow another network to "steal" the VID, causing route leakage.
func (c *Controller) reserveRecoveredVIDs(networkName string, macVRFVID, ipVRFVID int) error {
	var errs []error

	if macVRFVID > 0 {
		if err := c.vidAllocator.ReserveID(macVRFKey(networkName), macVRFVID); err != nil {
			errs = append(errs, fmt.Errorf("failed to reserve VID %d for MAC-VRF of network %s: %w", macVRFVID, networkName, err))
		} else {
			klog.V(4).Infof("Recovered VID %d for MAC-VRF of network %s", macVRFVID, networkName)
		}
	}
	if ipVRFVID > 0 {
		if err := c.vidAllocator.ReserveID(ipVRFKey(networkName), ipVRFVID); err != nil {
			errs = append(errs, fmt.Errorf("failed to reserve VID %d for IP-VRF of network %s: %w", ipVRFVID, networkName, err))
		} else {
			klog.V(4).Infof("Recovered VID %d for IP-VRF of network %s", ipVRFVID, networkName)
		}
	}

	return errors.Join(errs...)
}

// releaseVIDForNetwork releases the VIDs allocated for a network's VRFs.
//
// NOTE: VID release is not synchronized with node-side dataplane cleanup.
// In theory, a rapidly created new network could get the same VID while nodes
// are still tearing down the old network's bridge configuration. In practice,
// VID collisions are unlikely because the allocator is monotonic and won't
// reallocate the same VID unless the pool fills up or CUDNs are recycled rapidly.
// The actual mitigation is on the node-side: nodes should check for VID conflicts
// and refuse to configure a VID already in use by a different network, waiting
// until the old network is cleaned up.
func (c *Controller) releaseVIDForNetwork(networkName string) {
	macVID := c.vidAllocator.ReleaseID(macVRFKey(networkName))
	ipVID := c.vidAllocator.ReleaseID(ipVRFKey(networkName))
	if macVID >= 0 || ipVID >= 0 {
		klog.V(4).Infof("Released VIDs for network %s: MAC-VRF=%d, IP-VRF=%d", networkName, macVID, ipVID)
	}
}

func (c *Controller) Shutdown() {
	controllers := []controller.Reconciler{
		c.cudnController,
		c.udnController,
		c.nadNotifier.Controller,
		c.namespaceNotifier.Controller,
	}
	if c.vtepNotifier != nil {
		controllers = append(controllers, c.vtepNotifier.Controller)
	}
	controller.Stop(controllers...)
}

// ReconcileNetAttachDef enqueue NAD requests following NAD events.
func (c *Controller) ReconcileNetAttachDef(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("failed to split meta namespace key %q: %v", key, err)
	}
	nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get NetworkAttachmentDefinition %q from cache: %v", key, err)
	}
	ownerRef := metav1.GetControllerOf(nad)
	if ownerRef == nil {
		return nil
	}

	switch ownerRef.Kind {
	case "ClusterUserDefinedNetwork":
		owner, err := c.cudnLister.Get(ownerRef.Name)
		if err != nil {
			return fmt.Errorf("failed to get ClusterUserDefinedNetwork %q from cache: %v", ownerRef.Name, err)
		}
		ownerKey, err := cache.MetaNamespaceKeyFunc(owner)
		if err != nil {
			return fmt.Errorf("failed to generate meta namespace key for CUDN: %v", err)
		}
		c.cudnController.Reconcile(ownerKey)
	case "UserDefinedNetwork":
		owner, err := c.udnLister.UserDefinedNetworks(nad.Namespace).Get(ownerRef.Name)
		if err != nil {
			return fmt.Errorf("failed to get UserDefinedNetwork %q from cache: %v", ownerRef.Name, err)
		}
		ownerKey, err := cache.MetaNamespaceKeyFunc(owner)
		if err != nil {
			return fmt.Errorf("failed to generate meta namespace key for UDN: %v", err)
		}
		c.udnController.Reconcile(ownerKey)
	default:
		return nil
	}
	return nil
}

// ReconcileNamespace enqueue relevant Cluster UDN CR requests following namespace events.
func (c *Controller) ReconcileNamespace(key string) error {
	namespace, err := c.namespaceInformer.Lister().Get(key)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get namespace %q from cache: %w", key, err)
	}

	var namespaceLabels labels.Set
	if namespace != nil {
		namespaceLabels = namespace.Labels
	}

	c.namespaceTrackerLock.RLock()
	defer c.namespaceTrackerLock.RUnlock()

	for cudnName, affectedNamespaces := range c.namespaceTracker {
		affectedNamespace := affectedNamespaces.Has(key)

		// For deleted namespaces, only reconcile if tracked
		if namespace == nil {
			if affectedNamespace {
				klog.Errorf("BUG: namespace %q was deleted but still tracked by ClusterUDN %q, forcing reconcile to cleanup", key, cudnName)
				c.cudnController.Reconcile(cudnName)
			}
			continue
		}

		selectedNamespace := false
		if !affectedNamespace {
			cudn, err := c.cudnLister.Get(cudnName)
			if err != nil {
				return fmt.Errorf("failed to get CUDN %q from cache: %w", cudnName, err)
			}
			cudnSelector, err := metav1.LabelSelectorAsSelector(&cudn.Spec.NamespaceSelector)
			if err != nil {
				return fmt.Errorf("failed to convert CUDN namespace selector: %w", err)
			}
			selectedNamespace = cudnSelector.Matches(namespaceLabels)
		}

		if affectedNamespace || selectedNamespace {
			klog.Infof("Enqueue ClusterUDN %q following namespace %q event", cudnName, key)
			c.cudnController.Reconcile(cudnName)
		}
	}

	return nil
}

// UpdateSubsystemCondition may be used by other controllers handling UDN/NAD/network setup to report conditions that
// may affect UDN functionality.
// FieldManager should be unique for every subsystem.
// If given network is not managed by a UDN, no condition will be reported and no error will be returned.
// Events may be used to report additional information about the condition to avoid overloading the condition message.
// When condition should not change, but new events should be reported, pass condition = nil.
func (c *Controller) UpdateSubsystemCondition(
	networkName string,
	fieldManager string,
	condition *metav1.Condition,
	events ...*util.EventDetails,
) error {
	// try to find udn using network name
	udnNamespace, udnName := util.ParseNetworkName(networkName)
	if udnName == "" || udnNamespace == "" {
		return nil
	}
	udn, err := c.udnLister.UserDefinedNetworks(udnNamespace).Get(udnName)
	if err != nil {
		return nil
	}

	udnRef, err := reference.GetReference(userdefinednetworkscheme.Scheme, udn)
	if err != nil {
		return fmt.Errorf("failed to get object reference for UserDefinedNetwork %s/%s: %w", udnNamespace, udnName, err)
	}
	for _, event := range events {
		c.eventRecorder.Event(udnRef, event.EventType, event.Reason, event.Note)
	}

	if condition == nil {
		return nil
	}

	applyCondition := &metaapplyv1.ConditionApplyConfiguration{
		Type:               &condition.Type,
		Status:             &condition.Status,
		LastTransitionTime: &condition.LastTransitionTime,
		Reason:             &condition.Reason,
		Message:            &condition.Message,
	}

	udnStatus := udnapplyconfkv1.UserDefinedNetworkStatus().WithConditions(applyCondition)

	applyUDN := udnapplyconfkv1.UserDefinedNetwork(udnName, udnNamespace).WithStatus(udnStatus)
	opts := metav1.ApplyOptions{
		FieldManager: fieldManager,
		Force:        true,
	}
	_, err = c.udnClient.K8sV1().UserDefinedNetworks(udnNamespace).ApplyStatus(context.Background(), applyUDN, opts)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to update UserDefinedNetwork %s/%s status: %w", udnNamespace, udnName, err)
	}
	return nil
}

func (c *Controller) udnNeedUpdate(_, _ *userdefinednetworkv1.UserDefinedNetwork) bool {
	return true
}

// reconcileUDN get UserDefinedNetwork CR key and reconcile it according to spec.
// It creates NAD according to spec at the namespace the CR resides.
// The NAD objects are created with the same key as the request CR, having both kinds have the same key enable
// the controller to act on NAD changes as well and reconciles NAD objects (e.g: in case NAD is deleted it will be re-created).
func (c *Controller) reconcileUDN(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	udn, err := c.udnLister.UserDefinedNetworks(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get UserDefinedNetwork %q from cache: %v", key, err)
	}

	udnCopy := udn.DeepCopy()

	nadCopy, syncErr := c.syncUserDefinedNetwork(udnCopy)

	updateStatusErr := c.updateUserDefinedNetworkStatus(udnCopy, nadCopy, syncErr)

	var networkInUse *networkInUseError
	if errors.As(syncErr, &networkInUse) {
		// Call ReconcileRateLimited directly to ensure retries without the default limits
		c.udnController.ReconcileRateLimited(key)
		return updateStatusErr
	}

	return errors.Join(syncErr, updateStatusErr)
}

func (c *Controller) syncUserDefinedNetwork(udn *userdefinednetworkv1.UserDefinedNetwork) (*netv1.NetworkAttachmentDefinition, error) {
	if udn == nil {
		return nil, nil
	}

	var role, topology string
	if udn.Spec.Layer2 != nil {
		role = string(udn.Spec.Layer2.Role)
	} else if udn.Spec.Layer3 != nil {
		role = string(udn.Spec.Layer3.Role)
	}
	topology = string(udn.Spec.Topology)

	if !udn.DeletionTimestamp.IsZero() { // udn is being  deleted
		if controllerutil.ContainsFinalizer(udn, template.FinalizerUserDefinedNetwork) {
			if err := c.deleteNAD(udn, udn.Namespace); err != nil {
				return nil, fmt.Errorf("failed to delete NetworkAttachmentDefinition [%s/%s]: %w", udn.Namespace, udn.Name, err)
			}

			// Ensure that the network controller is stopped(GetActiveNetwork returns nil) before allowing the UDN
			// to be removed.
			if c.networkManager.GetActiveNetwork(util.GenerateUDNNetworkName(udn.Namespace, udn.Name)) != nil {
				return nil, &networkInUseError{err: fmt.Errorf("cannot remove UDN, controller for network %s is still running", util.GenerateUDNNetworkName(udn.Namespace, udn.Name))}
			}

			controllerutil.RemoveFinalizer(udn, template.FinalizerUserDefinedNetwork)
			udn, err := c.udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Update(context.Background(), udn, metav1.UpdateOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to remove finalizer to UserDefinedNetwork: %w", err)
			}
			klog.Infof("Finalizer removed from UserDefinedNetworks [%s/%s]", udn.Namespace, udn.Name)
			metrics.DecrementUDNCount(role, topology)
			metrics.DeleteDynamicUDNNodeCount(util.GenerateUDNNetworkName(udn.Namespace, udn.Name))
		}

		return nil, nil
	}

	if finalizerAdded := controllerutil.AddFinalizer(udn, template.FinalizerUserDefinedNetwork); finalizerAdded {
		udn, err := c.udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Update(context.Background(), udn, metav1.UpdateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to add finalizer to UserDefinedNetwork: %w", err)
		}
		klog.Infof("Added Finalizer to UserDefinedNetwork [%s/%s]", udn.Namespace, udn.Name)
		metrics.IncrementUDNCount(role, topology)
	}

	return c.updateNAD(udn, udn.Namespace)
}

func (c *Controller) updateUserDefinedNetworkStatus(udn *userdefinednetworkv1.UserDefinedNetwork, nad *netv1.NetworkAttachmentDefinition, syncError error) error {
	if udn == nil {
		return nil
	}

	networkCreatedCondition := newNetworkCreatedCondition(nad, syncError)

	updated := meta.SetStatusCondition(&udn.Status.Conditions, *networkCreatedCondition)
	if !updated {
		return nil
	}

	var err error
	conditionsApply := make([]*metaapplyv1.ConditionApplyConfiguration, len(udn.Status.Conditions))
	for i, condition := range udn.Status.Conditions {
		conditionsApply[i] = &metaapplyv1.ConditionApplyConfiguration{
			Type:               &condition.Type,
			Status:             &condition.Status,
			LastTransitionTime: &condition.LastTransitionTime,
			Reason:             &condition.Reason,
			Message:            &condition.Message,
		}
	}
	udnApplyConf := udnapplyconfkv1.UserDefinedNetwork(udn.Name, udn.Namespace).
		WithStatus(udnapplyconfkv1.UserDefinedNetworkStatus().
			WithConditions(conditionsApply...))
	opts := metav1.ApplyOptions{FieldManager: "user-defined-network-controller"}
	udn, err = c.udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).ApplyStatus(context.Background(), udnApplyConf, opts)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to update UserDefinedNetwork status: %w", err)
	}
	klog.Infof("Updated status UserDefinedNetwork [%s/%s]", udn.Namespace, udn.Name)
	return nil
}

func newNetworkCreatedCondition(nad *netv1.NetworkAttachmentDefinition, syncError error) *metav1.Condition {
	now := metav1.Now()
	networkCreatedCondition := &metav1.Condition{
		Type:               conditionTypeNetworkCreated,
		Status:             metav1.ConditionTrue,
		Reason:             reasonNADCreated,
		Message:            "NetworkAttachmentDefinition has been created",
		LastTransitionTime: now,
	}

	if nad != nil && !nad.DeletionTimestamp.IsZero() {
		networkCreatedCondition.Status = metav1.ConditionFalse
		networkCreatedCondition.Reason = reasonNADDeleted
		networkCreatedCondition.Message = "NetworkAttachmentDefinition is being deleted"
	}
	if syncError != nil {
		networkCreatedCondition.Status = metav1.ConditionFalse
		networkCreatedCondition.Reason = reasonSyncError
		networkCreatedCondition.Message = syncError.Error()
	}

	return networkCreatedCondition
}

func (c *Controller) cudnNeedUpdate(_ *userdefinednetworkv1.ClusterUserDefinedNetwork, _ *userdefinednetworkv1.ClusterUserDefinedNetwork) bool {
	return true
}

// reconcileCUDN get ClusterUserDefinedNetwork CR key and reconcile it according to spec.
// It creates NADs according to spec at the specified selected namespaces.
// The NAD objects are created with the same key as the request CR, having both kinds have the same key enable
// the controller to act on NAD changes as well and reconciles NAD objects (e.g: in case NAD is deleted it will be re-created).
func (c *Controller) reconcileCUDN(key string) error {
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	cudn, err := c.cudnLister.Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get ClusterUserDefinedNetwork %q from cache: %v", key, err)
	}

	cudnCopy := cudn.DeepCopy()

	nads, syncErr := c.syncClusterUDN(cudnCopy)

	updateStatusErr := c.updateClusterUDNStatus(cudnCopy, nads, syncErr)

	var networkInUse *networkInUseError
	if errors.As(syncErr, &networkInUse) {
		// Call ReconcileRateLimited directly to ensure retries without the default limits
		c.cudnController.ReconcileRateLimited(key)
		return updateStatusErr
	}

	// vtepNotFoundError is non-fatal: the status has been updated to reflect
	// the missing VTEP, and the VTEPNotifier will re-queue this CUDN when
	// the VTEP is created. No need to return an error that would cause retries.
	var vtepNotFound *vtepNotFoundError
	if errors.As(syncErr, &vtepNotFound) {
		return updateStatusErr
	}

	return errors.Join(syncErr, updateStatusErr)
}

func (c *Controller) syncClusterUDN(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork) ([]netv1.NetworkAttachmentDefinition, error) {
	c.namespaceTrackerLock.Lock()
	defer c.namespaceTrackerLock.Unlock()

	if cudn == nil {
		return nil, nil
	}

	cudnName := cudn.Name
	affectedNamespaces := c.namespaceTracker[cudnName]

	var role, topology string
	if cudn.Spec.Network.Layer2 != nil {
		role = string(cudn.Spec.Network.Layer2.Role)
	} else if cudn.Spec.Network.Layer3 != nil {
		role = string(cudn.Spec.Network.Layer3.Role)
	} else if cudn.Spec.Network.Localnet != nil {
		role = string(cudn.Spec.Network.Localnet.Role)
	}
	topology = string(cudn.Spec.Network.Topology)

	if !cudn.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(cudn, template.FinalizerUserDefinedNetwork) {
			var errs []error
			for nsToDelete := range affectedNamespaces {
				if err := c.deleteNAD(cudn, nsToDelete); err != nil {
					errs = append(errs, fmt.Errorf("failed to delete NetworkAttachmentDefinition [%s/%s]: %w",
						nsToDelete, cudnName, err))
				} else {
					c.namespaceTracker[cudnName].Delete(nsToDelete)
				}
			}

			if len(errs) > 0 {
				return nil, errors.Join(errs...)
			}

			// Ensure that the network controller is stopped(GetActiveNetwork returns nil) before allowing the cUDN
			// to be removed.
			if c.networkManager.GetActiveNetwork(util.GenerateCUDNNetworkName(cudn.Name)) != nil {
				return nil, &networkInUseError{err: fmt.Errorf("cannot remove cluster UDN, controller for network %s is still running", util.GenerateCUDNNetworkName(cudn.Name))}
			}

			var err error
			controllerutil.RemoveFinalizer(cudn, template.FinalizerUserDefinedNetwork)
			cudn, err = c.udnClient.K8sV1().ClusterUserDefinedNetworks().Update(context.Background(), cudn, metav1.UpdateOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to remove finalizer from ClusterUserDefinedNetwork %q: %w",
					cudnName, err)
			}
			klog.Infof("Finalizer removed from ClusterUserDefinedNetwork %q", cudn.Name)
			delete(c.namespaceTracker, cudnName)
			metrics.DecrementCUDNCount(role, topology)
			metrics.DeleteDynamicUDNNodeCount(util.GenerateCUDNNetworkName(cudn.Name))
			c.releaseVIDForNetwork(cudnName)
		}

		return nil, nil
	}

	if _, exist := c.namespaceTracker[cudnName]; !exist {
		// start tracking CR
		c.namespaceTracker[cudnName] = sets.Set[string]{}
	}

	if finalizerAdded := controllerutil.AddFinalizer(cudn, template.FinalizerUserDefinedNetwork); finalizerAdded {
		var err error
		cudn, err = c.udnClient.K8sV1().ClusterUserDefinedNetworks().Update(context.Background(), cudn, metav1.UpdateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to add finalizer to ClusterUserDefinedNetwork %q: %w", cudnName, err)
		}
		klog.Infof("Added Finalizer to ClusterUserDefinedNetwork %q", cudnName)
		metrics.IncrementCUDNCount(role, topology)
	}

	if err := c.validateEVPN(cudn); err != nil {
		return nil, err
	}

	selectedNamespaces, err := c.getSelectedNamespaces(cudn.Spec.NamespaceSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to get selected namespaces: %w", err)
	}

	var errs []error
	for nsToDelete := range affectedNamespaces.Difference(selectedNamespaces) {
		if err := c.deleteNAD(cudn, nsToDelete); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete NetworkAttachmentDefinition [%s/%s]: %w",
				nsToDelete, cudnName, err))
		} else {
			c.namespaceTracker[cudnName].Delete(nsToDelete)
		}
	}

	var nads []netv1.NetworkAttachmentDefinition
	for nsToUpdate := range selectedNamespaces {
		nad, err := c.updateNAD(cudn, nsToUpdate)
		if err != nil {
			errs = append(errs, err)
		} else {
			c.namespaceTracker[cudn.Name].Insert(nsToUpdate)
			nads = append(nads, *nad)
		}
	}

	return nads, errors.Join(errs...)
}

// getSelectedNamespaces list all selected namespaces according to given selector and create
// a set of the selected namespaces keys.
func (c *Controller) getSelectedNamespaces(sel metav1.LabelSelector) (sets.Set[string], error) {
	selectedNamespaces := sets.Set[string]{}
	labelSelector, err := metav1.LabelSelectorAsSelector(&sel)
	if err != nil {
		return nil, fmt.Errorf("failed to create label-selector: %w", err)
	}
	selectedNamespacesList, err := c.namespaceInformer.Lister().List(labelSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to list namespaces: %w", err)
	}
	for _, selectedNs := range selectedNamespacesList {
		if !selectedNs.DeletionTimestamp.IsZero() {
			klog.V(5).Infof("Namespace %s is being deleted, skipping", selectedNs.Name)
			continue
		}
		selectedNamespaces.Insert(selectedNs.Name)
	}
	return selectedNamespaces, nil
}

func (c *Controller) updateClusterUDNStatus(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork, nads []netv1.NetworkAttachmentDefinition, syncError error) error {
	if cudn == nil {
		return nil
	}

	// sort NADs by namespace names to avoid redundant updated due to inconsistent ordering
	slices.SortFunc(nads, func(a, b netv1.NetworkAttachmentDefinition) int {
		return strings.Compare(a.Namespace, b.Namespace)
	})

	networkCreatedCondition := newClusterNetworkCreatedCondition(nads, syncError)

	updated := meta.SetStatusCondition(&cudn.Status.Conditions, networkCreatedCondition)
	if !updated {
		return nil
	}
	conditionsApply := make([]*metaapplyv1.ConditionApplyConfiguration, len(cudn.Status.Conditions))
	for i, condition := range cudn.Status.Conditions {
		conditionsApply[i] = &metaapplyv1.ConditionApplyConfiguration{
			Type:               &condition.Type,
			Status:             &condition.Status,
			LastTransitionTime: &condition.LastTransitionTime,
			Reason:             &condition.Reason,
			Message:            &condition.Message,
		}
	}
	var err error
	applyConf := udnapplyconfkv1.ClusterUserDefinedNetwork(cudn.Name).
		WithStatus(udnapplyconfkv1.ClusterUserDefinedNetworkStatus().
			WithConditions(conditionsApply...))
	opts := metav1.ApplyOptions{FieldManager: "user-defined-network-controller"}
	cudnName := cudn.Name
	cudn, err = c.udnClient.K8sV1().ClusterUserDefinedNetworks().ApplyStatus(context.Background(), applyConf, opts)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to update ClusterUserDefinedNetwork status %q: %w", cudnName, err)
	}
	klog.Infof("Updated status ClusterUserDefinedNetwork %q", cudn.Name)

	return nil
}

func newClusterNetworkCreatedCondition(nads []netv1.NetworkAttachmentDefinition, syncError error) metav1.Condition {
	var namespaces []string
	for _, nad := range nads {
		namespaces = append(namespaces, nad.Namespace)
	}
	affectedNamespaces := strings.Join(namespaces, ", ")

	now := metav1.Now()
	condition := metav1.Condition{
		Type:               conditionTypeNetworkCreated,
		Status:             metav1.ConditionTrue,
		Reason:             reasonNADCreated,
		Message:            fmt.Sprintf("NetworkAttachmentDefinition has been created in following namespaces: [%s]", affectedNamespaces),
		LastTransitionTime: now,
	}

	var deletedNadKeys []string
	for _, nad := range nads {
		if nad.DeletionTimestamp != nil {
			deletedNadKeys = append(deletedNadKeys, nad.Namespace+"/"+nad.Name)
		}
	}
	if len(deletedNadKeys) > 0 {
		condition.Status = metav1.ConditionFalse
		condition.Reason = reasonNADDeleted
		condition.Message = fmt.Sprintf("NetworkAttachmentDefinition are being deleted: %v", deletedNadKeys)
	}

	if syncError != nil {
		condition.Status = metav1.ConditionFalse

		// Check for specific error types to provide better status reasons
		var vtepNotFound *vtepNotFoundError
		if errors.As(syncError, &vtepNotFound) {
			condition.Reason = reasonVTEPNotFound
			condition.Message = fmt.Sprintf("Cannot create network: VTEP '%s' does not exist. "+
				"Create the VTEP CR first or update the CUDN to reference an existing VTEP.",
				vtepNotFound.vtepName)
		} else {
			condition.Reason = reasonNADSyncError
			condition.Message = syncError.Error()
		}
	}

	return condition
}

// validateEVPN validates EVPN configuration for a CUDN.
// Returns an error if EVPN is requested but disabled, or if the referenced VTEP doesn't exist.
func (c *Controller) validateEVPN(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork) error {
	if cudn.Spec.Network.Transport != userdefinednetworkv1.TransportOptionEVPN {
		return nil // Not an EVPN network
	}

	if !config.OVNKubernetesFeature.EnableEVPN {
		return fmt.Errorf("EVPN transport requested but EVPN feature is not enabled")
	}
	if config.Gateway.Mode != config.GatewayModeLocal {
		return fmt.Errorf("EVPN transport requested but EVPN feature is only supported in local gateway mode")
	}

	// CEL validation ensures EVPN is set when transport is EVPN.
	vtepName := cudn.Spec.Network.EVPN.VTEP
	_, err := c.vtepLister.Get(vtepName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return &vtepNotFoundError{vtepName: vtepName}
		}
		return fmt.Errorf("failed to get VTEP %q: %w", vtepName, err)
	}

	return nil
}

// ReconcileVTEP handles VTEP events by re-queuing all CUDNs that reference the VTEP.
//
// This uses O(n) iteration over all CUDNs rather than maintaining an index because:
// VTEP create/delete events are expected to be rare; scanning all CUDNs from the
// informer cache keeps the logic simple. If this becomes a hot path at large
// CUDN counts, add an informer indexer keyed by VTEP.
func (c *Controller) ReconcileVTEP(vtepName string) error {
	cudns, err := c.cudnLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list CUDNs: %w", err)
	}

	for _, cudn := range cudns {
		if cudnReferencesVTEP(cudn, vtepName) {
			klog.V(4).InfoS("Re-queueing CUDN following VTEP event", "cudn", cudn.Name, "vtep", vtepName)
			c.cudnController.Reconcile(cudn.Name)
		}
	}

	return nil
}

// cudnReferencesVTEP returns true if the CUDN is an EVPN network referencing the given VTEP.
// CEL validation ensures EVPN is set when transport is EVPN.
func cudnReferencesVTEP(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork, vtepName string) bool {
	if cudn.Spec.Network.Transport != userdefinednetworkv1.TransportOptionEVPN {
		return false
	}
	return cudn.Spec.Network.EVPN.VTEP == vtepName
}
