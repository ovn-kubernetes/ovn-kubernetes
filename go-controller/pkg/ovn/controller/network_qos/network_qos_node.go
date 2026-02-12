package networkqos

import (
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func (c *Controller) processNextNQOSNodeWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	nqosNodeKey, quit := c.nqosNodeQueue.Get()
	if quit {
		return false
	}
	defer c.nqosNodeQueue.Done(nqosNodeKey)
	err := c.syncNetworkQoSNode(nqosNodeKey)
	if err == nil {
		c.nqosNodeQueue.Forget(nqosNodeKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with: %v", nqosNodeKey, err))

	if c.nqosNodeQueue.NumRequeues(nqosNodeKey) < maxRetries {
		c.nqosNodeQueue.AddRateLimited(nqosNodeKey)
		return true
	}

	c.nqosNodeQueue.Forget(nqosNodeKey)
	return true
}

// syncNetworkQoSNode triggers resync of all the NetworkQoSes when a node moves in/out of local zone
func (c *Controller) syncNetworkQoSNode(key string) error {
	startTime := time.Now()
	_, nodeName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(5).Infof("Processing sync for Node %s in Network QoS controller", nodeName)

	defer func() {
		klog.V(5).Infof("Finished syncing Node %s Network QoS controller: took %v", nodeName, time.Since(startTime))
	}()
	// node moves in/out of local zone, resync all the NetworkQoSes
	for _, nqosName := range c.nqosCache.GetKeys() {
		ns, name, _ := cache.SplitMetaNamespaceKey(nqosName)
		if nqos, err := c.nqosLister.NetworkQoSes(ns).Get(name); err != nil {
			klog.Errorf("Failed to get NetworkQoS %s: %v", nqosName, err)
		} else if nqos != nil {
			c.nqosQueue.Add(joinMetaNamespaceAndName(nqos.Namespace, nqos.Name))
		}
	}
	return nil
}

// isNodeInLocalZone returns whether the provided node is in a zone local to the zone controller
func (c *Controller) isNodeInLocalZone(node *corev1.Node) bool {
	return util.GetNodeZone(node) == c.zone
}
