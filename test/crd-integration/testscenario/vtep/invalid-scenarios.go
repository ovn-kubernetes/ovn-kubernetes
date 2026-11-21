// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package vtep

import (
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var Invalid = []testscenario.ValidateCRScenario{
	{
		Description: "CIDR must be a valid network address (not a host IP)",
		ExpectedErr: `CIDR must be a valid network address`,
		Object:      newVTEP("vtep-invalid-cidr-not-network", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"10.20.100.1/24"}}),
	},
	{
		Description: "CIDR cannot be empty",
		ExpectedErr: `spec.cidrs: Invalid value`,
		Object:      newVTEP("vtep-empty-cidrs", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{}}),
	},
	{
		Description: "CIDRs cannot have more than 20 items",
		ExpectedErr: `spec.cidrs: Too many`,
		Object: newVTEP("vtep-too-many-cidrs", vtepv1.VTEPSpec{
			CIDRs: []vtepv1.CIDR{
				"10.0.0.0/24",
				"10.1.0.0/24",
				"10.2.0.0/24",
				"10.3.0.0/24",
				"10.4.0.0/24",
				"10.5.0.0/24",
				"10.6.0.0/24",
				"10.7.0.0/24",
				"10.8.0.0/24",
				"10.9.0.0/24",
				"10.10.0.0/24",
				"10.11.0.0/24",
				"10.12.0.0/24",
				"10.13.0.0/24",
				"10.14.0.0/24",
				"10.15.0.0/24",
				"10.16.0.0/24",
				"10.17.0.0/24",
				"fd00:1::/64",
				"fd00:2::/64",
				"fd00:3::/64",
			},
		}),
	},
	{
		Description: "CIDR must be in valid format",
		ExpectedErr: `CIDR must be a valid network address`,
		Object:      newVTEP("vtep-invalid-cidr-format", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"invalid-cidr"}}),
	},
	{
		Description: "Mode must be a valid enum value",
		ExpectedErr: `spec.mode: Unsupported value`,
		Object:      newVTEP("vtep-invalid-mode", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"10.20.100.0/24"}, Mode: "InvalidMode"}),
	},
	{
		Description: "CIDRs must not overlap (superset contains subset)",
		ExpectedErr: `CIDRs must not overlap with each other`,
		Object:      newVTEP("vtep-overlap-superset", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"10.30.0.0/16", "10.30.1.0/24"}}),
	},
	{
		Description: "CIDRs must not overlap (identical CIDRs)",
		ExpectedErr: `CIDRs must not overlap with each other`,
		Object:      newVTEP("vtep-overlap-identical", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"10.31.0.0/24", "10.31.0.0/24"}}),
	},
}
