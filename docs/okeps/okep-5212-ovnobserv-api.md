# OKEP-5212: OVNObservability API

* Issue: [#5212](https://github.com/ovn-org/ovn-kubernetes/issues/5212)

## Problem Statement

Current functionality only allows enabling sampling for all supported features at 100% sampling rate at the same time, 
the goal is to provide an API for more fine-grained configuration.

## Goals

- Design a CRD to configure OVN Observability for given user stories

## Non-Goals

- Discuss OVN Observability feature in general

## Introduction

OVN Observability is a feature that provides visibility for ovn-kubernetes-managed network by using sampling mechanism.
That means, that network packets generate samples with user-friendly description to give more information about what 
and why is happening in the network.
This feature is designed to be used with the [netobserv-operator](https://github.com/netobserv/network-observability-operator)

## User-Stories/Use-Cases

- Observability: As a cluster admin, I want to enable OVN Observability integration with netobserv so that I can use network policy correlation feature.
- Debuggability: As a cluster admin, I want to debug a networking issue using OVN Observability feature.

## Proposed Solution

There is a number of k8s features that can be used to configure OVN Observability right now, and some more that we expect
to be added in the future.
Currently supported features are:
- NetworkPolicy
- (Baseline)AdminNetworkPolicy
- EgressFirewall
- Primary UDN network isolation from the default network
- Multicast ACLs


In the future more observability features, like egressIP and services, but also networking-specific features
like logical port ingress/egress will be added.

Every feature may set a sampling probability, that defines how often a packet will be sampled, in percent. 100% means every packet
will be sampled, 0% means no packets will be sampled.

Currently only on/off mode is supported, where on mode turns all available features with 100% probability.


### Observability use case

For observability use case, a cluster admin can set a sampling probability for each feature across the whole cluster.
As sampling configuration may affect cluster performance, not every admin may want all features enabled with 100% probability.
We expect this type of configuration to change rarely.

This configuration should be handled by ovn-kubernetes to configure sampling, and by netobserv ebpf agent to "subscribe" to
the samples enabled by this configuration.

### Debuggability use case

Debuggability is expected to be turned on during debugging sessions, and turned off after the session is finished.
It will most likely have 100% probability for required features, and also may enable more networking-related features, like
logical port ingress/egress for cluster admins. Debugging configuration is expected to have more granular filters to
debug specific problems with minimal overhead. For example, per-node or per-namespace filtering may be required.

Debuggability sampling may be used with or without Network Observability Operator being installed:
- without netobserv, samples may be printed or forwarded to a file by ovnkube-observ binary, which is a part of ovnkube-node pod on every node.
- with neobserv, samples may be displayed in a similar to observability use case way, but may need another interface to ensure
  cluster users won't be distracted by samples generated for debugging.

This configuration should be handled by ovn-kubernetes to configure sampling, and by netobserv ebpf agent OR ovnkube-observ script
to "subscribe" to the samples enabled by this configuration.

### Workflow Description

1. Cluster admin wants to enable OVN Observability for admin-managed features.
2. Cluster admin created a CR with 100% probabilities for (Baseline)AdminNetworkPolicy and EgressFirewall.
3. Messages like "Allowed by admin network policy test, direction Egress" appear on netobserv UI.
4. Cluster admin sees CPU usage by ovnkube pods grows up 10%, and reduces the sampling probability to 50%.
5. Still most of the flows have correct enrichment with lower overhead.
6. Admin is happy. Until...
7. A user from project "bigbank" reports that they can't access the internet from pod A.
8. Cluster admin enables debugging for project "bigbank" with 100% probability for NetworkPolicies.
9. netobserv UI or ovnkube-oberv script shows a sample with <`source pod A`, `dst_IP`> saying "Denied by network policy in namespace bigbank, direction Egress"
10. Admin now has to convince the user that they are missing an allow egress rule to `dst_IP` for pod A.
11. Network policy is fixed, debugging can be turned off.

### API Details

#### Observability filtering for debugging

Mostly for debugging (but also can be used for observability if needed), we may need more granular filtering,
like per-node or per-namespace filtering.

- Per-node filtering may be implemented by adding a node selector to `ObservabilityConfig`, and is easy to implement in ovn-k
  as every node has its own ovn-k instance with OVN-IC.
- Per-namespace filtering can only be applied to the namespaced Observability Features, like NetworkPolicy and EgressFirewall.
  Cluster-scoped features, like (Baseline)AdminNetworkPolicy, can only be additionally filtered based on name. Label selectors
  can't be used, as observability module doesn't watch any objects.

#### ObservabilityConfig CRD

The CRD for this feature will be a part of ovn-kubernetes to allow using it upstream.

```go

// ObservabilityConfig described OVN Observability configuration.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=observabilityconfig,scope=cluster
// +kubebuilder:singular=observabilityconfig
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type ObservabilityConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +kubebuilder:validation:Required
	// +required
	Spec ObservabilitySpec `json:"spec"`
	// +optional
	Status ObservabilityStatus `json:"status,omitempty"`
}

type ObservabilitySpec struct {
	// CollectorID is the ID of the collector that should be unique across the cluster, and will be used to receive configured samples.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +required
	CollectorID int32 `json:"collectorID"`
  // Features is a list of Observability features that can generate samples and their probabilities for a given collector.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +required
	Features []FeatureConfig `json:"features"`

	// Filter allows to apply ObservabilityConfig in a granular manner.
	// +optional
	Filter *Filter `json:"filter,omitempty"`
}

type FeatureConfig struct {
	// Probability is the probability of the feature being sampled in percent.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +required
	Probability int32
	// Feature is the Observability feature that should be sampled.
	// +kubebuilder:validation:Required
	// +required
	Feature ObservabilityFeature
}

// ObservabilityStatus contains the observed status of the ObservabilityConfig.
type ObservabilityStatus struct {
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Filter allows to apply ObservabilityConfig in a granular manner.
// Currently, it supports node and namespace based filtering.
// If both node and namespace filters are specifies, they are logically ANDed.
// +kubebuilder:validation:MinProperties=1
type Filter struct {
  // nodeSelector applies ObservabilityConfig only to nodes that match the selector.
	// +optional
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`
	// namespaces is a list of namespaces to which the ObservabilityConfig should be applied.
	// It only applies to the namespaced features, currently that includes NetworkPolicy and EgressFirewall.
	// +kubebuilder:MinItems=1
	// +optional
	Namespaces *[]string `json:"namespaces,omitempty"`
}

// ObservabilityStatus contains the observed status of the ObservabilityConfig.
type ObservabilityStatus struct {
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:validation:Enum=NetworkPolicy;AdminNetworkPolicy;EgressFirewall;UDNIsolation;MulticastIsolation
type ObservabilityFeature string

const (
	NetworkPolicy      ObservabilityFeature = "NetworkPolicy"
	AdminNetworkPolicy ObservabilityFeature = "AdminNetworkPolicy"
	EgressFirewall     ObservabilityFeature = "EgressFirewall"
	UDNIsolation       ObservabilityFeature = "UDNIsolation"
	MulticastIsolation ObservabilityFeature = "MulticastIsolation"
)
```

For now, we only have use cases for 2 different `ObservabilityConfig`s in a cluster, and ovn-kubernetes will likely have a
limit for the number of CRs that will be handled.

For the integration with netobserv, we could use a label on `ObservabilityConfig` to signal that configured samples should be
reflected via netobserv. For example, `netobserv.io/ovn-observability: "true"`.

### Implementation Details

Filtering for debugging use case may be tricky as explained in "Observability filtering for debugging" section.
But it is important to implement, as OVN implementation works in a way, where performance optimizations can only be
applied when there is only 1 `ObservabilityConfig` for a given feature. That means, adding a second `ObservabilityConfig`
for a feature will have a bigger performance impact than creating a new one.

### Testing Details

- e2e downstream
- perf/scale testing for dataplane and control plane as a GA plan
- netobserv testing for correct GUI representation

### Documentation Details

* New proposed additions to ovn-kubernetes.io for end users
to get started with this feature
* when you open an OKEP PR; you must also edit
https://github.com/ovn-org/ovn-kubernetes/blob/13c333afc21e89aec3cfcaa89260f72383497707/mkdocs.yml#L135
to include the path to your new OKEP (i.e Feature Title: okeps/<filename.md>)

## Risks, Known Limitations and Mitigations

The biggest risk of enabling observability is performance overhead.
This will be mitigated by scale-testing and sampling percentage configuration. 
e2e testing upstream is not possible until github runners allow using kernel 6.11

## OVN Kubernetes Version Skew

All components were designed to be backwards-compatible
- old kernel + new OVS: we control these versions in a cluster and make sure to bump OVS version on supported kernel
- old OVS + new OVN: OVN uses "sample" action that is available in older OVS versions
- old OVN + new ovn-k: rolled out at the same time, no version skew is expected
- old kernel/OVS/ovn-kubernetes + new netobserv: netobserv can detect if it's running with older kernel/OVS/ovn-kubernetes version 
and warn when the users try to enable the feature.

## Alternatives

## References
