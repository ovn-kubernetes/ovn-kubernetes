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
  network + all Cluster User Defined Network (CUDN) GRs) and also sent
  to NORMAL for LOCAL delivery.
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

When CUDNs are enabled, each CUDN gateway router (GR) has an external
interface that shares the node's physical IP. Sending external ARP and NDP
traffic to every GR creates duplicate replies and one copy of every learned
neighbor in each GR datapath. The shared-gateway bridge instead sends neighbor
discovery only to the default cluster network (CDN) GR. UDN GRs on the same
physical L2 domain use the CDN GR's MAC bindings.

The UDN external logical router port sets the OVN
`mac-binding-source=<CDN-external-LRP>` option. OVN redirects the UDN GR's
neighbor learning and lookup operations to that source port, including the
lookup that releases a packet buffered during a cold miss. The option is not
set for UDNs using a separate uplink because those GRs belong to a different
L2 domain and must learn their own neighbors.

#### No-Flood on CUDN Patch Ports

CUDN GR patch ports have the `OFPPC_NO_FLOOD` OpenFlow port config flag
set via `ovs-ofctl mod-port`. This prevents NORMAL's implicit flood
action from reaching them, while still allowing explicit `output:<port>`
actions to deliver traffic. The flag is initially set when the patch
port is registered in `SetNetworkOfPatchPort()` and periodically
re-applied by `SyncNoFlood()` during the OpenFlow sync loop (every
~15 s), since `OFPPC_NO_FLOOD` is volatile — it is not persisted in
OVSDB and is lost when `ovs-vswitchd` restarts or ports are re-created.

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
ingress port) and delivers to LOCAL via broadcast flooding (since
`dl_dst=ff:ff:ff:ff:ff:ff` for ARP, or solicited-node multicast for
NS). Because CUDN ports are no-flood, NORMAL's flood does not reach
them. Only the default GR replies — no storm.

#### Priority-45/52: External ARP/NDP Steering

When at least one CUDN patch is present, a priority-45 flow sends every ARP
packet arriving from the physical port to the CDN GR and NORMAL. This includes
requests, replies, and gratuitous ARP:

```text
priority=45, table=0, in_port=<phys>, arp,
    actions=output:<default-patch>,NORMAL
```

Priority-52 flows perform the same steering for the IPv6 control messages
that participate in router and neighbor discovery. The ICMPv6 type codes are:

- Router Advertisement (RA): type 134
- Neighbor Solicitation (NS): type 135
- Neighbor Advertisement (NA): type 136

```text
priority=52, table=0, in_port=<phys>, icmp6, icmpv6_type=134,
    actions=output:<default-patch>,NORMAL
priority=52, table=0, in_port=<phys>, icmp6, icmpv6_type=135,
    actions=output:<default-patch>,NORMAL
priority=52, table=0, in_port=<phys>, icmp6, icmpv6_type=136,
    actions=output:<default-patch>,NORMAL
```

Priority 52 is above the priority-50 IPv6 conntrack rule, so both multicast
and unicast NDP bypass the old table-1 fan-out path. ARP is not IP traffic, so
priority 45 only needs to precede the priority-10 generic fan-out rule.

NORMAL retains the gateway VLAN tag while it performs FDB learning and host
delivery. The no-flood setting excludes CUDN patches, while the explicit
output delivers one copy to the CDN patch. A GARP or unsolicited NA therefore
updates the CDN binding immediately, and all shared-L2 UDN GRs consume that
same binding without receiving a duplicate packet.

#### Priority-0: Catch-All NORMAL

All remaining traffic falls through to the default rule:

```text
priority=0, table=0, actions=NORMAL
```

Because CUDN patch ports have `no-flood`, NORMAL's flood does not reach them.
When a CUDN sends an ARP for an external host, NORMAL sends it to the physical
network. OVN records the reply against the CDN source port and releases the
requesting UDN's buffered packet through the shared binding lookup.

The priority-12, priority-45, and priority-52 table-0 flows are generated by
`neighborDiscoverySteeringFlows()` in
`go-controller/pkg/node/bridgeconfig/bridgeflows.go`. The priority-0 catch-all
NORMAL rule is part of the base bridge configuration.

#### Table-1: ICMPv6 FLOOD

Existing table-1 flows FLOOD ICMPv6 Router Advertisements (type 134) and
Neighbor Advertisements (type 136) because they cannot create conntrack
entries (kernel bug). On the shared bridge, CUDN patches are no longer
prepended as explicit outputs:

```text
priority=14, table=1, icmp6, icmpv6_type=134, actions=FLOOD
priority=14, table=1, icmp6, icmpv6_type=136, actions=FLOOD
```

A separate-uplink bridge has no CDN patch or shared binding owner, so its
priority-14 flows retain explicit output to that bridge's UDN patches.

## Code Reference

All flow generation lives in
`go-controller/pkg/node/bridgeconfig/bridgeflows.go`. The two main entry
points are:

- `flowsForDefaultBridge()` — flows specific to the default bridge setup
  (encap handling, egress, conntrack).
- `commonFlows()` — flows shared across configurations (fan-out, ARP
  filter, table 1/2/3/4/5 dispatch, UDN isolation).
