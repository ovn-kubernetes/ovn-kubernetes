// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import (
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var Layer2CUDNValid = []testscenario.ValidateCRScenario{
	{
		Description: "valid Primary network with defaultGatewayIPs",
		Object: newCUDN("primary-with-default-gateway", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:              "Primary",
				Subnets:           udnv1.DualStackCIDRs{"192.168.1.0/24"},
				DefaultGatewayIPs: udnv1.DualStackIPs{"192.168.1.1"},
			},
		}),
	},
	{
		Description: "valid dual-stack network with defaultGatewayIPs",
		Object: newCUDN("dual-stack-with-gateways", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:              "Primary",
				Subnets:           udnv1.DualStackCIDRs{"192.168.1.0/24", "2001:db8::/64"},
				DefaultGatewayIPs: udnv1.DualStackIPs{"192.168.1.1", "2001:db8::1"},
			},
		}),
	},
	{
		Description: "valid network with infrastructureSubnets",
		Object: newCUDN("network-with-infra-subnets", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:                  "Primary",
				Subnets:               udnv1.DualStackCIDRs{"192.168.1.0/24"},
				InfrastructureSubnets: []udnv1.CIDR{"192.168.1.0/28"},
			},
		}),
	},
	{
		Description: "valid network with defaultGatewayIPs in infrastructureSubnets",
		Object: newCUDN("gateway-in-infra-subnets", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:                  "Primary",
				Subnets:               udnv1.DualStackCIDRs{"192.168.1.0/24"},
				InfrastructureSubnets: []udnv1.CIDR{"192.168.1.0/28"},
				DefaultGatewayIPs:     udnv1.DualStackIPs{"192.168.1.1"},
			},
		}),
	},
	{
		Description: "valid network with reservedSubnets",
		Object: newCUDN("network-with-reserved-subnets", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:            "Primary",
				Subnets:         udnv1.DualStackCIDRs{"192.168.1.0/24"},
				ReservedSubnets: []udnv1.CIDR{"192.168.1.0/28", "192.168.1.16/28"},
			},
		}),
	},
	{
		Description: "valid network with non-overlapping infrastructureSubnets and reservedSubnets",
		Object: newCUDN("non-overlapping-subnets", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:                  "Primary",
				Subnets:               udnv1.DualStackCIDRs{"192.168.1.0/24"},
				ReservedSubnets:       []udnv1.CIDR{"192.168.1.16/28"},
				InfrastructureSubnets: []udnv1.CIDR{"192.168.1.0/28"},
				DefaultGatewayIPs:     udnv1.DualStackIPs{"192.168.1.1"},
			},
		}),
	},
	{
		Description: "valid complete dual-stack configuration",
		Object: newCUDN("complete-dual-stack-config", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:                  "Primary",
				Subnets:               udnv1.DualStackCIDRs{"192.168.1.0/24", "2001:db8::/64"},
				ReservedSubnets:       []udnv1.CIDR{"192.168.1.16/28", "2001:db8::1000/120"},
				InfrastructureSubnets: []udnv1.CIDR{"192.168.1.0/30", "2001:db8::/126"},
				DefaultGatewayIPs:     udnv1.DualStackIPs{"192.168.1.1", "2001:db8::1"},
				IPAM:                  &udnv1.IPAMConfig{Lifecycle: "Persistent"},
			},
		}),
	},
	{
		Description: "valid IPv6-only network",
		Object: newCUDN("ipv6-only-complete", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:                  "Primary",
				Subnets:               udnv1.DualStackCIDRs{"2001:db8::/64"},
				InfrastructureSubnets: []udnv1.CIDR{"2001:db8::/80"},
				DefaultGatewayIPs:     udnv1.DualStackIPs{"2001:db8::1"},
			},
		}),
	},
	{
		Description: "valid Primary network with all fields unset (minimal config)",
		Object: newCUDN("minimal-primary-config", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2:   &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"192.168.1.0/24"}},
		}),
	},
	{
		Description: "valid, secondary, disabled mac-security",
		Object: newCUDN("layer2-mac-security-disabled-success", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:        "Secondary",
				IPAM:        &udnv1.IPAMConfig{Mode: "Disabled"},
				MACSecurity: &udnv1.MACSecurityConfig{Mode: "Disabled"},
			},
		}),
	},
	{
		Description: "valid, secondary, explicitly enabled mac-security",
		Object: newCUDN("layer2-mac-security-explicitly-enabled-success", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:        "Secondary",
				IPAM:        &udnv1.IPAMConfig{Mode: "Disabled"},
				MACSecurity: &udnv1.MACSecurityConfig{Mode: "Enabled"},
			},
		}),
	},
	{
		Description: "valid, primary, ipam.mode unset, explicitly enabled mac-security",
		Object: newCUDN("layer2-mac-security-primary-default-ipam-success", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:        "Primary",
				Subnets:     udnv1.DualStackCIDRs{"192.168.1.0/24", "2001:db8::/64"},
				MACSecurity: &udnv1.MACSecurityConfig{Mode: "Enabled"},
			},
		}),
	},
	{
		Description: "valid, primary, ipam.mode set, explicitly enabled mac-security",
		Object: newCUDN("layer2-mac-security-primary-enabled-ipam-success", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:        "Primary",
				Subnets:     udnv1.DualStackCIDRs{"192.168.1.0/24", "2001:db8::/64"},
				IPAM:        &udnv1.IPAMConfig{Mode: "Enabled"},
				MACSecurity: &udnv1.MACSecurityConfig{Mode: "Enabled"},
			},
		}),
	},
}

// Layer2UDNValid scenarios use UserDefinedNetwork (namespace-scoped) — Name is intentionally
// left empty since cleanupValidateCRsTest only verifies ClusterUserDefinedNetworks.
var Layer2UDNValid = []testscenario.ValidateCRScenario{
	{
		Description: "valid Primary network with defaultGatewayIPs",
		Object: newUDN("primary-with-default-gateway", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:              "Primary",
				Subnets:           udnv1.DualStackCIDRs{"192.168.1.0/24"},
				DefaultGatewayIPs: udnv1.DualStackIPs{"192.168.1.1"},
			},
		}),
	},
	{
		Description: "valid dual-stack network with defaultGatewayIPs",
		Object: newUDN("dual-stack-with-gateways", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:              "Primary",
				Subnets:           udnv1.DualStackCIDRs{"192.168.1.0/24", "2001:db8::/64"},
				DefaultGatewayIPs: udnv1.DualStackIPs{"192.168.1.1", "2001:db8::1"},
			},
		}),
	},
	{
		Description: "valid network with infrastructureSubnets",
		Object: newUDN("network-with-infra-subnets", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:                  "Primary",
				Subnets:               udnv1.DualStackCIDRs{"192.168.1.0/24"},
				InfrastructureSubnets: []udnv1.CIDR{"192.168.1.0/28"},
			},
		}),
	},
	{
		Description: "valid network with defaultGatewayIPs in infrastructureSubnets",
		Object: newUDN("gateway-in-infra-subnets", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:                  "Primary",
				Subnets:               udnv1.DualStackCIDRs{"192.168.1.0/24"},
				InfrastructureSubnets: []udnv1.CIDR{"192.168.1.0/28"},
				DefaultGatewayIPs:     udnv1.DualStackIPs{"192.168.1.1"},
			},
		}),
	},
	{
		Description: "valid network with reservedSubnets",
		Object: newUDN("network-with-reserved-subnets", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:            "Primary",
				Subnets:         udnv1.DualStackCIDRs{"192.168.1.0/24"},
				ReservedSubnets: []udnv1.CIDR{"192.168.1.0/28", "192.168.1.16/28"},
			},
		}),
	},
	{
		Description: "valid network with non-overlapping infrastructureSubnets and reservedSubnets",
		Object: newUDN("non-overlapping-subnets", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:                  "Primary",
				Subnets:               udnv1.DualStackCIDRs{"192.168.1.0/24"},
				ReservedSubnets:       []udnv1.CIDR{"192.168.1.16/28"},
				InfrastructureSubnets: []udnv1.CIDR{"192.168.1.0/28"},
				DefaultGatewayIPs:     udnv1.DualStackIPs{"192.168.1.1"},
			},
		}),
	},
	{
		Description: "valid complete dual-stack configuration",
		Object: newUDN("complete-dual-stack-config", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:                  "Primary",
				Subnets:               udnv1.DualStackCIDRs{"192.168.1.0/24", "2001:db8::/64"},
				ReservedSubnets:       []udnv1.CIDR{"192.168.1.16/28", "2001:db8::1000/120"},
				InfrastructureSubnets: []udnv1.CIDR{"192.168.1.0/30", "2001:db8::/126"},
				DefaultGatewayIPs:     udnv1.DualStackIPs{"192.168.1.1", "2001:db8::1"},
				IPAM:                  &udnv1.IPAMConfig{Lifecycle: "Persistent"},
			},
		}),
	},
	{
		Description: "valid IPv6-only network",
		Object: newUDN("ipv6-only-complete", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:                  "Primary",
				Subnets:               udnv1.DualStackCIDRs{"2001:db8::/64"},
				InfrastructureSubnets: []udnv1.CIDR{"2001:db8::/80"},
				DefaultGatewayIPs:     udnv1.DualStackIPs{"2001:db8::1"},
			},
		}),
	},
	{
		Description: "valid, secondary, disabled mac-security",
		Object: newUDN("layer2-mac-security-disabled-success", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:        "Secondary",
				IPAM:        &udnv1.IPAMConfig{Mode: "Disabled"},
				MACSecurity: &udnv1.MACSecurityConfig{Mode: "Disabled"},
			},
		}),
	},
	{
		Description: "valid, secondary, explicitly enabled mac-security",
		Object: newUDN("layer2-mac-security-explicitly-enabled-success", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:        "Secondary",
				IPAM:        &udnv1.IPAMConfig{Mode: "Disabled"},
				MACSecurity: &udnv1.MACSecurityConfig{Mode: "Enabled"},
			},
		}),
	},
	{
		Description: "valid, primary, ipam.mode unset, explicitly enabled mac-security",
		Object: newUDN("layer2-mac-security-primary-default-ipam-success", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:        "Primary",
				Subnets:     udnv1.DualStackCIDRs{"192.168.1.0/24", "2001:db8::/64"},
				MACSecurity: &udnv1.MACSecurityConfig{Mode: "Enabled"},
			},
		}),
	},
	{
		Description: "valid, primary, ipam.mode set, explicitly enabled mac-security",
		Object: newUDN("layer2-mac-security-primary-enabled-ipam-success", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:        "Primary",
				Subnets:     udnv1.DualStackCIDRs{"192.168.1.0/24", "2001:db8::/64"},
				IPAM:        &udnv1.IPAMConfig{Mode: "Enabled"},
				MACSecurity: &udnv1.MACSecurityConfig{Mode: "Enabled"},
			},
		}),
	},
}
