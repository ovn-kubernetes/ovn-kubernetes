// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import (
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var NoOverlayInvalid = []testscenario.ValidateCRScenario{
	{
		Description: "NoOverlay transport is only supported for Layer3 primary networks - Layer2 network",
		ExpectedErr: `transport 'NoOverlay' is only supported for Layer3 primary networks`,
		Object: newCUDN("no-overlay-layer2-fail", udnv1.NetworkSpec{
			Topology:  "Layer2",
			Layer2:    &udnv1.Layer2Config{Role: "Primary", Subnets: udnv1.DualStackCIDRs{"10.10.0.0/16"}},
			Transport: "NoOverlay",
			NoOverlay: &udnv1.NoOverlayConfig{OutboundSNAT: "Enabled", Routing: "Managed"},
		}),
	},
	{
		Description: "NoOverlay transport is only supported for Layer3 primary networks - Layer3 secondary network",
		ExpectedErr: `transport 'NoOverlay' is only supported for Layer3 primary networks`,
		Object: newCUDN("no-overlay-layer3-secondary-fail", udnv1.NetworkSpec{
			Topology:  "Layer3",
			Layer3:    &udnv1.Layer3Config{Role: "Secondary", Subnets: []udnv1.Layer3Subnet{{CIDR: "10.10.0.0/16", HostSubnet: 24}}},
			Transport: "NoOverlay",
			NoOverlay: &udnv1.NoOverlayConfig{OutboundSNAT: "Enabled", Routing: "Managed"},
		}),
	},
	{
		Description: "NoOverlay transport is only supported for Layer3 primary networks - Localnet network",
		ExpectedErr: `transport 'NoOverlay' is only supported for Layer3 primary networks`,
		Object: newCUDN("no-overlay-localnet-fail", udnv1.NetworkSpec{
			Topology: "Localnet",
			Localnet: &udnv1.LocalnetConfig{
				Role:                "Secondary",
				PhysicalNetworkName: "physnet1",
				Subnets:             udnv1.DualStackCIDRs{"10.10.0.0/16"},
			},
			Transport: "NoOverlay",
			NoOverlay: &udnv1.NoOverlayConfig{OutboundSNAT: "Enabled", Routing: "Managed"},
		}),
	},
	{
		Description: "noOverlay is required when transport is NoOverlay",
		ExpectedErr: `spec.noOverlay is required when type transport is 'NoOverlay'`,
		Object: newCUDN("no-overlay-missing-options-fail", udnv1.NetworkSpec{
			Topology:  "Layer3",
			Layer3:    &udnv1.Layer3Config{Role: "Primary", Subnets: []udnv1.Layer3Subnet{{CIDR: "10.10.0.0/16", HostSubnet: 24}}},
			Transport: "NoOverlay",
		}),
	},
	{
		Description: "noOverlay is forbidden when transport is not set",
		ExpectedErr: `spec.noOverlay is forbidden when transport type is not 'NoOverlay'`,
		Object: newCUDN("no-overlay-options-without-transport-fail", udnv1.NetworkSpec{
			Topology:  "Layer3",
			Layer3:    &udnv1.Layer3Config{Role: "Primary", Subnets: []udnv1.Layer3Subnet{{CIDR: "10.10.0.0/16", HostSubnet: 24}}},
			NoOverlay: &udnv1.NoOverlayConfig{OutboundSNAT: "Enabled", Routing: "Managed"},
		}),
	},
}
