---
title: Features
hide:
  - toc
---

# Features

Networking capabilities provided by OVN-Kubernetes.

**[Universal Connectivity](user-defined-networks/index.md)**

User-defined networks, network segmentation, and cross-network connectivity.

**[Network Security Controls](network-security-controls/index.md)**

NetworkPolicy, AdminNetworkPolicy, and EgressFirewall enforcement via OVN ACLs.

**[Cluster Egress Controls](cluster-egress-controls/index.md)**

Control how traffic leaves the cluster with EgressIP, EgressService, and EgressQoS.

**[Infrastructure Security Controls](infrastructure-security-controls/index.md)**

Node-level identity and security mechanisms.

**[Multi-Networking](multiple-networks/index.md)**

Attach pods to multiple networks with multi-homing and multi-network policies.

**[Multicast](multicast.md)**

IGMP snooping and multicast relay support via OVN.

**[NetworkQoS](network-qos.md)**

DSCP marking and traffic shaping for pod network traffic.

**[Live Migration](live-migration.md)**

Persistent IPs and seamless networking for KubeVirt VM live migrations.

**[Hybrid Overlay](hybrid-overlay.md)**

Mixed Windows/Linux cluster networking using VXLAN tunnels.

**[OVS Dynamic CPU Affinity](ovs-dynamic-cpu-affinity.md)**

Automatically adjust OVS datapath thread CPU pinning based on load.

**[Hardware Acceleration](hardware-offload/index.md)**

Offload OVS datapath processing to SmartNICs and DPUs.

**[BGP Integration](bgp-integration/index.md)**

Route advertisements, no-overlay routing, and EVPN support via BGP.
