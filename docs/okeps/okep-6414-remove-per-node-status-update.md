# OKEP-6414: Remove per-node status updates for resources

* Issue: [#6414](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6414)

## Problem Statement

Several OVN-Kubernetes features today write Kubernetes API objects’ **status** subresources frequently in response to **node lifecycle** and **scale** events (adds/deletes/churn). Even when these writes use **server-side apply (SSA)**, they still generate sustained **kube-apiserver** traffic and **etcd** revision growth. Under rapid node scale-out or scale-in, that churn can contribute to large etcd databases (including retained history until compaction) and operational risk if etcd approaches size limits.

The churn comes from two places:

1. **Per-node `ovnkube-controller` instances** patch status with SSA `fieldManager` set to the **node name** when that node reconciles policy objects.
2. **Cluster-manager** rolls those shards up into cluster-scoped summary fields via **`StatusManager`** (for example EgressFirewall `status.status`, ANP `Ready-In-Zone-<node>` conditions on the primary CR).

**(C)UDN status is different:** `NetworkCreated` and `NetworkAllocationSucceeded` on UDN/CUDN; `TransportAccepted` and `UplinksReady` on **CUDN only**. These conditions are **low churn** on the happy path (deduped patches; no per-node shards on the primary object) and do not scale with node count × resource count the way policy-like per-node SSA shards do. The high-churn (C)UDN case is **dynamic UDN** `NodesSelected` on the primary CR, patched when the active-node count changes. That is addressed separately via a sidecar CR ([#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788)).

## Goals

* **Stop high-churn per-node status patches** on **policy-like** resources (success/error reporting only; no other controller consumes the per-node outcome as a control-plane input):
  * **AdminPolicyBasedExternalRoute**
  * **EgressFirewall**
  * **AdminNetworkPolicy** and **BaselineAdminNetworkPolicy**
  * **NetworkQoS**
  * **EgressQoS**
* Replace per-node status detail with **Prometheus metrics** (default). A **feature flag** (enabled by default) selects metrics mode; operators who disable it get today's status-update behaviour.
* Provide an **optional, low-frequency** cluster-manager summary on the primary policy CR, derived by [periodically querying Prometheus](#cm-rollup-prometheus-scrape). The rollup runs only when a metrics pipeline is available.
* For **dynamic UDN** only: introduce a **sparse sidecar CR** per **(node, network)** for active/render state ([#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788)); **deprecate** `NodesSelected` on the primary (C)UDN.
* **Keep** existing (C)UDN status conditions as they are today: `NetworkCreated` and `NetworkAllocationSucceeded` on UDN/CUDN; `TransportAccepted` and `UplinksReady` on **CUDN only**. They do not drive etcd growth on routine node churn the way policy-like per-node SSA shards do (see [(C)UDN status churn](#cudn-status-churn)).
* Document a clear migration path for anyone parsing today's status, conditions, or managedFields for debugging.

## Non-Goals

* Using **Kubernetes Events** as a replacement for per-node status (events also add kube-apiserver traffic).
* Replacing etcd compaction, quota, or apiserver request tuning—this OKEP targets **OVN-Kubernetes-originated** status churn tied to node lifecycle on policy-like CRs.
* **Coalescing or rate-limiting** per-node status updates instead of removing them—that still leaves etcd growth and watch traffic.
* Adopting a **dense** per-(node, policy) CRD model (one status object per node per EgressFirewall, etc.). A **sparse / low-cardinality** sidecar CRD for **dynamic UDN only** is in scope; see [separate status CRD (sidecar)](#2-separate-status-crd-sidecar-object-per-node-and-resource) in Alternatives.
* Replacing spec-driven or low-churn (C)UDN status conditions (`NetworkCreated`, `NetworkAllocationSucceeded` on UDN/CUDN; `TransportAccepted`, `UplinksReady` on CUDN only).
* Sidecar CRs for **static** UDNs (would scale as **nodes × UDNs**; see [separate status CRD (sidecar)](#2-separate-status-crd-sidecar-object-per-node-and-resource) in Alternatives).

## Introduction

OVN-Kubernetes propagates per-node sync outcomes into CRD status in several ways:

1. **Per-node `ovnkube-controller`** applies status with **`FieldManager` set to the node name** (EgressFirewall, EgressQoS, NetworkQoS, AdminPolicyBasedExternalRoute, Admin and Baseline Admin Network Policy). ANP/BANP use `Ready-In-Zone-<node>` condition types—one condition per node.
2. **Cluster-manager `StatusManager`** watches those objects, aggregates per-node shards (`messages[]` or `Ready-In-Zone-<node>`) into cluster-scoped summary fields, and reconciles when the set of OVN-managed nodes changes (`zone_tracker` when a node gains or loses host subnet—not on every node heartbeat).
3. **Cluster-manager network controllers** update UDN/CUDN conditions—`NetworkAllocationSucceeded` (deduped; sparse on the happy path), `UplinksReady` on **CUDN only** (deduped; see [(C)UDN status churn](#cudn-status-churn)), and, for dynamic UDN only, `NodesSelected` (patches when the active-node **count** changes).

At large node counts, ordinary cluster maintenance (rolling upgrades, autoscaling) creates bursts of **ApplyStatus** / **UpdateStatus** traffic across many **policy** CR instances. SSA helps with field-level merging but does **not** eliminate etcd writes or watch traffic.

Most policy-like features only need a **success/error** reporting channel for operators. This enhancement stops using CRD `.status` as a **high-churn, per-node** channel for that when the feature flag is on (default). Operators debug through **metrics**; an optional **low-frequency** summary on the primary object is fed by Prometheus queries when a scrape stack is available.

Dynamic UDN is different: other cluster-manager controllers (notably route advertisements) need to know **which nodes have a network rendered**, and they cannot consume Prometheus as a control-plane API. That case uses a kapi-visible per-node **sidecar**; see [Dynamic UDN sidecar](#dynamic-udn-sidecar).

## User-Stories/Use-Cases

Story 1: Operate large clusters without apiserver/etcd overload from per-node policy CR status patches

As a **platform engineer** running a cluster with thousands of nodes and frequent autoscaling, I want **OVN-Kubernetes to stop patching policy CRD status on every node add/delete** (by default), **so that** the control plane remains stable and etcd stays within safe operating bounds.

Story 2: Debug node-specific policy failures without scraping managedFields

As a **network or support engineer**, when a policy is unhealthy on **specific nodes**, I want **clear metrics and dashboards** (by node and feature) **so that** I can narrow incidents quickly without scraping per-node managedFields on many objects.

Story 3: Revert to legacy status when metrics are not an option

As a **platform engineer** who cannot run Prometheus yet, I want to **disable the feature flag** and keep today's per-node status shards and `StatusManager` rollup **so that** existing tooling that reads `.status` continues to work unchanged.

Story 4: A coarse kapi summary without per-node shards

As a **GitOps engineer**, I want a **single summary field** on the policy CR (updated infrequently from metrics) **so that** I can gate deployments on “Is there any failure?” without per-node detail on the object.

Story 5: Drive other controllers from per-node UDN render state

As a **cluster-manager controller** (for example route advertisements), I want a **Kubernetes-API object** that says whether a (C)UDN is **active** and **rendered** on a given node, **so that** I can advertise or tear down correctly without heuristics such as tunnel-id annotations ([#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788)).

## Proposed Solution

### High-level direction

1. **Policy-like CRDs: metrics instead of per-node status (default)**  
   When the feature flag is **on** (default), per-node `ovnkube-controller` instances **stop** `ApplyStatus` / `UpdateStatus` for routine **success/error** outcomes on AdminPolicyBasedExternalRoute, EgressFirewall, Admin and Baseline Admin Network Policy, NetworkQoS, and EgressQoS. Cluster-manager **stops** `StatusManager` rollup driven by per-node shards on the primary CR. Per-node detail is exported as **Prometheus metrics**.

2. **Feature flag for opt-out**  
   A single flag (name TBD during implementation, for example `PolicySyncStatusViaMetrics`, default **`true`**) controls this behaviour cluster-wide:
   * **`true` (default):** metrics mode—no per-node SSA shards on policy CRs; optional CM summary from Prometheus (below).
   * **`false`:** legacy mode—today's per-node status patches and `StatusManager` rollup unchanged.

3. **Optional CM summary via Prometheus**  
   When metrics mode is on, cluster-manager may **periodically query Prometheus** and patch a **single** aggregated summary on each policy CR **only when the queried outcome changes**. This is **not** driven by node informer events. The rollup is **skipped** when Prometheus is not configured or not reachable (see [Metrics availability](#metrics-availability)). When the feature flag is **off**, rollup uses today's `StatusManager` path (reading per-node shards from the primary CR). See [CM Prometheus rollup](#4-cluster-manager-writes-low-frequency-aggregated-status-selected-for-policy-summary) in Alternatives.

4. **(C)UDN status: keep as-is**  
   Existing conditions on the primary `UserDefinedNetwork` / `ClusterUserDefinedNetwork` remain:
   * **Spec-driven** — `NetworkCreated` on UDN/CUDN; `TransportAccepted` and `UplinksReady` on **CUDN only**—unchanged.
   * **`NetworkAllocationSucceeded`** on UDN/CUDN—unchanged; already sparse on the happy path (patches on allocation failure/recovery, not every node heartbeat).

   See [(C)UDN status churn](#cudn-status-churn) for etcd impact on node scale events.

5. **Dynamic UDN: sidecar CR; deprecate `NodesSelected`**  
   For dynamic UDN only, per-node **active** and **rendered** state moves to a **sidecar CR**. `NodesSelected` on the primary (C)UDN is **deprecated** and **no longer written** once the sidecar is available. See [Dynamic UDN sidecar](#dynamic-udn-sidecar).

6. **Cleanup of legacy shards**  
   One-time or startup cleanup of stale **managedFields** / per-node SSA entries may still be needed when upgrading to metrics mode; that is not ongoing per-node status reporting.

### Feature flag

| Flag (TBD) | Default | When `true` | When `false` |
|------------|---------|-------------|--------------|
| `PolicySyncStatusViaMetrics` (example name) | `true` | Per-node controllers export metrics; **no** per-node SSA shards on policy CRs; CM uses Prometheus rollup (if metrics available) or no summary | Per-node controllers write legacy status; CM `StatusManager` rollup **as today** |

The flag applies to **policy-like** resources only. It does **not** disable (C)UDN status or the dynamic-UDN sidecar.

### Metrics (policy-like resources)

Introduce (and document) Prometheus metrics from the component that owns the work (`ovnkube-controller` on each node), for example:

* **Sync outcome** per node (and per resource key): success/failure as labeled gauge or counter.
* **`node` label** with bounded cardinality.
* **Pre-aggregated** gauges where useful (e.g. count of nodes failing sync for a namespace policy) to support CM rollup queries without high-cardinality PromQL.

Metrics must follow **cardinality guidelines** (avoid unbounded label combinations; prefer recording rules for top-N dashboards). Each removed status field must map to a documented metric before the flag default disables status writes.

### Metrics availability

Cluster-manager needs a **central** way to decide whether Prometheus-backed rollup can run:

* **Configuration:** Prometheus query URL (or reuse an existing OVN-Kubernetes observability / monitoring config knob if one exists).
* **Health:** periodic probe (for example a simple `up` or canary query against a known OVN-Kubernetes metric). Cache **available / unavailable** with a TTL; do not block per-node reconcile on probe failure.
* **Behaviour:**
  * Metrics mode **on** + Prometheus **available** → per-node metrics exported; CM **may** run periodic rollup and patch summary status when the aggregate changes.
  * Metrics mode **on** + Prometheus **unavailable** → per-node metrics still exported (for when scrape comes online); CM **does not** patch summary status (log at low verbosity); operators use metrics endpoints directly or wait for Prometheus.
  * Metrics mode **off** → legacy status path; Prometheus availability is irrelevant to rollup.

Document that the CM summary is **eventually consistent** (scrape interval + query interval + lag) and is a **cache** of metrics, not the source of truth for per-node debug.

### CM rollup (Prometheus scrape)

When `PolicySyncStatusViaMetrics` is **true** and Prometheus is **available**:

1. Per-node `ovnkube-controller` instances write outcomes to **metrics only** (no per-node shards on the primary CR).
2. Cluster-manager runs a **timer** (interval TBD, for example 30–60s) per policy type or a shared worker.
3. CM issues **PromQL** queries (predefined per resource type) to compute an aggregate: for example “any node failed for this EgressFirewall?” or “all relevant nodes succeeded?”.
4. CM patches the primary CR **summary field only** (`status.status`, or a single condition) when the query result **differs** from what is already on the object.
5. CM is the **sole writer** of that summary field (`fieldManager: cluster-manager`).

When the feature flag is **false**, steps 1–4 are skipped; today's `StatusManager` informer + per-node shard aggregation applies instead.

Do **not** implement rollup as “reconcile `StatusManager` on every node informer event” in metrics mode.

### Dynamic UDN sidecar

[`#6788`](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788) needs a per-node signal that a (C)UDN is **rendered** (and later torn down) on a given node. That is **control-plane state other controllers must read**, not operator debug detail. Metrics are not sufficient.

**Scope:** **dynamic UDN only** (`EnableDynamicUDNAllocation`). Static UDNs do **not** get sidecars (would scale as nodes × UDNs with little benefit over today's annotation-based signals).

**Selected approach:** [separate status CRD (sidecar)](#2-separate-status-crd-sidecar-object-per-node-and-resource), following **UplinkState** ([PR #6555](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6555)).

#### CRD sketch (name/API finalized with [#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788))

| Field | Value |
|-------|--------|
| **Kind** | `NetworkRenderState` (proposed) |
| **API group** | `k8s.ovn.org/v1alpha1` (proposed) |
| **Scope** | Cluster |
| **Naming** | `<network>.<node>` (same pattern as `UplinkState`, e.g. `tenant-blue.worker-1`) |
| **Labels** | `k8s.ovn.org/network`, `k8s.ovn.org/node` for list/watch |

**Spec** (immutable identity):

```yaml
spec:
  networkName: tenant-blue   # internal network name
  nodeName: worker-1
```

**Status** (`status.conditions[]`):

| Condition | Meaning |
|-----------|---------|
| **`Active`** | Network is **active** on this node (pods or EgressIPs present)—replaces `NodesSelected` per-node semantics |
| **`Rendered`** | OVN logical topology for this network is **programmed** on this node ([#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788)) |

Example (healthy):

```yaml
apiVersion: k8s.ovn.org/v1alpha1
kind: NetworkRenderState
metadata:
  name: tenant-blue.worker-1
  labels:
    k8s.ovn.org/network: tenant-blue
    k8s.ovn.org/node: worker-1
spec:
  networkName: tenant-blue
  nodeName: worker-1
status:
  conditions:
    - type: Active
      status: "True"
      reason: NetworkReferenced
      message: Network is active on this node (pods or egress IPs present)
    - type: Rendered
      status: "True"
      reason: RenderSucceeded
      message: OVN logical network topology is programmed on this node
```

**Lifecycle:**

1. First pod / EgressIP on `(node, network)` → **create** sidecar (`Active=True`, `Rendered=False`).
2. Per-node `ovnkube-controller` finishes OVN programming → patch `Rendered=True` (RA may advertise).
3. Last workload leaves + grace period → teardown → `Rendered=False` → **delete** sidecar.

**Writers / readers:**

* **Writer:** `ovnkube-controller` on the node that renders the network owns status patches.
* **Readers:** cluster-manager (route advertisements and future consumers) **watch/list** sidecars; stop using tunnel-id annotations and `NodesSelected` on the primary CR.

**Deprecation:** `NodesSelected` on the primary `UserDefinedNetwork` / `ClusterUserDefinedNetwork` is **deprecated** when the sidecar ships; `updateDynamicUDNStatus` stops patching the primary CR. Per-node active count is derived by listing sidecars (`kubectl get networkrenderstates -l k8s.ovn.org/network=…`).

**Cardinality:** bounded by **active (node, network) pairs**, not nodes × all UDNs. Objects are created only when a network is active (or imminently active) on a node; **garbage collection (GC)** on teardown and node/network delete.

### (C)UDN status churn

These conditions live on `status.conditions[]` on the primary `UserDefinedNetwork` / `ClusterUserDefinedNetwork` (there is no `status.messages[]` shard array). They are **kept unchanged** because their etcd footprint on routine node churn is small compared with policy-like per-node SSA.

| Condition | Scope | Typical writer | Node-churn etcd risk |
|-----------|-------|----------------|----------------------|
| `NetworkCreated` | UDN/CUDN | UDN controller | **None** on the happy path—patches on NAD create/sync or spec errors only. |
| `TransportAccepted` | **CUDN only** | UDN controller (transport validation) | **None** on routine node churn—patches on transport/RA/VTEP config changes only. |
| `UplinksReady` | **CUDN only** | Uplink controller | **Low** on the happy path—the controller reconciles on node events, but the success message is static (`"Uplinks are ready for all active CUDN nodes"`) and `MergeStatusCondition` skips unchanged patches. Failure/recovery transitions may write. |
| `NetworkAllocationSucceeded` | UDN/CUDN | `NetworkClusterController` | **Very low** on the happy path—`updateNetworkStatus` runs after each node handler but **dedupes** per-node errors and patches the condition only when the **reported error node** changes; per-node detail goes to **Events**. |
| `NodesSelected` | dynamic UDN only | `NetworkClusterController` | **High**—patches the primary CR whenever the active-node **count** changes. **Deprecated**; replaced by the dynamic-UDN sidecar ([#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788)). |

**Why this is not policy-like churn:** policy-like resources scale as **nodes × policy objects** (each node writes its own SSA shard on the same CR). (C)UDN conditions are **one aggregated condition per network object**, with deduplication—so cost scales with the number of UDN/CUDN instances, not nodes × policies. The exception (`NodesSelected`) is the reason dynamic UDN moves to a sidecar.

### API Details

* **Policy-like CRDs:** when metrics mode is on, stop writing per-node shards; optional summary field remains. OpenAPI deprecation of unused shard fields is a follow-up (prefer **stop writing** first).
* **No new CRDs** for per-node success/error of EgressFirewall, EgressQoS, NetworkQoS, ANP/BANP, or APBExternalRoute.
* **New sidecar CRD** `NetworkRenderState` (or equivalent) for **dynamic UDN** active/render state only.
* Public documentation must state: policy-like resources (metrics + optional summary), (C)UDN primary status (unchanged), dynamic UDN sidecar (replaces `NodesSelected`).

### Implementation Details

#### Policy-like resources

| Area | Current pattern | Target (flag **on**, default) | Target (flag **off**) |
|------|-----------------|--------------------------------|------------------------|
| Admin Policy Based External Route | Per-node SSA `messages[]` + `StatusManager` rollup | Metrics; optional CM summary from Prometheus | Unchanged |
| Egress Firewall | Per-node SSA + `StatusManager` | Same | Unchanged |
| Admin and Baseline Admin Network Policy | Per-node `Ready-In-Zone-<node>` + node-delete cleanup | Metrics; optional CM summary | Unchanged |
| Egress QoS / Network QoS | Per-node `Ready-In-Zone-<node>` + `StatusManager` | Same | Unchanged |

Implementation steps:

* Add **`PolicySyncStatusViaMetrics`** (name TBD) flag, default **`true`**.
* Audit all `ApplyStatus` / `UpdateStatus` call paths for policy-like resources in per-node `ovnkube-controller` and `StatusManager`.
* When flag is **on:** gate out per-node SSA status writes; register metrics with documented names/labels; bypass `StatusManager` shard rollup.
* When flag is **off:** preserve today's code paths exactly.
* Implement **[metrics availability](#metrics-availability)** helper in cluster-manager (config + probe + cached state).
* Implement **CM Prometheus rollup worker:** periodic PromQL per policy type; patch summary only on change; no-op when flag off or Prometheus unavailable.
* Map every former status signal to a metric before enabling the default.
* One-time **managedFields** / stale shard cleanup on upgrade to metrics mode.
* Provide **Grafana** dashboard examples under `docs/` or the observability guide.

#### (C)UDN primary status

| Area | Current pattern | Target |
|------|-----------------|--------|
| Spec-driven conditions | `NetworkCreated` on UDN/CUDN; `TransportAccepted`, `UplinksReady` on **CUDN only** | **Unchanged** |
| `NetworkAllocationSucceeded` | Sparse; deduped on happy path | **Unchanged** |
| `NodesSelected` (dynamic UDN) | Patches primary CR on active-node count change | **Deprecated**; stop writing when sidecar ships |

No feature flag changes for (C)UDN primary status except removing `NodesSelected` writes after sidecar migration.

#### Dynamic UDN sidecar

| Area | Current pattern | Target |
|------|-----------------|--------|
| Active / render signal | `NodesSelected` + annotation heuristics (tunnel ID, subnets) | **`NetworkRenderState`** per (node, network); RA watches sidecars ([#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788)) |

Implementation steps:

* Define CRD (`NetworkRenderState` or name agreed in [#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788)): schema, RBAC, codegen, Helm/OLM manifests.
* **Per-node `ovnkube-controller`**: create/update/delete sidecar on active/render/teardown transitions; own `Active` and `Rendered` conditions.
* Remove `updateDynamicUDNStatus` / `NodesSelected` patches on primary (C)UDN.
* RA controller: list/watch sidecars; require `Active=True` **and** `Rendered=True` before advertising; drop tunnel-ID proxy for dynamic UDN L2.
* GC: delete sidecars on node delete, network delete, and after teardown grace.
* E2E: sidecar count tracks active pairs; primary CR does not patch on pod placement churn.

### Testing Details

* **Unit tests:** with flag **on**, per-node reconcile does **not** patch policy CR status; metrics increment correctly.
* **Unit tests:** with flag **off**, existing status behaviour unchanged (regression).
* **Unit tests:** metrics availability probe (available / unavailable / recovery).
* **Unit tests:** CM rollup patches summary only when PromQL result changes; no patch when Prometheus unavailable.
* **Unit tests:** dynamic-UDN sidecar lifecycle and RA consumer without tunnel-id annotations.
* **E2E / scale:** node churn does not increase policy CR status patch rate with default flag; sidecar count tracks rendered/active pairs, not every node event on primary CR.

### Documentation Details

* Feature flag: name, default, how to revert to legacy status.
* Metrics reference: names, labels, example PromQL for per-node debug and for CM rollup queries.
* Metrics availability: config knobs, behaviour when Prometheus is absent.
* Policy CR summary: staleness bounds, that metrics are the fresher source.
* Sidecar reference: schema, naming, labels, lifecycle, migration from `NodesSelected`.
* (C)UDN: which conditions stay on the primary object and why.

## Risks, Known Limitations and Mitigations

* **Limitation:** With default flag, policy CRs lose per-node detail on `.status`.  
  **Mitigation:** Metrics and dashboards; optional CM summary when Prometheus is available; flag to revert.

* **Limitation:** CM summary requires Prometheus when flag is on.  
  **Mitigation:** Central availability check; skip rollup gracefully; document that per-node debug uses metrics scrape targets directly.

* **Limitation:** CM summary is eventually consistent (scrape + query interval).  
  **Mitigation:** Document max staleness; metrics remain source of truth for debug.

* **Risk:** Operators grep `.status`, conditions, or managedFields for per-node health.  
  **Mitigation:** Release notes, migration guide, flag opt-out, metric name stability.

* **Risk:** High-cardinality metrics.  
  **Mitigation:** Bounded labels, optional detailed metrics behind a sub-flag, recording rules.

* **Risk:** Sidecar object count for dynamic UDN.  
  **Mitigation:** Sparse creation; GC on teardown; **not** used for static UDN or policy-like CRs.

* **Risk:** Disabling status writes before metrics are available leaves a visibility gap.  
  **Mitigation:** Checklist mapping each status field to a metric before default-on.

## OVN-Kubernetes Version Skew

To be set during implementation (target minor release TBD). Likely spans multiple PRs: feature flag and metrics first; CM rollup; dynamic-UDN sidecar coordinated with [#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788).

## Backwards Compatibility

* **Default-on flag** changes behaviour for policy-like CRs (no per-node shards; metrics + optional Prometheus summary).
* **`PolicySyncStatusViaMetrics=false`** restores today's per-node status writes and `StatusManager` rollup.
* **`NodesSelected`** deprecated on primary (C)UDN when sidecar ships; consumers switch to `NetworkRenderState` ([#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788)).
* **(C)UDN** (`NetworkCreated`, `NetworkAllocationSucceeded` on UDN/CUDN; `TransportAccepted`, `UplinksReady` on CUDN only): **no breaking change**.
* **Kubernetes:** prefer stop-writing before OpenAPI removal of deprecated fields.
* **Downgrade:** document mixed-version behaviour if older components still write per-node shards.

## Alternatives

**Selected combination:**

* **Option 1** (metrics) for policy-like per-node success/error, behind a **default-on feature flag** with opt-out to legacy status.
* **Option 4** (Prometheus periodic query → low-frequency CM summary) when metrics mode is on and Prometheus is available.
* **Option 2** (sidecar CR) for **dynamic UDN** active/render state only; deprecates `NodesSelected` on the primary CR.
* **(C)UDN primary status** unchanged except `NodesSelected` removal.

### 1. Metrics-only (selected for policy-like success/error)

Per-node `ovnkube-controller` instances **stop** per-node SSA status writes when the feature flag is on (default). Components export Prometheus metrics for per-node outcomes. Cluster-manager does not patch per-node shards on the primary CR.

| Pros | Cons |
|------|------|
| Eliminates status patch storms on node churn, so etcd does not grow from retained status revisions during scale-out or scale-in | No per-node signal on the CRD in metrics mode; `kubectl` cannot show per-node sync health |
| No new CRDs or managedFields shard cleanup on the primary object | **Requires Prometheus** (or compatible scrape/query stack) for per-node **debug** visibility; **`PolicySyncStatusViaMetrics=false`** restores legacy status |
| Fits high-cardinality data (node × resource) better than etcd | GitOps/CI that only watches CRD `.status` must adopt metrics, dashboards, [CM Prometheus rollup](#4-cluster-manager-writes-low-frequency-aggregated-status-selected-for-policy-summary), or flag opt-out |
| Matches CNI/datapath practice of keeping apply detail off high-churn policy objects (see prior art) | Metric cardinality must be designed carefully to avoid a different kind of scale problem |
| **Feature flag** (default on) allows revert without fork | Other controllers cannot watch metrics as a reliable control-plane API |

**Prior art** (named systems that keep high-churn apply/health detail **off** the policy object and use metrics or local debug instead):

* **Cilium** — CiliumEndpoint extra status PATCHes were disabled by default because they bottlenecked the apiserver ([cilium#10490](https://github.com/cilium/cilium/pull/10490)), then the `--endpoint-status` path was removed ([cilium#30761](https://github.com/cilium/cilium/pull/30761)). Users are directed to agent metrics (`cilium_endpoint_state`, `cilium_policy`, `cilium_controllers_failing`) and `cilium-dbg`.
* **kube-proxy** — `Service` has no per-node status. Datapath sync health is `/metrics` (`kubeproxy_sync_proxy_rules_*`) and healthz.
* **Kubernetes NetworkPolicy** — SIG-Network added a status subresource then **withdrew** it ([KEP-2943](https://github.com/kubernetes/enhancements/issues/2943), [kubernetes#115843](https://github.com/kubernetes/kubernetes/pull/115843)). Policies have no `.status`; CNIs do not write per-node apply results onto the policy.

These examples support **not** putting high-churn dataplane or policy-apply detail on API objects. They do **not** mean Kubernetes treats Prometheus as a general replacement for `.status`: [API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md) still put conditions on the object so other components can consume them without resource-specific knowledge.

**Not selected for:** dynamic UDN render state ([#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788))—controllers must watch kapi.

### 2. Separate status CRD (sidecar object per node and resource)

Publish sync or placement state on a dedicated CRD with a stable naming scheme (for example `<node_name>.<resource_name>` or `<resource_name>.<node_name>`). The per-node `ovnkube-controller` owns one object; cluster-manager watches it instead of patching the primary CR. For **dynamic UDN**, this OKEP uses the UplinkState pattern: `<network>.<node>` (see [Dynamic UDN sidecar](#dynamic-udn-sidecar)).

| Pros | Cons |
|------|------|
| Kubernetes-native visibility; `kubectl get` works per node; **other controllers can watch it** | **Object count** scales as `nodes × resources` if applied naively to every EgressFirewall, EgressQoS, etc. |
| Clear writer ownership (one field manager per sidecar object) | Watch fan-out and list cost grow with cluster size |
| Avoids SSA shard fights on the primary CR’s `.status` | Extra CRDs, RBAC, discovery, and lifecycle (garbage collection when node or policy is deleted) |
| Can be constrained to **sparse** creation (only on failure, or only for networks that render on a node)—see [Assume-success / errors-only status on the primary CR](#3-assume-success-errors-only-status-on-the-primary-cr) | Still etcd writes; dense success reporting for every namespaced policy or static UDN recreates the churn problem |

**Prior art:**

* **OVN-Kubernetes UplinkState** ([PR #6555](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6555)) — cluster-scoped sidecar named like `<uplink>.<node>` (for example `br-blue.node-a`). `ovnkube-node` publishes it; other controllers watch it.
* **kubernetes-nmstate `NodeNetworkConfigurationEnactment`** — one object per **(node, policy)**, named `<node>.<policy>`, carrying apply status for a cluster-scoped `NodeNetworkConfigurationPolicy`. Closest analog to “sidecar status for a policy on a node.” (`NodeNetworkState` is a different object: one per node for observed NIC state, not per policy.)
* **OpenShift `SriovNetworkNodeState`** — one object per node, named after the node, for discovered SR-IOV hardware and sync status.
* **OpenShift `MachineConfigNode`** — per-node sidecar; `MachineConfigPool` aggregates counts from those objects rather than stuffing every node into the pool spec object.
* **Kubernetes Node conditions** — per-node health lives on the `Node` object, not on every namespaced workload.

**Selected** for **dynamic UDN** `NetworkRenderState` (low-cardinality **network × node**; replaces `NodesSelected` and supplies `Rendered`; consumed by other controllers such as route advertisements). **Rejected** for policy-like CRs and **static** UDN: a sidecar per (node, policy) pair trades patch churn for object cardinality.

### 3. Assume-success / errors-only status on the primary CR

Do not report success per node. Variants include:

* **(a)** Condition stays `True` until any node fails; only patch when the failure set changes.
* **(b)** Patch `.status` only when `handlerErr != nil`; never write on successful node sync.
* **(c)** Errors-only on a sidecar CR (combine with [separate status CRD (sidecar)](#2-separate-status-crd-sidecar-object-per-node-and-resource)).

| Pros | Cons |
|------|------|
| Large reduction in writes on the happy path (node additions are often silent) | **Ambiguity:** `True` vs “not yet reconciled” vs “all nodes synced” |
| Simple for operators who only care about “Is there any failure?” | Stale failure state if a node is deleted without cleanup |
| (b) avoids success-path etcd traffic entirely | Per-node SSA shards may still need cleanup; does not by itself stop cluster-manager from reconciling on every node event |
| (c) keeps primary CR clean | Failure path still writes; dense failure storms during incidents |

**Prior art:**

* **Kubernetes NetworkPolicy** (after KEP-2943 withdrawal) — no status field; the object is assumed applied. Closest to “assume success.”
* **CiliumEndpoint** extra status default-off / removed — operators assume the endpoint is healthy unless metrics or `cilium-dbg` say otherwise.
* **cert-manager `Certificate` `Ready`** and **Gateway API `Accepted` / `Programmed`** — **not** errors-only. They always maintain coarse conditions on the **primary** object (positive polarity is expected to be set). That is a **low-cardinality summary on the CR**, closer to variant (a) or [CM Prometheus rollup](#4-cluster-manager-writes-low-frequency-aggregated-status-selected-for-policy-summary) than to sparse per-node error writes.

**Not selected** as the sole per-node solution: success-path silence does not give operators positive confirmation per node, and (a)/(b) still need care so cluster-manager does not patch on every node event. Variant (a) overlaps the **allowed** low-frequency [CM Prometheus rollup](#4-cluster-manager-writes-low-frequency-aggregated-status-selected-for-policy-summary). Variant (c) overlaps the [dynamic UDN sidecar](#dynamic-udn-sidecar) if objects are created only on failure—which is the wrong default for render state route advertisements must see on the happy path.

### 4. Cluster-manager writes low-frequency aggregated status (selected for policy summary)

Hybrid: per-node `ovnkube-controller` instances emit metrics ([Metrics-only](#1-metrics-only-selected-for-policy-like-successerror)) and stop per-node SSA shards on the primary CR. Cluster-manager periodically patches a **single** summary on the primary CR when a **Prometheus query** says the aggregate changed.

This OKEP selects **Prometheus scrape into cluster-manager** as the rollup transport when metrics mode is on and Prometheus is available (see [CM rollup (Prometheus scrape)](#cm-rollup-prometheus-scrape)). The rollup is **optional** and **skipped** without Prometheus; per-node debug still uses metrics endpoints directly. When **`PolicySyncStatusViaMetrics=false`**, today's **`StatusManager`** path (reading per-node shards from the primary CR) applies instead—no Prometheus rollup.

| Transport | Selected? | Notes |
|-----------|-----------|-------|
| **Prometheus scrape into CM** | **Yes** | Periodic PromQL; patch only on diff; requires [metrics availability](#metrics-availability); documented staleness |
| **Kapi sidecar + informer** | No (for policy summary) | Durable but adds CRDs; better fit for [#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788) render state |
| **RPC/HTTP push** | No | Fragile without retry/HA |

| Pros | Cons |
|------|------|
| Restores a **kapi-level** summary for `kubectl` / GitOps; other controllers and humans can read the object | Still writes to etcd (much lower rate than today's per-node-event rollup, but not zero) |
| Decouples per-node churn from per-node status patches | Requires Prometheus for rollup when metrics mode is on; summary can **lag** live metrics (scrape + query interval) until the next query |
| Can be tuned (periodic PromQL interval; patch only when the aggregate changes) | Implementation complexity versus [Metrics-only](#1-metrics-only-selected-for-policy-like-successerror) with no summary on the CR; uncommon scrape-then-status pattern |
| Distinct from today's "patch on every node event" rollup; reuses [Metrics-only](#1-metrics-only-selected-for-policy-like-successerror) metrics—no second node→CM transport; single CM writer on the summary field | No per-node detail on primary CR; compared with metrics-only alone, a delayed kapi summary is still better than none |

**Prior art:**

* No widely used Kubernetes controller was found that **scrapes Prometheus** and then writes CRD `.status`. The common direction is the reverse (kube-state-metrics reads objects and **exports** metrics). This OKEP **selects** this unusual scrape-then-status path for an **optional** coarse summary when metrics mode is on and Prometheus is available; staleness must be documented.
* **OpenShift `MachineConfigPool`** aggregates `updatedMachineCount` / `degradedMachineCount` from per-node `MachineConfigNode` (or Node) objects—kapi sidecars plus a low-frequency rollup, not Prometheus. That is the [separate status CRD (sidecar)](#2-separate-status-crd-sidecar-object-per-node-and-resource) + summary model; it is a reasonable analog but **not** the selected transport for policy-like summaries here (would reintroduce dense sidecars for every policy).

**Selected** for optional coarse summary when metrics mode is on. Putting operational truth on the primary object is desirable. Do **not** implement it as “reconcile `StatusManager` on every node informer event.”

### 5. Cluster-manager aggregated status only (drop per-node SSA, no new input)

Per-node `ovnkube-controller` instances stop patching; cluster-manager alone rolls up using today's `StatusManager` reading shards on the primary CR, still driven by **node informers**.

| Pros | Cons |
|------|------|
| Single writer on `.status`; simpler managedFields | **Broken once per-node SSA stops:** no shard source on the primary CR |
| Stops per-node SSA patches from `ovnkube-controller` | Rollup still patches on every node event if driven by informers, so etcd revision growth remains |

**Rejected.** This is today's cluster-manager path without per-node SSA shards, but with no replacement input once shards are gone. A change-driven or periodic summary fed by metrics ([CM Prometheus rollup](#4-cluster-manager-writes-low-frequency-aggregated-status-selected-for-policy-summary)) is the version that remains viable—or operators disable the feature flag for legacy behaviour.

### 6. Rate-limit or coalesce status patches

Batch or throttle `ApplyStatus` calls instead of removing them.

| Pros | Cons |
|------|------|
| Smaller implementation change than full removal | Still writes to etcd; delays visibility; harder to reason about “last known good” state |
| Reduces peak apiserver QPS | Does not bound total revision growth under sustained churn |

**Rejected.** Coalescing or throttling reduces peak apiserver QPS but still writes to etcd on node churn, delays visibility, and does not bound revision growth under sustained autoscaling—the problem this OKEP targets. [Metrics-only](#1-metrics-only-selected-for-policy-like-successerror) with optional [CM Prometheus rollup](#4-cluster-manager-writes-low-frequency-aggregated-status-selected-for-policy-summary) addresses the root cause instead.

### 7. Lease or ConfigMap sidecar status

Store per-node sync detail in Leases or ConfigMaps keyed by node/resource.

| Pros | Cons |
|------|------|
| Avoids patching primary CR `.status` | Non-standard; poor UX vs CRD conditions |
| TTL on Leases can garbage-collect stale entries | Still apiserver objects and watches; not idiomatic for policy health |

**Rejected** in favor of [Metrics-only](#1-metrics-only-selected-for-policy-like-successerror) for policy success/error and a typed [separate status CRD (sidecar)](#2-separate-status-crd-sidecar-object-per-node-and-resource) for dynamic UDN (see [Dynamic UDN sidecar](#dynamic-udn-sidecar)).

## References

* [#6414 - Remove per node status update for resources](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6414)
* [#6788 - Per-node status that a (C)UDN is rendered, for cluster-manager consumers](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788)
* OVN-Kubernetes cluster-manager status manager: `go-controller/pkg/clustermanager/status_manager/status_manager.go`
* Cluster-manager node tracking (`zone_tracker`): `go-controller/pkg/clustermanager/status_manager/zone_tracker/zone_tracker.go`
* UDN status: `go-controller/pkg/clustermanager/network_cluster_controller.go` (`updateNetworkStatus`, `updateDynamicUDNStatus`)
* UDN controller (spec-driven status): `go-controller/pkg/clustermanager/userdefinednetwork/controller.go`
* Route advertisements (render proxy TODO): `go-controller/pkg/clustermanager/routeadvertisements/controller.go`
* Kubernetes API conventions on conditions: [api-conventions.md](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md)
* [Option 1](#1-metrics-only-selected-for-policy-like-successerror): [Cilium endpoint-status removal](https://github.com/cilium/cilium/pull/30761); [Cilium CEP status disabled by default](https://github.com/cilium/cilium/pull/10490); kube-proxy `/metrics`; [KEP-2943 NetworkPolicy status withdrawn](https://github.com/kubernetes/enhancements/issues/2943)
* [Option 2](#2-separate-status-crd-sidecar-object-per-node-and-resource): OVN-Kubernetes UplinkState ([PR #6555](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6555)); [kubernetes-nmstate enactments](https://nmstate.github.io/kubernetes-nmstate/user-guide/102-configuration.html); [SriovNetworkNodeState](https://github.com/openshift/sriov-network-operator/blob/main/doc/api/node-state-api.md); OpenShift [MachineConfigNode](https://github.com/openshift/machine-config-operator/pull/4012)
* [Option 3](#3-assume-success-errors-only-status-on-the-primary-cr): NetworkPolicy (no status); cert-manager `Certificate` `Ready`; Gateway API `Accepted` / `Programmed` ([GEP-1364](https://gateway-api.sigs.k8s.io/geps/gep-1364/))
* [Option 4](#4-cluster-manager-writes-low-frequency-aggregated-status-selected-for-policy-summary): MachineConfigPool rollup from per-node objects as the informer analog; Prometheus scrape-then-status as this OKEP's chosen summary transport
