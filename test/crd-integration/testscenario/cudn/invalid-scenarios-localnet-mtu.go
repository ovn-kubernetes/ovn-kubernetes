// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import (
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var LocalnetInvalidMTU = []testscenario.ValidateCRScenario{
	{
		Description: "invalid MTU - higher than 65536",
		ExpectedErr: `spec.network.localnet.mtu in body should be less than or equal to 65536`,
		Object: newCUDN("localnet-mtu-exceed-max-ipam-disabled-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				IPAM:                &udnv1.IPAMConfig{Mode: "Disabled"},
				MTU:                 65537,
			},
		}),
	},
	{
		Description: "invalid MTU - lower than 576",
		ExpectedErr: `spec.network.localnet.mtu in body should be greater than or equal to 576`,
		Object: newCUDN("localnet-ipv4-mtu-below-min-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				Subnets:             udnv1.DualStackCIDRs{"10.0.0.0/24"},
				MTU:                 575,
			},
		}),
	},
	{
		Description: "invalid MTU - when IPv6 subnet is set, should be at least 1280",
		ExpectedErr: `MTU should be greater than or equal to 1280 when an IPv6 subnet is used`,
		Object: newCUDN("localnet-ipv6-mtu-below-min-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				Subnets:             udnv1.DualStackCIDRs{"2001:dbb::/64"},
				MTU:                 1279,
			},
		}),
	},
	{
		Description: "invalid MTU - when dualstack subnet is set, should be at least 1280",
		ExpectedErr: `MTU should be greater than or equal to 1280 when an IPv6 subnet is used`,
		Object: newCUDN("localnet-dualstack-mtu-below-min-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				Subnets:             udnv1.DualStackCIDRs{"192.168.0.0/16", "2001:dbb::/64"},
				MTU:                 1279,
			},
		}),
	},
}
