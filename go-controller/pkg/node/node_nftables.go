// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"fmt"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"sigs.k8s.io/knftables"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

const nftPMTUDChain = "no-pmtud"
const nftEgressIPDropChain = "egressip-drop"

// setupRemoteNodeNFTSets sets up the NFT sets that contain remote Kubernetes node IPs
func setupRemoteNodeNFTSets() error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper: %w", err)
	}

	tx := nft.NewTransaction()
	tx.Add(&knftables.Set{
		Name:    types.NFTRemoteNodeIPsv4,
		Comment: knftables.PtrTo("Block egress ICMP needs frag to remote Kubernetes nodes"),
		Type:    "ipv4_addr",
	})
	tx.Add(&knftables.Set{
		Name:    types.NFTRemoteNodeIPsv6,
		Comment: knftables.PtrTo("Block egress ICMPv6 packet too big to remote Kubernetes nodes"),
		Type:    "ipv6_addr",
	})

	err = nft.Run(context.TODO(), tx)
	if err != nil {
		return fmt.Errorf("could not add nftables sets for pmtud blocking: %v", err)
	}
	return nil
}

// setupNoOverlaySNATExemptNFTSets sets up the NFT sets for no-overlay SNAT exemption.
// These sets contain cluster CIDRs + local zone node IPs that should be exempted from SNAT.
// Caller should check if no-overlay mode with outbound SNAT is enabled before calling this.
func setupNoOverlaySNATExemptNFTSets() error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper: %w", err)
	}

	tx := nft.NewTransaction()
	if config.IPv4Mode {
		tx.Add(&knftables.Set{
			Name:    types.NFTNoOverlaySNATExemptV4,
			Comment: knftables.PtrTo("No-overlay SNAT exemption: cluster CIDRs + local zone node IPv4 addresses"),
			Type:    "ipv4_addr",
			Flags:   []knftables.SetFlag{knftables.IntervalFlag}, // Support CIDR ranges
		})
	}
	if config.IPv6Mode {
		tx.Add(&knftables.Set{
			Name:    types.NFTNoOverlaySNATExemptV6,
			Comment: knftables.PtrTo("No-overlay SNAT exemption: cluster CIDRs + local zone node IPv6 addresses"),
			Type:    "ipv6_addr",
			Flags:   []knftables.SetFlag{knftables.IntervalFlag}, // Support CIDR ranges
		})
	}

	err = nft.Run(context.TODO(), tx)
	if err != nil {
		return fmt.Errorf("could not add nftables sets for no-overlay SNAT exemption: %v", err)
	}
	return nil
}

// syncNoOverlaySNATExemptNFTSets updates the nftables sets for no-overlay SNAT exemption
// with cluster CIDRs + local zone node IPs. This should be called when the local node is added or updated.
func syncNoOverlaySNATExemptNFTSets(localNodeIPs []string) error {
	// Calculate desired addresses: cluster CIDRs + local node IPs
	var desiredIPv4Addrs, desiredIPv6Addrs []string

	// Add cluster CIDRs
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		cidrStr := clusterSubnet.CIDR.String()
		if config.IPv4Mode && clusterSubnet.CIDR.IP.To4() != nil {
			desiredIPv4Addrs = append(desiredIPv4Addrs, cidrStr)
		} else if config.IPv6Mode {
			desiredIPv6Addrs = append(desiredIPv6Addrs, cidrStr)
		}
	}

	// Add local node IPs
	for _, nodeIP := range localNodeIPs {
		if config.IPv4Mode && !utilnet.IsIPv6String(nodeIP) {
			desiredIPv4Addrs = append(desiredIPv4Addrs, nodeIP)
		} else if config.IPv6Mode && utilnet.IsIPv6String(nodeIP) {
			desiredIPv6Addrs = append(desiredIPv6Addrs, nodeIP)
		}
	}

	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper: %w", err)
	}

	tx := nft.NewTransaction()

	// Update IPv4 set if needed
	if config.IPv4Mode && len(desiredIPv4Addrs) > 0 {
		// Flush and repopulate the set
		tx.Flush(&knftables.Set{
			Name: types.NFTNoOverlaySNATExemptV4,
		})
		for _, addr := range desiredIPv4Addrs {
			tx.Add(&knftables.Element{
				Set: types.NFTNoOverlaySNATExemptV4,
				Key: []string{addr},
			})
		}
	}

	// Update IPv6 set if needed
	if config.IPv6Mode && len(desiredIPv6Addrs) > 0 {
		// Flush and repopulate the set
		tx.Flush(&knftables.Set{
			Name: types.NFTNoOverlaySNATExemptV6,
		})
		for _, addr := range desiredIPv6Addrs {
			tx.Add(&knftables.Element{
				Set: types.NFTNoOverlaySNATExemptV6,
				Key: []string{addr},
			})
		}
	}

	err = nft.Run(context.TODO(), tx)
	if err != nil {
		return fmt.Errorf("failed to sync no-overlay SNAT exemption nftables sets: %w", err)
	}

	return nil
}

// setupPMTUDNFTChain sets up the chain and rules to block PMTUD packets from being sent to k8s nodes
// Relies on the sets from setupPMTUDNFTSets.
func setupPMTUDNFTChain() error {
	counterIfDebug := ""
	if config.Logging.Level > 4 {
		counterIfDebug = "counter"
	}

	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper")
	}

	tx := nft.NewTransaction()
	tx.Add(&knftables.Chain{
		Name:     nftPMTUDChain,
		Comment:  knftables.PtrTo("Block egress needs frag/packet too big to remote k8s nodes"),
		Type:     knftables.PtrTo(knftables.FilterType),
		Hook:     knftables.PtrTo(knftables.OutputHook),
		Priority: knftables.PtrTo(knftables.FilterPriority),
	})

	tx.Flush(&knftables.Chain{
		Name: nftPMTUDChain,
	})
	if config.IPv4Mode {
		tx.Add(&knftables.Rule{
			Chain: nftPMTUDChain,
			Rule: knftables.Concat(
				"ip daddr @"+types.NFTRemoteNodeIPsv4,
				"meta l4proto icmp",
				"icmp type 3", // type 3 == Destination Unreachable
				"icmp code 4", // code 4 indicates fragmentation needed
				counterIfDebug,
				"drop",
			),
		})
	}

	if config.IPv6Mode {
		tx.Add(&knftables.Rule{
			Chain: nftPMTUDChain, // your egress chain for IPv6 traffic
			Rule: knftables.Concat(
				"meta l4proto icmpv6", // match on ICMPv6 packets
				"icmpv6 type 2",       // type 2 == Packet Too Big (PMTUD)
				"icmpv6 code 0",       // code 0 for that message
				"ip6 daddr @"+types.NFTRemoteNodeIPsv6,
				counterIfDebug,
				"drop", // drop the packet
			),
		})
	}

	err = nft.Run(context.TODO(), tx)
	if err != nil {
		return fmt.Errorf("could not update nftables rule for PMTUD: %v", err)
	}
	return nil
}

// SetupEgressIPARPBlockNFTables sets up the complete nftables configuration (table, set, chain, and rules)
// for blocking ARP/NDP responses for egress IPs during graceful shutdown.
func SetupEgressIPARPBlockNFTables(egressIPs []string) error {
	if len(egressIPs) == 0 {
		klog.V(5).Info("No egress IPs to setup ARP block rules for")
		return nil
	}

	var ipv4Addrs, ipv6Addrs []string
	for _, ipStr := range egressIPs {
		if !utilnet.IsIPv6String(ipStr) {
			ipv4Addrs = append(ipv4Addrs, ipStr)
		} else {
			ipv6Addrs = append(ipv6Addrs, ipStr)
		}
	}

	if config.IPv4Mode && len(ipv4Addrs) > 0 {
		nft, err := knftables.New(knftables.ARPFamily, nodenft.OVNKubernetesEgressIPv4NFTablesName)
		if err != nil {
			return fmt.Errorf("failed to create egress IP v4 nftables interface: %w", err)
		}

		tx := nft.NewTransaction()

		tx.Add(&knftables.Table{})

		tx.Add(&knftables.Set{
			Name:    types.NFTEgressIPARPBlockV4,
			Comment: knftables.PtrTo("IPv4 egress IPs"),
			Type:    "ipv4_addr",
		})

		for _, ipStr := range ipv4Addrs {
			tx.Add(&knftables.Element{
				Set: types.NFTEgressIPARPBlockV4,
				Key: []string{ipStr},
			})
		}

		tx.Add(&knftables.Chain{
			Name:     nftEgressIPDropChain,
			Comment:  knftables.PtrTo("Block ARP for egress IPs during application restart"),
			Type:     knftables.PtrTo(knftables.FilterType),
			Hook:     knftables.PtrTo(knftables.InputHook),
			Priority: knftables.PtrTo(knftables.FilterPriority),
		})

		tx.Add(&knftables.Rule{
			Chain: nftEgressIPDropChain,
			Rule: knftables.Concat(
				"arp daddr ip @"+types.NFTEgressIPARPBlockV4,
				"drop",
			),
		})

		if err = nft.Run(context.TODO(), tx); err != nil {
			return fmt.Errorf("could not setup egress IP v4 nftables: %v", err)
		}
	}

	if config.IPv6Mode && len(ipv6Addrs) > 0 {
		nft, err := knftables.New(knftables.InetFamily, nodenft.OVNKubernetesEgressIPv6NFTablesName)
		if err != nil {
			return fmt.Errorf("failed to create egress IP v6 nftables interface: %w", err)
		}

		tx := nft.NewTransaction()

		tx.Add(&knftables.Table{})

		tx.Add(&knftables.Set{
			Name:    types.NFTEgressIPNDPBlockV6,
			Comment: knftables.PtrTo("IPv6 egress IPs"),
			Type:    "ipv6_addr",
		})

		for _, ipStr := range ipv6Addrs {
			tx.Add(&knftables.Element{
				Set: types.NFTEgressIPNDPBlockV6,
				Key: []string{ipStr},
			})
		}

		tx.Add(&knftables.Chain{
			Name:     nftEgressIPDropChain,
			Comment:  knftables.PtrTo("Block NDP for egress IPs during application restart"),
			Type:     knftables.PtrTo(knftables.FilterType),
			Hook:     knftables.PtrTo(knftables.InputHook),
			Priority: knftables.PtrTo(knftables.FilterPriority),
		})

		tx.Add(&knftables.Rule{
			Chain: nftEgressIPDropChain,
			Rule: knftables.Concat(
				"icmpv6 type nd-neighbor-solicit",
				"icmpv6 taddr @"+types.NFTEgressIPNDPBlockV6,
				"drop",
			),
		})

		if err = nft.Run(context.TODO(), tx); err != nil {
			return fmt.Errorf("could not setup egress IP v6 nftables: %v", err)
		}
	}

	return nil
}

// CleanupEgressIPARPBlockNFTTable deletes the dedicated nftables tables for egress IP ARP/NDP blocking.
// Called during startup to remove stale rules from previous container shutdown.
// On full node reboot, nftables state is cleared automatically, so this primarily handles container restarts.
func CleanupEgressIPARPBlockNFTTable() error {
	if config.IPv4Mode {
		nft, err := knftables.New(knftables.ARPFamily, nodenft.OVNKubernetesEgressIPv4NFTablesName)
		if err != nil {
			return fmt.Errorf("failed to create egress IP v4 nftables interface: %w", err)
		}

		tx := nft.NewTransaction()
		tx.Delete(&knftables.Table{})

		if err = nft.Run(context.TODO(), tx); err != nil && !knftables.IsNotFound(err) {
			return fmt.Errorf("could not delete egress IP v4 nftables table: %v", err)
		} else {
			klog.Infof("Deleted stale egress IP v4 nftables table from previous shutdown")
		}
	}

	if config.IPv6Mode {
		nft, err := knftables.New(knftables.InetFamily, nodenft.OVNKubernetesEgressIPv6NFTablesName)
		if err != nil {
			return fmt.Errorf("failed to create egress IP v6 nftables interface: %w", err)
		}

		tx := nft.NewTransaction()
		tx.Delete(&knftables.Table{})

		if err = nft.Run(context.TODO(), tx); err != nil && !knftables.IsNotFound(err) {
			return fmt.Errorf("could not delete egress IP v6 nftables table: %v", err)
		} else {
			klog.Infof("Deleted stale egress IP v6 nftables table from previous shutdown")
		}
	}

	return nil
}
