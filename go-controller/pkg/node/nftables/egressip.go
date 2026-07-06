// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package nftables

import (
	"context"
	"fmt"

	"k8s.io/klog/v2"
	"sigs.k8s.io/knftables"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	nftEgressIPChain = "ovn-kube-egress-ip"
)

// TODO: further investigation is needed to redesign and simplify this approach.
// Marking packages and dropping them to avoid leaking might not be necessary.
// It might be possible to drop leaking packets solely based on source IP after prerouting SNAT.

// InitEgressIPNFTChain creates the nftables chain for egress IP traffic
// on secondary interface, so it makes sure no traffic with pod IP is leaked.
// This needs to be called once at controller startup
func InitEgressIPNFTChain(v4, v6 bool) error {
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
	if config.Logging.Level > 4 {
		counterIfDebug = "counter"
	}

	// Rule 1: DROP if marked AND from any cluster pod CIDR
	// This prevents pod traffic from leaking before SNAT is ready
	if v4 {
		for _, cidr := range podIPv4CIDRs {
			dropPodIPv4 := &knftables.Rule{
				Chain: nftEgressIPChain,
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
	if v6 {
		for _, cidr := range podIPv6CIDRs {
			dropPodIPv6 := &knftables.Rule{
				Chain: nftEgressIPChain,
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

	if err := nft.Run(context.TODO(), tx); err != nil {
		return fmt.Errorf("failed to setup egress IP nftables chain: %w", err)
	}
	return nil
}
