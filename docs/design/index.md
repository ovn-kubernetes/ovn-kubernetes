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

**[Gateway Modes](gateway-modes.md)**

Local vs shared gateway modes and how they affect traffic paths.

**[Traffic Flows](traffic-flows.md)**

End-to-end packet paths for pod-to-pod, pod-to-service, and external traffic.

**[Pod Creation Workflow](pod-creation-workflow.md)**

What happens in OVN when a new pod is scheduled on a node.

**[Service Creation Workflow](service-creation-workflow.md)**

How Kubernetes Services are translated into OVN load balancers.

**[Service Traffic Policy](service-traffic-policy.md)**

Internal and external traffic policy behavior for OVN-backed services.

**[Host To NodePort Hairpin](host-to-node-port-hairpin-trafficflow.md)**

Traffic flow when a node accesses its own NodePort service.

**[ExternalIPs / LoadBalancerIngress](external-ip-and-loadbalancer-ingress.md)**

How external IPs and LoadBalancer ingress addresses are handled.

**[Internal Subnets](ovn-kubernetes-subnets.md)**

Subnet allocation for join, transit, and masquerade networks.
