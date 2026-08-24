# OKEP-6815: Add NetworkQoS support for ipamless localnet networks

* Issue: [#6815](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6815)

## Problem Statement

NetworkQoS ([OKEP-4380](okep-4380-network-qos.md)) delivers DSCP marking and
bandwidth policing by building OVN QoS match expressions from pod IP addresses
read from the `k8s.ovn.org/pod-networks` annotation.

On secondary **localnet** networks with IPAM disabled (**ipamless**) - the topology commonly
used by KubeVirt / OpenShift Virtualization VMs whose IP addresses are managed
statically or by an external DHCP server - pods have no OVN-Kubernetes-managed
IPs, so `getPodAddresses()` returns nothing and the controller silently skips
them. As a result NetworkQoS is non-functional on these networks, and there is
today no supported way to apply network QoS (workload-tier prioritization,
backup-traffic deprioritization, or bandwidth capping) to VMs running on them.

Ipamless **layer2** networks share the same underlying limitation, but this
proposal is scoped to **localnet**; see [Non-Goals](#non-goals) and
[Future Goals](#future-goals) for how layer2 is treated.

## Goals

- Make NetworkQoS functional on secondary ipamless **localnet** networks. (The
  intent to do so without changing the NetworkQoS CRD is captured as R7; the need
  for network-dependent semantics to be discoverable to users is captured as
  R13.)
- Support, on ipamless networks, the QoS capabilities the target use cases
  require, **without requiring OVN-Kubernetes to discover or manage pod IP
  addresses**:
  - source pod/VM selection by label (`podSelector`);
  - DSCP marking;
  - bandwidth policing (`rate` / `burst`);
  - protocol + port classification (TCP/UDP/SCTP + port);
  - destination matching by IP range (`ipBlock` / CIDR);
  - priority ordering across and within NetworkQoS objects.
- Support QoS at **both** levels the use cases demand. OVN-Kubernetes
  guarantees bandwidth policing in the OVN/OVS datapath (excess dropped within
  the cluster). For fabric-level QoS, OVN-Kubernetes **marks** DSCP on egress
  traffic - which on tunnel-free localnet is directly visible to the fabric -
  but the fabric only enforces (queues/prioritizes) that marking when the
  physical network is configured to honor and map DSCP to hardware queues; that
  configuration is outside OVN-Kubernetes' control.
- Preserve **identical behavior on IPAM-enabled networks**. The existing
  IP-based address-set path remains the production-proven path for all current
  NetworkQoS deployments - no regressions, no behavioral change, no performance
  impact.

## Non-Goals

- **Ipamless layer2 topology, as a committed deliverable.** This proposal
  targets localnet. The enabling mechanism is expected to apply to ipamless
  layer2 as well; **if layer2 support comes essentially for free** (little or no
  additional work beyond the localnet implementation), it will be folded into
  this effort opportunistically. **If layer2 requires meaningful additional
  work**, it will be pursued as a **separate enhancement** rather than expanding
  this one (see [Future Goals](#future-goals)).
- **Destination selection by `podSelector` / `namespaceSelector` on ipamless
  networks.** Destination matching on ipamless networks is limited to
  `ipBlock` (IP / CIDR). The target use cases in this OKEP do not require
  selecting *destination* pods by label (see Requirements and User-Stories).
  Supporting labeled-pod destinations on ipamless networks is a
  [Future Goal](#future-goals): it depends on resolving destination pod
  identity, which under OVN Interconnect is topology-dependent (and, on
  localnet specifically, not reliably resolvable across nodes).
- **NetworkQoS on layer3 ipamless topology.** Layer3 uses per-node logical
  switches connected by a logical router; layer3 also exists primarily for
  routed multi-subnet deployments, which implies IPAM.
- **IP discovery or reporting for ipamless networks** (DHCP snooping, KubeVirt
  VMI status watching, ARP/NDP learning, IP-claim CRDs, or similar). This OKEP
  does not add any mechanism for OVN-Kubernetes to learn the guest's IP.
- **Changes to the NetworkQoS CRD API / schema.** Users create the same
  `NetworkQoS` resources with the same fields; only controller-internal
  behavior changes.
- **Changes to EgressQoS.** EgressQoS is default-network-only and always has
  IPAM.
- **MultiNetworkPolicy changes for ipamless networks.** The same restriction
  (only `ipBlock` peers on ipamless networks) exists for MultiNetworkPolicy and
  is unblocked by the same underlying enabling work, but the MNP implementation
  is a separate effort (see [Future Goals](#future-goals)).
- **Ingress-direction QoS and traffic shaping.** NetworkQoS is egress-only and
  *polices* (drops excess) rather than *shapes* (queues) - an existing product
  constraint, not introduced here.
- **Minimum bandwidth guarantees / reservations.** Not supported by OVN.
- **Arbitration against other node traffic (host/OCP, CSI, other networks).**
  NetworkQoS governs only the selected localnet UDN's own egress. It does not
  prioritize that traffic over - or protect it from - other traffic sharing the
  node's uplink. The two mechanisms it offers are: **throttling** the UDN's
  traffic (bandwidth policing, a rate cap on that traffic alone), and **DSCP
  marking**, which has no local forwarding effect and is only acted upon by the
  physical fabric. Neither is a cross-class scheduler, and there is no
  minimum-bandwidth guarantee relative to other traffic.

## Future Goals

- **Ipamless layer2 support**, if it is not delivered opportunistically as part
  of this proposal. Should enabling layer2 require more than trivial additional
  work beyond the localnet implementation, it will be tracked and delivered as a
  separate enhancement that reuses this work.
- **Destination `podSelector` / `namespaceSelector` on ipamless networks** -
  "apply QoS to traffic *toward* a labeled set of pods." This is the principal
  capability deferred by this OKEP. It becomes relevant only if a future
  requirement needs to select destination pods by label rather than by address
  range. Because destination matching is built from IP address sets (not port
  identity), delivering it comes down to getting the pod IP into the
  `k8s.ovn.org/pod-networks` annotation so the existing IP-based path applies -
  e.g. KubeVirt static-IP propagation, or the DHCP IPAM path of
  [OKEP-6224](okep-6224-dhcp-ipam-localnet.md).
- **MultiNetworkPolicy pod/namespace selectors on ipamless networks.** The same
  enabling mechanism lifts the current "IPAM-less networks can only have
  `ipBlock` peers" restriction for MNP. Separate PR.
- **Convergence with DHCP IPAM ([OKEP-6224](okep-6224-dhcp-ipam-localnet.md)).**
  DHCP-mode networks have subnets configured (for the DHCP pool), so
  `DoesNetworkRequireIPAM()` returns true and they already follow the standard
  IP-based path. A future optimization could unify the ipamless and DHCP paths.

## Introduction

### QoS mechanisms in OVN-Kubernetes today

OVN-Kubernetes offers three QoS mechanisms. The table summarizes their
capabilities relative to the requirements of this OKEP.

| Capability | NetworkQoS (`v1alpha1`) | EgressQoS (`v1`) | Pod Bandwidth Annotations |
|---|---|---|---|
| DSCP marking (0–63) | Yes | Yes | No |
| Bandwidth policing (rate+burst) | Yes - excess dropped | No | Egress only |
| Traffic shaping (queuing) | No | No | Ingress only (linux-htb) |
| Destination CIDR match | Yes | Yes | No |
| Destination pod/ns selector | Yes (IPAM-enabled only today) | No | No |
| Protocol + port match | Yes (TCP/UDP/SCTP) | No | No |
| Source pod selector | Yes (spec-level) | Yes (per-rule) | No |
| Multi-network / UDN | Yes | No (default only) | Limited |
| Multiple objects per namespace | Yes | No (one, named `default`) | N/A |
| Priority | 0–100 (higher wins) | Array position | N/A |
| Direction | Egress only | Egress only | Both |

**NetworkQoS is the only mechanism with the expressiveness the target use cases
need** - multi-network/UDN support, DSCP marking, bandwidth policing, and
traffic classification by protocol/port. EgressQoS is its default-network-only
predecessor; pod bandwidth annotations are per-interface with no traffic
classification.

### How NetworkQoS works, and its IP dependency

The NetworkQoS CRD (`k8s.ovn.org/v1alpha1`) is namespace-scoped and defines:

- **`podSelector`** - selects source pods by label (empty = all pods in the
  namespace).
- **`networkSelectors`** - restricts the rule to specific networks (default,
  UDN, NAD).
- **`priority`** (0–100) - resolves conflicts when multiple NetworkQoS objects
  match the same packet; higher value wins.
- **`egress`** (ordered list, up to 20 rules) - each rule specifies `dscp`
  (0–63), an optional `bandwidth` (`rate` in kbps, `burst` in kilobits;
  policing, not shaping), and an optional `classifier` matching by destination
  (`ipBlock`, `podSelector`, `namespaceSelector`) and/or by `ports` (protocol +
  port). Later rules in the list take higher precedence.

Internally, the controller creates OVN address sets for source (and
destination) pods, builds match expressions such as
`ip4.src == {$src_as} && tcp && tcp.dst == 8080`, and attaches OVN QoS entries
(direction `to-lport`, i.e. egress from the pod's perspective) to the network's
logical switch. Every step depends on pod IP addresses obtained from the
`k8s.ovn.org/pod-networks` annotation. On ipamless networks that annotation
carries a MAC but no IPs, so the controller has nothing to match on and skips
the pod.

### Why ipamless secondary localnet is the target topology

Enterprises migrating VM workloads to OpenShift Virtualization need the same
network QoS controls they had on traditional hypervisors (e.g. VMware ESX):
differentiating priority between workload tiers, between application and
infrastructure traffic, and capping bandwidth for specific traffic classes.

These VMs commonly attach to **secondary ipamless localnet UDNs** - the guest
OS or an external DHCP server manages addressing, not OVN-Kubernetes.

A natural question is why these workloads do not simply adopt DHCP IPAM
([OKEP-6224](okep-6224-dhcp-ipam-localnet.md)), which would place them on the
standard IP-based path (where destination `podSelector` also works). The target
population is precisely the set that cannot: VMs with truly static, externally
managed addresses - for these, no OVN-managed IP ever exists, so an
IP-independent matching path is the only option. Workloads that *can* use
DHCP IPAM should prefer it (see [Future Goals](#future-goals) on convergence).

This proposal targets localnet; ipamless layer2 networks share the same
limitation and may benefit from the same fix, but layer2 is not a committed
deliverable here (see [Non-Goals](#non-goals) and
[Future Goals](#future-goals)).

On localnet there is **no Geneve tunnel**: traffic egresses directly onto the
physical network, so a DSCP value stamped on the IP header is immediately
visible to the fabric with no inner/outer-header concerns. This is favorable
for fabric-level prioritization - provided the physical network is configured
to honor DSCP and map it to hardware queues.

The diagram below shows the egress traffic path for a KubeVirt VM on a secondary
ipamless localnet UDN, and where each QoS action is enforced. Everything left of
the enforcement boundary is under OVN-Kubernetes' control on the *sending* node;
everything right of it is the physical fabric, which OVN-Kubernetes never sees
again (the basis for the source-only matching argument in the next section).

```text
           sending node (OVN-Kubernetes control)          │  physical fabric
                                                          │  (out of OVN-K control)
 ┌───────────┐   ┌───────────────────────────────────────┐│
 │ KubeVirt  │   │        secondary localnet UDN         ││
 │   VM      │   │  (ipamless: MAC only, no OVN-managed  ││
 │ (source   │ ─>│   pod IP; label-selected source)      ││
 │  pod,     │   │                                       ││
 │  label:   │   │   logical switch port ── zone-local   ││
 │  tier=..) │   │   on the sending node, so the source  ││
 └───────────┘   │   is always identifiable (no pod IP)  ││
                 └───────────────┬───────────────────────┘│
                                 ▼                        │
                 ┌───────────────────────────────────────┐│
                 │        OVN/OVS QoS (to-lport)         ││
                 │  • source matching   (by port/label)  ││
                 │  • dst matching      (ipBlock/CIDR)   ││
                 │  • bandwidth policing (rate/burst)    ││
                 │  • DSCP marking      (stamp IP hdr)   ││
                 └───────────────┬───────────────────────┘│
                                 ▼                        │
                 ┌───────────────────────────────────────┐│      ┌─────────────┐
                 │   OVS bridge → physical NIC           ││      │ QoS-aware   │
                 │   (localnet: NO Geneve tunnel;        ││----▶ │ switches    │
                 │    DSCP on outer IP hdr, fabric-      ││ DSCP │ honor DSCP, │
                 │    visible immediately)               ││ on   │ map to HW   │
                 └───────────────────────────────────────┘│ wire │ queues      │
                                                          │      └─────────────┘
                 <------- enforcement boundary ---------->│
```

Because there is no tunnel, the DSCP value stamped by OVN/OVS QoS travels on the
same IP header the fabric reads - no inner/outer-header translation is needed for
the marking to reach QoS-aware physical switches.

### Scope of destination matching in this proposal

Destination matching in this OKEP is limited to **IP range (`ipBlock` / CIDR)**.
Source selection remains fully label-driven; classification by protocol/port is
unchanged; DSCP, bandwidth, and priority are unchanged. Labeled-pod destination
selection on ipamless networks is deferred to [Future Goals](#future-goals).

This is a deliberate scoping decision, not merely an implementation shortcut.
The reasons it is the right initial scope:

1. **On localnet, source is the only thing we can reliably match and enforce
   on.** OVN-Kubernetes enforces QoS at the sending node's OVN/OVS datapath. On
   localnet there is no tunnel and no cluster-managed path to the destination:
   the moment a packet egresses the node onto the physical fabric it leaves
   OVN-Kubernetes' control and visibility entirely - it can be routed, NAT'd,
   re-marked, or dropped, and OVN-Kubernetes will never see it again. The source
   pod, by contrast, is right there on the local switch and always identifiable.
   Destination pod *identity* is out of reach on ipamless networks for the same
   root reason the source would be without this proposal's enabling work: there
   is no OVN-managed IP in the pod annotation. (Destination matching is built
   from an IP address set - `ip4.dst == $dest_as`, evaluated against the packet
   header - so wherever pod IPs exist it already works cluster-wide regardless
   of OVN Interconnect zone; it is not gated on port identity or zone-locality.
   The blocker on ipamless networks is purely the missing IP, not topology.) On
   top of that, much QoS-relevant traffic terminates off-cluster (external
   services, appliances, hosts on the segment) where there is no destination pod
   to select at all. `ipBlock`, in contrast, is evaluated locally against the packet
   header before egress - no destination-side cooperation or resolution
   required - so it is not a lossy substitute for a working feature; it is the
   honest, complete story for what can be matched correctly today.

2. **The use cases are source-side; the destination refinements are address
   ranges.** Workload-tier differentiation (production vs. staging), per-class
   bandwidth capping, and DSCP marking for fabric prioritization are all decided
   by *who is sending* (selected by label). Destination matching appears only as
   a secondary refinement (e.g. "deprioritize traffic *to* the backup subnet"),
   and in every such case the destination is stable, fixed-address
   infrastructure - backup servers, storage/NFS targets, monitoring collectors.
   That is the canonical `ipBlock` case ("traffic to `10.20.0.0/16`"), not a
   churning label-selected pod set. Fabric-level enforcement reinforces this:
   once a packet is marked, the physical fabric queues it by that marking
   regardless of destination.

3. **It aligns with an already-accepted platform constraint.** MultiNetworkPolicy
   already restricts ipamless networks to `ipBlock` peers, and `ipBlock` is a
   long-established, widely-used selector in Kubernetes NetworkPolicy. Scoping
   destination matching to IP/CIDR follows an existing platform norm rather than
   introducing a novel limitation.

4. **There is a clear escape hatch, so the door is not permanently closed.** A
   user who genuinely needs label-based destination selection can get pod IPs
   into the `k8s.ovn.org/pod-networks` annotation - via DHCP IPAM
   ([OKEP-6224](okep-6224-dhcp-ipam-localnet.md)) or static-IP propagation -
   which moves the network onto the standard IP-based path where destination
   `podSelector` already works. This capability is deferred
   ([Future Goals](#future-goals)), not foreclosed.

## User-Stories/Use-Cases

### Definition of personas

- **Cluster admin** - creates and manages secondary networks and cluster-wide
  policies.
- **Namespace admin** - deploys workloads and manages per-namespace QoS
  policies within a namespace.
- **VM operator** - runs virtual machines via KubeVirt on secondary networks.

### Story 1: Workload-tier differentiation (production over staging)

**As a** cluster admin,
**I want** production VM workloads to receive higher network priority than
staging workloads, even when both share the same ipamless localnet subnet,
**so that** production traffic is forwarded preferentially by the physical
fabric.

**Example:** Production and staging VMs run on the same secondary localnet UDN,
labeled `tier: production` and `tier: staging`. The admin creates two
`NetworkQoS` objects selecting each tier by `podSelector`, marking production
with a high-priority DSCP class (e.g. EF / 46) and staging with a lower class
(e.g. AF11 / 10), and uses `priority` to resolve overlaps. This is entirely a
**source-side** decision. Without ipamless support these objects are silently
ignored.

### Story 2: Intra-VM backup-traffic deprioritization

**As a** namespace admin,
**I want** in-guest backup traffic from a VM to be marked at lower priority and
optionally rate-capped, relative to the VM's regular application traffic,
**so that** bulk backup transfers do not degrade application performance on the
shared NIC.

**Example:** VMs run an in-guest backup agent that sends bulk traffic to a
backup server on a known port and/or a stable address range. Within a single
`NetworkQoS` object, an earlier egress rule marks general traffic at a high
DSCP, and a later, higher-precedence rule classifies backup traffic (by
protocol/port and/or the backup server's `ipBlock`) with a low DSCP and a
bandwidth cap. Because later rules take higher precedence, the specific backup
rule overrides the general high-DSCP rule for backup packets. The backup
destination is stable infrastructure - the canonical `ipBlock` case - not a
churning set of label-selected pods.

### Story 3: Bandwidth capping per workload class

**As a** namespace admin,
**I want** to cap egress bandwidth for a specific class of VMs (selected by
label) on an ipamless localnet network,
**so that** a batch class cannot exceed a fixed egress rate and monopolize the
shared NIC, leaving headroom for latency-sensitive workloads - even when IP
addresses are managed outside Kubernetes.

**Example:** Batch-processing VMs (`workload: batch`) are capped via a
`NetworkQoS` `bandwidth` rate, leaving the remaining capacity to
latency-sensitive VMs. This is source- and/or port-scoped, not
destination-pod-scoped.

### Story 4: DSCP marking for fabric-level prioritization on tunnel-free localnet

**As a** cluster admin,
**I want** egress traffic from selected VMs on an ipamless localnet network to
carry a DSCP marking,
**so that** QoS-aware physical switches place the traffic in the appropriate
hardware queue - without OVN-Kubernetes needing to know the VMs' IP addresses.

**Example:** VMs labeled `priority: gold` have egress traffic marked DSCP 46.
Because localnet has no tunnel, the marking is visible on the physical
interface directly (verifiable with `tcpdump` on the OVS bridge port). This
`tcpdump` check verifies only that OVN-Kubernetes applied the mark; whether the
fabric honors it (queues accordingly, or re-marks/clears it at a trust boundary)
is a separate, out-of-scope prerequisite not validated by this OKEP.

## Requirements

Derived from the user stories above and the driving user requests tracked in
the upstream enhancement issue
([#6815](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6815)):

- **R1.** Production workloads must be able to have their egress traffic
  **marked** with a higher-priority DSCP class than staging workloads, even when
  both share the same localnet subnet. (Preferential *forwarding* of that traffic
  depends on the physical fabric being configured to honor the marking - outside
  OVN-Kubernetes' control; see R5 and [Non-Goals](#non-goals).)
- **R2.** Within a single VM, in-guest backup traffic must be able to receive
  lower network priority than the VM's regular application traffic.
- **R3.** QoS must be selectable by pod/VM **labels** and must support applying
  a DSCP mark to the selected egress traffic.
- **R4.** The solution must work on **secondary OVN localnet networks with
  statically managed IPs (ipamless)**. Ipamless **layer2** is not a committed
  target: it will be reused opportunistically if the localnet mechanism applies
  with little or no additional work, otherwise pursued as a separate
  enhancement.
- **R5.** QoS must operate at **both** levels: the OVN/OVS datapath, where
  OVN-Kubernetes guarantees bandwidth policing within the cluster; and the
  physical network fabric, where OVN-Kubernetes marks DSCP on egress traffic.
  Fabric-level enforcement of that marking depends on the physical network being
  configured to honor and map DSCP, which is outside OVN-Kubernetes' control.
- **R6.** The solution must work with **KubeVirt VMs on OpenShift
  Virtualization**.
- **R7.** **No changes to the NetworkQoS CRD API.** Requirements must be met by
  controller-internal behavior alone. *This requirement is provisional: it may be
  relaxed or removed depending on how outcomes are surfaced to the user (see
  R13).*
- **R8.** Destination matching on ipamless networks must support **IP range
  (`ipBlock` / CIDR)** classification. Destination selection by `podSelector` /
  `namespaceSelector` is deliberately out of scope for this proposal (see
  [Non-Goals](#non-goals) and [Future Goals](#future-goals)), because `ipBlock`
  is sufficient for the target use cases.
- **R9.** Bandwidth policing (`rate` in kbps, `burst` in kilobits) must be
  supported on ipamless networks, to enforce per-source or per-traffic-class
  egress rate limits (Stories 2 and 3).
- **R10.** Traffic classification by protocol (TCP/UDP/SCTP) and destination
  port must be supported on ipamless networks, to differentiate traffic classes
  within a workload (Story 2).
- **R11.** Priority ordering - both across NetworkQoS objects (`priority`, 0-100)
  and within an object's ordered `egress` rule list - must be honored on ipamless
  networks, to resolve conflicts when multiple policies or rules match the same
  traffic (Story 1).
- **R12.** The solution must preserve identical behavior, performance, and OVN
  datapath constructs for NetworkQoS on **IPAM-enabled** networks; the existing
  IP-based address-set path must remain unchanged, with no regressions (Goal 4).
  This guarantee applies to objects whose selected networks are all
  IPAM-enabled. Because a single `NetworkQoS` object can select multiple networks
  (the `networkSelectors` list, each entry a label selector), an object may
  select **both** an IPAM-enabled and an ipamless network; for such
  mixed-selection objects, any R13 degradation-surfacing may become observable on
  the object. Reconciling R12's no-change guarantee with R13 for mixed-selection
  objects is part of the R13 open question (see
  [Deferred / Open Questions](#deferred--open-questions)).
- **R13.** When a `NetworkQoS` configuration relies on a capability not
  supported on the target ipamless network (notably destination `podSelector` /
  `namespaceSelector`), the outcome must be **discoverable** to the user rather
  than a silent no-op. This is consistent with the Problem Statement's objection
  to silently skipped pods: because a single namespace-scoped object can select
  multiple networks and be honored on an IPAM-enabled network while degrading on
  an ipamless one, users need to be able to tell which semantics apply where.
  *How* this is surfaced is an open question (see
  [Deferred / Open Questions](#deferred--open-questions)) to be settled in the
  Proposed Solution; this requirement fixes only that the degradation must not be
  silent.
- **R14.** The distinction between DSCP *marking* (which OVN-Kubernetes performs)
  and DSCP *enforcement* (which requires the physical fabric to be configured to
  honor the marking) must be explicit to users, so that a marked-but-unenforced
  outcome - the expected result when the fabric is unconfigured - is not mistaken
  for a broken feature. Marking is verifiable at the source (e.g. `tcpdump` on the
  OVS bridge port, per Story 4); fabric enforcement is a separate, out-of-scope
  prerequisite (see R5 and [Non-Goals](#non-goals)).

## Proposed Solution

The controller gains an **IP-independent source-matching path** that activates
only on ipamless localnet networks. Instead of resolving source pods to IP
addresses and matching `ip4.src == {$address_set}`, the controller places each
selected source pod's **logical switch port (LSP)** into a per-`NetworkQoS`
**OVN port group** and matches `inport == @<port_group> && (ip4 || ip6)`.
Everything else about a `NetworkQoS` - destination `ipBlock`, protocol/port
classifier, DSCP action, bandwidth policing, priority ordering, and the
`to-lport` QoS row attached to the network's logical switch - is reused
unchanged from the existing IPAM implementation.

The path is selected at reconcile time by a single predicate,
`isIPAMlessLocalnet()` = `TopologyType() == LocalnetTopology && !DoesNetworkRequireIPAM(NetInfo)`
(`go-controller/pkg/ovn/controller/network_qos/utils.go`). When it is false - on
every IPAM-enabled network and on non-localnet topologies - the controller takes
the existing address-set path unchanged, which is what makes the no-regression
guarantee of R12 mechanical rather than aspirational.

Why a port group and not the source MAC: the [Alternatives](#alternatives)
section gives the full comparison, but in short, `inport` (the logical switch
port a packet actually ingresses on) is assigned by OVN and cannot be spoofed
from inside the guest, whereas an `eth.src` MAC match becomes spoofable once the
localnet disable-MAC-spoofing capability lands. The `(ip4 || ip6)` qualifier is
mandatory, not cosmetic: on ipamless networks `IPMode()` is `(false, false)`, so
without the qualifier the match would also catch ARP/ND/DHCP, and a low policing
`rate` would then throttle address resolution and break connectivity on the
segment.

On the OVS datapath this produces the two intended actions on egress from the
selected source: the DSCP field is stamped on the IP header, and the configured
`rate`/`burst` policer drops excess traffic. See
[Testing Details](#testing-details) for the behavior the tests assert.

### API Details

**No NetworkQoS CRD API changes (R7).** The feature is entirely
controller-internal: the same `k8s.ovn.org/v1alpha1` CRD, the same
`podSelector` / `networkSelectors` / `priority` / `egress` fields, and the same
`classifier` semantics. There is no OpenAPI/schema diff and no new field, and
existing manifests are interpreted identically - the only difference is that a
manifest selecting an ipamless localnet network, which is silently a no-op
today, becomes functional.

The one behavioral caveat is that on ipamless networks the `classifier.to`
`podSelector` / `namespaceSelector` destination forms are not honored (see
[Implementation Details](#implementation-details), R8, and R13); this is a
behavioral scoping of an existing field on a topology where it cannot be
resolved, not an API change.

### Implementation Details

All changes are confined to
`go-controller/pkg/ovn/controller/network_qos/` plus one registration in
`go-controller/pkg/libovsdb/ops/db_object_types.go`; no code outside the
NetworkQoS controller is modified.

**Branch predicate and match generation.**

- `isIPAMlessLocalnet()` (`utils.go`) gates every new behavior.
- `generateNetworkQoSMatch(...)` (`utils.go`) takes an `ipamless bool`. When set,
  the source fragment is `inport == @<SrcPortGroupName> && (ip4 || ip6)`;
  otherwise it is the unchanged `addressSetToMatchString(...)`
  (`ip4.src == {$hash}` / `ip6.src == {$hash}`). The destination `ipBlock`/CIDR
  fragment and the protocol/port classifier are appended identically in both
  cases.

**Port-group identity and ownership.**

- `db_object_types.go` registers
  `PortGroupNetworkQoS = newObjectIDsType(portGroup, NetworkQoSOwnerType, [ObjectNameKey])`,
  so the port group participates in the same ownership/GC machinery as the QoS
  rows and address sets.
- `GetNetworkQoSPortGroupDbIDs(ns, name, controller)` (`utils.go`) yields the
  deterministic external IDs; the OVN port-group name is derived from them via
  `libovsdbutil.GetPortGroupName` and stored on the in-memory state as
  `SrcPortGroupName` (`types.go`, set by `initSourcePortGroupName`).

**Port-group lifecycle (pod path).**

- `ipamlessSourceLSPNames(pod)` (`utils.go`) maps a selected source pod to its
  LSP name(s) via the pod's own network attachment(s) (its
  `k8s.v1.cni.cncf.io/networks` spec annotation → indexed nadKeys → LSP names).
  Cross-namespace attachment is handled correctly: a pod in namespace `vms`
  attaching through a `vms2` NAD lands in the same port group as a pod attaching
  through the `vms` NAD.
- `configureSourcePodIPAMless(...)` (`types.go`) resolves each LSP name to a UUID
  via `GetLogicalSwitchPort` and adds it with `AddPortsToPortGroupOps`. When the
  LSP is not yet present in NB it returns without error (skip-and-wait) - see the
  requeue fix below.
- `ensureSourcePortGroup(...)` (`network_qos_ovnnb.go`) creates the port group if
  absent and never clobbers existing membership.
- `removePodLSPFromSource(...)` / `DeletePortsFromPortGroupOps` remove ports when
  a pod stops matching or is deleted.
- The `to-lport` QoS row is bound to the source pods' **logical switch**, shared
  with the IPAM path (`bindQoSToPodSwitch`) - the port group is only the
  source-match set, not the QoS attachment point (`Port_Group` has no
  `qos_rules` column).

**Persisted NB DB shape (the contract).** Per egress rule, exactly one
`nbdb.QoS` row:

- `Direction`: `to-lport`
- `Priority`: `getQoSRulePriority(qosPriority, ruleIndex) = 10000 + qosPriority*10 + ruleIndex`
  (`types.go`)
- `Match`: `inport == @<SrcPortGroupName> && (ip4 || ip6)` [`&& <ipBlock dst>`]
  [`&& <proto/port>`]; **no `ip4.src == {$…}` fragment**
- `Action`: `{dscp: <n>}` when DSCP is set
- `Bandwidth`: `{rate: <kbps>, burst: <kbps>}` when set; empty map when only DSCP
  is requested
- `ExternalIDs`: owner keys (`OwnerController`, `OwnerType=NetworkQoS`,
  `ObjectName=<ns>:<name>`, `RuleIndex`) plus `NetworkExternalID` on UDN

The row is added to `Logical_Switch.qos_rules`; the source port group's `Ports`
list holds the resolved source-pod LSP UUIDs.

**Deletion ordering.** On `NetworkQoS` delete, `deleteSourcePortGroup(qosName)`
(`network_qos_ovnnb.go`; owner-ID predicate mirroring `deleteAddressSet`, gated
on `isIPAMlessLocalnet()`) runs from `deleteByName` **after** the QoS rows have
been removed from every referencing switch, so no switch ever references a
deleted QoS row and no QoS row ever references a deleted port group.

**Requeue on late-arriving source pods (correctness fix).** A source pod created
*after* its selecting `NetworkQoS` - the common KubeVirt case, where VMs are
created or migrated after the policy exists - must still join the port group
without waiting for an unrelated event. Two mechanisms currently prevent this on
ipamless networks:

1. The pod-update requeue filter `onNQOSPodUpdate`
   (`network_qos_controller.go`) keys "the port is ready now" on a change in the
   pod's OVN IP count (`util.GetPodIPsOfNetwork`). On ipamless networks there is
   no OVN IP, so the delta is permanently `0 == 0` and the "LSP is ready"
   transition is swallowed.
2. `configureSourcePodIPAMless` skip-and-waits on `ErrNotFound` when the LSP is
   not yet in NB, and the pod is not re-enqueued afterward.

The recommended fix is **level-based retry (Approach A)**: treat "pod is attached to
this network per its spec annotation (present from creation) but its source LSP
is not yet in NB" as a **transient error**, so the existing rate-limited work
queue retries until the LSP lands; return `nil` (no retry) only when the pod is
genuinely not attached. The fix is scope-guarded to `isIPAMlessLocalnet()`; the
IPAM path is unchanged. An annotation-edge requeue (Approach B) is not viable
because the OVN-written `k8s.ovn.org/pod-networks` annotation is published to the
apiserver *before* the source LSP is transacted into NB, so there is no reliable
LSP-readiness pod edge to trigger on. The retry budget is comfortably
sufficient: the LSP is created by the same ovnkube-controller process
milliseconds after the annotation, well within the existing `maxRetries`
backoff. The remaining decisions this fix defers (partial resolution of a
multi-attachment pod, retry accounting, and zone transitions under Interconnect)
are tracked in [Deferred / Open Questions](#deferred--open-questions).

### Worked example

The following mirrors Stories 1 and 2 on a single ipamless localnet UDN. The NAD
is ipamless (no `subnets`) and carries a label the QoS `networkSelectors` match:

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: tenant-blue
  namespace: vms
  labels:
    nqos-network: tenant-blue          # matched by networkSelectors below
spec:
  config: |
    {
      "cniVersion": "1.0.0",
      "name": "tenant-blue",
      "type": "ovn-k8s-cni-overlay",
      "topology": "localnet",
      "physicalNetworkName": "physnet",
      "vlanID": 100,
      "netAttachDefName": "vms/tenant-blue"
    }                                    # no "subnets" => ipamless
```

**Story 1 - tier differentiation.** Two objects select each tier by label;
production is marked EF (46), staging AF11 (10), with `priority` resolving any
overlap in favour of production:

```yaml
apiVersion: k8s.ovn.org/v1alpha1
kind: NetworkQoS
metadata: { name: tier-production, namespace: vms }
spec:
  networkSelectors:
  - networkSelectionType: NetworkAttachmentDefinitions
    networkAttachmentDefinitionSelector:
      namespaceSelector: {}
      networkSelector: { matchLabels: { nqos-network: tenant-blue } }
  podSelector: { matchLabels: { tier: production } }
  priority: 60
  egress:
  - dscp: 46                            # EF
    classifier: { to: [ { ipBlock: { cidr: 0.0.0.0/0 } } ] }
---
apiVersion: k8s.ovn.org/v1alpha1
kind: NetworkQoS
metadata: { name: tier-staging, namespace: vms }
spec:
  networkSelectors:
  - networkSelectionType: NetworkAttachmentDefinitions
    networkAttachmentDefinitionSelector:
      namespaceSelector: {}
      networkSelector: { matchLabels: { nqos-network: tenant-blue } }
  podSelector: { matchLabels: { tier: staging } }
  priority: 40
  egress:
  - dscp: 10                            # AF11
    classifier: { to: [ { ipBlock: { cidr: 0.0.0.0/0 } } ] }
```

On an ipamless network these produce `to-lport` QoS rows whose **source fragment
is `inport == @<port_group>`** rather than `ip4.src == {$address_set}`:

```text
# tier-production rule 0  (spec.priority 60, rule 0) -> OVN priority 10600
match: inport == @<tier-production pg> && (ip4 || ip6) && ip4.dst == 0.0.0.0/0   action: dscp=46
# tier-staging rule 0     (spec.priority 40, rule 0) -> OVN priority 10400
match: inport == @<tier-staging pg>    && (ip4 || ip6) && ip4.dst == 0.0.0.0/0   action: dscp=10
```

**Story 2 - intra-VM backup deprioritization, and the per-stage priority
resolution.** A single object with two rules: rule 0 marks all traffic EF; rule 1
(later in the list, hence higher precedence) demotes backup traffic to CS1 (8)
and caps it. It is set at `priority: 70`, **deliberately above** `tier-production`
(60) - see below.

```yaml
apiVersion: k8s.ovn.org/v1alpha1
kind: NetworkQoS
metadata: { name: backup-deprioritization, namespace: vms }
spec:
  networkSelectors:
  - networkSelectionType: NetworkAttachmentDefinitions
    networkAttachmentDefinitionSelector:
      namespaceSelector: {}
      networkSelector: { matchLabels: { nqos-network: tenant-blue } }
  podSelector: { matchLabels: { app: payments } }
  priority: 70
  egress:
  - dscp: 46                            # rule 0: regular app traffic -> EF
    classifier: { to: [ { ipBlock: { cidr: 0.0.0.0/0 } } ] }
  - dscp: 8                             # rule 1: backup traffic -> CS1 + cap
    bandwidth: { rate: 100000, burst: 5000 }
    classifier:
      to: [ { ipBlock: { cidr: 10.20.0.0/16 } } ]
      ports: [ { protocol: TCP, port: 2049 } ]
```

A payments VM carrying both `app: payments` and `tier: production` lands in the
source set of **both** objects, so all three rules install:

| Source rule | dscp | bandwidth | dst match | OVN priority |
|---|---|---|---|---|
| `tier-production` r0 | 46 | - | `0.0.0.0/0` | 10600 |
| `backup-deprioritization` r0 | 46 | - | `0.0.0.0/0` | 10700 |
| `backup-deprioritization` r1 | 8 | 100 Mbps | `10.20.0.0/16` tcp:2049 | 10701 |

OVN resolves **per packet, per pipeline stage** - marking and metering are
independent stages, and the highest-priority matching row wins *in each stage
separately*. This is why `backup-deprioritization` must sit **above**
`tier-production` in `priority`: if it did not, the tier catch-all (10600,
dscp 46) would outrank the backup marking rule in the *marking* stage, and backup
traffic would be throttled yet still marked EF - telling the fabric to prioritize
exactly the traffic being demoted. With backup at the higher priority, backup
packets resolve to dscp 8 **and** the rate cap in both stages, while the VM's
other traffic keeps EF and stays uncapped. This per-stage behavior is a property
of NetworkQoS priorities generally, but it is easy to get wrong and is called out
here because these multi-object VM scenarios are the target use cases.

`NetworkQoS.podSelector` matches the **virt-launcher pod**, so QoS labels belong
on the `VirtualMachine`'s `spec.template.metadata.labels`; guest addressing (a
static IP or external DHCP) is configured inside the guest and is never seen by
OVN-Kubernetes.

### Testing Details

- **Unit (NB shape).** Assert the complete persisted `nbdb.QoS` row (match,
  direction, priority, action, bandwidth), port-group membership, and switch
  binding for: the happy path (DSCP + `ipBlock` + protocol/port + rate/burst);
  DSCP-only (empty `Bandwidth`); and an IPAM regression proving the address-set
  path is unchanged (still `ip4.src == {$hash}`, never `inport == @`).
- **Unit (requeue fix).** attached-but-LSP-absent → transient error → requeue;
  LSP later present → success and the port joins the group; not-attached →
  `nil`, no requeue; IPAM path unchanged.
- **E2E.** Create the `NetworkQoS`, *then* create a source pod, and assert it
  joins the port group and its egress is DSCP-marked **without** any label toggle
  or CR edit (this is the acceptance test for the requeue fix); plus
  delete-on-delete cleanup (QoS rows and port group gone, switch
  `qos_rules == []`).
- **Datapath behavior (asserted by E2E).** DSCP: packets from the selected
  source carry `tos 0xb8` (DSCP 46 / EF) while an unselected pod's packets carry
  `tos 0x0`. Policing: throughput from the capped source drops sharply below an
  uncapped baseline and stays under the configured `rate` (for a 100 Mbit `rate`
  / 5 Mbit `burst`, a reduction of roughly one to two orders of magnitude is
  representative). Cross-namespace source resolution and delete-on-delete
  teardown are also covered.
- **Testing note.** OVN QoS bandwidth is a **policer, not a shaper**: assert
  "throughput drops sharply when metered," never `throughput == rate`. Note that
  the common test image `quay.io/openshift/origin-network-tools:latest` has no
  `iperf3`, so throughput checks should use a tool present in the image (e.g.
  `socat`/`dd`).

### Documentation Details

- Add this OKEP to `mkdocs.yml` under the OKEPs navigation section.
- Document the **permanent `ipBlock`-only destination limitation** prominently -
  users coming from IPAM NetworkQoS will expect destination `podSelector` /
  `namespaceSelector` to work.
- Document the marking-vs-enforcement distinction (R14): DSCP marking is
  performed by OVN-Kubernetes and verifiable at the source (`tcpdump` on the OVS
  bridge port); fabric enforcement is a separate, out-of-scope prerequisite.
- Add user-guide manifests for the target stories (workload-tier
  differentiation and intra-VM backup deprioritization), including the KubeVirt
  `VirtualMachine` manifests and guest addressing (static IP / external DHCP). A
  representative `NetworkQoS` + NAD example, and the per-pipeline-stage priority
  resolution users must get right, are given in [Worked example](#worked-example).

## Risks, Known Limitations and Mitigations

- **No destination `podSelector` / `namespaceSelector`** on ipamless networks
  (a permanent scope boundary, not a temporary limitation). *Mitigation:* document
  prominently; R13 requires that this must not degrade silently - the discovery
  mechanism is still open (see [Deferred / Open Questions](#deferred--open-questions)).
- **Policing, not shaping.** Bursty traffic sees drops rather than smoothing, and
  goodput sits below the configured `rate`. *Mitigation:* document; this is
  inherited OVN QoS behavior, not specific to ipamless.
- **Late source-pod requeue gap.** Without the level-based retry, QoS silently
  under-applies to source pods created after the CR. *Mitigation:* Approach A
  (see [Implementation Details](#implementation-details)) closes it and is part
  of this deliverable.
- **Source-MAC spoofing.** Addressed by design: the port-group (`inport`) match
  cannot be spoofed from the guest, unlike an `eth.src` match.
- **Stale/orphan port-group GC across controller rename/restart.**
  *Mitigation:* the implementation must confirm the ownership-keyed GC (the same
  machinery as address sets) reclaims orphaned port groups; this is called out as
  an implementation checkpoint.

## OVN-Kubernetes Version Skew

This feature is targeted for the next upcoming release, **release-1.4**.

The feature uses only OVN NB constructs that NetworkQoS already depends on - the
`QoS` table, `Port_Group`, `Logical_Switch.qos_rules`, and the `inport` / `ip4`
/ `ip6` match primitives. It introduces **no new OVN feature dependency**, so
there is no `ovn-northd`/OVN version floor beyond what NetworkQoS already
requires, and no CRD version change. During implementation, confirm that no
mixed-zone ordering assumption is introduced (moot for single-zone localnet;
relevant only under OVN Interconnect - see
[Deferred / Open Questions](#deferred--open-questions)).

## Backwards Compatibility

Strictly additive. NetworkQoS on ipamless localnet networks is non-functional
today (source pods are silently skipped for lack of an IP), so enabling it
changes no existing behavior. IPAM-enabled networks are untouched: the
`isIPAMlessLocalnet()` predicate isolates every new code path, and the IPAM
address-set match is emitted exactly as before (R12). There is no migration, no
data reformat, and no change to any persisted object on existing networks; an
upgrade simply makes previously-inert manifests take effect on ipamless localnet
networks.

## Alternatives

Source-matching approaches fall into two families: **IP-independent** matching
(needs no guest IP) and **IP-dependent** matching (obtains the guest IP and
reuses the existing address-set path). Approach 1 is the preferred alternative,
offered as a recommendation for the maintainers to weigh.

**IP-independent approaches (no guest IP required).**

1. **Port group / `inport` (preferred).** Match
   `inport == @<pg> && (ip4 || ip6)`, the port group holding the source LSPs.
   Spoof-resistant (`inport` is assigned by OVN, not derivable from
   guest-controlled headers) and requires nothing from the guest. OVN honors the
   `inport` predicate in a `to-lport` QoS row on a localnet switch, applying both
   DSCP marking and policing on egress.
2. **MAC address set / `eth.src`.** Match `eth.src == {$mac_set} && (ip4 || ip6)`.
   Functionally equivalent for source matching today, but becomes spoofable once
   the localnet disable-MAC-spoofing capability lands
   ([OKEP-3926](okep-3926-disable-port-security.md)) - a guest could set its own
   source MAC to evade or impersonate a QoS class. This spoofing exposure is the
   reason it is not the preferred option.

**IP-dependent approaches (learn the guest IP, reuse the existing path).** Both
of the following obtain the guest IP and feed it to the unchanged
`ip4.src == {$address_set}` machinery. Their shared upside is that they preserve
the **full** IPAM feature set - including destination `podSelector` /
`namespaceSelector` - because they produce a real IP. They differ only in *who*
supplies the IP (the user vs. the controller).

3. **Guest IP recorded in a pod annotation (user/CNI-provided).** Have the
   user/CNI record the guest IP in an annotation and feed the existing
   `ip4.src == {$address_set}` machinery. Not preferred: it pushes IPAM
   responsibility onto the user, is fragile for VMs that change IPs at runtime,
   and still would not remove the need for a different match on truly static
   addresses.
4. **OVN-Kubernetes actively introspects the guest to learn its IP (IP
   discovery).** Rather than asking the user to supply the IP, the controller
   *discovers* it and populates the source (and, for VM destinations, the
   destination) address set, restoring the full IPAM feature set. Candidate
   discovery mechanisms:
   - **KubeVirt VMI status watching** - read guest-reported addresses from
     `VirtualMachineInstance.status.interfaces[].ipAddress` and populate the
     address set.
   - **DHCP snooping** - observe DHCP ACKs on the localnet bridge to learn the
     lease the external server hands the guest.
   - **ARP/ND learning** - passively learn the source IP from the guest's own
     ARP/NDP traffic.
   - **IP-claim CRD** - an `IPAMClaim`-style cluster object recording the guest's
     address.

   Not preferred, and captured as a [Non-Goal](#non-goals) rather than a live
   design option, for several reasons:
   - **It reintroduces the IP-management responsibility this OKEP sets out to
     avoid.** The target population is precisely VMs whose addresses are managed
     entirely outside OVN-Kubernetes; making the controller track those addresses
     re-couples QoS to an IP OVN-K neither owns nor can authoritatively validate.
   - **Learned IPs are guest-asserted, hence spoofable.** DHCP-snooped and
     ARP-learned addresses originate from the guest; matching QoS on them reopens
     the spoofing exposure the `inport` design closes by construction (a guest
     could source-spoof to change or evade its QoS class), directly conflicting
     with the localnet disable-MAC-spoofing direction
     ([OKEP-3926](okep-3926-disable-port-security.md)).
   - **It is eventually-consistent, so it recreates the silent-skip failure in a
     new form.** Addresses are learned asynchronously and can change at runtime
     (renumbering, secondary/floating IPs, failover); between a change and its
     detection QoS silently under-applies - the very behavior the Problem
     Statement objects to - and address-set churn adds reconcile load.
   - **Cost/coupling is disproportionate.** The KubeVirt-status variant adds a
     hard control-plane dependency on the KubeVirt API for a feature that must
     also serve non-VM pods and non-KubeVirt deployments; the datapath variants
     (DHCP snooping, ARP/ND learning) are substantial new datapath features, each
     larger than the entire rest of this proposal.
   - **It is not required by the target use cases.** Every source-side use case
     (Stories 1-4) is satisfied by IP-independent `inport` matching. The only
     capability introspection would unlock - destination `podSelector` on
     ipamless networks - is an explicit [Future Goal](#future-goals), and the
     OKEP prefers to reach it through a clean, authoritative IP path (DHCP IPAM
     per [OKEP-6224](okep-6224-dhcp-ipam-localnet.md), or static-IP propagation)
     rather than by inferring IPs the controller does not manage.

**Choosing between the families.** The IP-independent approaches (1, 2) need no
guest IP and - for approach 1 - are spoof-resistant, but they can express only
`ipBlock`/CIDR destinations: "QoS toward `10.20.0.0/16`", not "QoS toward pods
labeled `role: backup-target`" without manual CIDR bookkeeping. The IP-dependent
approaches (3, 4) preserve the full destination feature set but reintroduce
IP-management burden, spoofable match criteria, and eventual-consistency gaps.
This OKEP chooses the IP-independent port group (approach 1) for the committed
scope and defers destination `podSelector` to a future clean-IP path. This is
the deliberate scope trade documented in R8 and
[Scope of destination matching](#scope-of-destination-matching-in-this-proposal):
source selection stays fully label-driven; destination selection is
`ipBlock`/CIDR only.

## Deferred / Open Questions

### From 2026-08-19 review

- **R13 discovery mechanism for unsupported/degraded configs on ipamless networks** — Requirements (R13) (P1, product-lens / scope-guardian, confidence 75)

  R13 requires that a `NetworkQoS` config relying on a capability unavailable on
  an ipamless network (notably destination `podSelector` / `namespaceSelector`)
  must not degrade silently - but deliberately does not fix *how* the user is
  told. This is an open design question for the Proposed Solution, with (at
  least) three candidates:

  1. **Silent no-op** (status quo) - rejected by R13's intent; listed only as the
     baseline being ruled out.
  2. **Emit a Kubernetes event per reconcile** - low-effort; events are not part
     of the `NetworkQoS` API surface, so this is arguably the most R7-clean
     option (controller-internal behavior). Downsides: events are ephemeral and
     noisy on a hot reconcile loop, and easy to miss after the fact.
  3. **Surface a condition on the `NetworkQoS` object's status** - persistent and
     queryable. Note this is **not** free with respect to R7: although the CRD
     struct already exposes `Status.Conditions` (no OpenAPI/schema diff needed),
     introducing a *new condition type* with defined semantics is an **additive
     API change** - an externally observable contract that must be documented and
     maintained, and not "controller-internal behavior alone" as R7 requires.
     So this option is in genuine tension with R7 as currently worded.

### Carried into implementation

These are settled enough to build the feature, but each needs a decision or a
verification step before GA:

- **Partial resolution of a multi-attachment source pod.** When a source pod has
  several relevant attachments and only some LSPs exist in NB yet, should the
  controller add the resolved LSPs immediately and retry the rest, or hold the
  whole pod until all resolve? Undecided; affects the granularity of the
  level-based retry.
- **Retry accounting for the level-based requeue.** Choose the `maxRetries`
  budget and log level so that a normal transient (source LSP not yet in NB) is
  not surfaced as an error in logs or metrics.
- **Zone transitions on ipamless.** The `onNQOSPodUpdate` filter comment also
  claims to catch zone switches; confirm the level-based retry path covers this,
  or record it as a separate latent gap. Moot for single-zone localnet; relevant
  under OVN Interconnect.
- **Orphan port-group garbage collection** across controller rename/restart;
  confirm the ownership-keyed GC reclaims orphaned port groups
  (see [Risks](#risks-known-limitations-and-mitigations)).

## References

- [OKEP-4380: Network QoS Support](okep-4380-network-qos.md) - existing
  NetworkQoS implementation.
- [OKEP-6224: DHCP IPAM Support for Localnet Networks](okep-6224-dhcp-ipam-localnet.md)
  - DHCP-based IP delivery (related, not a dependency).
- [OVN NB Schema - QoS Table](https://www.ovn.org/support/dist-docs/ovn-nb.5.html)
  - QoS match expression syntax.
- Upstream tracking:
  [ovn-kubernetes#6815](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6815)
  - enhancement tracking issue for this OKEP, where the driving user requests
  and discussion are recorded.
