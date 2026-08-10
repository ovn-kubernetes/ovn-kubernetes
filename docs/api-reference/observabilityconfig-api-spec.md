# API Reference

## Packages
- [k8s.ovn.org/v1alpha1](#k8sovnorgv1alpha1)


## k8s.ovn.org/v1alpha1

Package v1alpha1 contains API Schema definitions for the ObservabilityConfig v1alpha1 API group

### Resource Types
- [ObservabilityConfig](#observabilityconfig)



#### FeatureConfig







_Appears in:_
- [ObservabilitySpec](#observabilityspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `probability` _integer_ | Probability is the probability of the feature being sampled in percent. |  | Maximum: 100 <br />Minimum: 0 <br />Required: \{\} <br />Required: \{\} <br /> |
| `feature` _[ObservabilityFeature](#observabilityfeature)_ | Feature is the Observability feature that should be sampled. |  | Enum: [NetworkPolicy AdminNetworkPolicy EgressFirewall UDNIsolation MulticastIsolation] <br />Required: \{\} <br />Required: \{\} <br /> |


#### Filter



Filter allows to apply ObservabilityConfig in a granular manner.
Currently, it supports node and namespace based filtering.
If both node and namespace filters are specified, they are logically ANDed.

_Validation:_
- MinProperties: 1

_Appears in:_
- [ObservabilitySpec](#observabilityspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#labelselector-v1-meta)_ | nodeSelector applies ObservabilityConfig only to nodes that match the selector. |  | Optional: \{\} <br /> |
| `namespaces` _string array_ | namespaces is a list of namespaces to which the ObservabilityConfig should be applied.<br />It only applies to the namespaced features, currently that includes NetworkPolicy and EgressFirewall. |  | MinItems: 1 <br />Optional: \{\} <br /> |


#### ObservabilityConfig



ObservabilityConfig describes the OVN Observability API, which binds
observed samples to a collector ID, for a given set of features and filters.



_Appears in:_
- [ObservabilityConfigList](#observabilityconfiglist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1alpha1` | | |
| `kind` _string_ | `ObservabilityConfig` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ObservabilitySpec](#observabilityspec)_ |  |  | Required: \{\} <br />Required: \{\} <br /> |
| `status` _[ObservabilityStatus](#observabilitystatus)_ |  |  | Optional: \{\} <br /> |




#### ObservabilityFeature

_Underlying type:_ _string_



_Validation:_
- Enum: [NetworkPolicy AdminNetworkPolicy EgressFirewall UDNIsolation MulticastIsolation]

_Appears in:_
- [FeatureConfig](#featureconfig)

| Field | Description |
| --- | --- |
| `NetworkPolicy` |  |
| `AdminNetworkPolicy` |  |
| `EgressFirewall` |  |
| `UDNIsolation` |  |
| `MulticastIsolation` |  |


#### ObservabilitySpec







_Appears in:_
- [ObservabilityConfig](#observabilityconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `collectorID` _integer_ | CollectorID is the OVN Sample_Collector set_id: unique across the cluster, range 1 to 4,294,967,295 (MaxUint32). |  | Minimum: 1 <br />Required: \{\} <br />Required: \{\} <br /> |
| `features` _[FeatureConfig](#featureconfig) array_ | Features is a list of Observability features that can generate samples and their probabilities for a given collector. |  | MinItems: 1 <br />Required: \{\} <br />Required: \{\} <br /> |
| `filter` _[Filter](#filter)_ | Filter allows to apply ObservabilityConfig in a granular manner. |  | MinProperties: 1 <br />Optional: \{\} <br /> |


#### ObservabilityStatus



ObservabilityStatus contains the observed status of the ObservabilityConfig.



_Appears in:_
- [ObservabilityConfig](#observabilityconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#condition-v1-meta) array_ |  |  |  |


