---
date: 2026-09-01
authors:
  - kyrtapz
---

# Scaling User Defined Networks

User Defined Networks (UDNs) provide isolated networks for workloads as
an alternative to a single cluster-default network. Tenants and
applications can use separate address spaces and topologies, with
traffic that does not mix across networks. See the
[UDN feature page](../../features/user-defined-networks/user-defined-networks.md)
for the model and API.

<!-- more -->

That isolation has a control-plane cost. Each UDN performed much of the
same work as the default network: watching the API, programming OVN, and
configuring the node. At small network counts this was acceptable. At
larger counts the same design multiplied overhead across every network,
independent of user traffic.

Recent work has focused on preserving that isolation while reducing
per-network resource duplication, so control-plane cost scales with the
cluster rather than with the number of UDNs.

## Observed behavior at larger network counts

UDN connectivity remained correct. The cost showed up in the control
plane:

- Network creation took longer as the number of networks increased.
- Node CPU remained high with little user traffic.
- The API server received a large volume of node and pod updates.
- Multiple networks sharing the same uplink could see excess ARP
  traffic.
- Pod and network latency showed up across ovnkube-controller,
  ovnkube-node, and the control-plane components, not in a single place.

These effects were dominated by control-plane work that grew with the
number of networks, not with packet rate.

## Reducing per-network overhead

The default network was implemented as a single, cluster-wide network.
UDN reused that structure: a controller per network, separate watches of
nodes and pods, and separate node programming. Isolation requires
per-network state. It does not require repeating cluster-scoped work for
every UDN, for example parsing the same node object in each controller,
reconciling the same host rules once per network, or starting networks
serially while each waits to become ready.

The following changes reduce that duplication:

- Node handling runs on a shared path instead of inside every network
  controller ([#5832](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/5832)).
- Cross-network features such as EgressIP no longer repeat the same
  node listing and database writes for every network in a reconcile
  loop ([#6437](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6437)).
- Independent networks can start in parallel
  ([#6492](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6492)).

On the node, work that grew with the number of local networks was
reduced by using the OVS database client instead of forking commands on
a timer ([#6402](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6402),
[#6677](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6677)),
reconciling IP rules incrementally
([#6640](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6640)),
skipping cluster-wide pod scans that some topologies do not need
([#6577](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6577)),
and avoiding unused per-node allocations
([#6522](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6522)).

A burst of new networks previously produced a corresponding burst of
node annotation updates, retries, and client-side rate limiting. Further
batching of those updates is still in progress. The goal is to avoid
extra API reads that consume client rate-limit budget during pod churn.

Primary networks share a node IP and MAC on the gateway bridge.
Logical isolation in OVN is not isolation on the wire: if each logical
gateway answers ARP for the same address, the physical network sees
duplicated replies. ARP requests for the node address are now delivered
to the default gateway rather than to every UDN, and duplicate replies
have been restricted
([#6576](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6576),
[#6660](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6660)).
That applies whenever UDNs share the node’s physical interface, not
only to one topology.

The point of these changes is the growth rate. Adding a network still
costs CPU and memory, but closer to that network’s own state rather
than another full copy of cluster-wide work. Usage still
climbs as networks are added. It no longer grows as fast and linearly.

## Platform-wide objects

Improvements in OVN-Kubernetes do not automatically make a cluster
scale. Objects that select the entire cluster still add work on every
pod event, and that cost is independent of how cheap UDN controllers
become.

NetworkPolicy is the example that showed up here. A peer with an empty
namespace selector matches all namespaces. Paired with an empty pod
selector, it matches all pods in all namespaces. OVN-Kubernetes
maintains an address set for that peer and updates it on every pod add
and delete. A single such rule, easy to overlook, is enough to dominate
control-plane work while UDNs are being created or while pods are
churning.

A redundant “allow from every namespace” peer next to a rule that
already allows the same traffic is a common way to hit this. Removing
the extra peer drops a cluster-wide address set from the pod event path
without changing policy intent.

Anything that selects all pods or all namespaces, NetworkPolicy or
otherwise, should be treated as part of scale optimization.
Broad selection is appropriate only when it is the intended policy.

## OVN incremental processing

OVN-Kubernetes does not program OpenFlow directly. It writes the
Northbound database. northd and ovn-controller translate that into
logical and physical flows. With many UDNs, that path determines much of
the time to program a new pod.

Incremental processing is the expected behavior: adding a port should
not recompute every flow. Failures of that path are not always obvious.
CPU increases, pods remain pending, and the delay is attributed to the
network plugin.

UDNs create additional OVN objects: extra router ports
([commit](https://github.com/ovn-org/ovn/commit/36b9ca987d41)), patch
ports, and ports on other nodes
([commit](https://github.com/ovn-org/ovn/commit/860d5e4138ed)). Those
objects are where incremental processing gaps appear. A newly created
port can be treated as a type change and trigger a full recompute
([commit](https://github.com/ovn-org/ovn/commit/b408eedf6d)). northd
can write a status field, observe its own write, and leave the
incremental path. A port type that UDNs create in volume may never have
been handled incrementally, even when it behaves like a VIF.

OVN-Kubernetes cannot compensate for that from the Kubernetes side. OVN
changes in this effort made incremental processing apply to the objects
UDNs create, instead of falling back to a full recompute. Correct
incremental processing is a dependency of UDN scale, not an optional
optimization. Closing remaining gaps there is ongoing work with the OVN
community.

## Dynamic UDN

When most nodes do not need most networks, rendering every UDN on every
node is unnecessary. [Dynamic UDN](../../features/user-defined-networks/dynamic-udn.md)
builds a UDN only on nodes that use it. It is a separate feature with
its own tradeoff: the first pod on a new node waits for the network to
be rendered. The work described here applies when a network *does* need
to exist on a node.

## Next steps

UDN scale is not complete. The remaining work continues to remove
per-network copies of cluster-scoped operations.

On the OVN-Kubernetes side that includes handling pod events once
rather than fanning them out to every network, finishing the migration
away from subprocess calls to OVS and OVN on hot paths, batching node
annotation updates so network creation does not contend with the API,
and reducing ARP and NDP broadcast fan-out on the shared gateway
bridge ([OKEP-6691](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6687)).

OVN remains on the critical path. Incremental processing needs to remain
the default behavior as these topologies evolve, rather than a path that
falls back to a full recompute when a new object type appears.
