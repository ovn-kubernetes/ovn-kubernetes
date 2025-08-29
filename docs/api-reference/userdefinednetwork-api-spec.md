# API Reference

## Packages
- [k8s.ovn.org/v1](#k8sovnorgv1)


## k8s.ovn.org/v1

Package v1 contains API Schema definitions for the network v1 API group

### Resource Types
- [ClusterUserDefinedNetwork](#clusteruserdefinednetwork)
- [ClusterUserDefinedNetworkList](#clusteruserdefinednetworklist)
- [UserDefinedNetwork](#userdefinednetwork)
- [UserDefinedNetworkList](#userdefinednetworklist)



#### AccessVLANConfig



AccessVLANConfig describes an access VLAN configuration.



_Appears in:_
- [VLANConfig](#vlanconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `id` _integer_ | id is the VLAN ID (VID) to be set for the network.<br />id should be higher than 0 and lower than 4095. |  | Maximum: 4094 <br />Minimum: 1 <br /> |


#### CIDR

_Underlying type:_ _string_



_Validation:_
- MaxLength: 43

_Appears in:_
- [DualStackCIDRs](#dualstackcidrs)
- [Layer3Subnet](#layer3subnet)
- [LocalnetConfig](#localnetconfig)



#### ClusterUserDefinedNetwork



ClusterUserDefinedNetwork describe network request for a shared network across namespaces.



_Appears in:_
- [ClusterUserDefinedNetworkList](#clusteruserdefinednetworklist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `ClusterUserDefinedNetwork` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ClusterUserDefinedNetworkSpec](#clusteruserdefinednetworkspec)_ |  |  | Required: \{\} <br /> |
| `status` _[ClusterUserDefinedNetworkStatus](#clusteruserdefinednetworkstatus)_ |  |  |  |


#### ClusterUserDefinedNetworkList



ClusterUserDefinedNetworkList contains a list of ClusterUserDefinedNetwork.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `ClusterUserDefinedNetworkList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[ClusterUserDefinedNetwork](#clusteruserdefinednetwork) array_ |  |  |  |


#### ClusterUserDefinedNetworkSpec



ClusterUserDefinedNetworkSpec defines the desired state of ClusterUserDefinedNetwork.



_Appears in:_
- [ClusterUserDefinedNetwork](#clusteruserdefinednetwork)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `namespaceSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#labelselector-v1-meta)_ | NamespaceSelector Label selector for which namespace network should be available for. |  | Required: \{\} <br /> |
| `network` _[NetworkSpec](#networkspec)_ | Network is the user-defined-network spec |  | Required: \{\} <br /> |


#### ClusterUserDefinedNetworkStatus



ClusterUserDefinedNetworkStatus contains the observed status of the ClusterUserDefinedNetwork.



_Appears in:_
- [ClusterUserDefinedNetwork](#clusteruserdefinednetwork)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#condition-v1-meta) array_ | Conditions slice of condition objects indicating details about ClusterUserDefineNetwork status. |  |  |


#### DualStackCIDRs

_Underlying type:_ _[CIDR](#cidr)_



_Validation:_
- MaxItems: 2
- MaxLength: 43
- MinItems: 1

_Appears in:_
- [Layer2Config](#layer2config)
- [Layer3Config](#layer3config)
- [LocalnetConfig](#localnetconfig)



#### IPAMConfig





_Validation:_
- MinProperties: 1

_Appears in:_
- [Layer2Config](#layer2config)
- [LocalnetConfig](#localnetconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `mode` _[IPAMMode](#ipammode)_ | Mode controls how much of the IP configuration will be managed by OVN.<br />`Enabled` means OVN-Kubernetes will apply IP configuration to the SDN infrastructure and it will also assign IPs<br />from the selected subnet to the individual pods.<br />`Disabled` means OVN-Kubernetes will only assign MAC addresses and provide layer 2 communication, letting users<br />configure IP addresses for the pods.<br />`Disabled` is only available for Secondary networks.<br />By disabling IPAM, any Kubernetes features that rely on selecting pods by IP will no longer function<br />(such as network policy, services, etc). Additionally, IP port security will also be disabled for interfaces attached to this network.<br />Defaults to `Enabled`. |  | Enum: [Enabled Disabled] <br /> |
| `lifecycle` _[NetworkIPAMLifecycle](#networkipamlifecycle)_ | Lifecycle controls IP addresses management lifecycle.<br /><br />The only allowed value is Persistent. When set, the IP addresses assigned by OVN-Kubernetes will be persisted in an<br />`ipamclaims.k8s.cni.cncf.io` object. These IP addresses will be reused by other pods if requested.<br />Only supported when mode is `Enabled`. |  | Enum: [Persistent] <br /> |


#### IPAMMode

_Underlying type:_ _string_



_Validation:_
- Enum: [Enabled Disabled]

_Appears in:_
- [IPAMConfig](#ipamconfig)

| Field | Description |
| --- | --- |
| `Enabled` |  |
| `Disabled` |  |


#### Layer2Config







_Appears in:_
- [NetworkSpec](#networkspec)
- [UserDefinedNetworkSpec](#userdefinednetworkspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `role` _[NetworkRole](#networkrole)_ | Role describes the network role in the pod.<br /><br />Allowed value is "Secondary".<br />Secondary network is only assigned to pods that use `k8s.v1.cni.cncf.io/networks` annotation to select given network. |  | Enum: [Primary Secondary] <br />Required: \{\} <br /> |
| `mtu` _integer_ | MTU is the maximum transmission unit for a network.<br />MTU is optional, if not provided, the globally configured value in OVN-Kubernetes (defaults to 1400) is used for the network. |  | Maximum: 65536 <br />Minimum: 576 <br /> |
| `subnets` _[DualStackCIDRs](#dualstackcidrs)_ | Subnets are used for the pod network across the cluster.<br />Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.<br /><br />The format should match standard CIDR notation (for example, "10.128.0.0/16").<br />This field must be omitted if `ipam.mode` is `Disabled`. |  | MaxItems: 2 <br />MaxLength: 43 <br />MinItems: 1 <br /> |
| `joinSubnets` _[DualStackCIDRs](#dualstackcidrs)_ | JoinSubnets are used inside the OVN network topology.<br /><br />Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.<br />This field is only allowed for "Primary" network.<br />It is not recommended to set this field without explicit need and understanding of the OVN network topology.<br />When omitted, the platform will choose a reasonable default which is subject to change over time. |  | MaxItems: 2 <br />MaxLength: 43 <br />MinItems: 1 <br /> |
| `ipam` _[IPAMConfig](#ipamconfig)_ | IPAM section contains IPAM-related configuration for the network. |  | MinProperties: 1 <br /> |


#### Layer3Config







_Appears in:_
- [NetworkSpec](#networkspec)
- [UserDefinedNetworkSpec](#userdefinednetworkspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `role` _[NetworkRole](#networkrole)_ | Role describes the network role in the pod.<br /><br />Allowed values are "Primary" and "Secondary".<br />Primary network is automatically assigned to every pod created in the same namespace.<br />Secondary network is only assigned to pods that use `k8s.v1.cni.cncf.io/networks` annotation to select given network. |  | Enum: [Primary Secondary] <br />Required: \{\} <br /> |
| `mtu` _integer_ | MTU is the maximum transmission unit for a network.<br /><br />MTU is optional, if not provided, the globally configured value in OVN-Kubernetes (defaults to 1400) is used for the network. |  | Maximum: 65536 <br />Minimum: 576 <br /> |
| `subnets` _[Layer3Subnet](#layer3subnet) array_ | Subnets are used for the pod network across the cluster.<br /><br />Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.<br />Given subnet is split into smaller subnets for every node. |  | MaxItems: 2 <br />MinItems: 1 <br /> |
| `joinSubnets` _[DualStackCIDRs](#dualstackcidrs)_ | JoinSubnets are used inside the OVN network topology.<br /><br />Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.<br />This field is only allowed for "Primary" network.<br />It is not recommended to set this field without explicit need and understanding of the OVN network topology.<br />When omitted, the platform will choose a reasonable default which is subject to change over time. |  | MaxItems: 2 <br />MaxLength: 43 <br />MinItems: 1 <br /> |


#### Layer3Subnet







_Appears in:_
- [Layer3Config](#layer3config)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cidr` _[CIDR](#cidr)_ | CIDR specifies L3Subnet, which is split into smaller subnets for every node. |  | MaxLength: 43 <br /> |
| `hostSubnet` _integer_ | HostSubnet specifies the subnet size for every node.<br /><br />When not set, it will be assigned automatically. |  | Maximum: 127 <br />Minimum: 1 <br /> |


#### LocalnetConfig







_Appears in:_
- [NetworkSpec](#networkspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `role` _[NetworkRole](#networkrole)_ | role describes the network role in the pod, required.<br />Controls whether the pod interface will act as primary or secondary.<br />Localnet topology supports `Secondary` only.<br />The network will be assigned to pods that have the `k8s.v1.cni.cncf.io/networks` annotation in place pointing<br />to subject. |  | Enum: [Secondary] <br /> |
| `physicalNetworkName` _string_ | physicalNetworkName points to the OVS bridge-mapping's network-name configured in the nodes, required.<br />Min length is 1, max length is 253, cannot contain `,` or `:` characters.<br />In case OVS bridge-mapping is defined by Kubernetes-nmstate with `NodeNetworkConfigurationPolicy` (NNCP),<br />this field should point to the NNCP `spec.desiredState.ovn.bridge-mappings` item's `localnet` value. |  | MaxLength: 253 <br />MinLength: 1 <br /> |
| `subnets` _[DualStackCIDRs](#dualstackcidrs)_ | subnets is a list of subnets used for pods in this localnet network across the cluster.<br />The list may be either 1 IPv4 subnet, 1 IPv6 subnet, or 1 of each IP family.<br />When set, OVN-Kubernetes assigns an IP address from the specified CIDRs to the connected pod,<br />eliminating the need for manual IP assignment or reliance on an external IPAM service (e.g., a DHCP server).<br />subnets is optional. When omitted OVN-Kubernetes won't assign IP address automatically.<br />Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.<br />The format should match standard CIDR notation (for example, "10.128.0.0/16").<br />This field must be omitted if `ipam.mode` is `Disabled`.<br />When physicalNetworkName points to the OVS bridge mapping of a network that provides IPAM services<br />(e.g., a DHCP server), ipam.mode should be set to Disabled. This turns off OVN-Kubernetes IPAM and avoids<br />conflicts with the existing IPAM services on this localnet network. |  | MaxItems: 2 <br />MaxLength: 43 <br />MinItems: 1 <br /> |
| `excludeSubnets` _[CIDR](#cidr) array_ | excludeSubnets is a list of CIDRs to be removed from the specified CIDRs in `subnets`.<br />The CIDRs in this list must be in range of at least one subnet specified in `subnets`.<br />excludeSubnets is optional. When omitted no IP address is excluded and all IP addresses specified in `subnets`<br />are subject to assignment.<br />The format should match standard CIDR notation (for example, "10.128.0.0/16").<br />This field must be omitted if `subnets` is unset or `ipam.mode` is `Disabled`.<br />When `physicalNetworkName` points to OVS bridge mapping of a network with reserved IP addresses<br />(which shouldn't be assigned by OVN-Kubernetes), the specified CIDRs will not be assigned. For example:<br />Given: `subnets: "10.0.0.0/24"`, `excludeSubnets: "10.0.0.200/30", the following addresses will not be assigned<br />to pods: `10.0.0.201`, `10.0.0.202`. |  | MaxItems: 25 <br />MaxLength: 43 <br />MinItems: 1 <br /> |
| `ipam` _[IPAMConfig](#ipamconfig)_ | ipam configurations for the network.<br />ipam is optional. When omitted, `subnets` must be specified.<br />When `ipam.mode` is `Disabled`, `subnets` must be omitted.<br />`ipam.mode` controls how much of the IP configuration will be managed by OVN.<br />   When `Enabled`, OVN-Kubernetes will apply IP configuration to the SDN infra and assign IPs from the selected<br />   subnet to the pods.<br />   When `Disabled`, OVN-Kubernetes only assigns MAC addresses, and provides layer2 communication, and enables users<br />   to configure IP addresses on the pods.<br />`ipam.lifecycle` controls IP addresses management lifecycle.<br />   When set to 'Persistent', the assigned IP addresses will be persisted in `ipamclaims.k8s.cni.cncf.io` object.<br />	  Useful for VMs, IP address will be persistent after restarts and migrations. Supported when `ipam.mode` is `Enabled`. |  | MinProperties: 1 <br /> |
| `mtu` _integer_ | mtu is the maximum transmission unit for a network.<br />mtu is optional. When omitted, the configured value in OVN-Kubernetes (defaults to 1500 for localnet topology)<br />is used for the network.<br />Minimum value for IPv4 subnet is 576, and for IPv6 subnet is 1280.<br />Maximum value is 65536.<br />In a scenario `physicalNetworkName` points to OVS bridge mapping of a network configured with certain MTU settings,<br />this field enables configuring the same MTU on pod interface, having the pod MTU aligned with the network MTU.<br />Misaligned MTU across the stack (e.g.: pod has MTU X, node NIC has MTU Y), could result in network disruptions<br />and bad performance. |  | Maximum: 65536 <br />Minimum: 576 <br /> |
| `vlan` _[VLANConfig](#vlanconfig)_ | vlan configuration for the network.<br />vlan.mode is the VLAN mode.<br />  When "Access" is set, OVN-Kubernetes configures the network logical switch port in access mode.<br />vlan.access is the access VLAN configuration.<br />vlan.access.id is the VLAN ID (VID) to be set on the network logical switch port.<br />vlan is optional, when omitted the underlying network default VLAN will be used (usually `1`).<br />When set, OVN-Kubernetes will apply VLAN configuration to the SDN infra and to the connected pods. |  |  |


#### NetworkIPAMLifecycle

_Underlying type:_ _string_



_Validation:_
- Enum: [Persistent]

_Appears in:_
- [IPAMConfig](#ipamconfig)

| Field | Description |
| --- | --- |
| `Persistent` |  |


#### NetworkRole

_Underlying type:_ _string_





_Appears in:_
- [Layer2Config](#layer2config)
- [Layer3Config](#layer3config)
- [LocalnetConfig](#localnetconfig)

| Field | Description |
| --- | --- |
| `Primary` |  |
| `Secondary` |  |


#### NetworkSpec



NetworkSpec defines the desired state of UserDefinedNetworkSpec.



_Appears in:_
- [ClusterUserDefinedNetworkSpec](#clusteruserdefinednetworkspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `topology` _[NetworkTopology](#networktopology)_ | Topology describes network configuration.<br /><br />Allowed values are "Layer3", "Layer2" and "Localnet".<br />Layer3 topology creates a layer 2 segment per node, each with a different subnet. Layer 3 routing is used to interconnect node subnets.<br />Layer2 topology creates one logical switch shared by all nodes.<br />Localnet topology is based on layer 2 topology, but also allows connecting to an existent (configured) physical network to provide north-south traffic to the workloads. |  | Enum: [Layer2 Layer3 Localnet] <br />Required: \{\} <br /> |
| `layer3` _[Layer3Config](#layer3config)_ | Layer3 is the Layer3 topology configuration. |  |  |
| `layer2` _[Layer2Config](#layer2config)_ | Layer2 is the Layer2 topology configuration. |  |  |
| `localnet` _[LocalnetConfig](#localnetconfig)_ | Localnet is the Localnet topology configuration. |  |  |


#### NetworkTopology

_Underlying type:_ _string_





_Appears in:_
- [NetworkSpec](#networkspec)
- [UserDefinedNetworkSpec](#userdefinednetworkspec)

| Field | Description |
| --- | --- |
| `Localnet` |  |
| `Layer2` |  |
| `Layer3` |  |


#### UserDefinedNetwork



UserDefinedNetwork describe network request for a Namespace.



_Appears in:_
- [UserDefinedNetworkList](#userdefinednetworklist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `UserDefinedNetwork` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[UserDefinedNetworkSpec](#userdefinednetworkspec)_ |  |  | Required: \{\} <br /> |
| `status` _[UserDefinedNetworkStatus](#userdefinednetworkstatus)_ |  |  |  |


#### UserDefinedNetworkList



UserDefinedNetworkList contains a list of UserDefinedNetwork.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `UserDefinedNetworkList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[UserDefinedNetwork](#userdefinednetwork) array_ |  |  |  |


#### UserDefinedNetworkSpec



UserDefinedNetworkSpec defines the desired state of UserDefinedNetworkSpec.



_Appears in:_
- [UserDefinedNetwork](#userdefinednetwork)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `topology` _[NetworkTopology](#networktopology)_ | Topology describes network configuration.<br /><br />Allowed values are "Layer3", "Layer2".<br />Layer3 topology creates a layer 2 segment per node, each with a different subnet. Layer 3 routing is used to interconnect node subnets.<br />Layer2 topology creates one logical switch shared by all nodes. |  | Enum: [Layer2 Layer3] <br />Required: \{\} <br /> |
| `layer3` _[Layer3Config](#layer3config)_ | Layer3 is the Layer3 topology configuration. |  |  |
| `layer2` _[Layer2Config](#layer2config)_ | Layer2 is the Layer2 topology configuration. |  |  |


#### UserDefinedNetworkStatus



UserDefinedNetworkStatus contains the observed status of the UserDefinedNetwork.



_Appears in:_
- [UserDefinedNetwork](#userdefinednetwork)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#condition-v1-meta) array_ |  |  |  |


#### VLANConfig



VLANConfig describes the network VLAN configuration.



_Appears in:_
- [LocalnetConfig](#localnetconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `mode` _[VLANMode](#vlanmode)_ | mode describe the network VLAN mode.<br />Allowed value is "Access".<br />Access sets the network logical switch port in access mode, according to the config. |  | Enum: [Access] <br /> |
| `access` _[AccessVLANConfig](#accessvlanconfig)_ | Access is the access VLAN configuration |  |  |


#### VLANMode

_Underlying type:_ _string_



_Validation:_
- Enum: [Access]

_Appears in:_
- [VLANConfig](#vlanconfig)

| Field | Description |
| --- | --- |
| `Access` |  |


