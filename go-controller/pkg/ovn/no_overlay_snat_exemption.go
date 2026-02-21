package ovn

import (
	"fmt"
	"reflect"
	"sort"

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

	if netInfo.GetOutboundSNAT() != config.NoOverlaySNATEnabled {
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

	// Check if address set actually exists by checking UUIDs
	v4UUID, v6UUID := as.GetASUUID()
	if v4UUID == "" && v6UUID == "" {
		// Address set doesn't exist in the database
		return nil, nil
	}

	return as, nil
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

// syncNoOverlaySNATExemptionAddressSet recalculates all IPs that should be in the no-overlay
// SNAT exemption address set (cluster CIDRs + all local zone node IPs) and updates the address
// set if changed. This address set is used to exempt pod-to-pod and pod-to-node traffic from SNAT.
// This should be called when a local zone node is added or updated.
func syncNoOverlaySNATExemptionAddressSet(
	addressSetFactory addressset.AddressSetFactory,
	netInfo util.NetInfo,
	controllerName string,
	allLocalNodeIPs []string,
) error {
	as, err := getClusterCIDRAddressSet(addressSetFactory, netInfo, controllerName)
	if err != nil {
		return err
	}
	if as == nil {
		// Not in no-overlay mode or SNAT not enabled, nothing to do
		return nil
	}

	// Calculate desired addresses: cluster CIDRs + all local node IPs
	var desiredAddrs []string

	// Add cluster CIDRs
	for _, subnet := range netInfo.Subnets() {
		desiredAddrs = append(desiredAddrs, subnet.CIDR.String())
	}

	// Add all local node IPs
	desiredAddrs = append(desiredAddrs, allLocalNodeIPs...)

	// Get current addresses from the address set
	currentIPv4Addrs, currentIPv6Addrs := as.GetAddresses()
	currentAddrs := append(currentIPv4Addrs, currentIPv6Addrs...)

	// Check if update is needed - compare length first, then sort and compare
	needsUpdate := len(currentAddrs) != len(desiredAddrs)
	if !needsUpdate {
		sort.Strings(desiredAddrs)
		sort.Strings(currentAddrs)
		needsUpdate = !reflect.DeepEqual(currentAddrs, desiredAddrs)
	}

	// Update address set if needed (SetAddresses handles IPv4/IPv6 separation automatically)
	if needsUpdate {
		klog.V(5).Infof("Updating cluster CIDR address set for network %s with addresses %v", netInfo.GetNetworkName(), desiredAddrs)
		if err := as.SetAddresses(desiredAddrs); err != nil {
			return fmt.Errorf("failed to set addresses in cluster CIDR address set: %w", err)
		}
	}

	return nil
}

// getClusterCIDRAsUUID returns the hashed address set UUIDs for IPv4 and IPv6.
// These hash names are used to reference the address set in SNAT rules.
// Returns empty strings if not in no-overlay mode with outbound SNAT enabled.
func getClusterCIDRAsUUID(
	addressSetFactory addressset.AddressSetFactory,
	netInfo util.NetInfo,
	controllerName string,
) (string, string, error) {
	// Only return address set UUIDs if we're in no-overlay mode with outbound SNAT enabled.
	// This check is critical: even if an address set exists in the database from a previous
	// configuration, we must not use it when outboundSNAT is disabled.
	if netInfo.GetNetworkTransport() != config.TransportNoOverlay || netInfo.GetOutboundSNAT() != config.NoOverlaySNATEnabled {
		return "", "", nil
	}

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
