package netlinkdevicemanager

import "github.com/vishvananda/netlink"

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
// for four reasons:
//
//  1. Cross-device dependencies. Unlike routes, devices depend on each other:
//     a VXLAN requires its master bridge, an SVI requires both a VLAN parent
//     (bridge) and a master (VRF, managed by a different controller). When a
//     dependency is missing, synchronous apply cannot return an actionable error
//     — the caller cannot resolve the dependency; a synchronous error would
//     only force the caller to implement retry/backoff (or poll/track
//     dependency readiness), often delaying convergence. The async model
//     classifies this as pending and retries promptly when the
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
//  4. Netlink back-pressure control. Funneling all netlink I/O through a
//     single worker goroutine with a rate-limited workqueue gives the manager
//     direct control over how much pressure it puts on the kernel's netlink
//     subsystem.
//
// # Ownership
//
// Devices created through EnsureLink are marked with an IFLA_IFALIAS prefix
// ("ovn-k8s-ndm:"). Only devices with this alias are considered owned and may
// be modified or deleted. Devices without the alias (or with a foreign alias)
// are never touched — if a name collision occurs, the reconciler logs an
// error and stops retrying until the external device is removed.
//
// # Supported Device Types
//
// The manager supports Bridge, Vxlan, Vlan, Vrf, and Dummy device types.
//
// # NOTE on Route Management
//
// NDM manages addresses but DOES NOT manage routes. When an address is added
// to a device, the kernel may automatically create a connected route for that
// subnet. If someone removes a kernel-created route, neither the kernel nor
// NDM will restore it. Route management, if needed, must be handled through a
// separate mechanism (e.g., route manager).
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
	// this is a no-op — no reconciliation is enqueued.
	//
	// If a dependency (Master, VLANParent) does not exist in the kernel,
	// reconciliation logs a dependency error and the device is
	// automatically retried when the dependency appears via netlink event.
	//
	// The device is always brought UP after all configuration is applied,
	// regardless of the Flags field in Link.Attrs(). Callers cannot create
	// devices in a permanently DOWN state through this API.
	//
	// Updates do NOT bring the device down. When the configuration changes
	// the new settings are applied while the device remains up. The device
	// transitions through intermediate states that are not fully the old
	// nor the new configuration. Callers are responsible for ensuring that
	// such transitions are safe.
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
}

// DeviceConfig represents the complete desired configuration for a network device.
// Controllers provide the FULL configuration; manager enforces EXACTLY what's provided.
type DeviceConfig struct {
	// Link is the netlink device. Supported types: Bridge, Vxlan, Vlan, Vrf, Dummy.
	//
	// Only specific fields per type are managed by NDM. Setting unsupported fields
	// to non-zero values will cause EnsureLink to return an error.
	//
	// Managed LinkAttrs fields (common to all types):
	//   - Name (device identity, required)
	//   - MTU (0 = kernel default / don't care)
	//   - HardwareAddr (nil = don't care)
	//
	// Managed type-specific fields (see cloneLink for the canonical list):
	//   - Bridge:  VlanFiltering, VlanDefaultPVID
	//   - Vxlan:   VxlanId, SrcAddr, Port, FlowBased, VniFilter, Learning
	//   - Vlan:    VlanId, VlanProtocol
	//   - Vrf:     Table
	//   - Dummy:   (none)
	//
	// All other type-specific fields are unsupported and must remain at zero values.
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
	//
	// Architectural constraint (SVD model): a VID shared by two VXLAN devices
	// on the same bridge is not supported and behavior is undefined.
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
	Isolated      bool // Isolated ports cannot forward frames to each other (bridge link set dev X isolated on)
}
