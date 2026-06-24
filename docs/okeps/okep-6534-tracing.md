# OKEP-6534: Distributed Tracing Support for OVN-Kubernetes

* Issue: [#6534](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6534)

## Problem Statement

OVN-Kubernetes currently does not provide tracing for the operations it performs. This limits
visibility into how OVN-Kubernetes handles a resource after it receives a Kubernetes event.

Pod creation is the first path to trace because it crosses several asynchronous Kubernetes and
OVN-Kubernetes components. When a pod is slow to become ready, or fails before networking is
complete, operators can usually inspect events, logs, and metrics from individual components, but
they cannot follow the same pod networking operation through OVN-Kubernetes in a trace.

The initial tracing work should therefore focus on the OVN-Kubernetes operations that are directly
part of pod creation:

* allocating pod IPs and producing pod network annotations
* programming OVN logical resources for the pod
* applying namespace, gateway, SNAT, and related pod networking state
* handling CNI ADD on the node and configuring the pod interface

## Goals

* Add opt-in distributed tracing support to OVN-Kubernetes, gated by a feature gate.
* Add initial tracing coverage for pod creation networking operations, including pod IP
  allocation, OVN logical resource programming, and pod interface configuration.
* Support propagated trace context from Kubernetes object annotations so OVN-Kubernetes spans can
  be correlated with upstream Kubernetes work through OpenTelemetry span links.
* Make tracing configuration centrally manageable via `ovnkube-config` ConfigMap.
* Make propagated context annotation key configurable, defaulting to
  `tracing.k8s.io/traceparent`.

## Non-Goals

* Full tracing coverage for all OVN-Kubernetes resources in this initial implementation.
* Initial tracing support for NAD lifecycle operations.
* Defining backend-specific visualization requirements (Grafana/Tempo/Jaeger behavior).
* Replacing existing logs, events, or metrics.

## Introduction

Tracing support should be added as a general OVN-Kubernetes observability capability, not as a
one-off debug path for a single code branch. The initial implementation focuses on pod creation
because that is the most visible workflow where OVN-Kubernetes latency or failures directly affect
users.

Kubernetes components may propagate trace context through object annotations for asynchronous
workflows. OVN-Kubernetes is a reconciler: its work is triggered by resource events and is
decoupled in time from upstream components such as the API server, scheduler, and kubelet.
When propagated context is present, OVN-Kubernetes should attach that context as a span link on
a new reconcile root span so backends can correlate pod networking work with the upstream trigger
without modeling an incorrect synchronous parent-child relationship.

The design is intentionally incremental:

* provide immediate operational value for pod networking troubleshooting
* minimize risk by keeping tracing behind an opt-in config flag
* establish reusable tracing primitives and configuration for future expansion

## User-Stories/Use-Cases

### Story 1: Troubleshoot Pod Network Bring-Up

As a cluster operator, I want spans for OVN-Kubernetes pod creation work so I can identify whether
latency or failure happened during IP allocation, OVN logical programming, or CNI interface setup.

### Story 2: Correlate Pod Creation Across Kubernetes Components

As a cluster operator, I want OVN-Kubernetes reconcile spans to link to propagated trace
context from upstream Kubernetes components so I can correlate pod networking work with the
API server, scheduler, kubelet, and OVN-Kubernetes. Each component emits its own trace; span
links preserve the causal relationship without requiring a single nested trace view.

### Story 3: Enable Tracing Deliberately

As a platform administrator, I want tracing to be disabled by default and configured in
`ovnkube-config` so I can enable it only in clusters where a collector and sampling policy are
ready.

## Proposed Solution

This proposal adds an opt-in OpenTelemetry-based tracing path to OVN-Kubernetes. When enabled,
ovnkube-cluster-manager, ovnkube-node, and ovnkube CNI emit spans for selected pod
creation operations and export them to the configured OTLP endpoint.

The implementation uses `ovnkube-config` for tracing settings and reads propagated trace context
from Pod annotations when available. Invalid or missing propagated context does not affect pod
networking; OVN-Kubernetes falls back to starting a local root span without upstream links.

### API Details

No Kubernetes CRD API schema changes are required for this initial feature.

Configuration is introduced through the existing OVN-Kubernetes config surfaces:

* opt-in config flag in `[ovnkubernetesfeature]`
* `ovnkube-config` ConfigMap

### Feature Flag

Tracing is gated by a new OVN-Kubernetes config flag in `[ovnkubernetesfeature]`:

* `enable-tracing=false`: existing behavior; no tracing exporter initialization, no spans
  emitted.
* `enable-tracing=true`: tracing initialization and span emission enabled, subject to runtime
  config validity.

In `ovnkube-config`, this is configured in the existing feature section:

```ini
[ovnkubernetesfeature]
enable-tracing=true
```

### Tracing Configuration (`ovnkube-config`)

Add a `[tracing]` section under `ovnkube-config` for OpenTelemetry exporter and runtime
settings.

```ini
[tracing]
endpoint=127.0.0.1:4317
insecure=true
service-name=ovn-kubernetes
sampling-rate=1.0
propagated-context-annotation-key=tracing.k8s.io/traceparent
export-timeout=10
batch-timeout=5
max-export-batch-size=512
max-queue-size=2048
```

Initial fields:

* `endpoint`: OTLP gRPC endpoint for the exporter. Required when tracing is enabled.
* `insecure`: boolean for OTLP transport mode.
* `service-name`: OpenTelemetry `service.name`; defaults to `ovn-kubernetes`.
* `sampling-rate`: sampling ratio for new root spans, in range `[0.0, 1.0]`.
* `propagated-context-annotation-key`: Pod annotation key used for propagated context.
* `export-timeout`: exporter timeout, in seconds.
* `batch-timeout`: batch span processor timeout, in seconds.
* `max-export-batch-size`: maximum number of spans per export batch.
* `max-queue-size`: maximum queued spans before export.

Validation and fallback:

* If tracing is enabled and `endpoint` is empty, configuration is rejected.
* `sampling-rate` must be in range `[0.0, 1.0]`.
* Missing optional values fall back to defaults.
* Missing annotation key config falls back to `tracing.k8s.io/traceparent`.

### Propagated Context Semantics

OVN-Kubernetes consumes propagated trace context from Pod annotations. OVN-Kubernetes is an
asynchronous reconciler: controller and CNI work happen later and independently from upstream
Kubernetes requests. For that reason this OKEP follows the Kubernetes async trace context
propagation model and uses OpenTelemetry span links rather than remote parent-child
continuation.

The default annotation key is:

* `tracing.k8s.io/traceparent`

The annotation value uses the W3C `traceparent` format:

```text
00-<32hex trace id>-<16hex span id>-<2hex flags>
```

When the annotation exists and is valid, OVN-Kubernetes:

1. Parses trace ID, span ID, and trace flags from the annotation.
2. Build a remote `SpanContext` for the propagated upstream span.
3. Start a new local root span for the reconcile or CNI operation.
4. Attach the propagated context to that root span as an OpenTelemetry span link.

Nested OVN-Kubernetes work started during that operation uses normal context propagation and
becomes child spans of the local root span.

The resulting reconcile root span:

* has its own trace ID and span ID generated by OVN-Kubernetes
* is not a child of the upstream span referenced by the annotation
* includes a span link to the upstream trace/span ID from the annotation

When the annotation is missing or invalid, OVN-Kubernetes does not fail pod processing. It
starts a local root span for the operation without upstream links.

Each event-driven reconcile invocation starts a new root span. Retries and later updates on the
same Pod start new root spans and may link to the then-current annotation value. Startup sync
over already-existing Pods does not emit reconcile spans or consume propagated context.

OVN-Kubernetes has separate async entry points for control-plane reconcile work and node CNI
handling. Each entry point follows the same pattern: local root span, optional link to the Pod
annotation, child spans for nested work. Controller reconcile and CNI ADD/DEL are not modeled as
parent and child of each other unless one synchronously invokes the other in-process.

This aligns with [KEP-5915: Standardizing Async Trace Context Propagation][k8s-async-trace-kep],
which stores W3C Trace Context in object annotations and uses span links at asynchronous
boundaries to preserve causal relationships without implying synchronous dependency.

### Initial Instrumentation Scope

Initial instrumentation is limited to pod networking creation/update critical path:

* ovnkube-cluster-manager pod add/update/delete handling entry spans
* ovnkube-controller pod add/update/delete handling entry spans (default network and user-defined networks)
* logical port programming sub-operations
* namespace/address set related pod network setup
* gateway/SNAT routing operations for pod
* NBDB transaction operation for pod programming
* ovnkube-node CNI ADD/DEL handling spans and key sub-operations

Tracing is intended for new source events that represent active pod lifecycle work, such as pod
add, update, and delete events received by OVN-Kubernetes controllers. Startup sync over resources
that already exist in the API server is not traced as pod creation work. This avoids adding new
spans to stale propagated contexts and avoids producing misleading traces after a controller
restart.

NAD-specific lifecycle tracing is excluded from this initial OKEP.

## Implementation Details

### Components

* `pkg/tracing`:
  * tracer provider initialization/shutdown
  * propagated context extraction helper from pod annotations
  * reconcile/CNI root span helper that optionally attaches upstream span links
  * common attribute helpers
* ovnkube-cluster-manager pod path:
  * start pod add/update/delete handling root spans for cluster-wide pod networking decisions
  * link to propagated context from Pod annotations when valid
  * emit child spans for pod IP allocation on layer2 secondary networks
  * do not emit per-pod spans while syncing already-existing pods during startup
* ovnkube-controller pod path:
  * start reconcile root span for pod add/update resource handling
  * link to propagated context from Pod annotations when valid
  * pass context through nested pod handling functions
  * emit child spans for major sub-operations
* ovnkube-node/CNI path:
  * start CNI command root spans
  * link to propagated context from Pod annotations when valid
  * emit child spans for execution and marshal/transact steps

### Context Propagation Flow (Initial)

For asynchronous pod creation flows, an upstream component can write trace context to the Pod
annotation configured in `ovnkube-config`. OVN-Kubernetes pod handlers read that annotation when
they process the Pod.

For each event-driven reconcile or CNI invocation:

1. Start a new local root span for the OVN-Kubernetes operation.
2. If the annotation is valid, add a span link to the propagated upstream span context.
3. Pass the root span context through nested functions so sub-operations emit child spans.

If the annotation is missing or invalid, OVN-Kubernetes does not reject or delay pod processing.
The handler starts a local root span without upstream links and continues normally.

Initial sync after controller startup is treated as controller recovery, not as a new Kubernetes
resource operation. During this path OVN-Kubernetes reconciles already-existing objects from the
API server without emitting per-resource operation spans or consuming propagated context.

### Failure Handling

Tracing must not affect pod networking correctness. Failures in the tracing path are handled as
observability failures, not datapath failures.

* Invalid or unsupported propagated context: ignore the annotation, start a local root span
  without upstream links for that operation, and log enough detail for debugging without
  blocking pod processing.
* Invalid tracing configuration: reject startup when tracing is enabled and required fields are
  missing, or when values such as `sampling-rate` are invalid.
* Exporter or collector unavailable after startup: do not fail pod networking operations; rely on
  OpenTelemetry batching and export timeout behavior, and log exporter errors at an appropriate
  rate to avoid log spam.

### Performance Considerations

* Tracing is disabled by default.
* Sampling and batch settings are configurable in `ovnkube-config`.
* Production users are expected to tune sampling rates for scale.

### Span Naming Rules

Span names follow a dot-separated template:

```text
{service.component}.{subsystem}.{resource}.{operation}
```

`{service.component}` identifies the components that emits the span. It is set
once at startup via OpenTelemetry `service.component` and reused as the first segment of every
span name in that process.

| Component | Description |
|-----------|---------|
| `ovnkube-cluster-manager` | cluster-manager container in ovnkube-control-plane Pod|
| `ovnkube-node` | ovnkube-node process (network controller, node handlers, and CNI server) |
| `ovnkube-cni` | `ovn-k8s-cni-overlay` CNI plugin binary only |

CNI spans emitted by the in-process CNI server inside ovnkube-node use
`service.component=ovnkube-node` with `{subsystem}=cni`. The standalone
`ovn-k8s-cni-overlay` plugin, when instrumented separately, uses
`service.component=ovnkube-cni`.

`{subsystem}` names the logical controller or handler within the process. Pod networking
reconcile spans for both the default network and all user-defined network (UDN) topologies
(layer2, layer3, localnet) use `{subsystem}=network-controller`. Other examples include
`egressip` and `cni`.

`{resource}` is the Kubernetes/API resource kind in lowercase singular form, such as `pod` or
`node`. Network identity (default vs UDN, topology, primary/secondary) is carried in span
attributes rather than the span name:

* `ovn.network.name`
* `ovn.network.topology`
* `ovn.network.primary`
* `ovn.network.user_defined`
* `k8s.nad` (when applicable)

`{operation}` describes the work being traced:

* **Event-driven root spans** for network-controller pod handlers use plain Kubernetes
  lifecycle verbs: `add`, `update`, `delete`. Do not prefix these with `event.`.
* **Child spans** under a root reconcile span name the sub-step being executed, such as
  `setup-logical-network`, `allocate-network`, or `add-logical-port`.
* **CNI plugin** (`ovn-k8s-cni-overlay`): omit `{resource}` because kubelet invokes the
  plugin only for ADD/DEL. Use `{operation}` values `cmd.add` and `cmd.delete`.
* **CNI server** (in ovnkube-node): use `{subsystem}=cni`, `{resource}=pod`, and operations
  such as `add` / `delete` for the server-side handling path.
* **Retry queue**: when a reconcile is re-driven from the retry cache, the root span keeps the
  same `{operation}` (`add`, `update`, or `delete`) and records retry semantics via attributes
  (`retry.loop` on add, `retry.cache` on update) rather than encoding retry state in the span
  name.

Examples (with current `service.component` values):

```text
ovnkube-node.network-controller.pod.add
ovnkube-node.network-controller.pod.update
ovnkube-cluster-manager.network-controller.pod.allocate-network
ovnkube-node.cni.pod.add
ovnkube-cni.cmd.add
ovnkube-node.egressip.pod.event.add
```

## Testing Details

### Unit Testing

* Traceparent parser and validation tests:
  * valid format
  * invalid format
  * invalid hex fields
  * invalid lengths
* Context extraction helper tests:
  * missing annotation
  * configured key override
  * valid annotation yields upstream span context suitable for span links
* Reconcile root span tests for targeted functions:
  * valid annotation -> root span includes link to annotation trace/span ID
  * missing/invalid annotation -> root span without upstream links
  * nested operations -> child spans under the reconcile root span

### E2E Testing

* Feature flag disabled:
  * verify no tracing initialization and no span emission expectations
* Feature flag enabled + valid OTEL endpoint:
  * create pod with propagated annotation
  * verify OVN-Kubernetes pod-path spans are exported
  * verify exported reconcile root spans include links to the annotation trace/span ID
* Feature flag enabled + invalid annotation:
  * verify operation succeeds
  * verify fallback root span behavior without upstream links


## Documentation Details

* Add end-user docs for:
  * enabling `enable-tracing`
  * configuring `ovnkube-config` tracing block
  * annotation key behavior and defaults
  * span link semantics for propagated context
  * troubleshooting invalid context and exporter connectivity
* Update `mkdocs.yml` to include this OKEP path.

## Risks, Known Limitations and Mitigations

Risks:

* tracing overhead under high pod churn
* misconfigured exporter causing noisy logs
* inconsistent upstream annotation injection across components

Mitigations:

* feature flag disabled by default
* configurable sampling and batching
* graceful fallback when annotation or exporter path is invalid

Known limitations in this initial version:

* initial scope is pod path only
* NAD and other resources are not covered yet
* end-to-end correlation depends on upstream annotation propagation and backend support for span
  links

## OVN-Kubernetes Version Skew

Planned as an initial opt-in capability behind the `enable-tracing` config flag in the next
available release after merge. This is an OVN-Kubernetes configuration flag, not a Kubernetes
feature gate with alpha/beta graduation.

Skew behavior:

* upgraded components with tracing enabled can emit spans independently.
* mixed-version clusters may have partial span coverage.

## Backwards Compatibility

* Backward compatible by default (`enable-tracing=false`).
* No API breaking changes.
* No datapath behavior changes.

## Alternatives

* Hard-coded tracing config via environment variables only.
  * Not selected; ConfigMap-based configuration is more operationally manageable.
* Broad all-resource tracing in first iteration.
  * Not selected to reduce risk and scope.
* Remote parent-child continuation from propagated `traceparent`.
  * Not selected; OVN-Kubernetes reconcile and CNI work are asynchronous relative to upstream
    Kubernetes components. Span links preserve causal correlation without implying synchronous
    parent-child dependency. See [KEP-5915][k8s-async-trace-kep].

## References

* [KEP-5915: Standardizing Async Trace Context Propagation][k8s-async-trace-kep]

* OpenTelemetry Trace Context:
  * https://www.w3.org/TR/trace-context/
* OpenTelemetry tracing concepts:
  * https://opentelemetry.io/docs/concepts/signals/traces/
* OpenTelemetry span links:
  * https://opentelemetry.io/docs/concepts/signals/traces/#span-links

[k8s-async-trace-kep]: https://github.com/kubernetes/enhancements/issues/5915
