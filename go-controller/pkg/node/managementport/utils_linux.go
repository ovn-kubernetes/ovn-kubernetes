//go:build linux
// +build linux

package managementport

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"

	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// bringupManagementPortLink update the management port interface with the expected mac/name/mtu
func bringupManagementPortLink(netName string, link netlink.Link, macAddr *net.HardwareAddr, ifName string, mtu int) error {
	var err error

	// if this is called when deleting the management port, see if its original interface name is saved in the alias
	klog.V(5).Infof("Create management port link %s mac %+v current name %s mtu %v for network %s",
		ifName, macAddr, link.Attrs().Name, mtu, netName)

	setMac := false
	setMTU := false
	oldIfName := link.Attrs().Name
	setName := oldIfName != ifName
	if macAddr != nil {
		setMac = link.Attrs().HardwareAddr.String() != macAddr.String()
	}
	if mtu != 0 {
		setMTU = link.Attrs().MTU != mtu
	}
	if setMac || setName || setMTU {
		err = util.GetNetLinkOps().LinkSetDown(link)
		if err != nil {
			return fmt.Errorf("failed to set link down for management port %s for network %s: %v", oldIfName, netName, err)
		}

		if setMac {
			err = util.GetNetLinkOps().LinkSetHardwareAddr(link, *macAddr)
			if err != nil {
				return fmt.Errorf("failed to set MAC address %s for management port %s for network %s: %v", macAddr.String(), ifName, netName, err)
			}
		}

		if setName {
			err = util.GetNetLinkOps().LinkSetName(link, ifName)
			if err != nil {
				return fmt.Errorf("failed to rename management port name from %s to %s for network %s: %v", oldIfName, ifName, netName, err)
			}
			// when creating the management port, set the old link name as alias, it can then be used to renamed the link back.
			err = util.GetNetLinkOps().LinkSetAlias(link, oldIfName)
			if err != nil {
				return fmt.Errorf("failed to set alias %s on the renamed link %s for network %s: %v", oldIfName, ifName, netName, err)
			}
		}

		if setMTU {
			err = util.GetNetLinkOps().LinkSetMTU(link, mtu)
			if err != nil {
				return fmt.Errorf("failed to set MTU %d for management port %s for network %s: %v", mtu, ifName, netName, err)
			}
		}

	}
	// needs to bring the link up if this is to create the management port
	err = util.GetNetLinkOps().LinkSetUp(link)
	if err != nil {
		return fmt.Errorf("failed to set link up for management port %s for network %s: %v", ifName, netName, err)
	}
	return nil
}

// TearDownManagementPortLink tear down the management port interface, and rename back to the original link name if needed
func TearDownManagementPortLink(netName string, link netlink.Link, originalIfName string) error {
	attrs := link.Attrs()
	// check if the interface's original interface name is saved in the alias
	if attrs.Alias != "" {
		originalIfName = attrs.Alias
	}
	ifName := attrs.Name
	klog.V(5).Infof("Tear down management port link %s for network %s", ifName, netName)

	err := util.LinkAddrFlush(link)
	if err != nil {
		klog.Warningf("Failed to flush IP addresses on management port %s for network %s: %v", ifName, netName, err)
	}

	err = util.GetNetLinkOps().LinkSetDown(link)
	if err != nil {
		return fmt.Errorf("failed to set link down for management port %s for network %s: %v", ifName, netName, err)
	}

	if originalIfName != "" && ifName != originalIfName {
		klog.V(5).Infof("Restore the original management port name from %s to %s for network %s", ifName, originalIfName, netName)
		err = util.GetNetLinkOps().LinkSetName(link, originalIfName)
		if err != nil {
			return fmt.Errorf("failed to rename management port name from %s to %s for network %s: %v", ifName, originalIfName, netName, err)
		}
	}
	return nil
}

func createManagementPortOVSRepresentor(netName, ifname, ifaceid string, mtu int, externalIds []string) error {
	br_type, err := util.GetDatapathType("br-int")
	if err != nil {
		return fmt.Errorf("failed to get datapath type for bridge br-int : %v", err)
	}

	ovsArgs := []string{
		"--", "--may-exist", "add-port", "br-int", ifname,
		"--", "set", "interface", ifname,
		"external-ids:iface-id=" + ifaceid,
	}
	for _, v := range externalIds {
		ovsArgs = append(ovsArgs, fmt.Sprintf("external-ids:%s", v))
	}

	if br_type == types.DatapathUserspace {
		dpdkArgs := []string{"type=dpdk"}
		ovsArgs = append(ovsArgs, dpdkArgs...)
		ovsArgs = append(ovsArgs, fmt.Sprintf("mtu_request=%v", mtu))
	}

	klog.V(5).Infof("Add OVS representor OVS inteface %s to bridge br-int for network %s: ifaceID %s mtu %v externalIDs %v",
		ifname, netName, ifaceid, mtu, externalIds)
	// Plug management port representor to OVS.
	stdout, stderr, err := util.RunOVSVsctl(ovsArgs...)
	if err != nil {
		klog.Errorf("Failed to add port %q to br-int, stdout: %q, stderr: %q, error: %v",
			ifname, stdout, stderr, err)
		return err
	}
	return nil
}

// DeleteManagementPortOVSInterface delete the management port OVS interface from the br-int bridge:
func DeleteManagementPortOVSInterface(network, ovsIfName string) error {
	klog.V(5).Infof("Removed OVS management port OVS interface %s for network %s", ovsIfName, network)

	stdout, stderr, err := util.RunOVSVsctl("--if-exists", "del-port", "br-int", ovsIfName)
	if err != nil {
		return fmt.Errorf("failed to delete port %s from br-int for network %s, stdout: %q, stderr: %q, error: %v",
			ovsIfName, network, stdout, stderr, err)
	}
	return nil
}
