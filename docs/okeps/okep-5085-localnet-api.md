# OKEP-5085: Localnet API

## Problem Statement

As of today one can create a user-defined network over localnet topology using NetworkAttachmentDefinition (NAD).
Using NAD for localnet has some pitfalls due to the fact it is not managed and not validated on creation.
Misconfigurations are detected too late causing bad UX and frustration for users.

Configuring localnet topology networks require changes to cluster nodes network stack and involve some risk and
knowledge that require cluster-admin intervention.
Such as configuring the OVS switch to which the localnet network connects to, align MTU across the stack and 
configure the right VLANs that fit the provider network.

## Goals

- Enable creating user-defined-networks over localnet topology using OVN-K CUDN CRD.
- Streamline localnet UX: detect misconfigurations early, provide indications about issues or success.
- Allow localnet topology spec changes on day2, on top the CUDN CRD.
- Indicate whether workloads network-configuration is outdated, following network-configuration change.

## Non-Goals

## Introduction

As of today OVN-Kubernetes [multi-homing feature](../../docs/features/multiple-networks/multi-homing.md) 
support creating localnet topology networks, enable connecting workloads to the host network using `NetworkAttachmentDefinition` (NAD).

This proposal outlines introduction of well-formed API on top of the `ClusterUserDefinedNetwork` CRD.

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
- As a user I want my connected VMs to the localnet network to be able to migrate from one node to another, having its localnet network interface IP address unchanged.
- As an admin I want to be able to perform day2 changes to the localnet topology spec following env changes.
  - Change VLAN, e.g: following the being used has been decommissioned.
  - Changing MTU, e.g: following HW has been changed requiring MTU re-alignment.
  - Change the localnet topology bridge-mapping, e.g.: following a requirement to connect to different interfaces on the node.
  - When I use OVN-K IP assignment, I am able to add/remove excluded IP addresses from the subnet the platform uses, 
    because the external network introduced a new service requiring an additional reserved IP address.
- As an admin / support engineer / workload manager I want to be able to list workloads who are configured with outdated network-configuration (CUDN CR localnet spec change).

## Proposed Solution

### Summary
Extend the CUDN CRD to enable creating user-defined networks over localnet topology.
Since the CUDN CRD is targeted for cluster-admin users, it enables preventing non-admin users performing changes that
could disrupt the cluster or impact the physical network to which the workloads would connect to.

Allow spec mutability for the CUDN localnet topology spec.

Provide indication on pod whether the pod is configured with an outdated network-configuration, following network CR spec change.

#### Localnet using `NetworkAttachmentDefinition`
As of today OVN-K enables multi-homing including localnet topology networks using NADs
The following NAD YAML describe localnet topology configuration and options:

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
   The underlying network name.
    - Should match the node OVS bridge-mapping network-name.
    - In case Kubernetes-nmstate is used, should match the `NodeNetworkConfigurationPolicy` (NNCP) `spec.desiredState.ovn.bridge-mappings` item's `localnet` attribute value.
2. `physicalNetworkName`
   Points to the node OVS bridge-mapping network-name - the network-name mapped to the node OVS bridge that provides access to that network.
   (Can be defined using Kubernetes-nmstate NNCP - `spec.desiredState.ovn.bridge-mappings`)
    - Overrides the `name` attribute, defined in (1).
    - Allows multiple localnet topology NADs to refer to the same bridge-mapping (thus simplifying the admin’s life -
      fewer manifests to provision, and keep synced).
3. `subnets`
   Subnets to use for the network across the cluster.
4. `excludeSubnets`
   IP addresses range to exclude from the assignable IP address pool specified by subnets field.
5. `vlanID` - VLAN tag assigned to traffic.
6. `mtu` - maximum transmission unit for a network
7. `allowPersistentIPs`
   persist the OVN Kubernetes assigned IP addresses in a `ipamclaims.k8s.cni.cncf.io` object. This IP addresses will be
   reused by other pods if requested. Useful for [KubeVirt](https://kubevirt.io/) VMs.

#### Extend ClusterUserDefinedNetwork CRD
Given the CUDN CRD is targeted for cluster-admin users, it is a good fit for operations that require cluster-admin intervention,
such as localnet topology.

The suggested solution is to extend the CUDN CRD to enable creating localnet topology networks.

##### Underlying network name
Given the CUDN API doesn’t expose the underlying network name, represented by the NAD `spec.config.name` (net-conf-network-name),
by design.
Localnet topology requires the net-conf-network-name to match the OVN bridge-mapping network-name on the node.
In case [Kubernetes-nmstate](https://nmstate.io/kubernetes-nmstate/) is used, the NAD `spec.config.name` has to match the `NodeNetworkConfigurationPolicy`
`spec.desiredState.ovn.bridge-mappings` name:
```yaml
spec:
  desiredState:
      ovn:
        bridge-mappings:
        - localnet: physnet  <---- has to match the NAD config.spec.name. OR 
                                   the NAD config.spec.physicalNetworkName
          bridge: br-ex <--------- OVS switch
```
* To overcome this, and avoid exposing the net-conf-network-name in the CUDN CRD spec a new field should be introduced.
  The new field should allow users pointing to the bridge-mapping network-name they defined in the node.
  The field should be translated to the CNI physicalNetworkName field.

to have indication about which workload require restart following spec change, thus allowing the workload to consume the latest network config

#### Indicate workloads network-configuration is out of sync
Consider the following scenario:
Given localnet CUDN CR, and multiple pods connected to the network.
The CUDN CR localnet spec is changed, the controller updated the corresponding NAD spec accordingly.
Now the connected pods network-configuration is out-dated.
Re-creating the pods will allow them to consume the latest network-configuration.

Users should be able to check which connected pod is configured with outdated network-configuration,
allow them to recreate the workloads and consume the latest network-configuration.

Provide indication for users to check which workload configured with outdated network-configuration, 
following the network configuration has been changed (i.e.: CUDN CR network spec change).
The indication should enable users check which workloads are configured with an outdated network-configuration, 
save wasting time on troubleshooting, and act on it.

The proposed solution is to set an annotation on pods who are configured with outdated network-configuration: 
```yaml
`k8s.ovn.io/network-out-of-sync='["tenant1/red", "tenant1/blue",...]`
```
The annotation value should be a JSON list consist of the outdated network namespaced names.

Examples for checking pods who are configured with an outdated network-configuration:
List pods names in namespace "tenant1" who's network-configuration is out-dated:
```shell
# Given two pods connected configured with an outdated network-configuration 
$ kubectl get pod -o jsonpath='{range .items[?(@.metadata.annotations.k8s\.ovn\.io/network-out-of-sync)]}{.metadata.name}{"\n"}{end}'
```
List pod names namespace "tenant1" who are connected to network "red" who's network-configuration is outdated:
```shell
$ kubectl get pod -o json | jq '.items[].metadata | select(.annotations."k8s.ovn.io/network-out-of-sync" | fromjson | .[] | select(. == "tenant1/red" )).name'
```

#### Workflow Description

The CUDN CRD controller should be changed accordingly to support localnet topology.
It should validate localnet topology configuration and generate corresponding NADs for localnet as other topologies (Layer2 & Layer3)
in the selected namespaces.

As of today the CUDN CRD controller does not allow mutating `spec.network`.

OVN-Kubernetes support NAD spec mutations on day 2.
For the new network configuration (defined in the spec) to take place, the user must restart the workloads.

In order to comply with NAD spec mutation support and provide better UX,
the CUDN CRD controller should allow `spec.network` mutations for localnet topologies.
Enable changing at least the following fields: `MTU`, `VLAN`, `excludeSubnets` and `physicalNetworkName`.

In any order:
- The user configures localnet bridge mapping on the nodes, e.g.: using NNCP.
- Create CUDN, specifying the bridge-mapping network name in the spec.


When a CUDN CR localnet spec changes, and the change require updating the corresponding NAD spec.config, the controller should 
fetch all connected pods in the selected-namespaces, and annotate them with `k8s.ovn.io/network-out-of-sync` annotation.
In case the annotation is already exist and have items (networks), the controller should append the subject network to the list. 

#### Generating the NAD
##### OVS bridge-mapping’s network-name
Introduce attribute to point ovs-bridge bridge-mapping network-name.
This attribute name should be translated to the CNI “physicalNetworkName” attribute.

Proposal the CUDN spec field name:  “physicalNetworkName”.

##### MTU
Should be translated to the CNI “mtu” attribute.

By default, OVN-K sets the MTU of the UDN to be 100 bytes less than the physical MTU of the underlay network.
For the localnet topology this is not optimal because localnet does not use a Geneve overlay and is directly
connected to the underlay.
This results in a loss in throughput and potential MTU mismatch issues.

The MTU value may be set by the user, and if not set then OVN-Kubernetes will determine the default value to use.

##### VLAN
Should be translated to the CNI “vlanID” attribute.
If not specified it should not be present in NAD spec.config.

##### Subnets
The subnets and exclude-subnets should be in CIDR form, similar to Layer2 topology subnets.

##### Persistent IPs
In a scenario of VMs, migrated VMs should have a persistent IP address to prevent disruption to the workloads it runs.
Localnet topology should allow using persistent IP allowing setting the CNI allowPersistentIPs.

As of today, the Layer2 topology configuration API consist of the following stanza allow using persistent IPs,
the localnet topology spec should have the same options:
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

The API should have validation that ensures `spec.network.topology` match the topology configuration, similar to
existing validation for other topologies.

#### Localnet topology spec

| Field name          | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  | optional |
|---------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|
| Role                | Select the network role in the pod.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          | No       |
| PhysicalNetworkName | The OVN bridge mapping network name is configured on the node.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               | No       |
| MTU                 | The maximum transmission unit (MTU).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         | Yes      |
| VLAN                | The network VLAN ID.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         | Yes      |
| Subnets             | List of CIDRs used for the pod network across the cluster.Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed. The format should match standard CIDR notation (for example, "10.128.0.0/16"). This field must be omitted if `ipam.mode` is `Disabled`.                                                                                                                                                                                                                                                                                                                        | Yes      |
| ExcludeSubnets      | List of CIDRs removed from the specified CIDRs in `subnets`.The format should match standard CIDR notation (for example, "10.128.0.0/16"). This field must be omitted if `subnets` is unset or `ipam.mode` is `Disabled`.                                                                                                                                                                                                                                                                                                                                                                                                    | Yes      |
| IPAM                | Contains IPAM-related configuration, similar to Layer2  & Layer3 topologies.<br>Consist of the following fileds:<br>`mode`:<br> When `Enabled`, OVN-Kubernetes will apply IP configuration to the SDN infra and assign IPs from the selected subnet to the pods.<br>When `Disabled`, OVN-Kubernetes only assign MAC addresses and provides layer2 communication, enable users configure IP addresses to the pods.<br>`lifecycle`:<br> When `Persistent` enable workloads have persistent IP addresses. For example: Virtual Machines will have the same IP addresses along their lifecycle (stop, start migration, reboots). |          |          |

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
- `VLAN`: <br/>
  According to [dot1q (IEEE 802.1Q)](https://ieeexplore.ieee.org/document/10004498),
  VID (VLAN ID) is 12-bits field, providing 4096 values; 0 - 4095. <br/>
  The VLAN IDs `0`, `1` and `4095` are reserved. <br/>
  Suggested validations:
    - Minimum: 2, Maximum: 4094.
- `Subnets`:
    - Minimum items 1, Maximum items 2.
    - Items are valid CIDR (e.g.: "10.128.0.0/16")
    - When 2 items are specified they must be of different IP families.
- `ExcludeSubnets`:
    - Minimum items 1, Maximum items - 25.
    - Items are valid CIDR (e.g.: "10.128.0.0/16")
    - Cannot be set when Subnet is unset or `ipam.mode=Disabled`.
    - Ensure excluded subnet in range of at least on subnet in `spec.network.localnet.subnets`
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
The above spec will generate the following NAD, in namespace `blue`:
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
      vlan: 200
      ipam:
        lifecycle: Persistent
```
The above spec will generate the following NAD, in namespace `red`:
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

Example 3 - `WorkloadsDegraded` condition is true:
```yaml
...
status:
  conditions: 
  - type: WorkloadsDegraded
    status: True
    reason: WorkloadsRequireRestart
    message: workload require restart in order to be configured with the latest spec.
```
Example 4 - `WorkloadsDegraded` condition is false:
```yaml
...
status:
  conditions: 
  - type: WorkloadsDegraded
    status: False
    reason: WorkloadsReady
    message: workloads can connect to the network
```

### Implementation Details

The CUDN `spec.network.topology` field should be extended to accept `Localnet` string.
And the `spec.network` struct `NetworkSpec` should be to have an additional field for localnet topology configuration:

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

#### Localnet topology spec mutation support
As mentioned in [Workflow Description](#workflow-description) section, it's possible to mutate the spec 
of `NetworkAttachmentDefinition` (NAD), including for localnet topologies.
The proposed solution should provider the similar experience when localnet topologies created using CUDN CRs. 

As of today the CUDN has validation to ensure `spec.network` is immutable:
```go
// +kubebuilder:validation:XValidation:rule="self == oldSelf", message="Network spec is immutable"
```
This validation should be changed in a way it allows mutating localnet topology configurations,
at least `MTU`, `VLAN`, `physicalNetworkName` and `excludeSubnets`.
The proposed change is to invert the immutable-spec validation on CUDN `spec.network` as follows:
1. Remove mutation restrictions on `spec.network`.
2. Add CEL rule validations to restrict mutations of:
   `spec.network.topology`, `spec.network.layer2`, and `spec.network.layer3`.
3. Add CEL rule validations to restrict mutation of localnet topology settings:
   `spec.network.localnet.role`, `spec.network.localnet.subnets` and `spec.network.topology.ipam`.

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
1. Remove the enum validation exist on `NetworkRole` definition
2. At the `Layer2Config` definition, add enum validation to `role` field allowing: `Primary` or `Secondary`.
3. At the `Layer3Config` definition, add enum validation to `role` field allowing: `Primary` or `Secondary`.
4. At the proposed `LocalnetConfig` definition, add enum validation to `role` field allowing `Secondary` only.

```go
// +kubebuilder:validation:XValidation:rule="!has(self.ipam) || !has(self.ipam.mode) || self.ipam.mode == 'Enabled' ? has(self.subnets) : !has(self.subnets)", message="Subnets is required with ipam.mode is Enabled or unset, and forbidden otherwise"
// +kubebuilder:validation:XValidation:rule="!has(self.excludeSubnets) || has(self.excludeSubnets) && has(self.subnets)", message="excludeSubnets must be unset when subnets is unset"
// +kubebuilder:validation:XValidation:rule="!has(self.subnets) || !has(self.mtu) || !self.subnets.exists_one(i, isCIDR(i) && cidr(i).ip().family() == 6) || self.mtu >= 1280", message="MTU should be greater than or equal to 1280 when IPv6 subent is used"
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
    
    // vlan is the VLAN ID (VID) to be used for the network.
    // VLAN is optional, when omitted the underlying network default VLAN will be used (usually `1`).
    // When set, OVN-Kubernetes will apply VLAN configuration to the SDN infa and to the connected pods.
    // +kubebuilder:validation:Minimum=2
    // +kubebuilder:validation:Maximum=4094
    // +optional
    VLAN int32 `json:"vlan,omitempty"`
}
```

#### Implementation phases
1. Add support for localnet topology on top CUDN CRD
   - Extend the CUDN CRD.
   - Add support for localnet topology in the CUDN CRD controller.
   - Adjust CI multi-homing jobs to enable testing localnet CUDN CRs.
   - Update API reference docs.
2. Enable localnet CUDN CRs spec mutation 
   - Invert the CRD validations in a way spec mutation is allowed for localnet topology configuration only.
   - Add support for localnet topology configuration mutations in the CUDN CRD controller.
3. Change OVN-K to annotate pods with the proposed pod annotation
   - The annotation should consist of the CRs versions the pod was configured with.
4. Add for the purposed status condition.
   - Change the CRDs controller to add the proposed condition, utilizing the proposed pod annotation for managing the condition lifecycle.
5. Introduce CEL validation rule to ensure `excludedSubnets` items are in range of specified items in `subnets`.
   - Can be done once is resolved https://github.com/kubernetes/kubernetes/issues/130441.
   - Update the Kubernetes version using in CI that includes the bugfix.
   - Add the subject validation.
6. E2e test verifying Kubevirt VMs can communicate over localnet topology network created using CUDN CR.


### Testing Details

Unit tests should cover the new localnet topology API extension, and spec mutation for localnet topology spec.

As of today localnet topology implementation is tested e2e as part of multi-homing test suite.
The proposed solution should be tested e2e verifying pods can communicate over localnet topology created using CUDN CR.
Similar e2e tests should be introduced using Kubevirt VMs.

### Documentation Details

## Risks, Known Limitations and Mitigations

CEL rule validations:
Validating specified subnets exclusions (`spec.network.localnet.excludedSubnets`) are in range of the specified topology subnets 
(`spec.network.localnet.excludedSubnets`) is currently impossible due to a bug in the CEL rule library for validation IPs and CIDRs.
Such invalid CUDN CRs request will not be blocked by the cluster API.
Once the bug is resolved the validation can be added.
See the following issue for more details https://github.com/kubernetes/kubernetes/issues/130441.

To mitigate this, the mentioned validation should be done by the CUDN CRD controller:
In a scenario CUDN CR who has at least one exclude-subnet is not in range of the topology subnet,
the controller will not create the corresponding NAD, and report an error in the status.

## OVN Kubernetes Version Skew

## Alternatives

Following discussions about the proposed solution for indicating pod configured with outdated network-config,
some alternatives were suggested.
The proposed solution serves as a baseline for most suggested alternatives, makes them an addition on top
or an additional matter for providing similar indication as the proposed one.

1. Emit events 
Following CUDN CR locanet sepc changed, the CUDN controller should emit event on connected pods.
The event should indicate pod require restart to consume the latest network config.

Comparing to annotation, events are harder to manage and get garbage collected.
In addition, filtering events to realize which pod run outdated network-config is not intuitive 
comparing to filtering by an annotation.

Emitting events on pods can be a complementary matter alongside the proposed solution.

2. Status condition
The CUDN CRD should have status condition indicating workloads configured with outdated network-configuration
following localnet spec change.
The condition should be `true` when there is at least one connected pod configured with the old spec.
The condition should be `false` when all connected pods configured with the latest spec.

The proposed pod annotation is the baseline for providing such condition.
Adding status condition can be complimentary matter along side the proposed solution.
I can be added later on upon necessity.

3. Provide collective data for alerting framework
Alert is a notification that is raised based on various data sources in the cluster 
such as object status, event, logs, metrics, etc..

The proposed pod annotation is the baseline for providing collective data
that can be used by an alerting framework.
Providing collective data for alerting frameworks can be done later on upon necessity.

4. Add support for OVN-K CNI plugin to configure running pods  
As of today OVN-K configure the pod network-stack when a pod is created or deleted.

Introduce OVN-K CNI plugin using a think plugin architecture, similar to Multus CNI think-plugin
https://github.com/k8snetworkplumbingwg/multus-cni/blob/master/docs/thick-plugin.md
And a controller to monitor NADs and trigger the CNI on spec chanages, to update the pods  network-stack witht the latest
network-configuration.
Similar to the multus-dynamic-network-controller
https://github.com/k8snetworkplumbingwg/multus-dynamic-networks-controller

Having a OVN-K CNI being able to update the pods network-stack following network-configuration 
change, may eliminate the need for the proposed solution.
This alternative deserves its own design and consensus among the community.
In case this alternative is introduced it can replace the proposed one.

In addition, having OVN-K CNI re-configure running pods has potential to improve the UX for spec
mutation support dramatically, and it won't require re-creating the workloads.
For Kubevirt VMs the impact is event greater, usually VMs should have minimal downtime.

## References
