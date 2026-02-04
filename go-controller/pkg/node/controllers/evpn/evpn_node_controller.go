package evpn

import (
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/netlinkdevicemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
)

const vxlanPort = 4789

type Controller struct {
	nodeName     string
	watchFactory factory.NodeWatchFactory
	kube         kube.Interface
	networkMgr   networkmanager.Interface
	ndm          netlinkdevicemanager.Interface

	// vtepController reconciles VTEP CRs: ensures bridge, VXLAN, SVI, and OVS port
	// lifecycle for each VTEP assigned to this node.
	vtepController controller.Controller
	// nodeEventHandler watches for node annotation changes that should
	// trigger VTEP reconciliation if needed.
	nodeEventHandler cache.ResourceEventHandlerRegistration
	// nadReconciler triggers VTEP reconciliation when NADs referencing a VTEP
	// are added or removed, so VID/VNI mappings and SVIs stay in sync.
	nadReconciler   controller.Reconciler
	nadReconcilerID uint64
	nadVTEPInfoLock sync.Mutex
	// Cache NAD key -> VTEP name for cleanup when NADs are deleted.
	nadVTEPInfo map[string]string

	stopChan chan struct{}
}

func NewController(nodeName string, wf factory.NodeWatchFactory, kube kube.Interface, ndm netlinkdevicemanager.Interface, networkMgr networkmanager.Interface) (*Controller, error) {
	c := &Controller{
		nodeName:     nodeName,
		watchFactory: wf,
		kube:         kube,
		networkMgr:   networkMgr,
		ndm:          ndm,
		nadVTEPInfo:  make(map[string]string),
		stopChan:     make(chan struct{}),
	}

	vtepInformer := wf.VTEPInformer()
	c.vtepController = controller.NewController("evpn-node-vtep-controller", &controller.ControllerConfig[vtepv1.VTEP]{
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
	// VTEP informer cache sync is handled by StartWithInitialSync, but we also
	// need the node informer cache synced to discover VTEP IPs from annotations.
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

	return nil
}

func (c *Controller) Stop() {
	klog.Info("Stopping EVPN node controller")

	if c.nodeEventHandler != nil {
		if err := c.watchFactory.NodeCoreInformer().Informer().RemoveEventHandler(c.nodeEventHandler); err != nil {
			klog.Errorf("Failed to remove node event handler: %v", err)
		}
	}

	if c.nadReconcilerID != 0 {
		if err := c.networkMgr.DeRegisterNADReconciler(c.nadReconcilerID); err != nil {
			klog.Warningf("Failed to deregister EVPN NAD reconciler: %v", err)
		}
	}

	controller.Stop(c.vtepController, c.nadReconciler)

	close(c.stopChan)
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

	if util.NodeHostCIDRsAnnotationChanged(oldNode, newNode) {
		c.reconcileUnmanagedVTEPs(oldNode, newNode)
	}
}

// reconcileUnmanagedVTEPs reconciles unmanaged VTEPs whose CIDRs overlap with
// the host-cidrs that changed between oldNode and newNode.
func (c *Controller) reconcileUnmanagedVTEPs(oldNode, newNode *corev1.Node) {
	oldCIDRs, err := util.ParseNodeHostCIDRs(oldNode)
	if err != nil {
		klog.Errorf("Failed to parse old host-cidrs for node %s: %v", c.nodeName, err)
		return
	}
	newCIDRs, err := util.ParseNodeHostCIDRs(newNode)
	if err != nil {
		klog.Errorf("Failed to parse new host-cidrs for node %s: %v", c.nodeName, err)
		return
	}
	changed := oldCIDRs.SymmetricDifference(newCIDRs)
	if changed.Len() == 0 {
		return
	}

	changedNets, err := util.ParseIPNets(changed.UnsortedList())
	if err != nil {
		klog.Errorf("Failed to parse changed host CIDRs: %v", err)
		return
	}

	vteps, err := c.watchFactory.VTEPInformer().Lister().List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list VTEPs for host-cidrs reconciliation: %v", err)
		return
	}
	for _, vtep := range vteps {
		if vtep.Spec.Mode != vtepv1.VTEPModeUnmanaged {
			continue
		}
		var vtepCIDRStrs []string
		for _, c := range vtep.Spec.CIDRs {
			vtepCIDRStrs = append(vtepCIDRStrs, string(c))
		}
		vtepNets, err := util.ParseIPNets(vtepCIDRStrs)
		if err != nil {
			klog.Errorf("Failed to parse VTEP %s CIDRs: %v", vtep.Name, err)
			continue
		}
		if util.NetworksOverlap(vtepNets, changedNets) {
			klog.V(4).Infof("Host CIDRs changed on node %s, reconciling unmanaged VTEP %s", c.nodeName, vtep.Name)
			c.vtepController.Reconcile(vtep.Name)
		}
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
	if vtep.Spec.Mode == vtepv1.VTEPModeManaged {
		klog.Warningf("VTEP %s uses unsupported %s mode, cleaning up devices", vtep.Name, vtepv1.VTEPModeManaged)
		return c.deleteVTEPDevices(key)
	}

	node, err := c.watchFactory.GetNode(c.nodeName)
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", c.nodeName, err)
	}

	vtepIPv4, vtepIPv6, err := c.discoverUnmanagedVTEPIPs(vtep, node)
	if err != nil {
		return fmt.Errorf("failed to get VTEP IPs: %w", err)
	}
	if vtepIPv4 == nil && vtepIPv6 == nil {
		klog.Infof("VTEP %s IPs not yet available for node %s", vtep.Name, c.nodeName)
		return c.deleteVTEPDevices(key)
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

	mappings, err := c.getVIDVNIMappings(vtep.Name)
	if err != nil {
		return fmt.Errorf("failed to build VID/VNI mappings: %w", err)
	}

	if vtepIPv4 != nil {
		vxlan4Name := GetEVPNVXLANName(vtep.Name, utilnet.IPv4)
		if err := c.ensureVXLAN(vxlan4Name, bridgeName, vtepIPv4, mappings); err != nil {
			return err
		}
	} else {
		// Cover losing ipv4 vtep IP
		if err := c.ndm.DeleteLink(GetEVPNVXLANName(vtep.Name, utilnet.IPv4)); err != nil {
			return fmt.Errorf("failed to delete VTEP VXLAN device: %w", err)
		}
	}

	if vtepIPv6 != nil {
		vxlan6Name := GetEVPNVXLANName(vtep.Name, utilnet.IPv6)
		if err := c.ensureVXLAN(vxlan6Name, bridgeName, vtepIPv6, mappings); err != nil {
			return err
		}
	} else {
		// Cover losing ipv6 vtep IP
		if err := c.ndm.DeleteLink(GetEVPNVXLANName(vtep.Name, utilnet.IPv6)); err != nil {
			return fmt.Errorf("failed to delete VTEP VXLAN device: %w", err)
		}
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

func (c *Controller) getVIDVNIMappings(vtepName string) ([]netlinkdevicemanager.VIDVNIMapping, error) {
	var mappings []netlinkdevicemanager.VIDVNIMapping
	err := c.networkMgr.DoWithLock(func(netInfo util.NetInfo) error {
		if netInfo == nil || netInfo.EVPNVTEPName() != vtepName {
			return nil
		}

		macVID, macVNI := netInfo.EVPNMACVRFVID(), netInfo.EVPNMACVRFVNI()
		if macVID != 0 && macVNI != 0 {
			mappings = append(mappings, netlinkdevicemanager.VIDVNIMapping{
				VID: uint16(macVID),
				VNI: uint32(macVNI),
			})
		}

		ipVID, ipVNI := netInfo.EVPNIPVRFVID(), netInfo.EVPNIPVRFVNI()
		if ipVID != 0 && ipVNI != 0 {
			mappings = append(mappings, netlinkdevicemanager.VIDVNIMapping{
				VID: uint16(ipVID),
				VNI: uint32(ipVNI),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	return mappings, nil
}

func (c *Controller) deleteVTEPDevices(vtepName string) error {
	var errs []error
	if err := c.ndm.DeleteLink(GetEVPNVXLANName(vtepName, utilnet.IPv4)); err != nil {
		errs = append(errs, err)
	}
	if err := c.ndm.DeleteLink(GetEVPNVXLANName(vtepName, utilnet.IPv6)); err != nil {
		errs = append(errs, err)
	}
	if err := c.ndm.DeleteLink(GetEVPNBridgeName(vtepName)); err != nil {
		errs = append(errs, err)
	}
	return utilerrors.Join(errs...)
}

func (c *Controller) ensureVXLAN(vxlanName string, bridgeName string, srcIP net.IP, mappings []netlinkdevicemanager.VIDVNIMapping) error {
	err := c.ndm.EnsureLink(netlinkdevicemanager.DeviceConfig{
		Link: &netlink.Vxlan{
			LinkAttrs: netlink.LinkAttrs{Name: vxlanName, MTU: config.Default.MTU},
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
			// Isolated VXLAN ports can only forward to non-isolated ports.
			// In dual-stack setups with separate IPv4 and IPv6 VXLAN devices on the
			// same bridge, BUM traffic arriving on one VXLAN would be flooded out
			// the other, creating an amplification loop between VTEP peers.
			Isolated: true,
		},
		VIDVNIMappings: mappings,
	})
	if err != nil {
		return fmt.Errorf("failed to apply VXLAN %s: %w", vxlanName, err)
	}
	return nil
}

// discoverUnmanagedVTEPIPs finds IPs on the node that fall within the VTEP's CIDRs.
// For unmanaged VTEPs, an external provider has already assigned IPs to the node;
// this discovers them from the host-cidrs annotation. When multiple IPs match
// a single address family, it falls back to netlink to filter out secondary and
// VIP addresses that can float between nodes. If ambiguity remains, the
// lexicographically lowest IP is chosen for deterministic selection.
func (c *Controller) discoverUnmanagedVTEPIPs(vtep *vtepv1.VTEP, node *corev1.Node) (net.IP, net.IP, error) {
	var cidrs []*net.IPNet
	for _, cidr := range vtep.Spec.CIDRs {
		_, ipNet, err := net.ParseCIDR(string(cidr))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to parse VTEP CIDR %q: %w", cidr, err)
		}
		cidrs = append(cidrs, ipNet)
	}

	hostCIDRs, err := util.ParseNodeHostCIDRs(node)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse host-cidrs annotation: %w", err)
	}

	matchesIP := func(ip net.IP) bool {
		for _, cidr := range cidrs {
			if cidr.Contains(ip) {
				return true
			}
		}
		return false
	}

	var v4Matches, v6Matches []net.IP
	for hostCIDR := range hostCIDRs {
		ip, _, err := net.ParseCIDR(hostCIDR)
		if err != nil {
			return nil, nil, err
		}
		if matchesIP(ip) {
			if ip.To4() != nil {
				v4Matches = append(v4Matches, ip)
			} else {
				v6Matches = append(v6Matches, ip)
			}
		}
	}

	// When a single match exists per family, use it directly. Multiple matches
	// require netlink to filter out secondary/VIP addresses that float between nodes.
	// If ambiguity remains, the lowest IP is picked for deterministic selection.
	ipv4, err := c.pickVTEPIP(v4Matches, matchesIP, netlink.FAMILY_V4)
	if err != nil {
		return nil, nil, err
	}
	ipv6, err := c.pickVTEPIP(v6Matches, matchesIP, netlink.FAMILY_V6)
	if err != nil {
		return nil, nil, err
	}
	return ipv4, ipv6, nil
}

func (c *Controller) pickVTEPIP(matches []net.IP, matchesIP func(net.IP) bool, family int) (net.IP, error) {
	if len(matches) <= 1 {
		if len(matches) == 1 {
			return matches[0], nil
		}
		return nil, nil
	}

	klog.Infof("Multiple VTEP IP candidates %v (family %d), falling back to netlink", matches, family)
	addrs, err := util.GetNetLinkOps().AddrList(nil, family)
	if err != nil {
		return nil, fmt.Errorf("failed to list addresses (family %d): %w", family, err)
	}
	matches = matches[:0]
	for _, addr := range addrs {
		if util.IsAddressAddedByKeepAlived(addr) || (addr.Flags&unix.IFA_F_SECONDARY) != 0 {
			continue
		}
		if matchesIP(addr.IP) {
			matches = append(matches, addr.IP)
		}
	}
	slices.SortFunc(matches, func(a, b net.IP) int {
		addrA, _ := netip.AddrFromSlice(a)
		addrB, _ := netip.AddrFromSlice(b)
		return addrA.Compare(addrB)
	})
	if len(matches) > 0 {
		return matches[0], nil
	}
	return nil, nil
}
