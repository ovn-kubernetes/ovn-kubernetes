// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import (
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var Layer2CUDNInvalid = []testscenario.ValidateCRScenario{
	{
		Description: "defaultGatewayIPs is not allowed for Secondary network",
		ExpectedErr: `defaultGatewayIPs is only supported for Primary network`,
		Object: newCUDN("default-gateway-secondary-fail", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:              "Secondary",
				Subnets:           udnv1.DualStackCIDRs{"192.168.1.0/24"},
				DefaultGatewayIPs: udnv1.DualStackIPs{"192.168.1.1"},
			},
		}),
	},
	{
		Description: "defaultGatewayIPs must belong to subnets",
		ExpectedErr: `defaultGatewayIPs must belong to one of the subnets specified in the subnets field`,
		Object: newCUDN("default-gateway-outside-subnet-fail", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:              "Primary",
				Subnets:           udnv1.DualStackCIDRs{"192.168.1.0/24"},
				DefaultGatewayIPs: udnv1.DualStackIPs{"10.0.0.1"},
			},
		}),
	},
	{
		Description: "defaultGatewayIPs must belong to infrastructureSubnets when specified",
		ExpectedErr: `defaultGatewayIPs have to belong to infrastructureSubnets`,
		Object: newCUDN("default-gateway-outside-infra-fail", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:                  "Primary",
				Subnets:               udnv1.DualStackCIDRs{"192.168.1.0/24"},
				InfrastructureSubnets: []udnv1.CIDR{"192.168.1.0/28"},
				DefaultGatewayIPs:     udnv1.DualStackIPs{"192.168.1.20"},
			},
		}),
	},
	{
		Description: "dual-stack defaultGatewayIPs one IP outside subnet",
		ExpectedErr: `defaultGatewayIPs must belong to one of the subnets specified in the subnets field`,
		Object: newCUDN("default-gateway-dual-stack-fail", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:              "Primary",
				Subnets:           udnv1.DualStackCIDRs{"192.168.1.0/24", "2001:db8::/64"},
				DefaultGatewayIPs: udnv1.DualStackIPs{"10.0.0.1", "2001:db8::1"},
			},
		}),
	},

	{
		Description: "reservedSubnets must be unset when subnets is unset",
		ExpectedErr: `reservedSubnets must be unset when subnets is unset`,
		Object: newCUDN("reserved-subnets-no-subnets-fail", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:            "Secondary",
				ReservedSubnets: []udnv1.CIDR{"192.168.1.0/28"},
				IPAM:            &udnv1.IPAMConfig{Mode: "Disabled"},
			},
		}),
	},
	{
		Description: "infrastructureSubnets must be unset when subnets is unset",
		ExpectedErr: `infrastructureSubnets must be unset when subnets is unset`,
		Object: newCUDN("infra-subnets-no-subnets-fail", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:                  "Primary",
				InfrastructureSubnets: []udnv1.CIDR{"192.168.1.0/28"},
				IPAM:                  &udnv1.IPAMConfig{Mode: "Disabled"},
			},
		}),
	},
	{
		Description: "infrastructureSubnets and reservedSubnets must not overlap",
		ExpectedErr: `infrastructureSubnets and reservedSubnets must not overlap`,
		Object: newCUDN("infra-reserved-overlap-fail", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:                  "Primary",
				Subnets:               udnv1.DualStackCIDRs{"192.168.1.0/24"},
				ReservedSubnets:       []udnv1.CIDR{"192.168.1.8/29"},
				InfrastructureSubnets: []udnv1.CIDR{"192.168.1.0/28"},
			},
		}),
	},
	{
		Description: "dual-stack infrastructureSubnets and reservedSubnets overlap",
		ExpectedErr: `infrastructureSubnets and reservedSubnets must not overlap`,
		Object: newCUDN("dual-stack-overlap-fail", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:                  "Primary",
				Subnets:               udnv1.DualStackCIDRs{"192.168.1.0/24", "2001:db8::/64"},
				ReservedSubnets:       []udnv1.CIDR{"192.168.1.8/29", "2001:db8::/80"},
				InfrastructureSubnets: []udnv1.CIDR{"192.168.1.0/28", "2001:db8::/80"},
			},
		}),
	},
	{
		Description: "reservedSubnets must be subnetworks of subnets",
		ExpectedErr: `reservedSubnets must be subnetworks of the networks specified in the subnets field`,
		Object: newCUDN("reserved-subnets-outside-fail", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:            "Secondary",
				Subnets:         udnv1.DualStackCIDRs{"192.168.1.0/24"},
				ReservedSubnets: []udnv1.CIDR{"10.0.0.0/28"},
			},
		}),
	},
	{
		Description: "infrastructureSubnets must be subnetworks of subnets",
		ExpectedErr: `infrastructureSubnets must be subnetworks of the networks specified in the subnets field`,
		Object: newCUDN("infra-subnets-outside-fail", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:                  "Primary",
				Subnets:               udnv1.DualStackCIDRs{"192.168.1.0/24"},
				InfrastructureSubnets: []udnv1.CIDR{"10.0.0.0/28"},
			},
		}),
	},
	{
		Description: "IPv6 reservedSubnet outside main subnet",
		ExpectedErr: `reservedSubnets must be subnetworks of the networks specified in the subnets field`,
		Object: newCUDN("ipv6-reserved-subnet-fail", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:            "Secondary",
				Subnets:         udnv1.DualStackCIDRs{"2001:db8::/64"},
				ReservedSubnets: []udnv1.CIDR{"2001:db9::/80"},
			},
		}),
	},
	{
		Description: "IPv6 defaultGatewayIP outside subnet",
		ExpectedErr: `defaultGatewayIPs must belong to one of the subnets specified in the subnets field`,
		Object: newCUDN("ipv6-default-gateway-fail", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:              "Primary",
				Subnets:           udnv1.DualStackCIDRs{"2001:db8::/64"},
				DefaultGatewayIPs: udnv1.DualStackIPs{"2001:db9::1"},
			},
		}),
	},
	{
		Description: "mac-security mode Disabled is not allowed for Primary networks",
		ExpectedErr: `macSecurity.mode Disabled is only supported for Secondary networks`,
		Object: newCUDN("mac-security-disabled-layer2-primary-fail", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:        "Primary",
				IPAM:        &udnv1.IPAMConfig{Mode: "Disabled"},
				MACSecurity: &udnv1.MACSecurityConfig{Mode: "Disabled"},
			},
		}),
	},
	{
		Description: "mac-security disabled requires ipam disabled",
		ExpectedErr: `macSecurity.mode Disabled requires ipam.mode to be Disabled`,
		Object: newCUDN("mac-security-layer2-ipam-enabled-fail", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:        "Secondary",
				Subnets:     udnv1.DualStackCIDRs{"192.168.1.0/24"},
				MACSecurity: &udnv1.MACSecurityConfig{Mode: "Disabled"},
			},
		}),
	},
}

var Layer2CUDNUpdatesRejected = []testscenario.UpdateCRScenario{
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "enable macSecurity after creation is rejected",
			ExpectedErr: "Layer2 is immutable",
			Object: newCUDN("mac-security-layer2-update-immutable-disable-to-enable", udnv1.NetworkSpec{
				Topology: "Layer2",
				Layer2: &udnv1.Layer2Config{
					Role:        "Secondary",
					IPAM:        &udnv1.IPAMConfig{Mode: "Disabled"},
					MACSecurity: &udnv1.MACSecurityConfig{Mode: "Disabled"},
				},
			}),
		},
		InitialObject: newCUDN("mac-security-layer2-update-immutable-disable-to-enable", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2:   &udnv1.Layer2Config{Role: "Secondary", IPAM: &udnv1.IPAMConfig{Mode: "Disabled"}},
		}),
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "disable macSecurity after creation is rejected",
			ExpectedErr: "Layer2 is immutable",
			Object: newCUDN("mac-security-layer2-update-immutable-enable-to-disable", udnv1.NetworkSpec{
				Topology: "Layer2",
				Layer2:   &udnv1.Layer2Config{Role: "Secondary", IPAM: &udnv1.IPAMConfig{Mode: "Disabled"}},
			}),
		},
		InitialObject: newCUDN("mac-security-layer2-update-immutable-enable-to-disable", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:        "Secondary",
				IPAM:        &udnv1.IPAMConfig{Mode: "Disabled"},
				MACSecurity: &udnv1.MACSecurityConfig{Mode: "Disabled"},
			},
		}),
	},
}

var Layer2UDNInvalid = []testscenario.ValidateCRScenario{
	{
		Description: "defaultGatewayIPs is not allowed for Secondary network",
		ExpectedErr: `defaultGatewayIPs is only supported for Primary network`,
		Object: newUDN("default-gateway-secondary-fail", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:              "Secondary",
				Subnets:           udnv1.DualStackCIDRs{"192.168.1.0/24"},
				DefaultGatewayIPs: udnv1.DualStackIPs{"192.168.1.1"},
			},
		}),
	},
	{
		Description: "defaultGatewayIPs must belong to subnets",
		ExpectedErr: `defaultGatewayIPs must belong to one of the subnets specified in the subnets field`,
		Object: newUDN("default-gateway-outside-subnet-fail", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:              "Primary",
				Subnets:           udnv1.DualStackCIDRs{"192.168.1.0/24"},
				DefaultGatewayIPs: udnv1.DualStackIPs{"10.0.0.1"},
			},
		}),
	},
	{
		Description: "defaultGatewayIPs must belong to infrastructureSubnets when specified",
		ExpectedErr: `defaultGatewayIPs have to belong to infrastructureSubnets`,
		Object: newUDN("default-gateway-outside-infra-fail", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:                  "Primary",
				Subnets:               udnv1.DualStackCIDRs{"192.168.1.0/24"},
				InfrastructureSubnets: []udnv1.CIDR{"192.168.1.0/28"},
				DefaultGatewayIPs:     udnv1.DualStackIPs{"192.168.1.20"},
			},
		}),
	},
	{
		Description: "reservedSubnets must be unset when subnets is unset",
		ExpectedErr: `reservedSubnets must be unset when subnets is unset`,
		Object: newUDN("reserved-subnets-no-subnets-fail", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:            "Secondary",
				ReservedSubnets: []udnv1.CIDR{"192.168.1.0/28"},
				IPAM:            &udnv1.IPAMConfig{Mode: "Disabled"},
			},
		}),
	},
	{
		Description: "infrastructureSubnets must be unset when subnets is unset",
		ExpectedErr: `infrastructureSubnets must be unset when subnets is unset`,
		Object: newUDN("infra-subnets-no-subnets-fail", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:                  "Primary",
				InfrastructureSubnets: []udnv1.CIDR{"192.168.1.0/28"},
				IPAM:                  &udnv1.IPAMConfig{Mode: "Disabled"},
			},
		}),
	},
	{
		Description: "mac-security mode Disabled is not supported for Primary networks",
		ExpectedErr: `macSecurity.mode Disabled is only supported for Secondary networks`,
		Object: newUDN("mac-security-disabled-layer2-primary-fail", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:        "Primary",
				IPAM:        &udnv1.IPAMConfig{Mode: "Disabled"},
				MACSecurity: &udnv1.MACSecurityConfig{Mode: "Disabled"},
			},
		}),
	},
	{
		Description: "mac-security disabled requires ipam disabled",
		ExpectedErr: `macSecurity.mode Disabled requires ipam.mode to be Disabled`,
		Object: newUDN("mac-security-layer2-ipam-enabled-fail", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:        "Secondary",
				Subnets:     udnv1.DualStackCIDRs{"192.168.1.0/24"},
				MACSecurity: &udnv1.MACSecurityConfig{Mode: "Disabled"},
			},
		}),
	},
}

var Layer2UDNUpdatesRejected = []testscenario.UpdateCRScenario{
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "enable macSecurity after creation is rejected",
			ExpectedErr: "Layer2 is immutable",
			Object: newUDN("mac-security-layer2-update-immutable-disable-to-enable", udnv1.UserDefinedNetworkSpec{
				Topology: "Layer2",
				Layer2: &udnv1.Layer2Config{
					Role:        "Secondary",
					IPAM:        &udnv1.IPAMConfig{Mode: "Disabled"},
					MACSecurity: &udnv1.MACSecurityConfig{Mode: "Disabled"},
				},
			}),
		},
		InitialObject: newUDN("mac-security-layer2-update-immutable-disable-to-enable", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2:   &udnv1.Layer2Config{Role: "Secondary", IPAM: &udnv1.IPAMConfig{Mode: "Disabled"}},
		}),
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "disable macSecurity after creation is rejected",
			ExpectedErr: "Layer2 is immutable",
			Object: newUDN("mac-security-layer2-update-immutable-enable-to-disable", udnv1.UserDefinedNetworkSpec{
				Topology: "Layer2",
				Layer2:   &udnv1.Layer2Config{Role: "Secondary", IPAM: &udnv1.IPAMConfig{Mode: "Disabled"}},
			}),
		},
		InitialObject: newUDN("mac-security-layer2-update-immutable-enable-to-disable", udnv1.UserDefinedNetworkSpec{
			Topology: "Layer2",
			Layer2: &udnv1.Layer2Config{
				Role:        "Secondary",
				IPAM:        &udnv1.IPAMConfig{Mode: "Disabled"},
				MACSecurity: &udnv1.MACSecurityConfig{Mode: "Disabled"},
			},
		}),
	},
}
