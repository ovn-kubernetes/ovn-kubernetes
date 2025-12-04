package cudn

import "github.com/ovn-org/ovn-kubernetes/test/e2e/testscenario"

var EVPNCUDNValid = []testscenario.ValidateCRScenario{
	{
		Description: "valid Layer2 Primary EVPN with macVRF",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l2-evpn-primary
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
`,
	},
	{
		Description: "valid Layer2 Primary EVPN with macVRF and routeTarget",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l2-evpn-with-rt
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
        routeTarget: "65000:100"
`,
	},
	{
		Description: "valid Layer2 Primary EVPN with both macVRF and ipVRF",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l2-evpn-mac-ip-vrf
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
      ipVRF:
        vni: 101
`,
	},
	{
		Description: "valid Layer3 Primary EVPN with ipVRF",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l3-evpn-primary
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
    evpn:
      vtep: evpn-vtep
      ipVRF:
        vni: 200
`,
	},
	{
		Description: "valid Layer3 Primary EVPN with ipVRF and routeTarget",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l3-evpn-with-rt
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
    evpn:
      vtep: evpn-vtep
      ipVRF:
        vni: 200
        routeTarget: "65000:200"
`,
	},
	{
		Description: "valid Layer2 Primary EVPN dual-stack",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l2-evpn-dual-stack
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24", "2001:db8::/64"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 300
`,
	},
	{
		Description: "valid EVPN with max VNI value",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-max-vni
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 16777215
`,
	},
	{
		Description: "valid EVPN with 4-byte ASN in routeTarget",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-4byte-asn
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
        routeTarget: "4200000000:100"
`,
	},
}
