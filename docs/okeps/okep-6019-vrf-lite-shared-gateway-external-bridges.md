# OKEP-6019: VRF-Lite with Shared Gateway Mode using Dynamic External Bridges

* Issue: [#6019](https://github.com/ovn-org/ovn-kubernetes/issues/6019)

## Networking Glossary

| Term                    | Definition                                                                                 |
|-------------------------|--------------------------------------------------------------------------------------------|
| **VRF-Lite**            | Linux host VRF separation where interfaces are attached to specific VRFs without MPLS.     |
| **Shared Gateway Mode** | OVN and host share an external OVS bridge for north/south traffic.                         |
| **External Bridge**     | OVS bridge connected to a uplink for ingress/egress of advertised pod networks.            |
| **CUDN**                | `ClusterUserDefinedNetwork`, cluster-scoped primary or secondary user-defined network API. |
| **MEG**                 | Multiple External Gateways feature for pod egress steering using additional gateway(s).    |
| **Uplink**              | The physical NIC or physical sub-interface used as the external uplink for egress traffic. |

## Problem Statement

OVN-Kubernetes support for VRF-Lite is limited to only local gateway mode today. In addition, VRF-Lite today requires
manual host preparation(interface/bridge/VRF attachment), which is operationally expensive and error-prone in
non-homogeneous node environments.

OVN-Kubernetes currently assumes one default external bridge and optionally one extra bridge via `config.Gateway.EgressGWInterface`.
This model is not sufficient for VRF-Lite in shared gateway mode, where multiple CUDNs/VRFs may require distinct physical
uplinks and therefore multiple external bridges.

## Goals

* Support VRF-Lite with shared gateway mode by allowing more than one non-default external bridge.
* Introduce a Kubernetes API to declare additional external bridges and desired uplink resolution behavior.
* Allow OVN-Kubernetes to create/manage external bridges and move physical interfaces when explicitly configured.
* Allow CUDNs to select one or more `ExternalBridge` resources while enforcing one effective bridge per node.
* Support heterogeneous node interface naming through per-node overrides and an underlay network hint.
* Preserve existing behavior for default bridge and current MEG deployments when the new API is not used.
* No-overlay should also work with VRF-Lite + Shared Gateway mode.
* DPU support should also work with this feature.
* Service traffic flow via new external bridges should work.

## Non-Goals

* Replacing FRR/FRR-K8S APIs or changing BGP peering semantics.
* Supporting more than one effective external bridge per node for the same CUDN in the initial release.
* Auto-discovering and modifying host interfaces without explicit opt-in.
* Removing `config.Gateway.EgressGWInterface` in the initial phase.
* Allowing declaration of the default gateway bridge in the new Kubernetes API.
* Improving local gateway autoconfiguration by extending the APIs for specific interfaces (non OVS bridges).

## Future-Goals

* Allowing a CUDN to attach to multiple external bridges (ECMP/multi-homing per network).
* Converging legacy `EgressGWInterface` and default gateway bridge behavior into the `ExternalBridge` API.

## Introduction

The [BGP enhancement](./okep-5296-bgp.md) introduced RouteAdvertisements and VRF-aware route export/import with FRR. It also
introduced VRF-Lite use cases for CUDNs, but these rely on manual host configuration and are explicitly scoped to local
gateway mode in current documentation.

OVN-Kubernetes node gateway behavior today assumes:

* One default external bridge (`br-ex` / `breth0`) that OVN-Kubernetes can create automatically.
* One optional secondary bridge selected by `config.Gateway.EgressGWInterface`, primarily for MEG related paths.

This works for default networking and limited dual-uplink use cases, but not for VRF-Lite in shared gateway environments,
where each tenant VRF may require its own uplink and corresponding bridge. To scale this model, bridge lifecycle and
interface placement must be API-driven and dynamic rather than restricted to static node config.

As UDN/CUDN and BGP integration become more common, users need to expose multiple isolated tenant networks to different
provider links while keeping the shared gateway datapath. Requiring pre-created bridges and manually enslaved interfaces
for each node and each VRF does not scale operationally.

Furthermore, local gateway mode is limited in that does not provide hardware acceleration for DPUs/SmartNICs. Shared
gateway mode is the default mode, and providing support for VRF-Lite there would enable hardware acceleration for VRF-Lite.

## User-Stories/Use-Cases

* As a cluster admin, I want to declare external bridge intent once and have OVN-Kubernetes converge each node accordingly.
* As a cluster admin with heterogeneous server NIC names, I want node-specific overrides plus an underlay hint fallback.
* As a cluster admin, I want to map a primary CUDN to a declared external bridge and have VRF interface attachment handled automatically.
* As a platform operator, I want existing clusters that do not use this API to continue working unchanged.

### Story 1: Dynamic external bridge for a tenant VRF in shared gateway mode

As an admin, I want to create a CUDN `blue` and a CUDN `red` that map to different physical NICs on my host, `enp1s0`
and `enp2s0`, respectively. I create `ExternalBridge` CRs named `blue` and `red`, and provide uplink information.
OVN-Kubernetes creates `br-blue` and `br-red` and moves the found uplinks to the respective OVS bridges.

An admin defines primary CUDNs `blue` and `red` and configures each CR with an `externalBridgeSelector` that matches the
respective `ExternalBridge` CR labels.
OVN-Kubernetes will then enslave the `br-blue` and `br-red` OVS bridges to the `blue` and `red` VRFs.

Next, the admin configures BGP peering across each uplink using the standard FRR-K8S procedure documented for VRF-Lite.
The admin then configures a BGP RA as normally done for VRF-Lite, where the targetVRF is set to `auto`. The RA will
generate BGP advertisement configuration for each VRF independently.

## Proposed Solution

Introduce a new cluster-scoped `ExternalBridge` CRD and extend CUDN with a generic external uplink configuration.

At a high level:

1. Admin declares `ExternalBridge` resources describing bridge management policy and uplink resolution hints.
2. CUDN optionally configures one uplink for shared gateway VRF-Lite, backed by `ExternalBridge` selectors in phase 1.
3. Node controllers reconcile bridge existence, uplink placement, VRF attachment, OpenFlow programming for services in this CUDN.
4. Existing BGP RouteAdvertisements behavior remains, but now VRF-Lite can operate in shared gateway mode with managed bridges.

### Workflow Description

1. Admin creates one or more `ExternalBridge` CRs.
2. Admin creates a primary CUDN with `externalConnectivity.uplinks` selecting `ExternalBridge` CRs by selector.
3. Admin configures FRR peering as done today.
4. Admin creates `RouteAdvertisements` with `targetVRF: auto` selecting the CUDN.
5. OVN-Kubernetes reconciles bridge + uplink + VRF state on all relevant nodes.
6. BGP routes are advertised/received as today; dataplane uses shared gateway bridge flows and UDN VRF routing semantics.

## API Details

### New CRD: ExternalBridge

```yaml
apiVersion: k8s.ovn.org/v1alpha1
kind: ExternalBridge
metadata:
  name: blue-underlay
  labels:
    network: blue
spec:
  nodeSelector: {}
  bridgeName: br-blue
  managementPolicy: Managed # Managed | Unmanaged
  uplink:
    interfaceName: eno2
    nodeInterfaceOverrides:
      ovn-worker-a: ens6f0
      ovn-worker-b: p1p1
    underlayNetwork: 192.0.2.0/24
    allowMove: true
  ipam:
    defaultGateway:
      mode: Static # Auto | Disabled | Static
      ips:
      - 192.0.2.1
  mtu: 1500
status:
  conditions: []
  nodes:
  - nodeName: ovn-worker-a
    resolvedInterface: ens6f0
    bridgeName: br-blue
    phase: Ready
```

#### ExternalBridgeSpec

* `nodeSelector` (required): selected nodes where the bridge must exist.
* `bridgeName` (optional): desired OVS bridge name. If omitted in managed mode, generated from uplink (`br<iface>`).
* `managementPolicy` (required):
  * `Managed`: OVN-Kubernetes may create bridge and move uplink (if allowed).
  * `Unmanaged`: OVN-Kubernetes validates only; no interface move.
* `uplink.interfaceName` (optional): common interface name for homogeneous environments.
* `uplink.nodeInterfaceOverrides` (optional): map `nodeName -> interfaceName`, highest resolution priority.
* `uplink.underlayNetwork` (optional): CIDR hint to infer uplink when explicit name is unavailable.
* `uplink.allowMove` (optional, default `false`): required to permit moving interface onto the bridge in managed mode.
* `ipam.defaultGateway.mode` (optional, default `Auto`):
  * `Auto`: default route is required and learned from host routing on the selected uplink/bridge.
  * `Disabled`: no default route is required/programmed for this external bridge.
  * `Static`: default route is programmed using `ipam.defaultGateway.ips`.
* `ipam.defaultGateway.ips` (optional): default gateway IPs used when mode is `Static` (single-stack or dual-stack).
* `mtu` (optional): desired MTU for this bridge/uplink; when `managementPolicy=Managed`, OVN-Kubernetes reconciles the bridge MTU.
* If `ipam.defaultGateway.mode=Static`, `ipam.defaultGateway.ips` must be set.
* If `ipam.defaultGateway.mode` is not `Static`, `ipam.defaultGateway.ips` must be unset.

The `ipam` block is intentionally extensible so additional bridge/uplink addressing and routing controls can be added
in future phases without changing the top-level `ExternalBridge` API shape.

#### Uplink resolution order

1. `uplink.nodeInterfaceOverrides[nodeName]`
2. `uplink.interfaceName`
3. inferred from `uplink.underlayNetwork`

If resolution yields zero or multiple valid candidates, node status is set to error and reconciliation for that node stops.

### CUDN CRD extension

Add optional `externalConnectivity` block to CUDN spec:

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: blue
spec:
  namespaceSelector:
    matchLabels:
      tenant: blue
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.20.0.0/16
  externalConnectivity:
    uplinks:
    - externalBridgeSelector:
        matchLabels:
          network: blue
```

#### Validation rules

* `externalConnectivity` only valid for primary CUDNs.
* `externalConnectivity.uplinks` has `MinItems=1`, `MaxItems=1` in initial release.
* `externalConnectivity.uplinks[*]` supports only `externalBridgeSelector` in initial release.
* `externalConnectivity` block is immutable after CUDN creation.
* Every selected `ExternalBridge` must exist and be `Accepted`.
* Effective per-node mapping must be unique: for each node selected for the CUDN, exactly one selected `ExternalBridge`
  may match that node via `ExternalBridge.spec.nodeSelector`.
* If no bridge matches a selected node, set CUDN condition `ExternalBridgeNotFoundForNode`.
* If more than one bridge matches a selected node, set CUDN condition `ExternalBridgeOverlapOnNode`.
* A selector may match multiple `ExternalBridge` CRs (for example, different underlay subnets on different node groups),
  as long as the per-node uniqueness rule above is satisfied.
* Multiple CUDNs may select the same uplink, as long as they do not overlap in Subnet.

#### Future extensibility

The `uplinks` field is intentionally generic. In phase 1, one uplink entry is supported and it uses
`externalBridgeSelector`. Additional uplink entry types may be added in future phases (for example, local gateway
specific uplink models) without reshaping the top-level CUDN API.

### RouteAdvertisements interaction

No new RouteAdvertisements API is required in phase 1.

Expected usage with VRF-Lite + shared gateway:

```yaml
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: blue-ra
spec:
  targetVRF: auto
  advertisements:
  - PodNetwork
  nodeSelector: {}
  frrConfigurationSelector:
    matchLabels:
      routeAdvertisements: vpn-blue
  networkSelectors:
  - networkSelectionType: ClusterUserDefinedNetworks
    clusterUserDefinedNetworkSelector:
      networkSelector:
        matchLabels:
          k8s.ovn.org/metadata.name: blue
```

## Implementation Details

### Controller changes

#### ExternalBridge controller (cluster manager)

* Watches `ExternalBridge`, Nodes, and relevant CUDNs.
* Validates spec and reports cluster-scoped conditions (`Accepted`, `Progressing`, `Degraded`).

#### Node bridge reconciler (OVNKube-Node)

For each `ExternalBridge` on a node:

1. Resolve uplink interface.
2. If `Unmanaged`, verify bridge/uplink layout only and skip managed reconciliation.
3. If `Managed` and bridge missing:
   * create bridge,
   * attach uplink,
   * move L3 addresses/routes to bridge (existing bridge utility path).
4. If `Managed`, reconcile `ipam.defaultGateway` policy:
   * `Auto`: verify/provision required default route behavior for this bridge.
   * `Disabled`: do not require or program a default route for this bridge.
   * `Static`: program the default route using configured gateway IPs.
5. If `Managed`, reconcile `mtu` if specified.
6. Enslave the bridge to the CUDN VRF.
7. Configure the OVS bridge mappings with reference to the CUDN network name.
8. Update the `k8s.ovn.org/l3-gateway-config` with the CUDN network bridge information.
9. Manage OpenFlow on the new bridge according to the CUDN running on it (services, etc.).
10. Record status with resolved interface, bridge name, and failure reason when any.

#### CUDN/UDN node controller integration (OVNKube-Controller)

* When configuring the CUDN topology in OVN with shared gateway mode, the UDN will use the l3-gateway-config configured
by OVNKube-node to configure the Gateway Router correctly.
* The localnet for the external logical switch port in OVN will be configured to use the normalized network name when
the CUDN specifies the uplink.

### Shared gateway datapath updates

The current openflow manager models one default bridge plus one optional egress bridge. This enhancement changes bridge
handling to a dynamic bridge collection while keeping default bridge semantics unchanged.

Required updates:

* Replace fixed secondary-bridge fields with iterable bridge structures.
* Generate and apply ingress/egress flows for all active external bridges relevant to selected networks.
* Preserve existing default bridge flow behavior for clusters not using `ExternalBridge`.

### Node annotation changes

The `k8s.ovn.org/l3-gateway-config` annotation is serialized as a map and can hold multiple keys, but current
node helper APIs primarily read and write the `default` key and a single optional egress-gateway field inside that
default object.

For this enhancement, OVN-Kubernetes should use additional map keys in `k8s.ovn.org/l3-gateway-config` to represent
resolved `ExternalBridge` entries on the node, while preserving the `default` key semantics for compatibility.

Example with one `ExternalBridge` (`blue-underlay`) from this document:

```yaml
k8s.ovn.org/l3-gateway-config: |
  {
    "default": {
      "mode": "shared",
      "bridge-id": "breth0",
      "interface-id": "breth0_ovn-worker",
      "mac-address": "3a:b2:df:e2:71:71",
      "ip-addresses": ["172.18.0.6/16"],
      "ip-address": "172.18.0.6/16",
      "next-hops": ["172.18.0.1"],
      "next-hop": "172.18.0.1",
      "node-port-enable": "true",
      "vlan-id": "0"
    },
    "blue-underlay": {
      "mode": "shared",
      "bridge-id": "br-blue",
      "interface-id": "br-blue_ovn-worker",
      "mac-address": "02:42:c0:00:02:06",
      "ip-addresses": ["192.0.2.6/24"],
      "ip-address": "192.0.2.6/24",
      "next-hops": ["192.0.2.1"],
      "next-hop": "192.0.2.1",
      "node-port-enable": "false",
      "vlan-id": "0"
    }
  }
```

Compatibility requirement:

* Existing `default` key behavior remains unchanged for backward compatibility.
* Existing single egress-gateway fields remain supported in phase 1.

### Feature Compatibility

#### Egress IP

Egress IP should function as it does in shared gateway mode across the new OVS bridges. Today Egress IP is sent via
GR to the external bridge, and then SNAT'ed to the Egress IP. Rather than doing this on the default gateway bridge, it
will happen implicitly on the new OVS ExternalBridge.

#### Egress Service

Not currently supported with CUDN, but in the future should behave as above.

#### MEG

Still not supported for CUDN.

#### Service access

#### Ingress traffic into ExternalBridge uplink

Service flows should still be configured in the new ExternalBridge for the CUDN. Traffic entering the uplink on the external
bridge will function just like shared gateway does today for services, with traffic being forwarded to the patch port on
the GR of the respective CUDN.

Traffic that enters the uplink destined for a non-CUDN service like Kubernetes API will be sent to the LOCAL port, where
traffic will then be forwared to the default shared gateway bridge.

#### Ingress traffic into a secondary host interface

When nodeport traffic comes into the node destined towards a service on the CUDN from an uplink not attached to the ExternalBridge,
the forwarding of the traffic to the External Bridge local interface may not work without dedicating another masquerade
IP for the host. Support for this may be limited depending on implementation.

#### Pod service access from a CUDN on an ExternalBridge uplink

Pods should still be able to access services on their CUDN internally. Additionally, pods trying to access exposed
Kubernetes services like Kubernetes API service should still work and flow from the pod -> ovn-k8s-mpx -> default shared
gateway bridge.

### Failure and rollback behavior

#### External Bridges

* If bridge creation or interface movement fails, status is updated and retries continue.
* If an admin deletes the bridge, or the bridge becomes misconfigured, OVN-Kubernetes will attempt to reconcile and fix
the bridge configuration.
* If an `ExternalBridge` CR is deleted that affects the node, OVN-Kubernetes will delete the OVS bridge and return the
uplink back to the kernel as an IP interface. However, there is no guarantee that the IP interface configuration will be
restored back to its original state.
* An `ExternalBridge` CR may be deleted, even when selected by a CUDN.

#### CUDN Uplinks

* If an uplink for a node does not exist, the CUDN will report error status, but will not halt configuration for all other
nodes. The affected node will be left in a state with a failed UDN Controller and the UDN not active on that node.
* If an `ExternalBridge` is deleted while a CUDN is active on that node, the CUDN state will be in error on that node.
The l3-gateway-config annotation on the node will remove the entry, and future OVNKube-Controller UDN reconcilation will fail.
* If an `ExternalBridge` is configured later for a node, OVNKube-Controller and OVNKube-Node UDN reconciliation should succeed.

## Testing Details

* Unit tests:
  * uplink resolution precedence and ambiguity handling.
  * bridge guardrails (`allowMove`) and ownership checks.
  * default gateway mode validation (`Auto`, `Disabled`, `Static`) and static gateway IP validation.
  * bridge MTU reconciliation behavior.
  * CUDN validation for `externalConnectivity.uplinks`.
  * selector resolution and per-node uniqueness validation.
* Node integration tests:
  * managed bridge creation and IP/route migration.
  * unmanaged validation-only path.
  * VRF attachment of bridge interface.
* E2E tests:
  * shared gateway VRF-Lite with two CUDNs on two distinct external bridges.
  * mixed homogeneous/non-homogeneous interface names.
  * route advertisement + ingress/egress datapath verification.
* Cross-feature coverage:
  * MEG coexistence with legacy `EgressGWInterface`.
  * EgressIP and EgressService unaffected behavior checks.
  * local gateway mode unchanged behavior checks.

## Documentation Details

* Add a new user-facing guide for `ExternalBridge` lifecycle and troubleshooting.
* Update BGP/RouteAdvertisements docs to include VRF-Lite shared gateway workflows.
* Add an operations section on safe rollout and interface move precautions.

## Risks, Known Limitations and Mitigations

* Host reachability disruption during interface move.
  * Mitigation: explicit opt-in (`allowMove`) and phased rollout guidance.
* Incorrect default route behavior due to invalid gateway policy (`Auto`/`Disabled`/`Static`) configuration.
  * Mitigation: strict CRD validation and explicit status conditions for gateway policy errors.
* Incorrect uplink inference from underlay hint in complex routing tables.
  * Mitigation: deterministic precedence and explicit node overrides.
* Increased complexity from dynamic multi-bridge flow programming.
  * Mitigation: phased implementation and focused scale/perf testing.
* Legacy field/API overlap (`EgressGWInterface` vs `ExternalBridge`).
  * Mitigation: additive rollout and clear precedence documentation.
* Host uplink in misconfigured state after `ExternalBridge` removal.
  * Mitigation: user docs to explain how NMState can be used to configure the IP interface after `ExternalBridge` removal.

Known limitations in phase 1:

* One effective external bridge per node per CUDN.

## OVN-Kubernetes Version Skew

Planned introduction: 1.4.

## Backwards Compatibility

* Clusters not using `ExternalBridge` have no behavior change.
* Existing default bridge and `EgressGWInterface` behavior remains supported in phase 1.
* New CUDN fields are additive and optional.

## Alternatives

* Keep host networking manual (NMState only).
  * Rejected: high operational burden and drift risk at scale.
* Use only underlay CIDR inference without explicit bridge CRD.
  * Rejected: ambiguous intent and insufficient guardrails.
* Put external bridge attachment into RouteAdvertisements only.
  * Rejected: lifecycle belongs to network connectivity intent (CUDN), not route export policy.

## References

* [OKEP-5296: OVN-Kubernetes BGP Integration](./okep-5296-bgp.md)
* [OKEP-5088: EVPN Support](./okep-5088-evpn.md)
* [OKEP-5259: No Overlay Support](./okep-5259-no-overlay.md)
