# AGENTS.md — node

Node-local dataplane controller for OVN-Kubernetes. See
[`go-controller/pkg/AGENTS.md`](../AGENTS.md) for the package directory
overview and [`go-controller/AGENTS.md`](../../AGENTS.md) for component
architecture, codegen, and code conventions.

## Purpose

The `node` package implements the **ovnkube-node** component. It runs on
every node and is responsible for configuring local host networking so
that OVN-managed pod traffic can flow correctly:

- OVS bridge setup and OpenFlow flow management (br-int, br-ex).
- Gateway initialization — shared gateway (SGW) and local gateway (LGW)
  modes, including masquerade IPs, service routes, and NodePort/LB flows.
- Management port creation and configuration (ovn-k8s-mp0).
- nftables/iptables rules for service traffic, UDN isolation, PMTUD
  guarding, egress service SNAT, and forwarding policy.
- VRF setup and IP rule management for User Defined Networks.
- Netlink device lifecycle (bridges, VLANs, VXLANs, VRFs, dummy devices).
- DPU/SmartNIC support — DPU mode, DPU-host mode, and full mode.
- Node-side EgressIP plumbing on secondary host networks.
- Node-side EgressService nftables rules.
- EVPN VTEP and pod-neighbor management.
- Conntrack cleanup for stale service and external gateway entries.
- CNI server startup (delegated to `pkg/cni`).
- OVS CPU pinning integration via PodResources API.

ovnkube-node interacts with the Kubernetes API, OVS bridges, and host
networking (netlink, nftables, conntrack). It does not access OVN NB/SB
databases directly.

## Design Decisions

- **Why Init() is split from Start()** — `Init()` creates the OpenFlow
  manager and gateway structures. UDN node controllers (created by
  `controllermanager`) need a reference to the default network's
  OpenFlow manager before they can start. Splitting Init/Start allows
  the controller manager to wire up UDN controllers between the two
  phases.

- **Shared vs local gateway mode** — Two gateway modes exist because
  they serve different deployment needs. Shared gateway (SGW) keeps all
  traffic in the OVS datapath, enabling hardware offload to
  DPUs/SmartNICs. Local gateway (LGW) exits the OVS datapath to the
  host, which enables richer host-level policy (nftables) but is not
  offloadable. The mode is a cluster-wide configuration choice, not
  per-node.

- **Why nftables over iptables** — nftables replaced iptables as the
  primary packet-filter backend because it supports atomic rule
  replacement (transactions), is the upstream successor, and avoids
  iptables lock contention in high-churn environments. Legacy iptables
  helpers remain for older kernels that lack nftables support.

- **Declarative manager pattern** — The route, IP rule, link, and
  netlink device managers all follow a declarative desired-state
  pattern: controllers declare what should exist, and the manager
  reconciles actual state. This avoids ordering bugs between controllers
  that share the same kernel resources and makes cleanup reliable.

- **DPU mode split (full/dpu/dpu-host)** — A DPU deployment physically
  separates the SmartNIC (running OVS and representor ports) from the
  host (running the CNI and host networking). The `dpu` mode runs on
  the SmartNIC, `dpu-host` on the host, and `full` is the standard
  non-DPU mode. They share code via `BaseNodeNetworkController` but
  diverge in what kernel resources they manage.

- **Why conntrack cleanup is node-side** — Conntrack entries are
  kernel-local. Only the node itself can flush stale entries when
  service endpoints or external gateway routes change, so this logic
  lives in ovnkube-node rather than in the centralized controller.

- **Event-driven vs level-driven controllers** — The default node
  controller uses the older event-driven `pkg/retry` framework for
  namespace, endpoint-slice, and node watches. Newer node-side
  controllers (e.g. `controllers/egressip/`, `controllers/egressservice/`)
  use the level-driven `pkg/controller` framework, which reconciles
  from current informer-cache state instead of relying on event deltas.
  Level-driven controllers are preferred for new development because
  they are resilient to missed events and avoid the subtle race
  conditions inherent in event-driven designs.

## Key Types

| Type | Description |
|------|-------------|
| `CommonNodeNetworkControllerInfo` | Shared fields across all node network controllers: K8s client, watch factory, route manager, event recorder, node name. |
| `BaseNodeNetworkController` | Per-network base struct embedded by both default and UDN controllers. Holds network info, network manager, stop channel, and DPU connection tracking. |
| `DefaultNodeNetworkController` | Default-network node controller. Coordinates gateway, management port, link manager, CNI server, health checks, conntrack cleanup, PMTUD flows, and feature controllers. |
| `UserDefinedNodeNetworkController` | Per-UDN node controller. Handles primary UDN gateway programming and DPU pod wiring. |
| `Gateway` | Interface for gateway lifecycle — service/endpoint/EgressIP event handling and reconciliation. |
| `UserDefinedNetworkGateway` | UDN-specific gateway implementing VRF setup, IP rules, and OpenFlow plumbing for primary UDNs. |
| `UDNHostIsolationManager` | Manages nftables rules that isolate UDN pod traffic from the host network. |

## Lifecycle

### Default Network Controller

```text
NewDefaultNodeNetworkController(cnnci, networkManager, ovsClient)
  → creates link manager, masquerade reconciler, UDN isolation manager
  → initializes retry frameworks for namespaces, endpoint slices, nodes

Init(ctx)
  → configures global forwarding (IPv6)
  → bootstraps OVS flows
  → waits for node subnet annotation from ovnkube-controller
  → creates CNI server
  → creates management port controller
  → initializes gateway (shared/local mode or DPU-host)
  → sets node zone annotation

Start(ctx)
  → completes gateway initialization
  → starts gateway, management port controller
  → starts EgressIP health check server
  → starts namespace/endpoint-slice/node watches (conntrack, PMTUD)
  → starts CNI server, writes CNI config
  → starts EgressService, EgressIP, APBRoute controllers
  → starts link manager, OVS CPU pinning

Stop()
  → closes stop channel, waits for goroutines
```

### User Defined Network Controller

```text
NewUserDefinedNodeNetworkController(cnnci, netInfo, ...)
  → creates UDN gateway for primary networks

Start(ctx)
  → starts DPU pod handler (if DPU mode)
  → starts UDN gateway (VRF, IP rules, OpenFlow)

Stop()           — stops goroutines
Cleanup()        — removes OVS ports and gateway resources
Reconcile(info)  — reconciles gateway on network info changes
```

## Operating Modes

The node controller supports three operating modes via
`config.OvnKubeNode.Mode`:

| Mode | Description |
|------|-------------|
| `full` | Standard mode — full OVS + host networking + CNI. |
| `dpu` | Runs on the DPU/SmartNIC. Programs OVS representor ports, no host nftables. |
| `dpu-host` | Runs on the host paired with a DPU. Manages CNI, nftables, management port annotation export. No direct OVN/OVS bridge access. |

## Sub-packages

| Sub-package | Description |
|-------------|-------------|
| `managementport/` | Management port abstraction (`Interface`, `Controller`) and implementations for internal-port and representor-based management ports. |
| `routemanager/` | Declarative route ownership and reconciliation. Ensures desired routes exist and removes stale ones. |
| `iprulemanager/` | Declarative Linux IP rule management for VRF-based routing in UDN primary networks. |
| `iptables/` | Legacy iptables helpers for modes where nftables is not available. |
| `nftables/` | nftables table/chain/set management for OVN-Kubernetes node rules (PMTUD, masquerade, UDN isolation, egress service, forwarding). |
| `linkmanager/` | Tracks link address changes and reconciles MAC/IP addresses on node interfaces. |
| `netlinkdevicemanager/` | Async desired-state device manager for netlink devices (bridges, VRFs, VLANs, VXLANs, dummy). Explicitly decouples desired state from reconciliation. |
| `bridgeconfig/` | OVS bridge and per-network bridge state abstraction. |
| `vrfmanager/` | VRF device lifecycle management for UDN primary networks. |
| `ovspinning/` | OVS PMD thread CPU pinning via Kubernetes PodResources API. |
| `controllers/egressip/` | Node-side EgressIP plumbing for secondary host networks (IP assignment, routing rules). |
| `controllers/egressservice/` | Node-side EgressService nftables/iptables SNAT rules. |
| `controllers/evpn/` | EVPN VTEP and pod-neighbor node controller for VXLAN-based EVPN integration. |
| `dpulease/` | DPU node lease management — ensures connectivity between DPU and DPU-host via Kubernetes Lease objects. |
| `podresourcesapi/` | Client for the kubelet PodResources gRPC API, used by OVS CPU pinning. |
| `types/` | Node-specific constants (SNAT marks, interface names). |
| `util/` | Node-specific utilities — nftables forwarding rule helpers, interface checks. |

## Testing

Unit tests use Ginkgo suites alongside the code. Coverage spans gateway
initialization, OpenFlow management, route/IP-rule/nftables managers,
EVPN controllers, management port, and netlink device manager.
Mock implementations exist for the netlink device manager.
