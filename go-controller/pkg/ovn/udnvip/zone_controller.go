// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// Package udnvip implements the zone-level operations for UDN VIP LoadBalancer
// services (loadBalancerClass: k8s.ovn.org/udn-loadbalancer).
//
// The VIP uses the same OVN LB mechanism as ClusterIP — both live in the
// clusterLBGroup, so the GR carries LB rules for both. External traffic
// (BGP-routed) arrives at the node's GR and is DNAT'd there; no br-ex flows
// or kernel /32 routes are needed (empirically confirmed: no existing flows
// for UDN subnets on br-ex in a KIND cluster).
//
// The zone controller's sole job is to maintain a per-node annotation that
// lists the VIP CIDRs this node is "active" for (i.e. has a local backend
// pod or proxy pod running). The RouteAdvertisements controller reads this
// annotation to generate FRRConfiguration — advertising VIP /32s as plain
// ECMP from every node with local capacity.
//
// Annotation key (per-network scoped):
//
//	k8s.ovn.org/udn-lb-active-vips.<network-name>: ["10.200.200.1/32", ...]
package udnvip

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/udnvip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// UDNLBActiveVIPsAnnotation is the annotation key suffix written on Node
// objects by the zone controller. The full key is network-scoped:
//
//	<network-prefix>udn-lb-active-vips
//
// Value: JSON array of VIP CIDRs (/32 or /128) this node is advertising.
const UDNLBActiveVIPsAnnotation = "udn-lb-active-vips"

// ZoneController maintains the per-node VIP annotation for UDN VIP
// LoadBalancer services. It watches local backend pods (matching the service
// selector) and updates the annotation whenever the set of active VIPs on
// this node changes.
type ZoneController struct {
	mu       sync.Mutex
	nodeName string
	netInfo  util.NetInfo

	client     clientset.Interface
	svcLister  corelisters.ServiceLister
	podLister  corelisters.PodLister
	nodeLister corelisters.NodeLister

	queue workqueue.TypedRateLimitingInterface[string]

	// activeVIPs: svcKey (ns/name) → allocated VIP as net.IP
	activeVIPs map[string]net.IP
}

// NewZoneController constructs a ZoneController for the given network.
func NewZoneController(
	nodeName string,
	netInfo util.NetInfo,
	client clientset.Interface,
	svcInformer coreinformers.ServiceInformer,
	podInformer coreinformers.PodInformer,
	nodeInformer coreinformers.NodeInformer,
) *ZoneController {
	zc := &ZoneController{
		nodeName:   nodeName,
		netInfo:    netInfo,
		client:     client,
		svcLister:  svcInformer.Lister(),
		podLister:  podInformer.Lister(),
		nodeLister: nodeInformer.Lister(),
		activeVIPs: make(map[string]net.IP),
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{
				Name: "udnvip-zone-" + netInfo.GetNetworkName(),
			},
		),
	}

	_, _ = svcInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o interface{}) { zc.enqueueFromSvc(nil, o) },
		UpdateFunc: func(old, o interface{}) { zc.enqueueFromSvc(old, o) },
		DeleteFunc: func(o interface{}) { zc.enqueueFromSvc(o, nil) },
	})
	_, _ = podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o interface{}) { zc.enqueueFromPod(nil, o) },
		UpdateFunc: func(old, o interface{}) { zc.enqueueFromPod(old, o) },
		DeleteFunc: func(o interface{}) { zc.enqueueFromPod(o, nil) },
	})

	return zc
}

// Start runs the reconcile loop until ctx is cancelled.
func (zc *ZoneController) Start(ctx context.Context) error {
	defer zc.queue.ShutDown()
	go wait.UntilWithContext(ctx, zc.worker, 0)
	<-ctx.Done()
	return nil
}

// Stop shuts down the queue.
func (zc *ZoneController) Stop() { zc.queue.ShutDown() }

// ── Event handlers ────────────────────────────────────────────────────────────

func toSvc(obj interface{}) *corev1.Service {
	if obj == nil {
		return nil
	}
	if s, ok := obj.(*corev1.Service); ok {
		return s
	}
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		if s, ok := d.Obj.(*corev1.Service); ok {
			return s
		}
	}
	return nil
}

func toPod(obj interface{}) *corev1.Pod {
	if obj == nil {
		return nil
	}
	if p, ok := obj.(*corev1.Pod); ok {
		return p
	}
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		if p, ok := d.Obj.(*corev1.Pod); ok {
			return p
		}
	}
	return nil
}

func (zc *ZoneController) enqueueFromSvc(oldObj, newObj interface{}) {
	old, obj := toSvc(oldObj), toSvc(newObj)
	if obj != nil && udnvip.IsUDNVIPService(obj) && zc.inNetwork(obj.Namespace) {
		zc.enqueue(obj.Namespace + "/" + obj.Name)
	} else if old != nil && udnvip.IsUDNVIPService(old) && zc.inNetwork(old.Namespace) {
		zc.enqueue(old.Namespace + "/" + old.Name)
	}
}

func (zc *ZoneController) enqueueFromPod(oldObj, newObj interface{}) {
	for _, pod := range []*corev1.Pod{toPod(oldObj), toPod(newObj)} {
		if pod == nil || pod.Spec.NodeName != zc.nodeName || !zc.inNetwork(pod.Namespace) {
			continue
		}
		for _, key := range zc.affectedServices(pod) {
			zc.enqueue(key)
		}
	}
}

func (zc *ZoneController) enqueue(key string) { zc.queue.Add(key) }

// ── Worker ────────────────────────────────────────────────────────────────────

func (zc *ZoneController) worker(_ context.Context) {
	for {
		key, shutdown := zc.queue.Get()
		if shutdown {
			return
		}
		if err := zc.reconcileService(key); err != nil {
			utilruntime.HandleError(fmt.Errorf("udnvip zone: reconcile %s: %w", key, err))
			zc.queue.AddRateLimited(key)
		} else {
			zc.queue.Forget(key)
		}
		zc.queue.Done(key)
	}
}

// ── Core reconcile ────────────────────────────────────────────────────────────

func (zc *ZoneController) reconcileService(key string) error {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		return nil
	}
	ns, name := parts[0], parts[1]

	svc, err := zc.svcLister.Services(ns).Get(name)
	if apierrors.IsNotFound(err) || (svc != nil && svc.DeletionTimestamp != nil) {
		return zc.setActive(key, nil)
	}
	if err != nil {
		return err
	}
	if !udnvip.IsUDNVIPService(svc) || !zc.inNetwork(ns) {
		return zc.setActive(key, nil)
	}
	if len(svc.Status.LoadBalancer.Ingress) == 0 {
		return nil // VIP not yet allocated
	}
	vip := net.ParseIP(svc.Status.LoadBalancer.Ingress[0].IP)
	if vip == nil {
		return nil
	}

	hasLocal, err := zc.hasLocalPods(svc)
	if err != nil {
		return err
	}
	if hasLocal {
		return zc.setActive(key, vip)
	}
	return zc.setActive(key, nil)
}

// hasLocalPods returns true if the local node has a Running selector-matched
// backend pod for the given service.
func (zc *ZoneController) hasLocalPods(svc *corev1.Service) (bool, error) {
	pods, err := zc.podLister.Pods(svc.Namespace).List(labels.Everything())
	if err != nil {
		return false, err
	}
	sel := labels.Set(svc.Spec.Selector).AsSelector()
	if sel.Empty() {
		return false, nil
	}
	for _, pod := range pods {
		if pod.Spec.NodeName != zc.nodeName || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		if sel.Matches(labels.Set(pod.Labels)) {
			return true, nil
		}
	}
	return false, nil
}

// ── Annotation management ─────────────────────────────────────────────────────

// setActive adds or removes the VIP for svcKey and syncs the node annotation.
// vip == nil → remove.
func (zc *ZoneController) setActive(svcKey string, vip net.IP) error {
	zc.mu.Lock()
	defer zc.mu.Unlock()

	current, had := zc.activeVIPs[svcKey]
	if vip == nil && !had {
		return nil
	}
	if vip != nil && had && current.Equal(vip) {
		return nil
	}

	if vip == nil {
		delete(zc.activeVIPs, svcKey)
		klog.V(4).Infof("UDN VIP zone: cleared VIP for %s on node %s", svcKey, zc.nodeName)
	} else {
		c := make(net.IP, len(vip))
		copy(c, vip)
		zc.activeVIPs[svcKey] = c
		klog.V(4).Infof("UDN VIP zone: set VIP %s for %s on node %s", vip, svcKey, zc.nodeName)
	}

	return zc.syncAnnotation()
}

// syncAnnotation writes the current activeVIPs as a JSON array to the node.
// Must be called with zc.mu held.
func (zc *ZoneController) syncAnnotation() error {
	cidrs := make([]string, 0, len(zc.activeVIPs))
	for _, vip := range zc.activeVIPs {
		prefix := "/32"
		if vip.To4() == nil {
			prefix = "/128"
		}
		cidrs = append(cidrs, vip.String()+prefix)
	}

	raw, err := json.Marshal(cidrs)
	if err != nil {
		return err
	}

	node, err := zc.nodeLister.Get(zc.nodeName)
	if err != nil {
		return err
	}

	annKey := zc.netInfo.GetNetworkScopedName(UDNLBActiveVIPsAnnotation)
	if node.Annotations[annKey] == string(raw) {
		return nil // no change
	}

	nodeCopy := node.DeepCopy()
	if nodeCopy.Annotations == nil {
		nodeCopy.Annotations = make(map[string]string)
	}
	nodeCopy.Annotations[annKey] = string(raw)

	_, err = zc.client.CoreV1().Nodes().Update(
		context.TODO(), nodeCopy, metav1.UpdateOptions{})
	return err
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (zc *ZoneController) inNetwork(namespace string) bool {
	for _, ns := range zc.netInfo.GetNADNamespaces() {
		if ns == namespace {
			return true
		}
	}
	return false
}

func (zc *ZoneController) affectedServices(pod *corev1.Pod) []string {
	var keys []string
	svcs, _ := zc.svcLister.Services(pod.Namespace).List(labels.Everything())
	for _, svc := range svcs {
		if !udnvip.IsUDNVIPService(svc) || svc.Spec.Selector == nil {
			continue
		}
		if labels.Set(svc.Spec.Selector).AsSelector().Matches(labels.Set(pod.Labels)) {
			keys = append(keys, svc.Namespace+"/"+svc.Name)
		}
	}
	return keys
}
