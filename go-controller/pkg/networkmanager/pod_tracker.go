package networkmanager

import (
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type PodTrackerController struct {
	sync.Mutex
	// cache holds a mapping of node -> NAD namespaced name -> pod namespaced name
	cache map[string]map[string]map[string]struct{}
	// reverse index: pod key -> (node, NAD)
	reverse map[string]struct {
		node string
		nads []string
	}
	// callback when a node+NAD goes active/inactive
	onNetworkRefChange func(node, nad string, active bool)
	podController      controller.Controller
	podLister          v1.PodLister
	networkManger      Interface
}

func NewPodTrackerController(wf *factory.WatchFactory, nm Interface, onNetworkRefChange func(node, nad string, active bool)) *PodTrackerController {
	p := &PodTrackerController{
		cache: make(map[string]map[string]map[string]struct{}),
		reverse: make(map[string]struct {
			node string
			nads []string
		}),
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

	var nadList []string

	// 1. Primary NAD from namespace
	primaryNetwork, err := c.networkManger.GetActiveNetworkForNamespace(namespace)
	if err != nil {
		return err
	}

	if !primaryNetwork.IsDefault() {
		nadList = append(nadList, primaryNetwork.GetNADs()...)
	}

	// 2. Secondary NADs from pod annotation
	networks, err := util.GetK8sPodAllNetworkSelections(pod)
	if err != nil {
		return err
	}

	for _, net := range networks {
		ns := net.Namespace
		if ns == "" {
			ns = namespace // default to pod's ns
		}
		nadList = append(nadList, fmt.Sprintf("%s/%s", ns, net.Name))
	}

	// If no NADs → ensure cleanup
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

	c.reverse[key] = struct {
		node string
		nads []string
	}{node: node, nads: nads}
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
