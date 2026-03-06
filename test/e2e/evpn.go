package e2e

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	utilnet "k8s.io/utils/net"
)

// =============================================================================
// EVPN E2E Test Infrastructure
// =============================================================================
//
// This file contains utilities and tests for EVPN (Ethernet VPN) functionality
// in OVN-Kubernetes.
//
// =============================================================================
// Test Infrastructure Overview (PS: the values are just for illustration purposes)
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
// EVPN tests depend on the RouteAdvertisements/BGP infrastructure being set up
// during KIND cluster installation. EVPN tests REUSE the existing FRR container
// created at install time rather than creating a new one.
// We extend its configuration to support EVPN by adding:
//   - a Linux bridge with vlan_filtering for EVPN (unique per test, e.g. brevpn<suffix>)
//   - a VXLAN device (VTEP) with vnifilter for SVD mode (unique per test, e.g. vxevpn<suffix>)
//   - VRF and SVI configuration for IP-VRF tests
//   - Access ports for MAC-VRF tests
//   - BGP EVPN address-family configuration
//
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
)

// setupEVPNBridgeOnExternalFRR creates a Linux bridge and VXLAN device on the external FRR
// container. This is the foundation for both MAC-VRF and IP-VRF tests.
//
// Creates:
//   - bridgeName (e.g. "brevpn7a3f"): Linux bridge with vlan_filtering enabled, vlan_default_pvid 0
//   - vxlanName  (e.g. "vxevpn7a3f"): VXLAN device in SVD (Single VXLAN Device) mode with vnifilter
//
// The vxlanName device is configured with:
//   - dstport 4789: Standard VXLAN port
//   - local <frrVTEPIPAddress>: Local VTEP IP (FRR's IP on KIND network) - in future when VTEP CR is implemented, this will be the VTEP IP
//   - nolearning: Disable MAC learning (controlled by BGP EVPN)
//   - external: Allow external FDB programming
//   - vnifilter: Enable per-VLAN VNI mapping (SVD mode)
//
// Cleanup is automatically registered via ictx.AddCleanUpFn().
func setupEVPNBridgeOnExternalFRR(ictx infraapi.Context, frrVTEPIPAddress, bridgeName, vxlanName string) error {
	frr := infraapi.ExternalContainer{Name: externalFRRContainerName}

	// Create bridge with VLAN filtering
	commands := [][]string{
		{"ip", "link", "add", bridgeName, "type", "bridge", "vlan_filtering", "1", "vlan_default_pvid", "0"},
		{"ip", "link", "set", bridgeName, "addrgenmode", "none"},
	}

	for _, cmd := range commands {
		_, err := infraprovider.Get().ExecExternalContainerCommand(frr, cmd)
		if err != nil {
			return fmt.Errorf("failed to execute %v: %w", cmd, err)
		}
	}

	// Create VXLAN device in SVD mode
	vxlanCmd := []string{
		"ip", "link", "add", vxlanName, "type", "vxlan",
		"dstport", "4789",
		"local", frrVTEPIPAddress,
		"nolearning",
		"external",
		"vnifilter",
	}
	_, err := infraprovider.Get().ExecExternalContainerCommand(frr, vxlanCmd)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", vxlanName, err)
	}

	// Configure VXLAN device
	commands = [][]string{
		{"ip", "link", "set", vxlanName, "addrgenmode", "none"},
		{"ip", "link", "set", vxlanName, "master", bridgeName},
	}

	for _, cmd := range commands {
		_, err := infraprovider.Get().ExecExternalContainerCommand(frr, cmd)
		if err != nil {
			return fmt.Errorf("failed to execute %v: %w", cmd, err)
		}
	}

	// Bring up interfaces
	commands = [][]string{
		{"ip", "link", "set", bridgeName, "up"},
		{"ip", "link", "set", vxlanName, "up"},
	}

	for _, cmd := range commands {
		_, err := infraprovider.Get().ExecExternalContainerCommand(frr, cmd)
		if err != nil {
			return fmt.Errorf("failed to execute %v: %w", cmd, err)
		}
	}

	// Configure VXLAN bridge options for EVPN
	bridgeCmd := []string{
		"bridge", "link", "set", "dev", vxlanName,
		"vlan_tunnel", "on",
		"neigh_suppress", "on",
		"learning", "off",
	}
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr, bridgeCmd)
	if err != nil {
		return fmt.Errorf("failed to configure %s bridge options: %w", vxlanName, err)
	}

	// Register cleanup to remove bridge and VXLAN device.
	ictx.AddCleanUpFn(func() error {
		frr := infraapi.ExternalContainer{Name: externalFRRContainerName}

		// Delete VXLAN device first (it's attached to the bridge)
		_, err := infraprovider.Get().ExecExternalContainerCommand(frr, []string{"ip", "link", "del", vxlanName})
		if err != nil {
			return fmt.Errorf("failed to delete %s: %w", vxlanName, err)
		}

		// Delete bridge
		_, err = infraprovider.Get().ExecExternalContainerCommand(frr, []string{"ip", "link", "del", bridgeName})
		if err != nil {
			return fmt.Errorf("failed to delete %s: %w", bridgeName, err)
		}

		framework.Logf("EVPN bridge cleanup complete on %s", externalFRRContainerName)
		return nil
	})

	framework.Logf("EVPN bridge setup complete on %s (%s + %s with local IP %s)", externalFRRContainerName, bridgeName, vxlanName, frrVTEPIPAddress)
	return nil
}

// setupMACVRFOnExternalFRR configures MAC-VRF (Layer 2 EVPN) on the external FRR container.
// This adds the VLAN/VNI mapping to extend the L2 domain via EVPN Type-2/Type-3 routes.
//
// Requires: setupEVPNBridgeOnExternalFRR must be called first to create bridgeName and vxlanName.
//
// No explicit cleanup is registered here: deleting the bridge (done by setupEVPNBridgeOnExternalFRR's
// cleanup) removes all associated bridge VLAN and VNI entries automatically.
//
// Parameters:
//   - vni: VXLAN Network Identifier (e.g., 10100)
//   - vid: VLAN ID for local bridging (e.g., 100)
//   - bridgeName: name of the bridge device (e.g., "brevpn7a3f")
//   - vxlanName: name of the VXLAN device (e.g., "vxevpn7a3f")
func setupMACVRFOnExternalFRR(vni, vid int, bridgeName, vxlanName string) error {
	frr := infraapi.ExternalContainer{Name: externalFRRContainerName}
	vidStr := fmt.Sprintf("%d", vid)
	vniStr := fmt.Sprintf("%d", vni)

	commands := [][]string{
		// Add VLAN to bridge
		{"bridge", "vlan", "add", "dev", bridgeName, "vid", vidStr, "self"},
		// Add VLAN to vxlan device
		{"bridge", "vlan", "add", "dev", vxlanName, "vid", vidStr},
		// Add VNI to vxlan device
		{"bridge", "vni", "add", "dev", vxlanName, "vni", vniStr},
		// Map VLAN to VNI (tunnel_info)
		{"bridge", "vlan", "add", "dev", vxlanName, "vid", vidStr, "tunnel_info", "id", vniStr},
	}
	for _, cmd := range commands {
		if _, err := infraprovider.Get().ExecExternalContainerCommand(frr, cmd); err != nil {
			return fmt.Errorf("failed to setup MAC-VRF (VNI %d, VID %d): %w", vni, vid, err)
		}
	}

	framework.Logf("MAC-VRF setup complete on %s (VNI %d, VID %d)", externalFRRContainerName, vni, vid)
	return nil
}

// setupIPVRFOnExternalFRR configures IP-VRF (Layer 3 EVPN) on the external FRR container.
// This creates a Linux VRF with SVI for L3 routing via EVPN Type-5 routes.
//
// Requires: setupEVPNBridgeOnExternalFRR must be called first to create bridgeName and vxlanName.
//
// Parameters:
//   - vrfName: Name of the Linux VRF (e.g., "vrf202")
//   - vni: VXLAN Network Identifier (e.g., 20102)
//   - vid: VLAN ID for the SVI (e.g., 202)
//   - bridgeName: name of the bridge device (e.g., "brevpn7a3f")
//   - vxlanName: name of the VXLAN device (e.g., "vxevpn7a3f")
//
// Cleanup is automatically registered via ictx.AddCleanUpFn().
// Note: VLAN/VNI mappings are cleaned up when bridgeName/vxlanName are deleted.
func setupIPVRFOnExternalFRR(ictx infraapi.Context, vrfName string, vni, vid int, bridgeName, vxlanName string) error {
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
	if err := setupMACVRFOnExternalFRR(vni, vid, bridgeName, vxlanName); err != nil {
		return fmt.Errorf("failed to configure VLAN/VNI mapping: %w", err)
	}

	// Create SVI (VLAN sub-interface on bridgeName)
	sviName := fmt.Sprintf("%s.%d", bridgeName, vid)
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr,
		[]string{"ip", "link", "add", sviName, "link", bridgeName, "type", "vlan", "id", vidStr})
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
		sviName := fmt.Sprintf("%s.%d", bridgeName, vid)
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
		_, err = infraprovider.Get().ExecExternalContainerCommand(frr, vtyshCommand(
			"configure terminal", fmt.Sprintf("no vrf %s", vrfName), "end",
		))
		if err != nil {
			framework.Logf("Warning: failed to delete FRR VRF definition %s: %v", vrfName, err)
		}

		framework.Logf("IP-VRF cleanup complete on %s (VRF %s, VID %d)", externalFRRContainerName, vrfName, vid)
		return nil
	})

	framework.Logf("IP-VRF setup complete on %s (VRF %s, VNI %d, VID %d)", externalFRRContainerName, vrfName, vni, vid)
	return nil
}

// vtyshCommand builds a shell command that invokes vtysh with single-quoted -c arguments.
// Using sh -c with single-quoted args ensures correct argument parsing regardless of
// how the infra provider executes the command (docker exec, podman exec, SSH, etc.).
func vtyshCommand(args ...string) []string {
	var parts []string
	for _, arg := range args {
		parts = append(parts, fmt.Sprintf("-c '%s'", arg))
	}
	return []string{"sh", "-c", "vtysh " + strings.Join(parts, " ")}
}

// setupEVPNBGPOnExternalFRR ensures the global BGP EVPN settings are present on the external FRR container.
// This is a redundancy check — the l2vpn evpn address-family, advertise-all-vni, and neighbor
// activations are configured at KIND cluster install time (deploy_frr_external_container in kind-common.sh
// when ENABLE_EVPN=true). Calling this here is idempotent and guards against clusters not set up
// with ENABLE_EVPN.
//
// Parameters:
//   - asn: BGP Autonomous System Number (e.g., 64512)
//   - neighborIPs: List of cluster node IPs to peer with
//
// No cleanup is registered: these are shared cluster-level settings that must persist
// across all parallel EVPN tests.
func setupEVPNBGPOnExternalFRR(ictx infraapi.Context, asn int, neighborIPs []string) error {
	frr := infraapi.ExternalContainer{Name: externalFRRContainerName}

	args := []string{"configure terminal", fmt.Sprintf("router bgp %d", asn), "address-family l2vpn evpn", "advertise-all-vni"}
	for _, ip := range neighborIPs {
		args = append(args, fmt.Sprintf("neighbor %s activate", ip))
		args = append(args, fmt.Sprintf("neighbor %s route-reflector-client", ip))
	}
	args = append(args, "exit-address-family", "end")

	_, err := infraprovider.Get().ExecExternalContainerCommand(frr, vtyshCommand(args...))
	if err != nil {
		return fmt.Errorf("failed to configure EVPN BGP: %w", err)
	}

	// No per-test BGP cleanup needed on the external FRR:
	// - advertise-all-vni: global setting, shared across all parallel tests
	// - neighbor <nodeIP> activate: all tests peer with the same cluster nodes
	// Both are baseline infrastructure set up by the KIND cluster install (kind.sh -rae)
	// and must persist for the duration of the test suite.
	// IP-VRF per-VRF BGP config is cleaned up by setupIPVRFBGPOnExternalFRR.
	ictx.AddCleanUpFn(func() error {
		framework.Logf("EVPN BGP cleanup complete on %s (no-op: global BGP settings are shared infrastructure)", externalFRRContainerName)
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
//   - asn: BGP ASN (e.g., 64512). Must match the cluster's frr-k8s ASN and the global EVPN
//     BGP instance. Used for both the VRF BGP instance and the Route Distinguisher/Route Target.
//   - vni: VXLAN Network Identifier for route-target (e.g., 20102)
//   - ipFamilies: IP families to configure (e.g., sets.New(utilnet.IPv4) or sets.New(utilnet.IPv4, utilnet.IPv6))
//   - subnets: Subnets to advertise via BGP (e.g., []string{"172.27.102.0/24"} or {"172.27.102.0/24", "fd00:102::/64"})
//
// Cleanup is automatically registered via ictx.AddCleanUpFn().
func setupIPVRFBGPOnExternalFRR(ictx infraapi.Context, vrfName string, asn, vni int, ipFamilies sets.Set[utilnet.IPFamily], subnets []string) error {
	frr := infraapi.ExternalContainer{Name: externalFRRContainerName}
	rt := fmt.Sprintf("%d:%d", asn, vni)

	// Build vtysh args
	args := []string{
		"configure terminal",
		fmt.Sprintf("vrf %s", vrfName), fmt.Sprintf("vni %d", vni), "exit-vrf",
		fmt.Sprintf("router bgp %d vrf %s", asn, vrfName),
	}

	// Configure address-families with explicit network statements
	// (Using explicit 'network' statements instead of 'redistribute connected' to align
	// with how OVN-K advertises routes via RouteAdvertisements/FRRConfiguration Prefixes)
	if ipFamilies.Has(utilnet.IPv4) {
		args = append(args, "address-family ipv4 unicast")
		for _, subnet := range subnets {
			if !utilnet.IsIPv6CIDRString(subnet) {
				args = append(args, fmt.Sprintf("network %s", subnet))
			}
		}
		args = append(args, "exit-address-family")
	}
	if ipFamilies.Has(utilnet.IPv6) {
		args = append(args, "address-family ipv6 unicast")
		for _, subnet := range subnets {
			if utilnet.IsIPv6CIDRString(subnet) {
				args = append(args, fmt.Sprintf("network %s", subnet))
			}
		}
		args = append(args, "exit-address-family")
	}

	// l2vpn evpn - configure RD, RT, and advertise unicast routes
	args = append(args, "address-family l2vpn evpn", fmt.Sprintf("rd %s", rt), fmt.Sprintf("route-target import %s", rt), fmt.Sprintf("route-target export %s", rt))
	if ipFamilies.Has(utilnet.IPv4) {
		args = append(args, "advertise ipv4 unicast")
	}
	if ipFamilies.Has(utilnet.IPv6) {
		args = append(args, "advertise ipv6 unicast")
	}
	args = append(args, "exit-address-family", "end")

	_, err := infraprovider.Get().ExecExternalContainerCommand(frr, vtyshCommand(args...))
	if err != nil {
		return fmt.Errorf("failed to configure IP-VRF BGP for %s: %w", vrfName, err)
	}

	// Register cleanup to remove BGP VRF instance and VRF-VNI binding
	// NOTE: This cleanup may run after setupIPVRFOnExternalFRR cleanup has already deleted
	// the Linux VRF device, which triggers FRR to auto-cleanup the VRF config. In that case,
	// these commands may fail - that's OK, we just log warnings and won't retry that.
	ictx.AddCleanUpFn(func() error {
		frr := infraapi.ExternalContainer{Name: externalFRRContainerName}

		_, err := infraprovider.Get().ExecExternalContainerCommand(frr, vtyshCommand(
			"configure terminal", fmt.Sprintf("vrf %s", vrfName), fmt.Sprintf("no vni %d", vni), "exit-vrf", "end",
		))
		if err != nil {
			framework.Logf("Warning: failed to remove VNI binding (may already be cleaned up): %v", err)
		}

		_, err = infraprovider.Get().ExecExternalContainerCommand(frr, vtyshCommand(
			"configure terminal", fmt.Sprintf("no router bgp %d vrf %s", asn, vrfName), "end",
		))
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

	framework.Logf("IP-VRF BGP setup complete on %s (VRF %s, ASN %d, VNI %d, RT %s, families %v)", externalFRRContainerName, vrfName, asn, vni, rt, ipFamilies.UnsortedList())
	return nil
}

// =============================================================================
// EVPN Agnhost Utilities
// =============================================================================

// evpnAgnhostInfo holds the discovered network information for an EVPN agnhost container.
type evpnAgnhostInfo struct {
	agnhostIPs       []string
	agnhostInterface string
	frrIPs           []string
	frrInterface     string
}

// createEVPNAgnhost creates a Docker network with the given subnets, creates an agnhost
// container on it, attaches FRR to it, and discovers the assigned IPs and interface names.
//
// This is the shared foundation for both MAC-VRF and IP-VRF agnhost setups.
// The caller is responsible for configuring FRR's interface (e.g., adding it to bridgeName
// as an access port for MAC-VRF, or putting it in a VRF for IP-VRF).
//
// Parameters:
//   - networkName: Name for the Docker network (e.g., "macvrf-net-100", "ipvrf-net-202")
//   - containerName: Name for the agnhost container (e.g., "agnhost-macvrf-100")
//   - ipFamilies: Cluster IP family support, used to filter discovered IPs
//   - subnets: Subnets for the Docker network (e.g., "10.100.0.0/16" for IPv4, or both for dual-stack)
//   - ipv4: Optional IPv4 address to request for the agnhost container (empty = let IPAM decide)
//   - ipv6: Optional IPv6 address to request for the agnhost container (empty = let IPAM decide)
func createEVPNAgnhost(ictx infraapi.Context, networkName, containerName string, ipFamilies sets.Set[utilnet.IPFamily], subnets []string, ipv4, ipv6 string) (*evpnAgnhostInfo, error) {
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
		IPv4:        ipv4,
		IPv6:        ipv6,
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

	// Step 4: Discover assigned IPs and interface names
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

	frrInterface := frrNetInf.InfName
	if frrInterface == "" {
		return nil, fmt.Errorf("FRR interface name not found for network %s", networkName)
	}

	agnhostInterface := agnhostNetInf.InfName
	if agnhostInterface == "" {
		return nil, fmt.Errorf("agnhost interface name not found for network %s", networkName)
	}

	// Collect IPs only for cluster-supported address families.
	// Docker may assign IPs for families we didn't request (e.g., default IPv4 on IPv6-only networks),
	// so we filter based on what the cluster actually supports.
	var agnhostIPs, frrIPs []string
	if ipFamilies.Has(utilnet.IPv4) {
		if agnhostNetInf.IPv4 != "" {
			agnhostIPs = append(agnhostIPs, agnhostNetInf.IPv4)
		}
		if frrNetInf.IPv4 != "" {
			frrIPs = append(frrIPs, frrNetInf.IPv4)
		}
	}
	if ipFamilies.Has(utilnet.IPv6) {
		if agnhostNetInf.IPv6 != "" {
			agnhostIPs = append(agnhostIPs, agnhostNetInf.IPv6)
		}
		if frrNetInf.IPv6 != "" {
			frrIPs = append(frrIPs, frrNetInf.IPv6)
		}
	}

	framework.Logf("EVPN agnhost created: %s (agnhost IPs: %v, FRR IPs: %v, interface: %s, FRR interface: %s)", containerName, agnhostIPs, frrIPs, agnhostInterface, frrInterface)
	return &evpnAgnhostInfo{
		agnhostIPs:       agnhostIPs,
		agnhostInterface: agnhostInterface,
		frrIPs:           frrIPs,
		frrInterface:     frrInterface,
	}, nil
}

// =============================================================================
// MAC-VRF Agnhost Utilities
// =============================================================================

// secondToLastIP returns the second-to-last usable IP in the given subnet.
// Using the high end of the range avoids collisions with both OVN IPAM
// (which allocates from lower end onwards) and Docker IPAM (which allocates from lower end onwards).
// This assumes OVN-K CUDN IPAM won't allocate IPs from the top of the subnet range
// for pods in these e2e tests.
// Example: "10.100.0.0/24" -> 10.100.0.253, "fd00:100::/64" -> fd00:100::ffff:ffff:ffff:fffe
func secondToLastIP(ipNet *net.IPNet) net.IP {
	// Compute broadcast: network OR inverted mask
	broadcast := make(net.IP, len(ipNet.IP))
	for i := range ipNet.IP {
		broadcast[i] = ipNet.IP[i] | ^ipNet.Mask[i]
	}
	// Subtract 2 from broadcast to get second-to-last usable IP
	result := make(net.IP, len(broadcast))
	copy(result, broadcast)
	borrow := byte(2)
	for i := len(result) - 1; i >= 0 && borrow > 0; i-- {
		diff := int(result[i]) - int(borrow)
		if diff < 0 {
			result[i] = byte(diff + 256)
			borrow = 1
		} else {
			result[i] = byte(diff)
			borrow = 0
		}
	}
	return result
}

// setupMACVRFAgnhost creates an agnhost container connected to the EVPN bridge
// for MAC-VRF (Layer 2) connectivity testing.
//
// This function:
//  1. Creates a Docker network with the CUDN subnet and an agnhost on it,
//     requesting the second-to-last IP of each subnet to avoid collisions
//     with OVN IPAM and Docker IPAM (both allocate from the low end)
//  2. Connects FRR to the network (Docker creates a veth pair automatically)
//  3. Moves FRR's interface to bridgeName as an access port for the MAC-VRF VLAN
//
// Requires: setupEVPNBridgeOnExternalFRR and setupMACVRFOnExternalFRR must be called first.
//
// The agnhost will be on the same L2 segment as pods on the CUDN, allowing
// direct Layer 2 communication via EVPN Type-2/Type-3 routes.
//
// Parameters:
//   - vid: VLAN ID for the access port on bridgeName (e.g., 100)
//   - ipFamilies: Cluster IP family support (e.g., sets.New(utilnet.IPv4, utilnet.IPv6))
//   - subnets: Subnets for the Docker network matching the CUDN (e.g., "10.100.0.0/16")
//
// Returns:
//   - Agnhost's IP addresses without prefix (e.g., ["10.100.0.253"] for a /24 subnet)
func setupMACVRFAgnhost(ictx infraapi.Context, vid int, bridgeName string, ipFamilies sets.Set[utilnet.IPFamily], subnets ...string) ([]string, error) {
	networkName := fmt.Sprintf("macvrf-net-%d", vid)
	containerName := fmt.Sprintf("agnhost-macvrf-%d", vid)

	// Compute the second-to-last IP of each subnet to request at container creation.
	// Using the high end of the range avoids collisions with both OVN IPAM and
	// Docker/Podman IPAM, which allocate from the low end.
	var ipv4, ipv6 string
	var agnhostIPs []string
	for _, subnet := range subnets {
		_, ipNet, err := net.ParseCIDR(subnet)
		if err != nil {
			return nil, fmt.Errorf("failed to parse subnet %s: %w", subnet, err)
		}
		newIP := secondToLastIP(ipNet)
		if utilnet.IsIPv6CIDRString(subnet) {
			ipv6 = newIP.String()
		} else {
			ipv4 = newIP.String()
		}
		agnhostIPs = append(agnhostIPs, newIP.String())
	}

	info, err := createEVPNAgnhost(ictx, networkName, containerName, ipFamilies, subnets, ipv4, ipv6)
	if err != nil {
		return nil, err
	}

	// Move FRR's interface to bridgeName and configure as access port
	frr := infraapi.ExternalContainer{Name: externalFRRContainerName}
	vidStr := fmt.Sprintf("%d", vid)
	frrCmds := [][]string{
		{"ip", "link", "set", info.frrInterface, "master", bridgeName},
		{"bridge", "vlan", "add", "dev", info.frrInterface, "vid", vidStr, "pvid", "untagged"},
	}
	for _, cmd := range frrCmds {
		if _, err = infraprovider.Get().ExecExternalContainerCommand(frr, cmd); err != nil {
			return nil, fmt.Errorf("failed to configure %s as %s access port for VID %s: %w", info.frrInterface, bridgeName, vidStr, err)
		}
	}

	// Cleanup is handled automatically:
	// - ictx.CreateNetwork() registers network deletion
	// - ictx.CreateExternalContainer() registers container deletion
	// - ictx.AttachNetwork() registers network detachment

	framework.Logf("MAC-VRF agnhost setup complete: %s (IPs: %v, VID: %d, FRR interface: %s)", containerName, agnhostIPs, vid, info.frrInterface)
	return agnhostIPs, nil
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
//   - ipFamilies: Cluster IP family support (e.g., sets.New(utilnet.IPv4, utilnet.IPv6))
//   - subnets: Subnets for the Docker network (e.g., "172.27.102.0/24" for IPv4, or both for dual-stack)
//
// Returns:
//   - Agnhost's IP addresses (IPv4 and/or IPv6 depending on cluster IP family support)
func setupIPVRFAgnhost(ictx infraapi.Context, vid int, vrfName string, ipFamilies sets.Set[utilnet.IPFamily], subnets ...string) ([]string, error) {
	networkName := fmt.Sprintf("ipvrf-net-%d", vid)
	containerName := fmt.Sprintf("agnhost-ipvrf-%d", vid)

	info, err := createEVPNAgnhost(ictx, networkName, containerName, ipFamilies, subnets, "", "")
	if err != nil {
		return nil, err
	}

	// Put FRR's interface in the VRF
	// NOTE: keep_addr_on_down=1 is set at FRR container startup (in deploy_frr_external_container)
	// to preserve IPv6 addresses during VRF assignment. See https://github.com/FRRouting/frr/issues/1666
	frr := infraapi.ExternalContainer{Name: externalFRRContainerName}

	frrCmds := [][]string{
		{"ip", "link", "set", info.frrInterface, "master", vrfName},
		{"ip", "link", "set", info.frrInterface, "up"},
	}
	for _, cmd := range frrCmds {
		if _, err = infraprovider.Get().ExecExternalContainerCommand(frr, cmd); err != nil {
			return nil, fmt.Errorf("failed to assign %s to VRF %s: %w", info.frrInterface, vrfName, err)
		}
	}

	// Set agnhost's default routes via FRR (for each address family)
	agnhost := infraapi.ExternalContainer{Name: containerName}

	var routeCmds [][]string
	for _, gwIP := range info.frrIPs {
		cmd := []string{"ip"}
		if utilnet.IsIPv6String(gwIP) {
			cmd = append(cmd, "-6")
		}
		cmd = append(cmd, "route", "replace", "default", "via", gwIP, "dev", info.agnhostInterface)
		routeCmds = append(routeCmds, cmd)
	}
	for _, cmd := range routeCmds {
		if _, err = infraprovider.Get().ExecExternalContainerCommand(agnhost, cmd); err != nil {
			return nil, fmt.Errorf("failed to set default routes on agnhost: %w", err)
		}
	}

	// Cleanup is handled automatically:
	// - ictx.CreateNetwork() registers network deletion
	// - ictx.CreateExternalContainer() registers container deletion
	// - ictx.AttachNetwork() registers network detachment

	framework.Logf("IP-VRF agnhost setup complete: %s (IPs: %v, network: %s, VRF: %s)", containerName, info.agnhostIPs, networkName, vrfName)
	return info.agnhostIPs, nil
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

	// Register cleanup: delete VTEP and wait until it's fully removed
	ictx.AddCleanUpFn(func() error {
		err := client.K8sV1().VTEPs().Delete(context.Background(), name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			_, err := client.K8sV1().VTEPs().Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		})
	})

	// TODO: Add status check once VTEP controller implements status conditions
	framework.Logf("VTEP created: %s (CIDRs: %v, Mode: %s)", name, cidrs, mode)
	return nil
}

// =============================================================================
// EVPN VID Utilities
// =============================================================================

// randomVID generates a random VLAN ID in the valid range (2-4094).
// VIDs 0, 1, and 4095 are reserved and should not be used.
func randomVID() int {
	return rand.Intn(4093) + 2 // 2-4094
}

// randomVNI generates a random VXLAN Network Identifier in the valid 24-bit range (1-16777215).
func randomVNI() int32 {
	return int32(rand.Intn(16777215) + 1)
}

// randomCUDNSubnets generates random non-overlapping CUDN subnets for parallel test isolation.
// Uses /20 (4096 addresses) instead of /16 to allow randomizing both second and third octets,
// giving ~4032 possible subnets within 10.0.0.0/8 while avoiding collisions with:
//   - 10.96.0.0/16  (Kubernetes services)
//   - 10.132.0.0/16 (UDN perf tests)
//   - 10.243.0.0/16, 10.244.0.0/16 (pod CIDRs)
//
// Note: /20 supports up to 16 nodes with /24 per-node subnets for Layer3 topology.
// This is sufficient for KIND e2e clusters.
//
// Returns IPv4 (/20) and IPv6 (/52) subnets.
func randomCUDNSubnets() (ipv4, ipv6 string) {
	// 4096 possible /20 subnets in 10.0.0.0/8 (256 second octets * 16 /20-aligned third octets)
	// Exclude blocks overlapping known /16 reservations (16 /20 blocks each):
	//   10.96, 10.132, 10.243, 10.244 = 64 excluded → ~4032 usable
	for {
		second := rand.Intn(256)
		// 16 /20-aligned slots per second octet (256/16)
		third := rand.Intn(16) * 16 // 0, 16, 32, ..., 240
		switch second {
		case 96, 132, 243, 244:
			continue
		}
		n := second*16 + third/16
		return fmt.Sprintf("10.%d.%d.0/20", second, third), fmt.Sprintf("fd00:%x::/52", n)
	}
}

// randomIPVRFAgnhostSubnets generates random IP-VRF agnhost subnets for parallel test isolation.
// Uses /29 (8 IPs, 6 usable) which is sufficient for provider gateway + agnhost + FRR,
// giving 8192 possible subnets within 172.27.0.0/16 to minimize collision probability.
// The 172.27.0.0/16 space avoids collisions with:
//   - 172.18.0.0/16 (KIND primary network)
//   - 172.19.0.0/16 (XGW network)
//   - 172.22.0.0/16 (MetalLB client network)
//   - 172.26.0.0/16 (BGP server network)
//
// Returns IPv4 (/29) and IPv6 (/112) subnets.
func randomIPVRFAgnhostSubnets() (ipv4, ipv6 string) {
	// 8192 possible /29 subnets in 172.27.0.0/16
	n := rand.Intn(8192)
	// 32 /29-aligned slots per third octet (256/8), so divide to get octet pair
	third := n / 32
	fourth := (n % 32) * 8
	return fmt.Sprintf("172.27.%d.%d/29", third, fourth), fmt.Sprintf("fd01:%x::/112", n)
}

// randomVTEPSubnets generates random VTEP subnets for parallel test isolation.
// Uses /24 (254 usable IPs)
// Randomizes both second and third octets within RFC 6598 shared address space
// (100.64.0.0/10), giving 15,872 possible /24 subnets while avoiding:
//   - 100.64.0.0/16 (default join subnet)
//   - 100.65.0.0/16 (UDN primary join subnet)
//
// 100.88.0.0/16 (transit subnet) is NOT excluded because transit IPs are purely
// internal to OVN's logical network and never appear on physical interfaces.
// Safe second octets: 66-127 (62 values).
// Returns IPv4 (/24) and IPv6 (/112) subnets.
func randomVTEPSubnets() (ipv4, ipv6 string) {
	second := rand.Intn(62) + 66 // 66-127
	third := rand.Intn(256)      // 0-255
	return fmt.Sprintf("100.%d.%d.0/24", second, third), fmt.Sprintf("fd02:%x%02x::/112", second, third)
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

// =============================================================================
// REVERT ME: Temporary Cluster-Side EVPN Setup
// =============================================================================
// This section provides a temporary workaround to enable EVPN E2E tests before
// OVN-Kubernetes natively implements EVPN. It downloads and executes a setup
// script from a remote GitHub repository.
//
// The script performs:
//   - Step 7: Setup EVPN Bridge on cluster nodes (br0/vxlan0)
//   - Step 8: Configure MAC-VRF + IP-VRF (OVS port, OVN LSP, VLAN/VNI, SVI)
//   - Step 9: Configure frr-k8s for EVPN BGP (vtysh commands)
//
// Remove this entire section once OVN-K EVPN implementation is complete.
// =============================================================================

const (
	// clusterEVPNSetupScriptURL is the URL to the cluster-side EVPN setup script.
	// REVERT ME: Remove this once OVN-K implements EVPN natively.
	clusterEVPNSetupScriptURL = "https://raw.githubusercontent.com/tssurya/evpn-scripts-on-ovnk-kind-with-existing-bgp-setup/0b1ab74/evpn-cluster-setup.sh"
)

// runClusterEVPNSetupScript executes the cluster-side EVPN setup script from remote URL.
// The script discovers and configures all cluster nodes automatically using kubectl.
// This is a temporary workaround until OVN-K implements EVPN natively.
//
// REVERT ME: Remove this function once OVN-K EVPN implementation is complete.
func runClusterEVPNSetupScript(ictx infraapi.Context,
	networkName, externalFRRIP string,
	bgpASN int,
	hasMACVRF bool, macVRFVNI, macVRFVID int,
	hasIPVRF bool, ipVRFVNI, ipVRFVID int,
	cudnSubnets []string) error {

	// Build environment variables - script handles node discovery via kubectl
	// All vars prefixed with EVPN_ to avoid conflicts with other env vars
	env := map[string]string{
		"EVPN_NETWORK_NAME":    networkName,
		"EVPN_EXTERNAL_FRR_IP": externalFRRIP,
		"EVPN_BGP_ASN":         fmt.Sprintf("%d", bgpASN),
		"EVPN_CUDN_SUBNETS":    strings.Join(cudnSubnets, ","),
		"EVPN_OVN_NAMESPACE":   deploymentconfig.Get().OVNKubernetesNamespace(),
		"EVPN_FRR_NAMESPACE":   deploymentconfig.Get().FRRK8sNamespace(),
	}
	if hasMACVRF {
		env["EVPN_MACVRF_VNI"] = fmt.Sprintf("%d", macVRFVNI)
		env["EVPN_MACVRF_VID"] = fmt.Sprintf("%d", macVRFVID)
	}
	if hasIPVRF {
		env["EVPN_IPVRF_VNI"] = fmt.Sprintf("%d", ipVRFVNI)
		env["EVPN_IPVRF_VID"] = fmt.Sprintf("%d", ipVRFVID)
	}

	// Register cleanup FIRST - ensures cleanup runs even if setup fails
	ictx.AddCleanUpFn(func() error {
		cleanupEnv := copyEnvMap(env)
		cleanupEnv["EVPN_CLEANUP"] = "true"

		framework.Logf("Running cluster EVPN cleanup script")
		if err := runRemoteScript(clusterEVPNSetupScriptURL, cleanupEnv); err != nil {
			// Log but don't fail - cleanup should be best-effort
			framework.Logf("WARNING: cleanup script had errors (may be expected): %v", err)
		}
		return nil
	})

	// Run setup script - it handles all nodes internally
	framework.Logf("Running cluster EVPN setup script")
	if err := runRemoteScript(clusterEVPNSetupScriptURL, env); err != nil {
		return fmt.Errorf("setup script failed: %w", err)
	}

	framework.Logf("Cluster-side EVPN setup complete")
	return nil
}

// runRemoteScript executes a bash script from a URL with the given environment variables.
// Uses: curl -sL $url | bash
func runRemoteScript(url string, env map[string]string) error {
	cmd := exec.Command("bash", "-c", fmt.Sprintf("curl -sL %s | bash", url))

	// Set environment variables
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	output, err := cmd.CombinedOutput()
	if len(output) > 0 {
		framework.Logf("Script output:\n%s", string(output))
	}
	if err != nil {
		return fmt.Errorf("script failed: %w\nOutput: %s", err, string(output))
	}
	return nil
}

// copyEnvMap creates a copy of an environment variable map.
func copyEnvMap(m map[string]string) map[string]string {
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

// =============================================================================
// EVPN Test Helpers
// =============================================================================

// =============================================================================
// EVPN Tests
// =============================================================================

var _ = ginkgo.Describe("EVPN", func() {
	const baseName = "evpn"
	f := wrappedTestFramework(baseName)
	f.SkipNamespaceCreation = true

	// Common test parameters
	const (
		// bgpASN is the general BGP ASN matching kind.sh's external FRR setup.
		// Used for: EVPN control plane, frr-k8s node peering, route-target calculation.
		bgpASN = 64512

		// Connectivity test parameters
		timeout = 15 * time.Second
	)
	netexecPortStr := fmt.Sprintf("%d", agnhostHTTPPort)

	var (
		ictx infraapi.Context

		// Test identifiers (set in BeforeEach with unique suffix)
		testBaseName string

		// External FRR IP (discovered in BeforeEach)
		externalFRRIP string

		// IP families supported by cluster (populated in BeforeEach)
		ipFamilies sets.Set[utilnet.IPFamily]

		// Subnets (dual-stack aware, populated in BeforeEach based on cluster IP family support)
		ipVRFAgnhostSubnets []string
		vtepSubnets         []string

		// Unique bridge and VXLAN device names per test for parallel isolation
		// (Linux interface names are limited to 15 characters)
		bridgeName string
		vxlanName  string

		// Node IPs for BGP neighbors (populated in BeforeEach)
		nodeIPs []string
	)

	ginkgo.BeforeEach(func() {
		if !isLocalGWModeEnabled() {
			e2eskipper.Skipf("EVPN test cases only supported in Local Gateway mode")
		}
		if !isIPv4Supported(f.ClientSet) {
			// FRR does not support IPv6 underlay for EVPN VXLAN tunnels.
			// See: https://github.com/FRRouting/frr/issues/5885
			e2eskipper.Skipf("EVPN test cases require IPv4 for VXLAN underlay (FRR limitation)")
		}
		// Initialize infrastructure context (cleanup is automatic via ginkgo.DeferCleanup)
		ictx = infraprovider.Get().NewTestContext()

		// Generate unique test name with suffix
		testBaseName = baseName + framework.RandomSuffix()

		// Derive unique bridge/vxlan names from testBaseName for parallel isolation.
		// e.g. testBaseName="evpn7a3f" → bridgeName="brevpn7a3f", vxlanName="vxevpn7a3f"
		// keeping worst-case sviName ("brevpn9999.4094") at exactly 15 chars (Linux limit).
		bridgeName = "br" + testBaseName
		vxlanName = "vx" + testBaseName

		// Discover cluster IP family support
		ipFamilies = sets.New(getSupportedIPFamiliesSlice(f.ClientSet)...)

		// Reset slices for each test
		ipVRFAgnhostSubnets = nil
		vtepSubnets = nil
		nodeIPs = nil

		// Generate random subnets for parallel test isolation
		ipVRFAgnhostIPv4, ipVRFAgnhostIPv6 := randomIPVRFAgnhostSubnets()
		vtepIPv4, vtepIPv6 := randomVTEPSubnets()

		// Configure subnets based on cluster IP family support
		if ipFamilies.Has(utilnet.IPv4) {
			ipVRFAgnhostSubnets = append(ipVRFAgnhostSubnets, ipVRFAgnhostIPv4)
			vtepSubnets = append(vtepSubnets, vtepIPv4)
		}
		if ipFamilies.Has(utilnet.IPv6) {
			ipVRFAgnhostSubnets = append(ipVRFAgnhostSubnets, ipVRFAgnhostIPv6)
			vtepSubnets = append(vtepSubnets, vtepIPv6)
		}

		// Discover external FRR IP
		kindNetwork, err := infraprovider.Get().GetNetwork("kind")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		frrNetIf, err := infraprovider.Get().GetExternalContainerNetworkInterface(
			infraapi.ExternalContainer{Name: externalFRRContainerName}, kindNetwork)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Use IPv4 or IPv6 based on cluster support (prefer IPv4 if both available)
		if isIPv4Supported(f.ClientSet) && frrNetIf.IPv4 != "" {
			externalFRRIP = frrNetIf.IPv4
		} else if isIPv6Supported(f.ClientSet) && frrNetIf.IPv6 != "" {
			externalFRRIP = frrNetIf.IPv6
		}
		gomega.Expect(externalFRRIP).NotTo(gomega.BeEmpty(), "External FRR must have an IP on kind network")
		framework.Logf("External FRR IP: %s", externalFRRIP)

		// Collect node IPs for BGP neighbors
		nodes, err := f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		for _, node := range nodes.Items {
			if ipFamilies.Has(utilnet.IPv4) {
				addrs := e2enode.GetAddressesByTypeAndFamily(&node, corev1.NodeInternalIP, corev1.IPv4Protocol)
				nodeIPs = append(nodeIPs, addrs...)
			}
			if ipFamilies.Has(utilnet.IPv6) {
				addrs := e2enode.GetAddressesByTypeAndFamily(&node, corev1.NodeInternalIP, corev1.IPv6Protocol)
				nodeIPs = append(nodeIPs, addrs...)
			}
		}
		framework.Logf("Node IPs for BGP: %v", nodeIPs)
	})

	// Helper to test connectivity from pod to external server (handles both IPv4 and IPv6)
	testPodToServers := func(pod *corev1.Pod, serverIPs []string) {
		ginkgo.GinkgoHelper()
		for _, serverIP := range serverIPs {
			// Strip prefix length if present (e.g., "10.100.0.250/16" -> "10.100.0.250")
			ip := strings.Split(serverIP, "/")[0]
			url := fmt.Sprintf("http://%s/hostname", net.JoinHostPort(ip, netexecPortStr))
			framework.Logf("Testing connectivity to %s", url)
			_, err := e2epodoutput.RunHostCmdWithRetries(
				pod.Namespace,
				pod.Name,
				fmt.Sprintf("curl --max-time 1 -g -q -s %s", url),
				framework.Poll,
				timeout,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Failed to connect to %s", serverIP)
		}
	}

	// setupEVPNInfrastructure sets up the complete EVPN infrastructure including:
	// - FRR bridge and VRF configurations
	// - MAC-VRF and IP-VRF with agnhost containers
	// - EVPN BGP peering
	// - VTEP CR, FRRConfiguration, and cluster-side EVPN setup
	// Returns the MAC-VRF and IP-VRF agnhost IPs for connectivity testing.
	setupEVPNInfrastructure := func(networkSpec *udnv1.NetworkSpec) (macVRFAgnhostIPs, ipVRFAgnhostIPs []string) {
		ginkgo.GinkgoHelper()
		// Derive what to setup from networkSpec
		hasMACVRF := networkSpec.EVPN != nil && networkSpec.EVPN.MACVRF != nil
		hasIPVRF := networkSpec.EVPN != nil && networkSpec.EVPN.IPVRF != nil

		// Filter networkSpec subnets based on cluster IP family support
		if networkSpec.Layer2 != nil {
			networkSpec.Layer2.Subnets = filterDualStackCIDRs(f.ClientSet, networkSpec.Layer2.Subnets)
		}
		if networkSpec.Layer3 != nil {
			networkSpec.Layer3.Subnets = filterL3Subnets(f.ClientSet, networkSpec.Layer3.Subnets)
		}

		// Extract subnets from networkSpec for MAC-VRF agnhost IP derivation
		var cudnSubnetsFromSpec []string
		if networkSpec.Layer2 != nil {
			for _, cidr := range networkSpec.Layer2.Subnets {
				cudnSubnetsFromSpec = append(cudnSubnetsFromSpec, string(cidr))
			}
		} else if networkSpec.Layer3 != nil {
			for _, subnet := range networkSpec.Layer3.Subnets {
				cudnSubnetsFromSpec = append(cudnSubnetsFromSpec, string(subnet.CIDR))
			}
		}

		ginkgo.By("Setting up EVPN bridge on external FRR")
		gomega.Expect(setupEVPNBridgeOnExternalFRR(ictx, externalFRRIP, bridgeName, vxlanName)).To(gomega.Succeed())

		if hasMACVRF {
			// Generate random VIDs for external FRR side.
			// VID is local to each side - only VNI needs to match for EVPN connectivity.
			frrMACVRFVID := randomVID()
			framework.Logf("Generated random VIDs for external FRR: MAC-VRF VID=%d", frrMACVRFVID)
			ginkgo.By("Setting up MAC-VRF on external FRR")
			gomega.Expect(setupMACVRFOnExternalFRR(int(networkSpec.EVPN.MACVRF.VNI), frrMACVRFVID, bridgeName, vxlanName)).To(gomega.Succeed())

			ginkgo.By("Creating MAC-VRF agnhost")
			var err error
			macVRFAgnhostIPs, err = setupMACVRFAgnhost(ictx, frrMACVRFVID, bridgeName, ipFamilies, cudnSubnetsFromSpec...)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(macVRFAgnhostIPs).NotTo(gomega.BeEmpty())
		}
		ginkgo.By("Setting up EVPN BGP on external FRR")
		gomega.Expect(setupEVPNBGPOnExternalFRR(ictx, bgpASN, nodeIPs)).To(gomega.Succeed())

		if hasIPVRF {
			// Derive VRF name from VNI (unique per IP-VRF)
			ipVRFName := fmt.Sprintf("vrf%d", networkSpec.EVPN.IPVRF.VNI)
			frrIPVRFVID := randomVID()
			framework.Logf("Generated random VIDs for external FRR: IP-VRF VID=%d", frrIPVRFVID)
			ginkgo.By("Setting up IP-VRF on external FRR")
			gomega.Expect(setupIPVRFOnExternalFRR(ictx, ipVRFName, int(networkSpec.EVPN.IPVRF.VNI), frrIPVRFVID, bridgeName, vxlanName)).To(gomega.Succeed())
			ginkgo.By("Creating IP-VRF agnhost")
			var err error
			ipVRFAgnhostIPs, err = setupIPVRFAgnhost(ictx, frrIPVRFVID, ipVRFName, ipFamilies, ipVRFAgnhostSubnets...)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(ipVRFAgnhostIPs).NotTo(gomega.BeEmpty())
			// Configure BGP AFTER agnhost so FRR's interface is in the VRF
			// and has a connected route for the subnet we want to advertise
			ginkgo.By("Setting up IP-VRF BGP on external FRR")
			gomega.Expect(setupIPVRFBGPOnExternalFRR(ictx, ipVRFName, bgpASN, int(networkSpec.EVPN.IPVRF.VNI), ipFamilies, ipVRFAgnhostSubnets)).To(gomega.Succeed())
		}

		ginkgo.By("Creating VTEP CR")
		testVTEPName := testBaseName + "-vtep"
		err := createVTEP(f, ictx, testVTEPName, vtepSubnets, vtepv1.VTEPModeManaged)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Update VTEP name in network spec
		networkSpec.EVPN.VTEP = testVTEPName

		ginkgo.By("Creating FRRConfiguration for EVPN")
		frrConfigLabels := map[string]string{"network": testBaseName}
		err = createFRRConfiguration(ictx, testBaseName, deploymentconfig.Get().FRRK8sNamespace(), bgpASN, externalFRRIP, frrConfigLabels)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Creating namespace, CUDN, and RouteAdvertisements")
		testNamespace, err := createNamespaceWithPrimaryNetworkOfType(f, ictx, baseName, testBaseName, cudnAdvertisedEVPN, networkSpec)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		f.Namespace = testNamespace

		// REVERT ME: Temporary cluster-side EVPN setup until OVN-K implements it natively
		// Generate random VIDs for cluster-side EVPN setup (OVN-K side).
		// VID is local to each side - these don't need to match the FRR side VIDs.
		clusterMACVRFVID := randomVID()
		clusterIPVRFVID := randomVID()
		framework.Logf("Generated random VIDs for cluster side: MAC-VRF VID=%d, IP-VRF VID=%d", clusterMACVRFVID, clusterIPVRFVID)
		ginkgo.By("Running cluster-side EVPN setup script (REVERT ME)")
		var macVRFVNI, ipVRFVNI int
		if hasMACVRF {
			macVRFVNI = int(networkSpec.EVPN.MACVRF.VNI)
		}
		if hasIPVRF {
			ipVRFVNI = int(networkSpec.EVPN.IPVRF.VNI)
		}
		err = runClusterEVPNSetupScript(ictx,
			testBaseName, externalFRRIP, bgpASN,
			hasMACVRF, macVRFVNI, clusterMACVRFVID,
			hasIPVRF, ipVRFVNI, clusterIPVRFVID,
			cudnSubnetsFromSpec)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		return macVRFAgnhostIPs, ipVRFAgnhostIPs
	}

	// Note: Cleanup is automatic via ginkgo.DeferCleanup registered in NewTestContext()
	// All functions using ictx.AddCleanUpFn() will be cleaned up in LIFO order

	ginkgo.DescribeTable("EVPN connectivity", feature.RouteAdvertisements, feature.EVPN,
		func(networkSpec *udnv1.NetworkSpec) {
			macVRFAgnhostIPs, ipVRFAgnhostIPs := setupEVPNInfrastructure(networkSpec)

			ginkgo.By("Creating test pod on CUDN")
			testPod := e2epod.CreateExecPodOrFail(
				context.Background(),
				f.ClientSet,
				f.Namespace.Name,
				f.Namespace.Name+"-netexec-pod",
				func(p *corev1.Pod) {
					p.Spec.Containers[0].Args = []string{"netexec"}
				},
			)

			// Test connectivity to external servers
			hasMACVRF := networkSpec.EVPN != nil && networkSpec.EVPN.MACVRF != nil
			hasIPVRF := networkSpec.EVPN != nil && networkSpec.EVPN.IPVRF != nil
			if hasMACVRF {
				framework.Logf("Testing L2 connectivity to MAC-VRF agnhost IPs: %v", macVRFAgnhostIPs)
				testPodToServers(testPod, macVRFAgnhostIPs)
			}
			if hasIPVRF {
				framework.Logf("Testing L3 connectivity to IP-VRF agnhost IPs: %v", ipVRFAgnhostIPs)
				testPodToServers(testPod, ipVRFAgnhostIPs)
			}
		},
		// Layer2 with MAC-VRF only - L2 broadcast domain extended via EVPN
		ginkgo.Entry("Layer2 network with MAC-VRF", func() *udnv1.NetworkSpec {
			cudnIPv4, cudnIPv6 := randomCUDNSubnets()
			return &udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRolePrimary,
					Subnets: udnv1.DualStackCIDRs{udnv1.CIDR(cudnIPv4), udnv1.CIDR(cudnIPv6)},
				},
				Transport: udnv1.TransportOptionEVPN,
				EVPN: &udnv1.EVPNConfig{
					MACVRF: &udnv1.VRFConfig{
						VNI: randomVNI(),
					},
				},
			}
		}()),
		// Layer2 with MAC-VRF + IP-VRF - L2 domain with L3 routing to external
		ginkgo.Entry("Layer2 network with MAC-VRF and IP-VRF", func() *udnv1.NetworkSpec {
			cudnIPv4, cudnIPv6 := randomCUDNSubnets()
			return &udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRolePrimary,
					Subnets: udnv1.DualStackCIDRs{udnv1.CIDR(cudnIPv4), udnv1.CIDR(cudnIPv6)},
				},
				Transport: udnv1.TransportOptionEVPN,
				EVPN: &udnv1.EVPNConfig{
					MACVRF: &udnv1.VRFConfig{
						VNI: randomVNI(),
					},
					IPVRF: &udnv1.VRFConfig{
						VNI: randomVNI(),
					},
				},
			}
		}()),
		// Layer3 with IP-VRF only - L3 routing via EVPN Type-5 routes
		ginkgo.Entry("Layer3 network with IP-VRF", func() *udnv1.NetworkSpec {
			cudnIPv4, cudnIPv6 := randomCUDNSubnets()
			return &udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role: udnv1.NetworkRolePrimary,
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: udnv1.CIDR(cudnIPv4)},
						{CIDR: udnv1.CIDR(cudnIPv6)},
					},
				},
				Transport: udnv1.TransportOptionEVPN,
				EVPN: &udnv1.EVPNConfig{
					IPVRF: &udnv1.VRFConfig{
						VNI: randomVNI(),
					},
				},
			}
		}()),
	)
	ginkgo.Context("with multicast feature enabled for namespace", func() {
		var (
			clientNodeInfo, serverNodeInfo nodeInfo
		)
		ginkgo.BeforeEach(func() {

			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
			framework.ExpectNoError(err)
			if len(nodes.Items) < 2 {
				e2eskipper.Skipf(
					"Test requires >= 2 Ready nodes, but there are only %v nodes",
					len(nodes.Items))
			}

			ips := e2enode.CollectAddresses(nodes, corev1.NodeInternalIP)

			clientNodeInfo = nodeInfo{
				name:   nodes.Items[0].Name,
				nodeIP: ips[0],
			}

			serverNodeInfo = nodeInfo{
				name:   nodes.Items[1].Name,
				nodeIP: ips[1],
			}
		})
		ginkgo.DescribeTable("send multicast UDP traffic between nodes and receive multicast IGMP query on", feature.RouteAdvertisements, feature.EVPN,
			func(networkSpec *udnv1.NetworkSpec) {
				setupEVPNInfrastructure(networkSpec)

				ginkgo.By("Enabling multicast for namespace")
				enableMulticastForNamespace(f)

				ginkgo.By("send multicast UDP traffic between nodes")
				isLayer3 := networkSpec.Topology == udnv1.NetworkTopologyLayer3
				err := testMulticastUDPTraffic(f, clientNodeInfo, serverNodeInfo, udnPodInterface)
				if isLayer3 {
					gomega.Expect(err).To(gomega.HaveOccurred(), fmt.Sprintf("multicast UDP traffic should fail on %s network", networkSpec.Topology))
				} else {
					gomega.Expect(err).NotTo(gomega.HaveOccurred(), fmt.Sprintf("multicast UDP traffic should work on %s network", networkSpec.Topology))
				}

				ginkgo.By("receive multicast IGMP query")
				if isLayer3 {
					err = testMulticastIGMPQuery(f, clientNodeInfo, serverNodeInfo)
					gomega.Expect(err).To(gomega.HaveOccurred(), fmt.Sprintf("multicast IGMP query should fail on %s network", networkSpec.Topology))
				} else {
					// TODO: this test is broken, see https://github.com/ovn-kubernetes/ovn-kubernetes/issues/5309
					//err = testMulticastIGMPQuery(f, clientNodeInfo, serverNodeInfo)
					//gomega.Expect(err).NotTo(gomega.HaveOccurred(), fmt.Sprintf("multicast IGMP query should work on %s EVPN network", networkSpec.Topology))
				}
			},
			// Layer2 with MAC-VRF only - L2 broadcast domain extended via EVPN
			ginkgo.Entry("Layer2 network with MAC-VRF", func() *udnv1.NetworkSpec {
				cudnIPv4, cudnIPv6 := randomCUDNSubnets()
				return &udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer2,
					Layer2: &udnv1.Layer2Config{
						Role:    udnv1.NetworkRolePrimary,
						Subnets: udnv1.DualStackCIDRs{udnv1.CIDR(cudnIPv4), udnv1.CIDR(cudnIPv6)},
					},
					Transport: udnv1.TransportOptionEVPN,
					EVPN: &udnv1.EVPNConfig{
						MACVRF: &udnv1.VRFConfig{
							VNI: randomVNI(),
						},
					},
				}
			}()),
			// Layer2 with MAC-VRF + IP-VRF - L2 domain with L3 routing to external
			ginkgo.Entry("Layer2 network with MAC-VRF and IP-VRF", func() *udnv1.NetworkSpec {
				cudnIPv4, cudnIPv6 := randomCUDNSubnets()
				return &udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer2,
					Layer2: &udnv1.Layer2Config{
						Role:    udnv1.NetworkRolePrimary,
						Subnets: udnv1.DualStackCIDRs{udnv1.CIDR(cudnIPv4), udnv1.CIDR(cudnIPv6)},
					},
					Transport: udnv1.TransportOptionEVPN,
					EVPN: &udnv1.EVPNConfig{
						MACVRF: &udnv1.VRFConfig{
							VNI: randomVNI(),
						},
						IPVRF: &udnv1.VRFConfig{
							VNI: randomVNI(),
						},
					},
				}
			}()),
			// Layer3 with IP-VRF only - L3 routing via EVPN Type-5 routes
			ginkgo.Entry("Layer3 network with IP-VRF", func() *udnv1.NetworkSpec {
				cudnIPv4, cudnIPv6 := randomCUDNSubnets()
				return &udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer3,
					Layer3: &udnv1.Layer3Config{
						Role: udnv1.NetworkRolePrimary,
						Subnets: []udnv1.Layer3Subnet{
							{CIDR: udnv1.CIDR(cudnIPv4)},
							{CIDR: udnv1.CIDR(cudnIPv6)},
						},
					},
					Transport: udnv1.TransportOptionEVPN,
					EVPN: &udnv1.EVPNConfig{
						IPVRF: &udnv1.VRFConfig{
							VNI: randomVNI(),
						},
					},
				}
			}()),
		)
	})
})
