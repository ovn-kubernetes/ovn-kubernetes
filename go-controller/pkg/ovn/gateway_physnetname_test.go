// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"testing"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// TestPhysNetName verifies which networks share the host uplink's physical
// network ("physnet") versus getting a per-network physical network name. An
// advertised secondary UDN is north-south over the shared uplink and must share
// "physnet" so ovn-controller programs the shared br-int<->breth0 patch port; a
// non-advertised secondary keeps its own name and stays east/west-only.
func TestPhysNetName(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true

	primaryUDN := dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24")
	primary, err := util.NewNetInfo(primaryUDN.netconf())
	if err != nil {
		t.Fatalf("failed to build primary netInfo: %v", err)
	}
	secondaryUDN := dummySecondaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24")
	secondary, err := util.NewNetInfo(secondaryUDN.netconf())
	if err != nil {
		t.Fatalf("failed to build secondary netInfo: %v", err)
	}
	advertisedSecondary := util.NewMutableNetInfo(secondary)
	advertisedSecondary.SetPodNetworkAdvertisedVRFs(map[string][]string{"node": {"vrf"}})

	tests := []struct {
		name    string
		netInfo util.NetInfo
		want    string
	}{
		{"default network shares physnet", &util.DefaultNetInfo{}, types.PhysicalNetworkName},
		{"primary UDN sharing the uplink shares physnet", primary, types.PhysicalNetworkName},
		{"advertised secondary UDN sharing the uplink shares physnet", advertisedSecondary, types.PhysicalNetworkName},
		{"non-advertised secondary UDN keeps its own physical network name", secondary, secondary.GetNetworkName()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := physNetName(tt.netInfo); got != tt.want {
				t.Errorf("physNetName() = %q, want %q", got, tt.want)
			}
		})
	}
}
