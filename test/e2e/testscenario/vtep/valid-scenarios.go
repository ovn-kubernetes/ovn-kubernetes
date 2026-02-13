package vtep

import "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/testscenario"

var Valid = []testscenario.ValidateCRScenario{
	{
		Description: "Valid VTEP with single IPv4 CIDR and default mode",
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
		Description: "Valid VTEP with dual-stack CIDRs (IPv6 first)",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-dualstack-ipv6-first
spec:
  cidrs:
  - "fd00:100:68::/64"
  - "100.68.0.0/24"
`,
	},
}
