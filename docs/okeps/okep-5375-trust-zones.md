# OKEP-5375: Trust Zones for Interconnect mode

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

Allow cluster admins to dedicate Nodes to particular trust zones, making sure
that no traffic will be tunneled between disjoint zones.

Make sure that a rogue node cannot force other nodes to create unintended
tunnels to the rogue node.

## Non-Goals

Automatically determining zone assignment for Nodes based on workload
attributes (e.g. UDN attachments).

Implementation of the feature for Central or multi-node IC modes. (See detailed
rationale below in the `Risks, Known Limitations and Mitigations` section.)

## Introduction

Inter-chassis tunnels carry some cost, in terms of security, performance, and
stability. Even when a tunnel is not used, chassis attempt to create
corresponding Interfaces.

This is unnecessary and may even expose victim workloads to an attacker if they
manage to hijack a node since the attacker then has direct tunnel to all other
nodes.

This OKEP proposes to expose trust zone functionality through a `Node`
annotation. When set, the annotation will control which Nodes should have
tunnels between them, and which should not.

Only cluster admin should be able to set this annotation.

## User-Stories/Use-Cases

### Story 1: Deny tunnel traffic between some nodes

As a cluster admin, I want to apply an annotation to some Nodes that will
distinguish them into a separate trust zone, so that only chassis that belong
to the same zone will create tunnel endpoints between them.

### Story 2: Group nodes in multiple trust zones

As a cluster admin, I want to apply unique trust zones to disjoint subsets of
Nodes, so that each subset can host workloads that can communicate inside the
same subset but cannot communicate between disjoint subsets. (It's up to me as
an admin to make sure trust zone assignment reflects respective node selectors
used.)

### Story 3: Dedicate nodes to gateway appliances (pods)

As a cluster admin, I want to isolate most nodes into disjoint trust zones
while also dedicating a subset of shared "gateway" nodes that would be present
in multiple (maybe all) trust zones. These dedicated nodes will run gateway
appliances (pods) to handle external as well as internal traffic for disjoint
trust zones.

To make it work, some routing mechanism between isolated trust zones and their
respective gateway appliance pods is needed. (One option may be
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

Instead of using OVN Transport Zones directly, OVN-Kubernetes will maintain the
list of remote `Chassis` in SBDB so that only nodes that share a trust zone
with the local node will have a corresponding `Chassis` record in SBDB. Since
there won't be `Chassis` records for nodes from disjoint trust zones, no
tunnels will be created.

To guarantee that a rogue node won't be able to change its `Node` annotation to
expose the `Node` to illegal trust zones, an admission hook should be installed
to deny such updates.

### API Details

A new `Node` object annotation will be added:

```bash
kubectl annotate nodes ovn-central k8s.ovn.org/trust-zones='["tz1","tz2","tz3"]'
kubectl annotate nodes ovn-worker1 k8s.ovn.org/trust-zones='["tz1"]'
kubectl annotate nodes ovn-worker2 k8s.ovn.org/trust-zones='["tz2"]'
kubectl annotate nodes ovn-worker3 k8s.ovn.org/trust-zones='["tz3"]'
```

It will be ignored for Centralized mode.

### Implementation Details

#### DB configuration

On node trust zone annotation update, `ovnkube-controller` will:

1. If the annotation is not set, create `Chassis` records for remote nodes that
   also have no trust zones set, and remove `Chassis` records for remote nodes
   that have any trust zones set.

2. If the annotation is set, create `Chassis` records for remote nodes that
   have at least one common trust zone set, and remove `Chassis` records for
   remote nodes that do not have any common trust zones set.

Since we are not going to use OVN Transport Zones feature directly,
`ovnkube-controller` `chassis_handler.go` will **not** need access to local
OVSDB client to update trust zone assignment for the local node.

Attention should be given to transition of chassis between remote and local
states.

In addition to the `Chassis` records, `ovnkube-controller` will also insert or
delete any other records that pertain to remote chassis, namely:

* Logical Switch Port in the transit switch.
* Logical Router Port in the distributed cluster router.
* Static Route in the distributed cluster router.

This will be achieved by extending `UpdateResource` handler for `NodeType` to
call the `deleteNodeEvent` code path as needed.

#### Admission enforcement

The `nodeadmission.go` hook already enforces that a `ovnkube-controller` can
only update a particular explicit list of annotations, so no changes are needed
to guard against a rogue `Node` updating its own trust zones.

```console
[root@ovn-worker ~]# kubectl annotate nodes ovn-worker k8s.ovn.org/trust-zones='["tz1"]'
Error from server (Forbidden): nodes "ovn-worker" is forbidden: User "system:serviceaccount:ovn-kubernetes:ovn-node" cannot patch resource "nodes" in API group "" at the cluster scope
```

Note: since the rogue `Node`, by definition, has access to its own SBDB in
single-node IC mode, it can always affect its list of `Chassis` records. But
it's not enough to mislead other intact Nodes that use separate databases,
which is required to create tunnels between rogue and intact nodes.

### Testing Details

Unit tests should be updated to confirm that `UpdateResource` handler for
`NodeType` inserts and deletes remote `Chassis` and related records as per the
rules described above.

E2E test scenario should confirm inter-zone connectivity guarantees are
enforced. Pods should successfully communicate if they run on different nodes
that are not part of any trust zones, or if they are part of the same trust
zones. Pods should fail to communicate if they run on nodes that belong to
different trust zones.

### Documentation Details

A new page in `api-reference` guide should be added to list supported `Node`
annotations. (Only the newly proposed annotation has to be described there.)

A new page in `design` guide should be added to briefly explain the intent and
design of the trust zone feature from the perspective of an operator.

## Risks, Known Limitations and Mitigations

The feature is not implemented for multi-node IC or centralized modes that use
shared SBDB.

## OVN Kubernetes Version Skew

Proposed for 1.2.

## Alternatives

### OVN Transport Zones

As discussed above, trust zones could have been implemented using OVN Transport
Zones too. In general, it's a viable approach, but it has some drawbacks:

* `kubeovn-controller` would need to access local OVSDB to update
  `external-ids:ovn-transport-zones` setting, which is not currently the
  case. This would require additional code to pass a local OVSDB client into
  zone IC handler. Only interacting with SBDB simplifies the implementation a
  bit.
* The proposed approach allows to avoid creating remote `Chassis` and its
  supporting resources (LSP, LRP, Static Routes) for nodes that are disjoint
  from the local node. This should help with scalability, as the number of
  redundant records in SBDB is reduced.

### Using hypervisor firewall for isolation

Instead of using trust zones to determine connectivity between nodes, one could
also leave OVN-Kubernetes as-is while relying on a hypervisor firewall to block
any traffic between nodes that are not supposed to communicate with each other.
This approach may have its place in some configurations, but it may have a
performance impact in case of a large number of isolated zones which results in
the large number of firewall rules.

The firewall approach is not mutually exclusive with this OKEP and may be
applied on top of it for security in depth, if needed.

### Deriving trust zones from logical topology

Trust zone assignment could probably have been determined through complex
calculation of logical topology adjacency, e.g. based on UDN attachments. Even
if technically possible, it would complicate the logic and may not have covered
all possible administrative policy needs that cluster admins may want to impose
on inter-node communication.

## References

* [OVN Transport Zones patch](https://mail.openvswitch.org/pipermail/ovs-dev/2019-April/358314.html)
* [Blog about OVN transport zones](https://lucasgom.es/posts/ovn_transport_zones.html)
