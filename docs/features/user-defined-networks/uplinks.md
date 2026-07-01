# Uplinks for User Defined Networks

## Introduction

An `Uplink` is a cluster-scoped API that lets a
`ClusterUserDefinedNetwork` use a named external network path instead of the
cluster default gateway bridge. This is useful when different tenant networks
need different north-south paths, different Linux VRFs, or different BGP
peering interfaces.

An `Uplink` does not create or manage the external OVS bridge. The bridge and
its physical uplink must already exist on the selected nodes. OVN-Kubernetes
discovers the bridge, records node-local state in `UplinkState`, configures OVN
bridge mappings, and programs the gateway and service flows needed by the CUDN.

## Supported Scope

This feature currently supports:

* `ClusterUserDefinedNetwork` only.
* Primary `Layer2` and primary `Layer3` CUDNs.
* One Uplink name in `ClusterUserDefinedNetwork.spec.uplinks`.
* `Uplink.spec.nodeConfigs[].type: OVSBridge`.
* Shared gateway mode.
* Regular nodes and DPU deployments.

This feature does not currently support:

* Namespace-scoped `UserDefinedNetwork`.
* Secondary networks.
* `Localnet` topology.
* Local gateway mode.
* EVPN transport.
* Creating or configuring the external OVS bridge.
* More than one Uplink per CUDN.

## API Overview

`Uplink` selects node groups and names the host-visible interface used for each
selected node:

```yaml
apiVersion: k8s.ovn.org/v1alpha1
kind: Uplink
metadata:
  name: tenant-blue
spec:
  nodeConfigs:
  - type: OVSBridge
    nodeSelector:
      matchLabels:
        example.com/tenant-blue-uplink: "true"
    hostInterfaceName: ovsbr1
```

`hostInterfaceName` is the host-side Linux interface that carries the gateway
L3 identity for the uplink:

* On a regular non-accelerated node, this is normally the OVS bridge internal
  interface. The physical NIC must already be a port on that bridge.
* On a SmartNIC accelerated node, this is the VF/SF netdevice that carries the
  gateway IPs. Its representor must already be a port on the OVS bridge.
* On a DPU-host node, this is the host-visible interface connected to the DPU
  uplink path. OVN-Kubernetes uses it to discover the host-side L3 state.
* On the DPU side, OVN-Kubernetes resolves the DPU-local OVS bridge from the
  host-side state. The DPU-local interface name is not specified in the API.

Each selected node must match at most one `nodeConfig` in a single `Uplink`.
If multiple `nodeConfigs` select the same node, the `Uplink` is not ready and a
CUDN that needs that node reports `UplinksReady=False`.

A CUDN references the Uplink by name:

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: blue
spec:
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: blue
  uplinks:
  - tenant-blue
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.200.0.0/16
        hostSubnet: 24
```

## Regular Node Deployments

For non-DPU deployments, the administrator provisions the OVS bridge before the
`Uplink` is used. For non-accelerated nodes, the bridge must have:

* An internal interface named by `hostInterfaceName`.
* One physical uplink port that reaches the external network.
* The expected IP addresses and default gateways on the internal interface.
* Link state up on the bridge, internal interface, and physical uplink.
* MTU configured consistently on the physical NIC, bridge, and bridge ports.

For SmartNIC accelerated nodes, `hostInterfaceName` is the VF/SF netdevice with
the gateway IPs. Its representor must be a port on the OVS bridge. OVN-Kubernetes
discovers the representor from `hostInterfaceName`, then discovers the OVS
bridge from the representor.

OVN-Kubernetes discovers the bridge from `hostInterfaceName` or from its
representor. It rejects the configuration if the interface is the physical
uplink port itself, if the bridge cannot be resolved, if the bridge is `br-int`,
or if the bridge has no discoverable physical uplink.

After discovery succeeds, ovnkube-node updates the per-node `UplinkState` with
the resolved bridge name, MAC address, IP addresses, and default gateways.
OVN-Kubernetes then uses that state to build the CUDN gateway configuration.

## DPU Deployments

DPU deployments split Uplink discovery across the host and DPU components:

* The DPU-host ovnkube-node reads `hostInterfaceName`, discovers the host-side
  MAC address, IP addresses, and default gateways, and writes that data to
  `UplinkState`.
* The DPU ovnkube-node reads the same `UplinkState`, uses the host MAC address
  to find the DPU-local representor and OVS bridge, validates the bridge, and
  writes `status.ovsBridge.name`.
* Once both sides have published the required data, the `UplinkState` becomes
  `Ready=True`.

The user does not provide the DPU-local OVS bridge name in the `Uplink` API.
The DPU side derives it from the DPU-local OVS bridge state. The bridge must
already be provisioned with the host representor or bridge port that connects
to the host uplink path.

The same MTU requirement applies in DPU deployments. The host-side interface,
DPU representor, DPU OVS bridge, and physical uplink must be configured
consistently by the platform.

## UplinkState and Conditions

`UplinkState` is a cluster-scoped, per-node status object. Its name is derived
from the Uplink name and node name. It is owned by the `Uplink`.

Example:

```yaml
apiVersion: k8s.ovn.org/v1alpha1
kind: UplinkState
metadata:
  name: tenant-blue.worker-a
status:
  uplinkName: tenant-blue
  nodeName: worker-a
  type: OVSBridge
  hostInterfaceName: ovsbr1
  ovsBridge:
    name: ovsbr1
  macAddress: 02:42:ac:1c:00:02
  ipAddresses:
  - 172.28.0.2/16
  defaultGateways:
  - 172.28.0.1
  conditions:
  - type: Ready
    status: "True"
    reason: Ready
```

The `Uplink` object reports aggregate status:

* `Ready=True` means every selected node has a usable `UplinkState`.
* `Ready=False` means one or more selected nodes do not have usable
  `UplinkState`, or the `Uplink` spec selects nodes incorrectly.

`UplinkState` supports field selection by Uplink name and node name:

```bash
$ kubectl get uplinkstate --field-selector status.uplinkName=tenant-blue
NAME                   UPLINK        NODE       READY
tenant-blue.worker-a   tenant-blue   worker-a   True
tenant-blue.worker-b   tenant-blue   worker-b   True

$ kubectl get uplinkstate --field-selector status.uplinkName=tenant-blue,status.nodeName=worker-a -o yaml
apiVersion: v1
items:
- apiVersion: k8s.ovn.org/v1alpha1
  kind: UplinkState
  metadata:
    name: tenant-blue.worker-a
  status:
    uplinkName: tenant-blue
    nodeName: worker-a
    type: OVSBridge
    hostInterfaceName: ovsbr1
    ovsBridge:
      name: ovsbr1
    conditions:
    - type: Ready
      status: "True"
      reason: Ready
kind: List
metadata:
  resourceVersion: ""
```

When one or more CUDNs reference an `Uplink`, OVN-Kubernetes keeps a finalizer
on the `Uplink` so it cannot be deleted while still selected by a CUDN.

The CUDN reports a CUDN-specific `UplinksReady` condition. This condition is
computed from the active nodes for that CUDN, not directly from aggregate
`Uplink.status`. This matters with Dynamic UDN, where two CUDNs can reference
the same `Uplink` but be active on different nodes.

For a CUDN that does not set `spec.uplinks`, `UplinksReady=True` and existing
gateway behavior is preserved.

## Dynamic UDN

With Dynamic UDN enabled, OVN-Kubernetes delays Uplink gateway programming on a
node until the CUDN becomes active on that node. Inactive nodes do not block the
CUDN `UplinksReady` condition.

If a node later becomes inactive for the CUDN, OVN-Kubernetes does not
immediately remove the Uplink bridge configuration. The normal Dynamic UDN
inactive-network cleanup behavior applies to the CUDN network state.

See [Dynamic UDN Node Allocation](dynamic-udn.md) for the node activity model.

## Route Advertisements and VRFs

The Uplink feature works with RouteAdvertisements. The selected
RouteAdvertisements determine whether the Uplink bridge stays in the default
VRF or is attached to the CUDN VRF.

When a CUDN using an Uplink is advertised through the default VRF,
OVN-Kubernetes leaves the Uplink bridge in the default VRF. Routes learned by
FRR in the default VRF can be used for the CUDN, and pod egress uses the Uplink
gateway path.

When a CUDN using an Uplink is advertised with `targetVRF: auto`,
OVN-Kubernetes treats the Uplink as the VRF-Lite path for that CUDN. The Uplink
bridge interface is enslaved to the CUDN Linux VRF on regular nodes. In DPU
deployments, the DPU-local bridge interface is attached to the CUDN VRF on the
DPU side. FRR peers in that VRF, and ovnkube reflects the routes learned in
that VRF into the OVN gateway routers for the CUDN.

Example RouteAdvertisements for VRF-Lite:

```yaml
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: blue
spec:
  targetVRF: auto
  networkSelectors:
  - networkSelectionType: ClusterUserDefinedNetworks
    clusterUserDefinedNetworkSelector:
      networkSelector:
        matchLabels:
          advertise: blue
  nodeSelector: {}
  frrConfigurationSelector:
    matchLabels:
      network: blue
  advertisements:
  - PodNetwork
```

The Uplink object does not decide which routes exist. Routes come from the
kernel routing table and from RouteAdvertisements and FRR configuration.

## Services

OVN-Kubernetes programs service flows on the external bridge associated with
the CUDN. For a CUDN that uses an Uplink, per-UDN service flows are programmed
on the resolved Uplink bridge rather than only on the cluster default gateway
bridge.

For default-VRF Uplinks, NodePort traffic can enter from the cluster default
gateway path or from the Uplink path and still reach services backed by pods on
the Uplink CUDN.

For VRF-Lite Uplinks, service traffic is expected to enter through an interface
that belongs to the same VRF as the CUDN. Traffic that enters from an unrelated
VRF is not expected to reach services for that CUDN.

## Troubleshooting

Check the aggregate Uplink status:

```bash
kubectl get uplink tenant-blue -o yaml
```

Check the per-node discovery state:

```bash
kubectl get uplinkstate --field-selector status.uplinkName=tenant-blue -o wide
kubectl get uplinkstate tenant-blue.worker-a -o yaml
```

Check the CUDN condition:

```bash
kubectl get clusteruserdefinednetwork blue -o yaml
```

Common problems:

* `Uplink` with `Ready` condition status `False` and reason
  `MissingUplinkState`: the selected node has not created or published its
  `UplinkState`.
* `UplinkState` with `Ready` condition status `False` and reason
  `HostInterfaceNotFound`: the selected `hostInterfaceName` does not exist on
  the node.
* `UplinkState` with `Ready` condition status `False` and reason
  `BridgeNotFound`: OVN-Kubernetes could not map `hostInterfaceName` to an OVS
  bridge.
* `UplinkState` with `Ready` condition status `False` and reason
  `BridgeUplinkNotFound`: the resolved OVS bridge does not have a discoverable
  physical uplink port.
* `UplinkState` with `Ready` condition status `False` and reason
  `BridgeInvalid`: the selected bridge is not a valid external bridge, for
  example `br-int`.
* CUDN with `UplinksReady` condition status `False` and reason
  `UplinkNotReadyForNode`: at least one active node for the CUDN does not have
  a ready matching `UplinkState`.
* CUDN with `UplinksReady` condition status `False` and reason
  `UplinkNotFoundForNode`: the referenced Uplink does not select an active node
  for the CUDN.
* CUDN with `UplinksReady` condition status `False` and reason
  `UplinkOverlapOnNode`: multiple `nodeConfigs` in the referenced Uplink select
  the same active node.
* CUDN with `UplinksReady` condition status `False` and reason
  `UplinkUnsupportedGatewayMode`: the cluster is not running shared gateway
  mode.
* CUDN with `UplinksReady` condition status `False` and reason
  `UplinkUnsupportedTransport`: the CUDN uses EVPN transport, which is not
  supported with Uplinks.

## References

* [User Defined Networks](user-defined-networks.md)
* [Dynamic UDN Node Allocation](dynamic-udn.md)
* [Route Advertisements](../bgp-integration/route-advertisements.md)
* [DPU support](../hardware-offload/dpu-support.md)
