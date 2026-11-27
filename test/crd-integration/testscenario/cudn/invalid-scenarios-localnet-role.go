// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import (
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var LocalnetInvalidRole = []testscenario.ValidateCRScenario{
	{
		Description: "role unset",
		ExpectedErr: `spec.network.localnet.role: Required value`,
		Object: testscenario.WithoutFields(newCUDN("localnet-role-unset-should-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{PhysicalNetworkName: "test", Subnets: udnv1.DualStackCIDRs{"10.0.0.0/24"}},
		}), "spec.network.localnet.role"),
	},
	{
		Description: "role is primary",
		ExpectedErr: `spec.network.localnet.role: Unsupported value: "Primary": supported values: "Secondary"`,
		Object: newCUDN("localnet-role-primary-should-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Primary",
				PhysicalNetworkName: "test",
				Subnets:             udnv1.DualStackCIDRs{"10.0.0.0/24"},
			},
		}),
	},
}
