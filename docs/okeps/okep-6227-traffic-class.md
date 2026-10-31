# OKEP-6227: TrafficClass — Selective EgressIP

* Issue: [#6227](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6227)

## Problem Statement

An EgressIP applies to all egress traffic from selected pods. Multiple addresses within one EgressIP provide ECMP, but cannot select a source IP or egress interface by destination CIDR, protocol, or port.

## Goals

* Allow a cluster administrator to define traffic matches (destination CIDRs with optional L4 protocol/port filtering) via a new `TrafficClass` CRD.
* Allow an `EgressIP` to select `TrafficClass` resources via a `trafficSelector` label selector, routing only matching traffic through that egress IP.
* Support TCP, UDP, and SCTP filtering with optional source and destination ports or port ranges.
* Support multiple `EgressIP` objects with `trafficSelector` serving the same pod, each handling different destination networks or protocols.
* Support coexistence of `trafficSelector` EgressIPs and a non-`trafficSelector` default EgressIP on the OVN network and secondary host interfaces, with either interface type serving as the default on the cluster default network (Story 2).
* Work with EgressIPs assigned to both OVN primary network interfaces and secondary host interfaces.
* Support both local and shared OVN gateway modes.
* Support OVN-network EgressIPs on user-defined networks (UDNs) using overlay transport.

## Non-Goals

* Protocol-specific matching for ICMP and other non-port protocols. CIDR-only matches still include these protocols.
* Consumption of `TrafficClass` by APIs other than EgressIP, and source-address matching (`sourceCIDR`).
* EgressIP support on no-overlay networks ([OKEP-5259](okep-5259-no-overlay.md)).

## Future Goals

* Add source-address matching and additional `TrafficClass` consumers. Each consumer defines the action and whether matches apply to ingress or egress traffic.
* Add structured matching for ICMP and other protocols.
* Support secondary host interface EgressIPs on UDNs once the base feature supports them. Reuse the destination-specific host IP rules and SNAT within the UDN's VRF. Uplink-backed CUDN gateways ([OKEP-6019](okep-6019-vrf-lite-shared-gateway-external-bridges.md)) are a possible integration point.

## Introduction

Telecommunications workloads often reach several external networks, such as OAM, signaling, and user plane. Each network may require a different egress interface and a stable source IP for its ACLs. Different services on the same network may also need different EgressIPs. Node-IP masquerading cannot provide addresses that remain stable when the egress assignment moves between nodes.

This proposal adds a `TrafficClass` CRD for destination CIDRs and optional protocol/port filters. An EgressIP selects classes through `trafficSelector` and handles only matching traffic. Other traffic uses a coexisting default EgressIP, or normal OVN egress if no default EgressIP is assigned. An EgressIP without `trafficSelector` remains a catch-all.

### Behavioral change: multi-EgressIP pod support

Today, multiple EgressIPs selecting the same pod have undefined behavior: the controller chooses one to serve the pod and leaves the others on standby.

With `trafficSelector`, the behavior becomes deterministic:

- Multiple filtered EgressIPs can serve the same pod with disjoint destination or protocol/port matches. Overlapping matches follow the conflict-resolution rules below.
- On the cluster default network, a non-`trafficSelector` default EgressIP on either the OVN network or a secondary host interface can coexist with `trafficSelector` EgressIPs on the other interface type, handling traffic not matched by their destination and protocol/port filters.
- Filtered reroute LRPs use priority 100; catch-all LRPs move to 99. EgressService (101), no-reroute (102), and EgressIP QoS (103) retain their precedence. See **Upgrade Sequence** for migration.
- Multiple catch-all EgressIPs selecting the same pod remain undefined. A standalone catch-all retains its routing and SNAT behavior; only its LRP priority changes from 100 to 99.

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

As a cluster administrator, I want telco pods to use the source IP and interface required by each backend network.

**Story 2: Default egress IP with destination-specific overrides**

As a cluster administrator, I want a default EgressIP with destination-specific overrides, allowing either to use secondary host interfaces.

**Story 3: Per-protocol/port egress routing on the same network**

As a cluster administrator, I want SIP (UDP destination port 5060) and SSH (TCP destination port 22) to the same network to use different EgressIPs and interfaces.

**Story 4: Local RTP port range as source-port match**

As a cluster administrator, I want RTP traffic with UDP source ports 16384–32767 to use a dedicated EgressIP when remote destination ports are unknown.

## Proposed Solution

### API Details

#### TrafficClass CRD

A new cluster-scoped custom resource that defines matches on packet fields. Each match requires `destinationCIDR` and can include protocol, source-port, and destination-port filters. These fields retain their packet-header meaning regardless of the consumer's direction. Action and ingress/egress direction belong to the consumer; EgressIP implicitly applies the matches to egress traffic. This OKEP defines only the EgressIP consumer.

```go
// +genclient
// +genclient:nonNamespaced
// +kubebuilder:resource:shortName=tc,scope=Cluster
type TrafficClass struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata"`
    Spec              TrafficClassSpec `json:"spec"`
}

type TrafficClassSpec struct {
    // Matches defines packet matches with a required destination CIDR and optional L4 protocol/port filters.
    // +kubebuilder:validation:MaxItems=25
    // +kubebuilder:validation:MinItems=1
    Matches []TrafficMatch `json:"matches"`
}

// TrafficMatch matches a packet's destination address and optional protocol and port fields.
type TrafficMatch struct {
    // DestinationCIDR matches the packet's destination address against an IPv4 or IPv6 network CIDR. Required.
    // isCIDR() also accepts host-and-mask values (k8s.io/kubernetes#134224);
    // require the canonical network address.
    // +kubebuilder:validation:XValidation:rule="isCIDR(self) && cidr(self) == cidr(self).masked()",message="CIDR must be a valid network address"
    DestinationCIDR string `json:"destinationCIDR"`

    // Protocols is an optional list of L4 matches. When empty or omitted, all
    // protocols and ports to the CIDR match. When set, traffic must match at
    // least one entry (OR semantics). Exactly one protocol field must be set
    // per entry so unknown future protocols decode as {} for older clients
    // (NPEP-187 option 3).
    // +optional
    Protocols []ProtocolMatch `json:"protocols,omitempty"`
}

// ProtocolMatch is a one-of union. This OKEP supports TCP, UDP, and SCTP. A future
// OKEP can add ICMP (type/code) or other protocol structs without renaming
// this field from protocols to ports.
// +kubebuilder:validation:XValidation:rule="(has(self.tcp)?1:0)+(has(self.udp)?1:0)+(has(self.sctp)?1:0)==1",message="exactly one of tcp, udp, or sctp must be set"
type ProtocolMatch struct {
    TCP  *PortProtocol `json:"tcp,omitempty"`
    UDP  *PortProtocol `json:"udp,omitempty"`
    SCTP *PortProtocol `json:"sctp,omitempty"`
}

type PortProtocol struct {
    // DestinationPort matches the packet's destination port. Optional.
    // +optional
    DestinationPort *Port `json:"destinationPort,omitempty"`

    // SourcePort matches the packet's source port. Optional.
    // +optional
    SourcePort *Port `json:"sourcePort,omitempty"`
}

// Port is exactly one of Number or Range.
// +kubebuilder:validation:XValidation:rule="(has(self.number)?1:0)+(has(self.range)?1:0)==1",message="exactly one of number or range must be set"
type Port struct {
    // +optional
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=65535
    Number *int32 `json:"number,omitempty"`

    // +optional
    Range *PortRange `json:"range,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.end >= self.start",message="end must be >= start"
type PortRange struct {
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=65535
    Start int32 `json:"start"`
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=65535
    End int32 `json:"end"`
}
```

Key points:
- Cluster-scoped (not namespaced).
- Labels on the object are used for selection by `EgressIP.spec.trafficSelector`.
- CIDRs are validated at admission time via CEL (`isCIDR(self) && cidr(self) == cidr(self).masked()`), so host-and-mask values like `192.168.1.1/24` are rejected ([kubernetes#134224](https://github.com/kubernetes/kubernetes/issues/134224)).
- Maximum 25 matches per resource; at least one required. Each match requires `destinationCIDR`.
- `protocols` follows [NPEP-187](https://network-policy-api.sigs.k8s.io/npeps/npep-187-ports-and-protocols/) option 3: nested `tcp` / `udp` / `sctp` keys and optional `destinationPort` / `sourcePort` (`number` or `range`). OR across list entries; AND within an entry when both ports are set. Exactly one protocol key per entry.
- A match with no `protocols` matches all traffic to its `destinationCIDR`.
- A protocol-only match, such as all TCP traffic to the CIDR, omits both port fields.
- **Overlapping matches** are checked during reconciliation, when the selected classes and pods are known:

| Where | Allowed? |
|--------|----------|
| Two matches in one `TrafficClass` | Yes (redundant). |
| Two `TrafficClass` resources selected by the **same** EgressIP | Yes (same SNAT). |
| Two `TrafficClass` resources / EgressIPs, **different** pods | Yes. |
| Two `trafficSelector` EgressIPs, **same** pod, overlapping traffic | Only the conflict winner serves the pod; the loser reports `TrafficConflict` (see below). |

#### EgressIP TrafficSelector field

A new optional pointer field on `EgressIP.spec`:

```go
type EgressIPSpec struct {
    EgressIPs         []string              `json:"egressIPs"`
    NamespaceSelector metav1.LabelSelector  `json:"namespaceSelector"`
    // +optional
    PodSelector       metav1.LabelSelector  `json:"podSelector,omitempty"`
    // +kubebuilder:validation:XValidation:rule="(has(self.matchLabels) && size(self.matchLabels) > 0) || (has(self.matchExpressions) && size(self.matchExpressions) > 0)",message="trafficSelector must specify matchLabels or matchExpressions"
    TrafficSelector   *metav1.LabelSelector `json:"trafficSelector,omitempty"` // NEW
}
```

The field is a pointer to distinguish:
- **nil** (field absent): catch-all EgressIP behavior.
- **set**: selects `TrafficClass` resources by label. CEL requires at least one entry in `matchLabels` or `matchExpressions`; `trafficSelector: {}` is rejected.

When `trafficSelector` is set (non-nil), the EgressIP only handles traffic defined in matching `TrafficClass` resources.

**When no `TrafficClass` matches the `trafficSelector`**: The EgressIP handles no traffic and reports `TrafficSelectorResolved=False` with reason `NoMatchingTrafficClass`. The controller logs a warning and emits a `NoTrafficClass` event. Traffic uses a coexisting default EgressIP or normal OVN egress.

**Deleting or relabeling a `TrafficClass`**: Reconciliation removes its matches from EgressIPs that no longer select it. Other selected classes still apply; if none remain, `TrafficSelectorResolved=False` and traffic uses a coexisting default EgressIP or normal OVN egress. No finalizer is added because label changes can also remove a class from selection. Check affected EgressIPs before deleting or relabeling a class.

#### EgressIP Status Conditions

Two new conditions are added to the EgressIP status to improve observability:

```go
type EgressIPStatus struct {
    Items      []EgressIPStatusItem `json:"items"`
    Conditions []metav1.Condition   `json:"conditions,omitempty"` // NEW
}
```

**`TrafficSelectorResolved`**: With the feature enabled, set to `True` with reason `MatchesFound` when the selector resolves at least one supported traffic match. This remains `True` when no pods are selected or `TrafficConflict` prevents programming; the condition describes selector resolution independently of pod configuration. Set to `False` with one of these reasons when resolution fails:
- `NoMatchingTrafficClass`: No classes match the selector. Create a matching class or correct its labels.
- `AllMatchesUnsupported`: Classes matched, but all protocol entries were unsupported. Upgrade ovnkube-controller or use supported protocols; see **Unrecognized protocol entries**.
- `FeatureGateDisabled`: TrafficClass is disabled; see **Feature Gate**.

```yaml
conditions:
- type: TrafficSelectorResolved
  status: "True"
  reason: MatchesFound
  message: "Resolved 2 matches from 1 TrafficClass resource(s)"
```

```yaml
conditions:
- type: TrafficSelectorResolved
  status: "False"
  reason: NoMatchingTrafficClass
  message: "No TrafficClass resources match trafficSelector"
```

**`TrafficConflict`**: Set to `True` on the losing EgressIP when filtered EgressIPs select overlapping traffic for the same pod. Overlap within a class, or between classes selected by the same EgressIP, is allowed.

Overlap rules:
- A CIDR-only match overlaps any match with an intersecting CIDR, regardless of protocol or port filters.
- Two L4 matches overlap when their CIDRs intersect and at least one entry pair has the same protocol, overlapping destination ports, and overlapping source ports. An unset port field matches all ports on that side.
- Different protocols, or disjoint ranges for the same port field, do not overlap. A destination-port-only entry and a source-port-only entry of the same protocol do overlap when their CIDRs intersect, because each leaves the other port unconstrained.
- The controller checks overlap during pod assignment and identifies the conflicting EgressIP and matches in the condition message.

**Conflict resolution**: For each pod, sort candidate `trafficSelector` EgressIPs by ascending `metadata.creationTimestamp`, then lexicographic `metadata.name`. Starting with no winners, select each candidate only if its matches overlap no winner selected in this computation. Rejected candidates cannot block later candidates. Remove or omit all OVN and host configuration for each rejected candidate on that pod, including non-overlapping destinations, and report `TrafficConflict=True`. Recompute all affected candidates when matches, selectors, or EgressIPs change, giving currently programmed EgressIPs no preference. Apply the same rule after restart so event order cannot change the result.

```yaml
# The newer EgressIP loses; the older eip-signaling keeps serving this pod.
conditions:
- type: TrafficConflict
  status: "True"
  reason: OverlappingTraffic
  message: "Destination 192.168.250.0/24 UDP/5060 overlaps with EgressIP eip-signaling for pod selector app=telco-workload; not programmed"
```

**Conflict examples**:

1. **Overlapping classes selected by one EgressIP**: `eip-a` selects Class A, containing `192.168.250.0/24`. A new Class B, containing `192.168.250.128/25`, also matches its selector. Traffic to `192.168.250.130` matches both classes, but both matches use the same EgressIP address. The controller combines the matches, keeps `eip-a` configured, and raises no `TrafficConflict`.
2. **A class edit creates a conflict between two EgressIPs**: In a separate setup, both EgressIPs select pod P. The older `eip-a` selects Class A (`192.168.250.0/24`) and uses source address `192.168.150.101`; the newer `eip-b` selects Class B (`192.168.251.0/24`) and uses `192.168.150.102`. Both are active. An administrator adds `192.168.251.0/24` to Class A, so both EgressIPs now match traffic to that network. The controller selects the older `eip-a` for both networks, removes `eip-b`'s configuration for P, and sets `TrafficConflict=True` on `eip-b`. All of `eip-b`'s configuration for P is removed, including any non-overlapping destinations it also served. Pods without a conflict are unaffected.
3. **The overlap is removed**: Continuing the previous example, the administrator removes the added `192.168.251.0/24` entry from Class A. The controller restores `eip-b`'s configuration for P: traffic to `192.168.250.0/24` uses `eip-a`, and traffic to `192.168.251.0/24` again uses `eip-b`. It clears `TrafficConflict` on `eip-b` once no conflicts remain across its selected pods. Neither EgressIP object needs to be deleted or recreated.
4. **Three candidates**: For one pod, A is oldest and matches X, B matches X and Y, and C is newest and matches Y; X and Y are disjoint. Select A, reject B for overlapping A on X, then select C: rejected B cannot block it on Y.
5. **Recompute after A's match is removed**: Continuing the previous example, A stops matching X. Recompute from scratch: select B, reject C for overlapping B on Y, and remove C's configuration for the pod, even though C was previously active.

#### Example resources

```yaml
# CIDR-only destination: all traffic to OAM network
apiVersion: k8s.ovn.org/v1
kind: TrafficClass
metadata:
  name: mgmt-traffic
  labels:
    traffic-group: mgmt
spec:
  matches:
  - destinationCIDR: "192.168.250.0/24"
---
# L4-filtered matches: SIP signaling and RTP media on the same network
apiVersion: k8s.ovn.org/v1
kind: TrafficClass
metadata:
  name: signaling-traffic
  labels:
    traffic-group: signaling
spec:
  matches:
  - destinationCIDR: "192.168.250.0/24"
    protocols:
    - udp:
        destinationPort:
          number: 5060
    - udp:
        destinationPort:
          range:
            start: 8000
            end: 8010
  - destinationCIDR: "192.168.250.0/24"
    protocols:
    - tcp:
        destinationPort:
          number: 22
---
# Source-port match: local RTP bind range (remote dest ports unknown)
apiVersion: k8s.ovn.org/v1
kind: TrafficClass
metadata:
  name: rtp-media
  labels:
    traffic-group: media
spec:
  matches:
  - destinationCIDR: "192.168.250.0/24"
    protocols:
    - udp:
        sourcePort:
          range:
            start: 16384
            end: 32767
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

The feature is disabled by default and controlled by `--enable-traffic-class` (config: `enable-traffic-class`). It requires `--enable-egress-ip`; enabling it without EgressIP is a configuration error.

When EgressIP is enabled but this feature is disabled, the TrafficClass informer is not started. EgressIPs with `trafficSelector` remain unconfigured, report `TrafficSelectorResolved=False` with reason `FeatureGateDisabled`, and emit a warning event. If the feature was previously enabled, the controllers remove those EgressIPs' existing OVN and host configuration during reconciliation.

### Implementation Details

The implementation will update three components:

1. **ovnkube-controller** (`go-controller/pkg/ovn/egressip.go`): LRPs, NAT, and address sets in the OVN NB database; patch only `/status/conditions`, preserving assignment items.
2. **ovnkube-node** (`go-controller/pkg/node/controllers/egressip/egressip.go` and `go-controller/pkg/node/controllers/egressip/nftables.go`): Linux IP rules, routing tables, and nftables SNAT on the egress node.
3. **ovnkube-cluster-manager** (`go-controller/pkg/clustermanager/egressip_controller.go`): patch only `/status/items`, preserving conditions.

**RBAC**: Before enabling TrafficClass, add `trafficclasses` to the `ovnkube-node` ClusterRole's existing `k8s.ovn.org` read rule (`get`, `list`, `watch`). Its binding covers both consumers through `system:ovn-nodes` with node identity enabled, or the `ovn-kubernetes/ovnkube-node` service account otherwise.

#### ovnkube-controller changes

**Pod assignments**: For each pod/network, replace the single active `egressIPName` and status list with entries keyed by EgressIP name, storing programmed statuses and resolved matches. Reconcile candidates under the pod assignment lock so disjoint filtered EgressIPs and one catch-all can coexist. Keep legacy standby handling for competing catch-alls; recompute all affected candidates when matches, selectors, or EgressIPs change. Cleanup removes only the affected entry and configuration, preserving shared pod address-set membership while another assignment is active. Startup sync recomputes winners and reconstructs active entries.

**Match parsing**: Parse entries from all selected `TrafficClass` resources into CIDR, protocol, and source/destination port or range matches. For each filtered EgressIP, collect CIDR-only matches (no `protocols`) in a destination address set and update it when selected classes change. Encode L4 matches inline, with one clause per `protocols` entry.

**Unrecognized protocol entries**: Admission validates protocols against the installed CRD schema. During version skew, an older controller may not recognize a protocol field added to that schema. It skips the affected entry, programs no LRP or NAT for it, and emits a warning event identifying the class and entry. If no supported matches remain, the EgressIP reports `TrafficSelectorResolved=False` with reason `AllMatchesUnsupported`.

**Destination-filtered reroute LRP**: The reroute LRP match combines address set matching with inline L4 conditions:

```text
# CIDR-only (priority 100):
ip4.src == 10.244.1.3 && ip4.dst == $<dest-nets-addr-set>    reroute    10.244.2.2

# L4-filtered (priority 100):
ip4.src == 10.244.1.3 && (ip4.dst == 10.0.0.0/8 && tcp && tcp.dst == 5060)    reroute    10.244.2.2

# L4-filtered with source port (priority 100):
ip4.src == 10.244.1.3 && (ip4.dst == 10.0.0.0/8 && udp && udp.src >= 16384 && udp.src <= 32767)    reroute    10.244.2.2

# Mixed CIDR-only + L4 (priority 100):
ip4.src == 10.244.1.3 && (ip4.dst == $<addrset> || (ip4.dst == 10.0.0.0/8 && tcp && tcp.dst == 5060))    reroute    10.244.2.2

# Default EgressIP with no trafficSelector (priority 99):
ip4.src == 10.244.1.3    reroute    100.64.0.3
```

Port ranges use inclusive lower- and upper-bound comparisons joined by `&&`: `tcp.dst >= 5060 && tcp.dst <= 5062`, `udp.src >= 16384 && udp.src <= 32767`.

**Destination-filtered NAT (cluster default network)**: SNAT rules for `trafficSelector` EgressIPs must use the same destination and protocol/port matches as their reroute policies. When filtered and catch-all SNAT rules coexist on a gateway router, filtered rules take precedence.

Filtered rows set `NAT.Priority` to `100`, giving them an OVN logical-flow priority of `300 + 100`. Catch-all rows leave `NAT.Priority` unset because northd's `lrouter_nat_get_priority()` derives their flow priority as `prefix_len(logical_ip) + 1`: 33 for a pod-specific IPv4 `/32` and 129 for an IPv6 `/128`. Filtered rows therefore take precedence for both IPv4 and IPv6. These priorities apply within one router.

The CIDR-only and L4-filtered configurations below are alternatives; either can coexist with the catch-all row.

```text
# CIDR-only trafficSelector (match non-empty, NAT.Priority=100 -> real flow priority 400):
snat    172.18.0.200    10.244.1.3    match="ip4.dst == $<dest-nets-addr-set>"    priority=100

# L4-filtered trafficSelectors with disjoint protocols (flow priority 400):
snat    172.18.0.201    10.244.1.3    match="ip4.dst == 192.168.250.0/24 && tcp && tcp.dst == 22"    priority=100
snat    172.18.0.202    10.244.1.3    match="ip4.dst == 192.168.250.0/24 && udp && udp.src >= 16384 && udp.src <= 32767"    priority=100

# Default EgressIP: empty match, no NAT.Priority; /32 source gives flow priority 33.
snat    172.18.0.100    10.244.1.3
```

**UDN SNAT**: UDNs use the selected EgressIP's packet mark to choose bridge OpenFlow SNAT. Filtered reroutes (100) and catch-all reroutes (99) set that mark. On L3 and L2-with-transit gateway routers, use the same pod and destination/L4 matches in filtered mark-setting policies at priority 96, ahead of catch-all policies at 95. Retain their `pkt.mark == 0` guard and the existing NodePort-reply bypass. L2 without transit uses marked reroutes directly. Non-matching traffic uses the catch-all EgressIP or normal UDN egress. Reconcile mark policies and their ownership IDs on match/assignment changes, cleanup, and restart.

**EgressIP coexistence**: Filtered LRPs at priority 100 take precedence over the catch-all at 99. For example:

| EgressIP | TrafficSelector | Interface | LRP Priority | IP Rule Priority | Role |
|----------|----------------|-----------|-------------|-----------------|------|
| eip-mgmt | yes | secondary host | 100 | 6000 (per-dst) | Handles traffic to 192.168.250.0/24 |
| eip-signaling | yes | secondary host | 100 | 6000 (per-dst+L4) | Handles UDP 5060 to 192.168.251.0/24 |
| eip-default | no | OVN network | 99 | n/a (GR SNAT) | Handles all other traffic |

**Mixed interfaces across zones**: For a pod in the egress node's zone, the reroute next hop is the gateway router for an OVN-network EgressIP or the management port for a secondary host interface EgressIP. For a pod in another zone, the pod's zone reroutes to the egress node's transit-switch IP. If both EgressIPs are assigned to the same node, their source-zone policies use the same next hop, so the egress node's zone must select the correct path.

For a filtered OVN-network EgressIP serving a pod in another zone, the controller in the egress node's zone must also install a priority-100 LRP on `ovn_cluster_router`, matching the pod IP and the same destination and protocol/port filters, with the gateway router as its next hop. This takes precedence over the secondary-interface catch-all LRP at priority 99, which reroutes non-matching traffic to the management port. Reconcile these additional policies with TrafficClass changes, pod selection changes, and EgressIP reassignment or deletion.

**Reconciliation**: On a `TrafficClass` change, reconcile EgressIPs whose `trafficSelector` matches the class’s labels. For label changes, reconcile the union of EgressIPs matching the old and new labels, adding or removing the class’s matches as needed. Changes to an EgressIP’s `trafficSelector` tear down and rebuild its assignments.

#### ovnkube-node changes

The host IP rules, routing tables, and SNAT changes below apply only to secondary-host-interface EgressIPs on the cluster default network. Support for secondary-host-interface EgressIPs on UDNs is a future goal.

**Per-destination IP rules with L4 filtering**: Instead of a single catch-all IP rule per pod, one IP rule per destination (and per `protocols` entry when L4 is set) is created at priority 6000. When protocol/port fields are set, the IP rule includes `IPProto`, `Dport`, and `Sport` fields:

```text
# CIDR-only destination:
6000: from 10.244.1.3 to 192.168.250.0/24 lookup 1026

# L4-filtered matches:
6000: from 10.244.1.3 to 192.168.250.0/24 ipproto tcp dport 5060 lookup 1026
6000: from 10.244.1.3 to 192.168.250.0/24 ipproto udp dport 5060 lookup 1026
6000: from 10.244.1.3 to 192.168.250.0/24 ipproto udp dport 8000-8010 lookup 1026
6000: from 10.244.1.3 to 192.168.250.0/24 ipproto udp sport 16384-32767 lookup 1026
```

Non-`trafficSelector` catch-all rules on a **secondary host** EgressIP use priority 6001 so destination-specific rules are evaluated first:

```text
6001: from 10.244.1.3 lookup 1028
```

When this catch-all coexists with a filtered OVN-network EgressIP, the LRPs described in **Mixed interfaces across zones** direct matching traffic to the gateway router before it enters the secondary-interface host path. An OVN-network EgressIP does not install a host IP rule.

**Shared interface routing table**: EgressIPs assigned to the same host interface share a routing table derived from its interface index. Filtered EgressIPs reuse the existing default and more-specific routes, preserving their gateways. Do not generate routes from TrafficClass destination CIDRs; destination and protocol/port filtering belongs in the IP rules. Traffic that does not match a filtered rule continues to subsequent policy-routing rules, including a catch-all EgressIP rule when present. During cleanup, preserve routes still needed by other EgressIPs.

```text
# Interface routing table used by filtered and catch-all EgressIPs:
default via 192.168.150.1 dev eth-mgmt
192.168.150.0/24 dev eth-mgmt proto kernel scope link
```

**IP rule manager**: Extend `iprulemanager.IPRule` with `IPProto` and optional source and destination port ranges. Store ranges by value with an explicit indication of whether each filter is set, so rule keys compare port values. Represent a single port as a range with equal endpoints. Preserve these fields in both `IPRuleFromNetlinkRule()` and `toNetlinkRule()`. Rule equality, reconciliation, repair, and deletion must include all filters, keeping rules that differ only by protocol or port distinct.

**Traffic-matched nftables SNAT**: The existing `egress-ip-snat` chain jumps to a filtered SNAT chain before evaluating its IPv4 and IPv6 catch-all maps. Each filtered rule matches the pod source IP, output interface, destination CIDR, and any protocol and port filters, using the same criteria as its IP rule, then applies SNAT to the selected EgressIP. Filtered EgressIPs do not create catch-all map entries. If no filtered rule matches, evaluation returns to the existing map lookups. Reconcile filtered rules atomically with nftables transactions, removing stale rules and preserving rules belonging to other EgressIPs. Preserve the existing local-gateway connmark handling and secondary-interface leak-prevention rules.

```text
# Rule order in egress-ip-snat (dual-stack example):
jump egress-ip-snat-filtered
snat to ip saddr . meta oifname map @egress-ip-snat-v4
snat to ip6 saddr . meta oifname map @egress-ip-snat-v6
```

The CIDR-only and L4 examples below are alternative configurations of the regular `egress-ip-snat-filtered` chain in the existing `inet ovn-kubernetes` table:

```text
# CIDR-only destinations:
ip saddr 10.244.1.3 meta oifname "eth-mgmt" ip daddr 192.168.250.0/24 snat ip to 192.168.150.101
ip saddr 10.244.1.3 meta oifname "eth-mgmt" ip daddr 192.168.251.0/24 snat ip to 192.168.150.102

# L4-filtered destinations with disjoint protocols:
ip saddr 10.244.1.3 meta oifname "eth-mgmt" ip daddr 192.168.250.0/24 tcp dport 22 snat ip to 192.168.150.101
ip saddr 10.244.1.3 meta oifname "eth-mgmt" ip daddr 192.168.250.0/24 udp sport 16384-32767 snat ip to 192.168.150.103
```

IPv6 rules use `ip6 saddr`, `ip6 daddr`, and `snat ip6 to`. These rules select the SNAT mapping for new connections; conntrack handles subsequent packets and replies. Non-`trafficSelector` EgressIPs retain the existing maps keyed by pod IP and output interface.

Startup and repair rebuild the jump and filtered rules from current EgressIP and TrafficClass state, and remove stale filtered EgressIP entries from the catch-all maps. Remove obsolete filtered rules on match or label changes, pod deselection, EgressIP reassignment/deletion, conflicts, and feature-gate disablement.

**TrafficClass informer**: ovnkube-node watches `TrafficClass` resources and uses the same reconciliation rules, including both old and new labels when labels change.

#### EgressIP failover with `trafficSelector`

Destination-specific routing and SNAT can increase the amount of state programmed during EgressIP failover. Implementations should batch updates where possible. Failover latency and transient packet loss should be measured at representative pod and destination counts; uninterrupted connectivity or fallback to the node IP is not guaranteed.

### Testing Details

#### CRD Admission Tests

Use `test/crd-integration/` to verify canonical CIDRs, the 1–25 match limit, protocol and port one-of rules, port bounds, range ordering, and rejection of an empty `trafficSelector`.

#### Unit Tests

**Configuration tests** (`go-controller/pkg/config/config_test.go`):
- Enabling TrafficClass without EgressIP returns a configuration error and does not implicitly enable EgressIP

**Match parsing** (`go-controller/pkg/util/egressip/match_test.go`):
- `ParseMatches`: CIDR-only, TCP/UDP/SCTP with `destinationPort.number`, `destinationPort.range`, `sourcePort.number`, `sourcePort.range`, combined dest+src, invalid CIDR, more than one protocol key, empty protocols, multiple matches
- `ParseMatches`, version skew: skip a decoded protocol entry with TCP/UDP/SCTP all nil, without widening it to an unfiltered match or rejecting the class's supported entries
- `TrafficSelectorResolved`: a selected class containing only unsupported entries produces `False` with reason `AllMatchesUnsupported`
- `UniqueCIDRs`: deduplication across matches with different L4 filters
- `SplitByL4`: separation of CIDR-only vs L4-filtered matches
- Port overlap helpers used by `TrafficConflict`
- `HasL4Filter` and `ProtocolName` helpers

**Status writes** (`go-controller/pkg/clustermanager/egressip_controller_test.go` and `go-controller/pkg/ovn/egressip_test.go`): Verify concurrent assignment and condition updates preserve each other's fields.

**ovnkube-controller tests** (`go-controller/pkg/ovn/egressip_test.go`):
- Per-pod assignment cache: disjoint filtered EgressIPs and a catch-all coexist; deleting or reassigning one preserves the others and shared pod address-set membership; restart reconstructs all active entries
- `hasTrafficSelector`: nil vs set (empty `{}` is rejected at admission)
- Supported matches with no selected pods: `TrafficSelectorResolved=True` with reason `MatchesFound`, without pod configuration
- Supported matches with all selected pods conflicting: `TrafficSelectorResolved=True` and `TrafficConflict=True`; no OVN configuration for the losing EgressIP on those pods
- Feature gate disabled with `trafficSelector` set: no LRP/NAT programmed (not treated as nil/catch-all), `TrafficSelectorResolved=False`/`reason: FeatureGateDisabled`
- Feature gate disabled after being enabled: existing LRPs, NAT rows, and destination address sets for filtered EgressIPs are removed; non-`trafficSelector` EgressIPs remain configured
- `egressStatuses.containsAny`: empty, matching, non-matching
- `buildL4MatchClauses`: single dest port, dest port range, source port, source port range, combined dest+src, multiple `protocols` entries, IP family filtering, mixed CIDR-only and L4
- `buildPortMatch`: `number` and `range` notation for dest and source
- LRP match parsing: standard match, destination-filtered match, too-short match, IPv6
- NAT match: IPv4/IPv6 with and without destination match
- NAT priority: filtered rows have a non-empty `match` and `Priority: 100`; default rows have an empty `match` and no explicit priority. Verify the expected precedence for IPv4 and IPv6
- UDN SNAT: filtered gateway mark policies at 96 precede catch-all policies at 95, preserving existing marks; L2 without transit uses marked reroutes at 100/99. Verify destination/L4 matches, ownership IDs, updates, cleanup, and restart for IPv4 and IPv6
- EgressIP reroute priority: non-trafficSelector uses 99, trafficSelector uses 100
- Mixed interfaces on the cluster default network: filtered OVN-network EgressIP and secondary-interface catch-all assigned to the same egress node, with pods in that node's zone and in another zone. Verify filtered priority-100 LRPs select the gateway router ahead of the priority-99 management-port reroute; for pods in another zone, verify the matching source-zone policy uses the transit next hop and the egress node's zone has the additional filtered LRP. Cover CIDR-only and L4 filters for IPv4 and IPv6
- Reconcile the additional LRPs in the egress node's zone on TrafficClass match/label changes, pod deselection, and EgressIP reassignment/deletion; remove stale policies and preserve the catch-all path
- `TrafficConflict`: cover CIDR-only overlaps, intersecting L4 matches, and destination-only versus source-only matches of the same protocol. Different protocols or disjoint ranges for the same port field must not conflict. Verify creationTimestamp/name ordering and removal of all losing LRPs/NAT for the affected pod, including non-overlapping destinations and already-active configuration
- Stable selection: cover timestamp/name ordering with three or more candidates (both scenarios above), reversed event order, different initially programmed assignments, and restart/resync; verify winners, `TrafficConflict`, and removal of losing OVN/host configuration
- Conflict recovery: restore the losing EgressIP's configuration when overlap is removed; clear `TrafficConflict` only when no conflicts remain across its selected pods

**ovnkube-node tests** (`go-controller/pkg/node/controllers/egressip/egressip_test.go`):
- Feature gate disabled with `trafficSelector` set: no host IP rules, routes, nftables SNAT rules, or catch-all map entries are installed for that EgressIP
- Feature gate disabled after being enabled: remove filtered EgressIPs' host IP rules, unused routes, nftables SNAT rules, and any stale catch-all map entries, preserving state needed by other EgressIPs
- TrafficSelector EgressIP with single destination
- TrafficSelector EgressIP with multiple destinations: nftables SNAT rules match each destination
- Two TrafficSelector EgressIPs on the same secondary interface for the same pod: each nftables rule includes its destination and L4 filters and selects the correct SNAT address
- Non-TrafficSelector EgressIP: lower-priority catch-all IP rule and existing nftables map entry keyed by pod IP and output interface
- `TrafficConflict`: remove the losing EgressIP's IP rules, unused routes, and SNAT for the affected pod, preserving state for other pods; restore configuration when the conflict is resolved
- Shared interface routing table: filtered and catch-all EgressIPs selecting the same or different pods use the same table; preserve its default and more-specific routes and their gateways for IPv4 and IPv6
- TrafficClass destination updates: update filtered IP rules without generating destination-specific routes or modifying shared interface routes
- Shared-route cleanup: preserve routes needed by another EgressIP when a filtered EgressIP loses all matches, is disabled by the feature gate, or is deleted; remove only unused routes
- Policy-routing fallthrough: filtered rules precede catch-all rules and leave non-matching traffic eligible for subsequent policy-routing rules

**nftables tests** (`go-controller/pkg/node/controllers/egressip/nftables_test.go` and `go-controller/pkg/node/controllers/egressip/egressip_test.go`), for IPv4 and IPv6:
- The filtered-chain jump precedes the catch-all map lookups; rules preserve CIDR-only, protocol-only, single-port, port-range, and combined source/destination-port matches
- Filtered EgressIPs have no catch-all map entries; adding a selector to an existing EgressIP removes its old map entry, while removing the selector replaces filtered rules with the catch-all entry
- Startup and repair restore the jump and expected rules, remove stale filtered rules and map entries, and preserve other EgressIPs' configuration
- Atomic reconciliation removes obsolete rules on match/label changes, pod deselection, reassignment/deletion, conflicts, and feature-gate disablement without changing other EgressIPs' rules
- Existing local-gateway connmark handling and the secondary-interface drop rules after SNAT remain configured

**IP rule manager tests** (`go-controller/pkg/node/iprulemanager/ip_rule_manager_test.go`), for IPv4 and IPv6:
- Netlink conversions in both directions preserve protocol and port filters, including absent filters, single ports, ranges, and combined source/destination filters
- Equal port ranges compare equal even when their netlink objects were allocated separately
- Rules with the same source, destination, table, and priority but different protocols or ports remain distinct
- Repair restores the complete filtered rule; deletion preserves other rules that differ only by protocol or port

#### E2E Tests

Sixteen E2E test cases: cases 1–13 and 16 are tagged `[secondary-host-eip] [traffic-selector]`; case 14 uses `[udn] [local-gateway]`; case 15 uses `[traffic-selector]` and adds `[udn]` for its UDN variants.

1. **Matching and non-matching traffic**: Matching traffic uses the EgressIP; other traffic uses the node IP. Deleting the class makes its traffic fall through to the node IP. The destination in this test accepts only the EgressIP source address, so its rejection of the fallback traffic causes the expected connectivity loss.
2. **Multiple TrafficSelector EgressIPs**: Two secondary networks, two TrafficClass CRs, two EgressIPs; verifies each destination uses the correct EgressIP.
3. **Same secondary interface, multiple EgressIPs**: Two filtered EgressIPs on the same host NIC for the same pod, first with different destinations, then with the same destination and disjoint protocol/port matches. Repeat with a catch-all EgressIP on that interface. Verify new connections use the matching filtered EgressIP, non-matching traffic uses the catch-all when present, and replies succeed. Cover IPv4 and IPv6.
4. **Coexistence**: TrafficSelector EgressIP on secondary host network (priority 100) + non-TrafficSelector default EgressIP on OVN network (priority 99); verifies both work independently.
5. **TrafficClass matches update**: Changes `matches` on a live TrafficClass, verifies traffic transitions from non-matching to matching.
6. **TrafficClass label change, both directions**: Relabel a class to start matching the selector and verify installation of its matches. Relabel it to stop matching and verify cleanup.
7. **TrafficSelector added to existing EgressIP**: Starts with standard EgressIP, adds trafficSelector, verifies transition from catch-all to destination-filtered routing.
8. **L4 filtering — matching dest port**: TCP, UDP, and SCTP traffic to a matching destination port uses the EgressIP.
9. **L4 filtering — non-matching dest port**: TCP, UDP, and SCTP traffic to the same CIDR on a non-matching destination port does not use the EgressIP.
10. **L4 filtering — mixed matches**: CIDR-only and L4-filtered matches in the same TrafficClass; the CIDR-only match routes all protocols and ports to its CIDR.
11. **L4 filtering — dest port range**: `destinationPort.range` covering the external container's port.
12. **L4 filtering — source port range**: UDP from a bound local port in `sourcePort.range` uses EgressIP; traffic from a different local port to the same CIDR does not.
13. **`trafficSelector` destination overlapping a Service IP**: A `trafficSelector` EgressIP with a destination CIDR covering a ClusterIP and a LoadBalancer/ExternalIP service in the cluster; verifies Service traffic is unaffected (still load-balanced to a backend pod, not rerouted via the EgressIP) while other traffic in the same CIDR still uses the EgressIP.
14. **L2 UDN local-gateway without transit router, catch-all EgressIP + host-CIDR policy**: Send pod traffic to another node's primary/host IP. Verify that catch-all EgressIP at priority 99 wins over the migrated host-CIDR policy at 98, preserving existing behavior. This tests priority migration independently of `trafficSelector`.
15. **Filtered + default SNAT precedence, both on OVN network**: Assign both EgressIPs to the same node. Verify matching traffic uses the filtered EgressIP's source address, other traffic uses the default EgressIP's address, and replies succeed. Cover default-network NAT priority and UDN packet-mark selection (L3 and L2 with/without transit), including NodePort replies. Run with overlay transport in both gateway modes, with CIDR-only and L4 filters.
16. **Default on secondary host interface, filtered on OVN network** (tagged `[secondary-host-eip] [traffic-selector]`): On the cluster default network, assign both EgressIPs to the same egress node. Test pods in that node's zone and in another zone, with CIDR-only and L4-filtered matches. Verify matching traffic uses the gateway router and the filtered EgressIP's source address, while non-matching traffic uses the secondary host interface and the default EgressIP's source address. Run in both local and shared gateway modes.

These sixteen tests are **planned**, not yet implemented or run. Run all cases on networks using overlay transport. Run the `trafficSelector` cases in both local and shared gateway modes, skipping them when the TrafficClass feature gate is disabled; case 14 covers its specified local-gateway topology. Use fresh connections to verify SNAT changes after configuration updates.

#### Upgrade Validation

Planned: test both component upgrade orders following **First activation**. Verify existing catch-all EgressIPs keep working during upgrade, then verify matching and non-matching traffic on OVN and secondary-host EgressIP paths after activation.

#### Rollback Validation

Planned rollback checks:
- Verify selector EgressIPs and their OVN and host configuration are removed before any unsupported binary starts.
- Verify repeatable cleanup, preservation of unrelated rules and shared routes, and downgrade blocking on failure.
- With existing catch-all EgressIPs, verify restored OVN priorities and ownership IDs, priority-6000 host rules with the same routing tables, and affected L2 UDN host-CIDR policies at 99. Verify migrated copies are removed and cleanup works after EgressIP deletion or pod deselection. Cover IPv4 and IPv6.
- For IPv4 and IPv6, delete an EgressIP or deselect its pod after rollback while another EgressIP shares the interface. Verify the pod follows its remaining routing policy and the other EgressIP continues working.

#### Cross-feature interaction testing

The priority changes must preserve existing interactions with other features:

- **EgressFirewall / NetworkPolicy egress**: Denied traffic remains blocked when selected by an EgressIP.
- **Services (ClusterIP)**: Service traffic continues to reach a backend when the TrafficClass CIDR includes the service IP. E2E case 13 covers this overlap.
- **EgressService**: Its reroute priority 101 remains above filtered EgressIP at 100 and default EgressIP at 99.
- **LoadBalancer / ExternalIP hairpin**: E2E case 13 verifies service handling when a TrafficClass includes the external or LoadBalancer IP.

#### Scale Considerations

Each TrafficClass is limited to 25 matches. Neither the number of classes selected by an EgressIP nor the protocol entries per match is capped, so the total number of resolved rules is unbounded. Admission could enforce a protocol-entry limit per match, but that would not bound the total across selected classes. This OKEP accepts the scale risk without adding either limit.

The following estimates use 50 nodes, 500 pods, 20 filtered EgressIPs, and 5 classes with 25 matches each. The baseline assumes one protocol entry per match; these are estimates, not benchmark results.

| Dimension | Formula | At the representative scale |
|---|---|---|
| Logical address-set groups per zone | 1 per filtered EgressIP with CIDR-only matches; separate backing rows for IPv4/IPv6 | ≤ 20 groups per zone |
| Resolved matches per EgressIP | 25 × (classes matched by that EgressIP's selector) | up to 125 if one EgressIP matches all 5 classes |
| IP rules per resolved match | 1 if CIDR-only, otherwise the number of protocol entries | ≥ 1, no upper bound |
| `TrafficClass` update fan-out | EgressIPs matching current labels, or the union of old and new labels on a label change | ≤ 20 |
| IP rules/node (secondary-host case) | resolved matches × rules-per-match × pods assigned to that EgressIP on that node | 125 per pod if all 5 classes match at baseline; unbounded as protocol entries grow |

### Documentation Details

Document YAML examples, controller behavior, traffic diagrams, troubleshooting, best practices, and limitations at `docs/features/cluster-egress-controls/traffic-class.md`.

## Risks, Known Limitations and Mitigations

- **Overlapping traffic**: The losing EgressIP is unconfigured for the affected pod, including its non-overlapping destinations. `TrafficConflict` identifies the peer; adjust destinations or protocols to resolve it. See **Conflict resolution** for the stable creationTimestamp/name ordering.
- **UDN secondary host interface**: Unsupported until the base secondary-interface EgressIP feature supports UDNs; see **Future Goals**.
- **Unscoped secondary-host SNAT**: Rules matching only the pod IP and interface cannot distinguish filtered EgressIPs sharing an interface. Scope SNAT to the same destination and L4 criteria as the IP rules.
- **Mixed interfaces across zones**: A secondary-interface catch-all LRP can divert traffic intended for a filtered OVN-network EgressIP if the filtered reroute is missing from the egress node's zone. Mitigation: install the filtered priority-100 LRP there for pods in other zones and verify both paths in E2E case 16.
- **Unbounded rule count**: The number of selected classes and protocol entries per match can increase OVN and host configuration without a fixed upper bound. Operators should keep selectors scoped to a small set of classes and keep protocol lists small.
- **L2 UDN local-gateway without transit router**: Catch-all EgressIP and host-CIDR policies would otherwise share priority 99 on the gateway router. Move the host-CIDR policy to 98 as described in **Upgrade Sequence**.
- **Failover cost**: Destination and protocol match counts can increase failover state and latency. Batch updates where possible and measure failover latency and transient packet loss at representative scale; see **EgressIP failover with `trafficSelector`** above.

## OVN-Kubernetes Version Skew

This feature is proposed for introduction in v1.5.0 (the PR's current milestone). The `TrafficClass` CRD and `trafficSelector` field are additive. Existing EgressIP objects without `trafficSelector` keep the same SNAT/reroute behavior; their cluster-router (or L2-no-transit GR) reroute LRPs move from priority 100 to 99.

## Backwards Compatibility

- The `trafficSelector` field on `EgressIP` is optional (pointer, nil by default). Omitting it is still catch-all EgressIP for that object.
- The `TrafficClass` CRD is new and has no impact on existing resources.

### Upgrade Sequence

Before creating EgressIPs with `trafficSelector`, install the TrafficClass CRD and updated EgressIP CRD. Wait for both CRDs to be `Established` and verify that the served EgressIP schema includes `spec.trafficSelector`. An older schema can prune the field and turn the object into a catch-all EgressIP.

**OVN NB (ovnkube-controller), LRP priority 100 → 99**: Move existing catch-all EgressIP LRPs from 100 to 99; filtered LRPs retain 100. Repair must recognize both priorities, recreate catch-all policies at 99, and delete their stale priority-100 copies. EgressService (101), no-reroute (102), and EgressIP QoS (103) retain their precedence.

**`UDNHostCIDRPolicyPriority` 99 → 98**: On L2 UDNs in local-gateway mode without a transit router, EgressIP reroutes and host-CIDR policies share the gateway router. Lower `UDNHostCIDRPolicyPriority` to 98 and migrate existing policies from 99 to 98. This avoids a collision with catch-all EgressIP at 99 and preserves the priority order: filtered EgressIP (100), catch-all EgressIP (99), host-CIDR policy (98).

**ovnkube-node, IP rule priority 6000 → 6001**: Keep filtered rules at 6000 and move catch-all rules to 6001. The current `repairNode` only scans 6000; update it to recognize and reconcile both priorities.

During rolling upgrades and rollback:

1. **First activation**: Upgrade all `ovnkube-controller` and `ovnkube-node` instances with TrafficClass disabled and verify CRD readiness. Then enable TrafficClass everywhere and complete its rollout before creating or updating selector EgressIPs. Either component may upgrade first. Older binaries ignore selectors and install catch-all configuration; admission does not block premature use.
2. **New code on the node, repair of old 6000-only state**: new `repairNode` scans both 6000 and 6001, finds the single old-style 6000 catch-all rule, and replaces it with the correct 6001 catch-all plus any 6000 per-destination rules.
3. **Rollback**: Delete selector EgressIPs and wait for OVN and host cleanup. Stop the newer `ovnkube-controller` and `ovnkube-node` instances. Restore catch-all LRPs from 99 to 100, including priority-based ownership IDs, and affected L2 UDN host-CIDR policies from 98 to 99; remove migrated copies. Restore EgressIP-owned catch-all host rules at priority 6000 for IPv4 and IPv6, preserving their routing tables, then remove their priority-6001 copies. Verify restoration and cleanup before starting older code; block downgrade on failure. Cleanup must be repeatable and preserve unrelated policies, rules, and shared routes.

## Alternatives

1. **Per-pod secondary network attachments**: Instead of destination-based routing, each pod could attach to multiple networks directly. This requires application-level awareness and significantly complicates pod networking.
2. **Adding SNAT to EgressFirewall**: EgressFirewall provides Allow/Deny filtering. Adding SNAT would also require traffic matching, EgressIP assignment, and failover support. Extending EgressIP reuses its existing assignment and availability mechanisms.
3. **Destination CIDRs inline on EgressIP**: Embedding matches in each EgressIP would duplicate definitions shared by several EgressIPs. A separate TrafficClass allows reuse.

## References

- [EgressIP documentation](../features/cluster-egress-controls/egress-ip.md)
- [GitHub Issue #6227](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6227)
- [NPEP-187: More protocols support](https://network-policy-api.sigs.k8s.io/npeps/npep-187-ports-and-protocols/)
- [OKEP-6019: VRF-Lite with Shared Gateway Mode using Uplinks](okep-6019-vrf-lite-shared-gateway-external-bridges.md)
- [kubernetes#134224: CEL `isCIDR` accepts unmasked values](https://github.com/kubernetes/kubernetes/issues/134224)
