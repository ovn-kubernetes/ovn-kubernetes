package egressip

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/gaissmai/cidrtree"
	ocpconfigapi "github.com/openshift/api/config/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	informerscorev1 "k8s.io/client-go/informers/core/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipinformerv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/informers/externalversions/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func PlatformTypeIsEgressIPCloudProvider() bool {
	return config.Kubernetes.PlatformType == string(ocpconfigapi.AWSPlatformType) ||
		config.Kubernetes.PlatformType == string(ocpconfigapi.GCPPlatformType) ||
		config.Kubernetes.PlatformType == string(ocpconfigapi.AzurePlatformType) ||
		config.Kubernetes.PlatformType == string(ocpconfigapi.OpenStackPlatformType)
}

// GetNodeEIPConfig attempts to generate EIP configuration from a nodes
// annotations. If the platform is running in the cloud, retrieve config info
// from node obj annotation added by Cloud Network Config Controller (CNCC). If
// not on a cloud platform (i.e. baremetal), retrieve from the node obj primary
// interface annotation.
func GetNodeEIPConfig(node *corev1.Node) (*util.ParsedNodeEgressIPConfiguration, error) {
	var parsedEgressIPConfig *util.ParsedNodeEgressIPConfiguration
	var err error
	if PlatformTypeIsEgressIPCloudProvider() {
		parsedEgressIPConfig, err = util.ParseCloudEgressIPConfig(node)
	} else {
		parsedEgressIPConfig, err = util.ParseNodePrimaryIfAddr(node)
	}
	if err != nil {
		return nil, fmt.Errorf("unable to generate egress IP config for node %s: %w", node.Name, err)
	}
	return parsedEgressIPConfig, nil
}

func GetNodeHostCIDRsExcludeOVNNetworks(node *corev1.Node, eipConfig *util.ParsedNodeEgressIPConfiguration) ([]string, error) {
	networks, err := util.ParseNodeHostCIDRsList(node)
	if err != nil {
		return nil, err
	}
	networks = util.RemoveItemFromSliceUnstable(networks, eipConfig.V4.IP.String())
	networks = util.RemoveItemFromSliceUnstable(networks, eipConfig.V6.IP.String())
	return networks, nil
}

type watchFactory interface {
	NamespaceInformer() informerscorev1.NamespaceInformer
	EgressIPInformer() egressipinformerv1.EgressIPInformer
}

// IsEgressIPLocal checks if the EgressIP is hosted on a local network either
// through a primary or secondary interface.
func IsEgressIPLocal(
	wf watchFactory,
	nm networkmanager.Interface,
	eipCponfig *util.ParsedNodeEgressIPConfiguration,
	node *corev1.Node,
	eip string,
	ip net.IP,
) (bool, error) {
	checkAdvertisements := false
	primary, secondary, _, _, err := getEgressIPPrimarySecondaryAdvertised(wf, nm, eipCponfig, node, eip, ip, checkAdvertisements)
	return primary || secondary, err
}

// IsEgressIPLocalOrAdvertised checks if the EgressIP can be hosted on the
// given node: either because it is local to a network directly connected to the
// node through a primary or secondary interface, or because it is configured to
// be advertised for any of its selected namespaces on the given node.
func IsEgressIPLocalOrAdvertised(
	wf watchFactory,
	nm networkmanager.Interface,
	eipCponfig *util.ParsedNodeEgressIPConfiguration,
	node *corev1.Node,
	eip string,
	ip net.IP,
) (bool, error) {
	checkAdvertisements := true
	primary, secondary, advertised, _, err := getEgressIPPrimarySecondaryAdvertised(wf, nm, eipCponfig, node, eip, ip, checkAdvertisements)
	return primary || secondary || advertised, err
}

// IsEgressIPLocalToSecondaryInterface checks if the EgressIP can be hosted on
// secondary interface on the given node
func IsEgressIPLocalToSecondaryInterface(
	wf watchFactory,
	nm networkmanager.Interface,
	eipCponfig *util.ParsedNodeEgressIPConfiguration,
	node *corev1.Node,
	eip string,
	ip net.IP,
) (bool, error) {
	checkAdvertisements := false
	_, secondary, _, _, err := getEgressIPPrimarySecondaryAdvertised(wf, nm, eipCponfig, node, eip, ip, checkAdvertisements)
	return secondary, err
}

// GetEgressIPAdvertisedNodes return the common nodes on which an EgressIP is
// advertised for all the selected namespaces.
func GetEgressIPAdvertisedNodes(wf watchFactory, nm networkmanager.Interface, name string) (sets.Set[string], error) {
	if !AdvertisementsEnabled() {
		return sets.Set[string]{}, nil
	}

	var advertisedOnNodes sets.Set[string]
	eip, err := wf.EgressIPInformer().Lister().Get(name)
	if apierrors.IsNotFound(err) {
		return sets.Set[string]{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get EgressIP %s: %v", name, err)
	}
	selector, err := metav1.LabelSelectorAsSelector(&eip.Spec.NamespaceSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to parse EgressIP %s namespace selector: %v", name, err)
	}
	selectedNs, err := wf.NamespaceInformer().Lister().List(selector)
	if err != nil {
		return nil, fmt.Errorf("failed to list selected namespaces for EgressIP %s: %v", name, err)
	}
	for _, ns := range selectedNs {
		network := nm.GetActiveNetworkForNamespaceFast(ns.Name)
		nodes := network.GetEgressIPAdvertisedNodes()
		switch {
		case len(nodes) == 0:
			// not advertised for this network
			continue
		case advertisedOnNodes == nil:
			advertisedOnNodes = sets.New(nodes...)
		default:
			advertisedOnNodes = advertisedOnNodes.Intersection(sets.New(nodes...))
		}
	}
	if advertisedOnNodes == nil {
		advertisedOnNodes = sets.Set[string]{}
	}

	return advertisedOnNodes, nil
}

func AdvertisementsEnabled() bool {
	return util.IsRouteAdvertisementsEnabled() && config.OVNKubernetesFeature.EnableEgressIP
}

func getEgressIPPrimarySecondaryAdvertised(
	wf watchFactory,
	nm networkmanager.Interface,
	eipConfig *util.ParsedNodeEgressIPConfiguration,
	node *corev1.Node,
	eip string,
	ip net.IP,
	checkAdvertisements bool,
) (primary, secondary, advertised bool, network string, err error) {
	primary = isEgressIPPrimaryNetwork(eipConfig, ip)
	if primary {
		return
	}
	secondaryIfAddrs, err := GetNodeHostCIDRsExcludeOVNNetworks(node, eipConfig)
	if err != nil {
		return
	}
	secondary, err = IsEgressIPSecondaryNetwork(secondaryIfAddrs, ip)
	if secondary || err != nil {
		return
	}
	if !AdvertisementsEnabled() || !checkAdvertisements {
		return
	}
	advertised, err = isAdvertisedOnNode(wf, nm, eip, node.Name)
	return
}

func isEgressIPPrimaryNetwork(eIPConfig *util.ParsedNodeEgressIPConfiguration, ip net.IP) bool {
	if eIPConfig.V4.Net != nil && eIPConfig.V4.Net.Contains(ip) {
		return true
	}
	if eIPConfig.V6.Net != nil && eIPConfig.V6.Net.Contains(ip) {
		return true
	}
	return false
}

// IsEgressIPSecondaryNetwork check if there is a secondary host network
// to host the argument IP considering only global unicast addresses.
func IsEgressIPSecondaryNetwork(secondaryNetworks []string, ip net.IP) (bool, error) {
	// Do not attempt to check if a secondary host network may host an EIP if we
	// are in a cloud environment
	if util.PlatformTypeIsEgressIPCloudProvider() {
		return false, nil
	}
	cidrs, err := makeCIDRs(secondaryNetworks...)
	if err != nil {
		return false, err
	}
	if len(cidrs) == 0 {
		return false, nil
	}
	isIPv6 := ip.To4() == nil
	cidrs = filterIPVersion(cidrs, isIPv6)
	lpmTree := cidrtree.New(cidrs...)
	for _, prefix := range cidrs {
		if !prefix.Addr().IsGlobalUnicast() {
			lpmTree.Delete(prefix)
		}
	}
	addr, err := netip.ParseAddr(ip.String())
	if err != nil {
		return false, fmt.Errorf("failed to convert IP %s to netip address: %v", ip.String(), err)
	}
	_, found := lpmTree.Lookup(addr)
	return found, nil
}

func isAdvertisedOnNode(wf watchFactory, nm networkmanager.Interface, eip, node string) (bool, error) {
	nodes, err := GetEgressIPAdvertisedNodes(wf, nm, eip)
	if err != nil {
		return false, err
	}
	return nodes.Has(node), nil
}

func makeCIDRs(s ...string) (cidrs []netip.Prefix, err error) {
	for _, cidrString := range s {
		prefix, err := netip.ParsePrefix(cidrString)
		if err != nil {
			return nil, err
		}
		cidrs = append(cidrs, prefix)
	}
	return cidrs, nil
}

func filterIPVersion(cidrs []netip.Prefix, v6 bool) []netip.Prefix {
	validCIDRs := make([]netip.Prefix, 0, len(cidrs))
	for _, cidr := range cidrs {
		if cidr.Addr().Is4() && v6 {
			continue
		}
		if cidr.Addr().Is6() && !v6 {
			continue
		}
		validCIDRs = append(validCIDRs, cidr)
	}
	return validCIDRs
}
