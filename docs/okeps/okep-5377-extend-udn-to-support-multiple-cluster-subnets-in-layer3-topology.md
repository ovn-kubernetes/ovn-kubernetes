# OKEP-5377: Extend UDN to Support Multiple Cluster Subnets in Layer3 Topology 

* Issue: [#5377](https://github.com/ovn-org/ovn-kubernetes/issues/5377)

## Problem Statement

UDN currently supports only one cluster subnet per IP family in Layer3 topology. This limits IP
address planning and scalability for large clusters.

## Goals

* Support multiple cluster subnets per IP family in UDN's Layer3 topology.
* Align UDN’s subnet capabilities with existing support in NAD.
* Allow dynamic subnet expansion by appending new subnets to an existing UDN.
* Support only subnets with the same host subnet size(`hostSubnet`).

## Non-Goals

* Supporting multiple subnets in Layer2 or Localnet topologies.
* Supporting deletion or shrinking of existing subnets.

## Future-Goals

* Support subnets with different host subnet sizes(`hostSubnet`)

## Introduction

When using UDN to define a network in Layer3 topology, each node gets a subnet allocated from the
UDN's `subnets` list per IP family. Currently, UDN limits `subnets` to a single subnet per IP
family, enforced by CRD validation and full immutability of the UDN spec. This limitation blocks
practical use cases where operators need to expand IP space incrementally — a feature already
available through NAD.

This proposal aims to bring UDN's flexibility in line with NAD by supporting multiple cluster
subnets and enabling controlled mutability.


## User-Stories/Use-Cases

### Story 1: Expanding subnet pool for growing clusters

**As a** cluster operator managing a UDN Layer3 network,  
**I want** to add new subnets when existing ones near exhaustion,  
**so that** I can scale the cluster without recreating the network.


## Proposed Solution

### API Details

1. Update `UDN`/`CUDN` CRD to allow `layer3.subnets` to accept multiple subnets per IP family.
2. Allow updates to the `layer3.subnets` field, but only to **append** new entries.
   - Reject updates that attempt to remove or modify existing subnet entries.
   - Reject new entries that overlap with existing subnets or use a different `hostSubnet`.


### Implementation Details

### Node subnet allocation

The current NAD implementation already supports multiple subnets in Layer3 topology. However,
preliminary testing shows that a node’s allocated subnet may change when a new cluster subnet is
appended. This behavior must be addressed to ensure stability and predictability in node subnet
assignments. Nodes should retain their originally assigned subnets regardless of subsequent subnet
expansion.


### API validations

The following two validations on the `subnets` field must be removed to allow more than one subnet
per IP family:
```
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:XValidation:rule="size(self) != 2 || !isCIDR(self[0].cidr) || !isCIDR(self[1].cidr) || cidr(self[0].cidr).ip().family() != cidr(self[1].cidr).ip().family()", message="When 2 CIDRs are set, they must be from different IP families"
	Subnets []Layer3Subnet `json:"subnets,omitempty"`
```
In place of the above, new validation logic should be added to enforce the following rules:
* Reject updates that attempt to remove or modify existing subnet entries.
* Reject new entries that overlap with existing subnets.
* Reject new entries that use a different hostSubnet size than the existing ones.


Additionally, the validation rule that currently makes the entire `spec` immutable must be removed:
```
	// +kubebuilder:validation:XValidation:rule="self == oldSelf", message="Spec is immutable"
	Spec UserDefinedNetworkSpec `json:"spec"`
```
Instead, immutability should be enforced at the sub-field level to preserve immutability where
needed.


### Testing Details

* **Unit Tests:** Extend subnet allocator tests to cover multiple subnets and dynamic expansion scenarios.
* **E2E Tests:** Test UDN behavior when new subnets are appended, and verify that nodes receive
  allocations from the correct ranges.
* **API Tests:** Validate CRD updates for append-only behavior and ensure invalid changes (e.g., removal
  or modification of existing subnets) are correctly rejected.


### Documentation Details

* Update `mkdocs.yml` with the OKEP reference.

## Risks, Known Limitations and Mitigations

* **Limitations**:
  * Removing or modifying subnets after creation is not supported.
  * All subnets must use the same `hostSubnet` size.  
    Supporting subnets with different `hostSubnet` sizes is currently downstream-only and depends on
    OVN commit [27cc274](https://github.com/ovn-org/ovn/commit/27cc274e66acd9e0ed13525f9ea2597804107348)
    *northd: Use lower priority for all src routes*.  
    This limitation may be lifted in the future.


## OVN Kubernetes Version Skew

To be updated based on reviewer feedback.

## Alternatives

* Recreate UDN with new subnet list — disruptive and impractical for large clusters.
* Use NAD for Layer3 — not applicable for users who standardize on UDN.

