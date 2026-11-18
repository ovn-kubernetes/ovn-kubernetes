# OKEP-7282: State-preserving planned EgressIP handover

* Issue: [#7282](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/7282)
* Status: Draft for design discussion; no runtime implementation in this PR.

## Problem Statement

Moving an EgressIP between nodes can break established TCP connections even
when new connections recover quickly. The address moves, but the destination
does not inherit the source node's live NAT translation and conntrack state.

## Goals

- Preserve supported established TCP connections during planned EgressIP moves
  between healthy nodes.
- Make preservation opt-in and leave existing EgressIP behavior unchanged by
  default.
- Establish a consistent state-transfer and cutover protocol, including packets
  and connections arriving during preparation.
- Bound preparation, buffering, memory consumption and cleanup; make failure
  visible to operators.
- Define validation that detects a broken existing connection even if a new
  connection succeeds.

## Non-Goals

- Guaranteeing sub-second handover or zero packet loss from the lab results.
- Preserving state after an unexpected source-node failure. That requires
  state replicated before failure and is a separate extension.
- Initial support for secondary-interface EgressIPs, local gateway mode, UDP,
  SCTP, or hardware-offloaded conntrack.
- Changing node reachability detection or replacing ARP/NDP ownership updates.

The initial proposed implementation covers TCP on primary machine-network
EgressIPs in the default cluster network, with both IPv4 and IPv6. Additional
network types require their own zone and datapath validation before support.

## Introduction

EgressIP assignment and datapath programming are asynchronous. On an assignment
change, network controllers remove old logical NAT/routing configuration and
create the new configuration. Recreating a logical NAT rule does not transfer
the previous node's kernel conntrack entries.

A new translation can choose another source port. Even retaining the port is
insufficient if return traffic lacks its reverse mapping or required TCP state.
Consequently, assignment recovery and established-connection continuity are
different properties.

### Lab evidence and its limits

A lab using OpenShift 4.22.18, based on OVN-Kubernetes commit
`dc35130bb7d144854d80f4673493977ed26b56f1`, reproduced this after removing the
egress-assignable label from a healthy node. Direct IPv4 HTTPS excluded proxy
use and runtime DNS resolution. Packet captures and per-node conntrack samples
showed different translated ports for the same original connection after the
move; the source retained its original entry.

For example, the observed failure has this shape (addresses are documentation
prefixes and ports are illustrative):

```text
Original:     10.244.0.2:45000 -> 198.51.100.20:443
Before move:  192.0.2.10:38192 -> 198.51.100.20:443
After move:   192.0.2.10:38976 -> 198.51.100.20:443
```

To reproduce, place the selected pod on a third node, label source and
destination nodes, establish a long-lived TCP/TLS connection to a controlled
external server, remove the source's eligibility label, and exchange data on
the original socket while capturing both worker uplinks and conntrack state.
The server must have a known idle timeout and the test must assert that no
reconnect occurred. Use a separate fresh-connection probe to measure recovery.

A separate experiment established TLS, held the socket, performed a move, and
sent a request on that same socket without reconnecting:

| Case | Result |
|---|---|
| No migration | HTTP response received |
| Migration without state transfer | Read reached the configured 8-second deadline |
| Restore kernel SNAT binding and external-bridge entry before A to B | Same TLS connection received an HTTP response in 0.367 seconds |
| Restore state before B to A | Same TLS connection received an HTTP response in 0.275 seconds |

The response times start when the held request is sent after the move; they
are not failover durations. The endpoint returned HTTP 302, which demonstrates
transport/TLS continuity rather than application success. A long-idle control
also reproduced peer EOF without migration, so such trials were excluded from
the continuity comparison.

The prototype restored one idle, established IPv4 connection at a time. It did
not establish correctness under concurrent state changes, collisions, active
transfers, scale, or source failure. Current upstream master has not been
reproduced independently. The evidence motivates this proposal; it does not
constitute an integrated fix or a general connection-preservation guarantee.

## User-Stories/Use-Cases

As an administrator performing planned egress-node maintenance, I want existing
supported TCP sessions to survive moving an EgressIP while both nodes are
healthy, so maintenance does not require every application to reconnect.

As an operator, I want to know whether a preservation attempt completed,
aborted while retaining the old owner, or fell back after source failure, so I
can distinguish successful assignment from preserved connection state.

## Proposed Solution

Introduce an opt-in planned-handover policy. The coordinator prepares a
destination without changing the active assignment, obtains a consistent
checkpoint of the affected state, restores it in the destination datapath,
then commits the assignment change and retires old state.

**Design prerequisite:** copying a snapshot while traffic continues is not a
correct handover protocol. Before implementation, this proposal must identify
a supported datapath mechanism that fences/quiesces the affected traffic,
accounts for packets already in flight, and provides bounded buffering or an
explicitly defined alternative. The following state machine is a contract for
that mechanism, not evidence that it already exists in OVN/OVS.

### API Details

Candidate EgressIP API:

```yaml
spec:
  handoverPolicy: PreserveConnections
```

- `Immediate` is the default and preserves today's behavior.
- `PreserveConnections` requests the supported planned-handover path when the
  old and candidate nodes are healthy and compatible.
- With preservation selected, removing the eligibility label requests a
  handover. The old node becomes unavailable for new allocations but keeps
  serving the current assignment until commit or an explicit forced move.
- Unsupported network, gateway, protocol or datapath combinations must be
  documented. Where incompatibility is knowable, reject the request or report
  an explicit unsupported condition before attempting preservation; never
  report preservation success for an unsupported path.
- A preservation preparation timeout keeps the old healthy assignment and
  reports failure. An administrator can select `Immediate` to force existing
  migration behavior. Exact deadline configuration and policy-update semantics
  require API review.
- Unexpected source unreachability uses the existing availability-oriented
  failover path and reports that continuity could not be preserved. Opting in
  does not disable automatic failover or imply warm-standby replication.

The coordinator needs bounded status metadata for a handover ID/epoch, source
and destination node identities, phase, deadline and conditions. Node UIDs and
the EgressIP UID/generation bind acknowledgements to the intended resources.
`status.items` remains the active assignment until commit. Neither flow tuples
nor serialized conntrack entries belong in Kubernetes status or annotations.

Candidate phases are `Preparing`, `Quiescing`, `Restoring`, `Committing`,
`Cleaning`, and `Aborted`. The exact schema and field ownership remain review
items; this PR does not modify the CRD.

### Implementation Details

#### Coordinator and ownership

The cluster-manager selects the destination and records the handover epoch
without immediately invalidating the source's active status. Assignment
validation must distinguish a source serving an in-progress handover from an
eligible target for a new allocation.

Only the coordinator commits ownership. A destination's imported state must
remain passive: restoration alone must not advertise the EgressIP, activate a
second public owner, or change the pod's route. Stale acknowledgements cannot
commit a newer handover.

#### Consistent checkpoint and cutover

The proposed sequence is:

1. Verify both nodes' capabilities and the destination's network/IP capacity.
2. Prepare the destination and reserve bounded transfer/buffer resources while
   the source remains active.
3. Fence/quiesce the affected datapath using the mechanism agreed during
   design review; account for existing in-flight packets and state updates.
4. Export a final consistent checkpoint and restore it in the destination's
   corresponding conntrack domains. A snapshot-plus-delta implementation still
   needs a final consistency barrier; an empty event queue is not sufficient.
5. Verify restoration and acknowledge readiness for the exact epoch.
6. Commit assignment, routing and address ownership with a defined ordering
   relative to the fence. Define how queued packets and traffic still arriving
   at the old node reach the new owner without duplicate processing.
7. Release the fence and retire staged/old state after the agreed grace period.

Buffer overflow, resource exhaustion or failure before commit must release the
source fence, remove staged destination state and retain the healthy source.
Coordinator restarts resume from durable metadata and reconcile actual node
state. Failure after commit must not blindly reactivate stale source state.

The cutover mechanism, its backend support and its rollback boundary are
blocking design questions. They must be resolved before claiming the policy
preserves arbitrary active TCP traffic.

#### State identity and restoration

The node implementation must identify the relevant network, EgressIP and
original/reply tuples. Export includes the NAT binding and required status,
marks, labels, TCP metadata and remaining lifetime. Restoration must validate
the result, including the translated tuple and NAT status, before acknowledging.

Conntrack zone numbers are local implementation details. Map logical domains
to destination-local zones; do not copy a numeric zone blindly. The lab used
SNAT zone 0 and external-bridge zone 64000 with mark 1, but those values are
not a portable production contract.

Restoring tuple fields alone is insufficient. The lab needed the kernel NAT
attributes to establish an active `SRC_NAT` binding. The prototype's fixed TCP
metadata and permissive assumptions must not be adopted as a production
restore strategy. Define supported kernel/datapath state semantics, including
sequence/window handling, rather than relaxing global TCP validation.

Conflicting destination entries cause preparation to fail before commit.
Cleanup is scoped to entries owned/staged by the handover and must not flush
shared conntrack domains or unrelated traffic.

#### Transport and security

State transfer requires authenticated, authorized node-to-node communication
and coordinator-bound handover identity. Plaintext health-check compatibility
must not permit unauthenticated state import. Reusing a health-check endpoint
versus adding a separate service is a design decision, not an assumption.

The protocol needs version negotiation, certificate rotation behavior,
deadlines, cancellation, bounded batches, backpressure, and connection/byte
limits. Peers must validate network ownership, tuple scope and destination
authorization before importing state. Connection state must not be exposed in
ordinary logs or Kubernetes annotations.

#### Component and topology boundaries

- Cluster-manager: assignment reservation, epoch/status and commit coordination.
- Node-side agent: capability reporting, state export/import, validation,
  datapath fencing integration and scoped cleanup.
- Network controller: logical route/NAT programming and handover-aware lifetime
  of the old configuration.
- OVN/OVS backend: the consistent cutover primitive required above, if existing
  supported interfaces cannot provide it.

The first implementation is scoped to shared gateway and the default network.
Local gateway and secondary interfaces have different NAT/routing domains and
are excluded initially. UDNs and offloaded datapaths require explicit identity,
zone and capability work. IPv4 and IPv6 must share the same correctness
contract; IPv6 additionally needs validated NDP ownership ordering.

### Testing Details

Unit and component tests must cover epoch/generation checks, local-zone
mapping, state encoding/restoration, conflict rejection, partial batches,
timeouts, cancellation, ownership fencing, rollback and coordinator restarts.

E2E acceptance requires a controlled external TCP/TLS server with known idle
behavior. Establish a socket before migration, exchange data before and after
cutover, and verify the same connection continues without reconnecting. Also
run fresh-connection probes, but do not use them as the continuity assertion.

Required cases include active bidirectional transfers, connections created
during preparation, server-initiated data during cutover, two pods sharing a
source port to the same endpoint, multiple EgressIPs, IPv4/IPv6, repeated moves,
rollback, destination failure and source failure before/after commit. Verify
the documented continuity-loss condition on unplanned source failure.

Scale tests must bound transfer duration, state volume, buffering and CPU while
measuring p50/p99 latency and loss at cutover. No sub-second claim is justified
until these measurements exist at a stated scale.

API tests cover defaults, enum validation, policy updates and status ownership.
Cross-feature tests cover NetworkPolicy/ACL isolation and unsupported UDN,
local-gateway, secondary-interface and offload behavior. The implementation PR
must identify a CI lane that actually executes each new E2E case.

### Documentation Details

Add the approved policy, supported combinations, maintenance sequence,
conditions, forced-move procedure and limitations to the EgressIP feature and
API documentation. Explain assignment recovery separately from connection
continuity, and describe unplanned failure independently from planned handover.

## Risks, Known Limitations and Mitigations

- A non-atomic snapshot can lose or corrupt live state. Consistent cutover is
  a design/implementation prerequisite, not a follow-up optimization.
- State import is security-sensitive and can cross network boundaries if
  incorrectly scoped. Require authenticated peer identity and tuple/network
  authorization before activation.
- Preparation may delay node maintenance. Keep the source serving on failure
  and expose a condition plus an explicit forced-move path.
- Large tables or insufficient buffer capacity can exceed the deadline. Bound
  resources and abort before commit; measure supported scale.
- Kernel, OVS, NAT-helper and offload state may not be fully portable. Negotiate
  capabilities and reject incompatible preservation attempts.
- The lab proof covers idle established connections only. It does not resolve
  the active-traffic consistency prerequisite or unexpected source loss.

## OVN-Kubernetes Version Skew

No release target is committed by this draft. After design approval, select a
milestone and a feature-gated implementation with API/backend compatibility
review. Existing open milestones do not imply the backend prerequisites are
available.

During rollout, an old component cannot participate in preservation. The
coordinator must require compatible source, destination, network-controller
and backend capabilities. Keep default `Immediate` behavior unchanged and
report an opted-in preservation attempt as unavailable when those capabilities
are missing; do not silently claim continuity.

Downgrade procedures must finish or abort active handovers and return policies
to the supported default before removing participating components. The API
review must address how an older coordinator handles persisted pending status
and an opted-in object; merely adding an optional field does not solve skew.

## Backwards Compatibility

Existing objects and automation retain immediate label-removal semantics by
default. The proposed opted-in policy deliberately allows the old healthy node
to continue serving during preparation after its eligibility label is removed;
document that distinction for maintenance automation.

No CRD, gateway configuration, health-check behavior or runtime default changes
in this design-only PR. Existing E2E cases remain unchanged; implementation
adds separate continuity coverage.

## Alternatives

### Application reconnect/retry

This can recover requests after a move and remains useful, but does not
preserve an established transport session. It is not equivalent to this goal.

### Change source-port allocation only

Port preservation alone cannot restore reverse NAT or TCP state and must
handle collisions between pods. It is not a general continuity solution.

### Drain without state transfer

Keeping all old connections on the source until they finish may avoid some
planned disruptions, but can block indefinitely and needs a return-path/flow
affinity design if the same EgressIP is activated elsewhere. It does not meet
a bounded move requirement by itself.

### Continuous replication to a standby

This is a candidate extension for unplanned failure. It adds steady-state
cost, destination selection and consistency requirements and is outside the
initial planned-handover scope.

### Stateless NAT

General many-to-one EgressIP translation needs collision handling and reverse
mapping. Replacing stateful NAT is a separate architecture, not a small fix to
the current handover.

## References

- [EgressIP design](../design/egressip.md)
- [EgressIP feature documentation](../features/cluster-egress-controls/egress-ip.md)
- [Shipped lab source: EgressIP reconciliation](https://github.com/openshift/ovn-kubernetes/blob/dc35130bb7d144854d80f4673493977ed26b56f1/go-controller/pkg/ovn/egressip.go)
- [Shipped lab source: stateful NAT builder](https://github.com/openshift/ovn-kubernetes/blob/dc35130bb7d144854d80f4673493977ed26b56f1/go-controller/pkg/libovsdb/ops/router.go)
- [OVN NAT port-range behavior](https://www.ovn.org/support/dist-docs/ovn-nbctl.8.html)
- [Kernel conntrack netlink interface](https://www.kernel.org/doc/html/latest/netlink/specs/conntrack.html)
