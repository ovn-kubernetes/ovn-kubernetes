package virtualprivatenetworkconnect

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	vpnctypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/virtualprivatenetworkconnect/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	controllerName = "vpnc-ovn-controller"
	maxRetries     = 15

	// VPNCNodeNetworkSubnetsAnnotation is the annotation key for storing node-network subnet allocations
	VPNCNodeNetworkSubnetsAnnotation = "k8s.ovn.org/vpc-node-network-subnets"
	// VPNCNodeTunnelKeysAnnotation is the annotation key for storing per-node tunnel keys
	VPNCNodeTunnelKeysAnnotation = "k8s.ovn.org/vpnc-node-tunnel-keys"

	// InterconnectRouterPrefix is the prefix for interconnect transit router names
	InterconnectRouterPrefix = "vpnc-connect-router-"

	// VPNCRoutingPolicyPriority is the priority for VPNC interconnect routing policies
	// Set to high priority (9001) to ensure VPNC traffic is routed correctly
	VPNCRoutingPolicyPriority = 9001

	// PointToPointSubnetMask is the subnet mask used for point-to-point links between routers
	// /31 subnets provide exactly 2 usable IPs for point-to-point connections
	PointToPointSubnetMask = 31
	PointToPointSubnetBits = 32 // IPv4 has 32 bits total
)

// Controller manages OVN resources for VirtualPrivateNetworkConnect objects
type Controller struct {
	// libovsdb northbound client interface
	nbClient libovsdbclient.Client

	// watch factory for informers
	wf *factory.WatchFactory

	// node lister
	nodeLister corelisters.NodeLister

	// NAD lister for network information
	nadLister nadlisters.NetworkAttachmentDefinitionLister

	// work queue for processing VPNC events
	queue workqueue.TypedRateLimitingInterface[string]

	// vpncCacheLock protects the vpncCache
	vpncCacheLock sync.RWMutex
	// vpncCache stores current VPNC configurations
	vpncCache map[string]*vpncConfig

	// workerLoopPeriod determines the time between worker loop runs
	workerLoopPeriod time.Duration

	// zone is the name of the zone (node name) this controller is running on
	zone string
}

// vpncConfig stores the processed configuration for a VPNC
type vpncConfig struct {
	Name                   string
	SubnetAllocations      map[string]*net.IPNet // node-network -> subnet
	TunnelKeyAllocations   map[string]int        // node -> tunnel key
	InterconnectRouterName string                // name of the interconnect transit router
	TunnelKey              int                   // tunnel key for this zone's interconnect router
	CreatedOVNResources    []string              // track created resources for cleanup
}

// NewController creates a new VPNC OVN controller
func NewController(
	nbClient libovsdbclient.Client,
	wf *factory.WatchFactory,
	zone string,
) *Controller {
	klog.Infof("Creating VirtualPrivateNetworkConnect OVN controller for zone %s", zone)

	c := &Controller{
		nbClient:         nbClient,
		wf:               wf,
		nodeLister:       wf.NodeCoreInformer().Lister(),
		nadLister:        wf.NADInformer().Lister(),
		queue:            workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		vpncCache:        make(map[string]*vpncConfig),
		workerLoopPeriod: time.Second,
		zone:             zone,
	}

	// Set up informer event handlers
	_, err := wf.VirtualPrivateNetworkConnectInformer().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onVPNCAdd,
		UpdateFunc: c.onVPNCUpdate,
		DeleteFunc: c.onVPNCDelete,
	})
	if err != nil {
		klog.Errorf("Failed to add VPNC informer event handlers: %v", err)
	}

	return c
}

// Start begins the controller workers
func (c *Controller) Start(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting %s with %d workers", controllerName, workers)

	// Wait for caches to sync
	if !cache.WaitForCacheSync(stopCh,
		c.wf.VirtualPrivateNetworkConnectInformer().Informer().HasSynced,
		c.wf.NodeCoreInformer().Informer().HasSynced,
	) {
		utilruntime.HandleError(fmt.Errorf("failed to sync caches for %s", controllerName))
		return
	}

	// Start worker goroutines
	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, c.workerLoopPeriod, stopCh)
	}

	klog.Infof("%s started", controllerName)
	<-stopCh
	klog.Infof("Shutting down %s", controllerName)
}

// Stop shuts down the controller
func (c *Controller) Stop() {
	klog.Infof("Shutting down %s", controllerName)
	c.queue.ShutDown()
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the workqueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.queue.Get()
	if shutdown {
		return false
	}

	defer c.queue.Done(obj)

	key := obj
	if key == "" {
		c.queue.Forget(obj)
		utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
		return true
	}

	if err := c.syncVPNC(key); err != nil {
		c.handleErr(err, key)
		return true
	}

	c.queue.Forget(obj)
	return true
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *Controller) handleErr(err error, key string) {
	if c.queue.NumRequeues(key) < maxRetries {
		klog.Warningf("Error syncing VPNC %v: %v", key, err)
		c.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	klog.Warningf("Dropping VPNC %q out of the queue: %v", key, err)
	c.queue.Forget(key)
}

// Event handler functions
func (c *Controller) onVPNCAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding VPNC: %s", key)
	c.queue.Add(key)
}

func (c *Controller) onVPNCUpdate(oldObj, newObj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", newObj, err))
		return
	}
	klog.V(4).Infof("Updating VPNC: %s", key)
	c.queue.Add(key)
}

func (c *Controller) onVPNCDelete(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Deleting VPNC: %s", key)
	c.queue.Add(key)
}

// syncVPNC processes a single VPNC object
func (c *Controller) syncVPNC(key string) error {
	startTime := time.Now()
	klog.V(4).Infof("Processing VPNC %s", key)
	defer func() {
		klog.V(4).Infof("Finished processing VPNC %s (took %v)", key, time.Since(startTime))
	}()

	// Get VPNC object from the informer cache
	vpnc, err := c.wf.VirtualPrivateNetworkConnectInformer().Lister().Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// VPNC was deleted
			klog.V(2).Infof("VPNC %s not found, cleaning up", key)
			return c.cleanupVPNC(key)
		}
		return fmt.Errorf("failed to get VPNC %s: %w", key, err)
	}

	return c.processVPNC(vpnc)
}

// processVPNC handles the main processing logic for a VPNC object
func (c *Controller) processVPNC(vpnc *vpnctypes.VirtualPrivateNetworkConnect) error {
	klog.V(2).Infof("Processing VPNC %s", vpnc.Name)

	// Parse subnet and tunnel key allocations from annotations
	config, err := c.parseVPNCAnnotations(vpnc)
	if err != nil {
		return fmt.Errorf("failed to parse VPNC annotations for %s: %w", vpnc.Name, err)
	}

	// Get the tunnel key for this zone (node)
	tunnelKey, err := c.getTunnelKeyForZone(config)
	if err != nil {
		return fmt.Errorf("failed to get tunnel key for zone %s: %w", c.zone, err)
	}
	config.TunnelKey = tunnelKey

	// Update cache
	c.vpncCacheLock.Lock()
	c.vpncCache[vpnc.Name] = config
	c.vpncCacheLock.Unlock()

	// Create or update OVN resources
	if err := c.createOVNResources(config); err != nil {
		return fmt.Errorf("failed to create OVN resources for VPNC %s: %w", vpnc.Name, err)
	}

	klog.V(2).Infof("Successfully processed VPNC %s", vpnc.Name)
	return nil
}

// parseVPNCAnnotations extracts subnet and tunnel key allocations from VPNC annotations
func (c *Controller) parseVPNCAnnotations(vpnc *vpnctypes.VirtualPrivateNetworkConnect) (*vpncConfig, error) {
	config := &vpncConfig{
		Name:                   vpnc.Name,
		SubnetAllocations:      make(map[string]*net.IPNet),
		TunnelKeyAllocations:   make(map[string]int),
		InterconnectRouterName: InterconnectRouterPrefix + vpnc.Name, // e.g., "vpnc-connect-router-colored-enterprise"
		CreatedOVNResources:    []string{},
	}

	if vpnc.Annotations == nil {
		return config, nil // No allocations yet
	}

	// Parse subnet allocations
	if subnetData, exists := vpnc.Annotations[VPNCNodeNetworkSubnetsAnnotation]; exists {
		var subnetMap map[string]string
		if err := json.Unmarshal([]byte(subnetData), &subnetMap); err != nil {
			return nil, fmt.Errorf("failed to unmarshal subnet allocations: %w", err)
		}

		for owner, subnetStr := range subnetMap {
			_, subnet, err := net.ParseCIDR(subnetStr)
			if err != nil {
				klog.Warningf("Invalid subnet %s for owner %s: %v", subnetStr, owner, err)
				continue
			}
			config.SubnetAllocations[owner] = subnet
		}
	}

	// Parse tunnel key allocations
	if tunnelKeyData, exists := vpnc.Annotations[VPNCNodeTunnelKeysAnnotation]; exists {
		if err := json.Unmarshal([]byte(tunnelKeyData), &config.TunnelKeyAllocations); err != nil {
			return nil, fmt.Errorf("failed to unmarshal tunnel key allocations: %w", err)
		}
	}

	klog.V(4).Infof("Parsed VPNC %s: %d subnet allocations, %d tunnel key allocations",
		vpnc.Name, len(config.SubnetAllocations), len(config.TunnelKeyAllocations))

	return config, nil
}

// getTunnelKeyForZone extracts the tunnel key for the current zone (node) from the VPNC config
func (c *Controller) getTunnelKeyForZone(config *vpncConfig) (int, error) {
	tunnelKey, exists := config.TunnelKeyAllocations[c.zone]
	if !exists {
		return 0, fmt.Errorf("no tunnel key allocation found for zone %s in VPNC %s", c.zone, config.Name)
	}

	klog.V(4).Infof("Using tunnel key %d for zone %s in VPNC %s", tunnelKey, c.zone, config.Name)
	return tunnelKey, nil
}

// createOVNResources creates the necessary OVN logical resources for the VPNC
func (c *Controller) createOVNResources(config *vpncConfig) error {
	klog.V(4).Infof("Creating OVN resources for VPNC %s", config.Name)

	// Create interconnect transit router for routing between networks
	if err := c.createInterconnectRouter(config); err != nil {
		return fmt.Errorf("failed to create interconnect router: %w", err)
	}

	// Create patch ports connecting interconnect router to network routers for each node
	if err := c.createInterconnectPatchPorts(config); err != nil {
		return fmt.Errorf("failed to create interconnect patch ports: %w", err)
	}

	// Create routing policies and routes for connectivity
	if err := c.createRoutingPoliciesAndRoutes(config); err != nil {
		return fmt.Errorf("failed to create routing policies and routes: %w", err)
	}

	return nil
}

// createInterconnectRouter creates the interconnect transit router for the VPNC
func (c *Controller) createInterconnectRouter(config *vpncConfig) error {
	routerName := config.InterconnectRouterName

	// Create or update interconnect router (idempotent operation)
	interconnectRouter := &nbdb.LogicalRouter{
		Name: routerName,
		Options: map[string]string{
			"requested-tnl-key": strconv.Itoa(config.TunnelKey),
		},
		ExternalIDs: map[string]string{
			ovntypes.NetworkExternalID:     config.Name,
			ovntypes.TopologyExternalID:    "interconnect",
			ovntypes.NetworkRoleExternalID: "transit",
		},
	}

	if err := libovsdbops.CreateOrUpdateLogicalRouter(c.nbClient, interconnectRouter); err != nil {
		return fmt.Errorf("failed to create/update interconnect router %s: %w", routerName, err)
	}

	config.CreatedOVNResources = append(config.CreatedOVNResources, "router:"+routerName)
	klog.V(4).Infof("Created/updated interconnect router %s for VPNC %s with tunnel key %d", routerName, config.Name, config.TunnelKey)

	return nil
}

// createInterconnectPatchPorts creates patch ports connecting interconnect router to network routers
func (c *Controller) createInterconnectPatchPorts(config *vpncConfig) error {
	for nodeName := range config.TunnelKeyAllocations {
		if err := c.createNodeInterconnectPatchPort(config, nodeName); err != nil {
			return fmt.Errorf("failed to create interconnect patch port for node %s: %w", nodeName, err)
		}
	}
	return nil
}

// createNodeInterconnectPatchPort creates peer ports connecting interconnect router to network routers for a specific node
func (c *Controller) createNodeInterconnectPatchPort(config *vpncConfig, nodeName string) error {
	klog.V(4).Infof("Creating interconnect peer ports for node %s", nodeName)

	// Find all networks that have subnet allocations for this node
	networksForNode := c.findNetworksForNode(config, nodeName)

	for _, allocation := range networksForNode {
		if err := c.createPeerPortsForNetwork(config, nodeName, allocation); err != nil {
			return fmt.Errorf("failed to create peer ports for network %s on node %s: %w", allocation.NetInfo.GetNetworkName(), nodeName, err)
		}
	}

	return nil
}

// NetworkAllocation represents a network allocation for a node in VPNC
type NetworkAllocation struct {
	NetInfo          util.NetInfo // NetInfo object with all network configuration
	PointToPointCIDR string       // The point-to-point subnet allocated for this node-network pair
}

// findNetworksForNode finds all networks that have subnet allocations for the given node
func (c *Controller) findNetworksForNode(config *vpncConfig, nodeName string) []NetworkAllocation {
	var networks []NetworkAllocation

	for owner, subnet := range config.SubnetAllocations {
		// owner format: "{nodeName}-Network{networkID}"
		if strings.HasPrefix(owner, nodeName+"-Network") {
			networkIDStr := strings.TrimPrefix(owner, nodeName+"-Network")
			networkID, err := strconv.Atoi(networkIDStr)
			if err != nil {
				klog.Warningf("Failed to parse network ID %s: %v", networkIDStr, err)
				continue
			}

			// Look up network information using NetInfo
			netInfo, err := c.getNetInfoFromID(networkID)
			if err != nil {
				klog.Warningf("Failed to get NetInfo for ID %d: %v", networkID, err)
				continue // Skip networks we can't resolve
			}

			// Create network allocation with NetInfo and point-to-point subnet
			allocation := NetworkAllocation{
				NetInfo:          netInfo,
				PointToPointCIDR: subnet.String(),
			}
			networks = append(networks, allocation)
		}
	}

	return networks
}

// createPeerPortsForNetwork creates peer ports between interconnect router and a specific network router
func (c *Controller) createPeerPortsForNetwork(config *vpncConfig, nodeName string, allocation NetworkAllocation) error {
	interconnectRouterName := config.InterconnectRouterName

	// Generate port names following the pattern from the gist
	// Pattern: {network}-{node}-to-{vpnc-name} and {vpnc-name}-to-{network}-{node}
	networkName := allocation.NetInfo.GetNetworkName()
	networkPortName := fmt.Sprintf("%s-%s-to-%s", networkName, nodeName, config.Name)
	interconnectPortName := fmt.Sprintf("%s-to-%s-%s", config.Name, networkName, nodeName)

	// Parse the point-to-point subnet allocation to get the two IPs
	networkIP, interconnectIP, err := c.parsePointToPointIPs(allocation.PointToPointCIDR)
	if err != nil {
		return fmt.Errorf("failed to parse subnet %s for network %s: %w", allocation.PointToPointCIDR, networkName, err)
	}

	// Generate MAC addresses from IP addresses using utility function
	networkMAC := util.IPAddrToHWAddr(net.ParseIP(networkIP)).String()
	interconnectMAC := util.IPAddrToHWAddr(net.ParseIP(interconnectIP)).String()

	// Parse CIDR to get individual network assignments for each port
	_, ipNet, err := net.ParseCIDR(allocation.PointToPointCIDR)
	if err != nil {
		return fmt.Errorf("failed to parse CIDR %s: %w", allocation.PointToPointCIDR, err)
	}
	networkCIDR := &net.IPNet{IP: net.ParseIP(networkIP), Mask: ipNet.Mask}
	interconnectCIDR := &net.IPNet{IP: net.ParseIP(interconnectIP), Mask: ipNet.Mask}

	// Create logical router port on network router
	networkLRP := &nbdb.LogicalRouterPort{
		Name:     networkPortName,
		MAC:      networkMAC,
		Networks: []string{networkCIDR.String()},
		Peer:     &interconnectPortName,
	}

	// Get the network-scoped router name and network ID from NetInfo
	routerName := allocation.NetInfo.GetNetworkScopedClusterRouterName()
	networkID := allocation.NetInfo.GetNetworkID()

	// Create logical router port on interconnect router with network ID as tunnel key
	interconnectLRP := &nbdb.LogicalRouterPort{
		Name:     interconnectPortName,
		MAC:      interconnectMAC,
		Networks: []string{interconnectCIDR.String()},
		Peer:     &networkPortName,
		Options: map[string]string{
			"requested-tnl-key": strconv.Itoa(networkID),
		},
	}

	// Add ports to their respective routers using libovsdb operations
	if err := c.addLogicalRouterPort(routerName, networkLRP); err != nil {
		return fmt.Errorf("failed to add port %s to network router %s: %w", networkPortName, routerName, err)
	}

	if err := c.addLogicalRouterPort(interconnectRouterName, interconnectLRP); err != nil {
		return fmt.Errorf("failed to add port %s to interconnect router %s: %w", interconnectPortName, interconnectRouterName, err)
	}

	// Track created resources
	config.CreatedOVNResources = append(config.CreatedOVNResources,
		fmt.Sprintf("lrp:%s:%s", routerName, networkPortName),
		fmt.Sprintf("lrp:%s:%s", interconnectRouterName, interconnectPortName))

	klog.V(4).Infof("Created peer ports %s ↔ %s (interconnect port tunnel key: %d)", networkPortName, interconnectPortName, networkID)

	return nil
}

// addLogicalRouterPort adds a logical router port to a router
func (c *Controller) addLogicalRouterPort(routerName string, lrp *nbdb.LogicalRouterPort) error {
	// Check if the router exists first
	router := &nbdb.LogicalRouter{Name: routerName}
	existingRouter, err := libovsdbops.GetLogicalRouter(c.nbClient, router)
	if err != nil {
		// Check if this might be a Layer 2 network (not yet supported)
		if strings.Contains(routerName, "_cluster_router") {
			return fmt.Errorf("router %s not found - this appears to be a Layer 2 network which is not yet supported by VPNC: %w", routerName, err)
		}
		return fmt.Errorf("router %s not found - network may not be fully initialized yet, will retry: %w", routerName, err)
	}

	// Create or update the logical router port and add it to the router
	if err := libovsdbops.CreateOrUpdateLogicalRouterPort(c.nbClient, existingRouter, lrp, nil); err != nil {
		return fmt.Errorf("failed to create logical router port %s on router %s: %w", lrp.Name, routerName, err)
	}

	klog.V(4).Infof("Created logical router port %s on router %s", lrp.Name, routerName)
	return nil
}

// parsePointToPointIPs parses a point-to-point CIDR and returns the two IP addresses
func (c *Controller) parsePointToPointIPs(subnetCIDR string) (string, string, error) {
	_, ipNet, err := net.ParseCIDR(subnetCIDR)
	if err != nil {
		return "", "", fmt.Errorf("failed to parse CIDR %s: %w", subnetCIDR, err)
	}

	// For point-to-point subnets, we have exactly 2 IP addresses
	if ones, bits := ipNet.Mask.Size(); ones != PointToPointSubnetMask || bits != PointToPointSubnetBits {
		return "", "", fmt.Errorf("expected /%d subnet, got /%d", PointToPointSubnetMask, ones)
	}

	// Get the network address (first IP)
	networkAddr := ipNet.IP

	// Get the second IP by incrementing the last byte
	broadcastAddr := make(net.IP, len(networkAddr))
	copy(broadcastAddr, networkAddr)
	broadcastAddr[len(broadcastAddr)-1]++

	return networkAddr.String(), broadcastAddr.String(), nil
}

// getNetInfoFromID looks up NetInfo from network ID using NAD annotations
func (c *Controller) getNetInfoFromID(networkID int) (util.NetInfo, error) {
	// List all NADs across all namespaces to find the one with matching network ID
	allNADs, err := c.nadLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list NADs: %w", err)
	}

	for _, nad := range allNADs {
		if nad.Annotations == nil {
			continue
		}

		// Check if this NAD has the network ID annotation
		if nadNetworkIDStr, exists := nad.Annotations[ovntypes.OvnNetworkIDAnnotation]; exists {
			nadNetworkID, err := strconv.Atoi(nadNetworkIDStr)
			if err != nil {
				continue // Skip NADs with invalid network ID annotation
			}

			if nadNetworkID == networkID {
				// Found the NAD with matching network ID - parse it to get NetInfo
				netInfo, err := util.ParseNADInfo(nad)
				if err != nil {
					return nil, fmt.Errorf("failed to parse NetInfo from NAD %s/%s: %w", nad.Namespace, nad.Name, err)
				}

				klog.V(4).Infof("Found NetInfo for network ID %d: %s", networkID, netInfo.GetNetworkName())
				return netInfo, nil
			}
		}
	}

	return nil, fmt.Errorf("network with ID %d not found", networkID)
}

// createRoutingPoliciesAndRoutes creates routing policies and routes for VPNC connectivity
func (c *Controller) createRoutingPoliciesAndRoutes(config *vpncConfig) error {
	klog.V(4).Infof("Creating routing policies and routes for VPNC %s", config.Name)

	// Get all unique networks from subnet allocations
	networkRouters := make(map[string]string)            // networkName -> routerName
	nodeNetworks := make(map[string][]NetworkAllocation) // nodeName -> []NetworkAllocation

	// Parse all subnet allocations to get network and node information
	for owner, subnet := range config.SubnetAllocations {
		// owner format: "{nodeName}-Network{networkID}"
		parts := strings.Split(owner, "-Network")
		if len(parts) != 2 {
			continue
		}
		nodeName := parts[0]
		networkID, err := strconv.Atoi(parts[1])
		if err != nil {
			klog.Warningf("Failed to parse network ID from %s: %v", owner, err)
			continue
		}

		netInfo, err := c.getNetInfoFromID(networkID)
		if err != nil {
			klog.Warningf("Failed to get NetInfo for ID %d: %v", networkID, err)
			continue
		}

		allocation := NetworkAllocation{
			NetInfo:          netInfo,
			PointToPointCIDR: subnet.String(),
		}

		networkName := netInfo.GetNetworkName()
		routerName := netInfo.GetNetworkScopedClusterRouterName()
		networkRouters[networkName] = routerName
		nodeNetworks[nodeName] = append(nodeNetworks[nodeName], allocation)
	}

	// Create routing policies on each network router
	for networkName, routerName := range networkRouters {
		if err := c.createNetworkRoutingPolicies(config, networkName, routerName, networkRouters); err != nil {
			return fmt.Errorf("failed to create routing policies for network %s: %w", networkName, err)
		}
	}

	// Create static routes on the interconnect router
	if err := c.createInterconnectRoutes(config, nodeNetworks); err != nil {
		return fmt.Errorf("failed to create interconnect routes: %w", err)
	}

	return nil
}

// createNetworkRoutingPolicies creates routing policies on a network router to redirect traffic to other networks
func (c *Controller) createNetworkRoutingPolicies(config *vpncConfig, currentNetwork, currentRouter string, allNetworks map[string]string) error {
	klog.V(4).Infof("Creating routing policies for network %s (router: %s)", currentNetwork, currentRouter)

	// For each node that has this network, create policies to route to other networks
	for owner, _ := range config.SubnetAllocations {
		// Parse owner to get node and network info
		parts := strings.Split(owner, "-Network")
		if len(parts) != 2 {
			continue
		}
		nodeName := parts[0]
		networkID, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}

		// Skip if this is not the current network
		currentNetInfo, err := c.getNetInfoFromID(networkID)
		if err != nil || currentNetInfo.GetNetworkName() != currentNetwork {
			continue
		}

		// For this node-network combination, create policies to route to other networks
		for otherNetwork, _ := range allNetworks {
			if otherNetwork == currentNetwork {
				continue // Skip self
			}

			// Get the interconnect IP for this node-network pair
			interconnectIP, err := c.getInterconnectIPForNodeNetwork(config, nodeName, currentNetwork)
			if err != nil {
				return fmt.Errorf("failed to get interconnect IP for node %s network %s: %w", nodeName, currentNetwork, err)
			}

			// Get destination subnets for the other network (aggregate all nodes)
			destSubnets, err := c.getNetworkAggregateSubnet(config, otherNetwork)
			if err != nil {
				return fmt.Errorf("failed to get aggregate subnets for network %s: %w", otherNetwork, err)
			}

			// Create routing policies for each destination subnet (IPv4 and IPv6)
			for _, destSubnet := range destSubnets {
				// Generate the inport name - this is the router-to-switch port name
				inportName := fmt.Sprintf("%s%s_%s", ovntypes.RouterToSwitchPrefix, currentNetwork, nodeName)

				// Create match expression for traffic from this node to the other network
				// Choose appropriate IP version based on subnet family
				var ipVersion string
				if destSubnet.IP.To4() != nil {
					ipVersion = "ip4"
				} else {
					ipVersion = "ip6"
				}
				matchExpr := fmt.Sprintf(`inport == "%s" && %s.dst == %s`, inportName, ipVersion, destSubnet.String())

				lrp := &nbdb.LogicalRouterPolicy{
					Priority: VPNCRoutingPolicyPriority,
					Match:    matchExpr,
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{interconnectIP},
					ExternalIDs: map[string]string{
						"vpnc-name":   config.Name,
						"source-node": nodeName,
						"source-net":  currentNetwork,
						"dest-net":    otherNetwork,
						"ip-family": func() string {
							if destSubnet.IP.To4() != nil {
								return "ipv4"
							}
							return "ipv6"
						}(),
					},
				}

				// Predicate to find existing policy for this specific route
				p := func(item *nbdb.LogicalRouterPolicy) bool {
					return item.Match == lrp.Match &&
						item.Priority == lrp.Priority &&
						item.ExternalIDs["vpnc-name"] == config.Name
				}

				err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(
					c.nbClient, currentRouter, lrp, p, &lrp.Nexthops, &lrp.Action)
				if err != nil {
					return fmt.Errorf("failed to create routing policy on %s for subnet %s: %w",
						currentRouter, destSubnet.String(), err)
				}

				config.CreatedOVNResources = append(config.CreatedOVNResources,
					fmt.Sprintf("policy:%s:%s", currentRouter, lrp.Match))

				klog.V(4).Infof("Created routing policy on %s: traffic from node %s to network %s (%s, %s) via %s",
					currentRouter, nodeName, otherNetwork, destSubnet.String(), lrp.ExternalIDs["ip-family"], interconnectIP)
			}
		}
	}

	return nil
}

// createInterconnectRoutes creates static routes on the interconnect router
func (c *Controller) createInterconnectRoutes(config *vpncConfig, nodeNetworks map[string][]NetworkAllocation) error {
	klog.V(4).Infof("Creating static routes on interconnect router %s", config.InterconnectRouterName)

	for nodeName, networks := range nodeNetworks {
		for _, allocation := range networks {
			// Get the network IP from the point-to-point subnet (first IP goes to network router)
			networkIP, _, err := c.parsePointToPointIPs(allocation.PointToPointCIDR)
			if err != nil {
				return fmt.Errorf("failed to parse IPs from subnet %s: %w", allocation.PointToPointCIDR, err)
			}

			networkName := allocation.NetInfo.GetNetworkName()
			// Get the actual node subnets for this network (supports dual-stack)
			destSubnets, err := c.getNodeSubnetForNetwork(nodeName, networkName)
			if err != nil {
				return fmt.Errorf("failed to get node subnets for node %s network %s: %w", nodeName, networkName, err)
			}

			// Create static routes for each destination subnet (IPv4 and IPv6)
			for _, destSubnet := range destSubnets {
				// Create static route using libovsdbops
				// Based on gist example: lr-route-add colored-enterprise-interconnect-router 100.10.0.0/24 192.168.0.6 dst-ip

				lrsr := &nbdb.LogicalRouterStaticRoute{
					IPPrefix: destSubnet.String(),
					Nexthop:  networkIP,
					Policy:   &nbdb.LogicalRouterStaticRoutePolicyDstIP,
					ExternalIDs: map[string]string{
						"vpnc-name":    config.Name,
						"node-name":    nodeName,
						"network-name": networkName,
						"ip-family": func() string {
							if destSubnet.IP.To4() != nil {
								return "ipv4"
							}
							return "ipv6"
						}(),
					},
				}

				// Predicate to find existing static route for this destination
				p := func(item *nbdb.LogicalRouterStaticRoute) bool {
					return item.IPPrefix == lrsr.IPPrefix &&
						item.Nexthop == lrsr.Nexthop &&
						item.ExternalIDs["vpnc-name"] == config.Name &&
						item.ExternalIDs["node-name"] == nodeName &&
						item.ExternalIDs["network-name"] == networkName
				}

				err = libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(
					c.nbClient, config.InterconnectRouterName, lrsr, p, &lrsr.Nexthop)
				if err != nil {
					return fmt.Errorf("failed to create static route on %s for subnet %s: %w",
						config.InterconnectRouterName, destSubnet.String(), err)
				}

				config.CreatedOVNResources = append(config.CreatedOVNResources,
					fmt.Sprintf("route:%s:%s->%s", config.InterconnectRouterName, destSubnet.String(), networkIP))

				klog.V(4).Infof("Created static route on %s: %s via %s (node %s, network %s, %s)",
					config.InterconnectRouterName, destSubnet.String(), networkIP, nodeName, networkName,
					lrsr.ExternalIDs["ip-family"])
			}
		}
	}

	return nil
}

// getInterconnectIPForNodeNetwork gets the interconnect router IP for a specific node-network pair
func (c *Controller) getInterconnectIPForNodeNetwork(config *vpncConfig, nodeName, networkName string) (string, error) {
	// Find the subnet allocation for this node-network pair
	for owner, subnet := range config.SubnetAllocations {
		// Parse to see if this matches our node-network
		parts := strings.Split(owner, "-Network")
		if len(parts) != 2 {
			continue
		}
		ownerNode := parts[0]
		networkID, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}

		if ownerNode == nodeName {
			netInfo, err := c.getNetInfoFromID(networkID)
			if err != nil || netInfo.GetNetworkName() != networkName {
				continue
			}

			// Parse the point-to-point subnet to get the interconnect IP (second IP)
			_, interconnectIP, err := c.parsePointToPointIPs(subnet.String())
			return interconnectIP, err
		}
	}

	return "", fmt.Errorf("no subnet allocation found for node %s network %s", nodeName, networkName)
}

// getNetworkAggregateSubnet gets all aggregate subnets for a network (covering all nodes)
// Returns both IPv4 and IPv6 subnets if they exist, supporting dual-stack networks
func (c *Controller) getNetworkAggregateSubnet(config *vpncConfig, networkName string) ([]*net.IPNet, error) {
	// Find any NetworkAllocation for this network name to get the NetInfo
	for owner, _ := range config.SubnetAllocations {
		// owner format: "{nodeName}-Network{networkID}"
		parts := strings.Split(owner, "-Network")
		if len(parts) != 2 {
			continue
		}
		networkID, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}

		// Get NetInfo for this ID
		netInfo, err := c.getNetInfoFromID(networkID)
		if err != nil || netInfo.GetNetworkName() != networkName {
			continue
		}

		// Get the network subnets from NetInfo
		subnets := netInfo.Subnets()
		if len(subnets) == 0 {
			return nil, fmt.Errorf("network %s has no subnets defined", networkName)
		}

		// Extract all subnet CIDRs to support dual-stack
		aggregateSubnets := make([]*net.IPNet, len(subnets))
		for i, subnet := range subnets {
			aggregateSubnets[i] = subnet.CIDR
		}

		klog.V(4).Infof("Found %d aggregate subnet(s) for network %s: %v",
			len(aggregateSubnets), networkName,
			func() []string {
				subnetStrs := make([]string, len(aggregateSubnets))
				for i, subnet := range aggregateSubnets {
					subnetStrs[i] = subnet.String()
				}
				return subnetStrs
			}())

		return aggregateSubnets, nil
	}

	return nil, fmt.Errorf("no NetInfo found for network %s", networkName)
}

// getNodeSubnetForNetwork gets all node-specific subnets for a given network
// Returns both IPv4 and IPv6 subnets if they exist, supporting dual-stack networks
func (c *Controller) getNodeSubnetForNetwork(nodeName, networkName string) ([]*net.IPNet, error) {
	// Get the node object from the lister
	node, err := c.nodeLister.Get(nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	// Parse the host subnet annotation for this network
	hostSubnets, err := util.ParseNodeHostSubnetAnnotation(node, networkName)
	if err != nil {
		return nil, fmt.Errorf("failed to parse host subnet annotation for node %s network %s: %w", nodeName, networkName, err)
	}

	if len(hostSubnets) == 0 {
		return nil, fmt.Errorf("no host subnets found for node %s network %s", nodeName, networkName)
	}

	klog.V(4).Infof("Found %d host subnet(s) for node %s network %s: %v",
		len(hostSubnets), nodeName, networkName,
		func() []string {
			subnets := make([]string, len(hostSubnets))
			for i, subnet := range hostSubnets {
				subnets[i] = subnet.String()
			}
			return subnets
		}())

	return hostSubnets, nil
}

// cleanupVPNC removes OVN resources for a deleted VPNC
func (c *Controller) cleanupVPNC(vpncName string) error {
	klog.V(2).Infof("Cleaning up OVN resources for VPNC %s", vpncName)

	c.vpncCacheLock.RLock()
	config, exists := c.vpncCache[vpncName]
	c.vpncCacheLock.RUnlock()

	if !exists {
		klog.V(4).Infof("No cached config for VPNC %s, attempting cleanup by name", vpncName)
		// Try to clean up by name patterns
		return c.cleanupByName(vpncName)
	}

	// Clean up tracked resources
	for _, resource := range config.CreatedOVNResources {
		parts := strings.SplitN(resource, ":", 2)
		if len(parts) != 2 {
			continue
		}
		resourceType, resourceName := parts[0], parts[1]

		switch resourceType {
		case "router":
			if err := c.deleteLogicalRouter(resourceName); err != nil {
				klog.Errorf("Failed to delete logical router %s: %v", resourceName, err)
			}
		case "lrp":
			// resourceName format: "routerName:portName"
			parts := strings.Split(resourceName, ":")
			if len(parts) != 2 {
				klog.Errorf("Invalid LRP resource format %s, expected routerName:portName", resourceName)
				continue
			}
			routerName, portName := parts[0], parts[1]
			if err := c.deleteLogicalRouterPort(routerName, portName); err != nil {
				klog.Errorf("Failed to delete logical router port %s from router %s: %v", portName, routerName, err)
			}
		case "patchport":
			// Legacy patch ports are automatically cleaned up when routers are deleted
			klog.V(4).Infof("Patch port %s will be cleaned up with router deletion", resourceName)
		default:
			klog.Warningf("Unknown resource type %s for resource %s", resourceType, resourceName)
		}
	}

	// Remove from cache
	c.vpncCacheLock.Lock()
	delete(c.vpncCache, vpncName)
	c.vpncCacheLock.Unlock()

	return nil
}

// cleanupByName attempts to cleanup resources by naming patterns
func (c *Controller) cleanupByName(vpncName string) error {
	klog.V(4).Infof("Cleaning up VPNC resources by name pattern for %s", vpncName)

	// Clean up routing policies on all routers for this VPNC
	if err := c.cleanupVPNCRoutingPolicies(vpncName); err != nil {
		klog.Errorf("Failed to cleanup routing policies for VPNC %s: %v", vpncName, err)
	}

	// Clean up static routes on interconnect router
	interconnectRouterName := InterconnectRouterPrefix + vpncName
	if err := c.cleanupVPNCStaticRoutes(interconnectRouterName, vpncName); err != nil {
		klog.Errorf("Failed to cleanup static routes for VPNC %s: %v", vpncName, err)
	}

	// Delete interconnect router (this will cascade delete associated ports)
	if err := c.deleteLogicalRouter(interconnectRouterName); err != nil {
		klog.Errorf("Failed to delete interconnect router %s for VPNC %s: %v", interconnectRouterName, vpncName, err)
	}

	return nil
}

// cleanupVPNCRoutingPolicies removes all routing policies created for this VPNC
func (c *Controller) cleanupVPNCRoutingPolicies(vpncName string) error {
	klog.V(4).Infof("Cleaning up routing policies for VPNC %s", vpncName)

	// Find all logical router policies with this VPNC's external ID across all routers
	// We use the global predicate approach to find policies in any router
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.ExternalIDs != nil && item.ExternalIDs["vpnc-name"] == vpncName
	}

	// Get all logical routers first
	routers, err := libovsdbops.FindLogicalRoutersWithPredicate(c.nbClient, func(item *nbdb.LogicalRouter) bool {
		return true // Get all routers
	})
	if err != nil {
		return fmt.Errorf("failed to get logical routers for VPNC policy cleanup: %w", err)
	}

	// Clean up policies from each router that might contain VPNC policies
	for _, router := range routers {
		err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(c.nbClient, router.Name, p)
		if err != nil && err != libovsdbclient.ErrNotFound {
			klog.Errorf("Failed to delete routing policies for VPNC %s from router %s: %v", vpncName, router.Name, err)
			// Continue with other routers even if one fails
		}
	}

	klog.V(4).Infof("Successfully cleaned up routing policies for VPNC %s", vpncName)
	return nil
}

// cleanupVPNCStaticRoutes removes all static routes created for this VPNC on the interconnect router
func (c *Controller) cleanupVPNCStaticRoutes(interconnectRouterName, vpncName string) error {
	klog.V(4).Infof("Cleaning up static routes on %s for VPNC %s", interconnectRouterName, vpncName)

	// Delete all static routes with this VPNC's external ID
	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.ExternalIDs["vpnc-name"] == vpncName
	}

	err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(c.nbClient, interconnectRouterName, p)
	if err != nil {
		return fmt.Errorf("failed to delete static routes from router %s: %w", interconnectRouterName, err)
	}

	klog.V(4).Infof("Successfully cleaned up static routes for VPNC %s", vpncName)
	return nil
}

// deleteLogicalRouter deletes a logical router by name
func (c *Controller) deleteLogicalRouter(name string) error {
	klog.V(4).Infof("Deleting logical router %s", name)

	router := &nbdb.LogicalRouter{Name: name}
	err := libovsdbops.DeleteLogicalRouter(c.nbClient, router)
	if err != nil {
		return fmt.Errorf("failed to delete logical router %s: %w", name, err)
	}

	klog.V(4).Infof("Successfully deleted logical router %s", name)
	return nil
}

// deleteLogicalRouterPort deletes a logical router port by name from its router
func (c *Controller) deleteLogicalRouterPort(routerName, portName string) error {
	klog.V(4).Infof("Deleting logical router port %s from router %s", portName, routerName)

	router := &nbdb.LogicalRouter{Name: routerName}
	lrp := &nbdb.LogicalRouterPort{Name: portName}
	err := libovsdbops.DeleteLogicalRouterPorts(c.nbClient, router, lrp)
	if err != nil {
		return fmt.Errorf("failed to delete logical router port %s from router %s: %w", portName, routerName, err)
	}

	klog.V(4).Infof("Successfully deleted logical router port %s from router %s", portName, routerName)
	return nil
}
