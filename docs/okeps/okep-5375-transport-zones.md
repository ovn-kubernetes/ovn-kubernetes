# OKEP-5375: Transport Zones for Interconnect mode

* Issue: [#5375](https://github.com/ovn-org/ovn-kubernetes/issues/5375)

## Problem Statement

By default, OVN controller builds tunnels between all chassis, even if these
tunnels are, by administrative policy or configuration, not to be used. These
tunnels are unnecessary in some scenarios (Edge computing, or when particular
nodes are dedicated to run workloads that belong to a specific isolated
tenant).

Not just redundant, they may potentially expose a rogue node to other victim
nodes that perhaps never meant to communicate with it.

Security is the primary driver for this feature, while scalability
considerations are a secondary benefit.

## Goals

Allow cluster admins to dedicate Nodes to particular transport zones,
making sure that no traffic will be tunneled between disjoint zones.

Make sure that a rogue node cannot force other nodes to create unintended
tunnels to the rogue node.

## Non-Goals

Automatically determining zone assignment for Nodes based on workload
attributes (e.g. UDN attachments).

Implementation of the feature for Central mode. (See detailed rationale below
in the `Risks, Known Limitations and Mitigations` section.)

## Introduction

Inter-chassis tunnels carry some cost, in terms of security, performance, and
stability. Even when a tunnel is not used, chassis attempt to create
corresponding Interfaces.

This is unnecessary and may even expose victim workloads to an attacker if they
manage to hijack a node since the attacker then has direct tunnel to all other
nodes.

This OKEP proposes to expose transport zone functionality through a `Node`
annotation. When set, the annotation will control which Nodes should have
tunnels between them, and which should not.

Only cluster admin should be able to set this annotation.

## User-Stories/Use-Cases

### Story 1: Deny tunnel traffic between some nodes

As a cluster admin, I want to apply an annotation to some Nodes that will
distinguish them into a separate transport zone, so that only chassis that
belong to the same zone will create tunnel endpoints between them.

### Story 2: Group nodes in multiple transport zones

As a cluster admin, I want to apply unique transport zones to disjoint subsets
of Nodes, so that each subset can host workloads that can communicate inside
the same subset but cannot communicate between disjoint subsets. (It's up to me
as an admin to make sure transport zone assignment reflects respective node
selectors used.)

### Story 3: Dedicate nodes to gateway appliances (pods)

As a cluster admin, I want to isolate most nodes into disjoint transport zones
while also dedicating a subset of shared "gateway" nodes that would be present
in multiple (maybe all) transport zones. These dedicated nodes will run gateway
appliances (pods) to handle external as well as internal traffic for disjoint
transport zones.

To make it work, some routing mechanism between isolated transport zones and
their respective gateway appliance pods is needed. (One option may be
`AdminPolicyBasedExternalRoutes`. There may be alternatives using BGP and other
routing protocols.)

**Defining such redirection mechanism is out of scope for this OKEP.**

## Proposed Solution

### Core OVN

OVN [has](https://mail.openvswitch.org/pipermail/ovs-dev/2019-April/358314.html)
support for Transport Zones. Transport zones are configured in local OVSDB by
setting:

```bash
ovs-vsctl set open . external-ids:ovn-transport-zones=tz1
```

Then OVN controller configures its `Chassis:transport_zones` field in SBDB
accordingly.

When `transport_zones` are set, the chassis will only create tunnel Interfaces
for other chassis that also belong to at least one of the listed zones.

When `transport_zones` are not set, tunnel Interfaces are created for all other
chassis. But since peer chassis with `transport_zones` set will not set up
tunnels to these implicitly marked nodes, no tunnels between marked and
unmarked chassis will be created either.

### OVN-Kubernetes

* For local chassis, the `ovnkube-controller` container should set the local
  OVSDB attribute and rely on `ovn-controller` to translate it into SBDB.

* For remote chassis, `Chassis:transport_zones` will be set by
  `ovnkube-controller` itself.

To guarantee that a rogue node won't be able to change its `Node` annotation to
expose the `Node` to illegal transport zones, an admission hook should be
installed to deny such updates.

### API Details

A new `Node` object annotation will be added:

```bash
kubectl annotate nodes ovn-central k8s.ovn.org/transport-zones='["tz1","tz2","tz3"]'
kubectl annotate nodes ovn-worker1 k8s.ovn.org/transport-zones='["tz1"]'
kubectl annotate nodes ovn-worker2 k8s.ovn.org/transport-zones='["tz2"]'
kubectl annotate nodes ovn-worker3 k8s.ovn.org/transport-zones='["tz3"]'
```

It will be ignored for Centralized mode.

### Implementation Details

#### DB configuration

The `ovnkube-controller` `chassis_handler.go` will need access to local OVSDB
client to update transport zone assignment for the local node.

Attention should be given to transition of chassis between remote and local
states.

#### Admission enforcement

The `nodeadmission.go` hook already enforces that a `ovnkube-controller` can
only update a particular explicit list of annotations, so no changes are needed
to guard against a rogue `Node` updating its own transport zones.

```console
[root@ovn-worker ~]# kubectl annotate nodes ovn-worker k8s.ovn.org/transport-zones='["tz1"]'
Error from server (Forbidden): nodes "ovn-worker" is forbidden: User "system:serviceaccount:ovn-kubernetes:ovn-node" cannot patch resource "nodes" in API group "" at the cluster scope
```

Note: since the rogue `Node`, by definition, has access to its own SBDB, it can
always affect its own view of zone membership of all chassis. But it's not
enough to mislead other intact Nodes, which is required to create tunnels
between rogue and intact nodes.

### Testing Details

Unit tests should be updated to confirm `chassis_handler.go` correctly
configures transport chassis for remote and local scenarios, as well as for
transitions between the two.

E2E test scenario should confirm inter-zone connectivity guarantees are
enforced: pods should successfully communicate if they run on different nodes
that are not part of any transport zones, or if they are part of the same
transport zones; pods should fail to communicate if they run on nodes that
belong to different transport zones.

### Documentation Details

A new page in `api-reference` guide should be added to list supported `Node`
annotations. (Only the newly proposed annotation has to be described there.)

A new page in `design` guide should be added to briefly explain the intent and
design of the transport zone feature from the perspective of an operator.

## Risks, Known Limitations and Mitigations

In IC mode, a rogue node can affect its own view of Chassis transport zone
membership, which is not enough to mislead other nodes in different IC zones.

If an IC zone contains more than one node, then any rogue node from a
multi-node IC zone will be able to affect transport zone membership view of
other nodes in the same IC zone. It is possible because each `ovn-controller`
owns its own `Chassis` record,
[including](https://github.com/ovn-org/ovn/blob/93fc05d02607596ed5811684715e1e77bd46bfc6/northd/ovn-northd.c#L78)
its `transport_zones` field. This means all nodes in the same IC zone share
security destiny in terms of the discussed attack scenario.

In Centralized mode, all nodes are effectively part of the same IC zone since
they share the same SBDB. This means any single rogue node is able to force
creation of illegal tunnel Interfaces for all other nodes in the cluster by
updating its own `Chassis` record. This fact renders the feature useless for
Centralized mode. For this reason, the feature is not implemented for this
mode.

**We are not aware of a mitigation to this attack scenario for Centralized mode
or multi-node IC zones. For this reason, it is recommended to have only one
node in each IC zone.**

## OVN Kubernetes Version Skew

Nothing to add here.

## Alternatives

Transport zone assignment could probably have been determined through complex
calculation of logical topology adjacency, e.g. based on UDN attachments. Even
if technically possible, it would complicate the logic and may not have covered
all possible administrative policy needs that cluster admins may want to impose
on inter-node communication.

Instead of using transport zones to determine connectivity between nodes, one
could also leave OVN-Kubernetes as-is while relying on a hypervisor firewall to
block any traffic between nodes that are not supposed to communicate with each
other. This approach may have its place in some configurations, but it may have
a performance impact in case of a large number of isolated zones which results
in the large number of firewall rules.

That said, the firewall approach is not mutually exclusive with this OKEP and
may be applied on top of it for security in depth, if needed.

## References

* [OVN patch](https://mail.openvswitch.org/pipermail/ovs-dev/2019-April/358314.html)
* [Blog about the feature](https://lucasgom.es/posts/ovn_transport_zones.html)
