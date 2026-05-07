# Dynamic UDN Node Allocation

## Introduction

By default, OVN-Kubernetes renders each UDN on every node. Rendering a UDN
means that ovnkube-controller and ovnkube-node create the per-network OVN and
host-side state needed for that network on the node.

Dynamic UDN Node Allocation changes this behavior for UDN and CUDN managed
networks. When the feature is enabled, OVN-Kubernetes only renders a UDN on
nodes that are actively using it. A node becomes active for a UDN when one of
the following is true:

* A scheduled pod on the node attaches to a CUDN or UDN.
* The node is assigned as an EgressIP node for that UDN.
* A UDN is connected via the [Cluster Network Connect (CNC)](cluster-network-connect.md)
  feature to another UDN that meets either of the previously stated criteria.

When a node is inactive for a UDN, OVN-Kubernetes does not run the per-network
controllers or create the per-node OVN and host networking state for that UDN
on that node. Per-node allocations, such as Layer3 host subnets and Layer2
tunnel IDs, are also delayed until the node becomes active for the network.

## Why use Dynamic UDN?

Dynamic UDN is useful for clusters with many UDNs where most nodes only need a
small subset of those networks. For example, a cluster admin might dedicate
groups of worker nodes to different tenants and create one CUDN per tenant. In
that model, each tenant network only needs to exist on the nodes where that
tenant's workloads can run.

This can reduce the amount of OVN-Kubernetes work on each node, lower the
number of unused per-node network objects, and improve scale when a cluster has
many UDNs. It can also help IP planning for Layer3 UDNs because host subnets are
allocated only for active nodes instead of every node in the cluster.

## Enabling Dynamic UDN

Dynamic UDN is disabled by default. Enable it with the
`--enable-dynamic-udn-allocation` feature config option:

```text
--enable-network-segmentation=true
--enable-multi-network=true
--enable-dynamic-udn-allocation=true
```

Dynamic UDN requires Network Segmentation. OVN-Kubernetes will reject a
configuration that enables `--enable-dynamic-udn-allocation` without also
enabling `--enable-network-segmentation`. UDNs also depend on the multi-network
feature because the UDN controllers create NetworkAttachmentDefinitions
under the hood.

When a node becomes inactive for a UDN, OVN-Kubernetes does not remove the
network from that node immediately. A node is inactive for a UDN when it has no
primary or secondary pod attachments and no EgressIP assignment for that UDN.
OVN-Kubernetes waits for `--udn-deletion-grace-period` before tearing down the
inactive network state. The default grace period is `120s` and is applied from
the moment the node becomes inactive for that UDN. Increasing the value reduces
churn when workloads move quickly; reducing it cleans up inactive node state
sooner.

## Observing Dynamic UDN

UDN and CUDN status includes a `NodesSelected` condition when Dynamic UDN is
enabled. The condition reports whether any nodes currently render the network
and includes the current rendered node count:

```yaml
status:
  conditions:
  - type: NodesSelected
    status: "True"
    reason: DynamicAllocation
    message: "5 node(s) rendered with network"
```

If no nodes currently render the network, the condition is reported with
`status: "False"` and the message `no nodes currently rendered with network`.

The cluster manager also exposes the
`ovnkube_clustermanager_udn_nodes_rendered` metric with a `network_name` label.
This metric tracks the number of nodes where each UDN or CUDN is currently
rendered.

## Limitations

Dynamic UDN is a scale optimization, not a security boundary. It should not be
used as the only mechanism to prevent a tenant network from appearing on a
node. Use normal Kubernetes scheduling controls, node isolation, and policy for
that purpose.

The first pod for a UDN on an inactive node can take longer to start because
OVN-Kubernetes must render the UDN and allocate any required per-node network
state before the pod can be wired.

The feature applies globally to UDN and CUDN managed networks. It is not
currently configured on a per-UDN basis. Only NetworkAttachmentDefinitions
owned by a UDN or CUDN are activity-gated; standalone NetworkAttachmentDefinitions
are treated as present on all nodes.

Externally exposed UDN services with `externalTrafficPolicy: Cluster` can fail
when external traffic is sent to a node where the UDN is inactive. Prefer
`externalTrafficPolicy: Local` for externally exposed UDN services when
possible. For LoadBalancer services, Kubernetes health checks only pass on nodes
with local ready endpoints; those local endpoint pods make the UDN active on
the node, so load balancers that honor the health check will target nodes where
Dynamic UDN has rendered the network.

For NodePort, ExternalIP, or LoadBalancer providers that do not use those health
checks, make sure external traffic lands on nodes where the UDN is rendered. If
a provider such as MetalLB advertises service IPs for UDN workloads, configure
it so the advertisement happens from nodes where the UDN is rendered. One way
to do that is to schedule the speaker on the designated UDN nodes.

## References

* User Defined Networks [feature page](user-defined-networks.md)
* Dynamic UDN Node Allocation [enhancement](https://ovn-kubernetes.io/okeps/okep-5552-dynamic-udn-node-allocation/)
