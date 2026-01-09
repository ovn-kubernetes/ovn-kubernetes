package topology

import (
	"net"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type nodeAnnotationCache struct {
	mu    sync.RWMutex
	nodes map[string]*nodeAnnotationEntry
}

type nodeAnnotationEntry struct {
	networkMaps map[string]cachedNetworkMap
	subnetMaps  map[string]cachedSubnetMap
}

type cachedNetworkMap struct {
	raw    string
	parsed map[string]string
}

type cachedSubnetMap struct {
	raw    string
	parsed map[string][]*net.IPNet
}

var _ util.NodeAnnotationCache = (*nodeAnnotationCache)(nil)

func newNodeAnnotationCache() *nodeAnnotationCache {
	return &nodeAnnotationCache{nodes: map[string]*nodeAnnotationEntry{}}
}

func (c *nodeAnnotationCache) GetNetworkMap(nodeName, annotationName, raw string) (map[string]string, bool) {
	c.mu.RLock()
	entry := c.nodes[nodeName]
	if entry == nil {
		c.mu.RUnlock()
		return nil, false
	}
	cached, ok := entry.networkMaps[annotationName]
	c.mu.RUnlock()
	if !ok || cached.raw != raw {
		return nil, false
	}
	return cached.parsed, true
}

func (c *nodeAnnotationCache) SetNetworkMap(nodeName, annotationName, raw string, parsed map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.ensureEntryLocked(nodeName)
	entry.networkMaps[annotationName] = cachedNetworkMap{raw: raw, parsed: parsed}
}

func (c *nodeAnnotationCache) GetSubnetMap(nodeName, annotationName, raw string) (map[string][]*net.IPNet, bool) {
	c.mu.RLock()
	entry := c.nodes[nodeName]
	if entry == nil {
		c.mu.RUnlock()
		return nil, false
	}
	cached, ok := entry.subnetMaps[annotationName]
	c.mu.RUnlock()
	if !ok || cached.raw != raw {
		return nil, false
	}
	return cached.parsed, true
}

func (c *nodeAnnotationCache) SetSubnetMap(nodeName, annotationName, raw string, parsed map[string][]*net.IPNet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.ensureEntryLocked(nodeName)
	entry.subnetMaps[annotationName] = cachedSubnetMap{raw: raw, parsed: parsed}
}

func (c *nodeAnnotationCache) DeleteNode(nodeName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.nodes, nodeName)
}

func (c *nodeAnnotationCache) ensureEntryLocked(nodeName string) *nodeAnnotationEntry {
	entry := c.nodes[nodeName]
	if entry == nil {
		entry = &nodeAnnotationEntry{
			networkMaps: map[string]cachedNetworkMap{},
			subnetMaps:  map[string]cachedSubnetMap{},
		}
		c.nodes[nodeName] = entry
	}
	return entry
}
