// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"fmt"
	"testing"

	"github.com/onsi/gomega"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// TestIsMacBindingControllerOwned covers the ownership predicate: a binding is
// owned only on a gateway router external port and only for a non-masquerade IP.
func TestIsMacBindingControllerOwned(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())

	gwPort := types.GWRouterToExtSwitchPrefix + "GR_udnA_node1"
	masqIP := config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String()

	tests := []struct {
		name string
		port string
		ip   string
		want bool
	}{
		{"gateway external port, regular IP is owned", gwPort, "10.0.0.10", true},
		{"gateway external port, gateway masquerade IP is not owned", gwPort, masqIP, false},
		{"non-gateway port, regular IP is not owned", "some-other-port", "10.0.0.10", false},
		{"non-gateway port, masquerade IP is not owned", "some-other-port", masqIP, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(IsMacBindingControllerOwned(tt.port, tt.ip)).To(gomega.Equal(tt.want))
		})
	}
}

// TestCreateOrUpdateStaticMacBinding verifies the guard rejects controller-owned
// bindings (which must go through the MAC binding controller) while still writing
// the gateway's own masquerade bindings.
func TestCreateOrUpdateStaticMacBinding(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())

	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer cleanup.Cleanup()

	gwPort := types.GWRouterToExtSwitchPrefix + "GR_udnA_node1"
	masqIP := config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String()

	// A controller-owned binding (gateway external port, non-masquerade IP) is rejected.
	err = CreateOrUpdateStaticMacBinding(nbClient, &nbdb.StaticMACBinding{
		LogicalPort: gwPort, IP: "10.0.0.10", MAC: "0a:00:00:00:00:01", OverrideDynamicMAC: true,
	})
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("managed by the MAC binding controller")))

	// The gateway's own masquerade binding on the same port is written.
	g.Expect(CreateOrUpdateStaticMacBinding(nbClient, &nbdb.StaticMACBinding{
		LogicalPort: gwPort, IP: masqIP, MAC: "0a:00:00:00:00:02", OverrideDynamicMAC: true,
	})).To(gomega.Succeed())
	g.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(
		&nbdb.StaticMACBinding{LogicalPort: gwPort, IP: masqIP, MAC: "0a:00:00:00:00:02", OverrideDynamicMAC: true},
	))
}

// TestStaticMACBindingWritesOnPopulatedPort covers a gateway router external
// port that already carries the node IP bindings the MAC binding controller
// programs when gateway init writes, or gateway cleanup deletes, the gateway's
// own dummy next hop bindings on the same port. Those are (logical_port, ip)
// lookups of a binding that may not exist; they must resolve against the
// table's composite index only. If a lookup fell back to a broader
// logical_port index it would match every node IP row and fail with
// errMultipleResults, aborting gateway init and stale network cleanup and
// leaving the network's static bindings orphaned.
func TestStaticMACBindingWritesOnPopulatedPort(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	// this mode is what used to register the logical_port client index that made
	// the lookups below over-match
	config.Gateway.DisableUDNARPNDPFlood = true
	t.Cleanup(func() { config.Gateway.DisableUDNARPNDPFlood = false })

	gwPort := types.GWRouterToExtSwitchPrefix + "GR_udnA_node1"
	nodeIPs := []string{"10.0.0.2", "10.0.0.3", "10.0.0.4", "fd00::2", "fd00::3", "fd00::4"}
	initialDB := make([]libovsdbtest.TestData, 0, len(nodeIPs))
	for i, ip := range nodeIPs {
		initialDB = append(initialDB, &nbdb.StaticMACBinding{
			UUID: fmt.Sprintf("node-ip-%d", i), LogicalPort: gwPort, IP: ip,
			MAC: "0a:00:00:00:00:01", OverrideDynamicMAC: true,
		})
	}

	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: initialDB}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer cleanup.Cleanup()

	dummyNextHopIPs := []string{
		config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP.String(),
		config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP.String(),
	}
	for _, ip := range dummyNextHopIPs {
		g.Expect(CreateOrUpdateStaticMacBinding(nbClient, &nbdb.StaticMACBinding{
			LogicalPort: gwPort, IP: ip, MAC: "0a:00:00:00:00:02", OverrideDynamicMAC: true,
		})).To(gomega.Succeed(), "must create the dummy next hop binding for %s on a port carrying node IP bindings", ip)
	}

	// deleting a binding that is not there is a no-op and must leave the rest of
	// the port's bindings alone
	g.Expect(DeleteStaticMacBindings(nbClient, &nbdb.StaticMACBinding{LogicalPort: gwPort, IP: dummyNextHopIPs[0]})).To(gomega.Succeed())
	g.Expect(DeleteStaticMacBindings(nbClient, &nbdb.StaticMACBinding{LogicalPort: gwPort, IP: "10.0.0.9"})).To(gomega.Succeed())

	expected := []libovsdbtest.TestData{
		&nbdb.StaticMACBinding{LogicalPort: gwPort, IP: dummyNextHopIPs[1], MAC: "0a:00:00:00:00:02", OverrideDynamicMAC: true},
	}
	for _, ip := range nodeIPs {
		expected = append(expected, &nbdb.StaticMACBinding{
			LogicalPort: gwPort, IP: ip, MAC: "0a:00:00:00:00:01", OverrideDynamicMAC: true,
		})
	}
	g.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expected...))
}
