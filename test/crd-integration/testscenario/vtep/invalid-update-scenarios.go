// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package vtep

import "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"

var InvalidUpdates = []testscenario.UpdateCRScenario{
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-remove
spec:
  cidrs:
  - "100.80.0.0/24"
  - "100.81.0.0/24"
  mode: Managed
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: remove a CIDR from the list",
			Name:        "vtep-update-remove",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-remove
spec:
  cidrs:
  - "100.80.0.0/24"
  mode: Managed
`,
			ExpectedErr: "CIDRs cannot be removed in managed mode",
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-remove-multi
spec:
  cidrs:
  - "100.64.0.0/24"
  - "100.65.0.0/24"
  - "100.66.0.0/24"
  mode: Managed
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: remove multiple CIDRs (3 to 1) produces clean append-only error, not a CEL runtime out-of-bounds error from the position/mask rule",
			Name:        "vtep-update-remove-multi",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-remove-multi
spec:
  cidrs:
  - "100.64.0.0/24"
  mode: Managed
`,
			ExpectedErr: "CIDRs cannot be removed in managed mode",
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-replace
spec:
  cidrs:
  - "100.82.0.0/24"
  mode: Managed
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: replace CIDR with different network (same count)",
			Name:        "vtep-update-replace",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-replace
spec:
  cidrs:
  - "10.0.0.0/24"
  mode: Managed
`,
			ExpectedErr: "In managed mode, existing CIDRs must remain at the same position",
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-shrink
spec:
  cidrs:
  - "100.83.0.0/24"
  mode: Managed
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: shrink mask /24 to /28",
			Name:        "vtep-update-shrink",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-shrink
spec:
  cidrs:
  - "100.83.0.0/28"
  mode: Managed
`,
			ExpectedErr: "In managed mode, existing CIDRs must remain at the same position",
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-reorder
spec:
  cidrs:
  - "100.85.0.0/24"
  - "100.86.0.0/24"
  mode: Managed
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: reorder CIDRs (swap positions)",
			Name:        "vtep-update-reorder",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-reorder
spec:
  cidrs:
  - "100.86.0.0/24"
  - "100.85.0.0/24"
  mode: Managed
`,
			ExpectedErr: "In managed mode, existing CIDRs must remain at the same position",
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-non-containing
spec:
  cidrs:
  - "100.88.0.0/24"
  mode: Managed
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: expand to non-containing wider network",
			Name:        "vtep-update-non-containing",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-non-containing
spec:
  cidrs:
  - "10.0.0.0/8"
  mode: Managed
`,
			ExpectedErr: "In managed mode, existing CIDRs must remain at the same position",
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-shrink-to-32
spec:
  cidrs:
  - "100.84.0.0/24"
  mode: Managed
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: shrink /24 to /32 (subnet to single host)",
			Name:        "vtep-update-shrink-to-32",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-shrink-to-32
spec:
  cidrs:
  - "100.84.0.0/32"
  mode: Managed
`,
			ExpectedErr: "In managed mode, existing CIDRs must remain at the same position",
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-replace-32
spec:
  cidrs:
  - "100.87.0.1/32"
  mode: Managed
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: replace /32 with a different /32",
			Name:        "vtep-update-replace-32",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-replace-32
spec:
  cidrs:
  - "100.87.0.2/32"
  mode: Managed
`,
			ExpectedErr: "In managed mode, existing CIDRs must remain at the same position",
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-mode-u-to-m-remove
spec:
  cidrs:
  - "100.60.0.0/24"
  - "100.61.0.0/24"
  mode: Unmanaged
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Mode change to Managed with CIDR removal",
			Name:        "vtep-update-mode-u-to-m-remove",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-mode-u-to-m-remove
spec:
  cidrs:
  - "100.60.0.0/24"
  mode: Managed
`,
			ExpectedErr: "CIDRs cannot be removed in managed mode",
		},
	},
	{
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-default-remove
spec:
  cidrs:
  - "100.89.0.0/24"
  - "100.89.1.0/24"
`,
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Default mode (omitted): remove a CIDR triggers managed rules",
			Name:        "vtep-update-default-remove",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-default-remove
spec:
  cidrs:
  - "100.89.0.0/24"
`,
			ExpectedErr: "CIDRs cannot be removed in managed mode",
		},
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: append a CIDR that overlaps with an existing one",
			Name:        "vtep-update-append-overlap",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-append-overlap
spec:
  cidrs:
  - "100.93.0.0/16"
  - "100.93.1.0/24"
  mode: Managed
`,
			ExpectedErr: "CIDRs must not overlap with each other",
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-append-overlap
spec:
  cidrs:
  - "100.93.0.0/16"
  mode: Managed
`,
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Managed: expand mask causing overlap with another CIDR",
			Name:        "vtep-update-expand-overlap",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-expand-overlap
spec:
  cidrs:
  - "100.94.0.0/16"
  - "100.94.1.0/24"
  mode: Managed
`,
			ExpectedErr: "CIDRs must not overlap with each other",
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-expand-overlap
spec:
  cidrs:
  - "100.94.0.0/24"
  - "100.94.1.0/24"
  mode: Managed
`,
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Unmanaged: overlapping CIDRs are also rejected",
			Name:        "vtep-update-unmanaged-overlap",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-unmanaged-overlap
spec:
  cidrs:
  - "100.95.0.0/16"
  - "100.95.1.0/24"
  mode: Unmanaged
`,
			ExpectedErr: "CIDRs must not overlap with each other",
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-unmanaged-overlap
spec:
  cidrs:
  - "100.95.0.0/24"
  mode: Unmanaged
`,
	},
}
