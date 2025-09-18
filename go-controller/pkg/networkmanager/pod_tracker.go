package networkmanager

import (
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type nodeNAD struct {
	node string
	nads []string
}

type PodTrackerController struct {
	sync.Mutex
	// cache holds a mapping of node -> NAD namespaced name -> pod namespaced name
	cache map[string]map[string]map[string]struct{}
	// reverse index: pod key -> (node, NAD)
	reverse map[string]nodeNAD
	// callback when a node+NAD goes active/inactive
	onNetworkRefChange func(node, nad string, active bool)
	podController      controller.Controller
	podLister          v1.PodLister
	networkManger      Interface
}

func NewPodTrackerController(wf watchFactory, nm Interface, onNetworkRefChange func(node, nad string, active bool)) *PodTrackerController {
	p := &PodTrackerController{
		cache:              make(map[string]map[string]map[string]struct{}),
		reverse:            make(map[string]nodeNAD),
		onNetworkRefChange: onNetworkRefChange,
		podLister:          wf.PodCoreInformer().Lister(),
		networkManger:      nm,
	}

	cfg := &controller.ControllerConfig[corev1.Pod]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      p.reconcile,
		ObjNeedsUpdate: p.needUpdate,
		Threadiness:    1,
		Informer:       wf.PodCoreInformer().Informer(),
		Lister:         wf.PodCoreInformer().Lister().List,
	}
	p.podController = controller.NewController[corev1.Pod]("pod-tracker", cfg)
	return p
}

func (c *PodTrackerController) Start() error {
	klog.Infof("Starting pod tracker controller")
	return controller.StartWithInitialSync(c.syncAll, c.podController)
}

func (c *PodTrackerController) Stop() {
	klog.Infof("Stopping pod tracker controller")
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

	// Primary NAD from namespace
	primaryNetwork, err := c.networkManger.GetActiveNetworkForNamespace(pod.Namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to get primary network for ns %q: %v", pod.Namespace, err)
	}
	if !primaryNetwork.IsDefault() {
		nadList = append(nadList, primaryNetwork.GetNADs()...)
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

	return nadList, nil
}

// syncAll builds the cache on initial controller start
func (c *PodTrackerController) syncAll() error {
	klog.Infof("PodTrackerController: warming up cache with existing pods")

	pods, err := c.podLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list pods: %v", err)
	}

	for _, pod := range pods {
		if pod.Spec.NodeName == "" || pod.DeletionTimestamp != nil {
			continue
		}

		nadList, err := c.getNADsForPod(pod)
		if err != nil || len(nadList) == 0 {
			continue
		}

		c.addPodToCache(pod, pod.Spec.NodeName, nadList)
	}

	klog.Infof("PodTrackerController: cache warmup complete with %d pods", len(pods))
	return nil
}

// needUpdate return true when the pod has been deleted or created.
func (c *PodTrackerController) needUpdate(old, _ *corev1.Pod) bool {
	return old == nil
}

// reconcile notify subscribers with the request namespace key following namespace events.
func (c *PodTrackerController) reconcile(key string) error {
	klog.V(5).Infof("PodTrackerController reconcile called for pod %s", key)
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
	if err != nil || len(nadList) == 0 {
		c.deletePodFromCache(key)
		return nil
	}

	// Track pod under its node for each NAD
	c.addPodToCache(pod, pod.Spec.NodeName, nadList)

	return nil
}

func (c *PodTrackerController) addPodToCache(pod *corev1.Pod, node string, nads []string) {
	c.Lock()
	defer c.Unlock()
	klog.V(5).Infof("podTracker - addPodToCache for pod %s/%s, node: %s, nads: %#v", pod.Namespace, pod.Name, node, nads)

	key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	// First clean up any existing entry for this pod
	c.deletePodFromCacheLocked(key)

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
		if before == 0 && after == 1 && c.onNetworkRefChange != nil {
			// 0 → 1 transition
			c.onNetworkRefChange(node, nad, true)
		}
	}

	c.reverse[key] = nodeNAD{node: node, nads: nads}
}

func (c *PodTrackerController) deletePodFromCache(key string) {
	c.Lock()
	defer c.Unlock()
	c.deletePodFromCacheLocked(key)
}

func (c *PodTrackerController) deletePodFromCacheLocked(key string) {
	loc, ok := c.reverse[key]
	if !ok {
		return
	}

	for _, nad := range loc.nads {
		if _, ok := c.cache[loc.node][nad]; ok {
			before := len(c.cache[loc.node][nad])
			delete(c.cache[loc.node][nad], key)
			after := len(c.cache[loc.node][nad])
			if before == 1 && after == 0 && c.onNetworkRefChange != nil {
				// 1 → 0 transition
				c.onNetworkRefChange(loc.node, nad, false)
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
}
