# OKEP-6800: EgressIP Node Selector

* Issue: [#6800](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6800)

## Problem Statement

Cluster administrators managing multi-tenant or compliance-sensitive
environments cannot control which specific pool of egress-assignable nodes
hosts a particular EgressIP. Today, all nodes labeled with
`k8s.ovn.org/egress-assignable` form a single, flat pool shared by every
EgressIP object, forcing operators into coarse workarounds (dedicated clusters,
complex label choreography, or manual IP subnet partitioning) to satisfy
topology, regulatory, or zone-based placement requirements.

## Goals

- Allow EgressIP objects to specify a node selector that restricts which
  egress-assignable nodes are eligible to host that specific EgressIP.
- Maintain full backwards compatibility: EgressIP objects without a node
  selector continue to use all egress-assignable nodes as today.
- Reuse existing Kubernetes label selector semantics
  (`metav1.LabelSelector`) for consistency with `EgressService.spec.nodeSelector`
  and broader Kubernetes ecosystem patterns.
- Support failover: if a node matching the selector becomes unavailable
  or unreachable, the EgressIP is reassigned to another node that matches
  the selector (not any arbitrary egress-assignable node).
- Ensure the feature works on both bare-metal and cloud environments
  (CloudPrivateIPConfig flow).

## Future Goals

- Allow per-IP node selectors within a single EgressIP object (e.g.,
  different IPs in the same EgressIP assigned to different node pools).
  Today this is achievable by splitting IPs into separate EgressIP CRDs
  with different nodeSelectors, but that sacrifices the anti-co-location
  guarantee (IPs within the same EgressIP object are never assigned to
  the same node). A per-IP selector would preserve that guarantee while
  allowing heterogeneous node pools — only worth pursuing if a concrete
  use case emerges.
- Topology-aware scheduling (prefer nodes in the same zone/rack as the
  majority of selected pods). This would reduce cross-zone tunnel hops
  and inter-AZ bandwidth costs by assigning EgressIPs to nodes
  topologically close to the pods using them. However, it conflicts with
  EgressIP stability (pod distribution shifts would cause IP hopping),
  requires re-evaluation on every pod scheduling event, and operators
  can already achieve this manually by using the nodeSelector with
  per-zone labels. Worth revisiting for large multi-AZ clusters where
  inter-zone bandwidth is metered.

## Non-Goals

- Replacing the `k8s.ovn.org/egress-assignable` node label. The label
  remains a prerequisite — the node selector is an additional filter
  applied on top of the assignable pool.
- Supporting node affinity with `requiredDuringScheduling` /
  `preferredDuringScheduling` semantics. This OKEP implements hard
  requirement semantics only (matching nodes must satisfy the selector).
- Changing the EgressIP datapath or SNAT behavior. This is purely a
  control-plane assignment optimization.
- Modifying the health-check/reachability mechanism. Nodes must still pass
  reachability checks regardless of whether they match a selector.

## Introduction

### Background

OVN-Kubernetes provides the EgressIP feature to assign stable source IPs
to egress traffic from selected pods/namespaces. The cluster administrator
labels nodes with `k8s.ovn.org/egress-assignable` to designate them as
candidates for hosting EgressIPs. The `egressIPClusterController` in
ovnkube-cluster-manager then assigns each requested IP to one of the
assignable, ready, and reachable nodes, balancing allocations across the
pool.

### Problem

In production environments, the flat node pool model is insufficient for
operators who need per-EgressIP control over node placement. The use
cases below describe the specific scenarios where this gap causes pain.

## User-Stories/Use-Cases

### Story 1: Zone-Based / Compliance-Driven Placement

As a cluster administrator operating nodes in multiple operational zones
(e.g., DMZ vs internal, audited vs general), I want to ensure EgressIPs
are only assigned to nodes in the appropriate zone, so that traffic
placement respects network topology and compliance boundaries without
requiring a separate cluster. Multiple nodes may share the same subnet
and all pass the existing subnet-membership check, but only a subset
has the right connectivity, firewall rules, or audit status.

Example 1: Nodes in the DMZ are labeled `network-zone=dmz`. The EgressIP
for internet-facing pods specifies `nodeSelector: {matchLabels:
{network-zone: dmz}}`, ensuring IPs are only hosted on nodes with
external connectivity.

Example 2: As a security officer, I want EgressIPs for regulated
workloads to stay within an audited boundary. Nodes in the PCI-DSS
segment are labeled `compliance-zone=pci`. The EgressIP for regulated
workloads specifies `nodeSelector: {matchLabels: {compliance-zone: pci}}`,
ensuring egress traffic stays within the audited segment.

### Story 2: Multi-Tenant Egress Node Pools

As a platform operator running a multi-tenant cluster, I want each
tenant's EgressIP to be assigned only to nodes dedicated to that tenant's
egress traffic, so that tenants cannot exhaust each other's egress
capacity and external firewalls can be configured per-tenant.

Example: Tenant A's nodes are labeled `egress-pool=tenant-a`. Tenant A's
EgressIP specifies `nodeSelector: {matchLabels: {egress-pool: tenant-a}}`.

### Story 3: Graceful Maintenance Drain

As a cluster administrator, I want to drain EgressIPs from specific nodes
by removing a label that the EgressIP's node selector matches, without
affecting other EgressIP objects that don't use that label, so that I can
perform maintenance on a subset of egress nodes without disrupting
unrelated egress traffic.

### Story 4: Cloud Elastic IP Mapping Constraints

As a cloud infrastructure operator, I need to partition EgressIPs across
specific groups of VMs because cloud platforms map Elastic IP pools to
specific VM groups in the infrastructure layer. When multiple pools are
needed, each pool can only be attached to its designated set of VMs.

Example: A cluster has 10 worker nodes on VMs sharing subnet
192.168.40.0/24. The end user needs 10 EgressIPs drawn from the same
subnet, split into two EgressIP objects of 5 each. Each VM supports up
to 10 Elastic IPs, but the cloud infrastructure layer maps each pool
to a different group of VMs:

```text
                            OVN-Kubernetes Cluster
                All nodes labeled: k8s.ovn.org/egress-assignable: ""

            Node 1       Node 2       ...  Node 5       Node 6       ...  Node 10
          ┌──────────┐ ┌──────────┐      ┌──────────┐ ┌──────────┐      ┌──────────┐
EgressIPs │ .224     │ │ .225     │      │ .227     │ │ .229     │      │ .232     │
 assigned │ .229 ← ❌│ │          │      │          │ │          │      │          │
          └────┬─────┘ └────┬─────┘      └────┬─────┘ └────┬─────┘      └────┬─────┘
               │            │                  │            │                  │
─ ─ ─ ─ ─ ─ ─ ┼ ─ ─ ─ ─ ─ ┼ ─ ─ ─ ─ ─ ─ ─ ─┼─ ─ ─ ─ ─ ─┼─ ─ ─ ─ ─ ─ ─ ─ ┼ ─ ─ ─
               │  Infrastructure Layer         │            │                  │
          ┌────┴─────┐ ┌────┴─────┐      ┌────┴─────┐ ┌────┴─────┐      ┌────┴─────┐
VM IPs    │ VM 1     │ │ VM 2     │      │ VM 5     │ │ VM 6     │      │ VM 10    │
          │ .37      │ │ .38      │      │ .41      │ │ .42      │      │ .46      │
          └──────────┘ └──────────┘      └──────────┘ └──────────┘      └──────────┘

ElasticIP  ◄── Pool A: .224–.228 mapped to VMs 1–5 ──►
 Pools                                    ◄── Pool B: .229–.233 mapped to VMs 6–10 ──►

Problem: .229 (Pool B) lands on Node 1 — VM 1 has no Pool B mapping → ❌
         CloudPrivateIPConfig(.229, VM 1) fails.
```

Pool A (.224–.243) is pre-mapped in the cloud to VMs 1–5 only.
Pool B (.244–.263) is pre-mapped to VMs 6–10 only.

**Without nodeSelector**: The `egress-assignable` label is a single
global flag shared by all EgressIP objects — it cannot express
per-object affinity. All 10 nodes are correctly labeled (they all host
*some* EgressIPs), but the controller freely assigns Pool B IPs to
Pool A nodes and vice versa. You cannot unlabel nodes 6–10 to fix
Pool A, because that breaks Pool B. There is no way to express "these
IPs on these nodes, those IPs on those nodes" with a single boolean
label.

**With nodeSelector**: Nodes 1–5 are labeled `eip-pool=a`, nodes 6–10
labeled `eip-pool=b`. EgressIP object A specifies
`nodeSelector: {matchLabels: {eip-pool: "a"}}` and object B specifies
`{eip-pool: "b"}`. Both pools coexist safely — each EgressIP lands
only on VMs where the cloud mapping is configured.

### Ecosystem Precedent

- **EgressService (OVN-Kubernetes)**: Already has a `nodeSelector` field
  (`EgressServiceSpec.NodeSelector`) that limits which nodes can host the
  service's egress traffic. This OKEP proposes the same pattern for
  EgressIP.

- **CiliumEgressGatewayPolicy (Cilium)**: Uses `egressGateway.nodeSelector`
  to designate which node acts as the egress gateway for a policy. The
  pattern of pairing a node selector with an egress IP is well-established
  in the CNI ecosystem.

- **Kubernetes Scheduling**: The `nodeSelector` / `nodeAffinity` pattern
  is the standard Kubernetes mechanism for constraining workloads to
  specific nodes.

## Proposed Solution

Add an optional `nodeSelector` field to `EgressIPSpec`. When specified,
only nodes that:
1. Have the `k8s.ovn.org/egress-assignable` label, AND
2. Match the EgressIP's `nodeSelector`

are eligible to host that EgressIP. When not specified, behavior is
unchanged (all egress-assignable nodes are eligible).

### API Details

#### CRD Change

```go
// EgressIPSpec is a desired state description of EgressIP.
type EgressIPSpec struct {
	// EgressIPs is the list of egress IP addresses requested. Can be IPv4 and/or IPv6.
	// This field is mandatory.
	// +listType=atomic
	EgressIPs []string `json:"egressIPs"`

	// NamespaceSelector applies the egress IP only to the namespace(s) whose label
	// matches this definition. This field is mandatory.
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`

	// PodSelector applies the egress IP only to the pods whose label
	// matches this definition. This field is optional, and in case it is not set:
	// results in the egress IP being applied to all pods in the namespace(s)
	// matched by the NamespaceSelector.
	// +optional
	PodSelector metav1.LabelSelector `json:"podSelector,omitempty"`

	// NodeSelector limits the pool of nodes that can host this EgressIP.
	// Only nodes that have the k8s.ovn.org/egress-assignable label AND
	// match this selector are eligible for assignment. When not specified,
	// all egress-assignable nodes are eligible (existing behavior).
	// Note: the controller never assigns two IPs from the same EgressIP
	// object to the same node. If the nodeSelector matches fewer nodes
	// than the number of requested EgressIPs, the excess IPs will remain
	// unassigned rather than being co-located on an already-used node.
	// +optional
	NodeSelector metav1.LabelSelector `json:"nodeSelector,omitempty"`
}
```

#### Example YAML

```yaml
apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
  name: egressip-tenant-a
spec:
  egressIPs:
    - 192.168.50.10
    - 192.168.50.11
  namespaceSelector:
    matchLabels:
      tenant: a
  nodeSelector:
    matchLabels:
      egress-pool: tenant-a
```

This ensures that `192.168.50.10` and `192.168.50.11` are only assigned
to nodes that have both `k8s.ovn.org/egress-assignable=""` and
`egress-pool=tenant-a` labels.

#### Backwards Compatibility of API

The field is optional with an empty default. An empty `LabelSelector`
matches all objects (per Kubernetes API conventions), so existing EgressIP
objects without a `nodeSelector` continue to behave exactly as before —
all nodes with `k8s.ovn.org/egress-assignable` remain eligible. An
explicitly set but empty selector (`nodeSelector: {}`) is treated
identically — it matches all egress-assignable nodes.

#### Mutability

The `nodeSelector` field is mutable. Updating it triggers re-evaluation
of current assignments: IPs assigned to nodes that no longer match the
new selector are reassigned to nodes that do. This is consistent with
how the existing `egressIPs`, `namespaceSelector`, and `podSelector`
fields are all mutable and trigger reconciliation on update.

#### Validation

No additional CEL validation rules are needed beyond what
`metav1.LabelSelector` already provides. The field follows standard
Kubernetes label selector semantics including `matchLabels` and
`matchExpressions`.

### Implementation Details

#### Component: ovnkube-cluster-manager (`egressIPClusterController`)

The primary change is in the `assignEgressIPs` function in
`go-controller/pkg/clustermanager/egressip_controller.go`.

**Current behavior** (simplified):
1. `getSortedEgressData()` returns all nodes where
   `isEgressAssignable && isReady && isReachable`.
2. `assignEgressIPs()` iterates these nodes, checking subnet membership
   and capacity, and assigns the first available node.

**New behavior**:
1. `assignEgressIPs` receives the EgressIP's `nodeSelector` as an
   additional parameter.
2. After getting the sorted assignable nodes, filter the list to only
   include nodes whose labels match the `nodeSelector`.
3. If the filtered list is empty, check the unfiltered cache to
   distinguish "no egress-assignable nodes match the nodeSelector" from
   "matching nodes exist but are all unavailable" and emit the
   appropriate event.
4. Proceed with the existing assignment logic on the filtered list.

**Reconciliation trigger on node label changes:**

The existing `egressIPNodeEventHandler` in
`go-controller/pkg/clustermanager/egressip_event_handler.go` already
watches node label changes. When a node's labels change, we must
re-evaluate all EgressIP assignments that have a `nodeSelector` to
determine if:
- A previously ineligible node is now eligible (attempt assignment of
  any currently unassigned EgressIPs — valid existing assignments are
  not moved).
- A previously eligible node is no longer eligible (trigger reassignment
  of its EgressIPs to other eligible nodes).

This is handled by extending the existing node update handler to also
check EgressIP nodeSelector matching, similar to how it already checks
the `k8s.ovn.org/egress-assignable` label.

**Reconciliation trigger on nodeSelector spec change:**

When the EgressIP object's `nodeSelector` field is updated, the existing
EgressIP watch handler (`reconcileEgressIP`) fires because the spec
changed. The reconciliation logic re-runs `assignEgressIPs` with the new
selector, invalidating any current assignments to nodes that no longer
match and reassigning those IPs to nodes that do.

**Validation in `ensureAllocatorEgressIPAssignments`:**

The `ensureAllocatorEgressIPAssignments` function (called during sync)
validates that current assignments are still valid. This must be extended
to also verify that the assigned node still matches the EgressIP's
`nodeSelector`. If not, the assignment is marked invalid and the IP is
reassigned.

**Node reboot / unavailability:**

No change to existing behavior. When a node becomes unreachable (reboot,
network partition), the existing reachability checker marks it
unreachable and triggers reassignment to another eligible node. The
nodeSelector filter is applied during reassignment — the IP moves to
another node that is both reachable AND matches the selector. If no
such node exists, the IP remains unassigned until one becomes available.
This is identical to how EgressIPs behave today when no reachable
egress-assignable node exists.

**Multiple EgressIP objects with the same nodeSelector:**

This is fully supported. Multiple EgressIP objects can specify the same
(or overlapping) nodeSelectors. The existing load-balancing logic (sort
by allocation count) distributes IPs across the shared node pool. Each
EgressIP object's IPs are independently anti-co-located (no two IPs
from the same object on one node), but IPs from different objects can
share a node — this is existing behavior and is unchanged.

#### Component: ovnkube-controller

No changes required. The ovnkube-controller reads the EgressIP status
(assigned node + IP) and programs OVN logical router policies and NAT
rules accordingly. The datapath is unchanged — only the assignment
decision in cluster-manager is affected.

#### Component: ovnkube-node

No changes required. ovnkube-node handles the local plumbing (adding the
EgressIP to an interface, configuring ARP/NDP) based on what the
cluster-manager assigns. The node doesn't need to know why it was
selected.

#### Gateway Modes (lgw/sgw)

This feature does not touch the gateway datapath. It only affects the
control-plane assignment decision. Both local gateway and shared gateway
modes are unaffected — the EgressIP datapath flows remain the same
regardless of how the node was selected.

#### Cloud Environment (CloudPrivateIPConfig)

On cloud platforms, EgressIP assignment creates `CloudPrivateIPConfig`
objects. The node selector filtering happens before the
`CloudPrivateIPConfig` is created, so the cloud workflow is unchanged
— it just receives a different (filtered) set of candidate nodes.

#### Interaction with EgressIP MultiNIC (Secondary Host Networks)

The existing secondary host network filtering in `assignEgressIPs`
(which restricts nodes to those hosting the IP's network via MultiNIC)
is applied after the nodeSelector filter. The intersection of both
filters determines the final candidate set.

#### User Defined Networks (UDN)

EgressIP with nodeSelector works identically across all network types —
the default cluster network and User Defined Networks. The nodeSelector
is evaluated purely at the control-plane level during node assignment
in the cluster-manager. The per-network ovnkube-controllers that program
the OVN logical router policies and NAT rules for EgressIP on each
network are unaffected — they read the assigned node from the EgressIP
status and program flows regardless of how that node was chosen. No
per-network or per-topology changes are needed. This applies equally
whether the cluster uses BGP-advertised EgressIPs or not.

### Testing Details

#### Unit Tests

- `egressip_controller_test.go`:
  - Test assignment with nodeSelector matching a subset of nodes.
  - Test assignment when no nodes match the nodeSelector (expect
    unassigned status and warning event).
  - Test that a matching node losing the label triggers reassignment.
  - Test that a new node gaining a matching label triggers assignment
    of previously unassigned EgressIPs (valid assignments do not move).
  - Test empty nodeSelector behaves identically to no nodeSelector.
  - Test nodeSelector with `matchExpressions` (NotIn, Exists, DoesNotExist).
  - Test interaction with cloud provider path (CloudPrivateIPConfig
    created for filtered node only).
  - Test interaction with secondary host network filtering (both
    filters applied).

#### E2E Tests

- Create EgressIP with nodeSelector, verify assignment only to matching
  nodes.
- Remove matching label from assigned node, verify IP migrates to another
  matching node.
- Add matching label to a new node, verify load balancing considers it.
- Verify egress traffic uses the correct source IP when nodeSelector
  restricts the assignment.
- Verify that multiple EgressIP objects with different nodeSelectors
  correctly partition across different node pools.
- Update the EgressIP's nodeSelector to match a different set of nodes,
  verify IPs are reassigned to the new matching nodes.
- Update a node's labels so it no longer matches the nodeSelector while
  simultaneously updating the EgressIP's nodeSelector to match different
  nodes — verify correct convergence without IP duplication or loss.

#### Cross-Feature Interaction Tests

- EgressIP with nodeSelector + UDN: verify EgressIP works correctly for
  pods on User Defined Networks when nodeSelector is specified. Since the
  nodeSelector only affects control-plane assignment and not the datapath,
  the same behavior applies to all network types (default cluster network
  and User Defined Networks alike).

### Documentation Details

- Update `docs/egress-ip.md` with:
  - New `nodeSelector` field documentation.
  - Example YAML showing usage.
  - Explanation of interaction with `k8s.ovn.org/egress-assignable` label.
  - Troubleshooting section for when no nodes match both the label and
    selector.
- Update the EgressIP API reference in `docs/api-reference/`.
- Update `mkdocs.yml` to include this OKEP under Enhancement Proposals.

## Performance and Scale

### Assignment Overhead

The nodeSelector filtering adds one `metav1.LabelSelectorAsSelector()`
conversion per `assignEgressIPs` call (cached per reconciliation, not per
node) and one `selector.Matches()` call per assignable node. At 500
egress-assignable nodes, this is 500 label set comparisons per EgressIP
reconciliation — negligible compared to the existing node iteration and
network lookups.

### Watch Overhead

No new watches are added. The existing node watch already triggers
EgressIP reconciliation on label changes. The only additional work is
iterating EgressIP objects to check which ones have nodeSelectors
affected by the label change. With N EgressIP objects and M node
changes, this adds O(N) selector evaluations per node update.

At scale (1000 EgressIP objects, 500 nodes), a single node label change
triggers at most 1000 selector evaluations — each is a simple map
lookup (for `matchLabels`) completing in microseconds.

### OVN DB Impact

Zero additional OVN DB objects. This feature operates entirely in the
cluster-manager's assignment logic before any OVN DB mutations occur.

### Memory

The `nodeSelector` is stored as part of the EgressIP spec (already in the
informer cache). No additional caching is needed. The compiled
`labels.Selector` is created per-reconciliation and garbage collected
immediately.

## Risks, Known Limitations and Mitigations

### Risk: Overly restrictive selectors leading to unassigned EgressIPs

If the nodeSelector is too restrictive (matches zero nodes, or all
matching nodes are unreachable), the EgressIP remains unassigned.
Additionally, because the controller never co-locates two IPs from the
same EgressIP object on the same node, a nodeSelector that matches
fewer nodes than the number of requested IPs will leave the excess IPs
unassigned. For example, an EgressIP with 3 IPs and a nodeSelector
matching only 2 nodes will assign 2 IPs (one per node) and leave the
3rd unassigned — even if both nodes have the `k8s.ovn.org/egress-assignable`
label and are healthy.

**Mitigation**: Emit distinct Kubernetes events on the EgressIP object:
one for "no egress-assignable nodes match the nodeSelector" (selector
misconfiguration) and another for "matching nodes exist but are all
unavailable" (transient issue). Since `getSortedEgressData()` pre-filters
unreachable/not-ready nodes, the implementation should check the
unfiltered cache against the nodeSelector to distinguish between these
two cases. Add a status condition indicating the reason for
non-assignment.

### Risk: Label race during node label changes

If an operator changes node labels while the controller is mid-assignment,
there's a window where the cached node labels are stale.

**Mitigation**: The existing mutex-protected assignment logic serializes
access. Label changes trigger re-evaluation via the node event handler,
which will correct any stale assignments in the next reconciliation cycle.
This is the same pattern used for the `k8s.ovn.org/egress-assignable`
label today.

### Risk: Increased reconciliation frequency with many EgressIPs

If many EgressIP objects have nodeSelectors referencing the same labels,
a single label change on one node could trigger re-evaluation of many
EgressIP objects.

**Mitigation**: The re-evaluation only checks selector matches (cheap
operation). Actual reassignment only occurs if the current assignment is
invalidated. In the common case (label change doesn't affect existing
assignments), the reconciliation short-circuits.

### Limitation: Per-object node selector, not per-IP

All IPs within a single EgressIP object share the same nodeSelector. If
different IPs need different node pools, separate EgressIP objects must be
created. This is consistent with the existing per-object semantics for
namespace/pod selectors.

## OVN-Kubernetes Version Skew

This feature targets the next minor release (v1.4.0). During a rolling
upgrade:

- **Old cluster-manager, new CRD**: The old cluster-manager does not
  understand the `nodeSelector` field and assigns EgressIPs using the
  existing all-nodes behavior. During this window the hard constraint is
  not enforced. This is the standard Kubernetes upgrade pattern — the CRD
  schema must be updated first (so the field is not pruned by the API
  server), then the controller is rolled. Operators should not create
  EgressIP objects with `nodeSelector` until the new cluster-manager is
  running.
- **New cluster-manager, old EgressIP objects**: Objects without a
  `nodeSelector` field are treated as having an empty selector (match
  all nodes). No behavior change.

## Backwards Compatibility

- The `nodeSelector` field is optional and defaults to empty (matching
  all nodes). Existing EgressIP objects are unaffected.
- No datapath changes. Existing E2E tests continue to pass without
  modification.
- The CRD schema change is additive (new optional field) and does not
  require a new API version.
- Existing E2E tests that create EgressIP objects without `nodeSelector`
  validate that the current behavior is preserved.

## Alternatives

### Alternative 1: Use a separate CRD (EgressIPPool) to define node pools

Instead of adding a nodeSelector to EgressIP directly, create a new
`EgressIPPool` CRD that defines a pool of nodes (via a node selector) and
a set of IP addresses. EgressIP objects would reference a pool instead of
specifying a nodeSelector inline.

**Pros:**
- Separates concerns: pool definition vs. IP-to-pod binding.
- Allows reuse of the same pool across multiple EgressIP objects without
  repeating the selector.
- Could support IP capacity management at the pool level.

**Cons:**
- Introduces a new CRD, adding API surface area and operational
  complexity.
- Requires two objects to achieve what one object can do with an inline
  field.
- Increases coupling — deleting a pool could orphan EgressIP objects.
- The `EgressService` CRD already established the inline `nodeSelector`
  pattern in OVN-Kubernetes. Deviating from this creates inconsistency.

**Decision**: Rejected. The inline nodeSelector is simpler, consistent
with EgressService, consistent with Cilium's approach, and sufficient for
the use cases. A pool abstraction can be layered on top in the future if
demand materializes.

### Alternative 2: Soft scheduling hints (preferred node affinity)

Instead of a hard nodeSelector, add soft scheduling hints (similar to
`preferredDuringScheduling` node affinity) that the controller uses as
preferences rather than hard requirements.

**Pros:**
- More flexible — the controller can still assign to non-preferred nodes
  if preferred ones are full/unreachable.
- Avoids situations where EgressIPs are unassignable due to overly
  restrictive selectors.

**Cons:**
- Significantly more complex implementation (scoring, weights, fallback
  logic).
- Harder for operators to reason about — "where will my EgressIP end up?"
  becomes non-deterministic.
- Regulatory/compliance use cases require hard guarantees, not preferences.
- If implemented via node annotations, it moves configuration out of the
  CRD and onto node objects — the project is moving away from annotations
  toward declarative CRD-based APIs.
- Can be added later as an enhancement to this feature if needed.

**Decision**: Rejected for initial implementation. Hard selector semantics
are simpler, deterministic, and cover the primary use cases. Soft
preferences can be added as a future enhancement.

### Alternative 3: Extend the `k8s.ovn.org/egress-assignable` label to be per-EgressIP

Instead of a selector on the EgressIP object, use per-EgressIP labels on
nodes: `k8s.ovn.org/egress-assignable-<egressip-name>=""`.

**Pros:**
- No CRD change needed.
- Simple to understand: label the node for the specific EgressIP.

**Cons:**
- Violates the principle that the CRD declaratively expresses intent.
  The user must coordinate labels across two objects (EgressIP + nodes).
- Doesn't scale — with hundreds of EgressIP objects, nodes accumulate
  hundreds of labels.
- Label names are limited to 63 characters for the key, and EgressIP
  names can vary, making this error-prone.
- No ecosystem precedent for this pattern.

**Decision**: Rejected. The inline nodeSelector is declarative, scalable,
and follows Kubernetes conventions.

## References

- [PoC Implementation (PR #6791)](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6791) — proof-of-concept implementation
- [EgressIP types.go](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/go-controller/pkg/crd/egressip/v1/types.go) — current EgressIP CRD definition
- [EgressService types.go](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/go-controller/pkg/crd/egressservice/v1/types.go) — EgressService with existing `nodeSelector` pattern
- [egressip_controller.go](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/go-controller/pkg/clustermanager/egressip_controller.go) — cluster-manager EgressIP assignment logic
- [Cilium Egress Gateway Policy](https://docs.cilium.io/en/stable/network/egress-gateway/egress-gateway/) — Cilium's `egressGateway.nodeSelector` approach
- [OpenShift EgressIP documentation](https://docs.redhat.com/en/documentation/openshift_container_platform/4.21/html/ovn-kubernetes_network_plugin/configuring-egress-ips-ovn) — current EgressIP documentation
- [Kubernetes Label Selectors](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors) — standard label selector semantics
