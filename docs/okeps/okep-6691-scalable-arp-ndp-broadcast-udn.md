# OKEP-6691: Scalable ARP/NDP Broadcast Handling for UDN

* Issue: [#6691](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6691)

## Problem Statement

Certain inbound ARP/NDP control traffic entering br-ex is replicated to every UDN gateway patch port on the node. Each additional UDN adds another OVS resubmission path (approximately 100 resubmits per patch-port traversal), so the pipeline can reach OVS's hard-coded 4096-resubmit limit at roughly 35–50 UDNs. The result is packet loss, CPU cost that grows with the UDN count, and loss of external connectivity, which limits UDN scalability.

## Goals

- Scale to 500 primary UDNs per node without hitting the 4,096 resubmit limit.
- Eliminate O(N) ARP/broadcast fan-out across UDN pipelines on br-ex.
- Handle both IPv4 ARP and IPv6 NDP.
- Deliver with low side-effect risk.

## Non-Goals

- Changing the shared MAC architecture.
- OVN core changes (northd, ovn-controller, SB-DB/NB-DB schema).
- Addressing non-ARP/NDP broadcast traffic.
- DPU mode support.

## Introduction

This is a **mid-term fix** intended to be delivered quickly with limited architectural changes and complexity. Solutions that require OVN core changes (schema/northd/ovn-controller modifications), topology rework, or deeper architectural changes are deferred to a separate follow-up long-term effort.

### Per-UDN External Topology

Each primary UDN creates a complete, isolated set of OVN logical objects per node. On the external side, every UDN gets its own Gateway Router (GR) and external logical switch (ext\_LS), connected to br-ex via a localnet/patch port:

```text
                          Physical Network
                                |
                          [ br-ex / ens5 ]
                        MAC: 06:38:d6:0b:4b:a7    (node's physical NIC MAC)
                                |
                  OVS bridge with per-network patch ports
                 /              |              \              \
   [ext_<node>]      [ext_net1_<node>]    [ext_net2_<node>]   ...
   (localnet)         (localnet)           (localnet)
        |                  |                    |
  [GR_<node>]       [GR_net1_<node>]    [GR_net2_<node>]      ...
  default network   UDN "net1"          UDN "net2"
```

All ext\_LS localnet ports map to the same physical network bridge (`"physnet"` → br-ex) through patch ports. UDN patch ports are configured with the `NO_FLOOD` property so that the `NORMAL` and `FLOOD` actions on bridge flows don't output to them.

**Secondary localnet UDN patch ports:** In addition to GR ext\_LS patch ports, secondary localnet UDNs ([OKEP-5085](okep-5085-localnet-api.md)) mapped to the same bridge create their own OVN-controller-managed patch ports on br-ex. These ports are **not** configured with `NO_FLOOD`, so `NORMAL` and `FLOOD` actions reach them. Throughout this document, references to "`no-flood` excludes CUDN patches" do not apply to secondary localnet patch ports.

### The Shared MAC

All GR external ports (`rtoe-GR_*`) use the **same MAC address**, the node's physical NIC MAC.

Giving each GR a unique MAC is a significantly more complex change that is being evaluated as part of the long-term effort.

### How ARP and NDP Are Currently Forwarded

#### Outbound ARP (GR resolves upstream gateway)

When a UDN GR needs to reach an external IP, it must first resolve the upstream gateway's MAC via ARP. The GR sends a broadcast ARP request using its external port identity: the shared MAC and node IP.

1. ARP request exits the UDN's ext\_LS localnet port onto br-ex.
2. The priority-10 egress flow matches: `in_port=<patch>, dl_src=bridgeMAC → output:NORMAL`.
3. `NORMAL` action performs standard L2 broadcast flooding, the ARP outputs on all ports, including `ofPortPhys` as intended, except UDN patch ports which are configured with NO_FLOOD property.

#### Inbound Unicast ARP Reply (Gateway Responds)

The upstream gateway sends a unicast ARP reply addressed to the shared MAC.

1. Reply arrives on br-ex via `ofPortPhys`.
2. It matches the priority-10 unicast flood rule: `dl_dst=<bridgeMAC> → output:patch1,patch2,...,patchN,NORMAL`
3. This rule explicitly outputs to **ALL** N patch ports sequentially.
4. Each patch port delivers the ARP into its ext\_LS, which forwards to its GR. OVS processes all N pipelines sequentially.
5. At high UDN counts: total resubmits exceed 4,096 → OVS drops the packet.

This unicast flood rule exists because all GRs share the same MAC, OVS MAC learning and conntrack cannot determine which single patch port should receive the frame.

#### Inbound Broadcast ARP

##### External Host Asks "Who Has NodeIP?"
1. Broadcast ARP request (`arp_op=1`, `arp_tpa=<nodeIP>`) arrives on br-ex via `ofPortPhys`.
2. It matches the priority-12 rule: `arp, arp_op=1, arp_tpa=<nodeIP> → output:<default_patch>,NORMAL` (added in [PR #6660](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6660)).
3. Only the CDN GR patch receives the packet explicitly; `NORMAL` delivers to LOCAL (kernel) via broadcast flooding. CUDN patch ports are excluded by `no-flood`.
4. Only the CDN GR (and potentially the kernel on LOCAL) responds — no N duplicate replies.

#### External Host GARP

When an external host sends broadcast ARP that does **not** target the node IP — for example, a GARP announcing a gateway MAC change (`arp_tpa ≠ nodeIP`) — it is not caught by priority-12 and instead matches the priority-11 rule:

1. Broadcast ARP arrives on br-ex via `ofPortPhys`.
2. It matches the priority-11 rule: `in_port=<ofPortPhys>, dl_dst=ff:ff:ff:ff:ff:ff, arp → output:patch1,...,patchN,NORMAL`
3. Each patch port delivers the ARP into its ext\_LS, which forwards to its GR. OVS processes all N pipelines sequentially.
4. At high UDN counts, total resubmits can exceed 4,096 → OVS drops the packet.

This explicit fan-out is intentional: with `no-flood` on CUDN patch ports, external GARPs would otherwise never reach CUDN GRs, leaving their MAC\_Bindings stale.

#### NDP (IPv6 Neighbor Discovery)

- **Outbound NS/NA (UDN resolves external neighbor or responds to NS):** Same as ARP outbound — the priority-10 egress rule sends it to NORMAL, which forwards/floods out the physical port only (CUDN patches excluded by no-flood).
- **Inbound NS targeting the node IP:** Matches priority 12 based on `icmpv6_type=135, nd_target=<nodeIP>`. It goes to the default-network GR plus NORMAL/LOCAL; CUDN GRs are excluded.
- **Inbound NS targeting some other IP:** Falls through to NORMAL, which excludes no-flood CUDN patches.
- **Inbound unicast solicited NA:** Still hits priority 50/conntrack, then table 1 priority 14 explicitly outputs to every CUDN patch followed by FLOOD. This remains an N-way fan-out.
- **Inbound unsolicited multicast NA:** Priority 11 explicitly sends it to all GR patches—the IPv6 equivalent of preserving GARP propagation.

#### Current br-ex Flow Table

| Table | Pri | Match | Action | Purpose |
|-------|-----|-------|--------|---------|
| 0 | 50 | `ip/ipv6, dl_dst=<bridgeMAC>` | `ct(zone=...,nat,table=1)` | IP/IPv6 return traffic → conntrack (NDP RA/NA hit this) |
| 0 | 12 | `arp, arp_op=1, arp_tpa=<nodeIP>` | `output:<default_patch>,NORMAL` | Node-IP ARP request → CDN GR + LOCAL only |
| 0 | 12 | `icmp6, icmpv6_type=135, nd_target=<nodeIP>` | `output:<default_patch>,NORMAL` | Node-IP NS → CDN GR + LOCAL only |
| 0 | 11 | `in_port=<phys>, dl_dst=ff:ff:ff:ff:ff:ff, arp` | `output:<all-patches>,NORMAL` | External GARP → all GR patches |
| 0 | 11 | `in_port=<phys>, dl_dst=33:33:00:00:00:01, icmp6, icmpv6_type=136` | `output:<all-patches>,NORMAL` | Unsolicited multicast NA → all GR patches |
| 0 | 10 | `dl_dst=<bridgeMAC>` | `output:<all-patches>,NORMAL` | Unicast to bridge MAC (ARP replies, etc.) → all patches |
| 0 | 10 | `in_port=<patch>, dl_src=<bridgeMAC>` | `NORMAL` | Egress from GR with correct source MAC → standard forwarding |
| 0 | 9 | `in_port=<patch>` | `drop` | Drop patch-port traffic with wrong source MAC |
| 0 | 0 | *(catch-all)* | `NORMAL` | Standard L2 forwarding (excludes no-flood CUDN patches) |
| 1 | 14 | `icmp6, icmpv6_type=134` | `output:<CUDN-patches>,FLOOD` | RA → all CUDN patches + flood |
| 1 | 14 | `icmp6, icmpv6_type=136` | `output:<CUDN-patches>,FLOOD` | NA → all CUDN patches + flood |

Priority-12 and `no-flood` (PR #6660) already eliminated the node-IP ARP/NS storm and outbound fan-out. The remaining problem flows are priority-10 (unicast ARP replies), priority-11 (GARP/unsolicited NA), and table 1 priority-14 (RA/NA after conntrack) all of which still explicitly output to every CUDN patch port.

## User-Stories/Use-Cases

Story 1: Reliable external connectivity at scale

**As a** cluster admin deploying 50+ primary UDNs per node,
**I want** ARP and NDP neighbor resolution to work reliably regardless of UDN count
**so that** external connectivity is not broken by OVS resubmit limits.

At roughly 35-50 UDNs, the current ARP/NDP handling exceeds the OVS 4,096 resubmit limit, causing drops that prevent MAC resolution and break external connectivity for UDN workloads.

Story 2: Efficient broadcast handling at high UDN density

**As a** platform engineer scaling to 500 UDNs per node,
**I want** ARP and NDP neighbor resolution traffic to not fan out across all UDN pipelines
**so that** CPU is not wasted on unnecessary per-pipeline packet processing.

Even below the resubmit limit, every ARP packet traverses all N UDN pipelines unnecessarily, consuming CPU proportional to UDN count.

## Proposed Solution

### Solution Overview

Two mechanisms work together to eliminate ARP/NDP fan-out:

1. **Traffic Steering** (4 priority-52 flows per external-ingress port): Intercepts all inbound ARP and NDP from external-ingress ports above the existing paths that bypass `no-flood` (priority-10 and 50 flows), and redirects it to `default_patch` + `NORMAL` instead, so only the CDN GR and the kernel (via LOCAL) see it. An **external-ingress port** is any OVS port on the bridge that carries inbound ARP/NDP from outside OVN's GR pipeline: `ofPortPhys` (physical uplink) and secondary localnet UDN patch ports. When `enable-scalable-arp-ndp` is enabled, existing ARP/NDP fan-out flows (priority-10/11/12, table-1 priority-14) are not rendered.
2. **MAC_Binding Propagation** (for both IPv4 and IPv6): A new component propagates the CDN GR's resolved neighbor bindings to every UDN GR via direct SB-DB writes.

**Generalizing when Uplink feature is enabled:** The above describes the scenario when [Uplink](okep-6019-vrf-lite-shared-gateway-external-bridges.md) is not enabled, i.e. with the CDN GR as source (from where the MAC bindings are copied from) and every UDN GR as a follower (that gets a copy of the MAC bindings). More generally, networks that share a physical OVS bridge form a **group**, with one GR designated as the **source** and every other GR on that bridge acting as a **follower**. A UDN can instead be attached to a separate OVS bridge via the `Uplink` CRD ([OKEP-6019](okep-6019-vrf-lite-shared-gateway-external-bridges.md)); no CDN GR exists on that bridge, so the `openflow manager` designates one of its UDN GRs as the source, and the remaining UDN GRs on that same `Uplink` are followers. Both mechanisms above apply identically to any group, substituting "source GR" for "CDN GR" and "follower GR" for "UDN GR". How the uplink source is first chosen and later replaced is covered under **Source designation** and **Lifecycle Hooks** below. Secondary localnet UDNs cannot currently use Uplinks (blocked by CRD validation), so the external-ingress set on Uplink bridges covers only the physical port.

**Note on the following scenarios:** Scenarios 1-3 are drawn for the "default group" on `br-ex`, where `default_patch` is the CDN GR's patch port. On an `Uplink`-backed bridge, the same flow patterns apply unchanged, but `default_patch` refers to that bridge's designated **source** UDN GR's patch port instead (there is no CDN GR on an uplink bridge).

#### Scenario 1: Outbound ARP from UDN GR

```text
  UDN patch port
       │
  broadcast ARP request (arp_op=1, dl_src=bridgeMAC)
       │
       ▼
  ┌─ pri 10: Existing non-IP egress ─────────────┐
  │  in_port=<patch>, dl_src=bridgeMAC           │
  │  → output:NORMAL                             │
  │                                              │
  │  NORMAL with no-flood:                       │
  │    ofPortPhys ───────────────────────────────┼──▶ to physical network
  │    LOCAL (harmless — kernel ignores ARP      │    (real ARP on wire)
  │           from own identity)                 │
  │    default_patch (harmless — CDN GR          │
  │           ignores ARP for IPs it doesn't own)│
  │    secondary localnet patches (no no-flood   │
  │           — VMs see ARP from shared node IP) │
  └──────────────────────────────────────────────┘

  ✗ NOT flooded to CUDN patches (no-flood)
```

#### Scenario 2: Inbound Unicast ARP Reply

```text
  External-ingress port (ofPortPhys or secondary localnet patch)
       │
  unicast ARP reply (arp_op=2, dl_dst=bridgeMAC)
       │
       ▼
  ┌─ pri 52: External-Ingress ARP Steering ─────┐
  │  in_port=<ext-ingress>, arp                 │
  │  → output:default_patch, NORMAL             │
  └──────────┬──────────────────────────┬───────┘
             │                          │
             ▼                          ▼
        default_patch                 NORMAL
        (CDN GR creates               (1) FDB learning: records
         MAC_Binding in SB-DB)             src_MAC→in_port.
              │                       (2) FDB lookup: bridgeMAC→LOCAL
        SB-DB event →                     delivers to kernel. 
        MAC Binding Controller propagates
        MAC_Binding to all UDN GRs →
        ovn-controller installs flows →
        buffered packets reinjected

  ✗ NOT flooded via output:patch1,...,patchN (intercepted above pri 10)
  ✗ UDN patches never see inbound ARP replies
```

NOTE: The requesting UDN GR does **not** receive the reply/NA directly, only the CDN GR does. Propagation and packet reinjection complete resolution for the requesting UDN.

#### Scenario 3: Ingress GARP

```text
  External-ingress port (ofPortPhys or secondary localnet patch)
       │
  broadcast GARP (dl_dst=ff:ff:ff:ff:ff:ff, arp_tpa=<announcer's own IP>)
       │
       ▼
  ┌─ pri 52: External-Ingress ARP Steering ─────┐
  │  in_port=<ext-ingress>, arp                 │
  │  → output:default_patch, NORMAL             │
  └──────────┬──────────────────────────┬───────┘
             │                          │
             ▼                          ▼
        default_patch                 NORMAL
        (CDN GR learns                (FDB learning + broadcast
         announcer's MAC,              flood to LOCAL only —
         updates MAC_Binding →         kernel updates its own
         update propagated to          neighbor table)
         all UDN GRs)

  ✗ NOT flooded to CUDN patches (with no-flood configured)
```

#### IPv6 NDP

NDP traffic isolation uses the same unified steering approach as ARP: priority-52 steers all inbound NDP NAs from each external-ingress port (ofPortPhys or secondary localnet patch) above conntrack to `default_patch` + NORMAL (NORMAL delivers to LOCAL via FDB/flooding and performs MAC learning), and priority-52 steers all inbound NDP NS to the same destinations. UDN outbound NDP NS is handled by `no-flood`, it falls to the existing priority-10 `output:NORMAL` which with `no-flood` delivers to the wire without CUDN fan-out. Neighbor resolution itself uses the same mechanism as IPv4: the CDN GR's resolved IPv6 neighbors are propagated to UDN GRs via SB-DB `MAC_Binding` writes (see [MAC_Binding Propagation](#mac_binding-propagation)).

### API Details

#### Feature Gates

One new flag is added:

| Flag | Scope |
|------|-------|
| `enable-scalable-arp-ndp` | IPv4 + IPv6: priority-52 ARP/NDP steering (4 flows per external-ingress port) + MAC\_Binding propagation for both protocols |

Defaults to `false` and requires `enable-network-segmentation` (validated at startup).
Changing the flag requires an ovnkube-node restart, which triggers a full flow sync.


**Why not reuse `enable-network-segmentation`:** It is already `true` in production. A separate gate allows code to be merged incrementally and flipped only when the pipeline is proven working.

### Implementation Details

This feature applies to both LGW and SGW gateway modes. The br-ex flow table and patch port topology are the same in both modes.

#### Traffic Isolation and Steering

See [Complete Flow Priority Table](#complete-flow-priority-table-br-ex-table-0) for the full picture with match/action details.

**Per-bridge generation:** The following flows are generated independently **per bridge**. On `br-ex`, `default_patch` is the CDN GR's patch port. On an `Uplink`-backed bridge, `default_patch` would be replaced with the patch port of that bridge's designated source UDN GR. When the `openflow manager` re-designates the source on an uplink bridge (e.g. because the previous source UDN was removed), it updates that bridge's steering flows to point at the new source's patch port.

**Priority-52 flows** intercept all inbound ARP and NDP from external-ingress ports above the two existing paths that bypass `no-flood`:

1. The priority-10 unicast flood rule (`dl_dst=bridgeMAC → output:patch1,...,patchN,NORMAL`) uses explicit `output:<port>` actions that ignore `no-flood` entirely. Without the priority-52 ARP flow, unicast ARP replies would still fan out to all N patches.
2. The priority-50 conntrack flow (`ipv6, dl_dst=bridgeMAC → ct(table=1)`) sends unicast NDP NAs and RAs to table 1, where flows include explicit `output:<all-CUDN-patches>` that also bypasses `no-flood`. Without the priority-52 NA/RA flows, NAs and RAs would still reach all N CUDN pipelines. Priority 52 also avoids processing NDP through a conntrack path that cannot create CT entries ([kernel bug #11797](https://bugzilla.kernel.org/show_bug.cgi?id=11797): IPv6 conntrack treats NDP packets as invalid; workaround at [bridgeflows.go](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/bfe54f76ac802fba3a6d80b27cb348784140a75c/go-controller/pkg/node/bridgeconfig/bridgeflows.go#L1157-L1165)).

NDP flows must be above the priority-50 conntrack flow to bypass a conntrack path that cannot create CT entries for NDP; ARP is unified at the same priority to simplify the pipeline. When `enable-scalable-arp-ndp` is enabled, the existing priority-10 `dl_dst=bridgeMAC` fan-out, priority-11 GARP/NA fan-out, priority-12 node-IP steering, and table-1 priority-14 RA/NA fan-out are **not rendered**.

**Priority-52 NDP NA flow:** Steers all inbound Neighbor Advertisements (type 136) from each external-ingress port to `default_patch` + NORMAL, above conntrack. The `dl_dst` match is omitted to catch both unicast NAs (`dl_dst=bridgeMAC`) and multicast unsolicited NAs (`dl_dst=33:33:00:00:00:01`). `NORMAL` performs FDB learning and delivers to LOCAL (via static FDB entry for unicast, or flooding for multicast).

**Priority-52 NDP RA flow:** Same structure as the NA flow but matches Router Advertisements (type 134). CUDN GRs do not need RAs (OVN logical routers are statically configured); the only legitimate consumer is the kernel on `breth0` (reached via `NORMAL`).

**Priority-52 NDP NS flow:** Steers all inbound Neighbor Solicitations (type 135) from each external-ingress port to `default_patch` + NORMAL, above conntrack. This completes the unified NDP steering: all three NDP message types (NA, RA, NS) follow the same centralized path to the CDN GR, bypassing the priority-50 conntrack flow.

**Priority-52 ARP flow:** Steers all inbound ARP (request and reply, unicast and broadcast) from each external-ingress port to `default_patch` + NORMAL. `default_patch` must be explicit: NORMAL's FDB lookup for unicast (`dl_dst=bridgeMAC`) resolves only to LOCAL via the static FDB entry, never to `default_patch`. `NORMAL` still performs FDB learning and delivers to LOCAL (via the static FDB entry for unicast replies, or broadcast flooding for requests).

#### External-Ingress Port Discovery

An **external-ingress port** is any OVS port on the bridge that carries inbound ARP/NDP from entities outside OVN's GR pipeline. This includes:

- `ofPortPhys` (the physical uplink)
- Secondary localnet UDN patch ports (created by OVN controller when a secondary localnet UDN is mapped to the bridge)

Secondary localnet UDNs ([OKEP-5085](okep-5085-localnet-api.md)) do not have GRs, but same-node GR-to-localnet-VM ARP traverses their patch ports. Today, unicast ARP replies from localnet VMs hit the priority-10 `dl_dst=bridgeMAC` fan-out flow (which has no `in_port` match), causing the same O(N) fan-out to all GR patches that this OKEP eliminates. Including secondary localnet patch ports in the external-ingress set ensures their ARP is steered to the source GR at priority 52 instead. With all external-ingress ARP steered, the priority-10 fan-out flow is no longer reachable and is not rendered.

The external-ingress port set is updated on network lifecycle events (creation, deletion), same as GR patch ports.


#### MAC_Binding Propagation

For any neighbor a group's source GR has already resolved, the `MAC Binding Controller` propagates that resolution to every follower GR in the same group by writing a `MAC_Binding` entry directly to SB-DB for each follower GR's external port. This turns O(N) fan-out into a single control-plane propagation step, and applies identically to IPv4 (ARP) and IPv6 (NDP). 
These entries are dynamic (subject to `mac_binding_age_threshold`, default 300s) and benefit from OVN's built-in lifecycle management.

**Source designation:** For the default group the source is always the CDN GR, there is nothing to designate. For an uplink group, the `openflow manager` (which already owns that bridge's steering-flows) designates one of the UDN GRs on the bridge as source. The source is the network whose GR patch port is the one that will receive ARP/NDP replies from the wire. Since all UDNs on the same uplink bridge share the same L2 domain, any UDN's GR resolves the same neighbors; the selection among available UDNs is arbitrary. When the current source UDN is removed, the openflow manager picks any remaining UDN on the bridge, updates the steering-flows to point at it (see [Traffic Isolation and Steering](#traffic-isolation-and-steering)), and informs the `MAC Binding Controller`.

For brevity, the remainder of this section (Mechanism, Event Handling, MAC_Binding Lifecycle, Probing Amplification) illustrates the mechanism using the default group's terminology ("CDN GR", "UDN GR"); the same behavior applies identically within any uplink group by substituting "source GR" and "follower GR".

##### Mechanism

1. When any GR (default or UDN) resolves a neighbor (via ARP Request or NDP NS), the ARP reply/NA is steered to `default_patch` by the priority-52 flow ([Scenario 2](#scenario-2-inbound-unicast-arp-reply)), and the CDN GR creates or updates a dynamic `MAC_Binding` entry in SB-DB. When a UDN GR triggers resolution, it does not receive the reply/NA directly, only the CDN GR does. OVN buffers the original IP packet that triggered the ARP/NS request for up to 10 seconds (limits: 1000 unique destinations, 4 packets per destination).
2. The `MAC Binding Controller` watches `MAC_Binding` changes via the libovsdb SB-DB event handler, filtering for entries on tracked source GR ports (e.g. `rtoe-GR_<node>` for the default group).
3. When a binding appears or its MAC changes, the watcher writes a `MAC_Binding` entry directly to SB-DB for each UDN GR's external port (`rtoe-GR_*`) for that `(IP, MAC)` pair with a fresh timestamp, in a single batched transaction. ovn-controller installs the corresponding flows incrementally (no northd involvement) and reinjects any buffered packets for that destination. If the propagated binding is not yet installed while packets remain buffered, they wait; if the buffer window elapses first, the next application packet retriggers step 1.
4. Bidirectional UDN traffic (e.g. TCP) keeps entries alive via `MAC_CACHE_USE` (return traffic refreshes timestamp). Entries never expire while bidirectional traffic flows.
5. Idle entries expire after 300s (without controller involvement); on next traffic, resolution repeats from step 1.

**Scale:** This produces `N x M` MAC_Binding entries per node per protocol, where N is the number of UDNs and M is the number of neighbors the CDN GR has resolved for that protocol. M is driven by the GR's **connected route** for its external subnet. The GR's external port (`rtoe-GR_*`) is assigned the node's IP with a prefix length, which creates an implicit connected route for the entire subnet. Since all cluster nodes sit on the same external subnet, the best-case M = default gateway + number of nodes, for each of IPv4 and IPv6. At 500 UDNs and 500 nodes, that is ~250K MAC_Binding entries per node per protocol (~500K entries per node with both IPv4 and IPv6 enabled). Every resolved binding is propagated to **all** UDN GRs regardless of which UDN triggered the resolution (the returning reply/NA is steered to `default_patch` with no correlation to the originating UDN).

##### Event Handling

- **ADD** (new IP resolved): Write `MAC_Binding` for all UDN GRs with same `(IP, MAC)` and fresh timestamp in a single batched transaction. ovn-controller installs flows incrementally (no northd involvement).
- **UPDATE** (MAC changed): Update all UDN GR MAC_Bindings with the new MAC.
- **UPDATE** (timestamp refreshed, same MAC): Write a fresh timestamp to all UDN GR MAC_Bindings for that IP. This keeps a UDN GR's binding alive even when that UDN GR has no traffic of its own for its local `mac_cache_use` to refresh it directly.

  Timestamp-only UPDATE events come from the CDN GR's own `mac_cache_use` [periodic sweep](https://github.com/ovn-org/ovn/blob/a9d49f5629022657c77f79c383214ff27a63c11d/controller/mac-cache.c#L399-L445), not from individual packet arrivals: an ARP reply or NDP NA for an already-known neighbor does not, by itself, cause an immediate UPDATE event. Instead it just keeps that neighbor's MAC binding "marked as active," and it's the next periodic sweep (roughly every [~56s](https://github.com/ovn-org/ovn/blob/a9d49f5629022657c77f79c383214ff27a63c11d/controller/mac-cache.c#L95-L96) for the default `mac_binding_age_threshold` of 300s) that performs the actual timestamp refresh, based on whether the binding has been "active" since the last sweep. IP/IPv6 traffic (which includes NDP NA/NS), ARP replies, and ([recently](https://patchwork.ozlabs.org/project/ovn/patch/20260910085224.2004760-1-amusil@redhat.com/)) ARP requests sent by the tracked `(MAC, IP)` itself count towards keeping the binding "active". If none of the above occurs before the next sweep, no UPDATE is generated and the binding is left to age out normally. Because the sweep is periodic rather than per-packet, timestamp UPDATEs for all of a CDN GR's actively-used bindings tend to arrive in a burst. Implementations can take advantage of this by coalescing a burst into a single batched transaction to minimize SB-DB writes; likewise reconciling each binding independently, paired with a cooldown that skips a rewrite if a follower's row was refreshed recently enough, is an equally valid approach to reduce SB-DB writes.
- **DELETE** (CDN GR binding aged out): No action needed. UDN GR entries have independent timestamps and are managed by OVN's own lifecycle:
  - If UDN traffic is bidirectional → `MAC_CACHE_USE` keeps the UDN entry alive independently.
  - If UDN traffic is idle → the UDN entry ages out on its own. On next traffic, the CDN GR re-resolves → ADD event → re-propagated.

**Potential improvement:** If timestamp-driven write amplification becomes a concern at extreme scale, the controller could switch to periodic reconciliation: sweep every half binding expiration time, refresh UDN entries approaching expiry in one batch.

##### MAC_Binding Lifecycle

Dynamic `MAC_Binding` entries expire after `mac_binding_age_threshold` (default 300s) unless actively refreshed. OVN provides two complementary mechanisms that extend binding lifetime based on traffic:

1. **`mac_cache_use`** ([commit `33bb66c`](https://github.com/ovn-org/ovn/commit/33bb66c6c8e6e119bf9006dbe868457eecf82c9e)): the periodic sweep described in [Event Handling](#event-handling). **Bidirectional traffic** keeps a binding alive indefinitely: return traffic from the remote endpoint continually re-marks it as active before each sweep.

2. **Stale probes** (OVN 25.03+, [commit `58ce60d`](https://github.com/ovn-org/ovn/commit/58ce60d2f1d932b842512763c2b8fc0943e1f8e3)): ovn-controller periodically checks `MAC_Binding` entries approaching expiry and sends a unicast ARP request / NDP NS to refresh them. The probing decision is based on **egress** traffic activity i.e. whether the router has recently forwarded traffic *to* that destination. The reply refreshes the binding. This keeps bindings alive even for **unidirectional outbound traffic**. Important: this requires the [probing idle-guard fix](#probing-amplification) to correctly skip entries whose egress flows have never been hit.

Entries that are neither refreshed nor probed (idle) expire at 300s. On next traffic, the router re-resolves from scratch.

**CDN GR binding (propagation source):** Subject to the lifecycle mechanisms described above. For destinations the default network actually uses (gateway, other nodes), `mac_cache_use` and stale probes keep the binding alive directly from that traffic. For destinations reached only via UDN traffic, the CDN GR has no *IP* traffic of its own (UDN-bound data traffic is redirected by conntrack directly into the specific UDN's own pipeline, never reaching the CDN GR). This rarely leaves the binding idle, though: any inbound ARP request or NDP NS whose target matches the router's own IP (the shared node IP, identical on every GR) creates a `MAC_Binding` entry from the requester's `(IP, MAC)` regardless of the OVN `always_learn_from_arp_request` setting (OVN-Kubernetes default `false`), and any *subsequent* ARP/NDP of any kind from that same `(MAC, IP)` refreshes it in place, propagated by the `MAC Binding Controller` to all N follower GRs either way. This **ARP/NDP-request-driven keep-alive** contributes on keeping the full `M`-sized neighbor set (default gateway + every other cluster node) continuously resolved on the CDN GR and continuously propagated to all N follower GRs, regardless of whether any UDN is actually forwarding traffic to a given neighbor.

Whenever the CDN GR's binding is refreshed (by either mechanism), the `MAC Binding Controller` mirrors the timestamp update to all UDN GR bindings for that destination. This propagation is an **additional** refresh source for UDN GR bindings, on top of the OVN mechanisms that apply to them independently from their own traffic. When propagation stops (CDN GR binding expires), UDN GR bindings fall back to their own traffic-based refresh (see below).

**UDN GR binding (independent once propagated):** A propagated `MAC_Binding` entry on a UDN GR is an independent SB-DB row. Its lifetime does **not** depend on the CDN GR's entry continuing to exist, it is kept alive by its own traffic through the same OVN mechanisms described above:

- **Active bidirectional UDN traffic:** `mac_cache_use` on the UDN GR refreshes the timestamp from return traffic. The binding never expires while bidirectional traffic flows.
- **Active unidirectional outbound UDN traffic (25.03+):** The UDN GR's stale probe fires before expiry. The probe reply is steered to `default_patch` by the priority-52 flow, which refreshes (or re-creates) the CDN GR's binding; this triggers an ADD or UPDATE event that the `MAC Binding Controller` propagates back to the UDN GR. The binding stays alive as long as the UDN GR is forwarding traffic.
- **Idle, or unidirectional without stale probes (24.09):** The binding ages out at 300s. On next traffic, the UDN GR triggers resolution, repeating [step 1](#mechanism).

In all active-traffic cases, the UDN GR's binding can outlive the CDN GR's binding. When propagation timestamp refreshes are active (CDN GR binding is alive), the UDN GR's binding stays "fresh" from ovn-controller's perspective, so `mac_cache_use` and stale probes on the UDN GR rarely fire, propagation is the primary refresh path.

##### Probing Amplification

The `MAC Binding Controller` propagates every CDN GR binding to *all* UDN GRs, including those that have no traffic to that destination. These unused entries are kept fresh by propagation while the CDN GR's binding is alive. However, OVN's stale probe mechanism must correctly identify unused entries to avoid amplification: with N UDNs × M propagated bindings per UDN, probing all entries would produce O(N × M) unicast ARP/NDP packets on the wire every probing cycle.

OVN requires a fix that skips probing inactive entries. Without this fix, at 500 UDNs × 500 neighbors, up to ~250K probes fire per probing cycle per node per protocol. [This OVN fix](https://mail.openvswitch.org/pipermail/ovs-dev/2026-September/435673.html) is a **prerequisite** for the mid-term solution at scale.

##### Lifecycle Hooks

| Trigger | Action |
|---------|--------|
| GR port appears (`Port_Binding` add) | `MAC Binding Controller` adds the port to its source/follower map (a map of which GR ports mirror from which source) as a follower of the group's source, then reads all of that source's current `MAC_Binding` rows from the libovsdb SB cache and writes corresponding entries for the new follower GR to SB-DB. This covers new UDN creation, new Uplink selection. |
| GR port disappears (`Port_Binding` delete) | `MAC Binding Controller` removes the port from its source/follower map. northd explicitly deletes stale `MAC_Binding` rows for the removed port/datapath. This covers UDN deletion, Uplink CRD removal, node deselection (Uplink CRD's `nodeConfig.nodeSelector`). |
| Node process restart | libovsdb reconnects with full dump → the SB cache is repopulated. The `MAC Binding Controller` starts with an empty source/follower map, discovers every network active on the node, rebuilds that map, and for each follower reads its source's current `MAC_Binding` rows from the libovsdb SB cache and writes corresponding entries for the follower to SB-DB. No in-memory MAC binding data is retained; all IP→MAC reads use the standard libovsdb SB cache. |
| Source UDN removed from uplink group | Openflow manager designates a new source. The `MAC Binding Controller` detaches followers from the old source, re-assigns them to the new source in its map, and for each follower reads the new source's current `MAC_Binding` rows from the libovsdb SB cache and writes corresponding entries. Bindings already present on the new source (written while it was a follower) are propagated to remaining followers. |
| Uplink backing bridge change | See below |

**Edge case — backing bridge change:** `Uplink.spec.nodeConfigs` is mutable ([OKEP-6019](okep-6019-vrf-lite-shared-gateway-external-bridges.md)), so an administrator can change `hostInterfaceName` while CUDNs still reference the Uplink. OKEP-6019 documents this as a disruptive operation that "can temporarily degrade CUDNs while node state is rediscovered." The new interface may resolve to a different OVS bridge. If the previously learned IP-to-MAC mappings are not valid on the new physical attachment, existing source `MAC_Binding` rows are stale. When the GR is reconciled in place, its OVN logical topology and its `Datapath_Binding` remains unchanged, so those rows are not removed merely because the backing bridge changed. They remain subject to the GR's configured `mac_binding_age_threshold` and may be corrected earlier by OVN's stale-binding probing or a fresh ARP/NDP resolution. The `MAC Binding Controller` **amplifies** this pre-existing Uplink limitation by copying source bindings to all followers. It does not flush follower entries on a bridge change, and since the `MAC Binding Controller` ignores source DELETE events ([Event Handling](#event-handling)), follower rows can outlive the source's expired entries until their own timestamps expire. Connectivity to affected destinations may be disrupted during this convergence window.

#### Complete Flow Priority Table (br-ex Table 0)

The following shows the complete flow priority structure when `enable-scalable-arp-ndp` is enabled. Only the relevant priority range (9-52) is shown; flows at priorities 99-700 are unchanged and omitted.

**Note:** This table is titled for `br-ex`, but the same priority structure is installed on every bridge, including `Uplink`-backed bridges. `default_patch` is bridge-specific: on `br-ex` it is the CDN GR's patch port; on an uplink bridge it is that bridge's designated source UDN GR's patch port.
`<ext-ingress>` is expanded per-port at flow generation time; one flow set per discovered external-ingress port.

| Pri | Match | Action | Status |
|-----|-------|--------|--------|
| **52** | `in_port=<ext-ingress>, [matchVLAN,] arp` | `output:<default_patch>,NORMAL` | **NEW** (`enable-scalable-arp-ndp`) -- ALL ARP from external-ingress to source GR + kernel (+ FDB learning) |
| **52** | `in_port=<ext-ingress>, [matchVLAN,] icmp6, icmpv6_type=136` | `output:<default_patch>,NORMAL` | **NEW** (`enable-scalable-arp-ndp`) -- ALL NDP NA from external-ingress to source GR + kernel (above conntrack) |
| **52** | `in_port=<ext-ingress>, [matchVLAN,] icmp6, icmpv6_type=134` | `output:<default_patch>,NORMAL` | **NEW** (`enable-scalable-arp-ndp`) -- ALL NDP RA from external-ingress to source GR + kernel (above conntrack) |
| **52** | `in_port=<ext-ingress>, [matchVLAN,] icmp6, icmpv6_type=135` | `output:<default_patch>,NORMAL` | **NEW** (`enable-scalable-arp-ndp`) -- ALL NDP NS from external-ingress to source GR + kernel (above conntrack) |
| 50 | `ip/ipv6, dl_dst=<bridgeMAC>` | `ct(zone=...,nat,table=1)` | Existing -- IP/IPv6 return traffic to conntrack |
| 10 | `in_port=<patch>, dl_src=<bridgeMAC>` | `output:NORMAL` | Existing -- OVN non-IP egress (`no-flood` limits flood) |
| 9 | `in_port=<patch>` | `drop` | Existing -- Drop bad MAC from OVN |
| 0 | *(catch-all)* | `NORMAL` | Existing -- Default L2 forwarding (`no-flood` limits flood) |

When `enable-scalable-arp-ndp` is **disabled**, the current codebase flows are rendered unchanged (priority-10/11/12 fan-out flows, table-1 priority-14 RA/NA flows). When **enabled**, those flows are not rendered, they are replaced by the priority-52 per-external-ingress-port steering.

### Testing Details

* E2E tests for multi-UDN external connectivity (IPv4 and IPv6) and MAC\_Binding propagation across UDN gateway routers for both protocols.
* North-south scale validation (e.g. 70 UDNs) checking pod-to-external-destination connectivity.
* Regression coverage: existing e2e suites (EgressIP, Services, NetworkPolicy on UDN) run with `enable-scalable-arp-ndp` enabled.

### Documentation Details

* This OKEP serves as the primary design documentation.
* When this OKEP PR is opened, the `mkdocs.yml` nav section must be updated to include the path to this OKEP under "Enhancement Proposals".

## Risks, Known Limitations and Mitigations

* **Bootstrap latency:** First resolution for an unknown IP requires a wire round-trip plus MAC_Binding propagation delay before a UDN GR's MAC_Binding is available. OVN packet buffering (up to 10 seconds) covers this window, and reinjection happens without an application-triggered retry.

* **FDB learning dependency:** The trailing `NORMAL` in the priority-52 steering flows serves two roles: (1) FDB learning (OVS records `source_MAC → in_port`), and (2) LOCAL delivery (via the static FDB entry `bridgeMAC → LOCAL` for unicast, or broadcast flooding for broadcast/multicast). When ARP arrives from `ofPortPhys`, FDB learning records `source_MAC → ofPortPhys`. If `NORMAL` is accidentally removed, both FDB learning breaks and LOCAL stops receiving ARP/NDP (breaking the kernel's neighbor table).

* **MAC_Binding scale:** The `N x M` entry count (see [Scale](#mac_binding-propagation)) must be validated in scale testing to confirm SB-DB can handle the load. If timestamp-driven write amplification becomes a concern, the controller can switch to periodic reconciliation.

* **Thundering herd on northd batch-deletes:** If northd batch-deletes expired entries for all 500 UDN GRs across both protocols (e.g., all timestamps aligned), each UDN GR sends ARP/NS on next traffic. The neighbor receives up to 500 requests per protocol. Controller sees one ADD on the CDN GR per protocol → re-creates 500 entries in one batch transaction each. Mitigated by northd's `mac_binding_removal_limit` option which caps deletions per sweep.

* **CDN GR binding refresh for essentially the full `M` population:** Because hosts on the external subnet periodically re-resolve their neighbors/gateway as ordinary IP-stack behavior, independent of any UDN traffic, ARP/NDP-request-driven keep-alive is the dominant (often the *only*, since UDN-bound IP traffic never reaches the CDN GR's own pipeline) mechanism keeping the **entire** `M`-sized neighbor set (default gateway + every other cluster node) continuously alive on the CDN GR and continuously propagated to all N follower GRs, regardless of whether a specific UDN is actually forwarding traffic to a given neighbor. Concrete implications to validate in scale testing:
  - **Persistent, not transient, footprint:** the `N x M` MAC_Binding count should be expected to sit near its full ceiling essentially continuously.
  - **Continuous write load, not periodic bursts:** since `mac_cache_use`'s sweep is periodic (~56s), not per-packet, most of `M` can be marked active in the same sweep — so the `MAC Binding Controller` can face up to `N x M` propagation writes roughly every 56s.
**Design implication:** the [periodic-reconciliation mitigation](#event-handling) (a cooldown that skips a rewrite if a follower's row was refreshed recently enough, e.g. every ~150s instead of every ~56s) should be treated as a likely-needed default, not an optional fallback for extreme scale.

* **Process failure / SB-DB unavailable:** OVS retains its last-installed flow set on br-ex (including ovn-controller-programmed MAC_Binding flows), so the datapath continues forwarding autonomously during downtime. On restart or reconnect, libovsdb re-syncs state: delivers the full current state as ADD events, and the `MAC Binding Controller` re-applies UDN GR entries from current SB-DB state. Entries that aged out during downtime are re-created on next UDN traffic via the bootstrap path.

* **Event loss under extreme churn:** Server-side conditional filtering limits the monitored set to the local node's CDN GR entries only. If events are still lost, the next periodic reconciliation or default-GR ADD event corrects any drift.

* **No DPU mode support:** DPU mode uses a different architecture where the representor port replaces LOCAL. This design does not apply to DPU mode and the feature gate guard excludes it.

## OVN-Kubernetes Version Skew

TBD -- not yet assigned to a release milestone.

## Backwards Compatibility

The feature is behind a single feature gate, `enable-scalable-arp-ndp` (default `false`). When disabled, no steering flows are installed, the `MAC Binding Controller` is not started, and behavior is identical to the current codebase for both IPv4 and IPv6.

| State | Active Flows | Behavior |
|-------|-------------|----------|
| `enable-scalable-arp-ndp=false` | Current codebase flows only | Unchanged from today: ARP/NDP fan-out to all UDN patches |
| `enable-scalable-arp-ndp=true` | OKEP pri-52 ARP/NDP per external-ingress port + `MAC Binding Controller` (both protocols) | New ARP and NDP flows in effect; existing ARP/NDP fan-out flows not rendered |

When the gate is enabled:

* Existing ARP/NDP fan-out flows (priority-10 `dl_dst=bridgeMAC`, priority-11 GARP/NA, priority-12 node-IP, table-1 priority-14 RA/NA) are **not rendered**. This eliminates hidden fallback paths.
* The existing priority-10 non-IP egress rule (`in_port=<patch>, dl_src=bridgeMAC → NORMAL`) and priority-9 drop rule are unchanged, they handle UDN outbound ARP/NDP, with `no-flood` preventing CUDN delivery.
* No Kubernetes API changes (no CRD, no webhook, no schema changes).
* No OVN schema changes (uses existing `MAC_Binding` table).

## Alternatives

### Pure-OpenFlow NDP Responder on br-ex

IPv6 Neighbor Discovery uses ICMPv6 Neighbor Solicitation (NS, type 135) and Neighbor Advertisement (NA, type 136). Unlike ARP, NDP packets are more complex: they contain ICMPv6 headers with options (Source/Target Link-Layer Address) and require correct checksum computation.

OVS supports the following NDP-relevant OpenFlow fields:
- `nd_target`: The target IPv6 address in NS/NA. Writable via `set_field` on both NS and NA matches.
- `nd_sll`: Source Link-Layer Address option (in NS). Writable via `set_field`, but only when the match includes `icmpv6_type=135`.
- `nd_tll`: Target Link-Layer Address option (in NA). Writable via `set_field`, but only when the match includes `icmpv6_type=136`.
- `icmpv6_type`: Writable via `set_field`.

Each NDP field individually supports `set_field` when its prerequisite match is satisfied. However, OVS enforces prerequisites at **flow installation time** against the **match criteria**, not at packet execution time. Even chaining `set_field:136->icmpv6_type` before `set_field:...->nd_tll` in the actions is rejected if the match specifies `icmpv6_type=135`, OVS does not re-evaluate prerequisites after action-side field modifications.

**The fundamental blocker** is that OVS's kernel datapath cannot write the Target Link-Layer Address (`nd_tll`) into a Neighbor Solicitation packet. An NS carries an option type 1 (Source LLA), and `nd_tll` targets option type 2 (Target LLA); OVS's `packet_set_nd()` function ([`lib/packets.c`](https://github.com/openvswitch/ovs/blob/main/lib/packets.c)) scans the packet's ND options by type byte, finds type 1 instead of 2, and **silently does nothing**. The natural workarounds each fail:

- **Prerequisite two-table trick:** `nd_tll` requires `icmpv6_type=136` in the flow match, but we match `icmpv6_type=135` (NS). A two-table workaround (match 135 in table A, `set_field:136->icmpv6_type`, `goto_table` to table B where the match says 136) bypasses the prerequisite check ([`ovs-fields(7)`](https://www.openvswitch.org/support/dist-docs/ovs-fields.7.txt)), but does not change the option type byte in the actual packet, so `packet_set_nd()` still silently skips the write.

- **`nd_options_type`:** The only field that could rewrite the option type byte (1 → 2) uses `OVS_KEY_ATTR_ND_EXTENSIONS`, which the Linux kernel OVS module [explicitly rejects](https://lkml.iu.edu/hypermail/linux/kernel/2203.1/02245.html). It is userspace-datapath only (while OVN-K uses kernel datapath).

OVN's own `nd_na` action uses the controller slow path: NS is punted to ovn-controller via `CONTROLLER`, which constructs the NA from scratch in userspace (`pinctrl.c`) and injects it via packet-out.

**Rejected because:** No path exists to construct an NA from an NS purely in the OVS kernel datapath.

### ARP Proxy on br-ex (for IPv4)

For any neighbor the CDN GR has already resolved, answer UDN GR ARP requests locally on br-ex with a dynamic per-neighbor OpenFlow flow instead of propagating a `MAC_Binding` to every UDN GR. When an ARP reply is steered to `default_patch`, the CDN GR learns the neighbor and creates a `MAC_Binding` entry in SB-DB. A `MAC Binding Controller` observes these entries and programs a corresponding ARP responder flow on br-ex (writing to the openflowManager flow cache alongside existing producers like services and EgressIP):

```text
cookie=<ARPProxyCookie>, priority=40, table=0, arp, arp_op=1, arp_tpa=<IP>,
  actions=move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],
          set_field:<MAC>->eth_src,
          set_field:2->arp_op,
          move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],
          move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],
          set_field:<MAC>->arp_sha,
          set_field:<IP>->arp_spa,
          IN_PORT
```

This constructs a valid ARP reply in-place and sends it back to the requesting UDN GR via `IN_PORT`, which updates its own `MAC_Binding` in OVN SB. Only ADD/UPDATE/DELETE flow-cache maintenance is needed — no SB-DB writes for IPv4, since the requesting GR creates its own binding from the synthesized reply exactly as it would from a real one.

**Advantages of this approach:**
- **O(M) flows, independent of UDN count:** One flow per known neighbor on br-ex, versus `N x M` SB-DB entries for propagation. Scales with neighbor count, not UDN count.
- **Zero SB-DB write cost for IPv4:** Purely a data-plane mechanism; no MAC_Binding entries are written for UDN GRs, so there is no timestamp-refresh write amplification for this protocol.

**Rejected in favor of unified MAC_Binding propagation because:**
- **Two mechanisms instead of one:** IPv4 (OpenFlow flow generation, openflowManager cache interaction, `IN_PORT` semantics, flow-string validation) and IPv6 (SB-DB propagation) would remain two separate code paths with different failure modes, instead of one shared watcher and lifecycle.
- **Bootstrap requires an application retry:** The requesting UDN GR never receives the first ARP reply (only the CDN GR does, via `default_patch`). Once the proxy flow is programmed, the *next* application packet (e.g., a TCP SYN retransmit) must trigger a new ARP request for the proxy to answer — OVN does not retry ARP autonomously. With propagation, the `MAC Binding Controller` writes the MAC_Binding directly for the requesting UDN GR, and ovn-controller reinjects the already-buffered packets without waiting for an application-triggered retry.
- **Scale trade-off is acceptable:** Propagating IPv4 via SB-DB adds ~250K entries per node (matching the IPv6 footprint), doubling the total to ~500K. This is a quantitative increase on an already-necessary mechanism (IPv6 requires SB-DB propagation regardless, since no OpenFlow-only NDP responder is possible — see above), not a new category of risk. Timestamp refresh writes are bursty (~56s cooldown cycle) and batchable into single OVSDB transactions.

### NB-DB StaticMACBinding Propagation (for IPv6)

Watch the CDN GR's `MAC_Binding` in SB-DB. Write `StaticMACBinding` entries to NB-DB for each UDN GR. StaticMACBindings are permanent (no TTL, priority 150 in OpenFlow) and prevent UDN GRs from ever sending NDP NS. On CDN GR MAC_Binding deletion (aging), trigger a kernel NDP probe (`NUD_PROBE` via netlink) from breth0 to verify neighbor liveness; delete StaticMACBinding only if probe fails.

**Rejected in favor of direct SB MAC_Binding writes because:**

- **No traffic-based lifecycle:** StaticMACBinding entries are permanent. They accumulate for neighbors that are no longer being reached by any UDN. Cleanup requires either manual probing (kernel `NUD_PROBE` on DELETE events, ~100 lines of netlink infrastructure) or accepting permanent accumulation bounded by subnet size. Dynamic MAC_Binding entries self-clean via northd aging when idle — correct behavior with zero controller logic.
- **Probe infrastructure complexity:** To preserve liveness guarantees, StaticMACBinding requires a probe-on-DELETE mechanism (kernel NDP NS via netlink, NUD state monitoring, retry handling). Dynamic MAC_Binding delegates liveness to OVN's own aging and the inherent re-resolution path (buffered NS → re-creation), eliminating the probe infrastructure entirely.
- **Reconciliation complexity:** StaticMACBinding entries have no `ExternalIDs` field, making ownership tracking difficult. Startup reconciliation requires distinguishing propagated entries from dummy masquerade entries by IP range. Dynamic MAC_Binding entries are self-reconciling, the controller just re-creates from the CDN GR's current state.
- **northd cost:** `build_static_mac_binding_table()` is not incremental. Every NB-DB StaticMACBinding transaction triggers a full recompute of this function (iterates ALL entries). Direct SB MAC_Binding writes bypass northd entirely — ovn-controller processes them incrementally via `lflow_handle_changed_mac_bindings`.
- **Overrides dynamic bindings:** `override_dynamic_mac=true` at priority 150 prevents any mechanism from correcting a stale entry except the controller itself. If the controller misses a MAC change (crash during failover), the stale entry persists indefinitely causing a permanent black-hole that UDN GRs cannot self-heal from. Dynamic MAC_Binding at priority 100 expires naturally and is re-resolved with the correct MAC.

### Kernel Neighbor Table as ARP Proxy Source of Truth

Watch the Linux kernel's neighbor table on breth0 via netlink. Program ARP proxy flows for neighbors in usable NUD states (`NUD_REACHABLE`, `NUD_STALE`, `NUD_DELAY`, `NUD_PROBE`, `NUD_PERMANENT` with a non-empty hardware address). Require `arp_accept=2` sysctl so the kernel learns from ARP replies steered to LOCAL that it didn't originate (UDN GRs sent the request, not the kernel).

**Advantages of this approach:**
- **Lower latency:** The netlink path has fewer hops (kernel event → userspace → flow sync) compared to the SB-DB path (GR learns → SB-DB write → libovsdb notification → flow sync). Both are expected to complete well before the next data-plane packet triggers a retry, but the kernel path has fewer intermediate steps.
- **No new process-level dependency:** Does not require adding an SB-DB client to the node process. Uses the well-established `vishvananda/netlink` library already vendored.

**Rejected in favor of SB-DB MAC_Binding because:**
- **Security-relevant sysctl:** `arp_accept=2` allows same-subnet IPs to inject neighbor entries into the kernel without solicitation.
- **Stale MAC served without verification:** Kernel `NUD_STALE` entries are never probed. If a neighbor silently changes its MAC, the proxy serves the stale MAC indefinitely until kernel GC fires. OVN's MAC_Binding aging (300s) forces periodic re-resolution, ensuring correctness. OVN 25.03+ stale probes actively verify active bindings every ~56s.

## References

* [PR #6660](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6660): Short-term ARP/NDP fan-out fix (`no-flood` + priority-12/11 steering). This OKEP's foundation — `no-flood` is inherited; flow-level changes become dead code under OKEP gates.
* [PR #5334](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/5334): FDB learning fix — established the `output:patch,NORMAL` pattern for FDB learning on br-ex, precedent for the trailing `NORMAL` in this OKEP's priority-52 flows.
* OVN commit [`58ce60d`](https://github.com/ovn-org/ovn/commit/58ce60d2f1d932b842512763c2b8fc0943e1f8e3): Background ARP/NDP stale probes (OVN 25.03+)
* OVN commit [`33bb66c`](https://github.com/ovn-org/ovn/commit/33bb66c6c8e6e119bf9006dbe868457eecf82c9e): `mac_cache_use` flow for MAC_Binding timestamp refresh
* OVN patch [`neighbor-of: Lift the reply limitation for ARP mac_cache_use flows`](https://patchwork.ozlabs.org/project/ovn/patch/20260910085224.2004760-1-amusil@redhat.com/): removes the `arp_op==2` restriction on `OFTABLE_MAC_CACHE_USE`, so any ARP (request or reply) from a tracked `(MAC, IP)` refreshes its `MAC_Binding` timestamp, not just a reply.
