// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	utilnet "k8s.io/utils/net"
)

// EgressServicePodIPs returns the IP-to-node mapping of pods eligible for a
// Service's egress rules. EndpointSlice addresses and target references are
// user-writable, so ownership must be established independently from pod status.
// Selectorless Services may use any non-host-network pod in their namespace.
func EgressServicePodIPs(svc *corev1.Service, podLister corelisters.PodLister) (map[string]string, error) {
	pods, err := podLister.Pods(svc.Namespace).List(labels.SelectorFromSet(svc.Spec.Selector))
	if err != nil {
		return nil, err
	}
	ips := map[string]string{}
	for _, pod := range pods {
		if pod.Spec.HostNetwork || pod.Spec.NodeName == "" || PodCompleted(pod) {
			continue
		}
		podIPs := pod.Status.PodIPs
		if len(podIPs) == 0 && pod.Status.PodIP != "" {
			podIPs = []corev1.PodIP{{IP: pod.Status.PodIP}}
		}
		for _, podIP := range podIPs {
			if ip := utilnet.ParseIPSloppy(podIP.IP); ip != nil {
				ips[ip.String()] = pod.Spec.NodeName
			}
		}
	}
	return ips, nil
}

// EgressServicePodEventHandler reconciles namespace Services when pod ownership
// changes, including when pod and EndpointSlice informer events arrive out of
// order. This also removes rules for stale slices after their pods disappear.
func EgressServicePodEventHandler(serviceLister corelisters.ServiceLister, enqueue func(string)) cache.ResourceEventHandlerFuncs {
	enqueuePod := func(obj interface{}) {
		if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = tombstone.Obj
		}
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return
		}
		services, err := serviceLister.Services(pod.Namespace).List(labels.Everything())
		if err != nil {
			utilruntime.HandleError(err)
			return
		}
		// Include Services that no longer select this pod so their rules are removed.
		for _, svc := range services {
			if ServiceTypeHasLoadBalancer(svc) {
				enqueue(svc.Namespace + "/" + svc.Name)
			}
		}
	}
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    enqueuePod,
		DeleteFunc: enqueuePod,
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod, newPod := oldObj.(*corev1.Pod), newObj.(*corev1.Pod)
			if oldPod.UID != newPod.UID || oldPod.Spec.NodeName != newPod.Spec.NodeName ||
				oldPod.Spec.HostNetwork != newPod.Spec.HostNetwork || oldPod.Status.Phase != newPod.Status.Phase ||
				oldPod.Status.PodIP != newPod.Status.PodIP || !reflect.DeepEqual(oldPod.Status.PodIPs, newPod.Status.PodIPs) ||
				!reflect.DeepEqual(oldPod.Labels, newPod.Labels) {
				enqueuePod(newPod)
			}
		},
	}
}
