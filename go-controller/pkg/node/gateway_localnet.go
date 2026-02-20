//go:build linux
// +build linux

package node

import (
	"fmt"
	"net"
	"strings"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/managementport"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func initLocalGateway(hostSubnets []*net.IPNet, mgmtPort managementport.Interface) error {
	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		return nil
	}

	klog.Info("Adding iptables masquerading rules for new local gateway")

	var allCIDRs []*net.IPNet
	ifName := mgmtPort.GetInterfaceName()

	// First pass: collect all CIDRs and setup iptables filter rules per interface
	for _, hostSubnet := range hostSubnets {
		// local gateway mode uses mp0 as default path for all ingress traffic into OVN
		nextHop, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6CIDR(hostSubnet), mgmtPort.GetAddresses())
		if err != nil {
			return fmt.Errorf("failed to find management port address: %w", err)
		}

		// add iptables masquerading for mp0 to exit the host for egress
		cidr := nextHop.IP.Mask(nextHop.Mask)
		cidrNet := &net.IPNet{IP: cidr, Mask: nextHop.Mask}
		allCIDRs = append(allCIDRs, cidrNet)

		// Setup iptables filter rules for this interface/CIDR
		if err := initLocalGatewayIPTFilterRules(ifName, cidrNet); err != nil {
			return fmt.Errorf("failed to add local NAT rules for: %s, err: %v", ifName, err)
		}
	}

	// setup nftables masquerade rules for all CIDRs (v4, v6 or dualstack)
	if len(allCIDRs) > 0 {
		if err := initLocalGatewayNFTNATRules(allCIDRs...); err != nil {
			return fmt.Errorf("failed to setup nftables masquerade rules: %w", err)
		}
	}

	return nil
}

func getGatewayFamilyAddrs(gatewayIfAddrs []*net.IPNet) (string, string) {
	var gatewayIPv4, gatewayIPv6 string
	for _, gatewayIfAddr := range gatewayIfAddrs {
		if utilnet.IsIPv6(gatewayIfAddr.IP) {
			gatewayIPv6 = gatewayIfAddr.IP.String()
		} else {
			gatewayIPv4 = gatewayIfAddr.IP.String()
		}
	}
	return gatewayIPv4, gatewayIPv6
}

func getLocalAddrs() (map[string]net.IPNet, error) {
	localAddrSet := make(map[string]net.IPNet)
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		ip, ipNet, err := net.ParseCIDR(addr.String())
		if err != nil {
			return nil, err
		}
		localAddrSet[ip.String()] = *ipNet
	}
	klog.V(5).Infof("Node local addresses initialized to: %v", localAddrSet)
	return localAddrSet, nil
}

func cleanupLocalnetGateway(physnet string) error {
	if config.OvnKubeNode.Mode == types.NodeModeDPUHost {
		return nil
	}
	stdout, stderr, err := util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".",
		"external_ids:ovn-bridge-mappings")
	if err != nil {
		return fmt.Errorf("failed to get ovn-bridge-mappings stderr:%s (%v)", stderr, err)
	}
	bridgeMappings := strings.Split(stdout, ",")
	for _, bridgeMapping := range bridgeMappings {
		m := strings.Split(bridgeMapping, ":")
		if physnet == m[0] {
			bridgeName := m[1]
			_, stderr, err = util.RunOVSVsctl("--", "--if-exists", "del-br", bridgeName)
			if err != nil {
				return fmt.Errorf("failed to ovs-vsctl del-br %s stderr:%s (%v)", bridgeName, stderr, err)
			}
			break
		}
	}
	return err
}
