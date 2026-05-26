# OKEP-6181: Allow UDN/CUDN to Generate Multiple NADs

* Issue: [#6181](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6181)

## Problem Statement

Today, a UDN generates one NAD in its namespace, and a CUDN generates one NAD in each selected
namespace. This makes it difficult to support accelerated (VF/VFIO) and unaccelerated (veth)
workloads on the same logical network, or workloads that use different SR-IOV resource pools,
without creating separate logical networks.

## Goals

* Allow primary and secondary UDN/CUDN to declare multiple named attachments, each generating a
  distinct NAD for the same logical network.
* Preserve existing Multus/SR-IOV semantics by continuing to use the
  `k8s.v1.cni.cncf.io/resourceName` annotation at the NAD level.
* Support accelerated and unaccelerated workloads, or workloads that use different SR-IOV resource
  pools, on the same UDN/CUDN by choosing different NADs.

## Non-Goals

* Changing SR-IOV device allocation behavior in Multus/SR-IOV plugins.
* Implementing or owning workload resource mutation logic. Existing mutating webhooks that add
  `resources.requests` for SR-IOV pools are outside OVN-Kubernetes, but they may need updates to
  understand primary network NAD selection described by this OKEP.
* Redefining CNI semantics for resource requests.
* Changing cluster default network (CDN) behavior or introducing CDN-specific default-network
  selection semantics.


## Introduction

Today, each UDN/CUDN generates only one NAD per namespace. When a workload selects a NAD, Multus
uses that NAD to determine the SR-IOV resource pool for the attachment: if the NAD has the
`k8s.v1.cni.cncf.io/resourceName` annotation, the annotation identifies the resource pool used for
the corresponding CNI attachment.

However, for a single logical network, different workloads often need different network attachments:

* Unaccelerated/default attachment (no SR-IOV resource).
* Pod-accelerated attachment (typically VF resource pool).
* VM-accelerated attachment (typically VFIO resource pool).

Without multi-NAD generation from one UDN/CUDN, users cannot use one logical network with different
resource pools. That increases operational complexity and obscures intent.

This proposal introduces an `attachments` API in UDN/CUDN to generate multiple NADs for one
logical network while preserving current behavior for existing UDN/CUDN objects.

## User-Stories/Use-Cases

Story 1: Secondary network with mixed acceleration

As a cluster admin, I want a secondary UDN to generate a default NAD, a VF NAD, and a VFIO NAD so
that Pod and VM workloads can share the same logical network while selecting the right acceleration
mode.

Story 2: Primary network with optional acceleration path

As a cluster admin, I want a primary CUDN to expose both default and accelerated generated NADs
so that tenants can opt into acceleration by selecting the right NAD for the workload.

Story 3: Predictable network selection

As an application operator, I want generated NAD names to be deterministic and discoverable from
UDN/CUDN spec so that network annotation authoring is straightforward and stable.

## Proposed Solution

### attachments definition in UDN/CUDN
Add an `attachments` list to UDN/CUDN. Each attachment entry defines the full name of a generated
NAD and an optional SR-IOV resource mapping. OVN-Kubernetes generates one NAD per attachment name,
and all generated NADs use the same logical network topology/subnet settings from the parent
UDN/CUDN.

A user or workload selects the generated NAD it needs, as described in the next section.
Multus/SR-IOV behavior remains unchanged: the `k8s.v1.cni.cncf.io/resourceName` annotation on the
selected NAD drives resource allocation.
OVN-Kubernetes CNI continues to consume the resulting CNI ADD request as it does today.

For primary networks, selecting an accelerated generated NAD through
`v1.multus-cni.io/default-network` can affect which SR-IOV resource pool the workload must request.
OVN-Kubernetes does not mutate Pod's resource requests. Any mutating webhook that is
responsible for adding SR-IOV `resources.requests` must be updated outside OVN-Kubernetes to inspect
the same `v1.multus-cni.io/default-network` annotation, including the `primary-network` field, and
add the matching resource request when the selected generated NAD carries
`k8s.v1.cni.cncf.io/resourceName`.


### Pod network selection

OVN-Kubernetes currently supports configuring pods' secondary network interfaces through the
`k8s.v1.cni.cncf.io/networks` annotation, which contains a JSON array of NetworkSelectionElement
objects.

For pods' primary network, this proposal follows the model used by OKEP-5233. The
`v1.multus-cni.io/default-network` annotation continues to reference the default-network NAD,
and may include a new optional `primary-network` field. Its value is the full name of a
generated NAD from the primary UDN/CUDN.

OVN-Kubernetes uses `primary-network` to choose the generated NAD for the pod's primary
network attachment. If the field is omitted, OVN-Kubernetes uses the generated base NAD.
The namespace still has only one primary logical network; the generated NADs provide multiple
network attachments for that same logical network.

Example primary UDN/CUDN pod selection:

```yaml
metadata:
  annotations:
    v1.multus-cni.io/default-network: |
      [
        {
          "name":"default",
          "namespace":"ovn-kubernetes",
          "primary-network":"l3-primary-pod-accelerated"
        }
      ]
```

### API Details

#### CUDN/UDN API Changes

The `attachments` field is added at these paths:

* `UserDefinedNetwork.spec.attachments`
* `ClusterUserDefinedNetwork.spec.network.attachments`

A sample CUDN API using `attachments` is shown below:
```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l3-primary
spec:
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: udn-test
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.20.0.0/16
        hostSubnet: 24
    transport: Geneve
    attachments:
    - name: l3-primary
    - name: l3-primary-pod-accelerated
      resourceName: nvidia.com/asap2_vf
    - name: l3-primary-vm-accelerated
      resourceName: nvidia.com/asap2_vfio
```

#### Field definitions

* `attachments` (optional list)
  * If omitted, behavior is identical to today: OVN-Kubernetes creates the generated base NAD only.
  * If present, each item defines one generated network attachment.
  * Annotations that are copied from the UDN/CUDN to the generated base NAD today are copied to all
    generated NADs.
* `attachments[].name` (required, string)
  * Unique within the UDN/CUDN.
  * Full name of the generated NAD.
  * If equal to the UDN/CUDN name, the attachment configures the generated base NAD.
* `attachments[].resourceName` (optional, string)
  * When set, copied to generated NAD annotation `k8s.v1.cni.cncf.io/resourceName`.

#### Generated NAD naming

Deterministic naming is required:

* UDN always generates a generated base NAD named after the UDN in the UDN namespace.
* CUDN always generates a generated base NAD named after the CUDN in each target namespace.
* Each `attachments[].name` is the full generated NAD name.
* If `attachments[].name` equals the UDN/CUDN name, no additional NAD is generated for that entry.
  Its settings are applied to the generated base NAD.
* If `attachments[].name` differs from the UDN/CUDN name, it must start with the UDN/CUDN name.
  OVN-Kubernetes generates an additional NAD with that exact name in the relevant namespace.

#### Validation

* `attachments[].name` must be DNS-1123 label compatible.
* `attachments[].name` must be unique.
* `attachments[].name` must either equal the UDN/CUDN name, or start with the UDN/CUDN name and be
  distinct from the UDN/CUDN name.
* `resourceName` should follow existing resource naming rules used by the Multus/SR-IOV ecosystem.

### Implementation Details

#### Controller changes

UDN/CUDN NAD generation logic is extended to:

1. Parse `attachments` if present.
2. Always generate the base NAD.
3. Generate one additional NAD per attachment whose `name` differs from the UDN/CUDN name.
4. Treat all generated NADs as network attachments for the same logical network.
5. Materialize the `k8s.v1.cni.cncf.io/resourceName` annotation based on `attachments[].resourceName`.
6. Maintain owner references and reconciliation behavior for all generated NADs.

If `attachments` is not present, the current single-NAD generation path remains unchanged.

The NAD/network manager also needs to understand that multiple generated NADs can represent the
same primary logical network in a namespace. Internal caches and helper functions that currently
track a single primary NAD per namespace must be updated to:

1. Allow multiple primary NADs in the same namespace when their parsed network configs are
   compatible and point to the same logical network.
2. Keep the generated base NAD for default behavior when no explicit attachment is selected.
3. Track all generated NAD keys associated with the primary network so pod selection and
   reconciliation can validate that a selected attachment belongs to the namespace's primary
   UDN/CUDN.

Because the namespace still has one primary logical network, other controllers that request the
namespace's primary network can keep using the generated base NAD.

Attachment reconciliation follows declarative ownership semantics:

1. On create/update, the controller creates or updates the generated NAD set to match `attachments`.
2. If an attachment is removed from `attachments`, the corresponding generated NAD is deleted.
3. There is no special rename operation. A rename is represented as deleting the old attachment name
   and creating a new attachment name.
4. Updating `resourceName` updates only the generated NAD annotation. The logical network config remains
   unchanged because all generated NADs point to the same UDN/CUDN network.
5. Existing pods keep using the NAD selected at creation time. Workload rollout/recreation is required
   to consume a different network attachment.

#### Pod/VM network selection behavior

Secondary network:

* User specifies generated NAD name in `k8s.v1.cni.cncf.io/networks`.
* Example:

```yaml
metadata:
  annotations:
    k8s.v1.cni.cncf.io/networks: default/l3-primary-pod-accelerated
```

Primary network:

* The existing default primary path remains supported.
* A namespace still has only one primary logical network.
* Generated primary NADs are network attachments for that same primary logical network.
* `v1.multus-cni.io/default-network` continues to reference the operator-chosen default-network NAD.
* The optional `primary-network` field in the annotation JSON selects the full name of the generated
  primary NAD to use as the network attachment.
* If `primary-network` is omitted, OVN-Kubernetes uses the generated base NAD.
* If `primary-network` references a generated NAD that does not belong to the pod's primary UDN/CUDN,
  pod setup fails validation.

#### Datapath and mode impact

* No intended datapath change.

### Testing Details

* Unit testing details:
  * Reconciler tests for create/update/delete of multiple generated NADs.
  * Naming and collision handling tests.
  * Primary network selection tests for `primary-network` omitted, valid, and invalid values.
* E2E testing details:
  * Secondary UDN with default + VF + VFIO attachments; verify Pod/VM connectivity and expected
    resource behavior.
  * Primary network selection scenarios with and without explicit `primary-network` selection.
  * Attachment deletion and `resourceName` update scenarios.
* API testing details:
  * API validation tests for `attachments`.
  * Conflict/rejection tests for invalid attachment names and duplicates.

### Documentation Details

* Add a user-facing guide for `attachments`:
  * How to declare default and accelerated network attachments.
  * How to select generated NADs from Pod and VM manifests.
  * Primary network selection examples using `v1.multus-cni.io/default-network` with `primary-network`.
* Update OKEP index in `mkdocs.yml` with this document path.

## Risks, Known Limitations and Mitigations

* Risk: Primary network selection confusion because `v1.multus-cni.io/default-network` references one
  NAD while `primary-network` selects another generated NAD.
  * Mitigation: document the annotation usage clearly and validate that `primary-network` belongs
    to the pod's primary UDN/CUDN.
* Risk: Existing SR-IOV resource mutating webhooks may only inspect `k8s.v1.cni.cncf.io/networks`.
  * Mitigation: document that those webhooks are not part of OVN-Kubernetes and must be updated to
    inspect `v1.multus-cni.io/default-network` and its `primary-network` field for accelerated
    primary network attachments.
* Limitation: Users still need to choose the correct attachment for each workload type.
  * Mitigation: keep semantics explicit now; future automation or mutating webhooks remain possible
    but out of scope.

## OVN-Kubernetes Version Skew

Target release: TBD (to be aligned with maintainers and release planning).

Version skew considerations:

* New `attachments` field requires a controller version that understands it.
* Existing objects without `attachments` continue to function.

## Backwards Compatibility

* Backward compatible by default:
  * Existing UDN/CUDN objects continue with current single-NAD behavior when `attachments`
    is absent.
* API additive change:
  * Adds a new optional field; no removal of existing fields in this proposal.
* Existing workload manifests selecting current NAD names remain valid unless operators opt into
  `attachments`.

## Alternatives

* Keep single NAD per UDN/CUDN and encode more logic elsewhere.
  * Rejected because it does not natively model multiple resource pools on one logical network.

