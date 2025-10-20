# OKEP-5672: Metadata Service

* Issue: [#5672](https://github.com/ovn-org/ovn-kubernetes/issues/5672)

## Problem Statement

VM cloud platforms typically offer metadata service at 169.254.169.254.
For Kubevirt VMs running on ovn-kubernetes, we would like to offer the same kind of support.

## Goals

* Allow a user to configure metadata service access with their UDN that enables access to 169.254.169.254 with Internal Traffic Policy (ITP) local
* Limit access to this IP within the SDN (ovn-kubernetes networked pods)

## Non-Goals

* Standardizing any kind of REST API, or metadata pod backend for the service
* IPv6 Support

## Introduction

Most VM cloud platform providers expose a metadata service to VMs. This service is used by the VM to get cloud-init
data at boot time, as well as to query for metadata post-boot. The providers all consistently use 169.254.169.254 as
the IP to access metadata. However, each platform uses different REST API to query this metadata. Kubevirt offers
cloud-init via a [NoCloud](https://cloudinit.readthedocs.io/en/latest/reference/datasources/nocloud.html) option, but
this does not provide a dynamic source that can be queried post-boot.

The goal of this feature is to provide a way that a user can specify to OVN-Kubernetes that they would like to use
a metadata service provided at 169.254.169.254. OVN-Kubernetes will handle creating an OVN Load Balancer that will use
169.254.169.254 as the VIP, and then the user can specify backends. How these backends are implemented is beyond the
scope of this enhancement or OVN-Kubernetes. In the future it may be standardized by a higher layer, like Kubevirt.

Note there is limited support for metadata service on VM cloud platforms for IPv6. There is also no consistent IPv6
address used. For that reason, IPv6 support will be deferred from this enhancement.

## User-Stories/Use-Cases

Story 1: Provide Dynamic Metadata to a VM

As a tenant using Kubevirt, I want to enable my VMs to be able to query a metadata service I manage as pods
so that my VM can get updated token information during runtime. The VM should be able to query 169.254.169.254
to get dynamically updated information.

## Proposed Solution

This proposal includes specifying new API within the UDN CRD spec to allow a user to specify the metadata service
name in their UDN. Once identified, OVN-Kubernetes will add an OVN Load Balancer with the 169.254.169.254 VIP for
this service.

### API Details

CUDN and UDN API will be changed to allow a new field to reference a metadata service name:

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: shared-primary
spec:
  namespaceSelector:
    matchLabels:
      cudn-access: shared
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.20.0.0/16
    metadataServiceRef:
      name: shared-metadata
      namespace: infra-services
```

For CUDN, the namespace is required to indicate which namespace contains the service. For UDN, it is implied:

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: UserDefinedNetwork
metadata:
  name: tenant-a-primary
  namespace: tenant-a
spec:
  topology: Layer3
  layer3:
    role: Primary
    subnets:
    - cidr: 10.10.0.0/16
  metadataServiceRef:
    name: tenant-a-metadata
```

Additionally, the metadataServiceRef is only valid for Primary networks, since Secondary networks do not support
services. Full CRD golang changes:

```go
// MetadataServiceRef specifies a metadata service to be exposed via the special VIP 169.254.169.254.
type MetadataServiceRef struct {
	// Name is the name of the Service that backs the metadata endpoint.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name"`

	// Namespace is the namespace of the Service.
	//
	// This field is optional for UserDefinedNetwork (defaults to the same namespace),
	// but required for ClusterUserDefinedNetwork.
	//
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// UserDefinedNetworkSpec defines the desired state of a UserDefinedNetwork.
// +union
type UserDefinedNetworkSpec struct {
// Topology describes network configuration.
//
// Allowed values are "Layer3" and "Layer2".
// Layer3 topology creates a layer 2 segment per node, each with a different subnet.
// Layer 3 routing is used to interconnect node subnets.
// Layer2 topology creates one logical switch shared by all nodes.
//
// +kubebuilder:validation:Enum=Layer2;Layer3
// +kubebuilder:validation:Required
// +unionDiscriminator
Topology NetworkTopology `json:"topology"`

// Layer3 is the Layer3 topology configuration.
// +optional
Layer3 *Layer3Config `json:"layer3,omitempty"`

// Layer2 is the Layer2 topology configuration.
// +optional
Layer2 *Layer2Config `json:"layer2,omitempty"`

// MetadataServiceRef optionally references a Service that acts as the metadata endpoint for this network.
//
// The Service will be exposed at the special virtual IP 169.254.169.254 within the network.
// Only supported for Primary network roles.
//
// +optional
MetadataServiceRef *MetadataServiceRef `json:"metadataServiceRef,omitempty"`

// +kubebuilder:validation:XValidation:rule="!has(self.metadataServiceRef) || (has(self.layer3) && has(self.layer3.subnets) && self.layer3.subnets.exists(s, isCIDR(s.cidr) && cidr(s.cidr).ip().family() == 4)) || (has(self.layer2) && has(self.layer2.subnets) && self.layer2.subnets.exists(s, isCIDR(s) && cidr(s).ip().family() == 4))", message="metadataServiceRef requires at least one IPv4 subnet"
// +kubebuilder:validation:XValidation:rule="!has(self.metadataServiceRef) || (has(self.layer3) && has(self.layer3.role) && self.layer3.role == 'Primary') || (has(self.layer2) && has(self.layer2.role) && self.layer2.role == 'Primary')", message="metadataServiceRef is only supported for Primary network role"
}

// ClusterUserDefinedNetworkSpec defines the desired state of a ClusterUserDefinedNetwork.
type ClusterUserDefinedNetworkSpec struct {
// NamespaceSelector selects which namespaces this cluster-wide network applies to.
// +kubebuilder:validation:Required
NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`

// Network defines the desired network configuration.
// +kubebuilder:validation:Required
Network NetworkSpec `json:"network"`
}

// NetworkSpec defines the desired state of the underlying network configuration.
// +union
type NetworkSpec struct {
// Topology describes network configuration.
//
// Allowed values are "Layer3", "Layer2", and "Localnet".
//
// +kubebuilder:validation:Enum=Layer2;Layer3;Localnet
// +kubebuilder:validation:Required
// +unionDiscriminator
Topology NetworkTopology `json:"topology"`

// Layer3 is the Layer3 topology configuration.
// +optional
Layer3 *Layer3Config `json:"layer3,omitempty"`

// Layer2 is the Layer2 topology configuration.
// +optional
Layer2 *Layer2Config `json:"layer2,omitempty"`

// Localnet is the Localnet topology configuration.
// +optional
Localnet *LocalnetConfig `json:"localnet,omitempty"`

// MetadataServiceRef optionally references a Service that acts as the metadata endpoint for this network.
//
// The Service will be exposed at the special virtual IP 169.254.169.254 within the network.
// Only supported for Primary network roles.
// For ClusterUserDefinedNetwork, the namespace field is required.
//
// +optional
MetadataServiceRef *MetadataServiceRef `json:"metadataServiceRef,omitempty"`


// +kubebuilder:validation:XValidation:rule="!has(self.metadataServiceRef) || (has(self.layer3) && has(self.layer3.subnets) && self.layer3.subnets.exists(s, isCIDR(s.cidr) && cidr(s.cidr).ip().family() == 4)) || (has(self.layer2) && has(self.layer2.subnets) && self.layer2.subnets.exists(s, isCIDR(s) && cidr(s).ip().family() == 4))", message="metadataServiceRef requires at least one IPv4 subnet"
// +kubebuilder:validation:XValidation:rule="!has(self.metadataServiceRef) || (has(self.layer3) && has(self.layer3.role) && self.layer3.role == 'Primary') || (has(self.layer2) && has(self.layer2.role) && self.layer2.role == 'Primary')", message="metadataServiceRef is only supported for Primary network role"
// +kubebuilder:validation:XValidation:rule="!has(self.metadataServiceRef) || has(self.metadataServiceRef.namespace)", message="metadataServiceRef.namespace is required for ClusterUserDefinedNetwork"
}
```

### Implementation Details

When a (C)UDN is created with the metadataServiceRef field, the UDN controller will render the NAD with an updated
NetConf that specifies the metadata information. OVN-Kubernetes services controller will be modified to detect this
reference in the NAD, and then when rendering this service, it will add additional OVN LB to the worker switch only, with
VIP 169.254.169.254. Note, this LB will not be added to the Gateway Router (GR) as this LB will only be accessible
from ovn-kubernetes wired pods within the SDN.

### Testing Details

* New unit tests to make sure CRD works as expected.
* New unit tests to ensure that the load balancer is rendered on the worker switch correctly.
* New E2E test to test metadata service.

### Documentation Details

* New feature documentation to show how to use the metadata service, and an example use case.

## Risks, Known Limitations and Mitigations

Limitations include:

* No host network endpoint support.
* No inter-UDN support (like with Network Connect).
* No support for any other service type other than internal.
* No IPv6 support.

## OVN-Kubernetes Version Skew

Targeted for release 1.2.

## Alternatives

An alternative that is similar to OpenStack is to configure a localport for each UDN that connects from the OVN switch
to an HA proxy instance running in a netns (similar to the Kubernetes pod/container paradigm).
The packet from a VM destined to 169.254.169.254 is then rerouted towards this process. More details can be found here:
https://docs.openstack.org/developer/networking-ovn/design/metadata_api.html

The drawback to this is it is a specific solution, which is integrated into the OpenStack design. The scope of the
feature would now extend into the metadata endpoint running HA Proxy, rather than leaving it up to the user to implement.
The proposed solution accomplishes the same goal, with less constraints and a more simple solution.

## References

None
