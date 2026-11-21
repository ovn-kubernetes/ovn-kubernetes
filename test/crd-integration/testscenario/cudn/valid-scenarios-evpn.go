// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import (
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var EVPNCUDNValid = []testscenario.ValidateCRScenario{
	{
		Description: "valid Layer2 Primary EVPN with macVRF",
		Object: newCUDN("l2-evpn-primary", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100}},
		}),
	},
	{
		Description: "valid Layer2 Primary EVPN with macVRF and routeTarget",
		Object: newCUDN("l2-evpn-with-rt", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "65000:100"}},
		}),
	},
	{
		Description: "valid Layer2 Primary EVPN with both macVRF and ipVRF",
		Object: newCUDN("l2-evpn-mac-ip-vrf", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100}, IPVRF: &udnv1.VRFConfig{VNI: 101}},
		}),
	},
	{
		Description: "valid Layer3 Primary EVPN with ipVRF",
		Object: newCUDN("l3-evpn-primary", udnv1.NetworkSpec{
			Topology:  "Layer3",
			Layer3:    &udnv1.Layer3Config{Role: "Primary", Subnets: []udnv1.Layer3Subnet{{CIDR: "10.20.100.0/16", HostSubnet: 24}}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", IPVRF: &udnv1.VRFConfig{VNI: 200}},
		}),
	},
	{
		Description: "valid Layer3 Primary EVPN with ipVRF and routeTarget",
		Object: newCUDN("l3-evpn-with-rt", udnv1.NetworkSpec{
			Topology:  "Layer3",
			Layer3:    &udnv1.Layer3Config{Role: "Primary", Subnets: []udnv1.Layer3Subnet{{CIDR: "10.20.100.0/16", HostSubnet: 24}}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", IPVRF: &udnv1.VRFConfig{VNI: 200, RouteTarget: "65000:200"}},
		}),
	},
	{
		Description: "valid Layer2 Primary EVPN dual-stack",
		Object: newCUDN("l2-evpn-dual-stack", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24", "2001:db8::/64"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 300}},
		}),
	},
	{
		Description: "valid EVPN with max VNI value",
		Object: newCUDN("evpn-max-vni", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 16777215}},
		}),
	},
	{
		Description: "valid EVPN with 4-byte ASN in routeTarget (2-byte local admin)",
		Object: newCUDN("evpn-4byte-asn", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "4200000000:100"}},
		}),
	},
	{
		Description: "valid EVPN with 2-byte ASN and 4-byte local admin",
		Object: newCUDN("evpn-2byte-asn-4byte-local", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "65000:16777215"}},
		}),
	},
	{
		Description: "valid EVPN with IPv4 address format routeTarget",
		Object: newCUDN("evpn-ipv4-rt", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "192.168.1.1:100"}},
		}),
	},
	{
		Description: "valid EVPN with wildcard AS routeTarget",
		Object: newCUDN("evpn-wildcard-rt", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "*:100"}},
		}),
	},
	{
		Description: "valid EVPN with wildcard AS and large local admin",
		Object: newCUDN("evpn-wildcard-large-local", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "*:16777215"}},
		}),
	},
	{
		Description: "valid EVPN with AS=0 (FRR allows)",
		Object: newCUDN("evpn-as-zero", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "0:100"}},
		}),
	},
	{
		Description: "valid EVPN with local admin=0 (FRR allows)",
		Object: newCUDN("evpn-local-zero", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "65000:0"}},
		}),
	},
	{
		Description: "valid EVPN with wildcard and local admin=0 (FRR allows)",
		Object: newCUDN("evpn-wildcard-zero", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "*:0"}},
		}),
	},
	{
		Description: "valid EVPN with max 2-byte AS boundary (65535)",
		Object: newCUDN("evpn-2byte-as-max", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "65535:100"}},
		}),
	},
	{
		Description: "valid EVPN with min 4-byte AS boundary (65536)",
		Object: newCUDN("evpn-4byte-as-min", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "65536:100"}},
		}),
	},
	{
		Description: "valid EVPN with max local admin for 2-byte AS (4294967295)",
		Object: newCUDN("evpn-2byte-as-max-local", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "65000:4294967295"}},
		}),
	},
	{
		Description: "valid EVPN with max length routeTarget (21 chars: 255.255.255.255:65535)",
		Object: newCUDN("evpn-max-length-rt", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.20.100.0/24"}},
			Transport: "EVPN",
			EVPN:      &udnv1.EVPNConfig{VTEP: "evpn-vtep", MACVRF: &udnv1.VRFConfig{VNI: 100, RouteTarget: "255.255.255.255:65535"}},
		}),
	},
}
