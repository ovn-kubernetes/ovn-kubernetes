# OKEP-5535: Support Multi-NIC and Multi-VTEP in OVN-Kubernetes IC Mode

* Issue: [#5535](https://github.com/ovn-org/ovn-kubernetes/issues/5535)

## Problem Statement

OVN-Kubernetes Interconnect (IC) mode currently does not support multi-VTEP setup.
Nodes with multiple SR-IOV NICs cannot correctly encapsulate traffic through the appropriate
VTEP interface, breaking hardware offload and proper routing.

## Goals

* Support multiple VTEPs for nodes with multiple SR-IOV NICs in IC mode
* Ensure correct overlay traffic encapsulation per pod interface based on its underlying VF

## Non-Goals

* Support multi-VTEP on non-SRIOV NICs

## Introduction

In clusters where nodes are equipped with multiple SR-IOV NICs and VTEP interfaces, the OVS
database uses `external_ids:ovn-pf-encap-ip-mapping` to map each SR-IOV PF to its corresponding
encap IP.

In **Central mode**, when a pod is created and its VIF is backed by a VF, the pod’s encap IP is
derived from `external_ids:ovn-pf-encap-ip-mapping`. `ovnkube-node` sets this value on the OVS
interface's `external_ids:ovn-encap-ip`, and `ovn-controller` then updates the corresponding
`Port_Binding.encap` field in the centralized Southbound database, allowing the VIF’s encap IP
to be distributed to all other nodes in the cluster. For more details, see
the [Multi-VTEP Documentation](https://ovn-kubernetes.io/features/multiple-networks/multi-vtep).


In the current **IC mode** implementation:

- For **Layer 3** topology: each Transit Switch includes only a **single remote LSP per remote
  node**, which represents just one of the node's VTEPs. A static route is added to direct all
  traffic destined for the remote node subnet to this single LSP. As a result, **all traffic is
  forwarded through a fixed VTEP**, regardless of which VTEP the traffic should have used.

- For **Layer 2** topology: each remote VIF is represented by its own remote LSP, but because IC
  mode doesn't have a centralized Southbound database, nodes have no visibility into the actual
  `encap-ip` values used by those remote VIFs.


Multi-VTEP support is especially critical for clusters using multiple SR-IOV-capable NICs. VFs are
tied to specific PFs, and hardware offloading requires packets to be routed through the correct PF
(i.e., the correct VTEP IP). This proposal exposes each pod interface’s encap IP and adds the
necessary OVN logical network elements to enable correct multi-VTEP support in IC mode.


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

In the current OVN-Kubernetes implementation, each node exposes a list of its encap IPs via the
`k8s.ovn.org/node-encap-ips` annotation, and the subnets allocated per network through
`k8s.ovn.org/node-subnets`. For example, assume `node-a` has the following annotations:

```
k8s.ovn.org/node-id: "6"
k8s.ovn.org/node-encap-ips: '["10.0.0.1","10.0.0.2","10.0.0.3"]'
k8s.ovn.org/node-subnets: '{
  "default":["10.1.7.0/24"],
  "net-red":["10.2.13.0/24"],
  "net-blue":["10.3.17.0/24"]}'
```

The PF to encap IP mapping in OVS DB is:

```bash
$ ovs-vsctl get open . external_ids:ovn-pf-encap-ip-mapping
"enp1s0f0:10.0.0.1,enp2s0f0:10.0.0.2,enp3s0f0:10.0.0.3"
```

Networks defined via NAD are mapped to PFs as:

| Network  | Cluster Subnet | PF       | Encap IP |
| -------- | -------------- | -------- | -------- |
| default  | 10.1.0.0/16/24 | enp1s0f0 | 10.0.0.1 |
| net-red  | 10.2.0.0/16/24 | enp2s0f0 | 10.0.0.2 |
| net-blue | 10.3.0.0/16/24 | enp3s0f0 | 10.0.0.3 |


In Central mode, a pod VIF’s encap IP is propagated across the cluster via the centralized Southbound
database. However, in IC mode, there is no centralized DB, and the VIF’s encap IP is not available
to remote nodes. This limits the ability to perform correct encapsulation in multi-VTEP setups.

To address this, each pod must expose its interface’s encap IP explicitly in its
`k8s.ovn.org/pod-networks` annotation.

For example, Pod-a on node-a should have the following annotation:
```yaml
k8s.ovn.org/pod-networks: '{
  "default": {
    "ip_addresses": ["10.1.7.35/24"],
    "mac_address": "0a:58:0a:c0:12:23",
    "gateway_ips": ["10.192.18.1"],
    "encap_ip": "10.0.0.1",
    ...
  },
  "default/net-red": {
    "ip_addresses": ["10.2.13.4/24"],
    "mac_address": "0a:58:0a:c2:01:04",
    "encap_ip": "10.0.0.2",
    ...
  },
  "default/net-blue": {
    "ip_addresses": ["10.3.17.5/24"],
    "mac_address": "0a:58:0a:c2:01:05",
    "encap_ip": "10.0.0.3",
    ...
  }
}'
```


#### Layer 2 topology solution

For Layer 2 topology, the remote pod’s VIF is represented by a remote logical switch port (LSP)
in the `layer2_ovn_layer2_switch`.

Assuming `net-blue` is a Layer 2 network, the Pod-a's remote port might look like this on node-b:
```
 switch f99af2b3-8586-460b-940d-98c8adc3b2d2 (ovn.blue.layer2_ovn_layer2_switch)
    port default.ovn.blue_test_pod-a
        type: remote
        addresses: ["0a:58:0a:03:11:05 10.3.17.5"]
```

On node-b, the `ovnkube-controller` should populate the Pod-a's corresponding Port_Binding's
`encap` field for this remote port using the `encap_ip` (`10.0.0.3`) from the pod's
`k8s.ovn.org/pod-networks` annotation.

```
# ovn-sbctl show
Chassis node-a
    hostname: node-a
    Encap geneve
        ip: "10.0.0.1"
        options: {csum="true"}
    Encap geneve
        ip: "10.0.0.2"
        options: {csum="true"}
    Encap geneve
        ip: "10.0.0.3"
        options: {csum="true"}
    Port_Binding default.ovn.blue_test_pod-a  # The Port_Binding of Pod-a's `net-blue` network interface
...

# ovn-sbctl list Port_Binding default.ovn.blue_test_pod-a
_uuid               : 04fd102a-6679-4920-ad6c-663c52df4161
chassis             : a02a2083-6bd9-47f6-ab34-730322756c9e
datapath            : 6413cf4a-838f-481f-b4b9-1ac9ab467cb9
encap               : 4a64dec0-b51c-4e7b-bf0f-629cce23bf42 # set by ovn-controller according to OVS interface external_ids:encap-ip
logical_port        : default.ovn.blue_test_pod-a
tunnel_key          : 11
...

# ovn-sbctl  list encap 4a64dec0-b51c-4e7b-bf0f-629cce23bf42
_uuid               : 4a64dec0-b51c-4e7b-bf0f-629cce23bf42
chassis_name        : node-a
ip                  : "10.0.0.3"
options             : {csum="true"}
type                : geneve
...
```



#### Layer 3 topology solution

In OVN IC mode, Layer 3 networks use Transit Switches to route traffic between nodes. Each remote node is
represented by a remote logical switch port (LSP) in the network’s Transit Switch.

For example, on node-b, the following remote LSPs represent node-a on three different networks:
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

Each Transit Switch includes only one remote LSP for node-a, even though node-a has multiple VTEP interfaces.
Additionally, these LSPs do not have the `encap` field explicitly populated in their corresponding
`Port_Binding` entries.

##### Supporting Multiple Transit Ports Per Encap IP

In the current implementation, a remote Transit Switch port representing a node uses the
node ID(`k8s.ovn.org/node-id`) as tunnel key. This supports only a single VTEP per node.

To support multiple Transit Switch ports per node -- each associated with a specific encap IP, a new annotation
must be introduced:
- `k8s.ovn.org/node-transit-switch-port-tunnel-ids`:  
  Stores the tunnel IDs allocated for each Transit Switch port. These IDs are assigned using the same allocator
  (zoneClusterController.nodeIDAllocator) used for k8s.ovn.org/node-id.
  Since OVN/GENEVE supports a maximum of 32,000 tunnel keys per datapath, having up to 8 VTEPs per node limits
  the maximum cluster size to 4,000 nodes.

When a node with multiple VTEPs joins the cluster, ovnkube-cluster-manager allocates the tunnel ids and add
the annotation to the node. The number of tunnel IDs must same as the number of VTEPs(encap IPs) on the node.
ensuring that each encap IP is mapped to one tunnel ID.

For example, node-a should have following annotations:
```yaml
k8s.ovn.org/node-id: "6"
k8s.ovn.org/node-encap-ips: '["10.0.0.1","10.0.0.2","10.0.0.3"]'
k8s.ovn.org/node-transit-switch-port-tunnel-ids: '[6, 7, 8]'
```

This results in the following Transit Switch port configuration:

| LSP                   | Encap IP   | Tunnel Key  | TSP Address      |
|-----------------------|------------|-------------|------------------|
| tstor-node-a          | 10.0.0.1   | 6           | 100.88.0.6       |
| tstor-node-a_encap1   | 10.0.0.2   | 7           | 100.88.0.7       |
| tstor-node-a_encap2   | 10.0.0.3   | 8           | 100.88.0.8       |


If a node has only one encap IP, the existing Transit Switch port naming convention `tstor-<node-name>` remains.
For multi-VTEP nodes, the naming of the Trans Switch port is determined by the index of encap IP:
- first encap IP use name `tstor-<node-name>`,
- other encap IP use name  `tstor-<node-name>_encap[N]`, N is index of the encap IP.


##### Transit Switch Ports for Local Multi-VTEP Nodes

If the local node has only a single VTEP, the existing behavior remains unchanged.

However, if the local node has multiple VTEPs, the `router` Transit Switch ports representing the local
node’s tunnel endpoints will be created on demand. When a Pod is scheduled on the node, ovnkube-node uses
the VIF's `encap_ip` to determine which Transit Switch port to create. The corresponding Port_Binding's
`encap` field will also be populated accordingly.

Assume all local Transit Switch ports are created on node-a, the default network's OVN Cluster Router
and Transit Switch might look like the following:

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

If the remote node has only one VTEP interface, behavior remains unchanged.

Conversely, if local node has multiple VTEPs, the `remote` Transit Switch port and corresponding static
routes are created on-demand:

- **When the first Pod on the remote node is created**:
  - The local node inspects the Pod’s `encap_ip` and creates a remote Transit Switch port that corresponds to it.
  - A **static route** is added to the cluster router for the **remote node’s subnet**, using the remote Transit
    Switch port's address as the nexthop.

  For example, if a Pod is created on `node-a` with a VIF using `encap_ip: 10.0.0.2`, the local node (`node-b`)
  creates the following remote Transit Switch port:
  ```
  switch 514642eb-9ee5-4026-830c-153923417892 (transit_switch)
      port tstor-node-a_encap1
          type: remote
          addresses: ["0a:58:64:58:00:07 100.88.0.7/16"]
  ```
  A static route is also added to the `ovn_cluster_router`:
  ```
  IPv4 Routes
  Route Table <main>:
        10.1.7.0/24         100.88.0.7 dst-ip
  ```
- **If more Pods are created on the remote node with the same encap IP**, no new remote Transit Switch port
  or routes are needed.
- Later, if another Pod create on node with different encap IP, then a remote Transit Switch port corresponding
  to that encap IP.
- **Later, if a Pod uses a different encap IP**, a new remote Transit Switch port will be created.
  For instance, if a new Pod on `node-a` has the following annotation:
  ```
  k8s.ovn.org/pod-networks: '{
    "default": {
      "ip_addresses": [ "10.1.7.5/24" ],
      "mac_address": "0a:58:0a:01:07:05",
      "gateway_ips": [ "10.1.7.1" ],
      "encap_ip": "10.0.0.3",
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
  And it adds a static route with 10.1.7.5/32 to `ovn_cluster_router`:
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
  Currently `kind`-based E2E test environment doesn't support multi-NIC and multi-VTEP setups.
  Additionally, the multi-VTEP implementation is only supported on SR-IOV NICs.
  Therefore, this feature will be tested in downstream E2E environments that support SR-IOV and hardware offloading.

## References

Multi-VTEP Documentation: https://ovn-kubernetes.io/features/multiple-networks/multi-vtep