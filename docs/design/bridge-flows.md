# External Bridge (breth0) OpenFlow Design

## Introduction

OVN-Kubernetes programs OpenFlow rules on the external bridge (`breth0`)
to steer traffic between the physical network,
OVN patch ports, and the kernel (LOCAL port or a PF/VF representor on DPU
setups). This document describes the flow tables, their responsibilities,
and the design decisions behind notable flow sets.

For service-specific traffic flows, see
[Host-to-NodePort Hairpin Traffic](host-to-node-port-hairpin-trafficflow.md)
and [Service Traffic Policy](service-traffic-policy.md).

## The Shared Bridge

The external bridge (`breth0`) is common to both shared gateway and local
gateway modes. During node setup, ovnkube-node creates an OVS bridge named
`br<iface>` (e.g. `eth0` → `breth0`), moves the primary NIC into it as
an uplink port, and migrates the NIC's IP addresses and routes onto the
bridge. The host's physical IP and MAC are then shared between the kernel
and OVN — each Gateway Router's (GR) external port uses the same MAC as
`breth0`.

The key difference between gateway modes is what happens *after* traffic
leaves OVN:

- **Shared gateway** — traffic stays within the OVS datapath and exits
  directly via `breth0`.
- **Local gateway** — traffic exits OVN to the host kernel via the
  management port (`ovn-k8s-mp0`), then the host routes it out.

The OpenFlow rules on `breth0` described below apply to both modes unless
noted otherwise.

## Layer 2 Behavior

By default, `breth0` table 0 forwards packets using standard L2 switching
behavior via the `priority=0, actions=NORMAL` rule. Two higher-priority
rules handle traffic destined to the shared bridge MAC:

- **Priority 10 (fan-out)** — When a packet arrives with
  `dl_dst=<bridgeMAC>`, it is replicated to every OVN patch port (default
  network + all UDN GRs) and also sent to NORMAL for LOCAL delivery.
  This ensures all OVN networks can process traffic destined to the node.

  ```text
  priority=10, table=0, dl_dst=<bridgeMAC>,
      actions=output:<patch-default>,output:<patch-udn1>,...,NORMAL
  ```

- **Priority 10 / 9 (OVN egress validation)** — For each patch port, a
  rule verifies that traffic coming from OVN has `dl_src=<bridgeMAC>`. A
  companion priority 9 rule drops traffic from patch ports with incorrect
  source MAC. These don't conflict with the fan-out rule: the egress validation
  rules match `in_port=<patch-X>` (traffic originating from OVN), while
  the fan-out rule matches `dl_dst=<bridgeMAC>` (traffic destined *to*
  the node, typically arriving from the physical port).

  ```text
  priority=10, table=0, in_port=<patch-X>, dl_src=<bridgeMAC>, actions=output:NORMAL
  priority=9,  table=0, in_port=<patch-X>, actions=drop
  ```

### ARP/NDP Behavior

When Cluster User Defined Networks (CUDNs) are enabled, each CUDN gateway
router (GR) has an external interface that shares the node's physical IP.
Without filtering, every GR replies to ARP/NDP requests for the node IP,
creating a reply storm that floods the physical network with identical
frames. Remote nodes passively learn redundant `MAC_Binding` entries,
and `ovs-vswitchd` CPU scales linearly with the number of CUDNs. In
testing, 70 CUDNs drove `ovs-vswitchd` CPU to ~400% with packet drops.

#### No-Flood on CUDN Patch Ports

CUDN GR patch ports are configured with `other_config:no-flood=true`
via libovsdb. This prevents NORMAL's implicit flood action from reaching
them, while still allowing explicit `output:<port>` actions to deliver
traffic. The no-flood flag is set when the patch port is registered in
`SetNetworkOfPatchPort()`.

#### Priority-12: Node IP ARP/NDP Filter

Priority-12 flows intercept ARP requests and IPv6 Neighbor Solicitations
for the node IP. They match `arp_tpa` / `nd_target` for the node IP
without restricting `dl_dst` or `in_port`, so they catch broadcast,
unicast, and multicast variants from any source. The action sends only
to the default network patch port plus NORMAL:

```text
priority=12, table=0, arp, arp_op=1, arp_tpa=172.18.0.3,
    actions=output:<default-patch>,NORMAL
priority=12, table=0, icmp6, icmpv6_type=135, nd_target=fd00::3,
    actions=output:<default-patch>,NORMAL
```

NORMAL performs FDB learning (recording the external source MAC on its
ingress port) and delivers to LOCAL via the static FDB entry. Because
CUDN ports are no-flood, NORMAL's flood does not reach them. Only the
default GR replies — no storm.

#### Priority-11: External Broadcast ARP/NA Forwarding

With no-flood, broadcast ARP from external sources (e.g. a gateway router
announcing a new MAC via GARP) would no longer reach CUDNs. Priority-11
flows match broadcast ARP arriving on the physical port and explicitly
forward to all GR patches so their MAC_Bindings stay current:

```text
priority=11, table=0, in_port=<phys>, dl_dst=ff:ff:ff:ff:ff:ff, arp,
    actions=output:<default-patch>,output:<cudn1-patch>,...,NORMAL
priority=11, table=0, in_port=<phys>, dl_dst=33:33:00:00:00:01, icmp6, icmpv6_type=136,
    actions=output:<default-patch>,output:<cudn1-patch>,...,NORMAL
```

The IPv6 flow matches `dl_dst=33:33:00:00:00:01` — the Ethernet multicast
MAC for IPv6 all-nodes (`ff02::1`). This restricts to unsolicited Neighbor
Advertisements (the IPv6 equivalent of GARP) only; unicast solicited NAs
already reach the correct port via the priority-10 bridge-MAC fan-out.

The `in_port=<phys>` match ensures only externally-originated broadcast
is forwarded; OVN-originated traffic from patch ports is unaffected.
Node-IP ARPs never fall here (caught at priority-12 first).

#### Why All Three Layers Are Required

The no-flood setting, priority-12, and priority-11 form a dependency chain
where each compensates for the one below:

1. **no-flood** prevents NORMAL from flooding to CUDNs → solves the storm.
2. **Priority 11** is needed because no-flood also blocks legitimate
   broadcast ARP (GARPs from external routers) → explicitly forwards
   external broadcast to all GR patches so MAC bindings stay current.
3. **Priority 12** is needed because priority 11 fans out ALL external
   broadcast ARP, including node-IP ARP requests which would re-create
   the storm → intercepts node-IP ARPs first and sends only to the
   default patch.

Removing any one breaks the design:

- Without priority 12: node-IP ARPs hit priority 11 → storm returns.
- Without priority 11: GARPs never reach CUDNs → stale MAC bindings.
- Without no-flood: NORMAL floods everything to CUDNs → storm returns
  (and priorities 11/12 become pointless).

All table-0 flows are generated by `arpFanoutFilterFlows()` in
`go-controller/pkg/node/bridgeconfig/bridgeflows.go`.

#### Table-1: ICMPv6 FLOOD with CUDN Delivery

Existing table-1 flows FLOOD ICMPv6 Router Advertisements (type 134) and
Neighbor Advertisements (type 136) because they cannot create conntrack
entries (kernel bug). Since FLOOD also respects no-flood, CUDN patches
are prepended as explicit outputs:

```text
priority=14, table=1, icmp6, icmpv6_type=134,
    actions=output:<cudn1-patch>,output:<cudn2-patch>,...,FLOOD
priority=14, table=1, icmp6, icmpv6_type=136,
    actions=output:<cudn1-patch>,output:<cudn2-patch>,...,FLOOD
```

When no CUDNs are configured, these remain plain `actions=FLOOD`.

## Code Reference

All flow generation lives in
`go-controller/pkg/node/bridgeconfig/bridgeflows.go`. The two main entry
points are:

- `flowsForDefaultBridge()` — flows specific to the default bridge setup
  (encap handling, egress, conntrack).
- `commonFlows()` — flows shared across configurations (fan-out, ARP
  filter, table 1/2/3/4/5 dispatch, UDN isolation).
