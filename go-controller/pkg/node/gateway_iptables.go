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
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

const (
	iptableNodePortChain   = "OVN-KUBE-NODEPORT"   // called from nat-PREROUTING and nat-OUTPUT
	iptableExternalIPChain = "OVN-KUBE-EXTERNALIP" // called from nat-PREROUTING and nat-OUTPUT
	iptableETPChain        = "OVN-KUBE-ETP"        // called from nat-PREROUTING only
	iptableITPChain        = "OVN-KUBE-ITP"        // only cleaned up; never used
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
	iptRules := []nodeipt.Rule{
		{
			Table:    "nat",
			Chain:    "PREROUTING",
			Args:     []string{"-j", chain},
			Protocol: proto,
		},
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

// getNodePortIPTRules returns the IPTable DNAT rules for a service of type nodePort
// `svcPort` corresponds to port details for this service as specified in the service object
// `targetIP` is clusterIP towards which the DNAT of nodePort service is to be added
// `targetPort` is the port towards which the DNAT of the nodePort service is to be added
//
//	case1: if svcHasLocalHostNetEndPnt=false + isETPLocal=true targetIP=config.masqueradeIP["HostETPLocalMasqueradeIP"] and targetPort=svcPort.NodePort
//	case2: default: targetIP=clusterIP and targetPort=svcPort.Port
//
// `svcHasLocalHostNetEndPnt` is true if this service has at least one host-networked endpoint that is local to this node
// `isETPLocal` is true if the svc.Spec.ExternalTrafficPolicy=Local
func getNodePortIPTRules(svcPort corev1.ServicePort, targetIP string, targetPort int32, svcHasLocalHostNetEndPnt, isETPLocal bool) []nodeipt.Rule {
	chainName := iptableNodePortChain
	if !svcHasLocalHostNetEndPnt && isETPLocal {
		// DNAT it to the masqueradeIP:nodePort instead of clusterIP:targetPort
		targetIP = getMasqueradeVIP(targetIP)
		chainName = iptableETPChain
	}
	return []nodeipt.Rule{
		{
			Table: "nat",
			Chain: chainName,
			Args: []string{
				"-p", string(svcPort.Protocol),
				"-m", "addrtype",
				"--dst-type", "LOCAL",
				"--dport", fmt.Sprintf("%d", svcPort.NodePort),
				"-j", "DNAT",
				"--to-destination", util.JoinHostPortInt32(targetIP, targetPort),
			},
			Protocol: getIPTablesProtocol(targetIP),
		},
	}
}

func computeProbability(n, i int) string {
	return fmt.Sprintf("%0.10f", 1.0/float64(n-i+1))
}

// generateIPTRulesForLoadBalancersWithoutNodePorts generates iptables DNAT rules for load balancer services
// without NodePort allocation. It performs statistical load balancing between endpoints via iptables.
func generateIPTRulesForLoadBalancersWithoutNodePorts(svcPort corev1.ServicePort, externalIP string, localEndpoints util.PortToLBEndpoints) []nodeipt.Rule {
	if len(localEndpoints) == 0 {
		// either its smart nic mode; etp&itp not implemented, OR
		// fetching endpointSlices error-ed out prior to reaching here so nothing to do
		return []nodeipt.Rule{}
	}

	// Get the endpoints for the port key.
	// svcPortKey is of format e.g. "TCP/my-port-name" or "TCP/" if name is empty
	// (is the case when only a single ServicePort is defined on this service).
	svcPortKey := util.GetServicePortKey(svcPort.Protocol, svcPort.Name)
	lbEndpoints := localEndpoints[svcPortKey]

	// Get IPv4 or IPv6 IPs, depending on the type of the service's external IP.
	destinations := lbEndpoints.GetV4Destinations()
	if utilnet.IsIPv6String(externalIP) {
		destinations = lbEndpoints.GetV6Destinations()
	}

	numLocalEndpoints := len(destinations)
	iptRules := make([]nodeipt.Rule, 0, numLocalEndpoints)
	for i, destination := range destinations {
		iptRules = append([]nodeipt.Rule{
			{
				Table: "nat",
				Chain: iptableETPChain,
				Args: []string{
					"-p", string(svcPort.Protocol),
					"-d", externalIP,
					"--dport", fmt.Sprintf("%v", svcPort.Port),
					"-j", "DNAT",
					"--to-destination", util.JoinHostPortInt32(destination.IP, destination.Port),
					"-m", "statistic",
					"--mode", "random",
					"--probability", computeProbability(numLocalEndpoints, i+1),
				},
				Protocol: getIPTablesProtocol(externalIP),
			},
		}, iptRules...)
	}
	return iptRules
}

// getExternalIPTRules returns the IPTable DNAT rules for a service of type LB or ExternalIP
// `svcPort` corresponds to port details for this service as specified in the service object
// `externalIP` can either be the externalIP or LB.status.ingressIP
// `dstIP` corresponds to the IP to which the provided externalIP needs to be DNAT-ed to
//
//	case1: if svcHasLocalHostNetEndPnt=false + isETPLocal=true, dstIP=config.MasqueradeIP["HostETPLocalMasqueradeIP"]
//	case2: default: dstIP=clusterIP
//
// `svcHasLocalHostNetEndPnt` is true if this service has at least one host-networked endpoint that is local to this node
// `isETPLocal` is true if the svc.Spec.ExternalTrafficPolicy=Local
func getExternalIPTRules(svcPort corev1.ServicePort, externalIP, dstIP string, svcHasLocalHostNetEndPnt, isETPLocal bool) []nodeipt.Rule {
	targetPort := svcPort.Port
	chainName := iptableExternalIPChain
	if !svcHasLocalHostNetEndPnt && isETPLocal {
		// DNAT it to the masqueradeIP:nodePort instead of clusterIP:targetPort
		dstIP = getMasqueradeVIP(externalIP)
		targetPort = svcPort.NodePort
		chainName = iptableETPChain
	}
	return []nodeipt.Rule{
		{
			Table: "nat",
			Chain: chainName,
			Args: []string{
				"-p", string(svcPort.Protocol),
				"-d", externalIP,
				"--dport", fmt.Sprintf("%v", svcPort.Port),
				"-j", "DNAT",
				"--to-destination", util.JoinHostPortInt32(dstIP, targetPort),
			},
			Protocol: getIPTablesProtocol(externalIP),
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
	for _, chain := range []string{iptableNodePortChain, iptableExternalIPChain, iptableETPChain} {
		for _, proto := range clusterIPTablesProtocols() {
			ipt, err := util.GetIPTablesHelper(proto)
			if err != nil {
				return err
			}
			addChaintoTable(ipt, "nat", chain)
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

func cleanupGatewayServiceIPTables() {
	// We clean up both IPv4 and IPv6, regardless of what is currently in use
	for _, proto := range []iptables.Protocol{iptables.ProtocolIPv4, iptables.ProtocolIPv6} {
		ipt, err := util.GetIPTablesHelper(proto)
		if err != nil {
			continue
		}
		for _, chain := range []string{iptableITPChain} {
			_ = ipt.ClearChain("nat", chain)
			_ = ipt.DeleteChain("nat", chain)
		}
		_ = ipt.ClearChain("mangle", iptableITPChain)
		_ = ipt.DeleteChain("mangle", iptableITPChain)
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
// case1: If !svcHasLocalHostNetEndPnt and svcTypeIsETPLocal rules that redirect traffic
// to ovn-k8s-mp0 preserving sourceIP are added.
//
// case2: (default) A DNAT rule towards clusterIP svc is added ALWAYS.
func getGatewayIPTRules(service *corev1.Service, localEndpoints util.PortToLBEndpoints, svcHasLocalHostNetEndPnt bool) []nodeipt.Rule {
	rules := make([]nodeipt.Rule, 0)
	clusterIPs := util.GetClusterIPs(service)
	svcTypeIsETPLocal := util.ServiceExternalTrafficPolicyLocal(service)
	for _, svcPort := range service.Spec.Ports {
		if util.ServiceTypeHasNodePort(service) {
			err := util.ValidatePort(svcPort.Protocol, svcPort.NodePort)
			if err != nil {
				klog.Errorf("Skipping service: %s, invalid service NodePort: %v", svcPort.Name, err)
				continue
			}
			err = util.ValidatePort(svcPort.Protocol, svcPort.Port)
			if err != nil {
				klog.Errorf("Skipping service: %s, invalid service port %v", svcPort.Name, err)
				continue
			}
			for _, clusterIP := range clusterIPs {
				if svcTypeIsETPLocal && !svcHasLocalHostNetEndPnt {
					// case1 (see function description for details)
					// A DNAT rule to masqueradeIP is added that takes priority over DNAT to clusterIP.
					if config.Gateway.Mode == config.GatewayModeLocal {
						rules = append(rules, getNodePortIPTRules(svcPort, clusterIP, svcPort.NodePort, svcHasLocalHostNetEndPnt, svcTypeIsETPLocal)...)
					}
					// Note: getGatewayNFTRules will add rules to ensure that sourceIP is preserved
				}
				// case2 (see function description for details)
				rules = append(rules, getNodePortIPTRules(svcPort, clusterIP, svcPort.Port, svcHasLocalHostNetEndPnt, false)...)
			}
		}

		externalIPs := util.GetExternalAndLBIPs(service)

		for _, externalIP := range externalIPs {
			err := util.ValidatePort(svcPort.Protocol, svcPort.Port)
			if err != nil {
				klog.Errorf("Skipping service: %s, invalid service port %v", svcPort.Name, err)
				continue
			}
			if clusterIP, err := util.MatchIPStringFamily(utilnet.IsIPv6String(externalIP), clusterIPs); err == nil {
				if svcTypeIsETPLocal && !svcHasLocalHostNetEndPnt {
					// case1 (see function description for details)
					// DNAT traffic to masqueradeIP:nodePort instead of clusterIP:Port. We are leveraging the existing rules for NODEPORT
					// service so no need to add a rule to skip SNAT since the corresponding nodePort svc would have one.
					if !util.ServiceTypeHasNodePort(service) {
						rules = append(rules, generateIPTRulesForLoadBalancersWithoutNodePorts(svcPort, externalIP, localEndpoints)...)
					} else {
						rules = append(rules, getExternalIPTRules(svcPort, externalIP, "", svcHasLocalHostNetEndPnt, svcTypeIsETPLocal)...)
					}
				}
				// case2 (see function description for details)
				rules = append(rules, getExternalIPTRules(svcPort, externalIP, clusterIP, svcHasLocalHostNetEndPnt, false)...)
			}
		}
	}
	return rules
}
