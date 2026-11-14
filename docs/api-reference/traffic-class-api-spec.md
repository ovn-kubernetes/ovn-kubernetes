# API Reference

## Packages
- [k8s.ovn.org/v1](#k8sovnorgv1)


## k8s.ovn.org/v1

Package v1 contains API Schema definitions for the network v1 API group

### Resource Types
- [TrafficClass](#trafficclass)
- [TrafficClassList](#trafficclasslist)



#### Port



Port selects exactly one port number or inclusive range.



_Appears in:_
- [PortProtocol](#portprotocol)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `number` _integer_ | Number selects one port. |  | Maximum: 65535 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `range` _[PortRange](#portrange)_ | Range selects an inclusive range of ports. |  | Optional: \{\} <br /> |


#### PortProtocol



PortProtocol optionally restricts both destination and source ports (AND semantics).



_Appears in:_
- [ProtocolMatch](#protocolmatch)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `destinationPort` _[Port](#port)_ | DestinationPort matches the packet destination port. |  | Optional: \{\} <br /> |
| `sourcePort` _[Port](#port)_ | SourcePort matches the packet source port. |  | Optional: \{\} <br /> |


#### PortRange



PortRange is an inclusive range of transport ports.



_Appears in:_
- [Port](#port)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `start` _integer_ | Start is the first port. |  | Maximum: 65535 <br />Minimum: 1 <br /> |
| `end` _integer_ | End is the last port. |  | Maximum: 65535 <br />Minimum: 1 <br /> |


#### ProtocolMatch



ProtocolMatch selects exactly one transport protocol.



_Appears in:_
- [TrafficMatch](#trafficmatch)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `tcp` _[PortProtocol](#portprotocol)_ | TCP selects TCP traffic. |  | Optional: \{\} <br /> |
| `udp` _[PortProtocol](#portprotocol)_ | UDP selects UDP traffic. |  | Optional: \{\} <br /> |
| `sctp` _[PortProtocol](#portprotocol)_ | SCTP selects SCTP traffic. |  | Optional: \{\} <br /> |


#### TrafficClass



TrafficClass defines matches selectable by EgressIP resources.



_Appears in:_
- [TrafficClassList](#trafficclasslist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `TrafficClass` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[TrafficClassSpec](#trafficclassspec)_ |  |  |  |


#### TrafficClassList



TrafficClassList is a list of TrafficClasses.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `TrafficClassList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[TrafficClass](#trafficclass) array_ |  |  |  |


#### TrafficClassSpec



TrafficClassSpec defines destination matches.



_Appears in:_
- [TrafficClass](#trafficclass)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `matches` _[TrafficMatch](#trafficmatch) array_ | Matches are ORed together. |  | MaxItems: 25 <br />MinItems: 1 <br /> |


#### TrafficMatch



TrafficMatch matches a network, optionally restricted to L4 protocols and ports.



_Appears in:_
- [TrafficClassSpec](#trafficclassspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `destinationCIDR` _string_ | DestinationCIDR must use the canonical network address. |  | MaxLength: 43 <br /> |
| `protocols` _[ProtocolMatch](#protocolmatch) array_ | Protocols are ORed together; omission matches all protocols and ports. |  | Optional: \{\} <br /> |
