# OKEP-5233: Pre-assigned network configuration for primary user defined networks workloads

* Issue: [#5233](https://github.com/ovn-org/ovn-kubernetes/issues/5233)

## Problem Statement

Migrating legacy workloads with pre-assigned network configurations (IP, MAC, default gateway)
to OVN-Kubernetes is currently not possible. There is a need to import these workloads, preserving 
their network configuration, while also enabling non-NATed traffic to better integrate with
existing infrastructures.

## Goals

- Enable pods on primary User Defined Network (UDN) to use a pre-defined static network 
  configuration including IP address, MAC address, and default gateway. 
- Ensure it is possible to enable non-NATed traffic for pods with the static network 
  configuration by exposing the network through BGP.
- TODO: Explicit statement around Layer2/Localnet support (once agreed)

## Non-Goals

- Modifying the default gateway and management IPs of a primary UDN after it was created.
- Modifying a pod's network configuration after the pod was created.
- Support for non-NATed traffic in secondary networks.
- Pre-configured IP/MAC addresses for pods in Layer3 networks.

## Introduction

Legacy workloads, particularly virtual machines, are often set up with static
network configurations. When migrating to OVN-Kubernetes UDNs,
it should be possible to integrate these gradually to prevent disruptions. 

Currently, OVN-Kubernetes allocates IP addresses dynamically and it generates the MAC 
addresses from it. It sets the pod's default gateway to the first usable IP address of its subnet.
It additionally reserves the second usable IP address for the internal management port which 
excludes it from being available for workloads. 

## User-Stories/Use-Cases

- As a user, I want to define a specific default gateway IP for a new primary User Defined
Network (UDN)  so that it matches the gateway IP used by the workload previously.

- As a user, I want the ability to configure a primary UDN with a non-default management IP
address to prevent IP conflicts with the workloads I am importing.

- As a user, I want to assign a predefined IP address and MAC address to a pod to ensure the
network identity of my imported workload is maintained.

## Proposed Solution

### Primary UDN configuration

To support the migration of pre-configured workloads, the UDN and cluster UDN (CUDN) API has to 
be enhanced. The aim is to provide control over the IP addresses that OVN-Kubernetes
consumes in the overlay network, this includes the default gateway and management IPs.
The proposed changes are specified in the [User Defined Network API changes](#user-defined-network-api-changes) section.

### Pod network identity

OVN-Kubernetes currently supports configuring pods' secondary network interfaces through
the `k8s.v1.cni.cncf.io/networks` annotation, which contains a JSON array of 
[NetworkSelectionElement](https://github.com/k8snetworkplumbingwg/network-attachment-definition-client/blob/e12bd55d48a1f798a1720218819063f5903b72e3/pkg/apis/k8s.cni.cncf.io/v1/types.go#L136-L171)
objects. Additionally, it is possible to modify the cluster's default network attachment by 
setting the `v1.multus-cni.io/default-network` annotation to a singular NetworkSelectionElement 
object.

To enable using pre-defined MAC and IP addresses on pods attached to a primary UDN,
the `v1.multus-cni.io/default-network` will be reused, as it is a well-known annotation for 
configuring the pod's default network. The `k8s.v1.cni.cncf.io/networks` annotation is specific to 
secondary networks and expects a list of networks, which does not fit well with primary UDNs.
With the proposed approach, the `k8s.ovn.org/primary-udn-ipamclaim` annotation, used to link a 
pod with a matching claim, will be deprecated in favor of the `IPAMClaimReference` field in the 
NetworkSelectionElement. When `IPAMClaimReference` is specified we will update its status to reflect
the result of the IP allocation, see [IPAMClaim API changes](#ipamclaim-api-changes).
It is important that OVN-Kubernetes tries to handle potential MAC and IP address conflicts within
the realm of the overlay network in a clear and predictable fashion whenever possible.


### API Details

#### User Defined Network API changes

- TODO: (C)UDN CRD updates
- TODO: Ensure backwards compatibility
- TODO: Block day2 changes through CELs

#### IPAMClaim API changes

- TODO: Add status conditions to the IPAMClaim CRD

### Implementation Details

The changes outlined in this enhancement should be configurable. This means a configuration knob
is required to instruct OVN-Kubernetes on whether to process the annotation described in the 
[Pod network identity](#pod-network-identity) section.

The `v1.multus-cni.io/default-network` annotation should only be processed for new pods, 
modifying it after the addresses were allocated won't be reflected in the pods network 
configuration and this should be blocked through a
[Validating Admission Policy](https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/):
```
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: pre-assigned-network-identity
spec:
  matchConstraints:
    resourceRules:
      - apiGroups:   [""]
        apiVersions: ["v1"]
        operations:  ["UPDATE"]
        resources:   ["pods"]
  failurePolicy: Fail
  validations:
    - expression: "('v1.multus-cni.io/default-network' in oldObject.metadata.annotations) == ('v1.multus-cni.io/default-network' in object.metadata.annotations)"
      message: "The 'v1.multus-cni.io/default-network' annotation cannot be changed after the pod was created"
```
The `NetworkSelectionElement` structure has an extensive list of fields, this enhancement 
focuses only on the following:
```cgo
type NetworkSelectionElement struct {
	// Name contains the name of the Network object this element selects
	// TODO: Is the Name needed? What's the convention for default
	Name string `json:"name"`
	// IPRequest contains an optional requested IP addresses for this network
	// attachment
	IPRequest []string `json:"ips,omitempty"`
	// MacRequest contains an optional requested MAC address for this
	// network attachment
	MacRequest string `json:"mac,omitempty"`
	// IPAMClaimReference container the IPAMClaim name where the IPs for this
	// attachment will be located.
	IPAMClaimReference string `json:"ipam-claim-reference,omitempty"`
}
```

Any other field set in the struct will be ignored by OVN-Kubernetes. 
With `k8s.ovn.org/primary-udn-ipamclaim` being deprecated in favor of the `IPAMClaimReference` field
in the `NetworkSelectionElement` we have to define the expected behavior. To avoid conflicting 
settings when `v1.multus-cni.io/default-network` is set the `k8s.ovn.org/primary-udn-ipamclaim` is 
going to be ignored, it will be reflected in the opposite scenario for backwards compatibility 
with a plan to remove it in a future release.
> TODO: How do we deprecate an annotation

OVN-Kubernetes currently [generates](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/3ef29b9a32b04b7917a0afd6b0e9651d17242ed7/go-controller/pkg/util/net.go#L100-L113)
the overlay MAC addresses from the IPs:

- IPv4: It takes the four octets of the address (e.g `AA.BB.CC.DD`) and uses them to
create the MAC address with a constant prefix (e.g. `0A:58:AA:BB:CC:DD`).
- IPv6: Computes a SHA256 checksum from the IPv6 string and uses the first four bytes for the MAC
address with the `0A:58` constant prefix(e.g. `0A:58:SHA[0]:SHA[1]:SHA[2]:SHA[3]`).

Although unlikely, we need to implement logic that ensures that the MAC address requested through
the `NetworkSelectionElement` does not conflict with any other configured address on the UDN
(including addresses consumed by OVN-Kubernetes).
A similar approach is required for IP address conflict detection.
When a conflict is detected the pod should not start and an appropriate event should be emited.

When the `NetworkSelectionElement` contains an `IPAMClaimReference` the referenced IPAMClaim should
reflect the IP allocation status including error reporting through the newly introduced
`Conditions` status field.
In the opposite scenario where the `NetworkSelectionElement` does not specify the `IPAMClaimReference`
the IP allocation is not persisted when the pod is removed.

### Testing Details


TOOD: Important E2E testing scenarios:
- IP/MAC Address conflict detection and reporting through events and IPAMClaim conditions
- Annotation cannot be changed once set
- VM live migration works without issues while using predefined addresses
- After configuring custom GW and Management IPs they can be consumed as pre-assigned addresses


* Unit Testing details
* E2E Testing details
* API Testing details
* Scale Testing details
* Cross Feature Testing details - coverage for interaction with other features

### Documentation Details

## Risks, Known Limitations and Mitigations

- TODO: changing the NSE annotation after the pod was created
- TODO: with the current approach users won't be able to modify the cluster default network when using a primary UDN
- TODO: IP and MAC conflicts
- TODO: about IPAMClaims "limitation"
- TODO: Pre-configured IP/MAC addresses for pods in Layer3 networks - this won't work because
there is a subnet per node so because it is not possible to know which node the pod will land 
one and which subnet it will belong to. 
- TODO: BGP only works for CUDNs

## OVN Kubernetes Version Skew

## Alternatives

Instead of the [Pod network identity](#pod-network-identity) approach, we could expand the
IPAMClaim API. It currently lacks IP request capabilities, and using IPAMClaim for MAC addresses
is confusing. Introducing a new API would mean deprecating the IPAMClaim, while managing
upgrades and supporting both solutions for a period of time. This requires significant effort, which
is not feasible at this time.

## References

(Add any additional document links. Again, we should try to avoid
too much content not in version control to avoid broken links)