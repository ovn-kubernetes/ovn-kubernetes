# OKEP-6557: Plexus — Network Orchestration via AdministrativeNetworkDomains

* Issue: [#6557](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6557)

## Problem Statement

Achieving VPC-like network isolation on OVN-Kubernetes today requires manually
creating and wiring together multiple CRD primitives
(ClusterUserDefinedNetworks, ClusterNetworkConnects, RouteAdvertisements,
EgressIPs, NetworkPolicies, AdminNetworkPolicies). This manual wiring is
error-prone, provides no single pane of glass for network health, and does
not extend across cluster boundaries. Even after all that effort, there are
still gaps — constructs like Route Tables have no OVN-Kubernetes equivalent
today. Users deploying distributed applications need the ability to extend
networks between clusters and have policies, routes, and connectivity follow
wherever the network is present.

## Introduction

A **Virtual Private Cloud (VPC)** is a logically isolated virtual network
within a cloud environment. It gives the owner complete control over IP
address ranges, subnets, route tables, and network gateways. VPCs are a
foundational construct in both public and private clouds:

- [AWS VPC](https://docs.aws.amazon.com/vpc/latest/userguide/how-it-works.html) —
  an isolated section of the AWS cloud with user-defined IP ranges, subnets
  across Availability Zones, route tables, internet and NAT gateways,
  security groups, and VPC peering.
- [Google Cloud VPC](https://cloud.google.com/vpc/docs/overview) —
  a global VPC spanning all regions with regional subnets, firewall rules,
  routes, Cloud VPN / Cloud Interconnect for hybrid connectivity, and VPC
  peering for cross-project communication.
- [Azure Virtual Network (VNet)](https://learn.microsoft.com/en-us/azure/virtual-network/virtual-networks-overview) —
  the foundational private network in Azure, with user-defined address
  spaces, subnets, Network Security Groups, route tables, VNet peering,
  and VPN / ExpressRoute gateways for on-premises connectivity.
- [VMware NSX VPC](https://techdocs.broadcom.com/us/en/vmware-cis/nsx/vmware-nsx/4-2/administration-guide/nsx-multi-tenancy/nsx-virtual-private-clouds.html) —
  NSX VPCs provide a self-service multi-tenant networking model with
  independent routing domains, Layer 2 subnets, Tier-0/VRF gateway
  connectivity, DHCP, and IP address management. At the vSphere layer, VMs
  attach to networks via
  [distributed portgroups](https://techdocs.broadcom.com/us/en/vmware-cis/vsphere/vsphere/8-0/vsphere-networking-8-0/basic-networking-with-vnetwork-distributed-switches/dvport-groups.html)
  which define per-port VLAN, security, and traffic shaping policies on a
  distributed virtual switch.

The following subsections define the core constructs that make up a VPC.

### Subnets

A **subnet** is a segment of a VPC's IP address range where workloads are
placed. Subnets provide isolation within the VPC and can be classified by
their connectivity:

- **Public subnet**: Has a route to an internet gateway; resources are
  externally reachable.
- **Private subnet**: No direct internet route; uses NAT for outbound access.
- **Isolated subnet**: No routes to destinations outside the VPC.
- **VPN-only subnet**: Routes traffic through a VPN connection.

See: [AWS Subnets](https://docs.aws.amazon.com/vpc/latest/userguide/configure-subnets.html),
[NSX VPC Subnets](https://techdocs.broadcom.com/us/en/vmware-cis/nsx/vmware-nsx/4-2/administration-guide/nsx-multi-tenancy/nsx-virtual-private-clouds.html)

### Route Tables

A **route table** contains rules that determine where network traffic is
directed. Every subnet is associated with a route table. Routes specify a
destination CIDR and a target (e.g. internet gateway, NAT gateway, peering
connection). The most specific matching route (longest prefix match) wins.

In a VPC, all subnets automatically have a "local" route that enables
intra-VPC communication — subnets within the same VPC can always reach
each other.

See: [AWS Route Tables](https://docs.aws.amazon.com/vpc/latest/userguide/VPC_Route_Tables.html)

### Internet Gateway / NAT Gateway

An **internet gateway** enables inbound and outbound communication between
resources in public subnets and the public internet. A **NAT gateway** provides
outbound internet access for resources in private subnets without exposing
them to inbound internet traffic.

See: [AWS Internet Gateway](https://docs.aws.amazon.com/vpc/latest/userguide/working-with-igw.html)

### Security Groups

A **security group** is a set of stateful virtual firewall rules that control inbound and
outbound traffic at the instance level. Rules specify allowed protocols,
ports, and source/destination CIDRs. Because they are stateful, return
traffic is automatically allowed.

See: [AWS Security Groups](https://docs.aws.amazon.com/vpc/latest/userguide/vpc-security-groups.html)

### Network ACLs

A **network ACL (NACL)** is a stateless firewall at the subnet level. Unlike
security groups, NACLs evaluate rules in order (by rule number) and require
explicit allow/deny for both inbound and outbound traffic.

See: [AWS Network ACLs](https://docs.aws.amazon.com/vpc/latest/userguide/nacl-basics.html)

### VPC Peering

A **VPC peering connection** is a networking connection between two VPCs that
enables routing traffic between them using private IP addresses. It is not a
gateway or VPN -- there is no single point of failure or bandwidth bottleneck.
Peered VPCs cannot have overlapping CIDR blocks.

See: [AWS VPC Peering](https://docs.aws.amazon.com/vpc/latest/peering/vpc-peering-basics.html)

### Route Server

A **route server** enables dynamic routing within a VPC by
exchanging routes with network appliances via BGP. It automatically updates
route tables when devices fail, providing routing fault tolerance without
manual intervention.

See: [AWS VPC Route Server](https://docs.aws.amazon.com/vpc/latest/userguide/dynamic-routing-route-server.html)

### VPN Connections

A **VPN connection** provides encrypted connectivity between a VPC and an
on-premises network or another VPC over the public internet. AWS supports
Site-to-Site VPN (two redundant tunnels per connection) and Client VPN
(OpenVPN-based remote access).

See: [AWS VPN Connections](https://docs.aws.amazon.com/vpc/latest/userguide/vpn-connections.html)

### Transit Gateway

A **transit gateway** acts as a central hub for routing traffic between
multiple VPCs, VPN connections, and on-premises networks. It simplifies
network architecture by avoiding the need for full-mesh peering between
every pair of VPCs.

See: [AWS Transit Gateway](https://docs.aws.amazon.com/vpc/latest/tgw/what-is-transit-gateway.html)

## Goals

- Define a new **AdministrativeNetworkDomain** (AND) CRD — a
  backend-agnostic abstraction that provides the same isolation, subnetting,
  routing, and security capabilities as a VPC — and a **Plexus controller**
  that translates AND intent into OVN-Kubernetes networking constructs.
  The AND API will start at `v1beta1` — the scope of a full network
  orchestration layer is large, and it is better to start small and expand
  incrementally. A `v1beta1` API gives soak time for the initial feature
  set to stabilize through real-world usage before committing to a `v1`
  API, preserving the ability to make breaking changes if needed without
  the cost of a full migration. The primary user-facing interfaces for the
  initial delivery are the **CLI plugin** and **web UI plugin**. Users
  should not need to hand-craft AND YAML; the CLI and UI are the intended
  entry points.
- Design Plexus with a **backend plugin architecture** so that OVN-Kubernetes
  is the first and reference backend, but other backends (cloud provider
  APIs, other CNIs, non-Kubernetes platforms) can be added without changing
  the core controller.
- Provide a **CLI plugin** (`kubectl plexus`) for creating, managing, and
  inspecting ANDs from the command line.
- Provide a **web UI plugin** that adds a dedicated AND management
  page for visual creation and monitoring of ANDs, subnets, and
  their status.
- Support ANDs on single as well as multiple clusters on bare-metal
  environments.
- Model the following VPC features as part of the AND abstraction
  (for definitions see [Introduction](#introduction)):
    - subnets (public, private, isolated, vpn)
    - route tables
    - internet and NAT gateways
    - security groups and NACLs
- Design implementation constructs in OVN-Kubernetes where required.
    - Preserve backward compatibility of existing OVN-Kubernetes APIs.
- ANDs are modelled assuming EVPN as the transport fabric for the
  OVN-Kubernetes backend.
- Host the Plexus controller, CLI, and UI plugin in a **new repository**
  under the ovn-kubernetes organization (`ovn-kubernetes/plexus`).

## Future Goals

- Support ANDs on single as well as multiple clusters on cloud
  environments.
- Model the following additional VPC features as part of the AND
  abstraction (for definitions see [Introduction](#introduction)):
    - route server
    - vpn connections
    - network domain peering
    - transit gateway
- Add support for additional backends beyond OVN-Kubernetes (e.g.
  a totally non-Kubernetes backend).
- Add support for additional transport types beyond EVPN (e.g. GENEVE, pure
  BGP) in the OVN-Kubernetes backend.
- Sophisticated SNAT for Private subnets via EgressIP. The initial
  implementation uses node IPs for outbound NAT to reduce complexity. A
  future iteration will introduce EgressIP-based SNAT to provide dedicated,
  stable egress addresses per subnet — enabling firewall allowlisting,
  audit trails, and per-AZ egress affinity.
- AND-level `cidrBlocks` with automatic subnet CIDR allocation.
  Instead of users specifying a CIDR per subnet, an AND-level CIDR
  block would allow the IPAM allocator to automatically carve out subnet
  CIDRs from the AND's address space — similar to how AWS VPCs
  define an overarching CIDR from which subnets are allocated.
- Graduate the AND API from `v1beta1` to `v1` once the API surface
  has stabilized through CLI/UI usage and community feedback.

## Non-Goals

- Replacing or deprecating existing OVN-Kubernetes APIs (UDN, CUDN, CNC).
  Plexus is additive — users can still create bare UDNs and CNCs directly
  without an AND.
- Implementing non-OVN-Kubernetes backends in the initial delivery. The
  backend plugin interface will be defined, but only the OVN-Kubernetes
  backend will be implemented.
- Managing the physical network fabric. EVPN/BGP peering between cluster
  nodes and the physical fabric is an infrastructure prerequisite that Plexus
  does not provision. The cluster operator must create a
  [FRRConfiguration](https://ovn-kubernetes.io/features/bgp-integration/evpn/#step-1-configure-bgp-peering)
  CR on each cluster to establish BGP peering (ASN + neighbor addresses)
  before deploying any ANDs.

## User-Stories/Use-Cases

### Story 1: Infrastructure Networking

A VPC is a networking construct — it defines an isolated address space with
subnets, routing, and gateways. It exists in AWS, VMware NSX, Azure, and GCP
independently of any workload orchestrator. Kubernetes CRDs are an
implementation detail — the mechanism through which a backend realizes VPC
intent.

* **As a** network engineer, **I want** to define VPCs using familiar
  networking concepts (CIDR blocks, subnets, public/private/isolated
  connectivity, NAT, route tables) without needing to learn
  Kubernetes-specific primitives, **so that** I can design and operate the
  network using the same mental model I use for AWS or on-prem
  infrastructure.

* **As an** infrastructure architect, **I want** the VPC to be the network
  boundary for any workload type — containers, VMs, or future runtimes —
  **so that** the network design is decoupled from the workload
  orchestration layer and survives platform evolution.

### Story 2: User Experience

Today an admin must create 6-8 individual resources (ClusterUserDefinedNetwork,
ClusterNetworkConnect, RouteAdvertisements, EgressIP, NetworkPolicy,
AdminNetworkPolicy) and wire them together with correct labels. The VPC
should be the single input point.

* **As a** cluster administrator, **I want** to define a VPC — its address
  space, subnets, connectivity classes, and policies — in a single resource,
  **so that** the controller creates and manages all underlying networking
  primitives on my behalf.

* **As an** application developer, **I want** to deploy workloads into a
  namespace belonging to a VPC subnet without learning OVN-Kubernetes
  internals (ClusterUserDefinedNetworks, ClusterNetworkConnects, EgressIPs,
  RouteAdvertisements).

* **As a** cluster admin, **I want** intra-VPC routing to be automatic
  **so that** I do not have to manually wire connectivity between every pair
  of subnets in the same VPC.

### Story 3: Sovereign Cloud and Regulated Industries

[Sovereign cloud](https://redhat.com/en/resources/elements-of-cloud-sovereignty-overview)
mandates data residency, deterministic network egress, and isolation within
national/regional boundaries. A VPC provides the network isolation boundary
that maps to a regulatory domain — all traffic, routing, and policies are
scoped to a well-defined perimeter.

* **As a** sovereign cloud operator, **I want** to define an isolated network
  boundary (VPC) per regulatory domain **so that** all workload traffic,
  egress paths, and security policies are confined within that boundary and
  auditable as a single unit.

* **As a** platform admin in a regulated industry (finance, healthcare,
  government), **I want** network isolation enforced at the VPC level — not
  just namespace-level NetworkPolicy — **so that** I can demonstrate to
  auditors that tenant networks are architecturally separated, not just
  policy-separated.

* **As a** sovereign cloud operator, **I want** to define custom routes per
  VPC (e.g. force all egress through an in-region inspection appliance, or
  route to on-prem endpoints within the regulatory boundary) **so that** I
  have deterministic control over traffic.

### Story 4: VMware Migration

End users migrating from VMware (vSphere + NSX) expect network constructs
they already understand: isolated network segments, per-tenant subnets, NAT
gateways, firewall rules. A VPC maps directly to the NSX VPC / vSphere
portgroup model, providing a familiar landing zone for migrated VMs running
on KubeVirt.

* **As an** infrastructure team migrating from VMware vSphere + NSX, **I
  want** to recreate my existing network segments (portgroups, NAT, firewall
  rules) as VPC subnets, **so that** migrated VMs retain equivalent network
  isolation and connectivity without redesigning the network from scratch.

* **As a** VM owner migrating to KubeVirt, **I want** my VM
  to land in a VPC subnet with the same connectivity class (public, private,
  isolated) it had in VMware, **so that** the migration is transparent to
  applications.

### Story 5: Multi-Tenancy

UDNs provide per-namespace network isolation. A VPC adds what UDN alone
cannot: a grouping boundary that spans multiple subnets, automatic routing
between subnets in the same tenant, unified address space management, and
declarative control over how each subnet reaches the outside world.

* **As a** platform team serving multiple tenants, **I want** each tenant to
  get a VPC with its own address space and subnets, **so that** tenant
  isolation is enforced at the network boundary — not just by NetworkPolicy
  rules that can be misconfigured.

### Story 6: Multi-Cluster / Distributed Applications

Distributed databases (CockroachDB, YugabyteDB) and service meshes require
network connectivity that spans cluster boundaries. The network — and
everything that influences its behaviour (policies, routes, connectivity) —
must be consistent wherever the application is deployed.

* **As a** cluster operator, **I want** to extend my VPC across clusters
  **so that** distributed applications (e.g. CockroachDB) can communicate
  over a shared network with consistent policies.

* **As a** cluster admin managing multiple clusters, **I want** to define my
  network intent once and have it rendered on every cluster where the VPC is
  present, rather than manually replicating configuration across clusters.

## Proposed Solution

### Plexus and AdministrativeNetworkDomain

This OKEP proposes **Plexus** — a network orchestrator that introduces a
new CRD called **AdministrativeNetworkDomain**. The name "Plexus" comes
from Latin *plexus* meaning "an interwoven network" — Plexus weaves
lower-level networking primitives (UDNs, routing, policies, gateways)
into a cohesive network, just as a loom weaves threads into fabric.

We call the core abstraction an **AdministrativeNetworkDomain** rather than
"VPC" for two reasons. First, on cloud backends the underlying cloud
already has its own VPC (AWS) or VNet (Azure) as part of the infrastructure
— having a second "VPC" as an overlay abstraction on top of the cloud's own
VPC would create confusion between the Plexus-level construct and the
cloud-level construct. An AND sits *above* these backend-specific
primitives without duplicating their semantics. Second, an AND is
deliberately not a 1:1 mapping to any single cloud VPC model — it is
Plexus's own take on network domain isolation, drawing inspiration from
VPCs but tailored for Kubernetes-native multi-backend orchestration.
The name "domain" (from Latin *dominium* — "territory under governance")
captures this precisely: an administrative grouping of network constructs
(subnets, routes, gateways, security policies) under unified control.

### Approach

Plexus has three layers:

1. **User-facing interfaces** (portable): The `plexus` CLI (usable as a
   standalone binary or as a `kubectl` plugin for Kubernetes backends)
   and the web UI. These capture network intent — create an AND, add
   subnets, define connectivity — in a way that is independent of any
   backend.

2. **Intent API** (backend-specific): Defines how intent is stored and
   accessed. On Kubernetes, this is the **AdministrativeNetworkDomain CRD**
   (cluster-scoped) — a standard Kubernetes API that serves dual duty: it
   is both the intent storage layer and a user-facing API in its own right.
   Users can interact with it directly via `kubectl apply` and YAML, or
   through the CLI/UI from layer 1. On a non-Kubernetes backend
   (hypothetical), this layer would be a standalone Plexus API service
   backed by its own database — similar to how OpenStack Neutron provides
   its own REST API for network intent. The `plexus` CLI and web UI would
   talk to it the same way.

3. **Backend realization**: The **Plexus controller** translates the stored
   intent into backend-specific resources. For OVN-Kubernetes, this means
   creating UDNs, CNCs, RouteAdvertisements, and policies. Hypothetically,
   a future backend could be added but this is not in scope (see
   [Future Backend Possibilities](#future-backend-possibilities)).

The controller is implemented in a new repository under the ovn-kubernetes
organization (`ovn-kubernetes/plexus`). In multi-cluster deployments, it
also synchronizes network configuration, policies, and routes across
clusters.

**A note on why the AND is a new CRD in a separate project:** The AND
abstraction is intentionally not part of the OVN-Kubernetes project
itself. Not every OVN-Kubernetes user needs VPC-like constructs, and adding
this scope to OVN-Kubernetes would bloat the project for users who only care
about basic networking. By hosting the AND CRD and Plexus controller in a
separate repository, users who want VPC-like isolation opt in explicitly,
and the OVN-Kubernetes core stays focused on its existing APIs (UDN, CUDN,
CNC, etc.).

We plan to take a bottom-up approach at solving this for a single-cluster
first, and then extrapolating that for multi-cluster scenarios.

### Backend Plugin Architecture

Plexus is designed to be **backend-agnostic**. The user-facing interfaces
— the CLI (`plexus create`, `plexus add-subnet`) and the web UI — are
portable and capture network intent (subnets, routing, security, gateways)
independent of how that intent is realized. On Kubernetes, the intent is
stored as a CRD, but the CRD is an implementation detail of the Kubernetes
backend — not something users need to interact with directly. The controller
calls a **Backend interface** that translates the intent into
backend-specific constructs — for the OVN-Kubernetes backend, this means
the Plexus controller ultimately creates the underlying OVN-Kubernetes resources
(namespaces, UDNs, CNCs, RouteAdvertisements, NetworkPolicies, etc.).

**OVN-Kubernetes is the only backend in scope.** The rest of this OKEP
details that backend exclusively:

```
         ┌──────────────────────────────────────────────┐
         │              Plexus Controller               │
         │                                              │
         │  watches: AdministrativeNetworkDomain CRDs   │
         │  calls:   Backend interface                  │
         └──────────────────────┬───────────────────────┘
                                │
                                ▼
         ┌──────────────────────────────────────────────┐
         │          Plexus OVN-Kubernetes Backend       │
         │                                              │
         │  Creates OVN-Kubernetes API resources        │
         │  (ns, UDNs, CNCs, RA, NetworkPolicy, etc.)   │
         └──────────────────────┬───────────────────────┘
                                │
                                ▼
         ┌──────────────────────────────────────────────┐
         │          OVN-Kubernetes Controllers          │
         │                                              │
         │  Reconcile API resources into OVN datapath   │
         │  (existing ovnkube controllers, unchanged)   │
         └──────────────────────────────────────────────┘
```

#### Future Backend Possibilities

While only the OVN-Kubernetes backend is in scope, the backend interface is
designed to leave the door open for future backends — for example, a
non-Kubernetes backend that provisions network isolation directly on data
center switches, routers, or platforms like VMware NSX. The Plexus CLI and
UI would remain the same; only the backend and intent storage would differ.

### Assumptions (OVN-Kubernetes backend, initial delivery)

- The physical network must support EVPN (BGP peering between cluster nodes
  and the fabric) as an infrastructure prerequisite for the OVN-Kubernetes
  backend.
- Each AND subnet is modelled as a **flat
  [Layer 2](https://ovn-kubernetes.io/features/user-defined-networks/user-defined-networks/#implementation-details)
  UDN** — all pods/VMs connect to the same broadcast domain (a single
  OVN logical switch stretched across nodes) spanning the requested
  availability zone.
- Pod/VM IPs are allocated from the subnet's CIDR as a flat pool, not carved
  into per-node slices (as they would be in a Layer 3 topology).
- [Layer 3](https://ovn-kubernetes.io/features/user-defined-networks/user-defined-networks/#implementation-details)
  topologies (per-node subnets connected via a distributed router),
  Geneve overlay, and
  [no-overlay](https://ovn-kubernetes.io/okeps/okep-5259-no-overlay/)
  (pure BGP routing without encapsulation) may be supported in the future
  but are not in scope for the initial implementation.

### Deployment Model

The AND CRD is a **management-plane-only abstraction**. It is installed and
reconciled exclusively on the management cluster. Spoke clusters never see
the AND CRD — they receive only native OVN-Kubernetes resources (namespaces,
UDNs, CNCs, RouteAdvertisements, NetworkPolicy, etc.) pushed by the Plexus
controller.

- **Management cluster**: AND CRD is registered here; Plexus controller runs
  here; users create and manage AND resources here (via CLI, console, or
  YAML).
- **Spoke clusters**: No AND CRD, no Plexus controller. They only see
  standard Kubernetes/OVN-K resources applied by the hub.
- **Single-cluster mode**: The management cluster *is* the spoke — the AND
  CRD and the translated OVN-K resources all live on the same cluster. No
  kubeconfig secrets are needed.
- **Multi-cluster mode**: The Plexus controller discovers spoke clusters via
  **labeled kubeconfig Secrets** on the management cluster (see
  [Multiple Clusters](#multiple-clusters) for details).

### Single Cluster

In a single cluster, an AND is an isolation boundary that groups one
or more subnets into a logically isolated network with its own address space,
automatic intra-domain routing, security policies, and well-defined
points of external connectivity.

**Example: two ANDs on a single cluster**

```
┌─────────────────────────────────────┐
│ AND: production                     │
│ cidrBlocks: [10.0.0.0/16]           │
│                                     │
│ subnets:                            │
│   web-a  (Public,   10.0.1.0/24)    │
│   web-b  (Public,   10.0.2.0/24)    │
│   app-a  (Private,  10.0.10.0/24)   │
│   app-b  (Private,  10.0.11.0/24)   │
│   db-a   (Isolated, 10.0.20.0/24)   │
│   db-b   (Isolated, 10.0.21.0/24)   │
└──────────────────┬──────────────────┘
                   │
                   │ Plexus creates ns + UDN per subnet
                   ▼
┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐
│ ns:      │ │ ns:      │ │ ns:      │ │ ns:      │ │ ns:      │ │ ns:      │
│ prod-    │ │ prod-    │ │ prod-    │ │ prod-    │ │ prod-    │ │ prod-    │
│ web-a    │ │ web-b    │ │ app-a    │ │ app-b    │ │ db-a     │ │ db-b     │
│          │ │          │ │          │ │          │ │          │ │          │
│ UDN:     │ │ UDN:     │ │ UDN:     │ │ UDN:     │ │ UDN:     │ │ UDN:     │
│ web-a    │ │ web-b    │ │ app-a    │ │ app-b    │ │ db-a     │ │ db-b     │
│ Public   │ │ Public   │ │ Private  │ │ Private  │ │ Isolated │ │ Isolated │
│ L2 EVPN  │ │ L2 EVPN  │ │ L2 EVPN  │ │ L2 EVPN  │ │ L2 EVPN  │ │ L2 EVPN  │
│ + RA     │ │ + RA     │ │ + SNAT   │ │ + SNAT   │ │          │ │          │
└──────────┘ └──────────┘ └──────────┘ └──────────┘ └──────────┘ └──────────┘


┌─────────────────────────────────────┐
│ AND: staging                        │
│ cidrBlocks: [10.1.0.0/16]           │
│                                     │
│ subnets:                            │
│   app (Private, 10.1.0.0/24)        │
└──────────────────┬──────────────────┘
                   │
                   │ Plexus creates ns + UDN
                   ▼
              ┌──────────┐
              │ ns:      │
              │ staging- │
              │ app      │
              │          │
              │ UDN: app │
              │ Private  │
              │ L2 EVPN  │
              └──────────┘

Intra-domain routing: automatic routing between all
non-Isolated subnets within each AND (CNC for Geneve,
route leaking for EVPN). No manual wiring required.
```

For a detailed cross-platform comparison of AND constructs across AWS,
VMware NSX, and OVN-Kubernetes, see
[Appendix: Cross-Platform Feature Mapping](#appendix-cross-platform-feature-mapping).

When a user creates an AND, the Plexus controller (via the OVN-Kubernetes
backend):

- Validates that subnet CIDRs fall within the AND's address space
  (**CIDR governance**)
- Creates the underlying UDNs for each subnet defined in the spec
- Sets up automatic routing between all subnets in the AND (the
  equivalent of the implicit "local" route in AWS), so that subnets can
  communicate without the user having to manually create interconnect
  resources
- Aggregates status from member UDNs into a single AND status,
  giving the user one place to check the health of their network
- Supports day-2 expansion: new subnets can be added and removed from
  the AND spec

The controller maps AND intent to the following existing and new
OVN-Kubernetes resources, none of which users need to create or manage
directly:

| AND Concept | Underlying OVN-Kubernetes Resource | Notes |
|---|---|---|
| Subnet | (C)UDN (existing, unchanged) | Each (C)UDN is a subnet; heterogeneous topologies and transports within the same AND. Namespace-scoped primary UDNs may also be used once RA supports selecting them ([#6604](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6604)). |
| Intra-domain routing | CNC (Geneve) / Route leaking (EVPN, new) | Automatic "local" route between all non-Isolated member CUDNs |
| Domain peering | CNC (existing, extended with `networkDomainSelector`) | Transit fabric between ANDs |
| Custom routes | RouteTable (new) | Programs VRF (LGW) or GR (SGW) |
| Public subnets | RouteAdvertisements (existing) | BGP export |
| NAT / IGW | Node IP SNAT; EgressIP *(future)* | Outbound SNAT |
| Security groups | NetworkPolicy (existing) | Stateful pod-level firewall |
| NACLs | AdminNetworkPolicy (existing) | Cluster-scoped ordered rules |

Users can still create bare CUDNs and CNCs directly without a
AND — the existing OVN-Kubernetes workflows are fully preserved.
The AND layer is additive.

### Multiple Clusters

In single-cluster deployments, the management cluster is both hub and spoke —
the AND CRD and the translated OVN-K resources all live on the same cluster.
No kubeconfig secrets are needed. The hub-spoke model described below only
applies when the AND spans more than one cluster.

**TODO**: Determine whether Plexus should also support a standalone SaaS
deployment model where it runs outside any Kubernetes cluster. In that model,
the CLI/console would talk to the Plexus service directly (via REST API),
and Plexus would push rendered resources to target clusters via kubeconfigs
without requiring the AND CRD on any cluster. The current model assumes the
AND CRD is installed on a management cluster.

In a multi-cluster deployment, the AND must span cluster boundaries
so that subnets, gateways, route tables, and security groups are rendered on
every cluster where the AND is present. The proposed model is
**hub-spoke**.

```
 ┌──────────────────────────────────────────────────────────────┐
 │                    Management (Hub) Cluster                  │
 │                                                              │
 │  AND CRD registered here (management-plane only)             │
 │                                                              │
 │  AND: production                                             │
 │  subnets:                                                    │
 │   - web-a (Public, 10.0.1.0/24)                              │
 │   - app-a (Private, 10.0.10.0/24)                            │
 │   - db-a  (Isolated, 10.0.20.0/24)                           │
 │                                                              │
 │  ┌────────────────────────────────────────────────────┐      │
 │  │ Plexus Controller (multicluster-runtime)           │      │
 │  │ - Watches AND CRs on management cluster            │      │
 │  │ - Discovers spokes via labeled kubeconfig Secrets  │      │
 │  │   (plexus.io/cluster: "true")                      │      │
 │  │ - Renders ns + UDNs, policies, RA per cluster      │      │
 │  │ - Pushes rendered resources to spokes              │      │
 │  └────────┬───────────────────────────┬───────────────┘      │
 │           │                           │                      │
 │  ┌────────┴──────────┐  ┌─────────────┴────────┐             │
 │  │ Secret: cluster-a │  │ Secret: cluster-b    │             │
 │  │ plexus.io/cluster │  │ plexus.io/cluster    │             │
 │  │ plexus.io/region: │  │ plexus.io/region:    │             │
 │  │   us-east         │  │   us-west            │             │
 │  │ (kubeconfig data) │  │ (kubeconfig data)    │             │
 │  └────────┬──────────┘  └─────────────┬────────┘             │
 │           │                           │                      │
 └───────────┼───────────────────────────┼──────────────────────┘
             │ push via                  │ push via
             │ multicluster-runtime      │ multicluster-runtime
             ▼                           ▼
 ┌─────────────────────────────┐ ┌─────────────────────────────┐
 │ Spoke Cluster A             │ │ Spoke Cluster B             │
 │                             │ │                             │
 │ No AND CRD installed        │ │ No AND CRD installed        │
 │ Receives only OVN-K         │ │ Receives only OVN-K         │
 │ resources from hub          │ │ resources from hub          │
 │                             │ │                             │
 │ ovnkube-cluster-manager     │ │ ovnkube-cluster-manager     │
 │ ovnkube-controller          │ │ ovnkube-controller          │
 │ - Reconciles UDNs locally   │ │ - Reconciles UDNs locally   │
 │ - Programs OVN flows        │ │ - Programs OVN flows        │
 └─────────────────────────────┘ └─────────────────────────────┘
```

#### How it works

1. **Single source of truth**: The AND resource and all associated
   high-level constructs (security groups, NACLs, route tables, subnet
   definitions) are defined on the **hub cluster**. This is the only place
   users create or modify AND configuration.

2. **External IPAM**: OVN-Kubernetes's built-in IPAM is disabled for
   AND subnets. An external, pluggable IPAM controller manages
   each subnet's CIDR as a flat pool and assigns IPs to pods directly via
   annotations. See [IPAM](#ipam) for details.

3. **Hub renders and applies all resources**: The Plexus controller on the
   hub renders the full set of resources for each cluster — namespaces, UDNs
   (L2 EVPN), RouteAdvertisements (Public), NetworkPolicies,
   AdminNetworkPolicies, and RouteTables. For the hub itself, resources are
   created directly via the local API. For spokes, multicluster-runtime
   manages a per-spoke client built from **labeled kubeconfig Secrets**
   (`plexus.io/cluster: "true"`) on the management cluster, and the
   controller pushes rendered resources to each spoke's API server.

4. **Spokes receive standard Kubernetes resources**: Spoke clusters see only
   plain Kubernetes resources (UDN, RouteAdvertisements, NetworkPolicy, etc.)
   applied by the hub. They do not need to understand the AND
   abstraction.

   **Multi-cluster client implementation:** The Plexus controller uses
   [multicluster-runtime](https://github.com/multicluster-runtime/multicluster-runtime),
   a SIG-Multicluster-blessed extension to `controller-runtime`. It
   discovers spoke clusters via the `secret` Provider, which watches
   `Secret` resources labeled `plexus.io/cluster: "true"` on the management
   cluster. Each Secret contains a kubeconfig for one spoke cluster.
   Metadata labels on the Secret (e.g., `plexus.io/region`,
   `topology.kubernetes.io/zone`) serve as the cluster inventory for
   `clusterSelector` matching in AND subnet availability zones.

   The library provides:
   - Automatic per-cluster client lifecycle management (creation, caching,
     teardown when a Secret is removed)
   - Kubeconfig rotation handling
   - Built-in sharding across controller replicas via Rendezvous Hashing
   - A `Provider` interface that can be swapped for alternative cluster
     discovery mechanisms in the future

   See [Multi-Cluster Client Alternatives](#multi-cluster-client-alternatives)
   for other options considered.

5. **ovnkube handles the rest on spokes**: Once the rendered UDN, policy, and
   route resources land on a spoke cluster, ovnkube-cluster-manager and
   ovnkube-controller process them as normal — no Plexus-specific logic
   needed on spokes.

6. **Cross-cluster connectivity**: The UDNs on each cluster are connected via
   the underlying EVPN fabric. The Plexus controller ensures the same subnets
   exist on each cluster so that workloads like CockroachDB can communicate
   across cluster boundaries on the same network with real, routable pod IPs.

7. **Status and observability**: The Plexus controller on the hub reads the
   status of rendered resources on each spoke and aggregates per-cluster
   health into the AND `.status`. This gives users a single pane
   of glass — the AND resource on the hub reflects whether subnets,
   policies, and routes are healthy across all clusters.

**Pros and cons of hub-only rendering:**

| | |
|---|---|
| **Pro: Simpler spoke footprint** | No Plexus-specific controller to deploy, upgrade, or monitor on spokes. Spokes run standard ovnkube only. |
| **Pro: Single point of logic** | All AND-to-resource translation happens in one place. Easier to debug, audit, and upgrade. |
| **Pro: Easier upgrades** | Upgrade Plexus controller once on the hub, not across N spokes. |
| **Con: Hub is single management point** | If the hub goes down, no new AND changes reach spokes. Existing resources continue to work but drift cannot be corrected until the hub recovers. |
| **Con: Hub must know per-cluster details** | The hub renders cluster-specific resources and holds kubeconfig secrets for every spoke. |
| **Con: No domain-aware self-healing on spokes** | If a resource is deleted on a spoke, recovery depends on the hub controller's reconciliation loop detecting drift. |

#### Identity Assumptions

The multi-cluster model relies on the following "sameness" assumptions
for resource identity across clusters:

1. **Namespace sameness**: If a namespace with the same name exists on
   multiple clusters (created by Plexus as part of the same AND
   subnet), it represents the same logical subnet. Identity is
   determined by namespace name.
2. **UDN name sameness**: If a UDN with the same name exists in the
   same namespace on multiple clusters, it is the same logical
   network. Identity is determined by namespace + UDN name.
3. **Service name sameness**: If a Service with the same name exists
   in the same namespace on multiple clusters, it represents the same
   logical service. EndpointSlice mirroring and Service mirroring are
   keyed by namespace + service name — a Service with a different name
   in the same namespace is a different service, even if it happens to
   select the same pods.

These assumptions are enforced by Plexus: it creates namespaces and
UDNs with deterministic names derived from the AND spec, so the same
AND subnet always produces the same namespace + UDN name on every
cluster. Service identity follows naturally — users deploy Services
into Plexus-managed namespaces, and mirroring uses the namespace +
service name as the key.

#### Availability Zones and Subnet Pinning

In AWS, each subnet resides in a single Availability Zone. We support the
same model: each AND subnet can optionally be tied to an
Availability Zone via the `availabilityZone` field — a nested struct that
groups cluster and node selection together. If omitted, the subnet spans all
clusters and all nodes (no AZ pinning).

The `AvailabilityZone` struct contains two selectors:

- **`clusterSelector`** (required within the struct): selects which
  cluster(s) the subnet is placed on in multi-cluster deployments.
- **`nodeSelector`** (optional): selects which nodes within each target
  cluster the subnet's UDN is scheduled to. This pins the subnet to a
  failure domain. If empty, the entire cluster is the AZ.

The existing Kubernetes label `topology.kubernetes.io/zone` is the natural
fit for `nodeSelector`. Users label their nodes with zone values (e.g.
`rack-a`, `rack-b`) and the subnet's `nodeSelector` matches on that.

For example, the `production` AND might have:

- `web-a` → `availabilityZone: { clusterSelector: region=us-east, nodeSelector: zone=rack-a }`
- `web-b` → `availabilityZone: { clusterSelector: region=us-east, nodeSelector: zone=rack-b }`

This gives AWS-like zone-aware subnet placement for high availability.

**How it works — three layers of pinning:**

1. **Cluster selection** (`clusterSelector`): In multi-cluster deployments,
   the hub Plexus controller determines which spoke clusters a subnet is
   placed on. The hub maintains a cluster inventory — either label-bearing
   cluster objects or kubeconfig secrets with labels. The subnet's
   `clusterSelector` filters against these labels and the hub only creates
   the namespace + UDN on matching clusters.

2. **Node selection** (`nodeSelector`): Within each cluster, the Plexus
   controller sets a node selector on the subnet's namespace to ensure all
   pods schedule on AZ nodes. Two mechanisms are available:
   - **PodNodeSelector admission plugin** (upstream Kubernetes): The
     annotation `scheduler.alpha.kubernetes.io/node-selector` on the
     namespace injects a `nodeSelector` into every pod created in it.
   - Distribution-specific equivalents may also be available (e.g.
     `openshift.io/node-selector` on OpenShift).

3. **Dynamic UDN Node Allocation**: With
   [OKEP-5552](https://ovn-kubernetes.io/okeps/okep-5552-dynamic-udn-node-allocation/)
   enabled, OVN-Kubernetes only renders the UDN on nodes where pods actually
   exist. Combined with the namespace node selector, this means the UDN is
   only configured on AZ nodes — no wasted VTEP/VNI resources, OVN switches,
   or VRFs on irrelevant nodes.

### IPAM

IP address management for AND subnets is handled by an **external IPAM
controller**, not by OVN-Kubernetes's built-in IPAM. This applies to both
single-cluster and multi-cluster deployments — AND subnets use flat Layer 2
addressing (not per-node CIDR slices), so OVN-Kubernetes's built-in IPAM
(designed for per-node subnet allocation) is turned off for AND subnets.

**How it works:**

- Each AND has a pluggable IPAM plugin. The plugin holds one flat
  allocator per subnet.
- The allocator manages the full subnet CIDR as a single flat pool — IPs are
  handed out individually on demand, not pre-partitioned per cluster or per
  node. This avoids the address space wastage that comes with hard-splitting
  CIDRs across clusters.
- When a pod is scheduled, the external IPAM assigns an IP from the
  appropriate subnet pool and **annotates the pod directly**. OVN-Kubernetes reads
  the annotation and programs the datapath accordingly.
- In multi-cluster deployments, the same IPAM instance (or a coordinated
  set of instances) serves all clusters from the same flat pool, ensuring
  global uniqueness without per-cluster CIDR splits.

**Reference implementation:**

A lightweight reference IPAM implementation ships with Plexus — similar in
spirit to a static DHCP server. It is simple, stateless-friendly, and scales
well across clusters. Users can replace it with their own IPAM (e.g.
Infoblox, NetBox, or a custom allocator) by implementing the plugin
interface.

**Example:** A Private subnet `app-a` with CIDR `10.0.10.0/24` spanning two
clusters:

- Pod on Cluster A gets `10.0.10.3` (assigned by IPAM, annotated on pod)
- Pod on Cluster B gets `10.0.10.4` (assigned by IPAM, annotated on pod)
- No per-cluster CIDR split — both draw from the same `/24` pool

Pods communicate with their real IPs across clusters without NAT — important
for applications like CockroachDB that require stable, routable pod
addresses.

## API Details

This enhancement introduces the following API extensions. The API group is
`plexus.io/v1beta1`, hosted in the `ovn-kubernetes/plexus` repository.

### AdministrativeNetworkDomain CRD (new)

A cluster-scoped CRD that defines an isolated network boundary.

```go
// AdministrativeNetworkDomain defines an isolated network boundary that groups one or more
// subnets into a logically isolated network with its own address space and
// automatic intra-domain routing. Plexus translates this intent into
// backend-specific resources (UDNs, CNCs, RouteAdvertisements, etc. for the
// OVN-Kubernetes backend).
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=administrativenetworkdomains,scope=Cluster,shortName=and,singular=administrativenetworkdomain
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type AdministrativeNetworkDomain struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	// +required
	Spec AdministrativeNetworkDomainSpec `json:"spec"`

	// +optional
	Status AdministrativeNetworkDomainStatus `json:"status,omitempty"`
}

// AdministrativeNetworkDomainSpec defines the desired state of an AdministrativeNetworkDomain.
type AdministrativeNetworkDomainSpec struct {
	// subnets defines the subnets within this AdministrativeNetworkDomain. The Plexus
	// controller creates backend-specific network resources for each entry.
	// For the OVN-Kubernetes backend, each subnet becomes a namespace +
	// ClusterUserDefinedNetwork. Subnets within the same AdministrativeNetworkDomain can
	// have different topologies and transports.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +required
	Subnets []Subnet `json:"subnets"`
}

// SubnetType defines the external connectivity class of an AdministrativeNetworkDomain subnet.
// +kubebuilder:validation:Enum=Public;Private;Isolated;VPNOnly
type SubnetType string

const (
	SubnetTypePublic   SubnetType = "Public"
	SubnetTypePrivate  SubnetType = "Private"
	SubnetTypeIsolated SubnetType = "Isolated"
	SubnetTypeVPNOnly  SubnetType = "VPNOnly"
)

// Subnet defines a subnet within the AdministrativeNetworkDomain. The Plexus controller
// creates backend-specific resources from this definition. For the
// OVN-Kubernetes backend, each subnet maps to a namespace + UDN (L2 EVPN).
// Each subnet maps 1:1 to a single namespace.
type Subnet struct {
	// name is the subnet name. For the OVN-Kubernetes backend, the resulting
	// namespace and UDN are named "<networkdomain>-<subnet>".
	//
	// The combined "<networkdomain>-<subnet>" must respect Kubernetes
	// namespace name limits (63 characters, DNS label).
	//
	// +kubebuilder:validation:Required
	// +required
	Name string `json:"name"`

	// cidrs defines the IP address range(s) for this subnet.
	// At most two CIDRs may be specified: one IPv4 and one IPv6
	// for dual-stack. If a single CIDR is provided, the subnet is
	// single-stack (v4 or v6).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +listType=atomic
	// +required
	//
	// +kubebuilder:validation:XValidation:rule="self.size() <= 1 || (self.exists(c, c.contains(':')) && self.exists(c, !c.contains(':')))",message="when two CIDRs are specified they must be from different address families (one IPv4, one IPv6)"
	CIDRs []string `json:"cidrs"`

	// type defines the subnet type which determines its external
	// connectivity and the resources the Plexus controller provisions:
	//
	// - Public: externally reachable. OVN-Kubernetes backend creates
	//   RouteAdvertisements (BGP) to export subnet routes.
	// - Private: no direct external reachability. Outbound traffic is
	//   SNATed using node IPs.
	// - Isolated: no routes outside the AdministrativeNetworkDomain. No
	//   RouteAdvertisements, no SNAT, no intra-domain routing. Traffic
	//   is confined to the subnet.
	// - VPNOnly: traffic exits the AdministrativeNetworkDomain exclusively through a
	//   VPN connection.
	//
	// Defaults to Private if not specified.
	//
	// +optional
	// +kubebuilder:default=Private
	Type SubnetType `json:"type,omitempty"`

	// availabilityZone optionally pins this subnet to a specific
	// failure domain. It selects which clusters and which nodes
	// within those clusters the subnet is placed on.
	//
	// If omitted, the subnet spans all clusters and all nodes
	// (no AZ pinning).
	//
	// +optional
	AvailabilityZone *AvailabilityZone `json:"availabilityZone,omitempty"`
}

// AvailabilityZone selects the failure domain for an AdministrativeNetworkDomain subnet.
type AvailabilityZone struct {
	// clusterSelector selects which clusters this subnet is placed on
	// in multi-cluster deployments. The hub Plexus controller matches
	// these labels against its cluster inventory. Only clusters whose
	// labels satisfy the selector receive the resources for this subnet.
	//
	// +kubebuilder:validation:Required
	// +required
	ClusterSelector metav1.LabelSelector `json:"clusterSelector"`

	// nodeSelector optionally restricts the subnet to nodes matching
	// these labels within each target cluster. The Plexus controller
	// translates this into a namespace node selector so that all pods
	// in the subnet schedule only on matching nodes.
	//
	// Typical use: AZ pinning via
	// { "topology.kubernetes.io/zone": "rack-a" }.
	//
	// If empty or omitted, the subnet spans all nodes in the cluster.
	//
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

// AdministrativeNetworkDomainStatus defines the observed state of an AdministrativeNetworkDomain.
type AdministrativeNetworkDomainStatus struct {
	// conditions reports the status of AdministrativeNetworkDomain operations.
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

**Example AdministrativeNetworkDomain:**

```yaml
apiVersion: plexus.io/v1beta1
kind: AdministrativeNetworkDomain
metadata:
  name: production
spec:
  subnets:
    - name: web-a
      cidrs:
        - 10.0.1.0/24
      type: Public
      availabilityZone:
        clusterSelector:
          matchLabels:
            topology.kubernetes.io/region: us-east
        nodeSelector:
          topology.kubernetes.io/zone: rack-a
    - name: web-b
      cidrs:
        - 10.0.2.0/24
      type: Public
      availabilityZone:
        clusterSelector:
          matchLabels:
            topology.kubernetes.io/region: us-east
    - name: app-a
      cidrs:
        - 10.0.10.0/24
      type: Private
    - name: app-b
      cidrs:
        - 10.0.11.0/24
      type: Private
    - name: db-a
      cidrs:
        - 10.0.20.0/24
      type: Isolated
    - name: db-b
      cidrs:
        - 10.0.21.0/24
      type: Isolated
```

The Plexus controller (OVN-Kubernetes backend) creates six namespaces and UDNs (L2
EVPN) from this: `production-web-a`, `production-web-b`,
`production-app-a`, `production-app-b`, `production-db-a`, and
`production-db-b`. The `type` field drives what additional resources the
controller creates per subnet:

- **production-web-a**, **production-web-b** (Public): RouteAdvertisements
  for BGP export
- **production-app-a**, **production-app-b** (Private): node IP SNAT for
  outbound traffic
- **production-db-a**, **production-db-b** (Isolated): no external routing,
  no intra-domain routing

### Subnet Immutability

A subnet's CIDR is immutable once created. The underlying UDN spec in
OVN-Kubernetes does not support CIDR changes after creation, and this
constraint propagates to the AND API. This matches AWS behavior — an
AWS subnet cannot be resized; you delete it and create a new one with a
different CIDR.
**TODO(tssurya)**: not sure about this framing, needs discussion.

The AND spec itself is mutable as a container:

- **Add subnets**: Append new entries to `spec.subnets`. The Plexus
  controller creates the backend resources.
- **Remove subnets**: Remove entries from `spec.subnets`. The Plexus
  controller deletes the backend resources (and all workloads in that
  namespace for the OVN-Kubernetes backend).
- **Reorder subnets**: No effect — subnets are keyed by `name`, not
  position.

The `type` field (Public/Private/Isolated/VPNOnly) is also immutable after
creation. Switching types would change the resources the controller
provisions and has traffic-disruptive side effects. To change a subnet's
type, delete it and recreate it.

### RouteTable CRD (new, separate OKEP)

A new **RouteTable CRD** will replace the existing
[`AdminPolicyBasedExternalRoute`](https://ovn-kubernetes.io/api-reference/admin-epbr-api-spec/)
(APBR) API in OVN-Kubernetes. The RouteTable CRD will:

- Support both **egress and ingress** custom routes (APBR is egress-only).
- Work on **UDNs**, not just the cluster default network.
- Use a `networkSelector` to select which UDNs the routes apply to.
- Allow injection of custom static routes into the per-UDN VRF (LGW) or
  GR (SGW).

The scope and API of this CRD need further design work in a dedicated OKEP.

```yaml
apiVersion: k8s.ovn.org/v1beta1
kind: RouteTable
metadata:
  name: production-custom-routes
  labels:
    plexus.io/network-domain: production
spec:
  networkSelector:
    matchLabels:
      plexus.io/network-domain: production
      plexus.io/subnet-type: Private
  routes:
    - destination: 172.16.0.0/12
      nextHop: 10.0.10.1
      description: "Route to on-prem data center"
    - destination: 10.1.0.0/16
      nextHop: 10.0.10.254
      description: "Route to peered AND gateway"
```

## Implementation Details

### Workflow: CLI Plugin (`kubectl plexus`)

The CLI is implemented as a
[kubectl plugin](https://kubernetes.io/docs/tasks/extend-kubectl/kubectl-plugins/)
— a standalone Go binary named `kubectl-plexus` that lives in the user's
`$PATH`. It will be hosted in the `ovn-kubernetes/plexus` repository.

**Commands:**

| Command | Description |
|---|---|
| `kubectl plexus create <name>` | Create an empty AND |
| `kubectl plexus delete <name>` | Delete an AND and all its subnets |
| `kubectl plexus get` | List all ANDs with subnet count and status |
| `kubectl plexus describe <name>` | Show details: subnets, CIDRs, types, AZs, status |
| `kubectl plexus add-subnet <nd> <subnet>` | Add a subnet. Flags: `--cidr`, `--type`, `--availability-zone` |
| `kubectl plexus remove-subnet <nd> <subnet>` | Remove a subnet from an AND |

Each CLI command translates to an AND CR mutation — `kubectl plexus
create` creates the CR, `kubectl plexus add-subnet` patches `spec.subnets`,
and so on. The CLI reads back `.status` to show health and readiness.

The plugin is distributed via [krew](https://krew.sigs.k8s.io/) and as a
standalone download from the repository's release artifacts.

### Workflow: Web UI Plugin

The web UI plugin adds an AND management page. On clusters with a
web console that supports dynamic plugins, it integrates under
**Networking > AdministrativeNetworkDomains**. Otherwise it runs as a standalone web
application.

**Pages and features:**

| Page | Description |
|---|---|
| **AND List** | Table of all ANDs showing name, subnet count, status. Actions: Create, Delete. |
| **AND Detail** | Subnet table with CIDR, type, AZ, and status. Links to namespaces. Topology diagram showing subnets and connectivity. |
| **Create AND** | Wizard: name → add subnets (CIDR, type, optional AZ) → review → create. CIDR validation runs client-side. |
| **Add Subnet** | Modal form to add a subnet to an existing AND. |

The UI plugin is built with **React** and **PatternFly** and is hosted in
the `ovn-kubernetes/plexus` repository alongside the controller and CLI.

### Plexus Controller Architecture

The Plexus controller is a standard `controller-runtime` reconciler deployed
as a Deployment on the management cluster (or single cluster in single-cluster
mode). It can be deployed via Helm, kustomize, or OLM.

```
┌─────────────────────────────────────────────────────────────┐
│                    Management Cluster                       │
│                                                             │
│  ┌────────────────────────────────────────────────────────┐ │
│  │ kubectl plexus create / add-subnet / describe          │ │
│  │ (kubectl plugin binary)                                │ │
│  └──────────────────────┬─────────────────────────────────┘ │
│                         │ client-go                         │
│                         ▼                                   │
│  ┌────────────────────────────────────────────────────────┐ │
│  │ Kubernetes API Server                                  │ │
│  │ (AND CRD registered — management-plane only)           │ │
│  └──────────────────────┬─────────────────────────────────┘ │
│                         │                                   │
│                         ▼                                   │
│  ┌────────────────────────────────────────────────────────┐ │
│  │ Plexus Controller                                      │ │
│  │ (reconciles AND CR → backend resources)                │ │
│  │                                                        │ │
│  │  ┌──────────────────────────────────────────────────┐  │ │
│  │  │ Backend Plugin Interface                         │  │ │
│  │  │  ├── OVN-Kubernetes Backend (ns, UDNs, CNCs, RA) │  │ │
│  │  │  └── (future backends)                           │  │ │
│  │  └──────────────────────────────────────────────────┘  │ │
│  │                                                        │ │
│  │  ┌──────────────────────────────────────────────────┐  │ │
│  │  │ multicluster-runtime (secret Provider)           │  │ │
│  │  │  Discovers spoke clusters via labeled Secrets    │  │ │
│  │  │  (plexus.io/cluster: "true")                     │  │ │
│  │  │  Manages per-spoke client lifecycle              │  │ │
│  │  └────────┬─────────────────────────┬───────────────┘  │ │
│  └───────────┼─────────────────────────┼──────────────────┘ │
│              │                         │                    │
│  ┌───────────┴──────────┐ ┌────────────┴─────────┐          │
│  │ Secret: cluster-a    │ │ Secret: cluster-b    │          │
│  │ plexus.io/cluster    │ │ plexus.io/cluster    │          │
│  │ (kubeconfig data)    │ │ (kubeconfig data)    │          │
│  └───────────┬──────────┘ └────────────┬─────────┘          │
│              │                         │                    │
└──────────────┼─────────────────────────┼────────────────────┘
               │ push resources          │ push resources
               ▼                         ▼
      ┌──────────────────┐      ┌──────────────────┐
      │ Spoke Cluster A  │      │ Spoke Cluster B  │
      │ (OVN-K only,     │      │ (OVN-K only,     │
      │  no AND CRD)     │      │  no AND CRD)     │
      └──────────────────┘      └──────────────────┘
```

### Plexus Controller Configuration

The Plexus controller reads its runtime configuration from a
`PlexusControllerConfig` singleton CR on the management cluster. This CR
captures global settings that apply to the controller itself — not to
individual ANDs.

```yaml
apiVersion: plexus.io/v1alpha1
kind: PlexusControllerConfig
metadata:
  name: default
spec:
  # IPAM plugin configuration.
  ipam:
    plugin: built-in          # "built-in" | "infoblox" | "custom"

  # Namespace where the Plexus controller runs.
  namespace: plexus-system

  # Cluster discovery: label selector for kubeconfig Secrets.
  # multicluster-runtime watches Secrets matching this label.
  clusterDiscovery:
    secretLabel: plexus.io/cluster=true

  # Which backend plugin to use.
  # Only "ovn-kubernetes" is supported initially.
  # TODO: In multi-cluster deployments with heterogeneous backends
  # (e.g., one spoke on OVN-K, another on AWS VPC), this may need
  # to be per-cluster rather than a global setting. Determine
  # whether backend selection should move to the kubeconfig Secret
  # labels or a per-cluster config stanza.
  backend: ovn-kubernetes

  # Backend-specific configuration, keyed by backend name.
  # Each backend gets its own section; only the active backend's
  # config is read. Future backends (e.g., "aws-vpc") add their
  # own key without polluting the shared top-level config.
  backendConfig:
    ovn-kubernetes:
      # FRRConfiguration settings (OVN-K uses FRR for BGP peering).
      frrConfiguration:
        # Whether to auto-create FRRConfiguration CRs on spoke clusters
        # when BGP peering is not already configured.
        # TODO: Explore whether Plexus should auto-create FRRConfiguration
        # CRs as part of cluster onboarding, removing the manual
        # prerequisite step for operators.
        autoCreate: false         # default: false (user pre-creates)
        # Label selector for the FRRConfiguration CRs that Plexus should
        # reference when creating RouteAdvertisements. The user pre-creates
        # FRRConfiguration(s) with this label; Plexus passes it through
        # to the RA's frrConfigurationSelector field.
        selector:
          matchLabels:
            plexus.io/frr: "true"
```

**Key fields:**

- **`backend`**: Selects the backend plugin. Only `ovn-kubernetes` is
  in scope; the field exists so the interface is explicit from the start.
- **`ipam.plugin`**: Which IPAM implementation to use. The built-in
  reference implementation ships with Plexus; users can swap it for
  Infoblox, NetBox, or a custom allocator.
- **`clusterDiscovery.secretLabel`**: The label that multicluster-runtime's
  `secret` Provider uses to discover spoke cluster kubeconfig Secrets.
  Metadata labels on the Secret (e.g., `plexus.io/region`,
  `topology.kubernetes.io/zone`) serve as the cluster inventory for
  `clusterSelector` matching.
- **`backendConfig`**: Backend-specific configuration, keyed by backend
  name. Only the section matching `backend` is read. Future backends add
  their own key (e.g., `backendConfig.aws-vpc`) without polluting the
  shared top-level config.
- **`backendConfig.ovn-kubernetes.frrConfiguration.autoCreate`**: When
  `true`, the Plexus controller auto-creates `FRRConfiguration` CRs on
  spoke clusters that lack BGP peering configuration. When `false`
  (default), the operator must pre-create FRRConfiguration on each
  cluster.
- **`backendConfig.ovn-kubernetes.frrConfiguration.selector`**: Label
  selector matching the FRRConfiguration CR(s) that Plexus should
  reference. When creating RouteAdvertisements, the controller copies
  this selector into the RA's `frrConfigurationSelector` field. The user
  must label their pre-created FRRConfiguration(s) accordingly (e.g.,
  `plexus.io/frr: "true"`).

### OVN-Kubernetes Backend: Subnets (UDNs)

Each AdministrativeNetworkDomain subnet maps to a namespace + UDN (L2 EVPN).
The Plexus controller currently creates ClusterUserDefinedNetworks (CUDNs) for
each subnet. Namespace-scoped primary UDNs are a viable alternative once
RouteAdvertisements supports selecting them
([#6604](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6604)) — this
would allow the controller to create a namespace-scoped UDN directly instead of
a cluster-scoped CUDN with a single-namespace selector. The L3 and localnet
type topologies in the UDN world along with L2 GENEVE type topology and BGP
no-overlay topology will not be supported via ANDs or revealed to the users in
the initial implementation.

**OVN-Kubernetes mapping:**

| AWS Subnet property | UDN equivalent |
|---|---|
| VpcId (parent VPC) | Plexus controller creates the namespace + UDN — ownership tracked via `plexus.io/network-domain` label |
| CidrBlock | `spec.network.layer2.subnets[]` |
| AvailabilityZone | `topology.kubernetes.io/zone` node labels on bare metal |
| Immutable CIDR | Same — UDN CIDR is immutable once created |
| Route table association | Determined by `type` field; controller creates ancillary resources |

**Key difference from AWS:** In AWS, a subnet's external connectivity is
determined by what you put in its route table (IGW route = public, NAT GW
route = private, nothing = isolated). In the AND CRD, this is
captured declaratively by the `type` field, and the Plexus controller
creates the appropriate resources:

| `type` | External reachability | Controller creates |
|---|---|---|
| **Public** | Externally routable — subnet routes advertised via BGP | RouteAdvertisements (see Internet Gateway section) |
| **Private** | Outbound only — pods can reach external destinations | Node IP SNAT (EgressIP in future) |
| **Isolated** | None — no external routes, no intra-domain routing | (nothing) |
| **VPNOnly** | VPN only — traffic exits via encrypted tunnel | IPsec north-south encryption |

All non-Isolated subnets get automatic **intra-domain routing** between each
other.

**EVPN Transport and VTEP:**

All AND subnets use EVPN as the transport (`transport: EVPN`). The
Plexus controller creates (or reuses) a single shared
[VTEP](https://ovn-kubernetes.io/okeps/okep-5088-evpn/#vtep-crd) CR in
**unmanaged mode** using the node CIDR — node IPs serve as VTEP IPs, so no
additional loopback IP allocation is required. Each subnet UDN references
this shared VTEP.

**TODO**: The single shared VTEP design means all ANDs share the
same VNI namespace. With SVD (Single VXLAN Device), FRR maps VNIs to VLANs
on a single bridge, capping at ~4094 MAC+IP VRFs per node. For large
deployments with many ANDs and subnets, a per-AND VTEP
design (using dedicated managed loopback CIDRs) or multiple SVD bridge/VTEP
pairs may be needed. Determine the right approach during implementation.

**macVRF and ipVRF per subnet type:**

Every AND subnet gets a `macVRF` (L2 stretch via EVPN VXLAN). The
`ipVRF` (L3 routing domain, EVPN Type 5 IP prefix routes) is only added for
subnets that need external routability:

| Subnet type | macVRF | ipVRF | North-south behavior |
|---|---|---|---|
| **Public** | Yes (L2 stretch, unique VNI) | Yes (exports Type 5 routes for pod CIDRs, imports external routes) | Bidirectional — external entities reach pods via Type 5 routes advertised by RouteAdvertisements |
| **Private** | Yes (L2 stretch, unique VNI) | No | Outbound only — north-south via UDN's own OVN routing + node IP SNAT. No routes exported to EVPN fabric. |
| **Isolated** | Yes (L2 stretch, unique VNI) | No | None — no external routes, no SNAT |

A macVRF is purely Layer 2 — it has no concept of IP routing, no route
import/export, no default gateway. External reachability requires an ipVRF
which advertises Type 5 (IP prefix) routes and can import routes from the
spine/fabric. For Public subnets, the RouteAdvertisements CRD triggers the
RA controller to generate FRR-K8S configuration that activates the ipVRF
with `advertise ipv4 unicast` under `address-family l2vpn evpn`.

**New capability required:** The current
[EVPN OKEP](https://ovn-kubernetes.io/okeps/okep-5088-evpn/) requires a
RouteAdvertisements CR for EVPN to function — the RA controller is what
generates the FRR-K8S configuration that activates EVPN on the node. This
means there is no way today to enable EVPN east-west (macVRF) without also
advertising IP routes (ipVRF / Type 5). This requires extending the RA
controller (or the UDN API) to support an **east-west-only EVPN mode** that
configures FRR for macVRF advertisement (Type 2/3 routes) without ipVRF
(no Type 5 routes). See
[ovn-kubernetes#6631](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6631)
for the proposed enhancement.

- **Private subnets**: These require ipVRF for intra-domain route leaking
  (shared route target allows inter-subnet L3 reachability) but must NOT
  advertise Type 5 routes externally. The current workaround is for the
  Plexus controller to create its own FRRConfiguration CR configuring the
  Private subnet ipVRFs with route-target import/export but WITHOUT
  `advertise ipv4 unicast`. Route leaking works via Type 2 host routes
  carrying the shared ipVRF route-target (symmetric IRB). The RA selects
  only Public subnets, and `advertise-all-vni` from the Public subnet RA
  already covers Private subnet macVRF VNIs. **Known limitation:**
  `TransportAccepted` stays in `False` state for Private subnets because
  no RouteAdvertisements CR selects them — this is a cosmetic issue that
  does not affect data-plane functionality but will be resolved when
  [ovn-kubernetes#6631](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6631)
  is implemented. North-south for Private subnets uses UDN's own OVN
  north-south routing + node IP SNAT for outbound.
- **Isolated subnets**: east-west-only EVPN (macVRF), all north-south
  disabled — no SNAT, pure L2 stretch only. No ipVRF is configured.

For all non-Isolated subnets, the Plexus controller also programs
**intra-domain routes** so that subnets within the same AND can
reach each other. The `type` field only governs external (north-south)
reachability; east-west traffic within the AND is always permitted.

**Intra-domain routing: Route Leaking vs CNC**

Today, [CNC (ClusterNetworkConnect)](https://ovn-kubernetes.io/okeps/okep-5224-connecting-udns/okep-5224-connecting-udns/)
connects CUDNs by creating an OVN **connect-router** that links the
network routers of the connected UDNs via patch ports. This works for
Geneve-based CUDNs where OVN controls the entire datapath. However, CNC
cannot be used as-is to connect EVPN-based CUDNs:

- EVPN subnets are stretched across physical networks via VXLAN — their
  forwarding is split between OVN (local) and FRR/the EVPN fabric
  (cross-node). CNC's OVN-only connect-router does not participate in
  the EVPN control plane.
- Each EVPN CUDN has its own VRF (ipVRF). Connecting two EVPN subnets
  requires **inter-VRF route leaking** — importing one VRF's routes into
  another — which is a BGP/FRR concept, not an OVN concept.

For EVPN-based AND subnets, intra-domain connectivity is achieved via
**route leaking between ipVRFs**:

1. All non-Isolated subnets within the same AND share a common route
   target (or import each other's route targets).
2. FRR is configured to import the shared route target into each subnet's
   ipVRF, making all subnet CIDRs reachable from every other subnet in the
   domain.
3. Isolated subnets are excluded — their ipVRF (if present) does not
   import the shared route target, keeping them unreachable from other
   subnets.

**New capability required:** OVN-Kubernetes does not currently support
inter-VRF route leaking. The Plexus controller (or an extension to the
RouteAdvertisements / UDN API) must be able to configure FRR route target
import/export rules per VRF. This is a prerequisite for automatic
intra-domain routing in EVPN mode. The preferred approach is to use
**CNC as the unified API** for expressing connectivity intent across
both transports — for Geneve, CNC creates an OVN connect-router
(existing); for EVPN, CNC triggers route leaking between VRFs (new).
This keeps a single API for the Plexus controller to express
"connect these subnets" regardless of transport. Service reachability
follows from the connectivity — once CNC connects two subnets, each
network's OVN load balancers are added to the other network's
switches, enabling local DNAT to backend pod IPs which are then
routed via the connect-router.

**Short-term (implemented):** The UDN VRFConfig API already exposes an
optional `routeTarget` field. The Plexus controller achieves pod-to-pod L3
reachability across EVPN subnets by setting a shared route target on each
non-Isolated subnet's ipVRF (a deterministic hash of the AND name in
`*:<local-admin>` format). For Public subnets, the RA controller handles
FRR configuration natively (including `advertise ipv4 unicast` for Type 5
external advertisement). For Private subnets, the Plexus controller creates
a separate FRRConfiguration CR that configures VRF-VNI mapping and
route-target import/export WITHOUT `advertise ipv4 unicast` — this enables
route leaking via Type 2 routes (symmetric IRB) while keeping Private
subnets unreachable from the external fabric. See
[ovn-kubernetes#6631](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6631)
for the long-term fix that would allow the RA controller to natively support
this use case. Cross-subnet ClusterIP service access (which requires OVN LB
cross-attachment) will not be available until CNC is extended for EVPN.

| Connectivity mechanism | Transport | How it works |
|---|---|---|
| CNC (existing) | Geneve | OVN connect-router connects network routers via patch ports |
| CNC (extended) | EVPN | Route leaking — shared route target imported across ipVRFs via FRR |

**EVPN VRF configuration — VNI, Route Target, and AS allocation:**

Each subnet UDN requires EVPN VRF configuration. Per the
[VRFConfig API](https://ovn-kubernetes.io/api-reference/userdefinednetwork-api-spec/#vrfconfig),
each VRF (macVRF and optionally ipVRF) needs:

- **VNI** (required): A 24-bit Virtual Network Identifier, unique across all
  EVPN configurations in the cluster. Public and Private subnets need two
  VNIs each (one for macVRF, one for ipVRF — Private subnets need ipVRF
  for intra-domain route leaking); Isolated subnets need one (macVRF only).
- **Route Target** (optional): A BGP extended community in the format
  `<AS>:<local-admin>` that controls route import/export between VRFs. If
  omitted, OVN-Kubernetes omits the `route-target` lines from the generated
  FRR configuration and FRR auto-derives the route target as `<AS>:<VNI>`.
  The route target format follows RFC 4360 (6-byte extended community):
  `EF:OPQR` (2-byte AS : 4-byte local-admin) or `GHJK:MN` (4-byte AS :
  2-byte local-admin). When FRR auto-generates, it uses 2-byte
  local-admin format if the AS is 4-byte, and vice versa, to fit the
  6-byte constraint.

**Prerequisite: FRRConfiguration CR.** BGP/EVPN clusters require a
pre-existing
[FRRConfiguration](https://ovn-kubernetes.io/features/bgp-integration/evpn/#step-1-configure-bgp-peering)
CR (FRR-K8s API) that defines the BGP peering — including the ASN and
neighbor addresses. This is Step 1 of any EVPN setup and is created by the
cluster operator. The RA controller reads the ASN from this CR
(`spec.bgp.routers[].asn`) and writes it into the generated FRR config as
`router bgp <ASN>`. FRR then uses this ASN for route target auto-generation.
The Plexus controller does not need to discover or configure the ASN — it is
already present in the FRRConfiguration CR.

This means the Plexus controller must:

1. **Allocate VNIs**: Assign a unique VNI per VRF across all subnets in
   all ANDs. The controller needs a VNI allocator that tracks
   which VNIs are in use and avoids collisions. For an AND with 6
   subnets (2 Public, 2 Private, 2 Isolated), the controller allocates 10
   VNIs (2 Public x 2 VNIs each + 2 Private x 2 VNIs each + 2 Isolated
   x 1). Private subnets need ipVRF VNIs for intra-domain route leaking.

   **TODO (Shared L3VNI design)**: The current approach allocates a separate
   ipVRF VNI per subnet and uses RT-based route leaking to connect subnets
   within the same AND. This deviates from the industry-standard EVPN
   Symmetric IRB model where multiple subnets (L2VNIs) share a single L3VNI
   per tenant/VRF. In that model, all subnets in the same AND would share
   one ipVRF VNI, placing them in the same Linux VRF — eliminating the need
   for route leaking entirely. OVN-Kubernetes currently enforces VNI
   uniqueness per VTEP (rejecting duplicate ipVRF VNIs), which prevents this
   design. A future OVN-K enhancement to support shared L3VNIs would
   simplify intra-VPC routing significantly and align with how datacenter
   EVPN fabrics operate (RFC 9135 Symmetric IRB, one L3VNI per VRF). See
   [issue #6607](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6607).

2. **Ensure route target consistency in multi-cluster**: When a
   AND spans multiple clusters, the same subnet must use the same
   VNI and route target on every cluster so that the EVPN fabric correctly
   imports/exports routes between them. The hub Plexus controller is
   responsible for allocating VNIs and route targets centrally and
   propagating them to all spokes.

**TODO**: Determine whether VNI allocation is global (single counter across
all ANDs) or per-AND (each domain gets a VNI range).

#### Future: Supporting Other Transport Types

The initial implementation assumes EVPN as the transport for all AND
subnets. In the future, Plexus may need to support additional transport
types — Geneve overlay, Layer 3 (per-node subnets with distributed
routing), and no-overlay (pure BGP routing without encapsulation). There
are several options for how Plexus would learn which transport to use:

1. **Transport field on the AND or subnet spec.** The AND CRD could
   expose a `transport` field (e.g., `evpn`, `geneve`, `no-overlay`) at
   the AND level or per-subnet. Plexus would create the appropriate UDN
   topology and transport configuration based on this field. This is the
   most explicit approach but adds API surface.

2. **Infer from the PlexusControllerConfig.** A global or per-cluster
   default transport could be set in the `PlexusControllerConfig` CR.
   ANDs that don't specify a transport would inherit the default. This
   keeps the AND API simple but limits flexibility within a single
   cluster.

3. **Infer from cluster capabilities.** The Plexus controller could
   detect what transports are available on a spoke cluster (e.g., whether
   EVPN is configured via FRRConfiguration, whether Geneve is the
   default) and select the appropriate transport automatically. This is
   the most transparent but hardest to implement.

This is captured in [Future Goals](#future-goals) and does not affect the
initial EVPN-only implementation.

### OVN-Kubernetes Backend: Internet Gateway (Public Subnets)

In AWS, a public subnet has internet access because of three cooperating
resources:

1. **Internet Gateway (IGW)** — attached to the VPC (one per VPC). Provides
   bidirectional internet connectivity.
2. **Route** — an entry in the *public* subnet's route table:
   `0.0.0.0/0 → igw-id`.
3. **Public IP** — each instance in the public subnet gets a public IP
   (auto-assigned or an Elastic IP) that the IGW maps 1:1 to the private IP.

```
 internet ──► IGW ──► public pod (1:1 NAT to public IP)
 public pod ──► route table (0.0.0.0/0 → IGW) ──► IGW ──► internet
```

The IGW is stateless and bidirectional — external hosts can initiate
connections to the pod's public IP, and pods can initiate connections to
external hosts. There is no SNAT; the pod's public IP is its identity on the
internet.

**OVN-Kubernetes mapping: RouteAdvertisements**

In OVN-Kubernetes there is no separate "gateway" appliance. Instead,
RouteAdvertisements export the subnet's pod routes via BGP to the physical
network, making pods directly routable from outside the cluster:

| AWS Resource | OVN-Kubernetes equivalent |
|---|---|
| Internet Gateway (bidirectional internet) | RouteAdvertisements — BGP announces pod CIDRs to the physical fabric |
| Public IP (1:1 NAT) | Pod IP is the routable IP — no NAT needed when routes are advertised |
| Route `0.0.0.0/0 → IGW` | Physical network has return routes to pod CIDRs via BGP |
| IGW is VPC-scoped (one per VPC) | RouteAdvertisements can be AND-scoped — a single resource with `networkSelector` matching all public UDNs |

For an AND with public subnets, the Plexus controller creates a
namespace and UDN for each public subnet, plus a single RouteAdvertisements
that selects all of them:

```yaml
# Public subnet UDN: production-web-a
# macVRF (L2 stretch) + ipVRF (Type 5 routes)
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: web-a
  namespace: production-web-a
  labels:
    plexus.io/network-domain: production
    plexus.io/subnet: web-a
    plexus.io/subnet-type: Public
spec:
  topology: Layer2
  layer2:
    role: Primary
    subnets:
      - "10.0.1.0/24"
    transport: EVPN
    evpnConfiguration:
      vtep: nd-vtep
      macVRF:
        vni: 100
      ipVRF:
        vni: 101
```

A single RouteAdvertisements resource covers all public subnets:

```yaml
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: production-public
  labels:
    plexus.io/network-domain: production
spec:
  advertisements:
    - PodNetwork
  networkSelectors:
    - networkSelectionType: ClusterUserDefinedNetworks
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            plexus.io/network-domain: production
            plexus.io/subnet-type: Public
  nodeSelector:
    matchLabels: {}
  frrConfigurationSelector:
    matchLabels:
      plexus.io/frr: "true"
```

When a new public subnet is added, the Plexus controller labels its UDN with
`plexus.io/subnet-type: Public` and the existing RouteAdvertisements
automatically picks it up — no new RA resource needed. The physical network
learns routes to each public subnet's CIDR via BGP peering with the cluster
nodes. External hosts can reach pods directly by their pod IPs, and pods can
reach external hosts — bidirectional, just like an AWS IGW.

**Comparisons with AWS:**

- **No separate gateway resource.** There is no "IGW appliance" to create
  and attach. RouteAdvertisements configures the cluster's BGP speakers to
  announce pod routes — the physical fabric *is* the gateway.
- **No public IP allocation.** In AWS each instance needs a public IP that
  the IGW maps 1:1. With BGP route advertisement the pod IP itself is
  routable on the physical network — no NAT, no public IP allocation.
- **AND-scoped, like AWS.** A single RouteAdvertisements resource
  with a `networkSelector` matching `plexus.io/subnet-type: Public`
  covers all public subnets in the AND — mirroring the
  one-IGW-per-VPC model. Per-subnet RouteAdvertisements are also possible
  if finer control is needed.
- **Depends on physical network.** BGP peering must be configured between
  the cluster nodes and the physical fabric (ToR switches). This is an
  infrastructure prerequisite that has no AWS equivalent (AWS manages the
  physical network).

### OVN-Kubernetes Backend: NAT Gateway (Private Subnets)

In AWS, a private subnet reaches the internet through three cooperating
resources:

1. **Elastic IP (EIP)** — a static public IP address.
2. **NAT Gateway** — placed in a *public* subnet, associated with the EIP.
   Performs SNAT for outbound traffic.
3. **Route** — an entry in the *private* subnet's route table:
   `0.0.0.0/0 → nat-gateway-id`.

```
 private pod ──► private route table ──► NAT GW (in public subnet) ──► IGW ──► internet
                 0.0.0.0/0 → NAT GW      SNAT to EIP
```

**OVN-Kubernetes mapping: Node IP SNAT (EgressIP in future)**

Private subnets use **macVRF-only EVPN** — east-west traffic (pod-to-pod
within the subnet across nodes) travels over the EVPN L2 stretch, while
north-south traffic (pod-to-internet) is handled entirely by OVN, not
the EVPN fabric:

```
East-west (within subnet, across nodes):
  pod ──► OVN logical switch ──► macVRF (EVPN VXLAN) ──► remote node ──► pod

North-south (outbound to internet):
  pod ──► OVN logical switch ──► OVN logical router ──► ovn-k8s-mpx
      ──► UDN's Linux VRF ──► SNAT to node IP ──► internet
```

No ipVRF is configured — no Type 5 routes are advertised, so external
entities cannot route traffic into the subnet. The subnet is not visible
on the EVPN fabric at L3. Outbound traffic exits via OVN's default egress
path: the packet traverses the UDN's own OVN logical router, exits via
ovn-k8s-mpx into the UDN's Linux VRF, and is SNATed to the node's IP.

| AWS Resource | OVN-Kubernetes equivalent (initial) | OVN-Kubernetes equivalent (future: EgressIP) |
|---|---|---|
| Elastic IP (static public IP) | Node IP (not stable across reschedule) | `spec.egressIPs: ["192.168.1.100"]` |
| NAT Gateway (SNAT engine) | OVN SNAT to node IP via UDN's own routing | OVN SNAT on EgressIP-hosting node |
| NAT Gateway AZ placement | Determined by pod scheduling | Node labels (`k8s.ovn.org/egress-assignable`, `topology.kubernetes.io/zone`) |
| Route `0.0.0.0/0 → NAT GW` | Implicit — OVN default egress via UDN's routing | Implicit — OVN-Kubernetes routes via EgressIP node |

```yaml
# Private subnet UDN: production-app-a
# macVRF + ipVRF (ipVRF needed for intra-domain route leaking).
# North-south via UDN's own OVN routing + node IP SNAT.
# ipVRF route-target enables route leaking between subnets in the same AND.
# A separate Plexus-managed FRRConfiguration configures this ipVRF in FRR
# WITHOUT "advertise ipv4 unicast" (no Type 5 external advertisement).
# See https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6631
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: app-a
  namespace: production-app-a
  labels:
    plexus.io/network-domain: production
    plexus.io/subnet: app-a
    plexus.io/subnet-type: Private
spec:
  topology: Layer2
  layer2:
    role: Primary
    subnets:
      - "10.0.10.0/24"
    transport: EVPN
    evpnConfiguration:
      vtep: nd-vtep
      macVRF:
        vni: 200
      ipVRF:
        vni: 201
        routeTarget: "*:3827164590"  # shared per-AND hash
```

Outbound traffic from pods in this subnet is SNATed using the node's IP
address — this is OVN-Kubernetes's default egress behavior. No additional
resource (RouteAdvertisements, EgressIP) is needed for the initial
implementation — the Plexus controller only creates the namespace and UDN.

> **Note:** `TransportAccepted` will remain `False` for Private subnets
> because no RouteAdvertisements CR selects them (selecting them would cause
> the RA controller to generate `advertise ipv4 unicast`, making the subnet
> externally routable). This is a cosmetic status issue — the EVPN data
> plane still functions because `advertise-all-vni` from the Public subnet's
> RA covers all VNIs, and the Plexus-managed FRRConfiguration handles the
> ipVRF route-target configuration. This limitation will be resolved by
> [ovn-kubernetes#6631](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6631).

**Future: EgressIP-based SNAT.** A future iteration will introduce EgressIP
resources for private subnets to provide dedicated, stable egress addresses
(see [Future Goals](#future-goals)). This would allow firewall allowlisting,
audit trails, and per-AZ egress affinity — mirroring how AWS places a NAT
Gateway with a dedicated Elastic IP in a specific AZ.

**Comparisons with AWS:**

- **No separate gateway resource.** Node IP SNAT via the UDN's own OVN
  routing (and future EgressIP) replaces the entire NAT Gateway construct.
  There is no intermediate hop through a "public subnet" — SNAT happens
  on the node.
- **No explicit route.** The north-south path through the UDN's logical
  router and Linux VRF is implicit in OVN-Kubernetes rather than being a discrete
  route table entry.
- **Simpler day-2.** No NAT Gateway to create, place in an AZ, and wire
  into route tables. The AND `type: Private` declaration handles
  everything.

### OVN-Kubernetes Backend: Route Table (Custom Routes)

The `type` field on the AND subnet replaces the default-route
patterns that dominate AWS route tables:

- `type: Public` → RouteAdvertisements (no route to an IGW — BGP advertises
  pod routes directly)
- `type: Private` → Node IP SNAT (no route to a NAT GW — OVN performs SNAT
  internally; EgressIP in future)
- `type: Isolated` → nothing (traffic stays in the subnet by default)

The routing infrastructure already exists — every UDN gets an OVN logical
router (Linux VRF in LGW mode, Gateway Router in SGW mode). What doesn't
exist today is a user-facing API to program **custom route entries** into
those routers for advanced use cases:

- Static routes to on-prem networks (e.g. `172.16.0.0/12` via next-hop)
- Routes to peered AND gateways
- Traffic engineering overrides

The closest existing construct is
[`AdminPolicyBasedExternalRoute`](https://ovn-kubernetes.io/api-reference/admin-epbr-api-spec/)
(Multiple External Gateways / ICNI2), which allows admins to define static
or dynamic next-hop gateways for egress traffic from selected namespaces.
However, APBR is limited to the cluster default network (not supported on
UDNs per the
[UDN OKEP](https://ovn-kubernetes.io/okeps/okep-5193-user-defined-networks/))
and only configures external gateway hops — not arbitrary static routes
within a VRF.

**Implementation:** Using the RouteTable YAML from the
[API Details](#routetable-crd-new-separate-okep) section as an example, the
routes `172.16.0.0/12 via 10.0.10.1` and `10.1.0.0/16 via 10.0.10.254`
would be programmed as follows depending on the gateway mode:

- **Local Gateway Mode (LGW):** Routes are installed as `ip route` entries
  in the UDN's Linux VRF. For every node where the selected UDNs are
  present, the controller adds:
  ```
  ip route add 172.16.0.0/12 via 10.0.10.1 vrf production-app-a
  ip route add 10.1.0.0/16 via 10.0.10.254 vrf production-app-a
  ```
  Traffic from pods in the Private subnets hitting these prefixes is routed
  through the VRF to the specified next-hops instead of taking the default
  egress path.

- **Shared Gateway Mode (SGW):** Routes are installed as OVN logical router
  static routes on the UDN's Gateway Router (GR). For each selected UDN,
  the controller creates:
  ```
  ovn-nbctl lr-route-add GR-production-app-a 172.16.0.0/12 10.0.10.1
  ovn-nbctl lr-route-add GR-production-app-a 10.1.0.0/16 10.0.10.254
  ```
  OVN handles forwarding matching traffic to the specified next-hops before
  the default SNAT/egress path.

### OVN-Kubernetes Backend: Security Groups / Network ACLs

NetworkPolicy is scoped to the subnet (namespace + UDN) — policies within a
subnet apply to pods in that subnet. The peers in a NetworkPolicy can
reference pods in other subnets within the same AND.

For AND-level policies (applying across all subnets in an
AND), AdminNetworkPolicy (ANP), BaselineAdminNetworkPolicy (BANP),
and ClusterNetworkPolicy (CNP) are used. These cluster-scoped policies can
select namespaces by AND labels (e.g.
`plexus.io/network-domain: production`) to apply consistent security
posture across the entire AND.

**Templating for policy at scale:** Defining per-subnet NetworkPolicies
manually is tedious when an AND has many subnets. Two new CRDs are
under consideration — TemplatedNetworkPolicy and
TemplatedClusterNetworkPolicy — that use CEL expressions or webhook-based
template resolution to auto-generate policies per subnet from a single
template. For example, a template could express "allow ingress from all
Public subnets in this AND to all Private subnets" and the
controller would expand it into concrete NetworkPolicy resources for each
subnet pair.

**TODO**: Design the templating CRDs (TemplatedNetworkPolicy,
TemplatedClusterNetworkPolicy). Determine if CEL-based inline resolution is
sufficient or if webhook-based remote procedure calls are needed for complex
template expansion.

**TODO**: Define how the Plexus controller maps the high-level "security
groups" and "NACLs" concepts from the AND API into the underlying
NetworkPolicy / ANP / BANP / CNP resources. Options include:
- A `securityGroups` field inline in the AND spec (or a
  SecurityGroup CRD that references AND subnets) where users
  express rules in AWS-like terms (protocol, port, source/destination CIDR
  or group reference), and the Plexus controller translates them into
  NetworkPolicy / ANP.
- Declarative defaults — the Plexus controller auto-creates baseline
  policies per subnet type (e.g. Public subnets allow inbound from EVPN
  fabric, Private subnets deny all inbound, Isolated subnets deny all
  traffic except intra-subnet) so users get sane defaults without writing
  any policies.

### OVN-Kubernetes Backend: Services / Load Balancers (NLB)

Kubernetes has three service types. Each interacts differently with AND
subnets:

**Service creation model:**

Workflow:

1. User creates the AND (with subnets, types, AZs).
2. Plexus reconciles — creates namespaces, UDNs, CNCs,
   RouteAdvertisements, etc. on all target clusters.
3. User waits for the AND status to show subnets as ready.
4. User creates Services (and Deployments) in the subnet namespaces
   on a single cluster.
5. In multi-cluster deployments, Plexus mirrors the Service object to
   all other clusters where the subnet exists — determined by the
   subnet's `availabilityZone.clusterSelector`. Users do not need to
   deploy Services on every cluster manually. Plexus handles the
   remote propagation.

**TODO**: How does Plexus detect Service creation? The hub controller
would need to watch Service resources in AND subnet namespaces across
all spoke clusters. This means the hub's multicluster-runtime client
must have a watch on Services (in addition to EndpointSlices) scoped
to Plexus-managed namespaces. Determine the performance implications
of watching Services across all spokes and whether label-filtered
watches (e.g. only namespaces with `plexus.io/network-domain` label)
are sufficient to keep the watch scope bounded.

**Note: No AND-level service abstraction.** Plexus does not automate
Service creation — it only mirrors and propagates user-created
Services. Services cannot be defined in the AND spec because:
- Plexus has no knowledge of application-level concerns (selectors,
  ports, protocols) — only the user knows which pods a Service should
  target.
- Service selectors reference pod labels which are user-defined and
  opaque to Plexus.
- Adding service definitions to the AND would overload the AND
  abstraction with application-layer concerns it is not designed to
  manage.

**ClusterIP (TBD — to be decided during reviews)**

First, whether cross-cluster ClusterIP access is needed at all:

**Option A: ClusterIP is cluster-local.** ClusterIPs are only
reachable within the cluster where they are created. No cross-cluster
service stretching. For cross-cluster communication within the AND,
workloads use **direct pod IPs** (globally routable via the EVPN
fabric) or **LoadBalancer services** (with externally routable IPs
advertised via BGP on Public subnets). This is the simpler model — no
EndpointSlice mirroring, no service CIDR coordination, no new
machinery. Applications like CockroachDB that need cross-cluster
connectivity use pod IPs directly.

**Option B: ClusterIP is AND-scoped (stretched across clusters).**
A ClusterIP service in an AND subnet namespace is accessible from all
clusters where the subnet exists. This requires EndpointSlice
mirroring (remote pod IPs as backends) and a service CIDR strategy.
Within Option B, the choice of service CIDR approach applies uniformly
to single-cluster and multi-cluster deployments. Three sub-options are
under consideration:

---

**Option 1: Cluster-wide service CIDR (no changes to allocation).**

Services in AND subnet namespaces are allocated from the existing
cluster-wide `--service-cluster-ip-range` — standard Kubernetes
behavior. The ClusterIP never hits the wire; OVN performs DNAT at the
source, rewriting the destination to a backend pod IP before the
packet leaves the node. Since pod IPs are globally routable via EVPN,
the DNATed packet reaches any backend (local or remote) directly.

*Single-cluster:* Standard Kubernetes — no changes. Cross-subnet
service access works via CNC LB cross-attachment (described in common
section below).

*Multi-cluster:* Two sub-options:

- **Sub-option A: User deploys Service on each cluster (via GitOps).**
  Users deploy the same Service manifest on every cluster. Each
  cluster allocates its own ClusterIP independently. Simple but
  requires users to manage Service deployment across clusters.

- **Sub-option B: User creates Service on one cluster, Plexus
  mirrors.** The user creates the Service on a single cluster. Plexus
  mirrors the Service object to all other clusters where the subnet
  exists (determined by `availabilityZone.clusterSelector`). VIPs may
  differ across clusters because the original ClusterIP might already
  be in use on the remote cluster, or the clusters may have different
  service CIDR ranges — so Plexus creates the mirrored Service
  without specifying `clusterIP`, letting each cluster allocate
  independently.

In both sub-options, this is functionally correct (the ClusterIP
never crosses the wire) but provides a degraded user experience — the
same service has different IPs on different clusters. Note: overlapping service CIDRs across clusters do not cause VIP
collisions — mirrored Services are created without `spec.clusterIP`,
so the remote apiserver always allocates a non-conflicting IP. Even if
`orange-service` on cluster A and `green-service` on cluster B both
have ClusterIP `10.96.5.1`, when each is mirrored to the other
cluster it receives a fresh allocation (the original IP is already
taken). OVN LBs are keyed per-VIP per-service — no cross-contamination.

```
Cluster A (service VIP = 10.96.5.1):
  pod → 10.96.5.1 → OVN DNAT → remote pod IP (cluster B) → EVPN → cluster B

Cluster B (same service, VIP = 10.96.200.3):
  pod → 10.96.200.3 → OVN DNAT → remote pod IP (cluster A) → EVPN → cluster A
```

| | Pros | Cons |
|---|---|---|
| Option 1 | No changes to kube-apiserver or service allocation. Simple. No new IPAM. No webhook needed. | VIPs differ across clusters — bad UX (same service, different IPs). Functionally correct since VIP never crosses the wire, but confusing for users and debugging. |

---

**Option 2: Allocate service VIPs from the pod CIDR (reserved range).**

A portion of each subnet's pod CIDR is reserved for service VIPs
(e.g. the last 10 IPs of the subnet). The AND spec includes a
`reservedSubnet` field (or the Plexus controller automatically
reserves a fixed number of IPs from the end of each subnet's CIDR).
Pods are never assigned IPs from this reserved range; service VIPs
are.

Since the pod CIDR is globally consistent (EVPN L2 stretch — flat
addressing), VIPs allocated from this range are the same on every
cluster — giving a consistent service IP across all clusters in the
AND.

*Workflow:* Plexus creates a `ServiceCIDR` object covering the
reserved range (e.g. `10.0.1.246/29`) so that the apiserver accepts
IPs from this range. The user creates Services in one of two ways:
- **User sets fixed IP** — the user explicitly sets `spec.clusterIP`
  to an IP from the reserved range (e.g. `clusterIP: 10.0.1.250`).
  The apiserver validates it's within the registered ServiceCIDR and
  allocates it. No webhook intervention needed.
- **Plexus IPAM assigns** — a Plexus mutating webhook intercepts
  Service creation and sets `spec.clusterIP` to an IP allocated by
  Plexus IPAM from the reserved range.

**TODO**: Determine whether to support both workflows or tilt towards
Plexus always doing the allocation (better UX, consistent behavior).

Since the reserved range is globally consistent (same pod CIDR on all
clusters), the assigned VIP is the same everywhere.

```
Subnet CIDR: 10.0.1.0/24
  Pod range:     10.0.1.1 – 10.0.1.245
  Reserved range: 10.0.1.246 – 10.0.1.254 (service VIPs)

pod → 10.0.1.250 (service VIP) → OVN DNAT → 10.0.1.5 (backend pod) → same L2 fabric
```

Overlapping reserved ranges across different ANDs are not a problem —
each AND's subnets are separate UDNs with separate OVN logical
switches and separate LBs. EVPN provides L2 isolation (different
VNIs) between ANDs. The same VIP can exist in multiple ANDs without
conflict because they are on different logical switches with different
LB groups.

**Note:** Today OVN-K's `SubnetOverlapCheck` only validates against
the static `--service-cluster-ip-range` (cluster-wide service CIDR),
not against dynamically created ServiceCIDR API objects. If OVN-K
later adds validation against ServiceCIDR objects, care must be taken
to allow the reserved range (which intentionally overlaps with the
UDN subnet it's carved from).

| | Pros | Cons |
|---|---|---|
| Option 2 | Same VIP everywhere. Consistent across clusters since pod CIDR is globally flat. User can create headless and let Plexus assign, or pick a fixed IP themselves. | Reduces usable pod address space. Requires a ServiceCIDR object for the reserved range. Requires mutating webhook (for headless → VIP assignment). Plexus IPAM must allocate from reserved range. |

---

**Option 3: Per-UDN service CIDR via ServiceCIDR API (not
recommended).**

The AND spec includes a `serviceCIDR` field specifying a dedicated
service range for the AND. Plexus creates a Kubernetes
[ServiceCIDR](https://kubernetes.io/docs/tasks/network/reconfigure-default-service-ip-ranges/)
object (KEP-1880, GA in Kubernetes 1.33) on each cluster. A mutating
webhook intercepts Service creation in AND subnet namespaces and sets
`spec.clusterIP` explicitly from the AND's service range (allocated
by Plexus IPAM on the hub), ensuring consistent VIPs across clusters.

| | Pros | Cons |
|---|---|---|
| Option 3 | Same VIP everywhere. Clean separation between pod and service address spaces. | Requires users to specify a service CIDR in the AND spec — additional configuration burden that the AND abstraction is meant to eliminate. Requires Kubernetes 1.33+. ServiceCIDR is cluster-wide (can't scope to a namespace) — needs webhook to enforce. Hub IPAM is a new component. More API surface. |

---

**Cross-subnet service access (common to all options):**

Cross-subnet service access within the same AND (e.g. a pod in
subnet A accessing a ClusterIP service in subnet B) requires the
subnets to be connected via CNC. Today, CNC handles this for
Geneve-based UDNs — each network's OVN LBs are cross-attached to the
other network's switches, enabling local DNAT to backend pod IPs which
are then routed via the connect-router. For EVPN-based AND subnets,
CNC performs **both**:

1. **Route leaking** (shared route targets between ipVRFs) for
   pod-to-pod L3 reachability across subnets.
2. **Cross-attaching OVN load balancers** between connected networks'
   switches — the same LB cross-attachment mechanism CNC uses today
   for Geneve, applied in addition to route leaking.

Route leaking alone gives pod-to-pod reachability but does **not** add
the other network's OVN LBs to the local switch. Without LB
cross-attachment, a pod in subnet A could reach pod IPs in subnet B
(via leaked routes) but could not access subnet B's ClusterIP services.
Both mechanisms together ensure full cross-subnet connectivity
(pod-to-pod + ClusterIP services) within an AND.

| Cross-subnet mechanism | Geneve | EVPN |
|---|---|---|
| Pod-to-pod reachability | OVN connect-router (patch ports) | Route leaking between ipVRFs |
| Cross-subnet ClusterIP | LBs cross-attached to switches (via connect-router controller) | LBs cross-attached to switches (same mechanism) + route leaking for post-DNAT routing |

**EndpointSlice mirroring (common to all options):**

Cross-cluster ClusterIP access requires remote pod IPs as backends in
the local OVN load balancer. The Plexus hub controller runs an
EndpointSlice mirror controller that syncs across all spokes. The
approach mirrors OVN-Kubernetes's existing
[EndpointSlice mirror controller](../../go-controller/pkg/clustermanager/endpointslicemirror/)
— the hub watches EndpointSlices in AND subnet namespaces on each
spoke and creates corresponding mirrored EndpointSlices on the other
spokes where the same subnet exists. The
`availabilityZone.clusterSelector` on the subnet tells the controller
which clusters a subnet spans — any subnet placed on multiple
clusters implies its services need cross-cluster mirroring. Mirroring
is automatic — whatever EndpointSlices exist on the original cluster
are mirrored to the remote clusters.

Mirrored EndpointSlices carry the following metadata so that the
ovnkube service controller picks them up without changes:

- **Label** `k8s.ovn.org/service-name: <svcName>` — matches the
  service name, used by `GetServiceEndpointSlices` to find all
  EndpointSlices for a service on a UDN.
- **Annotation** `k8s.ovn.org/endpointslice-network: <networkName>`
  — identifies which UDN network the endpoints belong to.
- **Label** `endpointslice.kubernetes.io/managed-by: plexus.io/endpointslice-mirror`
  — ensures the local Kubernetes EndpointSlice controller and
  OVN-Kubernetes's own mirror controller ignore these resources.

The ovnkube service controller selects EndpointSlices by service name
label and network annotation only — it does not filter by
`managed-by`. This means remote endpoints are automatically included
in OVN load balancer programming without any changes to ovnkube.

OVN load balancers are present on EVPN L2 UDNs — the EVPN transport
adds a macVRF port to the logical switch but does not remove the GR or
load balancer groups. The DNAT happens in OVN before the packet reaches
the macVRF port, so the cross-cluster flow works naturally.

**NodePort**

NodePort services expose a port on every node in the cluster. In the
AND model, the question is which network the NodePort listens on — the
host network, the UDN's VRF, or both. For EVPN subnets where each UDN
has its own Linux VRF, a NodePort on the host network would bypass the
AND's isolation boundary.

**TODO**: Determine how NodePort services interact with AND subnets.
Options include scoping NodePort to the UDN's VRF (reachable only from
within the EVPN fabric), keeping it on the host network (breaks
isolation), or disabling NodePort for AND subnets entirely.

**LoadBalancer**

LoadBalancer services provision an external IP for ingress traffic. In
bare-metal EVPN environments, this is typically handled by MetalLB or
similar controllers that advertise the external IP via BGP. For Public
AND subnets, the LoadBalancer IP could come from the subnet's CIDR and
be advertised alongside pod routes via RouteAdvertisements. For Private
subnets, a LoadBalancer would need to SNAT — similar to an internal NLB
in AWS.

**TODO**: Determine how LoadBalancer services work across AND subnets —
whether a LoadBalancer IP is scoped to a single subnet or the entire
AND, and how it interacts with RouteAdvertisements for Public subnets.

### OVN-Kubernetes Backend: DNS

In the OpenStack world, each tenant network has its own DNS. The
AND model follows the same pattern: each UDN (subnet) gets its own
**CoreDNS instance** (or a CoreDNS configuration scoped to that UDN). Within
an AND, multiple DNS instances coexist — one per subnet.

This enables per-subnet DNS resolution where pods in a subnet resolve
service names to ClusterIPs allocated on their own UDN, not the cluster
default network.

**TODO**: Design the per-UDN CoreDNS deployment model. Determine whether the
Plexus controller deploys CoreDNS per subnet, per AND, or extends
the existing cluster CoreDNS with per-UDN configuration (e.g. server blocks
or plugins).

**TODO**: Determine how cross-subnet DNS resolution works within a
AND — e.g. can a pod in `web-a` resolve a service name in `app-a`
if they are in the same AND?

### OVN-Kubernetes Backend: Dependencies

| OKEP / Feature | Description | Status |
|---|---|---|
| [OKEP-5193](https://ovn-kubernetes.io/okeps/okep-5193-user-defined-networks/) | User Defined Networks — namespace-scoped and cluster-scoped UDN CRDs | Implemented |
| [OKEP-5296](https://ovn-kubernetes.io/okeps/okep-5296-bgp/) | BGP Integration | Implemented |
| [OKEP-5088](https://ovn-kubernetes.io/okeps/okep-5088-evpn/) | EVPN support — macVRF/ipVRF, VTEP CRD, FRR-K8S integration | Implemented |
| [OKEP-5224](https://ovn-kubernetes.io/okeps/okep-5224-connecting-udns/okep-5224-connecting-udns/) | Connecting UserDefinedNetworks — CNC for intra-domain routing | Implemented |
| [OKEP-5552](https://ovn-kubernetes.io/okeps/okep-5552-dynamic-udn-node-allocation/) | Dynamic UDN Node Allocation — AZ pinning optimization | Implemented |
| [#6607](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6607) | CNC extension for EVPN — inter-VRF route leaking (shared route targets) for pod-to-pod reachability + cross-attaching OVN LBs between switches for ClusterIP access across EVPN-based UDNs | New — to be proposed |
| TBD | East-west-only EVPN mode — macVRF without ipVRF | New — to be proposed |
| TBD | External IPAM plugin interface — disable OVN-Kubernetes IPAM, pod annotation-based IP allocation | New — to be proposed |
| TBD | RouteTable CRD — custom static routes for UDN VRFs | New — to be proposed |
| [#6604](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6604) | RouteAdvertisements + EVPN transport support for namespace-scoped UDNs (not just CUDNs) | New — to be proposed |
| TBD | EVPN support for `RoutingViaHost=false` (shared gateway mode) | New — to be proposed |
| TBD | Support for (Cluster)AdminNetworkPolicies on UDNs | New — to be proposed |
| TBD | DNS support for UDNs | New — to be proposed |

## Testing Details

* **Unit Testing**: Each component (controller reconcilers, backend plugin
  interface, OVN-Kubernetes backend, CLI commands, IPAM allocator) will have
  comprehensive unit tests. The OVN-Kubernetes backend will be tested with a fake
  `client.Client` to verify that the correct UDNs, CNCs,
  RouteAdvertisements, and policies are created for each AND
  configuration.

* **E2E Testing**: End-to-end tests will run on KIND clusters with
  OVN-Kubernetes deployed. Tests will cover:
    - Creating an AND with Public, Private, and Isolated subnets
      and verifying the expected namespaces, UDNs, CNCs, and
      RouteAdvertisements are created.
    - Verifying intra-domain routing between subnets.
    - Verifying Public subnet external reachability via RouteAdvertisements.
    - Verifying Private subnet outbound SNAT.
    - Verifying Isolated subnet isolation.
    - Day-2 operations: adding and removing subnets.
    - AND deletion and cleanup.
    - CLI plugin commands.

* **Scale Testing**: Scale tests will measure controller performance with
  large numbers of ANDs and subnets. Key metrics: reconciliation
  latency, memory footprint, API server load from watches and creates.

* **Multi-Cluster Testing**: Multi-cluster E2E tests will use multiple KIND
  clusters to verify hub-spoke resource propagation, cross-cluster
  connectivity via EVPN, and status aggregation.

* **Cross Feature Testing**: Tests will verify interaction with:
    - Dynamic UDN Node Allocation (OKEP-5552)
    - NetworkPolicy and AdminNetworkPolicy on UDN subnets
    - EgressIP (when supported)

## Documentation Details

* A new documentation section dedicated to Plexus will be added to
  ovn-kubernetes.io. The docs source will live in the `ovn-kubernetes/plexus`
  repository and be published as part of the ovn-kubernetes.io site. Topics
  include:
    - Plexus overview and architecture
    - AdministrativeNetworkDomain CRD reference
    - Getting started guide (single-cluster with KIND)
    - CLI plugin reference
    - Multi-cluster setup guide
    - Backend plugin development guide

* The OKEP itself is added to the Enhancement Proposals navigation in
  `mkdocs.yml`.

## Risks, Known Limitations and Mitigations

1. **EVPN dependency**: The OVN-Kubernetes backend requires EVPN as the transport
   fabric. Deployments without EVPN infrastructure cannot use ANDs
   with this backend. **Mitigation**: EVPN is a prerequisite documented
   upfront. Future backends (Geneve overlay, pure BGP) will broaden
   support.

2. **East-west-only EVPN mode does not exist yet**: Private and Isolated
   subnets require macVRF-only EVPN (no ipVRF). This capability does not
   exist in OVN-Kubernetes today. **Mitigation**: This will be proposed as
   a separate OKEP and implemented as a prerequisite.

3. **External IPAM is a new pattern**: OVN-Kubernetes's built-in IPAM is
   well-tested; disabling it for AND subnets and relying on
   external annotation-based allocation is a new code path.
   **Mitigation**: A reference IPAM implementation ships with Plexus, and
   the external IPAM interface will be thoroughly tested.

4. **Hub single point of management (multi-cluster)**: If the hub cluster
   goes down, no new AND changes reach spokes. **Mitigation**:
   Existing resources continue to work (ovnkube operates independently on
   spokes). Recovery requires restoring the hub. HA for the hub controller
   (leader election, multiple replicas) reduces this risk.

5. **Scale**: Large numbers of ANDs with many subnets across many
   clusters will generate significant API server load. **Mitigation**: Scale
   testing is part of the test plan. Dynamic UDN Node Allocation
   (OKEP-5552) reduces per-node overhead.

6. **VNI namespace exhaustion**: With SVD (Single VXLAN Device), FRR maps
   VNIs to VLANs on a single bridge, capping at ~4094 MAC+IP VRFs per node.
   Large deployments with many ANDs and subnets may exceed this.
   **Mitigation**: Per-AND VTEP designs or multiple SVD
   bridge/VTEP pairs will be evaluated during implementation.

## OVN-Kubernetes Version Skew

Plexus is a separate controller deployed independently from OVN-Kubernetes.
It targets OVN-Kubernetes **v1.4** as the minimum supported version, which
is when the prerequisite features (EVPN, Dynamic UDN Node Allocation) are
expected to be available.

The Plexus controller creates standard Kubernetes resources (UDN, CNC,
RouteAdvertisements, NetworkPolicy) — it does not modify OVN-Kubernetes
internals. Version skew between Plexus and OVN-Kubernetes is handled by
the CRD API versioning: Plexus targets specific CRD API versions and will
document minimum OVN-Kubernetes versions per Plexus release.

## Backwards Compatibility

Plexus is fully additive. It introduces new CRDs (AdministrativeNetworkDomain) and a new
controller in a separate repository. It does not modify any existing
OVN-Kubernetes APIs or behavior.

- **Existing UDN/CUDN/CNC workflows are unchanged.** Users can continue to
  create bare UDNs and CNCs without Plexus.
- **No changes to ovnkube binaries.** The OVN-Kubernetes controller, cluster
  manager, and node daemons are unmodified.
- **No changes to existing CRD schemas.** UDN, CUDN, CNC,
  RouteAdvertisements schemas are unchanged.
- **Plexus-managed resources are labeled.** All resources created by Plexus
  carry `plexus.io/network-domain` labels so they can be distinguished
  from manually-created resources.

## Alternatives

### Naming: Why not "VPC"?

"Virtual Private Cloud" (VPC) is the industry-standard term for this
concept. We considered using `kind: VPC` for the CRD. We chose
`kind: AdministrativeNetworkDomain` instead for several reasons:

1. **Backend-agnostic**: Plexus is designed to work with multiple backends.
   On the AWS backend, an AND would *create* an actual AWS VPC as
   output. Calling the intent layer "VPC" while it produces VPCs as output
   is semantically wrong.
2. **No cloud-provider baggage**: "VPC" carries AWS/GCP/NSX-specific
   connotations and an expectation of feature parity with those providers.
   "AdministrativeNetworkDomain" is a neutral term.
3. **"Cloud" in the name**: The "C" in VPC stands for "Cloud." For bare-metal
   and non-cloud deployments, the name is misleading.
4. **"Private" is misleading**: A VPC contains both public and private
   subnets. The construct itself is not "private."

### API Design: (C)UDN as AdministrativeNetworkDomain (no new CRD)

In this model, a single (C)UDN would represent an entire AND. The
(C)UDN spec would be extended with subnet support.

**Why this was not selected:**

1. A VPC has different **subnet types** (Public, Private, Isolated,
   VPNOnly) with distinct north-south behaviors. A single (C)UDN cannot
   represent both a public subnet (with RouteAdvertisements and BGP
   export) and an isolated subnet (no external routes) — each subnet in
   a (C)UDN is the same logical separation. Extending (C)UDN with
   per-subnet type semantics would overload the abstraction.
2. A (C)UDN has a single topology (Layer2 or Layer3). An AND needs
   subnets of different topologies.
3. A (C)UDN has a single transport mode. An AND may contain subnets
   with different transports.
4. CNC becomes awkward for AND peering — it would serve double duty
   (grouping subnets within a domain AND connecting domains) with ambiguous
   semantics.
5. (C)UDN spec immutability prevents day-2 mutations that ANDs
   require (adding/removing subnets with different topologies).

### API Design: CNC as AdministrativeNetworkDomain (no new CRD)

In this model, ClusterNetworkConnect would serve as the AND.

**Why this was not selected:**

1. CNC cannot be both the AND and AND peering.
2. `connectSubnets` is transit plumbing, not an AND CIDR.
3. Intra-domain routing should be implicit, not require manual transit CIDR
   allocation.
4. AND-level configuration (CIDR governance, DNS, security
   defaults, status aggregation) has no home on CNC.

### API Design: AdministrativeNetworkDomain as a field on (C)UDN

In this model, CUDNs with the same `networkDomain.name` value are grouped
into an AND.

**Why this was not selected:**

1. Duplicated AND-level configuration across every (C)UDN.
2. No resource for AND-level operations (peering, status).
3. Status reporting is scattered across multiple (C)UDN status fields.
4. Future extensibility suffers — all domain-level features crammed into
   (C)UDN spec.

### User Experience: AdministrativeNetworkDomain CRD Without a Controller

In this model, the AND CRD exists as a passive grouping resource
but there is no controller. Users still create every (C)UDN, CNC,
RouteAdvertisements, and policy by hand.

**Why this was not selected:**

1. Poor user experience — 6-8 resource creations for a single operation.
2. Error-prone label wiring with no feedback on typos.
3. Intra-domain routing is not automatic.
4. Day-2 is a manual checklist.
5. The CRD becomes a bookkeeping artifact with marginal value.

### Multi-Cluster: Hub-Spoke with Plexus Controller on Spokes

In this model, each spoke runs its own Plexus controller.

**Why this was not selected:**

1. Extra controller on every spoke adds operational overhead.
2. Version skew risk between hub and spoke controllers.
3. Redundant logic — same translation runs on every spoke.
4. Unnecessary for initial scope where translation is mechanical.

### Multi-Cluster: Replicated Model

Each cluster has its own independent copy of the AND.

**Why this was not selected:**

1. No single source of truth — specs can diverge.
2. IPAM coordination requires distributed consensus.
3. Policy drift between clusters.
4. User confusion about which cluster to modify.

### Multi-Cluster: Federated Model

The AND lives on an external federation control plane.

**Why this was not selected:**

1. External dependency (OCM, Fed v2) not all deployments have.
2. Indirection adds latency and complexity.
3. Tight coupling to federation API.

### IPAM: Hub-Managed Hard-Split CIDR

The hub pre-partitions each subnet's CIDR into non-overlapping per-cluster
slices.

**Why this was not selected:**

1. Address space wastage from pre-splitting.
2. Inflexible scaling — adding a cluster requires re-partitioning.
3. Not flat — breaks the L2 EVPN flat addressing property.
4. OVN-Kubernetes IPAM coupling — designed for per-node L3, not flat L2 pools.

### Multi-Cluster Client Alternatives

The Plexus controller uses
[multicluster-runtime](https://github.com/multicluster-runtime/multicluster-runtime)
for multi-cluster client management. Two other approaches were considered:

- **Hypershift style (manual client management).** The Plexus controller
  manually builds a `controller-runtime` `client.Client` per spoke from
  kubeconfig secrets. The controller owns client caching, kubeconfig
  rotation, and unreachable-cluster handling. This is what
  multicluster-runtime automates — useful only if the library proves
  too restrictive.

- **[OCM ManifestWork](https://open-cluster-management.io/docs/concepts/work-distribution/manifestwork/).**
  The hub creates `ManifestWork` resources in each spoke's cluster
  namespace. A work agent on each spoke watches its ManifestWork and
  applies the embedded manifests locally. Requires OCM infrastructure
  on every spoke.

| | Hypershift style | multicluster-runtime | OCM ManifestWork |
|---|---|---|---|
| Cluster discovery | Manual (watch kubeconfig secrets) | Automatic via Provider | OCM ManagedCluster / Placement |
| Client lifecycle | Controller manages | Library manages | Work agent on spoke manages |
| Kubeconfig rotation | Controller handles | Provider handles | OCM handles |
| Sharding | Not built in (single leader) | Built in (Rendezvous Hashing) | N/A (spoke agent pulls) |
| Status feedback | Controller reads back from spoke | Controller reads back from spoke | Built-in (resourceStatus, FeedbackRules) |
| Hub-to-spoke model | Hub pushes (direct API) | Hub pushes (direct API) | Spoke pulls from hub |
| Maturity | Battle-tested in Hypershift | Newer, SIG-blessed | Mature (OCM GA) |
| Dependency | controller-runtime only | + multicluster-runtime | OCM + work agent on every spoke |

**Why multicluster-runtime was selected:** It provides the same labeled
kubeconfig secrets pattern as the Hypershift style but with automatic client
lifecycle management, kubeconfig rotation, and built-in sharding — all
maintained by SIG-Multicluster. No point reimplementing what the library
already provides. OCM was ruled out because it requires infrastructure
(work agents) on every spoke, adding operational overhead.

## Appendix: Cross-Platform Feature Mapping

The following table maps AND constructs across AWS, OVN-Kubernetes, and
VMware NSX. Items marked *(future)* are not in scope for the initial
implementation.

| Feature | AWS VPCs | VMware NSX | OVN-Kubernetes Backend |
|---|---|---|---|
| **Tenancy** | AWS Account | NSX Project (Tenant) | Namespace (1:1 with subnet) |
| **Workload Attachment** | Subnet | NSX Segment (L2) / Subnet (L3) | K8s Namespace/UDN (EVPN L2 only) |
| **Fixed CIDR** | VPC CIDR block | IP Blocks allocated per VPC | *(future)* — AND-level `cidrBlocks` with automatic subnet allocation |
| **Multiple Subnets** | Yes | NSX Segments within a VPC | AND defines subnets; each becomes a ns + UDN (L2 EVPN) |
| **Public Subnets** | Subnet + IGW + Route + Public IP | Subnet type: Public | ns + UDN (L2 EVPN) + RouteAdvertisements (RA) |
| **Private Subnets** | Subnet + NAT GW + Route | Subnet type: Private | ns + UDN (L2 EVPN) + nodeIP SNAT |
| **Isolated Subnets** | Subnet (no routes to IGW/NAT) | Subnet type: Isolated | ns + UDN (L2 EVPN) with no nodeIP SNAT |
| **VPN-Only Subnets** | Subnet + VPN GW + Route | N/A | ns + UDN (L2 EVPN) + IPsec north-south |
| **Internet Gateway** | IGW resource + route entry | Tier-0 gateway uplink | RouteAdvertisements (BGP export) |
| **NAT Gateway** | NAT GW + EIP + route entry | SNAT rule on Tier-0/Tier-1 | Node IP SNAT (EgressIP in future) |
| **Route Tables** | Route table + route entries | Static routes on Tier-0/Tier-1 | RouteTable CRD (TBD) — custom static routes |
| **Security Groups** | Stateful, per-instance | Distributed Firewall (DFW) | NetworkPolicy / AdminNetworkPolicy (stateful ACLs) |
| **NACLs** | Stateless, per-subnet, ordered | Gateway Firewall rules | NetworkPolicy / AdminNetworkPolicy (stateless ACLs) |
| **Route Server** | Yes | BGP peering on Tier-0 | *(future)* |
| **Peering** | Peering connection + routes | Inter-VPC routing | *(future)* — CNC with `networkDomainSelector` |
| **VPN Connections** | VPN GW + tunnel config | IPsec VPN service | *(future)* |
| **Transit Gateway** | Hub-and-spoke router | Tier-0 gateway | *(future)* |

## References

- [openshift/enhancements#1967](https://github.com/openshift/enhancements/pull/1967) — OpenShift VPC enhancement proposal (the starting point for this design)
- [OKEP-5088: EVPN Support](https://ovn-kubernetes.io/okeps/okep-5088-evpn/) — underlay for on-prem subnets
- [OKEP-5193: User Defined Networks](https://ovn-kubernetes.io/okeps/okep-5193-user-defined-networks/) — UDNs that Plexus creates
- [OKEP-5224: Connecting UserDefinedNetworks](https://ovn-kubernetes.io/okeps/okep-5224-connecting-udns/okep-5224-connecting-udns/) — CNCs for intra-domain routing
- [OKEP-5259: No-Overlay Mode](https://ovn-kubernetes.io/okeps/okep-5259-no-overlay/) — RouteAdvertisements for public subnets
- [OKEP-5296: BGP Integration](https://ovn-kubernetes.io/okeps/okep-5296-bgp/) — BGP integration
- [OKEP-5552: Dynamic UDN Node Allocation](https://ovn-kubernetes.io/okeps/okep-5552-dynamic-udn-node-allocation/) — AZ pinning optimization
- [AWS VPC Documentation](https://docs.aws.amazon.com/vpc/latest/userguide/how-it-works.html)
- [Azure VNet Documentation](https://learn.microsoft.com/en-us/azure/virtual-network/virtual-networks-overview)
- [VMware NSX VPC Documentation](https://techdocs.broadcom.com/us/en/vmware-cis/nsx/vmware-nsx/4-2/administration-guide/nsx-multi-tenancy/nsx-virtual-private-clouds.html)
- [multicluster-runtime](https://github.com/multicluster-runtime/multicluster-runtime) — SIG-Multicluster controller-runtime extension
