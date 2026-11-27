// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package vtep

import (
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var Valid = []testscenario.ValidateCRScenario{
	{
		Description: "Valid VTEP with single IPv4 CIDR and default mode",
		Object:      newVTEP("vtep-ipv4-default", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/24"}}),
	},
	{
		Description: "Valid VTEP with single IPv4 CIDR and Managed mode",
		Object:      newVTEP("vtep-ipv4-managed", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.65.0.0/24"}, Mode: "Managed"}),
	},
	{
		Description: "Valid VTEP with single IPv4 CIDR and Unmanaged mode",
		Object:      newVTEP("vtep-ipv4-unmanaged", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.66.0.0/24"}, Mode: "Unmanaged"}),
	},
	{
		Description: "Valid VTEP with single IPv6 CIDR",
		Object:      newVTEP("vtep-ipv6", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"fd00:100:64::/64"}}),
	},
	{
		Description: "Valid VTEP with dual-stack CIDRs",
		Object:      newVTEP("vtep-dualstack", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.67.0.0/24", "fd00:100:67::/64"}}),
	},
	{
		Description: "Valid VTEP with multiple IPv4 CIDRs for capacity expansion",
		Object:      newVTEP("vtep-multi-ipv4", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.68.0.0/24", "100.68.1.0/24", "100.68.2.0/24"}}),
	},
	{
		Description: "Valid VTEP with multiple CIDRs including mixed families",
		Object:      newVTEP("vtep-multi-mixed", vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.69.0.0/24", "fd00:100:69::/64", "100.69.1.0/24"}}),
	},
}
