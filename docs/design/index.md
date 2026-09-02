---
title: Overview
hide:
  - toc
---

# Overview

Architecture, design decisions, and traffic flows in OVN-Kubernetes.

**[Architecture](architecture.md)**

Components, pods, and containers that make up an OVN-Kubernetes deployment.

**[Network Topology](topology.md)**

Logical switches, routers, and how they map to the physical cluster.

**[Service Traffic Policy](service-traffic-policy.md)**

Internal and external traffic policy behavior for OVN-backed services.

**[Host To NodePort Hairpin](host-to-node-port-hairpin-trafficflow.md)**

Traffic flow when a node accesses its own NodePort service.

**[ExternalIPs / LoadBalancerIngress](external-ip-and-loadbalancer-ingress.md)**

How external IPs and LoadBalancer ingress addresses are handled.

**[External Bridge (breth0) Flows](bridge-flows.md)**

OpenFlow tables and steering on the external gateway bridge.

**[Masquerade IPs](masquerade-ips.md)**

Node-local masquerade addresses used for SNAT and overlapping UDN subnets.

**[ACLs](acls.md)**

How OVN ACLs implement network policy and related features.

**[EgressIP Design](egressip.md)**

How EgressIP assignment, failover, and GARP handling work.

**[Gateway Accelerated Interface](gateway-accelerated-interface-configuration.md)**

Using a switchdev VF or SF as the gateway interface for hardware acceleration.

**[KubeVirt VM Live Migration](../features/live-migration.md)**

Persistent IPs and networking for KubeVirt VM live migrations.
