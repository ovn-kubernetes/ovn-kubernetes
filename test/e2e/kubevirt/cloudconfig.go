// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

type CloudInitNetworkData struct {
	Version   int                          `json:"version,omitempty"`
	Ethernets map[string]CloudInitEthernet `json:"ethernets,omitempty"`
	Bridges   map[string]CloudInitBridge   `json:"bridges,omitempty"`
}

type CloudInitEthernet struct {
	DHCP4     *bool    `json:"dhcp4,omitempty"`
	DHCP6     *bool    `json:"dhcp6,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
}

type CloudInitBridge struct {
	Interfaces []string       `json:"interfaces,omitempty"`
	MACAddress *string        `json:"macaddress,omitempty"`
	Addresses  []string       `json:"addresses,omitempty"`
	DHCP4      *bool          `json:"dhcp4,omitempty"`
	DHCP6      *bool          `json:"dhcp6,omitempty"`
	Parameters map[string]any `json:"parameters,omitempty"`
}

// UplinkLinuxBridgeNetworkData returns cloud-init network-data for creating linux-bridge
// on top the given interface, with given MAC address and IP addresses.
// This configuration is handy for testing OVN port security; MAC spoofed traffic,
// simulating nested virtualization or NFV.
func UplinkLinuxBridgeNetworkData(targetIface, bridgeIface, mac string, ips []string) CloudInitNetworkData {
	return CloudInitNetworkData{
		Version: 2,
		Ethernets: map[string]CloudInitEthernet{
			targetIface: {
				DHCP4: new(false),
				DHCP6: new(false),
			},
		},
		Bridges: map[string]CloudInitBridge{
			bridgeIface: {
				Interfaces: []string{targetIface},
				DHCP4:      new(false),
				DHCP6:      new(false),
				Addresses:  ips,
				MACAddress: new(mac),
				Parameters: map[string]any{
					"stp":           false,
					"forward-delay": 0,
				},
			},
		},
	}
}
