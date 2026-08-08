# Masquerade IPs

## Overview

OVN-Kubernetes uses **masquerade IPs** — synthetic node-local addresses
that must never appear on the physical network — as intermediate SNAT
addresses on the external bridge (`breth0`). They serve two main
purposes:

1. **Network identification on the shared bridge.** Multiple CUDNs can
   have **overlapping pod subnets** (e.g. two UDNs both using
   `10.244.0.0/16`). They all share the same `breth0` and the same
   node physical IP. When return traffic arrives at the node, OVS
   cannot distinguish which network the packet belongs to by
   destination IP alone because the pod subnets overlap. To solve
   this, a **double-SNAT** is used:

   - **Egress (pod → wire):** OVN's GR SNATs the pod source IP to the
     UDN's unique masquerade IP (e.g. `169.254.0.11`). OVS on `breth0`
     then SNATs the masquerade IP to the node's physical IP before
     sending the packet on the wire. The masquerade IP is recorded in
     conntrack alongside a per-network `ct_mark`:

     ```text
     priority=100, in_port=<cudn-patch>, dl_src=<bridgeMAC>, ip, ip_src=169.254.0.11,
         actions=ct(commit, zone=<ctZone>, nat(src=<nodeIP>), exec(set_field:<ct_mark>->ct_mark)),
                 output:<phys>
     ```

   - **Ingress (wire → pod):** OVS un-SNATs node IP → masquerade IP and
     uses the `ct_mark` to identify the originating network, routing
     the packet to the correct UDN patch port. OVN's GR then un-SNATs
     masquerade IP → original pod IP.

   The per-UDN masquerade IP is the piece that makes this work — it is
   a unique, non-overlapping identifier for each network in conntrack.

2. **Host-to-service traffic steering.** When the host kernel itself
   accesses a Kubernetes Service, OVS SNATs the host's source IP to a
   masquerade IP so the reply returns through OVS (where it can be
   un-SNATted) instead of going directly to the host (which would
   bypass conntrack state and break the connection). Similar masquerade
   IPs handle service hairpin and `externalTrafficPolicy=Local`
   scenarios.

Masquerade IPs are **internal to each node** and should **never appear
on the wire**. Traffic to/from these IPs is handled within the OVS
datapath across multiple flow priorities and tables on `breth0`
(priority-500 for specific masquerade IP matching, priority-100 for UDN
egress SNAT, priority-50 for conntrack entry, and tables 1–5 for
ct_mark-based dispatch). See [bridge-flows.md](bridge-flows.md) for the
OpenFlow design.

## Masquerade Subnet

The masquerade subnet is configured via two ovnkube flags:

| Flag | Default | Example for UDN-enabled clusters |
|------|---------|----------------------------------|
| `--gateway-v4-masquerade-subnet` | `169.254.169.0/29` (8 addresses) | `169.254.0.0/17` (32768 addresses) |
| `--gateway-v6-masquerade-subnet` | `fd69::/125` (8 addresses) | `fd69::/112` (65536 addresses) |

All masquerade IPs — both global (default network) and per-UDN — are
allocated from this single subnet.

The small default `/29` / `/125` contains only 8 addresses (offsets
0–7), which is sufficient for clusters that only use the default
network (6 of 8 slots used: the network address + 5 global IPs).
Clusters with User Defined Networks (UDNs) **must** configure a larger
subnet because the first per-UDN IP is at offset 11 — outside the
default `/29`. Each UDN allocates 2 additional IPs. A `/17` (IPv4) or
`/112` (IPv6) supports thousands of networks.

The IPv4 range (`169.254.0.0/16`) is link-local space; the IPv6 range
(`fd69::/112`) is within ULA space (`fd00::/8`), which *can* be routed
within private networks. Neither range is inherently non-routable —
they are intended strictly for node-local use and must not be advertised
or routed off-node.

## Address Layout Within the Subnet

The subnet is divided into two regions: a **fixed global region**
(offsets 0–5 allocated, 6–9 reserved) for the default network, and a
**dynamic UDN region** (offset 11+) for per-network masquerade IPs.

### Global Region (Offsets 0–9)

Offsets 0–9 are reserved for global use. Offset 0 is the subnet network
address and offsets 1–5 are currently allocated. Offsets 6–9 are
reserved for future global IPs. Note: offsets 6–9 only exist when the
subnet is larger than the default `/29` / `/125` (which contain only
offsets 0–7).

Using `169.254.0.0/17` as example:

| Offset | IPv4 | IPv6 | Role | Code constant |
|--------|------|------|------|---------------|
| 0 | `169.254.0.0` | `fd69::` | Subnet network address (not usable) | — |
| 1 | `169.254.0.1` | `fd69::1` | **OVN Masquerade** — GR SNAT source for default-network service traffic. The GR rewrites pod source IPs to this before forwarding to the service backend. | `V4OVNMasqueradeIP` |
| 2 | `169.254.0.2` | `fd69::2` | **Host Masquerade** — host-to-service SNAT source. When the host itself accesses a ClusterIP service, OVS SNATs the host IP to this so the reply returns through OVS (not direct to the host, which would bypass un-SNAT). | `V4HostMasqueradeIP` |
| 3 | `169.254.0.3` | `fd69::3` | **Host ETP=Local Masquerade** — `externalTrafficPolicy=Local` variant. Separate from .2 so that OVS can distinguish ETP=Local traffic and preserve the original client IP for the backend pod. | `V4HostETPLocalMasqueradeIP` |
| 4 | `169.254.0.4` | `fd69::4` | **Dummy Next-Hop** — synthetic next-hop for masquerade routing. The host routing table has a route to the masquerade subnet via this next-hop. A static ARP/ND entry maps this IP to a synthetic MAC, ensuring packets reach OVS without real ARP resolution. | `V4DummyNextHopMasqueradeIP` |
| 5 | `169.254.0.5` | `fd69::5` | **OVN Service Hairpin** — hairpin traffic SNAT source. When a pod sends traffic to a Service and the same pod is selected as backend, OVN SNATs to this IP to force the return path back through the load balancer. | `V4OVNServiceHairpinMasqueradeIP` |
| 6–9 | — | — | Reserved for future global use | — |

The global IPs are defined as fields in `MasqueradeIPsConfig`
(`go-controller/pkg/config/config.go`) and sequentially allocated at
startup by `config.AllocateV4MasqueradeIPs` /
`config.AllocateV6MasqueradeIPs` in `go-controller/pkg/config/utils.go`.

### UDN Region (Offset 11+)

When network segmentation is enabled, each UDN gets its own pair of
masquerade IPs computed from the network's integer ID. The allocation
base constant is 10 (`userDefinedNetworkMasqueradeIPBase`), but the
formula `base + networkID*2 - 1` means the first real allocation
(networkID=1) starts at offset 11. Each network consumes 2 IPs:

```text
GatewayRouter  = subnet_base + 10 + (networkID × 2) - 1
ManagementPort = subnet_base + 10 + (networkID × 2)
```

| Offset | networkID | IPv4 | IPv6 | Role |
|--------|-----------|------|------|------|
| 11 | 1 | `169.254.0.11` | `fd69::b` | UDN-1 Gateway Router masquerade |
| 12 | 1 | `169.254.0.12` | `fd69::c` | UDN-1 Management Port masquerade |
| 13 | 2 | `169.254.0.13` | `fd69::d` | UDN-2 Gateway Router masquerade |
| 14 | 2 | `169.254.0.14` | `fd69::e` | UDN-2 Management Port masquerade |
| ... | ... | ... | ... | ... |

Each UDN's **Gateway Router** masquerade IP serves the same role as the
global `V4OVNMasqueradeIP` (.1) but scoped to that network: the UDN's
GR SNATs pod source IPs to this address for service traffic. The
**Management Port** masquerade IP is used for management port traffic
within the UDN.

The gap between offset 5 (last used global IP) and offset 11 (first UDN
IP) provides room for future global allocations without breaking
existing UDN deployments.

Per-UDN allocation is in `udn.AllocateV4MasqueradeIPs` /
`udn.AllocateV6MasqueradeIPs` (`go-controller/pkg/generator/udn/masquerade_ips.go`).

## Kernel Static Neighbor Entries

### Why They Are Needed

Masquerade IPs are handled entirely within the OVS datapath. Under
normal operation, return service traffic arriving on the physical port
is matched by OVS flows, un-SNATted via conntrack, and dispatched to the
correct OVN patch port — the kernel is never involved.

However, in **LGW + no-overlay (BGP) mode**, the kernel can become
involved in the return path. Without static neighbor entries, the kernel
has no way to resolve the L2 address for masquerade IPs and falls back
to ARP/NDP — which should never happen for addresses that are internal
to the node.

A concrete example is cross-node NodePort traffic with CUDN pods:

```text
ovn-control-plane                              ovn-worker2
┌──────────────┐                           ┌──────────────┐
│ CUDN pod A   │                           │              │
│ (client)     │──── BGP route ───────────▶│ NodePort svc │
│              │                           │ (worker2 IP) │
│ CUDN pod B   │◀── BGP route ────────────│              │
│ (backend)    │                           │              │
└──────────────┘                           └──────────────┘
```

1. Pod A on `ovn-control-plane` sends to NodePort on `ovn-worker2`'s IP.
2. Traffic arrives at `ovn-worker2` via BGP (no GENEVE, no join IPs —
   packets go directly on the wire between nodes).
3. OVS on `ovn-worker2` → GR DNAT (NodePort → pod B on control-plane)
   → GR SNAT (pod A IP → masquerade IP `fd69::b`) → OVS SNAT
   (`fd69::b` → worker2 node IP) → out the wire to control-plane.
4. Pod B responds → traffic returns to `ovn-worker2` via BGP.
5. OVS un-SNATs (worker2 node IP → `fd69::b`), `ct_mark` identifies
   the CUDN → dispatches to CUDN patch port → GR un-SNATs/un-DNATs.

In step 5, if the return traffic leaks to the kernel (e.g. due to
conntrack state mismatch for IPv6), the kernel attempts to forward the
packet to `fd69::b`. Without a static neighbor entry, it performs
standard neighbor discovery:

- **IPv4 (without static entry)**: the kernel sends an ARP request
  (`dl_dst=ff:ff:ff:ff:ff:ff`) for the masquerade IP on `breth0` via
  the `LOCAL` port. Since `in_port=LOCAL`, this does **not** match
  priority-11 (which requires `in_port=<phys>`). It falls through to
  priority-0 `NORMAL`, which floods the broadcast to all non-NO_FLOOD
  ports — the physical port and the default network patch, but **not**
  CUDN patches. The ARP request leaks to the wire, reaches remote
  nodes, and one of them happens to respond (its CUDN GR owns the
  masquerade IP and replies). The ARP reply arrives back on the
  physical port with `dl_dst=bridgeMAC`, gets delivered to `LOCAL`, and
  the kernel learns the neighbor. So IPv4 happens to work — not because
  the local GR responds, but because the ARP leaks to the wire and a
  remote GR answers. This is undesirable: masquerade IP ARP should
  never appear on the wire.
- **IPv6 (without static entry)**: the kernel sends a Neighbor
  Solicitation (`dl_dst=33:33:ff:xx:xx:xx`) for the masquerade IP on
  `breth0`. This solicited-node multicast MAC is neither broadcast nor
  the bridge MAC, so it also falls through to priority-0 `NORMAL`.
  `NORMAL` floods unknown multicast to all non-NO_FLOOD ports — including
  the physical port — so the NS **also leaks to the wire**, just like
  IPv4 ARP. However, on the remote node the NS still gets no response:
  priority-11 only matches all-nodes multicast (`33:33:00:00:00:01`),
  not solicited-node multicast (`33:33:ff:xx:xx:xx`), so the NS falls
  to NORMAL again, where NO_FLOOD prevents it from reaching CUDN
  patches. No CUDN GR ever sees the NS, the kernel's neighbor entry
  enters `FAILED` state, and the traffic is dropped.

Static neighbor entries eliminate both problems: the kernel never sends
ARP/NS for masquerade IPs, keeping this traffic off the wire entirely.

### The Fix: Permanent Neighbor Entries

Rather than adding OpenFlow rules to forward NS to CUDN patches (which
would be a workaround for traffic that shouldn't be on the wire in the
first place), we add **permanent kernel neighbor entries** for each
masquerade IP on `breth0`:

```bash
ip neigh add 169.254.0.11 lladdr 0a:58:a9:fe:00:0b dev breth0 nud permanent
ip neigh add fd69::b      lladdr 0a:58:XX:XX:XX:XX dev breth0 nud permanent
```

The MAC is derived using `util.IPAddrToHWAddr()`, the same synthetic MAC
scheme used by `addHostMACBindings()` for global masquerade IPs. With
this entry present, the kernel never sends ARP/NS for the masquerade IP.
If traffic does reach the kernel, it constructs the L2 frame immediately
and sends it out via `breth0` (LOCAL port in OVS), where it is matched
by OVS flows and handled correctly.

### Global vs Per-UDN Neighbor Entries

| Scope | IPs | Added by | Reconciled |
|-------|-----|----------|------------|
| Global (default network) | `V4OVNMasqueradeIP` + `V4DummyNextHopMasqueradeIP` (and v6 equivalents — 2 per family) | `addHostMACBindings()` in gateway init | Yes — `masqueradeReconciler` re-applies on link events |
| Per-UDN | `V4MasqIPs.GatewayRouter`, `V6MasqIPs.GatewayRouter` | `addUDNMasqIPNeighbors()` in UDN `addNetwork()` | Not yet — only at network add time. On `ovnkube-node` restart, `addNetwork()` re-runs for every UDN and re-creates the entries. The only uncovered gap is `ovs-vswitchd` restart (which recreates the bridge interface and wipes kernel neighbors) while `ovnkube-node` stays running. Extending `masqueradeReconciler` to cover per-UDN entries is a follow-up. |

Cleanup: per-UDN entries are removed by `delUDNMasqIPNeighbors()` when
the network is deleted.

### Code References

- Global masquerade neighbor entries: `addHostMACBindings()` in
  `go-controller/pkg/node/gateway_shared_intf.go`
- Per-UDN masquerade neighbor entries: `addUDNMasqIPNeighbors()` /
  `delUDNMasqIPNeighbors()` in `go-controller/pkg/node/gateway_udn.go`
- Masquerade IP config and allocation:
  `go-controller/pkg/config/config.go` (`MasqueradeIPsConfig`)
- Per-UDN IP allocation:
  `go-controller/pkg/generator/udn/masquerade_ips.go`
- OVS priority-500 masquerade flows:
  `go-controller/pkg/node/bridgeconfig/bridgeflows.go`
  (`flowsForDefaultBridge()`)
