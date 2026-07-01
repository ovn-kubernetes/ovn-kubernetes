# OKEP-6602: Migration-stable DHCPv6 Prefix Delegation

* Issue: [#6602](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6602)

## Problem Statement

A workload attached to an OVN-Kubernetes network, typically a KubeVirt VM acting as a
downstream router, has no way to obtain a routed IPv6 prefix via DHCPv6 Prefix
Delegation (PD), have OVN route that prefix to it, and keep the prefix and route stable
across live migration. OVN's native DHCPv6 responder is stateless and cannot emit
`IA_PD`, and there is no controller to allocate a prefix, route it, or persist it.

## Goals

* Allocate a delegated IPv6 prefix (configurable length, default `/64`) from an
  operator-defined pool, to workloads that explicitly request one.
* Serve the prefix to the workload over DHCPv6-PD from the OVN dataplane (no responder
  pod).
* Program the route for the delegated prefix in OVN automatically, or leave routing to
  an external owner (`routeMode: None`).
* Preserve the delegated prefix, its route, and the interface address unchanged across
  KubeVirt live migration, reusing the existing `IPAMClaim` persistence mechanism.
* Advertise the delegation pool aggregate with RouteAdvertisements (dual-stack-aware).

## Non-Goals

* IPv4. DHCPv4 has no prefix delegation; this feature is IPv6-only.
* Nested or recursive delegation (the workload sub-delegating to its own clients).
* Managing the workload's downstream RA/DHCP toward its interior clients (the
  guest's responsibility, not OVN-Kubernetes').
* More than one delegated prefix per client, or multiple `IA_PD` per request.
* Allocating a prefix to every workload on a PD-enabled network. Allocation is
  opt-in per workload (see API Details), never network-wide-automatic.
* NAT for the delegated prefix (handled by the workload or the edge).
* Lease/pool/allocation state in the OVN dataplane. Allocation stays in the controller;
  OVN core only emits a CMS-supplied prefix.
* DHCPv6-PD to non-KubeVirt, non-Layer2-primary workloads in the initial release.

## Introduction

Edge and router VMs (virtual CPEs, lab gateway VMs, NFV functions) expect a routed
prefix delegated over DHCPv6-PD on their uplink, which they then re-advertise to an
interior LAN. On bare metal or with a traditional hypervisor this is served by a DHCPv6
server that supports PD plus a routing layer that routes the delegated prefix back to
the requesting router.

OVN-Kubernetes already provides most of the substrate for this on a Layer2
(Cluster)UserDefinedNetwork:

* OVN-IPAM allocates interface addresses, and persistent IPs (`IPAMClaim`) keep them
  stable across live migration.
* RouteAdvertisements and FRR-K8s can advertise network subnets over BGP.
* OVN's native DHCPv6 answers SOLICITs inline from the dataplane (`ovn-controller`),
  with options supplied by the CMS in the `DHCP_Options` table.

The missing piece is prefix delegation itself: allocating a prefix, routing it to the
workload's logical switch port, persisting it across migration, and having the dataplane
emit the `IA_PD`/`IA_PREFIX` option. OVN's DHCPv6 responder today has no server-side
`IA_PD` support: it can request a prefix as a client (the `pinctrl` prefix-delegation
relay), but it cannot serve one.

Operators work around this with an external DHCPv6 server plus manual route injection.
That approach re-implements migration re-pointing by hand, runs as a schedulable pod
(an eviction window on every reschedule), and fights OVN's inline DHCPv6 responder for
the same SOLICITs. This OKEP makes PD a native, dataplane-served capability instead.

## User-Stories/Use-Cases

Story 1: Routed prefix for a router VM

1. **As a** platform operator
2. **I want** to define an uplink network with a prefix-delegation pool and have a
   router VM opt in to a delegation
3. **so that** the VM gets a stable `/64` it can advertise to its interior LAN, routed
   automatically by OVN, without every other VM on the network consuming the pool.

Story 2: Migration without renumbering

1. **As a** VM owner
2. **I want** to live-migrate my router VM
3. **so that** its delegated prefix, interface address, and route do not change, and the
   interior clients behind it see no renumbering.

## Proposed Solution

A controller in ovn-kubernetes owns the stateful parts (prefix allocation, persistence,
route programming, and RouteAdvertisements registration), and OVN's dataplane answers the
PD request after a small enabling change in OVN core. End to end:

1. An operator configures a delegation pool on a Layer2 (Cluster)UserDefinedNetwork.
2. A workload opts in (annotation, below). The controller allocates a prefix from the
   pool and stores it in the workload's `IPAMClaim.Status.delegatedPrefixes`.
3. The controller writes the prefix into the port's `DHCP_Options` (`ia_pd` key) and,
   when `routeMode: Static` (the default), programs a `Logical_Router_Static_Route` for
   the prefix with the VM's uplink address as next hop. Under `routeMode: None` it skips
   the route and leaves reachability to an external owner.
4. OVN's dataplane DHCPv6 responder emits the `IA_PD`/`IA_PREFIX` from `DHCP_Options`
   (stateless; the value is CMS-supplied like every other DHCP option; this is the
   OVN-core dependency, WI-1 below).
5. RouteAdvertisements advertises the pool aggregate upstream.

On migration the `IPAMClaim` re-binds the same prefix on the target pod, and because the
VM's uplink address is itself persistent on the Layer2 network, the next hop, route, and
`DHCP_Options` re-resolve to identical values. Nothing renumbers.

### API Details

Two surfaces: an operator pool on the network, and a per-workload opt-in.

**Pool (operator).** Extend the existing `(Cluster)UserDefinedNetwork` Layer2 spec with
a `prefixDelegation` stanza, rather than introducing a new top-level CRD. This keeps the
configuration cohesive with `subnets` and scoped to the network that owns it.

```go
// Layer2Config (and the CUDN equivalent) gains:

// +optional
PrefixDelegation *PrefixDelegationConfig `json:"prefixDelegation,omitempty"`

// PrefixDelegationConfig configures DHCPv6-PD on a Layer2 primary network.
// +kubebuilder:validation:XValidation:rule="self.delegatedLength > 0 && self.delegatedLength <= 128",message="delegatedLength must be in (0,128]"
type PrefixDelegationConfig struct {
    // Pool is the IPv6 CIDR from which prefixes are delegated. Must not overlap the
    // network subnets, join/infra subnets, or another pool.
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:XValidation:rule="self.contains(':')",message="pool must be an IPv6 CIDR"
    Pool string `json:"pool"`

    // DelegatedLength is the prefix length handed to each workload. Must be longer
    // (numerically greater) than the pool's prefix length.
    // +kubebuilder:validation:Required
    DelegatedLength int32 `json:"delegatedLength"`

    // RouteMode controls whether OVN-Kubernetes programs the route that makes the
    // delegated prefix reachable inside OVN.
    //   Static (default): OVN-Kubernetes installs `delegated-prefix -> workload-port` and GCs it
    //     on claim deletion.
    //   None: OVN-Kubernetes delivers and persists the prefix but installs no route; an external
    //     component (e.g. the guest re-advertising via BGP, or another route controller)
    //     owns reachability. With None and nothing else routing the prefix, the prefix is
    //     delivered but unreachable.
    // +kubebuilder:validation:Enum=Static;None
    // +kubebuilder:default=Static
    // +optional
    RouteMode string `json:"routeMode,omitempty"`

    // AdvertiseMode controls how the delegation is advertised to RouteAdvertisements
    // (external BGP). Orthogonal to RouteMode (intra-OVN reachability). Requires
    // RouteAdvertisements/FRR-K8s to be configured to select this network.
    //   None (default): OVN-Kubernetes announces nothing upstream. External announcement (if any)
    //     is owned elsewhere: RA configured directly, the guest re-advertising, or the
    //     fabric routing the aggregate statically.
    //   Aggregate: OVN-Kubernetes registers the pool aggregate (one route regardless of how many
    //     prefixes are delegated) and installs the pool blackhole route so traffic to
    //     not-yet-delegated space is dropped locally. Best at scale (no per-VM churn).
    //   Individual: OVN-Kubernetes registers each delegated prefix as it is granted and withdraws
    //     it on release. Precise (no blackhole) but one route per delegation; the
    //     advertised set is driven from IPAMClaim status, so it requires persistent IPs.
    // Default None because external announcement depends on a separate subsystem that is
    // absent on most clusters and is an outward-facing action.
    // +kubebuilder:validation:Enum=None;Aggregate;Individual
    // +kubebuilder:default=None
    // +optional
    AdvertiseMode string `json:"advertiseMode,omitempty"`
}
```

* The whole `prefixDelegation` stanza is **immutable after creation** (matching how
  Layer2 network config is treated today). To change the pool, recreate the network.
* `family` is intentionally absent: PD is IPv6-only and the family is inferred from the
  `Pool` CIDR. A non-IPv6 `Pool` is rejected at admission.
* Cross-field checks (pool vs. `subnets`, `delegatedLength > poolLength`) are enforced in
  the network admission webhook, where the sibling fields are available.
* `advertiseMode` other than `None` requires `routeMode: Static` (CEL): advertising a
  prefix upstream that OVN-Kubernetes does not route would blackhole traffic to it.

**Opt-in (per workload).** A workload requests a delegation with the annotation
`k8s.ovn.org/prefix-delegation: "true"` on the pod (for KubeVirt VMs, set on the
`VirtualMachineInstance`/pod template so the migration-target pod inherits it). Absent
the annotation, no prefix is allocated. The documented alternative is a typed field on
the network-selection element; the annotation leads because it needs no multus/NSE
schema change and rides the existing pod-template propagation that already makes
KubeVirt migration work.

**Status.** The bound prefix surfaces on the workload's `IPAMClaim.Status` via a new
`delegatedPrefixes []string` field (a list, for forward-compatibility, though the common
case is one). `IPAMClaim` is owned by the external
`github.com/k8snetworkplumbingwg/ipamclaims` project, so this field is an **explicit
upstream dependency** (see Implementation and Backwards Compatibility).

### Implementation Details

**OVN core dependency (WI-1): stateless `IA_PD`/`IA_PREFIX` emit.**
The OVN DHCPv6 responder is taught to emit a delegated prefix supplied by the CMS in
`DHCP_Options`, with no pool/lease/allocation in the dataplane. Separate change in
`ovn-org/ovn`, submitted to `ovs-dev@openvswitch.org`. As implemented:

* A server-emittable option `ia_pd` (DHCPv6 option code 25), type `ia_pd`, registered in
  `lib/ovn-l7.h` and `supported_dhcpv6_opts[]` (`northd/northd.c`).
* `lib/actions.c` lexes/parses/encodes the option; the prefix value (e.g.
  `2001:db8:1000::/64`) is encoded into the action buffer as `[plen][16-byte prefix]`,
  `plen` from the CIDR mask.
* `controller/pinctrl.c` `compose_out_dhcpv6_opts` emits `IA_PD`/`IA_PREFIX` only when
  the client's request carries an `IA_PD`, echoing the client's IAID, with infinite
  lifetimes (`OVS_BE32_MAX`), mirroring the stateless `IA_NA` emit. The emit is gated to
  Solicit/Request like IA_NA.
* `DHCP_Options.options` is an open string-map (no schema change); the prefix is the new
  `ia_pd` key, documented in `ovn-nb.xml`.

DHCPv6 request-type behavior (OVN core): only Solicit (answered with Advertise) and Request
(answered with Reply) carrying `IA_PD` get an `IA_PD`/`IA_PREFIX`; `pd_requested` is forced false for
every other message type, so they never carry PD. OVN's stateless responder does not
serve Renew or Rebind at all: they fall through to the unsupported-type path and are
dropped (pre-existing OVN behavior; with infinite lifetimes there is nothing to renew).
Release elicits a status-only Reply (no lease state to free). OVN core can emit `IA_PD`
independently of `IA_NA`, but OVN-Kubernetes always pairs PD with a managed address (next section),
so a PD-only client is out of scope at the OVN-Kubernetes layer in this release.

**Allocator.** A prefix-range allocator in `pkg/allocator/ip` hands out `delegatedLength`
prefixes from the pool (today's allocator is flat per-address; this closes the standing
range-allocation FIXME). Guarantees no double-allocation, reports exhaustion as an error
condition (surfaced on the `IPAMClaim`), and supports release/reclaim. On controller
startup the allocator rebuilds its in-use set by listing existing
`IPAMClaim.Status.delegatedPrefixes` **before** servicing any new allocation, so a
restart cannot double-allocate an in-use prefix.

**Persistence (migration-stability): upstream IPAMClaim change.** Migration-stability
reuses the persistent-IP path: `pkg/persistentips` and `pkg/allocator/pod/pod_annotation.go`
allocate and persist the prefix in `IPAMClaim.Status.delegatedPrefixes`, re-allocating
the *exact* prior value on the migration-target pod (tolerating `ErrAllocated` when the
target re-requests its own prefix). Because `IPAMClaim` is vendored from
`k8snetworkplumbingwg/ipamclaims`, this requires, as an upstream prerequisite:
an ipamclaims API PR adding `delegatedPrefixes` to the status, CRD regeneration, client
regeneration and re-vendor in ovn-kubernetes, and an upgrade story for clusters whose CRD predates
the field (the controller treats an absent field as "no delegation" and re-derives it).

**Route.** When `routeMode: Static`, `pkg/kubevirt/router.go` programs the prefix route
via `pkg/libovsdb/ops/router.go`
(`CreateOrReplaceLogicalRouterStaticRouteWithPredicate`) on the network's logical router:

```text
Logical_Router_Static_Route:
  ip_prefix   = <delegated /64>          # dst-ip route
  nexthop     = <VM uplink IPv6 (IA_NA)> # the VM's own managed address on this L2 net
  policy      = dst-ip
  external_ids: { owner = "pd:<ipamclaim-uid>" }
```

The uniqueness predicate matches on `external_ids:owner`, so re-reconciliation replaces
rather than duplicates, and a stale route is garbage-collected when its `IPAMClaim` is
deleted. On a Layer2 network the VM's uplink address is persistent, so the next hop does
not change on migration. The route value is identical on the target; reconciliation only
re-asserts it. (No `output_port`/per-node host-route is needed because the destination is
reached via the VM's stable L2 address, unlike the per-pod node host-route precedent.)
Under `routeMode: None` the controller programs no route and asserts no ownership over
reachability for the prefix; allocation, persistence, and `DHCP_Options` are unaffected.

**DHCP options (feeds WI-1).** `pkg/kubevirt/dhcp.go` (`EnsureDHCPOptionsForLSP` /
`ComposeDHCPv6Options`) writes the delegated prefix into the LSP's `DHCP_Options` as the
`ia_pd` key, gated to Layer2-primary-UDN VMs that already require DHCP and carry the
opt-in annotation.

**Advertise.** `advertiseMode` selects how delegations reach RouteAdvertisements, and is
inert unless an RA selects the network (the advertised prefixes flow through the same
per-network path as the pod-network subnets). `Aggregate` registers the **pool aggregate**
(e.g. the `/52`), not the individual `/64`s: one stable route regardless of how many
delegations exist, avoiding per-VM BGP churn at scale; because the aggregate covers
unallocated space, the controller also installs a discard/blackhole route for the pool on
the network-scoped router so traffic to not-yet-delegated prefixes is dropped locally
rather than blackholed by the upstream fabric (the more-specific per-`/64` routes shadow it
via OVN longest-prefix match, so only undelegated space is discarded). `Individual`
instead registers each delegated `/64` as it is granted and withdraws it on release. The
advertised set is gathered from `IPAMClaim` status, so the RA controller watches IPAMClaims
to refresh it; precise (no blackhole needed) but one route per delegation. `Aggregate` is
the recommended default for scale; `Individual` suits small or sparsely-used pools where
advertising unallocated space is undesirable.

**Reconciliation (state machine).** The controller is edge-triggered on IPAMClaim and pod
events; each transition is idempotent and keyed by the IPAMClaim UID:

| Event | Action |
|-------|--------|
| Controller start | List all `IPAMClaim`s; rebuild allocator in-use set from `delegatedPrefixes`; re-assert routes/DHCP_Options; **gate new allocation until sync completes** |
| Pod add (opt-in, no prefix) | Allocate from pool; persist in `IPAMClaim`; write `DHCP_Options`; program route |
| Pod add (opt-in, prefix in claim) | Re-allocate exact prior prefix (tolerate `ErrAllocated`); re-assert route and `DHCP_Options` |
| Migration (target pod add) | Same as "prefix in claim"; value unchanged, so no renumbering |
| Migration (source pod delete) | No route/prefix change; the claim still binds the prefix; only the source LSP is torn down by existing logic |
| IPAMClaim delete | Release prefix to pool; delete route (predicate on owner) + `DHCP_Options` key; under `advertiseMode: Individual` the IPAMClaim event re-reconciles RA and withdraws that prefix (under `Aggregate` the pool registration is network-lived and unchanged) |

**Always-on / control-plane split.** The per-packet PD answer is served by
`ovn-controller`, the per-node dataplane: no schedulable responder, no eviction window.
Allocation/route-granting runs in the controller and is off the packet path: a controller
restart delays only *new* grants; existing delegations keep routing because the route and
`DHCP_Options` are native OVN state.

### Testing Details

* **Unit:** prefix allocator (no double-allocation, exhaustion, release/reclaim, restart
  rebuild-from-claims); persistence round-trip (allocate, persist, then re-allocate the exact
  prior value on a target pod). OVN-core: DHCPv6 option parse/encode and a request-matrix
  emit test (IA_NA-only yields no IA_PD; IA_PD present yields IA_PD/IA_PREFIX with correct option
  codes 25/26, IAID echo, prefix length, infinite lifetimes; PD omitted for every
  non-Solicit/Request message type; Renew/Rebind not served; Release yields a status-only Reply).
* **E2E (`test/e2e`):** opt-in VM on a PD-enabled network receives a `/64`; the
  prefix route is present with the VM uplink as next hop; the VM reaches upstream;
  live-migrate and confirm prefix, route, and address unchanged; restart the controller and confirm existing
  delegations not dropped, no double-allocation; a non-opt-in VM on the same network gets
  no prefix.
* **API:** admission of `prefixDelegation` (immutability, pool/subnet overlap rejection,
  `delegatedLength` bounds, non-IPv6 pool rejection).
* **Scale:** 100 to 1000 delegations from one pool; assert a single aggregate RA route
  (not 1000) and no allocation races.
* **Cross-feature:** persistent IPs, RouteAdvertisements/BGP, dual-stack networks, and
  rolling OVN upgrade (mixed `ia_pd`-capable nodes).

### Documentation Details

* New user-facing docs on ovn-kubernetes.io: enabling a delegation pool on a
  `(Cluster)UserDefinedNetwork`, the `prefixDelegation` fields, the per-workload opt-in
  annotation, and a router-VM example that RAs the delegated prefix downstream.
* Add the OKEP to `mkdocs.yml` under the OKEPs navigation when the PR is opened.

## Risks, Known Limitations and Mitigations

* **Cross-project IPAMClaim dependency.** Persistence needs a new status field in
  `k8snetworkplumbingwg/ipamclaims`. Mitigation: land the ipamclaims API change first as
  an explicit prerequisite; the controller tolerates the older CRD (absent field means no
  delegation) so OVN-Kubernetes upgrades don't hard-require the new CRD simultaneously. If that
  project is slow, the documented fallback is an OVN-Kubernetes-owned companion object keyed to the
  claim (see Alternatives).
* **OVN-core dependency (WI-1).** No prefix without the OVN `IA_PD` emit. Mitigation:
  WI-1 is small, stateless, self-contained, submitted upstream first; downstream builds
  carry it until it merges. The controller gates the feature on OVN `ia_pd` capability.
* **Mixed OVN versions during rolling upgrade.** Some `ovn-controller`/`northd` may not
  support `ia_pd`. Mitigation: capability probe (below) gates enablement until all
  relevant nodes support it; an `ia_pd` `DHCP_Options` key on an older OVN is ignored
  (open map) rather than fatal.
* **PD-only clients unsupported (OVN-Kubernetes layer).** The controller always pairs PD with a
  managed `IA_NA` address. A client soliciting only `IA_PD` is out of scope here, even
  though OVN core can emit IA_PD independently. Mitigation: documented; the router-VM use
  case always has an uplink address.
* **Pool exhaustion** is surfaced as an allocation error condition on the `IPAMClaim`,
  not a silent failure.
* **Aggregate advertisement covers unallocated space.** Mitigation: controller installs a
  blackhole/discard route for the pool so undelegated prefixes are dropped locally.
* **`routeMode: None` delivers an unreachable prefix unless an external owner routes it.**
  This is by design (decoupling delivery from reachability) but is an operator footgun.
  Mitigation: document it; default is `Static`; consider a status condition noting that no
  OVN route was programmed so the state is observable.
* **No deployment-specific values in code.** Pool and length are configuration; no
  site-specific prefixes appear in tree.

## OVN-Kubernetes Version Skew

Target the next minor release (confirm the exact version against repo milestones when the
tracking issue is opened). Gated behind a new feature gate `PrefixDelegation` (Alpha),
which itself requires `MultiNetwork`, `NetworkSegmentation`, and persistent IPs, and
`RouteAdvertisements` when `advertiseMode` is not `None`. It requires an OVN build that includes the
WI-1 `ia_pd` emit; the controller detects capability by checking the OVN NB schema /
`ovn-controller` version and keeps the feature disabled until every relevant node
supports `ia_pd`. Prerequisite matrix: Layer2 primary (Cluster)UDN, IPAM enabled,
persistent IPs enabled, KubeVirt for the VM use case, optional RouteAdvertisements.

## Backwards Compatibility

* The OVN-Kubernetes API change is purely additive: an optional `prefixDelegation` stanza and an
  optional opt-in annotation. Networks/workloads that don't set them behave exactly as
  before.
* `IPAMClaim.Status.delegatedPrefixes` is a new optional field in the upstream ipamclaims
  CRD. A cluster with the older CRD has no delegation; upgrading the CRD is
  additive and non-breaking. The controller never requires the field to be present.
* `DHCP_Options.options` is an open map; the `ia_pd` key is ignored by an OVN that
  doesn't understand it. On downgrade, a stale `ia_pd` key in an existing row is inert.
* No change to existing `IA_NA` behavior: a client that doesn't request `IA_PD` gets the
  same reply it does today.

## Alternatives

* **Per-`/64` dynamic RouteAdvertisements source.** Advertise each delegated prefix as
  granted: precise (no blackhole) but causes BGP churn at scale and needs a dynamic
  advertisement source in RA (the RA controller watching IPAMClaim status). Offered as the
  opt-in `advertiseMode: Individual`, not the default: `Aggregate` is the recommended mode
  for scale, and `Individual` is available where advertising unallocated pool space is
  undesirable.
* **OVN-Kubernetes-owned persistence object** instead of extending IPAMClaim. IPAMClaim is the
  chosen home because the delegated prefix has a 1:1 ownership and lifecycle match with
  the interface address that already lives in the claim: both are allocated to the same
  VM interface, persisted to the same owner, re-bound to the same migration-target pod,
  and released together. Putting the prefix in the same object means one reconcile loop,
  one deletion path, and direct reuse of the proven persistent-IP machinery
  (`pkg/persistentips` + the `pod_annotation.go` re-allocate-exact-prior-value path).
  `Status.delegatedPrefixes` is the generic counterpart of `Status.ips` and is ignored by
  CNIs that do not do PD. A separate OVN-Kubernetes-owned object avoids the cross-project dependency
  but is **less** clean: it introduces a second per-VM object that must be created, GC'd,
  and migrated in lockstep with the claim (re-introducing the very two-object consistency
  race this design otherwise avoids) and duplicates the migration machinery. It is kept
  only as the fallback if the upstream ipamclaims field cannot land in time. IPAMClaim is
  an API ovn-kubernetes already co-evolves (the persistent-IP feature is built on it), so a
  small additive status field is expected to be low-friction.
* **External DHCPv6-PD responder + manual route injection** (zero-fork interim). Works on
  stock clusters but re-implements migration re-pointing, runs as a schedulable pod
  (eviction-sensitive), and competes with OVN's inline DHCPv6 responder. Right bridge,
  wrong long-term home.
* **Stateful PD in OVN core (pool/lease in `pinctrl`).** Rejected: contradicts OVN's
  stateless native-DHCP design and puts allocation in the dataplane. Allocation belongs
  in the CMS.
* **New top-level `PrefixDelegationPool` CRD** instead of a network field. Kept as the
  documented alternative; the field on `(Cluster)UserDefinedNetwork` is the smaller, more
  cohesive surface and is what this OKEP leads with.
* **Client-side PD (OVN as the requesting router).** Already exists in `pinctrl`, but is
  the inverse feature (OVN obtaining a prefix from upstream), not serving prefixes to
  workloads.

## References

* OVN-core companion change (WI-1): stateless `IA_PD`/`IA_PREFIX` emit in `ovn-org/ovn`
  (`lib/ovn-l7.h`, `lib/actions.c`, `controller/pinctrl.c`, `northd/northd.c`), submitted
  to `ovs-dev@openvswitch.org`.
* RFC 8415: Dynamic Host Configuration Protocol for IPv6 (DHCPv6), prefix delegation
  (§18.3.3 server response rules).
* `github.com/k8snetworkplumbingwg/ipamclaims`: the IPAMClaim CRD (persistence anchor).
* OKEP-5296: OVN-Kubernetes BGP Integration (RouteAdvertisements).
* OKEP-5233: Predefined addresses for primary UDN workloads (IPAMClaim/persistent IPs).
</content>
