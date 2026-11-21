// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import (
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var LocalnetInvalidMACSecurity = []testscenario.ValidateCRScenario{
	{
		Description: "mac-security disabled requires ipam disabled",
		ExpectedErr: `macSecurity.mode Disabled requires ipam.mode to be Disabled`,
		Object: newCUDN("mac-security-localnet-ipam-enabled-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				MACSecurity:         &udnv1.MACSecurityConfig{Mode: "Disabled"},
			},
		}),
	},
}

var LocalnetUpdatesRejected = []testscenario.UpdateCRScenario{
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "localnet: enabling macSecurity after creation is rejected",
			ExpectedErr: "Localnet is immutable",
			Object: newCUDN("mac-security-localnet-update-immutable", udnv1.NetworkSpec{
				Topology: "Localnet",
				Localnet: &udnv1.LocalnetConfig{
					Role:                "Secondary",
					PhysicalNetworkName: "test",
					IPAM:                &udnv1.IPAMConfig{Mode: "Disabled"},
					MACSecurity:         &udnv1.MACSecurityConfig{Mode: "Disabled"},
				},
			}),
		},
		InitialObject: newCUDN("mac-security-localnet-update-immutable", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "test",
				IPAM:                &udnv1.IPAMConfig{Mode: "Disabled"},
			},
		}),
	},
}
