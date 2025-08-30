package virtualprivatenetworkconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/id"
	controllerutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
	udnlisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	vpnctypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/virtualprivatenetworkconnect/v1"
	vpncclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/virtualprivatenetworkconnect/v1/apis/clientset/versioned"
	vpnclisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/virtualprivatenetworkconnect/v1/apis/listers/virtualprivatenetworkconnect/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// VPNCNodeNetworkSubnetsAnnotation is the annotation key for storing node-network subnet allocations
	VPNCNodeNetworkSubnetsAnnotation = "k8s.ovn.org/vpc-node-network-subnets"
	// VPNCNodeTunnelKeysAnnotation is the annotation key for storing per-node tunnel keys
	VPNCNodeTunnelKeysAnnotation = "k8s.ovn.org/vpnc-node-tunnel-keys"

	// VPNCTunnelKeyStartID is the starting ID for VPNC tunnel key allocation (after network IDs)
	VPNCTunnelKeyStartID = 5000
	// VPNCTunnelKeyMaxID is the maximum ID for VPNC tunnel key allocation
	VPNCTunnelKeyMaxID = 10000
)

// DiscoveredNetwork represents a network discovered from network selectors
type DiscoveredNetwork struct {
	NetworkName  string
	NetworkID    int
	NADName      string
	NADNamespace string
}

// Controller manages VirtualPrivateNetworkConnect objects
type Controller struct {
	// vpncController is the controller used to handle VirtualPrivateNetworkConnect events
	vpncController controllerutil.Controller

	// nodeController watches for node events
	nodeController controllerutil.Controller

	// wf is the watch factory for accessing informers
	wf *factory.WatchFactory

	// vpncClient is used to interact with VirtualPrivateNetworkConnect CRDs
	vpncClient vpncclientset.Interface

	// vpncLister lists VirtualPrivateNetworkConnect objects
	vpncLister vpnclisters.VirtualPrivateNetworkConnectLister

	// networkManager provides network management capabilities
	nm networkmanager.Interface

	// nodeLister for listing nodes
	nodeLister corelisters.NodeLister

	// nadLister for listing network attachment definitions
	nadLister nadlisters.NetworkAttachmentDefinitionLister

	// udnLister for listing user defined networks
	udnLister udnlisters.UserDefinedNetworkLister

	// cudnLister for listing cluster user defined networks
	cudnLister udnlisters.ClusterUserDefinedNetworkLister

	// subnetAllocators tracks subnet allocators per VPNC object
	subnetAllocators      map[string]VPNCSubnetAllocator
	subnetAllocatorsMutex sync.RWMutex

	// tunnelKeyAllocator allocates global tunnel keys for VPNC interconnect transit routers
	tunnelKeyAllocator id.Allocator
}

// NewController builds a controller that reconciles VirtualPrivateNetworkConnect objects
func NewController(
	nm networkmanager.Interface,
	wf *factory.WatchFactory,
	ovnClient *util.OVNClusterManagerClientset,
) *Controller {
	// Create a SINGLE GLOBAL tunnel key allocator for VPNC interconnect transit routers
	// This allocator is shared by ALL VPNCs to ensure globally unique tunnel keys
	// across the entire cluster (required by OVN)
	// Allocate tunnel keys in the range 5000-10000 (after network IDs, up to 10K)
	tunnelKeyAllocator := id.NewIDAllocator("vpnc-tunnelkey", VPNCTunnelKeyMaxID)
	// Reserve IDs 0-4999 (used by network IDs and other allocations)
	for i := 0; i < VPNCTunnelKeyStartID; i++ {
		if err := tunnelKeyAllocator.ReserveID(fmt.Sprintf("reserved-%d", i), i); err != nil {
			klog.Errorf("Failed to reserve tunnel key ID %d: %v", i, err)
		}
	}

	c := &Controller{
		wf:                    wf,
		vpncClient:            ovnClient.VirtualPrivateNetworkConnectClient,
		vpncLister:            wf.VirtualPrivateNetworkConnectInformer().Lister(),
		nm:                    nm,
		nodeLister:            wf.NodeCoreInformer().Lister(),
		nadLister:             wf.NADInformer().Lister(),
		udnLister:             wf.UserDefinedNetworkInformer().Lister(),
		cudnLister:            wf.ClusterUserDefinedNetworkInformer().Lister(),
		subnetAllocators:      make(map[string]VPNCSubnetAllocator),
		subnetAllocatorsMutex: sync.RWMutex{},
		tunnelKeyAllocator:    tunnelKeyAllocator,
	}

	handleError := func(key string, errorstatus error) error {
		vpnc, err := c.vpncLister.Get(key)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("cannot get VirtualPrivateNetworkConnect %q to report error %v in status: %v",
				key,
				errorstatus,
				err,
			)
		}

		return c.updateVirtualPrivateNetworkConnectStatus(vpnc, "Error", errorstatus.Error())
	}

	vpncConfig := &controllerutil.ControllerConfig[vpnctypes.VirtualPrivateNetworkConnect]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcile,
		Threadiness:    1,
		Informer:       wf.VirtualPrivateNetworkConnectInformer().Informer(),
		Lister:         wf.VirtualPrivateNetworkConnectInformer().Lister().List,
		ObjNeedsUpdate: vpncNeedsUpdate,
		HandleError:    handleError,
	}

	c.vpncController = controllerutil.NewController("virtualprivatenetworkconnect-controller", vpncConfig)

	// Node controller to watch for node changes
	nodeConfig := &controllerutil.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileNode,
		Threadiness:    1,
		Informer:       wf.NodeCoreInformer().Informer(),
		Lister:         wf.NodeCoreInformer().Lister().List,
		ObjNeedsUpdate: nodeNeedsUpdate,
	}

	c.nodeController = controllerutil.NewController("virtualprivatenetworkconnect-node-controller", nodeConfig)

	return c
}

// Start starts the controller
func (c *Controller) Start() error {
	klog.Info("Starting VirtualPrivateNetworkConnect controller")
	return controllerutil.Start(c.vpncController, c.nodeController)
}

// Stop stops the controller
func (c *Controller) Stop() {
	klog.Info("Stopping VirtualPrivateNetworkConnect controller")
	controllerutil.Stop(c.vpncController, c.nodeController)
}

// vpncNeedsUpdate determines if a VirtualPrivateNetworkConnect needs to be reconciled
func vpncNeedsUpdate(oldObj, newObj *vpnctypes.VirtualPrivateNetworkConnect) bool {
	return oldObj == nil || newObj == nil || oldObj.Generation != newObj.Generation
}

// nodeNeedsUpdate determines if a node change should trigger reconciliation
func nodeNeedsUpdate(oldObj, newObj *corev1.Node) bool {
	// Reconcile on node addition or removal, or when node readiness changes
	return true // For now, reconcile on any node change
}

// reconcile is the main reconcile function for VirtualPrivateNetworkConnect objects
func (c *Controller) reconcile(key string) error {
	startTime := time.Now()
	klog.V(4).Infof("Processing VirtualPrivateNetworkConnect %s", key)
	defer func() {
		klog.V(4).Infof("Finished processing VirtualPrivateNetworkConnect %s (took %v)", key, time.Since(startTime))
	}()

	vpnc, err := c.vpncLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(2).Infof("VirtualPrivateNetworkConnect %s not found, likely deleted", key)
			return c.handleVirtualPrivateNetworkConnectDeletion(key)
		}
		return fmt.Errorf("failed to get VirtualPrivateNetworkConnect %s: %w", key, err)
	}

	return c.processVirtualPrivateNetworkConnect(vpnc)
}

// reconcileNode handles node events that may affect VirtualPrivateNetworkConnect subnet allocation
func (c *Controller) reconcileNode(key string) error {
	klog.V(4).Infof("Processing node %s for VirtualPrivateNetworkConnect updates", key)

	// When nodes change, we need to reconcile all VirtualPrivateNetworkConnect objects
	// to ensure proper subnet allocation
	vpncs, err := c.vpncLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list VirtualPrivateNetworkConnect objects: %w", err)
	}

	for _, vpnc := range vpncs {
		if err := c.processVirtualPrivateNetworkConnect(vpnc); err != nil {
			klog.Errorf("Failed to process VirtualPrivateNetworkConnect %s after node %s change: %v", vpnc.Name, key, err)
			// Continue processing other VPNC objects even if one fails
		}
	}

	return nil
}

// processVirtualPrivateNetworkConnect processes a VirtualPrivateNetworkConnect object
func (c *Controller) processVirtualPrivateNetworkConnect(vpnc *vpnctypes.VirtualPrivateNetworkConnect) error {
	klog.V(2).Infof("Processing VirtualPrivateNetworkConnect %s with %d network selectors and connect subnets %v",
		vpnc.Name, len(vpnc.Spec.NetworkSelectors), vpnc.Spec.ConnectSubnets)

	// 1. Validate the VirtualPrivateNetworkConnect object
	if err := c.validateVPNC(vpnc); err != nil {
		klog.Errorf("VirtualPrivateNetworkConnect %s validation failed: %v", vpnc.Name, err)
		return c.updateVirtualPrivateNetworkConnectStatus(vpnc, "Error", fmt.Sprintf("Validation failed: %v", err))
	}

	// 2. Discover networks matching the selectors
	discoveredNetworks, err := c.discoverNetworks(vpnc)
	if err != nil {
		klog.Errorf("Failed to discover networks for VirtualPrivateNetworkConnect %s: %v", vpnc.Name, err)
		return c.updateVirtualPrivateNetworkConnectStatus(vpnc, "Error", fmt.Sprintf("Network discovery failed: %v", err))
	}

	if len(discoveredNetworks) == 0 {
		klog.V(2).Infof("No matching networks found for VirtualPrivateNetworkConnect %s", vpnc.Name)
		return c.updateVirtualPrivateNetworkConnectStatus(vpnc, "Warning", "No networks found matching the selectors")
	}

	// 3. Get ready nodes
	nodes, err := c.getReadyNodes()
	if err != nil {
		klog.Errorf("Failed to get ready nodes for VirtualPrivateNetworkConnect %s: %v", vpnc.Name, err)
		return c.updateVirtualPrivateNetworkConnectStatus(vpnc, "Error", fmt.Sprintf("Failed to get nodes: %v", err))
	}

	if len(nodes) == 0 {
		klog.V(2).Infof("No ready nodes found for VirtualPrivateNetworkConnect %s", vpnc.Name)
		return c.updateVirtualPrivateNetworkConnectStatus(vpnc, "Warning", "No ready nodes found")
	}

	// 4. Allocate /31 point-to-point subnets for each node-network pair
	allocations, err := c.allocateSubnets(vpnc, nodes, discoveredNetworks)
	if err != nil {
		klog.Errorf("Failed to allocate subnets for VirtualPrivateNetworkConnect %s: %v", vpnc.Name, err)
		return c.updateVirtualPrivateNetworkConnectStatus(vpnc, "Error", fmt.Sprintf("Subnet allocation failed: %v", err))
	}

	// 5. Allocate tunnel keys for each node (one per VPNC-node pair)
	tunnelKeyAllocations, err := c.allocateTunnelKeys(vpnc, nodes)
	if err != nil {
		klog.Errorf("Failed to allocate tunnel keys for VirtualPrivateNetworkConnect %s: %v", vpnc.Name, err)
		return c.updateVirtualPrivateNetworkConnectStatus(vpnc, "Error", fmt.Sprintf("Tunnel key allocation failed: %v", err))
	}

	// 6. Update annotations on the VirtualPrivateNetworkConnect object
	if err := c.updateVPNCAnnotations(vpnc, allocations, tunnelKeyAllocations); err != nil {
		klog.Errorf("Failed to update annotations for VirtualPrivateNetworkConnect %s: %v", vpnc.Name, err)
		return c.updateVirtualPrivateNetworkConnectStatus(vpnc, "Error", fmt.Sprintf("Failed to update annotations: %v", err))
	}

	// 7. Update status conditions
	klog.V(2).Infof("Successfully processed VirtualPrivateNetworkConnect %s: allocated subnets for %d node-network pairs and tunnel keys for %d nodes", vpnc.Name, len(allocations), len(tunnelKeyAllocations))
	return c.updateVirtualPrivateNetworkConnectStatus(vpnc, "Ready", fmt.Sprintf("Allocated subnets for %d node-network pairs and tunnel keys for %d nodes", len(allocations), len(tunnelKeyAllocations)))
}

// handleVirtualPrivateNetworkConnectDeletion handles the deletion of a VirtualPrivateNetworkConnect
func (c *Controller) handleVirtualPrivateNetworkConnectDeletion(name string) error {
	klog.V(2).Infof("Handling deletion of VirtualPrivateNetworkConnect %s", name)

	// Clean up subnet allocator for this VPNC
	c.subnetAllocatorsMutex.Lock()
	defer c.subnetAllocatorsMutex.Unlock()

	if _, exists := c.subnetAllocators[name]; exists {
		delete(c.subnetAllocators, name)
		klog.V(4).Infof("Cleaned up subnet allocator for VirtualPrivateNetworkConnect %s", name)
	}

	// Release tunnel keys allocated for this VPNC
	// We need to iterate through all nodes and release tunnel keys for this VPNC
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list nodes during VPNC %s deletion: %v", name, err)
		// Continue with cleanup even if node listing fails
	} else {
		for _, node := range nodes {
			owner := fmt.Sprintf("%s-%s", name, node.Name)
			c.tunnelKeyAllocator.ReleaseID(owner)
			klog.V(4).Infof("Released tunnel key for node %s in VPNC %s", node.Name, name)
		}
	}

	return nil
}

// updateVirtualPrivateNetworkConnectStatus updates the status of a VirtualPrivateNetworkConnect
func (c *Controller) updateVirtualPrivateNetworkConnectStatus(vpnc *vpnctypes.VirtualPrivateNetworkConnect, conditionType, message string) error {
	// Create a copy to modify
	vpncCopy := vpnc.DeepCopy()

	// Update the condition
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             conditionType,
		Message:            message,
	}

	// Find and update existing condition or add new one
	conditionUpdated := false
	for i, existingCondition := range vpncCopy.Status.Conditions {
		if existingCondition.Type == conditionType {
			vpncCopy.Status.Conditions[i] = condition
			conditionUpdated = true
			break
		}
	}
	if !conditionUpdated {
		vpncCopy.Status.Conditions = append(vpncCopy.Status.Conditions, condition)
	}

	// Update the status
	_, err := c.vpncClient.K8sV1().VirtualPrivateNetworkConnects().UpdateStatus(
		context.TODO(),
		vpncCopy,
		metav1.UpdateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to update status for VirtualPrivateNetworkConnect %s: %w", vpnc.Name, err)
	}

	klog.V(4).Infof("Updated status for VirtualPrivateNetworkConnect %s", vpnc.Name)
	return nil
}

// validateVPNC validates the VirtualPrivateNetworkConnect object
func (c *Controller) validateVPNC(vpnc *vpnctypes.VirtualPrivateNetworkConnect) error {
	if len(vpnc.Spec.NetworkSelectors) == 0 {
		return fmt.Errorf("networkSelectors cannot be empty")
	}

	if len(vpnc.Spec.ConnectSubnets) == 0 {
		return fmt.Errorf("connectSubnets cannot be empty")
	}

	// Validate connect subnets are valid CIDR blocks
	for _, subnetCIDR := range vpnc.Spec.ConnectSubnets {
		if _, _, err := net.ParseCIDR(string(subnetCIDR)); err != nil {
			return fmt.Errorf("invalid connect subnet %q: %w", string(subnetCIDR), err)
		}
	}

	return nil
}

// discoverNetworks discovers networks matching the network selectors
func (c *Controller) discoverNetworks(vpnc *vpnctypes.VirtualPrivateNetworkConnect) ([]DiscoveredNetwork, error) {
	var discoveredNetworks []DiscoveredNetwork

	for _, selector := range vpnc.Spec.NetworkSelectors {
		networks, err := c.discoverNetworksFromSelector(selector)
		if err != nil {
			return nil, fmt.Errorf("failed to discover networks from selector: %w", err)
		}
		discoveredNetworks = append(discoveredNetworks, networks...)
	}

	klog.V(4).Infof("Discovered %d networks for VirtualPrivateNetworkConnect %s", len(discoveredNetworks), vpnc.Name)
	return discoveredNetworks, nil
}

// discoverNetworksFromSelector discovers networks from a single network selector
func (c *Controller) discoverNetworksFromSelector(selector types.NetworkSelector) ([]DiscoveredNetwork, error) {
	var discoveredNetworks []DiscoveredNetwork

	switch selector.NetworkSelectionType {
	case types.ClusterUserDefinedNetworks:
		if selector.ClusterUserDefinedNetworkSelector == nil {
			return nil, fmt.Errorf("clusterUserDefinedNetworkSelector cannot be nil for ClusterUserDefinedNetworks")
		}

		labelSelector, err := metav1.LabelSelectorAsSelector(&selector.ClusterUserDefinedNetworkSelector.NetworkSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid cluster network selector: %w", err)
		}

		cudns, err := c.cudnLister.List(labelSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to list ClusterUserDefinedNetworks: %w", err)
		}

		for _, cudn := range cudns {
			// Generate the network name that would be used in node annotations
			networkName := util.GenerateCUDNNetworkName(cudn.Name)

			// CUDNs create NADs in each selected namespace, but we need to discover which namespaces
			// For now, we'll need to find the NADs created by this CUDN across all namespaces
			nads, err := c.nadLister.List(labels.Everything())
			if err != nil {
				klog.Warningf("Failed to list NADs for CUDN %s: %v", cudn.Name, err)
				continue
			}

			// Find NADs owned by this CUDN
			for _, nad := range nads {
				if nad.Annotations != nil && nad.Annotations[ovntypes.OvnNetworkNameAnnotation] == networkName {
					discovered := DiscoveredNetwork{
						NetworkName:  networkName,
						NetworkID:    0, // Will be populated from NAD annotation
						NADName:      nad.Name,
						NADNamespace: nad.Namespace,
					}
					discoveredNetworks = append(discoveredNetworks, discovered)
				}
			}
		}

	case types.PrimaryUserDefinedNetworks:
		if selector.PrimaryUserDefinedNetworkSelector == nil {
			return nil, fmt.Errorf("primaryUserDefinedNetworkSelector cannot be nil for PrimaryUserDefinedNetworks")
		}

		namespaceSelector, err := metav1.LabelSelectorAsSelector(&selector.PrimaryUserDefinedNetworkSelector.NamespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid namespace selector: %w", err)
		}

		// Get matching namespaces
		namespaces, err := c.wf.NamespaceInformer().Lister().List(namespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to list namespaces: %w", err)
		}

		// For primary UDNs, we look for them in each matching namespace
		for _, ns := range namespaces {
			udns, err := c.udnLister.UserDefinedNetworks(ns.Name).List(labels.Everything())
			if err != nil {
				klog.Warningf("Failed to list UDNs in namespace %s: %v", ns.Name, err)
				continue
			}

			for _, udn := range udns {
				// Generate the network name that would be used in node annotations
				networkName := util.GenerateUDNNetworkName(udn.Namespace, udn.Name)

				// UDNs create a corresponding NAD in the same namespace with the same name
				discovered := DiscoveredNetwork{
					NetworkName:  networkName,
					NetworkID:    0,             // Will be populated from NAD annotation
					NADName:      udn.Name,      // UDN creates NAD with same name
					NADNamespace: udn.Namespace, // In same namespace as UDN
				}
				discoveredNetworks = append(discoveredNetworks, discovered)
			}
		}

	case types.NetworkAttachmentDefinitions:
		if selector.NetworkAttachmentDefinitionSelector == nil {
			return nil, fmt.Errorf("networkAttachmentDefinitionSelector cannot be nil for NetworkAttachmentDefinitions")
		}

		namespaceSelector, err := metav1.LabelSelectorAsSelector(&selector.NetworkAttachmentDefinitionSelector.NamespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid namespace selector: %w", err)
		}

		networkSelector, err := metav1.LabelSelectorAsSelector(&selector.NetworkAttachmentDefinitionSelector.NetworkSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid network selector: %w", err)
		}

		// Get matching namespaces
		namespaces, err := c.wf.NamespaceInformer().Lister().List(namespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to list namespaces: %w", err)
		}

		// Get NADs from matching namespaces
		for _, ns := range namespaces {
			nads, err := c.nadLister.NetworkAttachmentDefinitions(ns.Name).List(networkSelector)
			if err != nil {
				klog.Warningf("Failed to list NADs in namespace %s: %v", ns.Name, err)
				continue
			}

			for _, nad := range nads {
				// Only include NADs with role:primary annotation
				if nad.Annotations == nil || nad.Annotations["k8s.ovn.org/role"] != "primary" {
					continue
				}

				// For NADs, get the network name from the annotation if available
				// This should match what's used in nad annotations for network IDs
				networkName := nad.Name
				if nad.Annotations != nil && nad.Annotations[ovntypes.OvnNetworkNameAnnotation] != "" {
					networkName = nad.Annotations[ovntypes.OvnNetworkNameAnnotation]
				}

				discovered := DiscoveredNetwork{
					NetworkName:  networkName, // Network name for annotation lookup
					NetworkID:    0,           // Will be populated from NAD annotation
					NADName:      nad.Name,
					NADNamespace: nad.Namespace,
				}
				discoveredNetworks = append(discoveredNetworks, discovered)
			}
		}

	default:
		return nil, fmt.Errorf("unsupported network selection type: %s", selector.NetworkSelectionType)
	}

	return discoveredNetworks, nil
}

// getReadyNodes returns all ready nodes in the cluster
func (c *Controller) getReadyNodes() ([]*corev1.Node, error) {
	allNodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	var readyNodes []*corev1.Node
	for _, node := range allNodes {
		if c.isNodeReady(node) {
			readyNodes = append(readyNodes, node)
		}
	}

	klog.V(4).Infof("Found %d ready nodes out of %d total nodes", len(readyNodes), len(allNodes))
	return readyNodes, nil
}

// isNodeReady checks if a node is ready
func (c *Controller) isNodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// allocateSubnets allocates /31 subnets for each node-network pair
func (c *Controller) allocateSubnets(vpnc *vpnctypes.VirtualPrivateNetworkConnect, nodes []*corev1.Node, networks []DiscoveredNetwork) (map[string]*net.IPNet, error) {
	// Get or create subnet allocator for this VPNC
	allocator, err := c.getOrCreateSubnetAllocator(vpnc)
	if err != nil {
		return nil, fmt.Errorf("failed to get subnet allocator: %w", err)
	}

	allocations := make(map[string]*net.IPNet)

	// Allocate /31 subnet for each node-network pair
	for _, node := range nodes {
		for _, network := range networks {
			// All networks (UDNs, CUDNs, NADs) have corresponding NADs - get network ID from NAD annotation
			nad, nadErr := c.nadLister.NetworkAttachmentDefinitions(network.NADNamespace).Get(network.NADName)
			if nadErr != nil {
				klog.Warningf("Failed to get NAD %s/%s for network %s: %v", network.NADNamespace, network.NADName, network.NetworkName, nadErr)
				continue
			}

			if nad.Annotations == nil || nad.Annotations[ovntypes.OvnNetworkIDAnnotation] == "" {
				klog.V(2).Infof("Network ID annotation not set for NAD %s/%s (network %s), skipping", network.NADNamespace, network.NADName, network.NetworkName)
				continue
			}

			networkID, err := strconv.Atoi(nad.Annotations[ovntypes.OvnNetworkIDAnnotation])
			if err != nil {
				klog.Warningf("Failed to parse network ID from NAD %s/%s: %v", network.NADNamespace, network.NADName, err)
				continue
			}

			// Skip invalid network IDs
			if networkID == ovntypes.InvalidID {
				klog.V(2).Infof("Invalid network ID for network %s on node %s, skipping", network.NetworkName, node.Name)
				continue
			}

			owner := fmt.Sprintf("%s-Network%d", node.Name, networkID)

			subnet, err := allocator.AllocateSubnet(owner)
			if err != nil {
				return nil, fmt.Errorf("failed to allocate subnet for %s: %w", owner, err)
			}

			allocations[owner] = subnet
			klog.V(4).Infof("Allocated subnet %s for %s (network: %s)", subnet, owner, network.NetworkName)
		}
	}

	return allocations, nil
}

// getOrCreateSubnetAllocator gets or creates a subnet allocator for the given VPNC
func (c *Controller) getOrCreateSubnetAllocator(vpnc *vpnctypes.VirtualPrivateNetworkConnect) (VPNCSubnetAllocator, error) {
	c.subnetAllocatorsMutex.Lock()
	defer c.subnetAllocatorsMutex.Unlock()

	allocator, exists := c.subnetAllocators[vpnc.Name]
	if !exists {
		allocator = NewVPNCPointToPointAllocator()
		c.subnetAllocators[vpnc.Name] = allocator
	}

	// Add connect subnets to the allocator
	for _, subnetCIDR := range vpnc.Spec.ConnectSubnets {
		_, subnet, err := net.ParseCIDR(string(subnetCIDR))
		if err != nil {
			return nil, fmt.Errorf("invalid connect subnet %q: %w", string(subnetCIDR), err)
		}

		if err := allocator.AddConnectSubnet(subnet); err != nil {
			return nil, fmt.Errorf("failed to add connect subnet %s: %w", string(subnetCIDR), err)
		}
	}

	return allocator, nil
}

// allocateTunnelKeys allocates tunnel keys for each node
// Tunnel keys are GLOBALLY UNIQUE across all VPNCs and nodes in the cluster.
// This is achieved by using a single shared tunnelKeyAllocator for all VPNCs,
// with unique owner strings per VPNC-node combination.
func (c *Controller) allocateTunnelKeys(vpnc *vpnctypes.VirtualPrivateNetworkConnect, nodes []*corev1.Node) (map[string]int, error) {
	tunnelKeyAllocations := make(map[string]int)

	// Allocate one tunnel key per node for this VPNC
	// Each VPNC-node pair gets a globally unique tunnel key
	for _, node := range nodes {
		// Create unique owner string: VPNC1-node1, VPNC2-node1, etc.
		// This ensures tunnel keys are unique across ALL VPNCs and nodes
		owner := fmt.Sprintf("%s-%s", vpnc.Name, node.Name)

		tunnelKey, err := c.tunnelKeyAllocator.AllocateID(owner)
		if err != nil {
			return nil, fmt.Errorf("failed to allocate tunnel key for node %s in VPNC %s: %w", node.Name, vpnc.Name, err)
		}

		tunnelKeyAllocations[node.Name] = tunnelKey
		klog.V(4).Infof("Allocated globally unique tunnel key %d for node %s in VPNC %s", tunnelKey, node.Name, vpnc.Name)
	}

	return tunnelKeyAllocations, nil
}

// updateVPNCAnnotations updates the VPNC annotations with subnet and tunnel key allocations
func (c *Controller) updateVPNCAnnotations(vpnc *vpnctypes.VirtualPrivateNetworkConnect, allocations map[string]*net.IPNet, tunnelKeyAllocations map[string]int) error {
	// Create the subnet allocation map
	subnetMap := make(map[string]string)
	for owner, subnet := range allocations {
		subnetMap[owner] = subnet.String()
	}

	// Marshal subnet allocations to JSON
	subnetJSON, err := json.Marshal(subnetMap)
	if err != nil {
		return fmt.Errorf("failed to marshal subnet allocations: %w", err)
	}

	// Marshal tunnel key allocations to JSON
	tunnelKeyJSON, err := json.Marshal(tunnelKeyAllocations)
	if err != nil {
		return fmt.Errorf("failed to marshal tunnel key allocations: %w", err)
	}

	// Create a copy to modify
	vpncCopy := vpnc.DeepCopy()
	if vpncCopy.Annotations == nil {
		vpncCopy.Annotations = make(map[string]string)
	}

	vpncCopy.Annotations[VPNCNodeNetworkSubnetsAnnotation] = string(subnetJSON)
	vpncCopy.Annotations[VPNCNodeTunnelKeysAnnotation] = string(tunnelKeyJSON)

	// Update the object
	_, err = c.vpncClient.K8sV1().VirtualPrivateNetworkConnects().Update(
		context.TODO(),
		vpncCopy,
		metav1.UpdateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to update VirtualPrivateNetworkConnect %s annotations: %w", vpnc.Name, err)
	}

	klog.V(4).Infof("Updated annotations for VirtualPrivateNetworkConnect %s with %d subnet allocations and %d tunnel key allocations", vpnc.Name, len(allocations), len(tunnelKeyAllocations))
	return nil
}
