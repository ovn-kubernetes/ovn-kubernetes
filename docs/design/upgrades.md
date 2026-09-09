# Upgrades

This document describes how OVN-Kubernetes is expected to be upgraded in a
running cluster and what a code change must guarantee to be upgrade-safe.
OVN-Kubernetes is usually managed and upgraded by another operator (like
OpenShift's cluster-network-operator) or by platform-specific runbooks
with helm charts. That means that OVN-Kubernetes depends and implies
some of the platform's upgrade guarantees, which have not been clearly stated
before. Two past transitions, the move to interconnect
mode and the Layer2 transit router topology, are used as examples of
features that heavily depend on the upgrade model and the rules for upgrade-safe changes.

## The upgrade model

An upgrade moves a cluster from one OVN-Kubernetes release to the next one
while workload pods keep running. What is being replaced is the code running
in the OVN-Kubernetes pods and the manifests that deploy them; how those
manifests are delivered, whether through the Helm chart, a platform operator,
or hand-written YAML, is not part of the model. Nothing about the cluster's
traffic is allowed to depend on the upgrade being quick, atomic, or ordered.

### Supported paths

* Upgrades are supported from release N to release N+1 only. Skipping a
  release is not supported: a cluster on N that needs N+2 goes through N+1
  and completes that upgrade first. Compatibility guarantees in this document
  are therefore stated for a one-release window.
* Downgrades are not supported. Once any component of release N+1 has run in a
  cluster, the cluster is considered to be on N+1. Migrations may have
  rewritten state that release N cannot read, and there is no requirement that
  N+1 code leaves N-readable state behind.
* A patch release within the same minor version follows the same rules but is
  not expected to contain state migrations.

### Components

* `ovnkube-control-plane` Deployment: the cluster-manager. It allocates
  cluster-wide identifiers (node subnets, node IDs, transit switch and router
  IPs, tunnel keys, network IDs), publishes them as annotations on Nodes and
  NetworkAttachmentDefinitions, and aggregates per-zone status onto CRDs. It
  never talks to an OVN database.
* `ovnkube-node` DaemonSet: one pod per node running the zone controller
  (`ovnkube-controller`, programming the node-local NB database), the node
  agent (gateway, OpenFlow, CNI server), `northd`, `ovn-controller`, and the
  local NB/SB databases. Each node is its own zone and owns its OVN state.
* `ovnkube-identity`: the admission webhook restricting which annotations a
  node may write on its own Node object, and the approver for per-node
  certificate requests. It runs as a DaemonSet on control-plane nodes.

### Roll-out order between control-plane and node is not guaranteed

Platforms replace these components in different orders. The upstream upgrade
job (`test/scripts/upgrade-ovn.sh`, run by `ovn-upgrade-e2e` from the previous
release image to the PR image) scales `ovnkube-control-plane` to zero and then
runs the chart upgrade, which recreates the control plane with the new image
while the `ovnkube-node` DaemonSet is still rolling. That job therefore does
not enforce any order between the cluster-manager and the nodes, and it runs
its test suite only once the whole cluster is on the new release. OpenShift
upgrades worker nodes before control-plane nodes, so new ovnkube-node pods run
against an old cluster-manager for a while. Other platforms upgrade the control
plane first. Nodes upgrade one at a time and, where the platform also updates
the host OS, a node is drained and rebooted some time after its ovnkube pod was
replaced.

The consequence is that every combination inside the one-release window must
work:

* new `ovnkube-node` with old `ovnkube-control-plane`,
* old `ovnkube-node` with new `ovnkube-control-plane`,
* upgraded and non-upgraded nodes talking to each other across the
  interconnect for hours or days.

In central mode (not supported anymore) the order was fixed: nodes had 
to be upgraded before the master. A single
`ovnkube-master` programmed the shared NB database for the whole cluster, so
a new master could not start until every `ovnkube-node` was already able to
consume whatever the new master would write, and the master's one-shot
database migrations assumed all nodes were new. Interconnect removed that
constraint by giving every node its own database and its own migration.

Even though we want to keep supporting node-first upgrades, upgrading 
the cluster-manager first may simplify the upgrade procedure or improve
upgrade time because it allows the cluster-manager to start publishing 
new identifiers before any node needs them. Having ovnkube-node upgrade first
may require an extra round of restarts after the control-plane is upgraded
for ovnkube-node to pick up new identifiers.

### ovnkube-identity goes first

The one component with a real ordering constraint is `ovnkube-identity`. Its
validating webhook intercepts every `nodes/status` and `pods/status` update
made by an `ovnkube-node` service account and rejects the request unless the
node only touches its own Node object, only touches annotations, and every
annotation key is on a hard-coded allowlist (`pkg/ovnwebhook/nodeadmission.go`).
That allowlist is part of the release: whenever a release adds a
node annotation that ovnkube-node writes on startup, the new ovnkube-node
depends on the new webhook to accept it.

The skews therefore are not symmetric:

* New `ovnkube-identity`, old `ovnkube-node`: harmless. The webhook allows keys
  nobody writes yet.
* Old `ovnkube-identity`, new `ovnkube-node`: the node's first write of a new
  annotation is denied, its startup sync fails, and it retries indefinitely
  without ever becoming healthy. The same applies to a new pod annotation
  written by ovnkube-node, and to a change in the certificate identity the
  approver checks.

The cluster-manager has the same dependency for a narrower set of writes. The
webhook ignores requests from other users unless they touch a key it protects;
then the user must be listed with `--extra-allowed-user` and the per-key rule
still applies. The cluster-manager service account is on that list because it
writes the protected `k8s.ovn.org/pod-networks` annotation when it allocates
pod IPs centrally for secondary networks with IPAM. Its own node annotations
(node subnets, node IDs, transit addresses, tunnel keys) are not protected and
are never checked. So a release in which the cluster-manager starts writing a
protected node or pod key, or a new value shape for one, needs the new
`ovnkube-identity` first, exactly like ovnkube-node does.

So `ovnkube-identity` must be running the new release before the first
`ovnkube-node` or `ovnkube-control-plane` of that release starts, in every
otherwise-allowed order. This is
cheap to satisfy: the DaemonSet rolls with `maxSurge: 100%` and
`maxUnavailable: 0`, so a new pod is Ready on each control-plane node before
the old one is removed and the webhook never goes unreachable. 
The upstream job and the OpenShift network operator apply all components 
in one pass and rely on identity finishing its rollout in seconds while 
`ovnkube-node` rolls node by node.

Removing a key from the allowlist has the opposite constraint. Identity may be
the first component upgraded, so for the whole roll-out the new webhook
validates writes from old `ovnkube-node` and old `ovnkube-control-plane` pods
that still set the key. Dropping it in the same release that stops writing it
would deny those writes and break the not-yet-upgraded pods. A key therefore
stays on the allowlist, with its per-key rule unchanged, for one release after
the last writer is removed, and is deleted in the release after that.

### Two moments during an upgrade

There are two distinct points at which OVN-Kubernetes code
changes behaviour, and a change has to pick the right one:

1. **The ovnkube pod restarts with the new image.** Workload pods are
   still running and have established connections. Anything the new code does
   here must be transparent to those connections: adding objects, cleaning up
   stale ones, changing how new flows are created.
2. **The node has no workload pods.** On platforms that reboot nodes this
   happens after the drain; on others it may never happen automatically. This
   is the only moment at which traffic-affecting changes to existing
   topology, such as moving SNAT or re-attaching a gateway router, may be
   applied. Some changes may not be possible at all without this step and if
   so, the change must clearly document this requirement.

## Rules for upgrade-safe changes

**Adjacent releases interoperate.** Release N+1 of any component works with
release N of every other component and vice versa. Nothing older than N needs
to be understood, and nothing needs to remain readable by N once N+1 has run.
A change that cannot be made compatible in one step is split across two
releases: the first introduces the new behaviour while still understanding and
producing the old state, the next removes the old code path.

**New node or pod annotations are gated by the identity webhook.** Adding a key
that ovnkube-node or cluster-manager writes, means adding it to the webhook
allowlist in the same release, and ensuring the right upgrade order.
Removing a key is a two-release change: stop writing it in one release, drop it
from the allowlist in the next, so an upgraded webhook keeps accepting writes
from components still on the previous release.

**Fall back when a dependency is older.** If ovnkube-node starts needing data
the new cluster-manager publishes, it must be able to continue working with
the old logic for at least one release since cluster-manager may be updated
after the node. The network ID move from Node annotations to NAD annotations
initially violated this: workers upgraded first, looked for the ID on the NAD
only, and could not start pods until the cluster-manager migrated it (fixed in
`d5e8d2e8d`).

**Startup is a full, idempotent sync.** Every controller reconciles its
database against the desired state on start. Stale objects are recognised by
their `external_ids` and removed; one-shot migrations run inside this sync and
tolerate being interrupted and re-run. Examples: replacing the switch-based
drop ACL with a port-group ACL (`fd6110113`), moving `requested-chassis` from
chassis hostname to chassis ID (`3b1413382`), and reserving low tunnel keys
optimistically so pods that already hold them are skipped rather than broken
(`6f6575a2e`).

**Established connections survive a pod restart.** Restarting ovnkube-node,
`northd`, or `ovn-controller` must not re-SNAT, re-route, or re-key existing
flows. Changes that would do so are deferred to the no-workload moment.

**Transitional state is cleaned up.** Ports, routes, addresses, or
annotations added only to interoperate with older nodes are removed once every
node reports the new version, and that cleanup is part of the startup sync so
it runs even if the triggering event was missed.

**Upgrades should be tested.** Changes with any of the properties above need
coverage in `ovn-upgrade-e2e`, which upgrades a cluster from the previous
release to the PR image and runs the e2e suite, plus unit tests for the
mixed-release decision logic.

## Example: the move to interconnect mode

Before interconnect, one `ovnkube-master` Deployment programmed a shared,
RAFT-replicated NB database and `ovnkube-node` only consumed the SB database.
The upgrade order was: `ovnkube-identity`, then the `ovnkube-node` DaemonSet,
then `ovnkube-db`, then `ovnkube-master`. Nodes went first because they were
passive consumers that had to tolerate an old master, and the master went last
so that one-shot database migrations ran once with every node already new.

Interconnect removed the shared database and moved its work into every node,
leaving the cluster-manager with allocation and status duties only. That
inverted the dependency: nodes no longer depend on a central database, but on
identifiers the cluster-manager hands out. It also removed the natural
serialisation point for migrations. Every node now migrates its own state on
restart, so the rules above about idempotent startup sync and evidence-based
detection replaced "run the migration once in the master". The version window
between nodes and the cluster-manager became the primary compatibility
concern. The upstream upgrade job does not test that window today: it upgrades
everything and tests the end state, so the skew guarantees rest on code review
and on the rules above.

## Example: Layer2 transit router topology

The primary Layer2 UDN topology was changed to attach each node's gateway
router to a per-node transit router instead of directly to the distributed
Layer2 switch ([OKEP-5094](../okeps/okep-5094-layer2-transit-router.md)). The
change moves management-port SNAT and changes which ports carry east-west
traffic, so it cannot be applied under running workloads. It exercises every
rule above.
The extra tricky part here is that when the node switches to the new topology,
all remote nodes must know that and upgrade their remote ports from switch
to the router. Each node was using `k8s.ovn.org/layer2-topology-version`
annotation for that.

* **Evidence-based detection.** The cluster-manager stays in legacy
  allocation mode if any node has legacy per-node Layer2 tunnel ID
  annotations without a topology version annotation; a fresh or fully
  migrated cluster uses the new topology immediately.
* **Right moment.** ovnkube-node reads its own
  `k8s.ovn.org/layer2-topology-version` annotation. Without it, it checks the
  local NB database for pod ports on primary Layer2 switches and only switches
  topology when there are none, which on rebooting platforms is right after
  the drain.
* **Older dependency fallback.** Before switching, the node also verifies the
  cluster-manager has started assigning tunnel keys on NADs. A new node under
  an old cluster-manager keeps the legacy topology and keeps working.
* **Versioned state.** The node sets the annotation to `2.0` after switching;
  the webhook forbids changing it to anything else.
* **Mixed-node interoperation.** An upgraded node keeps old nodes' `remote`
  gateway-router ports on its switch and adds temporary ports carrying the
  gateway router's MAC and a join-subnet address. The join subnet is the one
  network both topologies understand, so traffic that crosses the
  interconnect between an old and a new node, which is the egress IP and
  service ingress case, is steered onto it with host routes that always win
  longest-prefix match. Pod-to-pod traffic is unaffected because pod ports
  keep their tunnel keys. Other nodes watch for the annotation and replace
  the peer's switch remote port with a transit router remote port.
* **Cleanup and retention.** Once every node carries the annotation, the
  temporary ports and the old gateway-router switch ports, routes, and NATs
  are removed from the startup sync. The old topology code stays for one
  release so clusters can pass through the mixed state.

## Checklist for authors and reviewers

* Does the change alter OVN objects carrying traffic for running pods? If so,
  is it deferred to the no-workload moment or transparent to existing flows?
* Does ovnkube-node need something new from the cluster-manager? Is there a
  fallback for nodes that start before the cluster-manager is upgraded?
* Does ovnkube-node or the cluster-manager write a new node or pod annotation,
  or a new value shape for a protected one? Is it on the identity webhook
  allowlist? Does it stop writing one? Is the key kept on the allowlist for one
  more release?
* Is legacy state detected from artifacts the old version left behind?
* Is the startup sync idempotent, including after a partial previous run?
* Is transitional state cleaned up, and does the cleanup run on restart?
* Is there `ovn-upgrade-e2e` coverage and a unit test for the mixed-version
  path?

## References

* [Architecture](architecture.md) for the interconnect component layout.
* [OKEP-5094: Primary UDN Layer2 topology improvements](../okeps/okep-5094-layer2-transit-router.md),
  sections "Rolling upgrade and traffic disruption" and "Upgrade Details".
* `test/scripts/upgrade-ovn.sh` and the `ovn-upgrade-e2e` job in
  `.github/workflows/test.yml`.
