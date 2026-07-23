---
title: API Reference Guide
hide:
  - toc
---

# API Reference Guide

Specifications for OVN-Kubernetes Custom Resource Definitions (CRDs).

**[Introduction](introduction.md)**

Overview of the OVN-Kubernetes API surface and CRD conventions.

**[EgressIP](egress-ip-api-spec.md)**

Assign stable source IPs to egress traffic from selected pods.

**[EgressService](egress-service-api-spec.md)**

Route egress traffic through a Kubernetes Service's load balancer IP.

**[EgressQoS](egress-qos-api-spec.md)**

Apply DSCP marking rules to egress traffic by namespace.

**[EgressFirewall](egress-firewall-api-spec.md)**

Control which external destinations pods in a namespace can reach.

**[AdminPolicyBasedExternalRoutes](admin-epbr-api-spec.md)**

Define cluster-wide external gateway routing policies.

**[UserDefinedNetwork](userdefinednetwork-api-spec.md)**

Create isolated or connected tenant networks with custom topologies.

**[RouteAdvertisements](routeadvertisements-api-spec.md)**

Advertise pod and service routes to external BGP peers.

**[ClusterNetworkConnect](clusternetworkconnect-api-spec.md)**

Enable controlled connectivity between isolated User Defined Networks.

**[VTEP](vtep-api-spec.md)**

Configure Virtual Tunnel Endpoints for external network integration.
