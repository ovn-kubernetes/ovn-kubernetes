# OKEP-5085: Localnet API

## Problem Statement

As of today one can create a user-defined network over localnet topology using NetworkAttachmentDefinition (NAD).
Using NAD for localnet has some pitfalls due to the fact it is not managed and not validated on creation.
Misconfigurations are detected too late causing bad UX and frustration for users.

Configuring localnet topology networks requires changes to cluster nodes network stack and involves some risk and
knowledge that require cluster-admin intervention, such as: configuring the OVS switch to which the localnet network connects, 
aligning MTU across the stack and configuring the right VLANs that fit the provider network.

## Goals

- Enable creating user-defined-networks over localnet topology using OVN-K CUDN CRD.
- Streamline localnet UX: detect misconfigurations early, provide indications about issues or success.

## Non-Goals

## Introduction

As of today OVN-Kubernetes [multi-homing feature](../../docs/features/multiple-networks/multi-homing.md) 
supports creating localnet topology networks and enables connecting workloads to the host network using `NetworkAttachmentDefinition` (NAD).

This proposal introduces a well-formed API on top of the `ClusterUserDefinedNetwork` CRD.

Managing localnet topology networks using a well-formed API could improve UX as it is managed by a controller, 
perform validations and reflect the state via status.

## User-Stories/Use-Cases

#### Definition of personas:
Admin - is the cluster admin.
User - non cluster-admin user, project manager.
Workloads - pod or [KubeVirt](https://kubevirt.io/) VMs.

- As an admin I want to create a user-defined network over localnet topology using CUDN CRD.
    - In case the network configuration is bad I want to get an informative message saying what went wrong.
- As an admin I want to enable users to connect workloads in project/namespaces they have permission to, to the localnet network I created for them.
- As a user I want to be able to connect my workloads (pod/VMs) to the localnet the admin created in my namespace.
- As a user I want my workloads to be able to communicate with each other over the localnet network.
- As a user I want my VMs connected to the localnet network to be able to migrate from one node to another, without any changes on the IP address of their localnet network interface.
## Proposed Solution

### Summary
Extend the CUDN CRD to enable creating user-defined networks over localnet topology.
Since the CUDN CRD is targeted for cluster-admin users, it prevents non-admin users from performing changes that
could disrupt the cluster or impact the physical network to which the workloads would connect.

#### Localnet using `NetworkAttachmentDefinition`
As of today OVN-K enables multi-homing including localnet topology networks using NADs.
The following NAD YAML describes localnet topology configuration and options:

```yaml
---
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: tenantblue
  namespace: blue
spec:
    config: > '{
        "cniVersion": "0.3.1",
        "type": "ovn-k8s-cni-overlay"
        "netAttachDefName": "blue/tenantblue",
        "topology": "localnet",
        "name": "tenantblue",#1
        "physicalNetworkName": "mylocalnet1", #2
        "subnets": "10.100.0.0/24", #3
        "excludeSubnets": "10.100.50.0/32", #4
        "vlanID": "200", #5
        "mtu": "1500", #6
        "allowPersistentIPs": true #7
    }'
```
1. `name`
   The underlying network name:
    - Should match the node OVS bridge-mapping network-name.
    - In case Kubernetes-nmstate is used, should match the `NodeNetworkConfigurationPolicy` (NNCP) `spec.desiredState.ovn.bridge-mappings` item's `localnet` attribute value.
2. `physicalNetworkName`
   Points to the node OVS bridge-mapping network-name - the network-name mapped to the node OVS bridge that provides access to that network.
   (Can be defined using Kubernetes-nmstate NNCP - `spec.desiredState.ovn.bridge-mappings`)
    - Overrides the `name` attribute, defined in (1).
    - Allows multiple localnet topology NADs to refer to the same bridge-mapping (thus simplifying the admin’s life -
      fewer manifests to provision and keep synced).
3. `subnets`
   Subnets to use for the network across the cluster.
4. `excludeSubnets`
   IP addresses ranges to exclude from the assignable IP address pool specified by the `subnets` field.
5. `vlanID` - VLAN tag assigned to traffic.
6. `mtu` - maximum transmission unit for a network
7. `allowPersistentIPs`
   persist the OVN-Kubernetes assigned IP addresses in a `ipamclaims.k8s.cni.cncf.io` object. These IP addresses will be
   reused by other pods if requested. Useful for [KubeVirt](https://kubevirt.io/) VMs.

#### Extend ClusterUserDefinedNetwork CRD
Given the CUDN CRD is targeted at cluster-admin users, it is a good fit for operations that require cluster-admin intervention,
such as localnet topology.

The suggested solution is to extend the CUDN CRD to enable the creating of localnet topology networks.

##### Underlying network name
The underlying network-name represented by the network-config name attribute, the NAD `spec.config.name` (net-conf-network-name).

Given the CUDN API doesn’t expose the net-conf-network-name by design, the localnet topology configuration require the net-conf-network-name 
to match an existing OVS bridge-mapping on the node. 

In case [Kubernetes-nmstate](https://nmstate.io/kubernetes-nmstate/) is used, the NAD `spec.config.name` has to match the `NodeNetworkConfigurationPolicy`
`spec.desiredState.ovn.bridge-mappings` name:
```yaml
spec:
  desiredState:
      ovn:
        bridge-mappings:
        - localnet: physnet  <---- has to match the NAD `config.spec.name` OR 
                                   the NAD `spec.config.physicalNetworkName`
          bridge: br-ex <--------- OVS switch
```
* To overcome this and to avoid exposing the net-conf-network-name in the CUDN CRD spec, a new field should be introduced.
  The new field allows users to point to the bridge-mapping network-name they defined in the node.
  The field should be translated to the CNI `physicalNetworkName` field.

#### Workflow Description

The CUDN CRD controller should be changed accordingly to support localnet topology.
It should validate localnet topology configuration and generate corresponding NADs for localnet as with other topologies (Layer2 & Layer3)
in the selected namespaces.

#### Generating the NAD
##### OVS bridge-mapping’s network-name
Introduces an attribute that points to the OVS bridge bridge-mapping network-name.
This attribute name should be translated to the CNI “physicalNetworkName” attribute.

Proposal for the CUDN spec field name:  “physicalNetworkName”.

##### MTU
Should be translated to the CNI “mtu” attribute.

By default, OVN-K sets the MTU of the UDN to be 100 bytes less than the physical MTU of the underlay network.
For the localnet topology this is not optimal because localnet does not use a Geneve overlay and is directly
connected to the underlay.
This results in a loss in throughput and potential MTU mismatch issues.

The MTU value may be set by the user, and if not set then OVN-Kubernetes will determine the default value to use -
1500 for localnet topology.

##### VLAN
Should be translated to the CNI “vlanID” attribute.
If not specified it should not be present in NAD spec.config.

##### Subnets, ExcludeSubnets
The subnets and exclude-subnets should be in CIDR form, similar to Layer2 topology subnets.

##### Persistent IPs
In a scenario of VMs, migrated VMs should have a persistent IP address to prevent workload disruption..
Localnet topology should allow using persistent IP allowing setting the CNI allowPersistentIPs.

As of today, the Layer2 topology configuration API consists of the following stanza, allowing to use persistent IPs,
and the localnet topology spec should have the same options:
```yaml
ipam:
  lifecycle: Persistent
```

By default, persistent IP is turned off.
Should be enabled by setting `ipam.lifecycle=Persistent`, similar to Layer2 topology.

### API Details

The ClusterUserDefinedNetwork CRD should be extended to support localnet topology.

####  CUDN spec

The CUDN `spec.network` follows the [discriminated union](https://github.com/openshift/enhancements/blob/master/dev-guide/api-conventions.md#discriminated-unions)
convention.
The `spec.network.topology` serves as the union discriminator, it should accept `Localnet` option.

The API should have validation that ensures `spec.network.topology` matches the topology configuration, similar to
existing validation for other topologies.

#### Localnet topology spec

| Field name          | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  | optional |
|---------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|
| Role                | Select the network role in the pod.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          | No       |
| PhysicalNetworkName | The OVN bridge mapping network name is configured on the node.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               | No       |
| MTU                 | The maximum transmission unit (MTU).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         | Yes      |
| VLAN                | Discriminated union for VLAN configurations for the network. <br>`mode`: When set to `Access`, OVN-Kubernetes applies the VLAN configuration to the network logical switch port in access mode, according to the config.<br>`access` is the access VLAN configuration.<br>`access.id` is the VLAN ID (VID) to be set on the logical network switch.                                                                                                                                                                                                                                                                          | Yes      |
| Subnets             | List of CIDRs used for the pod network across the cluster. Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed. The format should match standard CIDR notation (for example, "10.128.0.0/16"). This field must be omitted if `ipam.mode` is `Disabled`.                                                                                                                                                                                                                                                                                                                       | Yes      |
| ExcludeSubnets      | List of CIDRs removed from the specified CIDRs in `subnets`.The format should match standard CIDR notation (for example, "10.128.0.0/16"). This field must be omitted if `subnets` is unset or `ipam.mode` is `Disabled`.                                                                                                                                                                                                                                                                                                                                                                                                    | Yes      |
| IPAM                | Contains IPAM-related configuration, similar to Layer2 & Layer3 topologies.<br>Consists of the following fields:<br>`mode`:<br> When `Enabled`, OVN-Kubernetes will apply IP configuration to the SDN infra and assign IPs from the selected subnet to the pods.<br>When `Disabled`, OVN-Kubernetes only assign MAC addresses and provides layer2 communication, enable users configure IP addresses to the pods.<br>`lifecycle`:<br> When `Persistent` enable workloads have persistent IP addresses. For example: Virtual Machines will have the same IP addresses along their lifecycle (stop, start migration, reboots). |          |          |

#### Suggested API validations
- `Role`:
    - Required.
    - When `topology=Localnet`, the only allowed value is `Secondary`.
        - Having Role explicitly makes the API predictable and consistent with other topologies. In addition, it enables extending localnet to support future role options
- `PhysicalNetworkName`:
    -  Required.
    - Max length 253.
    - Cannot contain `,` or `:` characters.
- `MTU`:
    - Minimum 576 (minimal for IPv4). Maximum: 65536.
    - When Subnets consist of IPv6 CIDR, minimum MTU should be 1280.
- `VLAN`:
  - `Mode`: Allowed valued is "Access".
  - `ID` (`access.id`)
    According to [dot1q (IEEE 802.1Q)](https://ieeexplore.ieee.org/document/10004498),
    VID (VLAN ID) is 12-bits field, providing 4096 values; 0 - 4095. <br/>
    The VLAN IDs `0`, and `4095` are reserved. <br/>
    Suggested validations:
      - Minimum: 1, Maximum: 4094.
- `Subnets`:
    - Minimum items 1, Maximum items 2.
    - Items are valid CIDR (e.g.: "10.128.0.0/16")
    - When 2 items are specified they must be of different IP families.
- `ExcludeSubnets`:
    - Minimum items 1, Maximum items 25.
    - Items are valid CIDR (e.g.: "10.128.0.0/16")
    - Cannot be set when Subnet is unset or `ipam.mode=Disabled`.
    - Ensure excluded subnet in range of at least one subnet in `spec.network.localnet.subnets`.
      - Due to a bug in Kubernetes CEL validation IP/CIDR operations this validation can be implemented once the following issue is resolved
        https://github.com/kubernetes/kubernetes/issues/130441 
        The CRD controller should validate excludeSubnets items are in range of specified subnets. 
        In a case of an invalid request raise an error in the status.

#### YAML examples
Assuming the node has OVS bridge-mapping defined by [Kubernetes-nmstate](https://nmstate.io/kubernetes-nmstate/) 
using the following `NodeNetworkConfigurationPolicy` (NNCP):
```yaml
...
desiredState:
    ovn:
      bridge-mappings:
      - localnet: tenantblue 
        bridge: br-ex
```
Example 1:
```yaml
---
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: test-net
spec:
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values: ["red", "blue"]
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: tenantblue
      subnets: ["192.168.100.0/24", "2001:dbb::/64"]
      excludeSubnets: ["192.168.100.1/32", "2001:dbb::0/128"]
```
The above CR will make the controller create NAD in each selected namespace: "red" and "blue".
NAD in namespace `blue`:
```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: test-net
  namespace: blue
finalizers:
 - k8s.ovn.org/user-defined-network-protection
labels:
  k8s.ovn.org/user-defined-network: ""
ownerReferences:
- apiVersion: k8s.ovn.org/v1
  blockOwnerDeletion: true
  controller: true
  kind: ClusterUserDefinedNetwork
  name: test-net
  uid: 293098c2-0b7e-4216-a3c6-7f8362c7aa61
spec:
    config: > '{
        "cniVersion": "1.0.0",
        "type": "ovn-k8s-cni-overlay"
        "netAttachDefName": "blue/test-net",
        "role": "secondary",
        "topology": "localnet",
        "name": "cluster.udn.test-net",
        "physicalNetworkName: "tenantblue",
        "mtu": 1500,
        "subnets": "192.168.100.0/24,2001:dbb::/64",
        "excludeSubnets": "192.168.100.1/32,2001:dbb::0/128"
    }'
```

Example 2 (custom MTU, VLAN and sticky IPs):
```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: test-net
spec:
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values: ["red", "blue"]
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: tenantblue
      subnets: ["192.168.0.0/16", "2001:dbb::/64"]
      excludeSubnets: ["192.168.50.0/24"]
      mtu: 9000
      vlan:
        mode: Access
        access:
          id: 200
      ipam:
        lifecycle: Persistent
```
The above CR will make the controller create NAD in each selected namespace: "red" and "blue".
NAD in namespace `red`:
```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: test-net
  namespace: red
finalizers:
 - k8s.ovn.org/user-defined-network-protection
labels:
  k8s.ovn.org/user-defined-network: ""
ownerReferences:
- apiVersion: k8s.ovn.org/v1
  blockOwnerDeletion: true
  controller: true
  kind: ClusterUserDefinedNetwork
  name: test-net
spec:
    config: > '{
        "cniVersion": "1.0.0",
        "type": "ovn-k8s-cni-overlay"
        "netAttachDefName": "blue/test-net",
        "role": "secondary",
        "topology": "localnet",
        "name": "cluster.udn.test-net",
        "physicalNetworkName: "tenantblue",
        "subnets": "192.168.0.0/16,2001:dbb::/64",
        "allowPersistentIPs": true,
        "excludesubnets: "10.100.50.0/24",
        "mtu": 9000,
        "vlanID": 200
    }'
```

### Implementation Details

The CUDN `spec.network.topology` field should be extended to accept `Localnet` string.
And the `spec.network` struct `NetworkSpec` should have an additional field for localnet topology configuration:
```go
const NetworkTopologyLocalnet NetworkTopology = "Localnet"

...

// NetworkSpec defines the desired state of UserDefinedNetworkSpec.
// +union
type NetworkSpec struct {
    // Topology describes network configuration.
    //
    // Allowed values are "Layer3", "Layer2" and "Localnet".
    // Layer3 topology creates a layer 2 segment per node, each with a different subnet. Layer 3 routing is used to interconnect node subnets.
    // Layer2 topology creates one logical switch shared by all nodes.
    // Localnet topology attach to the nodes physical network. Enables egress to the provider's physical network. 
    //
    // +kubebuilder:validation:Required
    // +required
    // +unionDiscriminator
    Topology NetworkTopology `json:"topology"`
    ...
    // Localnet is the Localnet topology configuration.
    // +optional
    Localnet *LocalnetConfig `json:"localnet,omitempty"`
}
```

The CUDN spec should have additional validation rule for `spec.network.topology` field:
```go
// ClusterUserDefinedNetworkSpec defines the desired state of ClusterUserDefinedNetwork.
type ClusterUserDefinedNetworkSpec struct {
    ...
    // +required
    Network NetworkSpec `json:"network"`
}
```

#### Localnet topology configuration type
Introduce new topology configuration type for localnet - `LocalnetConfig`.
The CUDN CRD `spec.network` should feature proposed localnet topology configuration.

The Layer2 and Layer3 configuration types (`Layer2Config` & `Layer3Config`) `role` field is defined with `NetworkRole` type.
The `NetworkRole` type has the following enum validation, allowing `Secondary` and `Primary` values:
```
// +kubebuilder:validation:Enum=Primary;Secondary
```
The proposed localnet config type (`LocalnetConfig`) `role` field would have to accept `Secondary` value only.
In order to avoid misleading CRD scheme, the enum validation should be moved closer to each `NetworkRole` usage:
1. Remove the existing enum validation from `NetworkRole` definition
2. At the `Layer2Config` definition, add enum validation to `role` field allowing: `Primary` or `Secondary`.
3. At the `Layer3Config` definition, add enum validation to `role` field allowing: `Primary` or `Secondary`.
4. At the proposed `LocalnetConfig` definition, add enum validation to `role` field allowing `Secondary` only.

#### VLAN configuration type
As of today OVN-Kubernetes CNI allows to set the access VLAN of a localnet topology (using NADs).

There are two options to expose the VLAN attribute:
1. Single integer filed, living at the same level as other fields, for example:
   ```
    localnet:
       mtu: 1500
       vlan: 100
   ```
2. Discriminated union for accommodating VLAN related configurations, for example: 
   ```
    localnet:
       mtu: 1500
       vlan: 
         mode: Access
         access:
           id: 100 
   ```

Although option (1) is pretty straightforward, if additional VLAN configurations need to be supported, requiring
the exposure of additional fields, it will enforce having VLAN-related fields right next to non-VLAN-related ones, making the API look awkward.

In addition, in case future VLAN additions introduce mutual exclusive relation between VLAN config related fields,
the flat structure makes it harder to maintain the related API validations markers. (it is not centralized, lives alongside other validations and makes them harder to follow).

Option (2) enables all related VLAN configurations to live under the same roof, allowing the API to evolve smoothly while introducing less noise compared to (1),
and enable having all related API validations centralized.
For example [kubernetes-nmstate](https://github.com/nmstate/kubernetes-nmstate?tab=readme-ov-file) (utilize https://nmstate.io/) 
follows the same convention, that is having complex filed for VLAN configurations. 

The proposed options for VLAN configurations is (2)

```go
// VLANID is a VLAN ID (VID), should be greater than 0, and lower than 4095.
// +kubebuilder:validation:Minimum=1
// +kubebuilder:validation:Maximum=4094
type VLANID int32
// AccessVLANConfig describes an access VLAN configuration.
type AccessVLANConfig struct {
	// id is the VLAN ID (VID) to be set on the network logical switch port.
	ID VLANID `json:"id"`
}
// VLANConfig describes the network VLAN configuration.
// +union
type VLANConfig struct {
	// mode describe the network VLAN mode.
	// Allowed value is "Access".
	// Access sets the network logical switch port in access mode, according to the config.
	// +required
	// +unionDiscriminator
	// +kubebuilder:validation:Enum=Access
	Mode string `json:"mode"`  
    
	// Access is the access VLAN configuration 
	// +optional
	Access *AccessVlanConfig `json:"access"`
}
```

```go
// +kubebuilder:validation:XValidation:rule="!has(self.ipam) || !has(self.ipam.mode) || self.ipam.mode == 'Enabled' ? has(self.subnets) : !has(self.subnets)", message="Subnets is required with ipam.mode is Enabled or unset, and forbidden otherwise"
// +kubebuilder:validation:XValidation:rule="!has(self.excludeSubnets) || has(self.excludeSubnets) && has(self.subnets)", message="excludeSubnets must be unset when subnets is unset"
// +kubebuilder:validation:XValidation:rule="!has(self.subnets) || !has(self.mtu) || !self.subnets.exists_one(i, isCIDR(i) && cidr(i).ip().family() == 6) || self.mtu >= 1280", message="MTU should be greater than or equal to 1280 when IPv6 subent is used"
// +kubebuilder:validation:XValidation:rule="has(self.vlan) && has(self.vlan.mode) && self.vlan.mode == 'Access' ? has(self.vlan.access): !has(self.vlan.access)", message="vlan.access is required when vlan.mode is 'Access', and forbidden otherwise"
// + ---
// + TODO: enable the below validation once the following issue is resolved https://github.com/kubernetes/kubernetes/issues/130441
// + kubebuilder:validation:XValidation:rule="!has(self.excludeSubnets) || self.subnets.map(s, self.excludeSubnets.map(e, cidr(s).containCIDR(e)))", message="excludeSubnets should be in range of CIDRs specified in subnets"
// + kubebuilder:validation:XValidation:rule="!has(self.excludeSubnets) || self.excludeSubnets.all(e, self.subnets.exists(s, cidr(s).containsCIDR(cidr(e))))",message="excludeSubnets must be subnetworks of the networks specified in the subnets field",fieldPath=".excludeSubnets"
type LocalnetConfig struct {
    // role describes the network role in the pod, required.
    // Whether the pod interface will act as primary or secondary.
    // For Localnet topology only `Secondary` is allowed.
    // Secondary network is only assigned to pods that use `k8s.v1.cni.cncf.io/networks` annotation to select given network.
    // +kubebuilder:validation:Enum=Secondary
    // +required
    Role NetworkRole `json:"role"`
    
    // physicalNetworkName points to the OVS bridge-mapping's network-name configured in the nodes, required.
    // In case OVS bridge-mapping is defined by Kubernetes-nmstate with `NodeNetworkConfigurationPolicy` (NNCP),
    // this field should point to the value of the NNCP spec.desiredState.ovn.bridge-mappings.
    // Min length is 1, max length is 253, cannot contain `,` or `:` characters.
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self.matches('^[^,:]+$')", message="physicalNetworkName cannot contain `,` or `:` characters"
    // +required
    PhysicalNetworkName string `json:"physicalNetworkName"`
    
    // subnets are used for the pod network across the cluster.
    // When set, OVN-Kubernetes assign IP address of the specified CIDRs to the connected pod,
    // saving manual IP assigning or relaying on external IPAM service (DHCP server).
    // subnets is optional, when omitted OVN-Kubernetes won't assign IP address automatically.
    // Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.
    // The format should match standard CIDR notation (for example, "10.128.0.0/16").
    // This field must be omitted if `ipam.mode` is `Disabled`.
    // In a scenario `physicalNetworkName` points to OVS bridge mapping of a network who provide IPAM services (e.g.: DHCP server),
    // `ipam.mode` should set with `Disabled, turning off OVN-Kubernetes IPAM and avoid  conflicts with the existing IPAM services on the subject network.
    // +optional
    Subnets DualStackCIDRs `json:"subnets,omitempty"`
    
    // excludeSubnets list of CIDRs removed from the specified CIDRs in `subnets`.
    // excludeSubnets is optional. When omitted no IP address is excluded and all IP address specified by `subnets` subject to be assigned.
    // Each item should be in range of the specified CIDR(s) in `subnets`.
	// The maximal exceptions allowed is 25.
    // The format should match standard CIDR notation (for example, "10.128.0.0/16").
    // This field must be omitted if `subnets` is unset or `ipam.mode` is `Disabled`.
    // In a scenario `physicalNetworkName` points to OVS bridge mapping of a network who has reserved IP addresses
    // that shouldn't be assigned by OVN-Kubernetes, the specified CIDRs will not be assigned. For example:
    // Given: `subnets: "10.0.0.0/24"`, `excludeSubnets: "10.0.0.200/30", the following addresses will not be assigned to pods: `10.0.0.201`, `10.0.0.202`.
    // +optional
    // +kubebuilder:validation:MinItems=1
    // +kubebuilder:validation:MaxItems=25
    ExcludeSubnets []CIDR `json:"excludeSubnets,omitempty"`
    
    // ipam configurations for the network (optional).
    // IPAM is optional, when omitted, `subnets` should be specified.
    // When `ipam.mode` is `Disabled`, `subnets` should be omitted.
    // `ipam.mode` controls how much of the IP configuration will be managed by OVN.
    //    When `Enabled`, OVN-Kubernetes will apply IP configuration to the SDN infra and assign IPs from the selected subnet to the pods.
    //    When `Disabled`, OVN-Kubernetes only assign MAC addresses and provides layer2 communication, enable users configure IP addresses to the pods.
    // `ipam.lifecycle` controls IP addresses management lifecycle.
    //    When set with 'Persistent', the assigned IP addresses will be persisted in `ipamclaims.k8s.cni.cncf.io` object.
    // 	  Useful for VMs, IP address will be persistent after restarts and migrations. Supported when `ipam.mode` is `Enabled`.
    // +optional
    IPAM *IPAMConfig `json:"ipam,omitempty"`
    
    // mtu is the maximum transmission unit for a network.
    // MTU is optional, if not provided, the default MTU (1500) is used for the network.
    // Minimum value for IPv4 subnet is 576, and for IPv6 subnet is 1280. Maximum value is 65536.
    // In a scenario `physicalNetworkName` points to OVS bridge mapping of a network configured with certain MTU settings,
    // this field enable configuring the same MTU the pod interface, having the pod MTU aligned with the network.
    // Misaligned MTU across the stack (e.g.: pod has MTU X, node NIC has MTU Y), could result in network disruptions and bad performance.
    // +kubebuilder:validation:Minimum=576
    // +kubebuilder:validation:Maximum=65536
    // +optional
    MTU int32 `json:"mtu,omitempty"`

    // vlan configuration for the network.
	// vlan.mode is the VLAN mode.
	//   When "Access" is set, OVN-Kuberentes configures the network logical switch port in access mode.
	// vlan.access is the access VLAN configuration. 
	// vlan.access.id is the VLAN ID (VID) to be set on the network logical switch port.
	// vlan is optional, when omitted the underlying network default VLAN will be used (usually `1`).
    // When set, OVN-Kubernetes will apply VLAN configuration to the SDN infra and to the connected pods.
    // +optional
    VLAN VLANConfig `json:"vlan,omitempty"`
}
```

#### Implementation phases
1. Add support for localnet topology on top CUDN CRD
   - Extend the CUDN CRD.
   - Add support for localnet topology in the CUDN CRD controller.
   - Adjust CI multi-homing jobs to enable testing localnet CUDN CRs.
   - Update API reference docs.
2. Introduce CEL validation rule to ensure `excludedSubnets` items are in range of specified items in `subnets`.
   - Can be done once is resolved https://github.com/kubernetes/kubernetes/issues/130441.
   - Update the Kubernetes version using in CI that includes the bugfix.
   - Add the subject validation.

### Testing Details
The controller business logic is agnostic to topology types, the proposed solution can be covered fully by unit test.
In addition, the CRD controller business logic, and localnet topology functionality (using NADs) are tested e2e thoroughly. 
E2e test for localnet topology CUDN CR is optional in this particular case.

### Documentation Details

## Risks, Known Limitations and Mitigations

### CEL rule validations
Validating that the specified subnets exclusions (`spec.network.localnet.excludedSubnets`) are in the range of the specified topology subnets 
(`spec.network.localnet.excludedSubnets`) is currently impossible due to a bug in the CEL rule library for validation IPs and CIDRs.
Such invalid CUDN CRs request will not be blocked by the cluster API.
Once the bug is resolved the validation can be added.
See the following issue for more details https://github.com/kubernetes/kubernetes/issues/130441.

To mitigate this, the mentioned validation should be done by the CUDN CRD controller:
In a scenario where a CUDN CR has at least one exclude-subnet that is not within the range of the topology subnet,
the controller will not create the corresponding NAD and will report an error in the status.

## OVN-Kubernetes Version Skew

## Alternatives

## References
