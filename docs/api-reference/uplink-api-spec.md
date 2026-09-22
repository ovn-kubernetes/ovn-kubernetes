# API Reference

## Packages
- [k8s.ovn.org/v1alpha1](#k8sovnorgv1alpha1)


## k8s.ovn.org/v1alpha1

Package v1alpha1 contains API Schema definitions for the Uplink v1alpha1 API
group.

### Resource Types
- [Uplink](#uplink)
- [UplinkList](#uplinklist)
- [UplinkState](#uplinkstate)
- [UplinkStateList](#uplinkstatelist)



#### HostFunction



HostFunction identifies a PCI function: a physical function by its index,
or a virtual function by the PF index and its own index on that PF.



_Appears in:_
- [UplinkStateStatus](#uplinkstatestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `pfID` _integer_ | PFID is the index of the physical function backing the host interface. |  | Minimum: 0 <br />Required: \{\} <br /> |
| `vfID` _integer_ | VFID is the index of the virtual function on the physical function.<br />Absent when the host interface is the physical function itself. |  | Minimum: 0 <br />Optional: \{\} <br /> |


#### IPAddress

_Underlying type:_ _string_

IPAddress is an IP address.

_Validation:_
- MaxLength: 64

_Appears in:_
- [UplinkStateStatus](#uplinkstatestatus)



#### IPAddressCIDR

_Underlying type:_ _string_

IPAddressCIDR is an IP address with a network prefix.

_Validation:_
- MaxLength: 64

_Appears in:_
- [UplinkStateStatus](#uplinkstatestatus)



#### InterfaceName

_Underlying type:_ _string_

InterfaceName is a Linux interface name.

_Validation:_
- MaxLength: 15
- MinLength: 1
- Pattern: `^[^/\s]+$`

_Appears in:_
- [UplinkNodeConfig](#uplinknodeconfig)
- [UplinkStateStatus](#uplinkstatestatus)



#### MACAddress

_Underlying type:_ _string_

MACAddress is an IEEE 802 MAC address.

_Validation:_
- Pattern: `^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$`

_Appears in:_
- [UplinkStateStatus](#uplinkstatestatus)



#### OVSBridgeStatus







_Appears in:_
- [UplinkStateStatus](#uplinkstatestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the resolved OVS bridge used for OVN bridge mappings and flows. |  | MaxLength: 15 <br />MinLength: 1 <br />Pattern: `^[^/\s]+$` <br />Optional: \{\} <br /> |


#### Uplink



Uplink represents a physical network path out of a node that OVN-Kubernetes
can connect its logical topology to. Supported node config type: OVSBridge.



_Appears in:_
- [UplinkList](#uplinklist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1alpha1` | | |
| `kind` _string_ | `Uplink` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[UplinkSpec](#uplinkspec)_ |  |  | Required: \{\} <br />Required: \{\} <br /> |
| `status` _[UplinkStatus](#uplinkstatus)_ |  |  | Optional: \{\} <br /> |


#### UplinkList



UplinkList contains a list of Uplink resources.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1alpha1` | | |
| `kind` _string_ | `UplinkList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[Uplink](#uplink) array_ |  |  |  |


#### UplinkNodeConfig







_Appears in:_
- [UplinkSpec](#uplinkspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[UplinkType](#uplinktype)_ | Type is the uplink node config type. Supported value: OVSBridge. |  | Enum: [OVSBridge] <br />Required: \{\} <br />Required: \{\} <br /> |
| `nodeSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#labelselector-v1-meta)_ | NodeSelector selects nodes where this uplink config applies. An empty<br />selector matches all nodes. |  | Required: \{\} <br />Required: \{\} <br /> |
| `hostInterfaceName` _[InterfaceName](#interfacename)_ | HostInterfaceName is the host-visible Linux interface that carries the<br />gateway L3 identity for this uplink. In non-accelerated deployments,<br />this is typically the OVS bridge Linux interface, whose name usually<br />matches the OVS bridge name. In SmartNIC accelerated deployments, this is<br />the VF/SF netdevice and OVN-Kubernetes resolves the OVS bridge through<br />its representor. In DPU deployments, this is the DPU-host PF interface<br />whose DPU-side PF representor is attached to the OVS bridge. |  | MaxLength: 15 <br />MinLength: 1 <br />Pattern: `^[^/\s]+$` <br />Required: \{\} <br />Required: \{\} <br /> |


#### UplinkSpec







_Appears in:_
- [Uplink](#uplink)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeConfigs` _[UplinkNodeConfig](#uplinknodeconfig) array_ | NodeConfigs contains mappings of nodes to uplink configuration. A node may<br />be selected by at most one nodeConfig entry. Multiple nodeConfigs that<br />select the same node are treated as a configuration error. |  | MaxItems: 64 <br />MinItems: 1 <br />Required: \{\} <br />Required: \{\} <br /> |


#### UplinkState



UplinkState contains node-local discovery and gateway state for an Uplink.



_Appears in:_
- [UplinkStateList](#uplinkstatelist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1alpha1` | | |
| `kind` _string_ | `UplinkState` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[UplinkStateSpec](#uplinkstatespec)_ |  |  | Required: \{\} <br />Required: \{\} <br /> |
| `status` _[UplinkStateStatus](#uplinkstatestatus)_ |  |  | Optional: \{\} <br /> |


#### UplinkStateList



UplinkStateList contains a list of UplinkState resources.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1alpha1` | | |
| `kind` _string_ | `UplinkStateList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[UplinkState](#uplinkstate) array_ |  |  |  |


#### UplinkStateSpec







_Appears in:_
- [UplinkState](#uplinkstate)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `uplinkName` _string_ | UplinkName is the Uplink this state belongs to. |  | MaxLength: 253 <br />MinLength: 1 <br />Required: \{\} <br /> |
| `nodeName` _string_ | NodeName is the node this state belongs to. |  | MaxLength: 253 <br />MinLength: 1 <br />Required: \{\} <br /> |


#### UplinkStateStatus







_Appears in:_
- [UplinkState](#uplinkstate)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[UplinkType](#uplinktype)_ | Type is defined by the matched nodeConfig of the Uplink. |  | Enum: [OVSBridge] <br />Optional: \{\} <br /> |
| `hostInterfaceName` _[InterfaceName](#interfacename)_ | HostInterfaceName is the host-visible Linux interface selected by<br />Uplink spec. It carries the host-side gateway L3 identity in all modes. |  | MaxLength: 15 <br />MinLength: 1 <br />Pattern: `^[^/\s]+$` <br />Optional: \{\} <br /> |
| `ovsBridge` _[OVSBridgeStatus](#ovsbridgestatus)_ | OVSBridge contains resolved OVS bridge data. |  | Optional: \{\} <br /> |
| `macAddress` _[MACAddress](#macaddress)_ | MACAddress is the MAC address used for the OVN gateway interface. |  | Pattern: `^([0-9a-fA-F]\{2\}:)\{5\}[0-9a-fA-F]\{2\}$` <br />Optional: \{\} <br /> |
| `hostFunction` _[HostFunction](#hostfunction)_ | HostFunction identifies the PCI function (a PF, or a VF on that PF)<br />backing the host interface. Only SR-IOV capable functions are<br />published: they are the ones that can have a representor on a DPU,<br />which is what this field exists to resolve. In split DPU mode the<br />DPU-host publishes it so the DPU can resolve the interface's<br />representor and OVS bridge directly, without scanning bridges by host<br />MAC. Absent when the host interface has no such function or its<br />identity cannot be resolved. |  | Optional: \{\} <br /> |
| `ipAddresses` _[IPAddressCIDR](#ipaddresscidr) array_ | IPAddresses are host-side shared gateway IP addresses. |  | MaxItems: 2 <br />MaxLength: 64 <br />Optional: \{\} <br /> |
| `defaultGateways` _[IPAddress](#ipaddress) array_ | DefaultGateways are distinct next-hop IPs from the selected host interface's<br />lowest-metric default routes per IP family. Among those next hops only the<br />ones with the highest weight are published; weights themselves are not<br />represented, so lighter next hops of an unequal-weight multipath route are<br />omitted rather than programmed as equal-cost paths.<br />The limit is 256 total across both IP families per node and Uplink; it is<br />an API bound, not a guarantee of dataplane or hardware offload capacity. |  | MaxItems: 256 <br />MaxLength: 64 <br />Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#condition-v1-meta) array_ | Conditions reports node-local discovery and gateway programming state for<br />this resolved uplink. |  | Optional: \{\} <br /> |


#### UplinkStatus







_Appears in:_
- [Uplink](#uplink)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#condition-v1-meta) array_ | Conditions reports aggregate Uplink state. |  | Optional: \{\} <br /> |


#### UplinkType

_Underlying type:_ _string_





_Appears in:_
- [UplinkNodeConfig](#uplinknodeconfig)
- [UplinkStateStatus](#uplinkstatestatus)

| Field | Description |
| --- | --- |
| `OVSBridge` |  |


