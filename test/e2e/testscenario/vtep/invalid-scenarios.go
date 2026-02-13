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
		Description: "CIDRs cannot have more than 2 items",
		ExpectedErr: `spec.cidrs: Too many`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-too-many-cidrs
spec:
  cidrs:
  - "10.20.100.0/24"
  - "10.30.100.0/24"
  - "10.40.100.0/24"
`,
	},
	{
		Description: "Dual-stack CIDRs must be from different IP families",
		ExpectedErr: `When 2 CIDRs are set, they must be from different IP families`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-same-family-cidrs
spec:
  cidrs:
  - "10.20.100.0/24"
  - "10.30.100.0/24"
`,
	},
	{
		Description: "Dual-stack CIDRs must be from different IP families (IPv6)",
		ExpectedErr: `When 2 CIDRs are set, they must be from different IP families`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep-same-family-cidrs-ipv6
spec:
  cidrs:
  - "fd00:10:20::/64"
  - "fd00:10:30::/64"
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
