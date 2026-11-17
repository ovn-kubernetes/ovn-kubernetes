---
title: Network Security Controls
hide:
  - navigation
  - toc
---

# Network Security Controls

Network policies and firewall rules for controlling traffic in OVN-Kubernetes.

<div class="landing-grid" markdown>

<div class="landing-card" markdown>

## AdminNetworkPolicy

Cluster-scoped network policies that take precedence over namespace-level NetworkPolicy.

[AdminNetworkPolicy](admin-network-policy.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## NetworkPolicy

Standard Kubernetes NetworkPolicy enforcement via OVN ACLs.

[NetworkPolicy](network-policy.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## EgressFirewall

Restrict which external destinations pods in a namespace can reach.

[EgressFirewall](egress-firewall.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## DNS Name Resolution

Integrate EgressFirewall DNS rules with CoreDNS for dynamic address resolution.

[DNS Name Resolution](dns-name-resolution.md){ .landing-btn }

</div>

</div>
