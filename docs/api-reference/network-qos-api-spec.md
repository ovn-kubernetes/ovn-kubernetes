# API Reference

## Packages
- [k8s.ovn.org/v1alpha1](#k8sovnorgv1alpha1)


## k8s.ovn.org/v1alpha1

Package v1alpha1 contains API Schema definitions for the network v1 API group

### Resource Types
- [NetworkQoS](#networkqos)



#### Bandwidth



Bandwidth controls the maximum of rate traffic that can be sent
or received on the matching packets.



_Appears in:_
- [Rule](#rule)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `rate` _integer_ | rate The value of rate limit in kbps. Traffic over the limit<br />will be dropped. |  | Format: int64 <br />Maximum: 4.294967295e+09 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `burst` _integer_ | burst The value of burst rate limit in kilobits.<br />This also needs rate to be specified. |  | Format: int64 <br />Maximum: 4.294967295e+09 <br />Minimum: 1 <br />Optional: \{\} <br /> |


#### CIDR

_Underlying type:_ _string_

CIDR is an IPv4 or IPv6 network in CIDR notation, or a single IP address.

_Validation:_
- MaxLength: 49

_Appears in:_
- [IPBlock](#ipblock)



#### Classifier







_Appears in:_
- [Rule](#rule)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `to` _[Destination](#destination) array_ |  |  | MaxItems: 100 <br />Optional: \{\} <br /> |
| `ports` _[Port](#port) array_ |  |  | Optional: \{\} <br /> |


#### Destination



Destination describes a peer to apply NetworkQoS configuration for the outgoing traffic.
Only certain combinations of fields are allowed.



_Appears in:_
- [Classifier](#classifier)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `podSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#labelselector-v1-meta)_ | podSelector is a label selector which selects pods. This field follows standard label<br />selector semantics; if present but empty, it selects all pods.<br />If namespaceSelector is also set, then the NetworkQoS as a whole selects<br />the pods matching podSelector in the Namespaces selected by NamespaceSelector.<br />Otherwise it selects the pods matching podSelector in the NetworkQoS's own namespace. |  | Optional: \{\} <br /> |
| `namespaceSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#labelselector-v1-meta)_ | namespaceSelector selects namespaces using cluster-scoped labels. This field follows<br />standard label selector semantics; if present but empty, it selects all namespaces.<br />If podSelector is also set, then the NetworkQoS as a whole selects<br />the pods matching podSelector in the namespaces selected by namespaceSelector.<br />Otherwise it selects all pods in the namespaces selected by namespaceSelector. |  | Optional: \{\} <br /> |
| `ipBlock` _[IPBlock](#ipblock)_ | ipBlock defines policy on a particular IPBlock. If this field is set then<br />neither of the other fields can be. |  | Optional: \{\} <br /> |


#### IPBlock



IPBlock selects destination addresses by CIDR, with optional exclusions.



_Appears in:_
- [Destination](#destination)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cidr` _[CIDR](#cidr)_ | cidr is a CIDR, for example "192.168.1.0/24" or "2001:db8::/64", or a<br />single IP address. |  | MaxLength: 49 <br />Required: \{\} <br /> |
| `except` _[CIDR](#cidr) array_ | except lists CIDRs or single IP addresses that are excluded from cidr,<br />for example "192.168.1.0/28" or "2001:db8::/96". |  | MaxItems: 25 <br />MaxLength: 49 <br />Optional: \{\} <br /> |


#### NetworkQoS



NetworkQoS is a CRD that allows the user to define a DSCP marking and metering
for pods ingress/egress traffic on its namespace to specified CIDRs,
protocol and port. Traffic belong these pods will be checked against
each Rule in the namespace's NetworkQoS, and if there is a match the traffic
is marked with relevant DSCP value and enforcing specified policing
parameters.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1alpha1` | | |
| `kind` _string_ | `NetworkQoS` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[Spec](#spec)_ |  |  |  |
| `status` _[Status](#status)_ |  |  |  |


#### Port



Port specifies destination protocol and port on which NetworkQoS
rule is applied



_Appears in:_
- [Classifier](#classifier)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `protocol` _string_ | protocol (TCP, UDP, SCTP) that the traffic must match. |  | Enum: [TCP UDP SCTP] <br />Optional: \{\} <br /> |
| `port` _integer_ | port that the traffic must match |  | Maximum: 65535 <br />Minimum: 1 <br />Optional: \{\} <br /> |


#### Rule







_Appears in:_
- [Spec](#spec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `dscp` _integer_ | dscp marking value for matching pods' traffic. |  | Maximum: 63 <br />Minimum: 0 <br /> |
| `classifier` _[Classifier](#classifier)_ | classifier The classifier on which packets should match<br />to apply the NetworkQoS Rule.<br />This field is optional, and in case it is not set the rule is applied<br />to all egress traffic regardless of the destination. |  | Optional: \{\} <br /> |
| `bandwidth` _[Bandwidth](#bandwidth)_ |  |  | Optional: \{\} <br /> |


#### Spec



Spec defines the desired state of NetworkQoS



_Appears in:_
- [NetworkQoS](#networkqos)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `networkSelectors` _[NetworkSelectors](#networkselectors)_ | networkSelector selects the networks on which the pod IPs need to be added to the source address set.<br />NetworkQoS controller currently supports `NetworkAttachmentDefinitions` type only. |  | Optional: \{\} <br /> |
| `podSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#labelselector-v1-meta)_ | podSelector applies the NetworkQoS rule only to the pods in the namespace whose label<br />matches this definition. This field is optional, and in case it is not set<br />results in the rule being applied to all pods in the namespace. |  | Optional: \{\} <br /> |
| `priority` _integer_ | priority is a value from 0 to 100 and represents the NetworkQoS' priority.<br />QoSes with numerically higher priority takes precedence over those with lower. |  | Maximum: 100 <br />Minimum: 0 <br /> |
| `egress` _[Rule](#rule) array_ | egress a collection of Egress NetworkQoS rule objects. A total of 20 rules will<br />be allowed in each NetworkQoS instance. The relative precedence of egress rules<br />within a single NetworkQos object (all of which share the priority) will be<br />determined by the order in which the rule is written. Thus, a rule that appears<br />first in the list of egress rules would take the lower precedence. |  | MaxItems: 20 <br /> |


#### Status



Status defines the observed state of NetworkQoS



_Appears in:_
- [NetworkQoS](#networkqos)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `status` _string_ | A concise indication of whether the NetworkQoS resource is applied with success. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#condition-v1-meta) array_ | An array of condition objects indicating details about status of NetworkQoS object. |  | Optional: \{\} <br /> |


