// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"net"
	"reflect"

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

	l3GatewayConfigs    map[string]*util.L3GatewayConfig
	l3GatewayConfigsErr error

	subnets    map[string][]*net.IPNet
	subnetsErr error
}

func newNodeAnnotationState(
	nodeName string,
	networkIDs map[string]string,
	networkIDsErr error,
	l3GatewayConfigs map[string]*util.L3GatewayConfig,
	l3GatewayConfigsErr error,
	subnets map[string][]*net.IPNet,
	subnetsErr error,
) *NodeAnnotationState {
	return &NodeAnnotationState{
		nodeName:            nodeName,
		networkIDs:          networkIDs,
		networkIDsErr:       networkIDsErr,
		l3GatewayConfigs:    l3GatewayConfigs,
		l3GatewayConfigsErr: l3GatewayConfigsErr,
		subnets:             subnets,
		subnetsErr:          subnetsErr,
	}
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

// GatewayAnnotationChangedForNetworkWithState returns true if the node's
// per-network gateway annotation changed using pre-parsed annotation state.
func GatewayAnnotationChangedForNetworkWithState(oldState, newState *NodeAnnotationState, netName string) bool {
	if oldState == nil || newState == nil {
		return false
	}
	if oldState.l3GatewayConfigsErr != nil {
		if !util.IsAnnotationNotSetError(oldState.l3GatewayConfigsErr) {
			klog.Errorf("Failed to parse old node %s gateway annotation: %v", oldState.nodeName, oldState.l3GatewayConfigsErr)
		}
		return false
	}
	if newState.l3GatewayConfigsErr != nil {
		if !util.IsAnnotationNotSetError(newState.l3GatewayConfigsErr) {
			klog.Errorf("Failed to parse new node %s gateway annotation: %v", newState.nodeName, newState.l3GatewayConfigsErr)
		}
		return false
	}
	return !reflect.DeepEqual(oldState.l3GatewayConfigs[netName], newState.l3GatewayConfigs[netName])
}
