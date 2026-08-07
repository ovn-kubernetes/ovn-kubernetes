# OKEP-6414: Remove per-node status updates for resources

* Issue: [#6414](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6414)

## Problem Statement

Several OVN-Kubernetes features today write Kubernetes API objects’ **status** subresources frequently in response to **node lifecycle** and **scale** events (adds/deletes/churn). Even when these writes use **server-side apply (SSA)**, they still generate sustained **kube-apiserver** traffic and **etcd** revision growth. Under rapid node scale-out or scale-in, that churn can contribute to large etcd databases (including retained history until compaction) and operational risk if etcd approaches size limits.

The churn comes from two places:

1. **Zone-local controllers** (`ovnkube-controller`) patch status with **per-interconnect-zone** SSA `fieldManager` entries when nodes in that zone are reconciled.
2. **Cluster-manager** rolls those shards up—or maintains its own **aggregated** conditions (for example UDN `NetworkAllocationSucceeded`, dynamic UDN `NodesSelected`, or `StatusManager` success/failure over zone messages)—but those paths are still driven by **node informers** and fire on **every node event**, so they do not solve apiserver/etcd load.

## Goals

* **Stop all node-driven status writes** for the following resources:
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

* Using **Kubernetes Events** as a replacement for node-driven status.
* Replacing etcd compaction, quota, or apiserver request tuning—this OKEP targets **OVN-K–originated** status churn tied to node/zone lifecycle.
* Defining a new **first-class** per-node CRD solely to mirror old status (metrics are the preferred exposure for node detail).
* **Coalescing or rate-limiting** node-driven status instead of removing it—that still leaves etcd growth and watch traffic.

## Introduction

OVN-Kubernetes today propagates node placement and sync outcomes into CRD status in several ways:

1. **Zone-local controllers** apply status with **`FieldManager` set to the zone id** (EgressFirewall, EgressQoS, NetworkQoS, AdminPolicyBasedExternalRoute, Admin/Baseline Admin Network Policy).
2. **Cluster-manager `StatusManager`** watches those objects, aggregates zone messages into cluster-scoped status fields, and reconciles when **zone membership** changes (via `zone_tracker` on nodes).
3. **Cluster-manager network controllers** update UDN/CUDN conditions from **node reconciliation**—for example `NetworkClusterController.updateNetworkStatus` (`NetworkAllocationSucceeded`) and `updateDynamicUDNStatus` (`NodesSelected`)—even though a single condition is written, **each node event** can still trigger `ApplyStatus`.

At large node counts, ordinary cluster maintenance (rolling upgrades, autoscaling) creates bursts of **ApplyStatus** / **UpdateStatus** traffic across many CR instances. SSA helps with field-level merging but does **not** eliminate etcd writes or watch traffic.

This enhancement removes **node- and zone-sync-derived status entirely** for the affected resources. Operators observe placement and sync health through **metrics**; CRD status is no longer used as a high-churn channel for that information.

## User-Stories/Use-Cases

Story 1: Operate large clusters without apiserver/etcd overload from SDN status

As a **platform engineer** running a cluster with thousands of nodes and frequent autoscaling, I want **OVN-Kubernetes to stop patching CRD status on node churn** (including cluster-manager aggregation), **so that** the control plane remains stable and etcd stays within safe operating bounds.

Story 2: Debug node-specific failures without reading CRD status

As a **network or support engineer**, when a policy or UDN is unhealthy on **specific nodes**, I want **clear metrics and dashboards** (by node, zone, and feature) **so that** I can narrow incidents quickly without scraping per-zone managedFields or aggregated conditions on many objects.

## Proposed Solution

### High-level direction

1. **No node-driven status on affected CRDs**  
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
   One-time or startup cleanup of stale **managedFields** / per-zone SSA entries may still be needed during upgrade; that is not ongoing node-driven status reporting.

### API Details

* **Kubernetes CRD schema** may deprecate status fields that only carried node/zone sync detail—exact approach per resource during implementation (prefer **stop writing** before OpenAPI removal).
* **No new CRDs** for per-node status; metrics are the replacement.
* Public documentation must state that **node/zone health for these features is not available in `.status`**.

### Implementation Details

Work is expected to span:

| Area | Current pattern (simplified) | Target pattern |
|------|------------------------------|----------------|
| Admin Policy Based External Route | Zone SSA + cluster-manager `StatusManager` rollup | **No** node/zone sync status; metrics only |
| Egress Firewall | Zone SSA + `StatusManager` (relevant zones from node placement) | **No** node/zone sync status; metrics only |
| Admin / Baseline Admin Network Policy | Per-zone SSA on conditions + zone-delete cleanup | **No** per-zone status patches; metrics only |
| Egress QoS / Network QoS | Per-zone SSA on conditions + `StatusManager` | **No** node/zone sync status; metrics only |
| UDN / CUDN / Dynamic UDN | Zone/node-driven conditions (`NetworkAllocationSucceeded`, `NodesSelected`, etc.) | **Remove** those conditions; **add** metrics from `NetworkClusterController` (and related code) for the same node/allocation signals |

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

* **Risk:** Operators today grep `.status`, conditions, or managedFields for zone/node health.  
  **Mitigation:** Document metric equivalents; release notes; map each removed signal to a metric before merge.

* **Risk:** High-cardinality metrics if every CR name × node is exported unbounded.  
  **Mitigation:** Prefer bounded labels, opt-in “detailed” metrics behind a feature flag, or recording rules.

* **Risk:** Temporary loss of visibility if metrics are incomplete.  
  **Mitigation:** Implementation checklist must map each former status signal to a metric before disabling writes.

* **Limitation:** `kubectl describe` on affected CRDs will no longer show per-node sync detail in status.  
  **Mitigation:** Document observability workflow using metrics only.

## OVN-Kubernetes Version Skew

To be set during implementation (target minor release TBD). Likely spans multiple PRs behind feature gates if needed.

## Backwards Compatibility

* **Breaking behavior change** for consumers of per-zone SSA shards, `StatusManager` rollup fields, and UDN node-aggregated conditions.
* **Kubernetes:** Older `kubectl` behavior unchanged; CRD schema compatibility depends on chosen OpenAPI edits (prefer additive deprecation of unused status fields).
* **Downgrade:** Older components may still write status until fully upgraded; document mixed-version behavior.

## Alternatives

1. **Rate-limit or coalesce** status patches—rejected; still writes to etcd and does not meet this enhancement’s goals.
2. **Cluster-manager aggregated status only** (drop per-zone writes, keep rollup)—rejected; aggregation still runs on every node event and preserves apiserver churn.
3. **Per-node status CRDs**—rejected; multiplies object count and watch traffic.
4. **Lease / ConfigMap sidecar status**—non-standard; metrics preferred.

## References

* [#6414 – Remove per node status update for resources](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6414)
* Cluster-manager status manager: `go-controller/pkg/clustermanager/status_manager/status_manager.go`
* UDN node-driven status: `go-controller/pkg/clustermanager/network_cluster_controller.go` (`updateNetworkStatus`, `updateDynamicUDNStatus`)
* UDN controller (spec-driven status only, to be audited): `go-controller/pkg/clustermanager/userdefinednetwork/controller.go`
