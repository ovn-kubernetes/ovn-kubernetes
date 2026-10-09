# OKEP-7040: BGP NodeSelector for PodNetwork RouteAdvertisements

* Issue: [#7040](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/7040)

## Problem Statement

The `RouteAdvertisements` CRD currently requires `nodeSelector` to select all
nodes when `PodNetwork` is included in `advertisements`. This prevents users
from designating a subset of nodes as BGP gateway nodes for pod network route
advertisements. Users need the ability to restrict BGP peering to specific
gateway nodes while maintaining full pod reachability from the external network.

## Goals

* Allow `RouteAdvertisements.spec.nodeSelector` to select a subset of **BGP gateway** nodes when
  `PodNetwork` is advertised, for the **default cluster network** and for
  **primary Layer 2 and Layer 3** cluster user-defined networks (CUDNs).
* Gateway nodes advertise pod subnets for **all** nodes so pods on **non-gateway** nodes remain
  reachable from the provider network and can egress to BGP-learned
  destinations via gateway nodes.
* Support ingress (external → pod) and egress (pod → BGP-learned external)
  through gateway nodes in **shared gateway (SGW)** and **local gateway (LGW)**
  modes with **overlay (Geneve)** and **EVPN (VXLAN)** transports.
* Provide ECMP load-balancing and high availability across multiple gateway
  nodes.
* **Support IPv4, IPv6, and dual-stack networks** for both ingress and egress
  gateway reroute paths, BGP prefix advertisements, and OVS flows.
* Preserve `EgressIP` `nodeSelector` expected behavior when combined with
  `PodNetwork` `nodeSelector` selection.
* **Non-gateway nodes do not require FRR** for `PodNetwork` advertisement on
  the selected network.
* Preserve today's behavior when `nodeSelector` is empty.

## Non-Goals

* Support for no-overlay transport with `nodeSelector` for `PodNetwork` — there
  is no tunnel backplane to forward remote pod traffic.
* Replacing or modifying the existing EgressIP reroute mechanism.
* Automatic scheduling of workloads on gateway nodes — users may schedule pods
  on any node.

## Introduction

The original BGP OKEP ([OKEP-5296](okep-5296-bgp.md)) listed “Allow selecting
only a subset of nodes to advertise BGP” as a future goal. This OKEP fulfills
that goal for `PodNetwork` advertisements.

In many datacenter deployments, only a subset of nodes are connected to BGP
peers on the provider network. These “gateway nodes” act as the entry and exit
points for north/south traffic. Today, the `RouteAdvertisements` CRD enforces
that `PodNetwork` must be advertised on all nodes, which prevents this
deployment pattern.

When only gateway nodes peer with external BGP routers, two traffic forwarding
challenges arise:

1. **Ingress**: External traffic for a pod on a non-gateway node arrives at a
   gateway node. The gateway node must forward it to the correct node through
   the tunnel overlay (Geneve or VXLAN).

2. **Egress**: A pod on a non-gateway node needs to reach an external
   destination whose route was learned via BGP. Only gateway nodes have these
   BGP-learned routes, so the pod's traffic must be steered through a gateway
   node.

Both directions require data plane changes in addition to control plane
(RouteAdvertisements controller) changes.

## User-Stories/Use-Cases

### Story 1: Dedicated BGP gateway nodes

As a cluster admin, I want to designate specific nodes as BGP gateways so that
only those nodes peer with my provider network's BGP routers, while pods on any
node in the cluster remain reachable from the external network.

For example: In a 10-node cluster, only 2 nodes are connected to the spine
switches via BGP. These 2 gateway nodes advertise all pod subnets, and external
traffic enters/exits the cluster through them.

### Story 2: High availability with multiple gateways

As a cluster admin, I want to select multiple gateway nodes so that if one
gateway fails, traffic is automatically rerouted to the remaining gateways via
ECMP, with optional BFD for fast failover detection.

### Story 3: UDN pod network with gateway nodes

As a cluster admin, I want to advertise a Layer3 ClusterUserDefinedNetwork's pod
subnets through designated gateway nodes, with each UDN's routes advertised in
its corresponding VRF.

### Story 4: Primary L2 CUDN through gateways

As a cluster admin, I want Layer 2 primary CUDN `PodNetwork` reachability when
only gateway nodes peer with BGP: gateways advertise the **cluster subnet** and
terminate north-south ingress on br-ex; remote pods are reached via Geneve to
the correct logical switch.

### Story 5: Multiple provider BGP peers for the same network

As a cluster admin, I want to peer different gateway nodes with different
provider BGP routers for the same network, using multiple `RouteAdvertisements`
objects. Each `RouteAdvertisements` selects a different (or overlapping) set of
gateway nodes via `nodeSelector` and uses a distinct `frrConfigurationSelector`
to peer with different external routers. FRR-k8s merges the configurations on
each node, and the combined gateway set provides ECMP across all peers.

For example: In a multi-ToR (Top-of-Rack) deployment, `RouteAdvertisements` RA1
selects nodes in rack A (peering with ToR-A) and RA2 selects nodes in rack B
(peering with ToR-B). Both advertise the default network's pod subnets, providing
redundant paths to the external network.

### Story 6: EgressIP continues to work with gateway `nodeSelector`

As a cluster admin, I use EgressIP with its own node selection while also using
`RouteAdvertisements` with a `nodeSelector` for `PodNetwork`. EgressIP
reroute and no-reroute policies must keep working for affected pods; EgressIP
must not be broken by gateway reroute policies.

## Proposed Solution

### API Details

#### Remove the CEL validation rule

Remove the existing CEL validation on `RouteAdvertisementsSpec` that blocks
`nodeSelector` with `PodNetwork`:

```
// REMOVE:
// +kubebuilder:validation:XValidation:rule="(!has(self.nodeSelector.matchLabels) && !has(self.nodeSelector.matchExpressions)) || !('PodNetwork' in self.advertisements)"
// message="If 'PodNetwork' is selected for advertisement, a 'nodeSelector' can't be specified as it needs to be advertised on all nodes"
```

No new API fields are required. The existing `nodeSelector` field gains
additional semantics when used with `PodNetwork`.

#### Runtime validation

A controller-level validation rejects `nodeSelector` with `PodNetwork` when any
selected network uses `transport: NoOverlay`:

```
"nodeSelector is not supported for PodNetwork when selected networks use
NoOverlay transport"
```

This is enforced at the controller level (not CEL) because it requires
cross-resource validation — the transport is determined from the selected
network's configuration, not from the `RouteAdvertisements` spec itself.

* If `nodeSelector` matches **zero** ready nodes, `RouteAdvertisements` status
  is degraded or pending until at least one gateway node exists; no
  `FRRConfiguration` objects are generated in that state.
* If a selected gateway node has no matching `FRRConfiguration` (per
  `frrConfigurationSelector`), surface an error in `RouteAdvertisements`
  status for that node.
* **Multiple `RouteAdvertisements` may now select the same network** (this relaxes
  the OKEP-5296 conflict rule). Each `RouteAdvertisements` generates its own
  per-node `FRRConfiguration` objects (labeled per-RA); FRR-k8s merges them
  without conflict. When multiple `RouteAdvertisements` select the same network
  with different (or overlapping) `nodeSelector` values, the effective gateway
  set is the **union** of all selected nodes, and reroute policies use the
  **combined transit switch IPs** as nexthops for ECMP. Operators may
  intentionally configure overlapping gateway sets when peering with different
  provider BGP routers via distinct `frrConfigurationSelector` values.

### Scope matrix

| Network | Topology | Transport | `nodeSelector` + `PodNetwork` |
|---------|----------|-----------|-------------------------------|
| Default cluster network | L3 | Geneve (overlay) | **Supported** |
| Default cluster network | — | NoOverlay | **Rejected** |
| Primary L3 CUDN | Layer3 | Overlay | **Supported** |
| Primary L3 CUDN | Layer3 | NoOverlay | **Rejected** |
| Primary L3 CUDN | Layer3 | EVPN | **Supported** |
| Primary L2 CUDN | Layer2 | Overlay | **Supported** (cluster subnet on gateways) |
| Primary L2 CUDN | Layer2 | EVPN | **Supported** (cluster subnet on gateways) |
| Primary L2 CUDN | Layer2 | NoOverlay | **Rejected** |

**IP family support**: All "Supported" configurations work with **IPv4-only,
IPv6-only, and dual-stack (IPv4+IPv6)** networks. For dual-stack networks:
- Gateway nodes advertise both IPv4 and IPv6 prefixes to BGP peers.
- Ingress flows (breth0 OVS flows) are created for both IPv4 and IPv6 remote subnets.
- Egress reroute policies are created separately for IPv4 (`ip4.src`) and IPv6
  (`ip6.src`) with IP-family-matched nexthops.
- BFD sessions (if configured) are established for both IPv4 and IPv6 gateway nexthops.

**Router placement for reroute policies** (varies by network type):
- **Default cluster network**: Policies on `ovn_cluster_router`
- **Primary Layer 3 UDN**: Policies on `ovn_cluster_router_<network-name>`
- **Primary Layer 2 UDN**: Policies on per-node gateway routers `GR_<node>_<network>`

#### EVPN (VXLAN) considerations

EVPN CUDNs use the same gateway pattern as Geneve overlay: gateways advertise
prefixes and program br-ex; non-gateway nodes use reroute policies on the
**EVPN network's router** (either `ovn_cluster_router_<network>` for Layer 3 or
`GR_<node>_<network>` for Layer 2) to gateway transit IPs. Implementation must
align EVPN-specific behavior from
[OKEP-5088](okep-5088-evpn.md) with gateway-only FRR — in particular,
separating **network-level** `PodNetwork` advertisement (SNAT, global semantics)
from **per-node** gateway membership. Today `IsPodNetworkAdvertisedAtNode` can
report advertised for all nodes on EVPN networks; that logic is extended so
only gateway nodes run FRR and install north-south br-ex flows while non-gateway
pods retain correct pod-IP egress after reroute. VTEP and IP-VRF/MAC-VRF
advertisement rules follow the EVPN OKEP on **gateway** nodes only.

### Implementation Details

#### Control Plane: RouteAdvertisements Controller

**File:** `go-controller/pkg/clustermanager/routeadvertisements/controller.go`

When `nodeSelector` is non-empty and `PodNetwork` is in `advertisements`:

1. Remove the runtime guard at `controller.go:628-630` that rejects non-empty
   `nodeSelector` with `PodNetwork`.
2. **Remove cross-RA same-network conflict detection**: multiple
   `RouteAdvertisements` may now select the same network. Each RA generates
   independently labeled `FRRConfiguration` objects; FRR-k8s merges them on
   each node.
3. Add the no-overlay validation: reject if any selected network has
   `transport: NoOverlay`.
4. Change route generation for Layer3 networks: each selected gateway node
   advertises the host subnets of **all** nodes (not just its own). **For
   dual-stack networks, advertise both IPv4 and IPv6 subnets.** For Layer2
   networks, no change is needed — all nodes already advertise the cluster-wide
   network subnet.
5. **Union gateway set for reroute policies**: when multiple `RouteAdvertisements`
   select the same network, compute the **union** of all selected gateway nodes
   across all RAs for that network. **Place policies on the correct router for
   each network type**:
   * Default cluster network → `ovn_cluster_router`
   * Primary Layer 3 UDN → `ovn_cluster_router_<network-name>`
   * Primary Layer 2 UDN → per-node gateway routers `GR_<node>_<network>`
   
   **For dual-stack, create separate IPv4 and IPv6 reroute policies** (OVN does
   not support mixed IP family nexthops in a single policy). Each policy uses the
   **combined transit switch IPs** as nexthops for its respective IP family
   (specific to each network's transit switch).
6. On **node add**:
   * **Layer 3 networks**: allocate host subnet (IPv4 and/or IPv6), add that
     node's subnet prefixes to every gateway's advertisement and br-ex remote
     flow set (both IPv4 and IPv6 flows); add reroute policy rows (IPv4 and IPv6
     policies) **to the network's router** (as described in step 5) for that
     subnet if the node is non-gateway.
   * **Layer 2 networks**: add br-ex remote flows for the cluster-wide subnet on
     gateway nodes; add reroute policy to the non-gateway node's own GR
     (`GR_<node>_<network>`) for the cluster-wide subnet.
7. On **node delete**:
   * **Layer 3 networks**: withdraw that node's subnet prefixes from gateways
     (IPv4 and IPv6) and remove reroute policies for that subnet.
   * **Layer 2 networks**: remove br-ex flows and reroute policies for the
     deleted node; cluster-wide subnet advertisement continues on remaining nodes.

The generated per-node `FRRConfiguration` for a gateway node will contain
prefixes for all nodes' host subnets **(Layer 3 networks)** or the cluster-wide
network subnet **(Layer 2 networks)**. **For dual-stack networks, both IPv4 and
IPv6 prefixes are advertised:**

```yaml
# Generated for gateway node ovn-worker (Layer 3 dual-stack example):
spec:
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: ovn-worker
  bgp:
    routers:
      - asn: 64512
        prefixes:
          # IPv4 subnets for all nodes:
          - 10.244.0.0/24           # ovn-worker3's IPv4 subnet
          - 10.244.1.0/24           # ovn-control-plane's IPv4 subnet
          - 10.244.2.0/24           # ovn-worker's own IPv4 subnet
          - 10.244.3.0/24           # ovn-worker2's IPv4 subnet
          # IPv6 subnets for all nodes:
          - fd01:0:0:1::/64         # ovn-worker3's IPv6 subnet
          - fd01:0:0:2::/64         # ovn-control-plane's IPv6 subnet
          - fd01:0:0:3::/64         # ovn-worker's own IPv6 subnet
          - fd01:0:0:4::/64         # ovn-worker2's IPv6 subnet
        neighbors:
          - asn: 64513
            address: 192.168.1.1    # IPv4 BGP peer
            toAdvertise:
              allowed:
                prefixes:
                  - 10.244.0.0/24
                  - 10.244.1.0/24
                  - 10.244.2.0/24
                  - 10.244.3.0/24
          - asn: 64513
            address: fd99::1        # IPv6 BGP peer
            toAdvertise:
              allowed:
                prefixes:
                  - fd01:0:0:1::/64
                  - fd01:0:0:2::/64
                  - fd01:0:0:3::/64
                  - fd01:0:0:4::/64
```

#### Data Plane: Ingress (External → Pod on non-gateway node)

External traffic for a remote pod arrives at the gateway node. The gateway node
must forward it to the correct node via the tunnel overlay. **For dual-stack
networks, flows are created for both IPv4 and IPv6 subnets.**

##### Shared Gateway Mode (SGW)

Traffic enters breth0 on the gateway node. OVS flows must steer traffic for
**all** advertised pod subnets into the OVN pipeline via the patch port, not
just the gateway node's own subnet.

```
# Existing (own subnet):
cookie=0xdeff105, priority=300,ip,in_port=1,nw_dst=10.244.2.0/24 actions=output:2
cookie=0xdeff105, priority=300,ip6,in_port=1,ipv6_dst=fd01:0:0:3::/64 actions=output:2

# New (remote subnets) - IPv4:
cookie=0xdeff105, priority=300,ip,in_port=1,nw_dst=10.244.0.0/24 actions=output:2
cookie=0xdeff105, priority=300,ip,in_port=1,nw_dst=10.244.1.0/24 actions=output:2
cookie=0xdeff105, priority=300,ip,in_port=1,nw_dst=10.244.3.0/24 actions=output:2

# New (remote subnets) - IPv6:
cookie=0xdeff105, priority=300,ip6,in_port=1,ipv6_dst=fd01:0:0:1::/64 actions=output:2
cookie=0xdeff105, priority=300,ip6,in_port=1,ipv6_dst=fd01:0:0:2::/64 actions=output:2
cookie=0xdeff105, priority=300,ip6,in_port=1,ipv6_dst=fd01:0:0:4::/64 actions=output:2
```

Once inside OVN, the network's router (`ovn_cluster_router` for default network,
`ovn_cluster_router_<network>` for L3 UDN, or `GR_<node>_<network>` for L2 UDN)
has routes for all node subnets (both IPv4 and IPv6) and forwards via Geneve
tunnel to the correct node.

##### Local Gateway Mode (LGW)

Traffic enters breth0 and is sent to the host kernel via `actions=LOCAL`. The
host already has aggregate routes (`10.244.0.0/16 via <mp0-gw> dev ovn-k8s-mp0`
for IPv4 and `fd01::/48 via <mp0-gw-v6> dev ovn-k8s-mp0` for IPv6) that cover
all pod subnets. Only the breth0 OVS flows need to be added for remote subnets:

```
# Existing (own subnet):
cookie=0xdeff105, priority=300,ip,in_port=eth0,nw_dst=10.244.2.0/24 actions=LOCAL
cookie=0xdeff105, priority=300,ip6,in_port=eth0,ipv6_dst=fd01:0:0:3::/64 actions=LOCAL

# New (remote subnets) - IPv4:
cookie=0xdeff105, priority=300,ip,in_port=eth0,nw_dst=10.244.0.0/24 actions=LOCAL
cookie=0xdeff105, priority=300,ip,in_port=eth0,nw_dst=10.244.1.0/24 actions=LOCAL
cookie=0xdeff105, priority=300,ip,in_port=eth0,nw_dst=10.244.3.0/24 actions=LOCAL

# New (remote subnets) - IPv6:
cookie=0xdeff105, priority=300,ip6,in_port=eth0,ipv6_dst=fd01:0:0:1::/64 actions=LOCAL
cookie=0xdeff105, priority=300,ip6,in_port=eth0,ipv6_dst=fd01:0:0:2::/64 actions=LOCAL
cookie=0xdeff105, priority=300,ip6,in_port=eth0,ipv6_dst=fd01:0:0:4::/64 actions=LOCAL
```

Traffic then follows: `breth0 → LOCAL → host kernel → 10.244.0.0/16 via mp0 (or
fd01::/48 via mp0 for IPv6) → OVN network router (ovn_cluster_router for default
network, ovn_cluster_router_<network> for L3 UDN, or GR_<node>_<network> for
L2 UDN) → Geneve tunnel → remote node → pod`.

#### Data Plane: Egress (Pod on non-gateway node → External)

Pods on non-gateway nodes need to reach external destinations whose routes are
only known to gateway nodes (BGP-learned routes). Traffic must be steered
through a gateway node using OVN logical router reroute policies on **the router
that carries each network's default route**:

- **Default cluster network**: `ovn_cluster_router`
- **Primary Layer 3 UDN**: `ovn_cluster_router_<network-name>` (the network-specific
  cluster router)
- **Primary Layer 2 UDN**: Per-node gateway routers `GR_<node-name>_<network-name>`
  (each non-gateway node's GR gets a reroute policy)

This uses the same mechanism already proven by EgressIP. **For dual-stack
networks, both IPv4 and IPv6 reroute policies are created on each router:**

```
# Example 1: Default cluster network (on ovn_cluster_router)
# For each non-gateway node's pod subnet:

# IPv4 reroute policy:
LogicalRouterPolicy {
    action:   reroute
    match:    "ip4.src == 10.244.0.0/24"         # non-gateway node's IPv4 pod subnet
    nexthops: ["100.88.0.2", "100.88.0.3"]       # IPv4 transit switch IPs of gateway nodes
    priority: 99                                 # Below EIP reroute (100) to ensure EIP takes precedence
}

# IPv6 reroute policy:
LogicalRouterPolicy {
    action:   reroute
    match:    "ip6.src == fd01:0:0:1::/64"       # non-gateway node's IPv6 pod subnet
    nexthops: ["fd98::2", "fd98::3"]             # IPv6 transit switch IPs of gateway nodes
    priority: 99                                 # Below EIP reroute (100) to ensure EIP takes precedence
}


# Example 2: Primary Layer 3 UDN (on ovn_cluster_router_blue-network)
# For each non-gateway node's pod subnet on the UDN:

LogicalRouterPolicy {
    action:   reroute
    match:    "ip4.src == 10.100.0.0/24"         # non-gateway node's UDN pod subnet
    nexthops: ["10.200.0.2", "10.200.0.3"]       # UDN transit switch IPs of gateway nodes
    priority: 99
}


# Example 3: Primary Layer 2 UDN (on GR_ovn-worker3_red-network)
# For non-gateway node ovn-worker3 on Layer 2 UDN "red-network":

LogicalRouterPolicy {
    action:   reroute
    match:    "ip4.src == 10.150.0.0/24"         # ovn-worker3's L2 UDN pod subnet
    nexthops: ["10.250.0.2", "10.250.0.3"]       # L2 UDN gateway nodes' GR IPs
    priority: 99
}
```

In an IC (interconnect) cluster, nexthops are **transit switch port IPs** of the
gateway nodes. In a non-IC cluster, nexthops would be the **join switch IPs**.
For **dual-stack networks**, reroute policies include both IPv4 and IPv6 nexthops
in separate policies (OVN does not support mixed IP family nexthops in a single
policy).

Existing higher-priority policies protect intra-cluster traffic (OVN evaluates
policies from highest to lowest priority number):

| Priority | Policy                             | Effect                                                               |
|----------|------------------------------------|----------------------------------------------------------------------|
| 1004     | Node subnet reroute                | Pod → own node IP via mgmt port                                      |
| 102      | EIP no-reroute: Pod-to-Pod (IPv4)  | `ip4.src == 10.244.0.0/16 && ip4.dst == 10.244.0.0/16` → allow       |
| 102      | EIP no-reroute: Pod-to-Pod (IPv6)  | `ip6.src == fd01::/48 && ip6.dst == fd01::/48` → allow               |
| 102      | EIP no-reroute: Pod-to-Join (IPv4) | `ip4.src == 10.244.0.0/16 && ip4.dst == 100.64.0.0/16` → allow       |
| 102      | EIP no-reroute: Pod-to-Join (IPv6) | `ip6.src == fd01::/48 && ip6.dst == fd97::/64` → allow               |
| 102      | EIP no-reroute: Pod-to-Node        | Pod subnets → node IPs → allow (both IPv4 and IPv6)                  |
| 100      | EIP reroute                        | Pods with EgressIP → EgressIP node                                   |
| **99**   | **BGP gateway reroute (NEW)**      | Non-gateway pod subnets → gateway transit IPs (IPv4 and IPv6 separate) |

Only N/S (external-bound) traffic falls through to priority 99 BGP gateway
reroute. E/W traffic and EgressIP-selected pods are handled at higher priorities
and are never rerouted to BGP gateways.

Multiple nexthops provide **ECMP** load-balancing across gateway nodes. BFD
sessions can be configured for fast failover. **For dual-stack, BFD sessions
are created for both IPv4 and IPv6 nexthops:**

```
# IPv4 reroute with BFD:
LogicalRouterPolicy {
    action:      reroute
    match:       "ip4.src == 10.244.0.0/24"
    nexthops:    ["100.88.0.2", "100.88.0.3"]
    bfd_sessions: [<bfd-ipv4-uuid-1>, <bfd-ipv4-uuid-2>]
    priority:    99
}

# IPv6 reroute with BFD:
LogicalRouterPolicy {
    action:      reroute
    match:       "ip6.src == fd01:0:0:1::/64"
    nexthops:    ["fd98::2", "fd98::3"]
    bfd_sessions: [<bfd-ipv6-uuid-1>, <bfd-ipv6-uuid-2>]
    priority:    99
}
```

#### SNAT Behavior

When a network is advertised via BGP, SNAT is already conditionally disabled
(as documented in the existing BGP implementation). This remains unchanged —
pod traffic exits with the pod IP as source, which is the expected behavior for
advertised subnets.

#### Traffic Flow Summary

For a 4-node dual-stack cluster where ovn-worker and ovn-worker2 are gateway nodes:

```
INGRESS (external → pod on ovn-worker3 on default network):

  external
    │
    ▼
  gateway node (ovn-worker)
    breth0 → flow matches 10.244.0.0/24 (IPv4) or fd01:0:0:1::/64 (IPv6)
    │
    SGW: → patch port → OVN pipeline
    LGW: → LOCAL → host kernel → mp0 → OVN pipeline
    │
    ▼
  ovn_cluster_router (for default network; ovn_cluster_router_<network> for L3 UDN,
                      or GR_<node>_<network> for L2 UDN)
    → Geneve tunnel → ovn-worker3 → pod


EGRESS (pod on ovn-worker3 on default network → BGP-learned external destination):

  pod on ovn-worker3
    │
    ▼
  ovn_cluster_router (for default network; would be ovn_cluster_router_<network>
                      for L3 UDN, or GR_ovn-worker3_<network> for L2 UDN)
    │
    IPv4 traffic: reroute policy: ip4.src == 10.244.0.0/24
                  nexthops: [100.88.0.2, 100.88.0.3]
    IPv6 traffic: reroute policy: ip6.src == fd01:0:0:1::/64
                  nexthops: [fd98::2, fd98::3]
    │
    ▼ (Geneve tunnel)
  gateway node (ovn-worker)
    GR_ovn-worker → breth0 → FRR BGP route → external
```

#### Transport Support Matrix

| Transport        | NodeSelector + PodNetwork | Reason                                          |
|------------------|---------------------------|-------------------------------------------------|
| Overlay (Geneve) | Supported                 | Geneve tunnels provide forwarding path          |
| EVPN (VXLAN)     | Supported                 | VXLAN tunnels provide forwarding path           |
| NoOverlay        | Blocked                   | No tunnel backplane for remote pod forwarding   |

### SGW vs LGW Differences

| Aspect                         | SGW                            | LGW                                                                                           |
|--------------------------------|--------------------------------|-----------------------------------------------------------------------------------------------|
| Ingress breth0 flows           | `actions=output:<patch-port>`  | `actions=LOCAL`                                                                               |
| Host routes for remote subnets | Not needed (stays in OVS/OVN)  | Already covered by aggregate routes (IPv4: `10.244.0.0/16 via mp0`, IPv6: `fd01::/48 via mp0`) |
| Egress reroute                 | OVN LRP on network router      | Same — reroute happens before SGW/LGW split (both IPv4 and IPv6 policies); router varies by network type |
| SNAT handling                  | Conditional SNAT on GR         | Conditional SNAT on GR + host nftables (both IPv4 and IPv6)                                  |

#### EgressIP compatibility

* Behavior of EgressIP with **nodeSelector** is unchanged.
* EgressIP reroute (priority 100) takes precedence over BGP gateway reroute
  (priority 99) for pods selected by EgressIP; east-west traffic remains
  protected by existing no-reroute rules (priority 102).
* Regression tests of EgressIP on gateway vs non-gateway nodes; test pods on
  non-gateway nodes with and without EgressIP to verify correct policy precedence.

### Testing Details

#### Unit Tests

* **RouteAdvertisements controller**: Update `controller_test.go`:
  * Remove `"fails to reconcile pod network if node selector is not empty"` test.
  * Add test: **Layer 3 networks** — gateway nodes generate FRRConfigurations
    with all nodes' host subnets when `nodeSelector` is non-empty (vs. each node
    advertising only its own subnet when `nodeSelector` is empty).
  * Add test: **Layer 2 networks** — all nodes advertise the cluster-wide subnet
    regardless of `nodeSelector` value (empty or non-empty).
  * Add test: non-gateway nodes are not included in generated
    FRRConfigurations.
  * Add test: reject `nodeSelector` + `PodNetwork` with `NoOverlay` transport.
  * Add test: `nodeSelector` matching zero ready nodes reports degraded or
    pending status and does not generate `FRRConfiguration` objects.
  * Add test: **multiple `RouteAdvertisements` selecting the same network**
    with different `nodeSelector` values generate independent
    `FRRConfiguration` objects (labeled per-RA), and reroute policies use
    the **union** of selected gateway nodes' transit switch IPs as nexthops.
  * Add test: multiple `RouteAdvertisements` selecting the same network with
    overlapping `nodeSelector` values merge correctly without conflict.
  * Add test: **dual-stack networks** generate both IPv4 and IPv6
    `FRRConfiguration` prefixes for all nodes on gateway nodes.
  * Add test: **dual-stack reroute policies** are created with separate IPv4
    and IPv6 policies, each with IP-family-matched nexthops.

* **networkmanager**: Test `syncRouteAdvertisements` / `podAdvertisements`
  when only a subset of nodes are BGP gateways, including dual-stack networks.

* **OVN network controller**: Test that breth0 flows are created for remote pod
  subnets on gateway nodes, including both IPv4 and IPv6 flows for dual-stack
  networks.

* **Logical router policy**: Test that reroute policies are created on **the
  correct router for each network type**:
  * Default cluster network: policies on `ovn_cluster_router`
  * Primary Layer 3 UDN: policies on `ovn_cluster_router_<network-name>`
  * Primary Layer 2 UDN: policies on per-node gateway routers `GR_<node>_<network>`
  * Verify policies use gateway node transit switch IPs as nexthops (specific to
    each network's transit switch).
  * For dual-stack, verify separate IPv4 and IPv6 policies with family-matched
    nexthops on each router.

#### E2E Tests

* Deploy a Kind cluster with 4 nodes, designate 2 as gateway nodes via labels.
* Create `RouteAdvertisements` with `nodeSelector` matching the 2 gateway nodes.
* Verify:
  * External traffic reaches pods on non-gateway nodes via gateway nodes.
  * Pods on non-gateway nodes can reach BGP-learned external destinations.
  * Failover works when one gateway node is taken down.
  * Both SGW and LGW modes.
* **Multiple RouteAdvertisements for same network**:
  * Create two `RouteAdvertisements` selecting the same network with different
    `nodeSelector` values (e.g., RA1 selects worker-1, RA2 selects worker-2).
  * Verify both gateway nodes advertise prefixes for all nodes.
  * Verify reroute policies include transit IPs from both gateway nodes (ECMP).
  * Verify traffic load-balances across both gateways.
  * Take down one gateway; verify traffic fails over to the remaining gateway.
* **Dual-stack networks**:
  * Deploy a dual-stack Kind cluster with IPv4+IPv6 pod subnets.
  * Create `RouteAdvertisements` with gateway `nodeSelector`.
  * Verify:
    * Gateway nodes advertise **both IPv4 and IPv6** prefixes for all nodes to
      BGP peers (inspect FRRConfiguration and BGP RIB).
    * **IPv4 ingress**: External IPv4 traffic reaches IPv4 pod addresses on
      non-gateway nodes via gateway nodes (test via curl/ping).
    * **IPv6 ingress**: External IPv6 traffic reaches IPv6 pod addresses on
      non-gateway nodes via gateway nodes.
    * **IPv4 egress**: IPv4 pods on non-gateway nodes can reach IPv4
      BGP-learned external destinations (verify via traceroute that traffic
      routes through gateway nodes).
    * **IPv6 egress**: IPv6 pods on non-gateway nodes can reach IPv6
      BGP-learned external destinations.
    * OVS flows exist for both IPv4 and IPv6 remote subnets on gateway nodes
      (SGW and LGW modes).
    * Reroute policies exist for both `ip4.src` and `ip6.src` matches with
      IP-family-matched nexthops.
    * **Failover works for both IP families**: Take down one gateway node,
      verify both IPv4 and IPv6 traffic fail over to the remaining gateway.
* **IPv6-only networks**:
  * Deploy an IPv6-only Kind cluster.
  * Verify gateway advertisements, ingress, egress, and failover work correctly
    for IPv6-only traffic.
* **Primary Layer 3 UDN with gateway nodeSelector**:
  * Deploy a Kind cluster with a primary Layer 3 ClusterUserDefinedNetwork.
  * Create `RouteAdvertisements` selecting the UDN with gateway `nodeSelector`.
  * Verify:
    * Reroute policies are created on `ovn_cluster_router_<udn-name>`, **not**
      `ovn_cluster_router`.
    * UDN pods on non-gateway nodes can reach BGP-learned external destinations
      via gateway nodes.
    * Gateway nodes advertise all UDN pod subnets to BGP peers (in the correct
      VRF if `targetVRF: auto`).
* **Primary Layer 2 UDN with gateway nodeSelector**:
  * Deploy a Kind cluster with a primary Layer 2 ClusterUserDefinedNetwork.
  * Create `RouteAdvertisements` selecting the L2 UDN with gateway `nodeSelector`.
  * Verify:
    * Reroute policies are created on **per-node gateway routers**
      `GR_<node>_<network>` for each non-gateway node, **not** on
      `ovn_cluster_router`.
    * L2 UDN pods on non-gateway nodes can reach BGP-learned external
      destinations via gateway nodes.
    * Gateway nodes advertise the L2 UDN **cluster subnet** (not per-node
      subnets, as L2 networks share a single subnet).

#### CRD Integration Tests

* Verify the updated CEL validation allows `nodeSelector` with `PodNetwork`.
* Verify no regression in existing validation rules.

#### Scale Testing

* Many nodes with few gateways: BGP prefix count, FRR reconcile latency, and
  breth0 flow count as nodes scale.

#### Cross-Feature Testing

* **EgressIP**: precedence vs BGP gateway reroute (logical router policies must
  not override EgressIP reroute or no-reroute)

### Documentation Details

* Update `docs/features/bgp-integration/route-advertisements.md`:
  * Add a new section on gateway node deployment pattern with examples.
  * Document the NoOverlay restriction.
* Update the `RouteAdvertisements` API reference documentation.
* Add `mkdocs.yml` entry for this OKEP.

## Risks, Known Limitations and Mitigations

* **Gateway bottleneck**: All N/S traffic flows through gateway nodes. Users
  should select multiple gateway nodes for ECMP and monitor gateway node
  capacity.
* **Asymmetric routing not supported**: Ingress arrives via a gateway node; egress may exit
  from a different gateway node via ECMP. External networks must tolerate
  asymmetric paths or use loose RPF. This is standard behavior for ECMP
  deployments.
* **NoOverlay not supported**: The feature requires a tunnel overlay for
  cross-node forwarding. NoOverlay deployments must continue to advertise from
  all nodes.
* **Interaction with EgressIP**: When both EgressIP and BGP gateway NodeSelector
  are configured, the EgressIP reroute policy (priority 100) takes precedence
  over BGP gateway reroute (priority 99) for pods with EgressIP. Traffic from
  those pods is directed to the EgressIP node, not the BGP gateway node. This is
  the expected behavior — EgressIP controls source IP selection. The BGP gateway
  reroute uses priority 99 (numerically lower than EgressIP's 100) to ensure
  EgressIP policies are always evaluated first.
* **Empty gateway selection**: If `nodeSelector` matches no ready nodes,
  `RouteAdvertisements` does not generate gateway `FRRConfiguration` objects
  until at least one gateway node is available.

## OVN-Kubernetes Version Skew

This feature is planned for introduction in a future release. Check repo
milestones for the next release window.

## Backwards Compatibility

* **API**: The `nodeSelector` field already exists. Removing the CEL validation
  rule is a relaxation — previously invalid configurations become valid. No
  existing valid configurations are affected.
* **Same-network conflict rule relaxed**: OKEP-5296 prohibited multiple
  `RouteAdvertisements` from selecting the same network. This proposal **relaxes
  that restriction**: multiple `RouteAdvertisements` may now select the same
  network (each generates independently labeled `FRRConfiguration` objects that
  FRR-k8s merges). This is a compatibility expansion — configurations that were
  previously rejected are now allowed; no existing valid configurations break.
* **Existing behavior** (varies by network topology):
  * **Layer 3 networks** (default cluster network, primary Layer 3 UDNs): When
    `nodeSelector` is empty (selects all nodes), the behavior is identical to
    today — each node advertises only its own host subnet. The new "advertise
    all nodes' subnets" behavior only activates when `nodeSelector` is non-empty.
  * **Layer 2 networks** (primary Layer 2 UDNs): Layer 2 networks always
    advertise the **cluster-wide network subnet**, regardless of `nodeSelector`
    value (empty or non-empty). This behavior is unchanged — Layer 2 networks
    share a single subnet across all nodes, not per-node host subnets.
* **E2E tests**: Existing BGP/EVPN E2E tests should continue to pass without
  modification. New E2E tests will be added for the gateway node pattern.

## Alternatives

### Alternative 1: Gateway nodes advertise only their own subnets

Each gateway node advertises only its own host subnet. Pods on non-gateway nodes
are not reachable from the external network. Users must schedule
externally-reachable workloads on gateway nodes.

**Rejected because**: Users need the flexibility to schedule workloads on any
node while maintaining external reachability through gateway nodes.

### Alternative 2: Gateway nodes advertise aggregate CIDR

Instead of individual host subnets, gateway nodes advertise the parent network
CIDR (e.g., `10.244.0.0/16`). This is simpler but provides coarse-grained
routing.

**Rejected because**: Advertising individual host subnets provides more precise
routing information to external peers and is consistent with existing behavior.

### Alternative 3: Symmetric-only mode via configuration flag

Add a `podNetworkAdvertisementMode` field to control symmetric vs asymmetric
egress path. Symmetric mode would force all egress through gateway nodes;
asymmetric would allow direct egress from pod nodes.

**Rejected because**: The egress reroute through gateway nodes is required
regardless — non-gateway nodes do not have BGP-learned routes, so they cannot
forward external-bound traffic independently. Asymmetric egress is not viable
when only gateway nodes have BGP peering.

## References

* [OKEP-5296: OVN-Kubernetes BGP Integration](okep-5296-bgp.md) — original BGP
  OKEP listing node selection as a future goal.
* [GitHub issue #7040](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/7040)
* [Route Advertisements documentation](../features/bgp-integration/route-advertisements.md)
* [OVN Logical Router Policy](https://www.ovn.org/support/dist-docs/ovn-nb.5.html)
  — `Logical_Router_Policy` table with `reroute` action and multiple `nexthops`.
* [FRR-k8s](https://github.com/metallb/frr-k8s)
