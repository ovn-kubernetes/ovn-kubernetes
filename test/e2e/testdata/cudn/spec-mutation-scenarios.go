package cudn

import "github.com/ovn-org/ovn-kubernetes/test/e2e/testdata"

var Layer2SpecMutationScenarios = []testdata.ValidateCRScenario{
	{
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: layer2-test
spec:
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: red
  network:
    topology: Layer2
    layer2:
      role: Secondary
      subnets: ["10.0.0.0/24"]`,
		MutatedManifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: layer2-test
spec:
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: red
  network:
    topology: Layer2
    layer2:
      role: Secondary
      subnets: ["10.0.0.0/24"]
      mtu: 9000`,
		ExpectedErr: `The ClusterUserDefinedNetwork "layer2-test" is invalid: spec.network.layer2: Invalid value: "object": layer2 topology config is immutable`,
	},
}

var Layer3SpecMutationScenarios = []testdata.ValidateCRScenario{
	{
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: layer3-test
spec:
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: red
  network:
    topology: Layer3
    layer3:
      role: Secondary
      subnets: [ { cidr: "10.0.0.0/16", hostSubnet: 24 } ]
`,
		MutatedManifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: layer3-test
spec:
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: red
  network:
    topology: Layer3
    layer3:
      role: Secondary
      subnets: [ { cidr: "10.0.0.0/16", hostSubnet: 24 } ]
      mtu: 9000
`,
		ExpectedErr: `The ClusterUserDefinedNetwork "layer3-test" is invalid: spec.network.layer3: Invalid value: "object": layer3 topology config is immutable`,
	},
}

var LocalnetSpecMutationScenarios = []testdata.ValidateCRScenario{
	{
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-test
spec:
  namespaceSelector:
    matchExpressions:
      - key: kubernetes.io/metadata.name
        operator: In
        values: ["red", "blue"]
  network:
    topology: Localnet
    localnet:
      role: Secondary
      subnets: ["192.168.0.0/16", "2001:dbb::/64"]
      physicalNetworkName: test
      excludeSubnets: ["192.168.0.1/32", "2001:dbb::1/128"]
      vlan: 100
`,
		MutatedManifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-test
spec:
  namespaceSelector:
    matchExpressions:
      - key: kubernetes.io/metadata.name
        operator: In
        values: ["red", "blue"]
  network:
    topology: Localnet
    localnet:
      role: Secondary
      subnets: ["192.168.0.0/16", "2001:dbb::/64"]
      physicalNetworkName: test-mutated
      excludeSubnets: ["192.168.0.1/32", "2001:dbb::1/128", "192.168.100.1/32", "2001:dbb::2/128"]
      vlan: 200
      mtu: 9000
`,
	},
}
