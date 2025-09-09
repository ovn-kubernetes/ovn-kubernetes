package id

// TunnelKeysAllocator is used to allocate tunnel Keys for distributed OVN datapaths.
// It preserves first 4096 keys for the already-used transit switch IDs based on the networkID.
type TunnelKeysAllocator struct {
	idsAllocator   *idsAllocator
	preservedRange int
	idsOffset      int
}

// NewTunnelKeyAllocator returns an TunnelKeysAllocator
func NewTunnelKeyAllocator(name string) *TunnelKeysAllocator {
	// OVN-defined constants from
	// https://github.com/ovn-org/ovn/blob/cfaf849c034469502fc97149f20676dec4d76595/lib/ovn-util.h#L159-L164
	// total number of datapath(switches and routers) keys
	maxDPKey := (1 << 24) - 1
	// We have already used some keys for transit switch tunnels, the maximum tunnel key that is already allocated
	// is BaseTransitSwitchTunnelKey + MaxNetworks.
	// BaseTransitSwitchTunnelKey = 16711683
	// MaxNetworks = 4096
	rangeStart := 16711683 + 4096
	// this is how many keys are left for allocation
	freeIDs := maxDPKey - rangeStart + 1

	return &TunnelKeysAllocator{
		idsAllocator:   newIDsAllocator(name, freeIDs, rangeStart),
		preservedRange: 4096,
		idsOffset:      16711683,
	}
}

// AllocateKeys allocates 'numOfKeys' for the resource 'name'.
// Previously allocated keys for 'name' are preserved in case of error.
// If networkID is less than 4096, the first key will come from the preserved range
// based on the networkID.
// If less keys than numOfKeys are already allocated for the resource name, it will allocate the missing amount.
// If more keys than numOfKeys are already allocated for the resource name, it returns an error.
func (allocator *TunnelKeysAllocator) AllocateKeys(name string, networkID, numOfKeys int) ([]int, error) {
	allocatedIDs := make([]int, 0, numOfKeys)
	if networkID < allocator.preservedRange && numOfKeys > 0 {
		// transit switch tunnel key is preserved
		allocatedIDs = append(allocatedIDs, allocator.idsOffset+networkID)
		numOfKeys -= 1
	}
	newIDs, err := allocator.idsAllocator.AllocateIDs(name, numOfKeys)
	if err != nil {
		return nil, err
	}
	return append(allocatedIDs, newIDs...), nil
}

// ReserveKeys reserves 'tunnelKeys' for the resource 'name'. It returns an
// error if one of the 'tunnelKeys' is already reserved by a resource other than 'name'.
// It also returns an error if the resource 'name' has a different 'tunnelKeys' slice
// already reserved. Slice elements order is important for comparison.
func (allocator *TunnelKeysAllocator) ReserveKeys(name string, tunnelKeys []int) error {
	if len(tunnelKeys) > 0 && tunnelKeys[0]-allocator.idsOffset < allocator.preservedRange {
		// transit switch tunnel key is not allocated by the allocator
		tunnelKeys = tunnelKeys[1:]
	}
	return allocator.idsAllocator.ReserveIDs(name, tunnelKeys)
}

// ReleaseKeys releases the tunnelKeys allocated for the resource 'name'
func (allocator *TunnelKeysAllocator) ReleaseKeys(name string) {
	allocator.idsAllocator.ReleaseIDs(name)
}
