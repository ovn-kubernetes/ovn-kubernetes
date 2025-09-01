# OKEP-5535: Support Multi-NIC and Multi-VTEP in OVN-Kubernetes IC Mode

* Issue: [#5535](https://github.com/ovn-org/ovn-kubernetes/issues/5535)

## Problem Statement

OVN-Kubernetes Interconnect (IC) mode currently does not support multi-VTEP setups.
Nodes with multiple SR-IOV NICs cannot correctly encapsulate traffic through the appropriate
VTEP interface. This breaks hardware offload and proper routing.

## Goals

* Support multiple VTEPs for nodes with multiple SR-IOV NICs in IC mode
* Ensure correct overlay traffic encapsulation per pod interface based on its underlying VF

## Non-Goals

* Support multi-VTEP on non-SR-IOV NICs

## Introduction

In a cluster, when a node has multiple SR-IOV NICs, each PF can serve as a separate VTEP interface,
giving the node multiple encapsulation IPs. These encap IPs must be configured in the Open_vSwitch
table's `external_ids:ovn-encap-ip` in the local OVS database. With multiple encap IPs configured,
ovn-controller creates a distinct tunnel for each one. The mapping from each SR-IOV PF to its
corresponding encap IP is configured via `external_ids:ovn-pf-encap-ip-mapping`.

For example, assume a node has 3 SR-IOV NICs with PFs `enp1s0f0`, `enp2s0f0`, and `enp3s0f0`. Each
PF has an associated bridge interface (`brenp*`) with an IP address used as the encap IP:

```bash
$ ip -br addr
brenp1s0f0           UNKNOWN        10.0.0.2/16
brenp2s0f0           UNKNOWN        10.0.0.3/16
brenp3s0f0           UNKNOWN        10.0.0.4/16

$ ovs-vsctl get open . external_ids:ovn-encap-ip
"10.0.0.2,10.0.0.3,10.0.0.4"

$ ovs-vsctl get open . external_ids:ovn-pf-encap-ip-mapping
"enp1s0f0:10.0.0.2,enp2s0f0:10.0.0.3,enp3s0f0:10.0.0.4"
```

In **Central mode**, when a pod's VIF is backed by a VF, the VIF's encap IP is derived from
`external_ids:ovn-pf-encap-ip-mapping`. `ovnkube-node` sets this value on the OVS interface's
`external_ids:encap-ip`, and `ovn-controller` updates the corresponding `Port_Binding.encap` field
in the centralized Southbound database. This allows each VIF's encap IP to be distributed to all
nodes in the cluster. For details, see the
[Multi-VTEP Documentation](https://ovn-kubernetes.io/features/multiple-networks/multi-vtep).


In the current **IC mode** implementation, multi-VTEP does not work correctly:

- **Layer 3 topology**: Each Transit Switch has only a **single remote LSP per remote node**,
  representing just one of the node's VTEPs. A static route directs all traffic destined for the
  remote node's subnet to this single LSP. As a result, **all traffic goes through one VTEP**,
  regardless of which VTEP should have been used.

- **Layer 2 topology**: Each remote VIF has its own remote LSP, but because IC mode doesn't have a
  centralized Southbound database, nodes cannot see the `encap-ip` values of remote VIFs.

Multi-VTEP support is critical for clusters with multiple SR-IOV NICs. VFs are tied to specific
PFs, and hardware offloading requires packets to be routed through the correct PF (i.e., the
correct VTEP). This proposal exposes each pod interface's encap IP and adds the necessary OVN
logical network elements to enable multi-VTEP support in IC mode.

## User-Stories/Use-Cases

* Story 1: Hardware Offload with SR-IOV in Multi-VTEP Cluster

  As a network administrator, I want each pod’s traffic to be encapsulated using the VTEP IP of the
  PF that owns its associated VF, so that hardware offload works reliably and efficiently in
  multi-NIC environments.

* Story 2: Minimize LSP and Route Bloat in Large-Scale Multi-VTEP Deployments

  As a network administrator, I want the system to create remote LSPs and
  static routes on demand, instead of pre-provisioning them for every possible VTEP, so that the
  control plane remains scalable and efficient in large multi-VTEP clusters.


## Proposed Solution

### API Details

No CRD API changes are required.

### Implementation Details

In the current OVN-Kubernetes implementation, each node exposes its encap IPs via the
`k8s.ovn.org/node-encap-ips` annotation and its L3 network subnets via `k8s.ovn.org/node-subnets`.
For example, `node-a` might have:

```yaml
k8s.ovn.org/node-id: "6"
k8s.ovn.org/node-encap-ips: '["10.0.0.2","10.0.0.3","10.0.0.4"]'
k8s.ovn.org/node-subnets: '{
  "default":["10.1.7.0/24"],
  "net-red":["10.2.13.0/24"],
  "net-blue":["10.3.17.0/24"]}'
```

The PF-to-encap-IP mapping in OVS DB is:
```bash
$ ovs-vsctl get open . external_ids:ovn-pf-encap-ip-mapping
"enp1s0f0:10.0.0.2,enp2s0f0:10.0.0.3,enp3s0f0:10.0.0.4"
```

Networks defined via NAD are mapped to PFs as below:

| Network  | Cluster Subnet | PF       | Encap IP |
| -------- | -------------- | -------- | -------- |
| default  | 10.1.0.0/16/24 | enp1s0f0 | 10.0.0.2 |
| net-red  | 10.2.0.0/16/24 | enp2s0f0 | 10.0.0.3 |
| net-blue | 10.3.0.0/16/24 | enp3s0f0 | 10.0.0.4 |

In Central mode, a pod VIF's encap IP is propagated across the cluster via the centralized
Southbound database. In IC mode, however, there is no centralized database, so the VIF's encap IP
is not visible to remote nodes. This prevents correct encapsulation in multi-VTEP setups.

To address this, each pod must expose its interface's encap IP in its `k8s.ovn.org/pod-networks`
annotation.

When a pod is scheduled on a node and ovnkube-node handles the CNI ADD CMD, it checks whether
the pod's interface is backed by a VF. If so, it derives the encap IP from
`external_ids:ovn-pf-encap-ip-mapping` and adds the `encap_ip` field to the pod's
`k8s.ovn.org/pod-networks` annotation. For example, Pod-a on node-a would have:

```yaml
k8s.ovn.org/pod-networks: '{
  "default": {
    "ip_addresses": ["10.1.7.35/24"],
    "encap_ip": "10.0.0.2",
    ...
  },
  "default/net-red": {
    "ip_addresses": ["10.2.13.4/24"],
    "encap_ip": "10.0.0.3",
    ...
  },
  "default/net-blue": {
    "ip_addresses": ["10.3.17.5/24"],
    "encap_ip": "10.0.0.4",
    ...
  }
}'
```

#### Layer 2 topology solution

For Layer 2 networks, each remote pod's VIF is represented by a remote LSP in the `layer2_ovn_layer2_switch`.
To enable correct encapsulation, the local `ovnkube-controller` must set the
`Port_Binding.encap` field for each remote LSP based on the pod's `encap_ip` annotation.

For example, assume `net-blue` is a Layer 2 network and Pod-a is scheduled on node-a. On node-b,
Pod-a appears as a remote pod with the following remote LSP:

```yaml
 switch f99af2b3-8586-460b-940d-98c8adc3b2d2 (ovn.blue.layer2_ovn_layer2_switch)
    port default.ovn.blue_test_pod-a
        type: remote
        addresses: ["0a:58:0a:03:11:05 10.3.17.5"]
```

When `ovnkube-controller` on node-b processes this remote pod, it reads the `encap_ip` (`10.0.0.4`)
from Pod-a's `k8s.ovn.org/pod-networks` annotation and updates the Port_Binding's `encap` field
to point to the matching Encap entry for node-a:

```bash
$ ovn-sbctl show
Chassis node-a
    hostname: node-a
    Encap geneve
        ip: "10.0.0.2"
        options: {csum="true"}
    Encap geneve
        ip: "10.0.0.3"
        options: {csum="true"}
    Encap geneve
        ip: "10.0.0.4"
        options: {csum="true"}
    Port_Binding default.ovn.blue_test_pod-a
...

$ ovn-sbctl list Port_Binding default.ovn.blue_test_pod-a
_uuid               : 04fd102a-6679-4920-ad6c-663c52df4161
chassis             : a02a2083-6bd9-47f6-ab34-730322756c9e
datapath            : 6413cf4a-838f-481f-b4b9-1ac9ab467cb9
encap               : 4a64dec0-b51c-4e7b-bf0f-629cce23bf42
logical_port        : default.ovn.blue_test_pod-a
tunnel_key          : 11
...

$ ovn-sbctl  list encap 4a64dec0-b51c-4e7b-bf0f-629cce23bf42
_uuid               : 4a64dec0-b51c-4e7b-bf0f-629cce23bf42
chassis_name        : node-a
ip                  : "10.0.0.4"
options             : {csum="true"}
type                : geneve
...
```

With the `encap` field set, node-b's `ovn-controller` knows to use the tunnel with
`remote_ip=10.0.0.4` when sending traffic to Pod-a on `net-blue`.


#### Layer 3 topology solution

In OVN IC mode, Layer 3 networks use Transit Switches to forward traffic between nodes. Each remote
node is represented by a remote LSP in the network's Transit Switch.

For example, on node-b, the following remote LSPs represent node-a on three networks:

```
switch 8d303553-017f-45e4-8fb4-1fe549875eb1 (transit_switch)
   port tstor-node-a
        type: remote
        addresses: ["0a:58:64:58:00:06 100.88.0.6/16"]

switch ef9a91a3-85ec-476f-972c-de4de105ff3c (net.red_transit_switch)
   port net.red_tstor-node-a
        type: remote
        addresses: ["0a:58:64:58:00:06 100.88.0.6/16"]

switch a50d8fc0-b0fd-49b8-b5d0-cd718860bf9d (net.blue_transit_switch)
   port net.blue_tstor-node-a
        type: remote
        addresses: ["0a:58:64:58:00:06 100.88.0.6/16"]
```

Currently, each Transit Switch has only one remote LSP per node, even if that node has multiple
VTEPs. Additionally, these remote LSPs do not have the `encap` field set in their Port_Binding
entries. This means all traffic to a remote node goes through a single VTEP, breaking multi-VTEP
functionality.

##### Supporting Multiple Transit Ports Per Node

In the current implementation, a remote Transit Switch port uses the node ID
(`k8s.ovn.org/node-id`) as its tunnel key. This limits each node to a single VTEP.

To support multiple Transit Switch ports per node -- each associated with a specific encap IP, a new
annotation is introduced:

- **`k8s.ovn.org/node-transit-switch-port-tunnel-ids`**: Stores the tunnel IDs allocated for each
  Transit Switch port. These IDs are assigned by the same allocator
  (`zoneClusterController.nodeIDAllocator`) used for `k8s.ovn.org/node-id`. Since OVN/GENEVE
  supports a maximum of 32,000 tunnel keys per datapath, allowing up to 8 VTEPs per node limits
  the maximum cluster size to 4,000 nodes.

When a multi-VTEP node joins the cluster, `ovnkube-cluster-manager` allocates tunnel IDs and sets
this annotation. The number of tunnel IDs matches the number of encap IPs, ensuring a one-to-one
mapping between each encap IP and its tunnel ID.

For example, node-a would have the following annotations:
```yaml
k8s.ovn.org/node-id: "6"
k8s.ovn.org/node-encap-ips: '["10.0.0.2","10.0.0.3","10.0.0.4"]'
k8s.ovn.org/node-transit-switch-port-tunnel-ids: '[6,7,8]'
```

This results in three Transit Switch ports, one per encap IP:

| LSP                   | Encap IP   | Tunnel Key  | TSP Address      |
|-----------------------|------------|-------------|------------------|
| tstor-node-a          | 10.0.0.2   | 6           | 100.88.0.6       |
| tstor-node-a_encap1   | 10.0.0.3   | 7           | 100.88.0.7       |
| tstor-node-a_encap2   | 10.0.0.4   | 8           | 100.88.0.8       |

For single-VTEP nodes, the existing Transit Switch port naming convention `tstor-<node-name>` is unchanged.
For multi-VTEP nodes, the Transit Switch port name depends on the encap IP index:
- first encap IP uses the name `tstor-<node-name>`,
- other encap IPs use the name `tstor-<node-name>_encap[N]`, N is index of the encap IP.

##### Transit Switch Ports for Local Multi-VTEP Nodes

For single-VTEP nodes, the existing behavior remains unchanged.

For multi-VTEP nodes, the `router` Transit Switch ports representing the local
node’s tunnel endpoints will be created on demand. When a pod is
scheduled on the node, `ovnkube-controller` reads the VIF's `encap_ip` from the pod annotation and
creates the corresponding Transit Switch port if it doesn't exist. The Port_Binding's `encap`
field is also set accordingly.

For example, if all three encap IPs have been used by pods on node-a, the default network's
cluster router and Transit Switch would look like:

```
router 91efa770-d146-428e-bae0-5a394dbbcdc8 (ovn_cluster_router)
    port rtots-node-a
        mac: "0a:58:64:58:00:06"
        ipv6-lla: "fe80::858:64ff:fe58:6"
        networks: ["100.88.0.6/16"]
    port rtots-node-a_encap1
        mac: "0a:58:64:58:00:07"
        ipv6-lla: "fe80::858:64ff:fe58:7"
        networks: ["100.88.0.7/16"]
    port rtots-node-a_encap2
        mac: "0a:58:64:58:00:08"
        ipv6-lla: "fe80::858:64ff:fe58:8"
        networks: ["100.88.0.8/16"]

switch fe6b6dec-b8a3-4199-b6f6-f64d02389f28 (transit_switch)
    port tstor-node-a
        type: router
        router-port: rtots-node-a
    port tstor-node-a_encap1
        type: router
        router-port: rtots-node-a_encap1
    port tstor-node-a_encap2
        type: router
        router-port: rtots-node-a_encap2
```

##### Transit Switch Ports for Remote Multi-VTEP Nodes

For single-VTEP nodes, the existing behavior remains unchanged.

For multi-VTEP remote nodes, the `remote` Transit Switch port and corresponding static
routes are created on-demand:

- **When the first Pod on the remote node is created**:
  - The local `ovnkube-controller` reads the pod's `encap_ip`
  from `k8s.ovn.org/pod-networks` and creates a remote Transit Switch port for that encap IP.
  - A **static route** is added to the cluster router for the **remote node’s subnet**, using the remote Transit
    Switch port's address as the nexthop.

  For example, if a Pod is scheduled on `node-a` with `encap_ip: 10.0.0.3`, the local node (`node-b`)
  creates the following remote Transit Switch port:
  ```
  switch 514642eb-9ee5-4026-830c-153923417892 (transit_switch)
      port tstor-node-a_encap1
          type: remote
          addresses: ["0a:58:64:58:00:07 100.88.0.7/16"]
  ```
  A static route is added to the `ovn_cluster_router`:
  ```
  IPv4 Routes
  Route Table <main>:
        10.1.7.0/24         100.88.0.7 dst-ip
  ```
- **If more Pods are created on the remote node with the same encap IP**, no new remote Transit Switch port
  or static routes are needed.
- **Later, if another Pod is created on the node with a different encap IP**, then a new remote Transit Switch port corresponding
  to that encap IP will be created.
  For example, if a new Pod on `node-a` has the following annotation:
  ```
  k8s.ovn.org/pod-networks: '{
    "default": {
      "ip_addresses": [ "10.1.7.5/24" ],
      "gateway_ips": [ "10.1.7.1" ],
      "encap_ip": "10.0.0.4",
      ...
    },

  ```
  Then node-b creates the second remote Transit Switch port:
  ```
  switch 514642eb-9ee5-4026-830c-153923417892 (transit_switch)
      port tstor-node-a_encap1
          type: remote
          addresses: ["0a:58:64:58:00:07 100.88.0.7/16"]
      port tstor-node-a_encap2
          type: remote
          addresses: ["0a:58:64:58:00:08 100.88.0.8/16"]

  ```
  And it adds a /32 static route to `ovn_cluster_router`:
  ```
  IPv4 Routes
  Route Table <main>:
        10.1.7.5/32         100.88.0.8 dst-ip
        10.1.7.0/24         100.88.0.7 dst-ip

  ```

### Testing Details

* Unit Testing details
  - Validate that newly introduced node annotations are correctly applied:
    - `k8s.ovn.org/node-transit-switch-port-tunnel-ids`
  - Verify that Pod annotations include the correct `encap_ip` values.
  - Confirm that Transit Switch ports are created on demand based on Pod `encap_ip`.
    Ensure that `Port_Binding.encap` fields are correctly set for both local and remote ports.
  - Verify that the appropriate static routes are added to the OVN Cluster Router:
    - `/24` route added for the first Pod on a given remote encap IP.
    - `/32` route added for additional Pods using different encap IPs.

* E2E Testing details
  Currently `kind`-based E2E test environment doesn't support multi-VTEP testing because:
  - `kind` nodes have only a single NIC/VTEP interface
  - `kind` uses veth for pod interfaces, not SR-IOV VFs, so there is no way to derive `encap_ip`
    from `external_ids:ovn-pf-encap-ip-mapping`

  To enable multi-VTEP E2E testing in `kind`, the following changes are made:
  - Configure `kind` nodes with a second VTEP interface and set up source-based routing policy
    for multi-VTEP traffic
  - Introduce a test annotation `test.k8s.ovn.org/pod-network-encap-ip-mapping` that allows E2E
    tests to specify the `encap_ip` for veth-based pod interfaces. The format is:
    `{"<network-name>": "<encap-ip>", ...}`

  The E2E test cases cover the following scenarios:

  | Network Type     | Topology |
  |------------------|----------|
  | default          | Layer 3  |
  | Primary UDN      | Layer 3  |
  | Primary UDN      | Layer 2  |
  | Secondary UDN    | Layer 3  |
  | Secondary UDN    | Layer 2  |

  For each senario, the following cases are tested:
  - Basic VTEP connectivity: 1st VTEP to 1st VTEP, 2nd VTEP to 2nd VTEP
  - Two remote pods on same VTEP (verifies /24 node subnet routing)
  - Two remote pods on different VTEPs (verifies /32 host route for 2nd encap IP)
  - Two local pods on different VTEPs with one remote pod
  - Two local pods on different VTEPs with two remote pods on different VTEPs

  Each test case uses tcpdump to capture Geneve-encapsulated traffic on VTEP interfaces and
  verifies that packets are sent through the correct VTEP based on the pod's `encap_ip`.

  Additionally, this feature will be tested in downstream E2E test environments that support SR-IOV
  and hardware offloading to verify the multi-VTEP functionality on physical SR-IOV NICs.

## References

Multi-VTEP Documentation: https://ovn-kubernetes.io/features/multiple-networks/multi-vtep