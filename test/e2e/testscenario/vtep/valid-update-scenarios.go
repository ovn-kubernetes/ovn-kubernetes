package vtep

import "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/testscenario"

var ValidUpdates = []testscenario.UpdateCRScenario{
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "No change (idempotent update)",
			Name:        "vtep-update-noop",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-noop
spec:
  cidrs:
  - "100.70.0.0/24"
`,
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-noop
spec:
  cidrs:
  - "100.70.0.0/24"
`,
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Append a new CIDR to existing list",
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
`,
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-append
spec:
  cidrs:
  - "100.71.0.0/24"
`,
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Expand mask at position 0 (/24 to /20)",
			Name:        "vtep-update-expand",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-expand
spec:
  cidrs:
  - "100.73.0.0/20"
`,
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-expand
spec:
  cidrs:
  - "100.73.0.0/24"
`,
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Expand mask and append new CIDR simultaneously",
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
`,
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-expand-append
spec:
  cidrs:
  - "100.74.0.0/24"
`,
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Expand /32 single-host to /24 subnet",
			Name:        "vtep-update-expand-32-to-24",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-expand-32-to-24
spec:
  cidrs:
  - "100.76.0.0/24"
`,
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-expand-32-to-24
spec:
  cidrs:
  - "100.76.0.1/32"
`,
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Expand /31 point-to-point to /24 subnet",
			Name:        "vtep-update-expand-31-to-24",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-expand-31-to-24
spec:
  cidrs:
  - "100.77.0.0/24"
`,
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-expand-31-to-24
spec:
  cidrs:
  - "100.77.0.0/31"
`,
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "Expand only second CIDR while first stays unchanged",
			Name:        "vtep-update-expand-second",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-expand-second
spec:
  cidrs:
  - "100.78.0.0/24"
  - "100.79.0.0/20"
`,
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-update-expand-second
spec:
  cidrs:
  - "100.78.0.0/24"
  - "100.79.0.0/24"
`,
	},
}
