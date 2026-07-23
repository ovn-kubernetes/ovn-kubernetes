# External requirements

OVN-Kubernetes has additional dependencies for the external components, here are the recommended (not necessarily minimal)
supported versions.

| OVN-Kubernetes release | OVN release | nft binary | multus | CNI spec | k8s  | 
|------------------------|-------------|------------|--------|----------|------|
| master                 | 26.03       | 1.0.1+     | v4.1.3 | 1.1.0    | 1.35 |
| 1.2                    | 25.09       | 1.0.1+     | v4.1.3 | 0.4.0    | 1.34 |
| 1.1                    | 25.03       | 1.0.1+     | v4.1.3 | 0.4.0    | 1.33 |
| 1.0                    | 24.03       | -          | v4.1.0 | 0.4.0    | 1.29 |

OVN should work with any supported OVS release, extra requirements for OVS version may be specified per-feature

- [OVN releases](https://www.ovn.org/en/releases/all_releases/)
- [OVS release process](https://docs.openvswitch.org/en/latest/internals/release-process/)

Some of the requirements are feature-specific.

## UDN

### Masquerade Subnet Sizing

When UserDefinedNetworks are in use, the `internalMasqueradeSubnet` (IPv4) and
`internalMasqueradeV6Subnet` (IPv6) must be large enough to accommodate the maximum
number of concurrent networks (`MaxNetworks = 4096`).

The masquerade IP index formula (from `go-controller/pkg/generator/udn/masquerade_ips.go`):

```
GatewayRouter  index = 10 + networkID × 2 − 1
ManagementPort index = 10 + networkID × 2
```

For `networkID = MaxNetworks = 4096`, the highest index is `8202`. The subnet must
contain at least `8203` IPs:

| IPv4 Subnet | IPs    | Max safe networkID | Safe with MaxNetworks=4096? |
|-------------|--------|-------------------|----------------------------|
| /29         | 8      | 0                 | NO — cannot fit 1 UDN       |
| /24         | 256    | 123               | NO — crashes after ~123 total creates |
| /20         | 4096   | 2043              | NO — crashes after ~2043 total creates |
| /19         | 8192   | 4091              | NO — max index 8202 > 8192  |
| **/18**     | 16384  | 8187              | **YES — minimum safe size** |
| /17         | 32768  | 16379             | YES — recommended (default for new installs) |

**Recommendation:** Use `/17` for IPv4 (`169.254.0.0/17`). This matches the default
for new installs and provides headroom well beyond `MaxNetworks`. The `/17` range
(`169.254.0.0–169.254.127.255`) is specifically chosen to avoid overlapping with the
cloud metadata IP `169.254.169.254`.

**Note on the 4096 limit:** `MaxNetworks = 4096` is tied to OVN's transit switch
tunnel key reservation. OVN reserves a deterministic key at
`BaseTransitSwitchTunnelKey + networkID` for each network's transit switch, with
exactly 4096 slots reserved at the top of the 24-bit key space. See
[ovn-util.h](https://github.com/ovn-org/ovn/blob/cfaf849c034469502fc97149f20676dec4d76595/lib/ovn-util.h#L159-L164).

**Clusters upgraded from pre-4.17:** The default masquerade subnet prior to OCP 4.17
was `169.254.169.0/29` — only 8 IPs, insufficient for even a single UDN. Clusters
installed before 4.17 and subsequently upgraded retain this default. A day-2
configuration change is required before creating UDNs:

```bash
# IPv4 — change masquerade subnet to /17
kubectl patch networks.operator.openshift.io cluster --type=merge -p \
  '{"spec":{"defaultNetwork":{"ovnKubernetesConfig":{"gatewayConfig":
  {"ipv4":{"internalMasqueradeSubnet":"169.254.0.0/17"}}}}}}'
```

This triggers an ovnkube-node rollout. Once complete, UDN create/delete churn will
not produce "out of range" masquerade IP errors.

### Kubelet Network Probes

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
