// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package util

import (
	"crypto/sha256"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/k8snetworkplumbingwg/sriovnet"

	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/dpu-simulator/lib/dpusim"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
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
	GetDPUHostRepInterface(bridgeName string) (string, error)

	// GetHostGatewayMACAddress returns the MAC address of the host-side
	// interface that corresponds to the DPU-side Host representor.
	// nodeName is the K8s node name of the host this DPU operates behalf of.
	GetHostGatewayMACAddress(bridgeName, nodeName string) (net.HardwareAddr, error)

	// ResolveDeviceDetails returns PF and VF indices for a device identified
	// by either a PCI address (e.g. "0000:03:00.2") or a netdev name
	// (e.g. "eth0-1"). It is up to the implementation to interpret the deviceID
	// for the underlying platform.
	ResolveDeviceDetails(deviceID string) (*NetworkDeviceDetails, error)

	// GetPortRepresentor finds the DPU-side representor (VF representor in the case of switchdev hardware)
	// for the given PF and function indices. On simulation this follows the
	// pattern rep<pfId>-<funcId> (e.g. "rep0-1").
	GetPortRepresentor(pfId, funcId string) (string, error)

	// GetDeviceAddress returns an opaque, platform-specific identifier for
	// a representor interface. On switchdev hardware this is a PCI address
	// (e.g. "0000:01:00.2"); on simulated platforms it is the netdev name
	// itself. On switchdev, failure to resolve PCI for the representor is an error.
	GetDeviceAddress(repName string) (string, error)
}

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

// ---------------------------------------------------------------------------
// SwitchdevDPUOps - SR-IOV / switchdev hardware (NVIDIA BlueField, etc.)
// ---------------------------------------------------------------------------

type SwitchdevDPUOps struct{}

func (n *SwitchdevDPUOps) GetDPUHostRepInterface(bridgeName string) (string, error) {
	portsToInterfaces, err := getBridgePortsInterfaces(bridgeName)
	if err != nil {
		return "", err
	}

	for _, ifaces := range portsToInterfaces {
		for _, iface := range ifaces {
			stdout, stderr, err := RunOVSVsctl("get", "Interface", strings.TrimSpace(iface), "Name")
			if err != nil {
				return "", fmt.Errorf("failed to get Interface %q Name on bridge %q:, stderr: %q, error: %v",
					iface, bridgeName, stderr, err)

			}
			flavor, err := GetSriovnetOps().GetRepresentorPortFlavour(stdout)
			if err == nil && flavor == sriovnet.PORT_FLAVOUR_PCI_PF {
				// host representor interface found
				return stdout, nil
			}
			continue
		}
	}
	// No host interface found in provided bridge
	return "", fmt.Errorf("dpu host interface was not found for bridge %q", bridgeName)
}

func (n *SwitchdevDPUOps) GetHostGatewayMACAddress(bridgeName, _ string) (net.HardwareAddr, error) {
	hostRep, err := n.GetDPUHostRepInterface(bridgeName)
	if err != nil {
		return nil, err
	}
	return GetSriovnetOps().GetRepresentorPeerMacAddress(hostRep)
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

func (n *SwitchdevDPUOps) GetPortRepresentor(pfId, funcId string) (string, error) {
	return GetSriovnetOps().GetVfRepresentorDPU(pfId, funcId)
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

// getDPURepresentor builds rep<pfId>-<funcId> and verifies the link exists.
func (s *SimulatedDPUOps) getDPURepresentor(pfId, funcId string) (string, error) {
	rep := fmt.Sprintf(dpusim.DPURepresentorFmt, pfId, funcId)
	if _, err := GetNetLinkOps().LinkByName(rep); err != nil {
		return "", fmt.Errorf("simulated representor %s not found: %v", rep, err)
	}
	return rep, nil
}

func (s *SimulatedDPUOps) GetDPUHostRepInterface(_ string) (string, error) {
	return dpusim.HostGatewayPeerInterface, nil
}

func (s *SimulatedDPUOps) GetHostGatewayMACAddress(_, nodeName string) (net.HardwareAddr, error) {
	if nodeName == "" {
		return nil, fmt.Errorf("nodeName must be provided for simulated GetHostGatewayMACAddress")
	}

	// TODO: This identifies a need to have an API to get reliable information from the host (requested by the DPU)
	macStr := s.generateMACForHostToDpu(nodeName, "host", dpusim.HostGatewayInterfaceIndex)
	mac, err := net.ParseMAC(macStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse generated MAC %s: %v", macStr, err)
	}

	klog.Infof("Derived host gateway peer MAC %s for node %s", mac, nodeName)
	return mac, nil
}

func (s *SimulatedDPUOps) ResolveDeviceDetails(deviceID string) (*NetworkDeviceDetails, error) {
	matches := dpusim.ReSimulationNetdevFunc.FindStringSubmatch(deviceID)
	if len(matches) != 3 {
		return nil, fmt.Errorf("interface %s does not match simulated naming pattern *<pfId>-<funcId>", deviceID)
	}
	pfId, err := strconv.Atoi(matches[1])
	if err != nil {
		return nil, fmt.Errorf("failed to parse PF index from %q: %v", deviceID, err)
	}
	funcId, err := strconv.Atoi(matches[2])
	if err != nil {
		return nil, fmt.Errorf("failed to parse Function index from %q: %v", deviceID, err)
	}
	klog.Infof("Device %s resolved as simulated netdev: PfId=%d, FuncId=%d", deviceID, pfId, funcId)
	return &NetworkDeviceDetails{
		DeviceId: deviceID,
		PfId:     pfId,
		FuncId:   funcId,
	}, nil
}

func (s *SimulatedDPUOps) GetPortRepresentor(pfId, funcId string) (string, error) {
	return s.getDPURepresentor(pfId, funcId)
}

func (s *SimulatedDPUOps) GetDeviceAddress(repName string) (string, error) {
	return repName, nil
}
