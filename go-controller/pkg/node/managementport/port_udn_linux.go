package managementport

import (
	"fmt"
	"net"

	v1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type udnManagementPort interface {
	create() error
	delete() error
}

type UDNManagementPortController struct {
	cfg   *udnManagementPortConfig
	ports map[string]udnManagementPort
}

type udnManagementPortConfig struct {
	util.NetInfo
	node    *corev1.Node // TBD-merge lower case?
	subnets []*net.IPNet
	mpMAC   net.HardwareAddr
}

func newUDNManagementPortConfig(node *corev1.Node, networkLocalSubnets []*net.IPNet, netInfo util.NetInfo) (*udnManagementPortConfig, error) {
	if len(networkLocalSubnets) == 0 {
		return nil, fmt.Errorf("cannot determine subnets while configuring management port for network: %s", netInfo.GetNetworkName())
	}

	return &udnManagementPortConfig{
		NetInfo: netInfo,
		subnets: networkLocalSubnets,
		node:    node,
		mpMAC:   util.IPAddrToHWAddr(netInfo.GetNodeManagementIP(networkLocalSubnets[0]).IP),
	}, nil
}

func (c *UDNManagementPortController) Create() error {
	for _, port := range c.ports {
		err := port.create()
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *UDNManagementPortController) Delete() error {
	for _, port := range c.ports {
		err := port.delete()
		if err != nil {
			return err
		}
	}
	return nil
}

// NewUDNManagementPortControlle creates a new ManagementPorts for a primary UDN
func NewUDNManagementPortController(
	node *corev1.Node,
	networkLocalSubnets []*net.IPNet,
	netInfo util.NetInfo,
) (*UDNManagementPortController, error) {
	var mpcfg *util.NetworkDeviceDetails

	mgmtIfName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(netInfo.GetNetworkID()))

	cfg, err := newUDNManagementPortConfig(node, networkLocalSubnets, netInfo)
	if err != nil {
		return nil, err
	}

	// cleanup stale OVS management port entities created in different mode/configuration
	err = syncUDNManagementPort(cfg, mgmtIfName)
	if err != nil {
		return nil, err
	}

	c := &UDNManagementPortController{
		cfg:   cfg,
		ports: map[string]udnManagementPort{},
	}

	if config.OvnKubeNode.MgmtPortDPResourceName == "" {
		if config.OvnKubeNode.Mode == types.NodeModeFull {
			c.ports[ovsPort] = newUDNManagementPortOVS(cfg, mgmtIfName)
		} else {
			return nil, fmt.Errorf("MgmtPortDPResourceName must be set for %s mode", config.OvnKubeNode.Mode)
		}
	} else {
		mpcfgs, err := util.ParseNodeManagementPortAnnotation(node)
		if err != nil {
			return nil, err
		}
		mpcfg = mpcfgs[netInfo.GetNetworkName()]
		if mpcfg == nil {
			return nil, fmt.Errorf("failed to find management port defails for network %s", netInfo.GetNetworkName())
		}

		switch config.OvnKubeNode.Mode {
		case types.NodeModeFull:
			repDeviceName, err := util.GetFunctionRepresentorName(mpcfg.DeviceId)
			if err != nil {
				return nil, fmt.Errorf("failed to get management port representor name for device %s network %s",
					mpcfg.DeviceId, netInfo.GetNetworkName())
			}
			c.ports[netdevPort] = newUDNManagementPortNetdev(cfg, mgmtIfName, mpcfg.DeviceId)
			c.ports[representorPort] = newUDNManagementPortRep(cfg, repDeviceName)
		case types.NodeModeDPU:
			repDeviceName, err := util.GetSriovnetOps().GetVfRepresentorDPU(fmt.Sprintf("%d", mpcfg.PfId), fmt.Sprintf("%d", mpcfg.FuncId))
			if err != nil {
				return nil, fmt.Errorf("failed to get management port representor for pfID %v vfID %v network %s",
					mpcfg.PfId, mpcfg.FuncId, netInfo.GetNetworkName())
			}
			c.ports[representorPort] = newUDNManagementPortRep(cfg, repDeviceName)
		case types.NodeModeDPUHost:
			c.ports[netdevPort] = newUDNManagementPortNetdev(cfg, mgmtIfName, mpcfg.DeviceId)
		}
	}

	return c, nil
}

type udnManagementPortOVS struct {
	udnManagementPortConfig
	ifName string
}

type udnManagementPortNetdev struct {
	udnManagementPortConfig
	ifName   string
	deviceID string
}

type udnManagementPortRep struct {
	udnManagementPortConfig
	repDevice string
}

// newManagementPortUDNOVS creates a new managementPortUDNOVS
func newUDNManagementPortOVS(cfg *udnManagementPortConfig, ifName string) *udnManagementPortOVS {
	return &udnManagementPortOVS{
		udnManagementPortConfig: *cfg,
		ifName:                  ifName,
	}
}

// newUDNManagementPortNetdev creates a new udnManagementPortNetdev
func newUDNManagementPortNetdev(cfg *udnManagementPortConfig, ifName, deviceID string) *udnManagementPortNetdev {
	return &udnManagementPortNetdev{
		udnManagementPortConfig: *cfg,
		ifName:                  ifName,
		deviceID:                deviceID,
	}
}

// newUdnManagementPortRep creates a new udnManagementPortRep
func newUDNManagementPortRep(cfg *udnManagementPortConfig, repDeviceName string) *udnManagementPortRep {
	return &udnManagementPortRep{
		udnManagementPortConfig: *cfg,
		repDevice:               repDeviceName,
	}
}

// syncUDNManagementPort is to delete stale UDN management port entities created when the node was in different configuration/mode
func syncUDNManagementPort(cfg *udnManagementPortConfig, mgmtIfName string) error {
	var err error
	// representor OVS interface
	ovsRepIfName, _, _ := util.RunOVSVsctl("--no-headings",
		"--data", "bare",
		"--format", "csv",
		"--columns", "name",
		"find", "Interface", fmt.Sprintf("external-ids:%s=%s", types.OvnManagementPortNameExternalId, mgmtIfName))
	// internal OVS interface
	ovsInternalIfName, _, _ := util.RunOVSVsctl("--no-headings",
		"--data", "bare",
		"--format", "csv",
		"--columns", "name",
		"find", "Interface", "type=internal", fmt.Sprintf("name=%s", mgmtIfName))

	if config.OvnKubeNode.MgmtPortDPResourceName == "" && config.OvnKubeNode.Mode == types.NodeModeFull {
		// expect internal OVS management port interface
		if ovsRepIfName != "" {
			klog.V(5).Infof("Expected management port OVS internal interface, delete stale management port representor %s for network %s",
				ovsRepIfName, cfg.GetNetworkName())
			// first delete representor if exists
			err = DeleteManagementPortOVSInterface(cfg.GetNetworkName(), ovsRepIfName)
			if err != nil {
				klog.Errorf("Failed to delete OVS representor port interface %s for network %s: %v", ovsRepIfName, cfg.GetNetworkName(), err)
			}
			link, _ := util.GetNetLinkOps().LinkByName(ovsRepIfName)
			if link != nil {
				err = TearDownManagementPortLink(cfg.GetNetworkName(), link, "")
				if err != nil {
					klog.Errorf("Failed to delete OVS representor port interface %s for network %s: %v", ovsRepIfName, cfg.GetNetworkName(), err)
				}
			}
		}
		if ovsInternalIfName == "" {
			klog.V(5).Infof("Expected management port OVS internal interface, bring down stale management port netdev interface %s for network %s",
				mgmtIfName, cfg.GetNetworkName())
			link, _ := util.GetNetLinkOps().LinkByName(mgmtIfName)
			if link != nil {
				err = TearDownManagementPortLink(cfg.GetNetworkName(), link, "")
				if err != nil {
					klog.Errorf("Failed to bring down OVS netdev interface %s for network %s: %v", mgmtIfName, cfg.GetNetworkName(), err)
				}
			}
		}
	} else if config.OvnKubeNode.MgmtPortDPResourceName != "" && config.OvnKubeNode.Mode != types.NodeModeDPU {
		if ovsInternalIfName != "" {
			klog.V(5).Infof("Expected management port OVS netdev interface, bring down stale management port internal interface %s for network %s",
				mgmtIfName, cfg.GetNetworkName())
			err = DeleteManagementPortOVSInterface(cfg.GetNetworkName(), mgmtIfName)
			if err != nil {
				klog.Errorf("Failed to delete OVS internal interface %s for network %s: %v", mgmtIfName, cfg.GetNetworkName(), err)
			}
		}
	}

	return nil
}

// udnManagementPortOVS.create does the following:
// STEP1: creates the (netdevice) OVS interface on br-int for the UDN's management port
// STEP2: sets up the management port link on the host
// STEP3: enables IPv4 forwarding on the interface if the network has a v4 subnet
// Returns a netlink Link which is the UDN management port interface along with its MAC address
func (mp *udnManagementPortOVS) create() error {
	// STEP1
	stdout, stderr, err := util.RunOVSVsctl(
		"--", "--may-exist", "add-port", "br-int", mp.ifName,
		"--", "set", "interface", mp.ifName, fmt.Sprintf("mac=\"%s\"", mp.mpMAC.String()),
		"type=internal", "mtu_request="+fmt.Sprintf("%d", mp.MTU()),
		"external-ids:iface-id="+mp.GetNetworkScopedK8sMgmtIntfName(mp.node.Name),
		"external-ids:"+fmt.Sprintf("%s=%s", types.NetworkExternalID, mp.GetNetworkName()),
	)
	if err != nil {
		return fmt.Errorf("failed to add port to br-int for network %s, stdout: %q, stderr: %q, error: %w",
			mp.GetNetworkName(), stdout, stderr, err)
	}
	klog.V(3).Infof("Added OVS management port interface %s for network %s", mp.ifName, mp.GetNetworkName())

	// STEP2
	_, err = util.LinkSetUp(mp.ifName)
	if err != nil {
		return fmt.Errorf("failed to set the link up for interface %s while plumbing network %s, err: %v",
			mp.ifName, mp.GetNetworkName(), err)
	}
	klog.V(3).Infof("Setup management port link %s for network %s succeeded", mp.ifName, mp.GetNetworkName())

	// STEP3
	// IPv6 forwarding is enabled globally
	if ipv4, _ := mp.IPMode(); ipv4 {
		err = util.SetforwardingModeForInterface(mp.ifName)
		if err != nil {
			return err
		}
	}

	return nil
}

func (mp *udnManagementPortOVS) delete() error {
	return DeleteManagementPortOVSInterface(mp.GetNetworkName(), mp.ifName)
}

func (mp *udnManagementPortRep) create() error {
	klog.V(5).Infof("Lookup representor link and existing management port for '%v'", mp.repDevice)
	// Get management port representor netdevice
	link, err := util.GetNetLinkOps().LinkByName(mp.repDevice)
	if err != nil {
		return fmt.Errorf("failed to lookup management port representor interface %s for network %s: %v", mp.repDevice, mp.GetNetworkName(), err)
	}

	// configure management port: rename, set MTU and set link up and connect representor port to br-int
	klog.V(5).Infof("Setup representor management port %s for network %s", link.Attrs().Name, mp.GetNetworkName())
	err = bringupManagementPortLink(mp.GetNetworkName(), link, nil, mp.repDevice, mp.MTU())
	if err != nil {
		return fmt.Errorf("bring up management port %s for network %s failed: %v", mp.repDevice, mp.GetNetworkName(), err)
	}

	externalIds := []string{
		fmt.Sprintf("%s=%s", types.NetworkExternalID, mp.GetNetworkName()),
		fmt.Sprintf("%s=%s", types.OvnManagementPortNameExternalId, util.GetNetworkScopedK8sMgmtHostIntfName(uint(mp.GetNetworkID()))),
	}
	return createManagementPortOVSRepresentor(mp.GetNetworkName(), mp.repDevice, mp.GetNetworkScopedK8sMgmtIntfName(mp.node.Name), mp.MTU(), externalIds)
}

func (mp *udnManagementPortRep) delete() error {
	err := DeleteManagementPortOVSInterface(mp.GetNetworkName(), mp.repDevice)
	if err != nil {
		return err
	}

	link, err := util.GetNetLinkOps().LinkByName(mp.repDevice)
	if err != nil {
		return fmt.Errorf("failed to lookup management port representor interface %s for network %s: %v", mp.repDevice, mp.GetNetworkName(), err)
	}
	// representor name was not changed
	err = TearDownManagementPortLink(mp.GetNetworkName(), link, "")
	if err != nil {
		return fmt.Errorf("cleanup management port %s for network %s failed: %v", mp.repDevice, mp.GetNetworkName(), err)
	}
	return nil
}

func (mp *udnManagementPortNetdev) create() error {
	netdevice, err := util.GetNetdevNameFromDeviceId(mp.deviceID, v1.DeviceInfo{})
	if err != nil {
		return fmt.Errorf("failed to get netdev name for device %s allocated for %s network: %v", mp.deviceID, mp.GetNetworkName(), err)
	}

	link, err := util.GetNetLinkOps().LinkByName(netdevice)
	if err != nil {
		return fmt.Errorf("failed to get management port link %s for network %s", netdevice, mp.GetNetworkName())
	}

	klog.V(5).Infof("Setup netdevice management port %s for network %s: netdevice %s, MAC %v MTU: %v",
		mp.ifName, mp.GetNetworkName(), netdevice, mp.mpMAC, mp.MTU())
	err = bringupManagementPortLink(mp.GetNetworkName(), link, &mp.mpMAC, mp.ifName, mp.MTU())
	if err != nil {
		return fmt.Errorf("bring up management port %s for network %s failed: %v", mp.ifName, mp.GetNetworkName(), err)
	}

	if ipv4, _ := mp.IPMode(); ipv4 {
		err = util.SetforwardingModeForInterface(mp.ifName)
		if err != nil {
			return err
		}
	}
	return err
}

func (mp *udnManagementPortNetdev) delete() error {
	link, err := util.GetNetLinkOps().LinkByName(mp.ifName)
	if err != nil {
		klog.Warningf("Failed to lookup management port interface %s for network %s: %v", mp.ifName, mp.GetNetworkName(), err)
		return nil
	}

	// original management port interface name can be found from link alias
	err = TearDownManagementPortLink(mp.GetNetworkName(), link, "")
	if err != nil {
		return fmt.Errorf("tearing down management port %s for network %s failed: %v", mp.ifName, mp.GetNetworkName(), err)
	}
	return nil
}
