// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// Package udnvip implements VIP allocation and status management for UDN LoadBalancer
// Services. It runs in the cluster manager and covers both NAT-mode and routed-mode VIPs
// as described in OKEP-XXXX.
//
// Controller responsibilities:
//   - Watches LoadBalancer Services whose spec.loadBalancerClass is one of the two OVN
//     VIP class values (k8s.ovn.org/udn-vip-nat, k8s.ovn.org/udn-vip-routed).
//   - Identifies the CUDN that owns the Service's namespace (one CUDN per namespace).
//   - Allocates a VIP from the CUDN's serviceSubnets IPAM pool.
//   - Writes the allocated VIP to service.status.loadBalancer.ingress.
//   - Releases the VIP when the Service is deleted.
//
// The allocator is rebuilt from service status on restart — no separate persistence is
// required.
package udnvip

import (
	"context"
	"fmt"
	"net"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	kubernetes_scheme "k8s.io/client-go/kubernetes/scheme"
	corev1typed "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnv1lister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// udnVIPClasses is the set of loadBalancerClass values that trigger VIP allocation.
var udnVIPClasses = sets.New(
	types.UDNLoadBalancerClass,
)

// networkAllocator tracks VIP allocations for a single CUDN's serviceSubnets.
type networkAllocator struct {
	mu        sync.Mutex
	subnets   []*net.IPNet
	allocated map[string]net.IP // svcKey (namespace/name) -> allocated IP
}

// allocate allocates the next free IP from the serviceSubnets for the given service key.
// If the service already has an allocation it is returned unchanged (idempotent).
func (a *networkAllocator) allocate(svcKey string) (net.IP, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if ip, ok := a.allocated[svcKey]; ok {
		return ip, nil
	}

	// Build set of already-allocated IPs.
	used := make(map[string]struct{}, len(a.allocated))
	for _, ip := range a.allocated {
		used[ip.String()] = struct{}{}
	}

	for _, subnet := range a.subnets {
		// Iterate host addresses in the subnet (skip network and broadcast).
		for ip := nextIP(subnet.IP); subnet.Contains(ip); ip = nextIP(ip) {
			if isBroadcast(ip, subnet) {
				continue
			}
			if _, taken := used[ip.String()]; !taken {
				allocated := cloneIP(ip)
				a.allocated[svcKey] = allocated
				return allocated, nil
			}
		}
	}
	return nil, fmt.Errorf("no free VIP address available in serviceSubnets for network serving %q", svcKey)
}

// allocateSpecific claims a particular IP for svcKey. Returns an error if the
// IP is outside all serviceSubnets or is already allocated to a different key.
func (a *networkAllocator) allocateSpecific(svcKey string, ip net.IP) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Idempotent: already allocated to this key.
	if existing, ok := a.allocated[svcKey]; ok && existing.Equal(ip) {
		return nil
	}

	// Validate the IP is within one of our subnets.
	inSubnet := false
	for _, subnet := range a.subnets {
		if subnet.Contains(ip) {
			inSubnet = true
			break
		}
	}
	if !inSubnet {
		return fmt.Errorf("IP %s is not within any serviceSubnets", ip)
	}

	// Check it isn't already taken.
	for otherKey, existing := range a.allocated {
		if existing.Equal(ip) {
			return fmt.Errorf("IP %s already allocated to %s", ip, otherKey)
		}
	}

	a.allocated[svcKey] = cloneIP(ip)
	return nil
}

// release frees the allocation for the given service key. No-op if not allocated.
func (a *networkAllocator) release(svcKey string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.allocated, svcKey)
}

// seed marks an IP as already allocated for a given service key (used during restart).
func (a *networkAllocator) seed(svcKey string, ip net.IP) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.allocated[svcKey] = ip
}

// Controller manages VIP allocation for UDN LoadBalancer Services.
type Controller struct {
	client     kubernetes.Interface
	recorder   record.EventRecorder
	svcLister  corev1listers.ServiceLister
	svcSynced  cache.InformerSynced
	nsLister   corev1listers.NamespaceLister
	cudnLister udnv1lister.ClusterUserDefinedNetworkLister
	queue      workqueue.TypedRateLimitingInterface[string]

	mu         sync.Mutex
	allocators map[string]*networkAllocator // cudnName -> allocator
}

// NewController builds a new UDN VIP controller.
func NewController(
	client kubernetes.Interface,
	svcInformer corev1informers.ServiceInformer,
	nsInformer corev1informers.NamespaceInformer,
	cudnLister udnv1lister.ClusterUserDefinedNetworkLister,
) *Controller {
	scheme := runtime.NewScheme()
	_ = kubernetes_scheme.AddToScheme(scheme)
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&corev1typed.EventSinkImpl{
		Interface: client.CoreV1().Events(""),
	})

	c := &Controller{
		client:     client,
		recorder:   broadcaster.NewRecorder(scheme, corev1.EventSource{Component: "ovnk-udn-vip"}),
		svcLister:  svcInformer.Lister(),
		svcSynced:  svcInformer.Informer().HasSynced,
		nsLister:   nsInformer.Lister(),
		cudnLister: cudnLister,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
		allocators: make(map[string]*networkAllocator),
	}

	_, _ = svcInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onServiceAdd,
		UpdateFunc: c.onServiceUpdate,
		DeleteFunc: c.onServiceDelete,
	})

	return c
}

// Run starts the controller and blocks until stopCh is closed.
func (c *Controller) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Info("Starting UDN VIP controller")
	defer klog.Info("Shutting down UDN VIP controller")

	if !cache.WaitForCacheSync(stopCh, c.svcSynced) {
		klog.Error("UDN VIP controller: timed out waiting for caches to sync")
		return
	}

	// Rebuild allocator state from existing service status on startup.
	if err := c.rebuildAllocators(); err != nil {
		klog.Errorf("UDN VIP controller: failed to rebuild allocators: %v", err)
	}

	go func() {
		for c.processNextItem() {
		}
	}()

	<-stopCh
}

func (c *Controller) processNextItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	if err := c.syncService(key); err != nil {
		klog.Errorf("UDN VIP controller: error syncing service %q: %v — requeuing", key, err)
		c.queue.AddRateLimited(key)
	} else {
		c.queue.Forget(key)
	}
	return true
}

func (c *Controller) onServiceAdd(obj interface{}) {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		return
	}
	if c.isUDNVIPService(svc) {
		c.queue.Add(svc.Namespace + "/" + svc.Name)
	}
}

func (c *Controller) onServiceUpdate(_ interface{}, newObj interface{}) {
	svc, ok := newObj.(*corev1.Service)
	if !ok {
		return
	}
	if c.isUDNVIPService(svc) {
		c.queue.Add(svc.Namespace + "/" + svc.Name)
	}
}

func (c *Controller) onServiceDelete(obj interface{}) {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			svc, ok = tombstone.Obj.(*corev1.Service)
			if !ok {
				return
			}
		} else {
			return
		}
	}
	if c.isUDNVIPService(svc) {
		c.queue.Add(svc.Namespace + "/" + svc.Name)
	}
}

func (c *Controller) isUDNVIPService(svc *corev1.Service) bool {
	return svc.Spec.Type == corev1.ServiceTypeLoadBalancer &&
		svc.Spec.LoadBalancerClass != nil &&
		udnVIPClasses.Has(*svc.Spec.LoadBalancerClass)
}

// syncService reconciles a single LoadBalancer service: allocate or release VIP.
func (c *Controller) syncService(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil
	}

	svc, err := c.svcLister.Services(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		// Service deleted — release the VIP.
		return c.releaseVIP(namespace, name)
	}
	if err != nil {
		return err
	}

	if !c.isUDNVIPService(svc) {
		// loadBalancerClass changed away from our classes — release.
		return c.releaseVIP(namespace, name)
	}

	// Find the CUDN for this namespace.
	cudn, err := c.cudnForNamespace(namespace)
	if err != nil {
		return fmt.Errorf("could not determine CUDN for namespace %q: %w", namespace, err)
	}
	if cudn == nil {
		return fmt.Errorf("no CUDN found for namespace %q", namespace)
	}

	alloc, err := c.allocatorForCUDN(cudn)
	if err != nil {
		return err
	}
	if alloc == nil {
		return fmt.Errorf("CUDN %q has no serviceSubnets configured", cudn.Name)
	}

	// Check if VIP is already written in status.
	if len(svc.Status.LoadBalancer.Ingress) > 0 {
		// Already allocated — ensure it is seeded in our allocator (idempotent).
		for _, ing := range svc.Status.LoadBalancer.Ingress {
			if ing.IP != "" {
				alloc.seed(key, net.ParseIP(ing.IP))
			}
		}
		return nil
	}

	// Honour an explicit VIP request from the annotation.
	if requested := svc.Annotations[types.UDNVIPAnnotation]; requested != "" {
		ip := net.ParseIP(requested)
		if ip == nil {
			return fmt.Errorf("service %s annotation %s: %q is not a valid IP",
				key, types.UDNVIPAnnotation, requested)
		}
		if err := alloc.allocateSpecific(key, ip); err != nil {
			// The requested VIP is already claimed by another service or is
			// outside serviceSubnets. Emit a Warning event so the operator
			// can see it via 'kubectl describe svc', and do NOT requeue —
			// the conflict won't resolve itself without user intervention.
			c.recorder.Eventf(svc, corev1.EventTypeWarning, "VIPConflict",
				"annotation %s: %v", types.UDNVIPAnnotation, err)
			klog.Warningf("UDN VIP: %s/%s requested VIP %s but allocation failed: %v",
				svc.Namespace, svc.Name, ip, err)
			return nil // do not requeue; needs operator action
		}
		return c.writeServiceStatus(svc, ip)
	}

	// Allocate a new VIP.
	vip, err := alloc.allocate(key)
	if err != nil {
		return err
	}

	// Write VIP to service status.
	return c.writeServiceStatus(svc, vip)
}

// releaseVIP frees the allocation for a deleted/changed service.
func (c *Controller) releaseVIP(namespace, name string) error {
	key := namespace + "/" + name
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, alloc := range c.allocators {
		alloc.release(key)
	}
	return nil
}

// cudnForNamespace returns the CUDN whose namespaceSelector matches the given namespace,
// or nil if none is found. Returns an error only on lookup failures.
func (c *Controller) cudnForNamespace(namespace string) (*udnv1.ClusterUserDefinedNetwork, error) {
	ns, err := c.nsLister.Get(namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to get namespace %q: %w", namespace, err)
	}
	nsLabels := labels.Set(ns.Labels)

	cudns, err := c.cudnLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list CUDNs: %w", err)
	}
	for _, cudn := range cudns {
		sel, err := metav1.LabelSelectorAsSelector(&cudn.Spec.NamespaceSelector)
		if err != nil {
			continue
		}
		if sel.Matches(nsLabels) {
			return cudn, nil
		}
	}
	return nil, nil
}

// allocatorForCUDN returns (or creates) the networkAllocator for the given CUDN.
// Returns nil, nil if the CUDN has no serviceSubnets.
func (c *Controller) allocatorForCUDN(cudn *udnv1.ClusterUserDefinedNetwork) (*networkAllocator, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if alloc, ok := c.allocators[cudn.Name]; ok {
		return alloc, nil
	}

	svcSubnets := serviceSubnetsFromCUDN(cudn)
	if len(svcSubnets) == 0 {
		return nil, nil
	}

	alloc := &networkAllocator{
		subnets:   svcSubnets,
		allocated: make(map[string]net.IP),
	}
	c.allocators[cudn.Name] = alloc
	return alloc, nil
}

// serviceSubnetsFromCUDN extracts parsed *net.IPNet service subnets from a CUDN's Layer2Config.
func serviceSubnetsFromCUDN(cudn *udnv1.ClusterUserDefinedNetwork) []*net.IPNet {
	spec := cudn.Spec.Network
	if spec.Layer2 == nil {
		return nil
	}
	var result []*net.IPNet
	for _, cidr := range spec.Layer2.ServiceSubnets {
		_, ipNet, err := net.ParseCIDR(string(cidr))
		if err == nil {
			result = append(result, ipNet)
		}
	}
	return result
}

// rebuildAllocators re-seeds the VIP allocators from existing service statuses on startup.
func (c *Controller) rebuildAllocators() error {
	svcs, err := c.svcLister.List(labels.Everything())
	if err != nil {
		return err
	}
	for _, svc := range svcs {
		if !c.isUDNVIPService(svc) {
			continue
		}
		if len(svc.Status.LoadBalancer.Ingress) == 0 {
			continue
		}

		cudn, err := c.cudnForNamespace(svc.Namespace)
		if err != nil || cudn == nil {
			continue
		}
		alloc, err := c.allocatorForCUDN(cudn)
		if err != nil || alloc == nil {
			continue
		}

		for _, ing := range svc.Status.LoadBalancer.Ingress {
			if ip := net.ParseIP(ing.IP); ip != nil {
				key := svc.Namespace + "/" + svc.Name
				alloc.seed(key, ip)
				klog.V(4).InfoS("UDN VIP: re-seeded allocation from service status",
					"service", key, "vip", ip)
			}
		}
	}
	return nil
}

// writeServiceStatus patches the LoadBalancer ingress status with the allocated VIP.
func (c *Controller) writeServiceStatus(svc *corev1.Service, vip net.IP) error {
	svcCopy := svc.DeepCopy()
	svcCopy.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
		{IP: vip.String()},
	}
	_, err := c.client.CoreV1().Services(svc.Namespace).UpdateStatus(
		context.TODO(), svcCopy, metav1.UpdateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to update status for service %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	klog.V(2).InfoS("UDN VIP: allocated VIP to service",
		"service", svc.Namespace+"/"+svc.Name,
		"vip", vip,
		"class", *svc.Spec.LoadBalancerClass)
	return nil
}

// nextIP returns the next IP address after the given one.
func nextIP(ip net.IP) net.IP {
	ip = cloneIP(ip.To4())
	if ip == nil {
		return nil
	}
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			break
		}
	}
	return ip
}

func isBroadcast(ip net.IP, subnet *net.IPNet) bool {
	ones, bits := subnet.Mask.Size()
	if ones == bits {
		return false // /32 — single host
	}
	// broadcast = network address | ~mask
	broadcast := make(net.IP, len(ip))
	for i := range ip {
		broadcast[i] = subnet.IP[i] | ^subnet.Mask[i]
	}
	return ip.Equal(broadcast)
}

func cloneIP(ip net.IP) net.IP {
	clone := make(net.IP, len(ip))
	copy(clone, ip)
	return clone
}

// LoadBalancerClassForService returns the loadBalancerClass value, or empty string if unset.
func LoadBalancerClassForService(svc *corev1.Service) string {
	if svc.Spec.LoadBalancerClass == nil {
		return ""
	}
	return *svc.Spec.LoadBalancerClass
}

// IsUDNLoadBalancer returns true if the service uses the UDN LoadBalancer class.
func IsUDNLoadBalancer(svc *corev1.Service) bool {
	return LoadBalancerClassForService(svc) == types.UDNLoadBalancerClass
}

// IsUDNVIPService returns true if the service has any UDN VIP loadBalancerClass.
func IsUDNVIPService(svc *corev1.Service) bool {
	return udnVIPClasses.Has(LoadBalancerClassForService(svc))
}
