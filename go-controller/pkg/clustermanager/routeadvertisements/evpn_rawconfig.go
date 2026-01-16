package routeadvertisements

import (
	"fmt"
	"strings"
)

// generateEVPNRawConfig generates raw FRR configuration for EVPN.
// If asn/neighbors aren't provided the related sections are skipped.
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
//	  exit-vni
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
func generateEVPNRawConfig(selected *selectedNetworks, asn uint32, neighbors []string, vrfASNs map[string]uint32) string {
	var buf strings.Builder

	if asn > 0 && len(neighbors) > 0 {
		buf.WriteString(genGlobalEVPNSection(asn, neighbors, selected.macVRFConfigs))
	}
	for _, cfg := range selected.ipVRFConfigs {
		buf.WriteString(genVRFVNISection(cfg))
	}
	// Generate VRF-specific EVPN sections using each config's ASN
	for _, cfg := range selected.ipVRFConfigs {
		if vrfASN := vrfASNs[cfg.VRFName]; vrfASN > 0 {
			buf.WriteString(genVRFEVPNSection(vrfASN, cfg))
		}
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
//	  exit-vni
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
		buf.WriteString("  exit-vni\n")
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
