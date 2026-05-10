// Package dpusim provides shared constants for the DPU simulation
// environment. Both dpu-sim itself and OVN-Kubernetes import this module
// so naming conventions stay in sync.
package dpusim

import (
	"fmt"
	"regexp"
)

// Host-to-DPU data interface format strings.
// Use with fmt.Sprintf(..., index).
const (
	HostDataIfFmt = "eth0-%d" // host-side data interface  (e.g. eth0-0, eth0-1)
	DPUDataIfFmt  = "rep0-%d" // DPU-side representor      (e.g. rep0-0, rep0-1)

	// DPURepresentorFmt builds a representor name from string PF and function
	// IDs: fmt.Sprintf(DPURepresentorFmt, pfId, funcId) → "rep0-1".
	DPURepresentorFmt = "rep%s-%s"
)

// Well-known simulation interfaces and their indices.
const (
	HostGatewayInterfaceIndex = 0
	MgmtPortInterfaceIndex    = 1

	HostGatewayInterface     = "eth0-0" // fmt.Sprintf(HostDataIfFmt, HostGatewayInterfaceIndex)
	HostGatewayPeerInterface = "rep0-0" // fmt.Sprintf(DPUDataIfFmt, HostGatewayInterfaceIndex)
	MgmtPortNetDevName       = "eth0-1" // fmt.Sprintf(HostDataIfFmt, MgmtPortInterfaceIndex)
)

// ReSimulationNetdevFunc matches the trailing <pfId>-<funcId> suffix on
// simulated interface names (e.g. "eth0-1" → ["eth0-1","0","1"]).
var ReSimulationNetdevFunc = regexp.MustCompile(`(\d+)-(\d+)$`)

// MacOUI is the locally-administered OUI used for deterministic MACs in
// simulated DPU environments (QEMU/virtio convention).
const MacOUI = "52:54:00"

// HostDataIf returns the host-side data interface name for the given index.
func HostDataIf(index int) string {
	return fmt.Sprintf(HostDataIfFmt, index)
}

// DPUDataIf returns the DPU-side representor name for the given index.
func DPUDataIf(index int) string {
	return fmt.Sprintf(DPUDataIfFmt, index)
}
