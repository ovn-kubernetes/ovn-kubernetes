# OKEP-5259: No-overlay Mode For Layer-3 Networks

* Issue: [\#5259](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/5259)  
* Authors: Riccardo Ravaioli, Peng Liu

## Problem Statement

Currently, OVN-Kubernetes uses Geneve as its encapsulation method on the overlay
network for east-west traffic; this adds overhead and reduces throughput. By
leveraging OVN-Kubernetes support for BGP, we want to provide a way for users to
enable a no-overlay mode, which would disable Geneve encapsulation and use
direct routing between nodes for east-west traffic on selected networks.

Many environments, particularly on-premise deployments or those with dedicated
networking infrastructure, prefer to utilize the underlying physical network's
routing capabilities directly. This "no-overlay" approach can offer several
benefits:

* Improved Performance: Eliminates encapsulation/decapsulation overhead,
  potentially leading to lower latency and higher throughput for inter-pod
  communication.
* Simplified Troubleshooting: Traffic paths are more transparent as they align
  with the physical network's routing tables, simplifying debugging and network
  visibility.
* Leverage Existing Network Infrastructure: Integrates more seamlessly with
  existing BGP-capable network devices, allowing for direct routing to pod IPs.  
* Reduced Resource Consumption: Fewer CPU cycles spent on
  encapsulation/decapsulation.

## Goals

* Support no-overlay mode for the default network.
* Support no-overlay mode for Primary layer-3 ClusterUserDefinedNetworks
  (CUDNs).
* A cluster can have networks operating in overlay and no-overlay modes
  simultaneously.
* Use the BGP feature to exchange routes to node subnets across the cluster.
* Allow direct communication without any overlay encapsulation for east-west
  traffic.
* Maintain compatibility with existing OVN-Kubernetes features where applicable.
* Compatible with both local gateway and shared gateway modes.

## Future Goals

* When OVN-Kubernetes supports BGP for UserDefinedNetwork CRs, extend no-overlay
  mode support for Primary layer-3 UDNs.

* Support toggling no-overlay mode on/off for an existing network. To support
  this function, there are two things we need to consider.
  First, in no-overlay mode, as there is no Geneve overhead, a network can have
  the same MTU size as the provider network. However, overlay networks require a
  smaller MTU. We cannot modify a pod interface MTU without restarting the pod.
  So OVN-Kubernetes must work with other external controllers to accommodate
  such a migration.
  Secondly, to facilitate a smooth, node-by-node migration, it's crucial to
  allow networks to operate in a hybrid no-overlay and overlay mode.  

* Support EgressIP and EgressService features in networks operating in
  no-overlay mode. Both features leverage OVN-Kubernetes's logical routing
  policy to steer pod egress traffic. They also rely on overlay tunnels to
  transport pod egress traffic to egress nodes. Implementing these features in
  no-overlay mode with BGP requires significant architectural changes and
  careful consideration.

* Support a network operating in an overlay and no-overlay hybrid mode.
  Currently, BGP only supports advertising entire pod networks. In the future,
  we may add support for selective node advertisement, which will enable hybrid
  modes. This will allow networks to operate in overlay and no-overlay modes
  simultaneously. It is useful for scenarios such as overlay mode migration or
  hybrid provider networks.

## Non-Goals

* This enhancement does not aim to change the default behavior of
  OVN-Kubernetes, which will continue to use Geneve encapsulation for the
  default network and any user-defined networks unless no-overlay mode is
  explicitly enabled.

* This enhancement does not aim to change the existing CUDN/UDN isolation
  mechanism. CUDNs/UDNs will continue to be isolated from each other and the
  default network using OVN ACLs on every node logical switch.

* This enhancement does not aim to change the existing BGP configuration or
  behavior. The user must ensure that the BGP configuration is correctly set up
  to support no-overlay mode.

* This enhancement does not aim to change the existing CUDN lifecycle
  management. The user must ensure that the CUDN CRs are correctly managed
  according to the existing lifecycle management practices.

* This enhancement does not aim to implement no-overlay mode with the
  centralized OVN architecture, as the BGP feature is only available in a
  cluster running in interconnect mode.

* This enhancement does not aim to implement no-overlay mode for the layer-2 or
  localnet type of networks. The BGP feature doesn't support the localnet type
  of networks. The layer-2 type of networks is implemented by connecting pods to
  the same OVN distributed logical switch. Pods on different nodes are connected
  through a layer-2 segment using Geneve encapsulation. It's quite challenging
  to implement a layer-2 network over a layer-3 infrastructure without an
  overlay protocol.

## Introduction

In the [OVN-Kubernetes BGP Integration
enhancement](https://github.com/openshift/enhancements/blob/master/enhancements/network/bgp-ovn-kubernetes.md#no-tunneloverlay-mode),
no-overlay mode was briefly discussed. In this enhancement, we aim to describe
the feature in detail, define the API changes we want to introduce for it, and
address a number of concerns with respect to the existing BGP Integration and
User-Defined Network Segmentation features.

Avoiding Geneve encapsulation and using the provider network for east-west
traffic spawns from the need to minimize network overhead and maximize
throughput. Users who intend to enable BGP on their clusters can indeed leverage
BGP-learned routes to achieve this. The goal is to provide users with an API to
enable or disable no-overlay mode on selected networks (default or
user-defined), allowing traffic to skip Geneve encapsulation (i.e., the overlay
network) and simply make use of the learned routes in the underlay or provider
network for inter-node communication.

While BGP is the proposed solution for exchanging east-west routing information
within a cluster, it is not the only possible option. Future implementations of
no-overlay mode may incorporate additional routing technologies and protocols
beyond BGP.

## User-Stories/Use-Cases

### Story 1: Enable no-overlay mode for the default network

As a cluster admin, I want to avoid all encapsulation for traffic in the default
network to integrate seamlessly with existing BGP-capable networks, achieve
maximum network performance, and simplify troubleshooting.

### Story 2: Enable no-overlay mode for a CUDN

As a cluster admin, I want to avoid all encapsulation for traffic in a CUDN to
integrate seamlessly with existing BGP-capable networks, achieve maximum network
performance, and simplify troubleshooting.

### Story 3: Enable no-overlay mode without an external BGP router

As a cluster admin, I want to avoid all encapsulation for traffic in the
cluster, but I don't want to deploy an external BGP router for the cluster.

## Proposed Solution

The core idea is to leverage the existing BGP feature to advertise the IP subnet
allocated to each node (which contains the IPs of pods running on that node) to
an internal/external BGP route reflector. The BGP route reflector will then
populate these routes throughout the physical network, allowing other nodes to
directly route traffic to pod IPs without needing an overlay. Within a cluster,
different networks can operate in different overlay modes. Users can have
overlay and no-overlay networks simultaneously.

When no-overlay mode is enabled for a network:

* For north-south traffic, we will follow the existing implementation of the BGP
  feature; the traffic from pods will no longer be SNATed on egress. In the BGP
  feature, SNAT is disabled for all pod egress traffic that leaves the node,
  regardless of whether the destination route is BGP learned or not. For users
  who only want to have no-overlay mode but do not want to expose podIPs to the
  external, this behavior is not ideal. Instead of disabling SNAT for all the
  pod-to-external traffic, OVN-K shall adjust its SNAT behavior so that pod IPs
  are exposed for traffic routed via BGP, but SNAT is still applied for
  non-BGP-routed egress traffic.

* For east-west traffic, intra-node traffic (pod-to-pod, pod-to-clusterIP, and
  pod-to-host) remains unchanged, while cross-node traffic will follow the same
  path as north-south traffic.

For the default network, we will add a new flag
`--default-network-encapsulation` to the `ovnkube` binary. This flag accepts
either `none` or `geneve`, with `geneve` being the default. Setting
`--default-network-encapsulation=none` will configure the default network to
operate in no-overlay mode. If the `--default-network-encapsulation=none` flag
is set but the required RouteAdvertisements or FRRConfiguration CR is missing,
`ovnkube` will exit with an error.

For CUDNs, no-overlay mode shall be configured via the CUDN CRs.

### API Details

We introduce a new `layer3.encapsulation` field to be added to the Spec of the
ClusterUserDefinedNetwork (CUDN) CRD. This new field will enable control over
the network encapsulation behavior with the following options:

1. **Encapsulation Configuration**:  
   * `encapsulation`: specifies the encapsulation method (default: `Geneve`)  
   * Supported values: `Geneve` (default) or `None` (no-overlay)

The `spec` field of a ClusterUserDefinedNetwork CR is immutable. Therefore, the
encapsulation configuration cannot be changed after the
ClusterUserDefinedNetwork CR is created.

#### Example of a layer-3 CUDN with no-overlay mode enabled

A layer-3 CUDN with no-overlay mode enabled should look like this:

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: my-cudn
spec:
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values: ["red", "blue"]
  network:
    topology: Layer3
    layer3:
      role: Primary
      mtu: 1500
      subnets:
      - cidr: 10.10.0.0/16
        hostSubnet: 24
      encapsulation: "None"
```

The topology spec for each network type is shared between CUDN and UDN. Once we
add the new `encapsulation` field, it will be available for both CUDN and UDN.
To prevent a user from setting this field in a UDN CR accidentally, we will add
the following CEL validation rule to the UserDefinedNetwork CRD.

```yaml
    layer3:
      description: Layer3 is the Layer3 topology configuration.
      x-kubernetes-validations:
      - message: Setting layer3.encapsulation is not supported for UserDefinedNetworks
        rule: '!has(self.encapsulation)'
```

### Implementation Details

No-overlay mode relies on the BGP feature, which is exclusive to clusters
deployed in the single-node zone interconnect mode. Therefore, no-overlay mode
can only be enabled in clusters configured with single-node zone interconnect.

For networks configured with no-overlay mode, the OVN topology requires minor
adjustments. In the current interconnect architecture, a network is able to span
all cluster nodes thanks to the cross-node logical links that OVN establishes
between the transit switch and the ovn-cluster-router instances on each node.
That is the logical representation of the OVN network, and in practice, these
links are implemented through Geneve tunnels between every node in the cluster.
For a network to skip the encapsulation step and route pod traffic directly over
the underlay network, we need to change the logical network topology of this
network: its ovn-cluster-router won't connect to the transit switch anymore and
all east-west traffic will be naturally routed in the same path as north-south
traffic through the provider network.

The following diagram shows the OVN topology of a cluster with three networks:

* a CUDN (in green) that uses Geneve encapsulation (default behavior)  
* a default network (in gray) that is operating in no-overlay mode  
* a CUDN (in blue) that is operating in no-overlay mode

![No-overlay topology](../images/no_overlay_topology.jpg)

We need to update the ZoneInterconnectHandler for no-overlay mode. If a new
network is configured in no-overlay mode, ZoneInterconnectHandler shall not
create the transit switch for the network. For each remote zone, there is no
need to add the static nodeSubnet routes into the ovn_cluster_router. The
traffic shall be forwarded following the source-based route instead. The nexthop
depends on the gateway mode of the cluster.

#### Shared Gateway Mode

At the OVN ClusterRouter, the pod-to-remote_pod egress traffic will be
forwarded to the GatewayRouter via the join switch. Unlike the current route
advertisement behavior, the routes to remote node subnets will be imported to
the gateway router. Once the traffic reaches the GatewayRouter, it will be
forwarded to the nexthop node according to the imported BGP routes.

An example traffic path is as follows (assuming pod IP of 10.128.0.10,
destined to remote pod 10.128.2.11):

```mermaid
sequenceDiagram
    participant Pod1 on Node A
    participant OVN Cluster Router
    participant OVN Gateway Router (GR)
    participant Underlay Physical Network
    participant Node B

    Pod1 on Node A->>OVN Cluster Router: Packet to remote Pod2 (10.128.2.11)
    OVN Cluster Router->>OVN Gateway Router (GR): Forward packet to GR via join switch
    Note over OVN Gateway Router (GR): BGP route lookup
    OVN Gateway Router (GR)->>OVN Gateway Router (GR): Match BGP route: 10.128.2.0/23 via 172.18.0.3 (Node B's IP)
    OVN Gateway Router (GR)->>Underlay Physical Network: Route packet out via br-ex
    Underlay Physical Network->>Node B: Packet arrives at Node B
    Note over Node B: Traffic is delivered to Pod2
```

To maintain SNAT for non-BGP-routed egress traffic, in the gateway router, the
default SNAT rule will be kept. We will add all the BGP routes address prefix
into an address set, and use this address set to bypass SNAT for the pod egress
traffic routed to those destination.

In the future, if any non-BGP routes in the gateway router have prefixes that
overlap with those learned via BGP, we can add these prefixes to another address
set and apply SNAT to traffic routed through them.

In the following example, `10.128.0.0/24` is the local nodeSubnet,
`174.16.1.0/24` is the prefix of a BGP route. `174.16.1.0/25` is the prefix of a
non-BGP route.

```text
# Address_Set contains addresses routed via BGP routers
_uuid               : 44e31651-db99-44f2-ad01-ff1d00bbb223
addresses           : ["174.16.1.0/24"]
external_ids        : {}
name                : skip_snat_dst

# Address_Set contains addresses routed via non-BGP routers
_uuid               : 55e31651-db99-44f2-ad01-ff1d00bbb223
addresses           : ["174.16.1.0/25"]
external_ids        : {}
name                : snat_dst

# SNAT rules in the Gateway Router, exclude traffic to BGP routers
_uuid               : cd9264de-f2c6-4528-8f53-c2ccb5b23c8d
allowed_ext_ips     : []
exempted_ext_ips    : []
external_ids        : {}
external_ip         : "172.18.0.2"
external_mac        : []
external_port_range : ""
gateway_port        : []
logical_ip          : "10.244.1.7"
logical_port        : []
match               : "ip4.dst!=$skip_snat_dst||ip4.dst==$snat_dst"
options             : {stateless="false"}
priority            : 0
type                : snat
```

#### Local Gateway Mode

The pod-to-remote_pod traffic egress traffic will enter the host VRF via the
ovn-k8s-mpX interface. Then it will be forwarded to remote nodes according to
the host routing table of the VRF.

An example traffic path is as follows (assuming pod IP of 10.128.0.10,
destined to remote pod 10.128.2.11):

```mermaid
sequenceDiagram
    participant Pod1 on Node A
    participant OVN Cluster Router (on Node A)
    participant Host VRF/Kernel (on Node A)
    participant Underlay Physical Network
    participant Node B

    Pod1 on Node A->>OVN Cluster Router (on Node A): Packet to remote Pod2 (10.128.2.11)
    OVN Cluster Router (on Node A)->>Host VRF/Kernel (on Node A): Forward packet to host via ovn-k8s-mp0
    Note over Host VRF/Kernel (on Node A): BGP route lookup
    Host VRF/Kernel (on Node A)->>Host VRF/Kernel (on Node A): Match BGP route in host table: 10.128.2.0/23 via 172.18.0.3 (Node B's IP)
    Host VRF/Kernel (on Node A)->>Underlay Physical Network: Route packet out via eth0
    Underlay Physical Network->>Node B: Packet arrives at Node B
    Note over Node B: Traffic is delivered to Pod2
```

To maintain SNAT for non-BGP-routed egress traffic, the masquerade nftables rule
will be kept. The route import controller will insert an nftables rule for each
imported BGP route to bypass the SNAT rule to the gateway router.

If users add custom routes to the host routing table that overlap with BGP route
prefixes and want to retain SNAT for traffic routed through these custom routes,
they must also insert corresponding masquerade rules.

In the following example, `10.128.0.0/24` is the local nodeSubnet,
`174.16.1.0/24` is the CIDR of a BGP route.

```text
table ip nat {
    chain postrouting {
        type nat hook postrouting priority 100; policy accept;
        # skip SNAT for traffic via BGP routers
        ip saddr 10.128.0.0/24 ip daddr 174.16.1.0/24 return
        ip saddr 10.128.0.0/24 masquerade
    }
}
```

#### Import Routes to NodeSubnets

Changes to BGP behavior are necessary to import nodeSubnet routes into the
gateway router and the host routing table of each node. A new field
`encapsulation`, will be introduced to the NAD spec which indicates the
encapsulation of the network. If a network is operating in no-overlay mode, this
field will be set to `none`. This information can then be passed into the
NetInfo object and utilized by both the RouteAdvertisement controller and the
route import controller as described below.

An example OVN-Kubernetes NAD may look like:

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: l3-network
  namespace: default
spec:
  config: |
    {
            "cniVersion": "0.3.1",
            "name": "l3-network",
            "type": "ovn-k8s-cni-overlay",
            "topology":"layer3",
            "mtu": 1500,
            "netAttachDefName": "default/l3-network",
            "role": "primary",
            "encapsulation": "none"
    }
```

##### Gateway Router

Currently, BGP routes to other node subnets are not imported into the gateway
router, which ensures node-to-pod traffic traverses the OVN logical topology.
However, we need the routes to be imported into the gateway router in no-overlay
mode. We need to update the route import controller to check whether a network
is in no-overlay mode and handle the route importing accordingly.

##### Host Routing Tables

To [maintain isolation between UDNs](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/5140),
node BGP speakers do not accept routes to NodeSubnets by default. However, to
route east-west traffic in no-overlay mode, these routes must be imported to the
host routing table. We need to update the node FRRConfiguration generation logic
to check whether a network is in no-overlay mode and generate the node
FRRConfiguration accordingly.

#### No SNAT for egress traffic

The OVN-Kubernetes BGP feature currently disables SNAT only for traffic destined
to external networks, not for inter-pod communication across nodes.

In no-overlay mode, the goal is to fully leverage BGP and eliminate the SNAT
step on the gateway router, even for intra-cluster traffic. Within the same
network, pod-to-pod traffic will utilize BGP-learned routes to reach its
destination node directly, bypassing Geneve encapsulation and SNAT to the
nodeIP.

The only exception is for traffic destined to other nodes in the cluster
(pod-to-other_node), to ensure nodePort services can be accessed across
networks. This egress traffic will remain SNATed to the node IP before leaving
the node. This is the current behavior when a network is advertised. We will not
change it for no-overlay mode.

#### Active traffic blocking between UDNs

The [existing UDN isolation
mechanism](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/5186/commits/8f6e7d30ee5f4926a21e2de75488aad80344814b)
will still be in place. Pods in different UDNs will be isolated from each other
and the default network, utilizing the existing OVN ACLs on every node logical
switch: ACLs are enforced on outgoing traffic at each advertised UDN switch,
verifying whether both source and destination IPs belong to the same advertised
UDN. If the destination IP does not come from the same UDN subnet as the source
IP, traffic is dropped. No-overlay mode will not affect the isolation between
UDNs or between UDNs and the default network. So, even if multiple UDNs were
advertised to the same VRF, the cross-UDN traffic is still blocked. This is the
current behavior when a network is advertised. We will not change it for no
overlay mode.

### Workflow

#### Enable No-overlay Mode for the Default Network

1. The frr-k8s pod shall be deployed on each node.  
2. Before starting the ovnkube pods, the cluster admin shall create a base
   FRRConfiguration CR that is used for generating the per-node FRRConfiguration
   by OVN-K.

    ```yaml
    apiVersion: frrk8s.metallb.io/v1beta1
    kind: FRRConfiguration
    metadata:
      name: default
      namespace: frr-k8s-system
      label:
        # A custom label
        network: default
    spec:
      bgp:
        routers:
        - asn: 64512
          neighbors:
          # the external BGP route reflector
          - address: 172.20.0.2
            asn: 64512
            disableMP: true
    ```

3. The cluster admin shall create a RouteAdvertisements CR to advertise the
   default network. Make sure the RouteAdvertisements CR selects the
   FRRConfiguration defined in the last step.

    ```yaml
    apiVersion: k8s.ovn.org/v1
    kind: RouteAdvertisements
    metadata:
      name: default
    spec:
      advertisements:
      - PodNetwork
      # Select the FRRConfiguration defined in step-2 with the custom label.
      frrConfigurationSelector:
        matchLabels:
          network: default
      networkSelectors:
      - networkSelectionType: DefaultNetwork
      # The empty nodeSelector selects all nodes. We don't support a network in a overlay and no-overlay hybrid mode.
      nodeSelector: {}
    ```

4. The cluster admin enables no-overlay mode by running ovn-kubernetes with the
   configuration flag `--default-network-encapsulation=none` set.  
5. OVN-Kubernetes will generate the following FRRConfiguration for each node.

    ```yaml
    apiVersion: frrk8s.metallb.io/v1beta1
    kind: FRRConfiguration
    metadata:
      name: route-generated-blue
      namespace: frr-k8s-system
    spec:
      bgp:
        routers:
        - asn: 64512
          neighbors:
          - address: 10.89.0.37
            asn: 64512
            disableMP: true
            toAdvertise:
              allowed:
                prefixes:
                # Advertise the local nodeSubnet
                - 10.244.1.0/24
            toReceive:
              # Allow to receive the routes to remote nodeSubnets.
              # This is no-overlay specific behavior.
              allowed:
                mode: filtered
                prefixes:
                - ge: 24
                  le: 24
                  prefix: 10.244.0.0/14
          prefixes:
          - 10.244.1.0/24
    ```

#### Create A ClusterUserDefinedNetwork in No-overlay Mode

Here's a configuration example:

1. The frr-k8s pod shall be deployed on each node.  
2. A cluster admin wants to enable no-overlay mode for the blue network by
   creating the following ClusterUserDefinedNetwork CR.

    ```yaml
    apiVersion: k8s.ovn.org/v1
    kind: ClusterUserDefinedNetwork
    metadata:
      name: blue
      label:
        # A custom label
        network: blue
    spec:
      namespaceSelector:
        matchExpressions:
        - key: kubernetes.io/metadata.name
          operator: In
          values: ["ns1", "ns2"]
      network:
        topology: Layer3
        layer3:
          role: Primary
          # The UDN MTU shall not be larger than the provider network's MTU.
          mtu: 1500
          subnets:
          - cidr: 10.10.0.0/16
            hostSubnet: 24
          encapsulation: "None"
    ```

3. The cluster admin has created an FRRConfiguration CR to peer with an external
   BGP router `182.18.0.5`.

    ```yaml
    apiVersion: frrk8s.metallb.io/v1beta1
    kind: FRRConfiguration
    metadata:
      name: blue
      namespace: frr-k8s-system
      # A custom label
      labels:
        network: blue
    spec:
      bgp:
        routers:
        - asn: 64512
          neighbors:
          - address: 182.18.0.5
            asn: 64512
            disableMP: true
            holdTime: 1m30s
            keepaliveTime: 30s
            passwordSecret: {}
            port: 179
            toAdvertise:
              allowed:
                mode: filtered
            toReceive:
              # Allow to accept the routes to remote nodeSubnets.
              allowed:
                mode: filtered
                prefixes:
                - ge: 24
                  le: 24
                  prefix: 10.10.0.0/16
    ```

4. The cluster admin advertises the CUDN pod network. In this example, the
   `targetVRF` is set to default, meaning routes to the podNetworks will be
   advertised to the default VRF.

    ```yaml
    apiVersion: k8s.ovn.org/v1
    kind: RouteAdvertisements
    metadata:
      name: blue
    spec:
      # advertise routes to the default VRF
      targetVRF: default
      # nodeSelector must be empty, since we don't support a network in a overlay and no-overlay hybrid mode.
      nodeSelector: {}
      frrConfigurationSelector:
        matchLabels:
        # Select the FRRConfiguration defined in step-3
        network: blue
      networkSelectors:
      - networkSelectionType: ClusterUserDefinedNetwork
        clusterUserDefinedNetworkSelector:
          networkSelector:
            matchLabels:
              # Select the CUDN defined in step-2
              network: blue
      advertisements:
      - PodNetwork
    ```

5. OVN-Kubernetes will generate the following FRRConfiguration for each node.

    ```yaml
    apiVersion: frrk8s.metallb.io/v1beta1
    kind: FRRConfiguration
    metadata:
      name: route-generated-blue
      namespace: frr-k8s-system
    spec:
      bgp:
        routers:
        - asn: 64512
          neighbors:
          - address: 182.18.0.5
            disableMP: true
            asn: 64512
            toAdvertise:
              allowed:
                prefixes:
                # the local node subnet
                - 10.10.1.0/24
            toReceive:
              # Allow to receive the routes to remote nodeSubnets.
              # This is no-overlay specific behavior.
              allowed:
                mode: filtered
                prefixes:
                - ge: 24
                  le: 24
                  prefix: 10.10.0.0/16
          prefixes:
          - 10.10.1.0/24
    ```

### Deployment Consideration

#### BGP Topology

The selected `FRRConfiguration` CR determines the deployment mode for the
no-overlay network. There will be no new field added to the FRRConfiguration.
The goal is to exchange the routes to the nodeSubnets across the cluster. The
possible BGP topologies are varied. Users shall be able to choose any topology
that is most suitable for their use-case. However, frr and frr-k8s may not
support some BGP features. Here we list some common deployment options.

##### eBGP Peering with External Routers

A common deployment model is to use eBGP to peer each node with an external BGP
speaker, such as a Top-of-Rack (ToR) switch. This approach is often preferred in
enterprise environments as it provides clear Autonomous System (AS) boundaries
and allows for more advanced routing policies (e.g., AS path prepending, MEDs,
and filtering based on AS paths).

In this topology, the FRR instance on each node establishes an eBGP session with
its upstream router. The upstream routers are then responsible for advertising
the node subnet routes to the rest of the network. It is also possible for
different groups of nodes (e.g. nodes in different racks) to belong to different
Autonomous Systems.

```mermaid
graph LR
    subgraph AS 65002 [AS 65002: Rack 1]
        direction LR
        NodeA(Node A)
        NodeB(Node B)
    end
    subgraph AS 65003 [AS 65003: Rack 2]
        direction LR
        NodeC(Node C)
    end
    subgraph AS 65001 [AS 65001: External Network]
        direction LR
        ToR1(ToR Switch 1)
        ToR2(ToR Switch 2)
    end
    NodeA <-- eBGP --> ToR1
    NodeB <-- eBGP --> ToR1
    NodeC <-- eBGP --> ToR2
    ToR1  <-- iBGP --> ToR2
```

##### Full-Mesh iBGP across the Cluster

The FRR instance on each node maintains a full mesh BGP peer relationship with
all other nodes across the cluster. In this mode, users can enable no-overlay
mode without relying on external BGP routes.

```mermaid
graph LR
    subgraph AS 64512 [ AS 64512 ]
      direction LR
      A(Node A)
      B(Node B)
      C(Node C)
      A <-- iBGP --> B
      B <-- iBGP --> C
      C <-- iBGP --> A
    end
```

Here's an example FRRConfiguration:

```yaml
apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  labels:
    network: default
  name: receive-all
  namespace: frr-k8s-system
spec:
  bgp:
    routers:
    - asn: 64512
      # All nodes are added as neighbors to setup a full-mesh BGP topology
      neighbors:
      - address: 192.168.111.20
        asn: 64512
        disableMP: true
        toReceive:
          allowed:
            mode: all
      - address: 192.168.111.21
        asn: 64512
        disableMP: true
        toReceive:
          allowed:
            mode: all
      - address: 192.168.111.22
        asn: 64512
        disableMP: true
        toReceive:
          allowed:
            mode: all
```

##### External Route Reflectors for Larger Clusters

In a large cluster, a full-mesh BGP setup leads to more CPU and memory
consumption on the nodes. Instead of every node peering with every other node,
nodes peer only with external BGP route reflectors. This significantly reduces
the number of BGP sessions each node needs to maintain, improving scalability.

```mermaid
graph TD
    subgraph AS 64512 [ AS 64512 ]
        direction LR
        subgraph "Route Reflector"
            RR1(Route Reflector 1)
        end
        subgraph "Kubernetes Nodes"
            NodeA(Node A)
            NodeB(Node B)
            NodeC(Node C)
        end
    end
    NodeA <-- iBGP --> RR1
    NodeB <-- iBGP --> RR1
    NodeC <-- iBGP --> RR1
```

##### Internal Route Reflectors

If an FRR node instance can act as a BGP route reflector, it enables the
creation of an internal route reflector-based BGP network. However, it is not
currently supported by frr-k8s, which we use for integrating OVN-K with FRR.
Therefore, this is not a possible deployment today.

### Feature Compatibility

#### Multiple External Gateways (MEG)

The same as the route advertisement feature.

#### Egress IP

Not supported.

#### Services

The same as the route advertisement feature.

#### Egress Service

Not supported.

#### Egress Firewall

Full support.

#### Egress QoS

Full Support.

#### Network Policy/ANP

Full Support.

#### IPsec

Not supported.

### Testing Details

* Unit Testing details
* E2E Testing details
  The E2E test shall cover the following combination matrix:
  * Gateway Modes: LGW and SGW
  * IP Modes: IPv4, IPv6, and dual-stack
  * BGP Topologies: iBGP with an external route reflector, full-mesh iBGP,
    iBGP+eBGP combined and eBGP only.
* API Testing details
* Scale Testing details
* Cross Feature Testing details - coverage for interaction with other features

## Risks, Known Limitations and Mitigations

## OVN Kubernetes Version Skew

To be discussed.

## Alternatives

N/A

## References

1. [OVN-Kubernetes BGP Integration](https://github.com/openshift/enhancements/blob/master/enhancements/network/bgp-ovn-kubernetes.md#no-tunneloverlay-mode)
2. [OKEP-5193: User Defined Network Segmentation](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/docs/okeps/okep-5193-user-defined-networks.md)
