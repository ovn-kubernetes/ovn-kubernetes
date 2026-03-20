package netlinkdevicemanager

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	nl "github.com/vishvananda/netlink/nl"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

func getLink(name string) (netlink.Link, error) {
	link, err := util.GetNetLinkOps().LinkByName(name)
	return link, err
}

// requireLink looks up a netlink device by name, returning a dependencyError if missing.
func requireLink(name string) (netlink.Link, error) {
	link, err := getLink(name)
	if err != nil {
		if util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return nil, &dependencyError{dependency: name, reason: "not found"}
		}
		return nil, err
	}
	return link, nil
}

// applyDeviceConfig creates or updates a single device in the kernel.
//
// The state pointer is used to update ifindex and masterIfindex atomically
// during I/O, BEFORE operations that generate events dependent on those
// values. This allows event handlers running concurrently on the event
// loop goroutine to filter accurately.
//
// Ownership rules:
//   - If device doesn't exist: create it with our alias
//   - If device exists with our alias: update or recreate as needed
//   - If device exists without our alias: return notOwnedError (could be human-created)
func applyDeviceConfig(cfg *managedDeviceConfig, state *managedDeviceState) (err error) {
	name := cfg.Name()
	var linkState netlink.Link
	var master netlink.Link
	defer func() {
		if err == nil {
			return
		}
		state.setMasterIfindex(0)
		state.setIfindex(0)
		if linkState != nil {
			if errVLAN := removeBridgeSelfVLANs(linkState, master); errVLAN != nil {
				klog.Warningf("NetlinkDeviceManager: failed to cleanup bridge self VLANs for %s: %v", name, errVLAN)
			}
			klog.V(5).Infof("NetlinkDeviceManager: deleting link %s after error: %v", name, err)
			errDel := util.GetNetLinkOps().LinkDelete(linkState)
			if errDel != nil {
				klog.Warningf("NetlinkDeviceManager: failed to delete link %s after error: %v", name, errDel)
			}
		}
	}()

	linkState, err = getLink(name)
	if err != nil && !util.GetNetLinkOps().IsLinkNotFoundError(err) {
		return fmt.Errorf("failed to get link %s: %w", name, err)
	}

	if linkState != nil {
		if err := checkOwnership(linkState); err != nil {
			linkState = nil // make sure we don't delete links we don't own
			return err
		}
	}

	if cfg.Master != "" {
		master, err = requireLink(cfg.Master)
		if err != nil {
			return fmt.Errorf("failed to get master link %s: %w", cfg.Master, err)
		}
		// masterIfindex is always stored when a master is configured, even if the
		// kernel already has the correct value. This is required because EnsureLink
		// resets the index to 0, so without storing the masterIfindex we would otherwise
		// see a mismatch on every link event — causing an infinite reconcile loop.
		state.setMasterIfindex(master.Attrs().Index)
	}

	if cfg.VLANParent != "" {
		parent, err := requireLink(cfg.VLANParent)
		if err != nil {
			return fmt.Errorf("failed to get vlan parent link %s: %w", cfg.VLANParent, err)
		}
		cfg.Link.(*netlink.Vlan).ParentIndex = parent.Attrs().Index
	}

	// If device exists and has immutable field changes, delete it and create a new one.
	if linkState != nil && !linkImmutableFieldsEqual(normalizeLinkState(cfg.Link, linkState), cfg.Link) {
		klog.V(5).Infof("NetlinkDeviceManager: device %s has immutable field changes, deleting", name)
		err := util.GetNetLinkOps().LinkDelete(linkState)
		linkState = nil
		if err != nil {
			return fmt.Errorf("failed to delete mismatched device %s: %w", name, err)
		}
	}

	var setBridgePortSettings func(link netlink.Link, settings *BridgePortSettings) error
	switch {
	case linkState != nil:
		if err := updateDevice(linkState, cfg); err != nil {
			return err
		}
		setBridgePortSettings = ensureBridgePortSettings
	default:
		linkState, err = createLink(cfg)
		if err != nil {
			return err
		}
		setBridgePortSettings = applyBridgePortSettings
	}

	// Updating ifindex state atomically on the store.
	// This is needed for filtering netlink AddrUpdate events in event loop.
	// It must be set before reconciling addresses to avoid dropping address events
	// matching the new link ifindex. Dropping events before this point is harmless
	// since they are covered by the following syncAddresses.
	state.setIfindex(linkState.Attrs().Index)

	if err := ensureMaster(linkState, master); err != nil {
		return err
	}

	if err := setBridgePortSettings(linkState, cfg.BridgePortSettings); err != nil {
		return fmt.Errorf("failed to apply bridge port settings for %s: %w", name, err)
	}

	if err := syncAddresses(linkState, cfg.Addresses); err != nil {
		return err
	}

	if err := syncVIDVNIMappings(linkState, master, cfg.VIDVNIMappings); err != nil {
		return err
	}

	return ensureDeviceUp(linkState)
}

// createLink creates a netlink device with our ownership alias and returns
// the created link with kernel-assigned attributes (ifindex, etc.).
//
// The alias is set via a dedicated LinkSetAlias call after
// LinkAdd because the kernel's newlink does not process alias.
// See https://github.com/torvalds/linux/blob/v7.0-rc2/Documentation/netlink/specs/rt-link.yaml#L2384-L2412:
// ifalias is not listed as a valid newlink request attribute.
// If alias-setting fails, the device is rolled back (deleted) to avoid
// leaving an unowned device in the kernel.
func createLink(cfg *managedDeviceConfig) (netlink.Link, error) {
	name := cfg.Name()
	// Strip FlagUp so the device is always created in DOWN state.
	cfg.Link.Attrs().Flags &^= net.FlagUp
	if err := util.GetNetLinkOps().LinkAdd(cfg.Link); err != nil {
		return nil, fmt.Errorf("failed to create device %s: %w", name, err)
	}
	// Fetch the created device to get kernel-assigned attributes
	link, err := getLink(name)
	if err != nil {
		return nil, fmt.Errorf("failed to get created device %s: %w", name, err)
	}
	if err := util.GetNetLinkOps().LinkSetAlias(link, cfg.Alias()); err != nil {
		aliasErr := fmt.Errorf("failed to set alias on device %s: %w", name, err)
		if delErr := util.GetNetLinkOps().LinkDelete(link); delErr != nil {
			return nil, fmt.Errorf("rollback also failed, device %s may be orphaned: %w",
				name, utilerrors.Join(aliasErr, delErr))
		}
		return nil, aliasErr
	}
	klog.V(5).Infof("NetlinkDeviceManager: created device %s", name)
	return link, nil
}

// updateDevice applies mutable link attribute changes (via LinkModify) to an
// existing device.
//
// Preconditions:
//   - Caller has verified ownership (device has our alias)
func updateDevice(link netlink.Link, cfg *managedDeviceConfig) error {
	name := cfg.Name()

	// Only call LinkModify if there are actual differences to apply.
	// This prevents unnecessary netlink events.
	if !linkMutableFieldsEqual(normalizeLinkState(cfg.Link, link), cfg.Link) {
		modifiedLink := prepareLinkForModify(link, cfg)
		if err := util.GetNetLinkOps().LinkModify(modifiedLink); err != nil {
			return fmt.Errorf("failed to modify link %s: %w", name, err)
		}
		klog.V(5).Infof("NetlinkDeviceManager: applied LinkModify for device %s", name)
	}

	return nil
}

// prepareLinkForModify creates a Link object suitable for LinkModify.
// Performs a deep copy of managed fields, then sets Index to target the existing device.
// Including immutable fields that match the existing device is safe,
// the kernel treats matching immutable fields as no-ops.
func prepareLinkForModify(existing netlink.Link, cfg *managedDeviceConfig) netlink.Link {
	result := cloneLink(cfg.Link)
	result.Attrs().Index = existing.Attrs().Index
	return result
}

// deleteDevice removes a device from the kernel.
// Only deletes devices that have our alias prefix (ownership check).
func deleteDevice(name string) error {
	link, err := getLink(name)
	if err != nil {
		if util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return nil // Already gone
		}
		return fmt.Errorf("failed to find device %s for deletion: %w", name, err)
	}

	// Safety check - only delete if it's ours
	if err := checkOwnership(link); err != nil {
		return err
	}

	if err := cleanupBridgeSelfVLANs(link); err != nil {
		return fmt.Errorf("NetlinkDeviceManager: cleaning bridge self VLANs for %s: %v", name, err)
	}

	if err := util.GetNetLinkOps().LinkDelete(link); err != nil {
		return fmt.Errorf("failed to delete device %s: %w", name, err)
	}
	klog.V(5).Infof("NetlinkDeviceManager: deleted device %s", name)
	return nil
}

// ensureDeviceUp brings a device up. LinkSetUp is idempotent so no
// need to check current state first.
func ensureDeviceUp(link netlink.Link) error {
	if err := util.GetNetLinkOps().LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to set link %s up: %w", link.Attrs().Name, err)
	}
	return nil
}

// ensureMaster ensures the device's master matches the desired config:
//   - If master is non-nil: sets it if the current MasterIndex differs.
//   - If master is nil: detaches from the current master if attached.
func ensureMaster(link netlink.Link, master netlink.Link) error {
	if master == nil {
		if link.Attrs().MasterIndex != 0 {
			if err := util.GetNetLinkOps().LinkSetNoMaster(link); err != nil {
				return fmt.Errorf("failed to detach %s from master: %w", link.Attrs().Name, err)
			}
			klog.V(5).Infof("NetlinkDeviceManager: detached device %s from master (ifindex %d)", link.Attrs().Name, link.Attrs().MasterIndex)
		}
		return nil
	}
	if link.Attrs().MasterIndex != master.Attrs().Index {
		if err := util.GetNetLinkOps().LinkSetMaster(link, master); err != nil {
			return fmt.Errorf("failed to set master %s for device %s: %w", master.Attrs().Name, link.Attrs().Name, err)
		}
		klog.V(5).Infof("NetlinkDeviceManager: set master %s for device %s", master.Attrs().Name, link.Attrs().Name)
	}
	return nil
}

// ensureBridgePortSettings applies bridge port settings if they differ from current.
//
// Compare-then-write is required because the kernel emits RTM_NEWLINK even
// for no-op writes (e.g. setting Learning=false when it is already false).
// Without the comparison, every reconciliation would trigger notifications
// which would trigger an infinite loop.
func ensureBridgePortSettings(link netlink.Link, settings *BridgePortSettings) error {
	if settings == nil {
		return nil
	}

	name := link.Attrs().Name

	// LinkGetProtinfo performs an AF_BRIDGE dump. link.Attrs().Protinfo is
	// not populated by LinkByName (only by event deserialization).
	protinfo, err := util.GetNetLinkOps().LinkGetProtinfo(link)
	if err != nil {
		return fmt.Errorf("failed to read bridge port settings for %s: %w", name, err)
	}

	if !bridgePortSettingsMatch(&protinfo, settings) {
		if err := applyBridgePortSettings(link, settings); err != nil {
			return fmt.Errorf("failed to apply bridge port settings for %s: %w", name, err)
		}
	}

	return nil
}

// applyBridgePortSettings sets bridge port settings.
func applyBridgePortSettings(link netlink.Link, settings *BridgePortSettings) error {
	if settings == nil {
		return nil
	}
	nlOps := util.GetNetLinkOps()
	name := link.Attrs().Name

	// Set VLAN tunnel mode
	if err := nlOps.LinkSetVlanTunnel(link, settings.VLANTunnel); err != nil {
		return fmt.Errorf("failed to set vlan_tunnel on %s: %w", name, err)
	}

	// Set neighbor suppress mode
	if err := nlOps.LinkSetBrNeighSuppress(link, settings.NeighSuppress); err != nil {
		return fmt.Errorf("failed to set neigh_suppress on %s: %w", name, err)
	}

	// Set learning mode
	if err := nlOps.LinkSetLearning(link, settings.Learning); err != nil {
		return fmt.Errorf("failed to set learning on %s: %w", name, err)
	}

	// Set isolated mode
	if err := nlOps.LinkSetIsolated(link, settings.Isolated); err != nil {
		return fmt.Errorf("failed to set isolated on %s: %w", name, err)
	}

	klog.V(5).Infof("NetlinkDeviceManager: applied bridge port settings for device %s", name)
	return nil
}

// cleanupBridgeSelfVLANs removes bridge self VLANs associated with a VXLAN
// device's tunnel mappings.
func cleanupBridgeSelfVLANs(link netlink.Link) error {
	if _, ok := link.(*netlink.Vxlan); !ok {
		return nil
	}
	masterIdx := link.Attrs().MasterIndex
	if masterIdx == 0 {
		return nil
	}

	nlOps := util.GetNetLinkOps()
	bridgeLink, err := nlOps.LinkByIndex(masterIdx)
	if err != nil {
		if nlOps.IsLinkNotFoundError(err) {
			return nil // Bridge already gone — its self VLANs are gone with it.
		}
		return fmt.Errorf("failed to resolve master (ifindex %d) for %s: %w",
			masterIdx, link.Attrs().Name, err)
	}

	return removeBridgeSelfVLANs(link, bridgeLink)
}

// removeBridgeSelfVLANs removes bridge self VLANs for a VXLAN device given its
// bridge master. No-op if either link is not a VXLAN or bridgeLink is nil.
func removeBridgeSelfVLANs(vxlanLink netlink.Link, bridgeLink netlink.Link) error {
	if _, ok := vxlanLink.(*netlink.Vxlan); !ok || bridgeLink == nil {
		return nil
	}

	nlOps := util.GetNetLinkOps()
	mappings, err := getVIDVNIMappings(vxlanLink)
	if err != nil {
		return fmt.Errorf("failed to read tunnel info for %s: %w", vxlanLink.Attrs().Name, err)
	}

	var errs []error
	for _, m := range mappings {
		if err := nlOps.BridgeVlanDel(bridgeLink, m.VID, false, false, true, false); err != nil {
			if !nlOps.IsEntryNotFoundError(err) {
				errs = append(errs, fmt.Errorf("bridge self VID %d: %w", m.VID, err))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to remove bridge self VLANs for %s: %w", vxlanLink.Attrs().Name, utilerrors.Join(errs...))
	}
	return nil
}

// syncAddresses ensures the device has exactly the desired addresses.
// If desiredAddr is nil, no address management is performed (existing addresses preserved).
// Link-local addresses (fe80::/10) are never removed automatically.
func syncAddresses(currentLink netlink.Link, desiredAddr []netlink.Addr) error {
	if desiredAddr == nil {
		return nil // No address management requested
	}

	nlOps := util.GetNetLinkOps()
	name := currentLink.Attrs().Name

	current, err := nlOps.AddrList(currentLink, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("failed to list addresses on %s: %w", name, err)
	}

	// Build lookup maps (key = "IP/prefix") and compute diff using sets
	desiredMap := addrListToMap(desiredAddr)
	currentMap := addrListToMap(current)

	desiredKeys := sets.KeySet(desiredMap)
	currentKeys := sets.KeySet(currentMap)

	toAdd := desiredKeys.Difference(currentKeys)
	toRemove := currentKeys.Difference(desiredKeys)

	var errs []error

	for key := range toAdd {
		addr := desiredMap[key]
		if err := nlOps.AddrAdd(currentLink, addr); err != nil {
			// EEXIST is fine - address already exists (race or concurrent add)
			if !nlOps.IsAlreadyExistsError(err) {
				errs = append(errs, fmt.Errorf("failed to add address %s to %s: %w", key, name, err))
				continue
			}
		}
		klog.V(5).Infof("NetlinkDeviceManager: added address %s to %s", key, name)
	}

	for key := range toRemove {
		addr := currentMap[key]
		if isLinkLocalAddress(addr.IP) {
			continue // Never remove link-local addresses
		}
		if err := nlOps.AddrDel(currentLink, addr); err != nil {
			// EADDRNOTAVAIL/ENOENT is fine - address already gone
			if !nlOps.IsEntryNotFoundError(err) {
				errs = append(errs, fmt.Errorf("failed to remove address %s from %s: %w", key, name, err))
				continue
			}
		}
		klog.V(5).Infof("NetlinkDeviceManager: removed address %s from %s", key, name)
	}

	if len(errs) > 0 {
		return fmt.Errorf("address sync errors on %s: %w", name, utilerrors.Join(errs...))
	}
	return nil
}

// syncVIDVNIMappings ensures the VXLAN device has exactly the desired VID/VNI mappings.
// If mappings is nil, no mapping management is performed.
//
// Each mapping requires four independent kernel entries:
//   - Bridge self VLAN — the bridge's own VLAN filter (on the bridge device)
//   - VXLAN VID membership — the port VLAN filter (on the VXLAN bridge port)
//   - VNI filter entry — allowed VNIs (on the VXLAN device)
//   - Tunnel info — the VID→VNI mapping (on the VXLAN bridge port)
//
// All four are queried to build a complete picture; only mappings with at least
// one missing entry are re-applied (addVIDVNIMapping handles EEXIST for the
// intact entries). This self-heals against external removal of any individual
// entry without redundantly re-applying fully intact mappings.
func syncVIDVNIMappings(link netlink.Link, bridgeLink netlink.Link, mappings []VIDVNIMapping) error {
	if mappings == nil || bridgeLink == nil {
		return nil
	}

	nlOps := util.GetNetLinkOps()
	name := link.Attrs().Name

	tunnelMappings, err := getVIDVNIMappings(link)
	if err != nil {
		return fmt.Errorf("failed to read tunnel info for %s: %w", name, err)
	}

	vlanList, err := nlOps.BridgeVlanList()
	if err != nil {
		return fmt.Errorf("failed to read bridge VLAN list: %w", err)
	}

	vniList, err := nlOps.BridgeVniList()
	if err != nil {
		return fmt.Errorf("failed to read VNI filter list: %w", err)
	}

	vxlanIdx := int32(link.Attrs().Index)
	toAdd, toRemove := diffVIDVNIMappings(
		mappings,
		tunnelMappings,
		extractVIDs(vlanList[int32(bridgeLink.Attrs().Index)]),
		extractVIDs(vlanList[vxlanIdx]),
		extractVNIs(vniList[vxlanIdx]),
	)

	var errs []error

	for _, mapping := range toRemove {
		if err := removeVIDVNIMapping(bridgeLink, link, mapping); err != nil {
			klog.Warningf("NetlinkDeviceManager: failed to remove mapping VID=%d VNI=%d from %s: %v",
				mapping.VID, mapping.VNI, name, err)
			errs = append(errs, err)
		}
	}

	for _, mapping := range toAdd {
		if err := addVIDVNIMapping(bridgeLink, link, mapping); err != nil {
			klog.Warningf("NetlinkDeviceManager: failed to add mapping VID=%d VNI=%d on %s: %v",
				mapping.VID, mapping.VNI, name, err)
			errs = append(errs, err)
		}
	}

	if len(toAdd) > 0 || len(toRemove) > 0 {
		klog.V(5).Infof("NetlinkDeviceManager: VID/VNI mappings synced for %s (added=%d, removed=%d, errors=%d)",
			name, len(toAdd), len(toRemove), len(errs))
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to apply %d mappings on %s: %w", len(errs), name, utilerrors.Join(errs...))
	}
	return nil
}

// getVIDVNIMappings retrieves current VID/VNI mappings for a VXLAN device.
func getVIDVNIMappings(link netlink.Link) ([]VIDVNIMapping, error) {
	nlOps := util.GetNetLinkOps()
	name := link.Attrs().Name

	tunnels, err := nlOps.BridgeVlanTunnelShowDev(link)
	if err != nil {
		klog.V(5).Infof("NetlinkDeviceManager: could not get tunnel info for %s: %v", name, err)
		return nil, err
	}

	var mappings []VIDVNIMapping
	for _, t := range tunnels {
		if t.TunId > 0 {
			mappings = append(mappings, VIDVNIMapping{
				VID: t.Vid,
				VNI: t.TunId,
			})
		}
	}

	klog.V(5).Infof("NetlinkDeviceManager: found %d existing VID/VNI mappings on %s", len(mappings), name)
	return mappings, nil
}

// extractVIDs returns the set of VIDs from a BridgeVlanInfo slice.
func extractVIDs(infos []*nl.BridgeVlanInfo) sets.Set[uint16] {
	s := sets.New[uint16]()
	for _, info := range infos {
		s.Insert(info.Vid)
	}
	return s
}

// extractVNIs returns the set of VNIs from a BridgeVniInfo slice.
// Handles both single entries and ranges (VniEnd > 0).
func extractVNIs(infos []*nl.BridgeVniInfo) sets.Set[uint32] {
	s := sets.New[uint32]()
	for _, info := range infos {
		end := max(info.VniEnd, info.Vni)
		for vni := info.Vni; vni <= end; vni++ {
			s.Insert(vni)
		}
	}
	return s
}

// diffVIDVNIMappings computes which mappings need to be added or removed.
// Returns two slices:
//   - toRemove: tunnel info entries not in desired (stale mappings).
//   - toAdd: desired mappings where any of the four kernel entries is missing.
//
// Known limitation: toRemove is derived solely from tunnel-info (the "current"
// parameter). If tunnel-info for a mapping is externally removed and that
// mapping is later removed from the desired set, the remaining three kernel
// entries (bridge self VLAN, VXLAN port VID, VNI filter) become orphaned.
// This is because the Linux kernel provides no ownership marker for
// bridge VLAN/VNI entries, so we cannot distinguish entries we created from
// entries added by other systems.
func diffVIDVNIMappings(desired, current []VIDVNIMapping,
	bridgeSelfVIDs, vxlanPortVIDs sets.Set[uint16],
	activeVNIs sets.Set[uint32]) (toAdd, toRemove []VIDVNIMapping) {

	currentSet := sets.New(current...)
	desiredSet := sets.New(desired...)
	toRemove = currentSet.Difference(desiredSet).UnsortedList()

	for _, m := range desired {
		if !isMappingFullyPresent(m, bridgeSelfVIDs, vxlanPortVIDs, activeVNIs, currentSet) {
			toAdd = append(toAdd, m)
		}
	}
	return toAdd, toRemove
}

func isMappingFullyPresent(m VIDVNIMapping, bridgeSelfVIDs, vxlanPortVIDs sets.Set[uint16], activeVNIs sets.Set[uint32], currentSet sets.Set[VIDVNIMapping]) bool {
	return bridgeSelfVIDs.Has(m.VID) &&
		vxlanPortVIDs.Has(m.VID) &&
		activeVNIs.Has(m.VNI) &&
		currentSet.Has(m)
}

// addVIDVNIMapping adds a VID/VNI mapping to a VXLAN device.
//
// Order matters: bridge self VLAN is added LAST, after the VXLAN-side entries.
// This guarantees that if a bridge self VLAN exists for a VID, its tunnel info
// also exists, which is the invariant that removeBridgeSelfVLANs relies on to
// discover which VIDs need cleanup.
func addVIDVNIMapping(bridgeLink, vxlanLink netlink.Link, m VIDVNIMapping) error {
	nlOps := util.GetNetLinkOps()

	// Add VID to VXLAN with 'master' flag
	if err := nlOps.BridgeVlanAdd(vxlanLink, m.VID, false, false, false, true); err != nil {
		if !nlOps.IsAlreadyExistsError(err) {
			return fmt.Errorf("failed to add VID %d to VXLAN: %w", m.VID, err)
		}
	}

	// Add VNI to VNI filter
	if err := nlOps.BridgeVniAdd(vxlanLink, m.VNI); err != nil {
		if !nlOps.IsAlreadyExistsError(err) {
			return fmt.Errorf("failed to add VNI %d: %w", m.VNI, err)
		}
	}

	// Add tunnel info (VID -> VNI mapping)
	if err := nlOps.BridgeVlanAddTunnelInfo(vxlanLink, m.VID, m.VNI, false, true); err != nil {
		if !nlOps.IsAlreadyExistsError(err) {
			return fmt.Errorf("failed to add VID->VNI mapping: %w", err)
		}
	}

	// Add VID to bridge with 'self' flag (at LAST, see docstring)
	if err := nlOps.BridgeVlanAdd(bridgeLink, m.VID, false, false, true, false); err != nil {
		if !nlOps.IsAlreadyExistsError(err) {
			return fmt.Errorf("failed to add VID %d to bridge self: %w", m.VID, err)
		}
	}

	return nil
}

// removeVIDVNIMapping removes a VID/VNI mapping from a VXLAN device.
// Symmetric with addVIDVNIMapping: removes all four kernel entries in
// reverse order (tunnel info, VNI filter, VXLAN VID, bridge self VLAN).
//
// The bridge self VLAN removal is safe because the SVD (Single VXLAN Device)
// architecture guarantees exactly one VXLAN per bridge, and each VID is
// unique per network — no sibling device can reference the same VID.
func removeVIDVNIMapping(bridgeLink, vxlanLink netlink.Link, m VIDVNIMapping) error {
	nlOps := util.GetNetLinkOps()

	var errs []error

	// Remove tunnel_info mapping
	if err := nlOps.BridgeVlanDelTunnelInfo(vxlanLink, m.VID, m.VNI, false, true); err != nil {
		if !nlOps.IsEntryNotFoundError(err) {
			errs = append(errs, fmt.Errorf("tunnel_info VID=%d->VNI=%d: %w", m.VID, m.VNI, err))
		}
	}

	// Remove VNI from VNI filter
	if err := nlOps.BridgeVniDel(vxlanLink, m.VNI); err != nil {
		if !nlOps.IsEntryNotFoundError(err) {
			errs = append(errs, fmt.Errorf("VNI %d: %w", m.VNI, err))
		}
	}

	// Remove VID from VXLAN
	if err := nlOps.BridgeVlanDel(vxlanLink, m.VID, false, false, false, true); err != nil {
		if !nlOps.IsEntryNotFoundError(err) {
			errs = append(errs, fmt.Errorf("VXLAN VID %d: %w", m.VID, err))
		}
	}

	// Remove VID from bridge self
	if err := nlOps.BridgeVlanDel(bridgeLink, m.VID, false, false, true, false); err != nil {
		if !nlOps.IsEntryNotFoundError(err) {
			errs = append(errs, fmt.Errorf("bridge self VID %d: %w", m.VID, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to remove mapping from %s: %w", vxlanLink.Attrs().Name, utilerrors.Join(errs...))
	}
	return nil
}
