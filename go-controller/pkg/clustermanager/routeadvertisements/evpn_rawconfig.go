package routeadvertisements

// TODO: Delete this file when frr-k8s adds typed API support for EVPN.

import (
	"fmt"
	"strings"
)

// generateEVPNRawConfig generates raw FRR configuration for EVPN.
//
// Generated config structure:
//
//	router bgp <asn>                    <- genGlobalEVPNSection
//	 address-family l2vpn evpn
//	  neighbor <ip> activate
//	  advertise-all-vni
//	  vni <id>                          <- (one per MAC-VRF with RT, section only added when MAC-VRF RT is set)
//	   route-target import <rt>
//	   route-target export <rt>
//	 exit-address-family
//	exit
//	!
//	vrf <name>                          <- genVRFVNISection (one per IP-VRF)
//	 vni <id>
//	exit-vrf
//	!
//	router bgp <asn> vrf <name>         <- genVRFEVPNSection (one per IP-VRF)
//	 address-family l2vpn evpn
//	  advertise ipv4 unicast
//	  advertise ipv6 unicast
//	  route-target import <rt>
//	  route-target export <rt>
//	 exit-address-family
//	exit
//	!
func generateEVPNRawConfig(selected *selectedNetworks, asn uint32, neighbors []string) string {
	var buf strings.Builder

	buf.WriteString(genGlobalEVPNSection(asn, neighbors, selected.macVRFConfigs))
	for _, cfg := range selected.ipVRFConfigs {
		buf.WriteString(genVRFVNISection(cfg))
	}
	for _, cfg := range selected.ipVRFConfigs {
		buf.WriteString(genVRFEVPNSection(asn, cfg))
	}

	return buf.String()
}

// genVRFVNISection generates VRF-to-VNI mapping.
//
//	vrf <name>
//	 vni <id>
//	exit-vrf
//	!
func genVRFVNISection(cfg *ipVRFConfig) string {
	return fmt.Sprintf(`vrf %s
 vni %d
exit-vrf
!
`, cfg.VRFName, cfg.VNI)
}

// genGlobalEVPNSection generates the global router's EVPN address-family.
//
//	router bgp <asn>
//	 address-family l2vpn evpn
//	  neighbor <ip> activate
//	  advertise-all-vni
//	  vni <id>                          <- (Section only added when MAC-VRF RT is set)
//	   route-target import <rt>
//	   route-target export <rt>
//	 exit-address-family
//	exit
//	!
func genGlobalEVPNSection(asn uint32, neighbors []string, macVRFs []*vrfConfig) string {
	var buf strings.Builder

	fmt.Fprintf(&buf, "router bgp %d\n", asn)
	buf.WriteString(" address-family l2vpn evpn\n")

	for _, neighbor := range neighbors {
		fmt.Fprintf(&buf, "  neighbor %s activate\n", neighbor)
	}
	buf.WriteString("  advertise-all-vni\n")

	for _, cfg := range macVRFs {
		if cfg.RouteTarget == "" {
			continue
		}
		fmt.Fprintf(&buf, "  vni %d\n", cfg.VNI)
		fmt.Fprintf(&buf, "   route-target import %s\n", cfg.RouteTarget)
		fmt.Fprintf(&buf, "   route-target export %s\n", cfg.RouteTarget)
	}

	buf.WriteString(" exit-address-family\n")
	buf.WriteString("exit\n!\n")

	return buf.String()
}

// genVRFEVPNSection generates a VRF router's EVPN address-family.
//
//	router bgp 65000 vrf red
//	 address-family l2vpn evpn
//	  advertise ipv4 unicast
//	  advertise ipv6 unicast
//	  route-target import 65000:100
//	  route-target export 65000:100
//	 exit-address-family
//	exit
//	!
func genVRFEVPNSection(asn uint32, cfg *ipVRFConfig) string {
	var buf strings.Builder
	fmt.Fprintf(&buf, "router bgp %d vrf %s\n", asn, cfg.VRFName)
	buf.WriteString(" address-family l2vpn evpn\n")

	if cfg.HasIPv4 {
		buf.WriteString("  advertise ipv4 unicast\n")
	}
	if cfg.HasIPv6 {
		buf.WriteString("  advertise ipv6 unicast\n")
	}
	if cfg.RouteTarget != "" {
		fmt.Fprintf(&buf, "  route-target import %s\n", cfg.RouteTarget)
		fmt.Fprintf(&buf, "  route-target export %s\n", cfg.RouteTarget)
	}

	buf.WriteString(" exit-address-family\n")
	buf.WriteString("exit\n!\n")

	return buf.String()
}
