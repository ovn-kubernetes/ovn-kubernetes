package ovn

import (
	"fmt"
	"reflect"
	"sort"

	"k8s.io/klog/v2"

	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	asName = "no-overlay-snat-exemption"
)

// initNoOverlaySNATExemptionAddressSet creates the address set for SNAT exemption in no-overlay mode.
func initNoOverlaySNATExemptionAddressSet(
	addressSetFactory addressset.AddressSetFactory,
	netInfo util.NetInfo,
	controllerName string,
) (addressset.AddressSet, error) {
	dbIDs := libovsdbops.NewDbObjectIDs(
		libovsdbops.AddressSetNoOverlaySNATExemption,
		controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: asName,
			libovsdbops.NetworkKey:    netInfo.GetNetworkName(),
		},
	)

	// Create the address set with empty initial addresses
	// Addresses will be added during controller initialization
	as, err := addressSetFactory.EnsureAddressSet(dbIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to create no-overlay SNAT exemption address set for network %s: %w",
			netInfo.GetNetworkName(), err)
	}

	klog.Infof("Initialized no-overlay SNAT exemption address set for network %s", netInfo.GetNetworkName())
	return as, nil
}

// getNoOverlaySNATExemptionAddressSet retrieves the cluster CIDR address set from the OVN database.
// Returns nil if the address set doesn't exist (e.g., not in no-overlay mode).
func getNoOverlaySNATExemptionAddressSet(
	addressSetFactory addressset.AddressSetFactory,
	netInfo util.NetInfo,
	controllerName string,
) (addressset.AddressSet, error) {
	dbIDs := libovsdbops.NewDbObjectIDs(
		libovsdbops.AddressSetNoOverlaySNATExemption,
		controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: asName,
			libovsdbops.NetworkKey:    netInfo.GetNetworkName(),
		},
	)

	as, err := addressSetFactory.GetAddressSet(dbIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to get no-overlay SNAT exemption address set for network %s: %w",
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

// cleanupNoOverlaySNATExemptionAddressSet deletes the cluster CIDR address set.
// Should be called during controller cleanup.
func cleanupNoOverlaySNATExemptionAddressSet(
	addressSetFactory addressset.AddressSetFactory,
	netInfo util.NetInfo,
	controllerName string,
) error {
	dbIDs := libovsdbops.NewDbObjectIDs(
		libovsdbops.AddressSetNoOverlaySNATExemption,
		controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: asName,
			libovsdbops.NetworkKey:    netInfo.GetNetworkName(),
		},
	)

	if err := addressSetFactory.DestroyAddressSet(dbIDs); err != nil {
		return fmt.Errorf("failed to cleanup no-overlay SNAT exemption address set for network %s: %w",
			netInfo.GetNetworkName(), err)
	}

	klog.V(5).Infof("Cleaned up no-overlay SNAT exemption address set for network %s", netInfo.GetNetworkName())
	return nil
}

// syncNoOverlaySNATExemptionAddressSet recalculates all IPs that should be in the no-overlay
// SNAT exemption address set (cluster CIDRs + local zone node IPs) and updates the address
// set if changed. This address set is used to exempt pod-to-pod and pod-to-node traffic from SNAT.
// This should be called when the local zone node is added or updated.
// Note: No-overlay mode only supports one node per IC zone.
// Creates the address set if it doesn't already exist.
func syncNoOverlaySNATExemptionAddressSet(
	addressSetFactory addressset.AddressSetFactory,
	netInfo util.NetInfo,
	controllerName string,
	allLocalNodeIPs []string,
) error {
	klog.V(5).Infof("Syncing no-overlay SNAT exemption address set for network %s", netInfo.GetNetworkName())
	var as addressset.AddressSet
	var err error
	as, err = getNoOverlaySNATExemptionAddressSet(addressSetFactory, netInfo, controllerName)
	if err != nil {
		return err
	}
	if as == nil {
		// Address set doesn't exist yet - create it first
		if as, err = initNoOverlaySNATExemptionAddressSet(addressSetFactory, netInfo, controllerName); err != nil {
			return fmt.Errorf("failed to create no-overlay SNAT exemption address set: %w", err)
		}
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
		klog.V(5).Infof("Updating no-overlay SNAT exemption address set for network %s with addresses %v", netInfo.GetNetworkName(), desiredAddrs)
		if err := as.SetAddresses(desiredAddrs); err != nil {
			return fmt.Errorf("failed to set addresses in no-overlay SNAT exemption address set: %w", err)
		}
	}

	return nil
}

// getNoOverlaySNATExemptionAsUUID returns the hashed address set UUIDs for IPv4 and IPv6.
// These hash names are used to reference the address set in SNAT rules.
func getNoOverlaySNATExemptionAsUUID(
	addressSetFactory addressset.AddressSetFactory,
	netInfo util.NetInfo,
	controllerName string,
) (string, string, error) {
	as, err := getNoOverlaySNATExemptionAddressSet(addressSetFactory, netInfo, controllerName)
	if err != nil {
		return "", "", err
	}
	if as == nil {
		return "", "", nil
	}

	v4UUID, v6UUID := as.GetASUUID()
	return v4UUID, v6UUID, nil
}
