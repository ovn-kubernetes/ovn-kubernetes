# OKEP-7040: BGP NodeSelector for PodNetwork RouteAdvertisements

* Issue: [#7040](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/7040)

## Problem Statement

The `RouteAdvertisements` CRD currently requires `nodeSelector` to select all
nodes when `PodNetwork` is included in `advertisements`. This prevents users
from choosing which node pod subnets are advertised via BGP gateway nodes.
Users need the ability to selectively advertise pod subnets from chosen nodes
through BGP gateway nodes (selected by `FRRConfiguration.spec.nodeSelector`)
while controlling SNAT behavior on advertised nodes.

## Goals

* Allow `RouteAdvertisements.spec.nodeSelector` to select nodes whose pod
  subnets are advertised via BGP gateway nodes.
* BGP gateway nodes (selected by `FRRConfiguration.spec.nodeSelector`)
  advertise pod subnets for all selected nodes.
* SNAT is disabled on nodes selected by RA's `nodeSelector`, preserving pod
  IPs for external traffic.
* Support shared gateway (SGW) and local gateway (LGW) modes with overlay
  (Geneve) transport.
* Non-gateway nodes do not require FRR peering for the advertised network.
* Preserve today's behavior when `nodeSelector` is empty.
* EgressIP must continue to work as before when combined with RA
  `nodeSelector`.

## Non-Goals

* Support for no-overlay transport — without Geneve overlay, each node must
  peer with external BGP routers directly to advertise its own pod subnet.
  Selecting a subset of gateway nodes is not applicable.
* EVPN — selective RA with `nodeSelector` for `PodNetwork` is BGP-only. EVPN
  requires all nodes to participate in the VXLAN underlay and BGP full-mesh,
  which is incompatible with selective node advertisement.
* Dynamic UDN allocation — when dynamic UDN allocation is enabled, networks
  are only rendered on nodes that have pods. A gateway node selected by
  `nodeSelector` may not have any pods on the advertised network, so the
  network would not be rendered on that node and ingress traffic cannot be
  delivered to remote node pods. Combining this with selective `nodeSelector`
  advertisement is not supported.
* Egress rerouting — steering non-BGP node traffic through BGP gateway nodes
  via OVN logical router policies, transit switch nexthops, or any other
  datapath mechanism. This is a separate feature.
* FRR daemonset scheduling / scale-down — frr-k8s runs on all nodes but
  non-gateway nodes have no BGP peering with external routers, so idle
  containers cost little. The value of this feature is in reducing BGP
  session count and external router table size. Restricting FRR scheduling
  is a separate concern.

## Introduction

The original BGP OKEP ([OKEP-5296](okep-5296-bgp.md)) listed "Allow selecting
only a subset of nodes to advertise BGP" as a future goal. This OKEP fulfills
that goal for `PodNetwork` advertisements.

In many datacenter deployments, only a subset of nodes are connected to BGP
peers on the provider network. These "gateway nodes" act as entry and exit
points for north/south traffic. Today, `RouteAdvertisements` enforces that
`PodNetwork` must be advertised on all nodes, which prevents this deployment
pattern.

### Key Concepts

Two selectors control the feature:

| Selector | Source | Controls |
|----------|--------|----------|
| `RouteAdvertisements.spec.nodeSelector` | RA CR | Which node subnets are advertised and have SNAT disabled |
| `FRRConfiguration.spec.nodeSelector` | FRRConfig CR (matched via `frrConfigurationSelector`) | Which nodes are BGP gateways (run FRR, peer with external routers) |

## User Stories

### Story 1: Dedicated BGP gateway nodes

As a cluster admin, I designate 2 out of 10 nodes as BGP gateways (via
`FRRConfiguration.spec.nodeSelector`). I create a `RouteAdvertisements` that
selects all nodes via `nodeSelector` and advertises `PodNetwork`. The 2 gateway
nodes advertise pod subnets for all nodes to external BGP peers and pods on any
node are reachable from the external network. This applies to the default cluster
network, primary L3 CUDNs and primary L2 CUDNs.

### Story 2: Selective subnet advertisement (L3)

As a cluster admin, I want only a subset of node pod subnets advertised
externally for an L3 network. I set `RouteAdvertisements.spec.nodeSelector`
to match only those nodes. SNAT is disabled on the selected nodes so their
pods egress with pod IPs. Pods on non-selected nodes continue to use SNAT
(node IP) for external traffic.

### Story 3: Selective pod advertisement (L2 CUDN)

As a cluster admin, I have a primary Layer 2 CUDN and want only pods on a
subset of nodes to be reachable from the external network via BGP gateway
nodes.

### Story 4: High availability with multiple gateways

As a cluster admin, I select multiple BGP gateway nodes so that if one fails,
traffic is rerouted to the remaining gateways via ECMP.

## API Changes

**Remove CEL validation**: Remove the validation rule on `RouteAdvertisements`
that blocks `nodeSelector` with `PodNetwork`.

No new API fields are required.

## Implementation Details

### Internal Controller Details

The changes span three controllers across two controller managers:

#### Cluster Manager

* Route Advertisements Controller (`go-controller/pkg/clustermanager/routeadvertisements/controller.go`)
  * Remove the runtime guard that rejects non-empty `nodeSelector` with `PodNetwork`.
  * Add runtime validation to reject `nodeSelector` + `PodNetwork` when any
    selected network uses NoOverlay or EVPN transport, or when dynamic UDN
    allocation is enabled.
  * For L3 networks: each BGP gateway node advertises host subnets of all nodes
    selected by RA's `nodeSelector`.
  * For L2 networks: gateway nodes advertise individual pod IP addresses on
    the network. L2 networks share a flat subnet across all nodes, so
    advertising the entire subnet (e.g., `/16`) would make all pods reachable
    regardless of which nodes the selector picks.
  * The RA controller watches pod events (phase changes,
    `k8s.ovn.org/pod-networks` annotation updates) to trigger reconciliation,
    so that gateway nodes advertise or withdraw pod IP routes as pods are
    created or deleted.
  * When RA `nodeSelector` is empty and `FRRConfiguration` also selects all
    nodes, behavior is unchanged — each node advertises its own subnet as
    today. For L2 networks, each node advertises the cluster supernet.
* Network Controller (`go-controller/pkg/networkmanager/network_controller.go`)
  — Modified SNAT behavior: SNAT is disabled on nodes selected by RA's
  `nodeSelector`. Pods on these nodes preserve their pod IPs for external
  traffic. Nodes not selected retain normal SNAT behavior.

#### Node Controller Manager

* Bridge Flows (`go-controller/pkg/node/bridgeconfig/bridgeflows.go`) —
  Modified to add a cluster supernet ingress flow for L3 advertised networks
  on gateway nodes. For L2 networks, the existing breth0 flows already match
  on the network subnet — no changes needed.

### Data Plane

#### Ingress (External → Pod on non-gateway node)

External traffic for a remote pod arrives at a BGP gateway node. OVS flows on
breth0 must steer traffic for advertised pod subnets into the OVN pipeline,
not just the gateway node's own subnet.

Because only the necessary routes are advertised to the external router (per-node
host subnets for L3, per-pod IPs for L2), only traffic destined for advertised
prefixes arrives at the gateway node. This means breth0 flows can match on the
cluster supernet (e.g., `10.244.0.0/16`) at a lower priority than the gateway
node's own host subnet flow — no per-node or per-pod flow rules are needed on
breth0.

**L3 networks** — a new cluster supernet flow is added at priority 290 (below
the gateway node's own host subnet at priority 300):

**SGW**:

```
# Existing (gateway node's own subnet, priority 300):
cookie=0xdeff105, priority=300,ip,in_port=1,nw_dst=10.244.2.0/24 actions=output:2

# New (cluster supernet, priority 290 — below local subnet):
cookie=0xdeff105, priority=290,ip,in_port=1,nw_dst=10.244.0.0/16 actions=output:2
```

**LGW**:

```
# Existing (gateway node's own subnet, priority 300):
cookie=0xdeff105, priority=300,ip,in_port=eth0,nw_dst=10.244.2.0/24 actions=LOCAL

# New (cluster supernet, priority 290 — below local subnet):
cookie=0xdeff105, priority=290,ip,in_port=eth0,nw_dst=10.244.0.0/16 actions=LOCAL
```

Once inside OVN, the cluster router has routes for all node subnets and
forwards via Geneve tunnel to the correct node.

**L2 networks** — no new breth0 flows are needed. L2 networks already have
breth0 flows matching on the network subnet (e.g., `10.210.0.0/16`). Since
the RA controller only advertises per-pod IP routes to the external router,
only traffic for those specific pod IPs arrives at the gateway node. The
existing subnet match on breth0 steers it into OVN, which forwards via the
L2 overlay to the correct node.

#### Egress (Pod on non-gateway node → External)

Pods on nodes with SNAT disabled egress via the GR's default route to the
node's default gateway. The default gateway must be able to forward traffic
to the external destination. No OVN reroute policies are needed.

The return path is asymmetric — the external router (peered with BGP gateway
nodes) has BGP-learned routes for advertised pod subnets and steers reply
traffic through the BGP gateway node.

```
Egress:  Pod → GR → default gateway → external destination

Reply:   External destination → external router (has BGP-learned pod routes)
         → BGP gateway node → breth0 flow → OVN → Geneve → non-gateway node → pod
```

#### BGP Next-Hop

BGP next-hop is per neighbor, not per prefix — gateway nodes advertise
non-gateway subnets with themselves as next-hop (FRR default for eBGP).
External routers only need reachability to gateway nodes. When FRR selects
more nodes than RA, those extra nodes have BGP sessions but do not advertise
pod network routes. This is valid — BGP sessions may serve other purposes
(e.g., EgressIP).

### Traffic Flow Summary

```
INGRESS (external → pod on non-gateway node):

  external
    │
    ▼
  BGP gateway node (breth0 flow matches cluster supernet)
    │
    SGW: → patch port → OVN pipeline
    LGW: → LOCAL → host kernel → mp0 → OVN pipeline
    │
    ▼
  OVN cluster router → Geneve tunnel → non-gateway node → pod


EGRESS (pod on non-gateway node → external):

  pod on non-gateway node
    │
    ▼
  GR → default gateway → external destination

  Reply (asymmetric):
    external destination → external router (BGP-learned pod routes)
    → BGP gateway node → breth0 flow → OVN → Geneve → non-gateway node → pod
```

### Scope Matrix

| Network | Topology | Transport | Supported |
|---------|----------|-----------|-----------|
| Default cluster network | L3 | Geneve | Yes |
| Primary L3 CUDN | Layer 3 | Overlay | Yes |
| Primary L2 CUDN | Layer 2 | Overlay | Yes |
| Any | — | EVPN | No |
| Any | — | NoOverlay | No |

## Risks and Limitations

* **Asymmetric routing**: Egress goes via the node's default gateway, while
  reply traffic returns via the BGP gateway node. The default gateway sees
  only the outgoing packets (e.g., TCP SYN) but never the replies (e.g.,
  SYN-ACK), so its conntrack may mark subsequent packets as INVALID and drop
  them. The default gateway must either disable conntrack for pod traffic
  (e.g., `notrack`) or use stateless forwarding for advertised pod subnets.
  In LGW mode, `NB_Global.options:use_ct_inv_match=false` prevents OVN from
  dropping reply packets due to asymmetric paths.
* **Default gateway must forward pod traffic**: The node's default gateway
  must be able to forward traffic with pod source IPs to external
  destinations. It is the administrator's responsibility to ensure the
  default gateway is configured to forward pod traffic. On Kind clusters,
  the Docker DOCKER-USER iptables chain blocks cross-bridge forwarding by
  default. For L3 CUDNs, allow the non-gateway node's host subnet (e.g.,
  `iptables -I DOCKER-USER -s 10.200.0.0/16 -j ACCEPT`). For L2 CUDNs,
  allow the cluster supernet instead. Additionally, bypass conntrack for
  pod traffic to avoid asymmetric routing drops (e.g.,
  `nft insert rule ip raw PREROUTING ip saddr 10.200.0.0/16 notrack accept`).
* **Pod event watch overhead**: The RA controller watches pod events
  cluster-wide regardless of whether any L2 CUDN with `nodeSelector` exists.
  This adds overhead even in clusters that only use L3 networks or have no
  BGP advertisements at all. The event filter discards most events cheaply
  (only pod phase changes and `k8s.ovn.org/pod-networks` annotation updates
  pass through), but the volume of pod events in large clusters must be
  validated at scale to ensure no regressions.
* **L2 CUDN route scalability**: When L2 CUDNs with `nodeSelector` are in
  use, advertising per-pod IP routes increases the number of BGP routes
  proportionally with the number of pods on the network. Reconciliation is
  scoped to the network's namespaces, but frequent pod churn may cause
  repeated FRRConfiguration updates. [OKEP-5296](okep-5296-bgp.md) describes
  broader L2 routing optimizations (per-node contiguous address blocks,
  per-pod routes for VM migration) that would also address this concern.

## Testing Details

### Unit Tests

* **RouteAdvertisements controller** (`controller_test.go`):
  * Remove test that rejects non-empty `nodeSelector` with `PodNetwork`.
  * L3 networks: gateway nodes generate FRRConfigurations with selected
    node host subnets when `nodeSelector` is non-empty.
  * L2 networks: gateway nodes generate FRRConfigurations with per-pod IP
    routes for pods on the network's namespaces.
  * L2 networks: pod create/delete triggers RA reconciliation and updates
    advertised routes.
  * Non-gateway nodes are not included in generated FRRConfigurations.
  * Reject `nodeSelector` + `PodNetwork` with NoOverlay transport, EVPN
    transport, or dynamic UDN allocation enabled.
* **networkmanager** (`network_controller.go`):
  * `syncRouteAdvertisements` populates `podAdvertisements` only for nodes
    matching RA's `nodeSelector`.
* **Bridge flows** (`bridgeflows.go`):
  * L3 advertised networks: cluster supernet ingress flow is added on
    gateway nodes (SGW and LGW).
  * L2 advertised networks: no new breth0 flows — existing network subnet
    flows handle ingress.

### E2E Tests

* Deploy a Kind cluster with multiple nodes, designate a subset as BGP
  gateway nodes via labels.
* Create `RouteAdvertisements` with `nodeSelector` selecting advertised nodes.
* Verify:
  * External traffic reaches pods on non-gateway nodes via gateway nodes
    (ingress).
  * Pods on non-gateway nodes can reach external destinations (egress via
    default gateway, reply via BGP gateway node).
  * Both SGW and LGW modes.
* Primary L3 CUDN with RA `nodeSelector`.
* Primary L2 CUDN with RA `nodeSelector` — verify per-pod IP route
  advertisement and withdrawal on pod create/delete.
* Failover when a gateway node is taken down.
* EgressIP continues to work on gateway and non-gateway nodes when combined
  with RA `nodeSelector`.

### CRD Integration Tests

* Verify updated CEL validation allows `nodeSelector` with `PodNetwork`.
* Verify no regression in existing validation rules.

## Documentation Details

* Update the existing [Route Advertisements documentation](../features/bgp-integration/route-advertisements.md)
  to cover `nodeSelector` usage with `PodNetwork`.

## Backwards Compatibility

* Removing the CEL validation is a relaxation — previously invalid
  configurations become valid. No existing valid configurations are affected.
* When `nodeSelector` is empty, behavior is identical to today.

## OVN Kubernetes Version Skew

To be determined.

## Alternatives

Not applicable.

## References

None
