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

// newNodeAnnotationCache creates an empty per-node annotation parse cache.
func newNodeAnnotationCache() *nodeAnnotationCache {
	return &nodeAnnotationCache{nodes: map[string]*nodeAnnotationEntry{}}
}

// GetNetworkMap returns a cached parsed network annotation for a node.
// annotationName is the annotation key on the node object. raw is the exact
// string value read from that annotation before parsing.
// A hit requires both annotationName and raw to match.
// The returned map is the cached reference and is not deep-copied.
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

// SetNetworkMap stores a parsed network annotation for a node keyed by
// annotationName and the exact raw annotation string from the node object.
// The parsed map is stored by reference and is not deep-copied.
func (c *nodeAnnotationCache) SetNetworkMap(nodeName, annotationName, raw string, parsed map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.ensureEntryLocked(nodeName)
	entry.networkMaps[annotationName] = cachedNetworkMap{raw: raw, parsed: parsed}
}

// GetSubnetMap returns a cached parsed subnet annotation for a node.
// annotationName is the annotation key on the node object. raw is the exact
// string value read from that annotation before parsing.
// A hit requires both annotationName and raw to match.
// The returned map is the cached reference and is not deep-copied.
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

// SetSubnetMap stores a parsed subnet annotation for a node keyed by
// annotationName and the exact raw annotation string from the node object.
// The parsed map is stored by reference and is not deep-copied.
func (c *nodeAnnotationCache) SetSubnetMap(nodeName, annotationName, raw string, parsed map[string][]*net.IPNet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.ensureEntryLocked(nodeName)
	entry.subnetMaps[annotationName] = cachedSubnetMap{raw: raw, parsed: parsed}
}

// DeleteNode removes all cached annotation parse results for the node.
func (c *nodeAnnotationCache) DeleteNode(nodeName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.nodes, nodeName)
}

// ensureEntryLocked returns the cache entry for nodeName, creating one if
// needed. Caller must hold c.mu for writing.
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
