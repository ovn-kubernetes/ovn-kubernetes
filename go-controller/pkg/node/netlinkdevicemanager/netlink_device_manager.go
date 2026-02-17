package netlinkdevicemanager

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// Interface defines the contract for NetlinkDeviceManager Controller.
//
// # Design: Asynchronous API
//
// All mutating methods (EnsureLink, DeleteLink) are
// asynchronous: they store the desired state and return immediately. A single
// background worker goroutine performs all netlink I/O, serializing kernel
// operations and applying rate-limited retry on transient failures.
//
// This design was chosen over a synchronous "apply-and-return-error" approach
// for three reasons:
//
//  1. Cross-device dependencies. Unlike routes, devices depend on each other:
//     a VXLAN requires its master bridge, an SVI requires both a VLAN parent
//     (bridge) and a master (VRF, managed by a different controller). When a
//     dependency is missing, synchronous apply cannot return an actionable error
//     — the caller cannot resolve the dependency; a synchronous error would
//     only force the caller to implement retry/backoff (or poll/track
//     dependency readiness), often delaying convergence. The async model
//     classifies this as DeviceStatePending and retries promptly when the
//     dependency appears via a netlink event, without caller involvement.
//
//  2. Multi-step I/O. Each device reconciliation involves multiple netlink
//     calls (create, set alias, set master, apply bridge port settings, bring
//     up, sync addresses). A synchronous model must either hold a lock across
//     all of these (blocking all other callers and the event handler for the
//     full duration) or release the lock during I/O (duplicating the
//     reconciler's staleness-check logic and creating a second I/O path that
//     competes with the background worker). The async model avoids both:
//     a single worker goroutine owns all I/O, API methods never block on
//     kernel operations, and the workqueue deduplicates keys so bursty
//     netlink events collapse into fewer reconciles.
//
//  3. Single linearization point (policy + state machine in one place).
//     With a synchronous API, netlink events and periodic reconciliation,
//     multiple code paths would perform netlink I/O. To be correct, each
//     path must enforce the same invariants ("still desired?", "do we
//     own it?", current ifindex/kernel state, error classification/backoff,
//     state transitions). Any drift between paths creates subtle races
//     (e.g. resurrecting a deleted device or acting on stale state). The
//     async workqueue funnels all I/O and state transitions through one
//     reconciler goroutine, so these invariants are implemented once and
//     applied consistently. A "synchronous but centralized" design
//     would still need to serialize all entrypoints through that same
//     reconciler, which effectively converges on the workqueue pattern.
//
// Because the API is asynchronous, downstream controllers that depend on
// device readiness (e.g., EVPN waiting for the bridge before attaching OVS
// ports) are notified via the DeviceReconciler subscriber interface.
//
// Callers that need synchronous confirmation can poll GetDeviceState after
// calling EnsureLink. A dedicated synchronous interface may be added in the
// future if specific use cases (e.g., synchronous startup sequences) require
// it.
//
// # Startup Contract
//
// Controllers MUST call EnsureLink for all previously-existing desired devices
// before calling Run. On startup, Run performs orphan cleanup: it scans the
// kernel for devices with our ownership alias that are not in the desired state
// store and deletes them. If the store is empty, all previously-managed devices
// will be removed.
//
// # Ownership
//
// Devices created through EnsureLink are marked with an IFLA_IFALIAS prefix
// ("ovn-k8s-ndm:"). Only devices with this alias are considered owned and may
// be modified or deleted. Devices without the alias (or with a foreign alias)
// are never touched — if a name collision occurs, the reconciler sets the
// device state to DeviceStateBlocked (discoverable via GetDeviceState or
// subscriber notification).
//
// # Supported Device Types
//
// The manager supports Bridge, Vxlan, Vlan, Vrf, and Dummy device types.
type Interface interface {
	// EnsureLink declares the desired configuration for a device.
	//
	// Behavior is asynchronous: the configuration is stored and a
	// reconciliation is enqueued. The method returns nil once the intent is
	// recorded; it does not wait for the device to be created or updated in
	// the kernel. Only validation errors (invalid name, unsupported type)
	// are returned synchronously.
	//
	// Updates are handled in-place: callers may call EnsureLink again with
	// a changed config for the same device name. There is no need to call
	// DeleteLink before re-calling EnsureLink. The reconciler
	// transparently chooses the least-disruptive strategy: mutable
	// attributes are patched on the existing device while immutable
	// attributes trigger an automatic delete+recreate.
	//
	// If the device already exists in the store with an identical config,
	// this is a no-op — no reconciliation is enqueued, except for Blocked
	// devices where the caller may know the external conflict was resolved.
	//
	// If a dependency (Master, VLANParent) does not exist in the kernel,
	// the device is marked DeviceStatePending and automatically retried
	// when the dependency appears via netlink event.
	//
	// Full-state semantics: the provided config is the complete desired
	// state. Attributes not specified are treated as "use kernel default."
	// Addresses, if non-nil, are declarative — exactly the listed addresses
	// will exist on the device.
	EnsureLink(cfg DeviceConfig) error

	// DeleteLink removes a device from the desired state.
	//
	// Behavior is asynchronous: the device is removed from the store and
	// a reconciliation is enqueued to delete it from the kernel. The
	// method returns nil once the intent is recorded.
	//
	// Only devices with our ownership alias are deleted from the kernel.
	DeleteLink(name string) error

	// Has returns true if a device is registered in the desired state store.
	Has(name string) bool

	// GetConfig returns a copy of the config for a managed device, or nil
	// if the device is not in the store.
	GetConfig(name string) *DeviceConfig

	// ListDevicesByVLANParent returns configs for all devices whose
	// VLANParent matches the given name.
	ListDevicesByVLANParent(parentName string) []DeviceConfig

	// IsDeviceReady returns true if the device exists in the store and its
	// state is DeviceStateReady (kernel state matches desired config).
	IsDeviceReady(name string) bool

	// GetDeviceState returns the lifecycle state of a managed device.
	// Returns DeviceStateUnknown if the device is not in the store (never
	// declared via EnsureLink, or already removed via DeleteLink).
	GetDeviceState(name string) DeviceState

	// RegisterDeviceReconciler registers a subscriber to be notified on
	// device state transitions. The subscriber's ReconcileDevice method is
	// called with the device name whenever the device's state changes
	// (e.g., Pending→Ready, Ready→Failed). Subscribers should map the
	// device name to their own work items and re-queue them — heavy
	// processing should not be done inline.
	//
	// Safe to call before or after Run(). Subscribers that register after
	// Run() will receive notifications for all subsequent state transitions;
	// they can query current state of devices they care about via
	// GetDeviceState or IsDeviceReady.
	RegisterDeviceReconciler(r DeviceReconciler)

	// Run starts the background reconciler and netlink event listener.
	//
	// Callers MUST call EnsureLink for all desired devices before Run.
	// On startup, Run performs orphan cleanup (removing devices with our
	// alias not in the store) and then begins processing the reconciliation
	// queue. Netlink events are subscribed to before the worker starts to
	// prevent missed events.
	//
	// The reconciler runs until stopCh is closed. doneWg is decremented
	// when the reconciler has fully stopped.
	Run(stopCh <-chan struct{}, doneWg *sync.WaitGroup) error
}

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

// managedAliasPrefix is the prefix used in IFLA_IFALIAS to mark devices managed by this controller.
// This allows safe cleanup: only delete devices with this prefix.
// Format: "ovn-k8s-ndm:<type>:<name>" for debugging and collision avoidance.
const managedAliasPrefix = "ovn-k8s-ndm:"

// maxInterfaceNameLength is the maximum length for Linux interface names.
// Linux's IFNAMSIZ is 16 (including null terminator), so max usable length is 15.
const maxInterfaceNameLength = 15

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

// defaultReconcilePeriod is the default interval for periodic sync as a safety net.
const defaultReconcilePeriod = 60 * time.Second

// Key prefixes for workqueue item type routing.
// Workqueue deduplicates by key, so rapid updates to the same device coalesce.
const (
	deviceKeyPrefix = "device/"  // e.g., "device/br-evpn"
	fullSyncKey     = "fullsync" // Startup: orphan cleanup + re-enqueue all
	syncKey         = "sync"     // Periodic: re-enqueue all (no orphan scan)
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
	// Required for *netlink.Vlan devices. The parent's current ifindex is
	// resolved at creation time, which is resilient to parent recreation
	// (ifindex changes).
	// If the parent doesn't exist yet, config is stored as pending and retried on netlink events.
	VLANParent string

	// BridgePortSettings configures bridge port-specific settings.
	// Only applicable when Master is set and is a bridge.
	// Settings are applied after the device is attached to the bridge.
	// Typically used for VXLAN ports that need vlan_tunnel=on, neigh_suppress=on, learning=off.
	BridgePortSettings *BridgePortSettings

	// Addresses specifies IP addresses to configure on the device.
	//
	// Semantics:
	//   - nil:           No address management. Existing addresses are preserved.
	//   - empty slice:   Declarative empty state. All addresses will be removed
	//                    (except link-local: IPv6 fe80::/10 and IPv4 169.254.0.0/16).
	//   - non-empty:     Declarative. Exactly these addresses will exist.
	//                    Missing addresses are added, extra addresses are removed
	//                    (except link-local).
	//
	// Address equality is based on IPNet (IP + prefix length) only.
	// Other Addr fields (Flags, Scope, Label, ValidLft, PreferredLft) are
	// applied when adding but not used for comparison.
	//
	// Link-local addresses (IPv6 fe80::/10 and IPv4 169.254.0.0/16) are
	// never auto-removed because they are kernel-managed and removing them
	// can break network functionality.
	Addresses []netlink.Addr

	// VIDVNIMappings specifies VID↔VNI tunnel mappings for VXLAN bridge ports.
	// Only valid when Link is *netlink.Vxlan and Master is set (bridge master).
	//
	// Semantics:
	//   - nil:           No mapping management. Existing mappings are preserved.
	//   - empty slice:   Declarative empty state. All stale mappings will be removed.
	//   - non-empty:     Declarative. Exactly these mappings will exist.
	//                    Missing mappings are added, stale mappings are removed.
	//
	// Each VID and each VNI must be unique within the slice.
	VIDVNIMappings []VIDVNIMapping
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

// managedDevice tracks a device with its config and status
type managedDevice struct {
	cfg        DeviceConfig // Complete desired config
	state      DeviceState  // Lifecycle state (Ready, Pending, Failed, Blocked)
	lastError  error        // Last error from reconciliation (preserved for status/debug)
	generation uint64       // Monotonic counter incremented by EnsureLink on config change; used by reconciler for staleness detection
}

// Controller manages Linux network device lifecycle using a workqueue-based reconciler.
// Public API methods store desired state and enqueue work; a single worker goroutine
// performs all netlink I/O. Self-heals via periodic sync, orphan cleanup, and netlink
// event-driven reconciliation.
type Controller struct {
	mu    sync.RWMutex
	store map[string]*managedDevice // device name -> managed device info

	reconciler    controller.Reconciler // workqueue reconciler (single worker, all I/O)
	subscribersMu sync.RWMutex          // protects subscribers slice; allows post-Run() registration
	subscribers   []DeviceReconciler

	// ReconcilePeriod is the interval for periodic sync as a safety net.
	// Defaults to DefaultReconcilePeriod. Can be overridden before calling Run().
	ReconcilePeriod time.Duration
}

// NewController creates a new NetlinkDeviceManager with default settings.
// The ReconcilePeriod can be overridden before calling Run() if needed.
func NewController() *Controller {
	c := &Controller{
		store:           make(map[string]*managedDevice),
		ReconcilePeriod: defaultReconcilePeriod,
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
	// Snapshot config for lock-free I/O. Shallow-copy Link so downstream
	// functions can mutate it without affecting the stored config.
	cfg := device.cfg
	cfg.Link = cloneLink(cfg.Link)
	gen := device.generation
	previousState := device.state
	unlock()

	// All netlink I/O OUTSIDE lock
	modified, err := applyDeviceConfig(&cfg)

	// Update state under Lock
	c.mu.Lock()
	unlock = sync.OnceFunc(c.mu.Unlock)
	defer unlock()

	device, stillExists := c.store[name]
	if !stillExists {
		return nil // Deleted during I/O — re-queued key will handle delete
	}

	// Staleness guard: if config was replaced during I/O (concurrent EnsureLink
	// incremented generation), skip state update. The replacement already
	// enqueued a fresh reconcile that will apply the current config.
	if device.generation != gen {
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

	// Notify subscribers OUTSIDE lock (avoids deadlock if subscriber calls GetDeviceState).
	// Notify on state transitions OR when kernel state was modified.
	if previousState != newState || modified {
		c.notifySubscribers(name)
	}

	return reconcileErr
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
		klog.V(5).Infof("NetlinkDeviceManager: deleting orphaned device %s", name)
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
	return nil
}

// validateConfig checks that a DeviceConfig is well-formed.
func (cfg *DeviceConfig) validateConfig() error {
	name := cfg.deviceName()
	if err := validateInterfaceName(name, "device"); err != nil {
		return err
	}
	if err := validateLinkType(cfg.Link); err != nil {
		return fmt.Errorf("device %q: %w", name, err)
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
	for _, addr := range cfg.Addresses {
		if addr.IPNet != nil && isLinkLocalAddress(addr.IPNet.IP) {
			return fmt.Errorf("device %q: link-local address %s cannot be managed (kernel-managed)", name, addr.IPNet)
		}
	}
	if cfg.VIDVNIMappings != nil {
		if _, ok := cfg.Link.(*netlink.Vxlan); !ok {
			return fmt.Errorf("device %q: VIDVNIMappings is only valid for VXLAN devices, got %T", name, cfg.Link)
		}
		if cfg.Master == "" {
			return fmt.Errorf("device %q: VIDVNIMappings requires a bridge master", name)
		}
		if err := validateMappings(cfg.VIDVNIMappings); err != nil {
			return fmt.Errorf("device %q: %w", name, err)
		}
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
	if err := cfg.validateConfig(); err != nil {
		return err
	}

	name := cfg.deviceName()

	// Defensive copy: prevent data race if caller mutates the slices/link after return.
	cfg.Addresses = slices.Clone(cfg.Addresses)
	cfg.VIDVNIMappings = slices.Clone(cfg.VIDVNIMappings)
	cfg.Link = cloneLink(cfg.Link)

	c.mu.Lock()
	unlock := sync.OnceFunc(c.mu.Unlock)
	defer unlock()

	var gen uint64
	state := DeviceStatePending // default for new devices; existing devices preserve their state
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
		gen = existing.generation
		// preserve last reconciliation result so the reconciler detects the real state transition
		// and notifies subscribers
		state = existing.state
	}

	// Store desired config, increment generation, and enqueue.
	c.store[name] = &managedDevice{cfg: cfg, state: state, generation: gen + 1}
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
// Safe to call before or after Run(). Subscribers that register after Run() will receive
// notifications for all subsequent state transitions.
func (c *Controller) RegisterDeviceReconciler(r DeviceReconciler) {
	c.subscribersMu.Lock()
	c.subscribers = append(c.subscribers, r)
	c.subscribersMu.Unlock()
}

// notifySubscribers calls ReconcileDevice(name) on all registered subscribers.
// Called from the worker goroutine after state transitions.
// MUST be called OUTSIDE c.mu to avoid deadlock (subscribers may call GetDeviceState).
func (c *Controller) notifySubscribers(name string) {
	c.subscribersMu.RLock()
	subscribers := c.subscribers
	c.subscribersMu.RUnlock()

	for _, sub := range subscribers {
		if err := sub.ReconcileDevice(name); err != nil {
			klog.Warningf("NetlinkDeviceManager: subscriber error for device %s: %v", name, err)
		}
	}
}

// linkChanBufferSize is the buffer size for the netlink event channel.
// The buffer decouples the kernel socket drain rate from event processing,
// absorbing bursts (e.g., during startup or bulk reconfiguration) without
// tearing down the subscription.
const linkChanBufferSize = 100

// subscribeLinkEvents creates a buffered channel and subscribes to netlink link
// events. Returns the channel on success or nil on failure.
func subscribeLinkEvents(stopCh <-chan struct{}, onError func(error)) chan netlink.LinkUpdate {
	ch := make(chan netlink.LinkUpdate, linkChanBufferSize)
	options := netlink.LinkSubscribeOptions{
		ErrorCallback: onError,
	}
	if err := netlink.LinkSubscribeWithOptions(ch, stopCh, options); err != nil {
		onError(err)
		return nil
	}
	return ch
}

// Run starts the controller's workqueue reconciler and netlink event listener.
// Controllers should call EnsureLink for all desired devices BEFORE calling Run().
func (c *Controller) Run(stopCh <-chan struct{}, doneWg *sync.WaitGroup) error {
	reconcilePeriod := c.ReconcilePeriod

	// Subscribe to netlink events BEFORE starting workers.
	// This ensures no events are missed between worker startup and subscription.
	onSubscribeError := func(err error) {
		klog.Errorf("NetlinkDeviceManager: netlink subscribe error: %v", err)
	}
	linkChan := subscribeLinkEvents(stopCh, onSubscribeError)

	// Start reconciler with orphan cleanup as initial sync.
	if err := controller.StartWithInitialSync(
		c.cleanupOrphanedDevices,
		c.reconciler,
	); err != nil {
		return fmt.Errorf("failed to start reconciler: %w", err)
	}

	// Queue initial sync (not fullsync — orphan cleanup already ran via StartWithInitialSync)
	c.reconciler.Reconcile(syncKey)

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
					// The channel is closed when the subscribe goroutine exits.
					// Resubscribe and trigger a full sync to catch missed events.
					klog.Warning("NetlinkDeviceManager: netlink channel closed, resubscribing")
					linkChan = subscribeLinkEvents(stopCh, onSubscribeError)
					if linkChan != nil {
						c.reconciler.Reconcile(fullSyncKey)
					}
					continue
				}
				c.handleLinkUpdate(update.Link)

			case <-syncTimer.C:
				klog.V(5).Info("NetlinkDeviceManager: periodic sync")
				c.reconciler.Reconcile(syncKey)
				if linkChan == nil {
					linkChan = subscribeLinkEvents(stopCh, onSubscribeError)
					if linkChan != nil {
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
// Callers must ensure cfg.Link can be safely mutated.
// Returns DependencyError if a required dependency doesn't exist yet.
func resolveDependencies(cfg *DeviceConfig) error {
	vlan, isVlan := cfg.Link.(*netlink.Vlan)

	if !isVlan && cfg.VLANParent != "" {
		return fmt.Errorf("invalid DeviceConfig: VLANParent set but Link is %T, not *netlink.Vlan", cfg.Link)
	}
	if isVlan && cfg.VLANParent == "" {
		return fmt.Errorf("invalid DeviceConfig: VLAN %q requires VLANParent", cfg.deviceName())
	}

	if cfg.Master != "" {
		if _, err := requireLink(cfg.Master); err != nil {
			return fmt.Errorf("master: %w", err)
		}
	}

	if isVlan {
		parent, err := requireLink(cfg.VLANParent)
		if err != nil {
			return fmt.Errorf("VLAN parent: %w", err)
		}
		vlan.ParentIndex = parent.Attrs().Index
	}

	return nil
}

// applyDeviceConfig creates or updates a single device in the kernel.
// Returns (modified, err) where modified indicates whether kernel state was
// actually changed.
//
// Ownership rules:
//   - If device doesn't exist: create it with our alias
//   - If device exists with our alias: update or recreate as needed
//   - If device exists without our alias: return NotOwnedError (could be human-created)
func applyDeviceConfig(cfg *DeviceConfig) (bool, error) {
	name := cfg.deviceName()
	// Resolve all dependencies first. For the delete-then-recreate path, this ensures
	// we never delete an existing device unless all dependencies are present to recreate it.
	// For new devices, early failure here is equivalent to failure in createDevice() -
	// both return DependencyError and mark the config pending. But for existing devices,
	// failing after delete would leave us in a worse state (device gone, can't recreate).
	if err := resolveDependencies(cfg); err != nil {
		return false, err
	}
	modified := false
	// Check if device already exists
	link, err := util.GetNetLinkOps().LinkByName(name)
	if err == nil {
		// Device exists - verify ownership before modifying
		if err := checkOwnership(link); err != nil {
			return modified, err
		}

		// Check for critical mismatches (immutable attributes that require recreate)
		if hasCriticalMismatch(link, cfg) {
			klog.Warningf("NetlinkDeviceManager: device %s has critical config drift, recreating", name)
			if err := util.GetNetLinkOps().LinkDelete(link); err != nil {
				return modified, fmt.Errorf("failed to delete mismatched device %s: %w", name, err)
			}
			modified = true
			// Fall through to create
		} else {
			// Device exists with correct critical attrs, update mutable attrs
			return updateDevice(link, cfg)
		}
	} else if !util.GetNetLinkOps().IsLinkNotFoundError(err) {
		return modified, fmt.Errorf("failed to check device %s: %w", name, err)
	}

	// Device doesn't exist (or was just deleted), create it
	if err := createDevice(cfg); err != nil {
		return modified, err
	}
	return true, nil
}

// hasCriticalMismatch checks if the existing device has immutable attributes
// that differ from desired config. These require delete+recreate.
func hasCriticalMismatch(existing netlink.Link, cfg *DeviceConfig) bool {
	if cfg.Link == nil {
		return false
	}

	// Type mismatch is always critical
	if existing.Type() != cfg.Link.Type() {
		klog.V(5).Infof("NetlinkDeviceManager: type mismatch for %s: %s != %s",
			cfg.deviceName(), existing.Type(), cfg.Link.Type())
		return true
	}

	switch desired := cfg.Link.(type) {
	case *netlink.Vrf:
		// VRF table ID is immutable
		if e, ok := existing.(*netlink.Vrf); ok {
			if e.Table != desired.Table {
				klog.V(5).Infof("NetlinkDeviceManager: VRF %s table mismatch: %d != %d",
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
					klog.V(5).Infof("NetlinkDeviceManager: bridge %s vlan_filtering mismatch: %v != %v",
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
					klog.V(5).Infof("NetlinkDeviceManager: bridge %s vlan_default_pvid mismatch: %d != %d",
						cfg.deviceName(), existingPVID, *desired.VlanDefaultPVID)
					return true
				}
			}
		}

	case *netlink.Vxlan:
		// VXLAN VNI, src addr, port, FlowBased, VniFilter are immutable
		if e, ok := existing.(*netlink.Vxlan); ok {
			if e.VxlanId != desired.VxlanId {
				klog.V(5).Infof("NetlinkDeviceManager: VXLAN %s VNI mismatch: %d != %d",
					cfg.deviceName(), e.VxlanId, desired.VxlanId)
				return true
			}
			if desired.SrcAddr != nil && (e.SrcAddr == nil || !e.SrcAddr.Equal(desired.SrcAddr)) {
				klog.V(5).Infof("NetlinkDeviceManager: VXLAN %s src addr mismatch: %v != %v",
					cfg.deviceName(), e.SrcAddr, desired.SrcAddr)
				return true
			}
			if desired.Port > 0 && e.Port != desired.Port {
				klog.V(5).Infof("NetlinkDeviceManager: VXLAN %s port mismatch: %d != %d",
					cfg.deviceName(), e.Port, desired.Port)
				return true
			}
			if desired.FlowBased != e.FlowBased {
				klog.V(5).Infof("NetlinkDeviceManager: VXLAN %s FlowBased mismatch: %v != %v",
					cfg.deviceName(), e.FlowBased, desired.FlowBased)
				return true
			}
			if desired.VniFilter != e.VniFilter {
				klog.V(5).Infof("NetlinkDeviceManager: VXLAN %s VniFilter mismatch: %v != %v",
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
				klog.V(5).Infof("NetlinkDeviceManager: VLAN %s ID mismatch: %d != %d",
					cfg.deviceName(), e.VlanId, desired.VlanId)
				return true
			}
			if desired.ParentIndex > 0 && e.ParentIndex != desired.ParentIndex {
				klog.V(5).Infof("NetlinkDeviceManager: VLAN %s parent mismatch: ifindex %d != %d",
					cfg.deviceName(), e.ParentIndex, desired.ParentIndex)
				return true
			}
			if desired.VlanProtocol != 0 && e.VlanProtocol != desired.VlanProtocol {
				klog.V(5).Infof("NetlinkDeviceManager: VLAN %s protocol mismatch: %d != %d",
					cfg.deviceName(), e.VlanProtocol, desired.VlanProtocol)
				return true
			}
			desiredMAC := desired.Attrs().HardwareAddr
			if len(desiredMAC) > 0 && !bytes.Equal(e.Attrs().HardwareAddr, desiredMAC) {
				klog.V(5).Infof("NetlinkDeviceManager: VLAN %s MAC mismatch: %v != %v",
					cfg.deviceName(), e.Attrs().HardwareAddr, desiredMAC)
				return true
			}
		}

	case *netlink.Dummy:
		// No type-specific immutable fields

	default:
		panic(fmt.Sprintf("BUG: unsupported device type %T for %q reached hasCriticalMismatch", cfg.Link, cfg.deviceName()))
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

	// Set alias before creation.
	cfg.Link.Attrs().Alias = cfg.alias()

	link, err := createLink(cfg)
	if err != nil {
		return err
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
		klog.V(5).Infof("NetlinkDeviceManager: set master %s for device %s", cfg.Master, name)

		// Apply bridge port settings after attaching to master (required for settings to take effect)
		if cfg.BridgePortSettings != nil {
			if err := applyBridgePortSettings(link, *cfg.BridgePortSettings); err != nil {
				return fmt.Errorf("failed to apply bridge port settings for %s: %w", name, err)
			}
			klog.V(5).Infof("NetlinkDeviceManager: applied bridge port settings for device %s", name)
		}
	}

	// Bring the device up after creation
	if err := ensureDeviceUp(link); err != nil {
		return err
	}

	// Sync addresses if configured
	if err := syncAddresses(link, cfg); err != nil {
		return err
	}

	// Sync VID/VNI mappings (VXLAN bridge ports only)
	if err := syncVIDVNIMappings(link, cfg); err != nil {
		return err
	}

	klog.V(5).Infof("NetlinkDeviceManager: created device %s", name)
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
func updateDevice(link netlink.Link, cfg *DeviceConfig) (bool, error) {
	name := cfg.deviceName()
	currentAttrs := link.Attrs()
	modified := false

	// Only call LinkModify if there are actual differences to apply.
	// This prevents unnecessary netlink events.
	if needsLinkModify(link, cfg) {
		modifiedLink := prepareLinkForModify(link, cfg)
		if err := util.GetNetLinkOps().LinkModify(modifiedLink); err != nil {
			return modified, fmt.Errorf("failed to modify link %s: %w", name, err)
		}
		modified = true
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
				return modified, &DependencyError{Dependency: cfg.Master, Reason: "master not found (deleted after validation)"}
			}
			return modified, fmt.Errorf("failed to find master %s: %w", cfg.Master, err)
		}
		if currentAttrs.MasterIndex != masterLink.Attrs().Index {
			if err := util.GetNetLinkOps().LinkSetMaster(link, masterLink); err != nil {
				return modified, fmt.Errorf("failed to set master %s for device %s: %w", cfg.Master, name, err)
			}
			masterChanged = true
			modified = true
			klog.V(5).Infof("NetlinkDeviceManager: updated master %s for device %s", cfg.Master, name)
		}
	} else if currentAttrs.MasterIndex != 0 {
		// Desired config has no master, but device is currently attached to one.
		// Detach to match the declarative "no master" intent.
		if err := util.GetNetLinkOps().LinkSetNoMaster(link); err != nil {
			return modified, fmt.Errorf("failed to detach %s from master: %w", name, err)
		}
		modified = true
		klog.V(5).Infof("NetlinkDeviceManager: detached device %s from master (ifindex %d)", name, currentAttrs.MasterIndex)
	}

	// Apply bridge port settings if configured (not handled by LinkModify).
	if err := ensureBridgePortSettings(link, cfg, masterChanged); err != nil {
		return modified, err
	}

	if err := ensureDeviceUp(link); err != nil {
		return modified, err
	}

	// Sync addresses if configured
	if err := syncAddresses(link, cfg); err != nil {
		return modified, err
	}

	// Sync VID/VNI mappings (VXLAN bridge ports only)
	if err := syncVIDVNIMappings(link, cfg); err != nil {
		return modified, err
	}

	return modified, nil
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
		panic(fmt.Sprintf("BUG: unsupported device type %T for %q", cfg.Link, cfg.deviceName()))
	}
}

// deleteDevice removes a device from the kernel.
// Only deletes devices that have our alias prefix (ownership check).
func deleteDevice(name string) error {
	link, err := util.GetNetLinkOps().LinkByName(name)
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

	if err := util.GetNetLinkOps().LinkDelete(link); err != nil {
		return fmt.Errorf("failed to delete device %s: %w", name, err)
	}
	klog.V(5).Infof("NetlinkDeviceManager: deleted device %s", name)
	return nil
}

// cloneLink returns a shallow copy of the concrete struct behind a netlink.Link
// interface. Slice/pointer fields within the struct (e.g. HardwareAddr)
// are not recursively copied.
func cloneLink(link netlink.Link) netlink.Link {
	switch l := link.(type) {
	case *netlink.Bridge:
		cp := *l
		return &cp
	case *netlink.Vxlan:
		cp := *l
		return &cp
	case *netlink.Vlan:
		cp := *l
		return &cp
	case *netlink.Vrf:
		cp := *l
		return &cp
	case *netlink.Dummy:
		cp := *l
		return &cp
	default:
		panic(fmt.Sprintf("BUG: unsupported device type %T for cloneLink", link))
	}
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

// ensureDeviceUp brings a device up. LinkSetUp is idempotent so no
// need to check current state first.
func ensureDeviceUp(link netlink.Link) error {
	if err := util.GetNetLinkOps().LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to set link %s up: %w", link.Attrs().Name, err)
	}
	return nil
}

// syncAddresses ensures the device has exactly the desired addresses.
// If cfg.Addresses is nil, no address management is performed (existing addresses preserved).
// Link-local addresses (fe80::/10) are never removed automatically.
func syncAddresses(currentLink netlink.Link, cfg *DeviceConfig) error {
	if cfg.Addresses == nil {
		return nil // No address management requested
	}

	nlOps := util.GetNetLinkOps()
	name := currentLink.Attrs().Name

	current, err := nlOps.AddrList(currentLink, netlink.FAMILY_ALL)
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

// syncVIDVNIMappings ensures the VXLAN device has exactly the desired VID/VNI mappings.
// If cfg.VIDVNIMappings is nil, no mapping management is performed.
//
// Each mapping consists of four kernel components:
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
func syncVIDVNIMappings(link netlink.Link, cfg *DeviceConfig) error {
	if cfg.VIDVNIMappings == nil {
		return nil
	}
	if cfg.Master == "" {
		return nil
	}

	nlOps := util.GetNetLinkOps()
	name := link.Attrs().Name

	bridgeLink, err := nlOps.LinkByName(cfg.Master)
	if err != nil {
		if nlOps.IsLinkNotFoundError(err) {
			return &DependencyError{Dependency: cfg.Master, Reason: "bridge not found for VID/VNI mappings"}
		}
		return fmt.Errorf("failed to get bridge %s for mappings: %w", cfg.Master, err)
	}

	current, err := getVIDVNIMappings(link)
	if err != nil {
		return fmt.Errorf("failed to read current mappings for %s: %w", name, err)
	}

	toRemove := staleMappings(current, cfg.VIDVNIMappings)

	var errs []error

	for _, mapping := range toRemove {
		if err := removeVIDVNIMapping(link, mapping); err != nil {
			klog.Warningf("NetlinkDeviceManager: failed to remove mapping VID=%d VNI=%d from %s: %v",
				mapping.VID, mapping.VNI, name, err)
			errs = append(errs, err)
		}
	}

	for _, mapping := range cfg.VIDVNIMappings {
		if err := addVIDVNIMapping(bridgeLink, link, mapping); err != nil {
			klog.Warningf("NetlinkDeviceManager: failed to ensure mapping VID=%d VNI=%d on %s: %v",
				mapping.VID, mapping.VNI, name, err)
			errs = append(errs, err)
		}
	}

	if len(cfg.VIDVNIMappings) > 0 || len(toRemove) > 0 {
		klog.V(5).Infof("NetlinkDeviceManager: mappings %s (ensured=%d, removed=%d, errors=%d)",
			name, len(cfg.VIDVNIMappings), len(toRemove), len(errs))
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to apply %d mappings on %s: %w", len(errs), name, errors.Join(errs...))
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

// staleMappings returns mappings present in current but not in desired.
func staleMappings(current, desired []VIDVNIMapping) []VIDVNIMapping {
	return sets.New(current...).Difference(sets.New(desired...)).UnsortedList()
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
func ensureBridgePortSettings(link netlink.Link, cfg *DeviceConfig, masterChanged bool) error {
	if cfg.BridgePortSettings == nil || cfg.Master == "" {
		return nil
	}

	name := link.Attrs().Name

	// Master just changed - always apply settings
	if masterChanged {
		if err := applyBridgePortSettings(link, *cfg.BridgePortSettings); err != nil {
			return fmt.Errorf("failed to apply bridge port settings for %s: %w", name, err)
		}
		klog.V(5).Infof("NetlinkDeviceManager: applied bridge port settings for device %s", name)
		return nil
	}

	// Check if settings differ from current
	current, err := getBridgePortSettings(link)
	if err != nil {
		// Can't read current settings - log and skip.
		// This can happen if device is not yet attached to a bridge.
		klog.V(5).Infof("NetlinkDeviceManager: could not read bridge port settings for %s (skipping comparison): %v", name, err)
		return nil
	}

	if !ptr.Equal(current, cfg.BridgePortSettings) {
		if err := applyBridgePortSettings(link, *cfg.BridgePortSettings); err != nil {
			return fmt.Errorf("failed to apply bridge port settings for %s: %w", name, err)
		}
		klog.V(5).Infof("NetlinkDeviceManager: applied bridge port settings for device %s", name)
	}

	return nil
}

// getBridgePortSettings retrieves current bridge port settings for a device.
// Uses native netlink Protinfo from the link attributes.
func getBridgePortSettings(link netlink.Link) (*BridgePortSettings, error) {
	nlOps := util.GetNetLinkOps()
	name := link.Attrs().Name

	// Use LinkGetProtinfo which performs a proper AF_BRIDGE dump to get bridge port info.
	// This is more reliable than link.Attrs().Protinfo which is not populated by LinkByName.
	protinfo, err := nlOps.LinkGetProtinfo(link)
	if err != nil {
		return nil, fmt.Errorf("failed to get bridge port info for %s: %w", name, err)
	}

	return &BridgePortSettings{
		VLANTunnel:    protinfo.VlanTunnel,
		NeighSuppress: protinfo.NeighSuppress,
		Learning:      protinfo.Learning,
	}, nil
}

// applyBridgePortSettings sets bridge port settings.
func applyBridgePortSettings(link netlink.Link, settings BridgePortSettings) error {
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

	return nil
}

// deviceName extracts the device name from a DeviceConfig
func (cfg *DeviceConfig) deviceName() string {
	if cfg.Link == nil {
		return ""
	}
	return cfg.Link.Attrs().Name
}

// alias generates the ownership alias for this device.
// Format: "ovn-k8s-ndm:<type>:<name>" for debugging and collision avoidance.
func (cfg *DeviceConfig) alias() string {
	linkType := "unknown"
	if cfg.Link != nil {
		linkType = cfg.Link.Type()
	}
	return managedAliasPrefix + linkType + ":" + cfg.deviceName()
}

// linkMutableFieldsMatch reports whether current already satisfies desired for mutable
// link attributes that can be updated via LinkModify. Zero-valued fields in desired are
// treated as "unspecified, don't care" to avoid triggering unnecessary LinkModify calls
// that would keep generating netlink events.
// This is directional: Match(a, b) does NOT imply Match(b, a).
// Immutable fields like type are checked separately in hasCriticalMismatch.
//
// FIELD CONTRACT: This function and prepareLinkForModify must cover the same mutable
// fields. hasCriticalMismatch must cover all immutable fields. Together they must be
// exhaustive over all type-specific fields NDM manages. Tests enforce this invariant.
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

// linkImmutableFieldsEqual performs strict symmetric equality of immutable link fields.
// These are fields that require device deletion and recreation if they differ.
//
// FIELD CONTRACT: Every field checked here must also be checked by hasCriticalMismatch.
// hasCriticalMismatch may additionally check fields that are covered by
// linkMutableFieldsMatch for configsEqual purposes (e.g., Bridge VlanFiltering,
// VLAN HardwareAddr). The behavioral test matrix verifies that
// linkMutableFieldsMatch ∪ linkImmutableFieldsEqual ∪ configsEqual inline checks
// covers all managed fields.
func linkImmutableFieldsEqual(a, b netlink.Link) bool {
	if a.Type() != b.Type() {
		return false
	}

	switch aTyped := a.(type) {
	case *netlink.Vrf:
		bTyped := b.(*netlink.Vrf) // safe: type guard above
		if aTyped.Table != bTyped.Table {
			return false
		}

	case *netlink.Vxlan:
		bTyped := b.(*netlink.Vxlan) // safe: type guard above
		if aTyped.VxlanId != bTyped.VxlanId ||
			!ipEqual(aTyped.SrcAddr, bTyped.SrcAddr) ||
			aTyped.Port != bTyped.Port ||
			aTyped.FlowBased != bTyped.FlowBased ||
			aTyped.VniFilter != bTyped.VniFilter {
			return false
		}

	case *netlink.Vlan:
		bTyped := b.(*netlink.Vlan) // safe: type guard above
		if aTyped.VlanId != bTyped.VlanId ||
			aTyped.VlanProtocol != bTyped.VlanProtocol {
			return false
		}

	case *netlink.Bridge, *netlink.Dummy:
		// No type-specific immutable fields (type itself is checked above)

	default:
		panic(fmt.Sprintf("BUG: unsupported device type %T", a))
	}

	return true
}

// ipEqual compares two net.IP values for equality, handling nil and
// different byte-length representations (4-byte vs 16-byte IPv4).
func ipEqual(a, b net.IP) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Equal(b)
}

// configsEqual compares two DeviceConfigs for equality of all managed fields.
// Composed from building blocks that each cover a specific field category:
//   - DeviceConfig-level fields (name, master, vlanparent, bridgeport settings)
//   - linkMutableFieldsEqual (MTU, TxQLen, HardwareAddr, type-specific mutable fields)
//   - linkImmutableFieldsEqual (type-specific immutable fields: VNI, table, ParentIndex, etc.)
//   - addressesEqual (IP addresses, order-independent, IPNet-only)
//
// The only per-type logic here is VLAN ParentIndex, which depends on DeviceConfig.VLANParent
// (a DeviceConfig-level field) and thus can't be handled by linkImmutableFieldsEqual.
func configsEqual(a, b *DeviceConfig) bool {
	if a == nil || b == nil {
		return a == b
	}

	if (a.Link == nil) != (b.Link == nil) {
		return false
	}

	if a.deviceName() != b.deviceName() ||
		a.Master != b.Master ||
		a.VLANParent != b.VLANParent ||
		!ptr.Equal(a.BridgePortSettings, b.BridgePortSettings) {
		return false
	}

	if !linkMutableFieldsEqual(a.Link, b.Link) {
		return false
	}
	if !linkImmutableFieldsEqual(a.Link, b.Link) {
		return false
	}

	if !addressesEqual(a.Addresses, b.Addresses) {
		return false
	}

	if !vidVNIMappingsEqual(a.VIDVNIMappings, b.VIDVNIMappings) {
		return false
	}

	return true
}

// addressesEqual compares two address slices for equality.
// Two slices are equal if they have the same addresses (by IPNet string).
// nil and empty slice are treated as different (nil = no management, empty = want no addresses).
func addressesEqual(a, b []netlink.Addr) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return sets.KeySet(addrListToMap(a)).Equal(sets.KeySet(addrListToMap(b)))
}

// vidVNIMappingsEqual compares two VIDVNIMapping slices for equality (order-independent).
// nil and empty slice are treated as different (nil = no management, empty = want no mappings).
func vidVNIMappingsEqual(a, b []VIDVNIMapping) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return sets.New(a...).Equal(sets.New(b...))
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

// validateLinkType returns an error if the link type is not supported by the manager.
func validateLinkType(link netlink.Link) error {
	switch link.(type) {
	case *netlink.Bridge, *netlink.Vxlan, *netlink.Vlan, *netlink.Vrf, *netlink.Dummy:
		return nil
	default:
		return fmt.Errorf("unsupported link type %T; supported: Bridge, Vxlan, Vlan, Vrf, Dummy", link)
	}
}

// validateInterfaceName checks if an interface name is valid for Linux.
// Returns an error if the name is empty, exceeds IFNAMSIZ-1, contains
// characters rejected by the kernel (/, NUL, whitespace), or is reserved.
func validateInterfaceName(name, context string) error {
	if name == "" {
		return fmt.Errorf("%s name is empty", context)
	}
	if len(name) > maxInterfaceNameLength {
		return fmt.Errorf("%s name %q exceeds maximum length of %d characters (got %d)",
			context, name, maxInterfaceNameLength, len(name))
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%s name %q is reserved", context, name)
	}
	if strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("%s name %q contains invalid characters", context, name)
	}
	if strings.ContainsAny(name, " \t\n") {
		return fmt.Errorf("%s name %q contains whitespace", context, name)
	}
	return nil
}

// isOurDevice returns true only if the device has our alias prefix.
// This is the single source of truth for ownership:
//   - Empty alias = unknown ownership, NOT ours (could be human-created or other automation)
//   - Foreign alias = definitely NOT ours
//   - Our prefix = ours, safe to modify/delete
func isOurDevice(link netlink.Link) bool {
	return strings.HasPrefix(link.Attrs().Alias, managedAliasPrefix)
}

// checkOwnership returns nil if the device is ours, or a NotOwnedError explaining why not.
func checkOwnership(link netlink.Link) error {
	if isOurDevice(link) {
		return nil
	}
	alias := link.Attrs().Alias
	if alias == "" {
		return &NotOwnedError{DeviceName: link.Attrs().Name, Reason: "no alias (may be externally managed)"}
	}
	return &NotOwnedError{DeviceName: link.Attrs().Name, Reason: fmt.Sprintf("foreign alias %q", alias)}
}
