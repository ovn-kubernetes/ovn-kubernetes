// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package id

import (
	"fmt"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// NetworkTunnelKeyOwner returns the allocator owner for a NAD network.
func NetworkTunnelKeyOwner(name string) string {
	return "network/" + name
}

// NetworkConnectTunnelKeyOwner returns the allocator owner for a CNC router.
func NetworkConnectTunnelKeyOwner(name string) string {
	return "cnc/" + name
}

// TunnelKeysAllocator is used to allocate tunnel Keys for distributed OVN datapaths.
// It preserves the first types.MaxNetworks keys for the already-used transit switch IDs based on the networkID.
type TunnelKeysAllocator struct {
	idsAllocator   *idsAllocator
	preservedRange int
	idsOffset      int
}

// NewTunnelKeyAllocator returns an TunnelKeysAllocator
func NewTunnelKeyAllocator(name string) *TunnelKeysAllocator {
	// total number of datapath(switches and routers) keys
	maxDPKey := types.MaxDatapathTunnelKey
	// We have already used some keys for transit switch tunnels, the maximum tunnel key that is already allocated
	// is BaseTransitSwitchTunnelKey + types.MaxNetworks - 1.
	// BaseTransitSwitchTunnelKey = 16711683
	rangeStart := 16711683 + types.MaxNetworks
	// this is how many keys are left for allocation
	freeIDs := maxDPKey - rangeStart + 1

	return &TunnelKeysAllocator{
		idsAllocator:   newIDsAllocator(name, freeIDs, rangeStart),
		preservedRange: types.MaxNetworks,
		idsOffset:      16711683,
	}
}

// AllocateKeys allocates 'numOfKeys' for the resource 'name'.
// Previously allocated keys for 'name' are preserved in case of error.
// If networkID is less than types.MaxNetworks, the first key will come from the preserved range
// based on the networkID.
// If less keys than numOfKeys are already allocated for the resource name, it will allocate the missing amount.
// If more keys than numOfKeys are already allocated for the resource name, it returns an error.
func (allocator *TunnelKeysAllocator) AllocateKeys(name string, networkID, numOfKeys int) ([]int, error) {
	keys, _, err := allocator.allocateKeys(name, networkID, numOfKeys, false)
	return keys, err
}

// AllocateKeysWithRollback allocates keys and returns a function that releases
// only keys added by this call. Existing keys remain reserved on rollback.
// Surplus keys remain reserved until the caller persists the returned prefix
// and releases them with TrimKeys.
// The caller must serialize operations for this resource until it commits or
// rolls back the allocation.
func (allocator *TunnelKeysAllocator) AllocateKeysWithRollback(name string, networkID, numOfKeys int) ([]int, func(), error) {
	return allocator.allocateKeys(name, networkID, numOfKeys, true)
}

// allowSurplus returns the requested prefix without releasing extra reservations,
// so callers can persist the shorter annotation before releasing them with TrimKeys.
// When false, requesting fewer keys than already allocated returns an error.
func (allocator *TunnelKeysAllocator) allocateKeys(name string, networkID, numOfKeys int, allowSurplus bool) ([]int, func(), error) {
	allocator.idsAllocator.nameIdsMap.LockKey(name)
	defer allocator.idsAllocator.nameIdsMap.UnlockKey(name)
	existingIDs, _ := allocator.idsAllocator.nameIdsMap.Load(name)
	previousCount := len(existingIDs)
	allocatedIDs := make([]int, 0, numOfKeys)
	if networkID < allocator.preservedRange && numOfKeys > 0 {
		allocatedIDs = append(allocatedIDs, allocator.idsOffset+networkID)
		numOfKeys--
	}
	if previousCount > numOfKeys {
		if !allowSurplus {
			return nil, nil, fmt.Errorf("the resource %s already has more ids allocated %v than requested %v", name, existingIDs, numOfKeys)
		}
		return append(allocatedIDs, existingIDs[:numOfKeys]...), func() {}, nil
	}
	newIDs, err := allocator.idsAllocator.allocateIDs(name, numOfKeys)
	if err != nil {
		return nil, nil, err
	}
	rollback := func() {
		allocator.idsAllocator.TrimIDs(name, previousCount)
	}
	return append(allocatedIDs, newIDs...), rollback, nil
}

// ReserveKeys reserves 'tunnelKeys' for the resource 'name'. It returns an
// error if one of the 'tunnelKeys' is already reserved by a resource other than 'name'.
// It also returns an error if the resource 'name' has a different 'tunnelKeys' slice
// already reserved. Slice elements order is important for comparison.
func (allocator *TunnelKeysAllocator) ReserveKeys(name string, tunnelKeys []int) error {
	if len(tunnelKeys) > 0 && tunnelKeys[0] >= allocator.idsOffset && tunnelKeys[0]-allocator.idsOffset < allocator.preservedRange {
		// transit switch tunnel key is not allocated by the allocator
		tunnelKeys = tunnelKeys[1:]
	}
	return allocator.idsAllocator.ReserveIDs(name, tunnelKeys)
}

// ValidateKeys validates a network ID and its annotated tunnel keys. Keys in the
// preserved range are derived from the network ID rather than tracked by the
// allocator, so ReserveKeys cannot detect a forged transit-switch key.
func (allocator *TunnelKeysAllocator) ValidateKeys(networkID int, tunnelKeys []int) error {
	if networkID < 0 || networkID >= allocator.preservedRange {
		return fmt.Errorf("network ID %d is outside the valid range [0, %d)", networkID, allocator.preservedRange)
	}
	// Networks need at most 2 tunnel keys: a switch key and a router key.
	if len(tunnelKeys) > 2 {
		return fmt.Errorf("expected at most 2 tunnel keys for network %d, but got %d",
			networkID, len(tunnelKeys))
	}
	if len(tunnelKeys) == 0 {
		return nil
	}
	want := allocator.idsOffset + networkID
	if tunnelKeys[0] != want {
		return fmt.Errorf("expected tunnel key %d for the transit switch of network %d, but got %d",
			want, networkID, tunnelKeys[0])
	}
	return allocator.ValidatePreservedKeys(tunnelKeys[1:])
}

// ValidatePreservedKeys returns an error if one of 'tunnelKeys' is preserved for
// the transit switch of a network. ReserveKeys strips such a key instead of
// reserving it, because it is derived from a network ID rather than tracked by
// the allocator, so a resource that is not a network must not be given one.
func (allocator *TunnelKeysAllocator) ValidatePreservedKeys(tunnelKeys []int) error {
	for _, key := range tunnelKeys {
		if key >= allocator.idsOffset && key-allocator.idsOffset < allocator.preservedRange {
			return fmt.Errorf("tunnel key %d is preserved for the transit switch of network %d",
				key, key-allocator.idsOffset)
		}
	}
	return nil
}

// TrimKeys releases the datapath tunnel keys 'name' holds beyond what a network
// with 'networkID' needs for 'numOfKeys' keys, keeping the valid ones in place.
// It mirrors AllocateKeys: the transit switch key is derived from the network ID
// and not tracked by the allocator, so only the dynamic keys are counted.
func (allocator *TunnelKeysAllocator) TrimKeys(name string, networkID, numOfKeys int) {
	dynamicKeys := numOfKeys
	if networkID < allocator.preservedRange && numOfKeys > 0 {
		dynamicKeys -= 1
	}
	allocator.idsAllocator.TrimIDs(name, dynamicKeys)
}

// ReleaseKeys releases the tunnelKeys allocated for the resource 'name'
func (allocator *TunnelKeysAllocator) ReleaseKeys(name string) {
	allocator.idsAllocator.ReleaseIDs(name)
}
