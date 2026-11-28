// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

import (
	"net"
	"net/netip"
	"slices"

	corev1 "k8s.io/api/core/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// DefaultNetworkNeighborAnnouncer refreshes IPv4 neighbors for
// bridge-binding live-migratable VMs on the cluster default network.
type DefaultNetworkNeighborAnnouncer struct {
	interfaceName string
	broadcastGARP func(string, util.GARP) error
}

// NewDefaultNetworkNeighborAnnouncer creates a reconciler that sends
// announcements through the given management interface.
func NewDefaultNetworkNeighborAnnouncer(interfaceName string) *DefaultNetworkNeighborAnnouncer {
	return &DefaultNetworkNeighborAnnouncer{
		interfaceName: interfaceName,
		broadcastGARP: util.BroadcastGARP,
	}
}

// AnnounceIPv4AfterLiveMigration refreshes the IPv4 neighbors that can be
// reached from the target switch. Only an on-link peer can have a direct
// entry in the guest's cache, and its MAC depends on where that peer runs.
func (r *DefaultNetworkNeighborAnnouncer) AnnounceIPv4AfterLiveMigration(status *LiveMigrationStatus, peers []*corev1.Pod) error {
	if !status.IsTargetDomainReady() {
		return nil
	}
	targetPod := status.TargetPod
	if !IsPodLiveMigratable(targetPod) || util.PodWantsHostNetwork(targetPod) || util.PodCompleted(targetPod) {
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
	proxyMAC, err := net.ParseMAC(ARPProxyMAC)
	if err != nil {
		return err
	}
	var neighbors []ipv4Neighbor
	for _, gateway := range podAnnotation.Gateways {
		if gateway.To4() != nil {
			neighbors = append(neighbors, ipv4Neighbor{ip: gateway, mac: proxyMAC})
		}
	}
	neighbors = append(neighbors, migrationTargetPeerNeighbors(targetPod, podAnnotation, peers, proxyMAC)...)
	// Peers on this switch may still have the proxy MAC from when the VM
	// was remote. Advertise the directly reachable VM MAC on this switch.
	neighbors = append(neighbors, ipv4NeighborsForPod(podAnnotation, podAnnotation.MAC)...)
	return sendNeighborGARPs(r.interfaceName, neighbors, r.broadcastGARP)
}

func ipv4NeighborsForPod(podAnnotation *util.PodAnnotation, mac net.HardwareAddr) []ipv4Neighbor {
	var neighbors []ipv4Neighbor
	for _, addr := range podAnnotation.IPs {
		if addr.IP.To4() != nil {
			neighbors = append(neighbors, ipv4Neighbor{ip: addr.IP, mac: mac})
		}
	}
	return neighbors
}

func migrationTargetPeerNeighbors(target *corev1.Pod, annotation *util.PodAnnotation, peers []*corev1.Pod, proxyMAC net.HardwareAddr) []ipv4Neighbor {
	neighbors := make(map[netip.Addr]ipv4Neighbor)
	for _, peer := range peers {
		if !util.PodScheduled(peer) || util.PodWantsHostNetwork(peer) || util.PodCompleted(peer) {
			continue
		}
		peerAnnotation, err := util.UnmarshalPodAnnotation(peer.Annotations, types.DefaultNetworkName)
		if err != nil {
			// A pod without a network annotation has no assigned OVN addresses.
			continue
		}
		for _, addr := range peerAnnotation.IPs {
			ip, ok := netip.AddrFromSlice(addr.IP.To4())
			if !ok || !slices.ContainsFunc(annotation.IPs, func(subnet *net.IPNet) bool {
				return subnet.Contains(addr.IP)
			}) || slices.ContainsFunc(annotation.IPs, func(subnet *net.IPNet) bool {
				return subnet.IP.Equal(addr.IP)
			}) {
				continue
			}
			// A migrating peer can have two launcher pods with the same IP.
			// The local logical switch port takes precedence over proxy ARP.
			if peer.Spec.NodeName == target.Spec.NodeName {
				neighbors[ip] = ipv4Neighbor{ip: addr.IP, mac: peerAnnotation.MAC}
			} else if _, exists := neighbors[ip]; !exists {
				neighbors[ip] = ipv4Neighbor{ip: addr.IP, mac: proxyMAC}
			}
		}
	}
	result := make([]ipv4Neighbor, 0, len(neighbors))
	for _, neighbor := range neighbors {
		result = append(result, neighbor)
	}
	return result
}
