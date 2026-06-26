// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package egressip

import (
	"context"
	"fmt"
	"net"

	utilnet "k8s.io/utils/net"
	"sigs.k8s.io/knftables"

	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
)

const (
	// Chain names
	nftEgressIPUnreadyChain = "ovn-kube-egress-ip-unready"

	// Set names
	nftEgressIPReadyPodsV4 = "egressip-ready-pods-v4"
	nftEgressIPReadyPodsV6 = "egressip-ready-pods-v6"

	// Packet mark for unready egress IP traffic
	// Using decimal format (8192 = 0x2000) to match EgressIPNodeConnectionMark format
	egressIPUnreadyMark = "8192" // Bit 13 (0x2000 in hex)
)

// initEgressIPUnreadyNFTChain creates the nftables chain and sets for egress IP unready traffic
// This is called once at controller startup
func (c *Controller) initEgressIPUnreadyNFTChain() error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper: %w", err)
	}

	tx := nft.NewTransaction()

	// Create the dedicated chain for egress IP unready traffic
	// Use priority 100 (before local gateway masquerade at 101, but after egress IP SNAT)
	unreadyChain := &knftables.Chain{
		Name:     nftEgressIPUnreadyChain,
		Comment:  knftables.PtrTo("OVN egress IP unready traffic handling"),
		Type:     knftables.PtrTo(knftables.FilterType),
		Hook:     knftables.PtrTo(knftables.PostroutingHook),
		Priority: knftables.PtrTo(knftables.BaseChainPriority("mangle + 100")),
	}
	tx.Add(unreadyChain)

	// Create sets for ready pod IPs (one per IP family)
	if c.v4 {
		readyPodsSetV4 := &knftables.Set{
			Name:    nftEgressIPReadyPodsV4,
			Type:    "ipv4_addr",
			Comment: knftables.PtrTo("Pod IPs with ready egress IP SNAT"),
		}
		tx.Add(readyPodsSetV4)
	}

	if c.v6 {
		readyPodsSetV6 := &knftables.Set{
			Name:    nftEgressIPReadyPodsV6,
			Type:    "ipv6_addr",
			Comment: knftables.PtrTo("Pod IPs with ready egress IP SNAT"),
		}
		tx.Add(readyPodsSetV6)
	}

	// Flush chain to ensure clean state
	tx.Flush(unreadyChain)

	// Rule 1: CLEAR mark for ready pods
	// This MUST come before DROP rule
	if c.v4 {
		clearRuleV4 := &knftables.Rule{
			Chain: nftEgressIPUnreadyChain,
			Rule: knftables.Concat(
				"ip", "saddr", "@"+nftEgressIPReadyPodsV4,
				"meta", "mark", egressIPUnreadyMark,
				"meta", "mark", "set", "0",
				"comment", "\"Clear unready mark for ready egress IP pods\"",
			),
		}
		tx.Add(clearRuleV4)
	}

	if c.v6 {
		clearRuleV6 := &knftables.Rule{
			Chain: nftEgressIPUnreadyChain,
			Rule: knftables.Concat(
				"ip6", "saddr", "@"+nftEgressIPReadyPodsV6,
				"meta", "mark", egressIPUnreadyMark,
				"meta", "mark", "set", "0",
				"comment", "\"Clear unready mark for ready egress IP pods\"",
			),
		}
		tx.Add(clearRuleV6)
	}

	// Rule 2: DROP unready traffic
	// This catches any traffic that still has the mark (not in ready set)
	dropRule := &knftables.Rule{
		Chain: nftEgressIPUnreadyChain,
		Rule: knftables.Concat(
			"meta", "mark", egressIPUnreadyMark,
			"counter", // Count dropped packets for observability
			"drop",
			"comment", "\"Drop egress IP traffic until SNAT ready\"",
		),
	}
	tx.Add(dropRule)

	if err := nft.Run(context.TODO(), tx); err != nil {
		return fmt.Errorf("failed to setup egress IP unready nftables chain: %w", err)
	}

	return nil
}

// addPodIPToReadySet adds a pod IP to the ready pods set
// This signals that SNAT is configured and traffic can flow
func (c *Controller) addPodIPToReadySet(podIP net.IP) error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper: %w", err)
	}

	isIPv6 := utilnet.IsIPv6(podIP)
	setName := nftEgressIPReadyPodsV4
	if isIPv6 {
		setName = nftEgressIPReadyPodsV6
	}

	tx := nft.NewTransaction()

	// Add pod IP to the ready set
	elem := &knftables.Element{
		Set: setName,
		Key: []string{podIP.String()},
	}
	tx.Add(elem)

	if err := nft.Run(context.TODO(), tx); err != nil {
		return fmt.Errorf("failed to add pod IP %s to ready set: %w", podIP.String(), err)
	}

	return nil
}

// removePodIPFromReadySet removes a pod IP from the ready pods set
// This is called during cleanup
func (c *Controller) removePodIPFromReadySet(podIP net.IP) error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper: %w", err)
	}

	isIPv6 := utilnet.IsIPv6(podIP)
	setName := nftEgressIPReadyPodsV4
	if isIPv6 {
		setName = nftEgressIPReadyPodsV6
	}

	tx := nft.NewTransaction()

	// Remove pod IP from the ready set
	elem := &knftables.Element{
		Set: setName,
		Key: []string{podIP.String()},
	}
	tx.Delete(elem)

	if err := nft.Run(context.TODO(), tx); err != nil {
		return fmt.Errorf("failed to remove pod IP %s from ready set: %w", podIP.String(), err)
	}

	return nil
}

// getPodIPFromConfig extracts the pod IP from podIPConfig
// The pod IP is stored in the ipRule.Src field
func getPodIPFromConfig(config *podIPConfig) net.IP {
	if config == nil || config.ipRule.Src == nil {
		return nil
	}
	return config.ipRule.Src.IP
}
