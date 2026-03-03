package evpn

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// neighEntries represents static neighbor entries for a pod on EVPN devices.
type neighEntries struct {
	sviName   string
	macvrfVID int
	ips       []net.IP
	mac       net.HardwareAddr
}

// podNeedsUpdate returns true for local pods when the annotation changed, the pod completed, or was deleted.
func (c *Controller) podNeedsUpdate(oldObj, newObj *corev1.Pod) bool {
	var oldAnnot, newAnnot string
	if oldObj != nil {
		if oldObj.Spec.NodeName != c.nodeName {
			return false
		}
		oldAnnot = oldObj.Annotations[util.OvnPodAnnotationName]
	}
	if newObj != nil {
		if newObj.Spec.NodeName != c.nodeName {
			return false
		}
		if util.PodCompleted(newObj) {
			return true
		}
		newAnnot = newObj.Annotations[util.OvnPodAnnotationName]
	}
	return oldAnnot != newAnnot
}

func (c *Controller) reconcilePod(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	pod, err := c.podLister.Pods(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.deletePodNeighbors(key)
		}
		return err
	}

	if pod.Spec.NodeName != c.nodeName {
		return nil
	}

	if util.PodCompleted(pod) {
		return c.deletePodNeighbors(key)
	}

	// If we already have cached entries, ensure the neighbors are present without re-parsing the annotation.
	c.podNeighLock.Lock()
	existing, hasExisting := c.podNeighbors[key]
	c.podNeighLock.Unlock()
	if hasExisting {
		return c.ensurePodNeighbors(existing)
	}

	nadKey, err := c.networkMgr.GetPrimaryNADForNamespace(pod.Namespace)
	if err != nil {
		return err
	}
	if nadKey == types.DefaultNetworkName {
		return nil
	}

	netInfo := c.networkMgr.GetNetInfoForNADKey(nadKey)
	if netInfo == nil || netInfo.EVPNMACVRFVNI() == 0 {
		return nil
	}

	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
	if err != nil {
		return err
	}

	entries := &neighEntries{
		sviName:   GetEVPNL2SVIName(netInfo),
		macvrfVID: netInfo.EVPNMACVRFVID(),
		mac:       podAnnotation.MAC,
	}
	for _, ipNet := range podAnnotation.IPs {
		entries.ips = append(entries.ips, ipNet.IP)
	}
	if err := c.ensurePodNeighbors(entries); err != nil {
		return err
	}

	c.podNeighLock.Lock()
	c.podNeighbors[key] = entries
	c.podNeighLock.Unlock()

	return nil
}

// ensurePodNeighbors programs static neighbor entries for a pod's MAC/IPs.
func (c *Controller) ensurePodNeighbors(entries *neighEntries) error {
	svi, err := util.GetNetLinkOps().LinkByName(entries.sviName)
	if err != nil {
		return fmt.Errorf("failed to get L2 SVI %s: %w", entries.sviName, err)
	}

	for _, ip := range entries.ips {
		if err := util.LinkNeighAdd(svi, ip, entries.mac); err != nil {
			if !errors.Is(err, syscall.EEXIST) {
				return fmt.Errorf("failed to add neighbor %s on %s: %w", ip, entries.sviName, err)
			}
		}
		klog.V(5).Infof("Configured neighbor %s lladdr %s on %s", ip, entries.mac, entries.sviName)
	}
	return nil
}

func (c *Controller) deletePodNeighbors(key string) error {
	c.podNeighLock.Lock()
	entries, ok := c.podNeighbors[key]
	c.podNeighLock.Unlock()
	if !ok {
		return nil
	}

	link, err := util.GetNetLinkOps().LinkByName(entries.sviName)
	if err != nil {
		return fmt.Errorf("svi %s not found, skipping neighbor deletion for pod %s", entries.sviName, key)
	}
	for _, ip := range entries.ips {
		if err := util.LinkNeighDel(link, ip); err != nil {
			return fmt.Errorf("failed to delete neighbor %s from %s: %w", ip, entries.sviName, err)
		}
		klog.V(5).Infof("Deleted neighbor %s lladdr %s from %s for pod %s", ip, entries.mac, entries.sviName, key)
	}

	c.podNeighLock.Lock()
	delete(c.podNeighbors, key)
	c.podNeighLock.Unlock()
	return nil
}

// podInitialSync adds missing neighbor entries and removes stale ones.
func (c *Controller) podInitialSync() error {
	if !util.WaitForInformerCacheSyncWithTimeout("evpn-pod", c.stopChan, c.watchFactory.PodCoreInformer().Informer().HasSynced) {
		return fmt.Errorf("timed out waiting for pod informer cache to sync")
	}

	type evpnDevices struct {
		sviName   string
		macvrfVID int
	}
	evpnNetworks := make(map[string]evpnDevices)
	err := c.networkMgr.DoWithLock(func(netInfo util.NetInfo) error {
		if netInfo == nil || netInfo.EVPNVTEPName() == "" {
			return nil
		}
		evpnNetworks[netInfo.GetNetworkName()] = evpnDevices{
			sviName:   GetEVPNL2SVIName(netInfo),
			macvrfVID: netInfo.EVPNMACVRFVID(),
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to collect EVPN networks: %w", err)
	}

	type nsEVPNInfo struct {
		nadKey string
		evpnDevices
	}
	evpnNamespaces := make(map[string]nsEVPNInfo)
	for networkName, dev := range evpnNetworks {
		for _, nadKey := range c.networkMgr.GetNADKeysForNetwork(networkName) {
			ns, _, _ := cache.SplitMetaNamespaceKey(nadKey)
			evpnNamespaces[ns] = nsEVPNInfo{nadKey: nadKey, evpnDevices: dev}
		}
	}

	pods, err := c.podLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	desiredBySVI := make(map[string]sets.Set[string])
	for _, pod := range pods {
		if pod.Spec.NodeName != c.nodeName || util.PodCompleted(pod) {
			continue
		}

		nsInfo, ok := evpnNamespaces[pod.Namespace]
		if !ok {
			continue
		}

		key, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			klog.Errorf("Failed to get namespace key for pod %s: %v", pod.Name, err)
			continue
		}

		podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nsInfo.nadKey)
		if err != nil {
			klog.Errorf("Failed to unmarshal pod annotation for pod %s: %v", pod.Name, err)
			continue
		}

		entries := &neighEntries{sviName: nsInfo.sviName, macvrfVID: nsInfo.macvrfVID, mac: podAnnotation.MAC}

		for _, ipNet := range podAnnotation.IPs {
			if desiredBySVI[nsInfo.sviName] == nil {
				desiredBySVI[nsInfo.sviName] = sets.New[string]()
			}
			desiredBySVI[nsInfo.sviName].Insert(ipNet.IP.String())
			entries.ips = append(entries.ips, ipNet.IP)
		}

		if err := c.ensurePodNeighbors(entries); err != nil {
			klog.Errorf("Failed to ensure pod neighbor entries: %v", err)
			continue
		}

		c.podNeighLock.Lock()
		c.podNeighbors[key] = entries
		c.podNeighLock.Unlock()
	}

	for _, dev := range evpnNetworks {
		// Clean up stale neighbor entries on SVI
		svi, err := util.GetNetLinkOps().LinkByName(dev.sviName)
		if err != nil {
			klog.V(5).Infof("SVI %s not found, skipping", dev.sviName)
			continue
		}
		for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
			neighs, err := util.GetNetLinkOps().NeighList(svi.Attrs().Index, family)
			if err != nil {
				klog.Errorf("Failed to get neighbor entries from %s: %v", dev.sviName, err)
				continue
			}
			desiredIPs := desiredBySVI[dev.sviName]
			for _, n := range neighs {
				if n.State&netlink.NUD_PERMANENT == 0 {
					continue
				}
				if desiredIPs == nil || !desiredIPs.Has(n.IP.String()) {
					if err := util.LinkNeighDel(svi, n.IP); err != nil {
						klog.Errorf("Failed to delete stale neighbor %s from %s: %v", n.IP, dev.sviName, err)
					} else {
						klog.V(5).Infof("Deleted stale neighbor %s lladdr %s from %s", n.IP, n.HardwareAddr, dev.sviName)
					}
				}
			}
		}
	}
	return nil
}
