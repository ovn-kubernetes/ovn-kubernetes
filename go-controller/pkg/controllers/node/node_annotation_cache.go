package node

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// NodeAnnotationCache stores parsed node annotation values keyed by node and
// raw annotation value.
type NodeAnnotationCache struct {
	nodes *syncmap.SyncMap[*nodeAnnotationEntry]
}

type nodeAnnotationEntry struct {
	networkMaps map[string]cachedNetworkMap
	subnetMaps  map[string]cachedSubnetMap
}

type cachedNetworkMap struct {
	// raw is used for fast comparisons within the cache
	raw string
	// parsed holds the parsed annotation of network names -> string values
	parsed map[string]string
}

type cachedSubnetMap struct {
	// raw is used for fast comparisons within the cache
	raw string
	// parsed holds the parsed annotation of network names -> subnets
	parsed map[string][]*net.IPNet
}

// NewNodeAnnotationCache creates an empty per-node annotation parse cache.
func NewNodeAnnotationCache() *NodeAnnotationCache {
	return &NodeAnnotationCache{nodes: syncmap.NewSyncMap[*nodeAnnotationEntry]()}
}

// UpdateNodeAnnotationState builds a parse-once view of node annotations.
// When updateCache is true, parsed values refresh the per-node cache; callers
// comparing an older node snapshot against a newer one should pass false for
// the old snapshot so it cannot replace the latest cached value.
func (c *NodeAnnotationCache) UpdateNodeAnnotationState(node *corev1.Node, updateCache bool) *NodeAnnotationState {
	return c.updateNodeAnnotationState(node, updateCache)
}

// updateNodeAnnotationState builds a parse-once view of node annotations. When
// updateCache is true, parsed values refresh the per-node cache; callers
// comparing an older node snapshot against a newer one should pass false for
// the old snapshot so it cannot replace the latest cached value.
func (c *NodeAnnotationCache) updateNodeAnnotationState(node *corev1.Node, updateCache bool) *NodeAnnotationState {
	if node == nil {
		return nil
	}
	networkIDs, networkIDsErr := c.parseNetworkMapCached(node, util.OvnNetworkIDs, updateCache)
	tunnelIDs, tunnelIDsErr := c.parseNetworkMapCached(node, types.UDNLayer2NodeGRLRPTunnelIDAnnotation, updateCache)
	subnets, subnetsErr := c.parseSubnetMapCached(node, types.NodeSubnetsAnnotation, updateCache)
	return newNodeAnnotationState(node.Name, networkIDs, networkIDsErr, tunnelIDs, tunnelIDsErr, subnets, subnetsErr)
}

// ParseUDNLayer2NodeGRLRPTunnelIDCached returns the per-network tunnel ID from the
// node annotation map using this cache.
func (c *NodeAnnotationCache) ParseUDNLayer2NodeGRLRPTunnelIDCached(node *corev1.Node, netName string) (int, error) {
	tunnelIDs, err := c.parseNetworkMapCached(node, types.UDNLayer2NodeGRLRPTunnelIDAnnotation, true)
	if err != nil {
		return types.InvalidID, err
	}
	tunnelID, ok := tunnelIDs[netName]
	if !ok {
		return types.InvalidID, util.NewAnnotationNotSetError("node %q has no %q annotation for network %s", node.Name, types.UDNLayer2NodeGRLRPTunnelIDAnnotation, netName)
	}
	id, err := strconv.Atoi(tunnelID)
	if err != nil {
		return types.InvalidID, err
	}
	return id, nil
}

// parseNetworkMapCached returns the parsed per-network string map for the
// given annotation, reusing a cached parse result when the raw annotation
// value has not changed. If there is a cache miss, the annotation is parsed;
// updateCache controls whether the parsed result replaces the cached value for
// this node and annotation.
func (c *NodeAnnotationCache) parseNetworkMapCached(node *corev1.Node, annotationName string, updateCache bool) (map[string]string, error) {
	annotation, ok := node.Annotations[annotationName]
	if !ok {
		return nil, util.NewAnnotationNotSetError("could not find %q annotation", annotationName)
	}
	// fast path - see if we already have this parsed annotation in cache
	if cached, ok := c.getParsedNetworkMap(node.Name, annotationName, annotation); ok {
		return cached, nil
	}
	// cache miss - parse and optionally update the cache
	parsed, err := parseNetworkMapValue(annotationName, annotation)
	if err != nil {
		return nil, err
	}
	if updateCache {
		c.setNetworkMap(node.Name, annotationName, annotation, parsed)
	}
	return parsed, nil
}

func parseNetworkMapValue(annotationName, annotation string) (map[string]string, error) {
	out := map[string]string{}
	if err := json.Unmarshal([]byte(annotation), &out); err != nil {
		return nil, fmt.Errorf("could not parse %q annotation %q : %v", annotationName, annotation, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("unexpected empty %s annotation", annotationName)
	}
	return out, nil
}

func (c *NodeAnnotationCache) parseSubnetMapCached(node *corev1.Node, annotationName string, updateCache bool) (map[string][]*net.IPNet, error) {
	annotation, ok := node.Annotations[annotationName]
	if !ok {
		return nil, util.NewAnnotationNotSetError("could not find %q annotation", annotationName)
	}
	if cached, ok := c.getParsedSubnetMap(node.Name, annotationName, annotation); ok {
		return cached, nil
	}
	parsed, err := parseSubnetMapValue(annotationName, annotation)
	if err != nil {
		return nil, err
	}
	if updateCache {
		c.setSubnetMap(node.Name, annotationName, annotation, parsed)
	}
	return parsed, nil
}

func parseSubnetMapValue(annotationName, annotation string) (map[string][]*net.IPNet, error) {
	subnetsStrMap := map[string][]string{}
	subnetsDual := map[string][]string{}
	if err := json.Unmarshal([]byte(annotation), &subnetsDual); err == nil {
		subnetsStrMap = subnetsDual
	} else {
		subnetsSingle := map[string]string{}
		if err := json.Unmarshal([]byte(annotation), &subnetsSingle); err != nil {
			return nil, fmt.Errorf("could not parse %q annotation %q as either single-stack or dual-stack: %v", annotationName, annotation, err)
		}
		for netName, subnet := range subnetsSingle {
			subnetsStrMap[netName] = []string{subnet}
		}
	}
	if len(subnetsStrMap) == 0 {
		return nil, fmt.Errorf("unexpected empty %s annotation", annotationName)
	}
	subnetMap := make(map[string][]*net.IPNet, len(subnetsStrMap))
	for netName, subnetStrs := range subnetsStrMap {
		if len(subnetStrs) == 0 {
			return nil, fmt.Errorf("unexpected empty %s annotation entry for network %s", annotationName, netName)
		}
		ipnets := make([]*net.IPNet, 0, len(subnetStrs))
		for _, subnet := range subnetStrs {
			_, ipnet, err := net.ParseCIDR(subnet)
			if err != nil {
				return nil, fmt.Errorf("error parsing %q value: %v", annotationName, err)
			}
			ipnets = append(ipnets, ipnet)
		}
		subnetMap[netName] = ipnets
	}
	return subnetMap, nil
}

func equalIPNetSlices(a, b []*net.IPNet) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		switch {
		case a[i] == nil && b[i] == nil:
			continue
		case a[i] == nil || b[i] == nil:
			return false
		case a[i].String() != b[i].String():
			return false
		}
	}
	return true
}

// getParsedNetworkMap returns a cached parsed network annotation for a node.
// annotationName is the annotation key on the node object. raw is the exact
// string value read from that annotation before parsing.
// A hit requires both annotationName and raw to match.
// The returned map is the cached reference and is not deep-copied.
func (c *NodeAnnotationCache) getParsedNetworkMap(nodeName, annotationName, raw string) (map[string]string, bool) {
	var parsed map[string]string
	var ok bool
	_ = c.nodes.DoWithLock(nodeName, func(key string) error {
		entry, loaded := c.nodes.Load(key)
		if !loaded || entry == nil {
			return nil
		}
		cached, found := entry.networkMaps[annotationName]
		if !found || cached.raw != raw {
			return nil
		}
		parsed = cached.parsed
		ok = true
		return nil
	})
	return parsed, ok
}

// setNetworkMap stores a parsed network annotation for a node keyed by
// annotationName and the exact raw annotation string from the node object.
// The parsed map is stored by reference and is not deep-copied.
func (c *NodeAnnotationCache) setNetworkMap(nodeName, annotationName, raw string, parsed map[string]string) {
	_ = c.nodes.DoWithLock(nodeName, func(key string) error {
		entry := c.ensureEntryLocked(key)
		entry.networkMaps[annotationName] = cachedNetworkMap{raw: raw, parsed: parsed}
		return nil
	})
}

// getParsedSubnetMap returns a cached parsed subnet annotation for a node.
// annotationName is the annotation key on the node object. raw is the exact
// string value read from that annotation before parsing.
// A hit requires both annotationName and raw to match.
// The returned map is the cached reference and is not deep-copied.
func (c *NodeAnnotationCache) getParsedSubnetMap(nodeName, annotationName, raw string) (map[string][]*net.IPNet, bool) {
	var parsed map[string][]*net.IPNet
	var ok bool
	_ = c.nodes.DoWithLock(nodeName, func(key string) error {
		entry, loaded := c.nodes.Load(key)
		if !loaded || entry == nil {
			return nil
		}
		cached, found := entry.subnetMaps[annotationName]
		if !found || cached.raw != raw {
			return nil
		}
		parsed = cached.parsed
		ok = true
		return nil
	})
	return parsed, ok
}

// setSubnetMap stores a parsed subnet annotation for a node keyed by
// annotationName and the exact raw annotation string from the node object.
// The parsed map is stored by reference and is not deep-copied.
func (c *NodeAnnotationCache) setSubnetMap(nodeName, annotationName, raw string, parsed map[string][]*net.IPNet) {
	_ = c.nodes.DoWithLock(nodeName, func(key string) error {
		entry := c.ensureEntryLocked(key)
		entry.subnetMaps[annotationName] = cachedSubnetMap{raw: raw, parsed: parsed}
		return nil
	})
}

// deleteNode removes all cached annotation parse results for the node.
func (c *NodeAnnotationCache) deleteNode(nodeName string) {
	_ = c.nodes.DoWithLock(nodeName, func(key string) error {
		c.nodes.Delete(key)
		return nil
	})
}

// DeleteNode removes all cached annotation parse results for the node.
func (c *NodeAnnotationCache) DeleteNode(nodeName string) {
	c.deleteNode(nodeName)
}

// ensureEntryLocked returns the cache entry for nodeName, creating one if
// needed. Caller must hold the per-node syncmap key lock.
func (c *NodeAnnotationCache) ensureEntryLocked(nodeName string) *nodeAnnotationEntry {
	entry, _ := c.nodes.LoadOrStore(nodeName, &nodeAnnotationEntry{
		networkMaps: map[string]cachedNetworkMap{},
		subnetMaps:  map[string]cachedSubnetMap{},
	})
	return entry
}
