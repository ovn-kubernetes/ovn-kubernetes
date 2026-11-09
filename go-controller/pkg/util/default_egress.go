// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"

// UsesDefaultEgressRoute reports whether the network has a per-node Layer 3
// router whose default egress next hop is owned by gateway reconciliation.
func UsesDefaultEgressRoute(netInfo NetInfo) bool {
	return netInfo.TopologyType() == types.Layer3Topology
}

// UsesOverlayEgressPolicyChains reports whether egress traffic can arrive over
// the interconnect overlay and require a GR next hop in local gateway mode.
func UsesOverlayEgressPolicyChains(netInfo NetInfo) bool {
	return UsesDefaultEgressRoute(netInfo) &&
		netInfo.Transport() != types.NetworkTransportEVPN &&
		netInfo.Transport() != types.NetworkTransportNoOverlay
}
