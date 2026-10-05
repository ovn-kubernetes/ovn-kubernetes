# OKEP-7040: BGP NodeSelector for PodNetwork RouteAdvertisements

* Issue: [#7040](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/7040)

## Problem Statement

The `RouteAdvertisements` CRD currently requires `nodeSelector` to select all
nodes when `PodNetwork` is included in `advertisements`. This prevents users
from designating a subset of nodes as BGP gateway nodes for pod network route
advertisements. Users need the ability to restrict BGP peering to specific
gateway nodes while maintaining full pod reachability from the external network.

## Goals

* Allow `nodeSelector` to select a subset of **BGP gateway** nodes when
  `PodNetwork` is advertised, for the **default cluster network** and for
  **primary Layer 2 and Layer 3** cluster user-defined networks (CUDNs).
* Gateway nodes advertise routes so pods on **non-gateway** nodes remain
  reachable from the provider network and can egress to BGP-learned
  destinations via gateway nodes.
* Support ingress (external → pod) and egress (pod → BGP-learned external)
  through gateway nodes in **shared gateway (SGW)** and **local gateway (LGW)**
  modes with **overlay (Geneve)** and **EVPN (VXLAN)** transports.
* Provide ECMP load-balancing and high availability across multiple gateway
  nodes.
* Preserve `EgressIP` `nodeSelector` expected behavior when combined with
  `PodNetwork` `nodeSelector` selection.
* **Non-gateway nodes do not require FRR** for `PodNetwork` advertisement on
  the selected network.

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

### Story 5: EgressIP continues to work with gateway `nodeSelector`

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
* Multiple `RouteAdvertisements` objects for the same network continue to use
  existing conflict rules; operators should avoid overlapping gateway sets
  across objects unless that is intentional.

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

#### EVPN (VXLAN) considerations

EVPN CUDNs use the same gateway pattern as Geneve overlay: gateways advertise
prefixes and program br-ex; non-gateway nodes use cluster-router reroute to
gateway transit IPs. Implementation must align EVPN-specific behavior from
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
2. Add the no-overlay validation: reject if any selected network has
   `transport: NoOverlay`.
3. Change route generation for Layer3 networks: each selected gateway node
   advertises the host subnets of **all** nodes (not just its own). For Layer2
   networks, no change is needed — all nodes already advertise the cluster-wide
   network subnet.
4. On **node add**: allocate host subnet, add prefix to every gateway's
   advertisement and br-ex remote flow set; add reroute policy row for that
   subnet if the node is non-gateway.
5. On **node delete**: withdraw prefix from gateways and remove stale policies
   / flows.

The generated per-node `FRRConfiguration` for a gateway node will contain
prefixes for all nodes' host subnets:

```yaml
# Generated for gateway node ovn-worker:
spec:
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: ovn-worker
  bgp:
    routers:
      - asn: 64512
        prefixes:
          - 10.244.0.0/24   # ovn-worker3's subnet
          - 10.244.1.0/24   # ovn-control-plane's subnet
          - 10.244.2.0/24   # ovn-worker's own subnet
          - 10.244.3.0/24   # ovn-worker2's subnet
        neighbors:
          - asn: 64513
            address: 192.168.1.1
            toAdvertise:
              allowed:
                prefixes:
                  - 10.244.0.0/24
                  - 10.244.1.0/24
                  - 10.244.2.0/24
                  - 10.244.3.0/24
```

#### Data Plane: Ingress (External → Pod on non-gateway node)

External traffic for a remote pod arrives at the gateway node. The gateway node
must forward it to the correct node via the tunnel overlay.

##### Shared Gateway Mode (SGW)

Traffic enters breth0 on the gateway node. OVS flows must steer traffic for
**all** advertised pod subnets into the OVN pipeline via the patch port, not
just the gateway node's own subnet.

```
# Existing (own subnet):
cookie=0xdeff105, priority=300,ip,in_port=1,nw_dst=10.244.2.0/24 actions=output:2

# New (remote subnets):
cookie=0xdeff105, priority=300,ip,in_port=1,nw_dst=10.244.0.0/24 actions=output:2
cookie=0xdeff105, priority=300,ip,in_port=1,nw_dst=10.244.1.0/24 actions=output:2
cookie=0xdeff105, priority=300,ip,in_port=1,nw_dst=10.244.3.0/24 actions=output:2
```

Once inside OVN, the cluster router has routes for all node subnets and forwards
via Geneve tunnel to the correct node.

##### Local Gateway Mode (LGW)

Traffic enters breth0 and is sent to the host kernel via `actions=LOCAL`. The
host already has an aggregate route (`10.244.0.0/16 via <mp0-gw> dev
ovn-k8s-mp0`) that covers all pod subnets. Only the breth0 OVS flows need to be
added for remote subnets:

```
# Existing (own subnet):
cookie=0xdeff105, priority=300,ip,in_port=eth0,nw_dst=10.244.2.0/24 actions=LOCAL

# New (remote subnets):
cookie=0xdeff105, priority=300,ip,in_port=eth0,nw_dst=10.244.0.0/24 actions=LOCAL
cookie=0xdeff105, priority=300,ip,in_port=eth0,nw_dst=10.244.1.0/24 actions=LOCAL
cookie=0xdeff105, priority=300,ip,in_port=eth0,nw_dst=10.244.3.0/24 actions=LOCAL
```

Traffic then follows: `breth0 → LOCAL → host kernel → 10.244.0.0/16 via mp0 →
OVN cluster router → Geneve tunnel → remote node → pod`.

#### Data Plane: Egress (Pod on non-gateway node → External)

Pods on non-gateway nodes need to reach external destinations whose routes are
only known to gateway nodes (BGP-learned routes). Traffic must be steered
through a gateway node using OVN logical router reroute policies on the
`ovn_cluster_router`.

This uses the same mechanism already proven by EgressIP:

```
# On ovn_cluster_router (for each non-gateway node's pod subnet):
LogicalRouterPolicy {
    action:   reroute
    match:    "ip4.src == 10.244.0.0/24"         # non-gateway node's pod subnet
    nexthops: ["100.88.0.2", "100.88.0.3"]       # transit switch IPs of gateway nodes
    priority: <lower than existing EIP no-reroute policies (102)>
}
```

In an IC (interconnect) cluster, nexthops are **transit switch port IPs** of the
gateway nodes. In a non-IC cluster, nexthops would be the **join switch IPs**.

Existing higher-priority policies protect intra-cluster traffic:

| Priority | Policy                        | Effect                                                          |
|----------|-------------------------------|-----------------------------------------------------------------|
| 1004     | Node subnet reroute           | Pod → own node IP via mgmt port                                 |
| 102      | EIP no-reroute: Pod-to-Pod    | `ip4.src == 10.244.0.0/16 && ip4.dst == 10.244.0.0/16` → allow  |
| 102      | EIP no-reroute: Pod-to-Join   | `ip4.src == 10.244.0.0/16 && ip4.dst == 100.64.0.0/16` → allow  |
| 102      | EIP no-reroute: Pod-to-Node   | Pod subnets → node IPs → allow                                  |
| ≤100     | **BGP gateway reroute (NEW)** | Non-gateway pod subnets → gateway transit IPs                   |

Only N/S (external-bound) traffic falls through to the reroute policy. E/W
traffic is handled at higher priorities and is never rerouted.

Multiple nexthops provide **ECMP** load-balancing across gateway nodes. BFD
sessions can be configured for fast failover:

```
LogicalRouterPolicy {
    action:      reroute
    match:       "ip4.src == 10.244.0.0/24"
    nexthops:    ["100.88.0.2", "100.88.0.3"]
    bfd_sessions: [<bfd-uuid-1>, <bfd-uuid-2>]
    priority:    <appropriate>
}
```

#### SNAT Behavior

When a network is advertised via BGP, SNAT is already conditionally disabled
(as documented in the existing BGP implementation). This remains unchanged —
pod traffic exits with the pod IP as source, which is the expected behavior for
advertised subnets.

#### Traffic Flow Summary

For a 4-node cluster where ovn-worker and ovn-worker2 are gateway nodes:

```
INGRESS (external → pod on ovn-worker3):

  external
    │
    ▼
  gateway node (ovn-worker)
    breth0 → flow matches 10.244.0.0/24
    │
    SGW: → patch port → OVN pipeline
    LGW: → LOCAL → host kernel → mp0 → OVN pipeline
    │
    ▼
  ovn_cluster_router → Geneve tunnel → ovn-worker3 → pod


EGRESS (pod on ovn-worker3 → BGP-learned external destination):

  pod on ovn-worker3
    │
    ▼
  ovn_cluster_router
    │
    reroute policy: ip4.src == 10.244.0.0/24
    nexthops: [100.88.0.2, 100.88.0.3]
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

| Aspect                         | SGW                            | LGW                                                        |
|--------------------------------|--------------------------------|-------------------------------------------------------------|
| Ingress breth0 flows           | `actions=output:<patch-port>`  | `actions=LOCAL`                                             |
| Host routes for remote subnets | Not needed (stays in OVS/OVN)  | Already covered by `10.244.0.0/16 via mp0` aggregate route  |
| Egress reroute                 | OVN LRP on cluster router      | Same — reroute happens before SGW/LGW split                 |
| SNAT handling                  | Conditional SNAT on GR         | Conditional SNAT on GR + host nftables                      |

#### EgressIP compatibility

* Behavior of EgressIP with **nodeSelector** is unchanged.
* EgressIP reroute takes precedence over BGP gateway reroute for pods selected
  by EgressIP; east-west traffic remains protected by existing no-reroute rules.
* Regression tests of EgressIP on gateway vs non-gateway;
  pods on non-gateway node with and without EgressIP.

### Testing Details

#### Unit Tests

* **RouteAdvertisements controller**: Update `controller_test.go`:
  * Remove `"fails to reconcile pod network if node selector is not empty"` test.
  * Add test: gateway nodes generate FRRConfigurations with all nodes' host
    subnets when `nodeSelector` is non-empty.
  * Add test: non-gateway nodes are not included in generated
    FRRConfigurations.
  * Add test: reject `nodeSelector` + `PodNetwork` with `NoOverlay` transport.
  * Add test: Layer2 networks use cluster-wide subnets regardless of
    `nodeSelector`.
  * Add test: `nodeSelector` matching zero ready nodes reports degraded or
    pending status and does not generate `FRRConfiguration` objects.

* **networkmanager**: Test `syncRouteAdvertisements` / `podAdvertisements`
  when only a subset of nodes are BGP gateways.

* **OVN network controller**: Test that breth0 flows are created for remote pod
  subnets on gateway nodes.

* **Logical router policy**: Test that reroute policies are created on
  `ovn_cluster_router` for non-gateway node subnets with gateway node transit
  switch IPs as nexthops.

#### E2E Tests

* Deploy a Kind cluster with 4 nodes, designate 2 as gateway nodes via labels.
* Create `RouteAdvertisements` with `nodeSelector` matching the 2 gateway nodes.
* Verify:
  * External traffic reaches pods on non-gateway nodes via gateway nodes.
  * Pods on non-gateway nodes can reach BGP-learned external destinations.
  * Failover works when one gateway node is taken down.
  * Both SGW and LGW modes.

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
  * Remove the known limitation "Pod network IPs must be advertised from all
    nodes."
  * Add a new section on gateway node deployment pattern with examples.
  * Document the NoOverlay restriction.
* Update the `RouteAdvertisements` API reference documentation.
* Add `mkdocs.yml` entry for this OKEP.

## Risks, Known Limitations and Mitigations

* **Gateway bottleneck**: All N/S traffic flows through gateway nodes. Users
  should select multiple gateway nodes for ECMP and monitor gateway node
  capacity.
* **Asymmetric routing**: Ingress arrives via a gateway node; egress may exit
  from a different gateway node via ECMP. External networks must tolerate
  asymmetric paths or use loose RPF. This is standard behavior for ECMP
  deployments.
* **NoOverlay not supported**: The feature requires a tunnel overlay for
  cross-node forwarding. NoOverlay deployments must continue to advertise from
  all nodes.
* **Interaction with EgressIP**: When both EgressIP and BGP gateway NodeSelector
  are configured, the EgressIP reroute policy (priority 100) takes precedence
  for pods with EgressIP. Traffic from those pods is directed to the EgressIP
  node, not the BGP gateway node. This is the expected behavior — EgressIP
  controls source IP selection.
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
* **Existing behavior**: When `nodeSelector` is empty (selects all nodes), the
  behavior is identical to today — each node advertises only its own host
  subnet. The new "advertise all nodes' subnets" behavior only activates when
  `nodeSelector` is non-empty.
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
