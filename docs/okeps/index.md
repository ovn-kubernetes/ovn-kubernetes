---
title: Enhancement Proposals
hide:
  - navigation
  - toc
---

# Enhancement Proposals

OVN-Kubernetes Enhancement Proposals (OKEPs) describe design and implementation
plans for new features.

<div class="landing-grid" markdown>

<div class="landing-card" markdown>

## User Defined Networks

Bring primary network flexibility with tenant isolation and custom topologies.

[User Defined Networks](okep-5193-user-defined-networks.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Connecting UDNs

Connect isolated User Defined Networks together for controlled inter-UDN communication.

[Connecting UDNs](okep-5224-connecting-udns/okep-5224-connecting-udns.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Preconfigured UDN Addresses

Predefined static IP, MAC, and gateway for migrating legacy workloads to UDNs.

[Preconfigured UDN Addresses](okep-5233-preconfigured-udn-addresses.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Dynamic UDN Node Allocation

Render UDN topologies only on nodes where they are needed for better scalability.

[Dynamic UDN Node Allocation](okep-5552-dynamic-udn-node-allocation.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Multiple Cluster Subnets

Extend Layer 3 UDNs to support multiple cluster subnets per IP family.

[Multiple Cluster Subnets](okep-5377-extend-udn-to-support-multiple-cluster-subnets-in-layer3-topology.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Localnet API

Create localnet topology networks via the managed CUDN CRD with early validation.

[Localnet API](okep-5085-localnet-api.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Layer 2 Transit Router

Improve primary UDN Layer 2 topology with a transit router for robust live migration.

[Layer 2 Transit Router](okep-5094-layer2-transit-router.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## BGP Integration

Native BGP routing protocol integration for route advertisements and peering.

[BGP Integration](okep-5296-bgp.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## No-Overlay Mode

Direct routing between nodes via BGP, eliminating Geneve encapsulation overhead.

[No-Overlay Mode](okep-5259-no-overlay.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## EVPN

Expose UDNs externally via BGP+EVPN for standardized Layer 2/3 VPN integration.

[EVPN](okep-5088-evpn.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## VRF-Lite Shared Gateway Mode

VRF separation in shared gateway mode using managed Uplink resources.

[VRF-Lite Shared Gateway Mode](okep-6019-vrf-lite-shared-gateway-external-bridges.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Network QoS

DSCP marking and bandwidth shaping for differentiated traffic handling.

[Network QoS](okep-4380-network-qos.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Disable Port Security

Disable MAC spoof protection on secondary networks for nested virtualization and NFV.

[Disable Port Security](okep-3926-disable-port-security.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## EgressIP Node Selector

Per-object node selector to control which nodes host a specific EgressIP.

[EgressIP Node Selector](okep-6800-egressip-node-selector.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## MCP for Troubleshooting

Model Context Protocol server to accelerate multi-layer network debugging.

[MCP for Troubleshooting](okep-5494-ovn-kubernetes-mcp-server.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## OVN Observability API

Fine-grained CRD for configuring OVN sampling and network visibility.

[OVN Observability API](okep-5212-ovnobserv-api.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## DPU Healthcheck

Detect unhealthy DPUs and mark nodes as not-ready for pod scheduling.

[DPU Healthcheck](okep-5674-dpu-healthcheck.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## DHCP IPAM for Localnet

DHCP-based IPAM for localnet secondary networks.

[DHCP IPAM for Localnet](okep-6224-dhcp-ipam-localnet.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## NetworkQoS for IPAM-less Localnet

Add NetworkQoS support for IPAM-less localnet networks.

[NetworkQoS for IPAM-less Localnet](okep-6815-network-qos-ipamless.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Scalable ARP/NDP Broadcast for UDN

Scalable ARP/NDP broadcast handling for User-Defined Networks.

[Scalable ARP/NDP Broadcast for UDN](okep-6691-scalable-arp-ndp-broadcast-udn.md){ .landing-btn }

</div>

</div>
