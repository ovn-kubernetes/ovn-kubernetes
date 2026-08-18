# SNAT Exclusion for Egress Traffic

## Introduction

By default, OVN-Kubernetes masquerades (SNATs) pod egress traffic to the node's IP address
as it leaves the cluster. This is correct for most workloads, but some applications — such as
[Submariner](https://submariner.io/) — require the original pod source IP to be preserved for
traffic destined to specific subnets.

OVN-Kubernetes supports a per-node annotation that lets cluster administrators specify subnets
for which SNAT should be skipped, preserving the pod's original source IP for traffic to those
destinations.

## Annotation

```text
k8s.ovn.org/node-ingress-snat-exclude-subnets
```

The annotation accepts a JSON array of CIDR subnets. Traffic from pods on the annotated node
destined to any of the listed subnets will bypass masquerading and leave the cluster with the
pod's original source IP.

## Usage

Annotate a node to exclude one or more subnets from SNAT:

```bash
kubectl annotate node <node-name> \
  k8s.ovn.org/node-ingress-snat-exclude-subnets='["10.132.0.0/14","100.67.0.0/16"]'
```

To remove the exclusion and restore default SNAT behaviour:

```bash
kubectl annotate node <node-name> \
  k8s.ovn.org/node-ingress-snat-exclude-subnets-
```

## Gateway mode support

The annotation is supported in both gateway modes:

| Gateway mode | Supported |
|---|---|
| Shared gateway (default) | ✅ |
| Local gateway | ✅ |

## Use case: Submariner

Submariner connects multiple Kubernetes clusters at the network level and relies on seeing the
pod's real source IP to route return traffic back across clusters. When OVN-K SNATs pod traffic
to the node IP, Submariner loses the pod identity and cross-cluster communication breaks.

The recommended setup is to annotate the active Submariner gateway node with the remote
cluster's pod and service CIDRs:

```bash
kubectl annotate node <gateway-node> \
  k8s.ovn.org/node-ingress-snat-exclude-subnets='["<remote-pod-cidr>","<remote-service-cidr>"]'
```

Submariner's route agent sets this annotation automatically when OVN-Kubernetes is detected as
the CNI.
