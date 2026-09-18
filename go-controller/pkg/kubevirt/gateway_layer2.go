// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

import (
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/ndp"
)

// Layer2GatewayReconciler reconciles IPv4 and IPv6 default gateways after
// live migration on layer2 primary user-defined networks.
type Layer2GatewayReconciler struct {
	watchFactory  *factory.WatchFactory
	netInfo       util.NetInfo
	interfaceName string
	broadcastGARP func(string, util.GARP) error
	// getNetworkNameForNADKey resolves NAD keys to network names for UDNs.
	getNetworkNameForNADKey func(nadKey string) string
}

// NewLayer2GatewayReconciler creates a new Layer2GatewayReconciler.
func NewLayer2GatewayReconciler(watchFactory *factory.WatchFactory, netInfo util.NetInfo, interfaceName string, getNetworkNameForNADKey func(nadKey string) string) *Layer2GatewayReconciler {
	return &Layer2GatewayReconciler{
		watchFactory:            watchFactory,
		netInfo:                 netInfo,
		interfaceName:           interfaceName,
		broadcastGARP:           util.BroadcastGARP,
		getNetworkNameForNADKey: getNetworkNameForNADKey,
	}
}

// ReconcileIPv4AfterLiveMigration sends a GARP after live migration to update
// the default gateway MAC address to the node where the VM is now running.
func (r *Layer2GatewayReconciler) ReconcileIPv4AfterLiveMigration(liveMigrationStatus *LiveMigrationStatus) error {
	if !liveMigrationStatus.IsTargetDomainReady() {
		return nil
	}
	gateways, err := r.ipv4Gateways(liveMigrationStatus.TargetPod)
	if err != nil {
		return err
	}
	return sendGatewayGARPs(r.interfaceName, gateways, r.broadcastGARP)
}

func (r *Layer2GatewayReconciler) ipv4Gateways(targetPod *corev1.Pod) ([]ipv4Gateway, error) {
	var gateways []ipv4Gateway
	var gwMAC net.HardwareAddr
	if !config.Layer2UsesTransitRouter {
		targetNode, err := r.watchFactory.GetNode(targetPod.Spec.NodeName)
		if err != nil {
			return nil, err
		}

		lrpJoinAddress, err := udn.GetGWRouterIPv4(targetNode, r.netInfo)
		if err != nil {
			return nil, err
		}

		gwMAC = util.IPAddrToHWAddr(lrpJoinAddress)
	}
	for _, subnet := range r.netInfo.Subnets() {
		gwIP := r.netInfo.GetNodeGatewayIP(subnet.CIDR).IP.To4()
		if gwIP == nil {
			continue
		}
		if config.Layer2UsesTransitRouter {
			gwMAC = util.IPAddrToHWAddr(gwIP)
		}
		gateways = append(gateways, ipv4Gateway{ip: gwIP, mac: gwMAC})
	}
	return gateways, nil
}

// ReconcileIPv6AfterLiveMigration updates the VM's IPv6 default gateway path:
//   - Remove the IPv6 default gateway path from the VM's node before live migration.
//   - Add the IPv6 default gateway path from the VM's node after live migration.
//
// This is done by sending a pair of unsolicited RAs: one with lifetime=0 to
// remove the gateway path and another with lifetime=max to add the new default
// gateway path.
func (r *Layer2GatewayReconciler) ReconcileIPv6AfterLiveMigration(liveMigration *LiveMigrationStatus) error {
	if !liveMigration.IsTargetDomainReady() {
		return nil
	}
	nodes, err := r.watchFactory.GetNodes()
	if err != nil {
		return err
	}

	targetPod := liveMigration.TargetPod
	nadKeys, err := util.PodNADKeys(targetPod, r.netInfo, r.getNetworkNameForNADKey)
	if err != nil {
		return err
	}
	if len(nadKeys) != 1 {
		return fmt.Errorf("expected only one NAD key for network %q, got %d", r.netInfo.GetNetworkName(), len(nadKeys))
	}

	targetPodAnnotation, err := util.UnmarshalPodAnnotation(targetPod.Annotations, nadKeys[0])
	if err != nil {
		return ovntypes.NewSuppressedError(fmt.Errorf("failed parsing ovn pod annotation for pod '%s/%s' and network %q: %w", targetPod.Namespace, targetPod.Name, r.netInfo.GetNetworkName(), err))
	}

	destinationIP, err := util.MatchFirstIPNetFamily(true /* ipv6 */, targetPodAnnotation.IPs)
	if err != nil {
		return err
	}
	destinationMAC := targetPodAnnotation.MAC

	ras := make([]ndp.RouterAdvertisement, 0, len(nodes))
	for _, node := range nodes {
		if !config.Layer2UsesTransitRouter && node.Name == liveMigration.TargetPod.Spec.NodeName {
			// skip the target node since this is the proper gateway
			continue
		}
		nodeJoinAddrs, err := udn.GetGWRouterIPs(node, r.netInfo)
		if err != nil {
			return ovntypes.NewSuppressedError(fmt.Errorf("failed parsing join addresss from node %q and network %q to reconcile ipv6 gateway: %w", node.Name, r.netInfo.GetNetworkName(), err))
		}
		// During upgrades, nftables blocks Router Advertisements (RAs) from other nodes.
		// However, Virtual Machines (VMs) may still retain old default gateway paths.
		// To address this, we create a new Router Advertisement with a lifetime of 0
		// to signal the removal of the old default gateway.
		// NOTE: This is a workaround for the issue and may not be needed in the future, after
		//       upgrading to a version that supports the new behavior.
		ras = append(ras, newRouterAdvertisementFromIPAndLifetime(nodeJoinAddrs[0].IP, destinationMAC, destinationIP.IP, 0))
	}
	if !config.Layer2UsesTransitRouter {
		targetNode, err := r.watchFactory.GetNode(liveMigration.TargetPod.Spec.NodeName)
		if err != nil {
			return fmt.Errorf("failed fetching node %q to reconcile ipv6 gateway: %w", liveMigration.TargetPod.Spec.NodeName, err)
		}
		targetNodeJoinAddrs, err := udn.GetGWRouterIPs(targetNode, r.netInfo)
		if err != nil {
			return ovntypes.NewSuppressedError(fmt.Errorf("failed parsing join addresss from live migration target node %q and network %q to reconcile ipv6 gateway: %w", targetNode.Name, r.netInfo.GetNetworkName(), err))
		}
		ras = append(ras, newRouterAdvertisementFromIPAndLifetime(targetNodeJoinAddrs[0].IP, destinationMAC, destinationIP.IP, 65535))
	} else {
		if len(targetPodAnnotation.Gateways) == 0 {
			return fmt.Errorf("missing gateways to calculate ipv6 gateway reconciler RA")
		}
		// The LRP mac is calculated from the first address on the list.
		gwIP := targetPodAnnotation.Gateways[0]

		// Create Prefix Information Option with IPv6 join subnet
		prefixNet := r.netInfo.JoinSubnetV6()
		if prefixNet == nil {
			return fmt.Errorf("no IPv6 join subnet available for network %q", r.netInfo.GetNetworkName())
		}

		prefixInfo := ndp.PrefixInformation{
			Prefix:            *prefixNet,
			ValidLifetime:     0,
			PreferredLifetime: 0, // IP lifetime 0 as requested
			OnLink:            true,
			Autonomous:        true,
		}

		ras = append(ras, newRouterAdvertisementWithPrefixInfos(gwIP, destinationMAC, destinationIP.IP, 65535, []ndp.PrefixInformation{prefixInfo}))
	}

	return ndp.SendRouterAdvertisements(r.interfaceName, ras...)
}

// newRouterAdvertisementFromIPAndLifetime creates a new Router Advertisement (RA) message
// using the provided IP address, destination MAC, destination IP, and lifetime.
//
// This function performs the following:
// - Derives the source MAC address from the given IP using util.IPAddrToHWAddr.
// - Calculates the link-local address (LLA) from the source MAC using util.HWAddrToIPv6LLA.
// - Configures the destination IP and MAC address to use the provided values.
// - Sets the RA message's lifetime to the specified value.
//
// Parameters:
// - ip: The IP address used to derive the source MAC and LLA.
// - destinationMAC: The MAC address to which the RA message will be sent.
// - destinationIP: The IP address to which the RA message will be sent.
// - lifetime: The lifetime value for the RA message, in seconds.
//
// Returns:
// - An ndp.RouterAdvertisement object configured with the calculated source MAC, LLA, and the provided destination MAC, IP, and lifetime.
func newRouterAdvertisementFromIPAndLifetime(ip net.IP, destinationMAC net.HardwareAddr, destinationIP net.IP, lifetime uint16) ndp.RouterAdvertisement {
	sourceMAC := util.IPAddrToHWAddr(ip)
	return ndp.RouterAdvertisement{
		SourceMAC:      sourceMAC,
		SourceIP:       util.HWAddrToIPv6LLA(sourceMAC),
		DestinationMAC: destinationMAC,
		DestinationIP:  destinationIP,
		Lifetime:       lifetime,
	}
}

func newRouterAdvertisementWithPrefixInfos(ip net.IP, destinationMAC net.HardwareAddr, destinationIP net.IP, lifetime uint16, prefixInfos []ndp.PrefixInformation) ndp.RouterAdvertisement {
	sourceMAC := util.IPAddrToHWAddr(ip)
	return ndp.RouterAdvertisement{
		SourceMAC:      sourceMAC,
		SourceIP:       util.HWAddrToIPv6LLA(sourceMAC),
		DestinationMAC: destinationMAC,
		DestinationIP:  destinationIP,
		Lifetime:       lifetime,
		PrefixInfos:    prefixInfos,
	}
}
