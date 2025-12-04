package cudn

import "github.com/ovn-org/ovn-kubernetes/test/e2e/testscenario"

var EVPNCUDNInvalid = []testscenario.ValidateCRScenario{
	{
		Description: "EVPN transport requires evpnConfiguration",
		ExpectedErr: `evpnConfiguration is required if and only if transport is 'EVPN'`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-no-config
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
`,
	},
	{
		Description: "evpnConfiguration without EVPN transport is forbidden",
		ExpectedErr: `evpnConfiguration is required if and only if transport is 'EVPN'`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-config-no-transport
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    evpnConfiguration:
      vtep: evpn-vtep
      macVRF:
        vni: 100
`,
	},
	{
		Description: "EVPN is not supported for Secondary networks",
		ExpectedErr: `transport 'EVPN' is only supported for Layer2 or Layer3 primary networks`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-secondary
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Secondary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpnConfiguration:
      vtep: evpn-vtep
      macVRF:
        vni: 100
`,
	},
	{
		Description: "EVPN is not supported for Localnet topology",
		ExpectedErr: `transport 'EVPN' is only supported for Layer2 or Layer3 primary networks`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-localnet
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: physnet1
    transport: EVPN
    evpnConfiguration:
      vtep: evpn-vtep
      macVRF:
        vni: 100
`,
	},
	{
		Description: "Layer2 EVPN requires macVRF",
		ExpectedErr: `macVRF is required for Layer2 topology when transport is 'EVPN'`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l2-evpn-no-macvrf
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpnConfiguration:
      vtep: evpn-vtep
      ipVRF:
        vni: 100
`,
	},
	{
		Description: "Layer3 EVPN requires ipVRF",
		ExpectedErr: `ipVRF is required for Layer3 topology when transport is 'EVPN'`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l3-evpn-no-ipvrf
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: "10.20.100.0/16"
        hostSubnet: 24
    transport: EVPN
    evpnConfiguration:
      vtep: evpn-vtep
      macVRF:
        vni: 100
`,
	},
	{
		Description: "Layer3 EVPN forbids macVRF",
		ExpectedErr: `macVRF is forbidden for Layer3 topology when transport is 'EVPN'`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l3-evpn-with-macvrf
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: "10.20.100.0/16"
        hostSubnet: 24
    transport: EVPN
    evpnConfiguration:
      vtep: evpn-vtep
      macVRF:
        vni: 100
      ipVRF:
        vni: 101
`,
	},
	{
		Description: "evpnConfiguration requires at least macVRF or ipVRF",
		ExpectedErr: `at least one of macVRF or ipVRF must be specified`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-no-vrf
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpnConfiguration:
      vtep: evpn-vtep
`,
	},
	{
		Description: "VNI must be at least 1",
		ExpectedErr: `spec.network.evpnConfiguration.macVRF.vni: Invalid value`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-vni-zero
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpnConfiguration:
      vtep: evpn-vtep
      macVRF:
        vni: 0
`,
	},
	{
		Description: "VNI must not exceed 16777215",
		ExpectedErr: `spec.network.evpnConfiguration.macVRF.vni: Invalid value`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-vni-too-large
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpnConfiguration:
      vtep: evpn-vtep
      macVRF:
        vni: 16777216
`,
	},
	{
		Description: "routeTarget must be in valid format",
		ExpectedErr: `routeTarget must be in the format '<AS>:<VNI>'`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-invalid-rt-format
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpnConfiguration:
      vtep: evpn-vtep
      macVRF:
        vni: 100
        routeTarget: "invalid"
`,
	},
	{
		Description: "routeTarget AS must be positive",
		ExpectedErr: `AS number in routeTarget must be between 1 and 4294967295`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-invalid-rt-as
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpnConfiguration:
      vtep: evpn-vtep
      macVRF:
        vni: 100
        routeTarget: "0:100"
`,
	},
	{
		Description: "routeTarget VNI must be positive",
		ExpectedErr: `VNI in routeTarget must be between 1 and 16777215`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-invalid-rt-vni
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpnConfiguration:
      vtep: evpn-vtep
      macVRF:
        vni: 100
        routeTarget: "65000:0"
`,
	},
	{
		Description: "VTEP name is required in evpnConfiguration",
		ExpectedErr: `spec.network.evpnConfiguration.vtep`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-no-vtep
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpnConfiguration:
      macVRF:
        vni: 100
`,
	},
}
