# OKEP-5224: Connecting UserDefinedNetworks

## Problem Statement

The problem we are trying to solve here is how can a user connect
multiple [UserDefinedNetworks] or [ClusterUserDefinedNetworks] better known
as “Connecting (C)UDNs”. The assumption here is that each (C)UDN is an
isolated island and in order to make any two (C)UDNs communicate with
each other they have to be explicitly requested to be connected.

[UserDefinedNetworks]: https://ovn-kubernetes.io/api-reference/userdefinednetwork-api-spec/#userdefinednetwork
[ClusterUserDefinedNetworks]: https://ovn-kubernetes.io/api-reference/userdefinednetwork-api-spec/#clusteruserdefinednetwork

### User Personas:

* `Admin`: Cluster administrator who has the highest priviledge access to the cluster.
  Admin creates the namespaces for each tenant in the cluster.
* `Tenant`: A tenant is an end user who owns the resources (workloads, policies,
  services, udns) created in one or more namespaces that are designated to them by admins.
  They cannot update or mutate the namespace(s) but they live within the namespace(s) boundary
  controlled using RBAC.

## Goals

* Connecting and disconnecting UDNs and CUDNs within the same cluster
  aka intra-cluster-udn-connectivity for admins (can be done as day0 or day2).
  * Support for connecting `CUDN<->UDN<->NAD` across namespaces.<sup>[footnote]</sup>
  * Support for connecting `Layer3<->Layer3`, `Layer2<->Layer2` and `Layer3<->Layer2` type networks.
  * Support for only `role:Primary` type CUDN, UDN and NAD connections. From
    henceforth all references to CUDN/UDNs implies NADs as well.
* Connecting and disconnecting UDNs owned by the same tenant in a self-serve
  fashion without any admin intervention. A tenant should be able to connnect
  the networks that belong to them.
* Support for pods, services (clusterIPs only since expectation today is that
  NodePorts and LoadBalancer type services are already exposed externally and
  hence reachable across UDNs) and network policies across the connected UDNs and CUDNs.
* Ensure the proposed API is expandable for also connecting and disconnecting
  UDNs or CUDNs across clusters in the future if need be.

[footnote] Food for thought: Currently, there is no need to consider connecting
UDNs within the same namespace since there is no support for more than 1 primary-UDN
in a namespace and given each pod would have connection to its primary and secondary
UDNs in its namespace, there is currently no use case for connecting different UDNs
in the same namespace. However there is a future enhancement on [multiple primary
UDNs] which if accepted can automatically leverage the work happening in this
enhancement.

[multiple primary UDNs]: https://github.com/ovn-kubernetes/ovn-kubernetes/pull/5255

## Future Goals

* Tenants being able to request for interconnecting their networks
  with other tenants.
    * We need to understand exact use cases for why we need tenants to initiate
      connection requests with other tenants. How will a tenant know other tenants
      that exist on the cluster? In cases like finance tenant wanting to use IT
      tenant's services, they could open a ticket to the admin to make that happen.
    * Regardless, the tenant API might need more RBAC considerations if we get such use cases
* Admins being able to connect `role:Secondary` UDNs/CUDNs/NADs. The reason why this is a future
  goal is because of the following reasons:
    * For UDNs, given `SecondaryUserDefinedNetworkSelector` uses a combination of
      namespace selector and network selector, a tenant could change
      the labels on the UDN thereby causing security issues. Hence this
      type of selector support will come later where we might want to add a well-known
      label to be able to leverage selectors.
    * The secondary networks don't have the same level of topology parity as
      primary networks today. So there is no support for transit router or
      egress gateway today in `Layer2` secondary networks. Hence we would need
      to add the transit router to the secondary network `Layer2` topology as well
      which can come in the future. `Layer3` networks don't have the same issue
      as the required topology parity exists but given we'd need that change for
      `Layer2` might as well do the full secondary networks connect support in future.

## Non-Goals

* Connecting two UDNs with overlapping subnets won’t be supported.
  This is a valid use case scenario but hard to implement. Instead we need
  to perhaps allow these networks to communicate via the exposed clusterIPs.
* Support for this feature in non-interconnect architecture is out of scope.
* Support for `localnet` type network connections is out of scope.
  * `localnet` type networks can be connected together using bridges already
  * `localnet`<->`layer2` or `layer3` will not be covered here.
* Supporting Live Migration and PersistentIPs across connected UDNs.
* Connecting and disconnecting UDNs and CUDNs across two clusters
  aka inter-cluster-udn-connectivity for admins (can be done as day0 or day2).
  The plan is to be able to do this using EVPN protocol. See enhancement [on EVPN]
  to see how it is planned to be leveraged as the mode to connect different UDNs
  across clusters. But regardless, the API bits around how to request connectivity
  needs more fleshing and EVPN's primary use case is not about connecting UDNs across
  clusters.

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
communicate via APIs like frontend needing to access database through
the backend service.

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

When the two networks have overlapping subnets, using service clusterIP
as a way to allow communication is valid use case.

### Story 3: Allowing connectivity between two networks with overlapping subnets through exposed clusterIP services

**As a cluster admin**, **I want** to allow tenant-Green to be able to access
the clusterIP microservice exposed by tenent-Blue even though tenant-Green and tenant-Blue
have overlapping pod subnets.

Example: IPAddress Management is hard. It is not possible to predict the future
of how all applications will communicate with one another. There is a chance
app-type-green and app-type-blue got the same pod subnets and now they need
to talk to each other. While allowing direct connection of the two overlapping
pod subnet networks is hard, a middleground is to use clusterIPs to communicate.


### Story 4: Merging two tenants (maybe even temporarily)

**As a cluster admin**, **I want** to now connect my existing UDN-Blue and UDN-Green
**so that** they can communicate with each other as if being part of the same network.

Example: The admin created UDN-Blue and UDN-Green that were isolated
initially but now wants to treat them as being part of the same network.
Pods cannot be rewired given they are part of two different UDNs. Say
when different teams within the same organization operate as separate
tenants but collaborate on a project requiring unrestricted communication
for a period of time and then want to disconnect from each other later.

### Story 5: Same UDN across multiple clusters must be connected by default

**As a cluster admin**, **I want** to connect my workloads which are part
of the same UDN but are in different clusters. 

Example: The user has split their workloads into multiple clusters thereby
the same UDN could be fragmented across different clusters. These pods should
be able to communicate with each like it would be if they were on the same
cluster but be isolated from other workloads.

NOTE: Cross cluster UDN connecting is not targeted in this OKEP, but is
covered here since connecting UDNs within the cluster is the first step here.

### Story 6: Connecting mixed type networks together (Layer2 Network workloads with Layer3 Primary/Default Network)

**As a cluster admin**, **I manage** isolated tenants (a bunch of
interconnected pods) and share them with my users. Every tenant has
its own Primary Layer3 UDN to emulate its own cluster network, but
isolate it from the other tenants. Northbound/internet traffic from my
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

(TBD: Why is the tenant network just not layer2 then?)

### Story 7: Migration of connected tenant workloads from other platforms like NSX into OVN-Kubernetes

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

### Story 8: Tenant self-connection without admin intervention

**As a tenant owner**, **I want** to connect my workloads which are part
of two different namespaces (networks) in the same cluster with no
admin intervention.

Example: If the same tenant owns microservices across 5 namespaces that
are 5 different networks, there should be provision for this tenant to
connect their networks together without the requirement for an admin to
intervene. The reason could be the same as the above admin user stories
but this story captures the tenant intent with no admin intervention

## Proposed Solution

This section tries to answer: How can an admin declare two (C)UDNs to be connected?

### Config for enabling the feature

The whole feature will be behind a config flag called `--enable-connecting-networks`
which has to be set to true to get this feature.

### API Naming [WIP]

We think its better to call the API as one of the following:

* VirtualPrivateConnect (VPC)
* VirtualPrivateNetworkConnect (VPNC)

for easier adoption across end-users suiting to industry standards.
(A UDN maps to Subnet and UDNConnect maps to a VPC)
* https://docs.aws.amazon.com/vpc/latest/userguide/what-is-amazon-vpc.html
* https://techdocs.broadcom.com/us/en/vmware-cis/nsx/vmware-nsx/4-2/administration-guide/nsx-multi-tenancy/nsx-virtual-private-clouds.html
* https://kubeovn.github.io/docs/v1.10.x/en/advance/vpc-peering/
* https://cloud.google.com/vpc/docs/vpc
* https://learn.microsoft.com/en-us/azure/virtual-network/virtual-networks-overview

[TBD] Get consensus during reviews to change from `AdminNetworkConnect`
to `VirtualPrivateNetworkConnect` and `AdminVirtualPrivateNetworkConnect`?
Or should we keep it as `NetworkConnect` and `AdminNetworkConnect`?

### API Details [WIP]

This enhancement proposes a new CRD to allow admins to request multiple networks
to be connected together.

`AdminNetworkConnect` CR will be cluster-scoped resource and admin-only API
that will allow admins to select multiple different networks that need to be
connected together. It will also allow specifying what level of connectivity is
expected. See the API definitions for more details.

Admins can create a `AdminNetworkConnect` CR which
signals the controller to connect the topologies of the UDNs selected by this CR.

```go
TBD - waiting for the YAML fields to be more or less agreed on first
```

Sample YAML:

```yaml
apiVersion: k8s.ovn.org/v1
kind: VirtualPrivateNetworkConnect
metadata:
  name: colored-enterprise
spec:
  networkSelectors: # can match on UDNs and/or CUDNs
    - networkSelectionType: ClusterUserDefinedNetworks
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchExpressions:
          - key: kubernetes.io/metadata.name
            operator: In
            values:
            - blue-network
            - green-network
    - networkSelectionType: PrimaryUserDefinedNetworks
      primaryUserDefinedNetworkSelector:
        namespaceSelector:
          matchExpressions:
          - key: kubernetes.io/metadata.name
            operator: In
            values:
            - yellow
            - red
  connectSubnets: # can have at most 1 CIDR for each family type
    - 192.168.0.0/10
  featuresEnabled:
    - PodNetworkConnectivity
      # enabled through a well-known label on service if we need finer-grained control
    - ClusterIPServicesConnectivity
    - NetworkPolicyEnforcement
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
  networks you should have N×M IPs for allocation in this slice. This value is expected
  to be configured uniquely across multiple UDNConnect CRs that select the same network
  i.e. If a given network is selected as part of more than 1 connect API, each of those
  connect APIs must have a unique subnet. If a user accidentally configures overlapping
  values, we must have a status check to block and report this in the status (details to be fleshed out).

#### API Validations


#### Valid ways of Connecting/Selecting/Disconnecting UDNs

TBD
* A network could be selected as part of more than one network connnect, so what is the expectation
  when two Connect APIs are designed that select the exact same set of networks? How do
  we control validation of such scenarios?
* Or when a second connect API selects a subset of networks in the other connect API -> just
  ECMP connections? Or duplication on OVN layer? Or we want to prevent this - this is a hard
  problem -> I guess at the end of the day this problem exists for all KAPIs we just need rules
  to be additive.

### Implementation Details

### Network Topology

This section covers the topology changes that will be introduced to
connect networks together.

**Connecting Layer3 type networks together**

The below diagram shows the overlay part of the OVN Topology that OVN-Kubernetes
creates for 3 UDN networks (blue, green and yellow) across two nodes.

![3-isolated-l3-networks](https://raw.githubusercontent.com/tssurya/ovn-kubernetes/connecting-udns-okep/docs/okeps/okep-5224-connecting-udns/images/l3-connecting-udns-0.png)

Let’s take a look at how we can connect a blue network, green network and yellow network at the OVN layer.
NOTE: Besides the finalized proposal we have below for the topology change, there were other ideas
which were discarded and listed in the alternatives section.

**Add a new transit router between the ovn_cluster_routers of the networks**

When the `AdminNetworkConnect` CR gets created selecting these three networks, the controller
will make the following changes to the topology:
* Create a distributed `colored-enterprise_interconnect-router` (basically a
  transit-router type in OVN used for interconnection)
* Connect the `colored-enterprise_interconnect-router` to the `ovn_cluster_router`’s
  of the blue, green and yellow networks on each node using patch ports and tunnelkeys
* The port's will have IPs allocated from within the `connectSubnet`
  value provided on the CR (default=`192.168.0.0/10`). For connecting N networks on M nodes
  we would need 2×(N×M) IPs from the `connectSubnet`. See the controller design section for
  more details

![blue-green-yellow-l3-connect-idea](https://raw.githubusercontent.com/tssurya/ovn-kubernetes/connecting-udns-okep/docs/okeps/okep-5224-connecting-udns/images/l3-connecting-udns-v1.png)

So in this proposal it will be 1 new transit router per connect API.

**Connecting Layer2 type networks together**

The below diagram shows the overlay part of the OVN Topology that OVN-Kubernetes
creates for 3 UDN networks (blue, green and yellow) across two nodes.
NOTE: The topology representation is that of the new upcoming Layer2 topology.
The dotted pieces in network colors represent the bits in design and not there yet.
See enhancement on [new Layer2 topology] for more details on the new design.

[new Layer2 topology]: https://github.com/ovn-kubernetes/ovn-kubernetes/pull/5097

![3-isolated-l2-networks](https://raw.githubusercontent.com/tssurya/ovn-kubernetes/connecting-udns-okep/docs/okeps/okep-5224-connecting-udns/images/l2-connecting-udns-0.png)

**Add a new transit router between the transit routers of the networks**

When the `AdminNetworkConnect` CR gets created selecting these three networks, the controller
will make the following changes to the topology:
* Create a distributed `colored-enterprise_interconnect-router` (basically a
  transit-router type in OVN used for interconnection)
* Connect the `colored-enterprise_interconnect-router` to the `transit-router`’s
  of the blue, green and yellow networks on each node using patch ports and tunnelkeys
* The port's will have IPs allocated from within the `connectSubnet`
  value provided on the CR (default=`192.168.0.0/10`). For connecting N networks on M nodes
  we would need 2×(N×M) IPs from the `connectSubnet`. See the controller design section for
  more details

![blue-green-yellow-l2-connect-idea](https://raw.githubusercontent.com/tssurya/ovn-kubernetes/connecting-udns-okep/docs/okeps/okep-5224-connecting-udns/images/l2-connecting-udns-v1.png)

**Connecting mixed type (layer3 and/or layer2) networks together**

The below diagram shows the overlay part of the OVN Topology that OVN-Kubernetes
creates for 3 UDN networks (blue(l3), green(l3) and yellow(l2)) across two nodes.
NOTE: The yellow topology representation is that of the new upcoming Layer2 topology.
The dotted pieces in network colors represent the bits in design and not there yet.

![3-isolated-mixed-networks](https://raw.githubusercontent.com/tssurya/ovn-kubernetes/connecting-udns-okep/docs/okeps/okep-5224-connecting-udns/images/mixed-connecting-udns-0.png)

**Add a new transit router between the ovn_cluster_router's and transit-routers of the networks**


When the `AdminNetworkConnect` CR gets created selecting these three networks, the controller
will make the following changes to the topology:
* Create a distributed `colored-enterprise_interconnect-router` (basically a
  transit-router type in OVN used for interconnection)
* Connect the `colored-enterprise_interconnect-router` to the `transit-router`
  of the yellow network and `ovn_cluster_router`'s of the blue and green networks on each nod
  using patch ports and tunnelkeys
* The port's will have IPs allocated from within the `connectSubnet`
  value provided on the CR (default=`192.168.0.0/10`). For connecting N networks on M nodes
  we would need 2×(N×M) IPs from the `connectSubnet`. See the controller design section for
  more details

![blue-green-yellow-l2-connect-idea1](https://raw.githubusercontent.com/tssurya/ovn-kubernetes/connecting-udns-okep/docs/okeps/okep-5224-connecting-udns/images/mixed-connecting-udns-v1.png)

### Pods

**Layer3**

In order to ensure pods in different connected networks can talk to each other we will need to
add appropriate routes on the routers.

* Create policies on each connected network's `ovn_cluster_router` that steers the traffic
  towards the other networks' subnet to the `vpnc-connect-router`.
* On the interconect transit router we will add specific per network-node subnet routes
  that steers the traffic to the correct node.

These routes will look like this in the blue and green sample topology:

* Add logical router policies to `blue_blue.network_ovn_cluster_router` that steers
  traffic towards the green UDN’s subnet to interconnect router on all nodes
* Add logical router policies to `green_green.network_ovn_cluster_router` that steers
  traffic towards the blue UDN’s subnet to interconnect router on all nodes
* On the `colored-enterprise_interconnect-router` add routes to specific node subnet slices
  of the blue and green ovn_cluster_routers on the respective nodes.

```
sh-5.2# ovn-nbctl lr-policy-list green_green.network_ovn_cluster_router
Routing Policies
      9001 inport == "rtos-green_green.network_ovn-worker" && ip4.dst == 100.10.0.0/16         reroute                192.168.0.5
sh-5.2# ovn-nbctl lr-policy-list blue_blue.network_ovn_cluster_router
Routing Policies
      9001 inport == "rtos-blue_blue.network_ovn-worker" && ip4.dst == 200.10.0.0/16         reroute                192.168.0.3
sh-5.2# ovn-nbctl lr-route-list colored-enterprise-interconnect-router 
IPv4 Routes
Route Table <main>:
            100.10.0.0/24               192.168.0.6 dst-ip
            100.10.2.0/24               192.168.0.2 dst-ip
            200.10.0.0/24               192.168.0.8 dst-ip
            200.10.2.0/24               192.168.0.4 dst-ip

```

**Layer2**

In order to ensure pods in different connected networks can talk to each other we will need to
add appropriate routes on the routers.

* Create policies on each connected network's `transit-router` that steers the traffic
  towards the other networks' subnet to the `vpnc-connect-router`.
* On the interconect transit router we will add the specific network subnet routes
  that steers the traffic to the ....

These routes will look like this in the blue and green sample topology:

* Add logical router policies to `blue_blue.network_ovn_cluster_router` that steers
  traffic towards the green UDN’s subnet to interconnect router on all nodes
* Add logical router policies to `green_green.network_ovn_cluster_router` that steers
  traffic towards the blue UDN’s subnet to interconnect router on all nodes
* On the `colored-enterprise_interconnect-router` add routes to specific node subnet slices
  of the blue and green ovn_cluster_routers on the respective nodes.

```
sh-5.2# ovn-nbctl lr-policy-list green_green.network_ovn_cluster_router
Routing Policies
      9001 inport == "rtos-green_green.network_ovn-worker" && ip4.dst == 100.10.0.0/16         reroute                192.168.0.5
sh-5.2# ovn-nbctl lr-policy-list blue_blue.network_ovn_cluster_router
Routing Policies
      9001 inport == "rtos-blue_blue.network_ovn-worker" && ip4.dst == 200.10.0.0/16         reroute                192.168.0.3
sh-5.2# ovn-nbctl lr-route-list colored-enterprise-interconnect-router 
IPv4 Routes
Route Table <main>:
            100.10.0.0/24               192.168.0.6 dst-ip
            100.10.2.0/24               192.168.0.2 dst-ip
            200.10.0.0/24               192.168.0.8 dst-ip
            200.10.2.0/24               192.168.0.4 dst-ip

```

**Mixed**

#### Overlapping Subnets

Direct pod to pod connectivity of two networks
is not supported in phase1. Conflict overlap detection
and notification via API must be implemented.

### Services

If the `ClusterIPServicesConnectivity` is explicitly requested, then
it means the end user wants the services of both networks to be connected.
In this case, we need to modify the services controller to program
the OVN load balancers on the local network switches also for
connected networks so that once the DNAT to the backend happens it takes
the same path as the policies and routes outlined in the Pods section.

#### Overlapping Subnets connected via clusterIPs

TBD - see if there are simpler ways via sNATing and masquerading we
use for hairpin services that can be leveraged?
Can I program the load balancer such that the masqueradeIP is set
and hence in addition to DNAT we also SNAT to the origin network's
masqueradeIP and that takes care of cross network traffic.
Need to ensure the existing masqueradeIP is not used within/between
networks.

### Network Policies and Admin Network Policies

### Controller Design / Changes to Components [WIP]

**Cluster Manager**

A new `vpnc-controller` will be created that watches the
`VirtualPrivateNetworkConnect` objects and does the following:

1) It is responsible to allocate a /31 range for each node-network pair:
  * It get's the `connectSubnet` value from the VPNC and allocates
    a `/31` point to point link subnet for each node-network pair
  * These values are annotated on the `VirtualPrivateNetworkConnect`
    CRD for it to be then consumed on the `ovnkube-controller` side.
    Sample:
    ```yaml
    k8s.ovn.org/vpc-node-network-subnets: {
      Network1Node1: 192.168.0.0/31,
      Network1Node2: 192.168.0.2/31,
      Network1Node3: 192.168.0.4/31,
      Network2Node1: 192.168.0.6/31,
      Network2Node2: 192.168.0.8/31,
      Network2Node3: 192.168.0.10/31,
      ...
    }
    ```
    We will need N×M subnets for N networks and M nodes for Layer3
    and N×1 subnets for Layer2.
2) It is responsible for allocating the global tunnel key for each
  `vpnc-connect-router`:
  * [option1] Similar to the networkID allocator, we need to add a new
    global tunnelID allocator that reserves the 4096 IDs for maximum
    possible networksIDs and then allocates the new ones dynamically. This
    ensures we don't reuse already allocated networkIDs.
  * [option2] We reuse the networkID allocator but expand its limits
    beyond 4096 as maximum.
  * These values are annotated on the `VirtualPrivateNetworkConnect`
    CRD for it to be then consumed on the `ovnkube-controller` side.
    Sample:
    ```yaml
    k8s.ovn.org/vpnc-node-tunnel-keys: '{"ovn-control-plane":5000,"ovn-worker":5001,"ovn-worker2":5002}'
    ```
4) Status conditions will be updated based on feedback of setup
 from the `ovnkube-controllers` in different zones.

**OVN-Kubernetes Controller**

A new `vpnc-controller` will be created that watches the
`VirtualPrivateNetworkConnect` objects and does the following:

1) Based on the network selectors, creates the
   necessary network topology changes mentioned
   in the above section:
   * Creates the `vpnc-connect-router`
   * Creates the ports connecting each Layer3 `ovn_cluster_router` and
     each Layer2 `transit-router` to the `vpnc-connect-router`
   * Adds the necessary logical router policies on each network router
   * Adds the necessary logical router static routes on the `vpnc-connect-router`
2) Reads the `k8s.ovn.org/vpc-node-network-subnets`
   annotation set by the cluster manager and allocates
   an IP from /31 to each point to point router port link
   and uses that IP to create the ports
3) Reads the `k8s.ovn.org/vpnc-node-tunnel-keys` and uses
   the tunnel key to create the `vpnc-connect-router`
4) Within each `vpnc-connect-router`, using the `networkID` it is possible
   to obtain N tunnel-keys for the local ovnkube-controllers without
   any extra computation.

### Proof Of Concept/Implementation PRs in Progress:

* API:
* Layer3<->Layer3 Connectivity:
* Layer2<->Layer2 Connectivity:
* Layer3<->Layer2 Connectivity:

## Self Tenant Connect/Disconnect API [WIP]

* Introduct Tenant version of the API `TenantNetworkConnect`
  that allows only setting UDN selector (no CUDNs or NADs)
* [option1] Webhook
* [option2] Use controller logic itself similar to webhook to check for `SubjectAccessReview`
* [option3] Well known labels/annotations
* [option4] Owner Reference Validation/Service Accounts

Needs to be backwards compatible to allow for
cross-tenant compatibility in the future

## Cross Feature Interaction

### Connecting Advertised UDNs

The ACLs that are added to block traffic
routing between the two UDNs will be
removed so that traffic can flow between
networks once connected.

A side caveat here is instead of the edge
router doing the routing, OVN-Kubernetes
itself will take care of routing the traffic.

### Connecting UDNs with no overlay mode

This feature will not be supported if overlay
tunnel encapsulation is turned off. Cluster Manager
will perform the necessary steps to validate and
emit a message to the end user reporting an error
if they tried to connect UDNs when overlay is turned off.


## Testing Details

* Unit Testing details - All code modules will have necessary unit tests
* E2E Testing details - Entire feature will have E2E and Integration testing
* API Testing details - CEL rules and API validation will have its own apply test suite
* Scale Testing details - Bare minimum set of scale testing will be done for this feature
  although we probably need to fix UDN Scale first
* Cross Feature Testing details - coverage for interaction with other features like BGP
  and no-overlay mode

## Documentation Details

* User guide will be updated for the feature on ovn-kubernetes.io website
* Blog posts and easy to get started guides will be done on ovn-kubernetes.io website
* Developer docs whereever possible will be added

## Risks, Known Limitations and Mitigations

### Scale and Performance [WIP]

* Adding a /16 route to each router for each connected peer network and then adding /24 routes
  for each node-network combo on the transit router has scale implications specially on clusters
  with large nodes. A cluster with 500 nodes and 500 networks will end up with 250000 routes
  on each transit router distributed half on the node.
* In future there is a requirement to not create OVN constructs for UDNs
  on those nodes where we know there won't be pod's scheduled. This is to
  optimize resource consumption on large scale clusters with 1000 nodes where
  only 10 nodes are going to host UDNs. Whatever topology we pick here must
  work for that future enhancement as changing topologies is not really
  encouraged.
* The annotation: 

![blue-green-yellow-l3-connect-scale](https://raw.githubusercontent.com/tssurya/ovn-kubernetes/connecting-udns-okep/docs/okeps/okep-5224-connecting-udns/images/l3-connecting-udns-scale.png)

## OVN Kubernetes Version Skew

Targeting release 1.2

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

### Using alternative connection ideas on OVN topology level

See [discarded-alternatives.md](discarded-alternatives.md) for details
on alternative topology connection ideas that were considered but discarded.

### Using NodeNetwork CR instead of Annotations on Connect API

NOTE: Having the `NodeNetwork` CRD as an internal data storage API might be more ideal
to store the above information internally as this needs to be persisted into etcd on reboots.
It will simplify the annotations and also help with scale since there are known issues
around patching of annotation and waiting for reading and unmarshalling the annotation
impacts scale. But this theory is not proven yet and there was a concern of nodes getting
polluted with events?? (TBD: need to think why we didn't go this angle - circle back to meeting recording)

## Dependencies

* This enhancement depends on the [new Layer2 topology] changes getting merged first.
