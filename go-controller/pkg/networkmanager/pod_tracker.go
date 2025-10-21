package networkmanager

import (
	"fmt"
	"sync"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type nodeNAD struct {
	node string
	nads []string
}

type refChange struct {
	node   string
	nad    string
	active bool
}

type PodTrackerController struct {
	sync.Mutex
	name string
	// cache holds a mapping of node -> NAD namespaced name -> pod namespaced name
	cache map[string]map[string]map[string]struct{}
	// reverse index: pod key -> (node, NAD)
	reverse map[string]nodeNAD
	// callback when a node+NAD goes active/inactive
	onNetworkRefChange func(node, nad string, active bool)
	podController      controller.Controller
	podLister          v1.PodLister
	nadLister          nadlisters.NetworkAttachmentDefinitionLister
	namespaceLister    v1.NamespaceLister
}

func NewPodTrackerController(name string, wf watchFactory, onNetworkRefChange func(node, nad string, active bool)) *PodTrackerController {
	p := &PodTrackerController{
		name:               name,
		cache:              make(map[string]map[string]map[string]struct{}),
		reverse:            make(map[string]nodeNAD),
		onNetworkRefChange: onNetworkRefChange,
		podLister:          wf.PodCoreInformer().Lister(),
		nadLister:          wf.NADInformer().Lister(),
		namespaceLister:    wf.NamespaceInformer().Lister(),
	}

	cfg := &controller.ControllerConfig[corev1.Pod]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      p.reconcile,
		ObjNeedsUpdate: p.needUpdate,
		MaxAttempts:    controller.InfiniteAttempts,
		Threadiness:    1,
		Informer:       wf.PodCoreInformer().Informer(),
		Lister:         wf.PodCoreInformer().Lister().List,
	}
	p.podController = controller.NewController[corev1.Pod](p.name, cfg)
	return p
}

func (c *PodTrackerController) Start() error {
	klog.Infof("Starting %s controller", c.name)
	return controller.StartWithInitialSync(c.syncAll, c.podController)
}

func (c *PodTrackerController) Stop() {
	klog.Infof("Stopping %s controller", c.name)
	controller.Stop(c.podController)
}

func (c *PodTrackerController) NodeHasNAD(node, nad string) bool {
	c.Lock()
	defer c.Unlock()
	if _, ok := c.cache[node]; !ok {
		return false
	}
	if _, ok := c.cache[node][nad]; !ok {
		return false
	}
	return len(c.cache[node][nad]) > 0
}

// getNADsForPod resolves the primary and secondary networks for a pod.
func (c *PodTrackerController) getNADsForPod(pod *corev1.Pod) ([]string, error) {
	var nadList []string

	requiresUDN := false
	// check if required UDN label is on namespace
	ns, err := c.namespaceLister.Get(pod.Namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to get namespace %q: %w", pod.Namespace, err)
	}
	if _, exists := ns.Labels[types.RequiredUDNNamespaceLabel]; exists {
		requiresUDN = true
	}

	// Primary NAD from namespace
	primaryNAD := ""
	nads, err := c.nadLister.NetworkAttachmentDefinitions(pod.Namespace).List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list network attachment definitions: %w", err)
	}
	for _, nad := range nads {
		if nad.Name == types.DefaultNetworkName {
			continue
		}
		nadInfo, err := util.ParseNADInfo(nad)
		if err != nil {
			klog.Warningf("Failed to parse network attachment definition %q: %v", nad.Name, err)
			continue
		}
		if nadInfo.IsPrimaryNetwork() {
			primaryNAD = util.GetNADName(nad.Namespace, nad.Name)
		}
	}
	if len(primaryNAD) > 0 {
		nadList = append(nadList, primaryNAD)
	} else if requiresUDN {
		return nil, util.NewInvalidPrimaryNetworkError(pod.Namespace)
	}

	// Secondary NADs from pod annotation
	networks, err := util.GetK8sPodAllNetworkSelections(pod)
	if err != nil {
		return nil, fmt.Errorf("failed to parse network annotations for pod %s/%s: %v", pod.Namespace, pod.Name, err)
	}
	for _, net := range networks {
		ns := net.Namespace
		if ns == "" {
			ns = pod.Namespace
		}
		nadList = append(nadList, fmt.Sprintf("%s/%s", ns, net.Name))
	}

	klog.V(5).Infof("%s - tracked NADS for pod %q: %#v", c.name, pod.Name, nadList)

	return nadList, nil
}

// syncAll builds the cache on initial controller start
func (c *PodTrackerController) syncAll() error {
	klog.Infof("%s: warming up cache with existing pods", c.name)

	pods, err := c.podLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list pods: %v", err)
	}

	for _, pod := range pods {
		if pod.Spec.NodeName == "" || pod.DeletionTimestamp != nil {
			continue
		}

		nadList, err := c.getNADsForPod(pod)
		if err != nil {
			klog.Errorf("Pod Tracker sync - Failed to get nads for pod %s/%s: %v", pod.Namespace, pod.Name, err)
			continue
		}
		if len(nadList) == 0 {
			continue
		}

		c.addPodToCache(pod, pod.Spec.NodeName, nadList)
	}

	klog.Infof("%s: cache warmup complete with %d pods", c.name, len(pods))
	return nil
}

// needUpdate return true when the pod has been created or updated.
func (c *PodTrackerController) needUpdate(old, new *corev1.Pod) bool {
	// ignore pods that only want hostNetwork with no NADs
	if new != nil && util.PodWantsHostNetwork(new) {
		if _, ok := new.Annotations[nadv1.NetworkAttachmentAnnot]; !ok {
			return false
		}
	}

	// Add
	if old == nil {
		return true
	}

	// If the node assignment changed (including unscheduled -> scheduled), reconcile.
	if old.Spec.NodeName != new.Spec.NodeName {
		return true
	}

	// If the network attachment annotations changed, reconcile.
	oldAnno := old.Annotations[nadv1.NetworkAttachmentAnnot]
	newAnno := new.Annotations[nadv1.NetworkAttachmentAnnot]
	return oldAnno != newAnno
}

// reconcile notify subscribers with the request namespace key following namespace events.
func (c *PodTrackerController) reconcile(key string) error {
	klog.V(5).Infof("%s reconcile called for pod %s", c.name, key)
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("failed to split meta namespace key %q: %v", key, err)
	}

	pod, err := c.podLister.Pods(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Pod deleted → cleanup cache
			c.deletePodFromCache(key)
			return nil
		}
		return fmt.Errorf("failed to get pod %q from cache: %v", key, err)
	}

	// If pod not scheduled or is terminating → remove from cache
	if pod.Spec.NodeName == "" || pod.DeletionTimestamp != nil {
		c.deletePodFromCache(key)
		return nil
	}

	nadList, err := c.getNADsForPod(pod)
	if err != nil {
		return err
	}
	if len(nadList) == 0 {
		c.deletePodFromCache(key)
		return nil
	}

	// Track pod under its node for each NAD
	c.addPodToCache(pod, pod.Spec.NodeName, nadList)

	return nil
}

func (c *PodTrackerController) addPodToCache(pod *corev1.Pod, node string, nads []string) {
	c.Lock()
	klog.V(5).Infof("%s - addPodToCache for pod %s/%s, node: %s, nads: %#v", c.name, pod.Namespace, pod.Name, node, nads)

	key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	// Short-circuit if identical node/NAD set already cached
	if existing, found := c.reverse[key]; found && existing.node == node && equalStringSets(existing.nads, nads) {
		c.Unlock()
		return
	}

	// First clean up any existing entry for this pod
	refChanges := c.deletePodFromCacheLocked(key)

	for _, nad := range nads {
		if _, ok := c.cache[node]; !ok {
			c.cache[node] = make(map[string]map[string]struct{})
		}
		if _, ok := c.cache[node][nad]; !ok {
			c.cache[node][nad] = make(map[string]struct{})
		}
		before := len(c.cache[node][nad])
		c.cache[node][nad][key] = struct{}{}
		after := len(c.cache[node][nad])
		if before == 0 && after == 1 {
			// 0 → 1 transition
			refChanges = append(refChanges, refChange{node, nad, true})
		}
	}

	c.reverse[key] = nodeNAD{node: node, nads: append([]string(nil), nads...)}
	c.Unlock()
	if c.onNetworkRefChange != nil {
		for _, callback := range refChanges {
			c.onNetworkRefChange(callback.node, callback.nad, callback.active)
		}
	}
}

func (c *PodTrackerController) deletePodFromCache(key string) {
	klog.V(5).Infof("%s - deletePodFromCache for pod %s", c.name, key)
	c.Lock()
	changes := c.deletePodFromCacheLocked(key)
	c.Unlock()

	if c.onNetworkRefChange != nil {
		for _, ev := range changes {
			c.onNetworkRefChange(ev.node, ev.nad, ev.active)
		}
	}
}

func (c *PodTrackerController) deletePodFromCacheLocked(key string) []refChange {
	var refChanges []refChange
	loc, ok := c.reverse[key]
	if !ok {
		return nil
	}

	for _, nad := range loc.nads {
		if _, ok := c.cache[loc.node][nad]; ok {
			before := len(c.cache[loc.node][nad])
			delete(c.cache[loc.node][nad], key)
			after := len(c.cache[loc.node][nad])
			if before == 1 && after == 0 {
				// 1 → 0 transition
				refChanges = append(refChanges, refChange{loc.node, nad, false})
			}
			if after == 0 {
				delete(c.cache[loc.node], nad)
			}
		}
	}
	if len(c.cache[loc.node]) == 0 {
		delete(c.cache, loc.node)
	}

	delete(c.reverse, key)
	return refChanges
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, v := range a {
		set[v] = struct{}{}
	}
	for _, v := range b {
		if _, ok := set[v]; !ok {
			return false
		}
	}
	return true
}
