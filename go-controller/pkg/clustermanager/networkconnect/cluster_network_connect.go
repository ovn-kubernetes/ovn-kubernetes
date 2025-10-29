package networkconnect

import (
	"errors"
	"fmt"
	"net"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	apitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var (
	errConfig = errors.New("configuration error")
)

func (c *Controller) reconcileClusterNetworkConnect(key string) error {
	c.Lock()
	defer c.Unlock()

	startTime := time.Now()
	_, cncName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(5).Infof("reconcileClusterNetworkConnect %s", cncName)
	defer func() {
		klog.Infof("reconcileClusterNetworkConnect %s took %v", cncName, time.Since(startTime))
	}()
	cnc, err := c.cncLister.Get(cncName)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if cnc == nil {
		// CNC is being deleted, clean up resources
		cncState, exists := c.cncCache[cncName]
		if exists {
			// Release tunnel key
			c.tunnelKeysAllocator.ReleaseKeys(cncName)
			klog.V(4).Infof("Released tunnel key for deleted CNC %s", cncName)

			// Release allocated subnets from this CNC's allocator
			if cncState.allocator != nil {
				cncState.allocator.ReleaseAllAllocations()
				klog.V(4).Infof("Released all subnets for deleted CNC %s", cncName)
			}
		}

		// Clean up the cache
		delete(c.cncCache, cncName)
		klog.V(4).Infof("Cleaned up cache for deleted CNC %s", cncName)
		return nil
	}

	// STEP1: Validate the CNC
	// STEP2: Discover the selected UDNs and CUDNs
	discoveredLayer3Networks, discoveredLayer2Networks, err := c.discoverSelectedNetworks(cnc)
	if err != nil {
		return err
	}
	if len(discoveredLayer3Networks) == 0 && len(discoveredLayer2Networks) == 0 {
		klog.Infof("No networks found for CNC %s", cncName)
		return nil
	}
	// STEP3: Generate subnets of size CNC.Spec.ConnectSubnets.NetworkPrefix for each layer3 network
	//  and /31 subnet for each layer2 networks
	allocatedSubnets, err := c.allocateSubnets(discoveredLayer3Networks, discoveredLayer2Networks, cnc.Spec.ConnectSubnets, cncName)
	if err != nil {
		return err
	}
	if len(allocatedSubnets) > 0 {
		err = util.UpdateNetworkConnectSubnetAnnotation(cnc, c.cncClient, allocatedSubnets)
		if err != nil {
			return err
		}
	}
	// STEP4: Generate a tunnelID for the connect router corresponding to this CNC
	// passing a value greater than 4096 as networkID - actually we don't need this value,
	// but it's required by the allocator.
	tunnelID, err := c.tunnelKeysAllocator.AllocateKeys(cnc.Name, 4096+1, 1)
	if err != nil {
		return err
	}
	err = util.UpdateNetworkConnectRouterTunnelKeyAnnotation(cnc, c.cncClient, tunnelID[0])
	if err != nil {
		return err
	}
	return nil
}

func (c *Controller) discoverSelectedNetworks(cnc *networkconnectv1.ClusterNetworkConnect) ([]*util.NetInfo, []*util.NetInfo, error) {
	discoveredLayer3Networks := []*util.NetInfo{}
	discoveredLayer2Networks := []*util.NetInfo{}
	allMatchingNADKeys := []string{}

	for _, selector := range cnc.Spec.NetworkSelectors {
		switch selector.NetworkSelectionType {
		case apitypes.ClusterUserDefinedNetworks:
			networkSelector, err := metav1.LabelSelectorAsSelector(&selector.ClusterUserDefinedNetworkSelector.NetworkSelector)
			if err != nil {
				return nil, nil, err
			}
			nads, err := c.nadLister.List(networkSelector)
			if err != nil {
				return nil, nil, err
			}
			for _, nad := range nads {
				// check this NAD is controlled by a CUDN
				controller := metav1.GetControllerOfNoCopy(nad)
				isCUDN := controller != nil && controller.Kind == cudnController.Kind && controller.APIVersion == cudnController.GroupVersion().String()
				isUDN := controller != nil && controller.Kind == udnController.Kind && controller.APIVersion == udnController.GroupVersion().String()
				if !isCUDN && !isUDN {
					continue
				}
				network, err := util.ParseNADInfo(nad)
				if err != nil {
					return nil, nil, err
				}
				if !network.IsPrimaryNetwork() {
					continue
				}
				// This NAD passed all validation checks, so it's selected by this CNC
				nadKey := nad.Namespace + "/" + nad.Name
				allMatchingNADKeys = append(allMatchingNADKeys, nadKey)
				if network.TopologyType() == ovntypes.Layer3Topology {
					discoveredLayer3Networks = append(discoveredLayer3Networks, &network)
				}
				if network.TopologyType() == ovntypes.Layer2Topology {
					discoveredLayer2Networks = append(discoveredLayer2Networks, &network)
				}
			}
		case apitypes.PrimaryUserDefinedNetworks:
			namespaceSelector, err := metav1.LabelSelectorAsSelector(&selector.PrimaryUserDefinedNetworkSelector.NamespaceSelector)
			if err != nil {
				return nil, nil, err
			}
			namespaces, err := c.namespaceLister.List(namespaceSelector)
			if err != nil {
				return nil, nil, err
			}
			for _, ns := range namespaces {
				namespacePrimaryNetwork, err := c.networkManager.GetActiveNetworkForNamespace(ns.Name)
				if err != nil {
					return nil, nil, err
				}
				if namespacePrimaryNetwork.IsDefault() || !namespacePrimaryNetwork.IsPrimaryNetwork() {
					continue
				}
				// Add NAD key for primary UDN (NAD is in the same namespace as the network)
				// TODO (tssurya): Need to add the NAD name here. Right now we are using the network name as the NAD name.
				nadKey := ns.Name + "/" + namespacePrimaryNetwork.GetNetworkName()
				allMatchingNADKeys = append(allMatchingNADKeys, nadKey)

				if namespacePrimaryNetwork.TopologyType() == ovntypes.Layer3Topology {
					discoveredLayer3Networks = append(discoveredLayer3Networks, &namespacePrimaryNetwork)
				}
				if namespacePrimaryNetwork.TopologyType() == ovntypes.Layer2Topology {
					discoveredLayer2Networks = append(discoveredLayer2Networks, &namespacePrimaryNetwork)
				}
			}
		default:
			return nil, nil, fmt.Errorf("%w: unsupported network selection type %s", errConfig, selector.NetworkSelectionType)
		}
	}

	// Update the selectedNADs cache with all matching NADs found during discovery.
	// This includes NADs from both ClusterUserDefinedNetworks and PrimaryUserDefinedNetworks selectors.
	c.updateCNCSelectedNADs(cnc, allMatchingNADKeys)

	return discoveredLayer3Networks, discoveredLayer2Networks, nil
}

func computeNetworkOwner(networkType string, networkID int) string {
	return fmt.Sprintf("%s_%d", networkType, networkID)
}

func (c *Controller) allocateSubnets(layer3Networks []*util.NetInfo, layer2Networks []*util.NetInfo, connectSubnets []networkconnectv1.ConnectSubnet,
	cncName string) (map[string][]*net.IPNet, error) {
	connectSubnetAllocator, err := c.fetchConnectSubnetAllocator(connectSubnets, cncName)
	if err != nil {
		return nil, err
	}
	allocations := make(map[string][]*net.IPNet)
	if len(layer3Networks) > 0 {
		for _, network := range layer3Networks {
			netInfo := *network
			networkID := netInfo.GetNetworkID()
			if networkID == ovntypes.NoNetworkID {
				return nil, fmt.Errorf("network id is invalid for network %s", netInfo.GetNetworkName())
			}

			owner := computeNetworkOwner(ovntypes.Layer3Topology, networkID)
			subnets, err := connectSubnetAllocator.AllocateLayer3Subnet(owner)
			if err != nil {
				return nil, err
			}
			klog.V(5).Infof("Allocated subnets %v for %s (network: %s)", subnets, owner, netInfo.GetNetworkName())
			allocations[owner] = subnets

		}
	}
	if len(layer2Networks) > 0 {
		for _, network := range layer2Networks {
			netInfo := *network
			networkID := netInfo.GetNetworkID()
			owner := computeNetworkOwner(ovntypes.Layer2Topology, networkID)
			subnets, err := connectSubnetAllocator.AllocateLayer2Subnet(owner)
			if err != nil {
				return nil, err
			}
			klog.V(5).Infof("Allocated subnets %v for %s (network: %s)", subnets, owner, netInfo.GetNetworkName())
			allocations[owner] = subnets
		}
	}
	return allocations, nil
}

func (c *Controller) fetchConnectSubnetAllocator(connectSubnets []networkconnectv1.ConnectSubnet, cncName string) (HybridConnectSubnetAllocator, error) {
	// NOTE: Caller must hold the global lock
	cncState := c.cncCache[cncName] // Cache entry should exist from updateCNCSelectedNADs

	// Check if allocator is already set up
	if cncState.allocator == nil {
		connectSubnetAllocator := NewHybridConnectSubnetAllocator()
		cncState.allocator = connectSubnetAllocator
		for _, connectSubnet := range connectSubnets {
			_, netCIDR, err := net.ParseCIDR(string(connectSubnet.CIDR))
			if err != nil {
				return nil, err
			}
			if utilnet.IsIPv4CIDR(netCIDR) && config.IPv4Mode {
				if err := connectSubnetAllocator.AddNetworkRange(netCIDR, int(connectSubnet.NetworkPrefix)); err != nil {
					return nil, err
				}
				klog.V(5).Infof("Added network range %s to cluster network connect %s subnet allocator", netCIDR, cncName)
			}
			if utilnet.IsIPv6CIDR(netCIDR) && config.IPv6Mode {
				if err := connectSubnetAllocator.AddNetworkRange(netCIDR, int(connectSubnet.NetworkPrefix)); err != nil {
					return nil, err
				}
				klog.V(5).Infof("Added network range %s to cluster network connect %s subnet allocator", netCIDR, cncName)
			}
		}
	}
	return cncState.allocator, nil
}

// updateCNCSelectedNADs updates the selectedNADs cache for a CNC with the current set of matching NAD keys.
// This is ONLY called during CNC reconciliation (discoverSelectedNetworks) to populate the cache
// with the authoritative set of NADs that match the CNC's selectors (both CUDN and PUDN).
// NOTE: Caller must hold the global lock.
func (c *Controller) updateCNCSelectedNADs(cnc *networkconnectv1.ClusterNetworkConnect, matchingNADKeys []string) {
	cncState, cncExists := c.cncCache[cnc.Name]

	// If CNC state doesn't exist yet, create it
	if !cncExists {
		cncState = &clusterNetworkConnectState{
			name:         cnc.Name,
			selectedNADs: make(map[string]bool),
		}
		c.cncCache[cnc.Name] = cncState
	}

	// Clear the current selected NADs and repopulate with current matches
	cncState.selectedNADs = make(map[string]bool)
	for _, nadKey := range matchingNADKeys {
		cncState.selectedNADs[nadKey] = true
	}

	klog.V(5).Infof("Updated selectedNADs cache for CNC %s with %d NADs", cnc.Name, len(matchingNADKeys))
}
