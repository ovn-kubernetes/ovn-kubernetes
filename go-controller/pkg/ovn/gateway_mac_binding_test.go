// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"testing"

	"github.com/onsi/gomega"

	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	multinetworkmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks/multinetwork"
)

func TestUsesSharedMACBindingScope(t *testing.T) {
	tests := []struct {
		name            string
		uplink          string
		prefix          string
		physNetworkName string
		expected        bool
	}{
		{
			name:            "shared UDN external port",
			physNetworkName: types.PhysicalNetworkName,
			expected:        true,
		},
		{
			name:            "UDN on a separate uplink",
			uplink:          "uplink-a",
			physNetworkName: types.PhysicalNetworkName,
		},
		{
			name:            "UDN on another physical network",
			physNetworkName: "blue",
		},
		{
			name:            "UDN egress gateway port",
			prefix:          types.EgressGWSwitchPrefix,
			physNetworkName: types.PhysicalNetworkExGwName,
		},
		{
			name:            "default network external port",
			physNetworkName: types.PhysicalNetworkName,
			expected:        true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			netInfo := multinetworkmocks.NewNetInfo(t)
			netInfo.On("Uplink").Return(test.uplink).Maybe()
			gw := GatewayManager{nodeName: "node-a", netInfo: netInfo}

			g.Expect(gw.usesSharedMACBindingScope(test.prefix, test.physNetworkName)).To(gomega.Equal(test.expected))
		})
	}
}

func expectedMACBindingScope(nodeName string) *nbdb.MACBindingScope {
	name := types.MACBindingScopePrefix + nodeName
	return &nbdb.MACBindingScope{
		UUID:                      name + "-UUID",
		Name:                      name,
		AlwaysLearnFromArpRequest: ptr.To(false),
		DisableGarpRarp:           ptr.To(false),
		MACBindingAgeThreshold:    ptr.To(types.GRMACBindingAgeThreshold),
		ExternalIDs: map[string]string{
			types.NodeExternalID: nodeName,
		},
	}
}
