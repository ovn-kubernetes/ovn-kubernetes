// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func TestUsesDefaultEgressRoute(t *testing.T) {
	for _, tc := range []struct {
		topology, transport        string
		defaultRoute, policyChains bool
	}{
		{types.Layer3Topology, "", true, true},
		{types.Layer3Topology, types.NetworkTransportNoOverlay, true, false},
		{types.Layer3Topology, types.NetworkTransportEVPN, true, false},
		{types.Layer2Topology, "", false, false},
		{types.LocalnetTopology, "", false, false},
	} {
		t.Run(tc.topology+"/"+tc.transport, func(t *testing.T) {
			ni := &userDefinedNetInfo{topology: tc.topology, transport: tc.transport}
			require.Equal(t, tc.defaultRoute, UsesDefaultEgressRoute(ni))
			require.Equal(t, tc.policyChains, UsesOverlayEgressPolicyChains(ni))
		})
	}
}
