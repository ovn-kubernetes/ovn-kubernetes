package networkconnect

import (
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	apitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
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
		delete(c.cncCache, cncName)
		klog.V(4).Infof("Cleaned up cache for deleted CNC %s", cncName)
		return nil
	}
	// If CNC state doesn't exist yet (means its a CNC creation), create entry in the cache
	if !cncExists {
		cncState = &clusterNetworkConnectState{
			name:         cnc.Name,
			selectedNADs: sets.New[string](),
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
	if len(discoveredNetworks) == 0 {
		klog.Infof("No networks found for CNC %s", cncName)
		return nil
	}
	// STEP4: Generate subnets of size CNC.Spec.ConnectSubnets.NetworkPrefix for each layer3 network
	//  and /31 or /127 subnet for each layer2 networks

	// plumbing is now done, update the cache with latest
	cncState.selectedNADs = allMatchingNADKeys
	klog.V(5).Infof("Updated selectedNADs cache for CNC %s with %d NADs", cncName, allMatchingNADKeys.Len())
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
