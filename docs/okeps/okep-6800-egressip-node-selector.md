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
  nodes are eligible to host that specific EgressIP. This exposes the
  currently hardcoded `k8s.ovn.org/egress-assignable` label as a
  configurable, per-object `egressNodeSelector` field with a CRD default
  that preserves the existing label-based behavior.
- Maintain backwards compatibility for the EgressIP assignment path:
  EgressIP objects that omit the field automatically receive a default
  selector matching `k8s.ovn.org/egress-assignable`, so assignment
  behavior is unchanged.
- Update the health-check probe target set so it is driven by the union
  of `egressNodeSelector` matches across all EgressIP objects, rather
  than unconditionally probing all nodes with the
  `k8s.ovn.org/egress-assignable` label (see Health-Check Membership
  Reconciliation).
- Reuse existing Kubernetes label selector semantics
  (`metav1.LabelSelector`) for consistency with broader Kubernetes
  ecosystem patterns.
- Support failover: if a node matching the selector becomes unavailable
  or unreachable, the EgressIP is reassigned to another node that matches
  the selector (not any arbitrary node).
- Ensure the feature works on both bare-metal and cloud environments
  (CloudPrivateIPConfig flow).

## Non-Goals

- Supporting node affinity with `requiredDuringScheduling` /
  `preferredDuringScheduling` semantics. This OKEP implements hard
  requirement semantics only (matching nodes must satisfy the selector).
  This includes topology-aware scheduling (e.g., preferring nodes in the
  same zone as the majority of selected pods) — operators can already
  achieve zone-aware placement by using `egressNodeSelector` with
  per-zone labels.
- Changing the EgressIP datapath or SNAT behavior. This is purely a
  control-plane assignment optimization.
- Modifying the health-check/reachability probing mechanism itself
  (GRPC/connection-based probe logic). The underlying probe
  implementation is unchanged; only the set of nodes targeted by probes
  changes (see Goals).
- Deprecating or removing the `k8s.ovn.org/egress-assignable` label.
  The label remains the default selector value: EgressIP objects that omit
  `egressNodeSelector` receive a CRD default that matches this label,
  preserving current behavior. Users who prefer the label-based workflow
  can continue using it indefinitely.
- Per-IP node selectors within a single EgressIP object is a non-goal. The
  `egressNodeSelector` applies to all IPs in the object. Splitting IPs
  into separate EgressIP objects with different selectors is not
  equivalent: it sacrifices the anti-co-location guarantee (IPs from
  separate objects can land on the same node), and if both objects
  select the same namespace/pods only one is active while the other
  remains passive. Adding true per-IP selectors would require one of:
  (a) a new `egressIPConfigs` list that pairs each IP with its own
  selector (duplicating IPs between `egressIPs` and the new field),
  (b) converting `egressIPs` from `[]string` to a list of structs
  (breaking API change), or (c) making `egressNodeSelector` itself a
  struct that maps individual IPs to selectors — which adds unnecessary
  complexity to a field that should remain a simple `LabelSelector`.
  None of these are justified without a concrete use case.

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
for internet-facing pods specifies `egressNodeSelector: {matchLabels:
{network-zone: dmz}}`, ensuring IPs are only hosted on nodes with
external connectivity.

Example 2: As a security officer, I want EgressIPs for regulated
workloads to stay within an audited boundary. Nodes in the PCI-DSS
segment are labeled `compliance-zone=pci`. The EgressIP for regulated
workloads specifies `egressNodeSelector: {matchLabels: {compliance-zone: pci}}`,
ensuring egress traffic stays within the audited segment.

### Story 2: Multi-Tenant Egress Node Pools

As a platform operator running a multi-tenant cluster, I want each
tenant's EgressIP to be assigned only to nodes dedicated to that tenant's
egress traffic, so that tenants cannot exhaust each other's egress
capacity and external firewalls can be configured per-tenant by using
disjoint EgressIP objects that select tenant namespaces and disjoint
node selectors for each tenant on those objects.

Example: A cluster has 6 worker nodes split between two tenants —
staging and production. Nodes 1–3 are labeled `egress-pool=staging`,
nodes 4–6 are labeled `egress-pool=production`. Each tenant's
namespaces are labeled accordingly (`tenant: staging` and
`tenant: production`).

Staging EgressIP:
```yaml
spec:
  egressIPs: ["10.0.1.10"]
  namespaceSelector:
    matchLabels:
      tenant: staging
  egressNodeSelector:
    matchLabels:
      egress-pool: staging
```

Production EgressIP:
```yaml
spec:
  egressIPs: ["10.0.2.10", "10.0.2.11"]
  namespaceSelector:
    matchLabels:
      tenant: production
  egressNodeSelector:
    matchLabels:
      egress-pool: production
```

Staging IPs land only on nodes 1–3, production IPs land only on nodes
4–6. Neither tenant can consume the other's egress node capacity.

### Story 3: Controlled Maintenance Drain

As a cluster administrator preparing to put egress nodes into
maintenance across different maintenance windows, I want to exclude
specific nodes from a particular EgressIP's candidate pool in a single
operation, so that IPs move directly to the remaining
eligible nodes without intermediate hops. Today, removing the
`egress-assignable` label from node A causes its IPs to move to node B —
but if node B is also scheduled for maintenance, those IPs hop again
from B to C. I need a way to express "these IPs should only live on
nodes C and D" so that draining A and B happens in one step.

### Story 4: Infrastructure-Level IP-to-VM Mapping (eg. OpenStack UPI)

On platforms without the cloud-network-config-controller (CNCC) — such
as OpenStack deployed via Baremetal UPI (platform=None) — the
infrastructure team manually pre-configures which EgressIPs are allowed
on each VM's port using OpenStack
[allowed-address-pairs](https://docs.openstack.org/neutron/2026.1/admin/intro-os-networking.html).
OVN-Kubernetes is unaware of these static mappings. It assigns EgressIPs
based on the node's primary interface subnet, the `egress-assignable`
label, and node readiness/reachability. When all worker nodes share the
same subnet, OVN-Kubernetes treats them as interchangeable — but the
infrastructure only allows specific IPs on specific VMs.

On cloud IPI platforms (AWS, OpenStack IPI, etc.), CNCC dynamically
configures the cloud to accept whatever IP OVN-Kubernetes assigns via
CloudPrivateIPConfig. But on UPI platforms without CNCC, the
allowed-address-pairs are static: the cloud anti-spoofing mechanism
drops traffic sourced from IPs not in the VM's port configuration, so
an IP assigned to the wrong VM is silently black-holed.

Example: An OpenStack UPI cluster has 3 worker nodes (VMs) on subnet
192.168.0.0/24. All 3 are marked egress-assignable. The operator needs
6 EgressIPs, 2 per node, pre-configured in each VM's
allowed-address-pairs:

```text
                    OVN-Kubernetes Cluster (OpenStack UPI, no CNCC)
              All workers labeled: k8s.ovn.org/egress-assignable: ""

           worker-0          worker-1          worker-2
         ┌───────────┐     ┌───────────┐     ┌───────────┐
EgressIP │ .212      │     │ .213 ← ❌ │     │ .215 ← ❌ │
assigned │ .218 ← ❌ │     │ .214      │     │ .219      │
         └─────┬─────┘     └─────┬─────┘     └─────┬─────┘
               │                 │                  │
─ ─ ─ ─ ─ ─ ─ ┼ ─ ─ ─ ─ ─ ─ ─ ┼ ─ ─ ─ ─ ─ ─ ─ ─┼─ ─ ─ ─ ─ ─ ─
               │  OpenStack Infrastructure         │
         ┌─────┴─────┐     ┌─────┴─────┐     ┌─────┴─────┐
         │ VM w0     │     │ VM w1     │     │ VM w2     │
         │ .10       │     │ .11       │     │ .12       │
         │ AAP:      │     │ AAP:      │     │ AAP:      │
         │ .212,.213 │     │ .214,.215 │     │ .218,.219 │
         └───────────┘     └───────────┘     └───────────┘

AAP = allowed-address-pairs (pre-configured by infra team)

Problem: .218 (belongs on w2) lands on worker-0 → ❌ black-holed
         .213 (belongs on w0) lands on worker-1 → ❌ black-holed
         .215 (belongs on w1) lands on worker-2 → ❌ black-holed
```

Each EgressIP is pre-configured in exactly one VM's
allowed-address-pairs. All 3 workers share the same subnet
(192.168.0.0/24), so OVN-Kubernetes' primary interface subnet check
passes for every node. Without CNCC, there is no
`cloud.network.openshift.io/egress-ipconfig` annotation — capacity is
treated as unlimited. The IP-to-VM mapping is completely invisible to
OVN-Kubernetes.

**Without egressNodeSelector**: The `egress-assignable` label is a single
global flag shared by all EgressIP objects — it cannot express
per-object affinity. All 3 workers look identical to OVN-Kubernetes.
The controller freely distributes the 6 IPs across them without
respecting the static IP-to-VM mapping.There is no way to
express "these IPs on this node, those IPs on that node" with a single
boolean label.

**With egressNodeSelector**: Each worker gets a pool label (e.g.,
`eip-pool: w0`, `eip-pool: w1`, `eip-pool: w2`). Each EgressIP object
targets its corresponding worker:
`egressNodeSelector: {matchLabels: {eip-pool: "w0"}}` for .212/.213,
`{eip-pool: "w1"}` for .214/.215, and `{eip-pool: "w2"}` for
.218/.219. Every IP lands only on the VM where the infrastructure has
pre-configured it.

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

Add an `egressNodeSelector` field to `EgressIPSpec` with a CRD default
that matches the existing `k8s.ovn.org/egress-assignable` label. This
makes the currently hardcoded node selection mechanism visible and
configurable on each EgressIP object:

- **Field omitted**: The CRD default is applied automatically by the API
  server — the selector becomes
  `{matchExpressions: [{key: k8s.ovn.org/egress-assignable, operator: Exists}]}`,
  preserving today's behavior without any code-level fallback.
- **Field set by the user**: The user's value is used as-is. For example,
  `{matchLabels: {egress-pool: tenant-a}}` limits assignment to nodes
  in that pool. An empty selector (`{}`) matches all nodes, which also
  means the health-check probe set expands to every node in the cluster
  — a cost to consider at scale and not a recommended setting for admins.

Because the API server applies the default on reads (including for
existing objects stored before the schema change), the controller sees a
populated selector on every EgressIP object and always uses it — there is
no branching between "selector set" and "legacy fallback" paths.

One caveat of this approach is that users now have two ways to achieve
the same outcome: they can either label nodes with
`k8s.ovn.org/egress-assignable` (relying on the default selector) or set
a custom `egressNodeSelector` on each EgressIP object. Both are valid
workflows and can coexist in the same cluster.

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

	// EgressNodeSelector limits the pool of nodes that can host this
	// EgressIP. Only nodes whose labels match this selector are eligible
	// for assignment.
	// This field is optional. When not specified by the user, the CRD
	// default is applied: {matchExpressions: [{key:
	// k8s.ovn.org/egress-assignable, operator: Exists}]}.
	// An empty selector ({}) matches all nodes — use with caution as it
	// expands the eligible pool and health-check set to the entire cluster.
	// This field restricts only where the IPs in this EgressIP object are
	// placed; it does not reserve the matched nodes for exclusive use.
	// Other EgressIP objects can still consume capacity on the same nodes.
	// +kubebuilder:default={"matchExpressions":[{"key":"k8s.ovn.org/egress-assignable","operator":"Exists"}]}
	// +optional
	EgressNodeSelector *metav1.LabelSelector `json:"egressNodeSelector,omitempty"`
}
```

`EgressNodeSelector` is a pointer (`*metav1.LabelSelector`) rather than a value type for two reasons.
First, `encoding/json`'s `omitempty` tag has no effect on structs — a zero-value struct is never
omitted. Without a pointer, typed Go clients would always serialize this field as
`egressNodeSelector: {}`, preventing the CRD default from ever being applied (the field is never
absent in the submitted object). A nil pointer is correctly omitted by `omitempty`, allowing the
CRD default to be injected at admission. Second, `metav1.LabelSelector` has no required fields,
so `{}` is a valid user choice meaning "match all nodes" per
[Kubernetes API conventions](https://github.com/kubernetes/community/blob/main/contributors/devel/sig-architecture/api-conventions.md#serialization-of-optionalrequired-fields)
— a pointer is the only way to distinguish `nil` (unset, apply default) from `{}` (explicitly
match all nodes).

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
  egressNodeSelector:
    matchLabels:
      egress-pool: tenant-a
```

This ensures that `192.168.50.10` and `192.168.50.11` are only assigned
to nodes with the `egress-pool=tenant-a` label. Because the user
explicitly set `egressNodeSelector`, the CRD default
(`k8s.ovn.org/egress-assignable`) is overridden — the label is not
required on these nodes.

#### Backwards Compatibility of API

The field is `optional` with a CRD `default`. When a user omits
`egressNodeSelector`, the API server injects the default value
(`{matchExpressions: [{key: k8s.ovn.org/egress-assignable, operator: Exists}]}`)
at admission time and on reads from storage. This means:

- **Existing objects** (created before the schema change): the default is
  applied transparently on read, so the controller sees the selector
  without any object migration.
- **New objects without the field**: the default is applied automatically —
  no client changes needed.
- **New objects with a custom selector**: the user's value is used as-is.

#### Mutability

The `egressNodeSelector` field is mutable. Updating it triggers re-evaluation
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

The primary change is in the [`assignEgressIPs`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/d70ad38dc12ee50b9a90d53deb8c32097025b545/go-controller/pkg/clustermanager/egressip_controller.go#L1216) function in
`go-controller/pkg/clustermanager/egressip_controller.go`.

**Current behavior** (simplified):
1. [`getSortedEgressData()`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/d70ad38dc12ee50b9a90d53deb8c32097025b545/go-controller/pkg/clustermanager/egressip_controller.go#L520) returns all nodes where
   [`isEgressAssignable`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/d70ad38dc12ee50b9a90d53deb8c32097025b545/go-controller/pkg/clustermanager/egressip_controller.go#L524) `&& isReady && isReachable`.
2. `assignEgressIPs()` iterates these nodes, checking subnet membership
   and capacity, and assigns the first available node.

**New behavior**:
1. When evaluating an EgressIP, the controller compiles `egressNodeSelector`
   into a label selector and builds the candidate pool from all nodes whose
   labels match the selector and that are infrastructure-ready, ready, and
   reachable. In production, CRD defaulting ensures `egressNodeSelector` is
   always populated; unit tests must set the field explicitly since they bypass
   the API server.
2. The hardcoded [`isEgressAssignable`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/d70ad38dc12ee50b9a90d53deb8c32097025b545/go-controller/pkg/clustermanager/egressip_controller.go#L113) boolean (which matched the
   `k8s.ovn.org/egress-assignable` label) is fully removed and replaced by two
   separate concerns: per-EgressIP selector matching (via `egressNodeSelector`)
   and a node infrastructure-readiness check (host CIDRs parseable). For
   objects that kept the default selector, behavior is identical; for objects
   with a custom selector, only the custom labels matter.
3. If the candidate pool is empty, the controller emits a `NoMatchingNodeFound`
   event and logs the selector for diagnostics.
4. Proceed with the existing assignment logic (subnet, capacity,
   anti-co-location) on the candidate pool.

**Reconciliation trigger on node label changes:**

The existing [`egressIPClusterControllerEventHandler`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/d70ad38dc12ee50b9a90d53deb8c32097025b545/go-controller/pkg/clustermanager/egressip_event_handler.go#L98) in
`go-controller/pkg/clustermanager/egressip_event_handler.go` already
watches node label changes. When a node's labels change, we must
re-evaluate all EgressIP assignments that have an `egressNodeSelector` to
determine if:
- A previously ineligible node is now eligible (attempt assignment of
  any currently unassigned EgressIPs — valid existing assignments are
  not moved).
- A previously eligible node is no longer eligible (trigger reassignment
  of its EgressIPs to other eligible nodes).

This is handled by extending the existing node update handler to compile all
EgressIP selectors and check whether the updated node matches any of them,
replacing the old hardcoded `k8s.ovn.org/egress-assignable` check. The
health-check path uses the same mechanism to probe only nodes that match at
least one EgressIP's selector.

**Reconciliation trigger on egressNodeSelector spec change:**

When the EgressIP object's `egressNodeSelector` field is updated, the existing
EgressIP watch handler ([`reconcileEgressIP`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/d70ad38dc12ee50b9a90d53deb8c32097025b545/go-controller/pkg/clustermanager/egressip_controller.go#L931)) fires because the spec
changed. The reconciliation logic re-runs `assignEgressIPs` with the new
selector, invalidating any current assignments to nodes that no longer
match and reassigning those IPs to nodes that do.

**Validation in [`ensureAllocatorEgressIPAssignments`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/d70ad38dc12ee50b9a90d53deb8c32097025b545/go-controller/pkg/clustermanager/egressip_controller.go#L1855):**

The [`ensureAllocatorEgressIPAssignments`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/d70ad38dc12ee50b9a90d53deb8c32097025b545/go-controller/pkg/clustermanager/egressip_controller.go#L1855) function (called during sync)
validates that current assignments are still valid. The existing
[`isEgressAssignable` check](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/d70ad38dc12ee50b9a90d53deb8c32097025b545/go-controller/pkg/clustermanager/egressip_controller.go#L1490)
in the validation path must be **replaced** (not supplemented) with a
selector match against the EgressIP's `egressNodeSelector`. If the old
label-only check is retained alongside the selector, custom selectors
that target unlabeled nodes would have their assignments invalidated on
every sync — causing an infinite reassign loop. The readiness,
reachability, subnet, and network checks remain unchanged.

**Node reboot / unavailability:**

No change to existing behavior. When a node becomes unreachable (reboot,
network partition), the existing reachability checker marks it
unreachable and triggers reassignment to another eligible node. The
egressNodeSelector filter is applied during reassignment — the IP moves to
another node that is both reachable AND matches the selector. If no
such node exists, the IP remains unassigned until one becomes available.
This is identical to how EgressIPs behave today when no reachable
egress-assignable node exists.

**Multiple EgressIP objects with the same egressNodeSelector:**

This is fully supported. Multiple EgressIP objects can specify the same
(or overlapping) egressNodeSelectors. The existing load-balancing logic (sort
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

The existing secondary host network filtering in [`assignEgressIPs`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/d70ad38dc12ee50b9a90d53deb8c32097025b545/go-controller/pkg/clustermanager/egressip_controller.go#L1216)
(which restricts nodes to those hosting the IP's network via MultiNIC)
is applied after the egressNodeSelector filter. The intersection of both
filters determines the final candidate set.

#### User Defined Networks (UDN)

EgressIP with egressNodeSelector works identically across all network types —
the default cluster network and User Defined Networks. The egressNodeSelector
is evaluated purely at the control-plane level during node assignment
in the cluster-manager. The per-network ovnkube-controllers that program
the OVN logical router policies and NAT rules for EgressIP on each
network are unaffected — they read the assigned node from the EgressIP
status and program flows regardless of how that node was chosen. No
per-network or per-topology changes are needed. This applies equally
whether the cluster uses BGP-advertised EgressIPs or not.

When [Dynamic UDN Node Allocation](../features/user-defined-networks/dynamic-udn.md)
is enabled, EgressIP assignment is one of the activity triggers that
causes a UDN to be rendered on a node. If `egressNodeSelector` directs
an EgressIP to a node where the UDN is not yet rendered, dynamic UDN
will start rendering the network on that node. Conversely, if an
EgressIP is reassigned away from a node (due to a selector change or
node relabel), and that node has no remaining pods or EgressIP
assignments for the UDN, dynamic UDN will tear down the network after
the configured grace period (`--udn-deletion-grace-period`, default
120s). Administrators should be aware of this interaction when
combining `egressNodeSelector` with dynamic UDN — directing EgressIPs
to nodes that don't already run the UDN incurs a network rendering
delay before the datapath is ready.

#### Health-Check Membership Reconciliation

Today, [`checkEgressNodesReachabilityIterate`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/d70ad38dc12ee50b9a90d53deb8c32097025b545/go-controller/pkg/clustermanager/egressip_controller.go#L641) probes every node where
[`isEgressAssignable && isReady`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/d70ad38dc12ee50b9a90d53deb8c32097025b545/go-controller/pkg/clustermanager/egressip_controller.go#L645). With selectors, the probe target set is the
union of all `egressNodeSelector` matches across all EgressIP objects. On each
health-check tick (~5 s), all EgressIP selectors are compiled fresh and each
node's labels are checked against them. Only nodes matching at least one
EgressIP's selector are probed. This avoids maintaining a cached probe set
that would need invalidation on every EgressIP CRUD event and node label
change.

For EgressIP objects using the default selector (matching
`k8s.ovn.org/egress-assignable`), the probe set is identical to today's
behavior.

**Startup ordering**: When a new EgressIP is created with a custom
selector matching nodes that are not already in the probe set, those
nodes have no reachability status yet. The assignment logic requires
`isReachable`, so the first assignment may be delayed by up to one probe
interval while the prober catches up. This is a one-time delay per
newly targeted node.

**Behavioral change from today**: Labeling a node as `egress-assignable`
no longer unconditionally enables health-check probes on that node.
Probes are driven by actual EgressIP selector matches. This scopes
health-check traffic to nodes genuinely in use, but means that
pre-labeling nodes "just in case" no longer warms up their reachability
status in advance.

### Testing Details

#### Unit Tests

- `egressip_controller_test.go`:
  - Test assignment with egressNodeSelector matching a subset of nodes.
  - Test assignment when no nodes match the egressNodeSelector (expect
    unassigned status and warning event).
  - Test that a matching node losing the label triggers reassignment.
  - Test that a new node gaining a matching label triggers assignment
    of previously unassigned EgressIPs (valid assignments do not move).
  - Test that omitting egressNodeSelector results in the CRD default
    (matching `k8s.ovn.org/egress-assignable`) being applied — only
    labeled nodes are eligible, identical to current behavior.
  - Test egressNodeSelector with `matchExpressions` (NotIn, Exists, DoesNotExist).
  - Test that updating egressNodeSelector in a way that still matches the
    currently assigned node does not trigger reassignment (no unnecessary
    churn).
  - Test interaction with cloud provider path (CloudPrivateIPConfig
    created for filtered node only).
  - Test interaction with secondary host network filtering (both
    filters applied).

#### E2E Tests

- Create EgressIP with egressNodeSelector, verify assignment only to matching
  nodes.
- Remove matching label from assigned node, verify IP migrates to another
  matching node.
- Add matching label to a new node, verify load balancing considers it.
- Verify egress traffic uses the correct source IP when egressNodeSelector
  restricts the assignment.
- Verify that multiple EgressIP objects with different egressNodeSelectors
  correctly partition across different node pools.
- Update the EgressIP's egressNodeSelector to match a different set of nodes,
  verify IPs are reassigned to the new matching nodes.
- Update a node's labels so it no longer matches the egressNodeSelector while
  simultaneously updating the EgressIP's egressNodeSelector to match different
  nodes — verify correct convergence without IP duplication or loss.

#### Cross-Feature Interaction Tests

- EgressIP with egressNodeSelector + dual-stack / IPv6: verify that
  egressNodeSelector filtering works correctly for IPv4, IPv6, and
  dual-stack EgressIP assignments. The selector logic is IP-family
  agnostic, but E2E coverage should confirm no regressions.
- EgressIP with egressNodeSelector + UDN: verify EgressIP works correctly for
  pods on User Defined Networks when egressNodeSelector is specified. Since the
  egressNodeSelector only affects control-plane assignment and not the datapath,
  the same behavior applies to all network types (default cluster network
  and User Defined Networks alike).
- EgressIP with egressNodeSelector + Dynamic UDN: verify that assigning an
  EgressIP to a node where the UDN is not yet rendered triggers network
  rendering and that the datapath becomes functional after rendering completes.
  Run EgressIP E2E tests with `--enable-dynamic-udn-allocation` enabled.

### Documentation Details

- Update `docs/features/cluster-egress-controls/egress-ip.md` with:
  - New `egressNodeSelector` field documentation.
  - Example YAML showing usage.
  - Explanation that omitting the field defaults to matching
    `k8s.ovn.org/egress-assignable`, and custom selectors override it.
  - Troubleshooting section for when no nodes match the selector.
  - Best practices section covering:
    - Avoid frequent changes to `egressNodeSelector` or node labels in
      production; there is a race window during label changes where the
      cached node state may be stale, potentially causing brief EgressIP
      reassignment churn. Plan node label schemes upfront and perform
      label changes during a maintenance window.
    - An empty selector (`{}`) matches all nodes, expanding the eligible
      pool and health-check probe set to the entire cluster — use with
      caution at scale.
- Update the EgressIP API reference in `docs/api-reference/egress-ip-api-spec.md`.
- Update `mkdocs.yml` to include this OKEP under Enhancement Proposals.

## Performance and Scale

### Assignment Overhead

The egressNodeSelector filtering adds one `metav1.LabelSelectorAsSelector()`
conversion per [`assignEgressIPs`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/d70ad38dc12ee50b9a90d53deb8c32097025b545/go-controller/pkg/clustermanager/egressip_controller.go#L1216) call (cached per reconciliation, not per
node) and one `selector.Matches()` call per candidate node. At 500
candidate nodes, this is 500 label set comparisons per EgressIP
reconciliation — negligible compared to the existing node iteration and
network lookups.

### Watch Overhead

No new watches are added. The existing node watch already triggers
EgressIP reconciliation on label changes. The only additional work is
iterating EgressIP objects to check which ones have egressNodeSelectors
affected by the label change. With N EgressIP objects and M node
changes, this adds O(N) selector evaluations per node update.

At scale (1000 EgressIP objects, 500 nodes), a single node label change
triggers at most 1000 selector evaluations — each is a simple map
lookup (for `matchLabels`) and is cheap relative to the existing node
iteration and network lookups in the assignment path.

### OVN DB Impact

Zero additional OVN DB objects. This feature operates entirely in the
cluster-manager's assignment logic before any OVN DB mutations occur.

### Memory

The `egressNodeSelector` is stored as part of the EgressIP spec (already in the
informer cache). No additional caching is needed. The compiled
`labels.Selector` is created per-reconciliation and garbage collected
immediately.

## Risks, Known Limitations and Mitigations

### Risk: Overly restrictive selectors leading to unassigned EgressIPs

If the egressNodeSelector is too restrictive (matches zero nodes, or all
matching nodes are unreachable), the EgressIP remains unassigned.
Additionally, because the controller never co-locates two IPs from the
same EgressIP object on the same node, an egressNodeSelector that matches
fewer nodes than the number of requested IPs will leave the excess IPs
unassigned. For example, an EgressIP with 3 IPs and an egressNodeSelector
matching only 2 nodes will assign 2 IPs (one per node) and leave the
3rd unassigned — even if both nodes are healthy.

**Mitigation**: Emit distinct Kubernetes events on the EgressIP object:
one for "no nodes match the egressNodeSelector" (selector
misconfiguration) and another for "matching nodes exist but are all
unavailable" (transient issue). Since [`getSortedEgressData()`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/d70ad38dc12ee50b9a90d53deb8c32097025b545/go-controller/pkg/clustermanager/egressip_controller.go#L520) pre-filters
unreachable/not-ready nodes, the implementation should check the
unfiltered cache against the egressNodeSelector to distinguish between these
two cases.

### Risk: Label race during node label changes

If an operator changes node labels while the controller is mid-assignment,
there's a window where the cached node labels are stale.

**Mitigation**: The existing mutex-protected assignment logic serializes
access. Label changes trigger re-evaluation via the node event handler,
which will correct any stale assignments in the next reconciliation cycle.
This is the same pattern used for the `k8s.ovn.org/egress-assignable`
label today.

### Risk: Increased reconciliation frequency with many EgressIPs

If many EgressIP objects have egressNodeSelectors referencing the same labels,
a single label change on one node could trigger re-evaluation of many
EgressIP objects.

**Mitigation**: The re-evaluation only checks selector matches (cheap
operation). Actual reassignment only occurs if the current assignment is
invalidated. In the common case (label change doesn't affect existing
assignments), the reconciliation short-circuits.

### Limitation: Per-object node selector, not per-IP

All IPs within a single EgressIP object share the same egressNodeSelector. If
different IPs need different node pools, separate EgressIP objects must be
created (see Non-Goals for rationale). This is consistent with the existing
per-object semantics for namespace/pod selectors.

## OVN-Kubernetes Version Skew

This feature targets the next minor release (v1.5.0). The expected
upgrade order is: CRD schema first, then controller rollout. This is the
standard OVN-Kubernetes upgrade sequence (Helm/manifests apply CRDs
before controller pods restart).

During a rolling upgrade:

- **Old cluster-manager, new CRD**: The old cluster-manager does not
  understand the `egressNodeSelector` field and continues to use the
  hardcoded `k8s.ovn.org/egress-assignable` label check. Because the CRD
  default for `egressNodeSelector` matches that same label, the effective
  behavior is unchanged for existing objects. However, if a user creates
  an EgressIP with a custom `egressNodeSelector` during this window, the
  old cluster-manager will ignore the selector and assign the EgressIP to
  any labeled node — potentially outside the requested pool. The custom
  selector is enforced once the new cluster-manager is running, which
  triggers a reconciliation and corrects any mis-assignments. Operators
  should avoid creating EgressIP objects with custom selectors until the
  upgrade is complete.
- **New cluster-manager, old CRD** (abnormal order): This is a general
  concern for any new CRD field, not specific to `egressNodeSelector`.
  If the controller is rolled before the CRD schema is updated, the new
  field has no `default` and existing objects will not have it populated.
  As with any new field, the controller should handle a nil value
  defensively. The standard OVN-Kubernetes upgrade sequence (CRDs first,
  then controller) avoids this scenario.
- **New cluster-manager, new CRD** (normal post-upgrade): The controller
  reads `egressNodeSelector` from every object (always present due to CRD
  defaulting) and uses it as the sole candidate filter. Objects with the
  default selector behave identically to the old label-based path.
  Objects with custom selectors are handled by the new code path.

## Backwards Compatibility

- The `egressNodeSelector` field has a CRD default that matches
  `k8s.ovn.org/egress-assignable`. Existing EgressIP objects that omit
  the field automatically receive this default on read, so their
  behavior is unchanged.
- No datapath changes. Existing E2E tests continue to pass without
  modification.
- The CRD schema change is additive (new optional field with a default)
  and does not require a new API version.
- Existing E2E tests that create EgressIP objects without `egressNodeSelector`
  validate that the current behavior is preserved.

## Upgrade Behavior

CRD defaulting handles the transition transparently:

1. The updated CRD schema adds the `egressNodeSelector` field with a
   `default` of `{matchExpressions: [{key: k8s.ovn.org/egress-assignable,
   operator: Exists}]}`.
2. Existing EgressIP objects stored without the field automatically
   receive the default on read — no object migration is required.
3. The new controller replaces the hardcoded [`isEgressAssignable`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/d70ad38dc12ee50b9a90d53deb8c32097025b545/go-controller/pkg/clustermanager/egressip_controller.go#L113) check
   with a selector evaluation. Because every object now has a selector
   (either user-set or the default), the single code path covers both
   existing and new EgressIPs.
4. Users who prefer the label-based workflow need not change anything.
   Users who want per-object selectors set `egressNodeSelector`
   explicitly, overriding the default.

## Alternatives

### Alternative 1: Use a separate CRD (EgressIPPool) to define node pools

Instead of adding an egressNodeSelector to EgressIP directly, create a new
`EgressIPPool` CRD that defines a pool of nodes (via a node selector) and
a set of IP addresses. EgressIP objects would reference a pool instead of
specifying an egressNodeSelector inline.

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

**Decision**: Rejected. The inline egressNodeSelector is simpler, consistent
with EgressService, consistent with Cilium's approach, and sufficient for
the use cases. A pool abstraction can be layered on top in the future if
demand materializes.

### Alternative 2: Extend the `k8s.ovn.org/egress-assignable` label to be per-EgressIP

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

**Decision**: Rejected. The inline egressNodeSelector is declarative, scalable,
and follows Kubernetes conventions.

### Alternative 3: AND semantics (`egressNodeSelector` intersected with `egress-assignable`)

Require both the `k8s.ovn.org/egress-assignable` label AND the
`egressNodeSelector` to match for a node to be eligible.

**Pros:**
- Simple to implement — filter by label first, then by selector.
- The label acts as a cluster-admin veto (only node-edit privilege
  holders can opt a node into egress duty).

**Cons:**
- Operators must forever maintain both the label and the selector,
  doubling configuration burden.
- The selector alone already provides the same RBAC-like veto: only
  someone who can label nodes can make them eligible. Adding the label
  as a hard prerequisite is redundant.
- Users who want custom selectors still need to label nodes, defeating
  the purpose of per-object selectors.

**Decision**: Rejected. CRD defaulting achieves backwards compatibility
without an intersection — the default selector matches the label, and
custom selectors fully replace it.

### Alternative 4: "If set, use selector; else fall back to label" (no defaulting)

Use a pointer field (`*metav1.LabelSelector`) with `omitempty`. When
nil, the controller falls back to the hardcoded
`k8s.ovn.org/egress-assignable` label check. When set (even to `{}`),
the selector takes full ownership and the label is ignored. This
enables a phased deprecation of the label.

**Pros:**
- Provides a path to eventually deprecate the
  `k8s.ovn.org/egress-assignable` label entirely.
- The nil vs empty distinction gives fine-grained control: nil = legacy,
  `{}` = all nodes, `{matchLabels: ...}` = custom pool.

**Cons:**
- Requires branching logic in the controller ("is nil? use label; else
  use selector"), increasing code complexity.
- Requires a multi-phase migration plan (introduce field, deprecate
  label, remove label) spanning several releases.
- Breaking the label is risky for a heavily used feature (60%+ of
  OpenShift clusters use EgressIP) and requires coordination with
  external components like CNCC.
- Adding a required field later (to finish the deprecation) is itself
  a breaking API change.

**Decision**: Rejected in favor of CRD defaulting, which avoids the
entire deprecation and migration story while providing the same
per-object selector functionality.

## References

- [PoC Implementation (PR #6791)](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6791) — proof-of-concept implementation
- [EgressIP types.go](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/d70ad38dc12ee50b9a90d53deb8c32097025b545/go-controller/pkg/crd/egressip/v1/types.go) — current EgressIP CRD definition
- [EgressService types.go](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/d70ad38dc12ee50b9a90d53deb8c32097025b545/go-controller/pkg/crd/egressservice/v1/types.go) — EgressService with existing `nodeSelector` pattern
- [egressip_controller.go](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/d70ad38dc12ee50b9a90d53deb8c32097025b545/go-controller/pkg/clustermanager/egressip_controller.go) — cluster-manager EgressIP assignment logic
- [Cilium Egress Gateway Policy](https://docs.cilium.io/en/stable/network/egress-gateway/egress-gateway/) — Cilium's `egressGateway.nodeSelector` approach
- [OpenShift EgressIP documentation](https://docs.redhat.com/en/documentation/openshift_container_platform/4.21/html/ovn-kubernetes_network_plugin/configuring-egress-ips-ovn) — current EgressIP documentation
- [Kubernetes Label Selectors](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors) — standard label selector semantics
