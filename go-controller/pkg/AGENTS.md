# AGENTS.md — go-controller/pkg

Package directory for the go-controller codebase. See
[`go-controller/AGENTS.md`](../AGENTS.md) for binaries, component
architecture, codegen, and code conventions.

Packages that implement a major component will have their own AGENTS.md
with deeper context.

## Network Topologies

OVN-Kubernetes supports multiple networks per cluster (User Defined
Networks), each with one of the following topologies:

- **L3 (layer3)** — the clustermanager carves per-node subnets (based on
  host prefix) from a larger CIDR (cluster CIDR for the default network,
  or the subnet in the UDN spec for User Defined Networks) and assigns
  them to nodes. The ovnkube-controller handles pod IPAM and creates
  OVN logical switches, routers, and a distributed transit switch so
  pods on different nodes can communicate. By default uses GENEVE
  encapsulation for cross-node traffic. With no-overlay/BGP mode,
  encapsulation is removed and pod subnets are advertised via BGP for
  direct routing between nodes.
- **L2 (layer2)** — a single flat broadcast domain stretched across all
  nodes via GENEVE tunnels. No per-node subnets; IPAM is cluster-wide
  (managed by clustermanager). Used for User Defined Networks.
- **Localnet** — connects pods directly to a pre-existing physical network
  via an OVS bridge mapping. No tunneling. Used for User Defined Networks
  that need direct external connectivity.

The cluster default network (CDN) is always L3. The overall flow for User
Defined Networks:

1. **clustermanager** watches `UserDefinedNetwork`/`ClusterUserDefinedNetwork`
   CRDs and generates corresponding `NetworkAttachmentDefinition` (NAD)
   resources with subnet allocations.
2. **networkmanager** detects the NAD and spins up topology-specific
   controllers in both ovnkube-controller and ovnkube-node.
3. **ovn/** (ovnkube-controller) reads the NAD and creates OVN logical
   switches, routers, ports, and policies for the network.
4. **node/** (ovnkube-node) reads the NAD and handles local bridge
   plumbing, VRF setup, gateway configuration, and nftables rules.
5. **cni/** (ovnkube-node) handles pod add/del — wires pod interfaces
   to the correct OVS bridge and network on pod scheduling.

## Core Component Packages

These implement the three `ovnkube` components (see
[Component Architecture](../AGENTS.md#component-architecture)):

| Package | Component | Description |
|---------|-----------|-------------|
| `ovn/` | ovnkube-controller | Core logic — pods, namespaces, services, network policies, egress features, UDN controllers (L2/L3/localnet), multicast, gateway, zone interconnect |
| `node/` | ovnkube-node | Gateway init, OVS bridge config, nftables/iptables, management port, DPU support, UDN isolation, VRF/link/route managers |
| `cni/` | ovnkube-node | CNI server and shim, OVS port setup, bandwidth shaping, DPU CNI |
| `clustermanager/` | ovnkube-cluster-manager | Subnet/IP allocation, UDN management, zone coordination, EgressIP/EgressService cluster controllers, DNS name resolver, BGP, no-overlay, VTEP, cluster network connect |
| `networkmanager/` | all | NAD controller, network controller lifecycle, pod tracker, EgressIP tracker |
| `controllermanager/` | ovnkube-controller, ovnkube-node | Top-level wiring: initializes default network controller and creates UDN controllers. `controller_manager.go` for ovnkube-controller, `node_controller_manager.go` for ovnkube-node |

## OVN/OVS Database Layer

| Package | Description |
|---------|-------------|
| `libovsdb/` | Typed libovsdb client, operations (ACL, LB, router, switch, port group, QoS, etc.), and utilities. All OVN DB interactions go through here. |
| `libovsdb/ops/` | CRUD helpers for specific OVN DB table types |
| `libovsdb/util/` | Higher-level OVN DB query and mutation utilities |
| `nbdb/` | **Auto-generated** OVN Northbound DB models — do not edit, regenerate with `make modelgen` |
| `sbdb/` | **Auto-generated** OVN Southbound DB models — do not edit, regenerate with `make modelgen` |
| `vswitchd/` | **Auto-generated** OVS DB models — do not edit, regenerate with `make modelgen` |

Rules for working with OVN/OVS databases:

- All NB/SB interactions go through the typed libovsdb client — do not exec
  raw OVS commands for DB operations.
- New ACLs and logical flows must use priorities defined as constants in
  `types/const.go` — don't conflict with existing rules unless it's ok to do so.
- Changes must respect network segmentation boundaries, especially with
  User Defined Networks.
- OVN DB schema changes must be backwards compatible or include migration.
- Resources created in OVN DB must be cleaned up on delete/update.
- Maintain referential integrity — rows that reference other rows (e.g.
  logical switch ports referencing port groups, load balancers on routers)
  must respect dependency ordering on create and reverse ordering on delete.
  Do not leave dangling references.

## CRDs

`crd/` contains CRD type definitions and generated clients/informers/listers
(regenerate with `make codegen`). See [`crd/AGENTS.md`](crd/AGENTS.md) for
details and coding standards.

## Allocators

`allocator/` provides resource allocation strategies:

| Sub-package | Description |
|-------------|-------------|
| `bitmap/` | Bitmap-based allocation |
| `deviceresource/` | Device resource allocation |
| `id/` | Numeric ID allocation |
| `ip/` | IP address allocation (subnet IPAM) |
| `mac/` | MAC address allocation |
| `pod/` | Pod-level allocation coordination |

## Infrastructure

Shared packages used across multiple components:

| Package | Description |
|---------|-------------|
| `config/` | Configuration parsing (CLI flags, config file, env vars) |
| `controller/` | Generic controller framework (watch + reconcile loop) |
| `controllers/` | Shared node reconciliation controller (used by ovn, controllermanager, clustermanager) |
| `factory/` | Kubernetes informer and watch factory |
| `informer/` | Informer utilities |
| `kube/` | Kubernetes client helpers and health checks |
| `retry/` | Retry logic for K8s and OVN operations |
| `types/` | Shared constants (port names, subnet ranges, priorities) and error types |
| `util/` | Shared utilities — node/pod/namespace annotations, batching, EgressIP helpers, error aggregation, NDP, UDN helpers. All annotation definitions and parsers live here. |
| `metrics/` | Prometheus metrics definitions and recorders |
| `generator/` | IP and UDN resource generators |
| `syncmap/` | Concurrent-safe typed map |
| `cryptorand/` | Crypto-safe random number generation |
| `tls/` | TLS configuration helpers |

## Feature-Specific

| Package | Description |
|---------|-------------|
| `kubevirt/` | KubeVirt VM live migration support and persistent networking |
| `persistentips/` | Persistent IP management for KubeVirt VMs |
| `observability/` | Observability library (flow tracing, sampling integration) |
| `ovnwebhook/` | Admission webhooks for OVN-Kubernetes resources |
| `csrapprover/` | CSR (Certificate Signing Request) approval for node identity |

## Testing

- Unit tests use Ginkgo suites (`_suite_test.go`) and live alongside the
  code as `*_test.go` files.
- See [`docs/developer-guide/developer.md`](../../docs/developer-guide/developer.md)
  for mock generation and the `.mockery.yaml` config.

## Test Helpers

| Package | Description |
|---------|-------------|
| `testing/` | Shared test utilities |
| `testing/libovsdb/` | Fake libovsdb client for unit tests |
| `testing/mocks/` | General mock implementations |
