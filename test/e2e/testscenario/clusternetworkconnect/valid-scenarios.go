package clusternetworkconnect

import "github.com/ovn-org/ovn-kubernetes/test/e2e/testscenario"

var ValidScenarios = []testscenario.ValidateCRScenario{
	{
		Description: "valid CUDN selector with single connectivity type",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: valid-cudn-single
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
  connectivityEnabled: ["PodNetwork"]
`,
	},
	{
		Description: "valid PUDN selector with both connectivity types",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: valid-pudn-both
spec:
  networkSelectors:
    - networkSelectionType: "PrimaryUserDefinedNetworks"
      primaryUserDefinedNetworkSelector:
        namespaceSelector:
          matchLabels:
            name: test
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 24
  connectivityEnabled: ["PodNetwork", "ClusterIPServiceNetwork"]
`,
	},
	{
		Description: "valid dual-stack CIDRs",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:	
  name: valid-dual-stack
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
  connectivityEnabled: ["PodNetwork"]
`,
	},
	{
		Description: "valid single IPv6 CIDR",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: valid-ipv6-only
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
  connectivityEnabled: ["ClusterIPServiceNetwork"]
`,
	},
	{
		Description: "valid multiple network selectors",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: valid-multiple-selectors
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
  connectSubnets:
    - cidr: "192.168.0.0/16"
      networkPrefix: 24
    - cidr: "fd01::/64"
      networkPrefix: 96
  connectivityEnabled: ["PodNetwork", "ClusterIPServiceNetwork"]
`,
	},
	{
		Description: "valid CUDN selector with default connectSubnets",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: valid-default-subnets
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            name: test
  connectivityEnabled: ["PodNetwork"]
`,
	},
}
