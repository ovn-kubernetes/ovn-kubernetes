package netlinkdevicemanager

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vishvananda/netlink"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// DeviceState represents the lifecycle state of a managed device.
type DeviceState string

const (
	DeviceStateUnknown DeviceState = ""        // Not in store (never declared or already deleted)
	DeviceStateReady   DeviceState = "Ready"   // Device matches desired state in kernel
	DeviceStatePending DeviceState = "Pending" // Waiting for dependency (master, VLANParent)
	DeviceStateFailed  DeviceState = "Failed"  // Transient kernel error (will retry with backoff)
	DeviceStateBlocked DeviceState = "Blocked" // External device conflict (NotOwnedError)
)

// DeviceReconciler is notified when device state transitions.
// Implementations should re-queue their own work, not do heavy processing inline.
type DeviceReconciler interface {
	ReconcileDevice(key string) error
}

// ManagedAliasPrefix is the prefix used in IFLA_IFALIAS to mark devices managed by this controller.
// This allows safe cleanup: only delete devices with this prefix.
// Format: "ovn-k8s-ndm:<type>:<name>" for debugging and collision avoidance.
const ManagedAliasPrefix = "ovn-k8s-ndm:"

// MaxInterfaceNameLength is the maximum length for Linux interface names.
// Linux's IFNAMSIZ is 16 (including null terminator), so max usable length is 15.
const MaxInterfaceNameLength = 15

// validateInterfaceName checks if an interface name is valid for Linux.
// Returns an error if the name is empty, exceeds IFNAMSIZ-1, contains
// characters rejected by the kernel (/, NUL, whitespace), or is reserved.
func validateInterfaceName(name, context string) error {
	if name == "" {
		return fmt.Errorf("%s name is empty", context)
	}
	if len(name) > MaxInterfaceNameLength {
		return fmt.Errorf("%s name %q exceeds maximum length of %d characters (got %d)",
			context, name, MaxInterfaceNameLength, len(name))
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%s name %q is reserved", context, name)
	}
	if strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("%s name %q contains invalid characters", context, name)
	}
	for _, r := range name {
		if r == ' ' || r == '\t' || r == '\n' {
			return fmt.Errorf("%s name %q contains whitespace", context, name)
		}
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

// Key prefixes for workqueue item type routing.
// Workqueue deduplicates by key, so rapid updates to the same device coalesce.
const (
	deviceKeyPrefix   = "device/"   // e.g., "device/br-evpn"
	mappingsKeyPrefix = "mappings/" // e.g., "mappings/vxlan0"
	fullSyncKey       = "fullsync"  // Startup: orphan cleanup + re-enqueue all
	syncKey           = "sync"      // Periodic: re-enqueue all (no orphan scan)
)

// DeviceConfig represents the complete desired configuration for a network device.
// Controllers provide the FULL configuration; manager enforces EXACTLY what's provided.
type DeviceConfig struct {
	// Link is the netlink device (Bridge, Vxlan, Vlan, Device, etc.)
	// Must include all desired attributes in LinkAttrs (Name, HardwareAddr, etc.)
	// IMPORTANT: Use netlink.NewLinkAttrs() to create a new LinkAttrs struct with default values.
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
	VID      uint16 // VLAN ID (1-4094)
	PVID     bool   // Set as Port VLAN ID (native VLAN)
	Untagged bool   // Egress untagged
}

// managedDevice tracks a device with its config and status
type managedDevice struct {
	cfg       DeviceConfig // Complete desired config
	state     DeviceState  // Lifecycle state (Ready, Pending, Failed, Blocked)
	lastError error        // Last error from reconciliation (preserved for status/debug)
}

// managedVIDVNIMappings tracks VID/VNI mappings for a VXLAN device
type managedVIDVNIMappings struct {
	bridgeName string          // Parent bridge name (for self VLAN)
	vxlanName  string          // VXLAN device name
	mappings   []VIDVNIMapping // Desired mappings
}

// Controller manages Linux network device lifecycle using a workqueue-based reconciler.
// Public API methods store desired state and enqueue work; a single worker goroutine
// performs all netlink I/O. Self-heals via periodic sync, orphan cleanup, and netlink
// event-driven reconciliation.
type Controller struct {
	mu    sync.RWMutex
	store map[string]*managedDevice // device name -> managed device info

	reconciler  controller.Reconciler // workqueue reconciler (single worker, all I/O)
	subscribers []DeviceReconciler    // registered before Run(), immutable after
	started     atomic.Bool           // set once Run() begins; prevents late subscriber registration

	// ReconcilePeriod is the interval for periodic sync as a safety net.
	// Defaults to DefaultReconcilePeriod. Can be overridden before calling Run().
	ReconcilePeriod time.Duration

	// Stores for additional configuration on managed and external devices.

	// vidVNIMappingStore: VID/VNI tunnel mappings on VXLAN devices managed by NDM (via EnsureLink).
	vidVNIMappingStore map[string]*managedVIDVNIMappings // vxlanName -> mappings
}

// NewController creates a new NetlinkDeviceManager with default settings.
// The ReconcilePeriod can be overridden before calling Run() if needed.
func NewController() *Controller {
	c := &Controller{
		store:              make(map[string]*managedDevice),
		ReconcilePeriod:    DefaultReconcilePeriod,
		vidVNIMappingStore: make(map[string]*managedVIDVNIMappings),
	}

	c.reconciler = controller.NewReconciler("netlink-device-manager", &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   c.reconcileWorkqueue,
		Threadiness: 1,                           // Single worker — serializes all netlink I/O
		MaxAttempts: controller.InfiniteAttempts, // Self-healing: infinite retries
	})

	return c
}

// reconcileWorkqueue routes workqueue items to appropriate handlers based on key prefix.
// This is the single entry point for all I/O — called by the workqueue worker goroutine.
func (c *Controller) reconcileWorkqueue(key string) error {
	klog.V(5).Infof("NetlinkDeviceManager: reconciling %s", key)
	switch {
	case strings.HasPrefix(key, deviceKeyPrefix):
		return c.reconcileDeviceKey(strings.TrimPrefix(key, deviceKeyPrefix))
	case strings.HasPrefix(key, mappingsKeyPrefix):
		return c.reconcileMappingsKey(strings.TrimPrefix(key, mappingsKeyPrefix))
	case key == fullSyncKey:
		return c.reconcileFullSyncKey()
	case key == syncKey:
		return c.reconcileSyncKey()
	default:
		klog.Warningf("NetlinkDeviceManager: unknown reconcile key: %s", key)
		return nil
	}
}

// reconcileDeviceKey is the core device reconciler.
// Handles both create/update (device in store) and delete (device not in store).
// Pattern: Lock → copy config → Unlock → I/O outside lock → Lock → update state → Unlock → notify.
func (c *Controller) reconcileDeviceKey(name string) error {
	// Read config under RLock
	c.mu.RLock()
	unlock := sync.OnceFunc(c.mu.RUnlock)
	defer unlock()

	device, exists := c.store[name]
	if !exists {
		unlock()
		// Not desired — delete from kernel if present.
		err := deleteDevice(name)
		if err != nil && !IsNotOwnedError(err) {
			return err // rate-limited retry
		}
		c.notifySubscribers(name)
		return nil
	}
	// Interface copy: cfg.Link copies the interface value (type + data pointer).
	// The underlying concrete struct (*netlink.Bridge, etc.) is NOT mutated by
	// applyDeviceConfig — resolveDependencies creates a defensive copy for VLANs,
	// and all other paths only read from cfg.Link. If a concurrent EnsureLink
	// replaces device.cfg with a new Link pointer, the old concrete struct remains
	// valid (Go GC keeps it alive).
	cfg := device.cfg
	previousState := device.state
	unlock()

	// All netlink I/O OUTSIDE lock
	err := applyDeviceConfig(name, &cfg)

	// Update state under Lock
	c.mu.Lock()
	unlock = sync.OnceFunc(c.mu.Unlock)
	defer unlock()

	device, stillExists := c.store[name]
	if !stillExists {
		return nil // Deleted during I/O — re-queued key will handle delete
	}

	// Config staleness guard: if config changed while we were doing I/O
	// (concurrent EnsureLink replaced device.cfg), skip state update.
	// The new config's EnsureLink already enqueued a fresh reconcile that
	// will read the current config and apply it. Without this guard, we'd
	// set state to Ready/Failed based on the OLD config's result, which is
	// a correctness bug (state would briefly lie about a config never applied).
	if !configsEqual(&cfg, &device.cfg) {
		return nil
	}

	var newState DeviceState
	var reconcileErr error

	switch {
	case err == nil:
		newState = DeviceStateReady
		device.lastError = nil
	case isDependencyError(err):
		newState = DeviceStatePending
		device.lastError = err // Preserve reason: "pending on X (reason)" for status/debug
		// return nil: don't retry via rate-limited backoff
		// Will be re-triggered by netlink event when dependency appears
	case IsNotOwnedError(err):
		newState = DeviceStateBlocked
		device.lastError = err
		// return nil: permanent condition, no point retrying
		// Will be re-triggered by netlink event when external device removed
	default:
		newState = DeviceStateFailed
		device.lastError = err
		reconcileErr = err // return error → rate-limited retry via workqueue
	}

	device.state = newState
	unlock()

	// Notify subscribers OUTSIDE lock (avoids deadlock if subscriber calls GetDeviceState)
	if previousState != newState {
		c.notifySubscribers(name)
	}

	return reconcileErr
}

// reconcileMappingsKey reconciles VID/VNI mappings for a VXLAN device.
//
// Each VID/VNI mapping consists of four kernel components:
//  1. Bridge self VLAN (on the bridge device)
//  2. VXLAN VID membership (on the VXLAN bridge port)
//  3. VNI filter entry (on the VXLAN device)
//  4. Tunnel-info (VID→VNI mapping on the VXLAN bridge port)
//
// For removals, we diff against current tunnel-info (the only queryable component).
// For additions, we always ensure ALL desired mappings on every cycle rather than
// relying on tunnel-info as a proxy for full state. This is critical because the
// other three components can be independently removed (e.g., bridge self VLAN deleted
// externally) while tunnel-info remains intact, and addVIDVNIMapping is idempotent
// (handles EEXIST for each component).
func (c *Controller) reconcileMappingsKey(vxlanName string) error {
	// Copy desired state under RLock (read-only snapshot)
	c.mu.RLock()
	unlock := sync.OnceFunc(c.mu.RUnlock)
	defer unlock()

	m, exists := c.vidVNIMappingStore[vxlanName]
	if !exists {
		return nil
	}
	bridgeName := m.bridgeName
	desiredMappings := slices.Clone(m.mappings)
	unlock()

	// All I/O outside lock
	nlOps := util.GetNetLinkOps()
	vxlanLink, err := nlOps.LinkByName(vxlanName)
	if err != nil {
		if nlOps.IsLinkNotFoundError(err) {
			klog.V(5).Infof("NetlinkDeviceManager: VXLAN %s not found for mappings, will retry on link event", vxlanName)
			return nil
		}
		return fmt.Errorf("failed to get VXLAN %s for mappings: %w", vxlanName, err)
	}
	bridgeLink, err := nlOps.LinkByName(bridgeName)
	if err != nil {
		if nlOps.IsLinkNotFoundError(err) {
			klog.V(5).Infof("NetlinkDeviceManager: bridge %s not found for mappings, will retry on link event", bridgeName)
			return nil
		}
		return fmt.Errorf("failed to get bridge %s for mappings: %w", bridgeName, err)
	}

	// Read current tunnel-info solely to detect stale mappings that need removal.
	current, err := getVIDVNIMappings(vxlanName)
	if err != nil {
		if nlOps.IsLinkNotFoundError(err) {
			klog.V(5).Infof("NetlinkDeviceManager: VXLAN %s not found for mappings, will retry on link event", vxlanName)
			return nil
		}
		return fmt.Errorf("failed to read current mappings for %s: %w", vxlanName, err)
	}

	_, toRemove := diffMappings(current, desiredMappings)

	var errs []error

	// Remove stale mappings first (before ensures, to handle VID→VNI changes).
	for _, mapping := range toRemove {
		if err := removeVIDVNIMapping(vxlanLink, mapping); err != nil {
			klog.Warningf("NetlinkDeviceManager: failed to remove mapping VID=%d VNI=%d from %s: %v",
				mapping.VID, mapping.VNI, vxlanName, err)
			errs = append(errs, err)
		}
	}

	// Ensure all desired mappings regardless of what tunnel-info reports.
	for _, mapping := range desiredMappings {
		if err := addVIDVNIMapping(bridgeLink, vxlanLink, mapping); err != nil {
			klog.Warningf("NetlinkDeviceManager: failed to ensure mapping VID=%d VNI=%d on %s: %v",
				mapping.VID, mapping.VNI, vxlanName, err)
			errs = append(errs, err)
		}
	}

	if len(desiredMappings) > 0 || len(toRemove) > 0 {
		klog.V(4).Infof("NetlinkDeviceManager: mappings %s (ensured=%d, removed=%d, errors=%d)",
			vxlanName, len(desiredMappings), len(toRemove), len(errs))
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to apply %d mappings on %s: %w", len(errs), vxlanName, errors.Join(errs...))
	}
	return nil
}

// cleanupOrphanedDevices scans kernel for devices with our alias that are NOT in the
// desired state store, and deletes them. Used during startup/resubscribe.
//
// Note: a device could be re-desired (via EnsureLink) between the scan and the delete.
// We don't re-check for this — if we delete a re-desired device, the worker will
// recreate it immediately since EnsureLink already enqueued the key.
func (c *Controller) cleanupOrphanedDevices() error {
	// Scan kernel — I/O, no lock needed
	links, err := util.GetNetLinkOps().LinkList()
	if err != nil {
		return fmt.Errorf("failed to list links: %w", err)
	}

	// Find orphans — check store under RLock (read-only check)
	c.mu.RLock()
	var orphans []netlink.Link
	for _, link := range links {
		if isOurDevice(link) {
			name := link.Attrs().Name
			if _, desired := c.store[name]; !desired {
				orphans = append(orphans, link)
			}
		}
	}
	c.mu.RUnlock()

	// Delete orphans
	var deleted int
	for _, link := range orphans {
		name := link.Attrs().Name
		klog.V(4).Infof("NetlinkDeviceManager: deleting orphaned device %s", name)
		if err := util.GetNetLinkOps().LinkDelete(link); err != nil {
			klog.Errorf("NetlinkDeviceManager: failed to delete orphan %s: %v", name, err)
		} else {
			deleted++
		}
	}
	if deleted > 0 {
		klog.Infof("NetlinkDeviceManager: cleaned up %d orphaned devices", deleted)
	}
	return nil
}

// reconcileFullSyncKey runs orphan cleanup then re-enqueues all items.
// Used at startup and after netlink resubscribe.
func (c *Controller) reconcileFullSyncKey() error {
	if err := c.cleanupOrphanedDevices(); err != nil {
		klog.Errorf("NetlinkDeviceManager: orphan cleanup failed: %v", err)
		// Continue — enqueue items anyway
	}
	return c.reconcileSyncKey()
}

// reconcileSyncKey re-enqueues all stored items for individual reconciliation.
// Used for periodic sync. Does NOT do orphan cleanup (that's fullsync).
func (c *Controller) reconcileSyncKey() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for name := range c.store {
		c.reconciler.Reconcile(deviceKeyPrefix + name)
	}
	for vxlanName := range c.vidVNIMappingStore {
		c.reconciler.Reconcile(mappingsKeyPrefix + vxlanName)
	}
	return nil
}

// EnsureLink stores the desired device configuration and enqueues it for reconciliation.
//
// INVARIANT: This relies on MaxAttempts = InfiniteAttempts. If MaxAttempts
// were finite, a Failed device with unchanged config could be dropped from
// the workqueue and never retried (periodic sync would eventually catch it,
// but the gap could be up to ReconcilePeriod).
func (c *Controller) EnsureLink(cfg DeviceConfig) error {
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

	// Defensive copy: prevent data race if caller mutates the slice after return.
	cfg.Addresses = slices.Clone(cfg.Addresses)

	c.mu.Lock()
	unlock := sync.OnceFunc(c.mu.Unlock)
	defer unlock()

	if existing := c.store[name]; existing != nil {
		if configsEqual(&existing.cfg, &cfg) {
			// Config unchanged. Re-enqueue only for Blocked devices — the caller
			// may know the external conflict was resolved and wants to force retry.
			// Don't re-enqueue Failed — the workqueue already has it in rate-limited
			// backoff. Reconcile() bypasses the rate limiter (queue.Add), so calling
			// it here would reset the backoff and cause rapid retries.
			if existing.state == DeviceStateBlocked {
				unlock()
				c.reconciler.Reconcile(deviceKeyPrefix + name)
			}
			return nil
		}
	}

	// Store desired state and enqueue
	c.store[name] = &managedDevice{cfg: cfg, state: DeviceStatePending}
	unlock()

	c.reconciler.Reconcile(deviceKeyPrefix + name)
	return nil
}

// DeleteLink removes a device from the desired state and enqueues reconciliation.
// The worker will see the device absent from store and delete it from the kernel.
func (c *Controller) DeleteLink(name string) error {
	c.mu.Lock()
	unlock := sync.OnceFunc(c.mu.Unlock)
	defer unlock()

	_, wasManaged := c.store[name]
	if !wasManaged {
		return nil
	}

	delete(c.store, name)
	// Remove VID/VNI mappings for the device.
	delete(c.vidVNIMappingStore, name)
	unlock()

	c.reconciler.Reconcile(deviceKeyPrefix + name)
	return nil
}

// Has checks if a device is registered in the desired state.
func (c *Controller) Has(name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.store[name]
	return ok
}

// GetConfig returns the config for a managed device, or nil if not managed.
func (c *Controller) GetConfig(name string) *DeviceConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	existing := c.store[name]
	if existing == nil {
		return nil
	}
	cfgCopy := existing.cfg
	return &cfgCopy
}

// ListDevicesByVLANParent returns configs for all devices with the given VLANParent.
func (c *Controller) ListDevicesByVLANParent(parentName string) []DeviceConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var result []DeviceConfig
	for _, device := range c.store {
		if device.cfg.VLANParent == parentName {
			cfgCopy := device.cfg
			result = append(result, cfgCopy)
		}
	}
	return result
}

// IsDeviceReady returns true if the device exists in store and is in Ready state.
func (c *Controller) IsDeviceReady(name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if d, ok := c.store[name]; ok {
		return d.state == DeviceStateReady
	}
	return false
}

// GetDeviceState returns the current state of a managed device.
// Returns DeviceStateUnknown if the device is not in the store
// (never declared via EnsureLink, or already removed via DeleteLink).
func (c *Controller) GetDeviceState(name string) DeviceState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if d, ok := c.store[name]; ok {
		return d.state
	}
	return DeviceStateUnknown
}

// RegisterDeviceReconciler registers a reconciler to be notified on device state transitions.
// Must be called before Run(). After Run(), the subscriber list is immutable.
// Panics if called after Run() to surface programming errors early.
func (c *Controller) RegisterDeviceReconciler(r DeviceReconciler) {
	if c.started.Load() {
		panic("RegisterDeviceReconciler called after Run()")
	}
	c.subscribers = append(c.subscribers, r)
}

// notifySubscribers calls ReconcileDevice(name) on all registered subscribers.
// Called from the worker goroutine after state transitions. No lock needed —
// subscriber slice is immutable after Run().
// MUST be called OUTSIDE c.mu to avoid deadlock (subscribers may call GetDeviceState).
func (c *Controller) notifySubscribers(name string) {
	for _, sub := range c.subscribers {
		if err := sub.ReconcileDevice(name); err != nil {
			klog.Warningf("NetlinkDeviceManager: subscriber error for device %s: %v", name, err)
		}
	}
}

// EnsureBridgeMappings ensures a VXLAN device has exactly the specified VID/VNI mappings.
// It also ensures the bridge has the corresponding VLANs configured with 'self' flag.
//
// Semantics:
// - Provide ALL desired mappings (full-state, not incremental)
// - Manager stores desired state and enqueues reconciliation
// - Worker computes diff between current and desired, adds/removes as needed
// - Returns nil on success (intent stored). Validation errors returned immediately.
//
// Constraints:
// - Each VNI must be unique within the mappings (no two VIDs mapping to the same VNI)
// - Each VID must be unique within the mappings
func (c *Controller) EnsureBridgeMappings(bridgeName, vxlanName string, mappings []VIDVNIMapping) error {
	if err := validateInterfaceName(bridgeName, "bridge"); err != nil {
		return err
	}
	if err := validateInterfaceName(vxlanName, "vxlan"); err != nil {
		return err
	}
	if err := validateMappings(mappings); err != nil {
		return fmt.Errorf("invalid mappings for %s: %w", vxlanName, err)
	}

	// Copy the mappings slice to avoid races if caller mutates the original
	mappingsCopy := slices.Clone(mappings)

	c.mu.Lock()
	c.vidVNIMappingStore[vxlanName] = &managedVIDVNIMappings{
		bridgeName: bridgeName,
		vxlanName:  vxlanName,
		mappings:   mappingsCopy,
	}
	c.mu.Unlock()

	c.reconciler.Reconcile(mappingsKeyPrefix + vxlanName)
	return nil
}

// linkChanBufferSize is the buffer size for the netlink event channel.
// The buffer absorbs bursts of kernel events (e.g., during startup or bulk
// reconfiguration) while the event loop is busy. If the buffer overflows,
// events are lost — the periodic sync is the safety net for missed events.
const linkChanBufferSize = 100

// Run starts the controller's workqueue reconciler and netlink event listener.
// Controllers should call EnsureLink for all desired devices BEFORE calling Run().
func (c *Controller) Run(stopCh <-chan struct{}, doneWg *sync.WaitGroup) error {
	c.started.Store(true)
	reconcilePeriod := c.ReconcilePeriod

	// Subscribe to netlink events BEFORE starting workers.
	// This ensures no events are missed between worker startup and subscription.
	linkSubscribeOptions := netlink.LinkSubscribeOptions{
		ErrorCallback: func(err error) {
			klog.Errorf("NetlinkDeviceManager: netlink subscribe error: %v", err)
		},
	}

	linkChan := make(chan netlink.LinkUpdate, linkChanBufferSize)
	subscribed := false
	if err := netlink.LinkSubscribeWithOptions(linkChan, stopCh, linkSubscribeOptions); err != nil {
		klog.Errorf("NetlinkDeviceManager: initial netlink subscribe failed: %v", err)
	} else {
		subscribed = true
	}

	// Start reconciler with orphan cleanup as initial sync.
	if err := controller.StartWithInitialSync(
		c.cleanupOrphanedDevices,
		c.reconciler,
	); err != nil {
		return fmt.Errorf("failed to start reconciler: %w", err)
	}

	// Queue initial sync (not fullsync — orphan cleanup already ran via StartWithInitialSync)
	c.reconciler.Reconcile(syncKey)

	// Single event loop goroutine — all shared state (linkChan, subscribed) stays
	// in one goroutine, avoiding data races.
	doneWg.Add(1)
	go func() {
		defer doneWg.Done()
		defer controller.Stop(c.reconciler)

		syncTimer := time.NewTicker(reconcilePeriod)
		defer syncTimer.Stop()

		for {
			// Exit immediately if stopCh is closed
			// Handle race condition between stopCh and events.
			select {
			case <-stopCh:
				klog.Info("NetlinkDeviceManager: stopping")
				return
			default:
			}

			select {
			case update, ok := <-linkChan:
				// Note: we do NOT reset the periodic sync timer on link events.
				// Resetting would starve periodic sync on busy nodes with many
				// netlink events. The periodic sync is a hard safety net.
				if !ok {
					klog.Warning("NetlinkDeviceManager: netlink channel closed, resubscribing")
					subscribed = false
					newCh := make(chan netlink.LinkUpdate, linkChanBufferSize)
					if err := netlink.LinkSubscribeWithOptions(newCh, stopCh, linkSubscribeOptions); err != nil {
						klog.Errorf("NetlinkDeviceManager: resubscribe failed: %v", err)
						// Assign new blocking channel to prevent spin on closed channel
						linkChan = make(chan netlink.LinkUpdate)
					} else {
						linkChan = newCh
						subscribed = true
						c.reconciler.Reconcile(fullSyncKey)
					}
					continue
				}
				c.handleLinkUpdate(update.Link)

			case <-syncTimer.C:
				klog.V(5).Info("NetlinkDeviceManager: periodic sync")
				c.reconciler.Reconcile(syncKey)
				if !subscribed {
					newCh := make(chan netlink.LinkUpdate, linkChanBufferSize)
					if err := netlink.LinkSubscribeWithOptions(newCh, stopCh, linkSubscribeOptions); err != nil {
						klog.Errorf("NetlinkDeviceManager: resubscribe failed: %v", err)
					} else {
						linkChan = newCh
						subscribed = true
						c.reconciler.Reconcile(fullSyncKey)
					}
				}

			case <-stopCh:
				klog.Info("NetlinkDeviceManager: stopping")
				return
			}
		}
	}()

	klog.Info("NetlinkDeviceManager: running")
	return nil
}

// handleLinkUpdate enqueues reconciliation for devices affected by a netlink event.
func (c *Controller) handleLinkUpdate(link netlink.Link) {
	linkName := link.Attrs().Name
	klog.V(5).Infof("NetlinkDeviceManager: link update for %s", linkName)

	c.mu.RLock()
	defer c.mu.RUnlock()

	// Queue the device itself for reconciliation
	if _, exists := c.store[linkName]; exists {
		c.reconciler.Reconcile(deviceKeyPrefix + linkName)
	}

	// Queue devices that depend on this link.
	for name, device := range c.store {
		// Skip failed devices, they are already in rate-limited backoff.
		if device.state != DeviceStateFailed {
			if device.cfg.Master == linkName || device.cfg.VLANParent == linkName {
				c.reconciler.Reconcile(deviceKeyPrefix + name)
			}
		}
	}

	// Queue mappings for affected VXLANs
	if _, exists := c.vidVNIMappingStore[linkName]; exists {
		c.reconciler.Reconcile(mappingsKeyPrefix + linkName)
	}
	// If the link is a bridge, queue mappings for all VXLANs on the bridge.
	for vxlanName, m := range c.vidVNIMappingStore {
		if m.bridgeName == linkName {
			c.reconciler.Reconcile(mappingsKeyPrefix + vxlanName)
		}
	}
}

// requireLink looks up a netlink device by name, returning a DependencyError if missing.
func requireLink(name string) (netlink.Link, error) {
	link, err := util.GetNetLinkOps().LinkByName(name)
	if err != nil {
		if util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return nil, &DependencyError{Dependency: name, Reason: "not found"}
		}
		return nil, err
	}
	return link, nil
}

// resolveDependencies validates and resolves name-based dependencies to ifindices.
// Returns a resolved config copy with ParentIndex set for VLANs.
// Returns DependencyError if any required dependency doesn't exist yet.
//
// The returned config is safe to use for create/update operations.
// The original config is never modified (store integrity preserved).
func resolveDependencies(cfg *DeviceConfig) (*DeviceConfig, error) {
	vlan, isVlan := cfg.Link.(*netlink.Vlan)

	// VLANParent is only valid for VLAN devices
	if !isVlan && cfg.VLANParent != "" {
		return nil, fmt.Errorf("invalid DeviceConfig: VLANParent set but Link is %T, not *netlink.Vlan", cfg.Link)
	}

	// VLAN without VLANParent: validate legacy ParentIndex
	if isVlan && cfg.VLANParent == "" {
		if vlan.ParentIndex == 0 {
			return nil, fmt.Errorf("invalid DeviceConfig: VLAN %q requires VLANParent or ParentIndex", cfg.deviceName())
		}
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

	// Validate master exists before any destructive operations
	if cfg.Master != "" {
		if _, err := requireLink(cfg.Master); err != nil {
			return nil, fmt.Errorf("master: %w", err)
		}
	}

	// No VLAN parent to resolve - return original config (no copy needed)
	if cfg.VLANParent == "" {
		return cfg, nil
	}

	// Resolve VLANParent name to ifindex
	parent, err := requireLink(cfg.VLANParent)
	if err != nil {
		return nil, fmt.Errorf("VLAN parent: %w", err)
	}

	// Shallow copy with a deep-copied Link: only ParentIndex is mutated,
	// so we only need to protect the Link from writes to the stored config.
	resolved := *cfg
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
			if desired.SrcAddr != nil && (e.SrcAddr == nil || !e.SrcAddr.Equal(desired.SrcAddr)) {
				klog.V(4).Infof("NetlinkDeviceManager: VXLAN %s src addr mismatch: %v != %v",
					cfg.deviceName(), e.SrcAddr, desired.SrcAddr)
				return true
			}
			if desired.Port > 0 && e.Port != desired.Port {
				klog.V(4).Infof("NetlinkDeviceManager: VXLAN %s port mismatch: %d != %d",
					cfg.deviceName(), e.Port, desired.Port)
				return true
			}
			if desired.FlowBased != e.FlowBased {
				klog.V(4).Infof("NetlinkDeviceManager: VXLAN %s FlowBased mismatch: %v != %v",
					cfg.deviceName(), e.FlowBased, desired.FlowBased)
				return true
			}
			if desired.VniFilter != e.VniFilter {
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
	} else if currentAttrs.MasterIndex != 0 {
		// Desired config has no master, but device is currently attached to one.
		// Detach to match the declarative "no master" intent.
		if err := util.GetNetLinkOps().LinkSetNoMaster(link); err != nil {
			return fmt.Errorf("failed to detach %s from master: %w", name, err)
		}
		klog.V(4).Infof("NetlinkDeviceManager: detached device %s from master (ifindex %d)", name, currentAttrs.MasterIndex)
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
	if current.Attrs().Alias != cfg.alias() {
		return true
	}
	return !linkMutableFieldsMatch(current, cfg.Link)
}

// prepareLinkForModify creates a Link object suitable for LinkModify.
// It includes all mutable fields that linkMutableFieldsMatch checks.
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

	// Handle type-specific mutable fields.
	// Each supported type must have an explicit case to ensure the correct
	// IFLA_INFO_KIND is sent in the netlink message.
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
	case *netlink.Vrf:
		return &netlink.Vrf{
			LinkAttrs: baseAttrs,
			Table:     desired.Table,
		}
	case *netlink.Vlan:
		return &netlink.Vlan{
			LinkAttrs: baseAttrs,
			VlanId:    desired.VlanId,
		}
	case *netlink.Dummy:
		return &netlink.Dummy{LinkAttrs: baseAttrs}
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

// isLinkLocalAddress returns true for link-local addresses (IPv6 fe80::/10 or IPv4 169.254.0.0/16).
// These addresses are kernel-managed and should not be removed automatically.
func isLinkLocalAddress(ip net.IP) bool {
	return ip != nil && ip.IsLinkLocalUnicast()
}

// getVIDVNIMappings retrieves current VID/VNI mappings for a specific VXLAN device.
func getVIDVNIMappings(vxlanName string) ([]VIDVNIMapping, error) {
	nlOps := util.GetNetLinkOps()

	link, err := nlOps.LinkByName(vxlanName)
	if err != nil {
		klog.V(5).Infof("NetlinkDeviceManager: could not get link %s for tunnel info: %v", vxlanName, err)
		return nil, err
	}

	tunnels, err := nlOps.BridgeVlanTunnelShowDev(link)
	if err != nil {
		klog.V(5).Infof("NetlinkDeviceManager: could not get tunnel info for %s: %v", vxlanName, err)
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

	klog.V(5).Infof("NetlinkDeviceManager: found %d existing VID/VNI mappings on %s", len(mappings), vxlanName)
	return mappings, nil
}

// validateMappings checks VID/VNI ranges and uniqueness constraints.
// Range: VID [1, 4094], VNI [1, 16777215].
// Uniqueness is required because:
//   - Two VIDs mapping to the same VNI would cause removeVIDVNIMapping to delete the VNI filter
//     entry still needed by the other VID
//   - Duplicate VIDs would be ambiguous
const maxVNI = 1<<24 - 1 // 16777215

func validateMappings(mappings []VIDVNIMapping) error {
	seenVIDs := make(map[uint16]bool)
	seenVNIs := make(map[uint32]bool)

	for _, m := range mappings {
		if m.VID < 1 || m.VID > 4094 {
			return fmt.Errorf("VID %d out of valid range [1, 4094]", m.VID)
		}
		if m.VNI < 1 || m.VNI > maxVNI {
			return fmt.Errorf("VNI %d out of valid range [1, %d]", m.VNI, maxVNI)
		}
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
func addVIDVNIMapping(bridgeLink, vxlanLink netlink.Link, m VIDVNIMapping) error {
	nlOps := util.GetNetLinkOps()

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
// This function does NOT hold any locks - caller must ensure thread safety.
// Note: Unlike addVIDVNIMapping, this does NOT remove the bridge self VID because:
// 1. Other VXLAN mappings or ports might still use that VID
// 2. Bridge self VIDs are automatically cleaned up by the kernel when the bridge is deleted
func removeVIDVNIMapping(vxlanLink netlink.Link, m VIDVNIMapping) error {
	nlOps := util.GetNetLinkOps()

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
		return fmt.Errorf("failed to remove mapping from %s: %w", vxlanLink.Attrs().Name, errors.Join(errs...))
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

// ApplyBridgePortVLAN adds a VLAN to a bridge port.
// Stateless: performs inline netlink I/O. The caller owns the port lifecycle.
func ApplyBridgePortVLAN(linkName string, vlan BridgePortVLAN) error {
	nlOps := util.GetNetLinkOps()

	// Get the link
	link, err := nlOps.LinkByName(linkName)
	if err != nil {
		return fmt.Errorf("failed to get link %s: %w", linkName, err)
	}

	// Add VLAN with flags
	// BridgeVlanAdd(link, vid, pvid, untagged, self, master)
	// For port VLANs: self=false, master=true
	if err := nlOps.BridgeVlanAdd(link, vlan.VID, vlan.PVID, vlan.Untagged, false, true); err != nil {
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

// linkMutableFieldsMatch reports whether current already satisfies desired for mutable
// link attributes that can be updated via LinkModify. Zero-valued fields in desired are
// treated as "unspecified, don't care" to avoid triggering unnecessary LinkModify calls
// that would keep generating netlink events.
// This is directional: Match(a, b) does NOT imply Match(b, a).
// Immutable fields like type are checked separately in configsEqual and hasCriticalMismatch.
func linkMutableFieldsMatch(current, desired netlink.Link) bool {
	curAttrs := current.Attrs()
	desAttrs := desired.Attrs()

	if desAttrs.MTU != 0 && curAttrs.MTU != desAttrs.MTU {
		return false
	}
	// TxQLen: -1 means "unset" (from NewLinkAttrs()), 0 means "set to zero"
	if desAttrs.TxQLen >= 0 && curAttrs.TxQLen != desAttrs.TxQLen {
		return false
	}
	if len(desAttrs.HardwareAddr) > 0 && !bytes.Equal(curAttrs.HardwareAddr, desAttrs.HardwareAddr) {
		return false
	}

	// VXLAN-specific mutable fields
	curVxlan, curIsVxlan := current.(*netlink.Vxlan)
	desVxlan, desIsVxlan := desired.(*netlink.Vxlan)
	if curIsVxlan && desIsVxlan {
		if curVxlan.Learning != desVxlan.Learning {
			return false
		}
	}

	// Bridge-specific mutable fields
	curBridge, curIsBridge := current.(*netlink.Bridge)
	desBridge, desIsBridge := desired.(*netlink.Bridge)
	if curIsBridge && desIsBridge {
		if desBridge.VlanFiltering != nil && !ptr.Equal(curBridge.VlanFiltering, desBridge.VlanFiltering) {
			return false
		}
		if desBridge.VlanDefaultPVID != nil && !ptr.Equal(curBridge.VlanDefaultPVID, desBridge.VlanDefaultPVID) {
			return false
		}
	}

	return true
}

// linkMutableFieldsEqual performs strict symmetric equality of mutable link fields.
// Unlike linkMutableFieldsMatch, zero-valued fields are significant.
func linkMutableFieldsEqual(a, b netlink.Link) bool {
	return linkMutableFieldsMatch(a, b) && linkMutableFieldsMatch(b, a)
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

	// Compare VRF immutable fields
	aVrf, aIsVrf := a.Link.(*netlink.Vrf)
	bVrf, bIsVrf := b.Link.(*netlink.Vrf)
	if aIsVrf && bIsVrf {
		if aVrf.Table != bVrf.Table {
			return false
		}
	}

	// Compare VXLAN immutable fields
	aVxlan, aIsVxlan := a.Link.(*netlink.Vxlan)
	bVxlan, bIsVxlan := b.Link.(*netlink.Vxlan)
	if aIsVxlan != bIsVxlan {
		return false
	}
	if aIsVxlan && bIsVxlan {
		if (aVxlan.SrcAddr == nil) != (bVxlan.SrcAddr == nil) ||
			(aVxlan.SrcAddr != nil && !aVxlan.SrcAddr.Equal(bVxlan.SrcAddr)) ||
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
		if aVlan.VlanProtocol != bVlan.VlanProtocol {
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
