// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import (
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var EVPNCUDNInvalid = []testscenario.ValidateCRScenario{
	{
		Description: "EVPN transport requires evpn configuration field",
		ExpectedErr: `spec.evpn field is required when transport is 'EVPN'`,
		Object: newCUDN("evpn-no-config", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
		}),
	},
	{
		Description: "evpn configuration field is forbidden when transport is not 'EVPN'",
		ExpectedErr: `spec.evpn field is forbidden when transport is not 'EVPN'`,
		Object: newCUDN("evpn-config-no-transport", udnv1.NetworkSpec{
			Topology: "Layer2",
			Layer2:   &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			EVPN:     &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100}},
		}),
	},
	{
		Description: "EVPN is not supported for Secondary networks",
		ExpectedErr: `transport 'EVPN' is only supported for Layer2 or Layer3 primary networks`,
		Object: newCUDN("evpn-secondary", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Secondary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100}},
		}),
	},
	{
		Description: "EVPN is not supported for Localnet topology",
		ExpectedErr: `transport 'EVPN' is only supported for Layer2 or Layer3 primary networks`,
		Object: newCUDN("evpn-localnet", udnv1.NetworkSpec{
			Topology:  "Localnet",
			Localnet:  &udnv1.LocalnetConfig{Role: "Secondary", PhysicalNetworkName: "physnet1"},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100}},
		}),
	},
	{
		Description: "Layer2 EVPN requires macVRF",
		ExpectedErr: `spec.evpn.macVRF field is required for Layer2 topology when transport is 'EVPN'`,
		Object: newCUDN("l2-evpn-no-macvrf", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", IPVRF: &udnv1.VRFConfig{VNI: 100}},
		}),
	},
	{
		Description: "Layer3 EVPN requires ipVRF",
		ExpectedErr: `spec.evpn.ipVRF field is required for Layer3 topology when transport is 'EVPN'`,
		Object: newCUDN("l3-evpn-no-ipvrf", udnv1.NetworkSpec{
			Topology:  "Layer3",
			Layer3:    &udnv1.Layer3Config{Role: "Primary", Subnets: []udnv1.Layer3Subnet{{CIDR: "10.20.100.0/16", HostSubnet: 24}}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100}},
		}),
	},
	{
		Description: "Layer3 EVPN forbids macVRF",
		ExpectedErr: `spec.evpn.macVRF field is forbidden for Layer3 topology when transport is 'EVPN'`,
		Object: newCUDN("l3-evpn-with-macvrf", udnv1.NetworkSpec{
			Topology:  "Layer3",
			Layer3:    &udnv1.Layer3Config{Role: "Primary", Subnets: []udnv1.Layer3Subnet{{CIDR: "10.20.100.0/16", HostSubnet: 24}}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100}, IPVRF: &udnv1.VRFConfig{VNI: 101}},
		}),
	},
	{
		Description: "evpn requires at least macVRF or ipVRF",
		ExpectedErr: `at least one of macVRF or ipVRF must be specified`,
		Object: newCUDN("evpn-no-vrf", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep"},
		}),
	},
	{
		Description: "VNI must be at least 1",
		ExpectedErr: `spec.network.evpn.macVRF.vni: Invalid value`,
		Object: newCUDN("evpn-vni-zero", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{}},
		}),
	},
	{
		Description: "VNI must not exceed 16777215",
		ExpectedErr: `spec.network.evpn.macVRF.vni: Invalid value`,
		Object: newCUDN("evpn-vni-too-large", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 16777216}},
		}),
	},
	{
		Description: "routeTarget must be in valid format",
		ExpectedErr: `RT must contain exactly one colon`,
		Object: newCUDN("evpn-invalid-rt-format", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "invalid"}},
		}),
	},
	{
		Description: "routeTarget 4-byte AS requires 2-byte local admin (6-byte constraint)",
		ExpectedErr: `RT with 4-byte ASN global administrator must have format GHJK:MN where GHJK <= 4294967295 and MN <= 65535`,
		Object: newCUDN("evpn-invalid-rt-4byte-as", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "4200000000:70000"}},
		}),
	},
	{
		Description: "routeTarget IPv4 format requires valid IPv4 address",
		ExpectedErr: `RT global administrator must be either '*', an IPv4 address, or a number`,
		Object: newCUDN("evpn-invalid-rt-ipv4", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "999.999.999.999:100"}},
		}),
	},
	{
		Description: "routeTarget IPv4 format requires 2-byte local admin",
		ExpectedErr: `RT with IPv4 global administrator must have format A.B.C.D:MN where MN <= 65535`,
		Object: newCUDN("evpn-invalid-rt-ipv4-local", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "192.168.1.1:70000"}},
		}),
	},
	{
		Description: "routeTarget format must have exactly one colon",
		ExpectedErr: `RT must contain exactly one colon`,
		Object: newCUDN("evpn-invalid-rt-multiple-colons", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "65000:100:200"}},
		}),
	},
	{
		Description: "routeTarget format must include colon separator",
		ExpectedErr: `RT must contain exactly one colon`,
		Object: newCUDN("evpn-invalid-rt-no-colon", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "65000"}},
		}),
	},
	{
		Description: "VTEP name is required in evpn",
		ExpectedErr: `spec.network.evpn.vtep`,
		Object: testscenario.WithoutFields(newCUDN("evpn-no-vtep", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{MACVRF: &udnv1.VRFConfig{VNI: 100}},
		}), "spec.network.evpn.vtep"),
	},
	{
		Description: "VTEP name cannot be empty",
		ExpectedErr: `spec.network.evpn.vtep`,
		Object: newCUDN("evpn-empty-vtep", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{MACVRF: &udnv1.VRFConfig{VNI: 100}},
		}),
	},
	{
		Description: "routeTarget local administrator must be a number",
		ExpectedErr: `RT local administrator must be a number`,
		Object: newCUDN("evpn-invalid-rt-local-nan", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "65000:abc"}},
		}),
	},
	{
		Description: "routeTarget wildcard format local admin must not exceed 4294967295",
		ExpectedErr: `RT with wildcard global administrator must have format *:OPQR where OPQR <= 4294967295`,
		Object: newCUDN("evpn-invalid-rt-wildcard-overflow", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "*:5000000000"}},
		}),
	},
	{
		Description: "macVRF and ipVRF must use different VNIs",
		ExpectedErr: `macVRF and ipVRF must use different VNIs`,
		Object: newCUDN("evpn-same-vni", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100}, IPVRF: &udnv1.VRFConfig{VNI: 100}},
		}),
	},
	{
		Description: "routeTarget exceeds maximum length of 21 characters",
		ExpectedErr: `Too long`,
		Object: newCUDN("evpn-invalid-rt-too-long", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "255.255.255.255:655350"}},
		}),
	},
}
