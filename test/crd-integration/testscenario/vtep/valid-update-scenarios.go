// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package vtep

import (
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var ValidUpdates = []testscenario.UpdateCRScenario{
	{
		InitialObject: newVTEP("vtep-update-noop", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.70.0.0/24"}, Mode: "Managed"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: no change (idempotent update)",
			Object:      newVTEP("vtep-update-noop", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.70.0.0/24"}, Mode: "Managed"}),
		},
	},
	{
		InitialObject: newVTEP("vtep-update-append", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.71.0.0/24"}, Mode: "Managed"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: append a new CIDR to existing list",
			Object:      newVTEP("vtep-update-append", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.71.0.0/24", "100.72.0.0/24"}, Mode: "Managed"}),
		},
	},
	{
		InitialObject: newVTEP("vtep-update-expand", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.73.0.0/24"}, Mode: "Managed"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: expand mask /24 to /20",
			Object:      newVTEP("vtep-update-expand", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.73.0.0/20"}, Mode: "Managed"}),
		},
	},
	{
		InitialObject: newVTEP("vtep-update-expand-append", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.74.0.0/24"}, Mode: "Managed"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: expand mask and append new CIDR simultaneously",
			Object:      newVTEP("vtep-update-expand-append", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.74.0.0/20", "100.75.0.0/24"}, Mode: "Managed"}),
		},
	},
	{
		InitialObject: newVTEP("vtep-update-unmanaged-remove", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.90.0.0/24", "100.91.0.0/24"}, Mode: "Unmanaged"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Unmanaged: remove a CIDR from the list",
			Object:      newVTEP("vtep-update-unmanaged-remove", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.90.0.0/24"}, Mode: "Unmanaged"}),
		},
	},
	{
		InitialObject: newVTEP("vtep-update-unmanaged-shrink", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.92.0.0/24"}, Mode: "Unmanaged"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Unmanaged: shrink mask /24 to /28",
			Object:      newVTEP("vtep-update-unmanaged-shrink", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.92.0.0/28"}, Mode: "Unmanaged"}),
		},
	},
	{
		InitialObject: newVTEP("vtep-update-unmanaged-reorder", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.93.0.0/24", "100.94.0.0/24"}, Mode: "Unmanaged"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Unmanaged: reorder CIDRs (swap positions)",
			Object:      newVTEP("vtep-update-unmanaged-reorder", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.94.0.0/24", "100.93.0.0/24"}, Mode: "Unmanaged"}),
		},
	},
	{
		InitialObject: newVTEP("vtep-update-unmanaged-replace", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.95.0.0/24"}, Mode: "Unmanaged"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Unmanaged: replace CIDR with different network",
			Object:      newVTEP("vtep-update-unmanaged-replace", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.96.0.0/24"}, Mode: "Unmanaged"}),
		},
	},
	{
		InitialObject: newVTEP("vtep-update-unmanaged-expand", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.99.0.0/24"}, Mode: "Unmanaged"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Unmanaged: expand mask /24 to /20",
			Object:      newVTEP("vtep-update-unmanaged-expand", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.99.0.0/20"}, Mode: "Unmanaged"}),
		},
	},
	{
		InitialObject: newVTEP("vtep-update-mode-m-to-u-remove", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.62.0.0/24", "100.63.0.0/24"}, Mode: "Managed"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Mode change: Managed to Unmanaged with CIDR removal",
			Object:      newVTEP("vtep-update-mode-m-to-u-remove", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.62.0.0/24"}, Mode: "Unmanaged"}),
		},
	},
	{
		InitialObject: newVTEP("vtep-update-mode-m-to-u", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.97.0.0/24"}, Mode: "Managed"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Mode change: Managed to Unmanaged with same CIDRs",
			Object:      newVTEP("vtep-update-mode-m-to-u", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.97.0.0/24"}, Mode: "Unmanaged"}),
		},
	},
	{
		InitialObject: newVTEP("vtep-update-mode-u-to-m", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.98.0.0/24"}, Mode: "Unmanaged"}),
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Mode change: Unmanaged to Managed with same CIDRs",
			Object:      newVTEP("vtep-update-mode-u-to-m", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.98.0.0/24"}, Mode: "Managed"}),
		},
	},
}
