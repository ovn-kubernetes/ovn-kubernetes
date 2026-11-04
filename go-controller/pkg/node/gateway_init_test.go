// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"testing"

	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func TestInterfaceForEXGW(t *testing.T) {
	bridgeWithUplink := []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"breth1-uuid"}},
		&vswitchd.Bridge{UUID: "breth1-uuid", Name: "breth1", Ports: []string{"breth1-port-uuid", "eth1-port-uuid"}},
		&vswitchd.Port{UUID: "breth1-port-uuid", Name: "breth1", Interfaces: []string{"breth1-iface-uuid"}},
		&vswitchd.Interface{UUID: "breth1-iface-uuid", Name: "breth1", Type: "internal"},
		&vswitchd.Port{UUID: "eth1-port-uuid", Name: "eth1", Interfaces: []string{"eth1-iface-uuid"}},
		&vswitchd.Interface{UUID: "eth1-iface-uuid", Name: "eth1", Type: "system"},
	}
	// Bridge left behind after OVS removed the transient uplink port on restart.
	bridgeWithoutUplink := []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"breth1-uuid"}},
		&vswitchd.Bridge{UUID: "breth1-uuid", Name: "breth1", Ports: []string{"breth1-port-uuid"}},
		&vswitchd.Port{UUID: "breth1-port-uuid", Name: "breth1", Interfaces: []string{"breth1-iface-uuid"}},
		&vswitchd.Interface{UUID: "breth1-iface-uuid", Name: "breth1", Type: "internal"},
	}

	tests := []struct {
		name     string
		ovsData  []libovsdbtest.TestData
		intfName string
		want     string
	}{
		{
			name:     "no bridge returns the interface",
			ovsData:  []libovsdbtest.TestData{&vswitchd.OpenvSwitch{UUID: "root-ovs"}},
			intfName: "eth1",
			want:     "eth1",
		},
		{
			name:     "bridge name returns the bridge",
			ovsData:  bridgeWithUplink,
			intfName: "breth1",
			want:     "breth1",
		},
		{
			name:     "interface plugged into its bridge returns the bridge",
			ovsData:  bridgeWithUplink,
			intfName: "eth1",
			want:     "breth1",
		},
		{
			name:     "interface missing from its existing bridge returns the interface",
			ovsData:  bridgeWithoutUplink,
			intfName: "eth1",
			want:     "eth1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: test.ovsData})
			if err != nil {
				t.Fatalf("failed to create OVS test harness: %v", err)
			}
			t.Cleanup(cleanup.Cleanup)

			got, err := interfaceForEXGW(ovsClient, test.intfName)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != test.want {
				t.Fatalf("expected %q, got %q", test.want, got)
			}
		})
	}
}
