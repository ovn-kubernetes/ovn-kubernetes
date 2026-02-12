package networkqos

import (
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	nqosv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	crdtypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
)

func (c *Controller) processNextNQOSNamespaceWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	eventData, shutdown := c.nqosNamespaceQueue.Get()
	if shutdown {
		return false
	}
	defer c.nqosNamespaceQueue.Done(eventData)

	if err := c.syncNetworkQoSNamespace(eventData); err != nil {
		if c.nqosNamespaceQueue.NumRequeues(eventData) < maxRetries {
			klog.Errorf("%s: Failed to reconcile namespace %s: %v", c.controllerName, eventData.name(), err)
			c.nqosNamespaceQueue.AddRateLimited(eventData)
			return true
		}
		utilruntime.HandleError(fmt.Errorf("failed to reconcile namespace %s: %v", eventData.name(), err))
	}
	c.nqosNamespaceQueue.Forget(eventData)
	return true
}

// syncNetworkQoSNamespace checks if the namespace change affects any NetworkQoS
func (c *Controller) syncNetworkQoSNamespace(eventData *eventData[*corev1.Namespace]) error {
	startTime := time.Now()
	klog.V(5).Infof("Reconciling namespace event for %s ", eventData.name())
	defer func() {
		klog.V(5).Infof("Finished reconciling namespace %s, took %v", eventData.name(), time.Since(startTime))
	}()
	nqosNames, err := c.getNetworkQosForNamespaceChange(eventData)
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
func (c *Controller) getNetworkQosForNamespaceChange(eventData *eventData[*corev1.Namespace]) (sets.Set[string], error) {
	networkQoSes := sets.Set[string]{}
	nqoses, err := c.getAllNetworkQoSes()
	if err != nil {
		return nil, err
	}
	for _, nqos := range nqoses {
		ns := eventData.new
		if ns == nil {
			ns = eventData.old
		}
		// check if any network selector matches the namespace, or ns label change affects the network selection
		if namespaceMatchesNetworkSelector(ns, nqos) || networkSelectionChanged(nqos, eventData.new, eventData.old) {
			networkQoSes.Insert(joinMetaNamespaceAndName(nqos.Namespace, nqos.Name))
			continue
		}
		// check if any egress rule matches the namespace, or ns label change affects the egress selection
		if namespaceMatchesEgressRule(ns, nqos) || egressSelectionChanged(nqos, eventData.new, eventData.old) {
			networkQoSes.Insert(joinMetaNamespaceAndName(nqos.Namespace, nqos.Name))
		}
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
func networkSelectionChanged(nqos *nqosv1alpha1.NetworkQoS, new *corev1.Namespace, old *corev1.Namespace) bool {
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
		} else if old != nil && new != nil {
			return ls.Matches(labels.Set(old.Labels)) != ls.Matches(labels.Set(new.Labels))
		}
	}
	return false
}

func egressSelectionChanged(nqos *nqosv1alpha1.NetworkQoS, new *corev1.Namespace, old *corev1.Namespace) bool {
	for _, egress := range nqos.Spec.Egress {
		for _, dest := range egress.Classifier.To {
			if dest.NamespaceSelector == nil || dest.NamespaceSelector.Size() == 0 {
				// empty namespace selector won't make difference
				continue
			}
			if nsSelector, err := metav1.LabelSelectorAsSelector(dest.NamespaceSelector); err != nil {
				klog.Errorf("Failed to convert namespace selector in %s/%s: %v", nqos.Namespace, nqos.Name, err)
			} else if old != nil && new != nil {
				return nsSelector.Matches(labels.Set(old.Labels)) != nsSelector.Matches(labels.Set(new.Labels))
			}
		}
	}
	return false
}
