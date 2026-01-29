package util

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/k8snetworkplumbingwg/sriovnet"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"k8s.io/klog/v2"
)

var (
	dpuProviderInstance DPUProvider
	dpuProviderOnce     sync.Once
)

// GetDPUProvider returns the appropriate DPU provider based on configuration
func GetDPUProvider() DPUProvider {
	dpuProviderOnce.Do(func() {
		switch config.Default.DPUProviderMode {
		case "veth":
			klog.V(2).Infof("Using VethRepresentorProvider for DPU operations")
			dpuProviderInstance = NewVethRepresentorProvider()
		case "sriov":
			klog.V(2).Infof("Using SriovDPUProvider for DPU operations")
			dpuProviderInstance = NewSriovDPUProvider()
		default:
			klog.Warningf("Unknown DPU provider mode '%s', falling back to SR-IOV", config.Default.DPUProviderMode)
			dpuProviderInstance = NewSriovDPUProvider()
		}
	})
	return dpuProviderInstance
}

// DPUProvider defines the interface for DPU connection management
// This allows swapping between different DPU implementations (SR-IOV hardware vs veth simulation)
type DPUProvider interface {
	// Pod networking operations
	// Host side: DeviceID string -> DPUConnectionDetails
	// Creates DPU connection details from device information
	CreateConnectionDetails(deviceID, sandboxID, netdevName string) (*DPUConnectionDetails, error)

	// DPU side: DPUConnectionDetails -> (representor name, device identifier)
	// Gets representor information needed for OVS configuration
	GetRepresentorInfo(dpuCD *DPUConnectionDetails) (string, string, error)

	// Management port operations
	// Host side: Management interface -> NetworkDeviceDetails for annotation
	// Creates management port details for DPU-Host mode
	CreateManagementPortDetails(mgmtNetdev string) (*NetworkDeviceDetails, error)

	// DPU side: NetworkDeviceDetails -> representor name
	// Gets management port representor for DPU mode
	GetManagementPortRepresentor(mgmtDetails *NetworkDeviceDetails) (string, error)

	// Gateway bridge operations
	// DPU side: Get the interface name for the gateway/management interface that should be connected to br-ex
	// This replaces hardcoded detection logic and follows the DPU provider abstraction
	GetHostRepresentor() (string, error)

	// MAC address operations
	// DPU side: Get the MAC address of the peer interface for bridge configuration
	// This replaces direct sriovnet calls that expect SR-IOV eswitch metadata
	GetRepresentorPeerMacAddress(netdev string) (net.HardwareAddr, error)

	// Port flavour operations
	// DPU side: Get the representor port flavour (PF vs VF) for port validation
	// This replaces direct sriovnet calls that check eswitch port metadata
	GetRepresentorPortFlavour(netdev string) (sriovnet.PortFlavour, error)

	// VF representor operations
	// DPU side: Get VF representor name from PF and VF identifiers
	// This replaces direct sriovnet calls used in management port setup
	GetVfRepresentorDPU(pfId, vfId string) (string, error)

	// PCI device conversion operations
	// Get PCI address from network device name
	GetPciFromNetDevice(netdev string) (string, error)
	// Get PF PCI address from VF PCI address
	GetPfPciFromVfPci(vfPci string) (string, error)
	// Get network devices from PCI address
	GetNetDevicesFromPci(pciAddress string) ([]string, error)
}

// SriovDPUProvider implements DPUProvider for SR-IOV hardware using existing sriovnet logic
type SriovDPUProvider struct{}

// NewSriovDPUProvider creates a new SR-IOV DPU provider
func NewSriovDPUProvider() *SriovDPUProvider {
	return &SriovDPUProvider{}
}

// CreateConnectionDetails implements the host side logic - converts PCI device ID to DPU connection details
func (p *SriovDPUProvider) CreateConnectionDetails(deviceID, sandboxID, netdevName string) (*DPUConnectionDetails, error) {
	if deviceID == "" {
		return nil, fmt.Errorf("DeviceID must be set for SR-IOV DPU provider")
	}

	vfindex, err := GetSriovnetOps().GetVfIndexByPciAddress(deviceID)
	if err != nil {
		return nil, err
	}
	pfindex, err := GetSriovnetOps().GetPfIndexByVfPciAddress(deviceID)
	if err != nil {
		return nil, err
	}

	dpuConnDetails := DPUConnectionDetails{
		PfId:         fmt.Sprint(pfindex),
		VfId:         fmt.Sprint(vfindex),
		SandboxId:    sandboxID,
		VfNetdevName: netdevName,
	}

	return &dpuConnDetails, nil
}

// GetRepresentorInfo implements the DPU side logic - converts DPU connection details to representor info
func (p *SriovDPUProvider) GetRepresentorInfo(dpuCD *DPUConnectionDetails) (string, string, error) {
	vfRepName, err := GetSriovnetOps().GetVfRepresentorDPU(dpuCD.PfId, dpuCD.VfId)
	if err != nil {
		return "", "", err
	}

	vfPciAddress, err := GetSriovnetOps().GetPCIFromDeviceName(vfRepName)
	if err != nil {
		return "", "", err
	}

	return vfRepName, vfPciAddress, nil
}

// CreateManagementPortDetails implements the management port logic for DPU-Host side
func (p *SriovDPUProvider) CreateManagementPortDetails(mgmtNetdev string) (*NetworkDeviceDetails, error) {
	deviceID, err := GetDeviceIDFromNetdevice(mgmtNetdev)
	if err != nil {
		return nil, err
	}
	return GetNetworkDeviceDetails(deviceID)
}

// GetManagementPortRepresentor implements the management port logic for DPU side
func (p *SriovDPUProvider) GetManagementPortRepresentor(mgmtDetails *NetworkDeviceDetails) (string, error) {
	return GetSriovnetOps().GetVfRepresentorDPU(fmt.Sprint(mgmtDetails.PfId), fmt.Sprint(mgmtDetails.FuncId))
}

// GetHostRepresentor implements the gateway interface detection for SR-IOV hardware
func (p *SriovDPUProvider) GetHostRepresentor() (string, error) {
	// For SR-IOV hardware, the gateway interface is typically pf0
	// This follows the existing SR-IOV DPU pattern where pf0 is the management interface
	return "pf0", nil
}

// GetRepresentorPeerMacAddress delegates to existing sriovnet function for SR-IOV hardware
func (p *SriovDPUProvider) GetRepresentorPeerMacAddress(netdev string) (net.HardwareAddr, error) {
	return GetSriovnetOps().GetRepresentorPeerMacAddress(netdev)
}

// GetRepresentorPortFlavour delegates to existing sriovnet function for SR-IOV hardware
func (p *SriovDPUProvider) GetRepresentorPortFlavour(netdev string) (sriovnet.PortFlavour, error) {
	return GetSriovnetOps().GetRepresentorPortFlavour(netdev)
}

// GetVfRepresentorDPU delegates to existing sriovnet function for SR-IOV hardware
func (p *SriovDPUProvider) GetVfRepresentorDPU(pfId, vfId string) (string, error) {
	return GetSriovnetOps().GetVfRepresentorDPU(pfId, vfId)
}

// GetPciFromNetDevice delegates to existing sriovnet function for SR-IOV hardware
func (p *SriovDPUProvider) GetPciFromNetDevice(netdev string) (string, error) {
	return GetSriovnetOps().GetPciFromNetDevice(netdev)
}

// GetPfPciFromVfPci delegates to existing sriovnet function for SR-IOV hardware
func (p *SriovDPUProvider) GetPfPciFromVfPci(vfPci string) (string, error) {
	return GetSriovnetOps().GetPfPciFromVfPci(vfPci)
}

// GetNetDevicesFromPci delegates to existing sriovnet function for SR-IOV hardware
func (p *SriovDPUProvider) GetNetDevicesFromPci(pciAddress string) ([]string, error) {
	return GetSriovnetOps().GetNetDevicesFromPci(pciAddress)
}

// VethRepresentorProvider implements DPUProvider for veth-based DPU simulation
// This provider works with the established veth topology where host interfaces
// (eth0-0..15) connect to DPU representor interfaces (rep0-0..15, rep1-0..15)
type VethRepresentorProvider struct{}

// NewVethRepresentorProvider creates a new veth-based DPU provider
func NewVethRepresentorProvider() *VethRepresentorProvider {
	return &VethRepresentorProvider{}
}

// parseVethInterface parses veth interface names like "eth0-5" to extract pfId and vfId
func parseVethInterface(name string) (pfId, vfId string, err error) {
	// Match patterns like "eth0-5", "eth1-10", etc.
	re := regexp.MustCompile(`^eth(\d+)-(\d+)$`)
	matches := re.FindStringSubmatch(name)
	if len(matches) != 3 {
		return "", "", fmt.Errorf("invalid veth interface name format: %s (expected ethX-Y)", name)
	}
	return matches[1], matches[2], nil
}

// detectLocalPfId auto-detects which PF this DPU node serves by scanning for rep{pfId}-* interfaces
func (p *VethRepresentorProvider) detectLocalPfId() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("failed to list network interfaces: %v", err)
	}

	// Look for representor interfaces like "rep0-0", "rep1-5", etc.
	re := regexp.MustCompile(`^rep(\d+)-\d+$`)
	pfIds := make(map[string]bool)

	for _, iface := range interfaces {
		matches := re.FindStringSubmatch(iface.Name)
		if len(matches) == 2 {
			pfIds[matches[1]] = true
		}
	}

	// We should find exactly one PF ID on a DPU node
	if len(pfIds) == 0 {
		return "", fmt.Errorf("no representor interfaces found (rep{pfId}-{vfId} pattern)")
	}
	if len(pfIds) > 1 {
		var found []string
		for pfId := range pfIds {
			found = append(found, pfId)
		}
		return "", fmt.Errorf("multiple PF IDs detected: %v (DPU should serve only one PF)", found)
	}

	// Return the single PF ID found
	for pfId := range pfIds {
		return pfId, nil
	}
	return "", fmt.Errorf("unexpected error in PF ID detection")
}

// generateFakePCI creates deterministic fake PCI addresses for veth simulation
func generateFakePCI(pfId, vfId string) string {
	return fmt.Sprintf("veth:00:%02s.%s", pfId, vfId)
}

// CreateConnectionDetails implements the host side logic for veth provider
// Parses interface names like "eth0-5" to extract pfId="0", vfId="5"
func (p *VethRepresentorProvider) CreateConnectionDetails(deviceID, sandboxID, netdevName string) (*DPUConnectionDetails, error) {
	var interfaceName string

	// Try both deviceID and netdevName as interface name sources
	if netdevName != "" {
		interfaceName = netdevName
	} else if deviceID != "" {
		interfaceName = deviceID
	} else {
		return nil, fmt.Errorf("both deviceID and netdevName are empty for veth DPU provider")
	}

	// Parse the interface name to extract PF and VF IDs
	pfId, vfId, err := parseVethInterface(interfaceName)
	if err != nil {
		return nil, fmt.Errorf("failed to parse veth interface %s: %v", interfaceName, err)
	}

	dpuConnDetails := DPUConnectionDetails{
		PfId:         pfId,
		VfId:         vfId,
		SandboxId:    sandboxID,
		VfNetdevName: interfaceName,
	}

	return &dpuConnDetails, nil
}

// GetRepresentorInfo implements the DPU side logic for veth provider
// Auto-detects local DPU node and maps DPUConnectionDetails to representor name
func (p *VethRepresentorProvider) GetRepresentorInfo(dpuCD *DPUConnectionDetails) (string, string, error) {
	if dpuCD == nil {
		return "", "", fmt.Errorf("DPUConnectionDetails cannot be nil")
	}

	// Auto-detect which PF this DPU serves
	localPfId, err := p.detectLocalPfId()
	if err != nil {
		return "", "", fmt.Errorf("failed to detect local PF ID: %v", err)
	}

	// Verify this connection belongs to our PF
	if dpuCD.PfId != localPfId {
		return "", "", fmt.Errorf("connection PF ID %s does not match local DPU PF ID %s", dpuCD.PfId, localPfId)
	}

	// Generate representor name: rep{pfId}-{vfId}
	representorName := fmt.Sprintf("rep%s-%s", dpuCD.PfId, dpuCD.VfId)

	// Check if representor interface actually exists
	if _, err := net.InterfaceByName(representorName); err != nil {
		return "", "", fmt.Errorf("representor interface %s not found: %v", representorName, err)
	}

	// Generate fake PCI address for simulation
	pciAddress := generateFakePCI(dpuCD.PfId, dpuCD.VfId)

	return representorName, pciAddress, nil
}

// CreateManagementPortDetails implements the management port logic for veth DPU-Host side
func (p *VethRepresentorProvider) CreateManagementPortDetails(mgmtNetdev string) (*NetworkDeviceDetails, error) {
	// For veth simulation, the management port uses a data VF interface (eth0-0),
	// matching the SR-IOV pattern where one VF is dedicated to management.
	// Parse the interface name to extract PF and VF IDs.
	pfId, vfId, err := parseVethInterface(mgmtNetdev)
	if err != nil {
		return nil, fmt.Errorf("failed to parse management interface %s: %v", mgmtNetdev, err)
	}

	pfIdInt, err := strconv.Atoi(pfId)
	if err != nil {
		return nil, fmt.Errorf("invalid PF ID %s: %v", pfId, err)
	}
	vfIdInt, err := strconv.Atoi(vfId)
	if err != nil {
		return nil, fmt.Errorf("invalid VF ID %s: %v", vfId, err)
	}

	deviceID := fmt.Sprintf("veth-mgmt:%s:%s", pfId, vfId)

	return &NetworkDeviceDetails{
		PfId:     pfIdInt,
		FuncId:   vfIdInt,
		DeviceId: deviceID,
	}, nil
}

// GetManagementPortRepresentor implements the management port logic for veth DPU side
func (p *VethRepresentorProvider) GetManagementPortRepresentor(mgmtDetails *NetworkDeviceDetails) (string, error) {
	if mgmtDetails == nil {
		return "", fmt.Errorf("NetworkDeviceDetails cannot be nil")
	}

	// For veth simulation, use a data representor (rep0-0) as the management port,
	// matching the SR-IOV pattern where a VF representor is dedicated to management.
	// pfrep is reserved for the gateway bridge (breth0) via GetHostRepresentor().
	localPfId, err := p.detectLocalPfId()
	if err != nil {
		return "", fmt.Errorf("failed to detect local PF ID: %v", err)
	}
	mgmtRepresentor := fmt.Sprintf("rep%s-%d", localPfId, mgmtDetails.FuncId)

	// Verify the interface exists
	if _, err := net.InterfaceByName(mgmtRepresentor); err != nil {
		return "", fmt.Errorf("management representor interface %s not found: %v", mgmtRepresentor, err)
	}

	return mgmtRepresentor, nil
}

// GetHostRepresentor implements the gateway interface detection for veth simulation
func (p *VethRepresentorProvider) GetHostRepresentor() (string, error) {
	// For veth simulation, use pfrep as the dedicated management representor
	// This separates management (pfrep) from data (rep0-*) channels
	managementRepresentor := "pfrep"

	// Verify the interface exists
	if _, err := net.InterfaceByName(managementRepresentor); err != nil {
		return "", fmt.Errorf("management representor interface %s not found: %v", managementRepresentor, err)
	}

	return managementRepresentor, nil
}

// GetRepresentorPeerMacAddress generates deterministic MAC address for veth simulation
func (p *VethRepresentorProvider) GetRepresentorPeerMacAddress(netdev string) (net.HardwareAddr, error) {
	// Handle management interface (pfrep)
	if netdev == "pfrep" {
		// Generate deterministic MAC address for management interface
		// Format: 02:42:01:FF:00:00 (local unicast + veth identifier + management marker)
		mac := net.HardwareAddr{
			0x02, 0x42, // Local unicast prefix (Docker-style)
			0x01, 0xFF, // veth pair identifier + management marker (0xFF)
			0x00, 0x00, // Management interface (00:00)
		}
		return mac, nil
	}

	// Parse representor interface name (rep0-0, rep1-5, etc.) to get PF and VF IDs
	re := regexp.MustCompile(`^rep(\d+)-(\d+)$`)
	matches := re.FindStringSubmatch(netdev)
	if len(matches) != 3 {
		return nil, fmt.Errorf("interface name %s does not match rep{pfId}-{vfId} pattern", netdev)
	}

	pfId, vfId := matches[1], matches[2]

	pfIdInt, err := strconv.Atoi(pfId)
	if err != nil {
		return nil, fmt.Errorf("invalid PF ID %s: %v", pfId, err)
	}

	vfIdInt, err := strconv.Atoi(vfId)
	if err != nil {
		return nil, fmt.Errorf("invalid VF ID %s: %v", vfId, err)
	}

	// Generate deterministic MAC address for bridge configuration
	// Format: 02:42:01:00:PF:VF (local unicast + veth identifier + IDs)
	mac := net.HardwareAddr{
		0x02, 0x42, // Local unicast prefix (Docker-style)
		0x01, 0x00, // veth pair identifier
		byte(pfIdInt), byte(vfIdInt), // PF and VF IDs
	}

	return mac, nil
}

// GetRepresentorPortFlavour simulates SR-IOV port flavour for veth interfaces
func (p *VethRepresentorProvider) GetRepresentorPortFlavour(netdev string) (sriovnet.PortFlavour, error) {
	// Check if this is the management interface
	if netdev == "pfrep" {
		return sriovnet.PORT_FLAVOUR_PCI_PF, nil
	}

	// Parse interface name to determine type
	re := regexp.MustCompile(`^rep(\d+)-(\d+)$`)
	matches := re.FindStringSubmatch(netdev)
	if len(matches) != 3 {
		return sriovnet.PORT_FLAVOUR_UNKNOWN, fmt.Errorf("interface name %s does not match rep{pfId}-{vfId} pattern", netdev)
	}

	// All rep{pfId}-{vfId} interfaces are now data representors (VF equivalent)
	// Including rep0-0, which is no longer the management interface
	return sriovnet.PORT_FLAVOUR_PCI_VF, nil
}

// GetVfRepresentorDPU generates VF representor name for veth simulation
func (p *VethRepresentorProvider) GetVfRepresentorDPU(pfId, vfId string) (string, error) {
	// For veth simulation, always use PF 0 since we assume rep0-* interfaces
	return fmt.Sprintf("rep0-%s", vfId), nil
}

// GetPciFromNetDevice generates fake PCI address for veth simulation
func (p *VethRepresentorProvider) GetPciFromNetDevice(netdev string) (string, error) {
	// Handle management interface (pfrep)
	if netdev == "pfrep" {
		return "veth-mgmt:00:00.0", nil
	}

	// Parse representor interface name (rep0-5 -> pfId=0, vfId=5)
	re := regexp.MustCompile(`^rep(\d+)-(\d+)$`)
	matches := re.FindStringSubmatch(netdev)
	if len(matches) != 3 {
		return "", fmt.Errorf("interface name %s does not match rep{pfId}-{vfId} pattern", netdev)
	}

	pfId, vfId := matches[1], matches[2]
	return fmt.Sprintf("veth:00:%02s.%s", pfId, vfId), nil
}

// GetPfPciFromVfPci converts VF PCI to PF PCI for veth simulation
func (p *VethRepresentorProvider) GetPfPciFromVfPci(vfPci string) (string, error) {
	// Convert VF PCI to PF PCI (set VF ID to 0)
	// Example: veth:00:00.5 -> veth:00:00.0
	parts := strings.Split(vfPci, ".")
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid PCI address format: %s", vfPci)
	}
	return parts[0] + ".0", nil
}

// GetNetDevicesFromPci returns the veth interface name for the given PCI address
func (p *VethRepresentorProvider) GetNetDevicesFromPci(pciAddress string) ([]string, error) {
	// Extract PF and VF IDs from fake PCI address (veth:00:00.5 -> pfId=0, vfId=5)
	parts := strings.Split(pciAddress, ":")
	if len(parts) < 3 {
		return nil, fmt.Errorf("invalid PCI address format: %s", pciAddress)
	}

	deviceFunc := strings.Split(parts[2], ".")
	if len(deviceFunc) != 2 {
		return nil, fmt.Errorf("invalid PCI address format: %s", pciAddress)
	}

	pfId, vfId := deviceFunc[0], deviceFunc[1]

	// Remove leading zeros from pfId (00 -> 0)
	pfIdInt, err := strconv.Atoi(pfId)
	if err != nil {
		return nil, fmt.Errorf("invalid PF ID in PCI address %s: %v", pciAddress, err)
	}

	return []string{fmt.Sprintf("rep%d-%s", pfIdInt, vfId)}, nil
}
