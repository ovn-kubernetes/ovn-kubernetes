package managementport

import (
	"fmt"
	"net"

	v1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
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
	nodeName string
	subnets  []*net.IPNet
	mpMAC    net.HardwareAddr
}

func newUDNManagementPortConfig(nodeName string, networkLocalSubnets []*net.IPNet, netInfo util.NetInfo) (*udnManagementPortConfig, error) {
	if len(networkLocalSubnets) == 0 {
		return nil, fmt.Errorf("cannot determine subnets while configuring management port for network: %s", netInfo.GetNetworkName())
	}

	return &udnManagementPortConfig{
		NetInfo:  netInfo,
		subnets:  networkLocalSubnets,
		nodeName: nodeName,
		mpMAC:    util.IPAddrToHWAddr(netInfo.GetNodeManagementIP(networkLocalSubnets[0]).IP),
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

// NewUDNManagementPortController creates a new management port controller for a primary UDN
func NewUDNManagementPortController(
	nodeLister listers.NodeLister,
	nodeName string,
	networkLocalSubnets []*net.IPNet,
	netInfo util.NetInfo,
) (*UDNManagementPortController, error) {
	var mpdev *util.NetworkDeviceDetails

	mgmtIfName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(netInfo.GetNetworkID()))

	cfg, err := newUDNManagementPortConfig(nodeName, networkLocalSubnets, netInfo)
	if err != nil {
		return nil, err
	}

	node, err := nodeLister.Get(nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %v", nodeName, err)
	}
	mpdevs, err := util.ParseNodeManagementPortAnnotation(node)
	if err != nil {
		if !util.IsAnnotationNotSetError(err) {
			return nil, fmt.Errorf("failed to get management port details for network %s: %v", netInfo.GetNetworkName(), err)
		}
	} else {
		mpdev = mpdevs[netInfo.GetNetworkName()]
	}

	// in full mode and MgmtPortDPResourceName is empty, OVS internal management port can be used, otherwise, management
	// port must has been allocated for this network
	if (config.OvnKubeNode.Mode != types.NodeModeFull || config.OvnKubeNode.MgmtPortDPResourceName != "") && mpdev == nil {
		return nil, fmt.Errorf("management port resource not allocated for network %s (annotation missing or invalid)", cfg.GetNetworkName())
	}

	// cleanup stale OVS management port entities created in different mode/configuration
	err = syncUDNManagementPort(cfg, mgmtIfName, mpdev)
	if err != nil {
		return nil, err
	}

	c := &UDNManagementPortController{
		cfg:   cfg,
		ports: map[string]udnManagementPort{},
	}

	if config.OvnKubeNode.Mode == types.NodeModeFull && config.OvnKubeNode.MgmtPortDPResourceName == "" {
		c.ports[ovsPort] = newUDNManagementPortOVS(cfg, mgmtIfName)
		return c, nil
	}

	switch config.OvnKubeNode.Mode {
	case types.NodeModeFull:
		repDeviceName, err := util.GetFunctionRepresentorName(mpdev.DeviceId)
		if err != nil {
			return nil, fmt.Errorf("failed to get management port representor name for device %s network %s: %v",
				mpdev.DeviceId, netInfo.GetNetworkName(), err)
		}
		c.ports[netdevPort] = newUDNManagementPortNetdev(cfg, mgmtIfName, mpdev.DeviceId)
		c.ports[representorPort] = newUDNManagementPortRep(cfg, repDeviceName)
	case types.NodeModeDPU:
		repDeviceName, err := util.GetSriovnetOps().GetVfRepresentorDPU(fmt.Sprintf("%d", mpdev.PfId), fmt.Sprintf("%d", mpdev.FuncId))
		if err != nil {
			return nil, fmt.Errorf("failed to get management port representor for pfID %v vfID %v network %s: %v",
				mpdev.PfId, mpdev.FuncId, netInfo.GetNetworkName(), err)
		}
		c.ports[representorPort] = newUDNManagementPortRep(cfg, repDeviceName)
	case types.NodeModeDPUHost:
		c.ports[netdevPort] = newUDNManagementPortNetdev(cfg, mgmtIfName, mpdev.DeviceId)
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

// newUDNManagementPortOVS creates a new udnManagementPortOVS
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
func syncUDNManagementPort(cfg *udnManagementPortConfig, mgmtIfName string, mpdev *util.NetworkDeviceDetails) error {
	var err error
	// representor OVS interface
	ovsRepIfName, _, _ := util.RunOVSVsctl("--no-headings",
		"--data", "bare",
		"--format", "csv",
		"--columns", "name",
		"find", "Interface", fmt.Sprintf("external-ids:%s=%s", types.OvnManagementPortNameExternalID, mgmtIfName))
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
			err = DeleteManagementPortRepInterface(cfg.GetNetworkName(), ovsRepIfName, ovsRepIfName)
			if err != nil {
				klog.Errorf("Failed to delete stale OVS representor port interface %s for network %s: %v", ovsRepIfName, cfg.GetNetworkName(), err)
			}
		}
		// In gateway_udn unit testing, it pre-create the netdev link for management port OVS internal interface.
		// In order for unit testing to work, we can not tear-down/rename the link. Remove below for now.
		//
		//if ovsInternalIfName == "" {
		//	klog.V(5).Infof("Expected management port OVS internal interface, bring down stale management port netdev interface %s for network %s",
		//		mgmtIfName, cfg.GetNetworkName())
		//	link, _ := util.GetNetLinkOps().LinkByName(mgmtIfName)
		//	if link != nil {
		//		err = TearDownManagementPortLink(cfg.GetNetworkName(), link, "")
		//		if err != nil {
		//			klog.Errorf("Failed to bring down OVS netdev interface %s for network %s: %v", mgmtIfName, cfg.GetNetworkName(), err)
		//		}
		//	}
		//}
		return nil
	}

	if config.OvnKubeNode.Mode != types.NodeModeDPU {
		if ovsInternalIfName != "" {
			klog.V(5).Infof("Expected management port OVS netdev interface, bring down stale management port internal interface %s for network %s",
				mgmtIfName, cfg.GetNetworkName())
			err = DeleteManagementPortInternalOVSInterface(cfg.GetNetworkName(), mgmtIfName)
			if err != nil {
				klog.Errorf("Failed to delete OVS internal interface %s for network %s: %v", mgmtIfName, cfg.GetNetworkName(), err)
			}
		}
		// then check the current mgmtIfName to see if it is the same netdevice
		link, _ := util.GetNetLinkOps().LinkByName(mgmtIfName)
		if link == nil {
			return nil
		}
		deviceId, err := util.GetDeviceIDFromNetdevice(link.Attrs().Name)
		if err != nil {
			return nil
		}
		if deviceId != mpdev.DeviceId {
			klog.Infof("Management port device %s for network %s deviceID mismatch: expected %s but is %s",
				mgmtIfName, cfg.GetNetworkName(), mpdev.DeviceId, deviceId)
			err = TearDownManagementPortLink(cfg.GetNetworkName(), link, "")
			if err != nil {
				klog.Errorf("Failed to bring down stale OVS netdev interface %s for network %s: %v", mgmtIfName, cfg.GetNetworkName(), err)
			}
			// in that case, representor could also be associated with stale netdev and needs to be deleted
			if ovsRepIfName != "" {
				err = DeleteManagementPortRepInterface(cfg.GetNetworkName(), ovsRepIfName, ovsRepIfName)
				if err != nil {
					klog.Errorf("Failed to delete stale OVS representor port interface %s for network %s: %v", ovsRepIfName, cfg.GetNetworkName(), err)
				}
			}
		}
	} else if ovsRepIfName != "" {
		repDeviceName, _ := util.GetSriovnetOps().GetVfRepresentorDPU(fmt.Sprintf("%d", mpdev.PfId), fmt.Sprintf("%d", mpdev.FuncId))
		if repDeviceName != ovsRepIfName {
			err = DeleteManagementPortRepInterface(cfg.GetNetworkName(), ovsRepIfName, ovsRepIfName)
			if err != nil {
				klog.Errorf("Failed to delete stale OVS representor port interface %s for network %s: %v", ovsRepIfName, cfg.GetNetworkName(), err)
			}
		}
	}

	return nil
}

// udnManagementPortOVS.create does the following:
// STEP1: creates the (netdevice) OVS interface on br-int for the UDN's management port
// STEP2: sets up the management port link on the host
// STEP3: enables IPv4 forwarding on the interface if the network has a v4 subnet
func (mp *udnManagementPortOVS) create() error {
	// STEP1
	stdout, stderr, err := util.RunOVSVsctl(
		"--", "--may-exist", "add-port", "br-int", mp.ifName,
		"--", "set", "interface", mp.ifName, fmt.Sprintf("mac=\"%s\"", mp.mpMAC.String()),
		"type=internal", "mtu_request="+fmt.Sprintf("%d", mp.MTU()),
		"external-ids:iface-id="+mp.GetNetworkScopedK8sMgmtIntfName(mp.nodeName),
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

	// add loose mode for rp filter on management port
	if err = util.SetRPFilterLooseModeForInterface(mp.ifName); err != nil {
		return err
	}
	return nil
}

func (mp *udnManagementPortOVS) delete() error {
	return DeleteManagementPortInternalOVSInterface(mp.GetNetworkName(), mp.ifName)
}

// Create management port representor. Note that the representor device is not renamed. One can determine its associated
// UDN network and its management netdev interface from its external-ids.
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
		fmt.Sprintf("%s=%s", types.OvnManagementPortNameExternalID, util.GetNetworkScopedK8sMgmtHostIntfName(uint(mp.GetNetworkID()))),
	}
	return createManagementPortOVSRepresentor(mp.GetNetworkName(), mp.repDevice, mp.GetNetworkScopedK8sMgmtIntfName(mp.nodeName), mp.MTU(), externalIds)
}

func (mp *udnManagementPortRep) delete() error {
	return DeleteManagementPortRepInterface(mp.GetNetworkName(), mp.repDevice, mp.repDevice)
}

func (mp *udnManagementPortNetdev) create() error {
	netdevice, err := util.GetNetdevNameFromDeviceId(mp.deviceID, v1.DeviceInfo{})
	if err != nil {
		return fmt.Errorf("failed to get netdev name for device %s allocated for %s network: %v", mp.deviceID, mp.GetNetworkName(), err)
	}

	link, err := util.GetNetLinkOps().LinkByName(netdevice)
	if err != nil {
		return fmt.Errorf("failed to get management port link %s for network %s: %v", netdevice, mp.GetNetworkName(), err)
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

	// add loose mode for rp filter on management port
	if err = util.SetRPFilterLooseModeForInterface(mp.ifName); err != nil {
		return err
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
