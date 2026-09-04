# DHCP IPAM for Localnet Networks

## Introduction

A [`Localnet` network](user-defined-networks.md#ovn-kubernetes-implementation-details)
attaches pods and VMs directly to a physical network in the provider's
infrastructure. For IP addressing on such a network, `Localnet` offers three
IPAM modes:

* `Enabled`: OVN-Kubernetes allocates addresses from a statically configured
  `subnets` pool.
* `Disabled`: addressing is left entirely to the workload.
* `DHCP`: IP assignment is delegated to an external DHCP server managing the
  physical network, and OVN-Kubernetes learns the assigned addresses instead
  of allocating them itself.

This document covers the `DHCP` mode.

Lease handling depends on the workload type, selected automatically by
OVN-Kubernetes:

* **Pods** use the standard [DHCP CNI IPAM plugin](https://www.cni.dev/plugins/current/ipam/dhcp/)
  on the node, which acquires the lease and maintains it (renewals for the
  pod's lifetime, and a release attempt on pod deletion).
* **KubeVirt VMs** (including VFIO passthrough) run a DHCP client inside the
  guest OS, which owns the lease. OVN-Kubernetes only discovers and reports
  the initial IP so that features relying on the pod IP keep working.

## Enabling the Feature

`DHCP` IPAM mode does not have its own feature flag. It is available whenever
`Localnet` secondary networks are, i.e. with `--enable-multi-network` set. No
additional flag is required.

For the pod delegation path, the DHCP CNI IPAM plugin must be present on the
node:

* `/opt/cni/bin/dhcp` and a running `/opt/cni/bin/dhcp daemon` on every node
  that will host pods on a `DHCP` IPAM localnet network.
* `ovnkube-node` must have `/opt/cni/bin` and `/run/cni` mounted in from the
  host, so it can exec the `dhcp` plugin binary and reach the daemon's RPC
  socket.

KubeVirt VMs do not need the DHCP CNI plugin installed as the one-shot
discovery is performed by OVN-Kubernetes itself.

## Supported Scope

This feature currently supports:

* `Localnet` topology, `Secondary` role networks (the only role `Localnet`
  supports).
* IPv4 only.
* Regular pods, delegated to the DHCP CNI plugin daemon.
* KubeVirt VMs, including VFIO passthrough, `l2bridge`, and `managedTap`
  bindings, via one-shot DHCP discovery.
* Features that rely on the pod IP, such as [MultiNetworkPolicy](../multiple-networks/multi-network-policies.md)
  and [NetworkQoS](../network-qos/overview.md).
* KubeVirt VM cold migration (stopping a VM and starting it on another node).

This feature does not currently support:

* `Layer2` topology.
* KubeVirt VM live migration.
* IPv6 or dual-stack — the upstream DHCP CNI plugin and the one-shot VM
  discovery both speak DHCPv4 only.
* Combining `DHCP` mode with `subnets` — `subnets` must be omitted when
  `ipam.mode` is `DHCP`.

## API Overview

Set `spec.network.localnet.ipam.mode` to `DHCP` on a `Secondary` role
`Localnet` CUDN:

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-dhcp-ipam
spec:
  namespaceSelector:
    matchLabels:
      localnet: "true"
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: physnet
      ipam:
        mode: DHCP
```

The CUDN controller renders a `NetworkAttachmentDefinition` in every selected
namespace. With `ipam.mode: DHCP`, the generated NAD config carries an
explicit `ipam` section selecting DHCP addressing. The same NAD serves both
workload types: pods are delegated to the DHCP CNI plugin, while KubeVirt VMs
go through the one-shot discovery described above:

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: localnet-dhcp-ipam
  namespace: test-localnet-dhcp-ipam
  labels:
    k8s.ovn.org/user-defined-network: ""
spec:
  config: |
    {
      "cniVersion": "1.1.0",
      "type": "ovn-k8s-cni-overlay",
      "name": "cluster_udn_localnet-dhcp-ipam",
      "netAttachDefName": "test-localnet-dhcp-ipam/localnet-dhcp-ipam",
      "topology": "localnet",
      "role": "secondary",
      "physicalNetworkName": "physnet",
      "ipam": { "type": "dhcp" }
    }
```

Attach a pod or VM to the network the same way as any other secondary UDN, via
the `k8s.v1.cni.cncf.io/networks` annotation (or the KubeVirt equivalent for
VMs). No further per-pod configuration is required: ovnkube-node detects
`ipam.type: "dhcp"` in the NAD config automatically on CNI ADD.

## Troubleshooting

Check the DHCP-learned IP recorded on the pod. The entry for the `DHCP`
network carries an additional `ipam_mode: "dhcp"` field marking the address as
externally assigned rather than allocated by OVN-Kubernetes:

```bash
kubectl get pod test-localnet-pod -n test-localnet-dhcp-ipam \
  -o jsonpath='{.metadata.annotations.k8s\.ovn\.org/pod-networks}' | jq
```

```json
{
  "default": {
    "ip_addresses": ["10.192.89.6/26"],
    "mac_address": "0a:58:0a:c0:59:06",
    "gateway_ips": ["10.192.89.1"],
    "role": "primary"
  },
  "test-localnet-dhcp-ipam/localnet-dhcp-ipam": {
    "ip_addresses": ["172.18.0.242/16"],
    "mac_address": "7e:b3:9b:ea:06:bd",
    "gateway_ips": ["172.18.0.5"],
    "role": "secondary",
    "ipam_mode": "dhcp"
  }
}
```

The multus `k8s.v1.cni.cncf.io/network-status` annotation reports the same IP
along with the pod interface the network was attached on:

```json
[
    {
        "name": "test-localnet-dhcp-ipam/localnet-dhcp-ipam",
        "interface": "net1",
        "ips": [
            "172.18.0.242"
        ],
        "mac": "7e:b3:9b:ea:06:bd",
        "dns": {}
    }
]
```

For regular pods, the DHCP-assigned IP is also configured directly on that
interface by the OVN-Kubernetes CNI and can be confirmed from inside the pod:

```bash
kubectl exec test-localnet-pod -n test-localnet-dhcp-ipam -- ip -4 addr show net1
```

```text
3: net1@if52142: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UP group default qlen 1000
    link/ether 7e:b3:9b:ea:06:bd brd ff:ff:ff:ff:ff:ff link-netnsid 0
    inet 172.18.0.242/16 brd 172.18.255.255 scope global net1
       valid_lft forever preferred_lft forever
```

For KubeVirt VMs the annotation is populated the same way, and the guest's
own DHCP client configures the IP inside the VM.

Common problems:

* The pod is stuck in `ContainerCreating` and the CNI ADD fails for the
  network: check that the `dhcp daemon` is running on the node and that its
  socket (`/run/cni/dhcp.sock`) is mounted into `ovnkube-node` — for the pod
  path, ovnkube-node must be able to exec `/opt/cni/bin/dhcp` and reach the
  daemon.
* CNI ADD times out waiting for a lease: verify a DHCP server is actually
  reachable on the physical network mapped by `physicalNetworkName`, e.g. run
  a DHCP client or `tcpdump` port 67/68 on the mapped bridge on that node.
* The pod comes up but the annotation entry is missing `ipam_mode: "dhcp"`:
  the NAD in the pod's namespace does not carry `ipam: {"type": "dhcp"}` —
  confirm the CUDN has `ipam.mode: DHCP` and the NAD was re-rendered.

## Known Limitations

* **DHCP RELEASE with dnsmasq (pod path).** The upstream DHCP CNI plugin
  sends RELEASE packets from source IP `0.0.0.0` instead of the client's
  assigned IP, which `dnsmasq` drops. The lease simply expires naturally on
  the server side instead of being released early.
* **Lease loss on DHCP daemon restart (pod path).** The DHCP CNI daemon
  keeps leases in memory only; a daemon restart loses all active leases and
  no further renewals happen for pods relying on it. As a mitigation,
  configure the DHCP server with static MAC-to-IP reservations, and restart
  affected pods to trigger a fresh DORA that re-registers with the running
  daemon.

## References

* [User Defined Networks](user-defined-networks.md)
* [DHCP IPAM for Localnet Networks enhancement](../../okeps/okep-6224-dhcp-ipam-localnet.md)
