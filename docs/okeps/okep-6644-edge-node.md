# OKEP-6644: OVN-Kubernetes Edge Node

* Issue: [#6644](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6644)

## Problem Statement

One of the key benefits of Software Defined Networking is agility: a new logical network can be
created in seconds by a cluster administrator without touching any physical switch. Creating a
CUDN in OVN-Kubernetes requires no physical fabric changes — the overlay handles transport.

Bridging a physical VLAN into that SDN is a different matter. VLANs require configuration on
every switch in the path, and extending a VLAN to all cluster nodes to support a single CUDN
undermines the agility advantage the SDN was chosen for.

There is currently no supported mechanism in OVN-Kubernetes to bridge a physical 802.1Q VLAN
into an SDN network (UDN/CUDN) while preserving that agility — confining the physical
provisioning to a small, fixed set of dedicated edge nodes and letting the overlay carry
connectivity to the rest of the cluster.

## Goals

* Define the **OVN-Kubernetes Edge Node** role: a dedicated cluster node that bridges one or
  more physical 802.1Q VLANs into EVPN-transport Cluster User Defined Networks (CUDNs).
* Introduce the `EdgeNetworkBinding` CRD to express the mapping from physical interface + VLAN
  to a named CUDN, with a per-VLAN EVPN Ethernet Segment Identifier (ESI) for L2
  multihoming. ESI is scoped per VLAN because each VLAN represents a distinct broadcast
  domain whose redundancy scope is independent of the physical trunk it rides on.
* Extend the existing `ovnkube-node` EVPN network controller to attach physical VLAN
  subinterfaces to the Single VXLAN Device (SVD) bridge, participating in the same EVPN
  MAC-VRF/IP-VRF as all other cluster nodes.
* Support optional origination of EVPN Type-5 (RT-5) IP prefix routes from the edge node to
  act as a north/south gateway for CUDNs whose physical network does not include an
  EVPN-capable border router.
* Enable active-active L2 high availability across edge nodes through EVPN multihoming
  (Type-0 ESI), with each VLAN carrying its own ESI so that different VLANs on the same
  trunk can have independent redundancy topologies.

## Non-Goals

* GENEVE-transport CUDNs — physical VLAN bridging for GENEVE transport is deferred to a
  follow-on OKEP.
* iBGP route reflection — a dedicated edge node as a BGP route reflector for cluster worker
  nodes is noted as a future goal (see below) but is out of scope here.
* Replacing or modifying OVN's Gateway Router (GR) for non-EVPN networks.
* NAT (SNAT/DNAT) for EVPN CUDN egress traffic. Once traffic arrives at the edge node
  via the EVPN VRF, host-level forwarding and any NAT is the operator's responsibility
  (e.g. via NNCP/nftables). The correct OVN-Kubernetes mechanism for managed egress NAT
  is EgressIP, which does not currently support EVPN CUDNs and would be the subject of
  a separate OKEP.
* Scheduling user workload pods on the edge node — the node is tainted `NoSchedule`.
* Supporting the Cluster Default Network (CDN) — EVPN is not supported for the CDN.
* Asymmetric Integrated Routing and Bridging (IRB) with EVPN.
## Future Goals

* **iBGP Route Reflector**: Once iBGP + route-reflector support is added to the BGP OKEP
  (OKEP-5296 future goal), the edge node is the natural candidate to serve as the cluster's
  route reflector, eliminating the full-mesh or ToR-peer requirement for every worker node.
* **GENEVE transport**: Bridging physical VLANs into GENEVE-transport CUDNs, where the edge
  node exposes the physical interface as an OVN `localnet` port on the CUDN logical switch and
  OVN's GENEVE tunnel extends it to all other nodes.
* **EVPN ESI multihoming modes**: Type-1 and Type-3 ESI in addition to Type-0.
* **EgressIP for EVPN CUDNs**: EgressIP has been unsupported for EVPN CUDNs because
  there was no designated egress node — without one, SNAT cannot be pinned to a stable
  location. An edge node advertising `DefaultRoute` into a CUDN's IP-VRF is exactly that
  designated egress node: all pod egress for the CUDN flows through it, FRR already
  runs on it to advertise the EIP /32 into the underlay (the `RouteAdvertisements`
  controller already handles EgressIP BGP announcement on matching VRFs), and the
  CUDN's Linux VRF is present for a VRF-scoped nftables SNAT rule — the same pattern
  used by local gateway mode. A follow-on OKEP should formalise this integration.

## Introduction

### Background: Localnet and Edge Node

OVN-Kubernetes already supports connecting a UDN directly to a physical network segment via
the **Localnet topology** (OKEP-5085). With Localnet, OVN creates a `localnet`-type logical
switch port on every node in the CUDN. Each node requires a corresponding OVS bridge-mapping
that points to a physical bridge carrying the network segment. This is the right choice when
the physical segment is reachable at every cluster node — for example, a shared provider
network that all nodes are already connected to.

The Edge Node topology addresses a complementary scenario: the physical VLAN is only
present on a dedicated pair of border nodes, and the overlay should carry connectivity
to the rest of the cluster. The physical VLAN terminates on the edge nodes, is stitched
into the EVPN MAC-VRF or IP-VRF for the target CUDN, and is then reachable from pods on
any cluster node via the existing EVPN tunnel fabric.

| Consideration | Localnet | Edge Node (this OKEP) |
|---|---|---|
| Physical VLAN scope | Reachable at every node in the CUDN | Reachable at the edge node pair only |
| Switch port provisioning | VLAN trunk to every worker | VLAN trunk to edge node pair only |
| Overlay | None — traffic exits OVN at each node | EVPN VXLAN — overlay carries traffic to any node |
| SDN policy enforcement | Per-node at the localnet port | Cluster-wide via the CUDN |
| L2 HA | Physical redundancy per node | EVPN ESI multihoming between the edge nodes |

This approach is conceptually similar to **NSX Edge Bridging**, where a dedicated pair of
Edge Transport Nodes bridges VLANs into the NSX overlay rather than requiring VLAN
provisioning on every hypervisor host.

### EVPN Background

OVN-Kubernetes EVPN support (OKEP-5088) allows CUDNs to use VXLAN+EVPN as their transport
instead of Geneve. Each node that participates in an EVPN-transport CUDN:

* Receives a VTEP IP from the VTEP controller in `ovnkube-cluster-manager`.
* Runs a **Single VXLAN Device (SVD)** bridge (`br0`) with a `vxlan0` device operating in
  external VNIFILTER mode.
* Connects OVS to the SVD bridge via an internal port for each UDN, enabling pod traffic to
  enter the EVPN fabric.
* Uses FRR-K8S to configure FRR with per-VRF BGP sessions and EVPN address families.

The Edge Node participates in exactly the same way, and additionally attaches one or more
physical VLAN subinterfaces to the SVD bridge. From the EVPN fabric's perspective the edge
node is simply another PE (Provider Edge) router.

### North/South Gateway

When a cluster's physical network does not include an EVPN-capable border router, there is no
device in the fabric to originate EVPN Type-5 IP prefix routes that advertise external
reachability into the CUDNs' IP-VRFs. Without these routes, pods have no path for north/south
traffic within the EVPN VRF.

The edge node can optionally be configured to originate RT-5 routes (including a default
`0.0.0.0/0`) into the fabric for each CUDN's IP-VRF. Worker nodes import these routes via the
EVPN route-target and use the edge node's VTEP IP as their next hop for egress traffic.

This is a **stop-gap** for environments that lack EVPN-capable border routers. Customers with
border routers that already originate RT-5 routes into the fabric should **not** enable this
option, as it would create conflicting advertisements.

### EVPN Multihoming (ESI)

A pair of edge nodes connected to the same physical VLAN trunk constitutes an **Ethernet
Segment** in EVPN terminology. By assigning a shared Ethernet Segment Identifier (ESI) to the
segment, FRR enables EVPN multihoming:

* **Type-1** (Ethernet Auto-Discovery) and **Type-4** (Ethernet Segment) routes are exchanged
  between the two edge nodes.
* **Designated Forwarder (DF) election** is performed by FRR automatically, determining which
  edge node forwards BUM (Broadcast, Unknown unicast, Multicast) traffic per VNI.
* Unicast traffic is forwarded actively by both edge nodes (active-active).
* On failure of one edge node, the surviving node takes over BUM forwarding without
  administrator intervention.

This OKEP uses **Type-0** ESI (manually configured 9-byte value), which is the simplest form
and sufficient for a known, static pair of edge nodes.

## User-Stories/Use-Cases

### Story 1: Integrating physical VLAN devices into the SDN without fabric-wide provisioning

**As a** network administrator,  
**I want** to bridge a physical VLAN into a CUDN by configuring only a dedicated pair of edge
nodes,  
**so that** I can integrate legacy or physical VLAN-attached devices with Kubernetes workloads
without extending the VLAN across every switch in the path to every cluster node.

### Story 2: Pod connectivity to physical L2 segments

**As an** application developer,  
**I want** my pods (running on any node in the cluster) to communicate directly with physical
hosts on a legacy VLAN segment,  
**so that** I can migrate workloads incrementally without requiring the physical hosts to be
immediately re-platformed into the cluster.

### Story 3: Active-active L2 HA at the physical network boundary

**As a** platform engineer,  
**I want** two edge nodes to simultaneously bridge the same physical VLAN into the EVPN fabric,  
**so that** loss of a single edge node does not disrupt connectivity between pods and physical
hosts.

### Story 4: North/south egress for EVPN CUDNs without EVPN border routers

**As a** cluster administrator operating a data centre without EVPN-capable border routers,  
**I want** the edge node to advertise a default route into each CUDN's EVPN IP-VRF,  
**so that** pods in EVPN CUDNs can reach external networks without manual route configuration.

## Proposed Solution

### Overview

An **Edge Node** is a standard Kubernetes worker node that:

1. Carries the `k8s.ovn.org/edge-node: "true"` label and a `k8s.ovn.org/edge-node:NoSchedule`
   taint (applied by the cluster administrator or via a MachineConfig/NodeClass in OpenShift).
2. Runs `ovnkube-node` (and all other ovn-kubernetes DaemonSets) normally — it is a full OVN
   participant with OVS, FRR-K8S, and the VTEP infrastructure.
3. Has one or more physical interfaces connected to the data centre fabric, carrying
   VLANs or dedicated untagged networks.
4. Has an `EdgeNetworkBinding` CR that declares which interfaces and VLAN IDs map to
   which EVPN CUDNs, with a per-entry ESI identifying each broadcast domain.

The `ovnkube-node` EVPN network controller is extended to detect when it is running on an edge
node, read the applicable `EdgeNetworkBinding`, and add the appropriate VLAN subinterfaces to
the existing SVD bridge alongside the normal OVS and VXLAN ports.

No changes are required to the VTEP controller, the CUDN controller, OVN, or OVS.

### API Details

#### `EdgeNetworkBinding` CRD

```go
// EdgeNetworkBinding describes the EVPN network configuration for an edge
// node: which physical VLANs to bridge into which CUDNs, and which EVPN
// routes to originate on behalf of each CUDN.
//
// One EdgeNetworkBinding is created per edge node (or per set of edge nodes
// sharing the same node selector). Each entry in Networks targets one CUDN
// and independently controls EVPN route advertisement and optional physical
// VLAN bridging. An entry with no VLANMapping acts as a pure N/S gateway
// (route origination only); an entry with a VLANMapping bridges the physical
// segment into the CUDN's EVPN MAC-VRF or IP-VRF.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
type EdgeNetworkBinding struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   EdgeNetworkBindingSpec   `json:"spec"`
    Status EdgeNetworkBindingStatus `json:"status,omitempty"`
}

type EdgeNetworkBindingSpec struct {
    // NodeSelector selects the edge node(s) this binding applies to.
    // All selected nodes must have the k8s.ovn.org/edge-node label and must
    // carry the interfaces named in the Networks entries.
    // +required
    NodeSelector metav1.LabelSelector `json:"nodeSelector"`

    // Networks is the list of per-CUDN configuration entries for this edge
    // node. Each entry independently controls EVPN route advertisement and
    // optional physical VLAN bridging for one CUDN. At least one entry must
    // be present. An entry with an empty Advertise list and no VLANMapping
    // is rejected as a no-op.
    // +required
    // +kubebuilder:validation:MinItems=1
    Networks []NetworkEntry `json:"networks"`
}

// NetworkEntry configures the edge node's relationship with one EVPN-transport
// CUDN: what EVPN routes to originate and, optionally, which physical VLAN to
// bridge into it.
type NetworkEntry struct {
    // CUDN is the name of the ClusterUserDefinedNetwork. Must use EVPN
    // transport.
    // +required
    CUDN string `json:"cudn"`

    // Advertise is the list of EVPN route types this edge node originates
    // into the fabric for this CUDN's IP-VRF. When empty (default), no
    // routes are originated and the entry is valid only if VLANMapping is
    // set.
    //
    // DefaultRoute: originate a Type-5 0.0.0.0/0 into the fabric. Worker
    //   nodes import it and forward pod VRF egress to this edge node's VTEP.
    //   Use only when the fabric has no EVPN-capable border router already
    //   originating a default for this VRF. What the edge node does with
    //   that traffic after it arrives (routing, PBR, etc.) is host-level
    //   configuration outside this CRD.
    //
    // +optional
    // +kubebuilder:validation:items:Enum=DefaultRoute
    Advertise []AdvertiseMode `json:"advertise,omitempty"`

    // VLANMapping defines the physical interface and VLAN to bridge into
    // this CUDN via the EVPN SVD bridge. When omitted, the edge node acts
    // as a pure N/S gateway for this CUDN (route origination only).
    // +optional
    VLANMapping *VLANMapping `json:"vlanMapping,omitempty"`
}

// AdvertiseMode identifies a class of EVPN routes the edge node will
// originate for a CUDN's IP-VRF.
// +kubebuilder:validation:Enum=DefaultRoute
type AdvertiseMode string

const (
    // AdvertiseModeDefaultRoute causes the edge node to originate a
    // Type-5 0.0.0.0/0 EVPN prefix route for the CUDN's IP-VRF.
    AdvertiseModeDefaultRoute AdvertiseMode = "DefaultRoute"
)

// VLANMapping describes a physical interface and 802.1Q VLAN to bridge into
// a CUDN's EVPN MAC-VRF or IP-VRF.
type VLANMapping struct {
    // Interface is the physical trunk NIC (e.g. "eth1", "ens5f0"). Must
    // exist on every node selected by NodeSelector. Must not be the br-ex
    // uplink (OVS-enslaved NICs are not accessible to the Linux bridge
    // datapath).
    // +required
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=15
    Interface string `json:"interface"`

    // VLANID is the 802.1Q tag on the physical trunk. OVN-Kubernetes creates
    // a VLAN subinterface (<Interface>.<VLANID>, e.g. eth1.100) internally
    // and attaches it to the SVD bridge as an access port. The physical VLAN
    // ID is independent of the SVD bridge's internal VLAN allocation.
    //
    // When omitted, the interface is attached untagged (dedicated NIC, single
    // CUDN). An untagged interface may not appear in any other NetworkEntry
    // in the same binding.
    // +optional
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=4094
    VLANID *int32 `json:"vlanID,omitempty"`

    // ESI is the IEEE 802.3ad Type-0 Ethernet Segment Identifier for the
    // broadcast domain this VLAN represents. Scoped per VLAN because each
    // VLAN is an independent broadcast domain.
    //
    // EdgeNetworkBinding entries on different nodes that share the same ESI
    // are treated as co-members of the same broadcast domain; FRR performs
    // Designated Forwarder election between them to prevent duplicate BUM
    // flooding. ESI must be globally unique per distinct broadcast domain.
    //
    // Format: nine colon-separated two-digit hex octets,
    // e.g. "00:00:00:00:00:00:00:00:01". Must be non-zero.
    // +required
    // +kubebuilder:validation:Pattern=`^([0-9a-fA-F]{2}:){8}[0-9a-fA-F]{2}$`
    ESI string `json:"esi"`
}

type EdgeNetworkBindingStatus struct {
    // Conditions describes the current state of the EdgeNetworkBinding.
    // +optional
    // +listType=map
    // +listMapKey=type
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

#### Example EdgeNetworkBinding

```yaml
apiVersion: k8s.ovn.org/v1
kind: EdgeNetworkBinding
metadata:
  name: edge-pair-eth1
spec:
  nodeSelector:
    matchLabels:
      k8s.ovn.org/edge-node: "true"
  networks:
      # Pure N/S gateway: no VLAN bridging, just originate a default route
      # into blue-network's IP-VRF. The fabric has no EVPN border router.
    - cudn: blue-network
      advertise: [DefaultRoute]

      # VLAN bridging + default route origination.
      # VLAN 100 is extended to both edge nodes (shared ESI).
    - cudn: red-network
      advertise: [DefaultRoute]
      vlanMapping:
        interface: eth1
        vlanID: 100
        esi: "00:00:00:00:00:00:00:00:01"

      # VLAN bridging only; fabric border routers handle route origination.
      # VLAN 200 only reaches this node's ToR (unique ESI).
    - cudn: green-network
      vlanMapping:
        interface: eth1
        vlanID: 200
        esi: "00:00:00:00:00:00:00:00:02"

      # Dedicated NIC, no VLAN tagging, single CUDN.
    - cudn: purple-network
      vlanMapping:
        interface: eth2
        esi: "00:00:00:00:00:00:00:00:03"
```

#### Validation

The `EdgeNetworkBinding` controller in `ovnkube-cluster-manager` enforces the following:

* Every CUDN named in `networks[*].cudn` must exist and must have `transport: EVPN`;
  a GENEVE-transport CUDN is rejected with an error `Condition`.
* An entry with an empty `advertise` list and no `vlanMapping` is rejected (no-op).
* Each CUDN may appear at most once per `EdgeNetworkBinding`.
* Within a binding, `vlanMapping.interface` + `vlanMapping.vlanID` must be unique
  across all entries that have a `vlanMapping`.
* An untagged interface (`vlanMapping` present, `vlanID` omitted) may not share
  its interface name with any other entry in the same binding.
* `vlanMapping.esi` must be non-zero (the all-zero ESI is reserved).
* ESI values are globally unique per broadcast domain. Two `vlanMapping` entries on
  different nodes sharing the same ESI are treated as co-members of the same broadcast
  domain; FRR performs DF election between them.
* No two `EdgeNetworkBinding` objects may reference the same CUDN with conflicting ESI
  values on overlapping node selectors.

#### Node designation

The cluster administrator (or an OpenShift MachineConfig/NodeClass) applies the following to
edge nodes before creating any `EdgeNetworkBinding`:

```bash
# Label
kubectl label node <edge-node> k8s.ovn.org/edge-node=true

# Taint — prevents user workloads from being scheduled
kubectl taint node <edge-node> k8s.ovn.org/edge-node=:NoSchedule
```

All ovn-kubernetes DaemonSets (`ovnkube-node`, `ovs-node`, the FRR-K8S DaemonSet) must have
a matching toleration added to their pod specs so that the OVN stack continues to run on
tainted edge nodes:

```yaml
tolerations:
  - key: k8s.ovn.org/edge-node
    operator: Exists
    effect: NoSchedule
```

This change is required in the `dist/` manifests and the Helm chart `values.yaml`.

### Implementation Details

#### EdgeNetworkBinding controller (`ovnkube-cluster-manager`)

A new controller is added to `go-controller/pkg/clustermanager/edgenode/` following the
pattern of the existing VTEP controller. It:

1. Watches `EdgeNetworkBinding` objects and the CUDNs they reference.
2. Validates the binding (transport check, VLAN uniqueness, ESI cardinality) and writes
   `Conditions` to the status.
3. Ensures that each edge node selected by an `EdgeNetworkBinding` is enrolled in the
   referenced CUDNs. Because edge nodes are tainted and run no user pods, the CUDN
   controller would not otherwise include them. The edge node controller sets a well-known
   annotation on the node (e.g. `k8s.ovn.org/edge-cudn-membership`) that the CUDN controller
   uses to force node inclusion.
4. Does **not** directly configure any network interfaces — all host configuration is the
   responsibility of `ovnkube-node` on the edge node.

#### VTEP IP allocation

No changes are required to the existing VTEP controller. Once the edge node is enrolled in a
CUDN (step 3 above), the VTEP controller allocates a VTEP IP from the pool for that node
exactly as it does for any other EVPN-participating node. The edge node's VTEP IP is then used
as the local VXLAN source address on the SVD bridge.

#### Data plane: extending the EVPN network controller (`ovnkube-node`)

The existing per-UDN EVPN network controller in `ovnkube-node` sets up:

* A dummy interface `evlo-evpn-vtep` with the allocated VTEP IP.
* A single SVD bridge `br0` with `vlan_filtering 1` and a `vxlan0` device in `vnifilter` mode.
* An OVS internal port per UDN wired into `br0` to carry pod traffic.
* Static FDB and neighbour entries per pod.

On an edge node the same controller is extended to additionally process any
`EdgeNetworkBinding` that selects the current node. For each entry in `networks`:

A critical detail is that the SVD bridge uses its own internally-allocated bridge VLANs
(managed by the EVPN controller) to map to VNIs — these are entirely independent of the
802.1Q VLAN IDs on the physical fabric. For example, CUDN "blue-network" might be allocated
internal bridge VLAN 12 (mapping to VNI 100), while the physical network carries it on
VLAN 100. The VLAN subinterface mechanism handles this translation transparently:

```bash
# Parse the parent interface (eth1) and physical VLAN (100) from "eth1.100".
# Create the subinterface. The kernel automatically strips the 802.1Q VLAN 100
# tag from ingress frames and adds it back on egress — the bridge never sees it.
ip link add eth1.100 link eth1 type vlan id 100
ip link set eth1.100 up

# Attach eth1.100 to the SVD bridge as an access port.
# 'pvid 12' means untagged ingress frames on this port are assigned to
# internal bridge VLAN 12 (the EVPN controller's allocation for blue-network).
# 'pvid untagged' means egress frames in bridge VLAN 12 leave this port
# untagged — the subinterface then re-adds the VLAN 100 tag outbound.
# The physical VLAN ID (100) and the internal bridge VLAN (12) are
# completely independent; there is no collision risk.
ip link set eth1.100 master br0
bridge vlan add dev eth1.100 vid 12 pvid untagged
ip link set eth1.100 up
```

The EVPN controller looks up the internal bridge VLAN allocated for the referenced CUDN
and uses it as the `pvid`. The user only ever specifies the physical VLAN (encoded in the
spec.networks[*].vlanID); the internal bridge VLAN is an opaque implementation detail.

The physical subinterface acts as an **access port**: ingress frames from physical VLAN 100
enter internal bridge VLAN 12, which is mapped to VNI 100 (the CUDN's MAC-VRF). From there
the existing VXLAN path carries the traffic to all other nodes in the fabric.

**Inbound traffic (physical host → pod):**

```
Physical host (VLAN 100)
  → eth1.100 on edge node
  → br0 (bridge VLAN 12 → VNI 100)
  → vxlan0 (EVPN encapsulation, dst = worker VTEP IP)
  → Worker node: br0 decapsulates
  → OVS internal port for blue-network
  → br-int → pod
```

**Outbound traffic (pod → physical host):**

```
Pod → br-int → OVS internal port for blue-network
  → br0 on worker node (bridge VLAN 12 → VNI 100)
  → vxlan0 (EVPN encapsulation, dst = edge node VTEP IP)
  → Edge node: br0 decapsulates
  → eth1.100 → physical host (VLAN 100)
```

For Layer 3 UDNs (IP-VRF), the physical subinterface is wired to the VRF's SVI on the SVD
bridge using the same mechanism described in OKEP-5088.

#### ESI multihoming

When `spec.esi` is set, the `EdgeNetworkBinding` controller generates an additional
`FRRConfiguration` CR (via FRR-K8S) for each edge node:

ESI is scoped per VLAN (per `VLANMapping`) because each VLAN is an independent
broadcast domain. Two VLANs on the same trunk may have entirely different redundancy
topologies — one extended to both edge nodes, another terminating at a single ToR. The FRR
configuration reflects this: each VLAN subinterface is configured with its own ESI.

The `EdgeNetworkBinding` controller generates an `FRRConfiguration` CR that applies each
VLAN's ESI to its subinterface independently:

```
! VLAN 100 is shared between two edge nodes — both carry ESI 1.
interface eth1.100
 evpn mh es-id 1
 evpn mh es-sys-mac 00:00:00:00:00:01
 evpn mh df-pref 50000
!
! VLAN 200 only reaches this edge node's ToR — unique ESI 2.
interface eth1.200
 evpn mh es-id 2
 evpn mh es-sys-mac 00:00:00:00:00:02
 evpn mh df-pref 50000
!
```

FRR exchanges Type-1 (Ethernet Auto-Discovery) and Type-4 (Ethernet Segment) EVPN routes
with the peer edge node. DF election is handled entirely by FRR; no additional logic is
required in `ovnkube-node`. Both edge nodes forward unicast traffic (active-active); only one
forwards BUM traffic per VNI at any given time.

#### North/south gateway (opt-in RT-5 origination)

When `DefaultRoute` appears in a `NetworkEntry`'s `advertise` list, the
`EdgeNetworkBinding` controller adds an additional stanza to the `FRRConfiguration` CR for
that edge node:

```bash
router bgp 65000 vrf blue
 address-family l2vpn evpn
  default-originate ipv4
  advertise ipv4 unicast
 exit-address-family
exit
```

This causes FRR to originate a Type-5 default route (`0.0.0.0/0`) into the EVPN fabric for
the `blue` IP-VRF. Worker nodes running pods in `blue-network` import this route via the
route-target and send egress traffic toward the edge node's VTEP IP.

The edge node is then responsible for forwarding that egress traffic onward — either via its
physical uplink NIC or via a static/dynamic route installed by the administrator.

This option must **not** be enabled when the data centre fabric already has an EVPN-capable
border router originating RT-5 routes for the same VRF.

#### Gateway mode compatibility

The edge node supports only **shared gateway mode**. Traffic stays within the OVS/OVN
datapath and is hardware-offloadable to DPU/SmartNIC adapters. Local gateway mode is not
supported on edge nodes.

### Testing Details

* **Unit tests**: `EdgeNetworkBinding` controller validation logic (transport check, VLAN
  uniqueness, ESI cardinality, CUDN enrolment annotation).
* **Unit tests**: EVPN network controller extension — verify correct `ip link` and `bridge`
  commands issued for edge-node scenarios, with and without ESI.
* **E2E tests** (Kind cluster with simulated VLANs using veth pairs):
  * Single edge node: pod in CUDN can ping a simulated physical host on the mapped VLAN.
  * Dual edge node (ESI): same connectivity after one edge node is cordoned/drained.
  * `advertise: [DefaultRoute]`: pod egress reaches an external address via the edge node.
  * empty `advertise` with EVPN border router present: no conflicting advertisements.
  * Validation rejection: `EdgeNetworkBinding` referencing a GENEVE-transport CUDN is
    rejected with an appropriate `Condition`.
* **Scale tests**: 20 VLANs on a single edge node pair, 1000 pods spread across 10 worker
  nodes communicating with physical-VLAN hosts.
* **Cross-feature tests**: Interaction with NetworkPolicy, EgressIP, and EgressFirewall on
  EVPN CUDNs connected via the edge node.

### Documentation Details

* New feature guide at `docs/features/bgp-integration/edge-node.md` covering:
  * When to use the edge node vs. localnet topology.
  * Node labelling and tainting procedure (manual and via OpenShift MachineConfig).
  * `EdgeNetworkBinding` API reference and annotated examples.
  * N/S gateway configuration guide and guidance on when *not* to enable
    `advertise: [DefaultRoute]`.
  * ESI multihoming setup and verification steps.
* Update `mkdocs.yml` to include the new feature page and this OKEP.
* Update `dist/` manifests and Helm chart documentation to describe the new DaemonSet
  tolerations.

## Risks, Known Limitations and Mitigations

| Risk | Mitigation |
|---|---|
| Admin sets `advertise: [DefaultRoute]` when an EVPN border router already advertises a default for the same VRF | Conflicting RT-5 advertisements; BGP best-path selection may be non-deterministic. Document clearly; consider a future admission webhook that checks RA CRs for existing default-route advertisements. |
| Physical interface goes down on one edge node | FRR withdraws EVPN Type-2/5 routes for that node; traffic converges to the surviving peer. With ESI, DF re-election happens automatically. No OVN-Kubernetes intervention needed. |
| VLAN ID collision between two `EdgeNetworkBinding` objects on the same node | Controller rejects the conflicting binding with an error `Condition`. |
| Edge node taint prevents ovn-kubernetes DaemonSets from running | Toleration added to all DaemonSet pod specs; documented as a required manifest change. |
| SVD bridge VLAN namespace collides between UDN bridge VLANs and physical subinterface VLANs | Bridge VLANs for UDNs are allocated by the VTEP/EVPN controller from a separate internal range; physical VLAN IDs are in the 802.1Q user range (1–4094). Allocation code must enforce no overlap. |
| Single edge node: SPOF for physical VLAN connectivity | A single edge node is a valid configuration. ESI still takes effect for DF election if a peer is added later. Two nodes sharing a per-VLAN ESI are recommended for production availability but not enforced. |

## OVN-Kubernetes Version Skew

This feature is planned for introduction after the EVPN feature (OKEP-5088) reaches GA. The
`EdgeNetworkBinding` CRD is additive and cluster-scoped; older `ovnkube-node` binaries that
do not recognise the CRD will simply not configure the physical subinterfaces, leaving the
edge node operating as a normal EVPN-transport node without physical VLAN bridging.

## Backwards Compatibility

* The `EdgeNetworkBinding` CRD is new and additive; no existing API is modified.
* The `ovnkube-node` EVPN network controller change is gated behind detection of the
  `k8s.ovn.org/edge-node` label; existing nodes are unaffected.
* DaemonSet toleration additions are backwards compatible — tolerations do not affect nodes
  that do not carry the taint.
* Existing GENEVE-transport or non-EVPN clusters are not affected.

## Alternatives

### Localnet topology

When the physical segment is already reachable at every cluster node, the existing Localnet
topology (OKEP-5085) is the right choice and requires no new machinery. The Edge Node
topology is the appropriate alternative when the physical VLAN is only present at a dedicated
border node pair and extending it to every worker node is undesirable.

### External gateway with NAT / routing

Route traffic from physical hosts to pod CIDRs via an external router, without any L2
extension into the cluster. Rejected for this use case because it requires pod CIDRs to be
routable in the physical network, adds a routing hop and latency, and loses the L2 adjacency
needed for some legacy applications.

### Localnet on edge nodes only (no overlay)

Deploy the CUDN with localnet topology but restrict it to edge nodes via node selector, then
rely on pod scheduling affinity to land all workloads on edge nodes. Rejected because it
eliminates the benefit of distributing pods across the cluster and creates a scheduling
bottleneck.

## References

* [OKEP-5088: EVPN Support](okep-5088-evpn.md)
* [OKEP-5085: Localnet API](okep-5085-localnet-api.md)
* [OKEP-5296: OVN-Kubernetes BGP Integration](okep-5296-bgp.md)
* [FRR-K8S](https://github.com/metallb/frr-k8s)
* [EVPN Multihoming — RFC 7432](https://www.rfc-editor.org/rfc/rfc7432)
* [EVPN Type-5 Routes — RFC 9136](https://www.rfc-editor.org/rfc/rfc9136)
