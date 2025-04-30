# OKEP-5217: Routed ingress support for L2 UDN without dynamic IP allocation

* Issue: [#5217](https://github.com/ovn-org/ovn-kubernetes/issues/5217)

## Problem Statement

There is an increased interest for VMs in Kubernetes, which is generating the
desire to import VMs from another virtualization platforms.

VMs running in virtualization platforms are usually using static IP allocation,
where IPs are assigned directly in the guest.

One of the features most sought after by virtualization users is routed ingress
for L2 UDN, something which - right now - OVN-Kubernetes allows using the BGP
feature.

The current gap in OVN-Kubernetes is to provide routed ingress to VMs whose IPs
were assigned statically in the guest. This enhancement plans to bridge that
gap.

## Goals

- Detect/expose the MAC for the VM to be configured in OVNK for the VM (or disable port security)
- When creating the UDN, be able to specify the gateway IP address, so the migrated VM can keep the same default route.
- When creating the UDN, allow excludeSubnets to be used with L2 or L3 UDNs to ensure OVNK does not use an IP address from the range that VMs have already been assigned outside of the cluster.
- When creating the UDN, we may need a new IPAM mode to indicate to OVNK that it should reserve IPs for internal use (gateway IP potentially, and mp0 IP), but it should not auto assign IPs to pods. Note, we may not need this, but listing it just in case.

## Non-Goals

- `NetworkPolicies`, `MultiNetworkPolicies`, or `Services` to work, given
OVN-Kubernetes has no awareness of the assigned IP addresses. A way to
introspect / discover the IP address on the guest may be required to implement
any of those features.

## Follow-up goals

- Specifying the IP address of the VM for OVNK to use (but not assign) via persistent IP or some other mechanism

## Limitations

This feature is scoped for **virtualization** workloads. Hence, the only type
of overlay being targeted is `layer2`.

## Introduction

TODO

## User-Stories/Use-Cases

- As a traditional **VM owner** I want to import a VM - whose IPs were
statically configured - from an existing virtualization platform into
Kubernetes, attaching it to an overlay network. I want to ingress/egress using
the same IP address (the one assgined to the VM).

- As a VM owner, I want my VM's interfaces MAC addresses to stay the same when
the VM is migrated, restarted, or stopped then started, so that the established
TCP connections do not break.

- As a VM owner, I want to express the intent for my overlay UDN to have a
subnet defined, but not to feature IP address allocation, so that I can
set whatever IP addresses I want within the guest.

## Proposed Solution

What is the proposed solution to solve the problem statement?

### API Details

(... details, can point to PR PoC with changes but this section has to be
explained in depth including details about each API field and validation
details)

* add details if ovnkube API is changing

### Implementation Details

(... details on what changes will be made to ovnkube to achieve the
proposal; go as deep as possible; use diagrams wherever it makes sense)

* add details for differences between default mode and interconnect mode if any
* add details for differences between lgw and sgw modes if any
* add config knob details if any

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

## OVN Kubernetes Version Skew

which version is this feature planned to be introduced in?
check repo milestones/releases to get this information for
when the next release is planned for

## Alternatives

(List other design alternatives and why we did not go in that
direction)

## References

(Add any additional document links. Again, we should try to avoid
too much content not in version control to avoid broken links)
