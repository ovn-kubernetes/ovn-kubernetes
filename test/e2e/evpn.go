package e2e

import (
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/api"
)

// EVPNVRFConfig holds configuration for an EVPN VRF (MAC-VRF or IP-VRF) on the external FRR.
// This struct is used for both Layer 2 (MAC-VRF) and Layer 3 (IP-VRF) scenarios.
type EVPNVRFConfig struct {
	// Name is the VRF/network name (matches CUDN network name)
	Name string
	// VID is the VLAN ID used locally on the bridge
	VID int
	// VNI is the VXLAN Network Identifier (from CUDN spec)
	VNI int32
	// RouteTarget is the optional route target for IP-VRF, e.g., "64512:100" (for future use, not used today)
	RouteTarget string
}

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

// configureExternalFRRMACVRF configures a MAC-VRF on the external FRR container.
// This sets up the VID/VNI mapping on the bridge/VTEP for Layer 2 EVPN.
func configureExternalFRRMACVRF(frr infraapi.ExternalContainer, cfg EVPNVRFConfig) error {
	provider := infraprovider.Get()
	vid := fmt.Sprintf("%d", cfg.VID)
	vni := fmt.Sprintf("%d", cfg.VNI)

	// Add VLAN to bridge
	_, err := provider.ExecExternalContainerCommand(frr, []string{
		"bridge", "vlan", "add", "dev", "br0", "vid", vid, "self",
	})
	if err != nil {
		return fmt.Errorf("failed to add vid %s to br0: %w", vid, err)
	}

	// Add VLAN to VXLAN device
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"bridge", "vlan", "add", "dev", "vxlan0", "vid", vid,
	})
	if err != nil {
		return fmt.Errorf("failed to add vid %s to vxlan0: %w", vid, err)
	}

	// Add VNI mapping
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"bridge", "vni", "add", "dev", "vxlan0", "vni", vni,
	})
	if err != nil {
		return fmt.Errorf("failed to add vni %s to vxlan0: %w", vni, err)
	}

	// Map VID to VNI for tunnel
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"bridge", "vlan", "add", "dev", "vxlan0", "vid", vid, "tunnel_info", "id", vni,
	})
	if err != nil {
		return fmt.Errorf("failed to map vid %s to vni %s: %w", vid, vni, err)
	}

	return nil
}

// attachInterfaceToEVPNBridge attaches an interface inside the FRR container
// to the EVPN bridge (br0) as an access port with the specified VLAN.
// The interface becomes an untagged access port for the MAC-VRF.
func attachInterfaceToEVPNBridge(frr infraapi.ExternalContainer, ifaceName string, vid int) error {
	provider := infraprovider.Get()
	vidStr := fmt.Sprintf("%d", vid)

	// Attach interface to bridge
	_, err := provider.ExecExternalContainerCommand(frr, []string{
		"ip", "link", "set", ifaceName, "master", "br0",
	})
	if err != nil {
		return fmt.Errorf("failed to attach %s to br0: %w", ifaceName, err)
	}

	// Configure as access port: pvid (default ingress VLAN) and untagged (egress untagged)
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"bridge", "vlan", "add", "dev", ifaceName, "vid", vidStr, "pvid", "untagged",
	})
	if err != nil {
		return fmt.Errorf("failed to configure %s as access port for vid %s: %w", ifaceName, vidStr, err)
	}

	return nil
}

// configureExternalFRRIPVRF configures an IP-VRF on the external FRR container.
// This sets up a Linux VRF, SVI, and VID/VNI mapping for Layer 3 EVPN routing.
// The gatewayIP is the IP address for the SVI (e.g., "10.100.0.1/16").
func configureExternalFRRIPVRF(frr infraapi.ExternalContainer, cfg EVPNVRFConfig, gatewayIP string) error {
	provider := infraprovider.Get()
	vidStr := fmt.Sprintf("%d", cfg.VID)
	vniStr := fmt.Sprintf("%d", cfg.VNI)
	tableID := fmt.Sprintf("%d", cfg.VNI) // Use VNI as routing table ID
	sviName := fmt.Sprintf("vlan%d", cfg.VID)

	// Create Linux VRF
	_, err := provider.ExecExternalContainerCommand(frr, []string{
		"ip", "link", "add", cfg.Name, "type", "vrf", "table", tableID,
	})
	if err != nil {
		return fmt.Errorf("failed to create VRF %s: %w", cfg.Name, err)
	}

	// Bring VRF up
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"ip", "link", "set", cfg.Name, "up",
	})
	if err != nil {
		return fmt.Errorf("failed to bring up VRF %s: %w", cfg.Name, err)
	}

	// Add VLAN to bridge (for IP-VRF we need an SVI)
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"bridge", "vlan", "add", "dev", "br0", "vid", vidStr, "self",
	})
	if err != nil {
		return fmt.Errorf("failed to add vid %s to br0: %w", vidStr, err)
	}

	// Add VLAN to VXLAN device
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"bridge", "vlan", "add", "dev", "vxlan0", "vid", vidStr,
	})
	if err != nil {
		return fmt.Errorf("failed to add vid %s to vxlan0: %w", vidStr, err)
	}

	// Add VNI mapping
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"bridge", "vni", "add", "dev", "vxlan0", "vni", vniStr,
	})
	if err != nil {
		return fmt.Errorf("failed to add vni %s to vxlan0: %w", vniStr, err)
	}

	// Map VID to VNI for tunnel
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"bridge", "vlan", "add", "dev", "vxlan0", "vid", vidStr, "tunnel_info", "id", vniStr,
	})
	if err != nil {
		return fmt.Errorf("failed to map vid %s to vni %s: %w", vidStr, vniStr, err)
	}

	// Create SVI (VLAN interface on bridge)
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"ip", "link", "add", "link", "br0", "name", sviName, "type", "vlan", "id", vidStr,
	})
	if err != nil {
		return fmt.Errorf("failed to create SVI %s: %w", sviName, err)
	}

	// Attach SVI to VRF
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"ip", "link", "set", sviName, "master", cfg.Name,
	})
	if err != nil {
		return fmt.Errorf("failed to attach SVI %s to VRF %s: %w", sviName, cfg.Name, err)
	}

	// Bring SVI up
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"ip", "link", "set", sviName, "up",
	})
	if err != nil {
		return fmt.Errorf("failed to bring up SVI %s: %w", sviName, err)
	}

	// Configure gateway IP on SVI if provided
	if gatewayIP != "" {
		_, err = provider.ExecExternalContainerCommand(frr, []string{
			"ip", "addr", "add", gatewayIP, "dev", sviName,
		})
		if err != nil {
			return fmt.Errorf("failed to add gateway IP %s to SVI %s: %w", gatewayIP, sviName, err)
		}
	}

	return nil
}

// addIPVRFBGPConfig adds BGP configuration for an IP-VRF via vtysh.
// This configures the VRF with VNI mapping and enables EVPN route advertisement.
func addIPVRFBGPConfig(frr infraapi.ExternalContainer, cfg EVPNVRFConfig) error {
	provider := infraprovider.Get()
	vniStr := fmt.Sprintf("%d", cfg.VNI)

	// Configure VRF with VNI mapping
	_, err := provider.ExecExternalContainerCommand(frr, []string{
		"vtysh",
		"-c", "configure terminal",
		"-c", fmt.Sprintf("vrf %s", cfg.Name),
		"-c", fmt.Sprintf("vni %s", vniStr),
		"-c", "exit-vrf",
		"-c", fmt.Sprintf("router bgp 64512 vrf %s", cfg.Name),
		"-c", "address-family ipv4 unicast",
		"-c", "redistribute connected",
		"-c", "exit-address-family",
		"-c", "address-family l2vpn evpn",
		"-c", "advertise ipv4 unicast",
		"-c", "exit-address-family",
		"-c", "end",
	})
	if err != nil {
		return fmt.Errorf("failed to configure IP-VRF BGP for %s: %w", cfg.Name, err)
	}

	return nil
}
