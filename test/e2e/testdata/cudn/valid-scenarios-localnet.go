package cudn

import "github.com/ovn-org/ovn-kubernetes/test/e2e/testdata"

var LocalnetValid = []testdata.ValidateCRScenario{
	{
		Manifest: `
---
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-minimal-success
spec:
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: red
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: "test"
      subnets: ["10.0.0.0/24"]
`,
	},
	{
		Manifest: `
---
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-ipam-lifecycle-persistent-success
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
      physicalNetworkName: test
      ipam: {lifecycle: Persistent}
      subnets: ["192.168.0.0/16", "2001:dbb::/64"]
      excludeSubnets: ["192.168.0.1/32", "2001:dbb::1/128"]
      vlan: 3
      mtu: 9000
`,
	},
	{
		Manifest: `
---
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-ipam-disabled-success
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
      physicalNetworkName: test
      ipam: {mode: Disabled}
      vlan: 4094
      mtu: 9000
`,
	},
}
