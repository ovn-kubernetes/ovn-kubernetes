package egressip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gaissmai/cidrtree"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
	utilnet "k8s.io/utils/net"

	ovnconfig "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/informers/externalversions/egressip/v1"
	egressiplisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/listers/egressip/v1"
	egressiptrafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressiptraffic/v1"
	egressiptrafficinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressiptraffic/v1/apis/informers/externalversions/egressiptraffic/v1"
	egressiptrafficlisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressiptraffic/v1/apis/listers/egressiptraffic/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iptables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/linkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/egressip"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

const (
	ruleEIPTrafficPriority = 6000 // higher priority for trafficSelector EgressIPs with per-destination IP rules
	rulePriority           = 6001 // the priority of the ip routing rules created by the controller. Egress Service priority is 5000.
	ruleFwMarkPriority     = 5999 // the priority of the ip routing rules for LGW mode when we want to skip processing eip ip rules because dst is a node ip. Pkt will be fw marked with 1008.
	routingTableIDStart    = 1000
	chainName              = "OVN-KUBE-EGRESS-IP-MULTI-NIC"
	iptChainName           = utiliptables.Chain(chainName)
	maxRetries             = 15
)

var (
	_, defaultV4AnyCIDR, _ = net.ParseCIDR("0.0.0.0/0")
	_, defaultV6AnyCIDR, _ = net.ParseCIDR("::/0")
	_, linkLocalCIDR, _    = net.ParseCIDR("fe80::/64")
	iptJumpRule            = iptables.RuleArg{Args: []string{"-j", chainName}}
	iptSaveMarkRule        = iptables.RuleArg{Args: []string{"-m", "mark", "--mark", "1008", "-j", "CONNMARK", "--save-mark"}} // 1008 is pkt mark for node ip
	iptRestoreMarkRule     = iptables.RuleArg{Args: []string{"-m", "mark", "--mark", "0", "-j", "CONNMARK", "--restore-mark"}}
)

// eIPConfig represents exactly one EgressIP IP. It contains non-pod related EIP configuration information only.
type eIPConfig struct {
	// EgressIP IP
	addr   *netlink.Addr
	routes []netlink.Route
}

func newEIPConfig() *eIPConfig {
	return &eIPConfig{}
}

// state contains current state for an EgressIP as it was applied.
type state struct {
	// namespaceName -> pod ns/name -> pod IP configuration
	namespacesWithPodIPConfigs map[string]map[ktypes.NamespacedName]*podIPConfigList
	// eIPConfig IP contains all applied configuration for a given EgressIP IP. It does not contain any pod specific config
	eIPConfig *eIPConfig
}

func newState() *state {
	return &state{
		namespacesWithPodIPConfigs: map[string]map[ktypes.NamespacedName]*podIPConfigList{},
		eIPConfig:                  newEIPConfig(),
	}
}

// config is used to update an EIP to the latest state, it stores all required information for an
// update.
type config struct {
	// namespaceName -> pod ns/name -> pod IP configuration
	namespacesWithPodIPConfigs map[string]map[ktypes.NamespacedName]*podIPConfigList
	// eIPConfig IP contains all applied configuration for a given EgressIP IP. It does not contain any pod specific config
	eIPConfig *eIPConfig
}

// referencedObjects is used by pod and namespace handlers to find what is selected for an EgressIP
type referencedObjects struct {
	eIPNamespaces sets.Set[string]
	eIPPods       sets.Set[ktypes.NamespacedName]
}

// getActiveNetworkForNamespaceFn returns a NetInfo which contains NADs which refer to a network in addition to the basic
// network information.
type getActiveNetworkForNamespaceFn func(namespace string) (util.NetInfo, error)

// Controller implement Egress IP for secondary host networks
type Controller struct {
	eIPLister          egressiplisters.EgressIPLister
	eIPInformer        cache.SharedIndexInformer
	eIPQueue           workqueue.TypedRateLimitingInterface[string]
	eIPTrafficLister   egressiptrafficlisters.EgressIPTrafficLister
	eIPTrafficInformer cache.SharedIndexInformer
	nodeLister         corelisters.NodeLister
	namespaceLister    corelisters.NamespaceLister
	namespaceInformer  cache.SharedIndexInformer
	namespaceQueue     workqueue.TypedRateLimitingInterface[*corev1.Namespace]

	podLister                    corelisters.PodLister
	podInformer                  cache.SharedIndexInformer
	podQueue                     workqueue.TypedRateLimitingInterface[*corev1.Pod]
	getActiveNetworkForNamespace getActiveNetworkForNamespaceFn

	// cache is a cache of configuration states for EIPs, key is EgressIP Name.
	cache *syncmap.SyncMap[*state]

	// referencedObjects should only be accessed with referencedObjectsLock
	referencedObjectsLock sync.RWMutex
	// referencedObjects is a cache of objects that every EIP has selected for its config.
	// With this cache namespace and pod handlers may fetch affected EIP config.
	// key is EIP name.
	referencedObjects map[string]*referencedObjects

	routeManager    *routemanager.Controller
	linkManager     *linkmanager.Controller
	ruleManager     *iprulemanager.Controller
	iptablesManager *iptables.Controller
	kube            kube.Interface
	nodeName        string
	v4              bool
	v6              bool
}

func NewController(k kube.Interface, eIPInformer egressipinformer.EgressIPInformer, eIPTrafficInformer egressiptrafficinformer.EgressIPTrafficInformer,
	nodeInformer cache.SharedIndexInformer,
	namespaceInformer coreinformers.NamespaceInformer, podInformer coreinformers.PodInformer, getActiveNetworkForNamespaceFn getActiveNetworkForNamespaceFn,
	routeManager *routemanager.Controller, v4, v6 bool, nodeName string, linkManager *linkmanager.Controller) (*Controller, error) {

	c := &Controller{
		eIPLister:   eIPInformer.Lister(),
		eIPInformer: eIPInformer.Informer(),
		eIPQueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemFastSlowRateLimiter[string](time.Second, 5*time.Second, 5),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "eipeip"},
		),
		nodeLister:        corelisters.NewNodeLister(nodeInformer.GetIndexer()),
		namespaceLister:   namespaceInformer.Lister(),
		namespaceInformer: namespaceInformer.Informer(),
		namespaceQueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemFastSlowRateLimiter[*corev1.Namespace](time.Second, 5*time.Second, 5),
			workqueue.TypedRateLimitingQueueConfig[*corev1.Namespace]{Name: "eipnamespace"},
		),
		podLister:   podInformer.Lister(),
		podInformer: podInformer.Informer(),
		podQueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemFastSlowRateLimiter[*corev1.Pod](time.Second, 5*time.Second, 5),
			workqueue.TypedRateLimitingQueueConfig[*corev1.Pod]{Name: "eippods"},
		),
		getActiveNetworkForNamespace: getActiveNetworkForNamespaceFn,
		cache:                        syncmap.NewSyncMap[*state](),
		referencedObjectsLock:        sync.RWMutex{},
		referencedObjects:            map[string]*referencedObjects{},
		routeManager:                 routeManager,
		linkManager:                  linkManager,
		ruleManager:                  iprulemanager.NewController(v4, v6),
		iptablesManager:              iptables.NewController(),
		kube:                         k,
		nodeName:                     nodeName,
		v4:                           v4,
		v6:                           v6,
	}
	if ovnconfig.OVNKubernetesFeature.EnableEgressIPTraffic {
		c.eIPTrafficLister = eIPTrafficInformer.Lister()
		c.eIPTrafficInformer = eIPTrafficInformer.Informer()
	}
	return c, nil
}

// Run starts the Egress IP that is hosted in secondary host networks. Changes to this function
// need to be mirrored in test function setupFakeTestNode
func (c *Controller) Run(stopCh <-chan struct{}, wg *sync.WaitGroup, threads int) error {
	klog.Info("Starting Egress IP Controller")
	_, err := c.namespaceInformer.AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onNamespaceAdd,
			UpdateFunc: c.onNamespaceUpdate,
			DeleteFunc: c.onNamespaceDelete,
		}))
	if err != nil {
		return err
	}
	_, err = c.podInformer.AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onPodAdd,
			UpdateFunc: c.onPodUpdate,
			DeleteFunc: c.onPodDelete,
		}))
	if err != nil {
		return err
	}
	_, err = c.eIPInformer.AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onEIPAdd,
			UpdateFunc: c.onEIPUpdate,
			DeleteFunc: c.onEIPDelete,
		}))
	if err != nil {
		return err
	}
	if c.eIPTrafficInformer != nil {
		_, err = c.eIPTrafficInformer.AddEventHandler(
			cache.ResourceEventHandlerFuncs{
				AddFunc: c.onEIPTrafficChange,
				UpdateFunc: func(oldObj, newObj interface{}) {
					oldEIPT := oldObj.(*egressiptrafficv1.EgressIPTraffic)
					newEIPT := newObj.(*egressiptrafficv1.EgressIPTraffic)
					if !reflect.DeepEqual(oldEIPT.Labels, newEIPT.Labels) {
						c.onEIPTrafficChange(oldObj)
					}
					c.onEIPTrafficChange(newObj)
				},
				DeleteFunc: c.onEIPTrafficChange,
			})
		if err != nil {
			return err
		}
	}

	syncWg := &sync.WaitGroup{}
	var syncErrs []error
	cacheSyncs := []struct {
		resourceName string
		syncFn       cache.InformerSynced
	}{
		{"eipeip", c.eIPInformer.HasSynced},
		{"eipnamespace", c.namespaceInformer.HasSynced},
		{"eippod", c.podInformer.HasSynced},
	}
	if c.eIPTrafficInformer != nil {
		cacheSyncs = append(cacheSyncs, struct {
			resourceName string
			syncFn       cache.InformerSynced
		}{"eiptraffic", c.eIPTrafficInformer.HasSynced})
	}
	for _, se := range cacheSyncs {
		syncWg.Add(1)
		go func(resourceName string, syncFn cache.InformerSynced) {
			defer syncWg.Done()
			if !util.WaitForInformerCacheSyncWithTimeout(resourceName, stopCh, syncFn) {
				syncErrs = append(syncErrs, fmt.Errorf("timed out waiting for %q caches to sync", resourceName))
			}
		}(se.resourceName, se.syncFn)
	}
	syncWg.Wait()
	if len(syncErrs) != 0 {
		return utilerrors.Join(syncErrs...)
	}

	wg.Add(1)
	go func() {
		c.iptablesManager.Run(stopCh, 6*time.Minute)
		wg.Done()
	}()
	wg.Add(1)
	go func() {
		c.ruleManager.Run(stopCh, 5*time.Minute)
		wg.Done()
	}()

	// Tell rule manager and IPTable manager that we want to fully own all rules at a particular priority/table.
	// Any rules created with this priority or in that particular IPTables chain, that we do not recognize it, will be
	// removed by relevant manager.
	if err := c.ruleManager.OwnPriority(rulePriority); err != nil {
		return fmt.Errorf("failed to own priority %d for IP rules: %v", rulePriority, err)
	}
	if err := c.ruleManager.OwnPriority(ruleEIPTrafficPriority); err != nil {
		return fmt.Errorf("failed to own priority %d for EgressIPTraffic IP rules: %v", ruleEIPTrafficPriority, err)
	}
	if c.v4 {
		if err := c.iptablesManager.OwnChain(utiliptables.TableNAT, iptChainName, utiliptables.ProtocolIPv4); err != nil {
			return fmt.Errorf("unable to own chain %s: %v", iptChainName, err)
		}
		if err = c.iptablesManager.EnsureRule(utiliptables.TableNAT, utiliptables.ChainPostrouting, utiliptables.ProtocolIPv4, iptJumpRule); err != nil {
			return fmt.Errorf("failed to create rule in chain %s to jump to chain %s: %v", utiliptables.ChainPostrouting, iptChainName, err)
		}
		// for LGW mode, we need to restore pkt mark from conntrack in-order for RP filtering not to fail for return packets from cluster nodes
		if ovnconfig.Gateway.Mode == ovnconfig.GatewayModeLocal {
			if err = c.iptablesManager.EnsureRule(utiliptables.TableMangle, utiliptables.ChainPrerouting, utiliptables.ProtocolIPv4, iptRestoreMarkRule); err != nil {
				return fmt.Errorf("failed to create rule in chain %s to restore pkt marking: %v", utiliptables.ChainPrerouting, err)
			}
			if err = c.iptablesManager.EnsureRule(utiliptables.TableMangle, utiliptables.ChainPrerouting, utiliptables.ProtocolIPv4, iptSaveMarkRule); err != nil {
				return fmt.Errorf("failed to create rule in chain %s to save pkt marking: %v", utiliptables.ChainPrerouting, err)
			}

			// If dst is a node IP, use main routing table and skip EIP routing tables
			if err = c.ruleManager.Add(getNodeIPFwMarkIPRule(netlink.FAMILY_V4)); err != nil {
				return fmt.Errorf("failed to create IPv4 rule for node IPs: %v", err)
			}
			// The fwmark of the packet is included in reverse path route lookup. This permits rp_filter to function when the fwmark is
			// used for routing traffic in both directions.
			stdout, _, err := util.RunSysctl("-w", "net.ipv4.conf.all.src_valid_mark=1")
			if err != nil || stdout != "net.ipv4.conf.all.src_valid_mark = 1" {
				return fmt.Errorf("failed to set sysctl net.ipv4.conf.all.src_valid_mark to 1")
			}
		}
	}
	if c.v6 {
		if err := c.iptablesManager.OwnChain(utiliptables.TableNAT, iptChainName, utiliptables.ProtocolIPv6); err != nil {
			return fmt.Errorf("unable to own chain %s: %v", iptChainName, err)
		}
		if err = c.iptablesManager.EnsureRule(utiliptables.TableNAT, utiliptables.ChainPostrouting, utiliptables.ProtocolIPv6, iptJumpRule); err != nil {
			return fmt.Errorf("unable to ensure iptables rules for jump rule: %v", err)
		}
		// for LGW mode, we need to restore pkt mark from conntrack in-order for RP filtering not to fail for return packets from cluster nodes
		if ovnconfig.Gateway.Mode == ovnconfig.GatewayModeLocal {
			if err = c.iptablesManager.EnsureRule(utiliptables.TableMangle, utiliptables.ChainPrerouting, utiliptables.ProtocolIPv6, iptRestoreMarkRule); err != nil {
				return fmt.Errorf("failed to create rule in chain %s to restore pkt marking: %v", utiliptables.ChainPrerouting, err)
			}
			if err = c.iptablesManager.EnsureRule(utiliptables.TableMangle, utiliptables.ChainPrerouting, utiliptables.ProtocolIPv6, iptSaveMarkRule); err != nil {
				return fmt.Errorf("failed to create rule in chain %s to save pkt marking: %v", utiliptables.ChainPrerouting, err)
			}

			// If dst is a node IP, use main routing table and skip EIP routing tables
			// src_valid_mark is not applicable to ipv6
			if err = c.ruleManager.Add(getNodeIPFwMarkIPRule(netlink.FAMILY_V6)); err != nil {
				return fmt.Errorf("failed to create IPv6 rule for node IPs: %v", err)
			}
		}
	}

	err = wait.PollUntilContextTimeout(wait.ContextForChannel(stopCh), 1*time.Second, 10*time.Second, true,
		func(_ context.Context) (done bool, err error) {
			if err := c.migrateFromAddrLabelToAnnotation(); err != nil {
				klog.Errorf("Failed to migrate from managing EgressIP addresses using address labels to a node annotation - Retrying: %v", err)
				return false, err
			}
			return true, nil
		})
	if err != nil {
		return fmt.Errorf("failed to run EgressIP controller because migration from using address labels to a node annotation failed: %v", err)
	}

	err = wait.PollUntilContextTimeout(wait.ContextForChannel(stopCh), 1*time.Second, 10*time.Second, true,
		func(_ context.Context) (done bool, err error) {
			if err := c.repairNode(); err != nil {
				klog.Errorf("Failed to repair node: '%v' - Retrying", err)
				return false, err
			}
			return true, nil
		})
	if err != nil {
		return fmt.Errorf("failed to run EgressIP controller because repairing node failed: %v", err)
	}

	for i := 0; i < threads; i++ {
		for _, workerFn := range []func(*sync.WaitGroup){
			c.runEIPWorker,
			c.runPodWorker,
			c.runNamespaceWorker,
		} {
			wg.Add(1)
			go func(fn func(*sync.WaitGroup)) {
				defer wg.Done()
				wait.Until(func() {
					fn(wg)
				}, time.Second, stopCh)
			}(workerFn)
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		// wait until we're told to stop
		<-stopCh
		c.eIPQueue.ShutDown()
		c.podQueue.ShutDown()
		c.namespaceQueue.ShutDown()
	}()
	return nil
}

func (c *Controller) onEIPAdd(obj interface{}) {
	_, ok := obj.(*eipv1.EgressIP)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &eipv1.EgressIP{}, obj))
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding Egress IP %s", key)
	c.eIPQueue.Add(key)
}

func (c *Controller) onEIPUpdate(oldObj, newObj interface{}) {
	oldEIP, ok := oldObj.(*eipv1.EgressIP)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &eipv1.EgressIP{}, oldObj))
		return
	}
	newEIP, ok := newObj.(*eipv1.EgressIP)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &eipv1.EgressIP{}, newObj))
		return
	}
	if oldEIP == nil || newEIP == nil {
		utilruntime.HandleError(errors.New("invalid Egress IP policy to onEIPUpdate()"))
		return
	}
	if oldEIP.Generation == newEIP.Generation ||
		!newEIP.GetDeletionTimestamp().IsZero() {
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", newObj, err))
	}
	c.eIPQueue.Add(key)
}

func (c *Controller) onEIPDelete(obj interface{}) {
	_, ok := obj.(*eipv1.EgressIP)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tomstone %#v", obj))
			return
		}
		_, ok = tombstone.Obj.(*eipv1.EgressIP)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not an Egress IP object %#v", tombstone.Obj))
			return
		}
	}
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	c.eIPQueue.Add(key)
}

// onEIPTrafficChange re-queues all EgressIPs with a trafficSelector matching the changed EgressIPTraffic.
func (c *Controller) onEIPTrafficChange(obj interface{}) {
	eIPTraffic, ok := obj.(*egressiptrafficv1.EgressIPTraffic)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		eIPTraffic, ok = tombstone.Obj.(*egressiptrafficv1.EgressIPTraffic)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not an EgressIPTraffic %#v", tombstone.Obj))
			return
		}
	}
	egressIPs, err := c.eIPLister.List(labels.Everything())
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("failed to list EgressIPs for EgressIPTraffic change: %v", err))
		return
	}
	for _, eip := range egressIPs {
		if len(eip.Spec.TrafficSelector.MatchLabels) == 0 && len(eip.Spec.TrafficSelector.MatchExpressions) == 0 {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(&eip.Spec.TrafficSelector)
		if err != nil {
			continue
		}
		if sel.Matches(labels.Set(eIPTraffic.Labels)) {
			klog.V(4).Infof("EgressIPTraffic %s change triggers re-processing of EgressIP %s", eIPTraffic.Name, eip.Name)
			c.eIPQueue.Add(eip.Name)
		}
	}
}

func (c *Controller) runEIPWorker(wg *sync.WaitGroup) {
	for c.processNextEIPWorkItem(wg) {
	}
}

func (c *Controller) processNextEIPWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	key, shutdown := c.eIPQueue.Get()
	if shutdown {
		return false
	}
	defer c.eIPQueue.Done(key)
	klog.V(4).Infof("Processing Egress IP %s", key)
	if err := c.syncEIP(key); err != nil {
		if c.eIPQueue.NumRequeues(key) < maxRetries {
			klog.V(4).Infof("Error found while processing Egress IP %s: %v", key, err)
			c.eIPQueue.AddRateLimited(key)
			return true
		}
		klog.Errorf("Dropping Egress IP %q out of the queue: %v", key, err)
		utilruntime.HandleError(err)
	}
	c.eIPQueue.Forget(key)
	return true
}

func (c *Controller) syncEIP(eIPName string) error {
	// 1. Lock on the existing 'state', as we are going to use it for cleanup and update.
	// 2. Build latest 'config'. This includes listing referenced namespaces and pods.
	// To make sure there is no race with pod and namespace handlers, referencedObjects is acquired
	// before listing objects, and released when the 'config' is built. At this point namespace and pod
	// handler can use referencedObjects to see which objects were considered as related by the handler last time.
	// 3. With existing state and newly generated config, we can clean up and apply.
	return c.cache.DoWithLock(eIPName, func(eIPName string) error {
		informerEIP, err := c.eIPLister.Get(eIPName)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get Egress IP before sync: %w", err)
		}
		var update *config
		// get updated policy and update policy refs
		if apierrors.IsNotFound(err) || (informerEIP != nil && !informerEIP.DeletionTimestamp.IsZero()) {
			// EIP deleted
			update = nil
			c.deleteRefObjects(eIPName)
		} else {
			update, err = c.getConfigAndUpdateRefs(informerEIP, true)
			if err != nil {
				return fmt.Errorf("failed to get config and update references for Egress IP %s: %w", eIPName, err)
			}
		}
		existing, found := c.cache.Load(eIPName)
		if !found {
			if update == nil {
				// nothing to do
				return nil
			}
			existing = newState()
			c.cache.Store(eIPName, existing)
		}
		if err = c.updateEIP(eIPName, existing, update); err != nil {
			return fmt.Errorf("failed to update policy from %+v to %+v: %w", existing, update, err)
		}
		if update == nil {
			c.cache.Delete(eIPName)
		}
		return nil
	})
}

// getConfigAndUpdateRefs lists and updates all referenced objects for a given EIP and returns
// config to perform an update.
// This function should be the only one that lists referenced objects, and updates referencedObjects atomically.
func (c *Controller) getConfigAndUpdateRefs(eIP *eipv1.EgressIP, updateRefs bool) (*config, error) {
	c.referencedObjectsLock.Lock()
	defer c.referencedObjectsLock.Unlock()
	eIPConfig, selectedNamespaces, selectedPods, namespacesWithPodIPConfigs, err := c.processEIP(eIP)
	if err != nil {
		return nil, err
	}
	if updateRefs {
		refObjs := &referencedObjects{
			eIPNamespaces: selectedNamespaces,
			eIPPods:       selectedPods,
		}
		c.referencedObjects[eIP.Name] = refObjs
	}
	if eIPConfig == nil || len(namespacesWithPodIPConfigs) == 0 {
		return nil, nil
	}
	return &config{
		namespacesWithPodIPConfigs: namespacesWithPodIPConfigs,
		eIPConfig:                  eIPConfig,
	}, nil

}

// processEIP attempts to find namespaces and pods that match the EIP selectors and then attempts to find a network
// that can host one of the EIP IPs returning egress IP configuration, selected namespaces and pods

// getDestinationNetworksForEIP returns the parsed destination network CIDRs for an EgressIP
// based on its TrafficSelector. The returned bool indicates whether a TrafficSelector is set,
// distinguishing "no selector" (false, nil) from "selector with zero matches" (true, nil).
func (c *Controller) getDestinationNetworksForEIP(eip *eipv1.EgressIP) ([]*net.IPNet, bool, error) {
	if len(eip.Spec.TrafficSelector.MatchLabels) == 0 && len(eip.Spec.TrafficSelector.MatchExpressions) == 0 {
		return nil, false, nil
	}
	if c.eIPTrafficLister == nil {
		// Feature gate is off; treat as "selector set, no matches" to skip
		// node-side programming without returning an error that would cause
		// unwanted retries.
		return nil, true, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(&eip.Spec.TrafficSelector)
	if err != nil {
		return nil, true, fmt.Errorf("invalid TrafficSelector for EgressIP %s: %v", eip.Name, err)
	}
	egressIPTraffics, err := c.eIPTrafficLister.List(selector)
	if err != nil {
		return nil, true, fmt.Errorf("failed to list EgressIPTraffic by selector for EgressIP %s: %v", eip.Name, err)
	}
	var destNetworks []*net.IPNet
	for _, eipt := range egressIPTraffics {
		for _, cidr := range eipt.Spec.DestinationNetworks {
			_, ipNet, err := net.ParseCIDR(string(cidr))
			if err != nil {
				klog.Warningf("Failed to parse destination network CIDR %s from EgressIPTraffic %s: %v", cidr, eipt.Name, err)
				continue
			}
			destNetworks = append(destNetworks, ipNet)
		}
	}
	return destNetworks, true, nil
}

func (c *Controller) processEIP(eip *eipv1.EgressIP) (*eIPConfig, sets.Set[string], sets.Set[ktypes.NamespacedName],
	map[string]map[ktypes.NamespacedName]*podIPConfigList, error) {
	selectedNamespaces := sets.Set[string]{}
	selectedPods := sets.Set[ktypes.NamespacedName]{}
	selectedNamespacesPodIPs := map[string]map[ktypes.NamespacedName]*podIPConfigList{}
	var eipSpecificConfig *eIPConfig
	parsedNodeEIPConfig, err := c.getNodeEgressIPConfig()
	if err != nil {
		return nil, selectedNamespaces, selectedPods, selectedNamespacesPodIPs,
			fmt.Errorf("failed to determine egress IP config for node %s: %w", c.nodeName, err)
	}
	// max of 1 EIP IP is selected. Return when 1 is found.
	for _, status := range eip.Status.Items {
		if isValid := isEIPStatusItemValid(status, c.nodeName); !isValid {
			continue
		}
		ip := net.ParseIP(status.EgressIP)
		if ip == nil {
			return nil, selectedNamespaces, selectedPods, selectedNamespacesPodIPs,
				fmt.Errorf("failed to parse EgressIP %s IP %s", eip.Name, status.EgressIP)
		}
		if util.IsOVNNetwork(parsedNodeEIPConfig, ip) {
			continue
		}
		found, link, err := findLinkOnSameNetworkAsIP(ip, c.v4, c.v6)
		if err != nil {
			return nil, selectedNamespaces, selectedPods, selectedNamespacesPodIPs,
				fmt.Errorf("failed to find a network to host EgressIP %s IP %s: %v", eip.Name, status.EgressIP, err)
		}
		if !found {
			continue
		}
		// namespace selector is mandatory for EIP
		namespaces, err := c.listNamespacesBySelector(&eip.Spec.NamespaceSelector)
		if err != nil {
			return nil, selectedNamespaces, selectedPods, selectedNamespacesPodIPs, fmt.Errorf("failed to list namespaces: %w", err)
		}
		destNetworks, hasTrafficSelector, err := c.getDestinationNetworksForEIP(eip)
		if err != nil {
			return nil, selectedNamespaces, selectedPods, selectedNamespacesPodIPs, fmt.Errorf("failed to get destination networks for EgressIP %s: %v", eip.Name, err)
		}
		// When TrafficSelector is set but matches no destination networks, skip programming
		// entirely — the OVN controller's empty address set will ensure no traffic is rerouted.
		if hasTrafficSelector && len(destNetworks) == 0 {
			klog.V(4).Infof("EgressIP %s has TrafficSelector but no matching destination networks, skipping node-side programming", eip.Name)
			break
		}
		isEIPV6 := utilnet.IsIPv6(ip)
		for _, namespace := range namespaces {
			netInfo, err := c.getActiveNetworkForNamespace(namespace.Name)
			if err != nil {
				return nil, selectedNamespaces, selectedPods, selectedNamespacesPodIPs, fmt.Errorf("failed to get active network for namespace %s: %v", namespace.Name, err)
			}
			if netInfo == nil {
				// no active network
				continue
			}
			if netInfo.IsUserDefinedNetwork() {
				// EIP for secondary host interfaces is not supported for secondary networks
				continue
			}
			selectedNamespaces.Insert(namespace.Name)
			pods, err := c.listPodsByNamespaceAndSelector(namespace.Name, &eip.Spec.PodSelector)
			if err != nil {
				return nil, selectedNamespaces, selectedPods, selectedNamespacesPodIPs, fmt.Errorf("failed to list pods in namespace %s: %w",
					namespace.Name, err)
			}
			for _, pod := range pods {
				// Ignore completed pods, host networked pods, pods not scheduled
				if util.PodWantsHostNetwork(pod) || util.PodCompleted(pod) || !util.PodScheduled(pod) {
					continue
				}
				ips, err := util.DefaultNetworkPodIPs(pod)
				if err != nil {
					return nil, selectedNamespaces, selectedPods, selectedNamespacesPodIPs, fmt.Errorf("failed to get pod ips: %w", err)
				}
				if len(ips) == 0 {
					continue
				}
				podNamespaceName := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
				// generate pod specific configuration
				if selectedNamespacesPodIPs[namespace.Name] == nil {
					selectedNamespacesPodIPs[namespace.Name] = make(map[ktypes.NamespacedName]*podIPConfigList)
				}
				selectedNamespacesPodIPs[namespace.Name][podNamespaceName] = generatePodConfigWithDestNetworks(ips, link, ip, isEIPV6, destNetworks)
				selectedPods.Insert(podNamespaceName)
			}
		}
		// ensure at least one pod is selected before generating config
		if len(selectedNamespacesPodIPs) > 0 {
			eipSpecificConfig, err = generateEIPConfig(link, ip, isEIPV6, destNetworks)
			if err != nil {
				return nil, selectedNamespaces, selectedPods, selectedNamespacesPodIPs,
					fmt.Errorf("failed to generate EIP configuration for EgressIP %s IP %s: %v", eip.Name, status.EgressIP, err)
			}
		}
		break
	}

	return eipSpecificConfig, selectedNamespaces, selectedPods, selectedNamespacesPodIPs, nil
}

// generatePodConfigWithDestNetworks generates pod IP configurations. When destNetworks is non-empty,
// it creates one IP rule per pod IP per destination network CIDR, implementing per-destination EgressIP.
// Non-matching destination traffic falls through to the main routing table since the per-EgressIP
// routing table only contains destination-specific routes (no default route).
// When destNetworks is nil or empty, it creates one IP rule per pod IP (matching all destinations).
func generatePodConfigWithDestNetworks(podIPs []net.IP, link netlink.Link, eIP net.IP, isEIPV6 bool, destNetworks []*net.IPNet) *podIPConfigList {
	newPodIPConfigs := newPodIPConfigList()
	for _, podIP := range podIPs {
		isPodIPv6 := utilnet.IsIPv6(podIP)
		if isPodIPv6 != isEIPV6 {
			continue
		}
		if len(destNetworks) > 0 {
			// Create one IP rule per destination network CIDR.
			// The iptables SNAT rule is identical for all destinations (same source, interface, SNAT IP),
			// so it is only attached to the first config to avoid duplicate add/delete issues.
			snatRuleAssigned := false
			for _, destCIDR := range destNetworks {
				if utilnet.IsIPv6CIDR(destCIDR) != isPodIPv6 {
					continue
				}
				ipConfig := newPodIPConfig()
				if !snatRuleAssigned {
					ipConfig.ipTableRule = generateIPTablesSNATRuleArg(podIP, isPodIPv6, link.Attrs().Name, eIP.String())
					snatRuleAssigned = true
				}
				ipConfig.ipRule = generateIPRuleWithDst(podIP, isPodIPv6, link.Attrs().Index, destCIDR)
				ipConfig.v6 = isPodIPv6
				newPodIPConfigs.elems = append(newPodIPConfigs.elems, ipConfig)
			}
		} else {
			ipConfig := newPodIPConfig()
			ipConfig.ipTableRule = generateIPTablesSNATRuleArg(podIP, isPodIPv6, link.Attrs().Name, eIP.String())
			// Use lower priority so destination-specific rules from trafficSelector EgressIPs
			// are evaluated first when coexisting on the same interface.
			ipConfig.ipRule = generateIPRule(podIP, isPodIPv6, link.Attrs().Index)
			ipConfig.v6 = isPodIPv6
			newPodIPConfigs.elems = append(newPodIPConfigs.elems, ipConfig)
		}
	}
	return newPodIPConfigs
}

// generateEIPConfig generates configuration that isn't related to any pod EIPs to support config of a single EIP.
// When destNetworks is non-empty (TrafficSelector is set), the default route is excluded from the
// routing table and replaced with destination-specific routes. This avoids the need for broad
// MASQUERADE iptables rules that conflict across multiple EgressIPs.
func generateEIPConfig(link netlink.Link, eIP net.IP, isEIPV6 bool, destNetworks []*net.IPNet) (*eIPConfig, error) {
	eipConfig := newEIPConfig()
	linkRoutes, err := generateRoutesForLink(link, isEIPV6)
	if err != nil {
		return nil, err
	}
	if len(destNetworks) > 0 {
		linkRoutes = replaceDefaultRouteWithDestNetworks(linkRoutes, link.Attrs().Index, destNetworks, isEIPV6)
	}
	eipConfig.routes = linkRoutes
	eipConfig.addr = egressip.GetNetlinkAddress(eIP, link.Attrs().Index)
	return eipConfig, nil
}

func generateRoutesForLink(link netlink.Link, isV6 bool) ([]netlink.Route, error) {
	routeTable := 254 // main table number
	// check if device is a slave to a VRF device and if so, use VRF devices associated routing table to lookup routes instead of main table
	if isVRFSlaveDevice(link) {
		vrfLink, err := util.GetNetLinkOps().LinkByIndex(link.Attrs().MasterIndex)
		if err != nil {
			return nil, fmt.Errorf("failed to get VRF link from interface index %d: %w", link.Attrs().MasterIndex, err)
		}
		vrf, ok := vrfLink.(*netlink.Vrf)
		if !ok {
			actualType := reflect.TypeOf(vrfLink)
			return nil, fmt.Errorf("expected link %s to be type VRF, instead received type %s", vrfLink.Attrs().Name, actualType)
		}
		routeTable = int(vrf.Table)
	}
	filterRoute, filterMask := filterRouteByLinkTable(link.Attrs().Index, routeTable)
	linkRoutes, err := util.GetNetLinkOps().RouteListFiltered(util.GetIPFamily(isV6), filterRoute, filterMask)
	if err != nil {
		return nil, fmt.Errorf("failed to get routes for link %s: %v", link.Attrs().Name, err)
	}
	linkRoutes = ensureAtLeastOneDefaultRoute(linkRoutes, link.Attrs().Index, isV6)
	overwriteRoutesTableID(linkRoutes, util.CalculateRouteTableID(link.Attrs().Index))
	clearSrcFromRoutes(linkRoutes)
	return linkRoutes, nil
}

// replaceDefaultRouteWithDestNetworks removes default routes from the given routes and adds
// destination-specific routes for each destination network CIDR via the same link. This is used
// when TrafficSelector is set so that only traffic matching the destination networks is routed
// via the EgressIP interface, and non-matching traffic falls through to the main routing table.
//
// If a route for a destination CIDR already exists in the input (e.g., a specific route from
// the main routing table configured by the network admin), it is preserved as-is rather than
// replaced with a new route. This ensures that gateways configured on specific routes are not
// lost. Only destination CIDRs without existing routes get new routes using the default route's
// gateway (if available).
func replaceDefaultRouteWithDestNetworks(routes []netlink.Route, linkIndex int, destNetworks []*net.IPNet, isV6 bool) []netlink.Route {
	filtered := make([]netlink.Route, 0, len(routes))
	var routeTable int
	var defaultGw net.IP
	isDefaultRoute := func(r netlink.Route) bool {
		if r.Dst == nil {
			return true
		}
		ones, _ := r.Dst.Mask.Size()
		return r.Dst.IP.IsUnspecified() && ones == 0
	}
	for _, r := range routes {
		if isDefaultRoute(r) {
			// default route — skip it but capture its table ID and gateway
			routeTable = r.Table
			defaultGw = r.Gw
			continue
		}
		filtered = append(filtered, r)
	}
	if routeTable == 0 && len(routes) > 0 {
		routeTable = routes[0].Table
	}
	for _, destCIDR := range destNetworks {
		if utilnet.IsIPv6CIDR(destCIDR) != isV6 {
			continue
		}
		// If a route for this CIDR already exists (e.g., a specific route with a gateway
		// from the main table), keep it rather than creating a duplicate without a gateway.
		if hasRouteForDst(filtered, destCIDR) {
			continue
		}
		filtered = append(filtered, netlink.Route{
			Dst:       destCIDR,
			LinkIndex: linkIndex,
			Table:     routeTable,
			Gw:        defaultGw,
		})
	}
	return filtered
}

// hasRouteForDst returns true if any route in routes has the same destination CIDR.
func hasRouteForDst(routes []netlink.Route, dst *net.IPNet) bool {
	for _, r := range routes {
		if r.Dst != nil && r.Dst.IP.Equal(dst.IP) && r.Dst.Mask.String() == dst.Mask.String() {
			return true
		}
	}
	return false
}

func (c *Controller) deleteRefObjects(name string) {
	c.referencedObjectsLock.Lock()
	delete(c.referencedObjects, name)
	c.referencedObjectsLock.Unlock()
}

// updateEIP reconciles existing state towards update config. If update is nil, delete existing state.
func (c *Controller) updateEIP(eIPName string, existing *state, update *config) error {
	// cleanup first
	// cleanup pod specific configuration - aka ip rules and iptables
	if len(existing.namespacesWithPodIPConfigs) > 0 {
		// track which namespaces should be removed from targetNamespaces
		var namespacesToDelete []string
		for targetNamespace, targetPods := range existing.namespacesWithPodIPConfigs {
			// track which pods should be removed from targetPods
			var podsToDelete []ktypes.NamespacedName
			for podNamespacedName, existingPodConfig := range targetPods {
				podIPConfigsToDelete := newPodIPConfigList()
				// each pod IP will have its own configuration that needs to be tracked and possibly removed
				for _, existingPodIPConfig := range existingPodConfig.elems {
					// delete EIP config if:
					// 1. EIP deleted or no EIP found
					// 3. Is not present in update
					// 3. Target pod is not listed in update.targetNamespaces
					// 4. Pod IP config has changed
					if update == nil || update.namespacesWithPodIPConfigs[targetNamespace][podNamespacedName] == nil ||
						// delete if IPs dont match
						(update.namespacesWithPodIPConfigs[targetNamespace][podNamespacedName] != nil &&
							!update.namespacesWithPodIPConfigs[targetNamespace][podNamespacedName].has(existingPodIPConfig)) {
						podIPConfigsToDelete.insert(*existingPodIPConfig)
					}
				}
				if podIPConfigsToDelete.len() > 0 {
					for _, podIPConfigToDelete := range podIPConfigsToDelete.elems {
						if err := c.deleteIPConfig(podIPConfigToDelete); err != nil {
							existingPodConfig.insertOverwriteFailed(*podIPConfigToDelete)
							return err
						}
						existingPodConfig.delete(*podIPConfigToDelete)
					}
				}
				if update == nil || update.namespacesWithPodIPConfigs[targetNamespace][podNamespacedName] == nil {
					podsToDelete = append(podsToDelete, podNamespacedName)
				}
			}
			for _, podToDelete := range podsToDelete {
				delete(targetPods, podToDelete)
			}
			if update == nil || update.namespacesWithPodIPConfigs[targetNamespace] == nil {
				namespacesToDelete = append(namespacesToDelete, targetNamespace)
			}
		}
		for _, nsToDelete := range namespacesToDelete {
			delete(existing.namespacesWithPodIPConfigs, nsToDelete)
		}
	}
	// clean up pod independent configuration first
	// if EIP IP has changed and therefore could be hosted by a different interface, remove old EIP
	// Delete addresses and routes under the following conditions
	// 1. existing contains a non nil IP and update is nil
	// 2. existing contains an ip and update contains an ip and update contains an ip different to existing
	if (update == nil && existing.eIPConfig != nil && existing.eIPConfig.addr != nil) ||
		(update != nil && update.eIPConfig != nil && update.eIPConfig.addr != nil &&
			existing.eIPConfig != nil && existing.eIPConfig.addr != nil && !existing.eIPConfig.addr.Equal(*update.eIPConfig.addr)) {

		if err := c.linkManager.DelAddress(*existing.eIPConfig.addr); err != nil {
			// TODO(mk): if we fail to delete address, handle it
			return fmt.Errorf("failed to delete egress IP address %s: %w", existing.eIPConfig.addr.String(), err)
		}
		if err := c.deleteIPFromAnnotation(existing.eIPConfig.addr.IP.String()); err != nil {
			return fmt.Errorf("failed to delete egress IP address %s from annotation: %v", existing.eIPConfig.addr.String(), err)
		}
	}
	// delete stale routes
	// When deleting routes, only remove routes that are not needed by other EgressIPs on the same link.
	// This handles the case where a trafficSelector EgressIP (per-destination routes) and a non-trafficSelector
	// EgressIP (default route) share the same interface — each EgressIP's routes are cleaned up independently.
	if update == nil && existing.eIPConfig != nil && len(existing.eIPConfig.routes) > 0 && existing.eIPConfig.addr != nil {
		routesNeededByOthers := c.getRoutesForOtherEIPsOnLink(eIPName, existing.eIPConfig.addr.LinkIndex)
		for _, routeToDelete := range existing.eIPConfig.routes {
			if !containsRoute(routesNeededByOthers, routeToDelete) {
				err := c.routeManager.Del(routeToDelete)
				if err != nil {
					return fmt.Errorf("failed to delete egress IP route: %w", err)
				}
			}
		}
	} else if update != nil && update.eIPConfig != nil && len(update.eIPConfig.routes) > 0 &&
		existing.eIPConfig != nil && len(existing.eIPConfig.routes) > 0 {
		// delete delta between existing and update, but preserve routes needed by co-located EgressIPs
		routesToDelete := routeDifference(existing.eIPConfig.routes, update.eIPConfig.routes)
		routesNeededByOthers := c.getRoutesForOtherEIPsOnLink(eIPName, existing.eIPConfig.addr.LinkIndex)
		for _, routeToDelete := range routesToDelete {
			if containsRoute(routesNeededByOthers, routeToDelete) {
				continue
			}
			err := c.routeManager.Del(routeToDelete)
			if err != nil {
				return fmt.Errorf("failed to delete egress IP route: %w", err)
			}
		}
	}
	// apply new changes
	if update != nil && update.eIPConfig != nil && update.eIPConfig.addr != nil && len(update.eIPConfig.routes) > 0 {
		for updatedTargetNS, updatedTargetPod := range update.namespacesWithPodIPConfigs {
			existingNs, found := existing.namespacesWithPodIPConfigs[updatedTargetNS]
			if !found {
				existingNs = map[ktypes.NamespacedName]*podIPConfigList{}
				existing.namespacesWithPodIPConfigs[updatedTargetNS] = existingNs
			}
			for updatedPodNamespacedName, updatedPodIPConfig := range updatedTargetPod {
				existingTargetPodIPConfig, found := existingNs[updatedPodNamespacedName]
				if !found {
					existingTargetPodIPConfig = newPodIPConfigList()
					existingNs[updatedPodNamespacedName] = existingTargetPodIPConfig
				}
				// applyPodConfig will apply pod specific configuration - ip rules and iptables rules
				err := c.applyPodConfig(existingTargetPodIPConfig, updatedPodIPConfig)
				if err != nil {
					return fmt.Errorf("failed to apply pod %s configuration: %v", updatedPodNamespacedName.String(), err)
				}
			}
		}
		if err := c.addIPToAnnotation(update.eIPConfig.addr.IP.String()); err != nil {
			return fmt.Errorf("failed to add egress IP address to annotation: %v", err)
		}
		// TODO(mk): only apply the follow when its new config or when it failed to apply
		// Ok to repeat requests to route manager and link manager
		if err := c.linkManager.AddAddress(*update.eIPConfig.addr); err != nil {
			return fmt.Errorf("failed to add address to link: %v", err)
		}
		existing.eIPConfig.addr = update.eIPConfig.addr
		// route manager manages retry
		for _, routeToAdd := range update.eIPConfig.routes {
			if err := c.routeManager.Add(routeToAdd); err != nil {
				return err
			}
		}
		existing.eIPConfig.routes = update.eIPConfig.routes
	}
	return nil
}

func (c *Controller) deleteIPConfig(podIPConfigToDelete *podIPConfig) error {
	if err := c.ruleManager.Delete(podIPConfigToDelete.ipRule); err != nil {
		return err
	}
	if len(podIPConfigToDelete.ipTableRule.Args) == 0 {
		// No iptables rule for this config (shared SNAT rule handled by another config)
		return nil
	}
	if podIPConfigToDelete.v6 {
		if err := c.iptablesManager.DeleteRule(utiliptables.TableNAT, iptChainName, utiliptables.ProtocolIPv6,
			podIPConfigToDelete.ipTableRule); err != nil {
			return err
		}
	} else {
		if err := c.iptablesManager.DeleteRule(utiliptables.TableNAT, iptChainName, utiliptables.ProtocolIPv4,
			podIPConfigToDelete.ipTableRule); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) applyPodConfig(existingPodIPsConfig *podIPConfigList, updatedPodIPsConfig *podIPConfigList) error {
	if existingPodIPsConfig == nil {
		return fmt.Errorf("unexpected nil existing config")
	}
	if updatedPodIPsConfig == nil {
		return fmt.Errorf("unexpected nil updated config")
	}
	newPodIPConfigs := newPodIPConfigList()
	for _, newConfig := range updatedPodIPsConfig.elems {
		if !existingPodIPsConfig.hasWithoutError(newConfig) {
			newPodIPConfigs.insert(*newConfig)
		}
	}
	for _, newPodIPConfig := range newPodIPConfigs.elems {
		if err := c.ruleManager.Add(newPodIPConfig.ipRule); err != nil {
			existingPodIPsConfig.insertOverwriteFailed(*newPodIPConfig)
			return err
		}
		if len(newPodIPConfig.ipTableRule.Args) == 0 {
			// No iptables rule for this config (shared SNAT rule handled by another config)
			existingPodIPsConfig.insertOverwrite(*newPodIPConfig)
			continue
		}
		if newPodIPConfig.v6 {
			if err := c.iptablesManager.EnsureRule(utiliptables.TableNAT, iptChainName, utiliptables.ProtocolIPv6, newPodIPConfig.ipTableRule); err != nil {
				existingPodIPsConfig.insertOverwriteFailed(*newPodIPConfig)
				return fmt.Errorf("unable to ensure iptables rules: %v", err)
			}
		} else {
			if err := c.iptablesManager.EnsureRule(utiliptables.TableNAT, iptChainName, utiliptables.ProtocolIPv4, newPodIPConfig.ipTableRule); err != nil {
				existingPodIPsConfig.insertOverwriteFailed(*newPodIPConfig)
				return fmt.Errorf("failed to ensure rules (%+v) in chain %s: %v", newPodIPConfig.ipTableRule, iptChainName, err)
			}
		}
		existingPodIPsConfig.insertOverwrite(*newPodIPConfig)
	}
	return nil
}

func (c *Controller) getAllEIPs() ([]*eipv1.EgressIP, error) {
	eips, err := c.eIPLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list EgressIPs: %v", err)
	}
	return eips, nil
}

// addrLink is used to store information for an IP address and its associated link. Only used to implement comparable
// interface because netlink.Addr does not implement comparable
type addrLink struct {
	addr      string // IP + mask
	linkIndex int
}

// repairNode generates whats expected and what is seen on the node and removes any stale configuration. This should be
// called at Controller startup.
func (c *Controller) repairNode() error {
	// get address map for each interface -> addresses/mask
	// also map address/mask -> interface name
	assignedAddr := sets.New[addrLink]()
	assignedAddrStrToAddrs := make(map[string]netlink.Addr)
	assignedIPRoutes := sets.New[string]()
	assignedIPRouteStrToRoutes := make(map[string]netlink.Route)
	assignedIPRules := sets.New[string]()
	assignedIPRulesStrToRules := make(map[string]netlink.Rule)
	assignedIPTableV4Rules := sets.New[string]()
	assignedIPTableV6Rules := sets.New[string]()
	assignedIPTablesV4StrToRules := make(map[string]iptables.RuleArg)
	assignedIPTablesV6StrToRules := make(map[string]iptables.RuleArg)
	existingAddrsFromAnnot, err := c.getAnnotation()
	if err != nil {
		return fmt.Errorf("failed to get annotation: %v", err)
	}
	links, err := util.GetNetLinkOps().LinkList()
	if err != nil {
		return fmt.Errorf("failed to list links: %v", err)
	}
	for _, link := range links {
		link := link
		linkName := link.Attrs().Name
		// skip OVN managed interfaces that don't serve as secondary interfaces
		if strings.HasPrefix(linkName, types.K8sMgmtIntfNamePrefix) {
			continue
		}
		linkIdx := link.Attrs().Index
		addresses, err := util.GetFilteredInterfaceAddrs(link, c.v4, c.v6)
		if err != nil {
			return fmt.Errorf("unable to get link addresses for link %s: %v", linkName, err)
		}
		for _, address := range addresses {
			if existingAddrsFromAnnot.Has(address.IP.String()) {
				addressStr := address.IPNet.String()
				assignedAddr.Insert(addrLink{address.IPNet.String(), address.LinkIndex})
				assignedAddrStrToAddrs[addressStr] = address
			}
		}
		filter, mask := filterRouteByLinkTable(linkIdx, util.CalculateRouteTableID(linkIdx))
		existingRoutes, err := util.GetNetLinkOps().RouteListFiltered(netlink.FAMILY_ALL, filter, mask)
		if err != nil {
			return fmt.Errorf("unable to get route list using filter (%s): %v", filter.String(), err)
		}
		for _, existingRoute := range existingRoutes {
			routeStr := existingRoute.String()
			assignedIPRoutes.Insert(routeStr)
			assignedIPRouteStrToRoutes[routeStr] = existingRoute
		}
	}
	for _, priority := range []int{rulePriority, ruleEIPTrafficPriority} {
		filter, mask := filterRuleByPriority(priority)
		existingRules, err := util.GetNetLinkOps().RuleListFiltered(netlink.FAMILY_ALL, filter, mask)
		if err != nil {
			return fmt.Errorf("failed to list IP rules at priority %d: %v", priority, err)
		}
		for _, existingRule := range existingRules {
			ruleStr := existingRule.String()
			assignedIPRules.Insert(ruleStr)
			assignedIPRulesStrToRules[ruleStr] = existingRule
		}
	}
	// gather IPv4 and IPv6 IPTable rules and ignore what IP family we currently support because we may have converted from
	// dual to single or vice versa
	ipTableV4Rules, err := c.iptablesManager.GetIPv4ChainRuleArgs(utiliptables.TableNAT, chainName)
	if err != nil {
		return fmt.Errorf("failed to list IPTable IPv4 rules: %v", err)
	}
	for _, rule := range ipTableV4Rules {
		ruleStr := strings.Join(rule.Args, " ")
		assignedIPTableV4Rules.Insert(ruleStr)
		assignedIPTablesV4StrToRules[ruleStr] = rule
	}
	ipTableV6Rules, err := c.iptablesManager.GetIPv6ChainRuleArgs(utiliptables.TableNAT, chainName)
	if err != nil {
		// IPv6 NAT table may not be available by default on some distributions.
		ipTableV6Rules = make([]iptables.RuleArg, 0)
		klog.Warningf("Failed to list IPTable IPv6 rules: %v", err)
	}
	for _, rule := range ipTableV6Rules {
		ruleStr := strings.Join(rule.Args, " ")
		assignedIPTableV6Rules.Insert(ruleStr)
		assignedIPTablesV6StrToRules[ruleStr] = rule
	}

	expectedAddrs := sets.New[addrLink]()
	expectedIPRoutes := sets.New[string]()
	expectedIPRules := sets.New[string]()
	expectedIPTableV4Rules := sets.New[string]()
	expectedIPTableV6Rules := sets.New[string]()
	egressIPs, err := c.getAllEIPs()
	if err != nil {
		return err
	}
	parsedNodeEIPConfig, err := c.getNodeEgressIPConfig()
	if err != nil {
		return fmt.Errorf("failed to get node egress IP config: %v", err)
	}
	for _, egressIP := range egressIPs {
		if len(egressIP.Status.Items) == 0 {
			continue
		}
		for _, status := range egressIP.Status.Items {
			if isValid := isEIPStatusItemValid(status, c.nodeName); !isValid {
				continue
			}
			eIPNet, err := util.GetIPNetFullMask(status.EgressIP)
			if err != nil {
				return err
			}
			if util.IsOVNNetwork(parsedNodeEIPConfig, eIPNet.IP) {
				continue
			}
			isEIPV6 := utilnet.IsIPv6(eIPNet.IP)
			found, link, err := findLinkOnSameNetworkAsIP(eIPNet.IP, c.v4, c.v6)
			if err != nil {
				return fmt.Errorf("failed to find a network to host EgressIP %s IP %s: %v", egressIP.Name,
					eIPNet.IP.String(), err)
			}
			if !found {
				continue
			}
			linkIdx := link.Attrs().Index
			linkName := link.Attrs().Name
			destNetworks, hasTrafficSelector, err := c.getDestinationNetworksForEIP(egressIP)
			if err != nil {
				return fmt.Errorf("failed to get destination networks for EgressIP %s: %v", egressIP.Name, err)
			}
			if hasTrafficSelector && len(destNetworks) == 0 {
				continue
			}
			// copy routes associated with link to new route table
			linkRoutes, err := generateRoutesForLink(link, isEIPV6)
			if err != nil {
				return fmt.Errorf("failed to generate IP routes for link %s for EgressIP %s IP %s: %v", linkName,
					egressIP.Name, eIPNet.IP.String(), err)
			}
			if len(destNetworks) > 0 {
				linkRoutes = replaceDefaultRouteWithDestNetworks(linkRoutes, linkIdx, destNetworks, isEIPV6)
			}
			for _, route := range linkRoutes {
				expectedIPRoutes.Insert(route.String())
			}
			expectedAddrs.Insert(addrLink{eIPNet.String(), linkIdx})
			namespaceSelector, err := metav1.LabelSelectorAsSelector(&egressIP.Spec.NamespaceSelector)
			if err != nil {
				return fmt.Errorf("invalid namespaceSelector for egress IP %s: %v", egressIP.Name, err)
			}
			podSelector, err := metav1.LabelSelectorAsSelector(&egressIP.Spec.PodSelector)
			if err != nil {
				return fmt.Errorf("invalid podSelector for egress IP %s: %v", egressIP.Name, err)
			}
			namespaces, err := c.namespaceLister.List(namespaceSelector)
			if err != nil {
				return fmt.Errorf("failed to list namespaces using selector %s to configure egress IP %s: %v",
					namespaceSelector.String(), egressIP.Name, err)
			}
			for _, namespace := range namespaces {
				namespaceLabels := labels.Set(namespace.Labels)
				if namespaceSelector.Matches(namespaceLabels) {
					netInfo, err := c.getActiveNetworkForNamespace(namespace.Name)
					if err != nil {
						return fmt.Errorf("failed to get active network for namespace %s: %v", namespace.Name, err)
					}
					if netInfo == nil {
						// no active network
						continue
					}
					if netInfo.IsUserDefinedNetwork() {
						// EIP for secondary host interfaces is not supported for secondary networks
						continue
					}
					pods, err := c.podLister.Pods(namespace.Name).List(podSelector)
					if err != nil {
						return fmt.Errorf("failed to list pods using selector %s to configure egress IP %s: %v",
							podSelector.String(), egressIP.Name, err)
					}
					for _, pod := range pods {
						if util.PodCompleted(pod) || util.PodWantsHostNetwork(pod) || len(pod.Status.PodIPs) == 0 {
							continue
						}
						podIPs, err := util.DefaultNetworkPodIPs(pod)
						if err != nil {
							return err
						}
						for _, podIP := range podIPs {
							isPodIPV6 := utilnet.IsIPv6(podIP)
							if isPodIPV6 != isEIPV6 {
								continue
							}
							if !c.isIPSupported(isPodIPV6) {
								continue
							}
							ipTableRule := strings.Join(generateIPTablesSNATRuleArg(podIP, isPodIPV6, linkName, status.EgressIP).Args, " ")
							if isPodIPV6 {
								expectedIPTableV6Rules.Insert(ipTableRule)
							} else {
								expectedIPTableV4Rules.Insert(ipTableRule)
							}
							if len(destNetworks) > 0 {
								for _, destCIDR := range destNetworks {
									if utilnet.IsIPv6CIDR(destCIDR) != isPodIPV6 {
										continue
									}
									expectedIPRules.Insert(generateIPRuleWithDst(podIP, isPodIPV6, link.Attrs().Index, destCIDR).String())
								}
							} else {
								expectedIPRules.Insert(generateIPRule(podIP, isPodIPV6, link.Attrs().Index).String())
							}
						}
					}
				}
			}
		}
	}
	staleAddresses := assignedAddr.Difference(expectedAddrs)
	if err := c.removeStaleAddresses(staleAddresses, assignedAddrStrToAddrs); err != nil {
		return fmt.Errorf("failed to remove stale Egress IP addresse(s) (%+v): %v", staleAddresses, err)
	}

	staleIPRoutes := assignedIPRoutes.Difference(expectedIPRoutes)
	if err := c.removeStaleIPRoutes(staleIPRoutes, assignedIPRouteStrToRoutes); err != nil {
		return fmt.Errorf("failed to remove stale IP route(s) (%+v): %v", staleIPRoutes, err)
	}

	staleIPRules := assignedIPRules.Difference(expectedIPRules)
	if err := c.removeStaleIPRules(staleIPRules, assignedIPRulesStrToRules); err != nil {
		return fmt.Errorf("failed to remove stale IP rule(s) (%+v): %v", staleIPRules, err)
	}
	staleIPTableV4Rules := assignedIPTableV4Rules.Difference(expectedIPTableV4Rules)
	if err := c.removeStaleIPTableV4Rules(staleIPTableV4Rules, assignedIPTablesV4StrToRules); err != nil {
		return fmt.Errorf("failed to remove stale IPTable V4 rule(s) (%+v): %v", staleIPTableV4Rules, err)
	}
	staleIPTableV6Rules := assignedIPTableV6Rules.Difference(expectedIPTableV6Rules)
	if err := c.removeStaleIPTableV6Rules(staleIPTableV6Rules, assignedIPTablesV6StrToRules); err != nil {
		// IPv6 NAT table may not be available by default on some distributions.
		klog.Warningf("Failed to remove stale IPTable V6 rule(s) (%+v): %v", staleIPTableV6Rules, err)
	}
	return nil
}

// migrateFromAddrLabelToAnnotation gathers currently assigned EIP addresses and adds this info to a node annotation which
// will track assignment instead of using labels. The reason labels aren't sufficient to track assigned EIPs is because
// labels are only supported for IPv4 in linux. This method should only be used once at startup and should be retried if failure.
// This func should be executed before repairing a node and handlers workers are started because they depend on annot being set.
func (c *Controller) migrateFromAddrLabelToAnnotation() error {
	node, err := c.nodeLister.Get(c.nodeName)
	if err != nil {
		return err
	}
	if util.IsNodeSecondaryHostEgressIPsAnnotationSet(node) {
		// if annotation is set, exit early as migration from labels must have has been completed previously
		return nil
	}
	links, err := util.GetNetLinkOps().LinkList()
	if err != nil {
		return fmt.Errorf("failed to ensure IP is correctly configured becase we could not list links: %v", err)
	}
	assignedAddresses := make([]string, 0)
	for _, link := range links {
		linkName := link.Attrs().Name
		addresses, err := util.GetFilteredInterfaceAddrs(link, c.v4, false) // Only IPv4 addresses have labels
		if err != nil {
			return fmt.Errorf("unable to get link addresses for link %s: %v", linkName, err)
		}
		for _, address := range addresses {
			if address.Label == linkmanager.DeprecatedGetAssignedAddressLabel(linkName) {
				assignedAddresses = append(assignedAddresses, address.IP.String())
			}
		}
	}
	if len(assignedAddresses) == 0 {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err = c.nodeLister.Get(c.nodeName)
		if err != nil {
			return err
		}
		patch, err := json.Marshal(assignedAddresses)
		if err != nil {
			return err
		}
		nodeToUpdate := node.DeepCopy()
		if nodeToUpdate.Annotations == nil {
			nodeToUpdate.Annotations = map[string]string{}
		}
		nodeToUpdate.Annotations[util.OVNNodeSecondaryHostEgressIPs] = string(patch)
		return c.kube.UpdateNodeStatus(nodeToUpdate)
	})
}

// addIPToAnnotation adds an address to the collection of existing addresses stored in the nodes annotation. Caller
// may repeat addition of addresses without care for duplicate addresses being added.
func (c *Controller) addIPToAnnotation(ip string) error {
	if !isValidIP(ip) {
		return fmt.Errorf("invalid IP %q", ip)
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := c.nodeLister.Get(c.nodeName)
		if err != nil {
			return err
		}
		existingIPs, err := util.ParseNodeSecondaryHostEgressIPsAnnotation(node)
		if err != nil {
			if util.IsAnnotationNotSetError(err) {
				existingIPs = sets.New[string]()
			} else {
				return fmt.Errorf("failed to parse annotation key %q from node object: %v", util.OVNNodeSecondaryHostEgressIPs, err)
			}
		}
		if existingIPs.Has(ip) {
			return nil
		}
		existingIPs.Insert(ip)
		patch, err := json.Marshal(existingIPs.UnsortedList())
		if err != nil {
			return err
		}
		nodeToUpdate := node.DeepCopy()
		if nodeToUpdate.Annotations == nil {
			nodeToUpdate.Annotations = map[string]string{}
		}
		nodeToUpdate.Annotations[util.OVNNodeSecondaryHostEgressIPs] = string(patch)
		return c.kube.UpdateNodeStatus(nodeToUpdate)
	})
}

// deleteIPFromAnnotation deletes address from annotation. If multiple users, callers must synchronise.
// deletion of address that doesn't exist will not cause an error.
func (c *Controller) deleteIPFromAnnotation(ip string) error {
	if !isValidIP(ip) {
		return fmt.Errorf("invalid IP %q", ip)
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := c.nodeLister.Get(c.nodeName)
		if err != nil {
			return err
		}
		existingIPs, err := util.ParseNodeSecondaryHostEgressIPsAnnotation(node)
		if err != nil {
			if util.IsAnnotationNotSetError(err) {
				existingIPs = sets.New[string]()
			} else {
				return fmt.Errorf("failed to parse annotation key %q from node object: %v", util.OVNNodeSecondaryHostEgressIPs, err)
			}
		}
		if !existingIPs.Has(ip) {
			return nil
		}
		existingIPs.Delete(ip)
		patch, err := json.Marshal(existingIPs.UnsortedList())
		if err != nil {
			return err
		}
		nodeToUpdate := node.DeepCopy()
		if nodeToUpdate.Annotations == nil {
			nodeToUpdate.Annotations = map[string]string{}
		}
		nodeToUpdate.Annotations[util.OVNNodeSecondaryHostEgressIPs] = string(patch)
		return c.kube.UpdateNodeStatus(nodeToUpdate)
	})
}

// getAnnotation retrieves the egress IP annotation from the current node Nodes object. If multiple users, callers must synchronise.
// if annotation isn't present, empty set is returned
func (c *Controller) getAnnotation() (sets.Set[string], error) {
	node, err := c.nodeLister.Get(c.nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s from lister: %v", c.nodeName, err)
	}
	ips, err := util.ParseNodeSecondaryHostEgressIPsAnnotation(node)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			ips = sets.New[string]()
		} else {
			return nil, fmt.Errorf("failed to parse annotation key %q from node object: %v", util.OVNNodeSecondaryHostEgressIPs, err)
		}
	}
	return ips, nil
}

func (c *Controller) getNodeEgressIPConfig() (*util.ParsedNodeEgressIPConfiguration, error) {
	node, err := c.nodeLister.Get(c.nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s from lister: %v", c.nodeName, err)
	}
	return util.GetNodeEIPConfig(node)
}

func isEIPStatusItemValid(status eipv1.EgressIPStatusItem, nodeName string) bool {
	if status.Node != nodeName {
		return false
	}
	if status.EgressIP == "" {
		return false
	}
	return true
}

func (c *Controller) removeStaleAddresses(staleAddresses sets.Set[addrLink], addrStrToNetlinkAddr map[string]netlink.Addr) error {
	for _, address := range staleAddresses.UnsortedList() {
		nlAddr, ok := addrStrToNetlinkAddr[address.addr]
		if !ok {
			return fmt.Errorf("expected to find address %q in map: %+v", address, addrStrToNetlinkAddr)
		}
		if err := c.linkManager.DelAddress(nlAddr); err != nil {
			return fmt.Errorf("failed to delete address from link: %v", err)
		}
		if err := c.deleteIPFromAnnotation(nlAddr.IP.String()); err != nil {
			return fmt.Errorf("failed to delete address from annotation: %v", err)
		}
	}
	return nil
}

func (c *Controller) removeStaleIPRoutes(staleIPRoutes sets.Set[string], routeStrToNetlinkRoute map[string]netlink.Route) error {
	for _, ipRoute := range staleIPRoutes.UnsortedList() {
		route, ok := routeStrToNetlinkRoute[ipRoute]
		if !ok {
			return fmt.Errorf("expected to find route %q in map: %+v", ipRoute, routeStrToNetlinkRoute)
		}
		if err := c.routeManager.Del(route); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) removeStaleIPRules(staleIPRules sets.Set[string], ruleStrToNetlinkRule map[string]netlink.Rule) error {
	for _, ipRule := range staleIPRules.UnsortedList() {
		rule, ok := ruleStrToNetlinkRule[ipRule]
		if !ok {
			return fmt.Errorf("expected to find route %q in map: %+v", ipRule, ruleStrToNetlinkRule)
		}
		if err := c.ruleManager.Delete(rule); err != nil {
			return fmt.Errorf("failed to delete IP rule (%s): %v", rule.String(), err)
		}
	}
	return nil
}

func (c *Controller) removeStaleIPTableV4Rules(staleRules sets.Set[string], ruleStrToRule map[string]iptables.RuleArg) error {
	return c.removeStaleIPTableRules(utiliptables.ProtocolIPv4, staleRules, ruleStrToRule)
}

func (c *Controller) removeStaleIPTableV6Rules(staleRules sets.Set[string], ruleStrToRule map[string]iptables.RuleArg) error {
	return c.removeStaleIPTableRules(utiliptables.ProtocolIPv6, staleRules, ruleStrToRule)
}

func (c *Controller) removeStaleIPTableRules(proto utiliptables.Protocol, staleRules sets.Set[string], ruleStrToRule map[string]iptables.RuleArg) error {
	for _, rule := range staleRules.UnsortedList() {
		ruleArg, ok := ruleStrToRule[rule]
		if !ok {
			return fmt.Errorf("expected to find route %q in map: %+v", rule, ruleStrToRule)
		}
		if err := c.iptablesManager.DeleteRule(utiliptables.TableNAT, iptChainName, proto, ruleArg); err != nil {
			return fmt.Errorf("failed to delete IP rule (%s): %v", rule, err)
		}
	}
	return nil
}

func (c *Controller) isIPSupported(isIPV6 bool) bool {
	if !isIPV6 && c.v4 {
		return true
	}
	if isIPV6 && c.v6 {
		return true
	}
	return false
}

// routeDifference returns a slice of routes from routesA that are not in routesB.
// Assumes non-duplicate routes in each slice.
// For example:
// routesA = {a1, a2, a3}
// routesB = {a1, a2, a4, a5}
// routesDifference(routesA, routesB) = {a3}
// routesDifference(routesA, routesB) = {a4, a5}
// getRoutesForOtherEIPsOnLink collects routes from all other EgressIPs that share the same link.
// This is used during deletion to determine which routes must be preserved for co-located EgressIPs.
func (c *Controller) getRoutesForOtherEIPsOnLink(eIPName string, linkIndex int) []netlink.Route {
	var routes []netlink.Route
	for _, key := range c.cache.GetKeys() {
		if key == eIPName {
			continue
		}
		otherState, found := c.cache.Load(key)
		if !found || otherState.eIPConfig == nil || otherState.eIPConfig.addr == nil {
			continue
		}
		if otherState.eIPConfig.addr.LinkIndex != linkIndex {
			continue
		}
		routes = append(routes, otherState.eIPConfig.routes...)
	}
	return routes
}

// containsRoute checks if a route is present in the given route slice.
func containsRoute(routes []netlink.Route, candidate netlink.Route) bool {
	for _, r := range routes {
		if util.RouteEqual(&r, &candidate) {
			return true
		}
	}
	return false
}

func routeDifference(routesA, routesB []netlink.Route) []netlink.Route {
	diff := make([]netlink.Route, 0)
	var found bool
	for _, routeA := range routesA {
		found = false
		for _, routeB := range routesB {
			if util.RouteEqual(&routeA, &routeB) {
				found = true
				break
			}
		}
		if !found {
			diff = append(diff, routeA)
		}
	}
	return diff
}

func ensureAtLeastOneDefaultRoute(routes []netlink.Route, linkIndex int, isV6 bool) []netlink.Route {
	var defaultCIDR *net.IPNet
	if isV6 {
		defaultCIDR = defaultV6AnyCIDR
	} else {
		defaultCIDR = defaultV4AnyCIDR
	}
	var defaultRouteFound bool
	for _, route := range routes {
		if route.Dst != nil {
			if route.Dst.IP.Equal(defaultCIDR.IP) {
				ones, _ := route.Dst.Mask.Size()
				if ones == 0 {
					defaultRouteFound = true
					break
				}
			}
		}
	}
	if !defaultRouteFound {
		routes = append(routes, netlink.Route{LinkIndex: linkIndex, Dst: defaultCIDR})
	}
	return routes
}

func overwriteRoutesTableID(routes []netlink.Route, tableID int) {
	for i := range routes {
		routes[i].Table = tableID
	}
}

func clearSrcFromRoutes(routes []netlink.Route) {
	for i := range routes {
		routes[i].Src = nil
	}
}

func findLinkOnSameNetworkAsIP(ip net.IP, v4, v6 bool) (bool, netlink.Link, error) {
	found, link, err := findLinkOnSameNetworkAsIPUsingLPM(ip, v4, v6)
	if err != nil {
		return false, nil, fmt.Errorf("failed to find network to host IP %s: %v", ip.String(), err)
	}
	return found, link, nil

}

// findLinkOnSameNetworkAsIPUsingLPM iterates through all links found locally building a map of addresses associated with
// each link and attempts to find a network that will host the func parameter IP address using longest-prefix-match.
func findLinkOnSameNetworkAsIPUsingLPM(ip net.IP, v4, v6 bool) (bool, netlink.Link, error) {
	prefixLinks := map[string]netlink.Link{} // key is network CIDR
	prefixes := make([]netip.Prefix, 0)
	links, err := util.GetNetLinkOps().LinkList()
	if err != nil {
		return false, nil, fmt.Errorf("failed to list links: %v", err)
	}
	for _, link := range links {
		link := link
		linkPrefixes, err := getFilteredPrefixes(link, v4, v6)
		if err != nil {
			klog.Errorf("Failed to get address from link %s: %v", link.Attrs().Name, err)
			continue
		}
		prefixes = append(prefixes, linkPrefixes...)
		// create lookup table for later retrieval
		for _, prefixFound := range linkPrefixes {
			_, ipNet, err := net.ParseCIDR(prefixFound.String())
			if err != nil {
				klog.Errorf("Egress IP: skipping prefix %q due to parsing CIDR error: %v", prefixFound.String(), err)
				continue
			}
			prefixLinks[ipNet.String()] = link
		}
	}
	lpmTree := cidrtree.New(prefixes...)
	addr, err := netip.ParseAddr(ip.String())
	if err != nil {
		return false, nil, fmt.Errorf("failed to convert IP %s to netip addr: %v", ip.String(), err)
	}
	network, found := lpmTree.Lookup(addr)
	if !found {
		return false, nil, nil
	}
	link, ok := prefixLinks[network.String()]
	if !ok {
		return false, nil, nil
	}
	return true, link, nil
}

// getFilteredPrefixes returns address Prefixes from interfaces with the following characteristics:
// Link must be up
// Exclude keepalived assigned addresses
// Exclude addresses assigned by metal LB
// Exclude OVN reserved addresses
// Exclude networks with just one IP i.e  masks /32 for IPv4 or /128 for IPv6
// Exclude Link local addresses
func getFilteredPrefixes(link netlink.Link, v4, v6 bool) ([]netip.Prefix, error) {
	validAddresses := make([]netip.Prefix, 0)
	flags := link.Attrs().Flags.String()
	if !isLinkUp(flags) {
		return validAddresses, nil
	}
	linkAddresses, err := util.GetFilteredInterfaceAddrs(link, v4, v6)
	if err != nil {
		return nil, fmt.Errorf("failed to get link %s addresses: %v", link.Attrs().Name, err)
	}
	for _, address := range linkAddresses {
		if isOneIPNetwork(address.IPNet) {
			continue
		}
		addr, err := netip.ParsePrefix(address.IPNet.String())
		if err != nil {
			return nil, fmt.Errorf("unable to parse address %s on link %s: %v", address.String(), link.Attrs().Name, err)
		}
		validAddresses = append(validAddresses, addr)
	}
	return validAddresses, nil
}

// isOneIPNetwork returns true if only one address exists in the network attached to the IPs mask - itself.
func isOneIPNetwork(ipnet *net.IPNet) bool {
	ones, bits := ipnet.Mask.Size()
	// IPv4
	if ones == 32 && bits == 32 {
		return true
	}
	// IPv6
	if ones == 128 && bits == 128 {
		return true
	}
	return false
}

func isLinkUp(flags string) bool {
	// exclude interfaces that aren't up
	return strings.Contains(flags, "up")
}

// generateIPRule generates IP rules at a predefined priority for each pod IP with a custom routing table based
// from the links 'ifindex'.
func generateIPRule(srcIP net.IP, isIPv6 bool, ifIndex int) netlink.Rule {
	return generateIPRuleWithPriorityAndDst(srcIP, isIPv6, ifIndex, rulePriority, nil)
}

// generateIPRuleWithDst generates IP rules at trafficSelector priority for each pod IP with a custom routing table based
// from the links 'ifindex'. The dstCIDR sets the destination match for per-destination EgressIP routing.
func generateIPRuleWithDst(srcIP net.IP, isIPv6 bool, ifIndex int, dstCIDR *net.IPNet) netlink.Rule {
	return generateIPRuleWithPriorityAndDst(srcIP, isIPv6, ifIndex, ruleEIPTrafficPriority, dstCIDR)
}

func generateIPRuleWithPriorityAndDst(srcIP net.IP, isIPv6 bool, ifIndex, priority int, dstCIDR *net.IPNet) netlink.Rule {
	r := *netlink.NewRule()
	r.Table = util.CalculateRouteTableID(ifIndex)
	r.Priority = priority
	var ipFullMask string
	if isIPv6 {
		ipFullMask = fmt.Sprintf("%s/128", srcIP.String())
		r.Family = netlink.FAMILY_V6
	} else {
		ipFullMask = fmt.Sprintf("%s/32", srcIP.String())
		r.Family = netlink.FAMILY_V4
	}
	_, ipNet, _ := net.ParseCIDR(ipFullMask)
	r.Src = ipNet
	if dstCIDR != nil {
		r.Dst = dstCIDR
	}
	return r
}

func filterRouteByLinkTable(linkIndex, tableID int) (*netlink.Route, uint64) {
	return &netlink.Route{
			LinkIndex: linkIndex,
			Table:     tableID,
		},
		netlink.RT_FILTER_OIF | netlink.RT_FILTER_TABLE
}

func filterRuleByPriority(priority int) (*netlink.Rule, uint64) {
	return &netlink.Rule{
			Priority: priority,
		},
		netlink.RT_FILTER_PRIORITY
}

func getPodNamespacedName(pod *corev1.Pod) ktypes.NamespacedName {
	return ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
}

func generateIPTablesSNATRuleArg(srcIP net.IP, isIPv6 bool, infName, snatIP string) iptables.RuleArg {
	var srcIPFullMask string
	if isIPv6 {
		srcIPFullMask = fmt.Sprintf("%s/128", srcIP.String())
	} else {
		srcIPFullMask = fmt.Sprintf("%s/32", srcIP.String())
	}
	return iptables.RuleArg{Args: []string{"-s", srcIPFullMask, "-o", infName, "-j", "SNAT", "--to-source", snatIP}}
}

func isEgressIPOnLink(linkIndex, ipFamily int, assignedEIPs sets.Set[string]) (bool, error) {
	link, err := netlink.LinkByIndex(linkIndex)
	if err != nil {
		// If the link doesn't exist, there can't be an EgressIP on it.
		// This can happen when a route is added/deleted causing the interface
		// to momentarily disappear or change its index.
		if util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return false, nil
		}
		return false, err
	}
	addresses, err := util.GetNetLinkOps().AddrList(link, ipFamily)
	if err != nil {
		return false, err
	}
	for _, address := range addresses {
		if assignedEIPs.Has(address.IP.String()) {
			return true, nil
		}
	}
	return false, nil
}

func isValidIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	return len(ip) > 0
}

func getNodeIPFwMarkIPRule(ipFamily int) netlink.Rule {
	r := netlink.NewRule()
	r.Priority = ruleFwMarkPriority
	r.Mark = 1008 // pkt marked with 1008 is a node IP
	r.Table = 254 // main
	r.Family = ipFamily
	return *r
}

func isVRFSlaveDevice(link netlink.Link) bool {
	return link.Attrs().Slave != nil && link.Attrs().Slave.SlaveType() == "vrf"
}
