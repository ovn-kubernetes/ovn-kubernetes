// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import (
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var NoOverlayValid = []testscenario.ValidateCRScenario{
	{
		Description: "NoOverlay transport with managed routing and enabled SNAT",
		Object: newCUDN("no-overlay-managed-enabled-snat", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				MTU:     1500,
				Subnets: []udnv1.Layer3Subnet{{CIDR: "10.10.0.0/16", HostSubnet: 24}},
			},
			Transport: "NoOverlay",
			NoOverlay: &udnv1.NoOverlayConfig{OutboundSNAT: "Enabled", Routing: "Managed"},
		}),
	},
	{
		Description: "NoOverlay transport with unmanaged routing and disabled SNAT",
		Object: newCUDNForNamespaces("no-overlay-unmanaged-disabled-snat", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				MTU:     1500,
				Subnets: []udnv1.Layer3Subnet{{CIDR: "10.20.0.0/16", HostSubnet: 24}},
			},
			Transport: "NoOverlay",
			NoOverlay: &udnv1.NoOverlayConfig{OutboundSNAT: "Disabled", Routing: "Unmanaged"},
		}, "blue"),
	},
	{
		Description: "NoOverlay transport with managed routing and disabled SNAT",
		Object: newCUDNForNamespaces("no-overlay-managed-disabled-snat", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				MTU:     1500,
				Subnets: []udnv1.Layer3Subnet{{CIDR: "10.30.0.0/16", HostSubnet: 24}},
			},
			Transport: "NoOverlay",
			NoOverlay: &udnv1.NoOverlayConfig{OutboundSNAT: "Disabled", Routing: "Managed"},
		}, "green"),
	},
	{
		Description: "NoOverlay transport with unmanaged routing and enabled SNAT",
		Object: newCUDNForNamespaces("no-overlay-unmanaged-enabled-snat", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				MTU:     1500,
				Subnets: []udnv1.Layer3Subnet{{CIDR: "10.40.0.0/16", HostSubnet: 24}},
			},
			Transport: "NoOverlay",
			NoOverlay: &udnv1.NoOverlayConfig{OutboundSNAT: "Enabled", Routing: "Unmanaged"},
		}, "yellow"),
	},
	{
		Description: "NoOverlay transport with dual-stack subnets",
		Object: newCUDNForNamespaces("no-overlay-dual-stack", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				MTU:     1500,
				Subnets: []udnv1.Layer3Subnet{{CIDR: "10.50.0.0/16", HostSubnet: 24}, {CIDR: "fd00:10:50::/48", HostSubnet: 64}},
			},
			Transport: "NoOverlay",
			NoOverlay: &udnv1.NoOverlayConfig{OutboundSNAT: "Enabled", Routing: "Managed"},
		}, "purple"),
	},
	{
		Description: "Layer3 primary network with unset default transport (no transport field set)",
		Object: newCUDNForNamespaces("unset-default-transport", udnv1.NetworkSpec{
			Topology: "Layer3",
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				MTU:     1400,
				Subnets: []udnv1.Layer3Subnet{{CIDR: "10.60.0.0/16", HostSubnet: 24}},
			},
		}, "orange"),
	},
}
