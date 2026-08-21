// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package util

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/k8snetworkplumbingwg/sriovnet"

	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/dpu-simulator/lib/dpusim"
	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// DPU operations abstraction.
//
// DPUOps is the central interface that hides DPU operational details
// (SR-IOV, switchdev, sysfs) behind a uniform API. All DPU and DPU Host
// mode code should go through GetDPUOps() rather than calling SriovnetOps
// directly.
//
// Two concrete implementations exist today:
//   - SwitchdevDPUOps - SR-IOV / switchdev hardware (NVIDIA BlueField, etc.)
//   - SimulatedDPUOps - simulated DPU environments (Kind, VMs with virtio)
//
// The singleton is selected by the --simulate-dpu configuration flag.
// When the flag is absent (the default), SwitchdevDPUOps is used.
type DPUOps interface {
	// GetDPUHostRepInterface returns the host representor interface attached to bridge
	// On switchdev hardware this discovers the VF/SF representor via sriovnet.
	// On simulated platforms this is either a veth peer or virtio interface.
	GetDPUHostRepInterface(ovsClient libovsdbclient.Client, bridgeName string) (string, error)

	// GetHostGatewayMACAddress returns the MAC address of the host-side
	// interface that corresponds to the DPU-side Host representor.
	// nodeName is the K8s node name of the host this DPU operates behalf of.
	GetHostGatewayMACAddress(ovsClient libovsdbclient.Client, bridgeName, nodeName string) (net.HardwareAddr, error)

	// FindHostRepresentorByPeerMAC returns the DPU-side representor attached to
	// bridge whose host-side peer function has hostMAC. Unlike
	// GetHostGatewayMACAddress, which only considers the single host PF
	// representor that backs the default gateway bridge, this inspects every
	// host-facing representor on the bridge, so it also resolves uplinks backed
	// by a host VF or SF. It wraps ErrHostRepresentorNotFound when the bridge
	// has no representor peering with hostMAC, which lets callers distinguish a
	// clean miss from a lookup failure. nodeName is the K8s node name of the
	// host this DPU operates on behalf of.
	FindHostRepresentorByPeerMAC(ovsClient libovsdbclient.Client, bridge *vswitchd.Bridge, hostMAC net.HardwareAddr,
		nodeName string) (string, error)

	// IsHostFacingRepresentor reports whether netdev is a DPU-side representor
	// of a host function (PF, VF or SF). Such representors can never be a
	// bridge's physical uplink even though OVS reports them as system-type
	// ports.
	IsHostFacingRepresentor(netdev string) bool

	// ResolveDeviceDetails returns PF and VF indices for a device identified
	// by either a PCI address (e.g. "0000:03:00.2") or a netdev name
	// (e.g. "eth0-1"). It is up to the implementation to interpret the deviceID
	// for the underlying platform.
	ResolveDeviceDetails(deviceID string) (*NetworkDeviceDetails, error)

	// ResolvePFIndex returns the PF index of a host physical function
	// identified by either a PCI address (e.g. "0000:03:00.0") or a netdev
	// name. It errors when the device is a VF or is not an SR-IOV capable
	// physical function.
	ResolvePFIndex(deviceID string) (int, error)

	// GetPortRepresentor finds the DPU-side representor (VF representor in the case of switchdev hardware)
	// for the given PF and function indices. On simulation this follows the
	// pattern rep<pfId>-<funcId> (e.g. "rep0-1").
	GetPortRepresentor(pfId, funcId string) (string, error)

	// GetPFRepresentor finds the DPU-side representor of the host PF with
	// the given index (e.g. "pf0hpf" on switchdev hardware).
	GetPFRepresentor(pfId string) (string, error)

	// GetHostPeerMACAddress returns the MAC of the host-side function peered
	// with the given DPU representor. nodeName is the K8s node name of the
	// host this DPU operates on behalf of; simulated platforms derive the
	// peer MAC from it.
	GetHostPeerMACAddress(rep, nodeName string) (net.HardwareAddr, error)

	// GetDeviceAddress returns an opaque, platform-specific identifier for
	// a representor interface. On switchdev hardware this is a PCI address
	// (e.g. "0000:01:00.2"); on simulated platforms it is the netdev name
	// itself. On switchdev, failure to resolve PCI for the representor is an error.
	GetDeviceAddress(repName string) (string, error)
}

// ErrHostRepresentorNotFound is wrapped by FindHostRepresentorByPeerMAC when a
// bridge holds no representor whose host-side peer matches the requested MAC.
var ErrHostRepresentorNotFound = errors.New("dpu host representor not found")

// ---------------------------------------------------------------------------
// DPUOps singleton
// ---------------------------------------------------------------------------

var (
	dpuOps     DPUOps
	dpuOpsOnce sync.Once
)

func initDPUOps() {
	if IsSimulatedDPU() {
		dpuOps = &SimulatedDPUOps{}
		klog.Infof("DPUOps initialised: Simulated DPU environment")
	} else {
		dpuOps = &SwitchdevDPUOps{}
		klog.Infof("DPUOps initialised: Switchdev hardware DPU environment")
	}
}

// GetDPUOps returns the current DPUOps singleton. If the singleton has not
// been initialised, it defaults to SwitchdevDPUOps (SR-IOV / switchdev hardware).
func GetDPUOps() DPUOps {
	dpuOpsOnce.Do(initDPUOps)
	return dpuOps
}

// IsSimulatedDPU returns true if we are in a Simulated DPU environment.
func IsSimulatedDPU() bool {
	if config.IsModeDPU() || config.IsModeDPUHost() {
		return config.OvnKubeNode.SimulateDPU
	}
	return false
}

// parseSimulatedNetdevName extracts the PF and function indices from a
// simulated netdevice name (a trailing "<pfId>-<funcId>", e.g. "eth0-5").
func parseSimulatedNetdevName(name string) (pfId, funcId int, err error) {
	matches := dpusim.ReSimulationNetdevFunc.FindStringSubmatch(name)
	if len(matches) != 3 {
		return 0, 0, fmt.Errorf("interface %s does not match simulated naming pattern *<pfId>-<funcId>", name)
	}
	pfId, err = strconv.Atoi(matches[1])
	if err != nil {
		return 0, 0, fmt.Errorf("failed to parse PF index from %q: %v", name, err)
	}
	funcId, err = strconv.Atoi(matches[2])
	if err != nil {
		return 0, 0, fmt.Errorf("failed to parse Function index from %q: %v", name, err)
	}
	return pfId, funcId, nil
}

// IsSimulatedNetdevName reports whether name is a valid simulated netdevice
// name, the same condition SimulatedDPUOps.ResolveDeviceDetails requires of a
// device ID.
func IsSimulatedNetdevName(name string) bool {
	_, _, err := parseSimulatedNetdevName(name)
	return err == nil
}

// ---------------------------------------------------------------------------
// SwitchdevDPUOps - SR-IOV / switchdev hardware (NVIDIA BlueField, etc.)
// ---------------------------------------------------------------------------

type SwitchdevDPUOps struct{}

func (n *SwitchdevDPUOps) GetDPUHostRepInterface(ovsClient libovsdbclient.Client, bridgeName string) (string, error) {
	br, err := ovsops.GetBridge(ovsClient, bridgeName)
	if err != nil {
		return "", fmt.Errorf("failed to get bridge %q: %w", bridgeName, err)
	}

	portsToInterfaces, err := getBridgePortsInterfaces(ovsClient, br)
	if err != nil {
		return "", err
	}

	for _, ifaces := range portsToInterfaces {
		for _, iface := range ifaces {
			flavor, err := GetSriovnetOps().GetRepresentorPortFlavour(iface.Name)
			if err == nil && flavor == sriovnet.PORT_FLAVOUR_PCI_PF {
				// host representor interface found
				return iface.Name, nil
			}
		}
	}
	// No host interface found in provided bridge
	return "", fmt.Errorf("dpu host interface was not found for bridge %q", bridgeName)
}

func (n *SwitchdevDPUOps) GetHostGatewayMACAddress(ovsClient libovsdbclient.Client, bridgeName, _ string) (net.HardwareAddr, error) {
	hostRep, err := n.GetDPUHostRepInterface(ovsClient, bridgeName)
	if err != nil {
		return nil, err
	}
	return GetSriovnetOps().GetRepresentorPeerMacAddress(hostRep)
}

// hostFacingRepresentorFlavours are the switchdev port flavours whose
// representors have a host-side peer function that can carry the gateway L3
// identity an Uplink selects.
var hostFacingRepresentorFlavours = map[sriovnet.PortFlavour]struct{}{
	sriovnet.PORT_FLAVOUR_PCI_PF: {},
	sriovnet.PORT_FLAVOUR_PCI_VF: {},
	sriovnet.PORT_FLAVOUR_PCI_SF: {},
}

func (n *SwitchdevDPUOps) FindHostRepresentorByPeerMAC(
	ovsClient libovsdbclient.Client,
	bridge *vswitchd.Bridge,
	hostMAC net.HardwareAddr,
	_ string,
) (string, error) {
	bridgeName := bridge.Name
	portsToInterfaces, err := getBridgePortsInterfaces(ovsClient, bridge)
	if err != nil {
		return "", err
	}

	for _, ifaces := range portsToInterfaces {
		for _, iface := range ifaces {
			rep := normalizeOVSName(iface.Name)
			flavour, err := GetSriovnetOps().GetRepresentorPortFlavour(rep)
			if err != nil {
				klog.V(5).Infof("Bridge %s: skipping interface %s, not a switchdev representor: %v",
					bridgeName, rep, err)
				continue
			}
			if _, ok := hostFacingRepresentorFlavours[flavour]; !ok {
				klog.V(5).Infof("Bridge %s: skipping representor %s, port flavour %d has no host peer",
					bridgeName, rep, flavour)
				continue
			}
			peerMAC, err := hostPeerMACAddress(rep, flavour)
			if err != nil {
				klog.V(5).Infof("Bridge %s: skipping representor %s, failed to read host peer MAC: %v",
					bridgeName, rep, err)
				continue
			}
			if bytes.Equal(peerMAC, hostMAC) {
				klog.V(4).Infof("Bridge %s: representor %s (flavour %d) peers with host MAC %s",
					bridgeName, rep, flavour, hostMAC)
				return rep, nil
			}
			klog.V(5).Infof("Bridge %s: representor %s peers with host MAC %s, looking for %s",
				bridgeName, rep, peerMAC, hostMAC)
		}
	}
	return "", fmt.Errorf("%w: no representor on bridge %q peers with host MAC %s",
		ErrHostRepresentorNotFound, bridgeName, hostMAC)
}

// hostPeerMACAddress returns the MAC of the host-side function peered with the
// representor rep. devlink reports the function address for every flavour, so it
// is tried first. sriovnet.GetRepresentorPeerMacAddress is only a fallback for
// PF representors, where it additionally understands the legacy sysfs layout.
func hostPeerMACAddress(rep string, flavour sriovnet.PortFlavour) (net.HardwareAddr, error) {
	mac, devlinkErr := GetSriovnetOps().GetDevlinkPortFunctionMacAddress(rep)
	if devlinkErr == nil {
		return mac, nil
	}
	if flavour != sriovnet.PORT_FLAVOUR_PCI_PF {
		return nil, devlinkErr
	}
	mac, err := GetSriovnetOps().GetRepresentorPeerMacAddress(rep)
	if err != nil {
		return nil, fmt.Errorf("%v; devlink lookup also failed: %v", err, devlinkErr)
	}
	// GetRepresentorPeerMacAddress can succeed with an empty or all-zero MAC
	// when the peer function MAC is unset; treat that as absent too.
	if !IsUsableEthernetMAC(mac) {
		return nil, fmt.Errorf("representor %s peer MAC is unusable; devlink lookup also failed: %v", rep, devlinkErr)
	}
	return mac, nil
}

func (n *SwitchdevDPUOps) IsHostFacingRepresentor(netdev string) bool {
	flavour, err := GetSriovnetOps().GetRepresentorPortFlavour(normalizeOVSName(netdev))
	if err != nil {
		return false
	}
	_, ok := hostFacingRepresentorFlavours[flavour]
	return ok
}

func (n *SwitchdevDPUOps) ResolveDeviceDetails(deviceID string) (*NetworkDeviceDetails, error) {
	if IsPCIDeviceName(deviceID) {
		return GetNetworkDeviceDetails(deviceID)
	}
	// deviceID is a netdev name – look up its PCI address via sysfs first.
	pciAddr, err := GetDeviceIDFromNetdevice(deviceID)
	if err != nil {
		return nil, fmt.Errorf("failed to read sysfs device link for %s: %v", deviceID, err)
	}
	return GetNetworkDeviceDetails(pciAddr)
}

func (n *SwitchdevDPUOps) ResolvePFIndex(deviceID string) (int, error) {
	pciAddr := deviceID
	if !IsPCIDeviceName(deviceID) {
		var err error
		pciAddr, err = GetDeviceIDFromNetdevice(deviceID)
		if err != nil {
			return -1, fmt.Errorf("failed to read sysfs device link for %s: %v", deviceID, err)
		}
	}
	// A VF has a parent PF behind a physfn link; refuse it here so PF and VF
	// resolution stay distinct.
	if _, err := GetSriovnetOps().GetPfPciFromVfPci(pciAddr); err == nil {
		return -1, fmt.Errorf("device %s is a virtual function, not a physical function", pciAddr)
	}
	// Any netdev parses to some PCI function number, so require SR-IOV
	// capability before treating the device as a PF with a DPU representor.
	sriovCapable, err := GetFileSystemOps().PathExists(
		filepath.Join(sriovnet.PciSysDir, pciAddr, "sriov_totalvfs"))
	if err != nil {
		return -1, fmt.Errorf("failed to check SR-IOV capability of device %s: %v", pciAddr, err)
	}
	if !sriovCapable {
		return -1, fmt.Errorf("device %s is not an SR-IOV capable physical function", pciAddr)
	}
	// The PF index is the PCI function number, the same convention sriovnet
	// uses to derive a VF's parent PF index.
	var domain, bus, dev, fn int
	const pciParts = 4
	parsed, err := fmt.Sscanf(pciAddr, "%04x:%02x:%02x.%d", &domain, &bus, &dev, &fn)
	if err != nil || parsed != pciParts {
		return -1, fmt.Errorf("failed to parse PCI address %s of device %s: parsed %d of %d fields: %v",
			pciAddr, deviceID, parsed, pciParts, err)
	}
	return fn, nil
}

func (n *SwitchdevDPUOps) GetPortRepresentor(pfId, funcId string) (string, error) {
	return GetSriovnetOps().GetVfRepresentorDPU(pfId, funcId)
}

func (n *SwitchdevDPUOps) GetPFRepresentor(pfId string) (string, error) {
	return GetSriovnetOps().GetPfRepresentorDPU(pfId)
}

func (n *SwitchdevDPUOps) GetHostPeerMACAddress(rep, _ string) (net.HardwareAddr, error) {
	flavour, err := GetSriovnetOps().GetRepresentorPortFlavour(rep)
	if err != nil {
		return nil, fmt.Errorf("failed to get port flavour for representor %s: %v", rep, err)
	}
	if _, ok := hostFacingRepresentorFlavours[flavour]; !ok {
		return nil, fmt.Errorf("representor %s port flavour %d has no host peer", rep, flavour)
	}
	return hostPeerMACAddress(rep, flavour)
}

func (n *SwitchdevDPUOps) GetDeviceAddress(repName string) (string, error) {
	addr, err := GetSriovnetOps().GetPCIFromDeviceName(repName)
	if err != nil {
		return "", err
	}
	return addr, nil
}

// ---------------------------------------------------------------------------
// SimulatedDPUOps - simulated DPU environments (Kind containers, VMs)
//
// Uses interface naming conventions instead of sysfs / switchdev:
//   - Host interfaces: <prefix><pfId>-<funcId>  (e.g. eth0-1)
//   - DPU representors: rep<pfId>-<funcId>      (e.g. rep0-1)
// ---------------------------------------------------------------------------

type SimulatedDPUOps struct{}

// generateMACForHostToDpu returns a deterministic MAC for a host-to-DPU data
// interface. The hash is over nodeName + role("host" or "dpu"); the index is
// encoded in the last octet so each channel in a pair has a unique MAC.
// OUI 52:54:00 is commonly used for QEMU/virtio and marks the address as
// locally administered.
func (s *SimulatedDPUOps) generateMACForHostToDpu(nodeName, role string, index int) string {
	h := sha256.Sum256([]byte(nodeName + "\x00" + role))
	return fmt.Sprintf("%s:%02x:%02x:%02x", dpusim.MacOUI, h[0], h[1], index&0xff)
}

// getDPURepresentor builds rep<pfId>-<funcId> and verifies the link exists,
// resolving it through its alias when it has been renamed.
func (s *SimulatedDPUOps) getDPURepresentor(pfId, funcId string) (string, error) {
	rep := fmt.Sprintf(dpusim.DPURepresentorFmt, pfId, funcId)
	link, err := LinkByNameOrAlias(rep)
	if err != nil {
		return "", fmt.Errorf("failed to resolve simulated representor %s by link name or alias: %w", rep, err)
	}
	return link.Attrs().Name, nil
}

func (s *SimulatedDPUOps) GetDPUHostRepInterface(ovsClient libovsdbclient.Client, bridgeName string) (string, error) {
	if bridgeName == "" {
		return dpusim.HostGatewayPeerInterface, nil
	}

	bridge, err := ovsops.GetBridge(ovsClient, bridgeName)
	if err != nil {
		return "", fmt.Errorf("failed to get bridge %q: %w", bridgeName, err)
	}
	portsToInterfaces, err := getBridgePortsInterfaces(ovsClient, bridge)
	if err != nil {
		return "", err
	}
	for _, port := range SortedKeys(portsToInterfaces) {
		ifaces := portsToInterfaces[port]
		normalizedPort := normalizeOVSName(port)
		if simulatedDPURepresentorIndex(normalizedPort) >= 0 {
			return normalizedPort, nil
		}
		for _, iface := range ifaces {
			ifaceName := normalizeOVSName(iface.Name)
			if simulatedDPURepresentorIndex(ifaceName) >= 0 {
				return ifaceName, nil
			}
		}
	}
	return "", fmt.Errorf("simulated DPU host representor was not found for bridge %q", bridgeName)
}

func (s *SimulatedDPUOps) GetHostGatewayMACAddress(
	ovsClient libovsdbclient.Client,
	bridgeName, nodeName string,
) (net.HardwareAddr, error) {
	if nodeName == "" {
		return nil, fmt.Errorf("nodeName must be provided for simulated GetHostGatewayMACAddress")
	}

	index := dpusim.HostGatewayInterfaceIndex
	if bridgeName != "" {
		rep, err := s.GetDPUHostRepInterface(ovsClient, bridgeName)
		if err != nil {
			return nil, err
		}
		index = simulatedDPURepresentorIndex(rep)
		if index < 0 {
			return nil, fmt.Errorf("failed to parse simulated DPU representor %q", rep)
		}
	}

	// TODO: This identifies a need to have an API to get reliable information from the host (requested by the DPU)
	macStr := s.generateMACForHostToDpu(nodeName, "host", index)
	mac, err := net.ParseMAC(macStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse generated MAC %s: %v", macStr, err)
	}

	klog.Infof("Derived host gateway peer MAC %s for node %s bridge %s", mac, nodeName, bridgeName)
	return mac, nil
}

func (s *SimulatedDPUOps) FindHostRepresentorByPeerMAC(
	ovsClient libovsdbclient.Client,
	bridge *vswitchd.Bridge,
	hostMAC net.HardwareAddr,
	nodeName string,
) (string, error) {
	if nodeName == "" {
		return "", fmt.Errorf("nodeName must be provided for simulated FindHostRepresentorByPeerMAC")
	}
	bridgeName := bridge.Name
	portsToInterfaces, err := getBridgePortsInterfaces(ovsClient, bridge)
	if err != nil {
		return "", err
	}

	for port, ifaces := range portsToInterfaces {
		candidates := []string{port}
		for _, iface := range ifaces {
			candidates = append(candidates, iface.Name)
		}
		for _, candidate := range candidates {
			rep := normalizeOVSName(candidate)
			index := simulatedDPURepresentorIndex(rep)
			if index < 0 {
				continue
			}
			macStr := s.generateMACForHostToDpu(nodeName, "host", index)
			peerMAC, err := net.ParseMAC(macStr)
			if err != nil {
				return "", fmt.Errorf("failed to parse generated MAC %s: %v", macStr, err)
			}
			if bytes.Equal(peerMAC, hostMAC) {
				klog.V(4).Infof("Bridge %s: simulated representor %s peers with host MAC %s",
					bridgeName, rep, hostMAC)
				return rep, nil
			}
			klog.V(5).Infof("Bridge %s: simulated representor %s peers with host MAC %s, looking for %s",
				bridgeName, rep, peerMAC, hostMAC)
		}
	}
	return "", fmt.Errorf("%w: no simulated representor on bridge %q peers with host MAC %s",
		ErrHostRepresentorNotFound, bridgeName, hostMAC)
}

func (s *SimulatedDPUOps) IsHostFacingRepresentor(netdev string) bool {
	return simulatedDPURepresentorIndex(normalizeOVSName(netdev)) >= 0
}

func normalizeOVSName(name string) string {
	return strings.Trim(strings.TrimSpace(name), `"`)
}

func simulatedDPURepresentorIndex(iface string) int {
	if !strings.HasPrefix(iface, strings.TrimSuffix(dpusim.DPUDataIfFmt, "%d")) {
		return -1
	}
	matches := dpusim.ReSimulationNetdevFunc.FindStringSubmatch(iface)
	if len(matches) != 3 || matches[1] != "0" {
		return -1
	}
	index, err := strconv.Atoi(matches[2])
	if err != nil || index < dpusim.HostGatewayInterfaceIndex {
		return -1
	}
	return index
}

func (s *SimulatedDPUOps) ResolveDeviceDetails(deviceID string) (*NetworkDeviceDetails, error) {
	pfId, funcId, err := parseSimulatedNetdevName(deviceID)
	if err != nil {
		return nil, err
	}
	klog.Infof("Device %s resolved as simulated netdev: PfId=%d, FuncId=%d", deviceID, pfId, funcId)
	return &NetworkDeviceDetails{
		DeviceId: deviceID,
		PfId:     pfId,
		FuncId:   funcId,
	}, nil
}

func (s *SimulatedDPUOps) ResolvePFIndex(string) (int, error) {
	// Simulated deployments have no host PF; PF uplinks fall back to the host
	// MAC resolution path.
	return -1, fmt.Errorf("PF device details are not supported in simulated DPU environments")
}

func (s *SimulatedDPUOps) GetPortRepresentor(pfId, funcId string) (string, error) {
	return s.getDPURepresentor(pfId, funcId)
}

func (s *SimulatedDPUOps) GetPFRepresentor(string) (string, error) {
	return "", fmt.Errorf("PF representor lookup is not supported in simulated DPU environments")
}

func (s *SimulatedDPUOps) GetHostPeerMACAddress(rep, nodeName string) (net.HardwareAddr, error) {
	if nodeName == "" {
		return nil, fmt.Errorf("nodeName must be provided for simulated GetHostPeerMACAddress")
	}
	index := simulatedDPURepresentorIndex(normalizeOVSName(rep))
	if index < 0 {
		return nil, fmt.Errorf("interface %s is not a simulated representor", rep)
	}
	return net.ParseMAC(s.generateMACForHostToDpu(nodeName, "host", index))
}

func (s *SimulatedDPUOps) GetDeviceAddress(repName string) (string, error) {
	return repName, nil
}
