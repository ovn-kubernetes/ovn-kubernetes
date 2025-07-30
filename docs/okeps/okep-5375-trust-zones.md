# OKEP-5375: Trust Zones for Interconnect mode

* Issue: [#5375](https://github.com/ovn-org/ovn-kubernetes/issues/5375)

## Problem Statement

By default, OVN controller builds tunnels between all chassis, even if these
tunnels are, by administrative policy or configuration, not to be used. These
tunnels are unnecessary in some scenarios (Edge computing, or when particular
chassis are dedicated to run workloads that belong to a specific isolated
tenant).

Not just redundant, they may potentially expose a rogue chassis to other victim
chassis that perhaps never meant to communicate with it.

Security is the primary driver for this feature, while scalability
considerations are a secondary benefit.

## Goals

Allow cluster admins to dedicate chassis to particular trust zones, making sure
that no traffic will be tunneled between disjoint zones.

Make sure that a rogue chassis cannot force other chassis to create unintended
tunnels to the rogue chassis.

## Non-Goals

Automatically determining zone assignment for chassis based on workload
attributes (e.g. UDN attachments).

Implementation of the feature for Central or multi-node IC modes. (See detailed
rationale below in the `Risks, Known Limitations and Mitigations` section.)

## Introduction

Inter-chassis tunnels carry some cost, in terms of security, performance, and
stability. Even when a tunnel is not used, chassis attempt to create
corresponding Interfaces.

This is unnecessary and may even expose victim workloads to an attacker if they
manage to hijack a chassis, since the attacker then has direct tunnel to all
other chassis.

This OKEP proposes to expose trust zone functionality through a `Node`
annotation. When set, the annotation will control which chassis should have
tunnels between them, and which should not.

Only cluster admin should be able to set this annotation.

## User-Stories/Use-Cases

### Story 1: Deny tunnel traffic between some chassis

As a cluster admin, I want to apply an annotation to some chassis that will
distinguish them into a separate trust zone, so that only chassis that belong
to the same zone will create tunnel endpoints between them.

### Story 2: Group chassis in multiple trust zones

As a cluster admin, I want to apply unique trust zones to disjoint subsets of
chassis, so that each subset can host workloads that can communicate inside the
same subset but cannot communicate between disjoint subsets. (It's up to me as
an admin to make sure trust zone assignment reflects respective node selectors
used.)

### Story 3: Set trust zones for DPU chassis

As a cluster admin, I want to set trust zones for on-DPU OVN chassis that may
be different from trust zones that the on-Host OVN chassis is part of. By doing
so, I am able to isolate traffic handled by Host `ovnkube-controller` from
traffic handled by DPU `ovnkube-controller` on VTEP level.

### Story 4: Dedicate chassis to gateway appliances (pods)

As a cluster admin, I want to isolate most chassis into disjoint trust zones
while also dedicating a subset of shared "gateway" chassis that would be
present in multiple (maybe all) trust zones. These dedicated chassis will run
gateway appliances (pods) to handle external as well as internal traffic for
disjoint trust zones.

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
tunnels to these implicitly marked chassis, no tunnels between marked and
unmarked chassis will be created either.

### OVN-Kubernetes

Instead of using OVN Transport Zones directly, OVN-Kubernetes will maintain the
list of remote `Chassis` in SBDB so that only chassis that share a trust zone
with the local chassis will have a corresponding `Chassis` record in SBDB.
Since there won't be `Chassis` records for chassis from disjoint trust zones,
no tunnels will be created.

To guarantee that a rogue chassis won't be able to change its `Node` annotation
to expose the chassis to illegal trust zones, an admission hook should be
installed to deny such updates. (Note: this is already the case.)

### API Details

#### DPUs

The proposed solution should work in the presence of a DPU chassis, with and
without a Host chassis. DPU chassis don't have a separate `Node` object, so
they may have to use its Host `Node` object to communicate about trust zones.

There may be more than one DPU chassis per Host. The proposed solution
should support specifying trust zones for DPU chassis that are different from
other chassis that belong to the same `Node` (whether on-DPU or Host).

(In a similar vein, the proposed API format should be flexible enough for
multiple virtual OVN chassis running on the same Host  or per-VTEP trust
zones.)

#### Node annotation

To serve all possible permutations of chassis (DPU vs Host; numbers of each
type; multiple VTEPs), a new `Node` object annotation of map type will be
added.

Each key in the map will represent a particular endpoint entity. In the scope
of this OKEP, "endpoint entity" refers to an OVN chassis, but its definition
may later be expanded to e.g. support per VTEP trust zones. For OVN chassis,
`Chassis` `system-id` will be used as the key.

Each value of the map will be a list of trust zones that the endpoint entity
(chassis) belongs to.

A default key `__default` will be used to represent the default trust zones for
endpoints that do not have a specific key set.

---

For example, if all OVN chassis on a `Node` are part of the same trust zones,
one can set the annotation like this:

```yaml
apiVersion: v1
kind: Node
metadata:
  annotations:
    k8s.ovn.org/trust-zones: '{"__default": ["tz1","tz2","tz3"]}'
```

This setting will serve scenarios with a single OVN chassis on the `Node`
(either on-DPU or on-Host); or with multiple OVN chassis that all belong to the
same trust zones.

---

If multiple OVN chassis are present on a `Node`, and they belong to different
trust zones, the following annotation can be used:

```yaml
apiVersion: v1
kind: Node
metadata:
  annotations:
    k8s.ovn.org/trust-zones: '{"<system-id1>": ["tz1"], "<system-id2>": ["tz2"]}'
```

or:

```yaml
apiVersion: v1
kind: Node
metadata:
  annotations:
    k8s.ovn.org/trust-zones: '{"__default": ["tz1"], "<system-id2>": ["tz2"]}'
```

(in the latter case, all chassis but `<system-id2>` will be part of `tz1`).

---

In the future, if per-VTEP trust zones are needed, the annotation can be
extended to support multiple VTEPs per chassis, e.g.:

```yaml
apiVersion: v1
kind: Node
metadata:
  annotations:
    k8s.ovn.org/trust-zones: '{"<system-id1>": {"vtep1": ["tz1"], "vtep2": ["tz2"]}}'
```

or:

```yaml
apiVersion: v1
kind: Node
metadata:
  annotations:
    k8s.ovn.org/trust-zones: '{"<system-id1:vtep1>": ["tz1"], "<system-id1:vtep2>": ["tz2"]}'
```

(Defining the exact key format for this scenario is beyond the scope of this
OKEP.)

---

Note: the `trust-zones` annotation will be ignored for Centralized mode.

### Implementation Details

#### DB configuration

On `Node` `trust-zones` annotation update, `ovnkube-controller` will:

1. If the annotation is not set, create `Chassis` records for remote chassis
   that also have no trust zones set, and remove `Chassis` records for remote
   chassis that have any trust zones set.

2. If the annotation is set, create `Chassis` records for remote chassis that
   have at least one common trust zone set, and remove `Chassis` records for
   remote chassis that do not have any common trust zones set.

Since we are not going to use OVN Transport Zones feature directly,
`ovnkube-controller` `chassis_handler.go` will **not** need access to local
OVSDB client to update trust zone assignment for the local chassis.

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
to guard against a rogue chassis updating its own trust zones.

```console
[root@ovn-worker ~]# kubectl annotate nodes ovn-worker k8s.ovn.org/trust-zones='{"__default": ["tz1"]}'
Error from server (Forbidden): nodes "ovn-worker" is forbidden: User "system:serviceaccount:ovn-kubernetes:ovn-node" cannot patch resource "nodes" in API group "" at the cluster scope
```

Note: since the rogue chassis, by definition, has access to its own SBDB in
single-node IC mode, it can always affect its list of `Chassis` records. But
it's not enough to mislead other intact chassis that use separate databases,
which is required to create tunnels between rogue and intact chassis.

### Testing Details

Unit tests should be updated to confirm that `UpdateResource` handler for
`NodeType` inserts and deletes remote `Chassis` and related records as per the
rules described above.

E2E test scenario should confirm inter-zone connectivity guarantees are
enforced. Pods should successfully communicate if they run on different chassis
that are not part of any trust zones, or if they are part of the same trust
zones. Pods should fail to communicate if they run on chassis that belong to
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

* `ovnkube-controller` would need to access local OVSDB to update
  `external-ids:ovn-transport-zones` setting, which is not currently the
  case. This would require additional code to pass a local OVSDB client into
  zone IC handler. Only interacting with SBDB simplifies the implementation a
  bit.
* The proposed approach allows to avoid creating remote `Chassis` and its
  supporting resources (LSP, LRP, Static Routes) for chassis that are disjoint
  from the local chassis. This should help with scalability, as the number of
  redundant records in SBDB is reduced.

Note: future developments like per-VTEP trust zones may require using OVN
Transport Zones, but this OKEP does not cover such use cases. If so, both the
approach proposed in this OKEP and OVN Transport Zones may be combined for
multi-layered isolation and control plane scaling benefits.

### Using hypervisor firewall for isolation

Instead of using trust zones to determine connectivity between chassis, one
could also leave OVN-Kubernetes as-is while relying on a hypervisor firewall to
block any traffic between chassis that are not supposed to communicate with
each other. This approach may have its place in some configurations, but it may
have a performance impact in case of a large number of isolated zones which
results in the large number of firewall rules.

The firewall approach is not mutually exclusive with this OKEP and may be
applied on top of it for security in depth, if needed.

### Deriving trust zones from logical topology

Trust zone assignment could probably have been determined through complex
calculation of logical topology adjacency, e.g. based on UDN attachments. Even
if technically possible, it would complicate the logic and may not have covered
all possible administrative policy needs that cluster admins may want to impose
on inter-chassis communication.

## References

* [OVN Transport Zones patch](https://mail.openvswitch.org/pipermail/ovs-dev/2019-April/358314.html)
* [Blog about OVN transport zones](https://lucasgom.es/posts/ovn_transport_zones.html)
