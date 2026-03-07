# API Reference

## Packages
- [k8s.ovn.org/v1](#k8sovnorgv1)


## k8s.ovn.org/v1

Package v1 contains API Schema definitions for the ClusterNetworkConnect v1 API group

### Resource Types
- [ClusterNetworkConnect](#clusternetworkconnect)
- [ClusterNetworkConnectList](#clusternetworkconnectlist)



#### CIDR

_Underlying type:_ _string_



_Validation:_
- MaxLength: 43

_Appears in:_
- [ConnectSubnet](#connectsubnet)



#### ClusterNetworkConnect



ClusterNetworkConnect enables connecting multiple User Defined Networks
and/or Cluster User Defined Networks together.



_Appears in:_
- [ClusterNetworkConnectList](#clusternetworkconnectlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `ClusterNetworkConnect` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ClusterNetworkConnectSpec](#clusternetworkconnectspec)_ |  |  | Required: \{\} <br /> |
| `status` _[ClusterNetworkConnectStatus](#clusternetworkconnectstatus)_ |  |  |  |


#### ClusterNetworkConnectList



ClusterNetworkConnectList contains a list of ClusterNetworkConnect.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `ClusterNetworkConnectList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[ClusterNetworkConnect](#clusternetworkconnect) array_ |  |  |  |


#### ClusterNetworkConnectSpec



ClusterNetworkConnectSpec defines the desired state of ClusterNetworkConnect.



_Appears in:_
- [ClusterNetworkConnect](#clusternetworkconnect)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `networkSelectors` _[NetworkSelectors](#networkselectors)_ | networkSelectors selects the networks to be connected together.<br />This can match User Defined Networks (UDNs) and/or Cluster User Defined Networks (CUDNs).<br />Only ClusterUserDefinedNetworkSelector and PrimaryUserDefinedNetworkSelector can be selected. |  | Required: \{\} <br /> |
| `connectSubnets` _[ConnectSubnet](#connectsubnet) array_ | connectSubnets specifies the subnets used for interconnecting the selected networks.<br />This creates a shared subnet space that connected networks can use to communicate.<br />Can have at most 1 CIDR for each IP family (IPv4 and IPv6).<br />Must not overlap with:<br /> any of the pod subnets used by the selected networks.<br /> any of the transit subnets used by the selected networks.<br /> any of the service CIDR range used in the cluster.<br /> any of the join subnet of the selected networks to be connected.<br /> any of the masquerade subnet range used in the cluster.<br /> any of the node subnets chosen by the platform.<br /> any of other connect subnets for other ClusterNetworkConnects that might be selecting same networks.<br />Does not have a default value for the above reason so<br />that user takes care in setting non-overlapping subnets. |  | MaxItems: 2 <br />MinItems: 1 <br />Required: \{\} <br /> |
| `connectivity` _[ConnectivityType](#connectivitytype) array_ | connectivity specifies which connectivity types should be enabled for the connected networks. |  | Enum: [PodNetwork ServiceNetwork] <br />MaxItems: 2 <br />MinItems: 1 <br />Required: \{\} <br /> |


#### ClusterNetworkConnectStatus



ClusterNetworkConnectStatus defines the observed state of ClusterNetworkConnect.



_Appears in:_
- [ClusterNetworkConnect](#clusternetworkconnect)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `status` _[StatusType](#statustype)_ | status is a concise indication of whether the ClusterNetworkConnect<br />resource is applied with success. |  | Enum: [Success Failure] <br />Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#condition-v1-meta) array_ | conditions is an array of condition objects indicating details about<br />status of ClusterNetworkConnect object. |  |  |


#### ConnectSubnet







_Appears in:_
- [ClusterNetworkConnectSpec](#clusternetworkconnectspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cidr` _[CIDR](#cidr)_ | CIDR specifies ConnectSubnet, which is split into smaller subnets for every connected network.<br />This CIDR should be containing 2*((Number of L3 networks*Max Number of Nodes)+Number of L2 networks) IPs.<br />Example: cidr= "192.168.0.0/16", networkPrefix=24 and if the cluster has 128 nodes that means that you can<br />connect 256 layer3 networks and 0 layer2 networks OR 255 layer3 networks and 128 layer2 networks.<br />CIDR also restricts the maximum number of networks that can be connected together<br />based on what CIDR range is picked. So choosing a large enough CIDR for future use cases<br />is important.<br />The largest CIDR that can be used for this field is /16 (65536 IPs) because OVN<br />has a limit of 32K(2^15) tunnel keys per router. So we will only ever have 32K /31 or /127 slices<br />which is 2^16 IPs.<br />Having a CIDR greater than /16 will not be utilized fully for the same reason. |  | MaxLength: 43 <br /> |
| `networkPrefix` _integer_ | NetworkPrefix specifies the prefix length for every connected network.<br />This prefix length should be equal to or longer than the length of the CIDR prefix.<br />For example, if the CIDR is 10.0.0.0/16 and the networkPrefix is 24,<br />then the connect subnet for each connected layer3 network will be 10.0.0.0/24, 10.0.1.0/24, 10.0.2.0/24 etc.<br />For layer2 networks we will allocate the next available /networkPrefix range<br />that is then split into /31 or /127 slices for each layer2 network<br />A good practice is to set this to a value that ensures it contains more<br />than twice the number of maximum nodes planned to be deployed in the cluster.<br />Each node gets a /31 subnet for the layer3 networks, hence networkPrefix should<br />contain enough IPs for 4 times the maximum nodes planned<br />Example - recommended values:<br />if you plan to deploy 10 nodes, set the networkPrefix to /26 (40+ IPs)<br />if you plan to deploy 100 nodes, set the networkPrefix to /23 (400+ IPs)<br />if you plan to deploy 1000 nodes, set the networkPrefix to /20 (4000+ IPs)<br />if you plan to deploy 5000 nodes, set the networkPrefix to /17 (20000+ IPs)<br />This field restricts the maximum number of nodes that can be deployed in the cluster<br />and hence its good to plan this value carefully along with the CIDR. |  | Maximum: 127 <br />Minimum: 1 <br /> |


#### ConnectivityType

_Underlying type:_ _string_

ConnectivityType represents the different connectivity types that can be enabled for connected networks.

_Validation:_
- Enum: [PodNetwork ServiceNetwork]

_Appears in:_
- [ClusterNetworkConnectSpec](#clusternetworkconnectspec)

| Field | Description |
| --- | --- |
| `PodNetwork` | PodNetwork enables direct pod-to-pod communication across connected networks.<br /> |
| `ServiceNetwork` | ServiceNetwork enables ClusterIP service access across connected networks.<br />Note that services of type nodeports and loadbalancers are already reachable<br />across networks by default.<br /> |


#### StatusType

_Underlying type:_ _string_

StatusType represents the status of a ClusterNetworkConnect.

_Validation:_
- Enum: [Success Failure]

_Appears in:_
- [ClusterNetworkConnectStatus](#clusternetworkconnectstatus)

| Field | Description |
| --- | --- |
| `Success` | Success indicates that the ClusterNetworkConnect has been successfully applied.<br /> |
| `Failure` | Failure indicates that the ClusterNetworkConnect has failed to be applied.<br /> |


