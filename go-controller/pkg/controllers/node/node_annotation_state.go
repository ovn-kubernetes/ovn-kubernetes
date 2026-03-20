package node

import (
	"net"
	"strconv"

	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// NodeAnnotationState holds parsed node annotation data.
// Used during reconciliation to compare old and new node annotations quickly
// with already parsed data.
type NodeAnnotationState struct {
	nodeName string

	networkIDs    map[string]string
	networkIDsErr error

	tunnelIDs    map[string]string
	tunnelIDsErr error

	subnets    map[string][]*net.IPNet
	subnetsErr error
}

func newNodeAnnotationState(
	nodeName string,
	networkIDs map[string]string,
	networkIDsErr error,
	tunnelIDs map[string]string,
	tunnelIDsErr error,
	subnets map[string][]*net.IPNet,
	subnetsErr error,
) *NodeAnnotationState {
	return &NodeAnnotationState{
		nodeName:      nodeName,
		networkIDs:    networkIDs,
		networkIDsErr: networkIDsErr,
		tunnelIDs:     tunnelIDs,
		tunnelIDsErr:  tunnelIDsErr,
		subnets:       subnets,
		subnetsErr:    subnetsErr,
	}
}

// TunnelID returns the UDN L2 tunnel ID for a given network name.
func (s *NodeAnnotationState) TunnelID(netName string) (int, error) {
	if s.tunnelIDsErr != nil {
		return types.InvalidID, s.tunnelIDsErr
	}
	tunnelID, ok := s.tunnelIDs[netName]
	if !ok {
		return types.InvalidID, util.NewAnnotationNotSetError("node %q has no %q annotation for network %s", s.nodeName, types.UDNLayer2NodeGRLRPTunnelIDAnnotation, netName)
	}
	return strconv.Atoi(tunnelID)
}

// Subnets returns the parsed node subnets for the given network name.
func (s *NodeAnnotationState) Subnets(netName string) ([]*net.IPNet, error) {
	if s.subnetsErr != nil {
		return nil, s.subnetsErr
	}
	subnets, ok := s.subnets[netName]
	if !ok {
		return nil, util.NewAnnotationNotSetError("node %q has no %q annotation for network %s", s.nodeName, types.NodeSubnetsAnnotation, netName)
	}
	return util.CopyIPNets(subnets), nil
}

// NodeSubnetAnnotationChangedForNetworkWithState returns true if the node subnet
// annotation for the given network changed using pre-parsed annotation state.
func NodeSubnetAnnotationChangedForNetworkWithState(oldState, newState *NodeAnnotationState, netName string) bool {
	if oldState == nil || newState == nil {
		return false
	}
	if oldState.subnetsErr != nil {
		if !util.IsAnnotationNotSetError(oldState.subnetsErr) {
			klog.Errorf("Failed to parse old node %s annotation: %v", oldState.nodeName, oldState.subnetsErr)
		}
		return false
	}
	if newState.subnetsErr != nil {
		if !util.IsAnnotationNotSetError(newState.subnetsErr) {
			klog.Errorf("Failed to parse new node %s annotation: %v", newState.nodeName, newState.subnetsErr)
		}
		return false
	}
	return !equalIPNetSlices(oldState.subnets[netName], newState.subnets[netName])
}

// TunnelIDAnnotationChangedForNetworkWithState returns true if the node tunnel
// ID annotation for the given network changed using pre-parsed annotation
// state.
func TunnelIDAnnotationChangedForNetworkWithState(oldState, newState *NodeAnnotationState, netName string) bool {
	if oldState == nil || newState == nil {
		return false
	}
	if oldState.tunnelIDsErr != nil {
		if !util.IsAnnotationNotSetError(oldState.tunnelIDsErr) {
			klog.Errorf("Failed to parse old node %s tunnel ID annotation: %v", oldState.nodeName, oldState.tunnelIDsErr)
		}
		return false
	}
	if newState.tunnelIDsErr != nil {
		if !util.IsAnnotationNotSetError(newState.tunnelIDsErr) {
			klog.Errorf("Failed to parse new node %s tunnel ID annotation: %v", newState.nodeName, newState.tunnelIDsErr)
		}
		return false
	}
	return oldState.tunnelIDs[netName] != newState.tunnelIDs[netName]
}
