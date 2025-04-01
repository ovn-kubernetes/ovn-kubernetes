package cudn

import "github.com/ovn-org/ovn-kubernetes/test/e2e/testdata"

var LocalnetValidSpecMutationScenarios = []testdata.ValidateCRScenario{
	{
		Description: "should allow localnet topology config mutation (for VLAN, MTU, PhysicalNetworkName, ExcludeSubnets)",
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
      vlan:
        mode: Access
        access: {id: 100}
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
      vlan:
        mode: Access
        access: {id: 200}
      mtu: 9000
`,
	},
}
