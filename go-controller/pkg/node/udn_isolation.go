package node

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	nadlister "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/knftables"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	nodenft "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	nftablesUDNPodIPsv4 = "udn-pod-default-ips-v4"
	nftablesUDNPodIPsv6 = "udn-pod-default-ips-v6"
)

// UDNHostIsolationManager manages the host isolation for user defined networks.
// It uses nftables chain "udn-isolation" to only allow connection to primary UDN pods from kubelet.
// It also listens to systemd events to re-apply the rules after kubelet restart as cgroup matching is used.
type UDNHostIsolationManager struct {
	nft               knftables.Interface
	ipv4, ipv6        bool
	podController     controller.Controller
	podLister         corelisters.PodLister
	nadLister         nadlister.NetworkAttachmentDefinitionLister
	kubeletCgroupPath string

	udnPodIPsv4 *nftPodIPSet
	udnPodIPsv6 *nftPodIPSet
}

func NewUDNHostIsolationManager(ipv4, ipv6 bool, podInformer coreinformers.PodInformer,
	nadLister nadlister.NetworkAttachmentDefinitionLister) *UDNHostIsolationManager {
	m := &UDNHostIsolationManager{
		podLister:   podInformer.Lister(),
		nadLister:   nadLister,
		ipv4:        ipv4,
		ipv6:        ipv6,
		udnPodIPsv4: newNFTPodIPSet(nftablesUDNPodIPsv4),
		udnPodIPsv6: newNFTPodIPSet(nftablesUDNPodIPsv6),
	}
	controllerConfig := &controller.ControllerConfig[v1.Pod]{
		RateLimiter:    workqueue.NewItemFastSlowRateLimiter(time.Second, 5*time.Second, 5),
		Informer:       podInformer.Informer(),
		Lister:         podInformer.Lister().List,
		ObjNeedsUpdate: podNeedsUpdate,
		Reconcile:      m.reconcilePod,
		Threadiness:    1,
	}
	m.podController = controller.NewController[v1.Pod]("udn-host-isolation-manager", controllerConfig)
	return m
}

// Start must be called on node setup.
func (m *UDNHostIsolationManager) Start(ctx context.Context) error {
	// find kubelet cgroup path.
	// kind cluster uses "kubelet.slice/kubelet.service", while OCP cluster uses "system.slice/kubelet.service".
	// as long as ovn-k node is running as a privileged container, we can access the host cgroup directory.
	err := filepath.WalkDir("/sys/fs/cgroup", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Name() == "kubelet.service" {
			m.kubeletCgroupPath = strings.TrimPrefix(path, "/sys/fs/cgroup/")
			klog.Infof("Found kubelet cgroup path: %s", m.kubeletCgroupPath)
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil || m.kubeletCgroupPath == "" {
		return fmt.Errorf("failed to find kubelet cgroup path: %w", err)
	}
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed getting nftables helper: %w", err)
	}
	m.nft = nft
	if err = m.setupUDNFromHostIsolation(); err != nil {
		return fmt.Errorf("failed to setup UDN host isolation: %w", err)
	}
	if err = m.runKubeletRestartTracker(ctx); err != nil {
		return fmt.Errorf("failed to run kubelet restart tracker: %w", err)
	}
	return controller.StartWithInitialSync(m.podInitialSync, m.podController)
}

func (m *UDNHostIsolationManager) Stop() {
	controller.Stop(m.podController)
}

func CleanupUDNHostIsolation() error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed getting nftables helper: %w", err)
	}
	tx := nft.NewTransaction()
	safeDelete(tx, &knftables.Chain{
		Name: nodenft.UDNIsolationChain,
	})
	safeDelete(tx, &knftables.Set{
		Name:    nftablesUDNPodIPsv4,
		Comment: knftables.PtrTo("default network IPs of pods in user defined networks (IPv4)"),
		Type:    "ipv4_addr",
	})
	safeDelete(tx, &knftables.Set{
		Name:    nftablesUDNPodIPsv6,
		Comment: knftables.PtrTo("default network IPs of pods in user defined networks (IPv6)"),
		Type:    "ipv6_addr",
	})
	return nft.Run(context.TODO(), tx)
}

func (m *UDNHostIsolationManager) setupUDNFromHostIsolation() error {
	tx := m.nft.NewTransaction()
	tx.Add(&knftables.Chain{
		Name:     nodenft.UDNIsolationChain,
		Comment:  knftables.PtrTo("Host isolation for user defined networks"),
		Type:     knftables.PtrTo(knftables.FilterType),
		Hook:     knftables.PtrTo(knftables.OutputHook),
		Priority: knftables.PtrTo(knftables.FilterPriority),
	})
	tx.Flush(&knftables.Chain{
		Name: nodenft.UDNIsolationChain,
	})
	tx.Add(&knftables.Set{
		Name:    nftablesUDNPodIPsv4,
		Comment: knftables.PtrTo("default network IPs of pods in user defined networks (IPv4)"),
		Type:    "ipv4_addr",
	})
	tx.Add(&knftables.Set{
		Name:    nftablesUDNPodIPsv6,
		Comment: knftables.PtrTo("default network IPs of pods in user defined networks (IPv6)"),
		Type:    "ipv6_addr",
	})
	m.addRules(tx)

	err := m.nft.Run(context.TODO(), tx)
	if err != nil {
		return fmt.Errorf("could not setup nftables rules for UDN from host isolation: %v", err)
	}
	return nil
}

func (m *UDNHostIsolationManager) addRules(tx *knftables.Transaction) {
	if m.ipv4 {
		tx.Add(&knftables.Rule{
			Chain: nodenft.UDNIsolationChain,
			Rule: knftables.Concat(
				"socket", "cgroupv2", "level 2", m.kubeletCgroupPath,
				"ip", "daddr", "@", nftablesUDNPodIPsv4, "accept"),
		})
		tx.Add(&knftables.Rule{
			Chain: nodenft.UDNIsolationChain,
			Rule: knftables.Concat(
				"ip", "daddr", "@", nftablesUDNPodIPsv4, "drop"),
		})
	}
	if m.ipv6 {
		tx.Add(&knftables.Rule{
			Chain: nodenft.UDNIsolationChain,
			Rule: knftables.Concat(
				"socket", "cgroupv2", "level 2", m.kubeletCgroupPath,
				"ip6", "daddr", "@", nftablesUDNPodIPsv6, "accept"),
		})
		tx.Add(&knftables.Rule{
			Chain: nodenft.UDNIsolationChain,
			Rule: knftables.Concat(
				"ip6", "daddr", "@", nftablesUDNPodIPsv6, "drop"),
		})
	}
}

func (m *UDNHostIsolationManager) updateKubeletCgroup() error {
	tx := m.nft.NewTransaction()
	tx.Flush(&knftables.Chain{
		Name: nodenft.UDNIsolationChain,
	})
	m.addRules(tx)

	err := m.nft.Run(context.TODO(), tx)
	if err != nil {
		return fmt.Errorf("could not update nftables rule for management port: %v", err)
	}
	return nil
}

func (m *UDNHostIsolationManager) runKubeletRestartTracker(ctx context.Context) (err error) {
	conn, err := dbus.NewSystemdConnectionContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to systemd: %w", err)
	}
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()

	err = conn.Subscribe()
	if err != nil {
		return fmt.Errorf("failed to subscribe to systemd events: %w", err)
	}
	// interval is important here as we need to catch the restart state, before it is running again
	events, errChan := conn.SubscribeUnitsCustom(50*time.Millisecond, 0, func(u1, u2 *dbus.UnitStatus) bool { return *u1 != *u2 },
		func(s string) bool {
			return s != "kubelet.service"
		})
	// run until context is cancelled
	go func() {
		waitingForActive := false
		for {
			select {
			case <-ctx.Done():
				conn.Close()
				return
			case event := <-events:
				for _, status := range event {
					if status.ActiveState != "active" {
						waitingForActive = true
					} else if waitingForActive {
						klog.Infof("Kubelet was restarted, re-applying UDN host isolation")
						err = m.updateKubeletCgroup()
						if err != nil {
							klog.Errorf("Failed to re-apply UDN host isolation: %v", err)
						} else {
							waitingForActive = false
						}
					}
				}
			case err := <-errChan:
				klog.Errorf("Systemd listener error: %v", err)
			}
		}
	}()
	return nil
}

func (m *UDNHostIsolationManager) podInitialSync() error {
	pods, err := m.podLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list pods: %v", err)
	}
	v4PodsIPs := map[string]sets.Set[string]{}
	v6PodsIPs := map[string]sets.Set[string]{}
	for _, pod := range pods {
		// only add pods with primary UDN
		primaryUDN, err := m.isPodPrimaryUDN(pod)
		if err != nil {
			return fmt.Errorf("failed to check if pod %s in namespace %s is in primary UDN: %w", pod.Name, pod.Namespace, err)
		}
		if !primaryUDN {
			continue
		}

		podIPs, err := util.DefaultNetworkPodIPs(pod)
		if err != nil {
			return fmt.Errorf("failed to get pod IPs for pod %s in namespace %s: %v", pod.Name, pod.Namespace, err)
		}
		if podIPs == nil {
			continue
		}
		newV4IPs, newV6IPs := splitIPsPerFamily(podIPs)
		podKey, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			klog.Warningf("UDNHostIsolationManager failed to get key for pod %s in namespace %s: %v", pod.Name, pod.Namespace, err)
			continue
		}
		v4PodsIPs[podKey] = newV4IPs
		v6PodsIPs[podKey] = newV6IPs
	}
	if err = m.udnPodIPsv4.fullSync(m.nft, v4PodsIPs); err != nil {
		return err
	}
	if err = m.udnPodIPsv6.fullSync(m.nft, v6PodsIPs); err != nil {
		return err
	}
	return nil
}

func podNeedsUpdate(oldObj, newObj *v1.Pod) bool {
	if oldObj == nil || newObj == nil {
		return true
	}
	// react to pod IP changes
	return !reflect.DeepEqual(oldObj.Status, newObj.Status) || !reflect.DeepEqual(oldObj.Annotations, newObj.Annotations)
}

func (m *UDNHostIsolationManager) reconcilePod(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("UDNHostIsolationManager failed to split meta namespace cache key %s for pod: %v", key, err)
		return nil
	}
	pod, err := m.podLister.Pods(namespace).Get(name)
	if err != nil {
		if kerrors.IsNotFound(err) {
			// Pod was deleted, clean up.
			return m.updateUDNPodIPs(key, nil)
		}
		return fmt.Errorf("failed to fetch pod %s in namespace %s", name, namespace)
	}
	// only add pods with primary UDN
	primaryUDN, err := m.isPodPrimaryUDN(pod)
	if err != nil {
		return fmt.Errorf("failed to check if pod %s in namespace %s is in primary UDN: %w", name, namespace, err)
	}
	if !primaryUDN {
		return nil
	}
	podIPs, err := util.DefaultNetworkPodIPs(pod)
	if err != nil {
		// update event should come later with ips
		klog.V(5).Infof("Failed to get default network pod IPs for pod %s in namespace %s: %v", name, namespace, err)
		return nil
	}
	return m.updateUDNPodIPs(key, podIPs)
}

func (m *UDNHostIsolationManager) isPodPrimaryUDN(pod *v1.Pod) (bool, error) {
	activeNetwork, err := util.GetActiveNetworkForNamespace(pod.Namespace, m.nadLister)
	if err != nil {
		return false, err
	}
	return activeNetwork.IsPrimaryNetwork() && !activeNetwork.IsDefault(), nil
}

func (m *UDNHostIsolationManager) updateUDNPodIPs(namespacedName string, podIPs []net.IP) error {
	tx := m.nft.NewTransaction()
	newV4IPs, newV6IPs := splitIPsPerFamily(podIPs)

	m.udnPodIPsv4.updatePodIPsTX(namespacedName, newV4IPs, tx)
	m.udnPodIPsv6.updatePodIPsTX(namespacedName, newV6IPs, tx)

	if tx.NumOperations() == 0 {
		return nil
	}

	err := m.nft.Run(context.TODO(), tx)
	if err != nil {
		return fmt.Errorf("could not update nftables set for UDN pods: %v", err)
	}

	// update internal state only after successful transaction
	m.udnPodIPsv4.updatePodIPsAfterTX(namespacedName, newV4IPs)
	m.udnPodIPsv6.updatePodIPsAfterTX(namespacedName, newV6IPs)
	return nil
}

// nftPodIPSet is a helper struct to manage pod IPs in nftables set.
// Can be used for ipv4 and ipv6 sets.
type nftPodIPSet struct {
	setName string
	// podName: ips
	podsIPs map[string]sets.Set[string]
}

func newNFTPodIPSet(setName string) *nftPodIPSet {
	return &nftPodIPSet{
		setName: setName,
		podsIPs: make(map[string]sets.Set[string]),
	}
}

// updatePodIPsTX adds transaction operations to update pod IPs in nftables set.
// To update internal struct, updatePodIPsAfterTX must be called if transaction is successful.
func (n *nftPodIPSet) updatePodIPsTX(namespacedName string, podIPs sets.Set[string], tx *knftables.Transaction) {
	if n.podsIPs[namespacedName].Equal(podIPs) {
		return
	}
	// always delete all old ips, then add new ips.
	for existingIP := range n.podsIPs[namespacedName] {
		tx.Delete(&knftables.Element{
			Set: n.setName,
			Key: []string{existingIP},
		})
	}
	for newIP := range podIPs {
		tx.Add(&knftables.Element{
			Set: n.setName,
			Key: []string{newIP},
		})
	}
}

func (n *nftPodIPSet) updatePodIPsAfterTX(namespacedName string, podIPs sets.Set[string]) {
	n.podsIPs[namespacedName] = podIPs
}

// fullSync should be called on restart to sync all pods IPs.
// It flushes existing ips, and adds new ips.
func (n *nftPodIPSet) fullSync(nft knftables.Interface, podsIPs map[string]sets.Set[string]) error {
	tx := nft.NewTransaction()
	tx.Flush(&knftables.Set{
		Name: n.setName,
	})
	for podName, podIPs := range podsIPs {
		if len(podIPs) == 0 {
			continue
		}
		for ip := range podIPs {
			tx.Add(&knftables.Element{
				Set: n.setName,
				Key: []string{ip},
			})
		}
		n.podsIPs[podName] = podIPs
	}
	err := nft.Run(context.TODO(), tx)
	if err != nil {
		clear(n.podsIPs)
		return fmt.Errorf("initial pods sync for UDN host isolation failed: %w", err)
	}
	return nil
}

func splitIPsPerFamily(podIPs []net.IP) (sets.Set[string], sets.Set[string]) {
	newV4IPs := sets.New[string]()
	newV6IPs := sets.New[string]()
	for _, podIP := range podIPs {
		if podIP.To4() != nil {
			newV4IPs.Insert(podIP.String())
		} else {
			newV6IPs.Insert(podIP.String())
		}
	}
	return newV4IPs, newV6IPs
}

func safeDelete(tx *knftables.Transaction, obj knftables.Object) {
	tx.Add(obj)
	tx.Delete(obj)
}
