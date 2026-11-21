// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import (
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var Layer3InvalidSubnetsUpdate = []testscenario.UpdateCRScenario{
	{
		InitialObject: newCUDN("secondary-modify-subnet-ipv4", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3:   &udnv1.Layer3Config{Role: "Secondary", Subnets: []udnv1.Layer3Subnet{{CIDR: "10.1.0.0/16", HostSubnet: 24}}},
		}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "modifying Secondary network's subnets is not allowed",
			Object: newCUDN("secondary-modify-subnet-ipv4", udnv1.NetworkSpec{
				Topology: "Layer3",
				Layer3: &udnv1.Layer3Config{
					Role:    "Secondary",
					Subnets: []udnv1.Layer3Subnet{{CIDR: "10.1.0.0/16", HostSubnet: 24}, {CIDR: "10.2.0.0/16", HostSubnet: 24}},
				},
			}),
			ExpectedErr: "Subnets is immutable for Secondary role",
		},
	},
	{
		InitialObject: newCUDN("primary-remove-subnet-ipv4", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				Subnets: []udnv1.Layer3Subnet{{CIDR: "10.1.0.0/16", HostSubnet: 24}, {CIDR: "10.2.0.0/16", HostSubnet: 24}},
			},
		}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "IPv4: remove subnet is not allowed",
			Object: newCUDN("primary-remove-subnet-ipv4", udnv1.NetworkSpec{
				Topology: "Layer3",
				Layer3:   &udnv1.Layer3Config{Role: "Primary", Subnets: []udnv1.Layer3Subnet{{CIDR: "10.1.0.0/16", HostSubnet: 24}}},
			}),
			ExpectedErr: "Removing existing subnets is not allowed",
		},
	},
	{
		InitialObject: newCUDN("primary-mutate-host-subnet-ipv4", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3:   &udnv1.Layer3Config{Role: "Primary", Subnets: []udnv1.Layer3Subnet{{CIDR: "10.1.0.0/16", HostSubnet: 24}}},
		}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "IPv4: mutate hostSubnet is not allowed",
			Object: newCUDN("primary-mutate-host-subnet-ipv4", udnv1.NetworkSpec{
				Topology: "Layer3",
				Layer3:   &udnv1.Layer3Config{Role: "Primary", Subnets: []udnv1.Layer3Subnet{{CIDR: "10.1.0.0/16", HostSubnet: 18}}},
			}),
			ExpectedErr: "hostSubnet is immutable",
		},
	},
	{
		InitialObject: newCUDN("primary-remove-subnet-ipv6", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				Subnets: []udnv1.Layer3Subnet{{CIDR: "2001:db8:1::/48", HostSubnet: 64}, {CIDR: "2001:db8:2::/48", HostSubnet: 64}},
			},
		}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "IPv6: remove subnet is not allowed",
			Object: newCUDN("primary-remove-subnet-ipv6", udnv1.NetworkSpec{
				Topology: "Layer3",
				Layer3:   &udnv1.Layer3Config{Role: "Primary", Subnets: []udnv1.Layer3Subnet{{CIDR: "2001:db8:1::/48", HostSubnet: 64}}},
			}),
			ExpectedErr: "Removing existing subnets is not allowed",
		},
	},
	{
		InitialObject: newCUDN("primary-mutate-host-subnet-ipv6", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3:   &udnv1.Layer3Config{Role: "Primary", Subnets: []udnv1.Layer3Subnet{{CIDR: "2001:db8:1::/48", HostSubnet: 64}}},
		}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "IPv6: mutate hostSubnet is not allowed",
			Object: newCUDN("primary-mutate-host-subnet-ipv6", udnv1.NetworkSpec{
				Topology: "Layer3",
				Layer3:   &udnv1.Layer3Config{Role: "Primary", Subnets: []udnv1.Layer3Subnet{{CIDR: "2001:db8:1::/48", HostSubnet: 60}}},
			}),
			ExpectedErr: "hostSubnet is immutable",
		},
	},
}

var Layer3InvalidSubnets = []testscenario.ValidateCRScenario{
	{
		Description: "Secondary network with more than 2 subnets is not allowed",
		ExpectedErr: `Secondary networks may define at most 2 subnets`,
		Object: newCUDN("secondary-more-than-two-subnets", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role: "Secondary",
				Subnets: []udnv1.Layer3Subnet{
					{CIDR: "10.1.0.0/16", HostSubnet: 24},
					{CIDR: "2001:db8:1::/48", HostSubnet: 64},
					{CIDR: "10.2.0.0/16", HostSubnet: 24},
				},
			},
		}),
	},
	{
		Description: "Secondary network with 2 same-family subnets is not allowed",
		ExpectedErr: `When 2 CIDRs are set for Secondary networks, they must be from different IP families`,
		Object: newCUDN("secondary-same-family-subnets", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Secondary",
				Subnets: []udnv1.Layer3Subnet{{CIDR: "10.1.0.0/16", HostSubnet: 24}, {CIDR: "10.2.0.0/16", HostSubnet: 24}},
			},
		}),
	},
	{
		Description: "IPv4: overlap subnets are not allowed",
		ExpectedErr: `Subnets must not overlap or contain each other`,
		Object: newCUDN("primary-overlap-subnets-ipv4", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				Subnets: []udnv1.Layer3Subnet{{CIDR: "10.1.0.0/16", HostSubnet: 24}, {CIDR: "10.1.128.0/17", HostSubnet: 24}},
			},
		}),
	},
	{
		Description: "IPv4: same subnets are not allowed",
		ExpectedErr: `Subnets with same CIDR are not allowed`,
		Object: newCUDN("primary-same-subnets-ipv4", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				Subnets: []udnv1.Layer3Subnet{{CIDR: "10.1.0.0/16", HostSubnet: 24}, {CIDR: "10.1.0.0/16", HostSubnet: 24}},
			},
		}),
	},
	{
		Description: "IPv4: subnets with different hostSubnet is not allowed",
		ExpectedErr: `Subnets from the same IP family must use the same hostSubnet value`,
		Object: newCUDN("primary-different-host-subnet-ipv4", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				Subnets: []udnv1.Layer3Subnet{{CIDR: "10.1.0.0/16", HostSubnet: 24}, {CIDR: "10.2.0.0/16", HostSubnet: 27}},
			},
		}),
	},
	{
		Description: "IPv6: overlap subnets are not allowed",
		ExpectedErr: `Subnets must not overlap or contain each other`,
		Object: newCUDN("primary-overlap-subnets-ipv6", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				Subnets: []udnv1.Layer3Subnet{{CIDR: "2001:db8:1::/48", HostSubnet: 64}, {CIDR: "2001:db8:1:8000::/49", HostSubnet: 64}},
			},
		}),
	},
	{
		Description: "IPv6: same subnets are not allowed",
		ExpectedErr: `Subnets with same CIDR are not allowed`,
		Object: newCUDN("primary-same-subnets-ipv6", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				Subnets: []udnv1.Layer3Subnet{{CIDR: "2001:db8:1::/48", HostSubnet: 64}, {CIDR: "2001:db8:1::/48", HostSubnet: 64}},
			},
		}),
	},
	{
		Description: "IPv6: subnets with different hostSubnet is not allowed",
		ExpectedErr: `Subnets from the same IP family must use the same hostSubnet value`,
		Object: newCUDN("primary-different-host-subnet-ipv6", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				Subnets: []udnv1.Layer3Subnet{{CIDR: "2001:db8:1::/48", HostSubnet: 64}, {CIDR: "2001:db8:2::/48", HostSubnet: 60}},
			},
		}),
	},
}
