// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package vtep

import "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"

var Valid = []testscenario.ValidateCRScenario{
	{
		Description: "Valid VTEP with single IPv4 CIDR and default mode",
		Name:        "vtep-ipv4-default",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-ipv4-default
spec:
  cidrs:
  - "100.64.0.0/24"
`,
	},
	{
		Description: "Valid VTEP with single IPv4 CIDR and Managed mode",
		Name:        "vtep-ipv4-managed",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-ipv4-managed
spec:
  cidrs:
  - "100.65.0.0/24"
  mode: Managed
`,
	},
	{
		Description: "Valid VTEP with single IPv4 CIDR and Unmanaged mode",
		Name:        "vtep-ipv4-unmanaged",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-ipv4-unmanaged
spec:
  cidrs:
  - "100.66.0.0/24"
  mode: Unmanaged
`,
	},
	{
		Description: "Valid VTEP with single IPv6 CIDR",
		Name:        "vtep-ipv6",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-ipv6
spec:
  cidrs:
  - "fd00:100:64::/64"
`,
	},
	{
		Description: "Valid VTEP with dual-stack CIDRs",
		Name:        "vtep-dualstack",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-dualstack
spec:
  cidrs:
  - "100.67.0.0/24"
  - "fd00:100:67::/64"
`,
	},
	{
		Description: "Valid VTEP with multiple IPv4 CIDRs for capacity expansion",
		Name:        "vtep-multi-ipv4",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-multi-ipv4
spec:
  cidrs:
  - "100.68.0.0/24"
  - "100.68.1.0/24"
  - "100.68.2.0/24"
`,
	},
	{
		Description: "Valid VTEP with multiple CIDRs including mixed families",
		Name:        "vtep-multi-mixed",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-multi-mixed
spec:
  cidrs:
  - "100.69.0.0/24"
  - "fd00:100:69::/64"
  - "100.69.1.0/24"
`,
	},
}
