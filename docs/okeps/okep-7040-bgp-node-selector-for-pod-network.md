# OKEP-7040: BGP NodeSelector for PodNetwork RouteAdvertisements

* Issue: [#7040](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/7040)

## Problem Statement

The `RouteAdvertisements` CRD currently requires `nodeSelector` to select all
nodes when `PodNetwork` is included in `advertisements`. This prevents users
from choosing which nodes' pod subnets are advertised via BGP gateway nodes.
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
| `RouteAdvertisements.spec.nodeSelector` | RA CR | Which nodes' subnets are advertised and have SNAT disabled |
| `FRRConfiguration.spec.nodeSelector` | FRRConfig CR (matched via `frrConfigurationSelector`) | Which nodes are BGP gateways (run FRR, peer with external routers) |

## User Stories

### Story 1: Dedicated BGP gateway nodes

As a cluster admin, I designate 2 out of 10 nodes as BGP gateways (via
`FRRConfiguration.spec.nodeSelector`). I create a `RouteAdvertisements` that
selects all nodes via `nodeSelector` (or leaves it empty) and advertises
`PodNetwork`. The 2 gateway nodes advertise pod subnets for all nodes to
external BGP peers, and pods on any node are reachable from the external
network. This applies to the default cluster network, primary L3 CUDNs,
and primary L2 CUDNs.

### Story 2: Selective subnet advertisement (L3)

As a cluster admin, I want only a subset of nodes' pod subnets advertised
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

## Proposed Solution

### API Changes

**Remove CEL validation**: Remove the validation rule on `RouteAdvertisements`
that blocks `nodeSelector` with `PodNetwork`.

**Runtime validation**: Reject `nodeSelector` + `PodNetwork` when any selected
network uses `transport: NoOverlay` or `transport: EVPN` (requires
cross-resource validation).

No new API fields are required.

### Control Plane

**File:** `go-controller/pkg/clustermanager/routeadvertisements/controller.go`

When `nodeSelector` is non-empty and `PodNetwork` is advertised:

1. Remove the runtime guard that rejects non-empty `nodeSelector` with
   `PodNetwork`.
2. Add runtime validation to reject `nodeSelector` + `PodNetwork` when any
   selected network uses NoOverlay or EVPN transport.
3. For L3 networks: each BGP gateway node advertises host subnets of the
   nodes selected by RA's `nodeSelector`.
4. For L2 networks: gateway nodes advertise `/32` (or `/128` for IPv6) routes
   for individual pod IPs on the selected nodes. This requires the RA controller
   to watch pod events so that gateway nodes can advertise or withdraw `/32`
   routes as pods are created or deleted. **This needs further discussion** on
   scalability and design approach.
5. BGP next-hop is per neighbor, not per prefix — gateway nodes advertise
   non-gateway subnets with themselves as next-hop (FRR default for eBGP).
   External routers only need reachability to gateway nodes.
6. When FRR selects more nodes than RA, those extra nodes have BGP sessions
   but do not advertise pod network routes. This is valid — BGP sessions
   may serve other purposes (e.g., EgressIP).
7. When RA `nodeSelector` is empty, behavior is unchanged — each node
   advertises its own subnet as today.

**SNAT behavior** (`network_controller.go`): SNAT is disabled on nodes selected
by RA's `nodeSelector`. These nodes' pods preserve their pod IPs for external
traffic. Nodes not selected retain normal SNAT behavior.

### Data Plane: Ingress (External → Pod on non-gateway node)

External traffic for a remote pod arrives at a BGP gateway node. OVS flows on
breth0 must steer traffic for **all** advertised pod subnets into the OVN
pipeline, not just the gateway node's own subnet.

**SGW** — new flows steer remote subnets via patch port:

```
# Existing (gateway node's own subnet):
cookie=0xdeff105, priority=300,ip,in_port=1,nw_dst=10.244.2.0/24 actions=output:2

# New (remote node subnets):
cookie=0xdeff105, priority=300,ip,in_port=1,nw_dst=10.244.0.0/24 actions=output:2
cookie=0xdeff105, priority=300,ip,in_port=1,nw_dst=10.244.1.0/24 actions=output:2
cookie=0xdeff105, priority=300,ip,in_port=1,nw_dst=10.244.3.0/24 actions=output:2
```

**LGW** — new flows send remote subnets to LOCAL; host aggregate routes
(`10.244.0.0/16 via mp0`) forward into OVN:

```
# Existing (gateway node's own subnet):
cookie=0xdeff105, priority=300,ip,in_port=eth0,nw_dst=10.244.2.0/24 actions=LOCAL

# New (remote node subnets):
cookie=0xdeff105, priority=300,ip,in_port=eth0,nw_dst=10.244.0.0/24 actions=LOCAL
cookie=0xdeff105, priority=300,ip,in_port=eth0,nw_dst=10.244.1.0/24 actions=LOCAL
cookie=0xdeff105, priority=300,ip,in_port=eth0,nw_dst=10.244.3.0/24 actions=LOCAL
```

Once inside OVN, the cluster router has routes for all node subnets and
forwards via Geneve tunnel to the correct node.

**L2 CUDN** — for L2 networks with selective `nodeSelector`, breth0 flows
match individual pod IPs (`/32` or `/128`) instead of per-node subnets.
**This needs further discussion** alongside the L2 control plane design.

### Data Plane: Egress (Pod on non-gateway node → External)

Pods on nodes with SNAT disabled egress via the GR's default route to the
node's default gateway. The default gateway must be able to forward traffic
to the external destination. No OVN reroute policies are needed.

The return path is **asymmetric** — the external router (peered with BGP
gateway nodes) has BGP-learned routes for advertised pod subnets and steers
reply traffic through the BGP gateway node.

```
Egress:  Pod → GR → default gateway → external destination

Reply:   External destination → external router (has BGP-learned pod routes)
         → BGP gateway node → breth0 flow → OVN → Geneve → non-gateway node → pod
```

### Traffic Flow Summary

```
INGRESS (external → pod on non-gateway node):

  external
    │
    ▼
  BGP gateway node (breth0 flow matches remote subnet)
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
  default gateway is configured to forward pod traffic.
* **L2 CUDN scalability**: Advertising `/32` per-pod routes may not scale
  with large numbers of pods. This needs further discussion.

## Testing Details

### Unit Tests

* **RouteAdvertisements controller** (`controller_test.go`):
  * Remove test that rejects non-empty `nodeSelector` with `PodNetwork`.
  * L3 networks: gateway nodes generate FRRConfigurations with selected
    nodes' host subnets when `nodeSelector` is non-empty.
  * L2 networks: gateway nodes generate FRRConfigurations with `/32` pod
    routes for pods on selected nodes.
  * Non-gateway nodes are not included in generated FRRConfigurations.
  * Reject `nodeSelector` + `PodNetwork` with NoOverlay or EVPN transport.
* **networkmanager** (`network_controller.go`):
  * `syncRouteAdvertisements` populates `podAdvertisements` only for nodes
    matching RA's `nodeSelector`.
* **OVN network controller**:
  * breth0 flows are created for remote pod subnets on gateway nodes (SGW
    and LGW).

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
* Primary L2 CUDN with RA `nodeSelector` — verify `/32` pod route
  advertisement and withdrawal on pod create/delete.
* Failover when a gateway node is taken down.
* EgressIP continues to work on gateway and non-gateway nodes when combined
  with RA `nodeSelector`.

### CRD Integration Tests

* Verify updated CEL validation allows `nodeSelector` with `PodNetwork`.
* Verify no regression in existing validation rules.

## Backwards Compatibility

* Removing the CEL validation is a relaxation — previously invalid
  configurations become valid. No existing valid configurations are affected.
* When `nodeSelector` is empty, behavior is identical to today.

## References

* [OKEP-5296: OVN-Kubernetes BGP Integration](okep-5296-bgp.md)
* [GitHub issue #7040](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/7040)
* [Route Advertisements documentation](../features/bgp-integration/route-advertisements.md)
* [FRR-k8s](https://github.com/metallb/frr-k8s)
