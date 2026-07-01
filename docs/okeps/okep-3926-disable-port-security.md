# OKEP-3926: Disable Port Security on Secondary Networks

* Issue: [#3926](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/3926)

## Problem Statement

OVN-Kubernetes enforces port security on logical switch ports as follows:

- **Primary network**: MAC and IP anti-spoofing protection is always enabled on all ports.
- **Secondary networks**: MAC anti-spoofing protection is always enabled on all ports. IP
  anti-spoofing protection is enabled when IPAM is active and disabled when `ipam.mode` is
  `Disabled` (since no IPs are assigned).

There is currently no way to disable MAC-level port security on secondary networks. This
prevents legitimate use cases where traffic from multiple MAC addresses must traverse a
single OVN logical switch port, such as nested virtualization or NFV workloads. The bridge
CNI already supports `macspoofchk` for secondary networks, but OVN secondary networks lack
equivalent capability, forcing users who need this flexibility to choose alternative network
backends.

## Goals

- Provide a per-network configuration option to disable port security on IPAM less OVN secondary
  networks on layer2 and localnet topologies to achieve feature parity with the bridge CNI's
  `macspoofchk` capability.
- Enable unknown MAC address delivery.
- Avoid using ARP/NDP flooding as much as possible when port security is disabled.
- Support configuration through the NAD JSON config and the ClusterUserDefinedNetwork (CUDN) CRD
  API only.

## Non-Goals

- Disabling port security on the default cluster network - or any namespace primary network.
  The security implications of removing MAC/IP spoofing protection on the cluster's primary
  network are too significant.
- Per-pod granularity for port security settings. The configuration knob is per-network; all pods
  attached to a given network share the same port security posture.
- Layer3 topology support. Layer3 uses router-based forwarding where port security has different
  semantics and the concept of "unknown L2 addresses" does not apply.
- Replacing or modifying the existing `ipam.mode: Disabled` behavior, which already disables
  IP-level port security as a side effect.
- Replacing or modifying the existing `ipam.mode: Enabled` behavior, which already enables
  IP-level port security as a side effect.
- MAC learning or dynamic FDB management at the OVN-Kubernetes level; OVN manages its own FDB
  natively.
- Namespace-scoped UserDefinedNetwork (UDN) resources cannot enable this feature, as
  disabling port security is a cluster-admin decision that should not be available to namespace
  users.

## Future Goals

- **Per-attachment port security granularity**, probably added via a new `WorkloadOptOut` mode.
  Port security stays enabled by default on the network, but individual pods may opt out. The
  admin controls whether the network permits overrides; the annotation is the per-pod opt-in
  signal. This follows the two-level model proven by OpenStack Neutron.
- **Per-pod allowed MAC addresses**, probably via an `AllowList` mode. This will allow workloads
  to declare additional permitted MAC addresses without fully disabling port security.
- Both extensions are non-breaking enum additions to the existing `PortSecurityConfig` struct.

## Introduction

### OVN Port Security

In OVN, every `Logical_Switch_Port` has a `port_security` column. When populated, it restricts
traffic in two ways:

1. **Ingress (into the logical switch from the workload attached port)**: Only frames whose
   source MAC address (and optionally source IP) match the `port_security` entry are accepted.
   All other frames are dropped.
2. **Egress (from the logical switch to the workload attached port)**: Only frames whose
   destination MAC matches the `port_security` entry are delivered.

OVN-Kubernetes currently sets `port_security` to `"<MAC> <IP1> <IP2>..."` for every pod's LSP,
matching the pod's assigned MAC and IP addresses. This effectively prevents pods from sending or
receiving traffic with any other MAC or IP identity — a sensible default for most workloads.

However, this default breaks scenarios where a single pod legitimately needs to handle traffic
with multiple MAC addresses, such as when the pod runs a hypervisor that bridges traffic from
nested virtual machines.

### The `unknown` Address Keyword

OVN supports an `unknown` keyword in the `addresses` column of a Logical Switch Port. When a
logical switch processes a unicast Ethernet frame whose destination MAC address is not found in
any port's `addresses` column, it delivers the frame to every port whose `addresses` include
`unknown`.

This mechanism is essential for nested virtualization: the hypervisor inside the pod bridges
traffic from VMs whose MAC addresses are not known to OVN. Without `unknown`, unicast frames
destined for those VM MAC addresses would be silently dropped by the logical switch.

### The `force_fdb_lookup` Option

OVN 24.03 introduced the `force_fdb_lookup` option on logical switch ports. When set to `true`
on a port with `unknown` in its addresses, OVN uses the Forwarding Database (FDB) to look up
the destination MAC before delivering the frame, rather than flooding it to all `unknown` ports.

This optimization is critical for production deployments: without it, every frame destined for a
MAC not in OVN's address tables would be flooded to all ports marked `unknown`, creating
significant unnecessary traffic. With FDB lookups, OVN learns where each MAC resides (via
source MAC observation) and delivers subsequent frames directly.

OVN-Kubernetes already pins to OVN version 24.03 or later, so `force_fdb_lookup` is available
in all supported configurations.

### Existing Port Security Behavior with IPAM Disabled

The `IPAMConfig` in the UDN/CUDN CRD API already documents:

> *"By disabling IPAM, IP port security will also be disabled for interfaces attached to this network."*

When `ipam.mode` is set to `Disabled`, OVN-Kubernetes assigns only MAC addresses and provides
layer 2 communication. Since no IP addresses are assigned, IP-level port security cannot be
enforced. However, **MAC-level port security remains active**: the LSP still restricts traffic
to the assigned MAC address. The feature proposed in this OKEP goes further by also disabling
MAC-level port security and enabling `unknown` address handling.

`portSecurity.mode: Disabled` requires `ipam.mode: Disabled`.

### Bridge CNI Parity

The [bridge CNI plugin](https://www.cni.dev/plugins/current/main/bridge/) supports a
`macspoofchk` boolean in the NAD configuration. When set to `false`, nftables/iptables rules
that prevent MAC spoofing are not installed, allowing pods to send traffic with arbitrary source
MAC addresses.

OVN-Kubernetes secondary networks should offer equivalent functionality. Users who currently use
the bridge CNI solely because of `macspoofchk` support should be able to migrate to OVN secondary
networks without losing this capability.

## User-Stories/Use-Cases

### Definition of personas
- **Admin** - the cluster administrator who creates and manages secondary networks.
- **User** - a non-admin user who deploys workloads on secondary networks.
- **VM operator** - an administrator or user running virtual machines via KubeVirt.

### Story 1: KubeVirt Nested Virtualization with Bridged Networking

As a VM operator running [KubeVirt](https://kubevirt.io/), I want to disable port security on a
secondary OVN layer2 network so that VMs running inside VMs can use bridged networking with
their own MAC addresses, enabling those nested VMs to communicate directly on the L2 segment.

**Example scenario:** A KubeVirt VM is in turn running virtualization workloads. Without disabling
port security, OVN drops the frame because the nested VM source MAC does not match the outer VM LSP's
`port_security` entry. With port security disabled, the frame is accepted and forwarded on the
logical switch.

Additionally, other pods (or VMs) on the same L2 network need to send unicast traffic to the nested
VM. Without `unknown` in the addresses column, the logical switch has no port to deliver these
frames to, since the nested VM's MAC is not in any port's `addresses`. With `unknown` addresses and
FDB lookups, the switch learns the association between the nested VM's MAC and the hosting pod's
LSP, and delivers subsequent unicast frames directly.

### Story 2: Network Function Virtualization (NFV)

As a network engineer deploying virtual network functions (VNFs) in Kubernetes, I want to run
firewalls, load balancers, or routers on a secondary OVN network that need to forward traffic
with different source and destination MAC addresses, so that the VNF can operate as a transparent
L2 bridge or as a router performing MAC rewriting.

**Example scenario:** A pair of VMs runs keepalived for high availability on a secondary OVN
layer2 network. The active VM owns a virtual IP and virtual MAC address (e.g., a VRRP/CARP MAC
`00:00:5e:00:01:XX`). It sends gratuitous ARPs and responds to traffic using the virtual MAC.
Port security would drop these frames because the virtual MAC does not match the VM pod's
assigned MAC.

### Story 3: Lab and Test Environment L2 Flexibility

As a developer running a test lab in Kubernetes, I want to attach pods to an OVN layer2
secondary network without MAC restrictions so that I can test L2 protocols (LACP, STP, LLDP, or
custom discovery protocols) that require sending and receiving frames with arbitrary MAC
addresses.

### Story 4: Multi-VM Bridged Networking with Unicast Reachability

As a VM operator, I want multiple VMs behind a single pod's OVN port to be individually
reachable by their MAC addresses on the layer2 network, so that other workloads on the same
network can send unicast traffic to specific VMs without flooding.

**Example scenario:** A pod runs three VMs, each with a distinct MAC address. Another pod on the
same L2 secondary network sends a unicast frame to one of those VMs by MAC. With `force_fdb_lookup`
enabled, OVN learns the MAC-to-port association and delivers the frame directly to the hosting
pod's LSP, rather than flooding it to all `unknown` ports on the switch.

### Story 5: VM Bridging Between Localnet and Layer2 Overlay Networks

As a VM operator, I want to run a VM that acts as a network bridge between an OVN localnet
secondary network and an OVN layer2 overlay secondary network, forwarding traffic between the
two segments. The bridging VM has interfaces on both networks and relays frames with MAC
addresses that differ from its own assigned MACs. Without disabling port security, the relayed
frames are dropped on both networks because their source MACs don't match the VM pod's LSP
`port_security` entries.

## Proposed Solution

### Summary

Introduce a per-network `portSecurity` configuration sub-struct with a `mode` enum
discriminator that, when set to `Disabled`:

1. Leaves the `port_security` column empty on all LSPs attached to the network.
2. Appends `unknown` to the `addresses` column of each LSP.
3. Sets `force_fdb_lookup=true` in the `options` column of each LSP.

The `portSecurity` sub-struct with a `mode` enum discriminator follows the established CRD
pattern used by `VLANConfig` (mode + sub-struct) and `IPAMConfig` (mode + lifecycle).

### API Details

#### NAD JSON Configuration

Add one field to the `NetConf` struct:

```json
{
    "cniVersion": "0.3.1",
    "type": "ovn-k8s-cni-overlay",
    "netAttachDefName": "kubevirt/nested-virt-net",
    "topology": "layer2",
    "name": "nested-virt-net",
    "portSecurityMode": "Disabled"
}
```

The field is optional and defaults to `"Enabled"` (port security enabled, preserving current
behavior). It accepts the values `"Enabled"` and `"Disabled"`. It is valid on `layer2` and
`localnet` topologies only, requires `ipam.mode` to be `Disabled` (i.e., subnets must not be
specified), and the network role must be `"secondary"`. Setting it on a `layer3` topology or
with IPAM enabled results in a validation error.

#### ClusterUserDefinedNetwork CRD

Add a `portSecurity` sub-struct with a `mode` enum discriminator to `Layer2Config` and
`LocalnetConfig` in the CUDN CRD API, following the pattern established by
[OKEP-5085](okep-5085-localnet-api.md) for extending the CUDN CRD and the discriminated
union pattern used by `VLANConfig` and `IPAMConfig`. This field is only supported on
ClusterUserDefinedNetwork (cluster-scoped); namespace-scoped UserDefinedNetwork resources
cannot set this field.

**Layer2 example:**

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: kubevirt-l2-net
spec:
  namespaceSelector:
    matchLabels:
      purpose: kubevirt
  network:
    topology: Layer2
    layer2:
      role: Secondary
      ipam:
        mode: Disabled
      portSecurity:
        mode: Disabled
```

**Localnet example:**

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: provider-net
spec:
  namespaceSelector:
    matchLabels:
      tenant: blue
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: physnet1
      ipam:
        mode: Disabled
      portSecurity:
        mode: Disabled
```

#### CRD Field Definition

New type definitions:

```go
// PortSecurityMode defines the port security enforcement mode for logical switch ports.
// +kubebuilder:validation:Enum=Enabled;Disabled
type PortSecurityMode string

const (
	// PortSecurityModeEnabled restricts traffic on logical switch ports to the assigned
	// MAC (and IP, when IPAM is enabled) addresses. This is the default behavior.
	PortSecurityModeEnabled PortSecurityMode = "Enabled"

	// PortSecurityModeDisabled removes port security restrictions and enables unknown MAC
	// address handling via the OVN `unknown` address keyword with `force_fdb_lookup` for
	// FDB-based unicast delivery.
	PortSecurityModeDisabled PortSecurityMode = "Disabled"
)

// PortSecurityConfig configures port security behavior on logical switch ports attached
// to this network.
// +union
type PortSecurityConfig struct {
	// Mode controls the port security enforcement posture.
	// `Enabled` (default) restricts traffic to assigned addresses.
	// `Disabled` removes all port security restrictions and enables unknown MAC address
	// handling for nested virtualization and NFV use cases.
	// Only `Disabled` requires ipam.mode to be Disabled.
	// +kubebuilder:default=Enabled
	// +kubebuilder:validation:Required
	// +required
	// +unionDiscriminator
	Mode PortSecurityMode `json:"mode"`
}
```

For both `Layer2Config` and `LocalnetConfig`:

```go
// PortSecurity configures port security behavior on logical switch ports attached to this
// network. When omitted, port security defaults to Enabled (the current default behavior).
// Only applicable to Secondary role networks on ClusterUserDefinedNetwork resources.
// +optional
PortSecurity *PortSecurityConfig `json:"portSecurity,omitempty"`
```

#### CRD Validation Rules

The following CEL validations should be added:

- `portSecurity.mode: Disabled` is only allowed when `role` is `Secondary`:
  ```text
  rule: "!has(self.portSecurity) || self.portSecurity.mode != 'Disabled' || self.role == 'Secondary'"
  message: "portSecurity.mode Disabled is only supported for Secondary networks"
  ```

- `portSecurity` is not allowed on namespace-scoped `UserDefinedNetwork` resources.
  Since `UserDefinedNetworkSpec` and `ClusterUserDefinedNetworkSpec` share the same
  `Layer2Config` type, this constraint must be enforced at the `UserDefinedNetworkSpec` level:
  ```text
  rule: "!has(self.layer2) || !has(self.layer2.portSecurity)"
  message: "portSecurity is only supported on ClusterUserDefinedNetwork"
  ```

- `portSecurity.mode: Disabled` requires `ipam.mode` to be `Disabled`:
  ```text
  rule: "!has(self.portSecurity) || self.portSecurity.mode != 'Disabled' || (has(self.ipam) && has(self.ipam.mode) && self.ipam.mode == 'Disabled')"
  message: "portSecurity.mode Disabled requires ipam.mode to be Disabled"
  ```

- `portSecurity.mode` is immutable after creation (changing port security on a live network
  requires reconfiguring all existing LSPs; this can be relaxed in a future iteration):
  ```text
  rule: "!has(oldSelf.portSecurity) || (has(self.portSecurity) && oldSelf.portSecurity.mode == self.portSecurity.mode)"
  message: "portSecurity.mode is immutable after creation"
  ```

  > **Note:** The existing CRD definitions already enforce full immutability at the parent
  > struct level (`self == oldSelf` on `Layer2Config` and `LocalnetConfig`). The per-field
  > CEL rule above provides defense-in-depth and will become the primary constraint if
  > parent-level immutability is relaxed in a future iteration to allow other fields to be
  > updated independently.

#### NAD Generation from CRD

The CUDN controller that generates NADs from CRD specs must translate the
`portSecurity.mode` field to the NAD JSON config `portSecurityMode` field, following the
same pattern used for other fields like `physicalNetworkName`, `allowPersistentIPs`, etc.
When `portSecurity` is omitted in the CRD, the NAD should either omit `portSecurityMode`
or set it to `"Enabled"` (both are equivalent).

#### Interaction with IPAM Mode

| IPAM Mode  | portSecurity.mode      | MAC Port Security | Unknown Addresses | Notes                                |
|------------|------------------------|-------------------|-------------------|--------------------------------------|
| `Disabled` | `Enabled` (default)   | Enabled           | Disabled          | MAC-only security, no IPs assigned   |
| `Disabled` | `Disabled`             | Disabled          | Enabled           | Full L2 flexibility, no restrictions |

#### Future: Per-Attachment Port Security

The `PortSecurityMode` enum is intentionally extensible. A future `WorkloadOptOut` mode is
the anticipated path for per-attachment port security granularity, following the two-level
model proven by OpenStack Neutron where a network-level default can be overridden per port.

With `portSecurity.mode: WorkloadOptOut`, port security remains enabled by default on all
LSPs attached to the network, but individual pods may opt out via a pod annotation (e.g.,
`k8s.ovn.org/port-security-override`). The annotation is only honored on networks where
the cluster admin has explicitly set `WorkloadOptOut`; on `Enabled` or `Disabled` networks
the annotation has no effect. This preserves the admin as the sole decision-maker: the admin
controls whether the network permits overrides, while the annotation is just the opt-in
signal per attachment.

This mode addresses mixed-role networks where a small number of specialized pods (virtual
routers, nested hypervisors, keepalived pairs) need MAC freedom while the remaining pods
benefit from anti-spoofing protection. Without per-attachment granularity, the admin must
either disable port security for all pods or create separate networks for different security
postures.

The same per-attachment mechanism provides the natural integration point for a future
`AllowList` mode with per-pod `allowedMACs`. Rather than fully disabling port security, a
pod annotation could declare additional MAC addresses to append to the LSP's `port_security`
column (e.g., `k8s.ovn.org/allowed-addresses: '{"net": ["00:00:5e:00:01:01"]}'`). OVN's
`port_security` column natively accepts multiple MAC entries per LSP, so this maps directly
to the underlying data model.

Both extensions — `WorkloadOptOut` and `AllowList` — can be added as non-breaking enum
additions to the existing `PortSecurityConfig` struct. The initial `Enabled`/`Disabled` API
proposed in this OKEP is forward-compatible with these future modes.

### Implementation Details

#### OVN Logical Topology

The following diagram illustrates how port security affects traffic flow:

```
 Pod A (VM with MAC fa:16:3e:aa:bb:cc)
   |
   | veth pair
   |
 OVS Bridge (br-int)
   |
   | OVN LSP (port_security: [] if portSecurity.mode: Disabled)
   |          (addresses: ["0a:58:...", "unknown"])
   |          (options: {force_fdb_lookup: "true"})
   |
 OVN Logical Switch (L2 secondary network)
   |
   | OVN LSP for Pod B
   |
 OVS Bridge (br-int)
   |
 Pod B (can send unicast to fa:16:3e:aa:bb:cc via FDB)
```

When `portSecurity.mode` is `Enabled` (default), the LSP has:
- `addresses: ["0a:58:c0:a8:01:03"]`
- `port_security: ["0a:58:c0:a8:01:03"]`
- No `unknown` in addresses, no `force_fdb_lookup`

When `portSecurity.mode` is `Disabled` (requires `ipam.mode: Disabled`), the LSP has:
- `addresses: ["0a:58:c0:a8:01:03", "unknown"]` (MAC only, no IP — IPAM is disabled)
- `port_security: []` (empty)
- `options: {"force_fdb_lookup": "true", ...}`

### Testing Details

#### E2E Tests

- Create a secondary layer2 network with `portSecurity.mode: Disabled`.
- Deploy two pods on the network.
- From pod A, send traffic with a spoofed source MAC address.
- Verify pod B receives the traffic (it would be dropped without the feature).
- Repeat for localnet topology.
- Test with CUDN CRD (not just raw NAD).

#### Cross Feature Testing

- Verify that `allowPersistentIPs` and `portSecurity.mode: Disabled` are mutually exclusive:
  `allowPersistentIPs` requires IPAM enabled (to assign and persist IPs across live migrations),
  while `portSecurity.mode: Disabled` requires `ipam.mode: Disabled`. Validation should reject
  a CUDN that specifies both.

### Documentation Details

- Update `docs/features/multiple-networks/multi-homing.md` with the new configuration option
  for layer2 and localnet topologies, including usage examples.
- Add this OKEP to `mkdocs.yml` under the OKEPs navigation section.

## Risks, Known Limitations and Mitigations

1. **Security risk from disabled port security**: Disabling port security allows any pod on the
   network to send traffic with arbitrary MAC and IP addresses, enabling MAC spoofing, ARP
   spoofing/poisoning (allowing MITM attacks on the L2 segment), and potentially IP spoofing
   attacks.
   - **Mitigation**: The feature is opt-in, per-network, restricted to secondary networks with
     `Secondary` role only, and only available on cluster-scoped `ClusterUserDefinedNetwork`
     resources. Namespace-scoped `UserDefinedNetwork` resources cannot enable this feature,
     ensuring that only cluster administrators can disable port security. The default remains
     secure (port security enabled). Administrators deploying this feature should use encrypted
     transports on networks with port security disabled when sensitive traffic is present.

2. **Broadcast storm risk with unknown addresses**: Adding `unknown` to LSP addresses means
   unicast frames for unknown MACs are delivered to all `unknown` ports, which can create
   significant traffic amplification.
   - **Mitigation**: `force_fdb_lookup` is always enabled alongside `unknown` addresses. The FDB
     learns MAC-to-port associations from observed source MACs, so after initial discovery, frames
     are delivered directly. OVN 24.03+, which is the minimum version used by OVN-Kubernetes,
     supports this option.

3. **Immutability constraint**: The field is immutable after creation in the initial
   implementation, meaning administrators cannot toggle port security on an existing network.
   - **Mitigation**: This is a deliberate simplification. Toggling port security on a live
     network would require updating all existing LSPs atomically. This can be relaxed in a
     future iteration once the reconciliation logic is proven.

## OVN-Kubernetes Version Skew

This feature is planned for introduction in the next OVN-Kubernetes release. It requires
OVN 24.03 or later for the `force_fdb_lookup` optimization, which is already the minimum OVN
version supported by OVN-Kubernetes.

No special version skew handling is required: the feature is a per-network configuration option.
Controllers running older versions that do not recognize the `portSecurity` sub-struct will
ignore the field (it defaults to `Enabled`), and existing networks will continue to function
with port security enabled.

## Backwards Compatibility

- The `portSecurity` sub-struct is optional. When omitted, the default behavior is `Enabled`,
  preserving the current behavior of enforcing port security on all LSPs.
- The NAD JSON config `portSecurityMode` field is additive and optional. Existing NAD
  configurations without the field continue to work identically.
- The CRD field is an optional pointer with nil default. Existing CUDN resources are unaffected.
  UDN resources cannot use this field with `mode: Disabled`.
- No migration or upgrade steps are required.

## Alternatives

### Alternative 1: Two Separate Knobs (PR #4377 Original Design)

The original implementation in [PR #4377](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/4377)
proposed two independent boolean fields:
- `disablePortSecurity`: controls the `port_security` column on LSPs.
- `enableL2Unknown`: controls whether `unknown` is added to `addresses` and whether
  `force_fdb_lookup` is set.

**Why rejected:** There is no identified use case for controlling these behaviors independently.
If port security is disabled (allowing arbitrary source MACs), the network must also handle
`unknown` destination MACs — otherwise, return traffic to spoofed MAC addresses would be dropped.
Conversely, enabling `unknown` addresses without disabling port security is contradictory: the
port would accept traffic for unknown MACs but still enforce source MAC restrictions.

Two independent booleans create a 2×2 configuration matrix where two of the four states are
invalid or nonsensical. A single consolidated knob eliminates this confusion.

### Alternative 2: Per-Pod Annotation

Instead of a per-network setting, port security could be controlled via a per-pod annotation
(e.g., `k8s.ovn.org/allow-mac-spoofing: "true"`).

**Why rejected:**
- **Security**: Any pod (or user with pod-create access) could potentially set the annotation,
  widening the attack surface. A per-network setting on the cluster-scoped CUDN resource
  requires cluster-admin intervention.
- **Consistency**: The bridge CNI's `macspoofchk` is per-network, and OVN-Kubernetes should
  follow the same granularity for consistency.
- **Semantics**: Port security is a property of the network infrastructure, not of individual
  workloads. A mixed network where some pods have port security and others don't creates complex
  and hard-to-reason-about security boundaries.
- Per-pod control could be considered as a future extension if a use case emerges.

### Alternative 3: NAD-Only (No CRD Integration)

Keep the feature in the NAD JSON configuration only, without adding a field to the CUDN CRD.

**Why rejected:** The OVN-Kubernetes community is moving toward structured CRD APIs for network
configuration. [OKEP-5085](okep-5085-localnet-api.md) established the pattern of defining
features in the CUDN CRD with automatic NAD generation. Adding a feature only to NADs creates
a management gap where CUDN-managed networks cannot use the feature without manual NAD editing,
which defeats the purpose of the CRD controller.

### Alternative 4: Three-Valued Enum

Instead of two modes, use a three-valued enum `portSecurity.mode: Enabled | MACOnly | Disabled`:
- `Enabled` (default): full MAC + IP port security.
- `MACOnly`: only MAC port security, no IP restrictions.
- `Disabled`: no port security at all.

**Why rejected:** This is over-engineering for the current use cases. The `MACOnly` option has
no identified user demand. The `ipam.mode: Disabled` setting already handles the case where IP
port security is not applicable (no IPs to secure). Adding a third mode increases API surface
area and testing complexity without clear benefit. It can be added later as a non-breaking
change (widening the enum validation set) if a use case emerges.

### Alternative 5: Boolean Field

Use a simple boolean field `allowUnknownAddresses: bool` directly on `Layer2Config` and
`LocalnetConfig`, defaulting to `false`.

**Why rejected:** Kubernetes API conventions discourage boolean fields because they cannot grow
to accommodate future requirements. If a third security posture is needed later (e.g., an
allow-list mode for VRRP/keepalived), a boolean requires either a second field (creating
field interaction complexity) or a breaking API change. The `portSecurity` sub-struct with a
mode enum discriminator follows the established `VLANConfig` and `IPAMConfig` patterns in the
CRD, provides a natural extension point for future modes and per-mode parameters, and costs
only marginally more boilerplate than a boolean.

## References

- Feature request issue: [#3926](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/3926)
- Initial implementation PR: [#4377](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/4377)
- OVN Northbound DB schema — Logical_Switch_Port table: `port_security`, `addresses` columns
  and `force_fdb_lookup` option
  ([ovn-nb man page](https://man7.org/linux/man-pages/man5/ovn-nb.5.html))
- OVN 24.03 release notes — `force_fdb_lookup` introduction:
  [NEWS](https://github.com/ovn-org/ovn/blob/main/NEWS)
- Bridge CNI `macspoofchk` documentation:
  [CNI plugins — bridge](https://www.cni.dev/plugins/current/main/bridge/)
- OKEP-5085 — Localnet API (CRD design pattern reference):
  [okep-5085-localnet-api.md](okep-5085-localnet-api.md)
- OKEP-5193 — User Defined Networks (UDN CRD API):
  [okep-5193-user-defined-networks.md](okep-5193-user-defined-networks.md)
