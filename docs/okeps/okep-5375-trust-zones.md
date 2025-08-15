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

Allow cluster admins to dedicate chassis to particular trust zones, only building
Geneve tunnels between directly connected zones.

Make sure that a rogue chassis cannot force other chassis to create unintended
tunnels to the rogue chassis.

## Non-Goals

Automatically determining zone assignment for chassis based on workload
attributes (e.g. UDN attachments).

Implementation of the feature for Central or multi-node IC modes. (See detailed
rationale below in the `Risks, Known Limitations and Mitigations` section.)

## Introduction

Inter-chassis tunnels carry some cost, in terms of security, performance, and
scale. Even when a tunnel is not used, chassis attempt to create corresponding
Interfaces.

This is unnecessary and may even expose victim workloads to an attacker if they
manage to hijack a chassis, since the attacker then has direct tunnels to all
chassis.

This OKEP exposes trust zone functionality through a new `TrustZone` CRD. When
created, a `TrustZone` CR will affect which chassis have tunnels between them,
and which don't.

Only cluster admin should be able to create `TrustZone` CRs.

## User-Stories/Use-Cases

### Story 1: Isolate a set of chassis in a trust zone

As a cluster admin, I want to assign a set of chassis to a trust zone, so that
only a chassis that belongs to the trust zone creates tunnel endpoints to other
chassis from the zone.

### Story 2: Group a set of chassis in multiple trust zones

As a cluster admin, I want to apply unique trust zones to disjoint subsets of
chassis, so that each subset can tunnel traffic internally but not between
zones.

## Proposed Solution

### At a Glance

OVN-Kubernetes will maintain the list of remote `Chassis` in SBDB so that only
chassis that share a trust zone with the local chassis will have a
corresponding `Chassis` record in SBDB. Since there won't be `Chassis` records
for chassis from disjoint trust zones, no tunnels will be created.

To guarantee that a rogue chassis won't be able to change trust zone membership
for itself or other chassis, changing `TrustZone` CRs will not be denied using
RBAC unless the user is a cluster admin.

### API Details

#### TrustZone CRD

A new `TrustZone` CRD will be created in `k8s.ovn.org/v1alpha1` API group as
follows:

```
apiVersion: k8s.ovn.org/v1alpha1
kind: TrustZone
metadata:
  name: trust-zone-a
spec:
  nodeSelector:
    matchLabels:
      tenant: tenant-a
status:
  members:
    - ovnkube-pod-1
    - ovnkube-pod-2
    - ovnkube-pod-3
  conditions:
    - type: Ready
      status: "True"
      reason: AllTunnelsEstablished
      lastTransitionTime: "2025-08-13T15:04:05Z"
```

In the above example, the `TrustZone` named `trust-zone-a` selects nodes that
are assigned to a tenant using a `tenant` label. The `status.members` field
lists the names of `ovnkube-node` pods that belong to the trust zone. The
`status.conditions` field indicates whether all tunnels between members of the
trust zone have been successfully established.

```
apiVersion: k8s.ovn.org/v1alpha1
kind: TrustZone
metadata:
  name: trust-zone-a
spec:
  nodeSelector:
    matchExpressions:
      - key: k8s.ovn.org/zone-name
        operator: In
        values:
        - tenant-a-node1
        - tenant-a-node2
        - tenant-a-node3
status:
  members:
    - ovnkube-pod-1
    - ovnkube-pod-2
    - ovnkube-pod-3
  conditions:
    - type: Ready
      status: "True"
      reason: AllTunnelsEstablished
      lastTransitionTime: "2025-08-13T15:04:05Z"
```

In the above example, the `TrustZone` named `trust-zone-a` selects nodes that
match one of `zone-name` labels. The `status.members` field lists the names of
`ovnkube-node` pods that belong to the trust zone. The `status.conditions`
field indicates whether all tunnels between members of the trust zone have been
successfully established.

In the future, the `TrustZone` CRD may be extended to support additional
selector types, e.g. to match against `ovnkube-node` pod labels. (This may be
useful when running multiple `ovnkube-node` pods that belong to the same node
but different trust zones.)

### Implementation Details

A new controller will be added to `ovnkube-node` to watch for `TrustZone` CRs.
All `ovnkube-node` pods will watch for all `TrustZone` CRs in the cluster.

#### DB configuration

On `TrustZone` CR created, `ovnkube-node` will create `Chassis` records
for remote chassis that have at least one common trust zone set, and remove
`Chassis` records for remote chassis that do not have any common trust zones
set.

On `TrustZone` CR deleted, `ovnkube-node` will create `Chassis` records for
remote chassis that also have no trust zones set, and remove `Chassis` records
for remote chassis that have any trust zones set.

In addition to the `Chassis` records, `ovnkube-node` will also insert or
delete any other records that pertain to remote chassis, namely:

* Logical Switch Port in the transit switch.
* Logical Router Port in the distributed cluster router.
* Static Route in the distributed cluster router.

This will be achieved by extending `UpdateResource` handler for `NodeType` to
call the `deleteNodeEvent` code path as needed.

#### Admission enforcement

Only cluster admin should be able to create or delete `TrustZone` CRs. This
will be enforced using RBAC.

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

A new page in `api-reference` guide should be added to describe the new
`TrustZone` CRD.

A new page in `design` guide should be added to briefly explain the intent and
design of the trust zone feature from the perspective of an operator.

## Risks, Known Limitations and Mitigations

The feature is not implemented for multi-node IC or centralized modes that use
shared SBDB.

## OVN Kubernetes Version Skew

Proposed for 1.2.

## Alternatives

### OVN Transport Zones

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

Trust zones could have been implemented using OVN Transport Zones too. In
general, it's a viable approach, but it has some drawbacks:

* `ovnkube-node` would need to access local OVSDB to update
  `external-ids:ovn-transport-zones` setting, which is not currently the
  case. This would require additional code to pass a local OVSDB client into
  zone IC handler. Only interacting with SBDB simplifies the implementation a
  bit.

* The proposed approach allows to avoid creating remote `Chassis` and its
  supporting resources (LSP, LRP, Static Routes) for chassis that are disjoint
  from the local chassis. This should help with scalability, as the number of
  redundant records in SBDB is reduced.

Utilizing OVN Transport Zones directly may be revisited in the future if these
allow for even more granular segmentation than what we can achieve natively in
OVN-K.

### Using hypervisor firewall for isolation

Instead of using trust zones to determine connectivity between chassis, one
could also leave OVN-Kubernetes as-is while relying on a hypervisor firewall to
block any traffic between chassis that are not supposed to communicate with
each other. This approach may have its place in some configurations, but it may
have a performance impact in case of a large number of isolated zones which
results in the large number of firewall rules.

The firewall approach is not mutually exclusive with this OKEP and may be
applied on top of it for security in depth, if needed.

## References

* [OVN Transport Zones patch](https://mail.openvswitch.org/pipermail/ovs-dev/2019-April/358314.html)
* [Blog about OVN transport zones](https://lucasgom.es/posts/ovn_transport_zones.html)
