# OKEP-6227: EgressTrafficClass — Selective EgressIP

* Issue: [#6227](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6227)

## Problem Statement

The existing EgressIP feature applies to all egress traffic from a matched pod. An EgressIP object may list several addresses (ECMP among assigned next hops), but there is no way to choose a different EgressIP — and thus a different source IP or egress interface — based on destination CIDR or L4 protocol/port.

## Goals

* Allow a cluster administrator to define destinations (destination CIDRs with optional L4 protocol/port filtering) via a new `EgressTrafficClass` CRD.
* Allow an `EgressIP` to select `EgressTrafficClass` resources via a `trafficSelector` label selector, routing only matching traffic through that egress IP.
* Support L4-level filtering: route traffic based on IP protocol (TCP/UDP/SCTP) and optional destination and/or source port or port range, using an NPEP-187-style `protocols` list so additional protocols can be added later without a breaking API change.
* Support multiple `EgressIP` objects with `trafficSelector` serving the same pod, each handling different destination networks or protocols.
* Support coexistence of `trafficSelector` EgressIPs on secondary host interfaces with a non-`trafficSelector` default EgressIP on the OVN network (Story 2).
* Work with EgressIPs assigned to both OVN primary network interfaces and secondary host interfaces.
* Support both local and shared OVN gateway modes.
* Support EgressIPs on user-defined networks (UDNs) for OVN-network EgressIPs (SNAT on the gateway router).

## Non-Goals

* ICMP / non-port protocols in this OKEP: the `protocols` list is shaped so ICMP (or GRE, TCP flags, …) can be added later; this OKEP only defines TCP, UDP, and SCTP.
* Cross-feature consumption of `EgressTrafficClass` by NetworkQoS, ClusterNetworkPolicy, or other APIs in this OKEP. The type is a named **destination match** (CIDR + optional L4), not a policy: it has no allow/deny/drop, no rule order, and no ingress/`from` match. A later OKEP may select it from other **egress** consumers (for example a ClusterNetworkPolicy Allow rule or NetworkQoS dest matching); action and direction stay on those APIs. This OKEP's only consumer is EgressIP. A generic `TrafficClass` or ingress reuse would need `sources` (or `ingress`/`egress` subtrees) and is out of scope.
* Catch-all EgressIP on a secondary host interface coexisting with a `trafficSelector` EgressIP on the OVN network for the same pod. Both LRPs reroute to the same transit switch; the packet arrives on the assigned node's management port; the catch-all `6001: from <podIP>` IP rule then steals dests that should have reached gateway-router SNAT for the OVN EgressIP. This OKEP does not add dest-exclusion host IP rules to undo that.

## Future Goals

* Secondary host interface EgressIPs on UDNs, including with `trafficSelector`, once base secondary EgressIP on UDN exists. Reuse this OKEP's host path (per-destination IP rules and dest-scoped iptables SNAT) inside the UDN routing domain (VRF), rather than a second match model. A likely vehicle is Uplink-backed CUDN gateways ([OKEP-6019](okep-6019-vrf-lite-shared-gateway-external-bridges.md)), which already lists secondary EgressIP through `Uplink` as a future consumer.

## Introduction

In telecommunications and carrier deployments, a single pod often needs to reach multiple external networks (for example OAM, signaling, and user plane). Each network must see traffic leave an **egress node** interface with an **EgressIP** source address (not the pod IP, and not the node IP). Local-gateway masquerade to node IPs is not enough: those addresses are per-node, move when the pod or assignment moves, and are not the HA EgressIP that external ACLs allowlist. Secondary-host EgressIP can already steer via host routes, but if a pod matches more than one EgressIP the winner is undefined — operators cannot pin which EgressIP (hence which egress node interface and source IP) applies by destination CIDR or L4. Different services on the same network (e.g. SIP on UDP 5060 vs RTP on UDP 16384–32767) may also need different EgressIPs.

EgressTrafficClass solves this by introducing destination-based egress routing with optional L4 filtering. A new `EgressTrafficClass` CRD defines destinations — destination CIDRs with optional protocol/port filters — and a new `trafficSelector` field on `EgressIP` selects which `EgressTrafficClass` resources apply. A `trafficSelector` EgressIP only handles matching destinations. Non-matching traffic is not handled by that EgressIP: it uses a coexisting non-`trafficSelector` default EgressIP if one is assigned to the pod (Story 2), otherwise normal OVN egress (typically the node IP). Omitting `trafficSelector` is unchanged EgressIP behavior: all of the pod's egress uses that EgressIP.

### Behavioral change: multi-EgressIP pod support

Prior to this OKEP, having multiple `EgressIP` objects selecting the same pod was considered **undefined behavior**. The controller would arbitrarily pick one EgressIP as the "primary" and put the rest on standby. There was no guarantee about which EgressIP would serve the pod, and failover between them was non-deterministic.

This OKEP changes that behavior to be **deterministic** when `trafficSelector` is used:

- Multiple `EgressIP` objects with different `trafficSelector` values can coexist on the same pod, each handling traffic to different destination networks.
- A non-`trafficSelector` default EgressIP on the OVN network can coexist with `trafficSelector` EgressIPs on secondary host interfaces, acting as a catch-all for traffic not matching any destination. The reverse mix (catch-all on a secondary host NIC, `trafficSelector` on OVN) is a Non-Goal.
- Priority-based LRP evaluation (100 for `trafficSelector`, 99 for every non-`trafficSelector` EgressIP) ensures dest/L4 matches win over catch-all. On upgrade, existing catch-all EIP LRPs are rewritten from 100 to 99; `trafficSelector` LRPs use 100. Relative order vs EgressService (101), no-reroute (102), and EIP QoS (103) is unchanged.
- Without `trafficSelector`, multiple EgressIPs selecting the same pod remain undefined. Standalone EgressIP still SNAT/reroutes all pod egress; only the LRP priority changes (100 → 99).

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

As a cluster administrator, I want a default EgressIP on the OVN network that handles general internet traffic, combined with destination-specific EgressIPs on secondary host interfaces for particular backend networks, so that pods have predictable source IPs for all egress traffic without requiring per-destination configuration for every possible destination.

**Story 3: Per-protocol/port egress routing on the same network**

As a cluster administrator, I want SIP signaling traffic (UDP destination 5060) to the OAM network to use one egress IP on a dedicated interface, while SSH management traffic (TCP destination 22) to the same network uses a different egress IP, so each service type has the correct source IP and interface.

**Story 4: Local RTP port range as source-port match**

As a cluster administrator, I want media traffic from a pod that binds a local RTP port range (UDP source 16384–32767) to use a dedicated egress IP, even when remote RTP destination ports are unpredictable, so media and other traffic to the same destination CIDR can take different EgressIPs.

## Proposed Solution

### API Details

#### EgressTrafficClass CRD

A new cluster-scoped custom resource that defines destination matches (CIDR + optional L4). The name is **egress-scoped**, not EgressIP-specific: other egress features could select the same named dest sets later. It is not a policy object (no action, no order, no direction wrapper) and not an ingress classifier (`spec.destinations` is a `to:` list). This OKEP only defines the EgressIP consumer.

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
    // Destinations defines destination networks with optional L4 protocol/port filtering.
    // +kubebuilder:validation:MaxItems=25
    // +kubebuilder:validation:MinItems=1
    Destinations []Destination `json:"destinations"`
}

// Destination matches traffic to a destination CIDR, optionally narrowed by
// an NPEP-187-style protocols list (nested protocol keys, destinationPort
// and/or sourcePort).
type Destination struct {
    // CIDR is the destination network in IPv4 or IPv6 CIDR notation.
    // isCIDR() also accepts host-and-mask values (k8s.io/kubernetes#134224);
    // require the canonical network address.
    // +kubebuilder:validation:XValidation:rule="isCIDR(self) && cidr(self) == cidr(self).masked()",message="CIDR must be a valid network address"
    CIDR string `json:"cidr"`

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
type ProtocolMatch struct {
    TCP  *PortProtocol `json:"tcp,omitempty"`
    UDP  *PortProtocol `json:"udp,omitempty"`
    SCTP *PortProtocol `json:"sctp,omitempty"`
}

type PortProtocol struct {
    // DestinationPort matches the remote port. Optional.
    // +optional
    DestinationPort *Port `json:"destinationPort,omitempty"`

    // SourcePort matches the local (pod) port. Optional. Used when the
    // workload binds a well-known local port or port range (e.g. RTP).
    // +optional
    SourcePort *Port `json:"sourcePort,omitempty"`
}

// Port is exactly one of Number or Range.
type Port struct {
    // +optional
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=65535
    Number *int32 `json:"number,omitempty"`

    // +optional
    Range *PortRange `json:"range,omitempty"`
}

type PortRange struct {
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=65535
    Start int32 `json:"start"`
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=65535
    End int32 `json:"end"` // Must be >= Start
}
```

Key points:
- Cluster-scoped (not namespaced).
- Labels on the object are used for selection by `EgressIP.spec.trafficSelector`.
- CIDRs are validated at admission time via CEL (`isCIDR(self) && cidr(self) == cidr(self).masked()`), so host-and-mask values like `192.168.1.1/24` are rejected ([kubernetes#134224](https://github.com/kubernetes/kubernetes/issues/134224)).
- Maximum 25 destinations per resource; at least one required.
- `protocols` follows [NPEP-187](https://network-policy-api.sigs.k8s.io/npeps/npep-187-ports-and-protocols/) option 3: nested `tcp` / `udp` / `sctp` keys and optional `destinationPort` / `sourcePort` (`number` or `range`). OR across list entries; AND within an entry when both ports are set. Exactly one protocol key per entry.
- A destination with no `protocols` matches all traffic to the CIDR.
- Protocol-only match (e.g. all TCP) omits both port fields, or uses `destinationPort.range` covering 1–65535.
- When both `destinationPort` and `sourcePort` are set, a packet must match both.
- ICMP is reserved for a later OKEP.
- **Overlapping destinations** are not rejected at `EgressTrafficClass` admission (label selectors do not say which pods will share two CRs):

| Where | Allowed? |
|--------|----------|
| Two dests in one `EgressTrafficClass` | Yes (redundant). |
| Two `EgressTrafficClass` resources selected by the **same** EgressIP | Yes (same SNAT). |
| Two `EgressTrafficClass` resources / EgressIPs, **different** pods | Yes. |
| Two `trafficSelector` EgressIPs, **same** pod, overlapping traffic | No correct routing. Detected at assignment as `TrafficConflict` (see below). |

#### EgressIP TrafficSelector field

A new optional pointer field on `EgressIP.spec`:

```go
type EgressIPSpec struct {
    EgressIPs         []string              `json:"egressIPs"`
    NamespaceSelector metav1.LabelSelector  `json:"namespaceSelector"`
    PodSelector       metav1.LabelSelector  `json:"podSelector"`
    // +kubebuilder:validation:XValidation:rule="(has(self.matchLabels) && size(self.matchLabels) > 0) || (has(self.matchExpressions) && size(self.matchExpressions) > 0)",message="trafficSelector must specify matchLabels or matchExpressions"
    TrafficSelector   *metav1.LabelSelector `json:"trafficSelector,omitempty"` // NEW
}
```

The field is a pointer to distinguish:
- **nil** (field absent): no traffic selector — all of the pod's egress uses this EgressIP (existing behavior).
- **set**: must include `matchLabels` and/or `matchExpressions` with at least one entry (CEL). Matches only `EgressTrafficClass` resources whose labels satisfy the selector. `trafficSelector: {}` is rejected; matching every `EgressTrafficClass` in the cluster is not supported.

When `trafficSelector` is set (non-nil), the EgressIP only handles traffic defined in matching `EgressTrafficClass` resources.

**When no `EgressTrafficClass` matches the `trafficSelector`**: The reroute LRP matches no traffic, and the EgressIP effectively stops routing traffic. A warning is logged and a Kubernetes event is emitted on the EgressIP object:

```text
Warning  NoTrafficClass  TrafficSelector is set but no matching EgressTrafficClass
                          resources were found; traffic will not be routed via
                          this EgressIP until matching EgressTrafficClass resources
                          are created
```

This is by design: no matching egress traffic classes means no traffic should use this EgressIP. Operators should create the required `EgressTrafficClass` resources or verify the `trafficSelector` labels.

#### EgressIP Status Conditions

Two new conditions are added to the EgressIP status to improve observability:

```go
type EgressIPStatus struct {
    Items      []EgressIPStatusItem `json:"items"`
    Conditions []metav1.Condition   `json:"conditions,omitempty"` // NEW
}
```

**`TrafficSelectorResolved`**: Set to `True` when the `trafficSelector` successfully resolves to at least one `EgressTrafficClass` with destinations. Set to `False` when `trafficSelector` is set but no matching `EgressTrafficClass` resources are found.

```yaml
conditions:
- type: TrafficSelectorResolved
  status: "True"
  reason: DestinationsFound
  message: "Resolved 2 destinations from 1 EgressTrafficClass resource(s)"
```

```yaml
conditions:
- type: TrafficSelectorResolved
  status: "False"
  reason: NoMatchingEgressTrafficClass
  message: "No EgressTrafficClass resources match trafficSelector"
```

**`TrafficConflict`**: Set to `True` when two or more `trafficSelector` EgressIPs targeting the same pod have **overlapping traffic** — overlapping destination CIDRs **and** overlapping L4 matches. Disjoint L4 on the same CIDR is not a conflict (e.g. UDP dest 5060 vs TCP dest 22, or UDP dest 5060 vs UDP source 16384–32767 with no dest overlap). Overlap inside one `EgressTrafficClass`, or across `EgressTrafficClass` resources selected by the same EgressIP, is not a `TrafficConflict`.

Overlap rules:
- CIDR-only destination (no `protocols`) overlaps any other destination whose CIDR intersects that CIDR, including L4-narrowed ones, because the CIDR-only match accepts all protocols/ports.
- Two L4 destinations conflict only if their CIDRs intersect **and** at least one protocol entry pair intersects: same protocol, overlapping destination ports (unset dest port matches all dest ports), and overlapping source ports (unset source port matches all source ports).
- The controller performs this analysis during pod assignment and surfaces the conflicting peer EgressIP and matchers in the condition message.

```yaml
conditions:
- type: TrafficConflict
  status: "True"
  reason: OverlappingTraffic
  message: "Destination 192.168.250.0/24 UDP/5060 overlaps with EgressIP eip-signaling for pod selector app=telco-workload"
```

#### Example resources

```yaml
# CIDR-only destination: all traffic to OAM network
apiVersion: k8s.ovn.org/v1
kind: EgressTrafficClass
metadata:
  name: mgmt-traffic
  labels:
    traffic-group: mgmt
spec:
  destinations:
  - cidr: "192.168.250.0/24"
---
# L4-filtered destinations: SIP signaling and RTP media on the same network
apiVersion: k8s.ovn.org/v1
kind: EgressTrafficClass
metadata:
  name: signaling-traffic
  labels:
    traffic-group: signaling
spec:
  destinations:
  - cidr: "192.168.250.0/24"
    protocols:
    - udp:
        destinationPort:
          number: 5060
    - udp:
        destinationPort:
          range:
            start: 16384
            end: 16399
  - cidr: "192.168.250.0/24"
    protocols:
    - tcp:
        destinationPort:
          number: 22
---
# Source-port match: local RTP bind range (remote dest ports unknown)
apiVersion: k8s.ovn.org/v1
kind: EgressTrafficClass
metadata:
  name: rtp-media
  labels:
    traffic-group: media
spec:
  destinations:
  - cidr: "192.168.250.0/24"
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

The feature is gated by `--enable-egress-traffic-class` (config: `enable-egress-traffic-class`). When enabled, it implicitly enables `--enable-egress-ip` since EgressTrafficClass extends EgressIP. When disabled (default), the EgressTrafficClass informer is not registered, `trafficSelector` on EgressIP is ignored, and no destination-filtered LRPs or NATs are created.

### Implementation Details

The feature is implemented in OVN-Kubernetes across:

1. **ovnkube-controller** (`pkg/ovn/egressip.go`): LRPs, NAT, and address sets in the OVN NB database.
2. **ovnkube-node** (`pkg/node/controllers/egressip/egressip.go`): Linux IP rules, routing tables, and iptables SNAT on the egress node.

EgressIP node assignment in ovnkube-cluster-manager is unchanged.

#### ovnkube-controller changes

**Destination parsing**: Destinations from all matching `EgressTrafficClass` resources are parsed into structured matches (CIDR, protocol, destination and/or source port/range). These are split into two groups:
- **CIDR-only destinations** (no `protocols`): Added to an OVN address set.
- **L4-filtered destinations**: Converted to inline OVN match conditions (one clause per `protocols` entry).

**Destination networks address set**: For each EgressIP with `trafficSelector`, an OVN address set is created containing the CIDRs from CIDR-only destinations. L4-filtered destinations cannot be represented in address sets and are handled via inline match conditions. The address set is updated when `EgressTrafficClass` resources are created, updated, or deleted.

**Destination-filtered reroute LRP**: The reroute LRP match combines address set matching with inline L4 conditions:

```text
# CIDR-only (priority 100):
ip4.src == 10.244.1.3 && ip4.dst == $<dest-nets-addr-set>    reroute    10.244.2.2

# L4-filtered (priority 100):
ip4.src == 10.244.1.3 && (ip4.dst == 10.0.0.0/8 && tcp && tcp.dst == 5060)    reroute    10.244.2.2

# L4-filtered with source port (priority 100):
ip4.src == 10.244.1.3 && (ip4.dst == 10.0.0.0/8 && udp && udp.src == 16384..32767)    reroute    10.244.2.2

# Mixed CIDR-only + L4 (priority 100):
ip4.src == 10.244.1.3 && (ip4.dst == $<addrset> || (ip4.dst == 10.0.0.0/8 && tcp && tcp.dst == 5060))    reroute    10.244.2.2

# Without trafficSelector, coexisting as default (priority 99):
ip4.src == 10.244.1.3    reroute    100.64.0.3
```

Port ranges use OVN range syntax: `tcp.dst == 5060..5062`, `udp.src == 16384..32767`.

**Destination-filtered NAT**: When the EgressIP is on the OVN network and has `trafficSelector`, the SNAT rule on the gateway router must match the same traffic as the reroute LRP — destination CIDR for CIDR-only destinations, plus protocol/port for L4-filtered destinations — so multiple EgressIPs on the same pod select the correct SNAT IP:

```text
# CIDR-only trafficSelector:
snat    172.18.0.200    10.244.1.3    match="ip4.dst == $<dest-nets-addr-set>"

# L4-filtered trafficSelector (example):
snat    172.18.0.201    10.244.1.3    match="ip4.dst == 192.168.250.0/24 && udp && udp.dst == 5060"
snat    172.18.0.202    10.244.1.3    match="ip4.dst == 192.168.250.0/24 && udp && udp.src == 16384..32767"

# Non-trafficSelector default (no match):
snat    172.18.0.100    10.244.1.3
```

**EgressIP coexistence**: Multiple EgressIPs can serve the same pod when at least one has `trafficSelector`. `trafficSelector` LRPs use priority 100; every non-`trafficSelector` EgressIP uses 99 (standalone or as a Story 2 default), so dest/L4 matches are evaluated first. Existing catch-all LRPs at 100 are rewritten to 99 on upgrade (see Backwards Compatibility).

| EgressIP | TrafficSelector | Interface | LRP Priority | IP Rule Priority | Role |
|----------|----------------|-----------|-------------|-----------------|------|
| eip-mgmt | yes | secondary host | 100 | 6000 (per-dst) | Handles traffic to 192.168.250.0/24 |
| eip-signaling | yes | secondary host | 100 | 6000 (per-dst+L4) | Handles UDP 5060 to 192.168.250.0/24 |
| eip-default | no | OVN network | 99 | n/a (GR SNAT) | Handles all other traffic |

**Reconciliation**: `EgressTrafficClass` changes trigger re-reconciliation of all EgressIPs with matching `trafficSelector`. The update handler only processes the old object when labels change; when only `destinations` change, a single reconcile with the new object suffices. TrafficSelector changes on an existing EgressIP (CASE 3.0) trigger a full teardown and rebuild of assignments.

#### ovnkube-node changes

**Per-destination IP rules with L4 filtering**: Instead of a single catch-all IP rule per pod, one IP rule per destination (and per `protocols` entry when L4 is set) is created at priority 6000. When protocol/port fields are set, the IP rule includes `IPProto`, `Dport`, and `Sport` fields:

```text
# CIDR-only destination:
6000: from 10.244.1.3 to 192.168.250.0/24 lookup 1026

# L4-filtered destinations:
6000: from 10.244.1.3 to 192.168.250.0/24 ipproto tcp dport 5060 lookup 1026
6000: from 10.244.1.3 to 192.168.250.0/24 ipproto udp dport 5060 lookup 1026
6000: from 10.244.1.3 to 192.168.250.0/24 ipproto udp dport 16384-16399 lookup 1026
6000: from 10.244.1.3 to 192.168.250.0/24 ipproto udp sport 16384-32767 lookup 1026
```

Non-`trafficSelector` catch-all rules on a **secondary host** EgressIP use priority 6001 so destination-specific rules are evaluated first:

```text
6001: from 10.244.1.3 lookup 1028
```

Do not combine this catch-all with a `trafficSelector` EgressIP on the OVN network for the same pod (Non-Goal). A default EgressIP on the OVN network does not install a host catch-all IP rule.

**Destination-specific routing table**: The default route is replaced with destination-specific routes, preserving the gateway from the original default route. If a specific route for the destination CIDR already exists on the interface (e.g., configured by the network administrator), it is preserved with its gateway rather than replaced. Non-matching traffic falls through to the main routing table:

```text
# With trafficSelector:
192.168.250.0/24 via 192.168.150.1 dev eth-mgmt
192.168.150.0/24 dev eth-mgmt proto kernel scope link

# Without trafficSelector (standard):
default via 192.168.150.1 dev eth-mgmt
192.168.150.0/24 dev eth-mgmt proto kernel scope link
```

Routes remain L3-only — L4 filtering is at the IP rule level.

**IP rule comparison**: The `ruleKey()` function extends `netlink.Rule.String()` with `IPProto`, `Dport`, and `Sport` fields for use as set keys. Without this, rules differing only by protocol/port would appear identical to the rule manager and repair path, causing deduplication issues.

**Traffic-matched iptables SNAT**: For secondary host interface EgressIPs with `trafficSelector`, each iptables SNAT rule must be scoped to the same traffic as the corresponding IP rule / destination. Matching only on source IP and output interface is insufficient when two EgressIPs select the same pod and the same egress interface: each EgressIP has its own address, so two rules of the form `-s <podIP> -o <iface> -j SNAT --to-source <egressIP>` both match all pod traffic out that interface and the first rule wins for every destination.

```text
# Incorrect (ambiguous when multiple EIPs share pod + iface):
-s 10.244.1.3/32 -o eth-mgmt -j SNAT --to-source 192.168.150.101
-s 10.244.1.3/32 -o eth-mgmt -j SNAT --to-source 192.168.150.102

# Correct — CIDR-only destination:
-s 10.244.1.3/32 -d 192.168.250.0/24 -o eth-mgmt -j SNAT --to-source 192.168.150.101
-s 10.244.1.3/32 -d 192.168.251.0/24 -o eth-mgmt -j SNAT --to-source 192.168.150.102

# Correct — L4-filtered destination (protocol/port match the Destination protocols entry):
-s 10.244.1.3/32 -d 192.168.250.0/24 -p udp --dport 5060 -o eth-mgmt -j SNAT --to-source 192.168.150.101
-s 10.244.1.3/32 -d 192.168.250.0/24 -p udp --sport 16384:32767 -o eth-mgmt -j SNAT --to-source 192.168.150.103
```

IP rules still decide which routing table (and thus which interface) is used; destination/L4-conditioned SNAT decides which EgressIP address is applied once the packet egresses that interface. Non-`trafficSelector` EgressIPs keep the existing `-s <podIP> -o <iface>` SNAT form.

**EgressTrafficClass informer**: ovnkube-node watches `EgressTrafficClass` resources and re-queues affected EgressIPs when destinations change. The update handler only processes the old object when labels changed (optimization to avoid unnecessary re-reconciliation when only `destinations` change).

#### Differences between LGW and SGW modes

The feature works identically in both local and shared gateway modes. The destination-filtered LRP match, address sets, NAT rules, and node-side constructs are gateway-mode-agnostic.

### Testing Details

#### Unit Tests

**Destination parsing** (`pkg/util/egressip/destination_test.go`):
- `ParseDestinations`: CIDR-only, TCP/UDP/SCTP with `destinationPort.number`, `destinationPort.range`, `sourcePort.number`, `sourcePort.range`, combined dest+src, invalid CIDR, more than one protocol key, empty protocols, multiple destinations
- `UniqueCIDRs`: deduplication across destinations with different L4 filters
- `SplitByL4`: separation of CIDR-only vs L4-filtered destinations
- Port overlap helpers used by `TrafficConflict`
- `HasL4Filter` and `ProtocolName` helpers

**ovnkube-controller tests** (`pkg/ovn/egressip_test.go`):
- `hasTrafficSelector`: nil vs set (empty `{}` is rejected at admission)
- `egressStatuses.containsAny`: empty, matching, non-matching
- `buildL4MatchClauses`: single dest port, dest port range, source port, source port range, combined dest+src, multiple `protocols` entries, IP family filtering, mixed CIDR-only and L4
- `buildPortMatch`: `number` and `range` notation for dest and source
- LRP match parsing: standard match, destination-filtered match, too-short match, IPv6
- NAT match: IPv4/IPv6 with and without destination match
- EgressIP reroute priority: non-trafficSelector uses 99, trafficSelector uses 100
- `TrafficConflict`: CIDR-only vs CIDR-only, CIDR-only vs L4, disjoint dest ports (no conflict), disjoint dest vs source ports (no conflict), overlapping dest or source ports (conflict)

**ovnkube-node tests** (`pkg/node/controllers/egressip/egressip_test.go`):
- TrafficSelector EgressIP with single destination
- TrafficSelector EgressIP with multiple destinations (per-destination dest-scoped SNAT)
- Two TrafficSelector EgressIPs on the same secondary interface for the same pod: each SNAT rule includes distinct `-d` (and L4) matches and the correct `--to-source`
- Non-TrafficSelector EgressIP with lower priority catch-all rule and unscoped `-s/-o` SNAT
- `ruleKey`: IPProto/Dport/Sport inclusion, differentiation of same-CIDR rules by protocol/port
- `replaceDefaultRouteWithDestNetworks`: default route removal, gateway preservation, existing specific route preservation, IPv4/IPv6 filtering, empty destinations
- `hasRouteForDst`: matching CIDR, different CIDR, same IP different mask, empty routes
- `containsRoute`: matching, non-matching, empty
- `getRoutesForOtherEIPsOnLink`: co-located routes, empty when alone

#### E2E Tests

Twelve E2E test cases tagged `[secondary-host-eip] [traffic-selector]`:

1. **Matching and non-matching traffic**: Verifies matching destination traffic uses EgressIP, non-matching uses node IP, and EgressTrafficClass deletion breaks connectivity to the destination network.
2. **Multiple TrafficSelector EgressIPs**: Two secondary networks, two EgressTrafficClass CRs, two EgressIPs; verifies each destination uses the correct EgressIP.
3. **Same secondary interface, two EgressIPs**: Two EgressIPs on the same host NIC for the same pod with different destinations; verifies each destination is SNATed to the correct EgressIP (not the first-matching unscoped rule).
4. **Coexistence**: TrafficSelector EgressIP on secondary host network (priority 100) + non-TrafficSelector default EgressIP on OVN network (priority 99); verifies both work independently.
5. **EgressTrafficClass destinations update**: Changes destinations on a live EgressTrafficClass, verifies traffic transitions from non-matching to matching.
6. **EgressTrafficClass label change**: Changes labels so the EgressTrafficClass stops matching the trafficSelector, verifies traffic stops routing via EgressIP.
7. **TrafficSelector added to existing EgressIP**: Starts with standard EgressIP, adds trafficSelector, verifies transition from catch-all to destination-filtered routing.
8. **L4 filtering — matching dest port**: TCP traffic to matching destination port uses EgressIP.
9. **L4 filtering — non-matching dest port**: Traffic to same CIDR on non-matching destination port does NOT use EgressIP.
10. **L4 filtering — mixed destinations**: CIDR-only + L4-filtered destinations in same EgressTrafficClass; CIDR-only routes all traffic.
11. **L4 filtering — dest port range**: `destinationPort.range` covering the external container's port.
12. **L4 filtering — source port range**: UDP from a bound local port in `sourcePort.range` uses EgressIP; traffic from a different local port to the same CIDR does not.

All E2E tests pass on both local and shared gateway modes. Tests skip when the EgressTrafficClass feature gate is not enabled.

### Documentation Details

Feature documentation will be added at `docs/features/cluster-egress-controls/egress-traffic-class.md` covering:
- Workflow with YAML examples (basic, advanced, L4 filtering)
- Implementation details with traffic flow diagrams
- ovnkube-controller and ovnkube-node construct descriptions
- Troubleshooting guide
- Best practices, design notes, known limitations

## Risks, Known Limitations and Mitigations

- **Overlapping traffic**: If two EgressIPs with different `trafficSelector` match overlapping traffic for the same pod (intersecting CIDRs and intersecting L4, or a CIDR-only destination vs any intersecting CIDR), OVN LRP behavior at the same priority is undefined. Mitigation: the `TrafficConflict` status condition detects and surfaces overlapping traffic; operators should resolve by adjusting destinations or protocols. Disjoint L4 on the same CIDR is not treated as a conflict.
- **UDN secondary host interface**: Not in this OKEP (see Future Goals). Dest-scoped host IP rules and iptables SNAT are additive to today's unscoped secondary-host EgressIP programming and should not block a later UDN/VRF or Uplink-backed secondary EgressIP design.
- **Port range limits**: Port ranges in OVN match expressions use range syntax (`tcp.dst == 5060..5062`).
- **Unscoped secondary-host SNAT**: Matching iptables SNAT only on `-s <podIP> -o <iface>` is incorrect when multiple `trafficSelector` EgressIPs share a pod and secondary host interface. Mitigation: scope each SNAT rule to the destination’s CIDR and L4 fields (see **Traffic-matched iptables SNAT** above). Non-`trafficSelector` EgressIPs retain the existing unscoped form.
- **Catch-all on secondary host + `trafficSelector` on OVN**: Unsupported in this OKEP (see Non-Goals). The catch-all `6001: from <podIP>` IP rule on the assigned node intercepts packets that arrived via the transit switch / management port before gateway-router SNAT for the OVN EgressIP. The supported Story 2 mix is the reverse: `trafficSelector` on secondary host, default on OVN (no host catch-all IP rule).
- **L2 UDN local-gateway without transit router**: In that topology, EIP reroutes and UDN host-CIDR policies (`UDNHostCIDRPolicyPriority` 99) share the gateway router. After catch-all EIP LRPs move to 99, those two share a priority. Overlap is only EIP-pod traffic to the host CIDR. L2 with a transit router does not program that host-CIDR LRP. This OKEP does not change `UDNHostCIDRPolicyPriority`.

## OVN-Kubernetes Version Skew

This feature is proposed for introduction in a future release. The `EgressTrafficClass` CRD and `trafficSelector` field are additive. Existing EgressIP objects without `trafficSelector` keep the same SNAT/reroute behavior; their cluster-router (or L2-no-transit GR) reroute LRPs move from priority 100 to 99.

## Backwards Compatibility

- The `trafficSelector` field on `EgressIP` is optional (pointer, nil by default). Omitting it is still catch-all EgressIP for that object.
- The `EgressTrafficClass` CRD is new and has no impact on existing resources.
- **LRP priority 100 → 99 for non-`trafficSelector` EgressIP**: Today `EgressIPReroutePriority` is 100. This OKEP keeps 100 for `trafficSelector` LRPs and uses 99 for all other EgressIP reroute LRPs so dest/L4 matches win. On upgrade, ovnkube-controller rewrites existing EIP reroute LRPs from 100 to 99. Repair must treat both priorities as EIP during the transition (same idea as IP rule 6000/6001): find leftover 100, recreate at 99, delete stale 100. Rollback/mixed-version: old code only owns 100 and may recreate it; new code must not leave duplicate 100+99 reroutes for the same pod. Relative to EgressService (101), no-reroute (102), and EIP QoS (103), catch-all EIP is still lower, so those interactions do not change.
- Non-`trafficSelector` EgressIPs use IP rule priority 6001 (changed from 6000) on the node side. The `repairNode` sync handles the transition by scanning both priorities and cleaning up stale rules.

## Alternatives

1. **Per-pod secondary network attachments**: Instead of destination-based routing, each pod could attach to multiple networks directly. This requires application-level awareness and significantly complicates pod networking.
2. **EgressFirewall with SNAT**: Using EgressFirewall rules combined with custom SNAT configurations. This would require significant changes to the EgressFirewall feature and doesn't integrate with EgressIP's node assignment and HA capabilities.
3. **Destination CIDRs inline on EgressIP**: Instead of a separate `EgressTrafficClass` CRD, destinations could be specified directly on the `EgressIP` spec. This was rejected because it doesn't allow reuse of destination definitions across multiple EgressIPs and makes the EgressIP spec overly complex.

## References

- [EgressIP documentation](../features/cluster-egress-controls/egress-ip.md)
- [GitHub Issue #6227](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6227)
- [NPEP-187: More protocols support](https://network-policy-api.sigs.k8s.io/npeps/npep-187-ports-and-protocols/)
- [OKEP-6019: VRF-Lite with Shared Gateway Mode using Uplinks](okep-6019-vrf-lite-shared-gateway-external-bridges.md)
- [kubernetes#134224: CEL `isCIDR` accepts unmasked values](https://github.com/kubernetes/kubernetes/issues/134224)
