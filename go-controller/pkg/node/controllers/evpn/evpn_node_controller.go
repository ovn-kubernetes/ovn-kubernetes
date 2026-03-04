package evpn

import (
	"errors"
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
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/netlinkdevicemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
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
	ndm          netlinkdevicemanager.Interface
	ovsClient    libovsdbclient.Client

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

	// svisByBridge tracks SVI names created per bridge, so we can detect
	// stale SVIs
	svisByBridge map[string]sets.Set[string]

	// podController manages static FDB and neighbor entries for local pods
	// on EVPN networks, enabling ARP/ND suppression and known-unicast forwarding.
	podController controller.Controller
	podLister     corelisters.PodLister
	podNeighLock  sync.Mutex
	podNeighbors  map[string]*neighEntries

	stopChan chan struct{}
}

func NewController(nodeName string, wf factory.NodeWatchFactory, kube kube.Interface, ndm netlinkdevicemanager.Interface, networkMgr networkmanager.Interface, ovsClient libovsdbclient.Client) (*Controller, error) {
	c := &Controller{
		nodeName:     nodeName,
		watchFactory: wf,
		kube:         kube,
		networkMgr:   networkMgr,
		ndm:          ndm,
		ovsClient:    ovsClient,
		nadVTEPInfo:  make(map[string]string),
		svisByBridge: make(map[string]sets.Set[string]),
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

	podInformer := wf.PodCoreInformer()
	c.podLister = podInformer.Lister()
	c.podNeighbors = make(map[string]*neighEntries)
	c.podController = controller.NewController("evpn-pod-neighbor-controller",
		&controller.ControllerConfig[corev1.Pod]{
			RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:      c.reconcilePod,
			ObjNeedsUpdate: c.podNeedsUpdate,
			Threadiness:    1,
			Informer:       podInformer.Informer(),
			Lister:         c.podLister.List,
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
	return controller.StartWithInitialSync(c.initialSync, c.vtepController, c.nadReconciler, c.podController)
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

	node, err := c.watchFactory.GetNode(c.nodeName)
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", c.nodeName, err)
	}

	// Pre-populate NDM desired state so it doesn't remove existing devices on startup.
	// Only NDM-managed devices (bridge, VXLANs, SVIs) are handled here.
	// OVS ports are deferred to normal reconciliation after controllers start.
	activeVTEPs := sets.New[string]()
	for _, vtep := range vteps {
		activeVTEPs.Insert(vtep.Name)
		vtepIPv4, vtepIPv6, err := c.discoverUnmanagedVTEPIPs(vtep, node)
		if err != nil {
			klog.Errorf("Failed to get VTEP IPs for %s: %v", vtep.Name, err)
			continue
		}
		if vtepIPv4 == nil && vtepIPv6 == nil {
			continue
		}
		if _, err := c.ensureDevices(vtep, vtepIPv4, vtepIPv6); err != nil {
			klog.Errorf("Failed to pre-populate NDM for VTEP %s: %v", vtep.Name, err)
		}
	}

	// Clean up stale OVS ports from VTEPs that were deleted while the controller was down.
	// NDM handles netlink device cleanup via alias-based ownership, but OVS ports are managed
	// directly through libovsdb and need explicit cleanup here.
	if err := c.cleanupStaleOVSPorts(activeVTEPs); err != nil {
		klog.Errorf("Failed to to clean up stale OVS ports: %v", err)
	}

	if err := c.cleanStalePodEntries(); err != nil {
		klog.Errorf("Failed to sync pod neighbors: %v", err)
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

	controller.Stop(c.vtepController, c.nadReconciler, c.podController)

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

	networks, err := c.ensureDevices(vtep, vtepIPv4, vtepIPv6)
	if err != nil {
		return err
	}

	if err := c.reconcileOVSPorts(vtep.Name, GetEVPNBridgeName(vtep.Name), networks); err != nil {
		var linkNotFound netlink.LinkNotFoundError
		if errors.As(err, &linkNotFound) {
			klog.V(4).Infof("VTEP %s OVS port not yet available (%v), will retry", vtep.Name, err)
			c.vtepController.ReconcileRateLimited(vtep.Name)
			return nil
		}
		return fmt.Errorf("failed to reconcile OVS ports: %w", err)
	}

	return nil
}

// ensureDevices programs NDM-managed devices for a VTEP: bridge, VXLANs, and SVIs.
// OVS ports are handled separately by reconcileOVSPorts.
func (c *Controller) ensureDevices(vtep *vtepv1.VTEP, vtepIPv4, vtepIPv6 net.IP) ([]evpnNetworkInfo, error) {
	bridgeName := GetEVPNBridgeName(vtep.Name)

	klog.V(4).Infof("Applying EVPN devices for VTEP %s: bridge=%s, IPv4=%v, IPv6=%v",
		vtep.Name, bridgeName, vtepIPv4, vtepIPv6)

	err := c.ndm.EnsureLink(netlinkdevicemanager.DeviceConfig{
		Link: &netlink.Bridge{
			LinkAttrs:       netlink.LinkAttrs{Name: bridgeName},
			VlanFiltering:   ptr.To(true),
			VlanDefaultPVID: ptr.To(uint16(0)),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to apply bridge %s: %w", bridgeName, err)
	}

	networks, err := c.collectEVPNNetworks(vtep.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to collect EVPN networks: %w", err)
	}

	mappings := c.getVIDVNIMappings(networks)
	if vtepIPv4 != nil {
		vxlan4Name := GetEVPNVXLANName(vtep.Name, utilnet.IPv4)
		if err := c.ensureVXLAN(vxlan4Name, bridgeName, vtepIPv4, mappings); err != nil {
			return nil, err
		}
	} else {
		// Cover losing ipv4 vtep IP
		if err := c.ndm.DeleteLink(GetEVPNVXLANName(vtep.Name, utilnet.IPv4)); err != nil {
			return nil, fmt.Errorf("failed to delete VTEP VXLAN device: %w", err)
		}
	}

	if vtepIPv6 != nil {
		vxlan6Name := GetEVPNVXLANName(vtep.Name, utilnet.IPv6)
		if err := c.ensureVXLAN(vxlan6Name, bridgeName, vtepIPv6, mappings); err != nil {
			return nil, err
		}
	} else {
		// Cover losing ipv6 vtep IP
		if err := c.ndm.DeleteLink(GetEVPNVXLANName(vtep.Name, utilnet.IPv6)); err != nil {
			return nil, fmt.Errorf("failed to delete VTEP VXLAN device: %w", err)
		}
	}

	if err := c.reconcileSVIs(bridgeName, networks); err != nil {
		return nil, fmt.Errorf("failed to reconcile SVIs: %w", err)
	}

	return networks, nil
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
			c.vtepController.ReconcileRateLimited(vtepName)
		}
		return nil
	}

	vtepName := netInfo.EVPNVTEPName()
	if vtepName == "" {
		return nil
	}

	c.nadVTEPInfo[key] = vtepName
	c.vtepController.ReconcileRateLimited(vtepName)

	return nil
}

// evpnNetworkInfo holds EVPN config for a network.
type evpnNetworkInfo struct {
	macVRFVID, macVRFVNI int
	ipVRFVID, ipVRFVNI   int
	l3SVIName            string
	l2SVIName            string
	ovsPortName          string
	macVRFLSPName        string
	vrfName              string
}

// collectEVPNNetworks gathers EVPN network info for all networks using this VTEP.
func (c *Controller) collectEVPNNetworks(vtepName string) ([]evpnNetworkInfo, error) {
	var networks []evpnNetworkInfo

	err := c.networkMgr.DoWithLock(func(netInfo util.NetInfo) error {
		if netInfo == nil || netInfo.EVPNVTEPName() != vtepName {
			return nil
		}
		switchName := netInfo.GetNetworkScopedSwitchName(types.OVNLayer2Switch)
		networks = append(networks, evpnNetworkInfo{
			macVRFVID:     netInfo.EVPNMACVRFVID(),
			macVRFVNI:     int(netInfo.EVPNMACVRFVNI()),
			ipVRFVID:      netInfo.EVPNIPVRFVID(),
			ipVRFVNI:      int(netInfo.EVPNIPVRFVNI()),
			l3SVIName:     GetEVPNL3SVIName(netInfo),
			l2SVIName:     GetEVPNL2SVIName(netInfo),
			ovsPortName:   GetEVPNOVSPortName(netInfo),
			macVRFLSPName: util.GetMACVRFPortName(switchName),
			vrfName:       util.GetNetworkVRFName(netInfo),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return networks, nil
}

func (c *Controller) getVIDVNIMappings(networks []evpnNetworkInfo) []netlinkdevicemanager.VIDVNIMapping {
	// Use non-nil empty slice so NDM treats this as actively managed and removes stale mappings.
	mappings := make([]netlinkdevicemanager.VIDVNIMapping, 0)
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
// Creates L3 (IP-VRF) SVIs for routing and L2 (MAC-VRF) SVIs for ARP suppression
// and L2VNI-to-VRF association. Tracks created SVIs in svisByBridge to detect
// stale ones.
func (c *Controller) reconcileSVIs(bridgeName string, networks []evpnNetworkInfo) error {
	desiredSVIs := sets.New[string]()
	for _, net := range networks {
		if net.ipVRFVID != 0 {
			desiredSVIs.Insert(net.l3SVIName)
			if err := c.ndm.EnsureLink(netlinkdevicemanager.DeviceConfig{
				Link: &netlink.Vlan{
					LinkAttrs: netlink.LinkAttrs{Name: net.l3SVIName},
					VlanId:    net.ipVRFVID,
				},
				VLANParent: bridgeName,
				Master:     net.vrfName,
			}); err != nil {
				return fmt.Errorf("failed to ensure L3 SVI %s: %w", net.l3SVIName, err)
			}
		}

		if net.macVRFVID != 0 {
			desiredSVIs.Insert(net.l2SVIName)
			if err := c.ndm.EnsureLink(netlinkdevicemanager.DeviceConfig{
				Link: &netlink.Vlan{
					LinkAttrs: netlink.LinkAttrs{Name: net.l2SVIName},
					VlanId:    net.macVRFVID,
				},
				VLANParent: bridgeName,
				Master:     net.vrfName,
			}); err != nil {
				return fmt.Errorf("failed to ensure L2 SVI %s: %w", net.l2SVIName, err)
			}
		}
	}
	for sviName := range c.svisByBridge[bridgeName] {
		if !desiredSVIs.Has(sviName) {
			klog.Infof("SVI %s no longer applies to %s, removing it", sviName, bridgeName)
			if err := c.ndm.DeleteLink(sviName); err != nil {
				return fmt.Errorf("failed to delete stale SVI %s: %w", sviName, err)
			}
		}
	}
	c.svisByBridge[bridgeName] = desiredSVIs

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
		if err := c.ensureOVSPort(vtepName, bridgeName, net.ovsPortName, net.macVRFLSPName, net.macVRFVID); err != nil {
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
			if err := libovsdbops.DeletePortWithInterfaces(c.ovsClient, ovsBridgeInt, port.Name); err != nil {
				return fmt.Errorf("failed to delete stale OVS port %s: %w", port.Name, err)
			}
		}
	}

	return nil
}

// ensureOVSPort creates an OVS internal port on br-int, attaches it to the EVPN bridge, and sets VLAN access.
// The iface-id on the Interface external_ids is set to macVRFLSPName so that ovn-controller can bind the
// corresponding MAC-VRF logical switch port to this OVS port.
func (c *Controller) ensureOVSPort(vtepName, bridgeName, portName, macVRFLSPName string, vid int) error {
	if err := libovsdbops.CreateOrUpdatePortWithInterface(c.ovsClient, ovsBridgeInt, portName,
		map[string]string{externalIDEVPNVTEP: vtepName},
		map[string]string{"iface-id": macVRFLSPName}); err != nil {
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

	if err := util.GetNetLinkOps().BridgeVlanAdd(ovsLink, uint16(vid), true, true, false, true); err != nil {
		return fmt.Errorf("failed to set VLAN %d on OVS port %s: %w", vid, portName, err)
	}

	return nil
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
		if err := libovsdbops.DeletePortWithInterfaces(c.ovsClient, ovsBridgeInt, port.Name); err != nil {
			errs = append(errs, err)
		}
	}

	// Delete all SVIs parented to this bridge
	for sviName := range c.svisByBridge[bridgeName] {
		if err := c.ndm.DeleteLink(sviName); err != nil {
			errs = append(errs, err)
		}
	}
	delete(c.svisByBridge, bridgeName)

	if err := c.ndm.DeleteLink(GetEVPNVXLANName(vtepName, utilnet.IPv4)); err != nil {
		errs = append(errs, err)
	}
	if err := c.ndm.DeleteLink(GetEVPNVXLANName(vtepName, utilnet.IPv6)); err != nil {
		errs = append(errs, err)
	}
	if err := c.ndm.DeleteLink(bridgeName); err != nil {
		errs = append(errs, err)
	}
	return utilerrors.Join(errs...)
}

// cleanupStaleOVSPorts removes OVS ports tagged with the evpn-vtep external-id
// whose VTEP no longer exists. This handles VTEPs deleted while the controller was down,
// which the per-VTEP reconcile loop cannot catch.
func (c *Controller) cleanupStaleOVSPorts(activeVTEPs sets.Set[string]) error {
	ports, err := libovsdbops.FindOVSPortsWithPredicate(c.ovsClient, func(port *vswitchd.Port) bool {
		vtep, ok := port.ExternalIDs[externalIDEVPNVTEP]
		return ok && !activeVTEPs.Has(vtep)
	})
	if err != nil {
		return fmt.Errorf("failed to list stale OVS ports: %w", err)
	}
	var errs []error
	for _, port := range ports {
		klog.Infof("Cleaning up stale OVS port %s (VTEP %s no longer exists)", port.Name, port.ExternalIDs[externalIDEVPNVTEP])
		if err := libovsdbops.DeletePortWithInterfaces(c.ovsClient, ovsBridgeInt, port.Name); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete stale OVS port %s: %w", port.Name, err))
		}
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
