// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package node

import (
	"fmt"
	"net"

	"github.com/coreos/go-iptables/iptables"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodeipt "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iptables"
	nodeutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

const (
	iptableNodePortChain   = "OVN-KUBE-NODEPORT"   // called from nat-PREROUTING and nat-OUTPUT
	iptableExternalIPChain = "OVN-KUBE-EXTERNALIP" // called from nat-PREROUTING and nat-OUTPUT
	iptableETPChain        = "OVN-KUBE-ETP"        // called from nat-PREROUTING only
	iptableITPChain        = "OVN-KUBE-ITP"        // called from mangle-OUTPUT and nat-OUTPUT
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

// getMasqueradeVIP returns the .3 masquerade VIP based on the protocol (v4/v6) of provided IP string
func getMasqueradeVIP(ip string) string {
	if utilnet.IsIPv6String(ip) {
		return config.Gateway.MasqueradeIPs.V6HostETPLocalMasqueradeIP.String()
	}
	return config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String()
}

// insertIptRules adds the provided rules in an insert fashion
// i.e each rule gets added at the first position in the chain
func insertIptRules(rules []nodeipt.Rule) error {
	return nodeipt.AddRules(rules, false)
}

// restoreIptRulesFiltered restores the provided rules in an insert fashion with a filter for table/chain
// i.e each rule gets added at the first position in the chain
// filter is defined as a map of table/chains. Only rules matching this filter will be restored.
// If no rules match the filter, the chain will still be restored as empty as specified in the filter.
func restoreIptRulesFiltered(rules []nodeipt.Rule, filter map[string]map[string]struct{}) error {
	return nodeipt.RestoreRulesFiltered(rules, filter)
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

// getITPLocalIPTRules returns the IPTable REDIRECT or MARK rules for the provided service
// `svcPort` corresponds to port details for this service as specified in the service object
// `clusterIP` is clusterIP is the VIP of the service to match on
// `svcHasLocalHostNetEndPnt` is true if this service has at least one host-networked endpoint that is local to this node
// NOTE: Currently invoked only for Internal Traffic Policy
func getITPLocalIPTRules(svcPort corev1.ServicePort, clusterIP string, svcHasLocalHostNetEndPnt bool) []nodeipt.Rule {
	if svcHasLocalHostNetEndPnt {
		return []nodeipt.Rule{
			{
				Table: "nat",
				Chain: iptableITPChain,
				Args: []string{
					"-p", string(svcPort.Protocol),
					"-d", clusterIP,
					"--dport", fmt.Sprintf("%v", svcPort.Port),
					"-j", "REDIRECT",
					"--to-port", fmt.Sprintf("%v", int32(svcPort.TargetPort.IntValue())),
				},
				Protocol: getIPTablesProtocol(clusterIP),
			},
		}
	}
	return []nodeipt.Rule{
		{
			Table: "mangle",
			Chain: iptableITPChain,
			Args: []string{
				"-p", string(svcPort.Protocol),
				"-d", string(clusterIP),
				"--dport", fmt.Sprintf("%d", svcPort.Port),
				"-j", "MARK",
				"--set-xmark", string(types.OVNKubeITPMark),
			},
			Protocol: getIPTablesProtocol(clusterIP),
		},
	}
}

func getGatewayForwardRules(cidrs []*net.IPNet) map[iptables.Protocol][]nodeipt.Rule {
	returnRules := map[iptables.Protocol][]nodeipt.Rule{}

	// Add rules for all CIDRs.
	for _, cidr := range cidrs {
		protocol := getIPTablesProtocol(cidr.IP.String())
		returnRules[protocol] = append(returnRules[protocol], []nodeipt.Rule{
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
	for protocol := range returnRules {
		masqueradeIP := config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP
		if protocol == iptables.ProtocolIPv6 {
			masqueradeIP = config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP
		}
		returnRules[protocol] = append(returnRules[protocol], getMasqueradeIpTablesForwardRules(masqueradeIP, protocol)...)
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

// initExternalBridgeForwardingRules adds or removes iptables rules for br-* interface svc
// traffic forwarding:
// -A FORWARD -s 10.96.0.0/16 -j ACCEPT
// -A FORWARD -d 10.96.0.0/16 -j ACCEPT
// -A FORWARD -s 169.254.169.1 -j ACCEPT
// -A FORWARD -d 169.254.169.1 -j ACCEPT
func initExternalBridgeServiceForwardingRules(cidrs []*net.IPNet) error {
	allRules := getGatewayForwardRules(cidrs)
	for protocol, rules := range allRules {
		var err error
		if nodeutil.NeedIPTablesForwardingRules(protocol) {
			err = insertIptRules(rules)
		} else {
			err = deleteIptRules(rules)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func getLocalGatewayFilterRules(ifname string, protocol iptables.Protocol) []nodeipt.Rule {
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
	}
}

// initLocalGatewayIPTFilterRules creates or deletes iptables forward rules for the
// management port.
func initLocalGatewayIPTFilterRules(ifname string) error {
	for _, protocol := range clusterIPTablesProtocols() {
		rules := getLocalGatewayFilterRules(ifname, protocol)
		if nodeutil.NeedIPTablesForwardingRules(protocol) {
			// Insert (rather than append) the filter table rules because they
			// need to be evaluated BEFORE the DROP rules we have for
			// forwarding. DO NOT change the ordering; especially important
			// during SGW->LGW rollouts and restarts.
			err := insertIptRules(rules)
			if err != nil {
				return fmt.Errorf("unable to insert forwarding rules %v", err)
			}
		} else {
			err := deleteIptRules(rules)
			if err != nil {
				return fmt.Errorf("unable to clean up stale forwarding rules %v", err)
			}
		}
	}
	return nil
}

func addChaintoTable(ipt util.IPTablesHelper, tableName, chain string) {
	if err := ipt.NewChain(tableName, chain); err != nil {
		klog.V(5).Infof("Chain: \"%s\" in table: \"%s\" already exists, skipping creation: %v", chain, tableName, err)
	}
}

func handleGatewayIPTables(iptCallback func(rules []nodeipt.Rule) error, genGatewayChainRules func(chain string, proto iptables.Protocol) []nodeipt.Rule) error {
	rules := make([]nodeipt.Rule, 0)
	// (NOTE: Order is important, add jump to iptableETPChain before jump to NP/EIP chains)
	for _, chain := range []string{iptableITPChain, iptableNodePortChain, iptableExternalIPChain, iptableETPChain} {
		for _, proto := range clusterIPTablesProtocols() {
			ipt, err := util.GetIPTablesHelper(proto)
			if err != nil {
				return err
			}
			addChaintoTable(ipt, "nat", chain)
			if chain == iptableITPChain {
				addChaintoTable(ipt, "mangle", chain)
			}
			rules = append(rules, genGatewayChainRules(chain, proto)...)
		}
	}
	if err := iptCallback(rules); err != nil {
		return fmt.Errorf("failed to handle iptables rules %v: %v", rules, err)
	}
	return nil
}

func initSharedGatewayIPTables() error {
	if err := handleGatewayIPTables(insertIptRules, getGatewayInitRules); err != nil {
		return err
	}
	return nil
}

func initLocalGatewayIPTables() error {
	if err := handleGatewayIPTables(insertIptRules, getGatewayInitRules); err != nil {
		return err
	}
	return nil
}

func cleanupSharedGatewayIPTChains() {
	for _, chain := range []string{iptableNodePortChain, iptableExternalIPChain} {
		// We clean up both IPv4 and IPv6, regardless of what is currently in use
		for _, proto := range []iptables.Protocol{iptables.ProtocolIPv4, iptables.ProtocolIPv6} {
			ipt, err := util.GetIPTablesHelper(proto)
			if err != nil {
				return
			}
			_ = ipt.ClearChain("nat", chain)
			_ = ipt.DeleteChain("nat", chain)
		}
	}
}

func recreateIPTRules(table, chain string, keepIPTRules []nodeipt.Rule) error {
	var errors []error
	var err error
	klog.Infof("Recreating iptables rules for table: %s, chain: %s", table, chain)
	// filter is a map of the table/chain to program rules for, as all rules are included in keepIPTRules
	filter := map[string]map[string]struct{}{table: {chain: {}}}
	if err = restoreIptRulesFiltered(keepIPTRules, filter); err != nil {
		errors = append(errors, err)
	}
	return utilerrors.Join(errors...)
}

// getGatewayIPTRules returns ClusterIP, NodePort, ExternalIP and LoadBalancer iptables
// rules for service. This must be used in conjunction with getGatewayNFTRules.
//
// case3: if svcHasLocalHostNetEndPnt and svcTypeIsITPLocal, rule that redirects clusterIP traffic to host targetPort is added.
//
//	if !svcHasLocalHostNetEndPnt and svcTypeIsITPLocal, rule that marks clusterIP traffic to steer it to ovn-k8s-mp0 is added.
func getGatewayIPTRules(service *corev1.Service, localEndpoints util.PortToLBEndpoints, svcHasLocalHostNetEndPnt bool) []nodeipt.Rule {
	rules := make([]nodeipt.Rule, 0)
	clusterIPs := util.GetClusterIPs(service)
	svcTypeIsITPLocal := util.ServiceInternalTrafficPolicyLocal(service)
	for _, svcPort := range service.Spec.Ports {
		if svcTypeIsITPLocal {
			// case3 (see function decription for details)
			for _, clusterIP := range clusterIPs {
				rules = append(rules, getITPLocalIPTRules(svcPort, clusterIP, svcHasLocalHostNetEndPnt)...)
			}
		}
	}
	return rules
}
