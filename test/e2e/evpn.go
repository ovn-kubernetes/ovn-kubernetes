package e2e

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	udnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	vtepv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
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

// EVPNServerConfig holds configuration for an external server in EVPN tests.
type EVPNServerConfig struct {
	EVPNVRFConfig
	// ServerIP is the IP address with prefix for the server, e.g., "10.0.0.100/24"
	ServerIP string
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

// createEVPNMACVRFServer creates an agnhost server container and connects it to the
// external FRR's EVPN bridge as an access port for the specified MAC-VRF.
//
// The function:
// 1. Creates a docker network for the agnhost-FRR connection
// 2. Creates the agnhost container on that network
// 3. Attaches FRR to the network
// 4. Moves FRR's new interface to br0 as an access port with the MAC-VRF's VID
// 5. Configures the agnhost with the specified IP address
//
// Returns the created server container.
func createEVPNMACVRFServer(
	ictx infraapi.Context,
	frr infraapi.ExternalContainer,
	serverName string,
	serverIP string, // IP address with prefix, e.g., "10.0.0.100/24"
	vrfCfg EVPNVRFConfig,
) (infraapi.ExternalContainer, error) {
	provider := infraprovider.Get()

	// Create a network for the agnhost-FRR connection
	// This network is just used to establish the veth pair between containers
	networkName := serverName + "-net"
	network, err := ictx.CreateNetwork(networkName)
	if err != nil {
		return infraapi.ExternalContainer{}, fmt.Errorf("failed to create network %s: %w", networkName, err)
	}

	// Create agnhost server on the network
	server := infraapi.ExternalContainer{
		Name:    serverName,
		Image:   images.AgnHost(),
		CmdArgs: []string{"netexec", "--http-port=8080"},
		Network: network,
	}
	server, err = ictx.CreateExternalContainer(server)
	if err != nil {
		return infraapi.ExternalContainer{}, fmt.Errorf("failed to create server container %s: %w", serverName, err)
	}

	// Attach FRR to the same network to create the connection
	frrIface, err := ictx.AttachNetwork(network, frr.Name)
	if err != nil {
		return infraapi.ExternalContainer{}, fmt.Errorf("failed to attach FRR to network %s: %w", networkName, err)
	}

	// Get the interface name inside FRR for this network
	// The interface name is returned by AttachNetwork
	frrIfaceName := frrIface.InfName
	if frrIfaceName == "" {
		// If not provided, we need to find the interface by looking for the newest eth interface
		out, err := provider.ExecExternalContainerCommand(frr, []string{
			"sh", "-c", "ip -j link show | jq -r '.[].ifname' | grep eth | sort -t 'h' -k2 -n | tail -1",
		})
		if err != nil {
			return infraapi.ExternalContainer{}, fmt.Errorf("failed to find FRR interface for network %s: %w", networkName, err)
		}
		frrIfaceName = strings.TrimSpace(out)
	}

	// Attach the FRR interface to the EVPN bridge as an access port
	err = attachInterfaceToEVPNBridge(frr, frrIfaceName, vrfCfg.VID)
	if err != nil {
		return infraapi.ExternalContainer{}, fmt.Errorf("failed to attach interface to EVPN bridge: %w", err)
	}

	// Configure the agnhost server IP
	// First, clear any auto-assigned IP from docker
	_, err = provider.ExecExternalContainerCommand(server, []string{
		"ip", "addr", "flush", "dev", "eth0",
	})
	if err != nil {
		return infraapi.ExternalContainer{}, fmt.Errorf("failed to flush server IP: %w", err)
	}

	// Set the desired IP address
	_, err = provider.ExecExternalContainerCommand(server, []string{
		"ip", "addr", "add", serverIP, "dev", "eth0",
	})
	if err != nil {
		return infraapi.ExternalContainer{}, fmt.Errorf("failed to set server IP %s: %w", serverIP, err)
	}

	// Bring interface back up (in case flush brought it down)
	_, err = provider.ExecExternalContainerCommand(server, []string{
		"ip", "link", "set", "eth0", "up",
	})
	if err != nil {
		return infraapi.ExternalContainer{}, fmt.Errorf("failed to bring up server interface: %w", err)
	}

	return server, nil
}

// createEVPNIPVRFServer creates an agnhost server container and connects it to
// the external FRR's IP-VRF.
//
// The function:
// 1. Creates a docker network for the agnhost-FRR connection
// 2. Creates the agnhost container on that network
// 3. Attaches FRR to the network
// 4. Moves FRR's new interface to the IP-VRF
// 5. Configures the agnhost with the specified IP address
//
// Returns the created server container.
func createEVPNIPVRFServer(
	ictx infraapi.Context,
	frr infraapi.ExternalContainer,
	serverName string,
	serverIP string, // IP address with prefix, e.g., "10.0.0.100/24"
	vrfCfg EVPNVRFConfig,
) (infraapi.ExternalContainer, error) {
	provider := infraprovider.Get()

	// Create a network for the agnhost-FRR connection
	// This network is just used to establish the veth pair between containers
	networkName := serverName + "-net"
	network, err := ictx.CreateNetwork(networkName)
	if err != nil {
		return infraapi.ExternalContainer{}, fmt.Errorf("failed to create network %s: %w", networkName, err)
	}

	// Create agnhost server on the network
	server := infraapi.ExternalContainer{
		Name:    serverName,
		Image:   images.AgnHost(),
		CmdArgs: []string{"netexec", "--http-port=8080"},
		Network: network,
	}
	server, err = ictx.CreateExternalContainer(server)
	if err != nil {
		return infraapi.ExternalContainer{}, fmt.Errorf("failed to create server container %s: %w", serverName, err)
	}

	// Attach FRR to the same network to create the connection
	frrIface, err := ictx.AttachNetwork(network, frr.Name)
	if err != nil {
		return infraapi.ExternalContainer{}, fmt.Errorf("failed to attach FRR to network %s: %w", networkName, err)
	}

	// Get the interface name inside FRR for this network
	frrIfaceName := frrIface.InfName
	if frrIfaceName == "" {
		// If not provided, we need to find the interface by looking for the newest eth interface
		out, err := provider.ExecExternalContainerCommand(frr, []string{
			"sh", "-c", "ip -j link show | jq -r '.[].ifname' | grep eth | sort -t 'h' -k2 -n | tail -1",
		})
		if err != nil {
			return infraapi.ExternalContainer{}, fmt.Errorf("failed to find FRR interface for network %s: %w", networkName, err)
		}
		frrIfaceName = strings.TrimSpace(out)
	}

	// Attach the FRR interface to the IP-VRF
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"ip", "link", "set", frrIfaceName, "master", vrfCfg.Name,
	})
	if err != nil {
		return infraapi.ExternalContainer{}, fmt.Errorf("failed to attach %s to VRF %s: %w", frrIfaceName, vrfCfg.Name, err)
	}

	// Bring interface up
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"ip", "link", "set", frrIfaceName, "up",
	})
	if err != nil {
		return infraapi.ExternalContainer{}, fmt.Errorf("failed to bring up %s: %w", frrIfaceName, err)
	}

	// Configure the agnhost server IP
	// First, clear any auto-assigned IP from docker
	_, err = provider.ExecExternalContainerCommand(server, []string{
		"ip", "addr", "flush", "dev", "eth0",
	})
	if err != nil {
		return infraapi.ExternalContainer{}, fmt.Errorf("failed to flush server IP: %w", err)
	}

	// Set the desired IP address
	_, err = provider.ExecExternalContainerCommand(server, []string{
		"ip", "addr", "add", serverIP, "dev", "eth0",
	})
	if err != nil {
		return infraapi.ExternalContainer{}, fmt.Errorf("failed to set server IP %s: %w", serverIP, err)
	}

	// Bring interface back up (in case flush brought it down)
	_, err = provider.ExecExternalContainerCommand(server, []string{
		"ip", "link", "set", "eth0", "up",
	})
	if err != nil {
		return infraapi.ExternalContainer{}, fmt.Errorf("failed to bring up server interface: %w", err)
	}

	return server, nil
}

// extendExistingFRRWithEVPN extends an existing FRR container (from BGP tests) with EVPN support.
// This adds br0/vxlan0 and configures l2vpn evpn address-family via vtysh.
//
// This function is useful when the FRR container already exists from BGP RouteAdvertisements
// tests and we want to add EVPN functionality on top.
//
// Parameters:
//   - frr: Reference to the existing FRR container
//   - localIP: The FRR container's IP to use as local VTEP endpoint
//
// Returns error if setup fails.
func extendExistingFRRWithEVPN(frr infraapi.ExternalContainer, localIP string) error {
	provider := infraprovider.Get()

	// Check if br0 already exists
	out, err := provider.ExecExternalContainerCommand(frr, []string{
		"ip", "link", "show", "br0",
	})
	if err != nil || !strings.Contains(out, "br0") {
		// br0 doesn't exist, set it up
		err = setupExternalFRRWithEVPNBridge(frr, localIP)
		if err != nil {
			return fmt.Errorf("failed to setup EVPN bridge: %w", err)
		}
	}

	// Add l2vpn evpn address-family via vtysh
	// This is idempotent - if already configured, it won't fail
	_, err = provider.ExecExternalContainerCommand(frr, []string{
		"vtysh", "-c", "configure terminal",
		"-c", "router bgp 64512",
		"-c", "address-family l2vpn evpn",
		"-c", "advertise-all-vni",
		"-c", "end",
	})
	if err != nil {
		return fmt.Errorf("failed to configure l2vpn evpn: %w", err)
	}

	return nil
}

// deriveGatewayIP derives a gateway IP from a server IP by using .1 as the last octet.
// For example, "10.0.0.100/24" becomes "10.0.0.1/24".
func deriveGatewayIP(serverIP string) string {
	// Split IP and prefix
	parts := strings.Split(serverIP, "/")
	if len(parts) != 2 {
		return ""
	}
	ip := parts[0]
	prefix := parts[1]

	// Split IP into octets
	octets := strings.Split(ip, ".")
	if len(octets) != 4 {
		return ""
	}

	// Replace last octet with 1
	octets[3] = "1"
	return strings.Join(octets, ".") + "/" + prefix
}

// createVTEP creates a VTEP (VXLAN Tunnel Endpoint) Custom Resource for EVPN.
// The VTEP CR configures VTEP IP allocation for EVPN on cluster nodes.
//
// Parameters:
//   - f: Test framework
//   - ictx: Infrastructure context for cleanup registration
//   - name: Name of the VTEP CR
//   - cidrs: CIDR ranges for VTEP IP allocation (1 for single-stack, 2 for dual-stack)
func createVTEP(
	f *framework.Framework,
	ictx infraapi.Context,
	name string,
	cidrs []string,
) error {
	vtepClient, err := vtepclientset.NewForConfig(f.ClientConfig())
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
			Mode:  vtepv1.VTEPModeManaged,
		},
	}

	_, err = vtepClient.K8sV1().VTEPs().Create(context.Background(), vtep, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create VTEP %s: %w", name, err)
	}

	ictx.AddCleanUpFn(func() error {
		return vtepClient.K8sV1().VTEPs().Delete(context.Background(), name, metav1.DeleteOptions{})
	})

	return nil
}

// EVPN Test Infrastructure:
//
//	+---------------------------------------+                 +------------------+
//	|           KIND Cluster                |  KIND Primary   | External FRR     |
//	|                                       |  Network        | (reused from BGP)|
//	| +-------------+ +-------------+       |  (BGP/EVPN)     |                  |
//	| | Node 1      | | Node 2      |       |<--------------->| eth0 (primary)   |
//	| | - FRR-K8s   | | - FRR-K8s   |       |                 | br0  (EVPN)      |
//	| | - OVN VTEP  | | - OVN VTEP  |       |                 | vxlan0 (VTEP)    |
//	| +-------------+ +-------------+       |                 +------------------+
//	|                                       |                        |
//	| +-------------+  +----------------+   |        +------------------+------------------+
//	| | Node 3      |  | Pod (on CUDN)  |   |        | <serverName>-net   | <serverName>-net
//	| | - FRR-K8s   |  | (on any node)  |   |        v                    v
//	| | - OVN VTEP  |  +----------------+   |    +------------------+  +------------------+
//	| +-------------+                       |    | agnhost-macvrf   |  | agnhost-ipvrf    |
//	+---------------------------------------+    | (L2 via br0      |  | (L3 via VRF)     |
//	                                             |  access port)    |  |                  |
//	                                             +------------------+  +------------------+

// EVPN e2e tests
var _ = ginkgo.Describe("EVPN: Pod connectivity to external servers via EVPN", func() {
	const (
		timeout        = 240 * time.Second
		netexecPort    = 8080
		vtepSubnetIPv4 = "100.64.0.0/24"
	)
	var netexecPortStr = fmt.Sprintf("%d", netexecPort)

	f := wrappedTestFramework("evpn")

	var ictx infraapi.Context
	var testBaseName string

	ginkgo.BeforeEach(func() {
		if !isLocalGWModeEnabled() {
			e2eskipper.Skipf("EVPN test cases only supported in Local Gateway mode")
		}
		ictx = infraprovider.Get().NewTestContext()
		testBaseName = "evpn-" + framework.RandomSuffix()
	})

	ginkgo.AfterEach(func() {
		// Cleanup is handled by ictx
	})

	// Helper to test connectivity from pod to external server
	testPodToServer := func(pod *corev1.Pod, serverIP string) {
		ginkgo.GinkgoHelper()
		url := fmt.Sprintf("http://%s/hostname", net.JoinHostPort(serverIP, netexecPortStr))
		_, err := e2epodoutput.RunHostCmdWithRetries(
			pod.Namespace,
			pod.Name,
			fmt.Sprintf("curl --max-time 5 -g -q -s %s", url),
			framework.Poll,
			timeout,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	ginkgo.DescribeTable("EVPN connectivity",
		func(networkSpec *udnv1.NetworkSpec, macvrfCfg *EVPNServerConfig, ipvrfCfg *EVPNServerConfig) {
			// Use the existing FRR container from BGP RouteAdvertisements tests
			// This container is already connected to the KIND primary network
			const existingFRRName = "frr"

			ginkgo.By("Getting reference to existing FRR container")
			providerPrimaryNetwork, err := infraprovider.Get().PrimaryNetwork()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			frr := infraapi.ExternalContainer{Name: existingFRRName}
			frrNetIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(frr, providerPrimaryNetwork)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			frrIP := frrNetIface.IPv4
			framework.Logf("Using existing FRR container %s with IP %s", existingFRRName, frrIP)

			ginkgo.By("Extending existing FRR with EVPN support (br0/vxlan0 + l2vpn evpn)")
			err = extendExistingFRRWithEVPN(frr, frrIP)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Configure MAC-VRF and create external server if specified
			if macvrfCfg != nil {
				ginkgo.By("Configuring MAC-VRF on FRR")
				err = configureExternalFRRMACVRF(frr, macvrfCfg.EVPNVRFConfig)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("Creating MAC-VRF agnhost server")
				serverName := fmt.Sprintf("agnhost-%s-macvrf", testBaseName)
				_, err = createEVPNMACVRFServer(ictx, frr, serverName, macvrfCfg.ServerIP, macvrfCfg.EVPNVRFConfig)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}

			// Configure IP-VRF and create external server if specified
			if ipvrfCfg != nil {
				ginkgo.By("Configuring IP-VRF on FRR")
				gatewayIP := deriveGatewayIP(ipvrfCfg.ServerIP)
				err = configureExternalFRRIPVRF(frr, ipvrfCfg.EVPNVRFConfig, gatewayIP)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Add IP-VRF config to FRR via vtysh
				ginkgo.By("Adding IP-VRF BGP configuration")
				err = addIPVRFBGPConfig(frr, ipvrfCfg.EVPNVRFConfig)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("Creating IP-VRF agnhost server")
				serverName := fmt.Sprintf("agnhost-%s-ipvrf", testBaseName)
				_, err = createEVPNIPVRFServer(ictx, frr, serverName, ipvrfCfg.ServerIP, ipvrfCfg.EVPNVRFConfig)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}

			ginkgo.By("Creating VTEP CR")
			vtepName := testBaseName + "-vtep"
			err = createVTEP(f, ictx, vtepName, []string{vtepSubnetIPv4})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Update VTEP name in network spec
			networkSpec.EVPN.VTEP = vtepName

			ginkgo.By("Creating CUDN with EVPN transport")
			networkLabels := map[string]string{"network": testBaseName}
			err = createUserDefinedNetwork(f, ictx, f.Namespace, testBaseName, true, networkSpec, networkLabels)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("Creating RouteAdvertisements")
			err = createRouteAdvertisements(f, ictx, testBaseName, testBaseName, networkLabels, networkLabels)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("Creating test pod on CUDN")
			testPod := e2epod.CreateExecPodOrFail(
				context.Background(),
				f.ClientSet,
				f.Namespace.Name,
				testBaseName+"-pod",
				func(pod *corev1.Pod) {
					pod.Spec.Containers[0].Image = images.AgnHost()
				},
			)

			// Test connectivity based on which servers are configured
			if macvrfCfg != nil {
				ginkgo.By("Testing connectivity to external MAC-VRF server")
				serverIP := strings.Split(macvrfCfg.ServerIP, "/")[0] // Remove prefix
				testPodToServer(testPod, serverIP)
			}
			if ipvrfCfg != nil {
				ginkgo.By("Testing connectivity to external IP-VRF server")
				serverIP := strings.Split(ipvrfCfg.ServerIP, "/")[0] // Remove prefix
				testPodToServer(testPod, serverIP)
			}
		},
		// Layer2 with MAC-VRF only - L2 broadcast domain extended via EVPN
		ginkgo.Entry("Layer2 network with MAC-VRF",
			&udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRolePrimary,
					Subnets: udnv1.DualStackCIDRs{udnv1.CIDR("10.100.0.0/16")},
				},
				Transport: udnv1.TransportOptionEVPN,
				EVPN: &udnv1.EVPNConfig{
					VTEP: "", // Will be set dynamically
					MACVRF: &udnv1.VRFConfig{
						VNI: 10100,
					},
				},
			},
			&EVPNServerConfig{
				EVPNVRFConfig: EVPNVRFConfig{
					Name: "macvrf-l2",
					VID:  100,
					VNI:  10100,
				},
				ServerIP: "10.100.0.250/16",
			},
			nil, // No IP-VRF
		),
		// Layer2 with MAC-VRF + IP-VRF - L2 domain with L3 routing to external networks
		ginkgo.Entry("Layer2 network with MAC-VRF and IP-VRF",
			&udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRolePrimary,
					Subnets: udnv1.DualStackCIDRs{udnv1.CIDR("10.101.0.0/16")},
				},
				Transport: udnv1.TransportOptionEVPN,
				EVPN: &udnv1.EVPNConfig{
					VTEP: "", // Will be set dynamically
					MACVRF: &udnv1.VRFConfig{
						VNI: 10101,
					},
					IPVRF: &udnv1.VRFConfig{
						VNI: 20101,
					},
				},
			},
			&EVPNServerConfig{
				EVPNVRFConfig: EVPNVRFConfig{
					Name: "macvrf-l2l3",
					VID:  101,
					VNI:  10101,
				},
				ServerIP: "10.101.0.250/16",
			},
			&EVPNServerConfig{
				EVPNVRFConfig: EVPNVRFConfig{
					Name: "ipvrf-l2l3",
					VID:  201,
					VNI:  20101,
				},
				ServerIP: "172.20.101.100/24",
			},
		),
		// Layer3 with IP-VRF only - L3 routing via EVPN Type-5 routes
		ginkgo.Entry("Layer3 network with IP-VRF",
			&udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role: udnv1.NetworkRolePrimary,
					Subnets: []udnv1.Layer3Subnet{{
						CIDR: udnv1.CIDR("10.102.0.0/16"),
					}},
				},
				Transport: udnv1.TransportOptionEVPN,
				EVPN: &udnv1.EVPNConfig{
					VTEP: "", // Will be set dynamically
					IPVRF: &udnv1.VRFConfig{
						VNI: 20102,
					},
				},
			},
			nil, // No MAC-VRF
			&EVPNServerConfig{
				EVPNVRFConfig: EVPNVRFConfig{
					Name: "ipvrf-l3",
					VID:  202,
					VNI:  20102,
				},
				ServerIP: "172.20.102.100/24",
			},
		),
	)
})
