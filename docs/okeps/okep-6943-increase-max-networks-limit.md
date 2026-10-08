# OKEP-6943: Increase the MaxNetworks limit

* Issue: [#6943](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6943)

## Problem Statement

OVN-Kubernetes caps the number of networks per cluster at `MaxNetworks = 4096`. The cap is not a property of
any API; it exists because the tunnel key of a network's transit switch is derived arithmetically from its network
ID, and the range reserved for such derived keys is 4096 wide. Every network ID consumes one key from that range
whether or not the network has a transit switch. Localnet networks never create one, and neither do networks whose
transport is `no-overlay` ([OKEP-5259](okep-5259-no-overlay.md)) or `evpn` ([OKEP-5088](okep-5088-evpn.md)), whose
east-west traffic is routed by the provider network or carried by VXLAN instead of Geneve. Yet each of these
networks still takes a key away from networks that do use the overlay.

Clusters are now being planned with more than 4096 user-defined networks. The real constraints are the OVN
interconnect tunnel key space, about 65535 keys, and a few node-side resources derived from the network ID, so
the limit can be raised well past 4096 once tunnel keys stop being derived from the ID. A limit remains: this OKEP
increases it, it does not remove it.

## Goals

* Increase `MaxNetworks` limit be decoupling tunnel key from the networkID using centralized tunnel key allocator.
* Stop consuming interconnect tunnel keys for networks that have no Geneve datapath: localnet networks and
  layer3 or layer2 networks whose transport is `no-overlay` or `evpn`.
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
`GetTunnelKeys()` from the annotation. Networks with transport `no-overlay` or `evpn` create no transit switch,
no layer2 switch tunnel key and no transit router, so they use no global tunnel key at all.
`getNumberOfTunnelKeys` does not look at the transport today, so a primary layer2 network with `evpn` transport
is annotated with two keys it never programs.

The `MaxNetworks` constant sizes the network ID allocator in the network manager, so the preserved range and the
ID space are kept equal by construction. Because network IDs are allocated for all networks including localnet
and non-Geneve transports, each such network burns one preserved key that no datapath uses.

## User-Stories/Use-Cases

### Story 1: more than 4096 user-defined networks

As a cluster administrator running a multi-tenant platform, I want to create a primary UDN per tenant namespace
without hitting a cluster-wide cap of 4096 networks, since the cluster has capacity for more.

### Story 2: networks without a Geneve datapath do not count against the network budget

As a cluster administrator using many localnet NADs to expose VLANs to virtual machines, I do not want those
networks to reduce the number of overlay networks I can create, since they never use the overlay.

As a cluster administrator running my networks in `no-overlay` mode over a BGP-routed fabric, or as `evpn`
networks integrated with the data center fabric, I do not want those networks to consume Geneve tunnel keys
they never use, nor to be capped by the tunnel key pool.

## Proposed Solution

### Upgrade order makes this a single release

The one requirement is that the key a controller computes from the network ID and the key it reads from the
annotation agree for every network that exists while both kinds of controller are running. If cluster-manager
handed a network a pool key while a release N-1 ovnkube-controller still derived `BaseTransitSwitchTunnelKey +
networkID`, that controller would program a key the allocator may have given to another datapath, and two
datapaths with the same global key break interconnect traffic for both.

The upgrade model resolves this by ordering the roll-out: **ovnkube-node, which hosts ovnkube-controller in
interconnect mode, is upgraded on every node before ovnkube-control-plane**. See the
[upgrade model](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6915), which will document this order.
With that order, by the time a release N cluster-manager allocates its first pool key, every controller in the
cluster already reads the annotation. During the roll-out window itself the cluster-manager is still N-1: it
writes network IDs without tunnel keys, allocates no ID above 4095, and every controller, old or new, derives the
same `base + ID` key for those NADs. No phase, capability gate or operator restriction on network creation is
needed.

Make `k8s.ovn.org/tunnel-keys` authoritative for all distributed datapath keys, as follows.

### Component changes

**Cluster-manager**

* `getNumberOfTunnelKeys` returns 1 for layer3 and secondary layer2 networks, 2 for primary layer2 networks, and
  0 for localnet networks, the default network, and any network whose transport is `no-overlay` or `evpn`.
  Previously it returned 0 for layer3 and secondary layer2, leaving their keys implicit, and 2 for every primary
  layer2 network regardless of transport, allocating keys that `evpn` networks never program.
* Raise `MaxNetworks`. The proposed value is 99999, the largest network ID that still fits the five digits
  available in the ID-derived management port and VRF device names, see [Network ID bound](#network-id-bound).
  It deliberately exceeds the interconnect key pool, so that networks that take no key, localnet and
  `no-overlay` or `evpn` transports, are not capped by it. The network ID allocator stays round-robin.
* The tunnel key allocator covers the whole global range `[BaseTransitSwitchTunnelKey, 2^24-1]` and drops the
  derived-key special case. Network ID and tunnel key are unrelated for every network created from now on: a new
  network with ID 12 may get key `base + 7000`.
* **Existing networks keep their keys.** On startup, before allocating anything, cluster-manager reserves in this
  order: the default network's transit switch key, `BaseTransitSwitchTunnelKey + DefaultNetworkID`, under the
  default network's name; every key already annotated on a NAD or ClusterNetworkConnect; and, for every NAD that
  carries a network ID but no tunnel keys and whose topology needs one, the derived key `base + ID`. It then
  annotates those NADs with exactly that derived key through the existing path that allocates missing keys.
  The NAD now records the key its datapath already uses, and nothing in OVN changes for any pre-existing network.
  This is the only moment derivation happens in cluster-manager, and only for NADs last written by an N-1
  cluster-manager.
* Fail network creation with a clear condition on the UDN or CUDN when the pool is exhausted, replacing the
  current failure at network ID allocation. ClusterNetworkConnect already reports allocation failure in its
  status; both consumers draw from one pool, so exhaustion surfaces on whichever object is created last.
* Remove the synthetic network ID that ClusterNetworkConnect passes to skip the preserved range, since the
  allocator no longer needs an ID to decide anything.

**ovnkube-controller**

* `createOrUpdateTransitSwitch` and the secondary layer2 controller take the switch key from
  `GetTunnelKeys()[0]` when the annotation is present. Network ID and tunnel keys are written in the same NAD
  update, so a NAD annotated by a release N cluster-manager never carries an ID without its keys.
* When the annotation is absent, which happens only while cluster-manager is still release N-1, they fall back to
  `BaseTransitSwitchTunnelKey + networkID`, the value that cluster-manager assumes. The fallback is a safety net
  for the roll-out window and for a controller restart before cluster-manager has annotated an old NAD. It is
  removed in release N+1 as cleanup, not as a behavioural change: after one release of N cluster-manager every
  NAD is annotated, and a NAD with an ID but without keys is then treated as an error and retried.

**ovnkube-node** does not consume datapath tunnel keys and needs no change.

**Localnet, `no-overlay` and `evpn`** networks receive no keys. For localnet that is as before; for the two
transports it stops the allocation of keys their controllers never use. Cluster-manager does not reserve derived
keys for these topologies at startup, so the key slot an existing localnet network's ID implied is an ordinary
free key from release N on. Changing a network's transport is not a reconcilable update, so a network never has to
acquire or release keys in place.

**Operator requirement.** The roll-out must upgrade every ovnkube-node before ovnkube-control-plane. Platforms
that upgrade the control plane first must not do so for this release: an N-1 controller seeing a NAD with a pool
key would derive `base + ID` instead and could collide with another datapath. See
[Risks](#risks-known-limitations-and-mitigations).

At the end of the roll-out every network's key is recorded on its NAD, every consumer prefers the record, more
than 4096 networks can be created, and the dataplane of every pre-existing network is unchanged.

### Key budget

All 65533 usable global keys form one pool shared by every consumer: one per layer3 or secondary
layer2 network, two per primary layer2 network, one per ClusterNetworkConnect. Keys held by pre-existing networks
are simply reserved entries in that pool; localnet, `no-overlay` and `evpn` networks hold none. With `MaxNetworks`
at 99999 (see [network ID section](#network-id-bound)) the ID space is no longer what runs out first for overlay
networks; the key pool is, and `MaxNetworks` remains the hard cap above it for every topology, including the
networks that take no key. Operators sizing a cluster should count networks by topology plus CNCs against that
pool, and the exhaustion condition tells them when it is reached.

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
| any topology with transport `no-overlay` or `evpn` | 0 | none, no Geneve datapath exists |

The annotation is owned by cluster-manager and must not be set by users. It is not covered by the ovnkube-identity
webhook because it lives on the NAD, not on a node or pod.

### Implementation Details

* `getNumberOfTunnelKeys` in `pkg/networkmanager/nad_controller.go` per the table above, returning 0 when
  `NetInfo.Transport()` is `no-overlay` or `evpn`. A primary layer2 `evpn` NAD annotated with two keys by an
  older cluster-manager keeps them until the network is recreated; they are reserved at startup like any other
  annotated key and simply stay unused.
* `MaxNetworks` raised to 99999; `networkIDFromNADs` in the UDN controller follows the constant.
* `pkg/allocator/id/tunnelkeyallocator.go` drops `preservedRange` and `idsOffset`; the range is
  `[BaseTransitSwitchTunnelKey, 2^24-1]` and every key is reserved or allocated explicitly.
  `BaseTransitSwitchTunnelKey` is retained as the range start and for the startup derivation of unannotated NADs.
  `ReserveKeys` reserves every key passed to it, including keys it currently drops as preserved.
* Startup order in cluster-manager, `initTunnelKeysAllocator` in `pkg/clustermanager/clustermanager.go`: reserve
  `BaseTransitSwitchTunnelKey + DefaultNetworkID`, list NADs and CNCs, reserve all annotated keys, reserve
  `base + ID` for every unannotated NAD whose topology needs a key, then start allocating. The NAD controller's
  existing "unexpected number of annotated keys" path writes the reserved derived key onto those NADs on their
  first sync.
* `pkg/ovn/zone_interconnect/zone_ic_handler.go` and `pkg/ovn/base_secondary_layer2_network_controller.go` read
  the key from `NetInfo.GetTunnelKeys()` with the derivation fallback, to be removed in N+1.

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
existing constraint. Secondary networks do not allocate masquerade IPs and are bound only by the tunnel key pool
when they use Geneve; localnet, `no-overlay` and `evpn` networks are bound only by `MaxNetworks`.

### Testing Details

* Unit tests for `TunnelKeysAllocator`: seeding from annotated and unannotated NADs keeps every existing key,
  new networks draw from anywhere in the range including slots below `base + 4096` that no network holds,
  exhaustion error, CNC allocation without a synthetic ID.
* Unit tests for `getNumberOfTunnelKeys` per topology and per transport, asserting zero keys for `no-overlay`
  and `evpn`.
* Unit tests in `pkg/ovn` asserting that transit switch creation uses the annotated key and falls back with a log
  when it is absent.
* Unit test that the node-side `nadController` accepts a NAD annotated with an ID above 4095 and an arbitrary
  tunnel key.
* The staged upgrade e2e job, `nodes-first` order, runs release N-1 to N with UDN tests after each stage. It gains
  an assertion that after the control-plane stage every pre-existing layer3 and secondary layer2 NAD carries a
  `k8s.ovn.org/tunnel-keys` annotation equal to its previously derived key, that the transit switch
  `requested-tnl-key` in every zone is unchanged, and that a network created between the two stages, by the N-1
  cluster-manager, is annotated and keeps working.
* A scale-oriented e2e that creates more than 4096 networks of mixed topology on a kind cluster, using localnet
  NADs for most of them so that the test stays cheap, and verifies that overlay networks past ID 4096 get
  connectivity.

### Documentation Details

* `docs/design/upgrades.md` documents the node-first roll-out order as a requirement and gains this migration as
  a worked example alongside interconnect and the layer2 transit router: a record introduced with a fallback that
  is correct for everything an older cluster-manager can write, and the fallback removed one release later.
* The UDN feature documentation replaces the "4096 networks" statement with the new bound and the masquerade
  subnet prerequisite.
* Release notes for release N state the roll-out order requirement explicitly.

## Risks, Known Limitations and Mitigations

**Control plane upgraded before the nodes.** This is the one ordering the design does not tolerate. A release N
cluster-manager annotates new networks with pool keys, and an N-1 ovnkube-controller ignores the annotation and
programs `base + ID`, which may already belong to a transit router, a connect router or another network in that
controller's zone. Existing networks are unaffected, since their annotated key equals the derived one. The
requirement is stated in the upgrade model, the release notes and the UDN documentation. A network misprogrammed
by an N-1 controller is corrected when that controller restarts on N and reads the annotation. A runtime gate,
see [Alternatives](#alternatives), can be added later without changing the allocation scheme if a platform
cannot honour the order.

**Keys can no longer be inferred from the ID.** Networks that existed before the upgrade still show the familiar
`base + ID` key, networks created afterwards do not. The annotation and the transit switch
`other_config:requested-tnl-key` are the places to look when debugging.

**Masquerade subnet becomes the binding limit.** Raising `MaxNetworks` without a larger masquerade subnet moves
the failure from network ID allocation to masquerade IP generation, which happens later and on the node. The
startup validation prevents that configuration.

## OVN-Kubernetes Version Skew

Controllers on N with cluster-manager on N-1, the supported roll-out window: every NAD carries an ID and no keys,
controllers fall back to derivation and program exactly the key cluster-manager N-1 assumes. Controllers on N-1
and N side by side during the node roll: both derive the same key for the same NAD. Cluster-manager on N with
controllers on N: every NAD is annotated and the annotation is preferred.

Cluster-manager on N with a controller on N-1 is outside the supported order and is the risk described above.

Cluster-manager does not depend on ovnkube-identity for this change, since the NAD annotation is not webhook
protected.

## Backwards Compatibility

No existing network's tunnel key changes. The annotation is additive. Downgrade from N to N-1 is not supported by
the upgrade model; if attempted, networks that existed before the upgrade are unaffected since their keys never
changed, while networks created on N hold pool keys that N-1 controllers would derive differently and that the
N-1 cluster-manager, if the ID is above 4095, refuses, so a cluster that has created networks on N cannot be
downgraded cleanly.

## Alternatives

**Two releases with the low block bound to IDs in the first.** The earlier revision of this OKEP: release N keeps
`base + ID` for IDs below 4096 and draws pool keys only for IDs above 4095, release N+1 opens the low block and
removes the fallback. It tolerates any roll-out order at the cost of an operator restriction on creating or
recreating networks during the upgrade, a second release before keys held by localnet networks are recovered,
and a preserved-range special case in the allocator. Superseded once the upgrade model made node-first the
documented order, which removes the only skew the restriction guarded against.

**Keep `MaxNetworks` at 4096 in release N and widen only in N+1.** The absolutely safe variant under any order.
Rejected because it delays the ability to create more than 4096 networks by a full release for a skew the
roll-out order already excludes.

**Runtime gate on a per-node capability annotation.** Controllers stamp a node annotation once they read tunnel
keys and cluster-manager hands out pool keys only when every node carries it. Removes the ordering requirement at
the cost of a new node annotation, an identity webhook entry, and gate bookkeeping. Not pursued; it can be added
later without changing the allocation scheme if a platform cannot upgrade nodes first.

**Keep the low block bound to IDs forever.** Only IDs above 4095 would ever draw from the pool. Simpler allocator
and a fallback that is never wrong, but localnet networks on low IDs keep their keys idle permanently, so
recovering those keys would need an ID assignment preference per topology. Rejected in favor of one pool.

**Split the global range into fixed blocks per datapath type**, option 1 from OKEP-5094. Caps overlay networks at
roughly 21000 and leaves no room for new distributed datapath types. Rejected there and here.

**Assign all datapath keys, including zone-local ones**, option 4 from OKEP-5094. Removes the global range as a
constraint entirely but is a far larger change to how `ovn-northd` and OVN-Kubernetes divide responsibility.
Deferred until the global range itself becomes the limit.

## References

* [Issue #6943](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6943)
* [OKEP-5094 Layer2 transit router, tunnel keys section](okep-5094-layer2-transit-router.md#tunnel-keys-for-the-transit-switchesrouters)
* [OVN datapath tunnel key ranges](https://github.com/ovn-org/ovn/blob/main/lib/ovn-util.h)
* [Upgrade model](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6915), pending merge of the upgrade design document; switch to `../design/upgrades.md` once it lands
