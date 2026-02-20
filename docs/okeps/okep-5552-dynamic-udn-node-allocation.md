# OKEP-5552: Dynamic UDN Node Allocation

* Issue: [#5552](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/5552)

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

## Future Goals

 * Potentially enabling this feature on a per UDN basis, rather than globally.

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

When the last pod on the network is deleted from a node, OVNK will not immediately tear down the UDN.
Instead, OVNK will rely on a dead timer to expire to conclude that this UDN is no longer in use and
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

* Cluster Manager
  * UDN Controller — No change
  * Route Advertisements Controller — No change
  * Egress Service Cluster — Doesn't support UDN
  * Endpoint Mirror Controller — No change
  * EgressIP Controller — No change
  * Unidling Controller — No change
  * DNS Resolver — No change
  * Network Cluster Controller — Modified to report status and exclude nodes not serving the UDN
* Controller Manager (ovnkube-controller)
  * Default Network — No change
  * NAD Controller — Ignore NADs for UDNs that are not active on this node (no pods for the UDN and not an EIP node)
* Node Controller Manager
  * Default Network — No change
  * NAD Controller — Ignore NADs for UDNs that are not active on this node (no pods for the UDN and not an EIP node)

The resulting NAD Controller change will filter out NADs that do not apply to this node, stopping NAD keys from being
enqueued to the Controller Manager/Node Controller Manager's Network Manager. Those Controller Managers will not need
to create or run any sub-controllers for nodes that do not have the network. To do this cleanly, NAD Controller will be
modified to hold a filterFunc field, which the respective controller manager can set in order to filter out NADs. For
Cluster Manager, this function will not apply, but for Controller Manager and Node Controller Manager it will be a function
that filters based on if the UDN is serving pods on this node.

#### New Pod/EgressIP Tracker Controller

In order to know whether the Managers should filter out a UDN, a pod controller and egress IP controller will be used
in the Managers to track this information in memory. The pod controller will be a new level driven controller for
each manager. For Egress IP, another new controller will be introduced that watches EgressIPs, Namespaces, and NADs in
order to track which NAD maps to a node serving Egress IP.

When Managers are created, they will start these Pod/EgressIP Tracker Controllers, and set a filterFunc on NAD Controller.
The filterFunc will query the aforementioned controllers to determine if the NAD being synced matches the local node. If
not, then NADController will not create the UDN controller for that network.

Additionally, the Pod/EgressIP Tracker Controllers will expose a callback function, called "onNetworkRefChange". When
the first pod is detected as coming up on a node + NAD combination, or the node activates as an Egress IP node for the
first time, onNetworkRefChange will be triggered, which allows a callback mechanism to be leveraged for events. The
Controller Manager and Node Controller Manager will leverage this callback, so that they can trigger NAD Controller to
reconcile the NAD for these events. This is important as it provides a way to signal that NADController should remove
a UDN controller if it is no longer active, or alternatively, force the NAD Controller to reconcile a UDN Controller if for example,
a new remote node has activated.

#### Other Controller Changes

The Layer3 network controller will need to filter out nodes where the UDN is not rendered. Upon receiving events,
they will query a Manager function called NodeHasNAD. Managers will export a Tracker interface, that only contains this
method for UDN Controllers to query. The implementation of NodeHasNAD will rely on the Manager querying their pod and
egress IP trackers.

Upon UDN activation of a remote node, these controllers will need to receive events in order to reconcile the new remote node. 
To do this, the corresponding tracker will trigger its callback, "onNetworkRefChange". That will trigger the Manager
to ask NAD Controller to reconcile the UDN controller belonging to this NAD. Once that Layer 3 UDN controller reconciles,
it will walk nodes and determine what needs to be added or removed. It will take the applicable nodes, set their
syncZoneICFailed status, then immediately queue the objects to the retry framework with no backoff. This will allow
the Zone IC (ZIC) controller to properly configure the transit switch with the remote peers, or tear it down, if necessary.

#### Status Condition and Metric Changes

A new status condition will be added to CUDN/UDN that will indicate how many nodes are selected for a network:
```yaml
status:
  conditions:
    - type: NodesSelected
      status: "True"
      reason: DynamicAllocation
      message: "5 nodes rendered with network"
      lastTransitionTime: 2025-09-22T20:10:00Z
```

If the status is "False", then no nodes are currently allocated for the network - no pods or egress IPs assigned.

Cluster Manager will leverage instances of EgressIP and Pod Trackers in order to use that data for updating this status.
The nodes serving a network are defined as a node with at least one OVN networked pod or having an Egress IP assigned to
it on a NAD that maps to a UDN or CUDN.

Additionally, events will be posted to the corresponding UDN/CUDN when nodes have become active or inactive for
a node. This was chosen instead of doing per node status events, as that can lead to scale issues. Using events provides
the audit trail, without those scale implications. The one drawback of this approach pertains to UDN deactivation. There
is an "udn-deletion-grace-period" timer used to delay deactivation of a UDN on a node. This is to prevent churn if a pod
is deleted, then almost immediately re-added. Without storing the timestamp in the API, we are relying internally on in
memory data. While this is fine for normal operation, if OVN-Kube pod restarts, we lose that context. However, this should
be fine as when we restart we have to walk and start all network controllers anyway, so we are not really creating a lot of
extra work for OVN-Kube here.

A metric will also be exposed which allows the user to track over time how many nodes were active for a particular
network.

### Testing Details

* Unit Tests will be added to ensure the behavior works as expected, including checking that
OVN switches/routers are not created there is no pod/egress IP active on the node, etc.
* E2E Tests will be added to create a CUDN/UDN with the feature enabled and ensure pod traffic works correctly between nodes.
* Benchmark/Scale testing will be done to show the resource savings of 1000s of nodes with 1000s of UDNs.

### Documentation Details

* User-Defined Network feature documentation will be updated with a user guide for this new feature.

## Risks, Known Limitations and Mitigations

Risks:
 * Additional first-pod cold start latency per UDN/node. Could impact pod readiness SLOs.
 * Burst reconcile load on large rollouts of pods on inactive nodes.

Limitations:
 * No OVN central support.
 * NodePort/ExternalIP services with external traffic policy mode "cluster", will not work when sending traffic to inactive nodes.
 * MetalLB must be configured on nodes where the UDN is rendered. This can be achieved by scheduling a daemonset for the designated nodes on the UDN.

## OVN Kubernetes Version Skew

Targeted for release 1.2.

## Alternatives

Specifying a NodeSelector in the CUDN/UDN CRD in order to determine where a network should be rendered. This was the
initial idea of this enhancement, but was evaluated as less desirable than dynamic allocation. The dynamic allocation
provides more flexibility without a user/admin needing to intervene and update a CRD.

## References

None
