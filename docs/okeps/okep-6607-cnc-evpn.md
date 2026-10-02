# OKEP-6607: Extending ClusterNetworkConnect to Support EVPN-type CUDNs

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
* Connecting CUDNs via CNC exposes their subnets **north-south** to the
  EVPN fabric, not just east-west. External peers reach the CNC's
  MAC-VRF-only subnets via the parent VRF's route target, and each `ipVRF`
  child's subnet via that child's own route target. This north-south
  reachability across connected subnets is an expected consequence of
  participating in EVPN.
* A connected CUDN gains **east-west** reachability to its siblings' subnets
  but does **not** inherit a sibling's **north-south** reachability: if one
  connected CUDN can reach an external destination, its siblings do not gain
  that path by being in the same CNC. Only the connected CUDN subnets are
  shared between children — never a child's learned external routes.
* Provide **ClusterIP service access** across connected EVPN CUDNs.
  NodePort and LoadBalancer services are already reachable across
  UDNs by default since they are externally exposed.
* Support **partial connectivity** (ServiceNetwork only, without
  PodNetwork) across connected EVPN CUDNs, consistent with the
  existing CNC connectivity modes.
* Support **NetworkPolicy** enforcement across
  connected EVPN CUDNs, with policy peers spanning all connected
  networks when PodNetwork is enabled.
* Support connecting both Layer 2 and Layer 3 EVPN CUDNs via CNC —
  MAC-VRF-only CUDNs (no `ipVRF`) and CUDNs with `ipVRF` are both
  eligible.
* Support connecting CUDNs that reference **different VTEP CRs**
  within the same CNC.
* Allow integration with existing EVPN fabrics by supporting
  admin-specified L3 VNI and route target values on the CNC.

## Future Goals

* **Connecting EVPN CUDNs across clusters via CNC (inter-cluster CNC).**
  EVPN already provides cross-cluster reachability in principle — the
  parent VRF's Type 5 routes are visible to every BGP peer on the shared
  fabric, so a CUDN in one cluster can already be reached from another that
  imports the right route target. The missing piece CNC would own is
  *intent and identity across cluster boundaries*: each cluster has its own
  VTEP CR/API and its own VNI/VID allocations, so an inter-cluster CNC needs
  the parent-VRF L3 VNI and route target to be reconciled across clusters
  rather than auto-allocated per cluster, plus agreement on which VTEP each
  side originates from. A higher-level orchestrator such as
  [Plexus](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6557) is
  the natural owner of this cross-cluster allocation — assigning a common L3
  VNI and route target to a CNC that spans clusters — so the per-cluster CNC
  controllers only consume the values Plexus hands them. This is why
  inter-cluster CNC is a future goal and not just a consequence of the
  day-one design.
* Mixed-transport CNC: connecting a Geneve CUDN to an EVPN CUDN
  within the same CNC (requires bridging the OVN connect-router with
  the EVPN parent VRF).
* Supporting CNC for EVPN in shared gateway mode, once
  [OVN-native EVPN](https://github.com/dceara/ovn-kubernetes/commits/FDP-3311-ovnk-evpn/)
  lands. Because OVN-native EVPN keeps L3 routing inside OVN, the CNC
  transit-router design applies directly (much like the Geneve
  connect-router) — the parent VRF becomes an OVN transit logical router
  carrying the parent L3 VNI and route target, so no CNC API change is
  expected.
* Supporting CNC for EVPN on secondary (non-primary) UDNs. EVPN
  transport is only supported on primary networks today.
* Supporting EVPN CNC over IPv6 VTEPs. EVPN VTEPs accept only IPv4
  underlay addresses today (per OKEP-5088), because FRR does not yet
  support IPv6 VTEPs for EVPN. CNC inherits this constraint — the parent
  VRF's Type 5 next-hops resolve over the same IPv4 VTEP underlay — and
  will support IPv6 VTEPs once the underlying EVPN/FRR support lands.
  (Note this is about the VTEP *underlay* address family; dual-stack pod
  subnets carried as Type 5 prefixes are unaffected.)

## Non-Goals

* Changing the CNC API semantics for Geneve-based CUDNs. The existing
  connect-router mechanism remains unchanged for Geneve transport.
* Supporting EVPN CNC in shared gateway mode (EVPN is local gateway
  mode only per OKEP-5088).
* Supporting EVPN CNC for secondary UDNs (CNC is primary-only today).
* Supporting overlapping subnets across connected EVPN CUDNs.
* Interaction with EgressIP, EgressService, Multiple External Gateways
  (MEG), or IPSec — these features are not supported on EVPN CUDNs
  per OKEP-5088.

## Introduction

### Background: CNC for Geneve

[OKEP-5224](okep-5224-connecting-udns/okep-5224-connecting-udns.md) introduced
ClusterNetworkConnect as the API for connecting isolated CUDNs. For
Geneve-based networks, CNC creates an OVN **connect-router** — a distributed
transit router that links each connected CUDN's `ovn_cluster_router` (Layer 3)
or `transit_router` (Layer 2) via patch ports. Routes and policies on the
connect-router enable inter-subnet packet forwarding entirely within OVN.

### Background: EVPN Transport

[OKEP-5088](okep-5088-evpn.md) added EVPN support to OVN-Kubernetes. Each
EVPN CUDN gets:

* A **per-CUDN Linux VRF** — always created for management port isolation,
  regardless of whether `ipVRF` is configured. All CUDN interfaces live
  in this VRF. Named after the CUDN if it fits in 15 chars (e.g.
  `evpn-l3-alpha`), otherwise ID-derived as `mp<networkID>-udn-vrf`
  (e.g. `mp3-udn-vrf`).
* The **management port** (`ovn-k8s-mp<N>`) — the gateway between OVN's
  logical switch and the Linux networking stack. Enslaved to the per-CUDN VRF.
* A **MAC-VRF L2 SVI** (`svl2.<networkID>`, e.g. `svl2.3`) — a VLAN
  sub-interface on the SVD bridge, mapping to the CUDN's L2 VNI.
  Enslaved to the per-CUDN VRF. Provides ARP suppression and
  L2VNI-to-VRF association. Required for Layer2 topology, forbidden
  for Layer3.
* An **IP-VRF L3 SVI** (`svl3.<networkID>`, e.g. `svl3.1`) — a second
  VLAN sub-interface on the SVD bridge for the L3 VNI. Enslaved to the
  same per-CUDN VRF. This turns the VRF into an EVPN IP-VRF with Type 5
  route advertisement for inter-subnet routing via FRR
  (`vrf <name>; vni <L3-VNI>; exit-vrf`). Required for Layer3 topology,
  optional for Layer2.
* The SVD bridge itself is named `evbr-<vtep>` (e.g. `evbr-demo-vtep`), with
  VXLAN devices `evx4-<vtep>` / `evx6-<vtep>` (e.g. `evx4-demo-vtep`).
* FRR provides the BGP/EVPN control plane, advertising Type 2 (MAC/IP)
  and Type 5 (IP prefix) routes.

Example — Layer2 CUDN `evpn-l2l3-gamma` with MAC-VRF (VNI 300) and IP-VRF
(VNI 301) on VTEP `demo-vtep`:

```
evpn-l2l3-gamma (Linux VRF, table 1013)
├── ovn-k8s-mp3     (management port)
├── svl2.3          (L2 SVI, VLAN 4 on evbr-demo-vtep, VNI 300)
└── svl3.3          (L3 SVI, VLAN 5 on evbr-demo-vtep, VNI 301)

evbr-demo-vtep      (SVD bridge)
├── evx4-demo-vtep  (VXLAN device, IPv4 underlay)
├── svl2.3          (L2 SVI, VID 4 → VNI 300)
└── svl3.3          (L3 SVI, VID 5 → VNI 301)
```

Example — Layer3 CUDN `evpn-l3-alpha` with IP-VRF only (VNI 100) on VTEP
`demo-vtep`:

```
evpn-l3-alpha (Linux VRF, table 1022)
├── ovn-k8s-mp1     (management port)
└── svl3.1          (L3 SVI, VLAN 2 on evbr-demo-vtep, VNI 100)
    No svl2.1 — macVRF forbidden for Layer3 topology
```

A key design choice in OKEP-5088 is that **each CUDN maps 1:1 to a Linux
VRF**. This provides implicit network isolation but means connecting two
EVPN CUDNs requires either placing their interfaces into a shared VRF, or
leaking routes between their separate VRFs. OKEP-5088 acknowledges this
gap and notes that inter-VRF route leaking can be used to connect VRFs,
but does not define an API or exact implementation path for it.

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
routes for which tenants. This OKEP follows the same model: the CNC's
parent VRF L3 VNI can span CUDNs that reference different VTEP CRs.

When a tenant owns multiple networks, **CNC can serve as the tenant
boundary** — connecting CUDNs under a single CNC maps to placing
their subnets under a shared tenant IP-VRF.

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
traditional VPC.

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

### Story 5: Service-only connectivity between EVPN networks

As a cluster admin, I want two EVPN CUDNs to share ClusterIP
services without allowing direct pod-to-pod communication — the
same partial connectivity model that Geneve CNC supports via
`ServiceNetwork` without `PodNetwork`.

## Proposed Solution

### Overview

CNC creates a **parent VRF** per CNC that acts as the L3 routing hub
for all connected EVPN CUDNs. The parent VRF has its own L3 VNI
(admin-specified in the CNC spec), an auto-allocated VID, an L3 SVI
on the SVD bridge, and an FRR BGP stanza. It does **not** re-originate
everything under its own VNI: `ipVRF` children self-originate their own
subnets as Type 5 under their **own** L3 VNI/RT, and the parent advertises
only the MAC-VRF-only children's subnets — for which it is the IP-VRF —
under the parent VNI/RT. Its advertise route-map denies the `ipVRF`
children's prefixes, because re-originating them on every node would create
a spurious per-VTEP ECMP set that blackholes cross-node traffic (see
[Same-network vs cross-network traffic separation](#same-network-vs-cross-network-traffic-separation)).

Because the parent VRF participates in the EVPN fabric, connectivity is not
only east-west between CUDNs — the connected subnets are also visible
north-south to BGP peers, including external routers. An external peer
imports the **parent's RT** to reach the CNC's MAC-VRF-only subnets and each
`ipVRF` **child's own RT** to reach that child's subnet; a peer that wants
the full connected set imports every participating RT. This north-south
reachability is an inherent consequence of participating in EVPN; for
subnets that should remain cluster-internal, see
[North-South Route Advertisement Scope](#north-south-route-advertisement-scope).

There is a deliberate asymmetry here. Connecting CUDNs gives the children
**east-west** reachability to each other's subnets, and exposes each subnet
**north-south** to the fabric — but a child does **not** inherit a sibling's
north-south reachability. If `alpha` has a route to an external destination X,
`beta` does not gain a path to X by being in the same CNC. The parent imports
each child through a `CNC-<PARENT>-IMPORT` route-map scoped to the connected
CUDN subnets, so only those subnets — never a child's learned external
routes — enter the parent's table and become reachable to siblings (see
[Cluster Manager](#cluster-manager) and
[Alternative 9](#alternative-9-static-routes-instead-of-import-vrf)).

Inter-VRF routing is **hub-and-spoke** and is split between FRR and the
node datapath:

* The **parent VRF imports each `ipVRF` child** via FRR's `import vrf` (a
  local operation that copies the child's routes into the parent's table),
  filtered by an import route-map so the parent only absorbs the children's
  CUDN subnets — not any external routes a child may have learned. A
  **MAC-VRF-only child is not imported**; instead its L2 SVI is enslaved to
  the parent (the parent is its IP-VRF), so its subnet is a connected route
  in the parent table. Either way the parent ends up with a table holding
  every connected subnet: local children directly, remote pods via the EVPN
  routes each VRF already learns.
* **Children do NOT import the parent.** Instead, ovnkube-node programs a
  low-metric **override route** in each child VRF's table for every
  *sibling* subnet, pointing at the parent VRF device. Sibling-destined
  traffic is re-looked-up in the parent VRF (Linux VRF leaking via a
  device route), which resolves the correct next-hop. A more-specific
  per-pod `/32` learned via symmetric IRB still wins over the override.

Importing the parent into the children was deliberately avoided: it leaks
the parent's sibling aggregates into the child tables via the overlay
next-hop, which blackholes same-node traffic and out-specifies the node's
override on longest-prefix match (see
[Alternative 5](#alternative-5-direct-import-vrf-between-children-no-parent-vrf)
and [Alternative 9](#alternative-9-static-routes-instead-of-import-vrf)).
No BGP route target manipulation on the wire, no interface movement, no
downtime.

The parent VRF consumes one additional L3 VNI and one VID per CNC — this
overhead is acknowledged. It is the cost of a consistent design that
supports both L2-only and L3 CUDNs without conditional logic, avoids
requiring each MAC-VRF-only child to carry its own L3 VNI (which would
be N VNIs instead of one), and provides a stable topology that does not
restructure as networks join and leave a CNC. Several alternative approaches
were evaluated and rejected — see [Alternatives](#alternatives) for the
full list and rationale.

Three connected CUDNs from the validated cluster, joined by one CNC whose
parent VRF is `cnc.f61ee1a4` (L3 VNI 400, table 1000006):

```
  evpn-l3-alpha    ipVRF (Layer3)     L3 VNI 100   10.10.0.0/16   child table 1022
  evpn-l2-beta     MAC-VRF-only (L2)  L2 VNI 200   10.20.0.0/16   child table 1011
  evpn-l2l3-gamma  ipVRF (L2+L3)      L3 VNI 301   10.30.0.0/16   child table 1013

Each ipVRF child (alpha, gamma) self-originates its own subnet as Type 5 under
its own L3 VNI/RT. The MAC-VRF-only child (beta) has no L3 VNI of its own — the
parent is its IP-VRF.

Node-programmed override routes (metric 5) in every child VRF table steer each
sibling subnet into the parent VRF device:

  # child table 1022 (evpn-l3-alpha)
  10.20.0.0/16 dev cnc.f61ee1a4 metric 5   # -> beta
  10.30.0.0/16 dev cnc.f61ee1a4 metric 5   # -> gamma
          │
          ▼  re-lookup in the parent VRF
┌── parent: cnc.f61ee1a4  (L3 VNI 400, table 1000006) ──────────────────────────────┐
│ FRR: import vrf evpn-l3-alpha, import vrf evpn-l2l3-gamma  (ipVRF children, via    │
│      CNC-…-IMPORT route-map: connected CUDN subnets only)                          │
│      evpn-l2-beta NOT imported — its L2 SVI is enslaved here, so 10.20.0.0/16 is   │
│      a connected route in this table (parent is beta's IP-VRF)                     │
│ FRR: advertise ipv4 unicast route-map CNC-…-ADVERTISE                              │
│        deny  10.10.0.0/16, 10.30.0.0/16  (alpha, gamma self-originate own VNI)     │
│        permit 10.20.0.0/16               (beta; parent originates under VNI 400)   │
│ node: 10.20.0.0/16 dev evpn-l2-beta metric 5  (reinject; MAC-VRF-only delivery)    │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

Cross-network traffic to an **ipVRF child** (alpha or gamma) is encapsulated
with that child's **own L3 VNI** (100 or 301) — the parent imports the child's
routes but does not re-originate them, so the destination node decapsulates
straight into the child's VRF. Traffic to the **MAC-VRF-only child** (beta)
uses the **parent's L3 VNI 400** (the parent is beta's IP-VRF); the destination
node decapsulates into the parent VRF and the reinject route delivers to beta's
pods.

**Key properties:**

* **No interface movement** — all interfaces (management ports, L2 SVIs,
  L3 SVIs) remain in their per-CUDN VRFs. Zero downtime.
* **Children keep their own EVPN identity** — an `ipVRF` child continues to
  self-originate its subnets as Type 5 under its own L3 VNI and RT; the
  parent imports those routes locally but does not re-advertise them
  (preventing the multi-VTEP ECMP blackhole — see
  [Cluster Manager](#cluster-manager)). MAC-VRF-only children have no L3
  VNI, so the parent is their IP-VRF and advertises their subnets under the
  parent's VNI/RT. External peers see no change to per-CUDN routing.
* **Works for all CUDN types** — MAC-VRF-only CUDNs (no `ipVRF`) and CUDNs
  with `ipVRF` are both eligible. The parent VRF provides the L3 VNI that
  MAC-VRF-only children lack, so no child needs its own L3 VNI just for CNC
  connectivity.
* **Clean rollback** — removing a CUDN from CNC = drop its `import vrf` line
  from the parent and remove the node's override/reinject routes for it.
  Deleting the CNC = remove all imports, delete the parent VRF, L3 SVI, and
  VID. Children are untouched.
* **Consistent behavior** — the parent VRF is always created with the CNC.
  No conditional logic based on whether children have `ipVRF` or not.

### API Details

#### CNC spec changes

A new optional `evpnConfiguration` field is added to the CNC spec. Setting
it declares the CNC's intent to connect EVPN CUDNs: when `evpnConfiguration`
is present, all selected CUDNs must be EVPN type, and the field configures
the shared parent VRF. All diffs below are against master.

New spec-level CEL rules are added, plus the new `evpnConfiguration`
field with its own immutability rule
(`go-controller/pkg/crd/clusternetworkconnect/v1/types.go`):

```diff
 // ClusterNetworkConnectSpec defines the desired state of ClusterNetworkConnect.
 // +kubebuilder:validation:XValidation:rule="!self.networkSelectors.exists(i, ...)",message="Only ClusterUserDefinedNetworks or PrimaryUserDefinedNetworks can be selected"
+// +kubebuilder:validation:XValidation:rule="has(self.connectSubnets) || has(self.evpnConfiguration)",message="either connectSubnets or evpnConfiguration must be specified"
+// +kubebuilder:validation:XValidation:rule="!has(self.connectSubnets) || !has(self.evpnConfiguration)",message="connectSubnets and evpnConfiguration are mutually exclusive"
+// +kubebuilder:validation:XValidation:rule="has(oldSelf.evpnConfiguration) == has(self.evpnConfiguration)",message="transport is immutable: evpnConfiguration cannot be added or removed after creation"
 type ClusterNetworkConnectSpec struct {
     // ... networkSelectors, connectSubnets (loosened below), connectivity ...
+
+    // evpnConfiguration configures the parent VRF for connecting EVPN-based CUDNs.
+    // Required when the selected CUDNs use EVPN transport.
+    // Forbidden when selected CUDNs use Geneve transport (use connectSubnets instead).
+    // +optional
+    // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="evpnConfiguration is immutable"
+    EVPNConfiguration *EVPNCNCConfig `json:"evpnConfiguration,omitempty"`
 }
```

`EVPNCNCConfig` is a brand-new type:

```diff
+// EVPNCNCConfig configures the parent VRF created by a CNC to connect EVPN-based CUDNs.
+// The parent VRF acts as the L3 routing hub: it owns a single L3 VNI and imports routes
+// from each connected CUDN's per-CUDN VRF via FRR's `import vrf` directive.
+type EVPNCNCConfig struct {
+    // ipVRF configures the parent VRF's L3 VNI and optional route target.
+    // The VNI must be unique across all EVPN VNIs in the cluster (both CUDN VNIs and
+    // other CNC parent VRF VNIs). If routeTarget is not specified, it is auto-generated
+    // as "<ASN>:<VNI>".
+    // +kubebuilder:validation:Required
+    // +required
+    IPVRF types.VRFConfig `json:"ipVRF"`
+}
```

`EVPNCNCConfig` reuses the shared `VRFConfig` (`VNI` + optional `RouteTarget`) —
the same type the CUDN EVPN API uses. To share it, `VRFConfig` and
`RouteTargetString` are **relocated** (not modified) from the CUDN-only package
into `crd/types`, so both APIs import one definition:

```diff
-// go-controller/pkg/crd/userdefinednetwork/v1/evpn.go   (CUDN-only)
+// go-controller/pkg/crd/types/evpn.go                   (shared by CUDN + CNC)
 type VRFConfig struct {
     VNI         int32             `json:"vni"`                    // 24-bit, 1–16777215, required
     RouteTarget RouteTargetString `json:"routeTarget,omitempty"` // optional, auto "<ASN>:<VNI>"
 }
```

Example CNC with EVPN configuration (the validated PoC's `evpn-connect-all`):

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: evpn-connect-all
spec:
  networkSelectors:
    - networkSelectionType: ClusterUserDefinedNetworks
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            evpn-group: evpn-demo
  connectivity:
    - PodNetwork
    - ServiceNetwork
  evpnConfiguration:
    ipVRF:
      vni: 400
      routeTarget: "65000:400"   # optional; auto-derived as "<ASN>:<VNI>" if omitted
```

#### `connectSubnets` handling

The existing CNC spec requires `connectSubnets` (currently `+required`,
`MinItems=1`) to provide IPs for the OVN connect-router's patch ports in
Geneve mode. For EVPN CNC, routing is handled by the parent VRF, not OVN
patch ports, so `connectSubnets` is not needed. The field is made optional
(the `MaxItems=2`, immutability and dual-stack CEL rules on it are kept):

```diff
- // +kubebuilder:validation:Required
- // +kubebuilder:validation:MinItems=1
+ // Required when evpnConfiguration is not set (Geneve transport).
+ // Forbidden when evpnConfiguration is set (EVPN transport).
+ // +optional
  // +kubebuilder:validation:MaxItems=2
- // +required
- ConnectSubnets []ConnectSubnet `json:"connectSubnets"`
+ ConnectSubnets []ConnectSubnet `json:"connectSubnets,omitempty"`
```

Two CEL rules on the spec make `connectSubnets` and `evpnConfiguration`
**exactly one of** — at least one must be set, and both together is rejected:

```go
// +kubebuilder:validation:XValidation:rule="has(self.connectSubnets) || has(self.evpnConfiguration)",message="either connectSubnets or evpnConfiguration must be specified"
// +kubebuilder:validation:XValidation:rule="!has(self.connectSubnets) || !has(self.evpnConfiguration)",message="connectSubnets and evpnConfiguration are mutually exclusive"
```

This is a backwards-compatible loosening — existing Geneve CNC CRs
already have `connectSubnets` set (and no `evpnConfiguration`), so they
continue to pass both rules.

##### Immutability

Two rules keep a CNC's EVPN identity fixed for its lifetime. They close
different holes:

```go
// field-level, on evpnConfiguration:
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="evpnConfiguration is immutable"

// spec-level:
// +kubebuilder:validation:XValidation:rule="has(oldSelf.evpnConfiguration) == has(self.evpnConfiguration)",message="transport is immutable: evpnConfiguration cannot be added or removed after creation"
```

* **`evpnConfiguration` is immutable** (field-level `self == oldSelf`). The
  parent VRF's L3 VNI and route target *are* the connect's identity on the
  EVPN fabric. Changing them on a live CNC would reallocate the VID, rebuild
  the L3 SVI on the SVD bridge under a new VNI, re-originate every Type 5
  route, and break every external peer importing the old route target — a
  delete-and-recreate in disguise. Freezing the field makes that explicit.
  This mirrors `connectSubnets`, which is already immutable, so both transports
  behave consistently. Note this does **not** freeze CNC membership: which
  CUDNs are connected is driven by `networkSelectors`, which stays mutable, so
  day-2 add/remove of CUDNs (Story 4) still works.
* **Transport is immutable** (spec-level `has(oldSelf...) == has(self...)`).
  The field-level rule only fires while `evpnConfiguration` is present, so on
  its own it would not stop an update that *removes* `evpnConfiguration` while
  *adding* `connectSubnets` — flipping an EVPN CNC into a Geneve one (or vice
  versa) under the same object. The exactly-one and mutually-exclusive rules
  still hold across such an update, so they don't catch it either. This
  spec-level rule pins whichever transport the CNC was created with, turning a
  transport flip into a delete-and-recreate rather than a live conversion.

#### Validation rules

* `evpnConfiguration` is required when all selected CUDNs are EVPN type.
* `evpnConfiguration` is forbidden when any selected CUDN is Geneve type.
* Mixed transport (some Geneve, some EVPN) is rejected.
* The `ipVRF.vni` must not conflict with any other EVPN VNI **on the same
  VTEP**. Uniqueness is checked against a single cluster-wide conflict registry
  (`evpnregistry`) keyed `{VTEP, VNI} → owner`, shared by the UDN controller
  (CUDN MAC-VRF/IP-VRF VNIs) and the CNC controller (parent VRF VNIs). So the
  parent VNI must differ from **every CUDN's** MAC-VRF and IP-VRF VNI on that
  VTEP — *not only the CUDNs this CNC selects* — and from **every other CNC's**
  parent VRF VNI on that VTEP. Because the registry key includes the VTEP,
  networks on *different* VTEPs may reuse the same VNI; a conflict arises only
  when they share a VTEP. Day-one CNC scope is single-VTEP, so a CNC's parent
  VNI is in practice compared against all its selected CUDNs (which share that
  VTEP) plus anything else on it.
* **VNI uniqueness is not relaxed by CNC.** This per-VTEP rule is unchanged
  whether or not CUDNs are connected by a CNC. The parent VRF already provides
  shared routing — matching child VNIs would be redundant and reintroduce the
  one-way disconnection problem (see
  [Alternative 2](#alternative-2-idempotent-vni-reuse-matching-ipvrfvni)).
* For day-one scope: all selected CUDNs must reference the same VTEP CR.
  If VTEPs differ, the CNC is rejected with `VTEPMismatch` status condition.

> **TBD — resolve during review.** The core issue is that the VNI and the VID
> have their enforcement scope backwards relative to what each identifier
> actually is, and OVN-Kubernetes today enforces both at the wrong level:
> * **VNI is fabric-global but enforced per-VTEP (under-scoped).** In the
>   BGP-EVPN control plane a VNI is **globally significant across the entire
>   fabric** — every VTEP must agree a given VNI denotes the same segment, and
>   route-targets are derived from it. OVN-Kubernetes only enforces VNI
>   uniqueness *per VTEP* (via the `{VTEP, VNI}` registry), so it cannot detect
>   two VTEPs on the same interconnected fabric reusing a VNI for different
>   tenants. Ideally this would be enforced **globally**. Today the fabric-wide
>   guarantee is owned by the **end user** (they must allocate globally
>   consistent VNIs across VTEPs/clusters) and by the **Plexus orchestrator**
>   for cross-cluster CNC (see [Future Goals](#future-goals)).
> * **VID is node-local but allocated cluster-wide (over-scoped).** The VID
>   (transit-SVI VLAN ID) is only *locally* significant — it just tags a bridge
>   VLAN on a single node's SVD bridge and never leaves the wire — yet the
>   existing `vidAllocator` hands out VIDs **cluster-wide**, not per-VTEP/per-node
>   (this OKEP reuses that allocator as-is; a per-VTEP VID allocator is out of
>   scope, see the VID-allocation item). So the VID is enforced more strictly
>   than it needs to be, the mirror image of the VNI case.
>
> Neither mismatch blocks the day-one single-VTEP scope, but both should be
> settled during review, ideally in step with the UDN controller (which uses the
> same `{VTEP, VNI}` registry for CUDN VNIs today, so it has the identical
> VNI-scope question). Note the `VRFConfig.vni` godoc currently says it "Must be
> unique across all EVPN configurations in the cluster," which matches neither
> the enforced per-VTEP scope nor the ideal fabric-global scope — it should be
> corrected to whichever the review lands on.

#### Annotations

Cluster manager writes allocation results onto the CNC resource as
annotations for nodes to consume. The two annotations the existing Geneve
CNC uses are **not** used by the EVPN design, and the EVPN design adds one
new annotation that Geneve CNC never sets:

| Annotation | Geneve CNC | EVPN CNC |
|------------|:----------:|:--------:|
| `k8s.ovn.org/connect-router-tunnel-key` — tunnel key (VNI/Geneve TLV) for the OVN connect-router | ✅ used | ❌ unused |
| `k8s.ovn.org/network-connect-subnet` — subnets allocated for the connect-router patch ports | ✅ used | ❌ unused |
| `k8s.ovn.org/evpn-parent-vrf-vid` — VID for the parent VRF's L3 transit SVI (**new**) | ❌ unused | ✅ used |

Rationale: EVPN CNC has no OVN connect-router, so it needs neither a
connect-router tunnel key nor connect-router subnets — routing is done by
the parent VRF in the host FRR/kernel, not by an OVN logical router. Instead
the parent VRF's L3 SVI needs a VID, which (like the per-CUDN VIDs) is
allocated by cluster manager from the existing cluster-wide `vidAllocator`.
Because the parent VRF has no NAD to carry it (unlike CUDN VIDs, which ride
in the NAD config JSON), the VID is published on the CNC resource itself via
the new `k8s.ovn.org/evpn-parent-vrf-vid` annotation; the RA controller
watches this annotation to know when the CNC is provisioned (see the
[FRR configuration](#cluster-manager) item).

#### CNC status

The CNC status uses standard conditions to report readiness. No
per-node status writes from ovnkube-node — all status is computed
by the cluster manager:

```yaml
status:
  status: Success
  conditions:
    - type: Ready
      status: "True"
      reason: Applied
      message: "CNC applied successfully"
```

The status shape is **unchanged** — this design adds no new status fields.
It reuses the existing CNC `status` field (`Success`/`Failure`) plus the
standard `conditions`, and validation errors (e.g. `VTEPMismatch`,
`VNIConflict`) are reported through those existing conditions. The VID
(auto-allocated) is communicated to nodes via the
`k8s.ovn.org/evpn-parent-vrf-vid` annotation (see
[Annotations](#annotations)), not via status. As today, the cluster
manager cannot report whether the parent VRF is actually created on nodes,
since that is node-side work.

### Same-network vs cross-network traffic separation

Each connected subnet is advertised on the EVPN fabric exactly once, by
the VRF that owns it:

* An **`ipVRF` child** self-originates its subnet as Type 5 under its **own**
  L3 VNI and RT. The parent imports the child's routes locally (for hub
  routing) but **denies them on its own Type-5 advertisement**, so it never
  re-originates them. This is essential: every node imports the same
  per-node `/24` slices, and if each re-originated them under the shared
  parent L3 VNI the fabric would see one Type-5 path per VTEP for each slice
  — a spurious ECMP set that blackholes cross-node traffic (see
  [Cluster Manager](#cluster-manager)).
* A **MAC-VRF-only child** has no L3 VNI of its own, so the parent is its
  IP-VRF and advertises its subnet as Type 5 under the **parent's** L3 VNI
  and RT.

Because of this, the destination's L3 VNI selects the datapath:

* **Same-network cross-node** (alpha pod on A → alpha pod on B): Uses alpha's
  own L3 VNI, resolved entirely within alpha's VRF. Unchanged from today.
* **Cross-network cross-node, `ipVRF` destination** (alpha pod on A → gamma
  pod on B, gamma has `ipVRF`): the node's override route steers the packet
  from `evpn-l3-alpha` into the parent VRF, whose table holds gamma's subnet
  via **gamma's own L3 VNI** (imported from the local `evpn-l2l3-gamma`, which
  learned it over EVPN). The packet is encapsulated with gamma's VNI; node B
  decapsulates straight into `evpn-l2l3-gamma` — the parent VRF is not on the
  destination-node path.
* **Cross-network cross-node, MAC-VRF-only destination** (alpha pod on A →
  beta pod on B, beta MAC-VRF-only): the parent VRF reaches beta's pods via
  **the parent's L3 VNI** (symmetric-IRB `/32` host routes the parent learns
  as beta's IP-VRF). The packet is encapsulated with the parent VNI; node B
  decapsulates into the parent VRF, and the reinject route delivers into
  `evpn-l2-beta`.

**Scoping each VRF's Type-5 advertisements.** An `ipVRF` child advertises only
its own subnet: its stanza is unchanged from stock EVPN — a `network`
statement for its subnet plus plain `advertise ipv4 unicast`, scoped by its
own route-target — so the child never leaks anything else into EVPN:

```
router bgp 64512 vrf evpn-l3-alpha
 address-family ipv4 unicast
  network 10.10.1.0/24
 exit-address-family
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:100
  route-target export 65000:100
```

Only the **parent** applies an advertise route-map: its `advertise ipv4
unicast route-map CNC-<PARENT>-ADVERTISE` **denies** the `ipVRF` children's
subnets (they self-originate under their own route-targets) and permits
everything else — the MAC-VRF-only children's subnets, for which the parent
is the originator.

External peers can choose their view:
* Import a child's RT to see that CUDN's subnet.
* Import the parent's RT to see the CNC's MAC-VRF-only subnets. The `ipVRF`
  children are seen via their own RTs (the parent does not re-advertise
  them), so a peer that wants the full connected set imports every
  participating RT.

### Cross-node cross-network packet walk

The walk below uses the three CUDN types from the validated PoC (see
[Implementation Details](#implementation-details)), all connected by one CNC
whose parent VRF is `cnc.f61ee1a4` (L3 VNI 400, routing table 1000006 on this
node):

| CUDN | Topology | Subnet | L3 VNI | Child VRF table |
|------|----------|--------|--------|-----------------|
| `alpha` | Layer3, `ipVRF` | 10.10.0.0/16 (per-node /24) | 100 | 1022 |
| `beta`  | Layer2, MAC-VRF-only | 10.20.0.0/16 (flat) | — (parent's 400) | 1011 |
| `gamma` | Layer2 + `ipVRF` | 10.30.0.0/16 (flat) | 301 | 1013 |

The **source-node** steps are common to every cross-network flow. Starting
from an `alpha` pod (10.10.1.x) on node A:

1. Pod sends to the destination pod IP.
2. Packet traverses the OVN logical switch → exits via `alpha`'s management
   port into the `evpn-l3-alpha` VRF (table 1022).
3. Table 1022 has no same-network route for the destination. CNC adds
   node-programmed **override routes** (proto 85, metric 5), one per sibling
   subnet, that steer sibling-bound traffic into the parent VRF device. The
   `+` lines below are what CNC adds to the child's table; everything else is
   the CUDN's normal routing:

   ```diff
     # ip route show table 1022   (evpn-l3-alpha, on node A)
     unreachable default proto 85 metric 4278198272
     10.10.0.0/24 nhid 150 via 172.18.0.4 dev svl3.1 proto bgp metric 20 onlink  # alpha /24 on ovn-control-plane (alpha's own VNI 100)
     10.10.0.0/16 via 10.10.1.1 dev ovn-k8s-mp1 proto 85
     10.10.1.0/24 dev ovn-k8s-mp1 proto kernel scope link src 10.10.1.2          # alpha local /24
     10.10.2.0/24 nhid 149 via 172.18.0.2 dev svl3.1 proto bgp metric 20 onlink  # alpha /24 on ovn-worker2 (alpha's own VNI 100)
   + 10.20.0.0/16 dev cnc.f61ee1a4 proto 85 metric 5                             # -> beta,  into parent (CNC override)
   + 10.30.0.0/16 dev cnc.f61ee1a4 proto 85 metric 5                             # -> gamma, into parent (CNC override)
     10.96.0.0/16 via 169.254.0.4 dev breth0 proto 85 src 169.254.0.2 mtu 1400   # service network
   ```

4. The parent VRF (table 1000006) performs LPM. What happens next depends on
   the **destination network's type**, because the destination's L3 VNI
   selects the datapath.

**Case A — destination is an `ipVRF` child** (`alpha` pod → `gamma` pod
10.30.0.y): the parent's table holds `gamma`'s subnet as an imported route
pointing at the `evpn-l2l3-gamma` child VRF device (`import vrf
evpn-l2l3-gamma`), so LPM hands the packet to `gamma`'s VRF, where it resolves
through **`gamma`'s own L3 VNI 301** (the symmetric-IRB routes `gamma`
self-originates):

   ```
   # ip route show table 1000006   (parent cnc.f61ee1a4, on node A)
   10.30.0.0/16 nhid 111 dev evpn-l2l3-gamma proto bgp metric 20   # gamma imported (resolves in gamma's VRF, VNI 301)
   ```

The packet is VXLAN-encapsulated with **VNI 301** and sent to node B's VTEP.

* On node B it decapsulates **straight into `evpn-l2l3-gamma`** — the parent
  VRF is *not* on the destination-node path — and is delivered to the `gamma`
  pod via its management port.

**Case B — destination is a MAC-VRF-only child** (`alpha` pod → `beta` pod
10.20.0.z): the parent is `beta`'s IP-VRF, so it holds `beta`'s host routes
under the **parent's L3 VNI 400**, next-hopped out the parent's own VTEP
device (`cvl3.f61ee1a4`):

   ```
   # ip route show table 1000006   (parent cnc.f61ee1a4, on node A)
   10.20.0.5 via 172.18.0.4 dev cvl3.f61ee1a4 proto bgp metric 20 onlink
   ```

The packet is encapsulated with **VNI 400** and sent to node B's VTEP.

* On node B it decapsulates **into the parent VRF** (table 1000006). A
  node-programmed **reinject route** (proto 85, metric 5) delivers the
  packet's own subnet into the `beta` child VRF:

   ```
   # ip route show table 1000006   (parent cnc.f61ee1a4, on node B)
   10.20.0.0/16 dev evpn-l2-beta proto 85 metric 5   # reinject into beta
   ```

  From there it exits `evpn-l2-beta` → management port → OVN → `beta` pod.

The return path is symmetric with the roles swapped: `gamma`/`beta` pods reach
`alpha` via `alpha`'s own L3 VNI 100 (Case A, since `alpha` is an `ipVRF`
child), using the same override-route-into-parent funnel on their side.

**Same flow with ServiceNetwork only (no PodNetwork):**

Steps 1-2 differ: OVN Tier0 ACLs intercept the traffic.

1. `alpha` pod sends to ClusterIP 10.96.1.1:80.
2. OVN Tier0 ACL: `ip4.dst==10.96.0.0/16` → **pass** (priority 5000).
   OVN LB performs DNAT: 10.96.1.1:80 → the backend pod IP.
3. Packet exits OVN with the backend pod IP as destination, and from there
   follows **Case A or Case B above** depending on the backend network's type.

If the `alpha` pod sends directly to a `beta`/`gamma` pod IP (bypassing the
service) with PodNetwork disabled: OVN Tier0 ACL
`ip4.src==$connected_udn_subnets && ip4.dst==$connected_udn_subnets && ct.new`
→ **drop** (priority 4950). Packet never reaches the VRF.

The VRF routing setup is **identical for both connectivity modes**.
The PodNetwork vs ServiceNetwork distinction is enforced entirely by
OVN ACLs on the logical switch, independent of the underlay transport.
This is the same mechanism used for Geneve CNC
([OKEP-5224](okep-5224-connecting-udns/okep-5224-connecting-udns.md#services)).

### Parent VRF lifecycle

The parent VRF is tied to the **CNC lifecycle**, not to individual CUDNs. Its
Linux routing table is `config.EVPNCNCRoutingTableIDStart + VID`
(`EVPNCNCRoutingTableIDStart` = 1000000), a reserved range so the parent VRF
tables never collide with per-CUDN VRF tables; `vrfManager.repair()` skips
tables at or above the start value. On the validated cluster the parent
`cnc.f61ee1a4` (VID 6) lands on table 1000006.

* **CNC created** → Parent VRF created with its L3 VNI, VID auto-allocated,
  L3 SVI created on the SVD bridge, FRR BGP stanza generated. Remains for
  the life of the CNC.
* **CUDN joins CNC** (created and matches selector, or label changed to
  match):
    * *FRR (cluster manager side):* `import vrf <child>` is added to the
      **parent's** stanza for every `ipVRF` child, scoped by the parent's
      `CNC-<PARENT>-IMPORT` route-map so only the connected CUDN subnets
      leak in. Children do **not** `import vrf <parent>` — the hub direction
      is provided by node-side routes, not by FRR import. A MAC-VRF-only
      child gets no import at all; instead its L2 SVI is enslaved to the
      parent so the parent becomes its IP-VRF.
    * *Node side:* for each sibling subnet, an override route
      (`<sibling>/N dev cnc-<parent> proto 85 metric 5`) is programmed in the
      child's VRF table so sibling-bound traffic funnels into the parent VRF.
      For MAC-VRF-only children, a reinject route
      (`<own-subnet> dev <child> proto 85 metric 5`) is programmed in the
      **parent's** table so parent-VNI traffic is delivered back into the L2
      child.
* **CUDN leaves CNC** (deleted, or label changed to no longer match) → the
  parent's `import vrf <child>` line, the child's override routes and any
  reinject route are removed. The child's per-CUDN VRF and interfaces are
  untouched.
* **CNC deleted** → All `import vrf` lines, override routes and reinject
  routes removed. Parent VRF deleted, L3 SVI removed, VID released. All
  children return to their independent state.

Ordering does not affect steady state:
* *CNC first:* Parent VRF exists before any CUDN. When CUDNs are created
  and match, imports are added.
* *CUDNs first:* CUDNs have their own VRFs. When CNC is created, parent
  VRF is created and imports are added to all matching children.
* *Mixed:* CNC exists with some CUDNs connected. A new CUDN matches — its
  import is added. Existing CUDNs already have imports.

### Multi-VTEP support (TBD during review)

When connected CUDNs reference different VTEP CRs, each VTEP maps
to a separate SVD bridge (`evbr-<vtep>`) with its own VXLAN device
and its own VTEP source IP. The parent VRF needs an L3 SVI on the
SVD bridge for EVPN Type 5 encapsulation/decapsulation.

With different VTEPs, the parent VRF's L3 SVI must exist on the
correct SVD bridge. The design supports two approaches:

**Single-VTEP CNC (day-one scope):** All CUDNs in a CNC reference the
same VTEP CR. The parent VRF's L3 SVI is created on that VTEP's SVD
bridge. If CUDNs reference different VTEPs, the CNC controller rejects
the configuration with a status condition (`VTEPMismatch`).

**Multi-VTEP CNC (TBD during review — not in day-one scope):** The parent
VRF needs L3 SVIs on multiple SVD bridges — one per VTEP referenced by
connected CUDNs. A Linux VRF is a routing table and does not restrict which
bridge its member interfaces belong to, so L3 SVIs from different bridges can
coexist in the same VRF. This is a design sketch only; it is **not
implemented or datapath-validated** and the open questions below must be
settled during review before the single-VTEP restriction can be lifted.

```
Example: alpha on VTEP-A (evbr-a), gamma on VTEP-B (evbr-b)

┌────────── cnc.f61ee1a4 (L3 VNI 400) ────────────────────────────┐
│  evbr-a.4 (L3 SVI on VTEP-A's bridge, VNI 400 via vxlan-a)      │
│  evbr-b.4 (L3 SVI on VTEP-B's bridge, VNI 400 via vxlan-b)      │
└─────────────────────────────────────────────────────────────────┘
```

> **TBD — resolve during review.** Open questions for multi-VTEP, all of
> which need a multi-VTEP test bed before the single-VTEP restriction can be
> lifted:
> * The same L3 VNI (400) is mapped on both `vxlan-a/evbr-a` and
>   `vxlan-b/evbr-b`. Since these are separate bridges with separate VXLAN
>   devices and separate source IPs, the VNI filter mappings *should* not
>   conflict at the bridge level — needs confirmation on real hardware/kernel.
> * When FRR installs a Type 5 route with a remote VTEP IP as next-hop, the
>   kernel must resolve which VXLAN device to use. Each VXLAN device has a
>   different local VTEP IP, so the kernel *should* select the correct device
>   based on FDB entries and underlay routing — needs validation.
> * FRR's SVD model assumes one bridge and one VXLAN device per node. Whether
>   FRR and the kernel correctly handle dual L3 SVIs from different bridges in
>   the same VRF is unproven and must be validated.

### Implementation Details

The design below is implemented and datapath-validated on the PoC branch
[`tssurya/ovn-kubernetes` `evpn-cnc-6607`](https://github.com/tssurya/ovn-kubernetes/tree/evpn-cnc-6607);
the FRR and route snippets in this section are taken from that live cluster.

#### Cluster Manager

The cluster manager's CNC controller is extended to handle EVPN CUDNs:

1. **Transport detection**: When a CNC's selector matches CUDNs, the
   controller inspects their EVPN configuration. If all CUDNs have EVPN
   config, the EVPN path is used. If all are Geneve, the existing
   connect-router path is used. Mixed is rejected.

2. **VNI reservation**: The parent VRF's L3 VNI (from `evpnConfiguration.ipVRF.vni`)
   is reserved in the existing `reservedVNIs` map, keyed by the CNC name.
   Conflicts with CUDN VNIs are rejected.

3. **VID allocation**: A VID for the parent VRF's L3 SVI is allocated from
   the existing `vidAllocator`, keyed as `cnc-<name>/ipvrf`. Per-CUDN VIDs
   are stored in the NAD config JSON and read by nodes from there. Since the
   parent VRF has no NAD, the VID is communicated to nodes via a dedicated
   annotation on the CNC resource.

   > **FYI:** The existing `vidAllocator` is **cluster-wide**, not per-VTEP —
   > it hands out VIDs from a single pool shared across all VTEPs. This OKEP
   > **reuses that allocator as-is** and does not change its scoping;
   > per-VTEP VID allocation is out of scope here.

4. **FRR configuration**: The parent VRF's FRR stanzas are generated by the
   RouteAdvertisements (RA) controller in cluster-manager. Two changes wire
   this up:
   * The RA controller gains a **dedicated CNC sub-controller** — it now
     watches `ClusterNetworkConnect` (via `cncLister` / a
     `ClusterNetworkConnectInformer`), in addition to the RA resources it
     already reconciles. `cncNeedsUpdate` also watches the CNC's VID
     annotation, so RA re-reconciles once the CNC controller has allocated the
     parent VID and marked the CNC provisioned. On each reconcile it lists
     CNCs, matches their child networks against the RA's `networkSet`, and
     builds one `cncParentVRFConfig` per covered CNC.
   * `rawconfig.go` (`genCNCParentVRFSection`) turns each `cncParentVRFConfig`
     into the parent VRF's FRR stanza.

   **A CNC's child networks may be advertised by more than one RA.** A CNC
   does not have to line up 1:1 with a single `RouteAdvertisements`. Each
   reconcile computes the children this RA covers as the intersection of the
   CNC's selected networks with the RA's `networkSet`, and emits a **partial**
   parent VRF stanza that imports only those covered children. This works
   because frr-k8s raw config is plain string concatenation (ordered by
   `Priority`) and FRR (≥10.2.2, the minimum version ovn-kubernetes targets)
   processes duplicate `router bgp ... vrf cnc.<hash>` blocks **additively** —
   so the union of every covering RA's partial stanza converges to the correct
   full parent VRF once all of them have reconciled. No single RA needs the
   complete child set.

   The trade-off is a **transient, self-correcting window**: between the first
   and last covering RA reconciling, the parent VRF carries only a subset of
   the `import vrf` entries, so traffic between children whose imports have not
   yet appeared is briefly black-holed until the remaining RAs reconcile. This
   window is why the recommended **best practice** (not an enforced
   restriction) is to advertise all of a CNC's child networks under a single
   RA — the CNC's network set as a subset of one RA's `networkSet` — which
   makes the parent stanza complete in one reconcile and eliminates the window
   entirely. Surfacing partial-coverage as a condition on the CNC status
   (so operators can see when the parent VRF is not yet fully assembled) is a
   follow-up, tracked as a `TODO` in the PoC.

   The snippets below are taken verbatim from the validated PoC cluster
   (parent `cnc.f61ee1a4`, VNI 400; `ipVRF` children `alpha`/`gamma`;
   MAC-VRF-only child `beta`).

   The **parent** stanza `import vrf`'s only the `ipVRF` children, scoped by an
   **import route-map** so only the connected CUDN subnets leak into the hub
   (not each child's full external reachability), and applies an **advertise
   route-map** that *denies* the `ipVRF` children's subnets — they
   self-originate under their own L3 VNI, so the parent must not re-originate
   them under VNI 400 (doing so produces one spurious Type-5 path per VTEP →
   ECMP blackhole across nodes):

   ```
   ip prefix-list CNC-CNC.F61EE1A4-PREFIXES seq 10 permit 10.10.0.0/16 le 24
   ip prefix-list CNC-CNC.F61EE1A4-PREFIXES seq 20 permit 10.20.0.0/16
   ip prefix-list CNC-CNC.F61EE1A4-PREFIXES seq 30 permit 10.30.0.0/16
   !
   ip prefix-list CNC-CNC.F61EE1A4-IPVRF   seq 10 permit 10.10.0.0/16 le 32
   ip prefix-list CNC-CNC.F61EE1A4-IPVRF   seq 20 permit 10.30.0.0/16 le 32
   !
   route-map CNC-CNC.F61EE1A4-IMPORT permit 10
    match ip address prefix-list CNC-CNC.F61EE1A4-PREFIXES
   route-map CNC-CNC.F61EE1A4-IMPORT deny 20
   !
   route-map CNC-CNC.F61EE1A4-ADVERTISE deny 10
    match ip address prefix-list CNC-CNC.F61EE1A4-IPVRF
   route-map CNC-CNC.F61EE1A4-ADVERTISE permit 20
   !
   router bgp 64512 vrf cnc.f61ee1a4
    address-family ipv4 unicast
     import vrf route-map CNC-CNC.F61EE1A4-IMPORT
     import vrf evpn-l2l3-gamma
     import vrf evpn-l3-alpha
    exit-address-family
    address-family l2vpn evpn
     advertise ipv4 unicast route-map CNC-CNC.F61EE1A4-ADVERTISE
     advertise ipv6 unicast
    exit-address-family
   exit
   ```

   The MAC-VRF-only child `beta` is **not** imported by the parent — its L2 SVI
   is enslaved to the parent (parent is its IP-VRF), so `beta`'s subnet is a
   *connected* route in the parent table and is advertised by the parent's
   `permit 20` (10.20.0.0/16 is absent from the IPVRF deny list).

   Each **`ipVRF` child** keeps its own unchanged EVPN stanza — it originates
   just its own subnet as a `network` statement into `ipv4 unicast`, re-emits
   it into EVPN with plain `advertise ipv4 unicast`, and scopes it under its
   **own route-target** (`65000:<child-L3-VNI>`). Children do **not** apply an
   advertise route-map and do **not** `import vrf` the parent (the hub
   direction is node-side, see [Node](#node-ovnkube-node)):

   ```
   router bgp 64512 vrf evpn-l3-alpha
    address-family ipv4 unicast
     network 10.10.1.0/24
    exit-address-family
    address-family l2vpn evpn
     advertise ipv4 unicast
     route-target import 65000:100
     route-target export 65000:100
    exit-address-family
   exit
   ```

   The `network` statement plus the child's own route-target are what keep the
   child advertising only its own subnet, so no per-child advertise route-map
   is needed. (The controller still emits `CNC-EVPN-L3-ALPHA-PREFIXES` /
   `-ADVERTISE` route-map objects into FRR, but they are **not** applied on the
   child's `advertise` — only the **parent's** `CNC-<PARENT>-ADVERTISE` filter
   above is load-bearing.)

5. **VTEP validation**: Initially, the controller verifies all selected
   CUDNs reference the same VTEP CR and rejects with `VTEPMismatch` if
   not. This restriction is lifted once the multi-VTEP
   [open question](#open-questions) is confirmed. With multi-VTEP support, the
   parent VRF gets an L3 SVI on each referenced VTEP's SVD bridge.

#### Node (ovnkube-node)

The node-side parent VRF and inter-UDN datapath are managed by the existing
EVPN node controller (`evpn_node_controller.go`), extended to watch CNC
resources — a natural fit since it already owns per-CUDN VRFs and SVIs. The
node-side work is:

1. **Parent VRF creation**: A Linux VRF device (`cnc-<name>`, on the validated
   cluster `cnc.f61ee1a4`) is created via the existing `vrfManager` on routing
   table `EVPNCNCRoutingTableIDStart + VID` (1000006 here), with
   `RPFilterDisable=true` (see step 4).

2. **L3 SVI creation**: An L3 SVI VLAN sub-interface is created on the SVD
   bridge for the parent VRF's L3 VNI, using the allocated VID, and enslaved
   to the parent VRF — the same pattern as per-CUDN L3 SVIs in `reconcileSVIs`.
   For MAC-VRF-only children, the child's L2 SVI is additionally enslaved to
   the parent so the parent becomes their IP-VRF.

3. **Sibling / reinject routes** (`reconcileCNCSiblingRoutes`): the hub
   direction is programmed as static routes (proto 85, metric 5), *not* via
   FRR `import vrf`. For each child, one **override route** per sibling subnet
   funnels sibling-bound traffic into the parent VRF device (shown for
   `evpn-l3-alpha` in the
   [packet walk](#cross-node-cross-network-packet-walk)). For MAC-VRF-only
   children, one **reinject route** in the parent table delivers the L2
   network's own subnet back into the child VRF. The parent table otherwise
   fills from BGP — imported `ipVRF` slices resolved under each child's own
   VNI, MAC-VRF-only host routes under the parent VNI — so the `+` line is the
   only route CNC adds to it:

   ```diff
     # ip route show table 1000006   (parent cnc.f61ee1a4, on node A)
     10.10.0.0/24 nhid 150 via 172.18.0.4 dev svl3.1 proto bgp metric 20 onlink      # alpha /24 on ovn-control-plane (alpha's VNI 100)
     10.10.1.0/24 nhid 112 dev evpn-l3-alpha proto bgp metric 20                     # alpha local /24 (imported)
   + 10.20.0.0/16 dev evpn-l2-beta proto 85 metric 5                                 # beta reinject (CNC-added)
     10.20.0.5 nhid 185 via 172.18.0.4 dev cvl3.f61ee1a4 proto bgp metric 20 onlink  # remote beta host (parent VNI 400)
     10.30.0.0/16 nhid 111 dev evpn-l2l3-gamma proto bgp metric 20                   # gamma (imported, gamma's VNI 301)
   ```

4. **Reverse-path filtering** (`RPFilterDisable=true` on the parent VRF device
   and the CNC transit SVIs — `cvl3.<hash>`, `svl3.<n>`): the forward and
   reverse legs of an inter-UDN flow egress *different* VRF-master (l3mdev)
   devices. When a VXLAN-decapped transit packet lands on a CNC SVI, its
   reverse route for the *source* resolves out a VRF-master device (an l3mdev
   table redirect, e.g. `dev evpn-l2l3-gamma`), which `fib_validate_source`
   does not accept as a valid reverse path — **even in loose mode (`2`)**.
   Loose mode still requires the source to be reachable via *some* real
   interface; an l3mdev redirect is not one. So the filter must be fully
   disabled (`0`), not just loosened.

   **Why this is broader than it looks — and its downside.** `rp_filter` is
   evaluated as `max(net.ipv4.conf.all.rp_filter, net.ipv4.conf.<dev>.rp_filter)`,
   so a per-device `0` is inert while `conf.all` is non-zero. Making the
   per-device `0` effective therefore forces `net.ipv4.conf.all.rp_filter = 0`
   as well, which is **node-global**: it lowers the effective RPF of *every*
   interface to that interface's own per-device value. Any interface that was
   relying on `conf.all` to enforce strict/loose RPF loses that enforcement and
   falls back to its per-device setting (often `0`). Concretely, the anti-spoof
   guarantee RPF provides — dropping a packet whose source address is not
   routable back out its ingress interface — is weakened host-wide, not just on
   the CNC devices we intended to relax.

   What contains the blast radius:
   * OVN-Kubernetes already runs **asymmetric datapaths** and already sets
     **loose RPF (`2`)** on management ports, UDN mgmt ports, and EVPN SVIs, so
     it does not depend on strict host-global RPF for correctness. Dropping
     `conf.all` to `0` moves the node from "loose everywhere" to "off where a
     device is `0`," a smaller change than it first appears.
   * RPF is **not** the tenant-isolation mechanism here. Inter-UDN and
     north-south isolation is enforced by the `udn-isolation` / `udn-bgp-drop`
     nftables chains (see step 5) and by OVN ACLs — none of which RPF gates. So
     disabling RPF does not open cross-UDN leakage; it only removes a host-L3
     anti-spoofing check on a node that, as a CNI host, already forwards
     tenant-controlled traffic.
   * The relaxation is **local-gateway-mode-specific**. It exists only because
     inter-UDN L3 forwarding happens in the Linux host stack. OVN-native EVPN
     in shared gateway mode keeps this forwarding inside OVN, so the sysctl
     change disappears entirely and anti-spoofing/isolation is handled by OVN
     (see [Future Goals](#future-goals)).

   IPv6 has no equivalent sysctl (the kernel has no strict RPF mode for IPv6),
   so nothing changes for IPv6 and there is no corresponding exposure.

5. **Host isolation exemptions** (`reconcileCNCIsolationExemptions`): inter-UDN
   traffic must bypass the host-stack UDN drop chains. Node adds `udn-cnc-allow`
   nftables exemptions to **both** the `udn-isolation` and `udn-bgp-drop`
   chains for the connected CUDN subnets. (These host-stack artifacts exist
   because this is local gateway mode; OVN-native EVPN in shared gateway mode
   would enforce isolation in OVN ACLs instead — see [Future Goals](#future-goals).)

6. **Cleanup**: When the CNC is deleted, the override/reinject routes,
   nftables exemptions, L3 SVI and parent VRF are removed; the reserved table
   ID is released.

#### ovnkube-controller (OVN DB)

The existing CNC networkconnect controller handles the OVN-side setup,
which is largely transport-independent:

1. **ServiceNetwork**: When `ServiceNetwork` is enabled, OVN load balancers
   for each connected network are cross-attached to every other connected
   network's logical switches. This is the same mechanism used for Geneve
   CNC. After DNAT on the switch, traffic follows the inter-subnet routing
   path provided by the parent VRF.

2. **Partial connectivity ACLs**: When `ServiceNetwork` is enabled without
   `PodNetwork`, the same Tier0 ACLs from Geneve CNC are applied:
   pass-service (priority 5000), drop-pod (priority 4950). These operate
   on OVN logical switches, independent of the underlay transport.

3. **Advertised UDN isolation ACLs**: EVPN CUDNs are advertised networks.
   When `AdvertisedUDNIsolationMode=strict`, the advertised network
   controller installs a deny ACL at priority 1050 in `LportEgressAfterLB`
   that blocks all inter-network traffic. The CNC controller must install
   its own pass ACL at a priority between 1100 (intra-network pass) and
   1050 (inter-network deny) to allow cross-CUDN traffic for connected
   networks. This is handled by
   [PR #6272](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6272),
   which is a prerequisite for EVPN CNC.

4. **Connect-router**: For EVPN CUDNs, the OVN connect-router is **not
   created** — the parent VRF is the EVPN equivalent of the connect-router,
   providing inter-subnet routing across connected networks. The controller
   skips connect-router creation when EVPN transport is detected.

5. **NetworkPolicy**: When `PodNetwork` is enabled, policy peers span
   across all connected networks — same as Geneve CNC. No EVPN-specific
   changes needed. OVN ACLs operate on the logical switch level,
   independent of the underlay transport. Note: NetworkPolicy support
   across connected networks is still work in progress as part of the
   original CNC OKEP
   ([OKEP-5224](okep-5224-connecting-udns/okep-5224-connecting-udns.md#network-policies-and-admin-network-policies)).

#### Open questions

The design and datapath were validated end-to-end on the PoC branch. The
following behavior remains open:

* **Multi-VTEP L3 SVI coexistence.** When the parent VRF has L3 SVIs on
  multiple SVD bridges (one per VTEP), FRR/kernel handling of Type 5
  encap/decap through the correct VXLAN device needs validation. This blocks
  lifting the single-VTEP restriction; the PoC was validated single-VTEP.

### Testing Details

#### Unit Tests

*Cluster manager (CNC controller, `evpnregistry`, RA controller):*

* Parent VRF FRR config generation (`rawconfig.go`
  `genCNCParentVRFSection`) — correct `vrf`/`vni`/`import vrf route-map`
  stanzas, and the `CNC-<PARENT>-ADVERTISE` route-map that **denies**
  IP-VRF children's subnets (they self-originate under their own L3 VNI)
  while **permitting** MAC-VRF-only children (the parent is their
  IP-VRF originator). Cover: MAC-VRF-only children only, IP-VRF children
  only, and mixed. (`TestGenCNCParentVRFSection`.)
* Child FRR stanza generation — IP-VRF children emit plain
  `advertise ipv4 unicast` + `network <subnet>` + `route-target
  import/export`, **no** applied route-map; per-child route-map objects
  are emitted but not referenced.
* RA CNC sub-controller — `cncNeedsUpdate` re-reconciles on the CNC's
  VID annotation; child networks are matched to the RA network set and
  wired into `cncParentVRFConfig`.
* VNI reservation conflict detection (`evpnregistry`) — `Reserve`/
  `Release` keyed by `{VTEP, VNI}`; parent VNI conflicts against CUDN
  MAC-VRF/IP-VRF VNIs and other CNCs' parent VNIs on the same VTEP;
  same VNI on a *different* VTEP does not conflict.
* VID allocation for CNC — keyed `cnc-<name>/ipvrf` from the shared
  `vidAllocator`, published via the `k8s.ovn.org/evpn-parent-vrf-vid`
  annotation, released on CNC deletion.
* Transport detection — all-Geneve, all-EVPN, mixed (rejected).
* VTEP validation — same VTEP passes, different VTEPs rejected
  (`VTEPMismatch`).
* `import vrf` additions/removals as CUDNs join/leave the CNC.

*Node (ovnkube-node):*

* Child→parent static override route generation (proto 85, metric 5)
  funnelling each sibling aggregate into the parent VRF table; removed
  on CNC leave; requeued while networks are still pending.
* Reinject route generation (parent table → child VRF) for
  MAC-VRF-only children only.
* Parent VRF routing-table ID reservation — `EVPNCNCRoutingTableIDStart`
  (1000000) range reserved; `vrfmanager` `repair()` skips tables ≥ that.
* `rp_filter` disable on the parent VRF device and transit SVIs —
  both `all` and per-device sysctls zeroed (effective
  `max(all, dev)`); IPv6 no-op.
* nftables exemption of CNC inter-UDN traffic from UDN host isolation.

(The `RouteTargetString` CEL is not new — it is relocated unchanged from
`userdefinednetwork/v1` to `crd/types`; CEL rules are apiserver-enforced and
covered under CRD Integration Tests below, not as Go unit tests.)

#### E2E Tests

* **EVPN CNC with PodNetwork**: Two EVPN CUDNs (one with ipVRF, one
  MAC-VRF-only) connected via CNC. Verify pod-to-pod connectivity
  same-node and cross-node.
* **EVPN CNC with ServiceNetwork only**: Same setup, verify ClusterIP
  service access works but direct pod-to-pod is blocked.
* **Day-2 operations**: Add a third CUDN to existing CNC, verify
  connectivity. Remove a CUDN, verify remaining connectivity is
  unaffected and removed CUDN is isolated.
* **CNC deletion**: Verify all children return to independent state,
  parent VRF is cleaned up.
* **Mixed transport rejection**: CNC selecting both Geneve and EVPN
  CUDNs is rejected.
* **VNI conflict rejection**: CNC with a VNI that conflicts with an
  existing CUDN VNI is rejected.

#### CRD Integration Tests

* `evpnConfiguration` field validation: required for EVPN, forbidden
  for Geneve, VNI range validation.
* `connectSubnets` optionality when `evpnConfiguration` is present.
* Exactly-one-of spec CEL: both `connectSubnets` and `evpnConfiguration`
  set is rejected, and neither set is rejected.
* `evpnConfiguration` immutability CEL: updating `ipVRF.vni` or
  `ipVRF.routeTarget` on an existing CNC is rejected.
* Transport-immutability spec CEL: removing `evpnConfiguration` (with or
  without adding `connectSubnets`) on an existing CNC is rejected, and the
  reverse Geneve→EVPN flip is rejected too.
* `routeTarget` `RouteTargetString` CEL (relocated to `crd/types`):
  valid FRR RT forms accepted (`EF:OPQR`, `GHJK:MN`, `A.B.C.D:MN`,
  `*:OPQR`, `*:MN`), malformed values rejected.

#### Cross Feature Tests

* **NetworkPolicy with EVPN CNC**: Verify policies span connected
  networks when PodNetwork is enabled, and are scoped to single
  network when PodNetwork is not enabled.
* **RouteAdvertisements interaction**: Verify existing RouteAdvertisements
  for CUDNs continue to work when CNC adds `import vrf` directives.

### Documentation Details

* New section in the
  [Cluster Network Connect](../features/user-defined-networks/cluster-network-connect.md)
  feature doc covering EVPN CNC configuration, examples, and limitations.
* Update the CNC API reference doc with the new `evpnConfiguration` field.
* Add this OKEP to `mkdocs.yml` under Enhancement Proposals.

## Performance and Scale

The expected usage is a platform/network admin (or Plexus) connecting a
handful of EVPN tenant CUDNs for controlled inter-tenant connectivity, with
occasional day-2 add/remove — i.e. tens of CUDNs across tens of nodes, and a
small number of CNCs. At that scale the design's cost is negligible; the
points below are what actually matters when planning a deployment.

**Connecting CUDNs is cheap and does not grow fabric load.** Each connected
subnet is still advertised as EVPN Type 5 exactly once by its originator, so
CNC does **not** multiply BGP advertisements or per-node route counts versus
the same CUDNs running unconnected. It also does not merge L2 broadcast
domains: connectivity is L3-routed through the parent VRF (symmetric IRB,
Type 5), so BUM traffic stays contained within each CUDN's own MAC-VRF and is
never flooded across connected CUDNs. Data-plane cost is two kernel FIB
lookups (child → parent → destination) — negligible latency.

**Steady-state overhead per CNC is small and fixed:** one Linux VRF, one L3
VNI, one FRR BGP stanza per node, and one L3 SVI per node (plus the parent's
`import vrf` of each connected CUDN, scoped by the `CNC-<PARENT>-IMPORT`
route-map).

**The one ceiling to plan for is the VID pool.** Each parent VRF consumes one
VID (per VTEP where it has connected CUDNs) from the **cluster-wide** 4094-entry
pool it shares with all CUDN MAC-VRF/IP-VRF VIDs. This only becomes a concern
with a large number of CUDNs, CNCs, and VTEPs combined — see
[VLAN ID Exhaustion](#vlan-id-exhaustion-on-the-svd-bridge).

## Risks, Known Limitations and Mitigations

### North-South Route Advertisement Scope

Connecting CUDNs makes their subnets' Type 5 routes visible to **all**
BGP peers — both the cluster-internal iBGP mesh (other nodes, route
reflector) and any external peers (eBGP or otherwise). The MAC-VRF-only
subnets are advertised by the parent VRF; the `ipVRF` children advertise
their own. Type 5 advertisement is required for cross-node routing over
the EVPN fabric (`import vrf` and the node override routes only handle
same-node leaking), so the connected subnets are necessarily exposed to
whatever peers receive those advertisements.

1. **Inbound (open limitation)**: External routers learn routes to the
   connected subnets via these Type 5 advertisements. For subnets that
   should remain cluster-internal (private subnets), this is undesirable.
   Today there is no mechanism in OVN-Kubernetes to selectively scope
   Type 5 advertisement to internal-only peers. FRR could attach a
   `no-export` BGP community via route-map to prevent propagation beyond
   the iBGP mesh, but the RouteAdvertisements controller does not generate
   this configuration.

   **Mitigation**: Tracked separately in
   [#6631](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6631).
   Until then, admins must configure route-maps on external peers to filter
   private subnet prefixes; the parent VRF's route target can be used as a
   filter criterion.

2. **Outbound / cross-CUDN route scope**: Only the connected CUDN subnets
   are shared between children — a child's learned external routes are not.
   The parent imports each child through the `CNC-<PARENT>-IMPORT` route-map,
   scoped to the connected CUDN subnets, so a child's route to an external
   destination stays in that child and does not become reachable to its
   siblings via the parent (see [Cluster Manager](#cluster-manager)). A
   connected CUDN therefore does not inherit a sibling's north-south
   reachability.

### External Peer Route Target Awareness

The parent VRF introduces a new route target that external peers
must be aware of to receive the CNC's aggregated routes. External
peers configured for per-CUDN route targets continue to work —
per-CUDN Type 5 advertisements are untouched.

**Mitigation**: The parent VRF's route target is already known to the
admin — it is either set explicitly in `evpnConfiguration.ipVRF.routeTarget`
or auto-derived as `<ASN>:<VNI>` from the configured VNI — so no new status
surface is needed to discover it. Admins configure external peers using that
value. For Plexus-managed deployments, the orchestrator that provisions the
CNC also programs the peer route targets.

### Local Gateway Mode Only

EVPN in OVN-Kubernetes is only supported in local gateway mode today, so
EVPN CNC inherits that constraint — it is not specific to CNC (per
OKEP-5088). It is expected to be lifted once OVN-native EVPN lands — see
[Future Goals](#future-goals). The CNC API is deliberately kept free of
host-VRF / FRR-specific fields so the transit design carries over without
an API change.

### Multi-VTEP Validation

Connecting CUDNs that reference different VTEP CRs requires the parent
VRF to have L3 SVIs on multiple SVD bridges. This is a non-standard
configuration in EVPN (datacenter designs assume one VTEP per leaf
switch). Whether FRR and the kernel correctly handle this needs
validation before the single-VTEP restriction can be lifted.

**Mitigation**: Day-one scope requires all connected CUDNs to reference
the same VTEP CR. Multi-VTEP is in goals and will be enabled after
testing confirms correctness.

### VLAN ID Exhaustion on the SVD Bridge

Each CNC parent VRF consumes one VID (VLAN ID) on the SVD bridge for its
L3 transit SVI, on top of the VIDs already consumed by every CUDN's
MAC-VRF and IP-VRF SVIs. The bridge VLAN space is capped at 4094, and the
VID is drawn from the existing **cluster-wide** `vidAllocator`, so parent
VRFs and CUDNs across the whole cluster share one 4094-entry pool.

**Mitigation**: At practical scale exhaustion is unlikely. A per-VTEP VID
allocator (the allocator is cluster-wide today, stricter than the VID's
node-local significance) would relax this but is out of scope — see the
VID-scope [TBD](#validation-rules).

## OVN-Kubernetes Version Skew

This feature targets **v1.5.0**.
It requires:
* EVPN support (OKEP-5088, available since v1.2.0)
* CNC support (OKEP-5224, available since v1.2.0)
* FRR-K8S `import vrf` support (frr-k8s API, already available)

Existing EVPN CUDNs created in earlier versions can be connected via
CNC without recreation — the CNC controller adds the parent VRF and
`import vrf` configuration alongside the existing per-CUDN VRFs.

During rolling upgrades where some nodes run v1.4.x and others run
v1.5, the parent VRF is only configured on v1.5 nodes. Pods on
v1.4.x nodes in connected CUDNs retain their per-CUDN VRFs and cannot
route to other connected subnets until the upgrade completes.
Cross-CUDN connectivity is only available between pods on upgraded
nodes; the upgrade is complete when all nodes run v1.5.

## Backwards Compatibility

* **Geneve CNC**: No changes. Existing CNC CRs that select Geneve
  CUDNs continue to use the connect-router mechanism.
* **EVPN CUDNs without CNC**: No changes. CUDNs with their own ipVRF
  configuration continue to work as before. CNC is opt-in.
* **CNC API**: The new `evpnConfiguration` field is optional. Existing
  CNC CRs continue to work. When the controller detects EVPN CUDNs,
  it auto-detects the transport and applies the appropriate path
  (connect-router for Geneve, parent VRF for EVPN).
* **CUDNs joining a CNC**: A CUDN's per-CUDN VRF, interfaces, and
  independent EVPN config are untouched. CNC only adds `import vrf`
  directives. Removing the CUDN from CNC removes the imports — the
  CUDN returns to its independent state with no changes.
* **E2E tests**: Existing CNC E2E tests for Geneve must continue to
  pass. New tests are added for EVPN CNC.

## Alternatives

### Alternative 1: CNC-managed shared VRF (moving interfaces)

CNC creates and owns a shared VRF with its own L3 VNI. All connected
CUDNs' interfaces (management port, L2 SVI) are moved from their
per-CUDN VRFs into this shared VRF. The per-CUDN VRFs become empty
shells.

This approach is a natural fit for **L2-only CUDNs** (MAC-VRF only) —
moving MAC-VRF SVIs and management ports into a shared VRF with a single
L3 VNI is the standard EVPN multi-tenancy model and requires no route
leaking or filtering. However, it breaks down when **L3 CUDNs (with
`ipVRF`)** are involved.

**Why rejected:**
* Moving interfaces between VRFs causes traffic disruption
  (milliseconds to seconds) as routes are removed from the old VRF and
  added to the new one. This affects CNC creation, day-2 CUDN
  additions/removals, and CNC deletion.
* For CUDNs with `ipVRF`, the live VRF config diverges from the CUDN
  spec while the CNC is active — the spec's `ipVRF` is not reflected in
  the actual VRF. The admin's per-CUDN L3 VNI and route target choices
  are overridden. External peers filtering on per-CUDN route targets
  lose visibility until reconfigured.
* The child's independent north-south routing via its own L3 VNI is
  lost while CNC is active — all traffic routes through the shared
  VRF's L3 VNI instead.

### Alternative 2: Idempotent VNI reuse (matching ipVRF.vni)

CUDNs are created with matching `ipVRF.vni` values. CNC authorizes the
VNI reuse, allowing both CUDNs to share the same Linux VRF. The existing
VNI uniqueness constraint is relaxed for CNC-selected CUDNs.

**Why rejected:**
* **One-way problem**: CUDN spec is immutable. Unselecting a CUDN from
  CNC leaves it with the same VNI as remaining CUDNs. Without CNC
  authorizing the sharing, VNI uniqueness is violated and CUDNs enter
  error state. The only resolution is deleting a CUDN — disrupting
  workloads. CUDNs cannot be cleanly disconnected.
* **Ordering constraint**: Two CUDNs with matching VNIs cannot coexist
  without CNC already present. The admin must create CNC first — a
  non-obvious ordering requirement.
* **Limited scope**: Only works for the matching `ipVRF.vni` case. Does
  not handle MAC-VRF-only CUDNs (no VNI to match) or CUDNs with
  different VNIs.

### Alternative 3: Direct inter-VRF route leaking via BGP route targets

CNC keeps each CUDN's existing VRF and configures shared BGP route targets
to leak routes between them. Each VRF imports and exports a CNC-managed RT
in addition to its own, causing Type 5 routes from one VRF to appear in
the other's routing table.

**Why rejected:**
* Modifies route targets **on the wire** — BGP peers see the RT changes,
  causing reconvergence and route churn. External peers configured for
  per-CUDN RTs receive unexpected additional routes.
* Full-mesh RT complexity: each CNC adds an RT to every connected VRF.
  With N connected VRFs, each VRF imports routes from all others.
* FRR config changes on live VRFs cause potential traffic disruption
  during BGP reconvergence.
* Does not work for MAC-VRF-only CUDNs (no `ipVRF` = no Type 5 routes
  to leak).

### Alternative 4: Transit VRF with BGP route target leaking

CNC creates a transit VRF (hub) with its own L3 VNI. Each per-CUDN VRF
leaks routes to/from the transit VRF via shared BGP route targets, rather
than directly to each other.

**Why rejected:** Same RT-on-the-wire problems as Alternative 3 (BGP
reconvergence, external peer impact), plus:
* Adds an extra VRF and L3 VNI per CNC.
* More complex FRR configuration per CNC.
* Potential route loops: per-CUDN VRFs export with the shared RT, the
  transit VRF re-exports with the same RT, per-CUDN VRFs may re-import
  their own routes.
* Does not work for MAC-VRF-only CUDNs.

### Alternative 5: Direct `import vrf` between children (no parent VRF)

FRR `import vrf` between child VRFs directly. No parent VRF — each child
imports routes from every other child. Avoids the parent VRF's L3 VNI/VID
overhead.

**Why rejected:**
* `import vrf` requires a `router bgp <ASN> vrf <name>` stanza on each
  child. MAC-VRF-only CUDNs have no FRR BGP stanza — creating one is
  straightforward, but **cross-node routing still fails**. Without an L3
  VNI, a MAC-VRF-only child's VRF cannot participate in EVPN Type 5
  encapsulation. Imported routes from other children only cover local
  pods, not remote pods.
* If `ipVRF` were required on every child to make this work, connecting
  5 MAC-VRF-only CUDNs would require 5 separate L3 VNIs — one per child.
  The parent VRF approach achieves the same connectivity with a single
  shared L3 VNI.

### Alternative 6: Conditional parent VRF

Create the parent VRF only when at least one connected CUDN lacks `ipVRF`.
When all children have `ipVRF`, use direct `import vrf` (Alternative 5).

**Why rejected:** The parent VRF's existence depends on the mix of children.
As CUDNs join and leave, the topology restructures:
* MAC-VRF-only CUDN joins → create parent VRF, switch all children from
  direct imports to parent imports.
* MAC-VRF-only CUDN leaves → optionally tear down parent VRF, switch
  remaining children back to direct imports.
* Each transition changes FRR config across all children. The complexity
  savings (one L3 VNI + VID) do not justify the operational risk and
  implementation complexity.

### Alternative 7: OVN Connect-Router with VXLAN Bridging

Extend the existing OVN connect-router to also program Linux/FRR
constructs — bridge the connect-router's logical ports to the EVPN
fabric by creating VXLAN tunnels for each connect-router port.

**Why rejected:** This would require teaching OVN about EVPN, which
is a much larger change than using FRR's native VRF capabilities.
It would also create a hybrid routing path where some hops are in OVN
and others are in FRR, making debugging difficult. The parent VRF
approach keeps EVPN routing entirely in FRR/Linux where it naturally
belongs.

### Alternative 8: Explicit Tenant/Routing-Domain CRD

Instead of extending CNC, introduce a new "RoutingDomain" or "Tenant"
CRD that multiple CUDNs attach to. The RoutingDomain would own the
shared IP-VRF configuration.

**Why rejected:** This introduces a new API when CNC already expresses
the same intent ("connect these networks"). Adding another CRD creates
confusion about when to use CNC vs RoutingDomain. CNC's existing
semantics — "these networks should be connected" — map directly to
"these networks share routing via a parent IP-VRF" for EVPN. Keeping
CNC as the unified API is simpler for users and aligns with CNC's
design goal of being transport-agnostic.

### Alternative 9: Static routes instead of `import vrf`

Use per-subnet static routes in each VRF (`ip route <subnet> vrf
<parent>`) instead of FRR `import vrf`. Each child VRF gets a static
route for every other connected CUDN's subnet pointing at the parent
VRF. The parent VRF gets static routes pointing back to each child.

**Partially adopted (hybrid).** The final design is a *half-and-half* of
this alternative and `import vrf`, because the two mechanisms solve
different halves of the problem:

* **`import vrf` (control plane) — kept, for populating the parent's
  table.** The parent imports each `ipVRF` child (scoped by a
  `CNC-<PARENT>-IMPORT` route-map) so it learns every connected subnet
  declaratively and re-advertises MAC-VRF-only subnets as EVPN Type 5. A
  pure-static approach would still need `advertise ipv4 unicast` with
  `redistribute static` for cross-node Type 5 routing, so it does not
  actually avoid the advertisement-scope work
  ([#6631](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6631)).
* **Static routes (node datapath) — adopted, for the hub forwarding
  direction.** Children do **not** `import vrf` the parent; instead
  ovnkube-node programs low-metric **override routes** (sibling subnet →
  parent VRF device) in each child table, and **reinject routes** (own
  subnet → child VRF device) in the parent table for MAC-VRF-only
  children. This is exactly the per-subnet static approach of this
  alternative — the "operational churn" objection does not apply because
  the CNC node controller programs and withdraws these routes
  automatically as CUDNs join/leave, with no manual bookkeeping.

The reason the child direction is static rather than `import vrf` is
correctness, not preference: importing the parent into the children leaks
the parent's sibling aggregates over the overlay next-hop and blackholes
same-node traffic (see
[Alternative 5](#alternative-5-direct-import-vrf-between-children-no-parent-vrf)
and [Alternative 11](#alternative-11-default-route-from-child-to-parent-vrf-nexthop-vrf)).
The static override, being a device route into the parent VRF, keeps the
re-lookup local and lets a symmetric-IRB `/32` still win by
longest-prefix match.

### Alternative 10: Veth pairs between child and parent VRFs

Use veth pairs (one per child-parent connection) with link-local IPs
instead of FRR `import vrf` for inter-VRF routing within the parent
VRF design.

**Why rejected:** Adds N veth interfaces per CNC per node with
associated IP address management (link-local pairs per veth). More
complex lifecycle management as CUDNs join/leave. FRR `import vrf`
achieves identical routing semantics with zero extra interfaces and
is already used in the OVN-Kubernetes codebase (RouteAdvertisements
controller).

### Alternative 11: Default route from child to parent VRF (nexthop-vrf)

Instead of bidirectional `import vrf`, child VRFs install a default
route via the parent VRF using the Linux `nexthop-vrf` mechanism:
`ip route 0.0.0.0/0 nexthop-vrf cnc.f61ee1a4`. The child no longer
runs `import vrf <parent>` — unknown destinations are forwarded to the
parent VRF's routing table, which has EVPN-learned routes for all
connected subnets. No IP address is needed on the parent VRF; the
`nexthop-vrf` form redirects the lookup, not the packet to a gateway.

This does eliminate the ECMP feedback loop — the child never imports
parent's routes, so there is no re-import of its own type-5 routes.
The chosen route-map approach solves the same problem with narrower scope.

**Why rejected:**

* **Hides routing structure in the child** — the child's routing table
  has no explicit routes for sibling CUDN subnets, only a catch-all
  default. Operators debugging connectivity cannot inspect which CUDNs
  are reachable; they must look at the parent VRF's table.
* **Breaks same-node same-CUDN traffic** — if the child VRF has a
  default route to the parent and the parent has `import vrf <child>`,
  a packet from a local pod to a remote pod in the same CUDN whose
  /32 host route has not yet been learned locally will hit the default
  route, enter the parent VRF, and may be encapsulated with the parent's
  L3 VNI rather than the child's — incorrect encapsulation for
  same-CUDN cross-node traffic.
* **Asymmetric path selection** — the child uses the parent's type-5
  next-hop (parent L3 VNI) for all cross-node traffic regardless of
  whether the child has its own `ipVRF`. For children with `ipVRF`
  this bypasses the child's own type-5 advertisement and L3 VNI,
  causing traffic to trombone through the parent VRF unnecessarily.
* **Not composable with MAC-VRF-only children** — MAC-VRF-only children
  have no FRR BGP stanza. A default route installed by the node-side
  controller would need separate   lifecycle management outside FRR,
  whereas the chosen design manages everything through the FRR config
  generated by the route advertisements controller.

The chosen design (bidirectional `import vrf` with a route-map on the
child's inbound `import vrf` to deny its own subnets) preserves explicit
routing structure, works uniformly for all child types, and fixes the
feedback loop with minimal scope.

## References

* [#6607: Extend CNC to support EVPN-based UDNs](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6607)
* [PoC branch: `tssurya/ovn-kubernetes` `evpn-cnc-6607`](https://github.com/tssurya/ovn-kubernetes/tree/evpn-cnc-6607) — the datapath-validated implementation this OKEP describes (full-mesh cross-node inter-UDN connectivity across all three CUDN types)
* [#6631: RouteAdvertisements: support ipVRF without external advertisement](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6631)
* [#6557: OKEP: Plexus - Network Orchestration](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6557)
* [OKEP-5088: EVPN Support](okep-5088-evpn.md)
* [OKEP-5224: Connecting UserDefinedNetworks](okep-5224-connecting-udns/okep-5224-connecting-udns.md)
* [OKEP-5296: BGP Integration](okep-5296-bgp.md)
* [Cisco Programmable Fabric Multi-Tenancy](https://www.cisco.com/c/en/us/td/docs/switches/datacenter/pf/configuration/guide/b-pf-configuration/Multi-Tenancy.html)
* [FRR EVPN Configuration Guide](https://docs.frrouting.org/en/latest/evpn.html)
* [FRR VRF Import](https://docs.frrouting.org/en/latest/bgp.html#clicmd-import-vrf-VRFNAME)
* [OVN-native EVPN in shared gateway mode PoC (dceara/ovn-kubernetes `FDP-3311-ovnk-evpn`)](https://github.com/dceara/ovn-kubernetes/commits/FDP-3311-ovnk-evpn/)
