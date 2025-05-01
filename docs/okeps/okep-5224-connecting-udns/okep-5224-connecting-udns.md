# OKEP-5224: Connecting UserDefinedNetworks

## Problem Statement

The problem we are trying to solve here is how can an admin connect
multiple [UserDefinedNetworks] or [ClusterUserDefinedNetworks] better known
as “Connecting (C)UDNs”. The assumption here is that each (C)UDN is an
isolated island and in order to make any two (C)UDNs communicate with
each other they have to be explicitly requested to be connected.

[UserDefinedNetworks]: https://ovn-kubernetes.io/api-reference/userdefinednetwork-api-spec/#userdefinednetwork
[ClusterUserDefinedNetworks]: https://ovn-kubernetes.io/api-reference/userdefinednetwork-api-spec/#clusteruserdefinednetwork

## Goals

* Connecting and disconnecting UDNs and CUDNs within the same cluster
  aka intra-cluster-udn-connectivity for admins (can be done as day0 or day2).
  * Support for connecting `Layer3<->Layer3`, `Layer2<->Layer2` and `Layer3<->Layer2` type networks.
  * Support for both `role:Primary` and `role:Secondary` CUDN connections.
  * Support for both `role:Primary` and `role:Secondary` NAD connections. From
    henceforth all references to CUDN/UDNs implies NADs as well.
  * Support for only `role:Primary` UDN connections.
* Support for pods, services (clusterIPs only since expectation today is that
  NodePorts and LoadBalancer type services are already exposed externally and
  hence reachable across UDNs) and network policies across the connected UDNs and CUDNs.
* Ensure the proposed API is expandable for also connecting and disconnecting
  UDNs or CUDNs across clusters in the future.

## Future Goals

* Tenants being able to request for interconnecting their networks
  with other tenants.
    * We need to understand exact use cases for why we need tenants to initiate
      connection requests with other tenants. How will a tenant know other tenants
      that exist on the cluster? In cases like finance tenant wanting to use IT
      tenant's services, they could open a ticket to the admin to make that happen.
* Admins being able to connect `role:Secondary` UDNs. Given
  `SecondaryUserDefinedNetworkSelector` uses a combination of
  namespace selector and network selector, a tenant could change
  the labels on the UDN thereby causing security issues. Hence this
  type of selector support will come later
* Connecting and disconnecting UDNs and CUDNs across two clusters
  aka inter-cluster-udn-connectivity for admins (can be done as day0 or day2).
  There is a section that includes some basic details or open ended questions
  at the end to continue this work for cross-clusters but that is not in scope
  for the first phase of this enhancement. There are more basic designs that
  need to be considered like "what does it mean for a UDN to exist across
  two clusters" from an API standpoint and what is the multi-cluster
  networking story for OVN-Kubernetes, before we can decide UDN connections
  across clusters.

## Non-Goals

* Connecting two UDNs with overlapping subnets won’t be supported.
  This is not a valid supported scenario. We did think about making
  overlapping podIPs through services work, but even then it needs
  some kind of SNATing because replies could still go to pods in
  its own UDNs rather than to the other UDN.
* Support for this feature in non-interconnect architecture is out of scope.
* Support for `localnet` type network connections is out of scope.
  * `localnet` type networks can be connected together using bridges already
  * `localnet`<->`layer2` or `layer3` will not be covered here.
* Supporting Live Migration and PersistentIPs across connected UDNs (Ask
  if Virt team has use cases for this?).

## Introduction

OVN-Kubernetes supports namespace-scoped UserDefinedNetworks (UDNs) CRD
which can be used by tenants to isolate their workloads. They can creating a
namespace, create a UDN within that namespace and then any workloads created
inside that namespace will be attached to that UDN which then becomes the primary
network for those workloads. These workloads cannot talk to workloads attached
to other UDNs or default network. Similarly cluster-scoped ClusterUserDefinedNetworks (CUDNs)
CRD can be used by admins to connect more than 1 namespace as part of the same
primary network and achieve isolation.

When creating these networks with `role:secondary`, it also provides the
ability to achieve multihoming for workloads where a pod can be attached to more than 1 network.

However there are scenarios where we might want to allow
for partial or full connectivity between the different microservices
that are segmented across the different UDNs. See the use cases section
for more details. This effort tracks the details of implementing connectivity
between two (C)UDNs in a single cluster. The following sections call
out some of the assumptions that will ground the design/implementation.

### Symmetric nature of UDN Connectivity

If UDN-A is connected to UDN-B then that means UDN-B is connected to UDN-A.
When connecting those networks together it assumes bi-directional relationship.

### Non-Transitive nature of UDN Connectivity

If UDN-A and UDN-B are connected together and UDN-B and UDN-C are connected
together, that does not mean UDN-A is connected to UDN-C. Reasoning is we
cannot assume something indirectly - It’s always best to let the user
express what they’d like to see connected.

### What does it mean for two UDNs to be different?

* Any two CUDNs that don’t have the same metadata.name are considered
  to be “different UDNs”.
* Any two UDNs within the same namespace that don’t have the same
  metadata.name are considered to be “different UDNs”.
* Any two UDNs in different namespaces are considered to be “different UDNs”.

These statements hold true whether the UDNs are within the same cluster or
across clusters. However, this effort tracks solving (dis)connecting
two different UDNs within the same cluster.

## User-Stories/Use-Cases

### Story 1: Allowing network expansion for a tenant

**As a cluster admin**, **I want** to connect my newly created UDN-New with an
existing UDN-Old **so that** they can communicate with each other as if
being part of the same network.

Example: Existing (C)UDN has full occupancy of its address-space and
we don’t allow CIDR expansion or mutation of UDN Spec today.

The admin created UDN-Old with a defined subnet CIDR range which gets
exhausted on a 500thday, now the admin cannot mutate UDN-Old to change
or expand its subnet CIDR range. Admin creates a new UDN-New
(could be in a totally different cluster) and now wants to interconnect
them together.

```text
+-------------------+         Interconnect        +-------------------+
|    UDN-Old        |<--------------------------->|    UDN-New        |
| (Full CIDR Range) |                             | (New Subnet)      |
|                   |                             |                   |
| +--------------+  |                             |  +--------------+ |
| |   Pod(s)     |  |                             |  |   Pod(s)     | |
| +--------------+  |                             |  +--------------+ |
|                   |                             |                   |
+-------------------+                             +-------------------+
```

### Story 2: Allowing connectivity between two tenants through exposed clusterIP services

**As a cluster admin**, **I want** to allow tenant-Green to be able to access
the clusterIP microservice exposed by tenent-Blue.

Example: When separate services owned by different teams need to
communicate via APIs

The admin created UDN-Blue and UDN-Green that were isolated initially
but now wants to allow partial inter-tenant communication. Say data
produced by microservice blue needs to be consumed by microservice
green to do its task.

```text
+-------------------+                        +-------------------+
|    UDN-Blue       |                        |    UDN-Green      |
|                   |                        |                   |
| +--------------+  |                        |  +--------------+ |
| | Microservice |  |                        |  | Microservice | |
| |    Blue      |  |                        |  |    Green     | |
| +--------------+  |                        |  +--------------+ |
|        |          |                        |         |         |
|   +----v-----+    |                        |         |         |
|   |ClusterIP |<---+------------------------+---------+         |
|   | Service  |    |                        |                   |
|   +----------+    |                        |                   |
+-------------------+                        +-------------------+
```

### Story 3: Merging two tenants (maybe even temporarily)

**As a cluster admin**, **I want** to now connect my existing UDN-Blue and UDN-Green
**so that** they can communicate with each other as if being part of the same network.

Example: The admin created UDN-Blue and UDN-Green that were isolated
initially but now wants to treat them as being part of the same network.
Pods cannot be rewired given they are part of two different UDNs. Say
when different teams within the same organization operate as separate
tenants but collaborate on a project requiring unrestricted communication
for a period of time and then want to disconnect from each other later.

### Story 4: Same UDN across multiple clusters must be connected by default

**As a cluster admin**, **I want** to connect my workloads which are part
of the same UDN but are in different clusters. 

Example: The user has split their workloads into multiple clusters thereby
the same UDN could be fragmented across different clusters. These pods should
be able to communicate with each like it would be if they were on the same
cluster but be isolated from other workloads.

NOTE: Cross cluster UDN connecting is not targeted in this OKEP, but is
covered here since connecting UDNs within the cluster is the first step here.

### Story 5: Connecting mixed type networks together (Layer2 Network workloads with Layer3 Primary/Default Network)

**As a cluster admin**, **I manage** isolated tenants (a bunch of
interconnected pods) and share them with my users. Every tenant has
its own Primary Layer3 UDN to emulate its own cluster network, but
isolate it from the other tenants. Northboud/internet traffic from my
tenants is always flowing through additional gateways that I manage.
Gateways require HA support, which is done using OVN virtual Port type,
which is only supported when all parent ports are located on the same switch,
hence the UDN used by the Gateways is Layer2 (Primary).

To make tenants' traffic flow through the gateways, I need to interconnect
Primary Layer3 network with Primary Layer2 network. Here is a diagram
illustrating this ask:
```text
                 .---------.
               ,'           `.
              (   Internet    )
               '-.         ,-'
                  `---^---'
                     ++
 +-------------------+-----------------+
 |         Public Localnet NAD         |
 +----^-----------------------<+-------+
     ++                        |        
 +---+-------+            +----+------+ 
 | Internet  |            | Internet  | 
 |  Gateway  |            |  Gateway  | 
 |    Pod    |            |    Pod    | 
 |  (VRRP)   |            |  (VRRP)   | 
 +------^----+            +----^------+ 
       ++                      ++       
+------+------------------------+-----+ 
|             Layer2 NAD              |
| (VRRP Multicast to provide HA VIP)  |
+------------------^------------------+
                   |
               Interconnect
 +-----------------v-------------------+
 |         Layer3 Primary NAD          |
 +----^-------------^----------^-------+
     ++            ++         ++
   +-+--+        +-+--+    +--+-+
   |Pod |        |Pod |    |Pod |
   +----+        +----+    +----+
```

### Story 6: Migration of connected tenant workloads from other platforms like NSX into OVN-Kubernetes

**As a cluster admin**, **I manage** isolated tenants on
my native NSX platform where they are already seggregated into segments
using networks. Some of these segments are connected through vRouter
for mutual access. I want to migrate such workloads into OVN-Kubernetes
platform by leveraging user defined networks. I would need connecting UDNs
feature to replicate that same workload architecture. The alternative
of placing those workloads into same UDN and using network policies
will not work because it is significant architectural change we would
need to mandate on each tenant since tenant own their networks.

Example: Tenant-frontend has access to Tenant-backend's databaseAPI
workloads on NSX through vRouter, I want to migrate both these tenants
into OVN-Kubernetes platform keeping that architecture intact.

## Proposed Solution

This section tries to answer: How can an admin declare two (C)UDNs to be connected?

### Config for enabling the feature

The whole feature will be behind a config flag called `--enable-connecting-user-defined-networks`
which has to be set to true to get this feature.

### API Details

The API change must work for both intra-cluster and inter-cluster UDN connectivity (future).
This enhancement proposes a new CRD to allow admins to request multiple networks
to be connected together.

`UserDefinedNetworkAdminConnect` CR will be cluster-scoped resource and admin-only API
that will allow admins to select multiple different networks that need to be
connected together. It will also allow specifying what level of connectivity is
expected. See the API definitions for more details.

Admins can create a `UserDefinedNetworkAdminConnect` CR which
signals the controller to connect the topologies of the UDNs selected by this CR.

```go
TBD
```

Sample YAML:

```yaml
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetworkAdminConnect
metadata:
 name: colored-enterprise
spec:
 networkSelectors: # can match on UDNs and/or CUDNs
networkSelectionType: ClusterUserDefinedNetworks
clusterUserDefinedNetworkSelector:
  networkSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values:
      - blue-network
      - green-network
networkSelectionType: PrimaryUserDefinedNetworks
primaryUserDefinedNetworkSelector:
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values:
      - yellow
      - red
connectSubnets: # can have at most 1 CIDR for each family type
  - cidr: 192.168.0.0/10
    # This slice must be set to the maximum number of networks
    # a user plans to connect together (similar to the podsubnet
    # slice that equates max number of pods on each node). This
    # slice is then handed out to each node.
    # Can be removed if we go with Idea2 in the implementation section
    hostSubnet: 24
 featuresEnabled:
   - podNetworkConnectivity
     # enabled through a well-known label on service if we need finer-grained control
   - clusterIPServicesConnectivity
   - networkPolicyEnforcement
```
* `featuresEnabled` field will ensure we can add support for enabling more types of
  features for the connected UDNs granularly in the future. At least one of these
  options must be set and validations will be added for which combinations of these
  options could/should be set together
* If `podNetworkConnectivity` is enabled then admin is asking for full pod2pod
  connectivity across UDNs
* If `clusterIPServicesConnectivity` is enabled then admin is asking for
  clusterIPs to be reachable across UDNs.
    * If we don’t want to allow ALL clusterIPs to be exposed to other UDNs,
      then on a per-service level in addition to the UDN CR requesting for it,
      we can add a granularity level of “making that service be a candidate for UDNConnectivity”
* If `networkPolicyEnforcement` is enabled then network policies will span across UDNs.
   * If we want to only allow some policies then again label based granularity will be needed.
* `connectSubnet` field is used to take a configurable subnet from the end user that will
  be used to connect the different networks together. The slice needs to be big enough to
  accommodate an IP per node per network. So if there are N nodes in your cluster and M
  networks you should have N*M IPs for allocation in this slice. This value is expected
  to be configured uniquely across multiple UDNConnect CRs that select the same network
  i.e. If a given network is selected as part of more than 1 connect API, each of those
  connect APIs must have a unique subnet. If a user accidentally configures overlapping
  values, we must have a status check to block and report this in the status (details to be fleshed out).

#### API Validations


#### Valid ways of Connecting/Selecting UDNs

TBD
* A network could be selected as part of more than one network, so what is the expectation
  when two Connect APIs are designed that select the exact same set of networks? How do
  we control validation of such scenarios?
* Or when a second connect API selects a subset of networks in the other connect API -> just
  ECMP connections? Or duplication on OVN layer? Or we want to prevent this - this is a hard
  problem -> I guess at the end of the day this problem exists for all KAPIs we just need rules
  to be additive.

### Implementation Details

#### IP Address Management for Connected Networks


#### Controller Design

We will add: <--> TBD some of these bits depend on which design we pick (scroll to the Topology section) -> Idea2 will really simplify things
* A new connect-subnet-manager (TBD maybe a controller really?) running only on ovnkube-control-plane
  i.e cluster-manager pods that will do:
  * The per node `connectSubnet` slice allocation according to the set
    `connectSubnet.hostLength` field. A slice of the connect subnet will be handed
    out to each node and annotated on the node using a new annotation
    `k8s.ovn.org/node-connect-udn-subnets` that stores the value for each connect
    UDN API for that node. This mechanism will be very similar to the
    `k8s.ovn.org/node-subnets` annotation behaviour we have today.
  * Per node-network router tunnel-key allocation that the topology design
    (see further sections) needs since there is a requirement of no
    conflicts of tunnel-keys across nodes. Each connect API would need N*M tunnel keys
    if N is the number of nodes and M is the number of networks to be connected together.
    (TBD flesh out annotation data storage ideas...based on which implementation Idea we finalize on (Idea1 v/s Idea2))
  * TBD: status controller changes
* A new controller added to OVN-Kubernetes called `ConnectUDNsController` running
  on each ovnkube-controller that watches the `UserDefinedNetworkAdminConnect` resource will do:
  * The necessary Northbound database plumbing to provide connectivity between the UDNs.
  * Read the `k8s.ovn.org/node-connect-udn-subnets` annotation and hand out an IP
    to each network’s router during interconnection.

NOTE: Having the `NodeNetwork` CRD as an internal data storage API might be more ideal
to store the above information internally as this needs to be persisted into etcd on reboots.
It will simplify the annotations and also help with scale since there are known issues
around patching of annotation and waiting for reading and unmarshalling the annotation
impacts scale.

#### Network Topology

This section looks at the topology changes that will be introduced to
connect these three networks together.

**Connecting Layer3 type networks together**

The below diagram shows the overlay part of the OVN Topology that OVN-Kubernetes
creates for 3 UDN networks (blue, green and yellow) across two nodes.

![3-isolated-l3-networks](l3-connecting-udns-0.png)

Let’s take a look at how we can connect a blue network, green network and yellow network at the OVN layer.
Currently there are two ideas that I am torn between, I will cover both
and outline pros and cons that might make it easier to pick the best one
during reviews to then dive deeper and add more information. There were other ideas
which were discarded and listed in the alternatives section.

**Idea1: Add a new transit switch/router between the two ovn_cluster_routers of the networks**

When the UDNConnect CR gets created selecting these three networks, the controller
will make the following changes to the topology:
* Create a distributed colored-enterprise_interconnect-switch/router (basically a
  transit-switch/router type in OVN used for interconnection) with the `connectSubnet`
  value provided on the CR (default=192.168.0.0/10)
* Connect the colored-enterprise_interconnect-switch to the ovn_cluster_router’s
  of the blue, green and yellow networks on each node using remote patch ports and tunnelkeys

![blue-green-yellow-l3-connect-idea1](l3-connecting-udns-1.png)

So in this idea it will be 1 switch per connect API.

**Idea2: Connecting the ovn_cluster_routers directly**

When the UDNConnect CR gets created selecting these two networks, the controller
will make the following changes to the topology:
* Create a logical-router-port on `blue_blue.network_ovn_cluster_router` with an IP
  assigned from `connectSubnet`.
* Create a logical-router-port on `green_green.network_ovn_cluster_router` with an IP
  assigned from `connectSubnet`.
* Create a logical-router-port on `yellow_yellow.network_ovn_cluster_router` with an IP
  assigned from `connectSubnet`.
These three ports will have their `peer` field set as each other to allow for direct
connectivity.

![blue-green-yellow-l3-connect-idea2](l3-connecting-udns-4.png)

The above diagram shows what happens when all 3 networks are connected together.

**Which idea to pick?**

| Topic       |    Idea1    |  Idea2      |
| ----------- | ----------- | ----------- |
| Number of new IPs required for connectSubnet | N(nodes)*M(networks) | N(nodes)*M(networks) |
| Requires ovnkube-controller local IPAM for per node-network router      | YES       | YES       |
| Requires node-network router IP storage design (either annotations OR nodeNetworkCRD)      | YES       | YES       |
| Requires Centralized IPAM slicing per node   | YES        | NO - here given each node can re-use the same set of IPs, there is no need for centralized slicing - this is a huge advantage |
| Requires Centralized tunnel-ids allocation   | YES        | NO - this is a huge advantage |
| Reply is Symmetric | NO - but can be made symmetric with /24 routes if required | NO - replies will be assymmetric |
| Scalability of routes | /16 will be 1 route on each router per connected network | /16 will be 1 route on each router per connected network |
| Scalability of connections | Each router get's 1 additional port and the new switch will get M (number of networks) patch ports - so totally N*M patch ports | Point-to-point connections means all ovn_cluster_router's now have N*M patch ports |
| Dataplane churn caused for connect/disconnect | More isolated changes because adding and disconnecting a network is as simple as adding/removing the port only from that network's router and the central switch. | Direct R2R Connection - so disconnecting a network means adding/removing the port from each connected router towards this network. So removing a specific tenant network will involve touching routers of other tenant's networks. |
| Network being part of more than one connectAPI | Non-transitivity is automatically taken care of because there is no connection between the indirectly connected networks. |  Non-transitivity is taken care of because of the absence of routes on the router for the indirect connection to work. Any lingering stale routes could accidentally cause connectivity. |
| Extensibility for adding features | Better because adding a transit router or switch opens up adding newer features easily like having hierarchical hubs of connections | Direct Connection might put us in a bind with regards to how much topology change we could do in future |
| Throughput | Add's an extra hop | Better because its a direct connection |
| Networking model | Aligns with a vRouter/vSwitch model that users expect | Direct R2R Connection |
| Connecting mixed type networks | Aligns with a vRouter/vSwitch model that users expect - 1 transit switch per connectAPI | Direct R2R Connection |

Conclusion: I think idea2 does simplify implementation because I don't need a
centralized allocator, but it also boxes us in a bit with being able to extend
the topology further if future use cases need that.

**Connecting Layer2 type networks together**

The below diagram shows the overlay part of the OVN Topology that OVN-Kubernetes
creates for 3 UDN networks (blue, green and yellow) across two nodes.
NOTE: The topology representation is that of the new upcoming Layer2 topology.
The dotted pieces in network colors represent the bits in design and not there yet.

![3-isolated-l2-networks](l2-connecting-udns-0.png)

**Idea1: Add a new transit switch between the two transit routers of the networks**

![blue-green-yellow-l2-connect-idea1](l2-connecting-udns-1.png)

**Idea2: Connecting the ovn_cluster_routers directly**

![blue-green-yellow-l2-connect-idea2](l2-connecting-udns-2.png)

[TBD] Check with OVN team transit router can indeed have routes and policies right?

**Connecting mixed type (layer3 and/or layer2) networks together**

The below diagram shows the overlay part of the OVN Topology that OVN-Kubernetes
creates for 3 UDN networks (blue(l3), green(l3) and yellow(l2)) across two nodes.
NOTE: The yellow topology representation is that of the new upcoming Layer2 topology.
The dotted pieces in network colors represent the bits in design and not there yet.

![3-isolated-mixed-networks](mixed-connecting-udns-0.png)

**Idea1: Add a new transit switch between the two transit routers of the networks**

![blue-green-yellow-l2-connect-idea1](mixed-connecting-udns-1.png)

**Idea2: Connecting the ovn_cluster_routers directly**

![blue-green-yellow-l2-connect-idea2](mixed-connecting-udns-2.png)

#### Pods

In order to ensure the green pod can talk to blue pod and vice versa, controller will
also do the following bits:

* Add logical router policies to blue-network’s ovn_cluster_router that steers
  traffic towards the green UDN’s subnet to interconnect switch on all nodes
* Add routes to the green-network’s ovn_cluster_router that steers traffic
  towards the blue UDN’s subnet to interconnect switch on all nodes

```
sh-5.2# ovn-nbctl lr-policy-list green_green.network_ovn_cluster_router
Routing Policies
      9001 inport == "rtos-green_green.network_ovn-worker" && ip4.dst == 100.10.0.0/16         reroute                192.168.0.2
sh-5.2# ovn-nbctl lr-policy-list blue_blue.network_ovn_cluster_router
Routing Policies
      9001 inport == "rtos-blue_blue.network_ovn-worker" && ip4.dst == 200.10.0.0/24         reroute                192.168.0.3

```

Idea1.1: However adding /16 routes will cause assymmetric reply so this can be improved further by
adding specific /24 routes to go straight to the corresponding ovn_cluster_router if we go
with Idea1. However, adding /24 routes will also have scale implications. A cluster with
500 nodes and 500 networks will end up with 250000 routes on each ovn_cluster_router.

Idea1.2: Instead of adding routes on each cluster-router, we could add routes on the transit-router
for each /24. But what would be the next hop?

#### Services

#### Network Policies and Admin Network Policies

#### Connecting Advertised UDNs

#### Disconnecting UDNs

### Testing Details

* Unit Testing details
* E2E Testing details
* API Testing details
* Scale Testing details
* Cross Feature Testing details - coverage for interaction with other features

### Documentation Details

* New proposed additions to ovn-kubernetes.io for end users
to get started with this feature
* when you open an OKEP PR; you must also edit
https://github.com/ovn-org/ovn-kubernetes/blob/13c333afc21e89aec3cfcaa89260f72383497707/mkdocs.yml#L135
to include the path to your new OKEP (i.e Feature Title: okeps/<filename.md>)

## Risks, Known Limitations and Mitigations

**Scale and Performance**

TBD

## OVN Kubernetes Version Skew

which version is this feature planned to be introduced in?
check repo milestones/releases to get this information for
when the next release is planned for

## Alternatives

### Using BGP and RouterAdvertisement API

If admin wants to connect two UDNs together then they can simply expose
the two UDNs using RouterAdvertisement CR and the router acting as gateway
outside the cluster will learn the pod subnet routes from both these UDNs
thereby acting as the router and being able to connect pods together.
Today we add ACLs, but those could be removed to accommodate the connectivity.

**Pros**

* No need for a new API to connect UDNs together
* Implementation changes are also almost nil

**Cons**

* Some users might want to use BGP and UDNs only for the pods to be
  directly accessible from external destinations and within the cluster
  they might still want to respect segmentation - this is not possible
  if we declare exposing UDNs subnets using BGP also means they are now
  connected together. So basically the current API didn't account for BGP
  to solve connecting UDNs problem.
* Some users might not want to use BGP at all because that makes assumptions
  on having the right fabric at the cluster infrastructure layer like presence
  of FRR-K8s which might not be the case always - then how can they connect
  their UDNs together? Solution needs to be OVN-native where-ever possible.
* Users cannot ask for partial v/s full connectivity across these UDNs - example
  allow only services not pods.
* Hardware offload not possible since traffic is not curtailed only to the OVS stack

### Using alternative OVN connection Ideas for topologies

**Idea3: Connect the transit switches of the two UDNs together (either using an interconnect switch/router in the middle OR directly using remote ports)**

This idea was dropped because:
* Additional burden for the user and new API change: Given each transit
  router today has the same 100.88/16 subnet that is not configurable in
  UDNs on day2, if we want to connect transit switches directly together,
  we would need support day2 changes by exposing this subnet via the API
  and to place an additional constraint of either having unique transit-subnets
  in each of the UDNs that have to be connected together. This being an internal
  implementation details seems like a hard requirement for the user to know and
  configure.
* Upgrades and disruption of traffic: Users using UserDefinedNetworks feature
  in the older versions will need to make the above disruptive change of transit
  switch subnets that will lead to traffic disruption.
* Transitivity of interconnections will happen here which is not desired: If
  we now wanted to connect yellow with green, yellow would end up being connected
  to blue automatically which is not desired, in the case where it's directly
  connecting with remote ports.

**Idea3.1: Using NATs**

To avoid having to change the transit switch subnet ranges, NAT to a particular
UDN IP before sending it into the interconnect router - but I assume we want to
preserve srcIPs of the pods. This would also add additional unwanted complexity
so this idea was also not pursued.

## Unresolved

* NodeNetworkCR v/s annotations
* Idea1 v/s Idea2
* Controllers design
* decide whether it will transit switch OR transit router after looking at L2

## [Bonus for future] Connecting UDNs across clusters

This section covers some of the design complications that need to
be ironed out before we can fully design and implement connecting
UDNs across clusters. This is why this part as removed from the scope
of the enhancement. The only bit to consider is that we don't do an
API that locks us into a single cluster concept. So this section serves
as a reminder for the future work of how the existing API could be
leveraged for adding support for connecting UDNs across clusters.

TBD to fill in sections here.
