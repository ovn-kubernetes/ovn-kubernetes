# OKEP-6227: EgressTrafficClass — Per-Destination Egress IP Routing

* Issue: [#6227](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6227)

## Problem Statement

The existing EgressIP feature routes all egress traffic from a matched pod through a single egress IP address. There is no mechanism to assign different egress IP addresses based on the traffic's destination network, which is required in environments where workloads communicate with multiple external networks through different interfaces.

## Goals

* Allow a cluster administrator to define destination network CIDRs via a new `EgressTrafficClass` CRD.
* Allow an `EgressIP` to select `EgressTrafficClass` resources via a `trafficSelector` label selector, routing only matching destination traffic through that egress IP.
* Support multiple `EgressIP` objects with `trafficSelector` serving the same pod, each handling different destination networks.
* Support coexistence of `trafficSelector` EgressIPs with a non-`trafficSelector` "default" EgressIP for catch-all traffic.
* Work with EgressIPs assigned to both OVN primary network interfaces and secondary host interfaces.
* Support both local and shared OVN gateway modes.
* Support EgressIPs on user-defined networks (UDNs) for OVN-network EgressIPs. Secondary host interface EgressIPs on UDNs are not supported (same as the base EgressIP limitation on UDNs).

## Non-Goals

* Dynamic route synchronization with BGP: per-destination routing tables are snapshots; BGP integration is a separate effort ([OKEP-5296](okep-5296-bgp.md)).

## Introduction

In telecommunications and carrier deployments, a single pod often needs to communicate with multiple external networks; for example, OAM (Operations, Administration, Maintenance), signaling, and user plane networks. Each network requires traffic to egress through a specific interface with a specific source IP address. The existing EgressIP feature assigns a single egress IP to all traffic from a pod, making it impossible to route different destination traffic through different interfaces.

EgressTrafficClass solves this by introducing destination-based egress routing. A new `EgressTrafficClass` CRD defines sets of destination network CIDRs, and a new `trafficSelector` field on `EgressIP` selects which `EgressTrafficClass` resources apply. Only traffic matching the selected destination networks is routed through the EgressIP; all other traffic follows normal OVN routing or is handled by a non-`trafficSelector` default EgressIP.

### Behavioral change: multi-EgressIP pod support

Prior to this OKEP, having multiple `EgressIP` objects selecting the same pod was considered **undefined behavior**. The controller would arbitrarily pick one EgressIP as the "primary" and put the rest on standby. There was no guarantee about which EgressIP would serve the pod, and failover between them was non-deterministic.

This OKEP changes that behavior to be **deterministic** when `trafficSelector` is used:

- Multiple `EgressIP` objects with different `trafficSelector` values can coexist on the same pod, each handling traffic to different destination networks.
- A non-`trafficSelector` "default" EgressIP can coexist with `trafficSelector` EgressIPs, acting as a catch-all for traffic not matching any destination network.
- Priority-based LRP evaluation (100 for `trafficSelector`, 99 for default) ensures consistent, predictable routing.
- Without `trafficSelector`, the existing behavior is unchanged: multiple EgressIPs selecting the same pod remain undefined.

### Example topology

```text
                        ┌───────────────────────────────────────────┐
                        │              Egress Node                  │
                        │                                           │
Pod ──OVN──►            │  ┌─ eth-mgmt ──► Mgmt Network             │
  10.244.1.3            │  │   (192.168.150.101)  (192.168.250.0/24)│
                        │  │                                        │
         reroute ──────►│──┤                                        │
                        │  │                                        │
                        │  └─ eth-sig  ──► Signaling Network        │
                        │      (192.168.200.101)  (192.168.251.0/24)│
                        └───────────────────────────────────────────┘
```

## User-Stories/Use-Cases

**Story 1: Per-destination egress IP for multi-network workloads**

As a cluster administrator, I want pods in a telco namespace to use different source IP addresses depending on which backend network they are reaching, so that each network sees traffic from the correct interface and IP range.

**Story 2: Default egress IP with destination-specific overrides**

As a cluster administrator, I want a "default" EgressIP that handles general internet traffic, combined with destination-specific EgressIPs for particular backend networks, so that pods have predictable source IPs for all egress traffic without requiring per-destination configuration for every possible destination.

## Proposed Solution

### API Details

#### EgressTrafficClass CRD

A new cluster-scoped custom resource that defines destination network CIDRs. The name `EgressTrafficClass` is intentionally generic so other features (e.g., EgressFirewall, EgressQoS) may leverage it in the future:

```go
// +genclient
// +genclient:nonNamespaced
// +kubebuilder:resource:shortName=etc,scope=Cluster
type EgressTrafficClass struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata"`
    Spec              EgressTrafficClassSpec `json:"spec"`
}

type EgressTrafficClassSpec struct {
    // DestinationNetworks is the list of Network CIDRs that defines external
    // destinations for IPv4 and/or IPv6.
    // +kubebuilder:validation:MaxItems=25
    DestinationNetworks []CIDR `json:"destinationNetworks,omitempty"`
}

// CIDR is a network CIDR. IPv4 or IPv6.
// +kubebuilder:validation:MaxLength=43
// +kubebuilder:validation:XValidation:rule="isCIDR(self)"
type CIDR string
```

Key points:
- Cluster-scoped (not namespaced).
- Labels on the object are used for selection by `EgressIP.spec.trafficSelector`.
- CIDRs are validated at admission time via CEL (`isCIDR(self)`).
- Maximum 25 destination networks per resource.

#### EgressIP TrafficSelector field

A new optional pointer field on `EgressIP.spec`:

```go
type EgressIPSpec struct {
    EgressIPs         []string              `json:"egressIPs"`
    NamespaceSelector metav1.LabelSelector  `json:"namespaceSelector"`
    PodSelector       metav1.LabelSelector  `json:"podSelector"`
    TrafficSelector   *metav1.LabelSelector `json:"trafficSelector,omitempty"` // NEW
}
```

The field is a pointer to distinguish between:
- **nil** (field absent): no traffic selector — all traffic is selected (existing behavior, fully backwards compatible).
- **empty** (`trafficSelector: {}`): matches all `EgressTrafficClass` resources (selects all labeled destination networks).
- **non-empty** (`trafficSelector: {matchLabels: {...}}`): matches only `EgressTrafficClass` resources whose labels satisfy the selector.

When `trafficSelector` is set (non-nil), the EgressIP only handles traffic to the destination networks defined in matching `EgressTrafficClass` resources.

**When no `EgressTrafficClass` matches the `trafficSelector`**: The destination networks address set is empty, the reroute LRP matches no traffic, and the EgressIP effectively stops routing traffic. A warning is logged and a Kubernetes event is emitted on the EgressIP object:

```
Warning  NoDestinationNetworks  TrafficSelector is set but no matching EgressTrafficClass
                                 destination networks were found; traffic will not be routed
                                 via this EgressIP until matching EgressTrafficClass resources
                                 are created
```

This is by design: no matching destination networks means no traffic should use this EgressIP. Operators should create the required `EgressTrafficClass` resources or verify the `trafficSelector` labels.

#### EgressIP Status Conditions

Two new conditions are added to the EgressIP status to improve observability:

```go
type EgressIPStatus struct {
    Items      []EgressIPStatusItem `json:"items"`
    Conditions []metav1.Condition   `json:"conditions,omitempty"` // NEW
}
```

**`TrafficSelectorResolved`**: Set to `True` when the `trafficSelector` successfully resolves to at least one `EgressTrafficClass` with destination networks. Set to `False` when `trafficSelector` is set but no matching `EgressTrafficClass` resources are found.

```yaml
conditions:
- type: TrafficSelectorResolved
  status: "True"
  reason: DestinationNetworksFound
  message: "Resolved 2 destination networks from 1 EgressTrafficClass resource(s)"
```

```yaml
conditions:
- type: TrafficSelectorResolved
  status: "False"
  reason: NoMatchingEgressTrafficClass
  message: "No EgressTrafficClass resources match trafficSelector"
```

**`TrafficConflict`**: Set to `True` when overlapping destination CIDRs are detected across multiple EgressIPs with `trafficSelector` targeting the same pod. The controller performs cross-EgressIP analysis during pod assignment to identify CIDRs that overlap between different EgressIPs.

```yaml
conditions:
- type: TrafficConflict
  status: "True"
  reason: OverlappingDestinationCIDR
  message: "Destination CIDR 192.168.250.0/24 overlaps with EgressIP eip-signaling for pod selector app=telco-workload"
```

#### Example resources

```yaml
apiVersion: k8s.ovn.org/v1
kind: EgressTrafficClass
metadata:
  name: mgmt-traffic
  labels:
    traffic-group: mgmt
spec:
  destinationNetworks:
  - "192.168.250.0/24"
---
apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
  name: eip-mgmt
spec:
  egressIPs:
  - 192.168.150.101
  namespaceSelector:
    matchLabels:
      env: production
  podSelector:
    matchLabels:
      app: telco-workload
  trafficSelector:
    matchLabels:
      traffic-group: mgmt
```

### Feature Gate

The feature is gated by `--enable-egress-traffic-class` (config: `enable-egress-traffic-class`). When enabled, it implicitly enables `--enable-egress-ip` since EgressTrafficClass extends EgressIP. When disabled (default), the EgressTrafficClass informer is not registered, `trafficSelector` on EgressIP is ignored, and no destination-filtered LRPs or NATs are created.

### Implementation Details

The feature is implemented across two controllers:

1. **OVN central controller** (`pkg/ovn/egressip.go`): Manages OVN logical router policies (LRPs), NAT rules, and address sets in the NB database.
2. **Node controller** (`pkg/node/controllers/egressip/egressip.go`): Manages Linux IP rules, routing tables, and iptables SNAT rules on the egress node.

#### OVN central controller changes

**Destination networks address set**: For each EgressIP with `trafficSelector`, an OVN address set is created containing the destination CIDRs from all matching `EgressTrafficClass` resources. The address set is updated when `EgressTrafficClass` resources are created, updated, or deleted.

**Destination-filtered reroute LRP**: The reroute logical router policy match is extended with a destination filter when `trafficSelector` is set:

```text
# With trafficSelector (priority 100):
ip4.src == 10.244.1.3 && ip4.dst == $<dest-nets-addr-set>    reroute    10.244.2.2

# Without trafficSelector, standard behavior (priority 100):
ip4.src == 10.244.1.3                                         reroute    100.64.0.3

# Without trafficSelector, coexisting as default (priority 99):
ip4.src == 10.244.1.3                                         reroute    100.64.0.3
```

**Destination-filtered NAT**: When the EgressIP is on the OVN network and has `trafficSelector`, the SNAT rule on the gateway router includes a destination match to avoid conflicts with other EgressIPs on the same node:

```text
snat    172.18.0.200    10.244.1.3    match="ip4.dst == $<dest-nets-addr-set>"    (trafficSelector)
snat    172.18.0.100    10.244.1.3                                                (default, no match)
```

**EgressIP coexistence**: Multiple EgressIPs can serve the same pod when at least one has `trafficSelector`. The priority scheme ensures correct evaluation order:

| EgressIP | TrafficSelector | LRP Priority | IP Rule Priority | Role |
|----------|----------------|-------------|-----------------|------|
| eip-mgmt | yes | 100 | 6000 (per-dst) | Handles traffic to 192.168.250.0/24 |
| eip-signaling | yes | 100 | 6000 (per-dst) | Handles traffic to 192.168.251.0/24 |
| eip-default | no | 99 | 6001 (catch-all) | Handles all other traffic |

**SNAT rule collision**: When two trafficSelector EgressIPs target the same pod and the same secondary host interface, they share the same SNAT IP (since the EgressIP address is assigned to that interface). The iptables SNAT rule (`-s <podIP> -o <iface> -j SNAT --to-source <egressIP>`) is identical for both, so no collision occurs. Different interfaces produce different `-o` flags, also preventing collision.

**Reconciliation**: `EgressTrafficClass` changes trigger re-reconciliation of all EgressIPs with matching `trafficSelector`. TrafficSelector changes on an existing EgressIP (CASE 3.0) trigger a full teardown and rebuild of assignments.

#### Node controller changes

**Per-destination IP rules**: Instead of a single catch-all IP rule per pod, one IP rule per destination CIDR is created at priority 6000:

```text
6000: from 10.244.1.3 to 192.168.250.0/24 lookup 1026
6000: from 10.244.1.3 to 192.168.251.0/24 lookup 1028
```

Non-`trafficSelector` catch-all rules use priority 6001:

```text
6001: from 10.244.1.3 lookup 1028
```

**Destination-specific routing table**: The default route is replaced with destination-specific routes, preserving the gateway from the original default route. If a specific route for the destination CIDR already exists on the interface (e.g., configured by the network administrator), it is preserved with its gateway rather than replaced. Non-matching traffic falls through to the main routing table:

```text
# With trafficSelector:
192.168.250.0/24 via 192.168.150.1 dev eth-mgmt
192.168.150.0/24 dev eth-mgmt proto kernel scope link

# Without trafficSelector (standard):
default via 192.168.150.1 dev eth-mgmt
192.168.150.0/24 dev eth-mgmt proto kernel scope link
```

**iptables SNAT deduplication**: When multiple destination CIDRs use the same interface, the iptables SNAT rule is created only once (on the first destination CIDR config) to avoid duplicate add/delete issues.

**EgressTrafficClass informer**: The node controller watches `EgressTrafficClass` resources and re-queues affected EgressIPs when destination networks change. The update handler only processes the old object when labels changed (optimization to avoid unnecessary re-reconciliation when only `destinationNetworks` change).

#### Differences between LGW and SGW modes

The feature works identically in both local and shared gateway modes. The destination-filtered LRP match, address sets, NAT rules, and node-side constructs are gateway-mode-agnostic.

### Testing Details

#### Unit Tests

**OVN controller tests** (`pkg/ovn/egressip_test.go`):
- `hasTrafficSelector`: nil, empty, MatchLabels, MatchExpressions
- `egressStatuses.containsAny`: empty, matching, non-matching
- LRP match parsing: standard match, destination-filtered match, too-short match, IPv6
- NAT match: IPv4/IPv6 with and without destination match
- EgressIP reroute priority: non-trafficSelector uses 99, trafficSelector uses 100

**Node controller tests** (`pkg/node/controllers/egressip/egressip_test.go`):
- TrafficSelector EgressIP with single destination network
- TrafficSelector EgressIP with multiple destination networks (SNAT deduplication)
- Non-TrafficSelector EgressIP with lower priority catch-all rule
- `replaceDefaultRouteWithDestNetworks`: default route removal, gateway preservation, existing specific route preservation, IPv4/IPv6 filtering, empty destinations
- `hasRouteForDst`: matching CIDR, different CIDR, same IP different mask, empty routes
- `containsRoute`: matching, non-matching, empty
- `getRoutesForOtherEIPsOnLink`: co-located routes, empty when alone

#### E2E Tests

Six E2E test cases tagged `[secondary-host-eip] [traffic-selector]`:

1. **Matching and non-matching traffic**: Verifies matching destination traffic uses EgressIP, non-matching uses node IP, and EgressTrafficClass deletion breaks connectivity to the destination network.
2. **Multiple TrafficSelector EgressIPs**: Two secondary networks, two EgressTrafficClass CRs, two EgressIPs; verifies each destination uses the correct EgressIP.
3. **Coexistence**: TrafficSelector EgressIP on secondary host network (priority 100) + non-TrafficSelector default EgressIP on OVN network (priority 99); verifies both work independently.
4. **EgressTrafficClass destinationNetworks update**: Changes CIDRs on a live EgressTrafficClass, verifies traffic transitions from non-matching to matching.
5. **EgressTrafficClass label change**: Changes labels so the EgressTrafficClass stops matching the trafficSelector, verifies traffic stops routing via EgressIP.
6. **TrafficSelector added to existing EgressIP**: Starts with standard EgressIP, adds trafficSelector, verifies transition from catch-all to destination-filtered routing.

All E2E tests pass on both local and shared gateway modes. Tests skip when the EgressTrafficClass feature gate is not enabled.

### Documentation Details

Feature documentation will be added at `docs/features/cluster-egress-controls/egress-ip-traffic.md` covering:
- Workflow with YAML examples (basic and advanced setups)
- Implementation details with traffic flow diagrams
- OVN and node-side construct descriptions
- Troubleshooting guide
- Best practices, design notes, known limitations

## Risks, Known Limitations and Mitigations

- **Overlapping destination CIDRs**: If two EgressIPs with different `trafficSelector` match overlapping CIDRs for the same pod, the OVN LRP behavior at the same priority is undefined. Mitigation: the `TrafficConflict` status condition detects and surfaces overlapping CIDRs; operators should resolve them by adjusting destination networks.
- **BGP route staleness**: Per-destination routing tables on egress nodes are snapshots. Dynamic route changes (e.g., via BGP) are not automatically reflected until the next sync or EgressIP reconciliation. Mitigation: future BGP integration ([OKEP-5296](okep-5296-bgp.md)) will address this with netlink route subscriptions.
- **UDN secondary host interface**: EgressIPs with `trafficSelector` on user-defined networks are supported for OVN-network EgressIPs (where SNAT happens on the gateway router). Secondary host interface EgressIPs on UDNs are not supported (same as the base EgressIP UDN limitation).
- **Catch-all secondary host EgressIP with OVN-network trafficSelector EgressIP in IC mode**: A catch-all EgressIP (no `trafficSelector`) on a secondary host interface captures ALL pod traffic at the kernel IP rule level (priority 6001). In interconnect (IC) mode, this prevents coexisting `trafficSelector` EgressIPs on the OVN primary network from working for the same pod, because both the secondary-host and OVN-network EgressIP LRPs reroute to the same transit switch nexthop. The packet arrives at the egress node's management port and is intercepted by the catch-all IP rule before the OVN gateway router can apply SNAT for the OVN-network EgressIP. In non-IC mode (single zone), this works correctly because the OVN-network EgressIP reroutes to the gateway router nexthop directly.

## OVN-Kubernetes Version Skew

This feature is proposed for introduction in a future release. The `EgressTrafficClass` CRD and `trafficSelector` field are additive; existing EgressIP objects without `trafficSelector` continue to work unchanged.

## Backwards Compatibility

Fully backwards compatible:
- The `trafficSelector` field on `EgressIP` is optional (pointer, nil by default). Existing EgressIP objects without it behave exactly as before.
- The `EgressTrafficClass` CRD is new and has no impact on existing resources.
- Non-`trafficSelector` EgressIPs use IP rule priority 6001 (changed from 6000) on the node side. The `repairNode` sync handles the transition by scanning both priorities and cleaning up stale rules.

## Alternatives

1. **Per-pod secondary network attachments**: Instead of destination-based routing, each pod could attach to multiple networks directly. This requires application-level awareness and significantly complicates pod networking.
2. **EgressFirewall with SNAT**: Using EgressFirewall rules combined with custom SNAT configurations. This would require significant changes to the EgressFirewall feature and doesn't integrate with EgressIP's node assignment and HA capabilities.
3. **Destination CIDRs inline on EgressIP**: Instead of a separate `EgressTrafficClass` CRD, destination networks could be specified directly on the `EgressIP` spec. This was rejected because it doesn't allow reuse of destination network definitions across multiple EgressIPs and makes the EgressIP spec overly complex.

## References

- [EgressIP documentation](../features/cluster-egress-controls/egress-ip.md)
- [GitHub Issue #6227](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6227)
