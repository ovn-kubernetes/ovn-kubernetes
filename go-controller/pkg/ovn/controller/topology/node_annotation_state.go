package topology

import (
	"net"
	"strconv"

	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
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

// NetworkID returns the network ID for a given network name.
func (s *NodeAnnotationState) NetworkID(netName string) (int, error) {
	if s == nil {
		return types.InvalidID, util.NewAnnotationNotSetError("node %q has no %q annotation for network %s", "", util.OvnNetworkIDs, netName)
	}
	if s.networkIDsErr != nil {
		return types.InvalidID, s.networkIDsErr
	}
	networkID, ok := s.networkIDs[netName]
	if !ok {
		return types.InvalidID, util.NewAnnotationNotSetError("node %q has no %q annotation for network %s", s.nodeName, util.OvnNetworkIDs, netName)
	}
	return strconv.Atoi(networkID)
}

// TunnelID returns the UDN L2 tunnel ID for a given network name.
func (s *NodeAnnotationState) TunnelID(netName string) (int, error) {
	if s == nil {
		return types.InvalidID, util.NewAnnotationNotSetError("node %q has no %q annotation for network %s", "", udnLayer2GRLRPTunnelIDsAnnotationKey, netName)
	}
	if s.tunnelIDsErr != nil {
		return types.InvalidID, s.tunnelIDsErr
	}
	tunnelID, ok := s.tunnelIDs[netName]
	if !ok {
		return types.InvalidID, util.NewAnnotationNotSetError("node %q has no %q annotation for network %s", s.nodeName, udnLayer2GRLRPTunnelIDsAnnotationKey, netName)
	}
	return strconv.Atoi(tunnelID)
}

// Subnets returns the parsed node subnets for the given network name.
func (s *NodeAnnotationState) Subnets(netName string) ([]*net.IPNet, error) {
	if s == nil {
		return nil, util.NewAnnotationNotSetError("node %q has no %q annotation for network %s", "", nodeSubnetsAnnotation, netName)
	}
	if s.subnetsErr != nil {
		return nil, s.subnetsErr
	}
	subnets, ok := s.subnets[netName]
	if !ok {
		return nil, util.NewAnnotationNotSetError("node %q has no %q annotation for network %s", s.nodeName, nodeSubnetsAnnotation, netName)
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
