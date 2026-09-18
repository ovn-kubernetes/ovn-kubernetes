// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

import (
	"net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// ClusterDefaultNetworkGatewayReconciler refreshes IPv4 gateway neighbors for
// bridge-binding live-migratable VMs on the cluster default network.
type ClusterDefaultNetworkGatewayReconciler struct {
	interfaceName string
	broadcastGARP func(string, util.GARP) error
}

// NewClusterDefaultNetworkGatewayReconciler creates a reconciler that sends
// gateway announcements through the given management interface.
func NewClusterDefaultNetworkGatewayReconciler(interfaceName string) *ClusterDefaultNetworkGatewayReconciler {
	return &ClusterDefaultNetworkGatewayReconciler{
		interfaceName: interfaceName,
		broadcastGARP: util.BroadcastGARP,
	}
}

// ReconcileIPv4AfterLiveMigration advertises the ARP proxy MAC for the ready
// migration target's IPv4 gateways, including on return to the subnet owner.
func (r *ClusterDefaultNetworkGatewayReconciler) ReconcileIPv4AfterLiveMigration(status *LiveMigrationStatus) error {
	if !status.IsTargetDomainReady() {
		return nil
	}
	targetPod := status.TargetPod
	if !IsPodLiveMigratable(targetPod) || util.PodWantsHostNetwork(targetPod) {
		return nil
	}
	podAnnotation, err := util.UnmarshalPodAnnotation(targetPod.Annotations, types.DefaultNetworkName)
	if err != nil {
		return err
	}
	if podAnnotation.Role == types.NetworkRoleInfrastructure {
		return nil
	}
	// The proxy MAC works on every node, including the subnet owner.
	// If ARP later restores the owner's LRP MAC, the next migration
	// refreshes the mapping again.
	gwMAC, err := net.ParseMAC(ARPProxyMAC)
	if err != nil {
		return err
	}
	var gateways []ipv4Gateway
	for _, gateway := range podAnnotation.Gateways {
		if gateway.To4() != nil {
			gateways = append(gateways, ipv4Gateway{ip: gateway, mac: gwMAC})
		}
	}
	return sendGatewayGARPs(r.interfaceName, gateways, r.broadcastGARP)
}
