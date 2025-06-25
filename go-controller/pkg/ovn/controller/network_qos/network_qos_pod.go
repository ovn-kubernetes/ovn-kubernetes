package networkqos

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	nqosv1alpha1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func (c *Controller) processNextNQOSPodWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	podKey, shutdown := c.nqosPodQueue.Get()
	if shutdown {
		return false
	}
	defer c.nqosPodQueue.Done(podKey)

	if err := c.syncNetworkQoSPod(podKey); err != nil {
		klog.Warningf("%s: Failed to reconcile pod %s: %v", c.controllerName, podKey, err)
		if c.nqosPodQueue.NumRequeues(podKey) < maxRetries {
			c.nqosPodQueue.AddRateLimited(podKey)
			return true
		}
		klog.Errorf("%s: Stop reconciling pod %s after %d retries", c.controllerName, podKey, maxRetries)
		utilruntime.HandleError(err)
	}
	c.nqosPodQueue.Forget(podKey)
	return true
}

// syncNetworkQoSPod decides the main logic everytime
// we dequeue a key from the nqosPodQueue cache
func (c *Controller) syncNetworkQoSPod(podKey string) error {
	startTime := time.Now()
	nqosNames, err := c.getNetworkQosForPodChange(podKey)
	if err != nil {
		return err
	}
	for nqosName := range nqosNames {
		c.nqosQueue.Add(nqosName)
	}
	recordPodReconcileDuration(c.controllerName, time.Since(startTime).Milliseconds())
	return nil
}

// setPodForNQOS will check if the pod meets source selector or dest selector
// - match source: add the ip to source address set, bind qos rule to the switch
// - match dest: add the ip to the destination address set
func (c *Controller) setPodForNQOS(pod *corev1.Pod, nqosState *networkQoSState, namespace *corev1.Namespace, addressSetMap map[string]sets.Set[string]) error {
	addresses, err := getPodAddresses(pod, c.NetInfo, c.podNetworkResolver())
	if err == nil && len(addresses) == 0 {
		// pod either is not attached to this network, or hasn't been annotated with addresses yet, return without retry
		klog.V(6).Infof("Pod %s/%s doesn't have addresses on network %s, skip NetworkQoS processing", pod.Namespace, pod.Name, c.GetNetworkName())
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to parse addresses for pod %s/%s, network %s, err: %v", pod.Namespace, pod.Name, c.GetNetworkName(), err)
	}
	fullPodName := joinMetaNamespaceAndName(pod.Namespace, pod.Name)
	// is pod in this zone
	if c.isPodScheduledinLocalZone(pod) {
		if matchSource := nqosState.matchSourceSelector(pod); matchSource {
			// pod's labels match source selector
			if err = nqosState.configureSourcePod(c, pod, addresses); err == nil {
				populateAddresses(addressSetMap, nqosState.SrcAddrSet.GetName(), addresses)
			}
		} else {
			// pod's labels don't match selector, but it probably matched previously
			err = nqosState.removePodFromSource(c, fullPodName, addresses)
		}
		if err != nil {
			return err
		}
	} else {
		klog.V(4).Infof("Pod %s is not scheduled in local zone, call remove to ensure it's not in source", fullPodName)
		err = nqosState.removePodFromSource(c, fullPodName, addresses)
		if err != nil {
			return err
		}
	}
	return reconcilePodForDestinations(nqosState, namespace, pod, addresses, addressSetMap)
}

func reconcilePodForDestinations(nqosState *networkQoSState, podNs *corev1.Namespace, pod *corev1.Pod, addresses []string, addressSetMap map[string]sets.Set[string]) error {
	fullPodName := joinMetaNamespaceAndName(pod.Namespace, pod.Name)
	for _, rule := range nqosState.EgressRules {
		for index, dest := range rule.Classifier.Destinations {
			if dest.PodSelector == nil && dest.NamespaceSelector == nil {
				continue
			}
			if dest.matchPod(podNs, pod, nqosState.namespace) {
				// add pod address to address set
				if err := dest.addPod(pod.Namespace, pod.Name, addresses); err != nil {
					return fmt.Errorf("failed to add addresses {%s} to dest address set %s for NetworkQoS %s/%s, rule index %d: %v", strings.Join(addresses, ","), dest.DestAddrSet.GetName(), nqosState.namespace, nqosState.name, index, err)
				}
				populateAddresses(addressSetMap, dest.DestAddrSet.GetName(), addresses)
			} else {
				// no match, remove the pod if it's previously selected
				if err := dest.removePod(fullPodName, addresses); err != nil {
					return fmt.Errorf("failed to delete addresses {%s} from dest address set %s for NetworkQoS %s/%s, rule index %d: %v", strings.Join(addresses, ","), dest.DestAddrSet.GetName(), nqosState.namespace, nqosState.name, index, err)
				}
			}
		}
	}
	return nil
}

func (c *Controller) getNetworkQosForPodChange(podKey string) (sets.Set[string], error) {
	podNsStr, podName, err := cache.SplitMetaNamespaceKey(podKey)
	if err != nil {
		return nil, fmt.Errorf("failed to split pod key %s: %v", podKey, err)
	}
	var podIPs []net.IP
	pod, err := c.nqosPodLister.Pods(podNsStr).Get(podName)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get pod %s: %v", podKey, err)
		}
		// fake dead pod with meta only
		pod = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:         podNsStr,
				Name:              podName,
				Labels:            getCachedPodLabels(c.podLabelsCache, podKey),
				DeletionTimestamp: &metav1.Time{Time: time.Now()},
			},
		}
	} else if pod.DeletionTimestamp == nil {
		// skip reconcile if pod labels and IPs don't change
		if podIPs, err = util.GetPodIPsOfNetwork(pod, c.NetInfo, c.podNetworkResolver()); err != nil {
			klog.Errorf("Failed to get IPs from pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		if labels.Equals(getCachedPodLabels(c.podLabelsCache, podKey), pod.Labels) && util.IPsAreEqual(podIPs, getCachedPodIPs(c.podIPCache, podKey)) {
			return nil, nil
		}
	}
	podNs, err := c.nqosNamespaceLister.Get(podNsStr)
	if err != nil {
		return nil, fmt.Errorf("failed to get namespace %s: %v", pod.Namespace, err)
	}
	nqoses, err := c.getAllNetworkQoSes()
	if err != nil {
		return nil, err
	}
	affectedNetworkQoSes := sets.Set[string]{}
	for _, nqos := range nqoses {
		if podMatchesSourceSelector(pod, nqos) {
			affectedNetworkQoSes.Insert(joinMetaNamespaceAndName(nqos.Namespace, nqos.Name))
			continue
		}
		// check if pod matches any egress
		for _, egress := range nqos.Spec.Egress {
			if podMatchesEgressSelector(podNs, pod, nqos, &egress) {
				affectedNetworkQoSes.Insert(joinMetaNamespaceAndName(nqos.Namespace, nqos.Name))
				continue
			}
		}
		if pod.DeletionTimestamp == nil && podSelectionChanged(nqos, pod.Labels, getCachedPodLabels(c.podLabelsCache, podKey)) {
			affectedNetworkQoSes.Insert(joinMetaNamespaceAndName(nqos.Namespace, nqos.Name))
		}
	}
	if pod.DeletionTimestamp == nil {
		c.podLabelsCache.Store(podKey, pod.Labels)
		c.podIPCache.Store(podKey, podIPs)
	} else {
		c.podLabelsCache.Delete(podKey)
		c.podIPCache.Delete(podKey)
	}
	return affectedNetworkQoSes, nil
}

func podMatchesSourceSelector(pod *corev1.Pod, nqos *nqosv1alpha1.NetworkQoS) bool {
	if nqos.Namespace != pod.Namespace {
		return false
	}
	if nqos.Spec.PodSelector.Size() == 0 {
		return true
	}
	podSelector, err := metav1.LabelSelectorAsSelector(&nqos.Spec.PodSelector)
	if err != nil {
		klog.Errorf("Failed to convert pod selector in %s/%s: %v", nqos.Namespace, nqos.Name, err)
		return false
	}
	return podSelector.Matches(labels.Set(pod.Labels))
}

func podMatchesEgressSelector(podNs *corev1.Namespace, pod *corev1.Pod, nqos *nqosv1alpha1.NetworkQoS, egress *nqosv1alpha1.Rule) bool {
	var nsSelector labels.Selector
	var podSelector labels.Selector
	var err error
	match := false
	for _, dest := range egress.Classifier.To {
		if dest.NamespaceSelector != nil {
			if nsSelector, err = metav1.LabelSelectorAsSelector(dest.NamespaceSelector); err != nil {
				klog.Errorf("Failed to convert namespace selector in %s/%s: %v", nqos.Namespace, nqos.Name, err)
				continue
			}
		}
		if dest.PodSelector != nil {
			if podSelector, err = metav1.LabelSelectorAsSelector(dest.PodSelector); err != nil {
				klog.Errorf("Failed to convert pod selector in %s/%s: %v", nqos.Namespace, nqos.Name, err)
				continue
			}
		}
		switch {
		case nsSelector != nil && podSelector != nil:
			match = nsSelector.Matches(labels.Set(podNs.Labels)) && podSelector.Matches(labels.Set(pod.Labels))
		case nsSelector == nil && podSelector != nil:
			match = pod.Namespace == nqos.Namespace && podSelector.Matches(labels.Set(pod.Labels))
		case nsSelector != nil && podSelector == nil:
			match = nsSelector.Matches(labels.Set(podNs.Labels))
		default: //nsSelector == nil && podSelector == nil:
			match = false
		}
		if match {
			return true
		}
	}
	return false
}

func podSelectionChanged(nqos *nqosv1alpha1.NetworkQoS, newLabels, oldLabels map[string]string) bool {
	if nqos.Spec.PodSelector.Size() > 0 {
		if podSelector, err := metav1.LabelSelectorAsSelector(&nqos.Spec.PodSelector); err != nil {
			klog.Errorf("Failed to convert pod selector in %s/%s: %v", nqos.Namespace, nqos.Name, err)
		} else if podSelector.Matches(labels.Set(newLabels)) != podSelector.Matches(labels.Set(oldLabels)) {
			return true
		}
	}
	for _, egress := range nqos.Spec.Egress {
		for _, dest := range egress.Classifier.To {
			if dest.PodSelector == nil {
				continue
			}
			if podSelector, err := metav1.LabelSelectorAsSelector(dest.PodSelector); err != nil {
				klog.Errorf("Failed to convert pod selector in %s/%s: %v", nqos.Namespace, nqos.Name, err)
			} else if podSelector.Matches(labels.Set(newLabels)) != podSelector.Matches(labels.Set(oldLabels)) {
				return true
			}
		}
	}
	return false
}

func populateAddresses(addressSetMap map[string]sets.Set[string], name string, addresses []string) {
	if len(addresses) == 0 {
		return
	}
	addressSet := addressSetMap[name]
	if addressSet == nil {
		addressSet = sets.New[string]()
	}
	addressSet.Insert(addresses...)
	addressSetMap[name] = addressSet
}

func getCachedPodLabels(labelCache *sync.Map, podKey string) map[string]string {
	if podLabels, _ := labelCache.Load(podKey); podLabels != nil {
		return podLabels.(map[string]string)
	}
	return nil
}

func getCachedPodIPs(cachedPodIPs *sync.Map, podKey string) []net.IP {
	if ips, _ := cachedPodIPs.Load(podKey); ips != nil {
		return ips.([]net.IP)
	}
	return nil
}
