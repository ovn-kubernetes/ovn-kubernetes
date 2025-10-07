package networkconnect

import (
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	apitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var (
	errConfig = errors.New("configuration error")
)

func (c *Controller) reconcileClusterNetworkConnect(key string) error {
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
		// TODO: delete the CNC and related resources
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
	// STEP4: Generate a tunnelID for the connect router corresponding to this CNC

	return nil
}

func (c *Controller) discoverSelectedNetworks(cnc *networkconnectv1.ClusterNetworkConnect) ([]*util.NetInfo, []*util.NetInfo, error) {
	discoveredLayer3Networks := []*util.NetInfo{}
	discoveredLayer2Networks := []*util.NetInfo{}

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
	return discoveredLayer3Networks, discoveredLayer2Networks, nil
}
