# OKEP-6607: Extending ClusterNetworkConnect to Support EVPN-Based CUDNs

* Issue: [#6607](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6607)

## Problem Statement

Cluster administrators who deploy EVPN-based ClusterUserDefinedNetworks (CUDNs)
cannot connect them together using
[ClusterNetworkConnect](../features/user-defined-networks/cluster-network-connect.md)
(CNC). CNC today only supports Geneve-based CUDNs — it creates an OVN
**connect-router** that links network routers via patch ports, an OVN-only
mechanism that does not participate in the EVPN control plane. Administrators
who need inter-subnet routing between EVPN CUDNs must manually manage FRR
configuration, VRF route targets, and Linux VRF plumbing with no single API to
express connectivity intent.

## Goals

* Extend CNC to be the **unified API** for expressing connectivity intent
  between CUDNs. All networks selected by a single CNC must use the
  same transport — either all Geneve or all EVPN.
* Enable **pod-to-pod connectivity** across two or more EVPN-based CUDNs
  connected via CNC, with inter-subnet L3 routing working both
  same-node and cross-node.
* When connected CUDNs share an IP-VRF, external entities that can
  reach one CUDN's subnet via the EVPN fabric can also reach the
  other connected CUDNs' subnets through the same VRF. This
  **north-south reachability** across connected subnets is an
  expected consequence of sharing an IP-VRF, not just east-west.
* Provide **ClusterIP service access** across connected EVPN CUDNs.
  NodePort and LoadBalancer services are already reachable across
  UDNs by default since they are externally exposed.
* Support **partial connectivity** (ServiceNetwork only, without
  PodNetwork) across connected EVPN CUDNs, consistent with the
  existing CNC connectivity modes.
* Support **NetworkPolicy** enforcement across
  connected EVPN CUDNs, with policy peers spanning all connected
  networks when PodNetwork is enabled.
* Support connecting CUDNs that reference **different VTEP CRs**
  within the same CNC.
* Support connecting both Layer 2 and Layer 3 EVPN CUDNs via CNC.
* Allow integration with existing EVPN fabrics by supporting
  admin-specified L3 VNI and route target values.

## Future Goals

* Connecting EVPN CUDNs across clusters via CNC (inter-cluster CNC).
* Mixed-transport CNC: connecting a Geneve CUDN to an EVPN CUDN
  within the same CNC (requires bridging the OVN connect-router with
  the EVPN shared VRF).
* Advertising ClusterIP services externally via the shared IP-VRF.
* Supporting CNC for EVPN in shared gateway mode (pending OVN EVPN
  support).
* Supporting CNC for EVPN on secondary (non-primary) UDNs. EVPN
  transport is only supported on primary networks today.

## Non-Goals

* Changing the CNC API semantics for Geneve-based CUDNs. The existing
  connect-router mechanism remains unchanged for Geneve transport.
* Supporting EVPN CNC in shared gateway mode (EVPN is local gateway
  mode only per OKEP-5088).
* Supporting EVPN CNC for secondary UDNs (CNC is primary-only today).
* Supporting overlapping subnets across connected EVPN CUDNs.

## Introduction

### Background: CNC for Geneve

[OKEP-5224](okep-5224-connecting-udns/okep-5224-connecting-udns.md) introduced
ClusterNetworkConnect as the API for connecting isolated CUDNs. For
Geneve-based networks, CNC creates an OVN **connect-router** — a distributed
logical router that links each connected CUDN's `ovn_cluster_router` (Layer 3)
or `transit_router` (Layer 2) via patch ports. Routes and policies on the
connect-router enable inter-subnet packet forwarding entirely within OVN.

### Background: EVPN Transport

[OKEP-5088](okep-5088-evpn.md) added EVPN support to OVN-Kubernetes. Each
EVPN CUDN gets:

* A **per-CUDN Linux VRF** — always created for management port isolation,
  regardless of whether `ipVRF` is configured. All CUDN interfaces live
  in this VRF.
* A **MAC-VRF** (L2 VNI) — an L2 broadcast domain stretched across nodes
  via VXLAN, implemented as an L2 SVI on the SVD (Single VXLAN Device)
  bridge, enslaved to the per-CUDN VRF.
* An optional **IP-VRF** (L3 VNI) — when `ipVRF` is configured, an L3 SVI
  is added to the same per-CUDN VRF, turning it into an EVPN IP-VRF with
  Type 5 route advertisement for inter-subnet routing.
* FRR provides the BGP/EVPN control plane, advertising Type 2 (MAC/IP)
  and Type 5 (IP prefix) routes.

A key design choice in OKEP-5088 is that **each CUDN maps 1:1 to a Linux
VRF**. This provides implicit network isolation but means connecting two
EVPN CUDNs requires either placing their interfaces into a shared VRF, or
leaking routes between their separate VRFs via shared BGP route targets.
OKEP-5088 acknowledges this gap and notes that inter-VRF route leaking
(route target importing/exporting) can be used to connect VRFs, but does
not define an API or exact implementation path for it.

### Why CNC's Connect-Router Does Not Work for EVPN

The connect-router is an OVN-only construct. EVPN CUDNs' forwarding is split
between OVN (local, intra-node) and FRR/the EVPN fabric (cross-node):

* OVN handles local switching on `br-int`.
* FRR handles inter-node forwarding via VXLAN encapsulation and BGP route
  exchange.

Connecting two EVPN CUDNs via an OVN connect-router would create a routing
path that only works within OVN's logical datapath. Packets routed through
the connect-router would never enter the EVPN fabric because the
connect-router's ports do not have corresponding VNIs, VTEPs, or FRR
BGP sessions. Cross-node traffic between connected EVPN CUDNs would
fail silently.

### Current Workarounds (without CNC for EVPN)

Administrators can already achieve inter-CUDN routing manually
through two approaches, but neither is managed by CNC:

1. **External fabric inter-VRF leaking**: The physical fabric network
   administrator configures route leaking between IP-VRFs on the
   external EVPN fabric (spine/leaf switches). This puts the onus
   on the physical network team and can also be used to connect
   CUDNs to other IP-VRFs on the external network. OVN-Kubernetes
   should not prevent this but should not rely on it either.

2. **Manual FRR-K8S configuration**: An administrator creates
   FRRConfiguration CRs (via RouteAdvertisements or directly via
   frr-k8s) to configure inter-IP-VRF route leaking at the
   Kubernetes node level. This works but becomes cumbersome when
   many CUDNs need to be interconnected — each new CUDN requires
   updating route targets across all connected VRFs.

Both approaches are manual day-2 operations. CNC should provide a
simple declarative way to express "connect these CUDNs" and take
care of the VRF configuration automatically.

### EVPN Multi-Tenancy Model

In traditional EVPN datacenter designs (see
[Cisco Programmable Fabric Multi-Tenancy](https://www.cisco.com/c/en/us/td/docs/switches/datacenter/pf/configuration/guide/b-pf-configuration/Multi-Tenancy.html)),
a **tenant** maps to a single **IP-VRF** (L3 VNI), and multiple
**MAC-VRFs** (L2 VNIs) belonging to that tenant are all attached to
the tenant's IP-VRF:

```
Tenant "PEPSI"
├── IP-VRF: PEPSI-VRF (L3 VNI 50002)
│   ├── MAC-VRF: PEPSI-DEV  (L2 VNI 20021, VLAN 21, subnet 10.2.21.0/24)
│   ├── MAC-VRF: PEPSI-QA   (L2 VNI 20022, VLAN 22, subnet 10.2.22.0/24)
│   └── MAC-VRF: PEPSI-PROD (L2 VNI 20023, VLAN 23, subnet 10.2.23.0/24)
```

All MAC-VRFs within the tenant are automatically routable via the shared
IP-VRF — pods in PEPSI-DEV can reach pods in PEPSI-QA because both subnets
exist in the same VRF routing table and FRR advertises Type 5 routes for
all of them under the same L3 VNI.

Crucially, in datacenter EVPN fabrics the same tenant VRF (L3 VNI) is
configured on **every VTEP** (leaf/ToR switch) that has hosts belonging
to that tenant. The L3 VNI is what ties the VRF together across VTEPs,
not the VTEP identity. As the
[Cisco VXLAN BGP EVPN Design Guide](https://www.cisco.com/c/en/us/products/collateral/switches/nexus-9000-series-switches/guide-c07-734107.html)
states: "All VTEPs in an EVPN must have the same Layer-3 VNI for
inter-VXLAN routing." This means multiple VTEPs naturally participate
in the same VRF — BGP route targets control which VTEPs import/export
routes for which tenants. This OKEP follows the same model: a shared
L3 VNI can span CUDNs that reference different VTEP CRs.

## User-Stories/Use-Cases

### Story 1: Connecting EVPN tenant networks

As a cluster admin, I want to connect my Finance and HR EVPN CUDNs
together so that pods in the Finance network can reach pods and
services in the HR network, while both networks remain isolated from
other tenants — without manually configuring FRR route targets or
Linux VRF plumbing.

### Story 2: Network orchestration with Plexus

As a platform operator using
[Plexus](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6557),
I want to create an AdministrativeNetworkDomain with three EVPN
subnets (web, app, db) and have them automatically connected so that
pods across subnets can communicate, just as they would in a
traditional VPC — regardless of whether the underlying transport is
Geneve or EVPN.

### Story 3: Integrating with an existing EVPN fabric

As a network engineer, I have an existing EVPN fabric where my
tenant "ACME" uses L3 VNI 50010. I want to create multiple EVPN
CUDNs in Kubernetes for ACME's workloads and connect them using
CNC, specifying L3 VNI 50010 so that the Kubernetes CUDNs integrate
into my existing tenant's VRF on the external fabric.

### Story 4: Day-2 expansion of connected EVPN networks

As a cluster admin, I already have two EVPN CUDNs connected via
CNC. I want to add a third CUDN to the same CNC so that it
automatically joins the shared IP-VRF and can communicate with
pods in the existing two networks.

## Proposed Solution

### Overview

**What a single EVPN CUDN creates today (per node):**

Each EVPN CUDN creates the following Linux constructs on every node:

* A **per-CUDN Linux VRF** (`<network>-udn-vrf`) — created by the UDN
  gateway for management port isolation. All interfaces for this CUDN
  live in this VRF.
* The **management port** (`ovn-k8s-mpX-<network>`) — the gateway
  between OVN's logical switch and the Linux networking stack. Enslaved
  to the per-CUDN VRF.
* A **MAC-VRF L2 SVI** (`br0.<vid>`) — a VLAN sub-interface on the
  SVD bridge, mapping to the CUDN's L2 VNI. Enslaved to the per-CUDN
  VRF. Provides ARP suppression and L2VNI-to-VRF association.
* *(only with `ipVRF`)* An **IP-VRF L3 SVI** (`br0.<vid>`) — a second
  VLAN sub-interface for the L3 VNI. Enslaved to the same per-CUDN
  VRF. This turns the VRF into an EVPN IP-VRF with Type 5 route
  advertisement via FRR (`vrf <name>; vni <L3-VNI>; exit-vrf`).

There is **one Linux VRF per CUDN** — the UDN VRF and the EVPN
IP-VRF are the same VRF. Without `ipVRF`, the VRF has no L3 VNI
and no Type 5 routes; with `ipVRF`, the EVPN controller adds the
L3 SVI, L3 VNI mapping, and FRR BGP configuration to it.

**The central design question** is how CNC connects these per-CUDN
VRFs so that routes from multiple subnets are visible in the same
routing table. There are four approaches under consideration,
evaluated across three starting states:

* **No `ipVRF`** — CUDNs have L2-only VRFs (management port + L2
  SVI, no L3 VNI).
* **Matching `ipVRF.vni`** — CUDNs already specify the same L3 VNI,
  signaling shared routing intent.
* **Different `ipVRF.vni`** — CUDNs have separate L3 VNIs in
  separate VRFs.

**Ordering of operations.** CNC uses label selectors to determine
which CUDNs are connected. CUDNs can be selected or unselected at
any time — by creating/deleting the CNC, creating/deleting a CUDN,
or changing labels so a CUDN matches or no longer matches the
selector. Each approach must handle all orderings and converge to
the same steady state. The approaches below detail how selection
and unselection are handled in each case.

**Should MAC-VRF-only CUDNs be eligible for CNC?** A CUDN with
only a MAC-VRF (no `ipVRF`) is explicitly L2-only — it has no
inter-subnet routing and no Type 5 routes. This could signal that
the admin intends network isolation at L3. Requiring `ipVRF` as a
prerequisite for CNC eligibility would make the admin's intent to
participate in inter-subnet routing explicit. This varies by
approach:

* Approaches B, C, and D already require `ipVRF` — MAC-VRF-only
  CUDNs are rejected.
* Approach A is the only one that works with MAC-VRF-only CUDNs
  (CNC creates the shared VRF and adds L3 capability). This is
  convenient but means CNC silently upgrades an L2-only CUDN to
  L3, which may not match the admin's original intent.

If we require `ipVRF` for CNC eligibility, all four approaches
behave consistently and the admin must explicitly opt in to L3
routing before connecting CUDNs. If we allow MAC-VRF-only CUDNs,
only Approach A can handle them.

---

#### Approach A: CNC-managed shared VRF

CNC creates and owns a shared VRF with its own L3 VNI (specified
in the CNC spec or auto-allocated). All connected CUDNs' interfaces
(management port, L2 SVI) are placed into this shared VRF. One
L3 SVI is created for the shared L3 VNI. The per-CUDN VRFs become
empty shells (the UDN gateway continues to manage them for
lifecycle, but they hold no interfaces while the CNC is active).

**CNC API change:** The CNC spec needs a new optional
`evpnConfiguration` field where the admin can specify the L3 VNI
(and optionally a route target) for the shared VRF. If omitted,
the controller auto-allocates. The specified or allocated L3 VNI
must not overlap with any existing per-CUDN `ipVRF.vni` values
(unless the intent is to reuse that VRF, which is Approach B).
CNC selection drives which CUDNs are connected — no CUDN spec
changes needed.

**Handling each starting state:**

*No `ipVRF`:* CUDNs are L2-only. CNC creates the shared VRF with
an L3 VNI, creates the L3 SVI, and moves each CUDN's management
port and L2 SVI from its per-CUDN VRF into the shared VRF.
Configures FRR for the shared VRF (`vrf <name>; vni <L3-VNI>`
and Type 5 route advertisement for all connected subnets).

*Matching `ipVRF.vni` (all CUDNs share the same VNI):* CNC creates
the shared VRF (using its own VNI or the matching one). Each CUDN's
per-CUDN L3 SVI is removed and its management port + L2 SVI are
moved into the shared VRF. The per-CUDN VRFs become empty and their
ipVRF FRR stanzas are removed; the shared VRF gets the FRR config.

*Different `ipVRF.vni`:* Same as matching — CNC creates the shared
VRF, moves all interfaces, and replaces each per-CUDN ipVRF
config. The per-CUDN L3 VNIs are freed. The admin's per-CUDN
route target choices are overridden.

**Creation ordering:**

* *CNC first:* The shared VRF exists before any CUDN. When a CUDN
  is created and matches the CNC selector, the node logic creates
  the CUDN's interfaces directly into the shared VRF instead of
  the per-CUDN VRF.
* *CUDNs first:* Each CUDN has its own VRF with interfaces. When
  CNC is created, the reconciler creates the shared VRF and moves
  each CUDN's interfaces into it.
* *Mixed:* CNC exists with some CUDNs already connected. A new
  CUDN is created — it goes directly into the shared VRF. Existing
  CUDNs were moved earlier.

```
Steady state (all starting states converge here):
┌────────── cnc-shared-vrf (L3 VNI 5000) ──────────┐
│  br0.50 (L3 SVI for VNI 5000)                    │
│  br0.12 (blue L2 SVI, VNI 100)                   │
│  br0.14 (green L2 SVI, VNI 200)                  │
│  ovn-k8s-mpX-blue                                │
│  ovn-k8s-mpX-green                               │
└──────────────────────────────────────────────────┘
blue-udn-vrf: (empty)    green-udn-vrf: (empty)
```

**Unselecting a CUDN from CNC:** The CUDN's management port and
L2 SVI are moved back to its per-CUDN VRF. If the CUDN originally
had `ipVRF`, the controller must recreate the L3 SVI on the SVD
bridge for the CUDN's original L3 VNI, re-enslave it to the
per-CUDN VRF, and restore the FRR stanza (`vrf <name>; vni
<L3-VNI>`). The CUDN spec is immutable, so the original ipVRF
config is always available for reconstruction. If the CUDN had no
`ipVRF`, it returns to L2-only connectivity — no L3 SVI to
recreate. If all CUDNs are removed, the shared VRF and its L3 VNI
are deleted.

**Deleting the CNC:** All CUDNs' interfaces are moved back and
per-CUDN configs are restored. The shared VRF is deleted.

*Pros:*

- One VRF, one routing table, one L3 VNI — simplest topology.
- Aligns with the datacenter EVPN multi-tenancy model (one tenant =
  one IP-VRF with multiple MAC-VRFs).
- Works for all starting states and orderings — universal.
- CNC owns the VRF lifecycle — clean creation and teardown.

*Cons:*

- Replaces per-CUDN ipVRF config — admin's L3 VNI and route target
  choices are overridden while CNC is active. External peers
  filtering on the old per-CUDN route target lose visibility until
  reconfigured for the shared VRF's route target.
- Moving interfaces between VRFs causes brief traffic disruption
  (milliseconds to seconds) as routes are removed from the old VRF
  and added to the new one.
- The live VRF config differs from the CUDN spec while the CNC is
  active (the spec's ipVRF is not reflected in the actual VRF).

---

#### Approach B: Idempotent VNI reuse (admin-driven)

CUDNs are created with matching `ipVRF.vni` values. The node-level
VRF creation logic is updated to be **idempotent**: when a CUDN is
created with an `ipVRF.vni` that matches another CUDN **and** both
are selected by the same CNC, the second CUDN reuses the existing
VRF instead of creating a new one. CNC selection is the
**authorization gate** — matching VNIs alone do not trigger VRF
reuse. Without a CNC selecting both CUDNs, matching VNIs are
rejected by the existing uniqueness validation to prevent
accidental sharing. If selected CUDNs have no `ipVRF` or have
different `ipVRF.vni` values, CNC rejects the configuration with
a status condition (`IPVRFMismatch`).

This approach requires relaxing the VNI uniqueness constraint from
OKEP-5088, but **only for CUDNs selected by the same CNC**. The
per-VTEP VNI uniqueness validation was added in
[PR #6065](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6065)
and must be updated. New rule: L2 VNIs (`macVRF.vni`) remain
unique per VTEP; L3 VNIs (`ipVRF.vni`) may be shared when a CNC
selects both CUDNs (same L3 VNI = same Linux VRF). This aligns
with standard EVPN multi-tenancy where multiple MAC-VRFs belong
to one IP-VRF.

**VID handling:** Each CUDN keeps its own L2 VID (different L2
VNIs = different MAC-VRFs = different VIDs). The L3 VID must be
**shared** — the SVD bridge's VID-to-VNI mapping is 1:1, so only
one VID can map to a given L3 VNI. Today the cluster manager
allocates L3 VIDs keyed by network name
(`ipVRFKey(networkName)`), so two CUDNs with the same
`ipVRF.vni` would get different L3 VIDs. This must be updated:
the L3 VID allocation should be keyed by the L3 VNI value (not
the network name), so that CUDNs sharing an L3 VNI share the
same L3 VID and the same L3 SVI.

**CNC API change:** No new fields needed. CNC selection drives
which CUDNs are authorized to share a VRF. The controller verifies
matching `ipVRF.vni` across selected CUDNs and rejects mismatches
via status conditions. The node-level logic checks CNC membership
before allowing idempotent VRF reuse.

**Handling each starting state:**

*No `ipVRF`:* **Does not work.** There is no L3 VNI to match, and
CNC cannot assign one (it doesn't manage VRFs in this approach).
CNC rejects with a status condition, or the admin must delete and
recreate CUDNs with matching `ipVRF.vni` (spec is immutable).

*Matching `ipVRF.vni`:* **Works.** CNC authorizes the VNI reuse.
CUDNs are placed into the same VRF via idempotent creation. CNC
adds service connectivity (LB cross-attachment).

*Different `ipVRF.vni`:* **Does not work.** VNIs don't match. CNC
rejects with `IPVRFMismatch`.

**Selection ordering:**

* *CNC first:* CNC records which VNI is expected. When CUDNs are
  created later with matching VNIs and match the CNC selector,
  idempotent reuse is authorized and the VRF is shared.
* *CUDNs first:* Without CNC, two CUDNs with the same
  `ipVRF.vni` cannot coexist — the VNI uniqueness validation
  rejects the conflict and both CUDNs may end up in an error
  state (upon controller restart, which CUDN "wins" the VNI
  reservation is non-deterministic — the same problem as VTEP
  conflicts). The admin must create the CNC first (or
  simultaneously) to authorize the VNI reuse before creating the
  second CUDN. This means CUDNs-first with matching VNIs
  effectively requires CNC to exist first.

```
CNC selects blue and green (both have ipVRF.vni=5000):

First CUDN (blue) creates the VRF:
┌────────── VRF (L3 VNI 5000) ─────────────────────┐
│  br0.50 (L3 SVI)                                 │
│  br0.12 (blue L2 SVI, L2 VNI 100)               │
│  ovn-k8s-mpX-blue                                │
└──────────────────────────────────────────────────┘

Second CUDN (green, same ipVRF.vni=5000, same CNC) reuses the VRF:
┌────────── VRF (L3 VNI 5000) ─────────────────────┐
│  br0.50 (L3 SVI)                                 │
│  br0.12 (blue L2 SVI, L2 VNI 100)               │
│  br0.14 (green L2 SVI, L2 VNI 200)              │
│  ovn-k8s-mpX-blue                                │
│  ovn-k8s-mpX-green                               │
└──────────────────────────────────────────────────┘
```

**Unselecting a CUDN from CNC:** Problematic. Since `ipVRF.vni`
is immutable, the unselected CUDN still has the same VNI as the
remaining CUDNs. Without CNC authorizing the sharing, VNI
uniqueness is violated and both CUDNs enter an error state. The
only resolution is to delete one of the CUDNs (disrupting
workloads) or re-select it into a CNC. This makes Approach B
effectively **one-way** — once CUDNs are connected via matching
VNIs, they cannot be cleanly disconnected without deletion.

**Deleting a CUDN:** The CUDN's interfaces are removed from the
VRF. If it was the last CUDN using that VNI, the VRF and L3 SVI
are cleaned up.

*Pros:*

- No VRF restructuring when CNC is created first — CUDNs are born
  into the shared VRF.
- Admin expresses intent explicitly via matching VNIs + CNC
  selection — clear, intentional, no accidental sharing.
- Aligns with EVPN multi-tenancy (same L3 VNI = same tenant VRF).
- CNC acts as authorization, preventing unintended VRF reuse.

*Cons:*

- Only works for matching `ipVRF.vni` — does not handle the
  no-ipVRF or different-VNI cases.
- CUDNs-first ordering requires CNC to exist first — two CUDNs
  with the same VNI cannot coexist without CNC authorization.
- Effectively **one-way**: unselecting a CUDN from CNC causes a
  VNI uniqueness violation (spec is immutable), forcing CUDN
  deletion to resolve. CUDNs cannot be cleanly disconnected.

---

#### Approach C: Direct inter-VRF route leaking

CNC keeps each CUDN's existing per-CUDN VRF and configures shared
BGP route targets to leak routes between them. Each VRF imports and
exports a CNC-managed route target in addition to its own, causing
Type 5 routes from one VRF to appear in the other's routing table.
No interfaces are moved between VRFs.

**CNC API change:** The CNC spec may need a new optional
`evpnConfiguration` field to specify the shared route target. If
omitted, the controller auto-generates one. CNC selection drives
which CUDNs participate in the route leaking — no CUDN spec
changes needed.

**Handling each starting state:**

*No `ipVRF`:* **Does not work.** Per-CUDN VRFs have no L3 VNI and
no Type 5 routes — there is nothing to leak. CNC cannot add
`ipVRF` capability (spec is immutable).

*Matching `ipVRF.vni`:* **Not applicable.** Route leaking requires
separate VRFs with different VNIs. If CUDNs have matching VNIs,
use Approach A or B instead. The route leaking approaches (C and D)
preserve the existing VNI uniqueness constraint — no relaxation
needed.

*Different `ipVRF.vni`:* **Works.** Each CUDN has its own VRF with
its own L3 VNI. CNC adds a shared route target to each VRF to
leak routes.

**Creation ordering:**

* *CNC first:* CNC records the desired shared RT. When CUDNs with
  `ipVRF` are created and match the selector, their FRR stanzas
  are augmented with the shared RT.
* *CUDNs first:* CUDNs have their own VRFs. CNC adds the shared
  RT to each VRF's FRR config.
* Ordering does not affect steady state, but CUDNs-first requires
  FRR config modification on live VRFs.

```
┌─ blue VRF (L3 VNI 101) ──┐     ┌─ green VRF (L3 VNI 201) ──┐
│  br0.12 (SVI)             │     │  br0.14 (SVI)             │
│  ovn-k8s-mpX-blue         │     │  ovn-k8s-mpX-green        │
│  RT import/export:        │ ←→  │  RT import/export:        │
│  65000:101 + 65000:9999   │     │  65000:201 + 65000:9999   │
└───────────────────────────┘     └───────────────────────────┘
  shared RT 65000:9999 leaks Type 5 routes between VRFs
```

**Removing a CUDN from CNC:** Remove the shared RT from the CUDN's
VRF FRR config. BGP withdraws the leaked routes. The CUDN's VRF
remains intact with its own L3 VNI and original RT — clean
rollback.

**Deleting the CNC:** The shared RT is removed from all connected
VRFs. Each CUDN returns to its independent routing domain. No
VRF is deleted (CNC did not create any).

*Pros:*

- No VRF restructuring — per-CUDN VRFs and their interfaces are
  untouched.
- Each CUDN keeps its own L3 VNI and route target for independent
  north-south routing.
- Clean rollback on CNC removal (FRR config change only).

*Cons:*

- Only works when CUDNs have `ipVRF` with different VNIs. Does
  not handle the no-ipVRF case.
- Routes from all connected VRFs are duplicated in every
  participating VRF's routing table — memory and FIB overhead.
- Route target management scales poorly: each CNC adds an RT to
  every connected VRF.
- FRR config changes on live VRFs cause route churn and potential
  **traffic disruption** during BGP reconvergence.
- Full mesh complexity: with N connected VRFs, each VRF imports
  routes from all others.

---

#### Approach D: Hierarchical transit VRF (route leaking via hub)

This approach combines elements of A and C: like Approach A, CNC
creates and owns a new VRF (the transit VRF) with its own L3 VNI;
like Approach C, it uses route leaking (shared route targets)
instead of moving interfaces, keeping per-CUDN VRFs intact. The
transit VRF acts as a hub — each per-CUDN VRF leaks routes to/from
the transit VRF rather than directly to each other.

**CNC API change:** Same as Approach C — an optional
`evpnConfiguration` field for the transit VRF's L3 VNI and route
target. CNC selection drives which CUDNs participate. The
controller creates the transit VRF and manages its lifecycle.

**Handling each starting state:**

*No `ipVRF`:* **Does not work.** Same as Approach C — no L3 VNI
means no Type 5 routes to leak.

*Matching `ipVRF.vni`:* **Not applicable.** Same as Approach C —
route leaking requires different VNIs. VNI uniqueness constraint
is preserved.

*Different `ipVRF.vni`:* **Works.** Each CUDN's VRF leaks routes
to/from the transit VRF via a shared RT. The transit VRF provides
a hub routing domain.

**Creation ordering:**

* *CNC first:* CNC creates the transit VRF. When CUDNs are created,
  the shared RT is added to their FRR stanzas.
* *CUDNs first:* CNC creates the transit VRF and adds the shared
  RT to each existing CUDN's VRF FRR config.
* Ordering does not affect steady state.

```
┌─ blue VRF (L3 VNI 101) ──┐     ┌─ green VRF (L3 VNI 201) ──┐
│  br0.12 (SVI)             │     │  br0.14 (SVI)             │
│  ovn-k8s-mpX-blue         │     │  ovn-k8s-mpX-green        │
│  RT import/export:        │     │  RT import/export:        │
│  65000:101 + 65000:5000   │     │  65000:201 + 65000:5000   │
└───────────────────────────┘     └───────────────────────────┘
              ▲ route leaking              ▲ route leaking
              │                            │
              └──────────┬─────────────────┘
                         │
          ┌──────────────┴────────────────┐
          │  cnc-transit VRF (L3 VNI 5000)│
          │  RT import: 65000:5000        │
          │  RT export: 65000:5000        │
          └───────────────────────────────┘
```

**Removing a CUDN from CNC:** Remove the shared RT from the CUDN's
VRF FRR config. The CUDN's VRF remains intact. Same clean rollback
as Approach C.

**Deleting the CNC:** Remove the shared RT from all connected VRFs.
Delete the transit VRF and its L3 VNI.

*Pros:*

- No VRF restructuring — per-CUDN VRFs and interfaces untouched.
- Each CUDN keeps its own L3 VNI and route target for north-south.
- Hub topology avoids direct full-mesh route target import — each
  CUDN only needs one shared RT (to/from the transit VRF).
- Clean rollback on CNC removal.

*Cons:*

- Only works when CUDNs have `ipVRF` with different VNIs.
- Adds an extra VRF and L3 VNI per CNC (the transit VRF).
- More complex FRR configuration: each CNC requires a new
  `router bgp <ASN> vrf <transit>` stanza with its own L3 VNI,
  RD, and RT, plus route-target additions in every connected
  CUDN's VRF stanza.
- Routes are still duplicated across VRFs.
- FRR config changes on live VRFs cause route churn and potential
  **traffic disruption** during BGP reconvergence — same risk as
  Approach C.
- Potential **route loop**: per-CUDN VRFs export with the shared
  RT, the transit VRF re-exports with the same RT, and per-CUDN
  VRFs may re-import their own routes. FRR should prevent this via
  RD comparison, but may require explicit import/export route-maps.

---

#### Comparison

| | No ipVRF | Matching ipVRF.vni | Different ipVRF.vni |
|---|---|---|---|
| **A: CNC shared VRF** | Works (open question) | Works (empties per-CUDN VRFs) | Works (empties per-CUDN VRFs) |
| **B: VNI reuse** | Rejects | Works (CNC authorizes) | Rejects |
| **C: Direct route leak** | Rejects | N/A (use A or B) | Works (preserves VNI uniqueness) |
| **D: Transit VRF** | Rejects | N/A (use A or B) | Works (preserves VNI uniqueness) |

Approach A is the only one that works for all starting states.
Approach B is the simplest but only covers one case and has a
one-way problem (no clean unselection). Approaches C and D only
cover the different-VNI case and trade VRF restructuring for route
duplication and FRR complexity.

**The implementation should adopt a single consistent approach**
rather than mixing approaches based on starting state — a mix
adds implementation complexity, confuses users, and makes behavior
harder to reason about.

**Reviewers**: please leave your thoughts and pick one of these
approaches. If there are other ideas not considered here that you
prefer, please suggest them.

---

**CUDNs with `ipVRF` not in any CNC**

No change. CUDNs that are not selected by any CNC retain their
per-CUDN IP-VRF and work exactly as they do today per OKEP-5088.

---

**Multi-VTEP CNC (open design question)**

When connected CUDNs reference different VTEP CRs, each VTEP maps
to a separate SVD bridge (`br0`, `br1`, etc.) with its own VXLAN
device (`vxlan0`, `vxlan1`) and its own VTEP source IP. The shared
VRF can attach interfaces from multiple bridges — a Linux VRF is
just a routing table and does not restrict which bridge its member
interfaces belong to. MAC-VRF SVIs and `ovn-k8s-mpX` interfaces
from different bridges can coexist in the same VRF, and same-node
traffic routes locally without VXLAN.

```
Example: blue on VTEP-A (br0), green on VTEP-B (br1)

┌────────── shared VRF (L3 VNI 5000) ─────────────────────────┐
│  br0.50 (L3 SVI on VTEP-A's bridge, VNI 5000 via vxlan0)    │
│  br1.50 (L3 SVI on VTEP-B's bridge, VNI 5000 via vxlan1)    │
│  br0.12 (blue MAC-VRF SVI from VTEP-A)                      │
│  br1.14 (green MAC-VRF SVI from VTEP-B)                     │
│  ovn-k8s-mpX-blue                                           │
│  ovn-k8s-mpX-green                                          │
└─────────────────────────────────────────────────────────────┘
```

The open question is **cross-node inter-subnet routing** with the
shared L3 VNI on two bridges. In datacenter EVPN fabrics, each
leaf switch has one VTEP, so the same L3 VNI naturally exists on
one bridge per switch (see [Cisco VXLAN BGP EVPN Design Guide](https://www.cisco.com/c/en/us/products/collateral/switches/nexus-9000-series-switches/guide-c07-734107.html):
"All VTEPs in an EVPN must have the same Layer-3 VNI for
inter-VXLAN routing"). Having multiple VTEPs on the same node
is specific to OVN-Kubernetes and creates a non-standard
configuration:

* The same L3 VNI (5000) is mapped on both `vxlan0/br0` and
  `vxlan1/br1`. Since these are separate bridges with separate
  VXLAN devices and separate source IPs, the VNI filter mappings
  do not conflict at the bridge level.
* When FRR installs a Type 5 route in the VRF with a remote VTEP
  IP as next-hop, the kernel must resolve which VXLAN device to
  use for encapsulation. Since each VXLAN device has a different
  local VTEP IP, the kernel should be able to select the correct
  device based on FDB entries and underlay routing.
* However, FRR's SVD model assumes one bridge and one VXLAN device
  per node. Whether FRR and the kernel's symmetric IRB correctly
  handle dual L3 SVIs (from different bridges) in the same VRF
  needs validation.

This requires testing with a multi-VTEP setup to confirm that
cross-node Type 5 routing resolves correctly through the right
VXLAN device.

---

For all approaches, **ClusterIP service access** uses the same OVN
LB cross-attachment mechanism from Geneve CNC: each connected
network's OVN load balancers are added to every other connected
network's logical switches. After DNAT on the switch, the traffic
follows the inter-subnet routing path provided by the shared or
leaked VRF.

**NetworkPolicy** enforcement works the same as in Geneve CNC (see
[OKEP-5224 Network Policies](okep-5224-connecting-udns/okep-5224-connecting-udns.md#network-policies-and-admin-network-policies)):
when `PodNetwork` connectivity is requested, policy peers span
across all connected networks; when it is not, policies do not
apply across networks. No EVPN-specific changes are needed — OVN
ACLs operate on the logical switch level, which is independent of
the underlay transport.

STOP REVIEWS HERE. DON'T GO FURTHER TILL WE AGREE ON THE APPROACH.

### API Details

**`connectSubnets` handling:** The existing CNC spec requires
`connectSubnets` to provide IPs for the OVN connect-router's patch
ports in Geneve mode. For EVPN CNC, routing is handled by the Linux
shared VRF, not OVN patch ports, so `connectSubnets` is not needed.
When all selected CUDNs are EVPN type, `connectSubnets` should be
made optional. This is a backwards-compatible API change since CNC
for EVPN CUDNs is not supported today.

TODO: Remaining API details will be filled in after design is finalized.

### Implementation Details

TODO: Will be filled in after design is finalized.

### Testing Details

TODO: Will be filled in after design is finalized.

### Documentation Details

TODO: Will be filled in after design is finalized.

## Performance and Scale

TODO: Will be filled in after design is finalized.

## Risks, Known Limitations and Mitigations

### VRF Restructuring Disruption (Approach A)

Moving interfaces between VRFs causes a brief connectivity disruption
(milliseconds to seconds) as routes are removed from the old VRF and
added to the new one. This affects:
* Day-0: CNC creation when CUDNs already have running pods.
* Day-2: Adding/removing CUDNs from CNC.

**Mitigation**: The controller sequences the restructuring to minimize
disruption — new VRF is created and configured before interfaces are
moved. FRR is notified after all interfaces are in place. Documentation
recommends creating CNC before deploying workloads when possible.
Approach B avoids this entirely when CUDNs have matching VNIs.

### Route Churn During BGP Reconvergence (Approaches C and D)

Modifying FRR config on live VRFs to add/remove shared route targets
causes route churn and potential traffic disruption during BGP
reconvergence. This affects CNC creation, CUDN addition/removal, and
CNC deletion.

**Mitigation**: FRR's graceful restart and route dampening reduce the
impact. The controller batches FRR config changes to minimize churn.

### External Peer Route Target Awareness

In Approach A, CNC creates a shared VRF with a new route target.
External peers that were configured for per-CUDN route targets must
be updated. In Approaches C/D, the shared route target introduces
additional routes into per-CUDN VRFs that external peers may not
expect.

**Mitigation**: The CNC status should expose the shared route target
so admins can configure external peers accordingly. For
Plexus-managed ANDs, the controller handles this automatically.

### North-South Route Advertisement Scope

The shared VRF's `advertise ipv4 unicast` causes all connected
subnets' Type 5 routes to be advertised to all BGP peers, including
external routers. For private subnets that should only be routable
within the cluster, this is undesirable.

**Mitigation**: This is tracked separately in
[#6631](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6631).
Until that issue is resolved, admins must configure route-maps on
external peers to filter private subnet prefixes. The shared VRF's
route target can be used as a filter criterion.

### VLAN ID Exhaustion on SVD Bridge

Each CNC shared VRF (Approach A) or transit VRF (Approach D) consumes
one VLAN ID on the SVD bridge. Combined with per-CUDN MAC-VRF and
IP-VRF VLANs, the total is limited to 4094.

**Mitigation**: At practical scale (tens to low hundreds of CUDNs
and CNCs), VLAN exhaustion is unlikely. If needed, the SVD bridge
can be extended with multiple bridge/VTEP pairs (per OKEP-5088
discussion).

### Local Gateway Mode Only

EVPN CNC is limited to local gateway mode. Shared gateway mode is not
supported because EVPN requires Linux kernel networking primitives that
shared gateway mode bypasses.

**Mitigation**: This limitation is inherent to EVPN in OVN-Kubernetes
(per OKEP-5088) and not specific to CNC.

## OVN-Kubernetes Version Skew

This feature targets **v1.4.0** (aligned with the Plexus milestone).
It requires:
* EVPN support (OKEP-5088, available since v1.2.0)
* CNC support (OKEP-5224, available since v1.2.0)
* FRR-K8S API extensions for shared VRF configuration

During rolling upgrades where some nodes run v1.3.x and others run
v1.4.0, the shared VRF is only configured on v1.4.0 nodes. Pods on
v1.3.x nodes in connected CUDNs retain their per-CUDN VRFs and cannot
route to other connected subnets until the upgrade completes. The CNC
status reflects this with per-zone conditions showing which nodes have
completed the shared VRF setup.

## Backwards Compatibility

* **Geneve CNC**: No changes. Existing CNC CRs that select Geneve
  CUDNs continue to use the connect-router mechanism.
* **EVPN CUDNs without CNC**: No changes. CUDNs with their own ipVRF
  configuration continue to work as before. CNC is opt-in.
* **CNC API**: Any new fields added to the CNC spec for EVPN
  configuration will be optional. Existing CNC CRs continue to work.
  When the controller detects EVPN CUDNs, it auto-detects the
  transport and applies the appropriate path (connect-router for
  Geneve, shared VRF for EVPN).
* **E2E tests**: Existing CNC E2E tests for Geneve must continue to
  pass. New tests are added for EVPN CNC.

## Alternatives

### Alternative 1: OVN Connect-Router with VXLAN Bridging

Extend the existing OVN connect-router to also program Linux/FRR
constructs — bridge the connect-router's logical ports to the EVPN
fabric by creating VXLAN tunnels for each connect-router port.

**Why rejected:** This would require teaching OVN about EVPN, which
is a much larger change than using FRR's native VRF capabilities.
It would also create a hybrid routing path where some hops are in OVN
and others are in FRR, making debugging difficult. The shared VRF
approach keeps EVPN routing entirely in FRR/Linux where it naturally
belongs.

### Alternative 2: Explicit Tenant/Routing-Domain CRD

Instead of extending CNC, introduce a new "RoutingDomain" or "Tenant"
CRD that multiple CUDNs attach to. The RoutingDomain would own the
shared IP-VRF configuration.

**Why rejected:** This introduces a new API when CNC already expresses
the same intent ("connect these networks"). Adding another CRD creates
confusion about when to use CNC vs RoutingDomain. CNC's existing
semantics — "these networks should be connected" — map directly to
"these networks share an IP-VRF" for EVPN. Keeping CNC as the unified
API is simpler for users and aligns with CNC's design goal of being
transport-agnostic.

## References

* [#6607: Extend CNC to support EVPN-based UDNs](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6607)
* [#6631: RouteAdvertisements: support ipVRF without external advertisement](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6631)
* [#6557: OKEP: Plexus - Network Orchestration](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6557)
* [OKEP-5088: EVPN Support](okep-5088-evpn.md)
* [OKEP-5224: Connecting UserDefinedNetworks](okep-5224-connecting-udns/okep-5224-connecting-udns.md)
* [OKEP-5296: BGP Integration](okep-5296-bgp.md)
* [Cisco Programmable Fabric Multi-Tenancy](https://www.cisco.com/c/en/us/td/docs/switches/datacenter/pf/configuration/guide/b-pf-configuration/Multi-Tenancy.html)
* [FRR EVPN Configuration Guide](https://docs.frrouting.org/en/latest/evpn.html)
