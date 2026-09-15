# OKEP-6943: Increase the MaxNetworks limit

* Issue: [#6943](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6943)

## Problem Statement

OVN-Kubernetes caps the number of networks per cluster at `MaxNetworks = 4096`. The cap is not a property of
any API; it exists because the tunnel key of a network's transit switch is derived arithmetically from its network
ID, and the range reserved for such derived keys is 4096 wide. Every network ID consumes one key from that range
whether or not the network has a transit switch. Localnet networks never create one, yet each localnet network
still takes a key away from networks that do.

Clusters are now being planned with more than 4096 user-defined networks. The real constraints are the OVN
interconnect tunnel key space, about 65535 keys, and a few node-side resources derived from the network ID, so
the limit can be raised well past 4096 once tunnel keys stop being derived from the ID. A limit remains: this OKEP
increases it, it does not remove it.

## Goals

* Increase `MaxNetworks` limit using centralized tunnel key allocator.
* Stop consuming interconnect tunnel keys for localnet networks.
* Make the NAD `k8s.ovn.org/tunnel-keys` annotation the single source of truth for every distributed datapath key,
  and remove all arithmetic derivation of keys from network IDs.

## Non-Goals

* Assigning datapath keys for zone-local switches and routers, which `ovn-northd` allocates today. That is option 4
  of the analysis in [OKEP-5094](okep-5094-layer2-transit-router.md) and remains out of scope.
* Changing the OVN global tunnel key range or the Geneve encoding.
* Removing the network limit altogether. The interconnect key space, the masquerade subnet, and the ID-derived
  names all bound it; `MaxNetworks` stays as the explicit, documented cap.
* Lifting other network-ID-derived limits, such as the masquerade subnet size, beyond what is needed to raise the
  network count. Those are covered in the implementation details only as constraints on the new bound.

## Introduction

### How tunnel keys are assigned today

OVN splits the 24-bit datapath tunnel key space into a local part, assigned by `ovn-northd` per zone, and a global
part of 65535 keys, from 16711681 to 16777215, for datapaths that must carry the same key in every zone. In
OVN-Kubernetes those are the per-network transit switches, the layer2 transit routers, and the connect routers
created by ClusterNetworkConnect (CNC).

The global part is sliced as follows:

| Range                                                         | Assigned by | Used for |
|---------------------------------------------------------------|---|---|
| `BaseTransitSwitchTunnelKey, BaseTransitSwitchTunnelKey+4095` | derived, no allocator | transit switch of the default network and of every layer3, secondary layer2 and localnet network; layer2 switch of a primary layer2 network |
| `BaseTransitSwitchTunnelKey + 4096, .. 16777215` 61437 keys   | `TunnelKeysAllocator` in cluster-manager | layer2 transit router of a primary layer2 network; connect router of a ClusterNetworkConnect |

`BaseTransitSwitchTunnelKey` is 16711683.

The `TunnelKeysAllocator` already exists and returns the keys it allocates; it does not touch the API. The
network manager's NAD controller in cluster-manager formats those keys into the NAD annotation
`k8s.ovn.org/tunnel-keys` and writes it together with `k8s.ovn.org/network-id` in a single `SetAnnotationsOnNAD`
update, which is what guarantees a NAD never carries an ID without its keys. The allocator treats the first 4096
keys as a preserved range: when asked for keys for a network whose ID is below 4096, it returns the derived key as
the first element without recording it, and allocates only the remaining keys. `getNumberOfTunnelKeys` requests
two keys for primary layer2 networks and zero for everything else, so the annotation is currently only present on
primary layer2 NADs.

Consumers of derived keys are `ZoneInterconnectHandler.createOrUpdateTransitSwitch` for layer3 networks and
`BaseSecondaryLayer2NetworkController` for secondary layer2 networks. Both compute `BaseTransitSwitchTunnelKey +
networkID` at runtime and never look at the annotation. Primary layer2 networks already read
`GetTunnelKeys()` from the annotation.

The `MaxNetworks` constant sizes the network ID allocator in the network manager, so the preserved range and the
ID space are kept equal by construction. Because network IDs are allocated for all networks including localnet,
each localnet network burns one preserved key that no datapath uses.

## User-Stories/Use-Cases

### Story 1: more than 4096 user-defined networks

As a cluster administrator running a multi-tenant platform, I want to create a primary UDN per tenant namespace
without hitting a cluster-wide cap of 4096 networks, since the cluster has capacity for more.

### Story 2: localnet networks do not count against the network budget

As a cluster administrator using many localnet NADs to expose VLANs to virtual machines, I do not want those
networks to reduce the number of overlay networks I can create, since they never use the overlay.

## Proposed Solution

### Why this takes two releases

There are multiple ways to implement this feature with different trade-offs. The main requirement is that 
a key computed on a controller from the network ID and a key read from the annotation must agree during the upgrade. 
If cluster-manager handed out a network ID above 4095 while a release N-1
ovnkube-controller still derived keys, that controller would program `BaseTransitSwitchTunnelKey + networkID`
for the new network, which is a key the allocator may have given to a layer2 transit router or a connect router.
Two datapaths with the same global key break interconnect traffic for both.

The network ID allocator makes this worse than "more than 4096 networks at once". It is round-robin: it hands
out IDs from a cursor that only moves forward during the process lifetime and reuses freed IDs only after
wrapping. With a wider ID range, a cluster-manager that has created 4096 networks over its lifetime hands out
ID 4096 next even if only a handful exist.

The absolutely safe way would be to keep maxNetworks at 4096 until every controller has been upgraded with
the new code that reads the annotation, then widen the range.
Since waiting a full release just for that is a long time, I am proposing a compromise:
- Keep [0,4095] range in sync with the networkID. This makes sure that upgrade will go smoothly as long
we don't create networks beyond the 4096th one and don't recreate the networks too much too (see previous paragraph). 
Considering network (re)creation is not a common practice during the upgrade, I suggest that it is a reasonable compromise.
- Start allocating keys from the pool for networkIDs above 4095. This still allows us to create more than 4096 networks,
with the only limitation that localnet networks will keep consuming keys in the [0,4095] range.
This is a temporary limitation that will be removed in the next release, when we widen the range and remove the derivation fallback.

Make `k8s.ovn.org/tunnel-keys` authoritative for all distributed datapath keys, in two phases.

### Phase 1, release N: record every key, widen the ID range, keep the low block derived

**Cluster-manager**

* `getNumberOfTunnelKeys` returns 1 for layer3 and secondary layer2 networks, 2 for primary layer2 networks, and
  0 for localnet and the default network. Previously it returned 0 for layer3 and secondary layer2, leaving their
  keys implicit.
* Raise `MaxNetworks`. The proposed value is 99999, the largest network ID that still fits the five digits
  available in the ID-derived management port and VRF device names, see [Network ID bound](#network-id-bound).
  It deliberately exceeds the interconnect key pool, so that localnet networks, which take no key, are not
  capped by it. The network ID allocator stays round-robin.
* The tunnel key allocator keeps its current shape. For a network with an ID below 4096 the first key is
  `BaseTransitSwitchTunnelKey + networkID`, exactly as today, so the low block stays in sync with the network ID
  and a release N-1 controller that still derives the key programs the same value the annotation carries. For a
  network with an ID at or above 4096 every key comes from the dynamic pool `[BaseTransitSwitchTunnelKey + 4096,
  2^24-1]`, which is what `AllocateKeys` already does when the ID is outside the preserved range. A primary
  layer2 network with a low ID keeps taking its transit router key from the pool as today.
* For every layer3 and secondary layer2 network the annotation now records its key. Existing NADs with an ID and
  no annotation are annotated on the first sync after upgrade with exactly the key they already use, through the
  existing path that allocates missing keys via `AllocateKeys`. No OVN datapath changes for existing networks.
* The default network has no NAD, so its transit switch key is not covered by the annotation. Cluster-manager
  reserves it explicitly in `initTunnelKeysAllocator`, as `BaseTransitSwitchTunnelKey + DefaultNetworkID`, before
  it lists NADs and CNCs. This is the only key handled outside NAD and CNC annotations, and the reservation is
  permanent for the life of the process.
* Cluster-manager startup reserves the annotated keys of all NADs, not only primary layer2 and
  ClusterNetworkConnect. In this release that is bookkeeping, since derived keys cannot collide with dynamic ones
  by construction of the ranges; it is what lets phase 2 open the low block with every existing key accounted for.
* ClusterNetworkConnect is unchanged. Its keys are already annotated and reserved.

**ovnkube-controller**

* `createOrUpdateTransitSwitch` and the secondary layer2 controller take the switch key from
  `GetTunnelKeys()[0]` when the annotation is present.
* When the annotation is absent, which happens only while cluster-manager is still release N-1, they fall back to
  `BaseTransitSwitchTunnelKey + networkID`. Network ID and tunnel keys are
  written in the same NAD update, so a NAD annotated by a release N cluster-manager can never carry an ID without
  keys; the fallback triggers only for NADs last written by an older cluster-manager.

**ovnkube-node** does not consume datapath tunnel keys and needs no change.

**Localnet** networks receive no keys, as before. A localnet network holding an ID below 4096 still leaves its
derived key idle, because the low block is bound to IDs in this release. That is a temporary limitation of at
most 4096 keys, lifted in phase 2.

**Operator requirement during the N-1 to N roll-out.** While any release N-1 ovnkube-controller is still
running, do not create networks beyond the 4096th and avoid heavy network recreation. Both can move the
round-robin ID cursor past 4095 and produce a network whose key an N-1 controller would derive incorrectly. Once
every controller runs release N, networks may be created without restriction. See
[Risks](#risks-known-limitations-and-mitigations).

At the end of phase 1 every network's key is recorded on its NAD, every consumer prefers the record, more than
4096 networks can be created, and the dataplane of every pre-existing network is unchanged.

### Phase 2, release N+1: open the low block, existing keys stay

**Cluster-manager**

* Extend the tunnel key allocator to the whole global range `[BaseTransitSwitchTunnelKey, 2^24-1]` and drop the
  derived-key special case. On startup cluster-manager reserves every annotated key of every NAD and every CNC
  before allocating anything, together with the default network tunnel key, so **every network that exists at the time of the upgrade keeps the key it has**,
  derived or dynamic. Only networks created after this point draw from the free pool, and for them the ID and the
  key are unrelated: a new network with ID 12 may get key `base + 7000`, and a new network with ID 7000 may get
  the low-block key that a localnet network's ID 12 never used or that a deleted network released.
* Fail network creation with a clear condition on the UDN or CUDN when the pool is exhausted, replacing the
  current failure at network ID allocation. ClusterNetworkConnect already reports allocation failure in its
  status; both consumers draw from one pool, so exhaustion surfaces on whichever object is created last.
* Remove the synthetic network ID that ClusterNetworkConnect passes to skip the preserved range, since the
  allocator no longer needs an ID to decide anything.

**ovnkube-controller**

* Remove the derivation fallback. It would be wrong for a network created after the second upgrade with a low ID
  and a pool key, and it is unnecessary because after one full release of N cluster-manager every NAD is
  annotated. A NAD with a network ID but without tunnel keys is treated as an error and retried.

**Localnet** networks continue to receive no keys and, with the derivation gone, occupy no part of the key space.
The slot their ID would have implied is an ordinary free key.

### Key budget

After phase 1 the global range is split as today: 4096 derived keys bound to IDs 0 to 4095, of which those held
by localnet networks are idle, and a dynamic pool of 61437 keys serving high-ID networks, primary layer2 transit
routers, and ClusterNetworkConnect. After phase 2 all 65533 usable keys form one pool shared by every consumer:
one per layer3 or secondary layer2 network, two per primary layer2 network, one per ClusterNetworkConnect. With
`MaxNetworks` at 99999 (see [network ID section](#network-id-bound)) the ID space is no longer what runs out first for overlay networks; the key pool is, and
`MaxNetworks` remains the hard cap above it for every topology, including localnet networks that take no key.
Operators sizing a cluster should count networks by topology plus CNCs against that pool, and the exhaustion 
condition tells them when it is reached.

### API Details

No new API. The existing NAD annotation `k8s.ovn.org/tunnel-keys`, a JSON array of integers, is written for
layer3 and secondary layer2 networks in addition to primary layer2 networks. The order of the array is fixed per
topology:

| Topology | Keys | Meaning |
|---|---|---|
| layer3 | 1 | transit switch |
| secondary layer2 | 1 | layer2 switch |
| primary layer2 | 2 | layer2 switch, transit router |
| localnet, default | 0 | none |

The annotation is owned by cluster-manager and must not be set by users. It is not covered by the ovnkube-identity
webhook because it lives on the NAD, not on a node or pod.

### Implementation Details

**Phase 1**

* `getNumberOfTunnelKeys` in `pkg/networkmanager/nad_controller.go` per the table above. The existing warning
  for an unexpected number of annotated keys becomes the upgrade path for layer3 and secondary layer2 NADs: zero
  keys annotated, one expected, so `AllocateKeys` returns the derived key for IDs below 4096 and it is written.
* `MaxNetworks` raised to 99999; `networkIDFromNADs` in the UDN controller follows the constant.
* `TunnelKeysAllocator` keeps its preserved range and dynamic pool. Its literal `4096` is replaced with a named
  constant for the size of the derived block, since it and `MaxNetworks` are no longer the same number.
  `ReserveKeys` reserves every key passed to it, including keys in the preserved range that it currently drops,
  and cluster-manager startup reserves the annotated keys of all NADs. `initTunnelKeysAllocator` in
  `pkg/clustermanager/clustermanager.go` additionally reserves `BaseTransitSwitchTunnelKey + DefaultNetworkID`
  under the default network's name before listing NADs and CNCs.
* `pkg/ovn/zone_interconnect/zone_ic_handler.go` and `pkg/ovn/base_secondary_layer2_network_controller.go` read
  the key from `NetInfo.GetTunnelKeys()` with the derivation fallback.

**Phase 2**

* `pkg/allocator/id/tunnelkeyallocator.go` drops `preservedRange` and `idsOffset`; the range is
  `[BaseTransitSwitchTunnelKey, 2^24-1]` and every key is reserved or allocated explicitly.
  `BaseTransitSwitchTunnelKey` is retained only as the range start.
* Startup order in cluster-manager: list NADs and CNCs, reserve all annotated keys, then start allocating. This
  ordering already exists for the dynamic range and is extended to the full range.
* Derivation fallback removed from the controllers.

### Network ID bound

Once tunnel keys are decoupled, the network ID still feeds a handful of per-network values on the node. Each is
listed below with how it is computed and where it stops working, so the new `MaxNetworks` can be chosen against
them rather than against the key space.

* **UDN masquerade IPs**, `pkg/generator/udn/masquerade_ips.go`. Every primary UDN gets two addresses out of the
  cluster masquerade subnet, `config.Gateway.V4MasqueradeSubnet` and `V6MasqueradeSubnet`: one for its gateway
  router and one for its management port. They are computed as offsets from the subnet base: gateway router at
  `10 + 2*networkID - 1`, management port at `10 + 2*networkID`. Offsets 0 to 9 are left for the default
  network's masquerade addresses, which occupy the first five. `GenerateIP` fails when the offset leaves the
  subnet, so the highest usable network ID is about 16378 for the `169.254.0.0/17` that kind
  deploys and 32762 for `fd69::/112`. The built-in defaults, `169.254.169.0/29` and `fd69::/125`, hold eight
  addresses and cannot host a single UDN, which is why UDN clusters already override them. This is the binding
  limit in practice; the failure surfaces on the node when the UDN gateway is constructed, not at network
  creation. `(masqueradeSubnetIPs - 12)/2`

* **Conntrack mark**, `pkg/node/gateway_udn.go`. Each UDN's traffic through the shared gateway bridge is
  committed to conntrack with `ct_mark = 3 + networkID`, and the bridge flows match `ct_mark` on the reply path
  to steer established and related traffic back to the right gateway router. `ct_mark` is a 32-bit field and
  marks 0 to 2 are reserved for the default network, so the highest usable network ID is
  `2^32 - 1 - 3 = 4294967292`.

* **Packet mark**, `pkg/node/gateway_udn.go`. Host-originated traffic to a UDN's services is marked in nftables
  with `meta mark set 4096 + networkID`, and `ip rule` entries at priority `UDNMasqueradeIPRulePriority` match
  that fwmark to send the packet into the network's routing table. The base of 4096 keeps UDN marks clear of the
  low values other components use; it is unrelated to `MaxNetworks` despite the same number. The fwmark is a
  32-bit field, so the highest usable network ID is `2^32 - 1 - 4096 = 4294963199`.

* **VRF routing table**, `pkg/node/gateway_udn.go` and `pkg/util/net.go`. On DPU-host and full-mode nodes the
  table ID is not derived from the network ID at all: `CalculateRouteTableID` adds the management port's link
  index to `RoutingTableIDStart`. Only DPU-mode nodes, which have no host management port to take an index from,
  use `100000 + networkID` to stay clear of the link-index range. Kernel table IDs are 32-bit with 253 to 255
  reserved for `default`, `main` and `local`, all below the base, so the highest usable network ID is
  `2^32 - 1 - 100000 = 4294867295`.

* **Management port interface name**, `pkg/util/util.go`. The UDN management port is named
  `ovn-k8s-mp<networkID>` because the network name could exceed the kernel's 15-character interface name limit.
  The prefix is 10 characters, leaving **5 digits**, so IDs up to 99999 produce distinct names; beyond that the name
  is truncated and two networks could collide.

* **VRF device name**, `pkg/util/multi_network.go`. The UDN VRF is named after the CUDN when that fits in 15
  characters, otherwise `mp<networkID>-udn-vrf`. Prefix and suffix take 10 characters, again leaving 5 digits and
  an ID limit of 99999. The CUDN `status.vrfName` field publishes whichever form was used.

With `MaxNetworks` at 99999 the interface and VRF names are used to their full five digits, the marks and table
IDs are far from any limit, and the masquerade subnet is what decides how many **primary** UDNs a cluster can hold.
The effective limit for primary UDNs is therefore the smaller of the tunnel key budget and the masquerade subnet capacity.
Operators who need more primary UDNs than their masquerade subnet allows must configure a larger one before the
cluster reaches the bound; the subnet cannot be changed on a running cluster without disruption, which is an
existing constraint. Secondary networks do not allocate masquerade IPs and are bound only by the tunnel key pool (non-localnet).

### Testing Details

* Unit tests for `TunnelKeysAllocator`: phase 1 an ID below 4096 yields its derived key and an ID above 4095
  yields only pool keys, reservation of derived keys from annotations does not change allocation results; phase 2
  seeding from annotations keeps every existing key, new networks draw from the low block once it has free slots,
  exhaustion error.
* Unit tests for `getNumberOfTunnelKeys` per topology.
* Unit tests in `pkg/ovn` asserting that transit switch creation uses the annotated key and, in phase 1, falls back
  with a log when it is absent.
* Unit test that the node-side `nadController` accepts a NAD annotated with an ID above 4095 and an arbitrary
  tunnel key.
* The existing upgrade e2e job runs release N-1 to N. It gains an assertion that after upgrade every pre-existing
  layer3 and secondary layer2 NAD carries a `k8s.ovn.org/tunnel-keys` annotation equal to its previously derived
  key, and that the transit switch `requested-tnl-key` in every zone is unchanged.
* A scale-oriented e2e in phase 1 that creates more than 4096 networks of mixed topology on a kind cluster, using
  localnet NADs for most of them so that the test stays cheap, and verifies that overlay networks past ID 4096 get
  connectivity.

### Documentation Details

* `docs/design/upgrades.md` gains this migration as a worked example alongside interconnect and the layer2
  transit router: a record introduced in release N with fallback and an operator requirement for the roll-out
  window, low block opened and fallback removed in N+1.
* The UDN feature documentation replaces the "4096 networks" statement with the new bound, the masquerade subnet
  prerequisite, the localnet limitation of phase 1, and the roll-out requirement.
* Release notes for release N state the roll-out requirement explicitly.

## Risks, Known Limitations and Mitigations

**Network creation past 4096 or heavy recreation during the N-1 to N roll-out.** This is the compromise this
OKEP makes. A release N-1 ovnkube-controller derives the key for any network it sees. Networks with IDs below
4096 are safe because the annotation carries the same derived key. A network with an ID above 4095 is not: the
N-1 controller programs `base + ID`, which lies inside the dynamic pool and may already belong to a transit
router or connect router, breaking traffic for both in that controller's zone until it is upgraded. Such an ID
is handed out when the low block is full, or when the round-robin cursor has passed 4095 because more than 4096
networks were created over the cluster-manager's lifetime, which heavy recreation can cause without ever
holding 4096 networks at once. Network creation and recreation are uncommon during an upgrade window, so the
requirement is documented in the release notes and the UDN documentation rather than enforced: **do not create
networks beyond the 4096th and avoid bulk network recreation until every ovnkube controller runs release N**.
The window closes when the last controller upgrades; a network that was misprogrammed by an N-1 controller is
corrected when that controller restarts on N and reads the annotation.

**Localnet networks consume low-block keys in phase 1.** A localnet network with an ID below 4096 keeps its
derived key idle, so the pool available to overlay networks is reduced by the number of such localnet networks,
at most 4096. Lifted in phase 2 when the low block joins the pool.

**Network created or recreated during the N to N+1 roll-out.** Harmless. Release N controllers read the
annotation for every topology, their network ID cache accepts any ID because the underlying bitmap grows on
demand and has no range check on reservation, and nothing outside cluster-manager compares the ID to
`MaxNetworks`. A network created by cluster-manager N+1 with a low ID and a pool key is programmed correctly by an
N controller because the annotation is present and preferred over derivation. A unit test pins the node-side
acceptance of a high ID so a later change does not quietly introduce a range check.

**Keys can no longer be inferred from the ID after phase 2.** Networks that existed before the second upgrade
still show the familiar `base + ID` key, networks created afterwards do not. The annotation and the transit
switch `other_config:requested-tnl-key` are the places to look when debugging.

**Masquerade subnet becomes the binding limit.** Raising `MaxNetworks` without a larger masquerade subnet moves
the failure from network ID allocation to masquerade IP generation, which happens later and on the node. The
phase 1 startup validation prevents that configuration.

## OVN-Kubernetes Version Skew

Within release N: cluster-manager N writes derived keys for every ID below 4096, so controllers on N-1 that
derive and controllers on N that read the annotation program the same key for those networks. Cluster-manager
N-1 with controllers on N: controllers fall back to derivation, producing the key cluster-manager N-1 assumes.
The only disagreement possible is a network with an ID above 4095 seen by an N-1 controller, which the roll-out
requirement above is meant to avoid.

Within release N+1: controllers on N and N+1 both read the annotation. Controllers on N would fall back for an
unannotated NAD, but after cluster-manager N has run once no such NAD exists. Any network cluster-manager N+1
creates, including one with a low ID and a pool key, is programmed correctly by both.

Cluster-manager does not depend on ovnkube-identity for this change, since the NAD annotation is not webhook
protected.

## Backwards Compatibility

No existing network's tunnel key changes in either phase. The annotation is additive. Downgrade from N to N-1
works for networks with IDs below 4096 because N-1 derives the same values; networks with IDs above 4095 would
be misprogrammed by N-1 controllers and refused by the N-1 cluster-manager, so a cluster that has crossed 4096
networks cannot be downgraded. Downgrade from N+1 to N is not supported by the upgrade model; if attempted,
networks that existed before the second upgrade are unaffected since their keys never changed, and networks
created afterwards keep working on N controllers because they read the annotation.

## Alternatives

**Keep `MaxNetworks` at 4096 in release N and widen only in N+1.** The absolutely safe variant: no network can
receive a key an N-1 controller would derive differently, so no operator requirement is needed. Rejected because
it delays the ability to create more than 4096 networks by a full release for a risk that only materializes when
an operator creates or recreates networks in bulk during an upgrade.

**Widen everything in release N.** Open the low block to high-ID networks in the same release. Rejected because
the round-robin ID allocator can then hand a low-block key to a high-ID network while an N-1 controller still
derives `base + ID` for the low ID that key implies, and no operator requirement can bound that.

**Runtime gate on a per-node capability annotation.** Controllers stamp a node annotation once they read tunnel
keys and cluster-manager hands out IDs above 4095 only when every node carries it. Removes the operator
requirement at the cost of a new node annotation, an identity webhook entry, and gate bookkeeping. Not pursued;
it can be added later without changing the allocation scheme if the documented requirement proves insufficient.

**Keep the low block bound to IDs forever.** Only IDs above 4095 would ever draw from the pool. Simpler allocator
and a fallback that is never wrong, but localnet networks on low IDs keep their keys idle permanently, so
recovering those keys would need an ID assignment preference per topology. Rejected in favor of opening the low
block in phase 2, when the binding between ID and key is no longer needed for anything.

**Split the global range into fixed blocks per datapath type**, option 1 from OKEP-5094. Caps overlay networks at
roughly 21000 and leaves no room for new distributed datapath types. Rejected there and here.

**Assign all datapath keys, including zone-local ones**, option 4 from OKEP-5094. Removes the global range as a
constraint entirely but is a far larger change to how `ovn-northd` and OVN-Kubernetes divide responsibility.
Deferred until the global range itself becomes the limit.

## References

* [Issue #6943](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6943)
* [OKEP-5094 Layer2 transit router, tunnel keys section](okep-5094-layer2-transit-router.md#tunnel-keys-for-the-transit-switchesrouters)
* [OVN datapath tunnel key ranges](https://github.com/ovn-org/ovn/blob/main/lib/ovn-util.h)
* [Upgrade model](../design/upgrades.md)
