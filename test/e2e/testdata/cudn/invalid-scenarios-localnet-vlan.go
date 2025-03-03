package cudn

import "github.com/ovn-org/ovn-kubernetes/test/e2e/testdata"

var LocalnetInvalidVLAN = []testdata.ValidateCRScenario{
	{
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-vlan-id-lower-then-2-fail
spec:
  namespaceSelector: {matchLabels: { kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: "test"
      subnets: [ "192.168.0.0/16" ]
      vlan: 0
`,
		ExpectedErr: `spec.network.localnet.vlan in body should be greater than or equal to 2`,
	},
	{
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-vlan-id-higher-then-4094-fail
spec:
  namespaceSelector: {matchLabels: { kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: "test"
      subnets: [ "192.168.0.0/16" ]
      vlan: 4095
`,
		ExpectedErr: `spec.network.localnet.vlan in body should be less than or equal to 4094`,
	},
}
