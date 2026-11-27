// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import (
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var LocalnetValid = []testscenario.ValidateCRScenario{
	{
		Description: "should create localnet topology successfully - minimal",
		Object: newCUDN("localnet-minimal-success", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				Subnets:             udnv1.DualStackCIDRs{"10.0.0.0/24"},
			},
		}),
	},
	{
		Description: "should create localnet topology successfully - ipam persistent",
		Object: newCUDNForNamespaces("localnet-ipam-lifecycle-persistent-success", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				Subnets:             udnv1.DualStackCIDRs{"192.168.0.0/16", "2001:dbb::/64"},
				ExcludeSubnets:      []udnv1.CIDR{"192.168.0.1/32", "2001:dbb::1/128"},
				IPAM:                &udnv1.IPAMConfig{Lifecycle: "Persistent"},
				MTU:                 9000,
				VLAN:                &udnv1.VLANConfig{Mode: "Access", Access: &udnv1.AccessVLANConfig{ID: 3}},
			},
		}, "red", "blue"),
	},
	{
		Description: "should create localnet topology successfully - ipam disabled",
		Object: newCUDNForNamespaces("localnet-ipam-disabled-success", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				IPAM:                &udnv1.IPAMConfig{Mode: "Disabled"},
				MTU:                 9000,
				VLAN:                &udnv1.VLANConfig{Mode: "Access", Access: &udnv1.AccessVLANConfig{ID: 4094}},
			},
		}, "red", "blue"),
	},
	{
		Description: "valid disabled mac-security",
		Object: newCUDN("localnet-mac-security-disabled-success", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				IPAM:                &udnv1.IPAMConfig{Mode: "Disabled"},
				MACSecurity:         &udnv1.MACSecurityConfig{Mode: "Disabled"},
			},
		}),
	},
	{
		Description: "valid explicitly enabled mac-security",
		Object: newCUDN("localnet-mac-security-explicitly-enabled-success", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				IPAM:                &udnv1.IPAMConfig{Mode: "Disabled"},
				MACSecurity:         &udnv1.MACSecurityConfig{Mode: "Enabled"},
			},
		}),
	},
}
