package vtep

import "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/testscenario"

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
		Description: "CIDRs cannot have more than 10 items",
		ExpectedErr: `spec.cidrs: Too many`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-too-many-cidrs
spec:
  cidrs:
  - "10.20.0.0/24"
  - "10.21.0.0/24"
  - "10.22.0.0/24"
  - "10.23.0.0/24"
  - "10.24.0.0/24"
  - "10.25.0.0/24"
  - "10.26.0.0/24"
  - "10.27.0.0/24"
  - "10.28.0.0/24"
  - "10.29.0.0/24"
  - "10.30.0.0/24"
`,
	},
	{
		Description: "IPv6 CIDRs are not supported (FRR limitation)",
		ExpectedErr: `Only IPv4 CIDRs are currently supported`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-ipv6-not-supported
spec:
  cidrs:
  - "fd00:100:64::/64"
`,
	},
	{
		Description: "Dual-stack CIDRs are not supported (IPv6 not supported by FRR)",
		ExpectedErr: `Only IPv4 CIDRs are currently supported`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-dualstack-not-supported
spec:
  cidrs:
  - "100.64.0.0/24"
  - "fd00:100:64::/64"
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
}
