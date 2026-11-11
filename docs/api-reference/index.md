---
title: API Reference Guide
hide:
  - navigation
  - toc
---

# API Reference Guide

Specifications for OVN-Kubernetes Custom Resource Definitions (CRDs).

<div class="landing-grid" markdown>

<div class="landing-card" markdown>

## Introduction

Overview of the OVN-Kubernetes API surface and CRD conventions.

[Introduction](introduction.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## EgressIP

Assign stable source IPs to egress traffic from selected pods.

[EgressIP](egress-ip-api-spec.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## EgressService

Route egress traffic through a Kubernetes Service's load balancer IP.

[EgressService](egress-service-api-spec.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## EgressQoS

Apply DSCP marking rules to egress traffic by namespace.

[EgressQoS](egress-qos-api-spec.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## EgressFirewall

Control which external destinations pods in a namespace can reach.

[EgressFirewall](egress-firewall-api-spec.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## AdminPolicyBasedExternalRoutes

Define cluster-wide external gateway routing policies.

[AdminPolicyBasedExternalRoutes](admin-epbr-api-spec.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## UserDefinedNetwork

Create isolated or connected tenant networks with custom topologies.

[UserDefinedNetwork](userdefinednetwork-api-spec.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## RouteAdvertisements

Advertise pod and service routes to external BGP peers.

[RouteAdvertisements](routeadvertisements-api-spec.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## ClusterNetworkConnect

Enable controlled connectivity between isolated User Defined Networks.

[ClusterNetworkConnect](clusternetworkconnect-api-spec.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## VTEP

Configure Virtual Tunnel Endpoints for external network integration.

[VTEP](vtep-api-spec.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Uplink and UplinkState

Configure physical uplinks for UserDefinedNetworks across DPU, SmartNIC, and non-accelerated deployments.

[Uplink and UplinkState](uplink-api-spec.md){ .landing-btn }

</div>

</div>
