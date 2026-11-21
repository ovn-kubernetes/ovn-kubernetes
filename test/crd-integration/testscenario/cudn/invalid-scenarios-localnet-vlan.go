// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import (
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var LocalnetInvalidVLAN = []testscenario.ValidateCRScenario{
	{
		Description: "invalid VLAN - invalid mode",
		ExpectedErr: `spec.network.localnet.vlan.mode: Unsupported value: "Disabled": supported values: "Access`,
		Object: testscenario.WithoutFields(newCUDN("localnet-vlan-invalid-mode-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				Subnets:             udnv1.DualStackCIDRs{"192.168.0.0/16"},
				VLAN:                &udnv1.VLANConfig{Mode: "Disabled"},
			},
		}), "spec.network.localnet.vlan.access"),
	},
	{
		Description: "invalid VLAN -mode is 'Access' but vlan access config is unset",
		ExpectedErr: `vlan access config is required when vlan mode is 'Access', and forbidden otherwise`,
		Object: testscenario.WithoutFields(newCUDN("localnet-vlan--no-access-config-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				Subnets:             udnv1.DualStackCIDRs{"192.168.0.0/16"},
				VLAN:                &udnv1.VLANConfig{Mode: "Access"},
			},
		}), "spec.network.localnet.vlan.access"),
	},
	{
		Description: "invalid VLAN - vlan access config is unset",
		ExpectedErr: `spec.network.localnet.vlan.access.id: Required value`,
		Object: testscenario.WithoutFields(newCUDN("localnet-vlan-access-config-unset-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				Subnets:             udnv1.DualStackCIDRs{"192.168.0.0/16"},
				VLAN:                &udnv1.VLANConfig{Mode: "Access", Access: &udnv1.AccessVLANConfig{}},
			},
		}), "spec.network.localnet.vlan.access.id"),
	},
	{
		Description: "invalid VLAN - vlan access id is 0",
		ExpectedErr: `spec.network.localnet.vlan.access.id in body should be greater than or equal to 1`,
		Object: newCUDN("localnet-vlan-id-lower-then-2-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				Subnets:             udnv1.DualStackCIDRs{"192.168.0.0/16"},
				VLAN:                &udnv1.VLANConfig{Mode: "Access", Access: &udnv1.AccessVLANConfig{}},
			},
		}),
	},
	{
		Description: "invalid VLAN - vlan access id is 4095",
		ExpectedErr: `spec.network.localnet.vlan.access.id in body should be less than or equal to 4094`,
		Object: newCUDN("localnet-vlan-id-higher-then-4094-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				Subnets:             udnv1.DualStackCIDRs{"192.168.0.0/16"},
				VLAN:                &udnv1.VLANConfig{Mode: "Access", Access: &udnv1.AccessVLANConfig{ID: 4095}},
			},
		}),
	},
}
