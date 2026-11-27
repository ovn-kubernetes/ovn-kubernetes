// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import (
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var Layer3ValidSubnetsUpdates = []testscenario.UpdateCRScenario{
	{
		InitialObject: newCUDN("primary-with-multiple-subnets-ipv4", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				Subnets: []udnv1.Layer3Subnet{{CIDR: "10.1.0.0/16", HostSubnet: 24}, {CIDR: "10.2.0.0/16", HostSubnet: 24}},
			},
		}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "IPv4: valid Primary network with multiple subnets - add subnet",
			Object: newCUDN("primary-with-multiple-subnets-ipv4", udnv1.NetworkSpec{
				Topology: "Layer3",
				Layer3: &udnv1.Layer3Config{
					Role: "Primary",
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "10.1.0.0/16", HostSubnet: 24},
						{CIDR: "10.2.0.0/16", HostSubnet: 24},
						{CIDR: "10.3.0.0/16", HostSubnet: 24},
					},
				},
			}),
			ExpectedErr: "",
		},
	},
	{
		InitialObject: newCUDN("primary-with-multiple-subnets-ipv6", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				Subnets: []udnv1.Layer3Subnet{{CIDR: "2001:db8:1::/48", HostSubnet: 64}, {CIDR: "2001:db8:2::/48", HostSubnet: 64}},
			},
		}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "IPv6: valid Primary network with multiple subnets - add subnet",
			Object: newCUDN("primary-with-multiple-subnets-ipv6", udnv1.NetworkSpec{
				Topology: "Layer3",
				Layer3: &udnv1.Layer3Config{
					Role: "Primary",
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "2001:db8:1::/48", HostSubnet: 64},
						{CIDR: "2001:db8:2::/48", HostSubnet: 64},
						{CIDR: "2001:db8:3::/48", HostSubnet: 64},
					},
				},
			}),
			ExpectedErr: "",
		},
	},
}

var Layer3ValidSubnets = []testscenario.ValidateCRScenario{
	{
		Description: "IPv4: valid Primary network with multiple subnets",
		Object: newCUDN("primary-with-multiple-subnets-ipv4", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				Subnets: []udnv1.Layer3Subnet{{CIDR: "10.1.0.0/16", HostSubnet: 24}, {CIDR: "10.2.0.0/16", HostSubnet: 24}},
			},
		}),
	},
	{
		Description: "IPv6: valid Primary network with multiple subnets",
		Object: newCUDN("primary-with-multiple-subnets-ipv6", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				Subnets: []udnv1.Layer3Subnet{{CIDR: "2001:db8:1::/48", HostSubnet: 64}, {CIDR: "2001:db8:2::/48", HostSubnet: 64}},
			},
		}),
	},
	{
		Description: "dual-stack: valid Primary network with multiple subnets",
		Object: newCUDN("primary-with-multiple-subnets-dual-stack", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role: "Primary",
				Subnets: []udnv1.Layer3Subnet{
					{CIDR: "10.1.0.0/16", HostSubnet: 24},
					{CIDR: "10.2.0.0/16", HostSubnet: 24},
					{CIDR: "2001:db8:1::/48", HostSubnet: 64},
					{CIDR: "2001:db8:2::/48", HostSubnet: 64},
				},
			},
		}),
	},
}
