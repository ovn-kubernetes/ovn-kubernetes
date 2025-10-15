package networkconnect

import (
	"errors"
	"fmt"
	"net"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
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
	cncState, cncExists := c.cncCache[cncName]
	if cnc == nil {
		// CNC is being deleted, clean up resources
		// Clean up the cache
		// Note: allocator cleanup is not needed - it will be garbage collected
		// when the cache entry is deleted below since it's self-contained per-CNC
		delete(c.cncCache, cncName)
		klog.V(4).Infof("Cleaned up cache for deleted CNC %s", cncName)
		return nil
	}
	// If CNC state doesn't exist yet (means its a CNC creation), create entry in the cache
	if !cncExists {
		cncState = &clusterNetworkConnectState{
			name:             cnc.Name,
			selectedNADs:     sets.New[string](),
			selectedNetworks: sets.New[string](),
		}
		c.cncCache[cnc.Name] = cncState
	}
	// STEP1: Validate the CNC
	// STEP2: Generate a tunnelID for the connect router corresponding to this CNC
	// STEP3: Discover the selected UDNs and CUDNs
	discoveredNetworks, allMatchingNADKeys, err := c.discoverSelectedNetworks(cnc)
	if err != nil {
		return err
	}
	// STEP4: Generate or release subnets of size CNC.Spec.ConnectSubnets.NetworkPrefix for each layer3 network
	//  and /31 or /127 subnets for each layer2 network
	// We intentionally don't compute or use the networksNeedingAllocation set here because we want to return all
	// currently allocated subnets for each owner back to the annotation update step.
	allocatedSubnets, allMatchingNetworkKeys, err := c.allocateSubnets(discoveredNetworks, cnc.Spec.ConnectSubnets, cncName)
	if err != nil {
		return err
	}
	// This step will handle the release of subnets for networks that are no longer matched or are deleted.
	networksNeedingRelease := cncState.selectedNetworks.Difference(allMatchingNetworkKeys)
	if len(networksNeedingRelease) > 0 {
		err = c.releaseSubnets(cnc.Spec.ConnectSubnets, cncName, networksNeedingRelease)
		if err != nil {
			return err
		}
	}
	networksNeedingAllocation := allMatchingNetworkKeys.Difference(cncState.selectedNetworks)
	klog.V(5).Infof("DEBUG: cncState.selectedNetworks=%v, allMatchingNetworkKeys=%v, networksNeedingAllocation=%v, networksNeedingRelease=%v",
		cncState.selectedNetworks.UnsortedList(), allMatchingNetworkKeys.UnsortedList(), networksNeedingAllocation.UnsortedList(),
		networksNeedingRelease.UnsortedList())
	// we need to update the annotation only if there are networks that are newly matched or newly released
	if len(networksNeedingAllocation) > 0 || len(networksNeedingRelease) > 0 {
		err = util.UpdateNetworkConnectSubnetAnnotation(cnc, c.cncClient, allocatedSubnets)
		if err != nil {
			return err
		}
	}

	// plumbing is now done, update the cache with latest
	cncState.selectedNADs = allMatchingNADKeys
	klog.V(5).Infof("Updated selectedNADs cache for CNC %s with %d NADs", cncName, allMatchingNADKeys.Len())
	cncState.selectedNetworks = allMatchingNetworkKeys
	klog.V(5).Infof("Updated selectedNetworks cache for CNC %s with %d networks", cncName, allMatchingNetworkKeys.Len())
	return nil
}

func (c *Controller) discoverSelectedNetworks(cnc *networkconnectv1.ClusterNetworkConnect) ([]*util.NetInfo, sets.Set[string], error) {
	discoveredNetworks := []*util.NetInfo{}
	allMatchingNADKeys := sets.New[string]()
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
				isCUDN := controller != nil && controller.Kind == cudnGVK.Kind && controller.APIVersion == cudnGVK.GroupVersion().String()
				if !isCUDN {
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
				allMatchingNADKeys.Insert(nadKey)
				discoveredNetworks = append(discoveredNetworks, &network)
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
				if namespacePrimaryNetwork.IsDefault() {
					continue
				}
				// Get the NAD key for the primary network in this namespace.
				// Since this is the PrimaryUserDefinedNetworks selector (for namespace-scoped UDNs),
				// we expect exactly one NAD per network.
				// Today we don't support multiple primary NADs for a namespace, so this is safe.
				// Also note if the user misconfigures and ends up with CUDN and UDN for the same namespace,
				// and if the CUDN was created first - which means the UDN won't be created successfully,
				// then the user uses the P-UDN selector, the CUDN's NAD will be chosen here for this selector
				// but that's a design flaw in the user's configuration, and expectation is for users to use
				// the selectors correctly.
				primaryNADs := namespacePrimaryNetwork.GetNADs()
				if len(primaryNADs) != 1 {
					return nil, nil, fmt.Errorf("expected exactly one primary NAD for namespace %s, got %d", ns.Name, len(primaryNADs))
				}
				// GetNADs() returns NADs in "namespace/name" format, so use directly
				nadKey := primaryNADs[0]
				allMatchingNADKeys.Insert(nadKey)
				discoveredNetworks = append(discoveredNetworks, &namespacePrimaryNetwork)
			}
		default:
			return nil, nil, fmt.Errorf("%w: unsupported network selection type %s", errConfig, selector.NetworkSelectionType)
		}
	}

	return discoveredNetworks, allMatchingNADKeys, nil
}

func computeNetworkOwner(networkType string, networkID int) string {
	return fmt.Sprintf("%s_%d", networkType, networkID)
}

// parseNetworkOwner extracts the topology type from an owner key.
// Owner keys are formatted as "{topology}_{networkID}" (e.g., "layer3_1", "layer2_2").
func parseNetworkOwner(owner string) (topologyType string, ok bool) {
	if len(owner) > len(ovntypes.Layer3Topology)+1 && owner[:len(ovntypes.Layer3Topology)] == ovntypes.Layer3Topology {
		return ovntypes.Layer3Topology, true
	}
	if len(owner) > len(ovntypes.Layer2Topology)+1 && owner[:len(ovntypes.Layer2Topology)] == ovntypes.Layer2Topology {
		return ovntypes.Layer2Topology, true
	}
	return "", false
}

// allocateSubnets allocates subnets for the given discovered networks
// It returns a map of owner to subnets
// NOTE: If owner already had its subnets allocated, it will simply return those existing subnets
func (c *Controller) allocateSubnets(discoveredNetworks []*util.NetInfo, connectSubnets []networkconnectv1.ConnectSubnet,
	cncName string) (map[string][]*net.IPNet, sets.Set[string], error) {
	connectSubnetAllocator, err := c.fetchConnectSubnetAllocator(connectSubnets, cncName)
	if err != nil {
		return nil, nil, err
	}
	var owner string
	var subnets []*net.IPNet
	allMatchingNetworkKeys := sets.New[string]()
	allocatedSubnets := make(map[string][]*net.IPNet)
	for _, network := range discoveredNetworks {
		netInfo := *network
		networkID := netInfo.GetNetworkID()
		if networkID == ovntypes.NoNetworkID {
			return nil, nil, fmt.Errorf("network id is invalid for network %s", netInfo.GetNetworkName())
		}
		if netInfo.TopologyType() == ovntypes.Layer3Topology {
			owner = computeNetworkOwner(ovntypes.Layer3Topology, networkID)
			subnets, err = connectSubnetAllocator.AllocateLayer3Subnet(owner)
			if err != nil {
				return nil, nil, err
			}
		} else if netInfo.TopologyType() == ovntypes.Layer2Topology {
			owner = computeNetworkOwner(ovntypes.Layer2Topology, networkID)
			subnets, err = connectSubnetAllocator.AllocateLayer2Subnet(owner)
			if err != nil {
				return nil, nil, err
			}
		} else {
			// This should never happen
			return nil, nil, fmt.Errorf("unsupported network topology type %s for network %s", netInfo.TopologyType(), netInfo.GetNetworkName())
		}
		allocatedSubnets[owner] = subnets
		allMatchingNetworkKeys.Insert(owner)
		klog.V(5).Infof("Allocated subnets %v for %s (network: %s)", subnets, owner, netInfo.GetNetworkName())
	}
	return allocatedSubnets, allMatchingNetworkKeys, nil
}

func (c *Controller) fetchConnectSubnetAllocator(connectSubnets []networkconnectv1.ConnectSubnet, cncName string) (HybridConnectSubnetAllocator, error) {
	// NOTE: Caller must hold the global lock
	cncState := c.cncCache[cncName] // Cache entry should exist from updateCNCSelectedNADs

	// Check if allocator is already set up
	if cncState.allocator == nil {
		connectSubnetAllocator := NewHybridConnectSubnetAllocator()
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
		cncState.allocator = connectSubnetAllocator
		klog.V(5).Infof("Initialized subnet allocator for CNC %s", cncName)
	}
	return cncState.allocator, nil
}

// releaseSubnets releases subnets for the given network keys.
// Network keys encode topology type and network ID (e.g., "layer3_1", "layer2_2"),
// allowing subnet release without needing to re-discover network info.
func (c *Controller) releaseSubnets(connectSubnets []networkconnectv1.ConnectSubnet,
	cncName string, networksNeedingRelease sets.Set[string]) error {
	connectSubnetAllocator, err := c.fetchConnectSubnetAllocator(connectSubnets, cncName)
	if err != nil {
		return err
	}
	for networkKey := range networksNeedingRelease {
		topologyType, ok := parseNetworkOwner(networkKey)
		if !ok {
			klog.Warningf("Invalid network key format: %s, skipping release", networkKey)
			continue
		}
		switch topologyType {
		case ovntypes.Layer3Topology:
			connectSubnetAllocator.ReleaseLayer3Subnet(networkKey)
		case ovntypes.Layer2Topology:
			connectSubnetAllocator.ReleaseLayer2Subnet(networkKey)
		default:
			return fmt.Errorf("unsupported network topology type %s for network %s", topologyType, networkKey)
		}
		klog.V(5).Infof("Released subnets for network %s", networkKey)
	}
	return nil
}
