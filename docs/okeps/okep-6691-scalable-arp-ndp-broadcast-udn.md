# OKEP-6691: Scalable ARP and Broadcast Handling for UDN

* Issue: [#6691](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6691)

## Problem Statement

ARP and broadcast traffic on br-ex is flooded to all UDN patch ports, costing ~100 OVS resubmits per patch port traversal. This causes packet drops (OVS's hard-coded 4096 resubmit limit is reached at roughly 35-50 UDNs) and wastes CPU proportional to UDN count, breaking external connectivity and preventing UDN scalability.

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

```
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

### The Shared MAC

All GR external ports (`rtoe-GR_*`) use the **same MAC address**, the node's physical NIC MAC.

Giving each GR a unique MAC is a significantly more complex change that is being evaluated as part of the long-term effort.

### How ARP and NDP Are Currently Forwarded

#### Outbound ARP (GR resolves upstream gateway)

When a UDN GR needs to reach an external IP, it must first resolve the upstream gateway's MAC via ARP. The GR sends a broadcast ARP request using its external port identity: the shared MAC and node IP.

1. ARP request exits the UDN's ext\_LS localnet port onto br-ex.
2. The priority-10 egress flow matches: `in_port=<patch>, dl_src=bridgeMAC → output:NORMAL`).
3. `NORMAL` action performs standard L2 broadcast flooding, the ARP outputs on all ports, including `ofPortPhys` as intended, except UDN patch ports which are configured with NO_FLOOD property.

#### Inbound Unicast ARP Reply (Gateway Responds)

The upstream gateway sends a unicast ARP reply addressed to the shared MAC.

1. Reply arrives on br-ex via `ofPortPhys`.
2. It matches the priority-10 unicast flood rule: `dl_dst=<bridgeMAC> → output:patch1,patch2,...,patchN,NORMAL`
3. This rule explicitly outputs to **ALL** N patch ports sequentially.
4. Each patch port delivers the ARP into its ext\_LS, which forwards to its GR. OVS processes all N pipelines sequentially.
5. At high UDN counts: total resubmits exceed 4,096 → OVS drops the packet.

This unicast flood rule exists because all GRs share the same MAC, OVS MAC learning cannot determine which single patch port should receive the frame.

#### Inbound Broadcast ARP (External Host Asks "Who Has NodeIP?")

1. Broadcast ARP request arrives on br-ex via `ofPortPhys`.
2. It matches the priority-11 rule: `in_port=<ofPortPhys>, dl_dst=ff:ff:ff:ff:ff:ff → output:patch1,patch2,...,patchN,NORMAL`
3. As before, each patch port delivers the ARP into its ext\_LS, which forwards to its GR. OVS processes all N pipelines sequentially and if the total resubmit limit is exceeded, OVS drops the packet.
4. Additionally, as each UDN GR sees the request and recognizes the node IP on its external port, it generates an ARP reply producing N duplicate replies on the physical network.

#### NDP (IPv6 Neighbor Discovery)

NDP follows the same three-way pattern as ARP (outbound resolution, inbound reply, inbound external query) with these differences:

- **Multicast instead of broadcast:** NDP NS uses solicited-node multicast (`dl_dst=33:33:ff:xx:xx:xx`) rather than L2 broadcast, but `NORMAL` floods it to all non-UDN ports identically.
- **Conntrack interception:** Inbound unicast NDP NAs (`icmpv6_type=136`) are IPv6 and match the priority-50 conntrack flow (`ipv6, dl_dst=bridgeMAC → ct(table=1)`) *before* the priority-10 unicast flood rule. After conntrack, a priority-14 rule in table 1 sends the NA to all UDN patch ports and then `FLOOD`s. This is **worse** than the ARP path because it adds conntrack overhead on top of the per-pipeline cost.
- **Inbound solicited-node multicast NS:** Inbound solicited-node multicast NS from external hosts falls to the priority-0 `NORMAL` catch-all that outputs to all ports except the UDN patch ports.

#### Current br-ex Flow Table (Priority 9-50, Table 0)

| Table | Pri | Match | Action | Purpose |
|-------|-----|-------|--------|---------|
| 0 | 50 | `ip/ipv6, dl_dst=<bridgeMAC>` | `ct(zone=...,nat,table=1)` | IP/IPv6 return traffic → conntrack (NDP NAs hit this) |
| 0 | 12 | `arp/NS, target=<nodeIP>` | `output:<default_patch>,NORMAL` | ARP/NS requests to node IP may output to any port except UDN patch ports |
| 0 | 11 | `arp/NA, dl_dst=ff:ff:ff:ff:ff:ff / 33:33:00:00:00:01` | `output:patch1,...,patchN,NORMAL` | ARP broadcasts and unsolicited NA outputs to all patches |
| 0 | 10 | `dl_dst=<bridgeMAC>` | `output:patch1,...,patchN,NORMAL` | ARP replies outputs to all patches |
| 0 | 10 | `in_port=<patch>, dl_src=<bridgeMAC>` | `output:NORMAL` | OVN non-IP egress may output to any port except UDN patch ports |
| 0 | 0 | *(catch-all)* | `NORMAL` | Standard L2 forwarding may output to any port except UDN patch ports |
| 1 | 14 | `icmp6, icmpv6_type=136` | `output:<UDN patch ports...>,FLOOD` | NDP solicited NA output to all UDN patch ports |

The priority 11, 10, and 14 (table 1) flows are at the core of the problem as they fan out packets to all CUDN patch ports causing packet drops when the resubmit limit is hit and causing unnecesary CPU load.

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

Three mechanisms work together to eliminate ARP/NDP fan-out:

1. **Traffic Steering** (8 static flows at pri-52/45): Intercepts uplink ARP and NDP above the two existing paths that bypass `no-flood`: the priority-10 explicit `output:patch1,...,patchN` (for ARP) and the priority-50 conntrack → table-1 path where there are explicit `output:<all-CUDN-patches>` before `FLOOD` (for NDP NA and RA). Also steers default-patch and LOCAL ARP to the wire, this keeps them above the priority-40 ARP proxy (preventing stale probes from being short-circuited).
2. **ARP Proxy** (dynamic flows at pri-40): Answers UDN ARP requests locally for known neighbors, turning O(N) fan-out into a single-flow lookup.
3. **IPv6 NDP Resolution via MAC_Binding Propagation**: Propagates the default GR's resolved bindings to UDN GRs via SB-DB.

The IPv4 traffic scenarios are shown separately below; IPv6 NDP is handled via [MAC_Binding propagation](#ipv6-ndp-resolution-via-mac_binding-propagation).

#### Scenario 1: Outbound ARP from UDN GR

```
  UDN patch port
       │
  broadcast ARP request (arp_op=1, dl_src=bridgeMAC)
       │
       ▼
  ┌─ pri 40: ARP Proxy (dynamic) ────────────────┐
  │  arp_op=1, arp_tpa=<known IP>                │
  │                                              │
  │  Known neighbor?                             │
  │    YES → construct ARP reply → IN_PORT ──────┼──▶ back to requesting GR
  │          O(1). No wire traffic.              │    (GR updates MAC_Binding)
  │                                              │
  │    NO  → no match, fall through              │
  └──────────────────┬───────────────────────────┘
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
  │    default_patch (harmless — default GR      │
  │           ignores ARP for IPs it doesn't own)│
  └──────────────────────────────────────────────┘

  ✗ NOT flooded to CUDN patches (no-flood)
```

#### Scenario 2: Inbound Unicast ARP Reply

```
  Physical network (ofPortPhys)
       │
  unicast ARP reply (arp_op=2, dl_dst=bridgeMAC)
       │
       ▼
  ┌─ pri 45: Uplink ARP Steering ───────────────┐
  │  in_port=ofPortPhys, arp                    │
  │  → output:default_patch, NORMAL             │
  └──────────┬──────────────────────────┬───────┘
             │                          │
             ▼                          ▼
        default_patch                 NORMAL
        (default GR creates           (1) FDB learning: records
         MAC_Binding in SB-DB)             src_MAC→ofPortPhys.
              │                       (2) FDB lookup: bridgeMAC→LOCAL
        SB-DB event →                     delivers to kernel. 
        proxy flow updated (pri 40) →
        future UDN ARP answered
        locally

  ✗ NOT flooded via output:patch1,...,patchN (intercepted above pri 10)
  ✗ UDN patches never see inbound ARP replies
```

#### Scenario 3: Inbound Broadcast ARP ("who has nodeIP?")

```
  Physical network (ofPortPhys)
       │
  broadcast ARP request (dl_dst=ff:ff:ff:ff:ff:ff)
       │
       ▼
  ┌─ pri 45: Uplink ARP Steering ────────────────┐
  │  in_port=ofPortPhys, arp                    │
  │  → output:default_patch, NORMAL             │
  └──────────┬──────────────────────────┬───────┘
             │                          │
             ▼                          ▼
        default_patch                 NORMAL
        (default GR learns            (FDB learning + broadcast
         MAC bindings,                 flood to LOCAL only —
         responds for node IP)         kernel responds for
                                       node IP, EgressIPs, etc.)

  ✗ NOT flooded to UDN patches (pri 45 > pri 0 NORMAL; no-flood)
  ✗ No N duplicate ARP replies on the physical network
```

See [Bootstrap: First ARP Resolution](#bootstrap-first-arp-resolution) for the detailed first-resolution sequence.

#### IPv6 NDP

NDP traffic isolation uses the same unified steering approach as ARP: priority-52 steers all inbound NDP NAs from `ofPortPhys` above conntrack to `default_patch` + NORMAL (NORMAL delivers to LOCAL via FDB/flooding and performs MAC learning), and priority-45 steers all inbound NDP NS to the same destinations. UDN outbound NDP NS is handled by `no-flood`, it falls to the existing priority-10 `output:NORMAL` which with `no-flood` delivers to the wire without CUDN fan-out. Unlike ARP, IPv6 neighbor resolution cannot be answered directly on br-ex with OpenFlow (why is explained in [Pure-OpenFlow NDP Responder on br-ex](#pure-openflow-ndp-responder-on-br-ex) under Alternatives). Instead, a new component (`macBindingWatcher`) propagates the default GR's resolved IPv6 neighbors to UDN GRs via SB-DB; see [IPv6 NDP Resolution via MAC_Binding Propagation](#ipv6-ndp-resolution-via-mac_binding-propagation) for the mechanism.

### API Details

#### Feature Gates

Two new flags are added:

| Flag | Scope |
|------|-------|
| `enable-arp-proxy` | IPv4: priority-45 ARP steering (3 flows) + priority-40 ARP proxy (M dynamic flows) |
| `enable-ndp-proxy` | IPv6: priority-52 NDP NA + RA steering (2 flows) + priority-45 NDP NS steering (3 flows) + macBindingWatcher |

Both default to `false` and require `enable-network-segmentation` (validated at startup).
Changing either flag requires an ovnkube-node restart, which triggers a full flow sync.

**Why two separate flags rather than one:**
The IPv4 and IPv6 solutions use fundamentally different mechanisms with different risk profiles: IPv4 uses a pure-OpenFlow ARP proxy (M local flows, data-plane only), while IPv6 uses MAC\_Binding propagation that writes N x M entries to SB-DB. If the higher-risk IPv6 path causes issues at scale, operators can disable it without losing the IPv4 fix. The separation effort is minimal since all flows are cleanly separable by protocol (zero shared flows) and each gate controls the complete pipeline for its protocol (steering + resolution).

**Why not reuse `enable-network-segmentation`:** It is already `true` in production. A separate gate allows code to be merged incrementally and flipped only when each protocol's pipeline is proven working.

### Implementation Details

This feature applies to both LGW and SGW gateway modes. The br-ex flow table and patch port topology are the same in both modes.

#### Traffic Isolation and Steering

See [Complete Flow Priority Table](#complete-flow-priority-table-br-ex-table-0) for the full picture with match/action details.

**Priority-52 and priority-45 flows** intercept uplink ARP and NDP above the two existing paths that bypass `no-flood`:

1. The priority-10 unicast flood rule (`dl_dst=bridgeMAC → output:patch1,...,patchN,NORMAL`) uses explicit `output:<port>` actions that ignore `no-flood` entirely. Without the priority-45 ARP flows, unicast ARP replies would still fan out to all N patches.
2. The priority-50 conntrack flow (`ipv6, dl_dst=bridgeMAC → ct(table=1)`) sends unicast NDP NAs and RAs to table 1, where flows include explicit `output:<all-CUDN-patches>` that also bypasses `no-flood`. Without the priority-52 flows, NAs and RAs would still reach all N CUDN pipelines. Priority 52 also avoids processing NDP through a conntrack path that cannot create CT entries ([kernel bug #11797](https://bugzilla.kernel.org/show_bug.cgi?id=11797): IPv6 conntrack treats NDP packets as invalid; workaround at `bridgeflows.go:1163`).

**Priority-52 NDP NA flow:** Steers all inbound NDP NAs (type 136) from `ofPortPhys` to `default_patch` + NORMAL, above conntrack. The `dl_dst` match is omitted to catch both unicast NAs (`dl_dst=bridgeMAC`) and multicast unsolicited NAs (`dl_dst=33:33:00:00:00:01`). `NORMAL` performs FDB learning and delivers to LOCAL (via static FDB entry for unicast, or flooding for multicast).

**Priority-52 RA flow:** Steers all inbound Router Advertisements (type 134) from `ofPortPhys` to `default_patch` + NORMAL, above conntrack. Without this flow, unicast RAs (`dl_dst=bridgeMAC`) would fan out via the same conntrack → table-1 path as NAs. CUDN GRs do not need RAs, OVN logical routers are statically configured and have no RA-receiving capability. The only legitimate consumer is the kernel on `breth0` (reached via `NORMAL`). Multicast RAs (`dl_dst=33:33:00:00:00:01`) bypass conntrack and fall to pri-0 `NORMAL`, where `no-flood` handles them.

**Priority-45 ARP flows:** Steer all inbound ARP from `ofPortPhys` to `default_patch` + NORMAL, and route all ARP from default-patch/LOCAL to the wire. The inbound flow uses `output:default_patch, NORMAL`: `default_patch` must be explicit because NORMAL's FDB lookup for unicast `dl_dst=bridgeMAC` returns only LOCAL (via the static FDB entry), not `default_patch`. LOCAL delivery is handled by NORMAL itself, FDB unicast for replies, broadcast flooding for requests. All three source ports match ALL ARP (not just broadcast) because:

- External devices send unicast ARP requests for NUD probes. If only broadcast matched, these would fall to the priority-40 proxy, which would incorrectly answer for addresses it doesn't own.
- The kernel's unicast NUD probes from LOCAL must reach the physical network for real verification.
- OVN 25.03+ stale probes from the default GR (unicast ARP for MAC\_Binding liveness) must reach the wire, not be short-circuited by the proxy.

Matching all ARP also catches replies, making a separate unicast-reply steering flow unnecessary. Priority 45 is above the ARP proxy (priority 40), ensuring uplink, LOCAL, and default-patch traffic always reaches its real destination. The trailing `NORMAL` preserves FDB learning and delivers to LOCAL via the static FDB entry or broadcast flooding.

**Priority-45 NDP NS flows:** Steer all inbound NDP NS from `ofPortPhys` to `default_patch` + NORMAL, and route all NS from default-patch/LOCAL to the wire. NDP NS flows match `icmp6, icmpv6_type=135` without a `dl_dst` restriction to catch both solicited-node multicast and unicast NS.

#### ARP Proxy on br-ex

For any neighbor the default GR has already resolved, the ARP proxy answers locally on br-ex instead of forwarding the request to the physical network. This turns O(N) fan-out into a single-flow lookup.

##### Data Flow

When an ARP reply is steered to `default_patch` by the priority-45 flow, the default GR learns the neighbor and creates a `MAC_Binding` entry in SB-DB. A new component, the `macBindingWatcher`, monitors these entries in the ovnkube-node process via a libovsdb SB-DB client with server-side filtering (`logical_port == "rtoe-GR_<node>"`). It picks up the entry and programs a corresponding ARP responder flow on br-ex (writing to the openflowManager flow cache alongside existing producers like services and EgressIP). Because the default GR learns from any ARP packet that reaches its external port (not just replies it solicited) the proxy also preemptively learns external hosts' MACs from inbound broadcast ARP requests (Scenario 3), providing immediate proxy responses when a UDN GR later needs to reach that host.

##### IPv4 ARP Responder Flows (priority 40)

For each known IPv4 neighbor `(IP, MAC)` from the default GR's MAC_Binding table, the macBindingWatcher defines the following flow:

```
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

This constructs a valid ARP reply in-place and sends it back to the requesting port via `IN_PORT`.

**Why `IN_PORT`:** The request can only come from a UDN patch port (uplink, LOCAL, and default patch traffic is caught at priority 45). `IN_PORT` sends the reply back to the exact UDN GR that asked, which updates its `MAC_Binding` table in OVN SB.

##### Event Handling

- **ADD** (new IP resolved): Generate ARP proxy flow → write to openflowManager flow cache.
- **UPDATE** (MAC changed): Reprogram the ARP proxy flow with the new MAC.
- **UPDATE** (timestamp refreshed, same MAC): No action needed (proxy flow is stateless w.r.t. timestamps).
- **DELETE** (default GR binding aged out): Remove the ARP proxy flow from the flow cache.

##### Bootstrap: First ARP Resolution

The first time a UDN GR needs to resolve an unknown IP (e.g., the upstream gateway), no priority-40 proxy flow exists yet, so resolution takes the same wire path already shown in [Scenario 1](#scenario-1-outbound-arp-from-udn-gr) and [Scenario 2](#scenario-2-inbound-unicast-arp-reply): the request falls through the proxy to the existing priority-10 non-IP egress flow (`in_port=<patch>, dl_src=bridgeMAC → output:NORMAL`) and reaches the wire via `NORMAL`. With `no-flood`, `NORMAL` delivers to `ofPortPhys` (desired) plus `LOCAL` and `default_patch` (harmless). The gateway's reply is steered by priority-45 to `default_patch`, and the resulting `MAC_Binding` triggers the macBindingWatcher to program the proxy flow. The one point not shown in those diagrams: the requesting UDN GR does **NOT** receive this first reply directly, only the default GR does.

**OVN buffers the original IP packet** that triggered the ARP request (for up to 10 seconds, with limits of 1000 unique destinations and 4 packets per destination). When a subsequent packet for the same destination triggers a new ARP request, the proxy answers immediately through the new installed flow. The GR creates a dynamic `MAC_Binding` in SB, and ovn-controller reinjects the buffered packets. If the proxy flow is not yet installed when the retry arrives, the ARP simply takes the bootstrap path again.

**Bootstrap timing analysis:** The bootstrap path introduces a one-time delay for the first resolution of each unknown destination IP compared to the current flooding approach, where the requesting GR receives the ARP reply directly. The delay has two phases:

1. **Proxy flow programming:** wire RTT + ovn-controller `put_arp` (includes a randomized anti-collision delay) + SB-DB `MAC_Binding` write + libovsdb event delivery to macBindingWatcher + OpenFlow sync. All steps are event-driven and expected to complete well within OVN's 10-second packet buffer window, but exact latency depends on SB-DB load and ovn-controller main-loop timing.
2. **Application retry:** The requesting UDN GR does not receive the first ARP reply. For IPv4, OVN does not retry ARP autonomously — the next data packet from the application (e.g., TCP SYN retransmit) triggers a new ARP request that the now-installed proxy answers immediately. For IPv6, the macBindingWatcher writes `MAC_Binding` entries directly to SB-DB for UDN GRs, which ovn-controller matches against buffered packets, triggering reinjection without requiring an application retry.

OVN's logical router does not return ICMP unreachable or errors to the application during ARP resolution — it silently buffers packets. TCP applications see at most one additional SYN retransmit, not an error. The kernel's `tcp_syn_retries=6` provides substantial headroom before connection timeout. The primary risk factor is a sustained SB-DB outage during the bootstrap window, which is the same failure mode as any SB-DB outage and is not specific to this feature.

##### MAC_Binding Lifecycle and Proxy Flow Availability

The ARP proxy flow for a given IP exists on br-ex as long as the default GR has a corresponding `MAC_Binding` entry in SB-DB.

**Binding lifetime by destination type:**

- **Default gateway:** On OVN 25.03+ ([commit `58ce60d`](https://github.com/ovn-org/ovn/commit/58ce60d2f1d932b842512763c2b8fc0943e1f8e3)), ovn-controller sends background stale probes (~56s interval) that keep the binding alive indefinitely. On OVN 24.09 and earlier, the binding expires every 300s and is re-created via the [bootstrap path](#bootstrap-first-arp-resolution).
- **Same-subnet with default-network traffic** (e.g., other cluster nodes): [`mac_cache_use`](https://github.com/ovn-org/ovn/commit/33bb66c6c8e6e119bf9006dbe868457eecf82c9e) refreshes the binding from return traffic. Never expires while bidirectional traffic flows.
- **UDN-only same-subnet destinations:** No default-GR traffic refreshes the binding. Expires at 300s; re-resolved via bootstrap on next UDN traffic.

**Proxy flow updates:**

- **GARP:** Priority-45 steers to `default_patch` → default GR updates MAC\_Binding → proxy flow reprogrammed.
- **MAC change:** The default GR's stale probe detects MAC changes for active-traffic entries. For idle entries, the entry expires and next traffic re-resolves with the correct MAC.

A brief window without a proxy flow can occur when a binding expires and is re-created. This does not affect active UDN data traffic — the UDN GR retains its own MAC\_Binding and forwards data normally. On same-subnet destinations, `mac_cache_use` from return traffic independently keeps the UDN GR's binding alive.

#### IPv6 NDP Resolution via MAC_Binding Propagation

Unlike IPv4 where the ARP proxy constructs a reply in OpenFlow, a pure-OpenFlow NDP responder cannot be built on br-ex due to OVS kernel datapath limitations (see [Pure-OpenFlow NDP Responder on br-ex](#pure-openflow-ndp-responder-on-br-ex) in Alternatives). Instead, the macBindingWatcher writes `MAC_Binding` entries directly to SB-DB for each UDN GR's external port when the default GR resolves an IPv6 neighbor. These entries are dynamic (subject to `mac_binding_age_threshold=300`) and benefit from OVN's built-in lifecycle management.

The mechanism works as follows:

1. The default GR resolves an IPv6 neighbor via NDP: the neighbor's NA is steered to `default_patch` by the priority-52 flow. The default GR creates a dynamic `MAC_Binding` entry in SB-DB.
2. The macBindingWatcher observes the default GR's `MAC_Binding` entries in SB-DB.
3. When an IPv6 binding appears or its MAC changes, the watcher writes a `MAC_Binding` entry directly to SB-DB for each UDN GR's external port (`rtoe-GR_*`) for that `(IP, MAC)` pair with a fresh timestamp.
4. Bidirectional UDN traffic (e.g. TCP) keeps entries alive via `MAC_CACHE_USE` (return traffic refreshes timestamp). Entries never expire while bidirectional traffic flows.
5. Idle entries expire after 300s (without controller involvement).
6. When a UDN GR needs to reach an IPv6 neighbor whose MAC_Binding has not been propagated yet (cold start before the default GR has resolved, or after an idle entry has expired): the UDN GR sends an NDP NS which reaches the wire via the existing priority-10 egress flow. The cycle repeats from step 1. OVN buffers the original packet for up to 10 seconds, which is sufficient for the propagation round-trip.

**Scale:** This produces `N x M` MAC_Binding entries per node, where N is the number of UDNs and M is the number of IPv6 neighbors the default GR has resolved. M is driven by the GR's **connected route** for its external subnet. The GR's external port (`rtoe-GR_*`) is assigned the node's IP with a prefix length (e.g., `fd2e:.../64`), which creates an implicit connected route for the entire subnet. Since all cluster nodes sit on the same external subnet, the best-case M = default gateway + number of nodes. At 500 UDNs and 500 nodes, that is ~250K MAC_Binding entries per node. Every resolved binding is propagated to **all** UDN GRs regardless of which UDN triggered the resolution (the returning NA is steered to `default_patch` with no correlation to the originating UDN).

##### Event Handling

- **ADD** (new IP resolved): Write `MAC_Binding` for all UDN GRs with same `(IP, MAC)` and fresh timestamp in a single batched transaction. ovn-controller installs flows incrementally (no northd involvement).
- **UPDATE** (MAC changed): Update all UDN GR MAC_Bindings with the new MAC.
- **UPDATE** (timestamp refreshed, same MAC): Write a fresh timestamp to all UDN GR MAC_Bindings for that IP. This keeps UDN GR gateway bindings alive when `mac_cache_use` cannot refresh them directly. Batched; at most one write per UDN GR per refresh cycle.
- **DELETE** (default GR binding aged out): No action needed. UDN GR entries have independent timestamps and are managed by OVN's own lifecycle:
  - If UDN traffic is bidirectional → `MAC_CACHE_USE` keeps the UDN entry alive independently.
  - If UDN traffic is idle → the UDN entry ages out on its own. On next traffic, the default GR re-resolves → ADD event → re-propagated.

**Potential improvement:** If timestamp-driven write amplification becomes a concern at extreme scale, the controller could switch to periodic reconciliation: sweep every half binding expiration time, refresh UDN entries approaching expiry in one batch.

##### Lifecycle Hooks

| Trigger | Action |
|---------|--------|
| New UDN created | Query current default GR MAC_Bindings → batch-create entries for new UDN GR |
| UDN deleted | northd handles cleanup (deletes MAC_Bindings for removed datapaths via strong `datapath` reference) |
| Node process restart | libovsdb reconnects with full dump → watcher re-creates any missing UDN GR entries |

**UDN deletion:** Each `MAC_Binding` entry we create holds a strong UUID reference (`datapath` column) to the UDN GR's `Datapath_Binding`. When the UDN is deleted and northd removes the `Datapath_Binding`, OVSDB's referential integrity automatically garbage-collects all `MAC_Binding` rows that reference it. No explicit cleanup by the controller is needed.

#### Complete Flow Priority Table (br-ex Table 0)

The following shows how new flows (marked with **NEW**) fit within the existing priority structure. Only the relevant priority range (9-52) is shown; flows at priorities 99-700 are unchanged and omitted.

| Pri | Match | Action | Status |
|-----|-------|--------|--------|
| **52** | `in_port=ofPortPhys, [matchVLAN,] icmp6, icmpv6_type=136` | `output:<default_patch>,[strip_vlan,]NORMAL` | **NEW** (`enable-ndp-proxy`) -- ALL uplink NDP NA above conntrack |
| **52** | `in_port=ofPortPhys, [matchVLAN,] icmp6, icmpv6_type=134` | `output:<default_patch>,[strip_vlan,]NORMAL` | **NEW** (`enable-ndp-proxy`) -- ALL uplink RA above conntrack (GRs don't need RAs; only kernel via NORMAL) |
| 50 | `ip/ipv6, dl_dst=<bridgeMAC>` | `ct(zone=...,nat,table=1)` | Existing -- IP/IPv6 return traffic to conntrack |
| **45** | `in_port=ofPortPhys, [matchVLAN,] arp` | `output:<default_patch>,[strip_vlan,]NORMAL` | **NEW** (`enable-arp-proxy`) -- ALL uplink ARP to default GR + kernel + FDB learning |
| **45** | `in_port=ofPortPhys, [matchVLAN,] icmp6, icmpv6_type=135` | `output:<default_patch>,[strip_vlan,]NORMAL` | **NEW** (`enable-ndp-proxy`) -- ALL uplink NDP NS to default GR + kernel |
| **45** | `in_port=<default_patch>, arp` | `output:ofPortPhys` | **NEW** (`enable-arp-proxy`) -- ALL default GR ARP to wire |
| **45** | `in_port=<default_patch>, icmp6, icmpv6_type=135` | `output:ofPortPhys` | **NEW** (`enable-ndp-proxy`) -- Default GR NDP NS to wire |
| **45** | `in_port=LOCAL, arp` | `[modVLANID,]output:ofPortPhys` | **NEW** (`enable-arp-proxy`) -- ALL host ARP to wire |
| **45** | `in_port=LOCAL, icmp6, icmpv6_type=135` | `[modVLANID,]output:ofPortPhys` | **NEW** (`enable-ndp-proxy`) -- Host NDP NS to wire |
| **40** | `arp, arp_op=1, arp_tpa=<knownIP>` | ARP reply via `IN_PORT` | **NEW** (`enable-arp-proxy`) -- ARP proxy (dynamic, per neighbor) |
| 12 | `[matchVLAN,] arp, arp_op=1, arp_tpa=<nodeIP>` | `output:<default_patch>,[strip_vlan,]NORMAL` | Existing -- Node-IP ARP steering (unreachable when `enable-arp-proxy`) |
| 12 | `[matchVLAN,] icmp6, icmpv6_type=135, nd_target=<nodeIP>` | `output:<default_patch>,[strip_vlan,]NORMAL` | Existing -- Node-IP NDP NS steering (unreachable when `enable-ndp-proxy`) |
| 11 | `in_port=ofPortPhys, [matchVLAN,] dl_dst=ff:ff:ff:ff:ff:ff, arp` | `output:<all-patches>,[strip_vlan,]NORMAL` | Existing -- Broadcast ARP/GARP to all GRs despite `no-flood` (unreachable when `enable-arp-proxy`) |
| 11 | `in_port=ofPortPhys, [matchVLAN,] dl_dst=33:33:00:00:00:01, icmp6, icmpv6_type=136` | `output:<all-patches>,[strip_vlan,]NORMAL` | Existing -- Unsolicited NDP NA to all GRs despite `no-flood` (unreachable when `enable-ndp-proxy`) |
| 10 | `dl_dst=<bridgeMAC>` | `output:patch1,...,patchN,NORMAL` | Existing -- Unicast flood (harmless fallback: ARP intercepted above; NDP NAs never reach here) |
| 10 | `in_port=<patch>, dl_src=<bridgeMAC>` | `output:NORMAL` | Existing -- OVN non-IP egress (`no-flood` limits flood) |
| 9 | `in_port=<patch>` | `drop` | Existing -- Drop bad MAC from OVN |
| 0 | *(catch-all)* | `NORMAL` | Existing -- Default L2 forwarding (`no-flood` limits flood) |

### Testing Details

* E2E tests for multi-UDN external connectivity (IPv4 and IPv6) and IPv6 MAC\_Binding propagation across UDN gateway routers.
* North-south scale validation (e.g. 70 UDNs) checking pod-to-external-destination connectivity.
* Regression coverage: existing e2e suites (EgressIP, Services, NetworkPolicy on UDN) run with ARP proxy enabled by default.

### Documentation Details

* This OKEP serves as the primary design documentation.
* When this OKEP PR is opened, the `mkdocs.yml` nav section must be updated to include the path to this OKEP under "Enhancement Proposals".

## Risks, Known Limitations and Mitigations

* **Bootstrap latency:** First ARP/NDP resolution for an unknown IP requires a wire round-trip plus MAC_Binding propagation delay before the proxy flow or UDN GR MAC_Binding is available. OVN packet buffering (up to 10 seconds) covers this window. See [Bootstrap: First ARP Resolution](#bootstrap-first-arp-resolution) for the detailed sequence and timing analysis.

* **FDB learning dependency:** The trailing `NORMAL` in the priority-45 and priority-52 uplink flows serves two roles: (1) FDB learning (OVS records `source_MAC → ofPortPhys`), and (2) LOCAL delivery (via the static FDB entry `bridgeMAC → LOCAL` for unicast, or broadcast flooding for broadcast/multicast). If `NORMAL` is accidentally removed, both FDB learning breaks and LOCAL stops receiving ARP/NDP (breaking the kernel's neighbor table).

* **IPv6 MAC_Binding scale:** The `N x M` entry count (see [Scale](#ipv6-ndp-resolution-via-mac_binding-propagation)) must be validated in scale testing to confirm SB-DB can handle the load (e.g., 250K entries at 500 UDNs / 500 nodes). If timestamp-driven write amplification becomes a concern, the controller can switch to periodic reconciliation.

* **Thundering herd on northd batch-deletes:** If northd batch-deletes expired entries for all 500 UDN GRs (e.g., all timestamps aligned), each UDN GR sends NS on next traffic. The neighbor receives up to 500 NS. Controller sees one ADD on the default GR → re-creates 500 entries in one batch transaction. Mitigated by northd's `mac_binding_removal_limit` option which caps deletions per sweep.

* **Malformed flow risk:** The openflowManager applies all flows atomically; one malformed ARP proxy flow string rejects the entire update for ALL producers on br-ex until the bad entry is overwritten or ovnkube-node restarts. Mitigated by mandatory validation of every flow string before writing to the cache (reject entries with empty/zero IP or MAC).

* **Process failure / SB-DB unavailable:** OVS retains its last-installed flow set (including proxy flows) on br-ex, so the datapath continues forwarding autonomously during downtime. On restart or reconnect, libovsdb re-syncs state: delivers the full current state as ADD events, the macBindingWatcher reconciles missing UDN entries and reprograms proxy flows from current SB-DB state. Entries that aged out during downtime are re-created on next UDN traffic via the bootstrap path.

* **Event loss under extreme churn:** Server-side conditional filtering limits the monitored set to the local node's default GR entries only. If events are still lost, the periodic flow sync corrects any drift.

* **No DPU mode support:** DPU mode uses a different architecture where the representor port replaces LOCAL. This design does not apply to DPU mode and the feature gate guard excludes it.

## OVN-Kubernetes Version Skew

TBD -- not yet assigned to a release milestone.

## Backwards Compatibility

The feature is behind two independent feature gates: `enable-arp-proxy` (IPv4, default `false`) and `enable-ndp-proxy` (IPv6, default `false`). When both are disabled, no steering or proxy flows are installed and behavior is identical to the current codebase. When only one gate is enabled, only that protocol's flows are installed; the other protocol's behavior is unchanged. Every gate combination produces a clean, non-broken state:

| State | Active Flows | Behavior |
|-------|-------------|----------|
| + `enable-arp-proxy` | + OKEP pri-45 ARP + pri-40 proxy | New ARP flows in effect; current codebase ARP flows not hit; NDP via current codebase |
| + `enable-ndp-proxy` | + OKEP pri-52 NA/RA + pri-45 NS + macBindingWatcher | New NDP flows in effect; current codebase NDP flows not hit; ARP via current codebase|
| + both gates | + all OKEP flows | New ARP and NDP flows in effect; current codebase ARP & NDP flows not hit |

When either or both gates are enabled:

* The existing priority-10 unicast flood rule becomes a harmless fallback — uplink ARP is intercepted at priority 45 and NDP NAs at priority 52.
* The existing priority-10 non-IP egress rule handles UDN outbound ARP/NDP, with `no-flood` preventing CUDN delivery.
* All other flows handling ARP/NDP traffic are superseded by higher-priority OKEP flows. Cleanup is left as follow-up once long term fix will be in place.
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

### LOCAL as ARP Responder + NB-DB StaticMACBinding Propagation

Steer all ARP traffic (both requests and replies) to LOCAL only. The kernel answers ARP requests for addresses on breth0. A controller watches the kernel's neighbor table and writes `StaticMACBinding` entries into NB-DB for every UDN GR, so GRs never need to ARP themselves.

The concern is scale: the same `N x M` entry count applies as in [IPv6 MAC_Binding propagation](#ipv6-ndp-resolution-via-mac_binding-propagation) (~250K entries at 500 UDNs/500 nodes), except here the entries live in NB-DB rather than SB-DB. Changing a single MAC (e.g., gateway failover) is an O(N) operation, one NB-DB write per UDN GR.

**Rejected in favor of br-ex ARP responder flows because:**
- **Scale:** O(N x M) NB-DB entries vs O(M) br-ex flows — the responder approach is independent of UDN count.
- **Latency:** NB-DB writes propagate through northd → SB-DB → ovn-controller vs SB-DB MAC_Binding → flow update

### NB-DB StaticMACBinding Propagation (for IPv6)

Watch the default GR's `MAC_Binding` in SB-DB. Write `StaticMACBinding` entries to NB-DB for each UDN GR. StaticMACBindings are permanent (no TTL, priority 150 in OpenFlow) and prevent UDN GRs from ever sending NDP NS. On default GR MAC_Binding deletion (aging), trigger a kernel NDP probe (`NUD_PROBE` via netlink) from breth0 to verify neighbor liveness; delete StaticMACBinding only if probe fails.

**Rejected in favor of direct SB MAC_Binding writes because:**

- **No traffic-based lifecycle:** StaticMACBinding entries are permanent. They accumulate for neighbors that are no longer being reached by any UDN. Cleanup requires either manual probing (kernel `NUD_PROBE` on DELETE events, ~100 lines of netlink infrastructure) or accepting permanent accumulation bounded by subnet size. Dynamic MAC_Binding entries self-clean via northd aging when idle — correct behavior with zero controller logic.
- **Probe infrastructure complexity:** To preserve liveness guarantees, StaticMACBinding requires a probe-on-DELETE mechanism (kernel NDP NS via netlink, NUD state monitoring, retry handling). Dynamic MAC_Binding delegates liveness to OVN's own aging and the inherent re-resolution path (buffered NS → re-creation), eliminating the probe infrastructure entirely.
- **Reconciliation complexity:** StaticMACBinding entries have no `ExternalIDs` field, making ownership tracking difficult. Startup reconciliation requires distinguishing propagated entries from dummy masquerade entries by IP range. Dynamic MAC_Binding entries are self-reconciling, the controller just re-creates from the default GR's current state.
- **northd cost:** `build_static_mac_binding_table()` is not incremental. Every NB-DB StaticMACBinding transaction triggers a full recompute of this function (iterates ALL entries). Direct SB MAC_Binding writes bypass northd entirely — ovn-controller processes them incrementally via `lflow_handle_changed_mac_bindings`.
- **Overrides dynamic bindings:** `override_dynamic_mac=true` at priority 150 prevents any mechanism from correcting a stale entry except the controller itself. If the controller misses a MAC change (crash during failover), the stale entry persists indefinitely causing a permanent black-hole that UDN GRs cannot self-heal from. Dynamic MAC_Binding at priority 100 expires naturally and is re-resolved with the correct MAC.

### Kernel Neighbor Table as ARP Proxy Source of Truth

Watch the Linux kernel's neighbor table on breth0 via netlink. Program ARP proxy flows for neighbors in usable NUD states (`NUD_REACHABLE`, `NUD_STALE`, `NUD_DELAY`, `NUD_PROBE`, `NUD_PERMANENT` with a non-empty hardware address). Require `arp_accept=2` sysctl so the kernel learns from ARP replies steered to LOCAL that it didn't originate (UDN GRs sent the request, not the kernel).

**Advantages of this approach:**
- **Lower latency:** The netlink path has fewer hops (kernel event → userspace → flow sync) compared to the SB-DB path (GR learns → SB-DB write → libovsdb notification → flow sync). Both are expected to complete well before the next data-plane packet triggers a retry, but the kernel path has fewer intermediate steps.
- **No new process-level dependency:** Does not require adding an SB-DB client to the node process. Uses the well-established `vishvananda/netlink` library already vendored.

**Rejected in favor of SB-DB MAC_Binding because:**
- **Separate infrastructure from IPv6:** The IPv6 path already uses SB-DB MAC_Binding as its source of truth. Using the kernel table for IPv4 means two separate subsystems with different failure modes, event sources, and lifecycles. Unifying on SB-DB means one watcher, one event source, shared lifecycle code.
- **Security-relevant sysctl:** `arp_accept=2` allows same-subnet IPs to inject neighbor entries into the kernel without solicitation.
- **Stale MAC served without verification:** Kernel `NUD_STALE` entries are never probed. If a neighbor silently changes its MAC, the proxy serves the stale MAC indefinitely until kernel GC fires. OVN's MAC_Binding aging (300s) forces periodic re-resolution, ensuring correctness. OVN 25.03+ stale probes actively verify active bindings every ~56s.

## References

* [PR #6346](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6346): GARP storm and masquerade ARP fixes
* [PR #6660](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6660): Short-term ARP/NDP fan-out fix (`no-flood` + priority-12/11 steering). This OKEP's foundation — `no-flood` is inherited; flow-level changes become dead code under OKEP gates.
* [PR #5334](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/5334): FDB learning fix — established the `output:patch,NORMAL` pattern for FDB learning on br-ex, precedent for the trailing `NORMAL` in this OKEP's priority-45/52 flows.
* OVN commit [`58ce60d`](https://github.com/ovn-org/ovn/commit/58ce60d2f1d932b842512763c2b8fc0943e1f8e3): Background ARP/NDP stale probes (OVN 25.03+)
* OVN commit [`33bb66c`](https://github.com/ovn-org/ovn/commit/33bb66c6c8e6e119bf9006dbe868457eecf82c9e): `mac_cache_use` flow for MAC_Binding timestamp refresh
