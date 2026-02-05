package evpn

import (
	"fmt"
	"net"
	"reflect"
	"sync"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	vtepv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/netlinkdevicemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/vswitchd"
)

const (
	vxlanPort = 4789
	// externalIDEVPNVTEP is the external-id key used to tag OVS ports with their VTEP name
	externalIDEVPNVTEP = "evpn-vtep"
	ovsBridgeInt       = "br-int"
)

type Controller struct {
	nodeName     string
	watchFactory factory.NodeWatchFactory
	kube         kube.Interface
	networkMgr   networkmanager.Interface
	ndm          *netlinkdevicemanager.Controller
	ovsClient    libovsdbclient.Client

	vtepController  controller.Controller
	nadReconciler   controller.Reconciler
	nadReconcilerID uint64
	nadVTEPInfoLock sync.Mutex
	// Cache NAD key -> VTEP name for cleanup when NADs are deleted.
	nadVTEPInfo      map[string]string
	nodeEventHandler cache.ResourceEventHandlerRegistration

	stopChan chan struct{}
	wg       *sync.WaitGroup
}

func NewController(nodeName string, wf factory.NodeWatchFactory, kube kube.Interface, networkMgr networkmanager.Interface, ovsClient libovsdbclient.Client) (*Controller, error) {
	c := &Controller{
		nodeName:     nodeName,
		watchFactory: wf,
		kube:         kube,
		networkMgr:   networkMgr,
		ndm:          netlinkdevicemanager.NewController(),
		ovsClient:    ovsClient,
		nadVTEPInfo:  make(map[string]string),
		stopChan:     make(chan struct{}),
		wg:           &sync.WaitGroup{},
	}

	vtepInformer := wf.VTEPInformer()
	c.vtepController = controller.NewController("evpn-vtep-controller", &controller.ControllerConfig[vtepv1.VTEP]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcile,
		ObjNeedsUpdate: c.vtepNeedsUpdate,
		Threadiness:    1,
		Informer:       vtepInformer.Informer(),
		Lister:         vtepInformer.Lister().List,
	})
	c.nadReconciler = controller.NewReconciler("evpn-nad-reconciler", &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   c.reconcileNAD,
		Threadiness: 1,
		MaxAttempts: controller.InfiniteAttempts,
	})

	var err error
	c.nodeEventHandler, err = wf.NodeCoreInformer().Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: c.onNodeUpdate,
		})
	if err != nil {
		return nil, fmt.Errorf("failed to add node event handler: %w", err)
	}

	return c, nil
}

func (c *Controller) Start() error {
	klog.Info("Starting EVPN node controller")

	id, err := c.networkMgr.RegisterNADReconciler(c.nadReconciler)
	if err != nil {
		return err
	}
	c.nadReconcilerID = id
	return controller.StartWithInitialSync(c.initialSync, c.vtepController, c.nadReconciler)
}

func (c *Controller) initialSync() error {
	// VTEP cache sync is already done by StartWithInitialSync, but we also need node cache
	if !util.WaitForInformerCacheSyncWithTimeout("evpn-node", c.stopChan, c.watchFactory.NodeCoreInformer().Informer().HasSynced) {
		return fmt.Errorf("timed out waiting for node informer cache to sync")
	}

	vteps, err := c.watchFactory.VTEPInformer().Lister().List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list VTEPs: %w", err)
	}

	// Reconcile all VTEPs to populate NDM desired state before starting it.
	// This prevents NDM from removing interfaces that existed before restart.
	for _, vtep := range vteps {
		if err := c.reconcile(vtep.Name); err != nil {
			return fmt.Errorf("failed to reconcile VTEP %s: %w", vtep.Name, err)
		}
	}

	// TODO: This should be done outside of this controller
	if err := c.ndm.Run(c.stopChan, c.wg); err != nil {
		return fmt.Errorf("failed to start NDM: %w", err)
	}

	return nil
}

func (c *Controller) Stop() {
	klog.Info("Stopping EVPN node controller")

	if c.nodeEventHandler != nil {
		if err := c.watchFactory.NodeCoreInformer().Informer().RemoveEventHandler(c.nodeEventHandler); err != nil {
			klog.Errorf("Failed to remove node event handler: %v", err)
		}
	}

	if err := c.networkMgr.DeRegisterNADReconciler(c.nadReconcilerID); err != nil {
		klog.Warningf("Failed to deregister EVPN NAD reconciler: %v", err)
	}

	controller.Stop(c.vtepController, c.nadReconciler)

	close(c.stopChan)
	c.wg.Wait()
}

func (c *Controller) vtepNeedsUpdate(oldObj, newObj *vtepv1.VTEP) bool {
	if oldObj == nil || newObj == nil {
		return true
	}
	return !reflect.DeepEqual(oldObj.Spec, newObj.Spec)
}

func (c *Controller) onNodeUpdate(oldObj, newObj interface{}) {
	oldNode, ok := oldObj.(*corev1.Node)
	if !ok {
		return
	}
	newNode, ok := newObj.(*corev1.Node)
	if !ok {
		return
	}
	if newNode.Name != c.nodeName {
		return
	}
	if oldNode.Annotations[util.OVNNodeVTEPIPs] == newNode.Annotations[util.OVNNodeVTEPIPs] {
		return
	}

	oldIPs, err := util.ParseVTEPIPsAnnotation(oldNode)
	if err != nil {
		klog.Errorf("Failed to parse VTEP IPs: %v", err)
		return
	}
	newIPs, err := util.ParseVTEPIPsAnnotation(newNode)
	if err != nil {
		klog.Errorf("Failed to parse VTEP IPs: %v", err)
		return
	}

	// Find VTEPs that need reconciliation: added, removed, or changed
	toReconcile := sets.KeySet(oldIPs).SymmetricDifference(sets.KeySet(newIPs))
	for vtepName := range sets.KeySet(oldIPs).Intersection(sets.KeySet(newIPs)) {
		if !reflect.DeepEqual(oldIPs[vtepName], newIPs[vtepName]) {
			toReconcile.Insert(vtepName)
		}
	}

	for vtepName := range toReconcile {
		klog.V(4).Infof("VTEP %s IPs changed on node %s, reconciling", vtepName, c.nodeName)
		c.vtepController.Reconcile(vtepName)
	}
}

func (c *Controller) reconcile(key string) error {
	vtep, err := c.watchFactory.VTEPInformer().Lister().Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof("VTEP %s not found, cleaning up devices", key)
			return c.deleteVTEPDevices(key)
		}
		return err
	}

	node, err := c.watchFactory.GetNode(c.nodeName)
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", c.nodeName, err)
	}

	vtepIPv4, vtepIPv6, err := c.getVTEPIPs(vtep, node)
	if err != nil {
		return fmt.Errorf("failed to get VTEP IPs: %w", err)
	}
	if vtepIPv4 == nil && vtepIPv6 == nil {
		klog.Infof("VTEP %s IPs not yet available for node %s", vtep.Name, c.nodeName)
		return c.deleteVTEPDevices(key)
	}

	if vtep.Spec.Mode == vtepv1.VTEPModeManaged {
		if err := c.ensureDummyWithIPs(vtep, vtepIPv4, vtepIPv6); err != nil {
			return fmt.Errorf("failed to ensure VTEP dummy device: %w", err)
		}
	} else {
		// Ensure that the device is not present to cover the VTEP mode change
		if err := c.ndm.DeleteLink(GetEVPNDummyName(vtep.Name)); err != nil {
			return fmt.Errorf("failed to delete VTEP dummy device: %w", err)
		}
	}

	bridgeName := GetEVPNBridgeName(vtep.Name)

	klog.V(4).Infof("Applying EVPN devices for VTEP %s: bridge=%s, IPv4=%v, IPv6=%v",
		vtep.Name, bridgeName, vtepIPv4, vtepIPv6)

	err = c.ndm.EnsureLink(netlinkdevicemanager.DeviceConfig{
		Link: &netlink.Bridge{
			LinkAttrs:       netlink.LinkAttrs{Name: bridgeName},
			VlanFiltering:   ptr.To(true),
			VlanDefaultPVID: ptr.To(uint16(0)),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to apply bridge %s: %w", bridgeName, err)
	}

	var vxlan4Name, vxlan6Name string
	if vtepIPv4 != nil {
		vxlan4Name = GetEVPNVXLANName(vtep.Name, utilnet.IPv4)
		if err := c.ensureVXLAN(vxlan4Name, bridgeName, vtepIPv4); err != nil {
			return err
		}
	} else {
		// Cover loosing ipv4 vtep IP
		if err := c.ndm.DeleteLink(vxlan4Name); err != nil {
			return fmt.Errorf("failed to delete VTEP VXLAN device %s: %w", vxlan4Name, err)
		}
	}

	if vtepIPv6 != nil {
		vxlan6Name = GetEVPNVXLANName(vtep.Name, utilnet.IPv6)
		if err := c.ensureVXLAN(vxlan6Name, bridgeName, vtepIPv6); err != nil {
			return err
		}
	} else {
		// Cover loosing ipv6 vtep IP
		if err := c.ndm.DeleteLink(GetEVPNVXLANName(vtep.Name, utilnet.IPv6)); err != nil {
			return fmt.Errorf("failed to delete VTEP VXLAN device: %w", err)
		}
	}

	networks, err := c.collectEVPNNetworks(vtep.Name)
	if err != nil {
		return fmt.Errorf("failed to collect EVPN networks: %w", err)
	}

	mappings := c.getVIDVNIMappings(networks)
	if vtepIPv4 != nil {
		if err := c.ndm.EnsureBridgeMappings(bridgeName, vxlan4Name, mappings); err != nil {
			return fmt.Errorf("failed to ensure VXLAN mappings for %s: %w", vxlan4Name, err)
		}
	}
	if vtepIPv6 != nil {
		if err := c.ndm.EnsureBridgeMappings(bridgeName, vxlan6Name, mappings); err != nil {
			return fmt.Errorf("failed to ensure VXLAN mappings for %s: %w", vxlan6Name, err)
		}
	}

	if err := c.reconcileSVIs(bridgeName, networks); err != nil {
		return fmt.Errorf("failed to reconcile SVIs: %w", err)
	}

	if err := c.reconcileOVSPorts(vtep.Name, bridgeName, networks); err != nil {
		return fmt.Errorf("failed to reconcile OVS ports: %w", err)
	}

	return nil
}

func (c *Controller) reconcileNAD(key string) error {
	netInfo := c.networkMgr.GetNetInfoForNADKey(key)
	c.nadVTEPInfoLock.Lock()
	defer c.nadVTEPInfoLock.Unlock()

	if netInfo == nil {
		vtepName, ok := c.nadVTEPInfo[key]
		if ok {
			klog.Infof("Network %s removed, reconciling VTEP %s", key, vtepName)
			delete(c.nadVTEPInfo, key)
			c.vtepController.Reconcile(vtepName)
		}
		return nil
	}

	vtepName := netInfo.EVPNVTEPName()
	if vtepName == "" {
		return nil
	}

	c.nadVTEPInfo[key] = vtepName
	c.vtepController.Reconcile(vtepName)

	return nil
}

// evpnNetworkInfo holds EVPN config for a network.
type evpnNetworkInfo struct {
	macVRFVID, macVRFVNI int
	ipVRFVID, ipVRFVNI   int
	sviName              string
	ovsPortName          string
	vrfName              string
	routerMAC            net.HardwareAddr
}

// collectEVPNNetworks gathers EVPN network info for all networks using this VTEP.
func (c *Controller) collectEVPNNetworks(vtepName string) ([]evpnNetworkInfo, error) {
	var networks []evpnNetworkInfo

	err := c.networkMgr.DoWithLock(func(netInfo util.NetInfo) error {
		if netInfo == nil || netInfo.EVPNVTEPName() != vtepName {
			return nil
		}
		networks = append(networks, evpnNetworkInfo{
			macVRFVID:   netInfo.EVPNMACVRFVID(),
			macVRFVNI:   int(netInfo.EVPNMACVRFVNI()),
			ipVRFVID:    netInfo.EVPNIPVRFVID(),
			ipVRFVNI:    int(netInfo.EVPNIPVRFVNI()),
			sviName:     GetEVPNSVIName(netInfo),
			ovsPortName: GetEVPNOVSPortName(netInfo),
			vrfName:     util.GetNetworkVRFName(netInfo),
			routerMAC:   EVPNRouterMAC(netInfo.GetNetworkID()),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return networks, nil
}

func (c *Controller) getVIDVNIMappings(networks []evpnNetworkInfo) []netlinkdevicemanager.VIDVNIMapping {
	var mappings []netlinkdevicemanager.VIDVNIMapping
	for _, n := range networks {
		if n.macVRFVID != 0 && n.macVRFVNI != 0 {
			mappings = append(mappings, netlinkdevicemanager.VIDVNIMapping{VID: uint16(n.macVRFVID), VNI: uint32(n.macVRFVNI)})
		}
		if n.ipVRFVID != 0 && n.ipVRFVNI != 0 {
			mappings = append(mappings, netlinkdevicemanager.VIDVNIMapping{VID: uint16(n.ipVRFVID), VNI: uint32(n.ipVRFVNI)})
		}
	}
	return mappings
}

// reconcileSVIs ensures desired SVIs exist and removes stale ones.
// Uses NDM's ListDevicesByVLANParent to find current SVIs for this bridge.
func (c *Controller) reconcileSVIs(bridgeName string, networks []evpnNetworkInfo) error {
	desiredSVIs := sets.New[string]()
	for _, net := range networks {
		if net.ipVRFVID == 0 {
			continue
		}
		desiredSVIs.Insert(net.sviName)
		if err := c.ndm.EnsureLink(netlinkdevicemanager.DeviceConfig{
			Link: &netlink.Vlan{
				LinkAttrs: netlink.LinkAttrs{Name: net.sviName, HardwareAddr: net.routerMAC},
				VlanId:    net.ipVRFVID,
			},
			VLANParent: bridgeName,
			Master:     net.vrfName,
		}); err != nil {
			return fmt.Errorf("failed to ensure SVI %s: %w", net.sviName, err)
		}
	}

	for _, cfg := range c.ndm.ListDevicesByVLANParent(bridgeName) {
		sviName := cfg.Link.Attrs().Name
		if !desiredSVIs.Has(sviName) {
			klog.Infof("SVI %s no longer applies to %s, removing it", sviName, bridgeName)
			if err := c.ndm.DeleteLink(sviName); err != nil {
				return fmt.Errorf("failed to delete stale SVI %s: %w", sviName, err)
			}
		}
	}

	return nil
}

// reconcileOVSPorts ensures OVS internal ports exist for MAC-VRF networks and removes stale ones.
func (c *Controller) reconcileOVSPorts(vtepName, bridgeName string, networks []evpnNetworkInfo) error {
	desiredPorts := sets.New[string]()
	for _, net := range networks {
		if net.macVRFVID == 0 {
			continue
		}
		desiredPorts.Insert(net.ovsPortName)
		if err := c.ensureOVSPort(vtepName, bridgeName, net.ovsPortName, net.macVRFVID); err != nil {
			return fmt.Errorf("failed to ensure OVS port %s: %w", net.ovsPortName, err)
		}
	}

	ports, err := libovsdbops.FindOVSPortsWithPredicate(c.ovsClient, func(port *vswitchd.Port) bool {
		return port.ExternalIDs[externalIDEVPNVTEP] == vtepName
	})
	if err != nil {
		return fmt.Errorf("failed to list OVS ports for VTEP %s: %w", vtepName, err)
	}
	for _, port := range ports {
		if !desiredPorts.Has(port.Name) {
			klog.Infof("OVS port %s no longer needed for VTEP %s, removing", port.Name, vtepName)
			if err := c.deleteOVSPort(port.Name); err != nil {
				return fmt.Errorf("failed to delete stale OVS port %s: %w", port.Name, err)
			}
		}
	}

	return nil
}

// ensureOVSPort creates an OVS internal port on br-int, attaches it to the EVPN bridge, and sets VLAN access.
// This function is idempotent - it's safe to call multiple times.
func (c *Controller) ensureOVSPort(vtepName, bridgeName, portName string, vid int) error {
	if err := libovsdbops.CreateOrUpdatePortWithInterface(c.ovsClient, ovsBridgeInt, portName,
		map[string]string{externalIDEVPNVTEP: vtepName}); err != nil {
		return fmt.Errorf("failed to create OVS port %s: %w", portName, err)
	}

	ovsLink, err := util.GetNetLinkOps().LinkByName(portName)
	if err != nil {
		return fmt.Errorf("failed to get OVS port link %s: %w", portName, err)
	}
	evpnBridge, err := util.GetNetLinkOps().LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("failed to get EVPN bridge %s: %w", bridgeName, err)
	}
	if err := util.GetNetLinkOps().LinkSetMaster(ovsLink, evpnBridge); err != nil {
		return fmt.Errorf("failed to attach OVS port %s to bridge %s: %w", portName, bridgeName, err)
	}
	if err := util.GetNetLinkOps().LinkSetUp(ovsLink); err != nil {
		return fmt.Errorf("failed to bring up OVS port %s: %w", portName, err)
	}

	if err := c.ndm.EnsureBridgePortVLAN(portName, netlinkdevicemanager.BridgePortVLAN{
		VID:      vid,
		PVID:     true,
		Untagged: true,
	}); err != nil {
		return fmt.Errorf("failed to set VLAN on OVS port %s: %w", portName, err)
	}

	return nil
}

// deleteOVSPort removes an OVS port from br-int.
func (c *Controller) deleteOVSPort(portName string) error {
	if err := c.ndm.DeleteBridgePortVLAN(portName, 0); err != nil {
		return err
	}
	return libovsdbops.DeletePortWithInterfaces(c.ovsClient, ovsBridgeInt, portName)
}

func (c *Controller) deleteVTEPDevices(vtepName string) error {
	bridgeName := GetEVPNBridgeName(vtepName)
	var errs []error

	ports, err := libovsdbops.FindOVSPortsWithPredicate(c.ovsClient, func(port *vswitchd.Port) bool {
		return port.ExternalIDs[externalIDEVPNVTEP] == vtepName
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to list OVS ports: %w", err))
	}
	for _, port := range ports {
		if err := c.deleteOVSPort(port.Name); err != nil {
			errs = append(errs, err)
		}
	}

	// Delete all SVIs parented to this bridge
	for _, cfg := range c.ndm.ListDevicesByVLANParent(bridgeName) {
		sviName := cfg.Link.Attrs().Name
		if err := c.ndm.DeleteLink(sviName); err != nil {
			errs = append(errs, err)
		}
	}

	if err := c.ndm.DeleteLink(GetEVPNVXLANName(vtepName, utilnet.IPv4)); err != nil {
		errs = append(errs, err)
	}
	if err := c.ndm.DeleteLink(GetEVPNVXLANName(vtepName, utilnet.IPv6)); err != nil {
		errs = append(errs, err)
	}
	if err := c.ndm.DeleteLink(GetEVPNDummyName(vtepName)); err != nil {
		errs = append(errs, err)
	}
	if err := c.ndm.DeleteLink(bridgeName); err != nil {
		errs = append(errs, err)
	}
	return utilerrors.Join(errs...)
}

func (c *Controller) ensureVXLAN(vxlanName string, bridgeName string, srcIP net.IP) error {
	err := c.ndm.EnsureLink(netlinkdevicemanager.DeviceConfig{
		Link: &netlink.Vxlan{
			LinkAttrs: netlink.LinkAttrs{Name: vxlanName},
			Port:      vxlanPort,
			SrcAddr:   srcIP,
			FlowBased: true,
			VniFilter: true,
		},
		Master: bridgeName,
		BridgePortSettings: &netlinkdevicemanager.BridgePortSettings{
			VLANTunnel:    true,
			NeighSuppress: true,
			Learning:      false,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to apply VXLAN %s: %w", vxlanName, err)
	}
	return nil
}

func (c *Controller) getVTEPIPs(vtep *vtepv1.VTEP, node *corev1.Node) (net.IP, net.IP, error) {
	vtepIPs, err := util.ParseVTEPIPsAnnotation(node)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse VTEP IPs annotation: %w", err)
	}

	ipStrs, ok := vtepIPs[vtep.Name]
	if !ok || len(ipStrs) == 0 {
		return nil, nil, nil
	}

	var ipv4, ipv6 net.IP
	for _, ipStr := range ipStrs {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			return nil, nil, fmt.Errorf("invalid IP in annotation: %s", ipStr)
		}
		if ip.To4() != nil {
			ipv4 = ip
		} else {
			ipv6 = ip
		}
	}
	return ipv4, ipv6, nil
}

func (c *Controller) ensureDummyWithIPs(vtep *vtepv1.VTEP, ips ...net.IP) error {
	name := GetEVPNDummyName(vtep.Name)

	var addresses []netlink.Addr
	for _, ip := range ips {
		if ip == nil {
			continue
		}
		maskBits := 32
		if ip.To4() == nil {
			maskBits = 128
		}
		addresses = append(addresses, netlink.Addr{
			IPNet: &net.IPNet{
				IP:   ip,
				Mask: net.CIDRMask(maskBits, maskBits),
			},
		})
	}

	return c.ndm.EnsureLink(netlinkdevicemanager.DeviceConfig{
		Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}},
		Addresses: addresses,
	})
}
