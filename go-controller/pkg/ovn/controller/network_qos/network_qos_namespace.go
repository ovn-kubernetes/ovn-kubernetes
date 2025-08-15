package networkqos

import (
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	nqosv1alpha1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	crdtypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
)

func (c *Controller) processNextNQOSNamespaceWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	nsKey, shutdown := c.nqosNamespaceQueue.Get()
	if shutdown {
		return false
	}
	defer c.nqosNamespaceQueue.Done(nsKey)

	if err := c.syncNetworkQoSNamespace(nsKey); err != nil {
		klog.Warningf("%s: Failed to reconcile namespace %s: %v", c.controllerName, nsKey, err)
		if c.nqosNamespaceQueue.NumRequeues(nsKey) < maxRetries {
			c.nqosNamespaceQueue.AddRateLimited(nsKey)
			return true
		}
		klog.Errorf("%s: Stop reconciling namespace %s after %d retries", c.controllerName, nsKey, maxRetries)
		utilruntime.HandleError(err)
	}
	c.nqosNamespaceQueue.Forget(nsKey)
	return true
}

// syncNetworkQoSNamespace checks if the namespace change affects any NetworkQoS
func (c *Controller) syncNetworkQoSNamespace(nsKey string) error {
	startTime := time.Now()
	klog.V(5).Infof("Reconciling namespace event for %s ", nsKey)
	defer func() {
		klog.V(5).Infof("Finished reconciling namespace %s, took %v", nsKey, time.Since(startTime))
	}()
	nqosNames, err := c.getNetworkQosForNamespaceChange(nsKey)
	if err != nil {
		return err
	}
	for nqosName := range nqosNames {
		c.nqosQueue.Add(nqosName)
	}
	recordNamespaceReconcileDuration(c.controllerName, time.Since(startTime).Milliseconds())
	return nil
}

// getNetworkQosForNamespaceChange returns the set of NetworkQoS names that are affected by the namespace change
func (c *Controller) getNetworkQosForNamespaceChange(nsKey string) (sets.Set[string], error) {
	ns, err := c.nqosNamespaceLister.Get(nsKey)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get namespace %s: %v", nsKey, err)
		}
		// fake dead namespace
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:              nsKey,
				Labels:            getCachedNsLabels(c.nsLabelsCache, nsKey),
				DeletionTimestamp: &metav1.Time{Time: time.Now()},
			},
		}
	}
	if ns.ObjectMeta.DeletionTimestamp == nil && labels.Equals(getCachedNsLabels(c.nsLabelsCache, nsKey), ns.Labels) {
		return nil, nil
	}
	networkQoSes := sets.Set[string]{}
	nqoses, err := c.getAllNetworkQoSes()
	if err != nil {
		return nil, err
	}
	for _, nqos := range nqoses {
		// check if any network selector or egress rule matches the namespace
		if namespaceMatchesNetworkSelector(ns, nqos) || namespaceMatchesEgressRule(ns, nqos) {
			networkQoSes.Insert(joinMetaNamespaceAndName(nqos.Namespace, nqos.Name))
			continue
		}
		// check if namespace label change affects the network selection or egress selection
		if ns.DeletionTimestamp == nil && (networkSelectionChanged(nqos, ns.Labels, getCachedNsLabels(c.nsLabelsCache, nsKey)) || egressSelectionChanged(nqos, ns.Labels, getCachedNsLabels(c.nsLabelsCache, nsKey))) {
			networkQoSes.Insert(joinMetaNamespaceAndName(nqos.Namespace, nqos.Name))
		}
	}
	if ns.DeletionTimestamp == nil {
		c.nsLabelsCache.Store(nsKey, ns.Labels)
	} else {
		c.nsLabelsCache.Delete(nsKey)
	}
	return networkQoSes, nil
}

// namespaceMatchesNetworkSelector checks if the namespace matches any of the network selectors in the NetworkQoS
func namespaceMatchesNetworkSelector(namespace *corev1.Namespace, nqos *nqosv1alpha1.NetworkQoS) bool {
	for _, selector := range nqos.Spec.NetworkSelectors {
		var nsSelector *metav1.LabelSelector
		switch {
		case selector.NetworkAttachmentDefinitionSelector != nil:
			if selector.NetworkAttachmentDefinitionSelector.NamespaceSelector.Size() == 0 {
				// namespace selector is empty, match all
				return true
			}
			nsSelector = &selector.NetworkAttachmentDefinitionSelector.NamespaceSelector
		case selector.PrimaryUserDefinedNetworkSelector != nil:
			if selector.PrimaryUserDefinedNetworkSelector.NamespaceSelector.Size() == 0 {
				// namespace selector is empty, match all
				return true
			}
			nsSelector = &selector.PrimaryUserDefinedNetworkSelector.NamespaceSelector
		case selector.SecondaryUserDefinedNetworkSelector != nil:
			if selector.SecondaryUserDefinedNetworkSelector.NamespaceSelector.Size() == 0 {
				// namespace selector is empty, match all
				return true
			}
			nsSelector = &selector.SecondaryUserDefinedNetworkSelector.NamespaceSelector
		}
		if nsSelector == nil {
			continue
		}
		if ls, err := metav1.LabelSelectorAsSelector(nsSelector); err != nil {
			klog.Errorf("%s/%s - failed to convert namespace selector %s : %v", nqos.Namespace, nqos.Name, nsSelector.String(), err)
		} else if ls != nil && ls.Matches(labels.Set(namespace.Labels)) {
			return true
		}
	}
	return false
}

func namespaceMatchesEgressRule(namespace *corev1.Namespace, nqos *nqosv1alpha1.NetworkQoS) bool {
	for _, egress := range nqos.Spec.Egress {
		for _, dest := range egress.Classifier.To {
			if dest.NamespaceSelector == nil || dest.NamespaceSelector.Size() == 0 {
				// namespace selector is empty, match all
				return true
			}
			if ls, err := metav1.LabelSelectorAsSelector(dest.NamespaceSelector); err != nil {
				klog.Errorf("%s/%s - failed to convert egress namespace selector %s: %v", nqos.Namespace, nqos.Name, dest.NamespaceSelector.String(), err)
			} else if ls != nil && ls.Matches(labels.Set(namespace.Labels)) {
				return true
			}
		}
	}
	return false
}

// check if namespace change causes the network selection change
func networkSelectionChanged(nqos *nqosv1alpha1.NetworkQoS, newLabels, oldLabels map[string]string) bool {
	for _, selector := range nqos.Spec.NetworkSelectors {
		var nsSelector *metav1.LabelSelector
		switch selector.NetworkSelectionType {
		case crdtypes.PrimaryUserDefinedNetworks:
			if selector.PrimaryUserDefinedNetworkSelector != nil {
				nsSelector = &selector.PrimaryUserDefinedNetworkSelector.NamespaceSelector
			}
		case crdtypes.SecondaryUserDefinedNetworks:
			if selector.SecondaryUserDefinedNetworkSelector != nil {
				nsSelector = &selector.SecondaryUserDefinedNetworkSelector.NamespaceSelector
			}
		case crdtypes.NetworkAttachmentDefinitions:
			if selector.NetworkAttachmentDefinitionSelector != nil {
				nsSelector = &selector.NetworkAttachmentDefinitionSelector.NamespaceSelector
			}
		}
		if nsSelector == nil {
			continue
		}
		if ls, err := metav1.LabelSelectorAsSelector(nsSelector); err != nil {
			// namespace selector is not valid, skip this selector
			klog.Errorf("%s/%s - failed to convert namespace selector %s: %v", nqos.Namespace, nqos.Name, nsSelector.String(), err)
		} else {
			return ls.Matches(labels.Set(oldLabels)) != ls.Matches(labels.Set(newLabels))
		}
	}
	return false
}

func egressSelectionChanged(nqos *nqosv1alpha1.NetworkQoS, newLabels, oldLabels map[string]string) bool {
	for _, egress := range nqos.Spec.Egress {
		for _, dest := range egress.Classifier.To {
			if dest.NamespaceSelector == nil || dest.NamespaceSelector.Size() == 0 {
				// empty namespace selector won't make difference
				continue
			}
			if nsSelector, err := metav1.LabelSelectorAsSelector(dest.NamespaceSelector); err != nil {
				klog.Errorf("Failed to convert namespace selector in %s/%s: %v", nqos.Namespace, nqos.Name, err)
			} else {
				return nsSelector.Matches(labels.Set(oldLabels)) != nsSelector.Matches(labels.Set(newLabels))
			}
		}
	}
	return false
}

func getCachedNsLabels(labelCache *sync.Map, ns string) map[string]string {
	if nsLabels, _ := labelCache.Load(ns); nsLabels != nil {
		return nsLabels.(map[string]string)
	}
	return nil
}
