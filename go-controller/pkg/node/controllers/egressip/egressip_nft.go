// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package egressip

import (
	"context"
	"fmt"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"sigs.k8s.io/knftables"

	ovnconfig "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
)

const (
	// Chain names
	nftEgressIPUnreadyChain = "ovn-kube-egress-ip-unready"

	// Packet mark for unready egress IP traffic
	// Using decimal format (8192 = 0x2000) to match EgressIPNodeConnectionMark format
	egressIPUnreadyMark = "8192" // Bit 13 (0x2000 in hex)
)

// initEgressIPUnreadyNFTChain creates the nftables chain for egress IP unready traffic
// Uses cluster pod CIDR instead of per-pod address sets for simplicity
// This is called once at controller startup
func (c *Controller) initEgressIPUnreadyNFTChain() error {
	// Get cluster-wide pod CIDRs from config
	podIPv4CIDRs, podIPv6CIDRs, err := c.getClusterPodCIDRs()
	if err != nil {
		return fmt.Errorf("failed to get cluster pod CIDRs: %w", err)
	}
	if c.v4 && len(podIPv4CIDRs) == 0 {
		return fmt.Errorf("IPv4 enabled but no cluster pod CIDRs configured")
	}
	if c.v6 && len(podIPv6CIDRs) == 0 {
		return fmt.Errorf("IPv6 enabled but no cluster pod CIDRs configured")
	}
	klog.Infof("Using cluster pod CIDRs for egress IP unready traffic: IPv4=%v, IPv6=%v",
		podIPv4CIDRs, podIPv6CIDRs)

	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper: %w", err)
	}

	tx := nft.NewTransaction()

	// Create the dedicated chain for egress IP unready traffic
	// Use priority 101 (after all SNAT rules at 100, before local gateway masquerade)
	unreadyChain := &knftables.Chain{
		Name:     nftEgressIPUnreadyChain,
		Comment:  knftables.PtrTo("OVN egress IP unready traffic handling"),
		Type:     knftables.PtrTo(knftables.FilterType),
		Hook:     knftables.PtrTo(knftables.PostroutingHook),
		Priority: knftables.PtrTo(knftables.BaseChainPriority("srcnat + 1")),
	}
	tx.Add(unreadyChain)

	// Flush chain to ensure clean state
	tx.Flush(unreadyChain)

	// Rule 1: DROP if marked AND from any cluster pod CIDR
	// Create one rule per CIDR (typically just one CIDR per family)
	// This prevents pod traffic from leaking before SNAT is ready
	if c.v4 {
		for _, cidr := range podIPv4CIDRs {
			dropPodIPv4 := &knftables.Rule{
				Chain: nftEgressIPUnreadyChain,
				Rule: knftables.Concat(
					"ip", "saddr", cidr,
					"meta", "mark", egressIPUnreadyMark,
					"counter",
					"drop",
					"comment", fmt.Sprintf("\"Drop unready egress IP pod traffic from %s\"", cidr),
				),
			}
			tx.Add(dropPodIPv4)
		}
	}

	if c.v6 {
		for _, cidr := range podIPv6CIDRs {
			dropPodIPv6 := &knftables.Rule{
				Chain: nftEgressIPUnreadyChain,
				Rule: knftables.Concat(
					"ip6", "saddr", cidr,
					"meta", "mark", egressIPUnreadyMark,
					"counter",
					"drop",
					"comment", fmt.Sprintf("\"Drop unready egress IP pod traffic from %s\"", cidr),
				),
			}
			tx.Add(dropPodIPv6)
		}
	}

	// Rule 2: CLEAR mark unconditionally (safety net for non-pod traffic)
	// If SNAT is ready, it will have already cleared the mark
	// This rule only catches edge cases where non-pod traffic has the mark
	clearRule := &knftables.Rule{
		Chain: nftEgressIPUnreadyChain,
		Rule: knftables.Concat(
			"meta", "mark", egressIPUnreadyMark,
			"meta", "mark", "set", "0",
			"comment", "\"Clear unready mark for non-pod traffic\"",
		),
	}
	tx.Add(clearRule)

	if err := nft.Run(context.TODO(), tx); err != nil {
		return fmt.Errorf("failed to setup egress IP unready nftables chain: %w", err)
	}

	return nil
}

// getClusterPodCIDRs returns the cluster-wide pod CIDRs from config
// These are the subnets from which ALL pod IPs in the cluster are allocated
func (c *Controller) getClusterPodCIDRs() ([]string, []string, error) {
	var ipv4CIDRs, ipv6CIDRs []string

	// Get cluster subnets from global config
	// This is configured at cluster setup time (e.g., --cluster-subnets=10.244.0.0/16)
	for _, subnet := range ovnconfig.Default.ClusterSubnets {
		cidrStr := subnet.CIDR.String()
		if utilnet.IsIPv4CIDRString(cidrStr) {
			ipv4CIDRs = append(ipv4CIDRs, cidrStr)
		} else if utilnet.IsIPv6CIDRString(cidrStr) {
			ipv6CIDRs = append(ipv6CIDRs, cidrStr)
		}
	}

	if len(ipv4CIDRs) == 0 && len(ipv6CIDRs) == 0 {
		return nil, nil, fmt.Errorf("no cluster pod CIDRs configured")
	}

	return ipv4CIDRs, ipv6CIDRs, nil
}
