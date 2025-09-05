# OKEP-5552: UDN Node Selection

* Issue: [#5552](https://github.com/ovn-org/ovn-kubernetes/issues/5552)

## Problem Statement

When scaling UDNs, the control-plane cost of rendering a topology is high. This is the core limiting factor to
being able to scale to 1000s of UDNs. While there are plans to also improve network controller performance with UDNs,
there is still valuable savings to be had by not rendering UDNs on nodes where they are not needed.

An example use case where this makes sense is when a Kubernetes cluster has its node resources segmented per tenant. In
this case, it only makes sense to run the tenant network (UDN) on the nodes where a tenant is allowed to run pods. This
allows for horizontal scaling to much higher number of overall UDNs running in a cluster.

## Goals

 * To expose a nodeSelector on the UDN CRD to allow the network to only be rendered on specific nodes.
 * To increase overall scalability of the number UDNs in a Kubernetes cluster with this solution.
 * To increase the efficiency of ovnkube operations on nodes where a UDN exists, but is not needed.

## Non-Goals

 * Orchestrating/Controlling pod lifecycle so that pods attaching to UDNs that specify a nodeSelector will be coordinated to
   use the same UDN nodeSelector.
 * To provide any type of network security guarantee about exposing UDNs to limited subset of nodes.

## Introduction

The purpose of this feature is to add a simple nodeSelector to the UDN and CUDN CRD that will allow a user to
only create the UDN networks on those specific nodes.

## User-Stories/Use-Cases

Story 1: Segment groups of nodes per tenant

As a cluster admin, I plan to dedicate groups of nodes to either a single tenant or small group of tenants. I plan
to create a CUDN per tenant, which means my network will only really need to exist on this group of nodes. I would
like to be able to provide a nodeSelector in my CUDN so that I can limit this network to only be rendered and managed
on the group of nodes that I specify. This way I will be able to have less resource overhead from OVN-Kubernetes on each node,
and be able to scale to a higher number of UDNs in my cluster.

## Proposed Solution

The proposed solution is to add a simple nodeSelector to the CUDN and UDN CRDs. This nodeSelector is expected to work
for any topology type.

### API Details

The spec of CUDN and UDN will be modified to add a new nodeSelector field:

```yaml
// UserDefinedNetworkSpec defines the desired state of UserDefinedNetworkSpec.
// +union
type UserDefinedNetworkSpec struct {
	// Topology describes network configuration.
	//
	// Allowed values are "Layer3", "Layer2".
	// Layer3 topology creates a layer 2 segment per node, each with a different subnet. Layer 3 routing is used to interconnect node subnets.
	// Layer2 topology creates one logical switch shared by all nodes.
	//
	// +kubebuilder:validation:Enum=Layer2;Layer3
	// +kubebuilder:validation:Required
	// +required
	// +unionDiscriminator
	Topology NetworkTopology `json:"topology"`

	// Layer3 is the Layer3 topology configuration.
	// +optional
	Layer3 *Layer3Config `json:"layer3,omitempty"`

	// Layer2 is the Layer2 topology configuration.
	// +optional
	Layer2 *Layer2Config `json:"layer2,omitempty"`

    // nodeSelector limits the network to selected nodes. This field
    // follows standard label selector semantics.
    // +optional
    NodeSelector metav1.LabelSelector `json:"nodeSelector"`
}
```

```yaml
// ClusterUserDefinedNetworkSpec defines the desired state of ClusterUserDefinedNetwork.
type ClusterUserDefinedNetworkSpec struct {
	// NamespaceSelector Label selector for which namespace network should be available for.
	// +kubebuilder:validation:Required
	// +required
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`

	// Network is the user-defined-network spec
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="has(self.topology) && self.topology == 'Layer3' ? has(self.layer3): !has(self.layer3)", message="spec.layer3 is required when topology is Layer3 and forbidden otherwise"
	// +kubebuilder:validation:XValidation:rule="has(self.topology) && self.topology == 'Layer2' ? has(self.layer2): !has(self.layer2)", message="spec.layer2 is required when topology is Layer2 and forbidden otherwise"
	// +kubebuilder:validation:XValidation:rule="has(self.topology) && self.topology == 'Localnet' ? has(self.localnet): !has(self.localnet)", message="spec.localnet is required when topology is Localnet and forbidden otherwise"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf", message="Network spec is immutable"
	// +required
	Network NetworkSpec `json:"network"`

    // nodeSelector limits the network to selected nodes. This field
    // follows standard label selector semantics.
    // +optional
    NodeSelector metav1.LabelSelector `json:"nodeSelector"`
}
```

If NodeSelector is not provided, the behavior is the same as today where the network is rendered on all nodes.
The NodeSelector will be allowed to be updated within the CR.

### Implementation Details

When the NodeSelector is provided, the network controllers ovnkube-controller as well as the node side will not be
started on non-selected nodes. Pods which are launched on non-selected nodes, but specify the UDN as an attachment will
naturally fail to be started during CNI ADD. CNI will specify an error that the network does not exist on this node.

If a NodeSelector is updated to a new value, or the node's labels are modified which would exclude a node from a network,
the following will occur:

1. If this node already has the network rendered, and a pod is attached, the network will not be torn down. No new pods
will be allowed to launch on this node. An error state will be propagated to the CUDN/UDN to indicate there is a stale pod/network
on a node.
2. If there is no pod attached to this network, and the network exists, it will be stopped on this node.

If a NodeSelector is updated to a new value, or the node's labels are modified which would include a node on a network, the
following will occur:

1. The network controller and node network controller will both start.
2. Any outstanding pods trying to attach to this network will now be allowed to attach.

Finally, a new status condition will be added to CUDN/UDN that will indicate how many nodes are serving a network:
```yaml
status:
  conditions:
    - type: NodesSelected
      status: "True"
      reason: SelectorResolved
      message: "5 nodes matched selector 'region=west'"
```

A new StatusManager instance will be used for CUDN/UDN to enable per "zone" reporting that will allow for cumulative node
calculations and overall status reporting.

### Testing Details

* Unit Tests will be added to ensure the nodeSelector behavior works as expected, including API tests, checking that
OVN switches/routers are not created when excluded from the nodeSelector, etc.
* E2E Tests will be added to create a CUDN/UDN with a nodeSelector and ensure pod traffic works correctly between nodes.
* Benchmark/Scale testing will be done to show the resource savings of 1000s of nodes with 1000s of UDNs.

### Documentation Details

* User-Defined Network feature documentation will be updated with a user guide for this new feature.

## Risks, Known Limitations and Mitigations

No known risks. This is not a substitute for overall performance improvements for UDN, but rather an optimization for
clusters where UDNs are only needed on a subset of nodes.

## OVN Kubernetes Version Skew

Targeted for release 1.2.

## Alternatives

Dynamically detecting if a UDN is needed on a node. This could be done by not starting network controllers until a pod is
launched on a node that needs it. However, this could hurt pod latency as we will then have to wait for the network controller
to start. Additionally, things get complicated when the last pod connected to a UDN is deleted. We would then stop the network
controller, and if another pod starts we would have to bring it back up again. This could cause a lot of churn and actually
make performance worse.

## References

None
