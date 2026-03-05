//go:build linux
// +build linux

package managementport

import (
	"fmt"
	"net"
	"time"

	"github.com/vishvananda/netlink"

	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type managementPortRepresentor struct {
	cfg        *managementPortConfig
	ifName     string
	repDevName string
	link       netlink.Link
}

// newManagementPortRepresentor creates a new managementPort representor
// For management port representor only.
// name is types.K8sMgmtIntfName (on dpu mode node) or types.K8sMgmtIntfName+"_0" (on full mode)
// repDevName is the representor VF device name
func newManagementPortRepresentor(name, repDevName string, cfg *managementPortConfig) *managementPortRepresentor {
	return &managementPortRepresentor{
		cfg:        cfg,
		ifName:     name,
		repDevName: repDevName,
	}
}

func (mp *managementPortRepresentor) create() error {
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

	klog.V(5).Infof("Setup representor management port: %s", link.Attrs().Name)
	// configure management port: rename representor device to specified management port name, set MTU and bring the link up
	err = bringupManagementPortLink(types.DefaultNetworkName, link, nil, mp.ifName, config.Default.MTU)
	if err != nil {
		return fmt.Errorf("update management port for default network failed: %v", err)
	}
	// connect representor port to br-int, set OvnManagementPortNameExternalID external-id to indicate its
	// associated network name and management port device name
	externalIDs := []string{fmt.Sprintf("%s=%s", types.OvnManagementPortNameExternalID, types.K8sMgmtIntfName)}
	if mp.repDevName != mp.ifName {
		externalIDs = append(externalIDs, fmt.Sprintf("ovn-orig-mgmt-port-rep-name=%s", mp.repDevName))
	}
	err = createManagementPortOVSRepresentor(types.DefaultNetworkName, mp.ifName, types.K8sPrefix+mp.cfg.nodeName, config.Default.MTU, externalIDs)
	if err != nil {
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
		err = bringupManagementPortLink(types.DefaultNetworkName, link, nil, mp.ifName, config.Default.MTU)
		if err != nil {
			return err
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
	err = bringupManagementPortLink(types.DefaultNetworkName, link, &mgmtPortMac, mp.ifName, config.Default.MTU)
	if err != nil {
		return err
	}

	if mp.netdevDevName != mp.ifName && config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		// Store original interface name for later use
		if _, stderr, err := util.RunOVSVsctl("set", "Open_vSwitch", ".",
			"external-ids:ovn-orig-mgmt-port-netdev-name="+mp.netdevDevName); err != nil {
			return fmt.Errorf("failed to store original mgmt port interface name: %s", stderr)
		}
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
