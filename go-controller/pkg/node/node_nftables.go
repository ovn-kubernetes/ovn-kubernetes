package node

import (
	"context"
	"fmt"

	"sigs.k8s.io/knftables"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

const nftPMTUDChain = "no-pmtud"

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
