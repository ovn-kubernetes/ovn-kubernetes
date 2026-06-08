// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"fmt"

	"k8s.io/klog/v2"
	"sigs.k8s.io/knftables"

	ovnconfig "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// TODO: further investigation is needed to redesign and simplify this approach.
// Marking packages and dropping them to avoid leaking might not be necessary.
// It might be possible to drop leaking packets solely based on source IP after prerouting SNAT.

// initNFTables populates tx with the chain/map/rule definitions for EgressIP: the SNAT chain and
// v4/v6 maps, the LGW connmark chain (or its cleanup when not in LGW mode), and the
// secondary-interface chain that drops pod traffic that hasn't been SNATed yet. The caller owns
// creating and running the transaction.
func (c *Controller) initNFTables(tx *knftables.Transaction) error {
	tx.Add(&knftables.Chain{
		Name:     types.EgressIPNFTablesChainName,
		Type:     knftables.PtrTo(knftables.NATType),
		Hook:     knftables.PtrTo(knftables.PostroutingHook),
		Priority: knftables.PtrTo(knftables.SNATPriority),
	})
	tx.Flush(&knftables.Chain{Name: types.EgressIPNFTablesChainName})

	if c.v4 {
		tx.Add(&knftables.Map{
			Name: types.EgressIPNFTablesMapV4,
			Type: "ipv4_addr . ifname : ipv4_addr",
		})
		tx.Add(&knftables.Rule{
			Chain: types.EgressIPNFTablesChainName,
			Rule:  knftables.Concat("snat to", "ip saddr . meta oifname map", "@", types.EgressIPNFTablesMapV4),
		})
	}
	if c.v6 {
		tx.Add(&knftables.Map{
			Name: types.EgressIPNFTablesMapV6,
			Type: "ipv6_addr . ifname : ipv6_addr",
		})
		tx.Add(&knftables.Rule{
			Chain: types.EgressIPNFTablesChainName,
			Rule:  knftables.Concat("snat to", "ip6 saddr . meta oifname map", "@", types.EgressIPNFTablesMapV6),
		})
	}

	if ovnconfig.Gateway.Mode == ovnconfig.GatewayModeLocal {
		tx.Add(&knftables.Chain{
			Name:     types.EgressIPNFTablesConnmarkChainName,
			Type:     knftables.PtrTo(knftables.FilterType),
			Hook:     knftables.PtrTo(knftables.PreroutingHook),
			Priority: knftables.PtrTo(knftables.ManglePriority),
		})
		tx.Flush(&knftables.Chain{Name: types.EgressIPNFTablesConnmarkChainName})
		tx.Add(&knftables.Rule{
			Chain: types.EgressIPNFTablesConnmarkChainName,
			Rule:  knftables.Concat("meta mark", 0, "meta mark set ct mark"),
		})
		tx.Add(&knftables.Rule{
			Chain: types.EgressIPNFTablesConnmarkChainName,
			Rule:  knftables.Concat("meta mark", types.EgressIPConnmarkMark, "ct mark set meta mark"),
		})
	} else {
		// Clean up connmark chain from a previous LGW mode config
		tx.Add(&knftables.Chain{Name: types.EgressIPNFTablesConnmarkChainName})
		tx.Flush(&knftables.Chain{Name: types.EgressIPNFTablesConnmarkChainName})
		tx.Delete(&knftables.Chain{Name: types.EgressIPNFTablesConnmarkChainName})
	}

	// Get cluster-wide pod CIDRs from config
	podIPv4CIDRs, podIPv6CIDRs := util.GetClusterSubnets()
	if c.v4 && len(podIPv4CIDRs) == 0 {
		return fmt.Errorf("IPv4 enabled but no cluster pod CIDRs configured")
	}
	if c.v6 && len(podIPv6CIDRs) == 0 {
		return fmt.Errorf("IPv6 enabled but no cluster pod CIDRs configured")
	}
	klog.Infof("Using cluster pod CIDRs for egress IP traffic: IPv4=%v, IPv6=%v",
		podIPv4CIDRs, podIPv6CIDRs)

	// Create the dedicated chain for egress IP traffic
	// Use priority 101 (after all SNAT rules at 100, before local gateway masquerade)
	eipChain := &knftables.Chain{
		Name:     types.EgressIPNFTablesSecondaryInterfaceChainName,
		Comment:  knftables.PtrTo("OVN egress IP traffic handling"),
		Type:     knftables.PtrTo(knftables.FilterType),
		Hook:     knftables.PtrTo(knftables.PostroutingHook),
		Priority: knftables.PtrTo(knftables.BaseChainPriority("srcnat + 1")),
	}
	tx.Add(eipChain)

	tx.Flush(eipChain)

	counterIfDebug := ""
	if ovnconfig.Logging.Level > 4 {
		counterIfDebug = "counter"
	}

	// Rule 1: DROP if marked AND from any cluster pod CIDR
	// This prevents pod traffic from leaking before SNAT is ready
	if c.v4 {
		for _, cidr := range podIPv4CIDRs {
			dropPodIPv4 := &knftables.Rule{
				Chain: types.EgressIPNFTablesSecondaryInterfaceChainName,
				Rule: knftables.Concat(
					"ip", "saddr", cidr.String(),
					"meta", "mark", types.EgressIPSecondaryInterfaceMark,
					counterIfDebug,
					"drop",
					"comment", fmt.Sprintf("\"Drop egress IP pod traffic from %s\"", cidr),
				),
			}
			tx.Add(dropPodIPv4)
		}
	}
	if c.v6 {
		for _, cidr := range podIPv6CIDRs {
			dropPodIPv6 := &knftables.Rule{
				Chain: types.EgressIPNFTablesSecondaryInterfaceChainName,
				Rule: knftables.Concat(
					"ip6", "saddr", cidr.String(),
					"meta", "mark", types.EgressIPSecondaryInterfaceMark,
					counterIfDebug,
					"drop",
					"comment", fmt.Sprintf("\"Drop egress IP pod traffic from %s\"", cidr),
				),
			}
			tx.Add(dropPodIPv6)
		}
	}

	return nil
}
