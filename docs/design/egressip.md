# EgressIP

## Introduction

EgressIP provides a mechanism for Kubernetes pods to use a specific, predictable
source IP address for traffic leaving the cluster, instead of the default node
IP. This is essential in environments where external firewalls, access control
lists, or audit systems identify traffic by source IP. Without EgressIP, a pod's
egress traffic is SNATed to the node IP, which changes whenever the pod is
rescheduled to a different node.

There are two flavors of EgressIP based on the network the egress IP belongs to:

- **Primary (OVN) network EgressIP**: the egress IP belongs to the subnet of the
  primary machine network of the node. SNAT is performed atomically inside OVN's
  gateway router. The IP is added to the OVS bridge interface (e.g. `breth0`).
- **Secondary host network EgressIP**: the egress IP belongs to the node's secondary NICs.
  Traffic is rerouted out of OVN via the management port (`ovn-k8s-mp0`), then SNAT
  and routing happen in the host networking stack using iptables and policy-based routing.

## Multiple MAC Issue During EgressIP Failover (GARP Handling)

When an egress IP migrates from Node A (old) to Node B (new) in case of graceful
shutdown of Node A, Node B sends a GARP to announce its MAC as the owner of the
egress IP. However, Node A may continue responding to ARP requests for the egress IP
until it fully shuts down. This creates a window where external switches see two
different MACs for the same IP, causing traffic to be intermittently sent to the wrong
node (blackholed).

Two entities on the old node can respond to ARP for the egress IP:

1. **The egress IP attached to the bridge interface**: the IP remains on the
   bridge (e.g. `breth0`) until the interface is brought down during node
   shutdown. The Linux kernel responds to ARP requests for any IP on its
   interfaces.
   **TODO:** Further discussion is required to decide whether we need primary
   EgressIP on node interface or not.
2. **OVN logical flows (SNAT at the gateway router)**: OVN's gateway router ARP
   responder tables generate unicast ARP replies for any IP that has a SNAT entry.
   These flows remain active as long as `ovs-vswitchd` is running.

In bare-metal environments, node shutdown can take 10-20 seconds, creating a
prolonged window for multiple MAC responses. As a result, the ARP table at the
networking device may get updated with a wrong MAC address. The way network
devices are configured to update the ARP table also contributes to the issue. Modern
network devices can be configured to override existing neighbor entries without
GARP to support a scenario where a VIP moves without GARP. However, this is a
transient issue and gets resolved as soon as the old EgressIP node stops responding
and the external network device updates the ARP table.

```text
    Node A (old EIP owner)          Network         Node B (new EIP owner)
    ──────────────────────         ─────────         ──────────────────────
              │                        │                       │
              │     Health check fails │                       │
              │◄───────────────────────┤                       │
              │                        │   Cluster manager     │
              │                        │   reassigns EIP       │
              │                        │──────────────────────►│
              │                        │                       │
              │                        │       GARP (MAC_B)    │
              │                        │◄──────────────────────┤
              │                        │                       │
              │   ARP request for EIP  │                       │
              │◄───────────────────────┤                       │
              │                        │                       │
              │   ARP reply (MAC_A)    │   !! Duplicate MAC    │
              │───────────────────────►│                       │
              │                        │                       │
              │ Network sees TWO MACs for the EIP              │
              │ Traffic is black-holed intermittently          │
              │                        │                       │
```

### Solution 1: GARP Drop Flows on the Bridge (Startup)

When ovn-controller starts, it connects to the OVN Southbound database and
immediately sends GARPs for any egress IPs configured on its logical router
ports. The Southbound database may be stale if ovnkube-controller has not
synced yet with new egress IP CR status, causing GARPs for egress IPs that
have already been reassigned to other nodes.

To address this, OVN-Kubernetes installs the following OpenFlow rules on the
default bridge while shutting down node controller manager. We make sure to
remove these flows after ovn-controller has synced once.

```text
# Allow GARPs sourced from a node IP (priority 499)
cookie=0x0305,table=0,priority=499,in_port=<patch-port>,dl_dst=ff:ff:ff:ff:ff:ff,arp,arp_op=1,arp_spa=<node-ip>,actions=output:NORMAL

# Drop all other GARPs (priority 498)
cookie=0x0305,table=0,priority=498,in_port=<patch-port>,dl_dst=ff:ff:ff:ff:ff:ff,arp,arp_op=1,actions=drop
```

- **Priority 499**: allow GARP packets where the source IP is a node IP (via
  `generateGratuitousARPAllowFlow`).
- **Priority 498**: drop all other GARP packets (via `generateGratuitousARPDropFlow`).

These flows match on `dl_dst=ff:ff:ff:ff:ff:ff, arp, arp_op=1` (broadcast ARP
requests, the opcode OVN uses for GARPs per RFC 5227).

The flows remain in place until the ovnkube-controller has fully synced with the
OVN Northbound database. At that point, stale SNAT entries have been removed, so
ovn-controller will only GARP for valid egress IPs.

This solution prevents **outbound stale GARPs** from the restarting node, but
does **not** prevent the node from **responding** to incoming ARP requests.

### Solution 2: nftables ARP/NDP Blocking During Shutdown

The GARP drop flows above address the startup case but do not cover the shutdown
case: after the egress IP is reassigned to Node B, Node A's kernel and OVN
logical flows still respond to ARP requests until `ovs-vswitchd` stops.

#### Why nftables and the netdev Family

The key insight is that ARP responses on the old node originate from two
independent subsystems (kernel IP stack and OVN logical flows), and both must be
silenced before the bridge interface is brought down. The `netdev` table family
in nftables provides an ingress hook on the physical uplink interface that
evaluates packets **before** they reach the OVS bridge. This is the earliest
possible interception point — it stops ARP requests from reaching either the
bridge interface (where the kernel would respond) or `ovs-vswitchd` (where OVN
logical flows would respond).

#### Shutdown Sequence

The following sequence is executed during graceful shutdown:

1. **Collect assigned egress IPs**: queries the EgressIP informer to find all
   egress IPs currently assigned to this node.
2. **Install nftables drop rules**: the following components are added to the node
   to drop unwanted ARP packets:
   - A dedicated nftables table `ovn-kubernetes-egressip` in the `netdev`
     family.
   - A chain `egressip-drop` with an ingress hook on the physical uplink
     interface (e.g. `ens3f0np0`), at filter priority.
   - For IPv4: a set `egressip-v4` populated with the assigned egress IPs, and
     a rule that drops all ARP requests from outside destined towards the EgressIP
     hosted on this node.
   - For IPv6: a set `egressip-v6` populated with the assigned egress IPs, and
     a rule that drops all neighbor solicitations from outside destined towards
     the EgressIP hosted on this node.
3. **Re-enable GARP drop flows**: adds the aforementioned GARP drop flows to ensure
   that any stale GARPs from `br-int` are also dropped during the remaining
   shutdown window.
4. **Continue shutdown**: the default node network controller and network manager
   are stopped, eventually bringing down `ovs-vswitchd`.

The nftables rules are installed synchronously and atomically via a single
nftables transaction, guaranteeing they are active before any subsequent cleanup
steps remove the egress IP from the bridge or trigger OVN reconvergence.

**Known limitation (status-driven race):** step 1 decides what to block from
the EgressIP status in the informer cache, but the entities that answer ARP —
the OVN SNAT flow and the IP on the interface — are removed asynchronously. If
a reassignment moves the egress IP off this node in status *before* that
dataplane cleanup completes (e.g. an EIP re-selection unrelated to this node's
shutdown), the collected list may omit an egress IP that is still physically
present, leaving a window where the node keeps responding to ARP.

#### Startup Cleanup

On the next startup, NodeControllerManager deletes the entire
`ovn-kubernetes-egressip` table. This cleanup runs after the OVN controller has
synced and stale SNATs have been removed, ensuring the nftables rules are not
removed before OVN has converged to the correct state.

For a full node reboot, the kernel automatically clears all nftables state, so
the cleanup primarily handles container/process restarts where the host kernel
persists nftables rules across restarts of the ovnkube process.

#### Ungraceful Shutdown

In the case of an ungraceful shutdown (OOM kill, `SIGKILL`, power failure), the
nftables rules are not installed. However, this is acceptable because in an
ungraceful shutdown:

- `ovs-vswitchd` stops immediately, so OVN logical flows cannot respond to ARP.
- The network interface goes down (or the entire host is unreachable), so the
  kernel cannot respond either.

The multiple MAC problem specifically occurs during **graceful** shutdown where
the host networking stack remains operational for an extended period after the
egress IP has been reassigned.

#### Combined Protection

The two solutions work together to cover both directions of the race:

| Scenario | Protection |
|----------|------------|
| **Startup**: ovn-controller GARPs for stale egress IPs from the Southbound DB | GARP drop flows on the bridge (Solution 1) |
| **Shutdown**: old node responds to ARP requests from the new node's GARP broadcast | nftables netdev ingress drop rules (Solution 2) |
| **Shutdown**: old node's OVN logical flows generate stale GARPs via `br-int` | GARP drop flows re-enabled during `Stop()` (Solution 1, reactivated) |

#### Applicability by Network Type

The nftables netdev ingress drop rules (Solution 2) bind their ingress hook to a
single device — the physical uplink of the default gateway bridge (`breth0`),
returned by `Gateway.GetUplinkName()`. An ARP/NDP request is only dropped if it
ingresses on that device. Whether the fix applies therefore depends on which
interface hosts the egress IP:

| Egress IP network type | Covered by this fix? |
|------------------------|----------------------|
| Default cluster network | **Yes** |
| Primary user-defined network (UDN) | **Yes** |
| Secondary host network (egress IP on a secondary Linux interface) | **No** |

The secondary host network case is not covered because the egress IP is hosted on
a secondary NIC rather than the `breth0` uplink, so ARP/NDP requests never reach
the drop chain. Note that secondary UDNs may also need a similar per-interface
fix if/when EgressIP support is added for them.
