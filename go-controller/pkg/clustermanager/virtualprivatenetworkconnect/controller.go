package virtualprivatenetworkconnect

import (
	"context"
	"fmt"
	"net"
	"time"

	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

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
}

// NewController builds a controller that reconciles VirtualPrivateNetworkConnect objects
func NewController(
	nm networkmanager.Interface,
	wf *factory.WatchFactory,
	ovnClient *util.OVNClusterManagerClientset,
) *Controller {
	c := &Controller{
		wf:         wf,
		vpncClient: ovnClient.VirtualPrivateNetworkConnectClient,
		vpncLister: wf.VirtualPrivateNetworkConnectInformer().Lister(),
		nm:         nm,
		nodeLister: wf.NodeCoreInformer().Lister(),
		nadLister:  wf.NADInformer().Lister(),
		udnLister:  wf.UserDefinedNetworkInformer().Lister(),
		cudnLister: wf.ClusterUserDefinedNetworkInformer().Lister(),
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

	// 4. Update status conditions
	klog.V(2).Infof("Successfully processed VirtualPrivateNetworkConnect %s: discovered %d networks and %d nodes", vpnc.Name, len(discoveredNetworks), len(nodes))
	return c.updateVirtualPrivateNetworkConnectStatus(vpnc, "Ready", fmt.Sprintf("Discovered %d networks and %d nodes", len(discoveredNetworks), len(nodes)))
}

// handleVirtualPrivateNetworkConnectDeletion handles the deletion of a VirtualPrivateNetworkConnect
func (c *Controller) handleVirtualPrivateNetworkConnectDeletion(name string) error {
	klog.V(2).Infof("Handling deletion of VirtualPrivateNetworkConnect %s", name)
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