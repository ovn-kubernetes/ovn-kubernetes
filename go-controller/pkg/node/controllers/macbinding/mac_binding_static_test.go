// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package macbinding

import (
	"testing"

	"github.com/onsi/gomega"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

const (
	// nodeIPSource is the CDN GR external port, the designated source of the
	// default ("") uplink whose followers carry the node-IP static bindings.
	nodeIPSource = "rtoe-GR_node1"
)

// smb builds a static MAC binding row as the controller writes it
// (override_dynamic_mac always set). Expected rows carry no UUID and are matched
// with HaveDataIgnoringUUIDs.
func smb(port, ip, mac string) *nbdb.StaticMACBinding {
	return &nbdb.StaticMACBinding{LogicalPort: port, IP: ip, MAC: mac, OverrideDynamicMAC: true}
}

// TestSyncStaticMacBindingsToFollower verifies a follower is written the whole
// cached node-IP set for its source's uplink, and that a source with no known
// uplink writes nothing.
func TestSyncStaticMacBindingsToFollower(t *testing.T) {
	g := gomega.NewWithT(t)
	config.Gateway.DisableUDNARPNDPFlood = true
	const follower = "rtoe-GR_udnA_node1"

	nbClient, nbCleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer nbCleanup.Cleanup()

	c := newTestController()
	c.nbClient = nbClient
	c.cdnGatewayPort = nodeIPSource
	c.nodeIPs[""] = map[string]string{"10.0.0.10": nodeIPMAC1, "10.0.0.11": nodeIPMAC2}

	// The follower catches up on the whole set for its source's ("") uplink.
	g.Expect(c.syncStaticMacBindingsToFollower(nodeIPSource, follower)).To(gomega.Succeed())
	g.Eventually(c.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(
		smb(follower, "10.0.0.10", nodeIPMAC1),
		smb(follower, "10.0.0.11", nodeIPMAC2),
	))

	// A source that is not designated for any uplink is a no-op (no new rows).
	g.Expect(c.syncStaticMacBindingsToFollower("rtoe-GR_notasource_node1", "rtoe-GR_udnB_node1")).To(gomega.Succeed())
	g.Consistently(c.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(
		smb(follower, "10.0.0.10", nodeIPMAC1),
		smb(follower, "10.0.0.11", nodeIPMAC2),
	))
}

// TestDeleteStaticMacBindingsForFollower verifies that a port which stopped being
// a follower has every static binding reaped except the gateway masquerade IPs:
// a port that is no longer a follower owns nothing, so the node-IP cache is
// irrelevant and only the gateway-owned rows survive. The reap is scoped to the
// port, so other ports' bindings are untouched.
func TestDeleteStaticMacBindingsForFollower(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.Gateway.DisableUDNARPNDPFlood = true
	const (
		port    = "rtoe-GR_udnA_node1"
		nodeIP1 = "10.0.0.10"     // a node IP still in the cache
		nodeIP2 = "10.0.0.11"     // a node IP no longer in the cache
		otherIP = "10.1.0.10"     // an owned binding for a stale IP (reaped)
		masqIP  = "169.254.169.2" // the gateway's host masquerade IP (allow-listed)
		// other is a different port, whose binding the port-scoped reap must not touch.
		other       = "rtoe-GR_udnB_node1"
		otherPortIP = "10.0.0.20"
	)

	nbClient, nbCleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
		&nbdb.StaticMACBinding{UUID: "u1", LogicalPort: port, IP: nodeIP1, MAC: nodeIPMAC1, OverrideDynamicMAC: true},
		&nbdb.StaticMACBinding{UUID: "u2", LogicalPort: port, IP: nodeIP2, MAC: nodeIPMAC2, OverrideDynamicMAC: true},
		&nbdb.StaticMACBinding{UUID: "u3", LogicalPort: port, IP: otherIP, MAC: nodeIPMAC3, OverrideDynamicMAC: true},
		&nbdb.StaticMACBinding{UUID: "u4", LogicalPort: port, IP: masqIP, MAC: nodeIPMAC3, OverrideDynamicMAC: true},
		&nbdb.StaticMACBinding{UUID: "u5", LogicalPort: other, IP: otherPortIP, MAC: nodeIPMAC1, OverrideDynamicMAC: true},
	}}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer nbCleanup.Cleanup()

	c := newTestController()
	c.nbClient = nbClient
	// the cache is irrelevant for a removed follower: it owns nothing, so the
	// whole set except the gateway masquerade IP is reaped.
	c.nodeIPs[""] = map[string]string{nodeIP1: nodeIPMAC1}

	g.Expect(c.deleteStaticMacBindingsForFollower(port)).To(gomega.Succeed())
	// only the gateway masquerade row on port survives; the other port is untouched.
	g.Eventually(c.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(
		smb(port, masqIP, nodeIPMAC3),
		smb(other, otherPortIP, nodeIPMAC1),
	))
}

// repairTestNBData seeds a gateway router owning the external ports of the
// CDN, of a network the controller tracks and of a network the network manager
// has not synced yet, plus the given static MAC bindings. The external port of
// a network deleted while the controller was down has no logical router port.
func repairTestNBData(cdnPort, udnPort, unsyncedPort string, smbs ...*nbdb.StaticMACBinding) []libovsdbtest.TestData {
	data := []libovsdbtest.TestData{
		&nbdb.LogicalRouter{UUID: "gr", Name: "GR_node1", Ports: []string{"lrp-cdn", "lrp-udn", "lrp-unsynced"}},
		&nbdb.LogicalRouterPort{UUID: "lrp-cdn", Name: cdnPort},
		&nbdb.LogicalRouterPort{UUID: "lrp-udn", Name: udnPort},
		&nbdb.LogicalRouterPort{UUID: "lrp-unsynced", Name: unsyncedPort},
	}
	for _, smb := range smbs {
		data = append(data, smb)
	}
	return data
}

// TestRepairStaticMacBindings verifies the startup sweep reaps every
// controller-owned binding on a gateway external port whose logical router
// port no longer exists - a network deleted while the controller was down -
// however many there are, and leaves alone bindings on live ports (including
// ports of networks the network manager has not synced yet), the gateway's own
// masquerade IPs and non-gateway ports.
func TestRepairStaticMacBindings(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "must prepare the test config")
	config.Gateway.DisableUDNARPNDPFlood = true

	cdnPort := cdnPortFor("node1")
	const (
		udnPort      = "rtoe-GR_tenantred_node1"  // a tracked network's port
		unsyncedPort = "rtoe-GR_tenantblue_node1" // a live network not synced yet
		orphanPort   = "rtoe-GR_gone_node1"       // a network deleted while we were down
		nonGWPort    = "some-lsp"                 // not a gateway external port
	)
	masqIP := config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String()
	masqIPv6 := config.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String()
	row := func(uuid, port, ip, mac string) *nbdb.StaticMACBinding {
		return &nbdb.StaticMACBinding{UUID: uuid, LogicalPort: port, IP: ip, MAC: mac, OverrideDynamicMAC: true}
	}
	kept := []*nbdb.StaticMACBinding{
		// owned bindings on live gateway ports survive
		row("u1", cdnPort, "10.0.0.10", nodeIPMAC1),
		row("u2", udnPort, "10.0.0.11", nodeIPMAC2),
		row("u7", unsyncedPort, "10.0.0.15", nodeIPMAC2),
		// the gateway's masquerade IPs are not owned, even on the orphan port
		row("u4", orphanPort, masqIP, nodeIPMAC3),
		row("u9", orphanPort, masqIPv6, nodeIPMAC3),
		// a non-gateway port is not owned
		row("u5", nonGWPort, "10.0.0.13", nodeIPMAC1),
	}
	// every owned binding on the orphan gateway port is reaped
	orphans := []*nbdb.StaticMACBinding{
		row("u3", orphanPort, "10.0.0.12", nodeIPMAC3),
		row("u6", orphanPort, "10.0.0.14", nodeIPMAC1),
		row("u8", orphanPort, "fd00::12", nodeIPMAC2),
	}

	nbClient, nbCleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
		NBData: repairTestNBData(cdnPort, udnPort, unsyncedPort, append(append([]*nbdb.StaticMACBinding{}, kept...), orphans...)...),
	}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "must create the NB test harness with orphan bindings")
	defer nbCleanup.Cleanup()

	c := newTestController()
	c.nbClient = nbClient

	g.Expect(c.repairStaticMacBindings()).To(gomega.Succeed(), "repairStaticMacBindings must succeed")

	g.Eventually(c.nbClient).Should(libovsdbtest.HaveData(repairTestNBData(cdnPort, udnPort, unsyncedPort, kept...)),
		"repairStaticMacBindings must delete the orphan bindings and keep the rest")
}

// TestRepairStaticMacBindingsNothingStale verifies the sweep is a no-op when
// every owned binding sits on a live gateway port.
func TestRepairStaticMacBindingsNothingStale(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "must prepare the test config")
	config.Gateway.DisableUDNARPNDPFlood = true

	cdnPort := cdnPortFor("node1")
	const (
		udnPort      = "rtoe-GR_tenantred_node1"
		unsyncedPort = "rtoe-GR_tenantblue_node1"
	)
	live := &nbdb.StaticMACBinding{UUID: "u1", LogicalPort: udnPort, IP: "10.0.0.11", MAC: nodeIPMAC2, OverrideDynamicMAC: true}
	nbClient, nbCleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
		NBData: repairTestNBData(cdnPort, udnPort, unsyncedPort, live),
	}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "must create the NB test harness with only live bindings")
	defer nbCleanup.Cleanup()

	c := newTestController()
	c.nbClient = nbClient

	g.Expect(c.repairStaticMacBindings()).To(gomega.Succeed(), "repairStaticMacBindings must succeed")
	g.Consistently(c.nbClient).Should(libovsdbtest.HaveData(repairTestNBData(cdnPort, udnPort, unsyncedPort, live)),
		"repairStaticMacBindings must not delete bindings on live ports")
}

// TestRepairStaticMacBindingsFailure verifies a sweep that cannot delete the
// stale bindings reports the error, so the caller can retry it.
func TestRepairStaticMacBindingsFailure(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "must prepare the test config")
	config.Gateway.DisableUDNARPNDPFlood = true

	cdnPort := cdnPortFor("node1")
	orphan := &nbdb.StaticMACBinding{UUID: "u1", LogicalPort: "rtoe-GR_gone_node1", IP: "10.0.0.12", MAC: nodeIPMAC3, OverrideDynamicMAC: true}
	nbClient, nbCleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
		NBData: repairTestNBData(cdnPort, "rtoe-GR_tenantred_node1", "rtoe-GR_tenantblue_node1", orphan),
	}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "must create the NB test harness with an orphan binding")
	defer nbCleanup.Cleanup()

	c := newTestController()
	c.nbClient = nbClient
	// the cache keeps serving reads but no transaction can go through.
	nbClient.Close()

	g.Expect(c.repairStaticMacBindings()).To(gomega.MatchError(gomega.ContainSubstring("failed to delete stale static MAC bindings")),
		"repairStaticMacBindings must report that it could not delete the orphan binding")
}
