package cudn

import "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/testscenario"

var Layer2CUDNValid = []testscenario.ValidateCRScenario{
	{
		Description: "valid Primary network with defaultGatewayIPs",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-with-default-gateway
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["192.168.1.0/24"]
      defaultGatewayIPs: ["192.168.1.1"]
`,
	},
	{
		Description: "valid dual-stack network with defaultGatewayIPs",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: dual-stack-with-gateways
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["192.168.1.0/24", "2001:db8::/64"]
      defaultGatewayIPs: ["192.168.1.1", "2001:db8::1"]
`,
	},
	{
		Description: "valid network with infrastructureSubnets",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: network-with-infra-subnets
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["192.168.1.0/24"]
      infrastructureSubnets: ["192.168.1.0/28"]
`,
	},
	{
		Description: "valid network with defaultGatewayIPs in infrastructureSubnets",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: gateway-in-infra-subnets
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["192.168.1.0/24"]
      infrastructureSubnets: ["192.168.1.0/28"]
      defaultGatewayIPs: ["192.168.1.1"]
`,
	},
	{
		Description: "valid network with reservedSubnets",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: network-with-reserved-subnets
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["192.168.1.0/24"]
      reservedSubnets: ["192.168.1.0/28", "192.168.1.16/28"]
`,
	},
	{
		Description: "valid network with non-overlapping infrastructureSubnets and reservedSubnets",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: non-overlapping-subnets
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["192.168.1.0/24"]
      infrastructureSubnets: ["192.168.1.0/28"]
      reservedSubnets: ["192.168.1.16/28"]
      defaultGatewayIPs: ["192.168.1.1"]
`,
	},
	{
		Description: "valid complete dual-stack configuration",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: complete-dual-stack-config
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["192.168.1.0/24", "2001:db8::/64"]
      infrastructureSubnets: ["192.168.1.0/30", "2001:db8::/126"]
      reservedSubnets: ["192.168.1.16/28", "2001:db8::1000/120"]
      defaultGatewayIPs: ["192.168.1.1", "2001:db8::1"]
      ipam:
        lifecycle: Persistent
`,
	},
	{
		Description: "valid IPv6-only network",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: ipv6-only-complete
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["2001:db8::/64"]
      infrastructureSubnets: ["2001:db8::/80"]
      defaultGatewayIPs: ["2001:db8::1"]
`,
	},
	{
		Description: "valid Primary network with all fields unset (minimal config)",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: minimal-primary-config
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["192.168.1.0/24"]
`,
	},
}

var Layer2UDNValid = []testscenario.ValidateCRScenario{
	{
		Description: "valid Primary network with defaultGatewayIPs",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: primary-with-default-gateway
spec:
  topology: Layer2
  layer2:
    role: Primary
    subnets: ["192.168.1.0/24"]
    defaultGatewayIPs: ["192.168.1.1"]
`,
	},
	{
		Description: "valid dual-stack network with defaultGatewayIPs",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: dual-stack-with-gateways
spec:
  topology: Layer2
  layer2:
    role: Primary
    subnets: ["192.168.1.0/24", "2001:db8::/64"]
    defaultGatewayIPs: ["192.168.1.1", "2001:db8::1"]
`,
	},
	{
		Description: "valid network with infrastructureSubnets",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: network-with-infra-subnets
spec:
  topology: Layer2
  layer2:
    role: Primary
    subnets: ["192.168.1.0/24"]
    infrastructureSubnets: ["192.168.1.0/28"]
`,
	},
	{
		Description: "valid network with defaultGatewayIPs in infrastructureSubnets",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: gateway-in-infra-subnets
spec:
  topology: Layer2
  layer2:
    role: Primary
    subnets: ["192.168.1.0/24"]
    infrastructureSubnets: ["192.168.1.0/28"]
    defaultGatewayIPs: ["192.168.1.1"]
`,
	},
	{
		Description: "valid network with reservedSubnets",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: network-with-reserved-subnets
spec:
  topology: Layer2
  layer2:
    role: Primary
    subnets: ["192.168.1.0/24"]
    reservedSubnets: ["192.168.1.0/28", "192.168.1.16/28"]
`,
	},
	{
		Description: "valid network with non-overlapping infrastructureSubnets and reservedSubnets",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: non-overlapping-subnets
spec:
  topology: Layer2
  layer2:
    role: Primary
    subnets: ["192.168.1.0/24"]
    infrastructureSubnets: ["192.168.1.0/28"]
    reservedSubnets: ["192.168.1.16/28"]
    defaultGatewayIPs: ["192.168.1.1"]
`,
	},
	{
		Description: "valid complete dual-stack configuration",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: complete-dual-stack-config
spec:
  topology: Layer2
  layer2:
    role: Primary
    subnets: ["192.168.1.0/24", "2001:db8::/64"]
    infrastructureSubnets: ["192.168.1.0/30", "2001:db8::/126"]
    reservedSubnets: ["192.168.1.16/28", "2001:db8::1000/120"]
    defaultGatewayIPs: ["192.168.1.1", "2001:db8::1"]
    ipam:
      lifecycle: Persistent
`,
	},
	{
		Description: "valid IPv6-only network",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: ipv6-only-complete
spec:
  topology: Layer2
  layer2:
    role: Primary
    subnets: ["2001:db8::/64"]
    infrastructureSubnets: ["2001:db8::/80"]
    defaultGatewayIPs: ["2001:db8::1"]
`,
	},
}
