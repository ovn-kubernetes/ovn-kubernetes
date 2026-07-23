// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package util

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

const (
	ubuntuDefaultFile = "/etc/default/openvswitch-switch"
	rhelDefaultFile   = "/etc/default/openvswitch"
)

func GetBridgeName(iface string) string {
	return fmt.Sprintf("br%s", iface)
}

// getBridgePortsInterfaces returns a mapping of bridge ports to their
// resolved Interface rows.
func getBridgePortsInterfaces(ovsClient libovsdbclient.Client, br *vswitchd.Bridge) (map[string][]*vswitchd.Interface, error) {
	portUUIDs := map[string]struct{}{}
	ifaceUUIDs := map[string]struct{}{}
	portToIfaceUUIDs := map[string][]string{}
	portsToInterfaces := make(map[string][]*vswitchd.Interface)
	for _, portUUID := range br.Ports {
		portUUIDs[portUUID] = struct{}{}
	}
	ports, err := ovsops.FindOVSPortsWithPredicate(ovsClient, func(port *vswitchd.Port) bool {
		_, ok := portUUIDs[port.UUID]
		return ok
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get ports on bridge %q: %w", br.Name, err)
	}
	portByUUID := make(map[string]*vswitchd.Port, len(ports))
	for _, port := range ports {
		portByUUID[port.UUID] = port
	}
	for _, portUUID := range br.Ports {
		port, ok := portByUUID[portUUID]
		if !ok {
			return nil, fmt.Errorf("failed to get port %s on bridge %q: %w", portUUID, br.Name, libovsdbclient.ErrNotFound)
		}
		for _, ifaceUUID := range port.Interfaces {
			ifaceUUIDs[ifaceUUID] = struct{}{}
		}
		portToIfaceUUIDs[port.Name] = port.Interfaces
	}

	ifaces, err := ovsops.FindInterfacesWithPredicate(ovsClient, func(iface *vswitchd.Interface) bool {
		_, ok := ifaceUUIDs[iface.UUID]
		return ok
	})
	if err != nil {
		return nil, err
	}

	ifaceByUUID := make(map[string]*vswitchd.Interface, len(ifaces))
	for _, iface := range ifaces {
		ifaceByUUID[iface.UUID] = iface
	}
	for portName, ifaceUUIDs := range portToIfaceUUIDs {
		portsToInterfaces[portName] = make([]*vswitchd.Interface, 0, len(ifaceUUIDs))
		for _, ifaceUUID := range ifaceUUIDs {
			iface, ok := ifaceByUUID[ifaceUUID]
			if !ok {
				return nil, fmt.Errorf("failed to get interface %s on port %q: %w", ifaceUUID, portName, libovsdbclient.ErrNotFound)
			}
			portsToInterfaces[portName] = append(portsToInterfaces[portName], iface)
		}
	}
	return portsToInterfaces, nil
}

// GetNicName returns the physical NIC name, given an OVS bridge name
// configured by NicToBridge()
func GetNicName(ovsClient libovsdbclient.Client, brName string) (string, error) {
	br, err := ovsops.GetBridge(ovsClient, brName)
	if err != nil {
		return "", fmt.Errorf("failed to get bridge %q: %w", brName, err)
	}

	if uplink := br.ExternalIDs["bridge-uplink"]; uplink != "" {
		return uplink, nil
	}

	// Check for system type port (required to be set if using NetworkManager)
	portsToInterfaces, err := getBridgePortsInterfaces(ovsClient, br)
	if err != nil {
		return "", err
	}

	systemPorts := make([]string, 0)
	for port, ifaces := range portsToInterfaces {
		for _, iface := range ifaces {
			if iface.Type == "system" {
				systemPorts = append(systemPorts, port)
			}
		}
	}
	if len(systemPorts) == 1 {
		return systemPorts[0], nil
	} else if len(systemPorts) > 1 {
		klog.Infof("Found more than one system Type ports on the OVS bridge %s, so skipping "+
			"this method of determining the uplink port", brName)
	}

	if strings.HasPrefix(brName, "br") {
		// This would happen if the bridge was created before the bridge-uplink
		// changes got integrated. Assuming naming format of "br<nic name>".
		return brName[len("br"):], nil
	}
	return "", fmt.Errorf("unable to resolve uplink for bridge %q: no system-typed port, no bridge-uplink external-id, and bridge name has no \"br\" prefix to strip", brName)
}

func saveIPAddress(oldLink, newLink netlink.Link, addrs []netlink.Addr) error {
	for i := range addrs {
		addr := addrs[i]

		if addr.IP.IsGlobalUnicast() {
			// Remove from oldLink
			if err := netLinkOps.AddrDel(oldLink, &addr); err != nil {
				klog.Errorf("Remove addr from %q failed: %v", oldLink.Attrs().Name, err)
				return err
			}

			// Add to newLink
			addr.Label = newLink.Attrs().Name
			if err := netLinkOps.AddrAdd(newLink, &addr); err != nil {
				klog.Errorf("Add addr %q to newLink %q failed: %v", addr.String(), addr.Label, err)
				return err
			}
			klog.Infof("Successfully saved addr %q to newLink %q", addr.String(), addr.Label)
		}
	}

	return netLinkOps.LinkSetUp(newLink)
}

// delAddRoute removes 'route' from 'oldLink' and moves to 'newLink'
func delAddRoute(oldLink, newLink netlink.Link, route netlink.Route) error {
	// Remove route from old interface
	if err := netLinkOps.RouteDel(&route); err != nil && !strings.Contains(err.Error(), "no such process") {
		klog.Errorf("Remove route from %q failed: %v", oldLink.Attrs().Name, err)
		return err
	}

	// Add route to newLink
	route.LinkIndex = newLink.Attrs().Index
	if err := netLinkOps.RouteAdd(&route); err != nil && !os.IsExist(err) {
		klog.Errorf("Add route to newLink %q failed: %v", newLink.Attrs().Name, err)
		return err
	}

	klog.Infof("Successfully saved route %q", route.String())
	return nil
}

func saveRoute(oldLink, newLink netlink.Link, routes []netlink.Route) error {
	for i := range routes {
		route := routes[i]

		// Handle routes for default gateway later.  This is a special case for
		// GCE where we have /32 IP addresses and we can't add the default
		// gateway before the route to the gateway.
		if IsNilOrAnyNetwork(route.Dst) && route.Gw != nil && route.LinkIndex > 0 {
			continue
		} else if route.Dst != nil && !route.Dst.IP.IsGlobalUnicast() {
			continue
		}

		err := delAddRoute(oldLink, newLink, route)
		if err != nil {
			return err
		}
	}

	// Now add the default gateway (if any) via this interface.
	for i := range routes {
		route := routes[i]
		if IsNilOrAnyNetwork(route.Dst) && route.Gw != nil && route.LinkIndex > 0 {
			// Remove route from 'oldLink' and move it to 'newLink'
			err := delAddRoute(oldLink, newLink, route)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func setupDefaultFile() {
	platform, err := runningPlatform()
	if err != nil {
		klog.Errorf("Failed to set OVS package default file (%v)", err)
		return
	}

	var defaultFile, text string
	if platform == ubuntu {
		defaultFile = ubuntuDefaultFile
		text = "OVS_CTL_OPTS=\"$OVS_CTL_OPTS --delete-transient-ports\""
	} else if platform == rhel {
		defaultFile = rhelDefaultFile
		text = "OPTIONS=--delete-transient-ports"
	} else {
		return
	}

	fileContents, err := os.ReadFile(defaultFile)
	if err != nil {
		klog.Warningf("Failed to parse file %s (%v)",
			defaultFile, err)
		return
	}

	ss := strings.Split(string(fileContents), "\n")
	for _, line := range ss {
		if strings.Contains(line, "--delete-transient-ports") {
			// Nothing to do
			return
		}
	}

	// The defaultFile does not contain '--delete-transient-ports' set.
	// We should set it.
	f, err := os.OpenFile(defaultFile, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		klog.Errorf("Failed to open %s to write (%v)", defaultFile, err)
		return
	}
	defer f.Close()

	if _, err = f.WriteString(text); err != nil {
		klog.Errorf("Failed to write to %s (%v)",
			defaultFile, err)
		return
	}
}

// NicToBridge creates a OVS bridge for the 'iface' and also moves the IP
// address and routes of 'iface' to OVS bridge.
func NicToBridge(ovsClient libovsdbclient.Client, iface string) (string, error) {
	ifaceLink, err := netLinkOps.LinkByName(iface)
	if err != nil {
		return "", err
	}

	bridge := GetBridgeName(iface)
	nicMAC := ifaceLink.Attrs().HardwareAddr.String()
	if err := ovsops.CreateOrUpdateNicBridge(ovsClient, bridge, iface, nicMAC); err != nil {
		klog.Errorf("Failed to create OVS bridge %q: %v", bridge, err)
		return "", err
	}
	klog.Infof("Successfully created OVS bridge %q", bridge)

	setupDefaultFile()

	// Get ip addresses and routes before any real operations.
	family := syscall.AF_UNSPEC
	addrs, err := netLinkOps.AddrList(ifaceLink, family)
	if err != nil {
		return "", err
	}
	routes, err := netLinkOps.RouteList(ifaceLink, family)
	if err != nil {
		return "", err
	}

	// Unlike `ovs-vsctl add-br`, the libovsdb transaction returns as soon as
	// the OVSDB row is committed — ovs-vswitchd may not yet have materialised
	// the kernel netdev or applied its configuration. Poll until the netdev
	// exists with the MAC address inherited from the NIC, so callers see the
	// same "bridge ready for use" semantics as the legacy shell-out. In
	// particular, the bridge must not be brought up before its MAC address is
	// set: the kernel would generate the IPv6 link-local address from the
	// initial random MAC address and would not regenerate it when the MAC
	// address changes on a live interface.
	var bridgeLink netlink.Link
	if err := wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, 10*time.Second, true, func(_ context.Context) (bool, error) {
		bridgeLink, err = netLinkOps.LinkByName(bridge)
		if err != nil {
			if netLinkOps.IsLinkNotFoundError(err) {
				// netdev hasn't materialised yet; keep polling.
				return false, nil
			}
			// Any other error (netlink socket issue, etc.) is not
			// going to be fixed by waiting — exit immediately.
			return false, err
		}
		if bridgeLink.Attrs().HardwareAddr.String() != nicMAC {
			// hwaddr not applied yet; keep polling.
			return false, nil
		}
		return true, nil
	}); err != nil {
		return "", fmt.Errorf("bridge %q netdev did not appear with the MAC address %s of NIC %q: %w", bridge, nicMAC, iface, err)
	}

	// save ip addresses to bridge.
	if err = saveIPAddress(ifaceLink, bridgeLink, addrs); err != nil {
		return "", err
	}

	// save routes to bridge.
	if err = saveRoute(ifaceLink, bridgeLink, routes); err != nil {
		return "", err
	}

	return bridge, nil
}

// BridgeToNic moves the IP address and routes of internal port of the bridge to
// underlying NIC interface and deletes the OVS bridge. Patch ports attached to
// the bridge have their peers on br-int removed first.
func BridgeToNic(ovsClient libovsdbclient.Client, bridge string) error {
	// Internal port is named same as the bridge
	bridgeLink, err := netLinkOps.LinkByName(bridge)
	if err != nil {
		return err
	}

	// Get ip addresses and routes before any real operations.
	family := syscall.AF_UNSPEC
	addrs, err := netLinkOps.AddrList(bridgeLink, family)
	if err != nil {
		return err
	}
	routes, err := netLinkOps.RouteList(bridgeLink, family)
	if err != nil {
		return err
	}

	nicName, err := GetNicName(ovsClient, bridge)
	if err != nil {
		return err
	}
	ifaceLink, err := netLinkOps.LinkByName(nicName)
	if err != nil {
		return err
	}

	// save ip addresses to iface.
	if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil {
		return err
	}

	// save routes to iface.
	if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil {
		return err
	}

	// For every patch interface on the bridge, find its peer interface and
	// delete the peer port from the integration bridge.
	br, err := ovsops.GetBridge(ovsClient, bridge)
	if err != nil {
		klog.Errorf("Failed to look up OVS bridge %q: %v", bridge, err)
		return err
	}
	var ops []ovsdb.Operation
	for _, portUUID := range br.Ports {
		port := &vswitchd.Port{UUID: portUUID}
		ctx, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)
		err := ovsClient.Get(ctx, port)
		cancel()
		if err != nil {
			klog.Warningf("Failed to look up Port %s on bridge %q: %v", portUUID, bridge, err)
			continue
		}
		for _, ifaceUUID := range port.Interfaces {
			iface := &vswitchd.Interface{UUID: ifaceUUID}
			ctx, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)
			err := ovsClient.Get(ctx, iface)
			cancel()
			if err != nil {
				klog.Warningf("Failed to look up Interface %s on port %q: %v", ifaceUUID, port.Name, err)
				continue
			}
			if iface.Type != "patch" {
				continue
			}
			peer := iface.Options["peer"]
			if peer == "" {
				klog.Warningf("Patch interface %q has no peer option", iface.Name)
				continue
			}
			peerPort, err := ovsops.GetOVSPort(ovsClient, peer)
			if err != nil {
				if !errors.Is(err, libovsdbclient.ErrNotFound) {
					klog.Warningf("Failed to look up patch peer port %q: %v", peer, err)
				}
				continue
			}
			peerBridge, err := ovsops.GetPortBridge(ovsClient, peer)
			if err != nil {
				if !errors.Is(err, libovsdbclient.ErrNotFound) {
					klog.Warningf("Failed to look up bridge for patch peer port %q: %v", peer, err)
				}
				continue
			}
			if peerBridge.Name != "br-int" {
				klog.Warningf("Patch peer port %q is on bridge %q, not br-int", peer, peerBridge.Name)
				continue
			}
			ops, err = ovsops.DeletePortWithInterfacesOps(ovsClient, ops, peerPort, "br-int")
			if err != nil {
				klog.Warningf("Failed to build delete operations for patch port %q on br-int: %v", peer, err)
			}
		}
	}

	// Now delete the bridge
	ops, err = ovsops.DeleteBridgeOps(ovsClient, ops, bridge)
	if err != nil {
		klog.Errorf("Failed to build delete operations for OVS bridge %q: %v", bridge, err)
		return err
	}
	if _, err := ovsops.TransactAndCheck(ovsClient, ops); err != nil {
		klog.Errorf("Failed to delete OVS bridge %q: %v", bridge, err)
		return err
	}
	klog.Infof("Successfully deleted OVS bridge %q", bridge)
	return nil
}
