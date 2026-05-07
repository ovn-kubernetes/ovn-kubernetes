# External requirements

OVN-Kubernetes has additional dependencies for the external components, here are the recommended (not necessarily minimal)
supported versions.

| OVN-Kubernetes release | OVN release | nft binary | multus | CNI spec | k8s  | 
|------------------------|-------------|------------|--------|----------|------|
| master                 | 25.09       | 1.0.1+     | v4.1.3 | 1.1.0    | 1.35 |
| 1.2                    | 25.09       | 1.0.1+     | v4.1.3 | 0.4.0    | 1.34 |
| 1.1                    | 25.03       | 1.0.1+     | v4.1.3 | 0.4.0    | 1.33 |
| 1.0                    | 24.03       | -          | v4.1.0 | 0.4.0    | 1.29 |

OVN should work with any supported OVS release, extra requirements for OVS version may be specified per-feature

- [OVN releases](https://www.ovn.org/en/releases/all_releases/)
- [OVS release process](https://docs.openvswitch.org/en/latest/internals/release-process/)

Some of the requirements are feature-specific.

## UDN
For kubelet network probes to work with UDN pods, the following are required:
- kernel fix 7f3287db654395f9c5ddd246325ff7889f550286: netfilter: nft_socket: make cgroupsv2 matching work with namespaces)

  - introduced in [6.11](https://cdn.kernel.org/pub/linux/kernel/v6.x/ChangeLog-6.11), backported to 
  [6.10.12](https://cdn.kernel.org/pub/linux/kernel/v6.x/ChangeLog-6.10.12),
  [6.6.53](https://cdn.kernel.org/pub/linux/kernel/v6.x/ChangeLog-6.6.53), and 
  [6.1.112](https://cdn.kernel.org/pub/linux/kernel/v6.x/ChangeLog-6.1.112)
- cgroupv2: required for kubelet probes to work with UDN pods

## BGP, EVPN, and No-Overlay (ENABLE_ROUTE_ADVERTISEMENTS)

- [Route Advertisements feature link](bgp-integration/route-advertisements.md)
- [No-overlay feature link](bgp-integration/no-overlay.md)

| OVN-Kubernetes release | frr-k8s | frr  | 
|------------------------|---------|------|
| master                 | v0.0.21 | 10.4 |
| 1.2                    | v0.0.17 | 9.1  |
| 1.1                    | v0.0.17 | 9.1  |

## OVN Observability

[Feature link](../observability/ovn-observability.md)

OVS 3.4+ and linux kernel 6.11+

## Multi-VTEP

[Feature link](multiple-networks/multi-vtep.md)

OVN version newer than v24.03.2

## OVS acceleration with Kernel datapath

[Prerequisites](hardware-offload/ovs-kernel.md#prerequisites)

- Linux Kernel 5.7.0 or above
- Open vSwitch 2.13 or above
- iproute >= 4.12

## DPU healthcheck support

[DPU healthcheck support OKEP](../okeps/okep-5674-dpu-healthcheck.md)

- multus-CNI >= v4.2.4
- containerd >= v2 or crio >= 1.32
