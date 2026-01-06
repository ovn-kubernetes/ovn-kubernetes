package util

import (
	"net"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// NodeAnnotationState holds parsed node annotation data for a single reconcile.
type NodeAnnotationState struct {
	nodeName string

	networkIDs    map[string]string
	networkIDsErr error

	tunnelIDs    map[string]string
	tunnelIDsErr error

	subnets    map[string][]*net.IPNet
	subnetsErr error
}

// BuildNodeAnnotationState parses node annotations once and caches results per node.
func BuildNodeAnnotationState(node *corev1.Node, cache NodeAnnotationCache) *NodeAnnotationState {
	if node == nil {
		return nil
	}
	state := &NodeAnnotationState{nodeName: node.Name}
	state.networkIDs, state.networkIDsErr = parseNetworkMapAnnotationWithCacheNoCopy(node, OvnNetworkIDs, cache)
	state.tunnelIDs, state.tunnelIDsErr = parseNetworkMapAnnotationWithCacheNoCopy(node, ovnUDNLayer2NodeGRLRPTunnelIDs, cache)
	state.subnets, state.subnetsErr = parseSubnetAnnotationWithCacheNoCopy(node, ovnNodeSubnets, cache)
	return state
}

// NetworkID returns the network ID for a given network name.
func (s *NodeAnnotationState) NetworkID(netName string) (int, error) {
	if s == nil {
		return types.InvalidID, newAnnotationNotSetError("node %q has no %q annotation for network %s", "", OvnNetworkIDs, netName)
	}
	if s.networkIDsErr != nil {
		return types.InvalidID, s.networkIDsErr
	}
	networkID, ok := s.networkIDs[netName]
	if !ok {
		return types.InvalidID, newAnnotationNotSetError("node %q has no %q annotation for network %s", s.nodeName, OvnNetworkIDs, netName)
	}
	return strconv.Atoi(networkID)
}

// TunnelID returns the UDN L2 tunnel ID for a given network name.
func (s *NodeAnnotationState) TunnelID(netName string) (int, error) {
	if s == nil {
		return types.InvalidID, newAnnotationNotSetError("node %q has no %q annotation for network %s", "", ovnUDNLayer2NodeGRLRPTunnelIDs, netName)
	}
	if s.tunnelIDsErr != nil {
		return types.InvalidID, s.tunnelIDsErr
	}
	tunnelID, ok := s.tunnelIDs[netName]
	if !ok {
		return types.InvalidID, newAnnotationNotSetError("node %q has no %q annotation for network %s", s.nodeName, ovnUDNLayer2NodeGRLRPTunnelIDs, netName)
	}
	return strconv.Atoi(tunnelID)
}
