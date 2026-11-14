# Egress traffic selection

`TrafficClass` defines destination CIDRs and optional TCP, UDP, or SCTP
ports. An EgressIP's `trafficSelector` selects these resources by label. Only
matching traffic uses that EgressIP; other traffic follows its normal egress
path or a coexisting default EgressIP.

Enable `--enable-traffic-class` on ovnkube-controller and ovnkube-node. The flag
defaults to false and requires `--enable-egress-ip`; enabling it without EgressIP
is a configuration error.
For Helm use `global.enableTrafficClass=true`; for Kind use
`contrib/kind.sh --enable-traffic-class`. The deployment passes
`OVN_TRAFFIC_CLASS_ENABLE` directly to the binary's CLI configuration.
Installing the CRD alone does not enable the feature. With the gate disabled,
selective EgressIPs remain unconfigured and report `FeatureGateDisabled`.
Restart reconciliation removes their previously programmed state. EgressIPs
without a selector retain catch-all behavior.

## Configure traffic

```yaml
apiVersion: k8s.ovn.org/v1
kind: TrafficClass
metadata:
  name: signaling
  labels:
    traffic: signaling
spec:
  matches:
  - destinationCIDR: 192.0.2.0/24
    protocols:
    - udp:
        destinationPort:
          number: 5060
    - udp:
        sourcePort:
          range:
            start: 16384
            end: 32767
  - destinationCIDR: 2001:db8:100::/64
    protocols:
    - tcp:
        destinationPort:
          range:
            start: 5060
            end: 5062
---
apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
  name: signaling
spec:
  egressIPs:
  - 198.51.100.101
  namespaceSelector:
    matchLabels:
      tenant: example
  podSelector:
    matchLabels:
      app: phone
  trafficSelector:
    matchLabels:
      traffic: signaling
```

Each class contains 1–25 matches, each with a network CIDR. Omit
`protocols` to match all protocols and ports for that CIDR. Matches and
protocol entries are ORed. Each protocol entry sets exactly one of `tcp`,
`udp`, or `sctp`. Within an entry, source and destination port restrictions
are ANDed. An omitted port matches all ports; a specified port contains
exactly one `number` or inclusive `range`, between 1 and 65535.

Classes selected by one EgressIP are combined. Label or destination changes
update steering and SNAT without recreating the EgressIP. A selector matching
no class programs no matching traffic. Omitting `trafficSelector` retains
catch-all behavior; an explicitly empty selector is invalid.
Expression operators must be `In` or `NotIn` with a nonempty values list, or
`Exists` or `DoesNotExist` without values.

## Supported arrangements

1. Use a selective EgressIP on a secondary host interface for chosen
   destinations. Unmatched traffic uses normal OVN egress.
2. Add a catch-all EgressIP on the OVN network for remaining traffic while
   selective secondary-host EgressIPs handle their destinations.
3. Select multiple classes with one EgressIP to use one source address for
   several destination networks or applications.
4. Give a pod multiple selective EgressIPs, including EgressIPs on the same
   secondary interface, with disjoint CIDR or L4 matches.

For primary user-defined networks, selective OVN-network EgressIPs retain
the existing packet-mark and external-bridge OVS SNAT pipeline. Both Layer 2
and Layer 3 topologies use destination and L4 restrictions in their existing
steering and marking policies. Secondary-host EgressIPs on UDNs are unsupported.

On the cluster default network, the default may also use a secondary host
interface while selective EgressIPs use the OVN network. For pods in another
zone, an additional filtered reroute in the egress node's zone steers matching
traffic to the gateway router ahead of the host catch-all path.

Support is limited to overlay networks. Multiple catch-all EgressIPs selecting
one pod retain the existing undefined selection behavior.

## Observe and troubleshoot

```sh
kubectl get trafficclasses
kubectl get egressip signaling -o yaml
kubectl describe egressip signaling
```

`TrafficSelectorResolved=True`, reason `MatchesFound`, means selected
classes contain destinations. `False`, reason `NoMatchingTrafficClass`,
and a `NoTrafficClass` warning indicate that labels resolve to no destinations.
Check class labels and the EgressIP selector; allocating an address alone does
not cause a selective EgressIP to match traffic.

`TrafficSelectorResolved=False`, reason `InvalidTrafficSelection`, identifies
an invalid stored selector. Its traffic matches nothing, and `TrafficConflict`
is `Unknown` until the selection is corrected. Other EgressIPs continue to
reconcile normally.

`TrafficConflict=True`, reason `OverlappingTraffic`, identifies a peer and
shared pod with intersecting CIDR and L4 matches. Unset ports are wildcards:
UDP destination 5060 and UDP source 16384–32767 can match the same packet.
Definitions selected by the same EgressIP do not conflict with each other.
For each pod, candidates are ordered by EgressIP creation timestamp, oldest
first, then by name. A candidate is accepted only if its traffic overlaps no
accepted owner. Rejected candidates cannot block later candidates.

The losing owner removes all configuration for that pod, including its
non-overlapping destinations. Other pods remain configured. Editing a class
can therefore displace an already active, newer EgressIP. Removing the overlap
restores its configuration and clears the condition once no selected pod is
rejected. Overlaps selected by the same owner are harmless.

For example, A matches X, B matches X and Y, and C matches Y, in age order.
A and C win; rejected B cannot block C. If A stops matching X, recomputation
accepts B and rejects C. Existing configuration has no preference.

`TrafficSelectorResolved` describes match resolution independently of pods
or conflicts. Supported matches remain resolved when no pods are selected or
all selected pods lose conflicts. During version skew, unknown protocol entries
are skipped with an `UnsupportedProtocol` warning identifying the class and
entry. Supported entries still apply. If all selected entries are unsupported,
resolution reports `AllMatchesUnsupported`; it never becomes a wildcard.

`ovnkube-controller` writes conditions from cluster-wide informer snapshots,
including pods in other zones. Controllers compute the same global result and
skip equivalent writes. Resource-version-protected patches preserve assignment
items and unrelated conditions. Cluster-manager writes assignment items and the
existing packet-mark annotation.

OVN selective reroute policies use priority 100; catch-all policies use 99.
OVN port ranges are rendered as inclusive lower and upper comparisons.
Selective SNAT uses the same destination expression and takes precedence over
fallback SNAT. UDN host-CIDR policies migrate from priority 99 to 98 so
catch-all EgressIP continues to take precedence in L2 local-gateway mode without
a transit router.
UDN gateway marking uses 96 for selective matches and 95 for catch-alls so
the fallback cannot claim matching traffic when a packet arrives unmarked.

For secondary-host EgressIPs, check `ip rule` for selective rules at 6000 and
catch-all rules at 6001. Selective and catch-all owners share the interface's
routing table, retaining default and more-specific routes and their gateways.
Matches change IP rules and SNAT; they do not synthesize destination routes. In the OVN-Kubernetes nftables table,
`egress-ip-snat-filtered` precedes the existing catch-all SNAT maps and matches
the pod source, output interface, destination, protocol, and ports.

## Upgrade and rollback

The pre-merge resource was named `EgressTrafficClass` and used
`spec.destinations[].cidr`. Private deployments of that revision must save and
remove its selective EgressIPs with the old binaries before replacing the CRD.
Recreate classes as `TrafficClass` with `spec.matches[].destinationCIDR`; both
resource kinds are not served concurrently.

Install the TrafficClass CRD and updated EgressIP schema first. Wait for both
to be established and check the served field before creating selector EgressIPs:

```sh
kubectl wait --for=condition=Established --timeout=60s crd/trafficclasses.k8s.ovn.org crd/egressips.k8s.ovn.org
kubectl explain egressip.spec.trafficSelector
```

An old schema can prune the selector and turn an intended selective resource
into a catch-all. Upgrade all controller and node instances with TrafficClass
disabled; either component can upgrade first. Then enable it everywhere and
complete rollout before creating selector EgressIPs. An older binary ignores
selectors; a new binary with the gate disabled leaves selective owners
unconfigured.

Restart repair migrates catch-all OVN policies 100→99, affected L2 UDN
host-CIDR policies 99→98, and Linux catch-all rules 6000→6001. Filtered policies
and rules retain 100 and 6000. EgressService, no-reroute and QoS retain their
precedence. The feature is not supported on no-overlay networks.

### Restore state before downgrade

Plan a maintenance window. Save resources and database/rule snapshots. Delete
all EgressIPs with `trafficSelector` while the newer implementation is still
running, and wait for their reroute/NAT/mark policies, destination sets, host
rules and filtered SNAT entries to disappear. Retain ordinary catch-all
EgressIPs and their SNAT maps. Freeze EgressIP assignment/spec changes during
restoration.

Stop the newer ovnkube-controller and ovnkube-node instances using the
deployment's maintenance procedure before editing dataplane priorities. They
must remain stopped until restoration is verified; otherwise repair will
reapply the new priorities. Run OVN commands against each zone's NB database,
and host commands in each egress node's network namespace.

Inventory router attachments and full policy ownership before selecting rows:

```sh
ovn-nbctl --format=json --columns=_uuid,name,policies list Logical_Router > routers-before.json
ovn-nbctl --format=json --columns=_uuid,priority,match,action,nexthops,external_ids list Logical_Router_Policy > policies-before.json
ip -4 -details rule show
ip -6 -details rule show
```

For each priority-99 catch-all reroute, verify its `k8s.ovn.org/owner-type` is
`EgressIP`, its owner-controller is the controller being downgraded, and its
object name identifies a retained EgressIP/pod. Check its complete source-only
match, family, network and router attachment. Restore priority and both
priority-based ownership fields together. For example, after assigning
`policy_uuid`, `owner_controller`, `object_name`, `ip_family` and `network_name`
from that verified row:

```sh
restored_id="${owner_controller}:EgressIP:100:${object_name}:${ip_family}:${network_name}"
ovn-nbctl set Logical_Router_Policy "$policy_uuid" priority=100 external_ids:priority=100 "external_ids:k8s.ovn.org/id=$restored_id"
```

Preserve every other external ID, next hop, option and router attachment. If an
identical original priority-100 policy is already attached to that router,
retain one correct original row and remove the migrated copy from the router
instead of creating a duplicate:

```sh
ovn-nbctl remove Logical_Router "$router_name" policies "$migrated_policy_uuid"
```

For affected L2 UDN local-gateway/no-transit host-CIDR rows at 98, verify the
UDN network/topology IDs and exact generated destination-host-CIDR/source-pod-
subnet match before restoring 99. These rows use the gateway policy owner's
metadata, not EgressIP's priority-based ID:

```sh
ovn-nbctl set Logical_Router_Policy "$host_cidr_policy_uuid" priority=99
```

Do not blanket-rewrite policies by priority: 99 also belongs to catch-all
EgressIP, and other owners' policies must survive. Repeated restoration only
processes remaining migrated rows. Verify no duplicate original/migrated rows
remain attached to any affected router.

For each EgressIP-owned source-only host rule at 6001, record the exact pod
source prefix, family and interface-derived table. Restore its original rule
before deleting the migrated one. For a verified IPv4 rule:

```sh
ip -4 rule add priority 6000 from "$pod_source_prefix" table "$route_table"
ip -4 rule del priority 6001 from "$pod_source_prefix" table "$route_table"
```

Use `ip -6` for IPv6. If the exact original rule already exists, skip its add
and remove only the migrated copy. Confirm ownership against the saved pod,
EgressIP assignment and table inventory; priority alone is insufficient. Do
not flush routing tables, addresses, SNAT maps or rules belonging to other
features. Shared interface routes remain in place.

Before starting older binaries, verify selective state is absent, retained
catch-all LRPs and ownership IDs are at 100, host rules are at 6000 with the
same tables, and affected host-CIDR policies are at 99. Block downgrade if any
restoration or verification fails. After downgrade, verify traffic and deletion
or pod-deselection cleanup while another owner shares the interface. These
steps require deployment-specific execution and have not been exercised by
this changeset's static validation.

## Scale and validation boundaries

Each class has at most 25 matches. The number of selected classes and protocol
entries is not capped, so the total rule count is unbounded. Scope selectors
and keep protocol lists small. Reconciliation reuses existing queues and
batches nftables changes, but failover can interrupt connections. Measure
latency and transient loss at representative pod/match counts; uninterrupted
connectivity and fallback to node IP are not guaranteed.

The selective E2E scenarios run under the existing EgressIP suite and its UDN
variants. CI enables TrafficClass for control-plane and network-segmentation
lanes. The current binary initializes the L2 transit topology without a
production switch for no-transit; that legacy topology has focused database
coverage but no runnable E2E lane in this tree. Upgrade/rollback and gate
rollout checks also need a live maintenance environment. Compilation does not
establish those runtime results.
