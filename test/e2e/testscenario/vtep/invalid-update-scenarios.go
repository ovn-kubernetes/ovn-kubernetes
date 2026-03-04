package vtep

import "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/testscenario"

var InvalidUpdates = []testscenario.UpdateCRScenario{
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Remove a CIDR from the list",
			Name:        "vtep-update-remove",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-remove
spec:
  cidrs:
  - "100.80.0.0/24"
`,
			ExpectedErr: "CIDRs cannot be removed",
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-remove
spec:
  cidrs:
  - "100.80.0.0/24"
  - "100.81.0.0/24"
`,
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Replace CIDR with different network (same count)",
			Name:        "vtep-update-replace",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-replace
spec:
  cidrs:
  - "10.0.0.0/24"
`,
			ExpectedErr: "Existing CIDRs must remain at the same position",
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-replace
spec:
  cidrs:
  - "100.82.0.0/24"
`,
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Shrink mask /24 to /28",
			Name:        "vtep-update-shrink",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-shrink
spec:
  cidrs:
  - "100.83.0.0/28"
`,
			ExpectedErr: "Existing CIDRs must remain at the same position",
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-shrink
spec:
  cidrs:
  - "100.83.0.0/24"
`,
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Shrink /24 to /32 (subnet to single host)",
			Name:        "vtep-update-shrink-to-32",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-shrink-to-32
spec:
  cidrs:
  - "100.84.0.0/32"
`,
			ExpectedErr: "Existing CIDRs must remain at the same position",
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-shrink-to-32
spec:
  cidrs:
  - "100.84.0.0/24"
`,
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Reorder CIDRs (swap positions)",
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
`,
			ExpectedErr: "Existing CIDRs must remain at the same position",
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-reorder
spec:
  cidrs:
  - "100.85.0.0/24"
  - "100.86.0.0/24"
`,
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Replace /32 with a different /32",
			Name:        "vtep-update-replace-32",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-replace-32
spec:
  cidrs:
  - "100.87.0.2/32"
`,
			ExpectedErr: "Existing CIDRs must remain at the same position",
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-replace-32
spec:
  cidrs:
  - "100.87.0.1/32"
`,
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Replace with same-size different network",
			Name:        "vtep-update-diff-network",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-diff-network
spec:
  cidrs:
  - "100.89.0.0/24"
`,
			ExpectedErr: "Existing CIDRs must remain at the same position",
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-diff-network
spec:
  cidrs:
  - "100.88.0.0/24"
`,
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Expand to non-containing wider network (wider mask but old IP not in new range)",
			Name:        "vtep-update-non-containing",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-non-containing
spec:
  cidrs:
  - "10.0.0.0/8"
`,
			ExpectedErr: "Existing CIDRs must remain at the same position",
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-non-containing
spec:
  cidrs:
  - "100.88.0.0/24"
`,
	},
}
