# SNAT Exclusion via Node Annotation

## Introduction

By default, OVN-Kubernetes masquerades (SNATs) pod traffic to the node's IP address as it
crosses the management port (`ovn-k8s-mp0`). This is correct for most workloads, but some
applications require the original source IP to be preserved for traffic involving specific
subnets.

OVN-Kubernetes supports a per-node annotation that lets cluster administrators specify subnets
for which SNAT should be skipped, preserving the original source IP in both traffic directions.

## Annotation

```text
k8s.ovn.org/node-snat-exclude-subnets
```

The annotation accepts a JSON array of CIDR subnets and controls SNAT exclusion for:

- **Ingress traffic** (remote source → local pod via `ovn-k8s-mp0`): traffic whose source IP
  is in the excluded subnets bypasses SNAT in the `mgmtport-snat` nftables chain, preserving
  the original source IP as it enters OVN.
- **Egress traffic** (local pod → remote destination, local gateway mode only): traffic whose
  destination IP is in the excluded subnets bypasses masquerading in the
  `ovn-kube-pod-subnet-masq` nftables chain, preserving the local pod source IP as it exits
  toward the destination.

> **Note:** The annotation `k8s.ovn.org/node-ingress-snat-exclude-subnets` is also supported
> for backward compatibility but is deprecated. Migrate to
> `k8s.ovn.org/node-snat-exclude-subnets`.

## Usage

Annotate a node to exclude one or more subnets from SNAT:

```bash
kubectl annotate node <node-name> \
  k8s.ovn.org/node-snat-exclude-subnets='["10.132.0.0/14","100.67.0.0/16"]'
```

To remove the exclusion and restore default SNAT behaviour:

```bash
kubectl annotate node <node-name> \
  k8s.ovn.org/node-snat-exclude-subnets-
```

## Example use case

Applications that connect multiple Kubernetes clusters (such as
[Submariner](https://submariner.io/)) rely on preserving the original pod source IP for
cross-cluster traffic. Annotating the gateway node with the remote cluster's pod and service
CIDRs ensures traffic is not SNATed:

```bash
kubectl annotate node <gateway-node> \
  k8s.ovn.org/node-snat-exclude-subnets='["<remote-pod-cidr>","<remote-service-cidr>"]'
```
