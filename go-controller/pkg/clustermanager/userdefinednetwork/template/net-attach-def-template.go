package template

import (
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"strings"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	iputils "github.com/containernetworking/plugins/pkg/ip"
	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilnet "k8s.io/utils/net"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	OvnK8sCNIOverlay = "ovn-k8s-cni-overlay"

	FinalizerUserDefinedNetwork = "k8s.ovn.org/user-defined-network-protection"
	LabelUserDefinedNetwork     = "k8s.ovn.org/user-defined-network"

	cniVersion = "1.0.0"
)

type SpecGetter interface {
	GetTopology() userdefinednetworkv1.NetworkTopology
	GetLayer3() *userdefinednetworkv1.Layer3Config
	GetLayer2() *userdefinednetworkv1.Layer2Config
	GetLocalnet() *userdefinednetworkv1.LocalnetConfig
}

func RenderNetAttachDefManifest(obj client.Object, targetNamespace string) (*netv1.NetworkAttachmentDefinition, error) {
	if obj == nil {
		return nil, nil
	}

	if targetNamespace == "" {
		return nil, fmt.Errorf("namspace should not be empty")
	}

	var ownerRef metav1.OwnerReference
	var spec SpecGetter
	var networkName string
	switch o := obj.(type) {
	case *userdefinednetworkv1.UserDefinedNetwork:
		ownerRef = *metav1.NewControllerRef(obj, userdefinednetworkv1.SchemeGroupVersion.WithKind("UserDefinedNetwork"))
		spec = &o.Spec
		networkName = util.GenerateUDNNetworkName(targetNamespace, obj.GetName())
	case *userdefinednetworkv1.ClusterUserDefinedNetwork:
		ownerRef = *metav1.NewControllerRef(obj, userdefinednetworkv1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetwork"))
		spec = &o.Spec.Network
		networkName = util.GenerateCUDNNetworkName(obj.GetName())
	default:
		return nil, fmt.Errorf("unknown type %T", obj)
	}

	nadName := util.GetNADName(targetNamespace, obj.GetName())

	nadSpec, err := RenderNADSpec(networkName, nadName, spec)
	if err != nil {
		return nil, err
	}

	return &netv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:            obj.GetName(),
			OwnerReferences: []metav1.OwnerReference{ownerRef},
			Labels:          renderNADLabels(obj),
			Annotations:     renderNADAnnotations(obj),
			Finalizers:      []string{FinalizerUserDefinedNetwork},
		},
		Spec: *nadSpec,
	}, nil
}

func RenderNADSpec(networkName, nadName string, spec SpecGetter) (*netv1.NetworkAttachmentDefinitionSpec, error) {
	if err := validateTopology(spec); err != nil {
		return nil, fmt.Errorf("invalid topology specified: %w", err)
	}

	cniNetConf, err := renderCNINetworkConfig(networkName, nadName, spec)
	if err != nil {
		return nil, fmt.Errorf("failed to render CNI network config: %w", err)
	}
	cniNetConfRaw, err := json.Marshal(cniNetConf)
	if err != nil {
		return nil, err
	}

	return &netv1.NetworkAttachmentDefinitionSpec{
		Config: string(cniNetConfRaw),
	}, nil
}

// renderNADLabels copies labels from UDN to help RenderNADSpec
// function add those labels to corresponding NAD
func renderNADLabels(obj client.Object) map[string]string {
	labels := make(map[string]string)
	labels[LabelUserDefinedNetwork] = ""
	udnLabels := obj.GetLabels()
	if len(udnLabels) != 0 {
		maps.Copy(labels, udnLabels)
	}
	return labels
}

// renderNADAnnotations copies annotations from UDN to corresponding NAD
func renderNADAnnotations(obj client.Object) map[string]string {
	udnAnnotations := obj.GetAnnotations()
	annotations := make(map[string]string)
	for k, v := range udnAnnotations {
		if !strings.HasPrefix(k, types.OvnK8sPrefix) {
			annotations[k] = v
		}
	}
	if len(annotations) == 0 {
		return nil
	}
	return annotations
}

func validateTopology(spec SpecGetter) error {
	if spec.GetTopology() == userdefinednetworkv1.NetworkTopologyLayer3 && spec.GetLayer3() == nil ||
		spec.GetTopology() == userdefinednetworkv1.NetworkTopologyLayer2 && spec.GetLayer2() == nil ||
		spec.GetTopology() == userdefinednetworkv1.NetworkTopologyLocalnet && spec.GetLocalnet() == nil {
		return config.NewTopologyConfigMismatchError(string(spec.GetTopology()))
	}
	return nil
}

func renderCNINetworkConfig(networkName, nadName string, spec SpecGetter) (map[string]interface{}, error) {
	netConfSpec := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			CNIVersion: cniVersion,
			Type:       OvnK8sCNIOverlay,
			Name:       networkName,
		},
		NADName:  nadName,
		Topology: strings.ToLower(string(spec.GetTopology())),
	}

	switch spec.GetTopology() {
	case userdefinednetworkv1.NetworkTopologyLayer3:
		cfg := spec.GetLayer3()
		netConfSpec.Role = strings.ToLower(string(cfg.Role))
		netConfSpec.MTU = int(cfg.MTU)
		netConfSpec.Subnets = layer3SubnetsString(cfg.Subnets)
		netConfSpec.JoinSubnet = cidrString(renderJoinSubnets(cfg.Role, cfg.JoinSubnets))
	case userdefinednetworkv1.NetworkTopologyLayer2:
		cfg := spec.GetLayer2()
		if err := validateIPAM(cfg.IPAM); err != nil {
			return nil, err
		}
		if ipamEnabled(cfg.IPAM) && len(cfg.Subnets) == 0 {
			return nil, config.NewSubnetsRequiredError()
		}
		if !ipamEnabled(cfg.IPAM) && len(cfg.Subnets) > 0 {
			return nil, config.NewSubnetsMustBeUnsetError()
		}

		netConfSpec.Role = strings.ToLower(string(cfg.Role))
		netConfSpec.MTU = int(cfg.MTU)
		netConfSpec.AllowPersistentIPs = cfg.IPAM != nil && cfg.IPAM.Lifecycle == userdefinednetworkv1.IPAMLifecyclePersistent
		netConfSpec.Subnets = cidrString(cfg.Subnets)
		if util.IsPreconfiguredUDNAddressesEnabled() {
			netConfSpec.ReservedSubnets = cidrString(cfg.ReservedSubnets)
			netConfSpec.InfrastructureSubnets = cidrString(cfg.InfrastructureSubnets)
			netConfSpec.DefaultGatewayIPs = ipString(cfg.DefaultGatewayIPs)
		}
		netConfSpec.JoinSubnet = cidrString(renderJoinSubnets(cfg.Role, cfg.JoinSubnets))
		// now generate transit subnet for layer2 topology
		if cfg.Role == userdefinednetworkv1.NetworkRolePrimary {
			err := setTransitSubnets(netConfSpec)
			if err != nil {
				return nil, err
			}
		}
	case userdefinednetworkv1.NetworkTopologyLocalnet:
		cfg := spec.GetLocalnet()
		netConfSpec.Role = strings.ToLower(string(cfg.Role))
		netConfSpec.MTU = localnetMTU(cfg.MTU)
		netConfSpec.AllowPersistentIPs = cfg.IPAM != nil && cfg.IPAM.Lifecycle == userdefinednetworkv1.IPAMLifecyclePersistent
		netConfSpec.Subnets = cidrString(cfg.Subnets)
		netConfSpec.ExcludeSubnets = cidrString(cfg.ExcludeSubnets)
		netConfSpec.PhysicalNetworkName = cfg.PhysicalNetworkName

		if cfg.VLAN != nil && cfg.VLAN.Access != nil {
			netConfSpec.VLANID = int(cfg.VLAN.Access.ID)
		}
	}
	if netConfSpec.AllowPersistentIPs && !config.OVNKubernetesFeature.EnablePersistentIPs {
		return nil, fmt.Errorf("allowPersistentIPs is set but persistentIPs is Disabled")
	}

	if err := util.ValidateNetConf(nadName, netConfSpec); err != nil {
		return nil, err
	}

	if _, err := util.NewNetInfo(netConfSpec); err != nil {
		return nil, err
	}

	// Since 'ovncnitypes.NetConf' type and its embedded 'cnitypes.NetConf' type has
	// parameters that defined with 'ommitempty' JSON tag option but not as pointer,
	// they will always present in the marshaed JSON, making the UDN NAD spec config
	// having unexpected fields (e.g.:IPAM, RuntimeConfig).
	// Generating the net-conf JSON string using 'map[string]struct{}' provide the
	// expected result.
	cniNetConf := map[string]interface{}{
		"cniVersion":       cniVersion,
		"type":             OvnK8sCNIOverlay,
		"name":             networkName,
		"netAttachDefName": nadName,
		"topology":         netConfSpec.Topology,
		"role":             netConfSpec.Role,
	}
	if mtu := netConfSpec.MTU; mtu > 0 {
		cniNetConf["mtu"] = mtu
	}
	if len(netConfSpec.JoinSubnet) > 0 {
		cniNetConf["joinSubnet"] = netConfSpec.JoinSubnet
	}
	if len(netConfSpec.TransitSubnet) > 0 {
		cniNetConf["transitSubnet"] = netConfSpec.TransitSubnet
	}
	if len(netConfSpec.Subnets) > 0 {
		cniNetConf["subnets"] = netConfSpec.Subnets
	}
	if netConfSpec.AllowPersistentIPs {
		cniNetConf["allowPersistentIPs"] = netConfSpec.AllowPersistentIPs
	}
	if netConfSpec.PhysicalNetworkName != "" {
		cniNetConf["physicalNetworkName"] = netConfSpec.PhysicalNetworkName
	}
	if len(netConfSpec.ExcludeSubnets) > 0 {
		cniNetConf["excludeSubnets"] = netConfSpec.ExcludeSubnets
	}

	if netConfSpec.VLANID != 0 {
		cniNetConf["vlanID"] = netConfSpec.VLANID
	}
	if util.IsPreconfiguredUDNAddressesEnabled() {
		if len(netConfSpec.ReservedSubnets) > 0 {
			cniNetConf["reservedSubnets"] = netConfSpec.ReservedSubnets
		}
		if len(netConfSpec.InfrastructureSubnets) > 0 {
			cniNetConf["infrastructureSubnets"] = netConfSpec.InfrastructureSubnets
		}
		if len(netConfSpec.DefaultGatewayIPs) > 0 {
			cniNetConf["defaultGatewayIPs"] = netConfSpec.DefaultGatewayIPs
		}
	}
	return cniNetConf, nil
}

func localnetMTU(desiredMTU int32) int {
	// The MTU for localnet topology should be as the default MTU (1500) because the underlay
	// is not part of the SDN and compensating for the SDN overhead (100) is not required.
	mtu := config.Default.MTU + 100
	if desiredMTU > 0 {
		mtu = int(desiredMTU)
	}

	return mtu
}

func ipamEnabled(ipam *userdefinednetworkv1.IPAMConfig) bool {
	return ipam == nil || ipam.Mode == "" || ipam.Mode == userdefinednetworkv1.IPAMEnabled
}

func validateIPAM(ipam *userdefinednetworkv1.IPAMConfig) error {
	if ipam == nil {
		return nil
	}
	if ipam.Lifecycle == userdefinednetworkv1.IPAMLifecyclePersistent && !ipamEnabled(ipam) {
		return config.NewIPAMLifecycleNotSupportedError()
	}
	return nil
}

func renderJoinSubnets(role userdefinednetworkv1.NetworkRole, joinSubnetes []userdefinednetworkv1.CIDR) []userdefinednetworkv1.CIDR {
	if role != userdefinednetworkv1.NetworkRolePrimary {
		return nil
	}

	if len(joinSubnetes) == 0 {
		return []userdefinednetworkv1.CIDR{types.UserDefinedPrimaryNetworkJoinSubnetV4, types.UserDefinedPrimaryNetworkJoinSubnetV6}
	}

	return joinSubnetes
}

// setTransitSubnets generates transit subnet for primary layer2 UDNs and sets for a given netconf.
// It should be called with the final version of netconf to make sure that util.SubnetOverlapCheck(netconf) passes.
func setTransitSubnets(netconf *ovncnitypes.NetConf) error {
	transitSubnets := []userdefinednetworkv1.CIDR{}
	for _, subnetStr := range strings.Split(netconf.Subnets, ",") {
		_, subnet, err := net.ParseCIDR(subnetStr)
		if err != nil {
			return fmt.Errorf("can't generate transit subnets: failed to parse CIDR %q: %w", subnetStr, err)
		}
		transitSubnet, err := getTransitSubnet(netconf, utilnet.IsIPv4CIDR(subnet))
		if err != nil {
			return err
		}
		transitSubnets = append(transitSubnets, userdefinednetworkv1.CIDR(transitSubnet))
	}
	netconf.TransitSubnet = cidrString(transitSubnets)
	return nil
}

func getTransitSubnet(netconf *ovncnitypes.NetConf, isIPv4 bool) (string, error) {
	var transitSubnet *net.IPNet
	var err error
	if isIPv4 {
		_, transitSubnet, err = net.ParseCIDR(config.ClusterManager.V4TransitSubnet)
	} else {
		_, transitSubnet, err = net.ParseCIDR(config.ClusterManager.V6TransitSubnet)
	}
	if err != nil {
		return "", fmt.Errorf("can't generate transit subnets: failed to parse default transit subnet: %w", err)
	}
	// repeat until we find a non-overlapping subnet,
	// but limit the number of iterations to avoid infinite loop
	for i := 0; i < 10; i++ {
		// only add current transit subnet to the netconf for overlap check, final assignment should be done for all ipFamilies
		netconf.TransitSubnet = transitSubnet.String()
		// check if there is subnet overlap
		subnet1, subnet2, err := util.SubnetOverlapCheck(netconf)
		if err == nil {
			return transitSubnet.String(), nil
		}
		if subnet1 == nil || subnet2 == nil || subnet1.String() != transitSubnet.String() && subnet2.String() != transitSubnet.String() {
			// there is another problem with the config
			// or overlap is not with transit subnet
			return "", err
		}
		transitSubnet = getFirstNonOverlappingSubnet(subnet1, subnet2, transitSubnet.Mask)
	}
	// if the previous loop didn't return the result, we failed to find a non-overlapping subnet
	return "", fmt.Errorf("can't generate transit subnets: failed to find non-overlapping transit subnet: %w", err)
}

func getFirstNonOverlappingSubnet(subnet1, subnet2 *net.IPNet, netMask net.IPMask) *net.IPNet {
	// find the bigger network, and get the first subnet outside of it with the same netmask as default transit subnet
	subnet1MaskSize, _ := subnet1.Mask.Size()
	subnet2MaskSize, _ := subnet2.Mask.Size()
	// bigger mask size means smaller network
	baseSubnet := subnet1
	if subnet2MaskSize < subnet1MaskSize {
		baseSubnet = subnet2
	}
	// now find the first subnet outside the baseSubnet with the same mask
	baseSubnetLastIP := util.GetLastIPOfSubnet(baseSubnet, 0)
	nextIP := iputils.NextIP(baseSubnetLastIP.IP)
	return &net.IPNet{IP: nextIP, Mask: netMask}
}

// layer3SubnetsString converts Layer3Subnet slice to comma seperated string
// (e.g.: "10.100.0.0/24/16, 10.200.0.0/24, ...").
// In case a Layer3Subent's HostSubnet is '0' or not specified it will not be
// appended becase it will result in an invalid format (e.g.: "10.200.0.0/24/0").
func layer3SubnetsString(subnets []userdefinednetworkv1.Layer3Subnet) string {
	var cidrs []string
	for _, subnet := range subnets {
		if subnet.HostSubnet > 0 {
			cidrs = append(cidrs, fmt.Sprintf("%s/%d", subnet.CIDR, subnet.HostSubnet))
		} else {
			cidrs = append(cidrs, string(subnet.CIDR))
		}
	}
	return strings.Join(cidrs, ",")
}

type cidr interface {
	userdefinednetworkv1.DualStackCIDRs | []userdefinednetworkv1.CIDR
}

func cidrString[T cidr](subnets T) string {
	var cidrs []string
	for _, subnet := range subnets {
		cidrs = append(cidrs, string(subnet))
	}
	return strings.Join(cidrs, ",")
}

func ipString(ips userdefinednetworkv1.DualStackIPs) string {
	var ipStrings []string
	for _, ip := range ips {
		ipStrings = append(ipStrings, string(ip))
	}
	return strings.Join(ipStrings, ",")
}

func GetSpec(obj client.Object) SpecGetter {
	switch o := obj.(type) {
	case *userdefinednetworkv1.UserDefinedNetwork:
		return &o.Spec
	case *userdefinednetworkv1.ClusterUserDefinedNetwork:
		return &o.Spec.Network
	default:
		panic(fmt.Sprintf("unknown type %T", obj))
	}
}
