// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import (
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var LocalnetInvalidPhyNetName = []testscenario.ValidateCRScenario{
	{
		Description: "unset PhysicalNetworkName",
		ExpectedErr: `spec.network.localnet.physicalNetworkName: Required value`,
		Object: testscenario.WithoutFields(newCUDN("localnet-phynetname-unset-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{Role: "Secondary", Subnets: udnv1.DualStackCIDRs{"10.0.0.0/24"}},
		}), "spec.network.localnet.physicalNetworkName"),
	},
	{
		Description: "invalid PhysicalNetworkName - empty string",
		ExpectedErr: `spec.network.localnet.physicalNetworkName in body should be at least 1 chars long`,
		Object: newCUDN("localnet-invalid-phynetname-empty-string-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{Role: "Secondary", Subnets: udnv1.DualStackCIDRs{"10.0.0.0/24"}},
		}),
	},
	{
		Description: "invalid PhysicalNetworkName - too long",
		ExpectedErr: `spec.network.localnet.physicalNetworkName: Too long: may not be more than 253 bytes`,
		Object: newCUDN("localnet-invalid-phynetname-exceed-253-chars-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "ycx7b6dhkhytzva3wrma0cu6mjhqpo2ty20cmpdg9ptvmt1mo9dfnrs56nr0bvg6z6zha5y208js6e2iwk6xb97sp2sojg48lu9d5vxbzzq40rwj1wchae3ju3dpj6qfsjbzjzmc0k489bloe49z4857kds43rqpeca3p5z2dfz562qu59qqb8qa3vo6pmwuaume581dqhlsz57yvbvgu5hmmmzremac7w7l4rmuirkk91767llw0vskanlc33",
				Subnets:             udnv1.DualStackCIDRs{"10.0.0.0/24"},
			},
		}),
	},
	{
		Description: "invalid PhysicalNetworkName - contain `:` chars",
		ExpectedErr: "physicalNetworkName cannot contain `,` or `:` characters",
		Object: newCUDN("localnet-invalid-phynetname-has-colon-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "t:est",
				Subnets:             udnv1.DualStackCIDRs{"10.0.0.0/24"},
			},
		}),
	},
	{
		Description: "invalid PhysicalNetworkName - contain `,` chars",
		ExpectedErr: "physicalNetworkName cannot contain `,` or `:` characters",
		Object: newCUDN("localnet-invalid-phynetname-has-comma-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "tes,t",
				Subnets:             udnv1.DualStackCIDRs{"10.0.0.0/24"},
			},
		}),
	},
	{
		Description: "invalid PhysicalNetworkName - start with `:` char",
		ExpectedErr: "physicalNetworkName cannot contain `,` or `:` characters",
		Object: newCUDN("localnet-invalid-phynetname-start-with-colon-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: ":test",
				Subnets:             udnv1.DualStackCIDRs{"10.0.0.0/24"},
			},
		}),
	},
	{
		Description: "invalid PhysicalNetworkName - start with `,` char",
		Object: newCUDN("localnet-invalid-phynetname-start-with-comma-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: ",test",
				Subnets:             udnv1.DualStackCIDRs{"10.0.0.0/24"},
			},
		}),
		ExpectedErr: "physicalNetworkName cannot contain `,` or `:` characters",
	},
	{
		Description: "invalid PhysicalNetworkName - ends with `:` char",
		ExpectedErr: "physicalNetworkName cannot contain `,` or `:` characters",
		Object: newCUDN("localnet-invalid-phynetname-end-with-colon-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test:",
				Subnets:             udnv1.DualStackCIDRs{"10.0.0.0/24"},
			},
		}),
	},
	{
		Description: "invalid PhysicalNetworkName - ends with `,` char",
		ExpectedErr: "physicalNetworkName cannot contain `,` or `:` characters",
		Object: newCUDN("localnet-invalid-phynetname-end-with-comma-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test,",
				Subnets:             udnv1.DualStackCIDRs{"10.0.0.0/24"},
			},
		}),
	},
}
