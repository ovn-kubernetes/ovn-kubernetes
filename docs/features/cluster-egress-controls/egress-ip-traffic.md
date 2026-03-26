# EgressIPTraffic

## Introduction

The EgressIPTraffic feature extends EgressIP to support **per-destination egress IP routing**. It introduces a new `EgressIPTraffic` custom resource that defines destination network CIDRs, and a `trafficSelector` field on the `EgressIP` spec that selects which `EgressIPTraffic` resources apply. This allows a cluster administrator to assign different egress IP addresses for traffic to different destination networks from the same pod — for example, using one egress IP for management traffic and another for data plane traffic, each routed through a different secondary host interface.

Always check the dependencies on the [Requirements page](../requirements.md)

## Motivation

In telecommunications and carrier environments, workloads often need to communicate with multiple external networks (e.g., OAM, signaling, user plane) through different egress interfaces, each with its own source IP address. The existing EgressIP feature routes **all** egress traffic from a pod through a single egress IP. EgressIPTraffic adds the granularity to route traffic based on destination, enabling multi-network egress topologies without requiring multiple pods or network attachments.

### User-Stories/Use-Cases

**Story 1: Per-destination egress IP for multi-network workloads**

As a cluster administrator, I want pods in a telco namespace to use different source IP addresses depending on which backend network they are reaching, so that each network sees traffic from the correct interface and IP range.

For example: a pod sends management traffic to `192.168.250.0/24` via egress IP `192.168.150.101` (on interface `eth-mgmt`), and signaling traffic to `192.168.251.0/24` via egress IP `192.168.200.101` (on interface `eth-signaling`).

**Story 2: Default egress IP with destination-specific overrides**

As a cluster administrator, I want a "default" EgressIP that handles general internet traffic, combined with destination-specific EgressIPs for particular backend networks, so that pods have predictable source IPs for all egress traffic without requiring per-destination configuration for every possible destination.

## How to enable this feature on an OVN-Kubernetes cluster?

EgressIPTraffic requires its own feature gate: `--enable-egress-iptraffic=true`. This implicitly enables the EgressIP feature (`--enable-egress-ip`). The `EgressIPTraffic` CRD must be installed on the cluster (included in the standard OVN-Kubernetes deployment manifests).

The feature is supported in both **local** and **shared** OVN gateway modes.

## Workflow Description

### Basic setup: per-destination EgressIP

1. **Create `EgressIPTraffic` resources** defining the destination network CIDRs and labeled for selection:

```yaml
apiVersion: k8s.ovn.org/v1
kind: EgressIPTraffic
metadata:
  name: mgmt-traffic
  labels:
    traffic-group: mgmt
spec:
  destinationNetworks:
  - "192.168.250.0/24"
---
apiVersion: k8s.ovn.org/v1
kind: EgressIPTraffic
metadata:
  name: signaling-traffic
  labels:
    traffic-group: signaling
spec:
  destinationNetworks:
  - "192.168.251.0/24"
```

2. **Create `EgressIP` resources** with `trafficSelector` matching the `EgressIPTraffic` labels:

```yaml
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
---
apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
  name: eip-signaling
spec:
  egressIPs:
  - 192.168.200.101
  namespaceSelector:
    matchLabels:
      env: production
  podSelector:
    matchLabels:
      app: telco-workload
  trafficSelector:
    matchLabels:
      traffic-group: signaling
```

3. **Label egress nodes** and ensure the secondary host interfaces exist:

```bash
kubectl label node worker-1 k8s.ovn.org/egress-assignable=""
```

The egress node must have interfaces on the same subnets as the egress IPs (e.g., `192.168.150.0/24` and `192.168.200.0/24`).

4. **Result**: Traffic from matching pods to `192.168.250.0/24` is SNAT'd to `192.168.150.101` and exits via the management interface. Traffic to `192.168.251.0/24` is SNAT'd to `192.168.200.101` via the signaling interface. All other traffic follows normal OVN routing.

### Advanced setup: default EgressIP with destination-specific overrides

Add a non-`trafficSelector` EgressIP alongside the destination-specific ones:

```yaml
apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
  name: eip-default
spec:
  egressIPs:
  - 172.18.0.100
  namespaceSelector:
    matchLabels:
      env: production
  podSelector:
    matchLabels:
      app: telco-workload
```

This acts as a catch-all: traffic not matching any `trafficSelector` destination is routed through `eip-default` at a lower priority. TrafficSelector EgressIPs take precedence.

## Implementation Details

### User facing API Changes

#### EgressIPTraffic CRD

A new cluster-scoped custom resource:

```go
type EgressIPTraffic struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec              EgressIPTrafficSpec `json:"spec,omitempty"`
}

type EgressIPTrafficSpec struct {
    DestinationNetworks []CIDR `json:"destinationNetworks"`
}

type CIDR string
```

- `destinationNetworks`: List of destination CIDRs that this traffic group covers.
- Labels on the `EgressIPTraffic` object are used for selection by `EgressIP.spec.trafficSelector`.

#### EgressIP TrafficSelector field

A new optional field on `EgressIP.spec`:

```go
type EgressIPSpec struct {
    EgressIPs         []string             `json:"egressIPs"`
    NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`
    PodSelector       metav1.LabelSelector `json:"podSelector"`
    TrafficSelector   metav1.LabelSelector `json:"trafficSelector,omitempty"` // NEW
}
```

When `trafficSelector` is set, the EgressIP only handles traffic to the destination networks defined in matching `EgressIPTraffic` resources. When not set, the EgressIP handles all traffic (existing behavior).

### OVN-Kubernetes Implementation Details

The feature is implemented across two controllers:

1. **OVN central controller** (`pkg/ovn/egressip.go`): Manages OVN logical router policies (LRPs) and address sets in the NB database.
2. **Node controller** (`pkg/node/controllers/egressip/egressip.go`): Manages Linux IP rules, routing tables, and iptables SNAT rules on the egress node.

#### Traffic flow diagram

```text
Pod (10.244.1.3) sends packet to 192.168.250.1
    │
    ▼
OVN Cluster Router
    │ LRP match: ip4.src == 10.244.1.3 && ip4.dst == $dest-nets-eip-mgmt
    │ Priority: 100, Action: reroute → 10.244.2.2 (egress node mgmt port)
    ▼
Egress Node (ovn-k8s-mp0)
    │ IP rule: from 10.244.1.3 to 192.168.250.0/24 lookup 1026
    │ Route table 1026: 192.168.250.0/24 via 192.168.150.1 dev eth-mgmt
    │ iptables: -s 10.244.1.3/32 -o eth-mgmt -j SNAT --to-source 192.168.150.101
    ▼
External network (src: 192.168.150.101 → dst: 192.168.250.1)
```

For non-matching traffic (e.g., to 1.1.1.1):
- The destination-filtered LRP does **not** match
- Traffic follows normal OVN routing (default SNAT to node IP)
- Or, if a non-`trafficSelector` default EgressIP exists, its LRP at priority 99 catches the traffic

#### OVN Constructs created in the databases

**Destination networks address set** — one per EgressIP with `trafficSelector`:

```shell
$ ovn-nbctl find address_set name=a4776992171281246630
addresses : ["192.168.250.0/24"]
external_ids : {k8s.ovn.org/name=egressip-dest-nets-eip-mgmt, ...}
```

**Destination-filtered reroute LRP** — priority 100 with destination match:

```shell
$ ovn-nbctl lr-policy-list ovn_cluster_router
100 ip4.src == 10.244.1.3 && ip4.dst == $a4776992171281246630    reroute    10.244.2.2
```

Compare with a standard (non-`trafficSelector`) EgressIP reroute which has no destination filter and uses priority 99:

```shell
 99 ip4.src == 10.244.1.3                                        reroute    100.64.0.3
```

**Non-TrafficSelector default EgressIP** — always uses priority 99:

```shell
 99 ip4.src == 10.244.1.3                                        reroute    100.64.0.3
```

**Destination-filtered NAT** — when the EgressIP is on the OVN network (breth0) and has `trafficSelector`, the SNAT rule on the gateway router includes a destination match to avoid conflicts with other EgressIPs on the same node:

```shell
$ ovn-nbctl lr-nat-list GR_ovn-worker2
snat    172.18.0.200    10.244.1.3    match="ip4.dst == $a4776992171281246630"   (trafficSelector EgressIP)
snat    172.18.0.100    10.244.1.3                                               (default EgressIP, no match)
```

Without the match, two SNAT rules for the same pod IP on the same GW router would have undefined ordering. The destination match ensures the correct EgressIP is applied based on which LRP rerouted the traffic.

#### Node-side constructs

**Per-destination IP rules** — each destination CIDR gets its own rule at priority 6000:

```shell
$ ip rule
6000: from 10.244.1.3 to 192.168.250.0/24 lookup 1026
6000: from 10.244.1.3 to 192.168.251.0/24 lookup 1028
```

Non-`trafficSelector` catch-all rules use priority 6001 to ensure destination-specific rules are evaluated first:

```shell
6001: from 10.244.1.3 lookup 1028
```

**Destination-specific routing table** — no default route, only destination CIDRs:

```shell
$ ip route show table 1026
192.168.250.0/24 via 192.168.150.1 dev eth-mgmt
192.168.150.0/24 dev eth-mgmt proto kernel scope link
```

Compare with a standard EgressIP routing table which includes a default route:

```shell
$ ip route show table 1026
default via 192.168.150.1 dev eth-mgmt
192.168.150.0/24 dev eth-mgmt proto kernel scope link
```

**iptables SNAT** — one rule per EgressIP, same as standard EgressIP:

```shell
$ iptables -t nat -L OVN-KUBE-EGRESS-IP-MULTI-NIC -n
SNAT  -s 10.244.1.3/32 -o eth-mgmt  --to-source 192.168.150.101
SNAT  -s 10.244.1.3/32 -o eth-sig   --to-source 192.168.200.101
```

### EgressIP coexistence

Multiple EgressIPs can serve the same pod when at least one has `trafficSelector` set:

| EgressIP | TrafficSelector | LRP Priority | IP Rule Priority | Role |
|----------|----------------|-------------|-----------------|------|
| eip-mgmt | yes | 100 | 6000 (per-dst) | Handles traffic to 192.168.250.0/24 |
| eip-signaling | yes | 100 | 6000 (per-dst) | Handles traffic to 192.168.251.0/24 |
| eip-default | no | 99 | 6001 (catch-all) | Handles all other traffic |

The higher-priority destination-specific rules are always evaluated before the catch-all. Without a default EgressIP, non-matching traffic follows normal OVN routing with the node's default SNAT.

## Troubleshooting

### Verify EgressIPTraffic resources

```bash
kubectl get egressiptraffic -o yaml
```

### Verify EgressIP status and assignment

```bash
kubectl get egressip -o yaml
```

Check that `status.items` shows the EgressIP assigned to a node.

### Verify OVN LRP match includes destination filtering

```bash
ovn-nbctl lr-policy-list ovn_cluster_router | grep "ip4.dst"
```

You should see LRPs at priority 100 with `ip4.dst == $<address-set-hash>`.

### Verify address set contents

```bash
ovn-nbctl find address_set | grep -A2 "egressip-dest-nets"
```

The `addresses` field should contain the destination CIDRs from the matching `EgressIPTraffic` resources. If it's empty, no `EgressIPTraffic` resources match the `trafficSelector` — a warning will be logged:

```text
EgressIP <name> has a TrafficSelector but no matching EgressIPTraffic destination networks were found
```

### Verify node-side IP rules and routes

On the egress node:

```bash
ip rule list                    # Look for per-destination rules at priority 6000
ip route show table <table-id>  # Should show destination CIDRs, not a default route
iptables -t nat -L OVN-KUBE-EGRESS-IP-MULTI-NIC -n  # SNAT rules
```

### Common issues

- **Traffic not rerouted**: Check that the address set is non-empty and the LRP match is correct.
- **SNAT to wrong IP**: Check iptables rule ordering — SNAT rules should appear before any MASQUERADE rules.
- **Stale LRPs after update**: The stale sync runs periodically. Force reconciliation by deleting and recreating the EgressIP.

## Best Practices

- Use distinct labels on `EgressIPTraffic` resources for clear selection by `trafficSelector`.
- When using multiple EgressIPs with `trafficSelector` for the same pod, ensure destination CIDRs don't overlap.
- Add a non-`trafficSelector` default EgressIP if pods need predictable egress IPs for all destinations, not just the ones covered by `EgressIPTraffic`.
- If `trafficSelector` EgressIPs are sufficient (no default needed), non-matching traffic will use normal OVN routing with the node's default SNAT — verify this is acceptable.

## Design Notes

- When the last `EgressIPTraffic` matching a `trafficSelector` is deleted, the destination networks address set becomes empty and the EgressIP stops routing traffic. This is by design: no matching destination networks means no traffic should use this EgressIP. A warning is logged and a Kubernetes event is emitted on the EgressIP object to help operators notice potential misconfigurations.
- An IPv6 EgressIP with a `trafficSelector` matching only IPv4 destination networks (or vice versa) will not route any traffic for the mismatched address family. A destination networks address set is maintained for each EgressIP with `trafficSelector`; if no destination networks match the EgressIP's address family, no traffic is rerouted. This is by design: destination networks must match the EgressIP's address family.

## Future Items

- Watch for BGP route changes on egress interfaces and trigger re-reconciliation of affected EgressIPs.
- Support `EgressIPTraffic` with user-defined networks (UDNs).

## Known Limitations

- EgressIPTraffic is only supported for the cluster default network. User-defined networks (UDNs) are not supported.
- Per-destination routing tables on egress nodes are snapshots of the link's routes at processing time. Dynamic route changes (e.g., via BGP) are not automatically reflected until the next sync or EgressIP reconciliation.
- Overlapping destination CIDRs across different EgressIPs targeting the same pod are not detected. If two EgressIPs with different `trafficSelector` match overlapping CIDRs, the OVN LRP behavior at the same priority is undefined.

## References

- [EgressIP documentation](egress-ip.md)
- [EgressIP CRD types](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/go-controller/pkg/crd/egressip/v1/types.go)
- [EgressIPTraffic CRD types](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/go-controller/pkg/crd/egressiptraffic/v1/types.go)
