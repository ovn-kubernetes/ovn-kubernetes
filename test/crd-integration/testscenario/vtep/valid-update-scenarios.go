// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package vtep

import "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"

var ValidUpdates = []testscenario.UpdateCRScenario{
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-noop
spec:
  cidrs:
  - "100.70.0.0/24"
  mode: Managed
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: no change (idempotent update)",
			Name:        "vtep-update-noop",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-noop
spec:
  cidrs:
  - "100.70.0.0/24"
  mode: Managed
`,
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-append
spec:
  cidrs:
  - "100.71.0.0/24"
  mode: Managed
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: append a new CIDR to existing list",
			Name:        "vtep-update-append",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-append
spec:
  cidrs:
  - "100.71.0.0/24"
  - "100.72.0.0/24"
  mode: Managed
`,
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-expand
spec:
  cidrs:
  - "100.73.0.0/24"
  mode: Managed
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: expand mask /24 to /20",
			Name:        "vtep-update-expand",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-expand
spec:
  cidrs:
  - "100.73.0.0/20"
  mode: Managed
`,
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-expand-append
spec:
  cidrs:
  - "100.74.0.0/24"
  mode: Managed
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: expand mask and append new CIDR simultaneously",
			Name:        "vtep-update-expand-append",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-expand-append
spec:
  cidrs:
  - "100.74.0.0/20"
  - "100.75.0.0/24"
  mode: Managed
`,
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-unmanaged-remove
spec:
  cidrs:
  - "100.90.0.0/24"
  - "100.91.0.0/24"
  mode: Unmanaged
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Unmanaged: remove a CIDR from the list",
			Name:        "vtep-update-unmanaged-remove",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-unmanaged-remove
spec:
  cidrs:
  - "100.90.0.0/24"
  mode: Unmanaged
`,
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-unmanaged-shrink
spec:
  cidrs:
  - "100.92.0.0/24"
  mode: Unmanaged
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Unmanaged: shrink mask /24 to /28",
			Name:        "vtep-update-unmanaged-shrink",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-unmanaged-shrink
spec:
  cidrs:
  - "100.92.0.0/28"
  mode: Unmanaged
`,
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-unmanaged-reorder
spec:
  cidrs:
  - "100.93.0.0/24"
  - "100.94.0.0/24"
  mode: Unmanaged
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Unmanaged: reorder CIDRs (swap positions)",
			Name:        "vtep-update-unmanaged-reorder",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-unmanaged-reorder
spec:
  cidrs:
  - "100.94.0.0/24"
  - "100.93.0.0/24"
  mode: Unmanaged
`,
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-unmanaged-replace
spec:
  cidrs:
  - "100.95.0.0/24"
  mode: Unmanaged
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Unmanaged: replace CIDR with different network",
			Name:        "vtep-update-unmanaged-replace",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-unmanaged-replace
spec:
  cidrs:
  - "100.96.0.0/24"
  mode: Unmanaged
`,
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-unmanaged-expand
spec:
  cidrs:
  - "100.99.0.0/24"
  mode: Unmanaged
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Unmanaged: expand mask /24 to /20",
			Name:        "vtep-update-unmanaged-expand",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-unmanaged-expand
spec:
  cidrs:
  - "100.99.0.0/20"
  mode: Unmanaged
`,
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-mode-m-to-u-remove
spec:
  cidrs:
  - "100.62.0.0/24"
  - "100.63.0.0/24"
  mode: Managed
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Mode change: Managed to Unmanaged with CIDR removal",
			Name:        "vtep-update-mode-m-to-u-remove",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-mode-m-to-u-remove
spec:
  cidrs:
  - "100.62.0.0/24"
  mode: Unmanaged
`,
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-mode-m-to-u
spec:
  cidrs:
  - "100.97.0.0/24"
  mode: Managed
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Mode change: Managed to Unmanaged with same CIDRs",
			Name:        "vtep-update-mode-m-to-u",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-mode-m-to-u
spec:
  cidrs:
  - "100.97.0.0/24"
  mode: Unmanaged
`,
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-mode-u-to-m
spec:
  cidrs:
  - "100.98.0.0/24"
  mode: Unmanaged
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Mode change: Unmanaged to Managed with same CIDRs",
			Name:        "vtep-update-mode-u-to-m",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-mode-u-to-m
spec:
  cidrs:
  - "100.98.0.0/24"
  mode: Managed
`,
		},
	},
}
