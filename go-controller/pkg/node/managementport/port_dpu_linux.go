//go:build linux
// +build linux

package managementport

import (
	"fmt"
	"net"
	"time"

	"github.com/vishvananda/netlink"

	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type managementPortRepresentor struct {
	cfg        *managementPortConfig
	ifName     string
	repDevName string
	link       netlink.Link
}

// newManagementPortRepresentor creates a new managementPortRepresentor
func newManagementPortRepresentor(name, repDevName string, cfg *managementPortConfig) *managementPortRepresentor {
	return &managementPortRepresentor{
		cfg:        cfg,
		ifName:     name,
		repDevName: repDevName,
	}
}

func (mp *managementPortRepresentor) create() error {
	br_type, err := util.GetDatapathType("br-int")
	if err != nil {
		return fmt.Errorf("failed to get datapath type for bridge br-int : %v", err)
	}

	klog.V(5).Infof("Lookup representor link and existing management port for '%v'", mp.repDevName)
	// Get management port representor netdevice
	link, err := util.GetNetLinkOps().LinkByName(mp.repDevName)
	if err != nil {
		return err
	}

	if link.Attrs().Name != mp.ifName {
		if err := syncMgmtPortInterface(mp.ifName, false); err != nil {
			return fmt.Errorf("failed to check existing management port: %v", err)
		}
	}

	// configure management port: rename, set MTU and set link up and connect representor port to br-int
	klog.V(5).Infof("Setup representor management port: %s", link.Attrs().Name)

	setName := link.Attrs().Name != mp.ifName
	setMTU := link.Attrs().MTU != config.Default.MTU

	if setName || setMTU {
		if err = util.GetNetLinkOps().LinkSetDown(link); err != nil {
			return fmt.Errorf("failed to set link down for device %s. %v", mp.repDevName, err)
		}

		if setName {
			if err = util.GetNetLinkOps().LinkSetName(link, mp.ifName); err != nil {
				return fmt.Errorf("failed to set link name for device %s. %v", mp.repDevName, err)
			}
		}

		if setMTU {
			if err = util.GetNetLinkOps().LinkSetMTU(link, config.Default.MTU); err != nil {
				return fmt.Errorf("failed to set link MTU for device %s. %v", link.Attrs().Name, err)
			}
		}
	}

	if err = util.GetNetLinkOps().LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to set link up for device %s. %v", link.Attrs().Name, err)
	}

	ovsArgs := []string{
		"--", "--may-exist", "add-port", "br-int", mp.ifName,
		"--", "set", "interface", mp.ifName,
		"external-ids:iface-id=" + types.K8sPrefix + mp.cfg.nodeName,
	}
	if mp.repDevName != mp.ifName {
		ovsArgs = append(ovsArgs, "external-ids:ovn-orig-mgmt-port-rep-name="+mp.repDevName)
	}

	if br_type == types.DatapathUserspace {
		dpdkArgs := []string{"type=dpdk"}
		ovsArgs = append(ovsArgs, dpdkArgs...)
		ovsArgs = append(ovsArgs, fmt.Sprintf("mtu_request=%v", config.Default.MTU))
	}

	// Plug management port representor to OVS.
	stdout, stderr, err := util.RunOVSVsctl(ovsArgs...)
	if err != nil {
		klog.Errorf("Failed to add port %q to br-int, stdout: %q, stderr: %q, error: %v",
			mp.ifName, stdout, stderr, err)
		return err
	}

	mp.link = link
	return nil
}

func (mp *managementPortRepresentor) checkRepresentorPortHealth() error {
	// After host reboot, management port link name changes back to default name.
	link, err := util.GetNetLinkOps().LinkByName(mp.ifName)
	if err != nil {
		klog.Warningf("Failed to get link device %s: %v", mp.ifName, err)
		// Get management port representor by name
		link, err := util.GetNetLinkOps().LinkByName(mp.repDevName)
		if err != nil {
			return fmt.Errorf("failed to get link device %s: %w", mp.repDevName, err)
		}
		if err = util.GetNetLinkOps().LinkSetDown(link); err != nil {
			return fmt.Errorf("failed to set link down for device %s: %w", mp.repDevName, err)
		}
		if err = util.GetNetLinkOps().LinkSetName(link, mp.ifName); err != nil {
			return fmt.Errorf("failed to rename link from %s to %s: %w", mp.repDevName, mp.ifName, err)
		}
		if link.Attrs().MTU != config.Default.MTU {
			if err = util.GetNetLinkOps().LinkSetMTU(link, config.Default.MTU); err != nil {
				return fmt.Errorf("failed to set link MTU for device %s: %w", mp.ifName, err)
			}
		}
		if err = util.GetNetLinkOps().LinkSetUp(link); err != nil {
			return fmt.Errorf("failed to set link up for device %s: %w", mp.ifName, err)
		}
		mp.link = link
	} else if (link.Attrs().Flags & net.FlagUp) != net.FlagUp {
		if err = util.GetNetLinkOps().LinkSetUp(link); err != nil {
			return fmt.Errorf("failed to set link up for device %s: %w", mp.ifName, err)
		}
	}
	return nil
}

func (mp *managementPortRepresentor) reconcilePeriod() time.Duration {
	return 5 * time.Second
}

func (mp *managementPortRepresentor) doReconcile() error {
	return mp.checkRepresentorPortHealth()
}

type managementPortNetdev struct {
	ifName        string
	netdevDevName string
	cfg           *managementPortConfig
	routeManager  *routemanager.Controller
}

// newManagementPortNetdev creates a new managementPortNetdev
func newManagementPortNetdev(netdevDevName string, cfg *managementPortConfig, routeManager *routemanager.Controller) *managementPortNetdev {
	return &managementPortNetdev{
		ifName:        types.K8sMgmtIntfName,
		netdevDevName: netdevDevName,
		cfg:           cfg,
		routeManager:  routeManager,
	}
}

func (mp *managementPortNetdev) create() error {
	klog.V(5).Infof("Lookup netdevice link and existing management port using '%v'", mp.netdevDevName)
	link, err := util.GetNetLinkOps().LinkByName(mp.netdevDevName)
	if err != nil {
		return err
	}

	if link.Attrs().Name != mp.ifName {
		err = syncMgmtPortInterface(mp.ifName, false)
		if err != nil {
			return fmt.Errorf("failed to sync management port: %v", err)
		}
	}

	// configure management port: name, mac, MTU, iptables
	// mac addr, derived from the first entry in host subnets using the .2 address as mac with a fixed prefix.
	klog.V(5).Infof("Setup netdevice management port: %s", link.Attrs().Name)
	mgmtPortMac := util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(mp.cfg.hostSubnets[0]).IP)
	setMac := link.Attrs().HardwareAddr.String() != mgmtPortMac.String()
	setName := link.Attrs().Name != mp.ifName
	setMTU := link.Attrs().MTU != config.Default.MTU

	if setMac || setName || setMTU {
		err := util.GetNetLinkOps().LinkSetDown(link)
		if err != nil {
			return fmt.Errorf("failed to set link down for %s. %v", mp.netdevDevName, err)
		}

		if setMac {
			err := util.GetNetLinkOps().LinkSetHardwareAddr(link, mgmtPortMac)
			if err != nil {
				return fmt.Errorf("failed to set management port MAC address. %v", err)
			}
		}

		if setName {
			err := util.GetNetLinkOps().LinkSetName(link, mp.ifName)
			if err != nil {
				return fmt.Errorf("failed to set management port name. %v", err)
			}
		}

		if setMTU {
			err := util.GetNetLinkOps().LinkSetMTU(link, config.Default.MTU)
			if err != nil {
				return fmt.Errorf("failed to set management port MTU. %v", err)
			}
		}
	}

	if mp.netdevDevName != mp.ifName && config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		// Store original interface name for later use
		if _, stderr, err := util.RunOVSVsctl("set", "Open_vSwitch", ".",
			"external-ids:ovn-orig-mgmt-port-netdev-name="+mp.netdevDevName); err != nil {
			return fmt.Errorf("failed to store original mgmt port interface name: %s", stderr)
		}
	}

	// Set link up
	err = util.GetNetLinkOps().LinkSetUp(link)
	if err != nil {
		return fmt.Errorf("failed to set link up for %s. %v", mp.ifName, err)
	}

	// Setup Iptable and routes
	err = createPlatformManagementPort(mp.ifName, mp.cfg, mp.routeManager)
	if err != nil {
		return err
	}
	return nil
}

func (mp *managementPortNetdev) reconcilePeriod() time.Duration {
	return 30 * time.Second
}

func (mp *managementPortNetdev) doReconcile() error {
	return createPlatformManagementPort(mp.ifName, mp.cfg, mp.routeManager)
}
