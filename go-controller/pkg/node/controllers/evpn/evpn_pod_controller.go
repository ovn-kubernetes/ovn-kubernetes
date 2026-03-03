package evpn

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/vishvananda/netlink"
	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kubevirt"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// neighEntries represents static neighbor and FDB entries for a pod on EVPN devices.
type neighEntries struct {
	sviName     string
	ovsPortName string
	macvrfVID   int
	ips         []net.IP
	mac         net.HardwareAddr
}

// podNeedsUpdate returns true for local pods when the annotation changed, the pod completed, or was deleted.
// For non-local pods, it returns true when the kubevirt migration target ready timestamp changes,
// so that reconcilePod can remove the local source pod's entries.
func (c *Controller) podNeedsUpdate(oldObj, newObj *corev1.Pod) bool {
	if oldObj != nil && newObj != nil &&
		oldObj.Spec.NodeName != c.nodeName && newObj.Spec.NodeName != c.nodeName {
		return oldObj.Annotations[kubevirtv1.MigrationTargetReadyTimestamp] !=
			newObj.Annotations[kubevirtv1.MigrationTargetReadyTimestamp]
	}

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

// handleLiveMigrationTargetReady is called when a non-local kubevirt migration target pod
// becomes ready. It finds the local source pod and removes its neighbor/FDB entries so
// FRR withdraws the Type-2 routes from this node.
func (c *Controller) handleLiveMigrationTargetReady(targetPod *corev1.Pod) error {
	migrationStatus, err := kubevirt.DiscoverLiveMigrationStatus(c.podLister, targetPod)
	if err != nil {
		return fmt.Errorf("failed to discover live migration status: %w", err)
	}
	if migrationStatus == nil || !migrationStatus.IsTargetDomainReady() {
		return nil
	}
	if migrationStatus.SourcePod.Spec.NodeName != c.nodeName {
		return nil
	}

	key, err := cache.MetaNamespaceKeyFunc(migrationStatus.SourcePod)
	if err != nil {
		return err
	}
	klog.Infof("Live migration target %s/%s ready, removing entries for local source pod %s",
		targetPod.Namespace, targetPod.Name, key)
	return c.deletePodNeighbors(key)
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
		// Non-local pod: this is a kubevirt migration target whose ready timestamp changed.
		// Find the local source pod and remove its entries so FRR withdraws the Type-2 routes.
		return c.handleLiveMigrationTargetReady(pod)
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
		sviName:     GetEVPNL2SVIName(netInfo),
		ovsPortName: GetEVPNOVSPortName(netInfo),
		macvrfVID:   netInfo.EVPNMACVRFVID(),
		mac:         podAnnotation.MAC,
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

// ensurePodNeighbors programs static FDB and neighbor entries for a pod's MAC/IPs.
// The static FDB entry on the OVS port prevents the bridge from aging out the MAC,
// which would cause FRR to withdraw the Type-2 route.
func (c *Controller) ensurePodNeighbors(entries *neighEntries) error {
	svi, err := util.GetNetLinkOps().LinkByName(entries.sviName)
	if err != nil {
		return fmt.Errorf("failed to get L2 SVI %s: %w", entries.sviName, err)
	}
	ovsPort, err := util.GetNetLinkOps().LinkByName(entries.ovsPortName)
	if err != nil {
		return fmt.Errorf("failed to get OVS port %s: %w", entries.ovsPortName, err)
	}
	if err := util.LinkFDBAdd(ovsPort, entries.mac, entries.macvrfVID); err != nil {
		if !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("failed to add FDB entry for %s on %s: %w", entries.mac, entries.ovsPortName, err)
		}
	}
	klog.V(5).Infof("Configured FDB %s vlan %d on %s", entries.mac, entries.macvrfVID, entries.ovsPortName)
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

	ovsPort, err := util.GetNetLinkOps().LinkByName(entries.ovsPortName)
	if err != nil {
		return fmt.Errorf("ovs port %s not found for pod %s", entries.ovsPortName, key)
	}

	if err := util.LinkFDBDel(ovsPort, entries.mac, entries.macvrfVID); err != nil {
		return fmt.Errorf("failed to delete FDB entry %s from %s: %w", entries.mac, entries.ovsPortName, err)
	}
	klog.V(5).Infof("Deleted FDB %s from %s for pod %s", entries.mac, entries.ovsPortName, key)

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
		sviName     string
		ovsPortName string
		macvrfVID   int
	}
	evpnNetworks := make(map[string]evpnDevices)
	err := c.networkMgr.DoWithLock(func(netInfo util.NetInfo) error {
		if netInfo == nil || netInfo.EVPNVTEPName() == "" {
			return nil
		}
		evpnNetworks[netInfo.GetNetworkName()] = evpnDevices{
			sviName:     GetEVPNL2SVIName(netInfo),
			ovsPortName: GetEVPNOVSPortName(netInfo),
			macvrfVID:   netInfo.EVPNMACVRFVID(),
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
	desiredMACsByOVSPort := make(map[string]sets.Set[string])
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

		entries := &neighEntries{sviName: nsInfo.sviName, ovsPortName: nsInfo.ovsPortName, macvrfVID: nsInfo.macvrfVID, mac: podAnnotation.MAC}
		if desiredMACsByOVSPort[nsInfo.ovsPortName] == nil {
			desiredMACsByOVSPort[nsInfo.ovsPortName] = sets.New[string]()
		}
		desiredMACsByOVSPort[nsInfo.ovsPortName].Insert(podAnnotation.MAC.String())
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

		// Clean up stale FDB entries on OVS port
		ovsPort, err := util.GetNetLinkOps().LinkByName(dev.ovsPortName)
		if err != nil {
			klog.V(5).Infof("OVS port %s not found, skipping", dev.ovsPortName)
			continue
		}
		fdbs, err := util.GetNetLinkOps().NeighList(ovsPort.Attrs().Index, syscall.AF_BRIDGE)
		if err != nil {
			klog.Errorf("Failed to get FDB entries from %s: %v", dev.ovsPortName, err)
			continue
		}
		desiredMACs := desiredMACsByOVSPort[dev.ovsPortName]
		for _, f := range fdbs {
			if f.State&netlink.NUD_NOARP == 0 || f.Flags&netlink.NTF_MASTER == 0 {
				continue
			}
			if desiredMACs == nil || !desiredMACs.Has(f.HardwareAddr.String()) {
				if err := util.LinkFDBDel(ovsPort, f.HardwareAddr, f.Vlan); err != nil {
					klog.Errorf("Failed to delete stale FDB %s from %s: %v", f.HardwareAddr, dev.ovsPortName, err)
				} else {
					klog.V(5).Infof("Deleted stale FDB %s from %s", f.HardwareAddr, dev.ovsPortName)
				}
			}
		}
	}
	return nil
}
