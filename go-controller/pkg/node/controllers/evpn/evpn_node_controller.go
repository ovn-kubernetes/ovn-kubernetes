package evpn

import (
	"fmt"
	"net"
	"reflect"
	"sync"

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
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/netlinkdevicemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
)

const vxlanPort = 4789

type Controller struct {
	nodeName     string
	watchFactory factory.NodeWatchFactory
	kube         kube.Interface
	ndm          *netlinkdevicemanager.Controller

	vtepController   controller.Controller
	nodeEventHandler cache.ResourceEventHandlerRegistration

	stopChan chan struct{}
	wg       *sync.WaitGroup
}

func NewController(nodeName string, wf factory.NodeWatchFactory, kube kube.Interface) (*Controller, error) {
	c := &Controller{
		nodeName:     nodeName,
		watchFactory: wf,
		kube:         kube,
		ndm:          netlinkdevicemanager.NewController(),
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

	return controller.StartWithInitialSync(c.initialSync, c.vtepController)
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

	controller.Stop(c.vtepController)

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
		if err := c.ndm.DeleteLink(GetEVPNVXLANName(vtep.Name, utilnet.IPv4)); err != nil {
			return fmt.Errorf("failed to delete VTEP VXLAN device: %w", err)
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

	return nil
}

func (c *Controller) deleteVTEPDevices(vtepName string) error {
	var errs []error
	if err := c.ndm.DeleteLink(GetEVPNVXLANName(vtepName, utilnet.IPv4)); err != nil {
		errs = append(errs, err)
	}
	if err := c.ndm.DeleteLink(GetEVPNVXLANName(vtepName, utilnet.IPv6)); err != nil {
		errs = append(errs, err)
	}
	if err := c.ndm.DeleteLink(GetEVPNDummyName(vtepName)); err != nil {
		errs = append(errs, err)
	}
	if err := c.ndm.DeleteLink(GetEVPNBridgeName(vtepName)); err != nil {
		errs = append(errs, err)
	}
	return utilerrors.Join(errs...)
}

func (c *Controller) ensureVXLAN(vxlanName, bridgeName string, srcIP net.IP) error {
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
