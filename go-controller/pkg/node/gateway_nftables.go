//go:build linux
// +build linux

package node

import (
	"context"
	"fmt"
	"net"
	"strings"

	corev1 "k8s.io/api/core/v1"
	utilnet "k8s.io/utils/net"
	"sigs.k8s.io/knftables"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// gateway_nftables.go contains code for dealing with nftables rules; it is used in
// conjunction with gateway_iptables.go.
//
// For the most part, using a mix of iptables and nftables rules does not matter, since
// both of them are handled by netfilter. However, in cases where there is a close
// ordering dependency between two rules (especially, in any case where it's necessary to
// use an "accept" rule to override a later "drop" rule), then those rules will need to
// either both be iptables or both be nftables.

// nftables chain names
const (
	nftablesLocalGatewayMasqChain = "ovn-kube-local-gw-masq"
	nftablesPodSubnetMasqChain    = "ovn-kube-pod-subnet-masq"
	nftablesUDNMasqChain          = "ovn-kube-udn-masq"
)

// getNoSNATNodePortRules returns elements to add to the "mgmtport-no-snat-nodeports"
// set to prevent SNAT of sourceIP when passing through the management port, for an
// `externalTrafficPolicy: Local` service with NodePorts.
func getNoSNATNodePortRules(svcPort corev1.ServicePort) []*knftables.Element {
	return []*knftables.Element{
		{
			Set: types.NFTMgmtPortNoSNATNodePorts,
			Key: []string{
				strings.ToLower(string(svcPort.Protocol)),
				fmt.Sprintf("%d", svcPort.NodePort),
			},
		},
	}
}

// getNoSNATLoadBalancerIPRules returns elements to add to the
// "mgmtport-no-snat-services-v4" and "mgmtport-no-snat-services-v6" sets to prevent SNAT
// of sourceIP when passing through the management port, for an `externalTrafficPolicy:
// Local` service *without* NodePorts.
func getNoSNATLoadBalancerIPRules(svcPort corev1.ServicePort, localEndpoints util.PortToLBEndpoints) []*knftables.Element {
	var nftRules []*knftables.Element
	protocol := strings.ToLower(string(svcPort.Protocol))

	// Get the endpoints for the port key.
	// svcPortKey is of format e.g. "TCP/my-port-name" or "TCP/" if name is empty
	// (is the case when only a single ServicePort is defined on this service).
	svcPortKey := util.GetServicePortKey(svcPort.Protocol, svcPort.Name)
	lbEndpoints := localEndpoints[svcPortKey]

	for _, destination := range lbEndpoints.GetV4Destinations() {
		nftRules = append(nftRules,
			&knftables.Element{
				Set: types.NFTMgmtPortNoSNATServicesV4,
				Key: []string{destination.IP, protocol, fmt.Sprintf("%d", destination.Port)},
			},
		)
	}

	for _, destination := range lbEndpoints.GetV6Destinations() {
		nftRules = append(nftRules,
			&knftables.Element{
				Set: types.NFTMgmtPortNoSNATServicesV6,
				Key: []string{destination.IP, protocol, fmt.Sprintf("%d", destination.Port)},
			},
		)
	}

	return nftRules
}

// getUDNNodePortMarkNFTRule returns a verdict map element (nftablesUDNMarkNodePortsMap)
// with a key composed of the svcPort protocol and port.
// The value is a jump to the UDN chain mark if netInfo is provided, or nil that is useful for map entry removal.
func getUDNNodePortMarkNFTRule(svcPort corev1.ServicePort, netInfo *bridgeconfig.BridgeUDNConfiguration) *knftables.Element {
	var val []string
	if netInfo != nil {
		val = []string{fmt.Sprintf("jump %s", GetUDNMarkChain(netInfo.PktMark))}
	}
	return &knftables.Element{
		Map:   nftablesUDNMarkNodePortsMap,
		Key:   []string{strings.ToLower(string(svcPort.Protocol)), fmt.Sprintf("%v", svcPort.NodePort)},
		Value: val,
	}

}

// getUDNExternalIPsMarkNFTRules returns a verdict map elements (nftablesUDNMarkExternalIPsV4Map or nftablesUDNMarkExternalIPsV6Map)
// with a key composed of the external IP, svcPort protocol and port.
// The value is a jump to the UDN chain mark if netInfo is provided,  or nil that is useful for map entry removal.
func getUDNExternalIPsMarkNFTRules(svcPort corev1.ServicePort, externalIPs []string, netInfo *bridgeconfig.BridgeUDNConfiguration) []*knftables.Element {
	var nftRules []*knftables.Element
	var val []string

	if netInfo != nil {
		val = []string{fmt.Sprintf("jump %s", GetUDNMarkChain(netInfo.PktMark))}
	}
	for _, externalIP := range externalIPs {
		mapName := nftablesUDNMarkExternalIPsV4Map
		if utilnet.IsIPv6String(externalIP) {
			mapName = nftablesUDNMarkExternalIPsV6Map
		}
		nftRules = append(nftRules,
			&knftables.Element{
				Map:   mapName,
				Key:   []string{externalIP, strings.ToLower(string(svcPort.Protocol)), fmt.Sprintf("%v", svcPort.Port)},
				Value: val,
			},
		)

	}
	return nftRules
}

func recreateNFTSet(setName string, keepNFTElems []*knftables.Element) error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}
	tx := nft.NewTransaction()
	tx.Flush(&knftables.Set{
		Name: setName,
	})
	for _, elem := range keepNFTElems {
		if elem.Set == setName {
			tx.Add(elem)
		}
	}
	err = nft.Run(context.TODO(), tx)
	// no error if set is not created and we desire zero NFT elements
	if knftables.IsNotFound(err) && len(keepNFTElems) == 0 {
		return nil
	}
	return err
}

func recreateNFTMap(mapName string, keepNFTElems []*knftables.Element) error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}
	tx := nft.NewTransaction()
	tx.Flush(&knftables.Map{
		Name: mapName,
	})
	for _, elem := range keepNFTElems {
		if elem.Map == mapName {
			tx.Add(elem)
		}
	}
	err = nft.Run(context.TODO(), tx)
	// no error if set is not created and we desire zero NFT elements
	if knftables.IsNotFound(err) && len(keepNFTElems) == 0 {
		return nil
	}
	return err
}

// getGatewayNFTRules returns nftables rules for service. This must be used in conjunction
// with getGatewayIPTRules.
func getGatewayNFTRules(service *corev1.Service, localEndpoints util.PortToLBEndpoints, svcHasLocalHostNetEndPnt bool) []*knftables.Element {
	rules := make([]*knftables.Element, 0)
	svcTypeIsETPLocal := util.ServiceExternalTrafficPolicyLocal(service)
	for _, svcPort := range service.Spec.Ports {
		if svcTypeIsETPLocal && !svcHasLocalHostNetEndPnt {
			// For `externalTrafficPolicy: Local` services with pod-network
			// endpoints, we need to add rules to prevent them from being SNATted
			// when entering the management port, to preserve the client IP.
			if util.ServiceTypeHasNodePort(service) {
				rules = append(rules, getNoSNATNodePortRules(svcPort)...)
			} else if len(util.GetExternalAndLBIPs(service)) > 0 {
				rules = append(rules, getNoSNATLoadBalancerIPRules(svcPort, localEndpoints)...)
			}
		}
	}
	return rules
}

// getUDNNFTRules generates nftables rules for a UDN service.
// If netConfig is nil, the resulting map elements will have empty values,
// suitable only for entry removal.
func getUDNNFTRules(service *corev1.Service, netConfig *bridgeconfig.BridgeUDNConfiguration) []*knftables.Element {
	rules := make([]*knftables.Element, 0)
	for _, svcPort := range service.Spec.Ports {
		if util.ServiceTypeHasNodePort(service) {
			rules = append(rules, getUDNNodePortMarkNFTRule(svcPort, netConfig))
		}
		rules = append(rules, getUDNExternalIPsMarkNFTRules(svcPort, util.GetExternalAndLBIPs(service), netConfig)...)
	}
	return rules
}

// getLocalGatewayPodSubnetMasqueradeNFTRule creates a rule for masquerading traffic from the pod subnet CIDR
// in local gateway node in a seperate chain which is then called from local gateway masquerade chain.
//
//	chain ovn-kube-pod-subnet-masq {
//		ip saddr 10.244.0.0/24 masquerade
//		ip6 saddr fd00:10:244:1::/64 masquerade
//	}
//
// If isAdvertisedNetwork is true, masquerade only when destination matches remote node IPs.
// Rules look like:
// ip saddr 10.244.0.0/24 ip daddr @remote-node-ips-v4 masquerade
// ip6 saddr fd00:10:244:1::/64 ip6 daddr @remote-node-ips-v6 masquerade
func getLocalGatewayPodSubnetMasqueradeNFTRule(cidr *net.IPNet, isAdvertisedNetwork bool) (*knftables.Rule, error) {
	// Create the rule for masquerading traffic from the CIDR
	var ipPrefix string
	var remoteNodeSetName string
	if utilnet.IsIPv6CIDR(cidr) {
		ipPrefix = "ip6"
		remoteNodeSetName = types.NFTRemoteNodeIPsv6
	} else {
		ipPrefix = "ip"
		remoteNodeSetName = types.NFTRemoteNodeIPsv4
	}

	// If network is advertised, only masquerade if destination is a remote node IP
	var optionalDestRules []string
	if isAdvertisedNetwork {
		optionalDestRules = []string{ipPrefix, "daddr", "@", remoteNodeSetName}
	}
	rule := &knftables.Rule{
		Rule: knftables.Concat(
			ipPrefix, "saddr", cidr,
			optionalDestRules,
			"masquerade",
		),
		Chain: nftablesPodSubnetMasqChain,
	}

	return rule, nil
}

// getLocalGatewayNATNFTRules returns the nftables rules for local gateway NAT including masquerade IP rule,
// pod subnet rules, and UDN masquerade rules (if network segmentation is enabled).
// This function supports dual-stack by accepting multiple CIDRs and generating rules for all IP families.
//
//	chain ovn-kube-local-gw-masq {
//		comment "OVN local gateway masquerade"
//		type nat hook postrouting priority srcnat; policy accept;
//		ip saddr 169.254.0.1 masquerade
//		ip6 saddr fd69::1 masquerade
//		jump ovn-kube-pod-subnet-masq
//		jump ovn-kube-udn-masq
//	}
func getLocalGatewayNATNFTRules(cidrs ...*net.IPNet) ([]*knftables.Rule, error) {
	var rules []*knftables.Rule

	// Process each CIDR to support dual-stack
	for _, cidr := range cidrs {
		// Determine IP version and masquerade IP
		isIPv6 := utilnet.IsIPv6CIDR(cidr)
		var masqueradeIP net.IP
		var ipPrefix string
		if isIPv6 {
			masqueradeIP = config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP
			ipPrefix = "ip6"
		} else {
			masqueradeIP = config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP
			ipPrefix = "ip"
		}

		// Rule1: Masquerade IP rule for the main chain
		masqRule := &knftables.Rule{
			Chain: nftablesLocalGatewayMasqChain,
			Rule: knftables.Concat(
				ipPrefix, "saddr", masqueradeIP,
				"masquerade",
			),
		}
		rules = append(rules, masqRule)

		// Rule2: Pod subnet NAT rule for the pod subnet chain
		podSubnetRule, err := getLocalGatewayPodSubnetMasqueradeNFTRule(cidr, false)
		if err != nil {
			return nil, fmt.Errorf("failed to create pod subnet masquerade rule: %w", err)
		}
		rules = append(rules, podSubnetRule)
	}

	// Rule 3: UDN masquerade rules (if network segmentation is enabled)
	if util.IsNetworkSegmentationSupportEnabled() {
		if config.IPv4Mode {
			udnRules, err := getUDNMasqueradeNFTRules(utilnet.IPv4)
			if err != nil {
				return nil, fmt.Errorf("failed to create IPv4 UDN masquerade rules: %w", err)
			}
			rules = append(rules, udnRules...)
		}
		if config.IPv6Mode {
			udnRules, err := getUDNMasqueradeNFTRules(utilnet.IPv6)
			if err != nil {
				return nil, fmt.Errorf("failed to create IPv6 UDN masquerade rules: %w", err)
			}
			rules = append(rules, udnRules...)
		}
	}

	return rules, nil
}

// getUDNMasqueradeNFTRules returns the nftables rules for UDN masquerade.
// Chain creation is handled separately by setupLocalGatewayNATNFTRules.
//
//	chain ovn-kube-udn-masq {
//		comment "OVN UDN masquerade"
//		ip saddr != 169.254.0.0/29 ip daddr != 10.96.0.0/16 ip saddr 169.254.0.0/17 masquerade
//		ip6 saddr != fd69::/125 ip daddr != fd00:10:96::/112 ip6 saddr fd69::/112 masquerade
//	}
func getUDNMasqueradeNFTRules(ipFamily utilnet.IPFamily) ([]*knftables.Rule, error) {
	var rules []*knftables.Rule

	// Determine subnet and IP family
	srcUDNMasqueradePrefix := config.Gateway.V4MasqueradeSubnet
	ipPrefix := "ip"
	if ipFamily == utilnet.IPv6 {
		srcUDNMasqueradePrefix = config.Gateway.V6MasqueradeSubnet
		ipPrefix = "ip6"
	}

	// Calculate reserved masquerade prefix (first 8 IPs)
	_, ipnet, err := net.ParseCIDR(srcUDNMasqueradePrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to parse UDN masquerade subnet: %w", err)
	}
	_, prefixLen := ipnet.Mask.Size()
	defaultNetworkReservedMasqueradePrefix := fmt.Sprintf("%s/%d", ipnet.IP.String(), prefixLen-3)

	// Rule: RETURN for reserved masquerade prefix and service CIDRs
	// rest of the traffic is masqueraded

	for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
		if utilnet.IPFamilyOfCIDR(svcCIDR) != ipFamily {
			continue
		}
		masqueradeRule := &knftables.Rule{
			Chain: nftablesUDNMasqChain,
			Rule: knftables.Concat(
				ipPrefix, "saddr", "!=", defaultNetworkReservedMasqueradePrefix, // this guarantees we don't SNAT default network masqueradeIPs
				ipPrefix, "daddr", "!=", svcCIDR, // this guarantees we don't SNAT service traffic
				ipPrefix, "saddr", srcUDNMasqueradePrefix, // this guarantees we SNAT all UDN MasqueradeIPs traffic leaving the node
				"masquerade",
			),
		}
		rules = append(rules, masqueradeRule)
	}

	return rules, nil
}

// initLocalGatewayNFTNATRules sets up nftables rules for local gateway NAT functionality
// This function supports dual-stack by accepting multiple CIDRs and generating rules for all IP families
func initLocalGatewayNFTNATRules(cidrs ...*net.IPNet) error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper: %w", err)
	}

	// Create transaction and apply all chains and rules
	tx := nft.NewTransaction()

	// Create main local gateway masquerade chain
	// Use priority 101 instead of defaultknftables.SNATPriority (100) to ensure
	// iptables egress IP rules in OVN-KUBE-EGRESS-IP-MULTI-NIC chain run first
	// this also ensure for egress-services, the
	// 	chain egress-services {
	//	type nat hook postrouting priority srcnat; policy accept;
	// is called before the local gateway masquerade chain
	localGwMasqChain := &knftables.Chain{
		Name:     nftablesLocalGatewayMasqChain,
		Comment:  knftables.PtrTo("OVN local gateway masquerade"),
		Type:     knftables.PtrTo(knftables.NATType),
		Hook:     knftables.PtrTo(knftables.PostroutingHook),
		Priority: knftables.PtrTo(knftables.BaseChainPriority("101")),
	}
	tx.Add(localGwMasqChain)

	// Create dedicated pod subnet masquerade chain
	podSubnetMasqChain := &knftables.Chain{
		Name: nftablesPodSubnetMasqChain,
	}
	tx.Add(podSubnetMasqChain)

	// Create UDN masquerade chain only if network segmentation is enabled
	var udnMasqChain *knftables.Chain
	if util.IsNetworkSegmentationSupportEnabled() {
		udnMasqChain = &knftables.Chain{
			Name:    nftablesUDNMasqChain,
			Comment: knftables.PtrTo("OVN UDN masquerade"),
		}
		tx.Add(udnMasqChain)
	}

	// Flush existing chains to ensure clean state
	tx.Flush(localGwMasqChain)
	tx.Flush(podSubnetMasqChain)
	if util.IsNetworkSegmentationSupportEnabled() {
		tx.Flush(udnMasqChain)
	}

	// Get the existing local gateway NAT rules
	localGwRules, err := getLocalGatewayNATNFTRules(cidrs...)
	if err != nil {
		return fmt.Errorf("failed to get local gateway NAT rules: %w", err)
	}

	// Add the main local gateway NAT rules
	for _, rule := range localGwRules {
		tx.Add(rule)
	}

	// Add jump rule from main chain to pod subnet chain
	jumpToPodSubnetRule := &knftables.Rule{
		Chain: nftablesLocalGatewayMasqChain,
		Rule: knftables.Concat(
			"jump", nftablesPodSubnetMasqChain,
		),
	}
	tx.Add(jumpToPodSubnetRule)

	// Add jump rule to UDN chain only if network segmentation is enabled
	if util.IsNetworkSegmentationSupportEnabled() {
		jumpToUDNRule := &knftables.Rule{
			Chain: nftablesLocalGatewayMasqChain,
			Rule: knftables.Concat(
				"jump", nftablesUDNMasqChain,
			),
		}
		tx.Add(jumpToUDNRule)
	}

	err = nft.Run(context.TODO(), tx)
	if err != nil {
		return fmt.Errorf("failed to setup local gateway NAT nftables rules: %w", err)
	}

	return nil
}

// addOrUpdateLocalGatewayPodSubnetNFTRules adds nftables rules for pod subnet masquerading for multiple CIDRs
// These rules are added to the dedicated pod subnet masquerade chain.
// If the rules already exist, they are updated.
// If isAdvertisedNetwork is true, the masquerade rules also get a destination match
// that matches the remote node IP set.
func addOrUpdateLocalGatewayPodSubnetNFTRules(isAdvertisedNetwork bool, cidrs ...*net.IPNet) error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper: %w", err)
	}

	tx := nft.NewTransaction()

	// Ensure the pod subnet chain exists
	podSubnetChain := &knftables.Chain{
		Name: nftablesPodSubnetMasqChain,
	}
	tx.Add(podSubnetChain)

	// Flush the chain to remove all existing rules
	// if network toggles between advertised and non-advertised, we need to flush the chain and re-add correct rules
	tx.Flush(podSubnetChain)

	// Add the new rules for each CIDR
	for _, cidr := range cidrs {
		rule, err := getLocalGatewayPodSubnetMasqueradeNFTRule(cidr, isAdvertisedNetwork)
		if err != nil {
			return fmt.Errorf("failed to create nftables rules for CIDR %s: %w", cidr.String(), err)
		}

		// Add the rule
		tx.Add(rule)
	}

	if err := nft.Run(context.TODO(), tx); err != nil {
		return fmt.Errorf("failed to add pod subnet NAT rules: %w", err)
	}

	return nil
}

// delLocalGatewayPodSubnetNFTRules removes nftables rules for pod subnet masquerading for multiple CIDRs
// Since we use a separate chain, we can simply flush it to remove all pod subnet rules.
func delLocalGatewayPodSubnetNFTRules() error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper: %w", err)
	}

	tx := nft.NewTransaction()

	// In shared gateway mode, this chain might not exist if its
	// not migration from local gateway mode. In that case, let's
	// use the idiomatic way of adding the chain before trying to flush it.
	// I anyways also have the knftables.IsNotFound() check in the caller later.
	tx.Add(&knftables.Chain{
		Name: nftablesPodSubnetMasqChain,
	})

	// Simply flush the dedicated pod subnet masquerade chain
	// This removes all pod subnet masquerade rules at once
	tx.Flush(&knftables.Chain{Name: nftablesPodSubnetMasqChain})

	if err := nft.Run(context.TODO(), tx); err != nil && !knftables.IsNotFound(err) {
		return fmt.Errorf("failed to delete pod subnet NAT rules: %w", err)
	}

	return nil
}
