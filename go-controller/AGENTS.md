# AGENTS.md — go-controller

Main Go codebase for OVN-Kubernetes. See the root
[AGENTS.md](../AGENTS.md) for project overview, feature list, repository
layout, build/test commands, contribution conventions, and gateway modes.

## Binaries

Built via `make build` from this directory:

| Binary | Description |
|--------|-------------|
| `ovnkube` | Main binary — runs as cluster-manager, ovnkube-controller, or ovnkube-node |
| `ovn-k8s-cni-overlay` | CNI plugin executable invoked by the container runtime |
| `ovnkube-identity` | Certificate-based node identity and admission webhook |
| `ovnkube-trace` | Trace packet paths through OVN and OVS logical flows |
| `ovnkube-observ` | Observability and flow sampling |
| `ovn-kube-util` | CLI utilities (readiness probes, etc.) |
| `hybrid-overlay-node` | Hybrid overlay agent for Windows/Linux VXLAN clusters |

## Component Architecture

The `ovnkube` binary runs as three distinct components: **ovnkube-cluster-manager**,
**ovnkube-controller**, and **ovnkube-node**. See
[`docs/design/architecture.md`](../docs/design/architecture.md) for what each
component does, which pods/containers they run in, and their database access.

The key distinction for code navigation: only ovnkube-controller has direct
access to OVN NB/SB databases (via libovsdb). The cluster-manager interacts
with the K8s API only. ovnkube-node interacts with the K8s API, OVS bridges,
and host networking (nftables, VRF, routes, interfaces) but does not access
OVN databases directly.

Note: ovnkube-controller and ovnkube-node both run on every node as part of
the data plane and could logically be a single process. They are separate
today for historical reasons — in the former centralized architecture,
ovnkube-controller ran as part of ovnkube-master on control-plane nodes.
They have not yet been combined.

See [`pkg/AGENTS.md`](pkg/AGENTS.md) for which packages implement each
component.

## Directory Layout

| Directory | Description |
|-----------|-------------|
| `cmd/` | Binary entrypoints (see Binaries above) |
| `pkg/` | All library packages — see [`pkg/AGENTS.md`](pkg/AGENTS.md) |
| `etc/` | Default configuration file (`ovn_k8s.conf`) installed to `/etc/openvswitch/` |
| `hack/` | Build scripts, codegen, modelgen, linting, formatting |
| `hybrid-overlay/` | Windows/Linux VXLAN hybrid overlay (separate cmd + pkg tree) |
| `observability-lib/` | Standalone library for OVS flow sample parsing |
| `vendor/` | Vendored Go dependencies |

## Codegen, Modelgen, and Mocksgen

Three `make` targets produce generated code. Do not hand-edit generated files;
regenerate instead. See
[`docs/developer-guide/developer.md`](../docs/developer-guide/developer.md)
for details on all three.

| Target | What it generates | Output |
|--------|-------------------|--------|
| `make modelgen` | Typed Go models from OVN/OVS `.ovsschema` files | `pkg/nbdb/`, `pkg/sbdb/`, `pkg/vswitchd/` |
| `make codegen` | CRD clients, informers, listers from CRD type definitions | `pkg/crd/*/` |
| `make mocksgen` | Mock interfaces via mockery | Various `mocks/` directories |

## Go Code Conventions

Follow [Effective Go](https://go.dev/doc/effective_go) and the
[Go Code Review Comments](https://go.dev/wiki/CodeReviewComments).
Project-specific additions:

- **Logging** — use klog with appropriate verbosity levels. Use matching
  severity (klog.Errorf for errors, not klog.Infof).
- **Constants** — no hardcoded values. Use constants or config.
- **Thread safety** — shared state must be protected. Scrutinize lock
  ordering and flag unprotected concurrent map/slice access.
- **Error wrapping** — wrap errors with context using
  `fmt.Errorf("...: %w", err)` where appropriate. Don't wrap when the
  error already carries sufficient context. Use `%w` only when callers
  need `errors.Is`/`errors.As`; use `%v` when exposing the unwrapped
  type would leak implementation details.
- **Error shadowing** — avoid `err :=` inside nested blocks that shadow
  an outer `err` — common source of silently lost errors.
- **Nil checks** — check OVS/OVN DB results, optional K8s fields, and
  interface type assertions. Do not add redundant defensive nil checks
  where the value is guaranteed non-nil by the call chain or API contract.
- **Resource leaks** — close channels, namespace handles, informer
  factories. Goroutines must have shutdown paths.
- **Cleanup paths** — continue on partial failures, collect errors with
  `kerrors.NewAggregate` or `errors.Join`, don't bail on first error.
- **Dual-stack** — always consider IPv4+IPv6. Flag changes that only
  handle single-stack.
- **time.Duration** — use explicit units (`10*time.Second`, not `10`).
- **Shared helpers** — utility functions belong in `pkg/util/` or the
  relevant package's utils. Don't duplicate existing helpers.
- **Comments** — comments should explain *why*, not *what*. Don't restate
  what the code does (e.g. `// return the error` above `return err`,
  `// create the pod` above `CreatePod()`). Only non-obvious intent,
  trade-offs, or constraints warrant a comment.
- **Doc comments** — exported functions, types, and methods must have
  godoc comments starting with the identifier name
  (e.g. `// FuncName does ...`).
