// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package node

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/managementport"
)

func initLocalGateway(mgmtPort managementport.Interface, advertised bool) error {
	if config.IsModeDPU() {
		return nil
	}

	klog.Info("Adding iptables/nftables rules for new local gateway")

	// Set up iptables filter rules to allow traffic to/from the management port.
	ifName := mgmtPort.GetInterfaceName()
	if err := initLocalGatewayIPTFilterRules(ifName); err != nil {
		return fmt.Errorf("failed to configure local forwarding rules for: %s, err: %v", ifName, err)
	}

	allCIDRs := getLocalGatewaySNATSubnets()
	if len(allCIDRs) > 0 {
		if err := initLocalGatewayNFTNATRules(advertised, allCIDRs...); err != nil {
			return fmt.Errorf("failed to setup nftables masquerade rules: %w", err)
		}
	}

	return nil
}

// getLocalGatewaySNATSubnets includes retained VM addresses from other nodes in
// local gateway mode. It is used only by the default network gateway;
// UDN management-port SNAT keeps its existing scope.
func getLocalGatewaySNATSubnets() []*net.IPNet {
	subnets := make([]*net.IPNet, 0, len(config.Default.ClusterSubnets))
	for _, subnet := range config.Default.ClusterSubnets {
		subnets = append(subnets, subnet.CIDR)
	}
	return subnets
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

func cleanupLocalnetGateway(ovsClient libovsdbclient.Client, physnet string) error {
	if config.IsModeDPUHost() {
		return nil
	}
	ovs, err := ovsops.GetOpenvSwitch(ovsClient)
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			// Nothing configured yet — nothing to clean up.
			return nil
		}
		return fmt.Errorf("failed to get Open_vSwitch row: %w", err)
	}
	mappings := ovs.ExternalIDs["ovn-bridge-mappings"]
	if mappings == "" {
		return nil
	}
	for _, bridgeMapping := range strings.Split(mappings, ",") {
		m := strings.SplitN(bridgeMapping, ":", 2)
		if len(m) != 2 || m[1] == "" {
			klog.Warningf("Ignoring malformed ovn-bridge-mappings entry %q", bridgeMapping)
			continue
		}
		if physnet == m[0] {
			bridgeName := m[1]
			if err := ovsops.DeleteBridge(ovsClient, bridgeName); err != nil {
				return fmt.Errorf("failed to delete bridge %s: %w", bridgeName, err)
			}
			break
		}
	}
	return nil
}
