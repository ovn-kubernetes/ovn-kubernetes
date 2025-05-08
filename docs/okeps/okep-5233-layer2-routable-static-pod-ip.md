# OKEP-5233: Pre-assigned network configuration for primary user defined networks workloads

* Issue: [#5233](https://github.com/ovn-org/ovn-kubernetes/issues/5233)

## Problem Statement

Migrating workloads with pre-assigned network configurations (IP, MAC, default gateway)
to OVN-Kubernetes is currently not possible. There is a need to import workloads, preserving 
their network configuration, while also enabling non-NATed traffic to better integrate with
existing infrastructures.

## Goals

- Enable pods on primary user defined network to use a pre-defined static network configuration 
  including IP address, MAC address, and default gateway. 
- Ensure it is possible to enable non-NATed traffic for pods with the static network 
  configuration
- Allow for excluding subnets from the dynamic IP allocation for primary User-Defined Networks
  (UDNs) 

## Non-Goals

- TBD: Enabling non-NATed Pod traffic without exposing the network through BGP
- TBD: Secondary networks
- Modifying a pod's pre-assigned network configuration after the pod was created
- Persistent IPs for Layer3 networks

## Introduction

Legacy workloads, particularly virtual machines, are often set up with static
network configurations. When migrating to OVN-Kubernetes UDNs,
it should be possible to integrate these gradually to prevent disruptions. 

Currently, OVN-Kubernetes allocates IP addresses dynamically and it generates the MAC 
addresses from it. It sets the pod's default gateway to the first usable IP address of its subnet.
It additionally reserves the second usable IP address for the internal management port which 
excludes it from being available for workloads. 

## User-Stories/Use-Cases

(What new user-stories/use-cases does this OKEP introduce?)

A user story should typically have a summary structured this way:

1. **As a** [user concerned by the story]
2. **I want** [goal of the story]
3. **so that** [reason for the story]

The “so that” part is optional if more details are provided in the description.
A story can also be supplemented with examples, diagrams, or additional notes.

e.g

Story 1: Deny traffic at a cluster level

As a cluster admin, I want to apply non-overridable deny rules to certain pod(s)
and(or) Namespace(s) that isolate the selected resources from all other cluster
internal traffic.

For Example: The admin wishes to protect a sensitive namespace by applying an
AdminNetworkPolicy which denies ingress from all other in-cluster resources
for all ports and protocols.

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