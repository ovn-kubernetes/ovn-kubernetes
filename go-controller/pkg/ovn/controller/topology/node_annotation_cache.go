package topology

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"sync"

	corev1 "k8s.io/api/core/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	nodeSubnetsAnnotation                = "k8s.ovn.org/node-subnets"
	udnLayer2GRLRPTunnelIDsAnnotationKey = "k8s.ovn.org/udn-layer2-node-gateway-router-lrp-tunnel-ids"
)

// NodeAnnotationCache stores parsed node annotation values keyed by node and
// raw annotation value.
type NodeAnnotationCache struct {
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

// NewNodeAnnotationCache creates an empty per-node annotation parse cache.
func NewNodeAnnotationCache() *NodeAnnotationCache {
	return &NodeAnnotationCache{nodes: map[string]*nodeAnnotationEntry{}}
}

// newNodeAnnotationCache is kept for local tests/constructors within this package.
func newNodeAnnotationCache() *NodeAnnotationCache {
	return NewNodeAnnotationCache()
}

// BuildNodeAnnotationState builds a parse-once view of node annotations backed
// by this cache.
func (c *NodeAnnotationCache) BuildNodeAnnotationState(node *corev1.Node) *NodeAnnotationState {
	if node == nil {
		return nil
	}
	networkIDs, networkIDsErr := c.getOrParseNetworkMap(node, util.OvnNetworkIDs)
	tunnelIDs, tunnelIDsErr := c.getOrParseNetworkMap(node, udnLayer2GRLRPTunnelIDsAnnotationKey)
	subnets, subnetsErr := c.getOrParseSubnetMap(node, nodeSubnetsAnnotation)
	return newNodeAnnotationState(node.Name, networkIDs, networkIDsErr, tunnelIDs, tunnelIDsErr, subnets, subnetsErr)
}

// ParseUDNLayer2NodeGRLRPTunnelID returns the per-network tunnel ID from the
// node annotation map using this cache.
func (c *NodeAnnotationCache) ParseUDNLayer2NodeGRLRPTunnelID(node *corev1.Node, netName string) (int, error) {
	tunnelIDs, err := c.getOrParseNetworkMap(node, udnLayer2GRLRPTunnelIDsAnnotationKey)
	if err != nil {
		return types.InvalidID, err
	}
	tunnelID, ok := tunnelIDs[netName]
	if !ok {
		return types.InvalidID, util.NewAnnotationNotSetError("node %q has no %q annotation for network %s", node.Name, udnLayer2GRLRPTunnelIDsAnnotationKey, netName)
	}
	return strconv.Atoi(tunnelID)
}

// NodeSubnetAnnotationChangedForNetwork reports whether the per-network subnet
// slice changed between oldNode and newNode, using this cache.
func (c *NodeAnnotationCache) NodeSubnetAnnotationChangedForNetwork(oldNode, newNode *corev1.Node, netName string) bool {
	oldState := c.BuildNodeAnnotationState(oldNode)
	newState := c.BuildNodeAnnotationState(newNode)
	if oldState == nil || newState == nil {
		return false
	}
	oldSubnets, oldErr := oldState.Subnets(netName)
	if oldErr != nil {
		if !util.IsAnnotationNotSetError(oldErr) {
			return false
		}
		oldSubnets = nil
	}
	newSubnets, newErr := newState.Subnets(netName)
	if newErr != nil {
		if !util.IsAnnotationNotSetError(newErr) {
			return false
		}
		newSubnets = nil
	}
	return !equalIPNetSlices(oldSubnets, newSubnets)
}

func (c *NodeAnnotationCache) getOrParseNetworkMap(node *corev1.Node, annotationName string) (map[string]string, error) {
	annotation, ok := node.Annotations[annotationName]
	if !ok {
		return nil, util.NewAnnotationNotSetError("could not find %q annotation", annotationName)
	}
	if cached, ok := c.GetNetworkMap(node.Name, annotationName, annotation); ok {
		return cached, nil
	}
	parsed, err := parseNetworkMapValue(annotationName, annotation)
	if err != nil {
		return nil, err
	}
	c.SetNetworkMap(node.Name, annotationName, annotation, parsed)
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

func (c *NodeAnnotationCache) getOrParseSubnetMap(node *corev1.Node, annotationName string) (map[string][]*net.IPNet, error) {
	annotation, ok := node.Annotations[annotationName]
	if !ok {
		return nil, util.NewAnnotationNotSetError("could not find %q annotation", annotationName)
	}
	if cached, ok := c.GetSubnetMap(node.Name, annotationName, annotation); ok {
		return cached, nil
	}
	parsed, err := parseSubnetMapValue(annotationName, annotation)
	if err != nil {
		return nil, err
	}
	c.SetSubnetMap(node.Name, annotationName, annotation, parsed)
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

// GetNetworkMap returns a cached parsed network annotation for a node.
// annotationName is the annotation key on the node object. raw is the exact
// string value read from that annotation before parsing.
// A hit requires both annotationName and raw to match.
// The returned map is the cached reference and is not deep-copied.
func (c *NodeAnnotationCache) GetNetworkMap(nodeName, annotationName, raw string) (map[string]string, bool) {
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
func (c *NodeAnnotationCache) SetNetworkMap(nodeName, annotationName, raw string, parsed map[string]string) {
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
func (c *NodeAnnotationCache) GetSubnetMap(nodeName, annotationName, raw string) (map[string][]*net.IPNet, bool) {
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
func (c *NodeAnnotationCache) SetSubnetMap(nodeName, annotationName, raw string, parsed map[string][]*net.IPNet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.ensureEntryLocked(nodeName)
	entry.subnetMaps[annotationName] = cachedSubnetMap{raw: raw, parsed: parsed}
}

// DeleteNode removes all cached annotation parse results for the node.
func (c *NodeAnnotationCache) DeleteNode(nodeName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.nodes, nodeName)
}

// ensureEntryLocked returns the cache entry for nodeName, creating one if
// needed. Caller must hold c.mu for writing.
func (c *NodeAnnotationCache) ensureEntryLocked(nodeName string) *nodeAnnotationEntry {
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
