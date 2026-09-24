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
* When connected CUDNs share the CNC's parent IP-VRF, external entities
  that can reach one CUDN's subnet via the EVPN fabric can also reach
  the other connected CUDNs' subnets through the same VRF. This
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
* Support connecting both Layer 2 and Layer 3 EVPN CUDNs via CNC —
  MAC-VRF-only CUDNs (no `ipVRF`) and CUDNs with `ipVRF` are both
  eligible.
* Support connecting CUDNs that reference **different VTEP CRs**
  within the same CNC.
* Allow integration with existing EVPN fabrics by supporting
  admin-specified L3 VNI and route target values on the CNC.

## Future Goals

* Connecting EVPN CUDNs across clusters via CNC (inter-cluster CNC).
* Mixed-transport CNC: connecting a Geneve CUDN to an EVPN CUDN
  within the same CNC (requires bridging the OVN connect-router with
  the EVPN parent VRF).
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
  in this VRF. Named after the CUDN if it fits in 15 chars (e.g. `blue`),
  otherwise ID-derived as `mp<networkID>-udn-vrf` (e.g. `mp3-udn-vrf`).
* The **management port** (`ovn-k8s-mp<N>`) — the gateway between OVN's
  logical switch and the Linux networking stack. Enslaved to the per-CUDN VRF.
* A **MAC-VRF L2 SVI** (`svl2-<network>`, e.g. `svl2-blue`) — a VLAN
  sub-interface on the SVD bridge, mapping to the CUDN's L2 VNI.
  Enslaved to the per-CUDN VRF. Provides ARP suppression and
  L2VNI-to-VRF association. Required for Layer2 topology, forbidden
  for Layer3.
* An **IP-VRF L3 SVI** (`svl3-<network>`, e.g. `svl3-blue`) — a second
  VLAN sub-interface on the SVD bridge for the L3 VNI. Enslaved to the
  same per-CUDN VRF. This turns the VRF into an EVPN IP-VRF with Type 5
  route advertisement for inter-subnet routing via FRR
  (`vrf <name>; vni <L3-VNI>; exit-vrf`). Required for Layer3 topology,
  optional for Layer2.
* The SVD bridge itself is named `evbr-<vtep>` (e.g. `evbr-myvtep`), with
  VXLAN devices `evx4-<vtep>` / `evx6-<vtep>`.
* FRR provides the BGP/EVPN control plane, advertising Type 2 (MAC/IP)
  and Type 5 (IP prefix) routes.

Example — Layer2 CUDN `blue` with MAC-VRF (VNI 100) and IP-VRF
(VNI 301) on VTEP `myvtep`:

```
blue (Linux VRF, table 1001)
├── ovn-k8s-mp1     (management port)
├── svl2-blue       (L2 SVI, VLAN on evbr-myvtep, VNI 100)
└── svl3-blue       (L3 SVI, VLAN on evbr-myvtep, VNI 301)

evbr-myvtep         (SVD bridge)
├── evx4-myvtep     (VXLAN device, IPv4 underlay)
├── svl2-blue       (L2 SVI, VID→VNI 100)
└── svl3-blue       (L3 SVI, VID→VNI 301)
```

Example — Layer3 CUDN `red` with IP-VRF only (VNI 501) on VTEP `myvtep`:

```
red (Linux VRF, table 1002)
├── ovn-k8s-mp2     (management port)
└── svl3-red        (L3 SVI, VLAN on evbr-myvtep, VNI 501)
    No svl2-red — macVRF forbidden for Layer3 topology
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
on the SVD bridge, and an FRR BGP stanza that advertises all
connected subnets as EVPN Type 5 routes. Because the parent VRF
participates in the EVPN fabric, this is not only east-west
connectivity between CUDNs — the parent VRF's Type 5 routes are
visible to all BGP peers, including external routers. External
entities that can reach one connected CUDN's subnet via the EVPN
fabric can also reach the other connected subnets through the shared
parent IP-VRF. This north-south reachability is an inherent
consequence of the parent VRF's participation in EVPN; for subnets
that should remain cluster-internal, see
[North-South Route Advertisement Scope](#north-south-route-advertisement-scope).
**TBD (to be decided during reviews):** Whether this cross-CUDN
external reachability merging (if blue can reach external X, red
also gains that route) is acceptable by default, or whether
`import vrf` must be filtered via route-maps to only leak connected
CUDN subnets. See [validation item 6](#tbd-validation-items).

Inter-VRF routing uses FRR's `import vrf` directive — a local
operation on each node that copies routes from one VRF's routing
table into another. No BGP route target manipulation on the wire,
no interface movement, no downtime.

The parent VRF consumes one additional L3 VNI and one VID per CNC — this
overhead is acknowledged. It is the cost of a consistent design that
supports both L2-only and L3 CUDNs without conditional logic, avoids
requiring each MAC-VRF-only child to carry its own L3 VNI (which would
be N VNIs instead of one), and provides a stable topology that does not
restructure as networks join and leave a CNC. Several alternative approaches
were evaluated and rejected — see [Alternatives](#alternatives) for the
full list and rationale.

```
┌─ child: blue-udn-vrf ────────────┐    ┌─ child: green-udn-vrf ───────────┐
│  ovn-k8s-mpX-blue (mgmt port)    │    │  ovn-k8s-mpX-green (mgmt port)   │
│  svl2-blue (L2 SVI, L2 VNI 100)  │    │  svl2-green (L2 SVI, L2 VNI 200) │
│  svl3-blue (L3 SVI, VNI 301)*    │    │                                  │
│                                  │    │  * no ipVRF — L2-only CUDN       │
│  FRR: import vrf cnc-tenant-vrf  │    │  FRR: import vrf cnc-tenant-vrf  │
│        (sees green's subnet)     │    │        (sees blue's subnet)      │
└──────────────────────────────────┘    └──────────────────────────────────┘
         │                                          │
         │  FRR import vrf (bidirectional)          │
         ▼                                          ▼
┌─ parent: cnc-tenant-vrf (L3 VNI 5000) ───────────────────────────────────┐
│  svl3-cnc-tenant (L3 SVI, VNI 5000)                                      │
│                                                                          │
│  FRR: import vrf blue-udn-vrf    (sees 10.1.0.0/24 from blue)            │
│       import vrf green-udn-vrf   (sees 10.2.0.0/24 from green)           │
│       advertise ipv4 unicast     (Type 5 for all connected subnets)      │
│       vni 5000                                                           │
└──────────────────────────────────────────────────────────────────────────┘
                              * only if ipVRF configured on CUDN
```

**Key properties:**

* **No interface movement** — all interfaces (management ports, L2 SVIs,
  L3 SVIs) remain in their per-CUDN VRFs. Zero downtime.
* **No BGP route target manipulation on the wire** — `import vrf` is a
  local FRR operation. Each child VRF's EVPN config (L3 VNI, RT, Type 5
  advertisements) is completely untouched. External peers see no change
  to per-CUDN routing.
* **Works for all CUDN types** — MAC-VRF-only CUDNs (no `ipVRF`) and
  CUDNs with `ipVRF` are both eligible. The parent VRF provides the L3
  VNI that MAC-VRF-only children lack. This avoids requiring each child
  to have its own L3 VNI just for CNC connectivity.
* **Clean rollback** — removing a CUDN from CNC = remove `import vrf`
  lines from the parent and child FRR stanzas. Deleting the CNC = remove
  all imports, delete the parent VRF, L3 SVI, and VID. Children are
  untouched.
* **Consistent behavior** — the parent VRF is always created with the CNC.
  No conditional logic based on whether children have `ipVRF` or not.

### API Details

#### CNC spec changes

A new optional `evpnConfiguration` field is added to the CNC spec.
When all selected CUDNs are EVPN type, this field configures the
parent VRF. The field reuses the existing `VRFConfig` type for
consistent API semantics:

```go
type ClusterNetworkConnectSpec struct {
    // ... existing fields ...

    // evpnConfiguration configures the parent VRF for connecting
    // EVPN-based CUDNs. Required when the selected CUDNs use EVPN
    // transport. Forbidden when selected CUDNs use Geneve transport.
    // +optional
    EVPNConfiguration *EVPNCNCConfig `json:"evpnConfiguration,omitempty"`
}

type EVPNCNCConfig struct {
    // ipVRF configures the parent VRF's L3 VNI and optional route target.
    // The VNI must be unique across all EVPN VNIs in the cluster (both
    // CUDN VNIs and other CNC VNIs). If routeTarget is not specified,
    // it is auto-generated as "<ASN>:<VNI>".
    // +kubebuilder:validation:Required
    // +required
    IPVRF VRFConfig `json:"ipVRF"`
}
```

Example CNC with EVPN configuration:

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: tenant-acme
spec:
  networkSelector:
    matchLabels:
      tenant: acme
  connectivity:
    - PodNetwork
    - ServiceNetwork
  evpnConfiguration:
    ipVRF:
      vni: 50010
      # routeTarget: "65000:50010"  # optional, auto-derived from ASN:VNI
```

#### `connectSubnets` handling

The existing CNC spec requires `connectSubnets` (currently `+required`,
`MinItems=1`) to provide IPs for the OVN connect-router's patch ports in
Geneve mode. For EVPN CNC, routing is handled by the parent VRF, not OVN
patch ports, so `connectSubnets` is not needed. The field is made optional
when `evpnConfiguration` is present:

```diff
- // +kubebuilder:validation:Required
- // +kubebuilder:validation:MinItems=1
- // +required
- ConnectSubnets []ConnectSubnet `json:"connectSubnets"`
+ // +optional
+ ConnectSubnets []ConnectSubnet `json:"connectSubnets,omitempty"`
```

A CEL validation rule on the spec enforces that at least one of the two
must be set:

```go
// +kubebuilder:validation:XValidation:rule="has(self.connectSubnets) || has(self.evpnConfiguration)",message="either connectSubnets or evpnConfiguration must be specified"
```

This is a backwards-compatible loosening — existing Geneve CNC CRs
already have `connectSubnets` set and continue to pass validation.

#### Validation rules

* `evpnConfiguration` is required when all selected CUDNs are EVPN type.
* `evpnConfiguration` is forbidden when any selected CUDN is Geneve type.
* Mixed transport (some Geneve, some EVPN) is rejected.
* The `ipVRF.vni` must not conflict with any existing CUDN MAC-VRF or
  IP-VRF VNI on the same VTEP, or with any other CNC's parent VRF VNI.
* **VNI uniqueness is not relaxed by CNC.** The existing per-VTEP VNI
  uniqueness validation remains unchanged regardless of whether CUDNs
  are connected by a CNC. The parent VRF already provides shared routing
  — matching child VNIs would be redundant and reintroduce the one-way
  disconnection problem (see [Alternative 2](#alternative-2-idempotent-vni-reuse-matching-ipvrfvni)).
* For day-one scope: all selected CUDNs must reference the same VTEP CR.
  If VTEPs differ, the CNC is rejected with `VTEPMismatch` status condition.

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

The status mirrors the existing CNC status shape: a `status` field
(`Success`/`Failure`) plus standard `conditions`. The parent VRF name
is exposed in status (similar to the VRF name recently added to CUDN
status). The VID (auto-allocated) is communicated to nodes via an
annotation on the CNC resource. Conditions report validation errors
(e.g. `VTEPMismatch`, `VNIConflict`) — the cluster manager cannot
report whether the parent VRF is actually created on nodes since
that is node-side work.

### Same-network vs cross-network traffic separation

CUDNs with their own `ipVRF` keep their independent Type 5 advertisements.
The same subnet is advertised twice via EVPN — once by the child VRF (with
the child's L3 VNI and RT) and once by the parent VRF (with the parent's
L3 VNI and RT). These are different RTs, different VNIs, different VRFs on
remote nodes. No conflict.

* **Same-network cross-node** (blue pod on A → blue pod on B): Uses blue's
  own L3 VNI, resolved entirely within blue's VRF. Unchanged from today.
* **Cross-network cross-node** (blue pod on A → green pod on B): Blue VRF
  has an imported route for green's subnet (from the parent VRF). The
  parent VRF has an EVPN-learned route for green's subnet from node B
  (via the parent's L3 VNI). Traffic is encapsulated with the parent's
  L3 VNI, sent to node B, decapsulated into node B's parent VRF, routed
  to green VRF via imported route, then to the pod via the management port.

**Preventing imported route re-advertisement:** When a child VRF imports
routes from the parent (to enable local cross-VRF routing), those imported
routes must NOT be re-advertised as Type 5 under the child's own L3 VNI
and RT. Without filtering, blue would advertise green's subnet under
blue's RT — leaking routes on the wire, which is exactly what this design
avoids. Each child VRF's FRR stanza uses a route-map on
`advertise ipv4 unicast` to permit only the child's own prefixes:

```
route-map CNC-BLUE-ADVERTISE permit 10
 match ip address prefix-list BLUE-PREFIXES
!
ip prefix-list BLUE-PREFIXES seq 10 permit 10.1.0.0/24
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast route-map CNC-BLUE-ADVERTISE
```

The parent VRF has no such filter — it advertises all imported subnets
under the parent's RT. This is intentional: the parent's Type 5
advertisements provide the unified cross-CNC view.

External peers can choose their view:
* Import a child's RT to see that CUDN's subnet only.
* Import the parent's RT to see all CNC-connected subnets.

### Cross-node cross-network packet walk

**Blue pod on node A → green pod on node B (PodNetwork enabled):**

1. Blue pod sends packet to green pod IP (10.2.0.5).
2. Packet traverses OVN logical switch on node A → exits via blue's
   management port into `blue-udn-vrf`.
3. `blue-udn-vrf` has no local route for 10.2.0.0/24. The imported
   route from `cnc-tenant-vrf` matches.
4. Nexthop resolves in `cnc-tenant-vrf`. The parent VRF has an
   EVPN-learned Type 5 route for 10.2.0.0/24 from node B (via
   parent's L3 VNI 5000).
5. Packet is VXLAN-encapsulated with L3 VNI 5000, sent to node B's
   VTEP IP.
6. Node B decapsulates into `cnc-tenant-vrf`.
7. `cnc-tenant-vrf` has an imported route for 10.2.0.0/24 pointing
   to `green-udn-vrf` (local route, lower admin distance than EVPN).
8. Packet enters `green-udn-vrf` → management port → OVN → green pod.

**Same flow with ServiceNetwork only (no PodNetwork):**

Steps 1-2 differ: OVN Tier0 ACLs intercept the traffic.

1. Blue pod sends packet to ClusterIP 10.96.1.1:80.
2. OVN Tier0 ACL: `ip4.dst==10.96.0.0/16` → **pass** (priority 5000).
   OVN LB performs DNAT: 10.96.1.1:80 → 10.2.0.5:8080 (green pod).
3. Packet exits OVN with destination 10.2.0.5. Steps 3-8 are identical.

If blue pod sends directly to green pod IP 10.2.0.5 (bypassing service):
OVN Tier0 ACL: `ip4.src==$connected_udn_subnets && ip4.dst==$connected_udn_subnets && ct.new`
→ **drop** (priority 4950). Packet never reaches the VRF.

The VRF routing setup is **identical for both connectivity modes**.
The PodNetwork vs ServiceNetwork distinction is enforced entirely by
OVN ACLs on the logical switch, independent of the underlay transport.
This is the same mechanism used for Geneve CNC
([OKEP-5224](okep-5224-connecting-udns/okep-5224-connecting-udns.md#services)).

### Parent VRF lifecycle

The parent VRF is tied to the **CNC lifecycle**, not to individual CUDNs:

* **CNC created** → Parent VRF created with its L3 VNI, VID auto-allocated,
  L3 SVI created on the SVD bridge, FRR BGP stanza generated. Remains for
  the life of the CNC.
* **CUDN joins CNC** (created and matches selector, or label changed to
  match) → `import vrf <child>` added to parent's FRR stanza,
  `import vrf <parent>` added to child's FRR stanza. If the child has no
  existing FRR BGP stanza (MAC-VRF-only), a minimal one is created.
* **CUDN leaves CNC** (deleted, or label changed to no longer match) →
  `import vrf` lines removed from both parent and child stanzas. If the
  child's FRR stanza was created by CNC (MAC-VRF-only case), it is removed.
  The child's per-CUDN VRF and interfaces are untouched.
* **CNC deleted** → All `import vrf` lines removed from all children.
  Parent VRF deleted, L3 SVI removed, VID released. All children return
  to their independent state.

Ordering does not affect steady state:
* *CNC first:* Parent VRF exists before any CUDN. When CUDNs are created
  and match, imports are added.
* *CUDNs first:* CUDNs have their own VRFs. When CNC is created, parent
  VRF is created and imports are added to all matching children.
* *Mixed:* CNC exists with some CUDNs connected. A new CUDN matches — its
  import is added. Existing CUDNs already have imports.

### Multi-VTEP support

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

**Multi-VTEP CNC (requires validation):** The parent VRF needs L3 SVIs
on multiple SVD bridges — one per VTEP referenced by connected CUDNs.
A Linux VRF is a routing table and does not restrict which bridge its
member interfaces belong to, so L3 SVIs from different bridges can
coexist in the same VRF.

```
Example: blue on VTEP-A (evbr-a), green on VTEP-B (evbr-b)

┌────────── cnc-tenant-vrf (L3 VNI 5000) ─────────────────────────┐
│  evbr-a.50 (L3 SVI on VTEP-A's bridge, VNI 5000 via vxlan-a)    │
│  evbr-b.50 (L3 SVI on VTEP-B's bridge, VNI 5000 via vxlan-b)    │
└─────────────────────────────────────────────────────────────────┘
```

The open questions for multi-VTEP:

* The same L3 VNI (5000) is mapped on both `vxlan-a/evbr-a` and
  `vxlan-b/evbr-b`. Since these are separate bridges with separate
  VXLAN devices and separate source IPs, the VNI filter mappings
  do not conflict at the bridge level.
* When FRR installs a Type 5 route with a remote VTEP IP as next-hop,
  the kernel must resolve which VXLAN device to use. Each VXLAN device
  has a different local VTEP IP, so the kernel should select the
  correct device based on FDB entries and underlay routing.
* FRR's SVD model assumes one bridge and one VXLAN device per node.
  Whether FRR and the kernel correctly handle dual L3 SVIs from
  different bridges in the same VRF needs validation.

These items require testing with a multi-VTEP setup before the
single-VTEP restriction can be lifted.

### Implementation Details

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

4. **FRR configuration**: The RouteAdvertisements controller's `rawconfig.go`
   is extended to generate the parent VRF's FRR stanzas:

   ```
   vrf cnc-tenant-acme-vrf
    vni 50010
   exit-vrf
   !
   router bgp 65000 vrf cnc-tenant-acme-vrf
    address-family ipv4 unicast
     import vrf blue-udn-vrf
     import vrf green-udn-vrf
    exit-address-family
    address-family l2vpn evpn
     advertise ipv4 unicast
     advertise ipv6 unicast
    exit-address-family
   exit
   !
   ```

   Each child VRF's FRR stanza gets an `import vrf cnc-tenant-acme-vrf`
   directive added, along with a route-map on `advertise ipv4 unicast`
   to prevent imported routes from being re-advertised under the child's
   own RT. For children with existing `ipVRF`, the route-map is added
   to the existing `advertise` directive. For MAC-VRF-only children
   that have no existing FRR BGP stanza, a minimal one is created:

   ```
   ip prefix-list CNC-GREEN-PREFIXES seq 10 permit 10.2.0.0/24
   !
   route-map CNC-GREEN-ADVERTISE permit 10
    match ip address prefix-list CNC-GREEN-PREFIXES
   !
   router bgp 65000 vrf green-udn-vrf
    address-family ipv4 unicast
     import vrf cnc-tenant-acme-vrf
    exit-address-family
   exit
   !
   ```

   For children with `ipVRF` that already have `advertise ipv4 unicast`:

   ```
   ip prefix-list CNC-BLUE-PREFIXES seq 10 permit 10.1.0.0/24
   !
   route-map CNC-BLUE-ADVERTISE permit 10
    match ip address prefix-list CNC-BLUE-PREFIXES
   !
   router bgp 65000 vrf blue-udn-vrf
    address-family ipv4 unicast
     import vrf cnc-tenant-acme-vrf
    exit-address-family
    address-family l2vpn evpn
     advertise ipv4 unicast route-map CNC-BLUE-ADVERTISE
    exit-address-family
   exit
   !
   ```

5. **VTEP validation**: Initially, the controller verifies all selected
   CUDNs reference the same VTEP CR and rejects with `VTEPMismatch` if
   not. This restriction is lifted once the multi-VTEP validation items
   (see TBD section) are confirmed. With multi-VTEP support, the parent
   VRF gets an L3 SVI on each referenced VTEP's SVD bridge.

#### Node (ovnkube-node)

The node-side parent VRF infrastructure can be managed in one of two ways
(TBD — reviewer input requested):

* **Option A**: Extend the existing EVPN node controller
  (`evpn_node_controller.go`) to also watch CNC resources and manage
  the parent VRF. Natural extension since it already manages per-CUDN
  VRFs and SVIs.
* **Option B**: Create a dedicated CNC node controller that watches CNC
  resources and manages parent VRF lifecycle independently. Cleaner
  separation of concerns but adds a new controller for a small scope.

Either way, the node-side work is:

1. **Parent VRF creation**: A Linux VRF is created for the CNC
   (`cnc-<name>-vrf`) via the existing `vrfManager`.

2. **L3 SVI creation**: An L3 SVI VLAN sub-interface is created on the
   SVD bridge for the parent VRF's L3 VNI, using the allocated VID.
   The SVI is enslaved to the parent VRF. This follows the same pattern
   as per-CUDN L3 SVIs in `reconcileSVIs`.

3. **Cleanup**: When the CNC is deleted, the L3 SVI is removed and the
   parent VRF is deleted.

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

#### TBD: Validation items

The following behaviors need validation before or during implementation:

1. **Transitive `import vrf` nexthop resolution**: When blue imports
   from the parent VRF, and the parent imported green's routes from
   green VRF, does the nexthop chain resolve back to green VRF? If
   green has `ipVRF`, cross-network traffic may resolve through green's
   own L3 VNI rather than the parent's — effectively bypassing the
   parent VRF in the data plane for children with `ipVRF`. This is
   acceptable (the parent VRF's L3 VNI is still needed for MAC-VRF-only
   children and external fabric visibility), but the exact FRR behavior
   for transitive import nexthop resolution must be confirmed.

2. **FRR Type 5 advertisement of imported routes**: Confirm that routes
   imported into the parent VRF via `import vrf` are picked up by
   `advertise ipv4/ipv6 unicast` and correctly advertised as EVPN
   Type 5 routes with the parent VRF's L3 VNI.

3. **Multi-VTEP L3 SVI coexistence**: When the parent VRF has L3 SVIs
   on multiple SVD bridges (one per VTEP), confirm that FRR and the
   kernel correctly handle Type 5 encapsulation and decapsulation
   through the correct VXLAN device. This blocks lifting the
   single-VTEP restriction.

4. **MAC-VRF-only child minimal FRR stanza**: Confirm that adding a
   `router bgp <ASN> vrf <name>` stanza with only `import vrf` (no
   `vni`, no `advertise`) to a VRF that previously had no FRR BGP
   stanza does not interfere with the VRF's existing L2 EVPN behavior
   (Type 2 routes, ARP suppression).

5. **Route-map filtering on child VRF advertisement**: Confirm that
   applying a route-map on `advertise ipv4 unicast` in a child VRF's
   FRR stanza correctly prevents imported routes (from `import vrf`)
   from being re-advertised as Type 5 under the child's L3 VNI/RT,
   while still allowing the child's own prefixes to be advertised.
   Also confirm that adding the route-map to a child VRF that already
   has `advertise ipv4 unicast` (existing ipVRF) does not disrupt its

6. **Selective `import vrf` filtering via route-maps**: `import vrf`
   copies the entire source VRF routing table. Confirm whether FRR
   supports attaching a route-map to `import vrf` to selectively
   import only connected CUDN subnets rather than the full table.
   Without this, connecting two CUDNs via CNC merges their full
   external reachability — if blue can reach external destination X,
   red also gains that route. If route-map filtering on `import vrf`
   is supported, the CNC controller should generate prefix-lists
   scoped to the connected CUDNs' subnets to limit leaked routes.
   current Type 5 advertisements.

6. **Route withdrawal on CNC deletion**: Confirm that removing
   `import vrf` directives causes FRR to withdraw the corresponding
   routes from both the local RIB and the EVPN Type 5 advertisements
   promptly, without stale routes lingering.

### Testing Details

#### Unit Tests

* Parent VRF FRR config generation in `rawconfig.go` — verify correct
  `vrf`, `vni`, `import vrf`, `advertise` stanzas for various
  combinations of MAC-VRF-only and IP-VRF children.
* VNI reservation conflict detection — parent VRF VNI conflicts with
  CUDN VNIs.
* VID allocation for CNC — keyed correctly, released on CNC deletion.
* Transport detection — Geneve, EVPN, mixed rejection.
* VTEP validation — same VTEP passes, different VTEPs rejected.
* FRR import vrf additions/removals as CUDNs join/leave CNC.
* MAC-VRF-only children get minimal FRR stanza; stanza removed on
  CNC leave.

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

**Resource overhead per CNC:**
* One Linux VRF (kernel routing table).
* One L3 SVI (VLAN sub-interface on SVD bridge) per node.
* One VID consumed from the 4094 pool per CNC.
* One L3 VNI consumed per CNC.
* One FRR BGP stanza per CNC per node.
* N `import vrf` directives (one per connected CUDN) in the parent
  stanza, plus one `import vrf` in each child stanza.

**FRR route table impact:** Each `import vrf` directive copies the
source VRF's entire routing table into the destination VRF. The parent
VRF's routing table contains one entry per connected CUDN's subnet,
plus EVPN-learned routes from remote nodes. At practical scale (tens of
CUDNs per CNC, tens of nodes), this is hundreds of routes — well
within FRR's capacity. At extreme scale (hundreds of CUDNs per CNC),
FRR memory usage and route processing time increase; documentation
recommends limiting CNC fan-out to tens of CUDNs.

**EVPN Type 5 advertisement:** The parent VRF advertises all connected
subnets as Type 5 routes. With N connected CUDNs on M nodes, this is
N*M Type 5 routes exchanged via BGP. The BGP control plane overhead
scales linearly and is comparable to having N individual IP-VRFs.

**Data plane:** Cross-CNC traffic traverses two VRF lookups (child →
parent or parent → child). These are kernel routing table lookups —
O(1) with hash-based FIB — and add negligible latency.

**VID exhaustion:** Each CNC parent VRF consumes one VID per SVD bridge
(VTEP) where it has connected CUDNs. With multiple VTEPs, VID usage
scales as CNCs × VTEPs. Combined with per-CUDN MAC-VRF and IP-VRF VIDs,
the 4094 VID pool is shared. At practical scale (tens of CUDNs, handful
of CNCs, and a small number of VTEPs), exhaustion is unlikely, but
deployments with many VTEPs and many CNCs should account for this. If
needed, the SVD bridge can be extended with multiple bridge/VTEP pairs
(per OKEP-5088 discussion).

**BUM traffic:** CNC does not increase BUM (Broadcast, Unknown unicast,
Multicast) traffic. Each CUDN's L2 domain (MAC-VRF) is unchanged.
Cross-CUDN traffic is L3-routed through the parent VRF, not bridged.

## Risks, Known Limitations and Mitigations

### North-South Route Advertisement Scope

The parent VRF's `advertise ipv4 unicast` in its FRR BGP stanza
causes all connected subnets' Type 5 routes to be advertised to
**all** BGP peers — both the cluster-internal iBGP mesh (other nodes,
route reflector) and any external peers (eBGP or otherwise). This is
required: `import vrf` alone only provides local (same-node) route
leaking; Type 5 advertisement is what makes cross-node routing work
via the EVPN fabric.

The consequences are twofold:

1. **Inbound**: External routers learn routes to all connected subnets
   via the parent VRF's Type 5 advertisements. For subnets that should
   remain cluster-internal (private subnets), this is undesirable.

2. **Outbound / cross-CUDN route merging**: `import vrf` copies the
   entire routing table, not just connected subnets. If blue has a
   route to external destination X (learned via BGP from an external
   peer), that route is imported into the parent VRF, and from there
   into red. Red can now reach X through blue's external path — even
   though red was never explicitly given access to X. CNC effectively
   merges the external reachability of all connected CUDNs. Whether
   this is acceptable depends on the deployment's trust model between
   connected networks.

Today, there is no mechanism in OVN-Kubernetes to selectively scope
Type 5 advertisement to internal-only peers. FRR could attach a
`no-export` BGP community via route-map to prevent propagation
beyond the iBGP mesh, but the RouteAdvertisements controller does
not generate this configuration.

**Mitigation**: This is tracked separately in
[#6631](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6631).
Until that issue is resolved, admins must configure route-maps on
external peers to filter private subnet prefixes. The parent VRF's
route target can be used as a filter criterion on external peers.

### External Peer Route Target Awareness

The parent VRF introduces a new route target that external peers
must be aware of to receive the CNC's aggregated routes. External
peers configured for per-CUDN route targets continue to work —
per-CUDN Type 5 advertisements are untouched.

**Mitigation**: The CNC status exposes the parent VRF's route target
so admins can configure external peers. For Plexus-managed ANDs,
the controller handles this automatically.

### Local Gateway Mode Only

EVPN CNC is limited to local gateway mode. Shared gateway mode is not
supported because EVPN requires Linux kernel networking primitives that
shared gateway mode bypasses.

**Mitigation**: This limitation is inherent to EVPN in OVN-Kubernetes
(per OKEP-5088) and not specific to CNC.

### Multi-VTEP Validation

Connecting CUDNs that reference different VTEP CRs requires the parent
VRF to have L3 SVIs on multiple SVD bridges. This is a non-standard
configuration in EVPN (datacenter designs assume one VTEP per leaf
switch). Whether FRR and the kernel correctly handle this needs
validation before the single-VTEP restriction can be lifted.

**Mitigation**: Day-one scope requires all connected CUDNs to reference
the same VTEP CR. Multi-VTEP is in goals and will be enabled after
testing confirms correctness.

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

**Why rejected:** Static routes must be added and removed on every node
as CUDNs join or leave a CNC — more operational churn than `import vrf`
which is declarative. The parent VRF still needs `advertise ipv4
unicast` with `redistribute static` for cross-node EVPN Type 5
routing, so the north-south advertisement scope issue
([#6631](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6631))
remains. The only advantage is avoiding full route table copies between
VRFs, but at practical scale (tens of CUDNs) the route table size from
`import vrf` is not a concern. `import vrf` is simpler, already used in
the OVN-Kubernetes codebase, and avoids per-subnet bookkeeping.

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

## References

* [#6607: Extend CNC to support EVPN-based UDNs](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6607)
* [#6631: RouteAdvertisements: support ipVRF without external advertisement](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6631)
* [#6557: OKEP: Plexus - Network Orchestration](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6557)
* [OKEP-5088: EVPN Support](okep-5088-evpn.md)
* [OKEP-5224: Connecting UserDefinedNetworks](okep-5224-connecting-udns/okep-5224-connecting-udns.md)
* [OKEP-5296: BGP Integration](okep-5296-bgp.md)
* [Cisco Programmable Fabric Multi-Tenancy](https://www.cisco.com/c/en/us/td/docs/switches/datacenter/pf/configuration/guide/b-pf-configuration/Multi-Tenancy.html)
* [FRR EVPN Configuration Guide](https://docs.frrouting.org/en/latest/evpn.html)
* [FRR VRF Import](https://docs.frrouting.org/en/latest/bgp.html#clicmd-import-vrf-VRFNAME)
