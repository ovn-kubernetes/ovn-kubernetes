// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package nftables

import (
	"context"
	"fmt"
	"net"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"sigs.k8s.io/knftables"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	nftEgressIPChain = "ovn-kube-egress-ip"
)

// InitEgressIPNFTChain creates the nftables chain for egress IP traffic
// on secondary interface, so it makes sure no traffic with pod IP is leaked.
// This needs to be called once at controller startup.
// nodeSubnets are the pod CIDRs assigned to the local node; traffic from these
// subnets is accepted (Rule 0) while traffic from the wider cluster CIDRs is
// dropped (Rule 1) until proper SNAT rules are in place.
func InitEgressIPNFTChain(v4, v6 bool, nodeSubnets []*net.IPNet) error {
	// Get cluster-wide pod CIDRs from config
	podIPv4CIDRs, podIPv6CIDRs := util.GetClusterSubnets()
	if v4 && len(podIPv4CIDRs) == 0 {
		return fmt.Errorf("IPv4 enabled but no cluster pod CIDRs configured")
	}
	if v6 && len(podIPv6CIDRs) == 0 {
		return fmt.Errorf("IPv6 enabled but no cluster pod CIDRs configured")
	}
	klog.Infof("Using cluster pod CIDRs for egress IP traffic: IPv4=%v, IPv6=%v",
		podIPv4CIDRs, podIPv6CIDRs)

	var nodePodIPv4CIDRs, nodePodIPv6CIDRs []*net.IPNet
	for _, subnet := range nodeSubnets {
		if utilnet.IsIPv6CIDR(subnet) {
			nodePodIPv6CIDRs = append(nodePodIPv6CIDRs, subnet)
		} else {
			nodePodIPv4CIDRs = append(nodePodIPv4CIDRs, subnet)
		}
	}
	klog.Infof("Using local node pod CIDRs for egress IP accept rules: IPv4=%v, IPv6=%v",
		nodePodIPv4CIDRs, nodePodIPv6CIDRs)

	nft, err := GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper: %w", err)
	}
	tx := nft.NewTransaction()

	// Create the dedicated chain for egress IP traffic
	// Use priority 101 (after all SNAT rules at 100, before local gateway masquerade)
	eipChain := &knftables.Chain{
		Name:     nftEgressIPChain,
		Comment:  knftables.PtrTo("OVN egress IP traffic handling"),
		Type:     knftables.PtrTo(knftables.FilterType),
		Hook:     knftables.PtrTo(knftables.PostroutingHook),
		Priority: knftables.PtrTo(knftables.BaseChainPriority("srcnat + 1")),
	}
	tx.Add(eipChain)

	tx.Flush(eipChain)

	counterIfDebug := ""
	if config.Logging.Level > 1 {
		counterIfDebug = "counter"
	}

	dropPodIPv4 := &knftables.Rule{
		Chain: nftEgressIPChain,
		Rule: knftables.Concat(
			"oifname", "!=", "ovn-k8s-mp0",
			"return",
		),
	}
	tx.Add(dropPodIPv4)

	// Rule 0: Accept traffic from local node pod CIDRs in local gateway mode.
	// In LGW, pod traffic to external destinations leaves via the management port
	// and is routed through the host network stack, so it must not be dropped by
	// the cluster-wide Rule 1 below.
	if config.Gateway.Mode == config.GatewayModeLocal {
		if v4 {
			for _, cidr := range nodePodIPv4CIDRs {
				acceptNodePodIPv4 := &knftables.Rule{
					Chain: nftEgressIPChain,
					Rule: knftables.Concat(
						"ip", "saddr", cidr.String(),
						counterIfDebug,
						"accept",
						"comment", fmt.Sprintf("\"Accept egress IP pod traffic from %s\"", cidr),
					),
				}
				tx.Add(acceptNodePodIPv4)
			}
		}
		if v6 {
			for _, cidr := range nodePodIPv6CIDRs {
				acceptNodePodIPv6 := &knftables.Rule{
					Chain: nftEgressIPChain,
					Rule: knftables.Concat(
						"ip6", "saddr", cidr.String(),
						counterIfDebug,
						"accept",
						"comment", fmt.Sprintf("\"Accept egress IP pod traffic from %s\"", cidr),
					),
				}
				tx.Add(acceptNodePodIPv6)
			}
		}
	}

	// Rule 1: DROP if from any cluster pod CIDR
	// This prevents pod traffic from leaking before SNAT is ready
	if v4 {
		for _, cidr := range podIPv4CIDRs {
			dropPodIPv4 := &knftables.Rule{
				Chain: nftEgressIPChain,
				Rule: knftables.Concat(
					"ip", "saddr", cidr.String(),
					counterIfDebug,
					"drop",
					"comment", fmt.Sprintf("\"Drop egress IP pod traffic from %s\"", cidr),
				),
			}
			tx.Add(dropPodIPv4)
		}
	}
	if v6 {
		for _, cidr := range podIPv6CIDRs {
			dropPodIPv6 := &knftables.Rule{
				Chain: nftEgressIPChain,
				Rule: knftables.Concat(
					"ip6", "saddr", cidr.String(),
					counterIfDebug,
					"drop",
					"comment", fmt.Sprintf("\"Drop egress IP pod traffic from %s\"", cidr),
				),
			}
			tx.Add(dropPodIPv6)
		}
	}

	if err := nft.Run(context.TODO(), tx); err != nil {
		return fmt.Errorf("failed to setup egress IP nftables chain: %w", err)
	}
	return nil
}
