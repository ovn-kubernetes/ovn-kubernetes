package ovn

import (
	"fmt"

	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	clusterCIDR = "cluster-cidr"
)

// initClusterCIDRAddressSet creates the address set for cluster CIDRs in no-overlay mode.
// This address set contains all cluster pod subnet CIDRs and is used for SNAT exemption
// to prevent SNATing pod-to-pod traffic while allowing SNAT for pod-to-external traffic.
// Returns nil if the network is not using no-overlay transport or if outbound SNAT is disabled.
func initClusterCIDRAddressSet(
	addressSetFactory addressset.AddressSetFactory,
	netInfo util.NetInfo,
	controllerName string,
) error {
	// Only create for networks using no-overlay transport with outbound SNAT enabled
	if netInfo.GetNetworkTransport() != config.TransportNoOverlay {
		klog.V(5).Infof("Skipping cluster CIDR address set initialization: network %s not using no-overlay transport",
			netInfo.GetNetworkName())
		return nil
	}

	if config.NoOverlay.OutboundSNAT != config.NoOverlaySNATEnabled {
		klog.V(5).Infof("Skipping cluster CIDR address set initialization: outbound SNAT not enabled for network %s",
			netInfo.GetNetworkName())
		return nil
	}

	dbIDs := libovsdbops.NewDbObjectIDs(
		libovsdbops.AddressSetClusterCIDR,
		controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: clusterCIDR,
			libovsdbops.NetworkKey:    netInfo.GetNetworkName(),
		},
	)

	// Create the address set with empty initial addresses
	// Cluster subnets will be added during controller initialization
	_, err := addressSetFactory.EnsureAddressSet(dbIDs)
	if err != nil {
		return fmt.Errorf("failed to create node subnets address set for network %s: %w",
			netInfo.GetNetworkName(), err)
	}

	klog.Infof("Initialized node subnets address set for network %s", netInfo.GetNetworkName())
	return nil
}

// getClusterCIDRAddressSet retrieves the cluster CIDR address set from the OVN database.
// Returns nil if the address set doesn't exist (e.g., not in no-overlay mode).
func getClusterCIDRAddressSet(
	addressSetFactory addressset.AddressSetFactory,
	netInfo util.NetInfo,
	controllerName string,
) (addressset.AddressSet, error) {
	// Only retrieve for networks using no-overlay transport with outbound SNAT enabled
	if netInfo.GetNetworkTransport() != config.TransportNoOverlay {
		return nil, nil
	}

	if config.NoOverlay.OutboundSNAT != config.NoOverlaySNATEnabled {
		return nil, nil
	}

	dbIDs := libovsdbops.NewDbObjectIDs(
		libovsdbops.AddressSetClusterCIDR,
		controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: clusterCIDR,
			libovsdbops.NetworkKey:    netInfo.GetNetworkName(),
		},
	)

	as, err := addressSetFactory.GetAddressSet(dbIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to get node subnets address set for network %s: %w",
			netInfo.GetNetworkName(), err)
	}

	return as, nil
}

// addClusterCIDRsToAddressSet adds the cluster CIDRs to the cluster CIDR address set.
func addClusterCIDRsToAddressSet(
	addressSetFactory addressset.AddressSetFactory,
	netInfo util.NetInfo,
	controllerName string,
) error {
	if len(netInfo.Subnets()) == 0 {
		return nil
	}

	as, err := getClusterCIDRAddressSet(addressSetFactory, netInfo, controllerName)
	if err != nil {
		return err
	}
	if as == nil {
		// Not in no-overlay mode or SNAT not enabled, nothing to do
		return nil
	}

	var subnets []string
	for _, subnet := range netInfo.Subnets() {
		subnets = append(subnets, subnet.CIDR.String())
	}

	klog.V(5).Infof("Added cluster CIDRs %v to address set for network %s", subnets, netInfo.GetNetworkName())
	if err := as.AddAddresses(subnets); err != nil {
		return fmt.Errorf("failed to add cluster CIDRs %v to address set: %w", subnets, err)
	}

	return nil
}

// cleanupClusterCIDRAddressSet deletes the cluster CIDR address set.
// Should be called during controller cleanup.
func cleanupClusterCIDRAddressSet(
	addressSetFactory addressset.AddressSetFactory,
	netInfo util.NetInfo,
	controllerName string,
) error {
	dbIDs := libovsdbops.NewDbObjectIDs(
		libovsdbops.AddressSetClusterCIDR,
		controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: clusterCIDR,
			libovsdbops.NetworkKey:    netInfo.GetNetworkName(),
		},
	)

	if err := addressSetFactory.DestroyAddressSet(dbIDs); err != nil {
		return fmt.Errorf("failed to cleanup cluster CIDR address set for network %s: %w",
			netInfo.GetNetworkName(), err)
	}

	klog.V(5).Infof("Cleaned up cluster CIDR address set for network %s", netInfo.GetNetworkName())
	return nil
}

// getClusterCIDRAsUUID returns the hashed address set UUIDs for IPv4 and IPv6.
// These hash names are used to reference the address set in SNAT rules.
// Returns empty strings if not in no-overlay mode or if the address set doesn't exist.
func getClusterCIDRAsUUID(
	addressSetFactory addressset.AddressSetFactory,
	netInfo util.NetInfo,
	controllerName string,
) (string, string, error) {
	as, err := getClusterCIDRAddressSet(addressSetFactory, netInfo, controllerName)
	if err != nil {
		return "", "", err
	}
	if as == nil {
		return "", "", nil
	}

	v4UUID, v6UUID := as.GetASUUID()
	return v4UUID, v6UUID, nil
}
