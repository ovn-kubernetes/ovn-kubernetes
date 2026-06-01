// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func (oc *BaseNetworkController) uplinkGatewayConfig(node *corev1.Node) (*util.L3GatewayConfig, bool, error) {
	uplinkName := oc.Uplink()
	if uplinkName == "" {
		return nil, false, nil
	}

	stateName := uplinkutil.StateName(uplinkName, node.Name)
	state, err := oc.watchFactory.UplinkStateInformer().Lister().Get(stateName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, true, fmt.Errorf("waiting for UplinkState %s for uplink %q on node %q",
				stateName, uplinkName, node.Name)
		}
		return nil, true, fmt.Errorf("failed to get UplinkState %s: %w", stateName, err)
	}
	if err := uplinkutil.ValidateStateIdentity(state, stateName, uplinkName, node.Name); err != nil {
		return nil, true, err
	}

	chassisID, err := util.ParseNodeChassisIDAnnotation(node)
	if err != nil {
		return nil, true, fmt.Errorf("failed to get chassis ID for node %q: %w", node.Name, err)
	}
	if err := uplinkutil.ValidateOVSBridgeState(state, uplinkName, node.Name, true); err != nil {
		return nil, true, err
	}
	l3GatewayConfig, err := resolvedUplinkL3GatewayConfig(state, node.Name, chassisID)
	return l3GatewayConfig, true, err
}

func resolvedUplinkL3GatewayConfig(
	state *uplinkv1alpha1.UplinkState,
	nodeName, chassisID string,
) (*util.L3GatewayConfig, error) {
	macAddress, err := net.ParseMAC(string(state.Status.MACAddress))
	if err != nil {
		return nil, fmt.Errorf("failed to parse UplinkState MAC address %q: %w", state.Status.MACAddress, err)
	}

	ipAddresses := make([]*net.IPNet, 0, len(state.Status.IPAddresses))
	for _, ipAddress := range state.Status.IPAddresses {
		ip, cidr, err := net.ParseCIDR(string(ipAddress))
		if err != nil {
			return nil, fmt.Errorf("failed to parse UplinkState IP address %q: %w", ipAddress, err)
		}
		cidr.IP = ip
		ipAddresses = append(ipAddresses, cidr)
	}
	if len(ipAddresses) == 0 {
		return nil, fmt.Errorf("UplinkState has no gateway IP addresses")
	}

	defaultGateways := make([]net.IP, 0, len(state.Status.DefaultGateways))
	for _, defaultGateway := range state.Status.DefaultGateways {
		ip := net.ParseIP(string(defaultGateway))
		if ip == nil {
			return nil, fmt.Errorf("failed to parse UplinkState default gateway %q", defaultGateway)
		}
		defaultGateways = append(defaultGateways, ip)
	}

	bridgeName := state.Status.OVSBridge.Name
	return &util.L3GatewayConfig{
		Mode:           config.Gateway.Mode,
		ChassisID:      chassisID,
		BridgeID:       bridgeName,
		InterfaceID:    bridgeName + "_" + nodeName,
		MACAddress:     macAddress,
		IPAddresses:    ipAddresses,
		NextHops:       defaultGateways,
		NodePortEnable: config.Gateway.NodeportEnable,
	}, nil
}
