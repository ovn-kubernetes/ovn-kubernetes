# OKEP-5552: Dynamic UDN Node Allocation

* Issue: [#5552](https://github.com/ovn-org/ovn-kubernetes/issues/5552)

## Problem Statement

When scaling UDNs, the control-plane cost of rendering a topology is high. This is the core limiting factor to
being able to scale to 1000s of UDNs. While there are plans to also improve network controller performance with UDNs,
there is still valuable savings to be had by not rendering UDNs on nodes where they are not needed.

An example use case where this makes sense is when a Kubernetes cluster has its node resources segmented per tenant. In
this case, it only makes sense to run the tenant network (UDN) on the nodes where a tenant is allowed to run pods. This
allows for horizontal scaling to much higher number of overall UDNs running in a cluster.

## Goals

 * To dynamically allow the network to only be rendered on specific nodes.
 * To increase overall scalability of the number UDNs in a Kubernetes cluster with this solution.
 * To increase the efficiency of ovnkube operations on nodes where a UDN exists, but is not needed.

## Non-Goals

 * To fully solve control plane performance issues with UDNs. There will be several other fixes done to address that
   outside of this enhancement.
 * To provide any type of network security guarantee about exposing UDNs to limited subset of nodes.

## Introduction

The purpose of this feature is to add a configuration knob that users can turn on which will only render UDNs on nodes
where pods exist on that UDN. This feature will allow for higher overall UDN scale and less per-node control plane resource usage
under conditions where clusters do not have pods on every node, with connections to all UDNs. For example, if I have
1000 UDNs and 500 nodes, if a particular node only has pods connected to say 200 of those UDNs, then my node is only
responsible for rendering 200 UDNs instead of 1000 UDNs as it does today.

This can provide significant control plane savings, but comes at a cost. Using the previous example, if a pod is now
launched in UDN 201, the node will have to render UDN 201 before the pod can be wired. In other words, this introduces
a one time larger pod latency cost for the first pod wired to the UDN. Additionally, there are more tradeoffs with other
feature limitations outlined later in this document.

## User-Stories/Use-Cases

Story 1: Segment groups of nodes per tenant

As a cluster admin, I plan to dedicate groups of nodes to either a single tenant or small group of tenants. I plan
to create a CUDN per tenant, which means my network will only really need to exist on this group of nodes. I would
like to be able to limit this network to only be rendered on that subset of nodes.
This way I will be able to have less resource overhead from OVN-Kubernetes on each node,
and be able to scale to a higher number of UDNs in my cluster.

## Proposed Solution

The proposed solution is to add a configuration knob to OVN-Kubernetes, "--dynamic-udn-allocation", which will enable
this feature. Once enabled, NADs derived from CUDNs and UDNs will only be rendered on nodes where there is a pod
scheduled in that respective network. Additionally, if the node is scheduled as an Egress IP Node for a UDN, this node
will also render the UDN.

When the last pod on the network is deleted from a node, OVNK will not immediately
tear down the UDN. Instead, OVNK will rely on a dead timer to expire to conclude that this UDN is no longer in use and
may be removed. This timer will also be configurable in OVN-Kubernetes as "--udn-deletion-grace-period".

### API Details

There will be no API changes. There will be new status conditions introduced in the section below.

### Implementation Details

In OVN-Kubernetes we have three main controllers that handle rendering of networking features for UDNs. They exist as
 - Cluster Manager - runs on the control-plane, handles cluster-wide allocation, rendering of CUDN/UDNs
 - Controller Manager - runs on a per-zone basis, handles configuring OVN for all networking features
 - Node Controller Manager - runs on a per-node basis, handles configuring node specific things like nftables, VRFs, etc.

With this change, Cluster Manger will be largely untouched, while Controller Manager and Node Controller Manager will be
modified in a few places to filter out rendering UDNs when a pod doesn't exist.

#### Internal Controller Details

In OVN-Kubernetes we have many controllers that handle features for different networks, encompassed under three
controller manager containers. The breakdown of how these will be modified is outlined below:

- Cluster Manager
   - UDN Controller - No change
   - Route Advertisements Controller - No change
   - Egress Service Cluster - Doesn't support UDN
   - Endpoint Mirror Controller - No change
   - EgressIP Controller - No change
   - Unidling Controller - No change
   - DNS Resolver - No change
   - Network Cluster Controller - Modified to report status correctly and filter out nodes not serving the UDN
- Controller Manager (ovnkube-controller)
   - Default Network - No change
   - NAD Controller - Modified to ignore NADs that belong to UDNs that do not match nodeSelector
- Node Controller Manager
   - Default Network - No change
   - NAD Controller - Modified to ignore NADs that belong to UDNs that do not match nodeSelector

The resulting NAD Controller change will filter out NADs that do not apply to this node, stopping NAD keys from being
enqueued to the Controller Manager/Node Controller Manager's Network Manager. Those Controller Managers will not need
to create or run any sub-controllers for nodes that do not have the network. To do this cleanly, NAD Controller will be
modified to hold a filterFunc field, which the respective controller manager can set in order to filter out NADs. For
Cluster Manager, this function will not apply, but for Controller Manager and Node Controller Manager it will be a function
that filters based on if the UDN is serving pods on this node.

#### New Pod Controller

In order to know whether the Managers should filter out a UDN, a pod controller and egress IP controller will be used
in the Managers to track this information in memory. The pod controller will be a new level driven controller for
each manager. For Egress IP, Controller Manager already has an EIP controller that can be leveraged to hold this information.
For Node Controller Manager, a new EIP controller will need to be added.

#### Other Controller Changes

The network controllers: Layer3, Layer2, Localnet will need to filter out their node handlers for nodes where the UDN
is not rendered. Upon receiving events, they will also call the filter function from the NAD Controller to determine if
they need to ignore resources or not.

Upon UDN activation of a node, these controllers will need to receive events in order to reconcile the new node. The reconcile
function for network controller will be leveraged to find the new resources and queue them to the retry framework
with no backoff.

#### Status Condition Changes

A new status condition will be added to CUDN/UDN that will indicate how many nodes are serving a network:
```yaml
status:
  conditions:
    - type: NodesSelected
      status: "True"
      reason: DynamicAllocation
      message: "5 nodes rendered with network"
```

A new StatusManager instance will be used for CUDN/UDN to enable per "zone" reporting that will allow for cumulative node
calculations and overall status reporting.

Additionally, another status condition will be added for when the last pod or egress IP is removed from the node, and
the UDN is scheduled for garbage collection. A status condition with a timestamp will be placed on the UDN for the
respective node:

```yaml
status:
  conditions:
  - type: NodeUnused
    status: "True"
    reason: LastPodRemoved
    message: "The last pod on node worker-1 using this UDN has been removed"
    lastTransitionTime: "2025-09-15T14:00:00Z"
    nodeName: worker-1
```

This timer will be used in coordination with the "--udn-deletion-grace-period" delay to determine when the UDN will
be removed from the node.

### Testing Details

* Unit Tests will be added to ensure the behavior works as expected, including checking that
OVN switches/routers are not created when excluded from the nodeSelector, etc.
* E2E Tests will be added to create a CUDN/UDN with the feature enabled and ensure pod traffic works correctly between nodes.
* Benchmark/Scale testing will be done to show the resource savings of 1000s of nodes with 1000s of UDNs.

### Documentation Details

* User-Defined Network feature documentation will be updated with a user guide for this new feature.

## Risks, Known Limitations and Mitigations

No known risks, but limitations exist for features. As UDN is only supported with IC, there will be no OVN central support
for this feature. Additionally, some features will be impacted. Kubernetes services which use NodePort or ExternalIP and
external traffic policy set to "cluster", will not work when sending traffic to nodes where the UDN is not rendered. Additionally,
careful planning would need to take place in order to ensure that LoadBalancer type services work correctly. For example,
if using MetalLB, the MetalLB speakers would need to be scheduled on nodes where the UDN will always be rendered. This
could be achieved by running a daemonset of pods attached to the UDN on those nodes.

## OVN Kubernetes Version Skew

Targeted for release 1.2.

## Alternatives

Specifying a NodeSelector in the CUDN/UDN CRD in order to determine where a network should be rendered. This was the
initial idea of this enhancement, but was evaluated as less desirable than dynamic allocation. The dynamic allocation
provides more flexibility without a user/admin needing to intervene and update a CRD.

## References

None
