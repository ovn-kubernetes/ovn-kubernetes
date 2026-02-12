package networkconnect

import (
	"errors"
	"fmt"
	"net"
	"time"

	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	networkconnectv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var (
	errConfig = errors.New("configuration error")
)

// getPrimaryNADForNamespace returns the primary NAD key and network info for a namespace.
// This is used when processing namespaces that may have a primary UDN.
// Returns:
//   - nadKey: the primary NAD key in "namespace/name" format (empty if namespace uses default network)
//   - network: the network info for the primary network (nil if namespace uses default network)
//   - err: error if failed to get/validate the network
//
// If the namespace uses the default network (no primary UDN), returns ("", nil, nil).
// Callers should check for empty nadKey to determine if namespace has a primary UDN.
func getPrimaryNADForNamespace(networkMgr networkmanager.Interface, namespaceName string, nadLister nadlisters.NetworkAttachmentDefinitionLister) (nadKey string, network util.NetInfo, err error) {
	namespacePrimaryNetwork, err := networkMgr.GetActiveNetworkForNamespace(namespaceName)
	if err != nil {
		if util.IsInvalidPrimaryNetworkError(err) {
			// We intentionally ignore the invalid primary network error because
			// UDN Controller hasn't created the NAD yet, OR NAD doesn't exist in a
			// namespace that has the required UDN label. It could also be that the
			// UDN was deleted and the NAD is also gone.
			return "", nil, nil
		}
		return "", nil, err
	}
	if namespacePrimaryNetwork == nil || namespacePrimaryNetwork.IsDefault() {
		// No primary UDN in this namespace
		return "", nil, nil
	}
	primaryNADKey, err := networkMgr.GetPrimaryNADForNamespace(namespaceName)
	if err != nil {
		if util.IsInvalidPrimaryNetworkError(err) {
			return "", nil, nil
		}
		return "", nil, err
	}
	if primaryNADKey == ovntypes.DefaultNetworkName {
		return "", nil, nil
	}
	// There is a race condition where NAD is already deleted from kapi
	// but network manager is too slow to update the network manager cache.
	// In this case, the primary NAD key may still be cached even though it is deleted.
	// So let's fetch the NAD again from the kapi to double confirm it exists
	// before returning it.
	nadNamespace, nadName, err := cache.SplitMetaNamespaceKey(primaryNADKey)
	if err != nil {
		return "", nil, fmt.Errorf("failed to split NAD key %s: %w", primaryNADKey, err)
	}
	_, err = nadLister.NetworkAttachmentDefinitions(nadNamespace).Get(nadName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.Warningf("NAD %s not found in kapi, returning empty network info even if network manager cache says it exists", primaryNADKey)
			return "", nil, nil
		}
		return "", nil, err
	}
	return primaryNADKey, namespacePrimaryNetwork, nil
}

func (c *Controller) reconcileClusterNetworkConnect(key string) error {
	c.Lock()
	defer c.Unlock()
	startTime := time.Now()
	_, cncName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("failed to split CNC key %s: %w", key, err)
	}
	klog.V(5).Infof("reconcileClusterNetworkConnect %s", cncName)
	defer func() {
		klog.Infof("reconcileClusterNetworkConnect %s took %v", cncName, time.Since(startTime))
	}()
	cnc, err := c.cncLister.Get(cncName)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get CNC %s: %w", cncName, err)
	}
	cncState, cncExists := c.cncCache[cncName]
	if cnc == nil {
		// CNC is being deleted, clean up resources
		// Clean up the cache
		// Note: allocator cleanup is not needed - it will be garbage collected
		// when the cache entry is deleted below since it's self-contained per-CNC
		// Annotations also don't need to be removed since object is already deleted.
		if cncExists {
			// Release tunnel key
			c.tunnelKeysAllocator.ReleaseKeys(cncName)
			klog.V(4).Infof("Released tunnel key for deleted CNC %s", cncName)
		}

		// Clean up the cache
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
		connectSubnetAllocator, err := NewHybridConnectSubnetAllocator(cnc.Spec.ConnectSubnets, cncName)
		if err != nil {
			return fmt.Errorf("failed to initialize subnet allocator for CNC %s: %w", cncName, err)
		}
		cncState.allocator = connectSubnetAllocator
		klog.V(5).Infof("Initialized subnet allocator for CNC %s", cncName)
		c.cncCache[cnc.Name] = cncState
	}
	// STEP1: Validate the CNC
	// STEP2: Generate a tunnelID for the connect router corresponding to this CNC
	// passing a value greater than 4096 as networkID - actually we don't need this value,
	// but it's required by the allocator to ensure that the prederministic tunnel keys
	// that are derived from the networkID are not reused for backwards compatibility reasons.
	// So we want to skip that range and use the next available tunnel key.
	// do this only if the CNC is being created - its a one time allocation.
	if cncState.tunnelID == 0 { // cncState will exist as its created above
		tunnelID, err := c.tunnelKeysAllocator.AllocateKeys(cnc.Name, 4096+1, 1)
		if err != nil {
			return fmt.Errorf("failed to allocate tunnel key for CNC %s: %w", cncName, err)
		}
		err = util.UpdateNetworkConnectRouterTunnelKeyAnnotation(cnc.Name, c.cncClient, tunnelID[0])
		if err != nil {
			return fmt.Errorf("failed to update network connect router tunnel key annotation for CNC %s: %w", cncName, err)
		}
		cncState.tunnelID = tunnelID[0]
	}
	// STEP3: Discover the selected UDNs and CUDNs
	// Discovery, allocation, and release continue on per-network errors, so healthy networks
	// make progress. Errors are aggregated and returned at the end.
	var errs []error
	discoveredNetworks, allMatchingNADKeys, err := c.discoverSelectedNetworks(cnc)
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to discover selected networks for CNC %s: %w", cncName, err))
	}
	// STEP4: Generate or release subnets of size CNC.Spec.ConnectSubnets.NetworkPrefix for each layer3 network
	//  and /31 or /127 subnets for each layer2 network
	// We intentionally don't compute or use the networksNeedingAllocation set here because we want to return all
	// currently allocated subnets for each owner back to the annotation update step.
	allocatedSubnets, allMatchingNetworkKeys, err := c.allocateSubnets(discoveredNetworks, cncState.allocator)
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to allocate subnets for CNC %s: %w", cncName, err))
	}
	// This step will handle the release of subnets for networks that are no longer matched or are deleted.
	// NOTE: Since allMatchingNetworkKeys might not have the network or nad which had a transient error,
	// (a rare event like informer list of get or parse nad going wrong for a nad update event), its possible
	// we end up releasing and re-allocating subnets for networks that had a transient error. But that risk is
	// acceptable and we can live with it in favor of the gain we get by not blocking the setup of other healthy networks.
	networksNeedingRelease := cncState.selectedNetworks.Difference(allMatchingNetworkKeys)
	if len(networksNeedingRelease) > 0 {
		err = c.releaseSubnets(networksNeedingRelease, cncState.allocator)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to release subnets for CNC %s: %w", cncName, err))
		}
	}
	networksNeedingAllocation := allMatchingNetworkKeys.Difference(cncState.selectedNetworks)
	klog.V(5).Infof("CNC %s: selectedNetworks=%v, allMatchingNetworkKeys=%v, networksNeedingAllocation=%v, networksNeedingRelease=%v",
		cncName,
		cncState.selectedNetworks.UnsortedList(), allMatchingNetworkKeys.UnsortedList(), networksNeedingAllocation.UnsortedList(),
		networksNeedingRelease.UnsortedList())
	// we need to update the annotation only if there are networks that are newly matched or newly released
	if len(networksNeedingAllocation) > 0 || len(networksNeedingRelease) > 0 {
		err = util.UpdateNetworkConnectSubnetAnnotation(cnc, c.cncClient, allocatedSubnets)
		if err != nil {
			return fmt.Errorf("failed to update network connect subnet annotation for CNC %s: %w", cncName, err)
		}
	}
	// plumbing is now done, update the cache with latest
	cncState.selectedNADs = allMatchingNADKeys
	klog.V(5).Infof("Updated selectedNADs cache for CNC %s with %d NADs", cncName, allMatchingNADKeys.Len())
	cncState.selectedNetworks = allMatchingNetworkKeys
	klog.V(5).Infof("Updated selectedNetworks cache for CNC %s with %d networks", cncName, allMatchingNetworkKeys.Len())
	return kerrors.NewAggregate(errs)
}

func (c *Controller) discoverSelectedNetworks(cnc *networkconnectv1.ClusterNetworkConnect) ([]util.NetInfo, sets.Set[string], error) {
	discoveredNetworks := []util.NetInfo{}
	allMatchingNADKeys := sets.New[string]()
	var errs []error

	for _, selector := range cnc.Spec.NetworkSelectors {
		switch selector.NetworkSelectionType {
		case apitypes.ClusterUserDefinedNetworks:
			networkSelector, err := metav1.LabelSelectorAsSelector(&selector.ClusterUserDefinedNetworkSelector.NetworkSelector)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to parse CUDN network selector: %w", err))
				continue
			}
			nads, err := c.nadLister.List(networkSelector)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to list NADs for CUDN selector: %w", err))
				continue
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
					errs = append(errs, fmt.Errorf("failed to parse NAD %s/%s: %w", nad.Namespace, nad.Name, err))
					continue
				}
				if !network.IsPrimaryNetwork() {
					continue
				}
				// This NAD passed all validation checks, so it's selected by this CNC
				nadKey := nad.Namespace + "/" + nad.Name
				allMatchingNADKeys.Insert(nadKey)
				discoveredNetworks = append(discoveredNetworks, network)
			}
		case apitypes.PrimaryUserDefinedNetworks:
			namespaceSelector, err := metav1.LabelSelectorAsSelector(&selector.PrimaryUserDefinedNetworkSelector.NamespaceSelector)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to parse PUDN namespace selector: %w", err))
				continue
			}
			namespaces, err := c.namespaceLister.List(namespaceSelector)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to list namespaces for PUDN selector: %w", err))
				continue
			}
			for _, ns := range namespaces {
				nadKey, namespacePrimaryNetwork, err := getPrimaryNADForNamespace(c.networkManager, ns.Name, c.nadLister)
				if err != nil {
					errs = append(errs, fmt.Errorf("failed to get active network for namespace %s: %w", ns.Name, err))
					continue
				}
				if nadKey == "" {
					// Namespace uses default network (no primary UDN) or UDN was deleted
					continue
				}
				allMatchingNADKeys.Insert(nadKey)
				discoveredNetworks = append(discoveredNetworks, namespacePrimaryNetwork)
			}
		default:
			errs = append(errs, fmt.Errorf("%w: unsupported network selection type %s", errConfig, selector.NetworkSelectionType))
		}
	}

	return discoveredNetworks, allMatchingNADKeys, kerrors.NewAggregate(errs)
}

// allocateSubnets allocates subnets for the given discovered networks
// It returns a map of owner to subnets
// NOTE: If owner already had its subnets allocated, it will simply return those existing subnets
func (c *Controller) allocateSubnets(discoveredNetworks []util.NetInfo, allocator HybridConnectSubnetAllocator) (map[string][]*net.IPNet, sets.Set[string], error) {
	var owner string
	var subnets []*net.IPNet
	var errs []error
	allMatchingNetworkKeys := sets.New[string]()
	allocatedSubnets := make(map[string][]*net.IPNet)
	for _, network := range discoveredNetworks {
		networkID := network.GetNetworkID()
		if networkID == ovntypes.NoNetworkID {
			errs = append(errs, fmt.Errorf("network id is invalid for network %s", network.GetNetworkName()))
			continue
		}
		var err error
		if network.TopologyType() == ovntypes.Layer3Topology {
			owner = util.ComputeNetworkOwner(ovntypes.Layer3Topology, networkID)
			subnets, err = allocator.AllocateLayer3Subnet(owner)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to allocate Layer3 subnet for network %s: %w", network.GetNetworkName(), err))
				continue
			}
		} else if network.TopologyType() == ovntypes.Layer2Topology {
			owner = util.ComputeNetworkOwner(ovntypes.Layer2Topology, networkID)
			subnets, err = allocator.AllocateLayer2Subnet(owner)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to allocate Layer2 subnet for network %s: %w", network.GetNetworkName(), err))
				continue
			}
		} else {
			errs = append(errs, fmt.Errorf("unsupported network topology type %s for network %s", network.TopologyType(), network.GetNetworkName()))
			continue
		}
		allocatedSubnets[owner] = subnets
		allMatchingNetworkKeys.Insert(owner)
		klog.V(5).Infof("Allocated subnets %v for %s (network: %s)", subnets, owner, network.GetNetworkName())
	}
	return allocatedSubnets, allMatchingNetworkKeys, kerrors.NewAggregate(errs)
}

// releaseSubnets releases subnets for the given network keys.
// Network keys encode topology type and network ID (e.g., "layer3_1", "layer2_2"),
// allowing subnet release without needing to re-discover network info.
func (c *Controller) releaseSubnets(networksNeedingRelease sets.Set[string],
	allocator HybridConnectSubnetAllocator) error {
	var errs []error
	for networkKey := range networksNeedingRelease {
		topologyType, _, err := util.ParseNetworkOwner(networkKey)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid network key format: %s", networkKey))
			continue
		}
		switch topologyType {
		case ovntypes.Layer3Topology:
			allocator.ReleaseLayer3Subnet(networkKey)
		case ovntypes.Layer2Topology:
			allocator.ReleaseLayer2Subnet(networkKey)
		default:
			errs = append(errs, fmt.Errorf("unsupported network topology type %s for network %s", topologyType, networkKey))
			continue
		}
		klog.V(5).Infof("Released subnets for network %s", networkKey)
	}
	return kerrors.NewAggregate(errs)
}
