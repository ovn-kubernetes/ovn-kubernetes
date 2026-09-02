// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"testing"

	"github.com/onsi/gomega"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	multinetworkmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks/multinetwork"
)

func TestMACBindingSourceOptions(t *testing.T) {
	tests := []struct {
		name            string
		isUDN           bool
		uplink          string
		prefix          string
		physNetworkName string
		expected        map[string]string
	}{
		{
			name:            "shared UDN external port",
			isUDN:           true,
			physNetworkName: types.PhysicalNetworkName,
			expected: map[string]string{
				libovsdbops.MACBindingSource: "rtoe-GR_node-a",
			},
		},
		{
			name:            "UDN on a separate uplink",
			isUDN:           true,
			uplink:          "uplink-a",
			physNetworkName: types.PhysicalNetworkName,
		},
		{
			name:            "UDN on another physical network",
			isUDN:           true,
			physNetworkName: "blue",
		},
		{
			name:            "UDN egress gateway port",
			isUDN:           true,
			prefix:          types.EgressGWSwitchPrefix,
			physNetworkName: types.PhysicalNetworkExGwName,
		},
		{
			name:            "default network external port",
			physNetworkName: types.PhysicalNetworkName,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			netInfo := multinetworkmocks.NewNetInfo(t)
			netInfo.On("IsUserDefinedNetwork").Return(test.isUDN).Maybe()
			netInfo.On("Uplink").Return(test.uplink).Maybe()
			gw := GatewayManager{nodeName: "node-a", netInfo: netInfo}

			g.Expect(gw.macBindingSourceOptions(test.prefix, test.physNetworkName)).To(gomega.Equal(test.expected))
		})
	}
}
