package cudn

import "github.com/ovn-org/ovn-kubernetes/test/e2e/testdata"

var Layer2InvalidSpecMutationScenarios = []testdata.ValidateCRScenario{
	{
		Description: "should reject layer2 topology config mutation",
		ExpectedErr: `layer2 topology config is immutable`,
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
	},
}

var Layer3InvalidSpecMutationScenarios = []testdata.ValidateCRScenario{
	{
		Description: "should reject layer3 topology config mutation",
		ExpectedErr: `layer3 topology config is immutable`,
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
	},
}

var LocalnetInvalidSpecMutationScenarios = []testdata.ValidateCRScenario{
	{
		Description: "should reject localnet topology config mutation - subnets",
		ExpectedErr: "subnets is immutable",
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
      subnets: ["10.100.0.0/16", "2001:dbb::/64"]
      physicalNetworkName: test
`,
	},
}
