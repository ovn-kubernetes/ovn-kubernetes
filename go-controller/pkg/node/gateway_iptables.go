// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package node

import (
	"fmt"
	"net"

	"github.com/coreos/go-iptables/iptables"

	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodeipt "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iptables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// Legacy iptables service chains; only cleaned up; never used
	iptableNodePortChain   = "OVN-KUBE-NODEPORT"
	iptableExternalIPChain = "OVN-KUBE-EXTERNALIP"
	iptableETPChain        = "OVN-KUBE-ETP"
	iptableITPChain        = "OVN-KUBE-ITP"
)

func clusterIPTablesProtocols() []iptables.Protocol {
	var protocols []iptables.Protocol
	if config.IPv4Mode {
		protocols = append(protocols, iptables.ProtocolIPv4)
	}
	if config.IPv6Mode {
		protocols = append(protocols, iptables.ProtocolIPv6)
	}
	return protocols
}

// getIPTablesProtocol returns the IPTables protocol matching the protocol (v4/v6) of provided IP string
func getIPTablesProtocol(ip string) iptables.Protocol {
	if utilnet.IsIPv6String(ip) {
		return iptables.ProtocolIPv6
	}
	return iptables.ProtocolIPv4
}

// insertIptRules adds the provided rules in an insert fashion
// i.e each rule gets added at the first position in the chain
func insertIptRules(rules []nodeipt.Rule) error {
	return nodeipt.AddRules(rules, false)
}

// deleteIptRules removes provided rules from the chain
func deleteIptRules(rules []nodeipt.Rule) error {
	return nodeipt.DelRules(rules)
}

func getGatewayInitRules(chain string, proto iptables.Protocol) []nodeipt.Rule {
	iptRules := []nodeipt.Rule{}
	if chain == iptableITPChain {
		iptRules = append(iptRules,
			nodeipt.Rule{
				Table:    "mangle",
				Chain:    "OUTPUT",
				Args:     []string{"-j", chain},
				Protocol: proto,
			},
		)
	} else {
		iptRules = append(iptRules,
			nodeipt.Rule{
				Table:    "nat",
				Chain:    "PREROUTING",
				Args:     []string{"-j", chain},
				Protocol: proto,
			},
		)
	}
	if chain != iptableETPChain { // ETP chain only meant for external traffic
		iptRules = append(iptRules,
			nodeipt.Rule{
				Table:    "nat",
				Chain:    "OUTPUT",
				Args:     []string{"-j", chain},
				Protocol: proto,
			},
		)
	}
	return iptRules
}

func getGatewayForwardRules(cidrs []*net.IPNet) []nodeipt.Rule {
	var returnRules []nodeipt.Rule
	protocols := make(map[iptables.Protocol]struct{})

	// Add rules for all CIDRs.
	for _, cidr := range cidrs {
		protocol := getIPTablesProtocol(cidr.IP.String())
		protocols[protocol] = struct{}{}

		returnRules = append(returnRules, []nodeipt.Rule{
			{
				Table: "filter",
				Chain: "FORWARD",
				Args: []string{
					"-s", cidr.String(),
					"-j", "ACCEPT",
				},
				Protocol: protocol,
			},
			{
				Table: "filter",
				Chain: "FORWARD",
				Args: []string{
					"-d", cidr.String(),
					"-j", "ACCEPT",
				},
				Protocol: protocol,
			},
		}...)
	}

	// Add rules for MasqueraIPs.
	for protocol := range protocols {
		masqueradeIP := config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP
		if protocol == iptables.ProtocolIPv6 {
			masqueradeIP = config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP
		}
		returnRules = append(returnRules, getMasqueradeIpTablesForwardRules(masqueradeIP, protocol)...)
	}

	return returnRules
}

// getStaleMasqueradeIptablesRules returns all iptables rules may get added for a given masquerade IP.
func getStaleMasqueradeIptablesRules(masqueradeIP net.IP) []nodeipt.Rule {
	return append(getMasqueradeIpTablesForwardRules(masqueradeIP, getIPTablesProtocol(masqueradeIP.String())),
		getMasqueradeIpTablesNATRules(masqueradeIP, getIPTablesProtocol(masqueradeIP.String()))...)
}

func getMasqueradeIpTablesForwardRules(masqueradeIP net.IP, protocol iptables.Protocol) []nodeipt.Rule {
	return []nodeipt.Rule{
		{
			Table: "filter",
			Chain: "FORWARD",
			Args: []string{
				"-s", masqueradeIP.String(),
				"-j", "ACCEPT",
			},
			Protocol: protocol,
		},
		{
			Table: "filter",
			Chain: "FORWARD",
			Args: []string{
				"-d", masqueradeIP.String(),
				"-j", "ACCEPT",
			},
			Protocol: protocol,
		},
	}
}

func getMasqueradeIpTablesNATRules(masqueradeIP net.IP, protocol iptables.Protocol) []nodeipt.Rule {
	return []nodeipt.Rule{
		{
			Table: "nat",
			Chain: "POSTROUTING",
			Args: []string{
				"-s", masqueradeIP.String(),
				"-j", "MASQUERADE",
			},
			Protocol: protocol,
		},
	}
}

// initExternalBridgeForwardingRules sets up iptables rules for br-* interface svc traffic forwarding
// -A FORWARD -s 10.96.0.0/16 -j ACCEPT
// -A FORWARD -d 10.96.0.0/16 -j ACCEPT
// -A FORWARD -s 169.254.169.1 -j ACCEPT
// -A FORWARD -d 169.254.169.1 -j ACCEPT
func initExternalBridgeServiceForwardingRules(cidrs []*net.IPNet) error {
	return insertIptRules(getGatewayForwardRules(cidrs))
}

// delExternalBridgeServiceForwardingRules removes iptables rules which might
// have been added to disable forwarding
func delExternalBridgeServiceForwardingRules(cidrs []*net.IPNet) error {
	return deleteIptRules(getGatewayForwardRules(cidrs))
}

func getLocalGatewayFilterRules(ifname string, cidr *net.IPNet) []nodeipt.Rule {
	// Allow packets to/from the gateway interface in case defaults deny
	protocol := getIPTablesProtocol(cidr.IP.String())
	return []nodeipt.Rule{
		{
			Table: "filter",
			Chain: "FORWARD",
			Args: []string{
				"-o", ifname,
				"-j", "ACCEPT",
			},
			Protocol: protocol,
		},
		{
			Table: "filter",
			Chain: "FORWARD",
			Args: []string{
				"-i", ifname,
				"-j", "ACCEPT",
			},
			Protocol: protocol,
		},
		{
			Table: "filter",
			Chain: "INPUT",
			Args: []string{
				"-i", ifname,
				"-m", "comment", "--comment", "from OVN to localhost",
				"-j", "ACCEPT",
			},
			Protocol: protocol,
		},
	}
}

// initLocalGatewayIPTFilterRules sets up iptables rules for interfaces
func initLocalGatewayIPTFilterRules(ifname string, cidr *net.IPNet) error {
	// Insert the filter table rules because they need to be evaluated BEFORE the DROP rules
	// we have for forwarding. DO NOT change the ordering; specially important
	// during SGW->LGW rollouts and restarts.
	err := insertIptRules(getLocalGatewayFilterRules(ifname, cidr))
	if err != nil {
		return fmt.Errorf("unable to insert forwarding rules %v", err)
	}
	// NOTE: nftables masquerade rules are now handled separately in initLocalGatewayNFTNATRules
	return nil
}

func cleanupGatewayIPTables() error {
	rules := make([]nodeipt.Rule, 0)
	// We clean up both IPv4 and IPv6, regardless of what is currently in use
	for _, proto := range []iptables.Protocol{iptables.ProtocolIPv4, iptables.ProtocolIPv6} {
		ipt, err := util.GetIPTablesHelper(proto)
		if err != nil {
			continue
		}
		for _, chain := range []string{iptableITPChain, iptableNodePortChain, iptableExternalIPChain, iptableETPChain} {
			rules = append(rules, getGatewayInitRules(chain, proto)...)
		}
		if err := nodeipt.DelRules(rules); err != nil {
			return fmt.Errorf("failed to clean up stale iptables rules %v: %w", rules, err)
		}
		for _, chain := range []string{iptableITPChain, iptableNodePortChain, iptableExternalIPChain, iptableETPChain} {
			_ = ipt.ClearChain("nat", chain)
			_ = ipt.DeleteChain("nat", chain)
			if chain == iptableITPChain {
				_ = ipt.ClearChain("mangle", chain)
				_ = ipt.DeleteChain("mangle", chain)
			}
		}
	}
	return nil
}
