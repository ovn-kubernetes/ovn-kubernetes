package e2e

import (
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/api"
)

// setupExternalFRRWithEVPNBridge configures the EVPN bridge and VXLAN VTEP on
// the external FRR container. This creates the SVD (Single VXLAN Device) architecture:
//   - br0: Linux bridge with VLAN filtering enabled
//   - vxlan0: VXLAN device with external VNI filtering, attached to br0
//
// The localIP is the FRR container's IP used as the local VTEP endpoint.
func setupExternalFRRWithEVPNBridge(frr infraapi.ExternalContainer, localIP string) error {
	provider := infraprovider.Get()

	// Create bridge with VLAN filtering, no default PVID
	_, err := provider.ExecExternalContainerCommand(frr, []string{
		"ip", "link", "add", "br0", "type", "bridge",
		"vlan_filtering", "1", "vlan_default_pvid", "0",
	})
	if err != nil {
		return fmt.Errorf("failed to create bridge br0: %w", err)
	}

	// Disable IPv6 link-local address generation on bridge
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"ip", "link", "set", "br0", "addrgenmode", "none",
	})
	if err != nil {
		return fmt.Errorf("failed to set br0 addrgenmode: %w", err)
	}

	// Create VXLAN device with external VNI filtering
	// dstport 4789 is standard VXLAN port
	// nolearning: disable MAC learning (FRR handles via BGP)
	// external: use external control plane (FRR)
	// vnifilter: enable VNI filtering for multi-VNI support
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"ip", "link", "add", "vxlan0", "type", "vxlan",
		"dstport", "4789",
		"local", localIP,
		"nolearning", "external", "vnifilter",
	})
	if err != nil {
		return fmt.Errorf("failed to create vxlan0: %w", err)
	}

	// Disable IPv6 link-local address generation on VXLAN device
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"ip", "link", "set", "vxlan0", "addrgenmode", "none",
	})
	if err != nil {
		return fmt.Errorf("failed to set vxlan0 addrgenmode: %w", err)
	}

	// Attach VXLAN device to bridge
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"ip", "link", "set", "vxlan0", "master", "br0",
	})
	if err != nil {
		return fmt.Errorf("failed to attach vxlan0 to br0: %w", err)
	}

	// Bring up bridge and VXLAN device
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"ip", "link", "set", "br0", "up",
	})
	if err != nil {
		return fmt.Errorf("failed to bring up br0: %w", err)
	}

	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"ip", "link", "set", "vxlan0", "up",
	})
	if err != nil {
		return fmt.Errorf("failed to bring up vxlan0: %w", err)
	}

	// Configure VXLAN device for VNI <-> VID mapping support
	// vlan_tunnel: enable VLAN-to-VNI mapping
	// neigh_suppress: suppress ARP/ND flooding (FRR handles via BGP)
	// learning off: disable MAC learning
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"bridge", "link", "set", "dev", "vxlan0",
		"vlan_tunnel", "on", "neigh_suppress", "on", "learning", "off",
	})
	if err != nil {
		return fmt.Errorf("failed to configure vxlan0 bridge settings: %w", err)
	}

	return nil
}
