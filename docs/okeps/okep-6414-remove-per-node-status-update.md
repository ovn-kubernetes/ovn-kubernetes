# OKEP-6414: Remove per-node status updates for resources

* Issue: [#6414](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6414)

## Problem Statement

Several OVN-Kubernetes features today write Kubernetes API objects’ **status** subresources frequently in response to **node lifecycle** and **scale** events (adds/deletes/churn). Even when these writes use **server-side apply (SSA)**, they still generate sustained **kube-apiserver** traffic and **etcd** revision growth. Under rapid node scale-out or scale-in, that churn can contribute to large etcd databases (including retained history until compaction) and operational risk if etcd approaches size limits.

The churn comes from two places:

1. **Zone-local controllers** (`ovnkube-controller`) patch status with **per-interconnect-zone** SSA `fieldManager` entries when nodes in that zone are reconciled.
2. **Cluster-manager** rolls those shards up—or maintains its own **aggregated** conditions (for example UDN `NetworkAllocationSucceeded`, dynamic UDN `NodesSelected`, or `StatusManager` success/failure over zone messages)—but those paths are still driven by **node informers** and fire on **every node event**, so they do not solve apiserver/etcd load.

## Goals

* **Stop all per-node status updates** for the following resources:
  * Affected resources:
    * **AdminPolicyBasedExternalRoute**
    * **EgressFirewall**
    * **AdminNetworkPolicy** and **BaselineAdminNetworkPolicy**
    * **NetworkQoS**
    * **EgressQoS**
    * **UserDefinedNetwork**, **ClusterUserDefinedNetwork**, and dynamic UDN allocation
  * For those resources, remove:
    * per-zone / per-node SSA status patches from `ovnkube-controller`
    * **aggregated** status patches from `ovnkube-cluster-manager` that only summarize node or zone sync health
* Expose **node-level** and **zone-level** operational detail through **Prometheus metrics** only, not through CRD `.status` or **Kubernetes Events**.
* Document a clear migration path for anyone parsing today’s status, conditions, or managedFields for debugging.

## Non-Goals

* Using **Kubernetes Events** as a replacement for per-node status (events also add kube-apiserver traffic).
* Replacing etcd compaction, quota, or apiserver request tuning—this OKEP targets **OVN-K-originated** status churn tied to node/zone lifecycle.
* **Coalescing or rate-limiting** per-node status updates instead of removing them—that still leaves etcd growth and watch traffic.
* Adopting a **dense** per-(node, resource) CRD model (one status object per node per EgressFirewall, etc.)—see [Alternatives](#alternatives) for a constrained sidecar-CRD pattern that was considered and not selected as the default.

## Introduction

OVN-Kubernetes today propagates node placement and sync outcomes into CRD status in several ways:

1. **Zone-local controllers** apply status with **`FieldManager` set to the zone id** (EgressFirewall, EgressQoS, NetworkQoS, AdminPolicyBasedExternalRoute, Admin/Baseline Admin Network Policy).
2. **Cluster-manager `StatusManager`** watches those objects, aggregates zone messages into cluster-scoped status fields, and reconciles when **zone membership** changes (via `zone_tracker` on nodes).
3. **Cluster-manager network controllers** update UDN/CUDN conditions from **node reconciliation**—for example `NetworkClusterController.updateNetworkStatus` (`NetworkAllocationSucceeded`) and `updateDynamicUDNStatus` (`NodesSelected`)—even though a single condition is written, **each node event** can still trigger `ApplyStatus`.

At large node counts, ordinary cluster maintenance (rolling upgrades, autoscaling) creates bursts of **ApplyStatus** / **UpdateStatus** traffic across many CR instances. SSA helps with field-level merging but does **not** eliminate etcd writes or watch traffic.

This enhancement removes **per-node and zone-sync-derived status** for the affected resources. Operators observe placement and sync health through **metrics**; CRD status is no longer used as a high-churn channel for that information.

## User-Stories/Use-Cases

Story 1: Operate large clusters without apiserver/etcd overload from SDN status

As a **platform engineer** running a cluster with thousands of nodes and frequent autoscaling, I want **OVN-Kubernetes to stop patching CRD status on node churn** (including cluster-manager aggregation), **so that** the control plane remains stable and etcd stays within safe operating bounds.

Story 2: Debug node-specific failures without reading CRD status

As a **network or support engineer**, when a policy or UDN is unhealthy on **specific nodes**, I want **clear metrics and dashboards** (by node, zone, and feature) **so that** I can narrow incidents quickly without scraping per-zone managedFields or aggregated conditions on many objects.

## Proposed Solution

### High-level direction

1. **No per-node status updates on affected CRDs**  
   Zone controllers **stop** `ApplyStatus` / `UpdateStatus` for routine node or zone sync outcomes. Cluster-manager **stop** writing aggregated conditions or rolled-up status that only reflects “how many nodes/zones succeeded”—including paths such as `StatusManager`, `NetworkClusterController.updateNetworkStatus`, and `updateDynamicUDNStatus` for sync/selection health.

2. **Metrics as the only node-level contract**  
   Introduce (and document) Prometheus metrics from the component that owns the work (`ovnkube-controller`, `ovnkube-cluster-manager`, and/or `ovnkube-node`), for example:
   * **Sync outcome** per node (and per resource key): success/failure as labeled gauge or counter.
   * **Zone id** label where multi-zone semantics matter, with bounded cardinality.
   * **Pre-aggregated** gauges where useful (e.g. count of nodes failing sync for a namespace policy) without writing CRD status.

   Metrics must follow **cardinality guidelines** (avoid unbounded label combinations; prefer recording rules for top-N dashboards).

3. **What may remain on `.status` (if anything)**  
   Status updates that are **not** triggered by node or zone membership events—for example UDN/CUDN conditions that reflect **spec reconciliation** only (NAD creation, transport validation)—are **out of scope** for removal unless they are shown to churn on node events. Any such fields must be documented as **low frequency** and must not be updated from node informer handlers.

4. **Cleanup of legacy shards**  
   One-time or startup cleanup of stale **managedFields** / per-zone SSA entries may still be needed during upgrade; that is not ongoing per-node status reporting.

### API Details

* **Kubernetes CRD schema** may deprecate status fields that only carried node/zone sync detail—exact approach per resource during implementation (prefer **stop writing** before OpenAPI removal).
* **No new CRDs** for node/zone sync health in the default design; metrics are the replacement.
* Public documentation must state that **node/zone health for these features is not available in `.status`**.

### Implementation Details

Work is expected to span:

| Area | Current pattern (simplified) | Target pattern |
|------|------------------------------|----------------|
| Admin Policy Based External Route | Zone SSA + cluster-manager `StatusManager` rollup | **No** node/zone sync status; **add** Prometheus metrics for sync signals |
| Egress Firewall | Zone SSA + `StatusManager` (relevant zones from node placement) | **No** node/zone sync status; **add** metrics |
| Admin / Baseline Admin Network Policy | Per-zone SSA on conditions + zone-delete cleanup | **No** per-zone status patches; **add** metrics |
| Egress QoS / Network QoS | Per-zone SSA on conditions + `StatusManager` | **No** node/zone sync status; **add** metrics |
| UDN / CUDN / Dynamic UDN | Node-driven conditions (`NetworkAllocationSucceeded`, `NodesSelected`, etc.) from `NetworkClusterController` | **Remove** those conditions from `.status`; **add** metrics in `NetworkClusterController` (and related code). **Keep** spec-driven conditions (`NetworkCreated`, `TransportAccepted`) on UDN/CUDN |

Implementation will need to:

* Audit **all** `ApplyStatus` / `UpdateStatus` call paths tied to **node informers**, **zone_tracker**, and **StatusManager** for the above resources—including cluster-manager aggregation.
* Remove or bypass **StatusManager** typed managers and zone rollup logic where the only purpose is node/zone sync reporting.
* Define metric names, labels, and registration in existing OVN-K **metrics** packages; ensure scrape targets match the component that performs the work.
* Provide **Grafana** dashboard examples or metric documentation under `docs/` or the observability guide.

### Testing Details

* **Unit tests** verifying node/zone reconcile **does not** patch status for affected resources.
* **Unit tests** for metric registration and label cardinality bounds where feasible.
* **E2E** / scale checks: node add/delete storms should **not** increase status patch rate for listed CRDs; metrics should reflect node outcomes.
* **Scale / stress** (optional follow-up): measure apiserver request rate and status patch count before/after on large node churn scenarios.

### Documentation Details

* User-facing note: node/zone sync health is **metrics-only** for listed resources; which legacy status fields are deprecated.
* Metrics reference: name, type, labels, and example PromQL for “nodes failing sync for resource X”.

## Risks, Known Limitations and Mitigations

* **Limitation:** No node/zone sync visibility through the Kubernetes API (`kubectl get/describe` on the primary CRD).  
  **Mitigation:** Document metrics-based workflows; map each removed status field to a metric.

* **Limitation:** Requires a **metrics pipeline** (Prometheus or compatible scrape + query path). Clusters without metrics lose node-level observability for these features.  
  **Mitigation:** Document minimum observability requirements; provide example dashboards and alerts.

* **Risk:** Operators today grep `.status`, conditions, or managedFields for zone/node health.  
  **Mitigation:** Release notes and migration guide; metric name stability policy.

* **Risk:** High-cardinality metrics if every CR name × node is exported unbounded.  
  **Mitigation:** Prefer bounded labels, opt-in “detailed” metrics behind a feature flag, or recording rules.

* **Risk:** Temporary loss of visibility if metrics are incomplete.  
  **Mitigation:** Implementation checklist must map each former status signal to a metric before disabling writes.

## OVN-Kubernetes Version Skew

To be set during implementation (target minor release TBD). Likely spans multiple PRs behind feature gates if needed.

## Backwards Compatibility

* **Breaking behavior change** for consumers of per-zone SSA shards, `StatusManager` rollup fields, and UDN node-aggregated conditions.
* **Kubernetes:** Older `kubectl` behavior unchanged; CRD schema compatibility depends on chosen OpenAPI edits (prefer additive deprecation of unused status fields).
* **Downgrade:** Older components may still write status until fully upgraded; document mixed-version behavior.

## Alternatives

The following options were considered. **Option 1** is selected for this OKEP.

### 1. Metrics-only (selected)

Zone controllers and cluster-manager **stop** node/zone sync status writes. Components export Prometheus metrics for per-node/per-zone outcomes.

| Pros | Cons |
|------|------|
| Eliminates status patch storms on node churn, so etcd does not grow from retained status revisions during scale-out or scale-in | No node-level signal on the CRD; `kubectl` cannot show per-node sync health |
| No new CRDs or managedFields shard cleanup on the primary object | **Requires Prometheus** (or compatible scrape/query stack); bare clusters without metrics lose this visibility |
| Fits high-cardinality data (node × resource) better than etcd | GitOps/CI that only watches CRD `.status` must adopt metrics or in-process checks |
| Aligns with common Kubernetes operational practice for controller health | Metric cardinality must be designed carefully to avoid a different kind of scale problem |

**Prior art:** Many controllers expose reconcile health via `/metrics` rather than per-replica status on every watched object; Kubernetes itself uses Node conditions on the Node object, not on every namespaced workload.

### 2. Separate status CRD (sidecar object per node and resource)

Publish sync state on a dedicated CRD with a stable naming scheme (for example `<node_name>.<resource_name>` or `<resource_name>.<node_name>`), similar to the **UplinkState** pattern in OVN-Kubernetes ([PR #6555](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6555)): the node (or zone controller) owns one object; cluster-manager watches it instead of patching the primary CR.

| Pros | Cons |
|------|------|
| Kubernetes-native visibility; `kubectl get` works per node | **Object count** scales as `nodes × resources` if applied naively to every EgressFirewall, EgressQoS, etc. |
| Clear writer ownership (one field manager per sidecar object) | Watch fan-out and list cost grow with cluster size |
| Avoids SSA shard fights on the primary CR’s `.status` | Extra CRDs, RBAC, discovery, and lifecycle (GC when node or policy is deleted) |
| Can be constrained to **sparse** creation (only on failure)—see option 3 | Still etcd writes on failure; dense success reporting recreates the churn problem |

**Not selected as default** because, for namespace-scoped policies at scale, a sidecar per (node, policy) pair often trades patch churn for **object cardinality** unless strictly limited to error cases or low-cardinality relationships (e.g. per-network node state, not per EgressFirewall).

### 3. Assume-success / errors-only status on the primary CR

Do not report success per node. Variants include:

* **(a)** Condition stays `True` until any node fails; only patch when failure set changes.
* **(b)** Patch `.status` only when `handlerErr != nil`; never write on successful node sync.
* **(c)** Errors-only on a sidecar CR (combine with option 2).

| Pros | Cons |
|------|------|
| Large reduction in writes on the happy path (node adds often silent) | **Ambiguity:** `True` vs “not yet reconciled” vs “all nodes synced” |
| Simple for operators who only care about “any failure?” | Stale failure state if a node is deleted without cleanup |
| (b) avoids success-path etcd traffic entirely | Per-zone SSA shards may still need cleanup; does not fix cluster-manager rollup on every node event by itself |
| (c) keeps primary CR clean | Failure path still writes; dense failure storms during incidents |

**Not selected** as the sole solution: it mitigates but does not remove cluster-manager aggregation tied to node informers (for example `StatusManager`, `NetworkAllocationSucceeded`), and success-path silence does not help operators who need positive confirmation per node without metrics.

### 4. Cluster-manager reads metrics and writes low-frequency aggregated status

Hybrid: zone controllers emit metrics (or in-memory state); cluster-manager periodically—or on meaningful change—queries metrics and patches a **single** summary condition on the primary CR.

| Pros | Cons |
|------|------|
| Restores some **kapi-level** summary for `kubectl` / GitOps | Still writes to etcd (lower rate, but not zero) |
| Decouples per-node churn from per-node status patches | Cluster-manager needs a metrics query path or shared in-memory store; risk of **two sources of truth** |
| Can be tuned (e.g. reconcile every N minutes) | Clusters without Prometheus need another aggregation mechanism |
| Distinct from today’s “patch on every node event” rollup | Added complexity; lag between failure and status update |

**Not selected:** still couples operational truth to CRD status and reintroduces cluster-manager status writes that this OKEP removes. Could be revisited if the community prioritizes `kubectl` summary over zero status churn.

### 5. Cluster-manager aggregated status only (drop per-zone SSA, keep rollup)

Zone controllers stop patching; cluster-manager alone aggregates zone/node outcomes into one status field.

| Pros | Cons |
|------|------|
| Single writer on `.status`; simpler managedFields | Rollup still driven by **node informers**—patches on every node event |
| Removes per-zone SSA shards | Rollup still patches on every node event, so etcd revision growth remains |

**Rejected.**

### 6. Rate-limit or coalesce status patches

Batch or throttle `ApplyStatus` calls instead of removing them.

| Pros | Cons |
|------|------|
| Smaller implementation change than full removal | Still writes to etcd; delays visibility |
| Reduces peak apiserver QPS | Does not bound total revision growth under sustained churn |
| | Harder to reason about “last known good” state |

**Rejected.**

### 7. Lease or ConfigMap sidecar status

Store node sync detail in Leases or ConfigMaps keyed by node/resource.

| Pros | Cons |
|------|------|
| Avoids patching primary CR `.status` | Non-standard; poor UX vs CRD conditions |
| TTL on Leases can GC stale entries | Still apiserver objects and watches; not idiomatic for policy health |

**Rejected** in favor of metrics.

## References

* [#6414 - Remove per node status update for resources](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6414)
* OVN-Kubernetes cluster-manager status manager: `go-controller/pkg/clustermanager/status_manager/status_manager.go`
* UDN per-node status: `go-controller/pkg/clustermanager/network_cluster_controller.go` (`updateNetworkStatus`, `updateDynamicUDNStatus`)
* UDN controller (spec-driven status only, to be audited): `go-controller/pkg/clustermanager/userdefinednetwork/controller.go`
* OVN-Kubernetes Uplink / UplinkState (sidecar status CRD pattern): [PR #6555](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6555)
* Kubernetes Node conditions: per-node health on the `Node` object, not sharded across workload CRDs
* cert-manager / Gateway API: coarse `Ready` / `Accepted` conditions (assume success until failure on the primary object)
* OpenShift NMState: `NodeNetworkConfigurationState` per node for network apply status
