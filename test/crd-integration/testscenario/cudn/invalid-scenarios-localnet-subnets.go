// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import (
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var LocalnetInvalidSubnets = []testscenario.ValidateCRScenario{
	{
		Description: "unset subnets, and ipam.mode is unset",
		ExpectedErr: `Subnets is required with ipam.mode is Enabled or unset, and forbidden otherwise`,
		Object: newCUDN("localnet-subnets-and-ipam-disabled-unset-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{Role: "Secondary", PhysicalNetworkName: "test"},
		}),
	},
	{
		Description: "ipam.mode is Enabled but subnets is unset",
		ExpectedErr: `Subnets is required with ipam.mode is Enabled or unset, and forbidden otherwise`,
		Object: newCUDN("localnet-ipam-mode-disabled-ipam-lifecycle-persistent-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{Role: "Secondary", PhysicalNetworkName: "test", IPAM: &udnv1.IPAMConfig{Mode: "Enabled"}},
		}),
	},
	{
		Description: "subnets is set but ipam.mode is Disabled",
		ExpectedErr: `Subnets is required with ipam.mode is Enabled or unset, and forbidden otherwise`,
		Object: newCUDN("localnet-subnet-and-ipam-disabled-set-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "asd",
				Subnets:             udnv1.DualStackCIDRs{"10.0.0.0/24"},
				IPAM:                &udnv1.IPAMConfig{Mode: "Disabled"},
			},
		}),
	},
	{
		Description: "subnets is empty",
		ExpectedErr: `ClusterUserDefinedNetwork.k8s.ovn.org "localnet-empty-subnets-fail" is invalid: spec.network.localnet.subnets: Invalid value: 0: spec.network.localnet.subnets in body should have at least 1 items`,
		Object: testscenario.WithField(newCUDN("localnet-empty-subnets-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{Role: "Secondary", PhysicalNetworkName: "test"},
		}), []any{}, "spec.network.localnet.subnets"),
	},
	{
		Description: "excludeSubnets is set, subnets is unset",
		ExpectedErr: `excludeSubnets must be unset when subnets is unset`,
		Object: newCUDN("localnet-subnets-unset-and-excludesubnets-set-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				ExcludeSubnets:      []udnv1.CIDR{"10.0.0.1/24"},
			},
		}),
	},
	{
		Description: "excludeSubnets is set, ipam.mode is Disabled",
		ExpectedErr: `excludeSubnets must be unset when subnets is unset`,
		Object: newCUDN("localnet-ipam-disabled-and-excludesubnets-set-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				ExcludeSubnets:      []udnv1.CIDR{"10.0.0.1/24"},
				IPAM:                &udnv1.IPAMConfig{Mode: "Disabled"},
			},
		}),
	},
	{
		Description: "subnets & excludeSubnets are set, but ipam.mode is Disabled",
		ExpectedErr: "Subnets is required with ipam.mode is Enabled or unset, and forbidden otherwise",
		Object: newCUDN("localnet-ipam-disabled-subnetes-excludesubnetes-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "asd",
				Subnets:             udnv1.DualStackCIDRs{"10.0.0.0/24"},
				ExcludeSubnets:      []udnv1.CIDR{"10.0.0.1/32"},
				IPAM:                &udnv1.IPAMConfig{Mode: "Disabled"},
			},
		}),
	},
	{
		Description: "invalid subnets - invalid IPv4 CIDR",
		ExpectedErr: `ClusterUserDefinedNetwork.k8s.ovn.org "localnet-subnets-invalid-ipv4-cidr-fail" is invalid: spec.network.localnet.subnets[0]: Invalid value: "300.0.0.0/24": CIDR is invalid`,
		Object: newCUDN("localnet-subnets-invalid-ipv4-cidr-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				Subnets:             udnv1.DualStackCIDRs{"300.0.0.0/24"},
			},
		}),
	},
	{
		Description: "invalid subnets - invalid IPv6 CIDR",
		ExpectedErr: `ClusterUserDefinedNetwork.k8s.ovn.org "localnet-subnets-invalid-ipv6-cidr-fail" is invalid: spec.network.localnet.subnets[0]: Invalid value: "2014:100:200::0/300": CIDR is invalid`,
		Object: newCUDN("localnet-subnets-invalid-ipv6-cidr-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				Subnets:             udnv1.DualStackCIDRs{"2014:100:200::0/300"},
			},
		}),
	},
	{
		Description: "invalid subnets - two IPv4 CIDRs",
		ExpectedErr: `ClusterUserDefinedNetwork.k8s.ovn.org "localnet-subnets-multiple-ipv4-cidrs-fail" is invalid: spec.network.localnet.subnets: Invalid value: When 2 CIDRs are set, they must be from different IP families`,
		Object: newCUDN("localnet-subnets-multiple-ipv4-cidrs-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				Subnets:             udnv1.DualStackCIDRs{"10.10.0.0/24", "10.20.0.0/24"},
			},
		}),
	},
	{
		Description: "invalid subnets - two IPv6 CIDRs",
		ExpectedErr: `ClusterUserDefinedNetwork.k8s.ovn.org "localnet-subnets-multiple-ipv6-cidrs-fail" is invalid: spec.network.localnet.subnets: Invalid value: When 2 CIDRs are set, they must be from different IP families`,
		Object: newCUDN("localnet-subnets-multiple-ipv6-cidrs-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				Subnets:             udnv1.DualStackCIDRs{"2001:aaa::/64", "2001:bbb::/64"},
			},
		}),
	},
	{
		Description: "invalid excludeSubnets - empty slice",
		ExpectedErr: `ClusterUserDefinedNetwork.k8s.ovn.org "localnet-excludesubnet-empty-slice-fail" is invalid: spec.network.localnet.excludeSubnets: Invalid value: 0: spec.network.localnet.excludeSubnets in body should have at least 1 items`,
		Object: testscenario.WithField(newCUDN("localnet-excludesubnet-empty-slice-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				Subnets:             udnv1.DualStackCIDRs{"10.0.0.0/24"},
			},
		}), []any{}, "spec.network.localnet.excludeSubnets"),
	},
	{
		Description: "invalid excludeSubnets - invalid IPv4 CIDR",
		ExpectedErr: `ClusterUserDefinedNetwork.k8s.ovn.org "localnet-excludesubnet-invalid-ipv4-cidr-fail" is invalid: spec.network.localnet.excludeSubnets[0]: Invalid value: "10.0.0.0/300": CIDR is invalid`,
		Object: newCUDN("localnet-excludesubnet-invalid-ipv4-cidr-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				Subnets:             udnv1.DualStackCIDRs{"10.0.0.0/24"},
				ExcludeSubnets:      []udnv1.CIDR{"10.0.0.0/300"},
			},
		}),
	},
	{
		Description: "invalid excludeSubnets - invalid IPv6 CIDR",
		ExpectedErr: `ClusterUserDefinedNetwork.k8s.ovn.org "localnet-excludesubnet-invalid-ipv6-cidr-fail" is invalid: spec.network.localnet.excludeSubnets[0]: Invalid value: "2014:100:200::0/300": CIDR is invalid`,
		Object: newCUDN("localnet-excludesubnet-invalid-ipv6-cidr-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				Subnets:             udnv1.DualStackCIDRs{"10.0.0.0/24"},
				ExcludeSubnets:      []udnv1.CIDR{"2014:100:200::0/300"},
			},
		}),
	},
	{
		Description: "ipam.lifecycle is Persistent but ipam.mode is Disabled",
		ExpectedErr: `lifecycle Persistent is only supported when ipam.mode is Enabled`,
		Object: newCUDN("localnet-ipam-mode-disabled-ipam-lifecycle-persistent-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				IPAM:                &udnv1.IPAMConfig{Mode: "Disabled", Lifecycle: "Persistent"},
			},
		}),
	},
}
