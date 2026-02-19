package clusternetworkconnect

import "github.com/ovn-org/ovn-kubernetes/test/e2e/testscenario"

var InvalidScenarios = []testscenario.ValidateCRScenario{

	// =============================================
	// NetworkSelectors Field Validation
	// =============================================

	// Required field validation
	{
		Description: "missing networkSelectors field",
		ExpectedErr: "spec.networkSelectors: Required value",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: missing-network-selectors
spec:
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 24
  connectivity: ["PodNetwork"]
`,
	},
	{
		Description: "empty networkSelectors array",
		ExpectedErr: "spec.networkSelectors: Invalid value: 0: spec.networkSelectors in body should have at least 1 items",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: empty-network-selectors
spec:
  networkSelectors: []
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 24
  connectivity: ["PodNetwork"]
`,
	},

	// Schema validation (array limits)
	{
		Description: "too many networkSelectors",
		ExpectedErr: "spec.networkSelectors: Too many: 6: must have at most 5 items",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: too-many-network-selectors
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test1
    - networkSelectionType: "PrimaryUserDefinedNetworks"
      primaryUserDefinedNetworkSelector:
        namespaceSelector:
          matchLabels:
            name: test2
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test3
    - networkSelectionType: "PrimaryUserDefinedNetworks"
      primaryUserDefinedNetworkSelector:
        namespaceSelector:
          matchLabels:
            name: test4
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test5
    - networkSelectionType: "PrimaryUserDefinedNetworks"
      primaryUserDefinedNetworkSelector:
        namespaceSelector:
          matchLabels:
            name: test6
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 24
  connectivity: ["PodNetwork"]
`,
	},

	// Schema validation (enum values)
	{
		Description: "invalid networkSelector - non-CUDN/PUDN type",
		ExpectedErr: "Unsupported value: \"InvalidType\"",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: invalid-network-selector
spec:
  networkSelectors:
    - networkSelectionType: "InvalidType"
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 24
  connectivity: ["PodNetwork"]
`,
	},
	{
		Description: "invalid networkSelector - mixed valid and invalid types",
		ExpectedErr: "Unsupported value: \"InvalidType\"",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: invalid-mixed-selectors
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test1
    - networkSelectionType: "InvalidType"
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 24
  connectivity: ["PodNetwork"]
`,
	},

	// CEL validation (only CUDN/PUDN allowed)
	{
		Description: "invalid networkSelector - SecondaryUserDefinedNetworks type",
		ExpectedErr: "Only ClusterUserDefinedNetworks or PrimaryUserDefinedNetworks can be selected",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: invalid-secondary-udn-selector
spec:
  networkSelectors:
    - networkSelectionType: "SecondaryUserDefinedNetworks"
      secondaryUserDefinedNetworkSelector:
        namespaceSelector:
          matchLabels:
            name: test
        networkSelector:
          matchLabels:
            name: secondary-net
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 24
  connectivity: ["PodNetwork"]
`,
	},
	{
		Description: "invalid networkSelector - NetworkAttachmentDefinitions type",
		ExpectedErr: "Only ClusterUserDefinedNetworks or PrimaryUserDefinedNetworks can be selected",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: invalid-nad-selector
spec:
  networkSelectors:
    - networkSelectionType: "NetworkAttachmentDefinitions"
      networkAttachmentDefinitionSelector:
        namespaceSelector:
          matchLabels:
            name: test
        networkSelector:
          matchLabels:
            name: my-nad
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 24
  connectivity: ["PodNetwork"]
`,
	},
	{
		Description: "invalid networkSelector - DefaultNetwork type",
		ExpectedErr: "Only ClusterUserDefinedNetworks or PrimaryUserDefinedNetworks can be selected",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: invalid-default-network-selector
spec:
  networkSelectors:
    - networkSelectionType: "DefaultNetwork"
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 24
  connectivity: ["PodNetwork"]
`,
	},

	// =============================================
	// connectivity Field Validation
	// =============================================

	// Required field validation
	{
		Description: "missing connectivity field",
		ExpectedErr: "spec.connectivity: Required value",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: missing-connectivity-enabled
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 24
`,
	},
	{
		Description: "empty connectivity array",
		ExpectedErr: "spec.connectivity: Invalid value: 0: spec.connectivity in body should have at least 1 items",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: empty-connectivity-enabled
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 24
  connectivity: []
`,
	},

	// Schema validation (array limits)
	{
		Description: "too many connectivity values",
		ExpectedErr: "spec.connectivity: Too many: 3: must have at most 2 items",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: too-many-connectivity-enabled
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 24
  connectivity: ["PodNetwork", "ClusterIPServiceNetwork", "PodNetwork"]
`,
	},

	// Schema validation (enum values)
	{
		Description: "invalid connectivity value",
		ExpectedErr: "spec.connectivity[0]: Unsupported value: \"InvalidConnectivity\"",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: invalid-connectivity-value
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 24
  connectivity: ["InvalidConnectivity"]
`,
	},

	// CEL validation (no duplicates)
	{
		Description: "duplicate connectivity types",
		ExpectedErr: "connectivity cannot contain duplicate values",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: duplicate-connectivity
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 24
  connectivity: ["PodNetwork", "PodNetwork"]
`,
	},

	// =============================================
	// ConnectSubnets Field Validation
	// =============================================

	// Schema validation (networkPrefix limits)
	{
		Description: "networkPrefix below minimum (0)",
		ExpectedErr: "spec.connectSubnets[0].networkPrefix: Invalid value: 0: spec.connectSubnets[0].networkPrefix in body should be greater than or equal to 1",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: networkprefix-below-minimum
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 0
  connectivity: ["PodNetwork"]
`,
	},
	{
		Description: "networkPrefix above maximum (128)",
		ExpectedErr: "spec.connectSubnets[0].networkPrefix: Invalid value: 128: spec.connectSubnets[0].networkPrefix in body should be less than or equal to 127",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: networkprefix-above-maximum
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 128
  connectivity: ["PodNetwork"]
`,
	},

	// Schema validation (array limits)
	{
		Description: "too many connectSubnets CIDRs",
		ExpectedErr: "spec.connectSubnets: Too many: 3: must have at most 2 items",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: too-many-connect-subnets
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 24
    - cidr: "fd01::/64"
      networkPrefix: 96
    - cidr: "10.0.0.0/16"
      networkPrefix: 24
  connectivity: ["PodNetwork"]
`,
	},

	// CEL validation (CIDR format)
	{
		Description: "invalid CIDR - host address instead of network",
		ExpectedErr: "CIDR must be a valid network address",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: invalid-cidr-host
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test
  connectSubnets:
    - cidr: "192.168.0.1/16"
      networkPrefix: 24
  connectivity: ["PodNetwork"]
`,
	},
	{
		Description: "invalid CIDR format",
		ExpectedErr: "CIDR must be a valid network address",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: invalid-cidr-format
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test
  connectSubnets:
    - cidr: "not-a-cidr"
      networkPrefix: 24
  connectivity: ["PodNetwork"]
`,
	},

	// CEL validation (dual-stack IP families)
	{
		Description: "same IP family CIDRs",
		ExpectedErr: "When 2 CIDRs are set, they must be from different IP families",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: same-family-cidrs
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 24
    - cidr: "10.0.0.0/16"
      networkPrefix: 24
  connectivity: ["PodNetwork"]
`,
	},
	{
		Description: "same IPv6 family CIDRs",
		ExpectedErr: "When 2 CIDRs are set, they must be from different IP families",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: same-ipv6-family-cidrs
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test
  connectSubnets:
    - cidr: "fd01::/64"
      networkPrefix: 96
    - cidr: "fd02::/64"
      networkPrefix: 96
  connectivity: ["PodNetwork"]
`,
	},

	// CEL validation (dual-stack networkPrefix host bits must match)
	{
		Description: "dual-stack with mismatched networkPrefix host bits - IPv4 /24 vs IPv6 /64",
		ExpectedErr: "networkPrefix must have matching host bits",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: mismatched-host-bits-v4-24-v6-64
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 24
    - cidr: "fd01::/48"
      networkPrefix: 64
  connectivity: ["PodNetwork"]
`,
		// IPv4: 32-24=8 host bits, IPv6: 128-64=64 host bits - MISMATCH!
	},
	{
		Description: "dual-stack with mismatched networkPrefix host bits - IPv4 /28 vs IPv6 /123",
		ExpectedErr: "networkPrefix must have matching host bits",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: mismatched-host-bits-v4-28-v6-123
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test
  connectSubnets:
    - cidr: "192.168.0.0/24"
      networkPrefix: 28
    - cidr: "fd01::/120"
      networkPrefix: 123
  connectivity: ["PodNetwork"]
`,
		// IPv4: 32-28=4 host bits, IPv6: 128-123=5 host bits - MISMATCH!
	},
}
