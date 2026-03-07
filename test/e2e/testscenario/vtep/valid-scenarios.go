package vtep

import "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/testscenario"

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
		Description: "Valid VTEP with two non-overlapping IPv4 CIDRs (even count)",
		Name:        "vtep-two-ipv4",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-two-ipv4
spec:
  cidrs:
  - "100.67.0.0/24"
  - "100.68.0.0/24"
`,
	},
	{
		Description: "Valid VTEP with three non-overlapping IPv4 CIDRs (odd count)",
		Name:        "vtep-three-ipv4",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-three-ipv4
spec:
  cidrs:
  - "100.90.0.0/24"
  - "100.91.0.0/24"
  - "100.92.0.0/24"
`,
	},
}
