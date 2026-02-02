package netlinkdevicemanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// ManagedAliasPrefix is the prefix used in IFLA_IFALIAS to mark devices managed by this controller.
// This allows safe cleanup: only delete devices with this prefix.
// Format: "ovn-k8s-ndm:<type>:<name>" for debugging and collision avoidance.
const ManagedAliasPrefix = "ovn-k8s-ndm:"

// MaxInterfaceNameLength is the maximum length for Linux interface names.
// Linux's IFNAMSIZ is 16 (including null terminator), so max usable length is 15.
const MaxInterfaceNameLength = 15

// validateInterfaceName checks if an interface name is valid.
// Returns an error if the name is empty or exceeds the Linux limit.
func validateInterfaceName(name, context string) error {
	if name == "" {
		return fmt.Errorf("%s name is empty", context)
	}
	if len(name) > MaxInterfaceNameLength {
		return fmt.Errorf("%s name %q exceeds maximum length of %d characters (got %d)",
			context, name, MaxInterfaceNameLength, len(name))
	}
	return nil
}

// NotOwnedError is returned when an operation is blocked because the device
// exists but is not owned by us (no alias or foreign alias).
// This is a permanent error - retrying won't help unless the external device is removed.
type NotOwnedError struct {
	DeviceName string
	Reason     string
}

func (e *NotOwnedError) Error() string {
	return fmt.Sprintf("device %s not owned by us: %s", e.DeviceName, e.Reason)
}

// IsNotOwnedError returns true if the error indicates a device ownership conflict.
func IsNotOwnedError(err error) bool {
	var notOwned *NotOwnedError
	return errors.As(err, &notOwned)
}

// isOurDevice returns true only if the device has our alias prefix.
// This is the single source of truth for ownership:
//   - Empty alias = unknown ownership, NOT ours (could be human-created or other automation)
//   - Foreign alias = definitely NOT ours
//   - Our prefix = ours, safe to modify/delete
func isOurDevice(link netlink.Link) bool {
	return strings.HasPrefix(link.Attrs().Alias, ManagedAliasPrefix)
}

// DefaultReconcilePeriod is the default interval for periodic sync as a safety net.
const DefaultReconcilePeriod = 60 * time.Second

// DeviceConfig represents the complete desired configuration for a network device.
// Controllers provide the FULL configuration; manager enforces EXACTLY what's provided.
type DeviceConfig struct {
	// Link is the netlink device (Bridge, Vxlan, Vlan, Device, etc.)
	// Must include all desired attributes in LinkAttrs (Name, HardwareAddr, etc.)
	Link netlink.Link

	// Master is the name of the master device (e.g., bridge name for VXLAN, VRF name for SVI)
	// If the master doesn't exist yet, config is stored as pending and retried on netlink events.
	Master string

	// VLANParent is the name of the parent device for VLAN interfaces.
	// If set, the parent's current ifindex is resolved at creation time.
	// This is more resilient than relying on Link.(*netlink.Vlan).ParentIndex
	// because ifindex can change if the parent is deleted and recreated.
	// If the parent doesn't exist yet, config is stored as pending and retried on netlink events.
	VLANParent string

	// BridgePortSettings configures bridge port-specific settings.
	// Only applicable when Master is set (device is attached to a bridge).
	// Settings are applied after the device is attached to the bridge.
	// Typically used for VXLAN ports that need vlan_tunnel=on, neigh_suppress=on, learning=off.
	BridgePortSettings *BridgePortSettings

	// Addresses specifies IP addresses to configure on the device.
	//
	// Semantics:
	//   - nil:           No address management. Existing addresses are preserved.
	//   - empty slice:   Declarative empty state. All addresses will be removed
	//                    (except auto-configured link-local fe80::/10).
	//   - non-empty:     Declarative. Exactly these addresses will exist.
	//                    Missing addresses are added, extra addresses are removed
	//                    (except link-local).
	//
	// Address equality is based on IPNet (IP + prefix length) only.
	// Other Addr fields (Flags, Scope, Label, ValidLft, PreferredLft) are
	// applied when adding but not used for comparison.
	//
	// Link-local addresses (fe80::/10) are never auto-removed because they
	// are kernel-managed and removing them can break IPv6 functionality.
	Addresses []netlink.Addr
}

// VIDVNIMapping represents a single VID↔VNI mapping for bridge VXLAN configuration.
type VIDVNIMapping struct {
	VID uint16 // VLAN ID on the bridge (1-4094)
	VNI uint32 // VNI for VXLAN tunnel (1-16777215)
}

// BridgePortSettings configures bridge port-specific settings used for VXLAN port configuration.
type BridgePortSettings struct {
	VLANTunnel    bool // Enable VLAN tunnel mode (bridge link set dev X vlan_tunnel on)
	NeighSuppress bool // Enable neighbor suppression (bridge link set dev X neigh_suppress on)
	Learning      bool // Enable MAC learning
}

// BridgePortVLAN configures VLAN membership on a bridge port.
type BridgePortVLAN struct {
	VID      int  // VLAN ID
	PVID     bool // Set as Port VLAN ID (native VLAN)
	Untagged bool // Egress untagged
}

// managedDevice tracks a device with its config and status
type managedDevice struct {
	cfg     DeviceConfig // Complete desired config
	pending bool         // True if waiting for dependency (e.g., VRF, parent)
}

// managedVIDVNIMappings tracks VID/VNI mappings for a VXLAN device
type managedVIDVNIMappings struct {
	bridgeName string          // Parent bridge name (for self VLAN)
	vxlanName  string          // VXLAN device name
	mappings   []VIDVNIMapping // Desired mappings
}

// managedPortVLAN tracks VLAN configuration for a bridge port
type managedPortVLAN struct {
	linkName string         // Device name
	vlan     BridgePortVLAN // Desired VLAN config
}

// Controller manages Linux network device lifecycle.
// Returns errors for immediate caller feedback AND self-heals via periodic sync.
// Uses full-scan reconciliation on every sync cycle.
type Controller struct {
	mu      *sync.Mutex
	store   map[string]*managedDevice // device name -> managed device info
	started bool                      // True after Run() called

	// ReconcilePeriod is the interval for periodic sync as a safety net.
	// Defaults to DefaultReconcilePeriod. Can be overridden before calling Run().
	ReconcilePeriod time.Duration

	// Stores for mappings and port VLANs (desired state for self-healing)
	// Note: Bridge port settings are stored in DeviceConfig.BridgePortSettings
	vidVNIMappingStore map[string]*managedVIDVNIMappings   // vxlanName -> mappings
	portVLANStore      map[string]map[int]*managedPortVLAN // linkName -> vid -> VLAN config

	// Pending deletes (tombstones) for self-healing deletion retries
	// When DeleteLink() fails, the device is added here and retried in sync()
	pendingDeletes map[string]struct{} // device name -> needs deletion
}

// GetBridgeMappings returns a copy of the desired VID/VNI mappings for a VXLAN device.
// Returns nil if the device is not managed or has no mappings.
func (c *Controller) GetBridgeMappings(vxlanName string) []VIDVNIMapping {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry := c.vidVNIMappingStore[vxlanName]
	if entry == nil || len(entry.mappings) == 0 {
		return nil
	}
	return slices.Clone(entry.mappings)
}

// NewController creates a new NetlinkDeviceManager with default settings.
// The ReconcilePeriod can be overridden before calling Run() if needed.
func NewController() *Controller {
	return &Controller{
		mu:                 &sync.Mutex{},
		store:              make(map[string]*managedDevice),
		ReconcilePeriod:    DefaultReconcilePeriod,
		vidVNIMappingStore: make(map[string]*managedVIDVNIMappings),
		portVLANStore:      make(map[string]map[int]*managedPortVLAN),
		pendingDeletes:     make(map[string]struct{}),
	}
}

// EnsureLink stores the desired device configuration and creates/updates it in the kernel.
//
// Semantics:
//   - Provide the COMPLETE desired configuration
//   - Manager ensures device exists with specified master and brings it up
//   - If device exists with same stored config, this is a no-op (idempotent)
//
// Return values:
//   - nil: Device created/updated successfully, OR stored as pending (dependency missing).
//     Pending devices are retried automatically on netlink events and periodic sync.
//   - NotOwnedError: Device exists but is not owned by us (name collision with external device).
//     Caller should check IsNotOwnedError() and decide:
//     a) Try a different name: call DeleteLink() then EnsureLink() with new name
//     b) Wait for external device to be deleted: manager retries on netlink event
//     c) Give up: call DeleteLink() to remove intent
//     IMPORTANT: Caller should NOT requeue aggressively - manager handles retries internally.
//   - Other error: Transient failure (e.g., permission denied), caller may requeue.
//
// Reconciliation behavior:
//   - Mutable LinkAttrs (MTU, TxQLen, HardwareAddr, Alias): Updated via LinkModify
//   - Master attachment: Re-attached if changed
//   - BridgePortSettings: Re-applied if changed
//   - Up state: Ensured on every sync
//   - Immutable attrs (VxlanId, VlanId, VRF Table, etc.): Triggers delete+recreate
//
// Ownership contract:
//   - The manager stores cfg by reference. Caller MUST NOT mutate cfg.Link or
//     cfg.BridgePortSettings after this call returns.
//   - Create a fresh DeviceConfig for each call if reusing struct instances.
//
// Controllers MUST call EnsureLink for all desired devices BEFORE calling Run()
// to establish the desired state for startup reconciliation.

func (c *Controller) EnsureLink(cfg DeviceConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	name := cfg.deviceName()
	if err := validateInterfaceName(name, "device"); err != nil {
		return err
	}
	if cfg.Master != "" {
		if err := validateInterfaceName(cfg.Master, "master"); err != nil {
			return err
		}
	}
	if cfg.VLANParent != "" {
		if err := validateInterfaceName(cfg.VLANParent, "VLAN parent"); err != nil {
			return err
		}
	}

	// Check if config is unchanged (idempotent)
	if existing := c.store[name]; existing != nil {
		if configsEqual(&existing.cfg, &cfg) {
			if !existing.pending {
				klog.V(5).Infof("NetlinkDeviceManager: %s already in desired state, skipping", name)
				return nil
			}
			// Config unchanged but pending - don't overwrite, just skip
			// Manager will retry via periodic sync and netlink events
			klog.V(5).Infof("NetlinkDeviceManager: %s is pending, manager will retry", name)
			return nil
		}
	}

	// Store desired state (new or changed config)
	c.store[name] = &managedDevice{
		cfg:     cfg,
		pending: false,
	}

	// If not started yet, just store - will be applied in fullReconcile
	if !c.started {
		return nil
	}

	// Apply immediately (netlink I/O under lock)
	return c.ensureDevice(name, &cfg)
}

// ensureDevice applies device config and tracks pending state on dependency errors.
// Wraps applyDeviceConfig() with controller-level retry semantics: if a dependency is missing,
// the device is marked pending and will be retried when the dependency appears.
// Must be called with c.mu held.
func (c *Controller) ensureDevice(name string, cfg *DeviceConfig) error {
	if err := applyDeviceConfig(name, cfg); err != nil {
		// Mark as pending if dependency not ready - will retry on netlink event or sync
		if isDependencyError(err) {
			if device := c.store[name]; device != nil {
				device.pending = true
			}
			klog.V(4).Infof("NetlinkDeviceManager: %s stored as pending (dependency not ready): %v", name, err)
			return nil
		}
		// Ownership conflict - return error so caller can decide
		if IsNotOwnedError(err) {
			klog.Warningf("NetlinkDeviceManager: %s blocked by external device: %v", name, err)
			return err
		}
		// Other error - return to caller
		klog.Errorf("NetlinkDeviceManager: failed to ensure %s: %v", name, err)
		return fmt.Errorf("failed to ensure device %s: %w", name, err)
	}

	// Success - clear pending flag
	if device := c.store[name]; device != nil {
		device.pending = false
	}
	return nil
}

// DeleteLink removes a device from the desired state and deletes it from the kernel.
// If kernel deletion fails, the device is retried in sync(). Returns error for caller to retry.
//
// Cleans up entries keyed by device name (mappings if VXLAN, port VLANs).
// Cross-referenced entries are preserved: if bridge "br0" is deleted, mappings on "vxlan0"
// that reference "br0" remain in desired state. This allows self-healing when the bridge
// is recreated. To permanently remove mappings, delete the VXLAN or call DeleteBridgeMappings().
func (c *Controller) DeleteLink(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	_, wasManaged := c.store[name]
	_, isPendingDelete := c.pendingDeletes[name]
	if !wasManaged && !isPendingDelete {
		klog.V(5).Infof("NetlinkDeviceManager: %s not managed, nothing to delete", name)
		return nil
	}

	// Remove from desired state and related stores
	delete(c.store, name)
	c.cleanupRelatedStores(name)

	// Before Run(), just update desired state - fullReconcile() will clean stale devices
	if !c.started {
		delete(c.pendingDeletes, name)
		klog.V(5).Infof("NetlinkDeviceManager: %s removed from desired state (not started yet)", name)
		return nil
	}

	// Try kernel deletion (I/O under lock)
	if err := deleteDevice(name); err != nil {
		// Don't tombstone if device isn't ours (some external change took over the device) - retrying won't help
		if IsNotOwnedError(err) {
			delete(c.pendingDeletes, name)
			klog.Warningf("NetlinkDeviceManager: cannot delete %s: %v (device exists but not ours)", name, err)
			return nil // Treat as success - we removed from desired state
		}
		// Tombstone for retry
		c.pendingDeletes[name] = struct{}{}
		klog.Errorf("NetlinkDeviceManager: failed to delete %s, will retry: %v", name, err)
		return fmt.Errorf("failed to delete device %s: %w", name, err)
	}

	delete(c.pendingDeletes, name)
	klog.V(4).Infof("NetlinkDeviceManager: deleted device %s", name)
	return nil
}

// cleanupRelatedStores removes configuration entries keyed by device name.
// Must be called with c.mu held.
func (c *Controller) cleanupRelatedStores(name string) {
	delete(c.vidVNIMappingStore, name)
	delete(c.portVLANStore, name)
}

// Has checks if a device is registered in the desired state.
func (c *Controller) Has(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.store[name]
	return ok
}

// GetConfig returns the config for a managed device, or nil if not managed.
func (c *Controller) GetConfig(name string) *DeviceConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	existing := c.store[name]
	if existing == nil {
		return nil
	}
	cfgCopy := existing.cfg
	return &cfgCopy
}

// ListDevicesByVLANParent returns all managed devices that have the specified VLANParent.
func (c *Controller) ListDevicesByVLANParent(parent string) []DeviceConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result []DeviceConfig
	for _, dev := range c.store {
		if dev.cfg.VLANParent == parent {
			result = append(result, dev.cfg)
		}
	}
	return result
}

// EnsureBridgeMappings ensures a VXLAN device has exactly the specified VID/VNI mappings.
// It also ensures the bridge has the corresponding VLANs configured with 'self' flag.
//
// Semantics:
// - Provide ALL desired mappings (full-state, not incremental)
// - Manager stores desired state for periodic sync/self-healing
// - Computes diff between current and desired mappings
// - Stale mappings are removed, missing mappings are added
// - Returns aggregated error if any operation fails (caller can requeue)
//
// Constraints:
// - Each VNI must be unique within the mappings (no two VIDs mapping to the same VNI)
// - Each VID must be unique within the mappings
func (c *Controller) EnsureBridgeMappings(bridgeName, vxlanName string, mappings []VIDVNIMapping) error {
	// Validate uniqueness constraints
	if err := validateMappingsUniqueness(mappings); err != nil {
		return fmt.Errorf("invalid mappings for %s: %w", vxlanName, err)
	}

	// Copy the mappings slice to avoid races if caller mutates the original
	mappingsCopy := slices.Clone(mappings)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.vidVNIMappingStore[vxlanName] = &managedVIDVNIMappings{
		bridgeName: bridgeName,
		vxlanName:  vxlanName,
		mappings:   mappingsCopy,
	}

	// Before Run(), just store desired state - fullReconcile() will apply on startup
	if !c.started {
		klog.V(5).Infof("NetlinkDeviceManager: stored mappings for %s (not started yet)", vxlanName)
		return nil
	}

	// Get current mappings and compute diff
	currentMappings, err := getVIDVNIMappings(vxlanName)
	if err != nil {
		klog.V(5).Infof("NetlinkDeviceManager: failed to get current mappings for %s: %v", vxlanName, err)
		currentMappings = nil
	}

	toAdd, toRemove := diffMappings(currentMappings, mappingsCopy)
	if len(toAdd) == 0 && len(toRemove) == 0 {
		return nil
	}

	// Apply changes
	var errs []error
	for _, m := range toRemove {
		if err := removeVIDVNIMapping(vxlanName, m); err != nil {
			klog.Errorf("NetlinkDeviceManager: failed to remove mapping VID=%d VNI=%d from %s: %v",
				m.VID, m.VNI, vxlanName, err)
			errs = append(errs, err)
		}
	}
	for _, m := range toAdd {
		if err := addVIDVNIMapping(bridgeName, vxlanName, m); err != nil {
			klog.Errorf("NetlinkDeviceManager: failed to add mapping VID=%d VNI=%d to %s: %v",
				m.VID, m.VNI, vxlanName, err)
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		klog.V(4).Infof("NetlinkDeviceManager: %s mappings updated (+%d/-%d, errors=%d)",
			vxlanName, len(toAdd), len(toRemove), len(errs))
		return fmt.Errorf("failed to apply %d/%d mappings on %s: %w",
			len(errs), len(toAdd)+len(toRemove), vxlanName, errors.Join(errs...))
	}

	if len(toAdd) > 0 || len(toRemove) > 0 {
		klog.V(4).Infof("NetlinkDeviceManager: %s mappings updated (+%d/-%d)",
			vxlanName, len(toAdd), len(toRemove))
	}
	return nil
}

// EnsureBridgePortVLAN ensures a bridge port has the specified VLAN membership.
//
// Semantics:
// - Manager stores desired state for periodic sync/self-healing
// - Returns error if operation fails (caller can requeue)
//
// Used for OVS ports attached to Linux bridge that need VLAN tagging.
// TODO: Consider if we should handle OVS port lifecycle as well in this controller.
func (c *Controller) EnsureBridgePortVLAN(linkName string, vlan BridgePortVLAN) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Initialize nested map if needed
	if c.portVLANStore[linkName] == nil {
		c.portVLANStore[linkName] = make(map[int]*managedPortVLAN)
	}
	c.portVLANStore[linkName][vlan.VID] = &managedPortVLAN{
		linkName: linkName,
		vlan:     vlan,
	}

	// Before Run(), just store desired state - fullReconcile() will apply on startup
	if !c.started {
		klog.V(5).Infof("NetlinkDeviceManager: stored port VLAN %d for %s (not started yet)", vlan.VID, linkName)
		return nil
	}

	// Check if update needed
	current, err := getBridgePortVLAN(linkName, vlan.VID)
	if err == nil && ptr.Equal(current, &vlan) {
		klog.V(5).Infof("NetlinkDeviceManager: VLAN %d already configured on %s, skipping", vlan.VID, linkName)
		return nil
	}

	// Apply VLAN (I/O under lock)
	return applyBridgePortVLAN(linkName, vlan)
}

// DeleteBridgePortVLAN removes a VLAN from the port VLAN store and from the kernel.
func (c *Controller) DeleteBridgePortVLAN(linkName string, vid int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Remove from store
	if vlans := c.portVLANStore[linkName]; vlans != nil {
		delete(vlans, vid)
		if len(vlans) == 0 {
			delete(c.portVLANStore, linkName)
		}
	}

	// Before Run(), just update desired state - don't touch kernel
	if !c.started {
		return nil
	}

	// Remove from kernel
	return deleteBridgePortVLAN(linkName, vid)
}

// deleteBridgePortVLAN removes a VLAN from a bridge port in the kernel.
func deleteBridgePortVLAN(linkName string, vid int) error {
	nlOps := util.GetNetLinkOps()

	link, err := nlOps.LinkByName(linkName)
	if err != nil {
		if nlOps.IsLinkNotFoundError(err) {
			// Device already gone - nothing to remove (idempotent)
			return nil
		}
		return fmt.Errorf("failed to get link %s: %w", linkName, err)
	}

	// For port VLANs: self=false, master=true
	if err := nlOps.BridgeVlanDel(link, uint16(vid), false, false, false, true); err != nil {
		if !nlOps.IsEntryNotFoundError(err) {
			return fmt.Errorf("failed to delete VLAN %d from port %s: %w", vid, linkName, err)
		}
	}

	klog.V(4).Infof("NetlinkDeviceManager: deleted VLAN %d from port %s", vid, linkName)
	return nil
}

// Run starts the controller and watches for netlink events.
// Controllers should call EnsureLink for all desired devices BEFORE calling Run().
//
// Parameters:
//   - stopCh: When closed, signals the controller to stop watching for events and exit.
//     The caller is responsible for closing this channel when shutdown is desired.
//   - doneWg: The controller calls doneWg.Add(1) on start and doneWg.Done() on exit.
//     Callers can use doneWg.Wait() to block until the controller has fully stopped.
//
// Returns an error if the initial netlink subscription fails. Once running, netlink
// errors are logged but do not cause Run to return; the controller will attempt to
// resubscribe automatically.
func (c *Controller) Run(stopCh <-chan struct{}, doneWg *sync.WaitGroup) error {
	linkSubscribeOptions := netlink.LinkSubscribeOptions{
		ErrorCallback: func(err error) {
			klog.Errorf("NetlinkDeviceManager: error in LinkSubscribe callback: %v", err)
		},
	}

	subscribe := func() (bool, chan netlink.LinkUpdate, error) {
		linkChan := make(chan netlink.LinkUpdate)
		if err := netlink.LinkSubscribeWithOptions(linkChan, stopCh, linkSubscribeOptions); err != nil {
			return false, nil, err
		}
		// Full reconcile on startup/resubscribe
		c.fullReconcile()
		return true, linkChan, nil
	}

	return c.runInternal(stopCh, doneWg, subscribe)
}

type subscribeFn func() (bool, chan netlink.LinkUpdate, error)

func (c *Controller) runInternal(stopCh <-chan struct{}, doneWg *sync.WaitGroup, subscribe subscribeFn) error {
	// Copy ReconcilePeriod to avoid races if caller modifies it after Run() starts
	reconcilePeriod := c.ReconcilePeriod

	c.mu.Lock()
	c.started = true
	c.mu.Unlock()

	subscribed, linkChan, err := subscribe()
	if err != nil {
		return fmt.Errorf("error during netlink subscribe: %w", err)
	}

	doneWg.Add(1)
	go func() {
		defer doneWg.Done()

		syncTimer := time.NewTicker(reconcilePeriod)
		defer syncTimer.Stop()

		for {
			select {
			case update, ok := <-linkChan:
				syncTimer.Reset(reconcilePeriod)
				if !ok {
					// Channel closed, resubscribe
					if subscribed, linkChan, err = subscribe(); err != nil {
						klog.Errorf("NetlinkDeviceManager: error during netlink resubscribe: %v", err)
					}
					continue
				}
				// Process the link update
				c.handleLinkUpdate(update.Link)

			case <-syncTimer.C:
				klog.V(5).Info("NetlinkDeviceManager: periodic sync")
				c.sync()
				if !subscribed {
					if subscribed, linkChan, err = subscribe(); err != nil {
						klog.Errorf("NetlinkDeviceManager: error during netlink resubscribe: %v", err)
					}
				}

			case <-stopCh:
				klog.Info("NetlinkDeviceManager: stopping")
				return
			}
		}
	}()

	klog.Info("NetlinkDeviceManager is running")
	return nil
}

// handleLinkUpdate processes a single netlink link update event.
// This is the reactive dependency resolution mechanism.
func (c *Controller) handleLinkUpdate(link netlink.Link) {
	c.mu.Lock()
	defer c.mu.Unlock()

	linkName := link.Attrs().Name
	klog.V(5).Infof("NetlinkDeviceManager: link update for %s", linkName)

	// Retry pending devices that depend on this link.
	// Design note: Linear scan should be fine here because we expect a small number of pending devices.
	for name, device := range c.store {
		if !device.pending {
			continue
		}
		// Check if this device was waiting for the updated link
		if device.cfg.Master != linkName && device.cfg.VLANParent != linkName {
			continue
		}

		klog.V(4).Infof("NetlinkDeviceManager: retrying pending device %s (dependency %s appeared)", name, linkName)
		if err := applyDeviceConfig(name, &device.cfg); err != nil {
			if isDependencyError(err) {
				// Still waiting for dependency - keep pending, will retry on next event
				klog.V(5).Infof("NetlinkDeviceManager: %s still pending: %v", name, err)
			} else if IsNotOwnedError(err) {
				// Ownership conflict - clear pending, don't retry until user intervenes
				device.pending = false
				klog.Warningf("NetlinkDeviceManager: %s blocked by external device: %v", name, err)
			} else {
				// Other error (e.g., kernel error) - clear pending to avoid infinite retry loop
				// Device will be retried on periodic reconcile, not on every netlink event
				device.pending = false
				klog.Errorf("NetlinkDeviceManager: failed to create pending device %s (will retry on sync): %v", name, err)
			}
		} else {
			device.pending = false
			klog.V(4).Infof("NetlinkDeviceManager: pending device %s created successfully", name)
		}
	}

	// Also ensure the managed device itself if it exists in our store
	if device := c.store[linkName]; device != nil {
		if err := applyDeviceConfig(linkName, &device.cfg); err != nil {
			if isDependencyError(err) {
				device.pending = true
				klog.V(5).Infof("NetlinkDeviceManager: %s pending on dependency: %v", linkName, err)
			} else if IsNotOwnedError(err) {
				// Device exists but is not ours - someone took over the name
				klog.Warningf("NetlinkDeviceManager: %s ownership lost: %v", linkName, err)
			} else {
				klog.Warningf("NetlinkDeviceManager: error ensuring managed device %s: %v", linkName, err)
			}
		} else {
			device.pending = false
		}
	}

	// Sync mappings immediately for traffic continuity.
	// VID/VNI mappings are on the critical path - without them, VXLAN encap/decap fails.
	//
	// If this link is a VXLAN with mappings, sync immediately
	if m, exists := c.vidVNIMappingStore[linkName]; exists {
		c.syncMappingsForVXLAN(linkName, m)
	}
	// If this link is a bridge, sync any VXLANs using it immediately.
	// Linear scan is fine for typical deployments with a small number of VXLANs.
	for vxlanName, m := range c.vidVNIMappingStore {
		if m.bridgeName == linkName {
			c.syncMappingsForVXLAN(vxlanName, m)
		}
	}
	// Note: port VLANs (for OVS ports) are synced via periodic sync(),
	// not on individual link events (would require tracking OVS port names).
}

// fullReconcile creates missing devices, updates changed ones, and deletes stale ones.
// Runs on startup to sync all devices.
func (c *Controller) fullReconcile() {
	start := time.Now()
	klog.Info("NetlinkDeviceManager: starting full reconcile")

	c.mu.Lock()

	// Get all links in kernel with our alias prefix
	links, err := util.GetNetLinkOps().LinkList()
	if err != nil {
		c.mu.Unlock()
		klog.Errorf("NetlinkDeviceManager: failed to list links: %v", err)
		return
	}
	managedDevs := make(map[string]netlink.Link)
	for _, link := range links {
		if isOurDevice(link) {
			managedDevs[link.Attrs().Name] = link
		}
	}

	var created, updated, deleted, pending int

	// Ensure all desired devices exist
	for name, device := range c.store {
		_, inKernel := managedDevs[name]
		if err := applyDeviceConfig(name, &device.cfg); err != nil {
			if isDependencyError(err) {
				device.pending = true
				pending++
				klog.V(4).Infof("NetlinkDeviceManager: %s marked pending (dependency not ready)", name)
			} else if IsNotOwnedError(err) {
				klog.Warningf("NetlinkDeviceManager: %s blocked by external device: %v", name, err)
			} else {
				klog.Errorf("NetlinkDeviceManager: failed to ensure %s: %v", name, err)
			}
		} else {
			device.pending = false
			if inKernel {
				updated++
			} else {
				created++
			}
		}
	}

	// Delete stale devices (in kernel but not in desired state)
	for name, link := range managedDevs {
		if _, desired := c.store[name]; !desired {
			klog.V(4).Infof("NetlinkDeviceManager: deleting stale device %s", name)
			if err := util.GetNetLinkOps().LinkDelete(link); err != nil {
				klog.Errorf("NetlinkDeviceManager: failed to delete stale %s: %v", name, err)
			} else {
				deleted++
			}
		}
	}

	klog.Infof("NetlinkDeviceManager: full reconcile devices completed in %v (created=%d, updated=%d, deleted=%d, pending=%d)",
		time.Since(start), created, updated, deleted, pending)

	c.mu.Unlock()

	// Apply all other stores (mappings, port VLANs, pending deletes)
	// Note: Bridge port settings are applied as part of device creation/update via DeviceConfig.BridgePortSettings
	c.syncMappings()
	c.syncPortVLANs()
	c.syncDeletes()

	klog.Infof("NetlinkDeviceManager: full reconcile completed in %v", time.Since(start))
}

// sync ensures all managed resources are in desired state.
// Performs a full scan of all managed resources to catch any external drift.
//
// Called periodically as defensive measure - normally netlink events should catch everything.
//
// NOTE: In future if the number of devices increases, a fast/slow audit pattern
// could be implemented where full scans happen less frequently and most cycles only
// process pending/dirty entries.
func (c *Controller) sync() {
	c.syncDevices()
	c.syncMappings()
	c.syncPortVLANs()
	c.syncDeletes()
}

// syncDeletes retries pending deletions (tombstones).
// Called on every sync cycle to ensure failed deletions eventually succeed.
func (c *Controller) syncDeletes() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.pendingDeletes) == 0 {
		return
	}

	klog.V(4).Infof("NetlinkDeviceManager: retrying %d pending deletes", len(c.pendingDeletes))

	for name := range c.pendingDeletes {
		// Skip if device was re-added to desired state (EnsureLink called)
		if _, stillDesired := c.store[name]; stillDesired {
			delete(c.pendingDeletes, name)
			klog.V(4).Infof("NetlinkDeviceManager: skipping delete of %s (now desired)", name)
			continue
		}

		if err := deleteDevice(name); err != nil {
			klog.V(5).Infof("NetlinkDeviceManager: delete retry failed for %s: %v", name, err)
			// Leave in pendingDeletes for next sync
		} else {
			delete(c.pendingDeletes, name)
			klog.V(4).Infof("NetlinkDeviceManager: successfully deleted %s on retry", name)
		}
	}
}

// syncDevices syncs all managed devices.
func (c *Controller) syncDevices() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for name, device := range c.store {
		if err := applyDeviceConfig(name, &device.cfg); err != nil {
			if isDependencyError(err) {
				device.pending = true
			} else if IsNotOwnedError(err) {
				klog.Warningf("NetlinkDeviceManager: %s blocked by external device: %v", name, err)
				// Keep in store for retry when external device is removed
			} else {
				klog.Errorf("NetlinkDeviceManager: sync failed for %s: %v", name, err)
			}
		} else {
			device.pending = false
		}
	}
}

// syncMappings syncs VID/VNI mappings.
func (c *Controller) syncMappings() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for vxlanName, m := range c.vidVNIMappingStore {
		c.syncMappingsForVXLAN(vxlanName, m)
	}
}

// syncMappingsForVXLAN syncs VID/VNI mappings for a VXLAN device.
// Must be called with c.mu held.
func (c *Controller) syncMappingsForVXLAN(vxlanName string, m *managedVIDVNIMappings) {
	current, err := getVIDVNIMappings(vxlanName)
	if err != nil {
		klog.Warningf("NetlinkDeviceManager: sync mappings - failed to get current for %s: %v (will retry on next sync)", vxlanName, err)
		return
	}

	toAdd, toRemove := diffMappings(current, m.mappings)
	if len(toAdd) == 0 && len(toRemove) == 0 {
		return
	}

	var errCount int
	for _, mapping := range toRemove {
		if err := removeVIDVNIMapping(vxlanName, mapping); err != nil {
			klog.Warningf("NetlinkDeviceManager: sync mappings - failed to remove VID=%d VNI=%d from %s: %v",
				mapping.VID, mapping.VNI, vxlanName, err)
			errCount++
		}
	}
	for _, mapping := range toAdd {
		if err := addVIDVNIMapping(m.bridgeName, vxlanName, mapping); err != nil {
			klog.Warningf("NetlinkDeviceManager: sync mappings - failed to add VID=%d VNI=%d to %s: %v",
				mapping.VID, mapping.VNI, vxlanName, err)
			errCount++
		}
	}

	if len(toAdd) > 0 || len(toRemove) > 0 {
		klog.V(4).Infof("NetlinkDeviceManager: sync mappings %s (+%d/-%d, errors=%d)",
			vxlanName, len(toAdd), len(toRemove), errCount)
	}
}

// syncPortVLANs syncs bridge port VLAN configurations.
// Always does a full scan of all stored port VLANs.
func (c *Controller) syncPortVLANs() {
	c.mu.Lock()
	defer c.mu.Unlock()

	var synced int
	for _, vlans := range c.portVLANStore {
		for _, v := range vlans {
			current, err := getBridgePortVLAN(v.linkName, v.vlan.VID)
			if err != nil {
				// VLAN doesn't exist, need to add
				if err := applyBridgePortVLAN(v.linkName, v.vlan); err != nil {
					klog.Warningf("NetlinkDeviceManager: failed to sync port VLAN %d on %s: %v", v.vlan.VID, v.linkName, err)
				} else {
					synced++
				}
				continue
			}

			if ptr.Equal(current, &v.vlan) {
				continue
			}

			if err := applyBridgePortVLAN(v.linkName, v.vlan); err != nil {
				klog.Warningf("NetlinkDeviceManager: failed to sync port VLAN %d on %s: %v", v.vlan.VID, v.linkName, err)
			} else {
				synced++
			}
		}
	}

	if synced > 0 {
		klog.V(5).Infof("NetlinkDeviceManager: synced %d port VLAN entries", synced)
	}
}

// resolveDependencies validates and resolves name-based dependencies to ifindices.
// Returns a resolved config copy with ParentIndex set for VLANs.
// Returns DependencyError if any required dependency doesn't exist yet.
//
// This function:
//   - Errors if VLANParent is set but Link isn't *netlink.Vlan (invalid config)
//   - Errors if Link is VLAN but neither VLANParent nor ParentIndex is set (invalid config)
//   - Validates VLAN parent exists (by name or by ifindex, returns DependencyError if missing)
//   - Resolves VLANParent name → ParentIndex
//   - Validates Master exists (returns DependencyError if missing)
//
// The returned config is safe to use for create/update operations.
// The original config is never modified (store integrity preserved).
func resolveDependencies(cfg *DeviceConfig) (*DeviceConfig, error) {
	// Validate: VLANParent only makes sense for VLAN devices
	if cfg.VLANParent != "" {
		if _, ok := cfg.Link.(*netlink.Vlan); !ok {
			linkType := "nil"
			if cfg.Link != nil {
				linkType = cfg.Link.Type()
			}
			return nil, fmt.Errorf("invalid DeviceConfig: VLANParent set but Link is %s, not VLAN", linkType)
		}
	}

	// Validate: VLAN devices must have either VLANParent or ParentIndex
	// A VLAN without a parent cannot be created or meaningfully updated.
	if vlan, ok := cfg.Link.(*netlink.Vlan); ok {
		if cfg.VLANParent == "" && vlan.ParentIndex == 0 {
			return nil, fmt.Errorf("invalid DeviceConfig: VLAN %q requires VLANParent or ParentIndex", cfg.deviceName())
		}
		// For legacy style (ParentIndex set directly), validate parent exists.
		// This ensures uniform dependency handling for both name-based and ifindex-based parents.
		if cfg.VLANParent == "" && vlan.ParentIndex > 0 {
			if _, err := util.GetNetLinkOps().LinkByIndex(vlan.ParentIndex); err != nil {
				if util.GetNetLinkOps().IsLinkNotFoundError(err) {
					return nil, &DependencyError{
						Dependency: fmt.Sprintf("parent ifindex %d", vlan.ParentIndex),
						Reason:     "VLAN parent not found",
					}
				}
				return nil, fmt.Errorf("failed to check VLAN parent ifindex %d: %w", vlan.ParentIndex, err)
			}
		}
	}

	// Validate Master exists before any destructive operations
	if cfg.Master != "" {
		if _, err := util.GetNetLinkOps().LinkByName(cfg.Master); err != nil {
			if util.GetNetLinkOps().IsLinkNotFoundError(err) {
				return nil, &DependencyError{Dependency: cfg.Master, Reason: "master not found"}
			}
			return nil, fmt.Errorf("failed to check master %s: %w", cfg.Master, err)
		}
	}

	// No VLAN parent to resolve - return original config (no copy needed)
	if cfg.VLANParent == "" {
		return cfg, nil
	}

	// Resolve VLANParent name to ifindex
	parent, err := util.GetNetLinkOps().LinkByName(cfg.VLANParent)
	if err != nil {
		if util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return nil, &DependencyError{Dependency: cfg.VLANParent, Reason: "VLAN parent not found"}
		}
		return nil, fmt.Errorf("failed to check VLAN parent %s: %w", cfg.VLANParent, err)
	}

	// Defensive copy: don't mutate the stored config
	resolved := *cfg
	vlan := cfg.Link.(*netlink.Vlan) // Already validated above
	vlanCopy := *vlan
	vlanCopy.ParentIndex = parent.Attrs().Index
	resolved.Link = &vlanCopy

	return &resolved, nil
}

// applyDeviceConfig creates or updates a single device in the kernel.
// Ownership rules:
//   - If device doesn't exist: create it with our alias
//   - If device exists with our alias: update or recreate as needed
//   - If device exists without our alias: return NotOwnedError (could be human-created)
func applyDeviceConfig(name string, cfg *DeviceConfig) error {
	// Resolve all dependencies first. For the delete-then-recreate path, this ensures
	// we never delete an existing device unless all dependencies are present to recreate it.
	// For new devices, early failure here is equivalent to failure in createDevice() -
	// both return DependencyError and mark the config pending. But for existing devices,
	// failing after delete would leave us in a worse state (device gone, can't recreate).
	resolvedCfg, err := resolveDependencies(cfg)
	if err != nil {
		return err
	}

	// Check if device already exists
	link, err := util.GetNetLinkOps().LinkByName(name)
	if err == nil {
		// Device exists - verify ownership before modifying
		if !isOurDevice(link) {
			currentAlias := link.Attrs().Alias
			if currentAlias == "" {
				return &NotOwnedError{DeviceName: name, Reason: "no alias (may be externally managed)"}
			}
			return &NotOwnedError{DeviceName: name, Reason: fmt.Sprintf("foreign alias %q", currentAlias)}
		}

		// Check for critical mismatches (immutable attributes that require recreate)
		if hasCriticalMismatch(link, resolvedCfg) {
			klog.Warningf("NetlinkDeviceManager: device %s has critical config drift, recreating", name)
			if err := util.GetNetLinkOps().LinkDelete(link); err != nil {
				return fmt.Errorf("failed to delete mismatched device %s: %w", name, err)
			}
			// Fall through to create
		} else {
			// Device exists with correct critical attrs, update mutable attrs
			return updateDevice(link, resolvedCfg)
		}
	} else if !util.GetNetLinkOps().IsLinkNotFoundError(err) {
		return fmt.Errorf("failed to check device %s: %w", name, err)
	}

	// Device doesn't exist (or was just deleted), create it
	return createDevice(resolvedCfg)
}

// hasCriticalMismatch checks if the existing device has immutable attributes
// that differ from desired config. These require delete+recreate.
func hasCriticalMismatch(existing netlink.Link, cfg *DeviceConfig) bool {
	if cfg.Link == nil {
		return false
	}

	// Type mismatch is always critical
	if existing.Type() != cfg.Link.Type() {
		klog.V(4).Infof("NetlinkDeviceManager: type mismatch for %s: %s != %s",
			cfg.deviceName(), existing.Type(), cfg.Link.Type())
		return true
	}

	switch desired := cfg.Link.(type) {
	case *netlink.Vrf:
		// VRF table ID is immutable
		if e, ok := existing.(*netlink.Vrf); ok {
			if e.Table != desired.Table {
				klog.V(4).Infof("NetlinkDeviceManager: VRF %s table mismatch: %d != %d",
					cfg.deviceName(), e.Table, desired.Table)
				return true
			}
		}

	case *netlink.Bridge:
		// Bridge vlan_filtering and vlan_default_pvid are effectively immutable
		// (changing them causes disruption and loss of existing VLAN configuration).
		// We check for mismatch only when desired explicitly specifies a value (non-nil).
		// If desired is nil, we accept whatever the bridge currently has.
		if e, ok := existing.(*netlink.Bridge); ok {
			if desired.VlanFiltering != nil {
				existingFiltering := e.VlanFiltering != nil && *e.VlanFiltering
				if *desired.VlanFiltering != existingFiltering {
					klog.V(4).Infof("NetlinkDeviceManager: bridge %s vlan_filtering mismatch: %v != %v",
						cfg.deviceName(), existingFiltering, *desired.VlanFiltering)
					return true
				}
			}
			if desired.VlanDefaultPVID != nil {
				existingPVID := uint16(1) // kernel default is 1
				if e.VlanDefaultPVID != nil {
					existingPVID = *e.VlanDefaultPVID
				}
				if *desired.VlanDefaultPVID != existingPVID {
					klog.V(4).Infof("NetlinkDeviceManager: bridge %s vlan_default_pvid mismatch: %d != %d",
						cfg.deviceName(), existingPVID, *desired.VlanDefaultPVID)
					return true
				}
			}
		}

	case *netlink.Vxlan:
		// VXLAN VNI, src addr, port, FlowBased, VniFilter are immutable
		if e, ok := existing.(*netlink.Vxlan); ok {
			if e.VxlanId != desired.VxlanId {
				klog.V(4).Infof("NetlinkDeviceManager: VXLAN %s VNI mismatch: %d != %d",
					cfg.deviceName(), e.VxlanId, desired.VxlanId)
				return true
			}
			if desired.SrcAddr != nil && !e.SrcAddr.Equal(desired.SrcAddr) {
				klog.V(4).Infof("NetlinkDeviceManager: VXLAN %s src addr mismatch: %v != %v",
					cfg.deviceName(), e.SrcAddr, desired.SrcAddr)
				return true
			}
			if desired.Port > 0 && e.Port != desired.Port {
				klog.V(4).Infof("NetlinkDeviceManager: VXLAN %s port mismatch: %d != %d",
					cfg.deviceName(), e.Port, desired.Port)
				return true
			}
			if desired.FlowBased && !e.FlowBased {
				klog.V(4).Infof("NetlinkDeviceManager: VXLAN %s FlowBased mismatch: %v != %v",
					cfg.deviceName(), e.FlowBased, desired.FlowBased)
				return true
			}
			if desired.VniFilter && !e.VniFilter {
				klog.V(4).Infof("NetlinkDeviceManager: VXLAN %s VniFilter mismatch: %v != %v",
					cfg.deviceName(), e.VniFilter, desired.VniFilter)
				return true
			}
		}

	case *netlink.Vlan:
		// VLAN ID, ParentIndex, VlanProtocol, and HardwareAddr are immutable/critical
		// Note: cfg.Link has already been resolved by resolveDependencies(),
		// so ParentIndex contains the resolved ifindex.
		if e, ok := existing.(*netlink.Vlan); ok {
			if e.VlanId != desired.VlanId {
				klog.V(4).Infof("NetlinkDeviceManager: VLAN %s ID mismatch: %d != %d",
					cfg.deviceName(), e.VlanId, desired.VlanId)
				return true
			}
			if desired.ParentIndex > 0 && e.ParentIndex != desired.ParentIndex {
				klog.V(4).Infof("NetlinkDeviceManager: VLAN %s parent mismatch: ifindex %d != %d",
					cfg.deviceName(), e.ParentIndex, desired.ParentIndex)
				return true
			}
			if desired.VlanProtocol != 0 && e.VlanProtocol != desired.VlanProtocol {
				klog.V(4).Infof("NetlinkDeviceManager: VLAN %s protocol mismatch: %d != %d",
					cfg.deviceName(), e.VlanProtocol, desired.VlanProtocol)
				return true
			}
			desiredMAC := desired.Attrs().HardwareAddr
			if len(desiredMAC) > 0 && !bytes.Equal(e.Attrs().HardwareAddr, desiredMAC) {
				klog.V(4).Infof("NetlinkDeviceManager: VLAN %s MAC mismatch: %v != %v",
					cfg.deviceName(), e.Attrs().HardwareAddr, desiredMAC)
				return true
			}
		}
	}

	return false
}

// createDevice creates a new netlink device.
// Preconditions:
//   - Master (if specified) has been validated to exist
//   - VLANParent (if specified) has been resolved to ParentIndex
//     or alternatively ParentIndex (if specified) has been validated to exist
func createDevice(cfg *DeviceConfig) error {
	name := cfg.deviceName()

	// Creates the device
	link, err := createLink(cfg)
	if err != nil {
		return err
	}

	// Set alias for ownership tracking.
	// Without alias, the device becomes unmanageable (we won't recognize it as ours).
	if err := util.GetNetLinkOps().LinkSetAlias(link, cfg.alias()); err != nil {
		// Rollback: delete the device we just created
		klog.Errorf("NetlinkDeviceManager: failed to set alias on %s, rolling back: %v", name, err)
		if delErr := util.GetNetLinkOps().LinkDelete(link); delErr != nil {
			klog.Errorf("NetlinkDeviceManager: rollback failed, device %s may be orphaned: %v", name, delErr)
		}
		return fmt.Errorf("failed to set alias on device %s: %w", name, err)
	}

	// Set master if specified (Master existence already validated by resolveDependencies,
	// but it could be deleted between validation and now - treat as DependencyError for retry)
	if cfg.Master != "" {
		masterLink, err := util.GetNetLinkOps().LinkByName(cfg.Master)
		if err != nil {
			if util.GetNetLinkOps().IsLinkNotFoundError(err) {
				return &DependencyError{Dependency: cfg.Master, Reason: "master not found (deleted after validation)"}
			}
			return fmt.Errorf("failed to find master %s for device %s: %w", cfg.Master, name, err)
		}
		if err := util.GetNetLinkOps().LinkSetMaster(link, masterLink); err != nil {
			return fmt.Errorf("failed to set master %s for device %s: %w", cfg.Master, name, err)
		}
		klog.V(4).Infof("NetlinkDeviceManager: set master %s for device %s", cfg.Master, name)

		// Apply bridge port settings after attaching to master (required for settings to take effect)
		if cfg.BridgePortSettings != nil {
			if err := applyBridgePortSettings(name, *cfg.BridgePortSettings); err != nil {
				return fmt.Errorf("failed to apply bridge port settings for %s: %w", name, err)
			}
			klog.V(4).Infof("NetlinkDeviceManager: applied bridge port settings for device %s", name)
		}
	}

	// Bring the device up after creation
	if err := ensureDeviceUp(link); err != nil {
		return fmt.Errorf("failed to bring up %s: %w", name, err)
	}

	// Sync addresses if configured
	if err := syncAddresses(name, cfg); err != nil {
		return fmt.Errorf("failed to sync addresses on %s: %w", name, err)
	}

	klog.V(4).Infof("NetlinkDeviceManager: created device %s", name)
	return nil
}

// updateDevice updates an existing device to match config.
// Preconditions:
//   - Caller has verified ownership (device has our alias)
//   - Master (if specified) has been validated to exist
//
// This function reconciles mutable attributes that can be updated in-place:
//   - LinkAttrs (Alias, MTU, TxQLen, HardwareAddr, etc.) via LinkModify
//   - Master via LinkSetMaster (not handled by LinkModify)
//   - BridgePortSettings via bridge API (not handled by LinkModify)
//   - Up state via LinkSetUp
//
// Immutable attributes (VNI, SrcAddr, VlanId, etc.) are handled by hasCriticalMismatch
// which triggers delete+recreate instead.
func updateDevice(link netlink.Link, cfg *DeviceConfig) error {
	name := cfg.deviceName()
	currentAttrs := link.Attrs()

	// Only call LinkModify if there are actual differences to apply.
	// This prevents unnecessary netlink events.
	if needsLinkModify(link, cfg) {
		modifiedLink := prepareLinkForModify(link, cfg)
		if err := util.GetNetLinkOps().LinkModify(modifiedLink); err != nil {
			return fmt.Errorf("failed to modify link %s: %w", name, err)
		}
		klog.V(5).Infof("NetlinkDeviceManager: applied LinkModify for device %s", name)
	}

	// Check and update master (not handled by LinkModify).
	// Master existence already validated by preconditions, but it could be deleted
	// between validation and now - treat as DependencyError for retry.
	masterChanged := false
	if cfg.Master != "" {
		masterLink, err := util.GetNetLinkOps().LinkByName(cfg.Master)
		if err != nil {
			if util.GetNetLinkOps().IsLinkNotFoundError(err) {
				return &DependencyError{Dependency: cfg.Master, Reason: "master not found (deleted after validation)"}
			}
			return fmt.Errorf("failed to find master %s: %w", cfg.Master, err)
		}
		if currentAttrs.MasterIndex != masterLink.Attrs().Index {
			if err := util.GetNetLinkOps().LinkSetMaster(link, masterLink); err != nil {
				return fmt.Errorf("failed to set master %s for device %s: %w", cfg.Master, name, err)
			}
			masterChanged = true
			klog.V(4).Infof("NetlinkDeviceManager: updated master %s for device %s", cfg.Master, name)
		}
	}

	// Apply bridge port settings if configured (not handled by LinkModify).
	if err := ensureBridgePortSettings(name, cfg, masterChanged); err != nil {
		return err
	}

	if err := ensureDeviceUp(link); err != nil {
		return fmt.Errorf("failed to bring up %s: %w", name, err)
	}

	// Sync addresses if configured
	if err := syncAddresses(name, cfg); err != nil {
		return fmt.Errorf("failed to sync addresses on %s: %w", name, err)
	}

	return nil
}

// needsLinkModify checks if any mutable attributes differ between current link and desired config.
// Returns true if LinkModify should be called to reconcile differences.
// This prevents unnecessary LinkModify calls that would trigger netlink events.
func needsLinkModify(current netlink.Link, cfg *DeviceConfig) bool {
	// Check if alias differs (ownership marker, generated from config)
	if current.Attrs().Alias != cfg.alias() {
		return true
	}
	return !linkMutableFieldsEqual(current, cfg.Link)
}

// prepareLinkForModify creates a Link object suitable for LinkModify.
// It includes all mutable fields that linkMutableFieldsEqual checks.
func prepareLinkForModify(existing netlink.Link, cfg *DeviceConfig) netlink.Link {
	desiredAttrs := cfg.Link.Attrs()
	baseAttrs := netlink.LinkAttrs{
		Name:         desiredAttrs.Name,
		Index:        existing.Attrs().Index,
		MTU:          desiredAttrs.MTU,
		TxQLen:       desiredAttrs.TxQLen,
		HardwareAddr: desiredAttrs.HardwareAddr,
		Alias:        cfg.alias(),
	}

	// Handle type-specific mutable fields
	switch desired := cfg.Link.(type) {
	case *netlink.Vxlan:
		return &netlink.Vxlan{
			LinkAttrs: baseAttrs,
			Learning:  desired.Learning,
		}
	case *netlink.Bridge:
		return &netlink.Bridge{
			LinkAttrs:       baseAttrs,
			VlanFiltering:   desired.VlanFiltering,
			VlanDefaultPVID: desired.VlanDefaultPVID,
		}
	default:
		return &netlink.Device{LinkAttrs: baseAttrs}
	}
}

// deleteDevice removes a device from the kernel.
// Only deletes devices that have our alias prefix (ownership check).
func deleteDevice(name string) error {
	link, err := util.GetNetLinkOps().LinkByName(name)
	if util.GetNetLinkOps().IsLinkNotFoundError(err) {
		return nil // Already gone
	}
	if err != nil {
		return fmt.Errorf("failed to find device %s for deletion: %w", name, err)
	}

	// Safety check - only delete if it's ours
	if !isOurDevice(link) {
		alias := link.Attrs().Alias
		if alias == "" {
			return &NotOwnedError{DeviceName: name, Reason: "no alias (may be externally managed)"}
		}
		return &NotOwnedError{DeviceName: name, Reason: fmt.Sprintf("foreign alias %q", alias)}
	}

	if err := util.GetNetLinkOps().LinkDelete(link); err != nil {
		return fmt.Errorf("failed to delete device %s: %w", name, err)
	}
	klog.V(4).Infof("NetlinkDeviceManager: deleted device %s", name)
	return nil
}

// createLink creates a netlink device and returns the created link.
// The returned link has kernel-assigned attributes (ifindex, etc.).
func createLink(cfg *DeviceConfig) (netlink.Link, error) {
	name := cfg.deviceName()
	if err := util.GetNetLinkOps().LinkAdd(cfg.Link); err != nil {
		return nil, fmt.Errorf("failed to create device %s: %w", name, err)
	}
	// Fetch the created device to get kernel-assigned attributes
	link, err := util.GetNetLinkOps().LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("failed to get created device %s: %w", name, err)
	}
	return link, nil
}

// ensureDeviceUp brings a device up if it's not already.
func ensureDeviceUp(link netlink.Link) error {
	name := link.Attrs().Name
	// Re-fetch to get current state
	link, err := util.GetNetLinkOps().LinkByName(name)
	if err != nil {
		return fmt.Errorf("failed to get link %s: %w", name, err)
	}
	if link.Attrs().Flags&net.FlagUp != 0 {
		return nil // Already up
	}
	if err := util.GetNetLinkOps().LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to set link %s up: %w", name, err)
	}
	return nil
}

// syncAddresses ensures the device has exactly the desired addresses.
// If cfg.Addresses is nil, no address management is performed (existing addresses preserved).
// Link-local addresses (fe80::/10) are never removed automatically.
func syncAddresses(name string, cfg *DeviceConfig) error {
	if cfg.Addresses == nil {
		return nil // No address management requested
	}

	nlOps := util.GetNetLinkOps()

	link, err := nlOps.LinkByName(name)
	if err != nil {
		return fmt.Errorf("failed to get link %s for address sync: %w", name, err)
	}

	current, err := nlOps.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("failed to list addresses on %s: %w", name, err)
	}

	// Build lookup maps (key = "IP/prefix") and compute diff using sets
	desiredMap := addrListToMap(cfg.Addresses)
	currentMap := addrListToMap(current)

	desiredKeys := sets.KeySet(desiredMap)
	currentKeys := sets.KeySet(currentMap)

	toAdd := desiredKeys.Difference(currentKeys)
	toRemove := currentKeys.Difference(desiredKeys)

	var errs []error

	for key := range toAdd {
		addr := desiredMap[key]
		if err := nlOps.AddrAdd(link, addr); err != nil {
			// EEXIST is fine - address already exists (race or concurrent add)
			if !nlOps.IsAlreadyExistsError(err) {
				errs = append(errs, fmt.Errorf("failed to add address %s to %s: %w", key, name, err))
				continue
			}
		}
		klog.V(4).Infof("NetlinkDeviceManager: added address %s to %s", key, name)
	}

	for key := range toRemove {
		addr := currentMap[key]
		if isLinkLocalAddress(addr.IP) {
			continue // Never remove link-local addresses
		}
		if err := nlOps.AddrDel(link, addr); err != nil {
			// EADDRNOTAVAIL/ENOENT is fine - address already gone
			if !nlOps.IsEntryNotFoundError(err) {
				errs = append(errs, fmt.Errorf("failed to remove address %s from %s: %w", key, name, err))
				continue
			}
		}
		klog.V(4).Infof("NetlinkDeviceManager: removed address %s from %s", key, name)
	}

	if len(errs) > 0 {
		return fmt.Errorf("address sync errors on %s: %w", name, errors.Join(errs...))
	}
	return nil
}

// addrListToMap converts a slice of addresses to a map keyed by IPNet string.
func addrListToMap(addrs []netlink.Addr) map[string]*netlink.Addr {
	result := make(map[string]*netlink.Addr, len(addrs))
	for i := range addrs {
		addr := &addrs[i]
		if addr.IPNet != nil {
			result[addr.IPNet.String()] = addr
		}
	}
	return result
}

// isLinkLocalAddress returns true for IPv6 link-local addresses (fe80::/10).
// These addresses are kernel-managed and should not be removed automatically.
func isLinkLocalAddress(ip net.IP) bool {
	return ip != nil && ip.IsLinkLocalUnicast()
}

// getVIDVNIMappings retrieves current VID/VNI mappings for a specific VXLAN device.
// Uses `bridge -j vlan tunnelshow dev <name>` command for per-device filtering.
//
// Note: netlink.BridgeVlanTunnelShow() returns a flat list without ifindex,
// making it impossible to filter by device. We use the bridge command until
// the netlink library adds per-device tunnel info listing.
// TODO: Consider improving netlink.BridgeVlanTunnelShow().
func getVIDVNIMappings(vxlanName string) ([]VIDVNIMapping, error) {
	// Execute: bridge -j vlan tunnelshow dev <vxlanName>
	output, err := runBridgeCmd("-j", "vlan", "tunnelshow", "dev", vxlanName)
	if err != nil {
		// Device may not exist yet or no mappings - return empty
		klog.V(5).Infof("NetlinkDeviceManager: could not get bridge mappings for %s: %v", vxlanName, err)
		return nil, err
	}

	// Parse JSON output
	// Format: [{"ifname":"vxlan1","tunnels":[{"vlan":2,"vlanEnd":3,"tunid":10100,"tunidEnd":10101}]}]
	// When consecutive VIDs map to consecutive VNIs, they are collapsed into ranges.
	// Single mappings have vlan/tunid only (no vlanEnd/tunidEnd).
	var ports []struct {
		IfName  string `json:"ifname"`
		Tunnels []struct {
			VLAN     int `json:"vlan"`
			VLANEnd  int `json:"vlanEnd,omitempty"`
			TunID    int `json:"tunid"`
			TunIDEnd int `json:"tunidEnd,omitempty"`
		} `json:"tunnels"`
	}

	if err := json.Unmarshal([]byte(output), &ports); err != nil {
		klog.V(5).Infof("NetlinkDeviceManager: failed to parse bridge vlan tunnelshow JSON for %s: %v", vxlanName, err)
		return nil, err
	}

	// Find the port matching our device and expand ranges
	var mappings []VIDVNIMapping
	for _, port := range ports {
		if port.IfName != vxlanName {
			continue
		}
		for _, tunnel := range port.Tunnels {
			if tunnel.TunID == 0 {
				continue
			}
			// Handle ranges: vlanEnd/tunidEnd indicate the end of a range
			vlanEnd := tunnel.VLAN
			if tunnel.VLANEnd > 0 {
				vlanEnd = tunnel.VLANEnd
			}
			tunidEnd := tunnel.TunID
			if tunnel.TunIDEnd > 0 {
				tunidEnd = tunnel.TunIDEnd
			}
			// Expand the range into individual mappings
			for vid, vni := tunnel.VLAN, tunnel.TunID; vid <= vlanEnd && vni <= tunidEnd; vid, vni = vid+1, vni+1 {
				mappings = append(mappings, VIDVNIMapping{
					VID: uint16(vid),
					VNI: uint32(vni),
				})
			}
		}
	}

	klog.V(5).Infof("NetlinkDeviceManager: found %d existing VID/VNI mappings on %s", len(mappings), vxlanName)
	return mappings, nil
}

// validateMappingsUniqueness checks that all VIDs and VNIs are unique within the mappings.
// This is required because:
//   - Two VIDs mapping to the same VNI would cause removeVIDVNIMapping to delete the VNI filter
//     entry still needed by the other VID
//   - Duplicate VIDs would be ambiguous
func validateMappingsUniqueness(mappings []VIDVNIMapping) error {
	seenVIDs := make(map[uint16]bool)
	seenVNIs := make(map[uint32]bool)

	for _, m := range mappings {
		if seenVIDs[m.VID] {
			return fmt.Errorf("duplicate VID %d", m.VID)
		}
		if seenVNIs[m.VNI] {
			return fmt.Errorf("duplicate VNI %d", m.VNI)
		}
		seenVIDs[m.VID] = true
		seenVNIs[m.VNI] = true
	}
	return nil
}

// diffMappings computes the difference between current and desired mappings.
// Returns lists of mappings to add and remove.
func diffMappings(current, desired []VIDVNIMapping) (toAdd, toRemove []VIDVNIMapping) {
	currentSet := sets.New(current...)
	desiredSet := sets.New(desired...)

	return desiredSet.Difference(currentSet).UnsortedList(),
		currentSet.Difference(desiredSet).UnsortedList()
}

// addVIDVNIMapping adds a VID/VNI mapping to a VXLAN device.
// This function does NOT hold any locks - caller must ensure thread safety.
func addVIDVNIMapping(bridgeName, vxlanName string, m VIDVNIMapping) error {
	nlOps := util.GetNetLinkOps()

	// Get link objects for netlink calls
	bridgeLink, err := nlOps.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("failed to get bridge %s: %w", bridgeName, err)
	}
	vxlanLink, err := nlOps.LinkByName(vxlanName)
	if err != nil {
		return fmt.Errorf("failed to get VXLAN %s: %w", vxlanName, err)
	}

	// 1. Add VID to bridge with 'self' flag
	// This is required for the bridge to recognize the VLAN
	if err := nlOps.BridgeVlanAdd(bridgeLink, m.VID, false, false, true, false); err != nil {
		if !nlOps.IsAlreadyExistsError(err) {
			return fmt.Errorf("failed to add VID %d to bridge self: %w", m.VID, err)
		}
	}

	// 2. Add VID to VXLAN with 'master' flag
	if err := nlOps.BridgeVlanAdd(vxlanLink, m.VID, false, false, false, true); err != nil {
		if !nlOps.IsAlreadyExistsError(err) {
			return fmt.Errorf("failed to add VID %d to VXLAN: %w", m.VID, err)
		}
	}

	// 3. Add VNI to VNI filter
	// IMPORTANT: VNI must be explicitly added when VNI filtering is enabled
	if err := nlOps.BridgeVniAdd(vxlanLink, m.VNI); err != nil {
		if !nlOps.IsAlreadyExistsError(err) {
			return fmt.Errorf("failed to add VNI %d: %w", m.VNI, err)
		}
	}

	// 4. Add tunnel info (VID -> VNI mapping)
	if err := nlOps.BridgeVlanAddTunnelInfo(vxlanLink, m.VID, m.VNI, false, true); err != nil {
		if !nlOps.IsAlreadyExistsError(err) {
			return fmt.Errorf("failed to add VID->VNI mapping: %w", err)
		}
	}

	return nil
}

// removeVIDVNIMapping removes a VID/VNI mapping from a VXLAN device.
// Uses native netlink calls.
// This function does NOT hold any locks - caller must ensure thread safety.
// Note: Unlike addVIDVNIMapping, this does NOT remove the bridge self VID because:
// 1. Other VXLAN mappings or ports might still use that VID
// 2. Bridge self VIDs are automatically cleaned up by the kernel when the bridge is deleted
func removeVIDVNIMapping(vxlanName string, m VIDVNIMapping) error {
	nlOps := util.GetNetLinkOps()

	// Get link object for netlink calls
	vxlanLink, err := nlOps.LinkByName(vxlanName)
	if err != nil {
		if nlOps.IsLinkNotFoundError(err) {
			// Device already gone - nothing to remove (idempotent)
			klog.V(5).Infof("NetlinkDeviceManager: VXLAN %s not found for mapping removal, skipping", vxlanName)
			return nil
		}
		return fmt.Errorf("failed to get VXLAN %s for mapping removal: %w", vxlanName, err)
	}

	// Remove in reverse order of add for symmetry.
	var errs []error

	// 1. Remove tunnel_info mapping first
	if err := nlOps.BridgeVlanDelTunnelInfo(vxlanLink, m.VID, m.VNI, false, true); err != nil {
		if !nlOps.IsEntryNotFoundError(err) {
			errs = append(errs, fmt.Errorf("tunnel_info VID=%d->VNI=%d: %w", m.VID, m.VNI, err))
		}
	}

	// 2. Remove VNI from VNI filter
	if err := nlOps.BridgeVniDel(vxlanLink, m.VNI); err != nil {
		if !nlOps.IsEntryNotFoundError(err) {
			errs = append(errs, fmt.Errorf("VNI %d: %w", m.VNI, err))
		}
	}

	// 3. Remove VID from VXLAN
	if err := nlOps.BridgeVlanDel(vxlanLink, m.VID, false, false, false, true); err != nil {
		if !nlOps.IsEntryNotFoundError(err) {
			errs = append(errs, fmt.Errorf("VID %d: %w", m.VID, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to remove mapping from %s: %w", vxlanName, errors.Join(errs...))
	}
	return nil
}

// ensureBridgePortSettings applies bridge port settings if needed.
// Settings are applied if: master was just changed, or settings differ from current.
// If current settings can't be read, we skip to avoid loops (periodic reconciliation will retry).
func ensureBridgePortSettings(name string, cfg *DeviceConfig, masterChanged bool) error {
	if cfg.BridgePortSettings == nil || cfg.Master == "" {
		return nil
	}

	// Master just changed - always apply settings
	if masterChanged {
		if err := applyBridgePortSettings(name, *cfg.BridgePortSettings); err != nil {
			return fmt.Errorf("failed to apply bridge port settings for %s: %w", name, err)
		}
		klog.V(4).Infof("NetlinkDeviceManager: applied bridge port settings for device %s", name)
		return nil
	}

	// Check if settings differ from current
	current, err := getBridgePortSettings(name)
	if err != nil {
		// Can't read current settings - log and skip.
		// This can happen if device is not yet attached to a bridge.
		klog.V(5).Infof("NetlinkDeviceManager: could not read bridge port settings for %s (skipping comparison): %v", name, err)
		return nil
	}

	if !ptr.Equal(current, cfg.BridgePortSettings) {
		if err := applyBridgePortSettings(name, *cfg.BridgePortSettings); err != nil {
			return fmt.Errorf("failed to apply bridge port settings for %s: %w", name, err)
		}
		klog.V(4).Infof("NetlinkDeviceManager: applied bridge port settings for device %s", name)
	}

	return nil
}

// getBridgePortSettings retrieves current bridge port settings for a device.
// Uses native netlink Protinfo from the link attributes.
func getBridgePortSettings(linkName string) (*BridgePortSettings, error) {
	nlOps := util.GetNetLinkOps()

	link, err := nlOps.LinkByName(linkName)
	if err != nil {
		return nil, fmt.Errorf("failed to get link %s: %w", linkName, err)
	}

	// Use LinkGetProtinfo which performs a proper AF_BRIDGE dump to get bridge port info.
	// This is more reliable than link.Attrs().Protinfo which is not populated by LinkByName.
	protinfo, err := nlOps.LinkGetProtinfo(link)
	if err != nil {
		return nil, fmt.Errorf("failed to get bridge port info for %s: %w", linkName, err)
	}

	return &BridgePortSettings{
		VLANTunnel:    protinfo.VlanTunnel,
		NeighSuppress: protinfo.NeighSuppress,
		Learning:      protinfo.Learning,
	}, nil
}

// applyBridgePortSettings sets bridge port settings.
func applyBridgePortSettings(linkName string, settings BridgePortSettings) error {
	nlOps := util.GetNetLinkOps()

	link, err := nlOps.LinkByName(linkName)
	if err != nil {
		return fmt.Errorf("failed to get link %s: %w", linkName, err)
	}

	// Set VLAN tunnel mode
	if err := nlOps.LinkSetVlanTunnel(link, settings.VLANTunnel); err != nil {
		return fmt.Errorf("failed to set vlan_tunnel on %s: %w", linkName, err)
	}

	// Set neighbor suppress mode
	if err := nlOps.LinkSetBrNeighSuppress(link, settings.NeighSuppress); err != nil {
		return fmt.Errorf("failed to set neigh_suppress on %s: %w", linkName, err)
	}

	// Set learning mode
	if err := nlOps.LinkSetLearning(link, settings.Learning); err != nil {
		return fmt.Errorf("failed to set learning on %s: %w", linkName, err)
	}

	return nil
}

// getBridgePortVLAN retrieves current VLAN configuration for a specific VID on a port.
func getBridgePortVLAN(linkName string, vid int) (*BridgePortVLAN, error) {
	// Get the link
	link, err := util.GetNetLinkOps().LinkByName(linkName)
	if err != nil {
		return nil, fmt.Errorf("failed to get link %s: %w", linkName, err)
	}

	// Get VLAN list for this interface
	vlansMap, err := netlink.BridgeVlanList()
	if err != nil {
		return nil, fmt.Errorf("failed to get bridge vlan list: %w", err)
	}

	// Find VLANs for our interface
	vlans, ok := vlansMap[int32(link.Attrs().Index)]
	if !ok {
		return nil, fmt.Errorf("no vlan info for %s", linkName)
	}

	// Find the specific VID
	for _, vlan := range vlans {
		if int(vlan.Vid) == vid {
			return &BridgePortVLAN{
				VID:      vid,
				PVID:     vlan.PortVID(),
				Untagged: vlan.EngressUntag(),
			}, nil
		}
	}

	return nil, fmt.Errorf("VID %d not found on %s", vid, linkName)
}

// applyBridgePortVLAN adds a VLAN to a bridge port.
// Uses native netlink calls.
func applyBridgePortVLAN(linkName string, vlan BridgePortVLAN) error {
	nlOps := util.GetNetLinkOps()

	// Get the link
	link, err := nlOps.LinkByName(linkName)
	if err != nil {
		return fmt.Errorf("failed to get link %s: %w", linkName, err)
	}

	// Add VLAN with flags
	// BridgeVlanAdd(link, vid, pvid, untagged, self, master)
	// For port VLANs: self=false, master=true
	if err := nlOps.BridgeVlanAdd(link, uint16(vlan.VID), vlan.PVID, vlan.Untagged, false, true); err != nil {
		if !nlOps.IsAlreadyExistsError(err) {
			return fmt.Errorf("failed to add VLAN %d to port %s: %w", vlan.VID, linkName, err)
		}
	}
	return nil
}

// deviceName extracts the device name from a DeviceConfig
func (cfg *DeviceConfig) deviceName() string {
	if cfg.Link == nil {
		return ""
	}
	return cfg.Link.Attrs().Name
}

// deviceType returns the type of the link for alias generation
func (cfg *DeviceConfig) deviceType() string {
	if cfg.Link == nil {
		return "unknown"
	}
	return cfg.Link.Type()
}

// alias generates the ownership alias for this device.
// Format: "ovn-k8s-ndm:<type>:<name>" for debugging and collision avoidance.
func (cfg *DeviceConfig) alias() string {
	return ManagedAliasPrefix + cfg.deviceType() + ":" + cfg.deviceName()
}

// linkMutableFieldsEqual compares mutable link attributes that can be updated via LinkModify.
// This only checks fields that can be modified in-place; immutable fields like type are
// checked separately in configsEqual and hasCriticalMismatch.
func linkMutableFieldsEqual(a, b netlink.Link) bool {
	aAttrs := a.Attrs()
	bAttrs := b.Attrs()

	// Common mutable LinkAttrs
	// Only compare fields where b (desired) has an explicit non-zero value.
	// Zero value means "unspecified, don't care" - avoid triggering unnecessary
	// LinkModify calls that would keep generating netlink events.
	if bAttrs.MTU != 0 && aAttrs.MTU != bAttrs.MTU {
		return false
	}
	if bAttrs.TxQLen != 0 && aAttrs.TxQLen != bAttrs.TxQLen {
		return false
	}
	if len(bAttrs.HardwareAddr) > 0 && !bytes.Equal(aAttrs.HardwareAddr, bAttrs.HardwareAddr) {
		return false
	}

	// VXLAN-specific mutable fields
	aVxlan, aIsVxlan := a.(*netlink.Vxlan)
	bVxlan, bIsVxlan := b.(*netlink.Vxlan)
	if aIsVxlan && bIsVxlan {
		if aVxlan.Learning != bVxlan.Learning {
			return false
		}
	}

	// Bridge-specific mutable fields
	aBridge, aIsBridge := a.(*netlink.Bridge)
	bBridge, bIsBridge := b.(*netlink.Bridge)
	if aIsBridge && bIsBridge {
		if !ptr.Equal(aBridge.VlanFiltering, bBridge.VlanFiltering) ||
			!ptr.Equal(aBridge.VlanDefaultPVID, bBridge.VlanDefaultPVID) {
			return false
		}
	}

	return true
}

// configsEqual compares two DeviceConfigs for equality of stored configuration.
// Compares DeviceConfig fields and critical Link attributes that require device recreation.
// For VXLAN devices, this includes SrcAddr, Port, and VxlanId which cannot be changed in-place.
func configsEqual(a, b *DeviceConfig) bool {
	if a == nil || b == nil {
		return a == b
	}

	// Compare DeviceConfig fields
	if a.deviceName() != b.deviceName() ||
		a.Master != b.Master ||
		a.VLANParent != b.VLANParent ||
		!ptr.Equal(a.BridgePortSettings, b.BridgePortSettings) {
		return false
	}

	// Link type must match (immutable - requires recreation if different)
	if a.Link.Type() != b.Link.Type() {
		return false
	}

	// Compare mutable link attributes
	if !linkMutableFieldsEqual(a.Link, b.Link) {
		return false
	}

	// Compare VXLAN immutable fields
	aVxlan, aIsVxlan := a.Link.(*netlink.Vxlan)
	bVxlan, bIsVxlan := b.Link.(*netlink.Vxlan)
	if aIsVxlan != bIsVxlan {
		return false
	}
	if aIsVxlan && bIsVxlan {
		if !aVxlan.SrcAddr.Equal(bVxlan.SrcAddr) ||
			aVxlan.Port != bVxlan.Port ||
			aVxlan.VxlanId != bVxlan.VxlanId ||
			aVxlan.FlowBased != bVxlan.FlowBased ||
			aVxlan.VniFilter != bVxlan.VniFilter {
			return false
		}
	}

	// Compare VLAN-specific fields (can't use struct comparison: contains maps)
	aVlan, aIsVlan := a.Link.(*netlink.Vlan)
	bVlan, bIsVlan := b.Link.(*netlink.Vlan)
	if aIsVlan != bIsVlan {
		return false
	}
	if aIsVlan && bIsVlan {
		if aVlan.VlanId != bVlan.VlanId {
			return false
		}
		// For legacy style (VLANParent == ""), compare ParentIndex directly.
		// When VLANParent is set, the name comparison above is sufficient
		// since ParentIndex is resolved at apply time.
		if a.VLANParent == "" && b.VLANParent == "" && aVlan.ParentIndex != bVlan.ParentIndex {
			return false
		}
	}

	// Compare Addresses (nil vs non-nil is significant)
	if !addressesEqual(a.Addresses, b.Addresses) {
		return false
	}

	return true
}

// addressesEqual compares two address slices for equality.
// Two slices are equal if they have the same addresses (by IPNet string).
// nil and empty slice are treated as different (nil = no management, empty = want no addresses).
func addressesEqual(a, b []netlink.Addr) bool {
	// nil check: nil means "don't manage", empty means "manage but want none"
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil && b == nil {
		return true
	}
	return sets.KeySet(addrListToMap(a)).Equal(sets.KeySet(addrListToMap(b)))
}

// isDependencyError checks if the error is a dependency error
func isDependencyError(err error) bool {
	return errors.Is(err, ErrDependencyPending)
}

// ErrDependencyPending is a sentinel error indicating a dependency is not ready.
// Callers can check for this to distinguish "will auto-resolve" from real failures.
var ErrDependencyPending = errors.New("dependency pending")

// DependencyError indicates a device couldn't be created because a dependency is missing.
// Wraps ErrDependencyPending for errors.Is() compatibility.
type DependencyError struct {
	Dependency string
	Reason     string
}

func (e *DependencyError) Error() string {
	return fmt.Sprintf("dependency not ready: %s (%s)", e.Dependency, e.Reason)
}

// Unwrap allows errors.Is(err, ErrDependencyPending) to work
func (e *DependencyError) Unwrap() error {
	return ErrDependencyPending
}

// runBridgeCmd executes a bridge command and returns stdout.
// bridgeCmdTimeout is the maximum time to wait for a bridge command to complete.
// This prevents the controller from hanging indefinitely if the subprocess stalls.
const bridgeCmdTimeout = 10 * time.Second

func runBridgeCmd(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bridgeCmdTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bridge", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("bridge %v timed out after %v", args, bridgeCmdTimeout)
		}
		return "", fmt.Errorf("bridge %v failed: %w, output: %s", args, err, output)
	}
	return string(output), nil
}
