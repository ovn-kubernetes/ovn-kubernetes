package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"

	vtepv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/api"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	utilnet "k8s.io/utils/net"
)

// =============================================================================
// EVPN E2E Test Infrastructure
// =============================================================================
//
// This file contains utilities and tests for EVPN (Ethernet VPN) functionality
// in OVN-Kubernetes. EVPN enables Layer 2 and Layer 3 connectivity between
// pods in a Kubernetes cluster and external networks using BGP as the control
// plane and VXLAN as the data plane.
//
// =============================================================================
// Test Infrastructure Overview
// =============================================================================
//
// EVPN Test Infrastructure:
//
// +---------------------------------------+                      +------------------+
// | KIND Cluster                          |    KIND Primary      | External FRR     |
// |                                       |    Network           | (reused from BGP)|
// | +-------------+ +-------------+       |    (BGP/EVPN)        |                  |
// | | Node 1      | | Node 2      |       |    172.18.0.0/16     | eth0 (primary)   |
// | | - FRR-K8s   | | - FRR-K8s   |       | <----------------->  | br0 (EVPN)       |
// | | - OVN VTEP  | | - OVN VTEP  |       |                      | vxlan0 (VTEP)    |
// | +-------------+ +-------------+       |                      +------------------+
// |                                       |                             |
// | +-------------+ +----------------+    |         +------------------+--------------------+
// | | Node 3      | | Pod (on CUDN)  |    |         |   CUDN-network   | agnhost-ipvrf-net  |
// | | - FRR-K8s   | | 10.100.0.0/16  |    |         |   10.100.0.0/16  | 172.27.102.0/24    |
// | | - OVN VTEP  | +----------------+    |         +--------|---------+---------|---------+
// | +-------------+                       |                  |                   |
// +---------------------------------------+                  v                   v
//                                                   +------------------+--------------------+
//                                                   | agnhost-macvrf   | agnhost-ipvrf      |
//                                                   | 10.100.0.250     | 172.27.102.2       |
//                                                   | VNI: 10100       | VNI: 20102         |
//                                                   +------------------+--------------------+
//
// =============================================================================
// KIND Setup Prerequisites
// =============================================================================
//
// EVPN tests depend on the RouteAdvertisements/BGP infrastructure being set up
// during KIND cluster installation. This is enabled via:
//
//   $ ./contrib/kind.sh -rae
//
// This creates:
//   1. FRR container ("frr") connected to KIND's primary network
//   2. FRR-K8s operator deployed in the cluster
//   3. BGP peering between cluster nodes and external FRR
//
// EVPN tests REUSE the existing FRR container rather than creating a new one.
// We extend its configuration to support EVPN by adding:
//   - br0: Linux bridge with vlan_filtering for EVPN
//   - vxlan0: VXLAN device (VTEP) with vnifilter for SVD mode
//   - VRF and SVI configuration for IP-VRF tests
//   - Access ports for MAC-VRF tests
//   - BGP EVPN address-family configuration
//
// This approach:
//   - Avoids duplicate infrastructure setup
//   - Ensures consistent BGP AS numbers and peering
//   - Leverages existing FRR-K8s CRDs for cluster-side config
//
// =============================================================================
// Component Details
// =============================================================================
//
// KIND Cluster (172.18.0.0/16):
//   - 3 nodes running OVN-Kubernetes with EVPN support
//   - FRR-K8s daemonset on each node for BGP/EVPN peering
//   - OVN VTEP (br0 + vxlan0) configured on each node via VTEP CR
//
// External FRR Container:
//   - Reuses the existing FRR container from BGP/RouteAdvertisements tests
//   - Acts as the EVPN peer for the cluster nodes
//   - Components:
//     * eth0: Connected to KIND primary network (172.18.0.0/16) for BGP peering
//     * br0: Linux bridge for EVPN with vlan_filtering enabled
//     * vxlan0: VXLAN device (VTEP) with vnifilter for SVD (Single VXLAN Device) mode
//
// External Agnhost Containers (test endpoints):
//   - agnhost-macvrf: For L2 MAC-VRF connectivity tests
//     * Connected to br0 via evpn10100 access port (PVID 100)
//     * IP: 10.100.0.250/16 (same subnet as CUDN)
//     * Runs agnhost netexec for HTTP connectivity verification
//   - agnhost-ipvrf: For L3 IP-VRF connectivity tests
//     * Connected to vrf202 (routed via br0.202 SVI)
//     * IP: 172.27.102.2/24 (external routed network)
//     * Runs agnhost netexec for HTTP connectivity verification
//
// =============================================================================
// Test Type 1: Layer 3 CUDN with IP-VRF
// =============================================================================
//
// This test validates north-south L3 connectivity between pods on a Layer 3
// CUDN and an external server via EVPN Type-5 (IP Prefix) routes.
//
// Data Path (Pod -> External agnhost-ipvrf and vice versa):
//   Pod (10.100.x.x) -> OVN logical network -> ovn-k8s-mpX (management port)
//   -> Linux VRF (mpX-udn-vrf) -> br0.202 (SVI) -> br0 -> vxlan0 (VNI 20102)
//   -> VXLAN tunnel -> External FRR vxlan0 -> br0 -> br0.202 (SVI)
//   -> vrf202 -> agnhost-ipvrf (172.27.102.2)
//
// External FRR Configuration:
//   - vrf202: Linux VRF bound to VNI 20102
//   - br0.202: 802.1Q sub-interface (SVI) for routing, master of vrf202
//   - Bridge VLAN/VNI mapping: VID 202 <-> VNI 20102
//   - BGP: Type-5 routes advertise 172.27.102.0/24 with RT 64512:20102
//
// Cluster Node Configuration:
//   - br0.202: SVI sub-interface bound to OVN-K's UDN VRF (mpX-udn-vrf)
//   - Bridge VLAN/VNI mapping: VID 202 <-> VNI 20102
//   - FRR-K8s: Receives Type-5 routes into VRF, advertises pod subnet
//
// VNI/VID: 20102/202
// Network: agnhost-ipvrf-net (172.27.102.0/24)
//
// =============================================================================
// Test Type 2: Layer 2 CUDN with MAC-VRF
// =============================================================================
//
// This test validates north-south L2 connectivity between pods on a Layer 2
// CUDN and an external server via EVPN Type-2 (MAC/IP) and Type-3 (IMET) routes.
//
// Data Path (Pod -> External agnhost-macvrf and vice versa):
//   Pod (10.100.0.x) -> OVN logical switch -> OVS internal port (evpn10100)
//   -> br0 (tagged VID 100) -> vxlan0 (VNI 10100)
//   -> VXLAN tunnel -> External FRR vxlan0 -> br0 (VID 100)
//   -> evpn10100 (access port) -> agnhost-macvrf (10.100.0.250)
//
// External FRR Configuration:
//   - br0: Linux bridge with VLAN filtering, no VRF (pure L2)
//   - evpn10100: Access port on br0 with PVID 100 (untagged)
//   - Bridge VLAN/VNI mapping: VID 100 <-> VNI 10100
//   - BGP: Type-2/Type-3 routes for MAC learning and BUM traffic
//
// Cluster Node Configuration:
//   - evpn10100: OVS internal port connected to OVN logical switch
//   - OVS port enslaved to br0 with VID 100 (access mode)
//   - Bridge VLAN/VNI mapping: VID 100 <-> VNI 10100
//   - FRR-K8s: EVPN Type-2/Type-3 for MAC advertisement
//
// VNI/VID: 10100/100
// Network: CUDN-network (10.100.0.0/16) - same subnet as the CUDN
//
// =============================================================================
// Test Type 3: Layer 2 CUDN with both MAC-VRF and IP-VRF
// =============================================================================
//
// This test combines both L2 and L3 EVPN connectivity on a single Layer 2 CUDN.
// Pods can reach:
//   - agnhost-macvrf via L2 (MAC-VRF, same subnet bridging)
//   - agnhost-ipvrf via L3 (IP-VRF, routed via SVI, reply can be assymmetric)
//
// This is the most complex scenario, requiring both:
//   - OVS port to br0 for L2 MAC-VRF traffic (VNI 10100)
//   - SVI (br0.202) bound to OVN-K VRF for L3 IP-VRF traffic (VNI 20102)
//
// External FRR Configuration:
//   - MAC-VRF: evpn10100 access port, VID 100 <-> VNI 10100
//   - IP-VRF: vrf202 + br0.202 SVI, VID 202 <-> VNI 20102
//   - BGP: Both Type-2/Type-3 (MAC-VRF) and Type-5 (IP-VRF) routes
//
// Cluster Node Configuration:
//   - MAC-VRF: OVS internal port evpn10100 -> br0 (VID 100)
//   - IP-VRF: br0.202 SVI -> mpX-udn-vrf (VID 202)
//   - FRR-K8s: Full EVPN config for both MAC-VRF and IP-VRF
//
// =============================================================================
// SVD (Single VXLAN Device) Mode
// =============================================================================
//
// All tests use SVD mode where a single vxlan0 device handles multiple VNIs
// via the vnifilter option. This is more efficient than creating separate
// VXLAN devices per VNI.
//
// vxlan0 configuration:
//   - external: Allows FDB programming for remote VTEPs
//   - vnifilter: Enables per-VLAN VNI mapping via bridge
//   - nolearning: Disables MAC learning (controlled by BGP EVPN)
//
// Bridge VLAN/VNI mapping commands:
//   bridge vlan add dev br0 vid <VID> self
//   bridge vlan add dev vxlan0 vid <VID>
//   bridge vni add dev vxlan0 vni <VNI>
//   bridge vlan add dev vxlan0 vid <VID> tunnel_info id <VNI>
//
// =============================================================================
// See OKEP: https://github.com/ovn-org/ovn-kubernetes/blob/master/docs/okeps/okep-5088-evpn.md for more details.

// =============================================================================
// EVPN Utilities
// =============================================================================

const (
	// externalFRRContainerName is the name of the external FRR container
	// created during KIND cluster setup with BGP enabled (./contrib/kind.sh -rae)
	externalFRRContainerName = "frr"
	// agnhostHTTPPort is the HTTP port for agnhost netexec
	agnhostHTTPPort = 8080
	// netshootImage is a network troubleshooting container with tools like 'ip'.
	// We use it to create veth pairs in the host namespace and move them into
	// container namespaces. Running 'docker run --privileged' gives us CAP_NET_ADMIN
	// without requiring sudo on the test machine - only Docker permissions are needed.
	// The container runs with --network host --pid host to access host namespaces.
	netshootImage = "ghcr.io/nicolaka/netshoot:v0.13"
)

// setupEVPNBridgeOnExternalFRR creates the EVPN bridge (br0) and VXLAN device (vxlan0)
// on the external FRR container. This is the foundation for both MAC-VRF and IP-VRF tests.
//
// Creates:
//   - br0: Linux bridge with vlan_filtering enabled, vlan_default_pvid 0
//   - vxlan0: VXLAN device in SVD (Single VXLAN Device) mode with vnifilter
//
// The vxlan0 device is configured with:
//   - dstport 4789: Standard VXLAN port
//   - local <frrVTEPIPAddress>: Local VTEP IP (FRR's IP on KIND network) - in future when VTEP CR is implemented, this will be the VTEP IP
//   - nolearning: Disable MAC learning (controlled by BGP EVPN)
//   - external: Allow external FDB programming
//   - vnifilter: Enable per-VLAN VNI mapping (SVD mode)
//
// Cleanup is automatically registered via ictx.AddCleanUpFn().
func setupEVPNBridgeOnExternalFRR(ictx infraapi.Context, frrVTEPIPAddress string) error {
	frr := infraapi.ExternalContainer{Name: externalFRRContainerName}

	// Create br0 bridge with VLAN filtering
	commands := [][]string{
		{"ip", "link", "add", "br0", "type", "bridge", "vlan_filtering", "1", "vlan_default_pvid", "0"},
		{"ip", "link", "set", "br0", "addrgenmode", "none"},
	}

	for _, cmd := range commands {
		_, err := infraprovider.Get().ExecExternalContainerCommand(frr, cmd)
		if err != nil {
			return fmt.Errorf("failed to execute %v: %w", cmd, err)
		}
	}

	// Create vxlan0 in SVD mode
	vxlanCmd := []string{
		"ip", "link", "add", "vxlan0", "type", "vxlan",
		"dstport", "4789",
		"local", frrVTEPIPAddress,
		"nolearning",
		"external",
		"vnifilter",
	}
	_, err := infraprovider.Get().ExecExternalContainerCommand(frr, vxlanCmd)
	if err != nil {
		return fmt.Errorf("failed to create vxlan0: %w", err)
	}

	// Configure vxlan0
	commands = [][]string{
		{"ip", "link", "set", "vxlan0", "addrgenmode", "none"},
		{"ip", "link", "set", "vxlan0", "master", "br0"},
	}

	for _, cmd := range commands {
		_, err := infraprovider.Get().ExecExternalContainerCommand(frr, cmd)
		if err != nil {
			return fmt.Errorf("failed to execute %v: %w", cmd, err)
		}
	}

	// Bring up interfaces
	commands = [][]string{
		{"ip", "link", "set", "br0", "up"},
		{"ip", "link", "set", "vxlan0", "up"},
	}

	for _, cmd := range commands {
		_, err := infraprovider.Get().ExecExternalContainerCommand(frr, cmd)
		if err != nil {
			return fmt.Errorf("failed to execute %v: %w", cmd, err)
		}
	}

	// Configure vxlan0 bridge options for EVPN
	bridgeCmd := []string{
		"bridge", "link", "set", "dev", "vxlan0",
		"vlan_tunnel", "on",
		"neigh_suppress", "on",
		"learning", "off",
	}
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr, bridgeCmd)
	if err != nil {
		return fmt.Errorf("failed to configure vxlan0 bridge options: %w", err)
	}

	// Register cleanup to remove br0 and vxlan0
	// Note: Deleting br0 also removes all associated bridge vlan and vni entries,
	// so explicit MAC-VRF cleanup is not needed.
	ictx.AddCleanUpFn(func() error {
		frr := infraapi.ExternalContainer{Name: externalFRRContainerName}

		// Delete vxlan0 first (it's attached to br0)
		_, err := infraprovider.Get().ExecExternalContainerCommand(frr, []string{"ip", "link", "del", "vxlan0"})
		if err != nil {
			return fmt.Errorf("failed to delete vxlan0: %w", err)
		}

		// Delete br0
		_, err = infraprovider.Get().ExecExternalContainerCommand(frr, []string{"ip", "link", "del", "br0"})
		if err != nil {
			return fmt.Errorf("failed to delete br0: %w", err)
		}

		framework.Logf("EVPN bridge cleanup complete on %s", externalFRRContainerName)
		return nil
	})

	framework.Logf("EVPN bridge setup complete on %s (br0 + vxlan0 with local IP %s)", externalFRRContainerName, frrVTEPIPAddress)
	return nil
}

// setupMACVRFOnExternalFRR configures MAC-VRF (Layer 2 EVPN) on the external FRR container.
// This adds the VLAN/VNI mapping to extend the L2 domain via EVPN Type-2/Type-3 routes.
//
// Requires: setupEVPNBridgeOnExternalFRR must be called first to create br0 and vxlan0.
//
// Parameters:
//   - vni: VXLAN Network Identifier (e.g., 10100)
//   - vid: VLAN ID for local bridging (e.g., 100)
func setupMACVRFOnExternalFRR(vni, vid int) error {
	frr := infraapi.ExternalContainer{Name: externalFRRContainerName}
	vidStr := fmt.Sprintf("%d", vid)
	vniStr := fmt.Sprintf("%d", vni)

	// Add VLAN to bridge
	_, err := infraprovider.Get().ExecExternalContainerCommand(frr,
		[]string{"bridge", "vlan", "add", "dev", "br0", "vid", vidStr, "self"})
	if err != nil {
		return fmt.Errorf("failed to add VLAN %d to br0: %w", vid, err)
	}

	// Add VLAN to vxlan0
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr,
		[]string{"bridge", "vlan", "add", "dev", "vxlan0", "vid", vidStr})
	if err != nil {
		return fmt.Errorf("failed to add VLAN %d to vxlan0: %w", vid, err)
	}

	// Add VNI to vxlan0
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr,
		[]string{"bridge", "vni", "add", "dev", "vxlan0", "vni", vniStr})
	if err != nil {
		return fmt.Errorf("failed to add VNI %d to vxlan0: %w", vni, err)
	}

	// Map VLAN to VNI (tunnel_info)
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr,
		[]string{"bridge", "vlan", "add", "dev", "vxlan0", "vid", vidStr, "tunnel_info", "id", vniStr})
	if err != nil {
		return fmt.Errorf("failed to map VLAN %d to VNI %d: %w", vid, vni, err)
	}

	framework.Logf("MAC-VRF setup complete on %s (VNI %d, VID %d)", externalFRRContainerName, vni, vid)
	return nil
}

// setupIPVRFOnExternalFRR configures IP-VRF (Layer 3 EVPN) on the external FRR container.
// This creates a Linux VRF with SVI for L3 routing via EVPN Type-5 routes.
//
// Requires: setupEVPNBridgeOnExternalFRR must be called first to create br0 and vxlan0.
//
// Parameters:
//   - vrfName: Name of the Linux VRF (e.g., "vrf202")
//   - vni: VXLAN Network Identifier (e.g., 20102)
//   - vid: VLAN ID for the SVI (e.g., 202)
//
// Cleanup is automatically registered via ictx.AddCleanUpFn().
// Note: VLAN/VNI mappings are cleaned up when br0/vxlan0 are deleted.
func setupIPVRFOnExternalFRR(ictx infraapi.Context, vrfName string, vni, vid int) error {
	frr := infraapi.ExternalContainer{Name: externalFRRContainerName}
	vidStr := fmt.Sprintf("%d", vid)
	vniStr := fmt.Sprintf("%d", vni)

	// Create Linux VRF with routing table = VNI
	_, err := infraprovider.Get().ExecExternalContainerCommand(frr,
		[]string{"ip", "link", "add", vrfName, "type", "vrf", "table", vniStr})
	if err != nil {
		return fmt.Errorf("failed to create VRF %s: %w", vrfName, err)
	}

	// Bring up VRF
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr,
		[]string{"ip", "link", "set", vrfName, "up"})
	if err != nil {
		return fmt.Errorf("failed to bring up VRF %s: %w", vrfName, err)
	}

	// Configure VLAN/VNI mapping (reuse MAC-VRF setup for this part)
	if err := setupMACVRFOnExternalFRR(vni, vid); err != nil {
		return fmt.Errorf("failed to configure VLAN/VNI mapping: %w", err)
	}

	// Create SVI (VLAN sub-interface on br0)
	sviName := fmt.Sprintf("br0.%d", vid)
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr,
		[]string{"ip", "link", "add", sviName, "link", "br0", "type", "vlan", "id", vidStr})
	if err != nil {
		return fmt.Errorf("failed to create SVI %s: %w", sviName, err)
	}

	// Bind SVI to VRF
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr,
		[]string{"ip", "link", "set", sviName, "master", vrfName})
	if err != nil {
		return fmt.Errorf("failed to bind SVI %s to VRF %s: %w", sviName, vrfName, err)
	}

	// Bring up SVI
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr,
		[]string{"ip", "link", "set", sviName, "up"})
	if err != nil {
		return fmt.Errorf("failed to bring up SVI %s: %w", sviName, err)
	}

	// Register cleanup to remove SVI, Linux VRF, and FRR VRF definition
	ictx.AddCleanUpFn(func() error {
		frr := infraapi.ExternalContainer{Name: externalFRRContainerName}

		// Delete SVI
		sviName := fmt.Sprintf("br0.%d", vid)
		_, err := infraprovider.Get().ExecExternalContainerCommand(frr,
			[]string{"ip", "link", "del", sviName})
		if err != nil {
			framework.Logf("Warning: failed to delete SVI %s: %v", sviName, err)
		}

		// Delete Linux VRF
		_, err = infraprovider.Get().ExecExternalContainerCommand(frr,
			[]string{"ip", "link", "del", vrfName})
		if err != nil {
			framework.Logf("Warning: failed to delete Linux VRF %s: %v", vrfName, err)
		}

		// Delete FRR VRF definition (now that Linux VRF is gone, FRR should allow this)
		cmd := []string{"vtysh", "-c", "configure terminal", "-c", fmt.Sprintf("no vrf %s", vrfName), "-c", "end"}
		_, err = infraprovider.Get().ExecExternalContainerCommand(frr, cmd)
		if err != nil {
			framework.Logf("Warning: failed to delete FRR VRF definition %s: %v", vrfName, err)
		}

		framework.Logf("IP-VRF cleanup complete on %s (VRF %s, VID %d)", externalFRRContainerName, vrfName, vid)
		return nil
	})

	framework.Logf("IP-VRF setup complete on %s (VRF %s, VNI %d, VID %d)", externalFRRContainerName, vrfName, vni, vid)
	return nil
}

// setupEVPNBGPOnExternalFRR configures the global BGP settings for EVPN on the external FRR container.
// This enables the l2vpn evpn address-family, advertise-all-vni, and activates neighbors.
//
// This is required for ALL EVPN scenarios (MAC-VRF and IP-VRF).
// advertise-all-vni automatically handles MAC-VRF route advertisement.
//
// Parameters:
//   - asn: BGP Autonomous System Number (e.g., 64512)
//   - neighborIPs: List of cluster node IPs to peer with
//
// Cleanup is automatically registered via ictx.AddCleanUpFn().
func setupEVPNBGPOnExternalFRR(ictx infraapi.Context, asn int, neighborIPs []string) error {
	frr := infraapi.ExternalContainer{Name: externalFRRContainerName}

	// Build vtysh command with all the -c arguments
	cmd := []string{"vtysh"}
	cmd = append(cmd, "-c", "configure terminal")
	cmd = append(cmd, "-c", fmt.Sprintf("router bgp %d", asn))
	cmd = append(cmd, "-c", "address-family l2vpn evpn")
	cmd = append(cmd, "-c", "advertise-all-vni")

	// Activate each neighbor in the l2vpn evpn address-family
	for _, ip := range neighborIPs {
		cmd = append(cmd, "-c", fmt.Sprintf("neighbor %s activate", ip))
	}

	cmd = append(cmd, "-c", "exit-address-family")
	cmd = append(cmd, "-c", "end")

	_, err := infraprovider.Get().ExecExternalContainerCommand(frr, cmd)
	if err != nil {
		return fmt.Errorf("failed to configure EVPN BGP: %w", err)
	}

	// Register cleanup to remove advertise-all-vni and deactivate neighbors
	ictx.AddCleanUpFn(func() error {
		frr := infraapi.ExternalContainer{Name: externalFRRContainerName}

		cmd := []string{"vtysh"}
		cmd = append(cmd, "-c", "configure terminal")
		cmd = append(cmd, "-c", fmt.Sprintf("router bgp %d", asn))
		cmd = append(cmd, "-c", "address-family l2vpn evpn")
		cmd = append(cmd, "-c", "no advertise-all-vni")
		// Deactivate each neighbor in the l2vpn evpn address-family
		for _, ip := range neighborIPs {
			cmd = append(cmd, "-c", fmt.Sprintf("no neighbor %s activate", ip))
		}
		cmd = append(cmd, "-c", "exit-address-family")
		cmd = append(cmd, "-c", "end")

		_, err := infraprovider.Get().ExecExternalContainerCommand(frr, cmd)
		if err != nil {
			return fmt.Errorf("failed to cleanup EVPN BGP: %w", err)
		}

		framework.Logf("EVPN BGP cleanup complete on %s", externalFRRContainerName)
		return nil
	})

	framework.Logf("EVPN BGP setup complete on %s (ASN %d, neighbors: %v)", externalFRRContainerName, asn, neighborIPs)
	return nil
}

// setupIPVRFBGPOnExternalFRR configures BGP for an IP-VRF on the external FRR container.
// This binds the VRF to a VNI and configures route-targets for Type-5 route exchange.
//
// Requires: setupEVPNBGPOnExternalFRR must be called first for global EVPN BGP config.
//
// Parameters:
//   - vrfName: Name of the Linux VRF (must match the VRF created by setupIPVRFOnExternalFRR)
//   - asn: BGP Autonomous System Number (e.g., 64512)
//   - vni: VXLAN Network Identifier for route-target (e.g., 20102)
//   - ipFamilies: IP families to configure (e.g., []utilnet.IPFamily{utilnet.IPv4} or {utilnet.IPv4, utilnet.IPv6})
//   - subnets: Subnets to advertise via BGP (e.g., []string{"172.27.102.0/24"} or {"172.27.102.0/24", "fd00:102::/64"})
//
// Cleanup is automatically registered via ictx.AddCleanUpFn().
func setupIPVRFBGPOnExternalFRR(ictx infraapi.Context, vrfName string, asn, vni int, ipFamilies []utilnet.IPFamily, subnets []string) error {
	frr := infraapi.ExternalContainer{Name: externalFRRContainerName}
	rt := fmt.Sprintf("%d:%d", asn, vni)

	// Build vtysh command
	cmd := []string{"vtysh"}
	cmd = append(cmd, "-c", "configure terminal")

	// Bind VRF to VNI
	cmd = append(cmd, "-c", fmt.Sprintf("vrf %s", vrfName))
	cmd = append(cmd, "-c", fmt.Sprintf("vni %d", vni))
	cmd = append(cmd, "-c", "exit-vrf")

	// Configure BGP for the VRF
	cmd = append(cmd, "-c", fmt.Sprintf("router bgp %d vrf %s", asn, vrfName))

	// Configure address-families based on ipFamilies with explicit network statements
	// (Using explicit 'network' statements instead of 'redistribute connected' to align
	// with how OVN-K advertises routes via RouteAdvertisements/FRRConfiguration Prefixes)
	for _, family := range ipFamilies {
		switch family {
		case utilnet.IPv4:
			cmd = append(cmd, "-c", "address-family ipv4 unicast")
			for _, subnet := range subnets {
				if !strings.Contains(subnet, ":") { // IPv4 subnet
					cmd = append(cmd, "-c", fmt.Sprintf("network %s", subnet))
				}
			}
			cmd = append(cmd, "-c", "exit-address-family")
		case utilnet.IPv6:
			cmd = append(cmd, "-c", "address-family ipv6 unicast")
			for _, subnet := range subnets {
				if strings.Contains(subnet, ":") { // IPv6 subnet
					cmd = append(cmd, "-c", fmt.Sprintf("network %s", subnet))
				}
			}
			cmd = append(cmd, "-c", "exit-address-family")
		}
	}

	// l2vpn evpn - configure RD, RT, and advertise unicast routes
	cmd = append(cmd, "-c", "address-family l2vpn evpn")
	cmd = append(cmd, "-c", fmt.Sprintf("rd %s", rt))
	cmd = append(cmd, "-c", fmt.Sprintf("route-target import %s", rt))
	cmd = append(cmd, "-c", fmt.Sprintf("route-target export %s", rt))
	for _, family := range ipFamilies {
		switch family {
		case utilnet.IPv4:
			cmd = append(cmd, "-c", "advertise ipv4 unicast")
		case utilnet.IPv6:
			cmd = append(cmd, "-c", "advertise ipv6 unicast")
		}
	}
	cmd = append(cmd, "-c", "exit-address-family")

	cmd = append(cmd, "-c", "end")

	_, err := infraprovider.Get().ExecExternalContainerCommand(frr, cmd)
	if err != nil {
		return fmt.Errorf("failed to configure IP-VRF BGP for %s: %w", vrfName, err)
	}

	// Register cleanup to remove BGP VRF instance and VRF-VNI binding
	// NOTE: This cleanup may run after setupIPVRFOnExternalFRR cleanup has already deleted
	// the Linux VRF device, which triggers FRR to auto-cleanup the VRF config. In that case,
	// these commands may fail - that's OK, we just log warnings and won't retry that.
	ictx.AddCleanUpFn(func() error {
		frr := infraapi.ExternalContainer{Name: externalFRRContainerName}

		// Try to remove the VNI binding from VRF context
		cmd := []string{"vtysh"}
		cmd = append(cmd, "-c", "configure terminal")
		cmd = append(cmd, "-c", fmt.Sprintf("vrf %s", vrfName))
		cmd = append(cmd, "-c", fmt.Sprintf("no vni %d", vni))
		cmd = append(cmd, "-c", "exit-vrf")
		cmd = append(cmd, "-c", "end")

		_, err := infraprovider.Get().ExecExternalContainerCommand(frr, cmd)
		if err != nil {
			framework.Logf("Warning: failed to remove VNI binding (may already be cleaned up): %v", err)
		}

		// Remove BGP VRF instance
		cmd = []string{"vtysh"}
		cmd = append(cmd, "-c", "configure terminal")
		cmd = append(cmd, "-c", fmt.Sprintf("no router bgp %d vrf %s", asn, vrfName))
		cmd = append(cmd, "-c", "end")

		_, err = infraprovider.Get().ExecExternalContainerCommand(frr, cmd)
		if err != nil {
			framework.Logf("Warning: failed to remove BGP VRF (may already be cleaned up): %v", err)
		}

		// NOTE: We intentionally do NOT run "no vrf" here.
		// FRR's VRF definition is tied to the Linux VRF and will auto-cleanup
		// when the Linux VRF is deleted by setupIPVRFOnExternalFRR's cleanup.
		// Trying to delete the FRR VRF while the Linux VRF still has interfaces
		// attached causes "Only inactive VRFs can be deleted" errors.

		framework.Logf("IP-VRF BGP cleanup complete on %s (VRF %s)", externalFRRContainerName, vrfName)
		return nil
	})

	framework.Logf("IP-VRF BGP setup complete on %s (VRF %s, ASN %d, VNI %d, RT %s, families %v)", externalFRRContainerName, vrfName, asn, vni, rt, ipFamilies)
	return nil
}

// =============================================================================
// MAC-VRF Agnhost Utilities
// =============================================================================

// setupMACVRFAgnhost creates an agnhost container connected to the EVPN bridge
// for MAC-VRF (Layer 2) connectivity testing.
//
// This function:
//  1. Creates an agnhost container with --network none
//  2. Creates a veth pair using netshoot (requires host network/PID access)
//  3. Moves veth ends into agnhost and FRR container namespaces
//  4. Configures the FRR-side interface as an access port on br0
//  5. Assigns an IP address to the agnhost
//
// Requires: setupEVPNBridgeOnExternalFRR and setupMACVRFOnExternalFRR must be called first.
//
// The agnhost will be on the same L2 segment as pods on the CUDN, allowing
// direct Layer 2 communication via EVPN Type-2/Type-3 routes.
//
// Names are derived from VID to support multiple MAC-VRF agnhosts:
//   - Container: agnhost-macvrf-<vid> (e.g., agnhost-macvrf-100)
//   - FRR interface: macvrf<vid> (e.g., macvrf100)
//
// Parameters:
//   - vid: VLAN ID for the access port on br0 (e.g., 100)
//   - ipAddresses: IP addresses with prefix to assign to the agnhost (e.g., []string{"10.100.0.250/16", "fd00:100::250/64"})
func setupMACVRFAgnhost(ictx infraapi.Context, vid int, ipAddresses []string) error {
	// Derive names from VID to support multiple MAC-VRF agnhosts
	containerName := fmt.Sprintf("agnhost-macvrf-%d", vid)
	macvrfInterface := fmt.Sprintf("macvrf%d", vid)
	vethAgnhost := fmt.Sprintf("veth%da", vid)
	vethFRR := fmt.Sprintf("veth%df", vid)

	// Step 1: Create agnhost container with --network none
	agnhostContainer := infraapi.ExternalContainer{
		Name:        containerName,
		Image:       images.AgnHost(),
		Network:     nil, // --network none
		CmdArgs:     []string{"netexec", fmt.Sprintf("--http-port=%d", agnhostHTTPPort)},
		RuntimeArgs: []string{"--cap-add=NET_ADMIN"},
	}
	_, err := ictx.CreateExternalContainer(agnhostContainer)
	if err != nil {
		return fmt.Errorf("failed to create agnhost container %s: %w", containerName, err)
	}

	// Step 2: Get container PIDs for namespace manipulation
	agnhostPID, err := infraprovider.Get().GetExternalContainerPID(containerName)
	if err != nil {
		return fmt.Errorf("failed to get agnhost PID: %w", err)
	}

	frrPID, err := infraprovider.Get().GetExternalContainerPID(externalFRRContainerName)
	if err != nil {
		return fmt.Errorf("failed to get FRR PID: %w", err)
	}

	framework.Logf("Container PIDs - Agnhost (%s): %d, FRR: %d", containerName, agnhostPID, frrPID)

	// Step 3: Create veth pair using netshoot helper
	// netshoot runs with --network host --pid host to access host namespaces
	netshootRuntimeArgs := []string{"--privileged", "--pid=host", "--network=host"}

	// Create veth pair in host namespace
	_, err = infraprovider.Get().RunOneShotContainer(netshootImage,
		[]string{"ip", "link", "add", vethAgnhost, "type", "veth", "peer", "name", vethFRR},
		netshootRuntimeArgs)
	if err != nil {
		return fmt.Errorf("failed to create veth pair: %w", err)
	}

	// Move agnhost end to agnhost container namespace
	_, err = infraprovider.Get().RunOneShotContainer(netshootImage,
		[]string{"ip", "link", "set", vethAgnhost, "netns", fmt.Sprintf("%d", agnhostPID)},
		netshootRuntimeArgs)
	if err != nil {
		return fmt.Errorf("failed to move veth to agnhost namespace: %w", err)
	}

	// Move FRR end to FRR container namespace
	_, err = infraprovider.Get().RunOneShotContainer(netshootImage,
		[]string{"ip", "link", "set", vethFRR, "netns", fmt.Sprintf("%d", frrPID)},
		netshootRuntimeArgs)
	if err != nil {
		return fmt.Errorf("failed to move veth to FRR namespace: %w", err)
	}

	// Step 4: Rename and configure interfaces inside containers
	frr := infraapi.ExternalContainer{Name: externalFRRContainerName}
	agnhost := infraapi.ExternalContainer{Name: containerName}

	// Rename in agnhost: veth<vid>a -> eth0
	_, err = infraprovider.Get().ExecExternalContainerCommand(agnhost, []string{"ip", "link", "set", vethAgnhost, "name", "eth0"})
	if err != nil {
		return fmt.Errorf("failed to rename interface in agnhost: %w", err)
	}

	// Rename in FRR: veth<vid>f -> macvrf<vid>
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr, []string{"ip", "link", "set", vethFRR, "name", macvrfInterface})
	if err != nil {
		return fmt.Errorf("failed to rename interface in FRR: %w", err)
	}

	// Bring up interfaces
	_, err = infraprovider.Get().ExecExternalContainerCommand(agnhost, []string{"ip", "link", "set", "eth0", "up"})
	if err != nil {
		return fmt.Errorf("failed to bring up eth0 in agnhost: %w", err)
	}
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr, []string{"ip", "link", "set", macvrfInterface, "up"})
	if err != nil {
		return fmt.Errorf("failed to bring up %s in FRR: %w", macvrfInterface, err)
	}

	// Step 5: Add FRR's interface to br0 as access port for the MAC-VRF VLAN
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr, []string{"ip", "link", "set", macvrfInterface, "master", "br0"})
	if err != nil {
		return fmt.Errorf("failed to add %s to br0: %w", macvrfInterface, err)
	}

	// Configure as access port with PVID
	vidStr := fmt.Sprintf("%d", vid)
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr,
		[]string{"bridge", "vlan", "add", "dev", macvrfInterface, "vid", vidStr, "pvid", "untagged"})
	if err != nil {
		return fmt.Errorf("failed to configure %s as access port: %w", macvrfInterface, err)
	}

	// Step 6: Configure agnhost IP addresses (supports dual-stack)
	for _, ipWithPrefix := range ipAddresses {
		_, err = infraprovider.Get().ExecExternalContainerCommand(agnhost, []string{"ip", "addr", "add", ipWithPrefix, "dev", "eth0"})
		if err != nil {
			return fmt.Errorf("failed to configure IP %s on agnhost: %w", ipWithPrefix, err)
		}
	}

	// Cleanup is handled automatically:
	// - ictx.CreateExternalContainer() registers container deletion
	// - Veth pair is auto-deleted when the agnhost container is removed

	framework.Logf("MAC-VRF agnhost setup complete: %s (IPs: %v, VID: %d, interface: %s)", containerName, ipAddresses, vid, macvrfInterface)
	return nil
}

// =============================================================================
// IP-VRF Agnhost Utilities
// =============================================================================

// setupIPVRFAgnhost creates an agnhost container connected to the external FRR's VRF
// for IP-VRF (Layer 3) connectivity testing.
//
// This function:
//  1. Creates a Docker network with the specified subnet
//  2. Creates an agnhost container on that network
//  3. Connects FRR to the network
//  4. Discovers assigned IPs (Docker assigns them from subnet)
//  5. Puts FRR's interface for that network into the VRF
//  6. Sets agnhost's default route via FRR
//
// Requires: setupIPVRFOnExternalFRR must be called first to create the VRF.
//
// The agnhost will be on a separate routed subnet, reachable via EVPN Type-5 routes.
//
// Names are derived from VID to support multiple IP-VRF agnhosts:
//   - Network: ipvrf-net-<vid> (e.g., ipvrf-net-202)
//   - Container: agnhost-ipvrf-<vid> (e.g., agnhost-ipvrf-202)
//
// Parameters:
//   - vid: VLAN ID used to derive names (should match the VRF's VID)
//   - vrfName: Name of the VRF to put FRR's interface in (must match setupIPVRFOnExternalFRR)
//   - ipFamilies: Cluster IP family support (e.g., []utilnet.IPFamily{utilnet.IPv4, utilnet.IPv6})
//   - subnets: Subnets for the Docker network (e.g., "172.27.102.0/24" for IPv4, or both for dual-stack)
//
// Returns:
//   - Agnhost's IP addresses (IPv4 and/or IPv6 depending on cluster IP family support)
func setupIPVRFAgnhost(ictx infraapi.Context, vid int, vrfName string, ipFamilies []utilnet.IPFamily, subnets ...string) ([]string, error) {
	// Derive names from VID
	networkName := fmt.Sprintf("ipvrf-net-%d", vid)
	containerName := fmt.Sprintf("agnhost-ipvrf-%d", vid)

	// Step 1: Create Docker network with specific subnet(s)
	network, err := ictx.CreateNetwork(networkName, subnets...)
	if err != nil {
		return nil, fmt.Errorf("failed to create network %s: %w", networkName, err)
	}

	// Step 2: Create agnhost container on that network
	agnhostContainer := infraapi.ExternalContainer{
		Name:        containerName,
		Image:       images.AgnHost(),
		Network:     network,
		CmdArgs:     []string{"netexec", fmt.Sprintf("--http-port=%d", agnhostHTTPPort)},
		RuntimeArgs: []string{"--cap-add=NET_ADMIN"},
	}
	_, err = ictx.CreateExternalContainer(agnhostContainer)
	if err != nil {
		return nil, fmt.Errorf("failed to create agnhost container %s: %w", containerName, err)
	}

	// Step 3: Connect FRR to the network
	_, err = ictx.AttachNetwork(network, externalFRRContainerName)
	if err != nil {
		return nil, fmt.Errorf("failed to connect FRR to network %s: %w", networkName, err)
	}

	// Step 4: Discover assigned IPs using infraprovider (dual-stack aware)
	agnhostNetInf, err := infraprovider.Get().GetExternalContainerNetworkInterface(
		infraapi.ExternalContainer{Name: containerName}, network)
	if err != nil {
		return nil, fmt.Errorf("failed to get agnhost network interface: %w", err)
	}

	frrNetInf, err := infraprovider.Get().GetExternalContainerNetworkInterface(
		infraapi.ExternalContainer{Name: externalFRRContainerName}, network)
	if err != nil {
		return nil, fmt.Errorf("failed to get FRR network interface: %w", err)
	}

	// Collect agnhost IPs and FRR gateway IPs only for cluster-supported address families.
	// Docker may assign IPs for families we didn't request (e.g., default IPv4 on IPv6-only networks),
	// so we filter based on what the cluster actually supports.
	var agnhostIPs []string
	var frrGWIPs []string
	for _, family := range ipFamilies {
		switch family {
		case utilnet.IPv4:
			if agnhostNetInf.IPv4 != "" {
				agnhostIPs = append(agnhostIPs, agnhostNetInf.IPv4)
				frrGWIPs = append(frrGWIPs, frrNetInf.IPv4)
			}
		case utilnet.IPv6:
			if agnhostNetInf.IPv6 != "" {
				agnhostIPs = append(agnhostIPs, agnhostNetInf.IPv6)
				frrGWIPs = append(frrGWIPs, frrNetInf.IPv6)
			}
		}
	}

	framework.Logf("IP-VRF IPs - Agnhost: %v, FRR Gateway: %v", agnhostIPs, frrGWIPs)

	// Step 5: Put FRR's interface for this network in the VRF
	// The interface name is already available from GetExternalContainerNetworkInterface
	frrInterface := frrNetInf.InfName
	if frrInterface == "" {
		return nil, fmt.Errorf("FRR interface name not found for network")
	}

	framework.Logf("FRR interface for agnhost network: %s", frrInterface)
	frr := infraapi.ExternalContainer{Name: externalFRRContainerName}

	// Put FRR's interface in the VRF
	// NOTE: Moving an interface to a VRF in Linux removes IPv6 global addresses
	// (IPv4 addresses are preserved). We must re-add the IPv6 address after VRF assignment.
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr,
		[]string{"ip", "link", "set", frrInterface, "master", vrfName})
	if err != nil {
		return nil, fmt.Errorf("failed to put interface %s in VRF %s: %w", frrInterface, vrfName, err)
	}

	_, err = infraprovider.Get().ExecExternalContainerCommand(frr,
		[]string{"ip", "link", "set", frrInterface, "up"})
	if err != nil {
		return nil, fmt.Errorf("failed to bring up interface %s: %w", frrInterface, err)
	}

	// Step 6: Set agnhost's default routes via FRR (for each address family)
	agnhost := infraapi.ExternalContainer{Name: containerName}

	for _, gwIP := range frrGWIPs {
		// Delete existing default route for this family (ignore error if none exists)
		if strings.Contains(gwIP, ":") {
			_, _ = infraprovider.Get().ExecExternalContainerCommand(agnhost,
				[]string{"ip", "-6", "route", "del", "default"})
			// Add IPv6 default route via FRR gateway
			_, err = infraprovider.Get().ExecExternalContainerCommand(agnhost,
				[]string{"ip", "-6", "route", "add", "default", "via", gwIP, "dev", "eth0"})
		} else {
			_, _ = infraprovider.Get().ExecExternalContainerCommand(agnhost,
				[]string{"ip", "route", "del", "default"})
			// Add IPv4 default route via FRR gateway
			_, err = infraprovider.Get().ExecExternalContainerCommand(agnhost,
				[]string{"ip", "route", "add", "default", "via", gwIP, "dev", "eth0"})
		}
		if err != nil {
			return nil, fmt.Errorf("failed to set default route via %s on agnhost: %w", gwIP, err)
		}
	}

	// Cleanup is handled automatically:
	// - ictx.CreateNetwork() registers network deletion
	// - ictx.CreateExternalContainer() registers container deletion
	// - When network is deleted, Docker disconnects all attached containers (including FRR)

	framework.Logf("IP-VRF agnhost setup complete: %s (IPs: %v, network: %s, VRF: %s)", containerName, agnhostIPs, networkName, vrfName)
	return agnhostIPs, nil
}

// =============================================================================
// VTEP Utilities
// =============================================================================

// createVTEP creates a VTEP (VXLAN Tunnel Endpoint) custom resource for EVPN.
// The VTEP CR defines the IP range from which VTEP IPs are allocated for EVPN VXLAN tunnels.
//
// Parameters:
//   - f: Test framework (used to get client config)
//   - ictx: Infrastructure context for cleanup registration
//   - name: Name of the VTEP CR
//   - cidrs: CIDR ranges for VTEP IP allocation (supports dual-stack with 2 CIDRs)
//   - mode: VTEP mode - "Managed" (OVN-K allocates IPs) or "Unmanaged" (external provider)
func createVTEP(f *framework.Framework, ictx infraapi.Context, name string, cidrs []string, mode vtepv1.VTEPMode) error {
	client, err := vtepclientset.NewForConfig(f.ClientConfig())
	if err != nil {
		return fmt.Errorf("failed to create VTEP client: %w", err)
	}

	// Convert string CIDRs to vtepv1.CIDR type
	vtepCIDRs := make(vtepv1.DualStackCIDRs, len(cidrs))
	for i, cidr := range cidrs {
		vtepCIDRs[i] = vtepv1.CIDR(cidr)
	}

	vtep := &vtepv1.VTEP{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: vtepv1.VTEPSpec{
			CIDRs: vtepCIDRs,
			Mode:  mode,
		},
	}

	_, err = client.K8sV1().VTEPs().Create(context.Background(), vtep, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create VTEP %s: %w", name, err)
	}

	// Register cleanup
	ictx.AddCleanUpFn(func() error {
		return client.K8sV1().VTEPs().Delete(context.Background(), name, metav1.DeleteOptions{})
	})

	// TODO: Add status check once VTEP controller implements status conditions
	framework.Logf("VTEP created: %s (CIDRs: %v, Mode: %s)", name, cidrs, mode)
	return nil
}

// =============================================================================
// FRRConfiguration Utilities
// =============================================================================

// createFRRConfiguration creates an FRRConfiguration CR for BGP peering with the external FRR.
// This is used by RouteAdvertisements to determine which neighbors to advertise routes to.
//
// For EVPN L3 IP-VRF, we don't need toReceive because:
// - External routes come via EVPN Type-5 and are imported via route-target matching in the VRF
// - The FRRConfiguration just provides the BGP neighbor definition and label for RA selector
//
// Parameters:
//   - ictx: Infrastructure context for cleanup registration
//   - name: Name of the FRRConfiguration CR
//   - namespace: Namespace for the FRRConfiguration (typically frr-k8s-system)
//   - asn: BGP Autonomous System Number (e.g., 64512)
//   - neighborIP: IP address of the external FRR to peer with
//   - labels: Labels to apply to the FRRConfiguration (used by RouteAdvertisements selector)
func createFRRConfiguration(ictx infraapi.Context,
	name, namespace string,
	asn int,
	neighborIP string,
	labels map[string]string) error {

	// Build labels string for YAML
	labelsYAML := ""
	for k, v := range labels {
		labelsYAML += fmt.Sprintf("    %s: %s\n", k, v)
	}

	// Generate FRRConfiguration YAML
	// No toReceive needed - EVPN routes come via l2vpn evpn address-family
	// and are imported via route-target matching in the VRF
	yaml := fmt.Sprintf(`apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  name: %s
  namespace: %s
  labels:
%sspec:
  bgp:
    routers:
    - asn: %d
      neighbors:
      - address: %s
        asn: %d
        disableMP: true
`, name, namespace, labelsYAML, asn, neighborIP, asn)

	// Write to temp file
	tmpFile, err := os.CreateTemp("", "frrconfig-*.yaml")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(yaml); err != nil {
		return fmt.Errorf("failed to write FRRConfiguration YAML: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	// Apply via kubectl
	_, err = e2ekubectl.RunKubectl(namespace, "create", "-f", tmpFile.Name())
	if err != nil {
		return fmt.Errorf("failed to create FRRConfiguration: %w", err)
	}

	// Register cleanup
	ictx.AddCleanUpFn(func() error {
		_, err := e2ekubectl.RunKubectl(namespace, "delete", "frrconfiguration", name, "--ignore-not-found")
		if err != nil {
			return fmt.Errorf("failed to delete FRRConfiguration %s: %w", name, err)
		}
		framework.Logf("FRRConfiguration deleted: %s", name)
		return nil
	})

	framework.Logf("FRRConfiguration created: %s (neighbor: %s)", name, neighborIP)
	return nil
}
