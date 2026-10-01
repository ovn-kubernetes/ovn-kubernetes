// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package vtep

import "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"

var Invalid = []testscenario.ValidateCRScenario{
	{
		Description: "CIDR must be a valid network address (not a host IP)",
		ExpectedErr: `CIDR must be a valid network address`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-invalid-cidr-not-network
spec:
  cidrs:
  - "10.20.100.1/24"
`,
	},
	{
		Description: "CIDR cannot be empty",
		ExpectedErr: `spec.cidrs: Invalid value`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-empty-cidrs
spec:
  cidrs: []
`,
	},
	{
		Description: "CIDRs cannot have more than 20 items",
		ExpectedErr: `spec.cidrs: Too many`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-too-many-cidrs
spec:
  cidrs:
  - "10.0.0.0/24"
  - "10.1.0.0/24"
  - "10.2.0.0/24"
  - "10.3.0.0/24"
  - "10.4.0.0/24"
  - "10.5.0.0/24"
  - "10.6.0.0/24"
  - "10.7.0.0/24"
  - "10.8.0.0/24"
  - "10.9.0.0/24"
  - "10.10.0.0/24"
  - "10.11.0.0/24"
  - "10.12.0.0/24"
  - "10.13.0.0/24"
  - "10.14.0.0/24"
  - "10.15.0.0/24"
  - "10.16.0.0/24"
  - "10.17.0.0/24"
  - "fd00:1::/64"
  - "fd00:2::/64"
  - "fd00:3::/64"
`,
	},
	{
		Description: "CIDR must be in valid format",
		ExpectedErr: `CIDR must be a valid network address`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-invalid-cidr-format
spec:
  cidrs:
  - "invalid-cidr"
`,
	},
	{
		Description: "Mode must be a valid enum value",
		ExpectedErr: `spec.mode: Unsupported value`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-invalid-mode
spec:
  cidrs:
  - "10.20.100.0/24"
  mode: "InvalidMode"
`,
	},
	{
		Description: "CIDRs must not overlap (superset contains subset)",
		ExpectedErr: `CIDRs must not overlap with each other`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-overlap-superset
spec:
  cidrs:
  - "10.30.0.0/16"
  - "10.30.1.0/24"
`,
	},
	{
		Description: "CIDRs must not overlap (identical CIDRs)",
		ExpectedErr: `CIDRs must not overlap with each other`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-overlap-identical
spec:
  cidrs:
  - "10.31.0.0/24"
  - "10.31.0.0/24"
`,
	},
}
