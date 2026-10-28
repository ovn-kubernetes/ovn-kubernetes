# Gateway Modes

OVN-Kubernetes supports two gateway modes that control how north-south
(pod ↔ external) traffic leaves the cluster at each node. The mode is
set cluster-wide via `--gateway-mode`.

## Shared Gateway Mode (`shared`)

In shared gateway mode the node's external IP address is **shared**
between the host and OVN. Pod egress traffic is routed entirely through
OVN — the Gateway Router (GR) performs SNAT to the node IP and sends
packets directly out the physical uplink via the external bridge
(br-ex). The host kernel is not in the egress data path.

This is the default mode in standard OVN-Kubernetes deployments/manifests.
At the binary CLI level, omitting `--gateway-mode` leaves gateway functionality disabled.

## Local Gateway Mode (`local`)

In local gateway mode (default network), pod egress traffic exits the OVN br-int bridge
but is delivered to the **host kernel** (via the management port, ovn-k8s-mp0) instead
of going directly to the physical NIC. The host's routing table and
iptables/nftables masquerading handle the final hop out. This gives the
host full control over outbound path selection.

Note: For some no-overlay CUDN paths, OVN network-scoped router SNAT-exemption/NAT
objects also participate, so the exact SNAT mechanism is network-type dependent.

## Pod Egress Routing on the Cluster Router

In **shared** gateway mode, for Layer 3 networks whose east-west traffic goes
through the interconnect overlay, the cluster router (`ovn_cluster_router`)
carries a single default route per IP family that sends pod egress to the local
gateway router:

```text
0.0.0.0/0  dst-ip  100.64.0.2   # via GR_<node>
```

A default route is enough because a zone's cluster router only carries that
zone's subnets, so one route covers every pod in the zone. It also avoids a
dependency on how OVN orders routes: OVN ranks source-IP routes against
destination-IP routes by comparing prefix lengths across the two fields, so a
node subnet source-IP route outranks the destination-IP route to a remote node
subnet whenever the local node subnet mask is longer than the remote one, which
would send east-west traffic out through the node's gateway to be SNATed. A
default route cannot outrank anything, so the connected routes for local node
subnets, and the routes to remote node subnets and remote gateway router IPs,
all keep winning longest-prefix-match.

Everything else keeps one source-IP route per node subnet. In **local** gateway
mode it points at the management port, putting the host kernel in the egress
path; for the EVPN and no-overlay transports and for Layer 2 topologies it
points at the gateway router:

```text
10.244.1.0/24  src-ip  10.244.1.2   # local, via ovn-k8s-mp0
10.244.1.0/24  src-ip  100.64.0.2   # EVPN, no-overlay or Layer 2, via GR_<node>
```

In local gateway mode the source-IP route is load-bearing: it has to outrank the
less specific cluster subnet source-IP routes that point at the gateway router,
which is how pod egress is forced through the host kernel. A default route is
too weak to win that comparison.

## When to Use Which

| Consideration | Shared | Local |
|---|---|---|
| Simplicity | Simpler — fewer moving parts, no host iptables dependency for egress | More iptables/nftables rules on the host |
| Egress path control | OVN default route; BGP/Route Advertisements and MEG can steer traffic to dynamic next hops | Full control via host routing table, VRFs, policy routing; BGP/Route Advertisements also supported |
| VLAN tagging on external port | Supported (`--gateway-vlanid`) | Supported |
| Custom egress routing / VRFs | No | Yes |
| OVS offload (max throughput) | Yes — required for full OVS offload | No |

Choose **shared** when you want a straightforward setup and don't need
host-level routing control. Choose **local** when you need advanced
egress routing, VRF isolation, or EVPN integration.

## Configuration

```bash
--gateway-mode=shared   # deployment default (when enabled by tooling/manifests)
--gateway-mode=local
```

Helm:

```yaml
global:
  gatewayMode: "shared"
```

KinD:

```bash
./kind.sh --gateway-mode shared
./kind.sh --gateway-mode local
```
