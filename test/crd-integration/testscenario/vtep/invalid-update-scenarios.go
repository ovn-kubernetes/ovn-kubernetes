// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package vtep

import (
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var InvalidUpdates = []testscenario.UpdateCRScenario{
	{
		InitialObject: newVTEP("vtep-update-remove", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.80.0.0/24", "100.81.0.0/24"}, Mode: "Managed"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: remove a CIDR from the list",
			Object:      newVTEP("vtep-update-remove", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.80.0.0/24"}, Mode: "Managed"}),
			ExpectedErr: "CIDRs cannot be removed in managed mode",
		},
	},
	{
		InitialObject: newVTEP("vtep-update-remove-multi", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/24", "100.65.0.0/24", "100.66.0.0/24"}, Mode: "Managed"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: remove multiple CIDRs (3 to 1) produces clean append-only error, not a CEL runtime out-of-bounds error from the position/mask rule",
			Object:      newVTEP("vtep-update-remove-multi", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/24"}, Mode: "Managed"}),
			ExpectedErr: "CIDRs cannot be removed in managed mode",
		},
	},
	{
		InitialObject: newVTEP("vtep-update-replace", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.82.0.0/24"}, Mode: "Managed"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: replace CIDR with different network (same count)",
			Object:      newVTEP("vtep-update-replace", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"10.0.0.0/24"}, Mode: "Managed"}),
			ExpectedErr: "In managed mode, existing CIDRs must remain at the same position",
		},
	},
	{
		InitialObject: newVTEP("vtep-update-shrink", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.83.0.0/24"}, Mode: "Managed"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: shrink mask /24 to /28",
			Object:      newVTEP("vtep-update-shrink", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.83.0.0/28"}, Mode: "Managed"}),
			ExpectedErr: "In managed mode, existing CIDRs must remain at the same position",
		},
	},
	{
		InitialObject: newVTEP("vtep-update-reorder", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.85.0.0/24", "100.86.0.0/24"}, Mode: "Managed"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: reorder CIDRs (swap positions)",
			Object:      newVTEP("vtep-update-reorder", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.86.0.0/24", "100.85.0.0/24"}, Mode: "Managed"}),
			ExpectedErr: "In managed mode, existing CIDRs must remain at the same position",
		},
	},
	{
		InitialObject: newVTEP("vtep-update-non-containing", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.88.0.0/24"}, Mode: "Managed"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: expand to non-containing wider network",
			Object:      newVTEP("vtep-update-non-containing", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"10.0.0.0/8"}, Mode: "Managed"}),
			ExpectedErr: "In managed mode, existing CIDRs must remain at the same position",
		},
	},
	{
		InitialObject: newVTEP("vtep-update-shrink-to-32", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.84.0.0/24"}, Mode: "Managed"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: shrink /24 to /32 (subnet to single host)",
			Object:      newVTEP("vtep-update-shrink-to-32", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.84.0.0/32"}, Mode: "Managed"}),
			ExpectedErr: "In managed mode, existing CIDRs must remain at the same position",
		},
	},
	{
		InitialObject: newVTEP("vtep-update-replace-32", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.87.0.1/32"}, Mode: "Managed"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: replace /32 with a different /32",
			Object:      newVTEP("vtep-update-replace-32", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.87.0.2/32"}, Mode: "Managed"}),
			ExpectedErr: "In managed mode, existing CIDRs must remain at the same position",
		},
	},
	{
		InitialObject: newVTEP("vtep-update-mode-u-to-m-remove", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.60.0.0/24", "100.61.0.0/24"}, Mode: "Unmanaged"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Mode change to Managed with CIDR removal",
			Object:      newVTEP("vtep-update-mode-u-to-m-remove", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.60.0.0/24"}, Mode: "Managed"}),
			ExpectedErr: "CIDRs cannot be removed in managed mode",
		},
	},
	{
		InitialObject: newVTEP("vtep-update-default-remove", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.89.0.0/24", "100.89.1.0/24"}}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Default mode (omitted): remove a CIDR triggers managed rules",
			Object:      newVTEP("vtep-update-default-remove", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.89.0.0/24"}}),
			ExpectedErr: "CIDRs cannot be removed in managed mode",
		},
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: append a CIDR that overlaps with an existing one",
			Object:      newVTEP("vtep-update-append-overlap", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.93.0.0/16", "100.93.1.0/24"}, Mode: "Managed"}),
			ExpectedErr: "CIDRs must not overlap with each other",
		},
		InitialObject: newVTEP("vtep-update-append-overlap", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.93.0.0/16"}, Mode: "Managed"}),
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: expand mask causing overlap with another CIDR",
			Object:      newVTEP("vtep-update-expand-overlap", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.94.0.0/16", "100.94.1.0/24"}, Mode: "Managed"}),
			ExpectedErr: "CIDRs must not overlap with each other",
		},
		InitialObject: newVTEP("vtep-update-expand-overlap", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.94.0.0/24", "100.94.1.0/24"}, Mode: "Managed"}),
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Unmanaged: overlapping CIDRs are also rejected",
			Object:      newVTEP("vtep-update-unmanaged-overlap", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.95.0.0/16", "100.95.1.0/24"}, Mode: "Unmanaged"}),
			ExpectedErr: "CIDRs must not overlap with each other",
		},
		InitialObject: newVTEP("vtep-update-unmanaged-overlap", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.95.0.0/24"}, Mode: "Unmanaged"}),
	},
}
